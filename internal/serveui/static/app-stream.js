(() => {
'use strict';

const app = window.TermLLMApp;
const {
  UI_PREFIX, STORAGE_KEYS, state, elements, generateId, sanitizeInterruptState, INTERJECTION_PHASE, sanitizeMessage, syncTokenCookie, truncate, saveSessions,
  getActiveSession, createSession, scrollToBottom, setConnectionState, sessionSlug, updateURL,
  persistAndRefreshShell, updateSessionUsageDisplay, splitHeaderModelEffort, compactHeaderModelLabel, getDefaultProviderName, getDefaultModelForProvider, refreshRelativeTimes, requestHeaders: _unusedRequestHeaders, updateAssistantNode, updateUserNode,
  updateToolNode, updateToolGroupNode, createMessageNode, createToolGroupNode, updateModelSwapNode, renderSidebar, renderMessages, maybeNotifyResponseComplete,
  enqueueAssistantStreamUpdate, finalizeAssistantStreamRender, syncTurnActionPanels,
  subscribeToPush, shouldAutoSubscribeToPush, applyTextDirection, shouldSuppressPromptAutoFocus, setSessionOptimisticBusy, setSessionServerActiveRun,
  renderAttachments, buildAttachmentInputParts, cloneAttachmentForMessage
} = app;

const rebaseStreamAssetURL = (url) => (
  typeof app.rebaseHubAssetURL === 'function'
    ? app.rebaseHubAssetURL(url)
    : String(url || '').trim()
);

// ===== Network helpers =====
const requestHeaders = (sessionId, tokenOverride = '') => {
  const headers = {
    'Content-Type': 'application/json'
  };

  const token = tokenOverride || state.token;
  if (token) {
    headers.Authorization = `Bearer ${token}`;
  }
  if (sessionId) {
    headers.session_id = sessionId;
  }
  if (app.UI_VERSION) {
    headers['X-Term-LLM-UI-Version'] = app.UI_VERSION;
  }

  return headers;
};

const forceSidebarStatusRefreshSoon = () => {
  if (typeof window !== 'undefined' && typeof window.setTimeout === 'function') {
    window.setTimeout(() => app.refreshSidebarStatusPoll?.(true), 0);
    return;
  }
  app.refreshSidebarStatusPoll?.(true);
};

const normalizeError = async (response) => {
  let message = `Request failed (${response.status}).`;
  let parsed;

  try {
    parsed = await response.json();
  } catch {
    parsed = null;
  }

  if (response.status === 401) {
    message = 'Auth failed — check your token.';
  } else if (response.status === 429) {
    message = 'Rate limited. Try again shortly.';
  } else if (parsed?.error?.message) {
    message = parsed.error.message;
  }

  return { status: response.status, message };
};

const hasSessionContinuationContext = (session) => Boolean(
  session && (
    Number(session.number || 0) > 0
    || (Array.isArray(session.messages) && session.messages.length > 0)
  )
);

const normalizeEffortForCompare = (value) => {
  const normalized = String(value || '').trim();
  return normalized.toLowerCase() === 'default' ? '' : normalized;
};

const sessionHasQueueableActiveRun = (session) => Boolean(
  session
  && !state.draftSessionActive
  && !state.askUser
  && !state.approval
  && (
    (state.streaming && (!state.currentStreamSessionId || state.currentStreamSessionId === session.id))
    || session.activeResponseId
    || app.sessionHasInProgressState?.(session)
  )
);

const setSessionPendingEffort = (session, effort) => {
  if (!session) return;
  session.pendingEffort = String(effort || '').trim();
  session.pendingEffortQueued = true;
};

const clearSessionPendingEffort = (session) => {
  if (!session) return;
  delete session.pendingEffort;
  delete session.pendingEffortQueued;
};

const clearTerminalPendingEffort = (session) => {
  if (session?.pendingEffortQueued) {
    clearSessionPendingEffort(session);
  }
};

const classifyRecoverableContinuationFailure = (error, previousResponseId = '') => {
  const status = Number(error?.status || 0);
  const message = String(error?.message || '').trim();
  const lowered = message.toLowerCase();

  if (previousResponseId && (status === 0 || status === 400 || status === 409) && lowered.includes('previous_response_id')) {
    return 'previous_response_id';
  }
  if (lowered.includes('session is busy processing another request')) {
    return 'session_busy';
  }
  return '';
};

const fetchProviders = async (tokenOverride = '') => {
  const headers = {};
  const token = tokenOverride || state.token;
  if (token) headers.Authorization = `Bearer ${token}`;

  const response = await fetch(`${UI_PREFIX}/v1/providers`, { headers });
  if (!response.ok) {
    throw await normalizeError(response);
  }

  const data = await response.json().catch(() => ({ data: [] }));
  return Array.isArray(data.data) ? data.data : [];
};

const normalizeModelMetadata = (items) => {
  const ids = [];
  const byID = {};
  (Array.isArray(items) ? items : []).forEach((m) => {
    const id = String(m?.id || '').trim();
    if (!id) return;
    ids.push(id);
    const efforts = Array.isArray(m?.reasoning_efforts)
      ? m.reasoning_efforts.map((v) => String(v || '').trim()).filter(Boolean)
      : [];
    byID[id] = { id, reasoning_efforts: efforts };
  });
  return { ids, byID };
};

const fetchModels = async (tokenOverride = '', provider = '') => {
  const headers = {};
  const token = tokenOverride || state.token;
  if (token) headers.Authorization = `Bearer ${token}`;

  let url = `${UI_PREFIX}/v1/models`;
  if (provider) url += `?provider=${encodeURIComponent(provider)}`;

  const response = await fetch(url, { headers });
  if (!response.ok) {
    throw await normalizeError(response);
  }

  const data = await response.json().catch(() => ({ data: [] }));
  const { ids, byID } = normalizeModelMetadata(data.data);
  const requestedProvider = String(provider || state.selectedProvider || '').trim();
  if (!requestedProvider || requestedProvider === String(state.selectedProvider || '').trim()) {
    state.modelInfoByID = byID;
  }
  return ids;
};

const parseSSEStream = async (stream, onEvent) => {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  let buffer = '';

  const processBlock = async (block) => {
    let eventName = '';
    let data = '';
    let start = 0;
    const len = block.length;

    while (start < len) {
      let end = block.indexOf('\n', start);
      if (end === -1) end = len;
      const c = block.charCodeAt(start);
      if (c === 101 /* 'e' */ && block.startsWith('event:', start)) {
        eventName = block.slice(start + 6, end).trim();
      } else if (c === 100 /* 'd' */ && block.startsWith('data:', start)) {
        const chunk = block.slice(start + 5, end).trimStart();
        data = data ? data + '\n' + chunk : chunk;
      }
      start = end + 1;
    }

    return onEvent(eventName, data);
  };

  while (true) {
    const { value, done } = await reader.read();
    if (done) break;

    const decoded = decoder.decode(value, { stream: true });
    buffer += decoded.includes('\r') ? decoded.replace(/\r/g, '') : decoded;
    state.lastEventTime = Date.now();
    if (state.abortController) {
      state.abortController._heartbeatStaleThreshold = HEARTBEAT_STALE_THRESHOLD;
    }

    let idx;
    while ((idx = buffer.indexOf('\n\n')) !== -1) {
      const block = buffer.slice(0, idx);
      buffer = buffer.slice(idx + 2);
      const keepGoing = await processBlock(block);
      if (keepGoing === false) {
        reader.cancel().catch(() => {});
        return;
      }
    }
  }

  if (buffer.trim()) {
    await processBlock(buffer);
  }
};

const sleep = (ms) => new Promise((resolve) => window.setTimeout(resolve, ms));

const STREAM_FAST_RETRY_LIMIT = 5;
const STREAM_SLOW_RETRY_DELAY = 60000;
const streamReconnectDelay = (attempt) => {
  const normalized = Math.max(0, Number(attempt || 0));
  if (normalized >= STREAM_FAST_RETRY_LIMIT) return STREAM_SLOW_RETRY_DELAY;
  return 1000 * Math.pow(1.5, normalized);
};

const streamReconnectLabel = (attempt) => (
  attempt >= STREAM_FAST_RETRY_LIMIT
    ? 'Connection unstable; retrying once a minute…'
    : (attempt < 3 ? 'Reconnecting…' : `Reconnecting (attempt ${attempt + 1})…`)
);

const streamHadActivitySince = (timestamp) => Number(state.lastEventTime || 0) > Number(timestamp || 0);

const isTransientPreResponsePostError = (err) => {
  const status = Number(err?.status || 0);
  if (Object.prototype.hasOwnProperty.call(err || {}, 'status')) {
    return status === 0 || status === 408 || status === 425 || status === 429 || status >= 500;
  }
  const name = String(err?.name || '');
  const message = String(err?.message || '').toLowerCase();
  return name === 'TypeError' || name === 'NetworkError' || message.includes('network') || message.includes('failed to fetch');
};

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
      autoGrowPrompt();
    }
    return false;
  }
  elements.promptInput.value = draft.prompt;
  autoGrowPrompt();
  return true;
};

const restoreLatestDraftMessage = () => {
  return restoreDraftMessageForSession(state.activeSessionId);
};

const setActiveResponseTracking = (session, responseId, sequenceNumber = null) => {
  if (!session) return;
  const normalized = String(responseId || '').trim();
  if (!normalized) return;

  if (session.activeResponseId !== normalized) {
    session.activeResponseId = normalized;
    if (sequenceNumber === null) {
      session.lastSequenceNumber = 0;
    }
  }

  if (sequenceNumber !== null) {
    const nextSeq = Number(sequenceNumber);
    if (Number.isFinite(nextSeq) && nextSeq >= 0) {
      session.lastSequenceNumber = nextSeq;
    }
  }
};

let heartbeatTimerId = null;
const HEARTBEAT_STALE_THRESHOLD = 30000; // Backend pings every 10s
const HEARTBEAT_UPLOAD_GRACE_BYTES_PER_SECOND = 32 * 1024;
const HEARTBEAT_UPLOAD_GRACE_MAX = 15 * 60 * 1000;
const heartbeatUploadGraceThreshold = (bodyText = '') => {
  const bytes = String(bodyText || '').length;
  if (bytes <= 0) return HEARTBEAT_STALE_THRESHOLD;
  return Math.min(
    HEARTBEAT_UPLOAD_GRACE_MAX,
    Math.max(HEARTBEAT_STALE_THRESHOLD, HEARTBEAT_STALE_THRESHOLD + Math.ceil((bytes / HEARTBEAT_UPLOAD_GRACE_BYTES_PER_SECOND) * 1000))
  );
};
// Deliberately not passed to AbortController.abort(): custom abort reasons can
// make fetch reject with a raw string instead of an AbortError in some browsers.
const HEARTBEAT_ABORT_REASON = 'heartbeat';

const startHeartbeatMonitor = () => {
  stopHeartbeatMonitor();
  state.lastEventTime = Date.now();
  heartbeatTimerId = window.setInterval(() => {
    try {
      if (!state.abortController || !state.currentStreamSessionId) {
        stopHeartbeatMonitor();
        return;
      }
      // While an ask_user or approval modal is open the application event stream
      // may be intentionally quiet while blocked waiting for user input.  A
      // stale-heartbeat abort here would needlessly reconnect the SSE stream and
      // replay the prompt event, resetting any partial answer the user has typed.
      if (state.askUser || state.approval) return;
      const staleThreshold = Math.max(
        HEARTBEAT_STALE_THRESHOLD,
        Number(state.abortController?._heartbeatStaleThreshold || 0) || 0
      );
      if (Date.now() - state.lastEventTime > staleThreshold) {
        if (state.abortController) {
          state.abortController._heartbeatAbort = true;
          state.abortController.abort();
        }
      }
    } catch (err) {
      console.warn('[stream] heartbeat monitor failed', err);
    }
  }, 10000);
};

const stopHeartbeatMonitor = () => {
  if (heartbeatTimerId !== null) {
    window.clearInterval(heartbeatTimerId);
    heartbeatTimerId = null;
  }
};

const STREAM_PERSIST_INTERVAL = 1000;
let streamPersistTimerId = null;
let streamPersistDirty = false;
let streamScrollRafId = 0;

const scheduleStreamPersistence = () => {
  streamPersistDirty = true;
  if (streamPersistTimerId !== null) return;
  streamPersistTimerId = window.setTimeout(() => {
    streamPersistTimerId = null;
    if (!streamPersistDirty) return;
    streamPersistDirty = false;
    saveSessions();
  }, STREAM_PERSIST_INTERVAL);
};

const flushStreamPersistence = () => {
  if (streamPersistTimerId !== null) {
    clearTimeout(streamPersistTimerId);
    streamPersistTimerId = null;
  }
  if (!streamPersistDirty) return;
  streamPersistDirty = false;
  saveSessions();
};

const scheduleStreamScroll = () => {
  if (streamScrollRafId) return;
  streamScrollRafId = window.requestAnimationFrame(() => {
    streamScrollRafId = 0;
    scrollToBottom();
  });
};

// Guards against multiple concurrent resumeActiveResponse loops for the same response.
const activeResumeKeys = new Set();

const attachResponseStream = (session, responseId = '', controller = null) => {
  state.currentStreamSessionId = String(session?.id || '').trim();
  state.currentStreamResponseId = String(responseId || '').trim();
  state.abortController = controller;
  if (controller) {
    startHeartbeatMonitor();
  }
};

const clearResumeKeysForSession = (sessionId) => {
  const prefix = sessionId + ':';
  for (const key of activeResumeKeys) {
    if (key.startsWith(prefix)) activeResumeKeys.delete(key);
  }
};

const detachResponseStream = () => {
  stopHeartbeatMonitor();
  flushStreamPersistence();
  state.streamGeneration += 1;
  const controller = state.abortController;
  const detachedSessionId = state.currentStreamSessionId;
  state.abortController = null;
  state.currentStreamSessionId = '';
  state.currentStreamResponseId = '';
  if (controller) {
    try { controller.abort(); } catch (_) { /* stream may already be closed */ }
  }
  // Clear resume keys for the detached session so that a subsequent
  // resumeActiveResponse (e.g. when switching back) is not blocked by
  // a stale key from the still-unwinding previous resume loop.
  if (detachedSessionId) {
    clearResumeKeysForSession(detachedSessionId);
  }
  setConnectionState('', '');
  setStreaming(false);
};

const clearActiveResponseTracking = (session, responseId = '') => {
  if (!session) return;
  const currentId = String(session.activeResponseId || '').trim();
  const targetId = String(responseId || '').trim();

  if (!targetId || currentId === targetId || targetId.startsWith('resp_msg_')) {
    session.activeResponseId = null;
    session.lastSequenceNumber = 0;
  }
  if (
    !targetId
    || (
      state.currentStreamSessionId === String(session.id || '').trim()
      && (!state.currentStreamResponseId || state.currentStreamResponseId === targetId || targetId.startsWith('resp_msg_'))
    )
  ) {
    state.currentStreamSessionId = '';
    state.currentStreamResponseId = '';
    if (!state.abortController) {
      setConnectionState('', '');
    }
  }
};

const updateResponseSequence = (session, payload) => {
  if (!session || !payload || typeof payload !== 'object') return;
  const seq = Number(payload.sequence_number);
  if (!Number.isFinite(seq) || seq <= 0) return;
  const current = Number(session.lastSequenceNumber || 0);
  session.lastSequenceNumber = Math.max(current, seq);
};

// Visible-session render guards: stream events may arrive after the user has
// switched discussions, so session data can update while DOM work is limited to
// the conversation currently mounted in elements.messages.
const isSessionVisible = (session) => !state.draftSessionActive && state.activeSessionId === session.id;

const appendStreamMessageNode = (session, message, createNode = createMessageNode) => {
  if (!isSessionVisible(session)) return null;
  const node = createNode(message);
  elements.messages.appendChild(node);
  return node;
};

const updateVisibleToolGroupNode = (session, message) => {
  if (isSessionVisible(session)) updateToolGroupNode(message);
};

const updateVisibleModelSwapNode = (session, message) => {
  if (isSessionVisible(session)) updateModelSwapNode(message);
};

const enqueueVisibleAssistantStreamUpdate = (session, message) => {
  if (isSessionVisible(session)) enqueueAssistantStreamUpdate(message);
};

