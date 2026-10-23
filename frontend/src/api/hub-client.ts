import { hubPath, type HubConfig } from '../hub/config';
import type {
  AddNodeResponse,
  AttentionResponse,
  CredentialsResponse,
  DelegationsResponse,
  HubCredential,
  HubSessionResponse,
  NodeFormData,
  NodeOrderResponse,
  NodesResponse,
  RedirectResponse,
  RegistrationInfoResponse,
  RevokeSessionsResponse,
  SerializedPublicKeyCredential,
  TestNodeResponse,
  WebAuthnCreationWireOptions,
  WebAuthnRequestWireOptions,
} from '../hub/domain/types';

export class HubAPIError extends Error {
  readonly status: number;
  readonly type: string;

  constructor(status: number, message: string, type = '') {
    super(message);
    this.name = 'HubAPIError';
    this.status = status;
    this.type = type;
  }
}

type Fetch = typeof fetch;
type Navigate = (url: string) => void;

interface RequestOptions {
  method?: 'GET' | 'POST' | 'PATCH' | 'DELETE';
  body?: unknown;
  signal?: AbortSignal;
  redirectOnUnauthorized?: boolean;
  boundedRead?: boolean;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return Boolean(value) && typeof value === 'object' && !Array.isArray(value);
}

function errorType(value: unknown): string {
  if (!isRecord(value) || !isRecord(value.error)) return '';
  return typeof value.error.type === 'string' ? value.error.type : '';
}

function errorMessage(value: unknown, fallback: string): string {
  if (!isRecord(value)) return fallback;
  const error = value.error;
  if (isRecord(error) && typeof error.message === 'string' && error.message.trim())
    return error.message;
  if (typeof value.message === 'string' && value.message.trim()) return value.message;
  return fallback;
}

// Dashboard snapshots only: mutations, authentication and future long polls do
// not inherit this budget. The deadline includes headers and body consumption.
const dashboardReadMilliseconds = 10_000;
const dashboardBodyBytes = 8 << 20;

async function readDashboardBody(response: Response, signal: AbortSignal): Promise<string> {
  if (!response.body) return '';
  const reader = response.body.getReader();
  const abort = () => {
    void reader.cancel(signal.reason).catch(() => undefined);
  };
  signal.addEventListener('abort', abort, { once: true });
  if (signal.aborted) abort();
  try {
    const decoder = new TextDecoder();
    let bytes = 0;
    let text = '';
    for (;;) {
      const { done, value } = await reader.read();
      if (signal.aborted) throw signal.reason;
      if (done) break;
      bytes += value.byteLength;
      if (bytes > dashboardBodyBytes) {
        void reader.cancel().catch(() => undefined);
        throw new Error('Hub response is too large.');
      }
      text += decoder.decode(value, { stream: true });
    }
    return text + decoder.decode();
  } finally {
    signal.removeEventListener('abort', abort);
    reader.releaseLock();
  }
}

export class HubClient {
  private readonly fetcher: Fetch;
  private readonly navigate: Navigate;
  private readonly unauthorizedListeners = new Set<() => Promise<void>>();

  onUnauthorized(listener: () => Promise<void>): () => void {
    this.unauthorizedListeners.add(listener);
    return () => this.unauthorizedListeners.delete(listener);
  }

  constructor(
    readonly config: Pick<
      HubConfig,
      'basePath' | 'authMode' | 'cacheScope' | 'cacheDisplayAllowed'
    >,
    options: { fetch?: Fetch; navigate?: Navigate } = {},
  ) {
    this.fetcher = options.fetch ?? window.fetch.bind(window);
    this.navigate = options.navigate ?? ((url) => window.location.assign(url));
  }

  private mounted(path: string): string {
    return hubPath(this.config.basePath, path);
  }

