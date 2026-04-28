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
  const cancelDelayMs = Math.max(0, Number(options.cancelDelayMs || 0));
  const snapshotStatus = Number.isFinite(Number(options.snapshotStatus)) ? Number(options.snapshotStatus) : 200;
  const snapshotPayload = options.snapshotPayload || {
    id: responseId,
    status: 'in_progress',
    last_sequence_number: 0,
    recovery: {
      sequence_number: 0,
      messages: []
    }
  };
  const eventsStatus = Number.isFinite(Number(options.eventsStatus)) ? Number(options.eventsStatus) : 200;
  const eventsErrorPayload = options.eventsErrorPayload || {
    error: { message: `events failed (${eventsStatus})` }
  };
  let cancelRequested = false;
  let cancelResolve = null;
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
    chipProviderSelect: {
      value: '',
      innerHTML: '',
      appendChild() {},
      addEventListener() {},
    },
    chipModelSelect: {
      value: '',
      innerHTML: '',
      appendChild() {},
      addEventListener() {},
    },
    chipEffortSelect: {
      value: '',
      addEventListener() {},
    },
    chipProviderTrigger: {
      addEventListener() {},
      setAttribute() {},
      removeAttribute() {},
      getBoundingClientRect() { return { left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 }; },
    },
    chipModelTrigger: {
      addEventListener() {},
      setAttribute() {},
      removeAttribute() {},
      getBoundingClientRect() { return { left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 }; },
    },
    chipEffortTrigger: {
      addEventListener() {},
      setAttribute() {},
      removeAttribute() {},
      getBoundingClientRect() { return { left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 }; },
    },
    chipPopover: {
      hidden: true,
      innerHTML: '',
      style: {},
      classList: makeClassList(),
      addEventListener() {},
      appendChild() {},
      contains() { return false; },
      getBoundingClientRect() { return { left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 }; },
    },
    authTokenInput: {
      value: '',
      focus() {},
      select() {},
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
    pendingInterjectionBanner: null,
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
    pendingInterjections: [],
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

  const connectionStates = [];
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
    setConnectionState: (text) => { connectionStates.push(String(text || '')); },
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
    syncTurnActionPanels() {},
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
    addEventListener() {},
    removeEventListener() {},
    innerWidth: 1280,
    innerHeight: 800,
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
    if (url === `/ui/v1/responses/${responseId}`) {
      return new Response(JSON.stringify(snapshotPayload), {
        status: snapshotStatus,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (url === `/ui/v1/responses/${responseId}/cancel`) {
      cancelRequested = true;
      if (cancelDelayMs > 0) {
        await new Promise((resolve) => {
          cancelResolve = resolve;
          setTimeout(resolve, cancelDelayMs);
        });
      }
      return new Response('{}', {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (url.startsWith(`/ui/v1/responses/${responseId}/events?after=`)) {
      getEventsStarted = true;
      const signal = options.signal || null;
      if (eventsStatus !== 200) {
        return new Response(JSON.stringify(eventsErrorPayload), {
          status: eventsStatus,
          headers: { 'Content-Type': 'application/json' },
        });
      }
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
    connectionStates,
    getCancelRequested: () => cancelRequested,
    releaseCancel: () => {
      if (cancelResolve) {
        const r = cancelResolve;
        cancelResolve = null;
        r();
      }
    },
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

async function testResumeActiveResponseRecoversFromSnapshotBeforeReplaying() {
  const name = 'resumeActiveResponse recovers from snapshot before replaying tool events';
  const responseId = 'resp_recover';
  const harness = createHarness({
    responseId,
    snapshotPayload: {
      id: responseId,
      status: 'in_progress',
      last_sequence_number: 4,
      recovery: {
        sequence_number: 4,
        messages: [
          {
            id: `${responseId}_tool_group_1`,
            role: 'tool-group',
            created: 1001,
            status: 'running',
            tools: [
              { id: 'call_1', name: 'read_file', arguments: '{"path":"a.txt"}', status: 'done', created: 1001 },
              { id: 'call_2', name: 'grep', arguments: '{"pattern":"needle"}', status: 'running', created: 1002 },
            ],
          },
        ],
      },
    },
    eventsBody: [
      'id: 5\n',
      'event: response.output_item.added\n',
      'data: {"item":{"type":"function_call","call_id":"call_3","name":"glob","arguments":"{\\"pattern\\":\\"**/*.go\\"}"},"sequence_number":5}\n\n',
      'id: 6\n',
      'event: response.output_item.done\n',
      'data: {"item":{"type":"function_call","call_id":"call_3","name":"glob","arguments":"{\\"pattern\\":\\"**/*.go\\"}"},"sequence_number":6}\n\n',
      'id: 7\n',
      'event: response.tool_exec.end\n',
      'data: {"call_id":"call_3","sequence_number":7}\n\n',
      'id: 8\n',
      'event: response.completed\n',
      `data: {"response":{"id":"${responseId}","model":"test-model","status":"completed"},"sequence_number":8}\n\n`,
      'data: [DONE]\n\n',
    ].join(''),
  });

  const { app, state, fetchCalls, cleanup } = harness;

  const session = {
    id: 'session_recover',
    title: 'Recover test',
    messages: [
      { id: 'msg_user_local', role: 'user', content: 'find files', created: 1000 },
      {
        id: 'msg_tool_group_local',
        role: 'tool-group',
        created: 1001,
        status: 'done',
        expanded: false,
        tools: [
          { id: 'call_1', name: 'read_file', arguments: '{"path":"a.txt"}', status: 'done', created: 1001 },
          { id: 'call_2', name: 'grep', arguments: '{"pattern":"needle"}', status: 'done', created: 1002 },
        ],
      },
    ],
    lastResponseId: null,
    activeResponseId: responseId,
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  await app.resumeActiveResponse(session, { responseId, recoverFromSnapshot: true });

  const snapshotCall = fetchCalls.find((call) => call.url === `/ui/v1/responses/${responseId}`);
  if (!snapshotCall) {
    fail(name, 'expected resumeActiveResponse to fetch the response snapshot first', JSON.stringify(fetchCalls));
    await cleanup();
    return;
  }

  const eventsCall = fetchCalls.find((call) => call.url.startsWith(`/ui/v1/responses/${responseId}/events?after=`));
  if (!eventsCall || !eventsCall.url.endsWith('after=4')) {
    fail(name, 'expected replay subscription to start after the recovered sequence number', eventsCall ? eventsCall.url : JSON.stringify(fetchCalls));
    await cleanup();
    return;
  }

  const toolGroups = session.messages.filter((message) => message.role === 'tool-group');
  if (toolGroups.length !== 1) {
    fail(name, `expected exactly 1 tool group after recovery, got ${toolGroups.length}`, JSON.stringify(toolGroups));
    await cleanup();
    return;
  }
  if (toolGroups[0].tools.length !== 3) {
    fail(name, `expected recovered tool group to contain 3 tools, got ${toolGroups[0].tools.length}`, JSON.stringify(toolGroups[0]));
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testResumeActiveResponseFallsBackToReplayWhenSnapshotUnavailable() {
  const name = 'resumeActiveResponse falls back to event replay when snapshot fetch fails';
  const responseId = 'resp_snapshot_fallback';
  const harness = createHarness({
    responseId,
    snapshotStatus: 500,
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

  const session = {
    id: 'session_snapshot_fallback',
    title: 'Snapshot fallback',
    messages: [],
    lastResponseId: null,
    activeResponseId: responseId,
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  await app.resumeActiveResponse(session, { responseId, recoverFromSnapshot: true });

  const snapshotCall = fetchCalls.find((call) => call.url === `/ui/v1/responses/${responseId}`);
  if (!snapshotCall) {
    fail(name, 'expected snapshot fetch attempt before falling back', JSON.stringify(fetchCalls));
    await cleanup();
    return;
  }

  const eventsCall = fetchCalls.find((call) => call.url.startsWith(`/ui/v1/responses/${responseId}/events?after=`));
  if (!eventsCall || !eventsCall.url.endsWith('after=0')) {
    fail(name, 'expected event replay fallback to resume from the existing sequence number', eventsCall ? eventsCall.url : JSON.stringify(fetchCalls));
    await cleanup();
    return;
  }

  if (session.activeResponseId) {
    fail(name, 'session.activeResponseId should clear after fallback replay completes', JSON.stringify(session));
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testResumeActiveResponseClearsTerminalTrackingWhen409SnapshotHasNoRecovery() {
  const name = 'resumeActiveResponse clears tracking when 409 snapshot is terminal without recovery';
  const responseId = 'resp_terminal_snapshot';
  const harness = createHarness({
    responseId,
    eventsStatus: 409,
    snapshotPayload: {
      id: responseId,
      status: 'completed',
      last_sequence_number: 5,
    },
  });

  const { app, state, fetchCalls, cleanup } = harness;

  const session = {
    id: 'session_terminal_snapshot',
    title: 'Terminal snapshot',
    messages: [],
    lastResponseId: null,
    activeResponseId: responseId,
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  await app.resumeActiveResponse(session, { responseId });

  const snapshotCall = fetchCalls.find((call) => call.url === `/ui/v1/responses/${responseId}`);
  if (!snapshotCall) {
    fail(name, 'expected 409 recovery path to fetch response snapshot', JSON.stringify(fetchCalls));
    await cleanup();
    return;
  }

  if (session.activeResponseId) {
    fail(name, 'session.activeResponseId should be cleared by terminal snapshot recovery', JSON.stringify(session));
    await cleanup();
    return;
  }
  if (session.lastResponseId !== responseId) {
    fail(name, `session.lastResponseId = ${JSON.stringify(session.lastResponseId)}, want ${JSON.stringify(responseId)}`);
    await cleanup();
    return;
  }

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

async function testCancelActiveResponseTearsDownLocallyBeforeServerPost() {
  const name = 'cancelActiveResponse aborts local stream and shows Cancelling pill before /cancel POST returns';
  const harness = createHarness({ cancelDelayMs: 60000 });
  const { app, state, connectionStates, getCancelRequested, releaseCancel, cleanup } = harness;

  const controller = new AbortController();
  state.abortController = controller;
  state.currentStreamResponseId = 'resp_test';
  const session = { id: 'session_1', activeResponseId: 'resp_test' };
  state.sessions = [session];
  state.activeSessionId = session.id;
  app.scheduleSessionStatePoll = () => {};
  app.syncActiveSessionFromServer = async () => {};
  app.refreshSessionFromServerTruth = async () => {};

  const cancelPromise = app.cancelActiveResponse(session);

  if (!controller.signal.aborted) {
    fail(name, 'abortController was not aborted synchronously');
    releaseCancel();
    await cancelPromise.catch(() => {});
    await cleanup();
    return;
  }

  if (!connectionStates.includes('Cancelling\u2026')) {
    fail(name, 'expected "Cancelling\u2026" connection state after click', JSON.stringify(connectionStates));
    releaseCancel();
    await cancelPromise.catch(() => {});
    await cleanup();
    return;
  }

  const postStarted = await waitFor(() => getCancelRequested(), 75);
  if (!postStarted) {
    fail(name, 'cancel POST was never issued');
    releaseCancel();
    await cancelPromise.catch(() => {});
    await cleanup();
    return;
  }

  releaseCancel();
  await cancelPromise;
  await cleanup();
  pass(name);
}

async function testInterjectionClosesToolGroupAndInsertsUserMessageAtTail() {
  const name = 'response.interjection closes tool group and inserts user message at DOM tail';
  const harness = createHarness();
  const { app, state, cleanup } = harness;

  const session = {
    id: 'session_interject',
    title: 'Interject test',
    messages: [],
    lastResponseId: null,
    activeResponseId: 'resp_int',
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  state.pendingInterjections = [
    { sessionId: session.id, prompt: 'please also check X', messageId: 'msg_pending', action: 'interject' },
  ];
  state.pendingInterruptCommits = [
    { sessionId: session.id, prompt: 'please also check X', messageId: 'msg_pending' },
  ];

  const streamState = app.createResponseStreamState(session);
  const fakeToolGroup = { id: 'grp_1', role: 'tool-group', tools: [], status: 'running' };
  streamState.currentToolGroup = fakeToolGroup;
  streamState.currentAssistantMessage = null;

  app.applyResponseStreamEvent(session, streamState, 'response.interjection', {
    text: 'please also check X',
  });

  if (streamState.currentToolGroup) {
    fail(name, 'streamState.currentToolGroup should be null after interjection', JSON.stringify(streamState.currentToolGroup));
    await cleanup();
    return;
  }

  const userMessages = session.messages.filter((m) => m.role === 'user');
  if (userMessages.length !== 1) {
    fail(name, `expected 1 user message, got ${userMessages.length}`);
    await cleanup();
    return;
  }
  if (userMessages[0].id !== 'msg_pending') {
    fail(name, `user message id = ${userMessages[0].id}, want "msg_pending"`);
    await cleanup();
    return;
  }
  if (userMessages[0].interruptState !== 'interject') {
    fail(name, `interruptState = ${userMessages[0].interruptState}, want "interject"`);
    await cleanup();
    return;
  }
  if (state.pendingInterjections.length !== 0) {
    fail(name, 'pendingInterjections should be drained after matching interjection');
    await cleanup();
    return;
  }
  if (state.pendingInterruptCommits.length !== 0) {
    fail(name, 'pendingInterruptCommits should be drained after matching interjection');
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testRecoverInterruptConflictQueuesWhenRunStillActive() {
  const name = 'recoverInterruptConflict queues follow-up when server still reports active run';
  const harness = createHarness();
  const { app, state, cleanup } = harness;

  const session = {
    id: 'session_409_active',
    title: '409 active',
    messages: [],
    lastResponseId: null,
    activeResponseId: 'resp_still_running',
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  state.pendingInterjections = [
    { sessionId: session.id, prompt: 'late thought', messageId: 'msg_late', action: 'deciding' },
  ];
  state.pendingInterruptCommits = [
    { sessionId: session.id, prompt: 'late thought', messageId: 'msg_late' },
  ];

  app.syncActiveSessionFromServer = async () => ({ active_run: true, active_response_id: 'resp_still_running' });

  const recovered = await app.recoverInterruptConflict(session, 'late thought', 'msg_late');
  if (!recovered) {
    fail(name, 'recoverInterruptConflict returned false');
    await cleanup();
    return;
  }

  if (state.pendingInterjections.length !== 0) {
    fail(name, 'pendingInterjections should be cleared after 409 recovery', JSON.stringify(state.pendingInterjections));
    await cleanup();
    return;
  }
  if (state.pendingInterruptCommits.length !== 0) {
    fail(name, 'pendingInterruptCommits should be cleared after 409 recovery', JSON.stringify(state.pendingInterruptCommits));
    await cleanup();
    return;
  }

  if (state.queuedInterrupts.length !== 1 || state.queuedInterrupts[0].prompt !== 'late thought') {
    fail(name, 'expected follow-up queued for later delivery', JSON.stringify(state.queuedInterrupts));
    await cleanup();
    return;
  }

  const userMessages = session.messages.filter((m) => m.role === 'user');
  if (userMessages.length !== 1 || userMessages[0].id !== 'msg_late') {
    fail(name, 'expected one inline user message with reused id', JSON.stringify(userMessages));
    await cleanup();
    return;
  }
  if (userMessages[0].interruptState !== 'queue') {
    fail(name, `inline message interruptState = ${userMessages[0].interruptState}, want "queue"`);
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testRecoverInterruptConflictClearsPendingWhenRunFinished() {
  const name = 'recoverInterruptConflict clears pending entries without queueing when run is finished';
  const harness = createHarness();
  const { app, state, cleanup } = harness;

  const session = {
    id: 'session_409_idle',
    title: '409 idle',
    messages: [],
    lastResponseId: null,
    activeResponseId: null,
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  state.pendingInterjections = [
    { sessionId: session.id, prompt: 'now please', messageId: 'msg_idle', action: 'deciding' },
  ];
  state.pendingInterruptCommits = [
    { sessionId: session.id, prompt: 'now please', messageId: 'msg_idle' },
  ];

  app.syncActiveSessionFromServer = async () => ({ active_run: false });

  // sendMessage is called internally by recoverInterruptConflict in the
  // idle branch. Force sendMessage to bail out by flipping state.connected —
  // it then calls openAuthModal, which in turn calls app.refreshNotificationUI,
  // so stub that too to avoid a no-DOM crash.
  harness.state.connected = false;
  app.refreshNotificationUI = () => {};

  const recovered = await app.recoverInterruptConflict(session, 'now please', 'msg_idle');

  if (!recovered) {
    fail(name, 'recoverInterruptConflict returned false');
    await cleanup();
    return;
  }

  if (state.pendingInterjections.length !== 0) {
    fail(name, 'pendingInterjections should be cleared', JSON.stringify(state.pendingInterjections));
    await cleanup();
    return;
  }
  if (state.pendingInterruptCommits.length !== 0) {
    fail(name, 'pendingInterruptCommits should be cleared', JSON.stringify(state.pendingInterruptCommits));
    await cleanup();
    return;
  }

  // No inline "queue" message should have been added — the run is finished,
  // so we hand off to sendMessage (not the inline-queue path).
  const inlineQueued = session.messages.find((m) => m.role === 'user' && m.interruptState === 'queue');
  if (inlineQueued) {
    fail(name, 'should not add inline queue message when run is finished', JSON.stringify(inlineQueued));
    await cleanup();
    return;
  }

  if (state.queuedInterrupts.length !== 0) {
    fail(name, 'should not queue follow-up when run is finished', JSON.stringify(state.queuedInterrupts));
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testRunCompletesWithoutInterjectionQueuesOrphan() {
  const name = 'response.completed with orphaned pending interjection queues it as follow-up';
  const harness = createHarness();
  const { app, state, cleanup } = harness;

  const session = {
    id: 'session_orphan',
    title: 'Orphan test',
    messages: [],
    lastResponseId: null,
    activeResponseId: 'resp_orphan',
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  state.pendingInterjections = [
    { sessionId: session.id, prompt: 'dropped thought', messageId: 'msg_orphan', action: 'interject' },
  ];

  const streamState = app.createResponseStreamState(session);
  app.applyResponseStreamEvent(session, streamState, 'response.completed', {
    response: { id: 'resp_orphan', model: 'test', status: 'completed' },
    sequence_number: 5,
  });

  if (state.pendingInterjections.length !== 0) {
    fail(name, 'pendingInterjections should be drained on run completion');
    await cleanup();
    return;
  }
  if (state.queuedInterrupts.length !== 1) {
    fail(name, `expected 1 queued interrupt, got ${state.queuedInterrupts.length}`);
    await cleanup();
    return;
  }
  if (state.queuedInterrupts[0].prompt !== 'dropped thought') {
    fail(name, `queuedInterrupts[0].prompt = ${state.queuedInterrupts[0].prompt}, want "dropped thought"`);
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testFunctionCallArgumentDeltasFillToolPrompt() {
  const name = 'function_call_arguments.delta fills image prompt before tool finishes';
  const harness = createHarness();
  const { app, state, cleanup } = harness;

  const session = {
    id: 'session_arg_delta',
    title: 'arg delta',
    messages: [],
    lastResponseId: null,
    activeResponseId: 'resp_delta',
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  const streamState = app.createResponseStreamState(session);
  app.applyResponseStreamEvent(session, streamState, 'response.output_item.added', {
    output_index: 2,
    item: { type: 'function_call', call_id: 'call_img', name: 'image_generate' },
  });
  app.applyResponseStreamEvent(session, streamState, 'response.function_call_arguments.delta', {
    output_index: 2,
    delta: '{"aspect_ratio":"4:3","input_image":"/root/.local/share/term-llm/uploads/image.png",',
  });
  app.applyResponseStreamEvent(session, streamState, 'response.function_call_arguments.delta', {
    output_index: 2,
    delta: '"prompt":"turn this sketch into watercolor"}',
  });

  const group = session.messages.find((message) => message.role === 'tool-group');
  const tool = group && group.tools && group.tools[0];
  if (!tool || !String(tool.arguments || '').includes('turn this sketch into watercolor')) {
    fail(name, 'tool arguments did not accumulate prompt deltas', JSON.stringify(tool));
    await cleanup();
    return;
  }

  let parsed;
  try {
    parsed = JSON.parse(tool.arguments);
  } catch (err) {
    fail(name, 'accumulated tool arguments should be valid JSON', String(err));
    await cleanup();
    return;
  }
  if (parsed.prompt !== 'turn this sketch into watercolor') {
    fail(name, 'accumulated arguments should include prompt', JSON.stringify(parsed));
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testArgumentDeltaWithoutOutputIndexUsesLastRunningTool() {
  const name = 'function_call_arguments.delta without output_index uses last running tool';
  const harness = createHarness();
  const { app, state, cleanup } = harness;

  const session = {
    id: 'session_arg_delta_no_index',
    title: 'arg delta no index',
    messages: [],
    lastResponseId: null,
    activeResponseId: 'resp_delta_no_index',
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  const streamState = app.createResponseStreamState(session);
  app.applyResponseStreamEvent(session, streamState, 'response.output_item.added', {
    item: { type: 'function_call', call_id: 'call_first', name: 'shell' },
  });
  app.applyResponseStreamEvent(session, streamState, 'response.output_item.added', {
    item: { type: 'function_call', call_id: 'call_second', name: 'grep' },
  });
  app.applyResponseStreamEvent(session, streamState, 'response.function_call_arguments.delta', {
    delta: '{"pattern":"needle"}',
  });

  const group = session.messages.find((message) => message.role === 'tool-group');
  const tools = group && group.tools;
  if (!tools || tools.length !== 2) {
    fail(name, 'expected two tools in group', JSON.stringify(group));
    await cleanup();
    return;
  }
  if (tools[0].arguments) {
    fail(name, 'first tool should not receive missing-index delta', JSON.stringify(tools));
    await cleanup();
    return;
  }
  if (tools[1].arguments !== '{"pattern":"needle"}') {
    fail(name, 'second/latest running tool should receive missing-index delta', JSON.stringify(tools));
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
  await testResumeActiveResponseRecoversFromSnapshotBeforeReplaying();
  await testFunctionCallArgumentDeltasFillToolPrompt();
  await testArgumentDeltaWithoutOutputIndexUsesLastRunningTool();
  await testResumeActiveResponseFallsBackToReplayWhenSnapshotUnavailable();
  await testResumeActiveResponseClearsTerminalTrackingWhen409SnapshotHasNoRecovery();
  await testConnectTokenPreservesSelectedModelAndProviderFromState();
  await testCancelActiveResponseTearsDownLocallyBeforeServerPost();
  await testInterjectionClosesToolGroupAndInsertsUserMessageAtTail();
  await testRunCompletesWithoutInterjectionQueuesOrphan();
  await testRecoverInterruptConflictQueuesWhenRunStillActive();
  await testRecoverInterruptConflictClearsPendingWhenRunFinished();

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
