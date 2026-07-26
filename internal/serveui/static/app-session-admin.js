'use strict';

(function initSessionAdmin() {

const app = window.TermLLMApp;
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

const refineSessionTitle = async (session, options = {}) => {
  if (!session?.id || session._refiningTitle) return null;
  session._refiningTitle = true;
  renderSidebar();
  try {
    const resp = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(session.id)}/title/refine`, {
      method: 'POST',
      headers: requestHeaders(session.id),
      body: JSON.stringify({ preview: Boolean(options.preview) })
    });
    if (!resp.ok) {
      throw await normalizeError(resp);
    }
    const payload = await resp.json().catch(() => ({}));
    if (!options.preview) {
      app.reconcileServerSessionIdentity(session, payload);
      app.applyServerSessionSummary(session, payload);
      session.name = String(payload.name || '').trim();
      persistAndRefreshShell();
    }
    return payload;
  } finally {
    session._refiningTitle = false;
    renderSidebar();
  }
};

const setRenameGeneratedMode = (enabled) => {
  state.renameGeneratedMode = Boolean(enabled);
  elements.renameSessionNameField.classList.toggle('hidden', state.renameGeneratedMode);
  elements.renameGeneratedFields.classList.toggle('hidden', !state.renameGeneratedMode);
  elements.renameImproveTitleBtn.textContent = state.renameGeneratedMode ? 'Try again with AI' : 'Improve title with AI';
  elements.renameSessionIntro.textContent = state.renameGeneratedMode
    ? 'Review the AI suggestion before saving it as this session title.'
    : 'Choose the label shown in the sidebar, or let AI suggest a better title from this session.';
  elements.renameSessionInput.tabIndex = state.renameGeneratedMode ? -1 : 0;
  elements.renameGeneratedTitleInput.tabIndex = state.renameGeneratedMode ? 0 : -1;
  elements.renameGeneratedDetailInput.tabIndex = state.renameGeneratedMode ? 0 : -1;
};

const openRenameSessionModal = (session) => {
  if (!session?.id) return false;
  state.renameSessionId = session.id;
  setRenameGeneratedMode(false);
  elements.renameSessionInput.value = String(session.name || '').trim();
  elements.renameSessionInput.placeholder = String(session.title || 'Project kickoff notes').trim() || 'Project kickoff notes';
  elements.renameGeneratedTitleInput.value = String(session.generatedShortTitle || session.title || '').trim();
  elements.renameGeneratedDetailInput.value = String(session.generatedLongTitle || session.longTitle || '').trim();
  elements.renameSessionError.textContent = '';
  elements.renameImproveTitleBtn.disabled = false;
  elements.renameImproveTitleBtn.classList.remove('is-loading');
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
  state.renameGeneratedMode = false;
  elements.renameSessionModal.classList.add('hidden');
  elements.renameSessionError.textContent = '';
  elements.renameSessionInput.value = '';
  elements.renameGeneratedTitleInput.value = '';
  elements.renameGeneratedDetailInput.value = '';
  elements.renameSessionInput.placeholder = 'Project kickoff notes';
  elements.renameSessionInput.setAttribute('tabindex', '-1');
  elements.renameGeneratedTitleInput.setAttribute('tabindex', '-1');
  elements.renameGeneratedDetailInput.setAttribute('tabindex', '-1');
  elements.renameImproveTitleBtn.disabled = false;
  elements.renameImproveTitleBtn.classList.remove('is-loading');
  elements.renameImproveTitleBtn.textContent = 'Improve title with AI';
  elements.renameSessionSaveBtn.disabled = false;
  elements.renameSessionCancelBtn.disabled = false;
  elements.renameSessionSaveBtn.textContent = 'Save';
};

const improveRenameTitleSuggestion = async () => {
  const sessionId = String(state.renameSessionId || '').trim();
  if (!sessionId || elements.renameImproveTitleBtn.disabled) return false;
  const session = state.sessions.find((item) => item.id === sessionId);
  if (!session) return false;
  elements.renameSessionError.textContent = '';
  if (!state.renameGeneratedMode) {
    elements.renameGeneratedTitleInput.value = String(session.generatedShortTitle || session.title || '').trim();
    elements.renameGeneratedDetailInput.value = String(session.generatedLongTitle || session.longTitle || '').trim();
    setRenameGeneratedMode(true);
  }
  elements.renameImproveTitleBtn.disabled = true;
  elements.renameImproveTitleBtn.classList.add('is-loading');
  elements.renameImproveTitleBtn.textContent = 'Improving title…';
  try {
    const payload = await refineSessionTitle(session, { preview: true });
    if (!payload) return false;
    elements.renameGeneratedTitleInput.value = String(payload.generated_short_title || payload.short_title || session.title || '').trim();
    elements.renameGeneratedDetailInput.value = String(payload.generated_long_title || payload.long_title || session.longTitle || '').trim();
    setRenameGeneratedMode(true);
    window.setTimeout(() => {
      elements.renameGeneratedTitleInput.focus();
      elements.renameGeneratedTitleInput.select();
    }, 0);
    return true;
  } catch (err) {
    if (err?.status === 401) {
      closeRenameSessionModal();
      handleAuthFailure();
      return false;
    }
    elements.renameSessionError.textContent = err?.message || 'Failed to improve title.';
    return false;
  } finally {
    elements.renameImproveTitleBtn.disabled = false;
    elements.renameImproveTitleBtn.classList.remove('is-loading');
    elements.renameImproveTitleBtn.textContent = state.renameGeneratedMode ? 'Try again with AI' : 'Improve title with AI';
  }
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

  const patch = state.renameGeneratedMode
    ? {
      name: '',
      generated_short_title: elements.renameGeneratedTitleInput.value.trim(),
      generated_long_title: elements.renameGeneratedDetailInput.value.trim()
    }
    : { name: elements.renameSessionInput.value.trim() };
  elements.renameSessionError.textContent = '';
  elements.renameSessionSaveBtn.disabled = true;
  elements.renameSessionCancelBtn.disabled = true;
  elements.renameSessionSaveBtn.textContent = 'Saving…';
  try {
    const payload = await updateSessionMetadata(session, patch);
    app.reconcileServerSessionIdentity(session, payload);
    app.applyServerSessionSummary(session, payload);
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
    app.reconcileServerSessionIdentity(session, payload);
    app.applyServerSessionSummary(session, payload);
    if (archived && !state.showHiddenSessions) {
      await animateSessionHide(previousId);
      if (session.id !== previousId) await animateSessionHide(session.id);
      if (wasActive || session.id === state.activeSessionId) {
        await app.switchToDraftSession({ closeSidebar: false, clearPreviousComposerDraft: true });
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
    app.reconcileServerSessionIdentity(session, payload);
    app.applyServerSessionSummary(session, payload);
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

Object.assign(app, {
  updateSessionMetadata,
  refineSessionTitle,
  setRenameGeneratedMode,
  openRenameSessionModal,
  closeRenameSessionModal,
  improveRenameTitleSuggestion,
  submitRenameSessionModal,
  promptRenameSession,
  SESSION_HIDE_ANIMATION_MS,
  animateSessionHide,
  setSessionArchived,
  setSessionPinned
});
})();
