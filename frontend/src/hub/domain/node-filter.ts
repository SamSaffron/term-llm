import type { HubNode } from './types';

/** Which nodes the dashboard grid shows. Online is the default. */
export type NodeFilter = 'online' | 'offline' | 'all';

export const NODE_FILTERS: readonly NodeFilter[] = ['online', 'offline', 'all'];

/** The query parameter that carries a non-default node filter. */
export const NODE_FILTER_PARAM = 'nodes';

/** Reads the node filter from a URL query string; anything unknown means online. */
export function parseNodeFilter(search: string): NodeFilter {
  const value = new URLSearchParams(search).get(NODE_FILTER_PARAM);
  return value === 'offline' || value === 'all' ? value : 'online';
}

/**
 * Returns `href` carrying `filter`, leaving every other part of the URL alone.
 * The default (online) filter drops the parameter so the plain Hub URL stays clean.
 */
export function nodeFilterHref(href: string, filter: NodeFilter): string {
  const url = new URL(href);
  if (filter === 'online') url.searchParams.delete(NODE_FILTER_PARAM);
  else url.searchParams.set(NODE_FILTER_PARAM, filter);
  return url.href;
}

/** Whether `node` passes `filter`; a cached node uses its last-known reachability. */
export function nodeMatchesFilter(node: HubNode, filter: NodeFilter): boolean {
  if (filter === 'all') return true;
  return Boolean(node.status?.reachable) === (filter === 'online');
}

/**
 * Applies a reorder of the shown nodes to the full node order: the shown nodes
 * take, in their new order, the positions they already occupy, so hidden nodes
 * keep theirs.
 */
export function mergeShownOrder(allIds: readonly string[], shownIds: readonly string[]): string[] {
  const known = new Set(allIds);
  const ordered = shownIds.filter((id) => known.has(id));
  const shown = new Set(ordered);
  let next = 0;
  return allIds.map((id) => (shown.has(id) ? ordered[next++] : id));
}
