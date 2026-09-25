import { act, render, screen, waitFor } from '@testing-library/preact';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig, testSession } from '../stores/store-test-fixtures';
import { Sidebar } from './Sidebar';
import { Transcript } from './Transcript';

const stores: AppStore[] = [];
afterEach(() => {
  stores.splice(0).forEach((store) => store.dispose());
  vi.useRealTimers();
  vi.restoreAllMocks();
});

const LATENCY = 500;

/** Two sessions with durable transcripts, served over a slow link. */
function setup() {
  const store = new AppStore(testConfig);
  stores.push(store);
  store.sessions.value = [
    testSession({
      id: 's1',
      title: 'First',
      messageCount: 1,
      messages: [{ id: 'm1', role: 'user', content: 'First question', created: 1 }],
    }),
    testSession({ id: 's2', title: 'Second', messageCount: 1, messages: [] }),
  ];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  const slow = <T,>(value: T) =>
    new Promise<T>((resolve) => setTimeout(() => resolve(value), LATENCY));
  store.endpoints.models = vi.fn(() => slow({ models: [] }));
  store.endpoints.skills = vi.fn(() => slow({ skills: [] }));
  store.endpoints.tree = vi.fn(() => slow({ path_count: 1 }));
  store.endpoints.sessionState = vi.fn(() => slow({}));
  store.endpoints.selectedSession = vi.fn((id: string) =>
    slow({
      selected_session: { id, message_count: 1 },
      selected_transcript: {
        bodies: {
          rev: 1,
          messages: [
            {
              id: 1,
              sequence: 0,
              role: 'user',
              parts: [{ type: 'text', text: 'Second question' }],
            },
          ],
        },
      },
    }),
  );
  return store;
}

function mount(store: AppStore) {
  return render(
    <StoreContext.Provider value={store}>
      <Sidebar />
      <Transcript />
    </StoreContext.Provider>,
  );
}

describe('sidebar session switching', () => {
  it('does not flash the new-chat screen while the next transcript hydrates', async () => {
    vi.useFakeTimers();
    const store = setup();
    mount(store);
    expect(screen.getByText('First question')).toBeInTheDocument();

    let selection!: Promise<void>;
    await act(async () => {
      selection = store.selectSession(store.sessions.peek()[1]);
      await Promise.resolve();
    });

    // Mid-flight: the transcript for s2 has not arrived yet.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(LATENCY - 100);
    });
    expect(screen.queryByText('Start a conversation with your agent.')).toBeNull();
    expect(document.querySelector('.empty-chat')).toBeNull();
    expect(screen.getByLabelText('Loading conversation')).toBeInTheDocument();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(LATENCY + 1_000);
      await selection;
    });
    expect(screen.getByText('Second question')).toBeInTheDocument();
    expect(screen.queryByLabelText('Loading conversation')).toBeNull();
  });

  it('keeps the new-chat screen for drafts and for genuinely empty sessions', async () => {
    const store = setup();
    store.endpoints.sessionState = vi.fn(async () => ({}));
    store.endpoints.selectedSession = vi.fn(async (id: string) => ({
      selected_session: { id, message_count: 0 },
      selected_transcript: { bodies: { rev: 1, messages: [] } },
    }));
    store.endpoints.skills = vi.fn(async () => ({ skills: [] }));
    store.endpoints.tree = vi.fn(async () => ({ path_count: 1 }));
    mount(store);

    act(() => store.newChat());
    expect(screen.getByText('Start a conversation with your agent.')).toBeInTheDocument();

    await act(async () => {
      await store.selectSession(store.sessions.peek()[1]);
    });
    await waitFor(() =>
      expect(screen.getByText('Start a conversation with your agent.')).toBeInTheDocument(),
    );
    expect(screen.queryByLabelText('Loading conversation')).toBeNull();
  });
});
