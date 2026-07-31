'use strict';

const assert = require('assert');
const active = require('./active-response.js');
const conversationAPI = require('./conversation.js');

const envelope = (messages, rev = 1) => ({ rev, messages, renderedMessages() { return this.messages; } });

(() => {
  const transcript = new conversationAPI.ConversationController('read-only-session');
  const session = { id: 'read-only-session', transcript };
  transcript.addPendingIntent({ id: 'client-1', clientMessageId: 'client-1', role: 'user', content: 'derived' });
  assert.equal(transcript.addPendingIntent({ id: 'assistant-shadow', role: 'assistant', content: 'forbidden' }), null);
  assert.deepEqual(conversationAPI.sessionMessages(session).map((message) => message.content), ['derived']);
  assert.equal(Object.hasOwn(session, 'messages'), false);
})();

(() => {
  const transcript = new conversationAPI.ConversationController('terminal-snapshot');
  transcript.replaceActiveSnapshot({
    id: 'resp-snapshot', run_epoch: 4, status: 'completed', last_sequence_number: 3,
    final_rev: 2, durable_handoff: true, durable_output_count: 1,
    recovery: { messages: [{ role: 'assistant', assistant_segment_ordinal: 0, content: 'recovered' }] }
  });
  assert.equal(transcript.conversation.active.terminal.finalRev, 2);
  assert.equal(conversationAPI.sessionMessages({ transcript })[0].content, 'recovered');
  conversationAPI.applyDurable(transcript.conversation, envelope([
    { id: 1, durableRowId: 1, role: 'user', durable: true, clientMessageId: 'client-snapshot', content: 'question' },
    { id: 2, durableRowId: 2, role: 'assistant', durable: true, responseId: 'resp-snapshot', content: 'recovered' },
  ], 2));
  assert.equal(transcript.conversation.active, null, 'terminal recovery snapshot did not hand off atomically');
})();

(() => {
  const run = active.createActiveRun({ responseId: 'terminal-tools', runEpoch: 1 });
  active.reduceResponseEvent(run, 'response.output_item.added', {
    response_id: 'terminal-tools', run_epoch: 1, sequence_number: 1,
    item: { type: 'function_call', call_id: 'running', name: 'shell' }
  });
  active.reduceResponseEvent(run, 'response.failed', {
    response_id: 'terminal-tools', run_epoch: 1, sequence_number: 2,
    final_rev: 1, durable_handoff: true, durable_output_count: 1
  });
  assert.equal(run.projection[0].status, 'done');
  assert.equal(run.projection[0].tools[0].status, 'done');
})();

(() => {
  const run = active.createActiveRun({ responseId: 'native-search', runEpoch: 1 });
  active.reduceResponseEvent(run, 'response.tool_exec.start', {
    response_id: 'native-search', run_epoch: 1, sequence_number: 1,
    call_id: 'ws_native', tool_name: 'web_search'
  });
  assert.equal(run.projection.length, 1);
  assert.equal(run.projection[0].role, 'tool-group');
  assert.equal(run.projection[0].tools[0].name, 'web_search');
  assert.equal(run.projection[0].tools[0].status, 'running');
  active.reduceResponseEvent(run, 'response.tool_exec.end', {
    response_id: 'native-search', run_epoch: 1, sequence_number: 2,
    call_id: 'ws_native', tool_name: 'web_search', tool_arguments: '{"query":"discourse news"}', success: true
  });
  assert.equal(run.projection[0].tools[0].status, 'done');
  assert.equal(run.projection[0].tools[0].arguments, '{"query":"discourse news"}');
})();

(() => {
  const conversation = conversationAPI.createConversation({ sessionId: 'normal', durable: envelope([], 0) });
  conversationAPI.addIntent(conversation, { id: 'local-1', clientMessageId: 'client-1', role: 'user', content: 'question' });
  conversationAPI.startActiveRun(conversation, { responseId: 'resp-1', runEpoch: 1, anchor: { clientMessageId: 'client-1' } });
  conversationAPI.applyRunEvent(conversation, 'response.created', { response_id: 'resp-1', run_epoch: 1, sequence_number: 1 });
  conversationAPI.applyRunEvent(conversation, 'response.output_text.delta', {
    response_id: 'resp-1', run_epoch: 1, sequence_number: 2, assistant_segment_ordinal: 0, delta: 'hello'
  });
  assert.deepEqual(conversationAPI.visibleMessages(conversation).map((message) => message.role), ['user', 'assistant']);
  conversationAPI.applyRunEvent(conversation, 'response.completed', {
    response_id: 'resp-1', run_epoch: 1, sequence_number: 3,
    final_rev: 2, durable_handoff: true, durable_output_count: 1
  });
  assert.equal(conversationAPI.commitDurableHandoff(conversation), false, 'active output must survive before durable revision');
  conversationAPI.applyDurable(conversation, envelope([
    { id: 1, role: 'user', clientMessageId: 'client-1', content: 'question', durable: true },
    { id: 2, role: 'assistant', responseId: 'resp-1', content: 'hello', durable: true },
  ], 2));
  assert.equal(conversation.active, null);
  assert.deepEqual(conversationAPI.visibleMessages(conversation).map((message) => message.id), [1, 2]);
})();

