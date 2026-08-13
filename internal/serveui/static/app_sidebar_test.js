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
    hubAgentLinks: new Element('nav'),
  };
  elements.backToHubLink.classList.add('back-to-hub-link', 'hidden');
  elements.hubAgentLinks.classList.add('hub-agent-links', 'hidden');
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
    createEl(tag, className, text) {
      const element = document.createElement(tag);
      if (className) element.className = className;
      if (text !== undefined) element.textContent = text;
      return element;
    },
    UI_PREFIX: '/chat',
    state,
    elements,
    requestHeaders() { return {}; },
    renderSidebar() { renderSidebarCount += 1; },
  };
  const document = new Element('document');
  document.createElement = (tag) => new Element(tag);
  document.visibilityState = options.visibilityState || 'visible';
  const navigator = { platform: options.platform || 'Linux x86_64' };
  const origin = options.origin || 'https://node.example.test';
  const windowObj = new Element('window');
  windowObj.TermLLMApp = app;
  windowObj.TERM_LLM_HUB = options.hub;
  windowObj.location = {
    origin,
    pathname: options.pathname || '/chat/',
    protocol: `${new URL(origin).protocol}`,
    host: new URL(origin).host,
  };
  const nativeFetchRequests = [];
  const apiFetchRequests = [];
  const nativeFetchImpl = options.nativeFetch || (async () => nodesResponse([]));
  const apiFetchImpl = options.apiFetch || options.fetch || (async () => new Response(JSON.stringify({ sessions: [] }), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  }));
  const trackedNativeFetch = (...args) => {
    nativeFetchRequests.push(args);
    return nativeFetchImpl(...args);
  };
  const DateImpl = options.now
    ? class extends Date { static now() { return options.now(); } }
    : Date;
  const context = {
    window: windowObj,
    document,
    navigator,
    URL,
    URLSearchParams,
    Date: DateImpl,
    console,
    clearTimeout: options.clearTimeout || clearTimeout,
    setTimeout: options.setTimeout || setTimeout,
    fetch: trackedNativeFetch,
    Response,
    AbortController,
  };
  context.globalThis = context;
  app.apiFetch = (...args) => {
    apiFetchRequests.push(args);
    return apiFetchImpl(...args);
  };
  vm.runInNewContext(source, context, { filename: 'app-sidebar.js' });
  return {
    app,
    elements,
    state,
    document,
    window: windowObj,
    nativeFetchRequests,
    apiFetchRequests,
    get renderSidebarCount() { return renderSidebarCount; },
  };
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

const flushAsync = async () => {
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
};

const nodesResponse = (nodes, status = 200) => new Response(JSON.stringify({ nodes }), {
  status,
  headers: { 'Content-Type': 'application/json' },
});

