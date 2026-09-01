import type { Attachment, DiffComment, GuardianReview, Message, Session, ToolCall } from './types';

const randomID = (prefix: string): string =>
  `${prefix}_${globalThis.crypto?.randomUUID?.() || Math.random().toString(36).slice(2)}`;
const timestamp = (value: unknown): number => {
  const numeric = Number(value);
  if (Number.isFinite(numeric) && numeric > 0)
    return numeric < 10_000_000_000 ? numeric * 1000 : numeric;
  const parsed = typeof value === 'string' ? Date.parse(value) : Number.NaN;
  return Number.isFinite(parsed) ? parsed : Date.now();
};
const text = (value: unknown): string =>
  typeof value === 'string' ? value : value == null ? '' : String(value);
const record = (value: unknown): Record<string, unknown> | null =>
  Boolean(value) && typeof value === 'object' ? (value as Record<string, unknown>) : null;

export interface ServerPart {
  type?: string;
  [key: string]: unknown;
}
export interface ServerMessage {
  id?: string | number;
  ID?: string | number;
  sequence?: number;
  role?: string;
  created_at?: number | string;
  response_id?: string;
  responseId?: string;
  client_message_id?: string;
  clientMessageId?: string;
  assistant_segment_ordinal?: number;
  assistantSegmentOrdinal?: number;
  segment_start_sequence?: number;
  segmentStartSequence?: number;
  segment_end_sequence?: number;
  segmentEndSequence?: number;
  parts?: ServerPart[];
  compaction_tail?: boolean;
  transcriptEmptyBody?: boolean;
  [key: string]: unknown;
}
export interface ConvertOptions {
  rebaseAssetURL?: (value: string) => string;
  compactionSeq?: number;
  compactionCount?: number;
}

export function safeServerID(value: unknown): string {
  return text(value)
    .trim()
    .replace(/[^A-Za-z0-9_-]/g, '_');
}
function baseID(message: ServerMessage): string {
  if (Number.isFinite(Number(message.sequence))) return `srv_seq_${Number(message.sequence)}`;
  const token = safeServerID(message.id ?? message.ID);
  return token ? `srv_${token}` : randomID('msg');
}
function created(message: ServerMessage): number {
  return timestamp(message.created_at);
}
function sourceRowID(message: ServerMessage): string | number | null {
  const value = message.id ?? message.ID;
  return value == null || value === '' ? null : value;
}
function withSource(entry: Message, message: ServerMessage): Message {
  const responseID = message.response_id ?? message.responseId;
  const clientMessageID = message.client_message_id ?? message.clientMessageId;
  const segmentOrdinal = message.assistant_segment_ordinal ?? message.assistantSegmentOrdinal;
  const segmentStart = message.segment_start_sequence ?? message.segmentStartSequence;
  const segmentEnd = message.segment_end_sequence ?? message.segmentEndSequence;
  if (responseID) entry.responseId = text(responseID);
  if (clientMessageID) entry.clientMessageId = text(clientMessageID);
  if (message.interrupt_state) entry.interruptState = text(message.interrupt_state).toLowerCase();
  if (entry.role === 'assistant' && Number.isFinite(Number(segmentOrdinal)))
    entry.assistantSegmentOrdinal = Math.max(0, Math.trunc(Number(segmentOrdinal)));
  if (entry.role === 'assistant' && Number.isFinite(Number(segmentStart)))
    entry.segmentStartSequence = Math.max(0, Math.trunc(Number(segmentStart)));
  if (entry.role === 'assistant' && Number.isFinite(Number(segmentEnd)))
    entry.segmentEndSequence = Math.max(0, Math.trunc(Number(segmentEnd)));
  const row = sourceRowID(message);
  if (row != null) {
    entry.durableSourceRowIds = [
      ...new Set([...((entry.durableSourceRowIds as Array<string | number>) || []), row]),
    ];
    if (
      ['user', 'assistant', 'tool-group'].includes(entry.role) &&
      Number.isSafeInteger(Number(row)) &&
      Number(row) > 0
    )
      entry.durableRowId = Number(row);
  }
  if (Number.isFinite(Number(message.sequence))) entry.serverSeq = Number(message.sequence);
  return entry;
}

