import { batch, signal, type ReadonlySignal, type Signal } from '@preact/signals';
import type { Session } from '../domain/types';
import type { RuntimeOption } from './store-types';
import { listFrom } from './store-utils';
import type { AppStoreServices } from './app-store-services';
import { MODEL_CATALOG_FRESH_MS, providerFingerprint } from './workspace-cache';

export interface RuntimeStoreOptions {
  activeSession: ReadonlySignal<Session | null>;
  streaming: ReadonlySignal<boolean>;
}

/** Owns provider/model discovery and persisted runtime preferences. */
export class RuntimeStore {
  readonly providers = signal<RuntimeOption[]>([]);
  readonly models = signal<RuntimeOption[]>([]);
  readonly modelsLoadingProvider = signal<string | null>(null);
  readonly modelCatalogs = signal<Record<string, RuntimeOption[]>>({});
  private modelRequest: Promise<void> = Promise.resolve();
  private modelRequestProvider = '';
  private modelRequestPending = false;
  /** When each displayed catalog was fetched from the server (cached or live). */
  private readonly catalogFetchedAt = new Map<string, number>();
  private providersVerified = false;
  readonly selectedProvider: Signal<string>;
  readonly selectedModel: Signal<string>;
  readonly selectedEffort: Signal<string>;
  readonly selectedFast: Signal<boolean>;
  readonly selectedReasoningMode: Signal<string>;
  readonly selectedAgent: Signal<string>;

  private modelAbort: AbortController | null = null;
  private modelEpoch = 0;

  constructor(
    private readonly services: AppStoreServices,
    private readonly options: RuntimeStoreOptions,
  ) {
    const { storage, keys, config } = services;
    this.selectedProvider = signal(storage.getItem(keys.selectedProvider) || '');
    this.selectedModel = signal(storage.getItem(keys.selectedModel) || '');
    this.selectedEffort = signal(storage.getItem(keys.selectedEffort) || '');
    this.selectedFast = signal(storage.getItem(keys.selectedFast) === '1');
    this.selectedReasoningMode = signal(storage.getItem(keys.selectedReasoningMode) || 'standard');
    const agent = storage.getItem(keys.selectedAgent) || '';
    this.selectedAgent = signal(config.agentNames.includes(agent) ? agent : '');
  }

  /**
   * Installs a provider list. Only an authoritative (server) list may prune a
   * saved preference or refresh the persisted copy; a cached list is a label hint.
   */
  applyProviders(data: Record<string, unknown>, authoritative = true): void {
    const values = listFrom(data, 'data', 'providers', 'items')
      .map((entry) => ({
        ...entry,
        id: String(entry.id || entry.name || ''),
        name: String(entry.display_name || entry.name || entry.id || ''),
        models: Array.isArray(entry.models) ? entry.models : [],
      }))
      .filter((entry) => entry.id);
    if (authoritative) {
      const catalogs = { ...this.modelCatalogs.peek() };
      for (const id of Object.keys(catalogs)) {
        const previous = this.providers.peek().find((entry) => entry.id === id);
        const incoming = values.find((entry) => entry.id === id);
        if (providerFingerprint(previous) === providerFingerprint(incoming)) continue;
        // Freshness cannot outlive the provider configuration it describes.
        delete catalogs[id];
        this.catalogFetchedAt.delete(id);
        if (id === this.selectedProvider.peek()) this.models.value = [];
      }
      this.modelCatalogs.value = catalogs;
    }
    this.providers.value = values;
    if (!authoritative) return;
    this.providersVerified = true;
    this.services.workspaceCache.saveProviders(data);
    if (
      this.selectedProvider.value &&
      !values.some((provider) => provider.id === this.selectedProvider.value)
    ) {
      this.selectedProvider.value = '';
      this.services.storage.removeItem(this.services.keys.selectedProvider);
    }
  }

