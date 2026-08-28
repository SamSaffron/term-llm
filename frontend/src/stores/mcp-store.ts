import { signal, type ReadonlySignal } from '@preact/signals';
import { errorMessage } from '../domain/text';
import type { MCPServer, Session } from '../domain/types';
import type { AppStoreServices } from './app-store-services';
import { normalizeMCPState } from './store-utils';

export interface MCPState {
  servers: MCPServer[];
  enabled: string[];
  loading: boolean;
  pending: string;
  error: string;
}

export interface MCPStoreOptions {
  activeSession: ReadonlySignal<Session | null>;
  patchSession: (id: string, patch: Partial<Session>) => void;
}

/** Owns MCP server loading and optimistic enablement. */
export class MCPStore {
  readonly state = signal<MCPState>({
    servers: [],
    enabled: [],
    loading: false,
    pending: '',
    error: '',
  });

  constructor(
    private readonly services: AppStoreServices,
    private readonly options: MCPStoreOptions,
  ) {}

  async load(): Promise<void> {
    const session = this.options.activeSession.value;
    if (!session) return;
    this.state.value = { ...this.state.value, loading: true, error: '' };
    try {
      const data = await this.services.endpoints.getMCP(session.id);
      const state = normalizeMCPState(data);
      this.state.value = { ...state, loading: false, pending: '', error: '' };
      this.options.patchSession(session.id, { mcpEnabled: state.enabled });
    } catch (error) {
      this.state.value = {
        ...this.state.value,
        loading: false,
        error: errorMessage(error),
      };
    }
  }

  async toggle(name: string): Promise<void> {
    const session = this.options.activeSession.value;
    if (!session || this.state.value.pending) return;
    const previous = this.state.value.enabled;
    const enabled = previous.includes(name)
      ? previous.filter((entry) => entry !== name)
      : [...previous, name];
    this.state.value = { ...this.state.value, enabled, pending: name, error: '' };
    try {
      const data = await this.services.endpoints.setMCP(session.id, enabled);
      const state = normalizeMCPState(data);
      this.state.value = { ...state, loading: false, pending: '', error: '' };
      this.options.patchSession(session.id, { mcpEnabled: state.enabled });
    } catch (error) {
      this.state.value = {
        ...this.state.value,
        enabled: previous,
        pending: '',
        error: errorMessage(error),
      };
    }
  }
}
