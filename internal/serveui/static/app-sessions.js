(() => {
'use strict';

const app = window.TermLLMApp;
const {
  UI_PREFIX, STORAGE_KEYS, state, elements, generateId, truncate, asTimestamp, loadSessions, saveSessions, getActiveSession, createSession, ensureActiveSession,
  sessionIdFromURL, sessionSlug, findSessionBySlug, updateURL, scrollToBottom, setConnectionState, setStartupStatus, hideStartupSplash, persistAndRefreshShell, refreshRelativeTimes,
  openAuthModal, closeAuthModal, handleAuthFailure, closeAskUserModal, openAskUserModal, setActiveResponseTracking,
  clearActiveResponseTracking, setStreaming, resumeActiveResponse, renderSidebar, renderMessages, renderProviderOptions, renderModelOptions, normalizeSelectedProvider,
  autoGrowPrompt, updateVoiceUI, toggleVoiceRecording, fetchProviders, fetchModels, addErrorMessage, sendMessage, openSidebar, closeSidebar, closeSidebarIfMobile,
  connectToken, submitAskUserModal, cancelActiveResponse, handleFiles, isNearBottom,
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
      // No change — keep current active state, schedule next poll
    } else if (resp.ok) {
      const etag = resp.headers.get('ETag');
      if (etag) sidebarStatusEtag = etag;
      const data = await resp.json();
      if (Array.isArray(data.sessions)) {
        updateSidebarStatus(data.sessions);
        // Discover sessions created in other tabs/devices
        const localIds = new Set(state.sessions.map((s) => s.id));
        const hasUnknown = data.sessions.some((entry) => !localIds.has(entry.id));
        if (hasUnknown) mergeServerSessions();
      }
    }
  } catch (_e) {
    // Network error — just retry on next interval
  }

  sidebarHasActive = hasAnySessionInProgressState();
  const delay = sidebarHasActive ? SIDEBAR_POLL_ACTIVE : SIDEBAR_POLL_IDLE;
  scheduleSidebarStatusPoll(delay);
};

const startSidebarStatusPoll = () => {
  stopSidebarStatusPoll();
  sidebarStatusEtag = null;
  pollSidebarStatus();
};

