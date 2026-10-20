// Static-demo-only transport. No API requests leave the browser.
export function installBrowserTransport(worker, { prefix = '/demo-api', baseURL = location.href } = {}) {
  const nativeFetch = globalThis.fetch.bind(globalThis);
  const pending = new Map();
  let sequence = 0;
  const encoder = new TextEncoder();
  const abortWorker = (id) => worker.postMessage({ type: 'abort', id });
  worker.addEventListener('message', ({ data }) => {
    if (data.type !== 'http') return;
    const entry = pending.get(data.id);
    if (!entry) return;
    if (data.error) {
      const error = new Error(data.error);
      entry.controller?.error(error);
      entry.reject(error);
      entry.cleanup();
    } else if (data.event === 'headers') {
      const stream = new ReadableStream({
        start(controller) { entry.controller = controller; },
        cancel() { abortWorker(data.id); entry.cleanup(); },
      });
      entry.resolve(new Response([204, 205, 304].includes(data.status) ? null : stream, {
        status: data.status, headers: data.headers,
      }));
    } else if (data.event === 'chunk') {
      entry.controller?.enqueue(encoder.encode(data.chunk));
    } else if (data.event === 'end') {
      entry.controller?.close();
      entry.cleanup();
    }
  });
  globalThis.fetch = async (input, init = {}) => {
    const url = new URL(input instanceof Request ? input.url : input, baseURL);
    const root = new URL(prefix, baseURL);
    if (url.origin !== root.origin || !(url.pathname === root.pathname || url.pathname.startsWith(root.pathname + '/'))) {
      return nativeFetch(input, init);
    }
    const request = new Request(url, input instanceof Request ? input : undefined);
    const headers = Object.fromEntries(new Headers(init.headers ?? request.headers));
    const method = (init.method ?? request.method).toUpperCase();
    const signal = init.signal ?? (input instanceof Request ? input.signal : undefined);
    if (signal?.aborted) throw signal.reason ?? new DOMException('Aborted', 'AbortError');
    let body = '';
    if (method !== 'GET' && method !== 'HEAD') {
      body = init.body !== undefined ? await new Response(init.body).text() : await request.text();
    }
    return new Promise((resolve, reject) => {
      const id = ++sequence;
      const abort = () => {
        abortWorker(id);
        const error = signal.reason ?? new DOMException('Aborted', 'AbortError');
        const entry = pending.get(id);
        entry?.controller?.error(error);
        reject(error);
        entry?.cleanup();
      };
      const cleanup = () => { pending.delete(id); signal?.removeEventListener('abort', abort); };
      pending.set(id, { resolve, reject, cleanup });
      signal?.addEventListener('abort', abort, { once: true });
      worker.postMessage({ type: 'http', id, request: { method, path: url.pathname.slice(root.pathname.length) + url.search, headers, body } });
    });
  };
  return () => {
    globalThis.fetch = nativeFetch;
    for (const [id, entry] of pending) {
      abortWorker(id);
      const error = new Error('Demo runtime stopped');
      entry.controller?.error(error);
      entry.reject(error);
      entry.cleanup();
    }
  };
}

// The normal UI persists drafts separately from transcript caches. In the demo,
// all Web Storage is explicitly ephemeral too; model caches are unaffected.
export function memoryStorage() {
  const values = new Map();
  return {
    get length() { return values.size; },
    key(index) { return [...values.keys()][index] ?? null; },
    getItem(key) { return values.get(String(key)) ?? null; },
    setItem(key, value) { values.set(String(key), String(value)); },
    removeItem(key) { values.delete(String(key)); },
    clear() { values.clear(); },
  };
}

// The normal frontend uses its API prefix as its shell navigation base too.
// Keep its query-string routing, but stay on the static entry file so reloads
// work on any static host, including a subdirectory, without server rewrites.
export function installStaticNavigation(history, entryURL, prefix = '/demo-api') {
  const entry = new URL(entryURL);
  for (const method of ['pushState', 'replaceState']) {
    const original = history[method].bind(history);
    history[method] = (state, title, target) => {
      if (target != null) {
        const url = new URL(target, entry);
        if (url.origin === entry.origin && (url.pathname === prefix || url.pathname.startsWith(prefix + '/'))) {
          url.pathname = entry.pathname;
          target = url.href;
        }
      }
      return original(state, title, target);
    };
  }
}