  private async request<T>(path: string, options: RequestOptions = {}): Promise<T> {
    const method = options.method ?? 'GET';
    const headers = new Headers({ Accept: 'application/json' });
    let body: string | undefined;
    if (options.body !== undefined) {
      headers.set('Content-Type', 'application/json');
      body = JSON.stringify(options.body);
    }
    const response = await this.fetcher(this.mounted(path), {
      method,
      headers,
      body,
      credentials: 'same-origin',
      signal: options.signal,
    });
    // Revoke cached display as soon as headers arrive, before reading a body
    // that may stall and before passkey sign-in navigation leaves this page.
    if (response.status === 401 || response.status === 403) {
      await Promise.allSettled([...this.unauthorizedListeners].map((listener) => listener()));
    }
    // Authentication is a header-level decision; a stalled or canceled body
    // must not prevent sign-in navigation after cached data has been revoked.
    if (
      response.status === 401 &&
      this.config.authMode === 'passkey' &&
      options.redirectOnUnauthorized !== false
    ) {
      const supplied = response.headers.get('X-Term-LLM-Login-URL');
      const safeSupplied =
        supplied?.startsWith('/') && !supplied.startsWith('//') && !supplied.includes('\\')
          ? supplied
          : '';
      const login = safeSupplied || this.mounted('/auth/login');
      this.navigate(login);
    }
    const contentType = response.headers.get('Content-Type')?.toLowerCase() ?? '';
    const text =
      options.boundedRead && options.signal
        ? await readDashboardBody(response, options.signal)
        : await response.text().catch(() => '');
    let value: unknown;
    if (contentType.includes('json')) {
      try {
        value = JSON.parse(text);
      } catch {
        value = undefined;
      }
    } else {
      value = text || undefined;
    }
    if (!response.ok) {
      const fallback =
        typeof value === 'string' && value.trim()
          ? value.trim()
          : `Hub request failed (${response.status}).`;
      throw new HubAPIError(response.status, errorMessage(value, fallback), errorType(value));
    }
    if (value === undefined)
      throw new HubAPIError(response.status, 'Hub returned an empty response.');
    return value as T;
  }

  private async dashboardRead<T>(path: string, signal?: AbortSignal): Promise<T> {
    const controller = new AbortController();
    let rejectAbort!: (reason: unknown) => void;
    const aborted = new Promise<never>((_resolve, reject) => {
      rejectAbort = reject;
    });
    const abort = (reason?: unknown) => {
      controller.abort(reason);
      rejectAbort(controller.signal.reason);
    };
    const cancel = () => abort(signal?.reason);
    signal?.addEventListener('abort', cancel, { once: true });
    if (signal?.aborted) cancel();
    const timer = setTimeout(
      () => abort(new Error('Hub read timed out.')),
      dashboardReadMilliseconds,
    );
    try {
      // The race also bounds a fetch implementation that ignores cancellation.
      return await Promise.race([
        this.request<T>(path, { signal: controller.signal, boundedRead: true }),
        aborted,
      ]);
    } finally {
      clearTimeout(timer);
      signal?.removeEventListener('abort', cancel);
    }
  }

  listNodes(signal?: AbortSignal): Promise<NodesResponse> {
    return this.dashboardRead('/api/nodes', signal);
  }

  listAttention(signal?: AbortSignal): Promise<AttentionResponse> {
    return this.dashboardRead('/api/attention', signal);
  }

  clearAttention(): Promise<{ cleared: number; failed: number }> {
    return this.request('/api/attention/clear', { method: 'POST', body: {} });
  }

  /** Acknowledge exactly one cached review item on its originating node. */
  markAttentionSeen(item: {
    store_instance_id: string;
    session_id: string;
    attention_seq: number;
  }): Promise<{ cleared: number }> {
    return this.request('/api/attention/seen', {
      method: 'POST',
      body: {
        store_instance_id: item.store_instance_id,
        session_id: item.session_id,
        attention_seq: item.attention_seq,
      },
    });
  }

  listDelegations(signal?: AbortSignal): Promise<DelegationsResponse> {
    return this.dashboardRead('/api/delegations', signal);
  }

  testNode(value: NodeFormData): Promise<TestNodeResponse> {
    return this.request('/api/nodes/test', { method: 'POST', body: value });
  }

  addNode(value: NodeFormData): Promise<AddNodeResponse> {
    return this.request('/api/nodes', { method: 'POST', body: value });
  }

  removeNode(id: string): Promise<{ removed: string }> {
    return this.request(`/api/nodes/${encodeURIComponent(id)}`, { method: 'DELETE' });
  }

  /** Moves the listed nodes, in order, into the dashboard positions they already occupy. */
  reorderNodes(nodeIds: string[]): Promise<NodeOrderResponse> {
    return this.request('/api/nodes/order', { method: 'PATCH', body: { node_ids: nodeIds } });
  }

