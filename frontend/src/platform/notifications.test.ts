import { afterEach, describe, expect, it, vi } from 'vitest';
import type { AppConfig } from '../app/config';
import { APIError } from '../api/client';
import type { NativePushSubscription, TermLLMNativePush } from './push-platform';
import { NotificationController, base64URLToBytes } from './notifications';

const config: AppConfig = {
  prefix: '/ui',
  version: 'test',
  sidebarCategories: ['all'],
  agentName: '',
  agentNames: [],
  title: '',
  locationSharing: true,
  worktrees: true,
  hub: null,
  vapidKey: 'AQID',
  pushSupported: true,
};

const propertyRestorers: Array<() => void> = [];

function setProperty(target: object, key: string, value: unknown): void {
  const descriptor = Object.getOwnPropertyDescriptor(target, key);
  Object.defineProperty(target, key, { configurable: true, value });
  propertyRestorers.push(() => {
    if (descriptor) Object.defineProperty(target, key, descriptor);
    else Reflect.deleteProperty(target, key);
  });
}

afterEach(() => {
  vi.restoreAllMocks();
  propertyRestorers
    .splice(0)
    .reverse()
    .forEach((restore) => restore());
});

describe('notification enrollment controller', () => {
  it('decodes browser enrollment keys', () => {
    expect([...base64URLToBytes('AQID')]).toEqual([1, 2, 3]);
  });

  it('reports insecure contexts as unsupported without trusting cached enrollment', async () => {
    setProperty(globalThis, 'isSecureContext', false);
    localStorage.setItem('push-id', 'cached-subscription');
    const controller = new NotificationController(config, {} as never, localStorage, 'push-id');
    const state = await controller.reconcile();
    expect(state).toMatchObject({ status: 'unsupported', verified: false });
    expect(state.detail).toContain('HTTPS');
  });

  it('reconciles browser permission, subscription key, and canonical server acknowledgement', async () => {
    setProperty(globalThis, 'isSecureContext', true);
    setProperty(window, 'PushManager', class {});
    const notification = { permission: 'granted', requestPermission: vi.fn() };
    setProperty(window, 'Notification', notification);
    setProperty(globalThis, 'Notification', notification);
    const subscription = {
      endpoint: 'https://push.example/sub',
      options: { applicationServerKey: base64URLToBytes(config.vapidKey).buffer },
      toJSON: () => ({ endpoint: 'https://push.example/sub', keys: { p256dh: 'p', auth: 'a' } }),
      unsubscribe: vi.fn(async () => true),
    } as unknown as PushSubscription;
    const registration = {
      pushManager: {
        getSubscription: vi.fn(async () => subscription),
        subscribe: vi.fn(),
      },
    } as unknown as ServiceWorkerRegistration;
    setProperty(navigator, 'serviceWorker', { register: vi.fn(async () => registration) });
    const endpoints = {
      pushSubscribe: vi.fn(async () => ({
        id: 'canonical-sub',
        state: 'active',
        vapid_key_id: 'key',
      })),
      pushUnsubscribe: vi.fn(),
    };
    const controller = new NotificationController(
      config,
      endpoints as never,
      localStorage,
      'push-id',
    );
    const state = await controller.reconcile();
    expect(state).toMatchObject({
      status: 'subscribed',
      verified: true,
      subscriptionId: 'canonical-sub',
    });
    expect(localStorage.getItem('push-id')).toBe('canonical-sub');
    expect(registration.pushManager.subscribe).not.toHaveBeenCalled();
    expect(navigator.serviceWorker.register).toHaveBeenCalledWith('/ui/sw.js?v=test', {
      scope: '/ui/',
    });
    expect(endpoints.pushSubscribe).toHaveBeenCalledWith({
      ...subscription.toJSON(),
      client_kind: 'browser',
    });
    expect(notification.requestPermission).not.toHaveBeenCalled();
  });
});

const nativeSubscription: NativePushSubscription = {
  endpoint: 'https://relay.example/device/node',
  keys: { p256dh: 'native-p256dh', auth: 'native-auth' },
  applicationServerKey: config.vapidKey,
};

