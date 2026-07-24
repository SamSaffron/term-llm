#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const { webcrypto } = require('crypto');

const dir = __dirname;
const transcriptStoreSource = fs.readFileSync(path.join(dir, 'transcript-store.js'), 'utf8');
const coreSource = fs.readFileSync(path.join(dir, 'app-core.js'), 'utf8');
const planSource = fs.readFileSync(path.join(dir, 'app-plan.js'), 'utf8');
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

async function waitFor(predicate, message, attempts = 50) {
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    if (predicate()) return;
    await new Promise((resolve) => setImmediate(resolve));
  }
  throw new Error(message || 'condition was not met');
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

function makeNode(tagName = 'div') {
  const children = [];
  const attributes = new Map();
  const node = {
    tagName: String(tagName || 'div').toUpperCase(),
    classList: makeClassList(),
    style: {},
    dataset: {},
    hidden: false,
    disabled: false,
    checked: false,
    value: '',
    type: '',
    textContent: '',
    innerHTML: '',
    className: '',
    scrollHeight: 0,
    scrollTop: 0,
    clientHeight: 0,
    listeners: {},
    children,
    appendChild(child) {
      if (child) {
        child.parentNode = this;
        children.push(child);
      }
      return child;
    },
    removeChild(child) {
      const idx = children.indexOf(child);
      if (idx >= 0) children.splice(idx, 1);
      if (child) child.parentNode = null;
      return child;
    },
    replaceChildren(...nodes) {
      children.splice(0, children.length);
      nodes.forEach((child) => this.appendChild(child));
    },
    contains(target) {
      if (target === this) return true;
      return children.some((child) => child && typeof child.contains === 'function' && child.contains(target));
    },
    querySelector() { return null; },
    querySelectorAll(selector) {
      const results = [];
      const visit = (child) => {
        if (!child) return;
        if (selector === 'input[data-mcp-server]' && child.tagName === 'INPUT' && child.dataset && child.dataset.mcpServer) {
          results.push(child);
        }
        (child.children || []).forEach(visit);
      };
      children.forEach(visit);
      return results;
    },
    setAttribute(name, value) {
      attributes.set(name, String(value));
      this[name] = String(value);
      if (name === 'hidden') this.hidden = true;
      if (name === 'class') this.className = String(value);
    },
    removeAttribute(name) {
      attributes.delete(name);
      delete this[name];
      if (name === 'hidden') this.hidden = false;
    },
    hasAttribute(name) { return attributes.has(name); },
    addEventListener(type, handler) {
      if (!this.listeners[type]) this.listeners[type] = [];
      this.listeners[type].push(handler);
    },
    dispatchEvent(event) {
      const evt = event || { type: '' };
      if (!evt.target) evt.target = this;
      if (!evt.preventDefault) evt.preventDefault = () => { evt.defaultPrevented = true; };
      const handlers = this.listeners[evt.type] || [];
      handlers.forEach((handler) => handler(evt));
      return !evt.defaultPrevented;
    },
    focus() {},
    select() {},
    remove() {},
    click() { this.dispatchEvent({ type: 'click', target: this }); },
  };
  return node;
}

function parsedTestURL(url) {
  try {
    return new URL(String(url), 'https://example.test');
  } catch (_) {
    return null;
  }
}

function isTranscriptIndexURL(url, sessionId) {
  const parsed = parsedTestURL(url);
  return Boolean(parsed && parsed.pathname === `/ui/v1/sessions/${sessionId}/transcript`);
}

function isTranscriptBodiesURL(url, sessionId) {
  const parsed = parsedTestURL(url);
  return Boolean(parsed && parsed.pathname === `/ui/v1/sessions/${sessionId}/transcript/bodies`);
}

