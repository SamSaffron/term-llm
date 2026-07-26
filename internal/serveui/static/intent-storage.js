'use strict';

(function initIntentStorage() {
const app = window.TermLLMApp;
const { STORAGE_KEYS } = app;

const PENDING_INTENT_LIMIT = 256;
const LEGACY_INTENT_STORAGE_MARKER = ':intent:';

const transcriptIntentStoragePrefix = (sessionId = '') => (
  `${STORAGE_KEYS.pendingIntents}:${encodeURIComponent(String(sessionId || ''))}:`
);
const legacyTranscriptIntentStoragePrefix = (sessionId = '') => (
  `${STORAGE_KEYS.optimisticTranscript}${LEGACY_INTENT_STORAGE_MARKER}${encodeURIComponent(String(sessionId || ''))}:`
);

const transcriptIntentStorageKey = (sessionId, clientMessageId) => (
  `${transcriptIntentStoragePrefix(sessionId)}${encodeURIComponent(String(clientMessageId || ''))}`
);

const localStorageKeys = () => {
  const keys = [];
  const length = Math.max(0, Number(localStorage.length) || 0);
  for (let index = 0; index < length; index += 1) {
    const key = localStorage.key?.(index);
    if (key) keys.push(String(key));
  }
  return keys;
};

const isClientOwnedIntentEntry = (entry) => (
  window.TermLLMConversation?.transcriptIsClientOwnedIntent?.(entry) === true
);

const normalizeLegacyIntent = (entry) => {
  if (!isClientOwnedIntentEntry(entry)) return null;
  const clientMessageId = String(entry.clientMessageId || entry.client_message_id || entry.clientKey || entry.id || '').trim();
  return clientMessageId ? { ...entry, clientMessageId } : null;
};

const decodeLegacyPendingIntentSessions = (sessions) => {
  const migrated = {};
  for (const [sessionId, entries] of Object.entries(sessions || {})) {
    if (!Array.isArray(entries)) continue;
    const intent = entries.map(normalizeLegacyIntent).filter(Boolean);
    if (intent.length > 0) migrated[sessionId] = intent;
  }
  return migrated;
};

const readPendingIntentRegistry = () => {
  const migrated = {};
  try {
    const saved = JSON.parse(localStorage.getItem(STORAGE_KEYS.optimisticTranscript) || 'null');
    const sessions = saved?.sessions && typeof saved.sessions === 'object'
      ? saved.sessions
      : (saved?.sessionId && Array.isArray(saved.entries) ? { [saved.sessionId]: saved.entries } : null);
    if (sessions) Object.assign(migrated, decodeLegacyPendingIntentSessions(sessions));
  } catch {
    // Legacy optimistic recovery is best-effort.
  }
  try {
    const prefixes = [
      `${STORAGE_KEYS.pendingIntents}:`,
      `${STORAGE_KEYS.optimisticTranscript}${LEGACY_INTENT_STORAGE_MARKER}`,
    ];
    for (const key of localStorageKeys()) {
      const basePrefix = prefixes.find((prefix) => key.startsWith(prefix));
      if (!basePrefix) continue;
      const suffix = key.slice(basePrefix.length);
      const separator = suffix.indexOf(':');
      if (separator < 0) continue;
      const sessionId = decodeURIComponent(suffix.slice(0, separator));
       const rawEntry = JSON.parse(localStorage.getItem(key) || 'null');
       const entry = basePrefix.startsWith(`${STORAGE_KEYS.optimisticTranscript}${LEGACY_INTENT_STORAGE_MARKER}`)
         ? normalizeLegacyIntent(rawEntry)
         : rawEntry;
       if (!sessionId || !isClientOwnedIntentEntry(entry)) continue;
       if (!Array.isArray(migrated[sessionId])) migrated[sessionId] = [];
       const clientMessageId = String(entry.clientMessageId || entry.client_message_id || '').trim();
      if (!clientMessageId) continue;
      const existing = migrated[sessionId].findIndex((candidate) => String(candidate?.clientMessageId || candidate?.client_message_id || candidate?.id || '').trim() === clientMessageId);
      if (existing >= 0) migrated[sessionId][existing] = entry;
      else migrated[sessionId].push(entry);
    }
  } catch {
    // Per-intent recovery is best-effort; durable transcript bodies never live here.
  }
  return migrated;
};

const rekeyPendingIntentStorage = (previousId, nextId) => {
  if (!previousId || !nextId || previousId === nextId) return;
  try {
    const previousPrefixes = [transcriptIntentStoragePrefix(previousId), legacyTranscriptIntentStoragePrefix(previousId)];
    for (const key of localStorageKeys()) {
      if (!previousPrefixes.some((prefix) => key.startsWith(prefix))) continue;
      const entry = JSON.parse(localStorage.getItem(key) || 'null');
      const clientMessageId = String(entry?.clientMessageId || entry?.client_message_id || entry?.id || '').trim();
      if (clientMessageId && isClientOwnedIntentEntry(entry)) {
        localStorage.setItem(transcriptIntentStorageKey(nextId, clientMessageId), JSON.stringify(entry));
      }
      localStorage.removeItem(key);
    }
    const sessions = readPendingIntentRegistry();
    for (const entry of sessions[previousId] || []) {
      const clientMessageId = String(entry?.clientMessageId || entry?.client_message_id || entry?.id || '').trim();
      if (clientMessageId && isClientOwnedIntentEntry(entry)) {
        localStorage.setItem(transcriptIntentStorageKey(nextId, clientMessageId), JSON.stringify({ ...entry, clientMessageId }));
      }
    }
    const saved = JSON.parse(localStorage.getItem(STORAGE_KEYS.optimisticTranscript) || 'null');
    if (saved?.sessions && typeof saved.sessions === 'object' && Object.prototype.hasOwnProperty.call(saved.sessions, previousId)) {
      delete saved.sessions[previousId];
      if (Object.keys(saved.sessions).length === 0) localStorage.removeItem(STORAGE_KEYS.optimisticTranscript);
      else localStorage.setItem(STORAGE_KEYS.optimisticTranscript, JSON.stringify(saved));
    }
  } catch {
    // Identity reconciliation must succeed even when storage is unavailable.
  }
};

const removePendingIntentStorage = (sessionId, clientMessageId) => {
  const id = String(clientMessageId || '').trim();
  if (!sessionId || !id) return;
  try {
    localStorage.removeItem(transcriptIntentStorageKey(sessionId, id));
    localStorage.removeItem(`${legacyTranscriptIntentStoragePrefix(sessionId)}${encodeURIComponent(id)}`);
  } catch {
    // Conversation ownership cannot depend on storage availability.
  }
};

const persistPendingIntents = (session) => {
  const transcript = session?.transcript;
  if (!session?.id) return;
  try {
    const entries = [...(transcript?.conversation?.intents?.values?.() || [])]
      .filter((entry) => !entry?.transient && isClientOwnedIntentEntry(entry))
      .slice(-PENDING_INTENT_LIMIT)
      .map((entry) => ({ ...entry, clientMessageId: String(entry.clientMessageId || entry.client_message_id || entry.id || '').trim() }))
      .filter((entry) => entry.clientMessageId) || [];
    for (const entry of entries) {
      const key = transcriptIntentStorageKey(session.id, entry.clientMessageId);
      localStorage.setItem(key, JSON.stringify(entry));
    }
    for (const message of transcript?.publishedMessages || []) {
      if (message?.durable !== true || message?.role !== 'user') continue;
      removePendingIntentStorage(session.id, message.clientMessageId || message.client_message_id);
    }
    for (const key of localStorageKeys()) {
      if (key.startsWith(legacyTranscriptIntentStoragePrefix(session.id))) localStorage.removeItem(key);
    }

    // The shared v2 registry is migration input only. Remove this session from it
    // without writing current intent back into a cross-tab last-writer-wins blob.
    const saved = JSON.parse(localStorage.getItem(STORAGE_KEYS.optimisticTranscript) || 'null');
    if (saved?.sessions && typeof saved.sessions === 'object' && Object.prototype.hasOwnProperty.call(saved.sessions, session.id)) {
      delete saved.sessions[session.id];
      if (Object.keys(saved.sessions).length === 0) localStorage.removeItem(STORAGE_KEYS.optimisticTranscript);
      else localStorage.setItem(STORAGE_KEYS.optimisticTranscript, JSON.stringify(saved));
    }
  } catch {
    // Storage pressure must not interrupt a response.
  }
};


Object.assign(app, {
  transcriptIntentStoragePrefix,
  transcriptIntentStorageKey,
  localStorageKeys,
  isClientOwnedIntentEntry,
  decodeLegacyPendingIntentSessions,
  readPendingIntentRegistry,
  rekeyPendingIntentStorage,
  removePendingIntentStorage,
  persistPendingIntents
});
window.TermLLMConversation.initEffects();
})();
