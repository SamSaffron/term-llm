(() => {
'use strict';

const app = window.TermLLMApp;
const { UI_PREFIX, state, elements, getActiveSession, requestHeaders } = app;
const side = state.sideQuestion;
side.pending = false;
let openOperation = 0;
const isResolvedSessionIdentity = typeof app.isSessionIdentityResolved === 'function'
  ? app.isSessionIdentityResolved
  : (sessionId) => Boolean(String(sessionId || '').trim()) && !/^\d+$/.test(String(sessionId).trim());

const endpoint = (sessionId, suffix = '') => `${UI_PREFIX}/api/sessions/${encodeURIComponent(sessionId)}/side-question${suffix}`;

const appendExchange = (container, question, response, thinking = false) => {
  const exchange = document.createElement('section');
  exchange.className = 'side-question-exchange';

  const questionMessage = document.createElement('article');
  questionMessage.className = 'message user';
  const questionBody = document.createElement('div');
  questionBody.className = 'message-body';
  questionBody.textContent = String(question || '');
  questionMessage.appendChild(questionBody);

  const answerMessage = document.createElement('article');
  answerMessage.className = 'message assistant';
  const answerBody = document.createElement('div');
  answerBody.className = 'message-body markdown-body';
  app.renderAssistantMarkdown(answerBody, String(response || (thinking ? 'Thinking…' : '')));
  answerMessage.appendChild(answerBody);

  exchange.append(questionMessage, answerMessage);
  container.appendChild(exchange);
};

const render = () => {
  const transcript = elements.sideQuestionTranscript;
  const nearBottom = transcript.scrollHeight - transcript.scrollTop - transcript.clientHeight < 80;
  const entries = Array.isArray(side.history) ? side.history : [];
  transcript.replaceChildren();
  entries.forEach((entry) => appendExchange(transcript, entry?.question, entry?.response));
  const lastEntry = entries[entries.length - 1];
  const currentIsStored = !side.running && !side.pending
    && String(lastEntry?.question || '') === String(side.question || '')
    && String(lastEntry?.response || '') === String(side.response || '');
  const hasCurrentExchange = Boolean(side.running || side.pending || side.synthetic || side.response) && !currentIsStored;
  if (hasCurrentExchange) {
    appendExchange(transcript, side.question, side.response, side.running);
  }

  elements.sideQuestionOverlay.classList.toggle('hidden', !side.visible);
  elements.sideQuestionTranscript.classList.toggle('hidden', entries.length === 0 && !hasCurrentExchange);
  elements.sideQuestionError.textContent = side.error || '';
  elements.sideQuestionError.classList.toggle('hidden', !side.error);
  elements.sideQuestionComposer.classList.toggle('hidden', side.running);
  elements.sideQuestionInput.disabled = side.running;
  elements.sideQuestionSendBtn.disabled = side.running || !String(elements.sideQuestionInput.value || '').trim();
  const session = getActiveSession();
  const mainNeedsApproval = Boolean(state.approval || session?.pendingApproval);
  const mainNeedsInput = Boolean(state.askUser || session?.pendingAskUser);
  elements.sideQuestionMainAttention.classList.toggle('hidden', !mainNeedsApproval && !mainNeedsInput);
  elements.sideQuestionMainAttention.textContent = mainNeedsApproval ? 'Main needs approval.' : (mainNeedsInput ? 'Main needs input.' : '');
  if (nearBottom) transcript.scrollTop = transcript.scrollHeight;
};

const applyView = (view) => {
  side.running = Boolean(view?.running);
  side.question = String(view?.question || '');
  side.response = String(view?.response || '');
  side.synthetic = Boolean(view?.synthetic);
  side.error = String(view?.error || '');
  side.usage = view?.usage || {};
  side.totalUsage = view?.total_usage || {};
  side.requests = Number(view?.requests || 0);
  side.generation = Number(view?.generation || 0);
  side.history = Array.isArray(view?.history) ? view.history : [];
  side.pending = false;
  render();
};

const recover = async (sessionId) => {
  if (!isResolvedSessionIdentity(sessionId)) return false;
  const response = await fetch(endpoint(sessionId), { headers: requestHeaders(sessionId) });
  if (!response.ok) throw new Error(`Unable to load side questions (${response.status})`);
  applyView(await response.json());
  return true;
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
          side.response = String(event.result.response ?? side.response);
          side.synthetic = Boolean(event.result.synthetic);
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
  if (!isResolvedSessionIdentity(session)) {
    window.alert('This session is still loading. Try again in a moment.');
    return;
  }
  const trimmed = String(question || '').trim();
  const operation = ++openOperation;
  side.visible = true;
  side.error = '';
  render();
  if (!trimmed) {
    try {
      await recover(session.id);
      if (operation !== openOperation || !side.visible) return;
      render();
      focusComposer();
    } catch (err) {
      if (operation !== openOperation) return;
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
    if (operation !== openOperation) return;
    await recover(session.id);
    if (operation !== openOperation || !side.visible) return;
    render();
    focusComposer();
  } catch (err) {
    if (operation !== openOperation) return;
    await fetch(endpoint(session.id, '/active'), { method: 'DELETE', headers: requestHeaders(session.id) }).catch(() => {});
    side.running = false;
    side.pending = false;
    side.error = err?.message || String(err);
    render();
    focusComposer();
  }
};

const cancel = async (focus = true) => {
  openOperation += 1;
  const session = getActiveSession();
  if (!session) return;
  side.generation += 1;
  side.running = false;
  side.pending = false;
  side.question = '';
  side.response = '';
  side.error = '';
  render();
  if (focus) focusComposer();
  if (isResolvedSessionIdentity(session)) {
    await fetch(endpoint(session.id, '/active'), { method: 'DELETE', headers: requestHeaders(session.id) }).catch(() => {});
  }
};

const close = () => {
  openOperation += 1;
  side.visible = false;
  if (side.running) {
    void cancel(false);
    return;
  }
  render();
};

elements.sideQuestionCloseBtn.addEventListener('click', close);
elements.sideQuestionComposer.addEventListener('submit', (event) => {
  event.preventDefault();
  if (side.running) return;
  const question = String(elements.sideQuestionInput.value || '').trim();
  if (question) void openSideQuestion(question);
});
elements.sideQuestionInput.addEventListener('input', render);
document.addEventListener('keydown', (event) => {
  if (event.defaultPrevented || event.key !== 'Escape' || !side.visible) return;
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
  if (previousId && side.running && isResolvedSessionIdentity(previousId)) {
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
}, 500);

Object.assign(app, { openSideQuestion, recoverSideQuestion: recover, renderSideQuestion: render });
})();
