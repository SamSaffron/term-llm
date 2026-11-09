import { describe, expect, it } from 'vitest';
import type { AgentCatalogEntry } from '../api/generated/types';
import { agentModelLabel, agentSourceCounts, groupAgentCatalog } from './agent-catalog';
import { SLASH_COMMANDS } from './completions';

const agents: AgentCatalogEntry[] = [
  { name: 'reviewer', description: 'Reviews code', source: 'builtin' },
  { name: 'dev', description: 'Discourse developer', source: 'user', model: 'opus' },
  { name: 'commit', description: 'Writes commits', source: 'builtin' },
  { name: 'alpha', source: 'user', provider: 'openai', model: 'gpt-x' },
];

const names = (groups: ReturnType<typeof groupAgentCatalog>) =>
  groups.map((group) => [group.label, group.agents.map((agent) => agent.name)]);

describe('agent catalog', () => {
  it('registers a streaming-safe /agents command', () => {
    expect(SLASH_COMMANDS.find((entry) => entry.command === '/agents')).toMatchObject({
      streamingSafe: true,
    });
  });

  it('groups user agents before built-ins, sorted by name', () => {
    expect(names(groupAgentCatalog(agents, 'all', ''))).toEqual([
      ['User', ['alpha', 'dev']],
      ['Built-in', ['commit', 'reviewer']],
    ]);
  });

  it.each([
    ['user', [['User', ['alpha', 'dev']]]],
    ['builtin', [['Built-in', ['commit', 'reviewer']]]],
  ] as const)('filters to %s agents like the CLI flag', (filter, expected) => {
    expect(names(groupAgentCatalog(agents, filter, ''))).toEqual(expected);
  });

  it('matches the query against name, description and model', () => {
    expect(names(groupAgentCatalog(agents, 'all', 'REVIEW'))).toEqual([['Built-in', ['reviewer']]]);
    expect(names(groupAgentCatalog(agents, 'all', 'discourse'))).toEqual([['User', ['dev']]]);
    expect(names(groupAgentCatalog(agents, 'all', 'gpt'))).toEqual([['User', ['alpha']]]);
    expect(groupAgentCatalog(agents, 'builtin', 'gpt')).toEqual([]);
  });

  it('counts agents per source', () => {
    expect(agentSourceCounts(agents)).toEqual({ all: 4, user: 2, builtin: 2 });
  });

  it('labels pinned models', () => {
    expect(agentModelLabel(agents[3])).toBe('openai:gpt-x');
    expect(agentModelLabel(agents[1])).toBe('opus');
    expect(agentModelLabel(agents[0])).toBe('');
  });
});
