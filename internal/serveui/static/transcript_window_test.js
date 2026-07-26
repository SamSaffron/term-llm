'use strict';

const assert = require('assert');
const { TranscriptWindow, TRANSCRIPT_FLAG_EMPTY_BODY } = require('./transcript-window.js');

const index = {
  rev: 3,
  compaction_seq: -1,
  compaction_count: 0,
  rows: {
    ids: [1, 2, 3],
    seqs: [0, 1, 2],
    roles: 'uat',
    flags: [0, 0, TRANSCRIPT_FLAG_EMPTY_BODY],
    client_message_ids: { 0: 'client-one' },
    response_ids: ['', 'resp-one', 'resp-one'],
    assistant_segment_ordinals: [-1, 0, -1],
  },
};

(() => {
  const window = new TranscriptWindow('session-one', { maxMaterializedTurns: 1, overscanTurns: 0 });
  const applied = window.applyIndex(index, '"rev-3"');
  assert.equal(applied.changed, true);
  assert.deepEqual(window.clientMessageIDs, ['client-one', '', '']);
  assert.equal(window.segments.length, 1);
  window.materializeSegment(0, [
    { id: 1, sequence: 0, role: 'user', parts: [{ type: 'text', text: 'question' }] },
    { id: 2, sequence: 1, role: 'assistant', parts: [{ type: 'text', text: 'answer' }] },
  ]);
  const visible = window.renderedMessages();
  assert.equal(visible.length, 2);
  assert.equal(visible[0].clientMessageId, 'client-one');
  assert.equal(visible[1].responseId, 'resp-one');
  assert.equal(visible[1].assistantSegmentOrdinal, 0);
  window._checkInvariants();
  const compaction = window.applyIndex({ ...index, rev: 4, compaction_seq: 2, compaction_count: 1 });
  assert.equal(compaction.compactionChanged, true);
  assert.throws(() => window.applyIndex({
    ...index,
    rows: { ...index.rows, client_message_ids: { 1: 'assistant-cannot-own-client-id' } }
  }), /sparse client_message_id/);
})();

const turnIndex = (turns, rev = turns * 2) => {
  const ids = [];
  const seqs = [];
  const responseIDs = [];
  const assistantOrdinals = [];
  for (let turn = 0; turn < turns; turn++) {
    ids.push(turn * 2 + 1, turn * 2 + 2);
    seqs.push(turn * 2, turn * 2 + 1);
    responseIDs.push('', `resp-${turn}`);
    assistantOrdinals.push(-1, 0);
  }
  return {
    rev,
    compaction_seq: -1,
    compaction_count: 0,
    rows: {
      ids,
      seqs,
      roles: 'ua'.repeat(turns),
      flags: ids.map(() => 0),
      client_message_ids: Object.fromEntries(Array.from({ length: turns }, (_, turn) => [turn * 2, `client-${turn}`])),
      response_ids: responseIDs,
      assistant_segment_ordinals: assistantOrdinals,
    },
  };
};

(() => {
  const atTail = new TranscriptWindow('follow-tail', { maxMaterializedTurns: 2, overscanTurns: 0 });
  atTail.applyIndex(turnIndex(3));
  atTail.setViewport(5, 5);
  atTail.materializeSegment(1, [
    { id: 3, sequence: 2, role: 'user' },
    { id: 4, sequence: 3, role: 'assistant' },
  ]);
  atTail.materializeSegment(2, [
    { id: 5, sequence: 4, role: 'user' },
    { id: 6, sequence: 5, role: 'assistant' },
  ]);

  const appended = atTail.applyIndex(turnIndex(10));
  assert.equal(appended.appendOnly, true);
  assert.deepEqual(atTail.viewport, { firstOrdinal: 19, lastOrdinal: 19 }, 'tail-following viewport advances across an append-only gap');
  assert(atTail.pinnedSegments.has(9), 'new tail segment is pinned before body-budget enforcement');
  atTail.materializeSegment(9, [
    { id: 19, sequence: 18, role: 'user' },
    { id: 20, sequence: 19, role: 'assistant' },
  ]);
  assert.equal(atTail.segments[9].state, 'materialized', 'distant new tail survives a full materialized-turn budget');
  assert.equal(atTail.segments[1].state, 'evicted', 'an older unpinned turn pays the body budget instead');
  atTail._checkInvariants();

  const scrolledAway = new TranscriptWindow('preserve-scroll', { maxMaterializedTurns: 2, overscanTurns: 0 });
  scrolledAway.applyIndex(turnIndex(3));
  scrolledAway.setViewport(0, 1);
  scrolledAway.applyIndex(turnIndex(10));
  assert.deepEqual(scrolledAway.viewport, { firstOrdinal: 0, lastOrdinal: 1 }, 'an append-only gap does not drag a historical viewport to the tail');
  assert(!scrolledAway.pinnedSegments.has(9), 'new tail remains unpinned while the user reads history');
  scrolledAway._checkInvariants();
})();

(async () => {
  const window = new TranscriptWindow('anchor');
  window.applyIndex(index);
  window.materializeSegment(0, [
    { id: 1, sequence: 0, role: 'user', parts: [{ type: 'text', text: 'question' }] },
    { id: 2, sequence: 1, role: 'assistant', parts: [{ type: 'text', text: 'answer' }] },
  ]);
  let renders = 0;
  await window.withViewportAnchor({
    capture: () => ({ id: 1, top: 10 }),
    render: () => { renders++; },
    topForID: () => 10,
    adjustScroll: (delta) => { assert.equal(delta, 0); },
  }, async () => true);
  assert.equal(renders, 1);
  console.log('transcript window tests passed');
})().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
