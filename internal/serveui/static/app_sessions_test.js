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

function defaultAppStubs(app, overrides = {}) {
  return {
    persistAndRefreshShell() {},
    refreshRelativeTimes() {},
    openAuthModal() {},
    closeAuthModal() {},
    handleAuthFailure() {},
    closeAskUserModal() {},
    openAskUserModal() {},
    setActiveResponseTracking(session, responseId, sequenceNumber = null) {
      if (!session) return;
      const normalized = String(responseId || '').trim();
      if (!normalized) return;
      if (session.activeResponseId !== normalized) {
        session.activeResponseId = normalized;
        if (sequenceNumber === null) {
          session.lastSequenceNumber = 0;
        }
      }
      if (sequenceNumber !== null) {
        const nextSeq = Number(sequenceNumber);
        if (Number.isFinite(nextSeq) && nextSeq >= 0) {
          session.lastSequenceNumber = nextSeq;
        }
      }
    },
    clearActiveResponseTracking(session, responseId = '') {
      if (!session) return;
      const currentId = String(session.activeResponseId || '').trim();
      const targetId = String(responseId || '').trim();
      if (!targetId || currentId === targetId) {
        session.activeResponseId = null;
        session.lastSequenceNumber = 0;
      }
      if (!targetId || app.state.currentStreamResponseId === targetId) {
        app.state.currentStreamSessionId = '';
        app.state.currentStreamResponseId = '';
      }
    },
    setStreaming(next) {
      app.state.streaming = Boolean(next);
    },
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
    requeueUncommittedInterrupts() {},
    drainInterruptQueueIfIdle() {},
    requeuePendingInterjections() {},
    trackPendingInterjection() {},
    removePendingInterjectionById() {},
    trackPendingInterruptCommit() {},
    refreshPendingInterjectionBanner() {},
    restoreDraftMessageForSession() {},
    stageDraftMessage() {},
    HEARTBEAT_STALE_THRESHOLD: 45000,
    HEARTBEAT_ABORT_REASON: 'heartbeat stale',
    applyDesktopSidebarState() {},
    toggleSidebarCollapsed() {},
    flushStreamPersistence() {},
    requestHeaders() { return {}; },
    normalizeError: async (resp) => ({ status: resp.status, message: `HTTP ${resp.status}` }),
    renderAttachments() {},
    updateSidebarStatus() {},
    updateHeader() {},
    ...overrides,
  };
}

async function createSessionsHarness(options = {}) {
  const storage = new Map(Object.entries(options.initialStorage || {}));

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
    location: { pathname: options.pathname || '/ui/', search: '', origin: 'https://example.test' },
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
    fetch: options.fetchImpl || (async () => new Response(JSON.stringify({ sessions: [] }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' }
    })),
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
  Object.assign(app, defaultAppStubs(app, options.appOverrides || {}));

  vm.runInContext(sessionsSource, context, { filename: 'app-sessions.js' });
  await windowObj.__termllmInitializePromise;

  return { app, storage, windowObj };
}

async function testSwitchingSessionsStagesCurrentComposerBeforeRestore() {
  const name = 'switching sessions stages current composer before restoring target draft';
  const drafts = new Map();
  const { app } = await createSessionsHarness({
    fetchImpl: async () => new Response(JSON.stringify({ sessions: [] }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' }
    }),
    appOverrides: {
      stageDraftMessage(prompt, sessionId) {
        drafts.set(String(sessionId || ''), String(prompt || '').trim());
      },
      restoreDraftMessageForSession(sessionId, options = {}) {
        const key = String(sessionId || '');
        if (!drafts.has(key)) {
          if (options.replace) app.elements.promptInput.value = '';
          return false;
        }
        app.elements.promptInput.value = drafts.get(key);
        return true;
      }
    }
  });

  const sessionA = { id: 'sess_a', title: 'A', messages: [], lastResponseId: null, activeResponseId: null, lastSequenceNumber: 0 };
  const sessionB = { id: 'sess_b', title: 'B', messages: [], lastResponseId: null, activeResponseId: null, lastSequenceNumber: 0 };
  app.state.sessions = [sessionA, sessionB];
  app.state.activeSessionId = sessionA.id;
  app.state.draftSessionActive = false;
  app.elements.promptInput.value = 'unsent in A';

  await app.switchToSession(sessionB.id, { sync: false });
  if (drafts.get(sessionA.id) !== 'unsent in A') {
    fail(name, 'expected session A composer to be staged before switching', JSON.stringify(Array.from(drafts.entries())));
    return;
  }

  await app.switchToSession(sessionA.id, { sync: false });
  if (app.elements.promptInput.value !== 'unsent in A') {
    fail(name, 'expected staged session A composer to be restored when switching back', app.elements.promptInput.value);
    return;
  }
  pass(name);
}

