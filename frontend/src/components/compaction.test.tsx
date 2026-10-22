import { act, fireEvent, render, screen } from '@testing-library/preact';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import type { AppConfig } from '../app/config';
import { APIError } from '../api/client';
import type { ServeSessionActivity } from '../api/generated/types';
import { initialProjection } from '../domain/response';
import type { Session } from '../domain/types';
import { AppStore } from '../stores/app-store';
import { Composer } from './Composer';

// Compaction lifecycle as the user sees it: one banner, one disabled send
// button, and no path to send or steer while the server reports the session
// busy. Manual kickoff and automatic engine compaction share every guard.

const config: AppConfig = {
  prefix: '/ui',
  version: 'v1',
  sidebarCategories: ['all'],
  agentName: '',
  agentNames: ['jarvis'],
  title: '',
  locationSharing: true,
  worktrees: true,
  hub: null,
  vapidKey: '',
};

const BANNER = 'Compacting conversation context…';
const BUTTON = 'Compacting conversation context';

type Deferred<T> = {
  promise: Promise<T>;
  resolve: (value: T) => void;
  reject: (e: unknown) => void;
};
const deferred = <T,>(): Deferred<T> => {
  let resolve!: (value: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
};

const activity = (
  status: ServeSessionActivity['status'],
  overrides: Partial<ServeSessionActivity> = {},
): ServeSessionActivity => ({
  id: 'compaction_1',
  revision: status === 'running' ? 1 : 2,
  kind: 'compaction',
  status,
  started_at: 1_000,
  ...overrides,
});

const session = (id: string): Session => ({
  id,
  title: `Chat ${id}`,
  name: '',
  mode: 'chat',
  origin: 'web',
  archived: false,
  pinned: false,
  created: 1,
  lastMessageAt: 1,
  messages: [
    { id: `${id}-u1`, role: 'user', content: 'Question', created: 1 },
    { id: `${id}-a1`, role: 'assistant', content: 'Answer', created: 2 },
  ],
});

type StatusEntry = Record<string, unknown>;

/**
 * A fake /sessions/status server. It honours ETags like the real one: a
 * conditional request whose tag matches the current snapshot gets 304.
 */
class StatusServer {
  entries: Record<string, StatusEntry> = { s1: {}, s2: {} };
  etag = 'etag-0';
  failing = false;
  readonly calls: string[] = [];
  set(id: string, entry: StatusEntry): void {
    this.entries = { ...this.entries, [id]: entry };
    this.etag = `etag-${this.calls.length + 1}-${Math.random().toString(36).slice(2, 6)}`;
  }
  handler = async (...args: Parameters<AppStore['endpoints']['sessionStatus']>) => {
    const etag = args[3];
    this.calls.push(etag || '');
    if (this.failing) throw new TypeError('Load failed');
    if (etag && etag === this.etag) return { __notModified: true, __etag: this.etag };
    return {
      __etag: this.etag,
      sessions: Object.entries(this.entries).map(([id, entry]) => ({ id, ...entry })),
    };
  };
}

const stores: AppStore[] = [];
afterEach(() => {
  while (stores.length) stores.pop()!.dispose();
});

const createStore = () => {
  const store = new AppStore(config);
  stores.push(store);
  const status = new StatusServer();
  store.sessions.value = [session('s1'), session('s2')];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  store.endpoints.diffComments = vi.fn(async () => ({ comments: [], transcript_rev: 0 }));
  store.endpoints.sessionStatus = vi.fn(status.handler);
  store.endpoints.sessionState = vi.fn(async () => ({}));
  store.endpoints.selectedSession = vi.fn(async (id: string) => ({
    selected_session: { id },
    selected_transcript: { bodies: { rev: 1, messages: [] } },
  }));
  store.endpoints.compact = vi.fn(async () => {
    throw new Error('unexpected compaction request');
  });
  // Every real send/steer path ends at one of these; they must stay silent
  // while compaction owns the session.
  store.endpoints.createResponse = vi.fn(() => new Promise<Response>(() => {}));
  store.endpoints.interrupt = vi.fn(async () => ({}));
  store.endpoints.response = vi.fn(async () => {
    throw new Error('unexpected response snapshot request');
  });
  return { store, status };
};

const renderComposer = (store: AppStore) => {
  render(
    <StoreContext.Provider value={store}>
      <Composer />
    </StoreContext.Provider>,
  );
  return screen.getByRole('textbox', { name: 'Message' }) as HTMLTextAreaElement;
};

const typeAndEnter = (input: HTMLElement, value: string) => {
  fireEvent.input(input, { target: { value } });
  fireEvent.keyDown(input, { key: 'Enter' });
};

/**
 * Runs /compact from the keyboard. `settled` is the whole kickoff, including
 * its follow-up status refresh; it is wrapped so awaiting the helper does not
 * flatten into waiting for the compaction itself.
 */
const kickoff = async (
  store: AppStore,
  input: HTMLElement,
): Promise<{ settled: Promise<void> }> => {
  const spy = vi.spyOn(store.selectionStore, 'compactSession');
  await act(async () => typeAndEnter(input, '/compact'));
  const settled =
    (spy.mock.results.at(-1)?.value as Promise<void> | undefined) ?? Promise.resolve();
  spy.mockRestore();
  return { settled };
};
const kickoffAndSettle = (store: AppStore, input: HTMLElement) =>
  act(async () => {
    const { settled } = await kickoff(store, input);
    await settled;
  });

const banner = () => screen.queryByText(BANNER);

const expectBanner = () => {
  const progress = screen.getByText(BANNER);
  expect(progress).toHaveAttribute('role', 'status');
  expect(progress).toHaveAttribute('aria-live', 'polite');
  expect(progress.querySelector('.compaction-status-spinner')).toHaveAttribute(
    'aria-hidden',
    'true',
  );
};

/** Keyboard, button, store facade and engine entry points must all refuse. */
const expectLocked = async (store: AppStore, input: HTMLElement, draft: string) => {
  expectBanner();
  const button = screen.getByRole('button', { name: BUTTON });
  expect(button).toBeDisabled();
  expect(input).not.toBeDisabled();
  expect(store.canSteer.value).toBe(false);
  expect(store.sendBlocked.value).toBe(true);
  await act(async () => {
    fireEvent.click(button);
    fireEvent.keyDown(input, { key: 'Enter' });
    await store.send();
    await store.steer('A follow-up');
    await store.runEngine.send();
    await store.runEngine.steer('A follow-up');
  });
  expect(store.endpoints.createResponse).not.toHaveBeenCalled();
  expect(store.endpoints.interrupt).not.toHaveBeenCalled();
  expect(store.prompt.value).toBe(draft);
  expect(input).toHaveValue(draft);
};

/** The button goes back to its ordinary label; it is enabled once there is a draft. */
const expectUnlocked = (store: AppStore, label = 'Send message') => {
  expect(banner()).not.toBeInTheDocument();
  const button = screen.getByRole('button', { name: label });
  if (store.prompt.value.trim()) expect(button).toBeEnabled();
  expect(store.runEngine.compacting.value).toBe(false);
  expect(store.sendBlocked.value).toBe(false);
};

const toastMessages = (store: AppStore, kind?: 'info' | 'success' | 'error') =>
  store.toasts.value.filter((toast) => !kind || toast.kind === kind).map((toast) => toast.message);

describe('manual compaction', () => {
  it('locks from kickoff through acceptance until status reports a terminal snapshot', async () => {
    const { store, status } = createStore();
    const accepted = deferred<{ activity: ServeSessionActivity }>();
    store.endpoints.compact = vi.fn(() => accepted.promise);
    const input = renderComposer(store);

    const { settled } = await kickoff(store, input);
    expect(store.endpoints.compact).toHaveBeenCalledExactlyOnceWith('s1');
    expect(store.prompt.value).toBe('');
    await expectLocked(store, input, '');

    // A second /compact while the first is unconfirmed never reaches the server.
    await act(async () => typeAndEnter(input, '/compact'));
    expect(store.endpoints.compact).toHaveBeenCalledOnce();
    await act(async () => {
      fireEvent.input(input, { target: { value: 'My next message' } });
    });

    // The 202 reply is only an admission; the server snapshot still says running.
    status.set('s1', { activity: activity('running') });
    await act(async () => {
      accepted.resolve({ activity: activity('running') });
      await settled;
    });
    expect(store.endpoints.sessionStatus).toHaveBeenCalled();
    await expectLocked(store, input, 'My next message');
    // Activity is not a response run: nothing to attach to or resume.
    expect(store.runs.value).toEqual({});
    expect(store.endpoints.response).not.toHaveBeenCalled();
    expect(store.activeSession.value?.activeRun).toBeFalsy();

    // Still running on the next poll: still locked, with a single compact request.
    await act(() => store.statusReconciler.refresh(true));
    await expectLocked(store, input, 'My next message');
    expect(store.endpoints.compact).toHaveBeenCalledOnce();

    status.set('s1', { activity: activity('completed') });
    await act(() => store.statusReconciler.refresh(true));
    expectUnlocked(store);
    expect(store.canSteer.value).toBe(false);
    expect(store.sendBlocked.value).toBe(false);
    expect(toastMessages(store, 'error')).toEqual([]);

    await act(async () => {
      fireEvent.keyDown(input, { key: 'Enter' });
    });
    expect(store.endpoints.createResponse).toHaveBeenCalledOnce();
  });

  it('keeps the lock until the finished transcript revision is installed', async () => {
    const { store, status } = createStore();
    store.endpoints.compact = vi.fn(async () => ({ activity: activity('running') }));
    status.set('s1', { activity: activity('running') });
    const input = renderComposer(store);
    await kickoffAndSettle(store, input);
    await act(async () => {
      fireEvent.input(input, { target: { value: 'Draft' } });
    });
    await expectLocked(store, input, 'Draft');

    // The server finished and wrote a boundary at revision 5. The lock must not
    // drop before this client has the bodies for that revision.
    status.set('s1', { transcript_rev: 5, activity: activity('completed') });
    status.set('s2', {
      activity: activity('running', { id: 'other-compaction', started_at: 2000 }),
    });
    const requested = deferred<void>();
    const bodies = deferred<Record<string, unknown>>();
    store.endpoints.selectedSession = vi.fn((id: string) => {
      expect(id).toBe('s1');
      requested.resolve();
      return bodies.promise;
    });
    let refresh!: Promise<void>;
    await act(async () => {
      refresh = store.statusReconciler.refresh(true);
      await requested.promise;
    });
    await expectLocked(store, input, 'Draft');

    // A stale body set (older than the finished revision) is not enough.
    await act(async () => {
      bodies.resolve({
        selected_session: { id: 's1', transcript_rev: 4 },
        selected_transcript: { bodies: { rev: 4, messages: [] } },
      });
      await refresh;
    });
    await expectLocked(store, input, 'Draft');
    expect(store.sessionStore.activity.busy('s2')).toBe(true);

    store.endpoints.selectedSession = vi.fn(async (id: string) => ({
      selected_session: { id, transcript_rev: 5 },
      selected_transcript: { bodies: { rev: 5, messages: [] } },
    }));
    await act(() => store.statusReconciler.refresh());
    expect(status.calls.at(-1)).toBe('');
    expect(store.activeSession.value?.messageBodiesRev).toBe(5);
    expectUnlocked(store);
    expect(store.prompt.value).toBe('Draft');
    expect(toastMessages(store, 'error')).toEqual([]);
  });

  it('rejects a definitive 4xx immediately with a readable error and allows a retry', async () => {
    const { store } = createStore();
    store.endpoints.compact = vi.fn(async () => {
      throw new APIError('session is busy processing another request', 409, '{}', 'conflict');
    });
    const input = renderComposer(store);

    for (let attempt = 1; attempt <= 2; attempt++) {
      await kickoffAndSettle(store, input);
      expect(store.endpoints.compact).toHaveBeenCalledTimes(attempt);
      expectUnlocked(store);
    }
    expect(toastMessages(store, 'error')).toEqual([
      'session is busy processing another request',
      'session is busy processing another request',
    ]);
    expect(toastMessages(store, 'info')).toEqual([]);
  });

  it('holds a checking lock after a lost reply and failed refresh, then recovers from status', async () => {
    const { store, status } = createStore();
    store.endpoints.compact = vi.fn(async () => {
      throw new TypeError('Load failed');
    });
    status.failing = true;
    const input = renderComposer(store);

    await kickoffAndSettle(store, input);
    expect(store.endpoints.compact).toHaveBeenCalledOnce();
    expect(status.calls).toHaveLength(1);
    expect(toastMessages(store, 'info')).toEqual([
      'Could not confirm compaction. Checking session status…',
    ]);
    expect(toastMessages(store, 'error')).toEqual([]);
    await act(async () => {
      fireEvent.input(input, { target: { value: 'Draft' } });
    });
    await expectLocked(store, input, 'Draft');

    // Uncertainty is not failure: no replay of the POST, no duplicate request.
    await act(async () => typeAndEnter(input, '/compact'));
    expect(store.endpoints.compact).toHaveBeenCalledOnce();

    // The network returns; the server did receive the request after all.
    status.failing = false;
    status.set('s1', { activity: activity('running') });
    await act(() => store.statusReconciler.refresh(false));
    expect(status.calls.at(-1)).toBe('');
    await expectLocked(store, input, '/compact');

    status.set('s1', { activity: activity('completed') });
    await act(() => store.statusReconciler.refresh(false));
    expectUnlocked(store);
    expect(toastMessages(store, 'error')).toEqual([]);
  });

  it('does not refetch later transcript revisions for an already completed compaction', async () => {
    const { store, status } = createStore();
    const completed = activity('completed', { transcript_rev: 5 });
    store.sessionStore.update('s1', (entry) => ({
      ...entry,
      transcriptRev: 5,
      messageBodiesRev: 5,
    }));
    store.sessionStore.activity.observe('s1', completed);
    status.set('s1', { transcript_rev: 9, activity: completed });
    renderComposer(store);
    await act(() => store.statusReconciler.refresh());
    expect(store.endpoints.selectedSession).not.toHaveBeenCalled();
    expect(store.activeSession.value?.transcriptRev).toBe(9);
    expect(store.sessionStore.activity.busy('s1')).toBe(false);
  });

  it('resolves an uncertain bridge even if its session disappeared from the status universe', async () => {
    const { store, status } = createStore();
    store.endpoints.compact = vi.fn(async () => {
      throw new TypeError('Load failed');
    });
    status.failing = true;
    const input = renderComposer(store);
    await kickoffAndSettle(store, input);
    expect(store.sessionStore.activity.unconfirmed.value).toBe(true);
    await act(async () => {
      store.activeSessionId.value = 's2';
      store.sessions.value = store.sessions.peek().filter((entry) => entry.id !== 's1');
    });
    delete status.entries.s1;
    status.failing = false;
    await act(() => store.statusReconciler.refresh());
    expect(store.sessionStore.activity.unconfirmed.value).toBe(false);
    expect(store.sessionStore.activity.anyActive.value).toBe(false);
    expect(store.sessionStore.activity.sessionIds.value).not.toContain('s1');
    expect(store.endpoints.compact).toHaveBeenCalledOnce();
  });

  it('resolves a lost reply through an ordinary poll even when the server snapshot is unchanged', async () => {
    const { store, status } = createStore();
    const input = renderComposer(store);
    // An earlier poll cached the idle snapshot's ETag.
    await act(() => store.statusReconciler.refresh(false));
    expect(status.calls).toEqual(['']);

    store.endpoints.compact = vi.fn(async () => {
      throw new TypeError('Load failed');
    });
    status.failing = true;
    await kickoffAndSettle(store, input);
    await expectLocked(store, input, '');

    // The POST never reached the server, so its snapshot (and ETag) did not
    // change. A conditional poll would be answered 304 and the lock would
    // never clear; the unconfirmed bridge must force a full snapshot.
    status.failing = false;
    await act(() => store.statusReconciler.refresh(false));
    expect(status.calls.at(-1)).toBe('');
    expectUnlocked(store);
    expect(toastMessages(store, 'error')).toEqual([]);
  });

  it('scopes the lock to its session and locks again on reselect', async () => {
    const { store, status } = createStore();
    store.endpoints.compact = vi.fn(async () => ({ activity: activity('running') }));
    status.set('s1', { activity: activity('running') });
    const input = renderComposer(store);
    await kickoffAndSettle(store, input);
    await expectLocked(store, input, '');

    await act(() => {
      store.activeSessionId.value = 's2';
    });
    expectUnlocked(store);
    expect(store.sendBlocked.value).toBe(false);

    await act(() => {
      store.activeSessionId.value = 's1';
    });
    await expectLocked(store, input, '');

    // Polling while another session is selected still tracks s1's lifecycle.
    await act(() => {
      store.activeSessionId.value = 's2';
    });
    status.set('s1', { activity: activity('completed') });
    await act(() => store.statusReconciler.refresh(true));
    await act(() => {
      store.activeSessionId.value = 's1';
    });
    expectUnlocked(store);
  });

  it('reads a running activity from session state on reload and clears it when state settles', async () => {
    const { store } = createStore();
    store.endpoints.sessionState = vi.fn(async () => ({ activity: activity('running') }));
    const input = renderComposer(store);
    expectUnlocked(store);

    await act(() => store.loadSession('s1'));
    await act(async () => {
      fireEvent.input(input, { target: { value: 'Draft' } });
    });
    await expectLocked(store, input, 'Draft');
    expect(store.endpoints.compact).not.toHaveBeenCalled();
    expect(store.runs.value).toEqual({});

    store.endpoints.sessionState = vi.fn(async () => ({ activity: activity('completed') }));
    await act(() => store.loadSession('s1'));
    expectUnlocked(store);
    expect(store.prompt.value).toBe('Draft');
  });
});

describe('automatic compaction', () => {
  const streamingStore = () => {
    const created = createStore();
    created.store.runs.value = {
      s1: initialProjection({
        responseId: 'r1',
        sessionId: 's1',
        epoch: 1,
        status: 'streaming',
        lastSequence: 0,
        startedRev: 0,
        reconnects: 0,
      }),
    };
    created.store.runEngine.markResponseTransportActive('s1', 'r1');
    return created;
  };
  const event = (sequence: number, type: string, fields: Record<string, unknown> = {}) => ({
    type,
    response_id: 'r1',
    run_epoch: 1,
    sequence_number: sequence,
    ...fields,
  });

  it.each([
    ['completed', activity('completed'), 'Steer'],
    ['cancelled', activity('cancelled'), 'Steer'],
    ['failed', activity('failed', { error: 'compaction failed: context too large' }), 'Steer'],
  ])('uses the shared banner and guards and unlocks on %s', async (_outcome, terminal, label) => {
    const { store } = streamingStore();
    const input = renderComposer(store);
    await act(async () => {
      fireEvent.input(input, { target: { value: 'My next message' } });
    });
    expect(store.canSteer.value).toBe(true);

    // Human-readable phase text is not lifecycle truth.
    await act(() =>
      store.runEngine.applyResponseEvent(
        's1',
        event(1, 'response.phase', { text: 'Compacting: summarize history' }),
      ),
    );
    expect(banner()).not.toBeInTheDocument();
    expect(store.canSteer.value).toBe(true);

    await act(() =>
      store.runEngine.applyResponseEvent(
        's1',
        event(2, 'response.activity', { activity: activity('running') }),
      ),
    );
    await expectLocked(store, input, 'My next message');

    // Ordinary stream traffic during compaction does not clear the lock.
    await act(() =>
      store.runEngine.applyResponseEvent(
        's1',
        event(3, 'response.output_text.delta', { delta: 'Still working' }),
      ),
    );
    await expectLocked(store, input, 'My next message');

    await act(() =>
      store.runEngine.applyResponseEvent(
        's1',
        event(4, 'response.activity', { activity: terminal }),
      ),
    );
    expectUnlocked(store, label);
    expect(store.canSteer.value).toBe(true);
    expect(store.prompt.value).toBe('My next message');
    expect(toastMessages(store, 'error')).toEqual(terminal.error ? [terminal.error] : []);

    // The same terminal snapshot observed again is not re-toasted.
    await act(() =>
      store.runEngine.applyResponseEvent(
        's1',
        event(5, 'response.activity', { activity: terminal }),
      ),
    );
    expect(toastMessages(store, 'error')).toEqual(terminal.error ? [terminal.error] : []);
  });

  it('shows the same lock when status polling reports engine compaction for the selected run', async () => {
    const { store, status } = streamingStore();
    const input = renderComposer(store);
    await act(async () => {
      fireEvent.input(input, { target: { value: 'My next message' } });
    });

    status.set('s1', {
      active_run: true,
      active_response_id: 'r1',
      activity: activity('running'),
    });
    await act(() => store.statusReconciler.refresh(true));
    await expectLocked(store, input, 'My next message');
    expect(store.endpoints.compact).not.toHaveBeenCalled();

    status.set('s1', {
      active_run: true,
      active_response_id: 'r1',
      activity: activity('completed'),
    });
    await act(() => store.statusReconciler.refresh(true));
    expectUnlocked(store, 'Steer');
    expect(store.canSteer.value).toBe(true);
  });
});
