import { signal } from '@preact/signals';
import { testSession } from './store-test-fixtures';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { readInjectedConfig } from '../app/config';
import {
  extensionHost,
  extensionRuntime,
  initializeExtensionRecovery,
  recoveryURL,
} from './extension-runtime';
import { updateSessionRoute } from '../platform/routing';
import { loadExtensions, watchExtensionActivation } from '../api/extensions';
import { AppStore } from './app-store';

beforeEach(() => {
  sessionStorage.clear();
  history.replaceState(null, '', '/ui/');
  extensionRuntime.safeMode.value = false;
  extensionRuntime.loaded.value = [];
  extensionRuntime.errors.value = [];
});
describe('extension host', () => {
  it('exposes context usage and subscribes only to context changes', () => {
    document.body.innerHTML = '<div id="root"></div><div id="extension-mount"></div>';
    const store = new AppStore(readInjectedConfig());
    const session = testSession({ id: 'session' });
    store.sessions.value = [session];
    store.activeSessionId.value = session.id;
    const host = extensionHost(store, 'context-meter');
    const snapshots: Array<ReturnType<typeof host.getContextUsage>> = [];
    const unsubscribe = host.onContextUsageChanged((usage) => snapshots.push(usage));

    expect(host.getContextUsage()).toBeNull();
    expect(snapshots).toEqual([null]);

    store.sessionStore.patch(session.id, { title: 'Unrelated change' });
    expect(snapshots).toHaveLength(1);

    const contextUsage = {
      usedTokens: 135_000,
      inputLimit: 372_000,
      cachedInputTokens: 51_900_000,
      estimated: true,
    };
    store.sessionStore.patch(session.id, { contextUsage });
    expect(host.getContextUsage()).toEqual(contextUsage);
    expect(Object.isFrozen(host.getContextUsage())).toBe(true);
    expect(snapshots).toEqual([null, contextUsage]);

    store.sessions.value = [...store.sessions.value, testSession({ id: 'other' })];
    store.activeSessionId.value = 'other';
    expect(host.getContextUsage()).toBeNull();
    expect(snapshots).toEqual([null, contextUsage, null]);

    unsubscribe();
    store.sessionStore.patch('other', {
      contextUsage: { ...contextUsage, usedTokens: 136_000 },
    });
    expect(snapshots).toHaveLength(3);
    store.dispose();
  });
});

