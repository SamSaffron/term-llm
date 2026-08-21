(() => {
'use strict';

const app = window.TermLLMApp;
const ConversationController = window.TermLLMConversation?.ConversationController;
const {
  UI_PREFIX, STORAGE_KEYS, state, elements, asTimestamp, loadSessions, saveSessions, getActiveSession,
  ensureActiveSession, sessionIdFromURL, isSessionIdentityResolved, sessionSlug, findSessionBySlug, updateURL,
  updateDocumentTitle, setConnectionState, setStartupStatus, hideStartupSplash, clearProviderRetryStatus,
  persistAndRefreshShell, refreshRelativeTimes, splitHeaderModelEffort, handleAuthFailure, closeAskUserModal,
  openAskUserModal, setActiveResponseTracking, clearActiveResponseTracking, setStreaming, resumeActiveResponse,
  renderSidebar, renderMessages, renderProviderOptions, renderModelOptions, normalizeSelectedProvider,
  autoGrowPrompt, updateVoiceUI, fetchProviders, fetchModels, closeSidebar, closeSidebarIfMobile, openApprovalModal,
  closeApprovalModal, registerServiceWorker, subscribeToPush, refreshNotificationUI, shouldAutoSubscribeToPush,
  detachResponseStream, requestHeaders, discardPendingAttachments, sessionHasInProgressState,
  setSessionServerActiveRun, setSessionOptimisticBusy, moveSessionProgressState, drainInterruptQueueIfIdle,
  requeuePendingInterjections, trackPendingInterjection, removePendingInterjectionById, trackPendingInterruptCommit,
  refreshPendingInterjectionBanner, restoreDraftMessageForSession, stageDraftMessage, clearDraftMessageForSession
} = app;
let sessionStatePollTimer = null;
const SESSION_STATE_POLL_RETRY = 5000;

const resumeAndDrain = (session, options) => {
  void resumeActiveResponse(session, options).catch(async () => {
    const expectedResponseId = String(options?.responseId || session?.activeResponseId || '').trim();
    const stillOwnsFailedTransport = Boolean(expectedResponseId
      && state.currentStreamSessionId === session?.id
      && state.currentStreamResponseId === expectedResponseId);
    if (stillOwnsFailedTransport) detachResponseStream();
    try {
      const result = await syncActiveSessionFromServer(session, true, { skipMessagesFetch: true });
      if (result?.kind === 'retry' && session?.id) scheduleSessionStatePoll(session.id, 0);
    } catch (_err) {
      if (session?.id) scheduleSessionStatePoll(session.id, 0);
    }
  }).finally(() => {
    drainInterruptQueueIfIdle(session);
  });
};


const projectDraftKey = (projectId) => projectId ? `draft:${projectId}` : '';

const createAndSwitchToFreshSession = async (projectId = '') => {
  const requested = String(projectId || '').trim();
  if (state.projectsEnabled && !requested) {
    const active = getActiveSession();
    const candidates = [active?.projectId, state.activeProjectId, state.lastProjectId]
      .map((value) => String(value || '').trim()).filter((value, index, all) => value && all.indexOf(value) === index);
    const available = candidates.map((id) => state.projects.find((project) => project.id === id && project.available && !project.archived_at)).find(Boolean);
    if (!available) {
      app.setSidebarCollapsed?.(false);
      app.openSidebar?.();
      app.showToast?.('Choose a project or add one to start a chat', 'info');
      return null;
    }
    projectId = available.id;
  }
  return switchToDraftSession({ clearComposer: false, focusPrompt: true, projectId });
};

const forceNewSessionFromURL = () => {
  try {
    const params = new URLSearchParams(window.location.search || '');
    return params.has('new') || params.has('fresh');
  } catch {
    return false;
  }
};

const clearFreshSessionURL = () => {
  const target = `${UI_PREFIX}/`;
  if (typeof history !== 'undefined' && typeof history.replaceState === 'function') {
    history.replaceState(null, '', target);
    updateDocumentTitle();
    return;
  }
  updateURL('');
};

const stageCurrentComposerForSession = (sessionId) => {
  const prompt = String(elements.promptInput.value || '').trim();
  if (prompt || String(sessionId || '').startsWith('draft:')) {
    stageDraftMessage(prompt, sessionId);
    return;
  }
  clearDraftMessageForSession(sessionId);
};

const clearSessionProviderRetryOwner = (sessionId) => {
  const ownerSessionId = String(sessionId || '').trim();
  if (!ownerSessionId) return false;
  const session = state.sessions.find((item) => String(item?.id || '').trim() === ownerSessionId) || null;
  const responseId = String(session?.activeResponseId || (
    String(state.currentStreamSessionId || '').trim() === ownerSessionId
      ? state.currentStreamResponseId
      : ''
  ) || '').trim();
  if (!responseId) return false;
  return clearProviderRetryStatus(ownerSessionId, responseId);
};

const invalidateSessionStateForSelection = (sessionId = '') => {
  state.sessionStateRequestGeneration = Number(state.sessionStateRequestGeneration || 0) + 1;
  state.lastAppliedSessionStateRequestGeneration = state.sessionStateRequestGeneration;
  app.resetCurrentPlanForSession?.(sessionId);
};

const switchToDraftSession = async (options = {}) => {
  const requestedProjectId = String(options.projectId || state.activeProjectId || '').trim();
  const wasDraftSession = Boolean(state.draftSessionActive);
  const previousActiveSessionId = String(state.activeSessionId || '').trim();
  if (state.draftSessionActive && state.activeProjectId) {
    state.projectAttachments[state.activeProjectId] = Array.isArray(state.attachments) ? state.attachments.slice() : [];
  }
  const previousComposerSessionId = state.draftSessionActive ? projectDraftKey(state.activeProjectId) : previousActiveSessionId;
  const nextDraftKey = projectDraftKey(requestedProjectId);
  if (options.clearComposer && previousComposerSessionId === nextDraftKey) {
    clearDraftMessageForSession(nextDraftKey);
  } else if (options.clearPreviousComposerDraft) {
    clearDraftMessageForSession(previousComposerSessionId);
  } else {
    stageCurrentComposerForSession(previousComposerSessionId);
  }

  state.sessionSwitchGeneration = Number(state.sessionSwitchGeneration || 0) + 1;
  invalidateSessionStateForSelection('');

  stopSessionStatePoll();
  app.closeRenameSessionModal();
  closeAskUserModal();
  closeApprovalModal();
  app.closeMCPModal();
  clearSessionProviderRetryOwner(previousActiveSessionId);
  if (state.currentStreamSessionId) {
    detachResponseStream();
  } else if (previousActiveSessionId && state.currentStreamSessionId !== previousActiveSessionId) {
    setStreaming(false);
  }

  if (previousActiveSessionId) {
    const previousSession = findSessionById(previousActiveSessionId);
    previousSession?.transcript?.releaseBodies?.();
  }
  state.activeSessionId = '';
  state.draftSessionActive = true;
  state.activeProjectId = requestedProjectId;
  if (requestedProjectId) {
    state.lastProjectId = requestedProjectId;
    localStorage.setItem(app.STORAGE_KEYS.lastProject, requestedProjectId);
    const projectDraft = state.projectDrafts[requestedProjectId] || {};
    state.selectedWorktreeDir = String(projectDraft.worktreeDir || '');
    state.selectedWorktreeName = String(projectDraft.worktreeName || '');
  }
  state.pendingBranch = null;
  if (elements.branchTreeBtn) elements.branchTreeBtn.hidden = true;
  updateURL('');

  if (options.clearComposer) {
    elements.promptInput.value = '';
    discardPendingAttachments();
    if (requestedProjectId) state.projectAttachments[requestedProjectId] = [];
    autoGrowPrompt();
  } else if (previousComposerSessionId && !wasDraftSession) {
    discardPendingAttachments();
  }
  if (!options.clearComposer && requestedProjectId) {
    state.attachments = Array.isArray(state.projectAttachments[requestedProjectId])
      ? state.projectAttachments[requestedProjectId].slice()
      : [];
    app.renderAttachments?.();
  }

  refreshPendingInterjectionBanner();
  app.updateSendButtonState?.();
  persistAndRefreshShell();
  renderMessages(true);
  if (!options.clearComposer) {
    restoreDraftMessageForSession(nextDraftKey, { replace: true });
  }
  app.activateDiffSidebar?.('');
  app.invalidateMentionCompletions?.();
  void app.refreshSkillCommands?.('');

  if (options.focusPrompt) {
    elements.promptInput.focus();
  }
  if (options.closeSidebar !== false) {
    closeSidebarIfMobile();
  }
  return null;
};

const syncSelectedRuntimeFromSession = (session) => {
  if (!session) return false;
  // Selecting/synchronizing a session restores its applied runtime. Any unsent
  // runtime-control intent belonged to the previous view and must not authorize
  // a later metadata-driven model swap.
  delete session.runtimeSelectionIntent;
  const provider = String(session.provider || '').trim();
  let model = String(session.activeModel || '').trim();
  let effort = String(session.activeEffort || '').trim();
  const reasoningMode = String(session.activeReasoningMode || '').trim().toLowerCase();
  const split = splitHeaderModelEffort(model, effort, state.models);
  model = split.model;
  effort = split.effort;
  if (!provider && !model && !Object.prototype.hasOwnProperty.call(session, 'activeEffort')) {
    return false;
  }

  let changed = false;
  if (state.selectedProvider !== provider) {
    state.selectedProvider = provider;
    changed = true;
  }
  if (state.selectedModel !== model) {
    state.selectedModel = model;
    changed = true;
  }
  if (state.selectedEffort !== effort) {
    state.selectedEffort = effort;
    changed = true;
  }
  const selectedReasoningMode = reasoningMode === 'pro' ? 'pro' : 'standard';
  if (state.selectedReasoningMode !== selectedReasoningMode) {
    state.selectedReasoningMode = selectedReasoningMode;
    changed = true;
  }
  if (!changed) return false;

  const persistValue = (key, value) => {
    if (value) {
      localStorage.setItem(key, value);
    } else {
      localStorage.removeItem(key);
    }
  };
  persistValue(STORAGE_KEYS.selectedProvider, state.selectedProvider);
  persistValue(STORAGE_KEYS.selectedModel, state.selectedModel);
  persistValue(STORAGE_KEYS.selectedEffort, state.selectedEffort);
  persistValue(STORAGE_KEYS.selectedReasoningMode, state.selectedReasoningMode);

  if (elements.providerSelect) elements.providerSelect.value = state.selectedProvider || '';
  if (elements.modelSelect) elements.modelSelect.value = state.selectedModel || '';
  if (elements.effortSelect) elements.effortSelect.value = state.selectedEffort || '';
  if (elements.reasoningModeSelect) elements.reasoningModeSelect.value = state.selectedReasoningMode || 'standard';
  if (elements.chipProviderSelect) elements.chipProviderSelect.value = state.selectedProvider || '';
  if (elements.chipModelSelect) elements.chipModelSelect.value = state.selectedModel || '';
  if (elements.chipEffortSelect) elements.chipEffortSelect.value = state.selectedEffort || '';
  return true;
};

const selectedTranscriptReady = (session) => {
  if (!session || session._serverOnly) return false;
  const transcript = session.transcript;
  if (transcript) {
    return transcriptSyncSegmentIndexes(transcript).every((index) => (
      ['materialized', 'empty'].includes(transcript.segments[index]?.state)
    ));
  }
  if (Array.isArray(window.TermLLMConversation.sessionMessages(session)) && window.TermLLMConversation.sessionMessages(session).length > 0) return true;
  return Math.max(0, Number(session.messageCount) || 0) === 0;
};

const continueSessionSwitchHydration = (session, switchGeneration, options = {}) => {
  if (!session) return;
  const sessionId = String(session.id || '').trim();
  const isCurrent = () => state.sessionSwitchGeneration === switchGeneration
    && String(state.activeSessionId || '').trim() === sessionId
    && !state.draftSessionActive;

  if (options.sync !== false) {
    const statePromise = syncActiveSessionFromServer(session, true, {
      // Conversation readiness is owned by the selected sideload or its single
      // fallback. State still fetches a transcript when it reports a newer rev.
      skipMessagesFetch: true,
      expectedSwitchGeneration: switchGeneration
    });
    void Promise.resolve(statePromise).then(() => {
      if (isCurrent() && syncSelectedRuntimeFromSession(session)) app.updateHeader();
    }).catch(() => {});
  }

  if (isSessionIdentityResolved(session)) {
    void Promise.resolve().then(() => {
      if (!isCurrent()) return null;
      app.invalidateMentionCompletions?.();
      return app.refreshSkillCommands?.(sessionId);
    }).catch(() => {});
  }
};

const switchToSession = async (sessionId, options = {}) => {
  const nextId = String(sessionId || '').trim();
  if (!nextId) return null;

  const previousActiveSessionId = String(state.activeSessionId || '').trim();
  const wasProjectDraft = Boolean(state.draftSessionActive && state.activeProjectId);
  if (wasProjectDraft) state.projectAttachments[state.activeProjectId] = Array.isArray(state.attachments) ? state.attachments.slice() : [];
  const previousComposerSessionId = state.draftSessionActive ? projectDraftKey(state.activeProjectId) : previousActiveSessionId;
  stageCurrentComposerForSession(previousComposerSessionId);
  let session = state.sessions.find((item) => item.id === nextId);
  if (!session && Array.isArray(state.sidebarSearchResults)) {
    const searchResult = state.sidebarSearchResults.find((item) => item?.id === nextId) || null;
    if (searchResult) {
      session = { ...searchResult };
      state.sessions.push(session);
    }
  }
  if (!session) return null;
  if (state.pendingBranch && state.pendingBranch.sourceSessionId !== nextId) {
    app.cancelPendingBranch?.();
  }
  state.branchTree = null;
  if (elements.branchTreeBtn) elements.branchTreeBtn.hidden = true;

  const switchGeneration = (Number(state.sessionSwitchGeneration || 0) + 1);
  state.sessionSwitchGeneration = switchGeneration;
  invalidateSessionStateForSelection(nextId);
  const isCurrentSwitch = () => state.sessionSwitchGeneration === switchGeneration
    && String(state.activeSessionId || '').trim() === nextId
    && !state.draftSessionActive;

  stopSessionStatePoll();
  app.closeRenameSessionModal();
  if (state.askUser?.sessionId && state.askUser.sessionId !== nextId) {
    closeAskUserModal();
  }
  if (state.approval?.sessionId && state.approval.sessionId !== nextId) {
    closeApprovalModal();
  }
  if (previousActiveSessionId && previousActiveSessionId !== nextId) {
    app.closeMCPModal();
    clearSessionProviderRetryOwner(previousActiveSessionId);
  }
  if (state.currentStreamSessionId && state.currentStreamSessionId !== nextId) {
    detachResponseStream();
  }
  if (previousActiveSessionId && previousActiveSessionId !== nextId && state.currentStreamSessionId !== nextId) {
    setStreaming(false);
  }

  if (previousActiveSessionId !== nextId || state.draftSessionActive) {
    if (wasProjectDraft) {
      state.attachments = [];
      app.renderAttachments?.();
    } else {
      discardPendingAttachments();
    }
  }

  if (previousActiveSessionId && previousActiveSessionId !== nextId) {
    const previousSession = findSessionById(previousActiveSessionId);
    previousSession?.transcript?.releaseBodies?.();
  }
  state.activeSessionId = nextId;
  state.draftSessionActive = false;
  state.activeProjectId = String(session.projectId || '');
  if (state.activeProjectId) {
    state.lastProjectId = state.activeProjectId;
    localStorage.setItem(app.STORAGE_KEYS.lastProject, state.activeProjectId);
  }
  updateURL(sessionSlug(session));
  refreshPendingInterjectionBanner();

  const needsSelectedPayload = isSessionIdentityResolved(session) && !selectedTranscriptReady(session);
  const selectedPayloadPromise = needsSelectedPayload
    ? mergeServerSessions({
      selectedSession: session.id,
      selectedOnly: true,
      includeTranscript: true,
      expectedSwitchGeneration: switchGeneration
    })
    : null;

  persistAndRefreshShell();
  renderMessages(true);
  restoreDraftMessageForSession(session.id, { replace: true });
  app.activateDiffSidebar?.(session.id);

  let conversationReady = selectedTranscriptReady(session);
  if (selectedPayloadPromise) {
    const selectedResult = await selectedPayloadPromise;
    if (!isCurrentSwitch()) return null;
    conversationReady = selectedResult?.selectedTranscriptApplied === true || selectedTranscriptReady(session);
  }

  // Missing, malformed, or legacy selected payloads use the established
  // transcript path once. syncTranscript owns its single final render.
  if (!conversationReady && isSessionIdentityResolved(session)) {
    await loadServerSessionMessages(session.id);
    if (!isCurrentSwitch()) return null;
  }

  if (!isCurrentSwitch()) return null;
  if (syncSelectedRuntimeFromSession(session)) app.updateHeader();
  continueSessionSwitchHydration(session, switchGeneration, options);
  if (options.focusPrompt) {
    elements.promptInput.focus();
  }
  if (options.closeSidebar !== false) {
    closeSidebarIfMobile();
  }
  void app.refreshBranchTree?.({ render: false });
  return session;
};


const {
  findSessionById, ensureSessionTranscript, refreshSessionMessagesFromTranscript, touchTranscriptSkeleton,
  transcriptSyncSegmentIndexes, syncTranscript, loadServerSessionMessages, refreshActiveSessionMessagesFromServer,
  SESSION_STATE_RETRY_RESULT, loadServerSessionState
} = app;

const stopSessionStatePoll = () => {
  if (sessionStatePollTimer !== null) {
    clearTimeout(sessionStatePollTimer);
    sessionStatePollTimer = null;
  }
};

const scheduleSessionStatePoll = (sessionId, delay = 1200) => {
  stopSessionStatePoll();
  sessionStatePollTimer = setTimeout(async () => {
    const active = getActiveSession();
    if (!active || active.id !== sessionId || state.abortController) {
      stopSessionStatePoll();
      return;
    }
    let syncResult = SESSION_STATE_RETRY_RESULT;
    try {
      syncResult = await syncActiveSessionFromServer(active, true);
    } catch (_) {
      syncResult = SESSION_STATE_RETRY_RESULT;
    }
    if (syncResult?.kind === 'retry') {
      const stillActive = getActiveSession();
      if (stillActive && stillActive.id === sessionId && !state.abortController) {
        scheduleSessionStatePoll(sessionId, SESSION_STATE_POLL_RETRY);
      }
    }
  }, delay);
};

const syncActiveSessionFromServer = async (session, pollOnActive = false, { skipMessagesFetch = false, expectedSwitchGeneration = null } = {}) => {
  if (!session || !isSessionIdentityResolved(session)) return SESSION_STATE_RETRY_RESULT;

  const requestSessionId = String(session.id || '').trim();
  if (!requestSessionId) return SESSION_STATE_RETRY_RESULT;
  const requestGeneration = Number(state.sessionStateRequestGeneration || 0) + 1;
  state.sessionStateRequestGeneration = requestGeneration;
  const requestSwitchGeneration = Number(state.sessionSwitchGeneration || 0);
  const expectedGeneration = Number(expectedSwitchGeneration);
  const hasExpectedGeneration = Number.isFinite(expectedGeneration) && expectedGeneration > 0;
  const isStillActive = () => requestSessionId === String(state.activeSessionId || '').trim()
    && !state.draftSessionActive
    && state.sessionSwitchGeneration === requestSwitchGeneration
    && (!hasExpectedGeneration || state.sessionSwitchGeneration === expectedGeneration);
  const selectedResponseApplies = () => isStillActive()
    && requestGeneration >= Number(state.lastAppliedSessionStateRequestGeneration || 0);

  const busyBefore = sessionHasInProgressState(session);
  const sampledActiveProjection = {
    responseId: String(session.transcript?.activeRun?.id || '').trim(), runEpoch: Math.max(0, Number(session.transcript?.activeRun?.epoch) || 0),
    startedRev: Math.max(0, Number(session.transcript?.activeRun?.startedRev) || 0), terminal: Boolean(session.transcript?.activeRun?.terminal),
  };
  const sampledSessionResponseId = String(session.activeResponseId || '').trim();
  const sampledTransport = { askUser: state.askUser, approval: state.approval,
    controller: state.abortController,
    generation: Number(state.streamGeneration || 0),
    sessionId: String(state.currentStreamSessionId || '').trim(),
    responseId: String(state.currentStreamResponseId || '').trim(),
  };

  const loadResult = await loadServerSessionState(requestSessionId);
  if (loadResult.kind === 'auth') {
    stopSessionStatePoll();
    return loadResult;
  }
  if (loadResult.kind !== 'ok') return SESSION_STATE_RETRY_RESULT;
  const runtimeState = loadResult.state;
  const belongsToSelectedSession = requestSessionId === String(state.activeSessionId || '').trim() && !state.draftSessionActive;
  if ((hasExpectedGeneration || belongsToSelectedSession) && !selectedResponseApplies()) return loadResult;
  if (selectedResponseApplies()) {
    state.lastAppliedSessionStateRequestGeneration = requestGeneration;
    app.applyCurrentPlanState?.(requestSessionId, runtimeState);
  }

  let sessionChanged = false;
  if (app.applyMCPStateToSession(session, runtimeState)) {
    sessionChanged = true;
  }
  if (app.applyGoalStateToSession(session, runtimeState)) {
    sessionChanged = true;
    if (session.id === state.activeSessionId) app.updateGoalChip(session);
  }
  if (runtimeState.provider && runtimeState.provider !== session.provider) {
    session.provider = runtimeState.provider;
    sessionChanged = true;
  }
  const runtimeSplit = splitHeaderModelEffort(runtimeState.model, runtimeState.reasoning_effort, state.models);
  if (runtimeSplit.model && runtimeSplit.model !== session.activeModel) {
    session.activeModel = runtimeSplit.model;
    sessionChanged = true;
  }
  if (runtimeState.reasoning_effort !== undefined || runtimeSplit.effort) {
    const effort = String(runtimeSplit.effort || '');
    if (effort !== (session.activeEffort || '')) {
      session.activeEffort = effort;
      sessionChanged = true;
    }
  }
  if (runtimeState.reasoning_mode !== undefined) {
    const reasoningMode = String(runtimeState.reasoning_mode || '').trim().toLowerCase();
    if (reasoningMode !== (session.activeReasoningMode || '')) {
      session.activeReasoningMode = reasoningMode;
      sessionChanged = true;
    }
  }
  if (runtimeState.lastResponseId !== undefined) {
    const lastResponseId = String(runtimeState.lastResponseId || '').trim();
    if (lastResponseId && lastResponseId !== session.lastResponseId) {
      session.lastResponseId = lastResponseId;
      sessionChanged = true;
    }
  }
  if (sessionChanged) {
    saveSessions();
  }

  const prompts = Array.isArray(runtimeState.pending_ask_users)
    ? runtimeState.pending_ask_users
    : (runtimeState.pending_ask_user ? [runtimeState.pending_ask_user] : []);
  const prompt = prompts[0] || null;

  // Do not let state sampled before a live stream prompt opened overwrite it.
  const askUserStateApplies = isStillActive() && state.askUser === sampledTransport.askUser;
  if (askUserStateApplies && prompt && prompt.call_id && Array.isArray(prompt.questions) && prompt.questions.length > 0) {
    const samePrompt = state.askUser && state.askUser.sessionId === requestSessionId
      && state.askUser.callId === prompt.call_id;
    if (!samePrompt) {
      openAskUserModal(requestSessionId, prompt.call_id, prompt.questions);
    }
  } else if (askUserStateApplies && state.askUser?.sessionId === requestSessionId) {
    closeAskUserModal();
  }

  const pendingApproval = runtimeState.pending_approval || null;
  const approvalStateApplies = isStillActive() && state.approval === sampledTransport.approval;
  if (approvalStateApplies && pendingApproval && pendingApproval.approval_id && Array.isArray(pendingApproval.options) && pendingApproval.options.length > 0) {
    const sameApproval = state.approval && state.approval.sessionId === requestSessionId
      && state.approval.approvalId === pendingApproval.approval_id;
    if (!sameApproval) {
      openApprovalModal(requestSessionId, pendingApproval.approval_id, pendingApproval.path, pendingApproval.is_shell,
        pendingApproval.is_workspace, pendingApproval.title, pendingApproval.options, pendingApproval.resume_auto_available);
    }
  } else if (approvalStateApplies && state.approval?.sessionId === requestSessionId) {
    closeApprovalModal();
  }

  const pendingInterjection = runtimeState.pending_interjection || null;
  const pendingInterjectionText = pendingInterjection ? String(pendingInterjection.text || '').trim() : '';
  const pendingInterjectionId = pendingInterjection ? String(pendingInterjection.id || '').trim() : '';
  if (pendingInterjectionText && pendingInterjectionId) {
    const exists = state.pendingInterjections.some(entry =>
      entry.sessionId === session.id && entry.messageId === pendingInterjectionId);
    if (!exists) {
      trackPendingInterjection(session.id, pendingInterjectionText, pendingInterjectionId, 'interject');
      trackPendingInterruptCommit(session.id, pendingInterjectionText, pendingInterjectionId);
    }
  } else if (!pendingInterjectionText) {
    for (const entry of [...state.pendingInterjections]) {
      if (entry.sessionId === session.id) {
        removePendingInterjectionById(entry.messageId);
      }
    }
  }

  const transcript = ensureSessionTranscript(session);
  const runtimeTranscriptRev = Math.max(0, Number(runtimeState.transcript_rev) || 0);
  const activeResponseId = String(runtimeState.active_response_id || '').trim();
  const runEpoch = Math.max(0, Number(runtimeState.run_epoch) || 0);
  const latestObservedRunEpoch = Math.max(0, Number(transcript?.latestRunEpoch) || 0);
  if (activeResponseId && runEpoch > 0 && runEpoch < latestObservedRunEpoch) {
    window.TermLLMConversation?.transcriptDiagnostic?.('stale_status_rejection', {
      responseId: activeResponseId,
      transcriptRev: transcript?.rev,
      startRev: runtimeState.started_rev,
    });
    return loadResult;
  }
  const startedRev = Math.max(0, Number(runtimeState.started_rev) || 0);
  const targetTranscriptRev = runtimeTranscriptRev;
  if (transcript && targetTranscriptRev > transcript.rev && isStillActive()) {
    await syncTranscript(session, {
      reason: activeResponseId ? 'attach' : 'state',
      targetRev: targetTranscriptRev,
      force: Boolean(activeResponseId)
    });
  }

  const activeRun = Boolean(runtimeState.active_run);
  setSessionServerActiveRun(session, activeRun || Boolean(activeResponseId));
  const updateBusySidebar = () => {
    if (sessionHasInProgressState(session) !== busyBefore) {
      renderSidebar();
    }
    app.refreshSidebarStatusPoll();
  };

  if (activeResponseId) {
    if (transcript) {
      const accepted = await transcript.commands.enqueue(() => (
        window.TermLLMConversation.attachActiveRun(transcript, runtimeState, true)
      ));
      if (accepted !== true) {
        window.TermLLMConversation?.transcriptDiagnostic?.('stale_status_rejection', {
          responseId: activeResponseId,
          transcriptRev: transcript.rev,
          startRev: startedRev,
        });
        return loadResult;
      }
    }
    const recoverFromSnapshot = false;
    setActiveResponseTracking(session, activeResponseId);
    saveSessions();

    updateBusySidebar();
    if (isStillActive() && !state.abortController) {
      setStreaming(true);
      resumeAndDrain(session, { responseId: activeResponseId, recoverFromSnapshot });
      return loadResult;
    }
    if (pollOnActive && isStillActive()) {
      scheduleSessionStatePoll(session.id);
    }
    return loadResult;
  }

  if (activeRun && !state.abortController) {
    updateBusySidebar();
    if (isStillActive()) {
      setStreaming(true);
    }
    if (pollOnActive && isStillActive()) {
      scheduleSessionStatePoll(session.id);
    }
    return loadResult;
  }

  if (!activeRun) {
    const transportMatchesSample = () => state.abortController === sampledTransport.controller
      && Number(state.streamGeneration || 0) === sampledTransport.generation
      && String(state.currentStreamSessionId || '').trim() === sampledTransport.sessionId
      && String(state.currentStreamResponseId || '').trim() === sampledTransport.responseId;
    const idleOwnershipStable = () => {
      const currentOwnsTransport = state.currentStreamSessionId === session.id && Boolean(state.currentStreamResponseId);
      const sampledOwnedResponse = sampledTransport.sessionId === session.id && Boolean(sampledTransport.responseId);
      return String(session.activeResponseId || '').trim() === sampledSessionResponseId
        && !((state.abortController || currentOwnsTransport) && !(sampledOwnedResponse && transportMatchesSample()));
    };
    const finishIdleSessionSync = (refreshProjection = false) => {
      if (refreshProjection) {
        refreshSessionMessagesFromTranscript(session);
        if (isStillActive()) renderMessages(true);
      }
      if (isStillActive()) stopSessionStatePoll();
      const inactiveResponseId = session.activeResponseId || (
        state.currentStreamSessionId === session.id ? state.currentStreamResponseId : ''
      );
      if (state.currentStreamSessionId === session.id && state.currentStreamResponseId) {
        detachResponseStream();
      }
      if (inactiveResponseId) {
        clearActiveResponseTracking(session, inactiveResponseId);
        saveSessions();
      }
      setSessionOptimisticBusy(session, false);
      setSessionServerActiveRun(session, false);
      updateBusySidebar();
      if (isStillActive()) {
        setStreaming(false); setConnectionState('', '');
      }
    };

    let idleDecision = 'stable';
    if (transcript) {
      idleDecision = await transcript.commands.enqueue(() => {
        if (!selectedResponseApplies() || !idleOwnershipStable()) return 'stale';
        const current = transcript.activeRun;
        if (!sampledActiveProjection.responseId || sampledActiveProjection.terminal) {
          if (String(current?.id || '').trim() !== sampledActiveProjection.responseId
              || Math.max(0, Number(current?.epoch) || 0) !== sampledActiveProjection.runEpoch
              || Boolean(current?.terminal) !== sampledActiveProjection.terminal) return 'stale';
          return 'stable';
        }
        const retired = transcript.retireOrphanedActiveProjection({
          responseId: sampledActiveProjection.responseId,
          runEpoch: sampledActiveProjection.runEpoch,
          startedRev: sampledActiveProjection.startedRev,
          transcriptRev: runtimeTranscriptRev,
        });
        if (!retired) {
          if (String(current?.id || '').trim() !== sampledActiveProjection.responseId
              || Math.max(0, Number(current?.epoch) || 0) !== sampledActiveProjection.runEpoch
              || Boolean(current?.terminal) !== sampledActiveProjection.terminal) return 'stale';
          return 'refused';
        }
        // The authority decision, projection mutation, render publication,
        // transport detach, and tracking cleanup are one non-awaiting queued
        // transaction. Late stream work observes the retired epoch barrier.
        finishIdleSessionSync(true);
        return 'retired';
      });
    } else if (!idleOwnershipStable()) {
      idleDecision = 'stale';
    }
    if (idleDecision === 'stale') return loadResult;
    if (idleDecision === 'retired') {
      app.retireUnownedInterjectionIntents?.(session);
      requeuePendingInterjections(session);
      drainInterruptQueueIfIdle(session);
      return loadResult;
    }

    finishIdleSessionSync(false);
    if (isStillActive() && !skipMessagesFetch) {
      await refreshActiveSessionMessagesFromServer(session, {
        targetRev: runtimeTranscriptRev,
        forceScroll: true
      });
    }
    app.retireUnownedInterjectionIntents?.(session);
    requeuePendingInterjections(session);
    drainInterruptQueueIfIdle(session);
  }

  return loadResult;
};

const refreshCurrentPlanFromServer = async (session = getActiveSession()) => {
  if (!session || session.id !== state.activeSessionId || state.draftSessionActive) return null;
  return syncActiveSessionFromServer(session, false, { skipMessagesFetch: true });
};

const applyServerSessionSummary = (target, serverSession) => {
  if (!target || !serverSession) return target;
  target.name = String(serverSession.name || '');
  target.generatedShortTitle = String(serverSession.generated_short_title || target.generatedShortTitle || '');
  target.generatedLongTitle = String(serverSession.generated_long_title || target.generatedLongTitle || '');
  target.title = serverSession.short_title || target.title || 'New chat';
  target.longTitle = serverSession.long_title || '';
  target.mode = String(serverSession.mode || target.mode || 'chat');
  target.origin = String(serverSession.origin || target.origin || 'tui');
  target.archived = Boolean(serverSession.archived);
  target.pinned = Boolean(serverSession.pinned);
  target.created = asTimestamp(serverSession.created_at || target.created);
  const serverLastMessageAt = Number(serverSession.last_message_at);
  if (Number.isFinite(serverLastMessageAt) && serverLastMessageAt > 0) {
    target.lastMessageAt = serverLastMessageAt;
  } else if (!target.lastMessageAt) {
    target.lastMessageAt = target.created;
  }
  target.messageCount = Number(serverSession.message_count || target.messageCount || 0);
  const transcriptRev = Number(serverSession.transcript_rev);
  if (Number.isFinite(transcriptRev) && transcriptRev >= 0) target.transcriptRev = transcriptRev;
  target.number = Number(serverSession.number || target.number || 0);
  if (serverSession.provider) {
    target.provider = serverSession.provider;
  }
  target.projectId = String(serverSession.project_id || target.projectId || '');
  target.projectName = String(serverSession.project_name || target.projectName || '');
  if (serverSession.worktree_dir !== undefined) {
    target.worktreeDir = String(serverSession.worktree_dir || '');
    target.worktreeName = target.worktreeDir ? target.worktreeDir.split(/[\\/]/).filter(Boolean).pop() || 'worktree' : '';
  }
  if (Object.prototype.hasOwnProperty.call(serverSession, 'goal')) {
    target.goal = serverSession.goal && typeof serverSession.goal === 'object' ? { ...serverSession.goal } : null;
  }
  if (Object.prototype.hasOwnProperty.call(serverSession, 'file_change_summary')) {
    target.fileChangeSummary = serverSession.file_change_summary && typeof serverSession.file_change_summary === 'object'
      ? { ...serverSession.file_change_summary }
      : null;
  }
  if (Object.prototype.hasOwnProperty.call(serverSession, 'plan_summary')) {
    target.planSummary = serverSession.plan_summary && typeof serverSession.plan_summary === 'object'
      ? { ...serverSession.plan_summary }
      : null;
  }
  return target;
};

const applySelectedTranscriptSideload = (session, sideload, options = {}) => {
  if (!session || !sideload || typeof sideload !== 'object' || typeof ConversationController !== 'function') return false;
  const index = sideload.index;
  const bodies = sideload.bodies;
  const etag = String(sideload.index_etag || '').trim();
  const rev = Number(index?.rev);
  if (!index?.rows || !Number.isFinite(rev) || rev < 0 || !etag
      || !bodies || Number(bodies.rev) !== rev || !Array.isArray(bodies.messages)) return false;

  const current = ensureSessionTranscript(session);
  if (!current || current.rev > rev) return false;

  // Validate the complete transaction in a detached store before mutating the
  // selected canonical session. This catches malformed parallel arrays,
  // out-of-window bodies, and incomplete turns without exposing a partial
  // projection or disturbing optimistic entries.
  const staging = new ConversationController(`startup-sideload:${session.id}`, current.budgets);
  try {
    window.TermLLMConversation.applyTranscriptIndex(staging, index, etag, true);
    for (const intent of current.conversation?.intents?.values?.() || []) {
      window.TermLLMConversation.addPendingIntentToConversation(staging, { ...intent }, intent.revAtSend);
    }
    window.TermLLMConversation.attachActiveRun(staging, index, true);

    const wantedIndexes = transcriptSyncSegmentIndexes(staging);
    const allowedBodyIDs = new Set();
    for (const segmentIndex of wantedIndexes) {
      const segment = staging.segments[segmentIndex];
      if (!segment) continue;
      for (let ordinal = segment.startOrdinal; ordinal <= segment.endOrdinal; ordinal += 1) {
        allowedBodyIDs.add(staging.ids[ordinal]);
      }
    }
    const normalizedBodyID = (entry) => {
      const value = entry?.id ?? entry?.ID;
      return Number.isFinite(Number(value)) ? Number(value) : value;
    };
    if (bodies.messages.some((entry) => !allowedBodyIDs.has(normalizedBodyID(entry)))) return false;
    if (!window.TermLLMConversation.materializeTranscriptBodies(staging, bodies.messages, wantedIndexes)) return false;
  } catch (_) {
    return false;
  } finally {
    window.TermLLMConversation.destroyConversationController(staging);
  }

  try {
    window.TermLLMConversation.applyTranscriptIndex(current, index, etag, true);
    window.TermLLMConversation.materializeTranscriptBodies(current, bodies.messages);
    app.persistPendingIntents(session);
    refreshSessionMessagesFromTranscript(session);
    touchTranscriptSkeleton(session);
    if (options.startup === true) session._startupTranscriptSideloaded = true;
    if (session.id === state.activeSessionId && !state.draftSessionActive) {
      renderMessages(true);
      if (options.startup === true) session._startupTranscriptRendered = true;
    }
    return true;
  } catch (_) {
    // The detached validation above makes this path defensive only. Leave the
    // marker unset so activation falls back to the normal transcript endpoints.
    return false;
  }
};

const reconcileServerSessionIdentity = (session, serverSession) => {
  if (!session || !serverSession) return session;

  const nextId = String(serverSession.id || '').trim();
  const previousId = String(session.id || '').trim();
  if (!nextId || nextId === previousId) return session;

  session.transcript?.rekey?.(nextId);
  app.rekeyPendingIntentStorage(previousId, nextId);
  session.id = nextId;
  if (state.activeSessionId === previousId) state.activeSessionId = nextId;
  if (state.renameSessionId === previousId) state.renameSessionId = nextId;
  if (state.currentStreamSessionId === previousId) state.currentStreamSessionId = nextId;
  if (state.currentPlanSessionId === previousId) state.currentPlanSessionId = nextId;
  if (state.askUser?.sessionId === previousId) state.askUser.sessionId = nextId;
  if (state.approval?.sessionId === previousId) state.approval.sessionId = nextId;
  for (const entry of state.queuedInterrupts) {
    if (entry.sessionId === previousId) entry.sessionId = nextId;
  }
  for (const entry of state.pendingInterruptCommits) {
    if (entry.sessionId === previousId) entry.sessionId = nextId;
  }
  for (const entry of state.pendingInterjections) {
    if (entry.sessionId === previousId) entry.sessionId = nextId;
  }
  moveSessionProgressState(previousId, nextId);
  return session;
};

const mergeServerSessions = async (options = {}) => {
  const result = {
    selectedSession: null,
    selectedTranscriptApplied: false,
    selectedResponseCurrent: false
  };
  try {
    const categories = Array.isArray(options.categories) ? options.categories : state.sidebarSessionCategories;
    const includeArchived = typeof options.includeArchived === 'boolean'
      ? options.includeArchived
      : state.showHiddenSessions;
    const params = new URLSearchParams();
    if (Array.isArray(categories) && categories.length > 0 && !categories.includes('all')) {
      params.set('categories', categories.join(','));
    }
    if (includeArchived) {
      params.set('include_archived', '1');
    }
    const selectedSession = String(options.selectedSession || '').trim();
    if (selectedSession) {
      params.set('selected_session', selectedSession);
    }
    if (options.selectedOnly === true) {
      params.set('selected_only', '1');
    }
    if (options.includeTranscript === true) {
      params.set('include_transcript', '1');
    }
    if (options.includeWidgetStatus === true) {
      params.set('include_widget_status', '1');
    }
    const query = params.toString();
    const resp = await app.apiFetch(`${UI_PREFIX}/v1/sessions${query ? `?${query}` : ''}`, {
      headers: requestHeaders('')
    });
    if (!resp.ok) return result;
    const data = await resp.json();
    if (!Array.isArray(data.sessions)) return result;
    if (options.includeWidgetStatus === true) {
      app.applyWidgetStatus(data.widget_status);
    }

    const localById = new Map(state.sessions.map(s => [s.id, s]));
    const localByNumber = new Map(
      state.sessions
        .filter(s => Number(s.number) > 0 && /^\d+$/.test(s.id))
        .map(s => [Number(s.number), s])
    );

    const mergeServerSession = (serverSession) => {
      if (!serverSession || typeof serverSession !== 'object') return null;
      const sNum = Number(serverSession.number || 0);
      let local = localById.get(serverSession.id) ||
        (sNum > 0 ? localByNumber.get(sNum) : null) ||
        null;
      if (local) {
        reconcileServerSessionIdentity(local, serverSession);
        applyServerSessionSummary(local, serverSession);
        localById.set(local.id, local);
        if (Number(local.number) > 0) localByNumber.set(Number(local.number), local);
        return local;
      }

      local = applyServerSessionSummary({
        id: serverSession.id,
        number: 0,
        name: '',
        title: 'New chat',
        longTitle: '',
        mode: 'chat',
        origin: 'tui',
        archived: false,
        pinned: false,
        created: Date.now(),
        lastMessageAt: Date.now(),
            lastResponseId: null,
        activeResponseId: null,
        messageCount: 0,
        _serverOnly: true
      }, serverSession);
      state.sessions.push(local);
      localById.set(local.id, local);
      if (Number(local.number) > 0) localByNumber.set(Number(local.number), local);
      return local;
    };

    for (const serverSession of data.sessions) mergeServerSession(serverSession);

    const selected = mergeServerSession(data.selected_session);
    result.selectedSession = selected;
    const expectedSwitchGeneration = Number(options.expectedSwitchGeneration);
    const hasExpectedSwitchGeneration = Number.isFinite(expectedSwitchGeneration) && expectedSwitchGeneration > 0;
    result.selectedResponseCurrent = Boolean(selected)
      && selected.id === state.activeSessionId
      && !state.draftSessionActive
      && (!hasExpectedSwitchGeneration || state.sessionSwitchGeneration === expectedSwitchGeneration);
    if (result.selectedResponseCurrent && options.includeTranscript === true) {
      const transcript = ensureSessionTranscript(selected);
      result.selectedTranscriptApplied = await transcript.commands.enqueue(() => applySelectedTranscriptSideload(selected, data.selected_transcript, {
        startup: options.startupTranscript === true
      }));
    }
    if (result.selectedResponseCurrent && selected.id === state.activeSessionId && !state.draftSessionActive) {
      if (selected.fileChangeSummary) {
        app.applySessionDiffSummary?.(selected.id, selected.fileChangeSummary);
      }
      if (Object.prototype.hasOwnProperty.call(selected, 'planSummary')) {
        app.applyCurrentPlanSummary?.(selected.id, selected.planSummary);
      }
    }

    persistAndRefreshShell();
    return result;
  } catch {
    // Gracefully fall back to in-memory-only
    return result;
  }
};


// ===== Initialization =====
const configuredStartupHydrationTimeout = Number(window.TERM_LLM_STARTUP_HYDRATION_TIMEOUT_MS);
const STARTUP_HYDRATION_TIMEOUT_MS = Number.isFinite(configuredStartupHydrationTimeout)
  && configuredStartupHydrationTimeout >= 0
  ? configuredStartupHydrationTimeout
  : 10000;
let startupSplashReleased = false;

const releaseStartupSplash = () => {
  if (startupSplashReleased) return;
  startupSplashReleased = true;
  hideStartupSplash();
};

const refreshSkillCommandsAfterStartup = (sessionId) => {
  Promise.resolve()
    .then(() => app.refreshSkillCommands?.(sessionId))
    .catch(() => {});
};

const hydrateActiveSessionAfterStartup = async () => {
  const active = getActiveSession();
  if (!active || !isSessionIdentityResolved(active)) return false;

  const hasTranscriptSideload = active._startupTranscriptSideloaded === true;
  const hasRenderedTranscriptSideload = active._startupTranscriptRendered === true;
  // Start state sync immediately so the server round-trip overlaps with a
  // fallback transcript fetch. Runtime metadata and active-response recovery
  // continue independently after the selected transcript is ready to reveal.
  const statePromise = syncActiveSessionFromServer(active, true, {
    skipMessagesFetch: Boolean(active._serverOnly) || hasTranscriptSideload
  });
  void (async () => {
    try {
      await statePromise;
      if (syncSelectedRuntimeFromSession(active)) {
        app.updateHeader();
      }
    } catch (_) {
      // Runtime state is best-effort during startup and must not create an
      // unhandled rejection after transcript readiness releases the splash.
    } finally {
      delete active._startupTranscriptSideloaded;
      delete active._startupTranscriptRendered;
      refreshSkillCommandsAfterStartup(active.id);
    }
  })();

  if (hasRenderedTranscriptSideload) return true;

  const preloadMessagesPromise = active._serverOnly && !hasTranscriptSideload
    ? loadServerSessionMessages(active.id)
    : null;
  if (!preloadMessagesPromise) return false;

  const msgs = await preloadMessagesPromise;
  if (!Array.isArray(msgs)) return false;
  saveSessions();
  renderSidebar();
  // This force-scroll render is the fallback transcript's final startup
  // projection and bottom-position boundary.
  renderMessages(true);
  return true;
};

const waitForStartupHydration = async () => {
  const active = getActiveSession();
  if (!active || !isSessionIdentityResolved(active)) return false;

  setStartupStatus('Loading conversation…');
  const hydration = hydrateActiveSessionAfterStartup().catch(() => false);
  let timeoutID = null;
  const timeout = new Promise((resolve) => {
    timeoutID = window.setTimeout(() => resolve(false), STARTUP_HYDRATION_TIMEOUT_MS);
  });
  const rendered = await Promise.race([hydration, timeout]);
  if (timeoutID !== null) window.clearTimeout(timeoutID);
  return rendered === true;
};

const initialize = async () => {
  setStartupStatus('Loading your chat shell…');
  state.sessions = loadSessions();

  // Check URL for a specific session (number or ID)
  const forceNewSession = forceNewSessionFromURL();
  const urlSlug = forceNewSession ? '' : sessionIdFromURL();
  if (forceNewSession) {
    state.activeSessionId = '';
    state.draftSessionActive = true;
    clearFreshSessionURL();
  } else if (urlSlug) {
    const found = findSessionBySlug(urlSlug);
    if (found) {
      state.activeSessionId = found.id;
      state.draftSessionActive = false;
    } else {
      // Create a server-only stub that will be lazy-loaded
      const num = /^\d+$/.test(urlSlug) ? Number(urlSlug) : 0;
      const stub = {
        id: urlSlug,
        number: num,
        name: '',
        title: 'Loading…',
        longTitle: '',
        mode: 'chat',
        origin: 'tui',
        archived: false,
        pinned: false,
        created: Date.now(),
            lastResponseId: null,
        activeResponseId: null,
        _serverOnly: true
      };
      state.sessions.unshift(stub);
      state.activeSessionId = stub.id;
      state.draftSessionActive = false;
    }
  } else if (!state.activeSessionId && state.sessions.length === 0) {
    state.draftSessionActive = true;
  }

  ensureActiveSession();

  renderSidebar();
  renderMessages(true);
  renderProviderOptions();
  renderModelOptions();
  autoGrowPrompt();
  updateVoiceUI();
  refreshNotificationUI();
  void registerServiceWorker().then(() => refreshNotificationUI());

  try {
    setStartupStatus(state.token ? 'Checking your token…' : 'Connecting…');
    setConnectionState(state.token ? 'Validating token…' : 'Connecting…');

    await app.initializeProjectMode?.();
    const sessionsPromise = state.projectsEnabled && !state.projectsError
      ? (urlSlug
        ? mergeServerSessions({ selectedSession: urlSlug, selectedOnly: true, includeTranscript: true, startupTranscript: true, includeWidgetStatus: true })
        : Promise.resolve(state.sessions))
      : mergeServerSessions({
        selectedSession: urlSlug,
        includeTranscript: Boolean(urlSlug),
        startupTranscript: true,
        includeWidgetStatus: true
      });
    // Session selection owns conversation readiness. Begin transcript fallback
    // and runtime state as soon as the authoritative merge lands, without
    // waiting for providers or models. Only an actual selected transcript
    // render releases the splash here; draft/unresolved/error paths retain the
    // existing final or bounded fallback.
    const startupHydrationPromise = sessionsPromise.then(() => waitForStartupHydration());
    void startupHydrationPromise.then((rendered) => {
      if (rendered) releaseStartupSplash();
    }).catch(() => {});

    // Start a speculative models fetch immediately using the provider stored in
    // localStorage. For returning users this runs in parallel with fetchProviders,
    // saving one serial round trip. If normalizeSelectedProvider changes the
    // selection we discard the speculative result and re-fetch.
    const speculativeProvider = state.selectedProvider;
    const speculativeModelsPromise = speculativeProvider
      ? fetchModels('', speculativeProvider)
      : null;

    // Fetch providers to validate and normalize the stored selection.
    state.providers = await fetchProviders();
    normalizeSelectedProvider();
    renderProviderOptions();
    app.updateHeader?.();

    let modelsPromise;
    if (speculativeModelsPromise !== null && state.selectedProvider === speculativeProvider) {
      modelsPromise = speculativeModelsPromise;
    } else {
      if (speculativeModelsPromise !== null) speculativeModelsPromise.catch(() => {});
      modelsPromise = fetchModels('', state.selectedProvider);
    }
    setStartupStatus('Syncing sessions…');

    [state.models] = await Promise.all([modelsPromise, sessionsPromise]);
    app.setApplicationConnected?.(true, true);
    if (!app.setApplicationConnected) state.connected = true;
    renderModelOptions();
    app.updateHeader?.();
    setConnectionState('', '');
    // The primary sessions response is authoritative for startup. Arm the
    // ordinary cadence instead of immediately reconciling the same status and
    // triggering duplicate selected-session state work during first paint.
    app.refreshSidebarStatusPoll();
    void app.refreshBranchTree?.({ render: false });
    if (!state.draftSessionActive && !getActiveSession()) {
      ensureActiveSession();
      renderMessages(true);
    }
    // Boot may have changed the active session (URL slug, server sync);
    // activate the diff sidebar for wherever we actually landed.
    app.activateDiffSidebar?.(state.draftSessionActive ? '' : state.activeSessionId);

    // Retry push enrollment now that auth is confirmed. Also recover automatically
    // when the browser permission is already granted but the old localStorage flag
    // was never set (for example after earlier installs or app updates).
    if (shouldAutoSubscribeToPush()) {
      subscribeToPush();
    }

    await startupHydrationPromise;
  } catch (err) {
    const message = err?.message || 'Unable to validate token.';
    setStartupStatus(message);
    setConnectionState(message, 'bad');
    if (!state.token || err?.status === 401) {
      handleAuthFailure();
    }
  } finally {
    releaseStartupSplash();
  }
};



setInterval(refreshRelativeTimes, 60_000);

Object.assign(app, {
  createAndSwitchToFreshSession,
  stopSessionStatePoll,
  scheduleSessionStatePoll,
  syncActiveSessionFromServer,
  refreshCurrentPlanFromServer,
  invalidateSessionStateForSelection,
  syncSelectedRuntimeFromSession,
  applyServerSessionSummary,
  reconcileServerSessionIdentity,
  applySelectedTranscriptSideload,
  mergeServerSessions,
  switchToDraftSession,
  switchToSession,
  initialize
});

initialize();
})();
