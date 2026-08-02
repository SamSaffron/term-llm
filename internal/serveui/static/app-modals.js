'use strict';
(function initAppModals() {
const app = window.TermLLMApp;
const {
  UI_PREFIX, STORAGE_KEYS, state, elements, generateId, saveSessions,
  persistAndRefreshShell, setConnectionState, syncTokenCookie,
  setSessionOptimisticBusy, renderSidebar, subscribeToPush,
  shouldAutoSubscribeToPush, getActiveSession, requestHeaders, normalizeError,
  fetchProviders, fetchModels, setStreaming, normalizeSelectedProvider,
  canonicalizeSelectedModelEffort, renderProviderOptions, renderModelOptions,
  isSessionVisible, scrollVisibleStreamToBottom,
  modelMetadataFor, renderEffortOptions, persistRuntimeSelection,
  markRuntimeSelectionIntent, runtimeHasPendingAskUser, runtimeHasPendingApproval,
  refreshSessionFromServerTruth
} = app;

// ===== ask_user modal =====
const closeAskUserModal = () => {
  state.askUser = null;
  elements.askUserModal.classList.add('hidden');
  elements.askUserModalBody.innerHTML = '';
  elements.askUserError.textContent = '';
  elements.askUserSubmitBtn.disabled = false;
  elements.askUserCancelBtn.disabled = false;
  elements.askUserSubmitBtn.textContent = 'Continue';
  elements.askUserCancelBtn.textContent = 'Dismiss';
};

const askUserSummaryFromAnswers = (answers) => {
  if (!Array.isArray(answers) || answers.length === 0) return '';
  return answers
    .map((answer) => {
      const header = String(answer?.header || '').trim();
      const selected = String(answer?.selected || '').trim();
      if (!header) return selected;
      return `${header}: ${selected}`;
    })
    .filter(Boolean)
    .join(' | ');
};

const collectAskUserAnswers = () => {
  const prompt = state.askUser;
  if (!prompt) {
    throw new Error('No pending question.');
  }
  const answers = [];
  prompt.questions.forEach((question, index) => {
    const name = `ask_user_${index}`;
    if (question.multi_select) {
      const selectedList = Array.from(elements.askUserModalBody.querySelectorAll(`input[name="${name}"]:checked`))
        .map((input) => String(input.value || '').trim())
        .filter(Boolean);
      if (selectedList.length === 0) {
        throw new Error(`${question.header || `Question ${index + 1}`}: choose at least one option.`);
      }
      answers.push({
        question_index: index,
        header: question.header,
        selected: selectedList.join(', '),
        selected_list: selectedList,
        is_custom: false,
        is_multi_select: true
      });
      return;
    }

    const selected = elements.askUserModalBody.querySelector(`input[name="${name}"]:checked`);
    if (!selected) {
      throw new Error(`${question.header || `Question ${index + 1}`}: choose an option.`);
    }
    if (selected.value === '__custom__') {
      const textarea = elements.askUserModalBody.querySelector(`#askUserCustom_${index}`);
      const custom = String(textarea?.value || '').trim();
      if (!custom) {
        throw new Error(`${question.header || `Question ${index + 1}`}: enter your answer.`);
      }
      answers.push({
        question_index: index,
        header: question.header,
        selected: custom,
        is_custom: true,
        is_multi_select: false
      });
      return;
    }
    answers.push({
      question_index: index,
      header: question.header,
      selected: String(selected.value || '').trim(),
      is_custom: false,
      is_multi_select: false
    });
  });
  return answers;
};

const validateSingleQuestion = (index) => {
  const question = state.askUser?.questions[index];
  if (!question) return;
  const name = `ask_user_${index}`;

  if (question.multi_select) {
    const checked = elements.askUserModalBody.querySelectorAll(`input[name="${name}"]:checked`);
    if (checked.length === 0) throw new Error('Choose at least one option.');
    return;
  }

  const selected = elements.askUserModalBody.querySelector(`input[name="${name}"]:checked`);
  if (!selected) throw new Error('Choose an option.');
  if (selected.value === '__custom__') {
    const textarea = elements.askUserModalBody.querySelector(`#askUserCustom_${index}`);
    const custom = String(textarea?.value || '').trim();
    if (!custom) throw new Error('Enter your answer.');
  }
};

const switchAskUserTab = (newIndex) => {
  const prompt = state.askUser;
  if (!prompt) return;
  const total = prompt.questions.length;
  if (newIndex < 0 || newIndex >= total) return;

  prompt.activeTab = newIndex;

  elements.askUserModalBody.querySelectorAll('.ask-user-question').forEach((section) => {
    const idx = parseInt(section.dataset.questionIndex, 10);
    section.style.display = idx === newIndex ? '' : 'none';
  });

  elements.askUserModalBody.querySelectorAll('.ask-user-step').forEach((step, i) => {
    step.classList.toggle('active', i === newIndex);
    step.classList.toggle('completed', i < newIndex);
  });
  elements.askUserModalBody.querySelectorAll('.ask-user-step-line').forEach((line, i) => {
    line.classList.toggle('done', i + 1 <= newIndex);
  });

  elements.askUserModalTitle.textContent = `Question ${newIndex + 1} of ${total}`;
  elements.askUserCancelBtn.textContent = newIndex > 0 ? 'Back' : 'Dismiss';
  elements.askUserSubmitBtn.textContent = newIndex < total - 1 ? 'Next' : 'Continue';
  elements.askUserError.textContent = '';

  const activeSection = elements.askUserModalBody.querySelector(`.ask-user-question[data-question-index="${newIndex}"]`);
  if (activeSection) {
    const firstInput = activeSection.querySelector('input, textarea');
    firstInput?.focus();
  }
};

const renderAskUserModal = () => {
  const prompt = state.askUser;
  if (!prompt) return;

  const total = prompt.questions.length;
  const activeTab = prompt.activeTab || 0;

  elements.askUserModalTitle.textContent = total === 1 ? 'Answer question' : `Question ${activeTab + 1} of ${total}`;
  elements.askUserModalSubtitle.textContent = 'The agent needs your input to continue.';
  elements.askUserModalBody.innerHTML = '';
  elements.askUserError.textContent = '';

  if (total > 1) {
    const steps = document.createElement('div');
    steps.className = 'ask-user-steps';
    for (let i = 0; i < total; i++) {
      if (i > 0) {
        const line = document.createElement('div');
        line.className = 'ask-user-step-line';
        if (i <= activeTab) line.classList.add('done');
        steps.appendChild(line);
      }
      const dot = document.createElement('button');
      dot.type = 'button';
      dot.className = 'ask-user-step';
      if (i === activeTab) dot.classList.add('active');
      else if (i < activeTab) dot.classList.add('completed');
      dot.textContent = i + 1;
      dot.addEventListener('click', () => switchAskUserTab(i));
      steps.appendChild(dot);
    }
    elements.askUserModalBody.appendChild(steps);
  }

  prompt.questions.forEach((question, index) => {
    const section = document.createElement('section');
    section.className = 'ask-user-question';
    section.dataset.questionIndex = index;
    if (index !== activeTab) section.style.display = 'none';

    const headerEl = document.createElement('div');
    headerEl.className = 'ask-user-question-header';
    headerEl.textContent = question.header || `Question ${index + 1}`;
    section.appendChild(headerEl);

    const textEl = document.createElement('p');
    textEl.className = 'ask-user-question-text';
    textEl.textContent = question.question || '';
    section.appendChild(textEl);

    const options = document.createElement('div');
    options.className = 'ask-user-options';
    const inputType = question.multi_select ? 'checkbox' : 'radio';
    const groupName = `ask_user_${index}`;

    (Array.isArray(question.options) ? question.options : []).forEach((option) => {
      const label = document.createElement('label');
      label.className = 'ask-user-option';

      const input = document.createElement('input');
      input.type = inputType;
      input.name = groupName;
      input.value = option.label || '';

      const copy = document.createElement('span');
      copy.className = 'ask-user-option-copy';

      const titleEl = document.createElement('span');
      titleEl.className = 'ask-user-option-title';
      titleEl.textContent = option.label || 'Option';

      copy.appendChild(titleEl);
      if (option.description) {
        const desc = document.createElement('span');
        desc.className = 'ask-user-option-desc';
        desc.textContent = option.description;
        copy.appendChild(desc);
      }

      label.appendChild(input);
      label.appendChild(copy);
      options.appendChild(label);
    });

    if (!question.multi_select) {
      const customLabel = document.createElement('label');
      customLabel.className = 'ask-user-option';

      const customRadio = document.createElement('input');
      customRadio.type = 'radio';
      customRadio.name = groupName;
      customRadio.value = '__custom__';

      const customCopy = document.createElement('span');
      customCopy.className = 'ask-user-option-copy';

      const customTitle = document.createElement('span');
      customTitle.className = 'ask-user-option-title';
      customTitle.textContent = 'Other';

      const customDesc = document.createElement('span');
      customDesc.className = 'ask-user-option-desc';
      customDesc.textContent = 'Type your own answer.';

      customCopy.appendChild(customTitle);
      customCopy.appendChild(customDesc);
      customLabel.appendChild(customRadio);
      customLabel.appendChild(customCopy);
      options.appendChild(customLabel);

      section.appendChild(options);

      const textarea = document.createElement('textarea');
      textarea.id = `askUserCustom_${index}`;
      textarea.className = 'ask-user-custom-input';
      textarea.placeholder = 'Type your answer\u2026';
      textarea.addEventListener('focus', () => {
        customRadio.checked = true;
        textarea.classList.add('visible');
      });

      section.addEventListener('change', () => {
        textarea.classList.toggle('visible', customRadio.checked);
        if (customRadio.checked) setTimeout(() => textarea.focus(), 0);
      });

      section.appendChild(textarea);
    } else {
      section.appendChild(options);
    }

    const note = document.createElement('div');
    note.className = 'ask-user-note';
    note.textContent = question.multi_select
      ? 'Choose one or more options to continue.'
      : 'Choose one option or provide a custom answer.';
    section.appendChild(note);
    elements.askUserModalBody.appendChild(section);
  });

  if (total > 1) {
    elements.askUserCancelBtn.textContent = activeTab > 0 ? 'Back' : 'Dismiss';
    elements.askUserSubmitBtn.textContent = activeTab < total - 1 ? 'Next' : 'Continue';
  } else {
    elements.askUserCancelBtn.textContent = 'Dismiss';
    elements.askUserSubmitBtn.textContent = 'Continue';
  }
};

const openAskUserModal = (sessionId, callId, questions) => {
  if (!sessionId || !callId || !Array.isArray(questions) || questions.length === 0) return;
  state.askUser = {
    sessionId,
    callId,
    activeTab: 0,
    questions: questions.map((question) => ({
      ...question,
      options: Array.isArray(question?.options) ? question.options.map((option) => ({ ...option })) : []
    }))
  };
  renderAskUserModal();
  elements.askUserModal.classList.remove('hidden');
  setTimeout(() => {
    const firstInput = elements.askUserModalBody.querySelector('input, textarea');
    firstInput?.focus();
  }, 0);
};

const submitAskUserModal = async (cancelled = false) => {
  const prompt = state.askUser;
  if (!prompt) return;

  const total = prompt.questions.length;
  const activeTab = prompt.activeTab || 0;

  // Multi-question: "Back" button (cancel on non-first tab goes back)
  if (cancelled && total > 1 && activeTab > 0) {
    switchAskUserTab(activeTab - 1);
    return;
  }

  // Multi-question: "Next" button (submit on non-last tab advances)
  if (!cancelled && total > 1 && activeTab < total - 1) {
    try {
      validateSingleQuestion(activeTab);
    } catch (err) {
      elements.askUserError.textContent = err?.message || 'Please answer the question.';
      return;
    }
    switchAskUserTab(activeTab + 1);
    return;
  }

  let answers = [];
  if (!cancelled) {
    try {
      answers = collectAskUserAnswers();
    } catch (err) {
      elements.askUserError.textContent = err?.message || 'Please answer all questions.';
      return;
    }
  }

  elements.askUserError.textContent = '';
  elements.askUserSubmitBtn.disabled = true;
  elements.askUserCancelBtn.disabled = true;
  elements.askUserSubmitBtn.textContent = cancelled ? 'Closing…' : 'Sending…';
  elements.askUserCancelBtn.textContent = cancelled ? 'Dismissing…' : 'Dismiss';

  try {
    const response = await app.apiFetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(prompt.sessionId)}/ask_user`, {
      method: 'POST',
      headers: requestHeaders(prompt.sessionId),
      body: JSON.stringify(cancelled
        ? { call_id: prompt.callId, cancelled: true }
        : { call_id: prompt.callId, answers })
    });
    if (!response.ok) {
      throw await normalizeError(response);
    }
    const payload = await response.json().catch(() => ({}));
    if (!cancelled) {
      const session = state.sessions.find((item) => item.id === prompt.sessionId);
      if (session) {
        const normalized = Array.isArray(payload.answers) ? payload.answers : answers;
        const summary = String(payload.summary || askUserSummaryFromAnswers(normalized) || 'Answered prompt').trim();
        if (summary) {
          const message = {
            id: generateId('msg'),
            role: 'user',
            content: summary,
            created: Date.now(),
            askUser: true,
            askUserCallId: prompt.callId
          };
          app.trackPendingIntent?.(session, Object.assign(message, { clientMessageId: message.id }));
          if (isSessionVisible(session)) {
            const empty = elements.messages.querySelector('.empty-state');
            if (empty) empty.remove();
            app.renderMessages?.();
          }
          saveSessions();
          scrollVisibleStreamToBottom(session, true);
        }
      }
    }
    closeAskUserModal();
    if (!state.abortController) {
      setSessionOptimisticBusy(prompt.sessionId, true);
      setStreaming(true);
      persistAndRefreshShell();
      app.refreshSidebarStatusPoll?.();
      app.scheduleSessionStatePoll(prompt.sessionId, 400);
    }
  } catch (err) {
    if (err?.status === 409) {
      const session = state.sessions.find((item) => item.id === prompt.sessionId) || null;
      const runtimeState = session ? await refreshSessionFromServerTruth(session, true) : null;
      if (!runtimeHasPendingAskUser(runtimeState, prompt.callId)) {
        closeAskUserModal();
        return;
      }
    }

    elements.askUserError.textContent = err?.message || 'Failed to submit your answer.';
    if (err?.status === 401) {
      handleAuthFailure();
    }
    elements.askUserSubmitBtn.disabled = false;
    elements.askUserCancelBtn.disabled = false;
    elements.askUserSubmitBtn.textContent = 'Continue';
    elements.askUserCancelBtn.textContent = 'Dismiss';
  }
};

// ===== Approval modal =====
const openApprovalModal = (sessionId, approvalId, path, isShell, title, options) => {
  state.approval = { sessionId, approvalId, path, isShell, title, options, selectedIndex: 0 };

  elements.approvalTitle.textContent = title || 'Access Request';
  elements.approvalPath.textContent = path || '';
  elements.approvalError.textContent = '';
  elements.approvalApproveBtn.disabled = false;
  elements.approvalDenyBtn.disabled = false;
  elements.approvalApproveBtn.textContent = 'Approve';
  elements.approvalDenyBtn.textContent = 'Deny';

  // Build radio options as a vertical list
  const body = elements.approvalBody;
  body.innerHTML = '';
  const group = document.createElement('div');
  group.className = 'approval-options';
  options.forEach((opt, i) => {
    const label = document.createElement('label');
    label.className = 'approval-option';

    const radio = document.createElement('input');
    radio.type = 'radio';
    radio.name = 'approval_choice';
    radio.value = String(opt.index != null ? opt.index : i);
    if (i === 0) radio.checked = true;
    radio.addEventListener('change', () => { state.approval.selectedIndex = Number(radio.value); });

    const copy = document.createElement('div');
    copy.className = 'approval-option-copy';
    const titleEl = document.createElement('span');
    titleEl.className = 'approval-option-title';
    titleEl.textContent = opt.label || `Option ${i + 1}`;
    copy.appendChild(titleEl);
    if (opt.description) {
      const desc = document.createElement('span');
      desc.className = 'approval-option-desc';
      desc.textContent = opt.description;
      copy.appendChild(desc);
    }

    label.appendChild(radio);
    label.appendChild(copy);
    group.appendChild(label);
  });
  body.appendChild(group);

  elements.approvalModal.classList.remove('hidden');
  setTimeout(() => {
    const firstRadio = body.querySelector('input[type="radio"]');
    firstRadio?.focus();
  }, 0);
};

const closeApprovalModal = () => {
  state.approval = null;
  elements.approvalModal.classList.add('hidden');
  elements.approvalBody.innerHTML = '';
  elements.approvalError.textContent = '';
  elements.approvalApproveBtn.disabled = false;
  elements.approvalDenyBtn.disabled = false;
  elements.approvalApproveBtn.textContent = 'Approve';
  elements.approvalDenyBtn.textContent = 'Deny';
};

const submitApprovalModal = async (denied = false) => {
  const prompt = state.approval;
  if (!prompt) return;

  elements.approvalError.textContent = '';
  elements.approvalApproveBtn.disabled = true;
  elements.approvalDenyBtn.disabled = true;
  elements.approvalApproveBtn.textContent = denied ? 'Approve' : 'Sending…';
  elements.approvalDenyBtn.textContent = denied ? 'Denying…' : 'Deny';

  // Find the deny option by its choice field rather than assuming position.
  const denyOpt = prompt.options.find(o => o.choice === 'deny');
  const denyIndex = denyOpt ? denyOpt.index : prompt.options.length - 1;
  const choiceIndex = denied ? denyIndex : prompt.selectedIndex;
  const body = { approval_id: prompt.approvalId, choice: choiceIndex };

  try {
    const response = await app.apiFetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(prompt.sessionId)}/approval`, {
      method: 'POST',
      headers: requestHeaders(prompt.sessionId),
      body: JSON.stringify(body)
    });
    if (!response.ok) {
      throw await normalizeError(response);
    }
    closeApprovalModal();
    if (!state.abortController) {
      setSessionOptimisticBusy(prompt.sessionId, true);
      setStreaming(true);
      persistAndRefreshShell();
      app.refreshSidebarStatusPoll?.();
      app.scheduleSessionStatePoll(prompt.sessionId, 400);
    }
  } catch (err) {
    if (err?.status === 409) {
      const session = state.sessions.find((item) => item.id === prompt.sessionId) || null;
      const runtimeState = session ? await refreshSessionFromServerTruth(session, true) : null;
      if (!runtimeHasPendingApproval(runtimeState, prompt.approvalId)) {
        closeApprovalModal();
        return;
      }
    }

    elements.approvalError.textContent = err?.message || 'Failed to submit approval.';
    if (err?.status === 401) {
      handleAuthFailure();
    }
    elements.approvalApproveBtn.disabled = false;
    elements.approvalDenyBtn.disabled = false;
    elements.approvalApproveBtn.textContent = 'Approve';
    elements.approvalDenyBtn.textContent = 'Deny';
  }
};

