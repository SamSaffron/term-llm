import type { ActivitySnapshot, StatsChild } from '../api/endpoints';

const plain = (value: string | undefined) => (value || '').replace(/\s+/g, ' ').trim();
const short = (value: string, max = 76) =>
  value.length <= max ? value : `${value.slice(0, max - 1).replace(/\s+\S*$/, '')}…`;
const instructional =
  /\b(?:READ[- ]ONLY|PLANNING ONLY|user (?:asks|selected)|Your Mission|you are|parent will|prior .*worker|do not|no code edits|implement request fully|task context)\b/i;

/** The agent's configured name as written (`developer`, `codebase`), or '' when unknown. */
export function agentName(child: StatsChild): string {
  const prefixed = child.title.match(/^\s*@([^:]+):/);
  return plain(child.agent || prefixed?.[1]).replace(/^@/, '');
}

/**
 * Short task summary, or '' when only an injected task/system prompt is
 * available: those are never used as row bodies.
 */
export function taskSummary(child: StatsChild): string {
  const title = plain(child.title).replace(/^@[^:]+:\s*/, '');
  const summary = plain(child.task_summary);
  if (summary && summary.length <= 76 && !instructional.test(summary)) return summary;
  if (title && title.length <= 76 && !instructional.test(title)) return title;
  const file = `${title} ${summary}`.match(
    /\b(?:[\w.-]+\/)+[\w.-]+\.(?:[cm]?[jt]sx?|go|rb|py|rs|html|css|sql)\b/,
  );
  return file ? short(file[0], 60) : '';
}

export function statePresentation(state: string, live = false) {
  const code = plain(state).toLowerCase().replace(/[ -]/g, '_');
  if (/^(?:error|failed|failure|timed_out|timeout)$/.test(code))
    return {
      icon: 'error',
      tone: 'error',
      label: code.includes('time') ? 'Timed out' : 'Failed',
      active: false,
    };
  if (/^(?:waiting.*|pending_approval|awaiting.*|blocked|paused|input_required)$/.test(code))
    return {
      icon: 'waiting',
      tone: 'waiting',
      label: /approval/.test(code) ? 'Needs approval' : 'Waiting',
      active: true,
    };
  if (/^(?:queued|pending|scheduled)$/.test(code))
    return {
      icon: 'queued',
      tone: 'quiet',
      label: code === 'scheduled' ? 'Scheduled' : 'Queued',
      active: true,
    };
  if (/^(?:cancel_requested|cancelling|stopping)$/.test(code))
    return { icon: 'stopped', tone: 'waiting', label: 'Stopping', active: true };
  if (/^(?:complete|completed|succeeded|success|finished|turn_limit)$/.test(code))
    return {
      icon: 'done',
      tone: 'done',
      label: code === 'turn_limit' ? 'Turn limit reached' : 'Completed',
      active: false,
    };
  if (/^(?:interrupted|cancelled|canceled|stopped|skipped)$/.test(code))
    return {
      icon: 'stopped',
      tone: 'quiet',
      label: code === 'interrupted' ? 'Interrupted' : code === 'skipped' ? 'Skipped' : 'Stopped',
      active: false,
    };
  if (/^(?:active|running|starting|claimed|connecting)$/.test(code) && live)
    return {
      icon: 'running',
      tone: 'running',
      label: /^(starting|claimed|connecting)$/.test(code) ? 'Starting' : 'Running',
      active: true,
    };
  return { icon: 'unknown', tone: 'quiet', label: 'Status unknown', active: live };
}

function timestamp(value?: number): number | undefined {
  return value && Number.isFinite(value) ? (value < 1e11 ? value * 1000 : value) : undefined;
}

export function elapsed(started?: number, ended?: number, approximate = false): string {
  const start = timestamp(started),
    end = timestamp(ended);
  if (start === undefined || end === undefined || end < start) return '';
  const seconds = Math.floor((end - start) / 1000);
  const hours = Math.floor(seconds / 3600),
    minutes = Math.floor(seconds / 60) % 60;
  const duration = hours
    ? `${hours}h ${minutes}m`
    : seconds >= 60
      ? `${minutes}m ${seconds % 60}s`
      : `${seconds}s`;
  return `${approximate ? '~' : ''}${duration}`;
}

export function agentMetrics(child: StatsChild, now = Date.now()): string {
  // Core counters are disjoint. Reasoning and provider totals are not added again.
  const counters = [
    child.input_tokens,
    child.output_tokens,
    child.cached_input_tokens,
    child.cache_write_tokens,
  ];
  const total = counters.every((value) => Number.isSafeInteger(value) && value >= 0)
    ? counters.reduce((a, b) => a + b, 0)
    : null;
  const compact = (value: number) =>
    value >= 1e6
      ? `${Number((value / 1e6).toFixed(1))}M`
      : value >= 1e3
        ? `${Number((value / 1e3).toFixed(1))}k`
        : String(value);
  return [
    total === null ? '' : `${compact(total)} tokens`,
    elapsed(child.started_at, child.live ? now / 1000 : child.ended_at, child.approximate_times),
  ]
    .filter(Boolean)
    .join(' / ');
}

export interface ActivityRow {
  key: string;
  kind: 'agent' | 'job';
  title: string;
  /** Configured agent name for agent rows; shown beside the task summary. */
  agent?: string;
  subtitle: string;
  sessionId?: string;
  runId?: string;
  active: boolean;
  state: ReturnType<typeof statePresentation>;
}

export { activeExecutionCount, activityUncertain } from './session-activity-summary';

export function activityRows(snapshot: ActivitySnapshot | null, now = Date.now()): ActivityRow[] {
  if (!snapshot) return [];
  const agents: ActivityRow[] = (snapshot.agents || [])
    .filter((child) => child.kind !== 'skill_run')
    .map((child) => {
      const state = statePresentation(
        child.live && (child.pending_asks || child.attention) ? 'waiting' : child.state,
        Boolean(child.live),
      );
      return {
        key: `agent:${child.session_id}`,
        kind: 'agent',
        title: taskSummary(child),
        agent: agentName(child) || 'agent',
        subtitle: [plain(child.model), agentMetrics(child, now)].filter(Boolean).join(' · '),
        sessionId: child.session_id,
        state,
        active: child.live === true,
      };
    });
  const jobs: ActivityRow[] = (snapshot.jobs || []).map((job) => {
    const state = statePresentation(
      job.status,
      ['queued', 'claimed', 'running', 'cancel_requested'].includes(job.status),
    );
    return {
      key: `job:${job.job_id}:${job.run_id}`,
      kind: 'job',
      title: short(plain(job.title)) || 'Job',
      subtitle: [
        plain(job.model),
        'Job',
        elapsed(job.started_at, state.active ? now / 1000 : job.ended_at),
      ]
        .filter(Boolean)
        .join(' · '),
      sessionId: job.session_id,
      runId: job.run_id,
      state,
      active: ['queued', 'claimed', 'running', 'cancel_requested'].includes(job.status),
    };
  });
  return [...agents, ...jobs].sort((a, b) => Number(b.active) - Number(a.active));
}
