import { signal, type ReadonlySignal, type Signal } from '@preact/signals';
import type { Session } from '../domain/types';
import type { Modal, RuntimeOption } from './store-types';
import { listFrom } from './store-utils';
import type { AppStoreServices } from './app-store-services';

export interface RuntimeStoreOptions {
  activeSession: ReadonlySignal<Session | null>;
  streaming: ReadonlySignal<boolean>;
  modal: Signal<Modal>;
  bootstrap: () => Promise<void>;
}

/** Owns provider/model discovery and persisted runtime preferences. */
export class RuntimeStore {
  readonly providers = signal<RuntimeOption[]>([]);
  readonly models = signal<RuntimeOption[]>([]);
  readonly selectedProvider: Signal<string>;
  readonly selectedModel: Signal<string>;
  readonly selectedEffort: Signal<string>;
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
    this.selectedReasoningMode = signal(storage.getItem(keys.selectedReasoningMode) || 'standard');
    const agent = storage.getItem(keys.selectedAgent) || '';
    this.selectedAgent = signal(config.agentNames.includes(agent) ? agent : '');
  }

  applyProviders(data: Record<string, unknown>): void {
    const values = listFrom(data, 'data', 'providers', 'items')
      .map((entry) => ({
        ...entry,
        id: String(entry.id || entry.name || ''),
        name: String(entry.display_name || entry.name || entry.id || ''),
        models: Array.isArray(entry.models) ? entry.models : [],
      }))
      .filter((entry) => entry.id);
    this.providers.value = values;
    if (
      this.selectedProvider.value &&
      !values.some((provider) => provider.id === this.selectedProvider.value)
    ) {
      this.selectedProvider.value = '';
      this.services.storage.removeItem(this.services.keys.selectedProvider);
    }
  }

  async loadModels(provider = this.selectedProvider.value): Promise<void> {
    const epoch = ++this.modelEpoch;
    this.modelAbort?.abort();
    const controller = new AbortController();
    this.modelAbort = controller;
    let data: Record<string, unknown>;
    try {
      data = await this.services.endpoints.models(provider, controller.signal);
    } catch (error) {
      if (controller.signal.aborted || epoch !== this.modelEpoch) return;
      throw error;
    }
    if (controller.signal.aborted || epoch !== this.modelEpoch) return;
    this.models.value = listFrom(data, 'data', 'models', 'items')
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
      this.models.value = fallback
        .map((entry) => {
          const source: Record<string, unknown> =
            entry && typeof entry === 'object' ? (entry as Record<string, unknown>) : { id: entry };
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

  saveSettings(token: string): void {
    this.services.setToken(token);
    this.services.authRequired.value = false;
    this.options.modal.value = '';
    void this.options.bootstrap();
  }

  dispose(): void {
    this.modelAbort?.abort();
    this.modelAbort = null;
    this.modelEpoch += 1;
  }
}