const finalizeVisibleAssistantStreamRender = (session, message) => {
  if (isSessionVisible(session)) finalizeAssistantStreamRender(message);
};

const scrollVisibleStreamToBottom = (session, force = false) => {
  if (!isSessionVisible(session)) return;
  if (force) {
    scrollToBottom(true);
  } else {
    scrollToBottom();
  }
};

const scheduleVisibleStreamScroll = (session) => {
  if (isSessionVisible(session)) scheduleStreamScroll();
};

const createResponseStreamState = (session) => {
	let currentToolGroup = session.messages.findLast((message) => (
		message.role === 'tool-group' && message.status === 'running'
	)) || null;
	let currentAssistantMessage = null;
	let currentPhaseMessage = null;

  if (!currentToolGroup) {
    const lastMessage = session.messages[session.messages.length - 1];
    if (lastMessage?.role === 'assistant') {
      currentAssistantMessage = lastMessage;
    }
  }

  const ensureAssistantMessage = () => {
    if (currentAssistantMessage) return currentAssistantMessage;
    const msg = {
      id: generateId('msg'),
      role: 'assistant',
      content: '',
      created: Date.now()
    };
    session.messages.push(msg);
    appendStreamMessageNode(session, msg);
    currentAssistantMessage = msg;
    return msg;
  };

  const closeToolGroup = () => {
    if (!currentToolGroup) return;
    currentToolGroup.tools.forEach((tool) => { tool.status = 'done'; });
    currentToolGroup.status = 'done';
    updateVisibleToolGroupNode(session, currentToolGroup);
    currentToolGroup = null;
  };

  return {
    ensureAssistantMessage,
    closeToolGroup,
    get currentToolGroup() {
      return currentToolGroup;
    },
    set currentToolGroup(value) {
      currentToolGroup = value;
    },
    get currentAssistantMessage() {
      return currentAssistantMessage;
    },
    set currentAssistantMessage(value) {
      currentAssistantMessage = value;
    },
    get currentPhaseMessage() {
      return currentPhaseMessage;
    },
    set currentPhaseMessage(value) {
      currentPhaseMessage = value;
    }
  };
};

const applyResponseStreamEvent = (session, streamState, event, payload) => {
  if (event === 'response.output_text.delta') {
    const seq = payload.sequence_number;
    if (seq > session.lastSequenceNumber) session.lastSequenceNumber = seq;
    const delta = payload.delta || '';
    if (delta) {
      streamState.closeToolGroup();
      const msg = streamState.ensureAssistantMessage();
      msg.content += delta;
      scheduleStreamPersistence();
      enqueueVisibleAssistantStreamUpdate(session, msg);
    }
    return { terminal: false };
  }

  updateResponseSequence(session, payload);

  if (event === 'response.file_change') {
    app.handleFileChangeEvent?.(session, payload);
    return { terminal: false };
  }

  if (event === 'response.stream_error') {
    const errorType = String(payload?.error?.type || '').trim();
    if (errorType === 'stream_buffer_overflow') {
      applyResponseRecoverySnapshot(session, {
        id: session.activeResponseId || state.currentStreamResponseId || '',
        status: 'in_progress',
        last_sequence_number: payload.sequence_number,
        recovery: payload.recovery || null
      });
      streamState.currentToolGroup = session.messages.findLast((message) => (
        message.role === 'tool-group' && message.status === 'running'
      )) || null;
      streamState.currentAssistantMessage = streamState.currentToolGroup
        ? null
        : (session.messages[session.messages.length - 1]?.role === 'assistant'
          ? session.messages[session.messages.length - 1]
          : null);
      streamState.currentPhaseMessage = null;
      return { terminal: false, recoverableStreamError: true };
    }
    return { terminal: false };
  }

  if (event === 'response.created') {
    const responseId = String(payload?.response?.id || '').trim();
    setSessionOptimisticBusy(session, true);
    if (responseId) {
      setActiveResponseTracking(session, responseId, payload?.sequence_number ?? null);
      saveSessions();
    }
    const model = payload?.response?.model;
    if (model) {
      session.activeModel = model;
    }
    const provider = payload?.response?.provider;
    if (provider) {
      session.provider = provider;
    }
    if (Object.prototype.hasOwnProperty.call(payload?.response || {}, 'reasoning_effort')) {
      session.activeEffort = payload.response.reasoning_effort || '';
    }
    if (model || provider || Object.prototype.hasOwnProperty.call(payload?.response || {}, 'reasoning_effort')) {
      updateSessionUsageDisplay(session);
    }
    return { terminal: false };
  }

  if (event === 'response.model_switch') {
    const model = String(payload?.model || '').trim();
    if (model) session.activeModel = model;
    let appliedEffort = session.activeEffort || '';
    if (Object.prototype.hasOwnProperty.call(payload || {}, 'reasoning_effort')) {
      appliedEffort = payload.reasoning_effort || '';
      session.activeEffort = appliedEffort;
    }
    const pendingStillTargetsLater = Boolean(
      session.pendingEffortQueued
      && normalizeEffortForCompare(session.pendingEffort || '') !== normalizeEffortForCompare(appliedEffort)
    );
    const isActiveSession = session.id && session.id === state.activeSessionId;
    if (!pendingStillTargetsLater) {
      clearSessionPendingEffort(session);
      if (isActiveSession) state.selectedEffort = session.activeEffort || '';
    } else if (isActiveSession) {
      state.selectedEffort = session.pendingEffort || '';
    }
    if (isActiveSession) {
      persistRuntimeSelection();
      syncSettingsSelectValues();
      updateSessionUsageDisplay(session);
    }
    return { terminal: false };
  }

  if (event === 'response.model_swap.progress') {
    const stage = String(payload?.stage || '').trim();
    const message = String(payload?.message || '').trim();
    if (stage === 'failed') {
      if (payload?.previous_provider) session.provider = String(payload.previous_provider);
      if (payload?.previous_model) session.activeModel = String(payload.previous_model);
      if (Object.prototype.hasOwnProperty.call(payload || {}, 'previous_effort')) {
        session.activeEffort = payload.previous_effort || '';
      }
      updateSessionUsageDisplay(session);
    } else if (stage === 'complete') {
      if (payload?.target_provider) session.provider = String(payload.target_provider);
      if (payload?.target_model) session.activeModel = String(payload.target_model);
      if (Object.prototype.hasOwnProperty.call(payload || {}, 'target_effort')) {
        session.activeEffort = payload.target_effort || '';
      }
      updateSessionUsageDisplay(session);
    }
    if (message) {
      let marker = streamState.modelSwapProgressMessage || null;
      if (!marker) {
        marker = {
          id: generateId('swap'),
          role: 'model-swap',
          content: message,
          stage: payload?.stage || '',
          created: Date.now(),
          transient: true
        };
        streamState.modelSwapProgressMessage = marker;
        session.messages.push(marker);
        appendStreamMessageNode(session, marker);
      } else {
        marker.content = message;
        marker.stage = payload?.stage || marker.stage || '';
        updateVisibleModelSwapNode(session, marker);
      }
      scheduleStreamPersistence();
      scrollVisibleStreamToBottom(session);
    }
    return { terminal: false };
  }

  if (event === 'response.phase') {
    const text = String(payload?.text || '').trim();
    if (text) {
      if (streamState.currentAssistantMessage?.content) {
        finalizeVisibleAssistantStreamRender(session, streamState.currentAssistantMessage);
      }
      streamState.currentAssistantMessage = null;
      let marker = streamState.currentPhaseMessage || null;
      if (!marker) {
        marker = {
          id: generateId('phase'),
          role: 'phase',
          content: text,
          created: Date.now(),
          transient: true
        };
        streamState.currentPhaseMessage = marker;
        session.messages.push(marker);
        appendStreamMessageNode(session, marker);
      } else {
        marker.content = text;
        if (isSessionVisible(session)) updateUserNode(marker);
      }
      scheduleStreamPersistence();
      scrollVisibleStreamToBottom(session);
    }
    return { terminal: false };
  }

  if (event === 'response.output_text.new_segment') {
    streamState.closeToolGroup();
    if (streamState.currentAssistantMessage?.content) {
      finalizeVisibleAssistantStreamRender(session, streamState.currentAssistantMessage);
    }
    streamState.currentAssistantMessage = null;
    return { terminal: false };
  }

  if (event === 'response.output_item.added') {
    const item = payload.item;
    if (item?.type === 'function_call') {
      if (streamState.currentAssistantMessage?.content) {
        finalizeVisibleAssistantStreamRender(session, streamState.currentAssistantMessage);
      }
      const toolEntry = {
        id: item.call_id || generateId('tool'),
        name: String(item.name || 'tool'),
        arguments: String(item.arguments || ''),
        status: 'running',
        created: Date.now(),
        outputIndex: payload.output_index,
        // Providers usually stream arguments via deltas after an empty added
        // event. If a replay/snapshot seeds arguments here, treat them as
        // complete so repeated deltas cannot duplicate them.
        argumentsFinalized: Boolean(String(item.arguments || '').trim())
      };

      if (!streamState.currentToolGroup) {
        streamState.currentToolGroup = {
          id: generateId('msg'),
          role: 'tool-group',
          tools: [toolEntry],
          expanded: false,
          status: 'running',
          created: Date.now()
        };
        session.messages.push(streamState.currentToolGroup);
        appendStreamMessageNode(session, streamState.currentToolGroup, createToolGroupNode);
      } else {
        streamState.currentToolGroup.tools.push(toolEntry);
        updateVisibleToolGroupNode(session, streamState.currentToolGroup);
      }

      streamState.currentAssistantMessage = null;
      scheduleStreamPersistence();
      scrollVisibleStreamToBottom(session);
    }
    return { terminal: false };
  }

  if (event === 'response.function_call_arguments.delta') {
    if (streamState.currentToolGroup) {
      const outputIndex = payload.output_index;
      const delta = String(payload.delta || '');
      if (delta) {
        const tools = streamState.currentToolGroup.tools;
        const exactEntry = outputIndex == null
          ? null
          : tools.find((tool) => tool.outputIndex === outputIndex);
        const entry = exactEntry
          || tools.findLast((tool) => tool.status !== 'done')
          || tools[tools.length - 1];
        if (entry && !entry.argumentsFinalized) {
          entry.arguments = String(entry.arguments || '') + delta;
          updateVisibleToolGroupNode(session, streamState.currentToolGroup);
          scheduleStreamPersistence();
        }
      }
    }
    return { terminal: false };
  }

  if (event === 'response.output_item.done') {
    const item = payload.item;
    if (item?.type === 'function_call' && streamState.currentToolGroup) {
      const callId = item.call_id || item.id;
      const entry = callId
        ? streamState.currentToolGroup.tools.find((tool) => tool.id === callId)
        : streamState.currentToolGroup.tools.find((tool) => tool.name === String(item.name || '') && tool.status === 'running');
      if (entry) {
        entry.arguments = String(item.arguments || entry.arguments || '');
        entry.argumentsFinalized = true;
      }
      updateVisibleToolGroupNode(session, streamState.currentToolGroup);
      scheduleStreamPersistence();
    }
    return { terminal: false };
  }

  if (event === 'response.ask_user.prompt') {
    const callId = String(payload.call_id || '').trim();
    const questions = Array.isArray(payload.questions) ? payload.questions : [];
    if (callId && questions.length > 0) {
      const samePrompt = state.askUser
        && state.askUser.sessionId === session.id
        && state.askUser.callId === callId;
      if (!samePrompt) {
        openAskUserModal(session.id, callId, questions);
      }
    }
    return { terminal: false };
  }

  if (event === 'response.approval.prompt') {
    const approvalId = String(payload.approval_id || '').trim();
    const options = Array.isArray(payload.options) ? payload.options : [];
    if (approvalId && options.length > 0) {
      openApprovalModal(session.id, approvalId, payload.path, payload.is_shell, payload.title, options);
    }
    return { terminal: false };
  }

  if (event === 'response.tool_exec.end') {
    if (streamState.currentToolGroup) {
      const callId = payload.call_id;
      const entry = streamState.currentToolGroup.tools.find((tool) => tool.id === callId);
      if (entry) {
        entry.status = 'done';
        updateVisibleToolGroupNode(session, streamState.currentToolGroup);
      }
      if (streamState.currentToolGroup.tools.every((tool) => tool.status === 'done')) {
        streamState.currentToolGroup.status = 'done';
        updateVisibleToolGroupNode(session, streamState.currentToolGroup);
      }
    }
    if (payload.images && payload.images.length > 0 && streamState.currentToolGroup) {
      const callId = payload.call_id;
      const entry = callId
        ? streamState.currentToolGroup.tools.find((tool) => tool.id === callId)
        : streamState.currentToolGroup.tools.findLast((tool) => tool.status === 'done')
          || streamState.currentToolGroup.tools[streamState.currentToolGroup.tools.length - 1];
      if (entry) {
        const existing = Array.isArray(entry.images) ? entry.images : [];
        payload.images.forEach((url) => {
          const normalized = rebaseStreamAssetURL(url);
          if (normalized && !existing.includes(normalized)) existing.push(normalized);
        });
        if (existing.length > 0) entry.images = existing;
        updateVisibleToolGroupNode(session, streamState.currentToolGroup);
      }
    }
    saveSessions();
    scheduleVisibleStreamScroll(session);
    return { terminal: false };
  }

  if (event === 'response.interjection') {
    const interjectionText = String(payload.text || '').trim();
    if (interjectionText) {
      streamState.closeToolGroup();
      if (streamState.currentAssistantMessage) {
        finalizeVisibleAssistantStreamRender(session, streamState.currentAssistantMessage);
        streamState.currentAssistantMessage = null;
      }
      const payloadInterjectionId = String(payload.interjection_id || '').trim();
      // Resolve the optimistic pending entry by id first, but fall back to a
      // text match. The pending entry may have been tracked under a synthetic
      // or optimistic id (e.g. from session sync) that differs from the
      // server-issued interjection_id, so an id-only lookup would otherwise
      // leave the "will incorporate" banner stranded after commit.
      let pending = payloadInterjectionId
        ? removePendingInterjectionById(payloadInterjectionId)
        : null;
      if (!pending) {
        pending = consumePendingInterjectionByText(session.id, interjectionText);
      }
      let messageId = pending?.messageId || payloadInterjectionId;
      if (isSessionVisible(session)) {
        const emptyState = elements.messages.querySelector('.empty-state');
        if (emptyState) emptyState.remove();
      }
      let existingMessage = messageId
        ? session.messages.find(m => m.id === messageId && m.role === 'user')
        : null;
      if (!existingMessage) {
        existingMessage = session.messages.find(m => (
          m.role === 'user'
          && (sanitizeInterruptState(m.interruptState) === 'evaluating'
            || sanitizeInterruptState(m.interruptState) === 'pending_interject'
            || sanitizeInterruptState(m.interruptState) === 'interject')
          && String(m.content || '').trim() === interjectionText
        )) || null;
        if (existingMessage?.id) messageId = existingMessage.id;
      }
      if (!messageId) messageId = generateId('msg');
      const message = existingMessage || {
        id: messageId,
        role: 'user',
        content: interjectionText,
        created: Date.now(),
        interruptState: 'interject'
      };
      message.content = interjectionText;
      message.interruptState = 'interject';
      if (Array.isArray(payload.attachments) && payload.attachments.length > 0 && !message.attachments) {
        message.attachments = payload.attachments;
      }
      if (!existingMessage) {
        session.messages.push(message);
        appendStreamMessageNode(session, message);
      } else {
        updateUserNode(message);
      }
      if (isSessionVisible(session)) syncTurnActionPanels();
      const committed = payload.interjection_id
        ? resolvePendingInterruptCommitById(String(payload.interjection_id))
        : resolvePendingInterruptCommit(session.id, interjectionText);
      if (!committed) resolvePendingInterruptCommit(session.id, interjectionText);
      saveSessions();
      scrollVisibleStreamToBottom(session, true);
    }
    return { terminal: false };
  }

  if (event === 'response.completed') {
    const usage = payload?.response?.usage;
    streamState.closeToolGroup();
    markToolGroupsDone(session);
    requeuePendingInterjections(session);

    const responseId = String(payload?.response?.id || session.activeResponseId || state.currentStreamResponseId || '').trim();
    if (responseId) {
      session.lastResponseId = responseId;
    }
    clearActiveResponseTracking(session, responseId);
    setSessionOptimisticBusy(session, false);
    setSessionServerActiveRun(session, false);

    const sessionUsage = payload?.response?.session_usage;
    if (sessionUsage) session.sessionUsage = sessionUsage;
    if (usage) session.lastUsage = usage;
    const completedModel = payload?.response?.model;
    if (completedModel) session.activeModel = completedModel;
    const completedProvider = payload?.response?.provider;
    if (completedProvider) session.provider = completedProvider;
    if (Object.prototype.hasOwnProperty.call(payload?.response || {}, 'reasoning_effort')) {
      session.activeEffort = payload.response.reasoning_effort || '';
    }
    clearTerminalPendingEffort(session);
    updateSessionUsageDisplay(session);

    const lastAssistant = session.messages.findLast((message) => message.role === 'assistant');
    if (lastAssistant) {
      if (usage) lastAssistant.usage = usage;
      finalizeVisibleAssistantStreamRender(session, lastAssistant);
    }
    session.lastMessageAt = Date.now();
    flushStreamPersistence();
    saveSessions();
    renderSidebar();
    forceSidebarStatusRefreshSoon();
    void maybeNotifyResponseComplete(session, lastAssistant, responseId);
    app.refreshFileChangesAfterRun?.(session);
    scrollVisibleStreamToBottom(session);
    return { terminal: true };
  }

  if (event === 'response.cancelled') {
    streamState.closeToolGroup();
    markToolGroupsDone(session);
    requeuePendingInterjections(session);
    clearTerminalPendingEffort(session);
    clearActiveResponseTracking(session, session.activeResponseId || state.currentStreamResponseId);
    setSessionOptimisticBusy(session, false);
    setSessionServerActiveRun(session, false);
    const lastAssistant = session.messages.findLast((message) => message.role === 'assistant');
    if (lastAssistant) finalizeVisibleAssistantStreamRender(session, lastAssistant);
    flushStreamPersistence();
    saveSessions();
    renderSidebar();
    forceSidebarStatusRefreshSoon();
    app.refreshFileChangesAfterRun?.(session);
    scrollVisibleStreamToBottom(session, true);
    return { terminal: true };
  }

  if (event === 'response.failed') {
    const errorMessage = payload?.error?.message || 'The response failed.';
    const lowered = errorMessage.toLowerCase();
    const recoverableContinuationFailure = classifyRecoverableContinuationFailure(
      { message: errorMessage },
      session.lastResponseId
    );
    const canceledByInterrupt = state.expectCanceledRun && (
      lowered.includes('context canceled') ||
      lowered.includes('context cancelled') ||
      lowered.includes('cancelled') ||
      lowered.includes('canceled')
    );

    if (!canceledByInterrupt && !recoverableContinuationFailure) {
      addErrorMessage(errorMessage, session);
    }
    state.expectCanceledRun = false;

    streamState.closeToolGroup();
    markToolGroupsDone(session);
    requeuePendingInterjections(session);
    clearTerminalPendingEffort(session);
    clearActiveResponseTracking(session, session.activeResponseId || state.currentStreamResponseId);
    setSessionOptimisticBusy(session, false);
    setSessionServerActiveRun(session, false);

    const lastAssistant = session.messages.findLast((message) => message.role === 'assistant');
    if (lastAssistant) finalizeVisibleAssistantStreamRender(session, lastAssistant);
    flushStreamPersistence();
    saveSessions();
    renderSidebar();
    forceSidebarStatusRefreshSoon();
    app.refreshFileChangesAfterRun?.(session);
    scrollVisibleStreamToBottom(session, true);
    return {
      terminal: true,
      error: recoverableContinuationFailure
        ? { message: errorMessage, recoverableContinuationFailure }
        : null
    };
  }

  return { terminal: false };
};

