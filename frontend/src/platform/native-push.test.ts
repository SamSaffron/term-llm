import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { APIError } from '../api/client';
import type { AppConfig } from '../app/config';
import {
  nativePushBridge,
  type NativePushBridge,
  type NativePushSubscription,
} from './native-push';
import { NotificationController } from './notifications';

const config = {
  prefix: '/nodes/one',
  vapidKey: 'AQID',
  pushSupported: true,
} as AppConfig;

function fakeHost(permission: NotificationPermission = 'granted') {
  let currentPermission = permission;
  let subscription: NativePushSubscription | null = null;
  let endpoint = 'https://push.example/old';
  const bridge = {
    version: 1,
    permission: vi.fn(async () => currentPermission),
    requestPermission: vi.fn(async () => {
      if (currentPermission !== 'denied') currentPermission = 'granted';
      return currentPermission;
    }),
    getSubscription: vi.fn(async () => (currentPermission === 'granted' ? subscription : null)),
    subscribe: vi.fn(async (_scope: string, applicationServerKey: string) => {
      if (currentPermission !== 'granted') throw new Error('Not allowed');
      subscription = { endpoint, keys: { p256dh: 'public', auth: 'secret' }, applicationServerKey };
      return subscription;
    }),
    unsubscribe: vi.fn(async () => {
      subscription = null;
      return true;
    }),
    disable: vi.fn(async () => {
      currentPermission = 'default';
      subscription = null;
      return true;
    }),
  } satisfies NativePushBridge;
  return {
    bridge,
    rotateToken() {
      subscription = null;
      endpoint = 'https://push.example/new';
    },
    setPermission(value: NotificationPermission) {
      currentPermission = value;
    },
    setSubscription(value: NativePushSubscription) {
      subscription = value;
    },
  };
}

