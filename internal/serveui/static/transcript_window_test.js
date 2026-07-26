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
