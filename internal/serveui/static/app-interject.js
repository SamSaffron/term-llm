'use strict';

(function initAppInterject() {

const app = window.TermLLMApp;
const {
  UI_PREFIX, state, elements, INTERJECTION_PHASE, saveSessions, persistAndRefreshShell, conversationDOMFor,
  cloneAttachmentForMessage, getAttachmentImageDimensions
} = app;
const {
  requestHeaders, normalizeError, isSessionVisible, sendMessage
} = app;

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

const retireUnownedInterjectionIntents = (session) => {
  if (!session?.id) return 0;
  const tracked = new Set();
  for (const entry of state.pendingInterruptCommits) {
    if (entry.sessionId === session.id && entry.messageId) tracked.add(entry.messageId);
  }
  for (const entry of state.pendingInterjections) {
    if (entry.sessionId === session.id && entry.messageId) tracked.add(entry.messageId);
  }

  const retiredIds = [];
  for (const message of window.TermLLMConversation.sessionMessages(session)) {
    if (message?.role !== 'user'
      || (message.interruptState !== 'evaluating' && message.interruptState !== 'error')
      || tracked.has(message.id)) continue;
    const removed = app.retirePendingIntent?.(session, message.id) || [];
    if (removed.length > 0) retiredIds.push(message.id);
  }
  if (retiredIds.length === 0) return 0;

  app.refreshSessionMessagesFromTranscript?.(session);
  const conversationDOM = conversationDOMFor?.(session);
  if (conversationDOM) {
    for (const node of conversationDOM.querySelectorAll('[data-message-id]')) {
      if (retiredIds.includes(node.getAttribute('data-message-id'))) node.remove();
    }
  }
  return retiredIds.length;
};

const requeueUncommittedInterrupts = (session) => {
  if (!session?.id) return;
  const cancelled = new Set(state.pendingInterjections
    .filter((entry) => entry.sessionId === session.id && entry.cancelRequested)
    .map((entry) => entry.messageId));
  const remaining = [];
  for (const entry of state.pendingInterruptCommits) {
    if (entry.sessionId !== session.id) {
      remaining.push(entry);
      continue;
    }
    if (cancelled.has(entry.messageId)) continue;
    queueInterruptFollowUp(session.id, entry.prompt, entry.messageId, entry.attachments);
  }
  state.pendingInterruptCommits = remaining;
};
const drainingFollowUpSessions = new Set();
const drainInterruptQueueIfIdle = async (session) => {
  const sessionId = String(session?.id || '').trim();
  if (!sessionId || sessionId !== state.activeSessionId || drainingFollowUpSessions.has(sessionId) || state.streaming || state.abortController) return;
  drainingFollowUpSessions.add(sessionId);
  try {
    await app.awaitTranscriptTerminalHandoff?.(session);
    if (sessionId !== state.activeSessionId || state.streaming || state.abortController || session.activeResponseId) return;
    requeueUncommittedInterrupts(session);
    requeuePendingInterjections(session);
    const queuedSkillIndex = Array.isArray(state.queuedSkillInvocations) ? state.queuedSkillInvocations.findIndex((entry) => entry.sessionId === sessionId) : -1;
    if (queuedSkillIndex >= 0) {
      const [queued] = state.queuedSkillInvocations.splice(queuedSkillIndex, 1);
      void app.invokeSkill(session, queued.invocation, { reuseMessageId: queued.messageId });
      return;
    }
    const queued = state.queuedInterrupts.filter((entry) => entry.sessionId === sessionId);
    if (queued.length === 0) return;
    state.queuedInterrupts = state.queuedInterrupts.filter((entry) => entry.sessionId !== sessionId);
    let markTransferred;
    const transferred = new Promise((resolve) => { markTransferred = resolve; });
    const sending = sendMessage({ followUps: queued, _onTransportStarted: markTransferred });
    await Promise.race([sending, transferred]);
  } finally { drainingFollowUpSessions.delete(sessionId); }
};
// Transition pending composer state from the shared lifecycle spec. Pending
// interjections are deliberately not transcript intents; the committed event is
// the only transition that materializes a stream message.
const setInterjectionPhase = (_session, messageId, phase) => {
  const spec = INTERJECTION_PHASE[phase];
  if (!spec) return;
  if (spec.banner === null) {
    removePendingInterjectionById(messageId);
  } else {
    updatePendingInterjectionAction(messageId, spec.banner);
  }
};

const mergeCommittedInterjectionAttachments = (localAttachments, committedAttachments) => {
  const local = Array.isArray(localAttachments) ? localAttachments : [];
  const committed = Array.isArray(committedAttachments) ? committedAttachments : [];
  return Array.from({ length: Math.max(local.length, committed.length) }, (_, index) => {
    if (!local[index]) return cloneAttachmentForMessage(committed[index]);
    const attachment = cloneAttachmentForMessage(local[index]), metadata = committed[index];
    const dimensions = getAttachmentImageDimensions(metadata);
    if (dimensions) Object.assign(attachment, dimensions);
    for (const key of ['previewURL', 'dataURL']) if (metadata?.[key]) attachment[key] = String(metadata[key]);
    return attachment;
  });
};

const materializeCommittedInterjection = (session, messageId, committedMessage = null) => {
  const id = String(messageId || '').trim();
  if (!session || !id) return null;
  const intents = session.transcript?.conversation?.intents;
  const existing = intents?.get?.(id);
  if (existing) {
    existing.interruptState = 'interject';
    if (Array.isArray(committedMessage?.attachments) && committedMessage.attachments.length > 0) {
      existing.attachments = mergeCommittedInterjectionAttachments(existing.attachments, committedMessage.attachments);
    }
    return existing;
  }

  const pending = state.pendingInterruptCommits.find((entry) => entry.messageId === id)
    || state.pendingInterjections.find((entry) => entry.messageId === id);
  const prompt = String(committedMessage?.content ?? committedMessage?.text ?? pending?.prompt ?? '');
  const message = {
    id,
    clientMessageId: id,
    role: 'user',
    content: prompt,
    created: Number(committedMessage?.created) || Date.now(),
    interruptState: 'interject'
  };
  const committedAttachments = Array.isArray(committedMessage?.attachments) ? committedMessage.attachments : [], localAttachments = Array.isArray(pending?.attachments) ? pending.attachments : [];
  const attachments = mergeCommittedInterjectionAttachments(localAttachments, committedAttachments);
  if (attachments.length > 0) message.attachments = attachments;
  return app.trackPendingIntent?.(session, message) || message;
};

const commitPendingInterjection = (session, messageId, committedMessage = null) => {
  const id = String(messageId || '').trim();
  if (!id) return null;
  const intent = materializeCommittedInterjection(session, id, committedMessage);
  removePendingInterjectionById(id, { refresh: false });
  resolvePendingInterruptCommitById(id);
  state.queuedInterrupts = state.queuedInterrupts.filter((entry) => entry.messageId !== id);
  return intent;
};

const PENDING_INTERJECTION_LABELS = {
  deciding: 'deciding…',
  interject: 'will incorporate',
  queue: 'queued',
  cancel_queue: 'cancelling response; queued',
  cancel: 'cancelling'
};

const truncateForBanner = (text, max = 80) => {
  const value = String(text || '').replace(/\s+/g, ' ').trim();
  if (value.length <= max) return value;
  return value.slice(0, max - 1) + '…';
};

const createPendingInterjectionRow = (entry) => {
  const row = document.createElement('div');
  row.className = 'pending-interjection-row';
  row.dataset.messageId = entry.messageId;
  row.setAttribute('role', 'listitem');

  const icon = document.createElement('span');
  icon.className = 'pending-interjection-icon';
  icon.textContent = '⏳';
  const text = document.createElement('span');
  text.className = 'pending-interjection-text';
  text.textContent = truncateForBanner(entry.prompt);
  const tag = document.createElement('span');
  tag.className = 'pending-interjection-label';
  const label = PENDING_INTERJECTION_LABELS[entry.action] || PENDING_INTERJECTION_LABELS.deciding;
  tag.textContent = `(${label})`;
  row.appendChild(icon);
  row.appendChild(text);
  row.appendChild(tag);

  if (entry.action !== 'cancel') {
    const cancel = document.createElement('button');
    cancel.type = 'button';
    cancel.className = 'pending-interjection-cancel';
    cancel.textContent = 'Cancel';
    cancel.setAttribute('aria-label', `Cancel pending message: ${truncateForBanner(entry.prompt, 40)}`);
    cancel.addEventListener('click', () => cancelPendingInterjection(entry));
    row.appendChild(cancel);
  }
  return row;
};

const refreshPendingInterjectionBanner = () => {
  const banner = elements.pendingInterjectionBanner;
  if (!banner) return;
  const activeId = String(state.activeSessionId || '').trim();
  const pending = activeId
    ? state.pendingInterjections.filter((entry) => entry.sessionId === activeId)
    : [];
  banner.innerHTML = '';
  if (pending.length === 0) {
    banner.classList.add('hidden');
    return;
  }
  banner.setAttribute('role', 'list');
  banner.setAttribute('aria-label', 'Pending messages');
  for (const entry of pending) banner.appendChild(createPendingInterjectionRow(entry));
  banner.classList.remove('hidden');
  banner.scrollTop = banner.scrollHeight;
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

const removePendingInterjectionById = (messageId, options = {}) => {
  if (!messageId) return null;
  const idx = state.pendingInterjections.findIndex(entry => entry.messageId === messageId);
  if (idx < 0) return null;
  const [entry] = state.pendingInterjections.splice(idx, 1);
  if (options.refresh !== false) refreshPendingInterjectionBanner();
  return entry;
};

const cancelPendingInterjection = async (entry) => {
  if (!entry?.sessionId || !entry?.messageId) return;
  if (!entry.transportStarted) {
    removePendingInterjectionById(entry.messageId);
    discardPendingInterruptCommit(entry.messageId);
    state.queuedInterrupts = state.queuedInterrupts.filter((queued) => queued.messageId !== entry.messageId);
    return;
  }
  if (entry.deliveryPending || entry.action === 'deciding') {
    entry.cancelRequested = true;
    entry.action = 'cancel';
    discardPendingInterruptCommit(entry.messageId);
    state.queuedInterrupts = state.queuedInterrupts.filter((queued) => queued.messageId !== entry.messageId);
    refreshPendingInterjectionBanner();
    return;
  }
  try {
    if (entry.action === 'interject') {
      const response = await app.apiFetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(entry.sessionId)}/interjections/${encodeURIComponent(entry.messageId)}`, {
        method: 'DELETE',
        headers: requestHeaders(entry.sessionId)
      }, { policy: app.API_FETCH_POLICY.idempotentMutation });
      if (!response.ok) throw await normalizeError(response);
    }
    removePendingInterjectionById(entry.messageId);
    discardPendingInterruptCommit(entry.messageId);
    state.queuedInterrupts = state.queuedInterrupts.filter((queued) => queued.messageId !== entry.messageId);
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

const requeuePendingInterjections = (session) => {
  if (!session?.id) return;
  for (const entry of state.pendingInterjections) {
    if (entry.sessionId !== session.id || entry.cancelRequested) continue;
    entry.action = 'queue';
    discardPendingInterruptCommit(entry.messageId);
    queueInterruptFollowUp(session.id, entry.prompt, entry.messageId, entry.attachments);
  }
  refreshPendingInterjectionBanner();
};

const interruptActiveRunNow = async (session, prompt, messageId, contentParts = null, attachments = []) => {
  const body = Array.isArray(contentParts) && contentParts.length > 0
    ? { message: prompt, content: prompt ? [...contentParts, { type: 'input_text', text: prompt }] : contentParts, interjection_id: messageId, client_message_id: messageId, delivery: 'steer' }
    : { message: prompt, interjection_id: messageId, client_message_id: messageId, delivery: 'steer' };
  const headers = requestHeaders(session.id);
  headers['Idempotency-Key'] = `interrupt_${messageId}`;
  const response = await app.apiFetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(session.id)}/interrupt`, {
    method: 'POST',
    headers,
    body: JSON.stringify(body)
  }, { policy: app.API_FETCH_POLICY.idempotentMutation });
  if (!response.ok) {
    throw await normalizeError(response);
  }

  const payload = await response.json();
  const actionRaw = String(payload.action || 'queue').toLowerCase();
  const action = (actionRaw === 'interject' || actionRaw === 'cancel' || actionRaw === 'queue')
    ? actionRaw
    : 'queue';

  const pendingEntry = state.pendingInterjections.find((entry) => entry.messageId === messageId);
  if (pendingEntry?.cancelRequested) {
    if (action === 'interject') {
      const cancelResponse = await app.apiFetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(session.id)}/interjections/${encodeURIComponent(messageId)}`, {
        method: 'DELETE',
        headers: requestHeaders(session.id)
      }, { policy: app.API_FETCH_POLICY.idempotentMutation });
      if (!cancelResponse.ok) throw await normalizeError(cancelResponse);
    }
    removePendingInterjectionById(messageId);
    discardPendingInterruptCommit(messageId);
    state.queuedInterrupts = state.queuedInterrupts.filter((entry) => entry.messageId !== messageId);
    saveSessions();
    return 'discarded';
  }

  if (action === 'interject') {
    // The engine has only queued the interjection at this point; keep it in
    // the cancellable composer banner until response.interjection commits it.
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
  return action;
};

const interruptMutationTails = new Map();

// Serialize interrupt delivery per session. The stack is updated immediately,
// but server queue ownership follows the same FIFO order the user sees above
// the composer.
const interruptActiveRun = (session, prompt, messageId, contentParts = null, attachments = []) => {
  const sessionId = String(session?.id || '').trim();
  const previous = interruptMutationTails.get(sessionId) || Promise.resolve();
  const request = previous.then(() => {
    const entry = state.pendingInterjections.find((item) => item.messageId === messageId);
    if (!entry || entry.cancelRequested) {
      removePendingInterjectionById(messageId);
      discardPendingInterruptCommit(messageId);
      state.queuedInterrupts = state.queuedInterrupts.filter((item) => item.messageId !== messageId);
      return 'discarded';
    }
    if (entry) {
      entry.transportStarted = true;
      entry.deliveryPending = true;
    }
    return interruptActiveRunNow(session, prompt, messageId, contentParts, attachments).finally(() => {
      if (entry) entry.deliveryPending = false;
    });
  });
  const tail = request.then(() => undefined, () => undefined).finally(() => {
    if (interruptMutationTails.get(sessionId) === tail) interruptMutationTails.delete(sessionId);
  });
  interruptMutationTails.set(sessionId, tail);
  return request;
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
  retireUnownedInterjectionIntents,
  requeueUncommittedInterrupts,
  drainInterruptQueueIfIdle,
  setInterjectionPhase,
  materializeCommittedInterjection,
  commitPendingInterjection,
  PENDING_INTERJECTION_LABELS,
  truncateForBanner,
  refreshPendingInterjectionBanner,
  trackPendingInterjection,
  updatePendingInterjectionAction,
  removePendingInterjectionById,
  cancelPendingInterjection,
  requeuePendingInterjections,
  interruptActiveRun,
  runtimeStateFromSyncResult,
  runtimeHasActiveRun
});
})();
