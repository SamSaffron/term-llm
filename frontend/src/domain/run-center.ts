import type { ResponseProjection } from './response';
import type { InteractionRecord, Session } from './types';

export interface ChildRun {
  sessionId: string;
  parentSessionId: string;
  parentSpawnItemId?: number;
  parentSpawnCallId?: string;
  title: string;
  agent?: string;
  taskSummary?: string;
  state: string;
  attention: boolean;
  responseId?: string;
  runEpoch?: number;
  revision: number;
  startedAt?: number;
  endedAt?: number;
  approximateTimes?: boolean;
}

export type RunPhase =
  | 'connecting'
  | 'reasoning'
  | 'tool'
  | 'subagent'
  | 'awaiting decision'
  | 'reconnecting'
  | 'cancelling'
  | 'completed'
  | 'failed';

export interface RunCenterItem {
  id: string;
  sessionId: string;
  responseId?: string;
  transcriptItemId?: number;
  parentSpawnItemId?: number;
  parentSessionId?: string;
  title: string;
  phase: RunPhase;
  attention: boolean;
  startedAt?: number;
  endedAt?: number;
  approximateTime: boolean;
  child: boolean;
  queuedInterjections: number;
  summary?: string;
}

export function deriveRunCenter(
  sessions: Session[],
  projections: Record<string, ResponseProjection>,
  interactions: Record<string, InteractionRecord>,
  children: ChildRun[],
  interjections: Array<{ sessionId: string; state: string }> = [],
): RunCenterItem[] {
  const waitingSessions = new Set(
    Object.values(interactions)
      .filter((entry) => ['waiting', 'dismissed', 'failed'].includes(entry.state))
      .map((entry) => entry.sessionId),
  );
  const items: RunCenterItem[] = [];
  for (const [sessionId, projection] of Object.entries(projections)) {
    const session = sessions.find((entry) => entry.id === sessionId);
    const runningTool = projection.messages
      .flatMap((message) => message.tools || [])
      .find((tool) => tool.status === 'running');
    const status = projection.run.status;
    const activePlanStep = projection.plan?.plan.find(
      (step) => step.status === 'in_progress',
    )?.step;
    const terminalSummary = [...projection.messages]
      .reverse()
      .find((message) => message.role === 'assistant' && message.content.trim())
      ?.content.trim()
      .slice(0, 160);
    const queuedInterjections = interjections.filter(
      (entry) => entry.sessionId === sessionId && ['sending', 'pending'].includes(entry.state),
    ).length;
    let phase: RunPhase = 'reasoning';
    if (status === 'connecting')
      phase = projection.run.reconnects > 0 ? 'reconnecting' : 'connecting';
    else if (status === 'cancelling') phase = 'cancelling';
    else if (status === 'cancelled') phase = 'completed';
    else if (status === 'failed') phase = 'failed';
    else if (status === 'completed') phase = 'completed';
    else if (waitingSessions.has(sessionId)) phase = 'awaiting decision';
    else if (runningTool) phase = runningTool.name === 'spawn_agent' ? 'subagent' : 'tool';
    items.push({
      id: `${sessionId}:${projection.run.responseId}`,
      sessionId,
      responseId: projection.run.responseId,
      title: session?.title || 'Run',
      phase,
      attention: waitingSessions.has(sessionId) || status === 'failed',
      startedAt: projection.run.startedAt,
      endedAt: projection.run.endedAt,
      approximateTime: !projection.run.startedAt,
      child: false,
      queuedInterjections,
      summary:
        runningTool?.name ||
        activePlanStep ||
        (['completed', 'cancelled', 'failed'].includes(status)
          ? projection.run.summary || terminalSummary
          : undefined),
    });
  }
  for (const child of children) {
    const state = child.state.toLowerCase();
    items.push({
      id: `child:${child.sessionId}`,
      sessionId: child.sessionId,
      responseId: child.responseId,
      parentSpawnItemId: child.parentSpawnItemId,
      parentSessionId: child.parentSessionId,
      title: child.title || child.agent || 'Agent run',
      phase:
        state === 'error' || state === 'failed'
          ? 'failed'
          : state === 'complete'
            ? 'completed'
            : 'subagent',
      attention: child.attention,
      startedAt: child.startedAt,
      endedAt: child.endedAt,
      approximateTime: Boolean(child.approximateTimes),
      child: true,
      queuedInterjections: 0,
      summary: child.taskSummary,
    });
  }
  return items.sort(
    (left, right) =>
      Number(right.attention) - Number(left.attention) ||
      Number(!['completed', 'failed'].includes(right.phase)) -
        Number(!['completed', 'failed'].includes(left.phase)),
  );
}
