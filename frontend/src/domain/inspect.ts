export type InspectKind =
  | 'system'
  | 'developer'
  | 'user'
  | 'goal_steering'
  | 'job_notification'
  | 'assistant_text'
  | 'reasoning'
  | 'tool_call'
  | 'tool_result'
  | 'image'
  | 'file'
  | 'skill_activation'
  | 'path_note'
  | 'event'
  | 'model_change'
  | 'handover_document'
  | 'compaction_summary'
  | 'compaction_boundary';

export interface ModelInfo {
  provider?: string;
  model?: string;
  effort?: string;
  reasoning_mode?: string;
  agent?: string;
}

export interface InspectBlock {
  model_change?: { from: ModelInfo; to: ModelInfo; strategy?: string; status?: string };
  handover?: { source: string; target: string };
  synthetic?: boolean;
  id: string;
  kind: InspectKind;
  role?: string;
  message_id?: string;
  sequence?: number;
  part_index?: number;
  created_at?: number;
  title?: string;
  preview?: string;
  total_bytes?: number;
  total_lines?: number;
  has_body?: boolean;
  compaction_tail?: boolean;
  compaction_summary_id?: string;
  tool?: { call_id?: string; name?: string; is_error?: boolean };
  reasoning_kind?: 'summary' | 'raw' | 'unknown' | 'encrypted' | '';
  encrypted_bytes?: number;
  compaction?: { ordinal: number; active_boundary?: boolean; tail_count?: number } | null;
  child_session_ids?: string[];
}

export interface InspectRuntime {
  loaded: boolean;
  unavailable_reason?: '' | 'runtime_not_loaded' | 'runtime_busy';
  system_prompt_block_id?: string;
  system_prompt_matches_persisted?: boolean;
  // The engine's diagnostics are an extensible, provider-neutral JSON object.
  tool_discovery?: Record<string, unknown>;
  tools?: Array<{ block_id: string; name: string; description?: string; schema_bytes?: number }>;
}

export interface SessionInspect {
  schema_version: 1;
  session_id: string;
  transcript_rev: number;
  scope: 'persisted_history' | 'runtime_history';
  header: {
    // Optional when talking to an older v1 server.
    initial_model?: ModelInfo;
    current_model?: ModelInfo;
    provider?: string;
    model?: string;
    reasoning_effort?: string;
    agent?: string;
    compaction_seq?: number;
    compaction_count?: number;
    runtime: InspectRuntime;
  };
  blocks: InspectBlock[];
}

export interface InspectBody {
  id: string;
  format: 'text' | 'json';
  text: string;
  truncated?: boolean;
  total_bytes: number;
}

export interface SessionInspectBodies {
  transcript_rev: number;
  bodies: InspectBody[];
  missing?: string[];
  deferred?: string[];
}

export const INSPECT_CATEGORIES = [
  { id: 'system', label: 'System & developer' },
  { id: 'reasoning', label: 'Reasoning' },
  { id: 'tools', label: 'Tools' },
  { id: 'compaction', label: 'Compaction' },
] as const;
export type InspectCategory = (typeof INSPECT_CATEGORIES)[number]['id'];

export function inspectCategory(block: InspectBlock): InspectCategory | undefined {
  if (block.kind.startsWith('compaction_')) return 'compaction';
  if (
    [
      'system',
      'developer',
      'path_note',
      'goal_steering',
      'job_notification',
      'handover_document',
    ].includes(block.kind)
  )
    return 'system';
  if (block.kind === 'reasoning') return 'reasoning';
  if (block.kind === 'tool_call' || block.kind === 'tool_result') return 'tools';
  return undefined;
}

// Chips toggle category visibility; ordinary conversation rows are always shown.
export function filterInspectBlocks(blocks: InspectBlock[], hidden: ReadonlySet<InspectCategory>) {
  return blocks.filter((block) => {
    if (block.kind === 'model_change') return true;
    const category = inspectCategory(block);
    return (
      (!category || !hidden.has(category)) && !(block.compaction_tail && hidden.has('compaction'))
    );
  });
}

export function inspectBodyKey(block: Pick<InspectBlock, 'id' | 'total_bytes'>): string {
  return `${block.id}:${block.total_bytes ?? 0}`;
}

export function inspectWindow(blocks: InspectBlock[], limit = 200) {
  const earlier = Math.max(0, blocks.length - Math.max(0, limit));
  return { blocks: blocks.slice(earlier), earlier };
}

// Parts of one user message form one turn, even when windowing starts mid-message.
export function inspectTurnStarts(blocks: InspectBlock[]): ReadonlySet<string> {
  const starts = new Set<string>();
  let previousMessage: string | undefined;
  for (const block of blocks) {
    const message = block.message_id || block.id.split('.')[0];
    if (block.kind === 'user' && message !== previousMessage) starts.add(block.id);
    previousMessage = message;
  }
  return starts;
}

export function inspectTailStarts(blocks: InspectBlock[]): ReadonlySet<string> {
  return new Set(
    blocks
      .filter(
        (block, index) =>
          block.compaction_tail &&
          (!blocks[index - 1]?.compaction_tail ||
            blocks[index - 1]?.compaction_summary_id !== block.compaction_summary_id),
      )
      .map((block) => block.id),
  );
}

export function formatInspectModel(info: ModelInfo): string {
  return `${info.provider || 'unknown'}:${info.model || 'unknown'} · Thinking: ${info.effort || 'default'}${info.reasoning_mode ? ` · ${info.reasoning_mode}` : ''}${info.agent ? ` · @${info.agent}` : ''}`;
}

export function sameInspectModel(a: ModelInfo, b: ModelInfo): boolean {
  return (['provider', 'model', 'effort', 'reasoning_mode', 'agent'] as const).every(
    (key) => (a[key] || '') === (b[key] || ''),
  );
}

// Use the unfiltered timeline: a hidden category must never change provenance.
// TUI "started" markers are written after the new model is selected; failed
// switches restore the from side instead. Never fill unknown fields from now.
export function activeInspectModel(
  initial: ModelInfo,
  blocks: InspectBlock[],
  firstVisibleId: string | undefined,
): ModelInfo {
  let active = initial;
  for (const block of blocks) {
    if (block.id === firstVisibleId) break;
    if (block.kind === 'model_change' && block.model_change) {
      const change = block.model_change;
      active = change.status === 'failed' ? change.from : change.to;
    }
  }
  return active;
}
