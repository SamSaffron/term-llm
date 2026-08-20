'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const source = fs.readFileSync(path.join(__dirname, 'app-diff-comments.js'), 'utf8');
let failures = 0;
const assert = (condition, message) => { if (!condition) throw new Error(message); };
const run = async (name, fn) => {
  try { await fn(); console.log(`PASS ${name}`); }
  catch (error) { failures += 1; console.error(`FAIL ${name}: ${error.stack || error}`); }
};

const createDOM = () => {
  const document = { activeElement: null };
  const matches = (element, selector) => {
    if (!element) return false;
    if (selector.startsWith('#')) return element.id === selector.slice(1);
    const dataMatch = selector.match(/^\.([\w-]+)\[data-commentable="true"\]$/);
    if (dataMatch) return String(element.className).split(/\s+/).includes(dataMatch[1]) && element.dataset.commentable === 'true';
    if (selector.startsWith('.')) return String(element.className).split(/\s+/).includes(selector.slice(1));
    return element.tagName === selector.toUpperCase();
  };
  const descendants = (element) => element.children.flatMap((child) => [child, ...descendants(child)]);
  document.createElement = (tagName) => {
    const listeners = new Map();
    const attrs = new Map();
    const element = {
      tagName: String(tagName || '').toUpperCase(),
      children: [], dataset: {}, className: '', textContent: '', value: '', id: '', tabIndex: 0,
      parentNode: null, removed: false, disabled: false,
      classList: {
        add(...names) {
          const values = new Set(String(element.className).split(/\s+/).filter(Boolean));
          names.forEach((name) => values.add(name));
          element.className = Array.from(values).join(' ');
        },
        contains(name) { return String(element.className).split(/\s+/).includes(name); }
      },
      append(...nodes) { nodes.forEach((node) => this.appendChild(node)); },
      appendChild(node) {
        if (node.parentNode) node.remove();
        node.parentNode = this; node.removed = false; this.children.push(node); return node;
      },
      insertBefore(node, reference) {
        if (node.parentNode) node.remove();
        const index = reference ? this.children.indexOf(reference) : -1;
        node.parentNode = this; node.removed = false;
        if (index < 0) this.children.push(node); else this.children.splice(index, 0, node);
        return node;
      },
      setAttribute(name, value) {
        attrs.set(name, String(value));
        if (name === 'id') this.id = String(value);
        if (name === 'tabindex') this.tabIndex = Number(value);
      },
      getAttribute(name) { return attrs.has(name) ? attrs.get(name) : null; },
      addEventListener(type, listener) {
        if (!listeners.has(type)) listeners.set(type, []);
        listeners.get(type).push(listener);
      },
      async dispatch(type, init = {}) {
        const event = {
          type, target: this, key: '', metaKey: false, ctrlKey: false,
          defaultPrevented: false, immediateStopped: false,
          preventDefault() { this.defaultPrevented = true; },
          stopPropagation() {}, stopImmediatePropagation() { this.immediateStopped = true; },
          ...init
        };
        for (const listener of listeners.get(type) || []) {
          await listener(event);
          if (event.immediateStopped) break;
        }
        return event;
      },
      click() { return this.dispatch('click'); },
      focus() { document.activeElement = this; },
      remove() {
        if (this.parentNode) {
          const index = this.parentNode.children.indexOf(this);
          if (index >= 0) this.parentNode.children.splice(index, 1);
        }
        this.parentNode = null; this.removed = true;
      },
      querySelector(selector) { return descendants(this).find((node) => matches(node, selector)) || null; },
      querySelectorAll(selector) { return descendants(this).filter((node) => matches(node, selector)); },
      contains(node) { return node === this || descendants(this).includes(node); },
      closest(selector) {
        let current = this;
        while (current) { if (matches(current, selector)) return current; current = current.parentNode; }
        return null;
      }
    };
    Object.defineProperty(element, 'nextSibling', {
      get() {
        if (!this.parentNode) return null;
        const index = this.parentNode.children.indexOf(this);
        return this.parentNode.children[index + 1] || null;
      }
    });
    return element;
  };
  document.querySelector = () => null;
  return document;
};

