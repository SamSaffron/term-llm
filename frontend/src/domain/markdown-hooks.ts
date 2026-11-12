import { Renderer, type MarkedExtension } from 'marked';

export const escapeHTML = (value: unknown): string =>
  String(value || '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');

export const externalLink: Renderer['link'] = function (this: Renderer, { href, title, tokens }) {
  const label = this.parser.parseInline(tokens);
  const safeTitle = title ? ` title="${escapeHTML(title)}"` : '';
  return `<a href="${escapeHTML(href)}"${safeTitle} target="_blank" rel="noopener noreferrer">${label}</a>`;
};

// Wide tables scroll sideways inside their own box instead of being clipped by
// the message, so columns can size to their content.
export const scrollableTable: Renderer['table'] = function (this: Renderer, token) {
  return `<div class="markdown-table-scroll">${Renderer.prototype.table.call(this, token)}</div>\n`;
};

// Share only rendering policy. Chat and source-mapped documents retain their
// own parser instances, line-break settings, and math protection.
export const markdownHooks: MarkedExtension = {
  renderer: { link: externalLink, table: scrollableTable },
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
};
