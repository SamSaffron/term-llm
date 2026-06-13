// markdown-setup.js — marked configuration shared between the browser and Node tests.
//
// Browser: loaded as a <script> after marked.umd.min.js; calls setupMarkdown(marked) immediately.
// Node.js: require('./markdown-setup.js') returns setupMarkdown for use in tests.
(function (factory) {
  'use strict';
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = factory;
  } else {
    factory(marked); // eslint-disable-line no-undef
  }
})(function setupMarkdown(marked) {
  const escapeHtml = (value) => String(value || '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');

  marked.use({
    breaks: true,
    gfm: true
  });

  // KaTeX auto-render runs after markdown has produced sanitized HTML.
  //
  // Marked's `breaks: true` is useful for chat prose, but it turns display math
  // written in the common LLM shape
  //
  //   $$
  //   \frac{1}{2}
  //   $$
  //
  // into `$$<br>...<br>$$`. KaTeX auto-render scans text nodes, so those `<br>`
  // elements split the delimiters and the user sees raw `\frac` soup. Preserve
  // the whole display-math block as escaped text so KaTeX gets intact delimiters.
  //
  // Marked also treats backslashes before punctuation as markdown escapes, which
  // would turn explicit math delimiters like \(...\) and \[...\] into plain
  // parentheses or brackets before KaTeX ever sees them. Preserve those spans as
  // text too. Single-dollar inline math intentionally remains disabled to avoid
  // mangling currency in LLM prose.
  marked.use({
    extensions: [{
      name: 'math-display-block',
      level: 'block',
      start(src) {
        return src.indexOf('$$');
      },
      tokenizer(src) {
        const match = /^\$\$[ \t]*\n[\s\S]*?\n[ \t]*\$\$(?=\n|$)/.exec(src);
        if (!match) return false;
        return {
          type: 'math-display-block',
          raw: match[0],
          text: match[0]
        };
      },
      renderer(token) {
        return `<p>${escapeHtml(token.raw)}</p>\n`;
      }
    }, {
      name: 'math-delimiter-span',
      level: 'inline',
      start(src) {
        const inline = src.indexOf('\\(');
        const display = src.indexOf('\\[');
        if (inline === -1) return display;
        if (display === -1) return inline;
        return Math.min(inline, display);
      },
      tokenizer(src) {
        const match = /^(\\\((?:.|\n)*?\\\)|\\\[(?:.|\n)*?\\\])/.exec(src);
        if (!match) return false;
        return {
          type: 'math-delimiter-span',
          raw: match[0],
          text: match[0]
        };
      },
      renderer(token) {
        return escapeHtml(token.raw);
      }
    }]
  });

  // Disable single-tilde strikethrough. GFM's del rule matches both ~text~
  // and ~~text~~, but a bare ~ is far too common in LLM output (~$100,
  // ~200ms, ~5x). Keep ~~double-tilde~~ as intentional strikethrough;
  // convert single-tilde del tokens back to raw text.
  marked.use({
    walkTokens(token) {
      if (token.type === 'del' && !token.raw.startsWith('~~')) {
        token.type = 'text';
        token.text = token.raw;
        delete token.tokens;
      }
    }
  });
});
