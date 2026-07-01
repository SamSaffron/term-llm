#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const source = fs.readFileSync(path.join(__dirname, 'app-diffs.js'), 'utf8');
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
  getBoundingClientRect() { return { width: 320, height: 600, top: 0, left: 0, right: 320, bottom: 600 }; }
  setPointerCapture() {}
  releasePointerCapture() {}
}

function createHarness(options = {}) {
  const elements = {
    appShell: new Element('div'),
    diffSidebar: new Element('aside'),
    diffSidebarTotals: new Element('span'),
    diffSidebarCloseBtn: new Element('button'),
    diffResizeHandle: new Element('div'),
    diffFileList: new Element('div'),
    diffToggleBtn: new Element('button'),
    diffToggleBadge: new Element('span'),
    diffExpandAllBtn: new Element('button'),
    diffCollapseAllBtn: new Element('button'),
    diffFilterRow: new Element('div'),
    diffFilterInput: new Element('input')
  };
  elements.diffSidebar.hidden = true;
  elements.diffToggleBtn.hidden = true;
  elements.diffFilterRow.hidden = true;

  const state = {
    token: '',
    activeSessionId: 's1'
  };

  const storage = new Map();
  const localStorage = {
    getItem: (key) => (storage.has(key) ? storage.get(key) : null),
    setItem: (key, value) => storage.set(key, String(value)),
    removeItem: (key) => storage.delete(key)
  };

  const timers = [];
  const fetchCalls = [];
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

  const app = {
    UI_PREFIX: '/chat',
    STORAGE_KEYS: { diffSidebarWidth: 'term_llm_diff_sidebar_width' },
    state,
    elements,
    clipboardWrites: [],
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
  document.createElement = (tag) => new Element(tag);

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
  vm.runInNewContext(source, context, { filename: 'app-diffs.js' });

  const flushTimers = async () => {
    const pending = timers.splice(0, timers.length);
    for (const timer of pending) {
      if (!timer.cancelled) timer.fn();
    }
    await new Promise((resolve) => setImmediate(resolve));
  };

  return { app, elements, state, localStorage, storage, timers, fetchCalls, flushTimers, setDrawer, media, windowObj: context.window };
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

  await run('stale seq replays are idempotent', () => {
    const { app, elements } = createHarness();
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 5, dels: 2, seq: 7 });
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 1, dels: 1, seq: 3 });

    const counts = elements.diffFileList.querySelector('.diff-count-add');
    assertEqual(counts.textContent, '+5', 'older replayed event did not overwrite newer state');
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

  await run('toggle stays hidden for sessions without file changes', async () => {
    const { app, elements, state, flushTimers } = createHarness();
    assert(elements.diffToggleBtn.hidden, 'toggle hidden on load');

    // Activating a session whose server list is empty must not reveal anything.
    app.activateDiffSidebar('s1');
    await flushTimers();
    assert(elements.diffToggleBtn.hidden, 'toggle hidden after empty list fetch');
    assert(elements.diffSidebar.hidden, 'sidebar hidden after empty list fetch');

    // Switching away from a session with changes to one without hides both.
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 1, dels: 0, seq: 1 });
    assert(!elements.diffToggleBtn.hidden, 'toggle visible once a file changed');
    state.activeSessionId = 'clean';
    app.activateDiffSidebar('clean');
    assert(elements.diffToggleBtn.hidden, 'toggle hidden again on changeless session');
    assert(elements.diffSidebar.hidden, 'sidebar hidden again on changeless session');
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

  await run('activateDiffSidebar fetches the server list once', async () => {
    const { app, state, fetchCalls, flushTimers } = createHarness();
    state.activeSessionId = 's2';
    app.activateDiffSidebar('s2');
    await flushTimers();
    app.activateDiffSidebar('s2');
    await flushTimers();
    assertEqual(fetchCalls.filter((u) => u.includes('s2') && u.endsWith('/file-changes')).length, 1, 'list fetched once per session');
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

  await run('fresh page load keeps restored session diff sidebar closed', async () => {
    const { app, elements, flushTimers } = createHarness({
      fetch: async (url) => ({
        ok: true,
        json: async () => (String(url).includes('/diff?')
          ? { path: '/work/a.go', kind: 'create', lang: 'go', truncated: false, hunks: [] }
          : { file_changes: [{ path: '/work/a.go', kind: 'create', adds: 3, dels: 0, truncated: false }] })
      })
    });
    // No switchToSession call — the script itself activates for the restored
    // session, but page load must not open the diff panel without a click.
    await flushTimers();
    assert(elements.diffSidebar.hidden, 'sidebar remains closed on fresh load when session has changes');
    assert(!elements.diffToggleBtn.hidden, 'toggle visible on fresh load');
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-row').length, 0, 'file list not populated while hidden');

    app.toggleDiffSidebar();
    assert(!elements.diffSidebar.hidden, 'explicit toggle opens restored session sidebar');
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-row').length, 1, 'file list populated after user opens');
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

  await run('expand-all and collapse-all act on every file and stick', async () => {
    const { app, elements, flushTimers } = createHarness();
    app.toggleDiffSidebar();
    for (let i = 1; i <= 3; i += 1) {
      app.handleFileChangeEvent({ id: 's1' }, { path: `/f${i}`, kind: 'modify', adds: 1, dels: 0, seq: i });
    }
    await flushTimers();

    await elements.diffCollapseAllBtn.dispatchEvent({ type: 'click' });
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 0, 'collapse-all closes every body');

    app.handleFileChangeEvent({ id: 's1' }, { path: '/f1', kind: 'modify', adds: 2, dels: 0, seq: 4 });
    await flushTimers();
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 0, 'live changes do not undo collapse-all');

    await elements.diffExpandAllBtn.dispatchEvent({ type: 'click' });
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 3, 'expand-all opens every body');
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

  await run('live count updates patch the existing header instead of rebuilding', async () => {
    const { app, elements, flushTimers } = createHarness();
    app.toggleDiffSidebar();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 1, dels: 0, seq: 1 });
    await flushTimers();

    const before = elements.diffFileList.querySelectorAll('.diff-file-row')[0];
    let listRebuilds = 0;
    const original = elements.diffFileList.replaceChildren.bind(elements.diffFileList);
    elements.diffFileList.replaceChildren = (...nodes) => {
      listRebuilds += 1;
      return original(...nodes);
    };

    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 7, dels: 2, seq: 2 });
    await flushTimers();

    const after = elements.diffFileList.querySelectorAll('.diff-file-row')[0];
    assert(before === after, 'header node identity preserved across live updates');
    assertEqual(listRebuilds, 0, 'list container not rebuilt for an in-place update');
    assertEqual(elements.diffFileList.querySelector('.diff-count-add').textContent, '+7', 'counts updated in place');
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
