import { describe, expect, it, vi } from 'vitest';
import { highlight, highlightDiffLine } from './rich-highlight';
import { decorateRichContent, renderMarkdown, stableMarkdownBoundary } from './markdown';
import { applyDocumentURLPolicy } from './markdown-document';
import {
  analyzeStreamingMarkdown,
  elasticStreamingFrameDelay,
  elasticStreamingStep,
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
  it('disables repository-relative document assets without issuing live file URLs', () => {
    const root = document.createElement('div');
    root.innerHTML =
      '<a href="docs/guide.md">guide</a><a href="https://example.com">web</a><img src="images/chart.png" alt="chart"><img src="https://cdn.example.com/chart.png">';
    applyDocumentURLPolicy(root);
    const links = root.querySelectorAll('a');
    expect(links[0]).not.toHaveAttribute('href');
    expect(links[0]).toHaveClass('markdown-relative-link-unavailable');
    expect(links[1]).toHaveAttribute('target', '_blank');
    expect(root.querySelector('.markdown-local-asset')).toHaveTextContent(
      'chart — local asset unavailable in preview',
    );
    const remote = root.querySelector<HTMLImageElement>('img');
    expect(remote).toHaveAttribute('loading', 'lazy');
    expect(remote).toHaveAttribute('referrerpolicy', 'no-referrer');
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

  it('registers the curated high-value language set', () => {
    const languages = [
      'java',
      'kotlin',
      'php',
      'sql',
      'swift',
      'scss',
      'less',
      'dockerfile',
      'makefile',
      'lua',
      'powershell',
      'ini',
      'properties',
      'graphql',
      'protobuf',
      'dart',
      'scala',
      'r',
      'elixir',
      'erlang',
      'haskell',
      'objectivec',
      'cmake',
      'nix',
      'nginx',
      'apache',
      'perl',
      'clojure',
      'fsharp',
      'ocaml',
      'julia',
      'matlab',
      'fortran',
      'groovy',
      'gradle',
      'latex',
      'handlebars',
      'django',
      'twig',
      'erb',
      'wasm',
      'x86asm',
      'armasm',
      'verilog',
      'vhdl',
      'glsl',
      'awk',
      'tcl',
      'vim',
      'scheme',
      'lisp',
      'prolog',
      'terraform',
      'elm',
      'crystal',
      'nim',
      'ada',
      'vbnet',
      'coffeescript',
      'diff',
    ];
    expect(languages).toHaveLength(60);
    expect(languages.filter((language) => !highlight.getLanguage(language))).toEqual([]);
    expect(highlight.getLanguage('toml')).toBeTruthy();
  });

  it('drains streaming bursts elastically while preserving fractional cadence', () => {
    const shallow = elasticStreamingStep(10, 32);
    const deep = elasticStreamingStep(1_000, 32);
    expect(shallow.characters).toBeGreaterThanOrEqual(1);
    expect(deep.characters).toBeGreaterThan(shallow.characters);
    expect(deep.characters).toBeLessThan(1_000);

    const first = elasticStreamingStep(100, 10);
    const second = elasticStreamingStep(100 - first.characters, 10, first.remainder);
    expect(first.remainder).toBeGreaterThan(0);
    expect(second.characters).toBeGreaterThanOrEqual(first.characters);
    expect(elasticStreamingStep(0, 32, second.remainder)).toEqual({ characters: 0, remainder: 0 });
  });

  it('backs off presentation frames as rendered Markdown grows', () => {
    expect(elasticStreamingFrameDelay(8_000)).toBe(32);
    expect(elasticStreamingFrameDelay(8_001)).toBe(64);
    expect(elasticStreamingFrameDelay(32_001)).toBe(128);
    expect(elasticStreamingFrameDelay(64_001)).toBe(200);
  });

  it('bounds elastic catch-up after a suspended frame', () => {
    const ordinary = elasticStreamingStep(10_000, 250);
    const suspended = elasticStreamingStep(10_000, 30_000);
    expect(suspended).toEqual(ordinary);
    expect(suspended.characters).toBeLessThan(10_000);
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
    expect(hasIncrementalGlobalMarkdownSyntax('    - name: build\n      run: make')).toBe(true);
    expect(hasIncrementalGlobalMarkdownSyntax('  - nested\n    - deeply nested')).toBe(false);
    expect(hasIncrementalGlobalMarkdownSyntax('   1. nested\n      1. deeper')).toBe(false);
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

  it('gates the interactive shell and approval commands on server capabilities', () => {
    expect(composerCompletions('/sh', [], [], false, false).map((entry) => entry.value)).toEqual(
      [],
    );
    expect(composerCompletions('/sh', [], [], false, true).map((entry) => entry.value)).toEqual([
      '/shell',
    ]);
    expect(composerCompletions('/app', [], [], false, false, false)).toEqual([]);
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
      expect.arrayContaining(['/approvals', '/fork', '/thread', '/tree', '/side', '/review']),
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
