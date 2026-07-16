(() => {
'use strict';

const app = window.TermLLMApp;
const { UI_PREFIX, state, elements, getActiveSession, requestHeaders, getClipboardWriter } = app;

const side = state.sideQuestion;

const endpoint = (sessionId, suffix = '') => `${UI_PREFIX}/api/sessions/${encodeURIComponent(sessionId)}/side-question${suffix}`;

const render = () => {
  const entries = Array.isArray(side.history) ? side.history : [];
  const selectedEntry = side.selected >= 0 && side.selected < entries.length ? entries[side.selected] : null;
  const usage = selectedEntry?.usage || side.usage || {};
  const inputTokens = Number(usage.InputTokens ?? usage.input_tokens ?? 0) + Number(usage.CachedInputTokens ?? usage.cached_input_tokens ?? 0);
  const outputTokens = Number(usage.OutputTokens ?? usage.output_tokens ?? 0);
  const question = side.running ? side.question : (selectedEntry?.question || side.question || '');
  const response = side.running ? side.response : (selectedEntry?.response || side.response || '');
  elements.sideQuestionOverlay.classList.toggle('hidden', !side.visible);
  const usageLabel = inputTokens || outputTokens ? ` · ${inputTokens} in / ${outputTokens} out` : '';
  elements.sideQuestionStatus.textContent = (side.running ? ' · answering' : ' · done') + usageLabel;
  elements.sideQuestionQuestion.textContent = question;
  const responseText = response || (side.running ? 'Thinking…' : '');
  app.renderAssistantMarkdown(elements.sideQuestionResponse, responseText);
  elements.sideQuestionError.textContent = side.error || '';
  elements.sideQuestionCancelBtn.classList.toggle('hidden', !side.running);
  elements.sideQuestionPrevBtn.disabled = side.running || side.selected <= 0;
  elements.sideQuestionNextBtn.disabled = side.running || side.selected < 0 || side.selected >= entries.length - 1;
  elements.sideQuestionCopyBtn.disabled = side.running || !response;
  elements.sideQuestionClearBtn.disabled = side.running || entries.length === 0;
  elements.sideQuestionPosition.textContent = entries.length ? `${Math.max(1, side.selected + 1)} / ${entries.length}` : '';
  const session = getActiveSession();
  const mainNeedsAttention = Boolean(state.askUser || state.approval || session?.pendingAskUser || session?.pendingApproval);
  elements.sideQuestionMainAttention.classList.toggle('hidden', !mainNeedsAttention);
};

const applyView = (view) => {
  side.running = Boolean(view?.running);
  side.question = String(view?.question || '');
  side.response = String(view?.response || '');
  side.error = String(view?.error || '');
  side.usage = view?.usage || {};
  side.generation = Number(view?.generation || 0);
  side.history = Array.isArray(view?.history) ? view.history : [];
  side.selected = side.history.length - 1;
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
        if (event.result) side.response = String(event.result.Response ?? event.result.response ?? side.response);
      }
      render();
    }
  }
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
      side.visible = side.history.length > 0 || side.running;
      if (!side.visible) window.alert('Usage: /side <question>');
      render();
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
  side.question = trimmed;
  side.response = '';
  side.selected = side.history.length;
  side.generation += 1;
  elements.promptInput.value = '';
  render();
  try {
    const response = await fetch(endpoint(session.id), {
      method: 'POST',
      headers: requestHeaders(session.id),
      body: JSON.stringify({ question: trimmed, model: state.selectedModel, reasoning_effort: state.selectedEffort })
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
  } catch (err) {
    side.running = false;
    side.error = err?.message || String(err);
    render();
  }
};

const cancel = async () => {
  const session = getActiveSession();
  if (!session) return;
  side.generation += 1;
  side.running = false;
  side.response = '';
  render();
  await fetch(endpoint(session.id, '/active'), { method: 'DELETE', headers: requestHeaders(session.id) }).catch(() => {});
};

const close = async () => {
  if (side.running && !window.confirm('Cancel the running side question?')) return;
  if (side.running) await cancel();
  side.visible = false;
  render();
};

elements.sideQuestionCloseBtn.addEventListener('click', close);
elements.sideQuestionCancelBtn.addEventListener('click', cancel);
elements.sideQuestionPrevBtn.addEventListener('click', () => { if (side.selected > 0) side.selected -= 1; render(); });
elements.sideQuestionNextBtn.addEventListener('click', () => { if (side.selected + 1 < side.history.length) side.selected += 1; render(); });
elements.sideQuestionCopyBtn.addEventListener('click', () => {
  const entry = side.history[side.selected];
  const text = entry?.response || side.response;
  getClipboardWriter()?.writeText(String(text || '')).catch(() => {});
});
elements.sideQuestionClearBtn.addEventListener('click', async () => {
  const session = getActiveSession();
  if (!session || !window.confirm('Clear private side-question history?')) return;
  await fetch(endpoint(session.id, '/history'), { method: 'DELETE', headers: requestHeaders(session.id) }).catch(() => {});
  side.history = [];
  side.selected = -1;
  side.response = '';
  side.visible = false;
  render();
});
document.addEventListener('keydown', (event) => {
  if (event.key === 'Escape' && side.visible) {
    event.preventDefault();
    void close();
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
  side.question = '';
  side.response = '';
  side.error = '';
  side.history = [];
  side.selected = -1;
  render();
  if (currentId) recover(currentId).catch(() => {});
}, 500);

const initialSession = getActiveSession();
if (initialSession) recover(initialSession.id).catch(() => {});

Object.assign(app, { openSideQuestion, recoverSideQuestion: recover });
})();
