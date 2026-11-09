import '../styles/features/agents.css';
import { useEffect, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import type { AgentCatalogEntry } from '../api/generated/types';
import {
  AGENT_SOURCE_FILTERS,
  agentModelLabel,
  agentSourceCounts,
  groupAgentCatalog,
  type AgentSourceFilter,
} from '../domain/agent-catalog';
import { errorMessage } from '../domain/text';
import { SearchField } from './FormFields';
import { Overlay } from './Overlay';

/** Read-only `/agents` catalog, the Web UI's `term-llm agents`. */
export function AgentsModal() {
  const store = useStore();
  const [agents, setAgents] = useState<AgentCatalogEntry[] | null>(null);
  const [error, setError] = useState('');
  const [filter, setFilter] = useState<AgentSourceFilter>('all');
  const [query, setQuery] = useState('');
  useEffect(() => {
    const controller = new AbortController();
    void store.endpoints
      .agents(controller.signal)
      .then((data) => setAgents(data.agents ?? []))
      .catch((value: unknown) => {
        if (!controller.signal.aborted) setError(errorMessage(value));
      });
    return () => controller.abort();
  }, [store]);
  const counts = agentSourceCounts(agents ?? []);
  const groups = groupAgentCatalog(agents ?? [], filter, query);
  const shown = groups.reduce((total, group) => total + group.agents.length, 0);
  return (
    <Overlay title="Agents" className="agents-modal">
      <div class="agents-toolbar">
        <SearchField
          className="agents-search"
          aria-label="Filter agents"
          placeholder="Filter agents"
          value={query}
          autoFocus
          onInput={(event) => setQuery(event.currentTarget.value)}
        />
        <div class="agents-source-filter" role="group" aria-label="Agent source">
          {AGENT_SOURCE_FILTERS.map((option) => (
            <button
              key={option.id}
              type="button"
              aria-pressed={filter === option.id}
              aria-label={agents ? `${option.label} (${counts[option.id]})` : option.label}
              onClick={() => setFilter(option.id)}
            >
              {option.label}
              {agents && (
                <span class="agents-source-count" aria-hidden="true">
                  {counts[option.id]}
                </span>
              )}
            </button>
          ))}
        </div>
      </div>
      <div class="agents-scroll">
        {error ? (
          <p class="agents-empty" role="alert">
            Could not load agents: {error}
          </p>
        ) : !agents ? (
          <p class="agents-empty" role="status">
            Loading agents…
          </p>
        ) : shown === 0 ? (
          <p class="agents-empty" role="status">
            {agents.length === 0 ? 'No agents configured.' : 'No agents match this filter.'}
          </p>
        ) : (
          groups.map((group) => (
            <section class="agents-group" key={group.source} aria-label={`${group.label} agents`}>
              {filter === 'all' && <h3 class="agents-group-title">{group.label}</h3>}
              <ul class="agents-list">
                {group.agents.map((agent) => {
                  const model = agentModelLabel(agent);
                  return (
                    <li class="agent-row" key={agent.name}>
                      <strong class="agent-row-name">{agent.name}</strong>
                      {model && <code class="agent-row-model">{model}</code>}
                      {agent.description && (
                        <span class="agent-row-description">{agent.description}</span>
                      )}
                    </li>
                  );
                })}
              </ul>
            </section>
          ))
        )}
      </div>
    </Overlay>
  );
}
