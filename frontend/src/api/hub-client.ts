import { hubPath, type HubConfig } from '../hub/config';
import type {
  AddNodeResponse,
  AttentionResponse,
  CredentialsResponse,
  DelegationsResponse,
  HubCredential,
  HubSessionResponse,
  NodeFormData,
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

  constructor(status: number, message: string) {
    super(message);
    this.name = 'HubAPIError';
    this.status = status;
  }
}

type Fetch = typeof fetch;
type Navigate = (url: string) => void;

interface RequestOptions {
  method?: 'GET' | 'POST' | 'PATCH' | 'DELETE';
  body?: unknown;
  signal?: AbortSignal;
  redirectOnUnauthorized?: boolean;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return Boolean(value) && typeof value === 'object' && !Array.isArray(value);
}

function errorMessage(value: unknown, fallback: string): string {
  if (!isRecord(value)) return fallback;
  const error = value.error;
  if (isRecord(error) && typeof error.message === 'string' && error.message.trim())
    return error.message;
  if (typeof value.message === 'string' && value.message.trim()) return value.message;
  return fallback;
}

export class HubClient {
  private readonly fetcher: Fetch;
  private readonly navigate: Navigate;

  constructor(
    readonly config: Pick<HubConfig, 'basePath' | 'authMode'>,
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
    const contentType = response.headers.get('Content-Type')?.toLowerCase() ?? '';
    let value: unknown;
    if (contentType.includes('json')) {
      value = await response.json().catch(() => undefined);
    } else {
      const text = await response.text().catch(() => '');
      value = text || undefined;
    }
    if (!response.ok) {
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
      const fallback =
        typeof value === 'string' && value.trim()
          ? value.trim()
          : `Hub request failed (${response.status}).`;
      throw new HubAPIError(response.status, errorMessage(value, fallback));
    }
    if (value === undefined)
      throw new HubAPIError(response.status, 'Hub returned an empty response.');
    return value as T;
  }

  listNodes(signal?: AbortSignal): Promise<NodesResponse> {
    return this.request('/api/nodes', { signal });
  }

  listAttention(signal?: AbortSignal): Promise<AttentionResponse> {
    return this.request('/api/attention', { signal });
  }

  listDelegations(signal?: AbortSignal): Promise<DelegationsResponse> {
    return this.request('/api/delegations', { signal });
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
  ): Promise<RedirectResponse> {
    return this.request(`${prefix}/register/finish`, {
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

  beginReauthentication(): Promise<WebAuthnRequestWireOptions> {
    return this.request('/api/auth/reauth/begin', { method: 'POST', body: {} });
  }

  finishReauthentication(credential: SerializedPublicKeyCredential): Promise<{ ok: boolean }> {
    return this.request('/api/auth/reauth/finish', { method: 'POST', body: credential });
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
