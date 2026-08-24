#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const projectPickerSource = fs.readFileSync(path.join(__dirname, 'app-project-picker.js'), 'utf8');
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
  append(...nodes) { nodes.forEach((node) => this.appendChild(node)); }
  replaceChildren(...nodes) {
    this.children.forEach((child) => { child.parentNode = null; });
    this.children = [];
    nodes.forEach((node) => { if (node) this.appendChild(node); });
  }
  remove() {
    if (!this.parentNode) return;
    const idx = this.parentNode.children.indexOf(this);
    if (idx !== -1) this.parentNode.children.splice(idx, 1);
    this.parentNode = null;
  }
  setAttribute(name, value) { this.attributes.set(name, String(value)); if (name === 'class') this.className = String(value); }
  removeAttribute(name) { this.attributes.delete(name); }
  getAttribute(name) { return this.attributes.get(name) || null; }
  contains(node) { if (node === this) return true; return this.children.some((child) => child.contains(node)); }
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
  select() { this.selected = true; }
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
    promptInput: new Element('textarea'),
    appShell: new Element('div'),
    sessionGroups: new Element('div'),
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
    sidebarSearchError: '',
    capabilitiesLoaded: false,
    capabilitiesRequired: false,
    worktreesEnabled: false,
    projectsEnabled: false,
    sidebarGroups: [],
    projects: [],
    projectsError: '',
    sessions: [],
    projectExpansion: {},
    projectDrafts: {},
    projectAttachments: {},
    activeProjectId: '',
    lastProjectId: '',
    selectedWorktreeDir: '',
    selectedWorktreeName: '',
    worktrees: [],
    draftSessionActive: false,
  };
  let renderSidebarCount = 0;
  let renderWorktreeCount = 0;
  const storageValues = new Map(Object.entries(options.initialStorage || {}));
  const localStorage = {
    getItem(key) { return storageValues.has(key) ? storageValues.get(key) : null; },
    setItem(key, value) { storageValues.set(key, String(value)); },
    removeItem(key) { storageValues.delete(key); },
  };
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
    STORAGE_KEYS: { draftMessages: 'drafts', projectExpansion: 'expansion', lastProject: 'lastProject', draftSessionActive: 'draftActive' },
    requestHeaders() { return {}; },
    relativeTime() { return 'now'; },
    truncate(value, limit) { return String(value || '').slice(0, limit); },
    clearDraftMessageForSession(sessionId) {
      const records = JSON.parse(localStorage.getItem('drafts') || '[]');
      localStorage.setItem('drafts', JSON.stringify(records.filter((record) => record.sessionId !== sessionId)));
    },
    sidebarSessionRow(session) {
      const row = document.createElement('div'); row.className = 'session-row'; row.dataset.sessionId = session.id;
      row.appendChild(Object.assign(document.createElement('button'), { textContent: session.title }));
      return row;
    },
    switchToSession() {},
    switchToDraftSession(options = {}) { state.activeProjectId = String(options.projectId || ''); app.restoredProjectDraft = options; },
    createAndSwitchToFreshSession() {},
    renderSidebar() { renderSidebarCount += 1; },
    renderWorktreeChip() { renderWorktreeCount += 1; },
  };
  const document = new Element('document');
  document.body = new Element('body');
  document.appendChild(document.body);
  document.activeElement = null;
  document.createElement = (tag) => {
    const element = new Element(tag);
    const originalFocus = element.focus.bind(element);
    element.focus = () => { originalFocus(); document.activeElement = element; };
    return element;
  };
  document.getElementById = (id) => {
    let match = null;
    const walk = (node) => {
      if (node.id === id) { match = node; return; }
      node.children.forEach((child) => { if (!match) walk(child); });
    };
    walk(document);
    return match;
  };
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
    localStorage,
    URL,
    URLSearchParams,
    Date: DateImpl,
    console,
    clearTimeout: options.clearTimeout || clearTimeout,
    setTimeout: options.setTimeout || setTimeout,
    fetch: trackedNativeFetch,
    Response,
    AbortController,
    ...(options.IntersectionObserver ? { IntersectionObserver: options.IntersectionObserver } : {}),
  };
  context.globalThis = context;
  app.apiFetch = (...args) => {
    apiFetchRequests.push(args);
    return apiFetchImpl(...args);
  };
  vm.runInNewContext(projectPickerSource, context, { filename: 'app-project-picker.js' });
  vm.runInNewContext(source, context, { filename: 'app-sidebar.js' });
  return {
    app,
    elements,
    state,
    document,
    window: windowObj,
    localStorage,
    nativeFetchRequests,
    apiFetchRequests,
    get renderSidebarCount() { return renderSidebarCount; },
    get renderWorktreeCount() { return renderWorktreeCount; },
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

    now += 60000;
    await window.dispatchEvent({ type: 'focus' });
    await flushAsync();
    const latchedWorker = elements.hubAgentLinks.querySelectorAll('.hub-agent-link')
      .find((link) => link.querySelector('.hub-agent-name').textContent === 'Worker');
    assert(latchedWorker.querySelector('.hub-agent-attention'), 'attention remains latched while the agent stays idle');

    await latchedWorker.dispatchEvent({ type: 'click' });
    assertEqual(latchedWorker.querySelector('.hub-agent-attention'), null, 'click clears the visible dot immediately');
    assertEqual(latchedWorker.querySelector('.visually-hidden'), null, 'click clears accessible attention immediately');
    now += 60000;
    await window.dispatchEvent({ type: 'focus' });
    await flushAsync();
    const visitedWorker = elements.hubAgentLinks.querySelectorAll('.hub-agent-link')
      .find((link) => link.querySelector('.hub-agent-name').textContent === 'Worker');
    assertEqual(visitedWorker.querySelector('.hub-agent-attention'), null, 'visiting the agent clears attention');
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

  await run('project mode loads one grouped sidebar projection and renders accessible groups', async () => {
    const requests = [];
    const harness = createHarness({
      apiFetch: async (url) => {
        requests.push(String(url));
        if (String(url).endsWith('/v1/capabilities')) {
          return new Response(JSON.stringify({ projects: { enabled: true }, worktrees: { enabled: true } }), { status: 200, headers: { 'Content-Type': 'application/json' } });
        }
        return new Response(JSON.stringify({ groups: [{
          project: { id: 'prj_alpha', name: 'Alpha', canonical_dir: '/srv/alpha', available: true, git: true },
          session_count: 1,
          sessions: [{ id: 's1', number: 1, summary: 'Fix bug', project_id: 'prj_alpha', project_name: 'Alpha', created_at: '2026-08-21T00:00:00Z' }],
        }, { project: { id: 'prj_empty', name: 'Empty', canonical_dir: '/srv/empty', available: true, git: false }, session_count: 0, sessions: [] }] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      },
    });
    const { app, elements, state } = harness;
    await app.initializeProjectMode();
    assert(state.projectsEnabled, 'project mode enabled from authenticated capabilities');
    assert(state.worktreesEnabled, 'authenticated worktree capability is retained');
    assert(harness.renderWorktreeCount >= 2, 'worktree control re-renders after capabilities and project status load');
    assertEqual(requests.filter((url) => url.includes('/v1/sidebar')).length, 1, 'initial project history uses one grouped request');
    app.renderProjectSidebar();
    const groups = elements.sessionGroups.querySelectorAll('.project-group');
    assertEqual(groups.length, 2, 'active and empty projects are both rendered');
    const toggles = elements.sessionGroups.querySelectorAll('.project-group-toggle');
    assertEqual(toggles[0].getAttribute('aria-expanded'), 'true', 'group disclosure exposes aria-expanded');
    assert(toggles[0].querySelector('.project-group-chevron').innerHTML.includes('<svg'), 'group disclosure uses an SVG chevron');
    assertEqual(groups[0].querySelector('.project-group-header').children[0], toggles[0], 'project name and disclosure share the left-side click target');
    assertEqual(toggles[0].querySelector('.project-group-label').textContent, 'Alpha', 'project name renders inside the disclosure control');
    assertEqual(elements.sessionGroups.querySelectorAll('.project-group-action').length, 2, 'each active project renders only its overflow menu control');
    assertEqual(state.sessions[0].projectId, 'prj_alpha', 'grouped summary preserves stable project identity');
  });

  await run('pinned conversations stay in a section above projects', () => {
    const { app, elements, state } = createHarness();
    state.capabilitiesLoaded = true;
    state.projectsEnabled = true;
    state.sidebarGroups = [{
      project: { id: 'prj_alpha', name: 'Alpha', available: true },
      sessions: [
        { id: 'recent', summary: 'Recent chat', project_id: 'prj_alpha', created_at: '2026-08-21T12:00:00Z' },
        { id: 'pinned', summary: 'Pinned chat', project_id: 'prj_alpha', pinned: true, created_at: '2026-08-20T12:00:00Z' },
      ],
    }];

    app.renderProjectSidebar();

    const pinnedSection = elements.sessionGroups.children[0];
    assert(pinnedSection.classList.contains('pinned-sessions'), 'pinned section renders first');
    assertEqual(pinnedSection.querySelector('h3').textContent, 'Pinned', 'pinned section is labelled');
    assertEqual(pinnedSection.querySelector('.session-row').dataset.sessionId, 'pinned', 'pinned conversation renders in the top section');
    const project = elements.sessionGroups.querySelector('.project-group');
    assertEqual(project.querySelectorAll('.session-row').length, 1, 'pinned conversation is not duplicated in its project');
    assertEqual(project.querySelector('.session-row').dataset.sessionId, 'recent', 'regular conversation stays in its project');
  });

  await run('reload restores the remembered valid project draft context', async () => {
    const { app, state } = createHarness({
      apiFetch: async (url) => String(url).endsWith('/v1/capabilities')
        ? new Response(JSON.stringify({ projects: { enabled: true }, worktrees: { enabled: true } }), { status: 200, headers: { 'Content-Type': 'application/json' } })
        : new Response(JSON.stringify({ groups: [{ project: { id: 'prj_remembered', name: 'Remembered', available: true, git: true }, sessions: [] }] }), { status: 200, headers: { 'Content-Type': 'application/json' } }),
    });
    state.draftSessionActive = true;
    state.lastProjectId = 'prj_remembered';
    state.projectDrafts = { prj_remembered: { prompt: 'survives reload', created: 100 } };
    await app.initializeProjectMode();
    assertEqual(state.activeProjectId, 'prj_remembered', 'remembered valid project becomes active draft context');
    assertEqual(app.restoredProjectDraft.projectId, 'prj_remembered', 'project draft restoration uses explicit project identity');
  });

  await run('capability failure renders retry and never assumes legacy project binding', async () => {
    const { app, elements, state } = createHarness({
      apiFetch: async () => new Response('unavailable', { status: 503 }),
    });
    const enabled = await app.initializeProjectMode();
    assertEqual(enabled, false, 'failed capabilities do not enable projects');
    assert(state.capabilitiesRequired && !state.capabilitiesLoaded, 'capabilities remain unresolved');
    app.renderProjectSidebar();
    assertEqual(elements.sessionGroups.querySelectorAll('.project-inline-error').length, 1, 'inline capability error is rendered');
    assertEqual(elements.sessionGroups.querySelectorAll('button').length, 1, 'capability error has a Retry action');
  });

  await run('project drafts stay off the sidebar and do not reorder project groups', () => {
    const { app, elements, state } = createHarness();
    state.capabilitiesLoaded = true;
    state.projectsEnabled = true;
    state.sidebarGroups = [
      { project: { id: 'prj_recent_server', name: 'Recent server', available: true }, last_activity_at: '2026-08-21T12:00:00Z', sessions: [] },
      { project: { id: 'prj_recent_draft', name: 'Recent draft', available: true }, last_activity_at: '2026-08-20T12:00:00Z', sessions: [] },
    ];
    state.projectDrafts = {
      prj_recent_server: { prompt: 'server group draft', created: Date.parse('2026-08-21T11:00:00Z') },
      prj_recent_draft: { prompt: 'newer browser draft', created: Date.parse('2026-08-21T13:00:00Z') },
    };
    app.renderProjectSidebar();
    const groups = elements.sessionGroups.querySelectorAll('.project-group');
    assertEqual(groups[0].dataset.projectId, 'prj_recent_server', 'hidden composer drafts do not move project groups');
    assertEqual(elements.sessionGroups.querySelectorAll('.session-row').length, 0, 'composer drafts do not render sidebar rows');
  });

  await run('No project conversations paginate automatically without a Load more control', async () => {
    const urls = [];
    class ImmediateIntersectionObserver {
      constructor(callback) { this.callback = callback; }
      observe(target) { this.callback([{ isIntersecting: true, target }]); }
      unobserve() {}
      disconnect() {}
    }
    const { app, elements, state } = createHarness({
      IntersectionObserver: ImmediateIntersectionObserver,
      apiFetch: async (url) => {
        urls.push(String(url));
        return new Response(JSON.stringify({ sessions: [{ id: 'legacy-2', number: 2, summary: 'Older legacy', created_at: Date.now() }], next_cursor: '' }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      },
    });
    state.capabilitiesLoaded = true;
    state.projectsEnabled = true;
    state.sidebarGroups = [{ no_project: true, next_cursor: 'opaque-null-cursor', sessions: [] }];
    app.renderProjectSidebar();
    assert(!elements.sessionGroups.querySelector('.project-load-more'), 'manual Load more control is absent');
    assert(elements.sessionGroups.querySelector('.project-pagination-sentinel'), 'automatic pagination sentinel is rendered');
    await flushAsync();
    assert(urls.some((url) => url.includes('cursor=opaque-null-cursor') && !url.includes('project_id=')), 'No project cursor was not fetched automatically');
  });

  await run('no-project capability clears stale project drafts without a notification', async () => {
    const notifications = [];
    const { app, elements, state, localStorage } = createHarness({
      initialStorage: {
        drafts: JSON.stringify([
          { id: 'draft:prj_old', sessionId: 'draft:prj_old', prompt: 'stale project prompt' },
          { id: 'ordinary', sessionId: 'sess-1', prompt: 'ordinary retry' },
        ]),
        lastProject: 'prj_old',
        draftActive: '1',
      },
      apiFetch: async () => new Response(JSON.stringify({ projects: { enabled: false }, worktrees: { enabled: false } }), { status: 200, headers: { 'Content-Type': 'application/json' } }),
    });
    app.showToast = (message) => notifications.push(message);
    state.projectsEnabled = true;
    state.lastProjectId = 'prj_old';
    state.projectDrafts = { prj_old: { prompt: 'stale project prompt', worktreeDir: '/managed/old', worktreeName: 'old' } };
    state.selectedWorktreeDir = '/managed/old';
    state.selectedWorktreeName = 'old';
    state.worktrees = [{ dir: '/managed/old' }];
    state.draftSessionActive = true;
    await app.initializeProjectMode();
    const drafts = JSON.parse(localStorage.getItem('drafts') || '[]');
    assertEqual(drafts.length, 1, 'project drafts are removed while ordinary retry drafts remain');
    assertEqual(drafts[0].id, 'ordinary', 'ordinary session draft is preserved');
    assertEqual(localStorage.getItem('lastProject'), null, 'stale last-project context is cleared');
    assert(!state.draftSessionActive, 'stale active project draft is closed');
    assertEqual(state.selectedWorktreeDir, '', 'stale selected worktree directory is cleared');
    assertEqual(state.selectedWorktreeName, '', 'stale selected worktree name is cleared');
    assertEqual(state.worktrees.length, 0, 'stale project worktree list is cleared');
    assert(state.capabilitiesLoaded && !state.projectsEnabled, 'no-project mode is a loaded, supported capability state');
    assertEqual(notifications.length, 0, 'entering no-project mode does not emit a toast');
    assertEqual(app.renderProjectSidebar(), false, 'no-project mode delegates to the flat/date sidebar renderer');
    assertEqual(elements.sessionGroups.querySelectorAll('.project-group').length, 0, 'no-project mode leaves no project group rows');
  });

  await run('add project path disables text transformations and traps background focus', async () => {
    const { app, elements, document } = createHarness();
    app.openProjectModal();
    const pathInput = document.querySelector('.project-path-input');
    assert(pathInput, 'project path input rendered');
    assertEqual(pathInput.getAttribute('autocorrect'), 'off', 'autocorrect disabled');
    assertEqual(pathInput.autocapitalize, 'none', 'autocapitalization disabled');
    assertEqual(pathInput.spellcheck, false, 'spellcheck disabled');
    assert(elements.appShell.inert === true, 'background app shell is inert while modal is open');
    const cancel = document.querySelector('.project-modal-actions').querySelector('button');
    await cancel.dispatchEvent({ type: 'click' });
    assert(elements.appShell.inert === false, 'background inert state clears when modal closes');
  });

  await run('add project browser navigates server folders and fills the path', async () => {
    const urls = [];
    const { app, document } = createHarness({
      apiFetch: async (url) => {
        urls.push(String(url));
        const nested = String(url).includes(encodeURIComponent('/home/sam/Source')) || String(url).includes('path=%2Fhome%2Fsam%2FSource');
        return new Response(JSON.stringify(nested ? {
          path: '/home/sam/Source', parent: '/home/sam', home: '/home/sam',
          breadcrumbs: [{ label: '/', path: '/' }, { label: 'home', path: '/home' }, { label: 'sam', path: '/home/sam' }, { label: 'Source', path: '/home/sam/Source' }],
          entries: [{ name: 'term-llm', path: '/home/sam/Source/term-llm', git: true }],
        } : {
          path: '/home/sam', parent: '/home', home: '/home/sam',
          breadcrumbs: [{ label: '/', path: '/' }, { label: 'home', path: '/home' }, { label: 'sam', path: '/home/sam' }],
          entries: [{ name: 'Source', path: '/home/sam/Source' }],
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      },
    });
    app.openProjectModal();
    const browse = document.querySelector('.project-browse-button');
    await browse.dispatchEvent({ type: 'click', target: browse });
    await flushAsync();
    assert(urls[0].includes('/v1/project-directories'), 'browser loads from the authenticated server directory endpoint');
    assertEqual(document.querySelector('.project-path-input').value, '/home/sam', 'default browser folder fills the path');
    const source = document.querySelector('.project-browser-row');
    assertEqual(source.textContent, '', 'folder row uses structured child content');
    await source.dispatchEvent({ type: 'click', target: source });
    await flushAsync();
    assertEqual(document.querySelector('.project-path-input').value, '/home/sam/Source', 'navigating updates the selected server folder');
    assert(document.querySelector('.project-browser-badge'), 'Git directory metadata renders as a badge');
    assertEqual(browse.getAttribute('aria-expanded'), 'true', 'inline browser exposes its expanded state');
  });

  await run('archived duplicate preview restores stable project instead of starting a forbidden draft', async () => {
    const urls = [];
    let calls = 0;
    const { app, document } = createHarness({
      apiFetch: async (url) => {
        urls.push(String(url)); calls += 1;
        if (calls === 1) {
          return new Response(JSON.stringify({ duplicate: true, existing_project_id: 'prj_archived', canonical_dir: '/srv/archived', default_name: 'archived', project: { id: 'prj_archived', name: 'Archived', archived_at: '2026-08-20T00:00:00Z' } }), { status: 200, headers: { 'Content-Type': 'application/json' } });
        }
        if (String(url).includes('/v1/projects') && !String(url).includes('dry_run')) {
          return new Response(JSON.stringify({ restored: true, project: { id: 'prj_archived', name: 'Archived', available: true } }), { status: 201, headers: { 'Content-Type': 'application/json' } });
        }
        return new Response(JSON.stringify({ groups: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      },
    });
    app.openProjectModal();
    document.querySelector('.project-path-input').value = '/srv/archived';
    const actions = document.querySelector('.project-modal-actions');
    const submit = actions.children[1];
    await submit.dispatchEvent({ type: 'click' });
    assertEqual(submit.textContent, 'Restore project', 'archived duplicate requires explicit restore confirmation');
    assert(document.getElementById('projectModal'), 'archived duplicate keeps confirmation modal open');
    await submit.dispatchEvent({ type: 'click' });
    assert(urls.some((url) => url.endsWith('/v1/projects?dry_run=1')), 'dry-run preview was requested');
    assert(urls.some((url) => url.endsWith('/v1/projects')), 'confirmed archived duplicate was posted for restoration');
  });

  await run('project menu exposes keyboard semantics and restores focus on Escape', async () => {
    const { app, elements, state, document } = createHarness();
    state.capabilitiesLoaded = true;
    state.projectsEnabled = true;
    state.sidebarGroups = [{ project: { id: 'prj_menu', name: 'Menu project', canonical_dir: '/srv/menu', available: true, git: true }, sessions: [] }];
    app.renderProjectSidebar();
    const actions = elements.sessionGroups.querySelectorAll('.project-group-action');
    const menuTrigger = actions[actions.length - 1];
    assertEqual(menuTrigger.getAttribute('aria-haspopup'), 'menu', 'project menu trigger declares popup semantics');
    assertEqual(menuTrigger.getAttribute('aria-expanded'), 'false', 'project menu starts collapsed');
    await menuTrigger.dispatchEvent({ type: 'click', target: menuTrigger });
    assertEqual(menuTrigger.getAttribute('aria-expanded'), 'true', 'opening menu updates aria-expanded');
    const menu = menuTrigger.parentNode.children.find((child) => child.classList.contains('project-context-menu'));
    assert(menu && menu.getAttribute('role') === 'menu', 'project context menu renders with menu role');
    assertEqual(menu.getAttribute('aria-label'), 'Manage Menu project', 'menu has an accessible project label');
    const newChat = menu.children.find((child) => child.textContent === 'New chat');
    assert(newChat && newChat.getAttribute('role') === 'menuitem', 'new chat is available from the project overflow menu');
    assertEqual(document.activeElement, newChat, 'new chat is the initially focused menu action');
    await document.dispatchEvent({ type: 'keydown', key: 'Escape', preventDefault() {} });
    assert(!menuTrigger.parentNode.children.includes(menu), 'Escape dismisses project menu');
    assertEqual(menuTrigger.getAttribute('aria-expanded'), 'false', 'dismissal resets aria-expanded');
    assertEqual(document.activeElement, menuTrigger, 'Escape restores trigger focus');
  });

  await run('project overflow menu starts a new chat in that project', async () => {
    const { app, elements, state } = createHarness();
    let selectedProject = '';
    app.createAndSwitchToFreshSession = (projectID) => { selectedProject = projectID; };
    state.capabilitiesLoaded = true;
    state.projectsEnabled = true;
    state.sidebarGroups = [{ project: { id: 'prj_new_chat', name: 'Chat project', canonical_dir: '/srv/chat', available: true, git: false }, sessions: [] }];
    app.renderProjectSidebar();
    const actions = elements.sessionGroups.querySelectorAll('.project-group-action');
    assertEqual(actions.length, 1, 'active project header has no separate new-chat button');
    const menuTrigger = actions[0];
    await menuTrigger.dispatchEvent({ type: 'click', target: menuTrigger });
    const menu = menuTrigger.parentNode.children.find((child) => child.classList.contains('project-context-menu'));
    const newChat = menu.children.find((child) => child.textContent === 'New chat');
    await newChat.dispatchEvent({ type: 'click', target: newChat });
    assertEqual(selectedProject, 'prj_new_chat', 'new chat action targets the owning project');
    assert(!menuTrigger.parentNode.children.includes(menu), 'new chat closes the overflow menu');
  });

  await run('archiving requires the documented explicit confirmation', async () => {
    const requests = [];
    const { app, elements, state, document } = createHarness({
      apiFetch: async (url, options = {}) => {
        requests.push({ url: String(url), method: options.method || 'GET', body: options.body || '' });
        if (String(url).includes('/v1/sidebar')) return new Response(JSON.stringify({ groups: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
        return new Response(JSON.stringify({ project: { id: 'prj_archive' } }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      },
    });
    state.capabilitiesLoaded = true;
    state.projectsEnabled = true;
    state.sidebarGroups = [{ project: { id: 'prj_archive', name: 'Archive me', canonical_dir: '/srv/archive', available: true, git: false }, sessions: [] }];
    app.renderProjectSidebar();
    const actions = elements.sessionGroups.querySelectorAll('.project-group-action');
    const menuTrigger = actions[actions.length - 1];
    await menuTrigger.dispatchEvent({ type: 'click', target: menuTrigger });
    const contextMenu = menuTrigger.parentNode.children.find((child) => child.classList.contains('project-context-menu'));
    const archiveMenuItem = contextMenu.children.find((child) => child.textContent === 'Archive');
    await archiveMenuItem.dispatchEvent({ type: 'click', target: archiveMenuItem });
    const archiveButton = document.querySelector('.project-manage-archive');
    assert(document.querySelector('.project-manage-identity'), 'manage dialog renders project identity as a structured panel');
    assert(document.querySelector('.project-modal-close'), 'manage dialog has a visible close affordance');
    assertEqual(document.querySelector('.project-modal-actions').children.length, 2, 'destructive action is separated from footer actions');
    await archiveButton.dispatchEvent({ type: 'click', target: archiveButton });
    assertEqual(requests.filter((request) => request.method === 'PATCH').length, 0, 'first archive action does not mutate');
    assertEqual(archiveButton.textContent, 'Confirm archive', 'archive action becomes an explicit confirmation');
    assert(archiveButton.classList.contains('danger'), 'confirmed archive receives explicit danger styling');
    assert(!document.querySelector('.project-manage-warning').hidden, 'confirmation warning appears beside the archive action');
    await archiveButton.dispatchEvent({ type: 'click', target: archiveButton });
    assertEqual(requests.filter((request) => request.method === 'PATCH').length, 1, 'confirmed archive mutates exactly once');
  });

  await run('transient sidebar failure preserves the last valid project groups beneath Retry', async () => {
    const { app, elements, state } = createHarness({ apiFetch: async () => { throw new Error('offline'); } });
    state.capabilitiesLoaded = true;
    state.projectsEnabled = true;
    state.sidebarGroups = [{ project: { id: 'prj_cached', name: 'Cached', canonical_dir: '/srv/cached', available: true, git: false }, sessions: [{ id: 'cached-session', summary: 'Still usable', project_id: 'prj_cached' }] }];
    state.projects = state.sidebarGroups.map((group) => group.project);
    await app.loadProjectSidebar();
    app.renderProjectSidebar();
    assertEqual(elements.sessionGroups.querySelectorAll('.project-inline-error').length, 1, 'transient failure renders inline Retry');
    assertEqual(elements.sessionGroups.querySelectorAll('.project-group').length, 1, 'cached project group remains rendered');
    assertEqual(elements.sessionGroups.querySelectorAll('.session-row').length, 1, 'cached session remains usable');
  });

  await run('search failure stays distinct from zero results and offers Retry', async () => {
    const { app, elements, state } = createHarness({
      setTimeout(fn) { fn(); return 1; },
      apiFetch: async () => { throw new Error('offline'); },
    });
    state.capabilitiesLoaded = true;
    state.projectsEnabled = true;
    state.sidebarGroups = [{ project: { id: 'prj_search', name: 'Search', available: true }, sessions: [{ id: 'known', summary: 'Known row', project_id: 'prj_search' }] }];
    elements.sidebarSearchInput.value = 'needle';
    await elements.sidebarSearchInput.dispatchEvent({ type: 'input' });
    await flushAsync();
    app.renderProjectSidebar();
    assertEqual(state.sidebarSearchError, 'Could not search conversations', 'search transport failure has dedicated state');
    assertEqual(elements.sessionGroups.querySelectorAll('.project-inline-error').length, 1, 'search failure renders inline Retry');
    assertEqual(elements.sessionGroups.querySelectorAll('.sidebar-empty').length, 0, 'search failure is not mislabeled as no matches');
    assertEqual(elements.sessionGroups.querySelectorAll('.project-group').length, 1, 'known grouped rows remain visible during search failure');
  });

  await run('active No project conversation respects an explicit collapsed state', () => {
    const { app, elements, state } = createHarness();
    state.capabilitiesLoaded = true;
    state.projectsEnabled = true;
    state.activeSessionId = 'legacy-active';
    state.sessions = [{ id: 'legacy-active', projectId: '', title: 'Legacy active' }];
    state.projectExpansion.__no_project__ = false;
    state.sidebarGroups = [{ no_project: true, sessions: [{ id: 'legacy-active', summary: 'Legacy active', project_id: '' }] }];
    app.renderProjectSidebar();
    const group = elements.sessionGroups.querySelector('.project-group');
    assert(group.classList.contains('active'), 'No project group is marked active');
    assertEqual(group.querySelector('.project-group-toggle').getAttribute('aria-expanded'), 'false', 'active No project group remains user-collapsed');
    assert(!group.querySelector('.project-session-list'), 'collapsed active project does not render its conversation list');
  });

  await run('sidebar refresh never scrolls the active project into view', () => {
    const { app, elements, state } = createHarness();
    state.capabilitiesLoaded = true;
    state.projectsEnabled = true;
    state.activeSessionId = 'active-session';
    state.sessions = [{ id: 'active-session', projectId: 'prj_active', title: 'Active' }];
    state.sidebarGroups = [{ project: { id: 'prj_active', name: 'Active', available: true }, sessions: [{ id: 'active-session', summary: 'Active', project_id: 'prj_active' }] }];
    let scrollCalls = 0;
    const previousScrollIntoView = Element.prototype.scrollIntoView;
    Element.prototype.scrollIntoView = () => { scrollCalls += 1; };
    try {
      app.renderProjectSidebar();
      app.renderProjectSidebar();
    } finally {
      if (previousScrollIntoView) Element.prototype.scrollIntoView = previousScrollIntoView;
      else delete Element.prototype.scrollIntoView;
    }
    assertEqual(scrollCalls, 0, 'project refresh did not move the sidebar viewport');
  });

  await run('project disclosure toggles without a notification', async () => {
    const { app, elements, state } = createHarness();
    const notifications = [];
    app.showToast = (message) => notifications.push(message);
    state.capabilitiesLoaded = true;
    state.projectsEnabled = true;
    state.sidebarGroups = [{ project: { id: 'prj_quiet', name: 'Quiet project', available: true }, sessions: [] }];
    app.renderProjectSidebar();
    const toggle = elements.sessionGroups.querySelector('.project-group-toggle');
    await toggle.dispatchEvent({ type: 'click', target: toggle, preventDefault() {} });
    assertEqual(state.projectExpansion.prj_quiet, false, 'project group collapsed');
    app.renderProjectSidebar();
    const collapsedToggle = elements.sessionGroups.querySelector('.project-group-toggle');
    await collapsedToggle.dispatchEvent({ type: 'click', target: collapsedToggle, preventDefault() {} });
    assertEqual(state.projectExpansion.prj_quiet, true, 'project group expands again after rerender');
    app.renderProjectSidebar();
    assert(elements.sessionGroups.querySelector('.project-session-list').classList.contains('is-opening'), 'user expansion receives one restrained reveal');
    app.renderProjectSidebar();
    assert(!elements.sessionGroups.querySelector('.project-session-list').classList.contains('is-opening'), 'background rerender does not replay expansion animation');
    assertEqual(notifications.length, 0, 'project disclosure did not emit a toast');
  });

  await run('duplicate project names render shortened path disambiguators', () => {
    const { app, elements, state } = createHarness();
    state.capabilitiesLoaded = true;
    state.projectsEnabled = true;
    state.projects = [
      { id: 'prj_one', name: 'App', canonical_dir: '/srv/one/app', available: true },
      { id: 'prj_two', name: 'App', canonical_dir: '/srv/two/app', available: true },
    ];
    state.sidebarGroups = state.projects.map((project) => ({ project, sessions: [] }));
    app.renderProjectSidebar();
    const labels = elements.sessionGroups.querySelectorAll('.project-group-label').map((label) => label.textContent);
    assert(labels.some((label) => label.includes('one/app')) && labels.some((label) => label.includes('two/app')), 'duplicate names include distinct shortened paths');
  });

  await run('editing project details after preview requires a fresh dry run', async () => {
    const urls = [];
    const { app, document } = createHarness({
      apiFetch: async (url) => {
        urls.push(String(url));
        return new Response(JSON.stringify({ canonical_dir: '/srv/previewed', default_name: 'previewed', git: false }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      },
    });
    app.openProjectModal();
    const path = document.querySelector('.project-path-input');
    path.value = '/srv/first';
    const submit = document.querySelector('.project-modal-actions').children[1];
    await submit.dispatchEvent({ type: 'click' });
    path.value = '/srv/second';
    await path.dispatchEvent({ type: 'input', target: path });
    assertEqual(submit.textContent, 'Preview', 'editing invalidates prior confirmation');
    await submit.dispatchEvent({ type: 'click' });
    assertEqual(urls.filter((url) => url.endsWith('?dry_run=1')).length, 2, 'edited path is previewed again');
    assertEqual(urls.filter((url) => url.endsWith('/v1/projects')).length, 0, 'changed path was not created from stale preview');
  });

  await run('No project assignment lives off-row and transport failure remains retryable inline', async () => {
    const { app, elements, state, document } = createHarness({ apiFetch: async () => { throw new Error('offline'); } });
    state.capabilitiesLoaded = true;
    state.projectsEnabled = true;
    state.projects = [{ id: 'prj_assign', name: 'Assign target', canonical_dir: '/srv/assign', available: true, git: true }];
    const conversation = { id: 'legacy-assign', title: 'Assign me', projectId: '' };
    state.sidebarGroups = [{ no_project: true, sessions: [{ id: conversation.id, summary: conversation.title, project_id: '' }] }];
    app.renderProjectSidebar();
    assert(!elements.sessionGroups.querySelector('.assign-project-action'), 'assignment action is not rendered beside the conversation');
    app.openAssignProjectModal(conversation);
    const choiceList = document.querySelector('.project-choice-list');
    const choice = choiceList.querySelector('.project-choice');
    const dialog = document.getElementById('assignProjectModal').querySelector('.project-assign-modal');
    assertEqual(choiceList.getAttribute('role'), 'radiogroup', 'project choices expose single-select semantics');
    assertEqual(dialog.getAttribute('aria-describedby'), 'assignProjectNote', 'grouping-only warning describes the dialog');
    await choice.dispatchEvent({ type: 'click', target: choice });
    assertEqual(choice.getAttribute('aria-checked'), 'true', 'project choice is visibly selected before assignment');
    const assign = document.querySelector('.project-modal-actions').children[1];
    assert(!assign.disabled, 'selecting a project enables the explicit assignment action');
    await assign.dispatchEvent({ type: 'click', target: assign });
    assert(!assign.disabled, 'failed assignment action is re-enabled');
    const assignStatus = document.querySelector('.project-modal-status');
    assert(assignStatus.textContent.includes('Retry'), 'failed assignment exposes inline Retry copy');
    assertEqual(assignStatus.getAttribute('role'), 'alert', 'assignment failure is announced as an error');
  });

  await run('assignment upgrades the conversation workspace into a new project', async () => {
    const requests = [];
    const conversation = { id: 'legacy-upgrade', title: 'Upgrade me', projectId: '' };
    const { app, state, document } = createHarness({
      apiFetch: async (url, options = {}) => {
        requests.push({ url: String(url), method: options.method || 'GET', body: options.body || '' });
        if (String(url).includes('/v1/sidebar')) return new Response(JSON.stringify({ groups: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
        if ((options.method || 'GET') === 'POST') return new Response(JSON.stringify({ project_id: 'prj_upgraded', project_name: 'workspace' }), { status: 200, headers: { 'Content-Type': 'application/json' } });
        return new Response(JSON.stringify({ candidate: { canonical_dir: '/home/sam/workspace', default_name: 'workspace', git: true } }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      },
    });
    state.projects = [];
    app.openAssignProjectModal(conversation);
    await flushAsync();
    const upgrade = document.querySelector('.project-assign-upgrade');
    assert(!upgrade.hidden, 'candidate workspace renders as an inline project upgrade');
    assertEqual(upgrade.querySelector('.project-manage-path').textContent, '/home/sam/workspace', 'candidate path is visible before creation');
    const controls = document.querySelector('.project-assign-upgrade-controls');
    const name = controls.children[0]; const create = controls.children[1];
    assertEqual(name.value, 'workspace', 'candidate folder name pre-fills the project name');
    assertEqual(create.textContent, 'Create & assign', 'candidate exposes one-step project creation');
    await create.dispatchEvent({ type: 'click', target: create });
    await flushAsync();
    const createRequest = requests.find((request) => request.method === 'POST');
    assert(createRequest && JSON.parse(createRequest.body).create_from_workspace === true, 'upgrade posts the create-from-workspace assignment');
    assertEqual(conversation.projectId, 'prj_upgraded', 'created project is assigned to the conversation');
  });

  await run('grouped rows prefer canonical status-updated session data over stale sidebar summaries', () => {
    const { app, elements, state } = createHarness();
    state.capabilitiesLoaded = true;
    state.projectsEnabled = true;
    state.sessions = [{ id: 'status-current', projectId: 'prj_status', projectName: 'Status', title: 'Current title', lastMessageAt: 5000, created: 1000, pinned: false }];
    state.sidebarGroups = [{ project: { id: 'prj_status', name: 'Status', available: true }, sessions: [{ id: 'status-current', summary: 'Stale title', project_id: 'prj_status', created_at: 1000, last_message_at: 1000 }] }];
    app.renderProjectSidebar();
    const row = elements.sessionGroups.querySelector('.session-row');
    assertEqual(row.children[0].textContent, 'Current title', 'status-updated title wins over stale grouped summary');
  });

  await run('unavailable projects badge both group and affected conversation rows', () => {
    const { app, elements, state } = createHarness();
    state.capabilitiesLoaded = true;
    state.projectsEnabled = true;
    state.sidebarGroups = [{ project: { id: 'prj_down', name: 'Down', canonical_dir: '/srv/down', available: false, unavailable_reason: 'Directory identity changed' }, sessions: [{ id: 'down-session', summary: 'Blocked row', project_id: 'prj_down' }] }];
    app.renderProjectSidebar();
    assert(elements.sessionGroups.querySelector('.project-group').classList.contains('unavailable'), 'unavailable project group is badged');
    const row = elements.sessionGroups.querySelector('.session-row');
    assert(row.classList.contains('project-unavailable-row'), 'affected conversation row is badged');
    assertEqual(row.querySelectorAll('.project-session-unavailable').length, 1, 'row exposes accessible unavailable status');
    assert(elements.sessionGroups.querySelector('.project-status-detail').textContent.includes('/srv/down'), 'group status keeps canonical path visible');
  });

  await run('project management transport failures remain visible and retryable', async () => {
    const { app, elements, state, document } = createHarness({ apiFetch: async () => { throw new Error('offline'); } });
    state.capabilitiesLoaded = true;
    state.projectsEnabled = true;
    state.sidebarGroups = [{ project: { id: 'prj_manage_fail', name: 'Manage fail', canonical_dir: '/srv/fail', available: true, git: false }, sessions: [] }];
    app.renderProjectSidebar();
    const actions = elements.sessionGroups.querySelectorAll('.project-group-action');
    const menuTrigger = actions[actions.length - 1];
    await menuTrigger.dispatchEvent({ type: 'click', target: menuTrigger });
    const contextMenu = menuTrigger.parentNode.children.find((child) => child.classList.contains('project-context-menu'));
    const rename = contextMenu.children.find((child) => child.textContent === 'Rename');
    await rename.dispatchEvent({ type: 'click', target: rename });
    const modalActions = document.querySelector('.project-modal-actions');
    const save = modalActions.children[1];
    const nameInput = document.getElementById('projectManageNameInput');
    await nameInput.dispatchEvent({ type: 'keydown', key: 'Enter', target: nameInput, preventDefault() {} });
    assert(!save.disabled, 'failed rename action is retryable');
    const errorStatus = document.querySelector('.project-modal-status');
    assert(errorStatus.textContent.includes('offline'), 'rename transport failure remains in live status');
    assertEqual(errorStatus.getAttribute('role'), 'alert', 'rename failure is announced as an error');
  });

  await run('project search results retain project identity for regrouping', async () => {
    const { elements, state } = createHarness({
      setTimeout(fn) { fn(); return 1; },
      apiFetch: async () => new Response(JSON.stringify({ sessions: [{ id: 's1', short_title: 'Needle', project_id: 'prj_docs', project_name: 'Docs' }] }), { status: 200, headers: { 'Content-Type': 'application/json' } }),
    });
    elements.sidebarSearchInput.value = 'needle';
    await elements.sidebarSearchInput.dispatchEvent({ type: 'input' });
    await new Promise((resolve) => setImmediate(resolve));
    assertEqual(state.sidebarSearchResults[0].projectId, 'prj_docs', 'search result keeps project ID');
    assertEqual(state.sidebarSearchResults[0].projectName, 'Docs', 'search result keeps project name');
    assertEqual(state.sessions.filter((session) => session.id === 's1').length, 1, 'search-only result joins the authoritative session collection for row actions');
  });

  if (failures > 0) process.exit(1);
})();
