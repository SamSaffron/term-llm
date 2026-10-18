import { describe, expect, it, vi } from 'vitest';
import DOMPurify from 'dompurify';
import { highlight, highlightDiffLine } from './rich-highlight';
import {
  decorateRichContent,
  renderMarkdown,
  stableMarkdownBoundary,
  rebaseRenderedAssetURLs,
} from './markdown';
import { audioMediaHTML, preserveAudioMediaCards } from './audio-media';
import { applyDocumentURLPolicy, markdownDocumentBlocks } from './markdown-document';
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
  completedCursor,
  composerCompletions,
  mentionCompletions,
  type MentionSearchResponse,
} from './completions';

describe('markdown security and streaming', () => {
  it('shares link hardening while retaining each parser’s existing tilde behavior', () => {
    const source = '~literal~ ~~deleted~~ [**label**](https://example.com/?a=1&b=2 "a & b")';
    for (const [html, documentMode] of [
      [renderMarkdown(source), false],
      [renderMarkdown(source, () => undefined), false],
      [
        markdownDocumentBlocks(source)
          .map((block) => block.html)
          .join(''),
        true,
      ],
    ] as const) {
      const root = document.createElement('div');
      root.innerHTML = html;
      // Documents use lexer/parser directly, which does not invoke walkTokens.
      expect([...root.querySelectorAll('del')].map((node) => node.textContent)).toEqual(
        documentMode ? ['literal', 'deleted'] : ['deleted'],
      );
      if (!documentMode) expect(root.textContent).toContain('~literal~');
      const link = root.querySelector('a')!;
      expect(link.getAttribute('href')).toBe('https://example.com/?a=1&b=2');
      expect(link.getAttribute('title')).toBe('a & b');
      expect(link.getAttribute('target')).toBe('_blank');
      expect(link.getAttribute('rel')).toBe('noopener noreferrer');
      expect(link.querySelector('strong')?.textContent).toBe('label');
    }
  });
  it('resolves registered term-llm media references with contextual alt text', () => {
    const reference = '0123456789abcdef0123456789abcdef';
    const resolve = (value: string) =>
      value === reference
        ? ({ url: '/ui/media/result.webm', type: 'video' as const } as const)
        : undefined;
    const html = renderMarkdown(
      `Before\n\n![Demo recording](term-llm-media://${reference})\n\nAfter`,
      resolve,
    );
    expect(html).toContain('<video');
    expect(html).toContain('src="/ui/media/result.webm"');
    expect(html).toContain('aria-label="Demo recording"');
    expect(html).toContain('controls');
    expect(html).not.toContain('term-llm-media://');
  });

  it('normalizes mixed-case media references before lookup', () => {
    const lower = 'abcdef0123456789abcdef0123456789';
    const html = renderMarkdown(`![Chart](term-llm-media://${lower.toUpperCase()})`, (reference) =>
      reference === lower ? { url: '/ui/media/result.png', type: 'image' } : undefined,
    );
    expect(html).toContain('<img');
    expect(html).toContain('alt="Chart"');
    expect(html).not.toContain('unavailable');
  });

  it('does not resolve term-llm media references inside code', () => {
    const reference = '0123456789abcdef0123456789abcdef';
    const html = renderMarkdown(`\`![example](term-llm-media://${reference})\``, () => ({
      url: '/ui/media/result.png',
      type: 'image',
    }));
    expect(html).toContain('term-llm-media://');
    expect(html).not.toContain('<img');
  });

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
      expect.arrayContaining([
        '/approvals',
        '/archive',
        '/fork',
        '/pin',
        '/rename',
        '/thread',
        '/tree',
        '/side',
        '/review',
      ]),
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
      token: { start_utf16: 7, end_utf16: 11, query: 'typ', quoted: false },
      items: [{ path: 'types.go', kind: 'file', insert_text: '@types.go', segments: [] }],
    });
    expect(applyCompletion('review @typ now', completion)).toBe('review @types.go now');
  });

  it('starts mention tokens where the server does, including quoted and repeated @', () => {
    // internal/mentions.ActiveTokenAt scans back for a boundary @, so the token
    // starts at the first @ of the run and quoted payloads may contain @.
    expect(activeMentionAtCursor('@a@b', 4)).toMatchObject({ start: 0, query: 'a@b' });
    expect(activeMentionAtCursor('see @"a@b', 9)).toMatchObject({ start: 4, query: 'a@b' });
    expect(activeMentionAtCursor('@types/node', 11)).toMatchObject({ start: 0 });
    expect(activeMentionAtCursor('see @"foo bar', 13)).toMatchObject({
      start: 4,
      query: 'foo bar',
    });
    // A trailing escape stays active until the quote closes, matching the server.
    expect(activeMentionAtCursor('see @"foo\\', 10)).toMatchObject({ start: 4, query: 'foo\\' });
    expect(activeMentionAtCursor('@"closed"', 9)).toBeNull();
    expect(activeMentionAtCursor('me@example.com', 14)).toBeNull();
  });

  it('rebinds stale mention payloads to the live token range', () => {
    const stale: MentionSearchResponse = {
      active: true,
      token: { start_utf16: 0, end_utf16: 3, query: 'ja', quoted: false },
      items: [{ path: 'jar.go', kind: 'file', insert_text: '@jar.go', segments: [] }],
    };
    const [completion] = mentionCompletions(stale, { start: 0, end: 4 });
    expect(completion.replacement).toEqual({ start: 0, end: 4 });
    expect(applyCompletion('@jab', completion)).toBe('@jar.go ');
    expect(completedCursor('@jab', completion)).toBe('@jar.go '.length);
    expect(mentionCompletions(stale, { start: 4, end: 7 })).toEqual([]);
  });
});

