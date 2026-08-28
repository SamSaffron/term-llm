import { copyText } from '../platform/browser';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import {
  analyzeStreamingMarkdown,
  canStreamPlainTextTailIncremental,
  createStreamingState,
  findNextFencedCodeBlock,
  hasIncrementalGlobalMarkdownSyntax,
  inspectFencedCodeBlock,
  nextStreamingRenderDelay,
  type ActiveFencedCodeBlock,
  type StreamingMarkdownState,
} from '../domain/markdown-streaming';
import {
  decorateRichContent,
  rebaseRenderedAssetURLs,
  renderMarkdown,
  VIDEO_LINK_PATTERN,
} from '../domain/markdown';

interface MarkdownProps {
  value: string;
  className?: string;
  streaming?: boolean;
  rebase?: (value: string) => string;
  onMedia?: (source: string, type: 'image' | 'video') => void;
}

const COPY_ICON =
  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" aria-hidden="true"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M15 9V6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v7a2 2 0 0 0 2 2h3"/></svg>';
const COPIED_ICON =
  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="m5 12 4 4L19 6"/></svg>';

function addCodeCopyButtons(root: HTMLElement): void {
  const blocks = root.matches('pre')
    ? [root as HTMLPreElement, ...root.querySelectorAll('pre')]
    : [...root.querySelectorAll('pre')];
  blocks.forEach((pre) => {
    if (pre.querySelector('.code-copy-btn')) return;
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'code-copy-btn';
    button.title = 'Copy';
    button.setAttribute('aria-label', 'Copy code');
    button.innerHTML = COPY_ICON;
    pre.append(button);
  });
}

function useAdaptiveStreamingValue(value: string, streaming: boolean): string {
  const [rendered, setRendered] = useState(value);
  const latest = useRef(value);
  const timer = useRef<number | null>(null);
  const lastRender = useRef(performance.now());
  useEffect(() => {
    latest.current = value;
    if (!streaming) {
      if (timer.current !== null) clearTimeout(timer.current);
      timer.current = null;
      lastRender.current = performance.now();
      setRendered(value);
      return;
    }
    if (value === rendered || timer.current !== null) return;
    const delay = nextStreamingRenderDelay(value.length);
    const wait = Math.max(0, delay - (performance.now() - lastRender.current));
    timer.current = window.setTimeout(() => {
      timer.current = null;
      lastRender.current = performance.now();
      setRendered(latest.current);
    }, wait);
  }, [value, streaming, rendered]);
  useEffect(
    () => () => {
      if (timer.current !== null) clearTimeout(timer.current);
    },
    [],
  );
  return streaming ? rendered : value;
}

interface ActiveCodeDOM {
  block: ActiveFencedCodeBlock;
  pre: HTMLPreElement;
  code: HTMLElement;
  text: Text;
  renderedThrough: number;
}

class AssistantStreamRenderer {
  readonly root: HTMLDivElement;
  committed: HTMLDivElement | null = null;
  tail: HTMLDivElement | null = null;
  activeCode: ActiveCodeDOM | null = null;
  scanState: StreamingMarkdownState = createStreamingState();
  latestSource = '';
  committedLength = 0;
  tailSource = '';
  tailPlain = false;
  unsafe = false;
  fallback = false;
  hasStreamed = false;
  finalized = false;
  rebase?: (value: string) => string;

  constructor(root: HTMLDivElement, rebase?: (value: string) => string) {
    this.root = root;
    this.rebase = rebase;
  }

  setRebase(rebase?: (value: string) => string): void {
    this.rebase = rebase;
  }

  private decorate(root: HTMLElement, source: string, copyCode: boolean): void {
    if (this.rebase) rebaseRenderedAssetURLs(root, this.rebase);
    if (copyCode) addCodeCopyButtons(root);
    void decorateRichContent(root, source);
  }

  private createRegions(): void {
    const committed = document.createElement('div');
    committed.className = 'streaming-stable';
    const tail = document.createElement('div');
    tail.className = 'streaming-tail';
    this.root.replaceChildren(committed, tail);
    this.root.classList.add('streaming-markdown');
    this.root.removeAttribute('data-streaming-fallback');
    this.root.style.removeProperty('white-space');
    this.committed = committed;
    this.tail = tail;
    this.activeCode = null;
    this.committedLength = 0;
    this.tailSource = '';
    this.tailPlain = false;
    this.scanState = createStreamingState();
    this.finalized = false;
  }

