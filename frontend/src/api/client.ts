import type { AppConfig } from '../app/config';

export type FetchPolicy = 'safe-read' | 'idempotent-mutation' | 'mutation' | 'stream';
export type AuthOwner = 'session' | 'caller' | 'ignore';

export class APIError extends Error {
  readonly type: string;
  constructor(
    message: string,
    readonly status: number,
    readonly body = '',
    type = '',
  ) {
    super(message);
    this.name = 'APIError';
    this.type = type;
  }
}

export interface TransportHooks {
  getToken(): string;
  onAuthRequired(): void;
  onNetworkState?(state: 'online' | 'offline' | 'retrying', detail?: string): void;
  onVersionMismatch?(): void;
}

export interface RequestControls {
  policy?: FetchPolicy;
  auth?: AuthOwner;
  retries?: number;
  timeoutMs?: number;
  versionCheck?: boolean;
}

export interface RequestClassification {
  method: string;
  policy: FetchPolicy;
  retryable: boolean;
  retries: number;
  timeoutMs: number;
}
interface TransportRequestInit extends RequestInit {
  __termLLMRetrySafe?: boolean;
  __termLLMSkipVersionCheck?: boolean;
}

const SAFE_METHODS = new Set(['GET', 'HEAD', 'OPTIONS']);
const RETRYABLE_STATUSES = new Set([408, 425, 429]);
const MAX_RETRY_AFTER_MS = 60_000;
const DEFAULT_TIMEOUT_MS = 15_000;
const DEFAULT_MUTATION_TIMEOUT_MS = 30_000;

const sleep = (milliseconds: number, signal?: AbortSignal): Promise<void> =>
  new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(signal.reason || new DOMException('Aborted', 'AbortError'));
      return;
    }
    const timer = setTimeout(resolve, milliseconds);
    signal?.addEventListener(
      'abort',
      () => {
        clearTimeout(timer);
        reject(signal.reason || new DOMException('Aborted', 'AbortError'));
      },
      { once: true },
    );
  });

export function retryDelay(attempt: number): number {
  const normalized = Math.max(0, Math.trunc(Number(attempt) || 0));
  return normalized >= 5 ? 60_000 : Math.round(1_000 * 1.5 ** normalized);
}

export function retryAfterDelay(response: Pick<Response, 'headers'>, now = Date.now()): number {
  const value = String(response.headers.get('Retry-After') || '').trim();
  if (!value) return 0;
  const seconds = Number(value);
  if (Number.isFinite(seconds) && seconds >= 0)
    return Math.min(MAX_RETRY_AFTER_MS, seconds * 1_000);
  const date = Date.parse(value);
  return Number.isFinite(date) ? Math.min(MAX_RETRY_AFTER_MS, Math.max(0, date - now)) : 0;
}

export function classifyRequest(
  init: RequestInit = {},
  controls: RequestControls = {},
): RequestClassification {
  const method = String(init.method || 'GET').toUpperCase();
  let policy = controls.policy || (SAFE_METHODS.has(method) ? 'safe-read' : 'mutation');
  const headers = new Headers(init.headers);
  if (policy === 'idempotent-mutation' && method === 'POST' && !headers.has('Idempotency-Key'))
    policy = 'mutation';
  const retryable = policy === 'safe-read' || policy === 'idempotent-mutation';
  return {
    method,
    policy,
    retryable,
    retries: retryable ? Math.max(0, controls.retries ?? 2) : 0,
    timeoutMs:
      controls.timeoutMs === 0
        ? 0
        : Math.max(
            1,
            controls.timeoutMs ??
              (SAFE_METHODS.has(method) ? DEFAULT_TIMEOUT_MS : DEFAULT_MUTATION_TIMEOUT_MS),
          ),
  };
}

export function trustedExternalLoginURL(
  response: Pick<Response, 'status' | 'headers'>,
  currentURL = location.href,
): string {
  if (response.status !== 401) return '';
  const raw = String(response.headers.get('X-Term-LLM-Login-URL') || '').trim();
  if (!raw) return '';
  try {
    const parsed = new URL(raw, currentURL);
    return parsed.protocol === 'http:' || parsed.protocol === 'https:' ? parsed.href : '';
  } catch {
    return '';
  }
}

