'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const source = fs.readFileSync(path.join(__dirname, 'app-diff-queue.js'), 'utf8');
const commentSource = fs.readFileSync(path.join(__dirname, 'app-diff-comments.js'), 'utf8');
const scopeSource = fs.readFileSync(path.join(__dirname, 'app-diff-scopes.js'), 'utf8');
const indexSource = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
let failures = 0;
const assert = (condition, message) => { if (!condition) throw new Error(message); };
const run = async (name, fn) => {
  try { await fn(); console.log(`PASS ${name}`); }
  catch (error) { failures += 1; console.error(`FAIL ${name}: ${error.stack || error}`); }
};

class ClassList {
  constructor() { this.values = new Set(); }
  add(...names) { names.forEach((name) => this.values.add(name)); }
  remove(...names) { names.forEach((name) => this.values.delete(name)); }
  contains(name) { return this.values.has(name); }
  toggle(name, force) {
    const on = force === undefined ? !this.values.has(name) : Boolean(force);
    if (on) this.values.add(name); else this.values.delete(name);
    return on;
  }
}

const makeElement = (className = '') => {
  const attrs = new Map();
  const listeners = new Map();
  return {
    className, hidden: false, disabled: false, textContent: '', title: '', classList: new ClassList(), children: [], parentNode: null,
    setAttribute(name, value) { attrs.set(name, String(value)); },
    removeAttribute(name) { attrs.delete(name); },
    getAttribute(name) { return attrs.has(name) ? attrs.get(name) : null; },
    addEventListener(type, listener) { listeners.set(type, listener); },
    async dispatch(type) { return listeners.get(type)?.({ type, target: this }); },
    contains(node) { return node === this || this.children.some((child) => child.contains?.(node)); },
    focus() { if (this.ownerDocument) this.ownerDocument.activeElement = this; }
  };
};

const makeBar = () => {
  const bar = makeElement('diff-queue-bar');
  const count = makeElement('diff-queue-count');
  const send = makeElement('diff-queue-send');
  const discard = makeElement('diff-queue-discard');
  const children = { '.diff-queue-count': count, '.diff-queue-send': send, '.diff-queue-discard': discard };
  bar.children = [count, send, discard];
  bar.children.forEach((child) => { child.parentNode = bar; });
  bar.querySelector = (selector) => children[selector] || null;
  return { bar, count, send, discard };
};

const makeComment = (id, instruction = `instruction ${id}`, pathName = 'a.go', line = 2, seq = 1, scope = 'last_turn') => ({
  id, path: pathName, scope, side: 'new', line, file_change_seq: seq, line_text: `line ${line}`,
  context_before: [{ side: 'new', line: line - 1, text: 'before' }],
  context_after: [{ side: 'new', line: line + 1, text: 'after' }], instruction, created_at: seq
});

const createHarness = (options = {}) => {
  const storage = options.storage || new Map();
  const storageAttempts = [];
  const localStorage = {
    getItem(key) { return storage.has(key) ? storage.get(key) : null; },
    setItem(key, value) {
      const serialized = String(value);
      storageAttempts.push(serialized);
      if (options.storageLimit && serialized.length > options.storageLimit) throw new Error('quota');
      storage.set(String(key), serialized);
    },
    removeItem(key) { storage.delete(key); }
  };
  const document = { activeElement: null };
  const { bar, count, send, discard } = makeBar();
  const status = makeElement('diff-queue-status');
  const toggle = makeElement('diff-toggle');
  for (const element of [bar, count, send, discard, status, toggle]) element.ownerDocument = document;
  toggle.title = '2 changed files (+2)';
  toggle.setAttribute('aria-label', 'Toggle file changes: 2 changed files (+2)');
  const state = { activeSessionId: 's1', connected: true, branchContextQueuedSend: null };
  const optimistic = [], removedOptimistic = [], sendCalls = [], toasts = [], revisions = [], renders = [];
  const app = {
    state,
    elements: { diffToggleBtn: toggle },
    STORAGE_KEYS: { diffCommentQueue: 'term_llm_diff_comment_queue', pendingIntents: 'term_llm_pending_intent' },
    isSessionIdentityResolved: (id) => id.startsWith('s'),
    bumpDiffCommentPathRevision(sessionId, pathName) { revisions.push([sessionId, pathName]); },
    renderDiffSidebar(sessionId) { renders.push(sessionId); },
    showToast(message, detail) { toasts.push([message, detail]); },
    async sendMessage(spec) {
      sendCalls.push(spec);
      if (options.sendMessage) return options.sendMessage(spec);
      spec._onTransportStarted?.();
    }
  };
  const documentGetElementById = (id) => ({ diffQueueBar: bar, diffQueueStatus: status }[id] || null);
  document.getElementById = documentGetElementById;
  const context = {
    window: { TermLLMApp: app }, document, localStorage, console, Date, Math, Promise,
    setTimeout: options.setTimeout || setTimeout,
    clearTimeout: options.clearTimeout || clearTimeout,
    globalThis: { crypto: { randomUUID: () => `uuid-${sendCalls.length + 1}` } }
  };
  vm.runInNewContext(scopeSource, context, { filename: 'app-diff-scopes.js' });
  vm.runInNewContext(commentSource, context, { filename: 'app-diff-comments.js' });
  Object.assign(app, {
    addOptimisticDiffComment(sessionId, comment) { optimistic.push([sessionId, comment]); },
    removeOptimisticDiffComment(sessionId, id) { removedOptimistic.push([sessionId, id]); },
    hydrateDiffComments() { return Promise.resolve([]); }
  });
  vm.runInNewContext(source, context, { filename: 'app-diff-queue.js' });
  return { app, state, storage, storageAttempts, document, bar, count, status, send, discard, toggle, optimistic, removedOptimistic, sendCalls, toasts, revisions, renders };
};

