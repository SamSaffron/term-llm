import { describe, expect, it, vi } from 'vitest';
import type { AppStoreServices } from './app-store-services';
import { SessionActivityStore, type SessionActivity } from './session-activity-store';

const snapshot = (status = 'running', revision = 1): SessionActivity => ({
  id: 'compaction-1',
  revision,
  kind: 'compaction',
  status,
  started_at: 100,
});
const create = () => {
  const toast = vi.fn();
  return { store: new SessionActivityStore({ toast } as unknown as AppStoreServices), toast };
};

describe('server-authored session activities', () => {
  it('does not advance observation generations for unchanged idle sessions', () => {
    const { store } = create();
    const version = store.capture();
    store.observe('s1', undefined);
    expect(store.capture()).toBe(version);
    expect(store.sessionIds.value).toEqual([]);
  });

  it('keeps unconfirmed admission blocked until a post-request authoritative observation', () => {
    const { store } = create();
    const stale = store.capture();
    const token = store.begin('s1');
    store.observe('s1', undefined, stale);
    expect(store.busy('s1')).toBe(true);
    store.checking('s1', token);
    store.observe('s1', snapshot());
    expect(store.busy('s1')).toBe(true);
    expect(store.anyActive.value).toBe(true);
    store.observe('s1', snapshot('completed', 2));
    expect(store.busy('s1')).toBe(false);
    expect(store.anyActive.value).toBe(false);
  });

  it('does not let a pre-admission idle poll clear a checking bridge', () => {
    const { store } = create();
    const token = store.begin('s1');
    const duringRequest = store.capture();
    store.observe('s1', undefined, duringRequest);
    expect(store.busy('s1')).toBe(true);
    store.checking('s1', token);
    store.observe('s1', undefined, duringRequest);
    expect(store.busy('s1')).toBe(true);
    store.observe('s1', undefined, store.capture());
    expect(store.busy('s1')).toBe(false);
  });

  it('rejects old poll snapshots and out-of-order activity revisions', () => {
    const { store } = create();
    store.observe('s1', snapshot());
    const poll = store.capture();
    store.observe('s1', snapshot('completed', 2));
    store.observe('s1', snapshot(), poll);
    store.observe('s1', snapshot());
    expect(store.busy('s1')).toBe(false);
    const next = { ...snapshot(), id: 'compaction-2', started_at: 200 };
    store.observe('s1', next);
    store.observe('s1', snapshot('completed', 2));
    expect(store.snapshots.value.s1).toEqual(next);
    expect(store.busy('s1')).toBe(true);
  });

  it('uses a newer server revision even if a stream observation arrived during the request', () => {
    const { store } = create();
    const poll = store.capture();
    store.observe('s1', snapshot());
    expect(store.observe('s1', snapshot('completed', 2), poll)).toBe(true);
    expect(store.busy('s1')).toBe(false);
  });

  it('orders attempts that began in the same millisecond by revision', () => {
    const { store } = create();
    store.observe('s1', { ...snapshot(), id: 'newer', revision: 3 });
    expect(store.observe('s1', snapshot('completed', 2))).toBe(false);
    expect(store.busy('s1')).toBe(true);
  });

  it('does not clear a newer request when an older rejection arrives', () => {
    const { store } = create();
    const old = store.begin('s1');
    const next = store.begin('s1');
    store.reject('s1', old);
    expect(store.busy('s1')).toBe(true);
    store.reject('s1', next);
    expect(store.busy('s1')).toBe(false);
  });

  it('reports an observed failure once but does not replay historical failures in a new document', () => {
    const { store, toast } = create();
    const failed = { ...snapshot('failed', 2), error: 'Could not summarize history' };
    store.observe('s1', snapshot());
    store.observe('s1', failed);
    store.observe('s1', failed);
    expect(store.busy('s1')).toBe(false);
    expect(toast).toHaveBeenCalledExactlyOnceWith(failed.error, 'error');
    const other = create();
    other.store.observe('s1', failed);
    expect(other.toast).not.toHaveBeenCalled();
    expect(other.store.snapshots.value.s1).toEqual(failed);
  });

  it('reports an observed completion note once', () => {
    const { store, toast } = create();
    const done = {
      ...snapshot('completed', 2),
      message: 'Context compacted natively (900 → 300 tokens).',
    };
    store.observe('s1', snapshot());
    store.observe('s1', done);
    store.observe('s1', done);
    expect(toast).toHaveBeenCalledExactlyOnceWith(done.message, 'success');
  });

  it('does not turn cancellation into an error notification', () => {
    const { store, toast } = create();
    store.observe('s1', snapshot());
    store.observe('s1', snapshot('cancelled', 2));
    expect(store.busy('s1')).toBe(false);
    expect(toast).not.toHaveBeenCalled();
  });
});