// ===== Settings modal =====
let modalEffortSelectionDirty = false;

const openAuthModal = (errorText = '', required = !state.token) => {
  modalEffortSelectionDirty = false;
  state.authRequired = required;
  elements.authError.textContent = errorText;
  elements.authTokenInput.value = state.token || '';
  elements.authCancelBtn.style.display = required ? 'none' : 'inline-flex';
  elements.providerSelect.value = state.selectedProvider;
  elements.modelSelect.value = state.selectedModel;
  if (elements.effortSelect) {
    elements.effortSelect.value = state.selectedEffort;
  }
  if (elements.reasoningModeSelect) {
    elements.reasoningModeSelect.value = state.selectedReasoningMode || 'standard';
    const info = modelMetadataFor(state.selectedModel);
    elements.reasoningModeField.hidden = !Array.isArray(info?.reasoning_modes) || !info.reasoning_modes.includes('pro');
  }
  if (elements.showHiddenSessionsInput) {
    elements.showHiddenSessionsInput.checked = state.showHiddenSessions;
  }
  if (elements.showWidgetsSidebarInput) {
    elements.showWidgetsSidebarInput.checked = state.showWidgetsSidebar !== false;
  }
  app.refreshNotificationUI();
  elements.authModal.classList.remove('hidden');
  elements.providerSelect.removeAttribute('tabindex');
  elements.modelSelect.removeAttribute('tabindex');
  elements.effortSelect?.removeAttribute('tabindex');
  elements.reasoningModeSelect?.removeAttribute('tabindex');
  elements.authTokenInput.removeAttribute('tabindex');
  elements.showHiddenSessionsInput?.removeAttribute('tabindex');
  elements.showWidgetsSidebarInput?.removeAttribute('tabindex');

  setTimeout(() => {
    if (required) {
      elements.authTokenInput.focus();
      elements.authTokenInput.select();
    }
  }, 0);
};

