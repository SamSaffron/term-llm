import { act, fireEvent, render, screen, within } from '@testing-library/preact';
import { expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig, testSession } from '../stores/store-test-fixtures';
import { Composer } from './Composer';
import { DEFAULT_ATTACHMENT_POLICY } from '../domain/attachments';

const unconfirmed = (clientMessageId: string, content: string) => ({
  id: `pending-${clientMessageId}`,
  clientMessageId,
  content,
  created: Date.now(),
  state: 'unconfirmed',
});

const activeUnconfirmedStore = () => {
  const store = new AppStore(testConfig);
  store.sessions.value = [testSession()];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  store.startupDone.value = true;
  store.runEngine.trackIntent('s1', unconfirmed('client-one', 'possibly sent'));
  store.endpoints.createResponse = vi.fn();
  store.endpoints.cancelResponse = vi.fn();
  return store;
};

it.each([
  ['keyboard', false],
  ['touch-only', true],
])(
  'confirms dismissal in place with %s focus handling and never resends or cancels',
  async (_input, touchOnly) => {
    vi.spyOn(window, 'matchMedia').mockImplementation(
      (query: string) =>
        ({
          matches: touchOnly && query === '(hover: none) and (pointer: coarse)',
          addEventListener: vi.fn(),
          removeEventListener: vi.fn(),
        }) as unknown as MediaQueryList,
    );
    const store = activeUnconfirmedStore();
    let finishCheck!: () => void;
    const check = vi.spyOn(store, 'checkSendStatus').mockReturnValue(
      new Promise<void>((resolve) => {
        finishCheck = resolve;
      }),
    );
    // Escape must not also reach page-level handlers such as panel dismissal.
    const pageKeys = vi.fn();
    window.addEventListener('keydown', pageKeys);
    const view = render(
      <StoreContext.Provider value={store}>
        <Composer />
      </StoreContext.Provider>,
    );
    try {
      const checkStatus = screen.getByRole('button', { name: 'Check status' });
      fireEvent.click(checkStatus);
      fireEvent.click(checkStatus);
      expect(checkStatus).toHaveAttribute('aria-busy', 'true');
      expect(check).toHaveBeenCalledOnce();
      await act(async () => finishCheck());
      expect(checkStatus).not.toHaveAttribute('aria-busy');

      fireEvent.click(screen.getByRole('button', { name: 'Dismiss warning' }));
      const confirmation = screen.getByRole('group', { name: 'Confirm warning dismissal' });
      expect(confirmation).toHaveAccessibleDescription(/dismissing does not cancel it/);
      const keep = within(confirmation).getByRole('button', { name: 'Keep warning' });
      expect(keep).toHaveFocus();
      fireEvent.keyDown(keep, { key: 'Escape' });
      expect(pageKeys).not.toHaveBeenCalled();
      expect(screen.queryByRole('group', { name: 'Confirm warning dismissal' })).toBeNull();
      expect(screen.getByRole('button', { name: 'Dismiss warning' })).toHaveFocus();
      expect(store.sendBlocked.value).toBe(true);

      fireEvent.click(screen.getByRole('button', { name: 'Dismiss warning' }));
      fireEvent.click(screen.getByRole('button', { name: 'Keep warning' }));
      expect(screen.getByRole('button', { name: 'Dismiss warning' })).toHaveFocus();
      expect(store.sendBlocked.value).toBe(true);

      fireEvent.click(screen.getByRole('button', { name: 'Dismiss warning' }));
      fireEvent.click(screen.getByRole('button', { name: 'Dismiss anyway' }));
      expect(store.sendBlocked.value).toBe(false);
      expect(screen.queryByText(/Could not confirm whether this was sent/)).toBeNull();
      // Unlocking hands focus to the message box, except where that would
      // raise the on-screen keyboard.
      const message = screen.getByRole('textbox', { name: 'Message' });
      if (touchOnly) expect(message).not.toHaveFocus();
      else expect(message).toHaveFocus();
      expect(store.prompt.value).toBe('');
      expect(store.endpoints.createResponse).not.toHaveBeenCalled();
      expect(store.endpoints.cancelResponse).not.toHaveBeenCalled();
    } finally {
      window.removeEventListener('keydown', pageKeys);
      view.unmount();
      store.dispose();
    }
  },
);

