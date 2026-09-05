import { act, fireEvent, render, screen, within } from '@testing-library/preact';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { App } from '../app/App';
import { AppStore } from '../stores/app-store';
import { testConfig, testSession } from '../stores/store-test-fixtures';
import { initialProjection } from '../domain/response';
import type { DiffFile } from '../domain/types';
import { observeRenders } from '../test/render-counts';
import { Transcript } from './Transcript';
import { Sidebar } from './Sidebar';
import { DiffSidebar } from './Panels';
import { Composer } from './Composer';
import { VoiceOperation, type VoiceSnapshot } from '../platform/voice';

let renders: ReturnType<typeof observeRenders> | undefined;
afterEach(() => {
  renders?.dispose();
  vi.useRealTimers();
});
const observe = () => (renders = observeRenders());

function createStore() {
  const store = new AppStore(testConfig);
  store.sessions.value = [
    testSession({
      messages: [
        { id: 'u1', role: 'user', content: 'Earlier question', created: 1 },
        { id: 'a1', role: 'assistant', content: 'Earlier answer', created: 2 },
        {
          id: 't1',
          role: 'tool-group',
          content: '',
          created: 3,
          tools: [{ id: 'tool1', name: 'read_file', status: 'done', result: 'done' }],
        },
      ],
    }),
    testSession({ id: 's2', title: 'Background' }),
  ];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  store.runs.value = {
    s1: initialProjection({
      sessionId: 's1',
      responseId: 'r1',
      status: 'streaming',
      lastSequence: 0,
      startedAt: 1,
      epoch: 1,
      startedRev: 0,
      reconnects: 0,
    }),
    s2: initialProjection({
      sessionId: 's2',
      responseId: 'r2',
      status: 'streaming',
      lastSequence: 0,
      startedAt: 1,
      epoch: 1,
      startedRev: 0,
      reconnects: 0,
    }),
  };
  store.runEngine.markResponseTransportActive('s1', 'r1');
  store.runEngine.markResponseTransportActive('s2', 'r2');
  return store;
}
function textUpdate(store: AppStore, id: string, content: string) {
  const projection = store.runs.peek()[id];
  store.runs.value = {
    ...store.runs.peek(),
    [id]: {
      ...projection,
      messages: [
        { id: `${id}-u`, role: 'user', content: 'New question', created: 4 },
        {
          id: `${id}-a`,
          role: 'assistant',
          content,
          responseId: projection.run.responseId,
          created: 5,
        },
      ],
    },
  };
}
function openDiff(store: AppStore) {
  const file: DiffFile = {
    path: 'main.go',
    status: 'modify',
    expanded: true,
    additions: 100,
    deletions: 1,
    lines: [
      { kind: 'delete', oldLine: 1, content: 'old value' },
      ...Array.from({ length: 100 }, (_, i) => ({
        kind: 'add' as const,
        newLine: i + 1,
        content: `new value ${i}`,
      })),
    ],
  };
  store.diff.value = {
    ...store.diff.peek(),
    open: true,
    scope: 'uncommitted',
    sessionId: 's1',
    files: [file],
    width: 555,
  };
}