function createFakeTimers() {
  let nextId = 1;
  const timers = [];
  return {
    timers,
    setTimeout(fn, delay) {
      const timer = { id: nextId, fn, delay, cleared: false, fired: false };
      nextId += 1;
      timers.push(timer);
      return timer.id;
    },
    clearTimeout(id) {
      const timer = timers.find((entry) => entry.id === id);
      if (timer) timer.cleared = true;
    },
    active(delay) {
      return timers.filter((timer) => !timer.cleared && !timer.fired
        && (delay === undefined || timer.delay === delay));
    },
    fire(timer) {
      timer.fired = true;
      timer.fn();
    },
  };
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

  await run('back to hub link stays hidden without hub context and makes no Hub request', async () => {
    const { elements, nativeFetchRequests, apiFetchRequests } = createHarness();
    await flushAsync();
    assert(elements.backToHubLink.classList.contains('hidden'), 'link is hidden without TERM_LLM_HUB');
    assert(elements.hubAgentLinks.classList.contains('hidden'), 'agent links are hidden without TERM_LLM_HUB');
    assertEqual(nativeFetchRequests.length, 0, 'no native request is made without Hub context');
    assertEqual(apiFetchRequests.length, 0, 'no app API request is made without Hub context');
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

  await run('cross-origin Hub context keeps agent links hidden and makes zero requests', async () => {
    const { elements, nativeFetchRequests, apiFetchRequests } = createHarness({
      origin: 'https://node.example.test',
      hub: { url: 'https://hub.example.test/hub/', nodeId: 'alpha' },
    });
    await flushAsync();

    assert(elements.hubAgentLinks.classList.contains('hidden'), 'cross-origin agent links stay hidden');
    assertEqual(elements.hubAgentLinks.children.length, 0, 'cross-origin list stays empty');
    assertEqual(nativeFetchRequests.length, 0, 'cross-origin Hub performs no native fetches');
    assertEqual(apiFetchRequests.length, 0, 'cross-origin Hub performs no app API fetches');
  });

  await run('Hub agents use native fetch at the mounted API URL with reachable stable order', async () => {
    const { elements, nativeFetchRequests, apiFetchRequests } = createHarness({
      hub: { url: '/hub/', nodeId: 'alpha-a' },
      nativeFetch: async () => nodesResponse([
        { id: 'beta', name: 'Beta', status: { reachable: true }, new_session_path: '/hub/node/beta/?new=1' },
        { id: 'alpha-b', name: 'Alpha', status: { reachable: true }, new_session_path: '/hub/node/alpha-b/?new=1' },
        { id: 'down', name: 'Aardvark', status: { reachable: false }, new_session_path: '/hub/node/down/?new=1' },
        { id: 'alpha-z', name: 'alpha', status: { reachable: true }, new_session_path: '/hub/node/alpha-z/?new=1' },
        { id: 'alpha-a', name: 'Alpha', status: { reachable: true }, new_session_path: '/hub/node/alpha-a/?new=1' },
      ]),
    });
    await flushAsync();

    assertEqual(String(nativeFetchRequests[0][0]), '/hub/api/nodes', 'mounted Hub API URL is exact');
    assertEqual(nativeFetchRequests[0].length, 2, 'Hub request passes only native fetch options');
    assert(nativeFetchRequests[0][1].signal instanceof AbortSignal, 'Hub request has an abort signal');
    assertEqual(apiFetchRequests.length, 0, 'Hub request never uses app.apiFetch');
    const links = elements.hubAgentLinks.querySelectorAll('.hub-agent-link');
    assertEqual(links.length, 4, 'only reachable nodes with safe targets render');
    assertEqual(links.map((link) => link.href).join(','), [
      '/hub/node/alpha-a/?new=1',
      '/hub/node/alpha-b/?new=1',
      '/hub/node/alpha-z/?new=1',
      '/hub/node/beta/?new=1',
    ].join(','), 'agents sort by folded display name, raw name, then ID');
    assert(!elements.hubAgentLinks.classList.contains('hidden'), 'valid agent list is visible');
    const icons = elements.hubAgentLinks.querySelectorAll('.hub-agent-icon');
    assertEqual(icons.length, 4, 'every agent row renders one robot icon');
    icons.forEach((icon) => assertEqual(icon.getAttribute('aria-hidden'), 'true', 'robot icon is decorative'));
  });

  await run('hidden Hub startup waits for visibility before fetching', async () => {
    const { document, elements, nativeFetchRequests } = createHarness({
      visibilityState: 'hidden',
      hub: { url: '/hub/', nodeId: 'alpha' },
      nativeFetch: async () => nodesResponse([
        { id: 'alpha', name: 'Alpha', status: { reachable: true }, new_session_path: '/hub/node/alpha/?new=1' },
      ]),
    });
    await flushAsync();
    assertEqual(nativeFetchRequests.length, 0, 'hidden startup makes no Hub request');
    assert(elements.hubAgentLinks.classList.contains('hidden'), 'hidden startup keeps the list hidden');

    document.visibilityState = 'visible';
    await document.dispatchEvent({ type: 'visibilitychange' });
    await flushAsync();

    assertEqual(nativeFetchRequests.length, 1, 'becoming visible fetches Hub agents');
    assert(!elements.hubAgentLinks.classList.contains('hidden'), 'visible successful fetch reveals the list');
  });

  await run('empty Hub nodes response clears and hides the list', async () => {
    const { elements } = createHarness({
      hub: { url: '/hub/', nodeId: 'alpha' },
      nativeFetch: async () => nodesResponse([]),
    });
    elements.hubAgentLinks.appendChild(new Element('a'));
    elements.hubAgentLinks.classList.remove('hidden');
    await flushAsync();

    assert(elements.hubAgentLinks.classList.contains('hidden'), 'empty nodes hides the list');
    assertEqual(elements.hubAgentLinks.children.length, 0, 'empty nodes clears stale rows');
  });

  await run('Hub agent targets follow resume, active, recent, new, and proxy precedence', async () => {
    const { elements } = createHarness({
      hub: { url: '/', nodeId: 'resume' },
      nativeFetch: async () => nodesResponse([
        {
          id: 'resume', name: 'Direct', status: { reachable: true },
          sessions: {
            resume_path: '/node/resume/direct',
            active: [{ resume_path: '/node/resume/active' }],
            recent: [{ resume_path: '/node/resume/recent' }],
          },
          new_session_path: '/node/resume/?new=1', proxy_path: '/node/resume/',
        },
        {
          id: 'active', name: 'From active', status: { reachable: true },
          sessions: { active: [{ resume_path: '/node/active/session' }], recent: [{ resume_path: '/node/active/recent' }] },
          new_session_path: '/node/active/?new=1',
        },
        {
          id: 'recent', name: 'From recent', status: { reachable: true },
          sessions: { resume_path: 'https://unsafe.test/session', recent: [{ resume_path: '/node/recent/session' }] },
          new_session_path: '/node/recent/?new=1',
        },
        { id: 'new', name: 'New fallback', status: { reachable: true }, new_session_path: '/node/new/?new=1', proxy_path: '/node/new/' },
        { id: 'proxy', name: 'Proxy fallback', status: { reachable: true }, new_session_path: 'javascript:bad', proxy_path: '/node/proxy/?source=hub#sessions' },
        { id: 'unsafe', name: 'Unsafe', status: { reachable: true }, new_session_path: '//evil.test/', proxy_path: 'https://evil.test/' },
        { id: 'unsafe-backslash', name: 'Unsafe backslash', status: { reachable: true }, new_session_path: '/\\evil.test/', proxy_path: '/\\\\evil.test/' },
      ]),
    });
    await flushAsync();

    const targetsByName = new Map(elements.hubAgentLinks.querySelectorAll('.hub-agent-link').map((link) => [
      link.querySelector('.hub-agent-name').textContent,
      link.href,
    ]));
    assertEqual(targetsByName.get('Direct'), '/node/resume/direct', 'sessions.resume_path wins');
    assertEqual(targetsByName.get('From active'), '/node/active/session', 'first active resume path is used');
    assertEqual(targetsByName.get('From recent'), '/node/recent/session', 'first recent resume path is used');
    assertEqual(targetsByName.get('New fallback'), '/node/new/?new=1', 'new session path is used');
    assertEqual(targetsByName.get('Proxy fallback'), '/node/proxy/?source=hub&new=1#sessions', 'proxy fallback safely appends the new query');
    assert(!targetsByName.has('Unsafe'), 'node without a root-relative target is skipped');
    assert(!targetsByName.has('Unsafe backslash'), 'backslash authority escapes are skipped');
  });

  await run('Hub agent attention appears after a background run finishes', async () => {
    let now = 1000;
    let running = true;
    const { elements, window } = createHarness({
      now: () => now,
      hub: { url: '/', nodeId: 'current' },
      nativeFetch: async () => nodesResponse([
        { id: 'current', name: 'Current', status: { reachable: true }, sessions: { active_count: running ? 1 : 0 }, new_session_path: '/node/current/' },
        { id: 'worker', name: 'Worker', status: { reachable: true }, sessions: { active_count: running ? 1 : 0 }, new_session_path: '/node/worker/' },
      ]),
    });
    await flushAsync();
    assertEqual(elements.hubAgentLinks.querySelectorAll('.hub-agent-attention').length, 0, 'running agents do not ask for attention');

    running = false;
    now += 60000;
    await window.dispatchEvent({ type: 'focus' });
    await flushAsync();

    const links = elements.hubAgentLinks.querySelectorAll('.hub-agent-link');
    const current = links.find((link) => link.querySelector('.hub-agent-name').textContent === 'Current');
    const worker = links.find((link) => link.querySelector('.hub-agent-name').textContent === 'Worker');
    assertEqual(current.getAttribute('aria-current'), 'true', 'current node keeps selected state');
    assertEqual(current.querySelector('.hub-agent-attention'), null, 'current node never duplicates session attention');
    assertEqual(worker.children[0].className, 'hub-agent-icon', 'robot icon owns the aligned leading column');
    assertEqual(worker.children[1].className, 'hub-agent-name', 'agent name follows the icon');
    assertEqual(worker.children[2].className, 'hub-agent-attention', 'attention dot owns the trailing column');
    const dot = worker.querySelector('.hub-agent-attention');
    assertEqual(dot.title, 'Needs attention', 'attention dot has an explicit title');
    assertEqual(dot.getAttribute('aria-hidden'), 'true', 'visual dot is hidden from assistive technology');
    assertEqual(worker.querySelector('.visually-hidden').textContent, 'Needs attention', 'attention is announced once');
  });

  await run('initial Hub 401 hides and clears agent links', async () => {
    const { elements } = createHarness({
      hub: { url: '/hub/', nodeId: 'alpha' },
      nativeFetch: async () => new Response('unauthorized', { status: 401 }),
    });
    elements.hubAgentLinks.appendChild(new Element('a'));
    elements.hubAgentLinks.classList.remove('hidden');
    await flushAsync();

    assert(elements.hubAgentLinks.classList.contains('hidden'), 'initial 401 hides the list');
    assertEqual(elements.hubAgentLinks.children.length, 0, 'initial 401 clears stale rows');
  });

  await run('Hub 401 after success authoritatively clears the valid render', async () => {
    let now = 1000;
    let call = 0;
    const { elements, nativeFetchRequests, window } = createHarness({
      now: () => now,
      hub: { url: '/hub/', nodeId: 'alpha' },
      nativeFetch: async () => {
        call += 1;
        if (call === 1) return nodesResponse([
          { id: 'alpha', name: 'Alpha', status: { reachable: true }, new_session_path: '/hub/node/alpha/?new=1' },
        ]);
        return new Response('unauthorized', { status: 401 });
      },
    });
    await flushAsync();
    assert(!elements.hubAgentLinks.classList.contains('hidden'), 'successful render starts visible');

    now += 60000;
    await window.dispatchEvent({ type: 'focus' });
    await flushAsync();

    assertEqual(nativeFetchRequests.length, 2, 'focus performs the authoritative auth refresh');
    assert(elements.hubAgentLinks.classList.contains('hidden'), '401 after success hides the list');
    assertEqual(elements.hubAgentLinks.children.length, 0, '401 after success clears valid rows');
  });

  await run('transient Hub refresh failure preserves the last valid render', async () => {
    let now = 1000;
    let call = 0;
    const { elements, nativeFetchRequests, window } = createHarness({
      now: () => now,
      hub: { url: '/hub/', nodeId: 'alpha' },
      nativeFetch: async () => {
        call += 1;
        if (call === 1) return nodesResponse([
          { id: 'alpha', name: 'Alpha', status: { reachable: true }, new_session_path: '/hub/node/alpha/?new=1' },
        ]);
        return new Response('temporary failure', { status: 503 });
      },
    });
    await flushAsync();
    const initialLink = elements.hubAgentLinks.querySelector('.hub-agent-link');

    now += 60000;
    await window.dispatchEvent({ type: 'focus' });
    await flushAsync();

    assertEqual(nativeFetchRequests.length, 2, 'focus refreshes after the minimum interval');
    assert(!elements.hubAgentLinks.classList.contains('hidden'), 'transient failure leaves the valid list visible');
    assertEqual(elements.hubAgentLinks.querySelector('.hub-agent-link'), initialLink, 'transient failure preserves rendered rows');
  });

  await run('Hub focus and visibility refreshes share a 60 second throttle', async () => {
    let now = 1000;
    const clock = createFakeTimers();
    const { app, document, nativeFetchRequests, window } = createHarness({
      now: () => now,
      hub: { url: '/', nodeId: 'alpha' },
      nativeFetch: async () => nodesResponse([]),
      setTimeout: clock.setTimeout,
      clearTimeout: clock.clearTimeout,
    });
    await flushAsync();

    now += 1000;
    await window.dispatchEvent({ type: 'focus' });
    await document.dispatchEvent({ type: 'visibilitychange' });
    assertEqual(nativeFetchRequests.length, 1, 'focus and visibility do not fetch inside the throttle window');
    const refreshTimers = clock.active(59000);
    assertEqual(refreshTimers.length, 1, 'return events share one trailing refresh timer');
    assertEqual(refreshTimers[0].delay, 59000, 'trailing refresh waits for the remaining throttle interval');

    clock.fire(refreshTimers[0]);
    await flushAsync();
    assertEqual(nativeFetchRequests.length, 2, 'trailing refresh fetches once');
    assertEqual(clock.active(app.HUB_AGENT_LINKS_FETCH_TIMEOUT_MS).length, 0, 'completed fetch clears its timeout');
  });

  await run('Hub agent refresh stays single-flight', async () => {
    let now = 1000;
    let releaseFetch;
    const pendingResponse = new Promise((resolve) => { releaseFetch = resolve; });
    const { nativeFetchRequests, window } = createHarness({
      now: () => now,
      hub: { url: '/', nodeId: 'alpha' },
      nativeFetch: async () => pendingResponse,
    });
    assertEqual(nativeFetchRequests.length, 1, 'startup begins one Hub request');

    now += 60000;
    await window.dispatchEvent({ type: 'focus' });
    assertEqual(nativeFetchRequests.length, 1, 'focus reuses the startup request while it is in flight');

    releaseFetch(nodesResponse([]));
    await flushAsync();
  });

  await run('hung Hub fetch aborts and releases single-flight for a later refresh', async () => {
    let now = 1000;
    let firstSignal = null;
    let call = 0;
    const clock = createFakeTimers();
    const { app, nativeFetchRequests, window } = createHarness({
      now: () => now,
      hub: { url: '/', nodeId: 'alpha' },
      setTimeout: clock.setTimeout,
      clearTimeout: clock.clearTimeout,
      nativeFetch: async (_url, options) => {
        call += 1;
        if (call > 1) return nodesResponse([]);
        firstSignal = options.signal;
        return new Promise((resolve, reject) => {
          if (options.signal.aborted) {
            reject(new Error('aborted'));
            return;
          }
          options.signal.addEventListener('abort', () => reject(new Error('aborted')), { once: true });
        });
      },
    });
    await flushAsync();
    assertEqual(nativeFetchRequests.length, 1, 'startup begins the hung request');
    const timeout = clock.active(app.HUB_AGENT_LINKS_FETCH_TIMEOUT_MS)[0];
    assert(timeout, 'hung request has a bounded timeout');

    clock.fire(timeout);
    await flushAsync();
    assert(firstSignal.aborted, 'timeout aborts the native fetch signal');
    assert(timeout.cleared, 'fetch cleanup clears the timeout in finally');

    now += app.HUB_AGENT_LINKS_REFRESH_MS;
    await window.dispatchEvent({ type: 'focus' });
    await flushAsync();
    assertEqual(nativeFetchRequests.length, 2, 'later refresh starts after timed-out single-flight clears');
  });

  await run('sidebar search fetches server results', async () => {
    let requested = '';
    const { elements, state } = createHarness({
      setTimeout(fn) { fn(); return 1; },
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
