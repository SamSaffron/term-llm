(() => {
'use strict';

const app = window.TermLLMApp;
const {
  UI_PREFIX, STORAGE_KEYS, state, elements, generateId, truncate, asTimestamp, loadSessions, saveSessions, getActiveSession, createSession, ensureActiveSession,
  sessionIdFromURL, sessionSlug, findSessionBySlug, updateURL, updateDocumentTitle, scrollToBottom, setConnectionState, clearConnectionStateOwner, setStartupStatus, hideStartupSplash, clearProviderRetryStatus, persistAndRefreshShell, refreshRelativeTimes,
  splitHeaderModelEffort, updateMCPStatusDisplay, setElementHidden,
  openAuthModal, closeAuthModal, handleAuthFailure, closeAskUserModal, openAskUserModal, setActiveResponseTracking,
  clearActiveResponseTracking, setStreaming, resumeActiveResponse, renderSidebar, renderMessages, renderProviderOptions, renderModelOptions, normalizeSelectedProvider,
  autoGrowPrompt, updateVoiceUI, toggleVoiceRecording, fetchProviders, fetchModels, addErrorMessage, sendMessage, openSidebar, closeSidebar, closeSidebarIfMobile,
  connectToken, submitAskUserModal, cancelActiveResponse, handleFiles, noteUserScrollIntent, noteScrollPositionChanged, shouldDisableAutoScrollForKey,
  openApprovalModal, closeApprovalModal, submitApprovalModal, registerServiceWorker, subscribeToPush, refreshNotificationUI,
  requestNotificationPermission, shouldAutoSubscribeToPush, detachResponseStream, HEARTBEAT_STALE_THRESHOLD,
  applyDesktopSidebarState, toggleSidebarCollapsed, flushStreamPersistence, requestHeaders, normalizeError, discardPendingAttachments,
  updateSidebarStatus, sessionHasInProgressState, hasAnySessionInProgressState, setSessionServerActiveRun, setSessionOptimisticBusy,
  moveSessionProgressState, requeueUncommittedInterrupts, drainInterruptQueueIfIdle, requeuePendingInterjections,
  trackPendingInterjection, removePendingInterjectionById, trackPendingInterruptCommit, refreshPendingInterjectionBanner,
  restoreDraftMessageForSession, stageDraftMessage, clearDraftMessageForSession
} = app;
let sessionStatePollTimer = null;

const rebaseSessionAssetURL = (url) => (
  typeof app.rebaseHubAssetURL === 'function'
    ? app.rebaseHubAssetURL(url)
    : String(url || '').trim()
);

const resumeAndDrain = (session, options) => {
  void resumeActiveResponse(session, options).finally(() => {
    drainInterruptQueueIfIdle(session);
  });
};

const shouldRecoverActiveResponseFromSnapshot = (session, responseId, responseChanged = false) => {
  if (!session || !String(responseId || '').trim()) return false;
  if (responseChanged) return true;

  const currentSeq = Number(session.lastSequenceNumber || 0);
  // Once we have replayed at least one event for this response, resume from
  // that sequence number rather than re-fetching the full snapshot. This relies
  // on the invariant that local message state up to lastSequenceNumber already
  // reflects the replayed event stream.
  if (currentSeq > 0) return false;

  let lastUserIndex = -1;
  for (let i = session.messages.length - 1; i >= 0; i -= 1) {
    if (session.messages[i]?.role === 'user') {
      lastUserIndex = i;
      break;
    }
  }
  if (lastUserIndex < 0) return false;

  for (let i = lastUserIndex + 1; i < session.messages.length; i += 1) {
    const role = session.messages[i]?.role;
    if (role === 'assistant' || role === 'tool' || role === 'tool-group' || role === 'error') {
      return true;
    }
  }

  return false;
};

// ===== Sidebar status polling =====
const SIDEBAR_POLL_ACTIVE = 2000;
const SIDEBAR_POLL_VISIBLE_ACTIVE = 5000;
const SIDEBAR_POLL_IDLE = 30000;
// Retry selected-session state after transient upstream/proxy failures so a
// single hub/reverse blip does not permanently stop active-session updates.
const SESSION_STATE_POLL_RETRY = 5000;
const ACTIVE_TRANSCRIPT_CATCH_UP_TEXT = 'Catching up with this session\u2026';
let activeTranscriptCatchUpWarning = null;
let sidebarStatusTimer = null;
let sidebarStatusEtag = null;
let sidebarHasActive = false;
let sidebarStatusPollEnabled = false;
let sidebarStatusPollPromise = null;
let sidebarStatusPollController = null;
let sidebarStatusPollGeneration = 0;
let sidebarStatusPollInFlightGeneration = -1;
let sidebarStatusPollIsRecovery = false;
let sidebarStatusImmediatePending = false;

const refreshWidgetsSidebar = async () => {
  if (!app.renderWidgetSidebar) return;
  try {
    const resp = await fetch(`${UI_PREFIX}/admin/widgets/status`, {
      headers: requestHeaders('')
    });
    if (resp.status === 404) {
      state.widgets = [];
      state.widgetsLoaded = false;
      app.renderWidgetSidebar?.();
      return;
    }
    if (!resp.ok) return;
    const data = await resp.json();
    state.widgets = Array.isArray(data.widgets) ? data.widgets : [];
    state.widgetsLoaded = true;
    app.renderWidgetSidebar?.();
  } catch (_) {
    // Widgets are optional; leave the section hidden if the admin route is unavailable.
  }
};

const clearSidebarStatusTimer = () => {
  if (sidebarStatusTimer !== null) {
    clearTimeout(sidebarStatusTimer);
    sidebarStatusTimer = null;
  }
};

const stopSidebarStatusPoll = () => {
  const pending = sidebarStatusPollPromise;
  sidebarStatusPollEnabled = false;
  sidebarStatusImmediatePending = false;
  sidebarStatusPollGeneration += 1;
  clearSidebarStatusTimer();
  sidebarStatusPollController?.abort();
  return pending || Promise.resolve(false);
};

const scheduleSidebarStatusPoll = (delay) => {
  clearSidebarStatusTimer();
  if (!sidebarStatusPollEnabled || document.visibilityState === 'hidden') return;
  sidebarStatusTimer = setTimeout(() => {
    sidebarStatusTimer = null;
    return pollSidebarStatus(false);
  }, delay);
};

const sidebarStatusPollDelay = () => {
  sidebarHasActive = hasAnySessionInProgressState();
  if (sidebarHasActive) return SIDEBAR_POLL_ACTIVE;
  if (document.visibilityState === 'visible' && !state.draftSessionActive && getActiveSession()) {
    return SIDEBAR_POLL_VISIBLE_ACTIVE;
  }
  return SIDEBAR_POLL_IDLE;
};

const pollSidebarStatus = (isRecovery = false) => {
  if (!sidebarStatusPollEnabled || document.visibilityState === 'hidden') return Promise.resolve(false);
  if (sidebarStatusPollPromise) return sidebarStatusPollPromise;

  clearSidebarStatusTimer();
  const generation = sidebarStatusPollGeneration;
  const controller = new AbortController();
  sidebarStatusPollController = controller;
  sidebarStatusPollInFlightGeneration = generation;
  sidebarStatusPollIsRecovery = isRecovery;

  const isCurrent = () => (
    sidebarStatusPollEnabled
    && document.visibilityState !== 'hidden'
    && sidebarStatusPollGeneration === generation
    && sidebarStatusPollController === controller
  );

  const request = (async () => {
    try {
      const params = new URLSearchParams();
      const categories = state.sidebarSessionCategories;
      if (Array.isArray(categories) && categories.length > 0 && !categories.includes('all')) {
        params.set('categories', categories.join(','));
      }
      if (state.showHiddenSessions) params.set('include_archived', '1');
      const query = params.toString();

      const headers = requestHeaders('');
      if (sidebarStatusEtag) headers['If-None-Match'] = sidebarStatusEtag;

      const resp = await fetch(`${UI_PREFIX}/v1/sessions/status${query ? `?${query}` : ''}`, {
        headers,
        signal: controller.signal,
      });
      if (!isCurrent()) return false;

      if (resp.status === 304) {
        // No metadata change. If a prior status response revealed an active
        // transcript update but the detail fetch was deferred or failed, retry the
        // pending active-session reconciliation without issuing another status
        // request.
        await reconcilePendingActiveTranscriptRefresh();
        return isCurrent();
      }
      if (!resp.ok) return false;

      const data = await resp.json();
      if (!isCurrent()) return false;
      const etag = resp.headers.get('ETag');
      if (etag) sidebarStatusEtag = etag;
      if (Array.isArray(data.sessions)) {
        updateSidebarStatus(data.sessions);
        await reconcileActiveTranscriptFromStatus(data.sessions);
        if (!isCurrent()) return false;
        recordTranscriptVersionsFromStatus(data.sessions);
        // Discover sessions created in other tabs/devices
        const localIds = new Set(state.sessions.map((s) => s.id));
        const hasUnknown = data.sessions.some((entry) => !localIds.has(entry.id));
        if (hasUnknown) mergeServerSessions();
      }
      return true;
    } catch (_e) {
      // Network error or an intentional visibility abort — recover below when visible.
      return false;
    }
  })();

  const trackedRequest = request.finally(() => {
    if (sidebarStatusPollPromise !== trackedRequest) return;
    sidebarStatusPollPromise = null;
    sidebarStatusPollIsRecovery = false;
    if (sidebarStatusPollController === controller) sidebarStatusPollController = null;

    if (!sidebarStatusPollEnabled || document.visibilityState === 'hidden') return;
    if (sidebarStatusImmediatePending) {
      sidebarStatusImmediatePending = false;
      sidebarStatusEtag = null;
      void pollSidebarStatus(true);
      return;
    }
    if (sidebarStatusPollGeneration === generation) {
      scheduleSidebarStatusPoll(sidebarStatusPollDelay());
    }
  });
  sidebarStatusPollPromise = trackedRequest;
  return trackedRequest;
};

const ensureSidebarStatusPoll = () => {
  if (document.visibilityState === 'hidden') {
    stopSidebarStatusPoll();
    return Promise.resolve(false);
  }

  sidebarStatusPollEnabled = true;
  clearSidebarStatusTimer();
  if (sidebarStatusPollPromise) {
    if (sidebarStatusPollInFlightGeneration !== sidebarStatusPollGeneration) {
      sidebarStatusImmediatePending = true;
      return sidebarStatusPollPromise.then(() => sidebarStatusPollPromise || false);
    }
    return sidebarStatusPollPromise;
  }

  sidebarStatusImmediatePending = false;
  sidebarStatusEtag = null;
  return pollSidebarStatus(true);
};

const startSidebarStatusPoll = () => ensureSidebarStatusPoll();

const refreshSidebarStatusPoll = (forceNow = false) => {
  if (document.visibilityState === 'hidden') return Promise.resolve(false);
  if (forceNow) return ensureSidebarStatusPoll();

  sidebarStatusPollEnabled = true;
  if (!sidebarStatusPollPromise) scheduleSidebarStatusPoll(sidebarStatusPollDelay());
  return Promise.resolve(false);
};

const handleFetchTransportFallback = () => {
  if (document.visibilityState !== 'hidden' && sidebarStatusPollPromise && !sidebarStatusPollIsRecovery) {
    sidebarStatusPollEnabled = true;
    clearSidebarStatusTimer();
    sidebarStatusImmediatePending = true;
    return sidebarStatusPollPromise.then(() => sidebarStatusPollPromise || false);
  }
  return ensureSidebarStatusPoll();
};

const createAndSwitchToFreshSession = async () => {
  await switchToDraftSession({ clearComposer: true, focusPrompt: true });
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
  if (prompt) {
    stageDraftMessage(prompt, sessionId);
    return;
  }
  clearDraftMessageForSession(sessionId);
};

const showActiveTranscriptCatchUpWarning = (session, transcriptUpdatedAt) => {
  const sessionId = String(session?.id || '').trim();
  const targetTranscriptUpdatedAt = Number(transcriptUpdatedAt || 0);
  if (!sessionId || !Number.isFinite(targetTranscriptUpdatedAt) || targetTranscriptUpdatedAt <= 0) return false;

  activeTranscriptCatchUpWarning = {
    sessionId,
    transcriptUpdatedAt: targetTranscriptUpdatedAt,
    connectionStateOwner: setConnectionState(ACTIVE_TRANSCRIPT_CATCH_UP_TEXT, 'bad'),
  };
  return true;
};

const clearActiveTranscriptCatchUpWarning = (sessionId, reconciledTranscriptUpdatedAt = null) => {
  const ownerSessionId = String(sessionId || '').trim();
  const warning = activeTranscriptCatchUpWarning;
  if (!warning || warning.sessionId !== ownerSessionId) return false;

  if (reconciledTranscriptUpdatedAt !== null) {
    const reconciled = Number(reconciledTranscriptUpdatedAt || 0);
    if (!Number.isFinite(reconciled) || reconciled < warning.transcriptUpdatedAt) return false;
  }

  activeTranscriptCatchUpWarning = null;
  return clearConnectionStateOwner(warning.connectionStateOwner);
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
  const previousActiveSessionId = String(state.activeSessionId || '').trim();
  const previousComposerSessionId = state.draftSessionActive ? '' : previousActiveSessionId;
  if (options.clearComposer && previousComposerSessionId === '') {
    clearDraftMessageForSession('');
  } else if (options.clearPreviousComposerDraft) {
    clearDraftMessageForSession(previousComposerSessionId);
  } else {
    stageCurrentComposerForSession(previousComposerSessionId);
  }

  state.sessionSwitchGeneration = Number(state.sessionSwitchGeneration || 0) + 1;
  invalidateSessionStateForSelection('');

  stopSessionStatePoll();
  closeRenameSessionModal();
  closeAskUserModal();
  closeApprovalModal();
  closeMCPModal();
  clearActiveTranscriptCatchUpWarning(previousActiveSessionId);
  clearSessionProviderRetryOwner(previousActiveSessionId);
  if (state.currentStreamSessionId) {
    detachResponseStream();
  } else if (previousActiveSessionId && state.currentStreamSessionId !== previousActiveSessionId) {
    setStreaming(false);
  }

  state.activeSessionId = '';
  state.draftSessionActive = true;
  updateURL('');

  if (options.clearComposer) {
    elements.promptInput.value = '';
    discardPendingAttachments();
    autoGrowPrompt();
  } else if (previousComposerSessionId) {
    discardPendingAttachments();
  }

  refreshPendingInterjectionBanner();
  persistAndRefreshShell();
  renderMessages(true);
  if (!options.clearComposer) {
    restoreDraftMessageForSession('', { replace: true });
  }
  app.activateDiffSidebar?.('');
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

const switchToSession = async (sessionId, options = {}) => {
  const nextId = String(sessionId || '').trim();
  if (!nextId) return null;

  const previousActiveSessionId = String(state.activeSessionId || '').trim();
  const previousComposerSessionId = state.draftSessionActive ? '' : previousActiveSessionId;
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

  const switchGeneration = (Number(state.sessionSwitchGeneration || 0) + 1);
  state.sessionSwitchGeneration = switchGeneration;
  invalidateSessionStateForSelection(nextId);
  const isCurrentSwitch = () => state.sessionSwitchGeneration === switchGeneration
    && String(state.activeSessionId || '').trim() === nextId
    && !state.draftSessionActive;

  stopSessionStatePoll();
  closeRenameSessionModal();
  if (state.askUser?.sessionId && state.askUser.sessionId !== nextId) {
    closeAskUserModal();
  }
  if (state.approval?.sessionId && state.approval.sessionId !== nextId) {
    closeApprovalModal();
  }
  if (mcpModalSessionId && mcpModalSessionId !== nextId) {
    closeMCPModal();
  }
  if (previousActiveSessionId && previousActiveSessionId !== nextId) {
    clearActiveTranscriptCatchUpWarning(previousActiveSessionId);
    clearSessionProviderRetryOwner(previousActiveSessionId);
  }
  if (state.currentStreamSessionId && state.currentStreamSessionId !== nextId) {
    detachResponseStream();
  }
  if (previousActiveSessionId && previousActiveSessionId !== nextId && state.currentStreamSessionId !== nextId) {
    setStreaming(false);
  }

  if (previousActiveSessionId !== nextId || state.draftSessionActive) {
    discardPendingAttachments();
  }

  state.activeSessionId = nextId;
  state.draftSessionActive = false;
  updateURL(sessionSlug(session));
  refreshPendingInterjectionBanner();

  let preloadServerMessagesPromise = null;
  if (session._serverOnly) {
    preloadServerMessagesPromise = loadServerSessionMessages(session.id);
  }

  persistAndRefreshShell();
  renderMessages(true);
  restoreDraftMessageForSession(session.id, { replace: true });
  app.activateDiffSidebar?.(session.id);

  let didPreloadServerMessages = false;
  if (preloadServerMessagesPromise) {
    const msgs = await preloadServerMessagesPromise;
    if (!isCurrentSwitch()) return null;
    if (Array.isArray(msgs)) {
      mergeServerMessagesWithLocalState(session, msgs);
      persistAndRefreshShell();
      if (isCurrentSwitch()) {
        renderMessages(true);
      }
      didPreloadServerMessages = true;
    }
  }

  if (options.sync !== false) {
    await syncActiveSessionFromServer(session, true, {
      skipMessagesFetch: didPreloadServerMessages,
      expectedSwitchGeneration: switchGeneration
    });
    if (!isCurrentSwitch()) return null;
  }
  if (!isCurrentSwitch()) return null;
  await app.refreshSkillCommands?.(session.id);
  if (!isCurrentSwitch()) return null;
  if (syncSelectedRuntimeFromSession(session)) {
    app.updateHeader();
  }
  if (options.focusPrompt) {
    elements.promptInput.focus();
  }
  if (options.closeSidebar !== false) {
    closeSidebarIfMobile();
  }
  return session;
};

// ===== Server session helpers =====
const safeServerIdToken = (value) => {
  const token = String(value ?? '').trim();
  return token ? token.replace(/[^A-Za-z0-9_-]/g, '_') : '';
};

const serverMessageRawKey = (msg) => {
  if (!msg || typeof msg !== 'object') return '';
  if (msg.sequence !== undefined && msg.sequence !== null && Number.isFinite(Number(msg.sequence))) {
    return `seq:${Number(msg.sequence)}`;
  }
  if (msg.id !== undefined && msg.id !== null && String(msg.id).trim() !== '') {
    return `id:${String(msg.id)}`;
  }
  return '';
};

const serverMessageBaseId = (msg) => {
  if (!msg || typeof msg !== 'object') return generateId('msg');
  if (msg.sequence !== undefined && msg.sequence !== null && Number.isFinite(Number(msg.sequence))) {
    return `srv_seq_${Number(msg.sequence)}`;
  }
  if (msg.id !== undefined && msg.id !== null && String(msg.id).trim() !== '') {
    const token = safeServerIdToken(msg.id);
    if (token) return `srv_${token}`;
  }
  return generateId('msg');
};

const serverMessageSequence = (msg) => {
  const seq = Number(msg?.sequence);
  return Number.isFinite(seq) ? seq : null;
};

const serverMessageCreatedAt = (msg) => {
  const created = Number(msg?.created_at);
  return Number.isFinite(created) && created > 0 ? created : Date.now();
};

const isInternalCompactionSummaryText = (text) => (
  String(text || '').trimStart().startsWith('[Context Compaction]')
);

const compactionSummaryDisplayText = (text) => {
  let value = String(text || '').replace(/\r\n?/g, '\n');
  const summaryMatch = value.match(/<SUMMARY_AND_NEXT_ACTIONS>\n?([\s\S]*?)\n?<\/SUMMARY_AND_NEXT_ACTIONS>/);
  if (summaryMatch) return summaryMatch[1].trim();
  value = value.replace(/^\s*\[Context Compaction\]\s*/, '');
  value = value.replace(/<PREVIOUS_TURNS>\n?[\s\S]*?\n?<\/PREVIOUS_TURNS>/g, '');
  return value.trim();
};

const lineCount = (text) => {
  const value = String(text || '').trim();
  return value ? value.split('\n').length : 0;
};

const responseCompactionMetadata = (data = {}) => {
  const seq = Number(data.compaction_seq ?? data.compactionSeq);
  const count = Number(data.compaction_count ?? data.compactionCount);
  return {
    compactionSeq: Number.isFinite(seq) ? seq : -1,
    compactionCount: Number.isFinite(count) ? count : 0
  };
};

const messageDedupeKey = (message) => {
  if (!message || typeof message !== 'object') return '';
  if (message.role === 'skill-run') {
    return JSON.stringify({ role: message.role, runId: message.runId || '' });
  }
  if (message.role === 'tool-group') {
    return JSON.stringify({
      role: message.role,
      status: message.status || '',
      tools: Array.isArray(message.tools)
        ? message.tools.map((tool) => ({
          name: tool?.name || '',
          arguments: tool?.arguments || '',
          status: tool?.status || '',
          images: Array.isArray(tool?.images) ? tool.images : []
        }))
        : []
    });
  }
  return JSON.stringify({
    role: message.role || '',
    content: message.content || '',
    attachments: Array.isArray(message.attachments)
      ? message.attachments.map((attachment) => ({
        name: attachment?.name || '',
        type: attachment?.type || '',
        dataURL: attachment?.dataURL || '',
        previewURL: attachment?.previewURL || ''
      }))
      : []
  });
};

const messageFingerprints = (messages, metrics = null) => (Array.isArray(messages) ? messages : []).map((message) => {
  if (metrics) metrics.fingerprints = Number(metrics.fingerprints || 0) + 1;
  return messageDedupeKey(message);
});

const longestCompactionTailOverlap = (fingerprints, markerIndex, start, metrics = null) => {
  const maxLength = Math.min(markerIndex, fingerprints.length - start);
  if (maxLength <= 0) return 0;

  const pattern = fingerprints.slice(start, start + maxLength);
  const sequence = pattern.concat([null], fingerprints.slice(0, markerIndex));
  const prefix = new Array(sequence.length).fill(0);
  const equalAt = (left, right) => {
    if (metrics) metrics.operations = Number(metrics.operations || 0) + 1;
    return sequence[left] === sequence[right];
  };

  for (let index = 1; index < sequence.length; index += 1) {
    let matched = prefix[index - 1];
    while (matched > 0 && !equalAt(index, matched)) {
      matched = prefix[matched - 1];
    }
    if (equalAt(index, matched)) matched += 1;
    prefix[index] = matched;
  }

  return Math.min(maxLength, prefix[prefix.length - 1]);
};

const isSyntheticCompactionAckMessage = (message) => (
  message?.role === 'assistant' && String(message.content || '').trim() === "I've reviewed the context summary. I'll continue from where we left off."
);

const compactionDuplicateTailRange = (messages, markerIndex, fingerprints = null, metrics = null) => {
  if (markerIndex <= 0 || markerIndex + 1 >= messages.length) return { start: -1, length: 0 };
  const keys = Array.isArray(fingerprints) && fingerprints.length === messages.length
    ? fingerprints
    : messageFingerprints(messages, metrics);
  const candidates = [markerIndex + 1];
  if (isSyntheticCompactionAckMessage(messages[markerIndex + 1])) candidates.push(markerIndex + 2);
  let bestStart = -1;
  let bestLength = 0;
  candidates.forEach((start) => {
    if (start >= messages.length) return;
    const length = longestCompactionTailOverlap(keys, markerIndex, start, metrics);
    if (length > bestLength) {
      bestStart = start;
      bestLength = length;
    }
  });
  return { start: bestStart, length: bestLength };
};

const suppressCompactionTailMessages = (messages) => {
  if (!Array.isArray(messages) || messages.length === 0) return messages;
  const out = messages.slice();
  const fingerprints = messageFingerprints(out);
  for (let index = 0; index < out.length; index += 1) {
    if (out[index]?.role !== 'compaction') continue;
    if (out[index]?.authoritativeTailSuppressed) continue;
    const { start, length } = compactionDuplicateTailRange(out, index, fingerprints);
    if (length > 0) {
      const removeCount = start + length - (index + 1);
      out.splice(index + 1, removeCount);
      fingerprints.splice(index + 1, removeCount);
    } else if (isSyntheticCompactionAckMessage(out[index + 1])) {
      out.splice(index + 1, 1);
      fingerprints.splice(index + 1, 1);
    }
  }
  return out;
};

const annotateCompactionBoundary = (messages, options = {}) => {
  const seq = Number(options.compactionSeq);
  if (!Number.isFinite(seq) || seq < 0 || !Array.isArray(messages) || messages.length === 0) {
    return messages;
  }
  const boundaryIndex = messages.findIndex((message) => {
    const messageSeq = Number(message?.serverSeq);
    return Number.isFinite(messageSeq) && messageSeq >= seq;
  });
  if (boundaryIndex < 0) return messages;

  const count = Number(options.compactionCount);
  const boundary = messages[boundaryIndex];
  if (boundary?.role === 'compaction') {
    boundary.activeBoundary = true;
    boundary.compactionSeq = seq;
    if (Number.isFinite(count) && count > 0) boundary.compactionCount = count;
    return messages;
  }

  const marker = {
    id: `compaction_boundary_${seq}`,
    role: 'compaction-boundary',
    content: 'Context compacted',
    activeBoundary: true,
    compactionSeq: seq,
    created: boundary?.created || Date.now()
  };
  if (Number.isFinite(count) && count > 0) marker.compactionCount = count;
  messages.splice(boundaryIndex, 0, marker);
  return messages;
};

const convertServerMessages = (serverMessages, options = {}) => {
  const result = [];
  let currentGroup = null;
  let pendingCompactionMarkerIndex = -1;

  const normalizeImages = (images) => (
    Array.isArray(images)
      ? images.map((url) => rebaseSessionAssetURL(url)).filter(Boolean)
      : []
  );

  const appendUniqueImages = (tool, images) => {
    if (!tool || images.length === 0) return;
    const existing = Array.isArray(tool.images) ? tool.images : [];
    images.forEach((url) => {
      if (url && !existing.includes(url)) existing.push(url);
    });
    if (existing.length > 0) tool.images = existing;
  };

  const flushGroup = () => {
    if (currentGroup) {
      result.push(currentGroup);
      currentGroup = null;
    }
  };

  const markAuthoritativeCompactionTailSuppressed = () => {
    if (pendingCompactionMarkerIndex < 0) return;
    const marker = result[pendingCompactionMarkerIndex];
    if (marker?.role === 'compaction') marker.authoritativeTailSuppressed = true;
  };

  const clearPendingCompactionTail = () => {
    pendingCompactionMarkerIndex = -1;
  };

  const toolGroupId = (msg, partIndex) => `${serverMessageBaseId(msg)}_tools_${partIndex}`;
  const fallbackToolId = (msg, partIndex) => `${serverMessageBaseId(msg)}_tool_${partIndex}`;

  const ensureToolGroup = (created, msg, partIndex) => {
    if (!currentGroup) {
      currentGroup = {
        id: toolGroupId(msg, partIndex),
        role: 'tool-group',
        tools: [],
        expanded: false,
        status: 'done',
        created,
        ...(serverMessageSequence(msg) !== null ? { serverSeq: serverMessageSequence(msg) } : {})
      };
    }
    return currentGroup;
  };

  const attachToolResultState = (part, created, msg, partIndex) => {
    const images = normalizeImages(part.images);
    const callId = part.tool_call_id || '';
    let group = currentGroup;
    let tool = group && callId ? group.tools.find((entry) => entry.id === callId) : null;
    if (!tool && group && part.tool_name) {
      tool = group.tools.find((entry) => entry.name === part.tool_name);
    }
    // Result-only rows can be separated from their call by a page boundary.
    // Do not invent a generic row; conversion will correlate them once the page
    // containing the call is loaded. Image results still need a fallback card.
    if (!tool && images.length === 0) return;
    if (!group) group = ensureToolGroup(created, msg, partIndex);
    if (!tool) {
      tool = {
        id: callId || fallbackToolId(msg, partIndex),
        name: part.tool_name || 'tool',
        arguments: '',
        status: 'done',
        created
      };
      group.tools.push(tool);
    }
    const failed = Boolean(part.tool_error || part.is_error);
    tool.status = failed ? 'error' : 'done';
    tool.resultStatus = failed ? 'error' : 'success';
    appendUniqueImages(tool, images);
  };

  for (const msg of serverMessages) {
    const parts = Array.isArray(msg.parts) ? msg.parts : [];
    const created = serverMessageCreatedAt(msg);
    const baseId = serverMessageBaseId(msg);
    const seq = serverMessageSequence(msg);

    if (msg.role === 'system' || msg.role === 'developer') continue;
    if (msg.compaction_tail || msg.compactionTail) {
      flushGroup();
      markAuthoritativeCompactionTailSuppressed();
      continue;
    }
    clearPendingCompactionTail();

    if (msg.role === 'event') {
      flushGroup();
      const skillMarker = parts.find((part) => part.type === 'skill_activation' && part.skill_activation?.run_id);
      if (skillMarker) {
        const provenance = skillMarker.skill_activation;
        const textMarker = parts.find((part) => part.type === 'text');
        const text = String(textMarker?.text || '');
        const outputBreak = text.indexOf('\n\n');
        const started = Date.parse(provenance.started_at || '');
        const completed = Date.parse(provenance.completed_at || '');
        const skillRun = {
          id: `skill-run-${provenance.run_id}`,
          role: 'skill-run',
          runId: provenance.run_id,
          skill: provenance.name || 'skill',
          agent: provenance.agent || '',
          status: provenance.status || 'running',
          output: outputBreak >= 0 ? text.slice(outputBreak + 2) : '',
          childSessionId: provenance.child_session_id || '',
          durationMs: Number.isFinite(started) && Number.isFinite(completed) && completed >= started ? completed - started : 0,
          provenance,
          created,
          ...(seq !== null ? { serverSeq: seq } : {})
        };
        const previousIndex = result.findIndex((entry) => entry.role === 'skill-run' && entry.runId === provenance.run_id);
        if (previousIndex >= 0) result[previousIndex] = skillRun;
        else result.push(skillRun);
        continue;
      }
      const errorMarker = parts.find((part) => part.type === 'error');
      if (errorMarker) {
        result.push({
          id: baseId,
          role: 'error',
          content: errorMarker.text || 'The response failed.',
          created,
          ...(seq !== null ? { serverSeq: seq } : {})
        });
        continue;
      }
      const marker = parts.find((part) => part.type === 'model_swap') || parts.find((part) => part.type === 'text');
      result.push({
        id: baseId,
        role: 'model-swap',
        content: marker?.text || '↔ Model switch',
        created,
        ...(seq !== null ? { serverSeq: seq } : {})
      });
      continue;
    }

    if (msg.role === 'user') {
      flushGroup();

      const attachments = [];
      const textParts = [];
      for (const part of parts) {
        if (part.type === 'image' && part.image_url) {
          attachments.push({
            name: 'image',
            type: part.mime_type || 'image/*',
            dataURL: rebaseSessionAssetURL(part.image_url)
          });
        } else if (part.type === 'text' && part.text) {
          textParts.push(part.text);
        }
      }

      const content = textParts.join('\n');
      if (isInternalCompactionSummaryText(content)) {
        result.push({
          id: baseId,
          role: 'compaction',
          content: 'Context compacted',
          rawContent: content,
          lineCount: lineCount(compactionSummaryDisplayText(content)),
          created,
          ...(seq !== null ? { serverSeq: seq, compactionSeq: seq } : {})
        });
        pendingCompactionMarkerIndex = result.length - 1;
        continue;
      }

      result.push({
        id: baseId,
        role: 'user',
        content,
        created,
        ...(seq !== null ? { serverSeq: seq } : {}),
        ...(attachments.length > 0 ? { attachments } : {})
      });
      continue;
    }

    // Walk through assistant parts in order to preserve interleaving with tool calls.
    for (let partIndex = 0; partIndex < parts.length; partIndex += 1) {
      const part = parts[partIndex];
      if (part.type === 'text' && part.text && String(part.text).trim() !== '') {
        flushGroup();
        result.push({
          id: `${baseId}_text_${partIndex}`,
          role: 'assistant',
          content: part.text,
          created,
          ...(seq !== null ? { serverSeq: seq } : {})
        });
      } else if (part.type === 'tool_call') {
        const group = ensureToolGroup(created, msg, partIndex);
        const toolId = part.tool_call_id || fallbackToolId(msg, partIndex);
        let toolEntry = group.tools.find((entry) => entry.id === toolId);
        if (!toolEntry) {
          toolEntry = {
            id: toolId,
            name: part.tool_name || 'tool',
            arguments: part.tool_arguments || '',
            status: part.tool_error ? 'error' : 'done',
            ...(part.tool_error ? { resultStatus: 'error' } : {}),
            created
          };
          group.tools.push(toolEntry);
        } else {
          toolEntry.name = part.tool_name || toolEntry.name || 'tool';
          toolEntry.arguments = part.tool_arguments || toolEntry.arguments || '';
          toolEntry.status = part.tool_error ? 'error' : 'done';
          if (part.tool_error) toolEntry.resultStatus = 'error';
        }
        appendUniqueImages(toolEntry, normalizeImages(part.images));
      } else if (part.type === 'tool_result') {
        attachToolResultState(part, created, msg, partIndex);
      }
    }

  }

  flushGroup();
  return annotateCompactionBoundary(suppressCompactionTailMessages(result), options);
};

const SESSION_MESSAGES_PAGE_SIZE = 200;
const SESSION_OLDER_LOAD_TOP_THRESHOLD_PX = 600;
const SESSION_OLDER_LOAD_CHAIN_LIMIT = 25;

const findSessionById = (sessionId) => state.sessions.find((item) => item?.id === sessionId) || null;

const ensureSessionHistory = (session) => {
  if (!session || typeof session !== 'object') return null;
  if (!session._history || typeof session._history !== 'object') {
    session._history = {
      rawMessages: [],
      oldestSeq: 0,
      hasMoreOlder: false,
      loadingOlder: false,
      loadedTail: false,
      lastResponseId: '',
      compactionSeq: -1,
      compactionCount: 0,
      tailEtag: '',
      tailTranscriptUpdatedAt: 0,
      refreshingTail: false
    };
  }
  const history = session._history;
  if (!Array.isArray(history.rawMessages)) history.rawMessages = [];
  history.oldestSeq = Number.isFinite(Number(history.oldestSeq)) ? Number(history.oldestSeq) : 0;
  history.compactionSeq = Number.isFinite(Number(history.compactionSeq)) ? Number(history.compactionSeq) : -1;
  history.compactionCount = Number.isFinite(Number(history.compactionCount)) ? Number(history.compactionCount) : 0;
  history.hasMoreOlder = history.hasMoreOlder === true;
  history.loadingOlder = history.loadingOlder === true;
  history.loadedTail = history.loadedTail === true;
  history.lastResponseId = String(history.lastResponseId || '').trim();
  history.tailEtag = String(history.tailEtag || '').trim();
  history.tailTranscriptUpdatedAt = Number.isFinite(Number(history.tailTranscriptUpdatedAt)) ? Number(history.tailTranscriptUpdatedAt) : 0;
  history.refreshingTail = history.refreshingTail === true;
  return history;
};

const oldestSequenceFromRawMessages = (messages) => {
  let oldest = null;
  for (const msg of Array.isArray(messages) ? messages : []) {
    const seq = serverMessageSequence(msg);
    if (seq === null) continue;
    if (oldest === null || seq < oldest) oldest = seq;
  }
  return oldest;
};

const nextBeforeSeqFromResponse = (data) => {
  const seq = Number(data?.next_before_seq);
  return Number.isFinite(seq) && seq >= 0 ? seq : null;
};

const sortRawServerMessagesChronological = (messages) => messages
  .map((msg, index) => ({ msg, index }))
  .sort((a, b) => {
    const aSeq = serverMessageSequence(a.msg);
    const bSeq = serverMessageSequence(b.msg);
    if (aSeq !== null && bSeq !== null && aSeq !== bSeq) return aSeq - bSeq;
    const aCreated = Number(a.msg?.created_at) || 0;
    const bCreated = Number(b.msg?.created_at) || 0;
    if (aCreated !== bCreated) return aCreated - bCreated;
    return a.index - b.index;
  })
  .map((entry) => entry.msg);

const mergeRawServerMessages = (...messageLists) => {
  const keyed = new Map();
  const unkeyed = [];
  for (const list of messageLists) {
    for (const msg of Array.isArray(list) ? list : []) {
      const key = serverMessageRawKey(msg);
      if (key) {
        keyed.set(key, msg);
      } else {
        unkeyed.push(msg);
      }
    }
  }
  return sortRawServerMessagesChronological([...keyed.values(), ...unkeyed]);
};

const rawServerMessagesOverlap = (left, right) => {
  const keys = new Set((Array.isArray(left) ? left : []).map(serverMessageRawKey).filter(Boolean));
  if (keys.size === 0) return false;
  return (Array.isArray(right) ? right : []).some((msg) => {
    const key = serverMessageRawKey(msg);
    return key && keys.has(key);
  });
};

const convertedMessagesFromHistory = (history) => {
  const converted = convertServerMessages(history?.rawMessages || [], {
    compactionSeq: history?.compactionSeq,
    compactionCount: history?.compactionCount
  });
  const lastResponseId = String(history?.lastResponseId || '').trim();
  if (lastResponseId) converted.lastResponseId = lastResponseId;
  return converted;
};

const compactedTailRefreshPreservedPrefix = (previousRaw, rawTail, compactionSeq) => {
  const boundary = Number(compactionSeq);
  if (!Number.isFinite(boundary) || boundary < 0) return [];
  if (!Array.isArray(previousRaw) || previousRaw.length === 0) return [];
  if (!Array.isArray(rawTail) || rawTail.length === 0) return [];

  // A compaction appends a new active-context suffix to the server log. During
  // the completion/state refresh that follows, the tail window may no longer
  // overlap the scrollback the browser already loaded. Do not throw away the
  // pre-compaction transcript in that case; it is still valid display history
  // and may be the only copy the user can scroll back through without another
  // page fetch.
  return previousRaw.filter((msg) => {
    const seq = serverMessageSequence(msg);
    return seq !== null && seq < boundary;
  });
};

const applyTailMessagesToSessionHistory = (sessionId, data) => {
  const rawTail = Array.isArray(data?.messages) ? data.messages.slice() : [];
  const lastResponseId = String(data?.lastResponseId || '').trim();
  const compactionMeta = responseCompactionMetadata(data);
  const session = findSessionById(sessionId);
  if (!session) {
    const converted = convertServerMessages(rawTail, compactionMeta);
    if (lastResponseId) converted.lastResponseId = lastResponseId;
    return converted;
  }

  const history = ensureSessionHistory(session);
  const hadRawMessages = history.rawMessages.length > 0;
  const previousRawMessages = hadRawMessages ? history.rawMessages.slice() : [];
  const overlapsExisting = hadRawMessages && rawServerMessagesOverlap(history.rawMessages, rawTail);
  const preservedCompactionPrefix = overlapsExisting
    ? []
    : compactedTailRefreshPreservedPrefix(previousRawMessages, rawTail, compactionMeta.compactionSeq);
  const preservesExistingScrollback = preservedCompactionPrefix.length > 0;
  const previousHasMoreOlder = history.hasMoreOlder === true;
  const previousOldestSeq = Number(history.oldestSeq) || 0;

  history.rawMessages = overlapsExisting
    ? mergeRawServerMessages(history.rawMessages, rawTail)
    : mergeRawServerMessages(preservedCompactionPrefix, rawTail);
  history.loadedTail = true;
  history.loadingOlder = false;
  history.lastResponseId = lastResponseId;
  history.compactionSeq = compactionMeta.compactionSeq;
  history.compactionCount = compactionMeta.compactionCount;

  if (overlapsExisting || preservesExistingScrollback) {
    history.hasMoreOlder = previousHasMoreOlder || data?.has_more === true;
  } else {
    history.hasMoreOlder = data?.has_more === true;
  }

  const oldestSeq = oldestSequenceFromRawMessages(history.rawMessages);
  const responseCursor = nextBeforeSeqFromResponse(data);
  if ((overlapsExisting || preservesExistingScrollback) && previousOldestSeq > 0) {
    // Tail refreshes can overlap the loaded window after older pages have been
    // prepended. Keep the existing older cursor; the tail page cursor only
    // describes the oldest row in the tail page, not the oldest row already
    // loaded in the combined window. The same applies after compaction: a
    // non-overlapping compacted tail must not make the browser forget the
    // pre-compaction scrollback it already has.
    history.oldestSeq = previousOldestSeq;
  } else {
    history.oldestSeq = history.hasMoreOlder && responseCursor !== null
      ? responseCursor
      : (oldestSeq !== null ? oldestSeq : (responseCursor !== null ? responseCursor : 0));
  }

  const converted = convertedMessagesFromHistory(history);
  if (!overlapsExisting && compactionMeta.compactionSeq >= 0) {
    converted.preserveLocalCompactionScrollback = true;
  }
  return converted;
};

const fetchServerSessionMessagesPage = async (sessionId, { limit = 0, offset = 0, tail = false, beforeSeq = 0, etag = '' } = {}) => {
  const headers = {};
  if (state.token) headers.Authorization = `Bearer ${state.token}`;
  if (etag) headers['If-None-Match'] = etag;

  const params = new URLSearchParams();
  if (tail) params.set('tail', '1');
  if (beforeSeq > 0) params.set('before_seq', String(beforeSeq));
  if (limit > 0) params.set('limit', String(limit));
  if (offset > 0) params.set('offset', String(offset));
  const query = params.toString();

  const resp = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(sessionId)}/messages${query ? `?${query}` : ''}`, { headers });
  if (resp.status === 304) return false;
  if (!resp.ok) return null;

  const data = await resp.json().catch(() => null);
  if (!data || !Array.isArray(data.messages)) return null;
  return {
    data,
    etag: resp.headers.get('ETag') || ''
  };
};

const loadServerSessionMessages = async (sessionId, options = {}) => {
  try {
    const session = findSessionById(sessionId);
    const history = session ? ensureSessionHistory(session) : null;
    const explicitEtag = String(options.etag || '').trim();
    const etag = explicitEtag || (options.useEtag && history ? history.tailEtag : '');
    const page = await fetchServerSessionMessagesPage(sessionId, {
      tail: true,
      limit: SESSION_MESSAGES_PAGE_SIZE,
      etag
    });
    if (page === false) return false;
    if (!page) return null;
    if (history && page.etag) history.tailEtag = page.etag;
    const converted = applyTailMessagesToSessionHistory(sessionId, page.data);
    const transcriptUpdatedAt = numericTranscriptUpdatedAt(options.transcriptUpdatedAt);
    if (history && transcriptUpdatedAt > 0) {
      history.tailTranscriptUpdatedAt = Math.max(history.tailTranscriptUpdatedAt || 0, transcriptUpdatedAt);
      if (transcriptUpdatedAt > numericTranscriptUpdatedAt(session.transcriptUpdatedAt)) {
        session.transcriptUpdatedAt = transcriptUpdatedAt;
      }
    }
    return converted;
  } catch {
    return null;
  }
};

const loadOlderSessionMessages = async (session) => {
  if (!session || session.id !== state.activeSessionId) return false;
  if (state.abortController || sessionHasInProgressState(session)) return false;

  const history = ensureSessionHistory(session);
  if (!history.loadedTail || !history.hasMoreOlder || history.loadingOlder) return false;

  const beforeSeq = Number(history.oldestSeq);
  if (!Number.isFinite(beforeSeq) || beforeSeq <= 0) {
    history.hasMoreOlder = false;
    return false;
  }

  const chatScroll = elements.chatScroll;
  const oldHeight = Number(chatScroll?.scrollHeight) || 0;
  const oldTop = Number(chatScroll?.scrollTop) || 0;

  history.loadingOlder = true;
  try {
    const page = await fetchServerSessionMessagesPage(session.id, {
      beforeSeq,
      limit: SESSION_MESSAGES_PAGE_SIZE
    });
    if (!page || page === false) return false;

    const rawOlder = Array.isArray(page.data.messages) ? page.data.messages.slice() : [];
    if (rawOlder.length === 0) {
      const emptyPageCursor = nextBeforeSeqFromResponse(page.data);
      if (page.data.has_more === true && emptyPageCursor !== null && emptyPageCursor < beforeSeq) {
        history.oldestSeq = emptyPageCursor;
        history.hasMoreOlder = true;
      } else {
        history.hasMoreOlder = false;
      }
      return false;
    }

    const previousRaw = history.rawMessages.slice();
    history.rawMessages = mergeRawServerMessages(rawOlder, previousRaw);
    const oldestSeq = oldestSequenceFromRawMessages(history.rawMessages);
    const responseCursor = nextBeforeSeqFromResponse(page.data);
    history.oldestSeq = page.data.has_more === true && responseCursor !== null
      ? responseCursor
      : (oldestSeq !== null ? oldestSeq : (responseCursor !== null ? responseCursor : beforeSeq));
    history.hasMoreOlder = page.data.has_more === true;
    const lastResponseId = String(page.data.lastResponseId || '').trim();
    if (lastResponseId) history.lastResponseId = lastResponseId;
    const compactionMeta = responseCompactionMetadata(page.data);
    history.compactionSeq = compactionMeta.compactionSeq;
    history.compactionCount = compactionMeta.compactionCount;

    const converted = convertedMessagesFromHistory(history);
    mergeServerMessagesWithLocalState(session, converted);
    renderSidebar();

    if (session.id === state.activeSessionId) {
      noteUserScrollIntent();
      renderMessages(false);
      const newHeight = Number(chatScroll?.scrollHeight) || oldHeight;
      chatScroll.scrollTop = oldTop + Math.max(0, newHeight - oldHeight);
    }
    return true;
  } catch {
    return false;
  } finally {
    history.loadingOlder = false;
  }
};

const shouldLoadOlderForSession = (session) => {
  if (!session || session.id !== state.activeSessionId) return false;
  const history = ensureSessionHistory(session);
  if (!history.loadedTail || !history.hasMoreOlder || history.loadingOlder) return false;
  if (state.abortController || sessionHasInProgressState(session)) return false;

  const chatScroll = elements.chatScroll;
  const scrollTop = Number(chatScroll?.scrollTop) || 0;
  const clientHeight = Number(chatScroll?.clientHeight) || 0;
  const threshold = Math.max(SESSION_OLDER_LOAD_TOP_THRESHOLD_PX, clientHeight * 2);
  return scrollTop <= threshold;
};

const maybeLoadOlderSessionMessages = async () => {
  const session = getActiveSession();
  if (!shouldLoadOlderForSession(session)) return false;

  let loadedAny = false;
  for (let attempts = 0; attempts < SESSION_OLDER_LOAD_CHAIN_LIMIT; attempts += 1) {
    const loaded = await loadOlderSessionMessages(session);
    if (!loaded) return loadedAny;
    loadedAny = true;

    // A page containing only tool calls can merge into the existing collapsed
    // tool group at the top of the viewport, so the rendered height barely
    // changes and no follow-up scroll event fires. Keep draining while we are
    // still near the top so scrollback can cross tool-only page boundaries.
    if (!shouldLoadOlderForSession(session)) break;
  }
  return loadedAny;
};

const normalizeMCPServerView = (server) => {
  if (!server || typeof server !== 'object') return null;
  const name = String(server.name || '').trim();
  if (!name) return null;
  return {
    name,
    configured: server.configured !== false,
    enabled: Boolean(server.enabled),
    status: String(server.status || (server.enabled ? 'ready' : 'stopped')).trim() || 'stopped',
    error: String(server.error || '').trim(),
    tools: Number.isFinite(Number(server.tools)) ? Number(server.tools) : 0,
  };
};

const applyGoalStateToSession = (session, data) => {
  if (!session || !data || typeof data !== 'object' || !Object.prototype.hasOwnProperty.call(data, 'goal')) return false;
  const nextGoal = data.goal && typeof data.goal === 'object' ? { ...data.goal } : null;
  const before = JSON.stringify(session.goal || null);
  const after = JSON.stringify(nextGoal || null);
  if (before === after) return false;
  session.goal = nextGoal;
  return true;
};

const formatGoalChipText = (goal) => {
  if (!goal || typeof goal !== 'object') return '';
  const status = String(goal.status || 'active').trim() || 'active';
  let text = `🎯 ${status}`;
  const used = Number(goal.tokens_used || 0);
  const budget = Number(goal.token_budget || 0);
  if (budget > 0) text += ` · ${Math.max(0, used)}/${budget} tok`;
  const objective = String(goal.objective || '').trim();
  if (objective) text += ` · ${objective}`;
  return text;
};

const updateGoalChip = (session = ensureActiveSession?.()) => {
  const chip = elements.goalChip;
  if (!chip) return;
  const goal = session?.goal || null;
  const text = formatGoalChipText(goal);
  if (!text) {
    chip.className = 'goal-chip hidden';
    chip.textContent = '';
    return;
  }
  const status = String(goal.status || 'active').trim() || 'active';
  chip.className = `goal-chip goal-${status}`;
  chip.textContent = text;
  chip.title = text;
};

const applyMCPStateToSession = (session, data) => {
  if (!session || !data || typeof data !== 'object') return false;
  const hasServerField = Array.isArray(data.servers) || Array.isArray(data.mcp_servers);
  const hasEnabledField = Array.isArray(data.enabled) || Array.isArray(data.mcp_enabled);
  if (!hasServerField && !hasEnabledField) return false;
  const servers = hasServerField
    ? (Array.isArray(data.servers)
      ? data.servers.map(normalizeMCPServerView).filter(Boolean)
      : data.mcp_servers.map(normalizeMCPServerView).filter(Boolean))
    : (Array.isArray(session.mcpServers) ? session.mcpServers.slice() : []);
  const enabledSource = Array.isArray(data.enabled)
    ? data.enabled
    : (Array.isArray(data.mcp_enabled) ? data.mcp_enabled : servers.filter((server) => server.enabled).map((server) => server.name));
  const enabled = [];
  const seen = new Set();
  enabledSource.forEach((raw) => {
    const name = String(raw || '').trim();
    if (!name || seen.has(name)) return;
    seen.add(name);
    enabled.push(name);
  });
  const serverJSON = JSON.stringify(servers);
  const enabledJSON = JSON.stringify(enabled);
  const changed = JSON.stringify(session.mcpServers || []) !== serverJSON
    || JSON.stringify(session.mcpEnabled || []) !== enabledJSON;
  session.mcpServers = servers;
  session.mcpEnabled = enabled;
  updateMCPStatusDisplay(session);
  return changed;
};

// loadServerSessionState always returns one of these discriminated result
// shapes. Callers must retry only `retry`; `auth` is a terminal authentication
// failure and can never be confused with a falsy transient response.
const SESSION_STATE_AUTH_RESULT = Object.freeze({ kind: 'auth' });
const SESSION_STATE_RETRY_RESULT = Object.freeze({ kind: 'retry' });
const sessionStateOKResult = (stateValue) => ({ kind: 'ok', state: stateValue });

const loadServerSessionState = async (sessionId) => {
  try {
    const headers = {};
    if (state.token) headers.Authorization = `Bearer ${state.token}`;
    const resp = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(sessionId)}/state`, { headers });
    if (!resp.ok) {
      if (resp.status === 404) {
        return sessionStateOKResult({ active_run: false, active_response_id: '' });
      }
      if (resp.status === 401) {
        handleAuthFailure();
        return SESSION_STATE_AUTH_RESULT;
      }
      return SESSION_STATE_RETRY_RESULT;
    }
    const data = await resp.json().catch(() => null);
    if (!data || typeof data !== 'object') return SESSION_STATE_RETRY_RESULT;
    return sessionStateOKResult(data);
  } catch {
    return SESSION_STATE_RETRY_RESULT;
  }
};

