// Boot the real agent with a deliberately small host stack. This catches the
// iPhone startup overflow without downloading a model or pretending to be iOS.
// Run with node --stack-size=64 startup-smoke.mjs [agent.wasm] [wasm_exec.js].
import { readFile } from 'node:fs/promises';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { resolve } from 'node:path';
const here = fileURLToPath(new URL('.', import.meta.url));
const wasm = resolve(process.argv[2] ?? here + 'dist/term-llm.wasm');
const runtime = resolve(process.argv[3] ?? here + 'dist/wasm_exec.js');
await import(pathToFileURL(runtime));
globalThis.termLLMInfer = async () => { throw new Error('Startup smoke must not request inference'); };
const go = new Go();
go.argv = ['browser-startup-smoke'];
go.env = { HOME: '/workspace', TMPDIR: '/tmp', USER: 'demo' };
const { instance } = await WebAssembly.instantiate(await readFile(wasm), go.importObject);
go.run(instance).catch(error => { console.error(error.stack); process.exit(1); });
setTimeout(() => {
  if (globalThis.termLLMBooted !== true) { console.error('Agent did not finish startup'); process.exit(1); }
  console.log('Real Go/WASM agent booted successfully with the configured host-stack limit');
  process.exit(0);
}, 100);
