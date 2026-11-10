import { fireEvent, render, screen, waitFor } from '@testing-library/preact';
import { afterEach, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig } from '../stores/store-test-fixtures';
import type { ScriptCellsResponse } from '../api/endpoints';
import { activitySnapshot } from '../test/activity-fixtures';
import { activeExecutionCount } from '../domain/session-activity-summary';
import { ScriptCells } from './ScriptCells';

let store: AppStore;
afterEach(() => {
  store?.dispose();
  vi.restoreAllMocks();
});
it('shows per-session capacity, approval and terminal states with independent cancel and idle Stop', async () => {
  store = new AppStore(testConfig);
  const snapshot: ScriptCellsResponse = {
    cells: [
      {
        cell_id: 'cm_first',
        status: 'awaiting_approval',
        live: true,
        started_at: 1,
        elapsed_ms: 2000,
        has_more_output: false,
        unsettled_calls: 0,
      },
      {
        cell_id: 'cm_second',
        status: 'running',
        live: true,
        started_at: 1,
        elapsed_ms: 2000,
        has_more_output: true,
        unsettled_calls: 1,
      },
      {
        cell_id: 'cm_done',
        status: 'completed',
        live: false,
        started_at: 1,
        elapsed_ms: 1000,
        has_more_output: false,
        unsettled_calls: 0,
      },
    ],
    pending_approvals: [
      {
        approval_id: 'appr_one',
        cell_id: 'cm_first',
        title: 'Read Access Request',
        path: '/private',
        options: [{ index: 0, label: 'Allow once', description: 'One read' }],
      },
    ],
  };
  store.endpoints.scriptCells = vi.fn(async () => snapshot);
  store.endpoints.cancelScripts = vi.fn(async () => ({}));
  store.endpoints.approval = vi.fn(async () => ({}));
  store.childSessionStore.refreshActivity = vi.fn(async () => {});
  render(
    <StoreContext.Provider value={store}>
      <ScriptCells sessionId="session" />
    </StoreContext.Provider>,
  );
  expect(await screen.findByText('Scripts (2/8)')).toBeVisible();
  expect(screen.getByText(/Needs approval/)).toBeVisible();
  expect(screen.getByText(/Output available/)).toBeVisible();
  expect(screen.getByText(/Completed/)).toBeVisible();
  expect(screen.queryByRole('button', { name: 'Cancel script cm_done' })).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: 'Cancel script cm_second' }));
  await waitFor(() =>
    expect(store.endpoints.cancelScripts).toHaveBeenCalledWith('session', 'cm_second'),
  );
  await waitFor(() =>
    expect(screen.getByRole('button', { name: 'Stop all scripts' })).not.toBeDisabled(),
  );
  fireEvent.click(screen.getByRole('button', { name: 'Stop all scripts' }));
  await waitFor(() => expect(store.endpoints.cancelScripts).toHaveBeenCalledWith('session'));
  await waitFor(() =>
    expect(screen.getByRole('button', { name: 'Allow once' })).not.toBeDisabled(),
  );
  fireEvent.click(screen.getByRole('button', { name: 'Allow once' }));
  await waitFor(() =>
    expect(store.endpoints.approval).toHaveBeenCalledWith(
      'session',
      { approval_id: 'appr_one', choice: 0 },
      'appr_one_0',
    ),
  );
  expect(activeExecutionCount(activitySnapshot({ agents: [], cells: snapshot.cells || [] }))).toBe(
    2,
  );
});
