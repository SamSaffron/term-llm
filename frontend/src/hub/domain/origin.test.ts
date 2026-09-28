import { describe, expect, it } from 'vitest';
import { checkPasskeyOrigin } from './origin';

function at(href: string) {
  const url = new URL(href);
  return { origin: url.origin, pathname: url.pathname, search: url.search, hash: url.hash };
}

describe('checkPasskeyOrigin', () => {
  it('accepts the configured origin or a missing configuration', () => {
    expect(
      checkPasskeyOrigin(at('http://localhost:8080/ui/auth/login'), 'http://localhost:8080'),
    ).toEqual({ kind: 'ok' });
    expect(checkPasskeyOrigin(at('http://127.0.0.1:8080/ui/auth/login'), '')).toEqual({
      kind: 'ok',
    });
  });

  it('redirects loopback IP aliases of a localhost origin, keeping path and query', () => {
    for (const href of [
      'http://127.0.0.1:8080/ui/auth/login?return=%2Fui%2F',
      'http://[::1]:8080/ui/auth/login?return=%2Fui%2F',
    ]) {
      expect(checkPasskeyOrigin(at(href), 'http://localhost:8080')).toEqual({
        kind: 'redirect',
        url: 'http://localhost:8080/ui/auth/login?return=%2Fui%2F',
      });
    }
  });

  it('only reports other origins because they may be tunnels or proxies', () => {
    for (const [href, configured] of [
      ['http://127.0.0.1:9000/ui/auth/login', 'http://localhost:8080'],
      ['http://192.168.1.5:8080/ui/auth/login', 'http://localhost:8080'],
      ['http://127.0.0.1:443/ui/auth/login', 'https://localhost'],
      ['https://other.example/ui/auth/login', 'https://term.example'],
    ]) {
      expect(checkPasskeyOrigin(at(href), configured)).toEqual({
        kind: 'mismatch',
        url: `${configured}/ui/auth/login`,
      });
    }
  });
});
