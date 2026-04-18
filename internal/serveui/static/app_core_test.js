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

function loadAppCore() {
  const nodes = new Map();
  const document = {
    body: makeNode(),
    documentElement: makeNode(),
    cookie: '',
    getElementById(id) {
      if (!nodes.has(id)) nodes.set(id, makeNode());
      return nodes.get(id);
    },
    createElement() { return makeNode(); },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    addEventListener() {},
    removeEventListener() {},
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

  const windowObj = {
    TermLLMApp: {},
    TERM_LLM_UI_PREFIX: '/chat',
    TERM_LLM_SIDEBAR_SESSIONS: 'all',
    navigator: { standalone: false },
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
    navigator: {
      mediaDevices: null,
      serviceWorker: {
        register: async () => ({ scope: '/chat/' }),
        ready: Promise.resolve({ showNotification: async () => {} }),
      },
      clipboard: { writeText: async () => {} },
      standalone: false,
    },
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
  return context.window.TermLLMApp;
}

const app = loadAppCore();

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

if (failures > 0) {
  process.exit(1);
}
