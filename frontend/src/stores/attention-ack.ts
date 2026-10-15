import { ATTENTION_POLICY, type PresenceSource } from '../platform/presence';

/** The exact attention marker the selected session could acknowledge. */
export interface AttentionCandidate {
  sessionId: string;
  storeInstanceId: string;
  throughSeq: number;
  epoch: number;
}

export interface AttentionAckHost {
  /** The selected session's acknowledgeable marker once its final bodies are installed. */
  candidate(): AttentionCandidate | null;
  /** Durably acknowledge exactly this marker. */
  commit(candidate: AttentionCandidate): Promise<void>;
  online(): boolean;
  /** Whether a modal, drawer, or viewer covers the transcript. */
  obstructed?(): boolean;
}

export interface AttentionAckOptions {
  now?: () => number;
  visible?: () => boolean;
}

type Policy = Readonly<
  Record<
    keyof Pick<typeof ATTENTION_POLICY, 'dwellMs' | 'presenceLeadMs' | 'lateTimerToleranceMs'>,
    number
  >
>;

const keyOf = (candidate: AttentionCandidate) =>
  [candidate.storeInstanceId, candidate.sessionId, candidate.throughSeq, candidate.epoch].join(
    '\u0000',
  );

/**
 * Decides when the selected session's completion was actually noticed.
 *
 * Seen state is shared by every client of a session, so a false "seen" from a
 * forgotten window hides finished work everywhere while a false "unseen" costs
 * one interaction. Automatic acknowledgement therefore requires the document to
 * be visible and focused, the transcript to sit at its tail, and presence no
 * older than {@link ATTENTION_POLICY.presenceLeadMs} before the marker arrived,
 * all holding continuously for {@link ATTENTION_POLICY.dwellMs}.
 *
 * {@link evaluate} is idempotent: status polls, events, and navigation only
 * re-check eligibility and can never acknowledge directly.
 */
export class AttentionAckScheduler {
  private readonly now: () => number;
  private readonly visible: () => boolean;
  private readonly policy: Policy;
  private armed: { key: string; armedAt: number } | null = null;
  private eligibleSince = 0;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private timerDueAt = 0;
  private atTail: { sessionId: string; value: boolean } | null = null;
  private suppressedKey = '';
  private disposed = false;

  constructor(
    private readonly host: AttentionAckHost,
    private readonly presence: PresenceSource,
    options: AttentionAckOptions = {},
    policy: Policy = ATTENTION_POLICY,
  ) {
    this.now = options.now || (() => Date.now());
    this.visible =
      options.visible ||
      (() => typeof document !== 'undefined' && document.visibilityState === 'visible');
    this.policy = policy;
  }

  /** Report whether the transcript for `sessionId` is scrolled to its tail. */
  setAtTail(sessionId: string, value: boolean): void {
    if (this.atTail?.sessionId === sessionId && this.atTail.value === value) return;
    this.atTail = { sessionId, value };
    this.evaluate();
  }

  /** Keep this exact marker unseen until a new selection or a newer marker. */
  suppress(candidate: AttentionCandidate): void {
    this.suppressedKey = keyOf(candidate);
    this.cancelDwell();
  }

  /**
   * Re-check eligibility. `immediate` is for unambiguous engagement with the
   * selected session (sending a follow-up): it skips focus, tail, and dwell.
   */
  evaluate(options: { immediate?: boolean } = {}): void {
    if (this.disposed) return;
    const candidate = this.host.candidate();
    if (!candidate) {
      this.armed = null;
      this.cancelDwell();
      return;
    }
    const key = keyOf(candidate);
    if (this.armed?.key !== key) {
      this.armed = { key, armedAt: this.now() };
      this.cancelDwell();
    }
    if (!this.visible() || !this.host.online()) return this.cancelDwell();
    // Deliberate engagement also overrides a manual "mark unread".
    if (options.immediate) return this.commit(candidate);
    if (this.suppressedKey === key) return;
    if (!this.attended(candidate, this.armed.armedAt)) return this.cancelDwell();
    const now = this.now();
    // An overdue dwell timer (sleep, throttling) is not evidence of continuous
    // attention, whichever trigger notices it first.
    if (this.timer && now - this.timerDueAt > this.policy.lateTimerToleranceMs) this.cancelDwell();
    if (!this.eligibleSince) this.eligibleSince = now;
    const remaining = this.policy.dwellMs - (now - this.eligibleSince);
    if (remaining <= 0) return this.commit(candidate);
    this.schedule(remaining);
  }

  dispose(): void {
    this.disposed = true;
    this.cancelDwell();
  }

  private attended(candidate: AttentionCandidate, armedAt: number): boolean {
    return (
      !this.host.obstructed?.() &&
      this.presence.focused() &&
      this.atTail?.sessionId === candidate.sessionId &&
      this.atTail.value &&
      this.presence.lastPresenceAt() >= armedAt - this.policy.presenceLeadMs
    );
  }

  private commit(candidate: AttentionCandidate): void {
    this.cancelDwell();
    // A failed acknowledgement leaves the marker armed; the next trigger
    // restarts the dwell instead of retrying in a tight loop.
    void this.host.commit(candidate).catch(() => undefined);
  }

  private schedule(delay: number): void {
    if (this.timer) return;
    this.timerDueAt = this.now() + delay;
    this.timer = setTimeout(() => {
      this.timer = null;
      // Timers freeze during sleep and throttle in the background; a late
      // firing is not evidence of continuous attention.
      if (this.now() - this.timerDueAt > this.policy.lateTimerToleranceMs) this.eligibleSince = 0;
      this.evaluate();
    }, delay);
  }

  private cancelDwell(): void {
    this.eligibleSince = 0;
    if (this.timer) clearTimeout(this.timer);
    this.timer = null;
  }
}
