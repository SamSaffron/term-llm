import type { Attachment, DiffComment } from '../domain/types';
import type { HubContext } from '../app/config';

export const STORAGE_BASE_KEYS = {
  token: 'term_llm_token',
  activeSession: 'term_llm_active_session',
  draftSessionActive: 'term_llm_draft_session_active',
  selectedModel: 'term_llm_selected_model',
  selectedProvider: 'term_llm_selected_provider',
  selectedEffort: 'term_llm_selected_effort',
  selectedReasoningMode: 'term_llm_selected_reasoning_mode',
  selectedAgent: 'term_llm_selected_agent',
  sidebarCollapsed: 'term_llm_sidebar_collapsed',
  diffSidebarWidth: 'term_llm_diff_sidebar_width',
  showHiddenSessions: 'term_llm_show_hidden_sessions',
  showWidgetsSidebar: 'term_llm_show_widgets_sidebar',
  notificationsEnabled: 'term_llm_notifications_enabled',
  notificationSubscriptionID: 'term_llm_notification_subscription_id',
  draftMessages: 'term_llm_draft_messages',
  pendingIntents: 'term_llm_pending_intent',
  diffCommentQueue: 'term_llm_diff_comment_queue',
  projectExpansion: 'term_llm_project_expansion',
  sidebarView: 'term_llm_sidebar_view',
  lastProject: 'term_llm_last_project',
} as const;

export type StorageName = keyof typeof STORAGE_BASE_KEYS;
export type StorageKeys = Record<StorageName, string>;

export function storageKeys(hub: HubContext | null): StorageKeys {
  const scope = hub?.nodeId ? `:${hub.nodeId}` : '';
  return Object.fromEntries(
    Object.entries(STORAGE_BASE_KEYS).map(([name, key]) => {
      // Direct node auth uses the unscoped token even when Hub context is injected.
      const scoped = name === 'token' && hub && !hub.nodeBasePath ? key : `${key}${scope}`;
      return [name, scoped];
    }),
  ) as StorageKeys;
}

export function migrateScopedStorage(storage: Storage, hub: HubContext | null): StorageKeys {
  const keys = storageKeys(hub);
  if (!hub?.nodeId) return keys;
  for (const [name, base] of Object.entries(STORAGE_BASE_KEYS) as Array<[StorageName, string]>) {
    if (name === 'token' || keys[name] === base || storage.getItem(keys[name]) !== null) continue;
    const value = storage.getItem(base);
    if (value !== null) storage.setItem(keys[name], value);
  }
  return keys;
}

export function readJSON<T>(storage: Storage, key: string, fallback: T): T {
  try {
    const value = JSON.parse(storage.getItem(key) || 'null');
    return value === null ? fallback : (value as T);
  } catch {
    return fallback;
  }
}

export function writeJSON(storage: Storage, key: string, value: unknown): void {
  try {
    storage.setItem(key, JSON.stringify(value));
  } catch {
    /* Storage pressure/private mode. */
  }
}

function normalizeQueuedDiffComment(entry: unknown, ownerSessionId = ''): DiffComment | null {
  if (!entry || typeof entry !== 'object') return null;
  const raw = entry as Record<string, unknown>;
  const side = String(raw.side || '').toLowerCase();
  const line = Number(raw.line) || 0;
  const path = String(raw.path || '');
  // Legacy queues (pre-Preact UI) stored the text as `instruction`.
  const body = String(raw.body ?? raw.instruction ?? '').trim();
  if (!path || !body || (side !== 'old' && side !== 'new') || line <= 0) return null;
  const comment: DiffComment = { path, side, line, body };
  if (raw.id) comment.id = String(raw.id);
  if (raw.parentId ?? raw.parent_id) comment.parentId = String(raw.parentId ?? raw.parent_id);
  const sessionId = String(raw.sessionId || ownerSessionId || '');
  if (sessionId) comment.sessionId = sessionId;
  if (raw.scope) comment.scope = String(raw.scope);
  const context = raw.context ?? raw.line_text;
  if (context != null && context !== '') comment.context = String(context);
  const contextBefore = raw.contextBefore ?? raw.context_before;
  if (Array.isArray(contextBefore)) comment.contextBefore = contextBefore.map(String);
  const contextAfter = raw.contextAfter ?? raw.context_after;
  if (Array.isArray(contextAfter)) comment.contextAfter = contextAfter.map(String);
  const seq = Number(raw.fileChangeSeq ?? raw.file_change_seq) || 0;
  if (seq > 0) comment.fileChangeSeq = seq;
  const rev = Number(raw.rev) || 0;
  if (rev > 0) comment.rev = rev;
  const updatedAt = Number(raw.updatedAt ?? raw.updated) || 0;
  if (updatedAt > 0) comment.updatedAt = updatedAt;
  const state = String(raw.state || '');
  if (['fresh', 'stale', 'sending', 'failed'].includes(state))
    comment.state = state as DiffComment['state'];
  if (raw.anchorFingerprint) comment.anchorFingerprint = String(raw.anchorFingerprint);
  return comment;
}