let controllers: NotificationController[];
let workerDescriptor: PropertyDescriptor | undefined;
beforeEach(() => {
  controllers = [];
  localStorage.clear();
  vi.stubGlobal('Notification', undefined);
  vi.stubGlobal('PushManager', undefined);
  vi.stubGlobal('isSecureContext', true);
  vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue('iPhone');
  workerDescriptor = Object.getOwnPropertyDescriptor(navigator, 'serviceWorker');
  Object.defineProperty(navigator, 'serviceWorker', {
    configurable: true,
    get: () => {
      throw new Error('Native push must not access service workers');
    },
  });
});
afterEach(() => {
  controllers.forEach((controller) => controller.dispose());
  delete window.__termLLMNativePush;
  if (workerDescriptor) Object.defineProperty(navigator, 'serviceWorker', workerDescriptor);
  else Reflect.deleteProperty(navigator, 'serviceWorker');
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

function setup(permission: NotificationPermission = 'granted', overrides = {}) {
  const host = fakeHost(permission);
  window.__termLLMNativePush = host.bridge;
  const endpoints = {
    pushSubscribe: vi.fn(async (_body: unknown) => ({ id: 'canonical', state: 'active' })),
    pushUnsubscribe: vi.fn(async (_body: unknown) => undefined),
  };
  const controller = new NotificationController(
    { ...config, ...overrides },
    endpoints as never,
    localStorage,
    'push-id',
  );
  controllers.push(controller);
  return { host, endpoints, controller };
}

async function settle(controller: NotificationController) {
  await vi.waitFor(() => expect(controller.state.busy).toBe(false));
  await controller.reconcile();
}

describe('native notification host', () => {
  describe.each([
    'permission',
    'requestPermission',
    'getSubscription',
    'subscribe',
    'unsubscribe',
    'disable',
  ] as const)('invalid %s method', (method) => {
    it.each([undefined, 'not a function'])(
      'rejects %s and uses browser support detection',
      async (value) => {
        const { bridge } = fakeHost();
        window.__termLLMNativePush = { ...bridge, [method]: value } as unknown as NativePushBridge;
        // Browser fallback must report unsupported, not invoke a partial host.
        vi.stubGlobal('isSecureContext', false);
        expect(nativePushBridge()).toBeUndefined();
        const controller = new NotificationController(config, {} as never, localStorage, 'push-id');
        controllers.push(controller);
        expect(await controller.reconcile()).toMatchObject({
          status: 'unsupported',
          verified: false,
          detail: 'Notifications require a secure HTTPS connection.',
        });
        expect((await controller.enable()).status).toBe('unsupported');
        for (const callback of Object.values(bridge)) {
          if (typeof callback === 'function') expect(callback).not.toHaveBeenCalled();
        }
      },
    );
  });

  it('enrolls without browser APIs or the iOS Home Screen gate and verifies the server ID', async () => {
    const { controller, host, endpoints } = setup();
    expect(await controller.reconcile()).toMatchObject({
      status: 'subscribed',
      verified: true,
      subscriptionId: 'canonical',
      busy: false,
    });
    expect(host.bridge.permission).toHaveBeenCalledWith(config.prefix);
    expect(host.bridge.subscribe).toHaveBeenCalledWith(config.prefix, 'AQID');
    expect(host.bridge.requestPermission).not.toHaveBeenCalled();
    expect(endpoints.pushSubscribe).toHaveBeenCalledWith({
      endpoint: 'https://push.example/old',
      keys: { p256dh: 'public', auth: 'secret' },
    });
    expect(localStorage.getItem('push-id')).toBe('canonical');
  });

  it.each(['default', 'denied'] as const)(
    'never prompts or enrolls for %s permission',
    async (permission) => {
      const { controller, host, endpoints } = setup(permission);
      expect(await controller.reconcile()).toMatchObject({
        status: permission === 'denied' ? 'blocked' : 'unsubscribed',
        verified: false,
      });
      expect(host.bridge.requestPermission).not.toHaveBeenCalled();
      expect(host.bridge.subscribe).not.toHaveBeenCalled();
      expect(endpoints.pushSubscribe).not.toHaveBeenCalled();
      expect(await controller.enable()).toMatchObject({
        status: permission === 'denied' ? 'blocked' : 'subscribed',
      });
      expect(host.bridge.requestPermission).toHaveBeenCalledWith(config.prefix);
    },
  );

  it.each([{ vapidKey: '' }, { pushSupported: false }])(
    'still requires server support: %j',
    async (overrides) => {
      const { controller, host } = setup('granted', overrides);
      expect((await controller.enable()).status).toBe('unsupported');
      expect(host.bridge.requestPermission).not.toHaveBeenCalled();
      expect(host.bridge.subscribe).not.toHaveBeenCalled();
    },
  );

  it('persists opt-out through lifecycle checks and allows explicit re-enable', async () => {
    const { controller, host, endpoints } = setup();
    await controller.reconcile();
    expect((await controller.disable()).status).toBe('unsubscribed');
    expect(host.bridge.disable).toHaveBeenCalledWith(config.prefix);
    expect(host.bridge.unsubscribe).not.toHaveBeenCalled();
    expect(endpoints.pushUnsubscribe).toHaveBeenCalledWith({ id: 'canonical' });
    expect(localStorage.getItem('push-id')).toBeNull();
    controller.installLifecycle();
    window.dispatchEvent(new Event('termllm-native-push-change'));
    window.dispatchEvent(new Event('online'));
    await settle(controller);
    expect(host.bridge.subscribe).toHaveBeenCalledTimes(1);
    expect((await controller.enable()).status).toBe('subscribed');
    expect(host.bridge.subscribe).toHaveBeenCalledTimes(2);
  });

  it('captures the endpoint before disabling a host record without a cached ID', async () => {
    const { controller, host, endpoints } = setup();
    host.setSubscription({
      endpoint: 'https://push.example/old',
      keys: { p256dh: 'p', auth: 'a' },
      applicationServerKey: 'AQID',
    });
    await controller.disable();
    expect(endpoints.pushUnsubscribe).toHaveBeenCalledWith({
      endpoint: 'https://push.example/old',
    });
    expect(host.bridge.disable.mock.invocationCallOrder[0]).toBeLessThan(
      endpoints.pushUnsubscribe.mock.invocationCallOrder[0]!,
    );
  });

  it('repairs a rotated server key only when requested and deletes the old endpoint before replacement', async () => {
    const { controller, host, endpoints } = setup();
    host.setSubscription({
      endpoint: 'https://push.example/old',
      keys: { p256dh: 'p', auth: 'a' },
      applicationServerKey: 'BAUG',
    });
    expect((await controller.reconcile({ repair: false })).status).toBe('stale');
    expect(host.bridge.unsubscribe).not.toHaveBeenCalled();
    expect((await controller.enable()).status).toBe('subscribed');
    expect(host.bridge.unsubscribe).toHaveBeenCalledWith(config.prefix);
    expect(endpoints.pushUnsubscribe).toHaveBeenCalledExactlyOnceWith({
      endpoint: 'https://push.example/old',
    });
    expect(endpoints.pushUnsubscribe.mock.invocationCallOrder[0]).toBeLessThan(
      host.bridge.subscribe.mock.invocationCallOrder[0]!,
    );
  });

  it('repairs token rotation on the exact native change event and removes the listener on disposal', async () => {
    const { controller, host, endpoints } = setup();
    controller.installLifecycle();
    await settle(controller);
    host.rotateToken();
    window.dispatchEvent(new Event('termllm-native-push-change'));
    await vi.waitFor(() => expect(host.bridge.subscribe).toHaveBeenCalledTimes(2));
    await settle(controller);
    expect(endpoints.pushUnsubscribe).toHaveBeenCalledWith({ id: 'canonical' });
    expect(endpoints.pushSubscribe).toHaveBeenLastCalledWith({
      endpoint: 'https://push.example/new',
      keys: { p256dh: 'public', auth: 'secret' },
    });
    controller.dispose();
    const calls = host.bridge.permission.mock.calls.length;
    window.dispatchEvent(new Event('termllm-native-push-change'));
    window.dispatchEvent(new Event('pageshow'));
    await Promise.resolve();
    expect(host.bridge.permission).toHaveBeenCalledTimes(calls);
  });

  it.each([401, 403, 503])(
    'preserves canonical enrollment but not verification on HTTP %i',
    async (status) => {
      const { controller, endpoints } = setup();
      await controller.reconcile();
      endpoints.pushSubscribe.mockRejectedValueOnce(new APIError('unavailable', status));
      expect(await controller.reconcile()).toMatchObject({
        status: 'subscribed',
        verified: false,
        subscriptionId: 'canonical',
        busy: false,
      });
    },
  );

  it('preserves cached enrollment offline and recovers on the next check', async () => {
    const { controller, endpoints } = setup();
    await controller.reconcile();
    vi.spyOn(navigator, 'onLine', 'get').mockReturnValue(false);
    endpoints.pushSubscribe.mockRejectedValueOnce(new TypeError('offline'));
    expect(await controller.reconcile()).toMatchObject({
      status: 'subscribed',
      verified: false,
      subscriptionId: 'canonical',
    });
    expect((await controller.reconcile()).verified).toBe(true);
  });

  it('reports host failures without leaving enrollment busy', async () => {
    const { controller, host } = setup();
    host.bridge.permission.mockRejectedValueOnce(new Error('host unavailable'));
    expect(await controller.reconcile()).toMatchObject({
      status: 'stale',
      busy: false,
      verified: false,
      detail: 'host unavailable',
    });
    expect((await controller.reconcile()).status).toBe('subscribed');
  });

  it('observes permission changes without automatically requesting permission', async () => {
    const { controller, host } = setup();
    await controller.reconcile();
    host.setPermission('denied');
    expect((await controller.reconcile()).status).toBe('blocked');
    host.setPermission('default');
    expect((await controller.reconcile()).status).toBe('unsubscribed');
    expect(host.bridge.requestPermission).not.toHaveBeenCalled();
  });

  it('never touches serviceWorker.ready for native completion fallback', async () => {
    const { controller } = setup();
    await controller.reconcile();
    vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
    await expect(
      controller.signalCompletion({ responseId: 'r', sessionId: 's', outcome: 'completed' }),
    ).resolves.toBeUndefined();
  });
});
