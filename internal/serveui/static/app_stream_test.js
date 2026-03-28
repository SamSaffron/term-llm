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
  const fetchCalls = [];
  const responseId = options.responseId || 'resp_test';
  const postBody = String(options.postBody || '');
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
    streaming: false,
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
    providers: [],
    lastEventTime: 0,
    expectCanceledRun: false,
  };

  const app = {
    UI_PREFIX: '/ui',
    STORAGE_KEYS: {},
    state,
    elements,
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
  windowObj.fetch = async function fetch(url, options = {}) {
    fetchCalls.push({ url, method: options.method || 'GET' });
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
      return new Response(new ReadableStream({
        start(controller) {
          controller.enqueue(encoder.encode(eventsBody));
          controller.close();
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
    getEventsStarted: () => getEventsStarted,
    postStreamCanceled: () => postStreamCanceled,
    cleanup: async () => {
      if (postStreamController) {
        try {
          postStreamController.close();
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

(async () => {
  await testSendMessageHandsOffToEventsStream();
  await testSendMessageIgnoresPostBodyAfterHandoff();

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
