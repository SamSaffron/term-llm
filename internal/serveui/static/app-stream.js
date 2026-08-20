(() => {
'use strict';

const app = window.TermLLMApp;
const ConversationController = window.TermLLMConversation?.ConversationController;
const {
  UI_PREFIX, state, elements, generateId, sanitizeMessage, saveSessions, getActiveSession, scrollToBottom,
  setConnectionState, clearProviderRetryStatus, persistAndRefreshShell, updateSessionUsageDisplay, updateUserNode,
  updateToolGroupNode, createMessageNode, updateModelSwapNode, renderSidebar, renderMessages,
  insertMountedMessageNode, enqueueAssistantStreamUpdate, finalizeAssistantStreamRender, updateMountedToolGroupNode,
  updateMountedModelSwapNode, updateMountedUserNode, enqueueMountedAssistantStreamUpdate,
  finalizeMountedAssistantStreamRender, conversationDOMFor, isConversationMounted, shouldSuppressPromptAutoFocus,
  setSessionOptimisticBusy, setSessionServerActiveRun
} = app;

const rebaseStreamAssetURL = (url) => (
  typeof app.rebaseHubAssetURL === 'function'
    ? app.rebaseHubAssetURL(url)
    : String(url || '').trim()
);

// ===== Network helpers =====
const requestHeaders = (sessionId, tokenOverride = '') => {
  const headers = { 'Content-Type': 'application/json' };
  const token = tokenOverride || state.token;
  if (token) headers.Authorization = `Bearer ${token}`;
  if (sessionId) headers.session_id = sessionId;
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

  return {
    status: response.status,
    message,
    type: String(parsed?.error?.type || ''),
    code: String(parsed?.error?.code || '')
  };
};

const hasSessionContinuationContext = (session) => Boolean(
  session && (
    Number(session.number || 0) > 0
    || (Array.isArray(window.TermLLMConversation.sessionMessages(session)) && window.TermLLMConversation.sessionMessages(session).length > 0)
  )
);

const normalizeEffortForCompare = (value) => {
  const normalized = String(value || '').trim().toLowerCase();
  return normalized === 'default' ? '' : normalized;
};

const effectiveEffortForCompare = (model, effort) => {
  const explicit = normalizeEffortForCompare(effort);
  if (explicit) return explicit;
  const id = String(model || '').trim();
  const info = id && state.modelInfoByID
    ? state.modelInfoByID[id]
    : null;
  return normalizeEffortForCompare(info?.default_reasoning_effort || '');
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

// Runtime controls are global UI state, while the applied runtime belongs to a
// session. Only an explicit control action may authorize carrying global state
// into an existing conversation; metadata refreshes and session polling also
// mutate the global selection and must never cause a model swap on their own.
const markRuntimeSelectionIntent = (session = getActiveSession()) => {
  if (!session) return;
  session.runtimeSelectionIntent = true;
};

const clearRuntimeSelectionIntent = (session) => {
  if (!session) return;
  delete session.runtimeSelectionIntent;
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


const parseSSEStream = async (stream, onEvent, options = {}) => {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  const abortController = options.abortController || null;
  let heartbeatCancelPromise = null;
  const cancelForHeartbeat = abortController
    ? () => {
        if (!heartbeatCancelPromise) {
          heartbeatCancelPromise = reader.cancel();
        }
        return heartbeatCancelPromise;
      }
    : null;
  if (abortController) {
    // This remains sticky for the controller's lifetime. Once a fetch body has
    // been exposed, heartbeat recovery must never fall back to aborting that
    // fetch, even after the reader has finished unwinding.
    abortController._responseBodyAttached = true;
    abortController._heartbeatCancelStream = cancelForHeartbeat;
  }
  let buffer = '';

  try {
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
      // Ignore late bytes from a WebKit reader retired by heartbeat takeover
      // before they refresh global liveness or project stale events.
      if (abortController?._streamSuperseded) return;

      const decoded = decoder.decode(value, { stream: true });
      buffer += decoded.includes('\r') ? decoded.replace(/\r/g, '') : decoded;
      if (options.trackHeartbeat !== false) {
        state.lastEventTime = Date.now();
        if (state.abortController) {
          state.abortController._heartbeatStaleThreshold = HEARTBEAT_STALE_THRESHOLD;
        }
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
  } finally {
    if (abortController?._heartbeatCancelStream === cancelForHeartbeat) {
      delete abortController._heartbeatCancelStream;
    }
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
    ? 'Connection unstable; next retry within one minute (online or returning to this page retries now)…'
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


const setActiveResponseTracking = (session, responseId) => {
  if (!session) return;
  const normalized = String(responseId || '').trim();
  if (!normalized) return;

  const currentId = String(session.activeResponseId || '').trim();
  if (currentId && currentId !== normalized) {
    clearProviderRetryStatus(String(session.id || '').trim(), currentId);
  }
  if (currentId !== normalized) {
    session.activeResponseId = normalized;
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
// Avoid custom abort reasons: some browsers reject fetch with the raw string.
const HEARTBEAT_ABORT_REASON = 'heartbeat';
const HEARTBEAT_TAKEOVER_GRACE = 1000;
const scheduleHeartbeatTakeover = (controller) => {
  if (!controller || controller._heartbeatTakeoverScheduled) return;
  controller._heartbeatTakeoverScheduled = true;
  const activityBaseline = Number(state.lastEventTime || 0);
  window.setTimeout(() => {
    controller._heartbeatTakeoverScheduled = false;
    if (state.abortController !== controller || !controller._heartbeatAbort
        || Number(state.lastEventTime || 0) > activityBaseline) return;
    controller._heartbeatTakeover?.();
  }, HEARTBEAT_TAKEOVER_GRACE);
};

const startHeartbeatMonitor = () => {
  stopHeartbeatMonitor();
  state.lastEventTime = Date.now();
  heartbeatTimerId = window.setInterval(() => {
    try {
      if (!state.abortController || !state.currentStreamSessionId) {
        stopHeartbeatMonitor();
        return;
      }
      if (state.askUser || state.approval) return;
      const staleThreshold = Math.max(
        HEARTBEAT_STALE_THRESHOLD,
        Number(state.abortController?._heartbeatStaleThreshold || 0) || 0
      );
      if (Date.now() - state.lastEventTime > staleThreshold) {
        const controller = state.abortController;
        if (controller) {
          controller._heartbeatAbort = true;
          if (typeof controller._heartbeatCancelStream === 'function') {
            Promise.resolve(controller._heartbeatCancelStream()).catch((err) => {
              console.warn('[stream] heartbeat body cancellation failed', err);
            });
            // WebKit can leave reader.read() and reader.cancel() pending forever.
            // After a brief grace, release the caller to resume over HTTP.
            scheduleHeartbeatTakeover(controller);
          } else if (!controller._responseBodyAttached) {
            // Before response headers arrive there is no body reader to cancel.
            controller.abort();
          }
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
    app.persistPendingIntents?.(getActiveSession());
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
  app.persistPendingIntents?.(getActiveSession());
};

const scheduleStreamScroll = () => {
  if (streamScrollRafId) return;
  streamScrollRafId = window.requestAnimationFrame(() => {
    streamScrollRafId = 0;
    scrollToBottom();
  });
};

const activeResumeKeys = new Map();

const streamDiagnosticsEnabled = () => Boolean(
  window.__TERM_LLM_DIAGNOSTICS__ || window.__WEBRTC_DIAGNOSTICS__
);

const setReconnectDiagnostic = (reconnectState, reason = '', delay = 0) => {
  if (!streamDiagnosticsEnabled()) return;
  const dataset = elements.connectionState?.dataset;
  if (!dataset) return;
  dataset.reconnectState = String(reconnectState || '');
  dataset.reconnectReason = String(reason || '');
  dataset.reconnectDelayMs = String(Math.max(0, Number(delay) || 0));
};

const wakeResponseReconnect = ({ reason = '', sessionId = '', responseId = '' } = {}) => {
  const normalizedSessionId = String(sessionId || '').trim();
  const normalizedResponseId = String(responseId || '').trim();
  if (!normalizedSessionId || !normalizedResponseId) return false;
  setReconnectDiagnostic('waking', reason, 0);
  return app.wakeNetworkRetry(`${normalizedSessionId}:${normalizedResponseId}`, reason);
};

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
  app.cancelNetworkRetries?.(prefix, 'detached');
  for (const key of activeResumeKeys.keys()) {
    if (key.startsWith(prefix)) activeResumeKeys.delete(key);
  }
};

const detachResponseStream = () => {
  stopHeartbeatMonitor();
  flushStreamPersistence();
  state.streamGeneration += 1;
  const controller = state.abortController;
  const detachedSessionId = state.currentStreamSessionId;
  const detachedResponseId = state.currentStreamResponseId;
  state.abortController = null;
  state.currentStreamSessionId = '';
  state.currentStreamResponseId = '';
  if (detachedSessionId && detachedResponseId) {
    clearProviderRetryStatus(detachedSessionId, detachedResponseId);
  }
  if (controller) {
    try { controller.abort(); } catch (_) { /* stream may already be closed */ }
  }
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
  if (targetId || currentId) clearProviderRetryStatus(String(session.id || '').trim(), targetId || currentId);
  if (!targetId || currentId === targetId || targetId.startsWith('resp_msg_')) {
    session.activeResponseId = null;
  }
  const ownsTransport = state.currentStreamSessionId === String(session.id || '').trim();
  if (!targetId || (ownsTransport && (!state.currentStreamResponseId || state.currentStreamResponseId === targetId || targetId.startsWith('resp_msg_')))) {
    state.currentStreamSessionId = '';
    state.currentStreamResponseId = '';
    if (!state.abortController) setConnectionState('', '');
  }
};

const responseEventSequence = (session, responseId) => {
  const active = session?.transcript?.conversation?.active;
  return active?.responseID === String(responseId || '') ? Math.max(0, Number(active.lastSequence) || 0) : 0;
};

const notifyTranscriptTerminal = (session, responseId, payload = {}) => {
  const handler = app.noteTranscriptTerminal;
  if (typeof handler !== 'function') return Promise.resolve(false);
  const finalRev = payload?.final_rev ?? payload?.response?.final_rev ?? 0;
  const runEpoch = payload?.run_epoch ?? payload?.response?.run_epoch ?? 0;
  const durableHandoff = payload?.durable_handoff ?? payload?.response?.durable_handoff ?? true;
  const handoffError = payload?.durable_handoff_error ?? payload?.response?.durable_handoff_error ?? '';
  if (handler.length <= 2) return handler(session, finalRev);
  return handler(session, responseId, finalRev, runEpoch, durableHandoff, handoffError);
};

const terminalTranscriptHandoffs = new Map();
const trackTranscriptTerminalHandoff = (session, responseId, payload = {}) => {
  const sessionId = String(session?.id || '').trim();
  const handoff = Promise.resolve(notifyTranscriptTerminal(session, responseId, payload)).catch((err) => {
    console.warn('[transcript] terminal handoff failed', err);
    return false;
  });
  if (!sessionId) return handoff;
  terminalTranscriptHandoffs.set(sessionId, handoff);
  void handoff.finally(() => {
    if (terminalTranscriptHandoffs.get(sessionId) === handoff) terminalTranscriptHandoffs.delete(sessionId);
  });
  return handoff;
};
const awaitTranscriptTerminalHandoff = (session) => terminalTranscriptHandoffs.get(String(session?.id || '').trim()) || Promise.resolve(false);

const isSessionVisible = (session) => {
  if (!session) return false;
  if (typeof isConversationMounted === 'function') return isConversationMounted(session);
  return !state.draftSessionActive && state.activeSessionId === session.id;
};

const visibleConversationDOM = (session) => {
  if (typeof conversationDOMFor === 'function') return conversationDOMFor(session);
  return isSessionVisible(session) ? elements.messages : null;
};

const appendStreamMessageNode = (session, message, createNode = createMessageNode) => {
  const root = visibleConversationDOM(session);
  if (!root) return null;
  const node = createNode(message);
  if (node?.dataset) node.dataset.sessionId = String(session?.id || '');
  if (typeof insertMountedMessageNode === 'function') {
    return insertMountedMessageNode(session, message, node);
  }
  root.appendChild(node);
  return node;
};

const updateVisibleToolGroupNode = (session, message) => {
  if (typeof updateMountedToolGroupNode === 'function') {
    updateMountedToolGroupNode(session, message);
  } else if (isSessionVisible(session)) {
    updateToolGroupNode(message);
  }
};

const updateVisibleUserNode = (session, message) => {
  if (typeof updateMountedUserNode === 'function') {
    updateMountedUserNode(session, message);
  } else if (isSessionVisible(session)) {
    updateUserNode(message);
  }
};

const enqueueVisibleAssistantStreamUpdate = (session, message) => {
  if (typeof enqueueMountedAssistantStreamUpdate === 'function') {
    enqueueMountedAssistantStreamUpdate(session, message);
  } else if (isSessionVisible(session)) {
    enqueueAssistantStreamUpdate(message);
  }
};

const finalizeVisibleAssistantStreamRender = (session, message) => {
  if (typeof finalizeMountedAssistantStreamRender === 'function') {
    finalizeMountedAssistantStreamRender(session, message);
  } else if (isSessionVisible(session)) {
    finalizeAssistantStreamRender(message);
  }
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

const responseStreamOwnerId = (session, payload = null) => {
  const payloadResponseId = String(payload?.response_id || payload?.response?.id || '').trim();
  if (payloadResponseId) return payloadResponseId;
  const activeResponseId = String(session?.activeResponseId || '').trim();
  if (activeResponseId) return activeResponseId;
  if (String(state.currentStreamSessionId || '').trim() === String(session?.id || '').trim()) {
    return String(state.currentStreamResponseId || '').trim();
  }
  return '';
};

const clearProviderRetryForEvent = (session, payload = null) => {
  const responseId = responseStreamOwnerId(session, payload);
  if (!responseId) return false;
  return clearProviderRetryStatus(String(session?.id || '').trim(), responseId);
};

const createResponseStreamState = (session, options = {}) => {
  if (session && !session.transcript && typeof ConversationController === 'function') {
    session.transcript = new ConversationController(session.id || 'stream');
  }
  for (const message of window.TermLLMConversation.sessionMessages(session) || []) {
    if (message?.durable === true || message?.role !== 'user') continue;
    if (!message.clientMessageId) message.clientMessageId = String(message.id || '').trim();
    if (message.clientMessageId) window.TermLLMConversation.addPendingIntentToConversation(session.transcript, message, session.transcript.rev);
  }
  const responseId = String(options.responseId || responseStreamOwnerId(session) || '').trim();
  const runEpoch = Math.max(0, Number(options.runEpoch) || 0);
  if (responseId && runEpoch) window.TermLLMConversation.attachActiveRun(session?.transcript, { response_id: responseId, run_epoch: runEpoch });
  return {
    responseId,
    historicalReplayEvent: false,
    currentToolGroup: null,
    currentAssistantMessage: null,
    currentPhaseMessage: null,
    closeToolGroup() {},
  };
};


const replayPayloadResponseID = (payload, fallback = '') => String(
  payload?.response_id || payload?.response?.id || fallback || ''
).trim();

const createHistoricalReplayStage = (responseId, after, replayThrough, runEpoch = 0) => ({
  responseId: String(responseId || '').trim(),
  after: Math.max(0, Number(after) || 0),
  replayThrough: Math.max(0, Number(replayThrough) || 0),
  runEpoch: Math.max(0, Number(runEpoch) || 0),
  appliedSequence: Math.max(0, Number(after) || 0),
  events: [],
  terminal: null,
});

const reduceHistoricalReplayEvent = (stage, event, payload) => {
  const sequence = Math.max(0, Number(payload?.sequence_number) || 0);
  if (!sequence || sequence <= stage.appliedSequence) return true;
  if (sequence !== stage.appliedSequence + 1) throw new Error(`historical replay gap at ${sequence}`);
  const responseId = replayPayloadResponseID(payload, stage.responseId);
  if (stage.responseId && responseId && responseId !== stage.responseId) throw new Error('historical replay response identity changed');
  stage.appliedSequence = sequence;
  if (event === 'response.completed' || event === 'response.cancelled' || event === 'response.failed') {
    stage.terminal = { event, payload };
  } else {
    stage.events.push({ event, payload });
  }
  return true;
};

const mergeHistoricalReplayStage = async (session, _streamState, stage, isCurrent) => {
  const transcript = session?.transcript;
  if (!transcript) throw new Error('historical replay requires a transcript');
  const merged = await window.TermLLMConversation.enqueueDetachedReplay(transcript, stage.events, isCurrent);
  if (!merged || !isCurrent()) return false;
  for (const item of stage.events) {
    if (item?.event !== 'response.interjection') continue;
    const payload = item.payload || {};
    const clientMessageId = String(payload.client_message_id || payload.interjection_id || '').trim();
    if (!clientMessageId) continue;
    app.commitPendingInterjection?.(session, clientMessageId, {
      content: payload.text,
      attachments: payload.attachments,
      created: payload.created,
    });
  }
  app.refreshPendingInterjectionBanner?.();
  app.refreshSessionMessagesFromTranscript?.(session);
  app.persistPendingIntents?.(session);
  if (isSessionVisible(session)) renderMessages(false);
  else persistAndRefreshShell();
  return true;
};

const consumeResponseStreamInner = async (stream, session, streamState, options = {}) => {
  let sawTerminal = false;
  let sawDone = false;
  let sawRecoverableStreamError = false;
  let stale = false;
  let terminalError = null;
  const generation = Number.isFinite(Number(options.generation)) ? Number(options.generation) : state.streamGeneration;
  const sessionId = String(session?.id || '').trim();
  const expectedResponseId = String(options.responseId || '').trim();
  const replayAfterSequence = Math.max(0, Number(options.replayAfterSequence) || 0);
  const replayThroughSequence = Math.max(0, Number(options.replayThroughSequence) || 0);
  let replayBoundaryPending = replayThroughSequence > replayAfterSequence;
  const replayStage = replayBoundaryPending
    ? createHistoricalReplayStage(expectedResponseId, replayAfterSequence, replayThroughSequence, options.runEpoch)
    : null;
  const streamIsCurrent = () => {
    if (options.abortController?._streamSuperseded || generation !== state.streamGeneration) return false;
    const currentSessionId = state.currentStreamSessionId;
    if (currentSessionId && sessionId && currentSessionId !== sessionId) return false;
    const currentResponseId = state.currentStreamResponseId;
    return !(expectedResponseId && currentResponseId && currentResponseId !== expectedResponseId);
  };

  const completeReplayBoundary = async () => {
    if (!replayBoundaryPending || stale || !replayStage || replayStage.appliedSequence < replayThroughSequence || !streamIsCurrent()) return false;
    replayBoundaryPending = false;
    streamState.historicalReplayEvent = false;
    const merged = await mergeHistoricalReplayStage(session, streamState, replayStage, streamIsCurrent);
    if (!merged || !streamIsCurrent()) { stale = true; return false; }
    const stagedTerminal = replayStage.terminal;
    if (stagedTerminal) {
      streamState.historicalReplayEvent = true;
      const result = app.applyResponseStreamEvent(session, streamState, stagedTerminal.event, stagedTerminal.payload);
      streamState.historicalReplayEvent = false;
      if (result?.terminal) sawTerminal = true;
      if (result?.error) terminalError = result.error;
    }
    return true;
  };

  if (replayBoundaryPending) {
    // Replay remains detached until the ordered response header boundary is
    // observed. No stream cursor points at durable/session/DOM state here.
    streamState.historicalReplayEvent = true;
  }

  const eventSequenceNumber = (payload) => {
    const seq = Number(payload?.sequence_number);
    return Number.isFinite(seq) && seq > 0 ? seq : 0;
  };

  await parseSSEStream(stream, async (event, data) => {
    if (!streamIsCurrent()) {
      stale = true;
      return false;
    }

    if (data === '[DONE]') {
      if (replayBoundaryPending) {
        terminalError = {
          message: 'historical replay ended before its ordered boundary',
          recoverableReplayInterrupted: true,
        };
      } else if (!sawRecoverableStreamError) {
        sawDone = true;
        streamState.closeToolGroup();
            persistAndRefreshShell();
      }
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
    const eventResponseId = replayPayloadResponseID(payload, expectedResponseId);
    if (replayBoundaryPending && seq > 0 && seq <= replayThroughSequence) {
      try {
        reduceHistoricalReplayEvent(replayStage, event, payload);
      } catch (err) {
        terminalError = {
          message: err?.message || 'historical replay reduction failed',
          recoverableReplayInterrupted: true,
        };
        return false;
      }
      if (seq >= replayThroughSequence) await completeReplayBoundary();
      return true;
    }
    if (replayBoundaryPending && seq > replayThroughSequence) {
      terminalError = {
        message: 'fresh response event arrived before replay boundary completion',
        recoverableReplayInterrupted: true,
      };
      return false;
    }

    streamState.historicalReplayEvent = false;
    const currentSeq = responseEventSequence(session, eventResponseId);
    // Overlapping POST and GET transports share a response-scoped cursor. An
    // already-applied event is ignored without moving mutable cursors to a
    // projected/durable node.
    if (seq > 0 && (seq < currentSeq || (seq === currentSeq && event !== 'response.stream_error'))) {
      return true;
    }
    if (seq > currentSeq + 1) {
      terminalError = {
        message: `response event stream gap: expected sequence ${currentSeq + 1}, received ${seq}`,
        recoverableStreamGap: true,
      };
      return false;
    }

    let result;
    try {
      result = app.applyResponseStreamEvent(session, streamState, event, payload);
    } catch (err) {
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
  }, { abortController: options.abortController });

  return { terminal: sawTerminal || sawDone || !session.activeResponseId, stale, error: stale ? null : terminalError };
};

const consumeResponseStream = async (stream, session, streamState, options = {}) => {
  const controller = options.abortController || null;
  if (!controller) return consumeResponseStreamInner(stream, session, streamState, options);

  let takeoverResolve;
  const takeoverPromise = new Promise((resolve) => { takeoverResolve = resolve; });
  const heartbeatTakeover = () => {
    if (controller._streamSuperseded) return;
    controller._streamSuperseded = true;
    takeoverResolve({ terminal: false, stale: false, error: null, forcedHeartbeatRecovery: true });
  };
  controller._heartbeatTakeover = heartbeatTakeover;

  try {
    return await Promise.race([
      consumeResponseStreamInner(stream, session, streamState, options),
      takeoverPromise,
    ]);
  } finally {
    if (controller._heartbeatTakeover === heartbeatTakeover) delete controller._heartbeatTakeover;
  }
};

const fetchResponseSnapshot = async (session, responseId) => {
  const response = await app.apiFetch(`${UI_PREFIX}/v1/responses/${encodeURIComponent(responseId)}`, {
    headers: requestHeaders(session?.id || '')
  });
  if (!response.ok) {
    throw await normalizeError(response);
  }
  return response.json().catch(() => ({}));
};

const recoverResponseStateFromSnapshot = async (session, responseId) => {
  const snapshot = await fetchResponseSnapshot(session, responseId);
  await session.transcript.commands.enqueue(() => applyResponseRecoverySnapshot(session, snapshot));
  if (session?.id === state.activeSessionId && !state.draftSessionActive) {
    await app.refreshCurrentPlanFromServer?.(session);
  }
  return snapshot;
};

const applyResponseRecoverySnapshot = (session, payload) => {
  if (!session || !payload || typeof payload !== 'object') return false;

  const recovery = payload.recovery;
  const hasRecovery = recovery && typeof recovery === 'object';

  if (hasRecovery) {
    const responseId = String(payload.id || session.activeResponseId || state.currentStreamResponseId || '').trim();
    const currentOwner = String(session.activeResponseId || state.currentStreamResponseId || '').trim();
    if (responseId && currentOwner && responseId !== currentOwner) return false;
    if (!session.transcript && typeof ConversationController === 'function') {
      session.transcript = new ConversationController(session.id || 'recovery');
    }
    const rawMessages = Array.isArray(recovery.messages) ? recovery.messages : [];
    let recoveryAssistantOrdinal = 0;
    const recoveredMessages = rawMessages
      .map((message) => {
        const sanitized = sanitizeMessage(message);
        if (sanitized && message?.client_message_id) sanitized.clientMessageId = String(message.client_message_id);
        if (sanitized?.role === 'assistant') {
          if (!Number.isFinite(Number(sanitized.assistantSegmentOrdinal))) {
            sanitized.assistantSegmentOrdinal = recoveryAssistantOrdinal;
            recoveryAssistantOrdinal += 1;
          } else {
            recoveryAssistantOrdinal = Math.max(
              recoveryAssistantOrdinal,
              Math.trunc(Number(sanitized.assistantSegmentOrdinal)) + 1
            );
          }
        }
        return sanitized;
      })
      .filter(Boolean)
      .map((message) => ({ ...message, responseId: String(message.responseId || responseId) }));

    const recoveredInterjections = recoveredMessages.filter((message) => (
      message?.role === 'user' && message?.interruptState === 'interject'
    ));
    for (const message of recoveredInterjections) {
      const clientMessageId = String(message.clientMessageId || message.client_message_id || '').trim();
      app.commitPendingInterjection?.(session, clientMessageId, message);
    }
    app.refreshPendingInterjectionBanner?.();

    const transcript = session.transcript;
    if (transcript) {
      if (window.TermLLMConversation.replaceRunSnapshot(transcript, {
        ...payload,
        id: responseId,
        last_sequence_number: recovery.sequence_number ?? payload.last_sequence_number,
        recovery: { ...recovery, messages: recoveredMessages },
      }) !== true) return false;
      for (const fact of Array.isArray(recovery.events) ? recovery.events : []) {
        app.applyRecoveredInteractiveFact?.(session, String(fact?.event || ''), fact?.payload || {});
      }
      app.persistPendingIntents?.(session);
      if (typeof app.refreshSessionMessagesFromTranscript === 'function') {
        app.refreshSessionMessagesFromTranscript(session);
      }
    }
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

  const terminalStatus = ['completed', 'failed', 'cancelled'].includes(String(payload.status || ''));
  const continuationResponseId = String(payload.continuation_response_id || '').trim();
  const firstFinalization = !terminalStatus
    || String(session.activeResponseId || '') === responseId
    || (state.currentStreamSessionId === session.id && String(state.currentStreamResponseId || '') === responseId);
  if (payload.status === 'completed' && firstFinalization) {
    clearTerminalPendingEffort(session);
    if (continuationResponseId || responseId) session.lastResponseId = continuationResponseId || responseId;
    clearActiveResponseTracking(session, responseId);
    setSessionOptimisticBusy(session, false);
    setSessionServerActiveRun(session, false);
    app.requeuePendingInterjections(session);
    void trackTranscriptTerminalHandoff(session, responseId, payload);
  } else if ((payload.status === 'failed' || payload.status === 'cancelled') && firstFinalization) {
    clearTerminalPendingEffort(session);
    if (continuationResponseId) session.lastResponseId = continuationResponseId;
    clearActiveResponseTracking(session, responseId);
    setSessionOptimisticBusy(session, false);
    setSessionServerActiveRun(session, false);
    app.requeuePendingInterjections(session);
    void trackTranscriptTerminalHandoff(session, responseId, payload);
  } else if (!terminalStatus && responseId) {
    const runEpoch = Math.max(0, Number(payload.run_epoch) || 0);
    const latestRunEpoch = Math.max(0, Number(session.transcript?.latestRunEpoch) || 0);
    const currentResponseId = String(session.activeResponseId || '').trim();
    const staleOwnership = Boolean(
      currentResponseId
      && currentResponseId !== responseId
      && (runEpoch === 0 || runEpoch <= latestRunEpoch)
    );
    if (staleOwnership) {
      window.TermLLMConversation?.transcriptDiagnostic?.('stale_status_rejection', {
        responseId,
        transcriptRev: session.transcript?.rev,
        startRev: payload.started_rev,
      });
    } else {
      if (window.TermLLMConversation.attachActiveRun(session.transcript, payload) !== true) return false;
      setActiveResponseTracking(session, responseId);
      setSessionOptimisticBusy(session, true);
    }
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
  // Cleanup may run after detach has already registered a replacement loop, so
  // it must remove the key only while it still owns that registration.
  const resumeKey = `${session.id}:${responseId}`;
  if (activeResumeKeys.has(resumeKey)) return false;
  const resumeOwner = {};
  activeResumeKeys.set(resumeKey, resumeOwner);

  try {
    return await resumeActiveResponseInner(session, responseId, options, resumeOwner);
  } finally {
    if (activeResumeKeys.get(resumeKey) === resumeOwner) {
      activeResumeKeys.delete(resumeKey);
    }
  }
};

const resumeActiveResponseInner = async (session, responseId, options, resumeOwner) => {
  const resumeKey = `${session.id}:${responseId}`;
  const ownsResumeKey = () => activeResumeKeys.get(resumeKey) === resumeOwner;
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
        app.handleAuthFailure();
        return false;
      }
      // If the snapshot is briefly unavailable, fall back to the existing
      // event replay path rather than failing the reconnect outright.
    }
  }
  if (!ownsResumeKey()) {
    setStreaming(Boolean(state.currentStreamResponseId));
    return false;
  }

  let streamState = recoveredFromSnapshot
    ? createResponseStreamState(session)
    : (options.streamState || createResponseStreamState(session));
  let consecutiveHttpFailures = 0;
  let consecutiveHeartbeatAborts = 0;
  let reconnectReason = 'stream-ended';
  const recovery = app.createResumableStreamRecovery({
    key: resumeKey,
    getCursor: () => responseEventSequence(session, responseId),
    isTerminal: () => !ownsResumeKey() || session.activeResponseId !== responseId,
    heartbeat: true,
    onStatus: ({ phase, reason, delay, attempt }) => {
      setReconnectDiagnostic(phase, reason, delay);
      if (phase === 'waiting' || phase === 'offline') {
        setConnectionState(
          phase === 'offline' ? 'Offline — response is safe; reconnecting when online' : streamReconnectLabel(attempt),
          'bad'
        );
      }
    },
  });

  for (;;) {
    if (!ownsResumeKey()) {
      setStreaming(Boolean(state.currentStreamResponseId));
      return false;
    }
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
      setConnectionState('Checking session state\u2026', 'bad');
      setReconnectDiagnostic('checking-state', reconnectReason, 0);
      try {
        await app.syncActiveSessionFromServer(session, false);
      } catch (err) {
        // This is a fallback inside an infinite recovery loop. A transient
        // state/transcript error is unknown, not terminal: keep reconnecting.
        console.warn('[stream] session-state reconciliation failed; continuing reconnect', err);
      }
      if (!ownsResumeKey()) {
        setStreaming(Boolean(state.currentStreamResponseId));
        return false;
      }
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

    recovery.noteAttempt();
    try {
      const replayAfterSequence = responseEventSequence(session, responseId);
      const response = await app.apiFetch(`${UI_PREFIX}/v1/responses/${encodeURIComponent(responseId)}/events?after=${encodeURIComponent(replayAfterSequence)}`, {
        headers: requestHeaders(session.id),
        signal: controller.signal
      }, { policy: app.API_FETCH_POLICY.stream, timeoutMs: 0, retries: 0 });
      if (!ownsResumeKey()) {
        if (state.abortController === controller) state.abortController = null;
        try { await response.body?.cancel(); } catch (_) { /* response may already be closed */ }
        setStreaming(Boolean(state.currentStreamResponseId));
        return false;
      }
      if (!response.ok) {
        throw await normalizeError(response);
      }
      if (!response.body) {
        throw { status: 0, message: 'No response body from server.' };
      }

      consecutiveHttpFailures = 0;
      recovery.noteConnected();
      setConnectionState('', '');
      setReconnectDiagnostic('connected', '', 0);
      const streamGeneration = state.streamGeneration;
      streamActivityBaseline = Number(state.lastEventTime || 0);
      const replayThroughSequence = Math.max(
        replayAfterSequence,
        Number(response.headers?.get?.('X-Term-LLM-Replay-Through')) || 0
      );
      const result = await consumeResponseStream(response.body, session, streamState, {
        generation: streamGeneration,
        responseId,
        abortController: controller,
        replayAfterSequence,
        replayThroughSequence
      });
      if (streamHadActivitySince(streamActivityBaseline)) {
        recovery.noteActivity();
        consecutiveHeartbeatAborts = 0;
      }
      if (controller._heartbeatAbort) {
        // Reader cancellation ends the stream without throwing. Preserve the
        // same reconnect accounting used by the pre-response abort path.
        consecutiveHeartbeatAborts += 1;
        reconnectReason = 'heartbeat-stale';
      }
      if (state.abortController === controller) {
        state.abortController = null;
      }

      if (result.stale) {
        setStreaming(Boolean(state.currentStreamResponseId));
        return false;
      }
      if (result.error?.recoverableStreamGap || result.error?.recoverableStreamApplyFailure || result.error?.recoverableReplayInterrupted) {
        const sequenceBeforeRecovery = responseEventSequence(session, responseId);
        try {
          const snapshot = await recoverResponseStateFromSnapshot(session, responseId);
          streamState = createResponseStreamState(session);
          if (snapshot?.status !== 'in_progress') {
            setStreaming(Boolean(state.currentStreamResponseId));
            return true;
          }
          if (responseEventSequence(session, responseId) > sequenceBeforeRecovery) {
            continue;
          }
        } catch (snapshotErr) {
          if (snapshotErr?.status === 401) {
            app.handleAuthFailure();
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
        recovery.noteActivity();
        consecutiveHttpFailures = 0;
        consecutiveHeartbeatAborts = 0;
      }

      const controllerAborted = Boolean(controller.signal?.aborted || controller._heartbeatAbort || err?.name === 'AbortError');
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
        reconnectReason = 'heartbeat-stale';
      } else {
        consecutiveHttpFailures += 1;
        consecutiveHeartbeatAborts = 0;
        reconnectReason = err?.status ? `http-${err.status}` : 'network-error';
      }
      if (err?.status === 401) {
        app.handleAuthFailure();
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
            app.handleAuthFailure();
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

    if (session.activeResponseId !== responseId) {
      setStreaming(Boolean(state.currentStreamResponseId));
      return true;
    }
    const wakeReason = await recovery.wait(reconnectReason);
    if (wakeReason === 'detached' || !ownsResumeKey()) {
      setStreaming(Boolean(state.currentStreamResponseId));
      return false;
    }
  }
};

const cancelActiveResponse = async (session) => {
  const responseId = String(session?.activeResponseId || state.currentStreamResponseId || '').trim();

  // Instant UI feedback: fully detach the local stream before the server POST.
  // Merely aborting its controller leaves the controller registered until the
  // async reader unwinds. A fast /cancel response can then schedule a state poll
  // that refuses to run because it still sees that stale controller.
  // Detaching synchronously makes Stop final locally; the POST below still
  // drives the authoritative server-side cancel.
  state.expectCanceledRun = true;
  detachResponseStream();
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
    response = await app.apiFetch(`${UI_PREFIX}/v1/responses/${encodeURIComponent(responseId)}/cancel`, {
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
  if (streaming !== wasStreaming) app.updateSlashCommands?.();
  elements.promptInput.disabled = false;
  elements.sendBtn.disabled = false;
  app.updateSendButtonState();
  elements.stopBtn.classList.toggle('visible', streaming && (Boolean(state.abortController) || Boolean(state.currentStreamResponseId)));
  app.updateVoiceUI();
  updateSessionUsageDisplay(getActiveSession());
  if (!streaming) {
    flushStreamPersistence();
    const shouldRestoreFocus = state.restorePromptFocus;
    state.restorePromptFocus = false;
    // A completed run must not pull focus out of a control the user moved into.
    const active = document.activeElement;
    if (shouldRestoreFocus && (!active || active === document.body || active === elements.promptInput)) {
      elements.promptInput.focus();
    }
  }
};


const runtimeHasPendingAskUser = (syncResult, callId) => {
  const runtimeState = app.runtimeStateFromSyncResult(syncResult);
  const normalizedCallId = String(callId || '').trim();
  if (!normalizedCallId || !runtimeState || typeof runtimeState !== 'object') return false;
  const prompts = Array.isArray(runtimeState.pending_ask_users)
    ? runtimeState.pending_ask_users
    : (runtimeState.pending_ask_user ? [runtimeState.pending_ask_user] : []);
  return prompts.some((item) => String(item?.call_id || '').trim() === normalizedCallId);
};

const runtimeHasPendingApproval = (syncResult, approvalId) => {
  const runtimeState = app.runtimeStateFromSyncResult(syncResult);
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

const recoverInterruptFailure = async (session, prompt, messageId, attachments = [], sendOptions = {}) => {
  const pending = state.pendingInterjections.find((entry) => entry.messageId === messageId);
  if (pending?.cancelRequested) {
    app.removePendingInterjectionById(messageId);
    app.discardPendingInterruptCommit(messageId);
    state.queuedInterrupts = state.queuedInterrupts.filter((entry) => entry.messageId !== messageId);
    persistAndRefreshShell();
    return true;
  }

  const syncResult = await refreshSessionFromServerTruth(session, true);
  const runtimeState = app.runtimeStateFromSyncResult(syncResult);
  if (!runtimeState) {
    return false;
  }
  if (app.runtimeHasActiveRun(runtimeState)) {
    app.discardPendingInterruptCommit(messageId);
    app.updatePendingInterjectionAction(messageId, 'queue');
    app.queueInterruptFollowUp(session.id, prompt, messageId, attachments, sendOptions);
    persistAndRefreshShell();
    if (!sendOptions.preserveComposer) app.clearDraftMessageForSession(session.id);
    return true;
  }

  // syncActiveSessionFromServer is expected to clear stale local busy state
  // before retrying the prompt as a fresh response.
  app.discardPendingInterruptCommit(messageId);
  app.removePendingInterjectionById(messageId);
  await app.sendMessage({
    ...sendOptions,
    prompt,
    attachments,
    reuseMessageId: messageId,
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
    created: Date.now(),
    transient: true
  };
  appendStreamMessageNode(session, message);
};


Object.assign(app, {
  requestHeaders,
  normalizeError,
  rebaseStreamAssetURL,
  forceSidebarStatusRefreshSoon,
  normalizeEffortForCompare,
  clearTerminalPendingEffort,
  hasSessionContinuationContext,
  clearRuntimeSelectionIntent,
  classifyRecoverableContinuationFailure,
  isTransientPreResponsePostError,
  parseSSEStream,
  sleep,
  streamReconnectDelay,
  streamReconnectLabel,
  heartbeatUploadGraceThreshold,
  setActiveResponseTracking,
  attachResponseStream,
  detachResponseStream,
  clearActiveResponseTracking,
  notifyTranscriptTerminal,
  trackTranscriptTerminalHandoff,
  awaitTranscriptTerminalHandoff,
  createResponseStreamState,
  applyResponseRecoverySnapshot,
  responseEventSequence,
  consumeResponseStream,
  scheduleStreamPersistence,
  flushStreamPersistence,
  scheduleStreamScroll,
  HEARTBEAT_STALE_THRESHOLD,
  HEARTBEAT_TAKEOVER_GRACE,
  HEARTBEAT_ABORT_REASON,
  wakeResponseReconnect,
  resumeActiveResponse,
  cancelActiveResponse,
  isSessionVisible,
  updateVisibleToolGroupNode,
  enqueueVisibleAssistantStreamUpdate,
  scheduleVisibleStreamScroll,
  responseStreamOwnerId,
  clearProviderRetryForEvent,
  appendStreamMessageNode,
  updateVisibleUserNode,
  finalizeVisibleAssistantStreamRender,
  scrollVisibleStreamToBottom,
  markRuntimeSelectionIntent,
  effectiveEffortForCompare,
  sessionHasQueueableActiveRun,
  setSessionPendingEffort,
  clearSessionPendingEffort,
  refreshSessionFromServerTruth,
  setStreaming,
  recoverInterruptFailure,
  recoverInterruptConflict,
  addErrorMessage
});
})();
