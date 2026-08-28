import type { Session } from '../domain/types';

export function sessionSlug(session: Pick<Session, 'id' | 'number'>): string {
  return session.number && session.number > 0 ? String(session.number) : session.id;
}

export function sessionIDFromLocation(prefix: string, pathname = location.pathname): string {
  const base = prefix.replace(/\/$/, '');
  const relative = pathname.startsWith(base) ? pathname.slice(base.length) : '';
  const match = relative.match(/^\/chat\/([^/]+)\/?$/);
  return match ? decodeURIComponent(match[1]) : '';
}

export function updateSessionRoute(prefix: string, session: Session | null, replace = false): void {
  const path = session
    ? `${prefix}/chat/${encodeURIComponent(sessionSlug(session))}`
    : `${prefix}/`;
  const method = replace ? 'replaceState' : 'pushState';
  if (location.pathname !== path) history[method](null, '', path);
}
