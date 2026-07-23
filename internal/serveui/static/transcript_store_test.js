'use strict';

const assert = require('assert');
const {
  TranscriptStore,
  TRANSCRIPT_FLAG_EMPTY_BODY,
  transcriptStoreFromMessages,
  __transcriptStats
} = require('./transcript-store.js');

const envelope = (ids, options = {}) => ({
  rev: options.rev ?? 1,
  compaction_seq: options.compactionSeq ?? -1,
  compaction_count: options.compactionCount ?? 0,
  rows: {
    ids: ids.slice(),
    seqs: options.seqs || ids.map((_, index) => index),
    roles: options.roles || 'u'.repeat(ids.length),
    flags: options.flags || ids.map(() => 0)
  }
});

const body = (id, sequence, role = 'user', parts = null) => ({
  id,
  sequence,
  role,
  parts: parts || [{ type: 'text', text: `${role}-${id}` }]
});

const materializeOrdinals = (store, ordinals, estHeight = 20) => {
  for (const ordinal of ordinals) {
    const segmentIndex = store.segmentForOrdinal(ordinal);
    const segment = store.segments[segmentIndex];
    const messages = [];
    for (let i = segment.startOrdinal; i <= segment.endOrdinal; i += 1) {
      if ((store.flags[i] & TRANSCRIPT_FLAG_EMPTY_BODY) === 0) {
        messages.push(body(store.ids[i], store.seqs[i], ({ u: 'user', a: 'assistant', t: 'tool', e: 'event' })[store.roles[i]]));
      }
    }
    store.materializeSegment(segmentIndex, messages, estHeight);
  }
};

(() => {
  const store = new TranscriptStore('rollback', { maxMaterializedTurns: 10, overscanTurns: 0 });
  store.applyIndex(envelope([1, 2], { rev: 9, roles: 'ua' }), 'etag-newer');
  materializeOrdinals(store, [0]);
  assert.equal(store.bodies.size, 2);
  const result = store.applyIndex(envelope([1, 2], { rev: 3, roles: 'ua' }), 'etag-restored');
  assert.equal(result.rollback, true, 'lower server revision must be reported as a restore');
  assert.equal(store.rev, 3, 'restored revision must replace, not be masked by, the newer revision');
  assert.equal(store.bodies.size, 0, 'durable IDs can be reused by a restore, so cached bodies must be retired');
  assert.equal(store.etag, 'etag-restored');
})();

(() => {
  const store = new TranscriptStore('all-empty', { maxMaterializedTurns: 10, overscanTurns: 0 });
  store.applyIndex(envelope([1, 2], { rev: 1, roles: 'ue', flags: [TRANSCRIPT_FLAG_EMPTY_BODY, TRANSCRIPT_FLAG_EMPTY_BODY] }));
  assert.equal(store.segments.length, 1);
  assert.equal(store.segments[0].state, 'empty', 'all-empty turn should settle without a fetch spacer');
  assert.deepEqual(store.requiredBodyIDs(0), []);
  assert.equal(store.renderRuns().some((run) => run.type === 'gap'), false, 'all-empty turn must not render an inert gap');
  assert.deepEqual(store.renderedMessages(), []);
  store._checkInvariants();
})();

(() => {
  const store = new TranscriptStore('diffs', { maxMaterializedTurns: 20, overscanTurns: 0 });
  let result = store.applyIndex(envelope([1, 2], { rev: 1, roles: 'ua' }), 'etag-1');
  assert.equal(result.appendOnly, true);
  materializeOrdinals(store, [0]);
  result = store.applyIndex(envelope([1, 2, 3], { rev: 2, roles: 'uau' }), 'etag-2');
  assert.equal(result.appendOnly, true);
  assert.equal(store.bodies.has(1), true);
  result = store.applyIndex(envelope([1, 9, 3], { rev: 3, roles: 'uau' }));
  assert.equal(result.appendOnly, false);
  assert.equal(result.divergence, 1);
  assert.equal(store.bodies.has(2), false);
  result = store.applyIndex(envelope([1], { rev: 4, roles: 'u', compactionCount: 1, compactionSeq: 7 }));
  assert.equal(result.appendOnly, false);
  assert.equal(store.ids.length, 1);
  assert.equal(store.compactionCount, 1);
  store.noteIndexFetch(true, 'etag-4');
  assert.equal(store.etag, 'etag-4');
  assert.equal(store.stats.indexFetches, 1);
  assert.equal(store.stats.rewrites, 2);
  store._checkInvariants();
})();