export function compactionSummaryDisplayText(value: string): string {
  let source = String(value || '').replace(/\r\n?/g, '\n');
  const summary = source.match(
    /<SUMMARY_AND_NEXT_ACTIONS>\n?([\s\S]*?)\n?<\/SUMMARY_AND_NEXT_ACTIONS>/,
  );
  if (summary) return summary[1].trim();
  source = source
    .replace(/^\s*\[Context Compaction\]\s*/, '')
    .replace(/<PREVIOUS_TURNS>\n?[\s\S]*?\n?<\/PREVIOUS_TURNS>/g, '');
  return source.trim();
}
function messageFingerprint(message: Message): string {
  return JSON.stringify({
    role: message.role,
    content: message.content,
    client: message.clientMessageId || '',
    tools:
      message.tools?.map((tool) => [tool.name, tool.arguments, tool.status, tool.images]) || [],
    attachments:
      message.attachments?.map((item) => [item.name, item.type, item.url || item.dataURL]) || [],
  });
}
export function suppressCompactionTail(messages: Message[]): Message[] {
  const output = [...messages];
  for (let marker = 0; marker < output.length; marker += 1) {
    if (output[marker].role !== 'compaction') continue;
    let start = marker + 1;
    if (
      output[start]?.role === 'assistant' &&
      output[start].content.trim() ===
        "I've reviewed the context summary. I'll continue from where we left off."
    )
      start += 1;
    const suffix = output.slice(start).map(messageFingerprint);
    const prefix = output.slice(0, marker).map(messageFingerprint);
    let overlap = Math.min(suffix.length, prefix.length);
    while (
      overlap > 0 &&
      suffix.slice(0, overlap).join('\u0000') !==
        prefix.slice(prefix.length - overlap).join('\u0000')
    )
      overlap -= 1;
    if (overlap > 0) output.splice(marker + 1, start - marker - 1 + overlap);
    else if (start > marker + 1) output.splice(marker + 1, start - marker - 1);
  }
  return output;
}
export function annotateCompactionBoundary(
  messages: Message[],
  sequence?: number,
  count?: number,
): Message[] {
  if (!Number.isFinite(sequence) || Number(sequence) < 0 || !messages.length) return messages;
  const index = messages.findIndex((message) => Number(message.serverSeq) >= Number(sequence));
  if (index < 0) return messages;
  const current = messages[index];
  if (current.role === 'compaction') {
    current.activeBoundary = true;
    current.compactionSeq = sequence;
    current.compactionCount = count;
    return messages;
  }
  messages.splice(index, 0, {
    id: `compaction_boundary_${sequence}`,
    role: 'compaction-boundary',
    content: 'Context compacted',
    activeBoundary: true,
    compactionSeq: sequence,
    compactionCount: count,
    created: current.created,
  });
  return messages;
}

