'use strict';

(function initGoalsLocation() {

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

const openGoalModal = () => {
  const session = ensureActiveSession?.();
  if (!session || !elements.goalModal) return;
  const goal = session.goal || null;
  elements.goalObjectiveInput.value = goal?.objective || '';
  elements.goalTokenBudgetInput.value = goal?.token_budget ? String(goal.token_budget) : '';
  elements.goalError.textContent = '';
  const exists = Boolean(goal && goal.objective);
  const status = String(goal?.status || '').trim() || 'active';
  elements.goalSaveBtn.textContent = exists ? 'Save goal' : 'Set goal';
  elements.goalPauseBtn.hidden = !exists || status !== 'active';
  elements.goalResumeBtn.hidden = !exists || status === 'active' || status === 'complete';
  elements.goalClearBtn.hidden = !exists;
  elements.goalModal.classList.remove('hidden');
  elements.goalObjectiveInput.focus();
};

const closeGoalModal = () => {
  if (elements.goalModal) elements.goalModal.classList.add('hidden');
};

const postSessionGoal = async (action, extra = {}) => {
  const session = ensureActiveSession?.();
  if (!session) return null;
  const response = await app.apiFetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(session.id)}/runtime/goal`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ action, ...extra })
  });
  const data = await response.json().catch(() => ({}));
  if (!response.ok) {
    const message = data?.error?.message || data?.error || `Goal update failed (${response.status})`;
    throw new Error(message);
  }
  session.goal = data.goal || null;
  saveSessions();
  app.updateGoalChip(session);
  renderSidebar();
  return data.goal || null;
};

const saveGoalFromModal = async () => {
  if (!elements.goalObjectiveInput) return;
  const objective = String(elements.goalObjectiveInput.value || '').trim();
  if (!objective) {
    elements.goalError.textContent = 'Objective is required.';
    return;
  }
  const rawBudget = String(elements.goalTokenBudgetInput.value || '').trim();
  const payload = { objective };
  if (rawBudget) {
    const budget = Number(rawBudget);
    if (!Number.isFinite(budget) || budget <= 0) {
      elements.goalError.textContent = 'Token budget must be a positive number.';
      return;
    }
    payload.token_budget = Math.floor(budget);
  }
  try {
    const session = ensureActiveSession?.();
    await postSessionGoal(session?.goal ? 'edit' : 'set', payload);
    closeGoalModal();
  } catch (err) {
    elements.goalError.textContent = err?.message || String(err);
  }
};

const mutateGoalFromModal = async (action) => {
  try {
    await postSessionGoal(action);
    if (action === 'clear') closeGoalModal();
    else openGoalModal();
  } catch (err) {
    if (elements.goalError) elements.goalError.textContent = err?.message || String(err);
  }
};

// Composer add menu and file attachment handlers
let locationRequestPending = false;
let locationStatusTimer = null;

const showLocationStatus = (message, { persistent = false } = {}) => {
  if (!elements.locationStatus) return;
  if (locationStatusTimer) clearTimeout(locationStatusTimer);
  elements.locationStatus.textContent = message;
  elements.locationStatus.classList.toggle('hidden', !message);
  if (message && !persistent) {
    locationStatusTimer = setTimeout(() => {
      elements.locationStatus.textContent = '';
      elements.locationStatus.classList.add('hidden');
    }, 5000);
  }
};

const locationErrorMessage = (error) => {
  if (error?.code === 1) return 'Location permission was denied.';
  if (error?.code === 2) return 'Your device could not determine its location.';
  if (error?.code === 3) return 'Location request timed out.';
  return 'Could not get your current location.';
};

const shareCurrentLocation = () => {
  if (locationRequestPending) return;
  if (!window.isSecureContext) {
    showLocationStatus('Location sharing requires HTTPS or localhost.');
    return;
  }
  if (!navigator.geolocation || typeof navigator.geolocation.getCurrentPosition !== 'function') {
    showLocationStatus('Location sharing is not supported in this browser.');
    return;
  }

  locationRequestPending = true;
  elements.addLocationOption.disabled = true;
  showLocationStatus('Getting your current location…', { persistent: true });
  navigator.geolocation.getCurrentPosition((position) => {
    const latitude = Number(position.coords.latitude).toFixed(5);
    const longitude = Number(position.coords.longitude).toFixed(5);
    const accuracy = Math.max(1, Math.round(Number(position.coords.accuracy) || 0));
    const locationText = [
      'My current location:',
      `- Coordinates: ${latitude}, ${longitude}`,
      `- Accuracy: approximately ${accuracy} m`,
      `- Map: https://www.openstreetmap.org/?mlat=${latitude}&mlon=${longitude}#map=16/${latitude}/${longitude}`,
    ].join('\n');
    const existing = elements.promptInput.value.trimEnd();
    elements.promptInput.value = existing ? `${existing}\n\n${locationText}` : locationText;
    autoGrowPrompt();
    elements.promptInput.focus();
    showLocationStatus('Location added to your message. Review it before sending.');
    locationRequestPending = false;
    elements.addLocationOption.disabled = false;
  }, (error) => {
    showLocationStatus(locationErrorMessage(error));
    locationRequestPending = false;
    elements.addLocationOption.disabled = false;
  }, {
    enableHighAccuracy: false,
    timeout: 12000,
    maximumAge: 60000,
  });
};

