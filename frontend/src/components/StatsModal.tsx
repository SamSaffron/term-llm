import '../styles/features/stats.css';
import { useEffect, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import type { SessionMetrics, SessionStats, StatsChild, StatsModel } from '../api/endpoints';
import { errorMessage } from '../domain/text';
import { Overlay } from './Overlay';
import { formatElapsedDuration } from '../platform/elapsed-clock';

// One formatter for every count in the report: abbreviated so columns of
// millions can be compared at a glance, with the exact value on hover.
const compact = (value: number): string => {
  const abs = Math.abs(value);
  // The unit is chosen after rounding, so 999,950 reads 1M rather than 1000K.
  for (const [scale, suffix] of [
    [1_000_000_000, 'B'],
    [1_000_000, 'M'],
    [1_000, 'K'],
  ] as const)
    // Promote only once the smaller unit would round to a full thousand of
    // itself, so 999,950 reads 1M while 955,470 stays 955.5K.
    if (abs >= scale * 0.9995) return `${(value / scale).toFixed(1).replace(/\.0$/, '')}${suffix}`;
  return String(value);
};
// Grouped the way the server groups its own rows, so one modal never shows two
// separators for the same kind of number.
const exact = (value: number) => value.toLocaleString('en-US');
const count = (value?: number) => compact(value ?? 0);
// Seconds stop being readable after a minute or two: 3977.3s says nothing that
// 1h06m does not say better, so anything longer uses the shared compact format.
// Below a minute the tenth still matters, which that format cannot express (a
// 0.4s time to first token would read as 0s).
const duration = (ms?: number): string => {
  if (ms === undefined) return '—';
  const tenths = Math.round(ms / 100) / 10;
  return tenths < 60 ? `${tenths.toFixed(1)}s` : formatElapsedDuration(ms);
};
const exactDuration = (ms?: number) =>
  ms === undefined ? undefined : `${(ms / 1000).toFixed(1)}s`;
// Cents are the smallest unit worth reading; a sub-cent estimate is still spend.
const cost = (usd?: number, partial?: boolean) => {
  if (usd === undefined) return '—';
  const prefix = partial ? '≥' : '';
  if (usd > 0 && usd < 0.005) return `${prefix}<$0.01`;
  return `${prefix}$${usd.toFixed(2)}`;
};

// tokensOnly omits the activity rows for counters that carry no turn or tool
// provenance, so a structural zero is never shown as an observed zero.
function Count({ value }: { value?: number }) {
  return <dd title={exact(value ?? 0)}>{count(value)}</dd>;
}

function Counters({ metrics, tokensOnly }: { metrics: SessionMetrics; tokensOnly?: boolean }) {
  return (
    <dl class="stats-counters">
      <dt>Fresh input tokens</dt>
      <Count value={metrics.input_tokens} />
      <dt>Cache read tokens</dt>
      <Count value={metrics.cached_input_tokens} />
      <dt>Cache write tokens</dt>
      <Count value={metrics.cache_write_tokens} />
      <dt>Output tokens</dt>
      <Count value={metrics.output_tokens} />
      {!tokensOnly && (
        <>
          <dt>Tool calls</dt>
          <Count value={metrics.tool_calls} />
          <dt>Assistant turns</dt>
          <Count value={metrics.llm_turns} />
        </>
      )}
    </dl>
  );
}

const KIND_LABELS: Record<string, string> = {
  main: 'session',
  guardian: 'guardian',
  compaction: 'compaction',
  side_question: 'side question',
  handover: 'handover',
  subagent: 'delegated',
};

// Sections with nothing recorded are omitted rather than rendered as a wall of
// zeros. The server already does this; this guards the legacy fallback below.
const hasCounters = (metrics?: SessionMetrics) =>
  !!metrics &&
  [
    metrics.input_tokens,
    metrics.output_tokens,
    metrics.cached_input_tokens,
    metrics.cache_write_tokens,
    metrics.tool_calls,
    metrics.llm_turns,
  ].some((value) => (value ?? 0) > 0);

const kindSummary = (kinds?: string[]) =>
  kinds?.length ? kinds.map((kind) => KIND_LABELS[kind] ?? kind).join(', ') : '';

// Child sessions keep their own counters. They are reported separately from the
// parent's per-model rows so delegated work is never counted twice.
function childModels(children: StatsChild[]): StatsModel[] {
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
    // A child that could not be priced makes its model's total a lower bound
    // rather than erasing the spend of the ones that could.
    if (child.cost_usd === undefined) row.cost_partial = true;
    else row.cost_usd = (row.cost_usd ?? 0) + child.cost_usd;
    row.cost_partial = row.cost_partial || child.cost_partial;
    models.set(model, row);
  }
  return [...models.values()].sort((a, b) => a.model.localeCompare(b.model));
}