it("never carries an open dismissal acknowledgement to another conversation's warning", () => {
  const store = activeUnconfirmedStore();
  store.sessions.value = [testSession(), testSession({ id: 's2', title: 'Other' })];
  store.runEngine.trackIntent('s2', unconfirmed('client-two', 'also possibly sent'));
  const view = render(
    <StoreContext.Provider value={store}>
      <Composer />
    </StoreContext.Provider>,
  );
  try {
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss warning' }));
    expect(screen.getByRole('button', { name: 'Keep warning' })).toHaveFocus();
    // Focus has moved on (say, to the chat list) before the switch lands.
    const elsewhere = screen.getByRole('textbox', { name: 'Message' });
    elsewhere.focus();
    act(() => {
      store.activeSessionId.value = 's2';
    });
    expect(screen.queryByRole('group', { name: 'Confirm warning dismissal' })).toBeNull();
    expect(elsewhere).toHaveFocus();
    act(() => {
      store.activeSessionId.value = 's1';
    });
    expect(screen.queryByRole('group', { name: 'Confirm warning dismissal' })).toBeNull();
    expect(store.pendingIntents.value.s1[0].state).toBe('unconfirmed');
    expect(store.pendingIntents.value.s2[0].state).toBe('unconfirmed');
  } finally {
    view.unmount();
    store.dispose();
  }
});

it('keeps local new-chat warnings, one per owner, in the composer and in a delegated child view', () => {
  const store = activeUnconfirmedStore();
  store.runEngine.trackIntent('draft_lost', unconfirmed('client-a', 'first part'));
  store.runEngine.trackIntent('draft_lost', unconfirmed('client-b', 'second part'));
  const view = render(
    <StoreContext.Provider value={store}>
      <Composer />
    </StoreContext.Provider>,
  );
  try {
    const region = screen.getByRole('region', { name: 'Unconfirmed new conversations' });
    expect(region.closest('.composer-inner')).not.toBeNull();
    expect(within(region).getAllByRole('button', { name: 'Dismiss warning' })).toHaveLength(1);
    expect(within(region).getByText('first part')).toBeInTheDocument();
    expect(within(region).getByText('second part')).toBeInTheDocument();

    const child = {
      ...testSession({ id: 'child-1', title: 'Research task' }),
      parentSessionId: 's1',
      delegated: true,
      activeRun: false,
    };
    act(() => {
      store.sessionStore.transientSession.value = child;
      store.activeSessionId.value = child.id;
    });
    expect(screen.queryByRole('textbox', { name: 'Message' })).toBeNull();
    const standalone = screen.getByRole('region', { name: 'Unconfirmed new conversations' });
    expect(within(standalone).getByText('first part')).toBeInTheDocument();
    fireEvent.click(within(standalone).getByRole('button', { name: 'Dismiss warning' }));
    fireEvent.click(within(standalone).getByRole('button', { name: 'Dismiss anyway' }));
    expect(store.pendingIntents.value.draft_lost.map((intent) => intent.state)).toEqual([
      'dismissed-unconfirmed',
      'dismissed-unconfirmed',
    ]);
    expect(view.container).toBeEmptyDOMElement();
  } finally {
    view.unmount();
    store.dispose();
  }
});