(async () => {
  await run('queue status is a persistent visible polite live region outside the hidden action bar', () => {
    const statusIndex = indexSource.indexOf('id="diffQueueStatus"');
    const barIndex = indexSource.indexOf('id="diffQueueBar"');
    assert(statusIndex >= 0 && statusIndex < barIndex, 'persistent queue status is missing or nested in the hidden bar');
    const statusTag = indexSource.slice(indexSource.lastIndexOf('<', statusIndex), indexSource.indexOf('>', statusIndex) + 1);
    assert(statusTag.includes('role="status"') && statusTag.includes('aria-live="polite"') && !statusTag.includes('hidden'), 'queue status lacks persistent polite live semantics');
  });

  await run('queue state is per resolved session, persisted separately, editable, removable, and pruned', () => {
    const first = createHarness();
    assert(!first.app.queueDiffComment('123', makeComment('bad')), 'unresolved session accepted queue state');
    first.app.setDiffCommentSendMode('s1', 'queue');
    assert(first.app.queueDiffComment('s1', makeComment('a1')), 'first queue add failed');
    assert(first.app.queueDiffComment('s2', makeComment('b1', 'second', 'b.go')), 'second session queue add failed');
    assert(first.app.diffCommentSendMode('s1') === 'queue', 'mode did not persist in memory');
    assert(first.app.queuedDiffComments('s1').length === 1 && first.app.queuedDiffComments('s2').length === 1, 'session queues leaked');
    const persisted = JSON.parse(first.storage.get('term_llm_diff_comment_queue'));
    assert(persisted.sessions.s1.items[0].id === 'a1', 'queue was not stored under dedicated key');
    assert(!first.storage.has('term_llm_pending_intent'), 'unsent queue polluted sent-intent storage');

    const reloaded = createHarness({ storage: first.storage });
    assert(reloaded.app.diffCommentSendMode('s1') === 'queue' && reloaded.app.queuedDiffComments('s1')[0].id === 'a1', 'reload did not restore queue and mode');
    assert(reloaded.app.removeQueuedDiffComment('s1', 'a1')?.instruction === 'instruction a1', 'remove did not return queued item for editing');
    reloaded.app.pruneDiffCommentQueues(new Set(['s1']));
    assert(reloaded.app.queuedDiffComments('s2').length === 0, 'prune retained an evicted session queue');
  });

  await run('queue cap is 20 with exact reassuring microcopy and draft-safe rejection', () => {
    const harness = createHarness();
    for (let i = 0; i < 20; i += 1) assert(harness.app.queueDiffComment('s1', makeComment(`c${i}`)), `item ${i} rejected`);
    assert(!harness.app.queueDiffComment('s1', makeComment('overflow')), 'queue accepted item 21');
    assert(harness.app.queuedDiffComments('s1').length === 20, 'cap mutation changed existing queue');
    assert(harness.toasts.at(-1)?.[0] === 'Queue is full (20). Send or discard queued comments first.', 'cap microcopy changed');
  });

  await run('queue bar exposes calm labels, review title, and visible toggle accent', () => {
    const harness = createHarness();
    harness.app.queueDiffComment('s1', makeComment('a1', 'Keep the stable behavior here'));
    harness.app.queueDiffComment('s1', makeComment('a2', 'Avoid rebuilding this node', 'b.go', 8));
    harness.app.renderDiffCommentQueueBar('s1');
    assert(!harness.bar.hidden && harness.count.textContent === '2 queued', 'queue count missing');
    assert(harness.status.textContent === '2 inline comments queued, not sent.', 'persistent live status missing queued state');
    assert(harness.send.textContent === 'Send 2 comments' && harness.discard.textContent === 'Discard', 'queue actions use wrong copy');
    assert(harness.count.title.includes('a.go:2 — Keep the stable') && harness.count.title.includes('b.go:8 — Avoid rebuilding'), 'review title omitted anchors');
    assert(harness.toggle.classList.contains('has-queued'), 'Changes toggle lacks visible queued accent');
    assert(harness.toggle.getAttribute('aria-label').endsWith('· 2 queued comments not sent'), 'toggle queued state not announced');
  });

  await run('one batch sends one message with N parts, shared identity, and resets mode', async () => {
    const harness = createHarness();
    harness.document.activeElement = harness.send;
    harness.app.setDiffCommentSendMode('s1', 'queue');
    harness.app.queueDiffComment('s1', makeComment('a1', 'First'));
    harness.app.queueDiffComment('s1', makeComment('a2', 'Second', 'b.go', 5, 0, 'staged'));
    assert(await harness.app.sendQueuedDiffComments('s1'), 'batch send did not succeed');
    assert(harness.sendCalls.length === 1, 'batch created multiple send calls');
    const call = harness.sendCalls[0];
    assert(call.contentParts.length === 2 && call.diffComments.length === 2, 'batch lost diff comment parts');
    assert(call.contentParts[1].diff_comment.scope === 'staged' && call.contentParts[1].diff_comment.file_change_seq === 0, 'batch lost staged diff scope');
    assert(call.displayPrompt === '2 inline comments' && call.prompt.startsWith('[Inline diff instructions] (2 anchored comments)'), 'plural batch prompt/display changed');
    assert(harness.optimistic.length === 2 && harness.optimistic.every(([, item]) => item.client_message_id === call.reuseMessageId), 'optimistic entries did not share batch identity');
    assert(harness.app.queuedDiffComments('s1').length === 0 && harness.app.diffCommentSendMode('s1') === 'send', 'successful batch did not clear/reset mode');
    assert(harness.document.activeElement === harness.toggle, 'focus was left inside the hidden queue bar');
    assert(harness.status.textContent === 'No inline comments queued.', 'persistent live status did not announce the empty queue');
  });

  await run('N=1 batch prompt and display are identical to direct send', async () => {
    const harness = createHarness();
    const comment = makeComment('only', 'Keep exactly this');
    harness.app.queueDiffComment('s1', comment);
    assert(harness.app.buildDiffCommentBatchPrompt([comment]) === harness.app.formatDiffCommentInstruction(comment), 'single batch added wrapper bytes');
    await harness.app.sendQueuedDiffComments('s1');
    const call = harness.sendCalls[0];
    assert(call.displayPrompt === comment.instruction && call.contentParts.length === 1 && call.contentParts[0].diff_comment.instruction === comment.instruction, 'single batch display/content differs from direct send');
  });

  await run('failure after transport start keeps queue and removes optimistic markers', async () => {
    const harness = createHarness({ sendMessage: async (spec) => { spec._onTransportStarted(); spec._onTransportFailed(new Error('no')); } });
    harness.app.setDiffCommentSendMode('s1', 'queue');
    harness.app.queueDiffComment('s1', makeComment('a1'));
    assert(!await harness.app.sendQueuedDiffComments('s1'), 'failed transport reported success');
    assert(harness.app.queuedDiffComments('s1').length === 1 && harness.app.diffCommentSendMode('s1') === 'queue', 'failure lost queue or mode');
    assert(harness.removedOptimistic.some(([, id]) => id === 'a1'), 'failure left optimistic marker');
    assert(harness.toasts.at(-1)?.[0] === 'Queued comments were not sent: no', 'failure detail was not surfaced');
  });

  await run('branch-context parking and cancellation retain every queued comment', async () => {
    const harness = createHarness({ sendMessage: async (spec) => { spec._onTransportStarted({ queued: true }); } });
    harness.app.setDiffCommentSendMode('s1', 'queue');
    harness.app.queueDiffComment('s1', makeComment('a1'));
    harness.app.queueDiffComment('s1', makeComment('a2', 'second', 'b.go'));
    assert(await harness.app.sendQueuedDiffComments('s1'), 'parked batch was treated as a transport failure');
    assert(harness.app.queuedDiffComments('s1').length === 2 && harness.app.diffCommentQueueSending('s1'), 'parking cleared or unlocked the queued snapshot');
    harness.sendCalls[0]._onTransportCanceled({ queued: true, canceled: true });
    assert(harness.app.queuedDiffComments('s1').length === 2 && !harness.app.diffCommentQueueSending('s1'), 'branch cancellation lost queued comments or left mutation locked');
    assert(harness.app.diffCommentSendMode('s1') === 'queue', 'branch cancellation lost the queued send mode');
  });

  await run('persistence rejects additions it cannot store and rejects oversized items', () => {
    const harness = createHarness({ storageLimit: 500 });
    assert(harness.app.queueDiffComment('s1', makeComment('a1', 'first')), 'first bounded item was rejected');
    assert(!harness.app.queueDiffComment('s1', makeComment('a2', 'second')), 'unstorable item was accepted');
    assert(harness.app.queuedDiffComments('s1').length === 1, 'failed persistence changed the in-memory queue');
    const persisted = JSON.parse(harness.storage.get('term_llm_diff_comment_queue'));
    assert(persisted.sessions.s1.items.length === 1 && persisted.sessions.s1.items[0].id === 'a1', 'failed persistence diverged from the visible queue');
    assert(harness.toasts.some(([message]) => message.includes('could not be saved')), 'storage failure was not explained');
    const huge = makeComment('huge', 'x'.repeat(harness.app.MAX_QUEUED_DIFF_COMMENT_BYTES + 1));
    assert(!harness.app.queueDiffComment('s1', huge), 'oversized queued item bypassed the item bound');
  });

  await run('aggregate byte budget prevents a permanently unsendable batch', () => {
    const harness = createHarness();
    let accepted = 0;
    for (let index = 0; index < 20; index += 1) {
      const comment = makeComment(`large-${index}`, 'i'.repeat(7000), `f${index}.go`, index + 2, index + 1);
      comment.line_text = 'l'.repeat(7000);
      comment.context_before = Array.from({ length: 4 }, (_, offset) => ({ side: 'new', line: offset + 1, text: 'c'.repeat(4000) }));
      if (!harness.app.queueDiffComment('s1', comment)) break;
      accepted += 1;
    }
    assert(accepted > 0 && accepted < 20, 'aggregate limit did not stop the batch before the count cap');
    assert(harness.toasts.at(-1)?.[0].includes('safe batch-size limit'), 'aggregate rejection did not explain how to recover');
    assert(harness.app.MAX_QUEUED_DIFF_COMMENT_AGGREGATE_BYTES < 256 * 1024, 'client aggregate budget does not leave room below the server limit');
  });

  await run('double activation is idempotent while a batch is in flight', async () => {
    let release;
    const harness = createHarness({ sendMessage: (spec) => { spec._onTransportStarted(); return new Promise((resolve) => { release = resolve; }); } });
    harness.app.queueDiffComment('s1', makeComment('a1'));
    const first = harness.app.sendQueuedDiffComments('s1');
    const second = await harness.app.sendQueuedDiffComments('s1');
    assert(second === false && harness.sendCalls.length === 1, 'double activation posted twice');
    release();
    assert(await first, 'first send did not finish');
  });

  await run('discard confirmation timers remain independent per session', () => {
    let nextTimer = 0;
    const canceled = [];
    const harness = createHarness({
      setTimeout() { nextTimer += 1; return nextTimer; },
      clearTimeout(id) { canceled.push(id); }
    });
    harness.app.queueDiffComment('s1', makeComment('a1'));
    harness.app.queueDiffComment('s2', makeComment('b1', 'second', 'b.go'));
    harness.app.discardQueuedDiffComments('s1');
    harness.app.discardQueuedDiffComments('s2');
    assert(nextTimer === 2 && canceled.length === 0, 'arming one session canceled another session timer');
  });

  await run('discard is two-step and resets queue mode only after confirmation', () => {
    const harness = createHarness();
    harness.app.setDiffCommentSendMode('s1', 'queue');
    harness.app.queueDiffComment('s1', makeComment('a1'));
    assert(!harness.app.discardQueuedDiffComments('s1'), 'first discard removed items');
    assert(harness.discard.textContent === 'Discard 1?', 'first discard did not arm exact confirmation');
    harness.document.activeElement = harness.discard;
    assert(harness.app.discardQueuedDiffComments('s1'), 'second discard did not confirm');
    assert(harness.document.activeElement === harness.toggle, 'confirmed discard hid its focused control without moving focus');
    assert(harness.app.queuedDiffComments('s1').length === 0 && harness.app.diffCommentSendMode('s1') === 'send', 'discard-all did not clear/reset mode');
  });

  if (failures > 0) process.exit(1);
  console.log('All app-diff-queue tests passed');
})();
