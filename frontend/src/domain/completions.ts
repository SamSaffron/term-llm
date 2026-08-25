export const SLASH_COMMANDS = [
  { command: '/compact', description: 'Compact conversation context' },
  { command: '/model', description: 'Choose a model' },
  { command: '/effort', description: 'Choose reasoning effort' },
  { command: '/goal', description: 'Set or manage a session goal' },
  { command: '/mcp', description: 'Manage MCP servers' },
  { command: '/skills', description: 'Browse available skills' },
  { command: '/side', description: 'Ask a side question without interrupting the main run' },
  { command: '/undo', description: 'Remove the latest turn and restore its prompt' },
  { command: '/redo', description: 'Restore the last undone turn' },
  { command: '/paths', description: 'Open conversation paths' },
  { command: '/tree', description: 'Open conversation paths' },
  { command: '/worktree', description: 'Choose a worktree' },
  { command: '/new', description: 'Start a new chat' },
];

export function composerCompletions(value: string, agents: string[]): Array<{ value: string; label: string; description: string }> {
  const slash = value.match(/(?:^|\s)(\/[\w-]*)$/);
  if (slash) return SLASH_COMMANDS.filter((entry) => entry.command.startsWith(slash[1])).map((entry) => ({ value: entry.command, label: entry.command, description: entry.description }));
  const mention = value.match(/(?:^|\s)@([\w-]*)$/);
  if (mention) return agents.filter((agent) => agent.toLowerCase().startsWith(mention[1].toLowerCase())).map((agent) => ({ value: `@${agent}`, label: `@${agent}`, description: 'Agent' }));
  return [];
}

export function applyCompletion(value: string, completion: string): string {
  return value.replace(/(?:\/[\w-]*|@[\w-]*)$/, `${completion} `);
}
