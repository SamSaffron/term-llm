import '../styles/features/stats.css';
import { useEffect, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import type { SessionMetrics, SessionStats, StatsChild, StatsModel } from '../api/endpoints';
import { errorMessage } from '../domain/text';
import { Overlay } from './Overlay';

const count = (value?: number) => (value ?? 0).toLocaleString();
const duration = (ms?: number) => (ms === undefined ? '—' : `${(ms / 1000).toFixed(1)}s`);
const cost = (usd?: number, partial?: boolean) =>
  usd === undefined ? '—' : `${partial ? '≥' : ''}$${usd.toFixed(4)}`;

function Counters({ metrics }: { metrics: SessionMetrics }) {
  return (
    <dl class="stats-counters">
      <dt>Fresh input tokens</dt>
      <dd>{count(metrics.input_tokens)}</dd>
      <dt>Cache read tokens</dt>
      <dd>{count(metrics.cached_input_tokens)}</dd>
      <dt>Cache write tokens</dt>
      <dd>{count(metrics.cache_write_tokens)}</dd>
      <dt>Output tokens</dt>
      <dd>{count(metrics.output_tokens)}</dd>
      <dt>Tool calls</dt>
      <dd>{count(metrics.tool_calls)}</dd>
      <dt>Assistant turns</dt>
      <dd>{count(metrics.llm_turns)}</dd>
    </dl>
  );
}

// Old sessions retain counters by child model, but not request pricing or work time.
function historicalModels(children: StatsChild[]): StatsModel[] {
  const models = new Map<string, StatsModel>();
  for (const child of children) {
    const model = child.model || 'Unknown model';
    const row = models.get(model) ?? {
      model,
      input_tokens: 0,
      cached_input_tokens: 0,
      cache_write_tokens: 0,
      output_tokens: 0,
      tool_calls: 0,
      llm_turns: 0,
    };
    for (const key of [
      'input_tokens',
      'cached_input_tokens',
      'cache_write_tokens',
      'output_tokens',
      'tool_calls',
      'llm_turns',
    ] as const)
      row[key] += child[key] ?? 0;
    models.set(model, row);
  }
  return [...models.values()].sort((a, b) => a.model.localeCompare(b.model));
}

function Models({ models }: { models: StatsModel[] }) {
  return (
    <div class="stats-table-scroll" role="region" aria-label="Subagent model usage" tabIndex={0}>
      <table class="stats-table">
        <thead>
          <tr>
            {[
              'Model',
              'Time',
              'Tools',
              'Input',
              'Cache read',
              'Cache write',
              'Output',
              'Calls',
              'Tool calls',
              'Est. cost',
            ].map((label) => (
              <th scope="col" key={label}>
                {label}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {models.map((row) => (
            <tr key={row.model}>
              <th scope="row">
                {row.model}
                {row.running && <span aria-label="running"> *</span>}
              </th>
              <td>{duration(row.active_ms)}</td>
              <td>{duration(row.tool_ms)}</td>
              <td>{count(row.input_tokens)}</td>
              <td>{count(row.cached_input_tokens)}</td>
              <td>{count(row.cache_write_tokens)}</td>
              <td>{count(row.output_tokens)}</td>
              <td>{count(row.llm_turns)}</td>
              <td>{count(row.tool_calls)}</td>
              <td>{cost(row.cost_usd, row.cost_partial)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function StatsModal() {
  const store = useStore();
  const sessionId = store.activeSession.value?.id;
  const [refresh, setRefresh] = useState(0);
  const [data, setData] = useState<{
    sessionId: string;
    stats: SessionStats;
    children: StatsChild[];
    limit?: number;
  }>();
  const [error, setError] = useState('');
  useEffect(() => {
    setData(undefined);
    setError('');
    if (!sessionId) return;
    const controller = new AbortController();
    Promise.all([
      store.endpoints.sessionStats(sessionId, controller.signal),
      store.endpoints.sessionChildren(sessionId, controller.signal),
    ])
      .then(([stats, children]) => {
        if (!controller.signal.aborted)
          setData({ sessionId, stats, children: children.children ?? [], limit: children.limit });
      })
      .catch((reason: unknown) => {
        if (!controller.signal.aborted) setError(errorMessage(reason));
      });
    return () => controller.abort();
  }, [store, sessionId, refresh]);
  const current = data?.sessionId === sessionId ? data : undefined;
  const stats = current?.stats;
  const historical = stats?.scope === 'durable_history';
  const models = stats
    ? historical
      ? historicalModels(current!.children)
      : (stats.models ?? [])
    : [];
  return (
    <Overlay title="Chat Stats" className="stats-modal">
      {!sessionId ? (
        <p>No session selected.</p>
      ) : (
        <>
          <div class="stats-toolbar">
            <span class="stats-note">
              {historical ? 'Recorded history' : 'Observed in this runtime'}
            </span>
            <button
              class="stats-refresh"
              type="button"
              onClick={() => setRefresh((value) => value + 1)}
            >
              Refresh statistics
            </button>
          </div>
          {error && <p role="alert">{error}</p>}
          {!current && !error && <p role="status">Loading statistics…</p>}
          {stats && current && (
            <>
              <dl class="stats-headline">
                <div>
                  <dt>Active</dt>
                  <dd>{duration(stats.active_ms)}</dd>
                </div>
                <div>
                  <dt>Model</dt>
                  <dd>{duration(stats.model_ms)}</dd>
                </div>
                <div>
                  <dt>Tools</dt>
                  <dd>{duration(stats.tool_ms)}</dd>
                </div>
                <div>
                  <dt>Est. cost</dt>
                  <dd>{cost(stats.cost_usd, stats.cost_partial)}</dd>
                </div>
              </dl>
              <dl class="stats-counters stats-performance">
                <dt>Time to first token</dt>
                <dd>{duration(stats.ttft_ms)}</dd>
                <dt>Output speed</dt>
                <dd>
                  {stats.output_tokens_per_second === undefined
                    ? '—'
                    : `${stats.output_tokens_per_second.toFixed(1)} tokens/s`}
                </dd>
              </dl>
              <h3 class="stats-section-title">Subagent models</h3>
              {models.length ? (
                <Models models={models} />
              ) : (
                <p class="stats-note">No {historical ? 'recorded' : 'observed'} subagent usage.</p>
              )}
              {historical && models.length > 0 && (
                <p class="stats-note">
                  Recorded child counters grouped by session model; request-level models, timing and
                  pricing were not retained.
                </p>
              )}
              {historical && current.limit && current.children.length >= current.limit && (
                <p class="stats-note">
                  Most recent {current.limit} child sessions; this breakdown may be incomplete.
                </p>
              )}
              {models.some((row) => row.running) && <p class="stats-note">* running</p>}
              {models.some((row) => row.active_ms !== undefined) && (
                <p class="stats-note">Time summed across runs, not parallel wall time.</p>
              )}
              {(stats.cost_partial || models.some((row) => row.cost_partial)) && (
                <p class="stats-note">≥ partial estimate; some requests could not be priced.</p>
              )}
              {(stats.sections ?? []).map((section, index) => (
                <section class="stats-section" key={`${section.title}-${index}`}>
                  <h3 class="stats-section-title">
                    {section.title
                      .replace(' · runtime_local', ' · this runtime')
                      .replace(' · durable_history', ' · recorded history')}
                  </h3>
                  <dl class="stats-counters">
                    {section.rows.map((row, index) => (
                      <div class="stats-counter-row" key={`${row.label}-${index}`}>
                        <dt>{row.label}</dt>
                        <dd>{row.value}</dd>
                      </div>
                    ))}
                  </dl>
                </section>
              ))}
              {!stats.sections?.some((section) =>
                section.title.startsWith('Cumulative Token Usage'),
              ) && (
                <section>
                  <h3 class="stats-section-title">Token usage</h3>
                  <Counters metrics={stats.metrics} />
                </section>
              )}
              {stats.durable_metrics && (
                <section>
                  <h3 class="stats-section-title">Recorded parent-session totals</h3>
                  <Counters metrics={stats.durable_metrics} />
                  <p class="stats-note">
                    Separate historical counters; do not add to the runtime figures above.
                  </p>
                </section>
              )}
              <p class="stats-note stats-footnote">
                — not available. Runtime detail resets on eviction or restart; old history cannot
                supply unrecorded timing or per-request costs.
              </p>
            </>
          )}
        </>
      )}
    </Overlay>
  );
}
