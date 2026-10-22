import { readFileSync } from 'node:fs';
import { afterEach, describe, expect, it, vi } from 'vitest';

const source = readFileSync(
  new URL('../internal/serveui/static/sw.js', `file://${process.cwd()}/`),
  'utf8',
);

type WorkerListener = (event: {
  data?: unknown;
  respondWith?(value: Promise<Response>): void;
  notification?: { data?: { url?: string }; close(): void };
  ports?: Array<{ postMessage(message: unknown): void }>;
  waitUntil(value: Promise<unknown>): void;
}) => void;

type MemoryCaches = Map<string, Map<string, Response>>;

function workerHarness(
  assets: string[] = [],
  version = 'v7',
  storage: MemoryCaches = new Map(),
  scope = 'https://example.test/ui/',
) {
  const key = (request: string | { url: string }) =>
    typeof request === 'string' ? request : request.url;
  const cacheStorage = {
    keys: async () => [...storage.keys()],
    delete: async (name: string) => storage.delete(name),
    open: async (name: string) => {
      if (!storage.has(name)) storage.set(name, new Map());
      const entries = storage.get(name)!;
      return {
        match: async (request: string | { url: string }) => entries.get(key(request))?.clone(),
        put: async (request: string | { url: string }, response: Response) => {
          entries.set(key(request), response.clone());
        },
        keys: async () => [...entries.keys()].map((url) => ({ url })),
        delete: async (request: string | { url: string }) => entries.delete(key(request)),
      };
    },
  };
  const network = vi.fn(async () => new Response('network'));
  const listeners = new Map<string, WorkerListener>();
  const outstanding = new Map<string, { data: Record<string, unknown>; close(): void }>();
  const showNotification = vi.fn(
    async (_title: string, options: { tag?: string; data: Record<string, unknown> }) => {
      const tag = options.tag || `legacy:${outstanding.size}`;
      outstanding.set(tag, { data: options.data, close: () => outstanding.delete(tag) });
    },
  );
  const client = {
    id: 'client-owner',
    url: new URL('chat/one', scope).href,
    visibilityState: 'visible',
    focus: vi.fn(async () => undefined),
    postMessage: vi.fn(),
  };
  const windows = [client];
  const worker = {
    location: { origin: 'https://example.test' },
    registration: {
      scope,
      showNotification,
      getNotifications: vi.fn(async (options?: { tag: string }) =>
        options
          ? outstanding.has(options.tag)
            ? [outstanding.get(options.tag)]
            : []
          : [...outstanding.values()],
      ),
    },
    clients: {
      claim: vi.fn(),
      matchAll: vi.fn(async () => windows),
      openWindow: vi.fn(),
    },
    skipWaiting: vi.fn(),
    addEventListener: (name: string, listener: WorkerListener) => listeners.set(name, listener),
  };
  Object.defineProperty(globalThis, 'self', { configurable: true, value: worker });
  const rendered = source
    .replace('const HASHED_ASSETS = [];', `const HASHED_ASSETS = ${JSON.stringify(assets)};`)
    .replace('term-llm-shell-v7', `term-llm-shell-${version}`);
  new Function('caches', 'fetch', rendered)(cacheStorage, network);
  const dispatch = async (name: string, event: Record<string, unknown>) => {
    let completion: Promise<unknown> = Promise.resolve();
    listeners.get(name)?.({
      ...event,
      waitUntil(value) {
        completion = Promise.resolve(value);
      },
    });
    await completion;
  };
  const request = async (path: string, destination = 'script', mode = 'cors') => {
    let result: Promise<Response> | undefined;
    await dispatch('fetch', {
      request: {
        url: new URL(path, worker.registration.scope).href,
        method: 'GET',
        destination,
        mode,
      },
      respondWith(value: Promise<Response>) {
        result = value;
      },
    });
    return result;
  };
  return {
    dispatch,
    showNotification,
    getNotifications: worker.registration.getNotifications,
    outstanding,
    client,
    windows,
    clients: worker.clients,
    request,
    network,
    storage,
    cacheStorage,
  };
}