export function diffCommentKey(base: string, sessionId: string, commentId: string): string {
  return `${base}:${encodeURIComponent(sessionId)}:${encodeURIComponent(commentId)}`;
}

const diffCommentTombstoneKey = (base: string, sessionId: string, commentId: string): string =>
  `${base}:deleted:${encodeURIComponent(sessionId)}:${encodeURIComponent(commentId)}`;

const isDiffCommentDeleted = (
  storage: Storage,
  base: string,
  sessionId: string,
  commentId: string,
): boolean => storage.getItem(diffCommentTombstoneKey(base, sessionId, commentId)) !== null;

const storageKeysWithPrefix = (storage: Storage, prefix: string): string[] => {
  const keys: string[] = [];
  for (let index = 0; index < storage.length; index += 1) {
    const key = storage.key(index);
    if (key?.startsWith(prefix)) keys.push(key);
  }
  return keys;
};

const persistRecord = (storage: Storage, key: string, value: unknown): void => {
  try {
    storage.setItem(key, JSON.stringify(value));
  } catch (error) {
    throw new Error(`Could not persist browser data for ${key}: ${String(error)}`, {
      cause: error,
    });
  }
};

function legacyDiffComments(storage: Storage, key: string): DiffComment[] {
  const raw = readJSON<unknown>(storage, key, null);
  if (Array.isArray(raw))
    return raw
      .map((entry) => normalizeQueuedDiffComment(entry))
      .filter((comment): comment is DiffComment => comment !== null);
  const sessions = (raw as { sessions?: unknown } | null)?.sessions;
  if (!sessions || typeof sessions !== 'object') return [];
  const comments: DiffComment[] = [];
  for (const [sessionId, value] of Object.entries(sessions as Record<string, unknown>)) {
    const items = (value as { items?: unknown } | null)?.items;
    for (const item of Array.isArray(items) ? items : []) {
      const comment = normalizeQueuedDiffComment(item, sessionId);
      if (comment) comments.push(comment);
    }
  }
  return comments;
}

export function readDiffCommentQueue(storage: Storage, base: string): DiffComment[] {
  const records = new Map<string, DiffComment>();
  for (const key of storageKeysWithPrefix(storage, `${base}:`)) {
    if (key === `${base}:migration:v2` || key.startsWith(`${base}:deleted:`)) continue;
    const value = normalizeQueuedDiffComment(readJSON(storage, key, null));
    if (value?.id && value.sessionId) records.set(`${value.sessionId}:${value.id}`, value);
  }
  // One-release compatibility read. Migration is additive and never rewrites
  // or deletes the legacy aggregate, so an older open tab cannot erase records.
  const legacy = legacyDiffComments(storage, base);
  legacy.forEach((comment, index) => {
    const sessionId = comment.sessionId || 'unscoped';
    const id = comment.id || `legacy_${index}_${comment.path}_${comment.side}_${comment.line}`;
    const normalized = { ...comment, id, sessionId };
    const identity = `${sessionId}:${id}`;
    if (!records.has(identity) && !isDiffCommentDeleted(storage, base, sessionId, id)) {
      try {
        persistRecord(storage, diffCommentKey(base, sessionId, id), {
          ...normalized,
          rev: 1,
          updated: normalized.createdAt || Date.now(),
        });
      } catch {
        // Keep the legacy record readable in memory. The aggregate remains
        // untouched so migration can be retried after storage pressure clears.
      }
      records.set(identity, normalized);
    }
  });
  if (legacy.length) writeJSON(storage, `${base}:migration:v2`, { version: 2 });
  return [...records.values()];
}

