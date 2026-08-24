#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const dir = __dirname;
const source = fs.readFileSync(path.join(dir, 'app-core.js'), 'utf8');
const networkSource = fs.readFileSync(path.join(dir, 'app-network.js'), 'utf8');
const composerSource = fs.readFileSync(path.join(dir, 'app-composer.js'), 'utf8');

let failures = 0;
const pendingAsyncTests = [];

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
    add() {},
    remove() {},
    toggle() { return false; },
    contains() { return false; },
  };
}

function makeNode() {
  return {
    classList: makeClassList(),
    style: {},
    dataset: {},
    value: '',
    textContent: '',
    innerHTML: '',
    checked: false,
    options: [],
    scrollTop: 0,
    scrollHeight: 0,
    clientHeight: 0,
    appendChild(node) { return node; },
    removeChild() {},
    remove() {},
    querySelector() { return null; },
    querySelectorAll() { return []; },
    setAttribute() {},
    removeAttribute() {},
    addEventListener() {},
    removeEventListener() {},
    focus() {},
    select() {},
    setSelectionRange() {},
    closest() { return null; },
    getBoundingClientRect() {
      return { top: 0, left: 0, width: 0, height: 0, bottom: 0, right: 0 };
    },
    cloneNode() { return makeNode(); },
    play() { return Promise.resolve(); },
    pause() {},
  };
}

function loadAppCoreWith({ nodeOverrides = {}, docQSTracker = () => [], documentOverrides = {}, navigatorOverrides = {}, windowOverrides = {}, contextOverrides = {}, initialStorage = {}, agentName = '', agentNames = [], uiTitle = '', hub = null, now = () => Date.now(), timerOverrides = {}, loadComposer = false } = {}) {
  const nodes = new Map(Object.entries(nodeOverrides));
  const cookieWrites = [];
  const document = {
    title: 'Chat',
    body: makeNode(),
    documentElement: makeNode(),
    get cookie() { return cookieWrites[cookieWrites.length - 1] || ''; },
    set cookie(value) { cookieWrites.push(String(value)); },
    getElementById(id) {
      if (!nodes.has(id)) nodes.set(id, makeNode());
      return nodes.get(id);
    },
    createElement() { return makeNode(); },
    querySelector() { return null; },
    querySelectorAll: docQSTracker,
    addEventListener() {},
    removeEventListener() {},
  };

  Object.assign(document, documentOverrides);

  const storage = new Map(Object.entries(initialStorage).map(([key, value]) => [String(key), String(value)]));
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

  const navigatorObj = {
    mediaDevices: null,
    serviceWorker: {
      register: async () => ({ scope: '/chat/' }),
      ready: Promise.resolve({ showNotification: async () => {} }),
    },
    clipboard: { writeText: async () => {} },
    standalone: false,
    ...navigatorOverrides,
  };

  const windowObj = {
    TermLLMApp: {},
    TERM_LLM_UI_PREFIX: '/chat',
    TERM_LLM_SIDEBAR_SESSIONS: 'all',
    TERM_LLM_AGENT_NAME: agentName,
    TERM_LLM_AGENT_NAMES: agentNames,
    TERM_LLM_UI_TITLE: uiTitle,
    TERM_LLM_HUB: hub,
    navigator: navigatorObj,
    visualViewport: null,
    innerHeight: 1000,
    addEventListener() {},
    removeEventListener() {},
    matchMedia() {
      return { matches: false, addEventListener() {}, removeEventListener() {} };
    },
    requestAnimationFrame(fn) { return 1; },
    cancelAnimationFrame() {},
    setTimeout: timerOverrides.setTimeout || function setTimeoutStub(fn) { return 1; },
    clearTimeout: timerOverrides.clearTimeout || function clearTimeoutStub() {},
    location: { pathname: '/chat', href: 'http://localhost/chat', origin: 'http://localhost', protocol: 'http:', host: 'localhost' },
    history: { pushState() {} },
    MediaRecorder: undefined,
    focus() {},
    ...windowOverrides,
  };

  const DateShim = class extends Date {
    static now() { return now(); }
  };

  const context = {
    window: windowObj,
    document,
    localStorage,
    navigator: navigatorObj,
    Notification: undefined,
    history: windowObj.history,
    location: windowObj.location,
    renderMathInElement() {},
    crypto: { randomUUID: () => 'uuid-test' },
    URL,
    URLSearchParams,
    console,
    setTimeout,
    clearTimeout,
    Date: DateShim,
    TextEncoder,
    TextDecoder,
    Headers,
    Request,
    Response,
    AbortController,
    ReadableStream,
    ...contextOverrides,
  };
  context.globalThis = context;

  vm.runInNewContext(source, context, { filename: 'app-core.js' });
  vm.runInNewContext(networkSource, context, { filename: 'app-network.js' });
  if (loadComposer) vm.runInNewContext(composerSource, context, { filename: 'app-composer.js' });
  context.window.TermLLMApp.__testCookieWrites = cookieWrites;
  context.window.TermLLMApp.__testDocument = document;
  return context.window.TermLLMApp;
}

function loadAppCore() {
  return loadAppCoreWith();
}

(function testAgentSelectionDefaultsToServerBehaviorAndRemembersPreviousAgent() {
  const name = 'new conversation agent selection defaults to server behavior and remembers previous agent';
  const defaultApp = loadAppCoreWith({ agentNames: ['developer', 'reviewer'] });
  const rememberedApp = loadAppCoreWith({
    agentNames: ['developer', 'reviewer'],
    initialStorage: { term_llm_selected_agent: 'reviewer' },
  });
  const staleApp = loadAppCoreWith({
    agentNames: ['developer', 'reviewer'],
    initialStorage: { term_llm_selected_agent: 'removed-agent' },
  });
  if (defaultApp.state.selectedAgent !== ''
      || rememberedApp.state.selectedAgent !== 'reviewer'
      || staleApp.state.selectedAgent !== '') {
    fail(name, `unexpected selections: ${defaultApp.state.selectedAgent}, ${rememberedApp.state.selectedAgent}, ${staleApp.state.selectedAgent}`);
    return;
  }
  pass(name);
})();

const app = loadAppCore();

pendingAsyncTests.push((async function testHardRefreshClearsShellCachesAndUpdatesRegistration() {
  const name = 'hard refresh clears shell caches, updates registration, and reloads';
  const deleted = [];
  let updates = 0;
  let reloads = 0;
  const refreshApp = loadAppCoreWith({
    windowOverrides: {
      location: { pathname: '/chat', href: 'http://localhost/chat', origin: 'http://localhost', protocol: 'http:', host: 'localhost', reload() { reloads += 1; } },
    },
    contextOverrides: {
      caches: {
        async keys() { return ['term-llm-shell-v3', 'term-llm-shell-v4', 'unrelated-cache']; },
        async delete(key) { deleted.push(key); return true; },
      },
    },
  });
  refreshApp.state.serviceWorkerRegistration = { async update() { updates += 1; } };
  await refreshApp.hardRefreshUIAssets();
  if (deleted.length !== 2 || !deleted.includes('term-llm-shell-v3') || !deleted.includes('term-llm-shell-v4')) {
    fail(name, 'did not delete exactly the term-llm shell caches', JSON.stringify(deleted));
    return;
  }
  if (updates !== 1 || reloads !== 1) {
    fail(name, `updates=${updates} reloads=${reloads}`);
    return;
  }
  pass(name);
})());

(function testProjectDraftSendButtonExplainsInvalidBinding() {
  const name = 'project draft Send is disabled with an actionable binding reason';
  const composerApp = loadAppCoreWith({ loadComposer: true });
  composerApp.state.projectsEnabled = true;
  composerApp.state.draftSessionActive = true;
  composerApp.state.capabilitiesRequired = true;
  composerApp.state.capabilitiesLoaded = true;
  composerApp.state.activeProjectId = '';
  composerApp.elements.promptInput.value = 'hello';
  composerApp.updateSendButtonState();
  if (composerApp.elements.sendBtn.disabled) {
    fail(name, 'explicit No project draft remained disabled');
    return;
  }
  composerApp.state.activeProjectId = 'prj_missing';
  composerApp.updateSendButtonState();
  if (!composerApp.elements.sendBtn.disabled || composerApp.elements.sendBtn.title !== 'Choose a project before sending') {
    fail(name, 'unbound draft remained apparently sendable');
    return;
  }
  composerApp.state.activeProjectId = 'prj_valid';
  composerApp.state.projects = [{ id: 'prj_valid', available: true, archived_at: null }];
  composerApp.updateSendButtonState();
  if (composerApp.elements.sendBtn.disabled) {
    fail(name, 'valid project draft remained disabled');
    return;
  }
  composerApp.state.projects[0].available = false;
  composerApp.state.projects[0].unavailable_reason = 'Directory identity changed';
  composerApp.updateSendButtonState();
  if (!composerApp.elements.sendBtn.disabled || composerApp.elements.sendBtn.title !== 'Directory identity changed') {
    fail(name, 'unavailable draft did not expose its blocking reason');
    return;
  }
  pass(name);
})();

