import type { AppConfig } from '../app/config';
import type { Endpoints } from '../api/endpoints';
import { APIError } from '../api/client';
import { registerServiceWorker } from './browser';
import { nativePushBridge, type NativePushSubscription } from './native-push';

type Enrollment = Pick<PushSubscription, 'endpoint' | 'options' | 'toJSON' | 'unsubscribe'>;

export type NotificationStatus =
  'unsupported' | 'blocked' | 'unsubscribed' | 'subscribed' | 'stale';

export interface NotificationState {
  status: NotificationStatus;
  busy: boolean;
  detail: string;
  verified: boolean;
  subscriptionId?: string;
}

export interface CompletionNotificationEvent {
  responseId: string;
  sessionId: string;
  outcome: 'completed' | 'failed';
  createdAt?: string;
}

const initialState: NotificationState = {
  status: 'unsupported',
  busy: false,
  detail: 'Checking notification support…',
  verified: false,
};

export const notificationTag = (eventId: string) => `term-llm-completion:${eventId}`;
export const completionEventId = (responseId: string, subscriptionId: string) =>
  `completion:${responseId}:${subscriptionId}`;

export function base64URLToBytes(value: string): Uint8Array<ArrayBuffer> {
  const padding = '='.repeat((4 - (value.length % 4)) % 4);
  const raw = atob((value + padding).replaceAll('-', '+').replaceAll('_', '/'));
  return Uint8Array.from(raw, (character) => character.charCodeAt(0));
}

function keysEqual(actual: ArrayBuffer | null, expected: Uint8Array<ArrayBuffer>): boolean {
  if (!actual) return false;
  const bytes = new Uint8Array(actual);
  return (
    bytes.length === expected.length && bytes.every((value, index) => value === expected[index])
  );
}

function isIOS(): boolean {
  return (
    /iPad|iPhone|iPod/.test(navigator.userAgent) ||
    (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1)
  );
}

function isStandalone(): boolean {
  return (
    matchMedia('(display-mode: standalone)').matches ||
    Boolean((navigator as Navigator & { standalone?: boolean }).standalone)
  );
}

export class NotificationController {
  private current: NotificationState = initialState;
  private listener: (state: NotificationState) => void = () => {};
  private reconcilePromise: Promise<NotificationState> | null = null;
  private repairAttempted = false;
  private lastFocusCheck = 0;
  private disposed = false;
  private cleanup: Array<() => void> = [];
  private readonly native = nativePushBridge();
  private nativeEndpoint = '';

  private wrapNative(subscription: NativePushSubscription): Enrollment {
    this.nativeEndpoint = subscription.endpoint;
    return {
      endpoint: subscription.endpoint,
      options: {
        applicationServerKey: base64URLToBytes(subscription.applicationServerKey).buffer,
        userVisibleOnly: true,
      },
      toJSON: () => ({ endpoint: subscription.endpoint, keys: subscription.keys }),
      unsubscribe: async () => {
        await this.native!.unsubscribe(this.config.prefix);
        return true;
      },
    };
  }

  private async enrollmentManager() {
    if (this.native) {
      return {
        getSubscription: async (): Promise<Enrollment | null> => {
          const subscription = await this.native!.getSubscription(this.config.prefix);
          return subscription ? this.wrapNative(subscription) : null;
        },
        subscribe: async (): Promise<Enrollment> =>
          this.wrapNative(await this.native!.subscribe(this.config.prefix, this.config.vapidKey)),
      };
    }
    const registration = await registerServiceWorker(this.config);
    return registration?.pushManager;
  }

  constructor(
    private readonly config: AppConfig,
    private readonly endpoints: Endpoints,
    private readonly storage: Storage,
    private readonly cacheKey: string,
  ) {}

  get state(): NotificationState {
    return this.current;
  }

  setListener(listener: (state: NotificationState) => void): void {
    this.listener = listener;
    listener(this.current);
  }

  private update(next: NotificationState): NotificationState {
    if (this.disposed) return this.current;
    this.current = next;
    this.listener(next);
    return next;
  }

