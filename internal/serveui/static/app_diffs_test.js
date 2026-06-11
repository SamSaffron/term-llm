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
    diffToggleBadge: new Element('span')
  };
  elements.diffSidebar.hidden = true;
  elements.diffToggleBtn.hidden = true;

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
    elements
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

  await run('handleFileChangeEvent reveals sidebar and tracks files', () => {
    const { app, elements } = createHarness();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/work/a.go', kind: 'create', adds: 3, dels: 0, seq: 1 });

    assert(!elements.diffSidebar.hidden, 'sidebar revealed on first change');
    assert(elements.appShell.classList.contains('diff-open'), 'grid opens third column');
    assert(!elements.diffToggleBtn.hidden, 'toggle button visible');
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-row').length, 1, 'one file row rendered');
    assertEqual(elements.diffToggleBadge.textContent, '1', 'badge shows file count');
  });

  await run('stale seq replays are idempotent', () => {
    const { app, elements } = createHarness();
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

  await run('live changes auto-expand files inline (accordion)', async () => {
    const { app, elements, flushTimers } = createHarness();
    app.handleFileChangeEvent({ id: 's1' }, { path: '/a', kind: 'modify', adds: 1, dels: 0, seq: 1 });
    app.handleFileChangeEvent({ id: 's1' }, { path: '/b', kind: 'create', adds: 1, dels: 0, seq: 2 });
    await flushTimers();

    const rows = elements.diffFileList.querySelectorAll('.diff-file-row');
    assertEqual(rows.length, 2, 'both files listed');
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-body').length, 2, 'both changed files auto-expand');
    assert(rows[0].classList.contains('expanded'), 'header carries expanded state');
  });

  await run('explicit collapse sticks while the agent keeps editing', async () => {
    const { app, elements, flushTimers } = createHarness();
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

    // Another session is unaffected by s1's dismissal.
    state.activeSessionId = 's2';
    app.handleFileChangeEvent({ id: 's2' }, { path: '/b', kind: 'create', adds: 1, dels: 0, seq: 1 });
    assert(!elements.diffSidebar.hidden, 'other session still shows its sidebar');

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

  await run('drawer to wide viewport reopens diff as a grid column', async () => {
    const { app, elements, setDrawer, flushTimers } = createHarness({ drawer: true });
    app.handleFileChangeEvent({ id: 's1' }, { path: '/work/a.go', kind: 'create', adds: 3, dels: 0, seq: 1 });
    await flushTimers();

    assert(elements.diffSidebar.hidden, 'narrow drawer stays closed after live change');
    assert(!elements.appShell.classList.contains('diff-open'), 'narrow mode does not add a grid column');

    setDrawer(false);
    assert(!elements.diffSidebar.hidden, 'wide mode reveals the panel');
    assert(!elements.diffSidebar.classList.contains('open'), 'drawer-open class is cleared in wide mode');
    assert(elements.appShell.classList.contains('diff-open'), 'wide mode adds the grid diff column');
  });

  await run('wide to drawer viewport closes column but keeps toggle available', () => {
    const { app, elements, setDrawer } = createHarness();
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
    await elements.diffSidebar.dispatchEvent({ type: 'pointerup', target: elements.diffSidebar, clientX: 215, clientY: 48 });
    assert(!elements.diffSidebar.classList.contains('open'), 'rightward swipe closes drawer');
  });

  await run('drawer vertical scroll gesture does not close the diff drawer', async () => {
    const { app, elements, flushTimers } = createHarness({ drawer: true });
    app.handleFileChangeEvent({ id: 's1' }, { path: '/work/a.go', kind: 'create', adds: 3, dels: 0, seq: 1 });
    await flushTimers();
    app.toggleDiffSidebar();

    await elements.diffSidebar.dispatchEvent({ type: 'pointerdown', target: elements.diffSidebar, clientX: 120, clientY: 40 });
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

    app.handleFileChangeEvent({ id: 's1' }, { path: 'hello_world.txt', kind: 'delete', adds: 0, dels: 3, seq: 1 });
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-row').length, 1, 'one live row rendered');

    await app.fetchSessionFileChanges('s1');
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-row').length, 1, 'server refresh replaced stale live row instead of adding a duplicate');
    assertEqual(elements.diffToggleBadge.textContent, '1', 'badge counts unique files');
  });

  await run('fresh page load activates the sidebar for the restored session', async () => {
    const { elements, flushTimers } = createHarness({
      fetch: async (url) => ({
        ok: true,
        json: async () => (String(url).includes('/diff?')
          ? { path: '/work/a.go', kind: 'create', lang: 'go', truncated: false, hunks: [] }
          : { file_changes: [{ path: '/work/a.go', kind: 'create', adds: 3, dels: 0, truncated: false }] })
      })
    });
    // No switchToSession call — the script itself must activate for the
    // session restored at load (state.activeSessionId = 's1' in the harness).
    await flushTimers();
    assert(!elements.diffSidebar.hidden, 'sidebar visible on fresh load when session has changes');
    assert(!elements.diffToggleBtn.hidden, 'toggle visible on fresh load');
    assertEqual(elements.diffFileList.querySelectorAll('.diff-file-row').length, 1, 'file list populated from server');
  });

  if (failures > 0) {
    console.error(`\n${failures} failure(s)`);
    process.exit(1);
  }
  console.log('\nAll app-diffs tests passed');
})();