(function testCreateElInitializesOptionalProperties() {
  const name = 'createEl initializes only supplied class and text';
  const complete = app.createEl('span', 'status', 'ready');
  const bare = app.createEl('div');
  if (complete.className !== 'status' || complete.textContent !== 'ready') {
    fail(name, 'supplied properties were not assigned');
    return;
  }
  if (Object.prototype.hasOwnProperty.call(bare, 'className') || bare.textContent !== '') {
    fail(name, 'omitted properties should retain element defaults');
    return;
  }
  pass(name);
})();

(function testConversationMountGuardsAndScopedMessageLookup() {
  const name = 'conversation DOM helpers require mounted session ownership';
  const messages = makeNode();
  messages.dataset = { sessionId: 'session_a' };
  const nodes = [
    { dataset: { messageId: 'msg_a', sessionId: 'session_a' } },
    { dataset: { messageId: 'msg_b', sessionId: 'session_b' } },
  ];
  messages.querySelector = (selector) => {
    const match = String(selector || '').match(/\[data-message-id="([^"]+)"\]/);
    const id = match ? match[1] : '';
    return nodes.find((node) => node.dataset.messageId === id) || null;
  };

  const testApp = loadAppCoreWith({ nodeOverrides: { messages } });
  testApp.state.activeSessionId = 'session_a';
  testApp.state.draftSessionActive = false;

  if (testApp.mountedConversationSessionId() !== 'session_a') {
    fail(name, `mounted id = ${JSON.stringify(testApp.mountedConversationSessionId())}`);
    return;
  }
  if (!testApp.isConversationMounted('session_a') || testApp.conversationDOMFor('session_a') !== messages) {
    fail(name, 'expected session_a to own mounted conversation');
    return;
  }
  if (testApp.isConversationMounted('session_b') || testApp.conversationDOMFor('session_b') !== null) {
    fail(name, 'session_b must not own mounted conversation');
    return;
  }
  if (testApp.findMessageElement('msg_a') !== nodes[0]) {
    fail(name, 'expected same-session message lookup');
    return;
  }
  if (testApp.findMessageElement('msg_b') !== null) {
    fail(name, 'foreign stamped message should be rejected');
    return;
  }
  if (testApp.findMessageElement('msg_a', 'session_b') !== null) {
    fail(name, 'session-scoped lookup should reject unmounted owner');
    return;
  }

  messages.dataset.sessionId = 'session_b';
  if (testApp.isConversationMounted('session_a') || testApp.findMessageElement('msg_a') !== null) {
    fail(name, 'stale container session id should reject active-session DOM work');
    return;
  }

  pass(name);
})();

pendingAsyncTests.push((async function testClipboardWriterFallsBackToExecCommand() {
  const name = 'clipboard writer falls back to execCommand on insecure origins';
  let execCommandValue = '';
  let restoredFocus = false;
  const activeElement = Object.assign(makeNode(), { focus() { restoredFocus = true; } });
  const testApp = loadAppCoreWith({
    navigatorOverrides: { clipboard: undefined },
    documentOverrides: {
      activeElement,
      createElement(tagName) {
        const node = makeNode();
        node.tagName = String(tagName || '').toUpperCase();
        node.select = function select() { execCommandValue = this.value; };
        return node;
      },
      execCommand(command) {
        return command === 'copy' && execCommandValue === 'fallback text';
      },
    },
  });

  const writer = testApp.getClipboardWriter();
  if (!writer || typeof writer.writeText !== 'function') {
    fail(name, 'expected fallback clipboard writer');
    return;
  }
  await writer.writeText('fallback text').catch((err) => {
    fail(name, err && err.message ? err.message : String(err));
  });
  if (execCommandValue !== 'fallback text') {
    fail(name, `copied ${JSON.stringify(execCommandValue)}`);
    return;
  }
  if (!restoredFocus) {
    fail(name, 'expected focus to be restored after fallback copy');
    return;
  }
  pass(name);
})());

pendingAsyncTests.push((async function testExternalAuth401UsesTopLevelLogin() {
  const name = 'external auth 401 redirects through a trusted login URL';
  let redirectedTo = '';
  const location = {
    pathname: '/chat', href: 'https://example.test/chat/', origin: 'https://example.test',
    protocol: 'https:', host: 'example.test',
    assign(url) { redirectedTo = String(url); },
  };
  const testApp = loadAppCoreWith({
    windowOverrides: {
      location,
      fetch: async () => new Response('', {
        status: 401,
        headers: { 'X-Term-LLM-Login-URL': '/jarvis-api/auth/google?source=web' },
      }),
    },
  });

  await testApp.apiFetch('/chat/v1/providers', {}, { retries: 0 });
  if (redirectedTo !== 'https://example.test/jarvis-api/auth/google?source=web') {
    fail(name, `redirected to ${JSON.stringify(redirectedTo)}`);
    return;
  }
  pass(name);
})());

pendingAsyncTests.push((async function testExternalAuth401RejectsUnsafeLoginURL() {
  const name = 'external auth redirect rejects unsafe URL schemes';
  let redirects = 0;
  const location = {
    pathname: '/chat', href: 'https://example.test/chat/', origin: 'https://example.test',
    protocol: 'https:', host: 'example.test',
    assign() { redirects += 1; },
  };
  const testApp = loadAppCoreWith({
    windowOverrides: {
      location,
      fetch: async () => new Response('', {
        status: 401,
        headers: { 'X-Term-LLM-Login-URL': 'javascript:alert(1)' },
      }),
    },
  });

  await testApp.apiFetch('/chat/v1/providers', {}, { retries: 0 });
  if (redirects !== 0) {
    fail(name, 'unsafe login URL triggered navigation');
    return;
  }
  pass(name);
})());

(function testTokenCookieScopedToBasePathForWidgetsAndImages() {
  const name = 'syncTokenCookie scopes auth cookie to UI base path';
  const testApp = loadAppCore();

  testApp.syncTokenCookie('sec ret/val=');

  const writes = testApp.__testCookieWrites;
  const finalWrite = writes[writes.length - 1] || '';
  if (!finalWrite.includes('term_llm_token=sec%20ret%2Fval%3D; path=/chat;')) {
    fail(name, `got final cookie write ${JSON.stringify(finalWrite)}`);
    return;
  }
  if (finalWrite.includes('/images')) {
    fail(name, `cookie should not be scoped only to images: ${JSON.stringify(finalWrite)}`);
    return;
  }
  pass(name);
})();

(function testTokenCookieClearsLegacyImagesPath() {
  const name = 'syncTokenCookie clears legacy images-scoped cookie';
  const testApp = loadAppCore();

  testApp.syncTokenCookie('secret');

  const writes = testApp.__testCookieWrites;
  if (!writes.some((write) => write === 'term_llm_token=; path=/chat/images; SameSite=Strict; max-age=0')) {
    fail(name, `missing legacy clear write in ${JSON.stringify(writes)}`);
    return;
  }
  pass(name);
})();

(function testInitialTokenCookieUsesBasePath() {
  const name = 'initial token cookie uses UI base path';
  const testApp = loadAppCoreWith({ initialStorage: { term_llm_token: 'initial-token' } });
  const writes = testApp.__testCookieWrites;
  const finalWrite = writes[writes.length - 1] || '';
  if (finalWrite !== 'term_llm_token=initial-token; path=/chat; SameSite=Strict; max-age=31536000') {
    fail(name, `got ${JSON.stringify(finalWrite)}`);
    return;
  }
  pass(name);
})();

(function testDirectHubAwareNodeKeepsUnscopedStorage() {
  const name = 'direct Hub-aware node keeps direct storage and token';
  const testApp = loadAppCoreWith({
    hub: { url: '/', nodeId: 'jarvis', nodeName: 'Jarvis' },
    initialStorage: {
      term_llm_token: 'direct-token',
      term_llm_active_session: 'sess_direct',
      term_llm_selected_model: 'gpt-5.5'
    }
  });

  if (testApp.STORAGE_KEYS.token !== 'term_llm_token') {
    fail(name, `token key = ${JSON.stringify(testApp.STORAGE_KEYS.token)}`);
    return;
  }
  if (testApp.state.token !== 'direct-token' || testApp.state.activeSessionId !== 'sess_direct' || testApp.state.selectedModel !== 'gpt-5.5') {
    fail(name, `state did not read expected direct values: ${JSON.stringify({ token: testApp.state.token, activeSessionId: testApp.state.activeSessionId, selectedModel: testApp.state.selectedModel })}`);
    return;
  }
  pass(name);
})();

(function testHubProxyScopedStorageMigratesUnscopedKeysExceptToken() {
  const name = 'Hub-proxied storage copies direct keys except token';
  const testApp = loadAppCoreWith({
    hub: { url: '/', nodeId: 'jarvis', nodeName: 'Jarvis', nodeBasePath: '/chat' },
    initialStorage: {
      term_llm_token: 'direct-token',
      term_llm_active_session: 'sess_direct',
      term_llm_selected_model: 'gpt-5.5'
    }
  });

  if (testApp.STORAGE_KEYS.token !== 'term_llm_token:jarvis') {
    fail(name, `scoped token key = ${JSON.stringify(testApp.STORAGE_KEYS.token)}`);
    return;
  }
  if (testApp.state.token !== '' || testApp.state.activeSessionId !== 'sess_direct' || testApp.state.selectedModel !== 'gpt-5.5') {
    fail(name, `state did not read expected scoped values: ${JSON.stringify({ token: testApp.state.token, activeSessionId: testApp.state.activeSessionId, selectedModel: testApp.state.selectedModel })}`);
    return;
  }
  pass(name);
})();

