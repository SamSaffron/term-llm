import preact from '@preact/preset-vite';
import { existsSync } from 'node:fs';
import { resolve } from 'node:path';
import type { Plugin } from 'vite';
import { defineConfig } from 'vite';

const outputDirectory = resolve(import.meta.dirname, '../internal/serveui/static/dist');

function preserveChatBuild(): Plugin {
  return {
    name: 'term-llm-preserve-chat-build',
    buildStart() {
      for (const file of ['app.js', 'app.css']) {
        if (!existsSync(resolve(outputDirectory, file))) {
          throw new Error(`Hub build requires the chat build output dist/${file}`);
        }
      }
    },
    generateBundle(_options, bundle) {
      const chunks = Object.values(bundle).filter((output) => output.type === 'chunk');
      if (chunks.length !== 1 || chunks[0]?.fileName !== 'hub.js') {
        throw new Error('Hub must build as exactly one standalone JavaScript entry.');
      }
    },
    writeBundle(_options, bundle) {
      const assets = Object.values(bundle).filter((output) => output.type === 'asset');
      if (assets.length !== 1 || assets[0]?.fileName !== 'hub.css') {
        throw new Error(
          `Hub must build with exactly one standalone CSS asset and no other files: ${assets.map((asset) => asset.fileName).join(', ')}`,
        );
      }
      for (const file of ['app.js', 'app.css', 'hub.js', 'hub.css']) {
        if (!existsSync(resolve(outputDirectory, file))) {
          throw new Error(`Frontend build did not produce dist/${file}`);
        }
      }
    },
  };
}

export default defineConfig({
  base: './',
  plugins: [preserveChatBuild(), preact()],
  publicDir: false,
  build: {
    outDir: outputDirectory,
    emptyOutDir: false,
    manifest: false,
    sourcemap: false,
    assetsInlineLimit: 0,
    minify: 'esbuild',
    cssCodeSplit: false,
    rollupOptions: {
      input: resolve(import.meta.dirname, 'src/hub/main.tsx'),
      output: {
        entryFileNames: 'hub.js',
        assetFileNames: (asset) =>
          asset.names.some((name) => name.endsWith('.css')) ? 'hub.css' : 'hub-[name][extname]',
        // Vite 8's Rolldown output flag prevents dynamic/imported chunks; the
        // generateBundle assertion below also fails closed on future graph changes.
        codeSplitting: false,
      },
    },
  },
});
