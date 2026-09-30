import { act, fireEvent, render, screen } from '@testing-library/preact';
import { expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig, testSession } from '../stores/store-test-fixtures';
import { Modals } from './Modals';

vi.mock('./ExtensionSettings', () => {
  throw new Error('Chunk unavailable');
});
vi.mock('./CommitModal', () => {
  throw new Error('Chunk unavailable');
});
vi.mock('./WidgetsModal', () => {
  throw new Error('Chunk unavailable');
});
vi.mock('./RenameModal', () => {
  throw new Error('Chunk unavailable');
});

it('keeps a failed commit chunk dismissible through the commit store', async () => {
  const store = new AppStore(testConfig);
  store.modal.value = 'commit';
  const close = vi.spyOn(store.commitStore, 'close');
  try {
    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );
    expect(
      await screen.findByText('Could not load commit controls. Reload the page to retry.'),
    ).toHaveAttribute('role', 'alert');
    fireEvent.click(screen.getByRole('button', { name: 'Close Git commit' }));
    expect(close).toHaveBeenCalledOnce();
    expect(store.modal.value).toBe('');
  } finally {
    store.dispose();
  }
});

it('preserves the extension settings loading and import-error messages', async () => {
  const store = new AppStore(testConfig);
  store.modal.value = 'settings';
  try {
    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );
    fireEvent.click(await screen.findByRole('tab', { name: 'Extensions' }));
    expect(screen.getByText('Loading extension settings…')).toBeInTheDocument();
    expect(
      await screen.findByText('Could not load extension settings. Reload the page to retry.'),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByRole('tab', { name: 'Model' }));
    fireEvent.click(screen.getByRole('tab', { name: 'Extensions' }));
    expect(
      screen.getByText('Could not load extension settings. Reload the page to retry.'),
    ).toBeVisible();
  } finally {
    store.dispose();
  }
});

it('keeps a failed on-demand modal chunk dismissible with a reload hint', async () => {
  const store = new AppStore(testConfig);
  store.modal.value = 'widgets';
  const logged = vi.spyOn(console, 'error').mockImplementation(() => undefined);
  try {
    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );
    const dialog = await screen.findByRole('dialog', { name: 'Widgets' });
    expect(dialog).toHaveTextContent('Could not load this dialog. Reload the page to retry.');
    expect(logged).toHaveBeenCalledWith('Failed to load the Widgets dialog', expect.any(Error));
    fireEvent.click(screen.getByRole('button', { name: 'Close Widgets' }));
    expect(store.modal.value).toBe('');
  } finally {
    logged.mockRestore();
    store.dispose();
  }
});

it('clears the rename target when a failed rename chunk is dismissed', async () => {
  const store = new AppStore(testConfig);
  const logged = vi.spyOn(console, 'error').mockImplementation(() => undefined);
  try {
    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );
    act(() => store.sessionStore.openRename(testSession()));
    fireEvent.keyDown(await screen.findByRole('dialog', { name: 'Rename session' }), {
      key: 'Escape',
    });
    expect(store.modal.value).toBe('');
    expect(store.renameTarget.value).toBeNull();

    act(() => store.sessionStore.openRename(testSession()));
    fireEvent.click(screen.getByRole('button', { name: 'Close Rename session' }));
    expect(store.modal.value).toBe('');
    expect(store.renameTarget.value).toBeNull();
  } finally {
    logged.mockRestore();
    store.dispose();
  }
});

// This file deliberately never preloads deferred modals, so a synchronous
// query here fails if either prompt is ever moved behind a dynamic import.
it('renders agent approval and ask-user prompts without waiting for a chunk', () => {
  const store = new AppStore(testConfig);
  store.activeSessionId.value = 's1';
  store.askUser.value = {
    sessionId: 's1',
    callId: 'ask-1',
    questions: [{ question: 'Question?', options: [] }],
  };
  store.approval.value = {
    sessionId: 's1',
    id: 'approval-1',
    title: 'Approval first',
    scope: 'shared_shell',
    options: [{ index: 0, choice: 'once', label: 'Allow once' }],
  };
  try {
    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );
    expect(screen.getByRole('heading', { name: 'Approval first' })).toBeInTheDocument();
    act(() => {
      store.approval.value = null;
    });
    expect(screen.getByRole('heading', { name: 'Answer question' })).toBeInTheDocument();
  } finally {
    store.dispose();
  }
});