describe('trusted audio Markdown', () => {
  const reference = '0123456789abcdef0123456789abcdef';
  const url = `/ui/media/${reference}.ogg`;
  const source = `![Listen to the summary](term-llm-media://${reference})`;
  const renderAudio = (value = source, target = url) => {
    const root = document.createElement('div');
    root.innerHTML = renderMarkdown(value, () => ({
      url: target,
      type: 'audio',
      name: '../summary.ogg',
      caption: '<img src=x onerror=alert(1)> & details',
    }));
    return root;
  };

  it('renders an inline-compatible accessible card, escaped metadata and a persistent download', () => {
    const root = renderAudio(`Before ${source} after.`);
    expect(root.querySelector('p > .audio-media-card')).not.toBeNull();
    const audio = root.querySelector('audio')!;
    expect(audio).toHaveAttribute('src', url);
    expect(audio).toHaveAttribute('controls');
    expect(audio).toHaveAttribute('preload', 'metadata');
    expect(audio).toHaveAttribute('aria-label', 'Listen to the summary');
    expect(audio).toHaveAttribute('title', 'Listen to the summary');
    expect(audio).not.toHaveAttribute('autoplay');
    expect(audio).not.toHaveAttribute('loop');
    expect(root.querySelector('.audio-media-name')).toHaveTextContent('summary.ogg');
    expect(root.querySelector('.audio-media-caption')?.textContent).toBe(
      '<img src=x onerror=alert(1)> & details',
    );
    expect(root.querySelector('img')).toBeNull();
    const download = root.querySelector('a')!;
    expect(download).toHaveTextContent('Download audio');
    expect(download).toHaveAttribute('href', url);
    expect(download).toHaveAttribute('download', 'summary.ogg');
    expect(root.innerHTML).not.toContain('TERM_LLM_AUDIO_');
  });

  it.each([
    [{ caption: 'A caption', name: 'file.wav' }, 'A caption'],
    [{ name: 'file.wav' }, 'file.wav'],
    [{}, 'Audio recording'],
  ])('uses caption, filename, then generic accessible fallback: %s', (metadata, label) => {
    const root = document.createElement('div');
    root.innerHTML = renderMarkdown(`![](term-llm-media://${reference})`, () => ({
      url,
      type: 'audio',
      ...metadata,
    }));
    expect(root.querySelector('audio')).toHaveAttribute('aria-label', label);
    if ('caption' in metadata) expect(root.querySelector('.audio-media-caption')).toBeNull();
  });

  it.each([
    'javascript:alert(1)',
    'data:audio/wav;base64,AAAA',
    'blob:https://localhost/test',
    `https://tracker.example/media/${reference}.ogg`,
    `${location.origin.replace('://', '://user:pass@')}/ui/media/${reference}.ogg`,
    `${url}?token=secret`,
    `${url}#fragment`,
    `${url}?`,
    `/ui/uploads/${reference}.ogg`,
    '/ui/media/not-a-hash.ogg',
    `/ui/media/${reference}.mp4`,
  ])('rejects untrusted resolver URL %s', (target) => {
    const root = renderAudio(source, target);
    expect(root.querySelector('audio,a')).toBeNull();
    expect(root).toHaveTextContent('unavailable');
  });

  it('substitutes audio slots using only inert templates, even with ordinary images', () => {
    const createElement = vi.spyOn(document, 'createElement');
    try {
      const html = renderMarkdown(`${source}\n\n![Remote](https://example.com/image.png)`, () => ({
        url,
        type: 'audio',
      }));
      expect(html).toContain('<audio');
      expect(html).toContain('<img');
      expect(createElement).toHaveBeenCalledWith('template');
      expect(createElement).not.toHaveBeenCalledWith('div');
    } finally {
      createElement.mockRestore();
    }
  });

  it('places linked audio outside its enclosing anchor and retains the remaining link text', () => {
    const root = renderAudio(`[Before ${source} after](https://example.com)`);
    const card = root.querySelector('.audio-media-card')!;
    const anchor = root.querySelector('a[href="https://example.com"]')!;
    expect(anchor).toHaveTextContent('Before after');
    expect(anchor.querySelector('.audio-media-card,audio,a')).toBeNull();
    expect(card.closest('a')).toBeNull();
    expect(anchor.nextElementSibling).toBe(card);
    expect(card.querySelector('a')).toHaveAttribute('download', 'summary.ogg');
  });

  it('keeps multiple audio cards from one link in source order, outside the link', () => {
    const root = renderAudio(
      `[${source} ![Second](term-llm-media://${reference})](https://example.com)`,
    );
    const cards = [...root.querySelectorAll('.audio-media-card')];
    expect(cards.map((card) => card.querySelector('audio')?.getAttribute('aria-label'))).toEqual([
      'Listen to the summary',
      'Second',
    ]);
    expect(cards.every((card) => !card.closest('a'))).toBe(true);
    expect(cards[0].nextElementSibling).toBe(cards[1]);
  });

  it('does not reuse audio state across changed labels or sources, or different occurrences', () => {
    const previous = document.createElement('div');
    const prepared = document.createElement('div');
    previous.innerHTML = ['First', 'Second', 'Third']
      .map((label) => audioMediaHTML({ url, label }))
      .join('');
    const oldPlayers = [...previous.querySelectorAll('audio')];
    prepared.innerHTML =
      audioMediaHTML({ url, label: 'Changed' }) +
      audioMediaHTML({ url, label: 'Second' }) +
      audioMediaHTML({ url: url.replace('.ogg', '.wav'), label: 'Third' });
    const newPlayers = [...prepared.querySelectorAll('audio')];
    preserveAudioMediaCards(previous, prepared);
    const players = [...prepared.querySelectorAll('audio')];
    expect(players[0]).toBe(newPlayers[0]);
    expect(players[1]).toBe(oldPlayers[1]);
    expect(players[2]).toBe(newPlayers[2]);
    expect(previous.querySelectorAll('audio')).toHaveLength(2);
  });

  it('does not grant raw model HTML audio, source, autoplay or handlers permission', () => {
    const root = renderAudio(
      `<audio src="${url}" autoplay onplay="alert(1)"><source src="https://tracker.example/x"></audio>\n\n${source}`,
    );
    expect(root.querySelectorAll('audio')).toHaveLength(1);
    expect(root.querySelector('source,[autoplay],[onplay]')).toBeNull();
    expect(
      renderMarkdown(`<audio controls src="${url}"><source src="${url}"></audio>`),
    ).not.toContain('<audio');
  });

  it('ignores forged placeholder text, attributes, code and raw card wrappers', () => {
    const forged = 'TERM_LLM_AUDIO_00000000-0000-4000-8000-000000000000_TOKEN';
    const root = renderAudio(
      `<span class="audio-media-card" data-audio="0">${forged}</span>\n\n\`${forged}\`\n\n${source}`,
    );
    expect(root.querySelectorAll('audio')).toHaveLength(1);
    expect(root).toHaveTextContent(forged);
    expect(root.querySelector('code')).toHaveTextContent(forged);
    expect(
      renderAudio(`\`${source}\`\n\n\`\`\`md\n${source}\n\`\`\``).querySelector('audio'),
    ).toBeNull();
    expect(renderMarkdown(source, () => undefined)).toContain('unavailable');
  });
});

