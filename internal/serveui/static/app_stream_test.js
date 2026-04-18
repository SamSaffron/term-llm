#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const { TextEncoder, TextDecoder } = require('util');
const { ReadableStream } = require('stream/web');
const { webcrypto } = require('crypto');

const dir = __dirname;
const source = fs.readFileSync(path.join(dir, 'app-stream.js'), 'utf8');

let failures = 0;

function fail(name, message, details) {
  console.error('FAIL:', name, '-', message);
  if (details) {
    console.error('      ', details);
  }
  failures += 1;
}

function pass(name) {
  console.log('PASS:', name);
}

function makeClassList() {
  return {
    toggle() {},
    add() {},
    remove() {},
  };
}

function makeNode() {
  return {
    classList: makeClassList(),
    appendChild() {},
    querySelector() { return null; },
    querySelectorAll() { return []; },
    setAttribute() {},
    removeAttribute() {},
    remove() {},
    addEventListener() {},
    focus() {},
    value: '',
    textContent: '',
    innerHTML: '',
    disabled: false,
  };
}

function makeMessageContainer() {
  return {
    classList: makeClassList(),
    children: [],
    appendChild(node) {
      this.children.push(node);
      return node;
    },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    remove() {},
  };
}

function createHarness(options = {}) {
  let idCounter = 0;
  let postStreamController = null;
  let postStreamCanceled = false;
  let getEventsStarted = false;
  let eventsStreamController = null;
  const fetchCalls = [];
  const responseId = options.responseId || 'resp_test';
  const postBody = String(options.postBody || '');
  const eventsKeepOpen = Boolean(options.eventsKeepOpen);
  const eventsBody = String(options.eventsBody || [
    'id: 1\n',
    'event: response.created\n',
    `data: {"response":{"id":"${responseId}","model":"test-model","status":"in_progress"},"sequence_number":1}\n\n`,
    'id: 2\n',
    'event: response.output_text.delta\n',
    'data: {"delta":"hello","sequence_number":2}\n\n',
    'id: 3\n',
    'event: response.completed\n',
    `data: {"response":{"id":"${responseId}","model":"test-model","status":"completed"},"sequence_number":3}\n\n`,
    'data: [DONE]\n\n',
  ].join(''));

  const document = {
    activeElement: null,
    getElementById() { return makeNode(); },
    createElement() { return makeNode(); },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    addEventListener() {},
  };

  const storage = new Map();
  const localStorage = {
    getItem(key) {
      return storage.has(key) ? storage.get(key) : null;
    },
    setItem(key, value) {
      storage.set(String(key), String(value));
    },
    removeItem(key) {
      storage.delete(String(key));
    },
  };

  const elements = {
    promptInput: {
      value: '',
      disabled: false,
      style: {},
      scrollHeight: 48,
      focus() {},
    },
    messages: makeMessageContainer(),
    sendBtn: { disabled: false, classList: makeClassList() },
    stopBtn: { classList: makeClassList() },
    attachmentsStrip: {
      innerHTML: '',
      style: {},
      appendChild() {},
    },
    providerSelect: {
      value: '',
      innerHTML: '',
      appendChild() {},
      addEventListener() {},
      removeAttribute() {},
      setAttribute() {},
    },
    modelSelect: {
      value: '',
      innerHTML: '',
      appendChild() {},
      addEventListener() {},
      removeAttribute() {},
      setAttribute() {},
    },
    effortSelect: {
      value: '',
      addEventListener() {},
      removeAttribute() {},
      setAttribute() {},
    },
    authTokenInput: {
      value: '',
      focus() {},
      removeAttribute() {},
      setAttribute() {},
    },
    authModal: {
      classList: makeClassList(),
    },
    authError: { textContent: '' },
    authCancelBtn: { style: {}, removeAttribute() {}, setAttribute() {} },
    authConnectBtn: { disabled: false, textContent: 'Save' },
    showHiddenSessionsInput: { checked: false, removeAttribute() {}, setAttribute() {} },
    voiceBtn: null,
    voiceStatus: null,
    askUserModal: makeNode(),
    askUserModalBody: makeNode(),
    askUserError: { textContent: '' },
    askUserSubmitBtn: { disabled: false, textContent: 'Continue' },
    askUserCancelBtn: { disabled: false, textContent: 'Dismiss' },
  };
  document.activeElement = elements.promptInput;

  const state = {
    token: '',
    connected: true,
    authRequired: false,
    streaming: false,
    streamGeneration: 0,
    attachments: [],
    sessions: [],
    activeSessionId: '',
    draftSessionActive: false,
    abortController: null,
    currentStreamSessionId: '',
    currentStreamResponseId: '',
    restorePromptFocus: false,
    queuedInterrupts: [],
    pendingInterruptCommits: [],
    voice: { chunks: [] },
    askUser: null,
    approval: null,
    selectedModel: '',
    selectedProvider: '',
    selectedEffort: '',
    showHiddenSessions: false,
    providers: [],
    lastEventTime: 0,
    expectCanceledRun: false,
  };

  const app = {
    UI_PREFIX: '/ui',
    STORAGE_KEYS: {
      selectedProvider: 'selectedProvider',
      selectedModel: 'selectedModel',
      selectedEffort: 'selectedEffort',
      showHiddenSessions: 'showHiddenSessions',
      token: 'token',
    },
    state,
    elements,
    __busyTransitions: [],
    generateId(prefix) {
      idCounter += 1;
      return `${prefix}_${idCounter}`;
    },
    sanitizeInterruptState(value) { return value; },
    sanitizeMessage(value) { return value; },
    syncTokenCookie() {},
    truncate(value, len) { return String(value || '').slice(0, len); },
    saveSessions() {},
    getActiveSession() {
      return state.sessions.find((session) => session.id === state.activeSessionId) || null;
    },
    createSession() {
      return {
        id: `session_${state.sessions.length + 1}`,
        title: 'New chat',
        messages: [],
        lastResponseId: null,
        activeResponseId: null,
        lastSequenceNumber: 0,
        number: 0,
      };
    },
    findMessageElement() { return null; },
    scrollToBottom() {},
    setConnectionState() {},
    sessionSlug(s) { return s ? s.id : ''; },
    updateURL() {},
    persistAndRefreshShell() {},
    updateHeader() {},
    updateSessionUsageDisplay() {},
    refreshRelativeTimes() {},
    updateAssistantNode() {},
    updateUserNode() {},
    updateToolNode() {},
    updateToolGroupNode() {},
    createMessageNode() { return makeNode(); },
    createToolGroupNode() { return makeNode(); },
    renderSidebar() {},
    renderMessages() {},
    maybeNotifyResponseComplete: async () => {},
    enqueueAssistantStreamUpdate() {},
    finalizeAssistantStreamRender() {},
    subscribeToPush() {},
    shouldAutoSubscribeToPush() { return false; },
    applyTextDirection() {},
    shouldSuppressPromptAutoFocus() { return false; },
    syncActiveSessionFromServer: async () => {},
    scheduleSessionStatePoll() {},
    setSessionOptimisticBusy(sessionOrId, busy) {
      const id = typeof sessionOrId === 'string'
        ? sessionOrId
        : String(sessionOrId?.id || '');
      const session = state.sessions.find((item) => item.id === id) || null;
      if (session) {
        session.__optimisticBusy = Boolean(busy);
      }
      app.__busyTransitions.push({ id, busy: Boolean(busy) });
    },
    setSessionServerActiveRun(sessionOrId, activeRun) {
      const id = typeof sessionOrId === 'string'
        ? sessionOrId
        : String(sessionOrId?.id || '');
      const session = state.sessions.find((item) => item.id === id) || null;
      if (session) {
        session.__serverActiveRun = Boolean(activeRun);
      }
    },
    sessionHasInProgressState(sessionOrId) {
      const id = typeof sessionOrId === 'string'
        ? sessionOrId
        : String(sessionOrId?.id || '');
      const session = state.sessions.find((item) => item.id === id) || null;
      return Boolean(session?.__optimisticBusy || session?.__serverActiveRun);
    },
  };

  const windowObj = {
    TermLLMApp: app,
    setTimeout,
    clearTimeout,
    setInterval,
    clearInterval,
    requestAnimationFrame(callback) {
      return setTimeout(callback, 0);
    },
    cancelAnimationFrame(handle) {
      clearTimeout(handle);
    },
    location: { search: '', origin: 'https://example.test' },
  };

  const encoder = new TextEncoder();
  const context = {
    window: windowObj,
    document,
    console,
    localStorage,
    setTimeout,
    clearTimeout,
    setInterval,
    clearInterval,
    AbortController,
    DOMException,
    TextEncoder,
    TextDecoder,
    ReadableStream,
    Response,
    Headers,
    URL,
    URLSearchParams,
    Blob,
    performance: { now: () => Date.now() },
    navigator: { mediaDevices: null },
    MediaRecorder: undefined,
    FileReader: class {},
    alert() {},
    crypto: webcrypto,
  };
  context.globalThis = context;
  windowObj.document = document;
  windowObj.localStorage = localStorage;
  windowObj.fetch = async function fetch(url, options = {}) {
    fetchCalls.push({
      url,
      method: options.method || 'GET',
      body: typeof options.body === 'string' ? options.body : null,
    });
    if (url === '/ui/v1/responses') {
      return new Response(new ReadableStream({
        start(controller) {
          postStreamController = controller;
          if (postBody) {
            controller.enqueue(encoder.encode(postBody));
          }
        },
        cancel() {
          postStreamCanceled = true;
        },
      }), {
        status: 200,
        headers: { 'x-response-id': responseId },
      });
    }
    if (url.startsWith(`/ui/v1/responses/${responseId}/events?after=`)) {
      getEventsStarted = true;
      const signal = options.signal || null;
      return new Response(new ReadableStream({
        start(controller) {
          eventsStreamController = controller;
          if (eventsBody) {
            controller.enqueue(encoder.encode(eventsBody));
          }
          if (!eventsKeepOpen) {
            controller.close();
          } else if (signal) {
            // Wire abort signal to stream so pending reads reject with AbortError
            signal.addEventListener('abort', () => {
              try { controller.error(new DOMException('The operation was aborted.', 'AbortError')); } catch (_e) { /* ignore */ }
            });
          }
        },
      }), {
        status: 200,
        headers: { 'Content-Type': 'text/event-stream' },
      });
    }
    throw new Error(`unexpected fetch: ${url}`);
  };
  context.fetch = windowObj.fetch;

  vm.runInNewContext(source, context, { filename: 'app-stream.js' });

  return {
    app,
    elements,
    state,
    fetchCalls,
    localStorage,
    getEventsStarted: () => getEventsStarted,
    postStreamCanceled: () => postStreamCanceled,
    closeEventsStream: () => {
      if (eventsStreamController) {
        try { eventsStreamController.close(); } catch (_err) { /* ignore */ }
      }
    },
    cleanup: async () => {
      if (postStreamController) {
        try {
          postStreamController.close();
        } catch (_err) {
          // ignore cleanup errors
        }
      }
      if (eventsStreamController) {
        try {
          eventsStreamController.close();
        } catch (_err) {
          // ignore cleanup errors
        }
      }
      await new Promise((resolve) => setTimeout(resolve, 0));
    },
  };
}

