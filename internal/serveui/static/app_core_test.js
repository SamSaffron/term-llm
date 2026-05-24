#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const dir = __dirname;
const source = fs.readFileSync(path.join(dir, 'app-core.js'), 'utf8');

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
    disabled: false,
    scrollTop: 0,
    scrollHeight: 0,
    clientHeight: 0,
    appendChild(node) { return node; },
    removeChild() {},
    querySelector() { return null; },
    querySelectorAll() { return []; },
    setAttribute() {},
    removeAttribute() {},
    addEventListener() {},
    removeEventListener() {},
    focus() {},
    closest() { return null; },
    getBoundingClientRect() {
      return { top: 0, left: 0, width: 0, height: 0, bottom: 0, right: 0 };
    },
    cloneNode() { return makeNode(); },
    play() { return Promise.resolve(); },
    pause() {},
  };
}

function loadAppCoreWith({ nodeOverrides = {}, docQSTracker = () => [], navigatorOverrides = {}, initialStorage = {}, agentName = '' } = {}) {
  const nodes = new Map(Object.entries(nodeOverrides));
  const cookieWrites = [];
  const document = {
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
    setTimeout(fn) { return 1; },
    clearTimeout() {},
    location: { pathname: '/chat', href: '/chat' },
    history: { pushState() {} },
    MediaRecorder: undefined,
    focus() {},
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
    TextEncoder,
    TextDecoder,
  };
  context.globalThis = context;

  vm.runInNewContext(source, context, { filename: 'app-core.js' });
  context.window.TermLLMApp.__testCookieWrites = cookieWrites;
  return context.window.TermLLMApp;
}

function loadAppCore() {
  return loadAppCoreWith();
}

const app = loadAppCore();

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
  if (connectionNode.textContent !== 'Network offline') {
    fail(name, `got ${JSON.stringify(connectionNode.textContent)}`);
    return;
  }
  if (!classes.has('bad')) {
    fail(name, 'offline warning should have bad class');
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
    scrollTop: 820,
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

if (failures > 0) {
  process.exit(1);
}
