import assert from 'node:assert/strict';
import { test } from 'node:test';
import { installBrowserTransport, memoryStorage } from './web/transport.mjs';

class WorkerStub extends EventTarget {
  sent = [];
  postMessage(message) { this.sent.push(message); }
  emit(data) { this.dispatchEvent(new MessageEvent('message', { data })); }
}

test('API transport stays in worker and preserves streamed chunks', async () => {
  const worker = new WorkerStub();
  const restore = installBrowserTransport(worker, { baseURL: 'https://demo.example/' });
  try {
    const promise = fetch('https://demo.example/demo-api/v1/responses', { method: 'POST', body: '{"input":"hi"}' });
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(worker.sent[0].request.path, '/v1/responses');
    assert.equal(worker.sent[0].request.body, '{"input":"hi"}');
    worker.emit({ type: 'http', id: 1, event: 'headers', status: 200, headers: { 'Content-Type': 'text/event-stream' } });
    const response = await promise;
    worker.emit({ type: 'http', id: 1, event: 'chunk', chunk: 'data: hello\n\n' });
    worker.emit({ type: 'http', id: 1, event: 'end' });
    assert.equal(response.headers.get('content-type'), 'text/event-stream');
    assert.equal(await response.text(), 'data: hello\n\n');
  } finally { restore(); }
});

test('aborting an active stream cancels the worker operation', async () => {
  const worker = new WorkerStub();
  const restore = installBrowserTransport(worker, { baseURL: 'https://demo.example/' });
  try {
    const controller = new AbortController();
    const promise = fetch('https://demo.example/demo-api/events', { signal: controller.signal });
    worker.emit({ type: 'http', id: 1, event: 'headers', status: 200 });
    const response = await promise;
    controller.abort();
    await assert.rejects(response.text(), { name: 'AbortError' });
    assert.deepEqual(worker.sent.at(-1), { type: 'abort', id: 1 });
  } finally { restore(); }
});

test('static asset requests use the original fetch', async () => {
  const original = globalThis.fetch;
  globalThis.fetch = async () => new Response('static');
  const restore = installBrowserTransport(new WorkerStub(), { baseURL: 'https://demo.example/' });
  try {
    assert.equal(await (await fetch('https://demo.example/app.js')).text(), 'static');
    assert.equal(await (await fetch('https://other.example/demo-api/events')).text(), 'static');
  } finally { restore(); globalThis.fetch = original; }
});

test('storage is string-valued and isolated for every demo runtime', () => {
  const a = memoryStorage();
  a.setItem('draft', 42);
  assert.equal(a.getItem('draft'), '42');
  assert.equal(a.length, 1);
  assert.equal(memoryStorage().getItem('draft'), null);
  a.clear();
  assert.equal(a.getItem('draft'), null);
});

test('frontend navigation stays on the static entry while retaining session queries', async () => {
  const { installStaticNavigation } = await import('./web/transport.mjs');
  const calls = [];
  const history = { pushState: (...args) => calls.push(args), replaceState: (...args) => calls.push(args) };
  installStaticNavigation(history, 'https://demo.example/poc/index.html');
  history.replaceState({}, '', '/demo-api/?session=s1');
  assert.equal(calls[0][2], 'https://demo.example/poc/index.html?session=s1');
  history.pushState({}, '', '/unrelated');
  assert.equal(calls[1][2], '/unrelated');
});