(function testHubProxyScopedStorageKeepsExistingScopedValues() {
  const name = 'Hub-proxied storage keeps existing scoped values over direct keys';
  const testApp = loadAppCoreWith({
    hub: { url: '/', nodeId: 'jarvis', nodeName: 'Jarvis', nodeBasePath: '/chat' },
    initialStorage: {
      term_llm_token: 'direct-token',
      'term_llm_token:jarvis': 'scoped-token'
    }
  });

  if (testApp.state.token !== 'scoped-token') {
    fail(name, `token = ${JSON.stringify(testApp.state.token)}`);
    return;
  }
  pass(name);
})();

(function testHubAssetURLsRebaseToCurrentNodeMount() {
  const name = 'hub asset URLs rebase from node base path to current mount';
  const testApp = loadAppCoreWith({
    hub: { url: '/', nodeId: 'alpha', nodeName: 'Alpha', nodeBasePath: '/ui' }
  });

  const cases = [
    ['/ui/images/serve-abc.png', '/chat/images/serve-abc.png'],
    ['/ui/files/art.svg?download=1#preview', '/chat/files/art.svg?download=1#preview'],
    ['http://localhost/ui/images/serve-abc.png', 'http://localhost/chat/images/serve-abc.png'],
    ['/ui/v1/models', '/ui/v1/models'],
    ['images/local.png', 'images/local.png'],
    ['data:image/png;base64,aaa', 'data:image/png;base64,aaa'],
    ['https://elsewhere.test/ui/images/serve-abc.png', 'https://elsewhere.test/ui/images/serve-abc.png']
  ];

  for (const [input, expected] of cases) {
    const got = testApp.rebaseHubAssetURL(input);
    if (got !== expected) {
      fail(name, `${JSON.stringify(input)} -> ${JSON.stringify(got)}, want ${JSON.stringify(expected)}`);
      return;
    }
  }
  pass(name);
})();

(function testHubAssetURLsDoNotRebaseDirectHubContext() {
  const name = 'hub asset URL rebase is inert without proxied node base path';
  const testApp = loadAppCoreWith({
    hub: { url: '/', nodeId: 'alpha', nodeName: 'Alpha' }
  });
  const got = testApp.rebaseHubAssetURL('/ui/images/serve-abc.png');
  if (got !== '/ui/images/serve-abc.png') {
    fail(name, `got ${JSON.stringify(got)}`);
    return;
  }
  pass(name);
})();

(function testSidebarBrandUsesUiTitleOverride() {
  const name = 'sidebar brand uses UI title override';
  const brandNode = makeNode();
  loadAppCoreWith({
    agentName: 'jarvis',
    uiTitle: 'My Custom Title',
    nodeOverrides: { sidebarBrandText: brandNode },
  });

  if (brandNode.textContent !== 'My Custom Title') {
    fail(name, `got ${JSON.stringify(brandNode.textContent)}`);
    return;
  }
  pass(name);
})();

(function testSidebarBrandWhitespaceTitleFallsBackToAgent() {
  const name = 'sidebar brand whitespace UI title falls back to agent name';
  const brandNode = makeNode();
  loadAppCoreWith({
    agentName: 'jarvis',
    uiTitle: '   ',
    nodeOverrides: { sidebarBrandText: brandNode },
  });

  if (brandNode.textContent !== 'Jarvis') {
    fail(name, `got ${JSON.stringify(brandNode.textContent)}`);
    return;
  }
  pass(name);
})();

(function testDocumentTitleUsesUiTitlePrefix() {
  const name = 'document title uses UI title prefix';
  const testApp = loadAppCoreWith({ uiTitle: 'My Lab' });
  testApp.state.sessions = [{ id: 's1', title: 'can you visit hacker news', messages: [] }];
  testApp.state.activeSessionId = 's1';
  testApp.updateDocumentTitle();

  if (testApp.__testDocument.title !== 'My Lab · can you visit hacker news') {
    fail(name, `got ${JSON.stringify(testApp.__testDocument.title)}`);
    return;
  }
  pass(name);
})();

(function testDocumentTitleUsesOnlyUiTitleWithoutSession() {
  const name = 'document title uses only UI title without session';
  const testApp = loadAppCoreWith({ uiTitle: 'My Lab' });
  testApp.updateDocumentTitle();

  if (testApp.__testDocument.title !== 'My Lab') {
    fail(name, `got ${JSON.stringify(testApp.__testDocument.title)}`);
    return;
  }
  pass(name);
})();

(function testDocumentTitleFallsBackWithoutUiTitlePrefix() {
  const name = 'document title falls back without UI title prefix';
  const testApp = loadAppCoreWith();
  testApp.state.sessions = [{ id: 's1', title: 'can you visit hacker news', messages: [] }];
  testApp.state.activeSessionId = 's1';
  testApp.updateDocumentTitle();

  if (testApp.__testDocument.title !== 'Chat · can you visit hacker news') {
    fail(name, `got ${JSON.stringify(testApp.__testDocument.title)}`);
    return;
  }
  pass(name);
})();

(function testSidebarBrandUsesAgentName() {
  const name = 'sidebar brand uses injected agent name';
  const brandNode = makeNode();
  const testApp = loadAppCoreWith({
    agentName: 'jarvis',
    nodeOverrides: { sidebarBrandText: brandNode },
  });

  if (brandNode.textContent !== 'Jarvis') {
    fail(name, `got ${JSON.stringify(brandNode.textContent)}`);
    return;
  }
  if (testApp.displayAgentName('web-researcher') !== 'Web Researcher') {
    fail(name, `hyphenated agent label was ${JSON.stringify(testApp.displayAgentName('web-researcher'))}`);
    return;
  }
  pass(name);
})();

(function testSidebarBrandFallsBackToChat() {
  const name = 'sidebar brand falls back to Chat without an agent';
  const brandNode = makeNode();
  loadAppCoreWith({ nodeOverrides: { sidebarBrandText: brandNode } });

  if (brandNode.textContent !== 'Chat') {
    fail(name, `got ${JSON.stringify(brandNode.textContent)}`);
    return;
  }
  pass(name);
})();

(function testStripsDuplicateEffortSuffix() {
  const name = 'splitHeaderModelEffort strips matching suffix';
  const result = app.splitHeaderModelEffort('gpt-5.4-medium', 'medium');
  if (result.model !== 'gpt-5.4' || result.effort !== 'medium') {
    fail(name, `got ${JSON.stringify(result)}`, 'want {"model":"gpt-5.4","effort":"medium"}');
    return;
  }
  pass(name);
})();

(function testStripsConflictingEffortSuffixWhenBaseModelExists() {
  const name = 'splitHeaderModelEffort strips stale suffix when separate effort wins';
  const result = app.splitHeaderModelEffort('gpt-5.5-medium', 'xhigh', ['gpt-5.5']);
  if (result.model !== 'gpt-5.5' || result.effort !== 'xhigh') {
    fail(name, `got ${JSON.stringify(result)}`, 'want {"model":"gpt-5.5","effort":"xhigh"}');
    return;
  }
  pass(name);
})();

(function testKeepsKnownModelWhoseNameEndsWithEffortWord() {
  const name = 'splitHeaderModelEffort keeps known natural suffix model';
  const result = app.splitHeaderModelEffort('gpt-5.1-codex-max', 'xhigh', ['gpt-5.1-codex-max']);
  if (result.model !== 'gpt-5.1-codex-max' || result.effort !== 'xhigh') {
    fail(name, `got ${JSON.stringify(result)}`, 'want natural model untouched');
    return;
  }
  pass(name);
})();

(function testCompactHeaderModelLabelRemovesProviderNoise() {
  const name = 'compactHeaderModelLabel removes provider noise';
  const cases = [
    ['claude-sonnet-4.5-thinking-super-long-preview-build-20260613', 'sonnet 4.5'],
    ['claude-3-7-sonnet-latest', 'sonnet 3.7'],
    ['claude-opus-4.8', 'opus 4.8'],
    ['anthropic/claude-3-5-haiku-20241022', 'haiku 3.5'],
    ['chatgpt-gpt-5.5', 'gpt-5.5'],
    ['openai/gpt-5.5', 'gpt-5.5'],
    ['gpt-5.5', 'gpt-5.5'],
  ];
  for (const [input, expected] of cases) {
    const got = app.compactHeaderModelLabel(input);
    if (got !== expected) {
      fail(name, `for ${JSON.stringify(input)} got ${JSON.stringify(got)}, want ${JSON.stringify(expected)}`);
      return;
    }
  }
  pass(name);
})();