function guardianReviews(value: unknown): GuardianReview[] | undefined {
  if (!Array.isArray(value)) return undefined;
  return value
    .filter((entry) => record(entry))
    .map((entry) => {
      const source = entry as Record<string, unknown>;
      return {
        outcome: text(source.outcome),
        message: text(source.message),
        model: text(source.model),
        tool: text(source.tool),
        command: text(source.command),
        path: text(source.path),
        is_write: Boolean(source.is_write),
        workdir: text(source.workdir),
      };
    });
}
function attachmentsFromUser(
  parts: ServerPart[],
  rebase: (value: string) => string,
): { attachments: Attachment[]; comments: DiffComment[]; content: string } {
  const attachments: Attachment[] = [];
  const comments: DiffComment[] = [];
  const content: string[] = [];
  for (const part of parts) {
    if (part.type === 'diff_comment' && record(part.diff_comment)) {
      const source = record(part.diff_comment)!;
      comments.push({
        id: text(source.id),
        path: text(source.path),
        side: text(source.side) === 'old' ? 'old' : 'new',
        line: Number(source.line) || 0,
        body: text(source.instruction || source.body),
        scope: text(source.scope),
        context: text(source.line_text || source.context),
        fileChangeSeq: Number(source.file_change_seq || source.fileChangeSeq) || 0,
      });
    } else if (part.type === 'image' && part.image_url) {
      const width = Number(part.width);
      const height = Number(part.height);
      const valid = width > 0 && height > 0;
      attachments.push({
        name: text(part.filename || part.name) || 'image',
        type: text(part.mime_type) || 'image/*',
        url: rebase(text(part.image_url)),
        previewURL: rebase(text(part.image_url)),
        ...(valid ? { width: Math.round(width), height: Math.round(height) } : {}),
      });
    } else if (
      ['file', 'audio', 'video'].includes(text(part.type)) &&
      (part.file_url || part.audio_url || part.video_url || part.url || part.text)
    ) {
      const url = rebase(text(part.file_url || part.audio_url || part.video_url || part.url));
      attachments.push({
        name: text(part.filename || part.name || part.text) || text(part.type),
        type:
          text(part.mime_type || part.media_type) ||
          (part.type === 'file' ? 'text/plain' : `${part.type}/*`),
        url,
        previewURL: url,
        mention: Boolean(part.text && !url),
      });
    } else if (['text', 'output_text'].includes(text(part.type)) && part.text)
      content.push(text(part.text));
  }
  return { attachments, comments, content: content.join('\n') };
}

