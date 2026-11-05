import { afterEach, describe, expect, it, vi } from 'vitest';
import type { ActivitySnapshot } from '../api/endpoints';
import type { AppStoreServices } from './app-store-services';
import { activityChild, activitySnapshot } from '../test/activity-fixtures';
import { testSession } from './store-test-fixtures';
import { ChildSessionStore } from './child-session-store';

const stores: ChildSessionStore[] = [];
afterEach(() => {
  stores.splice(0).forEach((store) => store.dispose());
  vi.restoreAllMocks();
});
function harness() {
  const endpoints = {
    sessionChildren: vi.fn(async () => ({ children: [activityChild()] })),
    sessionActivity: vi
      .fn<(...args: unknown[]) => Promise<ActivitySnapshot>>()
      .mockResolvedValue(activitySnapshot({ __etag: 'v1' })),
  };
  const store = new ChildSessionStore({ endpoints } as unknown as AppStoreServices);
  stores.push(store);
  return { store, endpoints };
}

describe('ChildSessionStore activity snapshot', () => {
  it('scopes each drill-down to its own subtree, hides leaf work, and restores parent scope on Back', async () => {
    const { store, endpoints } = harness();
    endpoints.sessionActivity.mockImplementation(async (id) =>
      activitySnapshot({
        session_id: String(id),
        agents:
          id === 'parent'
            ? [
                activityChild(),
                activityChild({ session_id: 'grandchild', parent_session_id: 'child' }),
              ]
            : id === 'child'
              ? [activityChild({ session_id: 'grandchild', parent_session_id: 'child' })]
              : [],
        jobs:
          id === 'parent'
            ? [
                {
                  job_id: 'j',
                  run_id: 'r',
                  title: 'Job',
                  status: 'running',
                  session_id: 'local-job',
                },
              ]
            : [],
      }),
    );
    store.selectSession(testSession({ id: 'parent' }));
    await vi.waitFor(() => expect(store.activity.value?.agents).toHaveLength(2));
    expect(store.children.value).toHaveLength(1);
    store.selectSession(testSession({ id: 'child', parentSessionId: 'parent' }));
    expect(store.activitySessionId.value).toBe('child');
    expect(store.activity.value).toBeNull();
    await vi.waitFor(() => expect(store.activity.value?.session_id).toBe('child'));
    expect(store.activity.value?.agents?.map((row) => row.session_id)).toEqual(['grandchild']);
    expect(store.activity.value?.jobs).toEqual([]);
    store.selectSession(testSession({ id: 'grandchild', parentSessionId: 'child' }));
    expect(store.parentSessionId.value).toBe('grandchild');
    expect(store.activitySessionId.value).toBe('grandchild');
    await vi.waitFor(() => expect(store.activity.value?.agents).toEqual([]));
    expect(store.activityActive.value).toBe(false);
    store.selectSession(testSession({ id: 'local-job' }));
    expect(store.activitySessionId.value).toBe('local-job');
    await vi.waitFor(() => expect(store.activity.value?.session_id).toBe('local-job'));
    expect(store.activity.value?.jobs).toEqual([]);
    store.selectSession(testSession({ id: 'parent' }));
    await vi.waitFor(() => expect(store.activity.value?.agents).toHaveLength(2));
    expect(store.activitySessionId.value).toBe('parent');
    expect(endpoints.sessionActivity).toHaveBeenCalledTimes(5);
    store.selectSession(null);
    expect(store.activity.value).toBeNull();
    expect(store.activitySessionId.value).toBe('');
  });

  it('revalidates with ETag, keeps 304 rows, invalidates on descendant events, and survives optional failure', async () => {
    const { store, endpoints } = harness();
    store.selectSession(testSession({ id: 'parent' }));
    await vi.waitFor(() => expect(store.activity.value?.__etag).toBe('v1'));
    endpoints.sessionActivity.mockResolvedValueOnce({ __notModified: true } as ActivitySnapshot);
    await store.refreshActivity();
    expect(endpoints.sessionActivity).toHaveBeenLastCalledWith(
      'parent',
      expect.any(AbortSignal),
      'v1',
    );
    expect(store.activity.value?.agents).toHaveLength(1);
    endpoints.sessionActivity.mockRejectedValueOnce(new Error('optional unavailable'));
    await store.refreshActivity();
    expect(store.activityStale.value).toBe(true);
    expect(store.activity.value?.agents).toHaveLength(1);
    endpoints.sessionActivity.mockResolvedValueOnce(
      activitySnapshot({ jobs_availability: 'restricted' }),
    );
    store.childrenChanged('child');
    await vi.waitFor(() => expect(store.activity.value?.jobs_availability).toBe('restricted'));
    expect(endpoints.sessionActivity).toHaveBeenLastCalledWith(
      'parent',
      expect.any(AbortSignal),
      '',
    );
    expect(store.activityActive.value).toBe(true);
  });

  it('cancels hidden reads, coalesces concurrent reads, rejects late old-scope replies and clears validators', async () => {
    const { store, endpoints } = harness();
    let resolve!: (snapshot: ActivitySnapshot) => void;
    endpoints.sessionActivity.mockImplementationOnce(
      () =>
        new Promise((done) => {
          resolve = done;
        }),
    );
    store.selectSession(testSession({ id: 'parent' }));
    const signal = endpoints.sessionActivity.mock.calls[0][1] as AbortSignal;
    await store.refreshActivity();
    expect(endpoints.sessionActivity).toHaveBeenCalledTimes(1);
    store.selectSession(testSession({ id: 'other' }));
    expect(signal.aborted).toBe(true);
    resolve(activitySnapshot());
    await Promise.resolve();
    expect(store.activity.value).toBeNull();
    store.selectSession(null);
    endpoints.sessionActivity.mockResolvedValue(activitySnapshot());
    vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
    store.selectSession(testSession({ id: 'parent' }));
    await store.refreshActivity();
    expect(endpoints.sessionActivity).toHaveBeenCalledTimes(2);
    vi.restoreAllMocks();
    await store.refreshActivity();
    expect(endpoints.sessionActivity).toHaveBeenLastCalledWith(
      'parent',
      expect.any(AbortSignal),
      '',
    );
    expect(store.activity.value?.session_id).toBe('parent');
  });
});

