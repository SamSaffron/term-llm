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
    backToHubLink: new Element('a'),
  };
  elements.backToHubLink.classList.add('back-to-hub-link', 'hidden');
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
  const document = new Element('document');
  document.createElement = (tag) => new Element(tag);
  const navigator = { platform: options.platform || 'Linux x86_64' };
  const context = {
    window: { TermLLMApp: app, TERM_LLM_HUB: options.hub },
    document,
    navigator,
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
  return { app, elements, state, document, get renderSidebarCount() { return renderSidebarCount; } };
}

function keydownEvent(overrides = {}) {
  let prevented = false;
  return {
    type: 'keydown',
    key: 'k',
    metaKey: false,
    ctrlKey: false,
    altKey: false,
    shiftKey: false,
    preventDefault() { prevented = true; },
    get defaultPrevented() { return prevented; },
    ...overrides,
  };
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
    const badges = elements.widgetsModalList.querySelectorAll('.widget-state');
    assertEqual(badges.length, 1, 'only running widgets render a state indicator');
    assert(badges[0].classList.contains('running'), 'running widget renders green dot class');
    assertEqual(badges[0].textContent, '', 'running indicator has no repeated status text');
    assertEqual(badges[0].getAttribute('aria-label'), 'Running', 'running indicator is accessible');
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

  await run('back to hub link stays hidden without hub context', () => {
    const { elements } = createHarness();
    assert(elements.backToHubLink.classList.contains('hidden'), 'link is hidden without TERM_LLM_HUB');
  });

  await run('back to hub link renders from hub context', () => {
    const { elements } = createHarness({ hub: { url: '/', nodeId: 'jarvis', nodeName: 'Jarvis' } });
    assert(!elements.backToHubLink.classList.contains('hidden'), 'link is visible with TERM_LLM_HUB');
    assertEqual(elements.backToHubLink.href, '/', 'link points at the hub url');
    assertEqual(elements.backToHubLink.title, 'Back to Hub (this node: Jarvis)', 'title names the node');
  });

  await run('back to hub link ignores hub context without a url', () => {
    const { elements } = createHarness({ hub: { nodeId: 'jarvis' } });
    assert(elements.backToHubLink.classList.contains('hidden'), 'link stays hidden when hub context has no url');
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

  await run('cmd+k opens widgets modal on mac', async () => {
    const { app, elements, state, document } = createHarness({ platform: 'MacIntel' });
    state.widgets = [{ id: 'w1', mount: 'one', title: 'One' }];
    state.widgetsLoaded = true;
    app.renderWidgetSidebar();
    elements.widgetsModal.classList.add('hidden');

    const event = keydownEvent({ metaKey: true });
    await document.dispatchEvent(event);

    assert(event.defaultPrevented, 'cmd+k preventDefault called');
    assert(!elements.widgetsModal.classList.contains('hidden'), 'modal is open');
  });

  await run('cmd+k closes widgets modal when already open on mac', async () => {
    const { app, elements, state, document } = createHarness({ platform: 'MacIntel' });
    state.widgets = [{ id: 'w1', mount: 'one', title: 'One' }];
    state.widgetsLoaded = true;
    app.renderWidgetSidebar();
    // Modal starts open (no 'hidden' class)

    await document.dispatchEvent(keydownEvent({ metaKey: true }));

    assert(elements.widgetsModal.classList.contains('hidden'), 'modal closes on second press');
  });

  await run('ctrl+k toggles widgets modal on linux', async () => {
    const { app, elements, state, document } = createHarness({ platform: 'Linux x86_64' });
    state.widgets = [{ id: 'w1', mount: 'one', title: 'One' }];
    state.widgetsLoaded = true;
    app.renderWidgetSidebar();
    elements.widgetsModal.classList.add('hidden');

    await document.dispatchEvent(keydownEvent({ ctrlKey: true }));

    assert(!elements.widgetsModal.classList.contains('hidden'), 'ctrl+k opens modal on linux');
  });

  await run('ctrl+k is ignored on mac (preserves emacs kill-line)', async () => {
    const { app, elements, state, document } = createHarness({ platform: 'MacIntel' });
    state.widgets = [{ id: 'w1', mount: 'one', title: 'One' }];
    state.widgetsLoaded = true;
    app.renderWidgetSidebar();
    elements.widgetsModal.classList.add('hidden');

    const event = keydownEvent({ ctrlKey: true });
    await document.dispatchEvent(event);

    assert(!event.defaultPrevented, 'preventDefault NOT called for ctrl+k on mac');
    assert(elements.widgetsModal.classList.contains('hidden'), 'modal stays closed');
  });

  await run('cmd+k is ignored when widgets button is hidden', async () => {
    const { elements, state, document } = createHarness({ platform: 'MacIntel' });
    state.widgets = [];
    state.widgetsLoaded = true;
    // renderWidgetSidebar not called or no widgets => button stays hidden by default
    elements.widgetsOpenBtn.classList.add('hidden');
    elements.widgetsModal.classList.add('hidden');

    const event = keydownEvent({ metaKey: true });
    await document.dispatchEvent(event);

    assert(!event.defaultPrevented, 'preventDefault NOT called when no widgets');
    assert(elements.widgetsModal.classList.contains('hidden'), 'modal stays closed');
  });

  await run('cmd+shift+k does not trigger', async () => {
    const { app, elements, state, document } = createHarness({ platform: 'MacIntel' });
    state.widgets = [{ id: 'w1', mount: 'one', title: 'One' }];
    state.widgetsLoaded = true;
    app.renderWidgetSidebar();
    elements.widgetsModal.classList.add('hidden');

    const event = keydownEvent({ metaKey: true, shiftKey: true });
    await document.dispatchEvent(event);

    assert(!event.defaultPrevented, 'shift modifier blocks the binding');
    assert(elements.widgetsModal.classList.contains('hidden'), 'modal stays closed');
  });

  await run('cmd+ctrl+k does not trigger on mac', async () => {
    const { app, elements, state, document } = createHarness({ platform: 'MacIntel' });
    state.widgets = [{ id: 'w1', mount: 'one', title: 'One' }];
    state.widgetsLoaded = true;
    app.renderWidgetSidebar();
    elements.widgetsModal.classList.add('hidden');

    const event = keydownEvent({ metaKey: true, ctrlKey: true });
    await document.dispatchEvent(event);

    assert(!event.defaultPrevented, 'both modifiers blocks the binding on mac');
  });

  if (failures > 0) process.exit(1);
})();