(() => {
  const conversation = conversationAPI.createConversation({ sessionId: 'invalid', durable: envelope([], 0) });
  conversationAPI.startActiveRun(conversation, { responseId: 'resp-invalid', runEpoch: 2 });
  conversationAPI.applyRunEvent(conversation, 'response.output_text.delta', {
    response_id: 'resp-invalid', run_epoch: 2, sequence_number: 1, delta: 'partial'
  });
  conversationAPI.applyRunEvent(conversation, 'response.failed', {
    response_id: 'resp-invalid', run_epoch: 2, sequence_number: 2,
    final_rev: 0, durable_handoff: false, durable_output_count: 1, durable_handoff_error: 'write failed'
  });
  conversationAPI.applyDurable(conversation, envelope([], 99));
  assert(conversation.active, 'invalid durable handoff must preserve frozen active projection');
  assert.equal(conversation.protocolError, 'write failed');
  assert.equal(conversationAPI.visibleMessages(conversation)[0].content, 'partial');
})();

(() => {
  const conversation = conversationAPI.createConversation({ sessionId: 'invalid-zero-rev', durable: envelope([], 0) });
  conversationAPI.startActiveRun(conversation, { responseId: 'resp-zero-rev', runEpoch: 1 });
  conversationAPI.applyRunEvent(conversation, 'response.output_text.delta', {
    response_id: 'resp-zero-rev', run_epoch: 1, sequence_number: 1, delta: 'must remain'
  });
  conversationAPI.applyRunEvent(conversation, 'response.completed', {
    response_id: 'resp-zero-rev', run_epoch: 1, sequence_number: 2,
    final_rev: 0, durable_handoff: true, durable_output_count: 1
  });
  assert.equal(conversation.active.terminal.durableHandoff, false);
  assert.match(conversation.protocolError, /committed transcript revision/);
})();

(() => {
  const conversation = conversationAPI.createConversation({ sessionId: 'model-swap-position', durable: envelope([
    { id: 1, durableRowId: 1, role: 'user', durable: true, clientMessageId: 'swap-trigger', content: 'continue differently' },
  ], 1) });
  conversationAPI.startActiveRun(conversation, { responseId: 'resp-swap-position', runEpoch: 1, anchor: { clientMessageId: 'swap-trigger' } });
  conversationAPI.applyRunEvent(conversation, 'response.model_swap.progress', {
    response_id: 'resp-swap-position', run_epoch: 1, sequence_number: 1, text: 'Switching provider…'
  });
  conversationAPI.applyRunEvent(conversation, 'response.output_text.delta', {
    response_id: 'resp-swap-position', run_epoch: 1, sequence_number: 2, delta: 'new answer'
  });
  assert.deepEqual(conversationAPI.visibleMessages(conversation).map((message) => message.role), ['user', 'model-swap', 'assistant']);

  conversationAPI.applyRunEvent(conversation, 'response.completed', {
    response_id: 'resp-swap-position', run_epoch: 1, sequence_number: 3,
    final_rev: 3, durable_handoff: true, durable_output_count: 2
  });
  assert.deepEqual(
    conversationAPI.visibleMessages(conversation).map((message) => message.role),
    ['user', 'model-swap', 'assistant'],
    'model swap status must keep its stream position until durable handoff'
  );

  conversationAPI.applyDurable(conversation, envelope([
    { id: 1, durableRowId: 1, role: 'user', durable: true, clientMessageId: 'swap-trigger', content: 'continue differently' },
    { id: 2, durableRowId: 2, role: 'model-swap', durable: true, responseId: 'resp-swap-position', content: '↔ Model switched' },
    { id: 3, durableRowId: 3, role: 'assistant', durable: true, responseId: 'resp-swap-position', content: 'new answer' },
  ], 3));
  assert.deepEqual(conversationAPI.visibleMessages(conversation).map((message) => message.role), ['user', 'model-swap', 'assistant']);
})();

