export const SERVER_EVENT_TYPES = [
  'session.created',
  'session.metadata_changed',
  'session.transcript_changed',
  'session.runtime_changed',
  'session.attention_changed',
  'session.lifecycle_changed',
  'session.deleted',
  'project.created',
  'project.updated',
  'project.deleted',
  'project.membership_changed',
  'run.started',
  'run.finished',
  'interaction.changed',
  'children.changed',
  'files.changed',
  'snapshot.required',
] as const;

export type ServerEventType = (typeof SERVER_EVENT_TYPES)[number];
const eventTypes = new Set<string>(SERVER_EVENT_TYPES);

export interface ServerEvent {
  v: 1;
  sequence: number;
  instanceId: string;
  type: ServerEventType;
  occurredAt: number;
  sessionId?: string;
  responseId?: string;
  projectId?: string;
  parentSessionId?: string;
  transcriptRev?: number;
  runEpoch?: number;
  operationId?: string;
  reason?: string;
}

export interface ServerEventReady {
  v: 1;
  instanceId: string;
  latestSequence: number;
  heartbeatMs: number;
  replayLimit: number;
}

export interface ServerEventPollResponse {
  object: 'list';
  instanceId: string;
  data: ServerEvent[];
  latestSequence: number;
  nextAfter: number;
  timedOut: boolean;
}

const object = (value: unknown): Record<string, unknown> | null =>
  value !== null && typeof value === 'object' && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
const safeInteger = (value: unknown): value is number =>
  Number.isSafeInteger(value) && Number(value) >= 0;
const optionalString = (source: Record<string, unknown>, key: string): string | undefined => {
  const value = source[key];
  return typeof value === 'string' && value ? value : undefined;
};

export function parseServerEvent(value: unknown): ServerEvent | null {
  const source = object(value);
  if (
    !source ||
    source.v !== 1 ||
    !safeInteger(source.sequence) ||
    typeof source.instance_id !== 'string' ||
    !source.instance_id ||
    typeof source.type !== 'string' ||
    !eventTypes.has(source.type) ||
    !safeInteger(source.occurred_at)
  )
    return null;
  if (
    (source.transcript_rev !== undefined && !safeInteger(source.transcript_rev)) ||
    (source.run_epoch !== undefined && !safeInteger(source.run_epoch))
  )
    return null;
  return {
    v: 1,
    sequence: Number(source.sequence),
    instanceId: source.instance_id,
    type: source.type as ServerEventType,
    occurredAt: Number(source.occurred_at),
    ...(optionalString(source, 'session_id') ? { sessionId: String(source.session_id) } : {}),
    ...(optionalString(source, 'response_id') ? { responseId: String(source.response_id) } : {}),
    ...(optionalString(source, 'project_id') ? { projectId: String(source.project_id) } : {}),
    ...(optionalString(source, 'parent_session_id')
      ? { parentSessionId: String(source.parent_session_id) }
      : {}),
    ...(source.transcript_rev !== undefined
      ? { transcriptRev: Number(source.transcript_rev) }
      : {}),
    ...(source.run_epoch !== undefined ? { runEpoch: Number(source.run_epoch) } : {}),
    ...(optionalString(source, 'operation_id') ? { operationId: String(source.operation_id) } : {}),
    ...(optionalString(source, 'reason') ? { reason: String(source.reason) } : {}),
  };
}

export function parseServerEventReady(value: unknown): ServerEventReady | null {
  const source = object(value);
  if (
    !source ||
    source.v !== 1 ||
    typeof source.instance_id !== 'string' ||
    !source.instance_id ||
    !safeInteger(source.latest_sequence) ||
    !safeInteger(source.heartbeat_ms) ||
    !safeInteger(source.replay_limit)
  )
    return null;
  return {
    v: 1,
    instanceId: source.instance_id,
    latestSequence: Number(source.latest_sequence),
    heartbeatMs: Number(source.heartbeat_ms),
    replayLimit: Number(source.replay_limit),
  };
}

export function parseServerEventPollResponse(value: unknown): ServerEventPollResponse | null {
  const source = object(value);
  if (
    !source ||
    source.object !== 'list' ||
    typeof source.instance_id !== 'string' ||
    !source.instance_id ||
    !Array.isArray(source.data) ||
    !safeInteger(source.latest_sequence) ||
    !safeInteger(source.next_after)
  )
    return null;
  const data = source.data.map(parseServerEvent);
  if (data.some((event) => event === null)) return null;
  return {
    object: 'list',
    instanceId: source.instance_id,
    data: data as ServerEvent[],
    latestSequence: Number(source.latest_sequence),
    nextAfter: Number(source.next_after),
    timedOut: source.timed_out === true,
  };
}

export function eventFeedCapability(value: unknown): boolean {
  const source = object(value);
  const feed = source ? object(source.event_feed) : null;
  return Boolean(feed && feed.version === 1 && feed.sse === true && feed.long_poll === true);
}