const localCompactionScrollbackPrefix = (session, serverMessages) => {
  if (!session || !Array.isArray(session.messages) || session.messages.length === 0) return [];
  if (!Array.isArray(serverMessages) || serverMessages.preserveLocalCompactionScrollback !== true) return [];

  const existingKeys = new Set(serverMessages.map(messageDedupeKey).filter(Boolean));
  const prefix = [];
  for (const message of session.messages) {
    if (!message || message.transient || message.role === 'phase' || message.role === 'error') continue;
    const key = messageDedupeKey(message);
    if (key && existingKeys.has(key)) continue;
    prefix.push({ ...message });
    if (key) existingKeys.add(key);
  }
  return prefix;
};

const isPreservableLocalTailMessage = (message) => {
  if (!message || message.transient) return false;
  if (message.role === 'assistant') {
    return String(message.content || '').length > 0
      || (Array.isArray(message.attachments) && message.attachments.length > 0);
  }
  if (message.role === 'tool-group') {
    return Array.isArray(message.tools) && message.tools.length > 0;
  }
  if (message.role === 'skill-run') {
    return Boolean(message.runId);
  }
  if (message.role === 'model-swap') {
    return String(message.content || '').trim().length > 0;
  }
  return false;
};

// Identity keys for reconciling a locally streamed tool call with the server
// transcript's copy of the same call. The rendered content of the two copies
// (arguments accumulated from deltas vs stored arguments, stream-rebased vs
// history-rebased image URLs) rarely matches byte-for-byte, so content-based
// messageDedupeKey comparison cannot pair them up. Provider call ids are
// shared by both sides when available; the tool name covers entries where
// either side only has a synthetic id (generateId('tool') locally,
// "<msgid>_tool_<n>" from the history converter). Name matches are only
// meaningful because coverage checks are scoped to a single turn.
const toolCallIdentityKeys = (tool) => {
  const keys = [];
  const id = String(tool?.id || '').trim();
  if (id) keys.push(`id:${id}`);
  const name = String(tool?.name || '').trim();
  if (name) keys.push(`name:${name}`);
  return keys;
};