function nativeFixture(
  permission: NotificationPermission = 'granted',
  subscription: NativePushSubscription | null = null,
  overrides: Partial<AppConfig> = {},
) {
  const native = { permission, subscription };
  const bridge = {
    version: 1,
    permission: vi.fn(async () => native.permission),
    requestPermission: vi.fn(
      async (): Promise<NotificationPermission> => (native.permission = 'granted'),
    ),
    getSubscription: vi.fn(async () => native.subscription),
    subscribe: vi.fn(async (_scope: string, applicationServerKey: string) => {
      native.subscription = { ...nativeSubscription, applicationServerKey };
      return native.subscription;
    }),
    unsubscribe: vi.fn(async () => {
      native.subscription = null;
      return true;
    }),
    disable: vi.fn(async () => {
      native.permission = 'default';
      native.subscription = null;
      return true;
    }),
  } satisfies TermLLMNativePush;
  const endpoints = {
    pushSubscribe: vi.fn(async () => ({ id: 'native-sub', state: 'active', vapid_key_id: 'key' })),
    pushUnsubscribe: vi.fn(async () => ({})),
  };
  // WKWebView has neither browser push APIs nor service workers. Native mode
  // must not inspect them (or require the browser's secure/standalone flags).
  setProperty(window, '__termLLMNativePush', Object.freeze(bridge));
  setProperty(globalThis, 'isSecureContext', false);
  setProperty(window, 'Notification', undefined);
  setProperty(globalThis, 'Notification', undefined);
  setProperty(window, 'PushManager', undefined);
  setProperty(navigator, 'serviceWorker', undefined);
  const controller = new NotificationController(
    { ...config, ...overrides },
    endpoints as never,
    localStorage,
    'native-push-id',
  );
  return { controller, native, bridge, endpoints };
}

