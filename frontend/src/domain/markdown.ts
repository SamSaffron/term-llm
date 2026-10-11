import DOMPurify from 'dompurify';
import { marked, Renderer } from 'marked';
import { findStableMarkdownBoundary, isInCodeBlockFast } from './markdown-streaming';

import { escapeHTML, externalLink, markdownHooks } from './markdown-hooks';
import { audioMediaHTML, trustedPublishedMediaURL } from './audio-media';

marked.use({ gfm: true, breaks: true, ...markdownHooks });

// Keep math delimiters intact until the optional KaTeX pass. Placeholders are
// escaped text, never HTML, so sanitizer policy remains authoritative.
function protectMath(source: string): { source: string; values: string[] } {
  const values: string[] = [];
  const protectedSource = source.replace(
    /^\$\$[ \t]*\n[\s\S]*?\n[ \t]*\$\$(?=\n|$)|\\\([\s\S]*?\\\)|\\\[[\s\S]*?\\\]/gm,
    (value) => {
      const index = values.push(value) - 1;
      return `TERM_LLM_MATH_${index}_TOKEN`;
    },
  );
  return { source: protectedSource, values };
}

export function sanitizeMarkdownHTML(
  source: string,
  variant: 'chat' | 'document' = 'chat',
): string {
  return DOMPurify.sanitize(source, {
    USE_PROFILES: { html: true },
    ADD_ATTR: ['target', 'rel', 'data-language'],
    FORBID_TAGS: [
      'style',
      'script',
      'iframe',
      'object',
      'embed',
      'form',
      'input',
      'button',
      ...(variant === 'chat' ? ['audio', 'source'] : []),
    ],
    FORBID_ATTR: ['style', 'srcdoc', 'onerror', 'onload'],
    ALLOW_UNKNOWN_PROTOCOLS: false,
  });
}

export interface MarkdownMediaTarget {
  url: string;
  type: 'image' | 'video' | 'audio';
  name?: string;
  caption?: string;
}

export type MarkdownMediaResolver = (reference: string) => MarkdownMediaTarget | undefined;

function mediaRenderer(
  resolveMedia: MarkdownMediaResolver,
  audioCards: Map<string, string>,
): Renderer {
  const renderer = new Renderer();
  renderer.link = externalLink;
  const defaultImage = renderer.image.bind(renderer);
  renderer.image = function (token) {
    const prefix = 'term-llm-media://';
    if (!token.href.startsWith(prefix)) return defaultImage(token);
    const label = token.text.trim() || 'Media';
    const reference = token.href.slice(prefix.length).trim().toLowerCase();
    if (!/^[a-f0-9]{32}$/.test(reference))
      return `<span class="media-reference-missing">[${escapeHTML(label)} unavailable]</span>`;
    const media = resolveMedia(reference);
    if (!media)
      return `<span class="media-reference-missing">[${escapeHTML(label)} unavailable]</span>`;
    if (media.type === 'audio') {
      if (!trustedPublishedMediaURL(media.url, true))
        return `<span class="media-reference-missing">[${escapeHTML(label)} unavailable]</span>`;
      // The namespace is private to this render, never supplied by model HTML.
      const placeholder = `TERM_LLM_AUDIO_${crypto.getRandomValues(new Uint32Array(4)).join('-')}_TOKEN`;
      audioCards.set(
        placeholder,
        audioMediaHTML({
          url: media.url,
          name: media.name,
          caption: media.caption,
          label: token.text,
        }),
      );
      return `<span data-audio-placeholder="${placeholder}"></span>`;
    }
    if (media.type === 'video') {
      return `<video src="${escapeHTML(media.url)}" controls playsinline preload="metadata" aria-label="${escapeHTML(label)}"></video>`;
    }
    return `<img src="${escapeHTML(media.url)}" alt="${escapeHTML(label)}">`;
  };
  return renderer;
}

export function renderMarkdown(source: string, resolveMedia?: MarkdownMediaResolver): string {
  const math = protectMath(source || '');
  const audioCards = new Map<string, string>();
  const options = resolveMedia
    ? { async: false as const, renderer: mediaRenderer(resolveMedia, audioCards) }
    : { async: false as const };
  let rendered = marked.parse(math.source, options) as string;
  rendered = rendered.replace(/TERM_LLM_MATH_(\d+)_TOKEN/g, (_match, index: string) =>
    escapeHTML(math.values[Number(index)] || ''),
  );
  const sanitized = sanitizeMarkdownHTML(rendered);
  if (!audioCards.size) return sanitized;
  // Template contents stay inert: ordinary images must not fetch during this pass.
  const root = document.createElement('template');
  root.innerHTML = sanitized;
  const linkedCards = new Map<HTMLAnchorElement, ChildNode>();
  for (const slot of root.content.querySelectorAll<HTMLElement>('[data-audio-placeholder]')) {
    const placeholder = slot.dataset.audioPlaceholder || '';
    const card = audioCards.get(placeholder);
    if (!card || slot.closest('pre,code,textarea')) continue;
    const template = document.createElement('template');
    template.innerHTML = card;
    const anchor = slot.closest('a');
    if (anchor) {
      // Do not create nested anchors or let native controls activate the model link.
      // Keep multiple cards from the same link in their original occurrence order.
      const lastCard = template.content.lastChild!;
      (linkedCards.get(anchor) || anchor).after(template.content);
      linkedCards.set(anchor, lastCard);
      slot.remove();
    } else slot.replaceWith(template.content);
    // Each generated placeholder can be consumed only once.
    audioCards.delete(placeholder);
  }
  return root.innerHTML;
}

