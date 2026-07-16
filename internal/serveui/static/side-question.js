(() => {
'use strict';

const app = window.TermLLMApp;
const { UI_PREFIX, state, elements, getActiveSession, requestHeaders, getClipboardWriter } = app;
const side = state.sideQuestion;
side.pending = false;

const endpoint = (sessionId, suffix = '') => `${UI_PREFIX}/api/sessions/${encodeURIComponent(sessionId)}/side-question${suffix}`;

const latestAnswer = () => {
  const entries = Array.isArray(side.history) ? side.history : [];
  return String(entries[entries.length - 1]?.response || side.response || '');
};

const appendExchange = (container, question, response, thinking = false) => {
  const exchange = document.createElement('section');
  exchange.className = 'side-question-exchange';

  const questionLabel = document.createElement('div');
  questionLabel.className = 'side-question-speaker';
  questionLabel.textContent = 'You';
  const questionBody = document.createElement('div');
  questionBody.className = 'side-question-user';
  questionBody.textContent = String(question || '');

  const answerLabel = document.createElement('div');
  answerLabel.className = 'side-question-speaker';
  answerLabel.textContent = 'Side';
  const answerBody = document.createElement('div');
  answerBody.className = 'side-question-answer markdown-body';
  app.renderAssistantMarkdown(answerBody, String(response || (thinking ? 'Thinking…' : '')));

  exchange.append(questionLabel, questionBody, answerLabel, answerBody);
  container.appendChild(exchange);
};

const render = () => {
  const transcript = elements.sideQuestionTranscript;
  const nearBottom = transcript.scrollHeight - transcript.scrollTop - transcript.clientHeight < 80;
  const entries = Array.isArray(side.history) ? side.history : [];
  transcript.replaceChildren();
  entries.forEach((entry) => appendExchange(transcript, entry?.question, entry?.response));
  if (side.running || side.pending || side.synthetic || side.error) {
    appendExchange(transcript, side.question, side.response, side.running);
  }

  elements.sideQuestionOverlay.classList.toggle('hidden', !side.visible);
  const session = getActiveSession();
  const mainRunning = Boolean(state.streaming || session?.activeResponseId || app.sessionHasInProgressState?.(session));
  const mainLabel = mainRunning ? (state.streaming ? ' · main responding' : ' · main running') : '';
  elements.sideQuestionStatus.textContent = (side.running ? ' · answering' : ' · ready') + mainLabel;
  elements.sideQuestionError.textContent = side.error || '';
  elements.sideQuestionComposer.classList.toggle('hidden', side.running);
  elements.sideQuestionInput.disabled = side.running;
  elements.sideQuestionSendBtn.disabled = side.running || !String(elements.sideQuestionInput.value || '').trim();
  elements.sideQuestionCancelBtn.classList.toggle('hidden', !side.running);
  elements.sideQuestionCopyBtn.disabled = side.running || !latestAnswer();
  elements.sideQuestionClearBtn.disabled = side.running || entries.length === 0;
  const mainNeedsAttention = Boolean(state.askUser || state.approval || session?.pendingAskUser || session?.pendingApproval);
  elements.sideQuestionMainAttention.classList.toggle('hidden', !mainNeedsAttention);
  if (nearBottom) transcript.scrollTop = transcript.scrollHeight;
};

const applyView = (view) => {
  side.running = Boolean(view?.running);
  side.question = String(view?.question || '');
  side.response = String(view?.response || '');
  side.synthetic = Boolean(view?.synthetic);
  side.error = String(view?.error || '');
  side.usage = view?.usage || {};
  side.generation = Number(view?.generation || 0);
  side.history = Array.isArray(view?.history) ? view.history : [];
  side.pending = false;
  render();
};

const recover = async (sessionId) => {
  const response = await fetch(endpoint(sessionId), { headers: requestHeaders(sessionId) });
  if (!response.ok) throw new Error(`Unable to load side questions (${response.status})`);
  applyView(await response.json());
};

const parseSSE = async (response) => {
  if (!response.body) throw new Error('Side question stream unavailable');
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  let streamGeneration = null;
  const clientGeneration = side.generation;
  while (true) {
    if (side.generation !== clientGeneration) {
      await reader.cancel().catch(() => {});
      return;
    }
    const { value, done } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    let boundary;
    while ((boundary = buffer.indexOf('\n\n')) >= 0) {
      const block = buffer.slice(0, boundary);
      buffer = buffer.slice(boundary + 2);
      const line = block.split('\n').find((entry) => entry.startsWith('data: '));
      if (!line) continue;
      const event = JSON.parse(line.slice(6));
      const eventGeneration = Number(event.generation || 0);
      if (streamGeneration === null) streamGeneration = eventGeneration;
      if (eventGeneration !== streamGeneration) continue;
      if (event.type === 'text_delta') side.response += String(event.text || '');
      if (event.type === 'attempt_discard') side.response = '';
      if (event.type === 'done') {
        side.running = false;
        side.error = String(event.error || '');
        if (event.result) {
          side.response = String(event.result.Response ?? event.result.response ?? side.response);
          side.synthetic = Boolean(event.result.Synthetic ?? event.result.synthetic);
        }
      }
      render();
    }
  }
};

const focusComposer = () => {
  if (!side.running) window.setTimeout(() => elements.sideQuestionInput.focus(), 0);
};

const openSideQuestion = async (question = '') => {
  const session = getActiveSession();
  if (!session || state.draftSessionActive) {
    window.alert('Start the main conversation before asking a side question.');
    return;
  }
  const trimmed = String(question || '').trim();
  side.visible = true;
  side.error = '';
  render();
  if (!trimmed) {
    try {
      await recover(session.id);
      side.visible = true;
      render();
      focusComposer();
    } catch (err) {
      side.error = err?.message || String(err);
      render();
    }
    return;
  }
  if (side.running) {
    side.error = 'A side question is already running';
    render();
    return;
  }
  side.running = true;
  side.pending = true;
  side.question = trimmed;
  side.response = '';
  side.synthetic = false;
  side.generation += 1;
  elements.sideQuestionInput.value = '';
  render();
  try {
    const response = await fetch(endpoint(session.id), {
      method: 'POST',
      headers: requestHeaders(session.id),
      body: JSON.stringify({ question: trimmed })
    });
    if (!response.ok) {
      const payload = await response.json().catch(() => ({}));
      throw new Error(payload?.error?.message || `Side question failed (${response.status})`);
    }
    const serverGeneration = Number(response.headers.get('x-side-generation') || 0);
    if (serverGeneration) side.generation = serverGeneration;
    await parseSSE(response);
    await recover(session.id);
    side.visible = true;
    render();
    focusComposer();
  } catch (err) {
    side.running = false;
    side.pending = true;
    side.error = err?.message || String(err);
    render();
    focusComposer();
  }
};

const cancel = async () => {
  const session = getActiveSession();
  if (!session || !side.running) return;
  side.generation += 1;
  side.running = false;
  side.pending = false;
  side.question = '';
  side.response = '';
  side.error = '';
  render();
  focusComposer();
  await fetch(endpoint(session.id, '/active'), { method: 'DELETE', headers: requestHeaders(session.id) }).catch(() => {});
};

const close = () => {
  if (side.running) return;
  side.visible = false;
  render();
};

elements.sideQuestionCloseBtn.addEventListener('click', close);
elements.sideQuestionCancelBtn.addEventListener('click', () => { void cancel(); });
elements.sideQuestionComposer.addEventListener('submit', (event) => {
  event.preventDefault();
  if (side.running) return;
  const question = String(elements.sideQuestionInput.value || '').trim();
  if (question) void openSideQuestion(question);
});
elements.sideQuestionInput.addEventListener('input', render);
elements.sideQuestionCopyBtn.addEventListener('click', () => {
  getClipboardWriter()?.writeText(latestAnswer()).catch(() => {});
});
elements.sideQuestionClearBtn.addEventListener('click', async () => {
  const session = getActiveSession();
  if (!session || !window.confirm('Clear private side-question history?')) return;
  await fetch(endpoint(session.id, '/history'), { method: 'DELETE', headers: requestHeaders(session.id) }).catch(() => {});
  side.history = [];
  side.question = '';
  side.response = '';
  side.synthetic = false;
  side.error = '';
  side.pending = false;
  render();
  focusComposer();
});
document.addEventListener('keydown', (event) => {
  if (event.key !== 'Escape' || !side.visible) return;
  event.preventDefault();
  if (side.running) {
    void cancel();
  } else if (elements.sideQuestionInput.value) {
    elements.sideQuestionInput.value = '';
    render();
    focusComposer();
  } else {
    close();
  }
});

let observedSessionId = String(getActiveSession()?.id || '');
setInterval(() => {
  const currentId = String(getActiveSession()?.id || '');
  if (currentId === observedSessionId) return;
  const previousId = observedSessionId;
  observedSessionId = currentId;
  if (previousId && side.running) {
    fetch(endpoint(previousId, '/active'), { method: 'DELETE', headers: requestHeaders(previousId) }).catch(() => {});
  }
  side.generation += 1;
  side.visible = false;
  side.running = false;
  side.pending = false;
  side.question = '';
  side.response = '';
  side.error = '';
  side.history = [];
  render();
  if (currentId) recover(currentId).catch(() => {});
}, 500);

const initialSession = getActiveSession();
if (initialSession) recover(initialSession.id).catch(() => {});

Object.assign(app, { openSideQuestion, recoverSideQuestion: recover, renderSideQuestion: render });
})();
