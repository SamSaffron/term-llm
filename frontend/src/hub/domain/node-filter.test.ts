import { describe, expect, it } from 'vitest';
import { mergeShownOrder, nodeFilterHref, nodeMatchesFilter, parseNodeFilter } from './node-filter';
import type { HubNode } from './types';

const node = (reachable: boolean) => ({ id: 'n', status: { reachable } }) as HubNode;

describe('Hub node filter', () => {
  it.each([
    ['', 'online'],
    ['?nodes=online', 'online'],
    ['?nodes=offline', 'offline'],
    ['?a=1&nodes=all', 'all'],
    ['?nodes=ALL', 'online'],
    ['?nodes=bogus', 'online'],
  ])('reads %j as %s', (search, expected) => {
    expect(parseNodeFilter(search)).toBe(expected);
  });

  it('writes non-default filters to the URL and drops the default, keeping the rest', () => {
    const base = 'https://hub.test/hub/?a=1#frag';
    expect(nodeFilterHref(base, 'offline')).toBe('https://hub.test/hub/?a=1&nodes=offline#frag');
    expect(nodeFilterHref('https://hub.test/hub/?nodes=all&a=1', 'online')).toBe(
      'https://hub.test/hub/?a=1',
    );
    expect(nodeFilterHref('https://hub.test/hub/?nodes=offline', 'all')).toBe(
      'https://hub.test/hub/?nodes=all',
    );
  });

  it('matches nodes by reachability', () => {
    expect(nodeMatchesFilter(node(true), 'online')).toBe(true);
    expect(nodeMatchesFilter(node(false), 'online')).toBe(false);
    expect(nodeMatchesFilter(node(false), 'offline')).toBe(true);
    expect(nodeMatchesFilter(node(true), 'all')).toBe(true);
    expect(nodeMatchesFilter({ id: 'x' } as HubNode, 'offline')).toBe(true);
  });

  it('reorders shown nodes within the places they hold', () => {
    expect(mergeShownOrder(['a', 'x', 'b', 'y', 'c'], ['c', 'a', 'b'])).toEqual([
      'c',
      'x',
      'a',
      'y',
      'b',
    ]);
    // A shown node the full list no longer has is ignored.
    expect(mergeShownOrder(['a', 'x', 'b'], ['b', 'gone', 'a'])).toEqual(['b', 'x', 'a']);
  });
});