afterEach(() => vi.restoreAllMocks());

const completionPayload = (overrides: Record<string, unknown> = {}) => ({
  version: 2,
  kind: 'completion',
  event_id: 'completion:turn:sub',
  turn_id: 'turn',
  response_id: 'resp',
  window_id: 'window-owner',
  store_instance_id: 'store',
  session_id: 'session',
  attention_seq: 3,
  run_epoch: 1,
  source: '/ui',
  outcome: 'completed',
  title: 'Response complete',
  body: 'Ready',
  url: '/ui/chat/session',
  created_at: new Date(Date.now()).toISOString(),
  expires_at: new Date(Date.now() + 60_000).toISOString(),
  ...overrides,
});
const pushEvent = (payload: unknown) => ({ data: { json: () => payload } });
const manualPayload = (overrides: Record<string, unknown> = {}) => ({
  version: 2,
  kind: 'manual',
  event_id: 'manual:event',
  source: '/ui',
  title: 'A real manual title',
  body: 'Actual manual body',
  url: '/ui/',
  created_at: new Date(Date.now()).toISOString(),
  expires_at: new Date(Date.now() + 60_000).toISOString(),
  ...overrides,
});
const bind = async (
  harness: ReturnType<typeof workerHarness>,
  client = harness.client,
  windowId = 'window-owner',
  documentId = 'document-owner',
) => {
  const reply = { postMessage: vi.fn() };
  await harness.dispatch('message', {
    source: client,
    ports: [reply],
    data: { type: 'notification-bind', version: 2, window_id: windowId, document_id: documentId },
  });
  return reply.postMessage.mock.calls[0]?.[0];
};

