import { batch } from '@preact/signals';
import { act, fireEvent, render } from '@testing-library/preact';
import { expect, it, vi } from 'vitest';
import type { AppConfig } from '../app/config';
import { StoreContext } from '../app/context';
import { MemoryBackend, PersistentCache } from '../platform/persistent-cache';
import { AppStore } from '../stores/app-store';
import { Transcript } from './Transcript';

const config: AppConfig = {
  prefix: '/ui',
  version: 'v1',
  sidebarCategories: ['all'],
  agentName: '',
  agentNames: [],
  title: '',
  locationSharing: false,
  worktrees: false,
  hub: null,
  vapidKey: '',
  webRTC: false,
  signalingURL: '',
};

it('allows history paging after the visible startup placeholder hydrates', () => {
  const store = new AppStore(config, localStorage, new PersistentCache(new MemoryBackend()));
  const paging = vi.spyOn(store.selectionStore, 'loadOlderMessages').mockResolvedValue(undefined);
  vi.spyOn(HTMLElement.prototype, 'scrollHeight', 'get').mockImplementation(function (
    this: HTMLElement,
  ) {
    return this.id === 'chatScroll' && store.visibleMessages.peek().length ? 1_000 : 0;
  });
  vi.spyOn(HTMLElement.prototype, 'clientHeight', 'get').mockReturnValue(200);
  store.sessions.value = [
    {
      id: 's1',
      name: '',
      title: 'Hydrating',
      mode: 'chat',
      origin: 'web',
      archived: false,
      pinned: false,
      created: 1,
      lastMessageAt: 1,
      messages: [],
    },
  ];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  store.selectionStore.transcriptLoading.value = 's1';
  const rendered = render(
    <StoreContext.Provider value={store}>
      <Transcript />
    </StoreContext.Provider>,
  );
  try {
    const viewport = rendered.container.querySelector<HTMLElement>('#chatScroll')!;
    expect(viewport.scrollTop).toBe(0);
    act(() => {
      batch(() => {
        store.sessionStore.patch('s1', {
          messages: [
            { id: 'tail', role: 'user', content: 'Hydrated tail', created: 1, serverSeq: 100 },
          ],
          messageBodiesRev: 7,
          olderTranscriptAnchors: [1],
        });
        store.selectionStore.transcriptLoading.value = '';
      });
    });
    expect(viewport.scrollTop).toBeGreaterThan(0);
    expect(paging).not.toHaveBeenCalled();
    viewport.scrollTop = 0;
    fireEvent.scroll(viewport);
    expect(paging).toHaveBeenCalledOnce();
  } finally {
    rendered.unmount();
    store.dispose();
  }
});
