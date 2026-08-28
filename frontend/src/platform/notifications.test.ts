import { afterEach, describe, expect, it, vi } from 'vitest';
import type { AppConfig } from '../app/config';
import {
  NotificationController,
  base64URLToBytes,
  completionEventId,
  notificationTag,
} from './notifications';

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
  webRTC: false,
  signalingURL: '',
};

afterEach(() => vi.restoreAllMocks());

describe('notification enrollment controller', () => {
  it('derives deterministic completion identity and tags', () => {
    expect(completionEventId('resp_1', 'sub_1')).toBe('completion:resp_1:sub_1');
    expect(notificationTag('completion:resp_1:sub_1')).toBe(
      'term-llm-completion:completion:resp_1:sub_1',
    );
    expect([...base64URLToBytes('AQID')]).toEqual([1, 2, 3]);
  });

  it('reports insecure contexts as unsupported without trusting cached enrollment', async () => {
    Object.defineProperty(globalThis, 'isSecureContext', { configurable: true, value: false });
    localStorage.setItem('push-id', 'cached-subscription');
    const controller = new NotificationController(config, {} as never, localStorage, 'push-id');
    const state = await controller.reconcile();
    expect(state).toMatchObject({ status: 'unsupported', verified: false });
    expect(state.detail).toContain('HTTPS');
  });

  it('reconciles browser permission, subscription key, and canonical server acknowledgement', async () => {
    Object.defineProperty(globalThis, 'isSecureContext', { configurable: true, value: true });
    Object.defineProperty(window, 'PushManager', { configurable: true, value: class {} });
    Object.defineProperty(window, 'Notification', {
      configurable: true,
      value: { permission: 'granted', requestPermission: vi.fn() },
    });
    Object.defineProperty(globalThis, 'Notification', {
      configurable: true,
      value: { permission: 'granted', requestPermission: vi.fn() },
    });
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
    Object.defineProperty(navigator, 'serviceWorker', {
      configurable: true,
      value: { register: vi.fn(async () => registration) },
    });
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
  });
});
