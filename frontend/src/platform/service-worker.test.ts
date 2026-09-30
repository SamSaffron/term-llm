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

function workerHarness(assets: string[] = [], version = 'v7', storage: MemoryCaches = new Map()) {
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
  const outstanding = new Map<string, { close(): void }>();
  const showNotification = vi.fn(async (_title: string, options: { tag: string }) => {
    outstanding.set(options.tag, { close: () => outstanding.delete(options.tag) });
  });
  const client = {
    url: 'https://example.test/ui/chat/one',
    visibilityState: 'visible',
    focus: vi.fn(async () => undefined),
    postMessage: vi.fn(),
  };
  const worker = {
    location: { origin: 'https://example.test' },
    registration: {
      scope: 'https://example.test/ui/',
      showNotification,
      getNotifications: vi.fn(async ({ tag }: { tag: string }) =>
        outstanding.has(tag) ? [outstanding.get(tag)] : [],
      ),
    },
    clients: {
      claim: vi.fn(),
      matchAll: vi.fn(async () => [client]),
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
    outstanding,
    client,
    request,
    network,
    storage,
    cacheStorage,
  };
}

afterEach(() => vi.restoreAllMocks());

describe('service worker completion notifications', () => {
  it('shows every push while replacing one stable outstanding tag', async () => {
    const harness = workerHarness();
    const payload = {
      version: 1,
      event_id: 'completion:resp:sub',
      response_id: 'resp',
      title: 'Response complete',
      body: 'Ready',
      url: '/ui/chat/session',
    };
    const event = { data: { json: () => payload } };
    await harness.dispatch('push', event);
    await harness.dispatch('push', event);
    expect(harness.showNotification).toHaveBeenCalledTimes(2);
    expect(harness.showNotification).toHaveBeenLastCalledWith(
      'Response complete',
      expect.objectContaining({
        tag: 'term-llm-completion:completion:resp:sub',
        renotify: false,
      }),
    );
    expect(harness.outstanding.size).toBe(1);
    expect(harness.client.postMessage).toHaveBeenCalledWith({
      type: 'completion-push-shown',
      tag: 'term-llm-completion:completion:resp:sub',
    });
  });

  it('acknowledges local completion handling after deduplicating the stable tag', async () => {
    const harness = workerHarness();
    const reply = { postMessage: vi.fn() };
    const event = {
      data: {
        type: 'completion-notification',
        payload: {
          version: 1,
          event_id: 'completion:resp:sub',
          response_id: 'resp',
          title: 'Response complete',
          body: 'Ready',
          url: '/ui/chat/session',
        },
      },
      ports: [reply],
    };
    await harness.dispatch('message', event);
    await harness.dispatch('message', event);
    expect(harness.showNotification).toHaveBeenCalledOnce();
    expect(reply.postMessage).toHaveBeenCalledTimes(2);
    expect(reply.postMessage).toHaveBeenLastCalledWith({
      type: 'completion-notification-handled',
      tag: 'term-llm-completion:completion:resp:sub',
    });
    expect(harness.outstanding.size).toBe(1);
  });

  it('still displays a safe notification for malformed push data', async () => {
    const harness = workerHarness();
    await harness.dispatch('push', {
      data: {
        json: () => {
          throw new Error('bad json');
        },
        text: () => 'not-json',
      },
    });
    expect(harness.showNotification).toHaveBeenCalledOnce();
    expect(harness.showNotification).toHaveBeenCalledWith(
      'term-llm notification',
      expect.objectContaining({ tag: 'term-llm-completion:malformed-push' }),
    );
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
