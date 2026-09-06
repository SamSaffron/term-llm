import type { MarkedExtension, Renderer } from 'marked';

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

// Share only rendering policy. Chat and source-mapped documents retain their
// own parser instances, line-break settings, and math protection.
export const markdownHooks: MarkedExtension = {
  renderer: { link: externalLink },
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