(function testHeaderEffortShowsQueuedOnlyUntilApplied() {
  const name = 'header effort shows queued only until applied';
  const chipEffortLabel = makeNode();
  const testApp = loadAppCoreWith({
    nodeOverrides: {
      headerStats: makeNode(),
      chipEffortLabel,
      chipSepModelEffort: makeNode(),
      chipProviderLabel: makeNode(),
      chipModelLabel: makeNode(),
      chipSepProviderModel: makeNode(),
      chipProviderSelect: makeNode(),
      chipModelSelect: makeNode(),
      chipEffortSelect: makeNode(),
      chipProviderTrigger: makeNode(),
      chipModelTrigger: makeNode(),
      chipEffortTrigger: makeNode(),
      modelPicker: makeNode(),
      headerTokens: makeNode(),
      headerTokensSep: makeNode(),
    },
  });
  const session = {
    id: 'sess_effort_header',
    provider: 'chatgpt',
    activeModel: 'gpt-5.4',
    activeEffort: 'medium',
    pendingEffort: 'high',
    pendingEffortQueued: true,
  };
  testApp.state.streaming = true;
  testApp.state.activeSessionId = session.id;
  testApp.updateSessionUsageDisplay(session);
  if (chipEffortLabel.textContent !== 'high queued') {
    fail(name, `queued label = ${JSON.stringify(chipEffortLabel.textContent)}, want high queued`);
    return;
  }

  delete session.pendingEffort;
  delete session.pendingEffortQueued;
  session.activeEffort = 'high';
  testApp.updateSessionUsageDisplay(session);
  if (chipEffortLabel.textContent !== 'high') {
    fail(name, `applied label = ${JSON.stringify(chipEffortLabel.textContent)}, want high`);
    return;
  }
  if (testApp.elements.chipModelTrigger.dataset.effortLevel !== 'high') {
    fail(name, `model effort meter = ${JSON.stringify(testApp.elements.chipModelTrigger.dataset.effortLevel)}, want high`);
    return;
  }
  if (testApp.elements.chipModelTrigger.dataset.effortLabel !== 'high') {
    fail(name, `model effort meter label = ${JSON.stringify(testApp.elements.chipModelTrigger.dataset.effortLabel)}, want high`);
    return;
  }
  pass(name);
})();

(function testPendingInterjectBadgeStateIsDistinctFromInjected() {
  const name = 'pending_interject is a valid interrupt state labelled distinctly from injected';

  if (app.sanitizeInterruptState('pending_interject') !== 'pending_interject') {
    fail(name, 'expected sanitizeInterruptState to preserve "pending_interject"');
    return;
  }

  const meta = app.INTERRUPT_BADGE_META && app.INTERRUPT_BADGE_META.pending_interject;
  if (!meta) {
    fail(name, 'expected INTERRUPT_BADGE_META to define pending_interject');
    return;
  }
  if (meta.label === 'injected' || meta.label === app.INTERRUPT_BADGE_META.interject.label) {
    fail(name, `pending_interject label should differ from injected, got "${meta.label}"`);
    return;
  }
  pass(name);
})();

(function testInterjectionPhaseKeepsOnlyPendingWorkInComposerBanner() {
  const name = 'INTERJECTION_PHASE keeps pending work in composer and committed work out';
  const phases = app.INTERJECTION_PHASE;
  if (!phases) {
    fail(name, 'expected INTERJECTION_PHASE to be exported from app-core');
    return;
  }
  const expected = {
    evaluating: 'deciding',
    queued: 'interject',
    willQueue: 'queue',
    willCancel: 'cancel_queue',
    committed: null,
    failed: null
  };
  for (const [phase, banner] of Object.entries(expected)) {
    const got = phases[phase];
    if (!got) { fail(name, `missing phase ${phase}`); return; }
    if (got.banner !== banner) { fail(name, `phase ${phase} banner=${JSON.stringify(got.banner)}, want ${JSON.stringify(banner)}`); return; }
    if (Object.prototype.hasOwnProperty.call(got, 'badge')) {
      fail(name, `phase ${phase} must not define an optimistic stream badge`);
      return;
    }
  }
  pass(name);
})();

(function testLeavesDistinctModelUntouched() {
  const name = 'splitHeaderModelEffort keeps distinct model';
  const result = app.splitHeaderModelEffort('gpt-5.4', 'medium');
  if (result.model !== 'gpt-5.4' || result.effort !== 'medium') {
    fail(name, `got ${JSON.stringify(result)}`, 'want {"model":"gpt-5.4","effort":"medium"}');
    return;
  }
  pass(name);
})();

(function testHandlesUnderscoreSuffix() {
  const name = 'splitHeaderModelEffort strips underscore suffix';
  const result = app.splitHeaderModelEffort('foo_bar_medium', 'medium');
  if (result.model !== 'foo_bar' || result.effort !== 'medium') {
    fail(name, `got ${JSON.stringify(result)}`, 'want {"model":"foo_bar","effort":"medium"}');
    return;
  }
  pass(name);
})();

(function testRefreshRelativeTimesUsesMessagesScope() {
  const name = 'refreshRelativeTimes scopes query to elements.messages';

  const ts = 1_700_000_000_000;
  const timeNode = {
    textContent: '',
    title: '',
    getAttribute(attr) { return attr === 'data-created' ? String(ts) : null; },
  };

  let messagesQueried = false;
  let documentQueried = false;

  const messagesEl = Object.assign(makeNode(), {
    querySelectorAll(sel) {
      if (sel === '[data-created]') { messagesQueried = true; return [timeNode]; }
      return [];
    },
  });

  const testApp = loadAppCoreWith({
    nodeOverrides: { messages: messagesEl },
    docQSTracker(sel) {
      if (sel === '[data-created]') documentQueried = true;
      return [];
    },
  });

  testApp.refreshRelativeTimes();

  if (!messagesQueried) {
    fail(name, 'elements.messages.querySelectorAll was not called with [data-created]');
    return;
  }
  if (documentQueried) {
    fail(name, 'document.querySelectorAll was consulted — query must be scoped to elements.messages');
    return;
  }
  if (!timeNode.textContent) {
    fail(name, 'time node textContent was not updated');
    return;
  }
  pass(name);
})();

(function testConnectionStateStaysHiddenForNonWarnings() {
  const name = 'setConnectionState hides non-warning statuses';
  const classes = new Set(['bad']);
  const connectionNode = Object.assign(makeNode(), {
    hidden: true,
    classList: {
      add(...names) { names.forEach((n) => classes.add(n)); },
      remove(...names) { names.forEach((n) => classes.delete(n)); },
      toggle(name, force) {
        if (force === undefined ? !classes.has(name) : force) classes.add(name);
        else classes.delete(name);
        return classes.has(name);
      },
      contains(name) { return classes.has(name); },
    },
  });
  const testApp = loadAppCoreWith({
    nodeOverrides: { connectionState: connectionNode },
    navigatorOverrides: { onLine: true },
  });

  testApp.setConnectionState('⚡ direct', 'ok');

  if (!connectionNode.hidden) {
    fail(name, 'direct/ok status should stay hidden');
    return;
  }
  if (connectionNode.textContent !== '') {
    fail(name, `got visible text ${JSON.stringify(connectionNode.textContent)}`);
    return;
  }
  if (classes.has('ok')) {
    fail(name, 'ok class should not be retained');
    return;
  }
  pass(name);
})();

(function testConnectionStateShowsOfflineWarning() {
  const name = 'setConnectionState shows offline warning';
  const classes = new Set();
  const connectionNode = Object.assign(makeNode(), {
    hidden: true,
    classList: {
      add(...names) { names.forEach((n) => classes.add(n)); },
      remove(...names) { names.forEach((n) => classes.delete(n)); },
      toggle(name, force) {
        if (force === undefined ? !classes.has(name) : force) classes.add(name);
        else classes.delete(name);
        return classes.has(name);
      },
      contains(name) { return classes.has(name); },
    },
  });
  const testApp = loadAppCoreWith({
    nodeOverrides: { connectionState: connectionNode },
    navigatorOverrides: { onLine: false },
  });

  testApp.setConnectionState('', '');

  if (connectionNode.hidden) {
    fail(name, 'offline warning should be visible');
    return;
  }
  if (connectionNode.textContent !== 'Offline — reconnect to continue') {
    fail(name, `got ${JSON.stringify(connectionNode.textContent)}`);
    return;
  }
  if (!classes.has('bad')) {
    fail(name, 'offline warning should have bad class');
    return;
  }
  const session = { id: 'session_offline', activeResponseId: 'resp_offline', messages: [] };
  testApp.state.sessions = [session];
  testApp.state.activeSessionId = session.id;
  testApp.state.draftSessionActive = false;
  testApp.setProviderRetryStatus(session.id, 'resp_offline', 'Retrying provider…');
  testApp.clearProviderRetryStatus(session.id, 'resp_offline');
  if (connectionNode.hidden || connectionNode.textContent !== 'Offline — reconnect to continue' || !classes.has('bad')) {
    fail(name, 'provider retry set/clear changed the offline warning', connectionNode.textContent);
    return;
  }
  pass(name);
})();