const consumeResponseStream = async (stream, session, streamState, options = {}) => {
  let sawTerminal = false;
  let sawDone = false;
  let sawRecoverableStreamError = false;
  let stale = false;
  let terminalError = null;
  const generation = Number.isFinite(Number(options.generation)) ? Number(options.generation) : state.streamGeneration;
  const sessionId = String(session?.id || '').trim();
  const expectedResponseId = String(options.responseId || '').trim();

  const eventSequenceNumber = (payload) => {
    const seq = Number(payload?.sequence_number);
    return Number.isFinite(seq) && seq > 0 ? seq : 0;
  };

  const streamIsCurrent = () => {
    if (generation !== state.streamGeneration) return false;
    const currentSessionId = state.currentStreamSessionId;
    if (currentSessionId && sessionId && currentSessionId !== sessionId) return false;
    const currentResponseId = state.currentStreamResponseId;
    if (expectedResponseId && currentResponseId && currentResponseId !== expectedResponseId) return false;
    return true;
  };

  await parseSSEStream(stream, async (event, data) => {
    if (!streamIsCurrent()) {
      stale = true;
      return false;
    }

    if (data === '[DONE]') {
      if (!sawRecoverableStreamError) {
        sawDone = true;
        streamState.closeToolGroup();
        markToolGroupsDone(session);
      }
      persistAndRefreshShell();
      return false;
    }

    if (!data) return true;

    let payload;
    try {
      payload = JSON.parse(data);
    } catch {
      return true;
    }

    if (!streamIsCurrent()) {
      stale = true;
      return false;
    }
    const seq = eventSequenceNumber(payload);
    const currentSeq = Number(session.lastSequenceNumber || 0);
    if (seq > currentSeq + 1) {
      terminalError = {
        message: `response event stream gap: expected sequence ${currentSeq + 1}, received ${seq}`,
        recoverableStreamGap: true,
      };
      return false;
    }

    let result;
    try {
      result = applyResponseStreamEvent(session, streamState, event, payload);
    } catch (err) {
      session.lastSequenceNumber = currentSeq;
      terminalError = {
        message: err?.message || 'response event projection failed',
        recoverableStreamApplyFailure: true,
      };
      return false;
    }
    if (result?.terminal) {
      sawTerminal = true;
    }
    if (result?.recoverableStreamError) {
      sawRecoverableStreamError = true;
    }
    if (result?.error) {
      terminalError = result.error;
    }
    if (!streamIsCurrent()) {
      stale = true;
      return false;
    }
    return true;
  });

  return { terminal: sawTerminal || sawDone || !session.activeResponseId, stale, error: stale ? null : terminalError };
};

const fetchResponseSnapshot = async (session, responseId) => {
  const response = await fetch(`${UI_PREFIX}/v1/responses/${encodeURIComponent(responseId)}`, {
    headers: requestHeaders(session?.id || '')
  });
  if (!response.ok) {
    throw await normalizeError(response);
  }
  return response.json().catch(() => ({}));
};

const recoverResponseStateFromSnapshot = async (session, responseId) => {
  const snapshot = await fetchResponseSnapshot(session, responseId);
  applyResponseRecoverySnapshot(session, snapshot);
  return snapshot;
};

const clearCommittedInterjectionPendingState = (sessionId, messageId, content) => {
  const normalizedId = String(messageId || '').trim();
  const normalizedContent = String(content || '').trim();
  let pending = normalizedId ? removePendingInterjectionById(normalizedId) : null;
  if (!pending && normalizedContent) {
    pending = consumePendingInterjectionByText(sessionId, normalizedContent);
  }

  let committed = normalizedId ? resolvePendingInterruptCommitById(normalizedId) : null;
  if (!committed && pending?.messageId) {
    committed = resolvePendingInterruptCommitById(pending.messageId);
  }
  if (!committed && normalizedContent) {
    committed = resolvePendingInterruptCommit(sessionId, normalizedContent);
  }
  return { pending, committed };
};

const shouldDropPreservedOptimisticInterjection = (message, recoveredInterjections) => {
  if (message?.role !== 'user') return false;
  const interruptState = sanitizeInterruptState(message.interruptState);
  if (interruptState !== 'evaluating' && interruptState !== 'pending_interject' && interruptState !== 'interject') {
    return false;
  }
  const messageId = String(message.id || '').trim();
  const content = String(message.content || '').trim();
  return recoveredInterjections.some((recovered) => {
    const recoveredId = String(recovered?.id || '').trim();
    if (messageId && recoveredId && messageId === recoveredId) return true;
    return content && String(recovered?.content || '').trim() === content;
  });
};

const applyResponseRecoverySnapshot = (session, payload) => {
  if (!session || !payload || typeof payload !== 'object') return false;

  const recovery = payload.recovery;
  const hasRecovery = recovery && typeof recovery === 'object';

  if (hasRecovery) {
    const rawMessages = Array.isArray(recovery.messages) ? recovery.messages : [];
    const recoveredMessages = rawMessages
      .map((message) => sanitizeMessage(message))
      .filter(Boolean);

    const recoveredInterjections = recoveredMessages.filter((message) => (
      message?.role === 'user' && message?.interruptState === 'interject'
    ));
    for (const message of recoveredInterjections) {
      clearCommittedInterjectionPendingState(session.id, message.id, message.content);
    }

    let anchorIndex = -1;
    for (let i = session.messages.length - 1; i >= 0; i -= 1) {
      if (session.messages[i]?.role === 'user') {
        anchorIndex = i;
        break;
      }
    }

    const preserved = anchorIndex >= 0
      ? session.messages.slice(0, anchorIndex + 1).filter((message) => !shouldDropPreservedOptimisticInterjection(message, recoveredInterjections))
      : [];
    session.messages = preserved.concat(recoveredMessages);
  }

  const nextSeq = Number(payload.last_sequence_number ?? recovery?.sequence_number ?? session.lastSequenceNumber ?? 0);
  if (Number.isFinite(nextSeq) && nextSeq >= 0) {
    session.lastSequenceNumber = nextSeq;
  }

  const responseId = String(payload.id || session.activeResponseId || state.currentStreamResponseId || '').trim();
  const snapshotModel = String(payload.model || '').trim();
  if (snapshotModel) session.activeModel = snapshotModel;
  if (Object.prototype.hasOwnProperty.call(payload || {}, 'reasoning_effort')) {
    session.activeEffort = payload.reasoning_effort || '';
  }
  const sessionUsage = payload.session_usage;
  if (sessionUsage) {
    session.sessionUsage = sessionUsage;
  }
  updateSessionUsageDisplay(session);

  if (payload.status === 'completed') {
    clearTerminalPendingEffort(session);
    if (responseId) {
      session.lastResponseId = responseId;
    }
    clearActiveResponseTracking(session, responseId);
    setSessionOptimisticBusy(session, false);
    setSessionServerActiveRun(session, false);
    requeuePendingInterjections(session);
  } else if (payload.status === 'failed' || payload.status === 'cancelled') {
    clearTerminalPendingEffort(session);
    clearActiveResponseTracking(session, responseId);
    setSessionOptimisticBusy(session, false);
    setSessionServerActiveRun(session, false);
    requeuePendingInterjections(session);
  } else if (responseId) {
    setActiveResponseTracking(session, responseId, session.lastSequenceNumber);
    setSessionOptimisticBusy(session, true);
  }

  saveSessions();
  renderSidebar();
  forceSidebarStatusRefreshSoon();
  if (session.id === state.activeSessionId) {
    renderMessages(true);
  } else {
    persistAndRefreshShell();
  }
  return hasRecovery || Boolean(String(payload.status || '').trim());
};

const resumeActiveResponse = async (session, options = {}) => {
  if (!session) return false;

  const responseId = String(options.responseId || session.activeResponseId || '').trim();
  if (!responseId) return false;

  // Prevent multiple concurrent resume loops for the same session+response.
  const resumeKey = `${session.id}:${responseId}`;
  if (activeResumeKeys.has(resumeKey)) return false;
  activeResumeKeys.add(resumeKey);

  try {
    return await resumeActiveResponseInner(session, responseId, options);
  } finally {
    activeResumeKeys.delete(resumeKey);
  }
};

const resumeActiveResponseInner = async (session, responseId, options) => {
  if (state.currentStreamSessionId && state.currentStreamSessionId !== session.id) {
    detachResponseStream();
  }

  if (session.activeResponseId !== responseId) {
    setActiveResponseTracking(session, responseId, 0);
    saveSessions();
  } else if (state.currentStreamSessionId !== session.id || state.currentStreamResponseId !== responseId) {
    attachResponseStream(session, responseId, null);
  }

  let recoveredFromSnapshot = false;
  if (options.recoverFromSnapshot) {
    try {
      const snapshot = await recoverResponseStateFromSnapshot(session, responseId);
      recoveredFromSnapshot = true;
      if (session.activeResponseId !== responseId) {
        setStreaming(Boolean(state.currentStreamResponseId));
        return true;
      }
      if (snapshot?.status !== 'in_progress') {
        setStreaming(Boolean(state.currentStreamResponseId));
        return true;
      }
    } catch (err) {
      if (err?.status === 401) {
        handleAuthFailure();
        return false;
      }
      // If the snapshot is briefly unavailable, fall back to the existing
      // event replay path rather than failing the reconnect outright.
    }
  }

  let streamState = recoveredFromSnapshot
    ? createResponseStreamState(session)
    : (options.streamState || createResponseStreamState(session));
  let consecutiveHttpFailures = 0;
  let consecutiveHeartbeatAborts = 0;
  let retryAttempt = 0;

  for (;;) {
    if (session.activeResponseId !== responseId) {
      setStreaming(Boolean(state.currentStreamResponseId));
      return true;
    }

    // After repeated HTTP failures or stale-heartbeat reconnects, fall back to
    // session-state polling to detect whether the run has finished while we
    // can't reach the event stream.  The resume loop keeps recovering forever;
    // once a connection goes bad for long enough, retryDelay slows to one
    // attempt per minute until a stream delivers bytes again.
    if (consecutiveHttpFailures >= 5 || consecutiveHeartbeatAborts >= 5) {
      consecutiveHttpFailures = 0;
      consecutiveHeartbeatAborts = 0;
      setConnectionState('Checking session state\u2026');
      // Temporarily clear the abort controller so syncActiveSessionFromServer
      // can act on the server state.  The !activeRun && !state.abortController
      // branch inside sync refuses to clear tracking while a controller is set,
      // but our own retry loop is the one that set it — creating a deadlock
      // where the loop never exits even after the server confirms the run is done.
      if (state.abortController) {
        state.abortController = null;
      }
      await app.syncActiveSessionFromServer(session, false);
      if (session.activeResponseId !== responseId) {
        // Run completed/changed while we were polling — exit.
        setStreaming(Boolean(state.currentStreamResponseId));
        return true;
      }
      // State poll may have failed (null) or the run is still active — either
      // way, continue the retry loop with backoff.
    }

    const controller = new AbortController();
    // Tag the controller so heartbeat vs intentional aborts are distinguishable,
    // including in browsers where AbortSignal.reason is not supported.
    controller._heartbeatAbort = false;
    attachResponseStream(session, responseId, controller);
    setStreaming(true);
    let streamActivityBaseline = Number(state.lastEventTime || 0);

    try {
      const response = await fetch(`${UI_PREFIX}/v1/responses/${encodeURIComponent(responseId)}/events?after=${encodeURIComponent(session.lastSequenceNumber || 0)}`, {
        headers: requestHeaders(session.id),
        signal: controller.signal
      });
      if (!response.ok) {
        throw await normalizeError(response);
      }
      if (!response.body) {
        throw { status: 0, message: 'No response body from server.' };
      }

      consecutiveHttpFailures = 0;
      setConnectionState('', '');
      const streamGeneration = state.streamGeneration;
      streamActivityBaseline = Number(state.lastEventTime || 0);
      const result = await consumeResponseStream(response.body, session, streamState, { generation: streamGeneration, responseId });
      if (streamHadActivitySince(streamActivityBaseline)) {
        retryAttempt = 0;
        consecutiveHeartbeatAborts = 0;
      }
      if (state.abortController === controller) {
        state.abortController = null;
      }

      if (result.stale) {
        setStreaming(Boolean(state.currentStreamResponseId));
        return false;
      }
      if (result.error?.recoverableStreamGap || result.error?.recoverableStreamApplyFailure) {
        const sequenceBeforeRecovery = Number(session.lastSequenceNumber || 0);
        try {
          const snapshot = await recoverResponseStateFromSnapshot(session, responseId);
          streamState = createResponseStreamState(session);
          if (snapshot?.status !== 'in_progress') {
            setStreaming(Boolean(state.currentStreamResponseId));
            return true;
          }
          if (Number(session.lastSequenceNumber || 0) > sequenceBeforeRecovery) {
            continue;
          }
        } catch (snapshotErr) {
          if (snapshotErr?.status === 401) {
            handleAuthFailure();
            return false;
          }
        }
      }
      if (session.activeResponseId !== responseId) {
        setStreaming(Boolean(state.currentStreamResponseId));
        return true;
      }
      if (result.terminal) {
        setStreaming(Boolean(state.currentStreamResponseId));
        return true;
      }
    } catch (err) {
      if (state.abortController === controller) {
        state.abortController = null;
      }

      const sawStreamActivity = streamHadActivitySince(streamActivityBaseline);
      if (sawStreamActivity) {
        retryAttempt = 0;
        consecutiveHttpFailures = 0;
        consecutiveHeartbeatAborts = 0;
      }

      const controllerAborted = Boolean(controller.signal?.aborted || err?.name === 'AbortError');
      if (controllerAborted) {
        // Only retry if this was a heartbeat-triggered abort.
        // Intentional detach/session-switch aborts should exit immediately.
        if (!controller._heartbeatAbort) {
          setStreaming(Boolean(state.currentStreamResponseId));
          return false;
        }
        // Heartbeat abort: fall through to retry without counting this as an
        // HTTP failure.  Some browsers reject aborted fetches with the custom
        // abort reason instead of a DOMException, so key off the controller.
        consecutiveHeartbeatAborts += 1;
      } else {
        consecutiveHttpFailures += 1;
        consecutiveHeartbeatAborts = 0;
      }
      if (err?.status === 401) {
        handleAuthFailure();
        return false;
      }
      if (err?.status === 409) {
        try {
          const snapshot = await recoverResponseStateFromSnapshot(session, responseId);
          streamState = createResponseStreamState(session);
          if (snapshot?.status !== 'in_progress') {
            setStreaming(Boolean(state.currentStreamResponseId));
            return true;
          }
          continue;
        } catch (snapshotErr) {
          if (snapshotErr?.status === 401) {
            handleAuthFailure();
            return false;
          }
          if (snapshotErr?.status === 404) {
            clearActiveResponseTracking(session, responseId);
            saveSessions();
            await app.syncActiveSessionFromServer(session, false);
            setStreaming(Boolean(state.currentStreamResponseId));
            return false;
          }
        }
      }
      if (err?.status === 404) {
        clearActiveResponseTracking(session, responseId);
        saveSessions();
        await app.syncActiveSessionFromServer(session, false);
        setStreaming(Boolean(state.currentStreamResponseId));
        return false;
      }
      if (session.activeResponseId !== responseId) {
        setStreaming(Boolean(state.currentStreamResponseId));
        return true;
      }
    }

    setConnectionState(streamReconnectLabel(retryAttempt));
    if (session.activeResponseId !== responseId) {
      setStreaming(Boolean(state.currentStreamResponseId));
      return true;
    }
    const retryDelay = streamReconnectDelay(retryAttempt);
    retryAttempt += 1;
    await sleep(retryDelay);
  }
};

