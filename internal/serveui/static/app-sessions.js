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
  applyDesktopSidebarState, toggleSidebarCollapsed, flushStreamPersistence, requestHeaders, normalizeError, renderAttachments,
  updateSidebarStatus
} = app;
let sessionStatePollTimer = null;

// ===== Sidebar status polling =====
const SIDEBAR_POLL_ACTIVE = 2000;
const SIDEBAR_POLL_IDLE = 30000;
let sidebarStatusTimer = null;
let sidebarStatusEtag = null;
let sidebarHasActive = false;

const stopSidebarStatusPoll = () => {
  if (sidebarStatusTimer !== null) {
    clearTimeout(sidebarStatusTimer);
    sidebarStatusTimer = null;
  }
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
        sidebarHasActive = data.sessions.some(s => s.active_run);
        updateSidebarStatus(data.sessions);
      }
    }
  } catch (_e) {
    // Network error — just retry on next interval
  }

  const delay = sidebarHasActive ? SIDEBAR_POLL_ACTIVE : SIDEBAR_POLL_IDLE;
  sidebarStatusTimer = setTimeout(pollSidebarStatus, delay);
};

const startSidebarStatusPoll = () => {
  stopSidebarStatusPoll();
  sidebarStatusEtag = null;
  pollSidebarStatus();
};

const createAndSwitchToFreshSession = async () => {
  await switchToDraftSession({ clearComposer: true, focusPrompt: true });
};

const switchToDraftSession = async (options = {}) => {
  const previousActiveSessionId = String(state.activeSessionId || '').trim();

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
    state.attachments = [];
    renderAttachments();
    autoGrowPrompt();
  }

  persistAndRefreshShell();
  renderMessages(true);

  if (options.focusPrompt) {
    elements.promptInput.focus();
  }
  if (options.closeSidebar !== false) {
    closeSidebarIfMobile();
  }
  return null;
};

