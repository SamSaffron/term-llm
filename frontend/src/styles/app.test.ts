import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

const stylesRoot = resolve(process.cwd(), 'src/styles');

// `app.css` is a manifest of ordered `@import`s. Resolve it the way the bundler
// does so these assertions keep covering every rule regardless of which module
// file a given block currently lives in.
function readStylesheet(path: string): string {
  const source = readFileSync(path, 'utf8');
  return source.replace(/@import\s+['"]([^'"]+)['"];/g, (_match, specifier: string) =>
    readStylesheet(resolve(dirname(path), specifier)),
  );
}

const appCSS = readStylesheet(resolve(stylesRoot, 'app.css'));
const preactOverrides = readFileSync(
  resolve(stylesRoot, 'integration/preact-overrides.css'),
  'utf8',
);

describe('stylesheet manifest', () => {
  it('inlines every module and keeps the Preact integration layer last', () => {
    const manifest = readFileSync(resolve(stylesRoot, 'app.css'), 'utf8');
    const imports = [...manifest.matchAll(/@import\s+['"]\.\/([^'"]+)['"];/g)].map((m) => m[1]);

    expect(imports.length).toBeGreaterThan(1);
    expect(imports[0]).toBe('base/tokens.css');
    expect(imports.at(-1)).toBe('integration/preact-overrides.css');

    // The manifest must stay import-only so its order is unambiguously the
    // cascade order.
    const statements = manifest
      .replace(/\/\*[\s\S]*?\*\//g, '')
      .split('\n')
      .map((line) => line.trim())
      .filter(Boolean);
    expect(statements).toHaveLength(imports.length);
    expect(statements.every((line) => line.startsWith('@import '))).toBe(true);
  });
});

describe('commit modal styles', () => {
  it('loads after the generic modal shell and uses defined hierarchy tokens', () => {
    const manifest = readFileSync(resolve(stylesRoot, 'app.css'), 'utf8');
    expect(manifest.indexOf("@import './features/modals.css';")).toBeLessThan(
      manifest.indexOf("@import './features/commit.css';"),
    );
    const commitCSS = readFileSync(resolve(stylesRoot, 'features/commit.css'), 'utf8');
    expect(commitCSS).not.toContain('var(--muted)');
    expect(commitCSS).not.toContain('var(--mono)');
    expect(commitCSS).toContain('var(--text-muted)');
  });
});

describe('shell layout', () => {
  it('stacks the mutually exclusive companion panels in one grid cell', () => {
    expect(appCSS).toMatch(
      /\.app > \.diff-sidebar,\s*\.app > \.plan-surface\s*\{\s*grid-column: 3;\s*grid-row: 1;/,
    );
  });

  it('keeps queued comment controls visible in file fullscreen mode', () => {
    expect(appCSS).toContain(
      '.diff-sidebar.file-fullscreen > :not(.diff-file-list, .diff-queue-bar)',
    );
  });
});

describe('stacking layers', () => {
  it('lifts an archived session row with its open menu above later rows', () => {
    expect(appCSS).toMatch(
      /\.session-row\.menu-open\s*\{[^}]*z-index:\s*var\(--z-session-menu-open\);/s,
    );
  });

  it('centralizes every z-index behind a semantic token', () => {
    const tokens = readFileSync(resolve(stylesRoot, 'base/tokens.css'), 'utf8');
    const declarations = [...appCSS.matchAll(/z-index:\s*([^;]+);/g)].map((match) =>
      match[1].trim(),
    );

    expect(declarations.length).toBeGreaterThan(0);
    for (const declaration of declarations) {
      expect(declaration).toMatch(/^var\((--z-[a-z-]+)\)$/);
      const token = declaration.match(/^var\((--z-[a-z-]+)\)$/)?.[1];
      expect(tokens).toContain(`${token}:`);
    }
  });
});

describe('header styles', () => {
  it('does not add a redundant chevron beside the runtime effort meter', () => {
    expect(appCSS).not.toContain('.model-chip::after');
  });

  it('keeps title descenders inside the ellipsis clipping box', () => {
    expect(appCSS).toMatch(
      /\.header-title\s*\{[^}]*padding-bottom: 0\.2em;[^}]*overflow: hidden;/s,
    );
  });

  it('gives clipped worktree labels enough line height for descenders', () => {
    expect(appCSS).toMatch(/\.worktree-trigger \.chip-label\s*\{[^}]*line-height: 1\.3;/s);
  });

  it('keeps the mobile header on one model-priority row', () => {
    const start = preactOverrides.indexOf('@media (width <= 767px)');
    const end = preactOverrides.indexOf('@media (hover: none)', start);
    const mobile = preactOverrides.slice(start, end);

    expect(start).toBeGreaterThanOrEqual(0);
    expect(end).toBeGreaterThan(start);
    expect(mobile).toMatch(/\.header-title-row\s*\{[^}]*flex-wrap: nowrap;/s);
    expect(mobile).toMatch(/\.header-left\s*\{[^}]*flex: 1 1 0;[^}]*min-width: 0;/s);
    expect(mobile).toMatch(/\.header-title-context\s*\{[^}]*overflow: hidden;/s);
    expect(mobile).toMatch(/\.header-project-subtitle\s*\{[^}]*display: none;/s);
    expect(mobile).not.toMatch(/\.header-title-row\s*\{[^}]*flex-wrap: wrap;/s);
  });
});

