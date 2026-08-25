import type { ActiveRun, ApprovalPrompt, AskUserPrompt, CurrentPlan, GuardianReview, Message, ToolCall, Usage } from './types';

export interface ResponseProjection {
  run: ActiveRun;
  messages: Message[];
  plan: CurrentPlan | null;
  askUser: AskUserPrompt | null;
  approval: ApprovalPrompt | null;
  usage: Usage | null;
  fileChangeRevision: number;
  pendingGuardian: Record<string, GuardianReview[]>;
  modelSwap?: { stage: string; content: string };
  phase?: string;
  retry?: { attempt: number; delayMs: number; error: string };
}

export interface ResponseEvent {
  type: string; response_id?: string; run_epoch?: number; sequence_number?: number; [key: string]: unknown;
}

export class ResponseProtocolError extends Error {
  constructor(message: string, readonly code: 'owner' | 'epoch' | 'gap', readonly expected?: number, readonly received?: number) { super(message); this.name = 'ResponseProtocolError'; }
}

const text = (value: unknown): string => typeof value === 'string' ? value : value == null ? '' : String(value);
const number = (value: unknown, fallback = 0): number => Number.isFinite(Number(value)) ? Math.trunc(Number(value)) : fallback;
const clone = <T>(value: T): T => structuredClone(value);

export function initialProjection(run: ActiveRun): ResponseProjection {
  return { run, messages: [], plan: null, askUser: null, approval: null, usage: null, fileChangeRevision: 0, pendingGuardian: {} };
}

function assistant(messages: Message[], responseId: string, ordinal = 0): [Message[], Message, boolean] {
  const existing = [...messages].reverse().find((message) => message.role === 'assistant' && message.responseId === responseId && message.assistantSegmentOrdinal === ordinal);
  if (existing) return [messages, existing, false];
  const created: Message = { id: `${responseId}:assistant:${ordinal}`, role: 'assistant', content: '', created: Date.now(), responseId, assistantSegmentOrdinal: ordinal };
  return [[...messages, created], created, true];
}
function replaceMessage(messages: Message[], target: Message, patch: Partial<Message>): Message[] { return messages.map((message) => message === target ? { ...message, ...patch } : message); }
function closeToolGroups(messages: Message[]): Message[] {
  return messages.map((message) => message.role !== 'tool-group' || message.status !== 'running' ? message : { ...message, status: 'done', tools: message.tools?.map((tool) => tool.status === 'running' ? { ...tool, status: 'done' } : tool) });
}
function tool(messages: Message[], responseId: string, id: string, name = 'tool', pending: Record<string, GuardianReview[]> = {}): [Message[], ToolCall] {
  if (!id) throw new ResponseProtocolError('Tool event is missing call_id', 'gap');
  const group = [...messages].reverse().find((message) => message.role === 'tool-group' && message.responseId === responseId && message.status !== 'done');
  const existing = messages.flatMap((message) => message.tools || []).find((entry) => entry.id === id);
  if (existing) return [messages, existing];
  const entry: ToolCall = { id, name, status: 'running', guardianReviews: pending[id]?.map(clone) };
  if (group) return [replaceMessage(messages, group, { status: 'running', tools: [...(group.tools || []), entry] }), entry];
  return [[...messages, { id: `${responseId}:tools:${id}`, role: 'tool-group', content: '', created: Date.now(), responseId, status: 'running', tools: [entry] }], entry];
}
function patchTool(messages: Message[], id: string, patch: Partial<ToolCall>): Message[] {
  return messages.map((message) => message.role !== 'tool-group' ? message : { ...message, tools: (message.tools || []).map((entry) => entry.id === id ? { ...entry, ...patch } : entry) });
}
function appendNotice(messages: Message[], event: ResponseEvent, content: string, role: Message['role'] = 'guardian-notice'): Message[] {
  return [...messages, { id: `${text(event.response_id)}:${event.type}:${event.sequence_number || Date.now()}`, role, content, created: Date.now(), responseId: text(event.response_id) }];
}
function eventError(event: ResponseEvent, fallback: string): string {
  const source = event.error;
  if (source && typeof source === 'object') return text((source as Record<string, unknown>).message) || fallback;
  return text(source || event.message) || fallback;
}
function flushPendingGuardian(messages: Message[], pending: Record<string, GuardianReview[]>, event: ResponseEvent): Message[] {
  let output = messages;
  for (const [callID, reviews] of Object.entries(pending)) for (const review of reviews) {
    const outcome = review.outcome || 'warning';
    output = appendNotice(output, event, `${review.message || `Guardian ${outcome} review`} (unmatched tool call ${callID})`);
  }
  return output;
}
function validate(projection: ResponseProjection, event: ResponseEvent): { duplicate: boolean; sequence: number; responseId: string; epoch: number } {
  if (event.type === 'response.stream_error') return { duplicate: false, sequence: projection.run.lastSequence, responseId: projection.run.responseId, epoch: projection.run.epoch };
  const responseId = text(event.response_id || (event.response as Record<string, unknown> | undefined)?.id);
  if (!responseId) throw new ResponseProtocolError('Response event is missing response_id', 'owner');
  if (responseId !== projection.run.responseId) throw new ResponseProtocolError(`Response owner mismatch: ${responseId}`, 'owner');
  const epoch = number(event.run_epoch);
  if (!epoch || epoch !== projection.run.epoch) throw new ResponseProtocolError(`Response epoch mismatch: ${epoch}`, 'epoch');
  const sequence = number(event.sequence_number);
  if (!sequence) throw new ResponseProtocolError('Response event is missing sequence_number', 'gap', projection.run.lastSequence + 1, sequence);
  if (sequence <= projection.run.lastSequence) return { duplicate: true, sequence, responseId, epoch };
  if (sequence !== projection.run.lastSequence + 1) throw new ResponseProtocolError(`Response event gap: expected ${projection.run.lastSequence + 1}, got ${sequence}`, 'gap', projection.run.lastSequence + 1, sequence);
  return { duplicate: false, sequence, responseId, epoch };
}