(function testConnectivityStateIsSeparateAndActionable() {
  const name = 'connectivity state separates authentication from actionable network phases';
  const classes = new Set();
  const connectionNode = Object.assign(makeNode(), {
    hidden: true,
    classList: {
      add(...names) { names.forEach((n) => classes.add(n)); },
      remove(...names) { names.forEach((n) => classes.delete(n)); },
      contains(value) { return classes.has(value); },
    },
  });
  const testApp = loadAppCoreWith({
    nodeOverrides: { connectionState: connectionNode },
    navigatorOverrides: { onLine: true },
  });

  testApp.setApplicationConnected(true, true);
  testApp.setConnectivityState({ network: 'offline', phase: 'offline', pendingSafe: 1 });
  if (!testApp.state.connected || !testApp.state.connectivity.authenticated || !testApp.state.connectivity.startupConnected) {
    fail(name, 'network outage incorrectly cleared startup/authentication state');
    return;
  }
  if (connectionNode.textContent !== 'Offline — message pending safely; reconnect to continue' || !classes.has('bad')) {
    fail(name, 'pending-safe offline status was not actionable', connectionNode.textContent);
    return;
  }

  testApp.setConnectivityState({ network: 'recovering', phase: 'recovering', pendingSafe: 0 });
  if (connectionNode.textContent !== 'Network restored — recovering…' || !classes.has('retry')) {
    fail(name, 'recovering status was not visible', connectionNode.textContent);
    return;
  }
  testApp.setConnectivityState({ network: 'recovering', phase: 'catching-up' });
  if (connectionNode.textContent !== 'Catching up with the server…') {
    fail(name, 'catching-up status was not visible', connectionNode.textContent);
    return;
  }
  testApp.setConnectivityState({ network: 'unstable', phase: 'unstable' });
  if (connectionNode.textContent !== 'Connection unstable' || !classes.has('bad')) {
    fail(name, 'unstable status falsely promised an automatic retry', connectionNode.textContent);
    return;
  }
  testApp.setConnectionState('Response reconnect paused — retry from the composer', 'bad');
  if (connectionNode.textContent !== 'Response reconnect paused — retry from the composer') {
    fail(name, 'generic connectivity status obscured an actionable legacy warning', connectionNode.textContent);
    return;
  }
  testApp.setConnectionState('', '');
  testApp.setConnectivityState({ network: 'healthy', phase: '' });
  if (!connectionNode.hidden || connectionNode.textContent) {
    fail(name, 'healthy network should leave the header quiet', connectionNode.textContent);
    return;
  }
  pass(name);
})();

(function testConnectionStateOwnerCannotClearNewerWarning() {
  const name = 'connection state owners clear only the warning they set';
  const connectionNode = Object.assign(makeNode(), { hidden: true });
  const testApp = loadAppCoreWith({
    nodeOverrides: { connectionState: connectionNode },
    navigatorOverrides: { onLine: true },
  });

  const staleCatchUpOwner = testApp.setConnectionState('Catching up with this session…', 'bad');
  const currentCatchUpOwner = testApp.setConnectionState('Catching up with this session…', 'bad');
  if (testApp.clearConnectionStateOwner(staleCatchUpOwner)) {
    fail(name, 'stale owner reported clearing a newer warning');
    return;
  }
  if (connectionNode.hidden || connectionNode.textContent !== 'Catching up with this session…') {
    fail(name, 'stale owner cleared a newer same-text catch-up warning', connectionNode.textContent);
    return;
  }

  testApp.setConnectionState('Transport unavailable', 'bad');
  if (testApp.clearConnectionStateOwner(currentCatchUpOwner)) {
    fail(name, 'superseded catch-up owner reported clearing a transport warning');
    return;
  }
  if (connectionNode.hidden || connectionNode.textContent !== 'Transport unavailable') {
    fail(name, 'catch-up owner cleared a newer transport warning', connectionNode.textContent);
    return;
  }

  const ownedWarning = testApp.setConnectionState('Catching up with this session…', 'bad');
  if (!testApp.clearConnectionStateOwner(ownedWarning)) {
    fail(name, 'current owner did not clear its warning');
    return;
  }
  if (!connectionNode.hidden || connectionNode.textContent) {
    fail(name, 'matching owner left its warning visible', connectionNode.textContent);
    return;
  }
  pass(name);
})();

(function testProviderRetryStatusIsOwnedAndLegacyWarningHasPriority() {
  const name = 'provider retry status is response-owned and lower priority than legacy warnings';
  const classes = new Set();
  const connectionNode = Object.assign(makeNode(), {
    hidden: true,
    classList: {
      add(...names) { names.forEach((n) => classes.add(n)); },
      remove(...names) { names.forEach((n) => classes.delete(n)); },
      contains(name) { return classes.has(name); },
    },
  });
  const testApp = loadAppCoreWith({
    nodeOverrides: { connectionState: connectionNode },
    navigatorOverrides: { onLine: true },
  });
  const session = { id: 'session_retry', activeResponseId: 'resp_retry', messages: [] };
  testApp.state.sessions = [session];
  testApp.state.activeSessionId = session.id;
  testApp.state.draftSessionActive = false;

  testApp.setProviderRetryStatus(session.id, 'resp_retry', 'Retrying provider (2/6)…');
  if (connectionNode.hidden || connectionNode.textContent !== 'Retrying provider (2/6)…') {
    fail(name, 'owned retry status was not shown', JSON.stringify(connectionNode));
    return;
  }
  if (!classes.has('retry') || classes.has('bad')) {
    fail(name, 'retry status should use neutral retry mode', JSON.stringify(Array.from(classes)));
    return;
  }

  testApp.setConnectionState('Catching up session…', 'bad');
  testApp.setProviderRetryStatus(session.id, 'resp_retry', 'Retrying provider (3/6)…');
  if (connectionNode.textContent !== 'Catching up session…' || !classes.has('bad')) {
    fail(name, 'provider retry overwrote the legacy warning', connectionNode.textContent);
    return;
  }
  testApp.clearProviderRetryStatus(session.id, 'resp_retry');
  if (connectionNode.textContent !== 'Catching up session…' || connectionNode.hidden) {
    fail(name, 'clearing provider retry erased the legacy warning', connectionNode.textContent);
    return;
  }
  pass(name);
})();

(function testProviderRetryStatusRejectsStaleOwners() {
  const name = 'provider retry status ignores background sets and stale clears';
  const connectionNode = Object.assign(makeNode(), { hidden: true });
  const testApp = loadAppCoreWith({
    nodeOverrides: { connectionState: connectionNode },
    navigatorOverrides: { onLine: true },
  });
  const visible = { id: 'session_visible', activeResponseId: 'resp_new', messages: [] };
  testApp.state.sessions = [visible];
  testApp.state.activeSessionId = visible.id;
  testApp.state.draftSessionActive = false;

  testApp.setProviderRetryStatus('session_background', 'resp_old', 'Background retry');
  if (!connectionNode.hidden || connectionNode.textContent) {
    fail(name, 'background retry changed the visible header', connectionNode.textContent);
    return;
  }

  testApp.setProviderRetryStatus(visible.id, 'resp_new', 'Current retry');
  testApp.clearProviderRetryStatus(visible.id, 'resp_old');
  if (connectionNode.hidden || connectionNode.textContent !== 'Current retry') {
    fail(name, 'stale response cleared the current retry status', connectionNode.textContent);
    return;
  }
  testApp.clearProviderRetryStatus(visible.id, 'resp_new');
  if (!connectionNode.hidden || connectionNode.textContent) {
    fail(name, 'matching response did not clear retry status', connectionNode.textContent);
    return;
  }
  pass(name);
})();

(function testUserScrollIntentStopsStreamingAutoScroll() {
  const name = 'user scroll intent stops streaming auto-scroll';
  const chatScroll = Object.assign(makeNode(), {
    scrollTop: 900,
    scrollHeight: 1000,
    clientHeight: 100,
  });
  const testApp = loadAppCoreWith({ nodeOverrides: { chatScroll } });

  testApp.state.autoScroll = true;
  testApp.noteUserScrollIntent();
  testApp.scrollToBottom();

  if (chatScroll.scrollTop !== 900) {
    fail(name, `streaming scroll moved viewport to ${chatScroll.scrollTop}`);
    return;
  }
  if (testApp.state.autoScroll !== false) {
    fail(name, 'autoScroll should stay disabled after user scroll intent');
    return;
  }
  pass(name);
})();

(function testScrollPositionReenablesAutoScrollNearBottom() {
  const name = 'scrolling back near bottom re-enables auto-scroll';
  const chatScroll = Object.assign(makeNode(), {
    scrollTop: 800,
    scrollHeight: 1000,
    clientHeight: 100,
  });
  const testApp = loadAppCoreWith({ nodeOverrides: { chatScroll } });

  testApp.noteUserScrollIntent();
  testApp.noteScrollPositionChanged();
  if (testApp.state.autoScroll !== false) {
    fail(name, 'autoScroll should remain disabled while away from bottom');
    return;
  }

  chatScroll.scrollTop = 920;
  testApp.noteScrollPositionChanged();
  if (testApp.state.autoScroll !== true) {
    fail(name, 'autoScroll should re-enable near bottom');
    return;
  }
  pass(name);
})();

