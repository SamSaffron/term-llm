import { signal, type Signal } from '@preact/signals';
import type { AppConfig } from '../app/config';
import { APIClient } from '../api/client';
import { endpoints, type Endpoints } from '../api/endpoints';
import { errorMessage } from '../domain/text';
import { hardRefreshAssets, syncTokenCookie } from '../platform/browser';
import { NotificationController, type NotificationState } from '../platform/notifications';
import { migrateScopedStorage, type StorageKeys } from '../platform/storage';
import type { Modal, Toast } from './store-types';
import { uuid } from './store-utils';

export interface StoreDiagnostics {
  staleStatusResults: number;
  staleStreamCallbacks: number;
  supervisorRetries: number;
  supervisorRecoveries: number;
  streamWatchdogTimeouts: number;
  queueValidationFailures: number;
  interactionReconciliations: number;
  storageFailures: number;
}

/**
 * Infrastructure shared by feature stores. It deliberately owns no session,
 * composer, response, or panel domain state.
 */
export class AppStoreServices {
  readonly keys: StorageKeys;
  readonly token: Signal<string>;
  readonly authRequired = signal(false);
  readonly networkState = signal<'unknown' | 'online' | 'offline' | 'retrying'>('unknown');
  readonly connected = signal(false);
  readonly toasts = signal<Toast[]>([]);
  readonly diagnostics = signal<StoreDiagnostics>({
    staleStatusResults: 0,
    staleStreamCallbacks: 0,
    supervisorRetries: 0,
    supervisorRecoveries: 0,
    streamWatchdogTimeouts: 0,
    queueValidationFailures: 0,
    interactionReconciliations: 0,
    storageFailures: 0,
  });
  readonly notifications = signal<NotificationState>({
    status: 'unsupported',
    busy: false,
    detail: 'Checking notification support…',
    verified: false,
  });
  readonly api: APIClient;
  readonly endpoints: Endpoints;
  readonly notificationController: NotificationController;

  private readonly ownedTimers = new Set<number>();
  private disposed = false;

  constructor(
    readonly config: AppConfig,
    readonly storage: Storage,
    modal: Signal<Modal>,
  ) {
    this.keys = migrateScopedStorage(storage, config.hub);
    this.token = signal(storage.getItem(this.keys.token) || '');
    syncTokenCookie(config.prefix, this.token.value);
    this.api = new APIClient(config, {
      getToken: () => this.token.value,
      onAuthRequired: () => {
        this.authRequired.value = true;
        modal.value = 'settings';
      },
      onNetworkState: (state) => {
        this.networkState.value = state;
        this.connected.value = state === 'online';
      },
      onVersionMismatch: () => {
        void this.hardRefresh();
      },
    });
    this.endpoints = endpoints(this.api);
    this.notificationController = new NotificationController(
      config,
      this.endpoints,
      storage,
      this.keys.notificationSubscriptionID,
    );
    this.notificationController.setListener((state) => {
      this.notifications.value = state;
    });
  }

  get isDisposed(): boolean {
    return this.disposed;
  }

  schedule(callback: () => void, delay: number): number {
    const timer = window.setTimeout(() => {
      this.ownedTimers.delete(timer);
      if (!this.disposed) callback();
    }, delay);
    this.ownedTimers.add(timer);
    return timer;
  }

  bumpDiagnostic(key: keyof StoreDiagnostics): void {
    this.diagnostics.value = {
      ...this.diagnostics.peek(),
      [key]: this.diagnostics.peek()[key] + 1,
    };
  }

  toast(value: unknown, kind: Toast['kind'] = 'info'): void {
    const message = errorMessage(value);
    const toast = { id: uuid(), message, kind };
    this.toasts.value = [...this.toasts.value, toast];
    this.schedule(() => this.dismissToast(toast.id), 4_000);
  }

  dismissToast(id: string): void {
    const toast = this.toasts.peek().find((entry) => entry.id === id);
    if (!toast || toast.leaving) return;
    this.toasts.value = this.toasts.value.map((entry) =>
      entry.id === id ? { ...entry, leaving: true } : entry,
    );
    this.schedule(() => {
      this.toasts.value = this.toasts.value.filter((entry) => entry.id !== id);
    }, 160);
  }

  async hardRefresh(): Promise<void> {
    await hardRefreshAssets(this.config);
  }

  setToken(value: string): void {
    this.token.value = value.trim();
    if (this.token.value) this.storage.setItem(this.keys.token, this.token.value);
    else this.storage.removeItem(this.keys.token);
    syncTokenCookie(this.config.prefix, this.token.value);
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.ownedTimers.forEach((timer) => window.clearTimeout(timer));
    this.ownedTimers.clear();
    this.notificationController.dispose();
  }
}
