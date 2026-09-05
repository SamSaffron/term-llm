import { useCallback, useLayoutEffect, useRef } from 'preact/hooks';

/** Stable event identity with the latest committed closure.
 * For browser events, not callbacks invoked by a child's layout effect.
 */
export function useEventCallback<Args extends unknown[], Result>(
  callback: (...args: Args) => Result,
) {
  const latest = useRef(callback);
  useLayoutEffect(() => {
    latest.current = callback;
  });
  return useCallback((...args: Args) => latest.current(...args), []);
}