(() => {
  const conversation = conversationAPI.createConversation({ sessionId: 'ask-user-position', durable: envelope([], 0) });
  conversationAPI.startActiveRun(conversation, { responseId: 'resp-ask-position', runEpoch: 1 });
  conversationAPI.applyRunEvent(conversation, 'response.output_item.added', {
    response_id: 'resp-ask-position', run_epoch: 1, sequence_number: 1,
    item: { type: 'function_call', call_id: 'call-ask-position', name: 'ask_user' }
  });
  conversationAPI.applyRunEvent(conversation, 'response.tool_exec.end', {
    response_id: 'resp-ask-position', run_epoch: 1, sequence_number: 2,
    call_id: 'call-ask-position', tool_name: 'ask_user', success: true
  });
  conversationAPI.addIntent(conversation, {
    id: 'ask-answer', clientMessageId: 'ask-answer', role: 'user', content: 'Diplomacy: Bribe it',
    askUser: true, askUserCallId: 'call-ask-position'
  });
  conversationAPI.applyRunEvent(conversation, 'response.output_text.delta', {
    response_id: 'resp-ask-position', run_epoch: 1, sequence_number: 3, delta: 'Correct.'
  });
  assert.deepEqual(
    conversationAPI.visibleMessages(conversation).map((message) => message.role),
    ['tool-group', 'user', 'assistant'],
    'ask_user answer must remain immediately after its tool group'
  );
  conversationAPI.applyDurable(conversation, envelope([
    {
      id: 'ask-position-durable', role: 'user', durable: true, responseId: 'resp-ask-position',
      content: 'Diplomacy: Bribe it', askUser: true, askUserCallId: 'call-ask-position'
    }
  ], 1));
  assert.deepEqual(
    conversationAPI.visibleMessages(conversation).map((message) => message.role),
    ['tool-group', 'user', 'assistant'],
    'durable ask_user answer moved below active assistant output'
  );
  assert.equal(conversation.intents.size, 0, 'durable active answer did not retire its optimistic intent');

  const reloaded = conversationAPI.createConversation({ sessionId: 'ask-user-reload', durable: envelope([], 0) });
  conversationAPI.addIntent(reloaded, {
    id: 'ask-local', clientMessageId: 'ask-local', role: 'user', content: 'Diplomacy: Bribe it',
    askUser: true, askUserCallId: 'call-ask-reload'
  });
  conversationAPI.applyDurable(reloaded, envelope([
    { id: 'ask-durable', role: 'user', content: 'Diplomacy: Bribe it', durable: true, askUser: true, askUserCallId: 'call-ask-reload' }
  ], 1));
  assert.equal(reloaded.intents.size, 0, 'durable ask_user result did not acknowledge its local answer');
  assert.deepEqual(conversationAPI.visibleMessages(reloaded).map((message) => message.id), ['ask-durable']);
  conversationAPI.applyDurable(reloaded, envelope([], 2));
  const stale = conversationAPI.addIntent(reloaded, {
    id: 'ask-stale', clientMessageId: 'ask-stale', role: 'user', content: 'Diplomacy: Bribe it',
    askUser: true, askUserCallId: 'call-ask-reload'
  });
  assert.equal(stale, null, 'window eviction allowed an acknowledged ask_user answer to reappear');
})();

(() => {
  const conversation = conversationAPI.createConversation({ sessionId: 'interjection', durable: envelope([], 0) });
  conversationAPI.addIntent(conversation, { id: 'interject-local', clientMessageId: 'client-interject', role: 'user', content: 'change direction' });
  conversationAPI.startActiveRun(conversation, { responseId: 'resp-interject', runEpoch: 3 });
  conversationAPI.applyRunEvent(conversation, 'response.output_text.delta', {
    response_id: 'resp-interject', run_epoch: 3, sequence_number: 1, assistant_segment_ordinal: 0, delta: 'before'
  });
  conversationAPI.applyRunEvent(conversation, 'response.interjection', {
    response_id: 'resp-interject', run_epoch: 3, sequence_number: 2, client_message_id: 'client-interject'
  });
  conversationAPI.applyRunEvent(conversation, 'response.output_text.new_segment', {
    response_id: 'resp-interject', run_epoch: 3, sequence_number: 3, assistant_segment_ordinal: 1
  });
  conversationAPI.applyRunEvent(conversation, 'response.output_text.delta', {
    response_id: 'resp-interject', run_epoch: 3, sequence_number: 4, assistant_segment_ordinal: 1, delta: 'after'
  });
  assert.deepEqual(conversationAPI.visibleMessages(conversation).map((message) => message.content), ['before', 'change direction', 'after']);
})();

(() => {
  const delivered = active.createActiveRun({ responseId: 'snapshot-equivalence', runEpoch: 3 });
  active.reduceResponseEvent(delivered, 'response.output_text.delta', {
    response_id: 'snapshot-equivalence', run_epoch: 3, sequence_number: 1, assistant_segment_ordinal: 0, delta: 'hello'
  });
  active.reduceResponseEvent(delivered, 'response.output_item.added', {
    response_id: 'snapshot-equivalence', run_epoch: 3, sequence_number: 2,
    item: { type: 'function_call', call_id: 'call-equivalent', name: 'read_file' }
  });
  active.reduceResponseEvent(delivered, 'response.function_call_arguments.delta', {
    response_id: 'snapshot-equivalence', run_epoch: 3, sequence_number: 3,
    call_id: 'call-equivalent', delta: '{"path":"README.md"}'
  });
  active.reduceResponseEvent(delivered, 'response.tool_exec.end', {
    response_id: 'snapshot-equivalence', run_epoch: 3, sequence_number: 4,
    call_id: 'call-equivalent', success: true
  });
  const recovered = active.activeRunFromSnapshot({
    id: 'snapshot-equivalence', run_epoch: 3, status: 'in_progress', last_sequence_number: 4,
    recovery: { messages: [
      { role: 'assistant', assistant_segment_ordinal: 0, content: 'hello' },
      { role: 'tool-group', status: 'done', tools: [{ id: 'call-equivalent', callId: 'call-equivalent', name: 'read_file', arguments: '{"path":"README.md"}', status: 'done', resultStatus: 'success' }] }
    ] }
  });
  const structural = (run) => run.projection.map((entry) => entry.role === 'tool-group'
    ? { role: entry.role, status: entry.status, tools: entry.tools.map((tool) => ({ id: tool.id, name: tool.name, arguments: tool.arguments, status: tool.status, resultStatus: tool.resultStatus })) }
    : { role: entry.role, content: entry.content, assistantSegmentOrdinal: entry.assistantSegmentOrdinal });
  assert.deepEqual(structural(recovered), structural(delivered), 'snapshot projection differs from uninterrupted event fold');
})();

