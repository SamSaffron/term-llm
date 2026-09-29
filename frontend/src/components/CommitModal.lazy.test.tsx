import { act, render, screen } from '@testing-library/preact';
import { expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig, testSession } from '../stores/store-test-fixtures';
import { Modals } from './Modals';

const chunk = vi.hoisted(() => {
  let resolve!: () => void;
  const ready = new Promise<void>((done) => {
    resolve = done;
  });
  return { ready, resolve, requested: vi.fn() };
});
vi.mock('./CommitModal', async (importOriginal) => {
  chunk.requested();
  await chunk.ready;
  return importOriginal();
});

it('loads commit controls only when opened and reuses them on subsequent opens', async () => {
  const store = new AppStore(testConfig);
  store.sessions.value = [testSession()];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  try {
    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );
    await act(async () => {});
    expect(chunk.requested).not.toHaveBeenCalled();

    await act(async () => {
      store.modal.value = 'commit';
    });
    await vi.waitFor(() => expect(chunk.requested).toHaveBeenCalledOnce());
    expect(screen.queryByRole('dialog', { name: 'Git commit' })).toBeNull();
    await act(async () => {
      chunk.resolve();
    });
    expect(await screen.findByRole('dialog', { name: 'Git commit' })).toBeInTheDocument();

    act(() => store.commitStore.close());
    expect(screen.queryByRole('dialog', { name: 'Git commit' })).toBeNull();
    act(() => {
      store.modal.value = 'commit';
    });
    expect(screen.getByRole('dialog', { name: 'Git commit' })).toBeInTheDocument();
    expect(chunk.requested).toHaveBeenCalledOnce();
  } finally {
    chunk.resolve();
    store.dispose();
  }
});
