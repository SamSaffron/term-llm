/** A session's acknowledged attention watermark and its change revision. */
export interface SeenState {
  seenThroughSeq: number;
  /** Server revision bumped on every watermark change; undefined from older nodes. */
  seenRev?: number;
}

/**
 * Merge an incoming seen watermark. A revisioned watermark is authoritative:
 * the newer `seen_rev` wins in either direction (mark unread lowers it), and
 * equal revisions take the incoming value because the server bumps the
 * revision on every change. A side without a revision (a cache written by an
 * older bundle, or an older node) never overrides a revisioned one. Only when
 * neither side has a revision does the larger watermark win.
 */
export function mergeSeenState(
  existing: Partial<SeenState>,
  incoming: Partial<SeenState>,
  storeReplaced: boolean,
): SeenState {
  const existingSeen = Math.max(0, existing.seenThroughSeq || 0);
  const incomingSeen = Math.max(0, incoming.seenThroughSeq || 0);
  if (storeReplaced) return withRev(incomingSeen, incoming.seenRev);
  const existingRev = existing.seenRev;
  const incomingRev = incoming.seenRev;
  if (incomingRev !== undefined && (existingRev === undefined || incomingRev >= existingRev))
    return withRev(incomingSeen, incomingRev);
  if (existingRev !== undefined) return withRev(existingSeen, existingRev);
  return withRev(Math.max(existingSeen, incomingSeen), undefined);
}

/** Parse a server `seen_rev` field, ignoring absent or malformed values. */
export function parseSeenRev(value: unknown): number | undefined {
  if (value === undefined || value === null || value === '') return undefined;
  const rev = Number(value);
  return Number.isSafeInteger(rev) && rev >= 0 ? rev : undefined;
}

function withRev(seenThroughSeq: number, seenRev: number | undefined): SeenState {
  return seenRev === undefined ? { seenThroughSeq } : { seenThroughSeq, seenRev };
}
