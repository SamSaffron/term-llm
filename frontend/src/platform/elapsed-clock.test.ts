import { afterEach, describe, expect, it, vi } from 'vitest';
import { formatElapsedDuration, subscribeElapsedClock } from './elapsed-clock';

afterEach(() => {
  vi.useRealTimers();
});

describe('elapsed clock', () => {
  it('formats compact durations at second, minute, and hour boundaries', () => {
    expect(formatElapsedDuration(-1)).toBe('0s');
    expect(formatElapsedDuration(59_999)).toBe('59s');
    expect(formatElapsedDuration(60_000)).toBe('1m');
    expect(formatElapsedDuration(90_000)).toBe('1m30s');
    expect(formatElapsedDuration(3_600_000)).toBe('1h');
    expect(formatElapsedDuration(3_720_000)).toBe('1h02m');
  });

  it('shares one timer across subscribers and stops it after the final unsubscribe', async () => {
    vi.useFakeTimers();
    const first = vi.fn();
    const second = vi.fn();
    const unsubscribeFirst = subscribeElapsedClock(first);
    const unsubscribeSecond = subscribeElapsedClock(second);

    expect(vi.getTimerCount()).toBe(1);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(first).toHaveBeenCalledOnce();
    expect(second).toHaveBeenCalledOnce();
    expect(vi.getTimerCount()).toBe(1);

    unsubscribeFirst();
    expect(vi.getTimerCount()).toBe(1);
    unsubscribeSecond();
    expect(vi.getTimerCount()).toBe(0);
  });

  it('pauses while hidden and refreshes once when visibility returns', () => {
    vi.useFakeTimers();
    const hidden = Object.getOwnPropertyDescriptor(document, 'hidden');
    const listener = vi.fn();
    let unsubscribe = () => {};
    try {
      Object.defineProperty(document, 'hidden', { configurable: true, value: true });
      unsubscribe = subscribeElapsedClock(listener);
      expect(vi.getTimerCount()).toBe(0);

      Object.defineProperty(document, 'hidden', { configurable: true, value: false });
      document.dispatchEvent(new Event('visibilitychange'));
      expect(listener).toHaveBeenCalledOnce();
      expect(vi.getTimerCount()).toBe(1);
    } finally {
      unsubscribe();
      if (hidden) Object.defineProperty(document, 'hidden', hidden);
    }
  });
});
