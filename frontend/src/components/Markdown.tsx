import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import { analyzeStableMarkdownBoundary, canStreamPlainTextTailIncremental, createStreamingState, nextStreamingRenderDelay } from '../domain/markdown-streaming';
import { decorateRichContent, rebaseRenderedAssetURLs, renderMarkdown, VIDEO_LINK_PATTERN } from '../domain/markdown';

interface MarkdownProps {
  value: string;
  className?: string;
  streaming?: boolean;
  rebase?: (value: string) => string;
  onMedia?: (source: string, type: 'image' | 'video') => void;
}

function Rendered({ value, className, rebase, onMedia }: Omit<MarkdownProps, 'streaming'>) {
  const root = useRef<HTMLDivElement>(null);
  const html = useMemo(() => renderMarkdown(value), [value]);
  useEffect(() => {
    const element = root.current; if (!element) return;
    if (rebase) rebaseRenderedAssetURLs(element, rebase);
    void decorateRichContent(element, value);
  }, [value, rebase]);
  return <div ref={root} class={className} dangerouslySetInnerHTML={{ __html: html }} onClick={(event) => {
    const target = event.target as HTMLElement;
    const image = target.closest('img');
    if (image && onMedia) { event.preventDefault(); event.stopPropagation(); onMedia(image.currentSrc || image.src, 'image'); return; }
    const link = target.closest('a');
    if (link && onMedia && VIDEO_LINK_PATTERN.test(link.href)) { event.preventDefault(); onMedia(link.href, 'video'); }
  }} />;
}

function useAdaptiveStreamingValue(value: string, streaming: boolean): string {
  const [rendered, setRendered] = useState(value); const latest = useRef(value); const timer = useRef<number | null>(null); const lastRender = useRef(performance.now());
  useEffect(() => {
    latest.current = value;
    if (!streaming) { if (timer.current !== null) clearTimeout(timer.current); timer.current = null; lastRender.current = performance.now(); setRendered(value); return; }
    if (value === rendered || timer.current !== null) return;
    const delay = nextStreamingRenderDelay(value.length); const wait = Math.max(0, delay - (performance.now() - lastRender.current));
    timer.current = window.setTimeout(() => { timer.current = null; lastRender.current = performance.now(); setRendered(latest.current); }, wait);
  }, [value, streaming, rendered]);
  useEffect(() => () => { if (timer.current !== null) clearTimeout(timer.current); }, []);
  return streaming ? rendered : value;
}

export function Markdown({ value, className = 'markdown', streaming = false, rebase, onMedia }: MarkdownProps) {
  const renderValue = useAdaptiveStreamingValue(value, streaming); const state = useRef(createStreamingState());
  if (!streaming) { state.current = createStreamingState(); return <Rendered value={renderValue} className={className} rebase={rebase} onMedia={onMedia} />; }
  const analysis = analyzeStableMarkdownBoundary(renderValue, 1024); state.current.latestContent = renderValue; state.current.stableLength = analysis.boundary; state.current.lastBoundaryOperations = analysis.operations;
  if (analysis.overBudget) {
    state.current.plainTextScanSource = renderValue; state.current.plainTextEligible = true;
    return <div class={`${className} streaming-markdown streaming-over-budget`} data-streaming-fallback="plain" style={{ whiteSpace: 'pre-wrap' }}>{renderValue}</div>;
  }
  const stable = analysis.boundary > 0 ? renderValue.slice(0, analysis.boundary) : '';
  const tail = analysis.boundary > 0 ? renderValue.slice(analysis.boundary) : renderValue;
  const plain = canStreamPlainTextTailIncremental(state.current, tail);
  return <div class={`${className} streaming-markdown`}>
    {stable && <Rendered value={stable} className="streaming-stable" rebase={rebase} onMedia={onMedia} />}
    {plain
      ? <div class="streaming-tail streaming-tail-plain" style={{ whiteSpace: 'pre-wrap' }}>{tail}</div>
      : <Rendered value={tail} className="streaming-tail" rebase={rebase} onMedia={onMedia} />}
  </div>;
}