async function waitFor(predicate, timeoutMs) {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    if (predicate()) return true;
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  return false;
}

async function testSendMessageHandsOffToEventsStream() {
  const name = 'sendMessage hands off to /events after x-response-id even if POST body stalls';
  const harness = createHarness();
  const { app, elements, fetchCalls, getEventsStarted, cleanup } = harness;
  elements.promptInput.value = 'hello';

  let sendErr = null;
  const sendPromise = app.sendMessage().catch((err) => {
    sendErr = err;
  });

  const handedOff = await waitFor(() => getEventsStarted(), 75);
  if (!handedOff) {
    fail(name, 'client never opened the resumable /events stream', JSON.stringify(fetchCalls));
    await cleanup();
    await sendPromise;
    return;
  }

  await sendPromise;
  await cleanup();

  if (sendErr) {
    fail(name, 'sendMessage rejected unexpectedly', String(sendErr));
    return;
  }

  const session = harness.state.sessions[0];
  const assistant = session && session.messages.find((message) => message.role === 'assistant');
  if (!assistant || assistant.content !== 'hello') {
    fail(name, 'assistant content did not complete via /events handoff', assistant ? assistant.content : 'missing');
    return;
  }

  pass(name);
}

async function testSendMessageIgnoresPostBodyAfterHandoff() {
  const name = 'sendMessage ignores queued POST-body SSE once it hands off to /events';
  const harness = createHarness({
    postBody: [
      'id: 900\n',
      'event: response.output_text.delta\n',
      'data: {"delta":"stale","sequence_number":900}\n\n',
    ].join(''),
  });
  const { app, elements, cleanup, getEventsStarted, postStreamCanceled } = harness;
  elements.promptInput.value = 'hello';

  let sendErr = null;
  const sendPromise = app.sendMessage().catch((err) => {
    sendErr = err;
  });

  const handedOff = await waitFor(() => getEventsStarted(), 75);
  if (!handedOff) {
    fail(name, 'client never switched to the resumable /events stream');
    await cleanup();
    await sendPromise;
    return;
  }

  await sendPromise;
  await cleanup();

  if (sendErr) {
    fail(name, 'sendMessage rejected unexpectedly', String(sendErr));
    return;
  }

  if (!postStreamCanceled()) {
    fail(name, 'POST body stream was not canceled during handoff');
    return;
  }

  const session = harness.state.sessions[0];
  const assistant = session && session.messages.find((message) => message.role === 'assistant');
  if (!assistant || assistant.content !== 'hello') {
    fail(name, 'assistant content was polluted by POST-body data', assistant ? assistant.content : 'missing');
    return;
  }

  pass(name);
}

