import type { AppConfig } from '../app/config';
import type { Endpoints } from '../api/endpoints';
import { APIError } from '../api/client';
import { base64URLToBytes, nativePushRegistration, registerServiceWorker } from './push-platform';
import type { TermLLMNativePush } from './push-platform';
import type { NotificationOwner } from './notification-owner';
import type { PresenceSource } from './presence';

export type NotificationStatus =
  'unsupported' | 'blocked' | 'unsubscribed' | 'subscribed' | 'stale';

export interface NotificationState {
  status: NotificationStatus;
  busy: boolean;
  detail: string;
  verified: boolean;
  subscriptionId?: string;
}

const initialState: NotificationState = {
  status: 'unsupported',
  busy: false,
  detail: 'Checking notification support…',
  verified: false,
};

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
  private owner: NotificationOwner | null = null;
  private ownerPromise: Promise<NotificationOwner> | null = null;
  private presence: PresenceSource | undefined;
  private enrollmentGeneration = 0;
  private readonly nativeBridge: TermLLMNativePush | undefined;

  constructor(
    private readonly config: AppConfig,
    private readonly endpoints: Endpoints,
    private readonly storage: Storage,
    private readonly cacheKey: string,
    nativePush: TermLLMNativePush | undefined,
  ) {
    const bridge = nativePush;
    this.nativeBridge = bridge?.version === 1 ? bridge : undefined;
  }

  private getOwner(): Promise<NotificationOwner> {
    this.ownerPromise ||= import('./notification-owner').then(({ NotificationOwner }) => {
      const owner = new NotificationOwner(
        this.config.prefix,
        this.endpoints,
        () =>
          !this.disposed &&
          !this.optedOut() &&
          this.current.status === 'subscribed' &&
          this.current.verified
            ? this.current.subscriptionId || ''
            : '',
        this.nativeBridge,
      );
      this.owner = owner;
      if (this.disposed) owner.dispose();
      else if (this.presence) owner.start(this.presence);
      return owner;
    });
    return this.ownerPromise;
  }

  private optedOut(): boolean {
    return this.storage.getItem(`${this.cacheKey}:off`) !== null;
  }

  private syncWorkerPreference(): void {
    if (this.nativeBridge) return;
    void navigator.serviceWorker
      ?.getRegistration?.(`${this.config.prefix}/`)
      .then((registration) => {
        registration?.active?.postMessage({
          type: 'notification-preference',
          enabled: !this.optedOut(),
        });
      })
      .catch(() => undefined);
  }

  private hasRetirements(): boolean {
    const prefix = `${this.cacheKey}:retire:`;
    return Array.from({ length: this.storage.length }, (_, i) => this.storage.key(i)).some((key) =>
      key?.startsWith(prefix),
    );
  }

  private cleanupPending(): NotificationState {
    return this.update({
      status: 'stale',
      busy: false,
      verified: false,
      detail: 'Notification cleanup will retry when this site is signed in and online.',
    });
  }

  private disabledState(): NotificationState {
    return this.update({
      status: 'unsubscribed',
      busy: false,
      verified: false,
      detail: 'Notifications are disabled on this device for this site/agent.',
    });
  }

  async originHeaders(): Promise<Record<string, string>> {
    if (this.config.notificationVersion !== 2) return {};
    const origin = await this.getOwner()
      .then((owner) => owner.origin())
      .catch(() => null);
    return origin && !this.optedOut()
      ? {
          'X-Term-LLM-Notification-Version': '2',
          'X-Term-LLM-Window-ID': origin.window_id,
          'X-Term-LLM-Document-ID': origin.document_id,
          'X-Term-LLM-Notification-Source': this.config.prefix,
          'X-Term-LLM-Push-Subscription-ID': origin.subscription_id,
        }
      : {};
  }

  /** Called only with authenticated ACK/snapshot values, never cached UI hints. */
  acknowledgeRead(sessionId: string, state: Record<string, unknown>): void {
    const store = state.store_instance_id || state.attention_store_instance_id;
    const sequence = state.seen_through_seq;
    if (
      typeof store !== 'string' ||
      !store ||
      typeof sequence !== 'number' ||
      !Number.isSafeInteger(sequence) ||
      sequence <= 0
    )
      return;
    void this.getOwner()
      .then((owner) =>
        owner.read({
          version: 2,
          store_instance_id: store,
          source: this.config.prefix,
          session_id: sessionId,
          attention_seq: sequence,
        }),
      )
      .catch(() => undefined);
  }

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
    if (next.status === 'subscribed' && next.verified && this.config.notificationVersion === 2) {
      this.syncWorkerPreference();
      void this.getOwner()
        .then((owner) => owner.publish())
        .catch(() => undefined);
    }
    return next;
  }

  private unsupportedReason(): string {
    if (!this.nativeBridge) {
      if (!globalThis.isSecureContext) return 'Notifications require a secure HTTPS connection.';
      if (!('Notification' in window)) return 'This browser does not support notifications.';
      if (!('serviceWorker' in navigator)) return 'This browser does not support service workers.';
      if (!('PushManager' in window)) return 'This browser does not support Web Push.';
    }
    if (!this.config.vapidKey) return 'This server has not configured Web Push.';
    if (this.config.pushSupported === false)
      return 'Persistent notification storage is unavailable on this server.';
    if (!this.nativeBridge && isIOS() && !isStandalone())
      return 'On iPhone or iPad, add this site to the Home Screen before enabling notifications.';
    return '';
  }

  async reconcile(options: { repair?: boolean } = {}): Promise<NotificationState> {
    if (this.reconcilePromise) return this.reconcilePromise;
    this.reconcilePromise = this.reconcileNow(options)
      .catch((error: unknown) => {
        if (!this.nativeBridge) throw error;
        return this.nativeFailure(error);
      })
      .finally(() => {
        this.reconcilePromise = null;
      });
    return this.reconcilePromise;
  }

  private async reconcileNow({ repair = true }: { repair?: boolean }): Promise<NotificationState> {
    const generation = this.enrollmentGeneration;
    const cancelled = () => generation !== this.enrollmentGeneration || this.optedOut();
    this.syncWorkerPreference();
    // Retry a rejected native disable too: the local app preference, not only
    // server retirement, must become durable before off is reported reconciled.
    if (this.optedOut()) await this.nativeBridge?.disable(this.config.prefix);
    await this.drainRetirements();
    if (cancelled()) return this.disabledState();
    if (this.hasRetirements()) return this.cleanupPending();
    const unsupported = this.unsupportedReason();
    if (unsupported)
      return this.update({
        status: 'unsupported',
        busy: false,
        detail: unsupported,
        verified: false,
      });

    this.update({ ...this.current, busy: true, detail: 'Checking notification enrollment…' });
    const permission = this.nativeBridge
      ? await this.nativeBridge.permission(this.config.prefix)
      : Notification.permission;
    const registration = this.nativeBridge
      ? nativePushRegistration(this.nativeBridge, this.config.prefix, this.config.vapidKey)
      : await registerServiceWorker(this.config);
    if (!registration)
      return this.update({
        status: 'unsupported',
        busy: false,
        verified: false,
        detail: 'The notification service worker could not be registered.',
      });

    let subscription = await registration.pushManager.getSubscription();
    let cachedId = this.storage.getItem(this.cacheKey) || '';
    if (cancelled()) {
      await subscription?.unsubscribe().catch(() => false);
      return this.disabledState();
    }
    if (permission === 'denied') {
      return this.update({
        status: 'blocked',
        busy: false,
        verified: false,
        detail: this.nativeBridge
          ? 'Notifications are blocked for term-llm in iOS Settings.'
          : 'Notifications are blocked. Re-enable them in browser or system settings.',
      });
    }
    if (permission === 'default') {
      if (subscription) {
        await subscription.unsubscribe().catch(() => false);
      } else if (this.nativeBridge) {
        await this.nativeBridge.unsubscribe(this.config.prefix).catch(() => false);
      }
      if (subscription || this.nativeBridge) {
        await this.deleteServer(cachedId, subscription?.endpoint || '');
      }
      this.storage.removeItem(this.cacheKey);
      return this.update({
        status: 'unsubscribed',
        busy: false,
        verified: false,
        detail: this.nativeBridge
          ? 'Notifications are off for this site/agent.'
          : 'Notifications are not enabled.',
      });
    }

    if (this.nativeBridge && !subscription && cachedId && repair) {
      // Token rotation can hide the native record before we can read its old
      // endpoint. Retire its server ID before replacing it. Unlike best-effort
      // cleanup, a failed deletion must retain the ID for the next reconcile.
      if (!(await this.deleteServer(cachedId, ''))) return this.cleanupPending();
      this.storage.removeItem(this.cacheKey);
      cachedId = '';
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

    if (cancelled()) return this.disabledState();
    if (this.hasRetirements()) return this.cleanupPending();
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
        detail: this.nativeBridge
          ? 'Notification permission is granted, but this site/agent is not subscribed.'
          : 'Notification permission is granted, but this browser is not subscribed.',
      });
    }

    if (cancelled()) {
      await subscription.unsubscribe().catch(() => false);
      return this.disabledState();
    }
    try {
      const acknowledged = await this.endpoints.pushSubscribe({
        ...subscription.toJSON(),
        client_kind: this.nativeBridge ? 'app' : 'browser',
      });
      if (cancelled()) {
        await this.deleteServer(acknowledged.id, subscription.endpoint);
        await subscription.unsubscribe().catch(() => false);
        return this.disabledState();
      }
      this.storage.setItem(this.cacheKey, acknowledged.id);
      await this.nativeBridge?.notificationEnrollment?.({
        source: this.config.prefix,
        subscription_id: acknowledged.id,
      });
      if (cancelled()) {
        await this.deleteServer(acknowledged.id, subscription.endpoint);
        return this.disabledState();
      }
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
      if (cancelled()) return this.disabledState();
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
          detail: `${this.nativeBridge ? 'This site/agent' : 'This browser'} is subscribed; server verification will retry when online.`,
        });
      }
      return this.update({
        status: 'stale',
        busy: false,
        verified: false,
        subscriptionId: cachedId || undefined,
        detail: navigator.onLine
          ? `The ${this.nativeBridge ? 'native' : 'browser'} subscription could not be synchronized with the server.`
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
    if (!this.nativeBridge && Notification.permission === 'denied')
      return this.reconcile({ repair: false });
    this.enrollmentGeneration++;
    this.storage.removeItem(`${this.cacheKey}:off`);
    this.syncWorkerPreference();
    // A pending reconcile must finish (and retire its result) before explicit enable.
    if (this.reconcilePromise) await this.reconcilePromise;
    this.update({ ...this.current, busy: true, detail: 'Waiting for notification permission…' });
    if (this.nativeBridge) {
      try {
        await this.nativeBridge.requestPermission(this.config.prefix);
      } catch (error) {
        return this.nativeFailure(error);
      }
    } else if (Notification.permission === 'default') await Notification.requestPermission();
    this.repairAttempted = false;
    return this.reconcile({ repair: true });
  }

  async disable(): Promise<NotificationState> {
    this.storage.setItem(`${this.cacheKey}:off`, '1');
    this.enrollmentGeneration++;
    this.syncWorkerPreference();
    // Persist cleanup before any browser/native/network await.
    this.queueRetirement(
      this.storage.getItem(this.cacheKey) || this.current.subscriptionId || '',
      '',
    );
    void this.owner?.publish();
    if (this.nativeBridge) return this.disableNow().catch((error) => this.nativeFailure(error));
    return this.disableNow();
  }

  private async disableNow(): Promise<NotificationState> {
    this.update({ ...this.current, busy: true, detail: 'Disabling notifications…' });
    const registration = this.nativeBridge
      ? nativePushRegistration(this.nativeBridge, this.config.prefix, this.config.vapidKey)
      : await navigator.serviceWorker?.getRegistration(`${this.config.prefix}/`);
    const subscription = await registration?.pushManager.getSubscription();
    const cachedId = this.storage.getItem(this.cacheKey) || this.current.subscriptionId || '';
    if (this.nativeBridge) await this.nativeBridge.disable(this.config.prefix);
    else if (subscription) await subscription.unsubscribe().catch(() => false);
    await this.deleteServer(cachedId, subscription?.endpoint || '');
    this.storage.removeItem(this.cacheKey);
    this.repairAttempted = false;
    const permission = this.nativeBridge
      ? await this.nativeBridge.permission(this.config.prefix)
      : Notification.permission;
    return this.update({
      status: permission === 'denied' ? 'blocked' : 'unsubscribed',
      busy: false,
      verified: false,
      detail: this.nativeBridge
        ? permission === 'denied'
          ? 'Notifications are blocked for term-llm in iOS Settings.'
          : 'Notifications are off for this site/agent.'
        : permission === 'denied'
          ? 'Notifications remain blocked in browser or system settings.'
          : 'Notifications are disabled.',
    });
  }

  private nativeFailure(error: unknown): NotificationState {
    return this.update({
      ...this.current,
      status: 'stale',
      busy: false,
      verified: false,
      detail: error instanceof Error ? error.message : 'Native notification enrollment failed.',
    });
  }

  private queueRetirement(id: string, endpoint: string): string {
    if (!id && !endpoint) return '';
    const key = `${this.cacheKey}:retire:${encodeURIComponent(id || endpoint)}`;
    this.storage.setItem(key, JSON.stringify(id ? { id } : { endpoint }));
    return key;
  }

  private async drainRetirements(): Promise<void> {
    const pending = await this.nativeBridge?.notificationPendingCleanup?.({
      source: this.config.prefix,
    });
    for (const entry of pending || []) {
      if (typeof entry.subscription_id === 'string' && entry.subscription_id)
        this.queueRetirement(entry.subscription_id, '');
    }
    const prefix = `${this.cacheKey}:retire:`;
    const keys = Array.from({ length: this.storage.length }, (_, i) => this.storage.key(i)).filter(
      (key): key is string => Boolean(key?.startsWith(prefix)),
    );
    for (const key of keys) {
      const value = this.storage.getItem(key);
      if (!value) continue;
      // A failed cleanup remains durable, including across logout/offline/reload.
      const body = JSON.parse(value) as { id?: string; endpoint?: string };
      await this.endpoints
        .pushUnsubscribe(body)
        .then(async () => {
          if (body.id)
            await this.nativeBridge?.notificationCleanupAcknowledged?.({
              source: this.config.prefix,
              subscription_id: body.id,
            });
          if (this.storage.getItem(key) === value) this.storage.removeItem(key);
          if (body.id && this.storage.getItem(this.cacheKey) === body.id)
            this.storage.removeItem(this.cacheKey);
        })
        .catch(() => undefined);
    }
  }

  private async deleteServer(id: string, endpoint: string): Promise<boolean> {
    const key = this.queueRetirement(id, endpoint);
    if (!key) return true;
    try {
      await this.endpoints.pushUnsubscribe(id ? { id } : { endpoint });
      if (id)
        await this.nativeBridge?.notificationCleanupAcknowledged?.({
          source: this.config.prefix,
          subscription_id: id,
        });
      this.storage.removeItem(key);
      return true;
    } catch {
      return false;
    }
  }

  installLifecycle(presence?: PresenceSource): () => void {
    if (presence && this.config.notificationVersion === 2) {
      this.presence = presence;
      void this.getOwner().catch(() => undefined);
    }
    const reconcile = () => void this.reconcile();
    const focus = () => {
      const now = Date.now();
      if (now - this.lastFocusCheck < 60_000) return;
      this.lastFocusCheck = now;
      reconcile();
    };
    const storageChanged = (event: StorageEvent) => {
      if (event.storageArea === this.storage && event.key?.startsWith(this.cacheKey)) reconcile();
    };
    addEventListener('storage', storageChanged);
    addEventListener('pageshow', reconcile);
    addEventListener('online', reconcile);
    addEventListener('focus', focus);
    this.cleanup.push(
      () => removeEventListener('storage', storageChanged),
      () => removeEventListener('pageshow', reconcile),
      () => removeEventListener('online', reconcile),
      () => removeEventListener('focus', focus),
    );
    if (this.nativeBridge) {
      addEventListener('termllm-native-push-change', reconcile);
      this.cleanup.push(() => removeEventListener('termllm-native-push-change', reconcile));
    } else {
      navigator.serviceWorker?.addEventListener('controllerchange', reconcile);
      const message = (event: MessageEvent) => this.handleWorkerMessage(event);
      navigator.serviceWorker?.addEventListener('message', message);
      this.cleanup.push(
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
    }
    void this.reconcile();
    return () => this.dispose();
  }

  private handleWorkerMessage(event: MessageEvent): void {
    const data = event.data as { type?: string; tag?: string; url?: string } | null;
    if (data?.type === 'notification-route' && data.url) {
      const target = new URL(data.url, location.href);
      if (target.origin === location.origin && target.pathname.startsWith(`${this.config.prefix}/`))
        location.assign(target.href);
    }
  }

  dispose(): void {
    if (this.disposed) return;
    this.owner?.dispose();
    this.disposed = true;
    this.cleanup.splice(0).forEach((dispose) => dispose());
  }
}