describe('activity event/read races', () => {
  it('queues one authoritative reread when an event arrives during an existing read', async () => {
    const { store, endpoints } = harness();
    let resolve!: (snapshot: ActivitySnapshot) => void;
    endpoints.sessionActivity.mockImplementationOnce(
      () =>
        new Promise((done) => {
          resolve = done;
        }),
    );
    store.selectSession(testSession({ id: 'parent' }));
    store.childrenChanged('parent');
    store.childrenChanged('parent');
    expect(endpoints.sessionActivity).toHaveBeenCalledTimes(1);
    endpoints.sessionActivity.mockResolvedValueOnce(
      activitySnapshot({ agents: [activityChild({ live: false, state: 'complete' })] }),
    );
    resolve(activitySnapshot());
    await vi.waitFor(() => expect(store.activityActive.value).toBe(false));
    expect(endpoints.sessionActivity).toHaveBeenCalledTimes(2);
    expect(endpoints.sessionActivity).toHaveBeenLastCalledWith(
      'parent',
      expect.any(AbortSignal),
      '',
    );
  });

  it('aborts pending activity on hide/dispose without applying late replies', async () => {
    const { store, endpoints } = harness();
    let resolve!: (snapshot: ActivitySnapshot) => void;
    endpoints.sessionActivity.mockImplementationOnce(
      () =>
        new Promise((done) => {
          resolve = done;
        }),
    );
    store.selectSession(testSession({ id: 'parent' }));
    const signal = endpoints.sessionActivity.mock.calls[0][1] as AbortSignal;
    vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
    store.pauseActivity();
    expect(signal.aborted).toBe(true);
    resolve(activitySnapshot());
    await Promise.resolve();
    expect(store.activity.value).toBeNull();
    await store.refreshActivity();
    expect(endpoints.sessionActivity).toHaveBeenCalledTimes(1);
    vi.restoreAllMocks();
    await store.refreshActivity();
    expect(store.activity.value?.session_id).toBe('parent');
    store.dispose();
    await store.refreshActivity();
    expect(endpoints.sessionActivity).toHaveBeenCalledTimes(2);
  });
});

