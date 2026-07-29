'use strict';

(function activeResponseModule(root, factory) {
  const api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.TermLLMActiveResponse = api;
})(typeof window !== 'undefined' ? window : globalThis, function activeResponseFactory() {
  const int = (value, fallback = 0) => {
    const parsed = Number(value);
    return Number.isFinite(parsed) ? Math.trunc(parsed) : fallback;
  };

  const responseID = (payload, fallback = '') => String(
    payload?.response_id || payload?.response?.id || payload?.id || fallback || ''
  ).trim();

  const clone = (value) => {
    if (typeof structuredClone === 'function') return structuredClone(value);
    return JSON.parse(JSON.stringify(value));
  };

  const createActiveRun = ({ responseId, runEpoch = 0, anchor = null } = {}) => ({
    responseID: String(responseId || '').trim(),
    runEpoch: Math.max(0, int(runEpoch)),
    terminal: null,
    lastSequence: 0,
    anchor: anchor == null ? null : clone(anchor),
    projection: [],
    assistantByOrdinal: new Map(),
    toolByCallID: new Map(),
    // Internal semantic cursor: null or the exact current tool-group object in projection.
    currentToolGroup: null,
  });

  const publicActiveRun = (run) => ({
    responseID: run.responseID,
    runEpoch: run.runEpoch,
    terminal: run.terminal ? { ...run.terminal } : null,
    lastSequence: run.lastSequence,
    anchor: run.anchor == null ? null : clone(run.anchor),
    projection: run.projection.map((entry) => clone(entry)),
  });

  const assistantEntry = (run, ordinal) => {
    const normalized = Math.max(0, int(ordinal));
    let entry = run.assistantByOrdinal.get(normalized);
    if (entry) return entry;
    entry = {
      id: `${run.responseID}:assistant:${normalized}`,
      key: `${run.responseID}:assistant:${normalized}`,
      role: 'assistant',
      responseId: run.responseID,
      assistantSegmentOrdinal: normalized,
      content: '',
      terminalPolicy: 'durable',
    };
    run.assistantByOrdinal.set(normalized, entry);
    run.projection.push(entry);
    return entry;
  };

  const toolEntry = (run, callID, item = {}) => {
    const id = String(callID || item.call_id || item.id || '').trim();
    if (!id) throw new Error('response tool event is missing call_id');
    let entry = run.toolByCallID.get(id);
    if (entry) return entry;
    let group = run.terminal ? null : run.currentToolGroup;
    if (!group) {
      const key = `${run.responseID}:tools:${id}`;
      group = {
        id: key,
        key,
        role: 'tool-group',
        responseId: run.responseID,
        tools: [],
        status: 'running',
        terminalPolicy: 'durable',
      };
      run.projection.push(group);
    } else {
      group.status = 'running';
    }
    run.currentToolGroup = group;
    entry = {
      id,
      callId: id,
      name: String(item.name || ''),
      arguments: String(item.arguments || ''),
      argumentsFinalized: Boolean(String(item.arguments || '').trim()),
      status: 'running',
    };
    group.tools.push(entry);
    run.toolByCallID.set(id, entry);
    return entry;
  };

  const closeToolGroupsAtBoundary = (run) => {
    for (const group of run.projection) {
      if (group.role !== 'tool-group' || group.status !== 'running') continue;
      for (const tool of group.tools || []) if (tool.status === 'running') tool.status = 'done';
      group.status = 'done';
    }
    // Execution completion only changes display status. Assistant text, user
    // intent, and terminal events call this helper to end the semantic group.
    run.currentToolGroup = null;
  };

  const validateEnvelope = (run, payload) => {
    const owner = responseID(payload);
    if (!owner) {
      const error = new Error('response event is missing response_id');
      error.code = 'response_id_mismatch';
      throw error;
    }
    if (owner !== run.responseID) {
      const error = new Error(`response owner mismatch: active=${run.responseID} event=${owner}`);
      error.code = 'response_owner_mismatch';
      throw error;
    }
    const epoch = Math.max(0, int(payload?.run_epoch));
    if (!epoch || !run.runEpoch || epoch !== run.runEpoch) {
      const error = new Error(`response epoch mismatch: active=${run.runEpoch} event=${epoch}`);
      error.code = epoch < run.runEpoch || !epoch ? 'stale_run_epoch' : 'future_run_epoch';
      throw error;
    }

    const sequence = Math.max(0, int(payload?.sequence_number));
    if (!sequence) {
      const error = new Error('response event is missing sequence_number');
      error.code = 'response_event_gap';
      error.expected = run.lastSequence + 1;
      error.received = sequence;
      throw error;
    }
    if (sequence <= run.lastSequence) return { duplicate: true, sequence };
    if (sequence !== run.lastSequence + 1) {
      const error = new Error(`response event gap: expected ${run.lastSequence + 1}, got ${sequence}`);
      error.code = 'response_event_gap';
      error.expected = run.lastSequence + 1;
      error.received = sequence;
      throw error;
    }
    return { duplicate: false, sequence };
  };

  const reduceResponseEvent = (source, event, payload = {}) => {
    const run = source;
    if (!run || !run.responseID) throw new Error('active response is required');
    if (event === 'response.stream_error') return { run, changed: false, duplicate: false, structural: false };
    const validation = validateEnvelope(run, payload);
    if (validation.duplicate && event !== 'response.stream_error') {
      return { run, changed: false, duplicate: true, structural: false };
    }

    let structural = false;
    switch (event) {
      case 'response.created':
        break;
      case 'response.output_text.delta': {
        const delta = String(payload.delta || '');
        if (!delta) break;
        const ordinal = payload.assistant_segment_ordinal ?? payload.output_index ?? 0;
        const existed = run.assistantByOrdinal.has(Math.max(0, int(ordinal)));
        closeToolGroupsAtBoundary(run);
        const assistant = assistantEntry(run, ordinal);
        if (!existed) structural = true;
        assistant.content += delta;
        assistant.segmentStartSequence = assistant.segmentStartSequence || int(payload.segment_start_sequence, validation.sequence);
        assistant.segmentEndSequence = validation.sequence || assistant.segmentEndSequence || 0;
        break;
      }
      case 'response.output_text.new_segment':
        closeToolGroupsAtBoundary(run);
        assistantEntry(run, payload.assistant_segment_ordinal ?? payload.output_index ?? 0);
        structural = true;
        break;
      case 'response.output_item.added': {
        const item = payload.item || {};
        if (item.type === 'function_call') {
          const tool = toolEntry(run, item.call_id, item);
          tool.outputIndex = payload.output_index;
          structural = true;
        }
        break;
      }
      case 'response.function_call_arguments.delta': {
        const callID = String(payload.call_id || payload.item_id || '').trim();
        const values = [...run.toolByCallID.values()];
        const tool = callID
          ? toolEntry(run, callID, payload)
          : (payload.output_index != null
            ? values.find((candidate) => candidate.outputIndex === payload.output_index)
            : values.findLast((candidate) => candidate.status === 'running'));
        if (!tool) break;
        if (!tool.argumentsFinalized) tool.arguments += String(payload.delta || '');
        break;
      }
      case 'response.output_item.done': {
        const item = payload.item || {};
        if (item.type === 'function_call') {
          const tool = toolEntry(run, item.call_id, item);
            if (Object.prototype.hasOwnProperty.call(item, 'arguments')) tool.arguments = String(item.arguments || '');
            tool.argumentsFinalized = true;
        }
        break;
      }
      case 'response.tool_exec.end': {
        const tool = toolEntry(run, payload.call_id, payload);
        tool.status = payload.success === false ? 'error' : 'done';
        tool.resultStatus = payload.success === false ? 'error' : 'success';
        if (Array.isArray(payload.images) && payload.images.length) tool.images = payload.images.slice();
        const group = run.projection.find((candidate) => candidate.role === 'tool-group' && candidate.tools?.includes(tool));
        if (group && group.tools.every((candidate) => candidate.status !== 'running')) group.status = 'done';
        structural = true;
        break;
      }
      case 'response.interjection': {
        const clientMessageId = String(payload.client_message_id || payload.interjection_id || '').trim();
        if (!clientMessageId) throw new Error('response interjection is missing client_message_id');
        closeToolGroupsAtBoundary(run);
        if (!run.projection.some((entry) => entry.role === 'intent-ref' && entry.clientMessageId === clientMessageId)) {
          run.projection.push({
            key: `${run.responseID}:intent:${clientMessageId}`,
            role: 'intent-ref',
            clientMessageId,
            terminalPolicy: 'transient',
          });
          structural = true;
        }
        break;
      }
      case 'response.phase':
      case 'response.model_swap.progress':
      case 'response.guardian.review': {
        const key = `${run.responseID}:transient:${event}`;
        let entry = run.projection.find((candidate) => candidate.key === key);
        if (!entry) {
          const role = event === 'response.phase' ? 'phase' : (event === 'response.model_swap.progress' ? 'model-swap' : 'event');
          // A model-swap row has a durable replacement at handoff. Keep the
          // live row mounted through terminal state so it does not disappear
          // and reappear (or appear to move) while that replacement arrives.
          const terminalPolicy = role === 'model-swap' ? 'durable' : 'transient';
          entry = { key, id: key, role, responseId: run.responseID, terminalPolicy, transient: true, content: '' };
          run.projection.push(entry);
          structural = true;
        }
        entry.content = String(payload.text || payload.message || payload.status || '');
        break;
      }
      case 'response.completed':
      case 'response.cancelled':
      case 'response.failed': {
        closeToolGroupsAtBoundary(run);
        const finalRev = Math.max(0, int(payload.final_rev ?? payload.response?.final_rev));
        const durableOutputCount = Math.max(0, int(payload.durable_output_count ?? payload.response?.durable_output_count));
        const declaredHandoff = (payload.durable_handoff ?? payload.response?.durable_handoff) === true;
        const validHandoff = declaredHandoff && (durableOutputCount === 0 || finalRev > 0);
        run.terminal = {
          status: event.slice('response.'.length),
          finalRev,
          durableOutputCount,
          durableHandoff: validHandoff,
          error: validHandoff ? String(payload.durable_handoff_error ?? payload.response?.durable_handoff_error ?? '')
            : String(payload.durable_handoff_error ?? payload.response?.durable_handoff_error ?? (declaredHandoff ? 'durable output has no committed transcript revision' : '')),
          compactionSeq: int(payload.handoff_compaction_seq ?? payload.response?.handoff_compaction_seq, -1),
          compactionCount: Math.max(0, int(payload.handoff_compaction_count ?? payload.response?.handoff_compaction_count)),
        };
        structural = true;
        break;
      }
      case 'response.attempt.discard':
        run.projection = run.projection.filter((entry) => entry.role !== 'assistant' && entry.role !== 'tool-group');
        run.assistantByOrdinal.clear();
        run.toolByCallID.clear();
        run.currentToolGroup = null;
        structural = true;
        break;
      case 'response.stream_error':
        break;
      default:
        break;
    }
    if (validation.sequence) run.lastSequence = Math.max(run.lastSequence, validation.sequence);
    return { run, changed: true, duplicate: false, structural };
  };

  const reduceDetachedReplay = (run, events) => {
    const candidate = createActiveRun({ responseId: run.responseID, runEpoch: run.runEpoch, anchor: run.anchor });
    candidate.lastSequence = run.lastSequence;
    const currentToolGroupIndex = run.currentToolGroup ? run.projection.indexOf(run.currentToolGroup) : -1;
    candidate.projection = run.projection.map((entry) => clone(entry));
    candidate.currentToolGroup = currentToolGroupIndex >= 0 ? candidate.projection[currentToolGroupIndex] : null;
    for (const entry of candidate.projection) {
      if (entry.role === 'assistant') candidate.assistantByOrdinal.set(int(entry.assistantSegmentOrdinal), entry);
      if (entry.role === 'tool-group') {
        for (const tool of entry.tools || []) candidate.toolByCallID.set(String(tool.callId || tool.id || ''), tool);
      }
    }
    for (const item of events || []) reduceResponseEvent(candidate, item.event, item.payload || {});
    return candidate;
  };

  const activeRunFromSnapshot = (snapshot, options = {}) => {
    const responseId = responseID(snapshot, options.responseId);
    if (!responseId) throw new Error('recovery snapshot is missing response_id');
    const runEpoch = Math.max(0, int(snapshot.run_epoch));
    if (!runEpoch) throw new Error('recovery snapshot is missing run_epoch');
    const run = createActiveRun({ responseId, runEpoch, anchor: options.anchor });
    run.lastSequence = Math.max(0, int(snapshot.last_sequence_number ?? snapshot?.recovery?.sequence_number));
    for (const message of snapshot?.recovery?.messages || snapshot?.messages || []) {
      if (message.role === 'assistant') {
        closeToolGroupsAtBoundary(run);
        const entry = assistantEntry(run, message.assistant_segment_ordinal ?? 0);
        entry.content = String(message.content || message.text || '');
        entry.segmentStartSequence = int(message.segment_start_sequence);
        entry.segmentEndSequence = int(message.segment_end_sequence);
      } else if (message.role === 'tool-group') {
        // Recovery uses the same semantic rule as live folding: adjacent tool
        // rows continue one group unless an assistant or user row intervenes.
        const before = run.projection.length;
        for (const item of message.tools || []) Object.assign(toolEntry(run, item.id, item), clone(item));
        const group = run.projection.slice(before).find((entry) => entry.role === 'tool-group') || run.projection.findLast((entry) => entry.role === 'tool-group');
        if (group && message.status) group.status = String(message.status);
      } else if (message.role === 'user') {
        closeToolGroupsAtBoundary(run);
        const clientMessageId = String(message.client_message_id || message.clientMessageId || message.id || '').trim();
        if (clientMessageId) run.projection.push({ key: `${responseId}:intent:${clientMessageId}`, role: 'intent-ref', clientMessageId, terminalPolicy: 'transient' });
      }
    }
    const terminalSource = snapshot?.response && typeof snapshot.response === 'object' ? { ...snapshot, ...snapshot.response } : snapshot;
    if (terminalSource.status && terminalSource.status !== 'in_progress') {
      closeToolGroupsAtBoundary(run);
      const finalRev = Math.max(0, int(terminalSource.final_rev));
      const durableOutputCount = Math.max(0, int(terminalSource.durable_output_count));
      const declaredHandoff = terminalSource.durable_handoff === true;
      const validHandoff = declaredHandoff && (durableOutputCount === 0 || finalRev > 0);
      run.terminal = {
        status: String(terminalSource.status),
        finalRev,
        durableOutputCount,
        durableHandoff: validHandoff,
        error: validHandoff ? String(terminalSource.durable_handoff_error || '')
          : String(terminalSource.durable_handoff_error || (declaredHandoff ? 'durable output has no committed transcript revision' : '')),
        compactionSeq: int(terminalSource.handoff_compaction_seq, -1),
        compactionCount: Math.max(0, int(terminalSource.handoff_compaction_count)),
      };
    }
    return run;
  };

  return Object.freeze({
    createActiveRun,
    publicActiveRun,
    reduceResponseEvent,
    reduceDetachedReplay,
    activeRunFromSnapshot,
  });
});
