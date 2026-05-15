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
    dataset: {},
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
  const postKeepOpen = Boolean(options.postKeepOpen);
  const postBody = Object.prototype.hasOwnProperty.call(options, 'postBody')
    ? String(options.postBody || '')
    : [
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
    ].join('');
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
  const postStatus = Number.isFinite(Number(options.postStatus)) ? Number(options.postStatus) : 200;
  const postErrorPayload = options.postErrorPayload || { error: { message: `post failed (${postStatus})` } };
  const interruptStatus = Number.isFinite(Number(options.interruptStatus)) ? Number(options.interruptStatus) : 200;
  const interruptPayload = options.interruptPayload || { action: 'queue' };
  const interruptErrorPayload = options.interruptErrorPayload || { error: { message: `interrupt failed (${interruptStatus})` } };
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
    showWidgetsSidebarInput: { checked: true, removeAttribute() {}, setAttribute() {} },
    voiceBtn: null,
    voiceStatus: null,
    pendingInterjectionBanner: null,
    askUserModal: makeNode(),
    askUserModalTitle: { textContent: '' },
    askUserModalSubtitle: { textContent: '' },
    askUserModalBody: makeNode(),
    askUserError: { textContent: '' },
    askUserSubmitBtn: { disabled: false, textContent: 'Continue' },
    askUserCancelBtn: { disabled: false, textContent: 'Dismiss' },
    approvalModal: makeNode(),
    approvalTitle: { textContent: '' },
    approvalPath: { textContent: '' },
    approvalError: { textContent: '' },
    approvalBody: makeNode(),
    approvalApproveBtn: { disabled: false, textContent: 'Approve' },
    approvalDenyBtn: { disabled: false, textContent: 'Deny' },
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
    showWidgetsSidebar: true,
    providers: [],
    lastEventTime: 0,
    expectCanceledRun: false,
  };

  const connectionStates = [];
  let modelSwapUpdateCount = 0;
  const app = {
    UI_PREFIX: '/ui',
    STORAGE_KEYS: {
      selectedProvider: 'selectedProvider',
      selectedModel: 'selectedModel',
      selectedEffort: 'selectedEffort',
      showHiddenSessions: 'showHiddenSessions',
      showWidgetsSidebar: 'showWidgetsSidebar',
      token: 'token',
      draftMessages: 'draftMessages',
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
    updateSessionUsageDisplay(session) {
      if (typeof options.onUpdateSessionUsageDisplay === 'function') {
        options.onUpdateSessionUsageDisplay(session, state.streaming);
      }
    },
    refreshRelativeTimes() {},
    updateAssistantNode() {},
    updateUserNode(message) { if (typeof options.onUpdateUserNode === 'function') options.onUpdateUserNode(message); },
    updateToolNode() {},
    updateToolGroupNode() {},
    createMessageNode(message) { if (typeof options.onCreateMessageNode === 'function') options.onCreateMessageNode(message); return makeNode(); },
    createToolGroupNode() { return makeNode(); },
    updateModelSwapNode() { modelSwapUpdateCount += 1; },
    renderSidebar() {},
    renderWidgetSidebar() {},
    renderMessages() {},
    maybeNotifyResponseComplete: async () => {},
    enqueueAssistantStreamUpdate() {},
    finalizeAssistantStreamRender() {},
    syncTurnActionPanels() {},
    subscribeToPush() {},
    shouldAutoSubscribeToPush() { return false; },
    applyTextDirection() {},
    shouldSuppressPromptAutoFocus() { return false; },
    syncActiveSessionFromServer: async (session, pollOnActive, opts) => {
      if (typeof options.onSyncActiveSessionFromServer === 'function') {
        return options.onSyncActiveSessionFromServer(session, pollOnActive, opts);
      }
      return {};
    },
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
  const fileReaderClass = options.FileReader || class {};
  const urlAPI = options.urlAPI || URL;
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
    URL: urlAPI,
    URLSearchParams,
    Blob,
    performance: { now: () => Date.now() },
    navigator: { mediaDevices: null },
    MediaRecorder: undefined,
    FileReader: fileReaderClass,
    alert(message) { if (typeof options.onAlert === 'function') options.onAlert(message); },
    crypto: webcrypto,
  };
  context.globalThis = context;
  windowObj.document = document;
  windowObj.localStorage = localStorage;
  windowObj.URL = urlAPI;
  windowObj.fetch = async function fetch(url, options = {}) {
    fetchCalls.push({
      url,
      method: options.method || 'GET',
      body: typeof options.body === 'string' ? options.body : null,
    });
    if (url.includes('/ui/v1/sessions/') && url.endsWith('/interrupt')) {
      if (interruptStatus !== 200) {
        return new Response(JSON.stringify(interruptErrorPayload), {
          status: interruptStatus,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      return new Response(JSON.stringify(interruptPayload), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (url === '/ui/v1/responses' && (options.method || 'GET') === 'POST') {
      if (postStatus !== 200) {
        return new Response(JSON.stringify(postErrorPayload), {
          status: postStatus,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      const signal = options.signal || null;
      return new Response(new ReadableStream({
        start(controller) {
          postStreamController = controller;
          if (postBody) {
            controller.enqueue(encoder.encode(postBody));
          }
          if (!postKeepOpen) {
            controller.close();
          } else if (signal) {
            signal.addEventListener('abort', () => {
              try { controller.error(new DOMException('The operation was aborted.', 'AbortError')); } catch (_e) { /* ignore */ }
            });
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
    getModelSwapUpdateCount: () => modelSwapUpdateCount,
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

async function testInactiveSessionStreamEventsDoNotAppendToVisibleDOM() {
  const name = 'stream events for inactive session update data without appending to visible DOM';
  const harness = createHarness();
  const { app, state, elements, cleanup } = harness;

  const sessionA = {
    id: 'session_a',
    title: 'A',
    messages: [],
    lastResponseId: null,
    activeResponseId: 'resp_a',
    lastSequenceNumber: 0,
    number: 1,
  };
  const sessionB = {
    id: 'session_b',
    title: 'B',
    messages: [],
    lastResponseId: null,
    activeResponseId: null,
    lastSequenceNumber: 0,
    number: 2,
  };
  state.sessions.push(sessionA, sessionB);
  state.activeSessionId = sessionB.id;
  state.currentStreamSessionId = '';
  state.currentStreamResponseId = '';

  const streamState = app.createResponseStreamState(sessionA);
  app.applyResponseStreamEvent(sessionA, streamState, 'response.output_text.delta', {
    delta: 'leaked assistant text',
    sequence_number: 1,
  });
  app.applyResponseStreamEvent(sessionA, streamState, 'response.output_item.added', {
    item: { type: 'function_call', call_id: 'call_leak', name: 'read_file', arguments: '{"path":"secret"}' },
    sequence_number: 2,
  });

  const assistant = sessionA.messages.find((message) => message.role === 'assistant');
  const toolGroup = sessionA.messages.find((message) => message.role === 'tool-group');
  if (!assistant || assistant.content !== 'leaked assistant text') {
    fail(name, 'inactive session assistant data was not preserved', JSON.stringify(sessionA.messages));
    await cleanup();
    return;
  }
  if (!toolGroup || toolGroup.tools.length !== 1) {
    fail(name, 'inactive session tool data was not preserved', JSON.stringify(sessionA.messages));
    await cleanup();
    return;
  }
  if (elements.messages.children.length !== 0) {
    fail(name, `expected visible DOM to stay empty, got ${elements.messages.children.length} appended nodes`);
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testInactiveSessionPromptEventsRemainActionable() {
  const name = 'inactive ask-user and approval prompt events still create actionable modal state';
  const harness = createHarness();
  const { app, state, cleanup } = harness;

  const sessionA = { id: 'session_prompt_a', title: 'A', messages: [], activeResponseId: 'resp_prompt', lastSequenceNumber: 0, number: 1 };
  const sessionB = { id: 'session_prompt_b', title: 'B', messages: [], activeResponseId: null, lastSequenceNumber: 0, number: 2 };
  state.sessions.push(sessionA, sessionB);
  state.activeSessionId = sessionB.id;

  let streamState = app.createResponseStreamState(sessionA);
  app.applyResponseStreamEvent(sessionA, streamState, 'response.ask_user.prompt', {
    call_id: 'call_question',
    questions: [{ question: 'Pick one', options: [{ label: 'A', description: 'Option A' }, { label: 'B', description: 'Option B' }] }],
    sequence_number: 7,
  });

  if (!state.askUser || state.askUser.sessionId !== sessionA.id || state.askUser.callId !== 'call_question') {
    fail(name, 'inactive ask-user prompt did not create modal state', JSON.stringify(state.askUser));
    await cleanup();
    return;
  }
  if (sessionA.lastSequenceNumber !== 7) {
    fail(name, `ask-user sequence not recorded, got ${sessionA.lastSequenceNumber}`);
    await cleanup();
    return;
  }

  streamState = app.createResponseStreamState(sessionA);
  app.applyResponseStreamEvent(sessionA, streamState, 'response.approval.prompt', {
    approval_id: 'approval_1',
    title: 'Approve tool',
    path: '/tmp/file',
    options: [{ index: 0, label: 'Allow', choice: 'allow' }, { index: 1, label: 'Deny', choice: 'deny' }],
    sequence_number: 8,
  });

  if (!state.approval || state.approval.sessionId !== sessionA.id || state.approval.approvalId !== 'approval_1') {
    fail(name, 'inactive approval prompt did not create modal state', JSON.stringify(state.approval));
    await cleanup();
    return;
  }
  if (sessionA.lastSequenceNumber !== 8) {
    fail(name, `approval sequence not recorded, got ${sessionA.lastSequenceNumber}`);
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testInactiveSessionFailureDoesNotAppendToVisibleDOM() {
  const name = 'response.failed for inactive session stores error without appending to visible DOM';
  const harness = createHarness();
  const { app, state, elements, cleanup } = harness;

  const sessionA = { id: 'session_fail_a', title: 'A', messages: [], activeResponseId: 'resp_fail', lastSequenceNumber: 0, number: 1 };
  const sessionB = { id: 'session_fail_b', title: 'B', messages: [], activeResponseId: null, lastSequenceNumber: 0, number: 2 };
  state.sessions.push(sessionA, sessionB);
  state.activeSessionId = sessionB.id;

  const streamState = app.createResponseStreamState(sessionA);
  app.applyResponseStreamEvent(sessionA, streamState, 'response.failed', {
    error: { message: 'tool exploded' },
    sequence_number: 3,
  });

  const error = sessionA.messages.find((message) => message.role === 'error');
  if (!error || error.content !== 'tool exploded') {
    fail(name, 'inactive failure error was not preserved on the owning session', JSON.stringify(sessionA.messages));
    await cleanup();
    return;
  }
  if (elements.messages.children.length !== 0) {
    fail(name, `expected visible DOM to stay empty, got ${elements.messages.children.length} appended nodes`);
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testConsumeResponseStreamReportsStaleWithoutApplyingEvents() {
  const name = 'consumeResponseStream reports stale streams without applying events';
  const harness = createHarness();
  const { app, state, cleanup } = harness;
  const encoder = new TextEncoder();

  const session = { id: 'session_stale', title: 'Stale', messages: [], activeResponseId: 'resp_stale', lastSequenceNumber: 0, number: 1 };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  state.currentStreamSessionId = 'other_session';
  state.currentStreamResponseId = 'resp_stale';

  const stream = new ReadableStream({
    start(controller) {
      controller.enqueue(encoder.encode('event: response.output_text.delta\ndata: {"delta":"should not apply","sequence_number":1}\n\n'));
      controller.close();
    },
  });

  const result = await app.consumeResponseStream(stream, session, app.createResponseStreamState(session), {
    generation: state.streamGeneration,
    responseId: 'resp_stale',
  });

  if (!result || result.stale !== true || result.terminal !== false) {
    fail(name, 'expected stale non-terminal result', JSON.stringify(result));
    await cleanup();
    return;
  }
  if (session.messages.length !== 0 || session.lastSequenceNumber !== 0) {
    fail(name, 'stale stream should not apply events to the session', JSON.stringify(session));
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testSendMessageDoesNotResumeAfterStalePostStream() {
  const name = 'sendMessage does not resume a response after POST stream becomes stale';
  let harness;
  harness = createHarness({
    postBody: [
      'id: 1\n',
      'event: response.created\n',
      'data: {"response":{"id":"resp_test","model":"test-model","status":"in_progress"},"sequence_number":1}\n\n',
      'id: 2\n',
      'event: response.output_text.delta\n',
      'data: {"delta":"hello","sequence_number":2}\n\n',
    ].join(''),
    onCreateMessageNode(message) {
      if (message.role === 'assistant') harness.app.detachResponseStream();
    },
  });
  const { app, elements, fetchCalls, cleanup } = harness;
  elements.promptInput.value = 'hello';

  await app.sendMessage().catch(() => {});
  await cleanup();

  const eventCalls = fetchCalls.filter((call) => call.url.includes('/events?after='));
  if (eventCalls.length !== 0) {
    fail(name, 'stale POST stream should not be resumed via /events', JSON.stringify(fetchCalls));
    return;
  }

  pass(name);
}

async function testSendMessageRefreshesContinuationIdBeforePosting() {
  const name = 'sendMessage refreshes stale continuation id before posting';
  const staleId = 'resp_msg_796607';
  const latestId = 'resp_msg_796651';
  let syncCalls = 0;
  const harness = createHarness({
    onSyncActiveSessionFromServer(session) {
      syncCalls += 1;
      if (session.lastResponseId === staleId) {
        session.lastResponseId = latestId;
      }
      return { active_run: false, lastResponseId: session.lastResponseId };
    },
  });
  const { app, elements, state, fetchCalls, cleanup } = harness;
  const session = {
    id: 'session_stale_previous',
    title: 'Stale previous',
    messages: [{ id: 'msg_old', role: 'user', content: 'old', created: Date.now() - 1000 }],
    lastResponseId: staleId,
    activeResponseId: null,
    lastSequenceNumber: 0,
    number: 42,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  elements.promptInput.value = 'follow up';

  let sendErr = null;
  await app.sendMessage().catch((err) => {
    sendErr = err;
  });
  await cleanup();

  if (sendErr) {
    fail(name, 'sendMessage rejected unexpectedly', String(sendErr));
    return;
  }
  if (syncCalls !== 1) {
    fail(name, `expected one preflight sync, got ${syncCalls}`);
    return;
  }
  const postCall = fetchCalls.find((call) => call.url === '/ui/v1/responses' && call.method === 'POST');
  if (!postCall || !postCall.body) {
    fail(name, 'missing POST /ui/v1/responses body', JSON.stringify(fetchCalls));
    return;
  }
  const body = JSON.parse(postCall.body);
  if (body.previous_response_id !== latestId) {
    fail(name, `previous_response_id = ${JSON.stringify(body.previous_response_id)}, want ${JSON.stringify(latestId)}`, postCall.body);
    return;
  }

  pass(name);
}

async function testSendMessageConsumesPostStreamWhenAvailable() {
  const name = 'sendMessage consumes the original POST SSE stream when it is available';
  const harness = createHarness();
  const { app, elements, fetchCalls, postStreamCanceled, cleanup } = harness;
  elements.promptInput.value = 'hello';

  let sendErr = null;
  await app.sendMessage().catch((err) => {
    sendErr = err;
  });
  await cleanup();

  if (sendErr) {
    fail(name, 'sendMessage rejected unexpectedly', String(sendErr));
    return;
  }

  const eventCalls = fetchCalls.filter((call) => call.url.includes('/events?after='));
  if (eventCalls.length !== 0) {
    fail(name, 'client should not open /events when the POST stream already completed', JSON.stringify(fetchCalls));
    return;
  }

  if (postStreamCanceled()) {
    fail(name, 'POST body stream should not be canceled during a normal send');
    return;
  }

  const session = harness.state.sessions[0];
  const assistant = session && session.messages.find((message) => message.role === 'assistant');
  if (!assistant || assistant.content !== 'hello') {
    fail(name, 'assistant content did not complete from the POST stream', assistant ? assistant.content : 'missing');
    return;
  }

  pass(name);
}

async function testSendMessageRefreshesHeaderAfterCompletionUnlocksModelPicker() {
  const name = 'sendMessage refreshes header after completion unlocks model picker';
  const streamingStates = [];
  const harness = createHarness({
    onUpdateSessionUsageDisplay(_session, streaming) {
      streamingStates.push(Boolean(streaming));
    },
  });
  const { app, elements, cleanup } = harness;
  elements.promptInput.value = 'hello';

  let sendErr = null;
  await app.sendMessage().catch((err) => {
    sendErr = err;
  });
  await cleanup();

  if (sendErr) {
    fail(name, 'sendMessage rejected unexpectedly', String(sendErr));
    return;
  }
  if (streamingStates.length === 0 || streamingStates[streamingStates.length - 1] !== false) {
    fail(name, 'expected final header refresh after streaming=false', JSON.stringify(streamingStates));
    return;
  }
  pass(name);
}

async function testSendMessageResumesFromEventsAfterPostStreamDrops() {
  const name = 'sendMessage resumes from /events only after the POST stream ends before completion';
  const harness = createHarness({
    postBody: [
      'id: 1\n',
      'event: response.created\n',
      'data: {"response":{"id":"resp_test","model":"test-model","status":"in_progress"},"sequence_number":1}\n\n',
      'id: 2\n',
      'event: response.output_text.delta\n',
      'data: {"delta":"hello","sequence_number":2}\n\n',
    ].join(''),
    eventsBody: [
      'id: 3\n',
      'event: response.output_text.delta\n',
      'data: {"delta":" world","sequence_number":3}\n\n',
      'id: 4\n',
      'event: response.completed\n',
      'data: {"response":{"id":"resp_test","model":"test-model","status":"completed"},"sequence_number":4}\n\n',
      'data: [DONE]\n\n',
    ].join(''),
  });
  const { app, elements, cleanup, fetchCalls, getEventsStarted, postStreamCanceled } = harness;
  elements.promptInput.value = 'hello';

  let sendErr = null;
  const sendPromise = app.sendMessage().catch((err) => {
    sendErr = err;
  });

  const handedOff = await waitFor(() => getEventsStarted(), 75);
  if (!handedOff) {
    fail(name, 'client never reopened the resumable /events stream after the POST stream ended', JSON.stringify(fetchCalls));
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

  const resumeCall = fetchCalls.find((call) => call.url === '/ui/v1/responses/resp_test/events?after=2');
  if (!resumeCall) {
    fail(name, 'expected reconnect to resume after the last POST event instead of replaying from sequence 0', JSON.stringify(fetchCalls));
    return;
  }

  if (postStreamCanceled()) {
    fail(name, 'POST body stream should not be canceled when falling back after an early close');
    return;
  }

  const session = harness.state.sessions[0];
  const assistant = session && session.messages.find((message) => message.role === 'assistant');
  if (!assistant || assistant.content !== 'hello world') {
    fail(name, 'assistant content did not resume correctly after the POST stream ended early', assistant ? assistant.content : 'missing');
    return;
  }

  pass(name);
}

async function testNewChatDuringStreamingClearsStreamingState() {
  const name = 'New Chat during streaming clears streaming state and allows new session';

  const responseId = 'resp_long';
  const h = createHarness({
    responseId,
    postBody: '',
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

  // sendMessage should have been called — look for a POST to the explicit session append endpoint.
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

async function testSendMessageIncludesModelSwapForChangedTarget() {
  const name = 'sendMessage includes model_swap when active session target differs';
  const harness = createHarness();
  const { app, elements, state, fetchCalls, cleanup } = harness;
  const session = {
    id: 'session_swap',
    title: 'Swap test',
    provider: 'old-provider',
    activeModel: 'old-model',
    activeEffort: 'medium',
    messages: [
      { id: 'u1', role: 'user', content: 'hello', created: 1 },
      { id: 'a1', role: 'assistant', content: 'hi', created: 2 },
    ],
    lastResponseId: 'resp_previous',
    activeResponseId: null,
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  state.selectedProvider = 'new-provider';
  state.selectedModel = 'new-model';
  state.selectedEffort = 'high';
  elements.promptInput.value = 'continue';

  await app.sendMessage();
  await cleanup();

  const postCall = fetchCalls.find((call) => call.url === '/ui/v1/responses' && call.method === 'POST');
  if (!postCall || !postCall.body) {
    fail(name, 'missing POST /ui/v1/responses body', JSON.stringify(fetchCalls));
    return;
  }
  const body = JSON.parse(postCall.body);
  if (!body.model_swap || body.model_swap.mode !== 'auto' || body.model_swap.fallback !== 'handover') {
    fail(name, 'expected model_swap auto/handover in request body', postCall.body);
    return;
  }
  if (body.provider !== 'new-provider' || body.model !== 'new-model' || body.reasoning_effort !== 'high') {
    fail(name, 'request did not use selected target runtime', postCall.body);
    return;
  }
  if (body.previous_response_id !== 'resp_previous') {
    fail(name, 'expected previous_response_id when continuing', postCall.body);
    return;
  }
  pass(name);
}

async function testSendMessageOmitsModelSwapWhenTargetUnchanged() {
  const name = 'sendMessage omits model_swap when active session target is unchanged';
  const harness = createHarness();
  const { app, elements, state, fetchCalls, cleanup } = harness;
  const session = {
    id: 'session_no_swap',
    title: 'No swap test',
    provider: 'old-provider',
    activeModel: 'old-model',
    activeEffort: 'medium',
    messages: [
      { id: 'u1', role: 'user', content: 'hello', created: 1 },
      { id: 'a1', role: 'assistant', content: 'hi', created: 2 },
    ],
    lastResponseId: 'resp_previous',
    activeResponseId: null,
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  state.selectedProvider = 'old-provider';
  state.selectedModel = 'old-model';
  state.selectedEffort = 'medium';
  elements.promptInput.value = 'continue';

  await app.sendMessage();
  await cleanup();

  const postCall = fetchCalls.find((call) => call.url === '/ui/v1/responses' && call.method === 'POST');
  if (!postCall || !postCall.body) {
    fail(name, 'missing POST /ui/v1/responses body', JSON.stringify(fetchCalls));
    return;
  }
  const body = JSON.parse(postCall.body);
  if (body.model_swap) {
    fail(name, 'did not expect model_swap for unchanged selection', postCall.body);
    return;
  }
  if (body.provider !== 'old-provider' || body.model !== 'old-model' || body.reasoning_effort !== 'medium') {
    fail(name, 'request should stay pinned to current runtime', postCall.body);
    return;
  }
  pass(name);
}

function testModelSwapProgressEventUpdatesTransientMarker() {
  const name = 'model swap progress event updates transient marker without assistant text';
  const harness = createHarness();
  const { app, state } = harness;
  const session = {
    id: 'session_progress',
    title: 'Progress test',
    messages: [],
    lastResponseId: null,
    activeResponseId: null,
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  const streamState = app.createResponseStreamState(session);

  app.applyResponseStreamEvent(session, streamState, 'response.model_swap.progress', {
    stage: 'naive_start',
    message: 'Switching model: old → new; trying existing context…',
    sequence_number: 1,
  });
  app.applyResponseStreamEvent(session, streamState, 'response.model_swap.progress', {
    stage: 'handover_start',
    message: 'Naive continuation failed; preparing handover…',
    sequence_number: 2,
  });

  const markers = session.messages.filter((message) => message.role === 'model-swap');
  const assistants = session.messages.filter((message) => message.role === 'assistant');
  if (markers.length !== 1) {
    fail(name, `expected one model-swap marker, got ${markers.length}`, JSON.stringify(session.messages));
    return;
  }
  if (!markers[0].transient || markers[0].content !== 'Naive continuation failed; preparing handover…') {
    fail(name, 'progress marker was not updated in place', JSON.stringify(markers[0]));
    return;
  }
  if (assistants.length !== 0) {
    fail(name, 'progress event should not create assistant messages', JSON.stringify(assistants));
    return;
  }
  if (harness.getModelSwapUpdateCount() !== 1) {
    fail(name, `expected updateModelSwapNode to be called once, got ${harness.getModelSwapUpdateCount()}`);
    return;
  }
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
    fail(name, 'session message POST body was not valid JSON', String(err));
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

async function testArgumentDeltasContinueUntilOutputItemDone() {
  const name = 'function_call_arguments.delta continues until output_item.done finalizes args';
  const harness = createHarness();
  const { app, state, cleanup } = harness;

  const session = {
    id: 'session_arg_finalized',
    title: 'arg finalized',
    messages: [],
    lastResponseId: null,
    activeResponseId: 'resp_arg_finalized',
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  const streamState = app.createResponseStreamState(session);
  app.applyResponseStreamEvent(session, streamState, 'response.output_item.added', {
    output_index: 0,
    item: { type: 'function_call', call_id: 'call_json', name: 'write_file' },
  });
  app.applyResponseStreamEvent(session, streamState, 'response.function_call_arguments.delta', {
    output_index: 0,
    delta: '{"path":"a.txt"}',
  });
  app.applyResponseStreamEvent(session, streamState, 'response.function_call_arguments.delta', {
    output_index: 0,
    delta: ',"content":"still append even after valid partial JSON"}',
  });

  const group = session.messages.find((message) => message.role === 'tool-group');
  const tool = group && group.tools && group.tools[0];
  if (!tool || !String(tool.arguments || '').includes('still append')) {
    fail(name, 'delta after a valid JSON prefix should still append before finalization', JSON.stringify(tool));
    await cleanup();
    return;
  }

  app.applyResponseStreamEvent(session, streamState, 'response.output_item.done', {
    item: { type: 'function_call', call_id: 'call_json', name: 'write_file', arguments: '{"path":"final.txt"}' },
  });
  if (!tool.argumentsFinalized) {
    fail(name, 'output_item.done should mark arguments finalized', JSON.stringify(tool));
    await cleanup();
    return;
  }

  app.applyResponseStreamEvent(session, streamState, 'response.function_call_arguments.delta', {
    output_index: 0,
    delta: 'stale replay delta',
  });
  if (tool.arguments !== '{"path":"final.txt"}') {
    fail(name, 'delta after finalization should be ignored', JSON.stringify(tool));
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testSeededToolArgumentsIgnoreReplayDeltas() {
  const name = 'function_call_arguments.delta ignores replay deltas for seeded complete args';
  const harness = createHarness();
  const { app, state, cleanup } = harness;

  const session = {
    id: 'session_seeded_args',
    title: 'seeded args',
    messages: [],
    lastResponseId: null,
    activeResponseId: 'resp_seeded_args',
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  const streamState = app.createResponseStreamState(session);
  app.applyResponseStreamEvent(session, streamState, 'response.output_item.added', {
    output_index: 0,
    item: { type: 'function_call', call_id: 'call_seeded', name: 'grep', arguments: '{"pattern":"needle"}' },
  });
  app.applyResponseStreamEvent(session, streamState, 'response.function_call_arguments.delta', {
    output_index: 0,
    delta: '{"pattern":"needle"}',
  });

  const group = session.messages.find((message) => message.role === 'tool-group');
  const tool = group && group.tools && group.tools[0];
  if (!tool || tool.arguments !== '{"pattern":"needle"}' || !tool.argumentsFinalized) {
    fail(name, 'seeded complete arguments should be retained and marked finalized', JSON.stringify(tool));
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testToolExecImagesAttachToToolArtifactNotAssistantMarkdown() {
  const name = 'tool_exec.end images attach to tool artifact instead of assistant markdown';
  const harness = createHarness();
  const { app, state, cleanup } = harness;

  const session = {
    id: 'session_tool_image_artifact',
    title: 'image artifact',
    messages: [],
    lastResponseId: null,
    activeResponseId: 'resp_tool_image_artifact',
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  const streamState = app.createResponseStreamState(session);
  app.applyResponseStreamEvent(session, streamState, 'response.output_item.added', {
    output_index: 0,
    item: { type: 'function_call', call_id: 'call_img', name: 'image_generate' },
  });
  app.applyResponseStreamEvent(session, streamState, 'response.function_call_arguments.delta', {
    output_index: 0,
    delta: '{"prompt":"paint a cat"}',
  });
  app.applyResponseStreamEvent(session, streamState, 'response.output_item.done', {
    output_index: 0,
    item: { type: 'function_call', call_id: 'call_img', name: 'image_generate', arguments: '{"prompt":"paint a cat"}' },
  });
  app.applyResponseStreamEvent(session, streamState, 'response.tool_exec.end', {
    call_id: 'call_img',
    tool_name: 'image_generate',
    success: true,
    images: ['/ui/images/generated.png'],
  });

  const group = session.messages.find((message) => message.role === 'tool-group');
  const tool = group && group.tools && group.tools[0];
  if (!tool || !Array.isArray(tool.images) || tool.images[0] !== '/ui/images/generated.png') {
    fail(name, 'tool image URL was not stored on tool artifact', JSON.stringify(group));
    await cleanup();
    return;
  }

  const assistant = session.messages.find((message) => message.role === 'assistant');
  if (assistant && String(assistant.content || '').includes('Generated Image')) {
    fail(name, 'image URL should not be injected as assistant markdown', JSON.stringify(assistant));
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testSendMessageLazilyMaterializesAttachmentDataURLs() {
  const name = 'sendMessage lazily materializes attachment data URLs only when sending';
  let readCount = 0;
  const revokedURLs = [];
  const lifecycleEvents = [];

  class MockFileReader {
    constructor() {
      this.result = null;
      this.error = null;
      this.onload = null;
      this.onerror = null;
      this.onabort = null;
    }

    readAsDataURL(file) {
      readCount += 1;
      this.result = file.mockDataURL;
      setTimeout(() => {
        if (this.onload) this.onload();
      }, 0);
    }

    abort() {
      if (this.onabort) this.onabort();
    }
  }

  const harness = createHarness({
    FileReader: MockFileReader,
    onCreateMessageNode(message) {
      if (message?.role !== 'user') return;
      const previewURL = message?.attachments?.[0]?.previewURL || '';
      lifecycleEvents.push(`create:${previewURL}`);
    },
    onUpdateUserNode(message) {
      const previewURL = message?.attachments?.[0]?.previewURL || '';
      lifecycleEvents.push(`update:${previewURL}`);
    },
    urlAPI: {
      createObjectURL(file) {
        return `blob:${file.name}`;
      },
      revokeObjectURL(url) {
        lifecycleEvents.push(`revoke:${url}`);
        revokedURLs.push(url);
      }
    }
  });
  const { app, elements, state, fetchCalls, cleanup } = harness;

  const file = {
    name: 'cat.png',
    type: 'image/png',
    size: 4,
    mockDataURL: 'data:image/png;base64,Y2F0'
  };

  app.handleFiles([file]);

  if (readCount !== 0) {
    fail(name, `expected FileReader reads before send = 0, got ${readCount}`);
    await cleanup();
    return;
  }
  if (state.attachments.length !== 1) {
    fail(name, `expected 1 pending attachment, got ${state.attachments.length}`);
    await cleanup();
    return;
  }
  if (state.attachments[0].dataURL) {
    fail(name, 'pending attachment should not store a dataURL before send', JSON.stringify(state.attachments[0]));
    await cleanup();
    return;
  }
  if (state.attachments[0].previewURL !== 'blob:cat.png') {
    fail(name, `previewURL = ${JSON.stringify(state.attachments[0].previewURL)}, want "blob:cat.png"`);
    await cleanup();
    return;
  }

  elements.promptInput.value = 'describe this image';
  await app.sendMessage();

  if (readCount !== 1) {
    fail(name, `expected FileReader reads after send = 1, got ${readCount}`);
    await cleanup();
    return;
  }
  if (state.attachments.length !== 0) {
    fail(name, `expected pending attachments to be cleared after send, got ${state.attachments.length}`);
    await cleanup();
    return;
  }
  if (revokedURLs.includes('blob:cat.png')) {
    fail(name, 'blob preview URL should remain available for the sent user message', JSON.stringify(revokedURLs));
    await cleanup();
    return;
  }
  if (!lifecycleEvents.includes('create:blob:cat.png')) {
    fail(name, 'expected user message to keep rendering from the existing blob preview URL', JSON.stringify(lifecycleEvents));
    await cleanup();
    return;
  }

  const session = state.sessions[0];
  const userMessage = session && session.messages.find((message) => message.role === 'user');
  const storedAttachment = userMessage && userMessage.attachments && userMessage.attachments[0];
  if (!storedAttachment) {
    fail(name, 'expected stored user attachment after send', JSON.stringify(session));
    await cleanup();
    return;
  }
  if (Object.prototype.hasOwnProperty.call(storedAttachment, 'dataURL')) {
    fail(name, 'stored attachment should not retain the materialized data URL after send', JSON.stringify(storedAttachment));
    await cleanup();
    return;
  }
  if (storedAttachment.previewURL !== 'blob:cat.png') {
    fail(name, `stored attachment previewURL = ${JSON.stringify(storedAttachment.previewURL)}, want "blob:cat.png"`);
    await cleanup();
    return;
  }
  if (!Object.prototype.hasOwnProperty.call(storedAttachment, 'file')) {
    fail(name, 'stored attachment should retain the original File so retries can rebuild attachment input', JSON.stringify(storedAttachment));
    await cleanup();
    return;
  }

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
    fail(name, 'session message POST body was not valid JSON', String(err));
    await cleanup();
    return;
  }

	const parts = body.input?.[0]?.content;
  const imagePart = Array.isArray(parts) ? parts.find((part) => part.type === 'input_image') : null;
  if (!imagePart || imagePart.image_url !== file.mockDataURL) {
    fail(name, 'request body should include lazily materialized image data URL', JSON.stringify(body));
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testSendMessageKeepsComposerWhenAttachmentMaterializationFails() {
  const name = 'sendMessage keeps composer and pending attachments when attachment materialization fails';
  let readCount = 0;
  const revokedURLs = [];
  const alerts = [];

  class MockFileReader {
    constructor() {
      this.result = null;
      this.error = null;
      this.onload = null;
      this.onerror = null;
      this.onabort = null;
    }

    readAsDataURL(file) {
      readCount += 1;
      setTimeout(() => {
        if (file.mockError) {
          this.error = new Error(`cannot read ${file.name}`);
          if (this.onerror) this.onerror();
          return;
        }
        this.result = file.mockDataURL;
        if (this.onload) this.onload();
      }, 0);
    }

    abort() {
      if (this.onabort) this.onabort();
    }
  }

  const harness = createHarness({
    FileReader: MockFileReader,
    onAlert(message) {
      alerts.push(String(message || ''));
    },
    urlAPI: {
      createObjectURL(file) {
        return `blob:${file.name}`;
      },
      revokeObjectURL(url) {
        revokedURLs.push(url);
      }
    }
  });
  const { app, elements, state, fetchCalls, cleanup } = harness;

  const firstFile = {
    name: 'ok.png',
    type: 'image/png',
    size: 4,
    mockDataURL: 'data:image/png;base64,b2s='
  };
  const secondFile = {
    name: 'bad.png',
    type: 'image/png',
    size: 4,
    mockError: true
  };

  app.handleFiles([firstFile, secondFile]);
  elements.promptInput.value = 'please inspect these';
  await app.sendMessage();

  if (readCount !== 2) {
    fail(name, `expected FileReader reads = 2, got ${readCount}`);
    await cleanup();
    return;
  }
  if (fetchCalls.some((call) => call.url === '/ui/v1/responses')) {
    fail(name, 'request should not be sent when attachment materialization fails', JSON.stringify(fetchCalls));
    await cleanup();
    return;
  }
  if (state.sessions.length !== 0) {
    fail(name, 'session should not be created when attachment materialization fails', JSON.stringify(state.sessions));
    await cleanup();
    return;
  }
  if (elements.promptInput.value !== 'please inspect these') {
    fail(name, `prompt was not preserved, got ${JSON.stringify(elements.promptInput.value)}`);
    await cleanup();
    return;
  }
  if (state.attachments.length !== 2) {
    fail(name, `expected pending attachments to remain, got ${state.attachments.length}`);
    await cleanup();
    return;
  }
  if (state.attachments[0].previewURL !== 'blob:ok.png' || Object.prototype.hasOwnProperty.call(state.attachments[0], 'dataURL') || !Object.prototype.hasOwnProperty.call(state.attachments[0], 'file')) {
    fail(name, 'successful reads should stay transient when a later attachment fails', JSON.stringify(state.attachments[0]));
    await cleanup();
    return;
  }
  if (state.attachments[1].previewURL !== 'blob:bad.png' || !Object.prototype.hasOwnProperty.call(state.attachments[1], 'file')) {
    fail(name, 'failed attachment should remain pending with its original file/blob preview', JSON.stringify(state.attachments[1]));
    await cleanup();
    return;
  }
  if (revokedURLs.includes('blob:ok.png')) {
    fail(name, 'transient materialization should not revoke the pending attachment preview on failure', JSON.stringify(revokedURLs));
    await cleanup();
    return;
  }
  if (revokedURLs.includes('blob:bad.png')) {
    fail(name, 'blob URL for still-pending failed attachment should not be revoked', JSON.stringify(revokedURLs));
    await cleanup();
    return;
  }
  if (!alerts.some((message) => message.includes('cannot read bad.png'))) {
    fail(name, 'expected read failure to be shown to the user', JSON.stringify(alerts));
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testNonImageAttachmentsDoNotCreatePreviewObjectURLs() {
  const name = 'non-image attachments do not create unused preview object URLs';
  const createdURLs = [];
  const harness = createHarness({
    urlAPI: {
      createObjectURL(file) {
        createdURLs.push(file.name);
        return `blob:${file.name}`;
      },
      revokeObjectURL() {}
    }
  });
  const { app, state, cleanup } = harness;

  app.handleFiles([{ name: 'notes.txt', type: 'text/plain', size: 4 }]);

  if (createdURLs.length !== 0) {
    fail(name, 'text attachment should not get an object URL', JSON.stringify(createdURLs));
    await cleanup();
    return;
  }
  if (state.attachments.length !== 1 || state.attachments[0].previewURL !== '') {
    fail(name, 'text attachment should be stored without preview URL', JSON.stringify(state.attachments));
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testStaleInterrupt404RefreshesAndSendsMessage() {
  const name = 'interrupt 404 for stale active response refreshes state and sends message';
  const harness = createHarness({
    interruptStatus: 404,
    interruptErrorPayload: { error: { message: 'session runtime not found' } },
  });
  const { app, elements, state, fetchCalls, cleanup } = harness;
  const session = {
    id: 'session_stale_interrupt',
    title: 'Stale interrupt',
    messages: [],
    lastResponseId: null,
    activeResponseId: 'resp_stale',
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  state.streaming = true;
  state.currentStreamSessionId = session.id;
  state.currentStreamResponseId = 'resp_stale';
  elements.promptInput.value = 'send after restart';

  let syncCalls = 0;
  app.syncActiveSessionFromServer = async () => {
    syncCalls += 1;
    app.clearActiveResponseTracking(session, session.activeResponseId || state.currentStreamResponseId);
    app.setSessionOptimisticBusy(session, false);
    app.setSessionServerActiveRun(session, false);
    app.setStreaming(false);
    return { active_run: false, active_response_id: '' };
  };

  await app.sendMessage();
  await cleanup();

  if (syncCalls !== 1) {
    fail(name, `expected one server-truth refresh, got ${syncCalls}`);
    return;
  }
  if (session.activeResponseId || state.streaming || state.currentStreamResponseId) {
    fail(name, 'stale active response tracking should be cleared', JSON.stringify({ session, streaming: state.streaming, current: state.currentStreamResponseId }));
    return;
  }

  const interruptCalls = fetchCalls.filter((call) => call.url === `/ui/v1/sessions/${session.id}/interrupt` && call.method === 'POST');
  if (interruptCalls.length !== 1) {
    fail(name, 'expected initial send to attempt interrupt once', JSON.stringify(fetchCalls));
    return;
  }
  const postCalls = fetchCalls.filter((call) => call.url === '/ui/v1/responses' && call.method === 'POST');
  if (postCalls.length !== 1) {
    fail(name, 'expected recovery to POST /ui/v1/responses once', JSON.stringify(fetchCalls));
    return;
  }
  const body = JSON.parse(postCalls[0].body || '{}');
  const content = body.input?.[0]?.content;
  if (content !== 'send after restart') {
    fail(name, 'recovered POST did not preserve prompt', postCalls[0].body);
    return;
  }
  if (elements.promptInput.value !== '') {
    fail(name, 'composer should be clear after recovered send succeeds', elements.promptInput.value);
    return;
  }

  pass(name);
}

async function testFailedSendKeepsSessionDraftAndRestagesComposer() {
  const name = 'failed send keeps a session-bound draft and restores the composer';
  const harness = createHarness({ postStatus: 503, postErrorPayload: { error: { message: 'server unavailable' } } });
  const { app, elements, state, localStorage, cleanup } = harness;
  const session = {
    id: 'session_failed_draft',
    title: 'Draft failure',
    messages: [],
    lastResponseId: null,
    activeResponseId: null,
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  elements.promptInput.value = 'do not lose me';

  await app.sendMessage();
  await cleanup();

  const drafts = JSON.parse(localStorage.getItem('draftMessages') || '[]');
  if (drafts.length !== 1 || drafts[0].sessionId !== session.id || drafts[0].prompt !== 'do not lose me') {
    fail(name, 'expected failed message to remain staged for its session', JSON.stringify(drafts));
    return;
  }
  if (elements.promptInput.value !== 'do not lose me') {
    fail(name, 'composer should be restaged after failed send', elements.promptInput.value);
    return;
  }
  pass(name);
}

async function testSuccessfulSendRemovesOnlyMatchingDraft() {
  const name = 'successful send removes only the acknowledged session draft';
  const harness = createHarness();
  const { app, elements, state, localStorage, cleanup } = harness;
  const session = {
    id: 'session_success_draft',
    title: 'Draft success',
    messages: [],
    lastResponseId: null,
    activeResponseId: null,
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  localStorage.setItem('draftMessages', JSON.stringify([
    { id: 'old_same_session_draft', sessionId: session.id, prompt: 'acknowledged text', created: 1 },
    { id: 'other_draft', sessionId: 'other_session', prompt: 'other text', created: 2 },
  ]));
  elements.promptInput.value = 'acknowledged text';

  await app.sendMessage();
  await cleanup();

  const drafts = JSON.parse(localStorage.getItem('draftMessages') || '[]');
  if (drafts.length !== 1 || drafts[0].id !== 'other_draft') {
    fail(name, 'only unrelated session drafts should remain', JSON.stringify(drafts));
    return;
  }
  pass(name);
}

function testRestoreDraftMessageForSessionIsSessionBound() {
  const name = 'restoreDraftMessageForSession restores only the active session draft';
  const harness = createHarness();
  const { app, elements, localStorage } = harness;
  localStorage.setItem('draftMessages', JSON.stringify([
    { id: 'd1', sessionId: 'session_a', prompt: 'draft A', created: 10 },
    { id: 'd2', sessionId: 'session_b', prompt: 'draft B', created: 20 },
  ]));

  app.restoreDraftMessageForSession('session_a', { replace: true });
  if (elements.promptInput.value !== 'draft A') {
    fail(name, `session_a restored ${JSON.stringify(elements.promptInput.value)}`);
    return;
  }
  app.restoreDraftMessageForSession('session_b', { replace: true });
  if (elements.promptInput.value !== 'draft B') {
    fail(name, `session_b restored ${JSON.stringify(elements.promptInput.value)}`);
    return;
  }
  pass(name);
}

function testRestoreLatestDraftMessageDoesNotCrossSessionBoundary() {
  const name = 'restoreLatestDraftMessage does not restore another session draft';
  const harness = createHarness();
  const { app, elements, state, localStorage } = harness;
  state.activeSessionId = 'session_b';
  elements.promptInput.value = '';
  localStorage.setItem('draftMessages', JSON.stringify([
    { id: 'd1', sessionId: 'session_a', prompt: 'draft A', created: 20 },
  ]));

  const restored = app.restoreLatestDraftMessage();
  if (restored || elements.promptInput.value !== '') {
    fail(name, 'should not restore session_a draft into session_b', JSON.stringify({ restored, value: elements.promptInput.value }));
    return;
  }
  pass(name);
}

(async () => {
  await testInactiveSessionStreamEventsDoNotAppendToVisibleDOM();
  await testInactiveSessionPromptEventsRemainActionable();
  await testInactiveSessionFailureDoesNotAppendToVisibleDOM();
  await testConsumeResponseStreamReportsStaleWithoutApplyingEvents();
  await testSendMessageDoesNotResumeAfterStalePostStream();
  await testSendMessageRefreshesContinuationIdBeforePosting();
  await testSendMessageConsumesPostStreamWhenAvailable();
  await testSendMessageRefreshesHeaderAfterCompletionUnlocksModelPicker();
  await testSendMessageLazilyMaterializesAttachmentDataURLs();
  await testSendMessageKeepsComposerWhenAttachmentMaterializationFails();
  await testStaleInterrupt404RefreshesAndSendsMessage();
  await testFailedSendKeepsSessionDraftAndRestagesComposer();
  await testSuccessfulSendRemovesOnlyMatchingDraft();
  testRestoreDraftMessageForSessionIsSessionBound();
  testRestoreLatestDraftMessageDoesNotCrossSessionBoundary();
  await testNonImageAttachmentsDoNotCreatePreviewObjectURLs();
  await testSendMessageResumesFromEventsAfterPostStreamDrops();
  await testNewChatDuringStreamingClearsStreamingState();
  await testSendMessageMarksSessionBusyImmediately();
  await testDrainInterruptQueueAfterResumeCompletes();
  await testResumeActiveResponseRecoversFromSnapshotBeforeReplaying();
  await testFunctionCallArgumentDeltasFillToolPrompt();
  await testArgumentDeltaWithoutOutputIndexUsesLastRunningTool();
  await testArgumentDeltasContinueUntilOutputItemDone();
  await testSeededToolArgumentsIgnoreReplayDeltas();
  await testToolExecImagesAttachToToolArtifactNotAssistantMarkdown();
  await testResumeActiveResponseFallsBackToReplayWhenSnapshotUnavailable();
  await testResumeActiveResponseClearsTerminalTrackingWhen409SnapshotHasNoRecovery();
  await testSendMessageIncludesModelSwapForChangedTarget();
  await testSendMessageOmitsModelSwapWhenTargetUnchanged();
  testModelSwapProgressEventUpdatesTransientMarker();
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
