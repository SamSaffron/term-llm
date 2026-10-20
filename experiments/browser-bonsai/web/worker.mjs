import { loadBonsai } from './inference.bundle.mjs';
import { proseStream } from './prose-stream.mjs';
// Shell execution lives in a separate page-owned worker, not a nested worker
// (important for mobile engines and consistent module loading).
let pendingShell;
let shellSequence = 0;
globalThis.termLLMRunShell = requestJSON => {
  if (pendingShell) return Promise.reject(new Error('Linux shell is already running'));
  return new Promise((resolve, reject) => {
    pendingShell = { id: ++shellSequence, resolve, reject };
    postMessage({ type: 'run-shell', id: pendingShell.id, request: requestJSON });
  });
};
globalThis.termLLMCancelShell = () => {
  if (!pendingShell) return;
  postMessage({ type: 'cancel-shell', id: pendingShell.id });
  pendingShell.reject(new Error('Linux command cancelled'));
  pendingShell = undefined;
};
self.addEventListener('message', ({ data }) => {
  if (data.type !== 'shell-result' || data.id !== pendingShell?.id) return;
  const pending = pendingShell; pendingShell = undefined;
  data.error ? pending.reject(new Error(data.error)) : pending.resolve(data.result);
});

let inference;
let booted;
let activeInference;
let bootStage = 'Loading worker modules';
const progress = event => postMessage({ type: 'progress', event });
const stage = message => { bootStage = message; progress({ status: message, file: '' }); };
const failure = error => ({ error: error?.message ?? String(error), stack: error?.stack ?? '', stage: bootStage });

globalThis.termLLMCancelInference = () => activeInference?.abort();
globalThis.termLLMInfer = globalThis.__termLLMInfer = async (requestJSON, onDelta) => {
  if (!inference) throw new Error('Load Bonsai before sending a message');
  const request = JSON.parse(requestJSON);
  activeInference = new AbortController();
  const stream = proseStream(text => onDelta?.(JSON.stringify({ choices: [{ index: 0, delta: { content: text } }] })));
  let result;
  try {
    result = await inference.complete(request, { signal: activeInference.signal, onText: text => stream.push(text) });
    stream.end();
  } finally { activeInference = undefined; }
  return JSON.stringify(result);
};

async function boot() {
  stage('Loading Go runtime');
  await import('./wasm_exec.js');
  const go = new Go();
  go.argv = ['term-llm-browser'];
  go.env = { HOME: '/workspace', TMPDIR: '/tmp', USER: 'demo' };
  const response = await fetch(new URL('./term-llm.wasm', import.meta.url));
  if (!response.ok) throw new Error(`WASM HTTP ${response.status}`);
  stage('Compiling term-llm WASM');
  const { instance } = await WebAssembly.instantiateStreaming(response, go.importObject);
  stage('Starting term-llm WASM');
  go.run(instance).catch(error => postMessage({ type: 'fatal', ...failure(error) }));
  const started = Date.now();
  while (!globalThis.termLLMReady) {
    if (Date.now() - started > 30000) throw new Error('WASM runtime did not initialise');
    await new Promise(resolve => setTimeout(resolve, 20));
  }
  await globalThis.termLLMReady;
  if (typeof globalThis.termLLMRequest !== 'function') throw new Error('WASM request bridge is missing');
  return typeof globalThis.termLLMInfo === 'function' ? globalThis.termLLMInfo() : {};
}

self.addEventListener('message', async ({ data }) => {
  try {
    if (data.type === 'boot') {
      booted ??= boot();
      postMessage({ type: 'ready', info: await booted });
    } else if (data.type === 'load-model') {
      stage('Loading Bonsai WebGPU model');
      inference ??= await loadBonsai({ progress });
      postMessage({ type: 'model-ready', model: inference.model, context: inference.context });
    } else if (data.type === 'http') {
      await booted;
      let headersSent = false;
      let ended = false;
      await globalThis.termLLMRequest(JSON.stringify({ ...data.request, id: String(data.id), path: '/demo-api' + data.request.path }), (status, headersJSON, chunk, done) => {
        if (!headersSent && status) {
          postMessage({ type: 'http', id: data.id, event: 'headers', status, headers: JSON.parse(headersJSON || '{}') });
          headersSent = true;
        }
        if (chunk) postMessage({ type: 'http', id: data.id, event: 'chunk', chunk });
        if (done && !ended) { ended = true; postMessage({ type: 'http', id: data.id, event: 'end' }); }
      });
    } else if (data.type === 'abort') {
      globalThis.termLLMAbort?.(String(data.id));
    } else if (data.type === 'workspace') {
      postMessage({ type: 'workspace', files: globalThis.__termLLMWorkspace?.() ?? [] });
    }
  } catch (error) {
    postMessage({ type: data.type === 'http' ? 'http' : 'error', id: data.id, operation: data.type, ...failure(error) });
  }
});