(function testScrollToBottomIsThrottledToTwicePerSecond() {
  const name = 'scroll to bottom is throttled to twice per second';
  let nowMs = 1000;
  const timers = [];
  const chatScroll = Object.assign(makeNode(), {
    scrollTop: 0,
    scrollHeight: 1000,
    clientHeight: 100,
  });
  const testApp = loadAppCoreWith({
    nodeOverrides: { chatScroll },
    now: () => nowMs,
    timerOverrides: {
      setTimeout(fn, delay) {
        timers.push({ fn, delay });
        return timers.length;
      },
    },
  });

  testApp.state.autoScroll = true;
  testApp.scrollToBottom();
  if (chatScroll.scrollTop !== 1000) {
    fail(name, `expected first scroll immediately, got ${chatScroll.scrollTop}`);
    return;
  }

  nowMs = 1100;
  chatScroll.scrollHeight = 1100;
  testApp.scrollToBottom();
  if (chatScroll.scrollTop !== 1000) {
    fail(name, `second scroll inside throttle window should be delayed, got ${chatScroll.scrollTop}`);
    return;
  }
  if (timers.length !== 1 || timers[0].delay !== 400) {
    fail(name, `expected one trailing timer with 400ms delay, got ${JSON.stringify(timers.map((t) => t.delay))}`);
    return;
  }

  nowMs = 1200;
  chatScroll.scrollHeight = 1200;
  testApp.scrollToBottom();
  if (timers.length !== 1) {
    fail(name, `expected repeated scroll requests to share one timer, got ${timers.length}`);
    return;
  }

  nowMs = 1500;
  timers[0].fn();
  if (chatScroll.scrollTop !== 1200) {
    fail(name, `expected trailing scroll to latest bottom, got ${chatScroll.scrollTop}`);
    return;
  }
  pass(name);
})();

(function testForceScrollBypassesThrottle() {
  const name = 'force scroll bypasses throttle delay';
  let nowMs = 1000;
  let clearedTimer = 0;
  const timers = [];
  const chatScroll = Object.assign(makeNode(), {
    scrollTop: 0,
    scrollHeight: 1000,
    clientHeight: 100,
  });
  const testApp = loadAppCoreWith({
    nodeOverrides: { chatScroll },
    now: () => nowMs,
    timerOverrides: {
      setTimeout(fn, delay) {
        timers.push({ fn, delay });
        return timers.length;
      },
      clearTimeout(id) {
        clearedTimer = id;
      },
    },
  });

  testApp.state.autoScroll = true;
  testApp.scrollToBottom();
  nowMs = 1100;
  chatScroll.scrollHeight = 1100;
  testApp.scrollToBottom();
  if (chatScroll.scrollTop !== 1000 || timers.length !== 1) {
    fail(name, 'expected non-forced scroll to be throttled before forcing', JSON.stringify({ scrollTop: chatScroll.scrollTop, timers: timers.length }));
    return;
  }

  chatScroll.scrollHeight = 1200;
  testApp.scrollToBottom(true);
  if (clearedTimer !== 1) {
    fail(name, `expected forced scroll to clear pending trailing timer, got ${clearedTimer}`);
    return;
  }
  if (chatScroll.scrollTop !== 1200) {
    fail(name, `expected forced scroll to bottom immediately, got ${chatScroll.scrollTop}`);
    return;
  }
  if (testApp.state.autoScroll !== true) {
    fail(name, 'forced scroll should restore autoScroll');
    return;
  }

  pass(name);
})();

(function testPendingScrollDoesNotFightUserScrollIntent() {
  const name = 'pending scroll does not fight user scroll intent';
  let nowMs = 1000;
  let clearedTimer = 0;
  const timers = [];
  const chatScroll = Object.assign(makeNode(), {
    scrollTop: 0,
    scrollHeight: 1000,
    clientHeight: 100,
  });
  const testApp = loadAppCoreWith({
    nodeOverrides: { chatScroll },
    now: () => nowMs,
    timerOverrides: {
      setTimeout(fn, delay) {
        timers.push({ fn, delay });
        return timers.length;
      },
      clearTimeout(id) {
        clearedTimer = id;
      },
    },
  });

  testApp.state.autoScroll = true;
  testApp.scrollToBottom();
  nowMs = 1100;
  chatScroll.scrollHeight = 1100;
  testApp.scrollToBottom();
  testApp.noteUserScrollIntent();

  if (clearedTimer !== 1) {
    fail(name, `expected pending scroll timer to be cleared, got ${clearedTimer}`);
    return;
  }
  timers[0].fn();
  if (chatScroll.scrollTop !== 1000) {
    fail(name, `stale timer should not move viewport after user intent, got ${chatScroll.scrollTop}`);
    return;
  }
  if (testApp.state.autoScroll !== false) {
    fail(name, 'autoScroll should remain disabled after stale timer');
    return;
  }
  pass(name);
})();

(function testForceScrollRestoresAutoScroll() {
  const name = 'force scroll restores bottom stickiness';
  const chatScroll = Object.assign(makeNode(), {
    scrollTop: 500,
    scrollHeight: 1000,
    clientHeight: 100,
  });
  const testApp = loadAppCoreWith({ nodeOverrides: { chatScroll } });

  testApp.noteUserScrollIntent();
  testApp.scrollToBottom(true);

  if (chatScroll.scrollTop !== 1000) {
    fail(name, `expected forced bottom scroll, got ${chatScroll.scrollTop}`);
    return;
  }
  if (testApp.state.autoScroll !== true) {
    fail(name, 'forced scroll should restore autoScroll');
    return;
  }
  pass(name);
})();

(function testForcedScrollSettlesAfterPostRenderLayoutGrowth() {
  const name = 'forced scroll stays bottom-anchored while post-render layout settles';
  const frames = [];
  const timers = [];
  let resizeCallback = null;
  let observerDisconnected = false;
  const observed = [];
  const viewportListeners = { resize: [], scroll: [] };
  const visualViewport = {
    height: 844,
    offsetTop: 0,
    addEventListener(type, listener) { viewportListeners[type].push(listener); },
  };
  const chatScroll = Object.assign(makeNode(), {
    scrollTop: 0,
    scrollHeight: 1000,
    clientHeight: 100,
  });
  const messages = makeNode();

  class FakeResizeObserver {
    constructor(callback) { resizeCallback = callback; }
    observe(node) { observed.push(node); }
    disconnect() { observerDisconnected = true; }
  }

  const testApp = loadAppCoreWith({
    nodeOverrides: { chatScroll, messages },
    windowOverrides: {
      visualViewport,
      requestAnimationFrame(callback) {
        frames.push(callback);
        return frames.length;
      },
    },
    contextOverrides: { ResizeObserver: FakeResizeObserver },
    timerOverrides: {
      setTimeout(fn, delay) {
        timers.push({ fn, delay, cleared: false });
        return timers.length;
      },
      clearTimeout(id) {
        if (timers[id - 1]) timers[id - 1].cleared = true;
      },
    },
  });

  // Ignore app-core's initial viewport synchronization frame.
  frames.length = 0;
  testApp.scrollToBottom(true);
  if (chatScroll.scrollTop !== 1000) {
    fail(name, `expected immediate forced scroll, got ${chatScroll.scrollTop}`);
    return;
  }
  if (typeof resizeCallback !== 'function' || !observed.includes(chatScroll) || !observed.includes(messages)) {
    fail(name, 'forced scroll did not observe the transcript and scroll viewport while layout settles');
    return;
  }

  // Finish the initial follow-up frame, then model a later markdown/image/font
  // layout change that grows the transcript after renderMessages returned.
  while (frames.length) frames.shift()();
  chatScroll.scrollHeight = 1079;
  resizeCallback();
  while (frames.length) frames.shift()();

  if (chatScroll.scrollTop !== 1079) {
    fail(name, `post-render growth left viewport at ${chatScroll.scrollTop}`);
    return;
  }

  // Mobile browser chrome changes the visual viewport and then the chat box.
  // The settling anchor must run after that viewport synchronization as well.
  chatScroll.scrollHeight = 1158;
  viewportListeners.resize.forEach((listener) => listener());
  while (frames.length) frames.shift()();
  if (chatScroll.scrollTop !== 1158) {
    fail(name, `visual viewport resize left viewport at ${chatScroll.scrollTop}`);
    return;
  }

  const settleTimer = timers.find((timer) => timer.delay > 0);
  if (!settleTimer || settleTimer.delay > 2000) {
    fail(name, `settling anchor was not bounded, timers=${JSON.stringify(timers.map((timer) => timer.delay))}`);
    return;
  }
  settleTimer.fn();
  chatScroll.scrollHeight = 1200;
  resizeCallback();
  viewportListeners.resize.forEach((listener) => listener());
  while (frames.length) frames.shift()();
  if (!observerDisconnected || chatScroll.scrollTop !== 1158) {
    fail(name, 'settling work continued after its bounded window');
    return;
  }
  pass(name);
})();

