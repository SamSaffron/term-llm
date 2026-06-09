(() => {
'use strict';

const app = window.TermLLMApp;
const {
  UI_PREFIX, STORAGE_KEYS, state, elements, generateId, truncate, asTimestamp, loadSessions, saveSessions, getActiveSession, createSession, ensureActiveSession,
  sessionIdFromURL, sessionSlug, findSessionBySlug, updateURL, scrollToBottom, setConnectionState, setStartupStatus, hideStartupSplash, persistAndRefreshShell, refreshRelativeTimes,
  openAuthModal, closeAuthModal, handleAuthFailure, closeAskUserModal, openAskUserModal, setActiveResponseTracking,
  clearActiveResponseTracking, setStreaming, resumeActiveResponse, renderSidebar, renderMessages, renderProviderOptions, renderModelOptions, normalizeSelectedProvider,
  autoGrowPrompt, updateVoiceUI, toggleVoiceRecording, fetchProviders, fetchModels, addErrorMessage, sendMessage, openSidebar, closeSidebar, closeSidebarIfMobile,
  connectToken, submitAskUserModal, cancelActiveResponse, handleFiles, noteUserScrollIntent, noteScrollPositionChanged, shouldDisableAutoScrollForKey,
  openApprovalModal, closeApprovalModal, submitApprovalModal, registerServiceWorker, subscribeToPush, refreshNotificationUI,
  requestNotificationPermission, shouldAutoSubscribeToPush, detachResponseStream, HEARTBEAT_STALE_THRESHOLD, HEARTBEAT_ABORT_REASON,
  applyDesktopSidebarState, toggleSidebarCollapsed, flushStreamPersistence, requestHeaders, normalizeError, discardPendingAttachments,
  updateSidebarStatus, sessionHasInProgressState, hasAnySessionInProgressState, setSessionServerActiveRun, setSessionOptimisticBusy,
  moveSessionProgressState, requeueUncommittedInterrupts, drainInterruptQueueIfIdle, requeuePendingInterjections,
  trackPendingInterjection, removePendingInterjectionById, trackPendingInterruptCommit, refreshPendingInterjectionBanner,
  restoreDraftMessageForSession, stageDraftMessage, clearDraftMessageForSession
} = app;
let sessionStatePollTimer = null;

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
let sidebarStatusTimer = null;
let sidebarStatusEtag = null;
let sidebarHasActive = false;

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

const stopSidebarStatusPoll = () => {
  if (sidebarStatusTimer !== null) {
    clearTimeout(sidebarStatusTimer);
    sidebarStatusTimer = null;
  }
};

const scheduleSidebarStatusPoll = (delay) => {
  stopSidebarStatusPoll();
  sidebarStatusTimer = setTimeout(pollSidebarStatus, delay);
};

const sidebarStatusPollDelay = () => {
  sidebarHasActive = hasAnySessionInProgressState();
  if (sidebarHasActive) return SIDEBAR_POLL_ACTIVE;
  if (document.visibilityState === 'visible' && !state.draftSessionActive && getActiveSession()) {
    return SIDEBAR_POLL_VISIBLE_ACTIVE;
  }
  return SIDEBAR_POLL_IDLE;
};

const pollSidebarStatus = async () => {
  stopSidebarStatusPoll();
  if (document.visibilityState === 'hidden') return;

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

    const resp = await fetch(`${UI_PREFIX}/v1/sessions/status${query ? `?${query}` : ''}`, { headers });

    if (resp.status === 304) {
      // No metadata change. If a prior status response revealed an active
      // transcript update but the detail fetch was deferred or failed, retry the
      // pending active-session reconciliation without issuing another status
      // request.
      await reconcilePendingActiveTranscriptRefresh();
    } else if (resp.ok) {
      const etag = resp.headers.get('ETag');
      if (etag) sidebarStatusEtag = etag;
      const data = await resp.json();
      if (Array.isArray(data.sessions)) {
        updateSidebarStatus(data.sessions);
        await reconcileActiveTranscriptFromStatus(data.sessions);
        recordTranscriptVersionsFromStatus(data.sessions);
        // Discover sessions created in other tabs/devices
        const localIds = new Set(state.sessions.map((s) => s.id));
        const hasUnknown = data.sessions.some((entry) => !localIds.has(entry.id));
        if (hasUnknown) mergeServerSessions();
      }
    }
  } catch (_e) {
    // Network error — just retry on next interval
  }

  scheduleSidebarStatusPoll(sidebarStatusPollDelay());
};

const startSidebarStatusPoll = () => {
  stopSidebarStatusPoll();
  sidebarStatusEtag = null;
  return pollSidebarStatus();
};

const refreshSidebarStatusPoll = (forceNow = false) => {
  if (document.visibilityState === 'hidden') return Promise.resolve(false);
  if (forceNow) {
    return startSidebarStatusPoll();
  }
  scheduleSidebarStatusPoll(sidebarStatusPollDelay());
  return Promise.resolve(false);
};

const createAndSwitchToFreshSession = async () => {
  await switchToDraftSession({ clearComposer: true, focusPrompt: true });
};

const stageCurrentComposerForSession = (sessionId) => {
  const prompt = String(elements.promptInput.value || '').trim();
  if (prompt) {
    stageDraftMessage(prompt, sessionId);
    return;
  }
  clearDraftMessageForSession(sessionId);
};

const switchToDraftSession = async (options = {}) => {
  const previousActiveSessionId = String(state.activeSessionId || '').trim();
  const previousComposerSessionId = state.draftSessionActive ? '' : previousActiveSessionId;
  stageCurrentComposerForSession(previousComposerSessionId);

  stopSessionStatePoll();
  closeRenameSessionModal();
  closeAskUserModal();
  closeApprovalModal();
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
  restoreDraftMessageForSession('', { replace: true });

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
  const model = String(session.activeModel || '').trim();
  const effort = String(session.activeEffort || '').trim();
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

  if (elements.providerSelect) elements.providerSelect.value = state.selectedProvider || '';
  if (elements.modelSelect) elements.modelSelect.value = state.selectedModel || '';
  if (elements.effortSelect) elements.effortSelect.value = state.selectedEffort || '';
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

  stopSessionStatePoll();
  closeRenameSessionModal();
  if (state.askUser?.sessionId && state.askUser.sessionId !== nextId) {
    closeAskUserModal();
  }
  if (state.approval?.sessionId && state.approval.sessionId !== nextId) {
    closeApprovalModal();
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

  let didPreloadServerMessages = false;
  if (preloadServerMessagesPromise) {
    const msgs = await preloadServerMessagesPromise;
    if (Array.isArray(msgs)) {
      mergeServerMessagesWithLocalState(session, msgs);
      persistAndRefreshShell();
      if (session.id === state.activeSessionId) {
        renderMessages(true);
      }
      didPreloadServerMessages = true;
    }
  }

  if (options.sync !== false) {
    await syncActiveSessionFromServer(session, true, { skipMessagesFetch: didPreloadServerMessages });
  }
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

const messageRangesEquivalent = (messages, leftStart, rightStart, length) => {
  for (let offset = 0; offset < length; offset += 1) {
    if (messageDedupeKey(messages[leftStart + offset]) !== messageDedupeKey(messages[rightStart + offset])) {
      return false;
    }
  }
  return true;
};

const isSyntheticCompactionAckMessage = (message) => (
  message?.role === 'assistant' && String(message.content || '').trim() === "I've reviewed the context summary. I'll continue from where we left off."
);

const compactionDuplicateTailRange = (messages, markerIndex) => {
  if (markerIndex <= 0 || markerIndex + 1 >= messages.length) return { start: -1, length: 0 };
  const candidates = [markerIndex + 1];
  if (isSyntheticCompactionAckMessage(messages[markerIndex + 1])) candidates.push(markerIndex + 2);
  let bestStart = -1;
  let bestLength = 0;
  candidates.forEach((start) => {
    if (start >= messages.length) return;
    const maxLength = Math.min(markerIndex, messages.length - start);
    for (let length = 1; length <= maxLength; length += 1) {
      if (length > bestLength && messageRangesEquivalent(messages, markerIndex - length, start, length)) {
        bestStart = start;
        bestLength = length;
      }
    }
  });
  return { start: bestStart, length: bestLength };
};

const suppressCompactionTailMessages = (messages) => {
  if (!Array.isArray(messages) || messages.length === 0) return messages;
  const out = messages.slice();
  for (let index = 0; index < out.length; index += 1) {
    if (out[index]?.role !== 'compaction') continue;
    if (out[index]?.authoritativeTailSuppressed) continue;
    const { start, length } = compactionDuplicateTailRange(out, index);
    if (length > 0) {
      out.splice(index + 1, start + length - (index + 1));
    } else if (isSyntheticCompactionAckMessage(out[index + 1])) {
      out.splice(index + 1, 1);
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
      ? images.map((url) => String(url || '').trim()).filter(Boolean)
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

  const attachToolResultImages = (part, created, msg, partIndex) => {
    const images = normalizeImages(part.images);
    if (images.length === 0) return;
    const group = ensureToolGroup(created, msg, partIndex);
    const callId = part.tool_call_id || '';
    let tool = callId ? group.tools.find((entry) => entry.id === callId) : null;
    if (!tool && part.tool_name) {
      tool = group.tools.find((entry) => entry.name === part.tool_name);
    }
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
    tool.status = 'done';
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
            dataURL: part.image_url
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
            status: 'done',
            created
          };
          group.tools.push(toolEntry);
        } else {
          toolEntry.name = part.tool_name || toolEntry.name || 'tool';
          toolEntry.arguments = part.tool_arguments || toolEntry.arguments || '';
          toolEntry.status = 'done';
        }
        appendUniqueImages(toolEntry, normalizeImages(part.images));
      } else if (part.type === 'tool_result') {
        attachToolResultImages(part, created, msg, partIndex);
      }
    }

  }

  flushGroup();
  return annotateCompactionBoundary(suppressCompactionTailMessages(result), options);
};

const SESSION_MESSAGES_PAGE_SIZE = 200;
const SESSION_OLDER_LOAD_TOP_THRESHOLD_PX = 600;

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

const maybeLoadOlderSessionMessages = async () => {
  const session = getActiveSession();
  if (!session) return false;
  const history = ensureSessionHistory(session);
  if (!history.loadedTail || !history.hasMoreOlder || history.loadingOlder) return false;
  if (state.abortController || sessionHasInProgressState(session)) return false;

  const chatScroll = elements.chatScroll;
  const scrollTop = Number(chatScroll?.scrollTop) || 0;
  const clientHeight = Number(chatScroll?.clientHeight) || 0;
  const threshold = Math.max(SESSION_OLDER_LOAD_TOP_THRESHOLD_PX, clientHeight * 2);
  if (scrollTop > threshold) return false;

  return loadOlderSessionMessages(session);
};

const loadServerSessionState = async (sessionId) => {
  try {
    const headers = {};
    if (state.token) headers.Authorization = `Bearer ${state.token}`;
    const resp = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(sessionId)}/state`, { headers });
    if (!resp.ok) {
      if (resp.status === 404) {
        return { active_run: false, active_response_id: '' };
      }
      return null;
    }
    const data = await resp.json().catch(() => null);
    if (!data || typeof data !== 'object') return null;
    return data;
  } catch {
    return null;
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

const canRefreshActiveSessionMessages = (session) => {
  if (!session || session.id !== state.activeSessionId) return false;
  if (document.visibilityState === 'hidden') return false;
  if (state.draftSessionActive) return false;
  if (state.abortController || state.streaming) return false;
  if (session.activeResponseId) return false;
  if (state.currentStreamSessionId === session.id && state.currentStreamResponseId) return false;
  if (sessionHasInProgressState(session)) return false;
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

  if (!canRefreshActiveSessionMessages(session)) return false;

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

      if (!canRefreshActiveSessionMessages(session) || session.id !== state.activeSessionId) {
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

const reconcilePendingActiveTranscriptRefresh = async () => {
  const active = getActiveSession();
  if (!active) return false;
  const pendingTranscriptUpdatedAt = numericTranscriptUpdatedAt(active._pendingTranscriptUpdatedAt);
  if (pendingTranscriptUpdatedAt <= 0) return false;
  const currentTranscriptUpdatedAt = numericTranscriptUpdatedAt(active.transcriptUpdatedAt);
  if (pendingTranscriptUpdatedAt <= currentTranscriptUpdatedAt) {
    delete active._pendingTranscriptUpdatedAt;
    return false;
  }

  const refreshed = await refreshActiveSessionMessagesFromServer(active, {
    transcriptUpdatedAt: pendingTranscriptUpdatedAt,
    forceScroll: false
  });
  if (numericTranscriptUpdatedAt(active.transcriptUpdatedAt) === pendingTranscriptUpdatedAt) {
    delete active._pendingTranscriptUpdatedAt;
  }
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
    delete active._pendingTranscriptUpdatedAt;
    return false;
  }

  active._pendingTranscriptUpdatedAt = incomingTranscriptUpdatedAt;
  if (entry.active_run) {
    return false;
  }

  const refreshed = await refreshActiveSessionMessagesFromServer(active, {
    transcriptUpdatedAt: incomingTranscriptUpdatedAt,
    forceScroll: false
  });
  if (numericTranscriptUpdatedAt(active.transcriptUpdatedAt) === incomingTranscriptUpdatedAt) {
    delete active._pendingTranscriptUpdatedAt;
  }
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
    await syncActiveSessionFromServer(active, true);
  }, delay);
};

const syncActiveSessionFromServer = async (session, pollOnActive = false, { skipMessagesFetch = false } = {}) => {
  if (!session) return null;

  const busyBefore = sessionHasInProgressState(session);

  const runtimeState = await loadServerSessionState(session.id);
  if (!runtimeState) return null;

  let sessionChanged = false;
  if (runtimeState.provider && runtimeState.provider !== session.provider) {
    session.provider = runtimeState.provider;
    sessionChanged = true;
  }
  if (runtimeState.model && runtimeState.model !== session.activeModel) {
    session.activeModel = runtimeState.model;
    sessionChanged = true;
  }
  if (runtimeState.reasoning_effort !== undefined) {
    const effort = String(runtimeState.reasoning_effort || '');
    if (effort !== (session.activeEffort || '')) {
      session.activeEffort = effort;
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
    if (session.id === state.activeSessionId && !state.abortController) {
      setStreaming(true);
      resumeAndDrain(session, { responseId: activeResponseId, recoverFromSnapshot });
      return runtimeState;
    }
    if (pollOnActive) {
      scheduleSessionStatePoll(session.id);
    }
    return runtimeState;
  }

  if (activeRun && !state.abortController) {
    updateBusySidebar();
    if (session.id === state.activeSessionId && !state.draftSessionActive) {
      setStreaming(true);
    }
    if (pollOnActive) {
      scheduleSessionStatePoll(session.id);
    }
    return runtimeState;
  }

  if (!activeRun && !state.abortController) {
    stopSessionStatePoll();
    if (session.activeResponseId || state.currentStreamResponseId) {
      clearActiveResponseTracking(session, session.activeResponseId || state.currentStreamResponseId);
      saveSessions();
    }
    setSessionOptimisticBusy(session, false);
    setSessionServerActiveRun(session, false);
    updateBusySidebar();
    setStreaming(false);
    if (!skipMessagesFetch) {
      await refreshActiveSessionMessagesFromServer(session, {
        transcriptUpdatedAt: session.transcriptUpdatedAt,
        forceScroll: true
      });
    }
    if (runtimeState.last_error) {
      addErrorMessage(String(runtimeState.last_error), session);
      persistAndRefreshShell();
      if (session.id === state.activeSessionId) {
        scrollToBottom(true);
      }
    }
    setConnectionState('', '');
    setStreaming(false);
    requeuePendingInterjections(session);
    drainInterruptQueueIfIdle(session);
  }

  return runtimeState;
};

const applyServerSessionSummary = (target, serverSession) => {
  if (!target || !serverSession) return target;
  target.name = String(serverSession.name || '');
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

const openRenameSessionModal = (session) => {
  if (!session?.id) return false;
  state.renameSessionId = session.id;
  elements.renameSessionInput.value = String(session.name || '').trim();
  elements.renameSessionInput.placeholder = String(session.title || 'Project kickoff notes').trim() || 'Project kickoff notes';
  elements.renameSessionError.textContent = '';
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
  elements.renameSessionModal.classList.add('hidden');
  elements.renameSessionError.textContent = '';
  elements.renameSessionInput.value = '';
  elements.renameSessionInput.placeholder = 'Project kickoff notes';
  elements.renameSessionInput.setAttribute('tabindex', '-1');
  elements.renameSessionSaveBtn.disabled = false;
  elements.renameSessionCancelBtn.disabled = false;
  elements.renameSessionSaveBtn.textContent = 'Save';
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

  const nextName = elements.renameSessionInput.value.trim();
  elements.renameSessionError.textContent = '';
  elements.renameSessionSaveBtn.disabled = true;
  elements.renameSessionCancelBtn.disabled = true;
  elements.renameSessionSaveBtn.textContent = 'Saving…';
  try {
    const payload = await updateSessionMetadata(session, { name: nextName });
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
        await switchToDraftSession({ closeSidebar: false });
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
  if (syncSelectedRuntimeFromSession(active)) {
    app.updateHeader();
  }
};

const initialize = async () => {
  setStartupStatus('Loading your chat shell…');
  state.sessions = loadSessions();

  // Check URL for a specific session (number or ID)
  const urlSlug = sessionIdFromURL();
  if (urlSlug) {
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
    setConnectionState('', '');
    startSidebarStatusPoll();
    void refreshWidgetsSidebar();
    if (!state.draftSessionActive && !getActiveSession()) {
      ensureActiveSession();
      renderMessages(true);
    }

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

// File attachment handlers
elements.attachBtn.addEventListener('click', () => {
  elements.fileInput.click();
});
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
  startSidebarStatusPoll();
  const session = getActiveSession();
  if (!session) return;

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
    state.abortController.abort(HEARTBEAT_ABORT_REASON); // triggers retry in resumeActiveResponse
    return;
  }
  if (!state.streaming && !state.abortController) {
    await syncActiveSessionFromServer(session, true);
  }
});

window.addEventListener('pagehide', () => {
  flushStreamPersistence();
});

window.addEventListener('online', async () => {
  setConnectionState('', '');
  const session = getActiveSession();
  if (!session) return;
  if (session.activeResponseId && state.abortController) {
    // Abort the stale fetch so the existing resume loop reconnects immediately
    // instead of waiting for the 45s heartbeat timeout.
    state.abortController._heartbeatAbort = true;
    state.abortController.abort(HEARTBEAT_ABORT_REASON);
  } else if (session.activeResponseId && !state.abortController) {
    setConnectionState('Network restored, reconnecting\u2026');
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
  if (!event.persisted) return;
  const session = getActiveSession();
  if (!session) return;
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

initialize();

Object.assign(app, {
  convertServerMessages,
  loadServerSessionMessages,
  refreshActiveSessionMessagesFromServer,
  loadOlderSessionMessages,
  maybeLoadOlderSessionMessages,
  loadServerSessionState,
  mergeServerMessagesWithLocalState,
  stopSessionStatePoll,
  scheduleSessionStatePoll,
  syncActiveSessionFromServer,
  syncSelectedRuntimeFromSession,
  applyServerSessionSummary,
  mergeServerSessions,
  startSidebarStatusPoll,
  stopSidebarStatusPoll,
  refreshSidebarStatusPoll,
  promptRenameSession,
  setSessionArchived,
  setSessionPinned,
  switchToDraftSession,
  switchToSession,
  initialize
});
})();