export function convertServerMessages(
  input: ServerMessage[],
  options: ConvertOptions = {},
): Message[] {
  const output: Message[] = [];
  const rebase = options.rebaseAssetURL || ((value: string) => value);
  let group: Message | null = null;
  let pendingCompaction = -1;
  const flush = () => {
    if (!group) return;
    const current = group;
    group = null;
    output.push(current);
  };
  const ensureGroup = (message: ServerMessage, partIndex: number): Message => {
    if (!group)
      group = withSource(
        {
          id: `${baseID(message)}_tools_${partIndex}`,
          role: 'tool-group',
          content: '',
          tools: [],
          status: 'done',
          created: created(message),
        },
        message,
      );
    else withSource(group, message);
    return group;
  };
  const findTool = (current: Message, callID: string, name: string): ToolCall | undefined =>
    current.tools?.find((tool) => (callID ? tool.id === callID : tool.name === name));

  for (const message of input || []) {
    const role = text(message.role);
    const parts = Array.isArray(message.parts) ? message.parts : [];
    const messageID = baseID(message);
    const at = created(message);
    if (role === 'system' || role === 'developer') continue;
    if (message.compaction_tail || message.compactionTail) {
      flush();
      if (pendingCompaction >= 0) output[pendingCompaction].authoritativeTailSuppressed = true;
      continue;
    }
    if (message.transcriptEmptyBody && role !== 'tool') {
      if (role === 'user' || role === 'event') flush();
      continue;
    }
    pendingCompaction = -1;
    if (role === 'event') {
      flush();
      const skillPart = parts.find(
        (part) => part.type === 'skill_activation' && record(part.skill_activation)?.run_id,
      );
      if (skillPart) {
        const provenance = record(skillPart.skill_activation)!;
        const marker = parts.find((part) => part.type === 'text');
        const markerText = text(marker?.text);
        const split = markerText.indexOf('\n\n');
        const entry = withSource(
          {
            id: `skill-run-${text(provenance.run_id)}`,
            role: 'skill-run',
            content: split >= 0 ? markerText.slice(split + 2) : '',
            runId: text(provenance.run_id),
            skill: text(provenance.name) || 'skill',
            agent: text(provenance.agent),
            status: text(provenance.status) || 'running',
            childSessionId: text(provenance.child_session_id),
            created: at,
          },
          message,
        );
        const previous = output.findIndex(
          (candidate) => candidate.role === 'skill-run' && candidate.runId === entry.runId,
        );
        if (previous >= 0) output[previous] = entry;
        else output.push(entry);
        continue;
      }
      const error = parts.find((part) => part.type === 'error');
      if (error)
        output.push(
          withSource(
            {
              id: messageID,
              role: 'error',
              content: text(error.text) || 'The response failed.',
              created: at,
            },
            message,
          ),
        );
      else {
        const marker =
          parts.find((part) => part.type === 'model_swap') ||
          parts.find((part) => part.type === 'text');
        output.push(
          withSource(
            {
              id: messageID,
              role: 'model-swap',
              content: text(marker?.text) || 'Model switched',
              created: at,
            },
            message,
          ),
        );
      }
      continue;
    }
    if (role === 'path-note') {
      flush();
      const part = parts.find((candidate) => candidate.type === 'path_note');
      output.push(
        withSource(
          {
            id: messageID,
            role: 'path-note',
            content: text(part?.text),
            provenance: part?.path_note || null,
            created: at,
          },
          message,
        ),
      );
      continue;
    }
    if (role === 'compaction' || role === 'compaction-boundary') {
      flush();
      const content = parts
        .filter((part) => ['text', 'output_text'].includes(text(part.type)))
        .map((part) => text(part.text))
        .join('\n');
      output.push(
        withSource(
          {
            id: messageID,
            role: 'compaction-boundary',
            content: 'Context compacted',
            rawContent: content,
            lineCount: content.split('\n').filter(Boolean).length,
            created: at,
          },
          message,
        ),
      );
      continue;
    }
    if (role === 'user') {
      flush();
      const value = attachmentsFromUser(parts, rebase);
      if (value.content.trimStart().startsWith('[Context Compaction]')) {
        output.push(
          withSource(
            {
              id: messageID,
              role: 'compaction',
              content: 'Context compacted',
              rawContent: value.content,
              lineCount: compactionSummaryDisplayText(value.content).split('\n').filter(Boolean)
                .length,
              created: at,
            },
            message,
          ),
        );
        pendingCompaction = output.length - 1;
      } else
        output.push(
          withSource(
            {
              id: messageID,
              role: 'user',
              content: value.content,
              attachments: value.attachments.length ? value.attachments : undefined,
              diffComments: value.comments.length ? value.comments : undefined,
              created: at,
            },
            message,
          ),
        );
      continue;
    }
    const activities = parts
      .map((part, index) => ({ part, index }))
      .filter(({ part }) => part.type === 'tool_activity');
    const ordered = activities.length
      ? [
          ...activities,
          ...parts
            .map((part, index) => ({ part, index }))
            .filter(({ part }) => part.type !== 'tool_activity'),
        ]
      : parts.map((part, index) => ({ part, index }));
    for (const { part, index } of ordered) {
      if (['text', 'output_text'].includes(text(part.type)) && text(part.text).trim()) {
        flush();
        output.push(
          withSource(
            {
              id: `${messageID}_text_${index}`,
              role: 'assistant',
              content: text(part.text),
              created: at,
            },
            message,
          ),
        );
        continue;
      }
      if (['tool_call', 'tool_activity', 'function_call'].includes(text(part.type))) {
        const current = ensureGroup(message, index);
        const callID =
          text(part.tool_call_id || part.call_id || part.id) || `${messageID}_tool_${index}`;
        const failed =
          Boolean(part.tool_error) ||
          ['failed', 'error'].includes(text(part.tool_status).toLowerCase());
        let tool = findTool(current, callID, text(part.tool_name || part.name));
        if (!tool) {
          const name = text(part.tool_name || part.name) || 'tool';
          // Most successful tool results are omitted from history, but spawn_agent results are
          // retained, so a persisted spawn can remain running until its matching result arrives.
          const awaitsResult =
            part.type === 'function_call' || (part.type === 'tool_call' && name === 'spawn_agent');
          tool = {
            id: callID,
            name,
            arguments: text(part.tool_arguments || part.arguments),
            status: failed ? 'error' : awaitsResult ? 'running' : 'done',
          };
          current.tools!.push(tool);
        } else
          Object.assign(tool, {
            name: text(part.tool_name || part.name) || tool.name,
            arguments: text(part.tool_arguments || part.arguments) || tool.arguments,
            status: failed ? 'error' : tool.status,
          });
        tool.resultStatus = failed
          ? 'error'
          : part.type === 'tool_activity'
            ? 'success'
            : tool.resultStatus;
        tool.guardianReviews = guardianReviews(part.guardian_reviews) || tool.guardianReviews;
        const images = Array.isArray(part.images)
          ? part.images.map((url) => rebase(text(url))).filter(Boolean)
          : [];
        if (images.length) tool.images = [...new Set([...(tool.images || []), ...images])];
        continue;
      }
      if (part.type === 'tool_result') {
        const current = group;
        const callID = text(part.tool_call_id || part.call_id);
        const name = text(part.tool_name || part.name);
        const images = Array.isArray(part.images)
          ? part.images.map((url) => rebase(text(url))).filter(Boolean)
          : [];
        let tool = current ? findTool(current, callID, name) : undefined;
        const reviews = guardianReviews(part.guardian_reviews);
        const askAnswer =
          callID && !part.tool_error && name === 'ask_user'
            ? text(part.ask_user_summary).trim()
            : '';
        if (!tool && !images.length && !reviews?.length && !askAnswer) continue;
        const target = current || ensureGroup(message, index);
        tool ||= {
          id: callID || `${messageID}_tool_${index}`,
          name: name || 'tool',
          status: 'done',
        };
        if (!target.tools!.includes(tool)) target.tools!.push(tool);
        tool.status = part.tool_error || part.is_error ? 'error' : 'done';
        tool.resultStatus = tool.status === 'error' ? 'error' : 'success';
        tool.result = text(part.output || part.result || part.tool_info || part.text);
        tool.guardianReviews = reviews || tool.guardianReviews;
        if (askAnswer && tool.name === 'ask_user') tool.askUserAnswer = askAnswer;
        if (images.length) tool.images = [...new Set([...(tool.images || []), ...images])];
        const spawn = record(part.spawn_agent);
        if (spawn)
          tool.subagent = {
            agentName: text(spawn.agent_name),
            output: text(spawn.output),
            error: text(spawn.error),
            errorType: text(spawn.type),
            durationMs: Number(spawn.duration_ms) || 0,
            childSessionId: text(spawn.session_id),
          };
      }
    }
  }
  flush();
  return annotateCompactionBoundary(
    suppressCompactionTail(output),
    options.compactionSeq,
    options.compactionCount,
  );
}

