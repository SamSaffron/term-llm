import { defineConfig } from 'vitest/config';
import type { Plugin } from 'vite';
import preact from '@preact/preset-vite';
import { resolve } from 'node:path';
import { readFileSync, writeFileSync } from 'node:fs';

function deterministicStyles(): Plugin {
  return {
    name: 'term-llm-deterministic-styles',
    enforce: 'pre',
    transform(code, id) {
      if (!id.includes('/katex/dist/katex.min.css')) return null;
      return {
        code: code.replace(
          /src:url\(([^)]+\.woff2)\) format\("woff2"\),url\([^)]+\.woff\) format\("woff"\),url\([^)]+\.ttf\) format\("truetype"\)/g,
          'src:url($1) format("woff2")',
        ),
        map: null,
      };
    },
    generateBundle(_options, bundle) {
      const renamed = new Map<string, string>();
      for (const output of Object.values(bundle)) {
        if (output.type !== 'asset' || !output.fileName.endsWith('.css')) continue;
        const css = String(output.source);
        const target =
          css.length > 100_000
            ? 'app.css'
            : css.includes('KaTeX_Main')
              ? 'chunks/katex.css'
              : css.includes('.hljs')
                ? 'chunks/highlight.css'
                : `assets/${output.fileName.split('/').pop()}`;
        renamed.set(output.fileName, target);
        output.fileName = target;
      }
      for (const output of Object.values(bundle)) {
        if (output.type !== 'chunk') continue;
        for (const [before, after] of renamed) output.code = output.code.replaceAll(before, after);
        output.code = output.code
          .replaceAll('./assets/highlight.css', './chunks/highlight.css')
          .replaceAll('./assets/katex.css', './chunks/katex.css');
      }
    },
    closeBundle() {
      // Vite injects __vite__mapDeps after Rollup's generateBundle hooks. Keep
      // those generated preload URLs aligned with the deterministic CSS moves.
      const entry = resolve(import.meta.dirname, '../internal/serveui/static/dist/app.js');
      const code = readFileSync(entry, 'utf8')
        .replaceAll('./assets/highlight.css', './chunks/highlight.css')
        .replaceAll('./assets/katex.css', './chunks/katex.css');
      writeFileSync(entry, code);
    },
  };
}

export default defineConfig({
  base: './',
  plugins: [deterministicStyles(), preact()],
  publicDir: false,
  build: {
    outDir: resolve(import.meta.dirname, '../internal/serveui/static/dist'),
    emptyOutDir: true,
    manifest: false,
    sourcemap: false,
    assetsInlineLimit: 0,
    minify: 'esbuild',
    rollupOptions: {
      input: resolve(import.meta.dirname, 'src/main.tsx'),
      output: {
        entryFileNames: 'app.js',
        chunkFileNames: 'chunks/[name].js',
        assetFileNames: 'assets/[name][extname]',
        manualChunks(id) {
          if (id.includes('/katex/')) return 'katex';
          if (id.includes('/highlight.js/')) return 'highlight';
          if (id.endsWith('/src/stores/mcp-store.ts')) return 'mcp';
          if (
            id.includes('/node_modules/preact/') ||
            id.includes('/node_modules/@preact/signals/') ||
            id.includes('/node_modules/marked/') ||
            id.includes('/node_modules/dompurify/')
          )
            return 'vendor';
          return undefined;
        },
      },
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    include: ['src/**/*.test.ts', 'src/**/*.test.tsx'],
    outputFile: undefined,
  },
});