it('rebases audio/video sources and downloads without giving Ogg audio a video-link class', () => {
  const root = document.createElement('div');
  root.innerHTML =
    '<span class="audio-media-card"><audio src="/media/a.ogg"></audio><a href="/media/a.ogg" download="a.ogg">Download audio</a></span><video src="/media/b.mp4"></video><a href="/media/b.mp4">Video</a>';
  const rebase = (value: string) => (value.startsWith('/hub/node/') ? value : `/hub/node${value}`);
  rebaseRenderedAssetURLs(root, rebase);
  rebaseRenderedAssetURLs(root, rebase);
  expect(root.querySelector('audio')).toHaveAttribute('src', '/hub/node/media/a.ogg');
  expect(root.querySelector('video')).toHaveAttribute('src', '/hub/node/media/b.mp4');
  expect(root.querySelector('.audio-media-card a')).not.toHaveClass('video-link');
  expect(root.querySelector('a.video-link')).toHaveAttribute('href', '/hub/node/media/b.mp4');
});

it.each(['ogg', 'webm', 'mp4'])(
  'classifies bare .%s media links without treating Ogg audio as video',
  (extension) => {
    const root = document.createElement('div');
    root.innerHTML = renderMarkdown(
      `[Recording](/media/0123456789abcdef0123456789abcdef.${extension})`,
    );
    rebaseRenderedAssetURLs(root, (value) => value);
    expect(root.querySelector('a')?.classList.contains('video-link')).toBe(extension !== 'ogg');
  },
);