(() => {
  const run = active.createActiveRun({ responseId: 'resp-replay', runEpoch: 1 });
  active.reduceResponseEvent(run, 'response.output_text.delta', {
    response_id: 'resp-replay', run_epoch: 1, sequence_number: 1, delta: 'a'
  });
  const replayed = active.reduceDetachedReplay(run, [{
    event: 'response.output_text.delta', payload: { response_id: 'resp-replay', run_epoch: 1, sequence_number: 2, delta: 'b' }
  }]);
  assert.equal(run.projection[0].content, 'a', 'detached replay mutated the published run');
  assert.equal(replayed.projection[0].content, 'ab');
  assert.equal(active.reduceResponseEvent(replayed, 'response.output_text.delta', {
    response_id: 'resp-replay', run_epoch: 1, sequence_number: 2, delta: 'b'
  }).duplicate, true);
  assert.throws(() => active.reduceResponseEvent(replayed, 'response.output_text.delta', {
    response_id: 'resp-replay', run_epoch: 1, sequence_number: 4, delta: 'gap'
  }), (error) => error.code === 'response_event_gap');
})();

(() => {
  const run = active.createActiveRun({ responseId: 'strict-run', runEpoch: 7 });
  assert.throws(() => active.reduceResponseEvent(run, 'response.output_text.delta', { run_epoch: 7, sequence_number: 1, delta: 'x' }), (error) => error.code === 'response_id_mismatch');
  assert.throws(() => active.reduceResponseEvent(run, 'response.output_text.delta', { response_id: 'strict-run', sequence_number: 1, delta: 'x' }), (error) => error.code === 'stale_run_epoch');
  assert.throws(() => active.reduceResponseEvent(run, 'response.output_text.delta', { response_id: 'strict-run', run_epoch: 7, sequence_number: 5, delta: 'x' }), (error) => error.code === 'response_event_gap');
  const conversation = conversationAPI.createConversation({ sessionId: 'strict', durable: envelope([], 0) });
  conversationAPI.startActiveRun(conversation, { responseId: 'first', runEpoch: 1 });
  assert.throws(() => conversationAPI.startActiveRun(conversation, { responseId: 'second', runEpoch: 2 }), /active response/);
})();

(() => {
  const conversation = conversationAPI.createConversation({ sessionId: 'tools', durable: envelope([], 0) });
  conversationAPI.startActiveRun(conversation, { responseId: 'resp-tools', runEpoch: 1 });
  assert.equal(conversationAPI.applyRunEvent(conversation, 'response.output_item.added', {
    response_id: 'resp-tools', run_epoch: 1, sequence_number: 1, item: { type: 'function_call', call_id: 'call-1', name: 'shell' }
  }).structural, true);
  conversationAPI.applyRunEvent(conversation, 'response.function_call_arguments.delta', {
    response_id: 'resp-tools', run_epoch: 1, sequence_number: 2, call_id: 'call-1', delta: '{"command":"pwd"}'
  });
  conversationAPI.applyRunEvent(conversation, 'response.tool_exec.end', {
    response_id: 'resp-tools', run_epoch: 1, sequence_number: 3, call_id: 'call-1', success: true
  });
  const [group] = conversationAPI.visibleMessages(conversation);
  assert.equal(group.role, 'tool-group');
  assert.equal(group.status, 'done');
  assert.deepEqual(group.tools.map((tool) => [tool.id, tool.arguments, tool.resultStatus]), [
    ['call-1', '{"command":"pwd"}', 'success']
  ]);
})();

