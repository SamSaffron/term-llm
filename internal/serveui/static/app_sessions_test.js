#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const { webcrypto } = require('crypto');

const dir = __dirname;
const coreSource = fs.readFileSync(path.join(dir, 'app-core.js'), 'utf8');
const sessionsSource = fs.readFileSync(path.join(dir, 'app-sessions.js'), 'utf8')
  .replace('initialize();', 'window.__termllmInitializePromise = initialize();');

let failures = 0;

function fail(name, message, details) {
  console.error('FAIL:', name, '-', message);
  if (details) console.error('      ', details);
  failures += 1;
}

function pass(name) {
  console.log('PASS:', name);
}

function makeClassList() {
  const classes = new Set();
  return {
    add(...names) { names.forEach((name) => classes.add(name)); },
    remove(...names) { names.forEach((name) => classes.delete(name)); },
    toggle(name, force) {
      if (force === true) classes.add(name);
      else if (force === false) classes.delete(name);
      else if (classes.has(name)) classes.delete(name);
      else classes.add(name);
    },
    contains(name) { return classes.has(name); },
  };
}

function makeNode() {
  return {
    classList: makeClassList(),
    style: {},
    dataset: {},
    hidden: false,
    disabled: false,
    value: '',
    textContent: '',
    innerHTML: '',
    scrollHeight: 0,
    scrollTop: 0,
    clientHeight: 0,
    appendChild(node) { return node; },
    removeChild() {},
    querySelector() { return null; },
    querySelectorAll() { return []; },
    setAttribute(name, value) { this[name] = value; },
    removeAttribute(name) { delete this[name]; },
    addEventListener() {},
    focus() {},
    select() {},
    remove() {},
    click() {},
  };
}