// True when the server's copy of the current turn already records any of the
// local tool-group's calls. The server transcript is authoritative once it
// contains the calls, so the local group must be dropped rather than
// re-appended after the reply. A group the server has no trace of (a stop
// before the transcript flushed) is still preserved by the caller.
const localToolGroupCoveredByServerTurn = (merged, turnStart, turnEnd, localGroup) => {
  const serverToolKeys = new Set();
  for (let i = turnStart; i < turnEnd; i += 1) {
    const message = merged[i];
    if (message?.role !== 'tool-group' || !Array.isArray(message.tools)) continue;
    for (const tool of message.tools) {
      toolCallIdentityKeys(tool).forEach((key) => serverToolKeys.add(key));
    }
  }
  if (serverToolKeys.size === 0) return false;
  const localTools = Array.isArray(localGroup?.tools) ? localGroup.tools : [];
  return localTools.some((tool) => (
    toolCallIdentityKeys(tool).some((key) => serverToolKeys.has(key))
  ));
};

const assistantContentExtensionState = (serverMessage, localMessage) => {
  if (serverMessage?.role !== 'assistant' || localMessage?.role !== 'assistant') return '';
  const serverContent = String(serverMessage.content || '');
  const localContent = String(localMessage.content || '');
  if (!serverContent || !localContent || serverContent === localContent) return '';
  if (localContent.startsWith(serverContent)) return 'local-longer';
  if (serverContent.startsWith(localContent)) return 'server-longer';
  return '';
};

