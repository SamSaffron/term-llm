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

function makeNode(tagName = 'div') {
  let innerHTML = '';
  let textContent = '';
  return {
    tagName: String(tagName || 'div').toUpperCase(),
    classList: makeClassList(),
    style: {
      setProperty() {},
      removeProperty() {},
    },
    dataset: {},
    children: [],
    attributes: {},
    get innerHTML() {
      return innerHTML;
    },
    set innerHTML(value) {
      innerHTML = String(value || '');
    },
    get textContent() {
      return textContent;
    },
    set textContent(value) {
      textContent = String(value || '');
      innerHTML = textContent;
    },
    value: '',
    checked: false,
    disabled: false,
    hidden: false,
    open: false,
    scrollTop: 0,
    scrollHeight: 0,
    clientHeight: 0,
    appendChild(child) {
      this.children.push(child);
      return child;
    },
    removeChild(child) {
      this.children = this.children.filter((item) => item !== child);
      return child;
    },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    closest() { return null; },
    matches() { return false; },
    setAttribute(name, value) { this.attributes[name] = String(value); },
    getAttribute(name) { return this.attributes[name] || null; },
    removeAttribute(name) { delete this.attributes[name]; },
    addEventListener() {},
    removeEventListener() {},
    focus() {},
    blur() {},
    click() {},
    remove() {},
    scrollTo() {},
    showModal() { this.open = true; },
    close() { this.open = false; },
    getBoundingClientRect() {
      return { top: 0, left: 0, right: 0, bottom: 0, width: 0, height: 0 };
    },
    cloneNode() { return makeNode(tagName); },
    play() { return Promise.resolve(); },
    pause() {},
    select() {},
  };
}

function createHarness() {
  const localStorageData = new Map();
  const nodes = new Map();
  const document = {
    cookie: '',
    body: makeNode('body'),
    documentElement: makeNode('html'),
    getElementById(id) {
      if (!nodes.has(id)) {
        nodes.set(id, makeNode('div'));
      }
      return nodes.get(id);
    },
    createElement(tag) {
      return makeNode(tag);
    },
    createTextNode(text) {
      return { textContent: String(text || '') };
    },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    addEventListener() {},
    removeEventListener() {},
  };

  const windowObj = {
    TERM_LLM_UI_PREFIX: '/ui',
    TERM_LLM_SIDEBAR_SESSIONS: 'all',
    TermLLMApp: {},
    location: {
      pathname: '/chat/ui/',
      search: '',
      hash: '',
      origin: 'https://example.test',
    },
    history: {
      replaceState() {},
      pushState() {},
    },
    matchMedia() {
      return { matches: false, addEventListener() {}, removeEventListener() {} };
    },
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
  };

  const context = {
    window: windowObj,
    document,
    localStorage: {
      getItem(key) {
        return localStorageData.has(key) ? localStorageData.get(key) : null;
      },
      setItem(key, value) {
        localStorageData.set(key, String(value));
      },
      removeItem(key) {
        localStorageData.delete(key);
      },
    },
    navigator: {
      mediaDevices: null,
      serviceWorker: {
        addEventListener() {},
      },
      standalone: false,
    },
    Notification: function Notification() {},
    URL,
    Headers,
    Request,
    Response,
    fetch: async () => new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } }),
    console,
    setTimeout,
    clearTimeout,
    setInterval,
    clearInterval,
    Intl,
  };

  context.Notification.permission = 'denied';
  context.Notification.requestPermission = async () => 'denied';
  context.window.Notification = context.Notification;
  context.window.navigator = context.navigator;
  context.window.localStorage = context.localStorage;
  context.window.document = document;
  context.window.fetch = context.fetch;
  context.window.URL = URL;
  context.window.Headers = Headers;
  context.window.Request = Request;
  context.window.Response = Response;

  vm.runInNewContext(source, context, { filename: 'app-core.js' });

  return {
    app: context.window.TermLLMApp,
    elements: context.window.TermLLMApp.elements,
    state: context.window.TermLLMApp.state,
  };
}

function testSuppressesDuplicatedEffortForModelSuffix() {
  const { app, state, elements } = createHarness();
  state.selectedProvider = 'openai';
  state.selectedModel = 'gpt-5.4-medium';
  state.selectedEffort = 'medium';

  app.updateSessionUsageDisplay({});

  const html = elements.headerStats.innerHTML;
  if (!html.includes('gpt-5.4-medium')) {
    fail('suppress duplicated effort', 'expected model in header', html);
    return;
  }
  if (html.includes('stats-effort')) {
    fail('suppress duplicated effort', 'expected duplicated effort badge to be omitted', html);
    return;
  }
  pass('suppress duplicated effort');
}

function testShowsDistinctEffortWhenModelDoesNotImplyIt() {
  const { app, state, elements } = createHarness();
  state.selectedProvider = 'openai';
  state.selectedModel = 'gpt-5.4';
  state.selectedEffort = 'medium';

  app.updateSessionUsageDisplay({});

  const html = elements.headerStats.innerHTML;
  if (!html.includes('gpt-5.4')) {
    fail('show distinct effort', 'expected model in header', html);
    return;
  }
  if (!html.includes('stats-effort') || !html.includes('medium')) {
    fail('show distinct effort', 'expected distinct effort badge to remain visible', html);
    return;
  }
  pass('show distinct effort');
}

testSuppressesDuplicatedEffortForModelSuffix();
testShowsDistinctEffortWhenModelDoesNotImplyIt();

if (failures > 0) {
  process.exit(1);
}
