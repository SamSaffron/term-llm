'use strict';

(function initAppSend() {

const app = window.TermLLMApp;
const {
  UI_PREFIX, STORAGE_KEYS, state, elements, generateId, truncate, saveSessions, getActiveSession, createSession,
  setConnectionState, sessionSlug, updateURL, persistAndRefreshShell, refreshRelativeTimes, renderMessages,
  syncTurnActionPanels, setSessionOptimisticBusy, sessionHasInProgressState, renderAttachments, buildAttachmentInputParts,
  cloneAttachmentForMessage
} = app;
const DRAFT_MESSAGE_LIMIT = 10;
const draftMessagesStorageKey = () => STORAGE_KEYS.draftMessages || 'term_llm_draft_messages';

const loadDraftMessages = () => {
  try {
    const parsed = JSON.parse(localStorage.getItem(draftMessagesStorageKey()) || '[]');
    if (!Array.isArray(parsed)) return [];
    return parsed
      .map((item) => ({
        id: String(item?.id || '').trim(),
        sessionId: String(item?.sessionId || '').trim(),
        prompt: String(item?.prompt || ''),
        created: Number(item?.created || 0) || Date.now()
      }))
      .filter((item) => item.id && item.prompt.trim());
  } catch {
    return [];
  }
};

const saveDraftMessages = (drafts) => {
  const cleaned = (Array.isArray(drafts) ? drafts : [])
    .filter((item) => item?.id && String(item?.prompt || '').trim())
    .sort((a, b) => Number(b.created || 0) - Number(a.created || 0))
    .slice(0, DRAFT_MESSAGE_LIMIT);
  try {
    localStorage.setItem(draftMessagesStorageKey(), JSON.stringify(cleaned));
  } catch (err) {
    console.warn('[drafts] failed to save draft messages', err);
  }
  return cleaned;
};

const stageDraftMessage = (prompt, sessionId = '', draftId = '') => {
  const trimmed = String(prompt || '').trim();
  if (!trimmed) return '';
  const id = draftId || generateId('draft');
  const normalizedSessionId = String(sessionId || '').trim();
  const next = loadDraftMessages().filter((item) => (
    item.id !== id && String(item.sessionId || '').trim() !== normalizedSessionId
  ));
  next.unshift({
    id,
    sessionId: normalizedSessionId,
    prompt: trimmed,
    created: Date.now()
  });
  saveDraftMessages(next);
  return id;
};

const removeDraftMessage = (draftId) => {
  const id = String(draftId || '').trim();
  if (!id) return;
  saveDraftMessages(loadDraftMessages().filter((item) => item.id !== id));
};

const clearDraftMessageForSession = (sessionId = state.activeSessionId) => {
  const normalizedSessionId = String(sessionId || '').trim();
  saveDraftMessages(loadDraftMessages().filter((item) => (
    String(item.sessionId || '').trim() !== normalizedSessionId
  )));
};

const restoreDraftMessageForSession = (sessionId = state.activeSessionId, options = {}) => {
  if (!options.replace && String(elements.promptInput.value || '').trim()) return false;
  const id = String(sessionId || '').trim();
  const drafts = loadDraftMessages();
  const draft = drafts.find((item) => String(item.sessionId || '').trim() === id);
  if (!draft) {
    if (options.replace) {
      elements.promptInput.value = '';
      app.autoGrowPrompt();
    }
    return false;
  }
  elements.promptInput.value = draft.prompt;
  app.autoGrowPrompt();
  return true;
};

const restoreLatestDraftMessage = () => {
  return restoreDraftMessageForSession(state.activeSessionId);
};

const {
  requestHeaders, normalizeError, hasSessionContinuationContext, effectiveEffortForCompare,
  clearRuntimeSelectionIntent, classifyRecoverableContinuationFailure, streamReconnectDelay, streamReconnectLabel,
  isTransientPreResponsePostError, setActiveResponseTracking, HEARTBEAT_STALE_THRESHOLD,
  heartbeatUploadGraceThreshold, attachResponseStream, detachResponseStream, isSessionVisible,
  appendStreamMessageNode, updateVisibleUserNode, finalizeVisibleAssistantStreamRender, scrollVisibleStreamToBottom,
  createResponseStreamState, consumeResponseStream, resumeActiveResponse, setStreaming, recoverInterruptFailure,
  addErrorMessage, waitForNetworkRetry
} = app;

const restoreQueuedFollowUps = (entries, sessionId) => {
  for (const entry of entries) {
    if (!state.queuedInterrupts.some((queued) => queued.sessionId === sessionId && queued.messageId === entry.messageId)) state.queuedInterrupts.push(entry);
  }
};
const sendMessage = async (options = {}) => {
  let followUps = Array.isArray(options.followUps)
    ? options.followUps.filter((entry) => entry && String(entry.messageId || '').trim()) : [];
  const cancellableFollowUpIDs = new Set(state.pendingInterjections
    .filter((entry) => followUps.some((queued) => queued.messageId === entry.messageId)).map((entry) => entry.messageId));
  const batchingFollowUps = followUps.length > 0;
  const promptSource = batchingFollowUps ? followUps[followUps.length - 1].prompt
    : (typeof options.prompt === 'string' ? options.prompt : elements.promptInput.value);
  const prompt = String(promptSource || '').trim();
  const pendingAttachments = batchingFollowUps ? []
    : (Array.isArray(options.attachments) ? [...options.attachments] : [...state.attachments]);

  if (!batchingFollowUps && !prompt && pendingAttachments.length === 0) return;

  if (!state.connected) {
    if (batchingFollowUps) restoreQueuedFollowUps(followUps, followUps[0]?.sessionId);
    app.openAuthModal('Connect before sending a message.', true);
    return;
  }
  if (!batchingFollowUps && app.handleBranchSlashCommand?.(prompt)) return;
  if (!batchingFollowUps && /^\/(goal|mcp|model|new|tree)$/i.test(prompt)) {
    const command = prompt.toLowerCase();
    elements.promptInput.value = '';
    app.hideSlashCommands?.();
    app.autoGrowPrompt();
    switch (command) {
      case '/goal':
        app.openGoalModal?.();
        break;
      case '/mcp':
        await app.openSessionMCPModal?.();
        break;
      case '/model':
        elements.chipModelTrigger?.click();
        break;
      case '/new':
        await app.createAndSwitchToFreshSession?.();
        break;
      case '/tree':
        await app.openBranchTree?.();
        break;
    }
    return;
  }
  if (!batchingFollowUps && /^\/(undo|redo)\b/i.test(prompt)) {
    const match = prompt.match(/^\/(undo|redo)$/i);
    elements.promptInput.value = '';
    app.hideSlashCommands?.();
    app.autoGrowPrompt();
    if (!match) {
      app.showToast?.('Usage: /undo or /redo', { id: 'transcript-mutation', tone: 'warning' });
      return;
    }
    const operation = match[1].toLowerCase();
    const session = getActiveSession();
    if (!session || state.draftSessionActive) {
      app.showToast?.(`Start the conversation before using /${operation}.`, { id: 'transcript-mutation', tone: 'warning' });
      return;
    }
    if (state.transcriptMutating) return;
    if (state.streaming || state.compressing || state.sideQuestion?.running || sessionHasInProgressState?.(session)) {
      app.showToast?.(`Cannot ${operation} while work is active.`, { id: 'transcript-mutation', tone: 'warning' });
      return;
    }
    const transcript = session.transcript;
    const expectedRev = Math.max(0, Number(transcript?.rev) || 0);
    const ids = Array.isArray(transcript?.ids) ? transcript.ids : [];
    const expectedHeadId = ids.length > 0 ? Number(ids[ids.length - 1]) || 0 : 0;
    state.transcriptMutating = true;
    try {
      const response = await app.apiFetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(session.id)}/runtime/${operation}`, {
        method: 'POST',
        headers: requestHeaders(session.id),
        body: JSON.stringify({ expected_rev: expectedRev, expected_head_id: expectedHeadId })
      }, { policy: app.API_FETCH_POLICY.mutation });
      if (!response.ok) throw await normalizeError(response);
      const payload = await response.json();
      await app.refreshActiveSessionMessagesFromServer?.(session, {
        force: true,
        useEtag: false,
        forceScroll: true,
        reason: operation
      });
      await app.syncActiveSessionFromServer?.(session, false, { skipMessagesFetch: true });
      const sameActiveSession = !state.draftSessionActive
        && state.activeSessionId === session.id
        && getActiveSession() === session;
      if (operation === 'undo') {
        if (sameActiveSession) {
          elements.promptInput.value = String(payload?.user_text || '');
          app.autoGrowPrompt();
          elements.promptInput.focus?.();
        }
        const attachmentsOmitted = Boolean(payload?.attachments_omitted);
        const message = sameActiveSession
          ? (attachmentsOmitted
            ? 'Removed the latest turn. Your prompt is back in the composer. Attachments were not restored.'
            : 'Removed the latest turn. Your prompt is back in the composer.')
          : (attachmentsOmitted
            ? 'Removed the latest turn. Attachments were not restored.'
            : 'Removed the latest turn.');
        app.showToast?.(message, { id: 'transcript-mutation', tone: attachmentsOmitted ? 'warning' : 'success' });
      } else {
        if (sameActiveSession) {
          elements.promptInput.value = '';
          app.autoGrowPrompt();
        }
        app.showToast?.('Restored the undone turn.', { id: 'transcript-mutation', tone: 'success' });
      }
    } catch (err) {
      if (err?.status === 409) await app.refreshActiveSessionMessagesFromServer?.(session, { force: true, useEtag: false, reason: `${operation}-conflict` });
      app.showToast?.(err?.message || `${operation} failed.`, { id: 'transcript-mutation', tone: 'error', duration: 6500 });
    } finally {
      state.transcriptMutating = false;
    }
    return;
  }

  if (!batchingFollowUps && /^\/compact$/i.test(prompt)) {
    elements.promptInput.value = '';
    app.hideSlashCommands?.();
    app.autoGrowPrompt();
    const session = getActiveSession();
    if (!session || state.draftSessionActive) {
      window.alert('Start the conversation before compressing it.');
      return;
    }
    if (state.compressing) return;
    state.compressing = true;
    elements.sendBtn.classList.add('loading');
    elements.sendBtn.disabled = true;
    elements.sendBtn.title = 'Compressing conversation';
    try {
      const response = await app.apiFetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(session.id)}/runtime/compact`, {
        method: 'POST',
        headers: requestHeaders(session.id),
        body: '{}'
      });
      if (!response.ok) {
        const payload = await response.json().catch(() => ({}));
        throw new Error(payload?.error?.message || `Compression failed (${response.status})`);
      }
      await app.refreshActiveSessionMessagesFromServer?.(session, {
        force: true,
        useEtag: false,
        forceScroll: true
      });
    } catch (err) {
      addErrorMessage(err?.message || String(err), session);
    } finally {
      state.compressing = false;
      elements.sendBtn.classList.remove('loading');
      elements.sendBtn.disabled = false;
      app.updateSendButtonState();
    }
    return;
  }

  if (!batchingFollowUps && /^\/side(?:\s|$)/i.test(prompt)) {
    const question = prompt.replace(/^\/side\b/i, '').trim();
    elements.promptInput.value = '';
    app.hideSlashCommands?.();
    if (typeof app.openSideQuestion === 'function') await app.openSideQuestion(question);
    return;
  }

  const contextOperation = state.branchContextOperation;
  if (!batchingFollowUps && contextOperation?.sessionId === state.activeSessionId && !options._releaseBranchContextSend) {
    if (contextOperation.phase === 'creating') {
      app.showToast?.('Creating the new conversation path…', { id: 'conversation-branch' });
      return;
    }
    if (state.branchContextQueuedSend) {
      app.showToast?.('A message is already queued for this path.', { id: 'branch-context-queue', tone: 'warning' });
      return;
    }
    state.branchContextQueuedSend = {
      sessionId: state.activeSessionId,
      prompt,
      attachments: pendingAttachments.map((attachment) => cloneAttachmentForMessage(attachment))
    };
    elements.promptInput.value = '';
    state.attachments = [];
    renderAttachments();
    app.autoGrowPrompt?.();
    const active = getActiveSession();
    if (active?.branchContextStatus) active.branchContextStatus.queued = true;
    app.renderMessages?.(false);
    return;
  }

  let session = getActiveSession();
  const branchIntent = !batchingFollowUps && session && !state.draftSessionActive
    && state.pendingBranch?.sourceSessionId === session.id
    ? { ...state.pendingBranch }
    : null;
  const branchSourceSession = branchIntent ? session : null;
  const skillInvocation = !batchingFollowUps && pendingAttachments.length === 0
    ? app.matchSkillInvocation?.(prompt)
    : null;
  const heartbeatPostRetryCount = Math.max(0, Number(options._heartbeatPostRetry || 0));
  const retryingHeartbeatPost = heartbeatPostRetryCount > 0 && typeof options.reuseMessageId === 'string';
  const progressEntry = session ? state.sessionProgressById?.[session.id] : null;
  const ownsLiveStream = Boolean(
    session
    && state.currentStreamSessionId === session.id
    && (state.abortController || state.currentStreamResponseId || state.streaming)
  );
  const activeSessionBusy = Boolean(
    session
    && !state.draftSessionActive
    && (session.activeResponseId || progressEntry?.serverActiveRun || ownsLiveStream)
  );
  if (branchIntent && activeSessionBusy) {
    app.showToast?.('Cannot branch while work is active.', { id: 'conversation-branch', tone: 'warning' });
    return;
  }
  if (skillInvocation && session && !state.draftSessionActive) {
    if (activeSessionBusy && skillInvocation.execution !== 'isolated') {
      app.queueMainSkillInvocation(session, skillInvocation);
      return;
    }
    await app.invokeSkill(session, skillInvocation);
    return;
  }
  if (activeSessionBusy && batchingFollowUps && !retryingHeartbeatPost) {
    restoreQueuedFollowUps(followUps, session.id);
    return;
  }
  if (activeSessionBusy && !retryingHeartbeatPost) {
    const pendingMessageId = generateId('msg');
    let requestAttachmentParts = [];
    if (pendingAttachments.length > 0) {
      const controller = new AbortController();
      try {
        requestAttachmentParts = await buildAttachmentInputParts(pendingAttachments, controller.signal);
      } catch (err) {
        try { controller.abort(); } catch {}
        alert(err?.message || 'Failed to read attachment.');
        return;
      }
    }
    stageDraftMessage(prompt, session.id);
    app.trackPendingInterruptCommit(session.id, prompt, pendingMessageId, pendingAttachments);
    app.trackPendingInterjection(session.id, prompt || pendingAttachments[0]?.name || 'Attachment', pendingMessageId, 'interject', pendingAttachments);
    persistAndRefreshShell();
    elements.promptInput.value = '';
    state.attachments = [];
    renderAttachments();
    app.autoGrowPrompt();
    try {
      await app.interruptActiveRun(session, prompt, pendingMessageId, requestAttachmentParts, pendingAttachments);
      clearDraftMessageForSession(session.id);
    } catch (err) {
      if (err?.status && err.status !== 401) {
        try {
          const recovered = await recoverInterruptFailure(session, prompt, pendingMessageId, pendingAttachments);
          if (recovered) {
            return;
          }
        } catch (recoveryErr) {
          err = recoveryErr;
        }
      }
      app.discardPendingInterruptCommit(pendingMessageId);
      app.setInterjectionPhase(session, pendingMessageId, 'failed');
      const message = err?.message || 'Failed to interrupt active run.';
      addErrorMessage(message, session);
      if (err?.status === 401) {
        app.handleAuthFailure();
      }
      elements.promptInput.value = prompt;
      state.attachments = pendingAttachments;
      renderAttachments();
      app.autoGrowPrompt();
      persistAndRefreshShell();
      scrollVisibleStreamToBottom(session, true);
    }
    return;
  }
  const controller = new AbortController();
  controller._heartbeatAbort = false;
  let requestAttachmentParts = [];
  const batchAttachmentParts = new Map();
  try {
    if (batchingFollowUps) {
      for (const entry of followUps) {
        const attachments = Array.isArray(entry.attachments) ? entry.attachments : [];
        batchAttachmentParts.set(entry.messageId, attachments.length > 0
          ? await buildAttachmentInputParts(attachments, controller.signal)
          : []);
      }
    } else if (pendingAttachments.length > 0) {
      requestAttachmentParts = await buildAttachmentInputParts(pendingAttachments, controller.signal);
    }
  } catch (err) {
    try { controller.abort(); } catch {}
    alert(err?.message || 'Failed to read attachment.');
    if (batchingFollowUps) restoreQueuedFollowUps(followUps, session?.id);
    return;
  }
  if (batchingFollowUps) {
    followUps = followUps.filter((queued) => !cancellableFollowUpIDs.has(queued.messageId) || state.pendingInterjections.some((pending) => pending.messageId === queued.messageId && !pending.cancelRequested));
    if (followUps.length === 0) return;
  }
  const wasDraftSessionSend = !session || state.draftSessionActive;
  if (!session) {
    session = createSession();
    state.sessions.unshift(session);
    state.activeSessionId = session.id;
    state.draftSessionActive = false;
    updateURL(sessionSlug(session));
  }
  if (wasDraftSessionSend && session?.id && state.activeSessionId === session.id && elements.messages?.dataset) {
    elements.messages.dataset.sessionId = session.id;
  }
  const shouldRefreshMissingContinuation = !options._skipContinuationRefresh && Boolean(
    session
    && !session.activeResponseId
    && !String(session.lastResponseId || '').trim()
    && hasSessionContinuationContext(session)
  );
  if (shouldRefreshMissingContinuation && typeof app.syncActiveSessionFromServer === 'function') {
    try {
      await app.syncActiveSessionFromServer(session, false, { skipMessagesFetch: true });
      session = getActiveSession() || session;
    } catch (_err) {
    }
    if (state.streaming || session.activeResponseId) {
      return sendMessage({ ...options, _skipContinuationRefresh: true });
    }
  }
  const reuseMessageId = typeof options.reuseMessageId === 'string' ? options.reuseMessageId : '';
  const messageSpecs = batchingFollowUps ? followUps.map((entry) => ({
    prompt: String(entry.prompt || '').trim(),
    attachments: Array.isArray(entry.attachments) ? entry.attachments : [],
    messageId: String(entry.messageId || '').trim(),
    requestParts: batchAttachmentParts.get(entry.messageId) || [],
  })) : [{ prompt, attachments: pendingAttachments, messageId: reuseMessageId, requestParts: requestAttachmentParts }];
  for (const spec of messageSpecs) {
    if (!spec.messageId) continue;
    app.removePendingInterjectionById?.(spec.messageId, batchingFollowUps ? { refresh: false } : undefined);
    app.discardPendingInterruptCommit?.(spec.messageId);
  }
  if (batchingFollowUps) app.refreshPendingInterjectionBanner?.();
  if (!batchingFollowUps) stageDraftMessage(prompt, session.id);
  const userMessages = [];
  for (const spec of messageSpecs) {
    let message = spec.messageId ? window.TermLLMConversation.sessionMessages(session)
      .find((entry) => entry.id === spec.messageId && entry.role === 'user') || null : null;
    const isNew = !message;
    if (!message) {
      message = { id: spec.messageId || generateId('msg'), role: 'user', content: spec.prompt, created: Date.now() };
    } else {
      message.content = spec.prompt;
      delete message.interruptState;
    }
    message.clientMessageId = String(message.clientMessageId || message.id || '').trim();
    if (spec.attachments.length > 0) message.attachments = spec.attachments.map(cloneAttachmentForMessage);
    else delete message.attachments;
    if (isNew) message = app.trackPendingIntent?.(session, message) || message;
    if (isNew) appendStreamMessageNode(session, message);
    else updateVisibleUserNode(session, message);
    userMessages.push(message);
  }
  const userMessage = userMessages[userMessages.length - 1];
  userMessage.responseRequestId = String(userMessage.responseRequestId || options.responseRequestId || generateId('request')).trim();
  let followUpBatchRestored = false;
  const restoreFollowUpBatch = () => {
    if (!batchingFollowUps || followUpBatchRestored) return;
    followUpBatchRestored = true;
    restoreQueuedFollowUps(followUps, session.id);
    for (const spec of messageSpecs) {
      app.trackPendingInterjection?.(session.id, spec.prompt, spec.messageId, 'queue', spec.attachments);
      app.retirePendingIntent?.(session, spec.messageId);
    }
    app.refreshSessionMessagesFromTranscript?.(session);
    if (isSessionVisible(session)) app.renderMessages?.();
  };
  session.lastMessageAt = Date.now();
  if (!session.title || session.title === 'New chat') {
    const firstSpec = messageSpecs[0];
    session.title = truncate(firstSpec.prompt || firstSpec.attachments[0]?.name || 'Image', 60);
  }
  if (isSessionVisible(session)) {
    const hadEmptyState = elements.messages.querySelector('.empty-state');
    if (hadEmptyState) hadEmptyState.remove();
    syncTurnActionPanels();
  }
  setSessionOptimisticBusy(session, true);
  persistAndRefreshShell();
  if (!batchingFollowUps) {
    elements.promptInput.value = '';
    app.hideSlashCommands?.();
    if (!Array.isArray(options.attachments)) {
      state.attachments = [];
      renderAttachments();
    }
    app.autoGrowPrompt();
  }
  scrollVisibleStreamToBottom(session, true);
  state.expectCanceledRun = false;
  const sendGeneration = state.streamGeneration;
  attachResponseStream(session, '', controller);
  setStreaming(true);
  options._onTransportStarted?.();
  app.refreshSidebarStatusPoll?.();
  let streamState = createResponseStreamState(session);
  let previousResponseId = '';
  try {
    const input = messageSpecs.map((spec, index) => {
      let content;
      if (spec.requestParts.length > 0) {
        content = spec.requestParts.slice();
        if (spec.prompt) content.push({ type: 'input_text', text: spec.prompt });
      } else {
        content = spec.prompt;
      }
      return {
        type: 'message',
        role: 'user',
        client_message_id: userMessages[index].clientMessageId,
        content,
      };
    });
    const body = {
      stream: true,
      include_server_tools: true,
      client_message_id: userMessage.clientMessageId,
      input
    };
    previousResponseId = branchIntent
      ? String(branchIntent.previousResponseId || `resp_msg_${Number(branchIntent.anchorMessageId) || 0}`).trim()
      : String(session.lastResponseId || '').trim();
    if (!previousResponseId && session.worktreeDir) {
      body.worktree_dir = session.worktreeDir;
    }
    if (previousResponseId) {
      body.previous_response_id = previousResponseId;
    }
    if (branchIntent) {
      body.branch = true;
      body.expected_rev = Math.max(0, Number(branchIntent.expectedRev) || 0);
      body.idempotency_key = String(branchIntent.idempotencyKey || userMessage.responseRequestId).trim();
      const branchContextMode = String(branchIntent.branchContextMode || 'clean').trim();
      body.branch_context = {
        mode: branchContextMode,
        ...(branchContextMode === 'focused' ? { focus: String(branchIntent.branchContextFocus || '').trim() } : {})
      };
    }
    app.canonicalizeSelectedModelEffort();
    const currentProvider = session.provider || '';
    const currentModel = session.activeModel || '';
    const currentEffort = session.activeEffort || '';
    const hasPriorContext = Boolean(window.TermLLMConversation.sessionMessages(session).length > 1);
    const hasRuntimeSelectionIntent = Boolean(session.runtimeSelectionIntent);
    const useSelectedRuntime = !hasPriorContext || hasRuntimeSelectionIntent;
    const targetProvider = useSelectedRuntime ? (state.selectedProvider || currentProvider) : currentProvider;
    const targetModel = useSelectedRuntime ? (state.selectedModel || currentModel) : currentModel;
    const targetEffort = useSelectedRuntime ? (state.selectedEffort || '') : currentEffort;
    const targetDiffers = hasPriorContext && hasRuntimeSelectionIntent && Boolean(
      (targetProvider || '') !== (currentProvider || '')
      || (targetModel || '') !== (currentModel || '')
      || effectiveEffortForCompare(targetModel || currentModel, targetEffort)
        !== effectiveEffortForCompare(currentModel || targetModel, currentEffort)
    );
    const modeInfo = app.modelMetadataFor(targetModel || currentModel);
    const reasoningModes = Array.isArray(modeInfo?.reasoning_modes) ? modeInfo.reasoning_modes : [];
    const supportsReasoningMode = reasoningModes.includes('pro');
    if (elements.reasoningModeField) elements.reasoningModeField.hidden = !supportsReasoningMode;
    if (supportsReasoningMode) {
      const selectedMode = state.selectedReasoningMode === 'pro' ? 'pro' : 'standard';
      body.reasoning = { mode: selectedMode };
      session.activeReasoningMode = selectedMode;
    } else {
      session.activeReasoningMode = '';
      if (state.selectedReasoningMode === 'pro') {
        state.selectedReasoningMode = 'standard';
        localStorage.setItem(STORAGE_KEYS.selectedReasoningMode, 'standard');
      }
    }
    if (targetModel) {
      body.model = targetModel;
    }
    if (targetDiffers) {
      body.provider = targetProvider || currentProvider;
      if (targetEffort) {
        body.reasoning_effort = targetEffort;
      }
      body.model_swap = { mode: 'auto', fallback: 'handover' };
    } else {
      const activeEffort = useSelectedRuntime ? targetEffort : currentEffort;
      if (activeEffort) {
        body.reasoning_effort = activeEffort;
      }
      if (!session.provider && state.selectedProvider) {
        session.provider = state.selectedProvider;
      }
      if (session.provider) {
        body.provider = session.provider;
      }
    }
    const headers = requestHeaders(session.id);
    headers['Idempotency-Key'] = userMessage.responseRequestId;
    headers['X-Term-LLM-Request-ID'] = userMessage.responseRequestId;
    const requestBody = JSON.stringify(body);
    controller._heartbeatStaleThreshold = heartbeatUploadGraceThreshold(requestBody);
    let response = await app.apiFetch(`${UI_PREFIX}/v1/responses`, {
      method: 'POST',
      headers,
      body: requestBody,
      signal: controller.signal
    }, { policy: app.API_FETCH_POLICY.idempotentMutation, retries: 0, timeoutMs: 0 });
    controller._heartbeatStaleThreshold = HEARTBEAT_STALE_THRESHOLD;
    const headerResponseId = String(response.headers.get('x-response-id') || '').trim();
    const authoritativeSessionId = String(response.headers.get('x-session-id') || '').trim();
    const copiedBranchAnchorId = String(response.headers.get('x-branch-anchor-id') || '').trim();
    const headerSessionNumber = Number(response.headers.get('x-session-number') || 0);
    if (!response.ok) {
      throw await normalizeError(response);
    }
    if (branchIntent) {
      if (!authoritativeSessionId || authoritativeSessionId === branchSourceSession?.id) {
        throw new Error('Branch response did not identify a child session.');
      }
      clearDraftMessageForSession(branchSourceSession.id);
      session = app.adoptBranchedSessionOwnership?.(branchSourceSession, authoritativeSessionId, userMessages, copiedBranchAnchorId) || session;
      streamState = createResponseStreamState(session);
      attachResponseStream(session, headerResponseId, controller);
      if (headerResponseId) setActiveResponseTracking(session, headerResponseId, 0);
      try {
        await app.refreshActiveSessionMessagesFromServer?.(session, {
          force: true, useEtag: false, forceScroll: true, reason: 'branch-ownership'
        });
      } catch (_err) {
        app.showToast?.('New path created, but its notes have not loaded yet. Retrying…', { id: 'branch-notes-refresh', tone: 'warning' });
        setTimeout(() => app.refreshActiveSessionMessagesFromServer?.(session, {
          force: true, useEtag: false, forceScroll: false, reason: 'branch-notes-retry'
        }).catch(() => {}), 1000);
      }
    }
    if (headerSessionNumber > 0 && session.number !== headerSessionNumber) {
      session.number = headerSessionNumber;
      updateURL(sessionSlug(session));
    }
    setConnectionState('', '');
    clearDraftMessageForSession(session.id);
    if (wasDraftSessionSend) {
      clearDraftMessageForSession('');
    }
    if (headerResponseId) {
      setActiveResponseTracking(session, headerResponseId, 0);
      clearRuntimeSelectionIntent(session);
      attachResponseStream(session, headerResponseId, controller);
      saveSessions();
    }
    if (!response.body) {
      if (!session.activeResponseId) {
        throw { status: 0, message: 'No response body from server.' };
      }
      await resumeActiveResponse(session, { streamState, responseId: headerResponseId || session.activeResponseId });
    } else {
      const responseId = headerResponseId || session.activeResponseId;
      const result = await consumeResponseStream(response.body, session, streamState, {
        generation: sendGeneration,
        responseId,
        abortController: controller
      });
      if (!result.stale && result.error) {
        throw result.error;
      }
      if (!result.stale && controller._heartbeatAbort && !session.activeResponseId) {
        throw new Error('Heartbeat timed out before the response ID was received.');
      }
      if (!result.stale
        && (controller._heartbeatAbort || !result.terminal)
        && sendGeneration === state.streamGeneration
        && session.activeResponseId) {
        await resumeActiveResponse(session, { streamState, responseId });
      }
    }
    clearRuntimeSelectionIntent(session);
    if (sendGeneration === state.streamGeneration) {
      const lastAssistant = window.TermLLMConversation.sessionMessages(session).findLast(m => m.role === 'assistant');
      if (lastAssistant) finalizeVisibleAssistantStreamRender(session, lastAssistant);
      persistAndRefreshShell();
      scrollVisibleStreamToBottom(session);
    }
  } catch (err) {
    streamState.closeToolGroup();

    const controllerAborted = Boolean(controller.signal?.aborted || controller._heartbeatAbort || err?.name === 'AbortError');
    if (controllerAborted && !controller._heartbeatAbort) {
      persistAndRefreshShell();
      return;
    }

    if (sendGeneration !== state.streamGeneration) {
      return;
    }

    if (err?.status === 409 && err?.type === 'client_message_already_committed') {
      clearRuntimeSelectionIntent(session);
      for (const message of userMessages) {
        app.commitPendingInterjection?.(session, message.clientMessageId, message);
      }
      app.refreshPendingInterjectionBanner?.();
      app.refreshSessionMessagesFromTranscript?.(session);
      if (isSessionVisible(session)) app.renderMessages?.();
      clearDraftMessageForSession(session.id);
      try {
        await app.syncActiveSessionFromServer?.(session, false);
      } catch {
      }
      persistAndRefreshShell();
      return;
    }

    const inheritBatchRestore = async (retryOptions) => {
      const result = await sendMessage(retryOptions);
      if (result?.followUpBatchRestored) followUpBatchRestored = true;
      return result;
    };
    const retryPreResponsePost = async () => {
      const retryCount = heartbeatPostRetryCount;
      const retryOptions = {
        ...options,
        prompt,
        _heartbeatPostRetry: retryCount + 1,
        reuseMessageId: userMessage.id
      };
      if (Array.isArray(userMessage.attachments) && userMessage.attachments.length > 0) {
        retryOptions.attachments = userMessage.attachments.map(cloneAttachmentForMessage);
      }
      if (state.abortController === controller) {
        state.abortController = null;
      }
      detachResponseStream();
      attachResponseStream(session, '', null);
      setSessionOptimisticBusy(session, true);
      setStreaming(true);
      setConnectionState(
        typeof navigator !== 'undefined' && navigator.onLine === false
          ? 'Offline — message pending safely; reconnect to continue'
          : streamReconnectLabel(retryCount),
        'bad'
      );
      const retryGeneration = state.streamGeneration;
      const wakeReason = await waitForNetworkRetry(streamReconnectDelay(retryCount), {
        key: `${session.id}:initial:${userMessage.responseRequestId}`,
        reason: controller._heartbeatAbort ? 'heartbeat-stale' : 'pre-response-post',
        pendingSafe: true
      });
      if (wakeReason === 'detached' || state.streamGeneration !== retryGeneration || state.activeSessionId !== session.id) {
        restoreFollowUpBatch();
        persistAndRefreshShell();
        return { followUpBatchRestored };
      }
      return inheritBatchRestore(retryOptions);
    };

    if (!session.activeResponseId && (
      (controllerAborted && controller._heartbeatAbort)
      || isTransientPreResponsePostError(err)
    )) {
      return retryPreResponsePost();
    }

    const lastAssistant = window.TermLLMConversation.sessionMessages(session).findLast(m => m.role === 'assistant');
    if (lastAssistant) finalizeVisibleAssistantStreamRender(session, lastAssistant);

    if (session.activeResponseId) {
      clearRuntimeSelectionIntent(session);
      await resumeActiveResponse(session, { streamState });
      persistAndRefreshShell();
      return;
    }

    const recoverableContinuationFailure = !options._skipContinuationRefresh
      ? (err?.recoverableContinuationFailure || classifyRecoverableContinuationFailure(err, previousResponseId))
      : '';
    if (recoverableContinuationFailure && typeof app.syncActiveSessionFromServer === 'function') {
      if (state.abortController === controller) {
        state.abortController = null;
      }

      let continuationRefreshed = false;
      try {
        await app.syncActiveSessionFromServer(session, false, { skipMessagesFetch: true });
        session = getActiveSession() || session;
        continuationRefreshed = true;
      } catch {
        continuationRefreshed = false;
      }

      if (state.streaming || session.activeResponseId) {
        const retryOptions = {
          ...options,
          prompt,
          _skipContinuationRefresh: true,
          reuseMessageId: userMessage.id
        };
        if (Array.isArray(userMessage.attachments) && userMessage.attachments.length > 0) {
          retryOptions.attachments = userMessage.attachments.map(cloneAttachmentForMessage);
        }
        detachResponseStream();
        return inheritBatchRestore(retryOptions);
      }

      const continuationChanged = String(session.lastResponseId || '').trim() !== previousResponseId;
      if (continuationRefreshed && (recoverableContinuationFailure === 'session_busy' || continuationChanged)) {
        const retryOptions = {
          ...options,
          prompt,
          _skipContinuationRefresh: true,
          reuseMessageId: userMessage.id
        };
        if (Array.isArray(userMessage.attachments) && userMessage.attachments.length > 0) {
          retryOptions.attachments = userMessage.attachments.map(cloneAttachmentForMessage);
        }
        detachResponseStream();
        return inheritBatchRestore(retryOptions);
      }
    }

    if (state.abortController === controller) {
      state.abortController = null;
    }
    await app.syncActiveSessionFromServer(session, true);
    if (session.activeResponseId || state.abortController) {
      persistAndRefreshShell();
      return;
    }

    setSessionOptimisticBusy(session, false);
    app.refreshSidebarStatusPoll?.();
    restoreFollowUpBatch();
    const message = err?.message || 'Network error. Please try again.';
    addErrorMessage(message, session);
    if (err?.status === 401) {
      app.handleAuthFailure();
    }
    if (!batchingFollowUps && !String(elements.promptInput.value || '').trim()) {
      elements.promptInput.value = prompt;
      app.autoGrowPrompt();
    }

    persistAndRefreshShell();
    scrollVisibleStreamToBottom(session, true);
    return { followUpBatchRestored };
  } finally {
    if (state.abortController === controller) {
      state.abortController = null;
    }

    if (sendGeneration !== state.streamGeneration) {
      return;
    }

    const stillActive = Boolean(session.activeResponseId || state.currentStreamResponseId);
    if (!stillActive && state.askUser?.sessionId === session.id) {
      app.closeAskUserModal();
    }

    if (!stillActive) {
      setSessionOptimisticBusy(session, false);
      app.refreshSidebarStatusPoll?.();
      app.requeuePendingInterjections(session);
    }
    setStreaming(stillActive);
    refreshRelativeTimes();
    if (followUpBatchRestored) return;
    if (stillActive) {
      return;
    }

    app.drainInterruptQueueIfIdle(session);
  }
};