const preserveLocalTailAfterStoppedRefresh = (merged, session) => {
  if (!session || !Array.isArray(session.messages) || session.messages.length === 0 || !Array.isArray(merged)) return;

  let localUserIndex = -1;
  for (let i = session.messages.length - 1; i >= 0; i -= 1) {
    const message = session.messages[i];
    if (message?.role === 'user' && !message.askUser) {
      localUserIndex = i;
      break;
    }
  }
  if (localUserIndex < 0 || localUserIndex >= session.messages.length - 1) return;

  const localUserKey = messageDedupeKey(session.messages[localUserIndex]);
  if (!localUserKey) return;

  let mergedUserIndex = -1;
  for (let i = merged.length - 1; i >= 0; i -= 1) {
    const message = merged[i];
    if (message?.role === 'user' && !message.askUser && messageDedupeKey(message) === localUserKey) {
      mergedUserIndex = i;
      break;
    }
  }
  if (mergedUserIndex < 0) return;

  let turnEnd = merged.length;
  for (let i = mergedUserIndex + 1; i < merged.length; i += 1) {
    const message = merged[i];
    if (message?.role === 'user' && !message.askUser) {
      turnEnd = i;
      break;
    }
  }

  const existingKeys = new Set(merged.map(messageDedupeKey).filter(Boolean));
  const additions = [];
  for (const localMessage of session.messages.slice(localUserIndex + 1)) {
    if (!isPreservableLocalTailMessage(localMessage)) continue;
    const localKey = messageDedupeKey(localMessage);
    if (!localKey || existingKeys.has(localKey)) continue;

    let handledByServerMessage = false;
    for (let i = mergedUserIndex + 1; i < turnEnd; i += 1) {
      const state = assistantContentExtensionState(merged[i], localMessage);
      if (state === 'server-longer') {
        handledByServerMessage = true;
        break;
      }
      if (state === 'local-longer') {
        const serverIdentity = {
          id: merged[i].id,
          created: merged[i].created,
          serverSeq: merged[i].serverSeq,
        };
        merged[i] = { ...merged[i], ...localMessage, ...serverIdentity };
        existingKeys.add(localKey);
        handledByServerMessage = true;
        break;
      }
    }
    if (handledByServerMessage) continue;

    if (localMessage.role === 'tool-group'
      && localToolGroupCoveredByServerTurn(merged, mergedUserIndex + 1, turnEnd, localMessage)) {
      continue;
    }

    // If assistant text diverged rather than extending a stale server prefix,
    // keep the local bubble as a separate tail entry. Stop must never delete
    // text the user already saw, even when reconciliation is ambiguous.
    additions.push({ ...localMessage });
    existingKeys.add(localKey);
  }

  if (additions.length > 0) {
    merged.splice(turnEnd, 0, ...additions);
  }
};

const mergeServerMessagesWithLocalState = (session, serverMessages) => {
  if (!session || !Array.isArray(serverMessages)) return;

  const syntheticAskUserMessages = session.messages
    .filter((message) => message?.askUser && message.role === 'user' && message.content)
    .map((message) => ({ ...message }));

  const preservedCompactionScrollback = localCompactionScrollbackPrefix(session, serverMessages);
  const merged = [
    ...preservedCompactionScrollback,
    ...serverMessages.map((message) => ({ ...message }))
  ];
  preserveLocalTailAfterStoppedRefresh(merged, session);
  if (syntheticAskUserMessages.length > 0) {
    const insertAfter = (() => {
      for (let i = merged.length - 1; i >= 0; i -= 1) {
        if (merged[i].role === 'tool-group') return i + 1;
      }
      for (let i = merged.length - 1; i >= 0; i -= 1) {
        if (merged[i].role === 'user') return i + 1;
      }
      return merged.length;
    })();
    merged.splice(insertAfter, 0, ...syntheticAskUserMessages.filter((message) => !merged.some((existing) => existing.askUser && existing.content === message.content)));
  }

  if (serverMessages.lastResponseId) {
    session.lastResponseId = String(serverMessages.lastResponseId);
  }
  session.messages = merged;
  delete session._serverOnly;
  if (merged.length > 0 && (!session.title || session.title === 'New chat')) {
    const firstUserMsg = merged.find((message) => message.role === 'user' && !message.askUser);
    if (firstUserMsg?.content) {
      session.title = truncate(firstUserMsg.content, 60);
    }
  }

  // Derive lastMessageAt from the newest visible (user/assistant) message so the
  // sidebar's relative time and ordering stay current even if the /v1/sessions/status
  // poll hasn't fired yet after a stale-run reconciliation.
  let newestVisible = 0;
  for (const message of merged) {
    if (message.role !== 'user' && message.role !== 'assistant') continue;
    const created = Number(message.created);
    if (Number.isFinite(created) && created > newestVisible) {
      newestVisible = created;
    }
  }
  if (newestVisible > 0 && newestVisible > (Number(session.lastMessageAt) || 0)) {
    session.lastMessageAt = newestVisible;
  }
};

