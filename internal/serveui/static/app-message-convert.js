'use strict';
(function initMessageConvert() {
const app = window.TermLLMApp;
const {
  generateId
} = app;
const rebaseMessageAssetURL = (url) => typeof app.rebaseHubAssetURL === 'function' ? app.rebaseHubAssetURL(url) : String(url || '').trim();
// ===== Server session helpers =====
const safeServerIdToken = (value) => {
  const token = String(value ?? '').trim();
  return token ? token.replace(/[^A-Za-z0-9_-]/g, '_') : '';
};
const serverMessageRawKey = (msg) => {
  if (!msg || typeof msg !== 'object') return '';
  if (msg.sequence !== undefined && msg.sequence !== null && Number.isFinite(Number(msg.sequence))) {
    return `seq:${Number(msg.sequence)}`;
  }
  if (msg.id !== undefined && msg.id !== null && String(msg.id).trim() !== '') {
    return `id:${String(msg.id)}`;
  }
  return '';
};
const serverMessageBaseId = (msg) => {
  if (!msg || typeof msg !== 'object') return generateId('msg');
  if (msg.sequence !== undefined && msg.sequence !== null && Number.isFinite(Number(msg.sequence))) {
    return `srv_seq_${Number(msg.sequence)}`;
  }
  if (msg.id !== undefined && msg.id !== null && String(msg.id).trim() !== '') {
    const token = safeServerIdToken(msg.id);
    if (token) return `srv_${token}`;
  }
  return generateId('msg');
};
const serverMessageSequence = (msg) => {
  const seq = Number(msg?.sequence);
  return Number.isFinite(seq) ? seq : null;
};
const serverMessageCreatedAt = (msg) => {
  const created = Number(msg?.created_at);
  return Number.isFinite(created) && created > 0 ? created : Date.now();
};
const isInternalCompactionSummaryText = (text) => (
  String(text || '').trimStart().startsWith('[Context Compaction]')
);
const compactionSummaryDisplayText = (text) => {
  let value = String(text || '').replace(/\r\n?/g, '\n');
  const summaryMatch = value.match(/<SUMMARY_AND_NEXT_ACTIONS>\n?([\s\S]*?)\n?<\/SUMMARY_AND_NEXT_ACTIONS>/);
  if (summaryMatch) return summaryMatch[1].trim();
  value = value.replace(/^\s*\[Context Compaction\]\s*/, '');
  value = value.replace(/<PREVIOUS_TURNS>\n?[\s\S]*?\n?<\/PREVIOUS_TURNS>/g, '');
  return value.trim();
};
const lineCount = (text) => {
  const value = String(text || '').trim();
  return value ? value.split('\n').length : 0;
};
const responseCompactionMetadata = (data = {}) => {
  const seq = Number(data.compaction_seq ?? data.compactionSeq);
  const count = Number(data.compaction_count ?? data.compactionCount);
  return {
    compactionSeq: Number.isFinite(seq) ? seq : -1,
    compactionCount: Number.isFinite(count) ? count : 0
  };
};
const messageDedupeKey = (message) => {
  if (!message || typeof message !== 'object') return '';
  if (message.role === 'skill-run') {
    return JSON.stringify({ role: message.role, runId: message.runId || '' });
  }
  if (message.role === 'tool-group') {
    return JSON.stringify({
      role: message.role,
      status: message.status || '',
      tools: Array.isArray(message.tools)
        ? message.tools.map((tool) => ({
          name: tool?.name || '',
          arguments: tool?.arguments || '',
          status: tool?.status || '',
          images: Array.isArray(tool?.images) ? tool.images : []
        }))
        : []
    });
  }
  return JSON.stringify({
    role: message.role || '',
    content: message.content || '',
    attachments: Array.isArray(message.attachments)
      ? message.attachments.map((attachment) => ({
        name: attachment?.name || '',
        type: attachment?.type || '',
        dataURL: attachment?.dataURL || '',
        previewURL: attachment?.previewURL || ''
      }))
      : []
  });
};
const messageFingerprints = (messages, metrics = null) => (Array.isArray(messages) ? messages : []).map((message) => {
  if (metrics) metrics.fingerprints = Number(metrics.fingerprints || 0) + 1;
  return messageDedupeKey(message);
});
const longestCompactionTailOverlap = (fingerprints, markerIndex, start, metrics = null) => {
  const maxLength = Math.min(markerIndex, fingerprints.length - start);
  if (maxLength <= 0) return 0;
  const pattern = fingerprints.slice(start, start + maxLength);
  const sequence = pattern.concat([null], fingerprints.slice(0, markerIndex));
  const prefix = new Array(sequence.length).fill(0);
  const equalAt = (left, right) => {
    if (metrics) metrics.operations = Number(metrics.operations || 0) + 1;
    return sequence[left] === sequence[right];
  };
  for (let index = 1; index < sequence.length; index += 1) {
    let matched = prefix[index - 1];
    while (matched > 0 && !equalAt(index, matched)) {
      matched = prefix[matched - 1];
    }
    if (equalAt(index, matched)) matched += 1;
    prefix[index] = matched;
  }
  return Math.min(maxLength, prefix[prefix.length - 1]);
};
const isSyntheticCompactionAckMessage = (message) => (
  message?.role === 'assistant' && String(message.content || '').trim() === "I've reviewed the context summary. I'll continue from where we left off."
);
const compactionDuplicateTailRange = (messages, markerIndex, fingerprints = null, metrics = null) => {
  if (markerIndex <= 0 || markerIndex + 1 >= messages.length) return { start: -1, length: 0 };
  const keys = Array.isArray(fingerprints) && fingerprints.length === messages.length
    ? fingerprints
    : messageFingerprints(messages, metrics);
  const candidates = [markerIndex + 1];
  if (isSyntheticCompactionAckMessage(messages[markerIndex + 1])) candidates.push(markerIndex + 2);
  let bestStart = -1;
  let bestLength = 0;
  candidates.forEach((start) => {
    if (start >= messages.length) return;
    const length = longestCompactionTailOverlap(keys, markerIndex, start, metrics);
    if (length > bestLength) {
      bestStart = start;
      bestLength = length;
    }
  });
  return { start: bestStart, length: bestLength };
};
const suppressCompactionTailMessages = (messages) => {
  if (!Array.isArray(messages) || messages.length === 0) return messages;
  const out = messages.slice();
  const fingerprints = messageFingerprints(out);
  for (let index = 0; index < out.length; index += 1) {
    if (out[index]?.role !== 'compaction') continue;
    if (out[index]?.authoritativeTailSuppressed) continue;
    const { start, length } = compactionDuplicateTailRange(out, index, fingerprints);
    if (length > 0) {
      const removeCount = start + length - (index + 1);
      out.splice(index + 1, removeCount);
      fingerprints.splice(index + 1, removeCount);
    } else if (isSyntheticCompactionAckMessage(out[index + 1])) {
      out.splice(index + 1, 1);
      fingerprints.splice(index + 1, 1);
    }
  }
  return out;
};
const annotateCompactionBoundary = (messages, options = {}) => {
  const seq = Number(options.compactionSeq);
  if (!Number.isFinite(seq) || seq < 0 || !Array.isArray(messages) || messages.length === 0) {
    return messages;
  }
  const boundaryIndex = messages.findIndex((message) => {
    const messageSeq = Number(message?.serverSeq);
    return Number.isFinite(messageSeq) && messageSeq >= seq;
  });
  if (boundaryIndex < 0) return messages;
  const count = Number(options.compactionCount);
  const boundary = messages[boundaryIndex];
  if (boundary?.role === 'compaction') {
    boundary.activeBoundary = true;
    boundary.compactionSeq = seq;
    if (Number.isFinite(count) && count > 0) boundary.compactionCount = count;
    return messages;
  }
  const marker = {
    id: `compaction_boundary_${seq}`,
    role: 'compaction-boundary',
    content: 'Context compacted',
    activeBoundary: true,
    compactionSeq: seq,
    created: boundary?.created || Date.now()
  };
  if (Number.isFinite(count) && count > 0) marker.compactionCount = count;
  messages.splice(boundaryIndex, 0, marker);
  return messages;
};

const convertServerMessages = (serverMessages, options = {}) => {
  const result = [];
  let currentGroup = null;
  const askUserAnswersByGroup = new Map();
  let pendingCompactionMarkerIndex = -1;

  const normalizeImages = (images) => (
    Array.isArray(images)
      ? images.map((url) => rebaseMessageAssetURL(url)).filter(Boolean)
      : []
  );

  const durableSourceID = (msg) => {
    const id = msg?.id ?? msg?.ID;
    return id == null || id === '' ? null : id;
  };

  const addDurableSource = (entry, msg) => {
    if (!entry) return entry;
    const responseId = String(msg?.response_id || msg?.responseId || '').trim();
    if (responseId) entry.responseId = responseId;
    const clientMessageId = String(msg?.client_message_id || msg?.clientMessageId || '').trim();
    if (clientMessageId) entry.clientMessageId = clientMessageId;
    const segmentOrdinal = Number(msg?.assistant_segment_ordinal ?? msg?.assistantSegmentOrdinal);
    if (entry.role === 'assistant' && Number.isFinite(segmentOrdinal) && segmentOrdinal >= 0) {
      entry.assistantSegmentOrdinal = Math.trunc(segmentOrdinal);
      const startSequence = Number(msg?.segment_start_sequence ?? msg?.segmentStartSequence);
      const endSequence = Number(msg?.segment_end_sequence ?? msg?.segmentEndSequence);
      if (Number.isFinite(startSequence) && startSequence > 0) entry.segmentStartSequence = Math.trunc(startSequence);
      if (Number.isFinite(endSequence) && endSequence > 0) entry.segmentEndSequence = Math.trunc(endSequence);
    }
    const id = durableSourceID(msg);
    if (id == null) return entry;
    if (!Array.isArray(entry.durableSourceRowIds)) entry.durableSourceRowIds = [];
    if (!entry.durableSourceRowIds.includes(id)) entry.durableSourceRowIds.push(id);
    return entry;
  };

  const appendUniqueImages = (tool, images) => {
    if (!tool || images.length === 0) return;
    const existing = Array.isArray(tool.images) ? tool.images : [];
    images.forEach((url) => {
      if (url && !existing.includes(url)) existing.push(url);
    });
    if (existing.length > 0) tool.images = existing;
  };

  const flushGroup = () => {
    if (currentGroup) {
      const group = currentGroup;
      currentGroup = null;
      result.push(group);
      for (const answer of askUserAnswersByGroup.get(group) || []) result.push(answer);
      askUserAnswersByGroup.delete(group);
    }
  };

  const markAuthoritativeCompactionTailSuppressed = () => {
    if (pendingCompactionMarkerIndex < 0) return;
    const marker = result[pendingCompactionMarkerIndex];
    if (marker?.role === 'compaction') marker.authoritativeTailSuppressed = true;
  };

  const clearPendingCompactionTail = () => {
    pendingCompactionMarkerIndex = -1;
  };

  const toolGroupId = (msg, partIndex) => `${serverMessageBaseId(msg)}_tools_${partIndex}`;
  const fallbackToolId = (msg, partIndex) => `${serverMessageBaseId(msg)}_tool_${partIndex}`;

  const ensureToolGroup = (created, msg, partIndex) => {
    if (!currentGroup) {
      currentGroup = {
        id: toolGroupId(msg, partIndex),
        role: 'tool-group',
        tools: [],
        status: 'done',
        created,
        ...(serverMessageSequence(msg) !== null ? { serverSeq: serverMessageSequence(msg) } : {})
      };
    }
    addDurableSource(currentGroup, msg);
    return currentGroup;
  };

  const askUserAnswerEntry = (summary, callId, created, msg, partIndex) => addDurableSource({
    id: `${serverMessageBaseId(msg)}_ask_user_${partIndex}`,
    role: 'user', content: summary, askUser: true, askUserCallId: callId, created,
    ...(serverMessageSequence(msg) !== null ? { serverSeq: serverMessageSequence(msg) } : {})
  }, msg);

  const attachToolResultState = (part, created, msg, partIndex) => {
    const images = normalizeImages(part.images);
    const callId = part.tool_call_id || '';
    const failed = Boolean(part.tool_error || part.is_error);
    const spawnAgent = part.spawn_agent && typeof part.spawn_agent === 'object' ? part.spawn_agent : null;
    const askUserSummary = !failed && String(part.tool_name || '') === 'ask_user'
      ? String(part.ask_user_summary || '').trim()
      : '';
    let group = currentGroup;
    if (group) addDurableSource(group, msg);
    let tool = group && callId ? group.tools.find((entry) => entry.id === callId) : null;
    if (!tool && group && part.tool_name) tool = group.tools.find((entry) => entry.name === part.tool_name);
    // A page can contain the durable answer before the matching tool-call body.
    if (!tool && images.length === 0) {
      if (askUserSummary) result.push(askUserAnswerEntry(askUserSummary, callId, created, msg, partIndex));
      return;
    }
    if (!group) group = ensureToolGroup(created, msg, partIndex);
    if (!tool) {
      tool = {
        id: callId || fallbackToolId(msg, partIndex),
        name: part.tool_name || 'tool',
        arguments: '',
        status: 'done',
        created
      };
      group.tools.push(tool);
    }
    tool.status = failed ? 'error' : 'done';
    tool.resultStatus = failed ? 'error' : 'success';
    if (spawnAgent) {
      tool.subagent = {
        agentName: String(spawnAgent.agent_name || ''),
        output: String(spawnAgent.output || ''),
        error: String(spawnAgent.error || ''),
        errorType: String(spawnAgent.type || ''),
        durationMs: Number(spawnAgent.duration_ms || 0),
        childSessionId: String(spawnAgent.session_id || '')
      };
    }
    appendUniqueImages(tool, images);
    if (askUserSummary) {
      const answers = askUserAnswersByGroup.get(group) || [];
      answers.push(askUserAnswerEntry(askUserSummary, callId, created, msg, partIndex));
      askUserAnswersByGroup.set(group, answers);
    }
  };

  for (const msg of serverMessages) {
    const parts = Array.isArray(msg.parts) ? msg.parts : [];
    const created = serverMessageCreatedAt(msg);
    const baseId = serverMessageBaseId(msg);
    const seq = serverMessageSequence(msg);

    if (msg.role === 'system' || msg.role === 'developer') continue;
    if (msg.compaction_tail || msg.compactionTail) {
      flushGroup();
      markAuthoritativeCompactionTailSuppressed();
      continue;
    }
    if (msg.transcriptEmptyBody && msg.role !== 'tool') {
      if (msg.role === 'user' || msg.role === 'event') flushGroup();
      continue;
    }
    clearPendingCompactionTail();

    if (msg.role === 'event') {
      flushGroup();
      const skillMarker = parts.find((part) => part.type === 'skill_activation' && part.skill_activation?.run_id);
      if (skillMarker) {
        const provenance = skillMarker.skill_activation;
        const textMarker = parts.find((part) => part.type === 'text');
        const text = String(textMarker?.text || '');
        const outputBreak = text.indexOf('\n\n');
        const started = Date.parse(provenance.started_at || '');
        const completed = Date.parse(provenance.completed_at || '');
        const skillRun = {
          id: `skill-run-${provenance.run_id}`,
          role: 'skill-run',
          runId: provenance.run_id,
          skill: provenance.name || 'skill',
          agent: provenance.agent || '',
          status: provenance.status || 'running',
          output: outputBreak >= 0 ? text.slice(outputBreak + 2) : '',
          childSessionId: provenance.child_session_id || '',
          durationMs: Number.isFinite(started) && Number.isFinite(completed) && completed >= started ? completed - started : 0,
          provenance,
          created,
          ...(seq !== null ? { serverSeq: seq } : {})
        };
        addDurableSource(skillRun, msg);
        const previousIndex = result.findIndex((entry) => entry.role === 'skill-run' && entry.runId === provenance.run_id);
        if (previousIndex >= 0) {
          const previous = result[previousIndex];
          if (!Array.isArray(skillRun.durableSourceRowIds)) skillRun.durableSourceRowIds = [];
          for (const id of previous?.durableSourceRowIds || []) {
            if (!skillRun.durableSourceRowIds.includes(id)) skillRun.durableSourceRowIds.unshift(id);
          }
          result[previousIndex] = skillRun;
        } else result.push(skillRun);
        continue;
      }
      const errorMarker = parts.find((part) => part.type === 'error');
      if (errorMarker) {
        result.push(addDurableSource({
          id: baseId,
          role: 'error',
          content: errorMarker.text || 'The response failed.',
          created,
          ...(seq !== null ? { serverSeq: seq } : {})
        }, msg));
        continue;
      }
      const marker = parts.find((part) => part.type === 'model_swap') || parts.find((part) => part.type === 'text');
      result.push(addDurableSource({
        id: baseId,
        role: 'model-swap',
        content: marker?.text || '↔ Model switch',
        created,
        ...(seq !== null ? { serverSeq: seq } : {})
      }, msg));
      continue;
    }

    if (msg.role === 'path-note') {
      flushGroup();
      const note = parts.find((part) => part.type === 'path_note');
      result.push(addDurableSource({
        id: baseId,
        role: 'path-note',
        content: String(note?.text || ''),
        provenance: note?.path_note || null,
        created,
        ...(seq !== null ? { serverSeq: seq } : {})
      }, msg));
      continue;
    }

    if (msg.role === 'user') {
      flushGroup();

      const attachments = [];
      const textParts = [];
      const diffComments = [];
      for (const part of parts) {
        if (part.type === 'diff_comment' && part.diff_comment) {
          diffComments.push(part.diff_comment);
        } else if (part.type === 'image' && part.image_url) {
          const width = Number(part.width), height = Number(part.height), validDimensions = Number.isFinite(width) && Number.isFinite(height) && width > 0 && height > 0;
          attachments.push({ name: 'image', type: part.mime_type || 'image/*', dataURL: rebaseMessageAssetURL(part.image_url), ...(validDimensions ? { width: Math.round(width), height: Math.round(height) } : {}) });
        } else if (part.type === 'file' && part.text) {
          attachments.push({
            name: part.text,
            type: 'text/plain',
            mention: true
          });
        } else if (part.type === 'text' && part.text) {
          textParts.push(part.text);
        }
      }

      const content = textParts.join('\n');
      if (isInternalCompactionSummaryText(content)) {
        result.push(addDurableSource({
          id: baseId,
          role: 'compaction',
          content: 'Context compacted',
          rawContent: content,
          lineCount: lineCount(compactionSummaryDisplayText(content)),
          created,
          ...(seq !== null ? { serverSeq: seq, compactionSeq: seq } : {})
        }, msg));
        pendingCompactionMarkerIndex = result.length - 1;
        continue;
      }

      result.push(addDurableSource({
        id: baseId,
        role: 'user',
        content,
        created,
        ...(seq !== null ? { serverSeq: seq } : {}),
        ...(attachments.length > 0 ? { attachments } : {}),
        ...(diffComments.length > 0 ? { diffComments } : {})
      }, msg));
      continue;
    }
    // Display-only provider activities happened before the final answer.
    const indexedParts = parts.map((part, index) => ({ part, index }));
    const activities = indexedParts.filter(({ part }) => part.type === 'tool_activity');
    const assistantParts = activities.length ? activities.concat(indexedParts.filter(({ part }) => part.type !== 'tool_activity')) : indexedParts;
    // Walk through assistant parts in order to preserve interleaving with tool calls.
    for (const { part, index: partIndex } of assistantParts) {
      if (part.type === 'text' && part.text && String(part.text).trim() !== '') {
        flushGroup();
        result.push(addDurableSource({
          id: `${baseId}_text_${partIndex}`,
          role: 'assistant',
          content: part.text,
          created,
          ...(seq !== null ? { serverSeq: seq } : {})
        }, msg));
      } else if (part.type === 'tool_call' || part.type === 'tool_activity') {
        const group = ensureToolGroup(created, msg, partIndex);
        const toolId = part.tool_call_id || fallbackToolId(msg, partIndex);
        const isActivity = part.type === 'tool_activity', failed = Boolean(part.tool_error) || String(part.tool_status || '').toLowerCase() === 'failed';
        const status = failed ? 'error' : 'done';
        let toolEntry = group.tools.find((entry) => entry.id === toolId);
        if (!toolEntry) {
          toolEntry = { id: toolId, name: part.tool_name || 'tool', arguments: part.tool_arguments || '', status, created };
          group.tools.push(toolEntry);
        } else {
          toolEntry.name = part.tool_name || toolEntry.name || 'tool';
          toolEntry.arguments = part.tool_arguments || toolEntry.arguments || '';
          toolEntry.status = status;
        }
        if (failed || isActivity) toolEntry.resultStatus = failed ? 'error' : 'success';
        appendUniqueImages(toolEntry, normalizeImages(part.images));
      } else if (part.type === 'tool_result') {
        attachToolResultState(part, created, msg, partIndex);
      }
    }
  }

  flushGroup();
  return annotateCompactionBoundary(suppressCompactionTailMessages(result), options);
};

Object.assign(app, {
  safeServerIdToken,
  serverMessageRawKey,
  serverMessageBaseId,
  serverMessageSequence,
  serverMessageCreatedAt,
  isInternalCompactionSummaryText,
  compactionSummaryDisplayText,
  lineCount,
  responseCompactionMetadata,
  messageDedupeKey,
  messageFingerprints,
  longestCompactionTailOverlap,
  isSyntheticCompactionAckMessage,
  compactionDuplicateTailRange,
  suppressCompactionTailMessages,
  annotateCompactionBoundary,
  convertServerMessages
});
})();
