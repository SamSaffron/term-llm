#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const source = fs.readFileSync(path.join(__dirname, 'app-sidebar.js'), 'utf8');
let failures = 0;

function fail(name, message, details) {
  console.error('FAIL:', name, '-', message);
  if (details) console.error('      ', details);
  failures += 1;
}

function pass(name) {
  console.log('PASS:', name);
}

function assert(condition, message) {
  if (!condition) throw new Error(message);
}

function assertEqual(actual, expected, message) {
  if (actual !== expected) throw new Error(`${message}: expected ${JSON.stringify(expected)}, got ${JSON.stringify(actual)}`);
}

class ClassList {
  constructor(element) { this.element = element; }
  _values() { return new Set(String(this.element.className || '').split(/\s+/).filter(Boolean)); }
  _set(values) { this.element.className = Array.from(values).join(' '); }
  add(...tokens) { const values = this._values(); tokens.forEach((t) => values.add(t)); this._set(values); }
  remove(...tokens) { const values = this._values(); tokens.forEach((t) => values.delete(t)); this._set(values); }
  contains(token) { return this._values().has(token); }
  toggle(token, force) {
    const values = this._values();
    const add = force === undefined ? !values.has(token) : Boolean(force);
    if (add) values.add(token); else values.delete(token);
    this._set(values);
    return add;
  }
}

class Element {
  constructor(tagName) {
    this.tagName = String(tagName || '').toUpperCase();
    this.children = [];
    this.parentNode = null;
    this.dataset = {};
    this.attributes = new Map();
    this.className = '';
    this.classList = new ClassList(this);
    this.style = {};
    this.listeners = new Map();
    this.textContent = '';
    this.value = '';
    this.href = '';
    this.title = '';
  }
  appendChild(child) {
    if (child.parentNode) {
      const idx = child.parentNode.children.indexOf(child);
      if (idx !== -1) child.parentNode.children.splice(idx, 1);
    }
    child.parentNode = this;
    this.children.push(child);
    return child;
  }
  replaceChildren(...nodes) {
    this.children.forEach((child) => { child.parentNode = null; });
    this.children = [];
    nodes.forEach((node) => { if (node) this.appendChild(node); });
  }
  setAttribute(name, value) { this.attributes.set(name, String(value)); if (name === 'class') this.className = String(value); }
  getAttribute(name) { return this.attributes.get(name) || null; }
  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) || [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }
  async dispatchEvent(event) {
    const evt = event || { type: '' };
    const listeners = this.listeners.get(evt.type) || [];
    for (const listener of listeners) await listener(evt);
  }
  focus() { this.focused = true; }
  matches(selector) {
    if (selector.startsWith('.')) return this.classList.contains(selector.slice(1));
    return this.tagName.toLowerCase() === selector.toLowerCase();
  }
  querySelectorAll(selector) {
    const results = [];
    const walk = (node) => {
      node.children.forEach((child) => {
        if (child.matches(selector)) results.push(child);
        walk(child);
      });
    };
    walk(this);
    return results;
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
}

function createHarness(options = {}) {
  const elements = {
    widgetsOpenBtn: new Element('button'),
    widgetsModal: new Element('div'),
    widgetsModalList: new Element('div'),
    widgetsModalCloseBtn: new Element('button'),
    sidebarSearchInput: new Element('input'),
  };
  const state = {
    widgets: [],
    widgetsLoaded: false,
    showWidgetsSidebar: true,
    sidebarSessionCategories: ['all'],
    showHiddenSessions: false,
    sidebarSearchQuery: '',
    sidebarSearchResults: null,
    sidebarSearchLoading: false,
  };
  let renderSidebarCount = 0;
  const app = {
    UI_PREFIX: '/chat',
    state,
    elements,
    requestHeaders() { return {}; },
    renderSidebar() { renderSidebarCount += 1; },
  };
  const document = { createElement: (tag) => new Element(tag) };
  const context = {
    window: { TermLLMApp: app },
    document,
    URLSearchParams,
    console,
    clearTimeout,
    setTimeout: options.setTimeout || ((fn) => { fn(); return 1; }),
    fetch: options.fetch || (async () => new Response(JSON.stringify({ sessions: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } })),
    Response,
    AbortController,
  };
  context.globalThis = context;
  vm.runInNewContext(source, context, { filename: 'app-sidebar.js' });
  return { app, elements, state, get renderSidebarCount() { return renderSidebarCount; } };
}

async function run(name, fn) {
  try {
    await fn();
    pass(name);
  } catch (err) {
    fail(name, err.message, err.stack);
  }
}

(async () => {
  await run('renderWidgetSidebar shows button and modal links', () => {
    const { app, elements, state } = createHarness();
    state.widgets = [
      { id: 'w1', mount: 'one', title: 'One', description: 'First', state: 'running' },
      { id: 'w2', mount: 'two', title: 'Two', state: 'stopped' },
    ];
    state.widgetsLoaded = true;

    app.renderWidgetSidebar();

    assert(!elements.widgetsOpenBtn.classList.contains('hidden'), 'widgets button is visible');
    const links = elements.widgetsModalList.querySelectorAll('.widget-link');
    assertEqual(links.length, 2, 'modal contains all widgets');
    assertEqual(links[0].href, '/chat/widgets/one/', 'first link points to widget');
  });

  await run('renderWidgetSidebar hides button when preference is off', () => {
    const { app, elements, state } = createHarness();
    state.widgets = [{ id: 'w1', mount: 'one', title: 'One' }];
    state.widgetsLoaded = true;
    state.showWidgetsSidebar = false;

    app.renderWidgetSidebar();

    assert(elements.widgetsOpenBtn.classList.contains('hidden'), 'widgets button is hidden');
    assertEqual(elements.widgetsModalList.children.length, 0, 'modal list is cleared');
  });

  await run('sidebar search fetches server results', async () => {
    let requested = '';
    const { elements, state } = createHarness({
      fetch: async (url) => {
        requested = String(url);
        return new Response(JSON.stringify({ sessions: [{ id: 's1', short_title: 'Linux', snippet: 'match' }] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
    });

    elements.sidebarSearchInput.value = 'linux';
    await elements.sidebarSearchInput.dispatchEvent({ type: 'input' });
    await new Promise((resolve) => setImmediate(resolve));

    assert(requested.includes('/v1/sessions/search?'), 'search endpoint requested');
    assert(requested.includes('q=linux'), 'query sent');
    assertEqual(state.sidebarSearchResults.length, 1, 'one result mapped');
    assertEqual(state.sidebarSearchResults[0].title, 'Linux', 'result title mapped');
  });

  if (failures > 0) process.exit(1);
})();
