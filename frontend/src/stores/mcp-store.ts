import { signal, type ReadonlySignal } from '@preact/signals';
import { APIError } from '../api/client';
import { errorMessage } from '../domain/text';
import type { MCPOAuthFlow, MCPServer, Session } from '../domain/types';
import type { AppStoreServices } from './app-store-services';
import { normalizeMCPState } from './store-utils';

export interface MCPOAuthUIState {
  flowId: string;
  authorizationURL: string;
  state: MCPOAuthFlow['state'];
  error: string;
  popupBlocked: boolean;
}

export interface MCPState {
  ownerId: string;
  servers: MCPServer[];
  enabled: string[];
  loading: boolean;
  pending: string;
  error: string;
  oauth: Record<string, MCPOAuthUIState>;
}

export interface MCPStoreOptions {
  activeSession: ReadonlySignal<Session | null>;
  draftSessionId: () => string;
  patchSession: (id: string, patch: Partial<Session>) => void;
}

/** Owns MCP server loading, optimistic enablement, and ephemeral OAuth UI flows. */
export class MCPStore {
  readonly state = signal<MCPState>({
    ownerId: '',
    servers: [],
    enabled: [],
    loading: false,
    pending: '',
    error: '',
    oauth: {},
  });

  private readonly pollTimers = new Map<string, ReturnType<typeof setTimeout>>();
  private readonly popups = new Map<string, Window>();
  private readonly flowSessions = new Map<string, string>();
  private readonly messageListeners = new Map<string, (event: MessageEvent) => void>();

  constructor(
    private readonly services: AppStoreServices,
    private readonly options: MCPStoreOptions,
  ) {}

  private sessionId(): string {
    return this.options.activeSession.value?.id || this.options.draftSessionId();
  }

  enabledFor(ownerId: string): string[] {
    return this.state.value.ownerId === ownerId ? [...this.state.value.enabled] : [];
  }

  rekey(oldId: string, newId: string): void {
    if (!oldId || !newId || this.state.value.ownerId !== oldId) return;
    this.state.value = { ...this.state.value, ownerId: newId };
    this.options.patchSession(newId, { mcpEnabled: this.state.value.enabled });
    for (const [name, sessionId] of this.flowSessions)
      if (sessionId === oldId) this.flowSessions.set(name, newId);
  }

  async load(): Promise<void> {
    const session = this.options.activeSession.value;
    const sessionId = this.sessionId();
    if (!sessionId) return;
    this.pruneOAuth(sessionId);
    this.state.value = { ...this.state.value, ownerId: sessionId, loading: true, error: '' };
    try {
      const data = await this.services.endpoints.getMCP(sessionId);
      if (this.sessionId() !== sessionId) return;
      const state = normalizeMCPState(data);
      this.state.value = {
        ownerId: sessionId,
        ...state,
        loading: false,
        pending: '',
        error: '',
        oauth: this.state.value.oauth,
      };
      if (session) this.options.patchSession(session.id, { mcpEnabled: state.enabled });
    } catch (error) {
      if (this.sessionId() !== sessionId) return;
      this.state.value = {
        ...this.state.value,
        loading: false,
        error: errorMessage(error),
      };
    }
  }

  async toggle(name: string): Promise<void> {
    const session = this.options.activeSession.value;
    const sessionId = this.sessionId();
    if (!sessionId || this.state.value.pending) return;
    const previous = this.state.value.ownerId === sessionId ? this.state.value.enabled : [];
    const previousServers = this.state.value.ownerId === sessionId ? this.state.value.servers : [];
    const enabling = !previous.includes(name);
    const enabled = enabling ? [...previous, name] : previous.filter((entry) => entry !== name);
    const servers = previousServers.map((server) =>
      server.name === name
        ? {
            ...server,
            enabled: enabling,
            status: enabling ? 'starting' : 'stopped',
            error: '',
          }
        : server,
    );
    this.state.value = {
      ...this.state.value,
      ownerId: sessionId,
      servers,
      enabled,
      pending: name,
      error: '',
    };
    try {
      const data = await this.services.endpoints.setMCP(sessionId, enabled);
      if (this.sessionId() !== sessionId) return;
      const state = normalizeMCPState(data);
      this.state.value = {
        ownerId: sessionId,
        ...state,
        loading: false,
        pending: '',
        error: '',
        oauth: this.state.value.oauth,
      };
      if (session) this.options.patchSession(session.id, { mcpEnabled: state.enabled });
    } catch (error) {
      if (this.sessionId() !== sessionId) return;
      this.state.value = {
        ...this.state.value,
        ownerId: sessionId,
        servers: previousServers,
        enabled: previous,
        pending: '',
        error: errorMessage(error),
      };
    }
  }

