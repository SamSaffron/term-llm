/** Reuse equal JSON response subtrees without depending on object key order. */
function shareJSON<T>(previous: T, next: T): T {
  if (Object.is(previous, next)) return previous;
  if (!previous || !next || typeof previous !== 'object' || typeof next !== 'object') return next;
  if (Array.isArray(previous) && Array.isArray(next)) {
    const result = next.map((item, index) => shareJSON(previous[index], item));
    return (
      result.length === previous.length && result.every((item, index) => item === previous[index])
        ? previous
        : result
    ) as T;
  }
  if (Array.isArray(previous) || Array.isArray(next)) return next;
  const old = previous as Record<string, unknown>;
  const incoming = next as Record<string, unknown>;
  const keys = Object.keys(incoming);
  const result = Object.fromEntries(keys.map((key) => [key, shareJSON(old[key], incoming[key])]));
  return (
    keys.length === Object.keys(old).length &&
    keys.every((key) => Object.hasOwn(old, key) && result[key] === old[key])
      ? previous
      : result
  ) as T;
}

/** Preserve item identity across polls and reorders, and list identity on no-op polls. */
export function reconcileHubItems<T>(previous: T[], next: T[], key: (item: T) => string): T[] {
  const byKey = new Map(previous.map((item) => [key(item), item]));
  const result = next.map((item) => {
    const old = byKey.get(key(item));
    return old === undefined ? item : shareJSON(old, item);
  });
  return result.length === previous.length &&
    result.every((item, index) => item === previous[index])
    ? previous
    : result;
}
