import { Marked } from 'marked';
import type { MarkdownSourceBlock } from './types';
import { sanitizeMarkdownHTML } from './markdown';

interface BlockToken {
  type: string;
  raw: string;
}

interface TokenList extends Array<BlockToken> {
  links: Record<string, unknown>;
}

export interface RenderedMarkdownSourceBlock extends MarkdownSourceBlock {
  source: string;
  html: string;
}

const escapeHTML = (value: unknown): string =>
  String(value || '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');

const documentMarked = new Marked({ gfm: true, breaks: false });
documentMarked.use({
  renderer: {
    link({ href, title, tokens }) {
      const label = this.parser.parseInline(tokens);
      const safeTitle = title ? ` title="${escapeHTML(title)}"` : '';
      return `<a href="${escapeHTML(href)}"${safeTitle} target="_blank" rel="noopener noreferrer">${label}</a>`;
    },
  },
  walkTokens(token) {
    if (token.type === 'del' && !token.raw.startsWith('~~')) {
      const mutable = token as unknown as {
        type: string;
        text: string;
        raw: string;
        tokens?: unknown;
      };
      mutable.type = 'text';
      mutable.text = mutable.raw;
      delete mutable.tokens;
    }
  },
});

interface ProtectedSource {
  source: string;
  values: string[];
  originalOffset: (offset: number) => number;
}

function protectDocumentMath(source: string): ProtectedSource {
  let normalized = '';
  const normalizedBoundaries: number[] = [0];
  for (let index = 0; index < source.length; index += 1) {
    if (source[index] === '\r') {
      if (source[index + 1] === '\n') index += 1;
      normalized += '\n';
      normalizedBoundaries.push(index + 1);
    } else {
      normalized += source[index];
      normalizedBoundaries.push(index + 1);
    }
  }
  const expression =
    /^\$\$[ \t]*\n[\s\S]*?\n[ \t]*\$\$(?=\n|$)|\\\([\s\S]*?\\\)|\\\[[\s\S]*?\\\]/gm;
  const values: string[] = [];
  const boundaries: number[] = [0];
  let transformed = '';
  let cursor = 0;
  let match: RegExpExecArray | null;
  const appendOriginal = (start: number, end: number) => {
    transformed += normalized.slice(start, end);
    for (let index = start; index < end; index += 1) boundaries.push(index + 1);
  };
  while ((match = expression.exec(normalized))) {
    appendOriginal(cursor, match.index);
    const original = match[0];
    const placeholder = `TERM_LLM_MATH_${values.push(original) - 1}_TOKEN`;
    transformed += placeholder;
    for (let index = 0; index < placeholder.length; index += 1)
      boundaries.push(
        index === placeholder.length - 1 ? match.index + original.length : match.index,
      );
    cursor = match.index + original.length;
  }
  appendOriginal(cursor, normalized.length);
  return {
    source: transformed,
    values,
    originalOffset: (offset) => {
      const normalizedOffset =
        boundaries[Math.max(0, Math.min(boundaries.length - 1, offset))] ?? 0;
      return normalizedBoundaries[normalizedOffset] ?? source.length;
    },
  };
}

function lineAt(lineStarts: number[], offset: number): number {
  let low = 0;
  let high = lineStarts.length;
  while (low < high) {
    const middle = (low + high) >>> 1;
    if (lineStarts[middle] <= offset) low = middle + 1;
    else high = middle;
  }
  return Math.max(1, low);
}

function blockEndLine(source: string, lineStarts: number[], start: number, end: number): number {
  if (end <= start) return lineAt(lineStarts, start);
  let last = end - 1;
  while (last >= start && (source[last] === '\n' || source[last] === '\r')) last -= 1;
  return lineAt(lineStarts, Math.max(start, last));
}

function firstNonblankLine(lines: string[], startLine: number, endLine: number): number {
  for (let line = startLine; line <= endLine; line += 1)
    if ((lines[line - 1] || '').trim()) return line;
  return startLine;
}

