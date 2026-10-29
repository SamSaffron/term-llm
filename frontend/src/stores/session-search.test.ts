import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AppStore } from './app-store';
import { testConfig } from './store-test-fixtures';

function deferred() {
  let resolve!: (value: Record<string, unknown>) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<Record<string, unknown>>((yes, no) => {
    resolve = yes;
    reject = no;
  });
  return { promise, resolve, reject };
}

let store: AppStore;
beforeEach(() => {
  vi.useFakeTimers();
  localStorage.clear();
  store = new AppStore(testConfig);
});
afterEach(() => {
  store.dispose();
  vi.useRealTimers();
});

describe('session search ordering', () => {
  it('defaults to relevance, forwards filters and renders server order without client sorting', async () => {
    const search = vi.spyOn(store.endpoints, 'searchSessions').mockResolvedValue({
      sessions: [
        { id: 'server-first', last_message_at: 1 },
        { id: 'server-second', last_message_at: 9999999999, pinned: true },
      ],
    });
    store.showArchived.value = true;
    const pending = store.search('needle');
    await vi.advanceTimersByTimeAsync(180);
    await pending;
    expect(search).toHaveBeenCalledWith(
      'needle',
      true,
      testConfig.sidebarCategories,
      expect.any(AbortSignal),
      'relevance',
    );
    expect(store.searchResults.value?.map((session) => session.id)).toEqual([
      'server-first',
      'server-second',
    ]);
  });

  it.each(['success', 'failure'] as const)(
    'ignores an out-of-order %s for the same query with a previous sort',
    async (outcome) => {
      const stale = deferred();
      const latest = deferred();
      const search = vi
        .spyOn(store.endpoints, 'searchSessions')
        .mockReturnValueOnce(stale.promise)
        .mockReturnValueOnce(latest.promise);
      const first = store.search('needle');
      await vi.advanceTimersByTimeAsync(180);
      const second = store.sessionStore.setSearchSort('newest');
      expect(search.mock.calls[0][3]?.aborted).toBe(true);
      await vi.advanceTimersByTimeAsync(180);
      expect(search.mock.calls[1][4]).toBe('newest');
      latest.resolve({ sessions: [{ id: 'newest' }] });
      await second;
      if (outcome === 'success') stale.resolve({ sessions: [{ id: 'stale' }] });
      else stale.reject(new Error('stale failure'));
      await first;
      expect(store.searchResults.value?.map((session) => session.id)).toEqual(['newest']);
      expect(store.searchError.value).toBe('');
      expect(store.searchLoading.value).toBe(false);
    },
  );

  it('does not let a stale completion turn off loading for the latest sort', async () => {
    const stale = deferred();
    const latest = deferred();
    vi.spyOn(store.endpoints, 'searchSessions')
      .mockReturnValueOnce(stale.promise)
      .mockReturnValueOnce(latest.promise);
    const first = store.search('needle');
    await vi.advanceTimersByTimeAsync(180);
    const second = store.sessionStore.setSearchSort('oldest');
    await vi.advanceTimersByTimeAsync(180);
    stale.resolve({ sessions: [{ id: 'stale' }] });
    await first;
    expect(store.searchLoading.value).toBe(true);
    expect(store.searchResults.value).toEqual([]);
    latest.resolve({ sessions: [{ id: 'oldest' }] });
    await second;
    expect(store.searchLoading.value).toBe(false);
  });

  it('debounces rapid sort changes, settles cancelled callers, and preserves sort across queries', async () => {
    const search = vi.spyOn(store.endpoints, 'searchSessions').mockResolvedValue({ sessions: [] });
    const pending = store.search('needle');
    const newest = store.sessionStore.setSearchSort('newest');
    const oldest = store.sessionStore.setSearchSort('oldest');
    await vi.advanceTimersByTimeAsync(180);
    await Promise.all([pending, newest, oldest]);
    expect(search).toHaveBeenCalledTimes(1);
    expect(search.mock.calls[0][4]).toBe('oldest');
    await store.search('');
    expect(store.searchResults.value).toBeNull();
    const next = store.search('next query');
    await vi.advanceTimersByTimeAsync(180);
    await next;
    expect(search.mock.calls[1][4]).toBe('oldest');
    await store.sessionStore.setSearchSort('oldest');
    expect(search).toHaveBeenCalledTimes(2);
  });

  it('ignores pending responses after clearing search or disposing the store', async () => {
    const stale = deferred();
    vi.spyOn(store.endpoints, 'searchSessions').mockReturnValue(stale.promise);
    const pending = store.search('needle');
    await vi.advanceTimersByTimeAsync(180);
    await store.search('');
    stale.resolve({ sessions: [{ id: 'stale' }] });
    await pending;
    expect(store.searchResults.value).toBeNull();
    const debounce = store.search('other');
    store.dispose();
    await debounce;
    expect(store.endpoints.searchSessions).toHaveBeenCalledTimes(1);
  });
});
