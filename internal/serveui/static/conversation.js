'use strict';
(function conversationModule(root, factory) {
  const api = factory(
    typeof module === 'object' && module.exports ? require('./active-response.js') : root.TermLLMActiveResponse,
    typeof module === 'object' && module.exports ? require('./transcript-window.js').TranscriptWindow : root.TranscriptWindow,
    root
  );
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.TermLLMConversation = api;
})(typeof window !== 'undefined' ? window : globalThis, function conversationFactory(activeResponse, TranscriptWindow, root) {
  if (!activeResponse) throw new Error('active-response.js must load before conversation.js');
  if (!TranscriptWindow) throw new Error('transcript-window.js must load before conversation.js');
  const clone = (value) => {
    if (typeof structuredClone === 'function') return structuredClone(value);
    return JSON.parse(JSON.stringify(value));
  };
  const clientMessageID = (message) => String(
    message?.clientMessageId || message?.client_message_id || ''
  ).trim();
  const responseID = (message) => String(message?.responseId || message?.response_id || '').trim();
  const assistantSegmentKey = (messageOrResponseID, ordinal = null) => {
    const owner = typeof messageOrResponseID === 'object' ? responseID(messageOrResponseID) : String(messageOrResponseID || '').trim();
    const value = ordinal == null ? (messageOrResponseID?.assistantSegmentOrdinal ?? messageOrResponseID?.assistant_segment_ordinal) : ordinal;
    const segment = Number(value);
    return owner && Number.isFinite(segment) && segment >= 0 ? `${owner}:assistant:${Math.trunc(segment)}` : '';
  };
  const transcriptToolIdentityKey = (messageOrResponseID, callID = null) => {
    const owner = typeof messageOrResponseID === 'object' ? responseID(messageOrResponseID) : String(messageOrResponseID || '').trim();
    const value = callID == null ? (messageOrResponseID?.callId || messageOrResponseID?.call_id || messageOrResponseID?.toolCallId || messageOrResponseID?.tool_call_id) : callID;
    const tool = String(value || '').trim();
    return owner && tool ? `${owner}:tool:${tool}` : '';
  };
  const transcriptIsClientOwnedIntent = (entry) => Boolean(entry && typeof entry === 'object' && entry.durable !== true && entry.role === 'user');
  const transcriptDiagnostic = (kind, fields = {}) => {
    if (!(root?.__TERM_LLM_DIAGNOSTICS__ || root?.__WEBRTC_DIAGNOSTICS__) || typeof console === 'undefined') return;
    console.warn('[transcript]', { kind: String(kind || ''), responseId: String(fields.responseId || ''), transcriptRev: Number(fields.transcriptRev) || 0 });
  };
  const durableMessages = (durable) => {
    if (!durable) return [];
    if (Array.isArray(durable.publishedMessages)) return durable.publishedMessages;
    if (typeof durable.renderedMessages === 'function') return durable.renderedMessages();
    if (Array.isArray(durable.messages)) return durable.messages;
    return [];
  };
  const createConversation = ({ sessionId = '', durable = null } = {}) => ({
    sessionId: String(sessionId || ''),
    durable,
    intents: new Map(),
    acknowledgedIntentIDs: new Set(),
    acknowledgedAskUserCallIDs: new Set(),
    active: null,
    publishedRevision: 0,
    protocolError: '',
  });
  const addIntent = (conversation, intent) => {
    const id = clientMessageID(intent);
    if (!id) throw new Error('user intent requires client_message_id');
    const callID = String(intent?.askUserCallId || intent?.ask_user_call_id || '').trim();
    if (callID && conversation.acknowledgedAskUserCallIDs.has(callID)) return null;
    conversation.intents.set(id, { ...intent, id: intent.id || id, clientMessageId: id, role: 'user' });
    return conversation.intents.get(id);
  };
  const acknowledgeDurableIntents = (conversation) => {
    const durableClientIDs = new Set();
    const durableAskUserCallIDs = new Set();
    for (const message of durableMessages(conversation.durable)) {
      const id = clientMessageID(message);
      if (id) durableClientIDs.add(id);
      const callID = String(message?.askUserCallId || message?.ask_user_call_id || '').trim();
      if (callID) {
        durableAskUserCallIDs.add(callID);
        conversation.acknowledgedAskUserCallIDs.add(callID);
      }
    }
    for (const [id, intent] of conversation.intents) {
      const callID = String(intent?.askUserCallId || intent?.ask_user_call_id || '').trim();
      if (durableClientIDs.has(id) || (callID && (durableAskUserCallIDs.has(callID) || conversation.acknowledgedAskUserCallIDs.has(callID)))) {
        conversation.intents.delete(id);
        conversation.acknowledgedIntentIDs.add(id);
      }
    }
  };
  const startActiveRun = (conversation, descriptor) => {
    const responseId = String(descriptor?.responseId || '').trim();
    const incomingEpoch = Math.max(0, Number(descriptor?.runEpoch) || 0);
    if (!responseId || !incomingEpoch) throw new Error('active response requires response_id and run_epoch');
    if (conversation.active) {
      if (conversation.active.responseID !== responseId) throw new Error('cannot replace an active response before durable handoff');
      if (conversation.active.runEpoch !== incomingEpoch) throw new Error('cannot change the active response run_epoch');
      if (descriptor?.anchor != null) conversation.active.anchor = clone(descriptor.anchor);
      return true;
    }
    conversation.active = activeResponse.createActiveRun({ ...descriptor, responseId, runEpoch: incomingEpoch });
    conversation.protocolError = '';
    return true;
  };
  const applyRunEvent = (conversation, event, payload) => {
    const responseId = String(payload?.response_id || payload?.response?.id || '').trim();
    if (!conversation.active) startActiveRun(conversation, { responseId, runEpoch: payload?.run_epoch });
    if (!conversation.active || conversation.active.responseID !== responseId) {
      const error = new Error(`event does not own active response: ${responseId}`);
      error.code = 'inactive_response_event';
      throw error;
    }
    const result = activeResponse.reduceResponseEvent(conversation.active, event, payload);
    if (conversation.active.terminal?.durableHandoff === false) {
      conversation.protocolError = conversation.active.terminal.error || 'durable response handoff was rejected';
    }
    return result;
  };
  const replaceActiveFromSnapshot = (conversation, snapshot, options = {}) => {
    const candidate = activeResponse.activeRunFromSnapshot(snapshot, options);
    if (conversation.active && conversation.active.responseID !== candidate.responseID) {
      throw new Error('cannot replace an active response with another snapshot owner');
    }
    if (conversation.active?.runEpoch && candidate.runEpoch !== conversation.active.runEpoch) return false;
    conversation.active = candidate;
    conversation.protocolError = candidate.terminal?.durableHandoff === false ? candidate.terminal.error : '';
    return true;
  };
  const durableRegionReady = (durable, ordinals) => {
    if (!durable || !Array.isArray(ordinals) || ordinals.length === 0) return false;
    return ordinals.every((ordinal) => {
      const segmentIndex = typeof durable.segmentForOrdinal === 'function' ? durable.segmentForOrdinal(ordinal) : -1;
      const state = segmentIndex >= 0 ? durable.segments?.[segmentIndex]?.state : '';
      return state === 'materialized' || state === 'empty';
    });
  };
  const compactedReplacementReady = (durable, terminal) => {
    const compactionChanged = Number(durable?.compactionSeq ?? -1) !== Number(terminal?.compactionSeq ?? -1)
      || Number(durable?.compactionCount ?? 0) !== Number(terminal?.compactionCount ?? 0);
    if (!compactionChanged || !Array.isArray(durable?.ids) || durable.ids.length === 0) return false;
    const tailOrdinal = durable.ids.length - 1;
    return durableRegionReady(durable, [tailOrdinal]);
  };
  const responseRowsReady = (conversation) => {
    const active = conversation.active;
    if (!active?.terminal?.durableHandoff) return false;
    const durable = conversation.durable;
    const rev = Math.max(0, Number(durable?.rev) || 0);
    if (rev < active.terminal.finalRev) return false;
    if (active.terminal.durableOutputCount === 0) return true;
    if (Array.isArray(durable?.responseIDs)) {
      const ordinals = [];
      for (let ordinal = 0; ordinal < durable.responseIDs.length; ordinal++) {
        if (String(durable.responseIDs[ordinal] || '') === active.responseID) ordinals.push(ordinal);
      }
      if (ordinals.length > 0) return durableRegionReady(durable, ordinals);
      return compactedReplacementReady(durable, active.terminal);
    }
    return durableMessages(durable).some((message) => responseID(message) === active.responseID);
  };
  const commitDurableHandoff = (conversation) => {
    if (!conversation.active?.terminal) return false;
    if (conversation.active.terminal.durableHandoff !== true) return false;
    if (!responseRowsReady(conversation)) return false;
    conversation.active = null;
    conversation.protocolError = '';
    acknowledgeDurableIntents(conversation);
    return true;
  };
  const visibleMessages = (conversation) => {
    const active = conversation.active;
    const activeResponseID = active?.responseID || '';
    const intentRefs = new Set(
      active?.projection
        ?.filter((entry) => entry.role === 'intent-ref')
        .map((entry) => entry.clientMessageId) || []
    );
    const activeAskUserCallIDs = new Set();
    for (const entry of active?.projection || []) {
      if (entry.role !== 'tool-group') continue;
      for (const tool of entry.tools || []) {
        const callID = String(tool.callId || tool.id || '').trim();
        if (callID && tool.name === 'ask_user') activeAskUserCallIDs.add(callID);
      }
    }
    const durable = durableMessages(conversation.durable);
    const durableIntentByID = new Map();
    const durableAskUserByCallID = new Map();
    const base = [];
    const emittedIntents = new Set();
    const emittedAskUserCallIDs = new Set();
    for (const message of durable) {
      const owner = responseID(message);
      const role = String(message?.role || '');
      const askUserCallID = String(message?.askUserCallId || message?.ask_user_call_id || '').trim();
      if (askUserCallID && activeAskUserCallIDs.has(askUserCallID)) {
        durableAskUserByCallID.set(askUserCallID, message);
        continue;
      }
      if (activeResponseID && owner === activeResponseID && ['assistant', 'tool', 'tool-group', 'event', 'error', 'model-swap'].includes(role)) continue;
      const intentID = role === 'user' ? clientMessageID(message) : '';
      if (intentID) durableIntentByID.set(intentID, message);
      if (intentID && intentRefs.has(intentID)) continue;
      if (intentID) emittedIntents.add(intentID);
      base.push(message);
    }
    const pending = [...conversation.intents.entries()]
      .filter(([id, intent]) => {
        const callID = String(intent?.askUserCallId || intent?.ask_user_call_id || '').trim();
        return !intentRefs.has(id) && !emittedIntents.has(id) && !(callID && activeAskUserCallIDs.has(callID));
      })
      .sort(([, left], [, right]) => Number(left.created || 0) - Number(right.created || 0));
    for (const [id, intent] of pending) {
      base.push(intent);
      emittedIntents.add(id);
    }
    if (!active) return base;
    const activeProjection = [];
    for (const entry of active.projection) {
      if (entry.role === 'intent-ref') {
        const id = entry.clientMessageId;
        const intent = durableIntentByID.get(id) || conversation.intents.get(id);
        if (intent && !emittedIntents.has(id)) {
          activeProjection.push(intent);
          emittedIntents.add(id);
        }
        continue;
      }
      if (entry.terminalPolicy === 'transient' && active.terminal) continue;
      activeProjection.push(entry);
      if (entry.role === 'tool-group') {
        for (const tool of entry.tools || []) {
          const callID = String(tool.callId || tool.id || '').trim();
          if (!callID || tool.name !== 'ask_user') continue;
          const durableAnswer = durableAskUserByCallID.get(callID);
          if (durableAnswer && !emittedAskUserCallIDs.has(callID)) {
            activeProjection.push(durableAnswer);
            emittedAskUserCallIDs.add(callID);
            continue;
          }
          for (const [id, intent] of conversation.intents) {
            const intentCallID = String(intent?.askUserCallId || intent?.ask_user_call_id || '').trim();
            if (intentCallID === callID && !emittedIntents.has(id) && !emittedAskUserCallIDs.has(callID)) {
              activeProjection.push(intent);
              emittedIntents.add(id);
              emittedAskUserCallIDs.add(callID);
            }
          }
        }
      }
    }
    const anchorClientID = String(active.anchor?.clientMessageId || '').trim();
    const anchorRowID = active.anchor?.durableRowId;
    let anchorIndex = -1;
    for (let index = base.length - 1; index >= 0; index--) {
      const durableIdentity = base[index]?.durableRowId ?? base[index]?.id ?? '';
      const rangeStart = Number(base[index]?.durableRowStartId);
      const rangeEnd = Number(base[index]?.durableRowEndId);
      const rowAnchor = Number(anchorRowID);
      const anchorInRange = anchorRowID != null && Number.isFinite(rowAnchor) && Number.isFinite(rangeStart) && Number.isFinite(rangeEnd)
        && rowAnchor >= Math.min(rangeStart, rangeEnd) && rowAnchor <= Math.max(rangeStart, rangeEnd);
      if ((anchorClientID && clientMessageID(base[index]) === anchorClientID)
          || (anchorRowID != null && String(durableIdentity) === String(anchorRowID)) || anchorInRange) {
        anchorIndex = index;
        break;
      }
    }
    if (anchorIndex < 0 && (anchorClientID || anchorRowID != null)) anchorIndex = base.length - 1;
    base.splice(anchorIndex + 1, 0, ...activeProjection);
    return base;
  };
  const sessionMessages = (session) => (
    session?.transcript?.conversation ? visibleMessages(session.transcript.conversation) : []
  );
  const applyDurable = (conversation, durable) => {
    conversation.durable = durable;
    acknowledgeDurableIntents(conversation);
    commitDurableHandoff(conversation);
    conversation.publishedRevision++;
    return visibleMessages(conversation);
  };
  class SessionCommandQueue {
    constructor() {
      this.tail = Promise.resolve();
    }
    enqueue(command) {
      const execute = async () => command();
      const queued = this.tail.then(execute, execute);
      this.tail = queued.catch(() => {});
      return queued;
    }
  }
  class ConversationController extends TranscriptWindow {
    constructor(sessionId, budgets = {}) {
      super(sessionId, budgets);
      this.conversation = createConversation({ sessionId, durable: this });
      this.latestRunEpoch = 0;
      this.startedRev = 0;
      this.commands = new SessionCommandQueue();
    }
    get activeRun() {
      const active = this.conversation.active;
      return active ? { id: active.responseID, epoch: active.runEpoch, startedRev: this.startedRev, terminal: active.terminal } : null;
    }
    setActiveRun(responseId, startedRev = 0, runEpoch = 0, options = {}) {
      const id = String(responseId || '').trim();
      const epoch = Math.max(0, Number(runEpoch) || 0);
      this.latestRunEpoch = Math.max(this.latestRunEpoch, epoch);
      if (!id) {
        if (this.conversation.active && options.responseId && this.conversation.active.responseID !== String(options.responseId)) return false;
        return true;
      }
      this.startedRev = Math.max(0, Number(startedRev) || 0);
      const explicitClientMessageId = String(options.clientMessageId || options.client_message_id || '').trim();
      const explicitRowID = options.anchorRowId ?? options.anchor_row_id;
      const durableTailID = this.ids.at(-1);
      const anchor = options.anchor || (explicitClientMessageId
        ? { clientMessageId: explicitClientMessageId }
        : (explicitRowID != null && String(explicitRowID) !== ''
          ? { durableRowId: explicitRowID }
          : (durableTailID != null ? { durableRowId: durableTailID } : null)));
      return startActiveRun(this.conversation, {
        responseId: id,
        runEpoch: epoch,
        anchor,
      });
    }
    transitionAuthoritativeRun(responseId, startedRev = 0, runEpoch = 0, options = {}) {
      const id = String(responseId || '').trim();
      const epoch = Math.max(0, Number(runEpoch) || 0);
      const revision = Math.max(0, Number(startedRev) || 0);
      if (!id || !epoch) return false;
      const active = this.conversation.active;
      if (!active || active.responseID === id) {
        return this.setActiveRun(id, revision, epoch, options);
      }
      if (epoch <= active.runEpoch) return false;
      const explicitClientMessageId = String(options.clientMessageId || options.client_message_id || '').trim();
      const explicitRowID = options.anchorRowId ?? options.anchor_row_id;
      const durableTailID = this.ids.at(-1);
      const anchor = options.anchor || (explicitClientMessageId
        ? { clientMessageId: explicitClientMessageId }
        : (explicitRowID != null && String(explicitRowID) !== ''
          ? { durableRowId: explicitRowID }
          : (durableTailID != null ? { durableRowId: durableTailID } : null)));
      this.conversation.active = activeResponse.createActiveRun({
        responseId: id,
        runEpoch: epoch,
        anchor,
      });
      this.conversation.protocolError = '';
      this.startedRev = revision;
      this.latestRunEpoch = Math.max(this.latestRunEpoch, epoch);
      return true;
    }
    replaceActiveSnapshot(snapshot, options = {}) {
      const owner = String(snapshot?.id || snapshot?.response_id || options.responseId || '').trim();
      if (!owner) return false;
      const activeOwner = String(this.conversation.active?.responseID || '').trim();
      if (activeOwner && activeOwner !== owner) return false;
      const snapshotClientMessageId = String(snapshot?.client_message_id || snapshot?.clientMessageId || '').trim();
      const snapshotRowID = snapshot?.anchor_row_id ?? snapshot?.anchorRowId;
      const anchor = this.conversation.active?.anchor || options.anchor
        || (snapshotClientMessageId ? { clientMessageId: snapshotClientMessageId } : null)
        || (snapshotRowID != null && String(snapshotRowID) !== '' ? { durableRowId: snapshotRowID } : null);
      const changed = replaceActiveFromSnapshot(this.conversation, snapshot, { ...options, responseId: owner, anchor });
      if (!changed) return false;
      this.latestRunEpoch = Math.max(this.latestRunEpoch, Number(this.conversation.active?.runEpoch) || 0);
      if (this.conversation.active?.terminal) {
        const terminal = this.conversation.active.terminal;
        if (!Object.prototype.hasOwnProperty.call(terminal, 'compactionSeq')) terminal.compactionSeq = this.compactionSeq;
        if (!Object.prototype.hasOwnProperty.call(terminal, 'compactionCount')) terminal.compactionCount = this.compactionCount;
      }
      return true;
    }
    applyResponseEvent(event, payload = {}) {
      const active = this.conversation.active;
      if (active) {
        for (const entry of active.projection) {
          if (entry.role === 'assistant') {
            const ordinal = Math.max(0, Math.trunc(Number(entry.assistantSegmentOrdinal ?? entry.assistant_segment_ordinal) || 0));
            if (!active.assistantByOrdinal.has(ordinal)) active.assistantByOrdinal.set(ordinal, entry);
          } else if (entry.role === 'tool-group') {
            for (const tool of entry.tools || []) {
              const id = String(tool.callId || tool.id || '').trim();
              if (id && !active.toolByCallID.has(id)) active.toolByCallID.set(id, tool);
            }
          }
        }
      }
      const result = applyRunEvent(this.conversation, event, payload);
      if (this.conversation.active?.terminal) {
        const terminal = this.conversation.active.terminal;
        if (!Object.prototype.hasOwnProperty.call(terminal, 'compactionSeq')) terminal.compactionSeq = this.compactionSeq;
        if (!Object.prototype.hasOwnProperty.call(terminal, 'compactionCount')) terminal.compactionCount = this.compactionCount;
      }
      this.latestRunEpoch = Math.max(this.latestRunEpoch, Number(this.conversation.active?.runEpoch) || 0);
      return result;
    }
    applyDetachedReplay(events = []) {
      if (!this.conversation.active) throw new Error('active response is required for replay');
      this.conversation.active = activeResponse.reduceDetachedReplay(this.conversation.active, events);
      this.latestRunEpoch = Math.max(this.latestRunEpoch, Number(this.conversation.active.runEpoch) || 0);
      return this.conversation.active;
    }
    addPendingIntent(entry, revAtSend = this.rev) {
      if (!entry || typeof entry !== 'object' || entry.durable === true || entry.role !== 'user') return null;
      const intent = {
        ...entry,
        optimistic: true,
        clientMessageId: clientMessageID(entry),
        revAtSend: Number.isFinite(Number(entry.revAtSend)) ? Number(entry.revAtSend) : revAtSend,
        durableSeqAtSend: Number.isFinite(Number(entry.durableSeqAtSend)) ? Number(entry.durableSeqAtSend) : (this.seqs.at(-1) ?? -1),
      };
      return addIntent(this.conversation, intent);
    }
    removePendingIntent(entryOrKey) {
      const key = String(entryOrKey && typeof entryOrKey === 'object' ? (entryOrKey.clientMessageId || entryOrKey.clientKey || entryOrKey.id || '') : (entryOrKey || ''));
      if (!key) return [];
      const removed = [];
      for (const [id, entry] of this.conversation.intents) {
        if (id === key || String(entry.clientKey || entry.id || '') === key) {
          removed.push(entry);
          this.conversation.intents.delete(id);
        }
      }
      return removed;
    }
    rekey(sessionId) {
      super.rekey(sessionId);
      this.conversation.sessionId = this.sessionId;
      return this;
    }
  }
  const runDescriptor = (payload = {}) => ({
    responseId: String(payload.response_id || payload.active_response_id || payload.id || payload.response?.id || '').trim(),
    startedRev: payload.started_rev ?? payload.response?.started_rev ?? 0,
    runEpoch: payload.run_epoch ?? payload.response?.run_epoch ?? 0,
    options: { clientMessageId: payload.client_message_id || payload.response?.client_message_id, anchorRowId: payload.anchor_row_id ?? payload.response?.anchor_row_id },
  });
  const attachActiveRun = (controller, payload, authoritative = false) => {
    const run = runDescriptor(payload);
    return authoritative
      ? controller?.transitionAuthoritativeRun(run.responseId, run.startedRev, run.runEpoch, run.options)
      : controller?.setActiveRun(run.responseId, run.startedRev, run.runEpoch, run.options);
  };
  const dispatchRunEvent = (controller, event, payload = {}) => {
    attachActiveRun(controller, payload);
    return controller?.applyResponseEvent(event, payload) || null;
  };
  const replaceRunSnapshot = (controller, payload) => controller?.replaceActiveSnapshot(payload);
  const enqueueDetachedReplay = (controller, events) => controller?.commands.enqueue(() => controller.applyDetachedReplay(events));
  const addPendingIntentToConversation = (controller, intent, rev) => controller?.addPendingIntent(intent, rev);
  const applyTranscriptIndex = (controller, index, etag = '', tail = false) => {
    controller.applyIndex(index, etag);
    if (index.active_response_id) attachActiveRun(controller, index, true);
    else controller.setActiveRun('', 0, index.run_epoch || 0);
    if (tail && controller.ids.length) controller.setViewport(controller.ids.length - 1, controller.ids.length - 1, { deferBudget: true });
    return controller;
  };
  const materializeTranscriptBodies = (controller, messages, requiredSegments = []) => {
    controller.materialize(messages, { countFetch: false, deferBudget: true });
    const complete = requiredSegments.every((index) => ['materialized', 'empty'].includes(controller.segments[index]?.state));
    controller.enforceBudget();
    controller._checkInvariants?.();
    return complete;
  };
  const destroyConversationController = (controller) => controller?.destroy();
  // EFFECTS_SECTION_START: only this injected browser adapter may access app effects.
  let conversationEffectsInitialized = false;
  const initEffects = () => {
    if (conversationEffectsInitialized) return;
    conversationEffectsInitialized = true;
    const app = window.TermLLMApp;
    const ConversationController = window.TermLLMConversation?.ConversationController;
    const {
      UI_PREFIX, STORAGE_KEYS, state, elements, generateId, truncate, asTimestamp, loadSessions, saveSessions, getActiveSession, createSession, ensureActiveSession,
      sessionIdFromURL, isSessionIdentityResolved, sessionSlug, findSessionBySlug, updateURL, updateDocumentTitle, scrollToBottom, setConnectionState, setStartupStatus, hideStartupSplash, clearProviderRetryStatus, persistAndRefreshShell, refreshRelativeTimes,
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
    const TRANSCRIPT_RECENT_SKELETONS = [];
    const PENDING_INTENT_LIMIT = 256;
    const TRANSCRIPT_EMPTY_BODY_FLAG = Number(window.TRANSCRIPT_FLAG_EMPTY_BODY || 2);
    const findSessionById = (sessionId) => state.sessions.find((item) => item?.id === sessionId) || null;
    // Version 2 restricts local persistence to client-owned intent. Version 1 (and
    // the unversioned legacy shape) also stored assistant/tool recovery shadows,
    // which could shadow durable rows after a reload. Migration keeps pending user
    // intent and drops every server-produced projection.
    const ensureSessionTranscript = (session) => {
      if (!session || typeof ConversationController !== 'function') return null;
      if (!(session.transcript instanceof ConversationController)) {
        session.transcript = new ConversationController(session.id);
        const saved = app.readPendingIntentRegistry()[session.id];
        if (Array.isArray(saved)) {
          // Storage is the one place a projection could re-enter the store after a
          // reload, so hydration admits client-owned intent only. Assistant and tool
          // output is never restored here; it comes back from the server.
          saved
            .slice(-PENDING_INTENT_LIMIT)
            .forEach((entry) => {
              session.transcript.addPendingIntent(entry, entry.revAtSend);
            });
        }
      }
      return session.transcript;
    };
    const trackPendingIntent = (session, message) => {
      if (!session || !message || message.durable || message.role !== 'user') return null;
      const transcript = ensureSessionTranscript(session);
      if (!transcript) return null;
      const tracked = transcript.addPendingIntent(message, transcript.rev);
      app.persistPendingIntents(session);
      return tracked;
    };
    const retirePendingIntent = (session, messageOrKey) => {
      const transcript = session?.transcript;
      if (!transcript?.removePendingIntent) return [];
      const removed = transcript.removePendingIntent(messageOrKey);
      for (const entry of removed) {
        app.removePendingIntentStorage(session.id, entry.clientMessageId || entry.client_message_id || entry.id);
      }
      if (removed.length > 0) app.persistPendingIntents(session);
      return removed;
    };
    const noteTranscriptRunCreated = (session, responseId, startedRev, runEpoch = 0, options = {}) => {
      const transcript = ensureSessionTranscript(session);
      if (!transcript) return Promise.resolve(false);
      const epoch = Math.max(0, Number(runEpoch) || 0);
      return transcript.commands.enqueue(() => transcript.setActiveRun(responseId, startedRev, epoch, options));
    };
    const noteTranscriptTerminal = (session, responseId, finalRev, runEpoch = 0, durableHandoff = true, handoffError = '') => {
      const transcript = ensureSessionTranscript(session);
      if (!transcript) return Promise.resolve(false);
      if (durableHandoff === false) {
        session.durableHandoffError = String(handoffError || 'The completed response could not be committed to the transcript.');
        app.persistPendingIntents(session);
        refreshSessionMessagesFromTranscript(session);
        if (session.id === state.activeSessionId && !state.draftSessionActive) renderMessages(false);
        return Promise.resolve(false);
      }
      delete session.durableHandoffError;
      app.persistPendingIntents(session);
      return syncTranscript(session, {
        reason: 'terminal',
        targetRev: Math.max(0, Number(finalRev) || 0),
        force: true
      });
    };
    const transcriptViewportAdapter = (session, forceScroll = false) => {
      if (!session || session.id !== state.activeSessionId || !elements.messages || !elements.chatScroll) return null;
      const scrollRect = () => elements.chatScroll.getBoundingClientRect?.() || { top: 0, bottom: Number(elements.chatScroll.clientHeight) || 0 };
      const durableNodeForID = (id) => {
        const exact = elements.messages.querySelector?.(`[data-durable-id="${String(id)}"]`);
        if (exact) return exact;
        const target = Number(id);
        if (!Number.isFinite(target)) return null;
        return Array.from(elements.messages.querySelectorAll?.('[data-durable-start-id]') || []).find((candidate) => {
          const start = Number(candidate.dataset?.durableStartId);
          const end = Number(candidate.dataset?.durableEndId);
          return Number.isFinite(start) && Number.isFinite(end) && target >= start && target <= end;
        }) || null;
      };
      return {
        capture: () => {
          // Bottom stickiness is the anchor. Capturing a durable row here would
          // restore an old ordinal after append-only growth and let final budget
          // enforcement evict the newly fetched tail before it can render.
          if (forceScroll || state.autoScroll) return null;
          const viewport = scrollRect();
          const nodes = Array.from(elements.messages.querySelectorAll?.('[data-durable-id]') || []);
          const node = nodes.find((candidate) => {
            const rect = candidate.getBoundingClientRect?.();
            return rect && rect.bottom > viewport.top && rect.top < viewport.bottom;
          });
          if (!node) return null;
          const rect = node.getBoundingClientRect();
          return { id: Number(node.dataset.durableId), top: rect.top - viewport.top };
        },
        render: () => {
          // ConversationController is the durable/body/optimistic source of truth. Publish
          // Publish the bounded projection only at the transaction render boundary
          // so rendering cannot observe half-applied index or body state.
          refreshSessionMessagesFromTranscript(session);
          renderMessages(forceScroll);
        },
        topForID: (id) => {
          const node = durableNodeForID(id);
          if (!node) return null;
          return node.getBoundingClientRect().top - scrollRect().top;
        },
        adjustScroll: (delta) => {
          elements.chatScroll.scrollTop = (Number(elements.chatScroll.scrollTop) || 0) + delta;
        }
      };
    };
    const refreshSessionMessagesFromTranscript = (session) => {
      const transcript = session?.transcript;
      if (!transcript) return false;
      const display = [];
      for (const run of transcript.renderRuns()) {
        if (run.type === 'gap') {
          display.push({
            id: `transcript_gap_${run.startOrdinal}_${run.endOrdinal}`,
            role: 'transcript-gap',
            transcriptGap: true,
            startOrdinal: run.startOrdinal,
            endOrdinal: run.endOrdinal,
            startSegmentIndex: run.startSegmentIndex,
            endSegmentIndex: run.endSegmentIndex,
            estimatedHeight: run.height
          });
          continue;
        }
        const raw = [];
        for (let ordinal = run.startOrdinal; ordinal <= run.endOrdinal; ordinal += 1) {
          const entry = transcript.bodies.get(transcript.ids[ordinal]);
          if (!entry) continue;
          raw.push((transcript.flags[ordinal] & TRANSCRIPT_EMPTY_BODY_FLAG) !== 0
            ? { ...entry, transcriptEmptyBody: true }
            : entry);
        }
        const converted = app.convertServerMessages(raw);
        const claimedAnchors = new Set();
        converted.forEach((message) => {
          const sourceIDs = Array.isArray(message.durableSourceRowIds)
            ? message.durableSourceRowIds.filter((id) => id != null)
            : [];
          const durableRowId = sourceIDs.find((id) => !claimedAnchors.has(String(id)));
          sourceIDs.forEach((id) => claimedAnchors.add(String(id)));
          display.push({
            ...message,
            durable: true,
            ...(durableRowId != null ? { durableRowId } : {}),
            ...(sourceIDs.length > 0 ? {
              durableRowStartId: sourceIDs[0],
              durableRowEndId: sourceIDs[sourceIDs.length - 1]
            } : {}),
            transcriptSegmentIndex: run.segmentIndex
          });
        });
      }
      app.annotateCompactionBoundary(display, {
        compactionSeq: transcript.compactionSeq,
        compactionCount: transcript.compactionCount
      });
      transcript.publishedMessages = display;
      if (transcript.conversation && window.TermLLMConversation) {
        window.TermLLMConversation.applyDurable(transcript.conversation, transcript);
      }
      delete session._serverOnly;
      return true;
    };
    const touchTranscriptSkeleton = (session) => {
      const id = String(session?.id || '');
      const existing = TRANSCRIPT_RECENT_SKELETONS.indexOf(id);
      if (existing >= 0) TRANSCRIPT_RECENT_SKELETONS.splice(existing, 1);
      TRANSCRIPT_RECENT_SKELETONS.push(id);
      const max = Number(window.TRANSCRIPT_BUDGETS?.maxRecentSkeletons || 2);
      while (TRANSCRIPT_RECENT_SKELETONS.length > max) {
        const retired = TRANSCRIPT_RECENT_SKELETONS.shift();
        if (!retired || retired === state.activeSessionId) continue;
        const stale = findSessionById(retired);
        stale?.transcript?.destroy?.();
        if (stale) {
          delete stale.messages;
          delete stale.transcript;
          stale._serverOnly = true;
        }
      }
    };
    const TRANSCRIPT_MATERIALIZE_BATCH_TURNS = Math.max(1, Number(window.TRANSCRIPT_MATERIALIZE_BATCH_TURNS) || 32);
    const boundedTranscriptSegmentIndexes = (transcript, request) => {
      if (!transcript) return [];
      if (!Array.isArray(request)) {
        return transcript.selectGapBatch?.(
          request?.startSegmentIndex,
          request?.endSegmentIndex,
          { targetOrdinal: request?.targetOrdinal, direction: request?.direction }
        ) || [];
      }
      const selected = [];
      const seen = new Set();
      for (const value of request) {
        const index = Math.trunc(Number(value));
        if (!Number.isFinite(index) || seen.has(index)) continue;
        seen.add(index);
        const segment = transcript.segments[index];
        if (!segment || segment.state !== 'evicted') continue;
        if (selected.length >= TRANSCRIPT_MATERIALIZE_BATCH_TURNS) break;
        selected.push(index);
      }
      return selected.sort((a, b) => a - b);
    };
    const transcriptSyncSegmentIndexes = (transcript) => {
      if (!transcript) return [];
      const wanted = new Set(transcript.pinnedSegments);
      if (transcript.segments.length > 0) wanted.add(transcript.segments.length - 1);
      return [...wanted];
    };
    const fetchTranscriptSegments = async (session, segmentIndexes, options = {}) => {
      const transcript = ensureSessionTranscript(session);
      if (!transcript) return false;
      const requested = [];
      const boundedIndexes = boundedTranscriptSegmentIndexes(transcript, segmentIndexes);
      for (const index of boundedIndexes) {
        // Transport one anchor per conversational turn. The server expands each
        // anchor to the complete user-bounded segment; durable row count is not a
        // request or materialization budget and partial turns are never rendered.
        const id = transcript.ids[transcript.segments[index]?.startOrdinal];
        if (id != null) requested.push(id);
      }
      if (requested.length === 0) return true;
      const resp = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(session.id)}/transcript/bodies?ids=${requested.join(',')}`, {
        headers: requestHeaders(session.id)
      });
      if (!resp.ok) return false;
      const data = await resp.json().catch(() => null);
      if (!data || !Array.isArray(data.messages) || Number(data.rev) !== transcript.rev) return false;
      transcript.materialize(data.messages, { deferBudget: options.deferBudget === true });
      // materialize() retires every body in an incomplete segment, so this check
      // guarantees callers never render a partial conversational turn.
      return boundedIndexes.every((index) => ['materialized', 'empty'].includes(transcript.segments[index]?.state));
    };
    const materializeTranscriptSegmentsOnce = async (session, request) => {
      if (!session || session.id !== state.activeSessionId) return false;
      const transcript = ensureSessionTranscript(session);
      if (!transcript) return false;
      const indexes = boundedTranscriptSegmentIndexes(transcript, request);
      if (indexes.length === 0) return true;
      const adapter = transcriptViewportAdapter(session);
      const anchor = adapter?.capture?.() || null;
      const previousViewport = { ...transcript.viewport };
      const first = transcript.segments[indexes[0]];
      const last = transcript.segments[indexes[indexes.length - 1]];
      transcript.setViewport(first.startOrdinal, last.endOrdinal, { deferBudget: true });
      const anchorSegment = anchor ? transcript.segmentForID(anchor.id) : -1;
      if (anchorSegment >= 0) transcript.pinnedSegments.add(anchorSegment);
      const loaded = await fetchTranscriptSegments(session, indexes, { deferBudget: true });
      if (!loaded) {
        transcript.setViewport(previousViewport.firstOrdinal, previousViewport.lastOrdinal);
        return syncTranscript(session, { reason: 'stale-bodies', force: true });
      }
      transcript.enforceBudget();
      if (adapter) {
        // Publish the bounded store projection through the adapter once.
        adapter.render?.(transcript);
        const top = anchor == null ? null : adapter.topForID?.(anchor.id);
        if (Number.isFinite(top) && Number.isFinite(anchor.top)) adapter.adjustScroll?.(top - anchor.top);
      } else {
        refreshSessionMessagesFromTranscript(session);
        renderMessages(false);
      }
      app.persistPendingIntents(session);
      // Drop the temporary anchor pin after the anchored render. The viewport stays
      // on the newly loaded batch so the next fill can evict the old region.
      transcript.refreshPinnedSegments();
      return true;
    };
    const materializeTranscriptSegments = (session, request) => {
      const transcript = ensureSessionTranscript(session);
      if (!transcript?.commands) return Promise.resolve(false);
      return transcript.commands.enqueue(() => materializeTranscriptSegmentsOnce(session, request));
    };
    const syncTranscriptOnce = async (session, options = {}) => {
      const transcript = ensureSessionTranscript(session);
      if (!transcript) return false;
      const headers = requestHeaders(session.id);
      if (transcript.etag && !options.force) headers['If-None-Match'] = transcript.etag;
      const resp = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(session.id)}/transcript`, { headers });
      transcript.noteIndexFetch(resp.status === 304, resp.headers?.get?.('ETag') || '');
      if (resp.status !== 304 && !resp.ok) return false;
      let data = null;
      if (resp.status !== 304) {
        data = await resp.json().catch(() => null);
        if (!data?.rows) return false;
      }
    
      const adapter = transcriptViewportAdapter(session, options.forceScroll === true);
      const followTail = Boolean(adapter && (options.forceScroll === true || state.autoScroll));
      const loaded = await transcript.withViewportAnchor(adapter, async () => {
        if (data) applyTranscriptIndex(transcript, data, resp.headers?.get?.('ETag') || '');
        if (transcript.ids.length > 0 && (followTail || transcript.viewport.firstOrdinal < 0)) {
          const last = transcript.ids.length - 1;
          transcript.setViewport(last, last, { deferBudget: true });
        }
        const bodiesLoaded = await fetchTranscriptSegments(
          session,
          transcriptSyncSegmentIndexes(transcript),
          { deferBudget: true }
        );
        if (!bodiesLoaded) {
          transcript.etag = '';
          return false;
        }
        return true;
      });
      if (!loaded) return false;
      // Active transactions publish through the viewport adapter's render boundary.
      // Inactive transactions update the in-memory projection without touching DOM.
      if (!adapter) refreshSessionMessagesFromTranscript(session);
      app.persistPendingIntents(session);
      touchTranscriptSkeleton(session);
      return true;
    };
    
    const mergeTranscriptSyncRequest = (session, options = {}) => {
      const pending = session._transcriptSyncPending || {
        force: false,
        forceWithoutTarget: false,
        forceScroll: false,
        targetRev: 0,
        reason: ''
      };
      const targetRev = Math.max(0, Number(options.targetRev) || 0);
      const force = options.force === true;
      pending.force = pending.force || force;
      pending.forceWithoutTarget = pending.forceWithoutTarget || (force && targetRev === 0);
      pending.forceScroll = pending.forceScroll || options.forceScroll === true;
      pending.targetRev = Math.max(Number(pending.targetRev) || 0, targetRev);
      if (options.reason) pending.reason = String(options.reason);
      session._transcriptSyncPending = pending;
      return pending;
    };
    
    const syncTranscript = (session, options = {}) => {
      const transcript = session && isSessionIdentityResolved(session) ? ensureSessionTranscript(session) : null;
      if (!transcript?.commands) return Promise.resolve(false);
      mergeTranscriptSyncRequest(session, options);
      return transcript.commands.enqueue(async () => {
        for (;;) {
          const request = session._transcriptSyncPending;
          if (!request) return true;
          delete session._transcriptSyncPending;
          if (!await syncTranscriptOnce(session, request)) return false;
    
          const queuedTarget = Number(session._transcriptSyncPending?.targetRev) || 0;
          const targetRev = Math.max(Number(request.targetRev) || 0, queuedTarget);
          if (transcript.rev < targetRev) mergeTranscriptSyncRequest(session, { reason: 'target-revision', force: true, targetRev });
          const pending = session._transcriptSyncPending;
          const pendingTargetRev = Math.max(0, Number(pending?.targetRev) || 0);
          const pendingForceNeedsFetch = Boolean(pending?.force) && (Boolean(pending?.forceWithoutTarget) || pendingTargetRev === 0);
          if (pending && !pendingForceNeedsFetch && !pending.forceScroll && transcript.rev >= pendingTargetRev) {
            delete session._transcriptSyncPending;
          }
        }
      });
    };
    
    const loadServerSessionMessages = async (sessionId) => {
      const session = findSessionById(sessionId);
      if (!session) return null;
      return (await syncTranscript(session, { reason: 'activation' })) ? window.TermLLMConversation.sessionMessages(session) : null;
    };
    
    const refreshActiveSessionMessagesFromServer = async (session, options = {}) => syncTranscript(session, {
      reason: options.reason || 'refresh',
      force: options.force === true || options.useEtag === false,
      forceScroll: options.forceScroll === true,
      targetRev: options.targetRev
    });
    
    const loadOlderSessionMessages = async (session) => {
      const transcript = ensureSessionTranscript(session);
      if (!transcript) return false;
      const first = transcript.segmentForOrdinal(transcript.viewport.firstOrdinal);
      if (first <= 0) return false;
      return materializeTranscriptSegments(session, [first - 1]);
    };
    
    const maybeLoadOlderSessionMessages = async () => {
      const session = getActiveSession();
      if (!session || (Number(elements.chatScroll?.scrollTop) || 0) > 600) return false;
      return loadOlderSessionMessages(session);
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
    
    const reconcileTranscriptFromStatus = async (statusSessions, options = {}) => {
      if (!Array.isArray(statusSessions)) return false;
      const active = getActiveSession();
      if (!active) return false;
      const entry = statusSessions.find((item) => item?.id === active.id) || null;
      if (!entry) return false;
      const transcript = ensureSessionTranscript(active);
      if (!transcript) return false;
      const incomingRev = Math.max(0, Number(entry.transcript_rev) || 0);
      const activeResponseId = String(entry.active_response_id || '').trim();
      const runEpoch = Math.max(0, Number(entry.run_epoch) || 0);
      const sampledRunEpoch = options.sampledRunEpochs instanceof Map
        ? Math.max(0, Number(options.sampledRunEpochs.get(active.id)) || 0)
        : Math.max(0, Number(transcript.latestRunEpoch) || 0);
      const startedRev = Math.max(0, Number(entry.started_rev) || 0);
      if (activeResponseId && (!runEpoch || runEpoch < sampledRunEpoch)) return false;
      const targetRev = incomingRev;
      let refreshed = false;
      if (targetRev > transcript.rev) {
        refreshed = await syncTranscript(active, {
          reason: activeResponseId ? 'attach' : 'status',
          targetRev,
          force: Boolean(activeResponseId)
        });
      }
      if (activeResponseId) {
        const accepted = await transcript.commands.enqueue(() => (
          transcript.transitionAuthoritativeRun(activeResponseId, startedRev, runEpoch, {
            clientMessageId: entry.client_message_id,
            anchorRowId: entry.anchor_row_id,
          })
        ));
        if (accepted !== true) return refreshed;
        if (transcript.rev >= startedRev
          && !state.abortController
          && !state.streaming
          && !active.activeResponseId
          && document.visibilityState !== 'hidden') {
          await app.syncActiveSessionFromServer(active, true, { skipMessagesFetch: true });
        }
      }
      return refreshed;
    };
    
    Object.assign(app, {
      findSessionById,
      ensureSessionTranscript,
      trackPendingIntent,
      retirePendingIntent,
      noteTranscriptRunCreated,
      noteTranscriptTerminal,
      refreshSessionMessagesFromTranscript,
      touchTranscriptSkeleton,
      transcriptSyncSegmentIndexes,
      materializeTranscriptSegments,
      syncTranscript,
      loadServerSessionMessages,
      refreshActiveSessionMessagesFromServer,
      loadOlderSessionMessages,
      maybeLoadOlderSessionMessages,
      SESSION_STATE_RETRY_RESULT,
      loadServerSessionState,
      reconcileTranscriptFromStatus
    });
  };

  return Object.freeze({
    createConversation,
    addIntent,
    acknowledgeDurableIntents,
    startActiveRun,
    applyRunEvent,
    replaceActiveFromSnapshot,
    responseRowsReady,
    commitDurableHandoff,
    visibleMessages,
    sessionMessages,
    applyDurable,
    SessionCommandQueue,
    ConversationController,
    assistantSegmentKey,
    transcriptToolIdentityKey,
    transcriptIsClientOwnedIntent,
    transcriptDiagnostic,
    attachActiveRun, dispatchRunEvent, replaceRunSnapshot, enqueueDetachedReplay, addPendingIntentToConversation,
    applyTranscriptIndex, materializeTranscriptBodies, destroyConversationController,
    initEffects,
  });
});