const closeAuthModal = () => {
  if (state.authRequired && !state.token) return;
  modalEffortSelectionDirty = false;
  elements.authModal.classList.add('hidden');
  elements.authError.textContent = '';
  elements.providerSelect.setAttribute('tabindex', '-1');
  elements.modelSelect.setAttribute('tabindex', '-1');
  elements.effortSelect?.setAttribute('tabindex', '-1');
  elements.reasoningModeSelect?.setAttribute('tabindex', '-1');
  elements.authTokenInput.setAttribute('tabindex', '-1');
  elements.showHiddenSessionsInput?.setAttribute('tabindex', '-1');
  elements.showWidgetsSidebarInput?.setAttribute('tabindex', '-1');
};

const handleAuthFailure = () => {
  app.stopSessionStatePoll();
  closeAskUserModal();
  state.token = '';
  app.setApplicationConnected?.(false, false);
  localStorage.removeItem(STORAGE_KEYS.token);
  syncTokenCookie('');
  setConnectionState('Not connected', 'bad');
  openAuthModal('Auth failed — check your token.', true);
};

const connectToken = async () => {
  const token = elements.authTokenInput.value.trim();
  const nextShowHiddenSessions = Boolean(elements.showHiddenSessionsInput?.checked);
  const nextShowWidgetsSidebar = elements.showWidgetsSidebarInput ? Boolean(elements.showWidgetsSidebarInput.checked) : true;

  // Provider/model selections are committed live via the change handlers.
  // Re-reading the modal DOM here can clobber a valid in-memory choice if the
  // selects are temporarily stale (for example while startup/model refresh work
  // is still settling). Persist the current state instead.
  const persistedProvider = state.selectedProvider;
  const persistedModel = state.selectedModel;
  const newEffort = elements.effortSelect ? elements.effortSelect.value : '';
  const newReasoningMode = elements.reasoningModeSelect ? elements.reasoningModeSelect.value : 'standard';
  state.selectedProvider = persistedProvider;
  state.selectedModel = persistedModel;
  state.selectedEffort = newEffort;
  state.selectedReasoningMode = newReasoningMode === 'pro' ? 'pro' : 'standard';
  localStorage.setItem(STORAGE_KEYS.selectedReasoningMode, state.selectedReasoningMode);
  canonicalizeSelectedModelEffort();
  if (modalEffortSelectionDirty) {
    markRuntimeSelectionIntent();
  }
  persistRuntimeSelection();
  const showHiddenChanged = nextShowHiddenSessions !== state.showHiddenSessions;
  state.showHiddenSessions = nextShowHiddenSessions;
  localStorage.setItem(STORAGE_KEYS.showHiddenSessions, state.showHiddenSessions ? '1' : '0');
  const showWidgetsChanged = nextShowWidgetsSidebar !== (state.showWidgetsSidebar !== false);
  state.showWidgetsSidebar = nextShowWidgetsSidebar;
  localStorage.setItem(STORAGE_KEYS.showWidgetsSidebar, state.showWidgetsSidebar ? '1' : '0');
  if (showWidgetsChanged && app.renderWidgetSidebar) app.renderWidgetSidebar();
  app.updateHeader();

  if (state.authRequired && !token) {
    elements.authError.textContent = 'Token is required.';
    return;
  }

  const tokenChanged = token !== state.token;
  if (!tokenChanged) {
    renderEffortOptions();
    if (showHiddenChanged && state.connected) {
      void app.mergeServerSessions({ includeArchived: state.showHiddenSessions }).then(() => {
        renderSidebar();
      });
    } else {
      renderSidebar();
    }
    closeAuthModal();
    return;
  }

  elements.authConnectBtn.disabled = true;
  elements.authConnectBtn.textContent = 'Saving…';
  elements.authError.textContent = '';

  try {
    // Speculative models fetch in parallel with providers — same pattern as startup.
    const speculativeProvider = state.selectedProvider;
    const speculativeModelsPromise = speculativeProvider
      ? fetchModels(token, speculativeProvider)
      : null;

    state.providers = await fetchProviders(token);
    normalizeSelectedProvider();

    let models;
    if (speculativeModelsPromise !== null && state.selectedProvider === speculativeProvider) {
      models = await speculativeModelsPromise;
    } else {
      if (speculativeModelsPromise !== null) speculativeModelsPromise.catch(() => {});
      models = await fetchModels(token, state.selectedProvider);
    }
    state.token = token;
    state.models = models;
    app.setApplicationConnected?.(true, true);
    if (!app.setApplicationConnected) state.connected = true;
    localStorage.setItem(STORAGE_KEYS.token, token);
    syncTokenCookie(token);

    renderProviderOptions();
    renderModelOptions();
    setConnectionState('', '');
    state.authRequired = false;
    closeAuthModal();
    if (showHiddenChanged) {
      void app.mergeServerSessions({ includeArchived: state.showHiddenSessions }).then(() => {
        renderSidebar();
      });
    }

    // Retry push enrollment now that we have a valid token. Also recover if the
    // browser permission was already granted but the old client-side flag was missing.
    if (shouldAutoSubscribeToPush()) {
      subscribeToPush();
    }

    const active = getActiveSession();
    if (active) {
      await app.syncActiveSessionFromServer(active, true);
    }
  } catch (err) {
    const message = err?.message || 'Unable to validate token.';
    elements.authError.textContent = message;
    if (err?.status === 401) {
      state.token = '';
      app.setApplicationConnected?.(false, false);
      if (!app.setApplicationConnected) state.connected = false;
      localStorage.removeItem(STORAGE_KEYS.token);
      syncTokenCookie('');
    }
    setConnectionState('Not connected', 'bad');
  } finally {
    elements.authConnectBtn.disabled = false;
    elements.authConnectBtn.textContent = 'Save';
  }
};

elements.effortSelect?.addEventListener('change', () => {
  modalEffortSelectionDirty = true;
});

Object.assign(app, {
  closeAskUserModal, openApprovalModal, closeApprovalModal, submitApprovalModal,
  askUserSummaryFromAnswers, collectAskUserAnswers, validateSingleQuestion,
  switchAskUserTab, renderAskUserModal, openAskUserModal, submitAskUserModal,
  openAuthModal, closeAuthModal, handleAuthFailure, connectToken,
});
})();
