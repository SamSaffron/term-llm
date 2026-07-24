#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const { TextEncoder, TextDecoder } = require('util');
const { ReadableStream } = require('stream/web');
const { webcrypto } = require('crypto');

const dir = __dirname;
const attachmentsSource = fs.readFileSync(path.join(dir, 'app-attachments.js'), 'utf8');
const source = fs.readFileSync(path.join(dir, 'app-stream.js'), 'utf8');
const { reconcileToolCallProjection } = require('./transcript-store.js');

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
  const classes = new Set();
  return {
    toggle(name, force) {
      const enabled = typeof force === 'boolean' ? force : !classes.has(name);
      if (enabled) classes.add(name);
      else classes.delete(name);
      return enabled;
    },
    add(name) { classes.add(name); },
    remove(name) { classes.delete(name); },
    contains(name) { return classes.has(name); },
  };
}

function makeNode() {
  const listeners = new Map();
  const node = {
    classList: makeClassList(),
    children: [],
    appendChild(child) {
      this.children.push(child);
      if (child && Object.prototype.hasOwnProperty.call(child, 'value')) this.options.push(child);
      return child;
    },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    setAttribute() {},
    removeAttribute() {},
    remove() {},
    addEventListener(type, handler) {
      if (!listeners.has(type)) listeners.set(type, []);
      listeners.get(type).push(handler);
    },
    async dispatchEvent(event) {
      for (const handler of listeners.get(event?.type) || []) {
        await handler.call(this, event);
      }
      return true;
    },
    focus() {},
    value: '',
    textContent: '',
    disabled: false,
    dataset: {},
    options: [],
  };
  let inner = '';
  Object.defineProperty(node, 'innerHTML', {
    get() { return inner; },
    set(value) {
      inner = String(value || '');
      this.children = [];
      this.options = [];
    },
  });
  return node;
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
    dataset: {},
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
    sendBtn: {
      disabled: false,
      title: '',
      classList: makeClassList(),
      _arrow: { textContent: '↑' },
      setAttribute(name, value) { this[name] = value; },
      querySelector(selector) { return selector === '.arrow' ? this._arrow : null; },
    },
    stopBtn: { classList: makeClassList() },
    connectionState: makeNode(),
    attachmentsStrip: {
      innerHTML: '',
      style: {},
      appendChild() {},
    },
    providerSelect: makeNode(),
    modelSelect: makeNode(),
    effortSelect: makeNode(),
    reasoningModeSelect: makeNode(),
    reasoningModeField: makeNode(),
    chipProviderSelect: makeNode(),
    chipModelSelect: makeNode(),
    chipEffortSelect: makeNode(),
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
    selectedReasoningMode: 'standard',
    modelInfoByID: {},
    showHiddenSessions: false,
    showWidgetsSidebar: true,
    providers: [],
    lastEventTime: 0,
    expectCanceledRun: false,
  };
  let activeSessionIdValue = String(state.activeSessionId || '');
  Object.defineProperty(state, 'activeSessionId', {
    get() { return activeSessionIdValue; },
    set(value) {
      activeSessionIdValue = String(value || '');
      elements.messages.dataset.sessionId = activeSessionIdValue;
    },
    configurable: true,
    enumerable: true,
  });
  state.activeSessionId = activeSessionIdValue;

  const connectionStates = [];
  let providerRetryStatus = null;
  let modelSwapUpdateCount = 0;
  const app = {
    UI_PREFIX: '/ui',
    STORAGE_KEYS: {
      selectedProvider: 'selectedProvider',
      selectedModel: 'selectedModel',
      selectedEffort: 'selectedEffort',
      selectedReasoningMode: 'selectedReasoningMode',
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
    INTERJECTION_PHASE: {
      evaluating: { badge: 'evaluating', banner: 'deciding' },
      queued: { badge: 'pending_interject', banner: 'interject' },
      willQueue: { badge: 'queue', banner: null },
      willCancel: { badge: 'cancel', banner: null },
      committed: { badge: 'interject', banner: null },
      failed: { badge: 'error', banner: null }
    },
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
    isConversationMounted(sessionOrId) {
      const id = typeof sessionOrId === 'object' ? sessionOrId?.id : sessionOrId;
      return Boolean(id && !state.draftSessionActive && state.activeSessionId === id && elements.messages.dataset.sessionId === id);
    },
    conversationDOMFor(sessionOrId) {
      return app.isConversationMounted(sessionOrId) ? elements.messages : null;
    },
    scrollToBottom() {},
    setConnectionState(text, mode = '') {
      connectionStates.push({ source: 'legacy', text: String(text || ''), mode: String(mode || '') });
    },
    // Mirrors production's strict visible session/response ownership rules.
    setProviderRetryStatus(sessionId, responseId, text) {
      const ownerSessionId = String(sessionId || '').trim();
      const ownerResponseId = String(responseId || '').trim();
      const activeSession = state.sessions.find((session) => session.id === state.activeSessionId) || null;
      const visibleResponseId = String(activeSession?.activeResponseId || (
        state.currentStreamSessionId === ownerSessionId ? state.currentStreamResponseId : ''
      ) || '').trim();
      const applied = Boolean(
        ownerSessionId
        && ownerResponseId
        && ownerSessionId === state.activeSessionId
        && !state.draftSessionActive
        && ownerResponseId === visibleResponseId
      );
      connectionStates.push({
        source: 'provider-retry',
        action: 'set',
        sessionId: ownerSessionId,
        responseId: ownerResponseId,
        text: String(text || ''),
        mode: 'retry',
        applied,
      });
      if (applied) {
        providerRetryStatus = {
          sessionId: ownerSessionId,
          responseId: ownerResponseId,
          text: String(text || ''),
          mode: 'retry',
        };
      }
      return applied;
    },
    clearProviderRetryStatus(sessionId, responseId) {
      const ownerSessionId = String(sessionId || '').trim();
      const ownerResponseId = String(responseId || '').trim();
      const applied = Boolean(
        providerRetryStatus
        && providerRetryStatus.sessionId === ownerSessionId
        && providerRetryStatus.responseId === ownerResponseId
      );
      connectionStates.push({
        source: 'provider-retry',
        action: 'clear',
        sessionId: ownerSessionId,
        responseId: ownerResponseId,
        text: '',
        mode: 'retry',
        applied,
      });
      if (applied) providerRetryStatus = null;
      return applied;
    },
    sessionSlug(s) { return s ? s.id : ''; },
    updateURL() {},
    persistAndRefreshShell() {},
    updateHeader() {},
    updateSessionUsageDisplay(session) {
      if (typeof options.onUpdateSessionUsageDisplay === 'function') {
        options.onUpdateSessionUsageDisplay(session, state.streaming);
      }
    },
    splitHeaderModelEffort(model, effort) { return { model: String(model || '').trim(), effort: String(effort || '').trim() }; },
    refreshRelativeTimes() {},
    updateAssistantNode() {},
    updateUserNode(message) { if (typeof options.onUpdateUserNode === 'function') options.onUpdateUserNode(message); },
    updateToolNode() {},
    updateToolGroupNode() {},
    createMessageNode(message) { if (typeof options.onCreateMessageNode === 'function') options.onCreateMessageNode(message); return makeNode(); },
    createToolGroupNode() { return makeNode(); },
    updateModelSwapNode() { modelSwapUpdateCount += 1; },
    updateMountedToolGroupNode(sessionOrId, message) {
      if (!app.isConversationMounted(sessionOrId)) return false;
      app.updateToolGroupNode(message);
      return true;
    },
    updateMountedModelSwapNode(sessionOrId, message) {
      if (!app.isConversationMounted(sessionOrId)) return false;
      app.updateModelSwapNode(message);
      return true;
    },
    updateMountedUserNode(sessionOrId, message) {
      if (!app.isConversationMounted(sessionOrId)) return false;
      app.updateUserNode(message);
      return true;
    },
    enqueueMountedAssistantStreamUpdate(sessionOrId, message) {
      if (!app.isConversationMounted(sessionOrId)) return false;
      app.enqueueAssistantStreamUpdate(message);
      return true;
    },
    finalizeMountedAssistantStreamRender(sessionOrId, message) {
      if (!app.isConversationMounted(sessionOrId)) return false;
      app.finalizeAssistantStreamRender(message);
      return true;
    },
    renderSidebar() {},
    renderWidgetSidebar() {},
    renderMessages() {},
    reconcileSessionToolCallProjection(session) {
      session.messages = reconcileToolCallProjection(session.messages);
      if (typeof options.onReconcileSessionToolCallProjection === 'function') {
        options.onReconcileSessionToolCallProjection(session);
      }
      return true;
    },
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
    refreshCurrentPlanFromServer: async (session) => {
      if (typeof options.onRefreshCurrentPlanFromServer === 'function') {
        return options.onRefreshCurrentPlanFromServer(session);
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
    matchSkillInvocation: typeof options.matchSkillInvocation === 'function'
      ? options.matchSkillInvocation
      : () => null,
  };

  const windowObj = {
    TermLLMApp: app,
    __TERM_LLM_DIAGNOSTICS__: Boolean(options.diagnostics),
    setTimeout: typeof options.setTimeout === 'function' ? options.setTimeout : setTimeout,
    clearTimeout: typeof options.clearTimeout === 'function' ? options.clearTimeout : clearTimeout,
    setInterval: typeof options.setInterval === 'function' ? options.setInterval : setInterval,
    clearInterval: typeof options.clearInterval === 'function' ? options.clearInterval : clearInterval,
    requestAnimationFrame(callback) {
      if (typeof options.requestAnimationFrame === 'function') return options.requestAnimationFrame(callback);
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
    setTimeout: typeof options.setTimeout === 'function' ? options.setTimeout : setTimeout,
    clearTimeout: typeof options.clearTimeout === 'function' ? options.clearTimeout : clearTimeout,
    setInterval: typeof options.setInterval === 'function' ? options.setInterval : setInterval,
    clearInterval: typeof options.clearInterval === 'function' ? options.clearInterval : clearInterval,
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
    CSS: options.CSS,
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
  windowObj.CSS = options.CSS;
  windowObj.fetch = async function fetch(url, requestOptions = {}) {
    fetchCalls.push({
      url,
      method: requestOptions.method || 'GET',
      body: typeof requestOptions.body === 'string' ? requestOptions.body : null,
      headers: requestOptions.headers || null,
    });
    if (typeof options.fetchImpl === 'function') {
      return options.fetchImpl(url, requestOptions, {
        Response,
        ReadableStream,
        Headers,
        TextEncoder,
        TextDecoder,
        encoder,
        responseId,
      });
    }
    if (url.includes('/ui/v1/sessions/') && url.endsWith('/runtime/effort')) {
      const parsedBody = requestOptions.body ? JSON.parse(String(requestOptions.body)) : {};
      return new Response(JSON.stringify({
        status: 'queued',
        model: parsedBody.model || 'test-model',
        reasoning_effort: Object.prototype.hasOwnProperty.call(parsedBody, 'reasoning_effort') ? parsedBody.reasoning_effort : '',
      }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
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
    if (url === '/ui/v1/responses' && (requestOptions.method || 'GET') === 'POST') {
      if (postStatus !== 200) {
        return new Response(JSON.stringify(postErrorPayload), {
          status: postStatus,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      const signal = requestOptions.signal || null;
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
      const signal = requestOptions.signal || null;
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

  // app-attachments.js is a dependency leaf that app-stream.js destructures from
  // the shared app bag at load time, so it must run first (mirrors index.html).
  vm.runInNewContext(attachmentsSource, context, { filename: 'app-attachments.js' });
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
    getProviderRetryStatus: () => providerRetryStatus ? { ...providerRetryStatus } : null,
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

async function testModelEffortOptionsFollowMetadata() {
  const name = 'model effort options follow /v1/models metadata';
  const harness = createHarness({
    fetchImpl: async (url, _requestOptions, { Response }) => {
      if (url === '/ui/v1/models?provider=chatgpt') {
        return new Response(JSON.stringify({
          data: [
            { id: 'gpt-5.5', reasoning_efforts: ['minimal', 'low', 'medium', 'high', 'xhigh'], default_reasoning_effort: 'medium' },
            { id: 'listed-no-metadata' },
            { id: 'opus', reasoning_efforts: ['low', 'medium', 'high', 'xhigh', 'max'] },
          ],
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      throw new Error(`unexpected fetch: ${url}`);
    },
  });
  const { app, state, elements, cleanup } = harness;
  state.selectedProvider = 'chatgpt';
  state.selectedModel = 'gpt-5.5';
  state.selectedEffort = 'max';
  state.models = await app.fetchModels('', 'chatgpt');
  app.renderModelOptions();

  if (state.selectedEffort !== '') {
    fail(name, `invalid max effort was not cleared: ${JSON.stringify(state.selectedEffort)}`);
    await cleanup();
    return;
  }
  if (state.modelInfoByID['gpt-5.5']?.default_reasoning_effort !== 'medium') {
    fail(name, `gpt-5.5 default effort metadata was dropped: ${JSON.stringify(state.modelInfoByID['gpt-5.5'])}`);
    await cleanup();
    return;
  }
  const effortValues = elements.chipEffortSelect.options.map((opt) => opt.value);
  if (effortValues.includes('max')) {
    fail(name, `gpt-5.5 effort options include max: ${JSON.stringify(effortValues)}`);
    await cleanup();
    return;
  }
  for (const want of ['', 'minimal', 'low', 'medium', 'high', 'xhigh']) {
    if (!effortValues.includes(want)) {
      fail(name, `gpt-5.5 effort options missing ${JSON.stringify(want)}: ${JSON.stringify(effortValues)}`);
      await cleanup();
      return;
    }
  }

  state.selectedEffort = 'max';
  app.applyModelChange('listed-no-metadata');
  const fallbackValues = elements.chipEffortSelect.options.map((opt) => opt.value);
  if (state.selectedEffort !== 'max' || !fallbackValues.includes('max')) {
    fail(name, `listed model without efforts should use legacy fallback: state=${JSON.stringify(state.selectedEffort)} options=${JSON.stringify(fallbackValues)}`);
    await cleanup();
    return;
  }

  app.applyModelChange('opus');
  const opusValues = elements.chipEffortSelect.options.map((opt) => opt.value);
  if (!opusValues.includes('max')) {
    fail(name, `opus effort options missing max: ${JSON.stringify(opusValues)}`);
    await cleanup();
    return;
  }
  await cleanup();
  pass(name);
}

async function testResponseCreatedRecordsStartedTranscriptRevision() {
  const name = 'response.created records started_rev before active stream attachment';
  const harness = createHarness();
  const { app, state, cleanup } = harness;
  const calls = [];
  app.noteTranscriptRunCreated = (session, responseId, startedRev) => {
    calls.push({ sessionId: session.id, responseId, startedRev });
  };
  const session = { id: 'session_started_rev', messages: [], activeResponseId: null, lastSequenceNumber: 0 };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  const streamState = app.createResponseStreamState(session);
  app.applyResponseStreamEvent(session, streamState, 'response.created', {
    response: { id: 'resp_started', status: 'in_progress' },
    started_rev: 17,
    sequence_number: 1
  });
  if (calls.length !== 1 || calls[0].responseId !== 'resp_started' || calls[0].startedRev !== 17) {
    fail(name, 'started_rev was not forwarded to transcript attachment', JSON.stringify(calls));
    await cleanup();
    return;
  }
  await cleanup();
  pass(name);
}

async function testResponseCompletedForcesSidebarStatusRefresh() {
  const name = 'response.completed forces a sidebar status refresh';
  const harness = createHarness();
  const { app, state, cleanup } = harness;
  const refreshCalls = [];
  const terminalCalls = [];
  app.refreshSidebarStatusPoll = (forceNow) => {
    refreshCalls.push(forceNow);
  };
  app.noteTranscriptTerminal = (session, finalRev) => {
    terminalCalls.push({ sessionId: session.id, finalRev });
  };

  const session = {
    id: 'session_status_refresh',
    title: 'Status refresh',
    messages: [{ id: 'assistant_1', role: 'assistant', content: 'done', created: 1000 }],
    lastResponseId: null,
    activeResponseId: 'resp_status_refresh',
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  state.currentStreamSessionId = session.id;
  state.currentStreamResponseId = 'resp_status_refresh';

  const streamState = app.createResponseStreamState(session);
  app.applyResponseStreamEvent(session, streamState, 'response.completed', {
    response: { id: 'resp_status_refresh', model: 'test-model', status: 'completed' },
    sequence_number: 3,
    final_rev: 9
  });

  const refreshed = await waitFor(() => refreshCalls.length === 1, 75);
  if (!refreshed || refreshCalls[0] !== true) {
    fail(name, 'expected one forced status refresh after completion', JSON.stringify(refreshCalls));
    await cleanup();
    return;
  }
  if (terminalCalls.length !== 1 || terminalCalls[0].sessionId !== session.id || terminalCalls[0].finalRev !== 9) {
    fail(name, 'terminal event must reconcile the durable transcript at final_rev', JSON.stringify(terminalCalls));
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testResponseCompletedPreservesFailedToolStatus() {
  const name = 'response.completed preserves failed tool status while closing running tools';
  const harness = createHarness();
  const { app, state, cleanup } = harness;
  const failedTool = { id: 'call_failed', name: 'update_plan', status: 'error' };
  const runningTool = { id: 'call_running', name: 'read_file', status: 'running' };
  const toolGroup = {
    id: 'tools_1',
    role: 'tool-group',
    status: 'running',
    tools: [failedTool, runningTool],
    created: 1000,
  };
  const session = {
    id: 'session_failed_tool_status',
    title: 'Failed tool status',
    messages: [toolGroup],
    lastResponseId: null,
    activeResponseId: 'resp_failed_tool_status',
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  state.currentStreamSessionId = session.id;
  state.currentStreamResponseId = session.activeResponseId;

  const streamState = app.createResponseStreamState(session);
  app.applyResponseStreamEvent(session, streamState, 'response.completed', {
    response: { id: session.activeResponseId, model: 'test-model', status: 'completed' },
    sequence_number: 3,
  });

  if (failedTool.status !== 'error') {
    fail(name, `failed tool status = ${JSON.stringify(failedTool.status)}, want "error"`);
    await cleanup();
    return;
  }
  if (runningTool.status !== 'done' || toolGroup.status !== 'done') {
    fail(name, 'running tool group was not closed', JSON.stringify(toolGroup));
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testStaleTerminalStreamDoesNotRefreshStatus() {
  const name = 'stale terminal stream events do not request active transcript refresh';
  const harness = createHarness();
  const { app, state, cleanup } = harness;
  const refreshCalls = [];
  app.refreshSidebarStatusPoll = (forceNow) => {
    refreshCalls.push(forceNow);
  };

  const session = {
    id: 'session_stale_terminal',
    title: 'Stale terminal',
    messages: [],
    lastResponseId: null,
    activeResponseId: 'resp_stale_terminal',
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  state.currentStreamSessionId = 'other_session';
  state.currentStreamResponseId = 'resp_stale_terminal';

  const stream = new ReadableStream({
    start(controller) {
      controller.enqueue(new TextEncoder().encode([
        'event: response.completed\n',
        'data: {"response":{"id":"resp_stale_terminal","model":"test-model","status":"completed"},"sequence_number":9}\n\n',
        'data: [DONE]\n\n',
      ].join('')));
      controller.close();
    },
  });

  const result = await app.consumeResponseStream(stream, session, app.createResponseStreamState(session), {
    generation: state.streamGeneration,
    responseId: 'resp_stale_terminal',
  });

  if (!result || result.stale !== true || result.terminal !== false) {
    fail(name, 'expected stale terminal stream to be ignored before applying terminal event', JSON.stringify(result));
    await cleanup();
    return;
  }
  const unexpectedRefresh = await waitFor(() => refreshCalls.length > 0, 30);
  if (unexpectedRefresh) {
    fail(name, 'stale terminal stream should not force status refresh', JSON.stringify(refreshCalls));
    await cleanup();
    return;
  }
  if (session.lastResponseId) {
    fail(name, 'stale terminal stream should not update session response tracking', JSON.stringify(session));
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testConsumeResponseStreamIgnoresAlreadyProjectedEvents() {
  const name = 'response stream ignores replayed events that would duplicate or reorder messages';
  const harness = createHarness();
  const { app, state, cleanup } = harness;
  const session = {
    id: 'session_replayed_events',
    title: 'Replayed events',
    messages: [],
    lastResponseId: null,
    activeResponseId: 'resp_replayed_events',
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  state.currentStreamSessionId = session.id;
  state.currentStreamResponseId = session.activeResponseId;

  const stream = new ReadableStream({
    start(controller) {
      controller.enqueue(new TextEncoder().encode([
        'event: response.output_text.delta\n',
        'data: {"delta":"first","sequence_number":1}\n\n',
        'event: response.output_text.delta\n',
        'data: {"delta":" duplicate","sequence_number":1}\n\n',
        'event: response.phase\n',
        'data: {"text":"stale phase","sequence_number":1}\n\n',
        'event: response.output_text.delta\n',
        'data: {"delta":" last","sequence_number":2}\n\n',
        'data: [DONE]\n\n',
      ].join('')));
      controller.close();
    },
  });

  await app.consumeResponseStream(stream, session, app.createResponseStreamState(session), {
    generation: state.streamGeneration,
    responseId: session.activeResponseId,
  });

  const projected = session.messages.map((message) => `${message.role}:${message.content || ''}`);
  if (JSON.stringify(projected) !== JSON.stringify(['assistant:first last'])) {
    fail(name, 'replayed sequence numbers changed the projected transcript order', JSON.stringify(projected));
    await cleanup();
    return;
  }
  if (session.lastSequenceNumber !== 2) {
    fail(name, `last sequence = ${session.lastSequenceNumber}, want 2`);
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testConsumeResponseStreamPreservesOverflowRecoverySequenceException() {
  const name = 'response stream applies current overflow recovery but ignores stale overflow replay';
  const harness = createHarness();
  const { app, state, cleanup } = harness;
  const session = {
    id: 'session_overflow_sequence',
    title: 'Overflow sequence',
    messages: [],
    activeResponseId: 'resp_overflow_sequence',
    lastSequenceNumber: 0,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  state.currentStreamSessionId = session.id;
  state.currentStreamResponseId = session.activeResponseId;

  const stream = new ReadableStream({
    start(controller) {
      controller.enqueue(new TextEncoder().encode([
        'event: response.output_text.delta\n',
        'data: {"delta":"before","sequence_number":1}\n\n',
        'event: response.output_text.delta\n',
        'data: {"delta":" overflow","sequence_number":2}\n\n',
        'event: response.stream_error\n',
        'data: {"error":{"type":"stream_buffer_overflow"},"sequence_number":2,"recovery":{"sequence_number":2,"messages":[{"id":"recovered","role":"assistant","content":"recovered"}]}}\n\n',
        'event: response.stream_error\n',
        'data: {"error":{"type":"stream_buffer_overflow"},"sequence_number":1,"recovery":{"sequence_number":1,"messages":[{"id":"stale","role":"assistant","content":"stale"}]}}\n\n',
        'data: [DONE]\n\n',
      ].join('')));
      controller.close();
    },
  });

  await app.consumeResponseStream(stream, session, app.createResponseStreamState(session), {
    generation: state.streamGeneration,
    responseId: session.activeResponseId,
  });

  const projected = session.messages.map((message) => `${message.role}:${message.content || ''}`);
  if (JSON.stringify(projected) !== JSON.stringify(['assistant:recovered'])) {
    fail(name, 'overflow sequence exception applied the wrong recovery snapshot', JSON.stringify(projected));
    await cleanup();
    return;
  }
  if (session.lastSequenceNumber !== 2) {
    fail(name, `last sequence = ${session.lastSequenceNumber}, want 2`);
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testSkippedReplayRehydratesStreamProjectionState() {
  const name = 'skipped replay rehydrates stream state before fresh tool deltas';
  const harness = createHarness();
  const { app, state, cleanup } = harness;
  const session = {
    id: 'session_replay_tool_state',
    title: 'Replay tool state',
    messages: [],
    activeResponseId: 'resp_replay_tool_state',
    lastSequenceNumber: 0,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  state.currentStreamSessionId = session.id;
  state.currentStreamResponseId = session.activeResponseId;

  // This stream attached before an overlapping transport projected the tool.
  const streamState = app.createResponseStreamState(session);
  const projectedState = app.createResponseStreamState(session);
  app.applyResponseStreamEvent(session, projectedState, 'response.output_item.added', {
    sequence_number: 1,
    output_index: 0,
    item: { type: 'function_call', call_id: 'call_replayed', name: 'read_file', arguments: '' },
  });

  const stream = new ReadableStream({
    start(controller) {
      controller.enqueue(new TextEncoder().encode([
        'event: response.output_item.added\n',
        'data: {"sequence_number":1,"output_index":0,"item":{"type":"function_call","call_id":"call_replayed","name":"read_file","arguments":""}}\n\n',
        'event: response.function_call_arguments.delta\n',
        'data: {"sequence_number":2,"output_index":0,"delta":"{\\"path\\":\\"README.md\\"}"}\n\n',
        'data: [DONE]\n\n',
      ].join('')));
      controller.close();
    },
  });

  await app.consumeResponseStream(stream, session, streamState, {
    generation: state.streamGeneration,
    responseId: session.activeResponseId,
  });

  const tools = session.messages.flatMap((message) => message.role === 'tool-group' ? message.tools || [] : []);
  if (tools.length !== 1 || tools[0].arguments !== '{"path":"README.md"}') {
    fail(name, 'fresh tool delta was not applied to the already projected call', JSON.stringify(session.messages));
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
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

async function testInactiveExistingMessageUpdatesDoNotTouchVisibleDOM() {
  const name = 'inactive existing message stream updates do not touch visible DOM';
  let updatedUser = null;
  const harness = createHarness({
    onUpdateUserNode(message) {
      updatedUser = message;
    }
  });
  const { app, state, elements, cleanup } = harness;

  const sessionA = {
    id: 'session_existing_a',
    title: 'A',
    messages: [{ id: 'msg_pending', role: 'user', content: 'interrupt me', created: 1000, interruptState: 'evaluating' }],
    activeResponseId: 'resp_existing',
    lastSequenceNumber: 0,
    number: 1,
  };
  const sessionB = {
    id: 'session_existing_b',
    title: 'B',
    messages: [],
    activeResponseId: null,
    lastSequenceNumber: 0,
    number: 2,
  };
  state.sessions.push(sessionA, sessionB);
  state.activeSessionId = sessionB.id;
  elements.messages.dataset.sessionId = sessionB.id;

  const streamState = app.createResponseStreamState(sessionA);
  app.applyResponseStreamEvent(sessionA, streamState, 'response.interjection', {
    text: 'interrupt me',
    interjection_id: 'msg_pending',
    sequence_number: 1,
  });

  if (updatedUser) {
    fail(name, 'inactive session existing message update reached visible DOM updater', JSON.stringify(updatedUser));
    await cleanup();
    return;
  }
  if (sessionA.messages[0].interruptState !== 'interject') {
    fail(name, 'inactive session data was not updated', JSON.stringify(sessionA.messages[0]));
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

async function testInactiveInterruptHelpersDoNotTouchVisibleDOM() {
  const name = 'inactive interrupt helpers update data without touching visible DOM';
  let updatedUser = null;
  const harness = createHarness({
    onUpdateUserNode(message) {
      updatedUser = message;
    }
  });
  const { app, state, elements, cleanup } = harness;

  const sessionA = {
    id: 'session_interrupt_a',
    title: 'A',
    messages: [{ id: 'msg_interrupt', role: 'user', content: 'wait', created: 1000, interruptState: 'evaluating' }],
    activeResponseId: 'resp_interrupt',
    lastSequenceNumber: 0,
    number: 1,
  };
  const sessionB = { id: 'session_interrupt_b', title: 'B', messages: [], activeResponseId: null, lastSequenceNumber: 0, number: 2 };
  state.sessions.push(sessionA, sessionB);
  state.activeSessionId = sessionB.id;

  app.setInterruptMessageState(sessionA, 'msg_interrupt', 'queue');
  app.addInlineInterruptMessage(sessionA, 'background follow-up', 'msg_background', 'evaluating');

  if (updatedUser) {
    fail(name, 'inactive interrupt update reached visible DOM updater', JSON.stringify(updatedUser));
    await cleanup();
    return;
  }
  if (sessionA.messages[0].interruptState !== 'queue' || sessionA.messages.length !== 2) {
    fail(name, 'inactive session interrupt data was not updated', JSON.stringify(sessionA.messages));
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

async function testParseSSEStreamUpdatesHeartbeatOnCommentFrame() {
  const name = 'parseSSEStream updates heartbeat timestamp on comment keepalive frames';
  const harness = createHarness();
  const { app, state, cleanup } = harness;
  const encoder = new TextEncoder();
  state.lastEventTime = 1;

  const stream = new ReadableStream({
    start(controller) {
      controller.enqueue(encoder.encode(': ping\n\n'));
      controller.close();
    },
  });

  let eventCount = 0;
  await app.parseSSEStream(stream, () => {
    eventCount += 1;
    return true;
  });

  if (state.lastEventTime <= 1) {
    fail(name, `lastEventTime was not refreshed: ${state.lastEventTime}`);
    await cleanup();
    return;
  }
  if (eventCount !== 1) {
    fail(name, `expected one parsed empty comment block, got ${eventCount}`);
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testSendMessageHeartbeatCancelsPostStreamWithoutAbortingFetch() {
  const name = 'heartbeat timeout cancels an attached POST body without aborting fetch';
  const intervalCallbacks = [];
  const harness = createHarness({
    postKeepOpen: true,
    postBody: [
      'id: 1\n',
      'event: response.created\n',
      'data: {"response":{"id":"resp_test","model":"test-model","status":"in_progress"},"sequence_number":1}\n\n',
      'id: 2\n',
      'event: response.output_text.delta\n',
      'data: {"delta":"partial","sequence_number":2}\n\n',
    ].join(''),
    setInterval(callback) {
      intervalCallbacks.push(callback);
      return intervalCallbacks.length;
    },
    clearInterval() {},
  });
  const { app, elements, state, getEventsStarted, postStreamCanceled, cleanup } = harness;
  elements.promptInput.value = 'hello';

  const sendPromise = app.sendMessage();
  const attached = await waitFor(() => state.currentStreamResponseId === 'resp_test' && state.abortController, 1000);
  if (!attached) {
    fail(name, 'POST stream did not attach response tracking');
    await cleanup();
    await sendPromise.catch(() => {});
    return;
  }
  if (intervalCallbacks.length === 0) {
    fail(name, 'heartbeat monitor was not started');
    await cleanup();
    await sendPromise.catch(() => {});
    return;
  }

  const controller = state.abortController;
  const staleThreshold = Math.max(app.HEARTBEAT_STALE_THRESHOLD, Number(state.abortController?._heartbeatStaleThreshold || 0) || 0);
  state.lastEventTime = Date.now() - staleThreshold - 1;
  intervalCallbacks[intervalCallbacks.length - 1]();

  if (controller.signal.aborted) {
    fail(name, 'heartbeat aborted fetch after its response body was attached');
    await cleanup();
    await sendPromise.catch(() => {});
    return;
  }
  if (!postStreamCanceled()) {
    fail(name, 'heartbeat did not cancel the attached response body');
    await cleanup();
    await sendPromise.catch(() => {});
    return;
  }

  const resumed = await waitFor(() => getEventsStarted(), 1000);
  await sendPromise.catch((err) => {
    fail(name, 'sendMessage rejected after heartbeat cancellation', String(err));
  });
  if (controller.signal.aborted) {
    fail(name, 'heartbeat recovery later aborted the attached fetch');
    await cleanup();
    return;
  }
  await cleanup();

  if (!resumed) {
    fail(name, 'heartbeat abort did not resume via /events');
    return;
  }

  pass(name);
}

async function testSendMessageHeartbeatCancellationWithoutResponseIDRetriesPost() {
  const name = 'heartbeat cancellation without a response ID retries the POST';
  const intervalCallbacks = [];
  let firstSignal = null;
  let firstBodyCanceled = false;
  let postCount = 0;
  const postBodies = [];
  const harness = createHarness({
    setTimeout(callback) { return setTimeout(callback, 0); },
    clearTimeout(handle) { clearTimeout(handle); },
    setInterval(callback) {
      intervalCallbacks.push(callback);
      return intervalCallbacks.length;
    },
    clearInterval() {},
    fetchImpl: async (url, requestOptions, { Response, ReadableStream, TextEncoder }) => {
      if (url !== '/ui/v1/responses' || (requestOptions.method || 'GET') !== 'POST') {
        throw new Error(`unexpected fetch: ${url}`);
      }
      postCount += 1;
      postBodies.push(String(requestOptions.body || ''));
      if (postCount === 1) {
        firstSignal = requestOptions.signal;
        return new Response(new ReadableStream({
          cancel() {
            firstBodyCanceled = true;
          },
        }), { status: 200 });
      }
      const encoder = new TextEncoder();
      const body = [
        'event: response.created\n',
        'data: {"response":{"id":"resp_test","model":"test-model","status":"in_progress"},"sequence_number":1}\n\n',
        'event: response.completed\n',
        'data: {"response":{"id":"resp_test","model":"test-model","status":"completed"},"sequence_number":2}\n\n',
        'data: [DONE]\n\n',
      ].join('');
      return new Response(new ReadableStream({
        start(controller) {
          controller.enqueue(encoder.encode(body));
          controller.close();
        },
      }), {
        status: 200,
        headers: { 'x-response-id': 'resp_test' },
      });
    },
  });
  const { app, elements, state, cleanup } = harness;
  const session = {
    id: 'session_retry_runtime_swap',
    provider: 'chatgpt',
    activeModel: 'old-model',
    activeEffort: 'medium',
    messages: [
      { id: 'u1', role: 'user', content: 'hello', created: 1 },
      { id: 'a1', role: 'assistant', content: 'hi', created: 2 },
    ],
    lastResponseId: 'resp_previous',
    activeResponseId: null,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  state.draftSessionActive = false;
  state.selectedProvider = 'chatgpt';
  state.selectedModel = 'new-model';
  state.selectedEffort = 'high';
  app.applyModelChange('new-model');
  elements.promptInput.value = 'continue';

  const sendPromise = app.sendMessage();
  const attached = await waitFor(() => (
    postCount === 1
    && typeof state.abortController?._heartbeatCancelStream === 'function'
  ), 1000);
  if (!attached) {
    fail(name, 'first POST body reader was not attached');
    await cleanup();
    await sendPromise.catch(() => {});
    return;
  }

  state.lastEventTime = Date.now() - app.HEARTBEAT_STALE_THRESHOLD - 1;
  intervalCallbacks[intervalCallbacks.length - 1]();
  const retried = await waitFor(() => postCount === 2, 1000);
  await sendPromise.catch((err) => {
    fail(name, 'sendMessage rejected instead of retrying', String(err));
  });
  await cleanup();

  if (!retried || postCount !== 2) {
    fail(name, `expected one POST retry, got ${postCount}`);
    return;
  }
  if (postBodies.length !== 2 || postBodies[0] !== postBodies[1]) {
    fail(name, 'explicit runtime swap changed across attached-body retry', JSON.stringify(postBodies));
    return;
  }
  const retryBody = JSON.parse(postBodies[1]);
  if (!retryBody.model_swap || retryBody.model !== 'new-model' || retryBody.reasoning_effort !== 'high') {
    fail(name, 'retry dropped explicit runtime swap target', postBodies[1]);
    return;
  }
  if (!firstBodyCanceled || firstSignal?.aborted) {
    fail(name, 'first POST was not soft-canceled cleanly');
    return;
  }

  pass(name);
}

async function testSendMessageHeartbeatAbortRetriesBeforeResponseId() {
  const name = 'heartbeat abort before response id retries POST with same idempotency key';
  const intervalCallbacks = [];
  let postCount = 0;
  const postBodies = [];
  const idempotencyKeys = [];
  const retryDelays = [];
  const harness = createHarness({
    setTimeout(callback, ms) {
      retryDelays.push(Number(ms || 0));
      return setTimeout(callback, 0);
    },
    clearTimeout(handle) { clearTimeout(handle); },
    setInterval(callback) {
      intervalCallbacks.push(callback);
      return intervalCallbacks.length;
    },
    clearInterval() {},
    fetchImpl: async (url, requestOptions, { Response, ReadableStream, TextEncoder }) => {
      if (url !== '/ui/v1/responses' || (requestOptions.method || 'GET') !== 'POST') {
        throw new Error(`unexpected fetch: ${url}`);
      }
      postCount += 1;
      postBodies.push(String(requestOptions.body || ''));
      idempotencyKeys.push(requestOptions.headers?.['Idempotency-Key'] || '');
      if (postCount === 1) {
        return new Promise((_resolve, reject) => {
          requestOptions.signal.addEventListener('abort', () => {
            reject(new DOMException('The operation was aborted.', 'AbortError'));
          });
        });
      }
      const encoder = new TextEncoder();
      const body = [
        'event: response.created\n',
        'data: {"response":{"id":"resp_test","model":"test-model","status":"in_progress"},"sequence_number":1}\n\n',
        'event: response.completed\n',
        'data: {"response":{"id":"resp_test","model":"test-model","status":"completed"},"sequence_number":2}\n\n',
        'data: [DONE]\n\n',
      ].join('');
      return new Response(new ReadableStream({
        start(controller) {
          controller.enqueue(encoder.encode(body));
          controller.close();
        },
      }), {
        status: 200,
        headers: { 'x-response-id': 'resp_test' },
      });
    },
  });
  const { app, elements, state, cleanup } = harness;
  elements.promptInput.value = 'hello';

  const sendPromise = app.sendMessage();
  const attached = await waitFor(() => state.currentStreamSessionId && state.abortController && postCount === 1, 1000);
  if (!attached) {
    fail(name, 'initial POST did not attach heartbeat-monitored stream state');
    await cleanup();
    await sendPromise.catch(() => {});
    return;
  }
  if (intervalCallbacks.length === 0) {
    fail(name, 'heartbeat monitor was not started');
    await cleanup();
    await sendPromise.catch(() => {});
    return;
  }

  const preResponseStaleThreshold = Math.max(app.HEARTBEAT_STALE_THRESHOLD, Number(state.abortController?._heartbeatStaleThreshold || 0) || 0);
  state.lastEventTime = Date.now() - preResponseStaleThreshold - 1;
  intervalCallbacks[intervalCallbacks.length - 1]();
  const retried = await waitFor(() => postCount === 2, 1000);
  if (!retried) {
    fail(name, `retry POST did not start, postCount=${postCount}`);
    await cleanup();
    await sendPromise.catch(() => {});
    return;
  }
  await waitFor(() => !state.streaming && !state.currentStreamResponseId, 1000);
  await sendPromise.catch((err) => {
    fail(name, 'sendMessage rejected after pre-response-id heartbeat retry', String(err));
  });
  await cleanup();

  if (postCount !== 2) {
    fail(name, `expected exactly two POST attempts, got ${postCount}`);
    return;
  }
  if (!retryDelays.includes(1000)) {
    fail(name, 'first heartbeat retry did not use the normal reconnect delay', JSON.stringify(retryDelays));
    return;
  }
  if (!idempotencyKeys[0] || idempotencyKeys[0] !== idempotencyKeys[1]) {
    fail(name, 'retry did not reuse the same idempotency key', JSON.stringify(idempotencyKeys));
    return;
  }
  const session = state.sessions[0];
  const userMessages = (session?.messages || []).filter((message) => message.role === 'user');
  if (userMessages.length !== 1) {
    fail(name, `retry duplicated the user message: ${userMessages.length}`, JSON.stringify(session?.messages || []));
    return;
  }
  if (postBodies.length === 2 && postBodies[0] !== postBodies[1]) {
    fail(name, 'retry POST body changed', JSON.stringify(postBodies));
    return;
  }

  pass(name);
}

async function testSendMessageLargeUploadUsesLongerPreResponseHeartbeatGrace() {
  const name = 'large initial POST upload gets longer heartbeat grace before first response byte';
  const intervalCallbacks = [];
  let postCount = 0;
  const harness = createHarness({
    setInterval(callback) {
      intervalCallbacks.push(callback);
      return intervalCallbacks.length;
    },
    clearInterval() {},
    fetchImpl: async (url, requestOptions) => {
      if (url !== '/ui/v1/responses' || (requestOptions.method || 'GET') !== 'POST') {
        throw new Error(`unexpected fetch: ${url}`);
      }
      postCount += 1;
      return new Promise((_resolve, reject) => {
        requestOptions.signal.addEventListener('abort', () => {
          reject(new DOMException('The operation was aborted.', 'AbortError'));
        });
      });
    },
  });
  const { app, elements, state, cleanup } = harness;
  elements.promptInput.value = 'x'.repeat(1024 * 1024);

  const sendPromise = app.sendMessage();
  const attached = await waitFor(() => postCount === 1 && state.abortController && !state.abortController.signal.aborted, 1000);
  if (!attached) {
    fail(name, `large POST did not attach heartbeat controller, postCount=${postCount}`);
    await cleanup();
    await sendPromise.catch(() => {});
    return;
  }
  const controller = state.abortController;
  if (!(controller._heartbeatStaleThreshold > app.HEARTBEAT_STALE_THRESHOLD)) {
    fail(name, `expected upload heartbeat threshold > ${app.HEARTBEAT_STALE_THRESHOLD}, got ${controller._heartbeatStaleThreshold}`);
    app.detachResponseStream();
    await sendPromise.catch(() => {});
    await cleanup();
    return;
  }

  state.lastEventTime = Date.now() - app.HEARTBEAT_STALE_THRESHOLD - 1000;
  intervalCallbacks[intervalCallbacks.length - 1]();
  if (controller.signal.aborted) {
    fail(name, 'heartbeat aborted during large upload grace period');
    await cleanup();
    await sendPromise.catch(() => {});
    return;
  }

  app.detachResponseStream();
  await sendPromise.catch((err) => {
    fail(name, 'sendMessage rejected after intentional large-upload detach', String(err));
  });
  await cleanup();
  pass(name);
}

async function testSendMessageTransientPreResponseFailureRetries() {
  const name = 'transient pre-response POST failure retries with same idempotency key';
  let postCount = 0;
  const retryDelays = [];
  const idempotencyKeys = [];
  const harness = createHarness({
    setTimeout(callback, ms) {
      retryDelays.push(Number(ms || 0));
      return setTimeout(callback, 0);
    },
    clearTimeout(handle) { clearTimeout(handle); },
    fetchImpl: async (url, requestOptions, { Response, ReadableStream, TextEncoder }) => {
      if (url !== '/ui/v1/responses' || (requestOptions.method || 'GET') !== 'POST') {
        throw new Error(`unexpected fetch: ${url}`);
      }
      postCount += 1;
      idempotencyKeys.push(requestOptions.headers?.['Idempotency-Key'] || '');
      if (postCount === 1) {
        return new Response(JSON.stringify({ error: { message: 'temporary upstream failure' } }), {
          status: 503,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      const encoder = new TextEncoder();
      const body = [
        'event: response.created\n',
        'data: {"response":{"id":"resp_test","model":"test-model","status":"in_progress"},"sequence_number":1}\n\n',
        'event: response.completed\n',
        'data: {"response":{"id":"resp_test","model":"test-model","status":"completed"},"sequence_number":2}\n\n',
        'data: [DONE]\n\n',
      ].join('');
      return new Response(new ReadableStream({
        start(controller) {
          controller.enqueue(encoder.encode(body));
          controller.close();
        },
      }), {
        status: 200,
        headers: { 'x-response-id': 'resp_test' },
      });
    },
  });
  const { app, elements, state, cleanup } = harness;
  elements.promptInput.value = 'hello';

  await app.sendMessage().catch((err) => {
    fail(name, 'sendMessage rejected after transient pre-response retry', String(err));
  });
  await cleanup();

  if (postCount !== 2) {
    fail(name, `expected two POST attempts, got ${postCount}`);
    return;
  }
  if (!retryDelays.includes(1000)) {
    fail(name, 'transient failure did not use reconnect delay', JSON.stringify(retryDelays));
    return;
  }
  if (!idempotencyKeys[0] || idempotencyKeys[0] !== idempotencyKeys[1]) {
    fail(name, 'transient retry did not reuse idempotency key', JSON.stringify(idempotencyKeys));
    return;
  }
  const userMessages = (state.sessions[0]?.messages || []).filter((message) => message.role === 'user');
  if (userMessages.length !== 1) {
    fail(name, `transient retry duplicated user message: ${userMessages.length}`, JSON.stringify(state.sessions[0]?.messages || []));
    return;
  }

  pass(name);
}

async function testSendMessageHeartbeatAbortKeepsRetryingWithSlowBackoff() {
  const name = 'heartbeat abort before response id keeps retrying but slows to once a minute';
  const intervalCallbacks = [];
  const retryDelays = [];
  let postCount = 0;
  const idempotencyKeys = [];
  const harness = createHarness({
    setTimeout(callback, ms) {
      retryDelays.push(Number(ms || 0));
      return setTimeout(callback, 0);
    },
    clearTimeout(handle) { clearTimeout(handle); },
    setInterval(callback) {
      intervalCallbacks.push(callback);
      return intervalCallbacks.length;
    },
    clearInterval() {},
    fetchImpl: async (url, requestOptions, { Response, ReadableStream, TextEncoder }) => {
      if (url !== '/ui/v1/responses' || (requestOptions.method || 'GET') !== 'POST') {
        throw new Error(`unexpected fetch: ${url}`);
      }
      postCount += 1;
      idempotencyKeys.push(requestOptions.headers?.['Idempotency-Key'] || '');
      if (postCount <= 6) {
        return new Promise((_resolve, reject) => {
          requestOptions.signal.addEventListener('abort', () => {
            reject(new DOMException('The operation was aborted.', 'AbortError'));
          });
        });
      }
      const encoder = new TextEncoder();
      const body = [
        'event: response.created\n',
        'data: {"response":{"id":"resp_test","model":"test-model","status":"in_progress"},"sequence_number":1}\n\n',
        'event: response.completed\n',
        'data: {"response":{"id":"resp_test","model":"test-model","status":"completed"},"sequence_number":2}\n\n',
        'data: [DONE]\n\n',
      ].join('');
      return new Response(new ReadableStream({
        start(controller) {
          controller.enqueue(encoder.encode(body));
          controller.close();
        },
      }), {
        status: 200,
        headers: { 'x-response-id': 'resp_test' },
      });
    },
  });
  const { app, elements, state, cleanup } = harness;
  elements.promptInput.value = 'hello';

  const sendPromise = app.sendMessage();
  for (let attempt = 1; attempt <= 6; attempt += 1) {
    const attached = await waitFor(() => postCount === attempt && state.abortController && !state.abortController.signal.aborted, 1000);
    if (!attached) {
      fail(name, `POST attempt ${attempt} did not attach heartbeat controller; postCount=${postCount}`);
      await cleanup();
      await sendPromise.catch(() => {});
      return;
    }
    const preResponseStaleThreshold = Math.max(app.HEARTBEAT_STALE_THRESHOLD, Number(state.abortController?._heartbeatStaleThreshold || 0) || 0);
    state.lastEventTime = Date.now() - preResponseStaleThreshold - 1;
    intervalCallbacks[intervalCallbacks.length - 1]();
  }

  const recovered = await waitFor(() => postCount === 7 && !state.streaming && !state.currentStreamResponseId, 1000);
  await sendPromise.catch((err) => {
    fail(name, 'sendMessage rejected during long heartbeat retry recovery', String(err));
  });
  await cleanup();

  if (!recovered) {
    fail(name, `retry loop did not eventually recover, postCount=${postCount}`);
    return;
  }
  if (!retryDelays.includes(60000)) {
    fail(name, 'long-running retry loop never slowed to one minute', JSON.stringify(retryDelays));
    return;
  }
  const uniqueKeys = [...new Set(idempotencyKeys.filter(Boolean))];
  if (uniqueKeys.length !== 1 || idempotencyKeys.length !== 7) {
    fail(name, 'retry loop did not reuse a single idempotency key', JSON.stringify(idempotencyKeys));
    return;
  }
  const session = state.sessions[0];
  const userMessages = (session?.messages || []).filter((message) => message.role === 'user');
  if (userMessages.length !== 1) {
    fail(name, `retry loop duplicated the user message: ${userMessages.length}`, JSON.stringify(session?.messages || []));
    return;
  }

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

async function testSendMessageUsesLocalContinuationIdWithoutPreflightSync() {
  const name = 'sendMessage uses the local continuation id without preflight sync';
  const lastResponseId = 'resp_msg_796651';
  let syncCalls = 0;
  const harness = createHarness({
    onSyncActiveSessionFromServer() {
      syncCalls += 1;
      return { active_run: false, lastResponseId };
    },
  });
  const { app, elements, state, fetchCalls, cleanup } = harness;
  const session = {
    id: 'session_local_previous',
    title: 'Local previous',
    messages: [{ id: 'msg_old', role: 'user', content: 'old', created: Date.now() - 1000 }],
    lastResponseId,
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
  if (syncCalls !== 0) {
    fail(name, `expected no preflight sync, got ${syncCalls}`);
    return;
  }
  const postCall = fetchCalls.find((call) => call.url === '/ui/v1/responses' && call.method === 'POST');
  if (!postCall || !postCall.body) {
    fail(name, 'missing POST /ui/v1/responses body', JSON.stringify(fetchCalls));
    return;
  }
  const body = JSON.parse(postCall.body);
  if (body.previous_response_id !== lastResponseId) {
    fail(name, `previous_response_id = ${JSON.stringify(body.previous_response_id)}, want ${JSON.stringify(lastResponseId)}`, postCall.body);
    return;
  }
  if (body.include_server_tools !== true) {
    fail(name, `include_server_tools = ${JSON.stringify(body.include_server_tools)}, want true`, postCall.body);
    return;
  }

  pass(name);
}

async function testSendMessageIncludesServerToolsForFirstPartyUI() {
  const name = 'sendMessage includes server tools for first-party UI responses';
  const harness = createHarness();
  const { app, elements, fetchCalls, cleanup } = harness;
  elements.promptInput.value = 'use a tool';

  let sendErr = null;
  await app.sendMessage().catch((err) => {
    sendErr = err;
  });
  await cleanup();

  if (sendErr) {
    fail(name, 'sendMessage rejected unexpectedly', String(sendErr));
    return;
  }
  const postCall = fetchCalls.find((call) => call.url === '/ui/v1/responses' && call.method === 'POST');
  if (!postCall || !postCall.body) {
    fail(name, 'missing POST /ui/v1/responses body', JSON.stringify(fetchCalls));
    return;
  }
  const body = JSON.parse(postCall.body);
  if (body.include_server_tools !== true) {
    fail(name, `include_server_tools = ${JSON.stringify(body.include_server_tools)}, want true`, postCall.body);
    return;
  }

  pass(name);
}

async function testSendMessageRecoversStaleContinuationAfterConflict() {
  const name = 'sendMessage recovers stale continuation after conflict without preflight sync';
  const staleId = 'resp_msg_796607';
  const latestId = 'resp_msg_796651';
  let syncCalls = 0;
  let syncOpts = null;
  let postAttempts = 0;
  const harness = createHarness({
    fetchImpl: async (url, requestOptions, helpers) => {
      if (url === '/ui/v1/responses' && (requestOptions.method || 'GET') === 'POST') {
        postAttempts += 1;
        if (postAttempts === 1) {
          return new helpers.Response(JSON.stringify({
            error: { message: `previous_response_id ${JSON.stringify(staleId)} is stale; latest is ${JSON.stringify(latestId)}` }
          }), {
            status: 409,
            headers: { 'Content-Type': 'application/json' },
          });
        }

        const responseBody = [
          'id: 1\n',
          'event: response.created\n',
          `data: {"response":{"id":"${latestId}","model":"test-model","status":"in_progress"},"sequence_number":1}\n\n`,
          'id: 2\n',
          'event: response.output_text.delta\n',
          'data: {"delta":"hello","sequence_number":2}\n\n',
          'id: 3\n',
          'event: response.completed\n',
          `data: {"response":{"id":"${latestId}","model":"test-model","status":"completed"},"sequence_number":3}\n\n`,
          'data: [DONE]\n\n',
        ].join('');
        return new helpers.Response(new helpers.ReadableStream({
          start(controller) {
            controller.enqueue(helpers.encoder.encode(responseBody));
            controller.close();
          },
        }), {
          status: 200,
          headers: { 'x-response-id': latestId },
        });
      }
      throw new Error(`unexpected fetch: ${url}`);
    },
    onSyncActiveSessionFromServer(session, _pollOnActive, opts) {
      syncCalls += 1;
      syncOpts = opts || null;
      session.lastResponseId = latestId;
      return { active_run: false, lastResponseId: latestId };
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
    fail(name, `expected one recovery sync, got ${syncCalls}`);
    return;
  }
  if (!syncOpts || syncOpts.skipMessagesFetch !== true) {
    fail(name, 'expected recovery sync to skip message fetches', JSON.stringify(syncOpts));
    return;
  }
  if (postAttempts !== 2) {
    fail(name, `expected two POST attempts, got ${postAttempts}`);
    return;
  }
  const postCalls = fetchCalls.filter((call) => call.url === '/ui/v1/responses' && call.method === 'POST');
  if (postCalls.length !== 2) {
    fail(name, 'expected two POST /ui/v1/responses calls', JSON.stringify(fetchCalls));
    return;
  }
  const firstBody = JSON.parse(postCalls[0].body || '{}');
  const secondBody = JSON.parse(postCalls[1].body || '{}');
  if (firstBody.previous_response_id !== staleId) {
    fail(name, `first previous_response_id = ${JSON.stringify(firstBody.previous_response_id)}, want ${JSON.stringify(staleId)}`, postCalls[0].body);
    return;
  }
  if (secondBody.previous_response_id !== latestId) {
    fail(name, `second previous_response_id = ${JSON.stringify(secondBody.previous_response_id)}, want ${JSON.stringify(latestId)}`, postCalls[1].body);
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

async function testSendMessageRecoversAfterStreamBufferOverflowDone() {
  const name = 'sendMessage treats stream_buffer_overflow [DONE] as resumable';
  const harness = createHarness({
    postBody: [
      'id: 1\n',
      'event: response.created\n',
      'data: {"response":{"id":"resp_test","model":"test-model","status":"in_progress"},"sequence_number":1}\n\n',
      'id: 2\n',
      'event: response.output_text.delta\n',
      'data: {"delta":"hello","sequence_number":2}\n\n',
      'id: 2\n',
      'event: response.stream_error\n',
      'data: {"error":{"type":"stream_buffer_overflow"},"sequence_number":2,"min_replay_after":0,"recovery":{"sequence_number":2,"messages":[{"id":"a1","role":"assistant","content":"hello","created":1}]}}\n\n',
      'data: [DONE]\n\n',
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
  const { app, elements, cleanup, fetchCalls, getEventsStarted } = harness;
  elements.promptInput.value = 'hello';

  let sendErr = null;
  const sendPromise = app.sendMessage().catch((err) => {
    sendErr = err;
  });

  const handedOff = await waitFor(() => getEventsStarted(), 75);
  if (!handedOff) {
    fail(name, 'client did not reopen /events after recoverable stream overflow', JSON.stringify(fetchCalls));
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
    fail(name, 'expected reconnect after overflow to resume from recovered sequence 2', JSON.stringify(fetchCalls));
    return;
  }

  const session = harness.state.sessions[0];
  const assistant = session && session.messages.find((message) => message.role === 'assistant');
  if (!assistant || assistant.content !== 'hello world') {
    fail(name, 'assistant content did not recover after overflow', assistant ? assistant.content : 'missing');
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

async function testDraftSendIgnoresStaleGlobalStreamingFlag() {
  const name = 'New Chat draft send ignores stale global streaming flag from previous session';
  const harness = createHarness();
  const { app, elements, state, fetchCalls, cleanup } = harness;

  const oldSession = {
    id: 'session_old_busy',
    title: 'Old busy',
    messages: [],
    lastResponseId: null,
    activeResponseId: null,
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(oldSession);
  state.activeSessionId = '';
  state.draftSessionActive = true;
  state.streaming = true;
  state.currentStreamSessionId = '';
  state.currentStreamResponseId = '';
  elements.promptInput.value = 'fresh draft message';

  const sendPromise = app.sendMessage().catch(() => {});
  await new Promise((resolve) => setTimeout(resolve, 0));

  const interruptCalls = fetchCalls.filter((call) => String(call.url).endsWith('/interrupt'));
  if (interruptCalls.length !== 0) {
    fail(name, 'draft send should not post an interjection to an old session', JSON.stringify(fetchCalls));
    app.detachResponseStream();
    await sendPromise;
    await cleanup();
    return;
  }

  if (state.sessions.length < 2 || state.sessions[0].id === oldSession.id) {
    fail(name, 'draft send should create a fresh session', JSON.stringify(state.sessions.map((session) => session.id)));
    app.detachResponseStream();
    await sendPromise;
    await cleanup();
    return;
  }

  const postCalls = fetchCalls.filter((call) => call.url === '/ui/v1/responses' && call.method === 'POST');
  if (postCalls.length !== 1) {
    fail(name, 'draft send should post a normal response request once', JSON.stringify(fetchCalls));
    app.detachResponseStream();
    await sendPromise;
    await cleanup();
    return;
  }

  app.detachResponseStream();
  await sendPromise;
  await cleanup();
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
  state.queuedInterrupts.push({ sessionId: session.id, prompt: 'follow-up question', messageId: 'msg_queued' });

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

async function testDrainInterruptQueueIgnoresOtherSessionEntries() {
  const name = 'drainInterruptQueueIfIdle only drains entries for the active session';
  const harness = createHarness();
  const { app, state, fetchCalls, cleanup } = harness;

  const activeSession = {
    id: 'session_active_drain',
    title: 'Active drain test',
    messages: [],
    lastResponseId: null,
    activeResponseId: null,
    lastSequenceNumber: 0,
    number: 1,
  };
  const otherSession = {
    id: 'session_other_drain',
    title: 'Other drain test',
    messages: [],
    lastResponseId: null,
    activeResponseId: null,
    lastSequenceNumber: 0,
    number: 2,
  };
  state.sessions.push(activeSession, otherSession);
  state.activeSessionId = activeSession.id;
  state.queuedInterrupts.push({ sessionId: otherSession.id, prompt: 'other session thought', messageId: 'msg_other' });

  app.drainInterruptQueueIfIdle(activeSession);

  if (state.queuedInterrupts.length !== 1 || state.queuedInterrupts[0].sessionId !== otherSession.id) {
    fail(name, 'queued interrupt for another session should remain queued', JSON.stringify(state.queuedInterrupts));
    await cleanup();
    return;
  }
  const postCalls = fetchCalls.filter(c => c.url === '/ui/v1/responses' && c.method === 'POST');
  if (postCalls.length !== 0) {
    fail(name, 'should not send another session queued interrupt while active session is drained', JSON.stringify(fetchCalls));
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testResumeActiveResponseRecoversFromSnapshotBeforeReplaying() {
  const name = 'resumeActiveResponse recovers from snapshot before replaying tool events';
  const responseId = 'resp_recover';
  let planRefreshes = 0;
  const harness = createHarness({
    responseId,
    onRefreshCurrentPlanFromServer() {
      planRefreshes += 1;
      return {};
    },
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
            status: 'done',
            tools: [
              { id: 'call_1', name: 'update_plan', arguments: '{"plan":[{"step":"Recovered","status":"completed"}]}', status: 'done', resultStatus: 'success', created: 1001 },
              { id: 'call_2', name: 'update_plan', arguments: '{"plan":[]}', status: 'error', resultStatus: 'error', created: 1002 },
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
  if (toolGroups.length !== 2) {
    fail(name, `expected recovered and replayed tool groups, got ${toolGroups.length}`, JSON.stringify(toolGroups));
    await cleanup();
    return;
  }
  if (toolGroups.reduce((total, group) => total + group.tools.length, 0) !== 3) {
    fail(name, 'expected recovered and replayed groups to contain 3 tools total', JSON.stringify(toolGroups));
    await cleanup();
    return;
  }
  if (toolGroups[0].tools[0].resultStatus !== 'success'
    || toolGroups[0].tools[1].resultStatus !== 'error'
    || toolGroups[0].tools[1].status !== 'error') {
    fail(name, 'recovery lost successful or failed tool execution evidence', JSON.stringify(toolGroups[0]));
    await cleanup();
    return;
  }
  if (planRefreshes !== 2) {
    fail(name, `expected snapshot recovery and terminal fallback to refetch plan state, got ${planRefreshes}`);
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testRecoverySnapshotReconcilesDurableToolCallsIdempotently() {
  const name = 'recovery snapshot reconciles durable tool calls by stable call ID idempotently';
  let reconciliations = 0;
  const harness = createHarness({
    onReconcileSessionToolCallProjection() { reconciliations += 1; }
  });
  const { app, state, cleanup } = harness;
  const durable = {
    id: 'srv_seq_382_tools_0',
    role: 'tool-group',
    durable: true,
    status: 'done',
    tools: [
      { id: 'call_GwHMMHXf28QA81Zfm9vgRMap', name: 'queue_agent', status: 'done' },
      { id: 'call_XX3ODxDyeoIxc7ZUZwJm19qu', name: 'wait_for_jobs', status: 'done' }
    ],
    created: 1000
  };
  const interjection = {
    id: 'msg_interjection', role: 'user', content: 'keep going', interruptState: 'interject', created: 1100
  };
  const session = {
    id: 'session_durable_recovery_overlap',
    messages: [durable, interjection],
    activeResponseId: 'resp_overlap',
    lastSequenceNumber: 0,
    number: 1
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  const snapshot = {
    id: 'resp_overlap',
    status: 'in_progress',
    last_sequence_number: 9,
    recovery: {
      sequence_number: 9,
      messages: [{
        id: 'msg_441406d5-live-tools',
        role: 'tool-group',
        status: 'running',
        created: 1200,
        tools: [
          { id: 'call_GwHMMHXf28QA81Zfm9vgRMap', name: 'queue_agent', status: 'done' },
          { id: 'call_XX3ODxDyeoIxc7ZUZwJm19qu', name: 'wait_for_jobs', status: 'done' },
          { id: 'call_newer_shell', name: 'shell', status: 'running' }
        ]
      }]
    }
  };

  app.applyResponseRecoverySnapshot(session, snapshot);
  app.applyResponseRecoverySnapshot(session, snapshot);

  const toolGroups = session.messages.filter((message) => message.role === 'tool-group');
  const callIDs = toolGroups.flatMap((group) => group.tools.map((tool) => tool.id));
  const counts = new Map(callIDs.map((id) => [id, callIDs.filter((candidate) => candidate === id).length]));
  if ([...counts.values()].some((count) => count !== 1)) {
    fail(name, 'repeat recovery projected duplicate stable call IDs', JSON.stringify(session.messages));
    await cleanup();
    return;
  }
  if (toolGroups.map((group) => group.id).join(',') !== 'srv_seq_382_tools_0,msg_441406d5-live-tools'
      || toolGroups[1].tools.map((tool) => tool.id).join(',') !== 'call_newer_shell') {
    fail(name, 'partial overlap did not preserve only the newer optimistic call in order', JSON.stringify(toolGroups));
    await cleanup();
    return;
  }
  if (reconciliations !== 2) {
    fail(name, `expected one reconciliation per recovery, got ${reconciliations}`);
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testResumeActiveResponseRepairsSequenceGapWithSnapshot() {
  const name = 'resumeActiveResponse repairs replay sequence gaps with snapshot';
  const responseId = 'resp_gap_repair';
  const harness = createHarness({
    responseId,
    snapshotPayload: {
      id: responseId,
      status: 'completed',
      last_sequence_number: 9,
      recovery: {
        sequence_number: 9,
        messages: [
          { id: 'assistant-recovered', role: 'assistant', content: 'complete recovered answer', created: 1001 },
        ],
      },
    },
    eventsBody: [
      'id: 7\n',
      'event: response.output_text.delta\n',
      'data: {"delta":"tail only","sequence_number":7}\n\n',
      'id: 8\n',
      'event: response.output_text.delta\n',
      'data: {"delta":" should not apply","sequence_number":8}\n\n',
      'id: 9\n',
      'event: response.completed\n',
      `data: {"response":{"id":"${responseId}","model":"test-model","status":"completed"},"sequence_number":9}\n\n`,
      'data: [DONE]\n\n',
    ].join(''),
  });

  const { app, state, fetchCalls, cleanup } = harness;
  const session = {
    id: 'session_gap_repair',
    title: 'Gap repair',
    messages: [
      { id: 'msg_user_local', role: 'user', content: 'go', created: 1000 },
    ],
    activeResponseId: responseId,
    lastSequenceNumber: 4,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  await app.resumeActiveResponse(session, { responseId });

  const eventsCall = fetchCalls.find((call) => call.url.startsWith(`/ui/v1/responses/${responseId}/events?after=`));
  if (!eventsCall || !eventsCall.url.endsWith('after=4')) {
    fail(name, 'expected replay request from local sequence 4', eventsCall ? eventsCall.url : JSON.stringify(fetchCalls));
    await cleanup();
    return;
  }
  const snapshotCall = fetchCalls.find((call) => call.url === `/ui/v1/responses/${responseId}`);
  if (!snapshotCall) {
    fail(name, 'expected snapshot fetch after detecting sequence gap', JSON.stringify(fetchCalls));
    await cleanup();
    return;
  }
  const assistantMessages = session.messages.filter((message) => message.role === 'assistant');
  if (assistantMessages.length !== 1 || assistantMessages[0].content !== 'complete recovered answer') {
    fail(name, 'expected gapped replay tail to be discarded in favor of snapshot', JSON.stringify(session.messages));
    await cleanup();
    return;
  }
  if (session.lastSequenceNumber !== 0) {
    // completed snapshot clears active tracking, which resets the replay cursor.
    fail(name, `lastSequenceNumber = ${session.lastSequenceNumber}, want 0 after completed snapshot`);
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testRecoverySnapshotClearsSyntheticPendingInterjectionByText() {
  const name = 'recovery snapshot clears pending interjection tracked under synthetic id';
  const responseId = 'resp_snapshot_interjection_cleanup';
  const harness = createHarness({
    responseId,
    snapshotPayload: {
      id: responseId,
      status: 'completed',
      last_sequence_number: 9,
      recovery: {
        sequence_number: 9,
        messages: [
          { id: 'real-id', role: 'user', content: 'please also check X', interruptState: 'interject', created: 1002 },
          { id: 'assistant-after', role: 'assistant', content: 'checked', created: 1003 },
        ],
      },
    },
  });
  const { app, state, cleanup } = harness;
  const session = {
    id: 'session_snapshot_interjection_cleanup',
    title: 'Snapshot interjection cleanup',
    messages: [
      { id: 'msg_user_local', role: 'user', content: 'find files', created: 1000 },
      { id: 'synthetic-id', role: 'user', content: 'please also check X', created: 1001, interruptState: 'pending_interject' },
    ],
    lastResponseId: null,
    activeResponseId: responseId,
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  state.pendingInterjections = [
    { sessionId: session.id, prompt: 'please also check X', messageId: 'synthetic-id', action: 'interject' },
  ];
  state.pendingInterruptCommits = [
    { sessionId: session.id, prompt: 'please also check X', messageId: 'synthetic-id' },
  ];

  await app.resumeActiveResponse(session, { responseId, recoverFromSnapshot: true });

  if (state.pendingInterjections.length !== 0) {
    fail(name, 'pendingInterjections should be cleared by recovered committed interjection', JSON.stringify(state.pendingInterjections));
    await cleanup();
    return;
  }
  if (state.pendingInterruptCommits.length !== 0) {
    fail(name, 'pendingInterruptCommits should be cleared by recovered committed interjection', JSON.stringify(state.pendingInterruptCommits));
    await cleanup();
    return;
  }
  if (state.queuedInterrupts.length !== 0) {
    fail(name, 'committed interjection should not be requeued after terminal snapshot', JSON.stringify(state.queuedInterrupts));
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testRecoverySnapshotDoesNotDuplicateOptimisticInterjection() {
  const name = 'recovery snapshot does not duplicate optimistic interjection message';
  const responseId = 'resp_snapshot_interjection_dedupe';
  const harness = createHarness({
    responseId,
    snapshotPayload: {
      id: responseId,
      status: 'completed',
      last_sequence_number: 4,
      recovery: {
        sequence_number: 4,
        messages: [
          { id: 'real-id', role: 'user', content: 'please also check X', interruptState: 'interject', created: 1002 },
          { id: 'assistant-after', role: 'assistant', content: 'checked', created: 1003 },
        ],
      },
    },
  });
  const { app, state, cleanup } = harness;
  const session = {
    id: 'session_snapshot_interjection_dedupe',
    title: 'Snapshot interjection dedupe',
    messages: [
      { id: 'msg_user_local', role: 'user', content: 'find files', created: 1000 },
      { id: 'synthetic-id', role: 'user', content: 'please also check X', created: 1001, interruptState: 'pending_interject' },
    ],
    lastResponseId: null,
    activeResponseId: responseId,
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  state.pendingInterjections = [
    { sessionId: session.id, prompt: 'please also check X', messageId: 'synthetic-id', action: 'interject' },
  ];
  state.pendingInterruptCommits = [
    { sessionId: session.id, prompt: 'please also check X', messageId: 'synthetic-id' },
  ];

  await app.resumeActiveResponse(session, { responseId, recoverFromSnapshot: true });

  const matchingUsers = session.messages.filter((message) => message.role === 'user' && message.content === 'please also check X');
  if (matchingUsers.length !== 1) {
    fail(name, `expected one interjection user message after recovery, got ${matchingUsers.length}`, JSON.stringify(session.messages));
    await cleanup();
    return;
  }
  if (matchingUsers[0].interruptState !== 'interject') {
    fail(name, `recovered interjection interruptState = ${matchingUsers[0].interruptState}, want interject`, JSON.stringify(matchingUsers[0]));
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testResumeActiveResponseHeartbeatCancelSlowsAndRecovers() {
  const name = 'resumeActiveResponse heartbeat cancellations keep recovering with slow backoff';
  const responseId = 'resp_events_heartbeat_retry';
  const intervalCallbacks = [];
  const retryDelays = [];
  const eventSignals = [];
  let eventsCount = 0;
  let eventsCancelCount = 0;
  let syncCalls = 0;
  const harness = createHarness({
    responseId,
    setTimeout(callback, ms) {
      retryDelays.push(Number(ms || 0));
      return setTimeout(callback, 0);
    },
    clearTimeout(handle) { clearTimeout(handle); },
    setInterval(callback) {
      intervalCallbacks.push(callback);
      return intervalCallbacks.length;
    },
    clearInterval() {},
    fetchImpl: async (url, requestOptions, { Response, ReadableStream, TextEncoder }) => {
      if (url.startsWith(`/ui/v1/responses/${responseId}/events?after=`)) {
        eventsCount += 1;
        if (eventsCount <= 6) {
          const signal = requestOptions.signal;
          eventSignals.push(signal);
          return new Response(new ReadableStream({
            start(controller) {
              signal.addEventListener('abort', () => {
                try { controller.error(new DOMException('The operation was aborted.', 'AbortError')); } catch (_err) { /* ignore */ }
              });
            },
            cancel() {
              eventsCancelCount += 1;
            },
          }), {
            status: 200,
            headers: { 'Content-Type': 'text/event-stream' },
          });
        }
        const encoder = new TextEncoder();
        const body = [
          'event: response.created\n',
          `data: {"response":{"id":"${responseId}","model":"test-model","status":"in_progress"},"sequence_number":1}\n\n`,
          'event: response.completed\n',
          `data: {"response":{"id":"${responseId}","model":"test-model","status":"completed"},"sequence_number":2}\n\n`,
          'data: [DONE]\n\n',
        ].join('');
        return new Response(new ReadableStream({
          start(controller) {
            controller.enqueue(encoder.encode(body));
            controller.close();
          },
        }), {
          status: 200,
          headers: { 'Content-Type': 'text/event-stream' },
        });
      }
      throw new Error(`unexpected fetch: ${url}`);
    },
  });
  const { app, elements, state, cleanup } = harness;
  app.syncActiveSessionFromServer = async (session) => {
    syncCalls += 1;
    session.activeResponseId = responseId;
    return { active_run: true, active_response_id: responseId };
  };

  const session = {
    id: 'session_events_heartbeat_retry',
    title: 'Events heartbeat retry',
    messages: [],
    lastResponseId: null,
    activeResponseId: responseId,
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  const resumePromise = app.resumeActiveResponse(session, { responseId });
  for (let attempt = 1; attempt <= 6; attempt += 1) {
    const attached = await waitFor(() => (
      eventsCount === attempt
      && state.abortController
      && !state.abortController.signal.aborted
      && typeof state.abortController._heartbeatCancelStream === 'function'
    ), 1000);
    if (!attached) {
      fail(name, `events attempt ${attempt} did not attach heartbeat controller; eventsCount=${eventsCount}`);
      await cleanup();
      await resumePromise.catch(() => {});
      return;
    }
    const controller = state.abortController;
    state.lastEventTime = Date.now() - app.HEARTBEAT_STALE_THRESHOLD - 1;
    intervalCallbacks[intervalCallbacks.length - 1]();
    if (controller.signal.aborted) {
      fail(name, `events attempt ${attempt} aborted fetch after its body was attached`);
      await cleanup();
      await resumePromise.catch(() => {});
      return;
    }
    if (eventsCancelCount !== attempt) {
      fail(name, `events attempt ${attempt} did not cancel its response body; cancels=${eventsCancelCount}`);
      await cleanup();
      await resumePromise.catch(() => {});
      return;
    }
  }

  const recovered = await waitFor(() => eventsCount === 7 && !session.activeResponseId, 1000);
  await resumePromise.catch((err) => {
    fail(name, 'resumeActiveResponse rejected during heartbeat retry recovery', String(err));
  });
  await cleanup();

  if (!recovered) {
    fail(name, `resume loop did not eventually recover, eventsCount=${eventsCount}, active=${session.activeResponseId}`);
    return;
  }
  if (!retryDelays.includes(60000)) {
    fail(name, 'events retry loop never slowed to one minute', JSON.stringify(retryDelays));
    return;
  }
  if (syncCalls === 0) {
    fail(name, 'events retry loop did not poll server state after repeated heartbeat cancellations');
    return;
  }
  if (eventSignals.some((signal) => signal.aborted)) {
    fail(name, 'an attached events fetch was aborted instead of canceling its body');
    return;
  }
  if (Object.prototype.hasOwnProperty.call(elements.connectionState.dataset, 'reconnectState')) {
    fail(name, 'reconnect diagnostics were written without opt-in', JSON.stringify(elements.connectionState.dataset));
    return;
  }

  pass(name);
}

async function testResumeReconnectBackoffCanBeWokenWithoutDuplicateLoop() {
  const name = 'slow response reconnect backoff is wakeable without a duplicate resume loop';
  const responseId = 'resp_wakeable_retry';
  const timers = [];
  let nextTimerID = 0;
  let eventsCount = 0;
  const harness = createHarness({
    responseId,
    diagnostics: true,
    setTimeout(callback, ms) {
      const timer = { id: ++nextTimerID, callback, ms: Number(ms || 0), cleared: false };
      timers.push(timer);
      return timer.id;
    },
    clearTimeout(id) {
      const timer = timers.find((item) => item.id === id);
      if (timer) timer.cleared = true;
    },
    fetchImpl: async (url, _requestOptions, { Response, ReadableStream, TextEncoder }) => {
      if (!url.startsWith(`/ui/v1/responses/${responseId}/events?after=`)) {
        throw new Error(`unexpected fetch: ${url}`);
      }
      eventsCount += 1;
      if (eventsCount <= 6) {
        return new Response('temporary failure', { status: 503 });
      }
      const body = [
        'event: response.created\n',
        `data: {"response":{"id":"${responseId}","status":"in_progress"},"sequence_number":1}\n\n`,
        'event: response.completed\n',
        `data: {"response":{"id":"${responseId}","status":"completed"},"sequence_number":2}\n\n`,
        'data: [DONE]\n\n',
      ].join('');
      const encoder = new TextEncoder();
      return new Response(new ReadableStream({
        start(controller) {
          controller.enqueue(encoder.encode(body));
          controller.close();
        },
      }), { status: 200, headers: { 'Content-Type': 'text/event-stream' } });
    },
  });
  const { app, elements, state, connectionStates, cleanup } = harness;
  const session = {
    id: 'session_wakeable_retry',
    title: 'Wakeable retry',
    messages: [],
    activeResponseId: responseId,
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  const resumePromise = app.resumeActiveResponse(session, { responseId });
  for (let attempt = 1; attempt <= 5; attempt += 1) {
    const ready = await waitFor(() => eventsCount === attempt && timers.some((timer) => !timer.cleared), 1000);
    if (!ready) {
      fail(name, `retry ${attempt} did not schedule`);
      await cleanup();
      return;
    }
    const timer = timers.find((item) => !item.cleared);
    timer.cleared = true;
    timer.callback();
  }

  const slowReady = await waitFor(() => eventsCount === 6 && timers.some((timer) => !timer.cleared && timer.ms === 60000), 1000);
  if (!slowReady) {
    fail(name, 'slow one-minute retry was not scheduled', JSON.stringify(timers));
    await cleanup();
    return;
  }
  const duplicateResult = await app.resumeActiveResponse(session, { responseId });
  if (duplicateResult !== false) {
    fail(name, 'duplicate resume call should remain idempotently suppressed');
    await cleanup();
    return;
  }

  const slowTimer = timers.find((timer) => !timer.cleared && timer.ms === 60000);
  if (elements.connectionState.dataset.reconnectState !== 'waiting') {
    fail(name, 'opted-in diagnostics did not expose waiting reconnect state', JSON.stringify(elements.connectionState.dataset));
    await cleanup();
    return;
  }
  const partialWake = app.wakeResponseReconnect({ reason: 'online', responseId });
  if (partialWake || slowTimer.cleared) {
    fail(name, 'response-only wake must not match an ambiguous session');
    await cleanup();
    return;
  }
  const woke = app.wakeResponseReconnect({ reason: 'online', sessionId: session.id, responseId });
  if (!woke || !slowTimer.cleared) {
    fail(name, 'online wake did not resolve and cancel the slow backoff');
    await cleanup();
    return;
  }
  if (elements.connectionState.dataset.reconnectState !== 'waking') {
    fail(name, 'opted-in diagnostics did not expose wake state', JSON.stringify(elements.connectionState.dataset));
    await cleanup();
    return;
  }

  await resumePromise;
  await cleanup();
  if (eventsCount !== 7) {
    fail(name, `expected one resumed fetch after wake, got ${eventsCount} total fetches`);
    return;
  }
  if (!connectionStates.some((status) => status.source === 'legacy' && status.text.includes('within one minute'))) {
    fail(name, 'slow reconnect status was not exposed to the UI', JSON.stringify(connectionStates));
    return;
  }

  pass(name);
}

async function testDetachDuringSlowReconnectTransfersResumeOwnership() {
  const name = 'detach during slow reconnect cancels the old loop without releasing the new owner';
  const responseId = 'resp_detached_retry';
  const timers = [];
  let nextTimerID = 0;
  let eventsCount = 0;
  const harness = createHarness({
    responseId,
    setTimeout(callback, ms) {
      const timer = { id: ++nextTimerID, callback, ms: Number(ms || 0), cleared: false };
      timers.push(timer);
      return timer.id;
    },
    clearTimeout(id) {
      const timer = timers.find((item) => item.id === id);
      if (timer) timer.cleared = true;
    },
    fetchImpl: async (url, requestOptions, { Response, ReadableStream }) => {
      if (!url.startsWith(`/ui/v1/responses/${responseId}/events?after=`)) {
        throw new Error(`unexpected fetch: ${url}`);
      }
      eventsCount += 1;
      if (eventsCount <= 6) {
        return new Response('temporary failure', { status: 503 });
      }
      const signal = requestOptions.signal;
      return new Response(new ReadableStream({
        start(controller) {
          signal.addEventListener('abort', () => {
            try { controller.error(new DOMException('The operation was aborted.', 'AbortError')); } catch (_err) { /* ignore */ }
          });
        },
      }), { status: 200, headers: { 'Content-Type': 'text/event-stream' } });
    },
  });
  const { app, state, cleanup } = harness;
  const session = {
    id: 'session_detached_retry',
    title: 'Detached retry',
    messages: [],
    activeResponseId: responseId,
    lastSequenceNumber: 0,
    number: 1,
  };
  const otherSession = {
    id: 'session_other',
    title: 'Other session',
    messages: [],
    activeResponseId: null,
    lastSequenceNumber: 0,
    number: 2,
  };
  state.sessions.push(session, otherSession);
  state.activeSessionId = session.id;

  let oldResult;
  void app.resumeActiveResponse(session, { responseId }).then((result) => {
    oldResult = result;
  });
  for (let attempt = 1; attempt <= 5; attempt += 1) {
    const ready = await waitFor(() => eventsCount === attempt && timers.some((timer) => !timer.cleared), 1000);
    if (!ready) {
      fail(name, `retry ${attempt} did not schedule`);
      app.detachResponseStream();
      await cleanup();
      return;
    }
    const timer = timers.find((item) => !item.cleared);
    timer.cleared = true;
    timer.callback();
  }

  const slowReady = await waitFor(() => eventsCount === 6 && timers.some((timer) => !timer.cleared && timer.ms === 60000), 1000);
  if (!slowReady) {
    fail(name, 'old loop did not enter the one-minute reconnect backoff', JSON.stringify(timers));
    app.detachResponseStream();
    await cleanup();
    return;
  }
  const oldSlowTimer = timers.find((timer) => !timer.cleared && timer.ms === 60000);

  // Switch away and immediately back before promise continuations run. The new
  // loop must acquire the same resume key before the detached old loop cleans up.
  state.activeSessionId = otherSession.id;
  app.detachResponseStream();
  state.activeSessionId = session.id;
  const newResumePromise = app.resumeActiveResponse(session, { responseId });

  if (!oldSlowTimer.cleared) {
    fail(name, 'detach did not explicitly cancel the old reconnect waiter');
    await cleanup();
    return;
  }
  const oldStopped = await waitFor(() => oldResult !== undefined, 1000);
  if (!oldStopped || oldResult !== false) {
    fail(name, `detached old resume loop did not terminate; result=${String(oldResult)}`);
    await cleanup();
    return;
  }
  if (eventsCount !== 7) {
    fail(name, `only the new loop should issue an event request after switching back; got ${eventsCount} total`);
    app.detachResponseStream();
    await newResumePromise;
    await cleanup();
    return;
  }

  // The old loop's finally must not delete the new loop's registration. A
  // third call is suppressed only while that one new owner remains registered.
  let duplicateResult;
  void app.resumeActiveResponse(session, { responseId }).then((result) => {
    duplicateResult = result;
  });
  const duplicateSettled = await waitFor(() => duplicateResult !== undefined || eventsCount !== 7, 1000);
  if (!duplicateSettled || duplicateResult !== false || eventsCount !== 7) {
    fail(name, `expected exactly one new resume owner to remain; duplicateResult=${String(duplicateResult)}, events=${eventsCount}`);
    app.detachResponseStream();
    await cleanup();
    return;
  }
  oldSlowTimer.callback();
  await new Promise((resolve) => setTimeout(resolve, 0));
  if (eventsCount !== 7) {
    fail(name, `expired old waiter issued another event request; got ${eventsCount} total`);
    app.detachResponseStream();
    await newResumePromise;
    await cleanup();
    return;
  }

  app.detachResponseStream();
  await newResumePromise;
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
  app.applyModelChange('new-model');
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

async function testSendMessageIgnoresAutomaticEffortDriftWithoutUserIntent() {
  const name = 'sendMessage ignores automatic effort drift without user intent';
  const harness = createHarness();
  const { app, elements, state, fetchCalls, cleanup } = harness;
  const session = {
    id: 'session_automatic_effort_drift',
    title: 'Automatic effort drift test',
    provider: 'chatgpt',
    activeModel: 'gpt-5.6-sol',
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
  state.selectedProvider = 'chatgpt';
  state.selectedModel = 'gpt-5.6-sol';
  state.selectedEffort = '';
  state.modelInfoByID = {};
  elements.promptInput.value = 'continue';

  await app.sendMessage();
  await cleanup();

  const postCall = fetchCalls.find((call) => call.url === '/ui/v1/responses' && call.method === 'POST');
  if (!postCall?.body) {
    fail(name, 'missing POST /ui/v1/responses body', JSON.stringify(fetchCalls));
    return;
  }
  const body = JSON.parse(postCall.body);
  if (body.model_swap) {
    fail(name, 'automatic selected-effort drift requested a model swap', postCall.body);
    return;
  }
  if (body.reasoning_effort !== 'medium') {
    fail(name, `request effort = ${JSON.stringify(body.reasoning_effort)}, want persisted medium`, postCall.body);
    return;
  }
  pass(name);
}

async function testSendMessageIgnoresAutomaticExplicitEffortDriftWithoutUserIntent() {
  const name = 'sendMessage ignores automatic explicit effort drift without user intent';
  const harness = createHarness();
  const { app, elements, state, fetchCalls, cleanup } = harness;
  const session = {
    id: 'session_automatic_explicit_effort_drift',
    provider: 'chatgpt',
    activeModel: 'gpt-5.6-sol',
    activeEffort: '',
    messages: [
      { id: 'u1', role: 'user', content: 'hello', created: 1 },
      { id: 'a1', role: 'assistant', content: 'hi', created: 2 },
    ],
    lastResponseId: 'resp_previous',
    activeResponseId: null,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  state.selectedProvider = 'chatgpt';
  state.selectedModel = 'gpt-5.6-sol';
  state.selectedEffort = 'high';
  elements.promptInput.value = 'continue';

  await app.sendMessage();
  await cleanup();

  const postCall = fetchCalls.find((call) => call.url === '/ui/v1/responses' && call.method === 'POST');
  const body = postCall?.body ? JSON.parse(postCall.body) : null;
  if (!body) {
    fail(name, 'missing POST /ui/v1/responses body', JSON.stringify(fetchCalls));
    return;
  }
  if (body.model_swap || Object.prototype.hasOwnProperty.call(body, 'reasoning_effort')) {
    fail(name, 'automatic explicit effort drift leaked into pinned request', postCall.body);
    return;
  }
  pass(name);
}

async function testExplicitAutoEffortStillRequestsModelSwap() {
  const name = 'explicit Auto effort still requests a model swap';
  const harness = createHarness();
  const { app, elements, state, fetchCalls, cleanup } = harness;
  const session = {
    id: 'session_explicit_auto_effort',
    title: 'Explicit auto effort test',
    provider: 'chatgpt',
    activeModel: 'gpt-5.6-sol',
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
  state.selectedProvider = 'chatgpt';
  state.selectedModel = 'gpt-5.6-sol';
  state.selectedEffort = 'medium';
  state.modelInfoByID = {};

  await app.applyEffortChange('');
  elements.promptInput.value = 'continue';
  await app.sendMessage();
  await cleanup();

  const postCall = fetchCalls.find((call) => call.url === '/ui/v1/responses' && call.method === 'POST');
  if (!postCall?.body) {
    fail(name, 'missing POST /ui/v1/responses body', JSON.stringify(fetchCalls));
    return;
  }
  const body = JSON.parse(postCall.body);
  if (!body.model_swap) {
    fail(name, 'explicit Auto selection did not request a model swap', postCall.body);
    return;
  }
  if (Object.prototype.hasOwnProperty.call(body, 'reasoning_effort')) {
    fail(name, 'Auto swap should omit reasoning_effort', postCall.body);
    return;
  }
  pass(name);
}

async function testSendMessageTreatsImplicitDefaultEffortAsEquivalent() {
  const name = 'sendMessage treats implicit model default effort as unchanged';
  const harness = createHarness();
  const { app, elements, state, fetchCalls, cleanup } = harness;
  const session = {
    id: 'session_implicit_effort',
    title: 'Implicit effort test',
    provider: 'chatgpt',
    activeModel: 'gpt-5.6-sol',
    activeEffort: '',
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
  state.selectedProvider = 'chatgpt';
  state.selectedModel = 'gpt-5.6-sol';
  state.selectedEffort = 'medium';
  state.modelInfoByID = {
    'gpt-5.6-sol': {
      id: 'gpt-5.6-sol',
      reasoning_efforts: ['low', 'medium', 'high'],
      default_reasoning_effort: 'medium',
    },
  };
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
    fail(name, 'implicit medium → explicit medium must not request a model swap', postCall.body);
    return;
  }
  pass(name);
}

async function testSendMessageGatesReasoningModeByModelMetadata() {
  const supportedName = 'sendMessage sends Pro mode only for supporting models';
  {
    const { app, elements, state, fetchCalls, cleanup } = createHarness();
    state.selectedProvider = 'openai';
    state.selectedModel = 'gpt-5.6-sol';
    state.selectedReasoningMode = 'pro';
    state.modelInfoByID = {
      'gpt-5.6-sol': { id: 'gpt-5.6-sol', reasoning_modes: ['standard', 'pro'] },
    };
    elements.promptInput.value = 'hello';

    await app.sendMessage();
    await cleanup();

    const postCall = fetchCalls.find((call) => call.url === '/ui/v1/responses' && call.method === 'POST');
    const body = postCall?.body ? JSON.parse(postCall.body) : null;
    if (!body || body.reasoning?.mode !== 'pro') {
      fail(supportedName, 'expected reasoning.mode=pro', postCall?.body || 'missing request');
      return;
    }
    if (elements.reasoningModeField.hidden) {
      fail(supportedName, 'reasoning mode field should be visible');
      return;
    }
    pass(supportedName);
  }

  const unsupportedName = 'sendMessage clears Pro mode for unsupported models';
  {
    const { app, elements, state, fetchCalls, localStorage, cleanup } = createHarness();
    state.selectedProvider = 'chatgpt';
    state.selectedModel = 'gpt-5.6-sol';
    state.selectedReasoningMode = 'pro';
    state.modelInfoByID = {
      'gpt-5.6-sol': { id: 'gpt-5.6-sol', reasoning_modes: [] },
    };
    elements.promptInput.value = 'hello';

    await app.sendMessage();
    await cleanup();

    const postCall = fetchCalls.find((call) => call.url === '/ui/v1/responses' && call.method === 'POST');
    const body = postCall?.body ? JSON.parse(postCall.body) : null;
    if (!body || Object.prototype.hasOwnProperty.call(body, 'reasoning')) {
      fail(unsupportedName, 'reasoning controls must be omitted', postCall?.body || 'missing request');
      return;
    }
    if (state.selectedReasoningMode !== 'standard' || localStorage.getItem('selectedReasoningMode') !== 'standard') {
      fail(unsupportedName, 'expected stale Pro selection to be reset');
      return;
    }
    if (!elements.reasoningModeField.hidden) {
      fail(unsupportedName, 'reasoning mode field should be hidden');
      return;
    }
    pass(unsupportedName);
  }
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

function testGuardianReviewEventIsDisplayOnlyTransient() {
  const name = 'guardian review event is display-only and transient';
  const { app, state } = createHarness();
  const session = {
    id: 'session_guardian', title: 'Guardian', messages: [], lastResponseId: null,
    activeResponseId: null, lastSequenceNumber: 0, number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  const streamState = app.createResponseStreamState(session);
  app.applyResponseStreamEvent(session, streamState, 'response.guardian.review', {
    message: 'Guardian approved command', sequence_number: 1,
  });
  const marker = session.messages[0];
  if (!marker || marker.role !== 'event' || !marker.transient) {
    fail(name, 'guardian marker can leak into durable optimistic recovery', JSON.stringify(marker));
    return;
  }
  pass(name);
}

function testResponsePhaseEventUpdatesTransientMarker() {
  const name = 'response phase event updates transient marker without assistant text';
  const harness = createHarness();
  const { app, state } = harness;
  const session = {
    id: 'session_phase',
    title: 'Phase test',
    messages: [],
    lastResponseId: null,
    activeResponseId: null,
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  const streamState = app.createResponseStreamState(session);

  app.applyResponseStreamEvent(session, streamState, 'response.phase', {
    text: 'Compacting context...',
    sequence_number: 1,
  });
  app.applyResponseStreamEvent(session, streamState, 'response.phase', {
    text: 'Continuing from compacted context...',
    sequence_number: 2,
  });

  const markers = session.messages.filter((message) => message.role === 'phase');
  const assistants = session.messages.filter((message) => message.role === 'assistant');
  if (markers.length !== 1) {
    fail(name, `expected one phase marker, got ${markers.length}`, JSON.stringify(session.messages));
    return;
  }
  if (!markers[0].transient || markers[0].content !== 'Continuing from compacted context...') {
    fail(name, 'phase marker was not updated in place', JSON.stringify(markers[0]));
    return;
  }
  if (assistants.length !== 0) {
    fail(name, 'phase event should not create assistant messages', JSON.stringify(assistants));
    return;
  }
  if (session.lastSequenceNumber !== 2) {
    fail(name, `lastSequenceNumber = ${session.lastSequenceNumber}, want 2`);
    return;
  }
  pass(name);
}

function createRetryProjectionHarness(suffix = 'retry') {
  const harness = createHarness();
  const responseId = `resp_${suffix}`;
  const session = {
    id: `session_${suffix}`,
    title: 'Retry test',
    messages: [],
    lastResponseId: null,
    activeResponseId: responseId,
    lastSequenceNumber: 0,
    number: 1,
  };
  harness.state.sessions.push(session);
  harness.state.activeSessionId = session.id;
  harness.state.currentStreamSessionId = session.id;
  harness.state.currentStreamResponseId = responseId;
  return {
    ...harness,
    responseId,
    session,
    streamState: harness.app.createResponseStreamState(session),
  };
}

function projectRetry(harness, sequenceNumber = 1, message = 'Model stream interrupted; reconnecting (2/6)…') {
  harness.app.applyResponseStreamEvent(harness.session, harness.streamState, 'response.retry', {
    message,
    attempt: sequenceNumber + 1,
    max_attempts: 6,
    wait_seconds: 0.5,
    sequence_number: sequenceNumber,
  });
}

function testResponseRetryEventUpdatesOwnedHeaderStatus() {
  const name = 'response retry updates one owned header status without transcript messages';
  const harness = createRetryProjectionHarness('retry_status');

  projectRetry(harness, 1, 'Model stream interrupted; reconnecting (2/6)…');
  projectRetry(harness, 2, 'Model stream interrupted; reconnecting (3/6)…');

  const status = harness.getProviderRetryStatus();
  const sets = harness.connectionStates.filter((entry) => entry.source === 'provider-retry' && entry.action === 'set' && entry.applied);
  if (harness.session.messages.length !== 0) {
    fail(name, 'retry event created transcript messages', JSON.stringify(harness.session.messages));
    return;
  }
  if (!status || status.sessionId !== harness.session.id || status.responseId !== harness.responseId
      || status.text !== 'Model stream interrupted; reconnecting (3/6)…') {
    fail(name, 'retry attempts did not update the owned header status in place', JSON.stringify(status));
    return;
  }
  if (sets.length !== 2 || sets.some((entry) => entry.mode !== 'retry')) {
    fail(name, 'header status calls were not source/mode aware', JSON.stringify(sets));
    return;
  }
  if (harness.session.lastSequenceNumber !== 2) {
    fail(name, `lastSequenceNumber = ${harness.session.lastSequenceNumber}, want 2`);
    return;
  }
  pass(name);
}

function testProviderRetryClearsOnMeaningfulProgress() {
  const name = 'provider retry clears on every meaningful progress category';
  const cases = [
    ['non-empty text delta', 'response.output_text.delta', { delta: 'resumed' }],
    ['new text segment', 'response.output_text.new_segment', {}],
    ['output item added', 'response.output_item.added', { item: { type: 'message' } }],
    ['function arguments delta', 'response.function_call_arguments.delta', { delta: '{' }],
    ['output item done', 'response.output_item.done', { item: { type: 'message' } }],
  ];

  for (const [label, event, payload] of cases) {
    const harness = createRetryProjectionHarness(`progress_${event.replace(/[^a-z]+/g, '_')}`);
    projectRetry(harness, 1);
    harness.app.applyResponseStreamEvent(harness.session, harness.streamState, event, {
      ...payload,
      sequence_number: 2,
    });
    if (harness.getProviderRetryStatus() !== null) {
      fail(name, `${label} did not clear retry`, JSON.stringify(harness.getProviderRetryStatus()));
      return;
    }
  }
  pass(name);
}

function testProviderRetryPersistsAcrossNonProgressEvents() {
  const name = 'provider retry persists across heartbeat phase model-swap and recoverable stream errors';
  const cases = [
    ['heartbeat', 'response.heartbeat', {}],
    ['phase', 'response.phase', { text: 'Compacting context…' }],
    ['model swap progress', 'response.model_swap.progress', { stage: 'starting' }],
    ['recoverable stream error', 'response.stream_error', { error: { type: 'temporary_stream_error' } }],
    ['snapshot recovery', 'response.stream_error', {
      error: { type: 'stream_buffer_overflow' },
      recovery: { sequence_number: 2, messages: [] },
    }],
  ];

  for (const [label, event, payload] of cases) {
    const harness = createRetryProjectionHarness(`nonprogress_${event.replace(/[^a-z]+/g, '_')}`);
    projectRetry(harness, 1);
    harness.app.applyResponseStreamEvent(harness.session, harness.streamState, event, {
      ...payload,
      sequence_number: 2,
    });
    const status = harness.getProviderRetryStatus();
    if (!status || status.responseId !== harness.responseId) {
      fail(name, `${label} incorrectly cleared retry`, JSON.stringify(status));
      return;
    }
  }
  pass(name);
}

function testProviderRetryClearsOnTerminalEvents() {
  const name = 'provider retry clears on completion failure and cancellation';
  const cases = [
    ['response.completed', (responseId) => ({ response: { id: responseId, status: 'completed' } })],
    ['response.failed', () => ({ error: { message: 'terminal provider failure' } })],
    ['response.cancelled', () => ({})],
  ];

  for (const [event, makePayload] of cases) {
    const harness = createRetryProjectionHarness(`terminal_${event.replace(/[^a-z]+/g, '_')}`);
    projectRetry(harness, 1);
    harness.app.applyResponseStreamEvent(harness.session, harness.streamState, event, {
      ...makePayload(harness.responseId),
      sequence_number: 2,
    });
    if (harness.getProviderRetryStatus() !== null) {
      fail(name, `${event} did not clear retry`, JSON.stringify(harness.getProviderRetryStatus()));
      return;
    }
  }
  pass(name);
}

function testActiveResponseTransitionClearsObsoleteRetryOwner() {
  const name = 'active response transitions clear only the obsolete retry owner';
  const harness = createRetryProjectionHarness('response_transition');
  projectRetry(harness, 1, 'Old response retry');

  const oldResponseId = harness.responseId;
  const newResponseId = 'resp_response_transition_new';
  harness.app.setActiveResponseTracking(harness.session, newResponseId, 0);
  harness.state.currentStreamResponseId = newResponseId;
  if (harness.getProviderRetryStatus() !== null) {
    fail(name, 'obsolete retry remained visible after the active response changed', JSON.stringify(harness.getProviderRetryStatus()));
    return;
  }

  projectRetry(harness, 2, 'New response retry');
  harness.app.clearActiveResponseTracking(harness.session, oldResponseId);
  const status = harness.getProviderRetryStatus();
  if (status?.responseId !== newResponseId || harness.session.activeResponseId !== newResponseId) {
    fail(name, 'stale clear affected the newer response owner', JSON.stringify({ status, activeResponseId: harness.session.activeResponseId }));
    return;
  }
  pass(name);
}

function testProviderRetryOwnershipGuardsBackgroundDetachAndStaleClear() {
  const name = 'provider retry ownership guards background events detach and stale clears';
  const harness = createRetryProjectionHarness('ownership');
  const background = {
    id: 'session_background',
    title: 'Background',
    messages: [],
    activeResponseId: 'resp_background',
    lastSequenceNumber: 0,
  };
  harness.state.sessions.push(background);
  const backgroundState = harness.app.createResponseStreamState(background);

  harness.app.applyResponseStreamEvent(background, backgroundState, 'response.retry', {
    message: 'Background retry',
    sequence_number: 1,
  });
  if (harness.getProviderRetryStatus() !== null) {
    fail(name, 'background stream set visible retry status', JSON.stringify(harness.getProviderRetryStatus()));
    return;
  }

  projectRetry(harness, 1, 'Old response retry');
  const oldResponseId = harness.responseId;
  const newResponseId = 'resp_ownership_new';
  harness.session.activeResponseId = newResponseId;
  harness.state.currentStreamResponseId = newResponseId;
  projectRetry(harness, 2, 'New response retry');
  harness.app.clearProviderRetryStatus(harness.session.id, oldResponseId);
  if (harness.getProviderRetryStatus()?.responseId !== newResponseId) {
    fail(name, 'stale owner cleared newer response retry', JSON.stringify(harness.getProviderRetryStatus()));
    return;
  }

  harness.app.detachResponseStream();
  if (harness.getProviderRetryStatus() !== null) {
    fail(name, 'detach did not clear matching retry status', JSON.stringify(harness.getProviderRetryStatus()));
    return;
  }
  pass(name);
}

function testResponsePhaseUpdateCanStraddleResumedOutput() {
  const name = 'response phase update can straddle resumed output without duplicating its marker';
  const harness = createHarness();
  const { app, state } = harness;
  const session = {
    id: 'session_phase_straddle',
    title: 'Phase straddle test',
    messages: [],
    lastResponseId: null,
    activeResponseId: null,
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  const streamState = app.createResponseStreamState(session);

  app.applyResponseStreamEvent(session, streamState, 'response.phase', {
    text: 'Working…',
    sequence_number: 1,
  });
  app.applyResponseStreamEvent(session, streamState, 'response.output_text.delta', {
    delta: 'intermediate output',
    sequence_number: 2,
  });
  app.applyResponseStreamEvent(session, streamState, 'response.phase', {
    text: 'Done working.',
    sequence_number: 3,
  });

  const markers = session.messages.filter((message) => message.role === 'phase');
  if (markers.length !== 1 || markers[0].content !== 'Done working.') {
    fail(name, 'phase update created a duplicate marker around resumed output', JSON.stringify(session.messages));
    return;
  }
  pass(name);
}

function testResponsePhaseSeparatesAssistantSegments() {
  const name = 'response phase separates assistant segments in order';
  const harness = createHarness();
  const { app, state } = harness;
  const session = {
    id: 'session_phase_order',
    title: 'Phase ordering test',
    messages: [],
    lastResponseId: null,
    activeResponseId: null,
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  const streamState = app.createResponseStreamState(session);

  app.applyResponseStreamEvent(session, streamState, 'response.output_text.delta', {
    delta: 'checkpoint',
    sequence_number: 1,
  });
  app.applyResponseStreamEvent(session, streamState, 'response.phase', {
    text: 'Compacting context...',
    sequence_number: 2,
  });
  app.applyResponseStreamEvent(session, streamState, 'response.output_text.delta', {
    delta: 'continued answer',
    sequence_number: 3,
  });

  const roles = session.messages.map((message) => message.role);
  if (roles.join(',') !== 'assistant,phase,assistant') {
    fail(name, `unexpected message roles/order: ${roles.join(',')}`, JSON.stringify(session.messages));
    return;
  }
  if (session.messages[0].content !== 'checkpoint' || session.messages[2].content !== 'continued answer') {
    fail(name, 'assistant segments were not separated around phase marker', JSON.stringify(session.messages));
    return;
  }
  pass(name);
}

const runtimeIntentTestSession = (id) => ({
  id,
  provider: 'chatgpt',
  activeModel: 'gpt-5.6-sol',
  activeEffort: 'medium',
  messages: [
    { id: `${id}_u1`, role: 'user', content: 'hello', created: 1 },
    { id: `${id}_a1`, role: 'assistant', content: 'hi', created: 2 },
  ],
  lastResponseId: `${id}_previous`,
  activeResponseId: null,
  number: 1,
});

async function testSettingsEffortCancelDoesNotAuthorizeSwap() {
  const name = 'settings effort Cancel does not authorize a runtime swap';
  const harness = createHarness();
  const { app, elements, state, fetchCalls, cleanup } = harness;
  const session = runtimeIntentTestSession('settings_cancel');
  state.sessions.push(session);
  state.activeSessionId = session.id;
  state.token = 'token-123';
  state.selectedProvider = 'chatgpt';
  state.selectedModel = 'gpt-5.6-sol';
  state.selectedEffort = 'medium';
  elements.authTokenInput.value = 'token-123';

  elements.effortSelect.value = 'high';
  await elements.effortSelect.dispatchEvent({ type: 'change' });
  app.closeAuthModal();
  elements.promptInput.value = 'continue';
  await app.sendMessage();
  await cleanup();

  const postCall = fetchCalls.find((call) => call.url === '/ui/v1/responses' && call.method === 'POST');
  const body = postCall?.body ? JSON.parse(postCall.body) : null;
  if (!body || body.model_swap || body.reasoning_effort !== 'medium') {
    fail(name, 'Cancel leaked modal effort intent into the request', postCall?.body || JSON.stringify(fetchCalls));
    return;
  }
  pass(name);
}

async function testSettingsEffortSaveAuthorizesSwap() {
  const name = 'settings effort Save authorizes a runtime swap';
  const harness = createHarness();
  const { app, elements, state, fetchCalls, cleanup } = harness;
  const session = runtimeIntentTestSession('settings_save');
  state.sessions.push(session);
  state.activeSessionId = session.id;
  state.token = 'token-123';
  state.connected = true;
  state.selectedProvider = 'chatgpt';
  state.selectedModel = 'gpt-5.6-sol';
  state.selectedEffort = 'medium';
  elements.authTokenInput.value = 'token-123';

  elements.effortSelect.value = 'high';
  await elements.effortSelect.dispatchEvent({ type: 'change' });
  await app.connectToken();
  elements.promptInput.value = 'continue';
  await app.sendMessage();
  await cleanup();

  const postCall = fetchCalls.find((call) => call.url === '/ui/v1/responses' && call.method === 'POST');
  const body = postCall?.body ? JSON.parse(postCall.body) : null;
  if (!body?.model_swap || body.reasoning_effort !== 'high') {
    fail(name, 'Save did not carry explicit modal effort into a model swap', postCall?.body || JSON.stringify(fetchCalls));
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

  if (!connectionStates.some((status) => status.source === 'legacy' && status.text === 'Cancelling\u2026')) {
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

async function testReplayedInterjectionWithoutIdDoesNotDuplicateExistingInjectedMessage() {
  const name = 'replayed response.interjection without id does not duplicate existing injected user message';
  const harness = createHarness();
  const { app, state, cleanup } = harness;

  const session = {
    id: 'session_interject_replay',
    title: 'Interject replay test',
    messages: [
      {
        id: 'msg_already_injected',
        role: 'user',
        content: 'also format all sql nicely',
        created: 1000,
        interruptState: 'interject',
      },
    ],
    lastResponseId: null,
    activeResponseId: 'resp_int_replay',
    lastSequenceNumber: 10,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  state.pendingInterjections = [];
  state.pendingInterruptCommits = [];

  const streamState = app.createResponseStreamState(session);

  // Simulates a reconnect/replay path where the UI has already rendered the
  // committed interjection but receives a response.interjection event without
  // the stable interjection_id needed to match it. The correct behavior should
  // be to update/reuse the existing injected user message, not append another
  // identical bubble.
  app.applyResponseStreamEvent(session, streamState, 'response.interjection', {
    text: 'also format all sql nicely',
    sequence_number: 11,
  });

  const userMessages = session.messages.filter((m) => m.role === 'user' && m.content === 'also format all sql nicely');
  if (userMessages.length !== 1) {
    fail(name, `expected 1 injected user message after replay, got ${userMessages.length}`, JSON.stringify(session.messages));
    await cleanup();
    return;
  }
  if (userMessages[0].id !== 'msg_already_injected') {
    fail(name, `expected replay to keep existing message id, got ${userMessages[0].id}`);
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testCommittedInterjectionWithRealIdClearsStaleSyntheticPending() {
  const name = 'committed response.interjection with server id clears pending tracked under a different id';
  const harness = createHarness();
  const { app, state, cleanup } = harness;

  const session = {
    id: 'session_interject_stale',
    title: 'Interject stale id test',
    messages: [
      {
        id: 'synthetic-id',
        role: 'user',
        content: 'foo',
        created: 1000,
        interruptState: 'pending_interject',
      },
    ],
    lastResponseId: null,
    activeResponseId: 'resp_int_stale',
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  // The optimistic pending entries are tracked under a synthetic id, but the
  // committed event arrives with the real server-issued interjection_id. The
  // id-only lookup misses, so the cleanup must fall back to a text match.
  state.pendingInterjections = [
    { sessionId: session.id, prompt: 'foo', messageId: 'synthetic-id', action: 'interject' },
  ];
  state.pendingInterruptCommits = [
    { sessionId: session.id, prompt: 'foo', messageId: 'synthetic-id' },
  ];

  const streamState = app.createResponseStreamState(session);

  app.applyResponseStreamEvent(session, streamState, 'response.interjection', {
    text: 'foo',
    interjection_id: 'real-id',
  });

  if (state.pendingInterjections.length !== 0) {
    fail(name, 'pendingInterjections should be cleared after commit', JSON.stringify(state.pendingInterjections));
    await cleanup();
    return;
  }
  if (state.pendingInterruptCommits.length !== 0) {
    fail(name, 'pendingInterruptCommits should be cleared after commit', JSON.stringify(state.pendingInterruptCommits));
    await cleanup();
    return;
  }

  const userMessages = session.messages.filter((m) => m.role === 'user' && m.content === 'foo');
  if (userMessages.length !== 1) {
    fail(name, `expected 1 user message after commit, got ${userMessages.length}`, JSON.stringify(session.messages));
    await cleanup();
    return;
  }
  if (userMessages[0].id !== 'synthetic-id') {
    fail(name, `expected existing optimistic message to be reused, got id ${userMessages[0].id}`);
    await cleanup();
    return;
  }
  if (userMessages[0].interruptState !== 'interject') {
    fail(name, `interruptState = ${userMessages[0].interruptState}, want "interject"`);
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testCommittedInterjectionReusesOptimisticMessageEvenWhenPendingTrackedUnderServerId() {
  const name = 'committed response.interjection reuses optimistic message when pending entry has server id';
  const harness = createHarness();
  const { app, state, cleanup } = harness;

  const session = {
    id: 'session_interject_server_id_pending',
    title: 'Interject server id pending test',
    messages: [
      {
        id: 'optimistic-id',
        role: 'user',
        content: 'foo',
        created: 1000,
        interruptState: 'pending_interject',
      },
    ],
    lastResponseId: null,
    activeResponseId: 'resp_int_server_id_pending',
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  state.pendingInterjections = [
    { sessionId: session.id, prompt: 'foo', messageId: 'real-id', action: 'interject' },
  ];
  state.pendingInterruptCommits = [
    { sessionId: session.id, prompt: 'foo', messageId: 'real-id' },
  ];

  const streamState = app.createResponseStreamState(session);
  app.applyResponseStreamEvent(session, streamState, 'response.interjection', {
    text: 'foo',
    interjection_id: 'real-id',
  });

  const userMessages = session.messages.filter((m) => m.role === 'user' && m.content === 'foo');
  if (userMessages.length !== 1) {
    fail(name, `expected 1 user message after commit, got ${userMessages.length}`, JSON.stringify(session.messages));
    await cleanup();
    return;
  }
  if (userMessages[0].id !== 'optimistic-id') {
    fail(name, `expected optimistic message to be reused, got id ${userMessages[0].id}`);
    await cleanup();
    return;
  }
  if (userMessages[0].interruptState !== 'interject') {
    fail(name, `interruptState = ${userMessages[0].interruptState}, want "interject"`);
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testInterjectQueuedShowsPendingBadgeThenInjectedOnCommit() {
  const name = 'queued interjection shows "will incorporate" badge until committed event marks it injected';
  const harness = createHarness({ interruptPayload: { action: 'interject' } });
  const { app, state, cleanup } = harness;

  const session = {
    id: 'session_interject_lifecycle',
    title: 'Interject lifecycle test',
    messages: [
      {
        id: 'msg_x',
        role: 'user',
        content: 'foo',
        created: 1000,
        interruptState: 'evaluating',
      },
    ],
    lastResponseId: null,
    activeResponseId: 'resp_int_lifecycle',
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  state.pendingInterjections = [
    { sessionId: session.id, prompt: 'foo', messageId: 'msg_x', action: 'deciding' },
  ];
  state.pendingInterruptCommits = [
    { sessionId: session.id, prompt: 'foo', messageId: 'msg_x' },
  ];

  // Phase 1: /interrupt classifies as interject (queued, not yet committed).
  const action = await app.interruptActiveRun(session, 'foo', 'msg_x', null, []);
  if (action !== 'interject') {
    fail(name, `expected interrupt action "interject", got ${action}`);
    await cleanup();
    return;
  }

  const queuedMessage = session.messages.find((m) => m.id === 'msg_x');
  if (!queuedMessage || queuedMessage.interruptState !== 'pending_interject') {
    fail(name, `expected interruptState "pending_interject" while queued, got ${queuedMessage?.interruptState}`);
    await cleanup();
    return;
  }
  const stillPending = state.pendingInterjections.find((e) => e.messageId === 'msg_x');
  if (!stillPending || stillPending.action !== 'interject') {
    fail(name, 'pending interjection should remain cancellable with action "interject" while queued', JSON.stringify(state.pendingInterjections));
    await cleanup();
    return;
  }

  // Phase 2: engine drains and emits the committed response.interjection.
  const streamState = app.createResponseStreamState(session);
  app.applyResponseStreamEvent(session, streamState, 'response.interjection', {
    text: 'foo',
    interjection_id: 'msg_x',
  });

  const committedMessage = session.messages.find((m) => m.id === 'msg_x');
  if (!committedMessage || committedMessage.interruptState !== 'interject') {
    fail(name, `expected interruptState "interject" after commit, got ${committedMessage?.interruptState}`);
    await cleanup();
    return;
  }
  if (state.pendingInterjections.length !== 0) {
    fail(name, 'pendingInterjections should be cleared after commit', JSON.stringify(state.pendingInterjections));
    await cleanup();
    return;
  }
  if (state.pendingInterruptCommits.length !== 0) {
    fail(name, 'pendingInterruptCommits should be cleared after commit', JSON.stringify(state.pendingInterruptCommits));
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testUserCancelDiscardsPendingInterjectionStateButPreservesFollowUpQueue() {
  const name = 'user cancel discards pending interjection state but preserves queued follow-up';
  const harness = createHarness();
  const { app, state, cleanup } = harness;

  const session = {
    id: 'session_cancel_preserve_followup',
    title: 'Cancel preserve follow-up',
    messages: [],
    activeResponseId: 'resp_cancel_preserve_followup',
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  state.expectCanceledRun = true;
  state.pendingInterjections = [
    { sessionId: session.id, prompt: 'switch to x', messageId: 'msg_cancel', action: 'cancel' },
  ];
  state.pendingInterruptCommits = [
    { sessionId: session.id, prompt: 'switch to x', messageId: 'msg_cancel' },
  ];
  state.queuedInterrupts = [
    { sessionId: session.id, prompt: 'switch to x', messageId: 'msg_cancel', attachments: [] },
  ];

  const streamState = app.createResponseStreamState(session);
  app.applyResponseStreamEvent(session, streamState, 'response.cancelled', {
    response: { id: 'resp_cancel_preserve_followup', status: 'cancelled' },
    sequence_number: 5,
  });

  if (state.pendingInterjections.length !== 0 || state.pendingInterruptCommits.length !== 0) {
    fail(name, 'pending interjection tracking should be cleared after user cancel', JSON.stringify({
      pendingInterjections: state.pendingInterjections,
      pendingInterruptCommits: state.pendingInterruptCommits,
    }));
    await cleanup();
    return;
  }
  if (state.queuedInterrupts.length !== 1 || state.queuedInterrupts[0].messageId !== 'msg_cancel') {
    fail(name, 'queued follow-up should survive cancellation terminal event', JSON.stringify(state.queuedInterrupts));
    await cleanup();
    return;
  }
  if (state.expectCanceledRun) {
    fail(name, 'expectCanceledRun should be reset after cancellation terminal event');
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

  const attachment = { id: 'att_1', name: 'diagram.png', type: 'image/png', dataURL: 'data:image/png;base64,aW1n' };
  state.pendingInterjections = [
    { sessionId: session.id, prompt: 'late thought', messageId: 'msg_late', action: 'deciding', attachments: [attachment] },
  ];
  state.pendingInterruptCommits = [
    { sessionId: session.id, prompt: 'late thought', messageId: 'msg_late', attachments: [attachment] },
  ];

  app.syncActiveSessionFromServer = async () => ({ active_run: true, active_response_id: 'resp_still_running' });

  const recovered = await app.recoverInterruptConflict(session, 'late thought', 'msg_late', [attachment]);
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

  if (state.queuedInterrupts.length !== 1 || state.queuedInterrupts[0].sessionId !== session.id || state.queuedInterrupts[0].prompt !== 'late thought') {
    fail(name, 'expected follow-up queued for later delivery', JSON.stringify(state.queuedInterrupts));
    await cleanup();
    return;
  }
  if (!Array.isArray(state.queuedInterrupts[0].attachments) || state.queuedInterrupts[0].attachments[0]?.name !== 'diagram.png') {
    fail(name, 'expected queued follow-up to preserve attachments', JSON.stringify(state.queuedInterrupts));
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
  if (!Array.isArray(userMessages[0].attachments) || userMessages[0].attachments[0]?.name !== 'diagram.png') {
    fail(name, 'expected inline queued message to preserve attachment metadata', JSON.stringify(userMessages[0]));
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
  if (state.queuedInterrupts[0].sessionId !== session.id || state.queuedInterrupts[0].prompt !== 'dropped thought') {
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

async function testSuccessfulPlanToolCompletionRefetchesAuthoritativeState() {
  const name = 'successful update_plan completion alone refetches authoritative plan state';
  let refreshes = 0;
  const harness = createHarness({
    onRefreshCurrentPlanFromServer() {
      refreshes += 1;
      return {};
    },
  });
  const { app, state, cleanup } = harness;
  const session = {
    id: 'session_plan_refresh',
    title: 'plan refresh',
    messages: [],
    lastResponseId: null,
    activeResponseId: 'resp_plan_refresh',
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  const streamState = app.createResponseStreamState(session);
  const args = '{"plan":[{"step":"Wait for server","status":"in_progress"}]}';

  app.applyResponseStreamEvent(session, streamState, 'response.output_item.added', {
    output_index: 0,
    item: { type: 'function_call', call_id: 'call_plan', name: 'update_plan' },
  });
  app.applyResponseStreamEvent(session, streamState, 'response.function_call_arguments.delta', {
    output_index: 0,
    delta: args,
  });
  app.applyResponseStreamEvent(session, streamState, 'response.output_item.done', {
    output_index: 0,
    item: { type: 'function_call', call_id: 'call_plan', name: 'update_plan', arguments: args },
  });
  if (refreshes !== 0) {
    fail(name, 'finalized streamed arguments triggered a state refresh');
    await cleanup();
    return;
  }

  app.applyResponseStreamEvent(session, streamState, 'response.tool_exec.end', {
    call_id: 'call_plan',
    tool_name: 'update_plan',
    success: true,
  });
  const tool = session.messages.find((message) => message.role === 'tool-group')?.tools?.[0];
  if (refreshes !== 1 || tool?.resultStatus !== 'success') {
    fail(name, 'successful execution did not record positive evidence and refetch once', JSON.stringify({ refreshes, tool }));
    await cleanup();
    return;
  }

  const failedState = app.createResponseStreamState(session);
  app.applyResponseStreamEvent(session, failedState, 'response.output_item.added', {
    output_index: 1,
    item: { type: 'function_call', call_id: 'call_plan_failed', name: 'update_plan', arguments: args },
  });
  app.applyResponseStreamEvent(session, failedState, 'response.tool_exec.end', {
    call_id: 'call_plan_failed',
    tool_name: 'update_plan',
    success: false,
  });
  const failedTool = session.messages.findLast((message) => message.role === 'tool-group')?.tools?.[0];
  if (refreshes !== 1 || failedTool?.resultStatus !== 'error' || failedTool?.status !== 'error') {
    fail(name, 'failed execution refetched or lost generic error evidence', JSON.stringify({ refreshes, failedTool }));
    await cleanup();
    return;
  }

  state.activeSessionId = 'another-session';
  const inactiveState = app.createResponseStreamState(session);
  app.applyResponseStreamEvent(session, inactiveState, 'response.output_item.added', {
    output_index: 2,
    item: { type: 'function_call', call_id: 'call_plan_inactive', name: 'update_plan', arguments: args },
  });
  app.applyResponseStreamEvent(session, inactiveState, 'response.tool_exec.end', {
    call_id: 'call_plan_inactive',
    tool_name: 'update_plan',
    success: true,
  });
  if (refreshes !== 1) {
    fail(name, 'inactive session completion refetched selected plan state');
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

async function testToolExecImagesUseHubAssetRebase() {
  const name = 'tool_exec.end images use hub asset rebase helper';
  const harness = createHarness();
  const { app, state, cleanup } = harness;
  app.rebaseHubAssetURL = (url) => String(url || '').replace('/ui/images/', '/hub/node/alpha/images/');

  const session = {
    id: 'session_tool_image_hub',
    title: 'image artifact hub',
    messages: [],
    lastResponseId: null,
    activeResponseId: 'resp_tool_image_hub',
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  const streamState = app.createResponseStreamState(session);
  app.applyResponseStreamEvent(session, streamState, 'response.output_item.added', {
    output_index: 0,
    item: { type: 'function_call', call_id: 'call_img_hub', name: 'image_generate' },
  });
  app.applyResponseStreamEvent(session, streamState, 'response.tool_exec.end', {
    call_id: 'call_img_hub',
    tool_name: 'image_generate',
    success: true,
    images: ['/ui/images/generated.png'],
  });

  const tool = session.messages.find((message) => message.role === 'tool-group')?.tools?.[0];
  if (!tool || !Array.isArray(tool.images) || tool.images[0] !== '/hub/node/alpha/images/generated.png') {
    fail(name, 'tool image URL was not rebased through helper', JSON.stringify(session.messages));
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

async function testSendButtonMorphsToInterjectWhileBusyAndTyping() {
  const name = 'send button morphs to interject while busy and typing';
  const harness = createHarness();
  const { app, elements, state, cleanup } = harness;

  app.setStreaming(true);
  if (!elements.sendBtn.classList.contains('loading')) {
    fail(name, 'expected send button to show loading when busy and composer is empty');
    await cleanup();
    return;
  }
  if (elements.sendBtn._arrow.textContent !== '↑') {
    fail(name, `empty busy arrow = ${elements.sendBtn._arrow.textContent}, want ↑`);
    await cleanup();
    return;
  }

  elements.promptInput.value = 'quick note';
  app.autoGrowPrompt();
  if (elements.sendBtn.classList.contains('loading')) {
    fail(name, 'expected loading spinner to hide once user types while busy');
    await cleanup();
    return;
  }
  if (!elements.sendBtn.classList.contains('interject')) {
    fail(name, 'expected interject class once user types while busy');
    await cleanup();
    return;
  }
  if (elements.sendBtn._arrow.textContent !== '↳') {
    fail(name, `busy typed arrow = ${elements.sendBtn._arrow.textContent}, want ↳`);
    await cleanup();
    return;
  }
  if (elements.sendBtn.title !== 'Interject' || elements.sendBtn['aria-label'] !== 'Interject') {
    fail(name, `button labels = ${elements.sendBtn.title}/${elements.sendBtn['aria-label']}, want Interject`);
    await cleanup();
    return;
  }

  elements.promptInput.value = '';
  state.attachments.push({ id: 'att_1', name: 'image.png', type: 'image/png' });
  app.updateSendButtonState();
  if (!elements.sendBtn.classList.contains('interject') || elements.sendBtn._arrow.textContent !== '↳') {
    fail(name, 'expected attachment-only busy composer to use interject affordance');
    await cleanup();
    return;
  }

  app.setStreaming(false);
  if (elements.sendBtn.classList.contains('loading') || elements.sendBtn.classList.contains('interject') || elements.sendBtn._arrow.textContent !== '↑') {
    fail(name, 'expected send button to return to normal when idle');
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testAutoGrowPromptUsesNativeFieldSizingWithoutLayoutReads() {
  const name = 'autoGrowPrompt uses native field-sizing without layout reads';
  let supportsCalls = 0;
  const harness = createHarness({
    CSS: {
      supports(property, value) {
        supportsCalls += 1;
        return property === 'field-sizing' && value === 'content';
      },
    },
    requestAnimationFrame() {
      fail(name, 'native field-sizing should not schedule fallback sizing');
      return 0;
    },
  });
  const { app, elements, cleanup } = harness;
  let scrollReads = 0;
  Object.defineProperty(elements.promptInput, 'scrollHeight', {
    configurable: true,
    get() {
      scrollReads += 1;
      return 96;
    },
  });

  elements.promptInput.value = 'hello';
  app.autoGrowPrompt();

  if (supportsCalls !== 1) {
    fail(name, `expected one feature-detection call, got ${supportsCalls}`);
    await cleanup();
    return;
  }
  if (scrollReads !== 0 || elements.promptInput.style.height) {
    fail(name, `expected no fallback layout work, scrollReads=${scrollReads} height=${elements.promptInput.style.height || ''}`);
    await cleanup();
    return;
  }
  if (elements.sendBtn.title !== 'Send message') {
    fail(name, 'send button should still update while native sizing is active');
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testAutoGrowPromptFallbackCoalescesLayoutWork() {
  const name = 'autoGrowPrompt fallback coalesces layout work';
  const frames = [];
  const harness = createHarness({
    requestAnimationFrame(callback) {
      frames.push(callback);
      return frames.length;
    },
  });
  const { app, elements, cleanup } = harness;
  let scrollReads = 0;
  Object.defineProperty(elements.promptInput, 'scrollHeight', {
    configurable: true,
    get() {
      scrollReads += 1;
      return 96;
    },
  });

  elements.promptInput.value = 'one';
  app.autoGrowPrompt();
  elements.promptInput.value = 'one two';
  app.autoGrowPrompt();

  if (frames.length !== 1) {
    fail(name, `expected one animation frame for repeated calls, got ${frames.length}`);
    await cleanup();
    return;
  }
  if (scrollReads !== 0 || elements.promptInput.style.height) {
    fail(name, `fallback should defer layout reads until the frame, scrollReads=${scrollReads}`);
    await cleanup();
    return;
  }

  frames.shift()();
  if (scrollReads !== 1 || elements.promptInput.style.height !== '96px') {
    fail(name, `expected one layout read and 96px height, reads=${scrollReads} height=${elements.promptInput.style.height || ''}`);
    await cleanup();
    return;
  }

  elements.promptInput.value = 'one two three';
  app.autoGrowPrompt();
  if (frames.length !== 1) {
    fail(name, `changed same-height value should schedule fallback sizing, got ${frames.length}`);
    await cleanup();
    return;
  }
  frames.shift()();
  if (scrollReads !== 2 || elements.promptInput.style.height !== '96px') {
    fail(name, `changed same-height value should restore measured height, reads=${scrollReads} height=${elements.promptInput.style.height || ''}`);
    await cleanup();
    return;
  }

  app.autoGrowPrompt();
  if (frames.length !== 0) {
    fail(name, `unchanged value should not schedule fallback sizing, got ${frames.length} frame(s)`);
    await cleanup();
    return;
  }
  if (scrollReads !== 2) {
    fail(name, `unchanged value should skip fallback layout read, got ${scrollReads}`);
    await cleanup();
    return;
  }

  elements.promptInput.value = 'capped';
  Object.defineProperty(elements.promptInput, 'scrollHeight', {
    configurable: true,
    get() {
      scrollReads += 1;
      return 360;
    },
  });
  app.autoGrowPrompt();
  if (frames.length !== 1) {
    fail(name, `capped value should schedule fallback sizing, got ${frames.length}`);
    await cleanup();
    return;
  }
  frames.shift()();
  if (elements.promptInput.style.height !== '300px' || elements.promptInput.style.overflowY !== 'auto') {
    fail(name, `expected tall prompt to cap at 300px with scrolling, height=${elements.promptInput.style.height || ''} overflow=${elements.promptInput.style.overflowY || ''}`);
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
  const interruptBody = JSON.parse(interruptCalls[0].body || '{}');
  if (!interruptBody.interjection_id || interruptCalls[0].headers?.['Idempotency-Key'] !== interruptBody.interjection_id) {
    fail(name, 'interrupt retry key should match its stable interjection id', JSON.stringify(interruptCalls[0]));
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

async function testStaleInterruptRecoveryFailedPostKeepsDraft() {
  const name = 'interrupt recovery keeps draft when fresh POST fails';
  const harness = createHarness({
    interruptStatus: 404,
    interruptErrorPayload: { error: { message: 'session runtime not found' } },
    postStatus: 400,
    postErrorPayload: { error: { message: 'bad request' } },
  });
  const { app, elements, state, localStorage, cleanup } = harness;
  const session = {
    id: 'session_stale_interrupt_failed_post',
    title: 'Stale interrupt failed post',
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
  elements.promptInput.value = 'keep after failed recovery';

  app.syncActiveSessionFromServer = async () => {
    app.clearActiveResponseTracking(session, session.activeResponseId || state.currentStreamResponseId);
    app.setSessionOptimisticBusy(session, false);
    app.setSessionServerActiveRun(session, false);
    app.setStreaming(false);
    return { active_run: false, active_response_id: '' };
  };

  await app.sendMessage();
  await cleanup();

  const drafts = JSON.parse(localStorage.getItem('draftMessages') || '[]');
  if (drafts.length !== 1 || drafts[0].sessionId !== session.id || drafts[0].prompt !== 'keep after failed recovery') {
    fail(name, 'failed recovered POST should leave session draft staged', JSON.stringify(drafts));
    return;
  }
  if (elements.promptInput.value !== 'keep after failed recovery') {
    fail(name, 'composer should be restored after failed recovered POST', elements.promptInput.value);
    return;
  }
  pass(name);
}

async function testFailedSendKeepsSessionDraftAndRestagesComposer() {
  const name = 'failed send keeps a session-bound draft and restores the composer';
  const harness = createHarness({ postStatus: 400, postErrorPayload: { error: { message: 'bad request' } } });
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

async function testSuccessfulNewChatSendClearsNewConversationDraft() {
  const name = 'successful New Chat send clears the new-conversation draft';
  const harness = createHarness();
  const { app, elements, state, localStorage, cleanup } = harness;
  state.draftSessionActive = true;
  state.activeSessionId = '';
  localStorage.setItem('draftMessages', JSON.stringify([
    { id: 'new_draft', sessionId: '', prompt: 'hello from new chat', created: 1 },
    { id: 'other_draft', sessionId: 'other_session', prompt: 'other text', created: 2 },
  ]));
  app.restoreDraftMessageForSession('', { replace: true });

  await app.sendMessage();
  await cleanup();

  const drafts = JSON.parse(localStorage.getItem('draftMessages') || '[]');
  const createdSession = state.sessions[0];
  if (drafts.some((draft) => String(draft.sessionId || '') === '')) {
    fail(name, 'new-conversation draft should be cleared after successful send', JSON.stringify(drafts));
    return;
  }
  if (createdSession && drafts.some((draft) => draft.sessionId === createdSession.id)) {
    fail(name, 'created session draft should also be cleared after successful send', JSON.stringify(drafts));
    return;
  }
  if (drafts.length !== 1 || drafts[0].id !== 'other_draft') {
    fail(name, 'only unrelated session drafts should remain', JSON.stringify(drafts));
    return;
  }
  pass(name);
}

function testClearDraftMessageForSessionRemovesLogicalBucket() {
  const name = 'clearDraftMessageForSession removes the matching conversation draft';
  const harness = createHarness();
  const { app, localStorage } = harness;
  localStorage.setItem('draftMessages', JSON.stringify([
    { id: 'd1', sessionId: 'session_a', prompt: 'draft A', created: 1 },
    { id: 'd2', sessionId: 'session_b', prompt: 'draft B', created: 2 },
    { id: 'd3', sessionId: '', prompt: 'new draft', created: 3 },
  ]));

  app.clearDraftMessageForSession('session_a');
  const drafts = JSON.parse(localStorage.getItem('draftMessages') || '[]');
  if (drafts.some((draft) => draft.sessionId === 'session_a')) {
    fail(name, 'session_a draft should be removed', JSON.stringify(drafts));
    return;
  }
  if (!drafts.some((draft) => draft.sessionId === 'session_b') || !drafts.some((draft) => String(draft.sessionId || '') === '')) {
    fail(name, 'unrelated drafts should remain', JSON.stringify(drafts));
    return;
  }
  pass(name);
}

function testDraftMessageLimitIsTen() {
  const name = 'draft messages are capped at ten LRU entries';
  const harness = createHarness();
  const { app, localStorage } = harness;
  for (let i = 0; i < 12; i += 1) {
    app.stageDraftMessage(`draft ${i}`, `session_${i}`);
  }
  const drafts = JSON.parse(localStorage.getItem('draftMessages') || '[]');
  if (drafts.length !== 10) {
    fail(name, `expected 10 drafts, got ${drafts.length}`, JSON.stringify(drafts));
    return;
  }
  if (drafts.some((draft) => draft.sessionId === 'session_0' || draft.sessionId === 'session_1')) {
    fail(name, 'oldest drafts should be evicted first', JSON.stringify(drafts));
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

async function testQueueEffortWhileStreamingPostsRuntimeEffortAndShowsPending() {
  const name = 'queueing effort while streaming posts runtime effort and marks pending only while queued';
  const harness = createHarness();
  const { app, state, fetchCalls, cleanup } = harness;
  const session = {
    id: 'sess_effort_queue',
    title: 'Effort queue',
    messages: [{ id: 'u1', role: 'user', content: 'run', created: 1 }],
    activeResponseId: 'resp_effort_queue',
    activeModel: 'gpt-5.4',
    activeEffort: 'medium',
    provider: 'chatgpt',
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  state.currentStreamSessionId = session.id;
  state.streaming = true;

  await app.applyEffortChange('high');

  const effortCall = fetchCalls.find(call => call.url === '/ui/v1/sessions/sess_effort_queue/runtime/effort');
  if (!effortCall) {
    fail(name, 'expected runtime effort POST', JSON.stringify(fetchCalls));
    await cleanup();
    return;
  }
  const body = JSON.parse(effortCall.body || '{}');
  if (body.model !== 'gpt-5.4' || body.reasoning_effort !== 'high') {
    fail(name, 'unexpected runtime effort body', effortCall.body);
    await cleanup();
    return;
  }
  if (!session.pendingEffortQueued || session.pendingEffort !== 'high') {
    fail(name, 'expected pending effort marker', JSON.stringify({ pendingEffort: session.pendingEffort, pendingEffortQueued: session.pendingEffortQueued }));
    await cleanup();
    return;
  }
  if (state.selectedEffort !== 'high') {
    fail(name, `selectedEffort = ${JSON.stringify(state.selectedEffort)}, want high`);
    await cleanup();
    return;
  }

  await cleanup();
  pass(name);
}

async function testEquivalentImplicitEffortWhileStreamingDoesNotQueue() {
  const name = 'implicit default effort does not queue an equivalent runtime switch';
  const harness = createHarness();
  const { app, state, fetchCalls, cleanup } = harness;
  const session = {
    id: 'sess_effort_implicit',
    title: 'Implicit effort',
    messages: [{ id: 'u1', role: 'user', content: 'run', created: 1 }],
    activeResponseId: 'resp_effort_implicit',
    activeModel: 'gpt-5.6-sol',
    activeEffort: '',
    provider: 'chatgpt',
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  state.currentStreamSessionId = session.id;
  state.streaming = true;
  state.selectedModel = 'gpt-5.6-sol';
  state.modelInfoByID = {
    'gpt-5.6-sol': { id: 'gpt-5.6-sol', default_reasoning_effort: 'medium' },
  };

  await app.applyEffortChange('medium');

  const effortCall = fetchCalls.find(call => call.url === '/ui/v1/sessions/sess_effort_implicit/runtime/effort');
  if (effortCall) {
    fail(name, 'equivalent implicit medium → explicit medium queued a runtime switch', effortCall.body);
    await cleanup();
    return;
  }
  if (session.pendingEffortQueued) {
    fail(name, 'equivalent effort was marked pending');
    await cleanup();
    return;
  }
  await cleanup();
  pass(name);
}

function testResponseModelSwitchStabilizesEffortAndClearsPending() {
  const name = 'response.model_switch updates effort and clears queued marker';
  const harness = createHarness();
  const { app, state } = harness;
  const session = {
    id: 'sess_effort_applied',
    title: 'Effort applied',
    messages: [],
    activeResponseId: 'resp_effort_applied',
    activeModel: 'gpt-5.4',
    activeEffort: 'medium',
    pendingEffort: 'high',
    pendingEffortQueued: true,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  const streamState = app.createResponseStreamState(session);
  app.applyResponseStreamEvent(session, streamState, 'response.model_switch', {
    model: 'gpt-5.4',
    reasoning_effort: 'high',
    sequence_number: 7,
  });

  if (session.activeModel !== 'gpt-5.4' || session.activeEffort !== 'high') {
    fail(name, 'expected active runtime to update', JSON.stringify({ model: session.activeModel, effort: session.activeEffort }));
    return;
  }
  if (session.pendingEffortQueued || Object.prototype.hasOwnProperty.call(session, 'pendingEffort')) {
    fail(name, 'expected queued marker to be cleared after apply', JSON.stringify({ pendingEffort: session.pendingEffort, pendingEffortQueued: session.pendingEffortQueued }));
    return;
  }
  if (state.selectedEffort !== 'high') {
    fail(name, `selectedEffort = ${JSON.stringify(state.selectedEffort)}, want high`);
    return;
  }
  pass(name);
}

function testCompletedResponseClearsUnappliedQueuedEffort() {
  const name = 'response.completed clears queued effort without persistent applied banner state';
  const harness = createHarness();
  const { app, state } = harness;
  const session = {
    id: 'sess_effort_done',
    title: 'Effort done',
    messages: [],
    activeResponseId: 'resp_effort_done',
    activeModel: 'gpt-5.4',
    activeEffort: 'medium',
    pendingEffort: 'high',
    pendingEffortQueued: true,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;

  const streamState = app.createResponseStreamState(session);
  app.applyResponseStreamEvent(session, streamState, 'response.completed', {
    response: { id: 'resp_effort_done', model: 'gpt-5.4', status: 'completed', reasoning_effort: 'medium' },
    sequence_number: 8,
  });

  if (session.pendingEffortQueued || Object.prototype.hasOwnProperty.call(session, 'pendingEffort')) {
    fail(name, 'expected queued marker to be cleared at terminal event', JSON.stringify({ pendingEffort: session.pendingEffort, pendingEffortQueued: session.pendingEffortQueued }));
    return;
  }
  if (session.activeEffort !== 'medium') {
    fail(name, `activeEffort = ${JSON.stringify(session.activeEffort)}, want medium`);
    return;
  }
  pass(name);
}

async function testWebSlashCommandsInvokeExistingControls() {
  const name = 'web slash commands invoke existing controls';
  const harness = createHarness();
  const { app, elements, cleanup } = harness;
  const calls = [];
  app.openGoalModal = () => { calls.push('goal'); };
  app.openSessionMCPModal = async () => { calls.push('mcp'); };
  elements.chipModelTrigger.click = () => { calls.push('model'); };
  app.createAndSwitchToFreshSession = async () => { calls.push('new'); };

  for (const command of ['/goal', '/mcp', '/model', '/new']) {
    elements.promptInput.value = command;
    await app.sendMessage();
    if (elements.promptInput.value !== '') {
      fail(name, `${command} did not clear the composer`);
      await cleanup();
      return;
    }
  }
  await cleanup();

  if (JSON.stringify(calls) !== JSON.stringify(['goal', 'mcp', 'model', 'new'])) {
    fail(name, 'commands did not invoke their existing controls', JSON.stringify(calls));
    return;
  }
  pass(name);
}

async function testCompressCommandCompactsWithoutSendingMessage() {
  const name = '/compress compacts active context without sending a chat message';
  const harness = createHarness({
    fetchImpl: async (url, requestOptions, { Response }) => {
      if (url === '/ui/v1/sessions/session_compress/runtime/compact' && requestOptions.method === 'POST') {
        return new Response(JSON.stringify({ ok: true }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      return new Response('unexpected request', { status: 500 });
    },
  });
  const { app, elements, state, fetchCalls, cleanup } = harness;
  const session = {
    id: 'session_compress',
    title: 'Compress',
    messages: [],
    lastResponseId: null,
    activeResponseId: null,
    lastSequenceNumber: 0,
    number: 1,
  };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  elements.promptInput.value = '/compress';
  let refreshOptions = null;
  app.refreshActiveSessionMessagesFromServer = async (_session, options) => {
    refreshOptions = options;
    return true;
  };

  await app.sendMessage();
  elements.promptInput.value = '/compact';
  await app.sendMessage();
  await cleanup();

  const compactCalls = fetchCalls.filter((call) => call.url === '/ui/v1/sessions/session_compress/runtime/compact');
  const responseCalls = fetchCalls.filter((call) => call.url === '/ui/v1/responses');
  if (compactCalls.length !== 2 || compactCalls.some((call) => call.method !== 'POST' || call.body !== '{}')) {
    fail(name, 'expected /compress and /compact to issue compact POSTs', JSON.stringify(fetchCalls));
    return;
  }
  if (responseCalls.length !== 0 || session.messages.length !== 0) {
    fail(name, 'command was sent as a normal conversation message', JSON.stringify({ responseCalls, messages: session.messages }));
    return;
  }
  if (!refreshOptions?.force || refreshOptions?.useEtag !== false || elements.promptInput.value !== '') {
    fail(name, 'successful compression did not force-refresh and clear the composer', JSON.stringify({ refreshOptions, prompt: elements.promptInput.value }));
    return;
  }
  if (state.compressing || elements.sendBtn.classList.contains('loading')) {
    fail(name, 'compression left the composer busy');
    return;
  }
  pass(name);
}

async function testUnknownSlashTextRemainsNormalMessage() {
  const name = 'unknown slash text remains a normal message';
  const harness = createHarness({ matchSkillInvocation: () => null });
  const { app, elements, state, fetchCalls, cleanup } = harness;
  const session = { id: 'session_unknown_slash', title: 'Slash', messages: [], activeResponseId: null, lastResponseId: null, lastSequenceNumber: 0 };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  elements.promptInput.value = '/tmp/file should be explained';

  await app.sendMessage();

  const normalPost = fetchCalls.find((call) => call.url === '/ui/v1/responses' && call.method === 'POST');
  const skillPost = fetchCalls.find((call) => call.url.endsWith('/skills/invoke'));
  if (!normalPost || skillPost || !String(normalPost.body || '').includes('/tmp/file should be explained')) {
    fail(name, 'unknown slash text was not sent as ordinary input', JSON.stringify(fetchCalls));
    await cleanup();
    return;
  }
  await cleanup();
  pass(name);
}

async function testMainSkillUsesStructuredInvocationAndResponseStream() {
  const name = 'main skill uses structured invocation and normal response stream';
  const responseId = 'resp_skill_main';
  const harness = createHarness({
    responseId,
    matchSkillInvocation(value) {
      return value === '/explain src/main.go'
        ? { name: 'explain', arguments: 'src/main.go', execution: 'main', invocation: value }
        : null;
    },
    fetchImpl: async (url, requestOptions, { Response, ReadableStream, encoder }) => {
      if (url === '/ui/v1/sessions/session_skill_main/skills/invoke') {
        return new Response(JSON.stringify({ execution: 'main', response_id: responseId, events_url: `/v1/responses/${responseId}/events` }), {
          status: 202,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      if (url === `/ui/v1/responses/${responseId}/events?after=0`) {
        const body = [
          'id: 1\n',
          'event: response.created\n',
          `data: {"response":{"id":"${responseId}","status":"in_progress"},"sequence_number":1}\n\n`,
          'id: 2\n',
          'event: response.output_text.delta\n',
          'data: {"delta":"explained","sequence_number":2}\n\n',
          'id: 3\n',
          'event: response.completed\n',
          `data: {"response":{"id":"${responseId}","status":"completed"},"sequence_number":3}\n\n`,
        ].join('');
        return new Response(new ReadableStream({ start(controller) { controller.enqueue(encoder.encode(body)); controller.close(); } }), {
          status: 200,
          headers: { 'Content-Type': 'text/event-stream' },
        });
      }
      throw new Error(`unexpected fetch: ${url}`);
    },
  });
  const { app, elements, state, fetchCalls, cleanup } = harness;
  const session = { id: 'session_skill_main', title: 'Skills', messages: [], activeResponseId: null, lastResponseId: null, lastSequenceNumber: 0 };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  elements.promptInput.value = '/explain src/main.go';

  await app.sendMessage();

  const invoke = fetchCalls.find((call) => call.url.endsWith('/skills/invoke'));
  const body = invoke?.body ? JSON.parse(invoke.body) : null;
  if (!invoke || invoke.method !== 'POST' || body?.name !== 'explain' || body?.arguments !== 'src/main.go') {
    fail(name, 'structured invocation request was not sent', JSON.stringify(fetchCalls));
    await cleanup();
    return;
  }
  if (!session.messages.some((message) => message.role === 'user' && message.content === '/explain src/main.go')) {
    fail(name, 'concise slash invocation was not displayed', JSON.stringify(session.messages));
    await cleanup();
    return;
  }
  if (!session.messages.some((message) => message.role === 'assistant' && message.content === 'explained')) {
    fail(name, 'normal response event stream was not consumed', JSON.stringify(session.messages));
    await cleanup();
    return;
  }
  await cleanup();
  pass(name);
}

async function testIsolatedSkillStreamsIndependentlyAndCancelsIndependently() {
  const name = 'isolated skill streams and cancels independently of parent';
  const runId = 'skill_run_1';
  let cancelCalled = false;
  let releaseRun;
  const runDone = new Promise((resolve) => { releaseRun = resolve; });
  const harness = createHarness({
    matchSkillInvocation(value) {
      return value === '/review staged'
        ? { name: 'review', arguments: 'staged', execution: 'isolated', invocation: value }
        : null;
    },
    fetchImpl: async (url, requestOptions, { Response, ReadableStream, encoder }) => {
      if (url === '/ui/v1/sessions/session_skill_child/skills/invoke') {
        return new Response(JSON.stringify({
          execution: 'isolated',
          run_id: runId,
          child_session_id: 'child_1',
          events_url: `/v1/sessions/session_skill_child/skill-runs/${runId}/events`,
        }), { status: 202, headers: { 'Content-Type': 'application/json' } });
      }
      if (url.startsWith(`/ui/v1/sessions/session_skill_child/skill-runs/${runId}/events?after=`)) {
        return new Response(new ReadableStream({
          async start(controller) {
            controller.enqueue(encoder.encode([
              'id: 1\n',
              'event: skill_run.created\n',
              `data: {"sequence":1,"type":"skill_run.created","data":{"run_id":"${runId}","skill":"review","agent":"reviewer","child_session_id":"child_1"}}\n\n`,
              'id: 2\n',
              'event: skill_run.progress\n',
              'data: {"sequence":2,"type":"skill_run.progress","data":{"Type":"phase","Phase":"checking diff"}}\n\n',
            ].join('')));
            await runDone;
            controller.enqueue(encoder.encode([
              'id: 3\n',
              'event: skill_run.completed\n',
              'data: {"sequence":3,"type":"skill_run.completed","data":{"status":"cancelled","output":"partial review","child_session_id":"child_1"}}\n\n',
            ].join('')));
            controller.close();
          },
        }), { status: 200, headers: { 'Content-Type': 'text/event-stream' } });
      }
      if (url === `/ui/v1/sessions/session_skill_child/skill-runs/${runId}` && requestOptions.method === 'DELETE') {
        cancelCalled = true;
        releaseRun();
        return new Response(JSON.stringify({ id: runId, status: 'cancelling' }), { status: 202, headers: { 'Content-Type': 'application/json' } });
      }
      throw new Error(`unexpected fetch: ${url}`);
    },
  });
  const { app, elements, state, cleanup } = harness;
  const session = { id: 'session_skill_child', title: 'Skills', messages: [], activeResponseId: 'resp_parent', lastResponseId: null, lastSequenceNumber: 0 };
  state.sessions.push(session);
  state.activeSessionId = session.id;
  state.streaming = true;
  state.currentStreamSessionId = session.id;
  state.currentStreamResponseId = 'resp_parent';
  elements.promptInput.value = '/review staged';

  await app.sendMessage();
  const progressVisible = await waitFor(() => session.messages.some((message) => message.role === 'skill-run' && message.progress === 'checking diff'), 500);
  if (!progressVisible || !state.streaming || state.currentStreamResponseId !== 'resp_parent') {
    fail(name, 'child progress disturbed or failed to coexist with parent stream', JSON.stringify({ state, messages: session.messages }));
    releaseRun();
    await cleanup();
    return;
  }

  await app.cancelSkillRun(session.id, runId);
  const terminal = await waitFor(() => session.messages.some((message) => message.runId === runId && message.status === 'cancelled'), 500);
  const message = session.messages.find((entry) => entry.runId === runId);
  if (!cancelCalled || !terminal || message?.output !== 'partial review') {
    fail(name, 'cancel did not preserve the isolated partial result', JSON.stringify({ cancelCalled, message }));
    await cleanup();
    return;
  }
  if (!state.streaming || state.currentStreamResponseId !== 'resp_parent') {
    fail(name, 'isolated cancellation changed parent stream state', JSON.stringify(state));
    await cleanup();
    return;
  }
  await cleanup();
  pass(name);
}

(async () => {
  await testUnknownSlashTextRemainsNormalMessage();
  await testMainSkillUsesStructuredInvocationAndResponseStream();
  await testIsolatedSkillStreamsIndependentlyAndCancelsIndependently();
  await testModelEffortOptionsFollowMetadata();
  await testResponseCreatedRecordsStartedTranscriptRevision();
  await testResponseCompletedForcesSidebarStatusRefresh();
  await testResponseCompletedPreservesFailedToolStatus();
  await testStaleTerminalStreamDoesNotRefreshStatus();
  await testConsumeResponseStreamIgnoresAlreadyProjectedEvents();
  await testConsumeResponseStreamPreservesOverflowRecoverySequenceException();
  await testSkippedReplayRehydratesStreamProjectionState();
  await testInactiveSessionStreamEventsDoNotAppendToVisibleDOM();
  await testInactiveExistingMessageUpdatesDoNotTouchVisibleDOM();
  await testInactiveInterruptHelpersDoNotTouchVisibleDOM();
  await testInactiveSessionPromptEventsRemainActionable();
  await testInactiveSessionFailureDoesNotAppendToVisibleDOM();
  await testConsumeResponseStreamReportsStaleWithoutApplyingEvents();
  await testParseSSEStreamUpdatesHeartbeatOnCommentFrame();
  await testSendMessageHeartbeatCancelsPostStreamWithoutAbortingFetch();
  await testSendMessageHeartbeatCancellationWithoutResponseIDRetriesPost();
  await testSendMessageHeartbeatAbortRetriesBeforeResponseId();
  await testSendMessageLargeUploadUsesLongerPreResponseHeartbeatGrace();
  await testSendMessageTransientPreResponseFailureRetries();
  await testSendMessageHeartbeatAbortKeepsRetryingWithSlowBackoff();
  await testSendMessageDoesNotResumeAfterStalePostStream();
  await testSendMessageUsesLocalContinuationIdWithoutPreflightSync();
  await testSendMessageIncludesServerToolsForFirstPartyUI();
  await testSendMessageRecoversStaleContinuationAfterConflict();
  await testSendMessageConsumesPostStreamWhenAvailable();
  await testSendMessageRefreshesHeaderAfterCompletionUnlocksModelPicker();
  await testSendMessageLazilyMaterializesAttachmentDataURLs();
  await testSendMessageKeepsComposerWhenAttachmentMaterializationFails();
  await testWebSlashCommandsInvokeExistingControls();
  await testCompressCommandCompactsWithoutSendingMessage();
  await testStaleInterrupt404RefreshesAndSendsMessage();
  await testStaleInterruptRecoveryFailedPostKeepsDraft();
  await testFailedSendKeepsSessionDraftAndRestagesComposer();
  await testSuccessfulSendRemovesOnlyMatchingDraft();
  await testSuccessfulNewChatSendClearsNewConversationDraft();
  testClearDraftMessageForSessionRemovesLogicalBucket();
  testDraftMessageLimitIsTen();
  testRestoreDraftMessageForSessionIsSessionBound();
  testRestoreLatestDraftMessageDoesNotCrossSessionBoundary();
  await testSendButtonMorphsToInterjectWhileBusyAndTyping();
  await testAutoGrowPromptUsesNativeFieldSizingWithoutLayoutReads();
  await testAutoGrowPromptFallbackCoalescesLayoutWork();
  await testNonImageAttachmentsDoNotCreatePreviewObjectURLs();
  await testSendMessageResumesFromEventsAfterPostStreamDrops();
  await testSendMessageRecoversAfterStreamBufferOverflowDone();
  await testNewChatDuringStreamingClearsStreamingState();
  await testDraftSendIgnoresStaleGlobalStreamingFlag();
  await testSendMessageMarksSessionBusyImmediately();
  await testDrainInterruptQueueAfterResumeCompletes();
  await testDrainInterruptQueueIgnoresOtherSessionEntries();
  await testResumeActiveResponseRecoversFromSnapshotBeforeReplaying();
  await testRecoverySnapshotReconcilesDurableToolCallsIdempotently();
  await testResumeActiveResponseRepairsSequenceGapWithSnapshot();
  await testRecoverySnapshotClearsSyntheticPendingInterjectionByText();
  await testRecoverySnapshotDoesNotDuplicateOptimisticInterjection();
  await testFunctionCallArgumentDeltasFillToolPrompt();
  await testArgumentDeltaWithoutOutputIndexUsesLastRunningTool();
  await testArgumentDeltasContinueUntilOutputItemDone();
  await testSeededToolArgumentsIgnoreReplayDeltas();
  await testSuccessfulPlanToolCompletionRefetchesAuthoritativeState();
  await testToolExecImagesAttachToToolArtifactNotAssistantMarkdown();
  await testToolExecImagesUseHubAssetRebase();
  await testResumeActiveResponseHeartbeatCancelSlowsAndRecovers();
  await testResumeReconnectBackoffCanBeWokenWithoutDuplicateLoop();
  await testDetachDuringSlowReconnectTransfersResumeOwnership();
  await testResumeActiveResponseFallsBackToReplayWhenSnapshotUnavailable();
  await testResumeActiveResponseClearsTerminalTrackingWhen409SnapshotHasNoRecovery();
  await testSendMessageIncludesModelSwapForChangedTarget();
  await testSendMessageOmitsModelSwapWhenTargetUnchanged();
  await testSendMessageIgnoresAutomaticEffortDriftWithoutUserIntent();
  await testSendMessageIgnoresAutomaticExplicitEffortDriftWithoutUserIntent();
  await testExplicitAutoEffortStillRequestsModelSwap();
  await testSendMessageTreatsImplicitDefaultEffortAsEquivalent();
  await testSendMessageGatesReasoningModeByModelMetadata();
  await testQueueEffortWhileStreamingPostsRuntimeEffortAndShowsPending();
  await testEquivalentImplicitEffortWhileStreamingDoesNotQueue();
  testResponseModelSwitchStabilizesEffortAndClearsPending();
  testCompletedResponseClearsUnappliedQueuedEffort();
  testModelSwapProgressEventUpdatesTransientMarker();
  testGuardianReviewEventIsDisplayOnlyTransient();
  testResponsePhaseEventUpdatesTransientMarker();
  testResponseRetryEventUpdatesOwnedHeaderStatus();
  testProviderRetryClearsOnMeaningfulProgress();
  testProviderRetryPersistsAcrossNonProgressEvents();
  testProviderRetryClearsOnTerminalEvents();
  testActiveResponseTransitionClearsObsoleteRetryOwner();
  testProviderRetryOwnershipGuardsBackgroundDetachAndStaleClear();
  testResponsePhaseUpdateCanStraddleResumedOutput();
  testResponsePhaseSeparatesAssistantSegments();
  await testSettingsEffortCancelDoesNotAuthorizeSwap();
  await testSettingsEffortSaveAuthorizesSwap();
  await testConnectTokenPreservesSelectedModelAndProviderFromState();
  await testCancelActiveResponseTearsDownLocallyBeforeServerPost();
  await testInterjectionClosesToolGroupAndInsertsUserMessageAtTail();
  await testReplayedInterjectionWithoutIdDoesNotDuplicateExistingInjectedMessage();
  await testCommittedInterjectionWithRealIdClearsStaleSyntheticPending();
  await testCommittedInterjectionReusesOptimisticMessageEvenWhenPendingTrackedUnderServerId();
  await testInterjectQueuedShowsPendingBadgeThenInjectedOnCommit();
  await testUserCancelDiscardsPendingInterjectionStateButPreservesFollowUpQueue();
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
