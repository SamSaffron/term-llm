(() => {
'use strict';

const app = window.TermLLMApp;
const {
  STORAGE_KEYS, state, elements, generateId, sanitizeInterruptState, sanitizeMessage, syncTokenCookie, truncate, saveSessions,
  getActiveSession, ensureActiveSession, createSession, findMessageElement, scrollToBottom, setConnectionState,
  persistAndRefreshShell, updateSessionUsageDisplay, refreshRelativeTimes, requestHeaders: _unusedRequestHeaders, updateAssistantNode, updateUserNode,
  updateToolNode, updateToolGroupNode, createMessageNode, createToolGroupNode, renderSidebar, renderMessages, maybeNotifyResponseComplete
} = app;

// ===== Network helpers =====
const requestHeaders = (sessionId, tokenOverride = '') => {
  const headers = {
    'Content-Type': 'application/json'
  };

  const token = tokenOverride || state.token;
  if (token) {
    headers.Authorization = `Bearer ${token}`;
  }
  if (sessionId) {
    headers.session_id = sessionId;
  }

  return headers;
};

const normalizeError = async (response) => {
  let message = `Request failed (${response.status}).`;
  let parsed;

  try {
    parsed = await response.json();
  } catch {
    parsed = null;
  }

  if (response.status === 401) {
    message = 'Auth failed — check your token.';
  } else if (response.status === 429) {
    message = 'Rate limited. Try again shortly.';
  } else if (parsed?.error?.message) {
    message = parsed.error.message;
  }

  return { status: response.status, message };
};

const fetchModels = async (tokenOverride = '') => {
  const headers = {};
  const token = tokenOverride || state.token;
  if (token) headers.Authorization = `Bearer ${token}`;

  const response = await fetch('/v1/models', { headers });
  if (!response.ok) {
    throw await normalizeError(response);
  }

  const data = await response.json().catch(() => ({ data: [] }));
  return Array.isArray(data.data)
    ? data.data.map((m) => m?.id).filter(Boolean)
    : [];
};

const parseSSEStream = async (stream, onEvent) => {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  let buffer = '';

  const processBlock = async (block) => {
    if (!block.trim()) return true;

    let eventName = '';
    const dataLines = [];
    const lines = block.split('\n');

    for (const line of lines) {
      if (line.startsWith('event:')) {
        eventName = line.slice(6).trim();
      } else if (line.startsWith('data:')) {
        dataLines.push(line.slice(5).trimStart());
      }
    }

    return onEvent(eventName, dataLines.join('\n'));
  };

  while (true) {
    const { value, done } = await reader.read();
    if (done) break;

    buffer += decoder.decode(value, { stream: true }).replace(/\r/g, '');

    let idx;
    while ((idx = buffer.indexOf('\n\n')) !== -1) {
      const block = buffer.slice(0, idx);
      buffer = buffer.slice(idx + 2);
      const keepGoing = await processBlock(block);
      if (keepGoing === false) {
        reader.cancel().catch(() => {});
        return;
      }
    }
  }

  if (buffer.trim()) {
    await processBlock(buffer);
  }
};

const sleep = (ms) => new Promise((resolve) => window.setTimeout(resolve, ms));

const setActiveResponseTracking = (session, responseId, sequenceNumber = null) => {
  if (!session) return;
  const normalized = String(responseId || '').trim();
  if (!normalized) return;

  if (session.activeResponseId !== normalized) {
    session.activeResponseId = normalized;
    if (sequenceNumber === null) {
      session.lastSequenceNumber = 0;
    }
  }

  if (sequenceNumber !== null) {
    const nextSeq = Number(sequenceNumber);
    if (Number.isFinite(nextSeq) && nextSeq >= 0) {
      session.lastSequenceNumber = nextSeq;
    }
  }

  state.currentStreamResponseId = normalized;
};

const clearActiveResponseTracking = (session, responseId = '') => {
  if (!session) return;
  const currentId = String(session.activeResponseId || '').trim();
  const targetId = String(responseId || '').trim();

  if (!targetId || currentId === targetId) {
    session.activeResponseId = null;
    session.lastSequenceNumber = 0;
  }
  if (!targetId || state.currentStreamResponseId === targetId) {
    state.currentStreamResponseId = '';
  }
};

const updateResponseSequence = (session, payload) => {
  if (!session || !payload || typeof payload !== 'object') return;
  const seq = Number(payload.sequence_number);
  if (!Number.isFinite(seq) || seq <= 0) return;
  const current = Number(session.lastSequenceNumber || 0);
  session.lastSequenceNumber = Math.max(current, seq);
};

const createResponseStreamState = (session) => {
  let currentToolGroup = [...session.messages].reverse().find((message) => (
    message.role === 'tool-group' && message.status === 'running'
  )) || null;
  let currentAssistantMessage = null;

  if (!currentToolGroup) {
    const lastMessage = session.messages[session.messages.length - 1];
    if (lastMessage?.role === 'assistant') {
      currentAssistantMessage = lastMessage;
    }
  }

  const ensureAssistantMessage = () => {
    if (currentAssistantMessage) return currentAssistantMessage;
    const msg = {
      id: generateId('msg'),
      role: 'assistant',
      content: '',
      created: Date.now()
    };
    session.messages.push(msg);
    elements.messages.appendChild(createMessageNode(msg));
    currentAssistantMessage = msg;
    return msg;
  };

  const closeToolGroup = () => {
    if (!currentToolGroup) return;
    currentToolGroup.tools.forEach((tool) => { tool.status = 'done'; });
    currentToolGroup.status = 'done';
    updateToolGroupNode(currentToolGroup);
    currentToolGroup = null;
  };

  return {
    ensureAssistantMessage,
    closeToolGroup,
    get currentToolGroup() {
      return currentToolGroup;
    },
    set currentToolGroup(value) {
      currentToolGroup = value;
    },
    set currentAssistantMessage(value) {
      currentAssistantMessage = value;
    }
  };
};

const applyResponseStreamEvent = (session, streamState, event, payload) => {
  updateResponseSequence(session, payload);

  if (event === 'response.created') {
    const responseId = String(payload?.response?.id || '').trim();
    if (responseId) {
      setActiveResponseTracking(session, responseId, payload?.sequence_number ?? null);
      saveSessions();
    }
    const model = payload?.response?.model;
    if (model) {
      session.activeModel = model;
      updateSessionUsageDisplay(session);
    }
    return { terminal: false };
  }

  if (event === 'response.output_text.delta') {
    const delta = String(payload.delta || '');
    if (delta) {
      streamState.closeToolGroup();
      const msg = streamState.ensureAssistantMessage();
      msg.content += delta;
      updateAssistantNode(msg);
      saveSessions();
      scrollToBottom();
    }
    return { terminal: false };
  }

  if (event === 'response.output_text.new_segment') {
    streamState.closeToolGroup();
    streamState.currentAssistantMessage = null;
    streamState.ensureAssistantMessage();
    saveSessions();
    scrollToBottom();
    return { terminal: false };
  }

  if (event === 'response.output_item.added') {
    const item = payload.item;
    if (item?.type === 'function_call') {
      const toolEntry = {
        id: item.call_id || generateId('tool'),
        name: String(item.name || 'tool'),
        arguments: String(item.arguments || ''),
        status: 'running',
        created: Date.now()
      };

      if (!streamState.currentToolGroup) {
        streamState.currentToolGroup = {
          id: generateId('msg'),
          role: 'tool-group',
          tools: [toolEntry],
          expanded: false,
          status: 'running',
          created: Date.now()
        };
        session.messages.push(streamState.currentToolGroup);
        elements.messages.appendChild(createToolGroupNode(streamState.currentToolGroup));
      } else {
        streamState.currentToolGroup.tools.push(toolEntry);
        updateToolGroupNode(streamState.currentToolGroup);
      }

      streamState.currentAssistantMessage = null;
      saveSessions();
      scrollToBottom();
    }
    return { terminal: false };
  }

  if (event === 'response.output_item.done') {
    const item = payload.item;
    if (item?.type === 'function_call' && streamState.currentToolGroup) {
      const callId = item.call_id || item.id;
      const entry = callId
        ? streamState.currentToolGroup.tools.find((tool) => tool.id === callId)
        : streamState.currentToolGroup.tools.find((tool) => tool.name === String(item.name || '') && tool.status === 'running');
      if (entry) {
        entry.arguments = String(item.arguments || entry.arguments || '');
      }
      updateToolGroupNode(streamState.currentToolGroup);
      saveSessions();
    }
    return { terminal: false };
  }

  if (event === 'response.ask_user.prompt') {
    const callId = String(payload.call_id || '').trim();
    const questions = Array.isArray(payload.questions) ? payload.questions : [];
    if (callId && questions.length > 0) {
      openAskUserModal(session.id, callId, questions);
    }
    return { terminal: false };
  }

  if (event === 'response.approval.prompt') {
    const approvalId = String(payload.approval_id || '').trim();
    const options = Array.isArray(payload.options) ? payload.options : [];
    if (approvalId && options.length > 0) {
      openApprovalModal(session.id, approvalId, payload.path, payload.is_shell, payload.title, options);
    }
    return { terminal: false };
  }

  if (event === 'response.tool_exec.end') {
    if (streamState.currentToolGroup) {
      const callId = payload.call_id;
      const entry = streamState.currentToolGroup.tools.find((tool) => tool.id === callId);
      if (entry) {
        entry.status = 'done';
        updateToolGroupNode(streamState.currentToolGroup);
      }
      if (streamState.currentToolGroup.tools.every((tool) => tool.status === 'done')) {
        streamState.currentToolGroup.status = 'done';
        updateToolGroupNode(streamState.currentToolGroup);
      }
    }
    if (payload.images && payload.images.length > 0) {
      const msg = streamState.ensureAssistantMessage();
      payload.images.forEach((url) => {
        msg.content += `\n\n![Generated Image](${url})\n`;
      });
      updateAssistantNode(msg);
    }
    saveSessions();
    scrollToBottom();
    return { terminal: false };
  }

  if (event === 'response.interjection') {
    const interjectionText = String(payload.text || '').trim();
    if (interjectionText) {
      resolvePendingInterruptCommit(session.id, interjectionText);
      saveSessions();
    }
    return { terminal: false };
  }

  if (event === 'response.completed') {
    const usage = payload?.response?.usage;
    streamState.closeToolGroup();
    markToolGroupsDone(session);

    const responseId = String(payload?.response?.id || session.activeResponseId || state.currentStreamResponseId || '').trim();
    if (responseId) {
      session.lastResponseId = responseId;
    }
    clearActiveResponseTracking(session, responseId);

    const sessionUsage = payload?.response?.session_usage;
    if (sessionUsage) session.sessionUsage = sessionUsage;
    if (usage) session.lastUsage = usage;
    const completedModel = payload?.response?.model;
    if (completedModel) session.activeModel = completedModel;
    updateSessionUsageDisplay(session);

    const lastAssistant = [...session.messages].reverse().find((message) => message.role === 'assistant');
    if (lastAssistant) {
      if (usage) lastAssistant.usage = usage;
      updateAssistantNode(lastAssistant);
    }
    saveSessions();
    void maybeNotifyResponseComplete(session, lastAssistant, responseId);
    scrollToBottom();
    return { terminal: true };
  }

  if (event === 'response.failed') {
    const errorMessage = payload?.error?.message || 'The response failed.';
    const lowered = errorMessage.toLowerCase();
    const canceledByInterrupt = state.expectCanceledRun && (
      lowered.includes('context canceled') ||
      lowered.includes('context cancelled') ||
      lowered.includes('cancelled') ||
      lowered.includes('canceled')
    );

    if (!canceledByInterrupt) {
      addErrorMessage(errorMessage, session);
    }
    state.expectCanceledRun = false;

    streamState.closeToolGroup();
    markToolGroupsDone(session);
    clearActiveResponseTracking(session, session.activeResponseId || state.currentStreamResponseId);

    const lastAssistant = [...session.messages].reverse().find((message) => message.role === 'assistant');
    if (lastAssistant) updateAssistantNode(lastAssistant);
    saveSessions();
    scrollToBottom(true);
    return { terminal: true };
  }

  return { terminal: false };
};

const consumeResponseStream = async (stream, session, streamState) => {
  let sawTerminal = false;
  let sawDone = false;

  await parseSSEStream(stream, async (event, data) => {
    if (data === '[DONE]') {
      sawDone = true;
      streamState.closeToolGroup();
      markToolGroupsDone(session);
      persistAndRefreshShell();
      return false;
    }

    if (!data) return true;

    let payload;
    try {
      payload = JSON.parse(data);
    } catch {
      return true;
    }

    const result = applyResponseStreamEvent(session, streamState, event, payload);
    if (result?.terminal) {
      sawTerminal = true;
    }
    return true;
  });

  return sawTerminal || sawDone || !session.activeResponseId;
};

const fetchResponseSnapshot = async (session, responseId) => {
  const response = await fetch(`/v1/responses/${encodeURIComponent(responseId)}`, {
    headers: requestHeaders(session?.id || '')
  });
  if (!response.ok) {
    throw await normalizeError(response);
  }
  return response.json().catch(() => ({}));
};

const applyResponseRecoverySnapshot = (session, payload) => {
  if (!session || !payload || typeof payload !== 'object') return false;

  const recovery = payload.recovery;
  if (!recovery || typeof recovery !== 'object') return false;

  const rawMessages = Array.isArray(recovery.messages) ? recovery.messages : [];
  const recoveredMessages = rawMessages
    .map((message) => sanitizeMessage(message))
    .filter(Boolean);

  let anchorIndex = -1;
  for (let i = session.messages.length - 1; i >= 0; i -= 1) {
    if (session.messages[i]?.role === 'user') {
      anchorIndex = i;
      break;
    }
  }

  const preserved = anchorIndex >= 0
    ? session.messages.slice(0, anchorIndex + 1)
    : [];
  session.messages = preserved.concat(recoveredMessages);

  const nextSeq = Number(payload.last_sequence_number ?? recovery.sequence_number ?? session.lastSequenceNumber ?? 0);
  if (Number.isFinite(nextSeq) && nextSeq >= 0) {
    session.lastSequenceNumber = nextSeq;
  }

  const responseId = String(payload.id || session.activeResponseId || state.currentStreamResponseId || '').trim();
  const sessionUsage = payload.session_usage;
  if (sessionUsage) {
    session.sessionUsage = sessionUsage;
  }
  updateSessionUsageDisplay(session);

  if (payload.status === 'completed') {
    if (responseId) {
      session.lastResponseId = responseId;
    }
    clearActiveResponseTracking(session, responseId);
  } else if (payload.status === 'failed') {
    clearActiveResponseTracking(session, responseId);
  } else if (responseId) {
    setActiveResponseTracking(session, responseId, session.lastSequenceNumber);
  }

  saveSessions();
  if (session.id === state.activeSessionId) {
    renderMessages(true);
  } else {
    persistAndRefreshShell();
  }
  return true;
};

const resumeActiveResponse = async (session, options = {}) => {
  if (!session) return false;

  const responseId = String(options.responseId || session.activeResponseId || '').trim();
  if (!responseId) return false;

  if (session.activeResponseId !== responseId) {
    setActiveResponseTracking(session, responseId, 0);
    saveSessions();
  } else {
    state.currentStreamResponseId = responseId;
  }

  let streamState = options.streamState || createResponseStreamState(session);
  const maxAttempts = Number.isFinite(Number(options.maxAttempts)) ? Number(options.maxAttempts) : 8;

  for (let attempt = 0; attempt < maxAttempts; attempt += 1) {
    if (session.activeResponseId !== responseId) {
      setStreaming(Boolean(state.currentStreamResponseId));
      return true;
    }

    const controller = new AbortController();
    state.abortController = controller;
    state.currentStreamResponseId = responseId;
    setStreaming(true);

    try {
      const response = await fetch(`/v1/responses/${encodeURIComponent(responseId)}/events?after=${encodeURIComponent(session.lastSequenceNumber || 0)}`, {
        headers: requestHeaders(session.id),
        signal: controller.signal
      });
      if (!response.ok) {
        throw await normalizeError(response);
      }
      if (!response.body) {
        throw { status: 0, message: 'No response body from server.' };
      }

      setConnectionState('', '');
      const terminal = await consumeResponseStream(response.body, session, streamState);
      state.abortController = null;

      if (session.activeResponseId !== responseId) {
        setStreaming(Boolean(state.currentStreamResponseId));
        return true;
      }
      if (terminal) {
        setStreaming(Boolean(state.currentStreamResponseId));
        return true;
      }
    } catch (err) {
      state.abortController = null;

      if (err?.name === 'AbortError') {
        setStreaming(Boolean(state.currentStreamResponseId));
        return false;
      }
      if (err?.status === 401) {
        handleAuthFailure();
        return false;
      }
      if (err?.status === 409) {
        try {
          const snapshot = await fetchResponseSnapshot(session, responseId);
          applyResponseRecoverySnapshot(session, snapshot);
          streamState = createResponseStreamState(session);
          if (snapshot?.status !== 'in_progress') {
            setStreaming(Boolean(state.currentStreamResponseId));
            return true;
          }
          continue;
        } catch (snapshotErr) {
          if (snapshotErr?.status === 401) {
            handleAuthFailure();
            return false;
          }
          if (snapshotErr?.status === 404) {
            clearActiveResponseTracking(session, responseId);
            saveSessions();
            await app.syncActiveSessionFromServer(session, false);
            setStreaming(Boolean(state.currentStreamResponseId));
            return false;
          }
        }
      }
      if (err?.status === 404) {
        clearActiveResponseTracking(session, responseId);
        saveSessions();
        await app.syncActiveSessionFromServer(session, false);
        setStreaming(Boolean(state.currentStreamResponseId));
        return false;
      }
      if (session.activeResponseId !== responseId) {
        setStreaming(Boolean(state.currentStreamResponseId));
        return true;
      }
    }

    setConnectionState('Reconnecting…');
    await sleep(Math.min(400 * (attempt + 1), 2400));
  }

  if (session.activeResponseId === responseId) {
    state.abortController = null;
    state.currentStreamResponseId = responseId;
    setStreaming(true);
    app.scheduleSessionStatePoll(session.id, 2000);
    setConnectionState('Stream disconnected', 'bad');
  }
  return false;
};

const cancelActiveResponse = async (session) => {
  const responseId = String(session?.activeResponseId || state.currentStreamResponseId || '').trim();
  if (!responseId) {
    if (state.abortController) {
      state.abortController.abort();
    }
    return;
  }

  state.expectCanceledRun = true;
  const response = await fetch(`/v1/responses/${encodeURIComponent(responseId)}/cancel`, {
    method: 'POST',
    headers: requestHeaders(session?.id || '')
  });
  if (!response.ok && response.status !== 404 && response.status !== 409) {
    throw await normalizeError(response);
  }

  if (!state.abortController && session?.id) {
    app.scheduleSessionStatePoll(session.id, 250);
  }
};

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
    const response = await fetch(`/v1/sessions/${encodeURIComponent(prompt.sessionId)}/ask_user`, {
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
            askUser: true
          };
          session.messages.push(message);
          if (session.id === state.activeSessionId) {
            const empty = elements.messages.querySelector('.empty-state');
            if (empty) empty.remove();
            elements.messages.appendChild(createMessageNode(message));
          }
          saveSessions();
          scrollToBottom(true);
        }
      }
    }
    closeAskUserModal();
    if (!state.abortController) {
      setStreaming(true);
      app.scheduleSessionStatePoll(prompt.sessionId, 400);
    }
  } catch (err) {
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
    const response = await fetch(`/v1/sessions/${encodeURIComponent(prompt.sessionId)}/approval`, {
      method: 'POST',
      headers: requestHeaders(prompt.sessionId),
      body: JSON.stringify(body)
    });
    if (!response.ok) {
      throw await normalizeError(response);
    }
    closeApprovalModal();
    if (!state.abortController) {
      setStreaming(true);
      app.scheduleSessionStatePoll(prompt.sessionId, 400);
    }
  } catch (err) {
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
const openAuthModal = (errorText = '', required = !state.token) => {
  state.authRequired = required;
  elements.authError.textContent = errorText;
  elements.authTokenInput.value = state.token || '';
  elements.authCancelBtn.style.display = required ? 'none' : 'inline-flex';
  elements.modelSelect.value = state.selectedModel;
  app.refreshNotificationUI();
  elements.authModal.classList.remove('hidden');

  setTimeout(() => {
    if (required) {
      elements.authTokenInput.focus();
      elements.authTokenInput.select();
    }
  }, 0);
};

const closeAuthModal = () => {
  if (state.authRequired && !state.token) return;
  elements.authModal.classList.add('hidden');
  elements.authError.textContent = '';
};

const handleAuthFailure = () => {
  app.stopSessionStatePoll();
  closeAskUserModal();
  state.token = '';
  localStorage.removeItem(STORAGE_KEYS.token);
  syncTokenCookie('');
  setConnectionState('Not connected', 'bad');
  openAuthModal('Auth failed — check your token.', true);
};

const connectToken = async () => {
  const token = elements.authTokenInput.value.trim();

  // Save model selection regardless of token
  const newModel = elements.modelSelect.value;
  state.selectedModel = newModel;
  if (newModel) {
    localStorage.setItem(STORAGE_KEYS.selectedModel, newModel);
  } else {
    localStorage.removeItem(STORAGE_KEYS.selectedModel);
  }

  if (state.authRequired && !token) {
    elements.authError.textContent = 'Token is required.';
    return;
  }

  const tokenChanged = token !== state.token;
  if (!tokenChanged) {
    closeAuthModal();
    return;
  }

  elements.authConnectBtn.disabled = true;
  elements.authConnectBtn.textContent = 'Saving…';
  elements.authError.textContent = '';

  try {
    const models = await fetchModels(token);
    state.token = token;
    state.models = models;
    state.connected = true;
    localStorage.setItem(STORAGE_KEYS.token, token);
    syncTokenCookie(token);

    renderModelOptions();
    setConnectionState('', '');
    state.authRequired = false;
    closeAuthModal();
    const active = getActiveSession();
    if (active) {
      await app.syncActiveSessionFromServer(active, true);
    }
  } catch (err) {
    const message = err?.message || 'Unable to validate token.';
    elements.authError.textContent = message;
    if (err?.status === 401) {
      state.token = '';
      localStorage.removeItem(STORAGE_KEYS.token);
      syncTokenCookie('');
    }
    setConnectionState('Not connected', 'bad');
  } finally {
    elements.authConnectBtn.disabled = false;
    elements.authConnectBtn.textContent = 'Save';
  }
};

// ===== Model picker =====
const renderModelOptions = () => {
  const previous = state.selectedModel;
  elements.modelSelect.innerHTML = '';

  const autoOption = document.createElement('option');
  autoOption.value = '';
  autoOption.textContent = 'Auto (server default)';
  elements.modelSelect.appendChild(autoOption);

  state.models.forEach((id) => {
    const option = document.createElement('option');
    option.value = id;
    option.textContent = id;
    elements.modelSelect.appendChild(option);
  });

  if (previous && !state.models.includes(previous)) {
    const custom = document.createElement('option');
    custom.value = previous;
    custom.textContent = `${previous} (custom)`;
    elements.modelSelect.appendChild(custom);
  }

  elements.modelSelect.value = previous;
};

// ===== Composer logic =====
const autoGrowPrompt = () => {
  const el = elements.promptInput;
  el.style.height = 'auto';
  const next = Math.min(el.scrollHeight, 200);
  el.style.height = `${Math.max(48, next)}px`;
  el.style.overflowY = el.scrollHeight > 200 ? 'auto' : 'hidden';
};

// ===== File attachment =====
const renderAttachments = () => {
  const strip = elements.attachmentsStrip;
  strip.innerHTML = '';
  if (state.attachments.length === 0) {
    strip.style.display = 'none';
    return;
  }
  strip.style.display = 'flex';
  state.attachments.forEach((att) => {
    const chip = document.createElement('div');
    chip.className = 'attachment-chip';

    if (att.type.startsWith('image/')) {
      const img = document.createElement('img');
      img.src = att.dataURL;
      img.alt = att.name;
      chip.appendChild(img);
    }

    const name = document.createElement('span');
    name.className = 'att-name';
    name.textContent = att.name;
    name.title = `${att.name} (${(att.size / 1024).toFixed(1)} KB)`;
    chip.appendChild(name);

    const remove = document.createElement('button');
    remove.className = 'att-remove';
    remove.textContent = '\u00d7';
    remove.title = 'Remove';
    remove.addEventListener('click', () => {
      state.attachments = state.attachments.filter(a => a.id !== att.id);
      renderAttachments();
    });
    chip.appendChild(remove);

    strip.appendChild(chip);
  });
};

const MAX_ATTACHMENTS = 10;
const MAX_FILE_BYTES = 20 * 1024 * 1024; // 20 MB

const handleFiles = (fileList) => {
  const files = Array.from(fileList);
  for (const file of files) {
    if (state.attachments.length >= MAX_ATTACHMENTS) {
      alert(`Maximum ${MAX_ATTACHMENTS} attachments allowed.`);
      return;
    }
    if (file.size > MAX_FILE_BYTES) {
      alert(`${file.name} exceeds the 20 MB file size limit.`);
      continue;
    }
    const reader = new FileReader();
    reader.onload = () => {
      if (state.attachments.length >= MAX_ATTACHMENTS) return;
      const dataURL = reader.result;
      state.attachments.push({
        id: generateId('att'),
        name: file.name,
        type: file.type || 'application/octet-stream',
        size: file.size,
        dataURL
      });
      renderAttachments();
    };
    reader.readAsDataURL(file);
  }
};

const setStreaming = (streaming) => {
  state.streaming = streaming;
  elements.promptInput.disabled = false;
  elements.sendBtn.disabled = false;
  elements.sendBtn.classList.toggle('loading', streaming);
  elements.stopBtn.classList.toggle('visible', streaming && (Boolean(state.abortController) || Boolean(state.currentStreamResponseId)));
  if (!streaming) {
    elements.promptInput.focus();
  }
};

const queueInterruptFollowUp = (prompt, messageId) => {
  state.queuedInterrupts.push({ prompt, messageId });
};

const trackPendingInterruptCommit = (sessionId, prompt, messageId) => {
  state.pendingInterruptCommits = state.pendingInterruptCommits.filter(entry => entry.messageId !== messageId);
  state.pendingInterruptCommits.push({ sessionId, prompt, messageId });
};

const resolvePendingInterruptCommit = (sessionId, prompt) => {
  const idx = state.pendingInterruptCommits.findIndex(entry => entry.sessionId === sessionId && entry.prompt === prompt);
  if (idx < 0) return null;
  const [entry] = state.pendingInterruptCommits.splice(idx, 1);
  return entry;
};

const discardPendingInterruptCommit = (messageId) => {
  if (!messageId) return;
  state.pendingInterruptCommits = state.pendingInterruptCommits.filter(entry => entry.messageId !== messageId);
};

const requeueUncommittedInterrupts = (session) => {
  if (!session?.id) return;
  const remaining = [];
  for (const entry of state.pendingInterruptCommits) {
    if (entry.sessionId !== session.id) {
      remaining.push(entry);
      continue;
    }
    queueInterruptFollowUp(entry.prompt, entry.messageId);
  }
  state.pendingInterruptCommits = remaining;
};

const setInterruptMessageState = (session, messageId, interruptState) => {
  if (!messageId) return;
  const normalized = sanitizeInterruptState(interruptState);
  if (!normalized) return;
  const message = session.messages.find(m => m.id === messageId && m.role === 'user');
  if (!message) return;
  message.interruptState = normalized;
  updateUserNode(message);
};

const createPendingInterruptMessage = (session, prompt) => {
  const message = {
    id: generateId('msg'),
    role: 'user',
    content: prompt,
    created: Date.now(),
    interruptState: 'evaluating'
  };
  session.messages.push(message);

  const emptyState = elements.messages.querySelector('.empty-state');
  if (emptyState) emptyState.remove();
  elements.messages.appendChild(createMessageNode(message));
  return message;
};

const interruptActiveRun = async (session, prompt, messageId) => {
  const response = await fetch(`/v1/sessions/${encodeURIComponent(session.id)}/interrupt`, {
    method: 'POST',
    headers: requestHeaders(session.id),
    body: JSON.stringify({ message: prompt })
  });
  if (!response.ok) {
    throw await normalizeError(response);
  }

  const payload = await response.json();
  const actionRaw = String(payload.action || 'queue').toLowerCase();
  const action = (actionRaw === 'interject' || actionRaw === 'cancel' || actionRaw === 'queue')
    ? actionRaw
    : 'queue';

  setInterruptMessageState(session, messageId, action);

  if (action === 'interject') {
    trackPendingInterruptCommit(session.id, prompt, messageId);
  } else {
    discardPendingInterruptCommit(messageId);
  }

  if (action === 'cancel' || action === 'queue') {
    queueInterruptFollowUp(prompt, messageId);
  }
  if (action === 'cancel') {
    state.expectCanceledRun = true;
  }

  saveSessions();
  scrollToBottom(true);
  return action;
};

const addErrorMessage = (text, session) => {
  const message = {
    id: generateId('msg'),
    role: 'error',
    content: text,
    created: Date.now()
  };
  session.messages.push(message);
  elements.messages.appendChild(createMessageNode(message));
};

const markToolGroupsDone = (session) => {
  session.messages.forEach(m => {
    if (m.role === 'tool-group' && m.status === 'running') {
      m.tools.forEach(t => { t.status = 'done'; });
      m.status = 'done';
      updateToolGroupNode(m);
    }
    if (m.role === 'tool' && m.status === 'running') {
      m.status = 'done';
      updateToolNode(m);
    }
  });
};

// Rebuild a full conversation input array from locally-stored session messages.
// Used to recover when previous_response_id has expired server-side.
const rebuildInputFromSession = (session, currentInput) => {
  const input = [];
  for (const msg of session.messages) {
    if (msg.role === 'user' && !msg.askUser) {
      if (msg.attachments && msg.attachments.length > 0) {
        const parts = [];
        for (const att of msg.attachments) {
          if (att.type && att.type.startsWith('image/') && att.dataURL) {
            parts.push({ type: 'input_image', image_url: att.dataURL, filename: att.name });
          } else if (att.dataURL) {
            parts.push({ type: 'input_file', file_data: att.dataURL, filename: att.name });
          }
        }
        if (msg.content) parts.push({ type: 'input_text', text: msg.content });
        input.push({ type: 'message', role: 'user', content: parts });
      } else {
        input.push({ type: 'message', role: 'user', content: msg.content || '' });
      }
    } else if (msg.role === 'assistant') {
      input.push({ type: 'message', role: 'assistant', content: msg.content || '' });
    }
    // Skip tool/tool-group/error messages — they're internal
  }
  // Replace the last user message input with the current one (which may have
  // attachments encoded differently), or append if not already present.
  if (input.length > 0 && input[input.length - 1].role === 'user') {
    input[input.length - 1].content = currentInput;
  } else {
    input.push({ type: 'message', role: 'user', content: currentInput });
  }
  return input;
};

const sendMessage = async (options = {}) => {
  const promptSource = typeof options.prompt === 'string' ? options.prompt : elements.promptInput.value;
  const prompt = String(promptSource || '').trim();
  const pendingAttachments = Array.isArray(options.attachments)
    ? [...options.attachments]
    : [...state.attachments];

  if (!prompt && pendingAttachments.length === 0) return;

  if (!state.connected) {
    openAuthModal('Connect before sending a message.', true);
    return;
  }

  const session = ensureActiveSession();
  if (state.streaming) {
    if (pendingAttachments.length > 0) {
      alert('Attachments are not supported while a run is active.');
      return;
    }

    const pendingMessage = createPendingInterruptMessage(session, prompt);
    persistAndRefreshShell();
    scrollToBottom(true);

    elements.promptInput.value = '';
    autoGrowPrompt();

    try {
      await interruptActiveRun(session, prompt, pendingMessage.id);
    } catch (err) {
      discardPendingInterruptCommit(pendingMessage.id);
      setInterruptMessageState(session, pendingMessage.id, 'error');
      const message = err?.message || 'Failed to interrupt active run.';
      addErrorMessage(message, session);
      if (err?.status === 401) {
        handleAuthFailure();
      }
      elements.promptInput.value = prompt;
      autoGrowPrompt();
      persistAndRefreshShell();
      scrollToBottom(true);
    }
    return;
  }

  const reuseMessageId = typeof options.reuseMessageId === 'string' ? options.reuseMessageId : '';
  let userMessage = reuseMessageId
    ? session.messages.find(m => m.id === reuseMessageId && m.role === 'user') || null
    : null;
  const isNewUserMessage = !userMessage;

  if (!userMessage) {
    userMessage = {
      id: generateId('msg'),
      role: 'user',
      content: prompt,
      created: Date.now()
    };
    session.messages.push(userMessage);
  } else {
    userMessage.content = prompt;
    delete userMessage.interruptState;
  }

  if (pendingAttachments.length > 0) {
    userMessage.attachments = pendingAttachments.map(a => ({
      name: a.name,
      type: a.type,
      dataURL: a.dataURL
    }));
  } else {
    delete userMessage.attachments;
  }

  if (!session.title || session.title === 'New chat') {
    session.title = truncate(prompt || pendingAttachments[0]?.name || 'Image', 60);
  }

  const hadEmptyState = elements.messages.querySelector('.empty-state');
  if (hadEmptyState) hadEmptyState.remove();

  if (isNewUserMessage) {
    elements.messages.appendChild(createMessageNode(userMessage));
  } else {
    updateUserNode(userMessage);
  }

  persistAndRefreshShell();

  elements.promptInput.value = '';
  if (!Array.isArray(options.attachments)) {
    state.attachments = [];
    renderAttachments();
  }
  autoGrowPrompt();
  scrollToBottom(true);

  state.expectCanceledRun = false;
  const controller = new AbortController();
  state.abortController = controller;
  setStreaming(true);
  const streamState = createResponseStreamState(session);

  // Build input content: plain string or array with file/image parts
  let inputContent;
  if (pendingAttachments.length > 0) {
    const contentParts = [];
    for (const att of pendingAttachments) {
      if (att.type.startsWith('image/')) {
        contentParts.push({ type: 'input_image', image_url: att.dataURL, filename: att.name });
      } else {
        contentParts.push({ type: 'input_file', file_data: att.dataURL, filename: att.name });
      }
    }
    if (prompt) {
      contentParts.push({ type: 'input_text', text: prompt });
    }
    inputContent = contentParts;
  } else {
    inputContent = prompt;
  }

  const body = {
    stream: true,
    input: [{ type: 'message', role: 'user', content: inputContent }]
  };

  if (session.lastResponseId) {
    body.previous_response_id = session.lastResponseId;
  }

  if (state.selectedModel) {
    body.model = state.selectedModel;
  }

  try {
    let response = await fetch('/v1/responses', {
      method: 'POST',
      headers: {
        ...requestHeaders(session.id),
        'X-Term-LLM-UI': '1'
      },
      body: JSON.stringify(body),
      signal: controller.signal
    });

    // Recovery: if previous_response_id expired, rebuild conversation from
    // local messages and retry without chaining.
    if (!response.ok && body.previous_response_id) {
      const errData = await response.json().catch(() => null);
      const errMsg = errData?.error?.message || '';
      if (errMsg.includes('not found') && errMsg.includes('previous_response_id')) {
        delete body.previous_response_id;
        session.lastResponseId = null;
        body.input = rebuildInputFromSession(session, inputContent);

        response = await fetch('/v1/responses', {
          method: 'POST',
          headers: {
            ...requestHeaders(session.id),
            'X-Term-LLM-UI': '1'
          },
          body: JSON.stringify(body),
          signal: controller.signal
        });
      }
    }

    if (!response.ok) {
      throw await normalizeError(response);
    }

    if (!response.body) {
      throw { status: 0, message: 'No response body from server.' };
    }
    const headerResponseId = String(response.headers.get('x-response-id') || '').trim();
    if (headerResponseId) {
      setActiveResponseTracking(session, headerResponseId, 0);
      saveSessions();
    }

    const terminal = await consumeResponseStream(response.body, session, streamState);
    if (!terminal && session.activeResponseId) {
      await resumeActiveResponse(session, { streamState });
    }

    const lastAssistant = [...session.messages].reverse().find(m => m.role === 'assistant');
    if (lastAssistant) updateAssistantNode(lastAssistant);
    persistAndRefreshShell();
    scrollToBottom();
  } catch (err) {
    streamState.closeToolGroup();
    markToolGroupsDone(session);
    const lastAssistant = [...session.messages].reverse().find(m => m.role === 'assistant');
    if (lastAssistant) updateAssistantNode(lastAssistant);

    if (err?.name === 'AbortError') {
      persistAndRefreshShell();
      return;
    }

    if (session.activeResponseId) {
      await resumeActiveResponse(session, { streamState });
      persistAndRefreshShell();
      return;
    }

    await app.syncActiveSessionFromServer(session, true);
    if (session.activeResponseId || state.abortController) {
      persistAndRefreshShell();
      return;
    }

    const message = err?.message || 'Network error. Please try again.';
    addErrorMessage(message, session);
    if (err?.status === 401) {
      handleAuthFailure();
    }

    persistAndRefreshShell();
    scrollToBottom(true);
  } finally {
    if (state.abortController === controller) {
      state.abortController = null;
    }

    const stillActive = Boolean(session.activeResponseId || state.currentStreamResponseId);
    if (!stillActive && state.askUser?.sessionId === session.id) {
      closeAskUserModal();
    }

    setStreaming(stillActive);
    refreshRelativeTimes();
    if (stillActive) {
      return;
    }

    requeueUncommittedInterrupts(session);

    if (state.queuedInterrupts.length > 0) {
      const queued = state.queuedInterrupts.shift();
      elements.promptInput.value = queued.prompt;
      autoGrowPrompt();
      await sendMessage({
        prompt: queued.prompt,
        attachments: [],
        reuseMessageId: queued.messageId
      });
    }
  }
};

Object.assign(app, {
  requestHeaders,
  normalizeError,
  fetchModels,
  parseSSEStream,
  sleep,
  setActiveResponseTracking,
  clearActiveResponseTracking,
  updateResponseSequence,
  createResponseStreamState,
  applyResponseStreamEvent,
  consumeResponseStream,
  resumeActiveResponse,
  cancelActiveResponse,
  closeAskUserModal,
  openApprovalModal,
  closeApprovalModal,
  submitApprovalModal,
  askUserSummaryFromAnswers,
  collectAskUserAnswers,
  validateSingleQuestion,
  switchAskUserTab,
  renderAskUserModal,
  openAskUserModal,
  submitAskUserModal,
  openAuthModal,
  closeAuthModal,
  handleAuthFailure,
  connectToken,
  renderModelOptions,
  autoGrowPrompt,
  renderAttachments,
  MAX_ATTACHMENTS,
  MAX_FILE_BYTES,
  handleFiles,
  setStreaming,
  queueInterruptFollowUp,
  trackPendingInterruptCommit,
  resolvePendingInterruptCommit,
  discardPendingInterruptCommit,
  requeueUncommittedInterrupts,
  setInterruptMessageState,
  createPendingInterruptMessage,
  interruptActiveRun,
  addErrorMessage,
  markToolGroupsDone,
  sendMessage
});
})();