  registrationInfo(signal?: AbortSignal): Promise<RegistrationInfoResponse> {
    return this.request('/api/registration-info', { signal });
  }

  listCredentials(signal?: AbortSignal): Promise<CredentialsResponse> {
    return this.request('/api/auth/credentials', { signal });
  }

  session(signal?: AbortSignal): Promise<HubSessionResponse> {
    return this.request('/api/auth/session', { signal });
  }

  renameCredential(recordID: string, displayName: string): Promise<HubCredential> {
    return this.request(`/api/auth/credentials/${encodeURIComponent(recordID)}`, {
      method: 'PATCH',
      body: { display_name: displayName },
    });
  }

  removeCredential(recordID: string): Promise<{ ok: boolean }> {
    return this.request(`/api/auth/credentials/${encodeURIComponent(recordID)}`, {
      method: 'DELETE',
    });
  }

  revokeOtherSessions(): Promise<RevokeSessionsResponse> {
    return this.request('/api/auth/sessions/revoke-others', { method: 'POST', body: {} });
  }

  logout(): Promise<RedirectResponse> {
    return this.request('/api/auth/logout', { method: 'POST', body: {} });
  }

  verifyGrant(
    prefix: '/api/auth/bootstrap' | '/api/auth/recovery',
    code: string,
  ): Promise<{ ok: boolean }> {
    return this.request(`${prefix}/verify`, {
      method: 'POST',
      body: { code },
      redirectOnUnauthorized: false,
    });
  }

  beginGrantRegistration(
    prefix: '/api/auth/bootstrap' | '/api/auth/recovery',
    displayName: string,
  ): Promise<WebAuthnCreationWireOptions> {
    return this.request(`${prefix}/register/begin`, {
      method: 'POST',
      body: { display_name: displayName },
      redirectOnUnauthorized: false,
    });
  }

  finishGrantRegistration(
    prefix: '/api/auth/bootstrap' | '/api/auth/recovery',
    credential: SerializedPublicKeyCredential,
    returnPath = '',
  ): Promise<RedirectResponse> {
    // The server validates the return path against its mount and uses it only
    // after first-passkey setup (recovery always continues to sign-in).
    const query = returnPath ? `?return=${encodeURIComponent(returnPath)}` : '';
    return this.request(`${prefix}/register/finish${query}`, {
      method: 'POST',
      body: credential,
      redirectOnUnauthorized: false,
    });
  }

  beginLogin(returnPath: string): Promise<WebAuthnRequestWireOptions> {
    return this.request('/api/auth/login/begin', {
      method: 'POST',
      body: { return_path: returnPath },
      redirectOnUnauthorized: false,
    });
  }

  finishLogin(credential: SerializedPublicKeyCredential): Promise<RedirectResponse> {
    return this.request('/api/auth/login/finish', {
      method: 'POST',
      body: credential,
      redirectOnUnauthorized: false,
    });
  }

  beginReauthentication(redirectOnUnauthorized = true): Promise<WebAuthnRequestWireOptions> {
    return this.request('/api/auth/reauth/begin', {
      method: 'POST',
      body: {},
      redirectOnUnauthorized,
    });
  }

  finishReauthentication(
    credential: SerializedPublicKeyCredential,
    redirectOnUnauthorized = true,
  ): Promise<{ ok: boolean }> {
    return this.request('/api/auth/reauth/finish', {
      method: 'POST',
      body: credential,
      redirectOnUnauthorized,
    });
  }

  // The native approval page handles 401 itself so sign-in returns to it.
  authorizeNative(challenge: string): Promise<RedirectResponse> {
    return this.request('/api/auth/native/authorize', {
      method: 'POST',
      body: { challenge },
      redirectOnUnauthorized: false,
    });
  }

  beginAdditionalRegistration(displayName: string): Promise<WebAuthnCreationWireOptions> {
    return this.request('/api/auth/credentials/register/begin', {
      method: 'POST',
      body: { display_name: displayName },
    });
  }

  finishAdditionalRegistration(
    credential: SerializedPublicKeyCredential,
  ): Promise<{ ok: boolean }> {
    return this.request('/api/auth/credentials/register/finish', {
      method: 'POST',
      body: credential,
    });
  }
}