async function testNumericDeepLinkResolvesRealSessionId() {
  const name = 'numeric deep link resolves server session id';
  const fetchCalls = [];
  const storage = new Map();

  const document = {
    cookie: '',
    visibilityState: 'visible',
    body: { classList: makeClassList() },
    documentElement: { style: { setProperty() {} } },
    getElementById() { return makeNode(); },
    createElement() { return makeNode(); },
    querySelector() { return makeNode(); },
    querySelectorAll() { return []; },
    addEventListener() {},
  };

  const localStorage = {
    getItem(key) { return storage.has(key) ? storage.get(key) : null; },
    setItem(key, value) { storage.set(key, String(value)); },
    removeItem(key) { storage.delete(key); },
  };

  const windowObj = {
    TERM_LLM_UI_PREFIX: '/ui',
    TERM_LLM_SIDEBAR_SESSIONS: 'all',
    location: { pathname: '/ui/1291', search: '', origin: 'https://example.test' },
    history: { pushState(_state, _title, url) { windowObj.location.pathname = url; } },
    matchMedia() {
      return {
        matches: false,
        addEventListener() {},
        addListener() {},
        removeEventListener() {},
        removeListener() {},
      };
    },
    navigator: { standalone: false, mediaDevices: null, serviceWorker: null },
    visualViewport: null,
    addEventListener() {},
    removeEventListener() {},
    requestAnimationFrame(callback) { return setTimeout(callback, 0); },
    cancelAnimationFrame(handle) { clearTimeout(handle); },
    setTimeout,
    clearTimeout,
    setInterval() { return 0; },
    clearInterval() {},
    focus() {},
  };
  windowObj.document = document;
  windowObj.localStorage = localStorage;

  const context = {
    window: windowObj,
    document,
    localStorage,
    history: windowObj.history,
    location: windowObj.location,
    navigator: windowObj.navigator,
    console,
    setTimeout,
    clearTimeout,
    setInterval() { return 0; },
    clearInterval() {},
    URL,
    URLSearchParams,
    fetch: async (url) => {
      fetchCalls.push(url);
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({
          sessions: [{
            id: 'sess_real',
            number: 1291,
            short_title: 'Real session',
            long_title: 'Real session',
            mode: 'chat',
            origin: 'tui',
            archived: false,
            pinned: false,
            created_at: 1710000000000,
            message_count: 1,
          }]
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/sess_real/messages') {
        return new Response(JSON.stringify({
          messages: [{
            role: 'user',
            created_at: 1710000000000,
            parts: [{ type: 'text', text: 'hello from server' }],
          }]
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/sess_real/state') {
        return new Response(JSON.stringify({}), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/1291/messages') {
        return new Response(JSON.stringify({
          messages: [{
            role: 'user',
            created_at: 1710000000000,
            parts: [{ type: 'text', text: 'hello from server' }],
          }]
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/1291/state') {
        return new Response(JSON.stringify({}), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify([]), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
    Response,
    Headers,
    AbortController,
    Notification: undefined,
    renderMathInElement() {},
    crypto: webcrypto,
  };
  context.globalThis = context;

  vm.createContext(context);
  vm.runInContext(coreSource, context, { filename: 'app-core.js' });

  const app = windowObj.TermLLMApp;
  Object.assign(app, {
    persistAndRefreshShell() {},
    refreshRelativeTimes() {},
    openAuthModal() {},
    closeAuthModal() {},
    handleAuthFailure() {},
    closeAskUserModal() {},
    openAskUserModal() {},
    setActiveResponseTracking() {},
    clearActiveResponseTracking() {},
    setStreaming() {},
    resumeActiveResponse: async () => {},
    renderSidebar() {},
    renderMessages() {},
    renderProviderOptions() {},
    renderModelOptions() {},
    normalizeSelectedProvider() {},
    autoGrowPrompt() {},
    updateVoiceUI() {},
    toggleVoiceRecording() {},
    fetchProviders: async () => [],
    fetchModels: async () => [],
    addErrorMessage() {},
    sendMessage() {},
    openSidebar() {},
    closeSidebar() {},
    closeSidebarIfMobile() {},
    connectToken: async () => {},
    submitAskUserModal: async () => {},
    cancelActiveResponse: async () => {},
    handleFiles() {},
    openApprovalModal() {},
    closeApprovalModal() {},
    submitApprovalModal: async () => {},
    registerServiceWorker: async () => null,
    subscribeToPush() {},
    refreshNotificationUI() {},
    requestNotificationPermission: async () => 'default',
    shouldAutoSubscribeToPush() { return false; },
    detachResponseStream() {},
    HEARTBEAT_STALE_THRESHOLD: 45000,
    HEARTBEAT_ABORT_REASON: 'heartbeat stale',
    applyDesktopSidebarState() {},
    toggleSidebarCollapsed() {},
    flushStreamPersistence() {},
    requestHeaders() { return {}; },
    normalizeError: async (resp) => ({ status: resp.status, message: `HTTP ${resp.status}` }),
    renderAttachments() {},
    updateSidebarStatus() {},
  });

  vm.runInContext(sessionsSource, context, { filename: 'app-sessions.js' });
  await windowObj.__termllmInitializePromise;

  if (app.state.activeSessionId !== 'sess_real') {
    fail(name, 'active session id should be reconciled to real server id', `got ${app.state.activeSessionId}`);
    return;
  }
  if (!fetchCalls.includes('/ui/v1/sessions/sess_real/messages')) {
    fail(name, 'should load messages via resolved server session id', JSON.stringify(fetchCalls));
    return;
  }
  if (fetchCalls.includes('/ui/v1/sessions/pending_1291/messages')) {
    fail(name, 'should not use pending_ prefix in session id', JSON.stringify(fetchCalls));
    return;
  }
  pass(name);
}

async function testDeveloperMessagesAreHidden() {
  const name = 'developer role messages are excluded from converted messages';
  const storage = new Map();

  const document = {
    cookie: '',
    visibilityState: 'visible',
    body: { classList: makeClassList() },
    documentElement: { style: { setProperty() {} } },
    getElementById() { return makeNode(); },
    createElement() { return makeNode(); },
    querySelector() { return makeNode(); },
    querySelectorAll() { return []; },
    addEventListener() {},
  };

  const localStorage = {
    getItem(key) { return storage.has(key) ? storage.get(key) : null; },
    setItem(key, value) { storage.set(key, String(value)); },
    removeItem(key) { storage.delete(key); },
  };

  const windowObj = {
    TERM_LLM_UI_PREFIX: '/ui',
    TERM_LLM_SIDEBAR_SESSIONS: 'all',
    location: { pathname: '/ui/42', search: '', origin: 'https://example.test' },
    history: { pushState(_state, _title, url) { windowObj.location.pathname = url; } },
    matchMedia() {
      return {
        matches: false,
        addEventListener() {},
        addListener() {},
        removeEventListener() {},
        removeListener() {},
      };
    },
    navigator: { standalone: false, mediaDevices: null, serviceWorker: null },
    visualViewport: null,
    addEventListener() {},
    removeEventListener() {},
    requestAnimationFrame(callback) { return setTimeout(callback, 0); },
    cancelAnimationFrame(handle) { clearTimeout(handle); },
    setTimeout,
    clearTimeout,
    setInterval() { return 0; },
    clearInterval() {},
    focus() {},
  };
  windowObj.document = document;
  windowObj.localStorage = localStorage;

  const context = {
    window: windowObj,
    document,
    localStorage,
    history: windowObj.history,
    location: windowObj.location,
    navigator: windowObj.navigator,
    console,
    setTimeout,
    clearTimeout,
    setInterval() { return 0; },
    clearInterval() {},
    URL,
    URLSearchParams,
    fetch: async (url) => {
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({
          sessions: [{
            id: 'sess_dev',
            number: 42,
            short_title: 'Dev msg test',
            long_title: 'Dev msg test',
            mode: 'chat',
            origin: 'web',
            archived: false,
            pinned: false,
            created_at: 1710000000000,
            message_count: 2,
          }]
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/sess_dev/messages') {
        return new Response(JSON.stringify({
          messages: [
            {
              role: 'developer',
              created_at: 1710000000000,
              parts: [{ type: 'text', text: 'You are Jarvis, a personal assistant.' }],
            },
            {
              role: 'user',
              created_at: 1710000001000,
              parts: [{ type: 'text', text: 'hello' }],
            },
            {
              role: 'assistant',
              created_at: 1710000002000,
              parts: [{ type: 'text', text: 'hi there' }],
            },
          ]
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/sess_dev/state') {
        return new Response(JSON.stringify({}), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify([]), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
    Response,
    Headers,
    AbortController,
    Notification: undefined,
    renderMathInElement() {},
    crypto: webcrypto,
  };
  context.globalThis = context;

  vm.createContext(context);
  vm.runInContext(coreSource, context, { filename: 'app-core.js' });

  const app = windowObj.TermLLMApp;
  Object.assign(app, {
    persistAndRefreshShell() {},
    refreshRelativeTimes() {},
    openAuthModal() {},
    closeAuthModal() {},
    handleAuthFailure() {},
    closeAskUserModal() {},
    openAskUserModal() {},
    setActiveResponseTracking() {},
    clearActiveResponseTracking() {},
    setStreaming() {},
    resumeActiveResponse: async () => {},
    renderSidebar() {},
    renderMessages() {},
    renderProviderOptions() {},
    renderModelOptions() {},
    normalizeSelectedProvider() {},
    autoGrowPrompt() {},
    updateVoiceUI() {},
    toggleVoiceRecording() {},
    fetchProviders: async () => [],
    fetchModels: async () => [],
    addErrorMessage() {},
    sendMessage() {},
    openSidebar() {},
    closeSidebar() {},
    closeSidebarIfMobile() {},
    connectToken: async () => {},
    submitAskUserModal: async () => {},
    cancelActiveResponse: async () => {},
    handleFiles() {},
    openApprovalModal() {},
    closeApprovalModal() {},
    submitApprovalModal: async () => {},
    registerServiceWorker: async () => null,
    subscribeToPush() {},
    refreshNotificationUI() {},
    requestNotificationPermission: async () => 'default',
    shouldAutoSubscribeToPush() { return false; },
    detachResponseStream() {},
    HEARTBEAT_STALE_THRESHOLD: 45000,
    HEARTBEAT_ABORT_REASON: 'heartbeat stale',
    applyDesktopSidebarState() {},
    toggleSidebarCollapsed() {},
    flushStreamPersistence() {},
    requestHeaders() { return {}; },
    normalizeError: async (resp) => ({ status: resp.status, message: `HTTP ${resp.status}` }),
    renderAttachments() {},
    updateSidebarStatus() {},
  });

  vm.runInContext(sessionsSource, context, { filename: 'app-sessions.js' });
  await windowObj.__termllmInitializePromise;

  const session = app.state.sessions.find(s => s.id === 'sess_dev');
  if (!session) {
    fail(name, 'session not found');
    return;
  }

  const developerMsg = session.messages.find(m => m.role === 'developer');
  if (developerMsg) {
    fail(name, 'developer message should not appear in converted messages');
    return;
  }

  const devContent = session.messages.find(m => m.content && m.content.includes('You are Jarvis'));
  if (devContent) {
    fail(name, 'developer message content leaked into another role', `role=${devContent.role}`);
    return;
  }

  if (session.messages.length !== 2) {
    fail(name, `expected 2 messages (user + assistant), got ${session.messages.length}`);
    return;
  }

  pass(name);
}

(async () => {
  await testNumericDeepLinkResolvesRealSessionId();
  await testDeveloperMessagesAreHidden();
  if (failures > 0) process.exit(1);
  process.exit(0);
})();