(() => {
  const ids = Array.from({ length: 200 }, (_, index) => index + 1);
  const store = new TranscriptStore('explicit-gap', { maxMaterializedTurns: 250, overscanTurns: 0 });
  store.applyIndex(envelope(ids));
  materializeOrdinals(store, [
    ...Array.from({ length: 50 }, (_, index) => index),
    ...Array.from({ length: 51 }, (_, index) => index + 149)
  ]);
  const gaps = store.renderRuns().filter((run) => run.type === 'gap');
  assert.equal(gaps.length, 1, 'rows 1-50 and 150-200 must have one explicit interior gap');
  assert.deepEqual([gaps[0].startOrdinal + 1, gaps[0].endOrdinal + 1], [51, 149]);
  assert.deepEqual([gaps[0].startSegmentIndex, gaps[0].endSegmentIndex], [50, 148]);
  assert.equal(Object.hasOwn(gaps[0], 'segmentIndexes'), false, 'coalesced gaps must not retain per-segment lists');
  const rendered = store.renderedMessages();
  const gapIndex = rendered.findIndex((entry) => entry.transcriptGap);
  assert(gapIndex > 0 && gapIndex < rendered.length - 1, 'loaded ranges must never appear adjacent across unloaded rows');
  assert.equal(rendered[gapIndex - 1].id, 50);
  assert.equal(rendered[gapIndex + 1].id, 150);
  store._checkInvariants();
})();

(() => {
  const ids = Array.from({ length: 40 }, (_, index) => index + 1);
  const store = new TranscriptStore('row-22-anchor', { maxMaterializedTurns: 50, overscanTurns: 1 });
  store.applyIndex(envelope(ids));
  materializeOrdinals(store, ids.map((_, index) => index), 20);
  store.setViewport(21, 21);
  let scrollTop = 320;
  let currentStore = store;
  const absoluteTop = (id) => {
    const ordinal = currentStore.ordinalForID(id);
    if (ordinal < 0) return NaN;
    let top = 0;
    for (const run of currentStore.renderRuns()) {
      if (ordinal < run.startOrdinal) break;
      if (run.type === 'gap') {
        if (ordinal <= run.endOrdinal) return top;
        top += run.height;
      } else {
        const segment = currentStore.segments[run.segmentIndex];
        if (ordinal <= run.endOrdinal) return top + (ordinal - run.startOrdinal) * (segment.estHeight / (run.endOrdinal - run.startOrdinal + 1));
        top += segment.estHeight;
      }
    }
    return top;
  };
  const adapter = {
    capture: () => ({ id: 22, top: absoluteTop(22) - scrollTop }),
    render: (next) => { currentStore = next; },
    topForID: (id) => absoluteTop(id) - scrollTop,
    adjustScroll: (delta) => { scrollTop += delta; }
  };
  const assertAnchor = (label, mutate) => {
    const before = adapter.capture().top;
    store.withViewportAnchor(adapter, mutate);
    const after = absoluteTop(22) - scrollTop;
    assert(Math.abs(after - before) <= 1, `${label} moved row 22: before=${before} after=${after}`);
    assert(store.pinnedSegments.has(store.segmentForID(22)), `${label} evicted visible row 22`);
  };

  assertAnchor('append', () => store.applyIndex(envelope([...ids, 41, 42], { rev: 2 })));
  store.evictSegment(store.segmentForID(10));
  store.segments[store.segmentForID(10)].estHeight = 55;
  assertAnchor('interior gap fill', () => materializeOrdinals(store, [9], 20));
  assertAnchor('eviction', () => store.evictSegment(store.segmentForID(5), 35));
  const rewritten = [1001, 1002, ...store.ids];
  assertAnchor('rewrite', () => store.applyIndex(envelope(rewritten, { rev: 3 })));
  store._checkInvariants();
})();