  private unsupportedReason(): string {
    if (this.native) {
      if (!this.config.vapidKey) return 'This server has not configured Web Push.';
      if (this.config.pushSupported === false)
        return 'Persistent notification storage is unavailable on this server.';
      return '';
    }
    if (!globalThis.isSecureContext) return 'Notifications require a secure HTTPS connection.';
    if (!('Notification' in window)) return 'This browser does not support notifications.';
    if (!('serviceWorker' in navigator)) return 'This browser does not support service workers.';
    if (!('PushManager' in window)) return 'This browser does not support Web Push.';
    if (!this.config.vapidKey) return 'This server has not configured Web Push.';
    if (this.config.pushSupported === false)
      return 'Persistent notification storage is unavailable on this server.';
    if (isIOS() && !isStandalone())
      return 'On iPhone or iPad, add this site to the Home Screen before enabling notifications.';
    return '';
  }

  async reconcile(options: { repair?: boolean } = {}): Promise<NotificationState> {
    if (this.disposed) return this.current;
    if (this.reconcilePromise) return this.reconcilePromise;
    this.reconcilePromise = this.reconcileNow(options)
      .catch((error: unknown) =>
        this.update({
          status: 'stale',
          busy: false,
          verified: false,
          detail:
            error instanceof Error
              ? error.message
              : 'Notification enrollment could not be checked.',
        }),
      )
      .finally(() => {
        this.reconcilePromise = null;
      });
    return this.reconcilePromise;
  }

  private async reconcileNow({ repair = true }: { repair?: boolean }): Promise<NotificationState> {
    const unsupported = this.unsupportedReason();
    if (unsupported)
      return this.update({
        status: 'unsupported',
        busy: false,
        detail: unsupported,
        verified: false,
      });

    this.update({ ...this.current, busy: true, detail: 'Checking notification enrollment…' });
    const permission = this.native
      ? await this.native.permission(this.config.prefix)
      : Notification.permission;
    const manager = await this.enrollmentManager();
    if (!manager)
      return this.update({
        status: 'unsupported',
        busy: false,
        verified: false,
        detail: 'The notification service worker could not be registered.',
      });

    let subscription = await manager.getSubscription();
    const cachedId = this.storage.getItem(this.cacheKey) || '';
    if (permission === 'denied') {
      return this.update({
        status: 'blocked',
        busy: false,
        verified: false,
        detail: 'Notifications are blocked. Re-enable them in browser or system settings.',
      });
    }
    if (permission === 'default') {
      if (this.native && !subscription) await this.deleteServer(cachedId, this.nativeEndpoint);
      if (subscription) {
        await subscription.unsubscribe().catch(() => false);
        await this.deleteServer(cachedId, subscription.endpoint);
      }
      this.storage.removeItem(this.cacheKey);
      return this.update({
        status: 'unsubscribed',
        busy: false,
        verified: false,
        detail: 'Notifications are not enabled.',
      });
    }

    // Token/relay changes can invalidate the host record before its old
    // endpoint can be read. The canonical server ID still removes that route.
    if (this.native && !subscription && repair) {
      await this.deleteServer(cachedId, this.nativeEndpoint);
      this.storage.removeItem(this.cacheKey);
    }

    const applicationServerKey = base64URLToBytes(this.config.vapidKey);
    if (
      subscription &&
      !keysEqual(subscription.options.applicationServerKey, applicationServerKey)
    ) {
      if (!repair || this.repairAttempted) {
        return this.update({
          status: 'stale',
          busy: false,
          verified: false,
          subscriptionId: cachedId || undefined,
          detail: 'The server notification key changed. Retry to repair enrollment.',
        });
      }
      this.repairAttempted = true;
      const oldEndpoint = subscription.endpoint;
      await subscription.unsubscribe().catch(() => false);
      await this.deleteServer(cachedId, oldEndpoint);
      this.storage.removeItem(this.cacheKey);
      subscription = null;
    }

    if (!subscription && repair) {
      try {
        subscription = await manager.subscribe({
          userVisibleOnly: true,
          applicationServerKey,
        });
      } catch (error) {
        return this.update({
          status: 'stale',
          busy: false,
          verified: false,
          detail:
            error instanceof Error
              ? error.message
              : 'The browser subscription could not be created.',
        });
      }
    }
    if (!subscription) {
      return this.update({
        status: 'unsubscribed',
        busy: false,
        verified: false,
        detail: 'Notification permission is granted, but this browser is not subscribed.',
      });
    }

    try {
      const acknowledged = await this.endpoints.pushSubscribe(subscription.toJSON());
      this.storage.setItem(this.cacheKey, acknowledged.id);
      this.repairAttempted = false;
      return this.update({
        status: acknowledged.state === 'active' ? 'subscribed' : 'stale',
        busy: false,
        verified: acknowledged.state === 'active',
        subscriptionId: acknowledged.id,
        detail:
          acknowledged.state === 'active'
            ? 'Notifications are enabled and verified.'
            : 'The push service marked this subscription stale. Retry to repair it.',
      });
    } catch (error) {
      if (error instanceof APIError && (error.status === 401 || error.status === 403)) {
        return this.update({
          status: cachedId ? 'subscribed' : 'unsubscribed',
          busy: false,
          verified: false,
          subscriptionId: cachedId || undefined,
          detail: 'Sign in again to verify notification enrollment.',
        });
      }
      if (cachedId && (!navigator.onLine || (error instanceof APIError && error.status >= 500))) {
        return this.update({
          status: 'subscribed',
          busy: false,
          verified: false,
          subscriptionId: cachedId,
          detail: 'This browser is subscribed; server verification will retry when online.',
        });
      }
      return this.update({
        status: 'stale',
        busy: false,
        verified: false,
        subscriptionId: cachedId || undefined,
        detail: navigator.onLine
          ? 'The browser subscription could not be synchronized with the server.'
          : 'Notification enrollment is waiting for a network connection.',
      });
    }
  }