  private renderCanonical(source: string, decorate: boolean): void {
    this.root.innerHTML = renderMarkdown(source);
    this.root.classList.remove('streaming-markdown', 'streaming-over-budget');
    this.root.removeAttribute('data-streaming-fallback');
    this.root.style.removeProperty('white-space');
    this.committed = null;
    this.tail = null;
    this.activeCode = null;
    if (decorate) this.decorate(this.root, source, true);
  }

  renderStatic(source: string): void {
    if (this.finalized && source === this.latestSource) return;
    this.renderCanonical(source, true);
    this.latestSource = source;
    this.finalized = true;
  }

  private appendCommittedMarkdown(source: string): void {
    if (!source || !this.committed) return;
    const piece = document.createElement('div');
    piece.className = 'streaming-stable-piece';
    piece.innerHTML = renderMarkdown(source);
    this.committed.append(piece);
    this.decorate(piece, source, true);
  }

  private renderTail(source: string): void {
    if (!this.tail) return;
    const plain = canStreamPlainTextTailIncremental(this.scanState, source);
    if (
      plain &&
      this.tailPlain &&
      source.startsWith(this.tailSource) &&
      this.tail.firstChild?.nodeType === Node.TEXT_NODE
    ) {
      (this.tail.firstChild as Text).appendData(source.slice(this.tailSource.length));
    } else if (plain) {
      this.tail.replaceChildren(document.createTextNode(source));
    } else {
      this.tail.innerHTML = renderMarkdown(source);
      if (this.rebase) rebaseRenderedAssetURLs(this.tail, this.rebase);
    }
    this.tail.className = `streaming-tail${plain ? ' streaming-tail-plain' : ''}`;
    this.tail.style.whiteSpace = plain ? 'pre-wrap' : '';
    this.tailSource = source;
    this.tailPlain = plain;
  }

  private beginCode(block: ActiveFencedCodeBlock): void {
    if (!this.committed || !this.tail) return;
    this.tail.replaceChildren();
    this.tailSource = '';
    const pre = document.createElement('pre');
    const code = document.createElement('code');
    if (block.language) code.classList.add(`language-${block.language}`);
    const text = document.createTextNode('');
    code.append(text);
    pre.append(code);
    this.committed.append(pre);
    this.activeCode = { block, pre, code, text, renderedThrough: block.contentStart };
  }

  private updateActiveCode(source: string, finalized: boolean): boolean {
    const active = this.activeCode;
    if (!active) return false;
    const inspection = inspectFencedCodeBlock(source, active.block, finalized);
    const contentEnd = inspection.closeStart ?? inspection.contentEnd;
    if (contentEnd < active.renderedThrough) {
      this.enterUnsafe(source);
      return true;
    }
    active.text.appendData(source.slice(active.renderedThrough, contentEnd));
    active.renderedThrough = contentEnd;
    if (inspection.closeStart === null || inspection.closeEnd === null) return true;

    this.decorate(active.pre, source.slice(active.block.sourceStart, inspection.closeEnd), true);
    this.committedLength = inspection.closeEnd;
    this.activeCode = null;
    this.renderIncremental(source, finalized);
    return true;
  }

  private enterUnsafe(source: string): void {
    this.unsafe = true;
    this.renderCanonical(source, false);
  }

  private renderIncremental(source: string, finalized = false): void {
    if (this.updateActiveCode(source, finalized)) return;
    if (hasIncrementalGlobalMarkdownSyntax(source)) {
      this.enterUnsafe(source);
      return;
    }
    const analysis = analyzeStreamingMarkdown(source, 1024);
    this.scanState.latestContent = source;
    this.scanState.lastBoundaryOperations = analysis.operations;

    if (analysis.overBudget) {
      this.fallback = true;
      this.root.classList.add('streaming-markdown', 'streaming-over-budget');
      this.root.dataset.streamingFallback = 'plain';
      this.root.style.whiteSpace = 'pre-wrap';
      this.root.replaceChildren(document.createTextNode(source));
      this.committed = null;
      this.tail = null;
      return;
    }

    const block = findNextFencedCodeBlock(source, this.committedLength) || analysis.activeBlock;
    if (block?.indent) {
      this.enterUnsafe(source);
      return;
    }
    if (block && block.sourceStart >= this.committedLength) {
      this.appendCommittedMarkdown(source.slice(this.committedLength, block.sourceStart));
      this.committedLength = block.sourceStart;
      this.beginCode(block);
      this.updateActiveCode(source, finalized);
      return;
    }

    if (analysis.boundary > this.committedLength) {
      this.appendCommittedMarkdown(source.slice(this.committedLength, analysis.boundary));
      this.committedLength = analysis.boundary;
      this.tail?.replaceChildren();
      this.tailSource = '';
    }
    this.scanState.stableLength = this.committedLength;
    this.renderTail(source.slice(this.committedLength));
  }

