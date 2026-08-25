import { useEffect, useMemo, useRef } from 'preact/hooks';
import { canStreamPlainTextTail, findStableMarkdownBoundary } from '../domain/markdown-streaming';
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

export function Markdown({ value, className = 'markdown', streaming = false, rebase, onMedia }: MarkdownProps) {
  if (!streaming) return <Rendered value={value} className={className} rebase={rebase} onMedia={onMedia} />;
  const boundary = findStableMarkdownBoundary(value, 1024);
  const stable = boundary > 0 ? value.slice(0, boundary) : '';
  const tail = boundary > 0 ? value.slice(boundary) : value;
  return <div class={`${className} streaming-markdown`}>
    {stable && <Rendered value={stable} className="streaming-stable" rebase={rebase} onMedia={onMedia} />}
    {canStreamPlainTextTail(tail)
      ? <div class="streaming-tail streaming-tail-plain" style={{ whiteSpace: 'pre-wrap' }}>{tail}</div>
      : <Rendered value={tail} className="streaming-tail" rebase={rebase} onMedia={onMedia} />}
  </div>;
}