function restoreMath(html: string, values: string[]): string {
  return html.replace(/TERM_LLM_MATH_(\d+)_TOKEN/g, (_match, index: string) =>
    escapeHTML(values[Number(index)] || ''),
  );
}

export function applyDocumentURLPolicy(root: HTMLElement): void {
  root.querySelectorAll<HTMLImageElement>('img').forEach((image) => {
    const raw = image.getAttribute('src') || '';
    if (/^https?:\/\//i.test(raw)) {
      image.setAttribute('loading', 'lazy');
      image.setAttribute('referrerpolicy', 'no-referrer');
      return;
    }
    const replacement = document.createElement('span');
    replacement.className = 'markdown-local-asset';
    const alt = image.getAttribute('alt')?.trim();
    replacement.textContent = `${alt ? `${alt} — ` : ''}local asset unavailable in preview`;
    image.replaceWith(replacement);
  });
  root.querySelectorAll<HTMLAnchorElement>('a').forEach((anchor) => {
    const raw = anchor.getAttribute('href') || '';
    if (/^https?:\/\//i.test(raw)) {
      anchor.target = '_blank';
      anchor.rel = 'noopener noreferrer';
      return;
    }
    if (raw.startsWith('#')) {
      try {
        const id = decodeURIComponent(raw.slice(1));
        if (id && root.querySelector(`#${CSS.escape(id)}`)) {
          anchor.removeAttribute('target');
          return;
        }
      } catch {
        /* Malformed fragments are treated as unavailable. */
      }
    }
    anchor.removeAttribute('href');
    anchor.removeAttribute('target');
    anchor.classList.add('markdown-relative-link-unavailable');
    anchor.title = 'Repository-relative link unavailable in preview';
  });
}

/** Lexes a complete document once, preserving shared reference definitions, and
 * maps every visible top-level token monotonically back to exact source lines. */
export function markdownDocumentBlocks(source: string): RenderedMarkdownSourceBlock[] {
  const original = String(source || '');
  const protectedSource = protectDocumentMath(original);
  const tokens = documentMarked.lexer(protectedSource.source) as unknown as TokenList;
  const result: RenderedMarkdownSourceBlock[] = [];
  const lines = original.split(/\r?\n/);
  const lineStarts = [0];
  for (let index = 0; index < original.length; index += 1)
    if (original.charCodeAt(index) === 10) lineStarts.push(index + 1);
  let cursor = 0;
  let mappingValid = true;
  let visibleIndex = 0;

  for (const token of tokens) {
    const raw = String(token.raw || '');
    const mapped = mappingValid && protectedSource.source.startsWith(raw, cursor);
    if (mapped) cursor += raw.length;
    else mappingValid = false;
    if (token.type === 'space' || token.type === 'def') continue;

    const piece = [token] as TokenList;
    piece.links = tokens.links;
    const rendered = documentMarked.parser(piece as never) as string;
    const html = sanitizeMarkdownHTML(restoreMath(rendered, protectedSource.values));
    const transformedStart = mapped ? cursor - raw.length : 0;
    const startOffset = mapped ? protectedSource.originalOffset(transformedStart) : 0;
    const endOffset = mapped
      ? protectedSource.originalOffset(transformedStart + raw.length)
      : original.length;
    const startLine = mapped ? lineAt(lineStarts, startOffset) : 0;
    const endLine = mapped ? blockEndLine(original, lineStarts, startOffset, endOffset) : 0;
    const anchorLine = mapped ? firstNonblankLine(lines, startLine, endLine) : 0;
    result.push({
      id: `markdown-block-${visibleIndex++}`,
      type: token.type,
      startLine,
      endLine,
      anchorLine,
      source: mapped ? original.slice(startOffset, endOffset) : raw,
      html,
      commentable: mapped && Boolean(anchorLine) && Boolean(html.trim()),
    });
  }
  return result;
}