const cancelActiveResponse = async (session) => {
  const responseId = String(session?.activeResponseId || state.currentStreamResponseId || '').trim();

  // Instant UI feedback: tear down the local stream before the server POST.
  // If the run is blocked in a tool (e.g. a shell tool hung on cmd.Wait),
  // the /cancel POST can take seconds to return. Aborting the reader here
  // makes Stop feel responsive; the POST below still drives the
  // authoritative server-side cancel.
  state.expectCanceledRun = true;
  if (state.abortController) {
    state.abortController.abort();
  }
  setConnectionState('Cancelling\u2026');

  if (!responseId) {
    console.warn('[cancel] no responseId available, aborting local controller only');
    if (session?.id) {
      await refreshSessionFromServerTruth(session, true);
    }
    return;
  }

  let response;
  try {
    response = await fetch(`${UI_PREFIX}/v1/responses/${encodeURIComponent(responseId)}/cancel`, {
      method: 'POST',
      headers: requestHeaders(session?.id || '')
    });
  } catch (err) {
    console.warn('[cancel] fetch failed for response', responseId, err);
    throw err;
  }
  if (!response.ok) {
    if (response.status === 404 || response.status === 409) {
      console.warn('[cancel] server returned', response.status, 'for response', responseId);
      if (session?.id) {
        await refreshSessionFromServerTruth(session, true);
      }
      return;
    }
    throw await normalizeError(response);
  }

  console.log('[cancel] server accepted cancel for response', responseId);
  if (session?.id) {
    app.scheduleSessionStatePoll(session.id, 250);
    app.refreshSidebarStatusPoll?.(true);
  }
};

// ===== ask_user modal =====
const closeAskUserModal = () => {
  state.askUser = null;
  elements.askUserModal.classList.add('hidden');
  elements.askUserModalBody.innerHTML = '';
  elements.askUserError.textContent = '';
  elements.askUserSubmitBtn.disabled = false;
  elements.askUserCancelBtn.disabled = false;
  elements.askUserSubmitBtn.textContent = 'Continue';
  elements.askUserCancelBtn.textContent = 'Dismiss';
};

const askUserSummaryFromAnswers = (answers) => {
  if (!Array.isArray(answers) || answers.length === 0) return '';
  return answers
    .map((answer) => {
      const header = String(answer?.header || '').trim();
      const selected = String(answer?.selected || '').trim();
      if (!header) return selected;
      return `${header}: ${selected}`;
    })
    .filter(Boolean)
    .join(' | ');
};

const collectAskUserAnswers = () => {
  const prompt = state.askUser;
  if (!prompt) {
    throw new Error('No pending question.');
  }
  const answers = [];
  prompt.questions.forEach((question, index) => {
    const name = `ask_user_${index}`;
    if (question.multi_select) {
      const selectedList = Array.from(elements.askUserModalBody.querySelectorAll(`input[name="${name}"]:checked`))
        .map((input) => String(input.value || '').trim())
        .filter(Boolean);
      if (selectedList.length === 0) {
        throw new Error(`${question.header || `Question ${index + 1}`}: choose at least one option.`);
      }
      answers.push({
        question_index: index,
        header: question.header,
        selected: selectedList.join(', '),
        selected_list: selectedList,
        is_custom: false,
        is_multi_select: true
      });
      return;
    }

    const selected = elements.askUserModalBody.querySelector(`input[name="${name}"]:checked`);
    if (!selected) {
      throw new Error(`${question.header || `Question ${index + 1}`}: choose an option.`);
    }
    if (selected.value === '__custom__') {
      const textarea = elements.askUserModalBody.querySelector(`#askUserCustom_${index}`);
      const custom = String(textarea?.value || '').trim();
      if (!custom) {
        throw new Error(`${question.header || `Question ${index + 1}`}: enter your answer.`);
      }
      answers.push({
        question_index: index,
        header: question.header,
        selected: custom,
        is_custom: true,
        is_multi_select: false
      });
      return;
    }
    answers.push({
      question_index: index,
      header: question.header,
      selected: String(selected.value || '').trim(),
      is_custom: false,
      is_multi_select: false
    });
  });
  return answers;
};

const validateSingleQuestion = (index) => {
  const question = state.askUser?.questions[index];
  if (!question) return;
  const name = `ask_user_${index}`;

  if (question.multi_select) {
    const checked = elements.askUserModalBody.querySelectorAll(`input[name="${name}"]:checked`);
    if (checked.length === 0) throw new Error('Choose at least one option.');
    return;
  }

  const selected = elements.askUserModalBody.querySelector(`input[name="${name}"]:checked`);
  if (!selected) throw new Error('Choose an option.');
  if (selected.value === '__custom__') {
    const textarea = elements.askUserModalBody.querySelector(`#askUserCustom_${index}`);
    const custom = String(textarea?.value || '').trim();
    if (!custom) throw new Error('Enter your answer.');
  }
};

const switchAskUserTab = (newIndex) => {
  const prompt = state.askUser;
  if (!prompt) return;
  const total = prompt.questions.length;
  if (newIndex < 0 || newIndex >= total) return;

  prompt.activeTab = newIndex;

  elements.askUserModalBody.querySelectorAll('.ask-user-question').forEach((section) => {
    const idx = parseInt(section.dataset.questionIndex, 10);
    section.style.display = idx === newIndex ? '' : 'none';
  });

  elements.askUserModalBody.querySelectorAll('.ask-user-step').forEach((step, i) => {
    step.classList.toggle('active', i === newIndex);
    step.classList.toggle('completed', i < newIndex);
  });
  elements.askUserModalBody.querySelectorAll('.ask-user-step-line').forEach((line, i) => {
    line.classList.toggle('done', i + 1 <= newIndex);
  });

  elements.askUserModalTitle.textContent = `Question ${newIndex + 1} of ${total}`;
  elements.askUserCancelBtn.textContent = newIndex > 0 ? 'Back' : 'Dismiss';
  elements.askUserSubmitBtn.textContent = newIndex < total - 1 ? 'Next' : 'Continue';
  elements.askUserError.textContent = '';

  const activeSection = elements.askUserModalBody.querySelector(`.ask-user-question[data-question-index="${newIndex}"]`);
  if (activeSection) {
    const firstInput = activeSection.querySelector('input, textarea');
    firstInput?.focus();
  }
};

const renderAskUserModal = () => {
  const prompt = state.askUser;
  if (!prompt) return;

  const total = prompt.questions.length;
  const activeTab = prompt.activeTab || 0;

  elements.askUserModalTitle.textContent = total === 1 ? 'Answer question' : `Question ${activeTab + 1} of ${total}`;
  elements.askUserModalSubtitle.textContent = 'The agent needs your input to continue.';
  elements.askUserModalBody.innerHTML = '';
  elements.askUserError.textContent = '';

  if (total > 1) {
    const steps = document.createElement('div');
    steps.className = 'ask-user-steps';
    for (let i = 0; i < total; i++) {
      if (i > 0) {
        const line = document.createElement('div');
        line.className = 'ask-user-step-line';
        if (i <= activeTab) line.classList.add('done');
        steps.appendChild(line);
      }
      const dot = document.createElement('button');
      dot.type = 'button';
      dot.className = 'ask-user-step';
      if (i === activeTab) dot.classList.add('active');
      else if (i < activeTab) dot.classList.add('completed');
      dot.textContent = i + 1;
      dot.addEventListener('click', () => switchAskUserTab(i));
      steps.appendChild(dot);
    }
    elements.askUserModalBody.appendChild(steps);
  }

  prompt.questions.forEach((question, index) => {
    const section = document.createElement('section');
    section.className = 'ask-user-question';
    section.dataset.questionIndex = index;
    if (index !== activeTab) section.style.display = 'none';

    const headerEl = document.createElement('div');
    headerEl.className = 'ask-user-question-header';
    headerEl.textContent = question.header || `Question ${index + 1}`;
    section.appendChild(headerEl);

    const textEl = document.createElement('p');
    textEl.className = 'ask-user-question-text';
    textEl.textContent = question.question || '';
    section.appendChild(textEl);

    const options = document.createElement('div');
    options.className = 'ask-user-options';
    const inputType = question.multi_select ? 'checkbox' : 'radio';
    const groupName = `ask_user_${index}`;

    (Array.isArray(question.options) ? question.options : []).forEach((option) => {
      const label = document.createElement('label');
      label.className = 'ask-user-option';

      const input = document.createElement('input');
      input.type = inputType;
      input.name = groupName;
      input.value = option.label || '';

      const copy = document.createElement('span');
      copy.className = 'ask-user-option-copy';

      const titleEl = document.createElement('span');
      titleEl.className = 'ask-user-option-title';
      titleEl.textContent = option.label || 'Option';

      copy.appendChild(titleEl);
      if (option.description) {
        const desc = document.createElement('span');
        desc.className = 'ask-user-option-desc';
        desc.textContent = option.description;
        copy.appendChild(desc);
      }

      label.appendChild(input);
      label.appendChild(copy);
      options.appendChild(label);
    });

    if (!question.multi_select) {
      const customLabel = document.createElement('label');
      customLabel.className = 'ask-user-option';

      const customRadio = document.createElement('input');
      customRadio.type = 'radio';
      customRadio.name = groupName;
      customRadio.value = '__custom__';

      const customCopy = document.createElement('span');
      customCopy.className = 'ask-user-option-copy';

      const customTitle = document.createElement('span');
      customTitle.className = 'ask-user-option-title';
      customTitle.textContent = 'Other';

      const customDesc = document.createElement('span');
      customDesc.className = 'ask-user-option-desc';
      customDesc.textContent = 'Type your own answer.';

      customCopy.appendChild(customTitle);
      customCopy.appendChild(customDesc);
      customLabel.appendChild(customRadio);
      customLabel.appendChild(customCopy);
      options.appendChild(customLabel);

      section.appendChild(options);

      const textarea = document.createElement('textarea');
      textarea.id = `askUserCustom_${index}`;
      textarea.className = 'ask-user-custom-input';
      textarea.placeholder = 'Type your answer\u2026';
      textarea.addEventListener('focus', () => {
        customRadio.checked = true;
        textarea.classList.add('visible');
      });

      section.addEventListener('change', () => {
        textarea.classList.toggle('visible', customRadio.checked);
        if (customRadio.checked) setTimeout(() => textarea.focus(), 0);
      });

      section.appendChild(textarea);
    } else {
      section.appendChild(options);
    }

    const note = document.createElement('div');
    note.className = 'ask-user-note';
    note.textContent = question.multi_select
      ? 'Choose one or more options to continue.'
      : 'Choose one option or provide a custom answer.';
    section.appendChild(note);
    elements.askUserModalBody.appendChild(section);
  });

  if (total > 1) {
    elements.askUserCancelBtn.textContent = activeTab > 0 ? 'Back' : 'Dismiss';
    elements.askUserSubmitBtn.textContent = activeTab < total - 1 ? 'Next' : 'Continue';
  } else {
    elements.askUserCancelBtn.textContent = 'Dismiss';
    elements.askUserSubmitBtn.textContent = 'Continue';
  }
};

const openAskUserModal = (sessionId, callId, questions) => {
  if (!sessionId || !callId || !Array.isArray(questions) || questions.length === 0) return;
  state.askUser = {
    sessionId,
    callId,
    activeTab: 0,
    questions: questions.map((question) => ({
      ...question,
      options: Array.isArray(question?.options) ? question.options.map((option) => ({ ...option })) : []
    }))
  };
  renderAskUserModal();
  elements.askUserModal.classList.remove('hidden');
  setTimeout(() => {
    const firstInput = elements.askUserModalBody.querySelector('input, textarea');
    firstInput?.focus();
  }, 0);
};