const createHarness = (initialPayloads = {}) => {
  const calls = [];
  const payloads = { ...initialPayloads };
  const document = createDOM();
  const windowListeners = new Map();
  const state = {
    activeSessionId: 's1', token: '', connected: true, sessions: [{ id: 's1', transcriptRev: 1 }],
    pendingInterjections: [], pendingInterruptCommits: [], queuedInterrupts: [], branchContextQueuedSend: null
  };
  const app = {
    UI_PREFIX: '/ui', state,
    requestHeaders: (id) => ({ 'X-Session': id }),
    apiFetch: async (url) => {
      calls.push(url);
      const sessionId = decodeURIComponent(url.split('/sessions/')[1].split('/')[0]);
      const value = typeof payloads[sessionId] === 'function' ? payloads[sessionId]() : payloads[sessionId];
      const payload = Array.isArray(value) ? { comments: value, transcript_rev: 1 } : (value || { comments: [], transcript_rev: 1 });
      return { ok: true, json: async () => payload };
    },
    renderDiffSidebar() {}, showToast() {}
  };
  const window = {
    TermLLMApp: app,
    addEventListener(type, listener) {
      if (!windowListeners.has(type)) windowListeners.set(type, []);
      windowListeners.get(type).push(listener);
    },
    async dispatch(type, init = {}) {
      const event = {
        type, key: '', defaultPrevented: false, immediateStopped: false,
        preventDefault() { this.defaultPrevented = true; },
        stopImmediatePropagation() { this.immediateStopped = true; },
        ...init
      };
      for (const listener of windowListeners.get(type) || []) {
        await listener(event);
        if (event.immediateStopped) break;
      }
      return event;
    }
  };
  const context = {
    window, document, globalThis: {}, console, Date, Math, Promise,
    encodeURIComponent, decodeURIComponent, setTimeout, clearTimeout
  };
  vm.runInNewContext(source, context, { filename: 'app-diff-comments.js' });
  return { app, state, calls, payloads, document, window };
};

const commentEntry = (id, pathName, instruction, revision = 1) => ({ created_at: revision, client_message_id: id, diff_comment: {
  id, path: pathName, side: 'new', line: 2, file_change_seq: revision,
  line_text: 'x', instruction
} });

const decorateRow = (harness, options = {}) => {
  const table = options.table || harness.document.createElement('div');
  const rowElement = harness.document.createElement('div');
  rowElement.className = 'diff-row add';
  const row = options.row || { type: 'add', oldNo: 0, newNo: 2, text: 'x' };
  const rows = options.rows || [row];
  const restore = harness.app.decorateDiffCommentRow({
    sessionId: options.sessionId || 's1', path: options.path || 'a.go', row, rows,
    rowIndex: options.rowIndex || 0, rowElement, fileChangeSeq: options.fileChangeSeq || 1
  });
  table.appendChild(rowElement);
  restore?.();
  return { table, rowElement, button: rowElement.querySelector('.diff-comment-affordance') };
};