describe('extension recovery', () => {
  it('bypasses all extension requests, not just script execution', async () => {
    history.replaceState(null, '', '/ui/?safe-mode=1');
    expect(initializeExtensionRecovery(readInjectedConfig())).toBe(true);
    const status = vi.fn();
    await loadExtensions({ endpoints: { extensionsStatus: status } } as unknown as AppStore);
    expect(status).not.toHaveBeenCalled();
  });
  it('persists safe mode through navigation and bootstrap', () => {
    history.replaceState(null, '', '/ui/?safe-mode=1');
    initializeExtensionRecovery(readInjectedConfig());
    updateSessionRoute('/ui', testSession({ id: 'session', number: 42 }));
    expect(location.search).toBe('?safe-mode=1');
    history.replaceState(null, '', '/ui/');
    expect(initializeExtensionRecovery(readInjectedConfig())).toBe(true);
    expect(recoveryURL()).toContain('safe-mode=1');
  });
  it('does not share the override between app base paths', () => {
    history.replaceState(null, '', '/ui/?safe-mode=1');
    initializeExtensionRecovery(readInjectedConfig());
    history.replaceState(null, '', '/other/');
    expect(initializeExtensionRecovery({ ...readInjectedConfig(), prefix: '/other' })).toBe(false);
  });
  it('keeps recovery in the URL when storage is unavailable', () => {
    history.replaceState(null, '', '/ui/?safe-mode=1');
    const get = vi.spyOn(sessionStorage, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    const set = vi.spyOn(sessionStorage, 'setItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    expect(initializeExtensionRecovery(readInjectedConfig())).toBe(true);
    updateSessionRoute('/ui', testSession({ id: 'session', number: 42 }));
    expect(location.search).toBe('?safe-mode=1');
    get.mockRestore();
    set.mockRestore();
  });
});

afterEach(() => {
  vi.useRealTimers();
});

function activationFixture() {
  extensionRuntime.generation.value = 'old';
  const status = { generation: 'new', activation_generation: 'new', disabled: false };
  const store = {
    config: { webRTC: false },
    endpoints: { extensionsStatus: vi.fn(async () => status) },
    authRequired: signal(false),
    runActive: signal(false),
    runs: signal({}),
    composer: {
      prompt: signal(''),
      attachments: signal<unknown[]>([]),
      sendPending: signal(false),
    },
    modal: signal(''),
  };
  const reload = vi.fn();
  return {
    store,
    status,
    reload,
    start: () => watchExtensionActivation(store as unknown as AppStore, reload),
  };
}

describe('agent-requested extension activation', () => {
  it('waits for responses, background runs, drafts, attachments and dialogs, then reloads once', async () => {
    vi.useFakeTimers();
    const f = activationFixture();
    f.store.runActive.value = true;
    const stop = f.start();
    await vi.advanceTimersByTimeAsync(3000);
    expect(f.reload).not.toHaveBeenCalled();
    f.store.runActive.value = false;
    f.store.runs.value = { background: { run: { status: 'streaming' } } };
    await vi.advanceTimersByTimeAsync(3000);
    expect(f.reload).not.toHaveBeenCalled();
    f.store.runs.value = {};
    f.store.composer.prompt.value = 'unsent';
    await vi.advanceTimersByTimeAsync(3000);
    expect(f.reload).not.toHaveBeenCalled();
    f.store.composer.prompt.value = '';
    f.store.composer.attachments.value = [{}];
    await vi.advanceTimersByTimeAsync(3000);
    expect(f.reload).not.toHaveBeenCalled();
    f.store.composer.attachments.value = [];
    f.store.modal.value = 'settings';
    await vi.advanceTimersByTimeAsync(3000);
    expect(f.reload).not.toHaveBeenCalled();
    f.store.modal.value = '';
    await vi.advanceTimersByTimeAsync(9000);
    expect(f.reload).toHaveBeenCalledTimes(1);
    stop();
  });
  it('does not mistake a rescan, stale activation, kill switch or current generation for a new activation', async () => {
    vi.useFakeTimers();
    const f = activationFixture();
    f.status.activation_generation = '';
    const stop = f.start();
    await vi.advanceTimersByTimeAsync(3000);
    f.status.activation_generation = 'stale';
    await vi.advanceTimersByTimeAsync(3000);
    f.status.activation_generation = 'new';
    f.status.disabled = true;
    await vi.advanceTimersByTimeAsync(3000);
    f.status.disabled = false;
    extensionRuntime.generation.value = 'new';
    await vi.advanceTimersByTimeAsync(3000);
    expect(f.reload).not.toHaveBeenCalled();
    stop();
  });
  it('never polls from safe mode or WebRTC-only browsers', async () => {
    vi.useFakeTimers();
    const f = activationFixture();
    extensionRuntime.safeMode.value = true;
    f.start()();
    extensionRuntime.safeMode.value = false;
    f.store.config.webRTC = true;
    f.start()();
    await vi.advanceTimersByTimeAsync(6000);
    expect(f.store.endpoints.extensionsStatus).not.toHaveBeenCalled();
  });
  it('retries unavailable servers and stops cleanly', async () => {
    vi.useFakeTimers();
    const f = activationFixture();
    f.store.endpoints.extensionsStatus.mockRejectedValueOnce(new Error('offline'));
    const stop = f.start();
    await vi.advanceTimersByTimeAsync(3000);
    expect(f.reload).not.toHaveBeenCalled();
    stop();
    await vi.advanceTimersByTimeAsync(6000);
    expect(f.store.endpoints.extensionsStatus).toHaveBeenCalledTimes(1);
  });
});
