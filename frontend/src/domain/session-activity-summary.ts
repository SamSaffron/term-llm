import type { ActivitySnapshot } from '../api/endpoints';

/** Count executions, not state labels: live registry membership is authoritative. */
export function activeExecutionCount(snapshot: ActivitySnapshot | null): number {
  const executions = new Set<string>();
  for (const child of snapshot?.agents || []) {
    if (child.kind !== 'skill_run' && child.live === true)
      executions.add(`session:${child.session_id}`);
  }
  for (const job of snapshot?.jobs || []) {
    if (['queued', 'claimed', 'running', 'cancel_requested'].includes(job.status))
      executions.add(
        job.session_id ? `session:${job.session_id}` : `job:${job.job_id}:${job.run_id}`,
      );
  }
  for (const cell of snapshot?.cells || []) {
    if (cell.live) executions.add(`cell:${cell.cell_id}`);
  }
  return executions.size;
}

/** Incomplete discovery is a coverage caveat, not an unknown execution status. */
export function activityStatusUnknown(snapshot: ActivitySnapshot | null, stale = false): boolean {
  return (
    stale ||
    !snapshot ||
    snapshot.agents_availability !== 'available' ||
    (snapshot.jobs_availability !== 'available' && Boolean(snapshot.jobs?.length))
  );
}

export function activityUncertain(snapshot: ActivitySnapshot | null, stale = false): boolean {
  return (
    activityStatusUnknown(snapshot, stale) ||
    Boolean(snapshot?.agents_partial || snapshot?.jobs_partial)
  );
}
