import { afterEach, describe, expect, it, vi } from 'vitest';
import { PresenceTracker } from './presence';

function setup(options: { coarse?: boolean; focused?: boolean } = {}) {
  let now = 10_000;
  const focus = vi.spyOn(document, 'hasFocus').mockReturnValue(options.focused ?? false);
  const abort = new AbortController();
  const tracker = new PresenceTracker({
    now: () => now,
    coarsePointer: () => Boolean(options.coarse),
  });
  const listener = vi.fn();
  tracker.subscribe(listener);
  tracker.install(abort.signal);
  return {
    tracker,
    listener,
    abort,
    focus,
    tick: (ms: number) => {
      now += ms;
      return now;
    },
  };
}

const move = (x: number, y: number) =>
  window.dispatchEvent(Object.assign(new Event('pointermove'), { clientX: x, clientY: y }));

afterEach(() => vi.restoreAllMocks());

describe('PresenceTracker', () => {
  it.each(['keydown', 'pointerdown', 'wheel', 'touchstart', 'focus'])(
    'records %s as presence',
    (type) => {
      const h = setup();
      expect(h.tracker.lastPresenceAt()).toBe(0);
      const at = h.tick(500);
      window.dispatchEvent(new Event(type));
      expect(h.tracker.lastPresenceAt()).toBe(at);
      expect(h.listener).toHaveBeenCalled();
      h.abort.abort();
    },
  );

  it.each(['scroll', 'blur'])('does not record %s as presence', (type) => {
    const h = setup();
    h.tick(500);
    window.dispatchEvent(new Event(type));
    expect(h.tracker.lastPresenceAt()).toBe(0);
    h.abort.abort();
  });

  it('does not record a visibility change as presence but notifies', () => {
    const h = setup();
    document.dispatchEvent(new Event('visibilitychange'));
    expect(h.tracker.lastPresenceAt()).toBe(0);
    expect(h.listener).toHaveBeenCalledOnce();
    h.abort.abort();
  });

  it('requires deliberate pointer travel and samples movement', () => {
    const h = setup();
    move(0, 0);
    h.tick(10);
    move(3, 3);
    expect(h.tracker.lastPresenceAt()).toBe(0);
    const first = h.tick(500);
    move(6, 6);
    expect(h.tracker.lastPresenceAt()).toBe(first);
    h.tick(10);
    move(20, 20);
    expect(h.tracker.lastPresenceAt()).toBe(first);
    for (let i = 0; i < 4; i++) {
      h.tick(300);
      move(40 + i * 10, 40);
    }
    expect(h.tracker.lastPresenceAt()).toBe(first + 10 + 1_200);
    h.abort.abort();
  });

  it('never accumulates idle pointer drift into presence', () => {
    const h = setup();
    move(0, 0);
    for (let i = 1; i <= 20; i++) {
      h.tick(60_000);
      move(i, 0);
    }
    expect(h.tracker.lastPresenceAt()).toBe(0);
    h.abort.abort();
  });

  it('notifies again after the input handlers have run', () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      const h = setup();
      window.dispatchEvent(new Event('keydown'));
      expect(h.listener).toHaveBeenCalledOnce();
      vi.runAllTimers();
      expect(h.listener).toHaveBeenCalledTimes(2);
      h.abort.abort();
    } finally {
      vi.useRealTimers();
    }
  });

  it('does not treat a document that starts focused as present', () => {
    const h = setup({ focused: true });
    expect(h.tracker.lastPresenceAt()).toBe(0);
    h.abort.abort();
  });

  it('samples focus live and treats coarse pointers as focused', () => {
    const fine = setup();
    expect(fine.tracker.focused()).toBe(false);
    fine.focus.mockReturnValue(true);
    expect(fine.tracker.focused()).toBe(true);
    fine.abort.abort();

    const coarse = setup({ coarse: true });
    expect(coarse.tracker.focused()).toBe(true);
    coarse.abort.abort();
  });

  it('stops listening after abort', () => {
    const h = setup();
    h.abort.abort();
    h.tick(500);
    window.dispatchEvent(new Event('keydown'));
    expect(h.tracker.lastPresenceAt()).toBe(0);
    expect(h.listener).not.toHaveBeenCalled();
  });
});
