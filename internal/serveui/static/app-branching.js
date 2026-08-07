(() => {
'use strict';

const app = window.TermLLMApp;
if (!app) return;
const { state } = app;
state.pendingBranch ??= null;
state.branchTree ??= null;
state.branchContextOperation ??= null;

const elements = app.elements || {};
Object.assign(elements, {
  branchTreeBtn: document.getElementById('branchTreeBtn'),
  branchTreeModal: document.getElementById('branchTreeModal'),
  branchTreeCard: document.getElementById('branchTreeCard'),
  branchTreeList: document.getElementById('branchTreeList'),
  branchTreeCloseBtn: document.getElementById('branchTreeCloseBtn'),
  pendingBranchBanner: document.getElementById('pendingBranchBanner'),
  pendingBranchBannerText: document.getElementById('pendingBranchBannerText'),
  pendingBranchCancelBtn: document.getElementById('pendingBranchCancelBtn'),
});

const durableSourceTailID = (message) => {
  const ids = Array.isArray(message?.durableSourceRowIds) ? message.durableSourceRowIds : [];
  const value = ids.at(-1) ?? message?.durableRowId;
  const id = Number(value);
  return Number.isFinite(id) && id > 0 ? id : 0;
};

const beginBranchPoint = (point) => {
  const session = app.getActiveSession?.();
  if (!session || state.draftSessionActive || !point || point.role !== 'user') return false;
  if (state.streaming || state.compressing || state.sideQuestion?.running || app.sessionHasInProgressState?.(session)) {
    app.showToast?.('Cannot branch while work is active.', { id: 'conversation-branch', tone: 'warning' });
    return false;
  }
  const anchorMessageId = Math.max(0, Number(point.anchor_message_id) || 0);
  const messages = window.TermLLMConversation.sessionMessages(session);
  const selected = messages.find((message) => durableSourceTailID(message) === Number(point.message_id));
  const laterMessageCount = Math.max(0, Number(point.later_message_count) || 0);
  app.openBranchContextChooser?.({
    sourceSessionId: session.id,
    anchorMessageId,
    previousResponseId: `resp_msg_${anchorMessageId}`,
    expectedRev: Math.max(0, Number(session.transcript?.rev) || 0),
    idempotencyKey: app.generateId?.('branch') || `branch_${Date.now()}`,
    selectedMessageId: selected?.id || '',
    selectedMessageDurableId: Number(point.message_id) || 0,
    selectedRole: 'user',
    selectedText: String(point.prefill || ''),
    hasLaterConversation: laterMessageCount > 0,
    laterMessageCount,
    originalComposer: String(elements.promptInput.value || ''),
  });
  return true;
};

const syncPendingBranchBanner = () => {
  if (elements.pendingBranchBanner) elements.pendingBranchBanner.hidden = true;
};

const cancelPendingBranch = (options = {}) => {
  const pending = state.pendingBranch;
  state.pendingBranch = null;
  if (options.restoreComposer && pending?.originalComposer != null) {
    elements.promptInput.value = String(pending.originalComposer || '');
    app.autoGrowPrompt?.();
  }
  app.renderMessages?.(false);
};

const projectPendingBranchMessages = (messages, session) => {
  const pending = state.pendingBranch;
  if (!pending || !session || pending.sourceSessionId !== session.id) return messages;
  const index = messages.findIndex((message) => message?.id === pending.selectedMessageId
    || (pending.selectedMessageDurableId > 0 && durableSourceTailID(message) === pending.selectedMessageDurableId));
  if (index < 0) return messages;
  return messages.slice(0, index);
};

const branchTopLevelNode = (node, root) => {
  let current = node;
  while (current?.parentNode && current.parentNode !== root) current = current.parentNode;
  return current?.parentNode === root ? current : null;
};

const createBranchOriginNode = (session) => {
  const divider = document.createElement('div');
  divider.className = 'branch-origin-divider';
  const lineBefore = document.createElement('span');
  lineBefore.className = 'branch-origin-line';
  const lineAfter = document.createElement('span');
  lineAfter.className = 'branch-origin-line';
  const button = document.createElement('button');
  button.type = 'button';
  button.textContent = `Branched from ${session.branchParentTitle || 'earlier conversation'}`;
  button.addEventListener('click', () => app.switchToSession?.(session.branchParentSessionId, { focusPrompt: true }));
  divider.appendChild(lineBefore);
  divider.appendChild(button);
  divider.appendChild(lineAfter);
  return divider;
};

const createBranchContextStatusNode = (session) => {
  const status = session.branchContextStatus;
  if (!status) return null;
  const node = document.createElement('div');
  node.className = `branch-context-status ${status.phase || 'running'}`;
  node.setAttribute('role', 'status');
  node.setAttribute('aria-live', 'polite');
  if (status.phase === 'running') {
    const spinner = document.createElement('span');
    spinner.className = 'branch-context-spinner';
    spinner.setAttribute('aria-hidden', 'true');
    node.appendChild(spinner);
  }
  const copy = document.createElement('span');
  const count = Math.max(0, Number(status.sourceMessageCount) || 0);
  copy.textContent = status.phase === 'failed'
    ? 'Could not bring context from the earlier path.'
    : `Bringing ${status.mode === 'focused' ? 'specific' : 'useful'} context${count ? ` from ${count} later message${count === 1 ? '' : 's'}` : ''}…`;
  node.appendChild(copy);
  if (status.queued) {
    const queued = document.createElement('small');
    queued.textContent = 'Message queued and will send when context is ready';
    node.appendChild(queued);
  }
  if (status.phase === 'failed') {
    const retry = document.createElement('button');
    retry.type = 'button';
    retry.textContent = 'Try again';
    retry.addEventListener('click', () => startBranchContextPreparation(session, status.mode, status.focus, status.sourceMessageCount));
    const clean = document.createElement('button');
    clean.type = 'button';
    clean.textContent = 'Continue without it';
    clean.addEventListener('click', () => {
      session.branchContextStatus = null;
      app.restoreBranchContextQueuedSend?.(session.id);
      app.renderMessages?.(false);
    });
    node.appendChild(retry);
    node.appendChild(clean);
  }
  return node;
};

const syncBranchDecorations = () => {
  const root = elements.messages;
  const session = app.getActiveSession?.();
  if (!root || !session) return;
  root.querySelector?.('.branch-origin-divider')?.remove();
  root.querySelector?.('.branch-context-status')?.remove();
  if (!session.branchParentSessionId) return;
  const origin = createBranchOriginNode(session);
  const anchorID = Number(session.branchAnchorMessageId) || 0;
  let before = root.firstElementChild || null;
  if (anchorID > 0) {
    const anchor = root.querySelector?.(`[data-durable-id="${anchorID}"]`);
    const top = branchTopLevelNode(anchor, root);
    before = top?.nextSibling || null;
  }
  root.insertBefore(origin, before);
  const status = createBranchContextStatusNode(session);
  if (status) root.insertBefore(status, origin.nextSibling || null);
};

const applyPendingBranchProjection = () => app.renderMessages?.(false);

const syncBranchActions = () => {
  if (state.draftSessionActive) return;
  syncBranchDecorations();
  syncPendingBranchBanner();
};

const startBranchContextPreparation = async (session, mode, focus = '', sourceMessageCount = 0) => {
  if (!session || (mode !== 'notes' && mode !== 'focused')) return false;
  const operation = { sessionId: session.id, mode, focus, sourceMessageCount };
  state.branchContextOperation = operation;
  session.branchContextStatus = { phase: 'running', mode, focus, sourceMessageCount, queued: false };
  app.renderMessages?.(false);
  try {
    const response = await app.apiFetch(`${app.UI_PREFIX}/v1/sessions/${encodeURIComponent(session.id)}/path-notes`, {
      method: 'POST',
      headers: app.requestHeaders(session.id),
      body: JSON.stringify({ mode, ...(mode === 'focused' ? { focus } : {}) })
    }, { policy: app.API_FETCH_POLICY.idempotentMutation, retries: 0, timeoutMs: 0 });
    if (!response.ok) throw await app.normalizeError(response);
    if (state.branchContextOperation !== operation) return false;
    session.branchContextStatus = null;
    state.branchContextOperation = null;
    try {
      await app.refreshActiveSessionMessagesFromServer?.(session, {
        force: true, useEtag: false, forceScroll: false, reason: 'branch-context-ready'
      });
    } catch (_error) {
      app.showToast?.('Context is ready; retrying transcript refresh…', { id: 'branch-context-refresh', tone: 'warning' });
    }
    app.releaseBranchContextQueuedSend?.(session.id);
    app.showToast?.('Context ready.', { id: 'branch-context-ready' });
    app.renderMessages?.(false);
    return true;
  } catch (error) {
    if (state.branchContextOperation !== operation) return false;
    state.branchContextOperation = null;
    app.restoreBranchContextQueuedSend?.(session.id);
    session.branchContextStatus = { phase: 'failed', mode, focus, sourceMessageCount, queued: false };
    app.renderMessages?.(false);
    return false;
  }
};

const createConversationBranch = async (draft, mode = 'clean', focus = '') => {
  const source = app.getActiveSession?.();
  if (!draft || !source || source.id !== draft.sourceSessionId) return false;
  const creatingOperation = { sessionId: source.id, phase: 'creating' };
  state.branchContextOperation = creatingOperation;
  state.pendingBranch = { ...draft, branchContextMode: mode, branchContextFocus: focus };
  app.renderMessages?.(false);
  try {
    const response = await app.apiFetch(`${app.UI_PREFIX}/v1/sessions/${encodeURIComponent(source.id)}/branches`, {
      method: 'POST',
      headers: app.requestHeaders(source.id),
      body: JSON.stringify({
        anchor_message_id: Number(draft.anchorMessageId) || 0,
        expected_rev: Math.max(0, Number(draft.expectedRev) || 0),
        idempotency_key: String(draft.idempotencyKey || '').trim()
      })
    }, { policy: app.API_FETCH_POLICY.idempotentMutation, retries: 0 });
    if (!response.ok) throw await app.normalizeError(response);
    const payload = await response.json();
    const childID = String(payload?.session?.id || '').trim();
    if (!childID || childID === source.id) throw new Error('Branch response did not identify a child session.');
    const copiedAnchorID = Number(payload.copied_anchor_message_id) || 0;
    const child = adoptBranchedSessionOwnership(source, childID, [], copiedAnchorID ? `resp_msg_${copiedAnchorID}` : 'resp_msg_0');
    app.applyServerSessionSummary?.(child, payload.session || {});
    app.updateURL?.(app.sessionSlug?.(child) || childID);
    child.branchParentSessionId = String(payload.parent_session_id || source.id);
    child.branchParentTitle = String(payload.parent_title || source.title || 'earlier conversation');
    child.branchAnchorMessageId = Number(payload.copied_anchor_message_id) || 0;
    child.branchDepth = Math.max(1, Number(source.branchDepth || 0) + 1);
    child.branchRootSessionId = source.branchRootSessionId || source.id;
    state.branchContextOperation = null;
    app.persistAndRefreshShell?.();
    if (localStorage.getItem('term_llm_branching_notice') !== '1') {
      localStorage.setItem('term_llm_branching_notice', '1');
      app.showToast?.('Conversation context branches; filesystem and tool side effects do not rewind.', { id: 'conversation-branch', tone: 'warning', duration: 7000 });
    }
    elements.promptInput.value = String(draft.selectedText || '');
    app.autoGrowPrompt?.();
    app.renderMessages?.(true);
    const preparesContext = mode === 'notes' || mode === 'focused';
    if (preparesContext) {
      void startBranchContextPreparation(child, mode, focus, draft.laterMessageCount || 0);
    }
    try {
      await app.refreshActiveSessionMessagesFromServer?.(child, {
        force: true, useEtag: false, forceScroll: true, reason: 'branch-created'
      });
    } catch (_error) {
      app.showToast?.('New path created; retrying its transcript…', { id: 'branch-transcript-refresh', tone: 'warning' });
      setTimeout(() => app.refreshActiveSessionMessagesFromServer?.(child, {
        force: true, useEtag: false, forceScroll: false, reason: 'branch-created-retry'
      }).catch(() => {}), 1000);
    }
    app.renderSidebar?.();
    if (!preparesContext) elements.promptInput.focus?.();
    return true;
  } catch (error) {
    if (state.branchContextOperation === creatingOperation) state.branchContextOperation = null;
    state.pendingBranch = null;
    elements.promptInput.value = String(draft.originalComposer || '');
    app.autoGrowPrompt?.();
    app.renderMessages?.(false);
    app.showToast?.(error?.message || 'Could not create conversation path.', { id: 'conversation-branch', tone: 'error' });
    return false;
  }
};

const branchTreeDepth = (nodes, node) => {
  const byId = new Map(nodes.map((entry) => [entry.session_id, entry]));
  let depth = 0;
  let parent = node?.parent_session_id;
  while (parent && byId.has(parent) && depth < nodes.length) {
    depth += 1;
    parent = byId.get(parent)?.parent_session_id;
  }
  return depth;
};

const createBranchTreeSection = (title) => {
  const section = document.createElement('section');
  section.className = 'branch-tree-section';
  const heading = document.createElement('div');
  heading.className = 'branch-tree-section-title';
  heading.textContent = title;
  section.appendChild(heading);
  elements.branchTreeList.appendChild(section);
  return section;
};

const renderBranchTree = (tree) => {
  if (!elements.branchTreeList) return;
  elements.branchTreeList.innerHTML = '';
  const nodes = Array.isArray(tree?.nodes) ? tree.nodes : [];
  const paths = createBranchTreeSection('Existing paths');
  for (const node of nodes) {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = `branch-tree-item${node.session_id === state.activeSessionId ? ' active' : ''}`;
    button.style.paddingLeft = `${.65 + branchTreeDepth(nodes, node) * 1.1}rem`;
    const marker = document.createElement('span');
    marker.textContent = node.session_id === state.activeSessionId ? '●' : '○';
    const copy = document.createElement('span');
    const title = document.createElement('span');
    title.textContent = node.title || `Session #${node.session_number || ''}`;
    const detail = document.createElement('small');
    detail.textContent = node.anchor_preview ? `Forked after: ${node.anchor_preview}` : 'Original path';
    copy.appendChild(title);
    copy.appendChild(detail);
    button.appendChild(marker);
    button.appendChild(copy);
    button.addEventListener('click', async () => {
      elements.branchTreeModal.hidden = true;
      cancelPendingBranch();
      if (!state.sessions.some((session) => session.id === node.session_id)) {
        state.sessions.push({ id: node.session_id, number: Number(node.session_number) || 0, title: node.title || 'Conversation path', messages: [], lastResponseId: null, activeResponseId: null, created: Date.now(), _serverOnly: true });
      }
      await app.switchToSession?.(node.session_id, { focusPrompt: true });
    });
    paths.appendChild(button);
  }

  const points = (Array.isArray(tree?.branch_points) ? tree.branch_points : [])
    .filter((point) => point?.role === 'user');
  if (points.length > 0) {
    const branchPoints = createBranchTreeSection('Branch points');
    for (const point of points) {
      const button = document.createElement('button');
      button.type = 'button';
      button.className = 'branch-tree-item branch-tree-point';
      const marker = document.createElement('span');
      marker.className = 'branch-tree-role user';
      marker.textContent = 'U';
      const copy = document.createElement('span');
      const title = document.createElement('span');
      title.textContent = `Edit: ${point.preview || '(attachment content)'}`;
      const detail = document.createElement('small');
      const later = Math.max(0, Number(point.later_message_count) || 0);
      detail.textContent = `Message ${Math.max(1, Number(point.sequence) + 1)}${later ? ` · ${later} later message${later === 1 ? '' : 's'}` : ''}`;
      copy.appendChild(title);
      copy.appendChild(detail);
      button.appendChild(marker);
      button.appendChild(copy);
      button.addEventListener('click', () => {
        elements.branchTreeModal.hidden = true;
        beginBranchPoint(point);
      });
      branchPoints.appendChild(button);
    }
  }
};

const refreshBranchTree = async (options = {}) => {
  const session = app.getActiveSession?.();
  if (!session || state.draftSessionActive || !app.isSessionIdentityResolved?.(session)) {
    if (elements.branchTreeBtn) elements.branchTreeBtn.hidden = true;
    return null;
  }
  const ownerID = session.id;
  try {
    const suffix = options.includeBranchPoints ? '?include_branch_points=1' : '';
    const response = await app.apiFetch(`${app.UI_PREFIX}/v1/sessions/${encodeURIComponent(ownerID)}/tree${suffix}`, { headers: app.requestHeaders(session.id) });
    if (!response.ok) throw new Error('Conversation tree unavailable');
    const tree = await response.json();
    if (state.activeSessionId !== ownerID || state.draftSessionActive) return null;
    state.branchTree = { ...tree, session_id: ownerID };
    const nodes = Array.isArray(tree.nodes) ? tree.nodes : [];
    const byID = new Map(nodes.map((node) => [node.session_id, node]));
    nodes.forEach((node) => {
      const local = state.sessions.find((candidate) => candidate.id === node.session_id);
      if (!local) return;
      local.branchParentSessionId = String(node.parent_session_id || '');
      local.branchParentTitle = String(byID.get(node.parent_session_id)?.title || '');
      local.branchAnchorMessageId = Number(node.copied_anchor_message_id) || 0;
      local.branchDepth = branchTreeDepth(nodes, node);
      local.branchRootSessionId = String(tree.root_session_id || node.session_id);
    });
    app.renderSidebar?.();
    app.renderMessages?.(false);
    const count = Math.max(1, Number(tree.path_count) || 1);
    if (elements.branchTreeBtn) {
      elements.branchTreeBtn.textContent = count > 1 ? `${count} paths` : 'Paths';
      elements.branchTreeBtn.hidden = count <= 1;
    }
    if (options.render !== false) renderBranchTree(tree);
    return tree;
  } catch (_err) {
    if (state.activeSessionId === ownerID && elements.branchTreeBtn) elements.branchTreeBtn.hidden = true;
    return null;
  }
};

const openBranchTree = async () => {
  const tree = await refreshBranchTree({ includeBranchPoints: true });
  if (!tree || !elements.branchTreeModal) return;
  elements.branchTreeModal.hidden = false;
  elements.branchTreeCard?.focus?.({ preventScroll: true });
};

const adoptBranchedSessionOwnership = (source, childSessionId, userMessages = [], copiedAnchorResponseId = '') => {
  const childID = String(childSessionId || '').trim();
  if (!source || !childID || childID === source.id) return source;
  for (const message of userMessages) source.transcript?.removePendingIntent?.(message.clientMessageId || message.id);
  app.persistPendingIntents?.(source);
  app.refreshSessionMessagesFromTranscript?.(source);
  app.setSessionOptimisticBusy?.(source, false);

  let child = state.sessions.find((candidate) => candidate.id === childID);
  if (!child) {
    child = { ...source, id: childID, number: 0, title: source.title || 'Branched conversation', created: Date.now(), lastMessageAt: Date.now(), activeResponseId: null, messages: [], transcript: typeof window.ConversationController === 'function' ? new window.ConversationController(childID) : null, _serverOnly: false };
    state.sessions.unshift(child);
  }
  const copiedAnchor = String(copiedAnchorResponseId || '').trim();
  child.lastResponseId = copiedAnchor || child.lastResponseId || null;
  for (const message of userMessages) app.trackPendingIntent?.(child, message);
  state.pendingBranch = null;
  state.activeSessionId = childID;
  state.draftSessionActive = false;
  if (elements.messages?.dataset) elements.messages.dataset.sessionId = childID;
  app.updateURL?.(app.sessionSlug?.(child) || childID);
  app.persistAndRefreshShell?.();
  syncPendingBranchBanner();
  app.renderMessages?.(true);
  void refreshBranchTree({ render: false });
  return child;
};

if (elements.branchTreeBtn) elements.branchTreeBtn.addEventListener('click', openBranchTree);
if (elements.branchTreeCloseBtn) elements.branchTreeCloseBtn.addEventListener('click', () => { elements.branchTreeModal.hidden = true; });
if (elements.pendingBranchCancelBtn) elements.pendingBranchCancelBtn.addEventListener('click', () => cancelPendingBranch({ restoreComposer: true }));
if (elements.branchTreeModal) elements.branchTreeModal.addEventListener('click', (event) => {
  if (event.target === elements.branchTreeModal) elements.branchTreeModal.hidden = true;
});
document.addEventListener('keydown', (event) => {
  if (event.key === 'Escape' && elements.branchTreeModal && !elements.branchTreeModal.hidden) {
    elements.branchTreeModal.hidden = true;
    elements.branchTreeBtn?.focus?.();
  }
});

Object.assign(app, {
  beginBranchPoint,
  cancelPendingBranch,
  syncPendingBranchBanner,
  applyPendingBranchProjection,
  projectPendingBranchMessages,
  syncBranchDecorations,
  syncBranchActions,
  createConversationBranch,
  startBranchContextPreparation,
  refreshBranchTree,
  openBranchTree,
  adoptBranchedSessionOwnership,
});
syncPendingBranchBanner();
})();