describe('native notification enrollment', () => {
  it.each(['', '/ui', '/hub/nodes/artist/ui'])(
    'auto-subscribes granted permission with the exact node scope %j',
    async (prefix) => {
      const { controller, bridge, endpoints } = nativeFixture('granted', null, { prefix });
      expect(await controller.reconcile()).toMatchObject({
        status: 'subscribed',
        verified: true,
        busy: false,
        subscriptionId: 'native-sub',
      });
      expect(bridge.permission).toHaveBeenCalledWith(prefix);
      expect(bridge.getSubscription).toHaveBeenCalledWith(prefix);
      expect(bridge.subscribe).toHaveBeenCalledWith(prefix, config.vapidKey);
      expect(bridge.requestPermission).not.toHaveBeenCalled();
      expect(endpoints.pushSubscribe).toHaveBeenCalledWith({
        endpoint: nativeSubscription.endpoint,
        keys: nativeSubscription.keys,
        client_kind: 'app',
      });
      expect(localStorage.getItem('native-push-id')).toBe('native-sub');
      await controller.reconcile();
      expect(bridge.subscribe).toHaveBeenCalledTimes(1);
    },
  );

  it.each([nativeSubscription, null])(
    'forgets default-permission records without opting out (subscription=%j)',
    async (subscription) => {
      const { controller, bridge, endpoints } = nativeFixture('default', subscription);
      localStorage.setItem('native-push-id', 'cached-sub');
      expect(await controller.reconcile()).toMatchObject({
        status: 'unsubscribed',
        verified: false,
        busy: false,
      });
      expect(bridge.unsubscribe).toHaveBeenCalledWith(config.prefix);
      expect(bridge.disable).not.toHaveBeenCalled();
      expect(bridge.subscribe).not.toHaveBeenCalled();
      expect(endpoints.pushUnsubscribe).toHaveBeenCalledWith({ id: 'cached-sub' });
      expect(localStorage.getItem('native-push-id')).toBeNull();
    },
  );

  it.each([true, false])(
    'disables only this node and deletes by cached id or endpoint (cached=%s)',
    async (cached) => {
      const { controller, bridge, endpoints } = nativeFixture('granted', nativeSubscription);
      if (cached) localStorage.setItem('native-push-id', 'cached-sub');
      expect(await controller.disable()).toMatchObject({
        status: 'unsubscribed',
        detail: 'Notifications are off for this site/agent.',
        busy: false,
      });
      expect(bridge.disable).toHaveBeenCalledWith(config.prefix);
      expect(bridge.unsubscribe).not.toHaveBeenCalled();
      expect(endpoints.pushUnsubscribe).toHaveBeenCalledWith(
        cached ? { id: 'cached-sub' } : { endpoint: nativeSubscription.endpoint },
      );
      expect(localStorage.getItem('native-push-id')).toBeNull();
      await controller.reconcile();
      expect(bridge.subscribe).not.toHaveBeenCalled();
    },
  );

  it('retires a missing native enrollment before creating its replacement', async () => {
    const { controller, bridge, endpoints } = nativeFixture('granted', null);
    localStorage.setItem('native-push-id', 'old-sub');
    endpoints.pushUnsubscribe.mockImplementationOnce(async () => {
      expect(localStorage.getItem('native-push-id')).toBe('old-sub');
      expect(bridge.subscribe).not.toHaveBeenCalled();
      return {};
    });

    expect(await controller.reconcile()).toMatchObject({
      status: 'subscribed',
      verified: true,
      subscriptionId: 'native-sub',
    });
    expect(endpoints.pushUnsubscribe).toHaveBeenCalledWith({ id: 'old-sub' });
    expect(endpoints.pushUnsubscribe.mock.invocationCallOrder[0]).toBeLessThan(
      bridge.subscribe.mock.invocationCallOrder[0],
    );
    expect(endpoints.pushUnsubscribe.mock.invocationCallOrder[0]).toBeLessThan(
      endpoints.pushSubscribe.mock.invocationCallOrder[0],
    );
    expect(localStorage.getItem('native-push-id')).toBe('native-sub');
  });

  it('retains the old native enrollment id when retirement fails and retries it', async () => {
    const { controller, bridge, endpoints } = nativeFixture('granted', null);
    localStorage.setItem('native-push-id', 'old-sub');
    endpoints.pushUnsubscribe.mockRejectedValueOnce(new APIError('Unavailable', 503));

    expect(await controller.reconcile()).toMatchObject({
      status: 'stale',
      verified: false,
      busy: false,
    });
    expect(localStorage.getItem('native-push-id')).toBe('old-sub');
    expect(bridge.subscribe).not.toHaveBeenCalled();
    expect(endpoints.pushSubscribe).not.toHaveBeenCalled();

    expect(await controller.reconcile()).toMatchObject({ status: 'subscribed', verified: true });
    expect(endpoints.pushUnsubscribe).toHaveBeenNthCalledWith(1, { id: 'old-sub' });
    expect(endpoints.pushUnsubscribe).toHaveBeenNthCalledWith(2, { id: 'old-sub' });
    expect(endpoints.pushUnsubscribe).toHaveBeenCalledTimes(2);
    expect(localStorage.getItem('native-push-id')).toBe('native-sub');
  });

  it('leaves a missing native enrollment untouched when repair is disabled', async () => {
    const { controller, bridge, endpoints } = nativeFixture('granted', null);
    localStorage.setItem('native-push-id', 'old-sub');

    expect(await controller.reconcile({ repair: false })).toMatchObject({ status: 'unsubscribed' });
    expect(endpoints.pushUnsubscribe).not.toHaveBeenCalled();
    expect(bridge.subscribe).not.toHaveBeenCalled();
    expect(localStorage.getItem('native-push-id')).toBe('old-sub');

    expect(await controller.reconcile()).toMatchObject({ status: 'subscribed' });
    expect(endpoints.pushUnsubscribe).toHaveBeenCalledWith({ id: 'old-sub' });
  });

  it.each([401, 503])(
    'does not revive a retired native id when replacement synchronization fails with HTTP %s',
    async (status) => {
      const { controller, endpoints } = nativeFixture('granted', null);
      localStorage.setItem('native-push-id', 'old-sub');
      endpoints.pushSubscribe.mockRejectedValueOnce(new APIError('Unavailable', status));

      const state = await controller.reconcile();
      expect(state.subscriptionId).toBeUndefined();
      expect(state.verified).toBe(false);
      expect(localStorage.getItem('native-push-id')).toBeNull();
      expect(endpoints.pushUnsubscribe).toHaveBeenCalledOnce();

      expect(await controller.reconcile()).toMatchObject({
        status: 'subscribed',
        subscriptionId: 'native-sub',
      });
      expect(endpoints.pushUnsubscribe).toHaveBeenCalledOnce();
    },
  );

  it('repairs VAPID rotation without opting the node out', async () => {
    const old = { ...nativeSubscription, applicationServerKey: 'BAUG' };
    const { controller, bridge, endpoints } = nativeFixture('granted', old);
    localStorage.setItem('native-push-id', 'old-sub');
    expect(await controller.reconcile()).toMatchObject({ status: 'subscribed', verified: true });
    expect(bridge.unsubscribe).toHaveBeenCalledWith(config.prefix);
    expect(bridge.disable).not.toHaveBeenCalled();
    expect(endpoints.pushUnsubscribe).toHaveBeenCalledWith({ id: 'old-sub' });
    expect(bridge.subscribe).toHaveBeenCalledWith(config.prefix, config.vapidKey);
    expect(localStorage.getItem('native-push-id')).toBe('native-sub');
  });

  it('reports rotation as stale when repair is disabled, and repairs on retry', async () => {
    const { controller, bridge } = nativeFixture('granted', {
      ...nativeSubscription,
      applicationServerKey: 'BAUG',
    });
    expect(await controller.reconcile({ repair: false })).toMatchObject({ status: 'stale' });
    expect(bridge.unsubscribe).not.toHaveBeenCalled();
    expect(await controller.reconcile()).toMatchObject({ status: 'subscribed' });
  });

  it('uses native requestPermission to enable and clear the opt-out', async () => {
    const { controller, bridge } = nativeFixture('default');
    expect(await controller.enable()).toMatchObject({ status: 'subscribed' });
    expect(bridge.requestPermission).toHaveBeenCalledWith(config.prefix);
    expect(bridge.subscribe).toHaveBeenCalledWith(config.prefix, config.vapidKey);
  });

  it('reports native permission denial with iOS Settings guidance', async () => {
    const { controller, bridge } = nativeFixture('denied');
    expect(await controller.reconcile()).toMatchObject({
      status: 'blocked',
      detail: 'Notifications are blocked for term-llm in iOS Settings.',
      busy: false,
    });
    expect(bridge.subscribe).not.toHaveBeenCalled();
    bridge.requestPermission.mockResolvedValueOnce('denied');
    expect(await controller.enable()).toMatchObject({ status: 'blocked' });
    expect(bridge.requestPermission).toHaveBeenCalledWith(config.prefix);
  });

  it.each([{ vapidKey: '' }, { pushSupported: false }])(
    'keeps server capability checks: %j',
    async (overrides) => {
      const { controller, bridge } = nativeFixture('granted', null, overrides);
      expect(await controller.reconcile()).toMatchObject({ status: 'unsupported' });
      expect(bridge.permission).not.toHaveBeenCalled();
      expect(bridge.subscribe).not.toHaveBeenCalled();
    },
  );

  it('old native bridges preserve enrollment but fail closed for automatic ownership', async () => {
    const { controller } = nativeFixture('granted', nativeSubscription, { notificationVersion: 2 });
    await controller.reconcile();
    expect(await controller.originHeaders()).toEqual({});
    expect('signalCompletion' in controller).toBe(false);
  });

  it('reconciles native changes and common lifecycle events, never browser push listeners', async () => {
    const { controller, bridge, native } = nativeFixture();
    const workerListener = vi.fn();
    const query = vi.fn();
    setProperty(navigator, 'serviceWorker', { addEventListener: workerListener });
    setProperty(navigator, 'permissions', { query });
    const dispose = controller.installLifecycle();
    await controller.reconcile();
    expect(controller.state.status).toBe('subscribed');
    expect(workerListener).not.toHaveBeenCalled();
    expect(query).not.toHaveBeenCalled();
    native.permission = 'default';
    window.dispatchEvent(new Event('termllm-native-push-change'));
    await controller.reconcile();
    expect(controller.state.status).toBe('unsubscribed');
    native.permission = 'granted';
    for (const event of ['pageshow', 'online', 'focus']) {
      window.dispatchEvent(new Event(event));
      await controller.reconcile();
      expect(controller.state.status).toBe('subscribed');
    }
    dispose();
    bridge.permission.mockClear();
    for (const event of ['termllm-native-push-change', 'pageshow', 'online', 'focus'])
      window.dispatchEvent(new Event(event));
    expect(bridge.permission).not.toHaveBeenCalled();
  });

  it.each(['permission', 'getSubscription', 'subscribe'] as const)(
    'handles rejected native %s calls as a recoverable error',
    async (method) => {
      const { controller, bridge } = nativeFixture();
      bridge[method].mockRejectedValueOnce(new Error('Native push unavailable'));
      expect(await controller.reconcile()).toMatchObject({
        status: 'stale',
        verified: false,
        busy: false,
        detail: 'Native push unavailable',
      });
      expect(await controller.reconcile()).toMatchObject({ status: 'subscribed' });
    },
  );

  it.each(['enable', 'disable'] as const)('handles rejected native %s actions', async (action) => {
    const { controller, bridge } = nativeFixture();
    bridge[action === 'enable' ? 'requestPermission' : 'disable'].mockRejectedValueOnce(
      new Error('Please try again'),
    );
    expect(await controller[action]()).toMatchObject({
      status: 'stale',
      busy: false,
      detail: 'Please try again',
    });
  });

  it('retains cached enrollment on server verification failure, using the shared reconcile path', async () => {
    const { controller, endpoints } = nativeFixture('granted', nativeSubscription);
    localStorage.setItem('native-push-id', 'cached-sub');
    endpoints.pushSubscribe.mockRejectedValueOnce(new APIError('Unavailable', 503));
    expect(await controller.reconcile()).toMatchObject({
      status: 'subscribed',
      verified: false,
      subscriptionId: 'cached-sub',
      busy: false,
    });
    expect(localStorage.getItem('native-push-id')).toBe('cached-sub');
  });

  it('does not trust an unknown bridge version', async () => {
    const { controller } = nativeFixture();
    const bridge = window.__termLLMNativePush!;
    setProperty(window, '__termLLMNativePush', { ...bridge, version: 2 });
    const browser = new NotificationController(
      config,
      {} as never,
      localStorage,
      'browser-push-id',
    );
    expect(await browser.reconcile()).toMatchObject({ status: 'unsupported' });
    expect(browser.state.detail).toContain('HTTPS');
    expect(controller.state.status).toBe('unsupported');
  });
});

