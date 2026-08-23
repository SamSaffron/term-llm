'use strict';

(function initResponseEffects() {

const app = window.TermLLMApp;
const ConversationController = window.TermLLMConversation?.ConversationController;
const {
  state, saveSessions, setProviderRetryStatus, updateSessionUsageDisplay, renderSidebar, renderMessages,
  maybeNotifyResponseComplete, setSessionOptimisticBusy, setSessionServerActiveRun
} = app;
const { rebaseStreamAssetURL, forceSidebarStatusRefreshSoon, normalizeEffortForCompare, clearSessionPendingEffort, clearTerminalPendingEffort, classifyRecoverableContinuationFailure, setActiveResponseTracking, scheduleStreamPersistence, flushStreamPersistence, clearActiveResponseTracking, isSessionVisible, updateVisibleToolGroupNode, enqueueVisibleAssistantStreamUpdate, finalizeVisibleAssistantStreamRender, scrollVisibleStreamToBottom, scheduleVisibleStreamScroll, responseStreamOwnerId, clearProviderRetryForEvent, applyResponseRecoverySnapshot, addErrorMessage } = app;

const applyRecoveredInteractiveFact = (session, event, payload = {}) => {
  if (event === 'response.ask_user.prompt') {
    const callId = String(payload.call_id || '').trim();
    const questions = Array.isArray(payload.questions) ? payload.questions : [];
    if (!callId || questions.length === 0) return true;
    const samePrompt = state.askUser && state.askUser.sessionId === session.id && state.askUser.callId === callId;
    if (!samePrompt) app.openAskUserModal(session.id, callId, questions);
    return true;
  }
  if (event === 'response.approval.prompt') {
    const approvalId = String(payload.approval_id || '').trim();
    const options = Array.isArray(payload.options) ? payload.options : [];
    if (approvalId && options.length > 0) app.openApprovalModal(session.id, approvalId, payload.path, payload.is_shell,
      payload.is_workspace, payload.title, options, payload.resume_auto_available);
    return true;
  }
  return false;
};

const RESPONSE_PROJECTION_EVENTS = new Set([
  'response.output_text.delta',
  'response.output_text.new_segment',
  'response.output_item.added',
  'response.function_call_arguments.delta',
  'response.output_item.done',
  'response.tool_exec.start',
  'response.tool_exec.end',
]);

const applyOwnedResponseProjectionEvent = (session, event, payload) => {
  if (!RESPONSE_PROJECTION_EVENTS.has(event)) return null;
  if (!session.transcript && typeof ConversationController === 'function') {
    session.transcript = new ConversationController(session.id || 'stream');
  }
  const transcript = session.transcript;
  const ownedPayload = event === 'response.tool_exec.end' && Array.isArray(payload?.images)
    ? { ...payload, images: payload.images.map(rebaseStreamAssetURL).filter(Boolean) }
    : payload;
  const result = window.TermLLMConversation.dispatchRunEvent(transcript, event, ownedPayload);
  if (!result) return null;

  clearProviderRetryForEvent(session, payload);
  if (result.structural) {
    app.refreshSessionMessagesFromTranscript?.(session);
    if (isSessionVisible(session)) app.renderMessages?.();
  }
  const active = transcript.conversation?.active;
  if (event === 'response.output_text.delta') {
    const ordinal = Math.max(0, Number(payload?.assistant_segment_ordinal ?? payload?.output_index) || 0);
    const assistant = active?.assistantByOrdinal?.get(ordinal);
    if (assistant) enqueueVisibleAssistantStreamUpdate(session, assistant);
  } else {
    const callId = String(payload?.call_id || payload?.item?.call_id || payload?.item?.id || '').trim();
    const tool = active?.toolByCallID?.get(callId);
    const group = tool ? active.projection.find((entry) => entry.role === 'tool-group' && entry.tools?.includes(tool)) : null;
    if (group) updateVisibleToolGroupNode(session, group);
    if (event === 'response.tool_exec.end' && payload?.success !== false && String(payload?.tool_name || tool?.name || '') === 'update_plan'
        && session.id === state.activeSessionId && !state.draftSessionActive) {
      void app.refreshCurrentPlanFromServer?.(session);
    }
  }
  scheduleStreamPersistence();
  scheduleVisibleStreamScroll(session);
  return { terminal: false };
};

const terminalResponseId = (session, payload) => String(
  payload?.response_id || payload?.response?.id || session.activeResponseId || state.currentStreamResponseId || ''
).trim();

const finalizeNonCompletedResponse = (session, streamState, responseId, payload) => {
  const durableResponseId = String(payload?.response?.id || '').trim();
  if (durableResponseId) session.lastResponseId = durableResponseId;
  streamState.closeToolGroup();
  app.requeuePendingInterjections(session);
  state.expectCanceledRun = false;
  clearTerminalPendingEffort(session);
  clearActiveResponseTracking(session, responseId);
  setSessionOptimisticBusy(session, false);
  setSessionServerActiveRun(session, false);
  const lastAssistant = window.TermLLMConversation.sessionMessages(session).findLast((message) => message.role === 'assistant');
  if (lastAssistant) finalizeVisibleAssistantStreamRender(session, lastAssistant);
  flushStreamPersistence();
  saveSessions();
  renderSidebar();
  forceSidebarStatusRefreshSoon();
  app.refreshFileChangesAfterRun?.(session);
  scrollVisibleStreamToBottom(session, true);
  void app.trackTranscriptTerminalHandoff(session, responseId, payload);
};

const applyResponseStreamEvent = (session, streamState, event, payload) => {
  const projectionResult = applyOwnedResponseProjectionEvent(session, event, payload);
  if (projectionResult) return projectionResult;
  let lifecycleResult = null;
  if (String(event || '').startsWith('response.') && event !== 'response.file_change') {
    if (!session.transcript && typeof ConversationController === 'function') session.transcript = new ConversationController(session.id || 'stream');
    const responseId = responseStreamOwnerId(session, payload);
    try {
      lifecycleResult = window.TermLLMConversation.dispatchRunEvent(session.transcript, event, payload);
      if (!lifecycleResult) return { terminal: false, stale: true };
    } catch (error) {
      if (error?.code === 'stale_run_epoch' || error?.code === 'inactive_response_event') return { terminal: false, stale: true };
      throw error;
    }
  }
  if (event === 'response.file_change') {
    app.handleFileChangeEvent?.(session, payload);
    return { terminal: false };
  }

  if (event === 'response.stream_error') {
    const errorType = String(payload?.error?.type || '').trim();
    if (errorType === 'stream_buffer_overflow') {
      applyResponseRecoverySnapshot(session, {
        id: payload?.response_id || session.activeResponseId || state.currentStreamResponseId || '',
        run_epoch: payload?.run_epoch || session.transcript?.activeRun?.epoch || 0,
        status: 'in_progress',
        last_sequence_number: payload.sequence_number,
        recovery: payload.recovery || null
      });
      streamState.currentPhaseMessage = null;
      return { terminal: false, recoverableStreamError: true };
    }
    return { terminal: false };
  }

  if (event === 'response.created') {
    const responseId = String(payload?.response_id || payload?.response?.id || '').trim();
    const runEpoch = Math.max(0, Number(payload?.run_epoch ?? payload?.response?.run_epoch) || 0);
    setSessionOptimisticBusy(session, true);
    if (responseId) {
      setActiveResponseTracking(session, responseId, payload?.sequence_number ?? null);
      void app.noteTranscriptRunCreated?.(session, responseId, payload?.started_rev ?? payload?.response?.started_rev ?? 0, runEpoch, {
        clientMessageId: payload?.client_message_id || payload?.response?.client_message_id,
        anchorRowId: payload?.anchor_row_id ?? payload?.response?.anchor_row_id,
      });
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
      app.persistRuntimeSelection();
      app.syncSettingsSelectValues();
      updateSessionUsageDisplay(session);
    }
    return { terminal: false };
  }

  if (event === 'response.model_swap.progress') {
    const stage = String(payload?.stage || '').trim();
    if (stage === 'failed') {
      if (payload?.previous_provider) session.provider = String(payload.previous_provider);
      if (payload?.previous_model) session.activeModel = String(payload.previous_model);
      if (Object.prototype.hasOwnProperty.call(payload || {}, 'previous_effort')) session.activeEffort = payload.previous_effort || '';
      updateSessionUsageDisplay(session);
    } else if (stage === 'complete') {
      if (payload?.target_provider) session.provider = String(payload.target_provider);
      if (payload?.target_model) session.activeModel = String(payload.target_model);
      if (Object.prototype.hasOwnProperty.call(payload || {}, 'target_effort')) session.activeEffort = payload.target_effort || '';
      updateSessionUsageDisplay(session);
    }
    if (lifecycleResult?.changed) {
      app.refreshSessionMessagesFromTranscript?.(session);
      if (isSessionVisible(session)) app.renderMessages?.();
    }
    return { terminal: false };
  }

  if (event === 'response.phase' || event === 'response.compaction') {
    if (lifecycleResult?.changed) {
      app.refreshSessionMessagesFromTranscript?.(session);
      if (isSessionVisible(session)) app.renderMessages?.();
    }
    return { terminal: false };
  }

  if (event === 'response.retry') {
    const message = String(payload?.message || '').trim() || 'Model stream interrupted; reconnecting…';
    const responseId = responseStreamOwnerId(session, payload);
    if (responseId) {
      setProviderRetryStatus(String(session?.id || '').trim(), responseId, message);
    }
    return { terminal: false };
  }

  if (applyRecoveredInteractiveFact(session, event, payload)) return { terminal: false };

  if (event === 'response.guardian.review') {
    const active = session.transcript?.conversation?.active;
    const callId = String(payload?.tool_call_id || payload?.call_id || '').trim();
    const tool = callId ? active?.toolByCallID?.get(callId) : null;
    const group = tool ? active.projection.find((entry) => entry.role === 'tool-group' && entry.tools?.includes(tool)) : null;
    if (group) updateVisibleToolGroupNode(session, group);
    else if (lifecycleResult?.changed) {
      app.refreshSessionMessagesFromTranscript?.(session);
      if (isSessionVisible(session)) app.renderMessages?.();
    }
    saveSessions();
    return { terminal: false };
  }

  if (event === 'response.interjection') {
    const clientMessageId = String(payload?.client_message_id || '').trim();
    if (!clientMessageId) return { terminal: false, protocolError: 'interjection missing client_message_id' };
    const intent = app.commitPendingInterjection?.(session, clientMessageId, {
      content: payload?.text,
      attachments: payload?.attachments,
      created: payload?.created,
    });
    if (intent) intent.interruptState = 'interject';
    app.refreshPendingInterjectionBanner?.();
    app.refreshSessionMessagesFromTranscript?.(session);
    if (isSessionVisible(session)) app.renderMessages?.();
    saveSessions();
    scrollVisibleStreamToBottom(session, true);
    return { terminal: false };
  }

  if (event === 'response.completed') {
    const responseId = terminalResponseId(session, payload);
    if (lifecycleResult?.duplicate) {
      return { terminal: true, repeatedTerminal: true };
    }
    const usage = payload?.response?.usage;
    streamState.closeToolGroup();
    app.requeuePendingInterjections(session);

    const durableResponseId = String(payload?.response?.id || responseId).trim();
    if (durableResponseId) {
      session.lastResponseId = durableResponseId;
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

    const lastAssistant = window.TermLLMConversation.sessionMessages(session).findLast((message) => message.role === 'assistant');
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
    if (session.id === state.activeSessionId && !state.draftSessionActive) {
      void app.refreshCurrentPlanFromServer?.(session);
    }
    scrollVisibleStreamToBottom(session);
    void app.trackTranscriptTerminalHandoff(session, responseId, payload);
    return { terminal: true };
  }

  if (event === 'response.cancelled') {
    const responseId = terminalResponseId(session, payload);
    if (lifecycleResult?.duplicate) {
      return { terminal: true, repeatedTerminal: true };
    }
    finalizeNonCompletedResponse(session, streamState, responseId, payload);
    return { terminal: true };
  }

  if (event === 'response.failed') {
    const responseId = terminalResponseId(session, payload);
    if (lifecycleResult?.duplicate) {
      return { terminal: true, repeatedTerminal: true };
    }
    const durableResponseId = String(payload?.response?.id || '').trim();
    if (durableResponseId) session.lastResponseId = durableResponseId;
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
    finalizeNonCompletedResponse(session, streamState, responseId, payload);
    return {
      terminal: true,
      error: recoverableContinuationFailure
        ? { message: errorMessage, recoverableContinuationFailure }
        : null
    };
  }

  return { terminal: false };
};

Object.assign(app, { applyResponseStreamEvent, applyRecoveredInteractiveFact });
})();