async function testSwitchToSessionSyncsSelectedRuntime() {
  const name = 'switching sessions syncs selected runtime to active session';
  const { app, storage } = await createSessionsHarness({
    initialStorage: {
      term_llm_selected_provider: 'chatgpt',
      term_llm_selected_model: 'gpt-5.4',
      term_llm_selected_effort: 'xhigh',
    },
    fetchImpl: async () => new Response(JSON.stringify({ sessions: [] }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' }
    }),
  });

  const session = {
    id: 'sess_mini',
    title: 'Mini session',
    messages: [{ id: 'u1', role: 'user', content: 'hi', created: 1 }],
    provider: 'chatgpt',
    activeModel: 'gpt-5.4-mini',
    activeEffort: '',
    lastResponseId: 'resp_msg_1',
    activeResponseId: null,
    lastSequenceNumber: 0,
  };
  app.state.sessions = [session];
  app.state.selectedProvider = 'chatgpt';
  app.state.selectedModel = 'gpt-5.4';
  app.state.selectedEffort = 'xhigh';

  await app.switchToSession(session.id, { sync: false });

  if (app.state.selectedProvider !== 'chatgpt') {
    fail(name, `selectedProvider = ${JSON.stringify(app.state.selectedProvider)}, want chatgpt`);
    return;
  }
  if (app.state.selectedModel !== 'gpt-5.4-mini') {
    fail(name, `selectedModel = ${JSON.stringify(app.state.selectedModel)}, want gpt-5.4-mini`);
    return;
  }
  if (app.state.selectedEffort !== '') {
    fail(name, `selectedEffort = ${JSON.stringify(app.state.selectedEffort)}, want empty`);
    return;
  }
  if (storage.get('term_llm_selected_model') !== 'gpt-5.4-mini') {
    fail(name, 'expected selected model to be persisted for the active session', storage.get('term_llm_selected_model'));
    return;
  }
  if (storage.has('term_llm_selected_effort')) {
    fail(name, 'expected stale selected effort to be cleared', storage.get('term_llm_selected_effort'));
    return;
  }
  pass(name);
}