const submitAskUserModal = async (cancelled = false) => {
  const prompt = state.askUser;
  if (!prompt) return;

  const total = prompt.questions.length;
  const activeTab = prompt.activeTab || 0;

  // Multi-question: "Back" button (cancel on non-first tab goes back)
  if (cancelled && total > 1 && activeTab > 0) {
    switchAskUserTab(activeTab - 1);
    return;
  }

  // Multi-question: "Next" button (submit on non-last tab advances)
  if (!cancelled && total > 1 && activeTab < total - 1) {
    try {
      validateSingleQuestion(activeTab);
    } catch (err) {
      elements.askUserError.textContent = err?.message || 'Please answer the question.';
      return;
    }
    switchAskUserTab(activeTab + 1);
    return;
  }

  let answers = [];
  if (!cancelled) {
    try {
      answers = collectAskUserAnswers();
    } catch (err) {
      elements.askUserError.textContent = err?.message || 'Please answer all questions.';
      return;
    }
  }

  elements.askUserError.textContent = '';
  elements.askUserSubmitBtn.disabled = true;
  elements.askUserCancelBtn.disabled = true;
  elements.askUserSubmitBtn.textContent = cancelled ? 'Closing…' : 'Sending…';
  elements.askUserCancelBtn.textContent = cancelled ? 'Dismissing…' : 'Dismiss';

  try {
    const response = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(prompt.sessionId)}/ask_user`, {
      method: 'POST',
      headers: requestHeaders(prompt.sessionId),
      body: JSON.stringify(cancelled
        ? { call_id: prompt.callId, cancelled: true }
        : { call_id: prompt.callId, answers })
    });
    if (!response.ok) {
      throw await normalizeError(response);
    }
    const payload = await response.json().catch(() => ({}));
    if (!cancelled) {
      const session = state.sessions.find((item) => item.id === prompt.sessionId);
      if (session) {
        const normalized = Array.isArray(payload.answers) ? payload.answers : answers;
        const summary = String(payload.summary || askUserSummaryFromAnswers(normalized) || 'Answered prompt').trim();
        if (summary) {
          const message = {
            id: generateId('msg'),
            role: 'user',
            content: summary,
            created: Date.now(),
            askUser: true
          };
          session.messages.push(message);
          if (session.id === state.activeSessionId) {
            const empty = elements.messages.querySelector('.empty-state');
            if (empty) empty.remove();
            elements.messages.appendChild(createMessageNode(message));
          }
          saveSessions();
          scrollToBottom(true);
        }
      }
    }
    closeAskUserModal();
    if (!state.abortController) {
      setSessionOptimisticBusy(prompt.sessionId, true);
      setStreaming(true);
      persistAndRefreshShell();
      app.refreshSidebarStatusPoll?.();
      app.scheduleSessionStatePoll(prompt.sessionId, 400);
    }
  } catch (err) {
    if (err?.status === 409) {
      const session = state.sessions.find((item) => item.id === prompt.sessionId) || null;
      const runtimeState = session ? await refreshSessionFromServerTruth(session, true) : null;
      if (!runtimeHasPendingAskUser(runtimeState, prompt.callId)) {
        closeAskUserModal();
        return;
      }
    }

    elements.askUserError.textContent = err?.message || 'Failed to submit your answer.';
    if (err?.status === 401) {
      handleAuthFailure();
    }
    elements.askUserSubmitBtn.disabled = false;
    elements.askUserCancelBtn.disabled = false;
    elements.askUserSubmitBtn.textContent = 'Continue';
    elements.askUserCancelBtn.textContent = 'Dismiss';
  }
};

// ===== Approval modal =====
const openApprovalModal = (sessionId, approvalId, path, isShell, title, options) => {
  state.approval = { sessionId, approvalId, path, isShell, title, options, selectedIndex: 0 };

  elements.approvalTitle.textContent = title || 'Access Request';
  elements.approvalPath.textContent = path || '';
  elements.approvalError.textContent = '';
  elements.approvalApproveBtn.disabled = false;
  elements.approvalDenyBtn.disabled = false;
  elements.approvalApproveBtn.textContent = 'Approve';
  elements.approvalDenyBtn.textContent = 'Deny';

  // Build radio options as a vertical list
  const body = elements.approvalBody;
  body.innerHTML = '';
  const group = document.createElement('div');
  group.className = 'approval-options';
  options.forEach((opt, i) => {
    const label = document.createElement('label');
    label.className = 'approval-option';

    const radio = document.createElement('input');
    radio.type = 'radio';
    radio.name = 'approval_choice';
    radio.value = String(opt.index != null ? opt.index : i);
    if (i === 0) radio.checked = true;
    radio.addEventListener('change', () => { state.approval.selectedIndex = Number(radio.value); });

    const copy = document.createElement('div');
    copy.className = 'approval-option-copy';
    const titleEl = document.createElement('span');
    titleEl.className = 'approval-option-title';
    titleEl.textContent = opt.label || `Option ${i + 1}`;
    copy.appendChild(titleEl);
    if (opt.description) {
      const desc = document.createElement('span');
      desc.className = 'approval-option-desc';
      desc.textContent = opt.description;
      copy.appendChild(desc);
    }

    label.appendChild(radio);
    label.appendChild(copy);
    group.appendChild(label);
  });
  body.appendChild(group);

  elements.approvalModal.classList.remove('hidden');
  setTimeout(() => {
    const firstRadio = body.querySelector('input[type="radio"]');
    firstRadio?.focus();
  }, 0);
};

const closeApprovalModal = () => {
  state.approval = null;
  elements.approvalModal.classList.add('hidden');
  elements.approvalBody.innerHTML = '';
  elements.approvalError.textContent = '';
  elements.approvalApproveBtn.disabled = false;
  elements.approvalDenyBtn.disabled = false;
  elements.approvalApproveBtn.textContent = 'Approve';
  elements.approvalDenyBtn.textContent = 'Deny';
};

const submitApprovalModal = async (denied = false) => {
  const prompt = state.approval;
  if (!prompt) return;

  elements.approvalError.textContent = '';
  elements.approvalApproveBtn.disabled = true;
  elements.approvalDenyBtn.disabled = true;
  elements.approvalApproveBtn.textContent = denied ? 'Approve' : 'Sending…';
  elements.approvalDenyBtn.textContent = denied ? 'Denying…' : 'Deny';

  // Find the deny option by its choice field rather than assuming position.
  const denyOpt = prompt.options.find(o => o.choice === 'deny');
  const denyIndex = denyOpt ? denyOpt.index : prompt.options.length - 1;
  const choiceIndex = denied ? denyIndex : prompt.selectedIndex;
  const body = { approval_id: prompt.approvalId, choice: choiceIndex };

  try {
    const response = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(prompt.sessionId)}/approval`, {
      method: 'POST',
      headers: requestHeaders(prompt.sessionId),
      body: JSON.stringify(body)
    });
    if (!response.ok) {
      throw await normalizeError(response);
    }
    closeApprovalModal();
    if (!state.abortController) {
      setSessionOptimisticBusy(prompt.sessionId, true);
      setStreaming(true);
      persistAndRefreshShell();
      app.refreshSidebarStatusPoll?.();
      app.scheduleSessionStatePoll(prompt.sessionId, 400);
    }
  } catch (err) {
    if (err?.status === 409) {
      const session = state.sessions.find((item) => item.id === prompt.sessionId) || null;
      const runtimeState = session ? await refreshSessionFromServerTruth(session, true) : null;
      if (!runtimeHasPendingApproval(runtimeState, prompt.approvalId)) {
        closeApprovalModal();
        return;
      }
    }

    elements.approvalError.textContent = err?.message || 'Failed to submit approval.';
    if (err?.status === 401) {
      handleAuthFailure();
    }
    elements.approvalApproveBtn.disabled = false;
    elements.approvalDenyBtn.disabled = false;
    elements.approvalApproveBtn.textContent = 'Approve';
    elements.approvalDenyBtn.textContent = 'Deny';
  }
};

// ===== Settings modal =====
const openAuthModal = (errorText = '', required = !state.token) => {
  state.authRequired = required;
  elements.authError.textContent = errorText;
  elements.authTokenInput.value = state.token || '';
  elements.authCancelBtn.style.display = required ? 'none' : 'inline-flex';
  elements.providerSelect.value = state.selectedProvider;
  elements.modelSelect.value = state.selectedModel;
  if (elements.effortSelect) {
    elements.effortSelect.value = state.selectedEffort;
  }
  if (elements.showHiddenSessionsInput) {
    elements.showHiddenSessionsInput.checked = state.showHiddenSessions;
  }
  if (elements.showWidgetsSidebarInput) {
    elements.showWidgetsSidebarInput.checked = state.showWidgetsSidebar !== false;
  }
  app.refreshNotificationUI();
  elements.authModal.classList.remove('hidden');
  elements.providerSelect.removeAttribute('tabindex');
  elements.modelSelect.removeAttribute('tabindex');
  elements.effortSelect?.removeAttribute('tabindex');
  elements.authTokenInput.removeAttribute('tabindex');
  elements.showHiddenSessionsInput?.removeAttribute('tabindex');
  elements.showWidgetsSidebarInput?.removeAttribute('tabindex');

  setTimeout(() => {
    if (required) {
      elements.authTokenInput.focus();
      elements.authTokenInput.select();
    }
  }, 0);
};

const closeAuthModal = () => {
  if (state.authRequired && !state.token) return;
  elements.authModal.classList.add('hidden');
  elements.authError.textContent = '';
  elements.providerSelect.setAttribute('tabindex', '-1');
  elements.modelSelect.setAttribute('tabindex', '-1');
  elements.effortSelect?.setAttribute('tabindex', '-1');
  elements.authTokenInput.setAttribute('tabindex', '-1');
  elements.showHiddenSessionsInput?.setAttribute('tabindex', '-1');
  elements.showWidgetsSidebarInput?.setAttribute('tabindex', '-1');
};

const handleAuthFailure = () => {
  app.stopSessionStatePoll();
  closeAskUserModal();
  state.token = '';
  localStorage.removeItem(STORAGE_KEYS.token);
  syncTokenCookie('');
  setConnectionState('Not connected', 'bad');
  openAuthModal('Auth failed — check your token.', true);
};

const connectToken = async () => {
  const token = elements.authTokenInput.value.trim();
  const nextShowHiddenSessions = Boolean(elements.showHiddenSessionsInput?.checked);
  const nextShowWidgetsSidebar = elements.showWidgetsSidebarInput ? Boolean(elements.showWidgetsSidebarInput.checked) : true;

  // Provider/model selections are committed live via the change handlers.
  // Re-reading the modal DOM here can clobber a valid in-memory choice if the
  // selects are temporarily stale (for example while startup/model refresh work
  // is still settling). Persist the current state instead.
  const persistedProvider = state.selectedProvider;
  const persistedModel = state.selectedModel;
  const newEffort = elements.effortSelect ? elements.effortSelect.value : '';
  state.selectedProvider = persistedProvider;
  state.selectedModel = persistedModel;
  state.selectedEffort = newEffort;
  canonicalizeSelectedModelEffort();
  persistRuntimeSelection();
  const showHiddenChanged = nextShowHiddenSessions !== state.showHiddenSessions;
  state.showHiddenSessions = nextShowHiddenSessions;
  localStorage.setItem(STORAGE_KEYS.showHiddenSessions, state.showHiddenSessions ? '1' : '0');
  const showWidgetsChanged = nextShowWidgetsSidebar !== (state.showWidgetsSidebar !== false);
  state.showWidgetsSidebar = nextShowWidgetsSidebar;
  localStorage.setItem(STORAGE_KEYS.showWidgetsSidebar, state.showWidgetsSidebar ? '1' : '0');
  if (showWidgetsChanged && app.renderWidgetSidebar) app.renderWidgetSidebar();
  app.updateHeader();

  if (state.authRequired && !token) {
    elements.authError.textContent = 'Token is required.';
    return;
  }

  const tokenChanged = token !== state.token;
  if (!tokenChanged) {
    renderEffortOptions();
    if (showHiddenChanged && state.connected) {
      void app.mergeServerSessions({ includeArchived: state.showHiddenSessions }).then(() => {
        renderSidebar();
      });
    } else {
      renderSidebar();
    }
    closeAuthModal();
    return;
  }

  elements.authConnectBtn.disabled = true;
  elements.authConnectBtn.textContent = 'Saving…';
  elements.authError.textContent = '';

  try {
    // Speculative models fetch in parallel with providers — same pattern as startup.
    const speculativeProvider = state.selectedProvider;
    const speculativeModelsPromise = speculativeProvider
      ? fetchModels(token, speculativeProvider)
      : null;

    state.providers = await fetchProviders(token);
    normalizeSelectedProvider();

    let models;
    if (speculativeModelsPromise !== null && state.selectedProvider === speculativeProvider) {
      models = await speculativeModelsPromise;
    } else {
      if (speculativeModelsPromise !== null) speculativeModelsPromise.catch(() => {});
      models = await fetchModels(token, state.selectedProvider);
    }
    state.token = token;
    state.models = models;
    state.connected = true;
    localStorage.setItem(STORAGE_KEYS.token, token);
    syncTokenCookie(token);

    renderProviderOptions();
    renderModelOptions();
    setConnectionState('', '');
    state.authRequired = false;
    closeAuthModal();
    if (showHiddenChanged) {
      void app.mergeServerSessions({ includeArchived: state.showHiddenSessions }).then(() => {
        renderSidebar();
      });
    }

    // Retry push enrollment now that we have a valid token. Also recover if the
    // browser permission was already granted but the old client-side flag was missing.
    if (shouldAutoSubscribeToPush()) {
      subscribeToPush();
    }

    const active = getActiveSession();
    if (active) {
      await app.syncActiveSessionFromServer(active, true);
    }
  } catch (err) {
    const message = err?.message || 'Unable to validate token.';
    elements.authError.textContent = message;
    if (err?.status === 401) {
      state.token = '';
      localStorage.removeItem(STORAGE_KEYS.token);
      syncTokenCookie('');
    }
    setConnectionState('Not connected', 'bad');
  } finally {
    elements.authConnectBtn.disabled = false;
    elements.authConnectBtn.textContent = 'Save';
  }
};

// ===== Provider picker =====

// Clear stale selectedProvider if it no longer exists in the fetched provider list.
const normalizeSelectedProvider = () => {
  if (!state.selectedProvider) return;
  const exists = state.providers.some((p) => p.name === state.selectedProvider);
  if (!exists) {
    state.selectedProvider = '';
    localStorage.removeItem(STORAGE_KEYS.selectedProvider);
  }
};

const populateProviderSelectOptions = (sel, providers, previous) => {
  if (!sel) return;
  sel.innerHTML = '';

  const autoOption = document.createElement('option');
  autoOption.value = '';
  autoOption.textContent = 'Auto (server default)';
  sel.appendChild(autoOption);

  providers.filter((p) => p.configured || p.is_default).forEach((p) => {
    const option = document.createElement('option');
    option.value = p.name;
    option.textContent = p.name + (p.is_default ? ' (default)' : '');
    sel.appendChild(option);
  });

  sel.value = previous;
};

const renderProviderOptions = () => {
  const previous = state.selectedProvider;
  populateProviderSelectOptions(elements.providerSelect, state.providers, previous);
  populateProviderSelectOptions(elements.chipProviderSelect, state.providers, previous);
};

let providerChangeSequence = 0;

const applyProviderChange = async (provider) => {
  const changeSequence = ++providerChangeSequence;
  state.selectedProvider = provider;
  if (provider) {
    localStorage.setItem(STORAGE_KEYS.selectedProvider, provider);
  } else {
    localStorage.removeItem(STORAGE_KEYS.selectedProvider);
  }
  state.selectedModel = '';
  localStorage.removeItem(STORAGE_KEYS.selectedModel);

  const providerInfo = state.providers.find((p) => p.name === provider);
  state.models = providerInfo?.models?.length ? providerInfo.models : [];
  state.modelInfoByID = {};
  renderModelOptions();

  // Reflect the clicked provider immediately. Fetching the model list can be
  // slow, and the header chip should not keep showing the previous provider
  // while that async refresh is in flight. Rendering the provider's configured
  // model fallback (or an empty list) also avoids briefly exposing stale models
  // from the previously selected provider.
  syncSettingsSelectValues();
  app.updateHeader();

  let models;
  try {
    models = await fetchModels('', provider);
  } catch {
    models = providerInfo?.models?.length ? providerInfo.models : [];
  }
  if (changeSequence !== providerChangeSequence || state.selectedProvider !== provider) {
    return;
  }
  state.models = models;
  renderModelOptions();
  syncSettingsSelectValues();
  app.updateHeader();
};

const resolveEffectiveModelForEffort = (model, effort) => {
  const split = splitHeaderModelEffort(model || '', effort || '', state.models);
  if (split.model) return split.model;
  const provider = state.selectedProvider || getDefaultProviderName?.() || '';
  return getDefaultModelForProvider?.(provider) || '';
};

const effectiveModelForEffort = () => resolveEffectiveModelForEffort(state.selectedModel, state.selectedEffort);

const modelMetadataFor = (model) => {
  const id = String(model || '').trim();
  if (!id || !state.modelInfoByID) return null;
  return Object.prototype.hasOwnProperty.call(state.modelInfoByID, id) ? state.modelInfoByID[id] : null;
};

const reasoningEffortsForModel = (model) => {
  const info = modelMetadataFor(model);
  return Array.isArray(info?.reasoning_efforts)
    ? info.reasoning_efforts.map((v) => String(v || '').trim()).filter(Boolean)
    : [];
};

const LEGACY_REASONING_EFFORTS = ['minimal', 'low', 'medium', 'high', 'xhigh', 'max'];

