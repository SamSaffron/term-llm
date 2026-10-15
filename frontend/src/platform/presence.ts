/**
 * User-presence evidence for attention acknowledgement.
 *
 * A visible document is not proof that anyone is looking at it: an occluded or
 * second-monitor window stays `visible`. This tracker records the last moment
 * the user demonstrably interacted with this document so attention can stay
 * unseen when a run finishes in a window nobody is attending.
 */

/** Tunable thresholds for automatic attention acknowledgement. */
export const ATTENTION_POLICY = {
  /** Eligibility must hold continuously this long before acknowledging. */
  dwellMs: 2_000,
  /** Presence must be no older than this before the completion arrived. */
  presenceLeadMs: 60_000,
  /** Cumulative pointer travel that counts as deliberate movement. */
  pointerMovePx: 8,
  /** Pointer movement records presence at most this often. */
  pointerMoveSampleMs: 1_000,
  /** A dwell timer firing this late (sleep, throttling) restarts the dwell. */
  lateTimerToleranceMs: 1_000,
} as const;

/** Read-only presence evidence consumed by the attention scheduler. */
export interface PresenceSource {
  /** Wall-clock milliseconds of the last presence signal, or 0 if none. */
  lastPresenceAt(): number;
  /** Whether this document currently holds input focus. */
  focused(): boolean;
  /** Observe presence, focus, and visibility changes. */
  subscribe(listener: () => void): () => void;
}

export interface PresenceTrackerOptions {
  now?: () => number;
  target?: Window;
  doc?: Document;
  coarsePointer?: () => boolean;
}

const DISCRETE_INPUT_EVENTS = ['keydown', 'pointerdown', 'wheel', 'touchstart'] as const;

function defaultCoarsePointer(): boolean {
  return typeof matchMedia === 'function' && matchMedia('(pointer: coarse)').matches;
}

/**
 * Tracks keyboard, pointer, wheel, touch, and focus-gain signals. Scroll events
 * are deliberately ignored because the transcript scrolls itself while
 * streaming; visibility alone is not presence either.
 */
export class PresenceTracker implements PresenceSource {
  private readonly now: () => number;
  private readonly target: Window;
  private readonly doc: Document;
  private readonly coarsePointer: () => boolean;
  private readonly listeners = new Set<() => void>();
  private last = 0;
  private lastMoveMarkAt = 0;
  private lastMoveAt = 0;
  private moveTravel = 0;
  private deferredNotify: ReturnType<typeof setTimeout> | null = null;
  private lastPoint: { x: number; y: number } | null = null;
  private installed = false;

  constructor(options: PresenceTrackerOptions = {}) {
    this.now = options.now || (() => Date.now());
    this.target = options.target || window;
    this.doc = options.doc || document;
    this.coarsePointer = options.coarsePointer || defaultCoarsePointer;
  }

  lastPresenceAt(): number {
    return this.last;
  }

  /**
   * Touch devices report focus unreliably (iOS PWAs in particular), so there
   * visibility plus touch presence stands in for focus. Sampled live rather
   * than cached from blur events, which also fire when focus enters an iframe.
   */
  focused(): boolean {
    return this.coarsePointer() || this.doc.hasFocus();
  }

  subscribe(listener: () => void): () => void {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  install(signal: AbortSignal): void {
    if (this.installed || signal.aborted) return;
    this.installed = true;
    const options = { capture: true, passive: true, signal };
    for (const type of DISCRETE_INPUT_EVENTS)
      this.target.addEventListener(type, this.mark, options);
    this.target.addEventListener('pointermove', this.pointerMoved, options);
    this.target.addEventListener('focus', this.mark, { signal });
    this.target.addEventListener('blur', this.notify, { signal });
    this.doc.addEventListener('visibilitychange', this.notify, { signal });
    signal.addEventListener(
      'abort',
      () => {
        this.listeners.clear();
        if (this.deferredNotify) clearTimeout(this.deferredNotify);
        this.deferredNotify = null;
      },
      { once: true },
    );
    // A document that merely starts focused is not presence: browser session
    // restore, crash recovery, and auto-relaunch focus windows nobody is at.
  }

  /** Record presence explicitly, e.g. for an unambiguous in-app action. */
  readonly mark = (): void => {
    this.last = this.now();
    this.notify();
    // Notify again once the input's own handlers have run (closing an overlay,
    // scrolling to the tail), so eligibility reflects the resulting state.
    if (!this.deferredNotify)
      this.deferredNotify = setTimeout(() => {
        this.deferredNotify = null;
        this.notify();
      }, 0);
  };

  private readonly notify = (): void => {
    for (const listener of [...this.listeners]) listener();
  };

  private readonly pointerMoved = (event: Event): void => {
    const { clientX, clientY } = event as PointerEvent;
    const point = { x: Number(clientX) || 0, y: Number(clientY) || 0 };
    const now = this.now();
    // Travel only accumulates across a continuous gesture; an idle mouse's
    // occasional sensor drift must never add up to presence.
    if (now - this.lastMoveAt > ATTENTION_POLICY.pointerMoveSampleMs) this.moveTravel = 0;
    this.lastMoveAt = now;
    if (this.lastPoint)
      this.moveTravel +=
        Math.abs(point.x - this.lastPoint.x) + Math.abs(point.y - this.lastPoint.y);
    this.lastPoint = point;
    if (this.moveTravel < ATTENTION_POLICY.pointerMovePx) return;
    if (now - this.lastMoveMarkAt < ATTENTION_POLICY.pointerMoveSampleMs) return;
    this.lastMoveMarkAt = now;
    this.moveTravel = 0;
    this.mark();
  };
}