export function persistDiffComment(storage: Storage, base: string, comment: DiffComment): void {
  if (!comment.id || !comment.sessionId)
    throw new Error('A queued comment needs a stable identity.');
  const key = diffCommentKey(base, comment.sessionId, comment.id);
  storage.removeItem(diffCommentTombstoneKey(base, comment.sessionId, comment.id));
  const existing = readJSON<Record<string, unknown> | null>(storage, key, null);
  const existingRev = Number(existing?.rev) || 0;
  const existingComment = normalizeQueuedDiffComment(existing, comment.sessionId);
  const comparable = (value: DiffComment | null): string =>
    JSON.stringify(
      value
        ? {
            ...value,
            rev: undefined,
            updatedAt: undefined,
            optimistic: undefined,
          }
        : null,
    );
  if (
    existing &&
    comment.rev !== undefined &&
    existingRev > comment.rev &&
    comparable(existingComment) !== comparable(comment)
  )
    throw new Error(
      `Comment conflict for ${comment.path}:${comment.line}: another tab saved a newer revision. Reload before choosing a version.`,
    );
  const rev = Math.max(existingRev, comment.rev || 0) + 1;
  const updated = Date.now();
  persistRecord(storage, key, { ...comment, rev, updated });
  comment.rev = rev;
  comment.updatedAt = updated;
}

export function removeDiffComment(
  storage: Storage,
  base: string,
  sessionId: string,
  commentId: string,
): void {
  persistRecord(storage, diffCommentTombstoneKey(base, sessionId, commentId), {
    deletedAt: Date.now(),
  });
  storage.removeItem(diffCommentKey(base, sessionId, commentId));
}

export function clearSessionDiffComments(storage: Storage, base: string, sessionId: string): void {
  const prefix = `${base}:${encodeURIComponent(sessionId)}:`;
  storageKeysWithPrefix(storage, prefix).forEach((key) => {
    const value = normalizeQueuedDiffComment(readJSON(storage, key, null), sessionId);
    if (value?.id) {
      persistRecord(storage, diffCommentTombstoneKey(base, sessionId, value.id), {
        deletedAt: Date.now(),
      });
    }
    storage.removeItem(key);
  });
}

export interface PendingIntent {
  id: string;
  clientMessageId: string;
  content: string;
  created: number;
  attachments?: Attachment[];
  state?: string;
}
export type PendingIntentRegistry = Record<string, PendingIntent[]>;

export function pendingIntentKey(base: string, sessionId: string, clientMessageId: string): string {
  return `${base}:${encodeURIComponent(sessionId)}:${encodeURIComponent(clientMessageId)}`;
}

export function readPendingIntents(storage: Storage, base: string): PendingIntentRegistry {
  const registry = readJSON<PendingIntentRegistry>(storage, base, {});
  for (let index = 0; index < storage.length; index += 1) {
    const key = storage.key(index) || '';
    if (!key.startsWith(`${base}:`)) continue;
    const intent = readJSON<PendingIntent | null>(storage, key, null);
    if (!intent?.clientMessageId) continue;
    const encodedSession = key.slice(base.length + 1).split(':')[0] || '';
    let sessionId: string;
    try {
      sessionId = decodeURIComponent(encodedSession);
    } catch {
      continue;
    }
    registry[sessionId] = [
      ...(registry[sessionId] || []).filter(
        (entry) => entry.clientMessageId !== intent.clientMessageId,
      ),
      intent,
    ];
  }
  for (const intents of Object.values(registry))
    intents.sort((left, right) => left.created - right.created);
  return registry;
}

export function persistPendingIntent(
  storage: Storage,
  base: string,
  sessionId: string,
  intent: PendingIntent,
): void {
  persistRecord(storage, pendingIntentKey(base, sessionId, intent.clientMessageId), intent);
}

export function removeSessionPendingIntents(
  storage: Storage,
  base: string,
  sessionId: string,
): void {
  const prefix = `${base}:${encodeURIComponent(sessionId)}:`;
  const keys: string[] = [];
  for (let index = 0; index < storage.length; index += 1) {
    const key = storage.key(index);
    if (key?.startsWith(prefix)) keys.push(key);
  }
  keys.forEach((key) => storage.removeItem(key));
}

export function mergePendingIntents(
  current: PendingIntentRegistry,
  incoming: PendingIntentRegistry,
): PendingIntentRegistry {
  const merged: PendingIntentRegistry = { ...current };
  for (const [sessionId, intents] of Object.entries(incoming || {})) {
    const byID = new Map(
      (merged[sessionId] || []).map((intent) => [intent.clientMessageId, intent]),
    );
    for (const intent of intents || []) byID.set(intent.clientMessageId, intent);
    merged[sessionId] = [...byID.values()].sort((a, b) => a.created - b.created);
  }
  return merged;
}

export interface DraftMessage {
  sessionId: string;
  content: string;
  updated: number;
  rev?: number;
  attachments?: Attachment[];
  projectId?: string;
  provider?: string;
  model?: string;
  effort?: string;
  reasoningMode?: string;
  agent?: string;
  worktreeDir?: string;
}

const MAX_DRAFTS = 10;
export function draftKey(base: string, sessionId: string): string {
  return `${base}:${encodeURIComponent(sessionId)}`;
}