function startupTranscriptSideload({ rev, ids, messages, etag = `\"startup-${rev}\"`, activeResponseId = '', startedRev = 0 }) {
  return {
    index_etag: etag,
    index: {
      rev,
      compaction_seq: -1,
      compaction_count: 0,
      ...(activeResponseId ? { active_response_id: activeResponseId, started_rev: startedRev } : {}),
      rows: {
        ids,
        seqs: ids.map((_, index) => index),
        roles: 'u'.repeat(ids.length),
        flags: ids.map(() => 0),
      },
    },
    bodies: { rev, messages },
  };
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
      const currentId = String(session.activeResponseId || '').trim();
      if (currentId && currentId !== normalized) {
        app.clearProviderRetryStatus(String(session.id || '').trim(), currentId);
      }
      if (currentId !== normalized) {
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
      const retryOwnerId = targetId || currentId;
      if (retryOwnerId) {
        app.clearProviderRetryStatus(String(session.id || '').trim(), retryOwnerId);
      }
      if (!targetId || currentId === targetId || targetId.startsWith('resp_msg_')) {
        session.activeResponseId = null;
        session.lastSequenceNumber = 0;
      }
      if (
        !targetId
        || (
          app.state.currentStreamSessionId === String(session.id || '').trim()
          && (!app.state.currentStreamResponseId || app.state.currentStreamResponseId === targetId || targetId.startsWith('resp_msg_'))
        )
      ) {
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

  const elementMap = new Map();
  const getElement = (id) => {
    if (!elementMap.has(id)) elementMap.set(id, makeNode());
    return elementMap.get(id);
  };

  const document = {
    cookie: '',
    visibilityState: 'visible',
    body: { classList: makeClassList() },
    documentElement: { style: { setProperty() {} } },
    getElementById(id) { return getElement(id); },
    createElement(tagName) { return makeNode(tagName); },
    querySelector() { return makeNode(); },
    querySelectorAll() { return []; },
    listeners: {},
    addEventListener(type, handler) {
      if (!this.listeners[type]) this.listeners[type] = [];
      this.listeners[type].push(handler);
    },
  };

  const localStorage = {
    getItem(key) { return storage.has(key) ? storage.get(key) : null; },
    setItem(key, value) { storage.set(key, String(value)); },
    removeItem(key) { storage.delete(key); },
  };

  const timerSetTimeout = options.setTimeout || setTimeout;
  const timerClearTimeout = options.clearTimeout || clearTimeout;
  const timerSetInterval = options.setInterval || (() => 0);
  const timerClearInterval = options.clearInterval || (() => {});

  const windowObj = {
    TERM_LLM_UI_PREFIX: options.uiPrefix || '/ui',
    TERM_LLM_STARTUP_HYDRATION_TIMEOUT_MS: options.startupHydrationTimeoutMS,
    TERM_LLM_SIDEBAR_SESSIONS: 'all',
    TERM_LLM_HUB: options.hub || null,
    TERM_LLM_LOCATION_SHARING_ENABLED: options.locationSharingEnabled !== false,
    isSecureContext: options.isSecureContext !== false,
    location: { pathname: options.pathname || `${options.uiPrefix || '/ui'}/`, search: options.search || '', origin: 'https://example.test', protocol: 'https:', host: 'example.test' },
    history: {
      pushState(_state, _title, url) {
        const parsed = new URL(url, windowObj.location.origin);
        windowObj.location.pathname = parsed.pathname;
        windowObj.location.search = parsed.search;
      },
      replaceState(_state, _title, url) {
        const parsed = new URL(url, windowObj.location.origin);
        windowObj.location.pathname = parsed.pathname;
        windowObj.location.search = parsed.search;
      },
    },
    matchMedia() {
      return {
        matches: false,
        addEventListener() {},
        addListener() {},
        removeEventListener() {},
        removeListener() {},
      };
    },
    navigator: { standalone: false, mediaDevices: null, serviceWorker: null, geolocation: options.geolocation || null },
    visualViewport: null,
    listeners: {},
    addEventListener(type, handler) {
      if (!this.listeners[type]) this.listeners[type] = [];
      this.listeners[type].push(handler);
    },
    removeEventListener(type, handler) {
      const list = this.listeners[type] || [];
      const index = list.indexOf(handler);
      if (index >= 0) list.splice(index, 1);
    },
    requestAnimationFrame(callback) { return timerSetTimeout(callback, 0); },
    cancelAnimationFrame(handle) { timerClearTimeout(handle); },
    setTimeout: timerSetTimeout,
    clearTimeout: timerClearTimeout,
    setInterval: timerSetInterval,
    clearInterval: timerClearInterval,
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
    setTimeout: timerSetTimeout,
    clearTimeout: timerClearTimeout,
    setInterval: timerSetInterval,
    clearInterval: timerClearInterval,
    URL,
    URLSearchParams,
    CSS: { escape(value) { return String(value).replace(/"/g, '\\"'); } },
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
  vm.runInContext(transcriptStoreSource, context, { filename: 'transcript-store.js' });
  windowObj.TranscriptStore = context.TranscriptStore;
  windowObj.transcriptStoreFromMessages = context.transcriptStoreFromMessages;
  windowObj.reconcileToolCallProjection = context.reconcileToolCallProjection;
  windowObj.TRANSCRIPT_BUDGETS = context.TRANSCRIPT_BUDGETS;
  vm.runInContext(coreSource, context, { filename: 'app-core.js' });
  vm.runInContext(planSource, context, { filename: 'app-plan.js' });

  const app = windowObj.TermLLMApp;
  Object.assign(app, defaultAppStubs(app, options.appOverrides || {}));

  vm.runInContext(sessionsSource, context, { filename: 'app-sessions.js' });
  if (typeof options.onInitializeStarted === 'function') {
    options.onInitializeStarted({ app, windowObj, storage, elementMap });
  }
  await windowObj.__termllmInitializePromise;

  return { app, storage, windowObj, elementMap };
}

async function testSwitchingSessionsStagesCurrentComposerBeforeRestore() {
  const name = 'switching sessions stages current composer before restoring target draft';
  const drafts = new Map([['', 'existing blank draft']]);
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

async function testNewChatClearsExistingDraftComposer() {
  const name = 'new chat clears existing draft instead of restoring it';
  const drafts = new Map([['', 'old new-chat draft']]);
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

  app.state.activeSessionId = '';
  app.state.draftSessionActive = true;
  app.elements.promptInput.value = 'old new-chat draft';

  await app.switchToDraftSession({ clearComposer: true, focusPrompt: true });
  if (drafts.has('')) {
    fail(name, 'expected the new-chat draft bucket to be removed', JSON.stringify(Array.from(drafts.entries())));
    return;
  }
  if (app.elements.promptInput.value !== '') {
    fail(name, 'expected composer to stay empty after creating a fresh chat', app.elements.promptInput.value);
    return;
  }
  pass(name);
}

async function testNewChatFromSessionPreservesSessionDraft() {
  const name = 'new chat from a session preserves that session draft';
  const drafts = new Map([['', 'existing blank draft']]);
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

  const session = { id: 'sess_a', title: 'A', messages: [], lastResponseId: null, activeResponseId: null, lastSequenceNumber: 0 };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;
  app.elements.promptInput.value = 'unsent in A';

  await app.switchToDraftSession({ clearComposer: true, focusPrompt: true });
  if (drafts.get(session.id) !== 'unsent in A') {
    fail(name, 'expected active session composer to be staged before New Chat clears composer', JSON.stringify(Array.from(drafts.entries())));
    return;
  }
  if (app.elements.promptInput.value !== '') {
    fail(name, 'expected New Chat composer to be empty', app.elements.promptInput.value);
    return;
  }
  if (drafts.get('') !== 'existing blank draft') {
    fail(name, 'expected unrelated blank draft bucket to survive New Chat from a session', JSON.stringify(Array.from(drafts.entries())));
    return;
  }
  pass(name);
}

async function testArchivingActiveSessionClearsItsComposerDraft() {
  const name = 'archiving active hidden session clears its composer draft';
  const drafts = new Map([['', 'blank draft']]);
  const { app, windowObj } = await createSessionsHarness({
    fetchImpl: async (url, options = {}) => {
      if (parsedTestURL(url)?.pathname === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      if (url === '/ui/v1/sessions/sess_archive' && options.method === 'PATCH') {
        return new Response(JSON.stringify({
          id: 'sess_archive',
          short_title: 'Archive me',
          long_title: 'Archive me',
          archived: true,
          created_at: 1710000000000,
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' }
      });
    },
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

  const session = { id: 'sess_archive', title: 'Archive me', messages: [], archived: false, created: 1710000000000 };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;
  app.state.showHiddenSessions = false;
  app.elements.promptInput.value = 'discard me with archived session';
  let alertMessage = '';
  windowObj.alert = (message) => { alertMessage = String(message || ''); };

  const archived = await app.setSessionArchived(session, true);
  if (!archived) {
    fail(name, `setSessionArchived returned false: ${alertMessage}`);
    return;
  }
  if (drafts.has(session.id)) {
    fail(name, 'archived active session should not leave an orphan draft', JSON.stringify(Array.from(drafts.entries())));
    return;
  }
  if (drafts.get('') !== 'blank draft') {
    fail(name, 'blank draft bucket should remain available after archiving active session', JSON.stringify(Array.from(drafts.entries())));
    return;
  }
  if (app.elements.promptInput.value !== 'blank draft') {
    fail(name, 'expected blank draft to be restored after archiving active session', app.elements.promptInput.value);
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
      term_llm_selected_reasoning_mode: 'standard',
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
    activeReasoningMode: 'pro',
    lastResponseId: 'resp_msg_1',
    activeResponseId: null,
    lastSequenceNumber: 0,
    runtimeSelectionIntent: true,
  };
  app.state.sessions = [session];
  app.state.selectedProvider = 'chatgpt';
  app.state.selectedModel = 'gpt-5.4';
  app.state.selectedEffort = 'xhigh';
  app.state.selectedReasoningMode = 'standard';

  await app.switchToSession(session.id, { sync: false });

  if (session.runtimeSelectionIntent) {
    fail(name, 'switching back to a session retained stale runtime-selection intent');
    return;
  }
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
  if (app.state.selectedReasoningMode !== 'pro') {
    fail(name, `selectedReasoningMode = ${JSON.stringify(app.state.selectedReasoningMode)}, want pro`);
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
  if (storage.get('term_llm_selected_reasoning_mode') !== 'pro') {
    fail(name, 'expected selected reasoning mode to be persisted', storage.get('term_llm_selected_reasoning_mode'));
    return;
  }
  pass(name);
}

async function testNumericDeepLinkResolvesRealSessionId() {
  const name = 'numeric deep link resolves server session id';
  const fetchCalls = [];
  let appliedDiffSummary = null;
  const { app, storage, elementMap } = await createSessionsHarness({
    pathname: '/ui/1291',
    appOverrides: {
      applySessionDiffSummary(sessionId, summary) {
        appliedDiffSummary = { sessionId, summary };
        return true;
      },
    },
    fetchImpl: async (url) => {
      fetchCalls.push(url);
      const parsed = parsedTestURL(url);
      if (parsed?.pathname === '/ui/v1/sessions' && parsed.searchParams.get('selected_session') === '1291') {
        return new Response(JSON.stringify({
          sessions: [],
          selected_session: {
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
            file_change_summary: { file_count: 2, adds: 7, dels: 3 },
            plan_summary: { version: 4, step_count: 3, completed_steps: 1, position: 2, state: 'in_progress' },
          }
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isTranscriptIndexURL(url, 'sess_real')) {
        return new Response(JSON.stringify({
          rev: 1,
          compaction_seq: -1,
          compaction_count: 0,
          rows: { ids: [11], seqs: [0], roles: 'u', flags: [0] }
        }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"rev-1"' } });
      }
      if (isTranscriptBodiesURL(url, 'sess_real')) {
        return new Response(JSON.stringify({
          rev: 1,
          messages: [{ id: 11, sequence: 0, role: 'user', created_at: 1710000000000, parts: [{ type: 'text', text: 'hello from server' }] }]
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
  if (!fetchCalls.some((url) => isTranscriptIndexURL(url, 'sess_real'))) {
    fail(name, 'should load transcript index via resolved server session id', JSON.stringify(fetchCalls));
    return;
  }
  const unresolvedScopedCalls = fetchCalls.filter((url) => {
    const parsed = parsedTestURL(url);
    return Boolean(parsed && (
      parsed.pathname.includes('/v1/sessions/1291/')
      || parsed.pathname.includes('/api/sessions/1291/')
    ));
  });
  if (unresolvedScopedCalls.length > 0) {
    fail(name, 'numeric route stub reached a session-scoped endpoint before identity reconciliation', JSON.stringify(fetchCalls));
    return;
  }
  if (!appliedDiffSummary
    || appliedDiffSummary.sessionId !== 'sess_real'
    || appliedDiffSummary.summary.file_count !== 2
    || appliedDiffSummary.summary.adds !== 7
    || appliedDiffSummary.summary.dels !== 3) {
    fail(name, 'selected diff aggregate was not applied during session merge', JSON.stringify(appliedDiffSummary));
    return;
  }
  const planToggle = elementMap.get('planToggleBtn');
  if (planToggle.hidden
    || elementMap.get('planToggleWord').textContent !== 'Plan'
    || elementMap.get('planToggleProgress').textContent !== '2/3') {
    fail(name, 'selected plan summary did not render the initial affordance');
    return;
  }
  if (fetchCalls.some((url) => String(url).includes('/file-changes'))) {
    fail(name, 'startup must not fetch the full file-change list', JSON.stringify(fetchCalls));
    return;
  }
  const sessionsIndex = fetchCalls.findIndex((url) => {
    const parsed = parsedTestURL(url);
    return parsed?.pathname === '/ui/v1/sessions' && parsed.searchParams.get('selected_session') === '1291';
  });
  const transcriptIndex = fetchCalls.findIndex((url) => isTranscriptIndexURL(url, 'sess_real'));
  if (sessionsIndex < 0 || transcriptIndex <= sessionsIndex) {
    fail(name, 'canonical transcript request did not wait for session identity lookup', JSON.stringify(fetchCalls));
    return;
  }
  if (fetchCalls.some((url) => isSessionMessagesURL(url, 'pending_1291'))) {
    fail(name, 'should not use pending_ prefix in session id', JSON.stringify(fetchCalls));
    return;
  }
  if ([...storage.values()].some((value) => String(value).includes('hello from server'))) {
    fail(name, 'durable transcript bodies must not be written to localStorage', JSON.stringify([...storage.entries()]));
    return;
  }
  pass(name);
}

async function testIdleStartupSchedulesNormalPollWithoutImmediateRequest() {
  const name = 'idle startup schedules normal poll delay without immediate status request';
  const scheduled = [];
  let statusRequests = 0;
  const { app } = await createSessionsHarness({
    setTimeout(fn, delay) {
      const handle = { fn, delay, cleared: false };
      scheduled.push(handle);
      return handle;
    },
    clearTimeout(handle) { if (handle) handle.cleared = true; },
    fetchImpl: async (url) => {
      const parsed = parsedTestURL(url);
      if (parsed?.pathname === '/ui/v1/sessions/status') statusRequests += 1;
      if (parsed?.pathname === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [], selected_session: null, widget_status: { widgets: [] } }), {
          status: 200, headers: { 'Content-Type': 'application/json' },
        });
      }
      return new Response(JSON.stringify([]), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
  });
  const pollTimers = scheduled.filter((handle) => !handle.cleared && handle.delay >= 2000);
  if (statusRequests !== 0 || pollTimers.length !== 1 || pollTimers[0].delay !== 30000) {
    fail(name, 'idle startup did not arm exactly one normal idle timer', JSON.stringify({ statusRequests, timers: pollTimers.map(({ delay }) => delay) }));
    return;
  }
  app.stopSidebarStatusPoll();
  pass(name);
}

async function testStartupTranscriptSideloadRendersBoundedFirstPaintWithoutTranscriptRequests() {
  const name = 'startup transcript sideload renders bounded first paint with zero transcript requests';
  const ids = Array.from({ length: 20 }, (_, index) => index + 1);
  const authoritativeMessageCount = 4;
  const messages = ids.slice(-9).map((id) => ({
    id,
    sequence: id - 1,
    role: 'user',
    created_at: id * 1000,
    parts: [{ type: 'text', text: `turn-${id}` }],
  }));
  const fetchCalls = [];
  const events = [];
  const scheduled = [];
  let appRef = null;
  let releaseState;
  let releaseModels;
  let markStateStarted;
  let markModelsStarted;
  let markSideloadRendered;
  let stateResolved = false;
  let modelsResolved = false;
  const stateGate = new Promise((resolve) => { releaseState = resolve; });
  const modelsGate = new Promise((resolve) => { releaseModels = resolve; });
  const stateStarted = new Promise((resolve) => { markStateStarted = resolve; });
  const modelsStarted = new Promise((resolve) => { markModelsStarted = resolve; });
  const sideloadRendered = new Promise((resolve) => { markSideloadRendered = resolve; });
  const harnessPromise = createSessionsHarness({
    pathname: '/ui/3347',
    setTimeout(fn, delay) {
      const handle = { fn, delay, cleared: false };
      scheduled.push(handle);
      return handle;
    },
    clearTimeout(handle) { if (handle) handle.cleared = true; },
    fetchImpl: async (url) => {
      fetchCalls.push(String(url));
      const parsed = parsedTestURL(url);
      if (parsed?.pathname === '/ui/v1/sessions') {
        return new Response(JSON.stringify({
          sessions: [],
          selected_session: {
            id: 'sess_3347', number: 3347, short_title: 'Sideloaded', long_title: 'Sideloaded',
            mode: 'chat', origin: 'web', created_at: 1000, message_count: authoritativeMessageCount, transcript_rev: 20,
          },
          selected_transcript: startupTranscriptSideload({ rev: 20, ids, messages }),
          widget_status: { widgets: [] },
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed?.pathname === '/ui/v1/sessions/sess_3347/state') {
        markStateStarted();
        await stateGate;
        stateResolved = true;
        return new Response(JSON.stringify({ active_run: false, transcript_rev: 20 }), {
          status: 200, headers: { 'Content-Type': 'application/json' },
        });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
    appOverrides: {
      async fetchModels() {
        markModelsStarted();
        await modelsGate;
        modelsResolved = true;
        return [];
      },
      renderMessages() {
        const session = appRef?.state.sessions.find((item) => item.id === 'sess_3347');
        const contents = session?.messages.map((message) => message.content) || [];
        events.push({ type: 'render', contents });
        if (contents.filter((content) => typeof content === 'string').length === 9) markSideloadRendered();
      },
      hideStartupSplash() { events.push({ type: 'splash', stateResolved, modelsResolved }); },
    },
    onInitializeStarted({ app: startedApp }) { appRef = startedApp; },
  });

  await Promise.all([sideloadRendered, stateStarted, modelsStarted]);
  await new Promise((resolve) => setImmediate(resolve));
  const splashBeforeState = events.some((event) => event.type === 'splash' && !event.stateResolved && !event.modelsResolved);
  releaseState();
  releaseModels();
  const { app } = await harnessPromise;

  const startupURL = fetchCalls.find((url) => parsedTestURL(url)?.pathname === '/ui/v1/sessions');
  if (parsedTestURL(startupURL)?.searchParams.get('include_transcript') !== '1') {
    fail(name, 'selected startup request did not opt into transcript sideload', startupURL);
    return;
  }
  const transcriptRequests = fetchCalls.filter((url) => isTranscriptIndexURL(url, 'sess_3347') || isTranscriptBodiesURL(url, 'sess_3347'));
  if (transcriptRequests.length !== 0) {
    fail(name, 'valid sideload issued transcript endpoint requests', JSON.stringify(transcriptRequests));
    return;
  }
  const session = app.state.sessions.find((item) => item.id === 'sess_3347');
  const hydratedRender = events.findIndex((event) => event.type === 'render'
    && event.contents.filter((content) => typeof content === 'string').length === 9);
  const splash = events.findIndex((event) => event.type === 'splash');
  if (!splashBeforeState) {
    fail(name, 'splash remained coupled to delayed runtime state or models after the sideload render', JSON.stringify(events));
    return;
  }
  if (!session || hydratedRender < 0 || splash <= hydratedRender) {
    fail(name, 'bounded sideload was not rendered atomically before splash hide', JSON.stringify(events));
    return;
  }
  if (session.transcript.rev !== 20 || session.transcript.bodies.size !== 9
      || session.transcript.segments.filter((segment) => segment.state === 'materialized').length !== 9) {
    fail(name, 'sideload did not preserve the initial viewport budget', JSON.stringify({ rev: session.transcript.rev, bodies: session.transcript.bodies.size, segments: session.transcript.segments }));
    return;
  }
  if (session.messageCount !== authoritativeMessageCount || session.transcript.ids.length !== ids.length) {
    fail(name, 'sideload row application changed the authoritative server message count', JSON.stringify({ messageCount: session.messageCount, transcriptRows: session.transcript.ids.length }));
    return;
  }
  const statusRequests = fetchCalls.filter((url) => parsedTestURL(url)?.pathname === '/ui/v1/sessions/status');
  const liveTimers = scheduled.filter((handle) => !handle.cleared && handle.delay >= 2000);
  if (statusRequests.length !== 0 || liveTimers.length !== 1 || liveTimers[0].delay !== 5000) {
    fail(name, 'startup did not arm the normal visible-active-session poll delay', JSON.stringify({ statusRequests, liveTimers: liveTimers.map(({ delay }) => delay) }));
    return;
  }
  app.stopSidebarStatusPoll();
  pass(name);
}

async function testSelectedTranscriptSideloadReconcilesStableToolCallsAcrossSwitchBack() {
  const name = 'selected transcript sideload reconciles stable tool calls across switch away and back';
  const { app, windowObj } = await createSessionsHarness({
    fetchImpl: async () => new Response(JSON.stringify({ sessions: [] }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' }
    })
  });
  const session = {
    id: 'sess_tool_overlap',
    number: 956,
    title: 'Tool overlap',
    messages: [],
    messageCount: 2,
    activeResponseId: 'resp_tool_overlap',
    lastSequenceNumber: 9
  };
  app.state.sessions.push(session);
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;

  const transcript = new windowObj.TranscriptStore(session.id);
  session.transcript = transcript;
  transcript.addOptimistic({
    id: 'msg_441406d5-live-tools',
    role: 'tool-group',
    status: 'running',
    created: 1200,
    tools: [
      { id: 'call_GwHMMHXf28QA81Zfm9vgRMap', name: 'queue_agent', status: 'done' },
      { id: 'call_XX3ODxDyeoIxc7ZUZwJm19qu', name: 'wait_for_jobs', status: 'done' },
      { id: 'call_newer_shell', name: 'shell', status: 'running' }
    ]
  }, 7);
  const sideload = {
    index_etag: '"tool-overlap-7"',
    index: {
      rev: 7,
      compaction_seq: -1,
      compaction_count: 0,
      active_response_id: 'resp_tool_overlap',
      started_rev: 6,
      rows: {
        ids: [381, 382],
        seqs: [381, 382],
        roles: 'ua',
        flags: [0, 0]
      }
    },
    bodies: {
      rev: 7,
      messages: [
        { id: 381, sequence: 381, role: 'user', created_at: 1000, parts: [{ type: 'text', text: 'continue' }] },
        {
          id: 382,
          sequence: 382,
          role: 'assistant',
          created_at: 1100,
          parts: [
            { type: 'tool_call', tool_call_id: 'call_GwHMMHXf28QA81Zfm9vgRMap', tool_name: 'queue_agent', tool_arguments: '{}' },
            { type: 'tool_call', tool_call_id: 'call_XX3ODxDyeoIxc7ZUZwJm19qu', tool_name: 'wait_for_jobs', tool_arguments: '{}' }
          ]
        }
      ]
    }
  };

  const assertUniqueProjection = (phase) => {
    const groups = session.messages.filter((message) => message.role === 'tool-group');
    const callIDs = groups.flatMap((group) => group.tools.map((tool) => tool.id));
    const unique = new Set(callIDs);
    if (unique.size !== callIDs.length) {
      fail(name, `${phase} retained duplicate call IDs`, JSON.stringify(session.messages));
      return false;
    }
    if (groups.map((group) => group.id).join(',') !== 'srv_seq_382_tools_0,msg_441406d5-live-tools'
        || groups[1].tools.map((tool) => tool.id).join(',') !== 'call_newer_shell') {
      fail(name, `${phase} lost durable authority or newer optimistic order`, JSON.stringify(groups));
      return false;
    }
    return true;
  };

  if (!app.applySelectedTranscriptSideload(session, sideload) || !assertUniqueProjection('initial sideload')) return;

  // Reproduce switching away: mounted bodies and the projected message array are
  // released, while the optimistic transcript tail remains available for resume.
  session.transcript.releaseBodies();
  session.messages = [];
  if (!app.applySelectedTranscriptSideload(session, sideload) || !assertUniqueProjection('switch-back sideload')) return;

  pass(name);
}

async function testMalformedStartupTranscriptSideloadFallsBack() {
  const name = 'malformed startup transcript sideload falls back to index and bodies';
  let indexRequests = 0;
  let bodyRequests = 0;
  const { app } = await createSessionsHarness({
    pathname: '/ui/41',
    fetchImpl: async (url) => {
      const parsed = parsedTestURL(url);
      if (parsed?.pathname === '/ui/v1/sessions') {
        const malformed = startupTranscriptSideload({
          rev: 2,
          ids: [1, 2],
          messages: [{ id: 1, sequence: 0, role: 'user', parts: [{ type: 'text', text: 'fallback' }] }],
        });
        malformed.bodies.rev = 1;
        return new Response(JSON.stringify({
          sessions: [],
          selected_session: { id: 'sess_41', number: 41, short_title: 'Fallback', origin: 'web', created_at: 1, transcript_rev: 2 },
          selected_transcript: malformed,
          widget_status: { widgets: [] },
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isTranscriptIndexURL(url, 'sess_41')) {
        indexRequests += 1;
        return new Response(JSON.stringify({ rev: 2, compaction_seq: -1, compaction_count: 0, rows: { ids: [1], seqs: [0], roles: 'u', flags: [0] } }), {
          status: 200, headers: { 'Content-Type': 'application/json', ETag: '"fallback-2"' },
        });
      }
      if (isTranscriptBodiesURL(url, 'sess_41')) {
        bodyRequests += 1;
        return new Response(JSON.stringify({ rev: 2, messages: [{ id: 1, sequence: 0, role: 'user', parts: [{ type: 'text', text: 'fallback' }] }] }), {
          status: 200, headers: { 'Content-Type': 'application/json' },
        });
      }
      if (parsed?.pathname === '/ui/v1/sessions/sess_41/state') {
        return new Response(JSON.stringify({ active_run: false, transcript_rev: 2 }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
  });
  app.stopSidebarStatusPoll();
  const session = app.state.sessions.find((item) => item.id === 'sess_41');
  if (indexRequests !== 1 || bodyRequests !== 1 || session?.messages[0]?.content !== 'fallback') {
    fail(name, 'fallback did not use the existing activation path', JSON.stringify({ indexRequests, bodyRequests, messages: session?.messages }));
    return;
  }
  pass(name);
}

async function testStartupTranscriptSideloadRevisionRaceFetchesOnlyNewerRevision() {
  const name = 'startup transcript sideload refreshes only when concurrent state revision is newer';
  let indexRequests = 0;
  let bodyRequests = 0;
  const { app } = await createSessionsHarness({
    pathname: '/ui/52',
    fetchImpl: async (url) => {
      const parsed = parsedTestURL(url);
      if (parsed?.pathname === '/ui/v1/sessions') {
        return new Response(JSON.stringify({
          sessions: [],
          selected_session: { id: 'sess_52', number: 52, short_title: 'Race', origin: 'web', created_at: 1, transcript_rev: 2 },
          selected_transcript: startupTranscriptSideload({
            rev: 2,
            ids: [1, 2],
            messages: [
              { id: 1, sequence: 0, role: 'user', parts: [{ type: 'text', text: 'one' }] },
              { id: 2, sequence: 1, role: 'user', parts: [{ type: 'text', text: 'two' }] },
            ],
          }),
          widget_status: { widgets: [] },
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed?.pathname === '/ui/v1/sessions/sess_52/state') {
        return new Response(JSON.stringify({ active_run: false, transcript_rev: 3 }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isTranscriptIndexURL(url, 'sess_52')) {
        indexRequests += 1;
        return new Response(JSON.stringify({ rev: 3, compaction_seq: -1, compaction_count: 0, rows: { ids: [1, 2, 3], seqs: [0, 1, 2], roles: 'uuu', flags: [0, 0, 0] } }), {
          status: 200, headers: { 'Content-Type': 'application/json', ETag: '"race-3"' },
        });
      }
      if (isTranscriptBodiesURL(url, 'sess_52')) {
        bodyRequests += 1;
        return new Response(JSON.stringify({
          rev: 3,
          messages: [1, 2, 3].map((id) => ({ id, sequence: id - 1, role: 'user', parts: [{ type: 'text', text: String(id) }] })),
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
  });
  await new Promise((resolve) => setImmediate(resolve));
  app.stopSidebarStatusPoll();
  const session = app.state.sessions.find((item) => item.id === 'sess_52');
  if (indexRequests !== 1 || bodyRequests !== 1 || session?.transcript.rev !== 3 || session?.messages.at(-1)?.content !== '3') {
    fail(name, 'newer state revision was lost or fetched redundantly', JSON.stringify({ indexRequests, bodyRequests, rev: session?.transcript.rev, messages: session?.messages }));
    return;
  }
  pass(name);
}

async function testStartupActiveSideloadAvoidsDuplicateStateAfterScheduledStatus() {
  const name = 'active startup reveals rendered sideload before state and resumes without duplicate state';
  const scheduled = [];
  const events = [];
  let stateRequests = 0;
  let statusRequests = 0;
  let stateResolved = false;
  let appRef = null;
  let releaseState;
  let markStateStarted;
  let markSideloadRendered;
  const stateGate = new Promise((resolve) => { releaseState = resolve; });
  const stateStarted = new Promise((resolve) => { markStateStarted = resolve; });
  const sideloadRendered = new Promise((resolve) => { markSideloadRendered = resolve; });
  const harnessPromise = createSessionsHarness({
    pathname: '/ui/63',
    setTimeout(fn, delay) {
      const handle = { fn, delay, cleared: false };
      scheduled.push(handle);
      return handle;
    },
    clearTimeout(handle) { if (handle) handle.cleared = true; },
    fetchImpl: async (url) => {
      const parsed = parsedTestURL(url);
      if (parsed?.pathname === '/ui/v1/sessions') {
        return new Response(JSON.stringify({
          sessions: [],
          selected_session: { id: 'sess_63', number: 63, short_title: 'Active', origin: 'web', created_at: 1, transcript_rev: 5 },
          selected_transcript: startupTranscriptSideload({
            rev: 5,
            ids: [1],
            messages: [{ id: 1, sequence: 0, role: 'user', parts: [{ type: 'text', text: 'active' }] }],
            activeResponseId: 'resp_63',
            startedRev: 5,
          }),
          widget_status: { widgets: [] },
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed?.pathname === '/ui/v1/sessions/sess_63/state') {
        stateRequests += 1;
        markStateStarted();
        await stateGate;
        stateResolved = true;
        return new Response(JSON.stringify({ active_run: true, active_response_id: 'resp_63', started_rev: 5, transcript_rev: 6 }), {
          status: 200, headers: { 'Content-Type': 'application/json' },
        });
      }
      if (parsed?.pathname === '/ui/v1/sessions/status') {
        statusRequests += 1;
        return new Response(JSON.stringify({ sessions: [{ id: 'sess_63', active_response_id: 'resp_63', started_rev: 5, transcript_rev: 6 }] }), {
          status: 200, headers: { 'Content-Type': 'application/json' },
        });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
    appOverrides: {
      renderMessages() {
        const contents = appRef?.state.sessions.find((item) => item.id === 'sess_63')?.messages
          .map((message) => message.content) || [];
        events.push({ type: 'render', contents, stateResolved });
        if (contents.includes('active')) markSideloadRendered();
      },
      hideStartupSplash() { events.push({ type: 'splash', stateResolved }); },
      async resumeActiveResponse(session, options) {
        events.push({ type: 'resume', sessionId: session.id, responseId: options?.responseId, stateResolved });
      },
    },
    onInitializeStarted({ app: startedApp }) { appRef = startedApp; },
  });

  await Promise.all([sideloadRendered, stateStarted]);
  await new Promise((resolve) => setImmediate(resolve));
  const splashBeforeState = events.some((event) => event.type === 'splash' && !event.stateResolved);
  releaseState();
  const { app } = await harnessPromise;
  await new Promise((resolve) => setImmediate(resolve));

  if (!splashBeforeState) {
    fail(name, 'active transcript render remained blocked on runtime state', JSON.stringify(events));
    return;
  }
  const resumeEvent = events.find((event) => event.type === 'resume');
  if (!resumeEvent || resumeEvent.sessionId !== 'sess_63' || resumeEvent.responseId !== 'resp_63' || !resumeEvent.stateResolved) {
    fail(name, 'delayed runtime state did not resume the active response after reveal', JSON.stringify(events));
    return;
  }
  if (stateRequests !== 1 || statusRequests !== 0) {
    fail(name, 'startup issued immediate status or duplicate state request', JSON.stringify({ stateRequests, statusRequests }));
    return;
  }
  const firstPoll = scheduled.find((handle) => !handle.cleared && handle.delay === 2000);
  if (!firstPoll) {
    fail(name, 'active startup did not arm normal active poll delay', JSON.stringify(scheduled.map(({ delay, cleared }) => ({ delay, cleared }))));
    return;
  }
  firstPoll.cleared = true;
  await firstPoll.fn();
  await Promise.resolve();
  if (statusRequests !== 1 || stateRequests !== 1) {
    fail(name, 'status reconciliation duplicated selected-session state', JSON.stringify({ stateRequests, statusRequests }));
    return;
  }
  app.stopSidebarStatusPoll();
  pass(name);
}

async function testSidebarSwitchAppliesSelectedTranscriptBeforeAsyncStateAndSkills() {
  const name = 'sidebar switch applies one selected transcript payload before asynchronous state and skills';
  const fetchCalls = [];
  const events = [];
  let appRef = null;
  let releaseState;
  let releaseSkills;
  let markStateStarted;
  let markSkillsStarted;
  const stateGate = new Promise((resolve) => { releaseState = resolve; });
  const skillsGate = new Promise((resolve) => { releaseSkills = resolve; });
  const stateStarted = new Promise((resolve) => { markStateStarted = resolve; });
  const skillsStarted = new Promise((resolve) => { markSkillsStarted = resolve; });
  let appliedDiff = null;
  let appliedPlan = null;
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      fetchCalls.push(String(url));
      const parsed = parsedTestURL(url);
      if (parsed?.pathname === '/ui/v1/sessions' && parsed.searchParams.get('selected_session') === 'sess_switch') {
        events.push('selected');
        return new Response(JSON.stringify({
          sessions: [],
          selected_session: {
            id: 'sess_switch', short_title: 'Switch target', origin: 'web', created_at: 2,
            message_count: 1, transcript_rev: 4,
            file_change_summary: { file_count: 2, adds: 7, dels: 3 },
            plan_summary: { completed: 2, total: 3 },
          },
          selected_transcript: startupTranscriptSideload({
            rev: 4,
            ids: [41],
            messages: [{ id: 41, sequence: 0, role: 'user', parts: [{ type: 'text', text: 'sideloaded switch' }] }],
          }),
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed?.pathname === '/ui/v1/sessions/sess_switch/state') {
        events.push('state');
        markStateStarted();
        await stateGate;
        return new Response(JSON.stringify({ active_run: false, transcript_rev: 4 }), {
          status: 200, headers: { 'Content-Type': 'application/json' },
        });
      }
      if (isTranscriptIndexURL(url, 'sess_switch')) {
        events.push('transcript');
        return new Response(JSON.stringify({
          rev: 4, compaction_seq: -1, compaction_count: 0,
          rows: { ids: [41], seqs: [0], roles: 'u', flags: [0] },
        }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"legacy-switch-4"' } });
      }
      if (isTranscriptBodiesURL(url, 'sess_switch')) {
        events.push('bodies');
        return new Response(JSON.stringify({
          rev: 4,
          messages: [{ id: 41, sequence: 0, role: 'user', parts: [{ type: 'text', text: 'legacy switch' }] }],
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed?.pathname === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({}), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
    appOverrides: {
      renderMessages() {
        const visible = appRef?.state.sessions.find((item) => item.id === appRef.state.activeSessionId);
        if (visible?.messages.some((message) => message.content === 'sideloaded switch')) events.push('render');
      },
      applySessionDiffSummary(sessionId, summary) { appliedDiff = { sessionId, summary }; },
      applyCurrentPlanSummary(sessionId, summary) { appliedPlan = { sessionId, summary }; },
      async refreshSkillCommands(sessionId) {
        if (sessionId !== 'sess_switch') return [];
        events.push('skills');
        markSkillsStarted();
        await skillsGate;
        return [];
      },
    },
    onInitializeStarted({ app: startedApp }) { appRef = startedApp; },
  });
  app.stopSidebarStatusPoll();
  const previous = { id: 'sess_previous', title: 'Previous', messages: [], created: 1 };
  const target = { id: 'sess_switch', title: 'Switch target', messages: [], created: 2, _serverOnly: true };
  app.state.sessions = [previous, target];
  app.state.activeSessionId = previous.id;
  app.state.draftSessionActive = false;
  fetchCalls.length = 0;
  events.length = 0;

  const switching = app.switchToSession(target.id, { closeSidebar: false });
  await stateStarted;
  const completedBeforeState = await Promise.race([
    switching.then(() => true),
    new Promise((resolve) => setImmediate(() => resolve(false))),
  ]);
  releaseState();
  await skillsStarted;
  const completedBeforeSkills = await Promise.race([
    switching.then(() => true),
    new Promise((resolve) => setImmediate(() => resolve(false))),
  ]);
  releaseSkills();
  await switching;
  const completedBeforeStateAndSkills = completedBeforeState && completedBeforeSkills;

  const selectedRequests = fetchCalls.filter((url) => {
    const parsed = parsedTestURL(url);
    return parsed?.pathname === '/ui/v1/sessions' && parsed.searchParams.get('selected_session') === target.id;
  });
  const selectedURL = parsedTestURL(selectedRequests[0]);
  const transcriptRequests = fetchCalls.filter((url) => isTranscriptIndexURL(url, target.id) || isTranscriptBodiesURL(url, target.id));
  if (selectedRequests.length !== 1
      || selectedURL?.searchParams.get('selected_only') !== '1'
      || selectedURL?.searchParams.get('include_transcript') !== '1'
      || transcriptRequests.length !== 0) {
    fail(name, 'switch did not use exactly one selected-only transcript payload', JSON.stringify(fetchCalls));
    return;
  }
  if (!completedBeforeStateAndSkills || events.indexOf('render') < 0
      || events.indexOf('render') > events.indexOf('state') || events.indexOf('render') > events.indexOf('skills')) {
    fail(name, 'visible transcript remained coupled to runtime state or skills', JSON.stringify({ completedBeforeStateAndSkills, events }));
    return;
  }
  if (target.messages[0]?.content !== 'sideloaded switch' || target.transcript?.rev !== 4) {
    fail(name, 'selected transcript was not applied to the canonical target', JSON.stringify(target.messages));
    return;
  }
  if (appliedDiff?.sessionId !== target.id || appliedDiff.summary?.file_count !== 2
      || appliedPlan?.sessionId !== target.id || appliedPlan.summary?.completed !== 2) {
    fail(name, 'selected diff or plan summary was lost', JSON.stringify({ appliedDiff, appliedPlan }));
    return;
  }
  pass(name);
}

async function testRapidSidebarSwitchRejectsStaleSelectedTranscriptAndState() {
  const name = 'rapid sidebar switch rejects stale selected transcript and runtime state';
  const fetchCalls = [];
  const stateSessions = [];
  const skillSessions = [];
  let releaseA;
  let markAStarted;
  const aGate = new Promise((resolve) => { releaseA = resolve; });
  const aStarted = new Promise((resolve) => { markAStarted = resolve; });
  const selectedPayload = (id, rev, text) => ({
    sessions: [],
    selected_session: { id, short_title: id, origin: 'web', created_at: rev, message_count: 1, transcript_rev: rev },
    selected_transcript: startupTranscriptSideload({
      rev,
      ids: [rev],
      messages: [{ id: rev, sequence: 0, role: 'user', parts: [{ type: 'text', text }] }],
    }),
  });
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      fetchCalls.push(String(url));
      const parsed = parsedTestURL(url);
      if (parsed?.pathname === '/ui/v1/sessions' && parsed.searchParams.get('selected_session') === 'sess_a') {
        markAStarted();
        await aGate;
        return new Response(JSON.stringify(selectedPayload('sess_a', 1, 'stale A')), {
          status: 200, headers: { 'Content-Type': 'application/json' },
        });
      }
      if (parsed?.pathname === '/ui/v1/sessions' && parsed.searchParams.get('selected_session') === 'sess_b') {
        return new Response(JSON.stringify(selectedPayload('sess_b', 2, 'current B')), {
          status: 200, headers: { 'Content-Type': 'application/json' },
        });
      }
      if (parsed?.pathname === '/ui/v1/sessions/sess_a/state' || parsed?.pathname === '/ui/v1/sessions/sess_b/state') {
        const id = parsed.pathname.includes('sess_a') ? 'sess_a' : 'sess_b';
        stateSessions.push(id);
        return new Response(JSON.stringify({ active_run: false, transcript_rev: id === 'sess_a' ? 1 : 2 }), {
          status: 200, headers: { 'Content-Type': 'application/json' },
        });
      }
      if (parsed?.pathname === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({}), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
    appOverrides: {
      async refreshSkillCommands(sessionId) {
        skillSessions.push(String(sessionId || ''));
        return [];
      },
    },
  });
  app.stopSidebarStatusPoll();
  const previous = { id: 'sess_previous', title: 'Previous', messages: [], created: 0 };
  const sessionA = { id: 'sess_a', title: 'A', messages: [], created: 1, _serverOnly: true };
  const sessionB = { id: 'sess_b', title: 'B', messages: [], created: 2, _serverOnly: true };
  app.state.sessions = [previous, sessionA, sessionB];
  app.state.activeSessionId = previous.id;
  app.state.draftSessionActive = false;
  fetchCalls.length = 0;
  skillSessions.length = 0;

  const switchA = app.switchToSession(sessionA.id, { closeSidebar: false });
  await aStarted;
  const switchB = app.switchToSession(sessionB.id, { closeSidebar: false });
  await switchB;
  await waitFor(() => stateSessions.includes('sess_b') && skillSessions.includes('sess_b'), 'current switch hydration did not start');
  releaseA();
  await switchA;
  await new Promise((resolve) => setImmediate(resolve));

  if (app.state.activeSessionId !== sessionB.id || sessionB.messages[0]?.content !== 'current B') {
    fail(name, 'current B sideload did not remain selected', JSON.stringify({ active: app.state.activeSessionId, messages: sessionB.messages }));
    return;
  }
  if (sessionA.messages.some((message) => message.content === 'stale A') || Number(sessionA.transcript?.rev || 0) !== 0) {
    fail(name, 'stale A sideload mutated its transcript after B won', JSON.stringify({ messages: sessionA.messages, rev: sessionA.transcript?.rev }));
    return;
  }
  if (stateSessions.includes('sess_a') || skillSessions.includes('sess_a')) {
    fail(name, 'stale A runtime hydration started after B won', JSON.stringify({ stateSessions, skillSessions }));
    return;
  }
  const transcriptRequests = fetchCalls.filter((url) => /\/transcript(?:\/bodies)?(?:\?|$)/.test(String(url)));
  if (transcriptRequests.length !== 0) {
    fail(name, 'valid rapid-switch sideloads used transcript endpoints', JSON.stringify(transcriptRequests));
    return;
  }
  pass(name);
}

async function testRapidSidebarSwitchRejectsLateRuntimeResponse() {
  const name = 'rapid sidebar switch rejects late runtime response from the previous selection';
  let releaseAState;
  let markAStateStarted;
  const aStateGate = new Promise((resolve) => { releaseAState = resolve; });
  const aStateStarted = new Promise((resolve) => { markAStateStarted = resolve; });
  const openedPrompts = [];
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      const parsed = parsedTestURL(url);
      if (parsed?.pathname === '/ui/v1/sessions/sess_runtime_a/state') {
        markAStateStarted();
        await aStateGate;
        return new Response(JSON.stringify({
          active_run: false,
          provider: 'stale-provider',
          model: 'stale-model',
          transcript_rev: 0,
          pending_ask_user: { call_id: 'stale-call', questions: [{ question: 'stale?' }] },
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed?.pathname === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ active_run: false, transcript_rev: 0 }), {
        status: 200, headers: { 'Content-Type': 'application/json' },
      });
    },
    appOverrides: {
      openAskUserModal(sessionId) { openedPrompts.push(sessionId); },
      async refreshSkillCommands() { return []; },
    },
  });
  app.stopSidebarStatusPoll();
  const previous = { id: 'previous', messages: [], created: 0 };
  const sessionA = { id: 'sess_runtime_a', messages: [{ id: 'a', role: 'user', content: 'A' }], provider: 'provider-a', created: 1 };
  const sessionB = { id: 'sess_runtime_b', messages: [{ id: 'b', role: 'user', content: 'B' }], provider: 'provider-b', created: 2 };
  app.state.sessions = [previous, sessionA, sessionB];
  app.state.activeSessionId = previous.id;
  app.state.draftSessionActive = false;

  await app.switchToSession(sessionA.id, { closeSidebar: false });
  await aStateStarted;
  await app.switchToSession(sessionB.id, { sync: false, closeSidebar: false });
  releaseAState();
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));

  if (app.state.activeSessionId !== sessionB.id || sessionB.provider !== 'provider-b'
      || sessionA.provider !== 'provider-a' || openedPrompts.length !== 0) {
    fail(name, 'late A runtime state leaked after B became current', JSON.stringify({ active: app.state.activeSessionId, sessionA, sessionB, openedPrompts }));
    return;
  }
  pass(name);
}

async function testSidebarSwitchMalformedSideloadFallsBackExactlyOnce() {
  const name = 'sidebar switch malformed sideload falls back to transcript index and bodies exactly once';
  const order = [];
  let indexRequests = 0;
  let bodyRequests = 0;
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      const parsed = parsedTestURL(url);
      if (parsed?.pathname === '/ui/v1/sessions' && parsed.searchParams.get('selected_session') === 'sess_fallback') {
        order.push('selected');
        return new Response(JSON.stringify({
          sessions: [],
          selected_session: { id: 'sess_fallback', short_title: 'Fallback', origin: 'web', created_at: 1, message_count: 1, transcript_rev: 3 },
          selected_transcript: { index_etag: '"bad"', index: { rev: 3, rows: { ids: [31], seqs: [], roles: 'u', flags: [0] } }, bodies: { rev: 3, messages: [] } },
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isTranscriptIndexURL(url, 'sess_fallback')) {
        indexRequests += 1;
        order.push('index');
        return new Response(JSON.stringify({
          rev: 3, compaction_seq: -1, compaction_count: 0,
          rows: { ids: [31], seqs: [0], roles: 'u', flags: [0] },
        }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"fallback-3"' } });
      }
      if (isTranscriptBodiesURL(url, 'sess_fallback')) {
        bodyRequests += 1;
        order.push('bodies');
        return new Response(JSON.stringify({
          rev: 3,
          messages: [{ id: 31, sequence: 0, role: 'user', parts: [{ type: 'text', text: 'fallback once' }] }],
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed?.pathname === '/ui/v1/sessions/sess_fallback/state') {
        order.push('state');
        return new Response(JSON.stringify({ active_run: false, transcript_rev: 3 }), {
          status: 200, headers: { 'Content-Type': 'application/json' },
        });
      }
      if (parsed?.pathname === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({}), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
    appOverrides: { async refreshSkillCommands() { return []; } },
  });
  app.stopSidebarStatusPoll();
  const target = { id: 'sess_fallback', title: 'Fallback', messages: [], created: 1, _serverOnly: true };
  app.state.sessions = [{ id: 'previous', messages: [], created: 0 }, target];
  app.state.activeSessionId = 'previous';
  app.state.draftSessionActive = false;

  await app.switchToSession(target.id, { closeSidebar: false });
  await waitFor(() => order.includes('state'), 'fallback state hydration did not start');
  if (indexRequests !== 1 || bodyRequests !== 1 || target.messages[0]?.content !== 'fallback once') {
    fail(name, 'fallback request count or projection was incorrect', JSON.stringify({ indexRequests, bodyRequests, messages: target.messages }));
    return;
  }
  if (JSON.stringify(order.slice(0, 4)) !== JSON.stringify(['selected', 'index', 'bodies', 'state'])) {
    fail(name, 'fallback did not finish rendering before asynchronous state', JSON.stringify(order));
    return;
  }
  pass(name);
}

async function testSidebarSwitchActiveSideloadReconcilesNewerStartedRevision() {
  const name = 'sidebar active sideload reconciles a newer started revision before resuming';
  const order = [];
  const resumeCalls = [];
  let indexRequests = 0;
  let bodyRequests = 0;
  let appRef = null;
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      const parsed = parsedTestURL(url);
      if (parsed?.pathname === '/ui/v1/sessions' && parsed.searchParams.get('selected_session') === 'sess_active_switch') {
        order.push('selected');
        return new Response(JSON.stringify({
          sessions: [],
          selected_session: { id: 'sess_active_switch', short_title: 'Active', origin: 'web', created_at: 1, message_count: 2, transcript_rev: 6 },
          selected_transcript: startupTranscriptSideload({
            rev: 4,
            ids: [41],
            messages: [{ id: 41, sequence: 0, role: 'user', parts: [{ type: 'text', text: 'ready at four' }] }],
            activeResponseId: 'resp_old',
            startedRev: 4,
          }),
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed?.pathname === '/ui/v1/sessions/sess_active_switch/state') {
        order.push('state');
        return new Response(JSON.stringify({
          active_run: true, active_response_id: 'resp_new', started_rev: 5, transcript_rev: 6,
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isTranscriptIndexURL(url, 'sess_active_switch')) {
        indexRequests += 1;
        order.push('index');
        return new Response(JSON.stringify({
          rev: 5, compaction_seq: -1, compaction_count: 0,
          active_response_id: 'resp_new', started_rev: 5,
          rows: { ids: [41, 42], seqs: [0, 1], roles: 'uu', flags: [0, 0] },
        }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"active-switch-5"' } });
      }
      if (isTranscriptBodiesURL(url, 'sess_active_switch')) {
        bodyRequests += 1;
        order.push('bodies');
        return new Response(JSON.stringify({
          rev: 5,
          messages: [
            { id: 41, sequence: 0, role: 'user', parts: [{ type: 'text', text: 'ready at four' }] },
            { id: 42, sequence: 1, role: 'user', parts: [{ type: 'text', text: 'started at five' }] },
          ],
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed?.pathname === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({}), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
    appOverrides: {
      renderMessages() {
        const target = appRef?.state.sessions.find((item) => item.id === 'sess_active_switch');
        if (target?.messages.some((message) => message.content === 'ready at four')) order.push('render');
      },
      async resumeActiveResponse(session, options = {}) {
        order.push('resume');
        resumeCalls.push({ sessionId: session.id, responseId: options.responseId });
      },
      async refreshSkillCommands() { return []; },
    },
    onInitializeStarted({ app: startedApp }) { appRef = startedApp; },
  });
  app.stopSidebarStatusPoll();
  const target = { id: 'sess_active_switch', title: 'Active', messages: [], created: 1, _serverOnly: true };
  app.state.sessions = [{ id: 'previous', messages: [], created: 0 }, target];
  app.state.activeSessionId = 'previous';
  app.state.draftSessionActive = false;
  order.length = 0;

  await app.switchToSession(target.id, { closeSidebar: false });
  await waitFor(() => resumeCalls.length > 0, 'newer active response was not resumed');
  if (indexRequests !== 1 || bodyRequests !== 1 || target.transcript?.rev !== 5) {
    fail(name, 'newer started revision did not reconcile exactly once', JSON.stringify({ indexRequests, bodyRequests, rev: target.transcript?.rev }));
    return;
  }
  if (resumeCalls.length !== 1 || resumeCalls[0].responseId !== 'resp_new'
      || order.indexOf('render') < 0 || order.indexOf('render') > order.indexOf('state')
      || order.indexOf('bodies') > order.indexOf('resume')) {
    fail(name, 'active response ordering was incorrect', JSON.stringify({ order, resumeCalls }));
    return;
  }
  pass(name);
}

async function testSidebarSwitchSkipsLoadedAndUnresolvedSessionRequests() {
  const name = 'sidebar switch skips selected and scoped requests for loaded live and unresolved sessions';
  const fetchCalls = [];
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      fetchCalls.push(String(url));
      const parsed = parsedTestURL(url);
      if (parsed?.pathname === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({}), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
    appOverrides: { async refreshSkillCommands() { return []; } },
  });
  app.stopSidebarStatusPoll();
  const previous = { id: 'previous', messages: [], created: 0 };
  const loaded = {
    id: 'sess_loaded', messages: [{ id: 'live', role: 'assistant', content: 'already live' }], created: 1,
    activeResponseId: 'resp_live',
  };
  const unresolved = { id: '1291', number: 1291, messages: [], created: 2, _serverOnly: true };
  app.state.sessions = [previous, loaded, unresolved];
  app.state.activeSessionId = previous.id;
  app.state.draftSessionActive = false;
  fetchCalls.length = 0;

  await app.switchToSession(loaded.id, { sync: false, closeSidebar: false });
  await new Promise((resolve) => setImmediate(resolve));
  const loadedCalls = fetchCalls.slice();
  await app.switchToSession(unresolved.id, { closeSidebar: false });
  await new Promise((resolve) => setImmediate(resolve));

  if (loadedCalls.length !== 0) {
    fail(name, 'already-loaded live session refetched during selection', JSON.stringify(loadedCalls));
    return;
  }
  const unresolvedScoped = fetchCalls.filter((url) => String(url).includes('/v1/sessions/1291/'));
  if (unresolvedScoped.length !== 0) {
    fail(name, 'unresolved numeric identity reached a session-scoped endpoint', JSON.stringify(unresolvedScoped));
    return;
  }
  pass(name);
}

async function testStartupSplashWaitsForDeepLinkedTranscriptRender() {
  const name = 'fallback transcript render reveals startup before delayed state and skills';
  const events = [];
  let releaseBodies;
  let releaseState;
  let releaseSkills;
  let markBodiesStarted;
  let markStateStarted;
  let markHydratedRendered;
  let markSkillsStarted;
  let stateResolved = false;
  const bodiesStarted = new Promise((resolve) => { markBodiesStarted = resolve; });
  const stateStarted = new Promise((resolve) => { markStateStarted = resolve; });
  const hydratedRendered = new Promise((resolve) => { markHydratedRendered = resolve; });
  const skillsStarted = new Promise((resolve) => { markSkillsStarted = resolve; });
  const bodiesGate = new Promise((resolve) => { releaseBodies = resolve; });
  const stateGate = new Promise((resolve) => { releaseState = resolve; });
  const skillsGate = new Promise((resolve) => { releaseSkills = resolve; });

  const harnessPromise = createSessionsHarness({
    pathname: '/ui/3347',
    fetchImpl: async (url) => {
      if (parsedTestURL(url)?.pathname === '/ui/v1/sessions') {
        return new Response(JSON.stringify({
          sessions: [{
            id: 'sess_3347',
            number: 3347,
            short_title: 'Hydrated session',
            long_title: 'Hydrated session',
            mode: 'chat',
            origin: 'web',
            archived: false,
            pinned: false,
            created_at: 1710000000000,
            message_count: 2,
            transcript_rev: 2,
          }],
          widget_status: { widgets: [] },
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isTranscriptIndexURL(url, 'sess_3347')) {
        return new Response(JSON.stringify({
          rev: 2,
          compaction_seq: -1,
          compaction_count: 0,
          rows: { ids: [31, 32], seqs: [0, 1], roles: 'ua', flags: [0, 0] }
        }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"startup-2"' } });
      }
      if (isTranscriptBodiesURL(url, 'sess_3347')) {
        events.push('bodies-started');
        markBodiesStarted();
        await bodiesGate;
        events.push('bodies-resolved');
        return new Response(JSON.stringify({
          rev: 2,
          messages: [
            { id: 31, sequence: 0, role: 'user', created_at: 1000, parts: [{ type: 'text', text: 'question' }] },
            { id: 32, sequence: 1, role: 'assistant', created_at: 2000, parts: [{ type: 'text', text: 'answer' }] }
          ]
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (String(url) === '/ui/v1/sessions/sess_3347/state') {
        markStateStarted();
        await stateGate;
        stateResolved = true;
        return new Response(JSON.stringify({ active_run: false, transcript_rev: 2 }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      return new Response(JSON.stringify([]), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
    appOverrides: {
      hideStartupSplash() {
        events.push('splash-hidden');
      },
      renderMessages() {
        events.push('messages-rendered');
        if (events.includes('bodies-resolved')) markHydratedRendered();
      },
      async refreshSkillCommands() {
        events.push('skills-started');
        markSkillsStarted();
        await skillsGate;
        events.push('skills-resolved');
      },
    },
  });

  await Promise.all([bodiesStarted, stateStarted]);
  const hiddenBeforeBodies = events.includes('splash-hidden');
  releaseBodies();
  await hydratedRendered;
  await new Promise((resolve) => setImmediate(resolve));
  const splashBeforeState = events.includes('splash-hidden') && !stateResolved;
  releaseState();
  await skillsStarted;
  const skillsPendingWhenSplashHid = !events.includes('skills-resolved');
  releaseSkills();
  const { app } = await harnessPromise;

  if (hiddenBeforeBodies) {
    fail(name, 'startup splash hid while canonical transcript bodies were still pending', JSON.stringify(events));
    return;
  }
  const bodiesResolvedIndex = events.indexOf('bodies-resolved');
  const hydratedRenderIndex = events.indexOf('messages-rendered', bodiesResolvedIndex + 1);
  const splashHiddenIndex = events.indexOf('splash-hidden');
  if (bodiesResolvedIndex < 0 || hydratedRenderIndex < 0 || splashHiddenIndex <= hydratedRenderIndex) {
    fail(name, 'startup splash did not hide after the hydrated transcript render boundary', JSON.stringify(events));
    return;
  }
  if (!splashBeforeState || !skillsPendingWhenSplashHid) {
    fail(name, 'startup splash waited for delayed runtime state or skill-command refresh', JSON.stringify(events));
    return;
  }
  const session = app.state.sessions.find((item) => item.id === 'sess_3347');
  if (!session || session.messages.map((message) => message.content).join(',') !== 'question,answer') {
    fail(name, 'startup completed without the canonical transcript projection', JSON.stringify(session?.messages || []));
    return;
  }
  pass(name);
}

async function testStartupSideloadsWidgetsBeforeFirstPaint() {
  const name = 'startup sideloads authoritative widget status before first paint without widget requests';
  const events = [];
  const fetchCalls = [];
  const sideloadedWidgets = [
    { id: 'metrics', mount: 'metrics', title: 'Metrics', description: 'Local metrics', state: 'stopped' },
  ];

  let appRef = null;
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      fetchCalls.push(String(url));
      if (parsedTestURL(url)?.pathname === '/ui/v1/sessions') {
        return new Response(JSON.stringify({
          sessions: [],
          selected_session: null,
          widget_status: { widgets: sideloadedWidgets },
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    },
    appOverrides: {
      renderWidgetSidebar() {
        events.push({
          type: 'widgets-rendered',
          loaded: Boolean(appRef?.state.widgetsLoaded),
          widgets: JSON.parse(JSON.stringify(appRef?.state.widgets || [])),
        });
      },
      hideStartupSplash() { events.push({ type: 'splash-hidden' }); },
    },
    onInitializeStarted({ app: startedApp }) { appRef = startedApp; },
  });
  app.stopSidebarStatusPoll();

  const widgetRender = events.find((event) => event.type === 'widgets-rendered');
  const widgetRenderIndex = events.indexOf(widgetRender);
  const splashIndex = events.findIndex((event) => event.type === 'splash-hidden');
  const startupSessionsURL = fetchCalls.find((url) => parsedTestURL(url)?.pathname === '/ui/v1/sessions');
  if (!startupSessionsURL || parsedTestURL(startupSessionsURL)?.searchParams.get('include_widget_status') !== '1') {
    fail(name, 'primary startup request did not opt into widget status sideload', JSON.stringify(fetchCalls));
    return;
  }
  if (!widgetRender || !widgetRender.loaded || JSON.stringify(widgetRender.widgets) !== JSON.stringify(sideloadedWidgets)) {
    fail(name, 'first widget render did not use the authoritative sideload', JSON.stringify(events));
    return;
  }
  if (widgetRenderIndex < 0 || splashIndex <= widgetRenderIndex) {
    fail(name, 'widget sidebar did not render before the startup splash was hidden', JSON.stringify(events));
    return;
  }
  if (fetchCalls.some((url) => parsedTestURL(url)?.pathname === '/ui/admin/widgets/status')) {
    fail(name, 'startup requested the deferred widget status endpoint', JSON.stringify(fetchCalls));
    return;
  }
  if (fetchCalls.some((url) => parsedTestURL(url)?.pathname.startsWith('/ui/widgets/'))) {
    fail(name, 'startup eagerly requested full widget app content', JSON.stringify(fetchCalls));
    return;
  }
  pass(name);
}

async function testStartupSideloadsEmptyWidgetList() {
  const name = 'startup treats an empty sideloaded widget list as authoritative';
  let statusRequests = 0;
  const renders = [];
  let appRef = null;

  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      const parsed = parsedTestURL(url);
      if (parsed?.pathname === '/ui/admin/widgets/status') statusRequests += 1;
      if (parsed?.pathname === '/ui/v1/sessions') {
        return new Response(JSON.stringify({
          sessions: [],
          selected_session: null,
          widget_status: { widgets: [] },
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    },
    appOverrides: {
      renderWidgetSidebar() {
        renders.push({
          loaded: Boolean(appRef?.state.widgetsLoaded),
          count: appRef?.state.widgets.length || 0,
        });
      },
    },
    onInitializeStarted({ app: startedApp }) { appRef = startedApp; },
  });
  app.stopSidebarStatusPoll();

  if (statusRequests !== 0 || !app.state.widgetsLoaded || app.state.widgets.length !== 0) {
    fail(name, 'empty widget sideload was not final at startup', JSON.stringify({ statusRequests, state: app.state }));
    return;
  }
  if (renders.length !== 1 || !renders[0].loaded || renders[0].count !== 0) {
    fail(name, 'empty widget sideload caused a provisional or duplicate render', JSON.stringify(renders));
    return;
  }
  pass(name);
}

async function testExplicitWidgetRefreshStillUsesStatusEndpoint() {
  const name = 'explicit widget refresh uses the status endpoint after startup';
  let statusRequests = 0;
  const initialWidget = { id: 'one', mount: 'one', title: 'One', state: 'stopped' };
  const refreshedWidget = { id: 'two', mount: 'two', title: 'Two', state: 'running' };

  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      const parsed = parsedTestURL(url);
      if (parsed?.pathname === '/ui/v1/sessions') {
        return new Response(JSON.stringify({
          sessions: [],
          selected_session: null,
          widget_status: { widgets: [initialWidget] },
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed?.pathname === '/ui/admin/widgets/status') {
        statusRequests += 1;
        return new Response(JSON.stringify({ widgets: [refreshedWidget] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      return new Response(JSON.stringify({ sessions: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    },
    appOverrides: { renderWidgetSidebar() {} },
  });
  app.stopSidebarStatusPoll();

  if (statusRequests !== 0 || app.state.widgets[0]?.id !== 'one') {
    fail(name, 'startup did not remain request-free with sideloaded status', JSON.stringify({ statusRequests, widgets: app.state.widgets }));
    return;
  }
  if (typeof app.refreshWidgetsSidebar !== 'function') {
    fail(name, 'explicit widget refresh is not available');
    return;
  }
  await app.refreshWidgetsSidebar();
  if (statusRequests !== 1 || app.state.widgets[0]?.id !== 'two' || !app.state.widgetsLoaded) {
    fail(name, 'explicit refresh did not update from the status endpoint', JSON.stringify({ statusRequests, widgets: app.state.widgets }));
    return;
  }
  pass(name);
}

async function testStartupHydrationTimeoutDoesNotBlockIndefinitely() {
  const name = 'deep-linked startup hydration has a bounded splash fallback';
  let releaseBodies;
  let bodiesPending = false;
  let splashHidden = false;
  const bodiesGate = new Promise((resolve) => { releaseBodies = resolve; });

  const { app } = await createSessionsHarness({
    pathname: '/ui/3348',
    startupHydrationTimeoutMS: 0,
    fetchImpl: async (url) => {
      if (parsedTestURL(url)?.pathname === '/ui/v1/sessions') {
        return new Response(JSON.stringify({
          sessions: [{
            id: 'sess_3348',
            number: 3348,
            short_title: 'Slow session',
            mode: 'chat',
            origin: 'web',
            created_at: 1710000000000,
            message_count: 1,
            transcript_rev: 1,
          }]
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isTranscriptIndexURL(url, 'sess_3348')) {
        return new Response(JSON.stringify({
          rev: 1,
          compaction_seq: -1,
          compaction_count: 0,
          rows: { ids: [41], seqs: [0], roles: 'u', flags: [0] }
        }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"startup-slow-1"' } });
      }
      if (isTranscriptBodiesURL(url, 'sess_3348')) {
        bodiesPending = true;
        await bodiesGate;
        bodiesPending = false;
        return new Response(JSON.stringify({
          rev: 1,
          messages: [{ id: 41, sequence: 0, role: 'user', created_at: 1000, parts: [{ type: 'text', text: 'eventually' }] }]
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (String(url) === '/ui/v1/sessions/sess_3348/state') {
        return new Response(JSON.stringify({ active_run: false, transcript_rev: 1 }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      return new Response(JSON.stringify([]), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
    appOverrides: {
      hideStartupSplash() { splashHidden = true; },
    },
  });

  if (!splashHidden || !bodiesPending) {
    releaseBodies();
    fail(name, 'startup did not leave the splash through the bound while hydration remained pending', JSON.stringify({ splashHidden, bodiesPending }));
    return;
  }

  releaseBodies();
  for (let attempts = 0; attempts < 10 && app.state.sessions[0]?._serverOnly; attempts += 1) {
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
  pass(name);
}

async function testUnresolvedNumericDeepLinkSkipsSessionScopedStartupRequests() {
  const name = 'unresolved numeric deep link waits before session-scoped startup requests';
  const fetchCalls = [];
  const { app } = await createSessionsHarness({
    pathname: '/ui/3347',
    fetchImpl: async (url) => {
      fetchCalls.push(String(url));
      if (parsedTestURL(url)?.pathname === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      return new Response(JSON.stringify({}), {
        status: 200,
        headers: { 'Content-Type': 'application/json' }
      });
    }
  });

  if (app.state.activeSessionId !== '3347') {
    fail(name, `numeric route stub unexpectedly changed identity to ${JSON.stringify(app.state.activeSessionId)}`);
    return;
  }
  const scopedCalls = fetchCalls.filter((url) => {
    const parsed = parsedTestURL(url);
    return Boolean(parsed && (
      parsed.pathname.includes('/v1/sessions/3347/')
      || parsed.pathname.includes('/api/sessions/3347/')
    ));
  });
  if (scopedCalls.length > 0) {
    fail(name, 'startup issued requests against an unresolved numeric identity', JSON.stringify(fetchCalls));
    return;
  }
  pass(name);
}


async function testNewQueryStartsDraftInsteadOfLastSession() {
  const name = 'new query starts draft instead of resuming last session';
  const { app, windowObj } = await createSessionsHarness({
    search: '?new=1',
    initialStorage: {
      term_llm_active_session: 'sess_old',
      term_llm_draft_session_active: '0',
    },
    fetchImpl: async (url) => {
      if (parsedTestURL(url)?.pathname === '/ui/v1/sessions') {
        return new Response(JSON.stringify({
          sessions: [{
            id: 'sess_old',
            short_title: 'Old session',
            long_title: 'Old session',
            mode: 'chat',
            origin: 'web',
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

  if (!app.state.draftSessionActive || app.state.activeSessionId !== '') {
    fail(name, 'expected fresh draft to stay active', JSON.stringify({
      draftSessionActive: app.state.draftSessionActive,
      activeSessionId: app.state.activeSessionId,
    }));
    return;
  }
  if (windowObj.location.search !== '') {
    fail(name, 'expected ?new=1 to be cleared from the address bar', windowObj.location.search);
    return;
  }

  pass(name);
}

async function testNewQueryRefreshesHeaderAfterRuntimeMetadataLoads() {
  const name = 'new query refreshes header after runtime metadata loads';
  const headerCalls = [];
  await createSessionsHarness({
    search: '?new=1',
    appOverrides: {
      fetchProviders: async () => [{
        name: 'chatgpt',
        configured: true,
        is_default: true,
        default_model: 'gpt-5.5-medium',
        models: ['gpt-5.5'],
      }],
      fetchModels: async () => ['gpt-5.5'],
      updateHeader() {
        headerCalls.push({
          providers: this.state.providers.map((provider) => provider.name),
          models: this.state.models.slice(),
          draftSessionActive: this.state.draftSessionActive,
          activeSessionId: this.state.activeSessionId,
        });
      },
    },
  });

  const postMetadataCall = headerCalls.find((call) => (
    call.draftSessionActive
    && call.activeSessionId === ''
    && call.providers.includes('chatgpt')
    && call.models.includes('gpt-5.5')
  ));
  if (!postMetadataCall) {
    fail(name, 'expected initialize to refresh the draft header after providers and models load', JSON.stringify(headerCalls));
    return;
  }

  pass(name);
}

async function testMergeServerSessionsMigratesInterruptBuffersToRealSessionId() {
  const name = 'session id reconciliation migrates interrupt buffers to real session id';
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (parsedTestURL(url)?.pathname === '/ui/v1/sessions') {
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
      if (parsedTestURL(url)?.pathname === '/ui/v1/sessions') {
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
      if (isTranscriptIndexURL(url, 'sess_dev')) {
        return new Response(JSON.stringify({
          rev: 1,
          compaction_seq: -1,
          compaction_count: 0,
          rows: { ids: [21, 22], seqs: [1, 2], roles: 'ua', flags: [0, 0] }
        }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"dev-1"' } });
      }
      if (isTranscriptBodiesURL(url, 'sess_dev')) {
        return new Response(JSON.stringify({
          rev: 1,
          messages: [
            {
              id: 21,
              sequence: 1,
              role: 'user',
              created_at: 1710000001000,
              parts: [{ type: 'text', text: 'hello' }]
            },
            {
              id: 22,
              sequence: 2,
              role: 'assistant',
              created_at: 1710000002000,
              parts: [{ type: 'text', text: 'hi there' }]
            }
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

async function testRunErrorEventsConvertToErrorMessages() {
  const name = 'run error events convert to durable error messages';
  const { app } = await createSessionsHarness();

  const converted = app.convertServerMessages([
    {
      id: 1,
      sequence: 1,
      role: 'event',
      created_at: 1000,
      parts: [{ type: 'error', text: 'OpenAI Responses WebSocket retries exhausted' }],
    },
  ]);

  if (converted.length !== 1) {
    fail(name, `expected 1 converted message, got ${converted.length}`, JSON.stringify(converted));
    return;
  }
  const msg = converted[0];
  if (msg.role !== 'error' || msg.content !== 'OpenAI Responses WebSocket retries exhausted' || msg.serverSeq !== 1) {
    fail(name, 'converted run error missing expected fields', JSON.stringify(msg));
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

async function testCompactionDuplicateTailRangeIsLinear() {
  const name = 'legacy compaction tail overlap is correct with linear bounded work';
  const { app } = await createSessionsHarness();
  const prefixLength = 3000;
  const overlapLength = 1400;
  const messages = Array.from({ length: prefixLength }, (_, index) => ({
    role: index % 2 === 0 ? 'user' : 'assistant',
    content: `message-${index}`,
  }));
  const markerIndex = messages.length;
  messages.push({ role: 'compaction', content: 'Context compacted' });
  messages.push({
    role: 'assistant',
    content: "I've reviewed the context summary. I'll continue from where we left off.",
  });
  for (let index = prefixLength - overlapLength; index < prefixLength; index += 1) {
    messages.push({ ...messages[index] });
  }
  messages.push({ role: 'assistant', content: 'new content after duplicated tail' });

  const metrics = { operations: 0, fingerprints: 0 };
  const match = app.compactionDuplicateTailRange(messages, markerIndex, null, metrics);
  if (match.start !== markerIndex + 2 || match.length !== overlapLength) {
    fail(name, 'incorrect longest overlap', JSON.stringify(match));
    return;
  }
  if (metrics.fingerprints !== messages.length) {
    fail(name, `expected one precomputed fingerprint per message, got ${metrics.fingerprints}`);
    return;
  }
  if (metrics.operations > messages.length * 8) {
    fail(name, `overlap work scaled beyond a linear bound: ${metrics.operations} operations for ${messages.length} messages`);
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

async function testConvertServerMessagesAttachesToolErrorsWithoutPhantoms() {
  const name = 'server tool_result errors update matching calls without phantom rows';
  const { app } = await createSessionsHarness();

  const converted = app.convertServerMessages([
    {
      role: 'assistant',
      created_at: 1000,
      parts: [{
        type: 'tool_call',
        tool_name: 'update_plan',
        tool_call_id: 'call_plan',
        tool_arguments: '{"plan":[]}',
      }],
    },
    {
      role: 'tool',
      created_at: 2000,
      parts: [{
        type: 'tool_result',
        tool_name: 'update_plan',
        tool_call_id: 'call_plan',
        tool_error: true,
      }],
    },
  ]);
  const tool = converted.find((message) => message.role === 'tool-group')?.tools?.[0];
  if (!tool || tool.status !== 'error') {
    fail(name, 'matching tool call was not marked as failed', JSON.stringify(converted));
    return;
  }

  const orphaned = app.convertServerMessages([{
    role: 'tool',
    created_at: 3000,
    parts: [{
      type: 'tool_result',
      tool_name: 'update_plan',
      tool_call_id: 'call_from_older_page',
      tool_error: true,
    }],
  }]);
  if (orphaned.some((message) => message.role === 'tool-group')) {
    fail(name, 'orphaned error-only result created a phantom tool row', JSON.stringify(orphaned));
    return;
  }

  pass(name);
}

async function testConvertServerMessagesCorrelatesSuccessfulPlanResults() {
  const name = 'server plan results retain one confirmed result source';
  const { app } = await createSessionsHarness();
  const args = '{"plan":[{"step":"Done","status":"completed"}]}';
  const converted = app.convertServerMessages([
    { role: 'assistant', created_at: 1000, parts: [{ type: 'text', text: 'Before' }] },
    { role: 'assistant', created_at: 2000, parts: [{ type: 'tool_call', tool_name: 'update_plan', tool_call_id: 'call_plan_success', tool_arguments: args }] },
    { role: 'tool', created_at: 3000, parts: [{ type: 'tool_result', tool_name: 'update_plan', tool_call_id: 'call_plan_success', tool_error: false }] },
    { role: 'assistant', created_at: 4000, parts: [{ type: 'text', text: 'After' }] },
  ]);
  const groups = converted.filter((message) => message.role === 'tool-group');
  const tool = groups[0]?.tools?.[0];
  if (groups.length !== 1 || tool?.resultStatus !== 'success' || tool.status !== 'done') {
    fail(name, 'successful result was not correlated exactly once', JSON.stringify(converted));
    return;
  }
  if (converted.map((message) => message.role).join(',') !== 'assistant,tool-group,assistant') {
    fail(name, 'tool group moved out of transcript chronology', JSON.stringify(converted));
    return;
  }

  const orphaned = app.convertServerMessages([{
    role: 'tool',
    created_at: 3000,
    parts: [{ type: 'tool_result', tool_name: 'update_plan', tool_call_id: 'call_older_page', tool_error: false }],
  }]);
  if (orphaned.some((message) => message.role === 'tool-group')) {
    fail(name, 'result-only page created a phantom tool group', JSON.stringify(orphaned));
    return;
  }
  pass(name);
}

async function testConvertServerMessagesRebasesHubImageURLs() {
  const name = 'server message conversion rebases hub image URLs';
  const { app } = await createSessionsHarness({
    uiPrefix: '/hub/node/alpha',
    pathname: '/hub/node/alpha/',
    hub: { url: '/hub/', nodeId: 'alpha', nodeName: 'Alpha', nodeBasePath: '/ui' }
  });

  const converted = app.convertServerMessages([
    {
      role: 'user',
      created_at: 1000,
      parts: [
        { type: 'image', image_url: '/ui/images/upload.png', mime_type: 'image/png' },
        { type: 'text', text: 'look at this' },
      ],
    },
    {
      role: 'tool',
      created_at: 2000,
      parts: [{
        type: 'tool_result',
        tool_name: 'image_generate',
        tool_call_id: 'call_img',
        images: ['/ui/images/generated.png', '/ui/files/artifact.svg'],
      }],
    },
  ]);

  const user = converted.find((message) => message.role === 'user');
  const userImage = user?.attachments?.[0]?.dataURL;
  if (userImage !== '/hub/node/alpha/images/upload.png') {
    fail(name, `user image URL = ${JSON.stringify(userImage)}`, JSON.stringify(converted));
    return;
  }

  const tool = converted.find((message) => message.role === 'tool-group')?.tools?.[0];
  const images = tool?.images || [];
  if (images[0] !== '/hub/node/alpha/images/generated.png' || images[1] !== '/hub/node/alpha/files/artifact.svg') {
    fail(name, 'tool image URLs were not rebased', JSON.stringify(converted));
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
      if (parsedTestURL(url)?.pathname === '/ui/v1/sessions') {
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
          transcript_rev: 2,
          started_rev: 2
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isTranscriptIndexURL(url, 'sess_resume')) {
        return new Response(JSON.stringify({
          rev: 2,
          compaction_seq: -1,
          compaction_count: 0,
          rows: { ids: [31], seqs: [0], roles: 'u', flags: [0] }
        }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"resume-2"' } });
      }
      if (isTranscriptBodiesURL(url, 'sess_resume')) {
        return new Response(JSON.stringify({
          rev: 2,
          messages: [{ id: 31, sequence: 0, role: 'user', created_at: 1710000001000, parts: [{ type: 'text', text: 'check status' }] }]
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
  await waitFor(() => resumeCalls.length > 0, 'sidebar state hydration did not resume active response');

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

async function testSwitchToSessionAttachesChangedActiveResponseFromStartedRevision() {
  const name = 'sidebar session switch attaches a changed active response after started_rev reconciliation';
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
      if (parsedTestURL(url)?.pathname === '/ui/v1/sessions') {
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
          transcript_rev: 2,
          started_rev: 2
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (isTranscriptIndexURL(url, 'sess_resume')) {
        return new Response(JSON.stringify({
          rev: 2,
          compaction_seq: -1,
          compaction_count: 0,
          rows: { ids: [31], seqs: [0], roles: 'u', flags: [0] }
        }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"resume-2"' } });
      }
      if (isTranscriptBodiesURL(url, 'sess_resume')) {
        return new Response(JSON.stringify({
          rev: 2,
          messages: [{ id: 31, sequence: 0, role: 'user', created_at: 1710000001000, parts: [{ type: 'text', text: 'check status' }] }]
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
  await waitFor(() => resumeCalls.length > 0, 'changed active response did not resume after asynchronous state hydration');

  if (resumeCalls.length !== 1) {
    fail(name, 'expected session switch to trigger exactly one resume', JSON.stringify(resumeCalls));
    return;
  }
  if (resumeCalls[0].recoverFromSnapshot) {
    fail(name, 'new active run should replay from event sequence zero instead of attaching a stale local prefix', JSON.stringify(resumeCalls[0]));
    return;
  }
  const active = app.state.sessions.find((session) => session.id === 'sess_resume');
  if (Number(active?.transcript?.rev || 0) < 2) {
    fail(name, 'active response attached before transcript reached started_rev', JSON.stringify(active?.transcript || null));
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
      if (parsedTestURL(url)?.pathname === '/ui/v1/sessions') {
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
      if (parsedTestURL(url)?.pathname === '/ui/v1/sessions') {
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
      if (parsedTestURL(url)?.pathname === '/ui/v1/sessions') {
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

async function testPreConnectedVisibilityDoesNotStartSidebarStatusPoll() {
  const name = 'pre-connected visibility changes wait for initial session merge before status polling';
  let releaseSessions;
  let visibilityHandler = null;
  let statusCalls = 0;
  const sessionsGate = new Promise((resolve) => {
    releaseSessions = () => resolve(new Response(JSON.stringify({ sessions: [] }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    }));
  });

  const harnessPromise = createSessionsHarness({
    fetchImpl: async (url) => {
      if (String(url) === '/ui/v1/sessions') return sessionsGate;
      if (String(url) === '/ui/v1/sessions/status') statusCalls += 1;
      return new Response(JSON.stringify({ sessions: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    },
    onInitializeStarted({ windowObj }) {
      visibilityHandler = (windowObj.document.listeners.visibilitychange || [])[0] || null;
      if (!visibilityHandler) return;
      windowObj.document.visibilityState = 'hidden';
      void visibilityHandler({ type: 'visibilitychange' });
      windowObj.document.visibilityState = 'visible';
      void visibilityHandler({ type: 'visibilitychange' });
    },
  });

  if (!visibilityHandler) {
    releaseSessions();
    await harnessPromise;
    fail(name, 'expected visibilitychange listener during initialization');
    return;
  }
  if (statusCalls !== 0) {
    releaseSessions();
    await harnessPromise;
    fail(name, `pre-connected visibility started ${statusCalls} status requests`);
    return;
  }

  releaseSessions();
  const { app, windowObj } = await harnessPromise;
  if (!app.state.connected || statusCalls !== 0) {
    app.stopSidebarStatusPoll();
    fail(name, 'initial authoritative merge should only schedule status polling', JSON.stringify({ connected: app.state.connected, statusCalls }));
    return;
  }

  windowObj.document.visibilityState = 'hidden';
  await visibilityHandler({ type: 'visibilitychange' });
  windowObj.document.visibilityState = 'visible';
  await visibilityHandler({ type: 'visibilitychange' });
  if (statusCalls !== 1) {
    app.stopSidebarStatusPoll();
    fail(name, `later visibility recovery did not poll immediately; got ${statusCalls} status requests`);
    return;
  }

  app.stopSidebarStatusPoll();
  pass(name);
}

async function testPageshowWaitsForInitialConnectionBeforeStatusPoll() {
  const name = 'pageshow waits for initial session merge before starting status polling';
  let releaseSessions;
  let pageshowHandler = null;
  let statusCalls = 0;
  const sessionsGate = new Promise((resolve) => {
    releaseSessions = () => resolve(new Response(JSON.stringify({ sessions: [] }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    }));
  });

  const harnessPromise = createSessionsHarness({
    fetchImpl: async (url) => {
      if (String(url) === '/ui/v1/sessions') return sessionsGate;
      if (String(url) === '/ui/v1/sessions/status') statusCalls += 1;
      return new Response(JSON.stringify({ sessions: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    },
    onInitializeStarted({ windowObj }) {
      const pageshowHandlers = windowObj.listeners.pageshow || [];
      pageshowHandler = pageshowHandlers[pageshowHandlers.length - 1] || null;
      pageshowHandler?.({ type: 'pageshow', persisted: false });
    },
  });

  if (!pageshowHandler) {
    releaseSessions();
    await harnessPromise;
    fail(name, 'expected pageshow listener during initialization');
    return;
  }
  if (statusCalls !== 0) {
    releaseSessions();
    const { app } = await harnessPromise;
    app.stopSidebarStatusPoll();
    fail(name, `pre-connected pageshow started ${statusCalls} status requests`);
    return;
  }

  releaseSessions();
  const { app } = await harnessPromise;
  if (!app.state.connected || statusCalls !== 0) {
    app.stopSidebarStatusPoll();
    fail(name, 'initial authoritative merge should only schedule status polling', JSON.stringify({ connected: app.state.connected, statusCalls }));
    return;
  }

  await app.stopSidebarStatusPoll();
  pageshowHandler({ type: 'pageshow', persisted: true });
  if (statusCalls !== 1) {
    app.stopSidebarStatusPoll();
    fail(name, `connected BFCache pageshow did not recover immediately; got ${statusCalls} status requests`);
    return;
  }

  app.stopSidebarStatusPoll();
  pass(name);
}

async function testSidebarStatusPollRecoversIdempotentlyAfterPageShow() {
  const name = 'pageshow and transport recovery restart one immediate sidebar poll';
  const scheduled = [];
  let statusCalls = 0;
  let holdRecoveryStatus = false;
  let resolveRecoveryStatus = null;
  const { app, windowObj } = await createSessionsHarness({
    setTimeout(fn, delay) {
      const handle = { fn, delay, cleared: false, fired: false };
      scheduled.push(handle);
      return handle;
    },
    clearTimeout(handle) {
      if (handle) handle.cleared = true;
    },
    fetchImpl: async (url) => {
      if (url === '/ui/v1/sessions/status') {
        statusCalls += 1;
        if (holdRecoveryStatus && statusCalls === 1) {
          return new Promise((resolve) => {
            resolveRecoveryStatus = () => resolve(new Response(JSON.stringify({ sessions: [] }), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            }));
          });
        }
      }
      if (url.endsWith('/state')) {
        return new Response(JSON.stringify({ active_run: false, active_response_id: '' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      return new Response(JSON.stringify({ sessions: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    },
  });
  await app.stopSidebarStatusPoll();
  scheduled.length = 0;
  statusCalls = 0;
  holdRecoveryStatus = true;

  const session = {
    id: 'sess_pageshow_poll',
    title: 'Page show poll',
    origin: 'web',
    created: 1,
    messages: [],
    activeResponseId: null,
  };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;

  const visibilityHandler = (windowObj.document.listeners.visibilitychange || [])[0];
  const pageshowHandlers = windowObj.listeners.pageshow || [];
  const pageshowHandler = pageshowHandlers[pageshowHandlers.length - 1];
  if (!visibilityHandler || !pageshowHandler) {
    fail(name, 'expected visibilitychange and pageshow listeners');
    return;
  }

  windowObj.document.visibilityState = 'hidden';
  await visibilityHandler({ type: 'visibilitychange' });
  windowObj.document.visibilityState = 'visible';
  // BFCache restoration does not reliably emit another visibilitychange.
  pageshowHandler({ type: 'pageshow', persisted: true });

  if (typeof app.handleFetchTransportFallback !== 'function') {
    fail(name, 'expected an app-level transport fallback hook');
    return;
  }
  const repeatedRecovery = app.handleFetchTransportFallback();
  app.handleFetchTransportFallback();

  if (statusCalls !== 1 || typeof resolveRecoveryStatus !== 'function') {
    fail(name, `expected exactly one immediate status request, got ${statusCalls}`);
    return;
  }
  const liveBeforeResolve = scheduled.filter((handle) => !handle.cleared && !handle.fired);
  if (liveBeforeResolve.length !== 0) {
    fail(name, 'status timer was scheduled while the immediate request was in flight', JSON.stringify(liveBeforeResolve.map(({ delay }) => delay)));
    return;
  }

  resolveRecoveryStatus();
  await repeatedRecovery;
  await Promise.resolve();

  let liveTimers = scheduled.filter((handle) => !handle.cleared && !handle.fired);
  if (liveTimers.length !== 1 || liveTimers[0].delay !== 5000) {
    fail(name, 'expected exactly one continued visible-session poll timer', JSON.stringify(liveTimers.map(({ delay }) => delay)));
    return;
  }

  const nextTimer = liveTimers[0];
  nextTimer.fired = true;
  await nextTimer.fn();
  await Promise.resolve();
  if (statusCalls !== 2) {
    fail(name, `continued polling issued ${statusCalls} total requests, want 2`);
    return;
  }
  liveTimers = scheduled.filter((handle) => !handle.cleared && !handle.fired);
  if (liveTimers.length !== 1 || liveTimers[0].delay !== 5000) {
    fail(name, 'continued poll did not leave exactly one next timer', JSON.stringify(liveTimers.map(({ delay }) => delay)));
    return;
  }

  app.stopSidebarStatusPoll();
  pass(name);
}

async function testHiddenInFlightSidebarPollCannotRescheduleOrApplyStaleStatus() {
  const name = 'hidden in-flight sidebar status callback is stale and cannot restart polling';
  const scheduled = [];
  let statusCalls = 0;
  let resolveStatus = null;
  let staleSidebarUpdates = 0;
  const { app, windowObj } = await createSessionsHarness({
    setTimeout(fn, delay) {
      const handle = { fn, delay, cleared: false };
      scheduled.push(handle);
      return handle;
    },
    clearTimeout(handle) {
      if (handle) handle.cleared = true;
    },
    appOverrides: {
      updateSidebarStatus(sessions) {
        if (sessions.some((session) => session.id === 'stale_session')) staleSidebarUpdates += 1;
      },
    },
    fetchImpl: async (url) => {
      if (url === '/ui/v1/sessions/status') {
        statusCalls += 1;
        if (statusCalls >= 1) {
          return new Promise((resolve) => {
            resolveStatus = () => resolve(new Response(JSON.stringify({
              sessions: [{ id: 'stale_session', short_title: 'Stale' }],
            }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
          });
        }
      }
      return new Response(JSON.stringify({ sessions: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    },
  });
  await app.stopSidebarStatusPoll();
  scheduled.length = 0;
  staleSidebarUpdates = 0;
  await Promise.resolve();
  await Promise.resolve();

  const pollPromise = app.startSidebarStatusPoll();
  await Promise.resolve();
  if (typeof resolveStatus !== 'function') {
    fail(name, 'expected a status request to remain in flight');
    return;
  }
  const visibilityHandler = (windowObj.document.listeners.visibilitychange || [])[0];
  windowObj.document.visibilityState = 'hidden';
  await visibilityHandler({ type: 'visibilitychange' });
  resolveStatus();
  await pollPromise;
  await Promise.resolve();

  if (staleSidebarUpdates !== 0) {
    fail(name, `hidden stale response applied ${staleSidebarUpdates} sidebar updates`);
    return;
  }
  const liveTimers = scheduled.filter((handle) => !handle.cleared);
  if (liveTimers.length !== 0) {
    fail(name, 'hidden stale response restarted the background poll loop', JSON.stringify(liveTimers.map(({ delay }) => delay)));
    return;
  }

  pass(name);
}

async function testReconnectBackoffWakeSignalsReuseExistingLoop() {
  const name = 'online visibility and pageshow wake the existing response reconnect loop';
  const wakeReasons = [];
  let resumeCalls = 0;
  const { app, windowObj } = await createSessionsHarness({
    appOverrides: {
      wakeResponseReconnect({ reason, sessionId, responseId }) {
        wakeReasons.push(`${reason}:${sessionId}:${responseId}`);
        return true;
      },
      resumeActiveResponse: async () => { resumeCalls += 1; },
    },
  });
  app.stopSidebarStatusPoll();

  const session = {
    id: 'sess_wake_signals',
    title: 'Wake signals',
    origin: 'web',
    created: 1,
    messages: [],
    activeResponseId: 'resp_wake_signals',
  };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;
  app.state.abortController = null;

  const visibilityHandler = (windowObj.document.listeners.visibilitychange || [])[0];
  const onlineHandler = (windowObj.listeners.online || [])[0];
  const pageshowHandlers = windowObj.listeners.pageshow || [];
  const pageshowHandler = pageshowHandlers[pageshowHandlers.length - 1];
  if (!visibilityHandler || !onlineHandler || !pageshowHandler) {
    fail(name, 'expected all reconnect wake listeners to be registered');
    return;
  }

  await visibilityHandler({ type: 'visibilitychange' });
  await onlineHandler({ type: 'online' });
  pageshowHandler({ type: 'pageshow', persisted: false });

  const want = [
    'visibility:sess_wake_signals:resp_wake_signals',
    'online:sess_wake_signals:resp_wake_signals',
    'pageshow:sess_wake_signals:resp_wake_signals',
  ];
  if (JSON.stringify(wakeReasons) !== JSON.stringify(want)) {
    fail(name, 'unexpected reconnect wake calls', JSON.stringify(wakeReasons));
    return;
  }
  if (resumeCalls !== 0) {
    fail(name, `wake listeners started ${resumeCalls} duplicate resume loops`);
    return;
  }

  pass(name);
}

async function testLoadServerSessionStateUsesExplicitResultContract() {
  const name = 'session state loader returns explicit ok auth and retry results';
  let authFailures = 0;
  const { app } = await createSessionsHarness({
    appOverrides: {
      handleAuthFailure() { authFailures += 1; },
    },
    fetchImpl: async (url) => {
      if (url === '/ui/v1/sessions/sess_contract_ok/state') {
        return new Response(JSON.stringify({ active_run: true, active_response_id: 'resp_contract' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      if (url === '/ui/v1/sessions/sess_contract_auth/state') {
        return new Response('unauthorized', { status: 401 });
      }
      if (url === '/ui/v1/sessions/sess_contract_retry/state') {
        return new Response('unavailable', { status: 503 });
      }
      return new Response(JSON.stringify({ sessions: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    },
  });
  app.stopSidebarStatusPoll();

  const ok = await app.loadServerSessionState('sess_contract_ok');
  const auth = await app.loadServerSessionState('sess_contract_auth');
  const retry = await app.loadServerSessionState('sess_contract_retry');

  if (ok?.kind !== 'ok' || ok.state?.active_response_id !== 'resp_contract') {
    fail(name, 'successful state did not use the ok result contract', JSON.stringify(ok));
    return;
  }
  if (auth?.kind !== 'auth' || Object.prototype.hasOwnProperty.call(auth, 'state')) {
    fail(name, '401 did not use the distinct auth result contract', JSON.stringify(auth));
    return;
  }
  if (retry?.kind !== 'retry' || Object.prototype.hasOwnProperty.call(retry, 'state')) {
    fail(name, 'transient failure did not use the distinct retry result contract', JSON.stringify(retry));
    return;
  }
  if (authFailures !== 1) {
    fail(name, `expected one auth failure callback, got ${authFailures}`);
    return;
  }

  pass(name);
}

async function testSessionStatePollRetriesAfterTransientFailure() {
  const name = 'session state poll retries after transient failure';
  const scheduled = [];
  let stateCalls = 0;
  const { app } = await createSessionsHarness({
    setTimeout(fn, delay) {
      const handle = { fn, delay, cleared: false };
      scheduled.push(handle);
      return handle;
    },
    clearTimeout(handle) {
      if (handle) handle.cleared = true;
    },
    fetchImpl: async (url) => {
      if (url === '/ui/v1/sessions/sess_retry/state') {
        stateCalls += 1;
        if (stateCalls === 1) {
          return new Response('temporary upstream failure', { status: 502 });
        }
        return new Response(JSON.stringify({ active_run: false, active_response_id: '' }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });
  app.stopSidebarStatusPoll();
  scheduled.length = 0;

  const session = {
    id: 'sess_retry',
    title: 'Retry me',
    origin: 'web',
    created: 1,
    messages: [],
    activeResponseId: null,
    lastSequenceNumber: 0,
  };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = true;

  app.scheduleSessionStatePoll(session.id, 0);
  const first = scheduled.find((item) => item.delay === 0 && !item.cleared);
  if (!first) {
    fail(name, 'expected initial zero-delay state poll to be scheduled', JSON.stringify(scheduled.map(({ delay, cleared }) => ({ delay, cleared }))));
    return;
  }
  await first.fn();

  if (stateCalls !== 1) {
    fail(name, `expected one state endpoint fetch before retry, got ${stateCalls}`);
    return;
  }
  const retry = scheduled.find((item) => item !== first && item.delay === 5000 && !item.cleared);
  if (!retry) {
    fail(name, 'expected failed state poll to schedule a retry', JSON.stringify(scheduled.map(({ delay, cleared }) => ({ delay, cleared }))));
    return;
  }

  await retry.fn();
  if (stateCalls !== 2) {
    fail(name, `expected retry to fetch state endpoint again, got ${stateCalls}`);
    return;
  }
  const extraRetry = scheduled.find((item) => item !== retry && item.delay === 5000 && !item.cleared);
  if (extraRetry) {
    fail(name, 'expected successful retry to stop the transient retry loop', JSON.stringify(scheduled.map(({ delay, cleared }) => ({ delay, cleared }))));
    return;
  }

  pass(name);
}

async function testSessionStatePollTreats401AsAuthFailure() {
  const name = 'session state 401 triggers auth failure without retry';
  const scheduled = [];
  let authFailures = 0;
  let stateCalls = 0;
  const { app } = await createSessionsHarness({
    setTimeout(fn, delay) {
      const handle = { fn, delay, cleared: false };
      scheduled.push(handle);
      return handle;
    },
    clearTimeout(handle) {
      if (handle) handle.cleared = true;
    },
    appOverrides: {
      handleAuthFailure() { authFailures += 1; },
    },
    fetchImpl: async (url) => {
      if (url === '/ui/v1/sessions/sess_auth/state') {
        stateCalls += 1;
        return new Response('unauthorized', { status: 401 });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });
  app.stopSidebarStatusPoll();
  scheduled.length = 0;

  const session = { id: 'sess_auth', title: 'Auth', origin: 'web', created: 1, messages: [] };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = true;

  app.scheduleSessionStatePoll(session.id, 0);
  const first = scheduled.find((item) => item.delay === 0 && !item.cleared);
  if (!first) {
    fail(name, 'expected initial state poll');
    return;
  }
  await first.fn();

  if (stateCalls !== 1 || authFailures !== 1) {
    fail(name, `expected one request and one auth failure, got requests=${stateCalls} authFailures=${authFailures}`);
    return;
  }
  const retry = scheduled.find((item) => item !== first && item.delay === 5000 && !item.cleared);
  if (retry) {
    fail(name, '401 was treated as a transient failure and retried');
    return;
  }

  pass(name);
}

async function testSessionStatePollDoesNotRetryAfterSessionSwitch() {
  const name = 'session state poll does not retry after session switch';
  const scheduled = [];
  const { app } = await createSessionsHarness({
    setTimeout(fn, delay) {
      const handle = { fn, delay, cleared: false };
      scheduled.push(handle);
      return handle;
    },
    clearTimeout(handle) {
      if (handle) handle.cleared = true;
    },
    fetchImpl: async (url) => {
      if (url === '/ui/v1/sessions/sess_retry_switch/state') {
        app.state.activeSessionId = 'sess_other';
        return new Response('temporary upstream failure', { status: 502 });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });
  app.stopSidebarStatusPoll();
  scheduled.length = 0;

  const session = { id: 'sess_retry_switch', title: 'Retry switch', origin: 'web', created: 1, messages: [] };
  const other = { id: 'sess_other', title: 'Other', origin: 'web', created: 2, messages: [] };
  app.state.sessions = [session, other];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = true;

  app.scheduleSessionStatePoll(session.id, 0);
  const first = scheduled.find((item) => item.delay === 0 && !item.cleared);
  if (!first) {
    fail(name, 'expected initial zero-delay state poll to be scheduled', JSON.stringify(scheduled.map(({ delay, cleared }) => ({ delay, cleared }))));
    return;
  }
  await first.fn();

  const retry = scheduled.find((item) => item !== first && item.delay === 5000 && !item.cleared);
  if (retry) {
    fail(name, 'expected session switch to suppress transient retry', JSON.stringify(scheduled.map(({ delay, cleared }) => ({ delay, cleared }))));
    return;
  }

  pass(name);
}

async function testLateActiveRunSyncDoesNotMarkDraftStreaming() {
  const name = 'late active-run sync does not mark New Chat draft as streaming';
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (parsedTestURL(url)?.pathname === '/ui/v1/sessions') {
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

async function testAddMenuAttachOptionTriggersFileInput() {
  const name = 'plus menu attach option triggers file input';
  const { app } = await createSessionsHarness({
    fetchImpl: async () => new Response(JSON.stringify({ sessions: [] }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' }
    })
  });

  let clicks = 0;
  app.elements.fileInput.click = () => { clicks += 1; };
  app.elements.attachBtn.click();
  app.elements.addAttachOption.click();

  if (clicks !== 1) {
    fail(name, `file input clicks = ${clicks}, want 1`);
    return;
  }
  pass(name);
}

async function testAddMenuLocationAddsReviewableDraft() {
  const name = 'plus menu location action adds reviewable draft without sending';
  let requests = 0;
  let sends = 0;
  const { app } = await createSessionsHarness({
    geolocation: {
      getCurrentPosition(success, _error, options) {
        requests += 1;
        if (options.enableHighAccuracy !== false || options.timeout !== 12000) {
          throw new Error('unexpected geolocation options');
        }
        success({ coords: { latitude: -33.86882, longitude: 151.20929, accuracy: 24.6 } });
      }
    },
    appOverrides: { sendMessage() { sends += 1; } }
  });

  app.elements.promptInput.value = 'Find lunch nearby.';
  app.elements.attachBtn.click();
  app.elements.addLocationOption.click();

  const prompt = app.elements.promptInput.value;
  if (requests !== 1 || sends !== 0) {
    fail(name, `requests=${requests} sends=${sends}, want requests=1 sends=0`);
    return;
  }
  if (!prompt.includes('Find lunch nearby.\n\nMy current location:')
      || !prompt.includes('-33.86882, 151.20929')
      || !prompt.includes('approximately 25 m')
      || !prompt.includes('openstreetmap.org')) {
    fail(name, 'location draft missing expected reviewable content', prompt);
    return;
  }
  pass(name);
}

async function testLocationSharingCanBeDisabled() {
  const name = 'location sharing config hides plus menu action';
  const { app } = await createSessionsHarness({ locationSharingEnabled: false });
  if (!app.elements.addLocationOption.hidden) {
    fail(name, 'expected location action to be hidden');
    return;
  }
  pass(name);
}

async function testGoalStateResponseUpdatesChip() {
  const name = 'goal state response updates goal chip';
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      const parsed = parsedTestURL(url);
      if (parsed && parsed.pathname.endsWith('/state')) {
        return new Response(JSON.stringify({
          active_run: false,
          goal: {
            objective: 'ship the goal feature',
            status: 'active',
            token_budget: 100,
            tokens_used: 25
          }
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed && parsed.pathname.endsWith('/messages')) {
        return new Response(JSON.stringify({ messages: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed && (parsed.pathname === '/ui/v1/providers' || parsed.pathname === '/ui/v1/models')) {
        return new Response(JSON.stringify({ data: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });

  const session = app.createSession();
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;
  await app.syncActiveSessionFromServer(session, false, { skipMessagesFetch: true });

  if (!session.goal || session.goal.objective !== 'ship the goal feature') {
    fail(name, 'expected session goal to be stored from state response', JSON.stringify(session.goal));
    return;
  }
  if (String(app.elements.goalChip.className || '').includes('hidden')) {
    fail(name, 'expected goal chip to be visible', app.elements.goalChip.className);
    return;
  }
  const chipText = String(app.elements.goalChip.textContent || '');
  if (!chipText.includes('active') || !chipText.includes('25/100') || !chipText.includes('ship the goal feature')) {
    fail(name, 'unexpected goal chip text', chipText);
    return;
  }
  pass(name);
}

async function testGoalModalSavesGoal() {
  const name = 'goal modal saves goal through runtime endpoint';
  const posts = [];
  const { app } = await createSessionsHarness({
    fetchImpl: async (url, options = {}) => {
      const parsed = parsedTestURL(url);
      if (parsed && parsed.pathname.endsWith('/runtime/goal')) {
        posts.push({ pathname: parsed.pathname, body: JSON.parse(String(options.body || '{}')) });
        return new Response(JSON.stringify({
          goal: {
            objective: 'finish docs',
            status: 'active',
            token_budget: 123
          }
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed && (parsed.pathname === '/ui/v1/providers' || parsed.pathname === '/ui/v1/models')) {
        return new Response(JSON.stringify({ data: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });

  const session = app.createSession();
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;
  app.elements.goalModal.classList.add('hidden');

  app.elements.addGoalOption.click();
  if (app.elements.goalModal.classList.contains('hidden')) {
    fail(name, 'expected plus-menu goal option to open the modal');
    return;
  }
  app.elements.goalObjectiveInput.value = 'finish docs';
  app.elements.goalTokenBudgetInput.value = '123';
  await app.saveGoalFromModal();

  if (posts.length !== 1) {
    fail(name, `goal POST count = ${posts.length}, want 1`, JSON.stringify(posts));
    return;
  }
  if (posts[0].pathname !== `/ui/v1/sessions/${session.id}/runtime/goal`) {
    fail(name, 'unexpected goal endpoint path', JSON.stringify(posts[0]));
    return;
  }
  const wantBody = { action: 'set', objective: 'finish docs', token_budget: 123 };
  if (JSON.stringify(posts[0].body) !== JSON.stringify(wantBody)) {
    fail(name, 'unexpected goal POST body', JSON.stringify(posts[0].body));
    return;
  }
  if (!session.goal || session.goal.objective !== 'finish docs' || session.goal.token_budget !== 123) {
    fail(name, 'expected session goal to update from save response', JSON.stringify(session.goal));
    return;
  }
  if (String(app.elements.goalChip.textContent || '').indexOf('finish docs') < 0) {
    fail(name, 'expected chip to show saved objective', app.elements.goalChip.textContent);
    return;
  }
  pass(name);
}

async function testOpenMCPFromDraftCreatesSessionBeforeFetch() {
  const name = 'opening MCP from draft creates local session before fetch';
  const fetchCalls = [];
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      const parsed = parsedTestURL(url);
      fetchCalls.push(parsed ? parsed.pathname : String(url));
      if (parsed && parsed.pathname.endsWith('/mcp')) {
        return new Response(JSON.stringify({
          servers: [{ name: 'filesystem', configured: true, enabled: false, status: 'stopped', error: '', tools: 0 }],
          enabled: []
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed && (parsed.pathname === '/ui/v1/providers' || parsed.pathname === '/ui/v1/models')) {
        return new Response(JSON.stringify({ data: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });

  app.state.sessions = [];
  app.state.activeSessionId = '';
  app.state.draftSessionActive = true;

  await app.openSessionMCPModal();

  if (app.state.draftSessionActive || !app.state.activeSessionId || app.state.sessions.length !== 1) {
    fail(name, 'expected a local active session to be created', JSON.stringify({ active: app.state.activeSessionId, draft: app.state.draftSessionActive, sessions: app.state.sessions.length }));
    return;
  }
  const sessionId = app.state.activeSessionId;
  if (!fetchCalls.some((pathname) => pathname === `/ui/v1/sessions/${sessionId}/mcp`)) {
    fail(name, 'expected MCP endpoint to be fetched for the new session', JSON.stringify(fetchCalls));
    return;
  }
  pass(name);
}

async function testMCPStateResponseUpdatesHeaderPill() {
  const name = 'MCP state response updates header pill';
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      const parsed = parsedTestURL(url);
      if (parsed && parsed.pathname.endsWith('/state')) {
        return new Response(JSON.stringify({
          active_run: false,
          mcp_enabled: ['filesystem'],
          mcp_servers: [{ name: 'filesystem', configured: true, enabled: true, status: 'ready', error: '', tools: 3 }]
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed && parsed.pathname.endsWith('/messages')) {
        return new Response(JSON.stringify({ messages: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed && (parsed.pathname === '/ui/v1/providers' || parsed.pathname === '/ui/v1/models')) {
        return new Response(JSON.stringify({ data: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });

  const session = app.createSession();
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;
  await app.syncActiveSessionFromServer(session, false, { skipMessagesFetch: true });

  if (app.elements.mcpStatus.hidden || app.elements.mcpStatus.textContent !== 'MCP: 1') {
    fail(name, `mcpStatus=${JSON.stringify({ hidden: app.elements.mcpStatus.hidden, text: app.elements.mcpStatus.textContent })}`);
    return;
  }
  if (!String(app.elements.mcpStatus.title || '').includes('filesystem')) {
    fail(name, 'expected compact pill title to list enabled MCP', app.elements.mcpStatus.title);
    return;
  }
  pass(name);
}

async function testMCPHeaderPillOpensServersModal() {
  const name = 'MCP header pill opens servers modal';
  const mcpFetches = [];
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      const parsed = parsedTestURL(url);
      if (parsed && parsed.pathname.endsWith('/state')) {
        return new Response(JSON.stringify({
          active_run: false,
          mcp_enabled: ['filesystem'],
          mcp_servers: [{ name: 'filesystem', configured: true, enabled: true, status: 'ready', error: '', tools: 3 }]
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed && parsed.pathname.endsWith('/mcp')) {
        mcpFetches.push(parsed.pathname);
        return new Response(JSON.stringify({
          servers: [{ name: 'filesystem', configured: true, enabled: true, status: 'ready', error: '', tools: 3 }],
          enabled: ['filesystem']
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed && parsed.pathname.endsWith('/messages')) {
        return new Response(JSON.stringify({ messages: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed && (parsed.pathname === '/ui/v1/providers' || parsed.pathname === '/ui/v1/models')) {
        return new Response(JSON.stringify({ data: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });

  const session = app.createSession();
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;
  await app.syncActiveSessionFromServer(session, false, { skipMessagesFetch: true });

  app.elements.mcpStatus.click();
  await new Promise(resolve => setTimeout(resolve, 0));

  if (!mcpFetches.some((pathname) => pathname === `/ui/v1/sessions/${session.id}/mcp`)) {
    fail(name, 'expected compact MCP pill to fetch the MCP modal state', JSON.stringify(mcpFetches));
    return;
  }
  pass(name);
}

async function testMCPControlChangePatchesImmediately() {
  const name = 'MCP switch change patches immediately';
  const patchBodies = [];
  const { app } = await createSessionsHarness({
    fetchImpl: async (url, options = {}) => {
      const parsed = parsedTestURL(url);
      if (parsed && parsed.pathname.endsWith('/mcp') && options.method === 'PATCH') {
        patchBodies.push(JSON.parse(String(options.body || '{}')));
        return new Response(JSON.stringify({
          servers: [{ name: 'filesystem', configured: true, enabled: true, status: 'ready', error: '', tools: 2 }],
          enabled: ['filesystem']
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed && parsed.pathname.endsWith('/mcp')) {
        return new Response(JSON.stringify({
          servers: [{ name: 'filesystem', configured: true, enabled: false, status: 'stopped', error: '', tools: 0 }],
          enabled: []
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed && (parsed.pathname === '/ui/v1/providers' || parsed.pathname === '/ui/v1/models')) {
        return new Response(JSON.stringify({ data: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });

  const session = app.createSession();
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;
  await app.openSessionMCPModal();

  const inputs = app.elements.mcpModalBody.querySelectorAll('input[data-mcp-server]');
  if (inputs.length !== 1) {
    fail(name, 'expected one MCP switch', String(inputs.length));
    return;
  }
  inputs[0].checked = true;
  app.elements.mcpModalBody.dispatchEvent({ type: 'change', target: inputs[0] });
  await new Promise(resolve => setTimeout(resolve, 0));

  if (patchBodies.length !== 1 || JSON.stringify(patchBodies[0]) !== JSON.stringify({ enabled: ['filesystem'] })) {
    fail(name, 'expected one immediate PATCH with selected server', JSON.stringify(patchBodies));
    return;
  }
  if (JSON.stringify(session.mcpEnabled || []) !== JSON.stringify(['filesystem'])) {
    fail(name, 'expected session MCP state to update from PATCH response', JSON.stringify(session.mcpEnabled));
    return;
  }
  pass(name);
}

async function testMCPPatchConflictDoesNotOptimisticallyEnable() {
  const name = 'MCP PATCH 409 surfaces error without optimistic enable';
  const { app } = await createSessionsHarness({
    fetchImpl: async (url, options = {}) => {
      const parsed = parsedTestURL(url);
      if (parsed && parsed.pathname.endsWith('/mcp') && options.method === 'PATCH') {
        return new Response(JSON.stringify({ error: { message: 'cannot change MCP servers while a response is running' } }), {
          status: 409,
          headers: { 'Content-Type': 'application/json' }
        });
      }
      if (parsed && parsed.pathname.endsWith('/mcp')) {
        return new Response(JSON.stringify({
          servers: [{ name: 'filesystem', configured: true, enabled: false, status: 'stopped', error: '', tools: 0 }],
          enabled: []
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      if (parsed && (parsed.pathname === '/ui/v1/providers' || parsed.pathname === '/ui/v1/models')) {
        return new Response(JSON.stringify({ data: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });

  const session = app.createSession();
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;
  await app.fetchSessionMCP(session.id);
  const result = await app.applySessionMCP(session.id, ['filesystem']);

  if (result !== null) {
    fail(name, 'expected null result for conflict');
    return;
  }
  if ((session.mcpEnabled || []).length !== 0) {
    fail(name, 'expected session MCP enabled list to remain empty', JSON.stringify(session.mcpEnabled));
    return;
  }
  if (!String(app.elements.mcpError.textContent || '').includes('Cannot change MCPs')) {
    fail(name, 'expected modal error to mention running response', app.elements.mcpError.textContent);
    return;
  }
  pass(name);
}

function testSanitizeMessagePreservesSkillRunState() {
  const name = 'sanitizeMessage preserves isolated skill run state';
  return createSessionsHarness().then(({ app }) => {
    const sanitized = app.sanitizeMessage({
      id: 'skill-run-1', role: 'skill-run', runId: 'skill-1', sessionId: 'parent-1', skill: 'review', agent: 'reviewer',
      status: 'cancelling', progress: 'Stopping', output: 'partial', error: '', childSessionId: 'child-1', durationMs: 1200, created: 1000,
    });
    if (sanitized?.runId !== 'skill-1' || sanitized.status !== 'cancelling' || sanitized.output !== 'partial' || sanitized.childSessionId !== 'child-1') {
      fail(name, 'skill run state was discarded', JSON.stringify(sanitized));
      return;
    }
    pass(name);
  });
}

function testSanitizeMessagePreservesPlanExecutionEvidence() {
  const name = 'sanitizeMessage preserves failed and successful plan execution evidence';
  return createSessionsHarness().then(({ app }) => {
    const failed = app.sanitizeMessage({
      id: 'plan-group',
      role: 'tool-group',
      status: 'done',
      created: 1,
      tools: [{ id: 'plan-failed', name: 'update_plan', status: 'error', resultStatus: 'error', arguments: '{"plan":[]}', created: 1 }],
    });
    const succeeded = app.sanitizeMessage({
      id: 'plan-success', role: 'tool', name: 'update_plan', status: 'done', resultStatus: 'success', arguments: '{"plan":[]}', created: 1,
    });
    if (failed?.tools?.[0]?.status !== 'error' || failed.tools[0].resultStatus !== 'error' || succeeded?.resultStatus !== 'success') {
      fail(name, 'execution evidence was discarded', JSON.stringify({ failed, succeeded }));
      return;
    }
    pass(name);
  });
}

function testSkillProvenanceEventConvertsToLinkedRunBlock() {
  const name = 'skill provenance event converts to linked run block';
  return createSessionsHarness().then(({ app }) => {
    const converted = app.convertServerMessages([{
      id: 42,
      sequence: 8,
      role: 'event',
      created_at: Date.now(),
      parts: [
        {
          type: 'skill_activation',
          skill_activation: {
            name: 'review', execution: 'isolated', agent: 'reviewer', run_id: 'skill-1',
            child_session_id: 'child-1', status: 'complete', started_at: '2026-07-18T00:00:00Z', completed_at: '2026-07-18T00:00:02Z',
          },
        },
        { type: 'text', text: '↳ Skill /review · complete\n\nLooks good.' },
      ],
    }]);
    const message = converted[0];
    if (message?.role !== 'skill-run' || message.runId !== 'skill-1' || message.childSessionId !== 'child-1' || message.output !== 'Looks good.' || message.durationMs !== 2000) {
      fail(name, 'unexpected skill run conversion', JSON.stringify(converted));
      return;
    }
    pass(name);
  });
}

async function testSessionSwitchRefreshesSkillsAndDraftClearsThem() {
  const name = 'session switch refreshes skills and draft clears catalog';
  const refreshes = [];
  const { app } = await createSessionsHarness({
    appOverrides: {
      async refreshSkillCommands(sessionId) {
        refreshes.push(String(sessionId || ''));
      },
      async syncActiveSessionFromServer() {},
    },
  });
  const session = {
    id: 'session-skills',
    title: 'Skills',
    messages: [],
    activeResponseId: null,
    lastResponseId: null,
    lastSequenceNumber: 0,
  };
  app.state.sessions.push(session);

  await app.switchToSession(session.id, { closeSidebar: false });
  await app.switchToDraftSession({ closeSidebar: false });

  const tail = refreshes.slice(-2);
  if (JSON.stringify(tail) !== JSON.stringify([session.id, ''])) {
    fail(name, 'unexpected skill catalog refresh sequence', JSON.stringify(refreshes));
    return;
  }
  pass(name);
}

async function testOrdinaryStateSyncConvergesMissedPlanUpdate() {
  const name = 'ordinary session-state sync converges a missed plan completion event';
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      const parsed = parsedTestURL(url);
      if (parsed?.pathname === '/ui/v1/sessions/session-plan-poll/state') {
        return new Response(JSON.stringify({
          active_run: true,
          current_plan: { version: 3, steps: [{ step: 'Recovered by poll', status: 'in_progress' }] },
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
  });
  app.stopSidebarStatusPoll();
  const session = { id: 'session-plan-poll', title: 'Plan poll', messages: [], activeResponseId: null, lastSequenceNumber: 0 };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;
  app.resetCurrentPlanForSession(session.id);

  await app.syncActiveSessionFromServer(session, true, { skipMessagesFetch: true });
  if (app.state.currentPlan?.version !== 3 || app.state.currentPlan?.steps?.[0]?.step !== 'Recovered by poll') {
    fail(name, 'ordinary state sync did not apply authoritative plan', JSON.stringify(app.state.currentPlan));
    return;
  }
  pass(name);
}

async function testSessionStatePlanRequestsIgnoreOlderOverlaps() {
  const name = 'session plan state ignores older overlapping responses';
  const pending = [];
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      const parsed = parsedTestURL(url);
      if (parsed?.pathname === '/ui/v1/sessions/session-plan-race/state') {
        return new Promise((resolve) => pending.push(resolve));
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
  });
  app.stopSidebarStatusPoll();
  const session = { id: 'session-plan-race', title: 'Race', messages: [], activeResponseId: null, lastSequenceNumber: 0 };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;
  app.resetCurrentPlanForSession(session.id);

  const older = app.syncActiveSessionFromServer(session, false, { skipMessagesFetch: true });
  const newer = app.syncActiveSessionFromServer(session, false, { skipMessagesFetch: true });
  if (pending.length !== 2) {
    fail(name, `expected two overlapping state requests, got ${pending.length}`);
    return;
  }
  pending[1](new Response(JSON.stringify({
    active_run: true,
    current_plan: { version: 2, steps: [{ step: 'Newer', status: 'in_progress' }] },
  }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
  await newer;
  pending[0](new Response(JSON.stringify({ active_run: false, current_plan: null }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
  await older;

  if (app.state.currentPlan?.version !== 2 || app.state.currentPlan?.steps?.[0]?.step !== 'Newer') {
    fail(name, 'older authoritative clear overwrote newer plan', JSON.stringify(app.state.currentPlan));
    return;
  }
  pass(name);
}

async function testSessionStatePlanRequestsKeepNewerAuthoritativeClear() {
  const name = 'session plan state keeps a newer clear over an older present response';
  const pending = [];
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      const parsed = parsedTestURL(url);
      if (parsed?.pathname === '/ui/v1/sessions/session-plan-clear-race/state') {
        return new Promise((resolve) => pending.push(resolve));
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
  });
  app.stopSidebarStatusPoll();
  const session = { id: 'session-plan-clear-race', title: 'Clear race', messages: [], activeResponseId: null, lastSequenceNumber: 0 };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;
  app.resetCurrentPlanForSession(session.id);
  app.applyCurrentPlanState(session.id, { current_plan: { version: 9, steps: [{ step: 'Existing', status: 'in_progress' }] } });

  const older = app.syncActiveSessionFromServer(session, false, { skipMessagesFetch: true });
  const newer = app.refreshCurrentPlanFromServer(session);
  pending[1](new Response(JSON.stringify({ active_run: true, current_plan: null }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
  await newer;
  pending[0](new Response(JSON.stringify({
    active_run: true,
    current_plan: { version: 10, steps: [{ step: 'Stale present', status: 'completed' }] },
  }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
  await older;

  if (app.state.currentPlan !== null || !app.state.currentPlanInitialized) {
    fail(name, 'older present response overwrote newer authoritative clear', JSON.stringify(app.state.currentPlan));
    return;
  }
  pass(name);
}

async function testSessionSwitchClearsPlanBeforeRejectingOldResponse() {
  const name = 'session switch clears plan immediately and rejects previous session response';
  let resolveOldState;
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      const parsed = parsedTestURL(url);
      if (parsed?.pathname === '/ui/v1/sessions/session-old/state') {
        return new Promise((resolve) => { resolveOldState = resolve; });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
  });
  app.stopSidebarStatusPoll();
  const oldSession = { id: 'session-old', title: 'Old', messages: [], activeResponseId: null, lastSequenceNumber: 0 };
  const nextSession = { id: 'session-next', title: 'Next', messages: [], activeResponseId: null, lastSequenceNumber: 0 };
  app.state.sessions = [oldSession, nextSession];
  app.state.activeSessionId = oldSession.id;
  app.state.draftSessionActive = false;
  app.resetCurrentPlanForSession(oldSession.id);
  app.applyCurrentPlanState(oldSession.id, { current_plan: { version: 4, steps: [{ step: 'Old plan', status: 'in_progress' }] } });

  const oldRequest = app.syncActiveSessionFromServer(oldSession, false, { skipMessagesFetch: true });
  await app.switchToSession(nextSession.id, { sync: false, closeSidebar: false });
  if (app.state.currentPlan !== null || app.state.currentPlanSessionId !== nextSession.id || !app.elements.planToggleBtn.hidden) {
    fail(name, 'old plan remained visible during switch', JSON.stringify({ plan: app.state.currentPlan, owner: app.state.currentPlanSessionId }));
    return;
  }

  resolveOldState(new Response(JSON.stringify({
    active_run: false,
    current_plan: { version: 5, steps: [{ step: 'Late old plan', status: 'completed' }] },
  }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
  await oldRequest;
  if (app.state.currentPlan !== null || app.state.currentPlanSessionId !== nextSession.id) {
    fail(name, 'late response from previous session populated plan', JSON.stringify(app.state.currentPlan));
    return;
  }
  pass(name);
}

async function testSessionSwitchClearsProviderRetryOwner() {
  const name = 'session switching clears the previous provider retry owner';
  const clears = [];
  const { app } = await createSessionsHarness({
    fetchImpl: async () => new Response(JSON.stringify({ sessions: [] }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    }),
    appOverrides: {
      clearProviderRetryStatus(sessionId, responseId) {
        clears.push({ sessionId, responseId });
        return true;
      },
    },
  });
  app.stopSidebarStatusPoll();
  const oldSession = { id: 'session-retry-old', title: 'Old', messages: [], activeResponseId: 'resp-retry-old', lastSequenceNumber: 2 };
  const nextSession = { id: 'session-retry-next', title: 'Next', messages: [], activeResponseId: null, lastSequenceNumber: 0 };
  app.state.sessions = [oldSession, nextSession];
  app.state.activeSessionId = oldSession.id;
  app.state.draftSessionActive = false;
  clears.length = 0;

  await app.switchToSession(nextSession.id, { sync: false, closeSidebar: false });

  if (!clears.some((entry) => entry.sessionId === oldSession.id && entry.responseId === oldSession.activeResponseId)) {
    fail(name, 'previous session retry owner was not cleared', JSON.stringify(clears));
    return;
  }
  pass(name);
}

async function testStoppedRunReconciliationClearsProviderRetryOwner() {
  const name = 'stopped-run reconciliation clears matching provider retry owner';
  const clears = [];
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (url === '/ui/v1/sessions/session-retry-stopped/state') {
        return new Response(JSON.stringify({ active_run: false, active_response_id: '' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      return new Response(JSON.stringify({ sessions: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    },
    appOverrides: {
      clearProviderRetryStatus(sessionId, responseId) {
        clears.push({ sessionId, responseId });
        return true;
      },
    },
  });
  app.stopSidebarStatusPoll();
  const session = {
    id: 'session-retry-stopped',
    title: 'Stopped',
    messages: [],
    activeResponseId: 'resp-retry-stopped',
    lastSequenceNumber: 4,
  };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;
  app.state.abortController = null;
  clears.length = 0;

  await app.syncActiveSessionFromServer(session, false, { skipMessagesFetch: true });

  const matchingClears = clears.filter((entry) => (
    entry.sessionId === session.id && entry.responseId === 'resp-retry-stopped'
  ));
  if (matchingClears.length !== 1) {
    fail(name, 'stopped run should clear its retry owner once through active-response tracking', JSON.stringify(clears));
    return;
  }
  pass(name);
}

async function testSessionPruningDestroysTranscriptStores() {
  const name = 'session pruning destroys transcript stores for removed sessions';
  const { app } = await createSessionsHarness();
  const destroyed = [];
  app.state.sessions = Array.from({ length: 101 }, (_, index) => ({
    id: `prune-${index}`,
    created: index,
    messages: [],
    transcript: { destroy() { destroyed.push(index); } }
  }));
  app.state.activeSessionId = 'prune-100';
  app.state.draftSessionActive = false;
  app.saveSessions();
  if (app.state.sessions.length !== 100 || destroyed.length !== 1 || destroyed[0] !== 0) {
    fail(name, 'removed session retained its transcript store', JSON.stringify({ count: app.state.sessions.length, destroyed }));
    return;
  }
  pass(name);
}

async function testOptimisticTranscriptStorageIsPerSession() {
  const name = 'optimistic transcript storage keeps independent per-session entries';
  const { app, storage } = await createSessionsHarness();
  const first = { id: 'optimistic-a', messages: [] };
  const second = { id: 'optimistic-b', messages: [] };
  app.trackTranscriptOptimistic(first, { id: 'local-a', clientKey: 'send-a', role: 'user', content: 'a' });
  app.trackTranscriptOptimistic(second, { id: 'local-b', clientKey: 'send-b', role: 'user', content: 'b' });
  app.trackTranscriptOptimistic(first, { id: 'guardian-a', clientKey: 'guardian-a', role: 'event', content: 'review' });

  const saved = JSON.parse(storage.get('term_llm_optimistic_transcript') || 'null');
  const firstEntries = saved?.sessions?.[first.id] || [];
  const secondEntries = saved?.sessions?.[second.id] || [];
  if (firstEntries.length !== 1 || firstEntries[0].clientKey !== 'send-a' || secondEntries.length !== 1 || secondEntries[0].clientKey !== 'send-b') {
    fail(name, 'one session clobbered another or persisted a display-only row', JSON.stringify(saved));
    return;
  }
  pass(name);
}

async function testReloadMaterializesPersistedOptimisticToolTurnsBeforeTerminalReconciliation() {
  const name = 'reload reconciles matched persisted tools and retires unmatched completed tool UI at the terminal revision';
  const sessionId = 'stale-tool-reload';
  const storageKey = 'term_llm_optimistic_transcript';
  let nextID = 201;
  let nextSeq = 10;
  const message = (role, parts) => ({
    id: nextID++,
    sequence: nextSeq++,
    role,
    created_at: nextSeq * 1000,
    parts
  });
  const staleTurn = [
    message('user', [{ type: 'text', text: 'run the old tool' }]),
    message('assistant', [{ type: 'tool_call', tool_name: 'read_file', tool_call_id: 'stale-call' }]),
    message('tool', [{ type: 'tool_result', tool_name: 'read_file', tool_call_id: 'stale-call' }]),
    message('assistant', [{ type: 'text', text: 'Old turn done.' }])
  ];
  const turns = [staleTurn];
  for (let index = 0; index < 72; index += 1) {
    turns.push([
      message('user', [{ type: 'text', text: `filler ${index}` }]),
      message('assistant', [{ type: 'text', text: `answer ${index}` }])
    ]);
  }
  const unrelatedTurn = [
    message('user', [{ type: 'text', text: 'run a later tool' }]),
    message('assistant', [{ type: 'tool_call', tool_name: 'grep', tool_call_id: 'never-durable-call' }]),
    message('tool', [{ type: 'tool_result', tool_name: 'grep', tool_call_id: 'never-durable-call' }]),
    message('assistant', [{ type: 'text', text: 'Later turn done.' }])
  ];
  turns.push(unrelatedTurn);
  const raw = turns.flat();
  const persistedEntries = [
    {
      id: 'local-stale-tool-group',
      clientKey: 'local-stale-tool-group',
      role: 'tool-group',
      status: 'done',
      tools: [{ id: 'stale-call', name: 'read_file', status: 'done' }],
      revAtSend: 5,
      durableSeqAtSend: staleTurn[0].sequence,
      optimistic: true
    },
    {
      id: 'local-unmatched-tool-group',
      clientKey: 'local-unmatched-tool-group',
      role: 'tool-group',
      status: 'done',
      tools: [{ id: 'never-durable-call', name: 'write_file', status: 'done' }],
      revAtSend: 5,
      durableSeqAtSend: staleTurn[staleTurn.length - 1].sequence,
      optimistic: true
    },
    {
      id: 'local-queued-user',
      clientKey: 'local-queued-user',
      role: 'user',
      content: 'queued after the active run',
      revAtSend: 5,
      durableSeqAtSend: raw[raw.length - 1].sequence,
      optimistic: true
    }
  ];
  const persisted = JSON.stringify({
    version: 1,
    sessions: { [sessionId]: persistedEntries }
  });
  const bodyRequests = [];
  const targetStatesAtFetch = [];
  let indexRequests = 0;
  let primedMaterializationBudget = false;
  let session = null;
  const { app, storage } = await createSessionsHarness({
    initialStorage: { [storageKey]: persisted },
    fetchImpl: async (url) => {
      if (isTranscriptIndexURL(url, sessionId)) {
        indexRequests += 1;
        if (indexRequests > 1) {
          return new Response(null, { status: 304, headers: { ETag: '"stale-tool-rev-8"' } });
        }
        return new Response(JSON.stringify({
          rev: 8,
          compaction_seq: -1,
          compaction_count: 0,
          rows: {
            ids: raw.map((entry) => entry.id),
            seqs: raw.map((entry) => entry.sequence),
            roles: raw.map((entry) => ({ user: 'u', assistant: 'a', tool: 't' })[entry.role]).join(''),
            flags: raw.map(() => 0)
          }
        }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"stale-tool-rev-8"' } });
      }
      if (isTranscriptBodiesURL(url, sessionId)) {
        targetStatesAtFetch.push(session?.transcript?.segments?.[0]?.state || '');
        if (!primedMaterializationBudget) {
          primedMaterializationBudget = true;
          session.transcript.materialize(turns.slice(-60).flat(), { countFetch: false });
        }
        const requested = (parsedTestURL(url)?.searchParams.get('ids') || '')
          .split(',')
          .filter(Boolean)
          .map(Number);
        bodyRequests.push(requested);
        const requestedAnchors = new Set(requested);
        const messages = turns
          .filter((turn) => requestedAnchors.has(turn[0].id))
          .flat();
        return new Response(JSON.stringify({ rev: 8, messages }), {
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
  session = { id: sessionId, messages: [] };
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;

  const loaded = await app.syncTranscript(session, { reason: 'reload' });
  const requested = bodyRequests[0] || [];
  if (!loaded || targetStatesAtFetch[0] !== 'evicted') {
    fail(name, 'stale turn was not initially evicted before body materialization', JSON.stringify({ loaded, targetStatesAtFetch }));
    return;
  }
  if (turns.length <= 60) {
    fail(name, 'regression transcript did not exceed the materialized-turn budget', String(turns.length));
    return;
  }
  if (!requested.includes(staleTurn[0].id) || !requested.includes(unrelatedTurn[0].id)) {
    fail(name, 'reload did not fetch both the stale original turn and the pinned later turn', JSON.stringify(bodyRequests));
    return;
  }
  if (requested.length > 32 || requested.length >= turns.length) {
    fail(name, 'reload body fetch was not bounded to selected turn anchors', JSON.stringify({ requested: requested.length, turns: turns.length }));
    return;
  }
  let optimisticKeys = session.transcript.optimistic.map((entry) => entry.clientKey);
  if (JSON.stringify(optimisticKeys) !== JSON.stringify(['local-queued-user'])) {
    fail(name, '200 terminal reconciliation retained stale completed tool UI or dropped the queued user', JSON.stringify(optimisticKeys));
    return;
  }
  let saved = JSON.parse(storage.get(storageKey) || 'null');
  let savedKeys = (saved?.sessions?.[sessionId] || []).map((entry) => entry.clientKey);
  if (JSON.stringify(savedKeys) !== JSON.stringify(['local-queued-user'])) {
    fail(name, '200 terminal reconciliation was not persisted without the stale tool tail', JSON.stringify(saved));
    return;
  }
  if (session.transcript.segments[0]?.state !== 'evicted'
      || session.transcript.segments.filter((segment) => segment.state === 'materialized').length > 60) {
    fail(name, 'post-reconciliation budget did not evict the distant target turn', JSON.stringify({
      targetState: session.transcript.segments[0]?.state || 'missing',
      materialized: session.transcript.segments.filter((segment) => segment.state === 'materialized').length
    }));
    return;
  }

  session.transcript.addOptimistic({
    id: 'local-304-tool-group',
    clientKey: 'local-304-tool-group',
    role: 'tool-group',
    status: 'done',
    tools: [{ id: 'stale-call', name: 'read_file', status: 'done' }],
    revAtSend: 5,
    durableSeqAtSend: staleTurn[0].sequence,
    optimistic: true
  }, 5, { persisted: true });
  app.persistTranscriptOptimistic(session);

  const loadedNotModified = await app.syncTranscript(session, { reason: 'not-modified' });
  optimisticKeys = session.transcript.optimistic.map((entry) => entry.clientKey);
  if (!loadedNotModified || targetStatesAtFetch[1] !== 'evicted' || !bodyRequests[1]?.includes(staleTurn[0].id)) {
    fail(name, '304 sync did not rematerialize the evicted reconciliation target', JSON.stringify({ loadedNotModified, targetStatesAtFetch, bodyRequests }));
    return;
  }
  if (JSON.stringify(optimisticKeys) !== JSON.stringify(['local-queued-user'])) {
    fail(name, '304 reconciliation retained completed tool UI or dropped the queued user', JSON.stringify(optimisticKeys));
    return;
  }
  saved = JSON.parse(storage.get(storageKey) || 'null');
  savedKeys = (saved?.sessions?.[sessionId] || []).map((entry) => entry.clientKey);
  if (JSON.stringify(savedKeys) !== JSON.stringify(['local-queued-user'])) {
    fail(name, '304 reconciliation persistence did not retain only the queued user', JSON.stringify(saved));
    return;
  }
  const finalMaterialized = session.transcript.segments.filter((segment) => segment.state === 'materialized').length;
  if (session.transcript.segments[0]?.state !== 'evicted' || finalMaterialized > 60) {
    fail(name, '304 sync did not enforce the budget after reconciliation', JSON.stringify({
      targetState: session.transcript.segments[0]?.state || 'missing',
      materialized: finalMaterialized
    }));
    return;
  }
  pass(name);
}

async function testGroupedToolRowsPreserveDurableRangesAndLaterAnchors() {
  const name = 'grouped tool conversion preserves source range and later durable anchors';
  const { app, windowObj } = await createSessionsHarness();
  const raw = [
    { id: 101, sequence: 1, role: 'user', created_at: 1000, parts: [{ type: 'text', text: 'run it' }] },
    { id: 102, sequence: 2, role: 'assistant', created_at: 2000, parts: [{ type: 'tool_call', tool_name: 'read_file', tool_call_id: 'call-1' }] },
    { id: 103, sequence: 3, role: 'tool', created_at: 3000, parts: [{ type: 'tool_result', tool_name: 'read_file', tool_call_id: 'call-1' }] },
    { id: 104, sequence: 4, role: 'assistant', created_at: 4000, parts: [{ type: 'text', text: 'done' }] },
  ];
  const session = { id: 'durable-groups', messages: [] };
  session.transcript = new windowObj.TranscriptStore(session.id);
  session.transcript.applyIndex({
    rev: 4,
    compaction_seq: -1,
    compaction_count: 0,
    rows: { ids: [101, 102, 103, 104], seqs: [1, 2, 3, 4], roles: 'uata', flags: [0, 0, 2, 0] }
  });
  session.transcript.materialize(raw);
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;
  app.refreshSessionMessagesFromTranscript(session);

  const group = session.messages.find((message) => message.role === 'tool-group');
  const later = session.messages.find((message) => message.role === 'assistant' && message.content === 'done');
  const anchorIDs = session.messages.map((message) => message.durableRowId).filter((id) => id != null);
  if (!group || group.durableRowStartId !== 102 || group.durableRowEndId !== 103) {
    fail(name, 'tool group did not retain its complete durable source range', JSON.stringify(session.messages));
    return;
  }
  if (!later || later.durableRowId !== 104) {
    fail(name, 'later assistant inherited the grouped tool result row ID', JSON.stringify(session.messages));
    return;
  }
  if (new Set(anchorIDs).size !== anchorIDs.length) {
    fail(name, 'DOM durable anchors are not unique', JSON.stringify(anchorIDs));
    return;
  }
  pass(name);
}

async function testLargeToolTurnLoadsAsOneSegmentWithFarEndGrouping() {
  const name = '700 tool rows load atomically as one turn with far-end grouping context';
  const toolCalls = 350;
  const raw = [{
    id: 1,
    sequence: 0,
    role: 'user',
    created_at: 1000,
    parts: [{ type: 'text', text: 'run many tools' }]
  }];
  for (let index = 0; index < toolCalls; index += 1) {
    const callID = `call-${index}`;
    const toolName = index === toolCalls - 1 ? 'update_plan' : 'read_file';
    const failed = index === toolCalls - 1;
    raw.push({
      id: raw.length + 1,
      sequence: raw.length,
      role: 'assistant',
      created_at: 1001 + raw.length,
      parts: [{
        type: 'tool_call',
        tool_name: toolName,
        tool_call_id: callID,
        tool_arguments: '{}',
        tool_error: failed
      }]
    });
    raw.push({
      id: raw.length + 1,
      sequence: raw.length,
      role: 'tool',
      created_at: 1001 + raw.length,
      parts: [{
        type: 'tool_result',
        tool_name: toolName,
        tool_call_id: callID,
        tool_error: failed
      }]
    });
  }
  const requestedAnchors = [];
  const { app, windowObj } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (isTranscriptBodiesURL(url, 'large-tool-turn')) {
        const parsed = parsedTestURL(url);
        requestedAnchors.push(...String(parsed.searchParams.get('ids') || '').split(',').filter(Boolean).map(Number));
        return new Response(JSON.stringify({ rev: raw.length, messages: raw }), {
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
  app.stopSidebarStatusPoll();
  const session = { id: 'large-tool-turn', messages: [] };
  session.transcript = new windowObj.TranscriptStore(session.id, { maxMaterializedTurns: 60, overscanTurns: 0 });
  session.transcript.applyIndex({
    rev: raw.length,
    compaction_seq: -1,
    compaction_count: 0,
    rows: {
      ids: raw.map((entry) => entry.id),
      seqs: raw.map((entry) => entry.sequence),
      roles: `u${'at'.repeat(toolCalls)}`,
      flags: raw.map(() => 0)
    }
  });
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;

  const loaded = await app.materializeTranscriptSegments(session, [0]);
  const materialized = session.transcript.segments.filter((segment) => segment.state === 'materialized');
  const group = session.messages.find((message) => message.role === 'tool-group');
  const farTool = group?.tools?.find((tool) => tool.id === `call-${toolCalls - 1}`);
  if (!loaded || requestedAnchors.length !== 1 || requestedAnchors[0] !== 1) {
    fail(name, 'client did not request exactly one turn anchor', JSON.stringify({ loaded, requestedAnchors }));
    return;
  }
  if (session.transcript.segments.length !== 1 || materialized.length !== 1 || session.transcript.bodies.size !== raw.length) {
    fail(name, 'giant turn was split, rejected, or budgeted by durable rows', JSON.stringify({
      segments: session.transcript.segments.length,
      materialized: materialized.length,
      bodies: session.transcript.bodies.size
    }));
    return;
  }
  if (!group || group.tools.length !== toolCalls || group.durableRowEndId !== raw[raw.length - 1].id) {
    fail(name, 'tool conversion lost full-turn context at the far end', JSON.stringify(group));
    return;
  }
  if (!farTool || farTool.name !== 'update_plan' || farTool.status !== 'error' || farTool.resultStatus !== 'error') {
    fail(name, 'far-end update_plan result/error semantics were not preserved', JSON.stringify(farTool));
    return;
  }
  pass(name);
}

async function testTerminalTranscriptSyncQueuesBehindInflightRequest() {
  const name = 'terminal transcript sync follows an in-flight request until final revision';
  let resolveFirst;
  const transcriptRequests = [];
  const { app, windowObj } = await createSessionsHarness({
    fetchImpl: async (url, options = {}) => {
      if (isTranscriptIndexURL(url, 'sync-race')) {
        transcriptRequests.push({ url: String(url), headers: options.headers || {} });
        if (transcriptRequests.length === 1) {
          return new Promise((resolve) => { resolveFirst = resolve; });
        }
        return new Response(JSON.stringify({
          rev: 2, compaction_seq: -1, compaction_count: 0,
          rows: { ids: [], seqs: [], roles: '', flags: [] }
        }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"rev-2"' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });
  const session = { id: 'sync-race', messages: [] };
  session.transcript = new windowObj.TranscriptStore(session.id);
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;

  const ordinary = app.syncTranscript(session, { reason: 'ordinary' });
  while (typeof resolveFirst !== 'function') await new Promise((resolve) => setTimeout(resolve, 0));
  const terminal = app.noteTranscriptTerminal(session, 2);
  resolveFirst(new Response(JSON.stringify({
    rev: 1, compaction_seq: -1, compaction_count: 0,
    rows: { ids: [], seqs: [], roles: '', flags: [] }
  }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"rev-1"' } }));

  const [ordinaryResult, terminalResult] = await Promise.all([ordinary, terminal]);
  if (!ordinaryResult || !terminalResult || session.transcript.rev !== 2 || transcriptRequests.length !== 2) {
    fail(name, 'coalescing swallowed the forced final revision sync', JSON.stringify({ ordinaryResult, terminalResult, rev: session.transcript.rev, requests: transcriptRequests.length }));
    return;
  }
  if (Object.keys(transcriptRequests[1].headers).some((key) => key.toLowerCase() === 'if-none-match')) {
    fail(name, 'queued terminal follow-up was not forced', JSON.stringify(transcriptRequests[1].headers));
    return;
  }
  pass(name);
}

async function testSatisfiedTerminalSyncDoesNotRefetchTranscript() {
  const name = 'terminal force with a satisfied positive target is absorbed in flight';
  let resolveFirst;
  let indexRequests = 0;
  const { app, windowObj } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (isTranscriptIndexURL(url, 'satisfied-terminal-sync')) {
        indexRequests += 1;
        if (indexRequests === 1) {
          return new Promise((resolve) => { resolveFirst = resolve; });
        }
        return new Response(null, { status: 304, headers: { ETag: '"terminal-2"' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });
  const session = { id: 'satisfied-terminal-sync', messages: [] };
  session.transcript = new windowObj.TranscriptStore(session.id);
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;

  const activation = app.syncTranscript(session, { reason: 'activation' });
  while (typeof resolveFirst !== 'function') await new Promise((resolve) => setTimeout(resolve, 0));
  const terminal = app.noteTranscriptTerminal(session, 2);
  resolveFirst(new Response(JSON.stringify({
    rev: 2, compaction_seq: -1, compaction_count: 0,
    rows: { ids: [], seqs: [], roles: '', flags: [] }
  }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"terminal-2"' } }));

  const [activationLoaded, terminalLoaded] = await Promise.all([activation, terminal]);
  if (!activationLoaded || !terminalLoaded || session.transcript.rev !== 2) {
    fail(name, 'the completed request did not satisfy the terminal target', JSON.stringify({ activationLoaded, terminalLoaded, rev: session.transcript.rev }));
    return;
  }
  if (indexRequests !== 1) {
    fail(name, `satisfied terminal target caused ${indexRequests} transcript index requests`);
    return;
  }
  pass(name);
}

async function testUntargetedForceQueuesBehindInflightRequest() {
  const name = 'force without a positive target still follows an in-flight request';
  let resolveFirst;
  let indexRequests = 0;
  const { app, windowObj } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (isTranscriptIndexURL(url, 'untargeted-force-sync')) {
        indexRequests += 1;
        if (indexRequests === 1) {
          return new Promise((resolve) => { resolveFirst = resolve; });
        }
        return new Response(JSON.stringify({
          rev: 1, compaction_seq: -1, compaction_count: 0,
          rows: { ids: [], seqs: [], roles: '', flags: [] }
        }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"untargeted-1"' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });
  const session = { id: 'untargeted-force-sync', messages: [] };
  session.transcript = new windowObj.TranscriptStore(session.id);
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;

  const activation = app.syncTranscript(session, { reason: 'activation' });
  while (typeof resolveFirst !== 'function') await new Promise((resolve) => setTimeout(resolve, 0));
  const staleBodies = app.syncTranscript(session, { reason: 'stale-bodies', force: true, targetRev: 0 });
  resolveFirst(new Response(JSON.stringify({
    rev: 1, compaction_seq: -1, compaction_count: 0,
    rows: { ids: [], seqs: [], roles: '', flags: [] }
  }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"untargeted-1"' } }));

  const [activationLoaded, staleBodiesLoaded] = await Promise.all([activation, staleBodies]);
  if (!activationLoaded || !staleBodiesLoaded || indexRequests !== 2) {
    fail(name, 'untargeted force was incorrectly absorbed', JSON.stringify({ activationLoaded, staleBodiesLoaded, indexRequests }));
    return;
  }
  pass(name);
}

async function testForceScrollQueuesBehindSatisfiedInflightRequest() {
  const name = 'forceScroll still follows an in-flight request with a satisfied target';
  let resolveFirst;
  let indexRequests = 0;
  const { app, windowObj } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (isTranscriptIndexURL(url, 'force-scroll-sync')) {
        indexRequests += 1;
        if (indexRequests === 1) {
          return new Promise((resolve) => { resolveFirst = resolve; });
        }
        return new Response(null, { status: 304, headers: { ETag: '"force-scroll-1"' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });
  const session = { id: 'force-scroll-sync', messages: [] };
  session.transcript = new windowObj.TranscriptStore(session.id);
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;

  const activation = app.syncTranscript(session, { reason: 'activation' });
  while (typeof resolveFirst !== 'function') await new Promise((resolve) => setTimeout(resolve, 0));
  const forceScroll = app.syncTranscript(session, { reason: 'terminal-view', forceScroll: true, targetRev: 1 });
  resolveFirst(new Response(JSON.stringify({
    rev: 1, compaction_seq: -1, compaction_count: 0,
    rows: { ids: [], seqs: [], roles: '', flags: [] }
  }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"force-scroll-1"' } }));

  const [activationLoaded, forceScrollLoaded] = await Promise.all([activation, forceScroll]);
  if (!activationLoaded || !forceScrollLoaded || indexRequests !== 2) {
    fail(name, 'forceScroll request was incorrectly absorbed', JSON.stringify({ activationLoaded, forceScrollLoaded, indexRequests }));
    return;
  }
  pass(name);
}

async function testActiveStatusDefersInRunTranscriptRevisionsUntilTerminal() {
  const name = 'active status attaches at started_rev without reconciling in-run revisions';
  const fetchCalls = [];
  const { app, windowObj } = await createSessionsHarness({
    fetchImpl: async (url) => {
      fetchCalls.push(String(url));
      if (isTranscriptIndexURL(url, 'sess_active_rev')) {
        return new Response(JSON.stringify({
          rev: 9,
          compaction_seq: -1,
          compaction_count: 0,
          rows: { ids: [], seqs: [], roles: '', flags: [] }
        }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"active-9"' } });
      }
      if (parsedTestURL(url)?.pathname === '/ui/v1/sessions') {
        return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
  });
  const session = { id: 'sess_active_rev', messages: [], activeResponseId: null, lastSequenceNumber: 0 };
  session.transcript = new windowObj.TranscriptStore(session.id);
  session.transcript.applyIndex({ rev: 5, compaction_seq: -1, compaction_count: 0, rows: { ids: [], seqs: [], roles: '', flags: [] } });
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.draftSessionActive = false;
  app.state.streaming = true;
  fetchCalls.length = 0;

  await app.reconcileTranscriptFromStatus([{
    id: session.id,
    active_response_id: 'resp_active',
    started_rev: 5,
    transcript_rev: 9,
    active_run: true
  }]);
  if (fetchCalls.some((url) => isTranscriptIndexURL(url, session.id))) {
    fail(name, 'status poll reconciled rows persisted after started_rev while the run was active', JSON.stringify(fetchCalls));
    return;
  }
  if (session.transcript.activeRun?.startedRev !== 5) {
    fail(name, 'active run attachment did not retain started_rev', JSON.stringify(session.transcript.activeRun));
    return;
  }
  pass(name);
}

async function testHugeTranscriptGapTraversalStaysBoundedAndAnchored() {
  const name = 'huge transcript gaps materialize incrementally while traversal stays bounded and anchored';
  const rowCount = 5200;
  const materializeBudget = 60;
  const fetchBatchSizes = [];
  let appRef = null;
  let durableNodes = [];
  let absoluteTops = new Map();
  let renderedDescriptorCount = 0;

  const renderTranscriptDOM = () => {
    if (!appRef) return;
    const session = appRef.state.sessions.find((item) => item?.id === appRef.state.activeSessionId);
    const transcript = session?.transcript;
    if (!transcript) return;
    const messages = appRef.elements.messages;
    const chatScroll = appRef.elements.chatScroll;
    const topLevel = [];
    durableNodes = [];
    absoluteTops = new Map();
    let top = 0;
    for (const run of transcript.renderRuns()) {
      const node = makeNode(run.type === 'gap' ? 'div' : 'section');
      if (run.type === 'gap') {
        node.classList.add('transcript-gap');
        node.dataset.startSegmentIndex = String(run.startSegmentIndex);
        node.dataset.endSegmentIndex = String(run.endSegmentIndex);
        top += run.height;
      } else {
        node.classList.add('transcript-turn');
        node.dataset.segmentIndex = String(run.segmentIndex);
        const height = 52 + (run.segmentIndex % 3) * 7;
        for (let ordinal = run.startOrdinal; ordinal <= run.endOrdinal; ordinal += 1) {
          if (!transcript.bodies.has(transcript.ids[ordinal])) continue;
          const durable = makeNode('article');
          const nodeTop = top;
          durable.dataset.durableId = String(transcript.ids[ordinal]);
          durable.getBoundingClientRect = () => ({
            top: nodeTop - (Number(chatScroll.scrollTop) || 0),
            bottom: nodeTop + height - (Number(chatScroll.scrollTop) || 0)
          });
          node.appendChild(durable);
          durableNodes.push(durable);
          absoluteTops.set(transcript.ids[ordinal], nodeTop);
        }
        top += height;
      }
      topLevel.push(node);
    }
    messages.replaceChildren(...topLevel);
    renderedDescriptorCount = topLevel.length;
    chatScroll.scrollHeight = top;
  };

  const { app, windowObj } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (isTranscriptBodiesURL(url, 'sess_huge_scroll')) {
        const parsed = parsedTestURL(url);
        const ids = String(parsed.searchParams.get('ids') || '').split(',').filter(Boolean).map(Number);
        fetchBatchSizes.push(ids.length);
        if (ids.length > 32) {
          return new Response(JSON.stringify({ error: { message: 'client batch exceeded test cap' } }), {
            status: 413,
            headers: { 'Content-Type': 'application/json' }
          });
        }
        return new Response(JSON.stringify({
          rev: 1,
          messages: ids.map((id) => ({
            id,
            sequence: id - 1,
            role: 'user',
            created_at: 1710000000000 + id,
            parts: [{ type: 'text', text: `row-${id}` }]
          }))
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' }
      });
    },
    appOverrides: {
      renderMessages() { renderTranscriptDOM(); }
    }
  });
  appRef = app;
  const messages = app.elements.messages;
  const chatScroll = app.elements.chatScroll;
  chatScroll.clientHeight = 400;
  chatScroll.scrollTop = 0;
  chatScroll.getBoundingClientRect = () => ({ top: 0, bottom: chatScroll.clientHeight });
  messages.querySelectorAll = (selector) => selector === '[data-durable-id]' ? durableNodes.slice() : [];
  messages.querySelector = (selector) => {
    const match = String(selector).match(/^\[data-durable-id="([^"]+)"\]$/);
    return match ? durableNodes.find((node) => node.dataset.durableId === match[1]) || null : null;
  };

  const session = { id: 'sess_huge_scroll', messages: [] };
  const transcript = new windowObj.TranscriptStore(session.id, { maxMaterializedTurns: materializeBudget, overscanTurns: 8 });
  const ids = Array.from({ length: rowCount }, (_, index) => index + 1);
  transcript.applyIndex({
    rev: 1,
    compaction_seq: -1,
    compaction_count: 0,
    rows: { ids, seqs: ids.map((id) => id - 1), roles: 'u'.repeat(rowCount), flags: ids.map(() => 0) }
  });
  transcript.setViewport(rowCount - 1, rowCount - 1);
  transcript.materialize(ids.slice(-materializeBudget).map((id) => ({
    id,
    sequence: id - 1,
    role: 'user',
    parts: [{ type: 'text', text: `row-${id}` }]
  })), { countFetch: false });
  session.transcript = transcript;
  app.state.sessions = [session];
  app.state.activeSessionId = session.id;
  app.state.autoScroll = false;
  app.refreshSessionMessagesFromTranscript(session);
  renderTranscriptDOM();

  let previousFirst = rowCount - materializeBudget;
  for (let step = 0; step < 40; step += 1) {
    const gap = transcript.renderRuns()
      .filter((run) => run.type === 'gap' && run.endSegmentIndex < previousFirst)
      .sort((a, b) => b.endSegmentIndex - a.endSegmentIndex)[0];
    if (!gap) {
      fail(name, `lost the next coalesced gap at traversal step ${step}`);
      return;
    }
    const anchorID = gap.endOrdinal + 2;
    renderTranscriptDOM();
    const anchorAbsoluteTop = absoluteTops.get(anchorID);
    if (!Number.isFinite(anchorAbsoluteTop)) {
      fail(name, `missing materialized anchor row ${anchorID} at traversal step ${step}`);
      return;
    }
    chatScroll.scrollTop = anchorAbsoluteTop - 100;
    const anchorNode = messages.querySelector(`[data-durable-id="${anchorID}"]`);
    const beforeTop = anchorNode?.getBoundingClientRect?.().top;

    const loaded = await app.materializeTranscriptSegments(session, {
      startSegmentIndex: gap.startSegmentIndex,
      endSegmentIndex: gap.endSegmentIndex,
      targetOrdinal: gap.endOrdinal,
      direction: 'backward'
    });
    if (!loaded) {
      fail(name, `bounded gap materialization failed at traversal step ${step}`);
      return;
    }
    if (transcript.viewport.lastOrdinal !== gap.endOrdinal
      || transcript.viewport.lastOrdinal - transcript.viewport.firstOrdinal + 1 > 32) {
      fail(name, `viewport did not follow the bounded batch at traversal step ${step}`, JSON.stringify(transcript.viewport));
      return;
    }

    const afterNode = messages.querySelector(`[data-durable-id="${anchorID}"]`);
    const afterTop = afterNode?.getBoundingClientRect?.().top;
    if (!Number.isFinite(beforeTop) || !Number.isFinite(afterTop) || Math.abs(afterTop - beforeTop) > 1) {
      fail(name, `anchored row ${anchorID} jumped during fill ${step}`, `before=${beforeTop} after=${afterTop}`);
      return;
    }
    const materialized = transcript.segments.filter((segment) => segment.state === 'materialized');
    const pinnedMaterialized = [...transcript.pinnedSegments].filter((index) => transcript.segments[index]?.state === 'materialized').length;
    const allowedMaterialized = Math.max(materializeBudget, pinnedMaterialized);
    if (materialized.length > allowedMaterialized || transcript.bodies.size > allowedMaterialized) {
      fail(name, `materialized transcript exceeded turn budget plus pinned exceptions at step ${step}`, `turns=${materialized.length} pinned=${pinnedMaterialized} bodies=${transcript.bodies.size}`);
      return;
    }
    if (session.messages.length > materializeBudget + 3 || renderedDescriptorCount > materializeBudget + 3 || messages.children.length > materializeBudget + 3) {
      fail(name, `rendered transcript exceeded sparse DOM bounds at step ${step}`, `messages=${session.messages.length} descriptors=${renderedDescriptorCount} DOM=${messages.children.length}`);
      return;
    }
    const first = Math.min(...materialized.map((segment) => segment.startOrdinal));
    if (!(first < previousFirst)) {
      fail(name, `traversal made no progress at step ${step}`, `first=${first} previous=${previousFirst}`);
      return;
    }
    previousFirst = first;
  }

  if (fetchBatchSizes.length !== 40 || fetchBatchSizes.some((size) => size < 1 || size > 32)) {
    fail(name, 'body fetch batches were not uniformly bounded', JSON.stringify(fetchBatchSizes));
    return;
  }
  if (transcript.bodies.has(rowCount)) {
    fail(name, 'distant initial tail body was not evicted while traversing older batches');
    return;
  }
  if (transcript.stats.evictions === 0 || previousFirst >= rowCount - materializeBudget - 32 * 30) {
    fail(name, 'multi-batch traversal did not progressively load and evict distant turns', `first=${previousFirst} evictions=${transcript.stats.evictions}`);
    return;
  }
  transcript._checkInvariants();
  pass(name);
}

async function testTranscriptSyncCommitsOneRenderedProjection() {
  const name = 'transcript sync commits one rendered projection after index and bodies';
  const sessionId = 'single-render-sync';
  let renders = 0;
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (isTranscriptIndexURL(url, sessionId)) {
        return new Response(JSON.stringify({
          rev: 2,
          compaction_seq: -1,
          compaction_count: 0,
          rows: { ids: [101, 102], seqs: [0, 1], roles: 'ua', flags: [0, 0] }
        }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"single-render-2"' } });
      }
      if (isTranscriptBodiesURL(url, sessionId)) {
        return new Response(JSON.stringify({
          rev: 2,
          messages: [
            { id: 101, sequence: 0, role: 'user', created_at: 1000, parts: [{ type: 'text', text: 'question' }] },
            { id: 102, sequence: 1, role: 'assistant', created_at: 2000, parts: [{ type: 'text', text: 'answer' }] }
          ]
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' }
      });
    },
    appOverrides: {
      renderMessages() { renders += 1; }
    }
  });
  app.stopSidebarStatusPoll();
  const session = { id: sessionId, messages: [] };
  app.state.sessions = [session];
  app.state.activeSessionId = sessionId;
  app.state.draftSessionActive = false;
  renders = 0;

  const loaded = await app.syncTranscript(session, { reason: 'activation' });
  if (!loaded || session.messages.map((message) => message.content).join(',') !== 'question,answer') {
    fail(name, 'sync did not commit the complete projected transcript', JSON.stringify({ loaded, messages: session.messages }));
    return;
  }
  if (renders !== 1) {
    fail(name, `sync rendered ${renders} intermediate projections instead of one`);
    return;
  }
  pass(name);
}

async function testSatisfiedInflightRevisionDoesNotRefetchTranscript() {
  const name = 'in-flight sync absorbs forced active-status target satisfied by its response';
  const sessionId = 'coalesced-revision-sync';
  let indexRequests = 0;
  let releaseFirstIndex;
  let markFirstIndexStarted;
  const firstIndexStarted = new Promise((resolve) => { markFirstIndexStarted = resolve; });
  const firstIndexGate = new Promise((resolve) => { releaseFirstIndex = resolve; });
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (isTranscriptIndexURL(url, sessionId)) {
        indexRequests += 1;
        if (indexRequests === 1) {
          markFirstIndexStarted();
          await firstIndexGate;
          return new Response(JSON.stringify({
            rev: 2,
            compaction_seq: -1,
            compaction_count: 0,
            rows: { ids: [201, 202], seqs: [0, 1], roles: 'ua', flags: [0, 0] }
          }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"coalesced-2"' } });
        }
        return new Response(null, { status: 304, headers: { ETag: '"coalesced-2"' } });
      }
      if (isTranscriptBodiesURL(url, sessionId)) {
        return new Response(JSON.stringify({
          rev: 2,
          messages: [
            { id: 201, sequence: 0, role: 'user', created_at: 1000, parts: [{ type: 'text', text: 'question' }] },
            { id: 202, sequence: 1, role: 'assistant', created_at: 2000, parts: [{ type: 'text', text: 'answer' }] }
          ]
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' }
      });
    }
  });
  app.stopSidebarStatusPoll();
  const session = { id: sessionId, messages: [] };
  app.state.sessions = [session];
  app.state.activeSessionId = sessionId;
  app.state.draftSessionActive = false;
  app.state.streaming = true;

  const activation = app.syncTranscript(session, { reason: 'activation' });
  await firstIndexStarted;
  const statusSync = app.reconcileTranscriptFromStatus([{
    id: sessionId,
    transcript_rev: 9,
    active_response_id: 'resp_inflight',
    started_rev: 2,
    active_run: true
  }]);
  releaseFirstIndex();
  const [loaded] = await Promise.all([activation, statusSync]);
  if (!loaded || session.transcript.rev !== 2) {
    fail(name, 'the first response did not satisfy the queued target revision', JSON.stringify({ loaded, rev: session.transcript.rev }));
    return;
  }
  if (indexRequests !== 1) {
    fail(name, `satisfied revision caused ${indexRequests} transcript index requests`);
    return;
  }
  pass(name);
}

async function testInflightSyncAbsorbsQueuedZeroRevisionActivation() {
  const name = 'in-flight sync absorbs queued non-forced targetRev zero activation';
  const sessionId = 'coalesced-zero-revision-sync';
  let indexRequests = 0;
  let releaseFirstIndex;
  let markFirstIndexStarted;
  const firstIndexStarted = new Promise((resolve) => { markFirstIndexStarted = resolve; });
  const firstIndexGate = new Promise((resolve) => { releaseFirstIndex = resolve; });
  const { app } = await createSessionsHarness({
    fetchImpl: async (url) => {
      if (isTranscriptIndexURL(url, sessionId)) {
        indexRequests += 1;
        if (indexRequests === 1) {
          markFirstIndexStarted();
          await firstIndexGate;
          return new Response(JSON.stringify({
            rev: 1,
            compaction_seq: -1,
            compaction_count: 0,
            rows: { ids: [], seqs: [], roles: '', flags: [] }
          }), { status: 200, headers: { 'Content-Type': 'application/json', ETag: '"coalesced-zero-1"' } });
        }
        return new Response(null, { status: 304, headers: { ETag: '"coalesced-zero-1"' } });
      }
      return new Response(JSON.stringify({ sessions: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' }
      });
    }
  });
  app.stopSidebarStatusPoll();
  const session = { id: sessionId, messages: [] };
  app.state.sessions = [session];
  app.state.activeSessionId = sessionId;
  app.state.draftSessionActive = false;

  const stateSync = app.syncTranscript(session, { reason: 'state', targetRev: 0 });
  await firstIndexStarted;
  const activationSync = app.syncTranscript(session, { reason: 'activation', targetRev: 0 });
  releaseFirstIndex();
  const [stateLoaded, activationLoaded] = await Promise.all([stateSync, activationSync]);

  if (!stateLoaded || !activationLoaded || session.transcript.rev !== 1) {
    fail(name, 'the completed request did not satisfy both zero-revision callers', JSON.stringify({ stateLoaded, activationLoaded, rev: session.transcript.rev }));
    return;
  }
  if (indexRequests !== 1) {
    fail(name, `queued targetRev=0 activation caused ${indexRequests} transcript index requests`);
    return;
  }
  pass(name);
}

(async () => {
  await testSanitizeMessagePreservesSkillRunState();
  await testSanitizeMessagePreservesPlanExecutionEvidence();
  await testSkillProvenanceEventConvertsToLinkedRunBlock();
  await testSessionSwitchRefreshesSkillsAndDraftClearsThem();
  await testOrdinaryStateSyncConvergesMissedPlanUpdate();
  await testSessionStatePlanRequestsIgnoreOlderOverlaps();
  await testSessionStatePlanRequestsKeepNewerAuthoritativeClear();
  await testSessionSwitchClearsPlanBeforeRejectingOldResponse();
  await testSessionSwitchClearsProviderRetryOwner();
  await testStoppedRunReconciliationClearsProviderRetryOwner();
  await testSwitchingSessionsStagesCurrentComposerBeforeRestore();
  await testSwitchingSessionsClearsEmptyComposerDraft();
  await testNewChatClearsExistingDraftComposer();
  await testNewChatFromSessionPreservesSessionDraft();
  await testArchivingActiveSessionClearsItsComposerDraft();
  await testSwitchingSessionsDiscardsPendingAttachments();
  await testSwitchToSessionSyncsSelectedRuntime();
  await testNumericDeepLinkResolvesRealSessionId();
  await testIdleStartupSchedulesNormalPollWithoutImmediateRequest();
  await testStartupTranscriptSideloadRendersBoundedFirstPaintWithoutTranscriptRequests();
  await testSelectedTranscriptSideloadReconcilesStableToolCallsAcrossSwitchBack();
  await testMalformedStartupTranscriptSideloadFallsBack();
  await testStartupTranscriptSideloadRevisionRaceFetchesOnlyNewerRevision();
  await testStartupActiveSideloadAvoidsDuplicateStateAfterScheduledStatus();
  await testSidebarSwitchAppliesSelectedTranscriptBeforeAsyncStateAndSkills();
  await testRapidSidebarSwitchRejectsStaleSelectedTranscriptAndState();
  await testRapidSidebarSwitchRejectsLateRuntimeResponse();
  await testSidebarSwitchMalformedSideloadFallsBackExactlyOnce();
  await testSidebarSwitchActiveSideloadReconcilesNewerStartedRevision();
  await testSidebarSwitchSkipsLoadedAndUnresolvedSessionRequests();
  await testStartupSplashWaitsForDeepLinkedTranscriptRender();
  await testStartupSideloadsWidgetsBeforeFirstPaint();
  await testStartupSideloadsEmptyWidgetList();
  await testExplicitWidgetRefreshStillUsesStatusEndpoint();
  await testStartupHydrationTimeoutDoesNotBlockIndefinitely();
  await testUnresolvedNumericDeepLinkSkipsSessionScopedStartupRequests();
  await testNewQueryStartsDraftInsteadOfLastSession();
  await testNewQueryRefreshesHeaderAfterRuntimeMetadataLoads();
  await testMergeServerSessionsMigratesInterruptBuffersToRealSessionId();
  await testDeveloperMessagesAreHidden();
  await testRunErrorEventsConvertToErrorMessages();
  await testConvertServerMessagesCompactionSummariesBecomeMarkers();
  await testConvertServerMessagesSuppressesCompactionRetainedRawTail();
  await testCompactionDuplicateTailRangeIsLinear();
  await testConvertServerMessagesSuppressesAuthoritativeCompactionTailFlag();
  await testConvertServerMessagesHandlesMixedLegacyAndAuthoritativeCompactionTails();
  await testConvertServerMessagesInsertsBoundaryWhenSummaryNotLoaded();
  await testConvertServerMessagesAttachesToolResultImages();
  await testConvertServerMessagesAttachesToolErrorsWithoutPhantoms();
  await testConvertServerMessagesCorrelatesSuccessfulPlanResults();
  await testConvertServerMessagesRebasesHubImageURLs();
  await testConvertServerMessagesSuppressesNonBubbleAssistantRows();
  await testSessionPruningDestroysTranscriptStores();
  await testOptimisticTranscriptStorageIsPerSession();
  await testReloadMaterializesPersistedOptimisticToolTurnsBeforeTerminalReconciliation();
  await testTranscriptSyncCommitsOneRenderedProjection();
  await testSatisfiedInflightRevisionDoesNotRefetchTranscript();
  await testInflightSyncAbsorbsQueuedZeroRevisionActivation();
  await testGroupedToolRowsPreserveDurableRangesAndLaterAnchors();
  await testLargeToolTurnLoadsAsOneSegmentWithFarEndGrouping();
  await testTerminalTranscriptSyncQueuesBehindInflightRequest();
  await testSatisfiedTerminalSyncDoesNotRefetchTranscript();
  await testUntargetedForceQueuesBehindInflightRequest();
  await testForceScrollQueuesBehindSatisfiedInflightRequest();
  await testSwitchToSessionSyncsWithoutTokenAndResumes();
  await testSwitchToSessionAttachesChangedActiveResponseFromStartedRevision();
  await testActiveStatusDefersInRunTranscriptRevisionsUntilTerminal();
  await testHugeTranscriptGapTraversalStaysBoundedAndAnchored();
  await testIdleSessionSyncRescuesPendingInterruptCommit();
  await testSessionProgressStatePrefersLocalAndServerSignals();
  await testResumeAndDrainFiringViaSync();
  await testTerminalSyncRequeuesPendingInterjectionAsFollowUp();
  await testSyncUsesServerProvidedPendingInterjectionId();
  await testApplyServerSessionSummaryMapsLastMessageAt();
  await testSanitizeSessionPreservesLastMessageAt();
  await testPreConnectedVisibilityDoesNotStartSidebarStatusPoll();
  await testPageshowWaitsForInitialConnectionBeforeStatusPoll();
  await testSidebarStatusPollRecoversIdempotentlyAfterPageShow();
  await testHiddenInFlightSidebarPollCannotRescheduleOrApplyStaleStatus();
  await testReconnectBackoffWakeSignalsReuseExistingLoop();
  await testLoadServerSessionStateUsesExplicitResultContract();
  await testSessionStatePollRetriesAfterTransientFailure();
  await testSessionStatePollTreats401AsAuthFailure();
  await testSessionStatePollDoesNotRetryAfterSessionSwitch();
  await testLateActiveRunSyncDoesNotMarkDraftStreaming();
  await testAddMenuAttachOptionTriggersFileInput();
  await testAddMenuLocationAddsReviewableDraft();
  await testLocationSharingCanBeDisabled();
  await testGoalStateResponseUpdatesChip();
  await testGoalModalSavesGoal();
  await testOpenMCPFromDraftCreatesSessionBeforeFetch();
  await testMCPStateResponseUpdatesHeaderPill();
  await testMCPHeaderPillOpensServersModal();
  await testMCPControlChangePatchesImmediately();
  await testMCPPatchConflictDoesNotOptimisticallyEnable();

  if (failures > 0) process.exit(1);
  process.exit(0);
})();