const allowedReasoningEffortsForSelection = () => {
  const model = effectiveModelForEffort();
  const info = modelMetadataFor(model);
  const efforts = reasoningEffortsForModel(model);
  if (!info || efforts.length === 0) return LEGACY_REASONING_EFFORTS;
  return efforts;
};

const populateEffortSelectOptions = (sel, efforts, previous) => {
  if (!sel) return;
  sel.innerHTML = '';

  const autoOption = document.createElement('option');
  autoOption.value = '';
  autoOption.textContent = 'Auto (server default)';
  sel.appendChild(autoOption);

  efforts.forEach((effort) => {
    const option = document.createElement('option');
    option.value = effort;
    option.textContent = effort;
    sel.appendChild(option);
  });

  sel.disabled = efforts.length === 0;
  sel.value = efforts.includes(previous) ? previous : '';
};

const renderEffortOptions = () => {
  const efforts = allowedReasoningEffortsForSelection();
  const previous = state.selectedEffort || '';
  populateEffortSelectOptions(elements.effortSelect, efforts, previous);
  populateEffortSelectOptions(elements.chipEffortSelect, efforts, previous);
};

const persistRuntimeSelection = () => {
  const persist = (key, value) => {
    if (value) {
      localStorage.setItem(key, value);
    } else {
      localStorage.removeItem(key);
    }
  };
  persist(STORAGE_KEYS.selectedProvider, state.selectedProvider || '');
  persist(STORAGE_KEYS.selectedModel, state.selectedModel || '');
  persist(STORAGE_KEYS.selectedEffort, state.selectedEffort || '');
};

const canonicalizeSelectedModelEffort = () => {
  const split = splitHeaderModelEffort(state.selectedModel, state.selectedEffort, state.models);
  let nextModel = split.model;
  let nextEffort = split.effort;
  const effectiveModel = resolveEffectiveModelForEffort(nextModel, nextEffort);
  const info = modelMetadataFor(effectiveModel);
  const allowed = info ? reasoningEffortsForModel(effectiveModel) : [];
  if (nextEffort && info && allowed.length > 0 && !allowed.includes(nextEffort)) {
    nextEffort = '';
  }
  if (nextModel === (state.selectedModel || '') && nextEffort === (state.selectedEffort || '')) {
    return false;
  }
  state.selectedModel = nextModel;
  state.selectedEffort = nextEffort;
  persistRuntimeSelection();
  return true;
};

const applyModelChange = (model) => {
  state.selectedModel = model;
  canonicalizeSelectedModelEffort();
  renderEffortOptions();
  persistRuntimeSelection();
  syncSettingsSelectValues();
  app.updateHeader();
};

