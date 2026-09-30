import type { AppConfig } from '../app/config';
import type { Message, Session } from '../domain/types';
import { PersistentCache, type CacheLimits } from '../platform/persistent-cache';
import type { RuntimeOption } from './store-types';

/**
 * Last-known workspace persistence.
 *
 * Policy (see frontend/README.md "Startup caches"):
 * - Records are display hints. Every restored value is reconciled by the
 *   normal authoritative sidebar/session/discovery reads; nothing here decides
 *   authorization, liveness, pending interactions, or availability.
 * - The scope combines origin + UI prefix + Hub node + a server-injected opaque
 *   store/auth identity. Without that identity, private persistence is off.
 *   Credentials are never used as key material.
 * - Private content may be shown before the first API response only when the
 *   HTML navigation itself was authorization-gated (passkey, Hub proxy) or the
 *   server runs without auth. Bearer-token servers serve public HTML, so their
 *   cached workspace waits for the first authenticated response.
 * - Any 401/403 purges the scope. Records carry a schema version and age.
 */

export const WORKSPACE_CACHE_SCHEMA = 1;

const DAY = 24 * 60 * 60 * 1_000;
/** Older workspace snapshots are more misleading than useful. */
export const WORKSPACE_MAX_AGE_MS = 14 * DAY;
/** Provider/model catalogs older than this are not shown at all. */
export const DISCOVERY_MAX_AGE_MS = 7 * DAY;
/** Model catalogs younger than this are not refetched at startup (server caches 15 min). */
export const MODEL_CATALOG_FRESH_MS = 10 * 60 * 1_000;
/** Messages retained for one cached conversation (bounded tail window). */
export const CACHED_MESSAGE_LIMIT = 80;
const WRITE_DELAY_MS = 600;

export const WORKSPACE_LIMITS: CacheLimits = {
  maxEntries: 2,
  maxBytes: 1_600_000,
  maxRecordBytes: 800_000,
};
export const SESSION_LIMITS: CacheLimits = {
  // Eight conversations plus their `#number` route aliases.
  maxEntries: 16,
  maxBytes: 4_000_000,
  maxRecordBytes: 700_000,
};
export const DISCOVERY_LIMITS: CacheLimits = {
  maxEntries: 12,
  maxBytes: 2_000_000,
  maxRecordBytes: 600_000,
};

export type PrivateDisplayPolicy = 'immediate' | 'after-auth' | 'disabled';

export interface CachedSidebar {
  schema: number;
  projectsEnabled: boolean;
  worktreesEnabled: boolean;
  showArchived: boolean;
  payload: Record<string, unknown>;
}

export interface CachedSession {
  schema: number;
  /** Durable transcript revision that `session.messages` were read at. */
  bodiesRev: number;
  session: Session;
}

export interface CachedProviders {
  schema: number;
  payload: Record<string, unknown>;
}

export interface CachedModels {
  schema: number;
  /** Provider entry fingerprint; a config change invalidates the catalog. */
  fingerprint: string;
  models: RuntimeOption[];
}

export interface StartupSnapshot {
  sidebar: { value: CachedSidebar; updated: number } | null;
  session: { value: CachedSession; updated: number } | null;
}

export function privateDisplayPolicy(config: AppConfig): PrivateDisplayPolicy {
  if (!config.cacheScope) return 'disabled';
  if (config.shellAuthorized || config.passkeyAuth || config.hub?.nodeBasePath) return 'immediate';
  // Bearer mode serves public HTML: wait for the first authenticated response.
  return 'after-auth';
}

