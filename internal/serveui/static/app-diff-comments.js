(() => {
'use strict';

const app = window.TermLLMApp || (window.TermLLMApp = {});
const { UI_PREFIX, state, normalizeDiffScope, isTurnDiffScope } = app;
const CONTEXT_RADIUS = 2;
const COMMENT_REFRESH_MS = 15_000;
const commentStateBySession = new Map();

const liveSessionIDs = () => new Set((state.sessions || []).map((session) => String(session?.id || '').trim()).filter(Boolean));
const pruneDiffCommentState = (retainedIDs = liveSessionIDs()) => {
  const retained = retainedIDs instanceof Set ? retainedIDs : new Set(retainedIDs || []);
  for (const sessionId of commentStateBySession.keys()) {
    if (!retained.has(sessionId)) commentStateBySession.delete(sessionId);
  }
  app.pruneDiffCommentQueues?.(retained);
};

const sessionCommentState = (sessionId) => {
  let value = commentStateBySession.get(sessionId);
  if (!value) {
    value = {
      comments: [], loaded: false, inflight: null, lastLoadedAt: 0, serverRevision: -1,
      revisionsByPath: new Map(), editorDrafts: new Map(), retryIDs: new Map(), openPanel: null, pendingFocusKey: ''
    };
    commentStateBySession.set(sessionId, value);
  }
  return value;
};

const normalizeComment = (entry) => {
  const raw = entry?.diff_comment || entry;
  if (!raw || typeof raw !== 'object') return null;
  const side = String(raw.side || '').toLowerCase();
  const scope = normalizeDiffScope(raw.scope);
  const line = Number(raw.line) || 0;
  const seq = Number(raw.file_change_seq) || 0;
  const id = String(raw.id || '').trim();
  const path = String(raw.path || '');
  const instruction = String(raw.instruction || '').trim();
  if (!id || !path || !instruction || !scope || (side !== 'old' && side !== 'new') || line <= 0 || seq < 0 || (isTurnDiffScope(scope) ? seq === 0 : seq !== 0)) return null;
  return {
    id,
    parent_id: String(raw.parent_id || ''),
    path,
    scope,
    side,
    line,
    file_change_seq: seq,
    line_text: String(raw.line_text ?? ''),
    context_before: Array.isArray(raw.context_before) ? raw.context_before : [],
    context_after: Array.isArray(raw.context_after) ? raw.context_after : [],
    instruction,
    client_message_id: String(entry?.client_message_id || raw.client_message_id || ''),
    created_at: Number(entry?.created_at || raw.created_at) || Date.now(),
    optimistic: Boolean(raw.optimistic)
  };
};

const diffCommentPayload = ({
  id, parent_id, path, scope, side, line, file_change_seq,
  line_text, context_before, context_after, instruction
}) => ({
  id, ...(parent_id ? { parent_id } : {}), path, scope, side, line, file_change_seq,
  line_text, context_before, context_after, instruction
});

const scopeOf = (value) => normalizeDiffScope(value?.scope) || 'last_turn';
const scopeLabel = (value) => scopeOf(value).replaceAll('_', ' ').replace(/^./, (letter) => letter.toUpperCase());
const anchorKey = (comment) => `${scopeOf(comment)}\u0000${String(comment?.path || '')}\u0000${String(comment?.side || '')}\u0000${Number(comment?.line) || 0}`;
// Turn-window anchors invalidate on any newer retained snapshot. Git scopes
// have no session sequence, so their captured line text is the identity signal.
const sameAnchorSnapshot = (comment, anchor) => {
  if (scopeOf(comment) !== scopeOf(anchor)) return false;
  return isTurnDiffScope(scopeOf(anchor))
    ? comment.file_change_seq === anchor.file_change_seq
    : comment.line_text === anchor.line_text;
};
const commentsForAnchor = (sessionId, anchor) => sessionCommentState(sessionId).comments
  .filter((comment) => anchorKey(comment) === anchorKey(anchor))
  .sort((a, b) => (a.created_at - b.created_at) || a.id.localeCompare(b.id));

const pathFingerprint = (comments, path) => JSON.stringify(comments
  .filter((comment) => comment.path === path)
  .map((comment) => [comment.id, comment.parent_id, comment.scope, comment.side, comment.line, comment.file_change_seq, comment.instruction, comment.optimistic])
  .sort((a, b) => String(a[0]).localeCompare(String(b[0]))));

const bumpChangedPathRevisions = (cs, before, after) => {
  const paths = new Set([...before.map((comment) => comment.path), ...after.map((comment) => comment.path)]);
  for (const path of paths) {
    if (pathFingerprint(before, path) === pathFingerprint(after, path)) continue;
    cs.revisionsByPath.set(path, (cs.revisionsByPath.get(path) || 0) + 1);
  }
};

const localCommentStillPending = (sessionId, comment) => {
  const identities = new Set([comment.id, comment.client_message_id].filter(Boolean));
  for (const collection of [state.pendingInterjections, state.pendingInterruptCommits, state.queuedInterrupts]) {
    if ((collection || []).some((entry) => entry?.sessionId === sessionId && identities.has(String(entry?.messageId || '')))) return true;
  }
  const session = (state.sessions || []).find((entry) => String(entry?.id || '') === sessionId);
  const messages = window.TermLLMConversation?.sessionMessages?.(session) || session?.messages || [];
  return messages.some((message) => {
    const identity = String(message?.clientMessageId || message?.id || '');
    return message?.role === 'user' && identities.has(identity) && !Number.isFinite(Number(message?.serverSeq));
  });
};

const replaceComments = (sessionId, incoming, serverRevision = -1) => {
  const cs = sessionCommentState(sessionId);
  const before = cs.comments;
  const merged = new Map();
  for (const comment of before) {
    if (comment.optimistic && localCommentStillPending(sessionId, comment)) merged.set(comment.id, comment);
  }
  for (const entry of Array.isArray(incoming) ? incoming : []) {
    const comment = normalizeComment(entry);
    if (comment) merged.set(comment.id, { ...comment, optimistic: false });
  }
  cs.comments = Array.from(merged.values());
  cs.loaded = true;
  cs.lastLoadedAt = Date.now();
  if (Number.isFinite(Number(serverRevision)) && Number(serverRevision) >= 0) cs.serverRevision = Number(serverRevision);
  bumpChangedPathRevisions(cs, before, cs.comments);
};

const currentSessionRevision = (sessionId) => {
  const session = (state.sessions || []).find((entry) => String(entry?.id || '') === String(sessionId));
  const revisions = [Number(session?.transcriptRev), Number(session?.transcript?.rev)].filter((value) => Number.isFinite(value) && value >= 0);
  return revisions.length > 0 ? Math.max(...revisions) : -1;
};

const hydrateDiffComments = (sessionId, options = {}) => {
  const id = String(sessionId || '').trim();
  if (!id) return Promise.resolve([]);
  pruneDiffCommentState(new Set([...liveSessionIDs(), id]));
  const cs = sessionCommentState(id);
  const expectedRevision = Number.isFinite(Number(options.revision)) ? Number(options.revision) : currentSessionRevision(id);
  const revisionAhead = expectedRevision >= 0 && expectedRevision > cs.serverRevision;
  const freshByTime = Date.now() - cs.lastLoadedAt < COMMENT_REFRESH_MS;
  if (!options.force && cs.loaded && freshByTime && !revisionAhead) return Promise.resolve(cs.comments);
  if (cs.inflight) return cs.inflight;
  const headers = typeof app.requestHeaders === 'function' ? app.requestHeaders(id) : (state.token ? { Authorization: `Bearer ${state.token}` } : {});
  cs.inflight = app.apiFetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(id)}/diff-comments`, { headers })
    .then(async (response) => {
      if (!response.ok) throw new Error(`Unable to load inline comments (${response.status})`);
      const payload = await response.json();
      replaceComments(id, payload?.comments, payload?.transcript_rev);
      if (id === state.activeSessionId) app.renderDiffSidebar?.(id);
      return cs.comments;
    })
    .catch(() => cs.comments)
    .finally(() => { cs.inflight = null; });
  return cs.inflight;
};

const invalidateDiffComments = (sessionId) => {
  const cs = commentStateBySession.get(String(sessionId || '').trim());
  if (!cs) return;
  cs.lastLoadedAt = 0;
  cs.serverRevision = -1;
};

const diffCommentRevision = (sessionId, path) => commentStateBySession.get(sessionId)?.revisionsByPath.get(path) || 0;
const bumpDiffCommentPathRevision = (sessionId, path) => {
  const revisions = sessionCommentState(sessionId).revisionsByPath;
  revisions.set(path, (revisions.get(path) || 0) + 1);
};
const diffCommentPanelOpen = (sessionId) => Boolean(commentStateBySession.get(String(sessionId || ''))?.openPanel);
const clearDiffCommentPanel = (sessionId, path = '') => {
  const cs = commentStateBySession.get(String(sessionId || ''));
  if (!cs?.openPanel || (path && cs.openPanel.path !== path)) return false;
  cs.openPanel = null;
  return true;
};
const reconcileDiffCommentPanel = (sessionId, retainedPaths) => {
  const cs = commentStateBySession.get(String(sessionId || ''));
  if (!cs?.openPanel) return false;
  const retained = retainedPaths instanceof Set ? retainedPaths : new Set(retainedPaths || []);
  return retained.has(cs.openPanel.path) ? false : clearDiffCommentPanel(sessionId);
};

const rowAnchor = (path, row, fileChangeSeq, scope = 'last_turn') => {
  scope = normalizeDiffScope(scope) || 'last_turn';
  const side = row?.type === 'del' ? 'old' : (row?.newNo ? 'new' : 'old');
  return {
    path, scope,
    side,
    line: Number(side === 'old' ? row?.oldNo : row?.newNo) || 0,
    file_change_seq: isTurnDiffScope(scope) ? (Number(fileChangeSeq) || 0) : 0,
    line_text: String(row?.text ?? '')
  };
};

const contextLine = (row) => {
  if (!row || row.type === 'hunk') return null;
  const side = row.type === 'del' ? 'old' : (row.newNo ? 'new' : 'old');
  const line = Number(side === 'old' ? row.oldNo : row.newNo) || 0;
  return line > 0 ? { side, line, text: String(row.text ?? '') } : null;
};

const captureContext = (rows, rowIndex) => {
  const before = [], after = [];
  for (let index = rowIndex - 1; index >= 0 && before.length < CONTEXT_RADIUS; index -= 1) {
    if (rows[index]?.type === 'hunk') break;
    const line = contextLine(rows[index]);
    if (line) before.unshift(line);
  }
  for (let index = rowIndex + 1; index < rows.length && after.length < CONTEXT_RADIUS; index += 1) {
    if (rows[index]?.type === 'hunk') break;
    const line = contextLine(rows[index]);
    if (line) after.push(line);
  }
  return { before, after };
};

const formatContextLine = (line) => `${line.side} ${line.line} | ${line.text}`;
const formatAgentInstruction = (comment) => {
  const lines = [
    '[Inline diff instruction]',
    `Path: ${comment.path}`,
    `Scope: ${scopeOf(comment)}`,
    `Side: ${comment.side}`,
    `Line: ${comment.line}`
  ];
  if (isTurnDiffScope(scopeOf(comment))) lines.push(`Captured file-change seq: ${comment.file_change_seq}`);
  lines.push('', 'Captured context:');
  for (const line of comment.context_before) lines.push(`  ${formatContextLine(line)}`);
  lines.push(`> ${comment.side} ${comment.line} | ${comment.line_text}`);
  for (const line of comment.context_after) lines.push(`  ${formatContextLine(line)}`);
  lines.push('', 'Instruction:', comment.instruction);
  return lines.join('\n');
};

const makeID = () => {
  try {
    if (globalThis.crypto?.randomUUID) return `diff_comment_${globalThis.crypto.randomUUID()}`;
  } catch {}
  return `diff_comment_${Date.now()}_${Math.random().toString(36).slice(2)}`;
};

const addOptimisticComment = (sessionId, comment) => {
  const cs = sessionCommentState(sessionId);
  const before = cs.comments;
  if (!before.some((existing) => existing.id === comment.id)) {
    cs.comments = [...before, { ...comment, client_message_id: comment.client_message_id || comment.id, created_at: Date.now(), optimistic: true }];
    bumpChangedPathRevisions(cs, before, cs.comments);
  }
  app.renderDiffSidebar?.(sessionId);
};

const removeOptimisticComment = (sessionId, commentId) => {
  const cs = sessionCommentState(sessionId);
  const before = cs.comments;
  cs.comments = before.filter((comment) => !(comment.id === commentId && comment.optimistic));
  if (cs.comments.length !== before.length) {
    bumpChangedPathRevisions(cs, before, cs.comments);
    app.renderDiffSidebar?.(sessionId);
  }
};

const makeButton = (className, label, text) => {
  const button = document.createElement('button');
  button.type = 'button';
  button.className = className;
  button.setAttribute('aria-label', label);
  button.title = label;
  button.textContent = text;
  return button;
};

const panelIDForAnchor = (sessionId, anchor) => {
  const source = `${sessionId}\u0000${anchorKey(anchor)}`;
  let hash = 2166136261;
  for (let index = 0; index < source.length; index += 1) hash = Math.imul(hash ^ source.charCodeAt(index), 16777619);
  return `diff-comment-panel-${(hash >>> 0).toString(36)}`;
};

const closePanel = (panel, focusTarget, options = {}) => {
  const sessionId = String(options.sessionId || panel?._diffCommentSessionId || '');
  const key = String(options.key || panel?._diffCommentKey || '');
  const cs = commentStateBySession.get(sessionId);
  if (options.clearDraft && cs && key) {
    cs.editorDrafts.delete(key);
    cs.retryIDs.delete(key);
  }
  if (cs?.openPanel?.key === key) cs.openPanel = null;
  const trigger = focusTarget || panel?._diffCommentTrigger;
  trigger?.setAttribute?.('aria-expanded', 'false');
  panel?.remove?.();
  trigger?.focus?.();
};

const showEditor = (panel, sessionId, anchor, rows, rowIndex, trigger, prior, options = {}) => {
  const existingEditor = panel.querySelector?.('.diff-comment-editor');
  if (existingEditor) {
    if (options.focus !== false) existingEditor.querySelector?.('textarea')?.focus?.();
    return;
  }
  const cs = sessionCommentState(sessionId);
  const key = anchorKey(anchor);
  if (cs.openPanel?.key === key) cs.openPanel.editing = true;
  const editor = document.createElement('div');
  editor.className = 'diff-comment-editor';
  const textarea = document.createElement('textarea');
  textarea.rows = 2;
  textarea.placeholder = prior.length > 0 ? 'Add a follow-up instruction…' : 'Instruction for this line…';
  textarea.setAttribute('aria-label', 'Inline diff instruction');
  textarea.value = cs.editorDrafts.get(key) || '';
  const actions = document.createElement('div');
  actions.className = 'diff-comment-editor-actions';
  const cancel = makeButton('diff-comment-cancel', 'Cancel inline comment', 'Cancel');
  const split = document.createElement('div');
  split.className = 'diff-comment-send-split';
  const send = makeButton('diff-comment-send', 'Send now', 'Send now');
  const more = makeButton('diff-comment-send-more', 'More send options', '▾');
  more.setAttribute('aria-haspopup', 'menu');
  more.setAttribute('aria-expanded', 'false');
  const menu = document.createElement('div');
  menu.className = 'diff-comment-send-menu';
  menu.id = `${panel.id}-send-menu`;
  menu.setAttribute('role', 'menu');
  menu.hidden = true;
  more.setAttribute('aria-controls', menu.id);
  const sendNowOption = makeButton('diff-comment-send-option', 'Send now', 'Send now');
  sendNowOption.setAttribute('role', 'menuitem');
  sendNowOption.tabIndex = -1;
  const queueOption = makeButton('diff-comment-send-option', 'Queue comment — Deliver later as one batch', 'Queue comment');
  queueOption.setAttribute('role', 'menuitem');
  queueOption.tabIndex = -1;
  const queueDescription = document.createElement('small');
  queueDescription.textContent = 'Deliver later as one batch';
  queueOption.appendChild(queueDescription);
  menu.append(sendNowOption, queueOption);
  split.append(send, more, menu);
  actions.append(cancel, split);
  editor.append(textarea, actions);
  panel.appendChild(editor);

  const menuItems = [sendNowOption, queueOption];
  const setMenuOpen = (open, focusIndex = -1) => {
    menu.hidden = !open;
    if (open) menu.removeAttribute?.('hidden'); else menu.setAttribute?.('hidden', '');
    more.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (open && focusIndex >= 0) menuItems[focusIndex]?.focus?.();
  };
  const moveMenuFocus = (direction) => {
    const current = Math.max(0, menuItems.indexOf(document.activeElement));
    menuItems[(current + direction + menuItems.length) % menuItems.length]?.focus?.();
  };
  const updatePrimary = () => {
    const queueMode = app.diffCommentSendMode?.(sessionId) === 'queue';
    const label = queueMode ? 'Queue comment' : 'Send now';
    send.textContent = label;
    send.setAttribute('aria-label', label);
    send.title = `${label} (Ctrl/⌘+Enter)`;
  };
  const submit = async (requestedMode = app.diffCommentSendMode?.(sessionId) || 'send') => {
    const mode = requestedMode === 'queue' ? 'queue' : 'send';
    const instruction = String(textarea.value || '').trim();
    if (!instruction || send.disabled) return false;
    cs.editorDrafts.set(key, textarea.value);
    if (mode === 'send' && !state.connected) {
      app.openAuthModal?.('Connect before sending an inline instruction.', true);
      return false;
    }
    if (mode === 'send' && state.branchContextQueuedSend) {
      app.showToast?.('A message is already queued for this conversation path.', { id: 'diff-comment-send', tone: 'warning' });
      return false;
    }
    if (mode === 'queue' && (app.queuedDiffComments?.(sessionId) || []).length >= (app.MAX_QUEUED_DIFF_COMMENTS || 20)) {
      app.showToast?.('Queue is full (20). Send or discard queued comments first.', { id: 'diff-comment-queue-full', tone: 'warning' });
      return false;
    }
    const context = captureContext(rows, rowIndex);
    const comment = normalizeComment({
      ...anchor,
      id: cs.retryIDs.get(key) || makeID(),
      parent_id: prior.length > 0 ? prior[prior.length - 1].id : '',
      context_before: context.before,
      context_after: context.after,
      instruction,
      optimistic: mode === 'send'
    });
    if (!comment) return false;
    app.pinDiffFileExpanded?.(sessionId, anchor.path);
    cs.pendingFocusKey = key;
    if (mode === 'queue') {
      if (!app.queueDiffComment?.(sessionId, comment)) {
        cs.pendingFocusKey = '';
        textarea.focus?.();
        return false;
      }
      send.disabled = true;
      more.disabled = true;
      textarea.disabled = true;
      closePanel(panel);
      cs.editorDrafts.delete(key);
      cs.retryIDs.delete(key);
      return true;
    }
    send.disabled = true;
    more.disabled = true;
    textarea.disabled = true;
    closePanel(panel);
    addOptimisticComment(sessionId, comment);
    let transportStarted = false;
    let transportQueued = false;
    let transportFailed = false;
    let failureHandled = false;
    const keepRetryDraft = (tone = 'error') => {
      if (failureHandled) return;
      failureHandled = true;
      transportFailed = true;
      cs.retryIDs.set(key, comment.id);
      removeOptimisticComment(sessionId, comment.id);
      app.showToast?.('Inline instruction was not sent. Your draft is still available.', { id: 'diff-comment-send', tone });
    };
    try {
      await app.sendMessage?.({
        prompt: formatAgentInstruction(comment),
        displayPrompt: comment.instruction,
        contentParts: [{ type: 'diff_comment', diff_comment: diffCommentPayload(comment) }],
        diffComment: comment,
        attachments: [],
        preserveComposer: true,
        reuseMessageId: comment.id,
        _onTransportStarted(detail = {}) {
          if (detail.queued) {
            transportQueued = true;
            return;
          }
          transportStarted = true;
          cs.retryIDs.delete(key);
          cs.editorDrafts.delete(key);
          void Promise.resolve().then(() => hydrateDiffComments(sessionId, { force: true }));
        },
        _onTransportFailed() { keepRetryDraft(); },
        _onTransportCanceled() { keepRetryDraft('warning'); }
      });
    } catch {
      keepRetryDraft();
    }
    if (transportQueued && !transportStarted && !transportFailed) return true;
    if (!transportStarted || transportFailed) {
      keepRetryDraft();
      return false;
    }
    return true;
  };
  const choose = async (mode) => {
    app.setDiffCommentSendMode?.(sessionId, mode);
    updatePrimary();
    setMenuOpen(false);
    if (!String(textarea.value || '').trim()) { textarea.focus?.(); return; }
    await submit(mode);
  };
  textarea.addEventListener('input', () => cs.editorDrafts.set(key, textarea.value));
  cancel.addEventListener('click', () => closePanel(panel, trigger, { clearDraft: true, sessionId, key }));
  send.addEventListener('click', () => submit());
  more.addEventListener('click', (event) => {
    event.stopPropagation?.();
    const opening = menu.hidden;
    setMenuOpen(opening, opening ? 0 : -1);
  });
  more.addEventListener('keydown', (event) => {
    if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp' && event.key !== 'Escape') return;
    event.preventDefault?.();
    event.stopImmediatePropagation?.();
    if (event.key === 'Escape') setMenuOpen(false);
    else setMenuOpen(true, event.key === 'ArrowUp' ? menuItems.length - 1 : 0);
  });
  sendNowOption.addEventListener('click', () => choose('send'));
  queueOption.addEventListener('click', () => choose('queue'));
  menu.addEventListener('keydown', (event) => {
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault?.();
      moveMenuFocus(event.key === 'ArrowDown' ? 1 : -1);
      return;
    }
    if (event.key === 'Home' || event.key === 'End') {
      event.preventDefault?.();
      menuItems[event.key === 'Home' ? 0 : menuItems.length - 1]?.focus?.();
      return;
    }
    if (event.key !== 'Escape' && event.key !== 'Tab') return;
    if (event.key === 'Escape') {
      event.preventDefault?.();
      event.stopImmediatePropagation?.();
      more.focus?.();
    }
    setMenuOpen(false);
  });
  textarea.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') {
      event.preventDefault?.();
      event.stopImmediatePropagation?.();
      closePanel(panel, trigger, { sessionId, key });
    } else if (event.key === 'Enter' && (event.metaKey || event.ctrlKey)) {
      event.preventDefault?.();
      void submit();
    }
  });
  updatePrimary();
  if (options.focus !== false) textarea.focus?.();
};

const openCommentPanel = (sessionId, anchor, rows, rowIndex, rowElement, trigger, options = {}) => {
  const cs = sessionCommentState(sessionId);
  app.pinDiffFileExpanded?.(sessionId, anchor.path);
  const key = anchorKey(anchor);
  const current = rowElement.parentNode?.querySelector?.('.diff-comment-panel');
  if (current) current.remove?.();
  const panel = document.createElement('div');
  panel.className = 'diff-comment-panel';
  panel.id = panelIDForAnchor(sessionId, anchor);
  panel.setAttribute('role', 'region');
  panel.setAttribute('aria-label', `Inline instructions for ${anchor.side} line ${anchor.line}`);
  panel._diffCommentTrigger = trigger;
  panel._diffCommentSessionId = sessionId;
  panel._diffCommentKey = key;
  trigger?.setAttribute?.('aria-controls', panel.id);
  trigger?.setAttribute?.('aria-expanded', 'true');
  rowElement.parentNode?.insertBefore?.(panel, rowElement.nextSibling || null);
  const queued = app.queuedDiffComments?.(sessionId, anchor) || [];
  const queuedIDs = new Set(queued.map((comment) => comment.id));
  const prior = commentsForAnchor(sessionId, anchor).filter((comment) => !queuedIDs.has(comment.id));
  const history = [...prior, ...queued].sort((a, b) => (a.created_at - b.created_at) || a.id.localeCompare(b.id));
  const wasEditing = options.restore && cs.openPanel?.key === key ? Boolean(cs.openPanel.editing) : history.length === 0;
  cs.openPanel = { key, path: anchor.path, side: anchor.side, line: anchor.line, editing: wasEditing };
  if (history.length > 0) {
    const heading = document.createElement('div');
    heading.className = 'diff-comment-heading';
    heading.textContent = `Line ${anchor.line} · ${anchor.side === 'old' ? 'original' : 'current'} version`;
    panel.appendChild(heading);
    for (const comment of history) {
      const isQueued = queued.some((entry) => entry.id === comment.id);
      const item = document.createElement('div');
      item.className = `diff-comment-history-item${isQueued ? ' queued' : ''}`;
      const text = document.createElement('div');
      text.className = 'diff-comment-history-text';
      text.textContent = comment.instruction;
      const meta = document.createElement('div');
      meta.className = 'diff-comment-history-meta';
      meta.textContent = isQueued
        ? (sameAnchorSnapshot(comment, anchor) ? 'Queued — not sent' : 'File changed after this was queued')
        : (sameAnchorSnapshot(comment, anchor) ? (comment.optimistic ? 'Sending…' : 'Sent') : `File changed after this instruction${comment.optimistic ? ' · sending…' : ''}`);
      item.append(text, meta);
      if (isQueued) {
        const itemActions = document.createElement('div');
        itemActions.className = 'diff-comment-history-actions';
        const edit = makeButton('diff-comment-history-edit', 'Edit queued inline instruction', 'Edit');
        const remove = makeButton('diff-comment-history-remove', 'Remove queued inline instruction', 'Remove');
        const queueSending = Boolean(app.diffCommentQueueSending?.(sessionId));
        edit.disabled = queueSending;
        remove.disabled = queueSending;
        edit.addEventListener('click', () => {
          if (app.diffCommentQueueSending?.(sessionId)) return;
          const removed = app.removeQueuedDiffComment?.(sessionId, comment.id);
          if (!removed) return;
          cs.editorDrafts.set(key, comment.instruction);
          cs.retryIDs.delete(key);
          if (cs.openPanel?.key === key) cs.openPanel.editing = true;
          app.pinDiffFileExpanded?.(sessionId, comment.path);
          app.scrollDiffFileIntoView?.(comment.path);
        });
        remove.addEventListener('click', () => {
          if (app.diffCommentQueueSending?.(sessionId)) return;
          app.removeQueuedDiffComment?.(sessionId, comment.id);
        });
        itemActions.append(edit, remove);
        item.appendChild(itemActions);
      }
      panel.appendChild(item);
    }
    const followUp = makeButton('diff-comment-follow-up', 'Add follow-up inline instruction', 'Add follow-up');
    followUp.addEventListener('click', () => showEditor(panel, sessionId, anchor, rows, rowIndex, trigger, history));
    panel.appendChild(followUp);
  }
  if (wasEditing) showEditor(panel, sessionId, anchor, rows, rowIndex, trigger, history, { focus: options.focus !== false });
};

const focusAdjacentCommentRow = (rowElement, direction) => {
  const rows = Array.from(rowElement.parentNode?.querySelectorAll?.('.diff-row[data-commentable="true"]') || []);
  const index = rows.indexOf(rowElement);
  if (index < 0 || rows.length === 0) return;
  const next = rows[(index + direction + rows.length) % rows.length];
  rowElement.tabIndex = -1;
  next.tabIndex = 0;
  next.focus?.();
};

const decorateDiffCommentRow = ({ sessionId, path, scope = 'last_turn', row, rows, rowIndex, rowElement, fileChangeSeq, initialTabStop = rowIndex === 0 }) => {
  if (!rowElement || !row || row.type === 'hunk') return null;
  const anchor = rowAnchor(path, row, fileChangeSeq, scope);
  if (!anchor.line || (isTurnDiffScope(anchor.scope) && !anchor.file_change_seq)) return null;
  const queued = app.queuedDiffComments?.(sessionId, anchor) || [];
  const queuedIDs = new Set(queued.map((comment) => comment.id));
  const prior = commentsForAnchor(sessionId, anchor).filter((comment) => !queuedIDs.has(comment.id));
  const total = prior.length + queued.length;
  const hasComments = total > 0;
  const queuedSuffix = queued.length > 0 ? ` (${queued.length} queued, not sent)` : '';
  const label = hasComments ? `Show ${total} inline instruction${total === 1 ? '' : 's'} for ${anchor.side} line ${anchor.line}${queuedSuffix}` : `Comment on ${anchor.side} line ${anchor.line}`;
  const button = makeButton(`diff-comment-affordance${hasComments ? ' has-comments' : ''}${queued.length > 0 ? ' queued' : ''}`, label, hasComments ? '' : '+');
  button.tabIndex = -1;
  button.setAttribute('aria-expanded', 'false');
  button.setAttribute('aria-controls', panelIDForAnchor(sessionId, anchor));
  if (hasComments && [...prior, ...queued].some((comment) => !sameAnchorSnapshot(comment, anchor))) {
    button.classList.add('stale');
    button.title = `${label} (one or more anchors are outdated)`;
  }
  const open = (event) => {
    event?.preventDefault?.();
    event?.stopPropagation?.();
    const expanded = button.getAttribute?.('aria-expanded') === 'true';
    const panel = rowElement.parentNode?.querySelector?.(`#${panelIDForAnchor(sessionId, anchor)}`);
    if (expanded && panel) {
      closePanel(panel, button, { sessionId, key: anchorKey(anchor) });
      return;
    }
    openCommentPanel(sessionId, anchor, rows, rowIndex, rowElement, button);
  };
  button.addEventListener('click', open);
  rowElement.dataset.commentable = 'true';
  rowElement.tabIndex = initialTabStop ? 0 : -1;
  rowElement.setAttribute?.('aria-label', `${anchor.side} line ${anchor.line}. ${label}`);
  rowElement.addEventListener?.('keydown', (event) => {
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault?.();
      focusAdjacentCommentRow(rowElement, event.key === 'ArrowDown' ? 1 : -1);
    } else if (event.key === 'Enter' || String(event.key || '').toLowerCase() === 'c') {
      event.preventDefault?.();
      open(event);
    }
  });
  rowElement.appendChild(button);
  const cs = sessionCommentState(sessionId);
  const key = anchorKey(anchor);
  button._diffCommentKey = key; rowElement._diffCommentKey = key;
  const shouldRestorePanel = cs.openPanel?.key === key, shouldRestoreFocus = cs.pendingFocusKey === key;
  return (options = {}) => {
    const ownedFocus = options.commentFocus?.key === key ? options.commentFocus : null;
    if (shouldRestorePanel) openCommentPanel(sessionId, anchor, rows, rowIndex, rowElement, button, { restore: true, focus: false });
    const restoreFocus = () => {
      // Explicit submission/queue focus takes precedence over preserving the old control.
      if (shouldRestoreFocus) {
        cs.pendingFocusKey = ''; button.focus?.(); return;
      }
      if (!ownedFocus) return;
      if (ownedFocus.kind === 'marker') {
        (ownedFocus.target === 'row' ? rowElement : button).focus?.(); return;
      }
      const panel = rowElement.parentNode?.querySelector?.(`#${panelIDForAnchor(sessionId, anchor)}`);
      (panel?.querySelector?.('textarea') || button).focus?.();
    };
    if (options.deferFocus) return restoreFocus;
    restoreFocus(); return null;
  };
};

