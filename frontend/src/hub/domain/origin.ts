export type PasskeyOriginCheck =
  { kind: 'ok' } | { kind: 'redirect'; url: string } | { kind: 'mismatch'; url: string };

type BrowserLocation = Pick<Location, 'origin' | 'pathname' | 'search' | 'hash'>;

const loopbackAddresses = new Set(['127.0.0.1', '[::1]']);

/**
 * Passkeys and session cookies are bound to the configured origin. A loopback
 * IP alias of a configured localhost origin (same scheme and port) reaches the
 * same listener, so it is safe to move there automatically. Any other origin
 * may be a tunnel or proxy that the configured URL cannot replace, so it is
 * only reported.
 */
export function checkPasskeyOrigin(
  location: BrowserLocation,
  configured: string,
): PasskeyOriginCheck {
  if (!configured || location.origin === configured) return { kind: 'ok' };
  const target = new URL(configured);
  const current = new URL(location.origin);
  const url = `${configured}${location.pathname}${location.search}${location.hash}`;
  if (
    target.hostname === 'localhost' &&
    loopbackAddresses.has(current.hostname) &&
    target.protocol === current.protocol &&
    target.port === current.port
  ) {
    return { kind: 'redirect', url };
  }
  return { kind: 'mismatch', url };
}
