import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

const appCSS = readFileSync(resolve(process.cwd(), 'src/styles/app.css'), 'utf8');

describe('syntax theme', () => {
  it('defines contrasting dark and light palettes', () => {
    expect(appCSS).toContain('--syntax-text: #c9d1d9');
    expect(appCSS).toContain('--syntax-keyword: #ff7b72');

    const lightTheme = appCSS.slice(appCSS.indexOf('@media (prefers-color-scheme: light)'));
    expect(lightTheme).toContain('--syntax-text: #24292f');
    expect(lightTheme).toContain('--syntax-keyword: #cf222e');
    expect(lightTheme).toContain('--syntax-string: #0a3069');
  });

  it('scopes tokens to highlighted markdown and diff code', () => {
    expect(appCSS).toContain(':is(.markdown-body, .diff-code) :is(.hljs-doctag, .hljs-keyword');
    expect(appCSS).toContain('.diff-code :is(.hljs-addition, .hljs-deletion)');
    expect(appCSS).toContain('background: transparent');
  });
});