const queueActiveRunEffortChange = async (session, effort) => {
  const targetEffort = String(effort || '').trim();
  const model = String(session?.activeModel || state.selectedModel || '').trim();
  if (!session || !session.id || !model) return false;

  const response = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(session.id)}/runtime/effort`, {
    method: 'POST',
    headers: requestHeaders(session.id),
    body: JSON.stringify({
      model,
      reasoning_effort: targetEffort,
    }),
  });
  if (!response.ok) {
    throw await normalizeError(response);
  }
  const payload = await response.json().catch(() => ({}));
  const queuedEffort = Object.prototype.hasOwnProperty.call(payload || {}, 'reasoning_effort')
    ? String(payload.reasoning_effort || '').trim()
    : targetEffort;

  state.selectedEffort = queuedEffort;
  canonicalizeSelectedModelEffort();
  persistRuntimeSelection();
  syncSettingsSelectValues();

  if (normalizeEffortForCompare(queuedEffort) === normalizeEffortForCompare(session.activeEffort || '')) {
    clearSessionPendingEffort(session);
  } else {
    setSessionPendingEffort(session, queuedEffort);
  }
  updateSessionUsageDisplay(session);
  app.updateHeader();
  return true;
};

const applyEffortChange = async (effort) => {
  const session = getActiveSession();
  if (sessionHasQueueableActiveRun(session)) {
    try {
      const queued = await queueActiveRunEffortChange(session, effort);
      if (queued) return;
    } catch (err) {
      const message = err?.message || 'Failed to queue reasoning effort.';
      if (session) addErrorMessage(message, session);
      syncSettingsSelectValues();
      app.updateHeader();
      return;
    }
  }
  state.selectedEffort = effort;
  canonicalizeSelectedModelEffort();
  persistRuntimeSelection();
  syncSettingsSelectValues();
  app.updateHeader();
};

// Keep modal selects mirroring the live state so opening the settings cog never
// shows a stale value vs. what the header chips committed.
const syncSettingsSelectValues = () => {
  if (elements.providerSelect) elements.providerSelect.value = state.selectedProvider || '';
  if (elements.modelSelect) elements.modelSelect.value = state.selectedModel || '';
  if (elements.effortSelect) elements.effortSelect.value = state.selectedEffort || '';
  if (elements.chipProviderSelect) elements.chipProviderSelect.value = state.selectedProvider || '';
  if (elements.chipModelSelect) elements.chipModelSelect.value = state.selectedModel || '';
  if (elements.chipEffortSelect) elements.chipEffortSelect.value = state.selectedEffort || '';
};

elements.providerSelect.addEventListener('change', () => {
  void applyProviderChange(elements.providerSelect.value);
});

elements.modelSelect?.addEventListener('change', () => {
  applyModelChange(elements.modelSelect.value);
});

// Modal effort intentionally has no change listener: Cancel must discard the
// pending value. The settings modal commits effort only on Save (connectToken).
// The header chip below commits live, matching provider/model behavior.

elements.chipProviderSelect?.addEventListener('change', () => {
  void applyProviderChange(elements.chipProviderSelect.value);
});

elements.chipModelSelect?.addEventListener('change', () => {
  applyModelChange(elements.chipModelSelect.value);
});

elements.chipEffortSelect?.addEventListener('change', () => {
  void applyEffortChange(elements.chipEffortSelect.value);
});

// ===== Custom chip popover =====
// Replaces the native <select> dropdown UI: native pickers are inconsistent
// across OSes, ugly, and can render off-screen. The underlying <select> is kept
// for state/sync — popover items dispatch a 'change' event on it on selection.
const chipPopoverState = { selectEl: null, triggerEl: null, filterInput: null, mode: '' };

const buildChipOptionLabel = (opt) => {
  const text = opt.textContent || opt.value || '';
  const value = opt.value || '';
  if (!value) {
    return { primary: text, meta: '' };
  }
  const defaultMatch = text.match(/^(.*?)\s*\((.+)\)\s*$/);
  if (defaultMatch) {
    return { primary: defaultMatch[1], meta: defaultMatch[2] };
  }
  return { primary: text, meta: '' };
};

const positionChipPopover = (triggerEl) => {
  const pop = elements.chipPopover;
  if (!pop || !triggerEl?.getBoundingClientRect) return;
  pop.hidden = false;

  const vv = window.visualViewport;
  const viewportWidth = vv ? Math.round(vv.width) : window.innerWidth;
  const viewportHeight = vv ? Math.round(vv.height) : window.innerHeight;
  const viewportOffsetLeft = vv ? Math.max(0, Math.round(vv.offsetLeft)) : 0;
  const viewportOffsetTop = vv ? Math.max(0, Math.round(vv.offsetTop)) : 0;

  if (viewportWidth <= 540) {
    // On iPhone Safari the on-screen keyboard shrinks the visual viewport, but
    // CSS vh units and fixed bottom sheets can still end up underneath it. Pin
    // the picker to the visible viewport instead of the layout viewport so the
    // whole sheet stays inside the safe area while typing in the filter box.
    pop.style.left = `calc(${viewportOffsetLeft}px + 0.5rem + var(--safe-left))`;
    pop.style.top = `calc(${viewportOffsetTop}px + 0.5rem + var(--safe-top))`;
    pop.style.right = 'auto';
    pop.style.bottom = 'auto';
    pop.style.width = `calc(${viewportWidth}px - 1rem - var(--safe-left) - var(--safe-right))`;
    pop.style.minWidth = '';
    pop.style.maxWidth = 'none';
    pop.style.maxHeight = `calc(${viewportHeight}px - 1rem - var(--safe-top) - var(--safe-bottom))`;
    return;
  }

  pop.style.width = '';
  const rect = triggerEl.getBoundingClientRect();
  const margin = 6;
  pop.style.minWidth = `${Math.max(180, rect.width)}px`;
  pop.style.maxWidth = '';
  pop.style.right = 'auto';
  pop.style.bottom = 'auto';
  const popRect = pop.getBoundingClientRect();
  let left = rect.left;
  if (left + popRect.width > window.innerWidth - margin) {
    left = Math.max(margin, window.innerWidth - margin - popRect.width);
  }
  let top = rect.bottom + 4;
  if (top + popRect.height > window.innerHeight - margin) {
    const above = rect.top - 4 - popRect.height;
    top = above >= margin ? above : Math.max(margin, window.innerHeight - margin - popRect.height);
  }
  pop.style.left = `${Math.max(margin, left)}px`;
  pop.style.top = `${Math.max(margin, top)}px`;
  pop.style.maxHeight = '';
};

const closeChipPopover = () => {
  const pop = elements.chipPopover;
  if (!pop || pop.hidden) return;
  pop.hidden = true;
  pop.innerHTML = '';
  pop.classList?.remove('chip-popover-runtime');
  if (elements.chipPopoverBackdrop) elements.chipPopoverBackdrop.hidden = true;
  if (chipPopoverState.triggerEl) {
    chipPopoverState.triggerEl.setAttribute('aria-expanded', 'false');
  }
  chipPopoverState.selectEl = null;
  chipPopoverState.triggerEl = null;
  chipPopoverState.filterInput = null;
  chipPopoverState.mode = '';
};

const focusChipPopoverItem = (item) => {
  if (!item) return;
  const pop = elements.chipPopover;
  pop?.querySelectorAll?.('.chip-popover-item.focused').forEach((el) => {
    el.classList.remove('focused');
  });
  item.classList.add('focused');
  item.focus?.({ preventScroll: false });
};

// Items matching the active filter (or all items when no filter is shown).
// Keyboard navigation skips items hidden by the filter.
const visibleChipPopoverItems = () => {
  const pop = elements.chipPopover;
  const items = pop?.querySelectorAll?.('.chip-popover-item');
  if (!items) return [];
  return Array.from(items).filter((el) => !el.hidden);
};

const moveChipPopoverFocus = (direction) => {
  const pop = elements.chipPopover;
  if (!pop) return;
  const items = visibleChipPopoverItems();
  if (items.length === 0) return;
  const current = pop.querySelector('.chip-popover-item.focused')
    || pop.querySelector('.chip-popover-item[aria-selected="true"]');
  let idx = current ? items.indexOf(current) : -1;
  idx = idx + direction;
  if (idx < 0) idx = items.length - 1;
  if (idx >= items.length) idx = 0;
  focusChipPopoverItem(items[idx]);
};

// Show this many items before adding a filter input. Below this threshold the
// filter just adds noise to small pickers (effort, provider list).
const CHIP_POPOVER_FILTER_THRESHOLD = 10;

const applyChipPopoverFilter = (query) => {
  const pop = elements.chipPopover;
  if (!pop) return;
  const q = (query || '').trim().toLowerCase();
  const items = pop.querySelectorAll?.('.chip-popover-item') || [];
  let firstVisible = null;
  items.forEach((el) => {
    const haystack = el.dataset?.search || '';
    const match = !q || haystack.includes(q);
    el.hidden = !match;
    if (match && !firstVisible) firstVisible = el;
  });
  // Re-focus the first visible item so Enter/ArrowDown work intuitively after
  // typing — without this, focus could be on a now-hidden item.
  pop.querySelectorAll('.chip-popover-item.focused').forEach((el) => {
    if (el.hidden) el.classList.remove('focused');
  });
  if (firstVisible && !pop.querySelector('.chip-popover-item.focused')) {
    firstVisible.classList.add('focused');
  }
};

const commitChipPopoverItem = (item) => {
  const selectEl = chipPopoverState.selectEl;
  if (!item || !selectEl) return;
  const value = item.dataset.value || '';
  if (selectEl.value !== value) {
    selectEl.value = value;
    selectEl.dispatchEvent(new Event('change', { bubbles: true }));
  }
  closeChipPopover();
};

const openChipPopover = (selectEl, triggerEl) => {
  const pop = elements.chipPopover;
  if (!pop || !selectEl) return;
  if (chipPopoverState.triggerEl === triggerEl) {
    closeChipPopover();
    return;
  }
  closeChipPopover();
  pop.classList?.remove('chip-popover-runtime');
  chipPopoverState.mode = 'select';
  chipPopoverState.selectEl = selectEl;
  chipPopoverState.triggerEl = triggerEl;
  pop.innerHTML = '';

  const options = Array.from(selectEl.options);
  let filterInput = null;
  if (options.length > CHIP_POPOVER_FILTER_THRESHOLD) {
    filterInput = document.createElement('input');
    filterInput.type = 'text';
    filterInput.className = 'chip-popover-filter';
    filterInput.placeholder = 'Filter…';
    filterInput.setAttribute('aria-label', 'Filter options');
    filterInput.setAttribute('autocomplete', 'off');
    filterInput.setAttribute('spellcheck', 'false');
    filterInput.addEventListener('input', () => applyChipPopoverFilter(filterInput.value));
    filterInput.addEventListener('keydown', (e) => {
      switch (e.key) {
        case 'ArrowDown':
          e.preventDefault();
          moveChipPopoverFocus(1);
          return;
        case 'ArrowUp':
          e.preventDefault();
          moveChipPopoverFocus(-1);
          return;
        case 'Enter': {
          e.preventDefault();
          const focused = pop.querySelector('.chip-popover-item.focused');
          if (focused && !focused.hidden) commitChipPopoverItem(focused);
          return;
        }
        case 'Escape':
          e.preventDefault();
          closeChipPopover();
          chipPopoverState.triggerEl?.focus?.();
          return;
      }
    });
    chipPopoverState.filterInput = filterInput;
    pop.appendChild(filterInput);
  } else {
    chipPopoverState.filterInput = null;
  }

  const currentValue = selectEl.value;
  options.forEach((opt) => {
    const item = document.createElement('div');
    item.className = 'chip-popover-item';
    item.setAttribute('role', 'option');
    item.tabIndex = -1;
    item.dataset.value = opt.value;
    const { primary, meta } = buildChipOptionLabel(opt);
    item.dataset.search = `${primary} ${meta} ${opt.value}`.toLowerCase();
    if (opt.value === currentValue) item.setAttribute('aria-selected', 'true');
    const label = document.createElement('span');
    label.className = 'chip-popover-item-label';
    label.textContent = primary;
    item.appendChild(label);
    if (meta) {
      const metaEl = document.createElement('span');
      metaEl.className = 'chip-popover-item-meta';
      metaEl.textContent = meta;
      item.appendChild(metaEl);
    }
    item.addEventListener('click', () => commitChipPopoverItem(item));
    item.addEventListener('mouseenter', () => focusChipPopoverItem(item));
    pop.appendChild(item);
  });
  triggerEl.setAttribute('aria-expanded', 'true');
  if (elements.chipPopoverBackdrop) elements.chipPopoverBackdrop.hidden = false;
  positionChipPopover(triggerEl);
  const initial = pop.querySelector('.chip-popover-item[aria-selected="true"]')
    || pop.querySelector('.chip-popover-item');
  focusChipPopoverItem(initial);
  // Focus the filter input last so the user can type immediately. The selected
  // item is still highlighted (visually focused) without stealing input focus.
  if (filterInput) filterInput.focus?.();
};

const isRuntimePickerCompressed = () => {
  const providerChip = elements.chipProviderTrigger?.closest?.('.model-chip');
  if (!providerChip || !window.getComputedStyle) return false;
  return window.getComputedStyle(providerChip).display === 'none';
};

const copySelectOptions = (from, to, formatOption = null) => {
  to.innerHTML = '';
  Array.from(from?.options || []).forEach((opt) => {
    const clone = document.createElement('option');
    clone.value = opt.value;
    clone.textContent = formatOption ? formatOption(opt) : opt.textContent;
    clone.disabled = opt.disabled;
    to.appendChild(clone);
  });
};

const runtimeField = ({ label, value, sourceSelect, onChange, formatOption = null }) => {
  const field = document.createElement('label');
  field.className = 'runtime-popover-field';

  const labelEl = document.createElement('span');
  labelEl.className = 'runtime-popover-label';
  labelEl.textContent = label;
  field.appendChild(labelEl);

  const select = document.createElement('select');
  select.className = 'runtime-popover-select';
  copySelectOptions(sourceSelect, select, formatOption);
  select.value = value || '';
  select.addEventListener('change', async () => {
    select.disabled = true;
    try {
      await onChange(select.value);
    } finally {
      if (chipPopoverState.mode === 'runtime') renderRuntimePopoverContent();
    }
  });
  field.appendChild(select);
  return field;
};

const renderRuntimePopoverContent = () => {
  const pop = elements.chipPopover;
  if (!pop) return;
  pop.innerHTML = '';

  const header = document.createElement('div');
  header.className = 'runtime-popover-header';
  const title = document.createElement('div');
  title.className = 'runtime-popover-title';
  title.textContent = 'Runtime';
  const hint = document.createElement('div');
  hint.className = 'runtime-popover-hint';
  hint.textContent = 'Provider, model, and effort for the next reply';
  header.appendChild(title);
  header.appendChild(hint);
  pop.appendChild(header);

  const fields = document.createElement('div');
  fields.className = 'runtime-popover-fields';
  fields.appendChild(runtimeField({
    label: 'Provider',
    value: state.selectedProvider || '',
    sourceSelect: elements.chipProviderSelect,
    onChange: (value) => applyProviderChange(value),
  }));
  fields.appendChild(runtimeField({
    label: 'Model',
    value: state.selectedModel || '',
    sourceSelect: elements.chipModelSelect,
    onChange: (value) => applyModelChange(value),
    formatOption: (opt) => opt.value ? compactHeaderModelLabel(opt.value) : opt.textContent,
  }));
  fields.appendChild(runtimeField({
    label: 'Effort',
    value: state.selectedEffort || '',
    sourceSelect: elements.chipEffortSelect,
    onChange: (value) => applyEffortChange(value),
  }));
  pop.appendChild(fields);
};

const openRuntimePopover = (triggerEl) => {
  const pop = elements.chipPopover;
  if (!pop || !triggerEl) return;
  if (chipPopoverState.mode === 'runtime' && chipPopoverState.triggerEl === triggerEl) {
    closeChipPopover();
    return;
  }
  closeChipPopover();
  chipPopoverState.mode = 'runtime';
  chipPopoverState.selectEl = null;
  chipPopoverState.triggerEl = triggerEl;
  chipPopoverState.filterInput = null;
  pop.classList?.add('chip-popover-runtime');
  renderRuntimePopoverContent();
  triggerEl.setAttribute('aria-expanded', 'true');
  if (elements.chipPopoverBackdrop) elements.chipPopoverBackdrop.hidden = false;
  pop.hidden = false;
  positionChipPopover(triggerEl);
  // Leave focus on the trigger. Focusing a native <select> here can open the OS
  // picker immediately, making the runtime panel feel like dueling modals.
};

elements.chipPopoverBackdrop?.addEventListener('click', () => {
  closeChipPopover();
});

const wireChipTrigger = (triggerEl, selectEl) => {
  if (!triggerEl || !selectEl) return;
  triggerEl.addEventListener('click', (e) => {
    e.stopPropagation();
    if (triggerEl === elements.chipModelTrigger && isRuntimePickerCompressed()) {
      openRuntimePopover(triggerEl);
      return;
    }
    openChipPopover(selectEl, triggerEl);
  });
};

wireChipTrigger(elements.chipProviderTrigger, elements.chipProviderSelect);
wireChipTrigger(elements.chipModelTrigger, elements.chipModelSelect);
wireChipTrigger(elements.chipEffortTrigger, elements.chipEffortSelect);

document.addEventListener('click', (e) => {
  const pop = elements.chipPopover;
  if (!pop || pop.hidden) return;
  if (pop.contains?.(e.target)) return;
  if (chipPopoverState.triggerEl?.contains?.(e.target)) return;
  closeChipPopover();
});

document.addEventListener('keydown', (e) => {
  const pop = elements.chipPopover;
  if (!pop || pop.hidden) return;
  if (chipPopoverState.mode === 'runtime') {
    if (e.key === 'Escape') {
      e.preventDefault();
      closeChipPopover();
      chipPopoverState.triggerEl?.focus?.();
    }
    return;
  }
  // The filter input owns its own keydown handler for navigation/commit. Don't
  // run the document-level handler when it's focused — otherwise Space would be
  // preventDefault'd and the user couldn't type spaces.
  if (e.target === chipPopoverState.filterInput) return;
  switch (e.key) {
    case 'Escape':
      e.preventDefault();
      closeChipPopover();
      chipPopoverState.triggerEl?.focus?.();
      return;
    case 'ArrowDown':
      e.preventDefault();
      moveChipPopoverFocus(1);
      return;
    case 'ArrowUp':
      e.preventDefault();
      moveChipPopoverFocus(-1);
      return;
    case 'Home': {
      e.preventDefault();
      const items = visibleChipPopoverItems();
      focusChipPopoverItem(items[0]);
      return;
    }
    case 'End': {
      e.preventDefault();
      const items = visibleChipPopoverItems();
      focusChipPopoverItem(items[items.length - 1]);
      return;
    }
    case 'Enter':
    case ' ': {
      e.preventDefault();
      const focused = pop.querySelector('.chip-popover-item.focused');
      if (focused && !focused.hidden) commitChipPopoverItem(focused);
      return;
    }
    case 'Tab':
      closeChipPopover();
      return;
  }
});

const repositionChipPopover = () => {
  if (chipPopoverState.triggerEl) positionChipPopover(chipPopoverState.triggerEl);
};

window.addEventListener('resize', repositionChipPopover);
window.addEventListener('orientationchange', repositionChipPopover);
if (window.visualViewport) {
  window.visualViewport.addEventListener('resize', repositionChipPopover);
  window.visualViewport.addEventListener('scroll', repositionChipPopover);
}

// ===== Model picker =====
const populateModelSelectOptions = (sel, models, previous) => {
  if (!sel) return;
  sel.innerHTML = '';

  const autoOption = document.createElement('option');
  autoOption.value = '';
  autoOption.textContent = 'Auto (server default)';
  sel.appendChild(autoOption);

  models.forEach((id) => {
    const option = document.createElement('option');
    option.value = id;
    option.textContent = id;
    sel.appendChild(option);
  });

  if (previous && !models.includes(previous)) {
    const custom = document.createElement('option');
    custom.value = previous;
    custom.textContent = `${previous} (custom)`;
    sel.appendChild(custom);
  }

  sel.value = previous;
};

const renderModelOptions = () => {
  canonicalizeSelectedModelEffort();
  const previous = state.selectedModel;
  populateModelSelectOptions(elements.modelSelect, state.models, previous);
  populateModelSelectOptions(elements.chipModelSelect, state.models, previous);
  renderEffortOptions();
};

// ===== Composer logic =====
const formatVoiceDuration = (ms) => {
  const totalSeconds = Math.max(0, Math.floor(ms / 1000));
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  return `${minutes}:${String(seconds).padStart(2, '0')}`;
};

const stopVoiceTracks = () => {
  const stream = state.voice.stream;
  if (!stream) return;
  stream.getTracks().forEach((track) => track.stop());
  state.voice.stream = null;
};

const clearVoiceTimer = () => {
  if (state.voice.timerId !== null) {
    clearInterval(state.voice.timerId);
    state.voice.timerId = null;
  }
};

const setVoiceStatus = (message = '') => {
  state.voice.status = String(message || '');
  const el = elements.voiceStatus;
  if (!el) return;
  if (!state.voice.status) {
    el.className = 'voice-status hidden';
    el.innerHTML = '';
    return;
  }
  el.className = 'voice-status';
  el.innerHTML = state.voice.status;
};

const updateVoiceUI = () => {
  const btn = elements.voiceBtn;
  if (!btn) return;

  const unsupported = !state.voice.supported;
  const busy = state.voice.transcribing;
  const recording = state.voice.recording;

  btn.disabled = unsupported || busy;
  btn.classList.toggle('recording', recording);
  btn.classList.toggle('busy', busy);

  if (unsupported) {
    btn.title = 'Voice recording is not supported in this browser';
    btn.setAttribute('aria-label', 'Voice recording is not supported in this browser');
    setVoiceStatus('');
    return;
  }

  if (recording) {
    const elapsed = Date.now() - state.voice.startedAt;
    btn.title = 'Stop and send voice message';
    btn.setAttribute('aria-label', 'Stop and send voice message');
    setVoiceStatus(
      `<span class="voice-status-dot" aria-hidden="true"></span>` +
      `<span class="voice-status-copy">Recording <strong>${formatVoiceDuration(elapsed)}</strong></span>` +
      `<button type="button" class="voice-status-cancel" id="voiceCancelBtn">Cancel</button>`
    );
    const cancelBtn = document.getElementById('voiceCancelBtn');
    if (cancelBtn) {
      cancelBtn.addEventListener('click', () => stopVoiceRecording(true), { once: true });
    }
    return;
  }

  if (busy) {
    btn.title = 'Transcribing voice message';
    btn.setAttribute('aria-label', 'Transcribing voice message');
    setVoiceStatus('<span class="voice-status-spinner" aria-hidden="true"></span><span class="voice-status-copy">Transcribing voice message…</span>');
    return;
  }

  btn.title = 'Record voice message';
  btn.setAttribute('aria-label', 'Record voice message');
  setVoiceStatus('');
};

const voiceRecordingMimeType = () => {
  if (typeof MediaRecorder === 'undefined' || typeof MediaRecorder.isTypeSupported !== 'function') {
    return '';
  }
  const candidates = [
    'audio/webm;codecs=opus',
    'audio/mp4',
    'audio/webm',
    'audio/ogg;codecs=opus'
  ];
  return candidates.find((type) => MediaRecorder.isTypeSupported(type)) || '';
};

const audioFilenameForMimeType = (mimeType) => {
  const normalized = String(mimeType || '').toLowerCase();
  if (normalized.includes('mp4') || normalized.includes('m4a')) return 'voice-note.m4a';
  if (normalized.includes('ogg')) return 'voice-note.ogg';
  if (normalized.includes('wav')) return 'voice-note.wav';
  return 'voice-note.webm';
};

const transcribeVoiceBlob = async (blob, mimeType) => {
  const form = new FormData();
  form.append('file', blob, audioFilenameForMimeType(mimeType));

  const headers = {};
  if (state.token) {
    headers.Authorization = `Bearer ${state.token}`;
  }

  const response = await fetch(`${UI_PREFIX}/v1/transcribe`, {
    method: 'POST',
    headers,
    body: form
  });

  if (!response.ok) {
    throw await normalizeError(response);
  }

  const payload = await response.json().catch(() => null);
  const text = String(payload?.text || '').trim();
  if (!text) {
    throw new Error('Transcription came back empty.');
  }
  return text;
};

const handleRecordedVoiceBlob = async (blob, mimeType) => {
  state.voice.transcribing = true;
  updateVoiceUI();

  try {
    const transcript = await transcribeVoiceBlob(blob, mimeType);
    const existingPrompt = String(elements.promptInput.value || '').trim();

    if (!existingPrompt && state.attachments.length === 0) {
      void sendMessage({ prompt: transcript, attachments: [] });
      return;
    }

    elements.promptInput.value = existingPrompt ? `${existingPrompt}\n${transcript}` : transcript;
    autoGrowPrompt();
    elements.promptInput.focus();
  } finally {
    state.voice.transcribing = false;
    updateVoiceUI();
  }
};

const startVoiceRecording = async () => {
  if (!state.voice.supported || state.voice.recording || state.voice.transcribing) return;
  if (!state.connected) {
    openAuthModal('Connect before sending a voice message.', true);
    return;
  }

  try {
    const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
    const mimeType = voiceRecordingMimeType();
    const recorder = mimeType ? new MediaRecorder(stream, { mimeType }) : new MediaRecorder(stream);

    state.voice.recording = true;
    state.voice.recorder = recorder;
    state.voice.stream = stream;
    state.voice.chunks = [];
    state.voice.cancelOnStop = false;
    state.voice.startedAt = Date.now();
    state.voice.mimeType = mimeType || recorder.mimeType || 'audio/webm';

    recorder.addEventListener('dataavailable', (event) => {
      if (event.data && event.data.size > 0) {
        state.voice.chunks.push(event.data);
      }
    });

    recorder.addEventListener('stop', async () => {
      const cancelled = state.voice.cancelOnStop;
      const chunks = [...state.voice.chunks];
      const blobType = state.voice.mimeType || recorder.mimeType || 'audio/webm';

      state.voice.recording = false;
      state.voice.recorder = null;
      state.voice.chunks = [];
      state.voice.cancelOnStop = false;
      clearVoiceTimer();
      stopVoiceTracks();
      updateVoiceUI();

      if (cancelled || chunks.length === 0) {
        setVoiceStatus('');
        return;
      }

      const blob = new Blob(chunks, { type: blobType });
      try {
        await handleRecordedVoiceBlob(blob, blobType);
      } catch (err) {
        setVoiceStatus('');
        if (err?.status === 401) {
          handleAuthFailure();
          return;
        }
        alert(err?.message || 'Failed to transcribe voice message.');
      }
    }, { once: true });

    recorder.start();
    clearVoiceTimer();
    state.voice.timerId = window.setInterval(() => updateVoiceUI(), 250);
    updateVoiceUI();
  } catch (err) {
    stopVoiceTracks();
    state.voice.recording = false;
    state.voice.recorder = null;
    clearVoiceTimer();
    updateVoiceUI();
    alert(err?.message || 'Microphone access failed.');
  }
};

const stopVoiceRecording = (cancelled = false) => {
  if (!state.voice.recording || !state.voice.recorder) return;
  state.voice.cancelOnStop = cancelled;
  const recorder = state.voice.recorder;
  if (recorder.state !== 'inactive') {
    recorder.stop();
  }
};

const toggleVoiceRecording = async () => {
  if (state.voice.recording) {
    stopVoiceRecording(false);
    return;
  }
  await startVoiceRecording();
};

const updateSendButtonState = () => {
  const btn = elements.sendBtn;
  if (!btn) return;
  const hasComposerDraft = Boolean(String(elements.promptInput?.value || '').trim()) || state.attachments.length > 0;
  const interjecting = Boolean(state.streaming && hasComposerDraft);
  const loading = Boolean(state.streaming && !hasComposerDraft);
  btn.disabled = false;
  btn.classList.toggle('loading', loading);
  btn.classList.toggle('interject', interjecting);
  const label = interjecting ? 'Interject' : 'Send message';
  btn.title = label;
  if (typeof btn.setAttribute === 'function') {
    btn.setAttribute('aria-label', label);
  }
  const arrow = typeof btn.querySelector === 'function' ? btn.querySelector('.arrow') : null;
  if (arrow) {
    arrow.textContent = interjecting ? '↳' : '↑';
  }
};

const composerUsesNativeAutoSize = (() => {
  try {
    const css = window.CSS || globalThis.CSS;
    return !!(css && typeof css.supports === 'function' && css.supports('field-sizing', 'content'));
  } catch {
    return false;
  }
})();

let pendingPromptGrowFrame = 0;
let lastPromptGrowHeight = '';
let lastPromptGrowValue = null;

const promptFallbackMaxHeight = () => {
  const viewportHeight = Number(window.innerHeight || 0);
  if (!viewportHeight) return 300;
  return Math.max(200, Math.min(300, Math.floor(viewportHeight * 0.45)));
};

const applyPromptFallbackSize = () => {
  pendingPromptGrowFrame = 0;
  const el = elements.promptInput;
  if (!el || composerUsesNativeAutoSize) return;

  const value = String(el.value || '');
  if (lastPromptGrowValue === value && lastPromptGrowHeight) return;
  lastPromptGrowValue = value;

  el.style.height = 'auto';
  const scrollHeight = el.scrollHeight || 0;
  const maxHeight = promptFallbackMaxHeight();
  const measured = Math.max(48, Math.min(scrollHeight, maxHeight));
  const nextHeight = `${measured}px`;
  const nextOverflow = scrollHeight > maxHeight ? 'auto' : 'hidden';

  el.style.height = nextHeight;
  lastPromptGrowHeight = nextHeight;
  if (el.style.overflowY !== nextOverflow) {
    el.style.overflowY = nextOverflow;
  }
};

const schedulePromptFallbackSize = () => {
  if (composerUsesNativeAutoSize || pendingPromptGrowFrame) return;
  const el = elements.promptInput;
  if (!el) return;
  if (lastPromptGrowValue === String(el.value || '') && lastPromptGrowHeight) return;
  pendingPromptGrowFrame = window.requestAnimationFrame(applyPromptFallbackSize);
};

const autoGrowPrompt = () => {
  const el = elements.promptInput;
  if (!el) return;

  applyTextDirection(el, el.value || '');
  updateSendButtonState();
  schedulePromptFallbackSize();
};

// ===== File attachment =====
// Attachment helpers live in app-attachments.js (a dependency leaf). They are
// pulled off the app bag via the destructure at the top of this file.

const setStreaming = (streaming) => {
  const wasStreaming = state.streaming;
  if (streaming && !wasStreaming) {
    // Only restore focus after a reply if the user was already typing and the device
    // will not pop an on-screen keyboard just because we touched focus().
    state.restorePromptFocus = document.activeElement === elements.promptInput && !shouldSuppressPromptAutoFocus();
  }
  state.streaming = streaming;
  elements.promptInput.disabled = false;
  elements.sendBtn.disabled = false;
  updateSendButtonState();
  elements.stopBtn.classList.toggle('visible', streaming && (Boolean(state.abortController) || Boolean(state.currentStreamResponseId)));
  updateVoiceUI();
  updateSessionUsageDisplay(getActiveSession());
  if (!streaming) {
    flushStreamPersistence();
    const shouldRestoreFocus = state.restorePromptFocus;
    state.restorePromptFocus = false;
    if (shouldRestoreFocus) {
      elements.promptInput.focus();
    }
  }
};

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

const resolvePendingInterruptCommit = (sessionId, prompt) => {
  const idx = state.pendingInterruptCommits.findIndex(entry => entry.sessionId === sessionId && entry.prompt === prompt);
  if (idx < 0) return null;
  const [entry] = state.pendingInterruptCommits.splice(idx, 1);
  return entry;
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
  const queuedIndex = state.queuedInterrupts.findIndex(entry => entry.sessionId === session.id);
  if (queuedIndex >= 0) {
    const [queued] = state.queuedInterrupts.splice(queuedIndex, 1);
    elements.promptInput.value = queued.prompt;
    autoGrowPrompt();
    void sendMessage({ prompt: queued.prompt, attachments: queued.attachments || [], reuseMessageId: queued.messageId, _skipContinuationRefresh: true });
  }
};

const setInterruptMessageState = (session, messageId, interruptState) => {
  if (!messageId) return;
  const normalized = sanitizeInterruptState(interruptState);
  if (!normalized) return;
  const message = session.messages.find(m => m.id === messageId && m.role === 'user');
  if (!message) return;
  message.interruptState = normalized;
  updateUserNode(message);
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
    role: 'user',
    content: prompt,
    created: Date.now(),
    interruptState: normalized
  };
  if (Array.isArray(attachments) && attachments.length > 0) {
    message.attachments = attachments.map(cloneAttachmentForMessage);
  }
  session.messages.push(message);

  const emptyState = elements.messages.querySelector('.empty-state');
  if (emptyState) emptyState.remove();
  elements.messages.appendChild(createMessageNode(message));
  syncTurnActionPanels();
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
      const idx = session.messages.findIndex(m => m.id === entry.messageId && m.role === 'user');
      if (idx >= 0) session.messages.splice(idx, 1);
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

const consumePendingInterjectionByText = (sessionId, text) => {
  if (!sessionId) return null;
  const normalized = String(text || '').trim();
  let idx = -1;
  for (let i = 0; i < state.pendingInterjections.length; i += 1) {
    const entry = state.pendingInterjections[i];
    if (entry.sessionId !== sessionId) continue;
    if (String(entry.prompt || '').trim() === normalized) {
      idx = i;
      break;
    }
  }
  if (idx < 0) {
    for (let i = 0; i < state.pendingInterjections.length; i += 1) {
      const entry = state.pendingInterjections[i];
      if (entry.sessionId === sessionId) { idx = i; break; }
    }
  }
  if (idx < 0) return null;
  const [entry] = state.pendingInterjections.splice(idx, 1);
  refreshPendingInterjectionBanner();
  return entry;
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
    ? { message: prompt, content: prompt ? [...contentParts, { type: 'input_text', text: prompt }] : contentParts, interjection_id: messageId }
    : { message: prompt, interjection_id: messageId };
  const response = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(session.id)}/interrupt`, {
    method: 'POST',
    headers: requestHeaders(session.id),
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
  scrollToBottom(true);
  return action;
};

