import type { AppConfig } from '../app/config';
import type { Endpoints } from '../api/endpoints';
import type {
  NotificationController as Controller,
  NotificationState,
} from './notification-controller';
import type { PresenceSource } from './presence';

export type { NotificationState, NotificationStatus } from './notification-controller';
export { base64URLToBytes } from './push-platform';

/** Notification enrollment/runtime is optional; keep it outside the eager chat bundle. */
export class NotificationController {
  private current: NotificationState = {
    status: 'unsupported',
    busy: false,
    detail: 'Checking notification support…',
    verified: false,
  };
  private readonly nativePush = window.__termLLMNativePush;
  private listener: (state: NotificationState) => void = () => {};
  private controller: Controller | null = null;
  private pending: Promise<Controller> | null = null;
  private disposed = false;

  constructor(
    private readonly config: AppConfig,
    private readonly endpoints: Endpoints,
    private readonly storage: Storage,
    private readonly cacheKey: string,
  ) {}

  private load(): Promise<Controller> {
    this.pending ||= import('./notification-controller').then(({ NotificationController }) => {
      const controller = new NotificationController(
        this.config,
        this.endpoints,
        this.storage,
        this.cacheKey,
        this.nativePush,
      );
      this.controller = controller;
      controller.setListener((state) => {
        if (this.disposed) return;
        this.current = state;
        this.listener(state);
      });
      if (this.disposed) controller.dispose();
      return controller;
    });
    return this.pending;
  }

  get state(): NotificationState {
    return this.current;
  }

  setListener(listener: (state: NotificationState) => void): void {
    this.listener = listener;
    listener(this.current);
  }

  async reconcile(options: { repair?: boolean } = {}): Promise<NotificationState> {
    return (await this.load()).reconcile(options);
  }

  async enable(): Promise<NotificationState> {
    return (await this.load()).enable();
  }

  async disable(): Promise<NotificationState> {
    // Persist intent/cleanup synchronously, before even loading optional code.
    this.storage.setItem(`${this.cacheKey}:off`, '1');
    const id = this.storage.getItem(this.cacheKey) || this.current.subscriptionId;
    if (id)
      this.storage.setItem(
        `${this.cacheKey}:retire:${encodeURIComponent(id)}`,
        JSON.stringify({ id }),
      );
    return (await this.load()).disable();
  }

  async originHeaders(): Promise<Record<string, string>> {
    if (this.config.notificationVersion !== 2) return {};
    return this.load()
      .then((controller) => controller.originHeaders())
      .catch(() => ({}));
  }

  acknowledgeRead(sessionId: string, state: Record<string, unknown>): void {
    if (!Number.isSafeInteger(state.seen_through_seq) || Number(state.seen_through_seq) <= 0)
      return;
    void this.load()
      .then((controller) => controller.acknowledgeRead(sessionId, state))
      .catch(() => undefined);
  }

  installLifecycle(presence?: PresenceSource): () => void {
    void this.load()
      .then((controller) => {
        if (!this.disposed) controller.installLifecycle(presence);
      })
      .catch(() => undefined);
    return () => this.dispose();
  }

  dispose(): void {
    this.disposed = true;
    this.controller?.dispose();
  }
}