  async enable(): Promise<NotificationState> {
    const unsupported = this.unsupportedReason();
    if (unsupported)
      return this.update({
        status: 'unsupported',
        busy: false,
        detail: unsupported,
        verified: false,
      });
    if (!this.native && Notification.permission === 'denied')
      return this.reconcile({ repair: false });
    this.update({ ...this.current, busy: true, detail: 'Waiting for notification permission…' });
    if (this.native) {
      try {
        await this.native.requestPermission(this.config.prefix);
      } catch (error) {
        return this.update({
          status: 'stale',
          busy: false,
          verified: false,
          detail:
            error instanceof Error
              ? error.message
              : 'Notification permission could not be requested.',
        });
      }
    } else if (Notification.permission === 'default') await Notification.requestPermission();
    this.repairAttempted = false;
    return this.reconcile({ repair: true });
  }

  async disable(): Promise<NotificationState> {
    this.update({ ...this.current, busy: true, detail: 'Disabling notifications…' });
    if (this.native) return this.disableNative();
    const registration = await navigator.serviceWorker?.getRegistration(`${this.config.prefix}/`);
    const subscription = await registration?.pushManager.getSubscription();
    const cachedId = this.storage.getItem(this.cacheKey) || this.current.subscriptionId || '';
    if (subscription) await subscription.unsubscribe().catch(() => false);
    await this.deleteServer(cachedId, subscription?.endpoint || '');
    this.storage.removeItem(this.cacheKey);
    this.repairAttempted = false;
    return this.update({
      status: Notification.permission === 'denied' ? 'blocked' : 'unsubscribed',
      busy: false,
      verified: false,
      detail:
        Notification.permission === 'denied'
          ? 'Notifications remain blocked in browser or system settings.'
          : 'Notifications are disabled.',
    });
  }

  private async disableNative(): Promise<NotificationState> {
    try {
      const subscription = await this.native!.getSubscription(this.config.prefix);
      const endpoint = subscription?.endpoint || this.nativeEndpoint;
      const id = this.storage.getItem(this.cacheKey) || this.current.subscriptionId || '';
      // disable, unlike unsubscribe, persists the user's per-scope opt-out.
      await this.native!.disable(this.config.prefix);
      await this.deleteServer(id, endpoint);
      this.nativeEndpoint = '';
      this.storage.removeItem(this.cacheKey);
      this.repairAttempted = false;
      const permission = await this.native!.permission(this.config.prefix);
      return this.update({
        status: permission === 'denied' ? 'blocked' : 'unsubscribed',
        busy: false,
        verified: false,
        detail: 'Notifications are disabled.',
      });
    } catch (error) {
      return this.update({
        ...this.current,
        busy: false,
        verified: false,
        detail: error instanceof Error ? error.message : 'Notifications could not be disabled.',
      });
    }
  }

