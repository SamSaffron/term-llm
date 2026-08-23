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

  const responseID = (payload, fallback = '') => String(payload?.response_id || payload?.response?.id || payload?.id || fallback || '').trim();

  const clone = (value) => (typeof structuredClone === 'function' ? structuredClone(value) : JSON.parse(JSON.stringify(value)));
  const guardianReview = (payload = {}) => ({
    outcome: String(payload.outcome || 'warning').trim(), message: String(payload.message || payload.text || '').trim(), model: String(payload.model || '').trim(), tool: String(payload.tool || '').trim(),
    command: String(payload.command || '').trim(), path: String(payload.path || '').trim(), is_write: Boolean(payload.is_write), workdir: String(payload.workdir || '').trim(),
  });
  const flushPendingGuardianReviews = (run, sequence) => {
    let offset = 0; for (const [callID, reviews] of run.pendingGuardianByCallID) for (const review of reviews) {
      const key = `${run.responseID}:guardian-notice:${sequence}:${offset++}`, outcome = review.outcome || 'warning'; run.projection.push({ key, id: key, role: 'guardian-notice', responseId: run.responseID, toolCallId: callID, terminalPolicy: 'durable', content: `${review.message || `Guardian ${outcome} review`} (unmatched tool call ${callID})`, guardianReviews: [clone(review)] });
    }
    run.pendingGuardianByCallID.clear();
  };

  const createActiveRun = ({ responseId, runEpoch = 0, anchor = null } = {}) => ({
    responseID: String(responseId || '').trim(), runEpoch: Math.max(0, int(runEpoch)),
    terminal: null, lastSequence: 0, anchor: anchor == null ? null : clone(anchor), projection: [],
    assistantByOrdinal: new Map(), toolByCallID: new Map(), pendingGuardianByCallID: new Map(),
    // Internal semantic cursor: null or the exact current tool-group object in projection.
    currentToolGroup: null,
  });

  const publicActiveRun = (run) => ({
    responseID: run.responseID, runEpoch: run.runEpoch,
    terminal: run.terminal ? { ...run.terminal } : null, lastSequence: run.lastSequence,
    anchor: run.anchor == null ? null : clone(run.anchor), projection: run.projection.map((entry) => clone(entry)),
  });

  const assistantEntry = (run, ordinal) => {
    const normalized = Math.max(0, int(ordinal));
    let entry = run.assistantByOrdinal.get(normalized);
    if (entry) return entry;
    entry = {
      id: `${run.responseID}:assistant:${normalized}`, key: `${run.responseID}:assistant:${normalized}`,
      role: 'assistant', responseId: run.responseID, assistantSegmentOrdinal: normalized,
      content: '', terminalPolicy: 'durable',
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
        id: key, key, role: 'tool-group', responseId: run.responseID,
        tools: [], status: 'running', terminalPolicy: 'durable',
      };
      run.projection.push(group);
    } else {
      group.status = 'running';
    }
    run.currentToolGroup = group;
    entry = {
      id, callId: id, name: String(item.name || ''), arguments: String(item.arguments || ''),
      argumentsFinalized: Boolean(String(item.arguments || '').trim()), status: 'running',
    };
    group.tools.push(entry);
    run.toolByCallID.set(id, entry);
    const pendingGuardian = run.pendingGuardianByCallID.get(id);
    if (pendingGuardian?.length) {
      entry.guardianReviews = pendingGuardian.map((review) => clone(review));
      run.pendingGuardianByCallID.delete(id);
    }
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
      case 'response.tool_exec.start': {
        const callID = String(payload.call_id || '').trim(), existed = run.toolByCallID.has(callID);
        const tool = toolEntry(run, callID, { name: payload.tool_name, arguments: payload.tool_arguments });
        if (payload.tool_name) tool.name = String(payload.tool_name);
        if (payload.tool_arguments && !tool.arguments) tool.arguments = String(payload.tool_arguments);
        tool.status = 'running'; structural = !existed;
        break;
      }
      case 'response.tool_exec.end': {
        const callID = String(payload.call_id || '').trim(), tool = toolEntry(run, callID, { name: payload.tool_name, arguments: payload.tool_arguments });
        if (payload.tool_name) tool.name = String(payload.tool_name);
        if (payload.tool_arguments) tool.arguments = String(payload.tool_arguments);
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
      case 'response.compaction': {
        closeToolGroupsAtBoundary(run);
        const eventSequence = validation.sequence;
        const existing = [...run.projection].reverse().find((entry) => (
          entry.role === 'compaction-ref' && !entry.eventSequence
        ));
        if (existing) {
          const existingIndex = run.projection.indexOf(existing);
          if (existingIndex >= 0) run.projection.splice(existingIndex, 1);
          existing.eventSequence = eventSequence;
          run.projection.push(existing);
        } else {
          const key = `${run.responseID}:compaction:event:${eventSequence}`;
          run.projection.push({
            key, id: key, role: 'compaction-ref', responseId: run.responseID,
            eventSequence, pending: true, terminalPolicy: 'durable',
          });
        }
        structural = true;
        break;
      }
      case 'response.guardian.review': {
        const callID = String(payload.tool_call_id || payload.call_id || '').trim();
        const review = guardianReview(payload);
        if (callID) {
          const tool = run.toolByCallID.get(callID);
          if (tool) {
            if (!Array.isArray(tool.guardianReviews)) tool.guardianReviews = [];
            tool.guardianReviews.push(review);
          } else {
            const pending = run.pendingGuardianByCallID.get(callID) || [];
            pending.push(review);
            run.pendingGuardianByCallID.set(callID, pending);
          }
        } else if (review.message) {
          const key = `${run.responseID}:guardian-notice:${validation.sequence}`;
          run.projection.push({ key, id: key, role: 'guardian-notice', responseId: run.responseID,
            terminalPolicy: 'durable', content: review.message, guardianReviews: [review] });
          structural = true;
        }
        break;
      }
      case 'response.phase':
      case 'response.model_swap.progress': {
        const key = `${run.responseID}:transient:${event}`;
        let entry = run.projection.find((candidate) => candidate.key === key);
        if (!entry) {
          const role = event === 'response.phase' ? 'phase' : 'model-swap';
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
        flushPendingGuardianReviews(run, validation.sequence);
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
    if (Object.prototype.hasOwnProperty.call(run, 'durableStartCompactionSeq')) Object.assign(candidate, { durableStartCompactionSeq: run.durableStartCompactionSeq, durableStartCompactionCount: run.durableStartCompactionCount });
    candidate.lastSequence = run.lastSequence;
    const currentToolGroupIndex = run.currentToolGroup ? run.projection.indexOf(run.currentToolGroup) : -1;
    candidate.projection = run.projection.map((entry) => clone(entry));
    candidate.currentToolGroup = currentToolGroupIndex >= 0 ? candidate.projection[currentToolGroupIndex] : null;
    for (const [callID, reviews] of run.pendingGuardianByCallID) candidate.pendingGuardianByCallID.set(callID, reviews.map((review) => clone(review)));
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
        entry.content = String(message.content || message.text || ''); entry.created = Number(message.created || message.created_at) || 0;
        entry.segmentStartSequence = int(message.segment_start_sequence); entry.segmentEndSequence = int(message.segment_end_sequence);
      } else if (message.role === 'tool-group') {
        const before = run.projection.length;
        for (const item of message.tools || []) Object.assign(toolEntry(run, item.id, item), clone(item));
        const group = run.projection.slice(before).find((entry) => entry.role === 'tool-group') || run.projection.findLast((entry) => entry.role === 'tool-group');
        if (group) group.created = Number(message.created || message.created_at) || 0;
        if (group && message.status) group.status = String(message.status);
      } else if (message.role === 'guardian-notice') {
        closeToolGroupsAtBoundary(run); const key = String(message.id || `${responseId}:guardian-notice:${run.projection.length + 1}`);
        run.projection.push({ key, id: key, role: 'guardian-notice', responseId, terminalPolicy: 'durable', content: String(message.content || '') });
      } else if (message.role === 'compaction-ref') {
        closeToolGroupsAtBoundary(run);
        const eventSequence = int(message.compaction_sequence ?? message.eventSequence);
        const key = `${responseId}:compaction:event:${eventSequence || run.projection.length + 1}`;
        run.projection.push({
          key, id: key, role: 'compaction-ref', responseId,
          eventSequence, pending: true, terminalPolicy: 'durable',
        });
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

  const anchorIndexForMessages = (messages, run) => {
    const clientAnchor = String(run?.anchor?.clientMessageId || '').trim(), rowAnchor = run?.anchor?.durableRowId;
    for (let index = messages.length - 1; index >= 0; index--) {
      const message = messages[index], identity = message?.durableRowId ?? message?.id ?? '';
      const start = Number(message?.durableRowStartId), end = Number(message?.durableRowEndId), row = Number(rowAnchor);
      const inRange = rowAnchor != null && Number.isFinite(row) && Number.isFinite(start) && Number.isFinite(end)
        && row >= Math.min(start, end) && row <= Math.max(start, end);
      const clientID = String(message?.clientMessageId || message?.client_message_id || '').trim();
      if ((clientAnchor && clientID === clientAnchor) || (rowAnchor != null && String(identity) === String(rowAnchor)) || inRange) return index;
    }
    return -1;
  };
  const recordCompactionRefs = (run, durable) => {
    if (!run) return;
    const anchor = run.anchor ? anchorIndexForMessages(durable, run) : -1;
    const refs = run.projection.filter((entry) => entry.role === 'compaction-ref');
    const pendingCount = refs.filter((entry) => !String(entry.compactionId || '').trim()).length;
    if (anchor < 0 && pendingCount === 0) return;
    const recorded = new Set(refs.map((entry) => String(entry.compactionId || '').trim()).filter(Boolean));
    let candidates = (anchor < 0 ? durable : durable.slice(anchor + 1)).filter((message) => (
      message?.role === 'compaction' || message?.role === 'compaction-boundary'
    ));
    // If the anchor fell outside the transcript window, pair live boundaries
    // with the newest unmatched durable markers. Older markers predate this run.
    if (anchor < 0) {
      candidates = candidates.filter((message) => !recorded.has(String(message?.id || '').trim())).slice(-pendingCount);
    }
    for (const message of candidates) {
      const id = String(message?.id || '').trim();
      if (!id || recorded.has(id)) continue;
      const pending = refs.find((entry) => !String(entry.compactionId || '').trim());
      if (pending) {
        pending.compactionId = id;
        pending.compactionSeq = int(message.compactionSeq ?? message.serverSeq, -1);
        pending.pending = false;
        recorded.add(id);
        continue;
      }
      const ref = { key: `${run.responseID}:compaction:${id}`, role: 'compaction-ref', compactionId: id, terminalPolicy: 'durable' };
      const created = Number(message.created || message.created_at), later = Number.isFinite(created) && created > 0 ? run.projection.findIndex((entry) => Number(entry.created) > created) : -1;
      run.projection.splice(later < 0 ? run.projection.length : later, 0, ref);
      refs.push(ref);
      run.currentToolGroup = null; recorded.add(id);
    }
  };
  const restoreCompactionRefs = (source, target) => {
    if (!source || !target) return;
    for (let index = 0; index < source.projection.length; index++) {
      const ref = source.projection[index]; if (ref.role !== 'compaction-ref') continue;
      let existing = target.projection.find((entry) => entry.role === 'compaction-ref' && (
        (ref.compactionId && entry.compactionId === ref.compactionId)
        || (ref.eventSequence && entry.eventSequence === ref.eventSequence)
      ));
      if (!existing && ref.compactionId && !ref.eventSequence) {
        existing = target.projection.find((entry) => entry.role === 'compaction-ref' && entry.eventSequence && !entry.compactionId);
      }
      if (existing) {
        if (ref.compactionId && !existing.compactionId) {
          existing.compactionId = ref.compactionId;
          existing.compactionSeq = ref.compactionSeq;
          existing.pending = false;
        }
        continue;
      }
      const precedingKey = [...source.projection.slice(0, index)].reverse().find((entry) => entry.role !== 'compaction-ref')?.key;
      const targetIndex = precedingKey ? target.projection.findIndex((entry) => entry.key === precedingKey) : -1;
      target.projection.splice(targetIndex < 0 ? target.projection.length : targetIndex + 1, 0, clone(ref));
    }
  };
  return Object.freeze({
    createActiveRun, publicActiveRun, reduceResponseEvent, reduceDetachedReplay, activeRunFromSnapshot,
    anchorIndexForMessages, recordCompactionRefs, restoreCompactionRefs,
  });
});
