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

function parsedTestURL(url) {
  try {
    return new URL(String(url), 'https://example.test');
  } catch (_) {
    return null;
  }
}

function isSessionMessagesURL(url, sessionId) {
  const parsed = parsedTestURL(url);
  return Boolean(parsed && parsed.pathname === `/ui/v1/sessions/${sessionId}/messages`);
}

function isTailMessagesURL(url, sessionId) {
  const parsed = parsedTestURL(url);
  return Boolean(parsed
    && parsed.pathname === `/ui/v1/sessions/${sessionId}/messages`
    && parsed.searchParams.get('tail') === '1'
    && parsed.searchParams.get('limit') === '200'
    && !parsed.searchParams.has('offset')
    && !parsed.searchParams.has('before_seq'));
}

function isOlderMessagesURL(url, sessionId, beforeSeq) {
  const parsed = parsedTestURL(url);
  return Boolean(parsed
    && parsed.pathname === `/ui/v1/sessions/${sessionId}/messages`
    && parsed.searchParams.get('before_seq') === String(beforeSeq)
    && parsed.searchParams.get('limit') === '200'
    && !parsed.searchParams.has('offset')
    && !parsed.searchParams.has('tail'));
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
    discardPendingAttachments() {},
    requeueUncommittedInterrupts() {},
    drainInterruptQueueIfIdle() {},
    requeuePendingInterjections() {},
    trackPendingInterjection() {},
    removePendingInterjectionById() {},
    trackPendingInterruptCommit() {},
    refreshPendingInterjectionBanner() {},
    restoreDraftMessageForSession() {},
    stageDraftMessage() {},
    clearDraftMessageForSession() {},
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

async function testSwitchingSessionsClearsEmptyComposerDraft() {
  const name = 'switching sessions clears stored draft when composer was emptied';
  const drafts = new Map([['sess_a', 'old draft in A']]);
  const { app } = await createSessionsHarness({
    fetchImpl: async () => new Response(JSON.stringify({ sessions: [] }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' }
    }),
    appOverrides: {
      stageDraftMessage(prompt, sessionId) {
        drafts.set(String(sessionId || ''), String(prompt || '').trim());
      },
      clearDraftMessageForSession(sessionId) {
        drafts.delete(String(sessionId || ''));
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
  app.elements.promptInput.value = '';

  await app.switchToSession(sessionB.id, { sync: false });
  if (drafts.has(sessionA.id)) {
    fail(name, 'expected empty composer to clear session A draft', JSON.stringify(Array.from(drafts.entries())));
    return;
  }

  await app.switchToSession(sessionA.id, { sync: false });
  if (app.elements.promptInput.value !== '') {
    fail(name, 'cleared draft should not restore when switching back', app.elements.promptInput.value);
    return;
  }
  pass(name);
}

async function testSwitchingSessionsDiscardsPendingAttachments() {
  const name = 'switching sessions discards pending attachments so they do not leak between chats';
  let discardCalls = 0;
  let appRef = null;
  const { app } = await createSessionsHarness({
    fetchImpl: async () => new Response(JSON.stringify({ sessions: [] }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' }
    }),
    appOverrides: {
      discardPendingAttachments() {
        discardCalls += 1;
        appRef.state.attachments = [];
      },
    }
  });
  appRef = app;

  const sessionA = { id: 'sess_a', title: 'A', messages: [], created: 1 };
  const sessionB = { id: 'sess_b', title: 'B', messages: [], created: 2 };
  app.state.sessions = [sessionA, sessionB];
  app.state.activeSessionId = sessionA.id;
  app.state.draftSessionActive = false;
  app.state.attachments = [{ id: 'att_a', name: 'a.png', type: 'image/png' }];

  await app.switchToSession(sessionB.id, { sync: false });

  if (discardCalls !== 1) {
    fail(name, `expected discardPendingAttachments to be called once, got ${discardCalls}`);
    return;
  }
  if (app.state.attachments.length !== 0) {
    fail(name, 'expected pending attachments to be cleared after switching sessions', JSON.stringify(app.state.attachments));
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
      if (isTailMessagesURL(url, 'sess_real')) {
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
      if (isTailMessagesURL(url, '1291')) {
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
  if (!fetchCalls.some((url) => isTailMessagesURL(url, 'sess_real'))) {
    fail(name, 'should load tail messages via resolved server session id', JSON.stringify(fetchCalls));
    return;
  }
  if (fetchCalls.some((url) => isSessionMessagesURL(url, 'pending_1291'))) {
    fail(name, 'should not use pending_ prefix in session id', JSON.stringify(fetchCalls));
    return;
  }
  pass(name);
}

async function testMergeServerSessionsMigratesInterruptBuffersToRealSessionId() {
  const name = 'session id reconciliation migrates interrupt buffers to real session id';
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
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
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
  });

  app.state.sessions = [{ id: '1291', number: 1291, title: 'Local numeric', messages: [] }];
  app.state.activeSessionId = '1291';
  app.state.currentStreamSessionId = '1291';
  app.state.queuedInterrupts = [{ sessionId: '1291', prompt: 'queued', messageId: 'msg_q' }];
  app.state.pendingInterruptCommits = [{ sessionId: '1291', prompt: 'pending commit', messageId: 'msg_c' }];
  app.state.pendingInterjections = [{ sessionId: '1291', prompt: 'pending interject', messageId: 'msg_i' }];

  await app.mergeServerSessions();

  const allIds = [
    app.state.activeSessionId,
    app.state.currentStreamSessionId,
    ...app.state.queuedInterrupts.map(entry => entry.sessionId),
    ...app.state.pendingInterruptCommits.map(entry => entry.sessionId),
    ...app.state.pendingInterjections.map(entry => entry.sessionId),
  ];
  if (allIds.some(id => id !== 'sess_real')) {
    fail(name, 'expected all session-bound interrupt state to migrate to real session id', JSON.stringify({
      activeSessionId: app.state.activeSessionId,
      currentStreamSessionId: app.state.currentStreamSessionId,
      queuedInterrupts: app.state.queuedInterrupts,
      pendingInterruptCommits: app.state.pendingInterruptCommits,
      pendingInterjections: app.state.pendingInterjections,
    }));
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
      if (isTailMessagesURL(url, 'sess_dev')) {
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

async function testConvertServerMessagesCompactionSummariesBecomeMarkers() {
  const name = 'server compaction summary converts to compact marker with active boundary';
  const { app } = await createSessionsHarness();
  const raw = '[Context Compaction]\nInternal context only.\n\n<PREVIOUS_TURNS>\nnoisy tool output\n</PREVIOUS_TURNS>';

  const converted = app.convertServerMessages([
    {
      sequence: 9,
      role: 'system',
      created_at: 900,
      parts: [{ type: 'text', text: 'system prompt' }],
    },
    {
      sequence: 10,
      role: 'user',
      created_at: 1000,
      parts: [{ type: 'text', text: raw }],
    },
    {
      sequence: 11,
      role: 'assistant',
      created_at: 1100,
      parts: [{ type: 'text', text: 'ack' }],
    },
  ], { compactionSeq: 9, compactionCount: 2 });

  if (converted.length !== 2) {
    fail(name, `expected compaction marker + assistant, got ${converted.length}`, JSON.stringify(converted));
    return;
  }
  const marker = converted[0];
  if (marker.role !== 'compaction' || marker.rawContent !== raw || !marker.activeBoundary || marker.compactionCount !== 2) {
    fail(name, 'converted compaction marker missing expected fields', JSON.stringify(marker));
    return;
  }
  if (String(marker.content || '').includes('noisy tool output')) {
    fail(name, 'compact marker display content should not include raw transcript', JSON.stringify(marker));
    return;
  }

  pass(name);
}

async function testConvertServerMessagesSuppressesCompactionRetainedRawTail() {
  const name = 'server compaction conversion suppresses post-marker retained raw suffix';
  const { app } = await createSessionsHarness();
  const raw = '[Context Compaction]\n\n<SUMMARY_AND_NEXT_ACTIONS>\ncontinue with recent answer\n</SUMMARY_AND_NEXT_ACTIONS>';

  const converted = app.convertServerMessages([
    { sequence: 1, role: 'user', created_at: 1000, parts: [{ type: 'text', text: 'old prompt' }] },
    { sequence: 2, role: 'user', created_at: 2000, parts: [{ type: 'text', text: 'recent prompt' }] },
    { sequence: 3, role: 'assistant', created_at: 3000, parts: [{ type: 'text', text: 'recent answer' }] },
    { sequence: 4, role: 'user', created_at: 4000, parts: [{ type: 'text', text: raw }] },
    { sequence: 5, role: 'user', created_at: 5000, parts: [{ type: 'text', text: 'recent prompt' }] },
    { sequence: 6, role: 'assistant', created_at: 6000, parts: [{ type: 'text', text: 'recent answer' }] },
  ], { compactionSeq: 4, compactionCount: 1 });

  const rolesAndContent = converted.map((message) => `${message.role}:${message.content}`);
  const want = [
    'user:old prompt',
    'user:recent prompt',
    'assistant:recent answer',
    'compaction:Context compacted',
  ];
  if (JSON.stringify(rolesAndContent) !== JSON.stringify(want)) {
    fail(name, 'unexpected converted display messages', JSON.stringify(converted));
    return;
  }
  if (!converted[3].activeBoundary || converted[3].lineCount !== 1) {
    fail(name, 'compaction marker should remain the active boundary and count summary lines only', JSON.stringify(converted[3]));
    return;
  }

  const convertedWithAck = app.convertServerMessages([
    { sequence: 1, role: 'user', created_at: 1000, parts: [{ type: 'text', text: 'old prompt' }] },
    { sequence: 2, role: 'user', created_at: 2000, parts: [{ type: 'text', text: 'recent prompt' }] },
    { sequence: 3, role: 'assistant', created_at: 3000, parts: [{ type: 'text', text: 'recent answer' }] },
    { sequence: 4, role: 'user', created_at: 4000, parts: [{ type: 'text', text: raw }] },
    { sequence: 5, role: 'assistant', created_at: 4500, parts: [{ type: 'text', text: "I've reviewed the context summary. I'll continue from where we left off." }] },
    { sequence: 6, role: 'user', created_at: 5000, parts: [{ type: 'text', text: 'recent prompt' }] },
    { sequence: 7, role: 'assistant', created_at: 6000, parts: [{ type: 'text', text: 'recent answer' }] },
  ], { compactionSeq: 4, compactionCount: 1 });
  if (JSON.stringify(convertedWithAck.map((message) => `${message.role}:${message.content}`)) !== JSON.stringify(want)) {
    fail(name, 'synthetic ack and retained tail should both be suppressed', JSON.stringify(convertedWithAck));
    return;
  }

  pass(name);
}

async function testConvertServerMessagesSuppressesAuthoritativeCompactionTailFlag() {
  const name = 'server compaction conversion suppresses authoritative compaction tail flag';
  const { app } = await createSessionsHarness();
  const raw = '[Context Compaction]\n\n<SUMMARY_AND_NEXT_ACTIONS>\ncontinue\n</SUMMARY_AND_NEXT_ACTIONS>';

  const converted = app.convertServerMessages([
    { sequence: 1, role: 'user', compaction_tail: false, created_at: 1000, parts: [{ type: 'text', text: 'recent prompt' }] },
    { sequence: 2, role: 'assistant', compaction_tail: false, created_at: 2000, parts: [{ type: 'text', text: 'recent answer' }] },
    { sequence: 3, role: 'user', compaction_tail: false, created_at: 3000, parts: [{ type: 'text', text: raw }] },
    { sequence: 4, role: 'assistant', compaction_tail: true, created_at: 4000, parts: [{ type: 'text', text: 'recent answer' }] },
    { sequence: 5, role: 'user', compaction_tail: false, created_at: 5000, parts: [{ type: 'text', text: 'recent answer' }] },
  ], { compactionSeq: 3, compactionCount: 1 });

  const rolesAndContent = converted.map((message) => `${message.role}:${message.content}`);
  const want = [
    'user:recent prompt',
    'assistant:recent answer',
    'compaction:Context compacted',
    'user:recent answer',
  ];
  if (JSON.stringify(rolesAndContent) !== JSON.stringify(want)) {
    fail(name, 'unexpected converted display messages', JSON.stringify(converted));
    return;
  }

  pass(name);
}

async function testConvertServerMessagesHandlesMixedLegacyAndAuthoritativeCompactionTails() {
  const name = 'server compaction conversion handles mixed legacy and authoritative tails';
  const { app } = await createSessionsHarness();
  const raw = '[Context Compaction]\n\n<SUMMARY_AND_NEXT_ACTIONS>\ncontinue\n</SUMMARY_AND_NEXT_ACTIONS>';

  const converted = app.convertServerMessages([
    { sequence: 1, role: 'user', created_at: 1000, parts: [{ type: 'text', text: 'legacy prompt' }] },
    { sequence: 2, role: 'assistant', created_at: 2000, parts: [{ type: 'text', text: 'legacy answer' }] },
    { sequence: 3, role: 'user', created_at: 3000, parts: [{ type: 'text', text: raw }] },
    { sequence: 4, role: 'user', created_at: 4000, parts: [{ type: 'text', text: 'legacy prompt' }] },
    { sequence: 5, role: 'assistant', created_at: 5000, parts: [{ type: 'text', text: 'legacy answer' }] },
    { sequence: 6, role: 'user', created_at: 6000, parts: [{ type: 'text', text: 'repeat after flagged marker' }] },
    { sequence: 7, role: 'user', created_at: 7000, parts: [{ type: 'text', text: raw }] },
    { sequence: 8, role: 'user', compaction_tail: true, created_at: 8000, parts: [{ type: 'text', text: 'repeat after flagged marker' }] },
    { sequence: 9, role: 'assistant', compaction_tail: true, created_at: 9000, parts: [{ type: 'text', text: 'hidden flagged tail' }] },
    { sequence: 10, role: 'user', created_at: 10000, parts: [{ type: 'text', text: 'repeat after flagged marker' }] },
  ], { compactionSeq: 7, compactionCount: 2 });

  const rolesAndContent = converted.map((message) => `${message.role}:${message.content}`);
  const want = [
    'user:legacy prompt',
    'assistant:legacy answer',
    'compaction:Context compacted',
    'user:repeat after flagged marker',
    'compaction:Context compacted',
    'user:repeat after flagged marker',
  ];
  if (JSON.stringify(rolesAndContent) !== JSON.stringify(want)) {
    fail(name, 'legacy tail should be heuristically suppressed while flagged marker keeps later repeated visible content', JSON.stringify(converted));
    return;
  }

  pass(name);
}

async function testConvertServerMessagesInsertsBoundaryWhenSummaryNotLoaded() {
  const name = 'server compaction boundary marker appears when compacted summary is outside loaded page';
  const { app } = await createSessionsHarness();

  const converted = app.convertServerMessages([
    {
      sequence: 42,
      role: 'assistant',
      created_at: 4200,
      parts: [{ type: 'text', text: 'recent raw answer' }],
    },
  ], { compactionSeq: 10, compactionCount: 1 });

  if (converted.length !== 2 || converted[0].role !== 'compaction-boundary' || !converted[0].activeBoundary) {
    fail(name, 'expected synthetic boundary before first visible compacted message', JSON.stringify(converted));
    return;
  }
  if (converted[1].role !== 'assistant' || converted[1].content !== 'recent raw answer') {
    fail(name, 'expected original assistant after boundary', JSON.stringify(converted));
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

async function testConvertServerMessagesSuppressesNonBubbleAssistantRows() {
  const name = 'server message conversion suppresses assistant rows without display text';
  const { app } = await createSessionsHarness();

  const converted = app.convertServerMessages([
    { sequence: 1, role: 'user', created_at: 1000, parts: [{ type: 'text', text: 'run a tool' }] },
    { sequence: 2, role: 'assistant', created_at: 2000, parts: [{ type: 'text', text: '  \n\t' }] },
    { sequence: 3, role: 'assistant', created_at: 3000, parts: [] },
    {
      sequence: 4,
      role: 'assistant',
      created_at: 4000,
      parts: [{ type: 'tool_call', tool_name: 'read_file', tool_call_id: 'call_1', tool_arguments: '{"path":"README.md"}' }],
    },
    {
      sequence: 5,
      role: 'tool',
      created_at: 5000,
      parts: [{ type: 'tool_result', tool_name: 'read_file', tool_call_id: 'call_1' }],
    },
    { sequence: 6, role: 'assistant', created_at: 6000, parts: [{ type: 'text', text: 'Done.' }] },
  ]);

  const rolesAndContent = converted.map((message) => `${message.role}:${message.content || ''}`);
  const want = ['user:run a tool', 'tool-group:', 'assistant:Done.'];
  if (JSON.stringify(rolesAndContent) !== JSON.stringify(want)) {
    fail(name, 'unexpected converted display messages', JSON.stringify(converted));
    return;
  }

  const blankAssistant = converted.find((message) => message.role === 'assistant' && String(message.content || '').trim() === '');
  if (blankAssistant) {
    fail(name, 'blank assistant bubble should be suppressed', JSON.stringify(blankAssistant));
    return;
  }

  pass(name);
}

async function testSessionHistoryInitialLoadRequestsTailOnly() {
  const name = 'initial session history load requests tail only';
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
      if (isTailMessagesURL(url, 'sess_page')) {
        return new Response(JSON.stringify({
          messages: [{
            id: 201,
            sequence: 200,
            role: 'user',
            created_at: 1710000000000,
            parts: [{ type: 'text', text: 'tail page' }],
          }],
          has_more: true,
          next_before_seq: 200,
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isSessionMessagesURL(url, 'sess_page')) {
        return new Response('unexpected non-tail messages request', { status: 500 });
      }
      if (url === '/ui/v1/sessions/sess_page/state') {
        return new Response(JSON.stringify({}), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify([]), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });

  const messageFetches = fetchCalls.filter((url) => isSessionMessagesURL(url, 'sess_page'));
  if (messageFetches.length !== 1 || !isTailMessagesURL(messageFetches[0], 'sess_page')) {
    fail(name, 'expected exactly one tail messages fetch', JSON.stringify(fetchCalls));
    return;
  }
  const offsetFetches = messageFetches.filter((url) => parsedTestURL(url)?.searchParams.has('offset'));
  if (offsetFetches.length !== 0) {
    fail(name, 'expected no offset pagination requests during initial load', JSON.stringify(fetchCalls));
    return;
  }

  const session = app.state.sessions.find((item) => item.id === 'sess_page');
  if (!session) {
    fail(name, 'session not found after tail load');
    return;
  }
  if (session.messages.length !== 1 || session.messages[0].content !== 'tail page') {
    fail(name, 'expected tail page to hydrate session messages', JSON.stringify(session.messages));
    return;
  }
  if (!session._history?.hasMoreOlder || session._history.oldestSeq !== 200) {
    fail(name, 'expected runtime history cursor to track older pages', JSON.stringify(session._history));
    return;
  }

  pass(name);
}

async function testScrollNearTopLoadsOlderPageAndPreservesViewport() {
  const name = 'scroll near top loads older session page and preserves viewport';
  const fetchCalls = [];
  let appRef = null;
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      fetchCalls.push(url);
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isTailMessagesURL(url, 'sess_scroll')) {
        return new Response(JSON.stringify({
          messages: [{
            id: 200,
            sequence: 200,
            role: 'assistant',
            created_at: 1710000002000,
            parts: [{ type: 'text', text: 'newest tail' }],
          }],
          has_more: true,
          next_before_seq: 200,
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isOlderMessagesURL(url, 'sess_scroll', 200)) {
        return new Response(JSON.stringify({
          messages: [{
            id: 199,
            sequence: 199,
            role: 'user',
            created_at: 1710000001000,
            parts: [{ type: 'text', text: 'older message' }],
          }],
          has_more: false,
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ messages: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
    appOverrides: {
      renderMessages() {
        if (!appRef) return;
        const active = appRef.state.sessions.find((session) => session.id === appRef.state.activeSessionId);
        appRef.elements.chatScroll.scrollHeight = active && active.messages.length > 1 ? 1600 : 1000;
      }
    }
  });
  appRef = app;

  const session = {
    id: 'sess_scroll',
    title: 'Scroll session',
    origin: 'web',
    created: 1710000000000,
    messages: [],
  };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;

  const tail = await app.loadServerSessionMessages(session.id);
  app.mergeServerMessagesWithLocalState(session, tail);
  app.elements.chatScroll.scrollHeight = 1000;
  app.elements.chatScroll.scrollTop = 100;
  app.elements.chatScroll.clientHeight = 400;
  fetchCalls.length = 0;

  const loaded = await app.maybeLoadOlderSessionMessages();
  if (!loaded) {
    fail(name, 'expected near-top scroll to load older history');
    return;
  }
  if (!fetchCalls.some((url) => isOlderMessagesURL(url, 'sess_scroll', 200))) {
    fail(name, 'expected older page request with before_seq cursor', JSON.stringify(fetchCalls));
    return;
  }
  if (session.messages.length !== 2 || session.messages[0].content !== 'older message' || session.messages[1].content !== 'newest tail') {
    fail(name, 'expected older messages to be prepended before tail', JSON.stringify(session.messages));
    return;
  }
  if (app.elements.chatScroll.scrollTop !== 700) {
    fail(name, `expected scrollTop to preserve viewport at 700, got ${app.elements.chatScroll.scrollTop}`);
    return;
  }
  if (session._history.hasMoreOlder) {
    fail(name, 'expected hasMoreOlder=false after final older page', JSON.stringify(session._history));
    return;
  }

  pass(name);
}

async function testTailRefreshPreservesOlderHistoryCursor() {
  const name = 'tail refresh preserves older history cursor after older pages load';
  const fetchCalls = [];
  let tailCalls = 0;
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      fetchCalls.push(url);
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isTailMessagesURL(url, 'sess_cursor')) {
        tailCalls += 1;
        return new Response(JSON.stringify({
          messages: [{
            id: 100,
            sequence: 100,
            role: 'assistant',
            created_at: 1710000002000,
            parts: [{ type: 'text', text: `tail refresh ${tailCalls}` }],
          }],
          has_more: true,
          next_before_seq: 100,
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isOlderMessagesURL(url, 'sess_cursor', 100)) {
        return new Response(JSON.stringify({
          messages: [{
            id: 55,
            sequence: 55,
            role: 'user',
            created_at: 1710000001000,
            parts: [{ type: 'text', text: 'older cursor page' }],
          }],
          has_more: true,
          next_before_seq: 50,
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isOlderMessagesURL(url, 'sess_cursor', 50)) {
        return new Response(JSON.stringify({
          messages: [{
            id: 25,
            sequence: 25,
            role: 'user',
            created_at: 1710000000500,
            parts: [{ type: 'text', text: 'oldest page' }],
          }],
          has_more: false,
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ messages: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });

  const session = {
    id: 'sess_cursor',
    title: 'Cursor session',
    origin: 'web',
    created: 1710000000000,
    messages: [],
  };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;
  app.elements.chatScroll.scrollTop = 0;
  app.elements.chatScroll.scrollHeight = 1000;
  app.elements.chatScroll.clientHeight = 400;

  let loaded = await app.loadServerSessionMessages(session.id);
  app.mergeServerMessagesWithLocalState(session, loaded);
  if (session._history.oldestSeq !== 100) {
    fail(name, `initial tail cursor = ${session._history.oldestSeq}, want 100`);
    return;
  }

  const olderLoaded = await app.maybeLoadOlderSessionMessages();
  if (!olderLoaded || session._history.oldestSeq !== 50) {
    fail(name, 'expected older page to set cursor from server response', JSON.stringify(session._history));
    return;
  }

  loaded = await app.loadServerSessionMessages(session.id);
  app.mergeServerMessagesWithLocalState(session, loaded);
  if (session._history.oldestSeq !== 50) {
    fail(name, 'tail refresh should preserve already-loaded older cursor', JSON.stringify(session._history));
    return;
  }

  fetchCalls.length = 0;
  const finalLoaded = await app.maybeLoadOlderSessionMessages();
  if (!finalLoaded) {
    fail(name, 'expected next older load to use the preserved cursor');
    return;
  }
  if (!fetchCalls.some((url) => isOlderMessagesURL(url, 'sess_cursor', 50))
      || fetchCalls.some((url) => isOlderMessagesURL(url, 'sess_cursor', 100))) {
    fail(name, 'expected next older request to use before_seq=50, not repeat before_seq=100', JSON.stringify(fetchCalls));
    return;
  }

  pass(name);
}

async function testCompactedTailRefreshPreservesPreCompactionScrollback() {
  const name = 'compacted tail refresh preserves loaded pre-compaction scrollback without overlap';
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isTailMessagesURL(url, 'sess_compacted_refresh')) {
        return new Response(JSON.stringify({
          messages: [
            {
              id: 50,
              sequence: 50,
              role: 'user',
              created_at: 1710000050000,
              parts: [{ type: 'text', text: '[Context Compaction]\n\n<SUMMARY_AND_NEXT_ACTIONS>\ncontinue\n</SUMMARY_AND_NEXT_ACTIONS>' }],
            },
            {
              id: 51,
              sequence: 51,
              role: 'assistant',
              created_at: 1710000051000,
              parts: [{ type: 'text', text: 'post compact answer' }],
            },
          ],
          has_more: false,
          compaction_seq: 50,
          compaction_count: 1,
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ messages: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });

  const session = {
    id: 'sess_compacted_refresh',
    title: 'Compacted refresh',
    origin: 'web',
    created: 1710000000000,
    messages: [],
  };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;

  // Seed history like a user who already had pre-compaction scrollback loaded.
  session._history = {
    rawMessages: [
      { id: 1, sequence: 1, role: 'user', created_at: 1710000001000, parts: [{ type: 'text', text: 'pre compact prompt' }] },
      { id: 2, sequence: 2, role: 'assistant', created_at: 1710000002000, parts: [{ type: 'text', text: 'pre compact reply' }] },
    ],
    oldestSeq: 1,
    hasMoreOlder: false,
    loadingOlder: false,
    loadedTail: true,
    lastResponseId: '',
    compactionSeq: -1,
    compactionCount: 0,
  };

  const refreshed = await app.loadServerSessionMessages(session.id);
  app.mergeServerMessagesWithLocalState(session, refreshed);

  const contents = session.messages.map((message) => `${message.role}:${message.content}`);
  const want = [
    'user:pre compact prompt',
    'assistant:pre compact reply',
    'compaction:Context compacted',
    'assistant:post compact answer',
  ];
  if (JSON.stringify(contents) !== JSON.stringify(want)) {
    fail(name, 'expected pre-compaction messages to survive non-overlapping compacted tail refresh', JSON.stringify(session.messages));
    return;
  }
  if (session._history.oldestSeq !== 1 || session._history.hasMoreOlder) {
    fail(name, 'expected old scrollback cursor to remain intact', JSON.stringify(session._history));
    return;
  }

  pass(name);
}

async function testCompactedTailRefreshPreservesLocalScrollbackWithoutRawHistory() {
  const name = 'compacted tail refresh preserves local scrollback when raw history was never loaded';
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isTailMessagesURL(url, 'sess_local_compacted_refresh')) {
        return new Response(JSON.stringify({
          messages: [
            {
              id: 50,
              sequence: 50,
              role: 'user',
              created_at: 1710000050000,
              parts: [{ type: 'text', text: '[Context Compaction]\n\n<SUMMARY_AND_NEXT_ACTIONS>\ncontinue\n</SUMMARY_AND_NEXT_ACTIONS>' }],
            },
            {
              id: 51,
              sequence: 51,
              role: 'assistant',
              created_at: 1710000051000,
              parts: [{ type: 'text', text: 'post compact answer' }],
            },
          ],
          has_more: false,
          compaction_seq: 50,
          compaction_count: 1,
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ messages: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });

  const session = {
    id: 'sess_local_compacted_refresh',
    title: 'Local compacted refresh',
    origin: 'web',
    created: 1710000000000,
    messages: [
      { id: 'local_1', role: 'user', content: 'local pre compact prompt', created: 1710000001000 },
      { id: 'local_2', role: 'assistant', content: 'local pre compact reply', created: 1710000002000 },
    ],
  };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;

  const refreshed = await app.loadServerSessionMessages(session.id);
  app.mergeServerMessagesWithLocalState(session, refreshed);

  const contents = session.messages.map((message) => `${message.role}:${message.content}`);
  const want = [
    'user:local pre compact prompt',
    'assistant:local pre compact reply',
    'compaction:Context compacted',
    'assistant:post compact answer',
  ];
  if (JSON.stringify(contents) !== JSON.stringify(want)) {
    fail(name, 'expected visible local scrollback to survive compacted tail refresh', JSON.stringify(session.messages));
    return;
  }

  pass(name);
}

async function testOlderPageFailureAllowsRetryWithoutCorruptingTail() {
  const name = 'failed older-page fetch allows retry without corrupting loaded tail';
  const fetchCalls = [];
  let olderCalls = 0;
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      fetchCalls.push(url);
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isTailMessagesURL(url, 'sess_retry')) {
        return new Response(JSON.stringify({
          messages: [{
            id: 200,
            sequence: 200,
            role: 'assistant',
            created_at: 1710000002000,
            parts: [{ type: 'text', text: 'tail survives' }],
          }],
          has_more: true,
          next_before_seq: 200,
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isOlderMessagesURL(url, 'sess_retry', 200)) {
        olderCalls += 1;
        if (olderCalls === 1) {
          return new Response('temporary failure', { status: 500 });
        }
        return new Response(JSON.stringify({
          messages: [{
            id: 199,
            sequence: 199,
            role: 'user',
            created_at: 1710000001000,
            parts: [{ type: 'text', text: 'retry older' }],
          }],
          has_more: false,
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ messages: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });

  const session = {
    id: 'sess_retry',
    title: 'Retry session',
    origin: 'web',
    created: 1710000000000,
    messages: [],
  };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;

  const tail = await app.loadServerSessionMessages(session.id);
  app.mergeServerMessagesWithLocalState(session, tail);
  app.elements.chatScroll.scrollTop = 0;
  app.elements.chatScroll.scrollHeight = 1000;
  app.elements.chatScroll.clientHeight = 400;

  const failed = await app.maybeLoadOlderSessionMessages();
  if (failed) {
    fail(name, 'expected first older-page load to fail');
    return;
  }
  if (session._history.loadingOlder) {
    fail(name, 'expected loadingOlder=false after failure', JSON.stringify(session._history));
    return;
  }
  if (session.messages.length !== 1 || session.messages[0].content !== 'tail survives') {
    fail(name, 'expected failed older load not to corrupt tail messages', JSON.stringify(session.messages));
    return;
  }

  const retried = await app.maybeLoadOlderSessionMessages();
  if (!retried || olderCalls !== 2) {
    fail(name, 'expected retry to fetch older page successfully', JSON.stringify({ olderCalls, fetchCalls }));
    return;
  }
  if (session.messages.length !== 2 || session.messages[0].content !== 'retry older') {
    fail(name, 'expected retry older page to prepend successfully', JSON.stringify(session.messages));
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
      if (isTailMessagesURL(url, 'sess_other')) {
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
      if (isTailMessagesURL(url, 'sess_other')) {
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
      if (isTailMessagesURL(url, 'sess_lazy')) {
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

  const messageFetches = fetchCalls.filter((url) => isSessionMessagesURL(url, 'sess_lazy'));
  if (messageFetches.length !== 1 || !isTailMessagesURL(messageFetches[0], 'sess_lazy')) {
    fail(name, 'expected exactly one lazy session tail messages fetch during switch', JSON.stringify(fetchCalls));
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
      if (isTailMessagesURL(url, 'sess_other')) {
        return new Response(JSON.stringify({ messages: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/sess_stale/state') {
        return new Response(JSON.stringify({ active_run: false }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isTailMessagesURL(url, 'sess_stale')) {
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

  if (!fetchCalls.some((url) => isTailMessagesURL(url, 'sess_stale'))) {
    fail(name, 'expected completed session sync to reload tail messages from server', JSON.stringify(fetchCalls));
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
      if (isTailMessagesURL(url, 'sess_state_404')) {
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
  app.state.draftSessionActive = false;

  const runtimeState = await app.syncActiveSessionFromServer(session, false);

  if (!runtimeState || runtimeState.active_run !== false) {
    fail(name, 'expected state 404 to produce inactive runtime state', JSON.stringify(runtimeState));
    return;
  }
  if (!fetchCalls.some((url) => isTailMessagesURL(url, 'sess_state_404'))) {
    fail(name, 'expected inactive 404 sync to continue through tail message refresh', JSON.stringify(fetchCalls));
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
      if (isTailMessagesURL(url, 'sess_interrupt')) {
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
          sessionId: session.id,
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
        const queuedIndex = appRef.state.queuedInterrupts.findIndex(entry => entry.sessionId === session.id);
        if (queuedIndex >= 0) {
          const [queued] = appRef.state.queuedInterrupts.splice(queuedIndex, 1);
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
        const queuedIndex = appRef.state.queuedInterrupts.findIndex(entry => entry.sessionId === session.id);
        if (queuedIndex >= 0) {
          const [queued] = appRef.state.queuedInterrupts.splice(queuedIndex, 1);
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
  app.state.queuedInterrupts = [{ sessionId: session.id, prompt: 'queued after resume', messageId: 'msg_drain_1' }];

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
      if (isTailMessagesURL(url, 'sess_reload')) {
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
            appRef.state.queuedInterrupts.push({ sessionId: session.id, prompt: entry.prompt, messageId: entry.messageId });
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
        const queuedIndex = appRef.state.queuedInterrupts.findIndex(entry => entry.sessionId === session.id);
        if (queuedIndex >= 0) {
          const [queued] = appRef.state.queuedInterrupts.splice(queuedIndex, 1);
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

async function testSyncUsesServerProvidedPendingInterjectionId() {
  const name = 'idle sync tracks pending_interjection under server-provided id, not a synthetic id';
  const trackInterjectionCalls = [];
  const trackCommitCalls = [];
  let appRef = null;

  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (url === '/ui/v1/sessions/sess_reload/state') {
        return new Response(JSON.stringify({
          active_run: true,
          pending_interjection: { id: 'real-id', text: 'rescued prompt' }
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isTailMessagesURL(url, 'sess_reload')) {
        return new Response(JSON.stringify({ messages: [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      return new Response(JSON.stringify({ sessions: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' }
      });
    },
    appOverrides: {
      trackPendingInterjection(sessionId, prompt, messageId, action) {
        trackInterjectionCalls.push({ sessionId, prompt, messageId, action });
        appRef.state.pendingInterjections.push({ sessionId, prompt, messageId, action });
      },
      trackPendingInterruptCommit(sessionId, prompt, messageId) {
        trackCommitCalls.push({ sessionId, prompt, messageId });
      },
      removePendingInterjectionById() {},
      refreshPendingInterjectionBanner() {},
      requeuePendingInterjections() {},
      requeueUncommittedInterrupts() {},
      drainInterruptQueueIfIdle() {},
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

  if (trackInterjectionCalls.length !== 1) {
    fail(name, `expected 1 trackPendingInterjection call, got ${trackInterjectionCalls.length}`, JSON.stringify(trackInterjectionCalls));
    return;
  }
  if (trackInterjectionCalls[0].messageId !== 'real-id') {
    fail(name, `trackPendingInterjection messageId = ${trackInterjectionCalls[0].messageId}, want "real-id"`);
    return;
  }
  if (trackCommitCalls.length !== 1 || trackCommitCalls[0].messageId !== 'real-id') {
    fail(name, `trackPendingInterruptCommit messageId = ${trackCommitCalls[0]?.messageId}, want "real-id"`, JSON.stringify(trackCommitCalls));
    return;
  }

  pass(name);
}

async function testApplyServerSessionSummaryMapsTranscriptUpdatedAt() {
  const name = 'applyServerSessionSummary maps transcript_updated_at into transcriptUpdatedAt';
  const { app } = await createSessionsHarness();

  const target = {
    id: 'sess_transcript_summary',
    title: 'existing',
    created: 1000,
    messages: [],
  };
  app.applyServerSessionSummary(target, {
    id: 'sess_transcript_summary',
    short_title: 'Updated',
    created_at: 1000,
    transcript_updated_at: 987654321,
    message_count: 2,
  });

  if (target.transcriptUpdatedAt !== 987654321) {
    fail(name, `expected transcriptUpdatedAt=987654321, got ${target.transcriptUpdatedAt}`);
    return;
  }

  const omitted = { id: 'sess_omitted', created: 1000, transcriptUpdatedAt: 123, messages: [] };
  app.applyServerSessionSummary(omitted, { id: 'sess_omitted', created_at: 1000 });
  if (omitted.transcriptUpdatedAt !== 123) {
    fail(name, `existing transcriptUpdatedAt should be preserved when server omits field, got ${omitted.transcriptUpdatedAt}`);
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

async function testSanitizeSessionPreservesTranscriptUpdatedAt() {
  const name = 'sanitizeSession preserves transcriptUpdatedAt from stored and server-shaped payloads';
  const { app } = await createSessionsHarness();

  const stored = app.sanitizeSession({
    id: 'sess_transcript_stored',
    title: 'Stored',
    created: 1000,
    transcriptUpdatedAt: 3210,
    messages: [],
  });
  if (stored.transcriptUpdatedAt !== 3210) {
    fail(name, `expected transcriptUpdatedAt=3210 from camelCase, got ${stored.transcriptUpdatedAt}`);
    return;
  }

  const serverShaped = app.sanitizeSession({
    id: 'sess_transcript_server',
    title: 'Server',
    created: 1000,
    transcript_updated_at: 6540,
    messages: [],
  });
  if (serverShaped.transcriptUpdatedAt !== 6540) {
    fail(name, `expected transcriptUpdatedAt=6540 from snake_case, got ${serverShaped.transcriptUpdatedAt}`);
    return;
  }

  const absent = app.sanitizeSession({
    id: 'sess_transcript_absent',
    title: 'Absent',
    created: 1000,
    messages: [],
  });
  if (Object.prototype.hasOwnProperty.call(absent, 'transcriptUpdatedAt')) {
    fail(name, 'transcriptUpdatedAt should remain absent when no version was provided', JSON.stringify(absent));
    return;
  }

  pass(name);
}

async function testStatusPollAdvancementRefreshesActiveMessagesOnce() {
  const name = 'status poll transcript advancement refreshes active messages exactly once';
  const fetchCalls = [];
  let statusSessions = [];
  let messageCalls = 0;
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      fetchCalls.push(String(url));
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/status') {
        return new Response(JSON.stringify({ sessions: statusSessions }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isTailMessagesURL(url, 'sess_status_active')) {
        messageCalls += 1;
        return new Response(JSON.stringify({
          messages: [{
            id: `msg_${messageCalls}`,
            sequence: messageCalls,
            role: 'assistant',
            created_at: 1710000000000 + messageCalls,
            parts: [{ type: 'text', text: `server transcript ${messageCalls}` }],
          }]
        }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: `"tail-${messageCalls}"` } });
      }
      return new Response(JSON.stringify({ messages: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });
  app.stopSidebarStatusPoll();

  const session = {
    id: 'sess_status_active',
    title: 'Status active',
    origin: 'web',
    created: 1710000000000,
    transcriptUpdatedAt: 1000,
    messages: [],
  };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;
  statusSessions = [{
    id: session.id,
    short_title: 'Status active',
    long_title: 'Status active',
    active_run: false,
    message_count: 1,
    last_message_at: 1710000000001,
    transcript_updated_at: 2000,
  }];
  fetchCalls.length = 0;

  await app.startSidebarStatusPoll();
  app.stopSidebarStatusPoll();

  if (messageCalls !== 1) {
    fail(name, `expected one messages refresh after transcript advancement, got ${messageCalls}`, JSON.stringify(fetchCalls));
    return;
  }
  if (session.messages.length !== 1 || session.messages[0].content !== 'server transcript 1') {
    fail(name, 'expected active session messages to be replaced by server tail', JSON.stringify(session.messages));
    return;
  }
  if (session.transcriptUpdatedAt !== 2000 || session._history?.tailTranscriptUpdatedAt !== 2000) {
    fail(name, 'expected transcript version to be marked synced after refresh', JSON.stringify({ transcriptUpdatedAt: session.transcriptUpdatedAt, history: session._history }));
    return;
  }

  await app.startSidebarStatusPoll();
  app.stopSidebarStatusPoll();
  if (messageCalls !== 1) {
    fail(name, `unchanged transcript version should not fetch messages again, got ${messageCalls}`, JSON.stringify(fetchCalls));
    return;
  }

  pass(name);
}

async function testStatusPollUnchangedTranscriptDoesNotFetchMessages() {
  const name = 'status poll with unchanged active transcript version does not fetch messages';
  let messageCalls = 0;
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/status') {
        return new Response(JSON.stringify({
          sessions: [{
            id: 'sess_status_unchanged',
            short_title: 'Unchanged',
            active_run: false,
            message_count: 1,
            last_message_at: 1710000001000,
            transcript_updated_at: 5000,
          }]
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isTailMessagesURL(url, 'sess_status_unchanged')) {
        messageCalls += 1;
        return new Response(JSON.stringify({ messages: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ messages: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });
  app.stopSidebarStatusPoll();

  const session = {
    id: 'sess_status_unchanged',
    title: 'Unchanged',
    origin: 'web',
    created: 1710000000000,
    transcriptUpdatedAt: 5000,
    messages: [{ id: 'local_msg', role: 'assistant', content: 'already synced', created: 1710000000000 }],
  };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;

  await app.startSidebarStatusPoll();
  app.stopSidebarStatusPoll();

  if (messageCalls !== 0) {
    fail(name, `expected zero message fetches for unchanged transcript version, got ${messageCalls}`);
    return;
  }
  if (session.messages.length !== 1 || session.messages[0].content !== 'already synced') {
    fail(name, 'unchanged status should not alter active messages', JSON.stringify(session.messages));
    return;
  }

  pass(name);
}

async function testActiveTranscriptRefreshSkipsBusyStates() {
  const name = 'active transcript refresh skips while active session is busy';
  let messageCalls = 0;
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isTailMessagesURL(url, 'sess_busy_refresh')) {
        messageCalls += 1;
        return new Response(JSON.stringify({ messages: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });
  app.stopSidebarStatusPoll();

  const session = {
    id: 'sess_busy_refresh',
    title: 'Busy refresh',
    origin: 'web',
    created: 1710000000000,
    transcriptUpdatedAt: 1000,
    messages: [],
  };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;

  app.state.streaming = true;
  await app.refreshActiveSessionMessagesFromServer(session, { transcriptUpdatedAt: 2000 });
  app.state.streaming = false;

  app.state.abortController = new AbortController();
  await app.refreshActiveSessionMessagesFromServer(session, { transcriptUpdatedAt: 2000 });
  app.state.abortController = null;

  session.activeResponseId = 'resp_busy';
  await app.refreshActiveSessionMessagesFromServer(session, { transcriptUpdatedAt: 2000 });
  session.activeResponseId = null;

  app.setSessionServerActiveRun(session, true);
  await app.refreshActiveSessionMessagesFromServer(session, { transcriptUpdatedAt: 2000 });
  app.setSessionServerActiveRun(session, false);

  if (messageCalls !== 0) {
    fail(name, `expected no message fetches while busy, got ${messageCalls}`);
    return;
  }
  if (session.transcriptUpdatedAt !== 1000) {
    fail(name, `skipped refresh should leave transcriptUpdatedAt pending at 1000, got ${session.transcriptUpdatedAt}`);
    return;
  }

  pass(name);
}

async function testLateActiveMessagesResponseIgnoredAfterSessionSwitch() {
  const name = 'late active messages response is ignored after switching sessions';
  let resolveMessages;
  let messageCalls = 0;
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isTailMessagesURL(url, 'sess_late_a')) {
        messageCalls += 1;
        return new Promise((resolve) => {
          resolveMessages = () => resolve(new Response(JSON.stringify({
            messages: [{
              id: 'late_msg',
              sequence: 1,
              role: 'assistant',
              created_at: 1710000000001,
              parts: [{ type: 'text', text: 'late server message' }],
            }]
          }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
        });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });
  app.stopSidebarStatusPoll();

  const sessionA = { id: 'sess_late_a', title: 'A', origin: 'web', created: 1, transcriptUpdatedAt: 1000, messages: [] };
  const sessionB = { id: 'sess_late_b', title: 'B', origin: 'web', created: 2, messages: [] };
  app.state.sessions = [sessionA, sessionB];
  app.state.activeSessionId = sessionA.id;
  app.state.draftSessionActive = false;

  const refreshPromise = app.refreshActiveSessionMessagesFromServer(sessionA, { transcriptUpdatedAt: 2000 });
  if (messageCalls !== 1 || typeof resolveMessages !== 'function') {
    fail(name, 'expected refresh to start one tail request before switching', `calls=${messageCalls}`);
    return;
  }

  app.state.activeSessionId = sessionB.id;
  resolveMessages();
  const applied = await refreshPromise;

  if (applied) {
    fail(name, 'refresh should report false when active session changed before response applied');
    return;
  }
  if (sessionA.messages.length !== 0) {
    fail(name, 'late response should not mutate inactive session messages through active refresh', JSON.stringify(sessionA.messages));
    return;
  }
  if (sessionA.transcriptUpdatedAt !== 1000) {
    fail(name, `late response should not mark transcript synced, got ${sessionA.transcriptUpdatedAt}`);
    return;
  }

  pass(name);
}

async function testLateActiveRunSyncDoesNotMarkDraftStreaming() {
  const name = 'late active-run sync does not mark New Chat draft as streaming';
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/sess_late_busy/state') {
        return new Response(JSON.stringify({ active_run: true }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });
  app.stopSidebarStatusPoll();

  const session = {
    id: 'sess_late_busy',
    title: 'Late busy',
    origin: 'web',
    created: 1,
    messages: [],
    activeResponseId: null,
    lastSequenceNumber: 0,
  };
  app.state.sessions = [session];
  app.state.activeSessionId = '';
  app.state.draftSessionActive = true;
  app.state.streaming = false;

  await app.syncActiveSessionFromServer(session, false);

  if (app.state.streaming) {
    fail(name, 'draft New Chat should stay idle even if an inactive session sync reports active_run=true');
    return;
  }
  if (!app.sessionHasInProgressState(session)) {
    fail(name, 'inactive session should still retain its sidebar busy state');
    return;
  }

  pass(name);
}

async function testOlderPendingTranscriptVersionIsIgnoredOnStatus304() {
  const name = 'older pending transcript version is ignored on status 304';
  let messageCalls = 0;
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/status') {
        return new Response(null, { status: 304, headers: { ETag: '"unchanged"' } });
      }
      if (isTailMessagesURL(url, 'sess_pending_old')) {
        messageCalls += 1;
        return new Response(JSON.stringify({ messages: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ messages: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });
  app.stopSidebarStatusPoll();

  const session = {
    id: 'sess_pending_old',
    title: 'Pending old',
    origin: 'web',
    created: 1710000000000,
    transcriptUpdatedAt: 3000,
    _pendingTranscriptUpdatedAt: 2000,
    _history: {
      rawMessages: [],
      oldestSeq: 0,
      hasMoreOlder: false,
      loadingOlder: false,
      loadedTail: true,
      lastResponseId: '',
      compactionSeq: -1,
      compactionCount: 0,
      tailEtag: '"tail-current"',
      tailTranscriptUpdatedAt: 3000,
      refreshingTail: false,
    },
    messages: [],
  };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;

  await app.startSidebarStatusPoll();
  app.stopSidebarStatusPoll();

  if (messageCalls !== 0) {
    fail(name, `expected no tail refresh for stale pending version, got ${messageCalls}`);
    return;
  }
  if (session.transcriptUpdatedAt !== 3000 || session._history.tailTranscriptUpdatedAt !== 3000) {
    fail(name, 'stale pending version should not regress transcript markers', JSON.stringify({ transcriptUpdatedAt: session.transcriptUpdatedAt, history: session._history }));
    return;
  }
  if (Object.prototype.hasOwnProperty.call(session, '_pendingTranscriptUpdatedAt')) {
    fail(name, 'stale pending marker should be cleared', JSON.stringify(session));
    return;
  }

  pass(name);
}

async function testSyncActiveSessionIdleUsesTranscriptRefreshHelper() {
  const name = 'syncActiveSessionFromServer idle branch refreshes through active transcript helper';
  let messageCalls = 0;
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (url === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (url === '/ui/v1/sessions/sess_sync_helper/state') {
        return new Response(JSON.stringify({ active_run: false }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isTailMessagesURL(url, 'sess_sync_helper')) {
        messageCalls += 1;
        return new Response(JSON.stringify({
          messages: [{
            id: 'sync_msg',
            sequence: 1,
            role: 'assistant',
            created_at: 1710000000001,
            parts: [{ type: 'text', text: 'synced through helper' }],
          }]
        }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"sync-helper"' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });
  app.stopSidebarStatusPoll();

  const session = {
    id: 'sess_sync_helper',
    title: 'Sync helper',
    origin: 'web',
    created: 1710000000000,
    transcriptUpdatedAt: 7000,
    messages: [],
  };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;

  await app.syncActiveSessionFromServer(session, false);

  if (messageCalls !== 1) {
    fail(name, `expected idle sync to fetch active messages once, got ${messageCalls}`);
    return;
  }
  if (session.messages.length !== 1 || session.messages[0].content !== 'synced through helper') {
    fail(name, 'expected idle sync to apply refreshed server transcript', JSON.stringify(session.messages));
    return;
  }
  if (session._history?.tailTranscriptUpdatedAt !== 7000 || session._history?.tailEtag !== '"sync-helper"') {
    fail(name, 'expected helper to mark transcript version and tail ETag after idle sync', JSON.stringify(session._history));
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
      if (isTailMessagesURL(url, 'sess_search_only')) {
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
  if (!fetchCalls.some((url) => isTailMessagesURL(url, 'sess_search_only'))) {
    fail(name, 'expected tail messages endpoint to be fetched', JSON.stringify(fetchCalls));
    return;
  }

  pass(name);
}

(async () => {
  await testSwitchingSessionsStagesCurrentComposerBeforeRestore();
  await testSwitchingSessionsClearsEmptyComposerDraft();
  await testSwitchingSessionsDiscardsPendingAttachments();
  await testSwitchToSessionSyncsSelectedRuntime();
  await testNumericDeepLinkResolvesRealSessionId();
  await testMergeServerSessionsMigratesInterruptBuffersToRealSessionId();
  await testDeveloperMessagesAreHidden();
  await testConvertServerMessagesCompactionSummariesBecomeMarkers();
  await testConvertServerMessagesSuppressesCompactionRetainedRawTail();
  await testConvertServerMessagesSuppressesAuthoritativeCompactionTailFlag();
  await testConvertServerMessagesHandlesMixedLegacyAndAuthoritativeCompactionTails();
  await testConvertServerMessagesInsertsBoundaryWhenSummaryNotLoaded();
  await testConvertServerMessagesAttachesToolResultImages();
  await testConvertServerMessagesSuppressesNonBubbleAssistantRows();
  await testSessionHistoryInitialLoadRequestsTailOnly();
  await testScrollNearTopLoadsOlderPageAndPreservesViewport();
  await testTailRefreshPreservesOlderHistoryCursor();
  await testCompactedTailRefreshPreservesPreCompactionScrollback();
  await testCompactedTailRefreshPreservesLocalScrollbackWithoutRawHistory();
  await testOlderPageFailureAllowsRetryWithoutCorruptingTail();
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
  await testSyncUsesServerProvidedPendingInterjectionId();
  await testApplyServerSessionSummaryMapsTranscriptUpdatedAt();
  await testApplyServerSessionSummaryMapsLastMessageAt();
  await testMergeServerMessagesBumpsLastMessageAt();
  await testSanitizeSessionPreservesLastMessageAt();
  await testSanitizeSessionPreservesTranscriptUpdatedAt();
  await testStatusPollAdvancementRefreshesActiveMessagesOnce();
  await testStatusPollUnchangedTranscriptDoesNotFetchMessages();
  await testActiveTranscriptRefreshSkipsBusyStates();
  await testLateActiveMessagesResponseIgnoredAfterSessionSwitch();
  await testLateActiveRunSyncDoesNotMarkDraftStreaming();
  await testOlderPendingTranscriptVersionIsIgnoredOnStatus304();
  await testSyncActiveSessionIdleUsesTranscriptRefreshHelper();

  if (failures > 0) process.exit(1);
  process.exit(0);
})();
