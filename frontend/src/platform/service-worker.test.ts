import { readFileSync } from 'node:fs';
import { afterEach, describe, expect, it, vi } from 'vitest';

const source = readFileSync(
  new URL('../internal/serveui/static/sw.js', `file://${process.cwd()}/`),
  'utf8',
);

type WorkerListener = (event: {
  data?: unknown;
  notification?: { data?: { url?: string }; close(): void };
  waitUntil(value: Promise<unknown>): void;
}) => void;

function workerHarness() {
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
  // The script only touches caches from install/activate/fetch callbacks, which
  // are not invoked by this notification-focused harness.
  new Function(source)();
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
  return { dispatch, showNotification, outstanding, client };
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
