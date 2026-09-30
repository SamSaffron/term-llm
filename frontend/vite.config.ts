import { defineConfig } from 'vitest/config';
import type { Plugin } from 'vite';
import preact from '@preact/preset-vite';
import { resolve } from 'node:path';

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
  };
}

export default defineConfig({
  base: './',
  plugins: [deterministicStyles(), preact()],
  publicDir: false,
  build: {
    chunkSizeWarningLimit: 550,
    outDir: resolve(import.meta.dirname, '../internal/serveui/static/dist'),
    emptyOutDir: true,
    manifest: 'asset-manifest.json',
    sourcemap: false,
    assetsInlineLimit: 0,
    minify: 'esbuild',
    rollupOptions: {
      input: resolve(import.meta.dirname, 'src/main.tsx'),
      output: {
        entryFileNames: 'app-[hash].js',
        chunkFileNames: 'chunks/[name]-[hash].js',
        // Name CSS before Vite creates preload maps and Rollup hashes the graph.
        // No post-hash rewriting: every import and preload retains one identity.
        assetFileNames(asset) {
          const name = asset.names[0] ?? '';
          if (name === 'main.css') return 'app-[hash][extname]';
          if (name === 'katex.css' || name === 'highlight.css')
            return 'chunks/[name]-[hash][extname]';
          return 'assets/[name]-[hash][extname]';
        },
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
    // jsdom integration tests take ~1 s alone and several times that when the
    // whole suite saturates the CPU. The timeout exists to catch hangs, not to
    // measure speed, so leave headroom for loaded machines and CI.
    testTimeout: 20_000,
  },
});
