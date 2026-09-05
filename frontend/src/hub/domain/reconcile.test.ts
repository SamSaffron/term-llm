import { describe, expect, it } from 'vitest';
import { reconcileHubItems } from './reconcile';

const key = (item: { id: string }) => item.id;
describe('Hub response reconciliation', () => {
  it('preserves equal JSON independent of property order', () => {
    const previous = [{ id: 'a', status: { reachable: true, capabilities: ['chat'] } }];
    const next = [{ status: { capabilities: ['chat'], reachable: true }, id: 'a' }];
    expect(reconcileHubItems(previous, next, key)).toBe(previous);
  });
  it('retains unchanged siblings and nested data while accepting changes, additions, and removal', () => {
    const previous = [
      { id: 'a', value: 1, nested: { label: 'same' } },
      { id: 'b', value: 2, nested: { label: 'other' } },
    ];
    const added = { id: 'c', value: 3, nested: { label: 'new' } };
    const result = reconcileHubItems(previous, [{ ...previous[0], value: 4 }, added], key);
    expect(result.map((item) => item.id)).toEqual(['a', 'c']);
    expect(result[0].value).toBe(4);
    expect(result[0].nested).toBe(previous[0].nested);
    expect(result[1]).toBe(added);
  });
  it('does not retain removed properties or turn null into an object', () => {
    const previous = [{ id: 'a', error: 'old', value: { ok: true } }];
    const result = reconcileHubItems<Record<string, unknown> & { id: string }>(
      previous,
      [{ id: 'a', value: null }],
      key,
    );
    expect(result).toEqual([{ id: 'a', value: null }]);
    expect(result[0]).not.toBe(previous[0]);
  });
});