const runtimeHasActiveRun = (runtimeState) => {
  if (!runtimeState || typeof runtimeState !== 'object') return false;
  return Boolean(runtimeState.active_run || String(runtimeState.active_response_id || '').trim());
};

const runtimeHasPendingAskUser = (runtimeState, callId) => {
  const normalizedCallId = String(callId || '').trim();
  if (!normalizedCallId || !runtimeState || typeof runtimeState !== 'object') return false;
  const prompts = Array.isArray(runtimeState.pending_ask_users)
    ? runtimeState.pending_ask_users
    : (runtimeState.pending_ask_user ? [runtimeState.pending_ask_user] : []);
  return prompts.some((item) => String(item?.call_id || '').trim() === normalizedCallId);
};

const runtimeHasPendingApproval = (runtimeState, approvalId) => {
  const normalizedApprovalId = String(approvalId || '').trim();
  if (!normalizedApprovalId || !runtimeState || typeof runtimeState !== 'object') return false;
  const approvals = Array.isArray(runtimeState.pending_approvals)
    ? runtimeState.pending_approvals
    : (runtimeState.pending_approval ? [runtimeState.pending_approval] : []);
  return approvals.some((item) => String(item?.approval_id || '').trim() === normalizedApprovalId);
};

const refreshSessionFromServerTruth = async (session, pollOnActive = false) => {
  if (!session?.id) return null;
  return app.syncActiveSessionFromServer(session, pollOnActive);
};

const recoverInterruptFailure = async (session, prompt, messageId, attachments = []) => {
  const runtimeState = await refreshSessionFromServerTruth(session, true);
  if (!runtimeState) {
    return false;
  }
  if (runtimeHasActiveRun(runtimeState)) {
    discardPendingInterruptCommit(messageId);
    removePendingInterjectionById(messageId);
    const existing = session.messages.find(m => m.id === messageId && m.role === 'user');
    if (existing) {
      setInterruptMessageState(session, messageId, 'queue');
      if (Array.isArray(attachments) && attachments.length > 0 && !existing.attachments) {
        existing.attachments = attachments.map(cloneAttachmentForMessage);
        updateUserNode(existing);
      }
    } else {
      addInlineInterruptMessage(session, prompt, messageId, 'queue', attachments);
    }
    queueInterruptFollowUp(session.id, prompt, messageId, attachments);
    persistAndRefreshShell();
    scrollToBottom(true);
    clearDraftMessageForSession(session.id);
    return true;
  }

  // syncActiveSessionFromServer is expected to clear stale local busy state
  // before retrying the prompt as a fresh response.
  discardPendingInterruptCommit(messageId);
  removePendingInterjectionById(messageId);
  await sendMessage({
    prompt,
    attachments,
    _skipContinuationRefresh: true
  });
  return true;
};

const recoverInterruptConflict = recoverInterruptFailure;

const addErrorMessage = (text, session) => {
  const message = {
    id: generateId('msg'),
    role: 'error',
    content: text,
    created: Date.now()
  };
  session.messages.push(message);
  appendStreamMessageNode(session, message);
};

const markToolGroupsDone = (session) => {
  session.messages.forEach(m => {
    if (m.role === 'tool-group' && m.status === 'running') {
      m.tools.forEach(t => { t.status = 'done'; });
      m.status = 'done';
      updateVisibleToolGroupNode(session, m);
    }
    if (m.role === 'tool' && m.status === 'running') {
      m.status = 'done';
      if (isSessionVisible(session)) updateToolNode(m);
    }
  });
};

const sendMessage = async (options = {}) => {
  const promptSource = typeof options.prompt === 'string' ? options.prompt : elements.promptInput.value;
  const prompt = String(promptSource || '').trim();
  const pendingAttachments = Array.isArray(options.attachments)
    ? [...options.attachments]
    : [...state.attachments];

  if (!prompt && pendingAttachments.length === 0) return;

  if (!state.connected) {
    openAuthModal('Connect before sending a message.', true);
    return;
  }

  let session = getActiveSession();
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
    trackPendingInterruptCommit(session.id, prompt, pendingMessageId, pendingAttachments);
    trackPendingInterjection(session.id, prompt || pendingAttachments[0]?.name || 'Attachment', pendingMessageId, 'deciding', pendingAttachments);
    addInlineInterruptMessage(session, prompt, pendingMessageId, 'evaluating', pendingAttachments);
    persistAndRefreshShell();
    scrollToBottom(true);

    elements.promptInput.value = '';
    state.attachments = [];
    renderAttachments();
    autoGrowPrompt();

    try {
      await interruptActiveRun(session, prompt, pendingMessageId, requestAttachmentParts, pendingAttachments);
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

      discardPendingInterruptCommit(pendingMessageId);
      setInterjectionPhase(session, pendingMessageId, 'failed');
      const message = err?.message || 'Failed to interrupt active run.';
      addErrorMessage(message, session);
      if (err?.status === 401) {
        handleAuthFailure();
      }
      elements.promptInput.value = prompt;
      state.attachments = pendingAttachments;
      renderAttachments();
      autoGrowPrompt();
      persistAndRefreshShell();
      scrollToBottom(true);
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
    ? session.messages.find(m => m.id === reuseMessageId && m.role === 'user') || null
    : null;
  const isNewUserMessage = !userMessage;

  if (!userMessage) {
    userMessage = {
      id: generateId('msg'),
      role: 'user',
      content: prompt,
      created: Date.now()
    };
    session.messages.push(userMessage);
  } else {
    userMessage.content = prompt;
    delete userMessage.interruptState;
  }
  session.lastMessageAt = Date.now();

  if (pendingAttachments.length > 0) {
    userMessage.attachments = pendingAttachments.map(cloneAttachmentForMessage);
  } else {
    delete userMessage.attachments;
  }

  if (!session.title || session.title === 'New chat') {
    session.title = truncate(prompt || pendingAttachments[0]?.name || 'Image', 60);
  }

  const hadEmptyState = elements.messages.querySelector('.empty-state');
  if (hadEmptyState) hadEmptyState.remove();

  if (isNewUserMessage) {
    elements.messages.appendChild(createMessageNode(userMessage));
  } else {
    updateUserNode(userMessage);
  }
  syncTurnActionPanels();

  setSessionOptimisticBusy(session, true);
  persistAndRefreshShell();

  elements.promptInput.value = '';
  if (!Array.isArray(options.attachments)) {
    state.attachments = [];
    renderAttachments();
  }
  autoGrowPrompt();
  scrollToBottom(true);

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
      input: [{ type: 'message', role: 'user', content: inputContent }]
    };

    previousResponseId = String(session.lastResponseId || '').trim();
    if (previousResponseId) {
      body.previous_response_id = previousResponseId;
    }

    canonicalizeSelectedModelEffort();
    const currentProvider = session.provider || '';
    const currentModel = session.activeModel || '';
    const currentEffort = session.activeEffort || '';
    const targetProvider = state.selectedProvider || currentProvider;
    const targetModel = state.selectedModel || currentModel;
    const targetEffort = state.selectedEffort || '';
    const hasPriorContext = Boolean(session.messages.length > 1);
    const targetDiffers = hasPriorContext && Boolean(
      (targetProvider || '') !== (currentProvider || '')
      || (targetModel || '') !== (currentModel || '')
      || normalizeEffortForCompare(targetEffort) !== normalizeEffortForCompare(currentEffort)
    );

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
      const activeEffort = currentEffort || state.selectedEffort;
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
    headers['Idempotency-Key'] = userMessage.id;
    headers['X-Term-LLM-Request-ID'] = userMessage.id;
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
      const result = await consumeResponseStream(response.body, session, streamState, { generation: sendGeneration, responseId });
      if (!result.stale && result.error) {
        throw result.error;
      }
      if (!result.stale && !result.terminal && sendGeneration === state.streamGeneration && session.activeResponseId) {
        await resumeActiveResponse(session, { streamState, responseId });
      }
    }

    if (sendGeneration === state.streamGeneration) {
      const lastAssistant = session.messages.findLast(m => m.role === 'assistant');
      if (lastAssistant) updateAssistantNode(lastAssistant);
      persistAndRefreshShell();
      scrollToBottom();
    }
  } catch (err) {
    streamState.closeToolGroup();
    markToolGroupsDone(session);

    const controllerAborted = Boolean(controller.signal?.aborted || err?.name === 'AbortError');
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

    const lastAssistant = session.messages.findLast(m => m.role === 'assistant');
    if (lastAssistant) updateAssistantNode(lastAssistant);

    if (session.activeResponseId) {
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
      handleAuthFailure();
    }
    if (!String(elements.promptInput.value || '').trim()) {
      elements.promptInput.value = prompt;
      autoGrowPrompt();
    }

    persistAndRefreshShell();
    scrollToBottom(true);
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
      closeAskUserModal();
    }

    if (!stillActive) {
      setSessionOptimisticBusy(session, false);
      app.refreshSidebarStatusPoll?.();
      requeuePendingInterjections(session);
    }
    setStreaming(stillActive);
    refreshRelativeTimes();
    if (stillActive) {
      return;
    }

    drainInterruptQueueIfIdle(session);
  }
};

// Recover text that was submitted locally but never acknowledged by the server
// (for example a dropped POST, stale tab, or reload while the request was in flight).
restoreLatestDraftMessage();

Object.assign(app, {
  requestHeaders,
  normalizeError,
  fetchProviders,
  fetchModels,
  parseSSEStream,
  sleep,
  stageDraftMessage,
  removeDraftMessage,
  clearDraftMessageForSession,
  restoreLatestDraftMessage,
  restoreDraftMessageForSession,
  setActiveResponseTracking,
  attachResponseStream,
  detachResponseStream,
  clearActiveResponseTracking,
  updateResponseSequence,
  createResponseStreamState,
  applyResponseStreamEvent,
  consumeResponseStream,
  scheduleStreamPersistence,
  flushStreamPersistence,
  scheduleStreamScroll,
  HEARTBEAT_STALE_THRESHOLD,
  HEARTBEAT_ABORT_REASON,
  resumeActiveResponse,
  cancelActiveResponse,
  closeAskUserModal,
  openApprovalModal,
  closeApprovalModal,
  submitApprovalModal,
  askUserSummaryFromAnswers,
  collectAskUserAnswers,
  validateSingleQuestion,
  switchAskUserTab,
  renderAskUserModal,
  openAskUserModal,
  submitAskUserModal,
  openAuthModal,
  closeAuthModal,
  handleAuthFailure,
  connectToken,
  normalizeSelectedProvider,
  canonicalizeSelectedModelEffort,
  applyModelChange,
  applyEffortChange,
  queueActiveRunEffortChange,
  renderProviderOptions,
  renderModelOptions,
  autoGrowPrompt,
  updateSendButtonState,
  updateVoiceUI,
  startVoiceRecording,
  stopVoiceRecording,
  toggleVoiceRecording,
  setStreaming,
  queueInterruptFollowUp,
  trackPendingInterruptCommit,
  resolvePendingInterruptCommit,
  resolvePendingInterruptCommitById,
  discardPendingInterruptCommit,
  requeueUncommittedInterrupts,
  drainInterruptQueueIfIdle,
  setInterruptMessageState,
  addInlineInterruptMessage,
  trackPendingInterjection,
  updatePendingInterjectionAction,
  removePendingInterjectionById,
  consumePendingInterjectionByText,
  refreshPendingInterjectionBanner,
  requeuePendingInterjections,
  interruptActiveRun,
  recoverInterruptFailure,
  recoverInterruptConflict,
  addErrorMessage,
  markToolGroupsDone,
  sendMessage
});
})();
