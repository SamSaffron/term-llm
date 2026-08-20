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
       removeAttribute(name) { attrs.delete(name); },
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
  const pinCalls = [];
  const scrollCalls = [];
  const queued = [];
  const modes = new Map();
  let queueSending = false;
  const app = {
    UI_PREFIX: '/ui', state,
    MAX_QUEUED_DIFF_COMMENTS: 20,
    diffCommentSendMode: (id) => modes.get(id) || 'send',
    diffCommentQueueSending: () => queueSending,
    setDiffCommentSendMode(id, mode) { modes.set(id, mode); return mode; },
    queuedDiffComments(id, anchor = null) {
      const items = queued.filter((item) => item.sessionId === id).map((item) => item.comment);
      if (!anchor) return items;
      const key = `${anchor.path}\0${anchor.side}\0${anchor.line}`;
      return items.filter((item) => `${item.path}\0${item.side}\0${item.line}` === key);
    },
    queueDiffComment(id, comment) { queued.push({ sessionId: id, comment }); return true; },
    removeQueuedDiffComment(id, commentId) {
      const index = queued.findIndex((item) => item.sessionId === id && item.comment.id === commentId);
      return index >= 0 ? queued.splice(index, 1)[0].comment : null;
    },
    scrollDiffFileIntoView(pathName) { scrollCalls.push(pathName); },
    pinDiffFileExpanded(sessionId, pathName) { pinCalls.push([sessionId, pathName]); },
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
  return {
    app, state, calls, payloads, pinCalls, scrollCalls, queued, modes, document, window,
    setQueueSending(value) { queueSending = Boolean(value); }
  };
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

  await run('opening and submitting pin the file and restore rebuilt marker focus', async () => {
    const harness = createHarness();
    harness.app.sendMessage = async (options) => { options._onTransportStarted(); };
    const first = decorateRow(harness);
    await first.button.click();
    assert(harness.app.diffCommentPanelOpen('s1'), 'open panel state was not exposed');
    assert(harness.pinCalls.some(([id, pathName]) => id === 's1' && pathName === 'a.go'), 'opening did not pin the anchor file');
    const textarea = first.table.querySelector('textarea');
    textarea.value = 'keep this pinned';
    await textarea.dispatch('input');
    await first.table.querySelector('.diff-comment-send').click();
    const rebuilt = decorateRow(harness);
    assert(harness.document.activeElement === rebuilt.button, 'submit did not restore focus to the rebuilt marker');
    assert(harness.pinCalls.length >= 2, 'submit did not reaffirm the explicit expansion');
  });

  await run('split send mode switches without text and alternate selection acts immediately with text', async () => {
    const harness = createHarness();
    const first = decorateRow(harness);
    await first.button.click();
    const textarea = first.table.querySelector('textarea');
    const more = first.table.querySelector('.diff-comment-send-more');
    await more.click();
    const options = first.table.querySelectorAll('.diff-comment-send-option');
    await options[1].click();
    assert(harness.app.diffCommentSendMode('s1') === 'queue', 'empty alternate selection did not switch default');
    assert(harness.queued.length === 0 && harness.document.activeElement === textarea, 'empty selection acted or lost editor focus');
    assert(first.table.querySelector('.diff-comment-send').textContent === 'Queue comment', 'visible primary did not follow mode');

    textarea.value = 'queue this now';
    await textarea.dispatch('input');
    await more.click();
    await options[1].click();
    assert(harness.queued.length === 1 && harness.queued[0].comment.instruction === 'queue this now', 'non-empty alternate did not act immediately');
    const rebuilt = decorateRow(harness);
    assert(harness.document.activeElement === rebuilt.button, 'queue did not restore focus to rebuilt marker');
    assert(rebuilt.button.classList.contains('queued') && rebuilt.button.getAttribute('aria-label').includes('queued, not sent'), 'queued marker is not visually/accessibly distinct');
  });

  await run('primary Queue comment shortcut follows visible mode and menu Escape closes only menu', async () => {
    const harness = createHarness();
    harness.app.setDiffCommentSendMode('s1', 'queue');
    const decorated = decorateRow(harness);
    await decorated.button.click();
    const textarea = decorated.table.querySelector('textarea');
    const send = decorated.table.querySelector('.diff-comment-send');
    assert(send.textContent === 'Queue comment' && send.title === 'Queue comment (Ctrl/⌘+Enter)', 'queue primary copy/shortcut changed');
    const more = decorated.table.querySelector('.diff-comment-send-more');
    await more.click();
    const menu = decorated.table.querySelector('.diff-comment-send-menu');
    const options = decorated.table.querySelectorAll('.diff-comment-send-option');
    assert(harness.document.activeElement === options[0], 'opening the menu did not focus its first item');
    await menu.dispatch('keydown', { key: 'ArrowDown' });
    assert(harness.document.activeElement === options[1], 'ArrowDown did not move menu focus');
    await menu.dispatch('keydown', { key: 'ArrowUp' });
    assert(harness.document.activeElement === options[0], 'ArrowUp did not wrap menu focus');
    const escape = await menu.dispatch('keydown', { key: 'Escape' });
    assert(menu.hidden && escape.immediateStopped && !decorated.table.querySelector('.diff-comment-panel').removed, 'menu Escape leaked to panel close');
    textarea.value = 'keyboard queued';
    await textarea.dispatch('keydown', { key: 'Enter', metaKey: true });
    await new Promise((resolve) => setTimeout(resolve, 0));
    assert(harness.queued.some((item) => item.comment.instruction === 'keyboard queued'), 'shortcut did not run visible queue primary');
  });

  await run('rejected queue add keeps the editor open, enabled, and focused', async () => {
    const harness = createHarness();
    harness.app.setDiffCommentSendMode('s1', 'queue');
    harness.app.queueDiffComment = () => false;
    const decorated = decorateRow(harness);
    await decorated.button.click();
    const textarea = decorated.table.querySelector('textarea');
    textarea.value = 'keep this draft visible';
    await decorated.table.querySelector('.diff-comment-send').click();
    assert(!decorated.table.querySelector('.diff-comment-panel').removed, 'rejected queue add closed the editor');
    assert(!textarea.disabled && textarea.value === 'keep this draft visible', 'rejected queue add disabled or lost the draft');
    assert(harness.document.activeElement === textarea, 'rejected queue add did not return focus to the draft');
  });

  await run('Send now sends only current comment when a queue already exists', async () => {
    const harness = createHarness();
    harness.app.queueDiffComment('s1', { ...commentEntry('queued', 'a.go', 'wait').diff_comment, created_at: 1 });
    let sent;
    harness.app.sendMessage = async (options) => { sent = options; options._onTransportStarted(); };
    const decorated = decorateRow(harness);
    await decorated.button.click();
    await decorated.table.querySelector('.diff-comment-follow-up').click();
    const textarea = decorated.table.querySelector('textarea');
    textarea.value = 'jump the queue';
    await textarea.dispatch('input');
    await decorated.table.querySelector('.diff-comment-send').click();
    assert(sent.contentParts.length === 1 && sent.contentParts[0].diff_comment.instruction === 'jump the queue', 'Send now auto-attached queued items');
    assert(harness.queued.length === 1, 'Send now mutated unsent queue');
  });

  await run('queued history shows stale status and Edit pins, scrolls, removes, and prefills', async () => {
    const harness = createHarness();
    const queued = { ...commentEntry('queued', 'a.go', 'edit me', 1).diff_comment, created_at: 1 };
    harness.app.queueDiffComment('s1', queued);
    const decorated = decorateRow(harness, { fileChangeSeq: 2 });
    await decorated.button.click();
    assert(decorated.table.querySelector('.diff-comment-history-meta')?.textContent === 'File changed after this was queued', 'stale queued microcopy changed');
    await decorated.table.querySelector('.diff-comment-history-edit').click();
    assert(harness.queued.length === 0, 'Edit did not remove item from queue');
    assert(harness.pinCalls.some(([, pathName]) => pathName === 'a.go') && harness.scrollCalls.includes('a.go'), 'Edit did not pin and restore spatial context');
  });

  await run('queued Edit and Remove remain locked throughout batch send', async () => {
    const harness = createHarness();
    harness.app.queueDiffComment('s1', { ...commentEntry('queued', 'a.go', 'do not mutate', 1).diff_comment, created_at: 1 });
    harness.setQueueSending(true);
    const decorated = decorateRow(harness);
    await decorated.button.click();
    const edit = decorated.table.querySelector('.diff-comment-history-edit');
    const remove = decorated.table.querySelector('.diff-comment-history-remove');
    assert(edit.disabled && remove.disabled, 'sending queue actions were not disabled');
    await edit.click();
    await remove.click();
    assert(harness.queued.length === 1 && harness.pinCalls.length === 1 && harness.scrollCalls.length === 0, 'locked action mutated queue, draft, or spatial state');
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

  await run('branch-context cancellation retains a single draft and retry identity', async () => {
    const harness = createHarness();
    let parked;
    harness.app.sendMessage = async (options) => {
      parked = options;
      options._onTransportStarted({ queued: true });
    };
    const first = decorateRow(harness);
    await first.button.click();
    const textarea = first.table.querySelector('textarea');
    textarea.value = 'retry after branch cancellation';
    await textarea.dispatch('input');
    await first.table.querySelector('.diff-comment-send').click();
    const originalID = parked.reuseMessageId;
    parked._onTransportCanceled({ queued: true, canceled: true });

    const second = decorateRow(harness);
    await second.button.click();
    const retry = second.table.querySelector('textarea');
    assert(retry?.value === 'retry after branch cancellation', 'branch cancellation discarded the inline draft');
    harness.app.sendMessage = async (options) => {
      parked = options;
      options._onTransportStarted();
    };
    await second.table.querySelector('.diff-comment-send').click();
    assert(parked.reuseMessageId === originalID, 'retry created a duplicate failed transcript identity');
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

  await run('textarea Escape stops before global maximize handling', async () => {
    const harness = createHarness();
    const decorated = decorateRow(harness);
    await decorated.button.click();
    const panel = decorated.table.querySelector('.diff-comment-panel');
    const textarea = panel.querySelector('textarea');
    const escape = await textarea.dispatch('keydown', { key: 'Escape' });
    assert(escape.defaultPrevented && escape.immediateStopped && panel.removed, 'textarea Escape leaked past the comment editor');
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

  await run('plural transcript projection renders one summary and every untrusted anchor as text', () => {
    const { app } = createHarness();
    const node = app.createDiffCommentMessageNode({
      id: 'm2', role: 'user', created: 1,
      diffComments: [
        { id: 'c1', path: 'a.go', side: 'new', line: 4, file_change_seq: 8, line_text: '<one>', instruction: 'First' },
        { id: 'c2', path: '<b.go>', side: 'old', line: 9, file_change_seq: 9, line_text: '<two>', instruction: '<Second>' }
      ]
    });
    const body = node.children[0];
    assert(body.children[0].textContent === '2 inline comments', 'plural summary missing');
    assert(body.querySelectorAll('.diff-comment-message-block').length === 2, 'not every diff comment rendered');
    assert(body.querySelectorAll('.diff-comment-message-heading')[1].textContent.includes('<b.go>'), 'plural path was not kept as text');
    assert(body.querySelectorAll('.diff-comment-message-instruction')[1].textContent === '<Second>', 'plural instruction was not kept as text');
  });

  if (failures > 0) process.exit(1);
  console.log('All app-diff-comments tests passed');
})();
