import { afterEach, describe, expect, it, vi } from 'vitest';
import { StreamSupervisors } from './stream-supervisor';

describe('StreamSupervisors', () => {
  afterEach(() => vi.useRealTimers());

  it('invalidates the old generation before aborting it', () => {
    const supervisors = new StreamSupervisors();
    const first = supervisors.begin('s1', 'r1');
    let ownedDuringAbort = true;
    first.abort.signal.addEventListener('abort', () => {
      ownedDuringAbort = supervisors.owns(first);
    });

    const second = supervisors.begin('s1', 'r2');

    expect(ownedDuringAbort).toBe(false);
    expect(supervisors.owns(first)).toBe(false);
    expect(supervisors.owns(second)).toBe(true);
    expect(second.generation).toBe(first.generation + 1);
  });

  it('keeps the post owner while adopting a durable response and session ID', () => {
    const supervisors = new StreamSupervisors();
    const owner = supervisors.begin('draft_1', 'pending_1');

    expect(supervisors.adoptResponse(owner, 'r1')).toBe(true);
    expect(supervisors.rekey(owner, 's1')).toBe(true);

    expect(supervisors.current('draft_1')).toBeUndefined();
    expect(supervisors.current('s1')).toBe(owner);
    expect(supervisors.owns(owner, 'r1')).toBe(true);
  });

  it('allows one recovery and one owned retry', () => {
    vi.useFakeTimers();
    const supervisors = new StreamSupervisors();
    const owner = supervisors.begin('s1', 'r1');
    const retry = vi.fn();

    expect(supervisors.startRecovery(owner)).toBe(true);
    expect(supervisors.startRecovery(owner)).toBe(false);
    supervisors.finishRecovery(owner);
    expect(supervisors.startRecovery(owner)).toBe(true);
    supervisors.finishRecovery(owner);
    expect(supervisors.scheduleRetry(owner, retry, 100)).toBe(true);
    expect(supervisors.scheduleRetry(owner, retry, 100)).toBe(false);

    vi.advanceTimersByTime(100);
    expect(retry).toHaveBeenCalledOnce();
  });

  it('serializes subscriptions and owns inactivity watchdogs', () => {
    vi.useFakeTimers();
    const supervisors = new StreamSupervisors();
    const owner = supervisors.begin('s1', 'r1');
    const timeout = vi.fn();

    expect(supervisors.startSubscription(owner)).toBe(true);
    expect(supervisors.startSubscription(owner)).toBe(false);
    expect(supervisors.touchWatchdog(owner, timeout, 100)).toBe(true);
    supervisors.touchWatchdog(owner, timeout, 200);
    vi.advanceTimersByTime(100);
    expect(timeout).not.toHaveBeenCalled();
    supervisors.finishSubscription(owner);
    vi.advanceTimersByTime(200);
    expect(timeout).not.toHaveBeenCalled();
    expect(supervisors.startSubscription(owner)).toBe(true);
  });

  it('clears retries on cancel, replacement, retirement, and disposal', () => {
    vi.useFakeTimers();
    const supervisors = new StreamSupervisors();
    const callbacks = [vi.fn(), vi.fn(), vi.fn(), vi.fn()];

    const cancelled = supervisors.begin('s1', 'r1');
    supervisors.scheduleRetry(cancelled, callbacks[0], 10);
    supervisors.cancel('s1', 'r1');

    const replaced = supervisors.begin('s2', 'r1');
    supervisors.scheduleRetry(replaced, callbacks[1], 10);
    supervisors.begin('s2', 'r2');

    const retired = supervisors.begin('s3', 'r1');
    supervisors.scheduleRetry(retired, callbacks[2], 10);
    supervisors.retire(retired);

    const disposed = supervisors.begin('s4', 'r1');
    supervisors.scheduleRetry(disposed, callbacks[3], 10);
    supervisors.dispose();

    vi.runAllTimers();
    callbacks.forEach((callback) => expect(callback).not.toHaveBeenCalled());
  });
});