export function reduceResponse(projection: ResponseProjection, event: ResponseEvent): ResponseProjection {
  if (event.type === 'response.stream_error') return projection;
  const checked = validate(projection, event); if (checked.duplicate) return projection;
  const responseId = checked.responseId; let messages = projection.messages;
  const run: ActiveRun = { ...projection.run, responseId, epoch: checked.epoch, lastSequence: checked.sequence };
  const next: ResponseProjection = { ...projection, run };
  switch (event.type) {
    case 'response.created': return { ...next, run: { ...run, status: 'streaming', startedRev: number(event.started_rev, run.startedRev) } };
    case 'response.output_text.new_segment': {
      messages = closeToolGroups(messages); [messages] = assistant(messages, responseId, number(event.assistant_segment_ordinal ?? event.output_index, 0));
      return { ...next, messages, phase: undefined, retry: undefined };
    }
    case 'response.output_text.delta': {
      const delta = text(event.delta); if (!delta) return next;
      messages = closeToolGroups(messages); let target: Message; [messages, target] = assistant(messages, responseId, number(event.assistant_segment_ordinal ?? event.output_index, 0));
      messages = replaceMessage(messages, target, { content: target.content + delta, segmentStartSequence: target.segmentStartSequence || number(event.segment_start_sequence, checked.sequence), segmentEndSequence: checked.sequence });
      return { ...next, messages, run: { ...run, status: 'streaming' }, phase: undefined, retry: undefined };
    }
    case 'response.output_item.added': {
      const item = (event.item || {}) as Record<string, unknown>;
      if (item.type === 'function_call' || event.item_type === 'function_call') { const id = text(item.call_id || item.id || event.call_id || event.item_id); [messages] = tool(messages, responseId, id, text(item.name || event.name) || 'tool', projection.pendingGuardian); }
      return { ...next, messages, retry: undefined };
    }
    case 'response.function_call_arguments.delta': {
      const id = text(event.call_id || event.item_id); const candidates = messages.flatMap((message) => message.tools || []);
      const entry = id ? candidates.find((candidate) => candidate.id === id) : [...candidates].reverse().find((candidate) => candidate.status === 'running');
      if (!entry || entry.argumentsFinalized) return next;
      messages = patchTool(messages, entry.id, { arguments: text(entry.arguments) + text(event.delta) }); return { ...next, messages };
    }
    case 'response.output_item.done': {
      const item = (event.item || {}) as Record<string, unknown>; if (item.type !== 'function_call') return next;
      const id = text(item.call_id || item.id || event.call_id || event.item_id); const entry = messages.flatMap((message) => message.tools || []).find((candidate) => candidate.id === id);
      if (!entry) [messages] = tool(messages, responseId, id, text(item.name), projection.pendingGuardian);
      return { ...next, messages: patchTool(messages, id, { arguments: Object.hasOwn(item, 'arguments') ? text(item.arguments) : entry?.arguments, argumentsFinalized: true }) };
    }
    case 'response.tool_exec.start': {
      const id = text(event.call_id || event.tool_call_id || event.item_id); let entry: ToolCall; [messages, entry] = tool(messages, responseId, id, text(event.tool_name || event.name || event.tool), projection.pendingGuardian);
      return { ...next, messages: patchTool(messages, id, { name: text(event.tool_name || event.name || event.tool) || entry.name, arguments: text(event.tool_arguments) || entry.arguments, status: 'running' }), retry: undefined };
    }
    case 'response.tool_exec.end': {
      const id = text(event.call_id || event.tool_call_id || event.item_id); let entry: ToolCall; [messages, entry] = tool(messages, responseId, id, text(event.tool_name || event.name || event.tool), projection.pendingGuardian);
      const failed = event.success === false || event.error != null || event.status === 'error'; const images = Array.isArray(event.images) ? event.images.map(String) : entry.images;
      messages = patchTool(messages, id, { name: text(event.tool_name || event.name || event.tool) || entry.name, arguments: text(event.tool_arguments) || entry.arguments, status: failed ? 'error' : 'done', resultStatus: failed ? 'error' : 'success', result: text(event.output || event.result), images });
      messages = messages.map((message) => message.role === 'tool-group' && message.tools?.every((candidate) => candidate.status !== 'running') ? { ...message, status: 'done' } : message);
      let plan = next.plan; const completed = messages.flatMap((message) => message.tools || []).find((candidate) => candidate.id === id);
      if (completed?.name === 'update_plan' && !failed) { try { const value = JSON.parse(completed.arguments || '{}') as CurrentPlan; if (Array.isArray(value.plan)) plan = value; } catch { /* Server plan state remains authoritative. */ } }
      return { ...next, messages, plan, retry: undefined };
    }
    case 'response.guardian.review': {
      const id = text(event.call_id || event.tool_call_id || event.item_id); const review: GuardianReview = { outcome: text(event.outcome) || 'warning', message: text(event.message || event.text), model: text(event.model), tool: text(event.tool), command: text(event.command), path: text(event.path), is_write: Boolean(event.is_write), workdir: text(event.workdir) };
      const entry = messages.flatMap((message) => message.tools || []).find((candidate) => candidate.id === id);
      if (entry) return { ...next, messages: patchTool(messages, id, { guardianReviews: [...(entry.guardianReviews || []), review] }) };
      if (id) return { ...next, pendingGuardian: { ...projection.pendingGuardian, [id]: [...(projection.pendingGuardian[id] || []), review] } };
      return review.message ? { ...next, messages: appendNotice(messages, event, review.message) } : next;
    }
    case 'response.interjection': {
      const clientMessageId = text(event.client_message_id || event.interjection_id); if (!clientMessageId) throw new ResponseProtocolError('Interjection is missing client_message_id', 'gap');
      if (messages.some((message) => message.role === 'user' && message.clientMessageId === clientMessageId)) return next;
      return { ...next, messages: [...closeToolGroups(messages), { id: `${responseId}:intent:${clientMessageId}`, role: 'user', content: text(event.text || event.content || event.message), clientMessageId, created: Date.now(), responseId }] };
    }
    case 'response.compaction': return { ...next, messages: [...closeToolGroups(messages), { id: `${responseId}:compaction:${checked.sequence}`, role: 'compaction-boundary', content: 'Context compacted', created: Date.now(), responseId, pending: true, eventSequence: checked.sequence }] };
    case 'response.phase': return { ...next, phase: text(event.text || event.message || event.status), retry: undefined };
    case 'response.model_swap.progress': return { ...next, modelSwap: { stage: text(event.stage), content: text(event.text || event.message || event.content) || 'Switching model…' } };
    case 'response.model_switch': return { ...next, messages: appendNotice(closeToolGroups(messages), event, text(event.message || event.model) || 'Model switched', 'model-swap'), modelSwap: undefined };
    case 'response.retry': return { ...next, retry: { attempt: number(event.attempt), delayMs: number(event.delay_ms), error: text(event.error || event.message) } };
    case 'response.attempt.discard': return { ...next, messages: messages.filter((message) => message.responseId !== responseId || !['assistant', 'tool-group'].includes(message.role)), pendingGuardian: {}, phase: undefined, retry: undefined };
    case 'response.ask_user.prompt': return { ...next, askUser: { sessionId: projection.run.sessionId, callId: text(event.call_id), questions: Array.isArray(event.questions) ? event.questions as AskUserPrompt['questions'] : [] } };
    case 'response.approval.prompt': return { ...next, approval: { sessionId: projection.run.sessionId, id: text(event.approval_id || event.id), title: text(event.title), intro: text(event.intro), path: text(event.path), body: text(event.body || event.message), note: text(event.note), options: Array.isArray(event.options) ? event.options as ApprovalPrompt['options'] : [], selectedIndex: 0, resumeAutoAvailable: event.resume_auto_available === true } };
    case 'response.file_change': return { ...next, fileChangeRevision: projection.fileChangeRevision + 1 };
    case 'response.completed': return { ...next, messages: flushPendingGuardian(closeToolGroups(messages), projection.pendingGuardian, event), pendingGuardian: {}, run: { ...run, status: 'completed', finalRev: number(event.final_rev), durableHandoff: event.durable_handoff === true }, usage: (event.usage || (event.response as Record<string, unknown> | undefined)?.usage || null) as Usage | null, phase: undefined, retry: undefined };
    case 'response.cancelled': return { ...next, messages: flushPendingGuardian(closeToolGroups(messages), projection.pendingGuardian, event), pendingGuardian: {}, run: { ...run, status: 'cancelled', finalRev: number(event.final_rev), durableHandoff: event.durable_handoff === true }, phase: undefined, retry: undefined };
    case 'response.failed': {
      const content = eventError(event, 'Response failed');
      messages = flushPendingGuardian(closeToolGroups(messages), projection.pendingGuardian, event);
      return { ...next, run: { ...run, status: 'failed', error: content, finalRev: number(event.final_rev), durableHandoff: event.durable_handoff === true }, pendingGuardian: {}, messages: appendNotice(messages, event, content, 'error'), phase: undefined, retry: undefined };
    }
    default: return next;
  }
}

export const RESPONSE_EVENT_TYPES = [
  'response.created', 'response.output_text.new_segment', 'response.output_text.delta', 'response.output_item.added',
  'response.function_call_arguments.delta', 'response.output_item.done', 'response.tool_exec.start', 'response.tool_exec.end',
  'response.guardian.review', 'response.interjection', 'response.compaction', 'response.phase', 'response.model_swap.progress',
  'response.model_switch', 'response.retry', 'response.attempt.discard', 'response.ask_user.prompt', 'response.approval.prompt',
  'response.file_change', 'response.heartbeat', 'response.completed', 'response.cancelled', 'response.failed', 'response.stream_error',
] as const;