describe('optional source handoff', () => {
  it('retains known jobs during jobs-only unavailability but clears them on authoritative empty or restricted responses', async () => {
    const { store, endpoints } = harness();
    endpoints.sessionActivity.mockResolvedValueOnce(
      activitySnapshot({
        agents: [],
        jobs: [{ job_id: 'j', run_id: 'r', title: 'Known run', status: 'running' }],
      }),
    );
    store.selectSession(testSession({ id: 'parent' }));
    await vi.waitFor(() => expect(store.activityActive.value).toBe(true));
    endpoints.sessionActivity.mockResolvedValueOnce(
      activitySnapshot({ agents: [], jobs: [], jobs_availability: 'unavailable' }),
    );
    await store.refreshActivity();
    expect(store.activityActive.value).toBe(true);
    expect(store.activity.value?.jobs).toHaveLength(1);
    expect(store.activity.value?.jobs_availability).toBe('unavailable');
    endpoints.sessionActivity.mockResolvedValueOnce(
      activitySnapshot({ agents: [], jobs: [], jobs_availability: 'restricted' }),
    );
    await store.refreshActivity();
    expect(store.activity.value?.jobs).toEqual([]);
    endpoints.sessionActivity.mockResolvedValueOnce(activitySnapshot({ agents: [], jobs: [] }));
    await store.refreshActivity();
    expect(store.activityActive.value).toBe(false);
  });
});

describe('partially unavailable source merging', () => {
  it('accepts new/fresh executions alongside cached missing and unknown rows, including terminal updates', async () => {
    const { store, endpoints } = harness();
    endpoints.sessionActivity.mockResolvedValueOnce(
      activitySnapshot({
        agents: [activityChild(), activityChild({ session_id: 'missing-agent' })],
        jobs: [
          { job_id: 'j', run_id: 'cached', title: 'Cached run', status: 'running' },
          { job_id: 'j', run_id: 'fresh', title: 'Will finish', status: 'running' },
        ],
      }),
    );
    store.selectSession(testSession({ id: 'parent' }));
    await vi.waitFor(() => expect(store.activity.value?.jobs).toHaveLength(2));
    endpoints.sessionActivity.mockResolvedValueOnce(
      activitySnapshot({
        agents_availability: 'unavailable',
        jobs_availability: 'unavailable',
        agents: [
          activityChild({ state: 'error', live: false }),
          activityChild({ session_id: 'new-agent' }),
        ],
        jobs: [
          { job_id: 'j', run_id: 'cached', title: 'Lookup failed', status: 'unknown' },
          { job_id: 'j', run_id: 'fresh', title: 'Finished', status: 'complete' },
          { job_id: 'j', run_id: 'new', title: 'New active run', status: 'running' },
        ],
      }),
    );
    await store.refreshActivity();
    expect(store.activity.value?.jobs?.map((row) => [row.run_id, row.status])).toEqual([
      ['cached', 'running'],
      ['fresh', 'complete'],
      ['new', 'running'],
    ]);
    expect(
      store.activity.value?.agents?.map((row) => [row.session_id, row.state, row.live]),
    ).toEqual([
      ['child', 'error', false],
      ['missing-agent', 'active', true],
      ['new-agent', 'active', true],
    ]);
    endpoints.sessionActivity.mockResolvedValueOnce(activitySnapshot({ agents: [], jobs: [] }));
    await store.refreshActivity();
    expect(store.activity.value?.jobs).toEqual([]);
    expect(store.activity.value?.agents).toEqual([]);
  });
});
