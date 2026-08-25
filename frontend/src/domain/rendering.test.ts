import { describe, expect, it } from 'vitest';
import { renderMarkdown, stableMarkdownBoundary } from './markdown';
import { applyCompletion, composerCompletions } from './completions';

describe('markdown security and streaming', () => {
  it('sanitizes active content and hardens external links', () => {
    const html = renderMarkdown('[safe](https://example.com) <script>alert(1)</script><img src=x onerror=alert(1)>');
    expect(html).not.toContain('<script'); expect(html).not.toContain('onerror');
    expect(html).toContain('target="_blank"'); expect(html).toContain('rel="noopener noreferrer"');
  });
  it('keeps strict math syntax as text until the lazy decorator and avoids currency delimiters', () => {
    const html = renderMarkdown('Price is $5 and inline is \\(x^2\\).');
    expect(html).toContain('$5'); expect(html).toContain('\\(x^2\\)');
  });
  it('does not expose an unterminated fenced tail as stable markdown', () => {
    const input = 'paragraph\n\n```ts\nconst x = 1';
    expect(stableMarkdownBoundary(input)).toBe('paragraph\n\n'.length);
    expect(stableMarkdownBoundary('**done**')).toBe('**done**'.length);
  });
});

describe('composer completion', () => {
  it('matches slash commands and agent mentions without treating email as mention', () => {
    expect(composerCompletions('/co', []).map((entry) => entry.value)).toContain('/compact');
    expect(composerCompletions('ask @ja', ['jarvis', 'other']).map((entry) => entry.value)).toEqual(['@jarvis']);
    expect(composerCompletions('me@example.com', ['example'])).toEqual([]);
    expect(applyCompletion('please /co', '/compact')).toBe('please /compact ');
  });
});