const refreshSidebarStatusPoll = (forceNow = false) => {
  if (document.visibilityState === 'hidden') return;
  if (forceNow) {
    startSidebarStatusPoll();
    return;
  }
  sidebarHasActive = hasAnySessionInProgressState();
  const delay = sidebarHasActive ? SIDEBAR_POLL_ACTIVE : SIDEBAR_POLL_IDLE;
  scheduleSidebarStatusPoll(delay);
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

  state.activeSessionId = nextId;
  state.draftSessionActive = false;
  updateURL(sessionSlug(session));
  refreshPendingInterjectionBanner();

  let preloadServerMessagesPromise = null;
  if (session._serverOnly) {
    preloadServerMessagesPromise = loadServerSessionMessages(session.id, {
      onInitialMessages: (messages) => {
        mergeServerMessagesWithLocalState(session, messages);
        persistAndRefreshShell();
        if (session.id === state.activeSessionId) {
          renderMessages(true);
        }
      }
    });
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
const convertServerMessages = (serverMessages) => {
  const result = [];
  let currentGroup = null;

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

  const ensureToolGroup = (created) => {
    if (!currentGroup) {
      currentGroup = {
        id: generateId('msg'),
        role: 'tool-group',
        tools: [],
        expanded: false,
        status: 'done',
        created
      };
    }
    return currentGroup;
  };

  const attachToolResultImages = (part, created) => {
    const images = normalizeImages(part.images);
    if (images.length === 0) return;
    const group = ensureToolGroup(created);
    const callId = part.tool_call_id || '';
    let tool = callId ? group.tools.find((entry) => entry.id === callId) : null;
    if (!tool && part.tool_name) {
      tool = group.tools.find((entry) => entry.name === part.tool_name);
    }
    if (!tool) {
      tool = {
        id: callId || generateId('tool'),
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
    const created = msg.created_at || Date.now();

    if (msg.role === 'system' || msg.role === 'developer') continue;

    if (msg.role === 'event') {
      flushGroup();
      const marker = parts.find((part) => part.type === 'model_swap') || parts.find((part) => part.type === 'text');
      result.push({
        id: generateId('msg'),
        role: 'model-swap',
        content: marker?.text || '↔ Model switch',
        created
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

      result.push({
        id: generateId('msg'),
        role: 'user',
        content: textParts.join('\n'),
        created,
        ...(attachments.length > 0 ? { attachments } : {})
      });
      continue;
    }

    // Walk through assistant parts in order to preserve interleaving with tool calls.
    for (const part of parts) {
      if (part.type === 'text' && part.text) {
        flushGroup();
        result.push({
          id: generateId('msg'),
          role: 'assistant',
          content: part.text,
          created
        });
      } else if (part.type === 'tool_call') {
        const group = ensureToolGroup(created);
        const toolId = part.tool_call_id || generateId('tool');
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
        attachToolResultImages(part, created);
      }
    }

    // If message had no recognized parts, emit text content if present
    if (parts.length === 0 && msg.role === 'assistant') {
      flushGroup();
      result.push({
        id: generateId('msg'),
        role: 'assistant',
        content: '',
        created
      });
    }
  }

  flushGroup();
  return result;
};

const sessionMessagesEtag = new Map();
const SESSION_MESSAGES_PAGE_SIZE = 200;
// The serve UI session API uses stable offset pagination with a fixed page
// size, so prefetch a small window of pages to avoid one RTT per history page.
const SESSION_MESSAGES_PREFETCH_PAGES = 4;

const fetchServerSessionMessagesPage = async (sessionId, { limit = 0, offset = 0, etag = '' } = {}) => {
  const headers = {};
  if (state.token) headers.Authorization = `Bearer ${state.token}`;
  if (etag) headers['If-None-Match'] = etag;

  const params = new URLSearchParams();
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

const loadServerSessionMessages = async (sessionId, { onInitialMessages } = {}) => {
  try {
    const first = await fetchServerSessionMessagesPage(sessionId, { etag: sessionMessagesEtag.get(sessionId) || '' });
    if (first === false) return false;
    if (!first) return null;

    const allMessages = first.data.messages.slice();
    let hasMore = first.data.has_more === true;
    let nextOffset = Number(first.data.next_offset);
    const lastResponseId = String(first.data.lastResponseId || '').trim();
    const seenOffsets = new Set();

    if (hasMore && typeof onInitialMessages === 'function') {
      const initialMessages = convertServerMessages(allMessages);
      if (lastResponseId) initialMessages.lastResponseId = lastResponseId;
      onInitialMessages(initialMessages);
    }

    while (hasMore) {
      const offsets = [];
      let candidateOffset = nextOffset;
      while (offsets.length < SESSION_MESSAGES_PREFETCH_PAGES) {
        if (!Number.isFinite(candidateOffset) || candidateOffset < 0 || seenOffsets.has(candidateOffset)) {
          sessionMessagesEtag.delete(sessionId);
          return null;
        }
        seenOffsets.add(candidateOffset);
        offsets.push(candidateOffset);
        candidateOffset += SESSION_MESSAGES_PAGE_SIZE;
      }

      const pages = await Promise.all(offsets.map((offset) => (
        fetchServerSessionMessagesPage(sessionId, { limit: SESSION_MESSAGES_PAGE_SIZE, offset })
      )));

      let reachedEnd = false;
      for (let index = 0; index < pages.length; index += 1) {
        const page = pages[index];
        const offset = offsets[index];
        if (!page) {
          sessionMessagesEtag.delete(sessionId);
          return null;
        }
        allMessages.push(...page.data.messages);
        if (page.data.has_more === true) {
          nextOffset = Number(page.data.next_offset);
          if (!Number.isFinite(nextOffset) || nextOffset <= offset) {
            sessionMessagesEtag.delete(sessionId);
            return null;
          }
          continue;
        }
        hasMore = false;
        nextOffset = Number(page.data.next_offset);
        reachedEnd = true;
        break;
      }

      if (!reachedEnd) {
        hasMore = true;
      }
    }

    const converted = convertServerMessages(allMessages);
    if (lastResponseId) converted.lastResponseId = lastResponseId;
    if (first.etag) sessionMessagesEtag.set(sessionId, first.etag);
    return converted;
  } catch {
    return null;
  }
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

const mergeServerMessagesWithLocalState = (session, serverMessages) => {
  if (!session || !Array.isArray(serverMessages)) return;

  const syntheticAskUserMessages = session.messages
    .filter((message) => message?.askUser && message.role === 'user' && message.content)
    .map((message) => ({ ...message }));

  const merged = serverMessages.map((message) => ({ ...message }));
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
    const exists = state.pendingInterjections.some(entry =>
      entry.sessionId === session.id && entry.prompt === pendingInterjectionText);
    if (!exists) {
      const syntheticId = `msg_pending_${session.id}_${pendingInterjectionText.length}`;
      trackPendingInterjection(session.id, pendingInterjectionText, syntheticId, 'interject');
      trackPendingInterruptCommit(session.id, pendingInterjectionText, syntheticId);
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
    setStreaming(true);
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
    if (!skipMessagesFetch) {
      const serverMessages = await loadServerSessionMessages(session.id);
      if (Array.isArray(serverMessages)) {
        mergeServerMessagesWithLocalState(session, serverMessages);
        persistAndRefreshShell();
        if (session.id === state.activeSessionId) {
          renderMessages(true);
        }
      }
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
    ? loadServerSessionMessages(active.id, {
      onInitialMessages: (messages) => {
        mergeServerMessagesWithLocalState(active, messages);
        saveSessions();
        renderSidebar();
        if (active.id === state.activeSessionId) {
          renderMessages(true);
        }
      }
    })
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

elements.chatScroll.addEventListener('scroll', () => {
  state.autoScroll = isNearBottom();
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