async function testNewChatDuringStreamingClearsStreamingState() {
  const name = 'New Chat during streaming clears streaming state and allows new session';

  const responseId = 'resp_long';
  const h = createHarness({
    responseId,
    eventsKeepOpen: true,
    eventsBody: [
      'id: 1\n',
      'event: response.created\n',
      `data: {"response":{"id":"${responseId}","model":"test-model","status":"in_progress"},"sequence_number":1}\n\n`,
      'id: 2\n',
      'event: response.output_text.delta\n',
      `data: {"delta":"working","sequence_number":2}\n\n`,
    ].join(''),
  });

  h.elements.promptInput.value = 'sleep for 30 secs';

  let sendErr = null;
  const sendPromise = h.app.sendMessage().catch((err) => {
    sendErr = err;
  });

  // Wait for the events stream to start (stream stays open, reader is blocked)
  const started = await waitFor(() => h.getEventsStarted(), 75);
  if (!started) {
    fail(name, 'events stream never started');
    await h.cleanup();
    await sendPromise;
    return;
  }

  const session = h.state.sessions[0];
  if (!session) {
    fail(name, 'no session created');
    await h.cleanup();
    await sendPromise;
    return;
  }

  // Simulate "New Chat" — detach stream and switch to draft mode
  h.app.detachResponseStream();
  h.state.activeSessionId = '';
  h.state.draftSessionActive = true;

  // Wait for sendMessage to settle
  await sendPromise;

  if (h.state.streaming) {
    fail(name, 'state.streaming should be false after New Chat, but it is true');
    await h.cleanup();
    return;
  }

  // Verify sending again creates a fresh session (not an interrupt on the old one).
  // Session creation is synchronous inside sendMessage (before the first await),
  // so we can check it immediately after starting the promise.
  h.elements.promptInput.value = 'new message';
  h.closeEventsStream();

  const send2Promise = h.app.sendMessage().catch(() => {});
  // Yield once so the synchronous session-creation part of sendMessage runs.
  await new Promise((resolve) => setTimeout(resolve, 0));

  if (h.state.sessions.length < 2) {
    fail(name, `expected at least 2 sessions after New Chat + send, got ${h.state.sessions.length}`);
    // Detach to let the send settle, then cleanup.
    h.app.detachResponseStream();
    await send2Promise;
    await h.cleanup();
    return;
  }

  const newSession = h.state.sessions[0];
  if (newSession.id === session.id) {
    fail(name, 'second send reused old session instead of creating a new one');
    h.app.detachResponseStream();
    await send2Promise;
    await h.cleanup();
    return;
  }

  // Clean up: detach the second stream so sendMessage settles.
  h.app.detachResponseStream();
  await send2Promise;
  await h.cleanup();
  pass(name);
}

