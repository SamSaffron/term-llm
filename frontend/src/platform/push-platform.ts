import type { AppConfig } from '../app/config';

/**
 * Native push bridge implemented by the iOS shell in term-llm-app.
 * Scopes are controller config.prefix strings (including ''). The app emits
 * termllm-native-push-change on window when its effective push state changes.
 */
export interface TermLLMNativePush {
  version: 1;
  notificationCapabilities?(): Promise<{ version: 2; window_id: string; document_id?: string }>;
  notificationPresence?(payload: NotificationPresence): Promise<void>;
  notificationRead?(payload: NotificationRead): Promise<void>;
  notificationEnrollment?(payload: { source: string; subscription_id: string }): Promise<void>;
  notificationPendingCleanup?(payload: {
    source: string;
  }): Promise<Array<{ subscription_id: string }>>;
  notificationCleanupAcknowledged?(payload: {
    source: string;
    subscription_id: string;
  }): Promise<void>;
  permission(scope: string): Promise<NotificationPermission>;
  requestPermission(scope: string): Promise<NotificationPermission>;
  getSubscription(scope: string): Promise<NativePushSubscription | null>;
  subscribe(scope: string, applicationServerKey: string): Promise<NativePushSubscription>;
  /** Forgets a record without opting the node out (e.g. after key rotation). */
  unsubscribe(scope: string): Promise<boolean>;
  /** Remembers the user's per-node opt-out and forgets its record. */
  disable(scope: string): Promise<boolean>;
}

export interface NotificationPresence {
  version: 2;
  window_id: string;
  document_id: string;
  sequence: number;
  active: boolean;
  expires_at: string;
  source: string;
}

export interface NotificationRead {
  version: 2;
  store_instance_id: string;
  source: string;
  session_id: string;
  attention_seq: number;
}

export interface NativePushSubscription {
  endpoint: string;
  keys: { p256dh: string; auth: string };
  applicationServerKey: string;
}

declare global {
  interface Window {
    readonly __termLLMNativePush?: TermLLMNativePush;
  }
}

export type NotificationSubscription = Pick<
  PushSubscription,
  'endpoint' | 'options' | 'toJSON' | 'unsubscribe'
>;

export interface NotificationRegistration {
  pushManager: {
    getSubscription(): Promise<NotificationSubscription | null>;
    subscribe(options: PushSubscriptionOptionsInit): Promise<NotificationSubscription>;
  };
}

export function base64URLToBytes(value: string): Uint8Array<ArrayBuffer> {
  const padding = '='.repeat((4 - (value.length % 4)) % 4);
  const raw = atob((value + padding).replaceAll('-', '+').replaceAll('_', '/'));
  return Uint8Array.from(raw, (character) => character.charCodeAt(0));
}

/** Adapts native records to the Web Push surface used by enrollment reconciliation. */
export function nativePushRegistration(
  bridge: TermLLMNativePush,
  scope: string,
  applicationServerKey: string,
): NotificationRegistration {
  const wrap = (subscription: NativePushSubscription): NotificationSubscription => ({
    endpoint: subscription.endpoint,
    options: {
      applicationServerKey: base64URLToBytes(subscription.applicationServerKey).buffer,
      userVisibleOnly: true,
    },
    toJSON: () => ({ endpoint: subscription.endpoint, keys: subscription.keys }),
    unsubscribe: () => bridge.unsubscribe(scope),
  });
  return {
    pushManager: {
      async getSubscription() {
        const subscription = await bridge.getSubscription(scope);
        return subscription ? wrap(subscription) : null;
      },
      async subscribe() {
        // This registration is bound to the controller's original config.vapidKey
        // string; only the browser path needs its decoded applicationServerKey.
        return wrap(await bridge.subscribe(scope, applicationServerKey));
      },
    },
  };
}

export async function registerServiceWorker(
  config: AppConfig,
): Promise<ServiceWorkerRegistration | null> {
  if (!('serviceWorker' in navigator)) return null;
  try {
    return await navigator.serviceWorker.register(
      `${config.prefix}/sw.js?v=${encodeURIComponent(config.version)}`,
      { scope: `${config.prefix}/` },
    );
  } catch {
    return null;
  }
}
