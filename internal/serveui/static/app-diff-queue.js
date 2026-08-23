(() => {
'use strict';
const app = window.TermLLMApp || (window.TermLLMApp = {});
const { state, elements, STORAGE_KEYS } = app;
Object.assign(elements, {
  diffQueueBar: document.getElementById?.('diffQueueBar'),
  diffQueueStatus: document.getElementById?.('diffQueueStatus')
});
const MAX_QUEUED_DIFF_COMMENTS = 20;
const MAX_QUEUED_DIFF_COMMENT_BYTES = 32 * 1024;
const MAX_QUEUED_DIFF_COMMENT_AGGREGATE_BYTES = 240 * 1024;
const queueBySession = new Map();
const resolvedSessionID = (sessionId) => {
  const id = String(sessionId || '').trim();
  if (!id) return '';
  if (typeof app.isSessionIdentityResolved === 'function' && !app.isSessionIdentityResolved(id)) return '';
  return id;
};

const queueState = (sessionId) => {
  const id = String(sessionId || '').trim();
  let value = queueBySession.get(id);
  if (!value) {
    value = { mode: 'send', items: [], sending: false, discardArmedUntil: 0, discardTimer: null };
    queueBySession.set(id, value);
  }
  return value;
};

const utf8ByteLength = (value) => {
  const text = String(value || '');
  let bytes = 0;
  for (let index = 0; index < text.length; index += 1) {
    const code = text.charCodeAt(index);
    if (code < 0x80) bytes += 1;
    else if (code < 0x800) bytes += 2;
    else if (code >= 0xd800 && code <= 0xdbff && index + 1 < text.length
        && text.charCodeAt(index + 1) >= 0xdc00 && text.charCodeAt(index + 1) <= 0xdfff) {
      bytes += 4;
      index += 1;
    } else bytes += 3;
  }
  return bytes;
};

const queuedCommentBytes = (comment) => {
  try { return utf8ByteLength(JSON.stringify(comment)); } catch { return Infinity; }
};

const normalizeQueuedComment = (entry) => {
  const comment = app.normalizeDiffComment?.(entry);
  if (!comment) return null;
  return { ...comment, optimistic: false, created_at: Number(entry?.created_at || comment.created_at) || Date.now() };
};

const queueAggregateBytes = (items) => (items || []).reduce((total, item) => total + queuedCommentBytes(item), 0);
const boundedQueuedItems = (entries) => {
  const items = [];
  let aggregateBytes = 0;
  for (const entry of Array.isArray(entries) ? entries : []) {
    const item = normalizeQueuedComment(entry);
    if (!item) continue;
    const bytes = queuedCommentBytes(item);
    if (bytes > MAX_QUEUED_DIFF_COMMENT_BYTES || items.length >= MAX_QUEUED_DIFF_COMMENTS) continue;
    if (aggregateBytes + bytes > MAX_QUEUED_DIFF_COMMENT_AGGREGATE_BYTES) continue;
    items.push(item);
    aggregateBytes += bytes;
  }
  return items;
};

const storagePayload = () => {
  const sessions = {};
  queueBySession.forEach((queue, sessionId) => {
    if (queue.items.length > 0 || queue.mode !== 'send') sessions[sessionId] = { mode: queue.mode, items: queue.items };
  });
  return JSON.stringify({ v: 1, sessions });
};

const persistQueues = () => {
  const key = STORAGE_KEYS?.diffCommentQueue;
  if (!key) return false;
  try {
    localStorage.setItem(key, storagePayload());
    return true;
  } catch {
    app.showToast?.('Queued inline comments could not be saved in this browser. Free storage and try again.', { id: 'diff-comment-queue-storage', tone: 'warning' });
    return false;
  }
};

const loadQueues = () => {
  try {
    const raw = localStorage.getItem(STORAGE_KEYS?.diffCommentQueue || '');
    if (!raw) return;
    const parsed = JSON.parse(raw);
    if (parsed?.v !== 1 || !parsed.sessions || typeof parsed.sessions !== 'object') return;
    Object.entries(parsed.sessions).forEach(([sessionId, value]) => {
      const storedItems = Array.isArray(value?.items) ? value.items : [];
      const items = boundedQueuedItems(storedItems);
      if (items.length < storedItems.length) {
        app.showToast?.('Some saved inline comments exceeded the safe queue limits and were not restored.', { id: 'diff-comment-queue-load-limit', tone: 'warning' });
      }
      queueBySession.set(sessionId, { mode: value?.mode === 'queue' ? 'queue' : 'send', items, sending: false, discardArmedUntil: 0, discardTimer: null });
    });
  } catch {}
};

const queuedDiffComments = (sessionId, anchor = null) => {
  const items = queueBySession.get(String(sessionId || '').trim())?.items || [];
  if (!anchor) return items.slice();
  const key = app.anchorKey?.(anchor);
  return items.filter((item) => app.anchorKey?.(item) === key);
};

const diffCommentSendMode = (sessionId) => queueBySession.get(String(sessionId || '').trim())?.mode || 'send';
const setDiffCommentSendMode = (sessionId, mode) => {
  const id = resolvedSessionID(sessionId);
  if (!id) return 'send';
  const queue = queueState(id);
  queue.mode = mode === 'queue' ? 'queue' : 'send';
  persistQueues();
  return queue.mode;
};

const bumpPaths = (sessionId, items) => {
  new Set((items || []).map((item) => item.path).filter(Boolean)).forEach((path) => app.bumpDiffCommentPathRevision?.(sessionId, path));
};

const refreshQueueUI = (sessionId, changedItems = []) => {
  bumpPaths(sessionId, changedItems);
  app.renderDiffSidebar?.(sessionId);
  if (sessionId === state.activeSessionId) renderDiffCommentQueueBar(sessionId);
};

const queueDiffComment = (sessionId, entry) => {
  const id = resolvedSessionID(sessionId);
  if (!id) {
    app.showToast?.('Wait for this conversation to finish starting before queueing an inline comment.', { id: 'diff-comment-queue-session', tone: 'warning' });
    return false;
  }
  const comment = normalizeQueuedComment(entry);
  if (!comment) return false;
  if (queuedCommentBytes(comment) > MAX_QUEUED_DIFF_COMMENT_BYTES) {
    app.showToast?.('This inline comment is too large to queue safely. Shorten it and try again.', { id: 'diff-comment-queue-size', tone: 'warning' });
    return false;
  }
  const queue = queueState(id);
  if (queue.sending) {
    app.showToast?.('Wait for the queued inline comments to finish sending.', { id: 'diff-comment-queue-sending', tone: 'warning' });
    return false;
  }
  if (queue.items.some((item) => item.id === comment.id)) return true;
  if (queue.items.length >= MAX_QUEUED_DIFF_COMMENTS) {
    app.showToast?.('Queue is full (20). Send or discard queued comments first.', { id: 'diff-comment-queue-full', tone: 'warning' });
    return false;
  }
  if (queueAggregateBytes(queue.items) + queuedCommentBytes(comment) > MAX_QUEUED_DIFF_COMMENT_AGGREGATE_BYTES) {
    app.showToast?.('Queued inline comments are at the safe batch-size limit. Send or discard them before adding another.', { id: 'diff-comment-queue-aggregate-size', tone: 'warning' });
    return false;
  }
  queue.items.push(comment);
  if (!persistQueues()) {
    queue.items.pop();
    return false;
  }
  refreshQueueUI(id, [comment]);
  return true;
};

const removeQueuedDiffComment = (sessionId, commentId) => {
  const id = resolvedSessionID(sessionId);
  if (!id) return null;
  const queue = queueState(id);
  const index = queue.items.findIndex((item) => item.id === commentId);
  if (index < 0 || queue.sending) return null;
  const [removed] = queue.items.splice(index, 1);
  persistQueues();
  refreshQueueUI(id, [removed]);
  return removed;
};

const pruneDiffCommentQueues = (retainedIDs) => {
  const retained = retainedIDs instanceof Set ? retainedIDs : new Set(retainedIDs || []);
  let changed = false;
  for (const sessionId of queueBySession.keys()) {
    if (!retained.has(sessionId)) {
      const queue = queueBySession.get(sessionId);
      if (queue?.discardTimer) clearTimeout(queue.discardTimer);
      queueBySession.delete(sessionId);
      changed = true;
    }
  }
  if (changed) persistQueues();
};

const makeBatchID = () => {
  try { if (globalThis.crypto?.randomUUID) return `diff_comment_batch_${globalThis.crypto.randomUUID()}`; } catch {}
  return `diff_comment_batch_${Date.now()}_${Math.random().toString(36).slice(2)}`;
};

const buildBatchPrompt = (items) => {
  const blocks = items.map((item) => app.formatDiffCommentInstruction?.(item) || item.instruction);
  return items.length === 1 ? blocks[0] : `[Inline diff instructions] (${items.length} anchored comments)\n\n${blocks.join('\n\n')}`;
};

const diffCommentQueueSending = (sessionId) => Boolean(queueBySession.get(String(sessionId || '').trim())?.sending);
const focusBeforeQueueBarHides = () => {
  const bar = elements.diffQueueBar;
  if (!bar?.contains?.(document.activeElement)) return;
  const candidates = [elements.diffMaximizeBtn, elements.diffFilterInput, elements.diffToggleBtn];
  const target = candidates.find((element) => element && !element.hidden && element.getAttribute?.('hidden') === null);
  target?.focus?.();
};

const restoreFailedQueue = (sessionId, snapshot, mode) => {
  const queue = queueState(sessionId);
  const present = new Set(queue.items.map((item) => item.id));
  const merged = [...snapshot.filter((item) => !present.has(item.id)), ...queue.items];
  queue.items = boundedQueuedItems(merged);
  if (queue.items.length < merged.length) {
    app.showToast?.('Some newer queued comments could not be restored because the safe queue limit was reached.', { id: 'diff-comment-queue-restore-limit', tone: 'warning' });
  }
  queue.mode = mode;
  queue.sending = false;
  persistQueues();
  refreshQueueUI(sessionId, snapshot);
};

const sendQueuedDiffComments = async (sessionId = state.activeSessionId) => {
  const id = resolvedSessionID(sessionId);
  if (!id) {
    app.showToast?.('Wait for this conversation to finish starting before sending queued inline comments.', { id: 'diff-comment-batch-session', tone: 'warning' });
    return false;
  }
  const queue = queueState(id);
  if (queue.sending || queue.items.length === 0) return false;
  if (!state.connected) {
    app.openAuthModal?.('Connect before sending queued inline instructions.', true);
    return false;
  }
  if (state.branchContextQueuedSend) {
    app.showToast?.('A message is already queued for this conversation path.', { id: 'diff-comment-batch-send', tone: 'warning' });
    return false;
  }
  const snapshot = queue.items.slice();
  const previousMode = queue.mode;
  const batchId = makeBatchID();
  queue.sending = true;
  if (id === state.activeSessionId) renderDiffCommentQueueBar(id);
  snapshot.forEach((item) => app.addOptimisticDiffComment?.(id, { ...item, client_message_id: batchId }));
  let transportStarted = false;
  let transportQueued = false;
  let transportFailed = false;
  let failureHandled = false;
  const restoreBatch = (tone = 'error', error = null) => {
    if (failureHandled) return;
    failureHandled = true;
    transportFailed = true;
    snapshot.forEach((item) => app.removeOptimisticDiffComment?.(id, item.id));
    restoreFailedQueue(id, snapshot, previousMode);
    const detail = String(error?.message || '').trim();
    const message = detail
      ? `Queued comments were not sent: ${detail}`
      : 'Queued comments were not sent. They are still queued.';
    app.showToast?.(message, { id: 'diff-comment-batch-send', tone });
  };
  const clearAcceptedQueue = (detail = {}) => {
    if (detail.queued) {
      transportQueued = true;
      return;
    }
    if (transportStarted) return;
    transportStarted = true;
    const accepted = queue.items.filter((item) => snapshot.some((sent) => sent.id === item.id));
    const remaining = queue.items.filter((item) => !snapshot.some((sent) => sent.id === item.id));
    if (remaining.length === 0) focusBeforeQueueBarHides();
    queue.items = remaining;
    queue.mode = 'send';
    queue.sending = false;
    persistQueues();
    refreshQueueUI(id, accepted);
    void app.hydrateDiffComments?.(id, { force: true });
  };
  try {
    await app.sendMessage?.({
      prompt: buildBatchPrompt(snapshot),
      displayPrompt: snapshot.length === 1 ? snapshot[0].instruction : `${snapshot.length} inline comments`,
      contentParts: snapshot.map((item) => ({ type: 'diff_comment', diff_comment: app.diffCommentPayload(item) })),
      diffComment: snapshot.length === 1 ? snapshot[0] : null,
      diffComments: snapshot,
      attachments: [],
      preserveComposer: true,
      reuseMessageId: batchId,
      _onTransportStarted: clearAcceptedQueue,
      _onTransportFailed(error) { restoreBatch('error', error); },
      _onTransportCanceled(error) { restoreBatch('warning', error); }
    });
  } catch {
    restoreBatch();
  }
  if (transportQueued && !transportStarted && !transportFailed) return true;
  if (!transportStarted || transportFailed) {
    restoreBatch();
    return false;
  }
  return true;
};

const discardQueuedDiffComments = (sessionId = state.activeSessionId, force = false) => {
  const id = resolvedSessionID(sessionId);
  if (!id) return false;
  const queue = queueState(id);
  if (queue.sending || queue.items.length === 0) return false;
  const now = Date.now();
  if (!force && queue.discardArmedUntil < now) {
    queue.discardArmedUntil = now + 4000;
    if (id === state.activeSessionId) renderDiffCommentQueueBar(id);
    if (queue.discardTimer) clearTimeout(queue.discardTimer);
    queue.discardTimer = setTimeout(() => {
      queue.discardTimer = null;
      queue.discardArmedUntil = 0;
      if (id === state.activeSessionId) renderDiffCommentQueueBar(id);
    }, 4000);
    return false;
  }
  focusBeforeQueueBarHides();
  const removed = queue.items.splice(0);
  queue.mode = 'send';
  queue.discardArmedUntil = 0;
  if (queue.discardTimer) clearTimeout(queue.discardTimer);
  queue.discardTimer = null;
  persistQueues();
  refreshQueueUI(id, removed);
  return true;
};

const queuedSummaryTitle = (items) => items.map((item) => {
  const words = item.instruction.trim().split(/\s+/).slice(0, 8).join(' ');
  return `${item.path}:${item.line} — ${words}`;
}).join('\n');
const renderToggleQueueAccent = (items) => {
  const button = elements.diffToggleBtn;
  if (!button) return;
  const count = items.length;
  button.classList?.toggle?.('has-queued', count > 0);
  const suffixPattern = / · \d+ queued comments? not sent$/;
  const baseTitle = String(button.title || '').replace(suffixPattern, '');
  const baseAria = String(button.getAttribute?.('aria-label') || 'Toggle file changes').replace(suffixPattern, '');
  const suffix = count > 0 ? ` · ${count} queued comment${count === 1 ? '' : 's'} not sent` : '';
  button.title = `${baseTitle}${suffix}`;
  button.setAttribute?.('aria-label', `${baseAria}${suffix}`);
};

const renderDiffCommentQueueBar = (sessionId = state.activeSessionId) => {
  const id = String(sessionId || '');
  const queue = queueBySession.get(id) || { items: [], sending: false, discardArmedUntil: 0 };
  const items = queue.items;
  renderToggleQueueAccent(items);
  const status = elements.diffQueueStatus;
  if (status) {
    const nextStatus = queue.sending
      ? `Sending ${items.length} queued inline comment${items.length === 1 ? '' : 's'}…`
      : (items.length > 0
          ? `${items.length} inline comment${items.length === 1 ? '' : 's'} queued, not sent.`
          : 'No inline comments queued.');
    if (status.textContent !== nextStatus) status.textContent = nextStatus;
  }
  const bar = elements.diffQueueBar;
  if (!bar) return;
  bar.hidden = items.length === 0;
  if (bar.hidden) { bar.setAttribute?.('hidden', ''); return; }
  bar.removeAttribute?.('hidden');
  const count = bar.querySelector?.('.diff-queue-count');
  const send = bar.querySelector?.('.diff-queue-send');
  const discard = bar.querySelector?.('.diff-queue-discard');
  if (count) {
    count.textContent = `${items.length} queued`;
    count.title = queuedSummaryTitle(items);
  }
  if (send) {
    send.disabled = queue.sending;
    send.textContent = queue.sending ? 'Sending…' : `Send ${items.length} comment${items.length === 1 ? '' : 's'}`;
  }
  if (discard) {
    discard.disabled = queue.sending;
    discard.textContent = queue.discardArmedUntil >= Date.now() ? `Discard ${items.length}?` : 'Discard';
  }
};
loadQueues();
elements.diffQueueBar?.querySelector?.('.diff-queue-send')?.addEventListener?.('click', () => { void sendQueuedDiffComments(); });
elements.diffQueueBar?.querySelector?.('.diff-queue-discard')?.addEventListener?.('click', () => { discardQueuedDiffComments(); });
Object.assign(app, {
  MAX_QUEUED_DIFF_COMMENTS,
  MAX_QUEUED_DIFF_COMMENT_BYTES,
  MAX_QUEUED_DIFF_COMMENT_AGGREGATE_BYTES,
  diffCommentSendMode,
  diffCommentQueueSending,
  setDiffCommentSendMode,
  queuedDiffComments,
  queueDiffComment,
  removeQueuedDiffComment,
  pruneDiffCommentQueues,
  sendQueuedDiffComments,
  discardQueuedDiffComments,
  renderDiffCommentQueueBar,
  buildDiffCommentBatchPrompt: buildBatchPrompt
});
})();
