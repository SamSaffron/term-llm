import { act, render, screen, waitFor } from '@testing-library/preact';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig, testSession } from '../stores/store-test-fixtures';
import { Sidebar } from './Sidebar';

let store: AppStore;
afterEach(() => store?.dispose());

function show(pins: number, cursor: string, projects = false) {
  store = new AppStore(testConfig);
  store.projectsEnabled.value = projects;
  store.setSidebarView('projects');
  store.sessions.value = Array.from({ length: pins }, (_, index) =>
    testSession({ id: `pin-${index}`, title: `Pinned chat ${index}`, pinned: true }),
  );
  store.noProjectCursor.value = cursor;
  return render(
    <StoreContext.Provider value={store}>
      <Sidebar />
    </StoreContext.Provider>,
  );
}

describe.each([false, true])('sidebar pagination (projects enabled: %s)', (projects) => {
  it.each([0, 7, 30])('loads older chats with %i pins and no regular rows', async (pins) => {
    show(pins, 'older-page', projects);
    const yesterday = testSession({ id: 'yesterday', title: 'Yesterday chat' });
    const load = vi.spyOn(store, 'loadMoreNoProject').mockImplementation(async () => {
      await act(() => {
        store.sessions.value = [...store.sessions.peek(), yesterday];
        store.noProjectCursor.value = '';
      });
    });
    expect(
      screen.getByRole('status', { name: 'More conversations load automatically' }),
    ).toBeInTheDocument();
    await waitFor(() => expect(load).toHaveBeenCalledOnce());
    expect(screen.getByRole('button', { name: 'Yesterday chat' })).toBeInTheDocument();
    expect(
      screen.queryByRole('status', { name: 'More conversations load automatically' }),
    ).not.toBeInTheDocument();
  });

  it('does not paginate a complete pinned-only catalog', () => {
    const { container } = show(7, '', projects);
    expect(screen.getByRole('button', { name: 'Pinned chat 0' })).toBeInTheDocument();
    expect(container.querySelector('.project-pagination-sentinel')).toBeNull();
  });
});