function nativeV2Fixture() {
  const base = nativeFixture('granted', nativeSubscription, { notificationVersion: 2 });
  const bridge = {
    ...base.bridge,
    notificationCapabilities: vi.fn(async () => ({
      version: 2 as const,
      window_id: 'scene',
      document_id: 'native-document',
    })),
    notificationPresence: vi.fn(async (_body: unknown) => {}),
    notificationRead: vi.fn(async (_body: unknown) => {}),
    notificationEnrollment: vi.fn(async (_body: unknown) => {}),
    notificationPendingCleanup: vi.fn(
      async (_body: unknown): Promise<Array<{ subscription_id: string }>> => [],
    ),
    notificationCleanupAcknowledged: vi.fn(async (_body: unknown) => {}),
  } satisfies TermLLMNativePush;
  setProperty(window, '__termLLMNativePush', bridge);
  const endpoints = {
    ...base.endpoints,
    pushPresence: vi.fn(async () => ({ ok: true, version: 2 })),
  };
  const controller = new NotificationController(
    { ...config, notificationVersion: 2 },
    endpoints as never,
    localStorage,
    'v2-push-id',
  );
  return { ...base, controller, bridge, endpoints };
}

function browserFixture() {
  setProperty(window, '__termLLMNativePush', undefined);
  setProperty(globalThis, 'isSecureContext', true);
  setProperty(window, 'PushManager', class {});
  const notification = { permission: 'granted', requestPermission: vi.fn() };
  setProperty(window, 'Notification', notification);
  setProperty(globalThis, 'Notification', notification);
  const subscription = {
    endpoint: 'https://push.example/sub',
    options: { applicationServerKey: base64URLToBytes(config.vapidKey).buffer },
    toJSON: () => ({ endpoint: 'https://push.example/sub', keys: { p256dh: 'p', auth: 'a' } }),
    unsubscribe: vi.fn(async () => true),
  };
  const registration = {
    active: { postMessage: vi.fn() },
    pushManager: {
      getSubscription: vi.fn(async () => subscription),
      subscribe: vi.fn(async () => subscription),
    },
  };
  setProperty(navigator, 'serviceWorker', {
    register: vi.fn(async () => registration),
    getRegistration: vi.fn(async () => registration),
  });
  const endpoints = {
    pushSubscribe: vi.fn(async () => ({ id: 'browser-sub', state: 'active', vapid_key_id: 'key' })),
    pushUnsubscribe: vi.fn(async (_body: unknown) => ({})),
  };
  const create = () =>
    new NotificationController(config, endpoints as never, localStorage, 'browser-id');
  return { controller: create(), create, subscription, registration, endpoints };
}

