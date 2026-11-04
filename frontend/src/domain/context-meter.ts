import type { ContextUsage } from './types';

// Without a server-named compaction point the meter still warns as the window
// fills: amber from 70%, red from 90% (term-llm's default auto-compact point).
const WARM_RATIO = 0.7;
const HOT_RATIO = 0.9;

export type ContextTone = 'calm' | 'warm' | 'hot';

/** One display reading of a session's approximate context occupancy. */
export interface ContextReading {
  used: number;
  /** The model's input window; absent when unknown. */
  limit?: number;
  /** Whole percent of the window used; absent without a window. */
  percent?: number;
  /** Share of the ring to fill, 0..1. */
  fill: number;
  /** Tokens left in the window; absent without a window. */
  free?: number;
  /** Occupancy at which term-llm compacts; absent when it does not. */
  autoCompactAt?: number;
  /** Tokens left before term-llm compacts; 0 once past that point. */
  untilAutoCompact?: number;
  tone: ContextTone;
  estimated: boolean;
}

export function readContext(usage: ContextUsage): ContextReading {
  const used = Math.max(0, usage.usedTokens);
  const limit = usage.inputLimit && usage.inputLimit > 0 ? usage.inputLimit : undefined;
  const reading: ContextReading = { used, fill: 0, tone: 'calm', estimated: usage.estimated };
  if (!limit) return reading;
  const share = used / limit;
  const autoCompactAt =
    usage.autoCompactAt && usage.autoCompactAt > 0 && usage.autoCompactAt <= limit
      ? usage.autoCompactAt
      : undefined;
  const hotAt = autoCompactAt ?? limit * HOT_RATIO;
  // A configured early compaction point pulls the amber band down with it.
  const warmAt = Math.min(limit * WARM_RATIO, hotAt * (WARM_RATIO / HOT_RATIO));
  return {
    ...reading,
    limit,
    percent: Math.round(share * 100),
    // A sliver keeps a small but non-zero reading visible on the ring.
    fill: used > 0 ? Math.min(1, Math.max(0.02, share)) : 0,
    free: Math.max(0, limit - used),
    ...(autoCompactAt === undefined
      ? {}
      : { autoCompactAt, untilAutoCompact: Math.max(0, autoCompactAt - used) }),
    tone: used >= hotAt ? 'hot' : used >= warmAt ? 'warm' : 'calm',
  };
}

/**
 * Abbreviates a token count the way the TUI does, with lowercase k:
 * 35_200 → 35.2k, 160_000 → 160k. The unit is chosen after rounding, so
 * 999_950 reads 1M rather than 1000k.
 */
export function formatTokens(value: number): string {
  const abs = Math.abs(value);
  for (const [scale, suffix] of [
    [1_000_000_000, 'B'],
    [1_000_000, 'M'],
    [1_000, 'k'],
  ] as const)
    if (abs >= scale * 0.9995) return `${(value / scale).toFixed(1).replace(/\.0$/, '')}${suffix}`;
  return String(Math.round(value));
}

/** The percent label; a non-zero reading never shows as 0%. */
export function formatPercent(reading: ContextReading): string {
  if (reading.percent === undefined) return '';
  return reading.percent === 0 && reading.used > 0 ? '<1%' : `${reading.percent}%`;
}

/** Full wording for hover text and accessible names. */
export function describeContext(reading: ContextReading): string {
  const used = formatTokens(reading.used);
  const estimate = reading.estimated ? ' (estimated)' : '';
  if (reading.limit === undefined) return `${used} tokens, window size unknown${estimate}`;
  return `${formatPercent(reading)} used, ${used} of ${formatTokens(reading.limit)} tokens${estimate}`;
}