(function testUserIntentCancelsForcedScrollSettling() {
  const name = 'explicit user intent cancels forced-scroll settling corrections';
  const frames = [];
  const timers = [];
  let resizeCallback = null;
  let observerDisconnected = false;
  const chatScroll = Object.assign(makeNode(), {
    scrollTop: 0,
    scrollHeight: 1000,
    clientHeight: 100,
  });

  class FakeResizeObserver {
    constructor(callback) { resizeCallback = callback; }
    observe() {}
    disconnect() { observerDisconnected = true; }
  }

  const testApp = loadAppCoreWith({
    nodeOverrides: { chatScroll, messages: makeNode() },
    windowOverrides: {
      requestAnimationFrame(callback) {
        frames.push(callback);
        return frames.length;
      },
      cancelAnimationFrame() {},
    },
    contextOverrides: { ResizeObserver: FakeResizeObserver },
    timerOverrides: {
      setTimeout(fn, delay) {
        timers.push({ fn, delay, cleared: false });
        return timers.length;
      },
      clearTimeout(id) {
        if (timers[id - 1]) timers[id - 1].cleared = true;
      },
    },
  });

  frames.length = 0;
  testApp.scrollToBottom(true);
  while (frames.length) frames.shift()();
  testApp.noteUserScrollIntent();
  chatScroll.scrollHeight = 1120;
  resizeCallback?.();
  while (frames.length) frames.shift()();

  if (chatScroll.scrollTop !== 1000) {
    fail(name, `settling correction fought user intent and moved to ${chatScroll.scrollTop}`);
    return;
  }
  if (!observerDisconnected || !timers.some((timer) => timer.cleared)) {
    fail(name, 'user intent did not tear down the bounded settling work');
    return;
  }
  if (testApp.state.autoScroll !== false) {
    fail(name, 'autoScroll should remain disabled after user intent');
    return;
  }
  pass(name);
})();

(function testMessageEvictionKeepsActiveOlderSession() {
  const name = 'message eviction keeps active older session loaded';
  const testApp = loadAppCore();
  testApp.state.sessions = Array.from({ length: 11 }, (_, index) => ({
    id: `s${index + 1}`,
    title: `Session ${index + 1}`,
    created: 1000 + index,
    lastMessageAt: 1000 + index,
    messages: [{ id: `m${index + 1}`, role: 'user', content: 'hi', created: 1000 + index }],
  }));
  testApp.state.activeSessionId = 's1';

  testApp.saveSessions();

  const active = testApp.state.sessions.find((session) => session.id === 's1');
  if (!active || active._serverOnly || active.messages.length !== 1) {
    fail(name, 'active older session was evicted and would render blank');
    return;
  }
  const loaded = testApp.state.sessions.filter((session) => session.messages.length > 0 && !session._serverOnly);
  if (loaded.length !== 10) {
    fail(name, `expected exactly 10 loaded sessions, got ${loaded.length}`);
    return;
  }
  pass(name);
})();

(function testMessageEvictionUsesRecentActivity() {
  const name = 'message eviction prefers recent activity over creation time';
  const testApp = loadAppCore();
  testApp.state.sessions = Array.from({ length: 11 }, (_, index) => ({
    id: `s${index + 1}`,
    title: `Session ${index + 1}`,
    created: 1000 + index,
    lastMessageAt: 1000 + index,
    messages: [{ id: `m${index + 1}`, role: 'user', content: 'hi', created: 1000 + index }],
  }));
  testApp.state.sessions[0].lastMessageAt = 10_000;

  testApp.saveSessions();

  const recentlyActive = testApp.state.sessions[0];
  if (recentlyActive._serverOnly || recentlyActive.messages.length !== 1) {
    fail(name, 'recently active older-created session was evicted');
    return;
  }
  pass(name);
})();

(function testMessageEvictionReleasesDuplicateHistoryRepresentations() {
  const name = 'message eviction releases converted and raw history representations';
  const testApp = loadAppCore();
  testApp.state.sessions = Array.from({ length: 11 }, (_, index) => ({
    id: `s${index + 1}`,
    title: `Session ${index + 1}`,
    created: 1000 + index,
    lastMessageAt: 1000 + index,
    messages: [{ id: `m${index + 1}`, role: 'assistant', content: 'x'.repeat(1024), created: 1000 + index }],
    _history: {
      rawMessages: [{ sequence: index + 1, role: 'assistant', parts: [{ type: 'text', text: 'x'.repeat(1024) }] }],
      loadedTail: true,
    },
  }));
  testApp.state.activeSessionId = 's11';

  testApp.saveSessions();

  const evicted = testApp.state.sessions.find((session) => session.id === 's1');
  if (!evicted || !evicted._serverOnly || evicted.messages.length !== 0) {
    fail(name, 'expected oldest inactive session converted messages to be evicted');
    return;
  }
  if (Object.prototype.hasOwnProperty.call(evicted, '_history')) {
    fail(name, 'raw server history remained reachable after message eviction');
    return;
  }
  const retained = testApp.state.sessions.find((session) => session.id === 's11');
  if (!retained?._history?.rawMessages?.length || retained.messages.length !== 1) {
    fail(name, 'retained active session lost its history representations');
    return;
  }
  pass(name);
})();

(function testHistoryOnlySessionParticipatesInMessageCacheEviction() {
  const name = 'history-only inactive sessions participate in cache eviction';
  const testApp = loadAppCore();
  testApp.state.sessions = Array.from({ length: 11 }, (_, index) => ({
    id: `raw${index + 1}`,
    title: `Raw ${index + 1}`,
    created: 1000 + index,
    lastMessageAt: 1000 + index,
    messages: [],
    _history: { rawMessages: [{ sequence: index + 1, parts: [{ type: 'text', text: 'large raw history' }] }] },
  }));
  testApp.state.activeSessionId = 'raw11';

  testApp.saveSessions();

  const evicted = testApp.state.sessions.find((session) => session.id === 'raw1');
  if (!evicted?._serverOnly || Object.prototype.hasOwnProperty.call(evicted, '_history')) {
    fail(name, 'history-only session was not fully evicted');
    return;
  }
  pass(name);
})();

function dispatchSwipeListeners(listeners, target, event) {
  const evt = {
    target,
    button: 0,
    isPrimary: true,
    preventDefault() { this.defaultPrevented = true; },
    stopPropagation() { this.propagationStopped = true; },
    stopImmediatePropagation() {
      this.immediatePropagationStopped = true;
      this.propagationStopped = true;
    },
    ...event,
  };
  const list = (listeners.get(evt.type) || []).slice().sort((a, b) => Number(b.capture) - Number(a.capture));
  for (const entry of list) {
    entry.listener(evt);
    if (evt.immediatePropagationStopped) break;
  }
  return evt;
}

function makeSwipeEventTarget(defaultTarget = null) {
  const listeners = new Map();
  const target = {
    addEventListener(type, listener, options) {
      const list = listeners.get(type) || [];
      list.push({ listener, capture: options === true || Boolean(options?.capture) });
      listeners.set(type, list);
    },
    removeEventListener(type, listener) {
      const list = listeners.get(type) || [];
      const idx = list.findIndex((entry) => entry.listener === listener);
      if (idx !== -1) list.splice(idx, 1);
      listeners.set(type, list);
    },
    dispatchEvent(event) {
      return dispatchSwipeListeners(listeners, defaultTarget || target, event);
    },
  };
  return target;
}

function makeSwipePanel(width = 320, { ownerDocument = null } = {}) {
  const listeners = new Map();
  const styleValues = new Map();
  const classes = new Set();
  const syncClassName = (panel) => { panel.className = Array.from(classes).join(' '); };
  const panel = {
    className: '',
    ownerDocument,
    offsetWidth: width,
    style: {
      setProperty(name, value) { styleValues.set(String(name), String(value)); },
      removeProperty(name) { const value = styleValues.get(String(name)) || ''; styleValues.delete(String(name)); return value; },
      getPropertyValue(name) { return styleValues.get(String(name)) || ''; },
    },
    classList: {
      add(...tokens) { tokens.forEach((token) => classes.add(token)); syncClassName(panel); },
      remove(...tokens) { tokens.forEach((token) => classes.delete(token)); syncClassName(panel); },
      contains(token) { return classes.has(token); },
      toggle(token, force) {
        const enabled = force === undefined ? !classes.has(token) : Boolean(force);
        if (enabled) classes.add(token); else classes.delete(token);
        syncClassName(panel);
        return enabled;
      },
    },
    addEventListener(type, listener, options) {
      const list = listeners.get(type) || [];
      list.push({ listener, capture: options === true || Boolean(options?.capture) });
      listeners.set(type, list);
    },
    removeEventListener(type, listener) {
      const list = listeners.get(type) || [];
      const idx = list.findIndex((entry) => entry.listener === listener);
      if (idx !== -1) list.splice(idx, 1);
      listeners.set(type, list);
    },
    dispatchEvent(event) {
      return dispatchSwipeListeners(listeners, panel, event);
    },
    getBoundingClientRect() { return { width, height: 600, top: 0, left: 0, right: width, bottom: 600 }; },
    setPointerCapture() {},
    releasePointerCapture() {},
  };
  return panel;
}