const numericTranscriptUpdatedAt = (value) => {
  const n = Number(value);
  return Number.isFinite(n) && n > 0 ? n : 0;
};

const recordTranscriptVersionsFromStatus = (statusSessions) => {
  if (!Array.isArray(statusSessions)) return;
  const localById = new Map(state.sessions.map((session) => [session.id, session]));
  const activeId = String(state.activeSessionId || '').trim();
  for (const entry of statusSessions) {
    const local = localById.get(entry?.id) || null;
    if (!local || local.id === activeId) continue;
    const transcriptUpdatedAt = numericTranscriptUpdatedAt(entry.transcript_updated_at);
    if (transcriptUpdatedAt > 0) {
      local.transcriptUpdatedAt = transcriptUpdatedAt;
    }
  }
};

const canRefreshActiveSessionMessages = (session, options = {}) => {
  if (!session || session.id !== state.activeSessionId) return false;
  if (document.visibilityState === 'hidden') return false;
  if (state.draftSessionActive) return false;
  if (state.abortController || state.streaming) return false;
  if (session.activeResponseId) return false;
  if (state.currentStreamSessionId === session.id && state.currentStreamResponseId) return false;
  const progress = state.sessionProgressById?.[session.id] || null;
  const onlyServerRunIsActive = Boolean(
    options.allowServerActiveRun
    && progress?.serverActiveRun
    && !progress?.optimisticBusy
  );
  if (sessionHasInProgressState(session) && !onlyServerRunIsActive) return false;
  const history = ensureSessionHistory(session);
  if (!history || history.loadingOlder) return false;
  return true;
};

let activeMessagesRefreshPromise = null;
let activeMessagesRefreshSessionId = '';

const refreshActiveSessionMessagesFromServer = async (session, options = {}) => {
  if (!session || session.id !== state.activeSessionId) return false;

  if (activeMessagesRefreshPromise && activeMessagesRefreshSessionId === session.id) {
    return activeMessagesRefreshPromise;
  }

  const history = ensureSessionHistory(session);
  if (!history) return false;

  const targetTranscriptUpdatedAt = numericTranscriptUpdatedAt(options.transcriptUpdatedAt || session.transcriptUpdatedAt);
  if (options.force !== true && targetTranscriptUpdatedAt > 0 && history.tailTranscriptUpdatedAt >= targetTranscriptUpdatedAt) {
    if (targetTranscriptUpdatedAt > numericTranscriptUpdatedAt(session.transcriptUpdatedAt)) {
      session.transcriptUpdatedAt = targetTranscriptUpdatedAt;
    }
    return false;
  }

  if (!canRefreshActiveSessionMessages(session, options)) return false;

  const refreshSessionId = session.id;
  const refreshPromise = (async () => {
    history.refreshingTail = true;
    try {
      const page = await fetchServerSessionMessagesPage(refreshSessionId, {
        tail: true,
        limit: SESSION_MESSAGES_PAGE_SIZE,
        etag: options.useEtag === false ? '' : history.tailEtag
      });

      if (page === false) {
        if (session.id === state.activeSessionId && targetTranscriptUpdatedAt > 0) {
          history.tailTranscriptUpdatedAt = Math.max(history.tailTranscriptUpdatedAt || 0, targetTranscriptUpdatedAt);
          if (targetTranscriptUpdatedAt > numericTranscriptUpdatedAt(session.transcriptUpdatedAt)) {
            session.transcriptUpdatedAt = targetTranscriptUpdatedAt;
          }
        }
        return true;
      }
      if (!page) return false;

      if (!canRefreshActiveSessionMessages(session, options) || session.id !== state.activeSessionId) {
        return false;
      }

      const serverMessages = applyTailMessagesToSessionHistory(refreshSessionId, page.data);
      if (!Array.isArray(serverMessages)) return false;

      mergeServerMessagesWithLocalState(session, serverMessages);
      if (page.etag) history.tailEtag = page.etag;
      if (targetTranscriptUpdatedAt > 0) {
        history.tailTranscriptUpdatedAt = Math.max(history.tailTranscriptUpdatedAt || 0, targetTranscriptUpdatedAt);
        if (targetTranscriptUpdatedAt > numericTranscriptUpdatedAt(session.transcriptUpdatedAt)) {
          session.transcriptUpdatedAt = targetTranscriptUpdatedAt;
        }
      }
      persistAndRefreshShell();
      if (options.render !== false && session.id === state.activeSessionId) {
        renderMessages(options.forceScroll === true);
      }
      return true;
    } catch {
      return false;
    } finally {
      history.refreshingTail = false;
      if (activeMessagesRefreshPromise === refreshPromise) {
        activeMessagesRefreshPromise = null;
        activeMessagesRefreshSessionId = '';
      }
    }
  })();

  activeMessagesRefreshPromise = refreshPromise;
  activeMessagesRefreshSessionId = refreshSessionId;
  return refreshPromise;
};

const attachExternallyActiveResponseAfterCatchUp = async (session, refreshed) => {
  if (!refreshed || !session || session.id !== state.activeSessionId) return false;
  if (numericTranscriptUpdatedAt(session._pendingTranscriptUpdatedAt) > numericTranscriptUpdatedAt(session.transcriptUpdatedAt)) {
    return false;
  }
  if (document.visibilityState === 'hidden' || state.draftSessionActive) return false;
  if (state.abortController || state.streaming || session.activeResponseId) return false;
  const progress = state.sessionProgressById?.[session.id] || null;
  if (!progress?.serverActiveRun || progress?.optimisticBusy) return false;
  await syncActiveSessionFromServer(session, true, { skipMessagesFetch: true });
  return true;
};

const clearReconciledActiveTranscriptCatchUp = (session) => {
  if (!session) return false;
  const currentTranscriptUpdatedAt = numericTranscriptUpdatedAt(session.transcriptUpdatedAt);
  const pendingTranscriptUpdatedAt = numericTranscriptUpdatedAt(session._pendingTranscriptUpdatedAt);
  if (pendingTranscriptUpdatedAt > currentTranscriptUpdatedAt) return false;
  if (pendingTranscriptUpdatedAt > 0) delete session._pendingTranscriptUpdatedAt;
  return clearActiveTranscriptCatchUpWarning(session.id, currentTranscriptUpdatedAt);
};

const reconcilePendingActiveTranscriptRefresh = async () => {
  const active = getActiveSession();
  if (!active) return false;
  const pendingTranscriptUpdatedAt = numericTranscriptUpdatedAt(active._pendingTranscriptUpdatedAt);
  if (pendingTranscriptUpdatedAt <= 0) return false;
  const currentTranscriptUpdatedAt = numericTranscriptUpdatedAt(active.transcriptUpdatedAt);
  if (pendingTranscriptUpdatedAt <= currentTranscriptUpdatedAt) {
    clearReconciledActiveTranscriptCatchUp(active);
    return false;
  }

  const progress = state.sessionProgressById?.[active.id] || null;
  const refreshed = await refreshActiveSessionMessagesFromServer(active, {
    transcriptUpdatedAt: pendingTranscriptUpdatedAt,
    forceScroll: false,
    allowServerActiveRun: Boolean(progress?.serverActiveRun)
  });
  clearReconciledActiveTranscriptCatchUp(active);
  await attachExternallyActiveResponseAfterCatchUp(active, refreshed);
  return refreshed;
};

const reconcileActiveTranscriptFromStatus = async (statusSessions) => {
  if (!Array.isArray(statusSessions)) return false;
  const active = getActiveSession();
  if (!active) return false;
  const entry = statusSessions.find((item) => item?.id === active.id) || null;
  if (!entry) return false;

  const incomingTranscriptUpdatedAt = numericTranscriptUpdatedAt(entry.transcript_updated_at);
  if (incomingTranscriptUpdatedAt <= 0) return false;

  const currentTranscriptUpdatedAt = numericTranscriptUpdatedAt(active.transcriptUpdatedAt);
  if (incomingTranscriptUpdatedAt <= currentTranscriptUpdatedAt) {
    clearReconciledActiveTranscriptCatchUp(active);
    return false;
  }

  active._pendingTranscriptUpdatedAt = Math.max(
    numericTranscriptUpdatedAt(active._pendingTranscriptUpdatedAt),
    incomingTranscriptUpdatedAt
  );
  const refreshed = await refreshActiveSessionMessagesFromServer(active, {
    transcriptUpdatedAt: incomingTranscriptUpdatedAt,
    forceScroll: false,
    // A foreground tab can discover that another tab has already advanced the
    // transcript and started the next response. Catch up the committed tail
    // before attaching that response's event stream; the stream snapshot only
    // reconstructs its own turn and cannot fill gaps from earlier turns.
    allowServerActiveRun: Boolean(entry.active_run)
  });
  clearReconciledActiveTranscriptCatchUp(active);
  await attachExternallyActiveResponseAfterCatchUp(active, refreshed);
  return refreshed;
};

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
  if (!session) return SESSION_STATE_RETRY_RESULT;

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

  const loadResult = await loadServerSessionState(requestSessionId);
  if (loadResult.kind === 'auth') {
    stopSessionStatePoll();
    return loadResult;
  }
  if (loadResult.kind !== 'ok') return SESSION_STATE_RETRY_RESULT;
  const runtimeState = loadResult.state;
  const belongsToSelectedSession = requestSessionId === String(state.activeSessionId || '').trim() && !state.draftSessionActive;
  if (belongsToSelectedSession && !selectedResponseApplies()) return loadResult;
  if (selectedResponseApplies()) {
    state.lastAppliedSessionStateRequestGeneration = requestGeneration;
    app.applyCurrentPlanState?.(requestSessionId, runtimeState);
  }

  let sessionChanged = false;
  if (applyMCPStateToSession(session, runtimeState)) {
    sessionChanged = true;
  }
  if (applyGoalStateToSession(session, runtimeState)) {
    sessionChanged = true;
    if (session.id === state.activeSessionId) updateGoalChip(session);
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

  if (prompt && prompt.call_id && Array.isArray(prompt.questions) && prompt.questions.length > 0) {
    const samePrompt = state.askUser
      && state.askUser.sessionId === session.id
      && state.askUser.callId === prompt.call_id;
    if (!samePrompt) {
      openAskUserModal(session.id, prompt.call_id, prompt.questions);
    }
  } else if (state.askUser?.sessionId === session.id) {
    closeAskUserModal();
  }

  const pendingApproval = runtimeState.pending_approval || null;
  if (pendingApproval && pendingApproval.approval_id && Array.isArray(pendingApproval.options) && pendingApproval.options.length > 0) {
    const sameApproval = state.approval
      && state.approval.sessionId === session.id
      && state.approval.approvalId === pendingApproval.approval_id;
    if (!sameApproval) {
      openApprovalModal(session.id, pendingApproval.approval_id, pendingApproval.path,
        pendingApproval.is_shell, pendingApproval.title, pendingApproval.options);
    }
  } else if (state.approval?.sessionId === session.id) {
    closeApprovalModal();
  }

  const pendingInterjection = runtimeState.pending_interjection || null;
  const pendingInterjectionText = pendingInterjection ? String(pendingInterjection.text || '').trim() : '';
  if (pendingInterjectionText) {
    // Prefer the stable server-issued interjection id so the committed
    // response.interjection event (which carries the same id) can clear the
    // pending banner by id. Only fall back to a synthetic id when the server
    // omits one.
    const serverInterjectionId = pendingInterjection ? String(pendingInterjection.id || '').trim() : '';
    const pendingInterjectionId = serverInterjectionId
      || `msg_pending_${session.id}_${pendingInterjectionText.length}`;
    const exists = state.pendingInterjections.some(entry =>
      entry.sessionId === session.id
      && (entry.messageId === pendingInterjectionId || entry.prompt === pendingInterjectionText));
    if (!exists) {
      trackPendingInterjection(session.id, pendingInterjectionText, pendingInterjectionId, 'interject');
      trackPendingInterruptCommit(session.id, pendingInterjectionText, pendingInterjectionId);
    }
  } else {
    for (const entry of [...state.pendingInterjections]) {
      if (entry.sessionId === session.id) {
        removePendingInterjectionById(entry.messageId);
      }
    }
  }

  const activeResponseId = String(runtimeState.active_response_id || '').trim();
  const activeRun = Boolean(runtimeState.active_run);
  setSessionServerActiveRun(session, activeRun || Boolean(activeResponseId));
  const updateBusySidebar = () => {
    if (sessionHasInProgressState(session) !== busyBefore) {
      renderSidebar();
    }
    refreshSidebarStatusPoll();
  };

  if (activeResponseId) {
    const responseChanged = session.activeResponseId !== activeResponseId;
    const recoverFromSnapshot = shouldRecoverActiveResponseFromSnapshot(session, activeResponseId, responseChanged);
    setActiveResponseTracking(session, activeResponseId, responseChanged ? 0 : session.lastSequenceNumber);
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

  if (!activeRun && !state.abortController) {
    if (isStillActive()) stopSessionStatePoll();
    if (session.activeResponseId || (isStillActive() && state.currentStreamResponseId)) {
      clearActiveResponseTracking(session, session.activeResponseId || state.currentStreamResponseId);
      saveSessions();
    }
    setSessionOptimisticBusy(session, false);
    setSessionServerActiveRun(session, false);
    updateBusySidebar();
    if (isStillActive()) setStreaming(false);
    if (isStillActive() && !skipMessagesFetch) {
      await refreshActiveSessionMessagesFromServer(session, {
        transcriptUpdatedAt: session.transcriptUpdatedAt,
        forceScroll: true
      });
    }
    const lastError = String(runtimeState.last_error || '').trim();
    let currentTurnStart = 0;
    for (let i = session.messages.length - 1; i >= 0; i -= 1) {
      if (session.messages[i]?.role === 'user' && !session.messages[i]?.askUser) {
        currentTurnStart = i + 1;
        break;
      }
    }
    const alreadyHasLastError = Boolean(lastError) && session.messages.slice(currentTurnStart).some((message) => (
      message?.role === 'error' && String(message.content || '').trim() === lastError
    ));
    if (lastError && !alreadyHasLastError) {
      addErrorMessage(lastError, session);
      persistAndRefreshShell();
      if (isStillActive()) {
        scrollToBottom(true);
      }
    }
    if (isStillActive()) {
      setConnectionState('', '');
      setStreaming(false);
    }
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
  const transcriptUpdatedAt = numericTranscriptUpdatedAt(serverSession.transcript_updated_at);
  if (transcriptUpdatedAt > 0) {
    target.transcriptUpdatedAt = transcriptUpdatedAt;
  }
  target.number = Number(serverSession.number || target.number || 0);
  if (serverSession.provider) {
    target.provider = serverSession.provider;
  }
  if (serverSession.worktree_dir !== undefined) {
    target.worktreeDir = String(serverSession.worktree_dir || '');
    target.worktreeName = target.worktreeDir ? target.worktreeDir.split(/[\\/]/).filter(Boolean).pop() || 'worktree' : '';
  }
  if (Object.prototype.hasOwnProperty.call(serverSession, 'goal')) {
    target.goal = serverSession.goal && typeof serverSession.goal === 'object' ? { ...serverSession.goal } : null;
  }
  return target;
};

const reconcileServerSessionIdentity = (session, serverSession) => {
  if (!session || !serverSession) return session;

  const nextId = String(serverSession.id || '').trim();
  const previousId = String(session.id || '').trim();
  if (!nextId || nextId === previousId) return session;

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
    const query = params.toString();
    const resp = await fetch(`${UI_PREFIX}/v1/sessions${query ? `?${query}` : ''}`, {
      headers: requestHeaders('')
    });
    if (!resp.ok) return;
    const data = await resp.json();
    if (!Array.isArray(data.sessions)) return;

    const localById = new Map(state.sessions.map(s => [s.id, s]));
    const localByNumber = new Map(
      state.sessions
        .filter(s => Number(s.number) > 0 && /^\d+$/.test(s.id))
        .map(s => [Number(s.number), s])
    );

    for (const serverSession of data.sessions) {
      const sNum = Number(serverSession.number || 0);
      let local = localById.get(serverSession.id) ||
        (sNum > 0 ? localByNumber.get(sNum) : null) ||
        null;
      if (local) {
        reconcileServerSessionIdentity(local, serverSession);
        applyServerSessionSummary(local, serverSession);
        continue;
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
        messages: [],
        lastResponseId: null,
        activeResponseId: null,
        lastSequenceNumber: 0,
        messageCount: 0,
        _serverOnly: true
      }, serverSession);
      state.sessions.push(local);
    }

    persistAndRefreshShell();
  } catch {
    // Gracefully fall back to in-memory-only
  }
};