  /**
   * Shows the last-known provider list and the selected provider's model
   * catalog before discovery answers. Never prunes preferences.
   */
  async restoreCachedDiscovery(): Promise<void> {
    const cache = this.services.workspaceCache;
    if (!cache.enabled) return;
    const providers = await cache.readProviders();
    if (!providers) {
      this.services.recordDiscovery({ discoveryCacheMisses: 1 });
      return;
    }
    // A real response may already have landed while IndexedDB was reading.
    if (!this.providersVerified) this.applyProviders(providers.value.payload, false);
    const provider = this.selectedProvider.peek();
    const fingerprint = providerFingerprint(this.providers.peek().find((p) => p.id === provider));
    const models = provider ? await cache.readModels(provider, fingerprint) : null;
    this.services.recordDiscovery({
      discoveryCacheHits: models ? 2 : 1,
      discoveryCacheMisses: provider && !models ? 1 : 0,
    });
    // Authoritative providers may have landed (and changed this provider's
    // configuration) while IndexedDB was reading: never reinstall a catalog
    // for a fingerprint that is no longer current.
    const current = providerFingerprint(this.providers.peek().find((p) => p.id === provider));
    if (!models || current !== fingerprint || this.modelCatalogs.peek()[provider]) return;
    this.catalogFetchedAt.set(provider, models.updated);
    batch(() => {
      this.modelCatalogs.value = { ...this.modelCatalogs.peek(), [provider]: models.value.models };
      if (provider === this.selectedProvider.peek()) this.models.value = models.value.models;
    });
  }

  /**
   * Background startup discovery: skipped while the displayed catalog is
   * younger than the freshness window, otherwise one deduplicated request.
   */
  refreshModelsInBackground(provider = this.selectedProvider.peek()): Promise<void> {
    const fetchedAt = this.catalogFetchedAt.get(provider);
    if (
      this.modelCatalogs.peek()[provider] &&
      fetchedAt !== undefined &&
      Date.now() - fetchedAt < MODEL_CATALOG_FRESH_MS
    )
      return Promise.resolve();
    return this.loadModels(provider);
  }

  /** Concurrent loads of the same provider share one request. */
  loadModels(provider = this.selectedProvider.value): Promise<void> {
    if (this.modelRequestPending && this.modelRequestProvider === provider)
      return this.modelRequest;
    this.modelRequestProvider = provider;
    this.modelRequestPending = true;
    const request = this.fetchModels(provider).finally(() => {
      if (this.modelRequest === request) this.modelRequestPending = false;
    });
    this.modelRequest = request;
    return request;
  }

  whenModelsReady(provider = this.selectedProvider.peek()): Promise<void> {
    if (this.modelCatalogs.peek()[provider]) return Promise.resolve();
    if (this.modelsLoadingProvider.peek() === provider) return this.modelRequest;
    return this.loadModels(provider);
  }

  modelFor(provider: string, id: string): RuntimeOption | undefined {
    return (
      this.modelCatalogs.value[provider]?.find((entry) => entry.id === id) ||
      this.models.value.find(
        (entry) => entry.id === id && (!entry.provider || entry.provider === provider),
      )
    );
  }