it('does not restore replayed audio placeholders from a previous render', () => {
  const sanitize = DOMPurify.sanitize.bind(DOMPurify);
  let oldPlaceholder = '';
  const spy = vi.spyOn(DOMPurify, 'sanitize').mockImplementation((source, options) => {
    oldPlaceholder = String(source).match(/data-audio-placeholder="([^"]+)"/)?.[1] || '';
    return sanitize(source, options) as never;
  });
  const reference = '0123456789abcdef0123456789abcdef';
  const audio = `![Listen](term-llm-media://${reference})`;
  const resolve = () => ({ url: `/ui/media/${reference}.wav`, type: 'audio' as const });
  renderMarkdown(audio, resolve);
  spy.mockRestore();
  expect(oldPlaceholder).not.toBe('');
  const root = document.createElement('div');
  root.innerHTML = renderMarkdown(
    `<span data-audio-placeholder="${oldPlaceholder}"></span>\n\n${audio}`,
    resolve,
  );
  expect(root.querySelectorAll('audio')).toHaveLength(1);
  expect(root.querySelector('[data-audio-placeholder]')).toHaveAttribute(
    'data-audio-placeholder',
    oldPlaceholder,
  );
  root.innerHTML = renderMarkdown(`<code>${audio}</code>`, resolve);
  expect(root.querySelector('audio')).toBeNull();
});

it('does not reset audio sources when a rebasing pass is already current', () => {
  const root = document.createElement('div');
  root.innerHTML = '<audio src="/hub/media/recording.wav"></audio>';
  const player = root.querySelector('audio')!;
  const src = vi.spyOn(player, 'src', 'set');
  rebaseRenderedAssetURLs(root, (value) => value);
  expect(src).not.toHaveBeenCalled();
});

it.each(['mp3', 'wav', 'ogg', 'flac', 'm4a', 'weba'])(
  'accepts canonical published audio extension .%s',
  (extension) => {
    const reference = '0123456789abcdef0123456789abcdef';
    const url = `/custom/chat/media/${reference}.${extension}`;
    const html = renderMarkdown(`![Listen](term-llm-media://${reference})`, () => ({
      url,
      type: 'audio',
    }));
    expect(html).toContain(`<audio src="${url}"`);
  },
);