function retryableStatus(status: number): boolean {
  return RETRYABLE_STATUSES.has(status) || status >= 500;
}
function abortError(error: unknown): boolean {
  return error instanceof DOMException
    ? error.name === 'AbortError'
    : (error as { name?: string } | null)?.name === 'AbortError';
}

function timeoutSignal(
  parent: AbortSignal | null | undefined,
  timeoutMs: number,
): { signal?: AbortSignal; cleanup(): void } {
  if (!timeoutMs) return { signal: parent || undefined, cleanup() {} };
  const controller = new AbortController();
  const abort = () => controller.abort(parent?.reason || new DOMException('Aborted', 'AbortError'));
  parent?.addEventListener('abort', abort, { once: true });
  const timer = setTimeout(
    () => controller.abort(new DOMException('Request timed out', 'TimeoutError')),
    timeoutMs,
  );
  return {
    signal: controller.signal,
    cleanup() {
      clearTimeout(timer);
      parent?.removeEventListener('abort', abort);
    },
  };
}

let externalAuthRedirecting = false;

function rawJSON<T>(value: string): T {
  if (!value.trim()) return undefined as T;
  return JSON.parse(value) as T;
}

export interface UploadControls {
  signal?: AbortSignal;
  onProgress?: (loaded: number, total?: number) => void;
}

export class APIClient {
  constructor(
    readonly config: AppConfig,
    private hooks: TransportHooks,
  ) {}

