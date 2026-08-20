#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const source = fs.readFileSync(path.join(__dirname, 'app-diffs.js'), 'utf8');
const commentSource = fs.readFileSync(path.join(__dirname, 'app-diff-comments.js'), 'utf8');
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

class StyleDecl {
  constructor() { this.values = new Map(); }
  setProperty(name, value) { this.values.set(String(name), String(value)); }
  removeProperty(name) { const value = this.values.get(String(name)) || ''; this.values.delete(String(name)); return value; }
  getPropertyValue(name) { return this.values.get(String(name)) || ''; }
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
    this.style = new StyleDecl();
    this.listeners = new Map();
    this.textContent = '';
    this.value = '';
    this.title = '';
    this.hidden = false;
    this.scrollTop = 0;
    this._connectedOverride = null;
    this.focusCount = 0;
    this.rect = { width: 320, height: 40, top: 0, left: 0, right: 320, bottom: 40 };
  }
  get isConnected() {
    if (this._connectedOverride === false) return false;
    let root = this;
    while (root.parentNode) root = root.parentNode;
    return root.tagName === 'DOCUMENT';
  }
  set isConnected(value) { this._connectedOverride = value === false ? false : null; }
  append(...nodes) { nodes.forEach((node) => this.appendChild(node)); }
  appendChild(child) {
    if (child.parentNode) {
      const idx = child.parentNode.children.indexOf(child);
      if (idx !== -1) child.parentNode.children.splice(idx, 1);
    }
    child.parentNode = this;
    this.children.push(child);
    return child;
  }
  insertBefore(child, reference) {
    if (child.parentNode) {
      const idx = child.parentNode.children.indexOf(child);
      if (idx !== -1) child.parentNode.children.splice(idx, 1);
    }
    const index = reference ? this.children.indexOf(reference) : -1;
    child.parentNode = this;
    if (index === -1) this.children.push(child); else this.children.splice(index, 0, child);
    return child;
  }
  remove() {
    if (!this.parentNode) return;
    const document = this.ownerDocument;
    if (document?.activeElement && this.contains(document.activeElement)) document.activeElement = document.body;
    const index = this.parentNode.children.indexOf(this);
    if (index !== -1) this.parentNode.children.splice(index, 1);
    this.parentNode = null;
  }
  replaceChildren(...nodes) {
    const document = this.ownerDocument;
    if (document?.activeElement && this.children.some((child) => child.contains(document.activeElement))) {
      document.activeElement = document.body;
    }
    this.children.forEach((child) => { child.parentNode = null; });
    this.children = [];
    nodes.forEach((node) => { if (node) this.appendChild(node); });
  }
  setAttribute(name, value) { this.attributes.set(name, String(value)); if (name === 'class') this.className = String(value); if (name === 'hidden') this.hidden = true; }
  removeAttribute(name) { this.attributes.delete(name); if (name === 'hidden') this.hidden = false; }
  getAttribute(name) { return this.attributes.has(name) ? this.attributes.get(name) : null; }
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
  matches(selector) {
    if (selector.startsWith('#')) return this.id === selector.slice(1);
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
  closest(selector) {
    let current = this;
    while (current) {
      if (current.matches?.(selector)) return current;
      current = current.parentNode;
    }
    return null;
  }
  contains(node) {
    if (node === this) return true;
    return this.children.some((child) => child.contains(node));
  }
  get nextSibling() {
    if (!this.parentNode) return null;
    const index = this.parentNode.children.indexOf(this);
    return this.parentNode.children[index + 1] || null;
  }
  focus() {
    this.focusCount += 1;
    if (this.ownerDocument && this.isConnected) this.ownerDocument.activeElement = this;
  }
  scrollIntoView(options) { this.lastScrollIntoView = options || {}; }
  getBoundingClientRect() { return this.rect; }
  setPointerCapture() {}
  releasePointerCapture() {}
}

function createHarness(options = {}) {
  const elements = {
    appShell: new Element('div'),
    appMain: new Element('main'),
    sidebar: new Element('aside'),
    diffSidebar: new Element('aside'),
    diffSidebarTotals: new Element('span'),
    diffSidebarCloseBtn: new Element('button'),
    diffResizeHandle: new Element('div'),
    diffFileList: new Element('div'),
    diffToggleBtn: new Element('button'),
    diffToggleBadge: new Element('span'),
    diffBulkToggleBtn: new Element('button'),
    diffMaximizeBtn: new Element('button'),
    diffQueueBar: new Element('div'),
    diffFilterRow: new Element('div'),
    diffFilterInput: new Element('input')
  };
  elements.diffSidebar.hidden = true;
  elements.diffToggleBtn.hidden = true;
  elements.diffFilterRow.hidden = true;

  const state = {
    token: '',
    activeSessionId: 's1',
    connected: true,
    sessions: [{ id: 's1', transcriptRev: 1 }],
    pendingInterjections: [],
    pendingInterruptCommits: [],
    queuedInterrupts: [],
    branchContextQueuedSend: null
  };

  const storage = new Map();
  const localStorage = {
    getItem: (key) => (storage.has(key) ? storage.get(key) : null),
    setItem: (key, value) => storage.set(key, String(value)),
    removeItem: (key) => storage.delete(key)
  };

  const timers = [];
  const fetchCalls = [];
  let planCloseCalls = 0;
  const fetchImpl = options.fetch || (async (url) => ({
    ok: true,
    json: async () => (String(url).includes('/diff?')
      ? { path: '/p', kind: 'modify', lang: 'go', truncated: false, hunks: [] }
      : { file_changes: [] })
  }));

  let drawerMatches = Boolean(options.drawer);
  const mediaListeners = [];
  const media = {
    get matches() { return drawerMatches; },
    addEventListener(type, listener) { if (type === 'change') mediaListeners.push(listener); },
    removeEventListener(type, listener) {
      if (type !== 'change') return;
      const idx = mediaListeners.indexOf(listener);
      if (idx !== -1) mediaListeners.splice(idx, 1);
    },
    addListener(listener) { mediaListeners.push(listener); },
    removeListener(listener) {
      const idx = mediaListeners.indexOf(listener);
      if (idx !== -1) mediaListeners.splice(idx, 1);
    }
  };
  const windowListeners = new Map();
  const setDrawer = (value) => {
    drawerMatches = Boolean(value);
    mediaListeners.slice().forEach((listener) => listener({ matches: drawerMatches }));
  };

  const queuedComments = [];
  const commentSendModes = new Map();
  const app = {
    createEl(tag, className, text) {
      const element = document.createElement(tag);
      if (className) element.className = className;
      if (text !== undefined) element.textContent = text;
      return element;
    },
    UI_PREFIX: '/chat',
    STORAGE_KEYS: { diffSidebarWidth: 'term_llm_diff_sidebar_width' },
    state,
    elements,
    clipboardWrites: [],
    closeCurrentPlanSurface() { planCloseCalls += 1; },
    diffCommentSendMode(sessionId) { return commentSendModes.get(sessionId) || 'send'; },
    setDiffCommentSendMode(sessionId, mode) {
      commentSendModes.set(sessionId, mode === 'queue' ? 'queue' : 'send');
    },
    queuedDiffComments(sessionId, anchor = null) {
      const comments = queuedComments.filter((entry) => entry.sessionId === sessionId).map((entry) => entry.comment);
      if (!anchor) return comments;
      return comments.filter((comment) => comment.path === anchor.path && comment.side === anchor.side && comment.line === anchor.line);
    },
    queueDiffComment(sessionId, comment) {
      queuedComments.push({ sessionId, comment });
      this.bumpDiffCommentPathRevision?.(sessionId, comment.path);
      this.renderDiffSidebar?.(sessionId);
      return true;
    },
    getClipboardWriter() {
      const writes = this.clipboardWrites;
      return { writeText: (text) => { writes.push(String(text)); return Promise.resolve(); } };
    },
    setElementHidden(element, hidden) {
      if (!element) return;
      element.hidden = Boolean(hidden);
      if (hidden) element.setAttribute?.('hidden', '');
      else element.removeAttribute?.('hidden');
    },
    setAnimatedPanelOpen({ panel, open, openClass = 'open', hiddenWhenClosed = false, classTargets = null } = {}) {
      if (!panel) return;
      const targets = Array.isArray(classTargets) && classTargets.length > 0
        ? classTargets
        : [{ element: panel, className: openClass }];
      if (open && hiddenWhenClosed) app.setElementHidden(panel, false);
      targets.forEach((target) => target.element?.classList?.toggle?.(target.className || openClass, Boolean(open)));
      if (!open && hiddenWhenClosed) app.setElementHidden(panel, true);
    },
    initPanelSwipeToClose({ panel, side = 'left', isEnabled = () => true, isOpen = () => true, shouldIgnoreTarget = null, onClose = null } = {}) {
      const direction = side === 'right' ? 1 : -1;
      let start = null;
      panel.addEventListener('pointerdown', (event) => {
        if (!isEnabled() || !isOpen() || shouldIgnoreTarget?.(event.target)) return;
        start = { x: Number(event.clientX) || 0, y: Number(event.clientY) || 0, dragging: false };
      });
      panel.addEventListener('pointermove', (event) => {
        if (!start) return;
        const dx = (Number(event.clientX) || 0) - start.x;
        const dy = Math.abs((Number(event.clientY) || 0) - start.y);
        const closeDelta = dx * direction;
        if (!start.dragging) {
          if (Math.abs(dx) < 8 && dy < 8) return;
          if (dy > Math.abs(dx) * 1.15 || closeDelta <= 0) {
            start = null;
            return;
          }
          start.dragging = true;
          panel.classList.add('panel-swipe-dragging');
        }
        panel.style.setProperty('--panel-swipe-offset-x', `${direction * closeDelta}px`);
      });
      panel.addEventListener('pointerup', (event) => {
        if (!start) return;
        const dx = (Number(event.clientX) || 0) - start.x;
        const closeDelta = dx * direction;
        const shouldClose = start.dragging && closeDelta >= 70;
        panel.classList.remove('panel-swipe-dragging');
        panel.style.removeProperty('--panel-swipe-offset-x');
        start = null;
        if (shouldClose) onClose?.(event);
      });
      panel.addEventListener('pointercancel', () => {
        panel.classList.remove('panel-swipe-dragging');
        panel.style.removeProperty('--panel-swipe-offset-x');
        start = null;
      });
    }
  };

  const document = new Element('document');
  document.ownerDocument = document;
  document.activeElement = null;
  document.createElement = (tag) => {
    const element = new Element(tag);
    element.ownerDocument = document;
    return element;
  };
  document.body = document.createElement('body');
  document.appendChild(document.body);
  document.body.appendChild(elements.appShell);
  elements.appShell.append(elements.appMain, elements.sidebar, elements.diffSidebar);
  elements.appMain.appendChild(elements.diffToggleBtn);
  elements.diffSidebar.append(
    elements.diffSidebarTotals,
    elements.diffSidebarCloseBtn,
    elements.diffResizeHandle,
    elements.diffFileList,
    elements.diffBulkToggleBtn,
    elements.diffMaximizeBtn,
    elements.diffQueueBar,
    elements.diffFilterRow
  );
  elements.diffFilterRow.appendChild(elements.diffFilterInput);
  const elementIDs = {
    appMain: elements.appMain,
    diffMaximizeBtn: elements.diffMaximizeBtn,
    diffQueueBar: elements.diffQueueBar
  };
  document.getElementById = (id) => elementIDs[id] || null;
  Object.values(elements).forEach((element) => { element.ownerDocument = document; });

  const context = {
    window: {
      TermLLMApp: app,
      innerWidth: options.innerWidth || 1280,
      matchMedia: () => media,
      addEventListener(type, listener) {
        const listeners = windowListeners.get(type) || [];
        listeners.push(listener);
        windowListeners.set(type, listeners);
      },
      removeEventListener(type, listener) {
        const listeners = windowListeners.get(type) || [];
        const idx = listeners.indexOf(listener);
        if (idx !== -1) listeners.splice(idx, 1);
        windowListeners.set(type, listeners);
      },
      dispatchEvent(event) {
        const listeners = windowListeners.get(event?.type || '') || [];
        listeners.slice().forEach((listener) => listener(event));
      },
      ...(options.hljs ? { hljs: options.hljs } : {})
    },
    document,
    localStorage,
    console,
    URLSearchParams,
    encodeURIComponent,
    clearTimeout: (id) => {
      const timer = timers.find((t) => t.id === id);
      if (timer) timer.cancelled = true;
    },
    setTimeout: (fn, delay) => {
      const id = timers.length + 1;
      timers.push({ id, fn, delay, cancelled: false });
      return id;
    },
    fetch: (...args) => {
      fetchCalls.push(String(args[0]));
      return fetchImpl(...args);
    }
  };
  context.globalThis = context;
  app.apiFetch = (...args) => context.fetch(...args);
  if (options.diffComments) vm.runInNewContext(commentSource, context, { filename: 'app-diff-comments.js' });
  vm.runInNewContext(source, context, { filename: 'app-diffs.js' });

  const flushTimers = async () => {
    const pending = timers.splice(0, timers.length);
    for (const timer of pending) {
      if (!timer.cancelled) timer.fn();
    }
    await new Promise((resolve) => setImmediate(resolve));
  };

  return {
    app, elements, state, document, localStorage, storage, timers, fetchCalls, queuedComments, flushTimers, setDrawer, media, windowObj: context.window,
    get planCloseCalls() { return planCloseCalls; }
  };
}

