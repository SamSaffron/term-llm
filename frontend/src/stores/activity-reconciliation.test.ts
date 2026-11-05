import { afterEach, expect, it, vi } from 'vitest';
import type { ActivitySnapshot } from '../api/endpoints';
import { AppStore } from './app-store';
import { testConfig } from './store-test-fixtures';

let store: AppStore;
afterEach(() => {
  store?.dispose();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

it('reconciles an interaction event without waiting for the optional activity request', async () => {
  vi.useFakeTimers();
  store = new AppStore(testConfig);
  store.activeSessionId.value = 'selected';
  store.childSessionStore.activitySessionId.value = 'selected';
  let finishActivity!: (snapshot: ActivitySnapshot) => void;
  const activity = vi.spyOn(store.endpoints, 'sessionActivity').mockImplementation(
    () =>
      new Promise((resolve) => {
        finishActivity = resolve;
      }),
  );
  const status = vi.spyOn(store.statusReconciler, 'refresh').mockResolvedValue(undefined);
  const active = vi
    .spyOn(
      store as unknown as {
        reconcileServerEventActive(revision?: number): Promise<void>;
      },
      'reconcileServerEventActive',
    )
    .mockResolvedValue(undefined);
  const events = store.serverEventCoordinator as unknown as {
    route(event: { type: 'interaction.changed'; sessionId: string }): void;
  };
  try {
    events.route({ type: 'interaction.changed', sessionId: 'selected' });
    await vi.advanceTimersByTimeAsync(100);
    expect(status).toHaveBeenCalledWith(true);
    expect(activity).toHaveBeenCalledOnce();
    expect(active).toHaveBeenCalledOnce();
  } finally {
    finishActivity?.({
      session_id: 'selected',
      agents: [],
      jobs: [],
      agents_availability: 'available',
      jobs_availability: 'available',
      agents_partial: false,
      jobs_partial: false,
    });
    await Promise.resolve();
  }
});
