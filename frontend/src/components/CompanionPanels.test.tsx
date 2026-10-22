import { act, fireEvent, render, screen, waitFor } from '@testing-library/preact';
import { expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig } from '../stores/store-test-fixtures';
import { CompanionPanels } from './CompanionPanels';

const pending = vi.hoisted(() => ({ reject: null as ((error: Error) => void) | null }));
vi.mock(
  './Panels',
  () =>
    new Promise((_, reject) => {
      pending.reject = reject;
    }),
);

it('downloads viewers only on first use and keeps loading/error panels dismissible', async () => {
  const store = new AppStore(testConfig);
  try {
    render(
      <StoreContext.Provider value={store}>
        <CompanionPanels />
      </StoreContext.Provider>,
    );
    expect(pending.reject).toBeNull();
    expect(screen.queryByText('Loading panel…')).toBeNull();
    act(() => {
      store.diff.value = { ...store.diff.peek(), open: true };
    });
    expect(screen.getByRole('status')).toHaveTextContent('Loading panel…');
    await waitFor(() => expect(pending.reject).not.toBeNull());
    fireEvent.click(screen.getByRole('button', { name: 'Close Session file changes' }));
    expect(store.diff.value.open).toBe(false);
    expect(screen.queryByRole('status')).toBeNull();
    await act(async () => pending.reject!(new Error('Panel chunk unavailable')));
    expect(screen.queryByRole('alert')).toBeNull();
    act(() => {
      store.diff.value = { ...store.diff.peek(), open: true };
    });
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not load this panel.');
    fireEvent.click(screen.getByRole('button', { name: 'Close Session file changes' }));
    expect(store.diff.value.open).toBe(false);
    act(() => {
      store.currentPlan.value = { plan: [{ step: 'Try panel', status: 'pending' }] };
      store.planOpen.value = true;
    });
    expect(screen.getByRole('complementary', { name: 'Current plan' })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Close Current plan' }));
    expect(store.planVisible.value).toBe(false);
  } finally {
    store.dispose();
  }
});
