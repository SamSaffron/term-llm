'use strict';

(function initAppSend() {

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
    // localStorage can be full or disabled; draft preservation is best-effort.
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

const { requestHeaders, normalizeError, hasSessionContinuationContext, effectiveEffortForCompare, clearRuntimeSelectionIntent, classifyRecoverableContinuationFailure, sleep, streamReconnectDelay, streamReconnectLabel, isTransientPreResponsePostError, setActiveResponseTracking, HEARTBEAT_STALE_THRESHOLD, heartbeatUploadGraceThreshold, attachResponseStream, detachResponseStream, isSessionVisible, appendStreamMessageNode, updateVisibleUserNode, finalizeVisibleAssistantStreamRender, scrollVisibleStreamToBottom, createResponseStreamState, consumeResponseStream, resumeActiveResponse, setStreaming, recoverInterruptFailure, addErrorMessage } = app;

const sendMessage = async (options = {}) => {
  const promptSource = typeof options.prompt === 'string' ? options.prompt : elements.promptInput.value;
  const prompt = String(promptSource || '').trim();
  const pendingAttachments = Array.isArray(options.attachments)
    ? [...options.attachments]
    : [...state.attachments];

  if (!prompt && pendingAttachments.length === 0) return;

  if (!state.connected) {
    app.openAuthModal('Connect before sending a message.', true);
    return;
  }

  if (/^\/(goal|mcp|model|new)$/i.test(prompt)) {
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
    }
    return;
  }

  if (/^\/(compact|compress)$/i.test(prompt)) {
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
      const response = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(session.id)}/runtime/compact`, {
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

  if (/^\/side(?:\s|$)/i.test(prompt)) {
    const question = prompt.replace(/^\/side\b/i, '').trim();
    elements.promptInput.value = '';
    app.hideSlashCommands?.();
    if (typeof app.openSideQuestion === 'function') await app.openSideQuestion(question);
    return;
  }

  let session = getActiveSession();
  const skillInvocation = pendingAttachments.length === 0
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
  if (skillInvocation && session && !state.draftSessionActive) {
    if (activeSessionBusy && skillInvocation.execution !== 'isolated') {
      app.queueMainSkillInvocation(session, skillInvocation);
      return;
    }
    await app.invokeSkill(session, skillInvocation);
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
    app.trackPendingInterjection(session.id, prompt || pendingAttachments[0]?.name || 'Attachment', pendingMessageId, 'deciding', pendingAttachments);
    app.addInlineInterruptMessage(session, prompt, pendingMessageId, 'evaluating', pendingAttachments);
    persistAndRefreshShell();
    scrollVisibleStreamToBottom(session, true);

    elements.promptInput.value = '';
    state.attachments = [];
    renderAttachments();
    app.autoGrowPrompt();

    try {
      await app.interruptActiveRun(session, prompt, pendingMessageId, requestAttachmentParts, pendingAttachments);
      clearDraftMessageForSession(session.id);
    } catch (err) {
      // Interrupt can fail after backend restart or stale runtime state. For any
      // non-auth HTTP failure, resync server truth before deciding whether to
      // queue locally, retry as a fresh message, or surface the original error.
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
  if (pendingAttachments.length > 0) {
    try {
      requestAttachmentParts = await buildAttachmentInputParts(pendingAttachments, controller.signal);
    } catch (err) {
      try {
        controller.abort();
      } catch {
        // Ignore abort failures while tearing down attachment reads.
      }
      const message = err?.message || 'Failed to read attachment.';
      alert(message);
      return;
    }
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
      // Best effort only: if the continuation cursor is still unavailable we
      // fall back to the local session state below.
    }
    if (state.streaming || session.activeResponseId) {
      return sendMessage({ ...options, _skipContinuationRefresh: true });
    }
  }

  const reuseMessageId = typeof options.reuseMessageId === 'string' ? options.reuseMessageId : '';
  stageDraftMessage(prompt, session.id);
  let userMessage = reuseMessageId
    ? window.TermLLMConversation.sessionMessages(session).find(m => m.id === reuseMessageId && m.role === 'user') || null
    : null;
  const isNewUserMessage = !userMessage;

  if (!userMessage) {
    userMessage = {
      id: generateId('msg'),
      role: 'user',
      content: prompt,
      created: Date.now()
    };
  } else {
    userMessage.content = prompt;
    delete userMessage.interruptState;
  }
  userMessage.clientMessageId = String(userMessage.clientMessageId || userMessage.id || '').trim();
  userMessage.responseRequestId = String(userMessage.responseRequestId || generateId('request')).trim();
  session.lastMessageAt = Date.now();

  if (pendingAttachments.length > 0) {
    userMessage.attachments = pendingAttachments.map(cloneAttachmentForMessage);
  } else {
    delete userMessage.attachments;
  }
  if (isNewUserMessage) {
    userMessage = app.trackPendingIntent?.(session, userMessage) || userMessage;
  }

  if (!session.title || session.title === 'New chat') {
    session.title = truncate(prompt || pendingAttachments[0]?.name || 'Image', 60);
  }

  if (isSessionVisible(session)) {
    const hadEmptyState = elements.messages.querySelector('.empty-state');
    if (hadEmptyState) hadEmptyState.remove();
  }

  if (isNewUserMessage) {
    appendStreamMessageNode(session, userMessage);
  } else {
    updateVisibleUserNode(session, userMessage);
  }
  if (isSessionVisible(session)) syncTurnActionPanels();

  setSessionOptimisticBusy(session, true);
  persistAndRefreshShell();

  elements.promptInput.value = '';
  if (!Array.isArray(options.attachments)) {
    state.attachments = [];
    renderAttachments();
  }
  app.autoGrowPrompt();
  scrollVisibleStreamToBottom(session, true);

  state.expectCanceledRun = false;
  const sendGeneration = state.streamGeneration;
  attachResponseStream(session, '', controller);
  setStreaming(true);
  app.refreshSidebarStatusPoll?.();
  const streamState = createResponseStreamState(session);
  let previousResponseId = '';

  try {
    // Build input content: plain string or array with file/image parts
    let inputContent;
    if (requestAttachmentParts.length > 0) {
      const contentParts = requestAttachmentParts.slice();
      if (prompt) {
        contentParts.push({ type: 'input_text', text: prompt });
      }
      inputContent = contentParts;
    } else {
      inputContent = prompt;
    }

    const body = {
      stream: true,
      include_server_tools: true,
      client_message_id: userMessage.clientMessageId,
      input: [{ type: 'message', role: 'user', content: inputContent }]
    };

    previousResponseId = String(session.lastResponseId || '').trim();
    if (!previousResponseId && session.worktreeDir) {
      body.worktree_dir = session.worktreeDir;
    }
    if (previousResponseId) {
      body.previous_response_id = previousResponseId;
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
    let response = await fetch(`${UI_PREFIX}/v1/responses`, {
      method: 'POST',
      headers,
      body: requestBody,
      signal: controller.signal
    });
    controller._heartbeatStaleThreshold = HEARTBEAT_STALE_THRESHOLD;
    const headerResponseId = String(response.headers.get('x-response-id') || '').trim();
    const headerSessionNumber = Number(response.headers.get('x-session-number') || 0);
    if (headerSessionNumber > 0 && session.number !== headerSessionNumber) {
      session.number = headerSessionNumber;
      updateURL(sessionSlug(session));
    }
    if (!response.ok) {
      throw await normalizeError(response);
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
        // A body can be attached without an x-response-id. Reader cancellation
        // then completes normally, so route it through the pre-response retry
        // path instead of treating the send as terminal.
        throw new Error('Heartbeat timed out before the response ID was received.');
      }
      if (!result.stale
        && (controller._heartbeatAbort || !result.terminal)
        && sendGeneration === state.streamGeneration
        && session.activeResponseId) {
        await resumeActiveResponse(session, { streamState, responseId });
      }
    }

    // Keep explicit runtime intent across any pre-response POST rebuild. Once
    // this request has completed or attached to a durable response, subsequent
    // sends should use the server-confirmed session runtime again.
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

    // If the stream was detached (New Chat, switched session), don't
    // touch DOM or streaming state for this session.
    if (sendGeneration !== state.streamGeneration) {
      return;
    }

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
      setConnectionState(streamReconnectLabel(retryCount));
      const retryGeneration = state.streamGeneration;
      await sleep(streamReconnectDelay(retryCount));
      if (state.streamGeneration !== retryGeneration || state.activeSessionId !== session.id) {
        persistAndRefreshShell();
        return;
      }
      return sendMessage(retryOptions);
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
        return sendMessage(retryOptions);
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
        return sendMessage(retryOptions);
      }
    }

    // Clear our own controller so syncActiveSessionFromServer can act on
    // server state freely (its !state.abortController guard would block
    // cleanup otherwise).  If sync triggers a new resume, it will set a
    // fresh controller — the check below detects that case.
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
    const message = err?.message || 'Network error. Please try again.';
    addErrorMessage(message, session);
    if (err?.status === 401) {
      app.handleAuthFailure();
    }
    if (!String(elements.promptInput.value || '').trim()) {
      elements.promptInput.value = prompt;
      app.autoGrowPrompt();
    }

    persistAndRefreshShell();
    scrollVisibleStreamToBottom(session, true);
  } finally {
    if (state.abortController === controller) {
      state.abortController = null;
    }

    // If the stream was detached (New Chat, switched session), don't
    // touch streaming state — the navigation already set it correctly.
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
    if (stillActive) {
      return;
    }

    app.drainInterruptQueueIfIdle(session);
  }
};


restoreLatestDraftMessage();

Object.assign(app, {
  stageDraftMessage, removeDraftMessage, clearDraftMessageForSession,
  restoreLatestDraftMessage, restoreDraftMessageForSession, sendMessage,
});
})();