const updateSessionMetadata = async (session, patch) => {
  const resp = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(session.id)}`, {
    method: 'PATCH',
    headers: requestHeaders(session.id),
    body: JSON.stringify(patch)
  });
  if (!resp.ok) {
    throw await normalizeError(resp);
  }
  return resp.json().catch(() => ({}));
};

const refineSessionTitle = async (session, options = {}) => {
  if (!session?.id || session._refiningTitle) return null;
  session._refiningTitle = true;
  renderSidebar();
  try {
    const resp = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(session.id)}/title/refine`, {
      method: 'POST',
      headers: requestHeaders(session.id),
      body: JSON.stringify({ preview: Boolean(options.preview) })
    });
    if (!resp.ok) {
      throw await normalizeError(resp);
    }
    const payload = await resp.json().catch(() => ({}));
    if (!options.preview) {
      reconcileServerSessionIdentity(session, payload);
      applyServerSessionSummary(session, payload);
      session.name = String(payload.name || '').trim();
      persistAndRefreshShell();
    }
    return payload;
  } finally {
    session._refiningTitle = false;
    renderSidebar();
  }
};

const setRenameGeneratedMode = (enabled) => {
  state.renameGeneratedMode = Boolean(enabled);
  elements.renameSessionNameField.classList.toggle('hidden', state.renameGeneratedMode);
  elements.renameGeneratedFields.classList.toggle('hidden', !state.renameGeneratedMode);
  elements.renameImproveTitleBtn.textContent = state.renameGeneratedMode ? 'Try again with AI' : 'Improve title with AI';
  elements.renameSessionIntro.textContent = state.renameGeneratedMode
    ? 'Review the AI suggestion before saving it as this session title.'
    : 'Choose the label shown in the sidebar, or let AI suggest a better title from this session.';
  elements.renameSessionInput.tabIndex = state.renameGeneratedMode ? -1 : 0;
  elements.renameGeneratedTitleInput.tabIndex = state.renameGeneratedMode ? 0 : -1;
  elements.renameGeneratedDetailInput.tabIndex = state.renameGeneratedMode ? 0 : -1;
};

const openRenameSessionModal = (session) => {
  if (!session?.id) return false;
  state.renameSessionId = session.id;
  setRenameGeneratedMode(false);
  elements.renameSessionInput.value = String(session.name || '').trim();
  elements.renameSessionInput.placeholder = String(session.title || 'Project kickoff notes').trim() || 'Project kickoff notes';
  elements.renameGeneratedTitleInput.value = String(session.generatedShortTitle || session.title || '').trim();
  elements.renameGeneratedDetailInput.value = String(session.generatedLongTitle || session.longTitle || '').trim();
  elements.renameSessionError.textContent = '';
  elements.renameImproveTitleBtn.disabled = false;
  elements.renameImproveTitleBtn.classList.remove('is-loading');
  elements.renameSessionSaveBtn.disabled = false;
  elements.renameSessionCancelBtn.disabled = false;
  elements.renameSessionSaveBtn.textContent = 'Save';
  elements.renameSessionModal.classList.remove('hidden');
  elements.renameSessionInput.removeAttribute('tabindex');
  window.setTimeout(() => {
    elements.renameSessionInput.focus();
    elements.renameSessionInput.select();
  }, 0);
  return true;
};

const closeRenameSessionModal = () => {
  state.renameSessionId = '';
  state.renameGeneratedMode = false;
  elements.renameSessionModal.classList.add('hidden');
  elements.renameSessionError.textContent = '';
  elements.renameSessionInput.value = '';
  elements.renameGeneratedTitleInput.value = '';
  elements.renameGeneratedDetailInput.value = '';
  elements.renameSessionInput.placeholder = 'Project kickoff notes';
  elements.renameSessionInput.setAttribute('tabindex', '-1');
  elements.renameGeneratedTitleInput.setAttribute('tabindex', '-1');
  elements.renameGeneratedDetailInput.setAttribute('tabindex', '-1');
  elements.renameImproveTitleBtn.disabled = false;
  elements.renameImproveTitleBtn.classList.remove('is-loading');
  elements.renameImproveTitleBtn.textContent = 'Improve title with AI';
  elements.renameSessionSaveBtn.disabled = false;
  elements.renameSessionCancelBtn.disabled = false;
  elements.renameSessionSaveBtn.textContent = 'Save';
};

const improveRenameTitleSuggestion = async () => {
  const sessionId = String(state.renameSessionId || '').trim();
  if (!sessionId || elements.renameImproveTitleBtn.disabled) return false;
  const session = state.sessions.find((item) => item.id === sessionId);
  if (!session) return false;
  elements.renameSessionError.textContent = '';
  if (!state.renameGeneratedMode) {
    elements.renameGeneratedTitleInput.value = String(session.generatedShortTitle || session.title || '').trim();
    elements.renameGeneratedDetailInput.value = String(session.generatedLongTitle || session.longTitle || '').trim();
    setRenameGeneratedMode(true);
  }
  elements.renameImproveTitleBtn.disabled = true;
  elements.renameImproveTitleBtn.classList.add('is-loading');
  elements.renameImproveTitleBtn.textContent = 'Improving title…';
  try {
    const payload = await refineSessionTitle(session, { preview: true });
    if (!payload) return false;
    elements.renameGeneratedTitleInput.value = String(payload.generated_short_title || payload.short_title || session.title || '').trim();
    elements.renameGeneratedDetailInput.value = String(payload.generated_long_title || payload.long_title || session.longTitle || '').trim();
    setRenameGeneratedMode(true);
    window.setTimeout(() => {
      elements.renameGeneratedTitleInput.focus();
      elements.renameGeneratedTitleInput.select();
    }, 0);
    return true;
  } catch (err) {
    if (err?.status === 401) {
      closeRenameSessionModal();
      handleAuthFailure();
      return false;
    }
    elements.renameSessionError.textContent = err?.message || 'Failed to improve title.';
    return false;
  } finally {
    elements.renameImproveTitleBtn.disabled = false;
    elements.renameImproveTitleBtn.classList.remove('is-loading');
    elements.renameImproveTitleBtn.textContent = state.renameGeneratedMode ? 'Try again with AI' : 'Improve title with AI';
  }
};

const submitRenameSessionModal = async () => {
  const sessionId = String(state.renameSessionId || '').trim();
  if (!sessionId) {
    closeRenameSessionModal();
    return false;
  }
  const session = state.sessions.find((item) => item.id === sessionId);
  if (!session) {
    closeRenameSessionModal();
    return false;
  }
  if (elements.renameSessionSaveBtn.disabled) {
    return false;
  }

  const patch = state.renameGeneratedMode
    ? {
      name: '',
      generated_short_title: elements.renameGeneratedTitleInput.value.trim(),
      generated_long_title: elements.renameGeneratedDetailInput.value.trim()
    }
    : { name: elements.renameSessionInput.value.trim() };
  elements.renameSessionError.textContent = '';
  elements.renameSessionSaveBtn.disabled = true;
  elements.renameSessionCancelBtn.disabled = true;
  elements.renameSessionSaveBtn.textContent = 'Saving…';
  try {
    const payload = await updateSessionMetadata(session, patch);
    reconcileServerSessionIdentity(session, payload);
    applyServerSessionSummary(session, payload);
    session.name = String(payload.name || '').trim();
    persistAndRefreshShell();
    closeRenameSessionModal();
    return true;
  } catch (err) {
    if (err?.status === 401) {
      closeRenameSessionModal();
      handleAuthFailure();
      return false;
    }
    elements.renameSessionError.textContent = err?.message || 'Failed to rename session.';
    elements.renameSessionSaveBtn.disabled = false;
    elements.renameSessionCancelBtn.disabled = false;
    elements.renameSessionSaveBtn.textContent = 'Save';
    return false;
  }
};

const promptRenameSession = async (session) => openRenameSessionModal(session);

const SESSION_HIDE_ANIMATION_MS = 220;

const animateSessionHide = async (sessionId) => {
  const id = String(sessionId || '').trim();
  if (!id) return;
  if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;

  const selector = `.session-row[data-session-id="${CSS.escape(id)}"]`;
  const row = elements.sessionGroups.querySelector(selector);
  if (!row || row.classList.contains('is-hiding')) return;

  const height = row.getBoundingClientRect().height;
  if (!height) return;

  row.style.height = `${height}px`;
  row.style.pointerEvents = 'none';
  row.getBoundingClientRect();

  await new Promise((resolve) => {
    let done = false;
    const finish = () => {
      if (done) return;
      done = true;
      row.style.height = '';
      row.style.pointerEvents = '';
      resolve();
    };

    row.addEventListener('transitionend', (event) => {
      if (event.target === row && event.propertyName === 'height') {
        finish();
      }
    }, { once: true });

    window.requestAnimationFrame(() => {
      row.classList.add('is-hiding');
      row.style.height = '0px';
    });

    window.setTimeout(finish, SESSION_HIDE_ANIMATION_MS + 80);
  });
};

const setSessionArchived = async (session, archived) => {
  if (!session?.id) return false;
  try {
    const wasActive = session.id === state.activeSessionId;
    const previousId = session.id;
    const payload = await updateSessionMetadata(session, { archived });
    reconcileServerSessionIdentity(session, payload);
    applyServerSessionSummary(session, payload);
    if (archived && !state.showHiddenSessions) {
      await animateSessionHide(previousId);
      if (session.id !== previousId) await animateSessionHide(session.id);
      if (wasActive || session.id === state.activeSessionId) {
        await switchToDraftSession({ closeSidebar: false, clearPreviousComposerDraft: true });
      }
    }
    persistAndRefreshShell();
    return true;
  } catch (err) {
    if (err?.status === 401) {
      handleAuthFailure();
      return false;
    }
    window.alert(err?.message || 'Failed to update session visibility.');
    return false;
  }
};

const setSessionPinned = async (session, pinned) => {
  if (!session?.id) return false;
  try {
    const payload = await updateSessionMetadata(session, { pinned });
    reconcileServerSessionIdentity(session, payload);
    applyServerSessionSummary(session, payload);
    persistAndRefreshShell();
    return true;
  } catch (err) {
    if (err?.status === 401) {
      handleAuthFailure();
      return false;
    }
    window.alert(err?.message || 'Failed to update session pin.');
    return false;
  }
};

// ===== Initialization =====
const hydrateActiveSessionAfterStartup = async () => {
  const active = getActiveSession();
  if (!active) return;

  // Start state sync immediately so the server round-trip overlaps with the
  // messages fetch instead of serialising after it. For server-only sessions,
  // the explicit message preload below owns the message fetch to avoid a double
  // request.
  const statePromise = syncActiveSessionFromServer(active, true, { skipMessagesFetch: Boolean(active._serverOnly) });

  const preloadMessagesPromise = active._serverOnly
    ? loadServerSessionMessages(active.id)
    : null;

  if (preloadMessagesPromise) {
    const msgs = await preloadMessagesPromise;
    if (Array.isArray(msgs)) {
      mergeServerMessagesWithLocalState(active, msgs);
      saveSessions();
      renderSidebar();
      renderMessages(true);
    }
  }

  await statePromise;
  await app.refreshSkillCommands?.(active.id);
  if (syncSelectedRuntimeFromSession(active)) {
    app.updateHeader();
  }
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
        messages: [],
        lastResponseId: null,
        activeResponseId: null,
        lastSequenceNumber: 0,
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
  app.renderWidgetSidebar?.();
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

    const sessionsPromise = mergeServerSessions();

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
    state.connected = true;
    renderModelOptions();
    app.updateHeader?.();
    setConnectionState('', '');
    startSidebarStatusPoll();
    void refreshWidgetsSidebar();
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

    hideStartupSplash();
    await hydrateActiveSessionAfterStartup().catch(() => {});
  } catch (err) {
    const message = err?.message || 'Unable to validate token.';
    setStartupStatus(message);
    setConnectionState(message, 'bad');
    if (!state.token || err?.status === 401) {
      handleAuthFailure();
    }
  } finally {
    hideStartupSplash();
  }
};

// ===== Composer add menu / MCP controls =====
let mcpModalSessionId = '';
let mcpModalPending = false;
let mcpModalPendingEnabled = null;
let mcpModalErrorMessage = '';

const normalizeMCPEnabledNames = (names) => {
  const enabled = [];
  const seen = new Set();
  (Array.isArray(names) ? names : []).forEach((raw) => {
    const name = String(raw || '').trim();
    if (!name || seen.has(name)) return;
    seen.add(name);
    enabled.push(name);
  });
  return enabled;
};

const setAddMenuOpen = (open) => {
  if (!elements.addMenu || !elements.attachBtn) return;
  setElementHidden(elements.addMenu, !open);
  elements.attachBtn.setAttribute('aria-expanded', open ? 'true' : 'false');
};

const toggleAddMenu = () => {
  const open = elements.addMenu ? elements.addMenu.hidden : true;
  setAddMenuOpen(open);
};

const closeAddMenu = () => setAddMenuOpen(false);