(() => {
  const conversation = conversationAPI.createConversation({ sessionId: 'sequential-tools', durable: envelope([], 0) });
  conversationAPI.startActiveRun(conversation, { responseId: 'resp-sequential-tools', runEpoch: 1 });
  const apply = (event, sequenceNumber, payload = {}) => conversationAPI.applyRunEvent(conversation, event, {
    response_id: 'resp-sequential-tools', run_epoch: 1, sequence_number: sequenceNumber, ...payload
  });

  apply('response.output_item.added', 1, {
    item: { type: 'function_call', call_id: 'call-1', name: 'read_file' }
  });
  apply('response.tool_exec.end', 2, { call_id: 'call-1', success: true });
  apply('response.output_text.delta', 3, { assistant_segment_ordinal: 1, delta: '' });
  apply('response.output_item.added', 4, {
    item: { type: 'function_call', call_id: 'call-2', name: 'grep' }
  });
  apply('response.tool_exec.end', 5, { call_id: 'call-2', success: true });

  let visible = conversationAPI.visibleMessages(conversation);
  assert.deepEqual(visible.map((message) => message.role), ['tool-group']);
  assert.deepEqual(visible[0].tools.map((tool) => tool.id), ['call-1', 'call-2']);
  assert.equal(visible[0].status, 'done');

  apply('response.output_text.delta', 6, {
    assistant_segment_ordinal: 1, delta: 'I found another place to inspect.'
  });
  apply('response.output_item.added', 7, {
    item: { type: 'function_call', call_id: 'call-3', name: 'read_file' }
  });

  visible = conversationAPI.visibleMessages(conversation);
  assert.deepEqual(visible.map((message) => message.role), ['tool-group', 'assistant', 'tool-group']);
  assert.deepEqual(visible[0].tools.map((tool) => tool.id), ['call-1', 'call-2']);
  assert.deepEqual(visible[2].tools.map((tool) => tool.id), ['call-3']);
})();

(() => {
  const run = active.createActiveRun({ responseId: 'detached-tools', runEpoch: 1 });
  active.reduceResponseEvent(run, 'response.output_item.added', {
    response_id: 'detached-tools', run_epoch: 1, sequence_number: 1,
    item: { type: 'function_call', call_id: 'call-before-replay', name: 'read_file' }
  });
  active.reduceResponseEvent(run, 'response.tool_exec.end', {
    response_id: 'detached-tools', run_epoch: 1, sequence_number: 2,
    call_id: 'call-before-replay', success: true
  });

  const replayed = active.reduceDetachedReplay(run, [{
    event: 'response.output_item.added',
    payload: {
      response_id: 'detached-tools', run_epoch: 1, sequence_number: 3,
      item: { type: 'function_call', call_id: 'call-after-replay', name: 'grep' }
    }
  }]);

  assert.deepEqual(run.projection[0].tools.map((tool) => tool.id), ['call-before-replay'], 'detached replay mutated the published group');
  assert.deepEqual(replayed.projection.map((message) => message.role), ['tool-group']);
  assert.deepEqual(replayed.projection[0].tools.map((tool) => tool.id), ['call-before-replay', 'call-after-replay']);
})();

(() => {
  const recovered = active.activeRunFromSnapshot({
    id: 'recovered-tools', run_epoch: 1, status: 'in_progress', last_sequence_number: 2,
    recovery: { messages: [{
      role: 'tool-group', status: 'done',
      tools: [{ id: 'call-recovered', name: 'read_file', status: 'done', resultStatus: 'success' }]
    }] }
  });
  active.reduceResponseEvent(recovered, 'response.output_item.added', {
    response_id: 'recovered-tools', run_epoch: 1, sequence_number: 3,
    item: { type: 'function_call', call_id: 'call-live', name: 'grep' }
  });
  assert.deepEqual(recovered.projection.map((message) => message.role), ['tool-group']);
  assert.deepEqual(recovered.projection[0].tools.map((tool) => tool.id), ['call-recovered', 'call-live']);

  const recoveredAdjacent = active.activeRunFromSnapshot({
    id: 'recovered-adjacent-tools', run_epoch: 1, status: 'in_progress', last_sequence_number: 2,
    recovery: { messages: [
      { role: 'tool-group', status: 'done', tools: [{ id: 'call-adjacent-1', name: 'read_file', status: 'done' }] },
      { role: 'tool-group', status: 'done', tools: [{ id: 'call-adjacent-2', name: 'grep', status: 'done' }] }
    ] }
  });
  assert.deepEqual(recoveredAdjacent.projection.map((message) => message.role), ['tool-group']);
  assert.deepEqual(recoveredAdjacent.projection[0].tools.map((tool) => tool.id), ['call-adjacent-1', 'call-adjacent-2']);

  const recoveredBoundary = active.activeRunFromSnapshot({
    id: 'recovered-tool-boundary', run_epoch: 1, status: 'in_progress', last_sequence_number: 3,
    recovery: { messages: [
      { role: 'tool-group', status: 'done', tools: [{ id: 'call-before-text', name: 'read_file', status: 'done' }] },
      { role: 'assistant', assistant_segment_ordinal: 1, content: 'Inspecting another area.' },
      { role: 'tool-group', status: 'done', tools: [{ id: 'call-after-text', name: 'grep', status: 'done' }] }
    ] }
  });
  assert.deepEqual(recoveredBoundary.projection.map((message) => message.role), ['tool-group', 'assistant', 'tool-group']);
  assert.deepEqual(recoveredBoundary.projection[0].tools.map((tool) => tool.id), ['call-before-text']);
  assert.deepEqual(recoveredBoundary.projection[2].tools.map((tool) => tool.id), ['call-after-text']);
})();

