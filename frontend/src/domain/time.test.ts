import { describe, expect, it } from 'vitest';
import { relativeTime } from './time';

const now = Date.parse('2026-09-02T12:00:00Z');

describe('relative time', () => {
  it.each([
    [-1_000, 'now', 'just now', 'just now'],
    [44_999, 'now', 'just now', 'just now'],
    [45_000, 'now', 'just now', '1m ago'],
    [59_499, 'now', 'just now', '1m ago'],
    [59_500, '1m', 'just now', '1m ago'],
    [60_000, '1m', '1m ago', '1m ago'],
    [3_599_500, '1h', '59m ago', '59m ago'],
    [3_600_000, '1h', '1h ago', '1h ago'],
    [86_399_500, '1d', '23h ago', '23h ago'],
    [86_400_000, '1d', '1d ago', '1d ago'],
    [604_799_999, '7d', '6d ago', '6d ago'],
  ])('preserves each surface at age %i ms', (age, compact, session, activity) => {
    expect(relativeTime(now - age, { now, compact: true })).toBe(compact);
    expect(relativeTime(now - age, { now })).toBe(session);
    expect(relativeTime(now - age, { now, justNowThreshold: 45_000 })).toBe(activity);
  });

  it('uses local dates after a week only for activity labels', () => {
    const value = now - 604_800_000;
    const date = new Date(value).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
    expect(relativeTime(value, { now })).toBe(date);
    expect(relativeTime(value, { now, justNowThreshold: 45_000 })).toBe(date);
    expect(relativeTime(value, { now, compact: true })).toBe('7d');
  });
});