const signedParam = /[?&](?:sig|signature|token|access_token|expires|x-amz-[a-z-]+|key)=/i;
/** Signed/credentialed URLs embedded anywhere in text (e.g. markdown links). */
const embeddedSignedURL =
  /\b(https?:\/\/[^\s"'<>()?#]*)\?[^\s"'<>()#]*?\b(?:sig|signature|token|access_token|expires|x-amz-[a-z-]+|key)=[^\s"'<>()]*/gi;
const embeddedCredentialURL = /\b(https?:\/\/)[^\s/@"'<>()]+@/gi;
/** Large inline payloads in text (base64 images) are attachment bytes too. */
const embeddedDataURL = /data:[a-z0-9.+-]+\/[a-z0-9.+-]+;base64,[a-z0-9+/=]{256,}/gi;

/** Drops inline blobs and signed/credentialed URLs; never persists attachment bytes. */
function scrubURL(value: string): string {
  if (/^(?:data|blob):/i.test(value)) return '';
  if (signedParam.test(value)) return '';
  if (/^[a-z][a-z0-9+.-]*:\/\/[^/?#]*@/i.test(value)) return '';
  return value;
}

/** Keeps readable text but removes signing material, credentials and inline bytes. */
function scrubText(value: string): string {
  if (!value.includes('://') && !value.includes('data:')) return value;
  return value
    .replace(embeddedSignedURL, '$1')
    .replace(embeddedCredentialURL, '$1')
    .replace(embeddedDataURL, '');
}

/** Recursively sanitizes any server payload before it is persisted. */
export function scrub(value: unknown, key = ''): unknown {
  if (typeof value === 'string')
    return /url|uri|src$/i.test(key) ? scrubURL(value) : scrubText(value);
  if (Array.isArray(value)) return value.map((entry) => scrub(entry, key));
  if (!value || typeof value !== 'object') return value;
  const result: Record<string, unknown> = {};
  for (const [name, entry] of Object.entries(value as Record<string, unknown>)) {
    // Local previews and in-memory files are browser-only resources.
    if (name === 'file' || name === 'blobRef' || name === 'dataURL') continue;
    result[name] = scrub(entry, name);
  }
  return result;
}

const durable = (message: Message) =>
  Boolean((message.durableSourceRowIds as unknown[] | undefined)?.length) ||
  message.serverSeq !== undefined;

/**
 * Bounded, sanitized copy of a hydrated session. Live ownership, pending
 * interactions, history anchors and the installed-bodies revision are removed
 * so a restored row can never resume a stream, show an actionable prompt,
 * acknowledge attention, or page older history from unverified state.
 */
export function cacheableSession(session: Session): CachedSession | null {
  if (session.messageBodiesRev === undefined || session.id.startsWith('draft_')) return null;
  if (session.delegated || session.parentSessionId) return null;
  let messages = session.messages.filter(durable);
  if (messages.length > CACHED_MESSAGE_LIMIT) {
    messages = messages.slice(-CACHED_MESSAGE_LIMIT);
    // Start on a user turn so a restored window never opens mid tool group.
    const firstUser = messages.findIndex((message) => message.role === 'user');
    messages = firstUser < 0 ? [] : messages.slice(firstUser);
  }
  const rest: Partial<Session> = { ...session };
  for (const field of TRANSIENT_SESSION_FIELDS) delete rest[field];
  return {
    schema: WORKSPACE_CACHE_SCHEMA,
    bodiesRev: session.messageBodiesRev,
    session: scrub({
      ...rest,
      // The cached bodies describe exactly this revision; a newer server
      // revision must look newer so normal reconciliation refreshes them.
      transcriptRev: session.messageBodiesRev,
      messages,
    }) as Session,
  };
}

/** Live, interaction and paging state that only the server may establish. */
const TRANSIENT_SESSION_FIELDS: ReadonlyArray<keyof Session> = [
  'activeRun',
  'activeResponseId',
  'interactionRequired',
  'interactionResponseId',
  'interactionStateRev',
  'pendingInteractionCount',
  'pendingInteractionKinds',
  'interactionRequiredSince',
  'olderTranscriptAnchors',
  'messageBodiesRev',
];

/** Restores a cached session as an explicitly unverified, non-live row. */
export function restoredSession(record: CachedSession): Session {
  return {
    ...record.session,
    messages: Array.isArray(record.session.messages) ? record.session.messages : [],
    activeRun: false,
    activeResponseId: null,
    interactionRequired: false,
    pendingInteractionCount: 0,
    pendingInteractionKinds: [],
  };
}

const validSession = (value: unknown): value is CachedSession => {
  const record = value as CachedSession | null;
  return Boolean(
    record &&
    record.schema === WORKSPACE_CACHE_SCHEMA &&
    record.session &&
    typeof record.session.id === 'string' &&
    Array.isArray(record.session.messages),
  );
};

const validSidebar = (value: unknown): value is CachedSidebar => {
  const record = value as CachedSidebar | null;
  return Boolean(
    record &&
    record.schema === WORKSPACE_CACHE_SCHEMA &&
    record.payload &&
    typeof record.payload === 'object',
  );
};

export class WorkspaceCache {
  readonly policy: PrivateDisplayPolicy;
  private readonly scope: string;
  private generation = 0;
  private readonly pending = new Map<string, { timer: number; write: () => Promise<unknown> }>();

  constructor(
    config: AppConfig,
    private readonly cache: PersistentCache,
    private readonly now: () => number = () => Date.now(),
  ) {
    this.policy = privateDisplayPolicy(config);
    const origin = typeof location === 'undefined' ? '' : location.origin;
    this.scope = config.cacheScope
      ? `ui${WORKSPACE_CACHE_SCHEMA}|${origin}${config.prefix}|${config.hub?.nodeId || ''}|${config.cacheScope}`
      : '';
  }

  get enabled(): boolean {
    return this.policy !== 'disabled';
  }

  private namespace(kind: 'workspace' | 'sessions' | 'discovery'): string {
    return `${this.scope}|${kind}`;
  }

  /** Reads the sidebar and the session the startup route would select. */
  async readStartup(sessionHint: string): Promise<StartupSnapshot> {
    if (!this.enabled) return { sidebar: null, session: null };
    const generation = this.generation;
    const [sidebar, session] = await Promise.all([
      this.cache.get<unknown>(this.namespace('workspace'), 'sidebar'),
      sessionHint ? this.readSession(sessionHint) : Promise.resolve(null),
    ]);
    if (generation !== this.generation) return { sidebar: null, session: null };
    const fresh = (updated: number) => this.now() - updated <= WORKSPACE_MAX_AGE_MS;
    return {
      sidebar:
        sidebar && validSidebar(sidebar.value) && fresh(sidebar.updated)
          ? { value: sidebar.value, updated: sidebar.updated }
          : null,
      session: session && fresh(session.updated) ? session : null,
    };
  }

  /** Looks up by id, then by durable number (routes use either). */
  async readSession(route: string): Promise<{ value: CachedSession; updated: number } | null> {
    if (!this.enabled) return null;
    const idOrNumber = route.replace(/^#/, '');
    if (!idOrNumber) return null;
    const namespace = this.namespace('sessions');
    const fresh = (updated: number) => this.now() - updated <= WORKSPACE_MAX_AGE_MS;
    const direct = await this.cache.get<unknown>(namespace, idOrNumber);
    if (direct && validSession(direct.value) && fresh(direct.updated))
      return { value: direct.value, updated: direct.updated };
    // Only a malformed/expired session record is dropped, never an alias.
    if (direct && typeof direct.value !== 'string') await this.cache.delete(namespace, idOrNumber);
    if (!/^\d+$/.test(idOrNumber)) return null;
    const alias = await this.cache.get<string>(namespace, `#${idOrNumber}`);
    if (!alias || typeof alias.value !== 'string') return null;
    const aliased = await this.cache.get<unknown>(namespace, alias.value);
    return aliased && validSession(aliased.value) && fresh(aliased.updated)
      ? { value: aliased.value, updated: aliased.updated }
      : null;
  }

  saveSidebar(record: Omit<CachedSidebar, 'schema'>): void {
    if (!this.enabled) return;
    const namespace = this.namespace('workspace');
    this.schedule('sidebar', [[namespace, 'sidebar']], () =>
      this.cache.put(
        namespace,
        'sidebar',
        { schema: WORKSPACE_CACHE_SCHEMA, ...record, payload: scrub(record.payload) },
        WORKSPACE_LIMITS,
      ),
    );
  }

  saveSession(session: Session): void {
    if (!this.enabled || session.messageBodiesRev === undefined) return;
    const namespace = this.namespace('sessions');
    const written: Array<[string, string]> = [[namespace, session.id]];
    if (session.number) written.push([namespace, `#${session.number}`]);
    this.schedule(`session:${session.id}`, written, async () => {
      // Serialize only the newest snapshot, after the debounce.
      const record = cacheableSession(session);
      if (!record) return;
      const saved = await this.cache.put(namespace, session.id, record, SESSION_LIMITS);
      if (saved && session.number)
        await this.cache.put(namespace, `#${session.number}`, session.id, SESSION_LIMITS);
    });
  }

  forgetSession(id: string, number?: number): void {
    this.cancel(`session:${id}`);
    if (!this.enabled) return;
    const namespace = this.namespace('sessions');
    void this.cache.delete(namespace, id);
    if (number) void this.cache.delete(namespace, `#${number}`);
  }

  async readProviders(): Promise<{ value: CachedProviders; updated: number } | null> {
    if (!this.enabled) return null;
    const record = await this.cache.get<CachedProviders>(this.namespace('discovery'), 'providers');
    if (
      !record ||
      record.value?.schema !== WORKSPACE_CACHE_SCHEMA ||
      !record.value.payload ||
      this.now() - record.updated > DISCOVERY_MAX_AGE_MS
    )
      return null;
    return record;
  }

  saveProviders(payload: Record<string, unknown>): void {
    if (!this.enabled) return;
    const namespace = this.namespace('discovery');
    this.schedule('providers', [[namespace, 'providers']], () =>
      this.cache.put(
        namespace,
        'providers',
        { schema: WORKSPACE_CACHE_SCHEMA, payload: scrub(payload) },
        DISCOVERY_LIMITS,
      ),
    );
  }

  async readModels(
    provider: string,
    fingerprint: string,
  ): Promise<{ value: CachedModels; updated: number } | null> {
    if (!this.enabled) return null;
    const namespace = this.namespace('discovery');
    const record = await this.cache.get<CachedModels>(namespace, `models:${provider}`);
    if (!record) return null;
    if (
      record.value?.schema !== WORKSPACE_CACHE_SCHEMA ||
      !Array.isArray(record.value.models) ||
      record.value.fingerprint !== fingerprint ||
      this.now() - record.updated > DISCOVERY_MAX_AGE_MS
    ) {
      await this.cache.delete(namespace, `models:${provider}`);
      return null;
    }
    return record;
  }

  saveModels(provider: string, fingerprint: string, models: RuntimeOption[]): void {
    if (!this.enabled) return;
    const namespace = this.namespace('discovery');
    this.schedule(`models:${provider}`, [[namespace, `models:${provider}`]], () =>
      this.cache.put(
        namespace,
        `models:${provider}`,
        { schema: WORKSPACE_CACHE_SCHEMA, fingerprint, models: scrub(models) },
        DISCOVERY_LIMITS,
      ),
    );
  }

  /** Writes queued snapshots now (page hide) instead of after the debounce. */
  flush(): void {
    for (const [key, entry] of [...this.pending]) {
      window.clearTimeout(entry.timer);
      this.pending.delete(key);
      void entry.write();
    }
  }

  /**
   * Drops this scope's private records and every queued write. Used for
   * 401/403, account/token changes, and the explicit "clear local cache".
   */
  async purge(): Promise<void> {
    this.generation += 1;
    for (const key of [...this.pending.keys()]) this.cancel(key);
    if (!this.scope) return;
    await this.cache.purge(`${this.scope}|`);
  }

  dispose(): void {
    this.flush();
  }

  /**
   * Debounces one logical write. Only the newest snapshot per key is written,
   * never across a purge; if a purge lands while the write is in flight, the
   * records this write produced are deleted again (and nothing else, so
   * snapshots saved after a reauthentication survive).
   */
  private schedule(
    key: string,
    records: Array<[string, string]>,
    write: () => Promise<unknown>,
  ): void {
    this.cancel(key);
    const generation = this.generation;
    const guarded = async () => {
      if (generation !== this.generation) return;
      await write();
      if (generation !== this.generation)
        await Promise.all(records.map(([namespace, id]) => this.cache.delete(namespace, id)));
    };
    const timer = window.setTimeout(() => {
      this.pending.delete(key);
      void guarded();
    }, WRITE_DELAY_MS);
    this.pending.set(key, { timer, write: guarded });
  }

  private cancel(key: string): void {
    const entry = this.pending.get(key);
    if (!entry) return;
    window.clearTimeout(entry.timer);
    this.pending.delete(key);
  }
}

/** Stable fingerprint of the provider entry that owns a model catalog. */
export function providerFingerprint(provider: RuntimeOption | undefined): string {
  if (!provider) return '';
  const source = provider as Record<string, unknown>;
  return JSON.stringify([
    source.id,
    source.type,
    source.configured,
    source.is_builtin,
    source.default_model,
    source.service_tier,
  ]);
}
