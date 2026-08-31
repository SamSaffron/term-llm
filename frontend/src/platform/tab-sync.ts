export type TabEventType =
  | 'session-upserted'
  | 'session-removed'
  | 'run-changed'
  | 'draft-changed'
  | 'review-comment-changed'
  | 'interaction-changed'
  | 'attention-changed';

export interface TabEventV1 {
  v: 1;
  eventId: string;
  originTabId: string;
  operationId: string;
  type: TabEventType;
  sessionId?: string;
  responseId?: string;
  revision?: number;
  occurredAt: number;
}

const EVENT_TYPES = new Set<TabEventType>([
  'session-upserted',
  'session-removed',
  'run-changed',
  'draft-changed',
  'review-comment-changed',
  'interaction-changed',
  'attention-changed',
]);
const uuid = (): string => globalThis.crypto?.randomUUID?.() || Math.random().toString(36).slice(2);

export function parseTabEvent(value: unknown): TabEventV1 | 'legacy' | null {
  if (!value || typeof value !== 'object') return null;
  const raw = value as Record<string, unknown>;
  if (raw.type === 'sessions-changed' && raw.v === undefined) return 'legacy';
  if (
    raw.v !== 1 ||
    typeof raw.eventId !== 'string' ||
    !raw.eventId ||
    typeof raw.originTabId !== 'string' ||
    !raw.originTabId ||
    typeof raw.operationId !== 'string' ||
    !raw.operationId ||
    !EVENT_TYPES.has(raw.type as TabEventType) ||
    typeof raw.occurredAt !== 'number' ||
    !Number.isFinite(raw.occurredAt)
  )
    return null;
  if (
    raw.revision !== undefined &&
    (!Number.isSafeInteger(raw.revision) || Number(raw.revision) < 0)
  )
    return null;
  return {
    v: 1,
    eventId: raw.eventId,
    originTabId: raw.originTabId,
    operationId: raw.operationId,
    type: raw.type as TabEventType,
    ...(typeof raw.sessionId === 'string' && raw.sessionId ? { sessionId: raw.sessionId } : {}),
    ...(typeof raw.responseId === 'string' && raw.responseId ? { responseId: raw.responseId } : {}),
    ...(raw.revision !== undefined ? { revision: Number(raw.revision) } : {}),
    occurredAt: raw.occurredAt,
  };
}

/** Browser messages only accelerate server reconciliation; this class never
 * treats timestamps or peer payloads as authoritative state. */
export class TabSync {
  readonly tabId = uuid();
  private readonly seen = new Set<string>();
  private readonly seenOperations = new Set<string>();
  private readonly order: Array<{ eventId: string; operation: string }> = [];

  constructor(private readonly maxSeen = 512) {}

  create(
    type: TabEventType,
    detail: Partial<Pick<TabEventV1, 'sessionId' | 'responseId' | 'revision'>> = {},
    operationId = uuid(),
  ): TabEventV1 {
    return {
      v: 1,
      eventId: uuid(),
      originTabId: this.tabId,
      operationId,
      type,
      ...detail,
      occurredAt: Date.now(),
    };
  }

  accept(value: unknown): TabEventV1 | 'legacy' | null {
    const event = parseTabEvent(value);
    if (!event || event === 'legacy') return event;
    const operation = `${event.type}:${event.operationId}`;
    if (
      event.originTabId === this.tabId ||
      this.seen.has(event.eventId) ||
      this.seenOperations.has(operation)
    )
      return null;
    this.seen.add(event.eventId);
    this.seenOperations.add(operation);
    this.order.push({ eventId: event.eventId, operation });
    while (this.order.length > this.maxSeen) {
      const expired = this.order.shift();
      if (expired) {
        this.seen.delete(expired.eventId);
        this.seenOperations.delete(expired.operation);
      }
    }
    return event;
  }
}