const releaseBranchContextQueuedSend = (sessionId) => {
  const queued = state.branchContextQueuedSend;
  if (!queued || queued.sessionId !== sessionId || state.activeSessionId !== sessionId) return false;
  state.branchContextQueuedSend = null;
  const newerDraft = String(elements.promptInput.value || '');
  const newerAttachments = [...state.attachments];
  void sendMessage({
    prompt: queued.prompt,
    attachments: queued.attachments,
    _releaseBranchContextSend: true,
    _onTransportStarted() {
      elements.promptInput.value = newerDraft;
      state.attachments = newerAttachments;
      renderAttachments();
      app.autoGrowPrompt?.();
    }
  });
  return true;
};

const restoreBranchContextQueuedSend = (sessionId) => {
  const queued = state.branchContextQueuedSend;
  if (!queued || queued.sessionId !== sessionId) return false;
  state.branchContextQueuedSend = null;
  if (state.activeSessionId !== sessionId) return false;
  const current = String(elements.promptInput.value || '').trim();
  elements.promptInput.value = current ? `${queued.prompt}\n\n${current}` : queued.prompt;
  state.attachments = [...queued.attachments, ...state.attachments];
  renderAttachments();
  app.autoGrowPrompt?.();
  return true;
};

restoreLatestDraftMessage();

Object.assign(app, {
  stageDraftMessage, removeDraftMessage, clearDraftMessageForSession,
  restoreLatestDraftMessage, restoreDraftMessageForSession, sendMessage,
  releaseBranchContextQueuedSend, restoreBranchContextQueuedSend,
});
})();