export function sanitizeSession(
  source: Record<string, unknown>,
  options: ConvertOptions = {},
): Session {
  const messages = Array.isArray(source.messages)
    ? convertServerMessages(source.messages as ServerMessage[], options)
    : [];
  const archived = source.archived_at ? true : Boolean(source.archived);
  const fileSummary = record(source.file_change_summary || source.fileChangeSummary);
  const rawTranscriptRev = source.transcript_rev ?? source.rev;
  const transcriptRev = Number(rawTranscriptRev);
  const attentionSeq = Number(source.attention_seq ?? source.latest_attention_seq);
  const seenThroughSeq = Number(source.seen_through_seq);
  const attentionFinalRev = Number(source.attention_final_rev ?? source.final_rev);
  return {
    id: text(source.id) || randomID('sess'),
    number: Number(source.number || source.session_number) || undefined,
    name: text(source.name),
    title:
      text(
        source.short_title ||
          source.name ||
          source.title ||
          source.generated_short_title ||
          source.summary,
      ) || 'New chat',
    longTitle: text(source.long_title || source.longTitle || source.generated_long_title),
    generatedShortTitle: text(source.generated_short_title || source.generatedShortTitle),
    generatedLongTitle: text(source.generated_long_title || source.generatedLongTitle),
    mode: text(source.mode) || 'chat',
    origin: text(source.origin) || 'web',
    agent: text(source.agent),
    archived,
    pinned: Boolean(source.pinned),
    created: timestamp(source.created_at || source.created),
    lastMessageAt: timestamp(source.last_message_at || source.updated_at || source.created_at),
    lastResponseId: text(source.last_response_id || source.lastResponseId) || null,
    activeResponseId: text(source.active_response_id || source.activeResponseId) || null,
    activeModel: text(source.active_model || source.model),
    activeProvider: text(source.active_provider || source.provider_key || source.provider),
    activeEffort: text(source.active_effort || source.effort),
    activeReasoningMode: text(source.active_reasoning_mode),
    projectId: text(source.project_id),
    projectName: text(source.project_name),
    projectUnavailable: Boolean(source.project_unavailable),
    projectUnavailableReason: text(source.project_unavailable_reason),
    workingDir: text(source.working_dir || source.cwd),
    worktreeDir: text(source.worktree_dir),
    messages,
    messageCount:
      Number(source.message_count) ||
      messages.filter((message) => ['user', 'assistant'].includes(message.role)).length,
    ...(rawTranscriptRev != null && rawTranscriptRev !== '' && Number.isFinite(transcriptRev)
      ? { transcriptRev: Math.max(0, transcriptRev) }
      : {}),
    ...((source.attention_store_instance_id ?? source.store_instance_id) != null
      ? {
          attentionStoreInstanceId: text(
            source.attention_store_instance_id || source.store_instance_id,
          ),
        }
      : {}),
    ...(Number.isSafeInteger(attentionSeq) && attentionSeq >= 0 ? { attentionSeq } : {}),
    ...(source.attention_response_id != null
      ? { attentionResponseId: text(source.attention_response_id) }
      : {}),
    ...(Number.isSafeInteger(attentionFinalRev) && attentionFinalRev >= 0
      ? { attentionFinalRev }
      : {}),
    ...(Number.isSafeInteger(seenThroughSeq) && seenThroughSeq >= 0 ? { seenThroughSeq } : {}),
    ...(source.attention_unseen !== undefined
      ? { attentionUnseen: Boolean(source.attention_unseen) }
      : {}),
    ...(source.attention_outcome != null
      ? { attentionOutcome: text(source.attention_outcome) }
      : {}),
    ...((source.attention_terminal_at ?? source.terminal_at) != null
      ? { attentionTerminalAt: timestamp(source.attention_terminal_at ?? source.terminal_at) }
      : {}),
    ...(source.interaction_required !== undefined
      ? { interactionRequired: Boolean(source.interaction_required) }
      : {}),
    ...(source.interaction_response_id != null
      ? { interactionResponseId: text(source.interaction_response_id) }
      : {}),
    ...(Number.isSafeInteger(Number(source.interaction_state_rev)) &&
    Number(source.interaction_state_rev) >= 0
      ? { interactionStateRev: Number(source.interaction_state_rev) }
      : {}),
    ...(Number.isSafeInteger(Number(source.pending_interaction_count)) &&
    Number(source.pending_interaction_count) >= 0
      ? { pendingInteractionCount: Number(source.pending_interaction_count) }
      : {}),
    ...(Array.isArray(source.pending_interaction_kinds)
      ? {
          pendingInteractionKinds: source.pending_interaction_kinds
            .map((kind) => text(kind))
            .filter(Boolean),
        }
      : {}),
    ...(source.interaction_required_since != null
      ? { interactionRequiredSince: timestamp(source.interaction_required_since) }
      : {}),
    usage: (record(source.usage) as Session['usage']) || undefined,
    goal: (record(source.goal) as Session['goal']) || null,
    fileChangeSummary: fileSummary
      ? {
          fileCount: Number(fileSummary.file_count ?? fileSummary.fileCount) || 0,
          additions: Number(fileSummary.adds ?? fileSummary.additions) || 0,
          deletions: Number(fileSummary.dels ?? fileSummary.deletions) || 0,
          git: Boolean(fileSummary.git),
        }
      : undefined,
    mcpServers: Array.isArray(source.mcp_servers) ? source.mcp_servers.map(String) : [],
    mcpEnabled: Array.isArray(source.mcp_enabled) ? source.mcp_enabled.map(String) : [],
  };
}

