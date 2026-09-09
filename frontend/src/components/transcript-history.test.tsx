import { act, fireEvent, render, screen, waitFor } from '@testing-library/preact';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig, testSession } from '../stores/store-test-fixtures';
import { Transcript } from './Transcript';

const stores: AppStore[] = [];
afterEach(() => {
  stores.splice(0).forEach((store) => store.dispose());
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function setup(olderTranscriptAnchors: number[] = [1]) {
  const store = new AppStore(testConfig);
  stores.push(store);
  store.sessions.value = [
    testSession({
      transcriptRev: 7,
      messageBodiesRev: 7,
      olderTranscriptAnchors,
      messages: [
        {
          id: 'current',
          durableRowId: 3,
          serverSeq: 2,
          role: 'user',
          content: 'Current question',
          created: 1,
        },
      ],
    }),
  ];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  store.endpoints.transcriptBodies = vi.fn(async () => ({
    rev: 7,
    messages: [
      { id: 1, sequence: 0, role: 'user', parts: [{ type: 'text', text: 'First question' }] },
      { id: 2, sequence: 1, role: 'assistant', parts: [{ type: 'text', text: 'First answer' }] },
    ],
  }));
  return store;
}

function mount(store: AppStore) {
  return render(
    <StoreContext.Provider value={store}>
      <Transcript />
    </StoreContext.Provider>,
  );
}

describe('infinite transcript scroll', () => {
  it('loads older server bodies on intersection without requiring a click', async () => {
    let callback!: IntersectionObserverCallback;
    const disconnect = vi.fn();
    vi.stubGlobal(
      'IntersectionObserver',
      class {
        constructor(handler: IntersectionObserverCallback) {
          callback = handler;
        }
        observe = vi.fn();
        disconnect = disconnect;
      },
    );
    const store = setup();
    const { container } = mount(store);
    await waitFor(() => expect(callback).toBeTypeOf('function'));
    act(() =>
      callback([{ isIntersecting: true } as IntersectionObserverEntry], {} as IntersectionObserver),
    );
    expect(store.endpoints.transcriptBodies).not.toHaveBeenCalled();
    // A visible sentinel must not unpin a newly opened transcript. User intent arms it.
    fireEvent.wheel(container.querySelector('#chatScroll')!, { deltaY: -100 });
    act(() =>
      callback([{ isIntersecting: true } as IntersectionObserverEntry], {} as IntersectionObserver),
    );
    await screen.findByText('First question');
    expect(screen.queryByRole('button', { name: 'Load earlier messages' })).not.toBeInTheDocument();
    expect(disconnect).toHaveBeenCalled();
  });

  it('loads on scroll without IntersectionObserver and preserves the visible row position', async () => {
    vi.stubGlobal('IntersectionObserver', undefined);
    const store = setup();
    const { container } = mount(store);
    const viewport = container.querySelector<HTMLElement>('#chatScroll')!;
    Object.defineProperties(viewport, {
      clientHeight: { configurable: true, value: 400 },
      scrollHeight: { configurable: true, value: 2000 },
    });
    viewport.scrollTop = 100;
    const row = container.querySelector<HTMLElement>('[data-message-id="current"]')!;
    vi.spyOn(row, 'getBoundingClientRect').mockImplementation(
      () =>
        ({
          top: store.activeSession.value!.messages.length > 1 ? 350 : 50,
          bottom: store.activeSession.value!.messages.length > 1 ? 450 : 150,
        }) as DOMRect,
    );
    fireEvent.wheel(viewport, { deltaY: -100 });
    fireEvent.scroll(viewport);
    await screen.findByText('First question');
    expect(viewport.scrollTop).toBe(400);
  });

  it('exposes a retry button and does not retry failures on every scroll', async () => {
    const store = setup();
    const fetch = store.endpoints.transcriptBodies;
    store.endpoints.transcriptBodies = vi.fn(async () => {
      throw new Error('offline');
    });
    const { container } = mount(store);
    fireEvent.click(screen.getByRole('button', { name: 'Load earlier messages' }));
    const retry = await screen.findByRole('button', {
      name: 'Couldn’t load earlier messages. Retry',
    });
    fireEvent.scroll(container.querySelector('#chatScroll')!);
    expect(store.endpoints.transcriptBodies).toHaveBeenCalledOnce();
    store.endpoints.transcriptBodies = fetch;
    fireEvent.click(retry);
    await screen.findByText('First question');
  });

  it('automatically reveals locally windowed turns without a server request', async () => {
    const store = setup([]);
    store.sessions.value = [
      testSession({
        messages: Array.from({ length: 100 }, (_, index) => ({
          id: `user-${index}`,
          role: 'user',
          content: `Question ${index}`,
          created: 1,
        })),
      }),
    ];
    const { container } = mount(store);
    expect(screen.queryByText('Question 0')).not.toBeInTheDocument();
    const viewport = container.querySelector<HTMLElement>('#chatScroll')!;
    Object.defineProperties(viewport, {
      clientHeight: { configurable: true, value: 400 },
      scrollHeight: { configurable: true, value: 2000 },
    });
    fireEvent.wheel(viewport, { deltaY: -100 });
    fireEvent.scroll(viewport);
    await screen.findByText('Question 0');
    expect(store.endpoints.transcriptBodies).not.toHaveBeenCalled();
    expect(screen.queryByRole('button', { name: 'Load earlier messages' })).not.toBeInTheDocument();
  });
});