const createDiffCommentMessageNode = (message, createMetaNode) => {
  const raw = Array.isArray(message.diffComments) && message.diffComments.length > 0
    ? message.diffComments
    : [message.diffComment];
  const comments = raw.map(normalizeComment).filter(Boolean);
  if (comments.length === 0) comments.push({ path: '', side: '', line: 0, line_text: '', instruction: String(message.content || '') });
  const article = document.createElement('article');
  article.className = 'message user diff-comment-message';
  article.dataset.messageId = message.id;
  const body = document.createElement('div');
  body.className = 'message-body diff-comment-message-body';
  if (comments.length > 1) {
    const summary = document.createElement('div');
    summary.className = 'diff-comment-message-summary';
    summary.textContent = `${comments.length} inline comments`;
    body.appendChild(summary);
  }
  comments.forEach((comment) => {
    const block = document.createElement('div');
    block.className = 'diff-comment-message-block';
    const heading = document.createElement('div');
    heading.className = 'diff-comment-message-heading';
    heading.textContent = `Inline comment · ${scopeLabel(comment)} · ${comment.path}:${comment.line} (${comment.side})`;
    const instruction = document.createElement('div');
    instruction.className = 'diff-comment-message-instruction';
    instruction.textContent = comment.instruction;
    const exact = document.createElement('code');
    exact.className = 'diff-comment-message-line';
    exact.textContent = `${comment.side} ${comment.line} | ${comment.line_text}`;
    if (comments.length === 1) body.append(heading, instruction, exact);
    else { block.append(heading, instruction, exact); body.appendChild(block); }
  });
  article.appendChild(body);
  if (typeof createMetaNode === 'function') article.appendChild(createMetaNode(message.created, message));
  return article;
};