it('offers read-only Check status and warning-confirmed Dismiss, never Retry or a progress spinner for a paused send', async () => {
  const store = new AppStore(testConfig);
  store.sessions.value = [testSession()];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  store.startupDone.value = true;
  store.runEngine.trackIntent('s1', {
    id: 'pending-one',
    clientMessageId: 'client-one',
    content: 'possibly sent',
    created: Date.now(),
    state: 'unconfirmed',
  });
  const check = vi.spyOn(store, 'checkSendStatus').mockResolvedValue(undefined);
  store.endpoints.createResponse = vi.fn();
  store.endpoints.cancelResponse = vi.fn();
  const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
  const view = render(
    <StoreContext.Provider value={store}>
      <Composer />
    </StoreContext.Provider>,
  );
  try {
    expect(screen.getByText(/Could not confirm whether this was sent/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Retry same send/i })).not.toBeInTheDocument();
    expect(view.container.querySelector('.interrupt-spinner')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Check status' }));
    expect(check).toHaveBeenCalledOnce();
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss warning' }));
    expect(store.sendBlocked.value).toBe(true);
    expect(screen.getByText(/dismissing does not cancel it/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Keep warning' }));
    expect(store.sendBlocked.value).toBe(true);
    expect(screen.queryByRole('button', { name: 'Dismiss anyway' })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss warning' }));
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss anyway' }));
    expect(confirm).not.toHaveBeenCalled();
    expect(store.sendBlocked.value).toBe(false);
    expect(store.pendingIntents.value.s1[0].state).toBe('dismissed-unconfirmed');
    expect(store.prompt.value).toBe('');
    expect(store.endpoints.createResponse).not.toHaveBeenCalled();
    expect(store.endpoints.cancelResponse).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    store.dispose();
  }
});

it('exposes an ambiguous real new-chat send under its original local owner after AppStore bootstrap with no server session', async () => {
  vi.useFakeTimers();
  const route = `${location.pathname}${location.search}${location.hash}`;
  history.replaceState(null, '', '/ui?new=1');
  const original = new AppStore(testConfig);
  original.newChat(true);
  original.composer.applyAttachmentPolicy({ ...DEFAULT_ATTACHMENT_POLICY, uploadRefs: true });
  original.prompt.value = 'A new conversation that may already be running';
  original.attachments.value = [
    {
      id: 'original-photo',
      name: 'original-photo.png',
      type: 'image/png',
      size: 4,
      blobRef: 'preserve-original-blob',
      uploadId: `upl_${'a'.repeat(43)}`,
      uploadScope: `${location.origin}|/ui|`,
      status: 'ready',
    },
  ];
  original.endpoints.createResponse = vi
    .fn()
    .mockRejectedValue(new TypeError('lost new-chat acceptance'));
  let restored: AppStore | undefined;
  let view: ReturnType<typeof render> | undefined;
  try {
    await original.send();
    const ownerId = original.activeSessionId.value;
    expect(ownerId).toMatch(/^draft_/);
    expect(original.pendingIntents.value[ownerId][0].state).toBe('unconfirmed');
    original.dispose();

    restored = new AppStore(testConfig);
    restored.endpoints.capabilities = vi.fn(async () => ({
      projects: { enabled: false },
      attachments: { upload_refs: { version: 1 } },
    }));
    restored.endpoints.providers = vi.fn(async () => ({ object: 'list', data: [] }));
    restored.endpoints.models = vi.fn(async () => ({ object: 'list', data: [] }));
    restored.endpoints.sessions = vi.fn(async () => ({ sessions: [] }));
    restored.endpoints.sessionStatus = vi.fn(async () => ({ sessions: [] }));
    restored.endpoints.selectedSession = vi.fn();
    restored.endpoints.createResponse = vi.fn();
    restored.endpoints.cancelResponse = vi.fn();
    vi.spyOn(restored, 'loadWidgetStatus').mockResolvedValue(undefined);
    await restored.bootstrap(true);
    expect(restored.startupDone.value).toBe(true);
    expect(restored.activeSession.value).toBeNull();
    expect(restored.composer.runtimeDraftId()).not.toBe(ownerId);
    view = render(
      <StoreContext.Provider value={restored}>
        <Composer />
      </StoreContext.Provider>,
    );
    expect(
      screen.getByRole('region', { name: 'Unconfirmed new conversations' }),
    ).toBeInTheDocument();
    expect(view.container.querySelector(`[data-pending-owner="${ownerId}"]`)).not.toBeNull();
    expect(screen.getByText('A new conversation that may already be running')).toBeInTheDocument();
    expect(screen.getByText('original-photo.png')).toBeInTheDocument();
    expect(screen.getByText(/Could not confirm whether this was sent/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Check status' }));
    await vi.advanceTimersByTimeAsync(300_000);
    expect(restored.endpoints.createResponse).not.toHaveBeenCalled();
    expect(restored.endpoints.selectedSession).not.toHaveBeenCalled();
    expect(restored.pendingIntents.value[ownerId][0].attachments?.[0]).toMatchObject({
      id: 'original-photo',
      blobRef: 'preserve-original-blob',
      uploadId: `upl_${'a'.repeat(43)}`,
    });
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss warning' }));
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss anyway' }));
    expect(confirm).not.toHaveBeenCalled();
    expect(restored.pendingIntents.value[ownerId][0].state).toBe('dismissed-unconfirmed');
    expect(
      screen.queryByRole('region', { name: 'Unconfirmed new conversations' }),
    ).not.toBeInTheDocument();
    expect(restored.endpoints.createResponse).not.toHaveBeenCalled();
    expect(restored.endpoints.cancelResponse).not.toHaveBeenCalled();
  } finally {
    view?.unmount();
    original.dispose();
    restored?.dispose();
    history.replaceState(null, '', route);
    vi.useRealTimers();
  }
});
