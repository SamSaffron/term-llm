import { defineConfig } from 'vite';
import preact from '@preact/preset-vite';
import { resolve } from 'node:path';

export default defineConfig({
  root: import.meta.dirname,
  base: './',
  plugins: [preact()],
  publicDir: false,
  build: {
    outDir: resolve(import.meta.dirname, '../../../tmp/safari-smoke'),
    emptyOutDir: true,
    minify: 'esbuild',
  },
});
