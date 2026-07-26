'use strict';

(function initAppSkills() {

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
const { requestHeaders, normalizeError, parseSSEStream, sleep, streamReconnectDelay, setActiveResponseTracking, isSessionVisible, appendStreamMessageNode, updateVisibleUserNode, scrollVisibleStreamToBottom, resumeActiveResponse, setStreaming, drainInterruptQueueIfIdle, addErrorMessage } = app;

const skillRunStore = () => {
  if (!state.skillRunsById || typeof state.skillRunsById !== 'object') {
    state.skillRunsById = {};
  }
  return state.skillRunsById;
};

const skillRunAPIURL = (value) => {
  const path = String(value || '').trim();
  if (!path) return '';
  if (/^https?:\/\//i.test(path) || path.startsWith(`${UI_PREFIX}/`)) return path;
  if (path.startsWith('/')) return `${UI_PREFIX}${path}`;
  return `${UI_PREFIX}/${path}`;
};

const skillRunIsTerminal = (status) => ['complete', 'failed', 'cancelled'].includes(String(status || '').toLowerCase());

const skillRunMessages = new Map();

const skillRunMessageFor = (session, run) => {
  if (!session || !run?.id) return null;
  let message = skillRunMessages.get(`${session.id}:${run.id}`) || null;
  if (!message) {
    message = {
      id: `skill-run-${run.id}`,
      role: 'skill-run',
      runId: run.id,
      sessionId: run.sessionId || session.id,
      skill: run.skill || 'skill',
      agent: run.agent || '',
      status: run.status || 'running',
      progress: '',
      output: run.output || '',
      error: '',
      childSessionId: run.childSessionId || '',
      created: run.startedAt ? Date.parse(run.startedAt) || Date.now() : Date.now(),
    };
    skillRunMessages.set(`${session.id}:${run.id}`, message);
    appendStreamMessageNode(session, message);
  }
  return message;
};

const renderSkillRun = (run) => {
  const session = state.sessions.find((entry) => entry.id === run?.sessionId) || null;
  if (!session) return;
  const message = skillRunMessageFor(session, run);
  if (!message) return;
  message.skill = run.skill || message.skill;
  message.agent = run.agent || message.agent;
  message.status = run.status || message.status;
  message.progress = run.progress || '';
  message.output = run.output || '';
  message.error = run.error || '';
  message.childSessionId = run.childSessionId || message.childSessionId;
  if (run.startedAt && run.completedAt) {
    const start = Date.parse(run.startedAt);
    const end = Date.parse(run.completedAt);
    if (Number.isFinite(start) && Number.isFinite(end) && end >= start) message.durationMs = end - start;
  }
  message.content = [message.status, message.progress, message.output, message.error].filter(Boolean).join('\n');
  if (isSessionVisible(session)) updateVisibleUserNode(session, message);
  persistAndRefreshShell();
};

const progressTextForSkillRun = (data) => {
  if (!data || typeof data !== 'object') return '';
  const type = String(data.Type || data.type || '').toLowerCase();
  const phase = String(data.Phase || data.phase || '').trim();
  const text = String(data.Text || data.text || '').trim();
  const tool = String(data.ToolName || data.tool_name || '').trim();
  if (phase) return phase;
  if (type === 'tool_start' && tool) return `Running ${tool}`;
  if (type === 'tool_end' && tool) return `${tool} ${data.Success === false || data.success === false ? 'failed' : 'complete'}`;
  if (text) return text;
  return type.replaceAll('_', ' ');
};

const applySkillRunEvent = (run, envelope) => {
  if (!run || !envelope || typeof envelope !== 'object') return false;
  const sequence = Number(envelope.sequence || 0);
  if (sequence > 0 && sequence <= Number(run.lastSequence || 0)) return false;
  if (sequence > 0) run.lastSequence = sequence;
  const type = String(envelope.type || '').trim();
  const data = envelope.data && typeof envelope.data === 'object' ? envelope.data : {};
  if (type === 'skill_run.created') {
    run.skill = String(data.skill || run.skill || 'skill');
    run.agent = String(data.agent || run.agent || '');
    run.childSessionId = String(data.child_session_id || run.childSessionId || '');
    run.status = 'running';
  } else if (type === 'skill_run.progress') {
    run.progress = progressTextForSkillRun(data);
  } else if (type === 'skill_run.completed') {
    run.status = String(data.status || 'complete');
    run.output = String(data.output || '');
    run.error = String(data.error || '');
    run.childSessionId = String(data.child_session_id || run.childSessionId || '');
    run.completedAt = envelope.at || new Date().toISOString();
  }
  renderSkillRun(run);
  return true;
};

const followSkillRun = async (run) => {
  if (!run || !run.id || skillRunIsTerminal(run.status) || run.following) return;
  run.following = true;
  let retryAttempt = 0;
  try {
    while (!skillRunIsTerminal(run.status)) {
      const controller = new AbortController();
      run.controller = controller;
      try {
        const separator = run.eventsURL.includes('?') ? '&' : '?';
        const response = await fetch(`${run.eventsURL}${separator}after=${encodeURIComponent(run.lastSequence || 0)}`, {
          headers: { ...requestHeaders(run.sessionId), Accept: 'text/event-stream' },
          signal: controller.signal,
        });
        if (!response.ok) throw await normalizeError(response);
        if (!response.body) throw { status: 0, message: 'No skill run event stream from server.' };
        let sawEvent = false;
        await parseSSEStream(response.body, (_eventName, data) => {
          if (!data) return true;
          let envelope;
          try {
            envelope = JSON.parse(data);
          } catch {
            return true;
          }
          sawEvent = applySkillRunEvent(run, envelope) || sawEvent;
          return !skillRunIsTerminal(run.status);
        }, { trackHeartbeat: false });
        if (sawEvent) retryAttempt = 0;
      } catch (error) {
        if (controller.signal.aborted) return;
        run.progress = error?.message ? `Reconnecting: ${error.message}` : 'Reconnecting…';
        renderSkillRun(run);
      } finally {
        if (run.controller === controller) run.controller = null;
      }
      if (!skillRunIsTerminal(run.status)) {
        await sleep(streamReconnectDelay(retryAttempt));
        retryAttempt += 1;
      }
    }
  } finally {
    run.following = false;
    run.controller = null;
  }
};

const upsertSkillRunSnapshot = (sessionId, snapshot, eventsURL = '') => {
  const id = String(snapshot?.id || snapshot?.run_id || '').trim();
  if (!id) return null;
  const store = skillRunStore();
  const run = store[id] || { id, sessionId, lastSequence: 0 };
  run.sessionId = sessionId;
  run.skill = String(snapshot.skill || run.skill || 'skill');
  run.agent = String(snapshot.agent || run.agent || '');
  run.status = String(snapshot.status || run.status || 'running');
  run.output = String(snapshot.output || run.output || '');
  run.childSessionId = String(snapshot.child_session_id || run.childSessionId || '');
  run.startedAt = snapshot.started_at || run.startedAt || new Date().toISOString();
  run.completedAt = snapshot.completed_at || run.completedAt || '';
  run.eventsURL = skillRunAPIURL(eventsURL || snapshot.events_url || `/v1/sessions/${encodeURIComponent(sessionId)}/skill-runs/${encodeURIComponent(id)}/events`);
  store[id] = run;
  for (const event of (Array.isArray(snapshot.events) ? snapshot.events : [])) applySkillRunEvent(run, event);
  renderSkillRun(run);
  if (!skillRunIsTerminal(run.status)) void followSkillRun(run);
  return run;
};

const reconcileSkillRuns = (sessionId, snapshots) => {
  if (!sessionId || !Array.isArray(snapshots)) return;
  snapshots.forEach((snapshot) => upsertSkillRunSnapshot(sessionId, snapshot));
};

const cancelSkillRun = async (sessionId, runId) => {
  const run = skillRunStore()[runId];
  if (run && !skillRunIsTerminal(run.status)) {
    run.status = 'cancelling';
    renderSkillRun(run);
  }
  const response = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(sessionId)}/skill-runs/${encodeURIComponent(runId)}`, {
    method: 'DELETE',
    headers: requestHeaders(sessionId),
  });
  if (!response.ok) {
    const error = await normalizeError(response);
    if (run) {
      run.error = error.message;
      renderSkillRun(run);
    }
    throw error;
  }
  return response.json().catch(() => ({}));
};

const appendSkillInvocationMessage = (session, invocation, reuseMessageId = '') => {
  let message = reuseMessageId
    ? window.TermLLMConversation.sessionMessages(session).find((entry) => entry.id === reuseMessageId && entry.role === 'user') || null
    : null;
  if (!message) {
    message = {
      id: generateId('msg'),
      role: 'user',
      content: invocation.invocation || `/${invocation.name}${invocation.arguments ? ` ${invocation.arguments}` : ''}`,
      created: Date.now(),
    };
    message.clientMessageId = message.id;
    app.trackPendingIntent?.(session, message);
    appendStreamMessageNode(session, message);
  }
  delete message.interruptState;
  return message;
};

const invokeSkill = async (session, invocation, options = {}) => {
  if (!session || !invocation) return false;
  let execution = invocation.execution;
  const message = appendSkillInvocationMessage(session, invocation, options.reuseMessageId || '');
  session.lastMessageAt = Date.now();
  if (!session.title || session.title === 'New chat') session.title = truncate(message.content, 60);
  elements.promptInput.value = '';
  app.hideSlashCommands?.();
  app.autoGrowPrompt();
  persistAndRefreshShell();
  scrollVisibleStreamToBottom(session, true);

  if (invocation.execution !== 'isolated') {
    setSessionOptimisticBusy(session, true);
    setStreaming(true);
  }

  try {
    const response = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(session.id)}/skills/invoke`, {
      method: 'POST',
      headers: requestHeaders(session.id),
      body: JSON.stringify({ name: invocation.name, arguments: invocation.arguments || '' }),
    });
    if (!response.ok) throw await normalizeError(response);
    const payload = await response.json();
    execution = payload.execution || execution;
    if (execution === 'isolated') {
      upsertSkillRunSnapshot(session.id, {
        id: payload.run_id,
        skill: invocation.name,
        status: 'running',
        child_session_id: payload.child_session_id,
        started_at: new Date().toISOString(),
      }, payload.events_url);
      return true;
    }

    const responseId = String(payload.response_id || '').trim();
    const runEpoch = Math.max(0, Number(payload.run_epoch) || 0);
    if (!responseId || !runEpoch) throw new Error('Skill invocation did not return a response ID and run epoch.');
    await app.noteTranscriptRunCreated(session, responseId, payload.started_rev, runEpoch, {
      clientMessageId: payload.client_message_id,
      anchorRowId: payload.anchor_row_id,
    });
    setActiveResponseTracking(session, responseId, 0);
    saveSessions();
    await resumeActiveResponse(session, { responseId });
    return true;
  } catch (error) {
    addErrorMessage(error?.message || 'Failed to invoke skill.', session);
    if (error?.status === 401) app.handleAuthFailure();
    if (!String(elements.promptInput.value || '').trim()) {
      elements.promptInput.value = message.content;
      app.autoGrowPrompt();
    }
    persistAndRefreshShell();
    return false;
  } finally {
    if (execution !== 'isolated' && !session.activeResponseId) {
      setSessionOptimisticBusy(session, false);
      setStreaming(Boolean(state.currentStreamResponseId));
      drainInterruptQueueIfIdle(session);
    }
  }
};

const queueMainSkillInvocation = (session, invocation) => {
  const message = appendSkillInvocationMessage(session, invocation);
  message.interruptState = 'queue';
  updateVisibleUserNode(session, message);
  if (!Array.isArray(state.queuedSkillInvocations)) state.queuedSkillInvocations = [];
  state.queuedSkillInvocations.push({ sessionId: session.id, invocation, messageId: message.id });
  elements.promptInput.value = '';
  app.hideSlashCommands?.();
  app.autoGrowPrompt();
  persistAndRefreshShell();
};

Object.assign(app, {
  skillRunStore,
  skillRunAPIURL,
  skillRunIsTerminal,
  skillRunMessageFor,
  renderSkillRun,
  progressTextForSkillRun,
  applySkillRunEvent,
  followSkillRun,
  upsertSkillRunSnapshot,
  reconcileSkillRuns,
  cancelSkillRun,
  appendSkillInvocationMessage,
  invokeSkill,
  queueMainSkillInvocation
});
})();
