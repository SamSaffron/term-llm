const minute = 60_000;
const hour = 60 * minute;
const day = 24 * hour;

/** Compact transcript ages round to seconds; activity labels use exact elapsed time. */
export function relativeTime(
  value: number,
  { now = Date.now(), compact = false, justNowThreshold = minute } = {},
): string {
  const elapsed = Math.max(0, now - value);
  const difference = compact ? Math.round(elapsed / 1000) * 1000 : elapsed;
  if (difference < justNowThreshold) return compact ? 'now' : 'just now';
  if (!compact && difference >= 7 * day)
    return new Date(value).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
  const unit = difference < hour ? minute : difference < day ? hour : day;
  const suffix = difference < hour ? 'm' : difference < day ? 'h' : 'd';
  return `${Math.max(1, Math.floor(difference / unit))}${suffix}${compact ? '' : ' ago'}`;
}