const sessionIsBusyForMCP = (session) => Boolean(
  session && (
    sessionHasInProgressState(session)
    || session.activeResponseId
    || (state.streaming && (!state.currentStreamSessionId || state.currentStreamSessionId === session.id))
  )
);

const ensureActiveSessionForMCP = () => {
  let session = getActiveSession();
  if (session && !state.draftSessionActive) {
    return session;
  }
  session = createSession();
  state.sessions.unshift(session);
  state.activeSessionId = session.id;
  state.draftSessionActive = false;
  updateURL(sessionSlug(session));
  persistAndRefreshShell();
  renderMessages(true);
  app.activateDiffSidebar?.(session.id);
  return session;
};

const mcpHeaders = () => requestHeaders ? requestHeaders('') : { 'Content-Type': 'application/json' };

const fetchSessionMCP = async (sessionId) => {
  const resp = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(sessionId)}/mcp`, {
    headers: mcpHeaders(),
  });
  if (!resp.ok) throw await normalizeError(resp);
  const data = await resp.json().catch(() => ({ servers: [], enabled: [] }));
  const session = state.sessions.find((item) => item.id === sessionId) || null;
  if (session) {
    applyMCPStateToSession(session, data);
    saveSessions();
    app.updateHeader?.();
  }
  return data;
};

const closeMCPModal = () => {
  if (!elements.mcpModal) return;
  elements.mcpModal.classList.add('hidden');
  mcpModalSessionId = '';
  mcpModalPending = false;
  mcpModalPendingEnabled = null;
  mcpModalErrorMessage = '';
  if (elements.mcpError) {
    elements.mcpError.textContent = '';
    elements.mcpError.classList.remove('is-muted');
  }
};

const renderMCPModal = (session, { loading = false } = {}) => {
  if (!elements.mcpModalBody) return;
  const busy = sessionIsBusyForMCP(session);
  const disabled = loading || mcpModalPending || busy;
  elements.mcpModalBody.innerHTML = '';

  if (loading) {
    const row = document.createElement('div');
    row.className = 'mcp-server-loading';
    row.textContent = 'Loading configured MCP servers…';
    elements.mcpModalBody.appendChild(row);
  } else if (!Array.isArray(session?.mcpServers) || session.mcpServers.length === 0) {
    const row = document.createElement('div');
    row.className = 'mcp-server-empty';
    row.textContent = 'No MCP servers are configured yet. Add servers to ~/.config/term-llm/mcp.json, then reopen this panel.';
    elements.mcpModalBody.appendChild(row);
  } else {
    const enabledSet = new Set(
      mcpModalPending && Array.isArray(mcpModalPendingEnabled)
        ? mcpModalPendingEnabled
        : normalizeMCPEnabledNames(session.mcpEnabled || [])
    );
    session.mcpServers.forEach((server) => {
      const row = document.createElement('label');
      row.className = 'mcp-server-row';

      const icon = document.createElement('span');
      icon.className = 'mcp-server-icon';
      icon.setAttribute('aria-hidden', 'true');
      icon.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="5" y="5" width="5" height="5" rx="1.2"/><rect x="14" y="14" width="5" height="5" rx="1.2"/><path d="M10 7.5h2.5a4 4 0 0 1 4 4V14"/><path d="M14 16.5h-2.5a4 4 0 0 1-4-4V10"/></svg>';

      const copy = document.createElement('span');
      copy.className = 'mcp-server-copy';
      const titleRow = document.createElement('span');
      titleRow.className = 'mcp-server-title-row';
      const name = document.createElement('span');
      name.className = 'mcp-server-name';
      name.textContent = server.name;
      const status = document.createElement('span');
      status.className = `mcp-server-status ${String(server.status || '').toLowerCase()}`;
      status.textContent = server.status || 'stopped';
      titleRow.appendChild(name);
      titleRow.appendChild(status);

      const subtitle = document.createElement('span');
      subtitle.className = 'mcp-server-subtitle';
      const toolCount = Number(server.tools || 0);
      if (String(server.status || '').toLowerCase() === 'ready') {
        subtitle.textContent = `${toolCount} tool${toolCount === 1 ? '' : 's'} available`;
      } else if (String(server.status || '').toLowerCase() === 'starting') {
        subtitle.textContent = 'Starting server…';
      } else if (server.error) {
        subtitle.textContent = 'Failed to start';
      } else {
        subtitle.textContent = 'Tools load when enabled';
      }

      copy.appendChild(titleRow);
      copy.appendChild(subtitle);
      if (server.error) {
        const error = document.createElement('span');
        error.className = 'mcp-server-error';
        error.textContent = server.error;
        copy.appendChild(error);
      }

      const switchWrap = document.createElement('span');
      switchWrap.className = 'mcp-switch';
      const input = document.createElement('input');
      input.className = 'mcp-switch-input';
      input.type = 'checkbox';
      input.value = server.name;
      input.checked = enabledSet.has(server.name) || Boolean(server.enabled);
      input.disabled = disabled;
      input.dataset.mcpServer = server.name;
      const track = document.createElement('span');
      track.className = 'mcp-switch-track';
      const thumb = document.createElement('span');
      thumb.className = 'mcp-switch-thumb';
      track.appendChild(thumb);
      switchWrap.appendChild(input);
      switchWrap.appendChild(track);

      row.appendChild(icon);
      row.appendChild(copy);
      row.appendChild(switchWrap);
      elements.mcpModalBody.appendChild(row);
    });
  }

  if (elements.mcpError) {
    const statusMessage = busy && !mcpModalErrorMessage
      ? 'Cannot change MCPs while a response is running.'
      : (mcpModalPending ? 'Saving changes…' : mcpModalErrorMessage);
    elements.mcpError.textContent = statusMessage;
    elements.mcpError.classList.toggle('is-muted', Boolean(mcpModalPending && !mcpModalErrorMessage));
  }
};

const openSessionMCPModal = async () => {
  const session = ensureActiveSessionForMCP();
  if (!session) return null;
  mcpModalSessionId = session.id;
  mcpModalPendingEnabled = null;
  mcpModalErrorMessage = '';
  if (elements.mcpModal) elements.mcpModal.classList.remove('hidden');
  renderMCPModal(session, { loading: true });
  try {
    await fetchSessionMCP(session.id);
    const refreshed = state.sessions.find((item) => item.id === session.id) || session;
    renderMCPModal(refreshed);
    return refreshed;
  } catch (err) {
    if (err?.status === 401) {
      handleAuthFailure();
      return null;
    }
    mcpModalErrorMessage = err?.message || 'Failed to load MCP servers.';
    renderMCPModal(session);
    return null;
  }
};

const selectedMCPNamesFromModal = () => {
  if (!elements.mcpModalBody || typeof elements.mcpModalBody.querySelectorAll !== 'function') return [];
  return Array.from(elements.mcpModalBody.querySelectorAll('input[data-mcp-server]'))
    .filter((input) => input.checked)
    .map((input) => String(input.value || '').trim())
    .filter(Boolean);
};

const applySessionMCP = async (sessionId, enabledNames) => {
  const session = state.sessions.find((item) => item.id === sessionId) || null;
  if (!session || sessionIsBusyForMCP(session)) {
    mcpModalErrorMessage = 'Cannot change MCPs while a response is running.';
    renderMCPModal(session);
    return null;
  }
  const requestedEnabled = normalizeMCPEnabledNames(enabledNames);
  mcpModalPending = true;
  mcpModalPendingEnabled = requestedEnabled;
  mcpModalErrorMessage = '';
  renderMCPModal(session);
  try {
    const resp = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(sessionId)}/mcp`, {
      method: 'PATCH',
      headers: mcpHeaders(),
      body: JSON.stringify({ enabled: requestedEnabled }),
    });
    if (!resp.ok) throw await normalizeError(resp);
    const data = await resp.json().catch(() => ({ servers: [], enabled: [] }));
    applyMCPStateToSession(session, data);
    saveSessions();
    app.updateHeader?.();
    mcpModalPending = false;
    mcpModalPendingEnabled = null;
    renderMCPModal(session);
    return data;
  } catch (err) {
    mcpModalPending = false;
    mcpModalPendingEnabled = null;
    if (err?.status === 401) {
      handleAuthFailure();
      return null;
    }
    mcpModalErrorMessage = err?.status === 409
      ? 'Cannot change MCPs while a response is running.'
      : (err?.message || 'Failed to update MCP servers.');
    renderMCPModal(session);
    return null;
  }
};

// ===== Event listeners =====
elements.newChatBtn.addEventListener('click', createAndSwitchToFreshSession);
elements.sidebarRailNewChatBtn.addEventListener('click', async () => {
  await createAndSwitchToFreshSession();
});

elements.settingsBtn.addEventListener('click', () => {
  openAuthModal('', false);
});
elements.sidebarRailSettingsBtn.addEventListener('click', () => {
  openAuthModal('', false);
});

elements.mobileMenuBtn.addEventListener('click', openSidebar);
elements.sidebarToggleBtn.addEventListener('click', toggleSidebarCollapsed);
elements.sidebarPanelToggleBtn.addEventListener('click', toggleSidebarCollapsed);
elements.sidebarBackdrop.addEventListener('click', closeSidebar);
elements.sidebarCloseBtn.addEventListener('click', closeSidebar);

let lastChatTouchY = null;

elements.chatScroll.addEventListener('wheel', (event) => {
  if (event.deltaY < 0) {
    noteUserScrollIntent();
  }
}, { passive: true });

elements.chatScroll.addEventListener('touchstart', (event) => {
  lastChatTouchY = event.touches && event.touches.length ? event.touches[0].clientY : null;
}, { passive: true });

elements.chatScroll.addEventListener('touchmove', (event) => {
  if (!event.touches || !event.touches.length || lastChatTouchY === null) return;
  const nextY = event.touches[0].clientY;
  if (nextY > lastChatTouchY) {
    noteUserScrollIntent();
  }
  lastChatTouchY = nextY;
}, { passive: true });

elements.chatScroll.addEventListener('scroll', () => {
  noteScrollPositionChanged();
  void maybeLoadOlderSessionMessages();
});

window.addEventListener('keydown', (event) => {
  if (shouldDisableAutoScrollForKey(event)) {
    noteUserScrollIntent();
  }
});

elements.promptInput.addEventListener('input', autoGrowPrompt);
elements.promptInput.addEventListener('keydown', (event) => {
  if (event.key === 'Enter' && !event.shiftKey && !event.isComposing) {
    event.preventDefault();
    sendMessage();
  }
});

elements.sendBtn.addEventListener('click', sendMessage);
if (elements.voiceBtn) {
  elements.voiceBtn.addEventListener('click', () => {
    toggleVoiceRecording();
  });
}
elements.stopBtn.addEventListener('click', async () => {
  if (elements.stopBtn.disabled) return;
  const session = getActiveSession();
  const originalLabel = elements.stopBtn.textContent;
  elements.stopBtn.disabled = true;
  elements.stopBtn.textContent = 'Stopping\u2026';
  try {
    await cancelActiveResponse(session);
  } catch (err) {
    if (err?.status === 401) {
      handleAuthFailure();
      return;
    }
    if (state.abortController) {
      state.abortController.abort();
    }
  } finally {
    elements.stopBtn.disabled = false;
    elements.stopBtn.textContent = originalLabel || 'Stop';
  }
});

const openGoalModal = () => {
  const session = ensureActiveSession?.();
  if (!session || !elements.goalModal) return;
  const goal = session.goal || null;
  elements.goalObjectiveInput.value = goal?.objective || '';
  elements.goalTokenBudgetInput.value = goal?.token_budget ? String(goal.token_budget) : '';
  elements.goalError.textContent = '';
  const exists = Boolean(goal && goal.objective);
  const status = String(goal?.status || '').trim() || 'active';
  elements.goalSaveBtn.textContent = exists ? 'Save goal' : 'Set goal';
  elements.goalPauseBtn.hidden = !exists || status !== 'active';
  elements.goalResumeBtn.hidden = !exists || status === 'active' || status === 'complete';
  elements.goalClearBtn.hidden = !exists;
  elements.goalModal.classList.remove('hidden');
  elements.goalObjectiveInput.focus();
};

const closeGoalModal = () => {
  if (elements.goalModal) elements.goalModal.classList.add('hidden');
};

const postSessionGoal = async (action, extra = {}) => {
  const session = ensureActiveSession?.();
  if (!session) return null;
  const response = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(session.id)}/runtime/goal`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ action, ...extra })
  });
  const data = await response.json().catch(() => ({}));
  if (!response.ok) {
    const message = data?.error?.message || data?.error || `Goal update failed (${response.status})`;
    throw new Error(message);
  }
  session.goal = data.goal || null;
  saveSessions();
  updateGoalChip(session);
  renderSidebar();
  return data.goal || null;
};

const saveGoalFromModal = async () => {
  if (!elements.goalObjectiveInput) return;
  const objective = String(elements.goalObjectiveInput.value || '').trim();
  if (!objective) {
    elements.goalError.textContent = 'Objective is required.';
    return;
  }
  const rawBudget = String(elements.goalTokenBudgetInput.value || '').trim();
  const payload = { objective };
  if (rawBudget) {
    const budget = Number(rawBudget);
    if (!Number.isFinite(budget) || budget <= 0) {
      elements.goalError.textContent = 'Token budget must be a positive number.';
      return;
    }
    payload.token_budget = Math.floor(budget);
  }
  try {
    const session = ensureActiveSession?.();
    await postSessionGoal(session?.goal ? 'edit' : 'set', payload);
    closeGoalModal();
  } catch (err) {
    elements.goalError.textContent = err?.message || String(err);
  }
};

const mutateGoalFromModal = async (action) => {
  try {
    await postSessionGoal(action);
    if (action === 'clear') closeGoalModal();
    else openGoalModal();
  } catch (err) {
    if (elements.goalError) elements.goalError.textContent = err?.message || String(err);
  }
};

// Composer add menu and file attachment handlers
let locationRequestPending = false;
let locationStatusTimer = null;

const showLocationStatus = (message, { persistent = false } = {}) => {
  if (!elements.locationStatus) return;
  if (locationStatusTimer) clearTimeout(locationStatusTimer);
  elements.locationStatus.textContent = message;
  elements.locationStatus.classList.toggle('hidden', !message);
  if (message && !persistent) {
    locationStatusTimer = setTimeout(() => {
      elements.locationStatus.textContent = '';
      elements.locationStatus.classList.add('hidden');
    }, 5000);
  }
};

const locationErrorMessage = (error) => {
  if (error?.code === 1) return 'Location permission was denied.';
  if (error?.code === 2) return 'Your device could not determine its location.';
  if (error?.code === 3) return 'Location request timed out.';
  return 'Could not get your current location.';
};

const shareCurrentLocation = () => {
  if (locationRequestPending) return;
  if (!window.isSecureContext) {
    showLocationStatus('Location sharing requires HTTPS or localhost.');
    return;
  }
  if (!navigator.geolocation || typeof navigator.geolocation.getCurrentPosition !== 'function') {
    showLocationStatus('Location sharing is not supported in this browser.');
    return;
  }

  locationRequestPending = true;
  elements.addLocationOption.disabled = true;
  showLocationStatus('Getting your current location…', { persistent: true });
  navigator.geolocation.getCurrentPosition((position) => {
    const latitude = Number(position.coords.latitude).toFixed(5);
    const longitude = Number(position.coords.longitude).toFixed(5);
    const accuracy = Math.max(1, Math.round(Number(position.coords.accuracy) || 0));
    const locationText = [
      'My current location:',
      `- Coordinates: ${latitude}, ${longitude}`,
      `- Accuracy: approximately ${accuracy} m`,
      `- Map: https://www.openstreetmap.org/?mlat=${latitude}&mlon=${longitude}#map=16/${latitude}/${longitude}`,
    ].join('\n');
    const existing = elements.promptInput.value.trimEnd();
    elements.promptInput.value = existing ? `${existing}\n\n${locationText}` : locationText;
    autoGrowPrompt();
    elements.promptInput.focus();
    showLocationStatus('Location added to your message. Review it before sending.');
    locationRequestPending = false;
    elements.addLocationOption.disabled = false;
  }, (error) => {
    showLocationStatus(locationErrorMessage(error));
    locationRequestPending = false;
    elements.addLocationOption.disabled = false;
  }, {
    enableHighAccuracy: false,
    timeout: 12000,
    maximumAge: 60000,
  });
};