(function testPanelSwipeToCloseTracksLeftPanelAndCommits() {
  const name = 'initPanelSwipeToClose tracks a left panel and commits on touch move';
  const testApp = loadAppCore();
  const panel = makeSwipePanel(320);
  let closed = 0;
  testApp.initPanelSwipeToClose({
    panel,
    side: 'left',
    isEnabled: () => true,
    isOpen: () => true,
    onClose: () => { closed += 1; },
  });

  panel.dispatchEvent({ type: 'pointerdown', pointerId: 1, clientX: 220, clientY: 20 });
  const move = panel.dispatchEvent({ type: 'pointermove', pointerId: 1, clientX: 130, clientY: 24 });
  if (!move.defaultPrevented) {
    fail(name, 'dragging move should prevent the browser horizontal pan');
    return;
  }
  if (panel.style.getPropertyValue('--panel-swipe-offset-x') !== '-90px') {
    fail(name, `expected panel to follow finger at -90px, got ${panel.style.getPropertyValue('--panel-swipe-offset-x')}`);
    return;
  }
  if (!panel.classList.contains('panel-swipe-dragging')) {
    fail(name, 'drag class should be present while moving');
    return;
  }
  panel.dispatchEvent({ type: 'pointerup', pointerId: 1, clientX: 120, clientY: 24 });
  if (closed !== 1) {
    fail(name, `expected close callback once, got ${closed}`);
    return;
  }
  if (panel.style.getPropertyValue('--panel-swipe-offset-x')) {
    fail(name, 'drag offset should be cleared after release');
    return;
  }
  pass(name);
})();

(function testPanelSwipeToCloseIgnoresVerticalScrollIntent() {
  const name = 'initPanelSwipeToClose leaves vertical scrolling alone';
  const testApp = loadAppCore();
  const panel = makeSwipePanel(320);
  let closed = 0;
  testApp.initPanelSwipeToClose({
    panel,
    side: 'right',
    isEnabled: () => true,
    isOpen: () => true,
    onClose: () => { closed += 1; },
  });

  panel.dispatchEvent({ type: 'pointerdown', pointerId: 1, clientX: 120, clientY: 20 });
  const move = panel.dispatchEvent({ type: 'pointermove', pointerId: 1, clientX: 145, clientY: 120 });
  panel.dispatchEvent({ type: 'pointerup', pointerId: 1, clientX: 180, clientY: 160 });
  if (move.defaultPrevented) {
    fail(name, 'vertical intent should not be prevented');
    return;
  }
  if (closed !== 0) {
    fail(name, `vertical scroll should not close, got ${closed}`);
    return;
  }
  if (panel.classList.contains('panel-swipe-dragging')) {
    fail(name, 'vertical scroll should not enter drag mode');
    return;
  }
  pass(name);
})();

(function testPanelSwipeTracksDocumentMovesAfterPointerDown() {
  const name = 'initPanelSwipeToClose tracks document moves for right drawer drags';
  const testApp = loadAppCore();
  const ownerDocument = makeSwipeEventTarget();
  const panel = makeSwipePanel(320, { ownerDocument });
  let closed = 0;
  testApp.initPanelSwipeToClose({
    panel,
    side: 'right',
    isEnabled: () => true,
    isOpen: () => true,
    onClose: () => { closed += 1; },
  });

  panel.dispatchEvent({ type: 'pointerdown', pointerId: 7, clientX: 100, clientY: 20 });
  const move = ownerDocument.dispatchEvent({ type: 'pointermove', pointerId: 7, clientX: 195, clientY: 24 });
  if (!move.defaultPrevented || !move.immediatePropagationStopped) {
    fail(name, 'document-level drag move should win the event before child click handlers');
    return;
  }
  if (panel.style.getPropertyValue('--panel-swipe-offset-x') !== '95px') {
    fail(name, `expected right drawer to track document move at 95px, got ${panel.style.getPropertyValue('--panel-swipe-offset-x')}`);
    return;
  }
  ownerDocument.dispatchEvent({ type: 'pointerup', pointerId: 7, clientX: 205, clientY: 24 });
  if (closed !== 1) {
    fail(name, `expected document-tracked right drawer drag to close once, got ${closed}`);
    return;
  }
  pass(name);
})();

(function testPanelSwipeSuppressesSyntheticClickAfterDrag() {
  const name = 'initPanelSwipeToClose suppresses the click generated after a drag';
  const testApp = loadAppCore();
  const panel = makeSwipePanel(320);
  let clicked = false;
  testApp.initPanelSwipeToClose({
    panel,
    side: 'right',
    isEnabled: () => true,
    isOpen: () => true,
  });
  panel.addEventListener('click', () => { clicked = true; });

  panel.dispatchEvent({ type: 'pointerdown', pointerId: 1, clientX: 100, clientY: 20 });
  panel.dispatchEvent({ type: 'pointermove', pointerId: 1, clientX: 160, clientY: 22 });
  panel.dispatchEvent({ type: 'pointerup', pointerId: 1, clientX: 160, clientY: 22 });
  const click = panel.dispatchEvent({ type: 'click', pointerId: 1 });

  if (!click.defaultPrevented || !click.immediatePropagationStopped) {
    fail(name, 'post-drag click should be captured and prevented');
    return;
  }
  if (clicked) {
    fail(name, 'post-drag click should not reach row/button handlers');
    return;
  }
  pass(name);
})();

(function testPanelSwipeReleaseDecisionUsesInertiaProjection() {
  const name = 'panel swipe release decision closes when inertia crosses threshold';
  const testApp = loadAppCore();
  const panel = makeSwipePanel(320);
  const decision = testApp.panelSwipeReleaseDecision({
    panel,
    closeDelta: 45,
    velocity: 0.72,
  });

  if (decision.distance >= decision.threshold) {
    fail(name, 'test setup should be below the direct distance threshold');
    return;
  }
  if (!decision.shouldClose) {
    fail(name, `expected inertia projection ${decision.projectedDistance} to cross threshold ${decision.threshold}`);
    return;
  }
  pass(name);
})();

(function testPanelSwipeSmoothedVelocityIgnoresNoisyLastSample() {
  const name = 'panel swipe smoothed velocity ignores a noisy final sample';
  const testApp = loadAppCore();
  const velocity = testApp.panelSwipeSmoothedVelocity([
    { at: 0, closeDelta: 0 },
    { at: 50, closeDelta: 52 },
    { at: 100, closeDelta: 86 },
    { at: 104, closeDelta: 80 },
  ]);

  if (velocity <= 0.6) {
    fail(name, `expected smoothed velocity to preserve the flick, got ${velocity}`);
    return;
  }
  pass(name);
})();

(function testPanelSwipeToCloseCommitsNoisyFlickViaInertia() {
  const name = 'initPanelSwipeToClose commits a noisy flick via inertia projection';
  let now = 0;
  const testApp = loadAppCoreWith({ now: () => now });
  const panel = makeSwipePanel(320);
  let closed = 0;
  let closeDecision = null;
  testApp.initPanelSwipeToClose({
    panel,
    side: 'left',
    isEnabled: () => true,
    isOpen: () => true,
    onClose: (_event, decision) => { closed += 1; closeDecision = decision; },
  });

  panel.dispatchEvent({ type: 'pointerdown', pointerId: 1, clientX: 220, clientY: 20 });
  now = 50;
  panel.dispatchEvent({ type: 'pointermove', pointerId: 1, clientX: 168, clientY: 22 });
  now = 100;
  panel.dispatchEvent({ type: 'pointermove', pointerId: 1, clientX: 134, clientY: 24 });
  now = 104;
  panel.dispatchEvent({ type: 'pointerup', pointerId: 1, clientX: 140, clientY: 24 });

  if (closed !== 1) {
    fail(name, `expected noisy flick to close once, got ${closed}`);
    return;
  }
  if (!closeDecision || closeDecision.distance >= closeDecision.threshold || closeDecision.projectedDistance < closeDecision.threshold) {
    fail(name, `expected inertia, not direct distance, to commit: ${JSON.stringify(closeDecision)}`);
    return;
  }
  pass(name);
})();

(function testPanelSwipeCloseDurationUsesInertialEdgeTime() {
  const name = 'panel swipe close duration uses time to the closing edge';
  const testApp = loadAppCore();
  const duration = testApp.panelSwipeCloseDuration({
    width: 320,
    distance: 180,
    distanceToEdge: 140,
    velocity: 1.4,
  });

  if (duration < 90 || duration > 260) {
    fail(name, `duration should be clamped to sane release timing, got ${duration}`);
    return;
  }
  if (duration >= 260) {
    fail(name, `expected inertial edge time, got fallback-like duration ${duration}`);
    return;
  }
  pass(name);
})();

Promise.all(pendingAsyncTests).then(() => {
  if (failures > 0) {
    process.exit(1);
  }
}).catch((err) => {
  fail('async test runner', err && err.stack ? err.stack : String(err));
  process.exit(1);
});