const switchToSession = async (sessionId, options = {}) => {
  const nextId = String(sessionId || '').trim();
  if (!nextId) return null;

  const previousActiveSessionId = String(state.activeSessionId || '').trim();
  const session = state.sessions.find((item) => item.id === nextId);
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

  if (session._serverOnly) {
    const msgs = await loadServerSessionMessages(session.id);
    if (msgs !== null) {
      mergeServerMessagesWithLocalState(session, msgs);
    }
  }

  persistAndRefreshShell();
  renderMessages(true);

  if (options.sync !== false) {
    await syncActiveSessionFromServer(session, true);
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

  const flushGroup = () => {
    if (currentGroup) {
      result.push(currentGroup);
      currentGroup = null;
    }
  };

  for (const msg of serverMessages) {
    const parts = Array.isArray(msg.parts) ? msg.parts : [];
    const created = msg.created_at || Date.now();

    if (msg.role === 'system') continue;

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
        const toolEntry = {
          id: part.tool_call_id || generateId('tool'),
          name: part.tool_name || 'tool',
          arguments: part.tool_arguments || '',
          status: 'done',
          created
        };
        if (!currentGroup) {
          currentGroup = {
            id: generateId('msg'),
            role: 'tool-group',
            tools: [toolEntry],
            expanded: false,
            status: 'done',
            created
          };
        } else {
          currentGroup.tools.push(toolEntry);
        }
      }
      // tool_result parts are context for the LLM, skip in UI
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

const loadServerSessionMessages = async (sessionId) => {
  try {
    const headers = {};
    if (state.token) headers.Authorization = `Bearer ${state.token}`;
    const resp = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(sessionId)}/messages`, { headers });
    if (!resp.ok) return null;
    const data = await resp.json();
    if (!Array.isArray(data.messages)) return null;
    return convertServerMessages(data.messages);
  } catch {
    return null;
  }
};

const loadServerSessionState = async (sessionId) => {
  try {
    const headers = {};
    if (state.token) headers.Authorization = `Bearer ${state.token}`;
    const resp = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(sessionId)}/state`, { headers });
    if (!resp.ok) return null;
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

  session.messages = merged;
  delete session._serverOnly;
  if (merged.length > 0 && (!session.title || session.title === 'New chat')) {
    const firstUserMsg = merged.find((message) => message.role === 'user' && !message.askUser);
    if (firstUserMsg?.content) {
      session.title = truncate(firstUserMsg.content, 60);
    }
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

const syncActiveSessionFromServer = async (session, pollOnActive = false) => {
  if (!session || !state.token) return null;

  const runtimeState = await loadServerSessionState(session.id);
  if (!runtimeState) return null;

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

  const activeResponseId = String(runtimeState.active_response_id || '').trim();
  if (activeResponseId) {
    const responseChanged = session.activeResponseId !== activeResponseId;
    setActiveResponseTracking(session, activeResponseId, responseChanged ? 0 : session.lastSequenceNumber);
    saveSessions();
  }

  const activeRun = Boolean(runtimeState.active_run);
  if (activeResponseId) {
    if (session.id === state.activeSessionId && !state.abortController) {
      setStreaming(true);
      void resumeActiveResponse(session, { responseId: activeResponseId });
      return runtimeState;
    }
    if (pollOnActive) {
      scheduleSessionStatePoll(session.id);
    }
    return runtimeState;
  }

  if (activeRun && !state.abortController) {
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
    const serverMessages = await loadServerSessionMessages(session.id);
    if (serverMessages !== null) {
      mergeServerMessagesWithLocalState(session, serverMessages);
      persistAndRefreshShell();
      if (session.id === state.activeSessionId) {
        renderMessages(true);
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
  target.messageCount = Number(serverSession.message_count || target.messageCount || 0);
  target.number = Number(serverSession.number || target.number || 0);
  return target;
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

    for (const serverSession of data.sessions) {
      let local = state.sessions.find((item) => item.id === serverSession.id) || null;
      if (local) {
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
    // Gracefully fall back to localStorage-only
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
    const payload = await updateSessionMetadata(session, { archived });
    applyServerSessionSummary(session, payload);
    if (archived && !state.showHiddenSessions) {
      await animateSessionHide(session.id);
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
        id: num > 0 ? `pending_${urlSlug}` : urlSlug,
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

    // Fetch providers first so we can validate the stored selection.
    state.providers = await fetchProviders();
    normalizeSelectedProvider();
    renderProviderOptions();

    state.models = await fetchModels('', state.selectedProvider);
    state.connected = true;
    renderModelOptions();
    setConnectionState('', '');
    setStartupStatus('Syncing sessions…');

    // Merge server-side sessions after successful auth
    await mergeServerSessions();
    startSidebarStatusPoll();
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

    // Lazy-load messages for URL-targeted server session
    const active = getActiveSession();
    if (active && active._serverOnly) {
      const msgs = await loadServerSessionMessages(active.id);
      if (msgs !== null) {
        mergeServerMessagesWithLocalState(active, msgs);
        saveSessions();
        renderSidebar();
        renderMessages(true);
      }
    }
    if (active) {
      await syncActiveSessionFromServer(active, true);
    }
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
  const session = getActiveSession();
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
    id: num > 0 ? `pending_${urlSlug}` : urlSlug,
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
    void resumeActiveResponse(session, { responseId: session.activeResponseId });
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
    void resumeActiveResponse(session, { responseId: session.activeResponseId });
  } else if (!state.streaming) {
    await syncActiveSessionFromServer(session, true);
  }
});

window.addEventListener('offline', () => {
  if (state.streaming || state.abortController) {
    setConnectionState('Network offline', 'bad');
  }
});

window.addEventListener('pageshow', (event) => {
  if (!event.persisted) return;
  const session = getActiveSession();
  if (!session) return;
  if (session.activeResponseId) {
    setStreaming(true);
    void resumeActiveResponse(session, { responseId: session.activeResponseId });
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
  applyServerSessionSummary,
  mergeServerSessions,
  startSidebarStatusPoll,
  stopSidebarStatusPoll,
  promptRenameSession,
  setSessionArchived,
  setSessionPinned,
  switchToDraftSession,
  switchToSession,
  initialize
});
})();