if (elements.addLocationOption) {
  const locationEnabled = window.TERM_LLM_LOCATION_SHARING_ENABLED !== false;
  elements.addLocationOption.hidden = !locationEnabled;
}

elements.attachBtn.addEventListener('click', (event) => {
  event.preventDefault();
  toggleAddMenu();
});
if (elements.addAttachOption) {
  elements.addAttachOption.addEventListener('click', () => {
    closeAddMenu();
    elements.fileInput.click();
  });
}
if (elements.addLocationOption) {
  elements.addLocationOption.addEventListener('click', () => {
    closeAddMenu();
    shareCurrentLocation();
  });
}
if (elements.addMCPOption) {
  elements.addMCPOption.addEventListener('click', async () => {
    closeAddMenu();
    await openSessionMCPModal();
  });
}
if (elements.addGoalOption) {
  elements.addGoalOption.addEventListener('click', () => {
    closeAddMenu();
    openGoalModal();
  });
}
if (elements.goalChip) {
  elements.goalChip.addEventListener('click', () => {
    openGoalModal();
  });
}
if (elements.goalModalCloseBtn) {
  elements.goalModalCloseBtn.addEventListener('click', closeGoalModal);
}
if (elements.goalSaveBtn) {
  elements.goalSaveBtn.addEventListener('click', () => {
    void saveGoalFromModal();
  });
}
if (elements.goalPauseBtn) {
  elements.goalPauseBtn.addEventListener('click', () => {
    void mutateGoalFromModal('pause');
  });
}
if (elements.goalResumeBtn) {
  elements.goalResumeBtn.addEventListener('click', () => {
    void mutateGoalFromModal('resume');
  });
}
if (elements.goalClearBtn) {
  elements.goalClearBtn.addEventListener('click', () => {
    void mutateGoalFromModal('clear');
  });
}
if (elements.goalModal) {
  elements.goalModal.addEventListener('click', (event) => {
    if (event.target === elements.goalModal) closeGoalModal();
  });
  elements.goalModal.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && !event.defaultPrevented) {
      event.preventDefault();
      closeGoalModal();
      return;
    }
    if ((event.key === 'Enter' || event.key === 'NumpadEnter') && (event.metaKey || event.ctrlKey) && !event.defaultPrevented) {
      event.preventDefault();
      void saveGoalFromModal();
    }
  });
}
if (elements.mcpStatus) {
  elements.mcpStatus.addEventListener('click', async () => {
    await openSessionMCPModal();
  });
}
document.addEventListener('click', (event) => {
  if (!elements.addMenu || elements.addMenu.hidden) return;
  const target = event.target;
  if (target === elements.attachBtn || target === elements.addMenu) return;
  if (typeof elements.attachBtn.contains === 'function' && elements.attachBtn.contains(target)) return;
  if (typeof elements.addMenu.contains === 'function' && elements.addMenu.contains(target)) return;
  closeAddMenu();
});
if (elements.mcpModalCloseBtn) elements.mcpModalCloseBtn.addEventListener('click', closeMCPModal);
if (elements.mcpModalBody) {
  elements.mcpModalBody.addEventListener('change', async (event) => {
    const input = event.target;
    if (!input || !input.dataset || !input.dataset.mcpServer) return;
    if (!mcpModalSessionId || mcpModalPending) return;
    await applySessionMCP(mcpModalSessionId, selectedMCPNamesFromModal());
  });
}
if (elements.mcpModal) {
  elements.mcpModal.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && !event.defaultPrevented) {
      event.preventDefault();
      closeMCPModal();
    }
  });
}
elements.fileInput.addEventListener('change', () => {
  if (elements.fileInput.files.length > 0) {
    handleFiles(elements.fileInput.files);
    elements.fileInput.value = '';
  }
});

// Drag and drop
let dragCounter = 0;
const mainEl = document.querySelector('.main');
mainEl.addEventListener('dragenter', (e) => {
  e.preventDefault();
  dragCounter++;
  elements.dropOverlay.classList.remove('hidden');
});
mainEl.addEventListener('dragleave', (e) => {
  e.preventDefault();
  dragCounter--;
  if (dragCounter <= 0) {
    dragCounter = 0;
    elements.dropOverlay.classList.add('hidden');
  }
});
mainEl.addEventListener('dragover', (e) => {
  e.preventDefault();
});
mainEl.addEventListener('drop', (e) => {
  e.preventDefault();
  dragCounter = 0;
  elements.dropOverlay.classList.add('hidden');
  if (e.dataTransfer.files.length > 0) {
    handleFiles(e.dataTransfer.files);
  }
});

// Paste support
elements.promptInput.addEventListener('paste', (e) => {
  const files = [];
  if (e.clipboardData && e.clipboardData.items) {
    for (const item of e.clipboardData.items) {
      if (item.kind === 'file') {
        const file = item.getAsFile();
        if (file) files.push(file);
      }
    }
  }
  if (files.length > 0) {
    handleFiles(files);
  }
});

elements.authConnectBtn.addEventListener('click', connectToken);
if (elements.notificationBtn) {
  elements.notificationBtn.addEventListener('click', async () => {
    await requestNotificationPermission();
  });
}
elements.authCancelBtn.addEventListener('click', closeAuthModal);
elements.renameSessionCancelBtn.addEventListener('click', closeRenameSessionModal);
elements.renameImproveTitleBtn.addEventListener('click', () => {
  void improveRenameTitleSuggestion();
});
elements.renameSessionSaveBtn.addEventListener('click', () => {
  void submitRenameSessionModal();
});
elements.askUserSubmitBtn.addEventListener('click', () => {
  submitAskUserModal(false);
});
elements.askUserCancelBtn.addEventListener('click', () => {
  submitAskUserModal(true);
});
elements.askUserModal.addEventListener('keydown', (event) => {
  if (event.key === 'Escape' && !event.defaultPrevented) {
    event.preventDefault();
    submitAskUserModal(true);
  }
});
elements.approvalApproveBtn.addEventListener('click', () => submitApprovalModal(false));
elements.approvalDenyBtn.addEventListener('click', () => submitApprovalModal(true));
elements.approvalModal.addEventListener('keydown', (event) => {
  if (event.key === 'Escape' && !event.defaultPrevented) {
    event.preventDefault();
    submitApprovalModal(true);
  }
});
elements.authTokenInput.addEventListener('keydown', (event) => {
  if (event.key === 'Enter') {
    event.preventDefault();
    connectToken();
  }
});
elements.renameSessionModal.addEventListener('keydown', (event) => {
  if (event.key === 'Escape' && !event.defaultPrevented) {
    event.preventDefault();
    closeRenameSessionModal();
    return;
  }
  if (event.key === 'Enter' && !event.shiftKey && !event.defaultPrevented) {
    event.preventDefault();
    void submitRenameSessionModal();
  }
});

window.addEventListener('resize', () => {
  if (!window.matchMedia('(max-width: 767px)').matches) {
    closeSidebar();
  }
  applyDesktopSidebarState();
});

const sidebarViewportMedia = window.matchMedia('(max-width: 767px)');
const handleSidebarViewportChange = () => {
  if (!sidebarViewportMedia.matches) {
    closeSidebar();
  }
  applyDesktopSidebarState();
};
if (typeof sidebarViewportMedia.addEventListener === 'function') {
  sidebarViewportMedia.addEventListener('change', handleSidebarViewportChange);
} else if (typeof sidebarViewportMedia.addListener === 'function') {
  sidebarViewportMedia.addListener(handleSidebarViewportChange);
}

window.addEventListener('popstate', async () => {
  const urlSlug = sessionIdFromURL();
  if (!urlSlug) {
    await switchToDraftSession({ closeSidebar: false });
    return;
  }
  const found = findSessionBySlug(urlSlug);
  if (found) {
    if (found.id === state.activeSessionId) return;
    await switchToSession(found.id, { closeSidebar: false });
    return;
  }
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
    messages: [],
    lastResponseId: null,
    activeResponseId: null,
    lastSequenceNumber: 0,
    _serverOnly: true
  };
  state.sessions.unshift(stub);
  await switchToSession(stub.id, { closeSidebar: false });
});

document.addEventListener('visibilitychange', async () => {
  if (document.visibilityState !== 'visible') {
    flushStreamPersistence();
    stopSidebarStatusPoll();
    return;
  }
  // Reconcile the authoritative transcript before looking for an active
  // response. Another tab may have completed several turns and started a new
  // one while this page was hidden; attaching first would only replay the new
  // response and leave the earlier turns missing.
  await startSidebarStatusPoll();
  if (document.visibilityState !== 'visible') return;
  const session = getActiveSession();
  if (!session) return;

  const pendingTranscriptUpdatedAt = numericTranscriptUpdatedAt(session._pendingTranscriptUpdatedAt);
  if (pendingTranscriptUpdatedAt > numericTranscriptUpdatedAt(session.transcriptUpdatedAt)) {
    // Do not attach a newer response onto a stale transcript. The visible
    // status poll retains the pending version and retries the tail fetch; once
    // it succeeds, reconciliation attaches the external response itself.
    showActiveTranscriptCatchUpWarning(session, pendingTranscriptUpdatedAt);
    return;
  }

  if (session.activeResponseId && app.wakeResponseReconnect?.({
    reason: 'visibility',
    sessionId: session.id,
    responseId: session.activeResponseId
  })) {
    setConnectionState('Page visible, reconnecting\u2026', 'bad');
    setStreaming(true);
    return;
  }
  if (session.activeResponseId && !state.abortController) {
    setStreaming(true);
    resumeAndDrain(session, {
      responseId: session.activeResponseId,
      recoverFromSnapshot: shouldRecoverActiveResponseFromSnapshot(session, session.activeResponseId)
    });
    return;
  }
  if (state.abortController && state.lastEventTime > 0 && Date.now() - state.lastEventTime > HEARTBEAT_STALE_THRESHOLD) {
    state.abortController._heartbeatAbort = true;
    state.abortController.abort(); // triggers retry in resumeActiveResponse
    return;
  }
  if (!state.streaming && !state.abortController) {
    await syncActiveSessionFromServer(session, true);
  }
});

window.addEventListener('pagehide', () => {
  flushStreamPersistence();
  stopSidebarStatusPoll();
});

window.addEventListener('online', async () => {
  setConnectionState('', '');
  const session = getActiveSession();
  if (!session) return;
  if (session.activeResponseId && app.wakeResponseReconnect?.({
    reason: 'online',
    sessionId: session.id,
    responseId: session.activeResponseId
  })) {
    setConnectionState('Network restored, reconnecting\u2026', 'bad');
    setStreaming(true);
    return;
  }
  if (session.activeResponseId && state.abortController) {
    // Abort the stale fetch so the existing resume loop reconnects immediately
    // instead of waiting for the heartbeat timeout.
    state.abortController._heartbeatAbort = true;
    state.abortController.abort();
  } else if (session.activeResponseId && !state.abortController) {
    setConnectionState('Network restored, reconnecting\u2026', 'bad');
    setStreaming(true);
    resumeAndDrain(session, {
      responseId: session.activeResponseId,
      recoverFromSnapshot: shouldRecoverActiveResponseFromSnapshot(session, session.activeResponseId)
    });
  } else if (!state.streaming) {
    await syncActiveSessionFromServer(session, true);
  }
});

window.addEventListener('offline', () => {
  setConnectionState('Network offline', 'bad');
});

window.addEventListener('pageshow', (event) => {
  void ensureSidebarStatusPoll();
  const session = getActiveSession();
  if (!session) return;
  if (session.activeResponseId && app.wakeResponseReconnect?.({
    reason: 'pageshow',
    sessionId: session.id,
    responseId: session.activeResponseId
  })) {
    setConnectionState('Page restored, reconnecting\u2026', 'bad');
    setStreaming(true);
    return;
  }
  if (!event.persisted) return;
  if (session.activeResponseId) {
    setStreaming(true);
    resumeAndDrain(session, {
      responseId: session.activeResponseId,
      recoverFromSnapshot: shouldRecoverActiveResponseFromSnapshot(session, session.activeResponseId)
    });
  } else {
    void syncActiveSessionFromServer(session, true);
  }
});

setInterval(refreshRelativeTimes, 60_000);

Object.assign(app, {
  applyGoalStateToSession,
  formatGoalChipText,
  updateGoalChip,
  openGoalModal,
  closeGoalModal,
  postSessionGoal,
  saveGoalFromModal,
  mutateGoalFromModal
});

initialize();

Object.assign(app, {
  createAndSwitchToFreshSession,
  convertServerMessages,
  compactionDuplicateTailRange,
  loadServerSessionMessages,
  refreshActiveSessionMessagesFromServer,
  loadOlderSessionMessages,
  maybeLoadOlderSessionMessages,
  loadServerSessionState,
  applyGoalStateToSession,
  formatGoalChipText,
  updateGoalChip,
  openGoalModal,
  closeGoalModal,
  postSessionGoal,
  saveGoalFromModal,
  mutateGoalFromModal,
  applyMCPStateToSession,
  fetchSessionMCP,
  applySessionMCP,
  openSessionMCPModal,
  closeMCPModal,
  mergeServerMessagesWithLocalState,
  stopSessionStatePoll,
  scheduleSessionStatePoll,
  syncActiveSessionFromServer,
  refreshCurrentPlanFromServer,
  invalidateSessionStateForSelection,
  syncSelectedRuntimeFromSession,
  applyServerSessionSummary,
  mergeServerSessions,
  startSidebarStatusPoll,
  stopSidebarStatusPoll,
  refreshSidebarStatusPoll,
  handleFetchTransportFallback,
  promptRenameSession,
  refineSessionTitle,
  setSessionArchived,
  setSessionPinned,
  switchToDraftSession,
  switchToSession,
  initialize
});
})();