(() => {
  const ids = Array.from({ length: 120 }, (_, index) => index + 1);
  const store = new TranscriptStore('random-budget', { maxMaterializedTurns: 12, overscanTurns: 2 });
  store.applyIndex(envelope(ids));
  let seed = 123456789;
  const random = () => {
    seed = (1103515245 * seed + 12345) & 0x7fffffff;
    return seed / 0x80000000;
  };
  for (let step = 0; step < 300; step += 1) {
    const ordinal = Math.floor(random() * ids.length);
    if (random() < 0.7) materializeOrdinals(store, [ordinal]);
    const first = Math.floor(random() * ids.length);
    store.setViewport(first, Math.min(ids.length - 1, first + Math.floor(random() * 3)));
    store.enforceBudget();
    store._checkInvariants();
  }
  const materialized = store.segments.filter((segment) => segment.state === 'materialized').length;
  const pinnedMaterialized = [...store.pinnedSegments].filter((index) => store.segments[index]?.state === 'materialized').length;
  assert(materialized <= Math.max(12, pinnedMaterialized));
  assert(store.bodies.size <= materialized, 'one-row turns must bound bodies with turns');
  assert(store.renderRuns().length <= materialized + store.segments.length, 'DOM descriptors must remain sparse');
})();

(() => {
  const ids = Array.from({ length: 5000 }, (_, index) => index + 1);
  const store = new TranscriptStore('five-thousand-row-budget', { maxMaterializedTurns: 60, overscanTurns: 8 });
  store.applyIndex(envelope(ids));
  store.setViewport(4999, 4999);
  materializeOrdinals(store, Array.from({ length: 100 }, (_, index) => 4900 + index));
  store.enforceBudget();
  assert.equal(store.ids.length, 5000, 'complete durable identity skeleton must not be truncated');
  assert(store.segments.filter((segment) => segment.state === 'materialized').length <= 60);
  assert(store.bodies.size <= 60, 'one-row turn bodies must remain within the configured budget');
  const runs = store.renderRuns();
  assert(runs.length <= 61, '5000 durable rows should render as bounded turns plus one coalesced gap');
  const gap = runs.find((run) => run.type === 'gap');
  assert(gap, '5000 durable rows should retain a compact unloaded gap');
  assert.equal(Object.hasOwn(gap, 'segmentIndexes'), false, 'huge gaps must not allocate a segment index per turn');
  assert(JSON.stringify(gap).length < 200, 'serialized huge-gap metadata must remain constant-sized');
  const batch = store.selectGapBatch(gap.startSegmentIndex, gap.endSegmentIndex, {
    targetOrdinal: gap.endOrdinal,
    direction: 'backward'
  });
  assert(batch.length > 0, 'a huge gap should yield a nearby materialization batch');
  assert(batch.length <= 32, `gap batch exceeded the client turn cap: ${batch.length}`);
  assert(batch.every((index) => index >= gap.startSegmentIndex && index <= gap.endSegmentIndex));
  assert.equal(batch[batch.length - 1], gap.endSegmentIndex, 'backward traversal should begin at the nearest gap edge');
  store._checkInvariants();
})();

(() => {
  const giantRows = 701;
  const trailingTurns = 80;
  const ids = Array.from({ length: giantRows + trailingTurns }, (_, index) => index + 1);
  const roles = `u${'at'.repeat(350)}${'u'.repeat(trailingTurns)}`;
  const store = new TranscriptStore('giant-atomic-turn', { maxMaterializedTurns: 60, overscanTurns: 0 });
  store.applyIndex(envelope(ids, { roles }));
  assert.equal(store.segments.length, trailingTurns + 1, '700 tool rows must remain one user-bounded segment');
  assert.deepEqual(
    [store.segments[0].startOrdinal, store.segments[0].endOrdinal],
    [0, giantRows - 1],
    'giant turn boundaries must include every following assistant/tool row'
  );
  const giantBatch = store.selectGapBatch(0, 0, { targetOrdinal: giantRows - 1, direction: 'center' });
  assert.deepEqual(giantBatch, [0], 'an arbitrarily large single turn must remain requestable atomically');
  materializeOrdinals(store, [0]);
  assert.equal(store.segments[0].state, 'materialized');
  assert.equal(store.bodies.size, giantRows, 'all durable rows in the giant turn must be retained together');
  assert.equal(store.segments.filter((segment) => segment.state === 'materialized').length, 1, 'giant turn consumes one materialized-turn budget unit');

  store.setViewport(0, giantRows - 1);
  materializeOrdinals(store, Array.from({ length: trailingTurns }, (_, index) => giantRows + index));
  store.enforceBudget();
  const materialized = store.segments.filter((segment) => segment.state === 'materialized').length;
  const pinnedMaterialized = [...store.pinnedSegments].filter((index) => store.segments[index]?.state === 'materialized').length;
  assert.equal(store.segments[0].state, 'materialized', 'visible giant turn must remain pinned despite its practical byte size');
  assert(materialized <= Math.max(60, pinnedMaterialized), 'distant turns must still be evicted around pinned correctness exceptions');
  assert.equal(store.bodies.size, giantRows + materialized - 1, 'row count must not be mistaken for the materialized-turn count');
  store._checkInvariants();
})();

