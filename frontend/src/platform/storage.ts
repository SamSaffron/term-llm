import type { Attachment } from '../domain/types';
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
  lastNotifiedResponseId: 'term_llm_last_notified_response_id',
  draftMessages: 'term_llm_draft_messages',
  pendingIntents: 'term_llm_pending_intent',
  diffCommentQueue: 'term_llm_diff_comment_queue',
  optimisticTranscript: 'term_llm_optimistic_transcript',
  projectExpansion: 'term_llm_project_expansion',
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
  writeJSON(storage, pendingIntentKey(base, sessionId, intent.clientMessageId), intent);
}

export function removePendingIntent(
  storage: Storage,
  base: string,
  sessionId: string,
  clientMessageId: string,
): void {
  storage.removeItem(pendingIntentKey(base, sessionId, clientMessageId));
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
export function readDrafts(storage: Storage, key: string): DraftMessage[] {
  return readJSON<unknown[]>(storage, key, [])
    .filter(
      (value): value is DraftMessage =>
        Boolean(value) &&
        typeof value === 'object' &&
        typeof (value as DraftMessage).sessionId === 'string',
    )
    .sort((left, right) => right.updated - left.updated)
    .slice(0, MAX_DRAFTS);
}

export function saveDraft(storage: Storage, key: string, draft: DraftMessage): DraftMessage[] {
  const records = readDrafts(storage, key).filter((record) => record.sessionId !== draft.sessionId);
  if (draft.content.trim() || draft.attachments?.length || draft.projectId || draft.worktreeDir)
    records.unshift({ ...draft, updated: Date.now() });
  const bounded: DraftMessage[] = [];
  const retainedProjects = new Set<string>();
  for (const record of records) {
    if (
      bounded.length < MAX_DRAFTS ||
      (record.projectId && !retainedProjects.has(record.projectId))
    )
      bounded.push(record);
    if (record.projectId) retainedProjects.add(record.projectId);
  }
  writeJSON(storage, key, bounded);
  return bounded;
}

export function clearDraft(storage: Storage, key: string, sessionId: string): void {
  writeJSON(
    storage,
    key,
    readDrafts(storage, key).filter((record) => record.sessionId !== sessionId),
  );
}