async function testNumericDeepLinkResolvesRealSessionId() {
  const name = 'numeric deep link resolves server session id';
  const fetchCalls = [];
  const { app } = await createSessionsHarness({
    pathname: '/ui/1291',
    fetchImpl: async (url) => {
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
    }
  });

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
  const { app } = await createSessionsHarness({
    pathname: '/ui/42',
    fetchImpl: async (url) => {
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
    }
  });

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

async function testConvertServerMessagesAttachesToolResultImages() {
  const name = 'server tool_result image parts attach to tool group artifacts';
  const { app } = await createSessionsHarness();

  const converted = app.convertServerMessages([
    {
      role: 'user',
      created_at: 1000,
      parts: [{ type: 'text', text: 'make an image' }],
    },
    {
      role: 'assistant',
      created_at: 2000,
      parts: [{
        type: 'tool_call',
        tool_name: 'image_generate',
        tool_call_id: 'call_img',
        tool_arguments: '{"prompt":"cat"}',
      }],
    },
    {
      role: 'tool',
      created_at: 3000,
      parts: [{
        type: 'tool_result',
        tool_name: 'image_generate',
        tool_call_id: 'call_img',
        images: ['/ui/images/generated.png'],
      }],
    },
    {
      role: 'assistant',
      created_at: 4000,
      parts: [{ type: 'text', text: 'Done.' }],
    },
  ]);

  const group = converted.find((message) => message.role === 'tool-group');
  const tool = group && group.tools && group.tools[0];
  if (!tool || !Array.isArray(tool.images) || tool.images[0] !== '/ui/images/generated.png') {
    fail(name, 'converted tool group missing image artifact', JSON.stringify(converted));
    return;
  }
  const assistantMarkdown = converted.find((message) => (
    message.role === 'assistant' && String(message.content || '').includes('Generated Image')
  ));
  if (assistantMarkdown) {
    fail(name, 'tool result image should not become assistant markdown', JSON.stringify(assistantMarkdown));
    return;
  }

  pass(name);
}

async function testSessionHistoryPaginationLoadsAdditionalPages() {
  const name = 'session history pagination loads additional pages';
  const fetchCalls = [];
  const { app } = await createSessionsHarness({
    pathname: '/ui/77',
    fetchImpl: async (url) => {
      fetchCalls.push(url);
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({
          sessions: [{
            id: 'sess_page',
            number: 77,
            short_title: 'Paged session',
            long_title: 'Paged session',
            mode: 'chat',
            origin: 'web',
            archived: false,
            pinned: false,
            created_at: 1710000000000,
            message_count: 201,
          }]
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/sess_page/messages') {
        return new Response(JSON.stringify({
          messages: [{
            role: 'user',
            created_at: 1710000000000,
            parts: [{ type: 'text', text: 'first page' }],
          }],
          has_more: true,
          next_offset: 200,
        }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"sess-page-v1"' } });
      }
      if (url === '/ui/v1/sessions/sess_page/messages?limit=200&offset=200') {
        return new Response(JSON.stringify({
          messages: [{
            role: 'assistant',
            created_at: 1710000001000,
            parts: [{ type: 'text', text: 'second page' }],
          }],
          has_more: false,
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/sess_page/state') {
        return new Response(JSON.stringify({}), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify([]), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });

  if (!fetchCalls.includes('/ui/v1/sessions/sess_page/messages?limit=200&offset=200')) {
    fail(name, 'expected follow-up paginated fetch', JSON.stringify(fetchCalls));
    return;
  }

  const session = app.state.sessions.find((item) => item.id === 'sess_page');
  if (!session) {
    fail(name, 'session not found after paginated load');
    return;
  }
  if (session.messages.length !== 2) {
    fail(name, `expected 2 merged messages, got ${session.messages.length}`);
    return;
  }
  if (session.messages[0].content !== 'first page' || session.messages[1].content !== 'second page') {
    fail(name, 'expected both pages to merge in order', JSON.stringify(session.messages));
    return;
  }

  pass(name);
}

async function testSessionHistoryPaginationFailureClearsEtagForRetry() {
  const name = 'session history pagination failure clears etag for retry';
  const fetchCalls = [];
  let firstPageCalls = 0;
  let secondPageCalls = 0;
  const { app } = await createSessionsHarness({
    fetchImpl: async (url, opts = {}) => {
      fetchCalls.push({ url, ifNoneMatch: opts.headers && opts.headers['If-None-Match'] });
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/sess_retry/messages') {
        firstPageCalls += 1;
        return new Response(JSON.stringify({
          messages: [{ role: 'user', created_at: 1710000000000, parts: [{ type: 'text', text: `page one ${firstPageCalls}` }] }],
          has_more: true,
          next_offset: 200,
        }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"retry-v1"' } });
      }
      if (url === '/ui/v1/sessions/sess_retry/messages?limit=200&offset=200') {
        secondPageCalls += 1;
        if (secondPageCalls === 1) {
          return new Response('temporary failure', { status: 500 });
        }
        return new Response(JSON.stringify({
          messages: [{ role: 'assistant', created_at: 1710000001000, parts: [{ type: 'text', text: 'page two' }] }],
          has_more: false,
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({}), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });

  const failed = await app.loadServerSessionMessages('sess_retry');
  if (failed !== null) {
    fail(name, 'expected first paginated load to fail');
    return;
  }
  const loaded = await app.loadServerSessionMessages('sess_retry');
  if (!Array.isArray(loaded) || loaded.length !== 2 || loaded[1].content !== 'page two') {
    fail(name, 'expected retry to reload first page and fetch missing second page', JSON.stringify(loaded));
    return;
  }
  const conditionalFirstPageFetches = fetchCalls.filter((call) => call.url === '/ui/v1/sessions/sess_retry/messages' && call.ifNoneMatch);
  if (conditionalFirstPageFetches.length !== 0) {
    fail(name, 'expected no conditional first-page request after partial pagination failure', JSON.stringify(fetchCalls));
    return;
  }

  pass(name);
}

async function testSwitchToSessionSyncsWithoutTokenAndResumes() {
  const name = 'sidebar session switch syncs with server and resumes without token';
  const fetchCalls = [];
  const resumeCalls = [];
  const initialSessions = [
    {
      id: 'sess_other',
      title: 'Other session',
      origin: 'web',
      created: 1710000000000,
      messages: [],
    },
    {
      id: 'sess_resume',
      title: 'Resume me',
      origin: 'tui',
      created: 1710000001000,
      messages: [],
      activeResponseId: null,
      lastSequenceNumber: 0,
    },
  ];

  const { app } = await createSessionsHarness({
    initialStorage: {
      term_llm_active_session: 'sess_other',
    },
    fetchImpl: async (url) => {
      fetchCalls.push(url);
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({
          sessions: [
            {
              id: 'sess_other',
              short_title: 'Other session',
              long_title: 'Other session',
              mode: 'chat',
              origin: 'web',
              archived: false,
              pinned: false,
              created_at: 1710000000000,
              message_count: 0,
            },
            {
              id: 'sess_resume',
              short_title: 'Resume me',
              long_title: 'Resume me',
              mode: 'chat',
              origin: 'tui',
              archived: false,
              pinned: false,
              created_at: 1710000001000,
              message_count: 0,
            },
          ]
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/sess_other/state') {
        return new Response(JSON.stringify({ active_run: false }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/sess_other/messages') {
        return new Response(JSON.stringify({ messages: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/sess_resume/state') {
        return new Response(JSON.stringify({
          active_run: true,
          active_response_id: 'resp_resume_123',
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/status') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ messages: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
    appOverrides: {
      async resumeActiveResponse(session, options = {}) {
        resumeCalls.push({
          sessionId: session.id,
          responseId: options.responseId || '',
        });
        return true;
      },
    }
  });

  await app.switchToSession('sess_resume');

  if (!fetchCalls.includes('/ui/v1/sessions/sess_resume/state')) {
    fail(name, 'expected session switch to fetch runtime state without a token', JSON.stringify(fetchCalls));
    return;
  }

  if (resumeCalls.length !== 1 || resumeCalls[0].responseId !== 'resp_resume_123') {
    fail(name, 'expected session switch to resume the active response', JSON.stringify(resumeCalls));
    return;
  }

  const session = app.state.sessions.find((item) => item.id === 'sess_resume');
  if (!session || session.activeResponseId !== 'resp_resume_123') {
    fail(name, 'expected server active response id to replace local idle state', session ? `got ${session.activeResponseId}` : 'session missing');
    return;
  }

  pass(name);
}

async function testSwitchToSessionRecoversChangedActiveResponseFromSnapshot() {
  const name = 'sidebar session switch requests snapshot recovery when runtime reports a different active response';
  const resumeCalls = [];
  const initialSessions = [
    {
      id: 'sess_other',
      title: 'Other session',
      origin: 'web',
      created: 1710000000000,
      messages: [],
    },
    {
      id: 'sess_resume',
      title: 'Resume me',
      origin: 'tui',
      created: 1710000001000,
      messages: [
        { id: 'msg_user', role: 'user', content: 'check status', created: 1710000001000 },
        {
          id: 'msg_tool_group',
          role: 'tool-group',
          created: 1710000002000,
          status: 'done',
          expanded: false,
          tools: [
            { id: 'call_1', name: 'read_file', arguments: '{"path":"README.md"}', status: 'done', created: 1710000002000 },
          ],
        },
      ],
      activeResponseId: null,
      lastSequenceNumber: 0,
    },
  ];

  const { app } = await createSessionsHarness({
    initialStorage: {
      term_llm_active_session: 'sess_other',
    },
    fetchImpl: async (url) => {
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({
          sessions: [
            {
              id: 'sess_other',
              short_title: 'Other session',
              long_title: 'Other session',
              mode: 'chat',
              origin: 'web',
              archived: false,
              pinned: false,
              created_at: 1710000000000,
              message_count: 0,
            },
            {
              id: 'sess_resume',
              short_title: 'Resume me',
              long_title: 'Resume me',
              mode: 'chat',
              origin: 'tui',
              archived: false,
              pinned: false,
              created_at: 1710000001000,
              message_count: 2,
            },
          ]
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/sess_other/state') {
        return new Response(JSON.stringify({ active_run: false }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/sess_other/messages') {
        return new Response(JSON.stringify({ messages: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/sess_resume/state') {
        return new Response(JSON.stringify({
          active_run: true,
          active_response_id: 'resp_resume_123',
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/status') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ messages: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
    appOverrides: {
      async resumeActiveResponse(session, options = {}) {
        resumeCalls.push({
          sessionId: session.id,
          responseId: options.responseId || '',
          recoverFromSnapshot: Boolean(options.recoverFromSnapshot),
        });
        return true;
      },
    }
  });

  app.state.sessions = initialSessions.map((session) => ({ ...session }));
  app.state.activeSessionId = 'sess_other';
  app.state.draftSessionActive = false;
  resumeCalls.length = 0;

  await app.switchToSession('sess_resume');

  if (resumeCalls.length !== 1) {
    fail(name, 'expected session switch to trigger exactly one resume', JSON.stringify(resumeCalls));
    return;
  }
  if (!resumeCalls[0].recoverFromSnapshot) {
    fail(name, 'expected resumeActiveResponse to request snapshot recovery', JSON.stringify(resumeCalls[0]));
    return;
  }

  pass(name);
}

async function testSwitchToLazyLoadedSessionFetchesMessagesOnce() {
  const name = 'sidebar session switch fetches lazy-loaded session messages once';
  const fetchCalls = [];
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      fetchCalls.push(url);
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      if (url === '/ui/v1/sessions/sess_lazy/state') {
        return new Response(JSON.stringify({ active_run: false }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      if (url === '/ui/v1/sessions/sess_lazy/messages') {
        return new Response(JSON.stringify({
          messages: [
            {
              role: 'user',
              created_at: 1710000002000,
              parts: [{ type: 'text', text: 'lazy hello' }],
            },
            {
              role: 'assistant',
              created_at: 1710000003000,
              parts: [{ type: 'text', text: 'loaded once' }],
            },
          ]
        }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      if (url === '/ui/v1/sessions/status') {
        return new Response(JSON.stringify({ sessions: [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      return new Response(JSON.stringify({ messages: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' }
      });
    },
  });

  app.state.sessions = [
    {
      id: 'sess_other',
      title: 'Other session',
      origin: 'web',
      created: 1710000000000,
      messages: [],
    },
    {
      id: 'sess_lazy',
      title: 'Lazy session',
      origin: 'tui',
      created: 1710000001000,
      messages: [],
      _serverOnly: true,
    },
  ];
  app.state.activeSessionId = 'sess_other';
  app.state.draftSessionActive = false;
  fetchCalls.length = 0;

  await app.switchToSession('sess_lazy');

  const messageFetches = fetchCalls.filter((url) => url === '/ui/v1/sessions/sess_lazy/messages');
  if (messageFetches.length !== 1) {
    fail(name, 'expected exactly one lazy session messages fetch during switch', JSON.stringify(fetchCalls));
    return;
  }
  if (!fetchCalls.includes('/ui/v1/sessions/sess_lazy/state')) {
    fail(name, 'expected lazy session switch to still sync runtime state', JSON.stringify(fetchCalls));
    return;
  }

  const session = app.state.sessions.find((item) => item.id === 'sess_lazy');
  if (!session) {
    fail(name, 'lazy session missing after switch');
    return;
  }
  if (session._serverOnly) {
    fail(name, 'expected lazy session to be hydrated after message preload');
    return;
  }
  if (session.messages.length !== 2 || session.messages[1].content !== 'loaded once') {
    fail(name, 'expected preloaded lazy session messages to be preserved', JSON.stringify(session.messages));
    return;
  }

  pass(name);
}

async function testSwitchToSessionClearsStaleActiveResponseWithoutToken() {
  const name = 'sidebar session switch clears stale active response without token';
  const fetchCalls = [];
  const initialSessions = [
    {
      id: 'sess_other',
      title: 'Other session',
      origin: 'web',
      created: 1710000000000,
      messages: [],
    },
    {
      id: 'sess_stale',
      title: 'Stale session',
      origin: 'tui',
      created: 1710000001000,
      messages: [],
      activeResponseId: 'resp_stale_123',
      lastSequenceNumber: 17,
    },
  ];

  const { app } = await createSessionsHarness({
    initialStorage: {
      term_llm_active_session: 'sess_other',
    },
    fetchImpl: async (url) => {
      fetchCalls.push(url);
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({
          sessions: [
            {
              id: 'sess_other',
              short_title: 'Other session',
              long_title: 'Other session',
              mode: 'chat',
              origin: 'web',
              archived: false,
              pinned: false,
              created_at: 1710000000000,
              message_count: 0,
            },
            {
              id: 'sess_stale',
              short_title: 'Stale session',
              long_title: 'Stale session',
              mode: 'chat',
              origin: 'tui',
              archived: false,
              pinned: false,
              created_at: 1710000001000,
              message_count: 2,
            },
          ]
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/sess_other/state') {
        return new Response(JSON.stringify({ active_run: false }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/sess_other/messages') {
        return new Response(JSON.stringify({ messages: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/sess_stale/state') {
        return new Response(JSON.stringify({ active_run: false }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/sess_stale/messages') {
        return new Response(JSON.stringify({
          messages: [
            {
              role: 'user',
              created_at: 1710000002000,
              parts: [{ type: 'text', text: 'hello from another tab' }],
            },
            {
              role: 'assistant',
              created_at: 1710000003000,
              parts: [{ type: 'text', text: 'synced reply' }],
            },
          ]
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/status') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ messages: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });

  await app.switchToSession('sess_stale');

  if (!fetchCalls.includes('/ui/v1/sessions/sess_stale/state')) {
    fail(name, 'expected session switch to fetch runtime state without a token', JSON.stringify(fetchCalls));
    return;
  }

  if (!fetchCalls.includes('/ui/v1/sessions/sess_stale/messages')) {
    fail(name, 'expected completed session sync to reload messages from server', JSON.stringify(fetchCalls));
    return;
  }

  const session = app.state.sessions.find((item) => item.id === 'sess_stale');
  if (!session) {
    fail(name, 'session missing after switch');
    return;
  }
  if (session.activeResponseId !== null || session.lastSequenceNumber !== 0) {
    fail(name, 'expected stale local active response tracking to be cleared', `activeResponseId=${session.activeResponseId}, lastSequenceNumber=${session.lastSequenceNumber}`);
    return;
  }
  if (session.messages.length !== 2 || session.messages[1].content !== 'synced reply') {
    fail(name, 'expected local messages to refresh from server after stale run cleared', JSON.stringify(session.messages));
    return;
  }

  pass(name);
}

async function testSessionState404ClearsStaleActiveResponse() {
  const name = 'session state 404 is treated as inactive and clears stale active response';
  const fetchCalls = [];
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      fetchCalls.push(url);
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      if (url === '/ui/v1/sessions/sess_state_404/state') {
        return new Response(JSON.stringify({ error: { message: 'runtime not found' } }), {
          status: 404,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      if (url === '/ui/v1/sessions/sess_state_404/messages') {
        return new Response(JSON.stringify({ messages: [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      if (url === '/ui/v1/sessions/status') {
        return new Response(JSON.stringify({ sessions: [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      return new Response(JSON.stringify([]), {
        status: 200,
        headers: { 'Content-Type': 'application/json' }
      });
    }
  });

  const session = {
    id: 'sess_state_404',
    title: 'State 404',
    origin: 'web',
    created: 1710000000000,
    messages: [],
    activeResponseId: 'resp_stale_state_404',
    lastSequenceNumber: 42,
  };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.currentStreamSessionId = session.id;
  app.state.currentStreamResponseId = session.activeResponseId;
  app.state.streaming = true;

  const runtimeState = await app.syncActiveSessionFromServer(session, false);

  if (!runtimeState || runtimeState.active_run !== false) {
    fail(name, 'expected state 404 to produce inactive runtime state', JSON.stringify(runtimeState));
    return;
  }
  if (!fetchCalls.includes('/ui/v1/sessions/sess_state_404/messages')) {
    fail(name, 'expected inactive 404 sync to continue through message refresh', JSON.stringify(fetchCalls));
    return;
  }
  if (session.activeResponseId !== null || session.lastSequenceNumber !== 0) {
    fail(name, 'expected stale active response tracking to be cleared', `activeResponseId=${session.activeResponseId}, lastSequenceNumber=${session.lastSequenceNumber}`);
    return;
  }
  if (app.state.streaming || app.state.currentStreamResponseId) {
    fail(name, 'expected global streaming state to be cleared', JSON.stringify({ streaming: app.state.streaming, currentStreamResponseId: app.state.currentStreamResponseId }));
    return;
  }

  pass(name);
}

async function testIdleSessionSyncRescuesPendingInterruptCommit() {
  const name = 'idle session sync rescues pending interrupt commits';
  const sendCalls = [];
  const requeueCalls = [];
  let appRef = null;

  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      if (url === '/ui/v1/sessions/sess_interrupt/state') {
        return new Response(JSON.stringify({ active_run: false }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      if (url === '/ui/v1/sessions/sess_interrupt/messages') {
        return new Response(JSON.stringify({ messages: [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      if (url === '/ui/v1/sessions/status') {
        return new Response(JSON.stringify({ sessions: [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      return new Response(JSON.stringify([]), {
        status: 200,
        headers: { 'Content-Type': 'application/json' }
      });
    },
    appOverrides: {
      requeueUncommittedInterrupts(session) {
        requeueCalls.push(session.id);
        const [entry] = appRef.state.pendingInterruptCommits;
        if (!entry) return;
        appRef.state.pendingInterruptCommits = [];
        appRef.state.queuedInterrupts.push({
          prompt: entry.prompt,
          messageId: entry.messageId
        });
      },
      sendMessage(payload) {
        sendCalls.push(payload);
        return Promise.resolve();
      },
      drainInterruptQueueIfIdle(session) {
        if (!session || session.id !== appRef.state.activeSessionId) return;
        if (appRef.state.streaming || appRef.state.abortController) return;
        appRef.requeueUncommittedInterrupts(session);
        if (appRef.state.queuedInterrupts.length > 0) {
          const queued = appRef.state.queuedInterrupts.shift();
          appRef.elements.promptInput.value = queued.prompt;
          void appRef.sendMessage({ prompt: queued.prompt, attachments: [], reuseMessageId: queued.messageId });
        }
      },
    }
  });
  appRef = app;

  const session = {
    id: 'sess_interrupt',
    title: 'Interrupt me',
    origin: 'web',
    created: 1710000000000,
    messages: [],
    activeResponseId: 'resp_interrupt_123',
    lastSequenceNumber: 9,
  };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;
  app.state.pendingInterruptCommits = [{
    sessionId: session.id,
    prompt: 'follow up after reconnect',
    messageId: 'msg_interrupt_123'
  }];

  await app.syncActiveSessionFromServer(session, false);

  if (requeueCalls.length !== 1 || requeueCalls[0] !== session.id) {
    fail(name, 'expected idle sync to requeue pending interrupt commits', JSON.stringify(requeueCalls));
    return;
  }

  if (sendCalls.length !== 1) {
    fail(name, 'expected rescued interrupt to be sent immediately', JSON.stringify(sendCalls));
    return;
  }

  if (sendCalls[0].prompt !== 'follow up after reconnect' || sendCalls[0].reuseMessageId !== 'msg_interrupt_123') {
    fail(name, 'expected rescued interrupt payload to preserve prompt and message id', JSON.stringify(sendCalls[0]));
    return;
  }

  if (app.state.pendingInterruptCommits.length !== 0 || app.state.queuedInterrupts.length !== 0) {
    fail(name, 'expected rescued interrupt queues to be drained', JSON.stringify({
      pendingInterruptCommits: app.state.pendingInterruptCommits,
      queuedInterrupts: app.state.queuedInterrupts,
    }));
    return;
  }

  pass(name);
}

async function testSessionProgressStatePrefersLocalAndServerSignals() {
  const name = 'session progress state combines optimistic local state with server truth';
  const { app } = await createSessionsHarness();

  const session = {
    id: 'sess_progress',
    title: 'Busy session',
    origin: 'web',
    created: 1710000000000,
    messages: [],
    activeResponseId: null,
  };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;

  if (app.sessionHasInProgressState(session)) {
    fail(name, 'fresh session should start idle');
    return;
  }

  app.setSessionOptimisticBusy(session, true);
  if (!app.sessionHasInProgressState(session)) {
    fail(name, 'optimistic busy state should mark the session in progress immediately');
    return;
  }

  app.setSessionOptimisticBusy(session, false);
  if (app.sessionHasInProgressState(session)) {
    fail(name, 'clearing optimistic busy should restore idle state when no other signal exists');
    return;
  }

  app.setSessionServerActiveRun(session, true);
  if (!app.sessionHasInProgressState(session)) {
    fail(name, 'server active_run should mark the session in progress');
    return;
  }

  app.setSessionOptimisticBusy(session, true);
  app.setSessionServerActiveRun(session, false);
  if (!app.sessionHasInProgressState(session)) {
    fail(name, 'local busy state should keep the session in progress until cleared');
    return;
  }

  app.setSessionOptimisticBusy(session, false);
  if (app.sessionHasInProgressState(session)) {
    fail(name, 'session should be idle after both local and server busy signals clear');
    return;
  }

  pass(name);
}

async function testResumeAndDrainFiringViaSync() {
  const name = 'syncActiveSessionFromServer drains queued interrupts after resume completes';
  const sendCalls = [];
  let appRef = null;

  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      if (url === '/ui/v1/sessions/sess_drain/state') {
        return new Response(JSON.stringify({
          active_run: true,
          active_response_id: 'resp_drain_456',
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/status') {
        return new Response(JSON.stringify({ sessions: [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      return new Response(JSON.stringify([]), {
        status: 200,
        headers: { 'Content-Type': 'application/json' }
      });
    },
    appOverrides: {
      async resumeActiveResponse() {
        // Simulate the response completing: clear streaming + activeResponseId.
        appRef.state.streaming = false;
        appRef.state.abortController = null;
        const session = appRef.state.sessions.find(s => s.id === appRef.state.activeSessionId);
        if (session) session.activeResponseId = null;
        return true;
      },
      drainInterruptQueueIfIdle(session) {
        if (!session || session.id !== appRef.state.activeSessionId) return;
        if (appRef.state.streaming || appRef.state.abortController) return;
        appRef.requeueUncommittedInterrupts(session);
        if (appRef.state.queuedInterrupts.length > 0) {
          const queued = appRef.state.queuedInterrupts.shift();
          appRef.elements.promptInput.value = queued.prompt;
          void appRef.sendMessage({ prompt: queued.prompt, attachments: [], reuseMessageId: queued.messageId });
        }
      },
      sendMessage(payload) {
        sendCalls.push(payload);
        return Promise.resolve();
      },
    }
  });
  appRef = app;

  const session = {
    id: 'sess_drain',
    title: 'Drain test',
    origin: 'web',
    created: 1710000000000,
    messages: [],
    activeResponseId: null,
    lastSequenceNumber: 0,
  };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;

  // Queue an interrupt before the sync triggers resume.
  app.state.queuedInterrupts = [{ prompt: 'queued after resume', messageId: 'msg_drain_1' }];

  // syncActiveSessionFromServer will see active_response_id → call resumeAndDrain.
  // resumeAndDrain fires void resumeActiveResponse(...).finally(drainInterruptQueueIfIdle).
  // syncActiveSessionFromServer returns before .finally() fires, so we must yield.
  await app.syncActiveSessionFromServer(session, false);

  // Yield to let the .finally() microtask execute.
  await new Promise(resolve => setTimeout(resolve, 0));

  if (sendCalls.length !== 1) {
    fail(name, `expected 1 sendMessage call from drained interrupt, got ${sendCalls.length}`, JSON.stringify(sendCalls));
    return;
  }

  if (sendCalls[0].prompt !== 'queued after resume' || sendCalls[0].reuseMessageId !== 'msg_drain_1') {
    fail(name, 'drained interrupt payload mismatch', JSON.stringify(sendCalls[0]));
    return;
  }

  if (app.state.queuedInterrupts.length !== 0) {
    fail(name, 'expected queuedInterrupts to be empty after drain', JSON.stringify(app.state.queuedInterrupts));
    return;
  }

  pass(name);
}

async function testTerminalSyncRequeuesPendingInterjectionAsFollowUp() {
  const name = 'idle sync with pending_interjection requeues it as follow-up via requeuePendingInterjections';
  const requeueCalls = [];
  const sendCalls = [];
  let appRef = null;

  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      if (url === '/ui/v1/sessions/sess_reload/state') {
        return new Response(JSON.stringify({
          active_run: false,
          pending_interjection: { text: 'rescued prompt' }
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/sess_reload/messages') {
        return new Response(JSON.stringify({ messages: [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      if (url === '/ui/v1/sessions/status') {
        return new Response(JSON.stringify({ sessions: [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      return new Response(JSON.stringify([]), {
        status: 200,
        headers: { 'Content-Type': 'application/json' }
      });
    },
    appOverrides: {
      trackPendingInterjection(sessionId, prompt, messageId, action) {
        appRef.state.pendingInterjections.push({ sessionId, prompt, messageId, action });
      },
      removePendingInterjectionById(messageId) {
        const idx = appRef.state.pendingInterjections.findIndex(e => e.messageId === messageId);
        if (idx >= 0) appRef.state.pendingInterjections.splice(idx, 1);
      },
      trackPendingInterruptCommit(sessionId, prompt, messageId) {
        appRef.state.pendingInterruptCommits.push({ sessionId, prompt, messageId });
      },
      refreshPendingInterjectionBanner() {},
      requeuePendingInterjections(session) {
        requeueCalls.push(session.id);
        const remaining = [];
        for (const entry of appRef.state.pendingInterjections) {
          if (entry.sessionId === session.id) {
            appRef.state.queuedInterrupts.push({ prompt: entry.prompt, messageId: entry.messageId });
          } else {
            remaining.push(entry);
          }
        }
        appRef.state.pendingInterjections = remaining;
      },
      requeueUncommittedInterrupts() {},
      drainInterruptQueueIfIdle(session) {
        if (!session || session.id !== appRef.state.activeSessionId) return;
        if (appRef.state.streaming || appRef.state.abortController) return;
        if (appRef.state.queuedInterrupts.length > 0) {
          const queued = appRef.state.queuedInterrupts.shift();
          void appRef.sendMessage({ prompt: queued.prompt, attachments: [], reuseMessageId: queued.messageId });
        }
      },
      sendMessage(payload) {
        sendCalls.push(payload);
        return Promise.resolve();
      },
    }
  });
  appRef = app;

  const session = {
    id: 'sess_reload',
    title: 'Reload test',
    origin: 'web',
    created: 1710000000000,
    messages: [],
    activeResponseId: null,
    lastSequenceNumber: 0,
  };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;

  await app.syncActiveSessionFromServer(session, false);

  // The /state response says pending_interjection but active_run=false.
  // syncActiveSessionFromServer should:
  //   1. trackPendingInterjection (picks up pending_interjection from server)
  //   2. in the terminal branch, requeuePendingInterjections → moves to queuedInterrupts
  //   3. drainInterruptQueueIfIdle → sendMessage fires
  if (requeueCalls.length === 0 || requeueCalls[0] !== session.id) {
    fail(name, 'expected requeuePendingInterjections to be called on terminal state', JSON.stringify(requeueCalls));
    return;
  }

  if (app.state.pendingInterjections.length !== 0) {
    fail(name, 'pendingInterjections should be drained', JSON.stringify(app.state.pendingInterjections));
    return;
  }

  if (sendCalls.length !== 1) {
    fail(name, `expected 1 follow-up sendMessage, got ${sendCalls.length}`, JSON.stringify(sendCalls));
    return;
  }
  if (sendCalls[0].prompt !== 'rescued prompt') {
    fail(name, `follow-up prompt = ${sendCalls[0].prompt}, want "rescued prompt"`);
    return;
  }

  pass(name);
}

async function testApplyServerSessionSummaryMapsLastMessageAt() {
  const name = 'applyServerSessionSummary maps last_message_at into lastMessageAt';
  const { app } = await createSessionsHarness();

  const target = {
    id: 'sess1',
    title: 'existing',
    created: 1000,
    messages: [],
  };
  app.applyServerSessionSummary(target, {
    id: 'sess1',
    short_title: 'Updated',
    created_at: 1000,
    last_message_at: 5000,
    message_count: 3,
  });

  if (target.lastMessageAt !== 5000) {
    fail(name, `expected lastMessageAt=5000, got ${target.lastMessageAt}`);
    return;
  }

  const noBumpTarget = { id: 'sess2', created: 2000, lastMessageAt: 4000, messages: [] };
  app.applyServerSessionSummary(noBumpTarget, { id: 'sess2', created_at: 2000 });
  if (noBumpTarget.lastMessageAt !== 4000) {
    fail(name, `existing lastMessageAt should be preserved when server omits field, got ${noBumpTarget.lastMessageAt}`);
    return;
  }

  const freshTarget = { id: 'sess3', created: 3000, messages: [] };
  app.applyServerSessionSummary(freshTarget, { id: 'sess3', created_at: 3000 });
  if (freshTarget.lastMessageAt !== 3000) {
    fail(name, `lastMessageAt should fall back to created when absent on both sides, got ${freshTarget.lastMessageAt}`);
    return;
  }

  pass(name);
}

async function testMergeServerMessagesBumpsLastMessageAt() {
  const name = 'mergeServerMessagesWithLocalState advances lastMessageAt to newest visible message';
  const { app } = await createSessionsHarness();

  const session = {
    id: 'sess1',
    title: 'Existing',
    created: 1000,
    lastMessageAt: 2000,
    messages: [],
  };
  const serverShaped = [
    { role: 'user', created: 3000 },
    { role: 'assistant', created: 5000 },
    { role: 'tool-group', created: 9000 },
  ];
  app.mergeServerMessagesWithLocalState(session, serverShaped);
  if (session.lastMessageAt !== 5000) {
    fail(name, `expected lastMessageAt to advance to newest visible (5000), got ${session.lastMessageAt}`);
    return;
  }

  const stale = {
    id: 'sess2',
    created: 1000,
    lastMessageAt: 9999,
    messages: [],
  };
  app.mergeServerMessagesWithLocalState(stale, [{ role: 'user', created: 3000 }]);
  if (stale.lastMessageAt !== 9999) {
    fail(name, `existing newer lastMessageAt must not be regressed, got ${stale.lastMessageAt}`);
    return;
  }

  pass(name);
}

async function testSanitizeSessionPreservesLastMessageAt() {
  const name = 'sanitizeSession reads lastMessageAt from stored and server-shaped payloads';
  const { app } = await createSessionsHarness();

  const stored = app.sanitizeSession({
    id: 'sess1',
    title: 'Stored',
    created: 1000,
    lastMessageAt: 7500,
    messages: [],
  });
  if (stored.lastMessageAt !== 7500) {
    fail(name, `expected lastMessageAt=7500 from camelCase, got ${stored.lastMessageAt}`);
    return;
  }

  const serverShaped = app.sanitizeSession({
    id: 'sess2',
    title: 'ServerShape',
    created: 2000,
    last_message_at: 9000,
    messages: [],
  });
  if (serverShaped.lastMessageAt !== 9000) {
    fail(name, `expected lastMessageAt=9000 from snake_case fallback, got ${serverShaped.lastMessageAt}`);
    return;
  }

  const fallback = app.sanitizeSession({
    id: 'sess3',
    title: 'Fallback',
    created: 4000,
    messages: [],
  });
  if (fallback.lastMessageAt !== 4000) {
    fail(name, `expected lastMessageAt to fall back to created=4000, got ${fallback.lastMessageAt}`);
    return;
  }

  pass(name);
}

async function testSwitchToSearchOnlySessionHydratesResult() {
  const name = 'switching to a search-only session adds it to local state before hydration';
  const fetchCalls = [];
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      fetchCalls.push(String(url));
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      if (url === '/ui/v1/sessions/sess_search_only/messages') {
        return new Response(JSON.stringify({
          messages: [
            { id: 'u1', role: 'user', parts: [{ type: 'text', text: 'waterparks' }], created_at: 1710000000000 },
            { id: 'a1', role: 'assistant', parts: [{ type: 'text', text: 'loaded from server' }], created_at: 1710000001000 }
          ]
        }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      return new Response(JSON.stringify({ sessions: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' }
      });
    }
  });

  app.state.sessions = Array.from({ length: 101 }, (_, index) => ({
    id: `sess_existing_${index}`,
    title: `Existing ${index}`,
    created: 1800000000000 + index,
    lastMessageAt: 1800000000000 + index,
    messages: [],
    origin: 'web',
  }));
  app.state.sidebarSearchQuery = 'waterparks';
  app.state.sidebarSearchResults = [{
    id: 'sess_search_only',
    title: 'top 10 waterparks in the us',
    created: 1710000000000,
    lastMessageAt: 1710000001000,
    messages: [],
    _serverOnly: true,
  }];

  const session = await app.switchToSession('sess_search_only', { sync: false });

  if (!session) {
    fail(name, 'expected switchToSession to return the search result session');
    return;
  }
  if (!app.state.sessions.some((item) => item.id === 'sess_search_only')) {
    fail(name, 'expected search-only session to be added to state.sessions');
    return;
  }
  if (app.state.activeSessionId !== 'sess_search_only') {
    fail(name, `activeSessionId=${app.state.activeSessionId}, want sess_search_only`);
    return;
  }
  if (session._serverOnly) {
    fail(name, 'expected search-only session to be hydrated and no longer server-only');
    return;
  }
  if (session.messages.length !== 2 || session.messages[1].content !== 'loaded from server') {
    fail(name, 'expected hydrated server messages to be loaded', JSON.stringify(session.messages));
    return;
  }
  if (!fetchCalls.includes('/ui/v1/sessions/sess_search_only/messages')) {
    fail(name, 'expected messages endpoint to be fetched', JSON.stringify(fetchCalls));
    return;
  }

  pass(name);
}

(async () => {
  await testSwitchingSessionsStagesCurrentComposerBeforeRestore();
  await testSwitchToSessionSyncsSelectedRuntime();
  await testNumericDeepLinkResolvesRealSessionId();
  await testDeveloperMessagesAreHidden();
  await testConvertServerMessagesAttachesToolResultImages();
  await testSessionHistoryPaginationLoadsAdditionalPages();
  await testSessionHistoryPaginationFailureClearsEtagForRetry();
  await testSwitchToSessionSyncsWithoutTokenAndResumes();
  await testSwitchToSessionRecoversChangedActiveResponseFromSnapshot();
  await testSwitchToLazyLoadedSessionFetchesMessagesOnce();
  await testSwitchToSearchOnlySessionHydratesResult();
  await testSwitchToSessionClearsStaleActiveResponseWithoutToken();
  await testSessionState404ClearsStaleActiveResponse();
  await testIdleSessionSyncRescuesPendingInterruptCommit();
  await testSessionProgressStatePrefersLocalAndServerSignals();
  await testResumeAndDrainFiringViaSync();
  await testTerminalSyncRequeuesPendingInterjectionAsFollowUp();
  await testApplyServerSessionSummaryMapsLastMessageAt();
  await testMergeServerMessagesBumpsLastMessageAt();
  await testSanitizeSessionPreservesLastMessageAt();

  if (failures > 0) process.exit(1);
  process.exit(0);
})();
