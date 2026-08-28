import type { AppConfig } from '../app/config';
import type { Endpoints } from '../api/endpoints';
import { APIError } from '../api/client';
import { registerServiceWorker } from './browser';

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
    if (this.reconcilePromise) return this.reconcilePromise;
    this.reconcilePromise = this.reconcileNow(options).finally(() => {
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
    const permission = Notification.permission;
    const registration = await registerServiceWorker(this.config);
    if (!registration)
      return this.update({
        status: 'unsupported',
        busy: false,
        verified: false,
        detail: 'The notification service worker could not be registered.',
      });

    let subscription = await registration.pushManager.getSubscription();
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
        subscription = await registration.pushManager.subscribe({
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
    if (Notification.permission === 'denied') return this.reconcile({ repair: false });
    this.update({ ...this.current, busy: true, detail: 'Waiting for notification permission…' });
    if (Notification.permission === 'default') await Notification.requestPermission();
    this.repairAttempted = false;
    return this.reconcile({ repair: true });
  }

  async disable(): Promise<NotificationState> {
    this.update({ ...this.current, busy: true, detail: 'Disabling notifications…' });
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
    navigator.serviceWorker?.addEventListener('controllerchange', reconcile);
    const message = (event: MessageEvent) => this.handleWorkerMessage(event);
    navigator.serviceWorker?.addEventListener('message', message);
    this.cleanup.push(
      () => removeEventListener('pageshow', reconcile),
      () => removeEventListener('online', reconcile),
      () => removeEventListener('focus', focus),
      () => navigator.serviceWorker?.removeEventListener('controllerchange', reconcile),
      () => navigator.serviceWorker?.removeEventListener('message', message),
    );
    void navigator.permissions
      ?.query({ name: 'notifications' as PermissionName })
      .then((status) => {
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