async function testSendMessageMarksSessionBusyImmediately() {
  const name = 'sendMessage marks the session busy before polling catches up';
  const harness = createHarness();
  const { app, elements, state, cleanup } = harness;
  elements.promptInput.value = 'hello';

  const sendPromise = app.sendMessage().catch(() => {});
  const session = state.sessions[0];

  if (!session) {
    fail(name, 'sendMessage should create a session before the first await');
    await cleanup();
    await sendPromise;
    return;
  }

  if (!app.sessionHasInProgressState(session)) {
    fail(name, 'session should be marked busy immediately after send');
    await cleanup();
    await sendPromise;
    return;
  }

  const sawBusyTransition = app.__busyTransitions.some((entry) => entry.id === session.id && entry.busy === true);
  if (!sawBusyTransition) {
    fail(name, 'expected sendMessage to explicitly mark the session busy', JSON.stringify(app.__busyTransitions));
    await cleanup();
    await sendPromise;
    return;
  }

  await sendPromise;
  await cleanup();
  pass(name);
}

async function testDrainInterruptQueueAfterResumeCompletes() {
  const name = 'drainInterruptQueueIfIdle sends queued message after resumeActiveResponse finishes';
  const responseId = 'resp_resume';
  const harness = createHarness({
    responseId,
    eventsBody: [
      'id: 1\n',
      'event: response.created\n',
      `data: {"response":{"id":"${responseId}","model":"test-model","status":"in_progress"},"sequence_number":1}\n\n`,
      'id: 2\n',
      'event: response.completed\n',
      `data: {"response":{"id":"${responseId}","model":"test-model","status":"completed"},"sequence_number":2}\n\n`,
      'data: [DONE]\n\n',
    ].join(''),
  });

  const { app, state, fetchCalls, cleanup } = harness;

  // Set up a session with an active response (simulating a resumed stream).
  const session = {
    id: 'session_resume',
    title: 'Resume test',
    messages: [],
    lastResponseId: null,
    activeResponseId: responseId,
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  // Queue an interrupt that should be drained after the resume completes.
  state.queuedInterrupts.push({ prompt: 'follow-up question', messageId: 'msg_queued' });

  // Run resumeActiveResponse — the events stream will complete immediately.
  await app.resumeActiveResponse(session, { responseId });

  // At this point streaming should be false and activeResponseId cleared.
  if (state.streaming) {
    fail(name, 'state.streaming should be false after resume completes');
    await cleanup();
    return;
  }
  if (session.activeResponseId) {
    fail(name, 'session.activeResponseId should be cleared after response.completed');
    await cleanup();
    return;
  }

  // Simulate what the .then() callback does in app-sessions.js.
  app.drainInterruptQueueIfIdle(session);

  // The queued interrupt should have been shifted off.
  if (state.queuedInterrupts.length !== 0) {
    fail(name, `expected queuedInterrupts to be empty, got ${state.queuedInterrupts.length}`);
    await cleanup();
    return;
  }

  // sendMessage should have been called — look for a POST to /ui/v1/responses.
  const postCalls = fetchCalls.filter(c => c.url === '/ui/v1/responses' && c.method === 'POST');
  if (postCalls.length < 1) {
    fail(name, 'expected sendMessage to POST /ui/v1/responses for the queued interrupt', JSON.stringify(fetchCalls));
    await cleanup();
    return;
  }

  // Clean up the second sendMessage's stream.
  app.detachResponseStream();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await cleanup();
  pass(name);
}

async function testConnectTokenPreservesSelectedModelAndProviderFromState() {
  const name = 'connectToken preserves in-memory provider/model selection when modal DOM is stale';
  const harness = createHarness();
  const { app, elements, state, fetchCalls, cleanup } = harness;

  state.token = 'token-123';
  state.connected = true;
  state.selectedProvider = 'venice';
  state.selectedModel = 'claude-opus-4';
  elements.authTokenInput.value = 'token-123';

  // Simulate a stale modal DOM (for example, before async model loading has
  // caught up). The live state already reflects the user's real choice.
  elements.providerSelect.value = '';
  elements.modelSelect.value = 'claude-sonnet-4-6';

  await app.connectToken();

  if (state.selectedProvider !== 'venice') {
    fail(name, `selectedProvider = ${JSON.stringify(state.selectedProvider)}, want "venice"`);
    await cleanup();
    return;
  }
  if (state.selectedModel !== 'claude-opus-4') {
    fail(name, `selectedModel = ${JSON.stringify(state.selectedModel)}, want "claude-opus-4"`);
    await cleanup();
    return;
  }

  elements.promptInput.value = 'hello';
  await app.sendMessage();

  const postCall = fetchCalls.find((call) => call.url === '/ui/v1/responses' && call.method === 'POST');
  if (!postCall || !postCall.body) {
    fail(name, 'missing POST /ui/v1/responses body', JSON.stringify(fetchCalls));
    await cleanup();
    return;
  }

  let body;
  try {
    body = JSON.parse(postCall.body);
  } catch (err) {
    fail(name, 'response POST body was not valid JSON', String(err));
    await cleanup();
    return;
  }

  if (body.provider !== 'venice') {
    fail(name, `request provider = ${JSON.stringify(body.provider)}, want "venice"`, postCall.body);
    await cleanup();
    return;
  }
  if (body.model !== 'claude-opus-4') {
    fail(name, `request model = ${JSON.stringify(body.model)}, want "claude-opus-4"`, postCall.body);
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

(async () => {
  await testSendMessageHandsOffToEventsStream();
  await testSendMessageIgnoresPostBodyAfterHandoff();
  await testNewChatDuringStreamingClearsStreamingState();
  await testSendMessageMarksSessionBusyImmediately();
  await testDrainInterruptQueueAfterResumeCompletes();
  await testConnectTokenPreservesSelectedModelAndProviderFromState();

  if (failures > 0) {
    console.error(`\n${failures} test(s) failed`);
    process.exit(1);
  }

  console.log('\nAll tests passed');
  process.exit(0);
})().catch((err) => {
  console.error(err && err.stack ? err.stack : String(err));
  process.exit(1);
});
