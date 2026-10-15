import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { PresenceSource } from '../platform/presence';
import { AttentionAckScheduler, type AttentionCandidate } from './attention-ack';

const policy = { dwellMs: 2_000, presenceLeadMs: 60_000, lateTimerToleranceMs: 1_000 };

function harness(initial: Partial<AttentionCandidate> | null = {}) {
  let now = 1_000_000;
  let candidate: AttentionCandidate | null = initial
    ? { sessionId: 's1', storeInstanceId: 'store', throughSeq: 42, epoch: 1, ...initial }
    : null;
  const state = {
    focused: true,
    visible: true,
    online: true,
    obstructed: false,
    lastPresenceAt: now,
  };
  const commit = vi.fn(async (_candidate: AttentionCandidate) => {
    candidate = null;
  });
  const presence: PresenceSource = {
    lastPresenceAt: () => state.lastPresenceAt,
    focused: () => state.focused,
    subscribe: () => () => undefined,
  };
  const scheduler = new AttentionAckScheduler(
    {
      candidate: () => candidate,
      commit,
      online: () => state.online,
      obstructed: () => state.obstructed,
    },
    presence,
    { now: () => now, visible: () => state.visible },
    policy,
  );
  return {
    scheduler,
    commit,
    state,
    setCandidate: (next: Partial<AttentionCandidate> | null) => {
      candidate = next
        ? { sessionId: 's1', storeInstanceId: 'store', throughSeq: 42, epoch: 1, ...next }
        : null;
    },
    advance: async (ms: number) => {
      now += ms;
      await vi.advanceTimersByTimeAsync(ms);
    },
    skipClock: (ms: number) => {
      now += ms;
    },
    present: () => {
      state.lastPresenceAt = now;
    },
  };
}

beforeEach(() => vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] }));
afterEach(() => vi.useRealTimers());