(() => {
  const ids = Array.from({ length: 1000 }, (_, index) => index + 1);
  const roles = ids.map((_, index) => index % 10 === 0 ? 'u' : 'a').join('');
  const store = new TranscriptStore('turn-anchor-cap', { maxMaterializedTurns: 60, overscanTurns: 8 });
  store.applyIndex(envelope(ids, { roles }));
  const gap = store.renderRuns()[0];
  const batch = store.selectGapBatch(gap.startSegmentIndex, gap.endSegmentIndex, {
    targetOrdinal: 500,
    direction: 'center'
  });
  assert.equal(batch.length, 32, 'batch planning must be bounded only by conversational turn anchors');
  assert(batch.every((index) => store.segments[index].endOrdinal - store.segments[index].startOrdinal + 1 === 10));
})();

(() => {
  const store = new TranscriptStore('optimistic', { maxMaterializedTurns: 20, overscanTurns: 0 });
  store.applyIndex(envelope([1], { rev: 1 }));
  materializeOrdinals(store, [0]);
  store.addOptimistic({ id: 'local-user', clientKey: 'send-1-user', role: 'user' });
  store.addOptimistic({ id: 'local-assistant', clientKey: 'send-1-assistant', role: 'assistant' });
  store.addOptimistic({ id: 'duplicate', clientKey: 'send-1-assistant', role: 'assistant' });
  assert.equal(store.optimistic.length, 2, 'duplicate SSE replay must not duplicate optimistic rows');
  store.setActiveRun('resp-1', 1);
  assert.deepEqual(store.activeRun, { id: 'resp-1', startedRev: 1 });
  store.applyIndex(envelope([1, 2, 3], { rev: 3, roles: 'uua' }));
  materializeOrdinals(store, [1]);
  const removed = store.reconcileOptimistic();
  assert.equal(removed.length, 2);
  assert.equal(store.optimistic.length, 0);

  const stopped = new TranscriptStore('stopped');
  stopped.applyIndex(envelope([10], { rev: 5 }));
  stopped.addOptimistic({ clientKey: 'stopped-user', role: 'user' });
  stopped.reconcileOptimistic();
  assert.equal(stopped.optimistic.length, 1, 'stop-before-flush must preserve local rows');

  const tools = new TranscriptStore('tools');
  tools.applyIndex(envelope([20], { rev: 1 }));
  tools.addOptimistic({ clientKey: 'tools', role: 'tool-group', tools: [{ id: 'call-a' }, { id: 'call-b' }] });
  tools.applyIndex(envelope([20, 21, 22], { rev: 2, roles: 'uat' }));
  tools.materialize([
    body(20, 0, 'user'),
    body(21, 1, 'assistant', [{ type: 'tool_call', tool_call_id: 'call-a' }, { type: 'tool_call', tool_call_id: 'call-b' }]),
    body(22, 2, 'tool', [{ type: 'tool_result', tool_call_id: 'call-a' }, { type: 'tool_result', tool_call_id: 'call-b' }])
  ]);
  assert.equal(tools.reconcileOptimistic().length, 1, 'interleaved durable tool evidence must replace optimistic group');

  const scopedTools = new TranscriptStore('scoped-persisted-tools');
  scopedTools.applyIndex(envelope([30, 31], { rev: 1, roles: 'ua' }));
  scopedTools.addOptimistic({ clientKey: 'persisted-tools', role: 'tool-group', tools: [{ id: 'shared-call' }] }, 1, { persisted: true });
  scopedTools.addOptimistic({ clientKey: 'live-tools', role: 'tool-group', tools: [{ id: 'shared-call' }] });
  scopedTools.applyIndex(envelope([30, 31, 32, 33, 34, 35, 36], { rev: 2, roles: 'uauauat' }));
  scopedTools.materialize([
    body(30, 0, 'user'),
    body(31, 1, 'assistant'),
    body(32, 2, 'user'),
    body(33, 3, 'assistant'),
    body(34, 4, 'user'),
    body(35, 5, 'assistant', [{ type: 'tool_call', tool_call_id: 'shared-call' }]),
    body(36, 6, 'tool', [{ type: 'tool_result', tool_call_id: 'shared-call' }])
  ]);
  assert.deepEqual(
    scopedTools.reconcileOptimistic().map((entry) => entry.clientKey),
    ['live-tools'],
    'persisted tool IDs must match only their target segment while live entries retain later-turn matching'
  );
  assert.deepEqual(scopedTools.optimistic.map((entry) => entry.clientKey), ['persisted-tools']);

  const terminalTools = new TranscriptStore('terminal-tools');
  terminalTools.applyIndex(envelope([40], { rev: 5 }));
  terminalTools.addOptimistic({
    clientKey: 'completed-tool-tail',
    role: 'tool-group',
    status: 'done',
    tools: [{ id: 'local-only-call', status: 'done' }]
  }, 5, { persisted: true });
  terminalTools.addOptimistic({ clientKey: 'completed-standalone-tool', role: 'tool', status: 'error' }, 5, { persisted: true });
  terminalTools.addOptimistic({ clientKey: 'queued-user', role: 'user', durableSeqAtSend: 99 }, 5, { persisted: true });
  terminalTools.addOptimistic({
    clientKey: 'running-tool',
    role: 'tool-group',
    status: 'running',
    tools: [{ id: 'active-call', status: 'running' }]
  }, 5, { persisted: true });
  terminalTools.setActiveRun('resp-active', 5);
  terminalTools.applyIndex(envelope([40, 41], { rev: 6, roles: 'ua' }));
  materializeOrdinals(terminalTools, [1]);
  assert.equal(terminalTools.reconcileOptimistic().length, 0, 'active runs must retain unmatched completed tool UI');
  terminalTools.setActiveRun('', 0);
  assert.deepEqual(
    terminalTools.reconcileOptimistic().map((entry) => entry.clientKey),
    ['completed-tool-tail', 'completed-standalone-tool'],
    'an authoritative terminal revision must retire unmatched completed tool UI'
  );
  assert.deepEqual(
    terminalTools.optimistic.map((entry) => entry.clientKey),
    ['queued-user', 'running-tool'],
    'queued user messages and running tools must survive terminal tool cleanup'
  );

  const displayOnly = new TranscriptStore('display-only');
  displayOnly.addOptimistic({ clientKey: 'guardian', role: 'event', transient: true });
  displayOnly.addOptimistic({ clientKey: 'pending-user', role: 'user' });
  assert.equal(displayOnly.clearTransientOptimistic().length, 1, 'terminal lifecycle removes display-only rows explicitly');
  assert.deepEqual(displayOnly.optimistic.map((entry) => entry.clientKey), ['pending-user']);
  assert(__transcriptStats('display-only'));
  displayOnly.rekey('display-only-rekeyed');
  assert.equal(__transcriptStats('display-only'), null, 'rekeyed session retained its old diagnostics registry entry');
  assert(__transcriptStats('display-only-rekeyed'));
  displayOnly.destroy();
  assert.equal(__transcriptStats('display-only-rekeyed'), null, 'destroyed sessions must leave the global diagnostics registry');
})();