describe('widget launcher styles', () => {
  it('keeps the launcher bounded and its card grid independently scrollable', () => {
    expect(appCSS).toMatch(
      /\.modal\.widgets-modal\s*\{[^}]*max-height:[^;}]+;[^}]*overflow: hidden;/s,
    );
    expect(appCSS).toMatch(/\.widget-grid\s*\{[^}]*min-height: 0;[^}]*overflow-y: auto;/s);
    expect(appCSS).not.toContain('.widget-row');
    expect(appCSS).not.toContain('.widgets-modal-list');
  });
});

describe('close buttons', () => {
  it('uses a contained keyboard focus treatment instead of a detached outline', () => {
    expect(appCSS).toMatch(
      /\.close-button:focus-visible\s*\{[^}]*outline: none;[^}]*box-shadow: inset/s,
    );
    expect(appCSS).toMatch(
      /\.icon-btn\.close-button\s*\{[^}]*border-color: transparent;[^}]*background: transparent;/s,
    );
  });
});

describe('touch affordances', () => {
  it('keeps hover-revealed controls visible without a precise hovering pointer', () => {
    const touchRules = [
      ...appCSS.matchAll(/@media \(hover: none\), \(pointer: coarse\) \{([\s\S]*?)\n\}/g),
    ]
      .map((match) => match[1])
      .join('\n');

    for (const selector of [
      '.project-group-action',
      '.project-group-chevron',
      '.session-menu-trigger',
      '.session-group-chevron',
      '.code-copy-btn',
      '.math-copy-btn',
      '.diff-file-actions',
      '.diff-comment-affordance',
    ]) {
      expect(touchRules).toContain(selector);
    }
    expect(touchRules).toMatch(
      /\.diff-file-actions \.diff-action-btn\s*\{[^}]*min-width: 36px;[^}]*min-height: 36px;/s,
    );
    expect(touchRules).toMatch(
      /\.diff-file-actions \.diff-action-btn svg\s*\{[^}]*width: 17px;[^}]*height: 17px;/s,
    );
  });

  it('uses hover alone to reveal file actions for precise pointers', () => {
    expect(appCSS).toMatch(
      /@media \(hover: hover\) and \(pointer: fine\) \{[\s\S]*?\.diff-file-row:hover \.diff-file-actions\s*\{[^}]*display: inline-flex;/,
    );
    expect(appCSS).not.toContain('.diff-file-row:focus-within .diff-file-actions');
    expect(appCSS).not.toContain('.diff-file-row:focus-within .diff-file-counts');
  });
});

describe('diff review styles', () => {
  it('uses compact source-code spacing without letting comment controls expand every row', () => {
    expect(appCSS).toMatch(/\.diff-file-body\s*\{[^}]*line-height: 1\.3;/s);
    expect(appCSS).toMatch(/\.diff-comment-affordance\s*\{[^}]*height: 1\.15rem;/s);
  });

  it('sizes every file action glyph consistently', () => {
    expect(appCSS).toMatch(/\.diff-action-btn svg\s*\{[^}]*width: 19px;[^}]*height: 19px;/s);
  });

  it('anchors comment cards to code without pinning hunk text to the action column', () => {
    expect(appCSS).toMatch(/\.diff-comment-panel\s*\{[^}]*grid-column: 3 \/ -1;/s);
    expect(appCSS).not.toMatch(/\.diff-code\s*\{[^}]*grid-column:/s);
  });
});

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
    expect(appCSS).toMatch(
      /:is\(\.markdown-body,\s*\.diff-code\)\s*:is\(\s*\.hljs-doctag,\s*\.hljs-keyword/,
    );
    expect(appCSS).toMatch(/\.diff-code\s*:is\(\s*\.hljs-addition,\s*\.hljs-deletion\)/);
    expect(appCSS).toContain('background: transparent');
  });
});