  url(path: string): string {
    if (/^https?:\/\//.test(path)) return path;
    const prefix = this.config.prefix.replace(/\/+$/, '');
    if (path === prefix || path.startsWith(`${prefix}/`)) return path;
    return `${prefix}${path.startsWith('/') ? path : `/${path}`}`;
  }

  async request(
    path: string,
    init: RequestInit = {},
    policyOrControls?: FetchPolicy | RequestControls,
  ): Promise<Response> {
    const controls: RequestControls =
      typeof policyOrControls === 'string'
        ? { policy: policyOrControls }
        : { ...(policyOrControls || {}) };
    const classification = classifyRequest(init, controls);
    const targetURL = this.url(path);
    const sameOrigin = new URL(targetURL, location.href).origin === location.origin;
    const headers = new Headers(init.headers);
    headers.set('Accept', headers.get('Accept') || 'application/json');
    const token = this.hooks.getToken();
    if (!sameOrigin && controls.auth !== 'caller') headers.delete('Authorization');
    if (controls.auth !== 'ignore' && sameOrigin && token && !headers.has('Authorization'))
      headers.set('Authorization', `Bearer ${token}`);
    if (this.config.version) headers.set('X-Term-LLM-UI-Version', this.config.version);
    if (init.body && !(init.body instanceof FormData) && !headers.has('Content-Type'))
      headers.set('Content-Type', 'application/json');
    let lastError: unknown;
    for (let attempt = 0; attempt <= classification.retries; attempt += 1) {
      const timed = timeoutSignal(init.signal, classification.timeoutMs);
      try {
        const transportInit: TransportRequestInit = {
          ...init,
          signal: timed.signal,
          headers,
          credentials: sameOrigin ? 'same-origin' : 'omit',
          __termLLMRetrySafe: classification.retryable,
          __termLLMSkipVersionCheck: controls.versionCheck === false,
        };
        const response = await fetch(targetURL, transportInit);
        const serverVersion = response.headers.get('X-Term-LLM-UI-Version');
        if (
          controls.versionCheck !== false &&
          serverVersion &&
          this.config.version &&
          serverVersion !== this.config.version
        )
          this.hooks.onVersionMismatch?.();
        if (response.status === 401 && controls.auth === 'session' && !externalAuthRedirecting) {
          const loginURL = trustedExternalLoginURL(response);
          if (loginURL) {
            externalAuthRedirecting = true;
            location.assign(loginURL);
          }
        }
        if (
          (response.status === 401 || response.status === 403) &&
          controls.auth !== 'ignore' &&
          !externalAuthRedirecting
        )
          this.hooks.onAuthRequired();
        if (
          retryableStatus(response.status) &&
          classification.retryable &&
          attempt < classification.retries
        ) {
          this.hooks.onNetworkState?.('retrying', `Server returned ${response.status}`);
          try {
            await response.body?.cancel();
          } catch {
            /* A replacement request owns recovery. */
          }
          timed.cleanup();
          await sleep(retryAfterDelay(response) || retryDelay(attempt), init.signal || undefined);
          continue;
        }
        this.hooks.onNetworkState?.('online');
        timed.cleanup();
        return response;
      } catch (error) {
        timed.cleanup();
        lastError = error;
        if (abortError(error) && init.signal?.aborted) throw error;
        this.hooks.onNetworkState?.(
          navigator.onLine === false ? 'offline' : 'retrying',
          String(error),
        );
        if (!classification.retryable || attempt >= classification.retries) throw error;
        await sleep(retryDelay(attempt), init.signal || undefined);
      }
    }
    throw lastError;
  }

  async json<T>(
    path: string,
    init: RequestInit = {},
    policyOrControls?: FetchPolicy | RequestControls,
  ): Promise<T> {
    const response = await this.request(path, init, policyOrControls);
    if (!response.ok) {
      const body = await response.text();
      let message = body || `${response.status} ${response.statusText}`;
      let type = '';
      try {
        const parsed = JSON.parse(body) as {
          error?: string | { message?: string; type?: string; code?: string };
          message?: string;
          type?: string;
          code?: string;
        };
        if (typeof parsed.error === 'object') {
          message = parsed.error.message || message;
          type = parsed.error.type || parsed.error.code || '';
        } else {
          message = parsed.error || parsed.message || message;
          type = parsed.type || parsed.code || '';
        }
      } catch {
        /* Plain text error. */
      }
      throw new APIError(message, response.status, body, type);
    }
    if ([204, 205, 304].includes(response.status)) return undefined as T;
    return response.json() as Promise<T>;
  }

  get<T>(
    path: string,
    signal?: AbortSignal,
    controls?: Omit<RequestControls, 'policy'>,
  ): Promise<T> {
    return this.json<T>(path, { signal }, { policy: 'safe-read', ...controls });
  }
  post<T>(
    path: string,
    body: unknown,
    policy: FetchPolicy = 'mutation',
    headers?: HeadersInit,
  ): Promise<T> {
    return this.json<T>(
      path,
      { method: 'POST', headers, body: body instanceof FormData ? body : JSON.stringify(body) },
      policy,
    );
  }
  patch<T>(path: string, body: unknown): Promise<T> {
    return this.json<T>(
      path,
      { method: 'PATCH', body: JSON.stringify(body) },
      'idempotent-mutation',
    );
  }
  upload<T>(path: string, body: FormData, controls: UploadControls = {}): Promise<T> {
    const targetURL = this.url(path);
    const sameOrigin = new URL(targetURL, location.href).origin === location.origin;
    return new Promise<T>((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      let settled = false;
      const finish = (callback: () => void) => {
        if (settled) return;
        settled = true;
        controls.signal?.removeEventListener('abort', abort);
        callback();
      };
      const abort = () => {
        xhr.abort();
        finish(() =>
          reject(controls.signal?.reason || new DOMException('Upload cancelled', 'AbortError')),
        );
      };
      if (controls.signal?.aborted) {
        abort();
        return;
      }
      xhr.open('POST', targetURL, true);
      xhr.withCredentials = sameOrigin;
      xhr.setRequestHeader('Accept', 'application/json');
      const token = this.hooks.getToken();
      if (sameOrigin && token) xhr.setRequestHeader('Authorization', `Bearer ${token}`);
      if (this.config.version) xhr.setRequestHeader('X-Term-LLM-UI-Version', this.config.version);
      xhr.upload.onprogress = (event) =>
        controls.onProgress?.(event.loaded, event.lengthComputable ? event.total : undefined);
      xhr.onerror = () => {
        this.hooks.onNetworkState?.(navigator.onLine === false ? 'offline' : 'retrying');
        finish(() => reject(new TypeError('Transcription upload failed')));
      };
      xhr.onabort = () => finish(() => reject(new DOMException('Upload cancelled', 'AbortError')));
      xhr.onload = () => {
        const responseHeaders = new Headers();
        for (const line of xhr
          .getAllResponseHeaders()
          .trim()
          .split(/[\r\n]+/)) {
          const separator = line.indexOf(':');
          if (separator > 0)
            responseHeaders.append(
              line.slice(0, separator).trim(),
              line.slice(separator + 1).trim(),
            );
        }
        const serverVersion = responseHeaders.get('X-Term-LLM-UI-Version');
        if (serverVersion && this.config.version && serverVersion !== this.config.version)
          this.hooks.onVersionMismatch?.();
        if ((xhr.status === 401 || xhr.status === 403) && !externalAuthRedirecting) {
          const loginURL = trustedExternalLoginURL({
            status: xhr.status,
            headers: responseHeaders,
          });
          if (loginURL) {
            externalAuthRedirecting = true;
            location.assign(loginURL);
          } else {
            this.hooks.onAuthRequired();
          }
        }
        if (xhr.status < 200 || xhr.status >= 300) {
          const raw = xhr.responseText || '';
          let message = raw || `Upload returned ${xhr.status}`;
          let type = '';
          try {
            const parsed = JSON.parse(raw) as {
              error?: string | { message?: string; type?: string; code?: string };
              message?: string;
              type?: string;
            };
            if (typeof parsed.error === 'object') {
              message = parsed.error.message || message;
              type = parsed.error.type || parsed.error.code || '';
            } else {
              message = parsed.error || parsed.message || message;
              type = parsed.type || '';
            }
          } catch {
            /* Plain text response. */
          }
          finish(() => reject(new APIError(message, xhr.status, raw, type)));
          return;
        }
        this.hooks.onNetworkState?.('online');
        try {
          const value = rawJSON<T>(xhr.responseText);
          finish(() => resolve(value));
        } catch (error) {
          finish(() => reject(error));
        }
      };
      controls.signal?.addEventListener('abort', abort, { once: true });
      xhr.send(body);
    });
  }

  delete<T>(path: string, body?: unknown): Promise<T> {
    return this.json<T>(
      path,
      { method: 'DELETE', body: body === undefined ? undefined : JSON.stringify(body) },
      'idempotent-mutation',
    );
  }
}

export interface SSEMessage {
  event: string;
  data: string;
  id?: string;
  /** True when the server explicitly sent the SSE stream completion sentinel. */
  done?: true;
}

function decodeSSEBlock(block: string, onActivity?: () => void): SSEMessage | null {
  let event = 'message';
  let id: string | undefined;
  const data: string[] = [];
  for (const line of block.split(/\r?\n/)) {
    if (line.startsWith(':')) {
      onActivity?.();
      continue;
    }
    const separator = line.indexOf(':');
    const field = separator < 0 ? line : line.slice(0, separator);
    const valueText = separator < 0 ? '' : line.slice(separator + 1).replace(/^ /, '');
    if (field === 'event') event = valueText;
    else if (field === 'data') data.push(valueText);
    else if (field === 'id') id = valueText;
  }
  if (!data.length) return null;
  const value = data.join('\n');
  return { event, data: value, id, ...(value.trim() === '[DONE]' ? { done: true as const } : {}) };
}

export async function* decodeSSE(
  body: ReadableStream<Uint8Array>,
  signal?: AbortSignal,
  onActivity?: () => void,
): AsyncGenerator<SSEMessage> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  try {
    while (!signal?.aborted) {
      const { value, done } = await reader.read();
      if (value?.length) onActivity?.();
      buffer += decoder.decode(value || new Uint8Array(), { stream: !done });
      let boundary: number;
      while ((boundary = buffer.search(/\r?\n\r?\n/)) >= 0) {
        const block = buffer.slice(0, boundary);
        buffer = buffer.slice(boundary).replace(/^\r?\n\r?\n/, '');
        const message = decodeSSEBlock(block, onActivity);
        if (message) yield message;
      }
      if (done) {
        // SSE producers normally terminate events with a blank line, but a
        // transport can close immediately after the final field. TextDecoder's
        // final non-streaming decode above flushes a fragmented UTF-8 codepoint;
        // dispatch the remaining complete field block exactly once.
        const message = decodeSSEBlock(buffer.replace(/\r?\n$/, ''), onActivity);
        buffer = '';
        if (message) yield message;
        break;
      }
    }
  } finally {
    try {
      await reader.cancel(signal?.reason);
    } catch {
      /* Reader is already closed. */
    }
    reader.releaseLock();
  }
}