describe('render isolation', () => {
  it('does not rerender session rows for text updates but still updates terminal state', async () => {
    const store = createStore();
    const counts = observe();
    render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );
    expect(counts.count('SessionRow:s1')).toBeGreaterThan(0);
    counts.clear();
    await act(() => textUpdate(store, 's1', 'streaming'));
    expect(counts.count('SessionRow')).toBe(0);
    await act(() => {
      const run = store.runs.peek().s1;
      store.runs.value = {
        ...store.runs.peek(),
        s1: { ...run, run: { ...run.run, status: 'completed' } },
      };
    });
    expect(counts.count('SessionRow:s1')).toBeGreaterThan(0);
    expect(counts.count('SessionRow:s2')).toBe(0);
  });

  it('tracks transport-only changes and isolates background interaction counts', async () => {
    const store = createStore();
    const counts = observe();
    render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );
    counts.clear();
    await act(() => store.runEngine.clearResponseTransport('s1', 'r1'));
    expect(counts.count('SessionRow:s1')).toBeGreaterThan(0);
    expect(counts.count('SessionRow:s2')).toBe(0);
    counts.clear();
    await act(() => store.runEngine.markResponseTransportActive('s1', 'r1'));
    expect(counts.count('SessionRow:s1')).toBeGreaterThan(0);
    counts.clear();
    await act(() => {
      store.interactionStore.upsert('approval', 's2', 'r2', 'request', {
        sessionId: 's2',
        id: 'request',
        title: 'Run command',
        options: [],
      });
    });
    expect(counts.count('SessionRow:s1')).toBe(0);
    expect(counts.count('SessionRow:s2')).toBeGreaterThan(0);
  });

  it('keeps the foreground transcript stable during background and status-only updates', async () => {
    const store = createStore();
    const counts = observe();
    render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );
    const messages = store.visibleMessages.value;
    counts.clear();
    await act(() => textUpdate(store, 's2', 'background text'));
    expect(store.visibleMessages.value).toBe(messages);
    expect(counts.count('Transcript')).toBe(0);
    await act(() => {
      const run = store.runs.peek().s1;
      store.runs.value = { ...store.runs.peek(), s1: { ...run, phase: 'Thinking' } };
    });
    expect(store.visibleMessages.value).toBe(messages);
    expect(screen.getByText('Thinking')).toBeInTheDocument();
    await act(() => {
      store.activeSessionId.value = 's2';
    });
    expect(
      store.visibleMessages.value.some((message) => message.content === 'background text'),
    ).toBe(true);
  });

  it('does not execute historical message rows on foreground text updates', async () => {
    const store = createStore();
    const counts = observe();
    textUpdate(store, 's1', 'first');
    render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );
    expect(counts.count('MessageRow:a1')).toBeGreaterThan(0);
    counts.clear();
    await act(() => textUpdate(store, 's1', 'next'));
    expect(counts.count('MessageRow:s1-a')).toBeGreaterThan(0);
    for (const id of ['u1', 'a1', 't1']) expect(counts.count(`MessageRow:${id}`)).toBe(0);
  });

  it('refreshes timestamps without executing message rows or the transcript', async () => {
    vi.useFakeTimers();
    vi.setSystemTime(10_000);
    const store = createStore();
    const counts = observe();
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );
    expect(container.querySelector('time')).toHaveTextContent('now');
    counts.clear();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(container.querySelector('time')).toHaveTextContent('1m');
    expect(counts.count('MessageTime')).toBeGreaterThan(0);
    expect(counts.count('MessageRow')).toBe(0);
    expect(counts.count('Transcript')).toBe(0);
  });

  it('refreshes memoized Markdown when a media reference becomes available', async () => {
    const store = createStore();
    const counts = observe();
    const reference = 'a'.repeat(32);
    const url = `/media/${reference}.png`;
    store.sessions.value = [
      testSession({
        messages: [
          ...store.sessions.peek()[0].messages,
          {
            id: 'media-text',
            role: 'assistant',
            content: `![Picture](term-llm-media://${reference})`,
            created: 1,
          },
        ],
      }),
    ];
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );
    expect(container.querySelector('.media-reference-missing')).toBeInTheDocument();
    counts.clear();
    await act(() => {
      const run = store.runs.peek().s1;
      store.runs.value = {
        ...store.runs.peek(),
        s1: {
          ...run,
          messages: [
            {
              id: 'media-tool',
              role: 'tool-group',
              content: '',
              created: 2,
              tools: [
                {
                  id: 'media-call',
                  name: 'show_media',
                  status: 'done',
                  media: [{ reference, url, type: 'image/png' }],
                },
              ],
            },
          ],
        },
      };
    });
    expect(container.querySelector('.message.assistant img')).toHaveAttribute('src', url);
    expect(counts.count('MessageRow:a1')).toBe(0);
    expect(counts.count('MessageRow:media-text')).toBeGreaterThan(0);
  });

  it('does not rerender a normal diff sidebar during streaming, and isolates visible activity', async () => {
    const store = createStore();
    openDiff(store);
    const counts = observe();
    render(
      <StoreContext.Provider value={store}>
        <DiffSidebar />
      </StoreContext.Provider>,
    );
    expect(counts.count('File')).toBeGreaterThan(0);
    counts.clear();
    await act(() => textUpdate(store, 's1', 'streaming'));
    expect(counts.count('DiffSidebar')).toBe(0);
    expect(counts.count('File')).toBe(0);
    await act(() => {
      store.diff.value = { ...store.diff.peek(), maximized: true };
    });
    counts.clear();
    await act(() => textUpdate(store, 's1', 'more text'));
    expect(counts.count('File')).toBe(0);
    expect(counts.count('DiffActivity')).toBe(0);
    await act(() => {
      const run = store.runs.peek().s1;
      store.runs.value = {
        ...store.runs.peek(),
        s1: { ...run, run: { ...run.run, status: 'cancelling' } },
      };
    });
    expect(screen.getByLabelText('Assistant is responding: Stopping')).toBeInTheDocument();
    expect(counts.count('File')).toBe(0);
  });

  it('only rerenders the edited diff line and submits its latest draft', async () => {
    const store = createStore();
    openDiff(store);
    store.queueDiffComment = vi.fn();
    const counts = observe();
    render(
      <StoreContext.Provider value={store}>
        <DiffSidebar />
      </StoreContext.Provider>,
    );
    const buttons = screen.getAllByRole('button', { name: 'Comment on line 1' });
    fireEvent.click(buttons.at(-1)!);
    counts.clear();
    fireEvent.input(screen.getByRole('textbox', { name: 'Inline comment' }), {
      target: { value: 'Latest draft' },
    });
    expect(counts.count('Line')).toBe(1);
    expect(counts.count('Line:2')).toBe(0);
    fireEvent.click(screen.getByRole('button', { name: 'More send options' }));
    fireEvent.click(screen.getByRole('menuitem', { name: /Queue comment/ }));
    expect(store.queueDiffComment).toHaveBeenCalledWith(
      expect.objectContaining({ body: 'Latest draft', line: 1, side: 'new' }),
    );
  });

  it('changes drag layout without rendering content, commits on release, and cancels cleanly', () => {
    const store = createStore();
    openDiff(store);
    store.startupDone.value = true;
    store.bootstrap = vi.fn(async () => undefined);
    const counts = observe();
    const { container, unmount } = render(<App store={store} />);
    const shell = container.querySelector<HTMLElement>('#appShell')!;
    const handle = screen.getByRole('separator', { name: 'Resize changes panel' });
    const state = store.diff.peek();
    counts.clear();
    fireEvent.pointerDown(handle, { clientX: 600, pointerId: 1 });
    fireEvent.pointerMove(window, { clientX: 500, pointerId: 1 });
    expect(shell.style.getPropertyValue('--diff-sidebar-user-width')).toBe('655px');
    expect(store.diff.peek()).toBe(state);
    expect(counts.count('File')).toBe(0);
    expect(counts.count('App')).toBe(0);
    act(() => {
      store.shellStore.dockBottomSize.value = 333;
    });
    expect(shell.style.getPropertyValue('--shell-dock-bottom-size')).toBe('333px');
    expect(shell.style.getPropertyValue('--diff-sidebar-user-width')).toBe('655px');
    expect(counts.count('App')).toBe(0);
    fireEvent.pointerUp(window, { clientX: 500, pointerId: 1 });
    expect(store.diff.peek().width).toBe(655);
    fireEvent.pointerDown(handle, { clientX: 600, pointerId: 1 });
    fireEvent.pointerMove(window, { clientX: 500, pointerId: 1 });
    fireEvent.pointerCancel(window, { pointerId: 1 });
    expect(shell.style.getPropertyValue('--diff-sidebar-user-width')).toBe('655px');
    fireEvent.pointerDown(handle, { clientX: 600, pointerId: 1 });
    unmount();
    fireEvent.pointerMove(window, { clientX: 100, pointerId: 1 });
    expect(store.diff.peek().width).toBe(655);
  });

  it('updates voice progress without rerendering the composer and retains recording controls', async () => {
    const store = createStore();
    let publish!: (snapshot: VoiceSnapshot) => void;
    let snapshot!: VoiceSnapshot;
    vi.spyOn(VoiceOperation.prototype, 'subscribe').mockImplementation(function (
      this: VoiceOperation,
      listener,
    ) {
      publish = listener;
      snapshot = this.snapshot;
      listener(snapshot);
      return () => {};
    });
    const counts = observe();
    render(
      <StoreContext.Provider value={store}>
        <Composer />
      </StoreContext.Provider>,
    );
    expect(document.querySelector('#voiceStatus')).toBeNull();
    const inner = document.querySelector('.composer-inner')!;
    expect(
      [...inner.childNodes].some(
        (node) => node.nodeType === Node.TEXT_NODE && node.textContent === ' ',
      ),
    ).toBe(false);
    await act(() => {
      snapshot = { ...snapshot, phase: 'recording' };
      publish(snapshot);
    });
    counts.clear();
    await act(() => publish({ ...snapshot, durationMs: 2_000 }));
    expect(screen.getByText('Recording 0:02')).toBeInTheDocument();
    expect(
      within(screen.getByRole('status')).getByRole('button', { name: 'Stop' }),
    ).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeInTheDocument();
    expect(counts.count('VoiceStatus')).toBeGreaterThan(0);
    expect(counts.count('Composer')).toBe(0);
  });
});
