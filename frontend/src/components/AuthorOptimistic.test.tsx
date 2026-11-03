import { render, screen, waitFor } from '@testing-library/preact';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig } from '../stores/store-test-fixtures';
import { initialProjection } from '../domain/response';
import { AuthorByline } from './AuthorByline';

const viewer = { id: 'alice', name: 'Alice Example' };
let store: AppStore;
beforeEach(() => {
  localStorage.clear();
  store = new AppStore(testConfig);
  store.viewer.value = viewer;
  store.sessionStore.authors.filter.value = 'me';
});
afterEach(() => store.dispose());

it('shows You on the real optimistic send and keeps its new session in Started by me', async () => {
  let accept!: (response: Response) => void;
  store.endpoints.createResponse = vi.fn(
    () =>
      new Promise<Response>((resolve) => {
        accept = resolve;
      }),
  );
  store.prompt.value = 'A new attributed chat';
  const sending = store.send();
  try {
    await waitFor(() => expect(store.endpoints.createResponse).toHaveBeenCalledOnce());
    const user = store.visibleMessages.value.find((message) => message.role === 'user');
    expect(user?.author).toEqual(viewer);
    expect(store.activeSession.value?.createdBy).toEqual(viewer);
    expect(store.sidebarSessions.value.map((session) => session.id)).toContain(
      store.activeSessionId.value,
    );
    render(
      <StoreContext.Provider value={store}>
        <AuthorByline author={user?.author} />
      </StoreContext.Provider>,
    );
    expect(screen.getByText('You')).toBeInTheDocument();
  } finally {
    accept(new Response('synthetic rejection', { status: 400 }));
    await sending;
  }
});

it('keeps a locally materialized blank session visible through a partial server rekey', async () => {
  store.endpoints.createBlankSession = vi.fn(async () => ({ session: { id: 'blank', number: 1 } }));
  await expect(store.ensureSession()).resolves.toBe('blank');
  expect(store.activeSession.value?.createdBy).toEqual(viewer);
  expect(store.sidebarSessions.value.map((session) => session.id)).toContain('blank');
});

it('preserves author metadata on new and existing queued steering receipts', () => {
  store.sessionStore.prepend(store.sessionStore.sessionFrom({ id: 's', created_by: viewer }));
  store.runs.value = {
    s: initialProjection({
      sessionId: 's',
      responseId: 'r',
      epoch: 1,
      status: 'streaming',
      lastSequence: 0,
      startedRev: 0,
      reconnects: 0,
    }),
  };
  const apply = (sequence: number, author: typeof viewer) =>
    store.applyResponseEvent('s', {
      type: 'response.steering.queued',
      response_id: 'r',
      run_epoch: 1,
      sequence_number: sequence,
      client_message_id: 'guidance',
      text: 'please check',
      author,
    });
  apply(1, viewer);
  expect(store.steering.value[0]).toMatchObject({
    id: 'guidance',
    author: viewer,
    state: 'pending',
  });
  // A queued server receipt must replace the local sending row's metadata too.
  store.steering.value = store.steering.value.map((entry) => ({ ...entry, state: 'sending' }));
  const renamed = { ...viewer, name: 'Alice Renamed' };
  apply(2, renamed);
  expect(store.steering.value).toHaveLength(1);
  expect(store.steering.value[0].author).toEqual(renamed);
});