function elementText(node) {
  if (!node) return '';
  if (!node.children || node.children.length === 0) return node.textContent || '';
  return node.children.map(elementText).join('');
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
  await run('buildDiffRowModel numbers lines across hunks', () => {
    const { app } = createHarness();
    const rows = app.buildDiffRowModel([
      { old_start: 3, new_start: 3, lines: [
        { t: 'ctx', s: 'a' },
        { t: 'del', s: 'old' },
        { t: 'add', s: 'new1' },
        { t: 'add', s: 'new2' },
        { t: 'ctx', s: 'b' }
      ] },
      { old_start: 20, new_start: 21, lines: [{ t: 'add', s: 'tail' }] }
    ]);

    assertEqual(rows[0].type, 'ctx', 'first row is context');
    assertEqual(rows[0].oldNo, 3, 'context old line number');
    assertEqual(rows[0].newNo, 3, 'context new line number');
    assertEqual(rows[1].type, 'del', 'second row is deletion');
    assertEqual(rows[1].oldNo, 4, 'deletion advances old number');
    assertEqual(rows[1].newNo, 0, 'deletion has no new number');
    assertEqual(rows[2].newNo, 4, 'first addition new number');
    assertEqual(rows[3].newNo, 5, 'second addition new number');
    assertEqual(rows[4].oldNo, 5, 'trailing context old number skips deletion');
    assertEqual(rows[4].newNo, 6, 'trailing context new number counts additions');
    assertEqual(rows[5].type, 'hunk', 'hunk separator between hunks');
    assertEqual(rows[6].type, 'add', 'second hunk first row is an addition');
    assertEqual(rows[6].oldNo, 0, 'addition has no old number');
    assertEqual(rows[6].newNo, 21, 'second hunk new start');
  });

  await run('handleFileChangeEvent keeps sidebar closed and tracks files until explicit toggle', () => {
    const { app, elements } = createHarness();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/work/a.go', kind: 'create', adds: 3, dels: 0, seq: 1 });

    assert(elements.diffSidebar.hidden, 'sidebar stays closed on first change');
    assert(!elements.appShell.classList.contains('diff-open'), 'grid does not open third column');
    assert(!elements.diffToggleBtn.hidden, 'toggle button visible');
    assertEqual(elementText(elements.diffToggleBadge), '+3', 'badge shows diff stat');

    app.toggleDiffSidebar();
    assert(!elements.diffSidebar.hidden, 'explicit toggle reveals sidebar');
    assert(elements.appShell.classList.contains('diff-open'), 'explicit toggle opens third column');
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-row').length, 1, 'one file row rendered after toggle');
  });

  await run('collapsing or removing an anchor clears stale comment-panel state', async () => {
    const harness = createHarness({ fetch: async (url) => ({
      ok: true,
      json: async () => (String(url).includes('/diff?')
        ? { path: '/a', kind: 'modify', lang: 'go', truncated: false, hunks: [] }
        : { file_changes: [] })
    }) });
    const { app } = harness;
    let openPath = '/a';
    const cleared = [];
    app.diffCommentPanelOpen = () => Boolean(openPath);
    app.clearDiffCommentPanel = (_sessionId, pathName = '') => {
      if (pathName && pathName !== openPath) return false;
      cleared.push(pathName || openPath);
      openPath = '';
      return true;
    };
    app.reconcileDiffCommentPanel = (_sessionId, retained) => {
      if (openPath && !retained.has(openPath)) app.clearDiffCommentPanel(_sessionId);
    };
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 1, dels: 0, seq: 1 });
    app.toggleDiffSidebar();
    app.toggleDiffFile('s1', '/a');
    assert(cleared.includes('/a') && !app.diffCommentPanelOpen('s1'), 'explicit collapse retained stale openPanel state');

    openPath = '/a';
    await app.fetchSessionFileChanges('s1');
    assert(!app.diffCommentPanelOpen('s1'), 'authoritative anchor removal retained stale openPanel state');
  });

  await run('opening Changes closes the Plan surface in wide and drawer modes', () => {
    for (const drawer of [false, true]) {
      const harness = createHarness({ drawer });
      harness.app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 1, dels: 0, seq: 1 });
      harness.app.toggleDiffSidebar();
      assertEqual(harness.planCloseCalls, 1, `opening ${drawer ? 'drawer' : 'wide panel'} closes plan`);
      harness.app.toggleDiffSidebar();
      assertEqual(harness.planCloseCalls, 1, `closing ${drawer ? 'drawer' : 'wide panel'} does not close plan again`);
    }
  });

  await run('stale seq replays are idempotent', () => {
    const { app, elements } = createHarness();
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 5, dels: 2, seq: 7 });
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 1, dels: 1, seq: 3 });

    const counts = elements.diffFileList.querySelector('.diff-count-add');
    assertEqual(counts.textContent, '+5', 'older replayed event did not overwrite newer state');
  });

  await run('live operation metadata does not replace cumulative file metadata before refresh', () => {
    const { app, elements } = createHarness();
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/new.txt', kind: 'create', adds: 20, dels: 0, seq: 1 });
    app.handleFileChangeEvent({ id: 's1' }, { path: '/new.txt', kind: 'modify', adds: 1, dels: 1, seq: 2 });

    assertEqual(elements.diffFileList.querySelector('.diff-kind-badge').textContent, 'A', 'incremental modify did not relabel a cumulative create');
    assertEqual(elements.diffFileList.querySelector('.diff-count-add').textContent, '+20', 'incremental counts did not replace cumulative additions');
    assert(!elements.diffFileList.querySelector('.diff-count-del'), 'incremental deletion count did not flash before canonical refresh');
  });

  await run('events for inactive sessions update data without DOM', () => {
    const { app, elements } = createHarness();
    app.handleFileChangeEvent({ id: 'other' }, { path: '/b', kind: 'create', adds: 1, dels: 0, seq: 1 });

    assert(elements.diffSidebar.hidden, 'sidebar stays hidden for inactive session');
    assertEqual(elements.diffFileList.children.length, 0, 'no rows rendered');
  });

  await run('burst of events coalesces fetches', async () => {
    const { app, fetchCalls, flushTimers } = createHarness();
    app.toggleDiffSidebar();
    for (let i = 1; i <= 10; i += 1) {
      app.handleFileChangeEvent({ id: 's1' }, { path: '/hot.go', kind: 'modify', adds: i, dels: 0, seq: i });
    }
    // First display triggers one immediate fetch; the rest of the burst is
    // deduped by the in-flight guard.
    assertEqual(fetchCalls.filter((u) => u.includes('/diff?')).length, 1, 'one initial display fetch');
    await flushTimers();
    await flushTimers();
    // The initial fetch was stale (seq 1 of 10), so exactly one refresh
    // follows: 10 events → 2 fetches total, not 10.
    assertEqual(fetchCalls.filter((u) => u.includes('/diff?')).length, 2, 'one coalesced refresh after debounce');
    await flushTimers();
    assertEqual(fetchCalls.filter((u) => u.includes('/diff?')).length, 2, 'refresh converges once up to date');
  });

  await run('live changes follow the most recently edited file only', async () => {
    const { app, elements, flushTimers } = createHarness();
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 1, dels: 0, seq: 1 });
    app.handleFileChangeEvent({ id: 's1' }, { path: '/b', kind: 'create', adds: 1, dels: 0, seq: 2 });
    await flushTimers();

    const rows = elements.diffFileList.querySelectorAll('.diff-file-row');
    assertEqual(rows.length, 2, 'both files listed');
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 1, 'only the file being edited stays expanded');
    assertEqual(rows[0].dataset.path, '/b', 'most recent file sorts to the top');
    assert(rows[0].classList.contains('expanded'), 'followed file header carries expanded state');
    assert(!rows[1].classList.contains('expanded'), 'previously followed file auto-collapsed');
  });

  await run('user-expanded files survive live-follow auto-collapse', async () => {
    const { app, elements, flushTimers } = createHarness();
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 1, dels: 0, seq: 1 });
    await flushTimers();

    // /a is auto-expanded; the user pins it open by toggling it closed and open.
    app.toggleDiffFile('s1', '/a');
    app.toggleDiffFile('s1', '/a');
    app.handleFileChangeEvent({ id: 's1' }, { path: '/b', kind: 'create', adds: 1, dels: 0, seq: 2 });
    await flushTimers();

    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 2, 'pinned file stays open while live-follow moves on');
  });

  await run('comment interaction pins a live-followed file while explicit collapse still wins', async () => {
    const { app, elements, flushTimers } = createHarness();
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 1, dels: 0, seq: 1 });
    await flushTimers();
    assert(app.pinDiffFileExpanded('s1', '/a'), 'comment pin did not recognize the file');
    app.renderDiffSidebar('s1');
    app.handleFileChangeEvent({ id: 's1' }, { path: '/b', kind: 'create', adds: 1, dels: 0, seq: 2 });
    await flushTimers();
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 2, 'commented file collapsed when live-follow moved');
    app.toggleDiffFile('s1', '/a');
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 2, dels: 0, seq: 3 });
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 1, 'live edit overrode explicit user collapse');
  });

  await run('open comment panel suppresses and consumes live-follow pending scroll', async () => {
    const { app, elements, flushTimers } = createHarness();
    app.diffCommentPanelOpen = () => true;
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 1, dels: 0, seq: 1 });
    await flushTimers();
    const row = elements.diffFileList.querySelector('.diff-file-row');
    assert(!row.lastScrollIntoView, 'live-follow scrolled away while a comment panel was open');
    app.diffCommentPanelOpen = () => false;
    app.renderDiffSidebar('s1');
    assert(!row.lastScrollIntoView, 'suppressed pending scroll fired later');
  });

  await run('expanded diff refetch restores only focus owned by rebuilt inline comment UI', async () => {
    const files = ['/shipping.rb', '/receipt.rb', '/loyalty.rb'];
    const harness = createHarness({
      diffComments: true,
      fetch: async (url) => {
        if (String(url).includes('/diff?')) {
          const params = new URLSearchParams(String(url).split('?')[1] || '');
          const pathName = params.get('path') || '/loyalty.rb';
          return { ok: true, json: async () => ({
            path: pathName, kind: 'modify', lang: 'ruby', truncated: false,
            hunks: [{ old_start: 1, new_start: 1, lines: [{ t: 'add', s: `change in ${pathName}` }] }]
          }) };
        }
        return { ok: true, json: async () => ({ file_changes: files.map((pathName, index) => ({
          path: pathName, kind: 'modify', adds: 1, dels: 0, truncated: false, seq: index + 1
        })) }) };
      }
    });
    const { app, elements, document, flushTimers, queuedComments } = harness;
    const event = (type, target) => ({
      type, target, preventDefault() {}, stopPropagation() {}, stopImmediatePropagation() {}
    });

    app.toggleDiffSidebar();
    files.forEach((pathName, index) => app.handleFileChangeEvent(
      { id: 's1' },
      { path: pathName, kind: 'modify', adds: 1, dels: 0, seq: index + 1 }
    ));
    await flushTimers();
    await flushTimers();
    await elements.diffBulkToggleBtn.dispatchEvent(event('click', elements.diffBulkToggleBtn));
    await elements.diffMaximizeBtn.dispatchEvent(event('click', elements.diffMaximizeBtn));

    const loyaltyFile = elements.diffFileList.querySelectorAll('.diff-file-row')
      .find((row) => row.dataset.path === '/loyalty.rb')?.parentNode;
    const marker = loyaltyFile?.querySelector('.diff-comment-affordance');
    assert(marker, 'loyalty comment marker was not rendered');
    await marker.dispatchEvent(event('click', marker));
    const textarea = loyaltyFile.querySelector('textarea');
    textarea.value = 'Double points during ';
    await textarea.dispatchEvent(event('input', textarea));
    assert(document.activeElement === textarea && textarea.isConnected, 'editor did not begin with real connected focus');

    const oldBody = loyaltyFile.querySelector('.diff-file-body');
    app.refreshFileChangesAfterRun({ id: 's1' });
    await flushTimers();
    await flushTimers();

    const rebuiltLoyaltyFile = elements.diffFileList.querySelectorAll('.diff-file-row')
      .find((row) => row.dataset.path === '/loyalty.rb')?.parentNode;
    const restored = rebuiltLoyaltyFile?.querySelector('textarea');
    assert(rebuiltLoyaltyFile?.querySelector('.diff-file-body') !== oldBody, 'test did not replace the loyalty body');
    assert(rebuiltLoyaltyFile?.querySelectorAll('.diff-comment-panel').length === 1
      && rebuiltLoyaltyFile.querySelectorAll('.diff-comment-editor').length === 1, 'rebuilt comment UI was duplicated or lost');
    assert(restored?.value === 'Double points during ', 'rebuilt editor lost its draft');
    assert(document.activeElement === restored && restored.isConnected, `rebuilt connected editor did not regain owned focus (active=${document.activeElement?.tagName}.${document.activeElement?.className}, restoredFocusCount=${restored?.focusCount}, connected=${restored?.isConnected})`);

    const outside = document.createElement('button');
    elements.appMain.appendChild(outside);
    outside.focus();
    app.bumpDiffCommentPathRevision('s1', '/loyalty.rb');
    app.renderDiffSidebar('s1');
    assert(document.activeElement === outside, 'open editor stole focus after the user moved elsewhere');

    const currentEditor = rebuiltLoyaltyFile.querySelector('textarea');
    currentEditor.value = 'Double points during holidays';
    await currentEditor.dispatchEvent(event('input', currentEditor));
    app.setDiffCommentSendMode('s1', 'queue');
    const queueButton = rebuiltLoyaltyFile.querySelector('.diff-comment-send');
    queueButton.focus();
    await queueButton.dispatchEvent(event('click', queueButton));
    const rebuiltMarker = rebuiltLoyaltyFile.querySelector('.diff-comment-affordance');
    assert(queuedComments.length === 1, 'queue submission did not complete');
    assert(document.activeElement === rebuiltMarker && rebuiltMarker.isConnected, 'queue submission did not focus the attached rebuilt marker');
  });

  await run('file-list reorder preserves exact focus and draft in an untouched third-file editor', async () => {
    const initialFiles = [
      { path: '/loyalty.rb', seq: 1 },
      { path: '/shipping.rb', seq: 2 },
      { path: '/receipt.rb', seq: 3 }
    ];
    const harness = createHarness({
      diffComments: true,
      fetch: async (url) => {
        if (String(url).includes('/diff?')) {
          const params = new URLSearchParams(String(url).split('?')[1] || '');
          const pathName = params.get('path') || '/loyalty.rb';
          return { ok: true, json: async () => ({
            path: pathName, kind: 'modify', lang: 'ruby', truncated: false,
            hunks: [{ old_start: 1, new_start: 1, lines: [{ t: 'add', s: `change in ${pathName}` }] }]
          }) };
        }
        return { ok: true, json: async () => ({ file_changes: initialFiles.map((file) => ({
          path: file.path, kind: 'modify', adds: 1, dels: 0, truncated: false, seq: file.seq
        })) }) };
      }
    });
    const { app, elements, document, flushTimers } = harness;
    const event = (type, target) => ({
      type, target, preventDefault() {}, stopPropagation() {}, stopImmediatePropagation() {}
    });

    app.toggleDiffSidebar();
    initialFiles.forEach((file) => app.handleFileChangeEvent(
      { id: 's1' },
      { path: file.path, kind: 'modify', adds: 1, dels: 0, seq: file.seq }
    ));
    await flushTimers();
    await flushTimers();

    app.toggleDiffFile('s1', '/loyalty.rb');
    await flushTimers();
    await flushTimers();
    const loyaltyFile = elements.diffFileList.querySelectorAll('.diff-file-row')
      .find((row) => row.dataset.path === '/loyalty.rb')?.parentNode;
    const marker = loyaltyFile?.querySelector('.diff-comment-affordance');
    assert(marker, 'third-file loyalty comment marker was not rendered');
    await marker.dispatchEvent(event('click', marker));
    const textarea = loyaltyFile.querySelector('textarea');
    textarea.value = 'Keep the exact focused draft';
    await textarea.dispatchEvent(event('input', textarea));
    assert(document.activeElement === textarea, 'loyalty editor did not own focus before reorder');
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-row').map((row) => row.dataset.path).join(','),
      '/receipt.rb,/shipping.rb,/loyalty.rb', 'loyalty editor was not in the untouched third file');

    app.handleFileChangeEvent(
      { id: 's1' },
      { path: '/shipping.rb', kind: 'modify', adds: 2, dels: 0, seq: 4 }
    );
    await flushTimers();

    const reorderedLoyaltyFile = elements.diffFileList.querySelectorAll('.diff-file-row')
      .find((row) => row.dataset.path === '/loyalty.rb')?.parentNode;
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-row').map((row) => row.dataset.path).join(','),
      '/shipping.rb,/receipt.rb,/loyalty.rb', 'agent edit did not change file sort order');
    assert(reorderedLoyaltyFile === loyaltyFile, 'untouched loyalty block was rebuilt instead of reused');
    assert(reorderedLoyaltyFile.querySelector('textarea') === textarea, 'reorder replaced the exact loyalty textarea');
    assert(textarea.value === 'Keep the exact focused draft', 'reorder lost the loyalty draft');
    assert(document.activeElement === textarea && textarea.isConnected,
      `reorder did not restore exact connected textarea focus (active=${document.activeElement?.tagName})`);
  });

  await run('explicit collapse sticks while the agent keeps editing', async () => {
    const { app, elements, flushTimers } = createHarness();
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 1, dels: 0, seq: 1 });
    await flushTimers();

    app.toggleDiffFile('s1', '/a');
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 0, 'click collapses the body');

    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 2, dels: 0, seq: 2 });
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 0, 'further changes do not re-open a user-collapsed file');

    app.toggleDiffFile('s1', '/a');
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 1, 're-expanding works and clears the collapse memory');
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 3, dels: 0, seq: 3 });
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 1, 'stays expanded for later changes');
  });

  await run('large diffs render capped rows with a show-more control', async () => {
    const manyLines = Array.from({ length: 450 }, (_, i) => ({ t: 'add', s: `line ${i}` }));
    const { app, elements, flushTimers } = createHarness({
      fetch: async (url) => ({
        ok: true,
        json: async () => (String(url).includes('/diff?')
          ? { path: '/big.txt', kind: 'create', lang: '', truncated: false, hunks: [{ old_start: 1, new_start: 1, lines: manyLines }] }
          : { file_changes: [] })
      })
    });
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/big.txt', kind: 'create', adds: 450, dels: 0, seq: 1 });
    await flushTimers();
    await flushTimers();

    assertEqual(elements.diffFileList.querySelectorAll('.diff-row').length, 400, 'rows capped at the render limit');
    const rows = elements.diffFileList.querySelector('.diff-rows');
    assert(rows?.classList.contains('diff-rows-kind-create'), 'created text diff rows carry their namespaced kind class');
    const more = elements.diffFileList.querySelector('.diff-show-more');
    assert(more, 'show-more control rendered');
    assertEqual(more.textContent, 'Show 50 more lines', 'control reports hidden row count');

    await more.dispatchEvent({ type: 'click' });
    assertEqual(elements.diffFileList.querySelectorAll('.diff-row').length, 450, 'all rows rendered after opting in');
    assert(!elements.diffFileList.querySelector('.diff-show-more'), 'control gone once expanded');
  });

  await run('event bursts coalesce accordion re-renders', async () => {
    const { app, elements, flushTimers } = createHarness();
    app.toggleDiffSidebar();
    let renders = 0;
    const original = elements.diffFileList.replaceChildren.bind(elements.diffFileList);
    elements.diffFileList.replaceChildren = (...nodes) => {
      renders += 1;
      return original(...nodes);
    };

    for (let i = 1; i <= 20; i += 1) {
      app.handleFileChangeEvent({ id: 's1' }, { path: `/f${i}.txt`, kind: 'create', adds: 1, dels: 0, seq: i });
    }
    await flushTimers();
    await flushTimers();

    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-row').length, 20, 'all files present after coalesced render');
    assert(renders <= 5, `20 events should coalesce to a few renders, got ${renders}`);
  });

  await run('deleted text diffs carry their kind-specific styling class', async () => {
    const { app, elements, flushTimers } = createHarness({
      fetch: async (url) => ({
        ok: true,
        json: async () => (String(url).includes('/diff?')
          ? { path: '/old.txt', kind: 'create', lang: '', truncated: false, hunks: [{ old_start: 1, new_start: 1, lines: [{ t: 'del', s: 'old' }] }] }
          : { file_changes: [] })
      })
    });
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/old.txt', kind: 'delete', adds: 0, dels: 1, seq: 1 });
    await flushTimers();
    await flushTimers();

    const rows = elements.diffFileList.querySelector('.diff-rows');
    assert(rows?.classList.contains('diff-rows-kind-delete'), 'current file-list kind wins over stale cached diff kind');
  });

  await run('unknown file kinds safely fall back to modify styling', async () => {
    const { app, elements, flushTimers } = createHarness({
      fetch: async (url) => ({
        ok: true,
        json: async () => (String(url).includes('/diff?')
          ? { path: '/odd.txt', kind: 'create diff-error', lang: '', truncated: false, hunks: [{ old_start: 1, new_start: 1, lines: [{ t: 'add', s: 'line' }] }] }
          : { file_changes: [] })
      })
    });
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/odd.txt', kind: 'create diff-error', adds: 1, dels: 0, seq: 1 });
    await flushTimers();
    await flushTimers();

    const rows = elements.diffFileList.querySelector('.diff-rows');
    assert(rows?.classList.contains('diff-rows-kind-modify'), 'unknown kind uses modify styling');
    assert(!rows?.classList.contains('diff-error'), 'kind value cannot inject an additional class');
  });

  await run('syntax highlighting applies when hljs is available', async () => {
    const fakeHljs = {
      getLanguage: (name) => name === 'go',
      highlight: (text) => ({ value: `<span class="hljs-keyword">${text}</span>` })
    };
    const { app, elements, flushTimers } = createHarness({
      hljs: fakeHljs,
      fetch: async (url) => ({
        ok: true,
        json: async () => (String(url).includes('/diff?')
          ? { path: '/a.go', kind: 'modify', lang: 'go', truncated: false, hunks: [{ old_start: 1, new_start: 1, lines: [{ t: 'add', s: 'func main() {}' }] }] }
          : { file_changes: [] })
      })
    });
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a.go', kind: 'modify', adds: 1, dels: 0, seq: 1 });
    await flushTimers();
    await flushTimers();

    const rows = elements.diffFileList.querySelector('.diff-rows');
    assert(rows?.classList.contains('diff-rows-kind-modify'), 'modified text diff rows carry their namespaced kind class');
    const codes = elements.diffFileList.querySelectorAll('.diff-code');
    assert(codes.length > 0, 'diff rows rendered');
    assert(String(codes[0].innerHTML || '').includes('hljs-keyword'), 'code cell is hljs-highlighted');
  });

  await run('dismissing the sidebar is per-session and suppresses fetches', async () => {
    const { app, elements, state, fetchCalls, flushTimers } = createHarness();
    app.setDiffSidebarHidden(true);

    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 1, dels: 0, seq: 1 });
    assert(elements.diffSidebar.hidden, 'sidebar stays hidden in the dismissed session');
    assert(!elements.diffToggleBtn.hidden, 'toggle stays visible so user can reopen');
    await flushTimers();
    assertEqual(fetchCalls.filter((u) => u.includes('/diff?')).length, 0, 'no diff fetches while hidden');

    // Another session starts closed too; changes only make its toggle available.
    state.activeSessionId = 's2';
    app.handleFileChangeEvent({ id: 's2' }, { path: '/b', kind: 'create', adds: 1, dels: 0, seq: 1 });
    assert(elements.diffSidebar.hidden, 'other session also stays closed until explicitly opened');
    app.toggleDiffSidebar();
    assert(!elements.diffSidebar.hidden, 'other session opens after its own explicit toggle');

    // Switching back to the dismissed session keeps it dismissed.
    state.activeSessionId = 's1';
    app.activateDiffSidebar('s1');
    assert(elements.diffSidebar.hidden, 'dismissal remembered when returning to the session');
    assert(!elements.diffToggleBtn.hidden, 'toggle still offered on return');

    // Reopening via the toggle reveals it again for this session only.
    app.toggleDiffSidebar();
    assert(!elements.diffSidebar.hidden, 'toggle reopens the sidebar for the session');
  });

  await run('activateDiffSidebar with empty session hides everything', () => {
    const { app, elements } = createHarness();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 1, dels: 0, seq: 1 });
    app.activateDiffSidebar('');
    assert(elements.diffSidebar.hidden, 'sidebar hidden for draft session');
    assert(elements.diffToggleBtn.hidden, 'toggle hidden for draft session');
    assert(!elements.appShell.classList.contains('diff-open'), 'grid back to two columns');
  });

  await run('known clean session has no Changes toggle and makes no request', async () => {
    const { app, elements, fetchCalls, flushTimers } = createHarness();
    app.applySessionDiffSummary('s1', { file_count: 0, adds: 0, dels: 0 });
    app.activateDiffSidebar('s1');
    await flushTimers();

    assert(elements.diffToggleBtn.hidden, 'clean session has no changes affordance');
    assert(elements.diffSidebar.hidden, 'clean session keeps sidebar hidden');
    assertEqual(elements.diffToggleBadge.children.length, 0, 'clean session has no provisional badge');
    assertEqual(fetchCalls.filter((url) => url.endsWith('/file-changes')).length, 0, 'clean startup does not fetch the full list');
  });

  await run('aggregate metadata renders the final Changes header before the list loads', async () => {
    const { app, elements, fetchCalls, flushTimers } = createHarness();
    app.applySessionDiffSummary('s1', { file_count: 2, adds: 4, dels: 2 });
    app.activateDiffSidebar('s1');
    await flushTimers();

    assert(!elements.diffToggleBtn.hidden, 'changed session exposes Changes immediately');
    const fileCount = elements.diffToggleBadge.querySelector('.diff-toggle-file-count');
    assert(fileCount, 'changed toggle renders the file icon/count');
    assertEqual(fileCount.dataset.fileCount, '2', 'aggregate metadata supplies the actual file count');
    assertEqual(elementText(elements.diffToggleBadge), '+4−2', 'aggregate metadata supplies additions and deletions');
    assertEqual(elements.diffToggleBtn.title, '2 changed files (+4 −2)', 'initial title is final and accurate');
    assertEqual(elements.diffFileList.children.length, 0, 'full file rows remain lazy');
    assertEqual(fetchCalls.filter((url) => url.endsWith('/file-changes')).length, 0, 'rendering the header does not fetch the list');
  });

  await run('opening a known changed session fetches the full list exactly once', async () => {
    const { app, elements, fetchCalls, flushTimers } = createHarness({
      fetch: async (url) => ({
        ok: true,
        json: async () => (String(url).includes('/diff?')
          ? { path: '/work/a.go', kind: 'modify', lang: 'go', truncated: false, hunks: [] }
          : { file_changes: [
              { path: '/work/a.go', kind: 'modify', adds: 3, dels: 2, truncated: false },
              { path: '/work/b.go', kind: 'create', adds: 1, dels: 0, truncated: false }
            ] })
      })
    });
    app.applySessionDiffSummary('s1', { file_count: 2, adds: 4, dels: 2 });
    app.activateDiffSidebar('s1');

    app.toggleDiffSidebar();
    await flushTimers();
    assertEqual(fetchCalls.filter((url) => url.endsWith('/file-changes')).length, 1, 'first reveal fetches the exact list once');
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-row').length, 2, 'drawer is populated from the lazy list');

    app.toggleDiffSidebar();
    app.toggleDiffSidebar();
    await flushTimers();
    assertEqual(fetchCalls.filter((url) => url.endsWith('/file-changes')).length, 1, 'reopening reuses the loaded list');
  });

  await run('drawer mode: no auto-open, toggle opens populated panel', async () => {
    const { app, elements, flushTimers } = createHarness({ drawer: true });
    app.handleFileChangeEvent({ id: 's1' }, { path: '/work/a.go', kind: 'create', adds: 3, dels: 0, seq: 1 });
    await flushTimers();

    assert(elements.diffSidebar.hidden, 'drawer never auto-opens on changes');
    assert(!elements.diffToggleBtn.hidden, 'toggle visible so user can open the drawer');

    app.toggleDiffSidebar();
    assert(!elements.diffSidebar.hidden, 'drawer opens via toggle');
    assert(elements.diffSidebar.classList.contains('open'), 'drawer gets open class');
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-row').length, 1, 'file list populated on open');
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 1, 'changed file expanded on open');

    app.toggleDiffSidebar();
    assert(!elements.diffSidebar.classList.contains('open'), 'second toggle closes the drawer');
  });

  await run('drawer to wide viewport keeps diff closed without explicit open', async () => {
    const { app, elements, setDrawer, flushTimers } = createHarness({ drawer: true });
    app.handleFileChangeEvent({ id: 's1' }, { path: '/work/a.go', kind: 'create', adds: 3, dels: 0, seq: 1 });
    await flushTimers();

    assert(elements.diffSidebar.hidden, 'narrow drawer stays closed after live change');
    assert(!elements.appShell.classList.contains('diff-open'), 'narrow mode does not add a grid column');

    setDrawer(false);
    assert(elements.diffSidebar.hidden, 'wide mode stays closed without an explicit open');
    assert(!elements.diffSidebar.classList.contains('open'), 'drawer-open class is cleared in wide mode');
    assert(!elements.appShell.classList.contains('diff-open'), 'wide mode does not add the grid diff column');
    assert(!elements.diffToggleBtn.hidden, 'toggle remains available');
  });

  await run('wide to drawer viewport closes column but keeps toggle available', () => {
    const { app, elements, setDrawer } = createHarness();
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/work/a.go', kind: 'create', adds: 3, dels: 0, seq: 1 });
    assert(!elements.diffSidebar.hidden, 'wide panel starts visible');
    assert(elements.appShell.classList.contains('diff-open'), 'wide mode has a diff column');

    setDrawer(true);
    assert(elements.diffSidebar.hidden, 'narrow drawer is closed after crossing breakpoint');
    assert(!elements.appShell.classList.contains('diff-open'), 'narrow mode removes the grid diff column');
    assert(!elements.diffToggleBtn.hidden, 'toggle remains available in drawer mode');
  });

  await run('drawer swipe right closes the diff drawer', async () => {
    const { app, elements, flushTimers } = createHarness({ drawer: true });
    app.handleFileChangeEvent({ id: 's1' }, { path: '/work/a.go', kind: 'create', adds: 3, dels: 0, seq: 1 });
    await flushTimers();
    app.toggleDiffSidebar();
    assert(elements.diffSidebar.classList.contains('open'), 'drawer starts open');

    await elements.diffSidebar.dispatchEvent({ type: 'pointerdown', target: elements.diffSidebar, clientX: 120, clientY: 40 });
    await elements.diffSidebar.dispatchEvent({ type: 'pointermove', target: elements.diffSidebar, clientX: 205, clientY: 48 });
    assertEqual(elements.diffSidebar.style.getPropertyValue('--panel-swipe-offset-x'), '85px', 'drawer follows the touch move');
    await elements.diffSidebar.dispatchEvent({ type: 'pointerup', target: elements.diffSidebar, clientX: 215, clientY: 48 });
    assert(!elements.diffSidebar.classList.contains('open'), 'rightward swipe closes drawer');
  });

  await run('drawer vertical scroll gesture does not close the diff drawer', async () => {
    const { app, elements, flushTimers } = createHarness({ drawer: true });
    app.handleFileChangeEvent({ id: 's1' }, { path: '/work/a.go', kind: 'create', adds: 3, dels: 0, seq: 1 });
    await flushTimers();
    app.toggleDiffSidebar();

    await elements.diffSidebar.dispatchEvent({ type: 'pointerdown', target: elements.diffSidebar, clientX: 120, clientY: 40 });
    await elements.diffSidebar.dispatchEvent({ type: 'pointermove', target: elements.diffSidebar, clientX: 150, clientY: 150 });
    await elements.diffSidebar.dispatchEvent({ type: 'pointerup', target: elements.diffSidebar, clientX: 150, clientY: 150 });
    assert(elements.diffSidebar.classList.contains('open'), 'mostly vertical gesture keeps drawer open');
  });

  await run('clampDiffWidth bounds the panel width', () => {
    const { app } = createHarness();
    assertEqual(app.clampDiffWidth(100, 1280), 280, 'narrow drags clamp to minimum');
    assertEqual(app.clampDiffWidth(400, 1280), 400, 'in-range widths pass through');
    assertEqual(app.clampDiffWidth(2000, 1280), 768, 'wide drags clamp to 60% of viewport');
    assertEqual(app.clampDiffWidth(2000, 4000), 900, 'absolute cap at 900px');
    assertEqual(app.clampDiffWidth(500, 0), 500, 'missing viewport falls back sanely');
  });

  await run('activateDiffSidebar defers the server list until interaction', async () => {
    const { app, state, fetchCalls, flushTimers } = createHarness();
    state.activeSessionId = 's2';
    app.activateDiffSidebar('s2');
    await flushTimers();
    app.activateDiffSidebar('s2');
    await flushTimers();
    assertEqual(fetchCalls.filter((u) => u.includes('s2') && u.endsWith('/file-changes')).length, 0, 'activation does not fetch a hidden ancillary list');

    app.toggleDiffSidebar();
    await flushTimers();
    assertEqual(fetchCalls.filter((u) => u.includes('s2') && u.endsWith('/file-changes')).length, 1, 'first reveal lazily fetches the list once');
    app.activateDiffSidebar('s2');
    await flushTimers();
    assertEqual(fetchCalls.filter((u) => u.includes('s2') && u.endsWith('/file-changes')).length, 1, 'loaded list stays cached for the session');
  });

  await run('numeric route stub never reaches file-changes endpoint', async () => {
    const { app, state, fetchCalls, flushTimers } = createHarness();
    state.activeSessionId = '3347';
    app.activateDiffSidebar('3347');
    app.toggleDiffSidebar();
    await flushTimers();
    assertEqual(fetchCalls.filter((u) => u.includes('/sessions/3347/')).length, 0, 'unresolved identity emitted a session-scoped diff request');
  });

  await run('server list is authoritative and removes stale duplicate live rows', async () => {
    const { app, elements } = createHarness({
      fetch: async (url) => ({
        ok: true,
        json: async () => (String(url).includes('/diff?')
          ? { path: '/home/sam/Source/term-llm/hello_world.txt', kind: 'delete', lang: 'txt', truncated: false, hunks: [] }
          : { file_changes: [{ path: '/home/sam/Source/term-llm/hello_world.txt', kind: 'delete', adds: 0, dels: 3, truncated: false }] })
      })
    });

    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: 'hello_world.txt', kind: 'delete', adds: 0, dels: 3, seq: 1 });
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-row').length, 1, 'one live row rendered');

    await app.fetchSessionFileChanges('s1');
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-row').length, 1, 'server refresh replaced stale live row instead of adding a duplicate');
    assertEqual(elementText(elements.diffToggleBadge), '−3', 'badge shows cumulative diff stat');
  });

  await run('sortDiffPaths orders by recency then path', () => {
    const { app } = createHarness();
    const order = app.sortDiffPaths([
      { path: '/b', lastSeq: 1 },
      { path: '/c', lastSeq: 3 },
      { path: '/a', lastSeq: 1 },
      { path: '/d', lastSeq: 2 }
    ]);
    assertEqual(order.join(','), '/c,/d,/a,/b', 'recent first, ties by path');
  });

  await run('computeInlineEmphasis marks the changed span of paired lines', () => {
    const { app } = createHarness();
    const rows = app.computeInlineEmphasis([
      { type: 'ctx', text: 'unchanged' },
      { type: 'del', text: 'const a = 1;' },
      { type: 'del', text: 'foo' },
      { type: 'add', text: 'const a = 2;' },
      { type: 'add', text: 'totally different xyz' }
    ]);
    assertEqual(rows[1].emph.join(','), '10,11', 'del emphasis covers the changed literal');
    assertEqual(rows[3].emph.join(','), '10,11', 'add emphasis covers the changed literal');
    assert(!rows[2].emph, 'unrelated del line gets no emphasis');
    assert(!rows[4].emph, 'unrelated add line gets no emphasis');
    assert(!rows[0].emph, 'context lines get no emphasis');
  });

  await run('emphasized rows render a diff-word mark span', async () => {
    const { app, elements, flushTimers } = createHarness({
      fetch: async (url) => ({
        ok: true,
        json: async () => (String(url).includes('/diff?')
          ? { path: '/a.go', kind: 'modify', lang: '', truncated: false, hunks: [{ old_start: 1, new_start: 1, lines: [
              { t: 'del', s: 'count = 1' },
              { t: 'add', s: 'count = 2' }
            ] }] }
          : { file_changes: [] })
      })
    });
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a.go', kind: 'modify', adds: 1, dels: 1, seq: 1 });
    await flushTimers();
    await flushTimers();

    const marks = elements.diffFileList.querySelectorAll('.diff-word');
    assertEqual(marks.length, 2, 'both paired rows carry a word mark');
    assertEqual(marks[0].textContent, '1', 'del mark wraps the removed literal');
    assertEqual(marks[1].textContent, '2', 'add mark wraps the inserted literal');
  });

  await run('image diffs render before and after previews', async () => {
    const { app, elements, flushTimers } = createHarness({
      fetch: async (url) => ({
        ok: true,
        json: async () => (String(url).includes('/diff?')
          ? { path: '/work/animated preview.gif', kind: 'modify', lang: 'gif', truncated: false, image: true, hunks: [] }
          : { file_changes: [] })
      })
    });
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/work/animated preview.gif', kind: 'modify', adds: 0, dels: 0, seq: 1 });
    await flushTimers();
    await flushTimers();

    const previews = elements.diffFileList.querySelectorAll('.diff-image-preview');
    assertEqual(previews.length, 2, 'modified image shows both sides');
    assert(previews[0].src.includes('/file-changes/content?'), 'preview uses recorded content endpoint');
    assert(previews[0].src.includes('side=before'), 'first preview is baseline image');
    assert(previews[0].src.includes('path=%2Fwork%2Fanimated%20preview.gif'), 'image path is URL encoded');
    assert(previews[1].src.includes('side=after'), 'second preview is current image');
    assertEqual(elementText(elements.diffFileList.querySelector('.diff-image-label')), 'Before', 'preview side is labelled');
    assertEqual(elements.diffFileList.querySelectorAll('.diff-row').length, 0, 'binary image is not rendered as text rows');
    const actions = elements.diffFileList.querySelectorAll('.diff-action-btn');
    assert(actions[1].hidden, 'copy-diff action is hidden for image files');

    await previews[0].dispatchEvent({ type: 'error' });
    assert(previews[0].hidden, 'failed preview is hidden');
    assertEqual(elementText(elements.diffFileList.querySelector('.diff-image-error')), 'Preview unavailable', 'failed preview shows a useful fallback');
  });

  await run('created image diffs render only the current preview', async () => {
    const { app, elements, flushTimers } = createHarness({
      fetch: async (url) => ({
        ok: true,
        json: async () => (String(url).includes('/diff?')
          ? { path: '/work/new.png', kind: 'create', truncated: false, image: true, hunks: [] }
          : { file_changes: [] })
      })
    });
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/work/new.png', kind: 'create', adds: 0, dels: 0, seq: 1 });
    await flushTimers();
    await flushTimers();

    const previews = elements.diffFileList.querySelectorAll('.diff-image-preview');
    assertEqual(previews.length, 1, 'created image shows one preview');
    assert(previews[0].src.includes('side=after'), 'created image shows the current side');
    assertEqual(elementText(elements.diffFileList.querySelector('.diff-image-label')), 'After', 'created image side is labelled');
  });

  await run('deleted image diffs render only the baseline preview', async () => {
    const { app, elements, flushTimers } = createHarness({
      fetch: async (url) => ({
        ok: true,
        json: async () => (String(url).includes('/diff?')
          ? { path: '/work/old.gif', kind: 'delete', truncated: false, image: true, hunks: [] }
          : { file_changes: [] })
      })
    });
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/work/old.gif', kind: 'delete', adds: 0, dels: 0, seq: 1 });
    await flushTimers();
    await flushTimers();

    const previews = elements.diffFileList.querySelectorAll('.diff-image-preview');
    assertEqual(previews.length, 1, 'deleted image shows one preview');
    assert(previews[0].src.includes('side=before'), 'deleted image shows the baseline side');
    assertEqual(elementText(elements.diffFileList.querySelector('.diff-image-label')), 'Before', 'deleted image side is labelled');
  });

  await run('failed diff fetches surface an error with retry', async () => {
    let failFetches = true;
    const { app, elements, flushTimers } = createHarness({
      fetch: async (url) => {
        if (String(url).includes('/diff?') && failFetches) return { ok: false, json: async () => ({}) };
        return {
          ok: true,
          json: async () => (String(url).includes('/diff?')
            ? { path: '/a', kind: 'modify', lang: '', truncated: false, hunks: [{ old_start: 1, new_start: 1, lines: [{ t: 'add', s: 'hello' }] }] }
            : { file_changes: [] })
        };
      }
    });
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 1, dels: 0, seq: 1 });
    await flushTimers();
    await flushTimers();

    const error = elements.diffFileList.querySelector('.diff-error');
    assert(error, 'error note rendered after failed fetch');
    const retry = elements.diffFileList.querySelector('.diff-retry');
    assert(retry, 'retry control rendered');

    failFetches = false;
    await retry.dispatchEvent({ type: 'click' });
    await flushTimers();
    await flushTimers();

    assert(!elements.diffFileList.querySelector('.diff-error'), 'error cleared after successful retry');
    assertEqual(elements.diffFileList.querySelectorAll('.diff-row').length, 1, 'diff rows render after retry');
  });

  await run('very large diffs reveal in chunks with a show-all escape hatch', async () => {
    const manyLines = Array.from({ length: 900 }, (_, i) => ({ t: 'add', s: `line ${i}` }));
    const { app, elements, flushTimers } = createHarness({
      fetch: async (url) => ({
        ok: true,
        json: async () => (String(url).includes('/diff?')
          ? { path: '/big.txt', kind: 'create', lang: '', truncated: false, hunks: [{ old_start: 1, new_start: 1, lines: manyLines }] }
          : { file_changes: [] })
      })
    });
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/big.txt', kind: 'create', adds: 900, dels: 0, seq: 1 });
    await flushTimers();
    await flushTimers();

    assertEqual(elements.diffFileList.querySelectorAll('.diff-row').length, 400, 'first chunk capped at the render limit');
    const more = elements.diffFileList.querySelector('.diff-show-more');
    assertEqual(more.textContent, 'Show 400 more lines', 'chunk control offers the next chunk');
    const all = elements.diffFileList.querySelector('.diff-show-all');
    assertEqual(all.textContent, 'Show all 500 hidden lines', 'show-all control reports the full remainder');

    await more.dispatchEvent({ type: 'click' });
    assertEqual(elements.diffFileList.querySelectorAll('.diff-row').length, 800, 'second chunk revealed');
    assert(!elements.diffFileList.querySelector('.diff-show-all'), 'show-all gone once remainder fits one chunk');

    await elements.diffFileList.querySelector('.diff-show-more').dispatchEvent({ type: 'click' });
    assertEqual(elements.diffFileList.querySelectorAll('.diff-row').length, 900, 'all rows rendered');
    assert(!elements.diffFileList.querySelector('.diff-show-more'), 'controls gone once fully revealed');
  });

  await run('filter input appears for long lists and narrows rows', async () => {
    const { app, elements, flushTimers } = createHarness();
    app.toggleDiffSidebar();
    for (let i = 1; i <= 7; i += 1) {
      app.handleFileChangeEvent({ id: 's1' }, { path: `/src/f${i}.txt`, kind: 'create', adds: 1, dels: 0, seq: i });
    }
    await flushTimers();
    assert(elements.diffFilterRow.hidden, 'filter hidden below the file threshold');

    app.handleFileChangeEvent({ id: 's1' }, { path: '/src/g8.txt', kind: 'create', adds: 1, dels: 0, seq: 8 });
    await flushTimers();
    assert(!elements.diffFilterRow.hidden, 'filter appears once the list is long');

    elements.diffFilterInput.value = 'g8';
    await elements.diffFilterInput.dispatchEvent({ type: 'input', target: elements.diffFilterInput });
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-row').length, 1, 'filter narrows to matching paths');

    elements.diffFilterInput.value = 'no-such-file';
    await elements.diffFilterInput.dispatchEvent({ type: 'input', target: elements.diffFilterInput });
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-row').length, 0, 'no rows for a non-matching filter');
    assert(elements.diffFileList.querySelector('.diff-note'), 'empty state note shown');

    elements.diffFilterInput.value = '';
    await elements.diffFilterInput.dispatchEvent({ type: 'input', target: elements.diffFilterInput });
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-row').length, 8, 'clearing the filter restores all rows');
  });

  await run('adaptive bulk toggle expands, collapses, and sticks', async () => {
    const { app, elements, flushTimers } = createHarness();
    app.toggleDiffSidebar();
    for (let i = 1; i <= 3; i += 1) {
      app.handleFileChangeEvent({ id: 's1' }, { path: `/f${i}`, kind: 'modify', adds: 1, dels: 0, seq: i });
    }
    await flushTimers();

    assertEqual(elements.diffBulkToggleBtn.getAttribute('title'), 'Expand all', 'mixed accordion offers to expand all');
    assertEqual(elements.diffBulkToggleBtn.dataset.action, 'expand', 'expand icon state selected');
    assertEqual(elements.diffBulkToggleBtn.getAttribute('aria-label'), 'Expand all files', 'expand action announced');

    await elements.diffBulkToggleBtn.dispatchEvent({ type: 'click' });
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 3, 'first click opens every body');
    assertEqual(elements.diffBulkToggleBtn.getAttribute('title'), 'Collapse all', 'fully expanded accordion offers to collapse all');
    assertEqual(elements.diffBulkToggleBtn.dataset.action, 'collapse', 'collapse icon state selected');
    assertEqual(elements.diffBulkToggleBtn.getAttribute('aria-label'), 'Collapse all files', 'collapse action announced');

    await elements.diffBulkToggleBtn.dispatchEvent({ type: 'click' });
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 0, 'second click closes every body');
    assertEqual(elements.diffBulkToggleBtn.getAttribute('title'), 'Expand all', 'collapsed accordion returns to expand action');

    app.handleFileChangeEvent({ id: 's1' }, { path: '/f1', kind: 'modify', adds: 2, dels: 0, seq: 4 });
    await flushTimers();
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 0, 'live changes do not undo collapse-all');

    await elements.diffBulkToggleBtn.dispatchEvent({ type: 'click' });
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 3, 'expand action remains available after live updates');
  });

  await run('desktop maximize preserves same DOM, draft focus, scroll anchor, and restores cleanly', async () => {
    const { app, elements, document, flushTimers } = createHarness();
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 1, dels: 0, seq: 1 });
    await flushTimers();
    const body = elements.diffFileList.querySelector('.diff-file-body');
    const spatialRow = elements.diffFileList.querySelector('.diff-file-row');
    const textarea = document.createElement('textarea');
    textarea.value = 'draft survives';
    body.appendChild(textarea);
    textarea.focus();
    elements.diffFileList.scrollTop = 77;
    const beforeChildren = elements.diffFileList.children.slice();

    await elements.diffMaximizeBtn.dispatchEvent({ type: 'click' });
    assert(elements.diffSidebar.classList.contains('maximized') && elements.appShell.classList.contains('diff-maximized'), 'maximize classes missing');
    assert(elements.sidebar.getAttribute('inert') === '' && elements.appMain.getAttribute('inert') === '', 'background was not inert');
    assert(document.activeElement === textarea, 'focus inside Changes moved during maximize');
    assert(textarea.value === 'draft survives' && elements.diffFileList.scrollTop === 77, 'draft or scroll state changed');
    assert(spatialRow.lastScrollIntoView?.block === 'nearest', 'maximize did not restore a reflow-safe spatial anchor');
    assert(beforeChildren.every((child, index) => elements.diffFileList.children[index] === child), 'maximize rebuilt or reparented the list DOM');
    assertEqual(elements.diffMaximizeBtn.getAttribute('aria-label'), 'Restore changes', 'restore action not announced');
    assert(!elements.diffMaximizeBtn.getAttribute('aria-pressed'), 'maximize incorrectly exposed aria-pressed');

    await elements.diffMaximizeBtn.dispatchEvent({ type: 'click' });
    assert(!elements.diffSidebar.classList.contains('maximized') && !elements.appShell.classList.contains('diff-maximized'), 'restore classes remained');
    assert(elements.sidebar.getAttribute('inert') === null && elements.appMain.getAttribute('inert') === null, 'restore left background inert');
    assert(document.activeElement === textarea, 'restore moved panel focus');
  });

  await run('maximize falls back to the captured file path when an open panel anchor disconnects', async () => {
    const { app, elements, document, flushTimers } = createHarness();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 1, dels: 0, seq: 1 });
    app.toggleDiffSidebar();
    await flushTimers();
    const body = elements.diffFileList.querySelector('.diff-file-body');
    const row = elements.diffFileList.querySelector('.diff-file-row');
    const panel = document.createElement('div');
    panel.className = 'diff-comment-panel';
    body.appendChild(panel);
    panel.isConnected = false;
    await elements.diffMaximizeBtn.dispatchEvent({ type: 'click' });
    assert(row.lastScrollIntoView?.block === 'nearest', 'disconnected panel lost its file-path spatial fallback');
  });

  await run('maximize focus, Escape, viewport, session, and close cleanup are layered', async () => {
    const { app, elements, state, document, windowObj, setDrawer } = createHarness();
    app.toggleDiffSidebar();
    const backgroundControl = document.createElement('button');
    elements.appMain.appendChild(backgroundControl);
    backgroundControl.focus();
    await elements.diffMaximizeBtn.dispatchEvent({ type: 'click' });
    assert(document.activeElement === elements.diffMaximizeBtn, 'focus in inerted background did not move to maximize control');
    windowObj.dispatchEvent({ type: 'keydown', key: 'Escape', target: elements.diffMaximizeBtn, preventDefault() { this.defaultPrevented = true; } });
    assert(!elements.diffSidebar.classList.contains('maximized') && document.activeElement === backgroundControl, 'Escape did not restore maximize and prior focus');

    await elements.diffMaximizeBtn.dispatchEvent({ type: 'click' });
    setDrawer(true);
    assert(!elements.diffSidebar.classList.contains('maximized') && elements.appMain.getAttribute('inert') === null, 'drawer breakpoint left maximize residue');
    setDrawer(false);
    app.toggleDiffSidebar();
    await elements.diffMaximizeBtn.dispatchEvent({ type: 'click' });
    app.activateDiffSidebar('s2');
    assert(!elements.diffSidebar.classList.contains('maximized') && elements.sidebar.getAttribute('inert') === null, 'session activation left inert residue');
    state.activeSessionId = 's1';
    app.activateDiffSidebar('s1');
    app.toggleDiffSidebar();
    await elements.diffMaximizeBtn.dispatchEvent({ type: 'click' });
    await elements.diffSidebarCloseBtn.dispatchEvent({ type: 'click' });
    assert(!elements.diffSidebar.classList.contains('maximized') && elements.appMain.getAttribute('inert') === null, 'close left maximize residue');
  });

  await run('escape closes the diff drawer but leaves inputs alone', async () => {
    const { app, elements, windowObj, flushTimers } = createHarness({ drawer: true });
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 1, dels: 0, seq: 1 });
    await flushTimers();
    app.toggleDiffSidebar();
    assert(elements.diffSidebar.classList.contains('open'), 'drawer starts open');

    windowObj.dispatchEvent({ type: 'keydown', key: 'Escape', target: new Element('textarea') });
    assert(elements.diffSidebar.classList.contains('open'), 'escape while typing does not close the drawer');

    windowObj.dispatchEvent({ type: 'keydown', key: 'Escape', target: elements.diffSidebar });
    assert(!elements.diffSidebar.classList.contains('open'), 'escape closes the drawer');
  });

  await run('double-clicking the resize handle resets the stored width', async () => {
    const { app, elements, localStorage } = createHarness();
    localStorage.setItem('term_llm_diff_sidebar_width', '555');
    elements.appShell.style.setProperty('--diff-sidebar-user-width', '555px');

    await elements.diffResizeHandle.dispatchEvent({ type: 'dblclick' });
    assertEqual(localStorage.getItem('term_llm_diff_sidebar_width'), null, 'stored width cleared');
    assertEqual(elements.appShell.style.getPropertyValue('--diff-sidebar-user-width'), '', 'width override removed');
  });

  await run('buildUnifiedDiff reconstructs a patch from cached hunks', () => {
    const { app } = createHarness();
    const patch = app.buildUnifiedDiff('src/main.go', {
      hunks: [{ old_start: 3, new_start: 3, lines: [
        { t: 'ctx', s: 'a' },
        { t: 'del', s: 'old' },
        { t: 'add', s: 'new' }
      ] }]
    });
    const lines = patch.split('\n');
    assertEqual(lines[0], '--- a/src/main.go', 'old file header');
    assertEqual(lines[1], '+++ b/src/main.go', 'new file header');
    assertEqual(lines[2], '@@ -3,2 +3,2 @@', 'hunk header with computed lengths');
    assertEqual(lines[3], ' a', 'context line prefixed with space');
    assertEqual(lines[4], '-old', 'deletion prefixed with minus');
    assertEqual(lines[5], '+new', 'addition prefixed with plus');
  });

  await run('file headers expose copy actions', async () => {
    const { app, elements, flushTimers } = createHarness();
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/work/a.go', kind: 'modify', adds: 1, dels: 0, seq: 1 });
    await flushTimers();

    const actions = elements.diffFileList.querySelector('.diff-file-actions');
    assert(actions, 'actions container rendered in the header');
    assertEqual(actions.querySelectorAll('.diff-action-btn').length, 2, 'copy path and copy diff buttons present');
  });

  await run('canonical server paths inherit live-follow expansion state', async () => {
    const { app, elements } = createHarness({
      fetch: async (url) => ({
        ok: true,
        json: async () => (String(url).includes('/diff?')
          ? { path: '/w/hello.txt', kind: 'modify', lang: '', truncated: false, hunks: [] }
          : { file_changes: [{ path: '/w/hello.txt', kind: 'modify', adds: 1, dels: 0, truncated: false, seq: 5 }] })
      })
    });
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: 'hello.txt', kind: 'modify', adds: 1, dels: 0, seq: 5 });
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 1, 'live row auto-expanded');

    await app.fetchSessionFileChanges('s1');
    const rows = elements.diffFileList.querySelectorAll('.diff-file-row');
    assertEqual(rows.length, 1, 'canonical row replaced the live duplicate');
    assertEqual(rows[0].dataset.path, '/w/hello.txt', 'row carries the canonical path');
    assert(rows[0].classList.contains('expanded'), 'canonical row inherits live-follow expansion');
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 1, 'body stays open across canonicalization');
  });

  await run('a refetch with unchanged seq still refreshes the rendered body', async () => {
    let version = 1;
    const { app, elements, flushTimers } = createHarness({
      fetch: async (url) => ({
        ok: true,
        json: async () => (String(url).includes('/diff?')
          ? { path: '/a', kind: 'modify', lang: '', truncated: false, hunks: [{ old_start: 1, new_start: 1, lines: [{ t: 'add', s: `content v${version}` }] }] }
          : { file_changes: [{ path: '/a', kind: 'modify', adds: 1, dels: 0, truncated: false }] })
      })
    });
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 1, dels: 0, seq: 1 });
    await flushTimers();
    await flushTimers();
    assert(elementText(elements.diffFileList.querySelector('.diff-code')).includes('content v1'), 'initial body rendered');

    // Newer server content under the same local seq (events missed while
    // detached) must still replace the rendered rows.
    version = 2;
    app.refreshFileChangesAfterRun({ id: 's1' });
    await flushTimers();
    await flushTimers();
    assert(elementText(elements.diffFileList.querySelector('.diff-code')).includes('content v2'), 'refetched body replaces stale rows');
  });

  await run('enter on nested action buttons does not toggle the header', async () => {
    const { app, elements, flushTimers } = createHarness();
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 1, dels: 0, seq: 1 });
    await flushTimers();

    const header = elements.diffFileList.querySelector('.diff-file-row');
    const action = header.querySelector('.diff-action-btn');
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 1, 'starts expanded');

    await header.dispatchEvent({ type: 'keydown', key: 'Enter', target: action });
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 1, 'enter on action button leaves the accordion alone');

    await header.dispatchEvent({ type: 'keydown', key: 'Enter', target: header });
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 0, 'enter on the header itself toggles');
  });

  await run('emphasis never splits surrogate pairs', () => {
    const { app } = createHarness();
    const rows = app.computeInlineEmphasis([
      { type: 'del', text: '😀x' },
      { type: 'add', text: '😃x' }
    ]);
    assertEqual(rows[0].emph.join(','), '0,2', 'del mark spans the whole emoji (both UTF-16 units)');
    assertEqual(rows[1].emph.join(','), '0,2', 'add mark spans the whole emoji (both UTF-16 units)');
  });

  await run('copy path action writes through the app clipboard writer', async () => {
    const { app, elements, flushTimers } = createHarness();
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/w/a.go', kind: 'modify', adds: 1, dels: 0, seq: 1 });
    await flushTimers();

    const action = elements.diffFileList.querySelector('.diff-action-btn');
    await action.dispatchEvent({ type: 'click' });
    await flushTimers();
    assertEqual(app.clipboardWrites[0], '/w/a.go', 'copy path uses getClipboardWriter');
  });

  if (failures > 0) {
    console.error(`\n${failures} failure(s)`);
    process.exit(1);
  }
  console.log('\nAll app-diffs tests passed');
})();
