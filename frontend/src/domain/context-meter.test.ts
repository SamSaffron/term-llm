import { describe, expect, it } from 'vitest';
import { describeContext, formatPercent, formatTokens, readContext } from './context-meter';

const usage = (usedTokens: number, inputLimit?: number, autoCompactAt?: number) => ({
  usedTokens,
  cachedInputTokens: 0,
  estimated: true,
  ...(inputLimit === undefined ? {} : { inputLimit }),
  ...(autoCompactAt === undefined ? {} : { autoCompactAt }),
});

describe('context meter reading', () => {
  it('abbreviates token counts like the TUI', () => {
    expect(formatTokens(950)).toBe('950');
    expect(formatTokens(35_200)).toBe('35.2k');
    expect(formatTokens(160_000)).toBe('160k');
    expect(formatTokens(124_800)).toBe('124.8k');
    expect(formatTokens(999_950)).toBe('1M');
    expect(formatTokens(1_048_576)).toBe('1M');
  });

  it('reads occupancy against the window and the auto-compaction point', () => {
    const reading = readContext(usage(35_200, 160_000, 144_000));
    expect(reading).toMatchObject({
      percent: 22,
      free: 124_800,
      autoCompactAt: 144_000,
      untilAutoCompact: 108_800,
      tone: 'calm',
    });
    expect(reading.fill).toBeCloseTo(0.22);
    expect(describeContext(reading)).toBe('22% used, 35.2k of 160k tokens (estimated)');
  });

  it('warms at 70% and turns hot at the auto-compaction point', () => {
    expect(readContext(usage(111_000, 160_000)).tone).toBe('calm');
    expect(readContext(usage(112_000, 160_000)).tone).toBe('warm');
    expect(readContext(usage(144_000, 160_000)).tone).toBe('hot');
    // An earlier configured point moves both bands.
    expect(readContext(usage(70_000, 160_000, 80_000)).tone).toBe('warm');
    expect(readContext(usage(80_000, 160_000, 80_000))).toMatchObject({
      tone: 'hot',
      untilAutoCompact: 0,
    });
  });

  it('never shows a non-zero reading as empty and clamps an overfull ring', () => {
    const tiny = readContext(usage(300, 1_000_000));
    expect(formatPercent(tiny)).toBe('<1%');
    expect(tiny.fill).toBeGreaterThan(0);
    const over = readContext(usage(170_000, 160_000));
    expect(formatPercent(over)).toBe('106%');
    expect(over).toMatchObject({ fill: 1, free: 0, tone: 'hot' });
  });

  it('reports only the token count when the window is unknown', () => {
    const reading = readContext(usage(35_200, undefined, 144_000));
    expect(reading).toEqual({ used: 35_200, fill: 0, tone: 'calm', estimated: true });
    expect(formatPercent(reading)).toBe('');
    expect(describeContext(reading)).toBe('35.2k tokens, window size unknown (estimated)');
  });

  it('ignores an auto-compaction point outside the window', () => {
    expect(readContext(usage(35_200, 160_000, 400_000))).not.toHaveProperty('autoCompactAt');
  });
});