export function stableMarkdownBoundary(value: string): number {
  return (
    findStableMarkdownBoundary(value, 0) ||
    (isInCodeBlockFast(value, value.length) ? 0 : value.length)
  );
}

const MATH = [
  { expression: /\$\$([\s\S]+?)\$\$/g, display: true },
  { expression: /\\\[([\s\S]+?)\\\]/g, display: true },
  { expression: /\\\((.+?)\\\)/g, display: false },
];
let highlightPromise: Promise<typeof import('./rich-highlight')> | null = null;
let katexPromise: Promise<typeof import('./rich-katex')> | null = null;

export function needsHighlight(root: HTMLElement): boolean {
  return Boolean(root.querySelector('pre code'));
}
export function needsKatex(value: string): boolean {
  return /\$\$[\s\S]+?\$\$|\\\[[\s\S]+?\\\]|\\\(.+?\\\)/.test(value);
}

export async function decorateRichContent(
  root: HTMLElement,
  source: string,
  current: () => boolean = () => true,
): Promise<void> {
  // Wait for every dependency before touching the DOM, so code and math do not
  // arrive as independent visual layers. A failed optional renderer falls back
  // to sanitized plain content, rather than leaving the presentation pending.
  const [code, math] = await Promise.all([
    needsHighlight(root)
      ? (highlightPromise ||= import('./rich-highlight')).catch(() => null)
      : null,
    needsKatex(source) ? (katexPromise ||= import('./rich-katex')).catch(() => null) : null,
  ]);
  if (!current()) return;
  if (code) {
    const { highlight } = code;
    root.querySelectorAll<HTMLElement>('pre code').forEach((block) => {
      if (block.dataset.highlighted) return;
      const language = [...block.classList]
        .map((name) => /^language-(.+)$/.exec(name)?.[1] || '')
        .find(Boolean);
      if (language && !highlight.getLanguage(language)) {
        block.dataset.highlighted = 'yes';
        return;
      }
      highlight.highlightElement(block);
    });
  }
  if (math) {
    const { katex } = math;
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    const nodes: Text[] = [];
    while (walker.nextNode()) {
      const node = walker.currentNode as Text;
      if (!node.parentElement?.closest('pre,code,textarea,script,style,.katex')) nodes.push(node);
    }
    for (const node of nodes) {
      let html = escapeHTML(node.data);
      let changed = false;
      for (const rule of MATH)
        html = html.replace(rule.expression, (all, expression: string) => {
          changed = true;
          try {
            return katex.renderToString(expression, {
              displayMode: rule.display,
              throwOnError: false,
              trust: false,
              strict: 'warn',
            });
          } catch {
            return escapeHTML(all);
          }
        });
      if (!changed) continue;
      const span = document.createElement('span');
      span.innerHTML = DOMPurify.sanitize(html, {
        ADD_ATTR: ['class', 'style', 'aria-hidden'],
        FORBID_TAGS: ['script', 'iframe', 'object', 'embed'],
      });
      node.replaceWith(...Array.from(span.childNodes));
    }
  }
}

export const VIDEO_LINK_PATTERN = /\.(?:mp4|webm|mov|ogv)(?:[?#].*)?$/i;
export function rebaseRenderedAssetURLs(
  root: HTMLElement,
  rebase: (value: string) => string,
): void {
  root
    .querySelectorAll<HTMLImageElement | HTMLMediaElement>('img[src],audio[src],video[src]')
    .forEach((asset) => {
      const raw = asset.getAttribute('src') || asset.src;
      const next = rebase(raw);
      if (next !== raw) asset.src = next;
    });
  root.querySelectorAll<HTMLAnchorElement>('a[href]').forEach((anchor) => {
    const raw = anchor.getAttribute('href') || '';
    if (/^(?:\/|https?:)/i.test(raw)) anchor.href = rebase(raw);
    if (!anchor.closest('.audio-media-card') && VIDEO_LINK_PATTERN.test(raw))
      anchor.classList.add('video-link');
  });
}