  update(source: string): void {
    if (!this.hasStreamed || this.finalized || !this.committed || !this.tail) {
      this.createRegions();
      this.latestSource = '';
      this.unsafe = false;
      this.fallback = false;
    }
    this.hasStreamed = true;
    if (!source.startsWith(this.latestSource)) this.unsafe = true;
    if (this.unsafe) this.renderCanonical(source, false);
    else if (this.fallback) {
      this.root.replaceChildren(document.createTextNode(source));
    } else this.renderIncremental(source);
    this.latestSource = source;
  }

  finalize(source: string): void {
    if (!this.hasStreamed) {
      this.renderStatic(source);
      return;
    }
    if (!source.startsWith(this.latestSource)) this.unsafe = true;
    if (this.unsafe || this.fallback || !this.committed || !this.tail) {
      this.renderCanonical(source, true);
    } else {
      this.renderIncremental(source, true);
      if (this.unsafe || this.fallback || !this.committed || !this.tail) {
        this.renderCanonical(source, true);
      } else {
        const fragment = document.createDocumentFragment();
        for (const child of Array.from(this.committed.childNodes)) {
          if (child instanceof HTMLElement && child.classList.contains('streaming-stable-piece'))
            fragment.append(...Array.from(child.childNodes));
          else fragment.append(child);
        }
        fragment.append(...Array.from(this.tail.childNodes));
        this.root.replaceChildren(fragment);
        this.root.classList.remove('streaming-markdown', 'streaming-over-budget');
        this.root.style.removeProperty('white-space');
        this.committed = null;
        this.tail = null;
        this.decorate(this.root, source, true);
      }
    }
    this.latestSource = source;
    this.finalized = true;
  }
}

export function Markdown({
  value,
  className = 'markdown-body',
  streaming = false,
  rebase,
  onMedia,
}: MarkdownProps) {
  const renderValue = useAdaptiveStreamingValue(value, streaming);
  const root = useRef<HTMLDivElement>(null);
  const renderer = useRef<AssistantStreamRenderer | null>(null);
  const copyTimers = useRef(new Map<HTMLButtonElement, number>());

  useLayoutEffect(() => {
    const element = root.current;
    if (!element) return;
    renderer.current ||= new AssistantStreamRenderer(element, rebase);
    renderer.current.setRebase(rebase);
    if (streaming) renderer.current.update(renderValue);
    else renderer.current.finalize(renderValue);
  }, [renderValue, streaming, rebase]);

  useEffect(() => {
    const timers = copyTimers.current;
    return () => {
      timers.forEach((timer) => clearTimeout(timer));
      timers.clear();
    };
  }, []);

  const handleCodeCopy = async (button: HTMLButtonElement): Promise<void> => {
    const code = button.closest('pre')?.querySelector('code');
    if (!code) return;
    const existingTimer = copyTimers.current.get(button);
    if (existingTimer !== undefined) clearTimeout(existingTimer);
    try {
      await copyText(code.textContent || '');
      if (!button.isConnected) return;
      button.classList.add('copied');
      button.classList.remove('copy-failed');
      button.title = 'Copied';
      button.setAttribute('aria-label', 'Copied');
      button.innerHTML = COPIED_ICON;
    } catch {
      if (!button.isConnected) return;
      button.classList.remove('copied');
      button.classList.add('copy-failed');
      button.title = 'Copy failed';
      button.setAttribute('aria-label', 'Copy failed');
    }
    const timer = window.setTimeout(() => {
      button.classList.remove('copied', 'copy-failed');
      button.title = 'Copy';
      button.setAttribute('aria-label', 'Copy code');
      button.innerHTML = COPY_ICON;
      copyTimers.current.delete(button);
    }, 1_500);
    copyTimers.current.set(button, timer);
  };

  return (
    <div
      ref={root}
      class={className}
      onClick={(event) => {
        const target = event.target as HTMLElement;
        const copyButton = target.closest<HTMLButtonElement>('.code-copy-btn');
        if (copyButton && event.currentTarget.contains(copyButton)) {
          event.preventDefault();
          event.stopPropagation();
          void handleCodeCopy(copyButton);
          return;
        }
        const image = target.closest('img');
        if (image && onMedia) {
          event.preventDefault();
          event.stopPropagation();
          onMedia(image.currentSrc || image.src, 'image');
          return;
        }
        const link = target.closest('a');
        if (link && onMedia && VIDEO_LINK_PATTERN.test(link.href)) {
          event.preventDefault();
          onMedia(link.href, 'video');
        }
      }}
    />
  );
}
