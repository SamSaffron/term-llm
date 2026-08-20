(() => {
'use strict';

const app = window.TermLLMApp || (window.TermLLMApp = {});
const { UI_PREFIX, state } = app;
const CONTEXT_RADIUS = 2;
const COMMENT_REFRESH_MS = 15_000;
const commentStateBySession = new Map();

const liveSessionIDs = () => new Set((state.sessions || []).map((session) => String(session?.id || '').trim()).filter(Boolean));
const pruneDiffCommentState = (retainedIDs = liveSessionIDs()) => {
  const retained = retainedIDs instanceof Set ? retainedIDs : new Set(retainedIDs || []);
  for (const sessionId of commentStateBySession.keys()) {
    if (!retained.has(sessionId)) commentStateBySession.delete(sessionId);
  }
};

const sessionCommentState = (sessionId) => {
  let value = commentStateBySession.get(sessionId);
  if (!value) {
    value = {
      comments: [], loaded: false, inflight: null, lastLoadedAt: 0, serverRevision: -1,
      revisionsByPath: new Map(), editorDrafts: new Map(), openPanel: null
    };
    commentStateBySession.set(sessionId, value);
  }
  return value;
};

const normalizeComment = (entry) => {
  const raw = entry?.diff_comment || entry;
  if (!raw || typeof raw !== 'object') return null;
  const side = String(raw.side || '').toLowerCase();
  const line = Number(raw.line) || 0;
  const seq = Number(raw.file_change_seq) || 0;
  const id = String(raw.id || '').trim();
  const path = String(raw.path || '');
  const instruction = String(raw.instruction || '').trim();
  if (!id || !path || !instruction || (side !== 'old' && side !== 'new') || line <= 0 || seq <= 0) return null;
  return {
    id,
    parent_id: String(raw.parent_id || ''),
    path,
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

const anchorKey = (comment) => `${String(comment?.path || '')}\u0000${String(comment?.side || '')}\u0000${Number(comment?.line) || 0}`;
const commentsForAnchor = (sessionId, anchor) => sessionCommentState(sessionId).comments
  .filter((comment) => anchorKey(comment) === anchorKey(anchor))
  .sort((a, b) => (a.created_at - b.created_at) || a.id.localeCompare(b.id));

const pathFingerprint = (comments, path) => JSON.stringify(comments
  .filter((comment) => comment.path === path)
  .map((comment) => [comment.id, comment.parent_id, comment.side, comment.line, comment.file_change_seq, comment.instruction, comment.optimistic])
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

const rowAnchor = (path, row, fileChangeSeq) => {
  const side = row?.type === 'del' ? 'old' : (row?.newNo ? 'new' : 'old');
  return {
    path,
    side,
    line: Number(side === 'old' ? row?.oldNo : row?.newNo) || 0,
    file_change_seq: Number(fileChangeSeq) || 0,
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
    `Side: ${comment.side}`,
    `Line: ${comment.line}`,
    `Captured file-change seq: ${comment.file_change_seq}`,
    '',
    'Captured context:'
  ];
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
    cs.comments = [...before, { ...comment, client_message_id: comment.id, created_at: Date.now(), optimistic: true }];
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
  if (options.clearDraft && cs && key) cs.editorDrafts.delete(key);
  if (cs?.openPanel?.key === key) cs.openPanel = null;
  const trigger = focusTarget || panel?._diffCommentTrigger;
  trigger?.setAttribute?.('aria-expanded', 'false');
  panel?.remove?.();
  trigger?.focus?.();
};

const showEditor = (panel, sessionId, anchor, rows, rowIndex, trigger, prior) => {
  const existingEditor = panel.querySelector?.('.diff-comment-editor');
  if (existingEditor) {
    existingEditor.querySelector?.('textarea')?.focus?.();
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
  const send = makeButton('diff-comment-send', 'Send inline comment now', 'Send now');
  actions.append(cancel, send);
  editor.append(textarea, actions);
  panel.appendChild(editor);

  const submit = async () => {
    const instruction = String(textarea.value || '').trim();
    if (!instruction || send.disabled) return;
    cs.editorDrafts.set(key, textarea.value);
    if (!state.connected) {
      app.openAuthModal?.('Connect before sending an inline instruction.', true);
      return;
    }
    if (state.branchContextQueuedSend) {
      app.showToast?.('A message is already queued for this conversation path.', { id: 'diff-comment-send', tone: 'warning' });
      return;
    }
    const context = captureContext(rows, rowIndex);
    const comment = normalizeComment({
      ...anchor,
      id: makeID(),
      parent_id: prior.length > 0 ? prior[prior.length - 1].id : '',
      context_before: context.before,
      context_after: context.after,
      instruction,
      optimistic: true
    });
    if (!comment) return;
    send.disabled = true;
    textarea.disabled = true;
    closePanel(panel);
    addOptimisticComment(sessionId, comment);
    let transportStarted = false;
    let transportFailed = false;
    try {
      await app.sendMessage?.({
        prompt: formatAgentInstruction(comment),
        displayPrompt: comment.instruction,
        contentParts: [{ type: 'diff_comment', diff_comment: {
          id: comment.id,
          ...(comment.parent_id ? { parent_id: comment.parent_id } : {}),
          path: comment.path,
          side: comment.side,
          line: comment.line,
          file_change_seq: comment.file_change_seq,
          line_text: comment.line_text,
          context_before: comment.context_before,
          context_after: comment.context_after,
          instruction: comment.instruction
        }}],
        diffComment: comment,
        attachments: [],
        preserveComposer: true,
        reuseMessageId: comment.id,
        _onTransportStarted() { transportStarted = true; },
        _onTransportFailed() { transportFailed = true; }
      });
    } catch {
      transportFailed = true;
    }
    if (!transportStarted || transportFailed) {
      removeOptimisticComment(sessionId, comment.id);
      app.showToast?.('Inline instruction was not sent. Your draft is still available.', { id: 'diff-comment-send', tone: 'error' });
      return;
    }
    cs.editorDrafts.delete(key);
    void hydrateDiffComments(sessionId, { force: true });
  };
  textarea.addEventListener('input', () => cs.editorDrafts.set(key, textarea.value));
  cancel.addEventListener('click', () => closePanel(panel, trigger, { clearDraft: true, sessionId, key }));
  send.addEventListener('click', submit);
  textarea.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') {
      event.preventDefault?.();
      closePanel(panel, trigger, { sessionId, key });
    } else if (event.key === 'Enter' && (event.metaKey || event.ctrlKey)) {
      event.preventDefault?.();
      void submit();
    }
  });
  textarea.focus?.();
};

const openCommentPanel = (sessionId, anchor, rows, rowIndex, rowElement, trigger, options = {}) => {
  const cs = sessionCommentState(sessionId);
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
  const prior = commentsForAnchor(sessionId, anchor);
  const wasEditing = options.restore && cs.openPanel?.key === key ? Boolean(cs.openPanel.editing) : prior.length === 0;
  cs.openPanel = { key, path: anchor.path, side: anchor.side, line: anchor.line, editing: wasEditing };
  if (prior.length > 0) {
    const heading = document.createElement('div');
    heading.className = 'diff-comment-heading';
    heading.textContent = `Line ${anchor.line} · ${anchor.side === 'old' ? 'original' : 'current'} version`;
    panel.appendChild(heading);
    for (const comment of prior) {
      const item = document.createElement('div');
      item.className = 'diff-comment-history-item';
      const text = document.createElement('div');
      text.className = 'diff-comment-history-text';
      text.textContent = comment.instruction;
      const meta = document.createElement('div');
      meta.className = 'diff-comment-history-meta';
      meta.textContent = comment.file_change_seq === anchor.file_change_seq
        ? (comment.optimistic ? 'Sending…' : 'Sent')
        : `File changed after this instruction${comment.optimistic ? ' · sending…' : ''}`;
      item.append(text, meta);
      panel.appendChild(item);
    }
    const followUp = makeButton('diff-comment-follow-up', 'Add follow-up inline instruction', 'Add follow-up');
    followUp.addEventListener('click', () => showEditor(panel, sessionId, anchor, rows, rowIndex, trigger, prior));
    panel.appendChild(followUp);
  }
  if (wasEditing) showEditor(panel, sessionId, anchor, rows, rowIndex, trigger, prior);
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

const decorateDiffCommentRow = ({ sessionId, path, row, rows, rowIndex, rowElement, fileChangeSeq }) => {
  if (!rowElement || !row || row.type === 'hunk') return null;
  const anchor = rowAnchor(path, row, fileChangeSeq);
  if (!anchor.line || !anchor.file_change_seq) return null;
  const prior = commentsForAnchor(sessionId, anchor);
  const hasComments = prior.length > 0;
  const label = hasComments ? `Show ${prior.length} inline instruction${prior.length === 1 ? '' : 's'} for ${anchor.side} line ${anchor.line}` : `Comment on ${anchor.side} line ${anchor.line}`;
  const button = makeButton(`diff-comment-affordance${hasComments ? ' has-comments' : ''}`, label, hasComments ? '' : '+');
  button.tabIndex = -1;
  button.setAttribute('aria-expanded', 'false');
  button.setAttribute('aria-controls', panelIDForAnchor(sessionId, anchor));
  if (hasComments && prior.some((comment) => comment.file_change_seq !== anchor.file_change_seq)) {
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
  rowElement.tabIndex = rows.slice(0, rowIndex).some((candidate) => candidate?.type !== 'hunk' && contextLine(candidate)) ? -1 : 0;
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
  const shouldRestore = sessionCommentState(sessionId).openPanel?.key === anchorKey(anchor);
  return shouldRestore ? () => openCommentPanel(sessionId, anchor, rows, rowIndex, rowElement, button, { restore: true }) : null;
};

const createDiffCommentMessageNode = (message, createMetaNode) => {
  const comment = normalizeComment(message.diffComment) || { path: '', side: '', line: 0, line_text: '', instruction: String(message.content || '') };
  const article = document.createElement('article');
  article.className = 'message user diff-comment-message';
  article.dataset.messageId = message.id;
  const body = document.createElement('div');
  body.className = 'message-body diff-comment-message-body';
  const heading = document.createElement('div');
  heading.className = 'diff-comment-message-heading';
  heading.textContent = `Inline comment · ${comment.path}:${comment.line} (${comment.side})`;
  const instruction = document.createElement('div');
  instruction.className = 'diff-comment-message-instruction';
  instruction.textContent = comment.instruction;
  const exact = document.createElement('code');
  exact.className = 'diff-comment-message-line';
  exact.textContent = `${comment.side} ${comment.line} | ${comment.line_text}`;
  body.append(heading, instruction, exact);
  article.appendChild(body);
  if (typeof createMetaNode === 'function') article.appendChild(createMetaNode(message.created, message));
  return article;
};

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
  captureDiffCommentContext: captureContext,
  formatDiffCommentInstruction: formatAgentInstruction,
  normalizeDiffComment: normalizeComment,
  hydrateDiffComments,
  invalidateDiffComments,
  pruneDiffCommentState,
  diffCommentRevision,
  decorateDiffCommentRow,
  createDiffCommentMessageNode
});
})();