  async startOAuth(name: string, force = false): Promise<void> {
    const sessionId = this.sessionId();
    const existing = this.state.value.oauth[name];
    if (!sessionId || existing?.state === 'starting' || existing?.state === 'pending') return;
    if (existing) this.removeOAuth(name);

    // Open synchronously from the click gesture. Navigation happens after the
    // authenticated start response supplies the authorization URL.
    const popup = window.open('', '_blank', 'popup=yes,width=560,height=720');
    this.flowSessions.set(name, sessionId);
    this.setOAuth(name, {
      flowId: '',
      authorizationURL: '',
      state: 'starting',
      error: '',
      popupBlocked: !popup,
    });
    try {
      const flow = await this.services.endpoints.startMCPOAuth(sessionId, name, force);
      this.setOAuth(name, {
        flowId: flow.flow_id,
        authorizationURL: flow.authorization_url || '',
        state: flow.state,
        error: flow.error || '',
        popupBlocked: !popup,
      });
      if (popup && flow.authorization_url) {
        this.popups.set(name, popup);
        popup.location.assign(flow.authorization_url);
      }
      const onMessage = (event: MessageEvent) => {
        const data = event.data as { type?: string; flow_id?: string } | null;
        if (
          event.origin === window.location.origin &&
          data?.type === 'term-llm-mcp-oauth' &&
          data.flow_id === flow.flow_id
        ) {
          window.removeEventListener('message', onMessage);
          this.messageListeners.delete(name);
          void this.pollOAuth(name, flow.flow_id, true);
        }
      };
      this.clearMessageListener(name);
      this.messageListeners.set(name, onMessage);
      window.addEventListener('message', onMessage);
      this.schedulePoll(name, flow.flow_id);
    } catch (error) {
      popup?.close();
      this.setOAuth(name, {
        flowId: '',
        authorizationURL: '',
        state: 'failed',
        error: errorMessage(error),
        popupBlocked: !popup,
      });
    }
  }

  async cancelOAuth(name: string): Promise<void> {
    const sessionId = this.flowSessions.get(name) || this.sessionId();
    const flow = this.state.value.oauth[name];
    if (!sessionId || !flow?.flowId) return;
    this.clearPoll(name);
    try {
      await this.services.endpoints.cancelMCPOAuth(sessionId, name, flow.flowId);
      this.popups.get(name)?.close();
      this.popups.delete(name);
      this.removeOAuth(name);
      await this.load();
    } catch (error) {
      this.setOAuth(name, { ...flow, state: 'failed', error: errorMessage(error) });
    }
  }

  async logoutOAuth(name: string): Promise<void> {
    const sessionId = this.sessionId();
    if (!sessionId) return;
    try {
      await this.services.endpoints.logoutMCPOAuth(sessionId, name);
      this.removeOAuth(name);
      await this.load();
    } catch (error) {
      this.state.value = { ...this.state.value, error: errorMessage(error) };
    }
  }

  async copyOAuthLink(name: string): Promise<void> {
    const url = this.state.value.oauth[name]?.authorizationURL;
    if (url) await navigator.clipboard.writeText(url);
  }

  private schedulePoll(name: string, flowId: string): void {
    this.clearPoll(name);
    this.pollTimers.set(
      name,
      setTimeout(() => void this.pollOAuth(name, flowId), 1000),
    );
  }

  private async pollOAuth(name: string, flowId: string, immediate = false): Promise<void> {
    if (!immediate && this.state.value.oauth[name]?.flowId !== flowId) return;
    try {
      const flow = await this.services.endpoints.getMCPOAuthFlow(flowId);
      const current = this.state.value.oauth[name];
      if (!current || current.flowId !== flowId) return;
      if (flow.state === 'starting' || flow.state === 'pending') {
        this.setOAuth(name, {
          ...current,
          state: flow.state,
          authorizationURL: flow.authorization_url || current.authorizationURL,
        });
        this.schedulePoll(name, flowId);
        return;
      }
      this.clearPoll(name);
      this.clearMessageListener(name);
      this.popups.get(name)?.close();
      this.popups.delete(name);
      if (flow.state === 'succeeded') {
        this.removeOAuth(name);
        await this.load();
      } else {
        this.setOAuth(name, {
          ...current,
          state: flow.state,
          authorizationURL: '',
          error: flow.error || 'Authorization did not complete',
        });
      }
    } catch (error) {
      const current = this.state.value.oauth[name];
      if (current?.flowId !== flowId) return;
      if (error instanceof APIError && error.status === 404) {
        // The flow no longer exists server-side (expired and pruned, or the
        // server restarted). Stop polling instead of retrying forever.
        this.clearPoll(name);
        this.clearMessageListener(name);
        this.popups.get(name)?.close();
        this.popups.delete(name);
        this.setOAuth(name, {
          ...current,
          state: 'failed',
          authorizationURL: '',
          error: 'Authorization flow expired — try signing in again',
        });
        return;
      }
      this.setOAuth(name, { ...current, error: errorMessage(error) });
      this.schedulePoll(name, flowId);
    }
  }

  private setOAuth(name: string, flow: MCPOAuthUIState): void {
    this.state.value = {
      ...this.state.value,
      oauth: { ...this.state.value.oauth, [name]: flow },
    };
  }

  private removeOAuth(name: string): void {
    this.clearPoll(name);
    this.clearMessageListener(name);
    const oauth = { ...this.state.value.oauth };
    delete oauth[name];
    this.flowSessions.delete(name);
    this.state.value = { ...this.state.value, oauth };
  }

  private pruneOAuth(sessionId: string): void {
    for (const [name, ownerSessionId] of this.flowSessions) {
      if (ownerSessionId === sessionId) continue;
      this.clearPoll(name);
      this.clearMessageListener(name);
      this.popups.get(name)?.close();
      this.popups.delete(name);
      const oauth = { ...this.state.value.oauth };
      delete oauth[name];
      this.flowSessions.delete(name);
      this.state.value = { ...this.state.value, oauth };
    }
  }

  private clearMessageListener(name: string): void {
    const listener = this.messageListeners.get(name);
    if (listener) window.removeEventListener('message', listener);
    this.messageListeners.delete(name);
  }

  private clearPoll(name: string): void {
    const timer = this.pollTimers.get(name);
    if (timer) clearTimeout(timer);
    this.pollTimers.delete(name);
  }
}
