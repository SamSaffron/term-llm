(() => {
'use strict';

const app = window.TermLLMApp;
if (!app) return;
const { state } = app;
state.pendingBranch ??= null;
state.branchTree ??= null;

const elements = app.elements || {};
Object.assign(elements, {
  branchTreeBtn: document.getElementById('branchTreeBtn'),
  branchTreeModal: document.getElementById('branchTreeModal'),
  branchTreeList: document.getElementById('branchTreeList'),
  branchTreeCloseBtn: document.getElementById('branchTreeCloseBtn'),
  pendingBranchBanner: document.getElementById('pendingBranchBanner'),
  pendingBranchBannerText: document.getElementById('pendingBranchBannerText'),
  pendingBranchCancelBtn: document.getElementById('pendingBranchCancelBtn'),
});

const BRANCHING_NOTICE_KEY = 'term_llm_branching_notice';
const durableSourceTailID = (message) => {
  const ids = Array.isArray(message?.durableSourceRowIds) ? message.durableSourceRowIds : [];
  const value = ids.at(-1) ?? message?.durableRowId;
  const id = Number(value);
  return Number.isFinite(id) && id > 0 ? id : 0;
};

const syncPendingBranchBanner = () => {
  const pending = state.pendingBranch;
  const active = app.getActiveSession?.();
  const visible = Boolean(pending && active && !state.draftSessionActive && pending.sourceSessionId === active.id);
  if (elements.pendingBranchBanner) elements.pendingBranchBanner.hidden = !visible;
  if (visible && elements.pendingBranchBannerText) {
    elements.pendingBranchBannerText.textContent = pending.selectedRole === 'user'
      ? 'Editing into a new conversation path'
      : 'Continuing in a new conversation path';
  }
};

const cancelPendingBranch = (options = {}) => {
  const pending = state.pendingBranch;
  state.pendingBranch = null;
  if (options.restoreComposer && pending?.originalComposer != null) {
    elements.promptInput.value = String(pending.originalComposer || '');
    app.autoGrowPrompt?.();
  }
  syncPendingBranchBanner();
  app.renderMessages?.(false);
};

const beginPendingBranch = (message, role = '') => {
  const session = app.getActiveSession?.();
  if (!session || state.draftSessionActive || message?.durable !== true) return false;
  if (state.streaming || state.compressing || state.sideQuestion?.running || app.sessionHasInProgressState?.(session)) {
    app.showToast?.('Cannot branch while work is active.', { id: 'conversation-branch', tone: 'warning' });
    return false;
  }
  const messages = window.TermLLMConversation.sessionMessages(session);
  const index = messages.findIndex((candidate) => candidate?.id === message.id);
  if (index < 0) return false;
  const selectedRole = role || message.role;
  let anchorMessageId = durableSourceTailID(message);
  if (selectedRole === 'user') {
    anchorMessageId = 0;
    for (let cursor = index - 1; cursor >= 0; cursor -= 1) {
      const candidate = messages[cursor];
      if (candidate?.role !== 'user' && candidate?.role !== 'assistant') continue;
      anchorMessageId = durableSourceTailID(candidate);
      if (anchorMessageId > 0) break;
    }
  }
  if (selectedRole !== 'user' && anchorMessageId <= 0) return false;
  const originalComposer = String(elements.promptInput.value || '');
  state.pendingBranch = {
    sourceSessionId: session.id,
    anchorMessageId,
    previousResponseId: `resp_msg_${anchorMessageId}`,
    expectedRev: Math.max(0, Number(session.transcript?.rev) || 0),
    idempotencyKey: app.generateId?.('branch') || `branch_${Date.now()}`,
    selectedMessageId: message.id,
    selectedRole,
    originalComposer,
  };
  elements.promptInput.value = selectedRole === 'user' ? String(message.content || '') : '';
  app.autoGrowPrompt?.();
  elements.promptInput.focus?.();
  syncPendingBranchBanner();
  app.renderMessages?.(false);
  if (localStorage.getItem(BRANCHING_NOTICE_KEY) !== '1') {
    localStorage.setItem(BRANCHING_NOTICE_KEY, '1');
    app.showToast?.('Conversation context branches; filesystem and tool side effects do not rewind.', { id: 'conversation-branch', tone: 'warning', duration: 7000 });
  }
  return true;
};

const applyPendingBranchProjection = () => {
  const root = elements.messages;
  if (!root) return;
  root.querySelectorAll?.('.branch-hidden').forEach((node) => node.classList.remove('branch-hidden'));
  root.querySelector?.('.branch-pending-divider')?.remove();
  const pending = state.pendingBranch;
  const session = app.getActiveSession?.();
  if (!pending || !session || pending.sourceSessionId !== session.id) return;
  const selected = app.findMessageElement?.(pending.selectedMessageId);
  if (!selected) return;
  let hide = false;
  for (const node of Array.from(root.children || [])) {
    if (node === selected) {
      if (pending.selectedRole === 'user') node.classList.add('branch-hidden');
      hide = true;
      continue;
    }
    if (hide) node.classList.add('branch-hidden');
  }
  const divider = document.createElement('div');
  divider.className = 'branch-pending-divider';
  divider.textContent = pending.selectedRole === 'user' ? 'Editing into a new path' : 'Continuing in a new path';
  root.appendChild(divider);
};

const syncBranchActions = () => {
  const root = elements.messages;
  const session = app.getActiveSession?.();
  if (!root || !session || state.draftSessionActive) return;
  const messages = window.TermLLMConversation.sessionMessages(session);
  for (const message of messages) {
    if (message?.role !== 'user' || message.durable !== true || durableSourceTailID(message) <= 0) continue;
    const node = app.findMessageElement?.(message.id);
    if (!node || node.querySelector?.('.message-branch-action')) continue;
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'message-branch-action';
    button.textContent = 'Edit from here';
    button.addEventListener('click', () => beginPendingBranch(message, 'user'));
    node.appendChild(button);
  }
  root.querySelectorAll?.('.turn-action-panel').forEach((panel) => {
    const assistantID = panel.dataset?.turnAssistantId || '';
    const message = messages.find((candidate) => candidate?.id === assistantID);
    if (!message || message.durable !== true || durableSourceTailID(message) <= 0) return;
    let button = panel.querySelector?.('.turn-branch-btn');
    if (!button) {
      button = document.createElement('button');
      button.type = 'button';
      button.className = 'turn-action-btn turn-branch-btn';
      button.textContent = 'Branch from here';
      button.addEventListener('click', (event) => {
        event.preventDefault();
        const current = window.TermLLMConversation.sessionMessages(app.getActiveSession?.())
          .find((candidate) => candidate?.id === button.dataset.turnAssistantId);
        if (current) beginPendingBranch(current, 'assistant');
      });
      panel.appendChild(button);
    }
    button.dataset.turnAssistantId = assistantID;
  });
  applyPendingBranchProjection();
  syncPendingBranchBanner();
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

const renderBranchTree = (tree) => {
  if (!elements.branchTreeList) return;
  elements.branchTreeList.innerHTML = '';
  const nodes = Array.isArray(tree?.nodes) ? tree.nodes : [];
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
    elements.branchTreeList.appendChild(button);
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
    const response = await app.apiFetch(`${app.UI_PREFIX}/v1/sessions/${encodeURIComponent(ownerID)}/tree`, { headers: app.requestHeaders(session.id) });
    if (!response.ok) throw new Error('Conversation tree unavailable');
    const tree = await response.json();
    if (state.activeSessionId !== ownerID || state.draftSessionActive) return null;
    state.branchTree = { ...tree, session_id: ownerID };
    const count = Math.max(1, Number(tree.path_count) || 1);
    if (elements.branchTreeBtn) {
      elements.branchTreeBtn.textContent = `${count} path${count === 1 ? '' : 's'}`;
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
  const tree = await refreshBranchTree();
  if (!tree || Number(tree.path_count) <= 1 || !elements.branchTreeModal) return;
  elements.branchTreeModal.hidden = false;
  elements.branchTreeCloseBtn?.focus?.();
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
  beginPendingBranch,
  cancelPendingBranch,
  applyPendingBranchProjection,
  syncBranchActions,
  refreshBranchTree,
  openBranchTree,
  adoptBranchedSessionOwnership,
});
syncPendingBranchBanner();
})();
