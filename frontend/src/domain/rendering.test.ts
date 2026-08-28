import { describe, expect, it, vi } from 'vitest';
import { highlightDiffLine } from './rich-highlight';
import { decorateRichContent, renderMarkdown, stableMarkdownBoundary } from './markdown';
import {
  analyzeStreamingMarkdown,
  findActiveFencedCodeBlock,
  hasIncrementalGlobalMarkdownSyntax,
  inspectFencedCodeBlock,
} from './markdown-streaming';
import {
  activeMentionAtCursor,
  applyCompletion,
  composerCompletions,
  mentionCompletions,
} from './completions';

describe('markdown security and streaming', () => {
  it('sanitizes active content and hardens external links', () => {
    const html = renderMarkdown(
      '[safe](https://example.com) <script>alert(1)</script><img src=x onerror=alert(1)>',
    );
    expect(html).not.toContain('<script');
    expect(html).not.toContain('onerror');
    expect(html).toContain('target="_blank"');
    expect(html).toContain('rel="noopener noreferrer"');
  });
  it('keeps strict math syntax as text until the lazy decorator and avoids currency delimiters', () => {
    const html = renderMarkdown('Price is $5 and inline is \\(x^2\\).');
    expect(html).toContain('$5');
    expect(html).toContain('\\(x^2\\)');
  });
  it('highlights supported fence aliases and silently leaves unknown languages alone', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    const root = document.createElement('div');
    root.innerHTML =
      '<pre><code class="language-text">plain</code></pre><pre><code class="language-html">&lt;p&gt;hello&lt;/p&gt;</code></pre><pre><code class="language-gitattributes">*.js text</code></pre><pre><code class="language-unknown-fence">value</code></pre>';
    await decorateRichContent(root, '');
    expect(warn).not.toHaveBeenCalled();
    expect(root.querySelectorAll('[data-highlighted="yes"]')).toHaveLength(4);
    warn.mockRestore();
  });

  it('highlights CSS diff lines with the registered CSS grammar', () => {
    expect(highlightDiffLine('.header-action:hover {', 'css')).toContain('hljs-selector-class');
    expect(highlightDiffLine('  background: var(--surface);', 'css')).toContain('hljs-attribute');
  });

  it('does not expose an unterminated fenced tail as stable markdown', () => {
    const input = 'paragraph\n\n```ts\nconst x = 1';
    expect(stableMarkdownBoundary(input)).toBe('paragraph\n\n'.length);
    expect(stableMarkdownBoundary('**done**')).toBe('**done**'.length);
  });

  it('reports active fenced-code metadata without accepting partial closing markers', () => {
    const input = 'intro\n\n~~~~ typescript extra\nconst value = `x`;\n~~~';
    const block = findActiveFencedCodeBlock(input);
    expect(block).toMatchObject({
      type: 'fenced-code',
      sourceStart: 'intro\n\n'.length,
      language: 'typescript',
      char: '~',
      width: 4,
      indent: 0,
    });
    expect(findActiveFencedCodeBlock('  ```ts\nvalue')).toMatchObject({
      indent: 2,
      language: 'ts',
    });
    expect(analyzeStreamingMarkdown(input, 0).activeBlock).toEqual(block);
    expect(inspectFencedCodeBlock(input, block!)).toMatchObject({
      closeStart: null,
      contentEnd: input.lastIndexOf('~~~'),
    });

    const closed = `${input}~\nfollowing`;
    expect(inspectFencedCodeBlock(closed, block!)).toMatchObject({
      closeStart: input.lastIndexOf('~~~'),
      closeEnd: input.length + 2,
    });
  });

  it('latches globally sensitive markdown onto the canonical fallback path', () => {
    expect(hasIncrementalGlobalMarkdownSyntax('[name]: https://example.com')).toBe(true);
    expect(hasIncrementalGlobalMarkdownSyntax('    indented code')).toBe(true);
    expect(hasIncrementalGlobalMarkdownSyntax('```md\n[name]: still code\n```')).toBe(false);
  });
});

describe('composer completion', () => {
  it('matches slash commands and agent mentions without treating email as mention', () => {
    expect(composerCompletions('/co', []).map((entry) => entry.value)).toContain('/compact');
    expect(composerCompletions('ask @ja', ['jarvis', 'other']).map((entry) => entry.value)).toEqual(
      ['@jarvis'],
    );
    expect(composerCompletions('me@example.com', ['example'])).toEqual([]);
    expect(activeMentionAtCursor('open @src/domain', 16)).toMatchObject({ query: 'src/domain' });
    expect(applyCompletion('please /co', '/compact')).toBe('please /compact ');
  });

  it('restores branch commands and live skill filtering while streaming', () => {
    const skills = [
      {
        name: 'review',
        description: 'Review changes',
        argument_hint: '[scope]',
        execution: 'isolated',
        source: 'local',
      },
      { name: 'explain', description: 'Explain code', execution: 'main', source: 'user' },
      { name: 'compact', collides_with_builtin: true },
    ];
    const values = composerCompletions('/', [], skills, true).map((entry) => entry.value);
    expect(values).toEqual(
      expect.arrayContaining(['/fork', '/thread', '/tree', '/side', '/review']),
    );
    expect(values).not.toEqual(expect.arrayContaining(['/compact', '/undo', '/explain']));
    expect(
      composerCompletions('/', [], skills).filter((entry) => entry.value === '/compact'),
    ).toHaveLength(1);
    expect(
      composerCompletions('/', [], skills).find((entry) => entry.value === '/review')?.label,
    ).toBe('/review [scope]');
  });

  it('applies server mention ranges without damaging text after the caret', () => {
    const [completion] = mentionCompletions({
      active: true,
      token: { start_utf16: 7, end_utf16: 11, query: 'typ' },
      items: [{ path: 'types.go', kind: 'file', insert_text: '@types.go', segments: [] }],
    });
    expect(applyCompletion('review @typ now', completion)).toBe('review @types.go now');
  });
});