// Columns a source never records are omitted, so a table of dashes is not
// presented as missing data the user could go looking for. Child sessions keep
// no clock but do have a price.
function Models({
  models,
  label,
  timing = true,
  cost: showCost = true,
}: {
  models: StatsModel[];
  label: string;
  timing?: boolean;
  cost?: boolean;
}) {
  return (
    <div class="stats-table-scroll" role="region" aria-label={label} tabIndex={0}>
      <table class="stats-table">
        <thead>
          <tr>
            {[
              'Model',
              ...(timing ? ['Time', 'Tools'] : []),
              'Input',
              'Cache read',
              'Cache write',
              'Output',
              'Calls',
              'Tool calls',
              ...(showCost ? ['Est. cost'] : []),
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
                {kindSummary(row.kinds) && (
                  <span class="stats-model-kinds"> ({kindSummary(row.kinds)})</span>
                )}
              </th>
              {timing && (
                <>
                  <td title={exactDuration(row.active_ms)}>{duration(row.active_ms)}</td>
                  <td title={exactDuration(row.tool_ms)}>{duration(row.tool_ms)}</td>
                </>
              )}
              {[
                row.input_tokens,
                row.cached_input_tokens,
                row.cache_write_tokens,
                row.output_tokens,
                row.llm_turns,
                row.tool_calls,
              ].map((value, index) => (
                <td key={index} title={exact(value ?? 0)}>
                  {count(value)}
                </td>
              ))}
              {showCost && (
                <td
                  title={
                    row.cost_usd === undefined
                      ? undefined
                      : `${row.cost_partial ? '≥' : ''}$${row.cost_usd.toFixed(4)}`
                  }
                >
                  {cost(row.cost_usd, row.cost_partial)}
                </td>
              )}
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
  // Both scopes now report the session's own model alongside its helpers and
  // delegated work. Child sessions stay in their own table so the two never
  // double count the same tokens.
  const models = stats?.models ?? [];
  const children = current ? childModels(current.children) : [];
  // Timing and cost both span every runtime the session ever had. A tile with
  // nothing to report is dropped rather than shown as a dash.
  const headline: [string, string, string | undefined][] = [];
  if (stats?.active_ms !== undefined)
    headline.push(['Active', duration(stats.active_ms), exactDuration(stats.active_ms)]);
  if (stats?.model_ms !== undefined)
    headline.push(['Model', duration(stats.model_ms), exactDuration(stats.model_ms)]);
  if (stats?.tool_ms !== undefined)
    headline.push(['Tools', duration(stats.tool_ms), exactDuration(stats.tool_ms)]);
  if (stats?.cost_usd !== undefined)
    headline.push([
      'Est. cost (session)',
      cost(stats.cost_usd, stats.cost_partial),
      `${stats.cost_partial ? '≥' : ''}$${stats.cost_usd.toFixed(4)}`,
    ]);
  return (
    <Overlay title="Chat Stats" className="stats-modal" focusContent={false}>
      {!sessionId ? (
        <p>No session selected.</p>
      ) : (
        <>
          <div class="stats-toolbar">
            <span class="stats-note">
              {historical ? 'Recorded history' : 'Session totals · runtime detail'}
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
              {headline.length > 0 && (
                <dl class="stats-headline">
                  {headline.map(([label, value, title]) => (
                    <div key={label}>
                      <dt>{label}</dt>
                      <dd title={title}>{value}</dd>
                    </div>
                  ))}
                </dl>
              )}
              {(stats.ttft_ms !== undefined || stats.output_tokens_per_second !== undefined) && (
                <dl class="stats-counters stats-performance">
                  {stats.ttft_ms !== undefined && (
                    <>
                      <dt>Time to first token</dt>
                      <dd title={exactDuration(stats.ttft_ms)}>{duration(stats.ttft_ms)}</dd>
                    </>
                  )}
                  {stats.output_tokens_per_second !== undefined && (
                    <>
                      <dt>Output speed</dt>
                      <dd>{`${stats.output_tokens_per_second.toFixed(1)} tokens/s`}</dd>
                    </>
                  )}
                </dl>
              )}
              <h3 class="stats-section-title">Models</h3>
              {models.length ? (
                <Models models={models} label="Model usage" />
              ) : (
                <p class="stats-note">
                  No {historical ? 'recorded' : 'observed'} per-model usage
                  {historical && '; only combined totals exist'}.
                </p>
              )}
              {historical && models.length > 0 && (
                <p class="stats-note">Cost priced from recorded totals at base rates.</p>
              )}
              {children.length > 0 && (
                <>
                  <h3 class="stats-section-title">Child sessions</h3>
                  <Models models={children} label="Child session usage" timing={false} />
                  <p class="stats-note">
                    {models.some((row) => row.kinds?.includes('subagent'))
                      ? 'Also counted in Models above.'
                      : 'Included in the session total above.'}
                  </p>
                </>
              )}
              {stats.unattributed && (
                <section>
                  <h3 class="stats-section-title">Not attributed to a model</h3>
                  <Counters metrics={stats.unattributed} tokensOnly />
                  <p class="stats-note">Known only in total; already in the session totals.</p>
                </section>
              )}
              {!!current.limit && current.children.length >= current.limit && (
                <p class="stats-note">Most recent {current.limit} child sessions.</p>
              )}
              {models.some((row) => row.running) && <p class="stats-note">* running</p>}
              {models.some((row) => row.active_ms !== undefined) && (
                <p class="stats-note">Time summed across runs.</p>
              )}
              {(stats.cost_partial || models.some((row) => row.cost_partial)) && (
                <p class="stats-note">≥ lower bound.</p>
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
                        <dd title={row.exact}>{row.value}</dd>
                      </div>
                    ))}
                  </dl>
                </section>
              ))}
              {!stats.sections?.some((section) =>
                section.title.startsWith('Cumulative Token Usage'),
              ) &&
                hasCounters(stats.metrics) && (
                  <section>
                    <h3 class="stats-section-title">Token usage</h3>
                    <Counters metrics={stats.metrics} />
                  </section>
                )}
              {stats.durable_metrics && (
                <section>
                  <h3 class="stats-section-title">Recorded parent-session totals</h3>
                  <Counters metrics={stats.durable_metrics} />
                  <p class="stats-note">Separate counters; do not add to the figures above.</p>
                </section>
              )}
            </>
          )}
        </>
      )}
    </Overlay>
  );
}