export interface TranscriptRowContext {
  index: number;
  responseText: string;
  copyTarget: boolean;
}
export function indexTranscriptTurns(
  messages: Message[],
  messageText: (message: Message) => string,
): Map<Message, TranscriptRowContext> {
  const result = new Map<Message, TranscriptRowContext>();
  let start = 0;
  while (start < messages.length) {
    let end = start + 1;
    while (end < messages.length && messages[end].role !== 'user') end += 1;
    const assistantParts: { responseId?: string; text: string }[] = [];
    let copyTarget = -1;
    for (let index = start; index < end; index += 1) {
      const message = messages[index];
      const value = messageText(message);
      if (message.role === 'assistant' && value)
        assistantParts.push({ responseId: message.responseId, text: value });
      if (message.role === 'assistant' && message.content.trim()) copyTarget = index;
    }
    const responseId = copyTarget >= 0 ? messages[copyTarget].responseId : undefined;
    const responseText = assistantParts
      .filter((part) => !responseId || part.responseId === responseId)
      .map((part) => part.text)
      .join('\n\n');
    for (let index = start; index < end; index += 1)
      result.set(messages[index], { index, responseText, copyTarget: index === copyTarget });
    start = end;
  }
  return result;
}

export interface TranscriptRun {
  type: 'gap' | 'messages';
  key: string;
  messages?: Message[];
  count?: number;
  height?: number;
}
export function windowTranscript(
  messages: Message[],
  maxTurns = 80,
  _nearTail = true,
): TranscriptRun[] {
  if (messages.length <= maxTurns) return [{ type: 'messages', key: 'all', messages }];
  let start = messages.length;
  let turns = 0;
  while (start > 0 && turns < maxTurns) {
    start -= 1;
    if (messages[start].role === 'user') turns += 1;
  }
  const visible = messages.slice(start);
  return [
    {
      type: 'gap',
      key: `gap-${start}`,
      count: start,
      height: Math.max(72, Math.min(12_000, start * 48)),
    },
    { type: 'messages', key: `tail-${visible[0]?.id || 'empty'}`, messages: visible },
  ];
}