(() => {
  const conversation = conversationAPI.createConversation({ sessionId: 'deltas', durable: envelope([], 0) });
  conversationAPI.startActiveRun(conversation, { responseId: 'resp-deltas', runEpoch: 1 });
  let structural = 0;
  for (let sequence = 1; sequence <= 100; sequence++) {
    if (conversationAPI.applyRunEvent(conversation, 'response.output_text.delta', {
      response_id: 'resp-deltas', run_epoch: 1, sequence_number: sequence, delta: 'x'
    }).structural) structural++;
  }
  assert.equal(structural, 1, 'text delta fast path should publish structure once');
  assert.equal(conversation.active.projection[0].content.length, 100);
})();

(() => {
  const transcript = new conversationAPI.ConversationController('compacted-handoff');
  transcript.applyIndex({
    rev: 1, compaction_seq: -1, compaction_count: 0,
    rows: { ids: [1], seqs: [0], roles: 'u', flags: [0], response_ids: [''], assistant_segment_ordinals: [-1] }
  });
  transcript.setActiveRun('resp-compacted', 1, 1);
  transcript.applyResponseEvent('response.output_text.delta', {
    response_id: 'resp-compacted', run_epoch: 1, sequence_number: 1, delta: 'answer'
  });
  transcript.applyResponseEvent('response.completed', {
    response_id: 'resp-compacted', run_epoch: 1, sequence_number: 2,
    final_rev: 2, durable_handoff: true, durable_output_count: 1
  });
  transcript.applyIndex({
    rev: 3, compaction_seq: 0, compaction_count: 1,
    rows: { ids: [9], seqs: [0], roles: 'a', flags: [0], response_ids: [''], assistant_segment_ordinals: [-1] }
  });
  transcript.materialize([{ id: 9, sequence: 0, role: 'assistant', parts: [{ type: 'text', text: 'compacted answer' }] }]);
  transcript.publishedMessages = [{ id: 9, durableRowId: 9, role: 'assistant', content: 'compacted answer', durable: true }];
  conversationAPI.applyDurable(transcript.conversation, transcript);
  assert.equal(transcript.conversation.active, null, 'coherent compaction must satisfy terminal handoff');
  assert.deepEqual(conversationAPI.sessionMessages({ transcript }).map((message) => message.content), ['compacted answer']);
})();

(() => {
  const transcript = new conversationAPI.ConversationController('post-compaction-terminal-snapshot');
  transcript.applyIndex({
    rev: 4, compaction_seq: 2, compaction_count: 1,
    rows: { ids: [20], seqs: [0], roles: 'a', flags: [0], response_ids: [''], assistant_segment_ordinals: [-1] }
  });
  transcript.materialize([{ id: 20, sequence: 0, role: 'assistant', parts: [{ type: 'text', text: 'coherent summary' }] }]);
  transcript.publishedMessages = [{ id: 20, durableRowId: 20, role: 'assistant', content: 'coherent summary', durable: true }];
  transcript.replaceActiveSnapshot({
    id: 'resp-before-compaction', run_epoch: 2, status: 'completed', last_sequence_number: 5,
    final_rev: 3, durable_handoff: true, durable_output_count: 1,
    handoff_compaction_seq: -1, handoff_compaction_count: 0,
    recovery: { messages: [{ role: 'assistant', assistant_segment_ordinal: 0, content: 'original answer' }] }
  });
  conversationAPI.applyDurable(transcript.conversation, transcript);
  assert.equal(transcript.conversation.active, null, 'terminal snapshot after compaction did not hand off to coherent summary');
})();

(() => {
  const transcript = new conversationAPI.ConversationController('premature-compaction');
  transcript.applyIndex({
    rev: 1, compaction_seq: -1, compaction_count: 0,
    rows: { ids: [], seqs: [], roles: '', flags: [], response_ids: [], assistant_segment_ordinals: [] }
  });
  transcript.setActiveRun('resp-wait', 1, 1);
  transcript.applyResponseEvent('response.output_text.delta', {
    response_id: 'resp-wait', run_epoch: 1, sequence_number: 1, delta: 'still visible'
  });
  transcript.applyResponseEvent('response.completed', {
    response_id: 'resp-wait', run_epoch: 1, sequence_number: 2,
    final_rev: 5, durable_handoff: true, durable_output_count: 1
  });
  transcript.applyIndex({
    rev: 4, compaction_seq: 0, compaction_count: 1,
    rows: { ids: [], seqs: [], roles: '', flags: [], response_ids: [], assistant_segment_ordinals: [] }
  });
  transcript.publishedMessages = [];
  conversationAPI.applyDurable(transcript.conversation, transcript);
  assert(transcript.conversation.active, 'compaction before final_rev must preserve frozen active output');
})();

