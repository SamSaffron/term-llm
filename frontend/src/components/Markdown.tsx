import { copyText } from '../platform/browser';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import {
  analyzeStableMarkdownBoundary,
  canStreamPlainTextTailIncremental,
  createStreamingState,
  nextStreamingRenderDelay,
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
  root.querySelectorAll('pre').forEach((pre) => {
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

function Rendered({
  value,
  className,
  rebase,
  onMedia,
  copyCode = true,
}: Omit<MarkdownProps, 'streaming'> & { copyCode?: boolean }) {
  const root = useRef<HTMLDivElement>(null);
  const copyTimers = useRef(new Map<HTMLButtonElement, number>());
  const html = useMemo(() => renderMarkdown(value), [value]);
  useEffect(() => {
    const element = root.current;
    if (!element) return;
    if (rebase) rebaseRenderedAssetURLs(element, rebase);
    if (copyCode) addCodeCopyButtons(element);
    void decorateRichContent(element, value);
    const timers = copyTimers.current;
    return () => {
      timers.forEach((timer) => clearTimeout(timer));
      timers.clear();
    };
  }, [value, rebase, copyCode]);
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
      dangerouslySetInnerHTML={{ __html: html }}
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

export function Markdown({
  value,
  className = 'markdown-body',
  streaming = false,
  rebase,
  onMedia,
}: MarkdownProps) {
  const renderValue = useAdaptiveStreamingValue(value, streaming);
  const state = useRef(createStreamingState());
  if (!streaming) {
    state.current = createStreamingState();
    return <Rendered value={renderValue} className={className} rebase={rebase} onMedia={onMedia} />;
  }
  const analysis = analyzeStableMarkdownBoundary(renderValue, 1024);
  state.current.latestContent = renderValue;
  state.current.stableLength = analysis.boundary;
  state.current.lastBoundaryOperations = analysis.operations;
  if (analysis.overBudget) {
    state.current.plainTextScanSource = renderValue;
    state.current.plainTextEligible = true;
    return (
      <div
        class={`${className} streaming-markdown streaming-over-budget`}
        data-streaming-fallback="plain"
        style={{ whiteSpace: 'pre-wrap' }}
      >
        {renderValue}
      </div>
    );
  }
  const stable = analysis.boundary > 0 ? renderValue.slice(0, analysis.boundary) : '';
  const tail = analysis.boundary > 0 ? renderValue.slice(analysis.boundary) : renderValue;
  const plain = canStreamPlainTextTailIncremental(state.current, tail);
  return (
    <div class={`${className} streaming-markdown`}>
      {stable && (
        <Rendered
          value={stable}
          className="streaming-stable"
          rebase={rebase}
          onMedia={onMedia}
          copyCode={false}
        />
      )}
      {plain ? (
        <div class="streaming-tail streaming-tail-plain" style={{ whiteSpace: 'pre-wrap' }}>
          {tail}
        </div>
      ) : (
        <Rendered
          value={tail}
          className="streaming-tail"
          rebase={rebase}
          onMedia={onMedia}
          copyCode={false}
        />
      )}
    </div>
  );
}
