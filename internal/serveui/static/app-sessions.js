(() => {
'use strict';

const app = window.TermLLMApp;
const {
  UI_PREFIX, STORAGE_KEYS, state, elements, generateId, truncate, loadSessions, saveSessions, getActiveSession, createSession, ensureActiveSession,
  sessionIdFromURL, updateURL, scrollToBottom, setConnectionState, setStartupStatus, hideStartupSplash, persistAndRefreshShell, refreshRelativeTimes,
  openAuthModal, closeAuthModal, handleAuthFailure, closeAskUserModal, openAskUserModal, setActiveResponseTracking,
  clearActiveResponseTracking, setStreaming, resumeActiveResponse, renderSidebar, renderMessages, renderModelOptions,
  autoGrowPrompt, updateVoiceUI, toggleVoiceRecording, fetchModels, addErrorMessage, sendMessage, openSidebar, closeSidebar, closeSidebarIfMobile,
  connectToken, submitAskUserModal, cancelActiveResponse, handleFiles, isNearBottom,
  openApprovalModal, closeApprovalModal, submitApprovalModal, registerServiceWorker, subscribeToPush, refreshNotificationUI,
  requestNotificationPermission, shouldAutoSubscribeToPush
} = app;
let sessionStatePollTimer = null;

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
  if (merged.length > 0) {
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
  if (!session || !state.token) return;

  const runtimeState = await loadServerSessionState(session.id);
  if (!runtimeState) return;

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
      return;
    }
    if (pollOnActive) {
      scheduleSessionStatePoll(session.id);
    }
    return;
  }

  if (activeRun && !state.abortController) {
    setStreaming(true);
    if (pollOnActive) {
      scheduleSessionStatePoll(session.id);
    }
    return;
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
};

const mergeServerSessions = async () => {
  try {
    const headers = {};
    if (state.token) headers.Authorization = `Bearer ${state.token}`;
    const resp = await fetch(`${UI_PREFIX}/v1/sessions`, { headers });
    if (!resp.ok) return;
    const data = await resp.json();
    if (!Array.isArray(data.sessions)) return;

    const localIds = new Set(state.sessions.map(s => s.id));

    for (const serverSession of data.sessions) {
      if (localIds.has(serverSession.id)) continue;
      // Also check if session ID appears with the sess_ prefix convention
      const prefixedId = `sess_${serverSession.id}`;
      if (localIds.has(prefixedId)) continue;

      state.sessions.push({
        id: serverSession.id,
        title: serverSession.summary || 'New chat',
        created: serverSession.created_at || Date.now(),
        messages: [],
        lastResponseId: null,
        activeResponseId: null,
        lastSequenceNumber: 0,
        messageCount: serverSession.message_count || 0,
        _serverOnly: true
      });
    }

    saveSessions();
    renderSidebar();
  } catch {
    // Gracefully fall back to localStorage-only
  }
};

// ===== Initialization =====
const initialize = async () => {
  setStartupStatus('Loading your chat shell…');
  state.sessions = loadSessions();

  // Check URL for a specific session ID
  const urlSessionId = sessionIdFromURL();
  if (urlSessionId) {
    const found = state.sessions.find(s => s.id === urlSessionId);
    if (found) {
      state.activeSessionId = found.id;
    } else {
      // Create a server-only stub that will be lazy-loaded
      const stub = {
        id: urlSessionId,
        title: 'Loading…',
        created: Date.now(),
        messages: [],
        lastResponseId: null,
        activeResponseId: null,
        lastSequenceNumber: 0,
        _serverOnly: true
      };
      state.sessions.unshift(stub);
      state.activeSessionId = urlSessionId;
    }
  }

  ensureActiveSession();

  renderSidebar();
  renderMessages(true);
  renderModelOptions();
  autoGrowPrompt();
  updateVoiceUI();
  refreshNotificationUI();
  void registerServiceWorker().then(() => refreshNotificationUI());

  try {
    setStartupStatus(state.token ? 'Checking your token…' : 'Connecting…');
    setConnectionState(state.token ? 'Validating token…' : 'Connecting…');
    state.models = await fetchModels();
    state.connected = true;
    renderModelOptions();
    setConnectionState('', '');
    setStartupStatus('Syncing sessions…');

    // Merge server-side sessions after successful auth
    await mergeServerSessions();

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
elements.newChatBtn.addEventListener('click', () => {
  if (state.streaming) return;

  stopSessionStatePoll();
  if (state.askUser) {
    closeAskUserModal();
  }

  const session = createSession();
  state.sessions.unshift(session);
  state.activeSessionId = session.id;
  updateURL(session.id);
  persistAndRefreshShell();
  renderMessages(true);
  elements.promptInput.focus();
  closeSidebarIfMobile();
});

elements.settingsBtn.addEventListener('click', () => {
  openAuthModal('', false);
});

elements.mobileMenuBtn.addEventListener('click', openSidebar);
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

window.addEventListener('resize', () => {
  if (!window.matchMedia('(max-width: 767px)').matches) {
    closeSidebar();
  }
});

window.addEventListener('popstate', async () => {
  const urlId = sessionIdFromURL();
  if (!urlId || urlId === state.activeSessionId) return;

  stopSessionStatePoll();
  if (state.askUser?.sessionId && state.askUser.sessionId !== urlId) {
    closeAskUserModal();
  }

  const found = state.sessions.find(s => s.id === urlId);
  if (found) {
    state.activeSessionId = found.id;
    if (found._serverOnly) {
      const msgs = await loadServerSessionMessages(found.id);
      if (msgs !== null) {
        mergeServerMessagesWithLocalState(found, msgs);
      }
    }
    persistAndRefreshShell();
    renderMessages(true);
    await syncActiveSessionFromServer(found, true);
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
  mergeServerSessions,
  initialize
});
})();
