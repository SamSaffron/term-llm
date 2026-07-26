'use strict';

(function initAppInterject() {

const app = window.TermLLMApp;
const {
  UI_PREFIX, STORAGE_KEYS, state, elements, generateId, sanitizeInterruptState, INTERJECTION_PHASE, sanitizeMessage, syncTokenCookie, truncate, saveSessions,
  getActiveSession, createSession, scrollToBottom, setConnectionState, setProviderRetryStatus, clearProviderRetryStatus, sessionSlug, updateURL,
  persistAndRefreshShell, updateSessionUsageDisplay, splitHeaderModelEffort, compactHeaderModelLabel, getDefaultProviderName, getDefaultModelForProvider, refreshRelativeTimes, requestHeaders: _unusedRequestHeaders, updateUserNode,
  updateToolNode, updateToolGroupNode, createMessageNode, createToolGroupNode, updateModelSwapNode, renderSidebar, renderMessages, maybeNotifyResponseComplete,
  insertMountedMessageNode, enqueueAssistantStreamUpdate, finalizeAssistantStreamRender, syncTurnActionPanels,
  updateMountedToolGroupNode, updateMountedModelSwapNode, updateMountedUserNode, enqueueMountedAssistantStreamUpdate, finalizeMountedAssistantStreamRender,
  conversationDOMFor, isConversationMounted,
  subscribeToPush, shouldAutoSubscribeToPush, applyTextDirection, shouldSuppressPromptAutoFocus, setSessionOptimisticBusy, setSessionServerActiveRun,
  renderAttachments, buildAttachmentInputParts, cloneAttachmentForMessage
} = app;
const { requestHeaders, normalizeError, isSessionVisible, appendStreamMessageNode, updateVisibleUserNode, scrollVisibleStreamToBottom, sendMessage } = app;

const queueInterruptFollowUp = (sessionId, prompt, messageId, attachments = []) => {
  const normalizedSessionId = String(sessionId || '').trim();
  if (!normalizedSessionId) return;
  const normalizedMessageId = String(messageId || '').trim();
  if (normalizedMessageId && state.queuedInterrupts.some(entry => (
    entry.sessionId === normalizedSessionId && entry.messageId === normalizedMessageId
  ))) {
    return;
  }
  state.queuedInterrupts.push({ sessionId: normalizedSessionId, prompt, messageId, attachments: Array.isArray(attachments) ? attachments : [] });
};

const trackPendingInterruptCommit = (sessionId, prompt, messageId, attachments = []) => {
  state.pendingInterruptCommits = state.pendingInterruptCommits.filter(entry => entry.messageId !== messageId);
  state.pendingInterruptCommits.push({ sessionId, prompt, messageId, attachments: Array.isArray(attachments) ? attachments : [] });
};

const resolvePendingInterruptCommitById = (messageId) => {
  if (!messageId) return null;
  const idx = state.pendingInterruptCommits.findIndex(entry => entry.messageId === messageId);
  if (idx < 0) return null;
  const [entry] = state.pendingInterruptCommits.splice(idx, 1);
  return entry;
};

const discardPendingInterruptCommit = (messageId) => {
  if (!messageId) return;
  state.pendingInterruptCommits = state.pendingInterruptCommits.filter(entry => entry.messageId !== messageId);
};

const requeueUncommittedInterrupts = (session) => {
  if (!session?.id) return;
  const remaining = [];
  for (const entry of state.pendingInterruptCommits) {
    if (entry.sessionId !== session.id) {
      remaining.push(entry);
      continue;
    }
    queueInterruptFollowUp(session.id, entry.prompt, entry.messageId, entry.attachments);
  }
  state.pendingInterruptCommits = remaining;
};

const drainInterruptQueueIfIdle = (session) => {
  if (!session || session.id !== state.activeSessionId) return;
  if (state.streaming || state.abortController) return;
  requeueUncommittedInterrupts(session);
  requeuePendingInterjections(session);
  const queuedSkillIndex = Array.isArray(state.queuedSkillInvocations)
    ? state.queuedSkillInvocations.findIndex((entry) => entry.sessionId === session.id)
    : -1;
  if (queuedSkillIndex >= 0) {
    const [queued] = state.queuedSkillInvocations.splice(queuedSkillIndex, 1);
    void app.invokeSkill(session, queued.invocation, { reuseMessageId: queued.messageId });
    return;
  }
  const queuedIndex = state.queuedInterrupts.findIndex(entry => entry.sessionId === session.id);
  if (queuedIndex >= 0) {
    const [queued] = state.queuedInterrupts.splice(queuedIndex, 1);
    elements.promptInput.value = queued.prompt;
    app.autoGrowPrompt();
    void sendMessage({ prompt: queued.prompt, attachments: queued.attachments || [], reuseMessageId: queued.messageId, _skipContinuationRefresh: true });
  }
};

const setInterruptMessageState = (session, messageId, interruptState) => {
  if (!messageId) return;
  const normalized = sanitizeInterruptState(interruptState);
  if (!normalized) return;
  const message = window.TermLLMConversation.sessionMessages(session).find(m => m.id === messageId && m.role === 'user');
  if (!message) return;
  message.interruptState = normalized;
  updateVisibleUserNode(session, message);
};

// Transition an interjection to a lifecycle phase, updating both the inline
// badge and the pending banner from the single INTERJECTION_PHASE spec so the
// two views cannot drift out of sync. A null banner clears the pending entry
// (no longer cancellable); otherwise the banner action is updated in place.
const setInterjectionPhase = (session, messageId, phase) => {
  const spec = INTERJECTION_PHASE[phase];
  if (!spec) return;
  setInterruptMessageState(session, messageId, spec.badge);
  if (spec.banner === null) {
    removePendingInterjectionById(messageId);
  } else {
    updatePendingInterjectionAction(messageId, spec.banner);
  }
};

const addInlineInterruptMessage = (session, prompt, messageId, interruptState, attachments = []) => {
  const normalized = sanitizeInterruptState(interruptState) || 'evaluating';
  const message = {
    id: messageId || generateId('msg'),
    clientMessageId: messageId || '',
    role: 'user',
    content: prompt,
    created: Date.now(),
    interruptState: normalized
  };
  if (Array.isArray(attachments) && attachments.length > 0) {
    message.attachments = attachments.map(cloneAttachmentForMessage);
  }
  app.trackPendingIntent?.(session, message);

  if (isSessionVisible(session)) {
    const emptyState = elements.messages.querySelector('.empty-state');
    if (emptyState) emptyState.remove();
  }
  appendStreamMessageNode(session, message);
  if (isSessionVisible(session)) syncTurnActionPanels();
  return message;
};

const PENDING_INTERJECTION_LABELS = {
  deciding: 'deciding…',
  interject: 'will incorporate',
  queue: 'queued',
  cancel: 'cancelling'
};

const truncateForBanner = (text, max = 80) => {
  const value = String(text || '').replace(/\s+/g, ' ').trim();
  if (value.length <= max) return value;
  return value.slice(0, max - 1) + '…';
};

const refreshPendingInterjectionBanner = () => {
  const banner = elements.pendingInterjectionBanner;
  if (!banner) return;
  const activeId = String(state.activeSessionId || '').trim();
  if (!activeId) {
    banner.classList.add('hidden');
    banner.innerHTML = '';
    return;
  }
  let latest = null;
  for (const entry of state.pendingInterjections) {
    if (entry.sessionId !== activeId) continue;
    latest = entry;
  }
  if (!latest) {
    banner.classList.add('hidden');
    banner.innerHTML = '';
    return;
  }
  const label = PENDING_INTERJECTION_LABELS[latest.action] || PENDING_INTERJECTION_LABELS.deciding;
  banner.innerHTML = '';
  const icon = document.createElement('span');
  icon.className = 'pending-interjection-icon';
  icon.textContent = '⏳';
  const text = document.createElement('span');
  text.className = 'pending-interjection-text';
  text.textContent = truncateForBanner(latest.prompt);
  const tag = document.createElement('span');
  tag.className = 'pending-interjection-label';
  tag.textContent = `(${label})`;
  banner.appendChild(icon);
  banner.appendChild(text);
  banner.appendChild(tag);
  if (latest.action === 'interject' || latest.action === 'queue') {
    const cancel = document.createElement('button');
    cancel.type = 'button';
    cancel.className = 'pending-interjection-cancel';
    cancel.textContent = 'Cancel';
    cancel.addEventListener('click', () => cancelPendingInterjection(latest));
    banner.appendChild(cancel);
  }
  banner.classList.remove('hidden');
};

const trackPendingInterjection = (sessionId, prompt, messageId, action, attachments = []) => {
  if (!sessionId || !messageId) return;
  const existing = state.pendingInterjections.find(entry => entry.messageId === messageId);
  if (existing) {
    existing.prompt = prompt;
    existing.action = action;
    existing.attachments = Array.isArray(attachments) ? attachments : [];
  } else {
    state.pendingInterjections.push({ sessionId, prompt, messageId, action, attachments: Array.isArray(attachments) ? attachments : [] });
  }
  refreshPendingInterjectionBanner();
};

const updatePendingInterjectionAction = (messageId, action) => {
  if (!messageId) return;
  const entry = state.pendingInterjections.find(item => item.messageId === messageId);
  if (!entry) return;
  entry.action = action;
  refreshPendingInterjectionBanner();
};

const removePendingInterjectionById = (messageId) => {
  if (!messageId) return null;
  const idx = state.pendingInterjections.findIndex(entry => entry.messageId === messageId);
  if (idx < 0) return null;
  const [entry] = state.pendingInterjections.splice(idx, 1);
  refreshPendingInterjectionBanner();
  return entry;
};

const cancelPendingInterjection = async (entry) => {
  if (!entry?.sessionId || !entry?.messageId) return;
  try {
    const response = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(entry.sessionId)}/interjections/${encodeURIComponent(entry.messageId)}`, {
      method: 'DELETE',
      headers: requestHeaders(entry.sessionId)
    });
    if (!response.ok) throw await normalizeError(response);
    removePendingInterjectionById(entry.messageId);
    const session = state.sessions.find(s => s.id === entry.sessionId);
    if (session) {
      app.retirePendingIntent?.(session, entry.messageId);
      if (isSessionVisible(session)) {
        const node = Array.from(elements.messages.querySelectorAll('[data-message-id]'))
          .find(el => el.getAttribute('data-message-id') === entry.messageId);
        if (node) node.remove();
      }
    }
    persistAndRefreshShell();
  } catch (err) {
    alert(err?.message || 'Unable to cancel interjection. It may already have been submitted.');
  }
};

const discardPendingInterruptStateForSession = (session) => {
  if (!session?.id) return;
  state.pendingInterjections = state.pendingInterjections.filter(entry => entry.sessionId !== session.id);
  state.pendingInterruptCommits = state.pendingInterruptCommits.filter(entry => entry.sessionId !== session.id);
  refreshPendingInterjectionBanner();
};

const requeuePendingInterjections = (session) => {
  if (!session?.id) return;
  const remaining = [];
  for (const entry of state.pendingInterjections) {
    if (entry.sessionId !== session.id) {
      remaining.push(entry);
      continue;
    }
    queueInterruptFollowUp(session.id, entry.prompt, entry.messageId, entry.attachments);
  }
  state.pendingInterjections = remaining;
  refreshPendingInterjectionBanner();
};

const interruptActiveRun = async (session, prompt, messageId, contentParts = null, attachments = []) => {
  const body = Array.isArray(contentParts) && contentParts.length > 0
    ? { message: prompt, content: prompt ? [...contentParts, { type: 'input_text', text: prompt }] : contentParts, interjection_id: messageId, client_message_id: messageId }
    : { message: prompt, interjection_id: messageId, client_message_id: messageId };
  const headers = requestHeaders(session.id);
  headers['Idempotency-Key'] = `interrupt_${messageId}`;
  const response = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(session.id)}/interrupt`, {
    method: 'POST',
    headers,
    body: JSON.stringify(body)
  });
  if (!response.ok) {
    throw await normalizeError(response);
  }

  const payload = await response.json();
  const actionRaw = String(payload.action || 'queue').toLowerCase();
  const action = (actionRaw === 'interject' || actionRaw === 'cancel' || actionRaw === 'queue')
    ? actionRaw
    : 'queue';

  if (action === 'interject') {
    // The engine has only *queued* the interjection at this point; it remains
    // cancellable (banner "will incorporate") until drainInterjections() commits
    // it and emits response.interjection, which advances it to the committed
    // phase ("✓ injected"). See INTERJECTION_PHASE in app-core.
    setInterjectionPhase(session, messageId, 'queued');
  } else {
    setInterjectionPhase(session, messageId, action === 'cancel' ? 'willCancel' : 'willQueue');
  }

  if (action === 'cancel' || action === 'queue') {
    queueInterruptFollowUp(session.id, prompt, messageId, attachments);
  }
  if (action === 'cancel') {
    state.expectCanceledRun = true;
  }

  saveSessions();
  scrollVisibleStreamToBottom(session, true);
  return action;
};

const runtimeStateFromSyncResult = (result) => (
  result?.kind === 'ok' ? result.state : (result?.kind ? null : result)
);

const runtimeHasActiveRun = (syncResult) => {
  const runtimeState = runtimeStateFromSyncResult(syncResult);
  if (!runtimeState || typeof runtimeState !== 'object') return false;
  return Boolean(runtimeState.active_run || String(runtimeState.active_response_id || '').trim());
};

Object.assign(app, {
  queueInterruptFollowUp,
  trackPendingInterruptCommit,
  resolvePendingInterruptCommitById,
  discardPendingInterruptCommit,
  requeueUncommittedInterrupts,
  drainInterruptQueueIfIdle,
  setInterruptMessageState,
  setInterjectionPhase,
  addInlineInterruptMessage,
  PENDING_INTERJECTION_LABELS,
  truncateForBanner,
  refreshPendingInterjectionBanner,
  trackPendingInterjection,
  updatePendingInterjectionAction,
  removePendingInterjectionById,
  cancelPendingInterjection,
  discardPendingInterruptStateForSession,
  requeuePendingInterjections,
  interruptActiveRun,
  runtimeStateFromSyncResult,
  runtimeHasActiveRun
});
})();
