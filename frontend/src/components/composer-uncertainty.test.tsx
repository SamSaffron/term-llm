import { fireEvent, render, screen } from '@testing-library/preact';
import { expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig, testSession } from '../stores/store-test-fixtures';
import { Composer } from './Composer';
import { DEFAULT_ATTACHMENT_POLICY } from '../domain/attachments';

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
  const confirm = vi.spyOn(window, 'confirm').mockReturnValueOnce(false).mockReturnValueOnce(true);
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
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss warning' }));
    expect(confirm).toHaveBeenLastCalledWith(
      expect.stringContaining('dismissing does not cancel it'),
    );
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
    vi.spyOn(window, 'confirm').mockReturnValue(true);
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss warning' }));
    expect(restored.pendingIntents.value[ownerId][0].state).toBe('dismissed-unconfirmed');
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