const closeOpenSendMenus = (event) => {
  for (const menu of document.querySelectorAll?.('.diff-comment-send-menu') || []) {
    if (!menu.hidden && !event.target?.closest?.('.diff-comment-send-split')) {
      menu.hidden = true;
      menu.setAttribute?.('hidden', '');
      menu.parentNode?.querySelector?.('.diff-comment-send-more')?.setAttribute?.('aria-expanded', 'false');
    }
  }
};
document.addEventListener?.('pointerdown', closeOpenSendMenus);

window.addEventListener?.('keydown', (event) => {
  if (event.key !== 'Escape') return;
  const focused = document.activeElement;
  const panel = focused?.closest?.('.diff-comment-panel');
  if (!panel || !panel.contains?.(focused)) return;
  event.preventDefault?.();
  event.stopImmediatePropagation?.();
  closePanel(panel, panel._diffCommentTrigger);
});

Object.assign(app, {
  anchorKey,
  diffCommentPayload,
  captureDiffCommentContext: captureContext,
  formatDiffCommentInstruction: formatAgentInstruction,
  normalizeDiffComment: normalizeComment,
  hydrateDiffComments,
  invalidateDiffComments,
  pruneDiffCommentState,
  diffCommentRevision,
  bumpDiffCommentPathRevision,
  diffCommentPanelOpen,
  clearDiffCommentPanel,
  reconcileDiffCommentPanel,
  addOptimisticDiffComment: addOptimisticComment,
  removeOptimisticDiffComment: removeOptimisticComment,
  decorateDiffCommentRow,
  createDiffCommentMessageNode
});
})();
