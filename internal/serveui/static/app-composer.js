'use strict';

(function initAppComposer() {

const app = window.TermLLMApp;
const {
  UI_PREFIX, state, elements, applyTextDirection
} = app;
const { normalizeError } = app;

// ===== Composer logic =====
const formatVoiceDuration = (ms) => {
  const totalSeconds = Math.max(0, Math.floor(ms / 1000));
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  return `${minutes}:${String(seconds).padStart(2, '0')}`;
};

const stopVoiceTracks = () => {
  const stream = state.voice.stream;
  if (!stream) return;
  stream.getTracks().forEach((track) => track.stop());
  state.voice.stream = null;
};

const clearVoiceTimer = () => {
  if (state.voice.timerId !== null) {
    clearInterval(state.voice.timerId);
    state.voice.timerId = null;
  }
};

const setVoiceStatus = (message = '') => {
  state.voice.status = String(message || '');
  const el = elements.voiceStatus;
  if (!el) return;
  if (!state.voice.status) {
    el.className = 'voice-status hidden';
    el.innerHTML = '';
    return;
  }
  el.className = 'voice-status';
  el.innerHTML = state.voice.status;
};

const updateVoiceUI = () => {
  const btn = elements.voiceBtn;
  if (!btn) return;

  const unsupported = !state.voice.supported;
  const busy = state.voice.transcribing;
  const recording = state.voice.recording;

  btn.disabled = unsupported || busy;
  btn.classList.toggle('recording', recording);
  btn.classList.toggle('busy', busy);

  if (unsupported) {
    btn.title = 'Voice recording is not supported in this browser';
    btn.setAttribute('aria-label', 'Voice recording is not supported in this browser');
    setVoiceStatus('');
    return;
  }

  if (recording) {
    const elapsed = Date.now() - state.voice.startedAt;
    btn.title = 'Stop and send voice message';
    btn.setAttribute('aria-label', 'Stop and send voice message');
    setVoiceStatus(
      `<span class="voice-status-dot" aria-hidden="true"></span>` +
      `<span class="voice-status-copy">Recording <strong>${formatVoiceDuration(elapsed)}</strong></span>` +
      `<button type="button" class="voice-status-cancel" id="voiceCancelBtn">Cancel</button>`
    );
    const cancelBtn = document.getElementById('voiceCancelBtn');
    if (cancelBtn) {
      cancelBtn.addEventListener('click', () => stopVoiceRecording(true), { once: true });
    }
    return;
  }

  if (busy) {
    btn.title = 'Transcribing voice message';
    btn.setAttribute('aria-label', 'Transcribing voice message');
    setVoiceStatus('<span class="voice-status-spinner" aria-hidden="true"></span><span class="voice-status-copy">Transcribing voice message…</span>');
    return;
  }

  btn.title = 'Record voice message';
  btn.setAttribute('aria-label', 'Record voice message');
  setVoiceStatus('');
};

const voiceRecordingMimeType = () => {
  if (typeof MediaRecorder === 'undefined' || typeof MediaRecorder.isTypeSupported !== 'function') {
    return '';
  }
  const candidates = [
    'audio/webm;codecs=opus',
    'audio/mp4',
    'audio/webm',
    'audio/ogg;codecs=opus'
  ];
  return candidates.find((type) => MediaRecorder.isTypeSupported(type)) || '';
};

const audioFilenameForMimeType = (mimeType) => {
  const normalized = String(mimeType || '').toLowerCase();
  if (normalized.includes('mp4') || normalized.includes('m4a')) return 'voice-note.m4a';
  if (normalized.includes('ogg')) return 'voice-note.ogg';
  if (normalized.includes('wav')) return 'voice-note.wav';
  return 'voice-note.webm';
};

const transcribeVoiceBlob = async (blob, mimeType) => {
  const form = new FormData();
  form.append('file', blob, audioFilenameForMimeType(mimeType));

  const headers = {};
  if (state.token) {
    headers.Authorization = `Bearer ${state.token}`;
  }

  const response = await app.apiFetch(`${UI_PREFIX}/v1/transcribe`, {
    method: 'POST',
    headers,
    body: form
  });

  if (!response.ok) {
    throw await normalizeError(response);
  }

  const payload = await response.json().catch(() => null);
  const text = String(payload?.text || '').trim();
  if (!text) {
    throw new Error('Transcription came back empty.');
  }
  return text;
};

const handleRecordedVoiceBlob = async (blob, mimeType) => {
  state.voice.transcribing = true;
  updateVoiceUI();

  try {
    const transcript = await transcribeVoiceBlob(blob, mimeType);
    const existingPrompt = String(elements.promptInput.value || '').trim();

    if (!existingPrompt && state.attachments.length === 0) {
      void app.sendMessage({ prompt: transcript, attachments: [] });
      return;
    }

    elements.promptInput.value = existingPrompt ? `${existingPrompt}\n${transcript}` : transcript;
    autoGrowPrompt();
    elements.promptInput.focus();
  } finally {
    state.voice.transcribing = false;
    updateVoiceUI();
  }
};

const startVoiceRecording = async () => {
  if (!state.voice.supported || state.voice.recording || state.voice.transcribing) return;
  if (!state.connected) {
    app.openAuthModal('Connect before sending a voice message.', true);
    return;
  }

  try {
    const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
    const mimeType = voiceRecordingMimeType();
    const recorder = mimeType ? new MediaRecorder(stream, { mimeType }) : new MediaRecorder(stream);

    state.voice.recording = true;
    state.voice.recorder = recorder;
    state.voice.stream = stream;
    state.voice.chunks = [];
    state.voice.cancelOnStop = false;
    state.voice.startedAt = Date.now();
    state.voice.mimeType = mimeType || recorder.mimeType || 'audio/webm';

    recorder.addEventListener('dataavailable', (event) => {
      if (event.data && event.data.size > 0) {
        state.voice.chunks.push(event.data);
      }
    });

    recorder.addEventListener('stop', async () => {
      const cancelled = state.voice.cancelOnStop;
      const chunks = [...state.voice.chunks];
      const blobType = state.voice.mimeType || recorder.mimeType || 'audio/webm';

      state.voice.recording = false;
      state.voice.recorder = null;
      state.voice.chunks = [];
      state.voice.cancelOnStop = false;
      clearVoiceTimer();
      stopVoiceTracks();
      updateVoiceUI();

      if (cancelled || chunks.length === 0) {
        setVoiceStatus('');
        return;
      }

      const blob = new Blob(chunks, { type: blobType });
      try {
        await handleRecordedVoiceBlob(blob, blobType);
      } catch (err) {
        setVoiceStatus('');
        if (err?.status === 401) {
          app.handleAuthFailure();
          return;
        }
        alert(err?.message || 'Failed to transcribe voice message.');
      }
    }, { once: true });

    recorder.start();
    clearVoiceTimer();
    state.voice.timerId = window.setInterval(() => updateVoiceUI(), 250);
    updateVoiceUI();
  } catch (err) {
    stopVoiceTracks();
    state.voice.recording = false;
    state.voice.recorder = null;
    clearVoiceTimer();
    updateVoiceUI();
    alert(err?.message || 'Microphone access failed.');
  }
};

const stopVoiceRecording = (cancelled = false) => {
  if (!state.voice.recording || !state.voice.recorder) return;
  state.voice.cancelOnStop = cancelled;
  const recorder = state.voice.recorder;
  if (recorder.state !== 'inactive') {
    recorder.stop();
  }
};

const toggleVoiceRecording = async () => {
  if (state.voice.recording) {
    stopVoiceRecording(false);
    return;
  }
  await startVoiceRecording();
};

const updateSendButtonState = () => {
  const btn = elements.sendBtn;
  if (!btn) return;
  const hasComposerDraft = Boolean(String(elements.promptInput?.value || '').trim()) || state.attachments.length > 0;
  const interjecting = Boolean(state.streaming && hasComposerDraft);
  const loading = Boolean(state.streaming && !hasComposerDraft);
  btn.disabled = false;
  btn.classList.toggle('loading', loading);
  btn.classList.toggle('interject', interjecting);
  const label = interjecting ? 'Interject' : 'Send message';
  btn.title = label;
  if (typeof btn.setAttribute === 'function') {
    btn.setAttribute('aria-label', label);
  }
  const arrow = typeof btn.querySelector === 'function' ? btn.querySelector('.arrow') : null;
  if (arrow) {
    arrow.textContent = interjecting ? '↳' : '↑';
  }
};

const composerUsesNativeAutoSize = (() => {
  try {
    const css = window.CSS || globalThis.CSS;
    return !!(css && typeof css.supports === 'function' && css.supports('field-sizing', 'content'));
  } catch {
    return false;
  }
})();

let pendingPromptGrowFrame = 0;
let lastPromptGrowHeight = '';
let lastPromptGrowValue = null;

const promptFallbackMaxHeight = () => {
  const viewportHeight = Number(window.innerHeight || 0);
  if (!viewportHeight) return 300;
  return Math.max(200, Math.min(300, Math.floor(viewportHeight * 0.45)));
};

const applyPromptFallbackSize = () => {
  pendingPromptGrowFrame = 0;
  const el = elements.promptInput;
  if (!el || composerUsesNativeAutoSize) return;

  const value = String(el.value || '');
  if (lastPromptGrowValue === value && lastPromptGrowHeight) return;
  lastPromptGrowValue = value;

  el.style.height = 'auto';
  const scrollHeight = el.scrollHeight || 0;
  const maxHeight = promptFallbackMaxHeight();
  const measured = Math.max(48, Math.min(scrollHeight, maxHeight));
  const nextHeight = `${measured}px`;
  const nextOverflow = scrollHeight > maxHeight ? 'auto' : 'hidden';

  el.style.height = nextHeight;
  lastPromptGrowHeight = nextHeight;
  if (el.style.overflowY !== nextOverflow) {
    el.style.overflowY = nextOverflow;
  }
};

const schedulePromptFallbackSize = () => {
  if (composerUsesNativeAutoSize || pendingPromptGrowFrame) return;
  const el = elements.promptInput;
  if (!el) return;
  if (lastPromptGrowValue === String(el.value || '') && lastPromptGrowHeight) return;
  pendingPromptGrowFrame = window.requestAnimationFrame(applyPromptFallbackSize);
};

const autoGrowPrompt = () => {
  const el = elements.promptInput;
  if (!el) return;

  applyTextDirection(el, el.value || '');
  updateSendButtonState();
  schedulePromptFallbackSize();
};

Object.assign(app, {
  formatVoiceDuration,
  stopVoiceTracks,
  clearVoiceTimer,
  setVoiceStatus,
  updateVoiceUI,
  voiceRecordingMimeType,
  audioFilenameForMimeType,
  transcribeVoiceBlob,
  handleRecordedVoiceBlob,
  startVoiceRecording,
  stopVoiceRecording,
  toggleVoiceRecording,
  updateSendButtonState,
  composerUsesNativeAutoSize,
  pendingPromptGrowFrame,
  lastPromptGrowHeight,
  lastPromptGrowValue,
  promptFallbackMaxHeight,
  applyPromptFallbackSize,
  schedulePromptFallbackSize,
  autoGrowPrompt
});
})();