  private async fetchModels(provider: string): Promise<void> {
    const epoch = ++this.modelEpoch;
    this.modelAbort?.abort();
    const controller = new AbortController();
    this.modelAbort = controller;
    this.modelsLoadingProvider.value = this.modelCatalogs.peek()[provider] ? null : provider;
    let data: Record<string, unknown>;
    const started = performance.now();
    this.services.recordDiscovery({ discoveryFetches: 1 });
    try {
      data = await this.services.endpoints.models(provider, controller.signal);
    } catch (error) {
      if (controller.signal.aborted || epoch !== this.modelEpoch) return;
      this.modelsLoadingProvider.value = null;
      throw error;
    }
    this.services.recordDiscovery({}, performance.now() - started);
    if (controller.signal.aborted || epoch !== this.modelEpoch) return;
    const models = listFrom(data, 'data', 'models', 'items')
      .map((entry) => ({
        ...entry,
        id: String(entry.id || entry.name || ''),
        name: String(entry.display_name || entry.name || entry.id || ''),
        provider: String(entry.provider || provider || ''),
        efforts: Array.isArray(entry.reasoning_efforts)
          ? entry.reasoning_efforts.map(String)
          : Array.isArray(entry.efforts)
            ? entry.efforts.map(String)
            : undefined,
        default_effort: String(entry.default_reasoning_effort || entry.default_effort || ''),
      }))
      .filter((entry) => entry.id) as RuntimeOption[];
    batch(() => {
      this.modelCatalogs.value = { ...this.modelCatalogs.peek(), [provider]: models };
      // Settings previews intentionally load a non-selected provider into this
      // list. Runtime labels must use the provider-scoped modelFor lookup.
      this.models.value = models;
      this.modelsLoadingProvider.value = null;
    });
    this.catalogFetchedAt.set(provider, Date.now());
    // Persist against the verified provider entry only; a catalog fetched
    // while providers are still last-known would carry an unverified fingerprint.
    if (this.providersVerified)
      this.services.workspaceCache.saveModels(
        provider,
        providerFingerprint(this.providers.peek().find((entry) => entry.id === provider)),
        models,
      );
    if (provider !== this.selectedProvider.peek()) return;
    if (
      this.selectedModel.value &&
      !this.models.value.some((model) => model.id === this.selectedModel.value)
    ) {
      const matching = this.models.value.find(
        (model) =>
          model.id ===
          this.selectedModel.value.replace(/[-_](?:none|minimal|low|medium|high|xhigh|max)$/i, ''),
      );
      if (matching) this.setPreference('model', matching.id, false);
    }
  }

  setFast(value: boolean): void {
    this.selectedFast.value = value;
    if (value) this.services.storage.setItem(this.services.keys.selectedFast, '1');
    else this.services.storage.removeItem(this.services.keys.selectedFast);
  }

  setPreference(
    name: 'provider' | 'model' | 'effort' | 'reasoning' | 'agent',
    value: string,
    commit = true,
  ): void {
    const { keys, storage } = this.services;
    const map = {
      provider: [this.selectedProvider, keys.selectedProvider],
      model: [this.selectedModel, keys.selectedModel],
      effort: [this.selectedEffort, keys.selectedEffort],
      reasoning: [this.selectedReasoningMode, keys.selectedReasoningMode],
      agent: [this.selectedAgent, keys.selectedAgent],
    } as const;
    const [target, key] = map[name];
    const changed = target.peek() !== value;
    target.value = value;
    if (value) storage.setItem(key, value);
    else storage.removeItem(key);
    if (name === 'provider' && changed) {
      this.selectedModel.value = '';
      storage.removeItem(keys.selectedModel);
      const provider = this.providers.peek().find((entry) => entry.id === value);
      const fallback = Array.isArray(provider?.models) ? provider.models : [];
      this.models.value =
        this.modelCatalogs.peek()[value] ||
        fallback
          .map((entry) => {
            const source: Record<string, unknown> =
              entry && typeof entry === 'object'
                ? (entry as Record<string, unknown>)
                : { id: entry };
            const id = String(source.id || source.name || '');
            return {
              ...source,
              id,
              name: String(source.display_name || source.name || id),
              provider: value,
            } as RuntimeOption;
          })
          .filter((entry) => entry.id);
      void this.loadModels().catch((error) => this.services.toast(error, 'error'));
    }
    const activeSession = this.options.activeSession.value;
    if (commit && name === 'effort' && this.options.streaming.value && activeSession) {
      void this.services.endpoints
        .runtime(activeSession.id, 'effort', {
          model: this.selectedModel.value || activeSession.activeModel,
          provider: this.selectedProvider.value || activeSession.activeProvider,
          reasoning_effort: value,
        })
        .catch((error) => this.services.toast(error, 'error'));
    }
  }

  dispose(): void {
    this.modelAbort?.abort();
    this.modelAbort = null;
    this.modelEpoch += 1;
  }
}