(async () => {
  await run('captures deleted-line context without crossing hunks', () => {
    const { app } = createHarness();
    const rows = [
      { type: 'ctx', oldNo: 10, newNo: 10, text: 'before two' },
      { type: 'ctx', oldNo: 11, newNo: 11, text: 'before one' },
      { type: 'del', oldNo: 12, newNo: 0, text: 'removed' },
      { type: 'add', oldNo: 0, newNo: 12, text: 'replacement' },
      { type: 'hunk' },
      { type: 'ctx', oldNo: 30, newNo: 30, text: 'other hunk' }
    ];
    const context = app.captureDiffCommentContext(rows, 2);
    assert(context.before.length === 2 && context.before[0].line === 10, 'missing before context');
    assert(context.after.length === 1 && context.after[0].side === 'new' && context.after[0].line === 12, 'context crossed hunk or lost added line');
  });

  await run('agent instruction includes complete captured anchor and exact text', () => {
    const { app } = createHarness();
    const text = app.formatDiffCommentInstruction({
      path: 'internal/a.go', side: 'old', line: 12, file_change_seq: 99, line_text: '<exact & old>',
      context_before: [{ side: 'old', line: 11, text: 'before' }],
      context_after: [{ side: 'new', line: 12, text: 'after' }], instruction: 'Keep this behavior.'
    });
    for (const expected of ['Path: internal/a.go', 'Side: old', 'Line: 12', 'Captured file-change seq: 99', '> old 12 | <exact & old>', 'Instruction:\nKeep this behavior.']) {
      assert(text.includes(expected), `missing ${expected}`);
    }
  });

  await run('hydrates by transcript revision and isolates per-path revisions', async () => {
    const first = { comments: [commentEntry('a1', 'a.go', 'A'), commentEntry('b1', 'b.go', 'B')], transcript_rev: 1 };
    const { app, state, calls, payloads } = createHarness({ s1: first, s2: { comments: [], transcript_rev: 1 } });
    state.sessions.push({ id: 's2', transcriptRev: 1 });
    await app.hydrateDiffComments('s1', { revision: 1 });
    const aRevision = app.diffCommentRevision('s1', 'a.go');
    const bRevision = app.diffCommentRevision('s1', 'b.go');
    await app.hydrateDiffComments('s1', { revision: 1 });
    payloads.s1 = { comments: [commentEntry('a1', 'a.go', 'A changed', 2), commentEntry('b1', 'b.go', 'B')], transcript_rev: 2 };
    await app.hydrateDiffComments('s1', { revision: 2 });
    await app.hydrateDiffComments('s2', { revision: 1 });
    assert(calls.length === 3, `expected revision-aware requests, got ${calls.length}`);
    assert(app.diffCommentRevision('s1', 'a.go') > aRevision, 'file A revision did not advance');
    assert(app.diffCommentRevision('s1', 'b.go') === bRevision, 'file A update rebuilt file B');
    assert(app.diffCommentRevision('s2', 'a.go') === 0, 'comments leaked between sessions');
    app.pruneDiffCommentState(new Set(['s2']));
    assert(app.diffCommentRevision('s1', 'a.go') === 0, 'evicted session retained comment state');
  });

  await run('open editor draft survives a diff body rebuild', async () => {
    const harness = createHarness();
    const first = decorateRow(harness);
    await first.button.click();
    const textarea = first.table.querySelector('textarea');
    textarea.value = 'typed but not sent';
    await textarea.dispatch('input');
    first.table.remove();

    const second = decorateRow(harness);
    const restored = second.table.querySelector('textarea');
    assert(restored?.value === 'typed but not sent', 'rerender discarded open editor draft');
    assert(second.button.getAttribute('aria-expanded') === 'true', 'restored trigger lost expanded state');
  });

  await run('early send return removes optimistic instruction and keeps retry draft', async () => {
    const harness = createHarness();
    harness.app.sendMessage = async () => undefined;
    const first = decorateRow(harness);
    await first.button.click();
    const textarea = first.table.querySelector('textarea');
    textarea.value = 'retry me';
    await textarea.dispatch('input');
    await first.table.querySelector('.diff-comment-send').click();

    const second = decorateRow(harness);
    assert(!second.button.classList.contains('has-comments'), 'unsent optimistic instruction remained visible forever');
    await second.button.click();
    assert(second.table.querySelector('textarea')?.value === 'retry me', 'failed send discarded retry draft');
  });

  await run('server hydration reconciles an accepted optimistic instruction', async () => {
    const harness = createHarness();
    let submittedID = '';
    harness.app.sendMessage = async (options) => {
      submittedID = options.diffComment.id;
      options._onTransportStarted();
      harness.payloads.s1 = { comments: [commentEntry(submittedID, 'a.go', 'accepted')], transcript_rev: 2 };
    };
    const first = decorateRow(harness);
    await first.button.click();
    const textarea = first.table.querySelector('textarea');
    textarea.value = 'accepted';
    await textarea.dispatch('input');
    await first.table.querySelector('.diff-comment-send').click();
    await new Promise((resolve) => setTimeout(resolve, 0));

    const second = decorateRow(harness, { fileChangeSeq: 2 });
    assert(second.button.classList.contains('has-comments'), 'accepted server comment disappeared during reconciliation');
    await second.button.click();
    const history = second.table.querySelector('.diff-comment-history-text');
    assert(history?.textContent === 'accepted', 'server entry did not replace optimistic instruction');
  });

  await run('Escape only closes a focused comment panel', async () => {
    const harness = createHarness();
    const decorated = decorateRow(harness);
    await decorated.button.click();
    const panel = decorated.table.querySelector('.diff-comment-panel');
    const outside = harness.document.createElement('button');
    outside.focus();
    const drawerEscape = await harness.window.dispatch('keydown', { key: 'Escape' });
    assert(!drawerEscape.defaultPrevented && !panel.removed, 'comment handler swallowed drawer Escape');
    panel.querySelector('textarea').focus();
    const panelEscape = await harness.window.dispatch('keydown', { key: 'Escape' });
    assert(panelEscape.defaultPrevented && panel.removed, 'focused comment panel did not handle Escape');
  });

  await run('diff rows provide one keyboard entry point with panel semantics', async () => {
    const harness = createHarness();
    const table = harness.document.createElement('div');
    const rows = [
      { type: 'add', oldNo: 0, newNo: 1, text: 'one' },
      { type: 'add', oldNo: 0, newNo: 2, text: 'two' }
    ];
    const first = decorateRow(harness, { table, rows, row: rows[0], rowIndex: 0 });
    const second = decorateRow(harness, { table, rows, row: rows[1], rowIndex: 1 });
    assert(first.rowElement.tabIndex === 0 && second.rowElement.tabIndex === -1, 'rows did not use roving tabindex');
    assert(first.button.tabIndex === -1, 'hidden affordance remained a tab stop');
    assert(Boolean(first.button.getAttribute('aria-controls')) && first.button.getAttribute('aria-expanded') === 'false', 'trigger lacks disclosure semantics');
    await first.rowElement.dispatch('keydown', { key: 'Enter' });
    const panel = table.querySelector('.diff-comment-panel');
    assert(panel?.getAttribute('role') === 'region' && first.button.getAttribute('aria-expanded') === 'true', 'keyboard entry did not open a semantic panel');
  });

  await run('transcript projection writes untrusted values through textContent', () => {
    const { app } = createHarness();
    const node = app.createDiffCommentMessageNode({
      id: 'm1', role: 'user', created: 1,
      diffComment: { id: 'c1', path: '<img src=x>', side: 'new', line: 4, file_change_seq: 8, line_text: '<script>x</script>', instruction: '<b>do not parse</b>' }
    });
    const body = node.children[0];
    assert(body.children[0].textContent.includes('<img src=x>'), 'path was not kept as text');
    assert(body.children[1].textContent === '<b>do not parse</b>', 'instruction was not kept as text');
    assert(body.children[2].textContent.includes('<script>x</script>'), 'line was not kept as text');
  });

  if (failures > 0) process.exit(1);
  console.log('All app-diff-comments tests passed');
})();
