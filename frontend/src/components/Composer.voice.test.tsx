import { act, render, screen } from '@testing-library/preact';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig, testSession } from '../stores/store-test-fixtures';
import { VoiceOperation, type VoiceSnapshot } from '../platform/voice';
import { Composer } from './Composer';

beforeEach(() => localStorage.clear());
afterEach(() => vi.restoreAllMocks());

describe('thinking commands and voice controls', () => {
  it('keeps the live-call stop control available when dictation is busy', async () => {
    const busy: VoiceSnapshot = {
      phase: 'transcribing',
      capability: { supported: true, reason: '' },
      generation: 1,
      owner: '',
      ownerRevision: 0,
      durationMs: 0,
      bytes: 100,
    };
    vi.spyOn(VoiceOperation.prototype, 'snapshot', 'get').mockReturnValue(busy);
    vi.spyOn(VoiceOperation.prototype, 'subscribe').mockImplementation((listener) => {
      listener(busy);
      return () => {};
    });
    const store = new AppStore(testConfig);
    store.sessions.value = [testSession()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.startupDone.value = true;
    store.liveStore.enabled.value = true;
    store.liveStore.capability.value = { supported: true, reason: '' };
    const view = render(
      <StoreContext.Provider value={store}>
        <Composer />
      </StoreContext.Provider>,
    );
    expect(screen.getByRole('button', { name: 'Start live voice' })).toBeDisabled();
    // A live call can be started by another surface while dictation is pending.
    await act(() => {
      store.liveStore.liveId.value = 'live-one';
      store.liveStore.phase.value = 'listening';
    });
    expect(screen.getByRole('button', { name: 'Stop live voice' })).toBeEnabled();
    await act(() => {
      store.prompt.value = '/thinking expanded';
    });
    expect(screen.getByRole('button', { name: 'Send text to live voice' })).toBeDisabled();
    // Unmount inside act: Preact defers passive cleanup, and this test ends the
    // file, so a deferred cleanup could run after jsdom is torn down.
    await act(() => view.unmount());
    store.dispose();
  });
});