export function mergeDurableProjection(durable: Message[], projected: Message[]): Message[] {
  const compactionSequence = (message: Message): number | null => {
    // Persisted summary rows expose their durable identity as serverSeq;
    // recovery/live boundaries carry the same value as compactionSeq.
    const value = Number(message.compactionSeq ?? message.serverSeq);
    return Number.isSafeInteger(value) && value >= 0 ? value : null;
  };
  const durableCompactions = new Map<number, Message>();
  for (const message of durable) {
    if (message.role !== 'compaction' && message.role !== 'compaction-boundary') continue;
    const sequence = compactionSequence(message);
    if (sequence != null) durableCompactions.set(sequence, message);
  }
  const adoptedCompactions = new Map<Message, Message>();
  const adoptedDurableCompactions = new Set<Message>();
  for (const message of projected) {
    if (message.role !== 'compaction' && message.role !== 'compaction-boundary') continue;
    const sequence = compactionSequence(message);
    const durableMessage = sequence == null ? undefined : durableCompactions.get(sequence);
    if (!durableMessage || adoptedDurableCompactions.has(durableMessage)) continue;
    adoptedCompactions.set(message, durableMessage);
    adoptedDurableCompactions.add(durableMessage);
  }
  const clientIDs = new Set(durable.map((message) => message.clientMessageId).filter(Boolean));
  const responseSegments = new Map(
    durable
      .filter((message) => message.role === 'assistant' && message.responseId)
      .map((message) => [`${message.responseId}:${message.assistantSegmentOrdinal || 0}`, message]),
  );
  const allToolIDs = new Set(
    durable.flatMap((message) => message.tools || []).map((tool) => tool.id),
  );
  const legacyToolIDs = new Set(
    durable
      .filter((message) => !message.responseId)
      .flatMap((message) => message.tools || [])
      .map((tool) => tool.id),
  );
  const scopedToolIDs = new Set(
    durable.flatMap((message) =>
      message.responseId
        ? (message.tools || []).map((tool) => `${message.responseId}:${tool.id}`)
        : [],
    ),
  );
  const hasDurableTool = (message: Message, tool: ToolCall): boolean =>
    message.responseId
      ? scopedToolIDs.has(`${message.responseId}:${tool.id}`) || legacyToolIDs.has(tool.id)
      : allToolIDs.has(tool.id);
  const pending = projected.filter((message) => {
    if (
      message.role === 'user' &&
      message.clientMessageId &&
      clientIDs.has(message.clientMessageId)
    )
      return false;
    if (message.role === 'assistant' && message.responseId) {
      const durableSegment = responseSegments.get(
        `${message.responseId}:${message.assistantSegmentOrdinal || 0}`,
      );
      if (durableSegment)
        return (
          Number(message.segmentEndSequence || 0) > Number(durableSegment.segmentEndSequence || 0)
        );
    }
    if (
      message.role === 'tool-group' &&
      message.tools?.length &&
      message.tools.every((tool) => hasDurableTool(message, tool))
    )
      return false;
    return true;
  });
  const insertBefore = new Map<Message, Message>();
  let nextCompaction: Message | undefined;
  for (let index = pending.length - 1; index >= 0; index -= 1) {
    const message = pending[index];
    const adoptedCompaction = adoptedCompactions.get(message);
    if (adoptedCompaction) nextCompaction = adoptedCompaction;
    else if (message.role === 'compaction' || message.role === 'compaction-boundary')
      nextCompaction = undefined;
    else if (nextCompaction) insertBefore.set(message, nextCompaction);
  }
  const output = [...durable];
  for (const message of pending) {
    if (adoptedCompactions.has(message)) {
      // The durable row already renders this boundary. Its exact sequence lets
      // pending rows before it be inserted here without adding a second marker.
      continue;
    }
    if (message.role === 'assistant' && message.responseId) {
      const durableSegment = responseSegments.get(
        `${message.responseId}:${message.assistantSegmentOrdinal || 0}`,
      );
      const index = durableSegment ? output.indexOf(durableSegment) : -1;
      if (durableSegment && index >= 0) {
        output[index] = {
          ...durableSegment,
          ...message,
          id: durableSegment.id,
          created: durableSegment.created,
          durableRowId: durableSegment.durableRowId,
          durableSourceRowIds: durableSegment.durableSourceRowIds,
        };
        continue;
      }
    }
    const boundary = insertBefore.get(message);
    const boundaryIndex = boundary ? output.indexOf(boundary) : -1;
    const insertionIndex = boundaryIndex >= 0 ? boundaryIndex : output.length;
    const previous = output[insertionIndex - 1];
    if (
      previous?.role === 'tool-group' &&
      previous.toolGroupClosed !== true &&
      message.role === 'tool-group' &&
      previous.responseId === message.responseId
    ) {
      const tools = [...(previous.tools || [])];
      for (const incoming of message.tools || []) {
        const index = tools.findIndex((tool) => tool.id === incoming.id);
        if (index < 0) {
          if (hasDurableTool(message, incoming)) continue;
          tools.push(incoming);
          continue;
        }
        const defined = Object.fromEntries(
          Object.entries(incoming).filter(([, value]) => value !== undefined),
        ) as Partial<ToolCall>;
        const status =
          incoming.status === undefined ||
          (tools[index].status !== 'running' && incoming.status === 'running')
            ? tools[index].status
            : incoming.status;
        tools[index] = { ...tools[index], ...defined, status };
      }
      output[insertionIndex - 1] = {
        ...previous,
        ...message,
        id: previous.id,
        created: previous.created,
        durableRowId: previous.durableRowId,
        durableSourceRowIds: [
          ...new Set([
            ...((previous.durableSourceRowIds as Array<string | number> | undefined) || []),
            ...((message.durableSourceRowIds as Array<string | number> | undefined) || []),
          ]),
        ],
        tools,
      };
      continue;
    }
    if (message.role === 'tool-group' && message.tools?.length) {
      const tools = message.tools.filter((tool) => !hasDurableTool(message, tool));
      if (!tools.length) continue;
      output.splice(
        insertionIndex,
        0,
        tools.length === message.tools.length ? message : { ...message, tools },
      );
    } else output.splice(insertionIndex, 0, message);
  }
  return output;
}
