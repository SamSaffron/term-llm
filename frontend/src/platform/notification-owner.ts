import type { Endpoints } from '../api/endpoints';
import type { TermLLMNativePush, NotificationRead } from './push-platform';
import { ATTENTION_POLICY, type PresenceSource } from './presence';

export interface NotificationOrigin {
  version: 2;
  window_id: string;
  document_id: string;
  subscription_id: string;
}

/** One document's authenticated binding and bounded attendance lease, never read evidence. */
export class NotificationOwner {
  private windowId = '';
  private documentId: string = crypto.randomUUID();
  private previousDocumentId = '';
  private sequence = 0;
  private presence: PresenceSource | null = null;
  private timer: ReturnType<typeof setInterval> | undefined;
  private unsubscribe: (() => void) | undefined;
  private active = false;
  private publishedActive: boolean | undefined;
  private stopped = false;
  private publication: Promise<boolean> | null = null;

  constructor(
    private readonly prefix: string,
    private readonly endpoints: Endpoints,
    private readonly subscription: () => string,
    private readonly bridge?: TermLLMNativePush,
  ) {}

  private documentKey(): string {
    return `term_llm_notification_document:${this.prefix}:${this.windowId}`;
  }

  private rememberPredecessor(workerPrevious = ''): void {
    // Last HTTP-ACKed incarnation survives a crash between SW bind and HTTP ACK.
    // A clone gets a new window key before this is consulted.
    const acknowledged = sessionStorage.getItem(this.documentKey()) || workerPrevious;
    if (acknowledged && acknowledged !== this.documentId) this.previousDocumentId = acknowledged;
  }

  private async bind(): Promise<boolean> {
    try {
      if (this.bridge) {
        if (!this.bridge.notificationPresence || !this.bridge.notificationRead) return false;
        const capability = await this.bridge.notificationCapabilities?.();
        if (capability?.version !== 2 || !capability.window_id) return false;
        this.windowId = capability.window_id;
        if (capability.document_id) this.documentId = capability.document_id;
        this.rememberPredecessor();
        return true;
      }
      const key = `term_llm_notification_window:${this.prefix}`;
      this.windowId = sessionStorage.getItem(key) || crypto.randomUUID();
      const registration = await navigator.serviceWorker.getRegistration(`${this.prefix}/`);
      const worker = registration?.active;
      if (!worker) return false;
      for (let attempt = 0; attempt < 2; attempt++) {
        const result = await new Promise<{ status?: string; previous_document_id?: string }>(
          (resolve) => {
            const channel = new MessageChannel();
            const timer = setTimeout(() => finish({ status: 'unavailable' }), 1_500);
            const finish = (value: { status?: string; previous_document_id?: string }) => {
              clearTimeout(timer);
              channel.port1.close();
              resolve(value);
            };
            channel.port1.onmessage = (event) => finish(event.data || {});
            worker.postMessage(
              {
                type: 'notification-bind',
                version: 2,
                window_id: this.windowId,
                document_id: this.documentId,
              },
              [channel.port2],
            );
          },
        );
        if (result.status === 'bound') {
          this.rememberPredecessor(result.previous_document_id);
          sessionStorage.setItem(key, this.windowId);
          return true;
        }
        if (result.status !== 'collision') return false;
        // Duplicate-tab sessionStorage is a copy, not ownership of the original window.
        this.windowId = crypto.randomUUID();
      }
    } catch {
      // Private storage / old workers / old native shells fail closed for automatic alerts.
    }
    return false;
  }

  start(presence: PresenceSource): void {
    if (this.presence) return;
    this.presence = presence;
    this.unsubscribe = presence.subscribe(() => {
      const active = this.attended();
      if (active !== this.publishedActive) void this.publish();
    });
    this.timer = setInterval(() => void this.publish(), 15_000);
    addEventListener('pageshow', this.pageshow);
    addEventListener('pagehide', this.pagehide);
    void this.publish();
  }

  private attended(): boolean {
    const last = this.presence?.lastPresenceAt() || 0;
    return (
      !this.stopped &&
      document.visibilityState === 'visible' &&
      Boolean(this.presence?.focused()) &&
      last > 0 &&
      Date.now() - last >= 0 &&
      Date.now() - last < ATTENTION_POLICY.presenceLeadMs
    );
  }

  private readonly pageshow = () => {
    this.stopped = false;
    void this.publish();
  };

  private readonly pagehide = () => {
    this.stopped = true;
    void this.publish();
  };

  /** Each update is serialized; old HTTP results cannot win over a local inactive update. */
  publish(): Promise<boolean> {
    if (this.publication) return this.publication;
    const next = Promise.resolve().then(async () => {
      if (!this.subscription()) return false;
      if (!(await this.bind())) return false;
      this.active = this.attended();
      this.publishedActive = this.active;
      const now = Date.now();
      const expires = this.active
        ? Math.min(now + 30_000, this.presence!.lastPresenceAt() + ATTENTION_POLICY.presenceLeadMs)
        : now + 30_000;
      {
        const key = `term_llm_notification_sequence:${this.prefix}:${this.windowId}`;
        try {
          this.sequence = Math.max(this.sequence, Number(sessionStorage.getItem(key)) || 0) + 1;
          sessionStorage.setItem(key, String(this.sequence));
        } catch {
          return false;
        }
      }
      const payload = {
        version: 2 as const,
        window_id: this.windowId,
        document_id: this.documentId,
        sequence: this.sequence,
        active: this.active,
        expires_at: new Date(expires).toISOString(),
      };
      try {
        await this.bridge?.notificationPresence?.({ ...payload, source: this.prefix });
        await this.endpoints.pushPresence({
          ...payload,
          subscription_id: this.subscription(),
          source: this.prefix,
          ...(this.previousDocumentId ? { previous_document_id: this.previousDocumentId } : {}),
        });
        sessionStorage.setItem(this.documentKey(), this.documentId);
        this.previousDocumentId = '';
        return true;
      } catch {
        return false;
      }
    });
    this.publication = next.finally(() => {
      this.publication = null;
      if (this.presence && this.subscription() && this.attended() !== this.active)
        void this.publish();
    });
    return this.publication;
  }

  async origin(): Promise<NotificationOrigin | null> {
    const subscriptionId = this.subscription();
    if (
      !subscriptionId ||
      this.stopped ||
      !(await this.publish()) ||
      this.subscription() !== subscriptionId
    )
      return null;
    return {
      version: 2,
      window_id: this.windowId,
      document_id: this.documentId,
      subscription_id: subscriptionId,
    };
  }

  read(payload: NotificationRead): void {
    if (this.bridge) {
      void this.bridge.notificationRead?.(payload).catch(() => undefined);
    } else {
      void navigator.serviceWorker
        ?.getRegistration(`${this.prefix}/`)
        .then((registration) => {
          registration?.active?.postMessage({ type: 'notification-read', ...payload });
        })
        .catch(() => undefined);
    }
  }

  dispose(): void {
    this.pagehide();
    clearInterval(this.timer);
    this.unsubscribe?.();
    removeEventListener('pagehide', this.pagehide);
    removeEventListener('pageshow', this.pageshow);
  }
}