describe('durable opt-out and exact native coordination', () => {
  it('preserves browser off through reload/reconcile and retires failed unsubscribe IDs on reconnect', async () => {
    const { controller, create, endpoints, registration } = browserFixture();
    await controller.reconcile();
    endpoints.pushUnsubscribe.mockRejectedValueOnce(new APIError('offline', 503));
    expect(await controller.disable()).toMatchObject({ status: 'unsubscribed' });
    expect(localStorage.getItem('browser-id')).toBeNull();
    expect(
      Array.from({ length: localStorage.length }, (_, i) => localStorage.key(i)).some((key) =>
        key?.includes(':retire:'),
      ),
    ).toBe(true);
    const reloaded = create();
    await reloaded.reconcile();
    expect(endpoints.pushUnsubscribe).toHaveBeenCalledWith({ id: 'browser-sub' });
    expect(endpoints.pushSubscribe).toHaveBeenCalledOnce();
    expect(reloaded.state.status).toBe('unsubscribed');
    expect(registration.active.postMessage).toHaveBeenCalledWith({
      type: 'notification-preference',
      enabled: false,
    });
    expect(await reloaded.enable()).toMatchObject({ status: 'subscribed' });
    expect(endpoints.pushSubscribe).toHaveBeenCalledTimes(2);
  });

  it('fences a late subscribe ACK racing browser disable and persists the late ID until DELETE succeeds', async () => {
    const { controller, create, endpoints, subscription } = browserFixture();
    let accept!: (ack: { id: string; state: string; vapid_key_id: string }) => void;
    endpoints.pushSubscribe.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          accept = resolve;
        }),
    );
    const reconciling = controller.reconcile();
    await vi.waitFor(() => expect(endpoints.pushSubscribe).toHaveBeenCalledOnce());
    await controller.disable();
    endpoints.pushUnsubscribe.mockRejectedValueOnce(new APIError('offline', 503));
    accept({ id: 'late-id', state: 'active', vapid_key_id: 'key' });
    expect(await reconciling).toMatchObject({ status: 'unsubscribed', verified: false });
    expect(localStorage.getItem('browser-id')).toBeNull();
    expect(subscription.unsubscribe).toHaveBeenCalled();
    const reloaded = create();
    await reloaded.reconcile();
    expect(endpoints.pushUnsubscribe).toHaveBeenLastCalledWith({ id: 'late-id' });
    expect(endpoints.pushSubscribe).toHaveBeenCalledOnce();
  });

  it('never repairs while an authenticated cleanup tombstone still fails', async () => {
    const { controller, endpoints } = browserFixture();
    await controller.reconcile();
    endpoints.pushUnsubscribe.mockRejectedValue(new APIError('sign in', 401));
    await controller.disable();
    expect(await controller.enable()).toMatchObject({ status: 'stale', verified: false });
    expect(endpoints.pushSubscribe).toHaveBeenCalledOnce();
    endpoints.pushUnsubscribe.mockResolvedValue({});
    expect(await controller.reconcile()).toMatchObject({ status: 'subscribed' });
  });

  it('uses native document, authenticated presence and source headers only after successful binding', async () => {
    const { controller, endpoints, bridge } = nativeV2Fixture();
    await controller.reconcile();
    expect(await controller.originHeaders()).toEqual({
      'X-Term-LLM-Notification-Version': '2',
      'X-Term-LLM-Window-ID': 'scene',
      'X-Term-LLM-Document-ID': 'native-document',
      'X-Term-LLM-Push-Subscription-ID': 'native-sub',
      'X-Term-LLM-Notification-Source': '/ui',
    });
    expect(bridge.notificationEnrollment).toHaveBeenCalledWith({
      source: '/ui',
      subscription_id: 'native-sub',
    });
    expect(endpoints.pushPresence).toHaveBeenCalledWith(
      expect.objectContaining({ source: '/ui', document_id: 'native-document' }),
    );
    endpoints.pushPresence.mockRejectedValueOnce(new APIError('binding expired', 409));
    expect(await controller.originHeaders()).toEqual({});
    controller.dispose();
  });

  it('retries a failed native local disable on reconcile instead of treating server cleanup alone as opt-out', async () => {
    const { controller, bridge } = nativeV2Fixture();
    await controller.reconcile();
    bridge.disable.mockRejectedValueOnce(new Error('Native storage busy'));
    expect(await controller.disable()).toMatchObject({ status: 'stale', verified: false });
    expect(await controller.reconcile()).toMatchObject({ status: 'unsubscribed', verified: false });
    expect(bridge.disable).toHaveBeenCalledTimes(2);
    expect(await controller.originHeaders()).toEqual({});
    controller.dispose();
  });

  it('merges native pending cleanup and ACKs native only after authenticated DELETE success', async () => {
    const { controller, bridge, endpoints } = nativeV2Fixture();
    bridge.notificationPendingCleanup.mockResolvedValue([
      { subscription_id: 'other-loaded-node-id' },
    ]);
    endpoints.pushUnsubscribe.mockRejectedValueOnce(new APIError('unauthorized', 401));
    expect(await controller.reconcile()).toMatchObject({ status: 'stale' });
    expect(bridge.notificationCleanupAcknowledged).not.toHaveBeenCalled();
    expect(endpoints.pushSubscribe).not.toHaveBeenCalled();
    await controller.reconcile();
    expect(bridge.notificationPendingCleanup).toHaveBeenCalledWith({ source: '/ui' });
    expect(bridge.notificationCleanupAcknowledged).toHaveBeenCalledWith({
      source: '/ui',
      subscription_id: 'other-loaded-node-id',
    });
    expect(endpoints.pushUnsubscribe.mock.invocationCallOrder[1]).toBeLessThan(
      bridge.notificationCleanupAcknowledged.mock.invocationCallOrder[0],
    );
    controller.dispose();
  });

  it('forwards exact authoritative ACK fields, never missing or guessed read sequences', async () => {
    const { controller, bridge } = nativeV2Fixture();
    controller.acknowledgeRead('session', { store_instance_id: 'store', latest_attention_seq: 99 });
    controller.acknowledgeRead('session', { store_instance_id: 'store', seen_through_seq: -1 });
    expect(bridge.notificationRead).not.toHaveBeenCalled();
    controller.acknowledgeRead('session', {
      store_instance_id: 'store',
      seen_through_seq: 3,
      latest_attention_seq: 99,
    });
    await vi.waitFor(() =>
      expect(bridge.notificationRead).toHaveBeenCalledWith({
        version: 2,
        source: '/ui',
        store_instance_id: 'store',
        session_id: 'session',
        attention_seq: 3,
      }),
    );
    controller.acknowledgeRead('session', {
      attention_store_instance_id: 'store',
      seen_through_seq: 4,
    });
    await vi.waitFor(() =>
      expect(bridge.notificationRead).toHaveBeenLastCalledWith(
        expect.objectContaining({ attention_seq: 4 }),
      ),
    );
  });
});
