'use strict';

(function initSessionEvents() {

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

// ===== Event listeners =====
let pageResumeFollowTail = false;
let pageWasBackgrounded = false;
const rememberPageTailOwnership = () => {
  if (pageWasBackgrounded) return;
  pageResumeFollowTail = Boolean(state.autoScroll);
  pageWasBackgrounded = true;
};
const restorePageTailOwnership = () => {
  if (!pageWasBackgrounded) return false;
  const followTail = pageResumeFollowTail;
  pageWasBackgrounded = false;
  pageResumeFollowTail = false;
  // Mobile viewport resize/scroll events can flip autoScroll while the page is
  // suspended. Restore the user's pre-background ownership instead of treating
  // those synthetic layout events as intentional scrolling in either direction.
  state.autoScroll = followTail;
  return followTail;
};

elements.newChatBtn.addEventListener('click', app.createAndSwitchToFreshSession);
elements.sidebarRailNewChatBtn.addEventListener('click', async () => {
  await app.createAndSwitchToFreshSession();
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
  void app.maybeLoadOlderSessionMessages();
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


elements.attachBtn.addEventListener('click', (event) => {
  event.preventDefault();
  app.toggleAddMenu();
});
if (elements.addAttachOption) {
  elements.addAttachOption.addEventListener('click', () => {
    app.closeAddMenu();
    elements.fileInput.click();
  });
}
document.addEventListener('click', (event) => {
  if (!elements.addMenu || elements.addMenu.hidden) return;
  const target = event.target;
  if (target === elements.attachBtn || target === elements.addMenu) return;
  if (typeof elements.attachBtn.contains === 'function' && elements.attachBtn.contains(target)) return;
  if (typeof elements.addMenu.contains === 'function' && elements.addMenu.contains(target)) return;
  app.closeAddMenu();
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
elements.renameSessionCancelBtn.addEventListener('click', () => app.closeRenameSessionModal());
elements.renameImproveTitleBtn.addEventListener('click', () => {
  void app.improveRenameTitleSuggestion();
});
elements.renameSessionSaveBtn.addEventListener('click', () => {
  void app.submitRenameSessionModal();
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
    app.closeRenameSessionModal();
    return;
  }
  if (event.key === 'Enter' && !event.shiftKey && !event.defaultPrevented) {
    event.preventDefault();
    void app.submitRenameSessionModal();
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
    await app.switchToDraftSession({ closeSidebar: false });
    return;
  }
  const found = findSessionBySlug(urlSlug);
  if (found) {
    if (found.id === state.activeSessionId) return;
    await app.switchToSession(found.id, { closeSidebar: false });
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
    lastResponseId: null,
    activeResponseId: null,
    _serverOnly: true
  };
  state.sessions.unshift(stub);
  await app.switchToSession(stub.id, { closeSidebar: false });
});

app.addNetworkRecoveryHook?.(async (_reason) => {
  if (!state.connected || document.visibilityState === 'hidden') return;
  await app.startSidebarStatusPoll();
  const session = getActiveSession();
  if (!session) return;

  // The sidebar status pass reconciles durable transcript/session truth once
  // before retry waiters are released. This recovers a POST accepted before
  // x-response-id arrived and avoids independent stream/poll stampedes.
  const streamIsStale = Date.now() - Number(state.lastEventTime || 0) > HEARTBEAT_STALE_THRESHOLD;
  if (state.abortController && streamIsStale) {
    state.abortController._heartbeatAbort = true;
    if (typeof state.abortController._heartbeatCancelStream === 'function') {
      await state.abortController._heartbeatCancelStream().catch(() => {});
    } else if (!state.abortController._responseBodyAttached) {
      state.abortController.abort();
    }
  } else if (!state.abortController && session.activeResponseId) {
    setStreaming(true);
    void app.resumeAndDrain?.(session, {
      responseId: session.activeResponseId,
      recoverFromSnapshot: false
    });
  }
});

document.addEventListener('visibilitychange', async () => {
  if (document.visibilityState !== 'visible') {
    rememberPageTailOwnership();
    flushStreamPersistence();
    app.stopSidebarStatusPoll();
    return;
  }
  restorePageTailOwnership();
  if (state.connected) await app.runCoordinatedNetworkRecovery?.('visibility');
});

window.addEventListener('pagehide', () => {
  rememberPageTailOwnership();
  flushStreamPersistence();
  app.stopSidebarStatusPoll();
});

window.addEventListener('online', async () => {
  if (state.connected) await app.runCoordinatedNetworkRecovery?.('online');
});

window.addEventListener('offline', () => {
  app.setConnectivityState?.({ network: 'offline', phase: 'offline' });
});

window.addEventListener('pageshow', async () => {
  restorePageTailOwnership();
  if (state.connected) await app.runCoordinatedNetworkRecovery?.('pageshow');
});
})();
