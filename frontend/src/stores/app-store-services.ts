import { signal, type Signal } from '@preact/signals';
import type { AppConfig } from '../app/config';
import { APIClient } from '../api/client';
import { endpoints, type Endpoints } from '../api/endpoints';
import { errorMessage } from '../domain/text';
import { hardRefreshAssets, syncTokenCookie } from '../platform/browser';
import { NotificationController, type NotificationState } from '../platform/notifications';
import { migrateScopedStorage, type StorageKeys } from '../platform/storage';
import { IndexedDBBackend, PersistentCache } from '../platform/persistent-cache';
import type { Toast } from './store-types';
import { uuid } from './store-utils';
import { WorkspaceCache } from './workspace-cache';

/**
 * Startup timings in milliseconds since navigation start, plus discovery
 * cache counters. Never contains transcript content or credentials.
 */
export interface StartupMetrics {
  /** Shell showed a usable workspace (cached or fresh). */
  firstUsefulPaint?: number;
  /** Selected conversation (or new chat) confirmed by the server. */
  authoritative?: number;
  /** Startup finished; lifecycle, events and sending are enabled. */
  actionsReady?: number;
  /** The first useful paint came from the persistent workspace cache. */
  restoredFromCache: boolean;
  discoveryCacheHits: number;
  discoveryCacheMisses: number;
  discoveryFetches: number;
  /** Duration of the most recent model discovery request. */
  lastDiscoveryMs?: number;
}

export interface StoreDiagnostics {
  staleStatusResults: number;
  staleStreamCallbacks: number;
  supervisorRetries: number;
  supervisorRecoveries: number;
  streamWatchdogTimeouts: number;
  queueValidationFailures: number;
  interactionReconciliations: number;
  storageFailures: number;
  serverEventSSEConnections: number;
  serverEventPollFallbacks: number;
  serverEventReconnects: number;
  serverEventCursorResets: number;
  serverEventSequenceGaps: number;
  serverEventMalformed: number;
  serverEventNoopBatches: number;
  serverEventSSEJams: number;
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
  readonly eventFeedHealthy = signal(false);
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
    serverEventSSEConnections: 0,
    serverEventPollFallbacks: 0,
    serverEventReconnects: 0,
    serverEventCursorResets: 0,
    serverEventSequenceGaps: 0,
    serverEventMalformed: 0,
    serverEventNoopBatches: 0,
    serverEventSSEJams: 0,
  });
  readonly notifications = signal<NotificationState>({
    status: 'unsupported',
    busy: false,
    detail: 'Checking notification support…',
    verified: false,
  });
  readonly startupMetrics = signal<StartupMetrics>({
    restoredFromCache: false,
    discoveryCacheHits: 0,
    discoveryCacheMisses: 0,
    discoveryFetches: 0,
  });
  readonly api: APIClient;
  readonly endpoints: Endpoints;
  readonly notificationController: NotificationController;
  readonly workspaceCache: WorkspaceCache;

  /**
   * Counts 401/403 responses from any request, including detached background
   * work, so startup cannot declare success over a lost auth failure.
   */
  authFailures = 0;

  private readonly ownedTimers = new Set<number>();
  private disposed = false;

  constructor(
    readonly config: AppConfig,
    readonly storage: Storage,
    persistentCache?: PersistentCache,
  ) {
    this.workspaceCache = new WorkspaceCache(
      config,
      persistentCache ||
        new PersistentCache(new IndexedDBBackend(), () => this.bumpDiagnostic('storageFailures')),
    );
    this.keys = migrateScopedStorage(storage, config.hub);
    if (config.passkeyAuth) storage.removeItem(this.keys.token);
    this.token = signal(config.passkeyAuth ? '' : storage.getItem(this.keys.token) || '');
    syncTokenCookie(config.prefix, this.token.value);
    this.api = new APIClient(config, {
      getToken: () => this.token.value,
      onAuthRequired: () => {
        this.authFailures += 1;
        this.authRequired.value = true;
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

  /** Records a startup milestone once, as a Performance mark and a metric. */
  markStartup(name: 'firstUsefulPaint' | 'authoritative' | 'actionsReady'): void {
    if (this.startupMetrics.peek()[name] !== undefined) return;
    const at = Math.round(typeof performance === 'undefined' ? Date.now() : performance.now());
    try {
      performance.mark(`term-llm:${name}`);
    } catch {
      /* Performance marks are optional instrumentation. */
    }
    this.startupMetrics.value = { ...this.startupMetrics.peek(), [name]: at };
  }

  recordDiscovery(
    patch: Partial<
      Pick<StartupMetrics, 'discoveryCacheHits' | 'discoveryCacheMisses' | 'discoveryFetches'>
    >,
    durationMs?: number,
  ): void {
    const current = this.startupMetrics.peek();
    this.startupMetrics.value = {
      ...current,
      discoveryCacheHits: current.discoveryCacheHits + (patch.discoveryCacheHits || 0),
      discoveryCacheMisses: current.discoveryCacheMisses + (patch.discoveryCacheMisses || 0),
      discoveryFetches: current.discoveryFetches + (patch.discoveryFetches || 0),
      ...(durationMs === undefined ? {} : { lastDiscoveryMs: Math.round(durationMs) }),
    };
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
    const previous = this.token.peek();
    this.token.value = value.trim();
    // Defense in depth for every credential removal/replacement path (connect()
    // additionally awaits its purge before switching): never carry the previous
    // credential's cached workspace across a change.
    if (previous && previous !== this.token.value) void this.workspaceCache.purge();
    if (this.token.value) this.storage.setItem(this.keys.token, this.token.value);
    else this.storage.removeItem(this.keys.token);
    syncTokenCookie(this.config.prefix, this.token.value);
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.workspaceCache.dispose();
    this.ownedTimers.forEach((timer) => window.clearTimeout(timer));
    this.ownedTimers.clear();
    this.notificationController.dispose();
  }
}
