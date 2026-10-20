import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { transform } from 'esbuild';
import { MODEL_URL, MODEL_BYTES, openPinnedModelSource } from './web/inference.mjs';

// Exercise the actual unmodified vendored HTTP source, without a GPU or model
// download. Only TypeScript syntax is removed; no implementation is mocked.
const { code } = await transform(await readFile(new URL('./vendor/kernels.ts', import.meta.url), 'utf8'), { loader: 'ts', format: 'esm', target: 'es2022' });
const { UrlByteSource } = await import('data:text/javascript;base64,' + Buffer.from(code).toString('base64'));

test('pinned source loads when cached HEAD omits Accept-Ranges', async () => {
  const calls = [];
  const fetch = async (url, init) => {
    calls.push(init);
    if (init.method === 'HEAD') return new Response(null, { headers: { 'Content-Length': String(MODEL_BYTES) } });
    return new Response(new Uint8Array([71, 71, 85, 70]), { status: 206, headers: { 'Content-Range': `bytes 0-3/${MODEL_BYTES}`, 'Content-Length': '4' } });
  };
  // Reproduce the original failure from a valid but unadvertised range source.
  await assert.rejects(UrlByteSource.open(MODEL_URL, { fetch, retries: 0 }), /did not advertise Accept-Ranges/);
  calls.length = 0;
  const source = await openPinnedModelSource({ open: (url, opts) => UrlByteSource.open(url, { ...opts, fetch, retries: 0 }) });
  assert.equal(source.size, MODEL_BYTES);
  assert.equal(calls.length, 0, 'opening a pinned source must not probe HEAD');
  assert.deepEqual(await source.readRange(0, 4), new Uint8Array([71, 71, 85, 70]));
  assert.equal(calls.length, 1);
  assert.equal(calls[0].headers.Range, 'bytes=0-3');
});

test('pinned source still rejects servers that ignore Range and return 200', async () => {
  const source = await openPinnedModelSource({ open: (url, opts) => UrlByteSource.open(url, {
    ...opts, retries: 0, fetch: async () => new Response(new Uint8Array([71, 71, 85, 70]), { status: 200 }),
  }) });
  await assert.rejects(source.readRange(0, 4), /[Rr]ange|206/);
});