// The server is the only completion authority. Visibility is not attention.
describe('service worker v2 notification receiver', () => {
  it('durably deduplicates completion and manual events, even after worker restart', async () => {
    const storage: MemoryCaches = new Map();
    const harness = workerHarness([], 'v7', storage);
    const payload = completionPayload();
    await harness.dispatch('push', pushEvent(payload));
    await harness.dispatch('push', pushEvent(payload));
    expect(harness.showNotification).toHaveBeenCalledOnce();
    expect(harness.client.postMessage).not.toHaveBeenCalled();
    const restarted = workerHarness([], 'v8', storage);
    await restarted.dispatch('push', pushEvent(payload));
    expect(restarted.showNotification).not.toHaveBeenCalled();
    await restarted.dispatch('push', pushEvent(manualPayload()));
    await restarted.dispatch('push', pushEvent(manualPayload()));
    expect(restarted.showNotification).toHaveBeenCalledOnce();
    expect(restarted.showNotification).toHaveBeenCalledWith(
      'A real manual title',
      expect.objectContaining({ body: 'Actual manual body', tag: 'term-llm-manual:manual:event' }),
    );
  });

  it('preserves only the strict legacy manual shape and ignores retired local completion messages', async () => {
    const harness = workerHarness();
    await harness.dispatch('push', pushEvent({ title: 'Legacy title', body: 'Legacy body' }));
    expect(harness.showNotification).toHaveBeenCalledWith(
      'Legacy title',
      expect.objectContaining({ body: 'Legacy body' }),
    );
    await harness.dispatch('message', {
      source: harness.client,
      data: { type: 'completion-notification', payload: completionPayload() },
    });
    expect(harness.showNotification).toHaveBeenCalledOnce();
  });

  it.each([
    null,
    { title: 'Bad', body: 'Bad', response_id: 'r' },
    { version: 1, event_id: 'old', title: 'Old auto', body: 'No' },
    { version: 9, kind: 'completion', title: 'Unknown', body: 'No' },
    completionPayload({ window_id: '' }),
    completionPayload({ attention_seq: 1.5 }),
    completionPayload({ source: '/another-node' }),
    completionPayload({ expires_at: 'bad' }),
    manualPayload({ response_id: 'r' }),
  ])(
    'drops malformed/unsupported/cross-scope payloads rather than treating them as manual: %j',
    async (payload) => {
      const harness = workerHarness();
      await harness.dispatch('push', pushEvent(payload));
      expect(harness.showNotification).not.toHaveBeenCalled();
    },
  );

  it.each([0, undefined])(
    'rejects completion with zero or missing run_epoch: %s',
    async (epoch) => {
      const harness = workerHarness();
      // Match the wire shape: an undefined epoch is absent from the JSON envelope.
      const payload = JSON.parse(JSON.stringify(completionPayload({ run_epoch: epoch })));
      await harness.dispatch('push', pushEvent(payload));
      expect(harness.showNotification).not.toHaveBeenCalled();
      expect(harness.outstanding.size).toBe(0);
    },
  );

  it('preserves typed manual content with a local source under a different public Hub mount', async () => {
    const harness = workerHarness([], 'v7', new Map(), 'https://example.test/node/Dev/');
    await harness.dispatch(
      'push',
      pushEvent(manualPayload({ source: '/ui', url: '/ui/chat/session' })),
    );
    expect(harness.showNotification).toHaveBeenCalledWith(
      'A real manual title',
      expect.objectContaining({
        body: 'Actual manual body',
        data: expect.objectContaining({ url: 'https://example.test/node/Dev/' }),
      }),
    );
  });

  it('binds actual WindowClient identity, rejects cloned live ownership and accepts reload', async () => {
    const harness = workerHarness();
    const sibling = { ...harness.client, id: 'sibling', postMessage: vi.fn(), focus: vi.fn() };
    harness.windows.unshift(sibling);
    expect(await bind(harness)).toEqual({ status: 'bound' });
    expect(await bind(harness, sibling)).toEqual({ status: 'collision' });
    expect(await bind(harness, sibling, 'window-sibling', 'doc-sibling')).toEqual({
      status: 'bound',
    });
    const payload = completionPayload();
    await harness.dispatch('notificationclick', {
      notification: { data: payload, close: vi.fn() },
    });
    expect(harness.client.focus).toHaveBeenCalledOnce();
    expect(sibling.focus).not.toHaveBeenCalled();
    expect(harness.client.postMessage).toHaveBeenCalledWith({
      type: 'notification-route',
      url: 'https://example.test/ui/chat/session',
    });
    const reloaded = { ...harness.client, id: 'reloaded', focus: vi.fn(), postMessage: vi.fn() };
    harness.windows.splice(harness.windows.indexOf(harness.client), 1, reloaded);
    expect(await bind(harness, reloaded, 'window-owner', 'new-doc')).toEqual({
      status: 'bound',
      previous_document_id: 'document-owner',
    });
    await harness.dispatch('notificationclick', {
      notification: { data: payload, close: vi.fn() },
    });
    expect(reloaded.focus).toHaveBeenCalledOnce();
    harness.windows.splice(harness.windows.indexOf(reloaded), 1);
    await harness.dispatch('notificationclick', {
      notification: { data: payload, close: vi.fn() },
    });
    expect(harness.clients.openWindow).toHaveBeenCalledWith('https://example.test/ui/chat/session');
    expect(sibling.focus).not.toHaveBeenCalled();
  });

  it('persists routing across worker restart and opens fresh for manual/unknown owner', async () => {
    const storage: MemoryCaches = new Map();
    const one = workerHarness([], 'v7', storage);
    await bind(one);
    const restarted = workerHarness([], 'v8', storage);
    await restarted.dispatch('notificationclick', {
      notification: { data: completionPayload(), close: vi.fn() },
    });
    expect(restarted.client.focus).toHaveBeenCalledOnce();
    await restarted.dispatch('notificationclick', {
      notification: { data: manualPayload({ url: 'https://evil.test/' }), close: vi.fn() },
    });
    expect(restarted.clients.openWindow).toHaveBeenCalledWith('https://example.test/ui/');
  });

  it('clears only exact successful attention through the ACK sequence, not newer/manual/other sources', async () => {
    const harness = workerHarness();
    for (const payload of [
      completionPayload(),
      completionPayload({ event_id: 'newer', attention_seq: 4 }),
      completionPayload({ event_id: 'other-store', store_instance_id: 'other' }),
      completionPayload({ event_id: 'other-session', session_id: 'other' }),
      manualPayload(),
    ])
      await harness.dispatch('push', pushEvent(payload));
    expect(harness.outstanding.size).toBe(5);
    await harness.dispatch('message', {
      source: harness.client,
      data: {
        type: 'notification-read',
        version: 2,
        store_instance_id: 'store',
        source: '/ui',
        session_id: 'session',
        attention_seq: 3,
      },
    });
    expect(harness.outstanding.size).toBe(4);
    expect(harness.outstanding.has('term-llm-completion:newer')).toBe(true);
    expect(harness.outstanding.has('term-llm-manual:manual:event')).toBe(true);
    await harness.dispatch('push', pushEvent(completionPayload({ event_id: 'late-retry' })));
    expect(harness.outstanding.size).toBe(4);
  });

  it.each([
    { read: {}, closes: true },
    { read: { attention_seq: 8 }, closes: true },
    { read: { attention_seq: 2 }, closes: false },
    { read: { store_instance_id: 'other' }, closes: false },
    { read: { session_id: 'other' }, closes: false },
    { read: { source: '/another-node' }, closes: false },
  ])('rechecks exact reads after a deferred OS display: %j', async ({ read, closes }) => {
    const harness = workerHarness();
    const display = harness.showNotification.getMockImplementation()!;
    let releaseDisplay!: () => void;
    const displayGate = new Promise<void>((resolve) => {
      releaseDisplay = resolve;
    });
    let signalStarted!: () => void;
    const displayStarted = new Promise<void>((resolve) => {
      signalStarted = resolve;
    });
    harness.showNotification.mockImplementationOnce(async (title, options) => {
      signalStarted();
      await displayGate;
      await display(title, options);
    });

    const payload = completionPayload();
    const push = harness.dispatch('push', pushEvent(payload));
    await displayStarted;
    // Reservation must already be durable while the OS display is still pending.
    const cache = [...harness.storage.entries()].find(([key]) =>
      key.startsWith('term-llm-notifications-'),
    )![1];
    const state = await [...cache.values()][0].clone().json();
    expect(state.events).toContainEqual(expect.objectContaining({ id: payload.event_id }));
    await harness.dispatch('message', {
      source: harness.client,
      data: {
        type: 'notification-read',
        version: 2,
        store_instance_id: 'store',
        source: '/ui',
        session_id: 'session',
        attention_seq: 3,
        ...read,
      },
    });
    // The concurrent read finishes and snapshots no cards before display resolves.
    if (read.source !== '/another-node') {
      expect(harness.getNotifications).toHaveBeenCalledOnce();
      expect(await harness.getNotifications.mock.results[0].value).toEqual([]);
    }
    expect(harness.outstanding.size).toBe(0);
    for (const other of [
      completionPayload({ event_id: 'newer', attention_seq: 20 }),
      completionPayload({ event_id: 'other-store', store_instance_id: 'unrelated' }),
      completionPayload({ event_id: 'other-session', session_id: 'unrelated' }),
      manualPayload(),
    ])
      await harness.dispatch('push', pushEvent(other));

    releaseDisplay();
    await push;
    expect(harness.outstanding.has(`term-llm-completion:${payload.event_id}`)).toBe(!closes);
    expect(harness.outstanding.size).toBe(closes ? 4 : 5);
    expect(harness.outstanding.has('term-llm-completion:newer')).toBe(true);
    expect(harness.outstanding.has('term-llm-completion:other-store')).toBe(true);
    expect(harness.outstanding.has('term-llm-completion:other-session')).toBe(true);
    expect(harness.outstanding.has('term-llm-manual:manual:event')).toBe(true);
    // Cleanup does not release the replay reservation or permit another sound.
    await harness.dispatch('push', pushEvent(payload));
    expect(harness.showNotification).toHaveBeenCalledTimes(5);
  });

  it('fails closed before OS display when durable replay reservation cannot be written', async () => {
    const harness = workerHarness();
    const cache = await harness.cacheStorage.open('test');
    vi.spyOn(cache, 'put').mockRejectedValue(new Error('quota denied'));
    vi.spyOn(harness.cacheStorage, 'open').mockResolvedValue(cache);
    await harness.dispatch('push', pushEvent(completionPayload()));
    expect(harness.showNotification).not.toHaveBeenCalled();
  });

  it('durably gates incoming pushes after browser opt-out, and only explicit enable restores', async () => {
    const storage: MemoryCaches = new Map();
    const one = workerHarness([], 'v7', storage);
    await one.dispatch('message', {
      source: one.client,
      data: { type: 'notification-preference', enabled: false },
    });
    const restarted = workerHarness([], 'v8', storage);
    await restarted.dispatch('push', pushEvent(completionPayload()));
    expect(restarted.showNotification).not.toHaveBeenCalled();
    await restarted.dispatch('message', {
      source: restarted.client,
      data: { type: 'notification-preference', enabled: true },
    });
    await restarted.dispatch('push', pushEvent(completionPayload()));
    expect(restarted.showNotification).toHaveBeenCalledOnce();
  });

  it('never evicts live exact reads when saturated, instead failing closed until the safe horizon', async () => {
    const storage: MemoryCaches = new Map();
    const harness = workerHarness([], 'v7', storage);
    const now = Date.now();
    vi.spyOn(Date, 'now').mockReturnValue(now);
    for (let i = 0; i < 513; i++)
      await harness.dispatch('message', {
        source: harness.client,
        data: {
          type: 'notification-read',
          version: 2,
          store_instance_id: 'store',
          source: '/ui',
          session_id: `session-${i}`,
          attention_seq: 3,
        },
      });
    const restarted = workerHarness([], 'v8', storage);
    for (const session_id of ['session-0', 'session-512', 'another-session'])
      await restarted.dispatch('push', pushEvent(completionPayload({ session_id })));
    expect(restarted.showNotification).not.toHaveBeenCalled();
    const cache = [...storage.entries()].find(([key]) =>
      key.startsWith('term-llm-notifications-'),
    )![1];
    const state = await [...cache.values()][0].clone().json();
    expect(state.reads).toHaveLength(512);
    expect(state.reads[0].session_id).toBe('session-0');
    expect(state.read_block_until).toBe(now + 24 * 60 * 60 * 1000);
    vi.spyOn(Date, 'now').mockReturnValue(now + 24 * 60 * 60 * 1000 + 1);
    await restarted.dispatch('push', pushEvent(completionPayload()));
    expect(restarted.showNotification).toHaveBeenCalledOnce();
  });

  it('keeps the durable replay ledger bounded and expires old records', async () => {
    const harness = workerHarness();
    for (let i = 0; i < 515; i++)
      await harness.dispatch('push', pushEvent(manualPayload({ event_id: `event-${i}` })));
    const cache = [...harness.storage.entries()].find(([key]) =>
      key.startsWith('term-llm-notifications-'),
    )![1];
    let state = await [...cache.values()][0].clone().json();
    expect(state.events).toHaveLength(512);
    expect(harness.showNotification).toHaveBeenCalledTimes(512);
    await harness.dispatch('push', pushEvent(manualPayload({ event_id: 'event-0' })));
    expect(harness.showNotification).toHaveBeenCalledTimes(512);
    vi.spyOn(Date, 'now').mockReturnValue(Date.now() + 24 * 60 * 60 * 1000 + 1);
    await harness.dispatch('push', pushEvent(manualPayload({ event_id: 'after-horizon' })));
    state = await [...cache.values()][0].clone().json();
    expect(state.events).toHaveLength(1);
  });
});