interface DraftTombstone {
  sessionId: string;
  deleted: true;
  updated: number;
  rev: number;
}

type DraftStorageRecord = DraftMessage | DraftTombstone;

const validDraft = (value: unknown): value is DraftMessage =>
  Boolean(value) &&
  typeof value === 'object' &&
  (value as { deleted?: unknown }).deleted !== true &&
  typeof (value as DraftMessage).sessionId === 'string';

const validDraftRecord = (value: unknown): value is DraftStorageRecord =>
  validDraft(value) ||
  (Boolean(value) &&
    typeof value === 'object' &&
    (value as DraftTombstone).deleted === true &&
    typeof (value as DraftTombstone).sessionId === 'string');

export function readDrafts(storage: Storage, base: string): DraftMessage[] {
  const records = new Map<string, DraftMessage>();
  const seen = new Set<string>();
  for (const key of storageKeysWithPrefix(storage, `${base}:`)) {
    if (key === `${base}:migration:v2`) continue;
    const value = readJSON<unknown>(storage, key, null);
    if (!validDraftRecord(value)) continue;
    seen.add(value.sessionId);
    if (validDraft(value)) records.set(value.sessionId, value);
  }
  const legacy = readJSON<unknown[]>(storage, base, []).filter(validDraft);
  let migrationComplete = true;
  for (const draft of legacy) {
    if (seen.has(draft.sessionId)) continue;
    const migrated = { ...draft, rev: draft.rev || 1 };
    try {
      persistRecord(storage, draftKey(base, draft.sessionId), migrated);
    } catch {
      // Preserve compatibility data in memory and retry migration after storage
      // pressure/private-mode failures are resolved.
      migrationComplete = false;
    }
    records.set(draft.sessionId, migrated);
  }
  if (legacy.length && migrationComplete) {
    try {
      writeJSON(storage, `${base}:migration:v2`, { version: 2 });
    } catch {
      // Marker failure is retryable; successfully migrated records remain valid.
    }
  }
  return [...records.values()].sort((left, right) => right.updated - left.updated);
}

export function saveDraft(storage: Storage, base: string, draft: DraftMessage): DraftMessage[] {
  const key = draftKey(base, draft.sessionId);
  const existing = readJSON<DraftStorageRecord | null>(storage, key, null);
  if (
    existing &&
    !('deleted' in existing && existing.deleted) &&
    draft.rev !== undefined &&
    (existing.rev || 0) > draft.rev &&
    JSON.stringify({ ...existing, rev: 0, updated: 0 }) !==
      JSON.stringify({ ...draft, rev: 0, updated: 0 })
  )
    throw new Error(
      `Draft conflict for ${draft.sessionId}: another tab saved a newer revision. Reload before choosing which version to keep.`,
    );
  if (draft.content.trim() || draft.attachments?.length || draft.projectId || draft.worktreeDir) {
    // Persist the new record before retention GC so pressure can never remove
    // the previous durable intent before the replacement is safe.
    persistRecord(storage, key, {
      ...draft,
      updated: Date.now(),
      rev: Math.max(existing?.rev || 0, draft.rev || 0) + 1,
    });
  } else {
    persistRecord(storage, key, {
      sessionId: draft.sessionId,
      deleted: true,
      updated: Date.now(),
      rev: Math.max(existing?.rev || 0, draft.rev || 0) + 1,
    } satisfies DraftTombstone);
  }

  const records = readDrafts(storage, base);
  const retainedProjects = new Set<string>();
  const retained = new Set<string>();
  for (const record of records) {
    if (retained.size < MAX_DRAFTS || (record.projectId && !retainedProjects.has(record.projectId)))
      retained.add(record.sessionId);
    if (record.projectId) retainedProjects.add(record.projectId);
  }
  for (const record of records) {
    if (retained.has(record.sessionId)) continue;
    const recordKey = draftKey(base, record.sessionId);
    const existingRecord = readJSON<DraftStorageRecord | null>(storage, recordKey, null);
    persistRecord(storage, recordKey, {
      sessionId: record.sessionId,
      deleted: true,
      updated: Date.now(),
      rev: Math.max(existingRecord?.rev || 0, record.rev || 0) + 1,
    } satisfies DraftTombstone);
  }
  return records.filter((record) => retained.has(record.sessionId));
}

export function clearDraft(storage: Storage, base: string, sessionId: string): void {
  const key = draftKey(base, sessionId);
  const existing = readJSON<DraftStorageRecord | null>(storage, key, null);
  persistRecord(storage, key, {
    sessionId,
    deleted: true,
    updated: Date.now(),
    rev: (existing?.rev || 0) + 1,
  } satisfies DraftTombstone);
}
