(() => {
'use strict';

const app = window.TermLLMApp;
const {
  UI_PREFIX, STORAGE_KEYS, state, elements, generateId, sanitizeInterruptState, sanitizeMessage, syncTokenCookie, truncate, saveSessions,
  getActiveSession, createSession, scrollToBottom, setConnectionState, sessionSlug, updateURL,
  persistAndRefreshShell, updateSessionUsageDisplay, refreshRelativeTimes, requestHeaders: _unusedRequestHeaders, updateAssistantNode, updateUserNode,
  updateToolNode, updateToolGroupNode, createMessageNode, createToolGroupNode, renderSidebar, renderMessages, maybeNotifyResponseComplete,
  enqueueAssistantStreamUpdate, finalizeAssistantStreamRender,
  subscribeToPush, shouldAutoSubscribeToPush, applyTextDirection, shouldSuppressPromptAutoFocus, setSessionOptimisticBusy, setSessionServerActiveRun
} = app;

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

  return headers;
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
  return Array.isArray(data.data)
    ? data.data.map((m) => m?.id).filter(Boolean)
    : [];
};

const parseSSEStream = async (stream, onEvent) => {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  let buffer = '';

  const processBlock = async (block) => {
    if (!block.trim()) return true;

    let eventName = '';
    const dataLines = [];
    const lines = block.split('\n');

    for (const line of lines) {
      if (line.startsWith('event:')) {
        eventName = line.slice(6).trim();
      } else if (line.startsWith('data:')) {
        dataLines.push(line.slice(5).trimStart());
      }
    }

    return onEvent(eventName, dataLines.join('\n'));
  };

  while (true) {
    const { value, done } = await reader.read();
    if (done) break;

    buffer += decoder.decode(value, { stream: true }).replace(/\r/g, '');
    state.lastEventTime = Date.now();

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

const cancelResponseBody = async (response) => {
  if (!response?.body || typeof response.body.cancel !== 'function') return;
  try {
    await response.body.cancel();
  } catch {
    // Ignore body cancellation failures; the resumable /events stream is the
    // authoritative source once we know the response ID.
  }
};

const sleep = (ms) => new Promise((resolve) => window.setTimeout(resolve, ms));

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
const HEARTBEAT_STALE_THRESHOLD = 45000; // Backend pings every 20s
const HEARTBEAT_ABORT_REASON = 'heartbeat';

const startHeartbeatMonitor = () => {
  stopHeartbeatMonitor();
  state.lastEventTime = Date.now();
  heartbeatTimerId = window.setInterval(() => {
    if (!state.abortController || !state.currentStreamResponseId) {
      stopHeartbeatMonitor();
      return;
    }
    // While an ask_user or approval modal is open the server is intentionally
    // silent — it is blocked waiting for user input.  A stale-heartbeat abort
    // here would needlessly reconnect the SSE stream and replay the prompt
    // event, resetting any partial answer the user has typed.
    if (state.askUser || state.approval) return;
    if (Date.now() - state.lastEventTime > HEARTBEAT_STALE_THRESHOLD) {
      if (state.abortController) {
        state.abortController._heartbeatAbort = true;
        state.abortController.abort(HEARTBEAT_ABORT_REASON);
      }
    }
  }, 10000);
};

const stopHeartbeatMonitor = () => {
  if (heartbeatTimerId !== null) {
    clearInterval(heartbeatTimerId);
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

  if (!targetId || currentId === targetId) {
    session.activeResponseId = null;
    session.lastSequenceNumber = 0;
  }
  if (
    !targetId
    || (
      state.currentStreamSessionId === String(session.id || '').trim()
      && (!state.currentStreamResponseId || state.currentStreamResponseId === targetId)
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

const createResponseStreamState = (session) => {
  let currentToolGroup = [...session.messages].reverse().find((message) => (
    message.role === 'tool-group' && message.status === 'running'
  )) || null;
  let currentAssistantMessage = null;

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
    elements.messages.appendChild(createMessageNode(msg));
    currentAssistantMessage = msg;
    return msg;
  };

  const closeToolGroup = () => {
    if (!currentToolGroup) return;
    currentToolGroup.tools.forEach((tool) => { tool.status = 'done'; });
    currentToolGroup.status = 'done';
    updateToolGroupNode(currentToolGroup);
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
    }
  };
};

const applyResponseStreamEvent = (session, streamState, event, payload) => {
  updateResponseSequence(session, payload);

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
      updateSessionUsageDisplay(session);
    }
    return { terminal: false };
  }

  if (event === 'response.output_text.delta') {
    const delta = String(payload.delta || '');
    if (delta) {
      streamState.closeToolGroup();
      const msg = streamState.ensureAssistantMessage();
      msg.content += delta;
      scheduleStreamPersistence();
      enqueueAssistantStreamUpdate(msg);
    }
    return { terminal: false };
  }

  if (event === 'response.output_text.new_segment') {
    streamState.closeToolGroup();
    if (streamState.currentAssistantMessage?.content) {
      finalizeAssistantStreamRender(streamState.currentAssistantMessage);
    }
    streamState.currentAssistantMessage = null;
    return { terminal: false };
  }

  if (event === 'response.output_item.added') {
    const item = payload.item;
    if (item?.type === 'function_call') {
      if (streamState.currentAssistantMessage?.content) {
        finalizeAssistantStreamRender(streamState.currentAssistantMessage);
      }
      const toolEntry = {
        id: item.call_id || generateId('tool'),
        name: String(item.name || 'tool'),
        arguments: String(item.arguments || ''),
        status: 'running',
        created: Date.now()
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
        elements.messages.appendChild(createToolGroupNode(streamState.currentToolGroup));
      } else {
        streamState.currentToolGroup.tools.push(toolEntry);
        updateToolGroupNode(streamState.currentToolGroup);
      }

      streamState.currentAssistantMessage = null;
      saveSessions();
      scrollToBottom();
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
      }
      updateToolGroupNode(streamState.currentToolGroup);
      saveSessions();
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
        updateToolGroupNode(streamState.currentToolGroup);
      }
      if (streamState.currentToolGroup.tools.every((tool) => tool.status === 'done')) {
        streamState.currentToolGroup.status = 'done';
        updateToolGroupNode(streamState.currentToolGroup);
      }
    }
    if (payload.images && payload.images.length > 0) {
      const msg = streamState.ensureAssistantMessage();
      payload.images.forEach((url) => {
        msg.content += `\n\n![Generated Image](${url})\n`;
      });
      enqueueAssistantStreamUpdate(msg);
    }
    saveSessions();
    scheduleStreamScroll();
    return { terminal: false };
  }

  if (event === 'response.interjection') {
    const interjectionText = String(payload.text || '').trim();
    if (interjectionText) {
      resolvePendingInterruptCommit(session.id, interjectionText);
      saveSessions();
    }
    return { terminal: false };
  }

  if (event === 'response.completed') {
    const usage = payload?.response?.usage;
    streamState.closeToolGroup();
    markToolGroupsDone(session);

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
    updateSessionUsageDisplay(session);

    const lastAssistant = [...session.messages].reverse().find((message) => message.role === 'assistant');
    if (lastAssistant) {
      if (usage) lastAssistant.usage = usage;
      finalizeAssistantStreamRender(lastAssistant);
    }
    flushStreamPersistence();
    saveSessions();
    renderSidebar();
    app.refreshSidebarStatusPoll?.();
    void maybeNotifyResponseComplete(session, lastAssistant, responseId);
    scrollToBottom();
    return { terminal: true };
  }

  if (event === 'response.failed') {
    const errorMessage = payload?.error?.message || 'The response failed.';
    const lowered = errorMessage.toLowerCase();
    const canceledByInterrupt = state.expectCanceledRun && (
      lowered.includes('context canceled') ||
      lowered.includes('context cancelled') ||
      lowered.includes('cancelled') ||
      lowered.includes('canceled')
    );

    if (!canceledByInterrupt) {
      addErrorMessage(errorMessage, session);
    }
    state.expectCanceledRun = false;

    streamState.closeToolGroup();
    markToolGroupsDone(session);
    clearActiveResponseTracking(session, session.activeResponseId || state.currentStreamResponseId);
    setSessionOptimisticBusy(session, false);
    setSessionServerActiveRun(session, false);

    const lastAssistant = [...session.messages].reverse().find((message) => message.role === 'assistant');
    if (lastAssistant) finalizeAssistantStreamRender(lastAssistant);
    flushStreamPersistence();
    saveSessions();
    renderSidebar();
    app.refreshSidebarStatusPoll?.();
    scrollToBottom(true);
    return { terminal: true };
  }

  return { terminal: false };
};

const consumeResponseStream = async (stream, session, streamState) => {
  let sawTerminal = false;
  let sawDone = false;

  await parseSSEStream(stream, async (event, data) => {
    if (data === '[DONE]') {
      sawDone = true;
      streamState.closeToolGroup();
      markToolGroupsDone(session);
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

    const result = applyResponseStreamEvent(session, streamState, event, payload);
    if (result?.terminal) {
      sawTerminal = true;
    }
    return true;
  });

  return sawTerminal || sawDone || !session.activeResponseId;
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

const applyResponseRecoverySnapshot = (session, payload) => {
  if (!session || !payload || typeof payload !== 'object') return false;

  const recovery = payload.recovery;
  if (!recovery || typeof recovery !== 'object') return false;

  const rawMessages = Array.isArray(recovery.messages) ? recovery.messages : [];
  const recoveredMessages = rawMessages
    .map((message) => sanitizeMessage(message))
    .filter(Boolean);

  let anchorIndex = -1;
  for (let i = session.messages.length - 1; i >= 0; i -= 1) {
    if (session.messages[i]?.role === 'user') {
      anchorIndex = i;
      break;
    }
  }

  const preserved = anchorIndex >= 0
    ? session.messages.slice(0, anchorIndex + 1)
    : [];
  session.messages = preserved.concat(recoveredMessages);

  const nextSeq = Number(payload.last_sequence_number ?? recovery.sequence_number ?? session.lastSequenceNumber ?? 0);
  if (Number.isFinite(nextSeq) && nextSeq >= 0) {
    session.lastSequenceNumber = nextSeq;
  }

  const responseId = String(payload.id || session.activeResponseId || state.currentStreamResponseId || '').trim();
  const sessionUsage = payload.session_usage;
  if (sessionUsage) {
    session.sessionUsage = sessionUsage;
  }
  updateSessionUsageDisplay(session);

  if (payload.status === 'completed') {
    if (responseId) {
      session.lastResponseId = responseId;
    }
    clearActiveResponseTracking(session, responseId);
    setSessionOptimisticBusy(session, false);
    setSessionServerActiveRun(session, false);
  } else if (payload.status === 'failed') {
    clearActiveResponseTracking(session, responseId);
    setSessionOptimisticBusy(session, false);
    setSessionServerActiveRun(session, false);
  } else if (responseId) {
    setActiveResponseTracking(session, responseId, session.lastSequenceNumber);
    setSessionOptimisticBusy(session, true);
  }

  saveSessions();
  renderSidebar();
  app.refreshSidebarStatusPoll?.();
  if (session.id === state.activeSessionId) {
    renderMessages(true);
  } else {
    persistAndRefreshShell();
  }
  return true;
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

  let streamState = options.streamState || createResponseStreamState(session);
  let consecutiveHttpFailures = 0;

  for (let attempt = 0; ; attempt += 1) {
    if (session.activeResponseId !== responseId) {
      setStreaming(Boolean(state.currentStreamResponseId));
      return true;
    }

    // After repeated HTTP failures, fall back to session-state polling to
    // detect whether the run has finished while we can't reach the event stream.
    if (consecutiveHttpFailures >= 5) {
      consecutiveHttpFailures = 0;
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
      const terminal = await consumeResponseStream(response.body, session, streamState);
      if (state.abortController === controller) {
        state.abortController = null;
      }

      if (session.activeResponseId !== responseId) {
        setStreaming(Boolean(state.currentStreamResponseId));
        return true;
      }
      if (terminal) {
        setStreaming(Boolean(state.currentStreamResponseId));
        return true;
      }
    } catch (err) {
      if (state.abortController === controller) {
        state.abortController = null;
      }

      if (err?.name === 'AbortError') {
        // Only retry if this was a heartbeat-triggered abort.
        // Intentional detach/session-switch aborts should exit immediately.
        if (!controller._heartbeatAbort) {
          setStreaming(Boolean(state.currentStreamResponseId));
          return false;
        }
        // Heartbeat abort: fall through to retry
      } else {
        consecutiveHttpFailures += 1;
      }
      if (err?.status === 401) {
        handleAuthFailure();
        return false;
      }
      if (err?.status === 409) {
        try {
          const snapshot = await fetchResponseSnapshot(session, responseId);
          applyResponseRecoverySnapshot(session, snapshot);
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

    setConnectionState(attempt < 3 ? 'Reconnecting\u2026' : `Reconnecting (attempt ${attempt + 1})\u2026`);
    if (session.activeResponseId !== responseId) {
      setStreaming(Boolean(state.currentStreamResponseId));
      return true;
    }
    await sleep(Math.min(1000 * Math.pow(1.5, Math.min(attempt, 6)), 8000));
  }
};

const cancelActiveResponse = async (session) => {
  const responseId = String(session?.activeResponseId || state.currentStreamResponseId || '').trim();
  if (!responseId) {
    console.warn('[cancel] no responseId available, aborting local controller only');
    if (state.abortController) {
      state.abortController.abort();
    }
    if (session?.id) {
      await refreshSessionFromServerTruth(session, true);
    }
    return;
  }

  state.expectCanceledRun = true;
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
  app.refreshNotificationUI();
  elements.authModal.classList.remove('hidden');
  elements.providerSelect.removeAttribute('tabindex');
  elements.modelSelect.removeAttribute('tabindex');
  elements.effortSelect?.removeAttribute('tabindex');
  elements.authTokenInput.removeAttribute('tabindex');
  elements.showHiddenSessionsInput?.removeAttribute('tabindex');

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

  // Provider/model selections are committed live via the change handlers.
  // Re-reading the modal DOM here can clobber a valid in-memory choice if the
  // selects are temporarily stale (for example while startup/model refresh work
  // is still settling). Persist the current state instead.
  const persistedProvider = state.selectedProvider;
  if (persistedProvider) {
    localStorage.setItem(STORAGE_KEYS.selectedProvider, persistedProvider);
  } else {
    localStorage.removeItem(STORAGE_KEYS.selectedProvider);
  }
  const persistedModel = state.selectedModel;
  if (persistedModel) {
    localStorage.setItem(STORAGE_KEYS.selectedModel, persistedModel);
  } else {
    localStorage.removeItem(STORAGE_KEYS.selectedModel);
  }
  const newEffort = elements.effortSelect ? elements.effortSelect.value : '';
  state.selectedEffort = newEffort;
  if (newEffort) {
    localStorage.setItem(STORAGE_KEYS.selectedEffort, newEffort);
  } else {
    localStorage.removeItem(STORAGE_KEYS.selectedEffort);
  }
  const showHiddenChanged = nextShowHiddenSessions !== state.showHiddenSessions;
  state.showHiddenSessions = nextShowHiddenSessions;
  localStorage.setItem(STORAGE_KEYS.showHiddenSessions, state.showHiddenSessions ? '1' : '0');
  app.updateHeader();

  if (state.authRequired && !token) {
    elements.authError.textContent = 'Token is required.';
    return;
  }

  const tokenChanged = token !== state.token;
  if (!tokenChanged) {
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
    state.providers = await fetchProviders(token);
    normalizeSelectedProvider();
    const models = await fetchModels(token, state.selectedProvider);
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

const renderProviderOptions = () => {
  const previous = state.selectedProvider;
  elements.providerSelect.innerHTML = '';

  const autoOption = document.createElement('option');
  autoOption.value = '';
  autoOption.textContent = 'Auto (server default)';
  elements.providerSelect.appendChild(autoOption);

  state.providers.filter((p) => p.configured || p.is_default).forEach((p) => {
    const option = document.createElement('option');
    option.value = p.name;
    option.textContent = p.name + (p.is_default ? ' (default)' : '');
    elements.providerSelect.appendChild(option);
  });

  elements.providerSelect.value = previous;
};

elements.providerSelect.addEventListener('change', async () => {
  const provider = elements.providerSelect.value;
  state.selectedProvider = provider;
  if (provider) {
    localStorage.setItem(STORAGE_KEYS.selectedProvider, provider);
  } else {
    localStorage.removeItem(STORAGE_KEYS.selectedProvider);
  }
  // Reset model selection when provider changes
  state.selectedModel = '';
  localStorage.removeItem(STORAGE_KEYS.selectedModel);
  try {
    state.models = await fetchModels('', provider);
  } catch {
    // Fall back to curated models from provider metadata
    const providerInfo = state.providers.find((p) => p.name === provider);
    state.models = providerInfo?.models?.length ? providerInfo.models : [];
  }
  renderModelOptions();
  app.updateHeader();
});

elements.modelSelect?.addEventListener('change', () => {
  const model = elements.modelSelect.value;
  state.selectedModel = model;
  if (model) {
    localStorage.setItem(STORAGE_KEYS.selectedModel, model);
  } else {
    localStorage.removeItem(STORAGE_KEYS.selectedModel);
  }
  app.updateHeader();
});

// Effort is a staged modal value: the dropdown only reflects the pending choice
// inside the settings modal and is committed to state/localStorage on Save
// (connectToken). Cancelling the modal discards the pending value; the next
// openAuthModal() resets the select from state.selectedEffort.

// ===== Model picker =====
const renderModelOptions = () => {
  const previous = state.selectedModel;
  elements.modelSelect.innerHTML = '';

  const autoOption = document.createElement('option');
  autoOption.value = '';
  autoOption.textContent = 'Auto (server default)';
  elements.modelSelect.appendChild(autoOption);

  state.models.forEach((id) => {
    const option = document.createElement('option');
    option.value = id;
    option.textContent = id;
    elements.modelSelect.appendChild(option);
  });

  if (previous && !state.models.includes(previous)) {
    const custom = document.createElement('option');
    custom.value = previous;
    custom.textContent = `${previous} (custom)`;
    elements.modelSelect.appendChild(custom);
  }

  elements.modelSelect.value = previous;
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

const autoGrowPrompt = () => {
  const el = elements.promptInput;
  applyTextDirection(el, el.value || '');
  el.style.height = 'auto';
  const next = Math.min(el.scrollHeight, 200);
  el.style.height = `${Math.max(48, next)}px`;
  el.style.overflowY = el.scrollHeight > 200 ? 'auto' : 'hidden';
};

// ===== File attachment =====
const renderAttachments = () => {
  const strip = elements.attachmentsStrip;
  strip.innerHTML = '';
  if (state.attachments.length === 0) {
    strip.style.display = 'none';
    return;
  }
  strip.style.display = 'flex';
  state.attachments.forEach((att) => {
    const chip = document.createElement('div');
    chip.className = 'attachment-chip';

    if (att.type.startsWith('image/')) {
      const img = document.createElement('img');
      img.src = att.dataURL;
      img.alt = att.name;
      chip.appendChild(img);
    }

    const name = document.createElement('span');
    name.className = 'att-name';
    name.textContent = att.name;
    name.title = `${att.name} (${(att.size / 1024).toFixed(1)} KB)`;
    chip.appendChild(name);

    const remove = document.createElement('button');
    remove.className = 'att-remove';
    remove.textContent = '\u00d7';
    remove.title = 'Remove';
    remove.addEventListener('click', () => {
      state.attachments = state.attachments.filter(a => a.id !== att.id);
      renderAttachments();
    });
    chip.appendChild(remove);

    strip.appendChild(chip);
  });
};

const MAX_ATTACHMENTS = 10;
const MAX_FILE_BYTES = 20 * 1024 * 1024; // 20 MB

const handleFiles = (fileList) => {
  const files = Array.from(fileList);
  for (const file of files) {
    if (state.attachments.length >= MAX_ATTACHMENTS) {
      alert(`Maximum ${MAX_ATTACHMENTS} attachments allowed.`);
      return;
    }
    if (file.size > MAX_FILE_BYTES) {
      alert(`${file.name} exceeds the 20 MB file size limit.`);
      continue;
    }
    const reader = new FileReader();
    reader.onload = () => {
      if (state.attachments.length >= MAX_ATTACHMENTS) return;
      const dataURL = reader.result;
      state.attachments.push({
        id: generateId('att'),
        name: file.name,
        type: file.type || 'application/octet-stream',
        size: file.size,
        dataURL
      });
      renderAttachments();
    };
    reader.readAsDataURL(file);
  }
};

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
  elements.sendBtn.classList.toggle('loading', streaming);
  elements.stopBtn.classList.toggle('visible', streaming && (Boolean(state.abortController) || Boolean(state.currentStreamResponseId)));
  updateVoiceUI();
  if (!streaming) {
    flushStreamPersistence();
    const shouldRestoreFocus = state.restorePromptFocus;
    state.restorePromptFocus = false;
    if (shouldRestoreFocus) {
      elements.promptInput.focus();
    }
  }
};

const queueInterruptFollowUp = (prompt, messageId) => {
  const normalizedMessageId = String(messageId || '').trim();
  if (normalizedMessageId && state.queuedInterrupts.some(entry => entry.messageId === normalizedMessageId)) {
    return;
  }
  state.queuedInterrupts.push({ prompt, messageId });
};

const trackPendingInterruptCommit = (sessionId, prompt, messageId) => {
  state.pendingInterruptCommits = state.pendingInterruptCommits.filter(entry => entry.messageId !== messageId);
  state.pendingInterruptCommits.push({ sessionId, prompt, messageId });
};

const resolvePendingInterruptCommit = (sessionId, prompt) => {
  const idx = state.pendingInterruptCommits.findIndex(entry => entry.sessionId === sessionId && entry.prompt === prompt);
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
    queueInterruptFollowUp(entry.prompt, entry.messageId);
  }
  state.pendingInterruptCommits = remaining;
};

const drainInterruptQueueIfIdle = (session) => {
  if (!session || session.id !== state.activeSessionId) return;
  if (state.streaming || state.abortController) return;
  requeueUncommittedInterrupts(session);
  if (state.queuedInterrupts.length > 0) {
    const queued = state.queuedInterrupts.shift();
    elements.promptInput.value = queued.prompt;
    autoGrowPrompt();
    void sendMessage({ prompt: queued.prompt, attachments: [], reuseMessageId: queued.messageId });
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

const createPendingInterruptMessage = (session, prompt) => {
  const message = {
    id: generateId('msg'),
    role: 'user',
    content: prompt,
    created: Date.now(),
    interruptState: 'evaluating'
  };
  session.messages.push(message);

  const emptyState = elements.messages.querySelector('.empty-state');
  if (emptyState) emptyState.remove();
  elements.messages.appendChild(createMessageNode(message));
  return message;
};

const interruptActiveRun = async (session, prompt, messageId) => {
  const response = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(session.id)}/interrupt`, {
    method: 'POST',
    headers: requestHeaders(session.id),
    body: JSON.stringify({ message: prompt })
  });
  if (!response.ok) {
    throw await normalizeError(response);
  }

  const payload = await response.json();
  const actionRaw = String(payload.action || 'queue').toLowerCase();
  const action = (actionRaw === 'interject' || actionRaw === 'cancel' || actionRaw === 'queue')
    ? actionRaw
    : 'queue';

  setInterruptMessageState(session, messageId, action);

  if (action === 'interject') {
    trackPendingInterruptCommit(session.id, prompt, messageId);
  } else {
    discardPendingInterruptCommit(messageId);
  }

  if (action === 'cancel' || action === 'queue') {
    queueInterruptFollowUp(prompt, messageId);
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

const recoverInterruptConflict = async (session, prompt, messageId) => {
  const runtimeState = await refreshSessionFromServerTruth(session, true);
  if (!runtimeState) {
    return false;
  }
  if (runtimeHasActiveRun(runtimeState)) {
    discardPendingInterruptCommit(messageId);
    setInterruptMessageState(session, messageId, 'queue');
    queueInterruptFollowUp(prompt, messageId);
    persistAndRefreshShell();
    scrollToBottom(true);
    return true;
  }

  discardPendingInterruptCommit(messageId);
  await sendMessage({
    prompt,
    attachments: [],
    reuseMessageId: messageId
  });
  return true;
};

const addErrorMessage = (text, session) => {
  const message = {
    id: generateId('msg'),
    role: 'error',
    content: text,
    created: Date.now()
  };
  session.messages.push(message);
  elements.messages.appendChild(createMessageNode(message));
};

const markToolGroupsDone = (session) => {
  session.messages.forEach(m => {
    if (m.role === 'tool-group' && m.status === 'running') {
      m.tools.forEach(t => { t.status = 'done'; });
      m.status = 'done';
      updateToolGroupNode(m);
    }
    if (m.role === 'tool' && m.status === 'running') {
      m.status = 'done';
      updateToolNode(m);
    }
  });
};

// Rebuild a full conversation input array from locally-stored session messages.
// Used to recover when previous_response_id has expired server-side.
const rebuildInputFromSession = (session, currentInput) => {
  const input = [];
  for (const msg of session.messages) {
    if (msg.role === 'user' && !msg.askUser) {
      if (msg.attachments && msg.attachments.length > 0) {
        const parts = [];
        for (const att of msg.attachments) {
          if (att.type && att.type.startsWith('image/') && att.dataURL) {
            parts.push({ type: 'input_image', image_url: att.dataURL, filename: att.name });
          } else if (att.dataURL) {
            parts.push({ type: 'input_file', file_data: att.dataURL, filename: att.name });
          }
        }
        if (msg.content) parts.push({ type: 'input_text', text: msg.content });
        input.push({ type: 'message', role: 'user', content: parts });
      } else {
        input.push({ type: 'message', role: 'user', content: msg.content || '' });
      }
    } else if (msg.role === 'assistant') {
      input.push({ type: 'message', role: 'assistant', content: msg.content || '' });
    }
    // Skip tool/tool-group/error messages — they're internal
  }
  // Replace the last user message input with the current one (which may have
  // attachments encoded differently), or append if not already present.
  if (input.length > 0 && input[input.length - 1].role === 'user') {
    input[input.length - 1].content = currentInput;
  } else {
    input.push({ type: 'message', role: 'user', content: currentInput });
  }
  return input;
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
  const sessionBusy = state.streaming || (session && session.activeResponseId);
  if (sessionBusy) {
    if (pendingAttachments.length > 0) {
      alert('Attachments are not supported while a run is active.');
      return;
    }

    const pendingMessage = createPendingInterruptMessage(session, prompt);
    persistAndRefreshShell();
    scrollToBottom(true);

    elements.promptInput.value = '';
    autoGrowPrompt();

    try {
      await interruptActiveRun(session, prompt, pendingMessage.id);
    } catch (err) {
      if (err?.status === 409) {
        try {
          const recovered = await recoverInterruptConflict(session, prompt, pendingMessage.id);
          if (recovered) {
            return;
          }
        } catch (recoveryErr) {
          err = recoveryErr;
        }
      }

      discardPendingInterruptCommit(pendingMessage.id);
      setInterruptMessageState(session, pendingMessage.id, 'error');
      const message = err?.message || 'Failed to interrupt active run.';
      addErrorMessage(message, session);
      if (err?.status === 401) {
        handleAuthFailure();
      }
      elements.promptInput.value = prompt;
      autoGrowPrompt();
      persistAndRefreshShell();
      scrollToBottom(true);
    }
    return;
  }

  if (!session) {
    session = createSession();
    state.sessions.unshift(session);
    state.activeSessionId = session.id;
    state.draftSessionActive = false;
    updateURL(sessionSlug(session));
  }

  const reuseMessageId = typeof options.reuseMessageId === 'string' ? options.reuseMessageId : '';
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

  if (pendingAttachments.length > 0) {
    userMessage.attachments = pendingAttachments.map(a => ({
      name: a.name,
      type: a.type,
      dataURL: a.dataURL
    }));
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
  const controller = new AbortController();
  const sendGeneration = state.streamGeneration;
  attachResponseStream(session, '', controller);
  setStreaming(true);
  app.refreshSidebarStatusPoll?.();
  const streamState = createResponseStreamState(session);

  // Build input content: plain string or array with file/image parts
  let inputContent;
  if (pendingAttachments.length > 0) {
    const contentParts = [];
    for (const att of pendingAttachments) {
      if (att.type.startsWith('image/')) {
        contentParts.push({ type: 'input_image', image_url: att.dataURL, filename: att.name });
      } else {
        contentParts.push({ type: 'input_file', file_data: att.dataURL, filename: att.name });
      }
    }
    if (prompt) {
      contentParts.push({ type: 'input_text', text: prompt });
    }
    inputContent = contentParts;
  } else {
    inputContent = prompt;
  }

  const body = {
    stream: true,
    input: [{ type: 'message', role: 'user', content: inputContent }]
  };

  if (session.lastResponseId) {
    body.previous_response_id = session.lastResponseId;
  } else if (session.messages.length > 1) {
    body.input = rebuildInputFromSession(session, inputContent);
  }

  if (state.selectedModel) {
    body.model = state.selectedModel;
  }
  if (state.selectedEffort) {
    body.reasoning_effort = state.selectedEffort;
  }
  if (!session.provider && state.selectedProvider) {
    session.provider = state.selectedProvider;
  }
  if (session.provider) {
    body.provider = session.provider;
  }

  try {
    let response = await fetch(`${UI_PREFIX}/v1/responses`, {
      method: 'POST',
      headers: {
        ...requestHeaders(session.id),
        'X-Term-LLM-UI': '1'
      },
      body: JSON.stringify(body),
      signal: controller.signal
    });

    // Recovery: if previous_response_id expired, rebuild conversation from
    // local messages and retry without chaining.
    if (!response.ok && body.previous_response_id) {
      const errData = await response.json().catch(() => null);
      const errMsg = errData?.error?.message || '';
      if (errMsg.includes('not found') && errMsg.includes('previous_response_id')) {
        delete body.previous_response_id;
        session.lastResponseId = null;
        body.input = rebuildInputFromSession(session, inputContent);

        response = await fetch(`${UI_PREFIX}/v1/responses`, {
          method: 'POST',
          headers: {
            ...requestHeaders(session.id),
            'X-Term-LLM-UI': '1'
          },
          body: JSON.stringify(body),
          signal: controller.signal
        });
      }
    }

    const headerResponseId = String(response.headers.get('x-response-id') || '').trim();
    const headerSessionNumber = Number(response.headers.get('x-session-number') || 0);
    if (headerSessionNumber > 0 && session.number !== headerSessionNumber) {
      session.number = headerSessionNumber;
      updateURL(sessionSlug(session));
    }
    if (!response.ok) {
      throw await normalizeError(response);
    }

    if (headerResponseId) {
      setActiveResponseTracking(session, headerResponseId, 0);
      saveSessions();

      // The POST stream is only used to create the run and surface the
      // response ID. After that, use the resumable events endpoint so a
      // stalled transport does not strand the UI until reload.
      await cancelResponseBody(response);
      await resumeActiveResponse(session, { streamState, responseId: headerResponseId });
    } else {
      if (!response.body) {
        throw { status: 0, message: 'No response body from server.' };
      }

      const terminal = await consumeResponseStream(response.body, session, streamState);
      if (!terminal && session.activeResponseId) {
        await resumeActiveResponse(session, { streamState });
      }
    }

    if (sendGeneration === state.streamGeneration) {
      const lastAssistant = [...session.messages].reverse().find(m => m.role === 'assistant');
      if (lastAssistant) updateAssistantNode(lastAssistant);
      persistAndRefreshShell();
      scrollToBottom();
    }
  } catch (err) {
    streamState.closeToolGroup();
    markToolGroupsDone(session);

    if (err?.name === 'AbortError') {
      persistAndRefreshShell();
      return;
    }

    // If the stream was detached (New Chat, switched session), don't
    // touch DOM or streaming state for this session.
    if (sendGeneration !== state.streamGeneration) {
      return;
    }

    const lastAssistant = [...session.messages].reverse().find(m => m.role === 'assistant');
    if (lastAssistant) updateAssistantNode(lastAssistant);

    if (session.activeResponseId) {
      await resumeActiveResponse(session, { streamState });
      persistAndRefreshShell();
      return;
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
    }
    setStreaming(stillActive);
    refreshRelativeTimes();
    if (stillActive) {
      return;
    }

    drainInterruptQueueIfIdle(session);
  }
};

Object.assign(app, {
  requestHeaders,
  normalizeError,
  fetchProviders,
  fetchModels,
  parseSSEStream,
  sleep,
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
  renderProviderOptions,
  renderModelOptions,
  autoGrowPrompt,
  updateVoiceUI,
  startVoiceRecording,
  stopVoiceRecording,
  toggleVoiceRecording,
  renderAttachments,
  MAX_ATTACHMENTS,
  MAX_FILE_BYTES,
  handleFiles,
  setStreaming,
  queueInterruptFollowUp,
  trackPendingInterruptCommit,
  resolvePendingInterruptCommit,
  discardPendingInterruptCommit,
  requeueUncommittedInterrupts,
  drainInterruptQueueIfIdle,
  setInterruptMessageState,
  createPendingInterruptMessage,
  interruptActiveRun,
  addErrorMessage,
  markToolGroupsDone,
  sendMessage
});
})();