describe('service worker asset caching', () => {
  const first = './dist/app-AAAAAAAA.js';
  const second = './dist/app-BBBBBBBB.js';
  const third = './dist/app-CCCCCCCC.js';

  it('serves hashed assets cache-first including query variants, but mutable scripts network-first', async () => {
    const harness = workerHarness([first]);
    expect(await (await harness.request(first))?.text()).toBe('network');
    harness.network.mockImplementation(async () => new Response('updated'));
    expect(await (await harness.request(first + '?v=old'))?.text()).toBe('network');
    expect(harness.network).toHaveBeenCalledTimes(1);
    expect(await (await harness.request('./mutable.js'))?.text()).toBe('updated');
    harness.network.mockImplementation(async () => new Response('newer'));
    expect(await (await harness.request('./mutable.js'))?.text()).toBe('newer');
    expect(harness.network).toHaveBeenCalledTimes(3);
  });

  it('bypasses navigations, APIs, extensions, Hub and other origins/scopes', async () => {
    const harness = workerHarness([first]);
    for (const path of [
      './api/sessions',
      './v1/models',
      './extensions/view.js',
      './dist/hub.js',
      'https://other.test/a.js',
      '/outside.js',
    ]) {
      expect(await harness.request(path)).toBeUndefined();
    }
    expect(await harness.request(first, 'script', 'navigate')).toBeUndefined();
    expect(harness.network).not.toHaveBeenCalled();
  });

  it('does not store a 404 or an authentication redirect under a hashed URL', async () => {
    const harness = workerHarness([first]);
    harness.network.mockImplementation(async () => new Response('missing', { status: 404 }));
    expect((await harness.request(first))?.status).toBe(404);
    const redirect = new Response('login');
    Object.defineProperty(redirect, 'redirected', { value: true });
    harness.network.mockImplementation(async () => redirect);
    await harness.request(first);
    harness.network.mockImplementation(async () => new Response('real asset'));
    expect(await (await harness.request(first))?.text()).toBe('real asset');
    expect(harness.network).toHaveBeenCalledTimes(3);
  });

  it('falls back to HTTP when CacheStorage is unavailable', async () => {
    const harness = workerHarness([first]);
    vi.spyOn(harness.cacheStorage, 'open').mockRejectedValue(new Error('storage denied'));
    expect(await (await harness.request(first))?.text()).toBe('network');
  });

  it('prunes to the current and previous deployment, preserves previous on repeat activation', async () => {
    const storage: MemoryCaches = new Map();
    const one = workerHarness([first], 'one', storage);
    await one.cacheStorage.open('term-llm-shell-obsolete');
    await one.dispatch('activate', {});
    await one.request(first);
    const two = workerHarness([second], 'two', storage);
    await two.dispatch('activate', {});
    expect(await (await two.request(first))?.text()).toBe('network');
    expect(two.network).not.toHaveBeenCalled();
    await two.request(second);
    await two.dispatch('activate', {});
    await two.request(first);
    expect(two.network).toHaveBeenCalledTimes(1);
    // The browser can stop/restart a worker without running activate again.
    const restarted = workerHarness([second], 'two', storage);
    expect(await (await restarted.request(first))?.text()).toBe('network');
    expect(restarted.network).not.toHaveBeenCalled();
    const three = workerHarness([third], 'three', storage);
    await three.dispatch('activate', {});
    const cachedURLs = [...storage.values()].flatMap((entries) => [...entries.keys()]);
    expect(cachedURLs).not.toContain(new URL(first, 'https://example.test/ui/').href);
    expect(cachedURLs).toContain(new URL(second, 'https://example.test/ui/').href);
    await three.request(second);
    expect(three.network).not.toHaveBeenCalled();
    expect([...storage.keys()].filter((name) => name.startsWith('term-llm-shell-'))).toEqual([]);
    await three.request(first);
    expect(three.network).toHaveBeenCalledTimes(1);
    expect(
      [...storage.values()].some((entries) =>
        entries.has(new URL(first, 'https://example.test/ui/').href),
      ),
    ).toBe(false); // Retired hashes never re-enter either cache.
  });
});