(() => {
  const conversation = conversationAPI.createConversation({
    sessionId: 'missing-durable-anchor',
    durable: envelope([{ id: 1, durableRowId: 1, role: 'user', content: 'durable tail', durable: true }], 1)
  });
  conversationAPI.startActiveRun(conversation, { responseId: 'server-run', runEpoch: 1, anchor: { durableRowId: 999 } });
  conversationAPI.applyRunEvent(conversation, 'response.output_text.delta', {
    response_id: 'server-run', run_epoch: 1, sequence_number: 1, delta: 'server output'
  });
  assert.deepEqual(conversationAPI.visibleMessages(conversation).map((message) => message.content), ['durable tail', 'server output']);
})();

(() => {
  const transcript = new conversationAPI.ConversationController('exact-anchor');
  transcript.addPendingIntent({ id: 'local-a', clientMessageId: 'client-a', role: 'user', content: 'first', created: 1 });
  transcript.addPendingIntent({ id: 'local-b', clientMessageId: 'client-b', role: 'user', content: 'second', created: 2 });
  transcript.setActiveRun('resp-exact-anchor', 0, 1, { clientMessageId: 'client-a' });
  transcript.applyResponseEvent('response.output_text.delta', {
    response_id: 'resp-exact-anchor', run_epoch: 1, sequence_number: 1, delta: 'answer first'
  });
  assert.deepEqual(conversationAPI.sessionMessages({ transcript }).map((message) => message.content), ['first', 'answer first', 'second']);
})();

(() => {
  const transcript = new conversationAPI.ConversationController('active-compaction-order');
  transcript.applyIndex({
    rev: 1, compaction_seq: -1, compaction_count: 0,
    rows: { ids: [1], seqs: [1], roles: 'u', flags: [0], client_message_ids: { 0: 'prompt' }, response_ids: [''], assistant_segment_ordinals: [-1] }
  });
  transcript.publishedMessages = [
    { id: 1, durableRowId: 1, role: 'user', clientMessageId: 'prompt', content: 'question', durable: true }
  ];
  conversationAPI.applyDurable(transcript.conversation, transcript);
  transcript.setActiveRun('long-run', 1, 1, { anchorRowId: 1 });
  transcript.applyResponseEvent('response.output_text.delta', {
    response_id: 'long-run', run_epoch: 1, sequence_number: 1,
    assistant_segment_ordinal: 0, delta: 'before compaction'
  });

  transcript.publishedMessages = [
    { id: 1, durableRowId: 1, role: 'user', clientMessageId: 'prompt', content: 'question', durable: true },
    { id: 2, durableRowId: 2, role: 'compaction', content: 'Context compacted', durable: true, activeBoundary: true },
    { id: 3, durableRowId: 3, role: 'assistant', content: 'external output', durable: true }
  ];
  conversationAPI.applyDurable(transcript.conversation, transcript);
  conversationAPI.dispatchRunEvent(transcript, 'response.output_text.new_segment', {
    response_id: 'long-run', run_epoch: 1, sequence_number: 2, assistant_segment_ordinal: 1
  });
  conversationAPI.dispatchRunEvent(transcript, 'response.output_text.delta', {
    response_id: 'long-run', run_epoch: 1, sequence_number: 3,
    assistant_segment_ordinal: 1, delta: 'after compaction'
  });

  assert.deepEqual(conversationAPI.sessionMessages({ transcript }).map((message) => message.content), [
    'question', 'before compaction', 'Context compacted', 'after compaction', 'external output'
  ], 'a compaction observed during an active response must not trail newer live output');

  transcript.replaceActiveSnapshot({
    id: 'long-run', run_epoch: 1, status: 'in_progress', last_sequence_number: 3,
    recovery: { messages: [
      { role: 'assistant', assistant_segment_ordinal: 0, content: 'before compaction' },
      { role: 'assistant', assistant_segment_ordinal: 1, content: 'after compaction' }
    ] }
  });
  assert.deepEqual(conversationAPI.sessionMessages({ transcript }).map((message) => message.content), [
    'question', 'before compaction', 'Context compacted', 'after compaction', 'external output'
  ], 'snapshot replacement must preserve an observed compaction position');
})();

(() => {
  const conversation = conversationAPI.createConversation({
    sessionId: 'recovered-compaction-order',
    durable: envelope([
      { id: 1, durableRowId: 1, role: 'user', content: 'question', durable: true, created: 100 },
      { id: 2, durableRowId: 2, role: 'compaction', content: 'Context compacted', durable: true, created: 300 }
    ], 2)
  });
  conversationAPI.replaceActiveFromSnapshot(conversation, {
    id: 'recovered-run', run_epoch: 1, status: 'in_progress', last_sequence_number: 2,
    recovery: { messages: [
      { role: 'assistant', assistant_segment_ordinal: 0, content: 'before compaction', created: 200 },
      { role: 'assistant', assistant_segment_ordinal: 1, content: 'after compaction', created: 400 }
    ] }
  }, { anchor: { durableRowId: 1 } });
  assert.deepEqual(conversationAPI.visibleMessages(conversation).map((message) => message.content), [
    'question', 'before compaction', 'Context compacted', 'after compaction'
  ], 'fresh recovery should place compaction by durable and recovered creation times');
})();

