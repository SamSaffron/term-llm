/** Persisted 1-based ranks keyed by ID; undefined clears a rank. */
export type OrderRanks = ReadonlyMap<string, number | undefined>;

/** One entry of a saved order, in display order, with its persisted rank. */
export interface OrderEntry {
  readonly id: string;
  readonly rank?: number;
}

/**
 * Moves the listed IDs, in order, into the positions they already occupy in
 * `ids`; every other ID keeps its position.
 */
export function permuteOrder(ids: readonly string[], listed: readonly string[]): string[] {
  const slots = listed.map((id) => ids.indexOf(id)).sort((left, right) => left - right);
  const next = [...ids];
  listed.forEach((id, index) => {
    next[slots[index]] = id;
  });
  return next;
}

/**
 * A server-persisted order that users arrange by hand, such as the pinned
 * conversations or the projects. A new order shows at once through `apply`.
 * Saves run one at a time, and a newer order replaces one not yet sent. Until
 * the newest save settles, server snapshots defer to it through
 * `pendingRank`. If the newest save fails, the order from before the first
 * unsaved change returns.
 */
export class SavedOrder {
  /** Optimistic ranks of the newest unsaved order. */
  private pending: Map<string, number> | null = null;
  /** Ranks from before the first unsaved order, restored when saving fails. */
  private baseline: Map<string, number | undefined> | null = null;
  private generation = 0;
  private queue: Promise<unknown> = Promise.resolve();
  /** Requests stay tracked until the newest save settles so only a later
   * request containing every listed ID may replace a queued one. */
  private readonly requests = new Map<number, ReadonlySet<string>>();

  constructor(private readonly apply: (ranks: OrderRanks) => void) {}

  private coveredByLater(generation: number, listed: readonly string[]): boolean {
    for (const [later, ids] of this.requests)
      if (later > generation && listed.every((id) => ids.has(id))) return true;
    return false;
  }

  /** The unsaved rank a server snapshot must show instead of its own, if any. */
  pendingRank(id: string): number | undefined {
    return this.pending?.get(id);
  }

  /** Stops overriding an entry whose membership changed, such as a new pin. */
  forget(id: string): void {
    this.pending?.delete(id);
    this.baseline?.delete(id);
  }

  /**
   * Moves the listed IDs, in order, into the positions they occupy in
   * `current` (every entry, in display order), shows the result, and saves the
   * listed IDs with `send`, which resolves with the committed ranks. Resolves
   * true once this order is saved and applied, or false when it changed
   * nothing or a newer order superseded it. If this is the newest order and
   * its save fails, the baseline returns and the error is rethrown.
   */
  async save(
    current: readonly OrderEntry[],
    orderedIds: readonly string[],
    send: (listed: string[]) => Promise<OrderRanks>,
  ): Promise<boolean> {
    const currentIDs = current.map((entry) => entry.id);
    const listed = orderedIds.filter(
      (id, index) => currentIDs.includes(id) && orderedIds.indexOf(id) === index,
    );
    const next = permuteOrder(currentIDs, listed);
    if (next.every((id, index) => id === currentIDs[index])) return false;

    this.baseline ??= new Map(current.map((entry) => [entry.id, entry.rank]));
    this.pending = new Map(next.map((id, index) => [id, index + 1]));
    this.apply(this.pending);
    const generation = ++this.generation;
    this.requests.set(generation, new Set(listed));
    const saving = this.queue.then(() =>
      this.coveredByLater(generation, listed) ? null : send(listed),
    );
    this.queue = saving.catch(() => undefined);
    let ranks: OrderRanks | null;
    try {
      ranks = await saving;
    } catch (error) {
      // A later request covering the same IDs decides their outcome. A failed
      // request for a different subset must still be reported to the user.
      if (generation !== this.generation) {
        if (this.coveredByLater(generation, listed)) return false;
        throw error;
      }
      const baseline = this.baseline;
      this.pending = null;
      this.baseline = null;
      this.requests.clear();
      if (baseline) this.apply(baseline);
      throw error;
    }
    if (generation !== this.generation || !ranks) return false;
    this.pending = null;
    this.baseline = null;
    this.requests.clear();
    this.apply(ranks);
    return true;
  }
}
