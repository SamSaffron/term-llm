import type { AgentCatalogEntry } from '../api/generated/types';

/** Mirrors `term-llm agents`, `--builtin` and `--user`. */
export type AgentSourceFilter = 'all' | 'builtin' | 'user';

export const AGENT_SOURCE_FILTERS: ReadonlyArray<{ id: AgentSourceFilter; label: string }> = [
  { id: 'all', label: 'All' },
  { id: 'user', label: 'User' },
  { id: 'builtin', label: 'Built-in' },
];

export interface AgentCatalogGroup {
  source: string;
  label: string;
  agents: AgentCatalogEntry[];
}

const GROUP_LABELS: Record<string, string> = { user: 'User', builtin: 'Built-in' };

export function agentSourceCounts(
  agents: readonly AgentCatalogEntry[],
): Record<AgentSourceFilter, number> {
  return {
    all: agents.length,
    user: agents.filter((agent) => agent.source === 'user').length,
    builtin: agents.filter((agent) => agent.source === 'builtin').length,
  };
}

/**
 * Filters by source and a case-insensitive query over name, description and
 * model, then groups user agents before built-ins like the CLI listing.
 */
export function groupAgentCatalog(
  agents: readonly AgentCatalogEntry[],
  filter: AgentSourceFilter,
  query: string,
): AgentCatalogGroup[] {
  const needle = query.trim().toLowerCase();
  const matches = agents.filter(
    (agent) =>
      (filter === 'all' || agent.source === filter) &&
      (!needle ||
        [agent.name, agent.description, agent.model].some((field) =>
          String(field || '')
            .toLowerCase()
            .includes(needle),
        )),
  );
  const order = ['user', 'builtin'];
  const sources = [...new Set(matches.map((agent) => agent.source))].sort(
    (left, right) =>
      (order.indexOf(left) + 1 || order.length + 1) -
        (order.indexOf(right) + 1 || order.length + 1) || left.localeCompare(right),
  );
  return sources.map((source) => ({
    source,
    label: GROUP_LABELS[source] || source,
    agents: matches
      .filter((agent) => agent.source === source)
      .sort((left, right) => left.name.localeCompare(right.name)),
  }));
}

/** "provider:model", whichever parts the agent pins, or '' for the session default. */
export function agentModelLabel(agent: AgentCatalogEntry): string {
  return [agent.provider, agent.model].filter(Boolean).join(':');
}