(() => {
  const conversation = conversationAPI.createConversation({
    sessionId: 'active-compaction-tools',
    durable: envelope([{ id: 1, durableRowId: 1, role: 'user', content: 'question', durable: true }], 1)
  });
  conversationAPI.startActiveRun(conversation, { responseId: 'tool-run', runEpoch: 1, anchor: { durableRowId: 1 } });
  conversationAPI.applyRunEvent(conversation, 'response.output_item.added', {
    response_id: 'tool-run', run_epoch: 1, sequence_number: 1,
    item: { type: 'function_call', call_id: 'before', name: 'shell' }
  });
  conversationAPI.applyDurable(conversation, envelope([
    { id: 1, durableRowId: 1, role: 'user', content: 'question', durable: true },
    { id: 2, durableRowId: 2, role: 'compaction-boundary', content: 'Context compacted', durable: true }
  ], 2));
  conversationAPI.applyDurable(conversation, conversation.durable);
  conversationAPI.applyRunEvent(conversation, 'response.output_item.added', {
    response_id: 'tool-run', run_epoch: 1, sequence_number: 2,
    item: { type: 'function_call', call_id: 'after', name: 'shell' }
  });
  assert.deepEqual(conversationAPI.visibleMessages(conversation).map((message) => (
    message.role === 'tool-group' ? message.tools.map((tool) => tool.id).join(',') : message.content
  )), ['question', 'before', 'Context compacted', 'after'], 'compaction must split adjacent live tool groups');
})();

(() => {
  const transcript = new conversationAPI.ConversationController('authoritative-transition');
  transcript.setActiveRun('resp-old', 4, 10);
  assert.throws(() => transcript.setActiveRun('resp-new', 5, 11), /active response/);
  assert.equal(transcript.transitionAuthoritativeRun('resp-new', 5, 11), true);
  assert.equal(transcript.activeRun.id, 'resp-new');
  assert.equal(transcript.activeRun.epoch, 11);
  assert.equal(transcript.activeRun.startedRev, 5);
  assert.equal(transcript.transitionAuthoritativeRun('resp-stale', 6, 10), false);
  assert.equal(transcript.transitionAuthoritativeRun('resp-same-epoch', 6, 11), false);
  assert.equal(transcript.activeRun.id, 'resp-new');
})();

(() => {
  const transcript = new conversationAPI.ConversationController('attached-external-write');
  transcript.applyIndex({
    rev: 1, compaction_seq: -1, compaction_count: 0,
    rows: { ids: [1], seqs: [0], roles: 'u', flags: [0], client_message_ids: { 0: 'local-user' }, response_ids: [''], assistant_segment_ordinals: [-1] }
  });
  transcript.publishedMessages = [{ id: 1, durableRowId: 1, role: 'user', clientMessageId: 'local-user', content: 'question', durable: true }];
  conversationAPI.applyDurable(transcript.conversation, transcript);
  transcript.setActiveRun('web-run', 1, 1, { anchorRowId: 1 });
  transcript.applyResponseEvent('response.output_text.delta', {
    response_id: 'web-run', run_epoch: 1, sequence_number: 1, delta: 'active web output'
  });

  // A CLI/TUI writer commits an unrelated durable row while the web run is
  // attached. Exact response ownership keeps both objects visible and ordered.
  transcript.applyIndex({
    rev: 2, compaction_seq: -1, compaction_count: 0,
    rows: { ids: [1, 2], seqs: [0, 1], roles: 'ua', flags: [0, 0], client_message_ids: { 0: 'local-user' }, response_ids: ['', ''], assistant_segment_ordinals: [-1, -1] }
  });
  transcript.publishedMessages = [
    { id: 1, durableRowId: 1, role: 'user', clientMessageId: 'local-user', content: 'question', durable: true },
    { id: 2, durableRowId: 2, role: 'assistant', content: 'external CLI output', durable: true }
  ];
  conversationAPI.applyDurable(transcript.conversation, transcript);
  assert.deepEqual(conversationAPI.sessionMessages({ transcript }).map((message) => message.content), [
    'question', 'active web output', 'external CLI output'
  ]);
  assert.equal(transcript.activeRun.id, 'web-run');
})();

(async () => {
  const queue = new conversationAPI.SessionCommandQueue();
  const order = [];
  const first = queue.enqueue(async () => { await new Promise((resolve) => setTimeout(resolve, 5)); order.push(1); });
  const second = queue.enqueue(async () => { order.push(2); });
  await Promise.all([first, second]);
  assert.deepEqual(order, [1, 2]);
  console.log('conversation lifecycle tests passed');
})().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
