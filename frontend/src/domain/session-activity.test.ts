import { describe, expect, it } from 'vitest';
import { activityCell, activityChild, activitySnapshot } from '../test/activity-fixtures';
import { activityRows, agentMetrics, statePresentation, taskLabel } from './session-activity';

describe('native session activity presentation', () => {
  it('uses short task labels, never injected prompt dumps or personal model aliases', () => {
    expect(taskLabel(activityChild())).toBe('Fix navigation');
    expect(
      taskLabel(
        activityChild({
          title: '@reviewer: Your Mission: do not edit any code',
          task_summary: 'You are a worker. Read all files.',
        }),
      ),
    ).toBe('Developer task');
    expect(
      taskLabel(
        activityChild({
          title: 'Your Mission: do not edit code',
          task_summary: 'Read frontend/src/main.tsx before continuing',
        }),
      ),
    ).toBe('Read frontend/src/main.tsx before continuing');
    expect(activityRows(activitySnapshot())[0].subtitle).toContain('provider:real-model');
  });

  it('adds only the four disjoint usage counters and uses approximate elapsed wall-clock', () => {
    expect(
      agentMetrics(
        activityChild({ live: false, started_at: 1000, ended_at: 1065, approximate_times: true }),
      ),
    ).toBe('1k tokens / ~1m 5s');
    expect(agentMetrics(activityChild({ started_at: 1000 }), 1065000)).toBe('1k tokens / 1m 5s');
    expect(agentMetrics(activityChild({ input_tokens: NaN, started_at: undefined }))).toBe('');
  });

  it.each([
    ['active', true, 'Running', true],
    ['active', false, 'Status unknown', false],
    ['waiting_for_user', true, 'Waiting', true],
    ['cancel_requested', true, 'Stopping', true],
    ['queued', false, 'Queued', true],
    ['turn_limit', false, 'Turn limit reached', false],
    ['error', true, 'Failed', false],
    ['interrupted', false, 'Interrupted', false],
    ['timed_out', false, 'Timed out', false],
    ['complete', false, 'Completed', false],
  ])('presents lifecycle %s (%s) accurately', (code, live, label, active) => {
    expect(statePresentation(code, live)).toMatchObject({ label, active });
  });

  it('keeps exact job runs, active-first ordering and only trusted session links', () => {
    const rows = activityRows(
      activitySnapshot({
        agents: [
          activityChild({ live: false, state: 'complete' }),
          activityChild({ session_id: 'waiting', pending_asks: 1 }),
        ],
        jobs: [
          {
            job_id: 'j1',
            run_id: 'r1',
            title: 'Remote run',
            status: 'running',
            model: 'actual-job-model',
          },
          {
            job_id: 'j1',
            run_id: 'r2',
            title: 'Local run',
            status: 'complete',
            session_id: 'trusted-local',
          },
        ],
      }),
    );
    expect(rows.map((row) => row.key)).toEqual([
      'agent:waiting',
      'job:j1:r1',
      'agent:child',
      'job:j1:r2',
    ]);
    expect(rows[0].state.label).toBe('Waiting');
    expect(rows[1].sessionId).toBeUndefined();
    expect(rows[1].subtitle).toBe('actual-job-model · Job');
    expect(rows[1].subtitle).not.toContain('tokens');
    expect(rows[3].sessionId).toBe('trusted-local');
  });
});

describe('authoritative execution counting', () => {
  it('counts only live agents and jobs, deduplicates local sessions, and ignores scripts and durable state handoffs', async () => {
    const { activeExecutionCount } = await import('./session-activity');
    const snapshot = activitySnapshot({
      agents: [
        activityChild({ state: 'complete', live: true }),
        activityChild({ session_id: 'queued', state: 'queued', live: false }),
        activityChild({ session_id: 'skill', kind: 'skill_run', live: true }),
      ],
      jobs: [
        {
          job_id: 'j',
          run_id: 'r',
          title: 'Same execution',
          status: 'running',
          session_id: 'child',
        },
        { job_id: 'j', run_id: 'r2', title: 'Remote execution', status: 'queued' },
      ],
    });
    snapshot.cells = [activityCell()];
    expect(activeExecutionCount(snapshot)).toBe(2);
    expect(activityRows(snapshot).find((row) => row.key === 'agent:child')?.active).toBe(true);
    expect(activityRows(snapshot).find((row) => row.key === 'agent:queued')?.active).toBe(false);
    expect(activityRows(snapshot).find((row) => row.key === 'agent:skill')).toBeUndefined();
  });
});