(() => {
  const fixture = [
    body(1, 0, 'user'),
    body(2, 1, 'assistant'),
    { id: 3, sequence: 2, role: 'event', parts: [], compaction_tail: true }
  ];
  const direct = new TranscriptStore('direct-fallback', { maxMaterializedTurns: 10 });
  direct.applyIndex(envelope([1, 2, 3], {
    rev: 0,
    roles: 'uae',
    flags: [0, 0, 3],
    compactionSeq: 2,
    compactionCount: 1
  }));
  direct.materialize(fixture, { countFetch: false });
  const fallback = transcriptStoreFromMessages('converted-fallback', [{
    messages: fixture.slice(1),
    has_more: true,
    compaction_seq: 2,
    compaction_count: 1
  }, {
    messages: fixture.slice(0, 1),
    has_more: false,
    compaction_seq: 2,
    compaction_count: 1
  }], { maxMaterializedTurns: 10 });
  assert.deepEqual(fallback.ids, direct.ids);
  assert.deepEqual(fallback.seqs, direct.seqs);
  assert.equal(fallback.roles, direct.roles);
  assert.deepEqual(fallback.flags, direct.flags);
  assert.deepEqual(fallback.renderedMessages().map((entry) => entry.id || entry.role), direct.renderedMessages().map((entry) => entry.id || entry.role));
  assert(__transcriptStats('converted-fallback'));
  fallback._checkInvariants();
})();

console.log('transcript-store tests passed');