describe('AttentionAckScheduler', () => {
  it('acknowledges the captured marker after a continuous dwell while attended', async () => {
    const h = harness();
    h.scheduler.setAtTail('s1', true);
    await h.advance(1_999);
    expect(h.commit).not.toHaveBeenCalled();
    await h.advance(1);
    expect(h.commit).toHaveBeenCalledWith({
      sessionId: 's1',
      storeInstanceId: 'store',
      throughSeq: 42,
      epoch: 1,
    });
  });

  it('never acknowledges a visible but unfocused window', async () => {
    const h = harness();
    h.state.focused = false;
    h.scheduler.setAtTail('s1', true);
    await h.advance(60_000);
    h.scheduler.evaluate();
    await h.advance(60_000);
    expect(h.commit).not.toHaveBeenCalled();
  });

  it.each([
    ['hidden', (h: ReturnType<typeof harness>) => (h.state.visible = false)],
    ['offline', (h: ReturnType<typeof harness>) => (h.state.online = false)],
  ])('does not acknowledge while %s', async (_name, apply) => {
    const h = harness();
    apply(h);
    h.scheduler.setAtTail('s1', true);
    await h.advance(10_000);
    expect(h.commit).not.toHaveBeenCalled();
  });

  it('keeps a completion that arrived while idle unseen until fresh presence', async () => {
    const h = harness(null);
    h.scheduler.setAtTail('s1', true);
    h.skipClock(5 * 60_000);
    h.setCandidate({});
    h.scheduler.evaluate();
    await h.advance(30_000);
    expect(h.commit).not.toHaveBeenCalled();

    h.present();
    h.scheduler.evaluate();
    await h.advance(2_000);
    expect(h.commit).toHaveBeenCalledOnce();
  });

  it('accepts presence shortly before the completion arrived', async () => {
    const h = harness(null);
    h.scheduler.setAtTail('s1', true);
    h.skipClock(59_000);
    h.setCandidate({});
    h.scheduler.evaluate();
    await h.advance(2_000);
    expect(h.commit).toHaveBeenCalledOnce();
  });

  it('waits while an overlay covers the transcript', async () => {
    const h = harness();
    h.state.obstructed = true;
    h.scheduler.setAtTail('s1', true);
    await h.advance(10_000);
    expect(h.commit).not.toHaveBeenCalled();
    h.state.obstructed = false;
    h.present();
    h.scheduler.evaluate();
    await h.advance(2_000);
    expect(h.commit).toHaveBeenCalledOnce();
  });

  it('waits until the transcript is back at its tail', async () => {
    const h = harness();
    h.scheduler.setAtTail('s1', false);
    await h.advance(10_000);
    expect(h.commit).not.toHaveBeenCalled();
    h.scheduler.setAtTail('s1', true);
    await h.advance(2_000);
    expect(h.commit).toHaveBeenCalledOnce();
  });

  it('ignores a tail report for another session', async () => {
    const h = harness();
    h.scheduler.setAtTail('other', true);
    await h.advance(10_000);
    expect(h.commit).not.toHaveBeenCalled();
  });

  it('restarts the dwell when focus is lost mid-dwell', async () => {
    const h = harness();
    h.scheduler.setAtTail('s1', true);
    await h.advance(1_500);
    h.state.focused = false;
    h.scheduler.evaluate();
    await h.advance(1_000);
    h.state.focused = true;
    h.present();
    h.scheduler.evaluate();
    await h.advance(1_999);
    expect(h.commit).not.toHaveBeenCalled();
    await h.advance(1);
    expect(h.commit).toHaveBeenCalledOnce();
  });

  it('re-arms for a newer marker that arrives mid-dwell', async () => {
    const h = harness();
    h.scheduler.setAtTail('s1', true);
    await h.advance(1_500);
    h.setCandidate({ throughSeq: 43 });
    h.scheduler.evaluate();
    await h.advance(1_999);
    expect(h.commit).not.toHaveBeenCalled();
    await h.advance(1);
    expect(h.commit).toHaveBeenCalledWith(expect.objectContaining({ throughSeq: 43 }));
  });

  it('does not let repeated polls shorten the dwell', async () => {
    const h = harness();
    h.scheduler.setAtTail('s1', true);
    for (let i = 0; i < 10; i++) {
      h.scheduler.evaluate();
      await h.advance(100);
    }
    expect(h.commit).not.toHaveBeenCalled();
    await h.advance(1_000);
    expect(h.commit).toHaveBeenCalledOnce();
  });

  it('restarts the dwell when the timer fires late after sleep', async () => {
    const h = harness();
    h.scheduler.setAtTail('s1', true);
    h.skipClock(10 * 60_000);
    h.present();
    await h.advance(2_000);
    expect(h.commit).not.toHaveBeenCalled();
    await h.advance(2_000);
    expect(h.commit).toHaveBeenCalledOnce();
  });

  it('restarts an overdue dwell even when another trigger notices it first', async () => {
    const h = harness();
    h.scheduler.setAtTail('s1', true);
    h.skipClock(10 * 60_000);
    h.present();
    h.scheduler.evaluate();
    expect(h.commit).not.toHaveBeenCalled();
    await h.advance(1_999);
    expect(h.commit).not.toHaveBeenCalled();
    await h.advance(1);
    expect(h.commit).toHaveBeenCalledOnce();
  });

  it('lets unambiguous engagement override a manual unread', () => {
    const h = harness();
    h.scheduler.suppress({ sessionId: 's1', storeInstanceId: 'store', throughSeq: 42, epoch: 1 });
    h.scheduler.evaluate({ immediate: true });
    expect(h.commit).toHaveBeenCalledOnce();
  });

  it('acknowledges immediately on unambiguous engagement', () => {
    const h = harness();
    h.state.focused = false;
    h.scheduler.evaluate({ immediate: true });
    expect(h.commit).toHaveBeenCalledOnce();
  });

  it('keeps a suppressed marker unseen until a new selection epoch', async () => {
    const h = harness();
    h.scheduler.setAtTail('s1', true);
    h.scheduler.suppress({ sessionId: 's1', storeInstanceId: 'store', throughSeq: 42, epoch: 1 });
    h.scheduler.evaluate();
    await h.advance(10_000);
    expect(h.commit).not.toHaveBeenCalled();

    h.setCandidate({ epoch: 2 });
    h.present();
    h.scheduler.evaluate();
    await h.advance(2_000);
    expect(h.commit).toHaveBeenCalledOnce();
  });

  it('retries after a failed acknowledgement only on a later trigger and dwell', async () => {
    const h = harness();
    h.commit.mockRejectedValueOnce(new Error('offline'));
    h.scheduler.setAtTail('s1', true);
    await h.advance(2_000);
    expect(h.commit).toHaveBeenCalledOnce();
    await h.advance(10_000);
    expect(h.commit).toHaveBeenCalledOnce();
    h.present();
    h.scheduler.evaluate();
    await h.advance(2_000);
    expect(h.commit).toHaveBeenCalledTimes(2);
  });

  it('stops after dispose', async () => {
    const h = harness();
    h.scheduler.setAtTail('s1', true);
    h.scheduler.dispose();
    await h.advance(10_000);
    expect(h.commit).not.toHaveBeenCalled();
  });
});
