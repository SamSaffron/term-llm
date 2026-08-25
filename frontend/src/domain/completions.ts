export interface SkillCommand {
  name?: unknown;
  description?: unknown;
  argument_hint?: unknown;
  execution?: unknown;
  source?: unknown;
  collides_with_builtin?: unknown;
}

export interface Completion {
  value: string;
  label: string;
  description: string;
  kind: 'slash' | 'agent' | 'mention';
  streamingSafe?: boolean;
  replacement?: { start: number; end: number };
  segments?: Array<{ text: string; matched?: boolean }>;
}

export interface MentionSearchToken {
  start_utf16: number;
  end_utf16: number;
  query: string;
  quoted?: boolean;
}
export interface MentionSearchItem {
  path: string;
  kind: 'file' | 'directory';
  insert_text: string;
  segments?: Array<{ text: string; matched?: boolean }>;
}
export interface MentionSearchResponse {
  active?: boolean;
  token?: MentionSearchToken;
  items?: MentionSearchItem[];
  index_truncated?: boolean;
}

export const SLASH_COMMANDS = [
  { command: '/compact', description: 'Compact conversation context' },
  { command: '/effort', description: 'Choose reasoning effort' },
  {
    command: '/fork',
    description: 'Create a parallel continuation from the last safe point',
    streamingSafe: true,
  },
  { command: '/goal', description: 'Set or manage a session goal' },
  { command: '/mcp', description: 'Manage MCP servers' },
  { command: '/model', description: 'Choose a model' },
  { command: '/new', description: 'Start a new chat' },
  { command: '/paths', description: 'Open conversation paths', streamingSafe: true },
  { command: '/redo', description: 'Restore the last undone turn' },
  {
    command: '/side',
    description: 'Ask a side question without interrupting the main run',
    streamingSafe: true,
  },
  { command: '/skills', description: 'Browse available skills' },
  {
    command: '/thread',
    description: 'Start a related conversation with optional context',
    streamingSafe: true,
  },
  { command: '/tree', description: 'Open conversation paths', streamingSafe: true },
  { command: '/undo', description: 'Remove the latest turn and restore its prompt' },
  { command: '/worktree', description: 'Choose a worktree' },
].sort((left, right) => left.command.localeCompare(right.command));

const BUILTIN_NAMES = new Set(SLASH_COMMANDS.map((entry) => entry.command));

export function skillCompletions(skills: SkillCommand[]): Completion[] {
  const seen = new Set<string>();
  return skills.flatMap((skill): Completion[] => {
    const bare = String(skill.name || '').trim();
    const value = `/${bare}`;
    if (
      !/^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(bare) ||
      skill.collides_with_builtin ||
      BUILTIN_NAMES.has(value) ||
      seen.has(value)
    )
      return [];
    seen.add(value);
    const hint = String(skill.argument_hint || '').trim();
    const source = String(skill.source || 'skill').trim();
    const isolated = skill.execution === 'isolated';
    return [
      {
        value,
        label: hint ? `${value} ${hint}` : value,
        description: `${String(skill.description || '').trim()} · skill:${source}${isolated ? ' · isolated' : ''}`,
        kind: 'slash',
        streamingSafe: isolated,
      },
    ];
  });
}

export function composerCompletions(
  value: string,
  agents: string[],
  skills: SkillCommand[] = [],
  streaming = false,
): Completion[] {
  const slash = value.match(/^\/[\w-]*$/);
  if (slash) {
    const entries: Completion[] = [
      ...SLASH_COMMANDS.map((entry) => ({
        value: entry.command,
        label: entry.command,
        description: entry.description,
        kind: 'slash' as const,
        streamingSafe: entry.streamingSafe,
      })),
      ...skillCompletions(skills),
    ];
    return entries
      .filter((entry) => entry.value.startsWith(slash[0]) && (!streaming || entry.streamingSafe))
      .sort((left, right) => left.value.localeCompare(right.value));
  }
  const mention = activeMentionAtCursor(value, value.length);
  if (mention)
    return agents
      .filter((agent) => agent.toLowerCase().startsWith(mention.query.toLowerCase()))
      .map((agent) => ({
        value: `@${agent}`,
        label: `@${agent}`,
        description: 'Agent',
        kind: 'agent',
        replacement: { start: mention.start, end: mention.end },
      }));
  return [];
}

export function activeMentionAtCursor(
  value: string,
  cursor: number,
): { start: number; end: number; query: string } | null {
  const bounded = Math.max(0, Math.min(value.length, cursor));
  const before = value.slice(0, bounded);
  const lineStart = before.lastIndexOf('\n') + 1;
  const line = before.slice(lineStart);
  const match = line.match(/(?:^|[\s。 、？！])@(?:(?:"(?:\\.|[^"\\])*)|[^\s"]*)$/u);
  if (!match) return null;
  const at = line.lastIndexOf('@');
  const token = line.slice(at);
  return { start: lineStart + at, end: bounded, query: token.slice(1).replace(/^"/, '') };
}

export function mentionCompletions(payload: MentionSearchResponse | null): Completion[] {
  if (!payload?.active || !payload.token) return [];
  const replacement = { start: payload.token.start_utf16, end: payload.token.end_utf16 };
  return (payload.items || []).flatMap((item): Completion[] => {
    const path = String(item.path || '');
    const value = String(item.insert_text || '');
    if (!path || !value) return [];
    return [
      {
        value,
        label: path,
        description: item.kind === 'directory' ? 'directory' : 'file',
        kind: 'mention',
        replacement,
        segments: item.segments || [],
      },
    ];
  });
}

export function applyCompletion(value: string, completion: string | Completion): string {
  const item = typeof completion === 'string' ? null : completion;
  const replacement = item?.replacement;
  const inserted = typeof completion === 'string' ? completion : completion.value;
  if (replacement) {
    const after = value.slice(replacement.end);
    const separator = !after || !/^\s/u.test(after) ? ' ' : '';
    return value.slice(0, replacement.start) + inserted + separator + after;
  }
  return value.replace(/(?:\/[\w-]*|@[\w-]*)$/, `${inserted} `);
}

export function completedCursor(value: string, completion: Completion): number {
  if (!completion.replacement) return applyCompletion(value, completion).length;
  const after = value.slice(completion.replacement.end);
  return (
    completion.replacement.start + completion.value.length + (!after || !/^\s/u.test(after) ? 1 : 0)
  );
}