  private async deleteServer(id: string, endpoint: string): Promise<void> {
    if (!id && !endpoint) return;
    await this.endpoints.pushUnsubscribe(id ? { id } : { endpoint }).catch(() => undefined);
  }

  installLifecycle(): () => void {
    const reconcile = () => void this.reconcile();
    const focus = () => {
      const now = Date.now();
      if (now - this.lastFocusCheck < 60_000) return;
      this.lastFocusCheck = now;
      reconcile();
    };
    addEventListener('pageshow', reconcile);
    addEventListener('online', reconcile);
    addEventListener('focus', focus);
    const nativeChange = () => {
      // A change arriving during enrollment must be checked after that pass.
      void (this.reconcilePromise || Promise.resolve()).then(() => {
        if (!this.disposed) void this.reconcile();
      });
    };
    if (this.native) {
      addEventListener('termllm-native-push-change', nativeChange);
      this.cleanup.push(() => removeEventListener('termllm-native-push-change', nativeChange));
    }
    const worker = this.native ? undefined : navigator.serviceWorker;
    worker?.addEventListener('controllerchange', reconcile);
    const message = (event: MessageEvent) => this.handleWorkerMessage(event);
    worker?.addEventListener('message', message);
    this.cleanup.push(
      () => removeEventListener('pageshow', reconcile),
      () => removeEventListener('online', reconcile),
      () => removeEventListener('focus', focus),
      () => worker?.removeEventListener('controllerchange', reconcile),
      () => worker?.removeEventListener('message', message),
    );
    if (!this.native)
      void navigator.permissions
        ?.query({ name: 'notifications' as PermissionName })
        .then((status) => {
          if (this.disposed) return;
          status.addEventListener('change', reconcile);
          this.cleanup.push(() => status.removeEventListener('change', reconcile));
        })
        .catch(() => undefined);
    void this.reconcile();
    return () => this.dispose();
  }

  private handleWorkerMessage(event: MessageEvent): void {
    const data = event.data as { type?: string; tag?: string; url?: string } | null;
    if (
      data?.type === 'completion-push-shown' &&
      data.tag &&
      document.visibilityState === 'visible'
    ) {
      void navigator.serviceWorker.ready
        .then((registration) => registration.getNotifications({ tag: data.tag! }))
        .then((notifications) => notifications.forEach((notification) => notification.close()));
    }
    if (data?.type === 'notification-route' && data.url) {
      const target = new URL(data.url, location.href);
      if (target.origin === location.origin && target.pathname.startsWith(`${this.config.prefix}/`))
        location.assign(target.href);
    }
  }

  async signalCompletion(
    event: CompletionNotificationEvent,
    originatingSubscriptionId = this.current.subscriptionId || '',
  ): Promise<void> {
    if (
      document.visibilityState !== 'hidden' ||
      this.current.status !== 'subscribed' ||
      !originatingSubscriptionId ||
      this.current.subscriptionId !== originatingSubscriptionId
    )
      return;
    // Native completion delivery belongs to the server/host, never a local
    // service-worker fallback (which is absent in embedded web views).
    if (this.native) return;
    const eventId = completionEventId(event.responseId, originatingSubscriptionId);
    const registration = await navigator.serviceWorker.ready;
    registration.active?.postMessage({
      type: 'completion-notification',
      payload: {
        version: 1,
        event_id: eventId,
        response_id: event.responseId,
        session_id: event.sessionId,
        outcome: event.outcome,
        title: event.outcome === 'failed' ? 'Response failed' : 'Response complete',
        body:
          event.outcome === 'failed'
            ? 'Your term-llm response stopped with an error.'
            : 'Your term-llm response is ready.',
        url: `${this.config.prefix}/chat/${encodeURIComponent(event.sessionId)}`,
        created_at: event.createdAt || new Date().toISOString(),
      },
    });
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.cleanup.splice(0).forEach((dispose) => dispose());
  }
}
