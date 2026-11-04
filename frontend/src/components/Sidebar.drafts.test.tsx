import { act, fireEvent, render, screen } from '@testing-library/preact';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig, testSession } from '../stores/store-test-fixtures';
import { Sidebar } from './Sidebar';

let store: AppStore;
beforeEach(() => localStorage.clear());
afterEach(() => store?.dispose());

async function setupWithDraft(content: string) {
  store = new AppStore(testConfig);
  store.sessions.value = [
    testSession({ id: 's1', title: 'First chat', lastMessageAt: Date.now() }),
  ];
  store.endpoints.selectedSession = vi.fn(async (id: string) => ({
    selected_session: { id },
    selected_transcript: { bodies: { messages: [] } },
  }));
  store.endpoints.skills = vi.fn(async () => ({ skills: [] }));
  store.endpoints.tree = vi.fn(async () => ({}));
  store.endpoints.sessionState = vi.fn(async () => ({}));
  store.endpoints.models = vi.fn(async () => ({ object: 'list', data: [] }));
  store.newChat();
  store.prompt.value = content;
  await store.selectSession(store.sessions.peek()[0]!);
  return render(
    <StoreContext.Provider value={store}>
      <Sidebar />
    </StoreContext.Provider>,
  );
}

describe('sidebar new-chat drafts', () => {
  it('shows an unsent draft and reopens it', async () => {
    await setupWithDraft('Plan the migration\nwith more detail below');

    const row = screen.getByRole('button', { name: 'Unsent draft: Plan the migration' });
    expect(row).toHaveTextContent('Unsent · just now');
    expect(document.querySelector('#newChatBtn')).toHaveAttribute(
      'aria-label',
      'New chat, unsent draft',
    );

    await act(async () => {
      fireEvent.click(row);
    });

    expect(store.draftActive.value).toBe(true);
    expect(store.prompt.value).toBe('Plan the migration\nwith more detail below');
    expect(screen.queryByRole('button', { name: /^Unsent draft/ })).not.toBeInTheDocument();
    expect(document.querySelector('#newChatBtn')).not.toHaveAttribute('aria-label');
  });

  it('discards a draft from its row', async () => {
    await setupWithDraft('Throwaway idea');

    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Discard draft' }));
    });

    expect(screen.queryByRole('button', { name: /^Unsent draft/ })).not.toBeInTheDocument();
    expect(store.toasts.value.at(-1)).toMatchObject({ action: { label: 'Undo' } });
  });
});