if (elements.addLocationOption) {
  const locationEnabled = window.TERM_LLM_LOCATION_SHARING_ENABLED !== false;
  elements.addLocationOption.hidden = !locationEnabled;
}

if (elements.addLocationOption) {
  elements.addLocationOption.addEventListener('click', () => {
    app.closeAddMenu();
    shareCurrentLocation();
  });
}
if (elements.addMCPOption) {
  elements.addMCPOption.addEventListener('click', async () => {
    app.closeAddMenu();
    await app.openSessionMCPModal();
  });
}
if (elements.addGoalOption) {
  elements.addGoalOption.addEventListener('click', () => {
    app.closeAddMenu();
    openGoalModal();
  });
}
if (elements.goalChip) {
  elements.goalChip.addEventListener('click', () => {
    openGoalModal();
  });
}
if (elements.goalModalCloseBtn) {
  elements.goalModalCloseBtn.addEventListener('click', closeGoalModal);
}
if (elements.goalSaveBtn) {
  elements.goalSaveBtn.addEventListener('click', () => {
    void saveGoalFromModal();
  });
}
if (elements.goalPauseBtn) {
  elements.goalPauseBtn.addEventListener('click', () => {
    void mutateGoalFromModal('pause');
  });
}
if (elements.goalResumeBtn) {
  elements.goalResumeBtn.addEventListener('click', () => {
    void mutateGoalFromModal('resume');
  });
}
if (elements.goalClearBtn) {
  elements.goalClearBtn.addEventListener('click', () => {
    void mutateGoalFromModal('clear');
  });
}
if (elements.goalModal) {
  elements.goalModal.addEventListener('click', (event) => {
    if (event.target === elements.goalModal) closeGoalModal();
  });
  elements.goalModal.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && !event.defaultPrevented) {
      event.preventDefault();
      closeGoalModal();
      return;
    }
    if ((event.key === 'Enter' || event.key === 'NumpadEnter') && (event.metaKey || event.ctrlKey) && !event.defaultPrevented) {
      event.preventDefault();
      void saveGoalFromModal();
    }
  });
}

Object.assign(app, {
  openGoalModal,
  closeGoalModal,
  postSessionGoal,
  saveGoalFromModal,
  mutateGoalFromModal,
  locationRequestPending,
  locationStatusTimer,
  showLocationStatus,
  locationErrorMessage,
  shareCurrentLocation
});
})();
