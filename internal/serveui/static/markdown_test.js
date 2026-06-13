#!/usr/bin/env node
// markdown_test.js — Node.js test for markdown-setup.js rendering rules.
// Run directly: node internal/serveui/static/markdown_test.js
// Or via: go test ./internal/serveui/ (invoked by TestMarkdownSetupJS)
'use strict';

const path = require('path');
const dir = __dirname;

const marked = require(path.join(dir, 'vendor/marked/marked.umd.min.js'));
const setupMarkdown = require(path.join(dir, 'markdown-setup.js'));
setupMarkdown(marked);

let failures = 0;

function check(name, html, contains, absent) {
  for (const s of contains) {
    if (!html.includes(s)) {
      console.error('FAIL:', name, '— expected to contain', JSON.stringify(s));
      console.error('      got:', JSON.stringify(html));
      failures++;
    }
  }
  for (const s of absent) {
    if (html.includes(s)) {
      console.error('FAIL:', name, '— expected NOT to contain', JSON.stringify(s));
      console.error('      got:', JSON.stringify(html));
      failures++;
    }
  }
}

const cases = [
  {
    name: 'single tilde before number renders as plain text',
    input: '~100',
    contains: ['~100'],
    absent: ['<del>'],
  },
  {
    name: 'single tilde in prose renders as plain text',
    input: 'costs ~$100 and takes ~200ms',
    contains: ['~$100', '~200ms'],
    absent: ['<del>'],
  },
  {
    name: 'single tilde approximation renders as plain text',
    input: 'roughly ~5x faster',
    contains: ['~5x'],
    absent: ['<del>'],
  },
  {
    name: 'double tilde strikethrough is preserved',
    input: '~~deleted~~',
    contains: ['<del>deleted</del>'],
    absent: [],
  },
  {
    name: 'double tilde strikethrough in prose is preserved',
    input: 'this is ~~wrong~~ text',
    contains: ['<del>wrong</del>'],
    absent: [],
  },
  {
    name: 'fenced code with language gets language class',
    input: '```ruby\nputs "hello"\n```',
    contains: ['<code class="language-ruby">'],
    absent: [],
  },
  {
    name: 'fenced code without language gets no language class',
    input: '```\nplain text\n```',
    contains: ['<code>'],
    absent: ['language-'],
  },
  {
    name: 'fenced code with sql language gets language class',
    input: '```sql\nSELECT 1;\n```',
    contains: ['<code class="language-sql">'],
    absent: [],
  },
  {
    name: 'inline escaped math delimiters survive markdown parsing',
    input: 'Planck length is \\(\\ell_P = \\sqrt{\\hbar G / c^3}\\).',
    contains: ['\\(\\ell_P = \\sqrt{\\hbar G / c^3}\\)'],
    absent: ['(\\ell_P = \\sqrt{\\hbar G / c^3})'],
  },
  {
    name: 'display escaped math delimiters survive markdown parsing',
    input: '\\[\\ell_P = \\sqrt{\\hbar G / c^3}\\]',
    contains: ['\\[\\ell_P = \\sqrt{\\hbar G / c^3}\\]'],
    absent: ['[\\ell_P = \\sqrt{\\hbar G / c^3}]'],
  },
  {
    name: 'math delimiter preservation escapes html before katex pass',
    input: '\\(<img src=x onerror=alert(1)>\\)',
    contains: ['\\(&lt;img src=x onerror=alert(1)&gt;\\)'],
    absent: ['<img src=x onerror=alert(1)>'],
  },
  {
    name: 'single dollar math stays ordinary prose',
    input: 'Cost is $100 and formula is $x + 1$.',
    contains: ['Cost is $100 and formula is $x + 1$.'],
    absent: ['katex'],
  },
  {
    name: 'display dollar math block survives markdown breaks',
    input: '$$\n\\int e^{2x} \\, dx = \\frac{1}{2}e^{2x} + C\n$$',
    contains: ['$$\n\\int e^{2x} \\, dx = \\frac{1}{2}e^{2x} + C\n$$'],
    absent: ['<br>'],
  },
];

for (const tc of cases) {
  const html = marked.parse(tc.input);
  check(tc.name, html, tc.contains, tc.absent);
  if (tc.contains.every(s => html.includes(s)) && tc.absent.every(s => !html.includes(s))) {
    console.log('PASS:', tc.name);
  }
}

if (failures > 0) {
  console.error('\n' + failures + ' test(s) failed');
  process.exit(1);
} else {
  console.log('\nAll tests passed');
  process.exit(0);
}
