import { effect } from '@preact/signals';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { AppConfig } from '../app/config';
import { APIError } from '../api/client';
import { MemoryBackend, PersistentCache } from '../platform/persistent-cache';
import { saveDraft } from '../platform/storage';
import { AppStore } from './app-store';

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
  cacheScope: 'abcdef0123456789',
  shellAuthorized: true,
};
type Payload = Record<string, unknown>;
const deferred = <T>() => {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((done, fail) => {
    resolve = done;
    reject = fail;
  });
  return { promise, resolve, reject };
};
const row = (id = 's1', rev = 7) => ({
  id,
  session_number: id === 's1' ? 42 : 43,
  title: `Chat ${id}`,
  name: `Chat ${id}`,
  mode: 'chat',
  origin: 'web',
  transcript_rev: rev,
});
const selected = (id = 's1', text = 'cached transcript', rev = 7): Payload => ({
  selected_session: row(id, rev),
  selected_transcript: {
    bodies: {
      rev,
      messages: [
        { id: 1, role: 'user', sequence: 1, parts: [{ type: 'text', text: 'question' }] },
        { id: 2, role: 'assistant', sequence: 2, parts: [{ type: 'text', text }] },
      ],
    },
  },
});
const capabilities = { projects: { enabled: false }, worktrees: { enabled: false } };
const providers = {
  data: [{ id: 'p', display_name: 'Provider', type: 'test', configured: true, default_model: 'm' }],
};
const models = { data: [{ id: 'm', display_name: 'Model', provider: 'p' }] };
const stores: AppStore[] = [];
const createStore = (cache?: PersistentCache, override: Partial<AppConfig> = {}) => {
  const store = new AppStore({ ...config, ...override }, localStorage, cache);
  stores.push(store);
  store.endpoints.capabilities = vi.fn(async () => capabilities);
  store.endpoints.providers = vi.fn(async () => providers);
  store.endpoints.models = vi.fn(async () => models);
  store.endpoints.sessions = vi.fn(async () => ({ sessions: [row(), row('s2')] }));
  store.endpoints.sidebar = vi.fn(async () => ({ recent_sessions: [row(), row('s2')] }));
  store.endpoints.sessionState = vi.fn(async () => ({}));
  store.endpoints.selectedSession = vi.fn(async (id) => selected(id));
  store.endpoints.skills = vi.fn(async () => ({ skills: [] }));
  store.endpoints.tree = vi.fn(async () => ({}));
  store.endpoints.widgetStatus = vi.fn(async () => ({ widgets: [] }));
  store.endpoints.verifyToken = vi.fn(async () => ({}));
  (store as unknown as { startStatusPoll(): void }).startStatusPoll = vi.fn();
  return store;
};
const pendingNetwork = (store: AppStore) => {
  const pending = {
    capabilities: deferred<Payload>(),
    providers: deferred<Payload>(),
    sidebar: deferred<Payload>(),
    state: deferred<Payload>(),
    selected: deferred<Payload>(),
    models: deferred<Payload>(),
    skills: deferred<Payload>(),
    tree: deferred<Payload>(),
  };
  store.endpoints.capabilities = vi.fn(() => pending.capabilities.promise);
  store.endpoints.providers = vi.fn(() => pending.providers.promise);
  store.endpoints.sessions = vi.fn(() => pending.sidebar.promise);
  store.endpoints.sidebar = vi.fn(() => pending.sidebar.promise);
  store.endpoints.sessionState = vi.fn(() => pending.state.promise);
  store.endpoints.selectedSession = vi.fn(() => pending.selected.promise);
  store.endpoints.models = vi.fn(() => pending.models.promise);
  store.endpoints.skills = vi.fn(() => pending.skills.promise);
  store.endpoints.tree = vi.fn(() => pending.tree.promise);
  return pending;
};
const resolveNetwork = (
  pending: ReturnType<typeof pendingNetwork>,
  text = 'fresh transcript',
  rev = 8,
) => {
  pending.capabilities.resolve(capabilities);
  pending.providers.resolve(providers);
  pending.sidebar.resolve({ sessions: [row('s1', rev), row('s2')] });
  pending.state.resolve({});
  pending.selected.resolve(selected('s1', text, rev));
  pending.models.resolve(models);
  pending.skills.resolve({ skills: [] });
  pending.tree.resolve({});
};
const seed = async (override: Partial<AppConfig> = {}) => {
  const backend = new MemoryBackend();
  const cache = new PersistentCache(backend);
  history.replaceState(null, '', '/ui/chat/s1');
  const first = createStore(cache, override);
  first.storage.setItem(first.keys.selectedProvider, 'p');
  first.selectedProvider.value = 'p';
  await first.bootstrap();
  expect(first.startupDone.value).toBe(true);
  expect(first.activeSession.value?.messages.at(-1)?.content).toBe('cached transcript');
  await vi.waitFor(() => expect(first.models.value[0]?.id).toBe('m'));
  first.services.workspaceCache.flush();
  await vi.waitFor(() => expect(backend.rows.size).toBe(5));
  first.dispose();
  return { backend, cache, first };
};
beforeEach(() => {
  localStorage.clear();
  history.replaceState(null, '', '/ui');
});
afterEach(() => {
  stores.splice(0).forEach((store) => store.dispose());
  vi.useRealTimers();
  vi.unstubAllGlobals();
  history.replaceState(null, '', '/ui');
});

describe('non-blocking startup discovery', () => {
  it('finishes with hydrated messages while models, skills and branches never resolve', async () => {
    const store = createStore(new PersistentCache(new MemoryBackend()));
    const never = deferred<Payload>();
    store.endpoints.models = vi.fn(() => never.promise);
    store.endpoints.skills = vi.fn(() => never.promise);
    store.endpoints.tree = vi.fn(() => never.promise);
    await store.bootstrap();
    expect(store.startupDone.value).toBe(true);
    expect(store.activeSession.value?.messages.at(-1)?.content).toBe('cached transcript');
    expect(store.endpoints.models).toHaveBeenCalledOnce();
    expect(store.endpoints.skills).toHaveBeenCalledOnce();
    expect(store.toasts.value).toEqual([]);
  });

  it('treats failed model discovery as optional without a toast', async () => {
    const store = createStore(new PersistentCache(new MemoryBackend()));
    store.endpoints.models = vi.fn().mockRejectedValue(new APIError('Model discovery failed', 500));
    await store.bootstrap();
    expect(store.startupDone.value).toBe(true);
    expect(store.toasts.value).toEqual([]);
  });
});

describe('cross-reload workspace startup', () => {
  it.each([
    { rev: 8, text: 'newer transcript' },
    { rev: 7, text: 'cached transcript' },
  ])(
    'shows non-live cached content before every network response, then verifies revision $rev',
    async ({ rev, text }) => {
      const { cache } = await seed();
      const store = createStore(cache);
      const pending = pendingNetwork(store);
      const mark = vi.spyOn(store.services, 'markStartup');
      const bootstrap = store.bootstrap();
      await vi.waitFor(() => expect(store.workspaceShown.value).toBe(true));
      expect(store.startupDone.value).toBe(false);
      expect(store.workspaceNotice.value).toBe('cached');
      expect(store.sendBlocked.value).toBe(true);
      expect(store.sidebarSessions.value.map((entry) => entry.id).sort()).toEqual(['s1', 's2']);
      expect(store.activeSession.value).toMatchObject({
        id: 's1',
        activeRun: false,
        activeResponseId: null,
      });
      expect(store.activeSession.value?.messages.at(-1)?.content).toBe('cached transcript');
      expect(store.selectionStore.unverifiedSessionId.value).toBe('s1');
      expect(store.startupMetrics.value.firstUsefulPaint).toEqual(expect.any(Number));
      expect(store.startupMetrics.value.authoritative).toBeUndefined();
      expect(store.startupMetrics.value.restoredFromCache).toBe(true);
      const send = vi.spyOn(store.runEngine, 'send');
      const steer = vi.spyOn(store.runEngine, 'steer');
      await store.send();
      await store.steer('not authorized yet');
      expect(send).not.toHaveBeenCalled();
      expect(steer).not.toHaveBeenCalled();
      resolveNetwork(pending, text, rev);
      await bootstrap;
      expect(store.startupDone.value).toBe(true);
      expect(store.workspaceNotice.value).toBe('');
      expect(store.selectionStore.unverifiedSessionId.value).toBe('');
      expect(store.activeSession.value?.messages.at(-1)?.content).toBe(text);
      expect(store.activeSession.value?.messageBodiesRev).toBe(rev);
      expect(mark.mock.calls.map(([name]) => name)).toEqual([
        'firstUsefulPaint',
        'authoritative',
        'actionsReady',
      ]);
      expect(JSON.stringify(store.startupMetrics.value)).not.toContain('transcript');
    },
  );

  it('does not show bearer-mode private content until capabilities authenticates', async () => {
    const { cache } = await seed({ shellAuthorized: false });
    const store = createStore(cache, { shellAuthorized: false });
    const pending = pendingNetwork(store);
    const bootstrap = store.bootstrap();
    // Wait for the actual cache read, not a delay: no authenticated response has landed.
    await store.services.workspaceCache.readStartup('s1');
    expect(store.workspaceShown.value).toBe(false);
    expect(store.activeSession.value).toBeNull();
    expect(store.sidebarSessions.value).toEqual([]);
    pending.capabilities.resolve(capabilities);
    await vi.waitFor(() => expect(store.workspaceShown.value).toBe(true));
    expect(store.workspaceNotice.value).toBe('cached');
    resolveNetwork(pending);
    await bootstrap;
    expect(store.startupDone.value).toBe(true);
  });

  it('purges the entire bearer scope after capabilities rejects authentication without revealing it', async () => {
    const { cache, backend } = await seed({ shellAuthorized: false });
    const store = createStore(cache, { shellAuthorized: false });
    const pending = pendingNetwork(store);
    const bootstrap = store.bootstrap();
    pending.capabilities.reject(new APIError('Unauthorized', 401));
    await bootstrap;
    expect(store.workspaceShown.value).toBe(false);
    expect(store.activeSession.value).toBeNull();
    expect(store.authRequired.value).toBe(true);
    expect(store.startupDone.value).toBe(false);
    await vi.waitFor(() => expect(backend.rows.size).toBe(0));
  });

  it.each(['404', '410', 'null'] as const)(
    'forgets a deleted restored conversation (%s) and clears its route',
    async (missing) => {
      const { cache } = await seed();
      const store = createStore(cache);
      const pending = pendingNetwork(store);
      const bootstrap = store.bootstrap();
      await vi.waitFor(() => expect(store.workspaceShown.value).toBe(true));
      if (missing === 'null') {
        pending.state.resolve({});
        pending.selected.resolve({ selected_session: null });
      } else pending.state.reject(new APIError('Gone', Number(missing)));
      // The sidebar can still race deletion and list the missing session.
      resolveNetwork(pending);
      await bootstrap;
      expect(store.startupDone.value).toBe(true);
      expect(store.activeSessionId.value).toBe('');
      expect(store.draftActive.value).toBe(true);
      expect(store.sessions.value.some((entry) => entry.id === 's1')).toBe(false);
      expect(store.sidebarSessions.value.some((entry) => entry.id === 's1')).toBe(false);
      expect(location.pathname).toBe('/ui/');
      expect(await store.services.workspaceCache.readSession('s1')).toBeNull();
      expect(await store.services.workspaceCache.readSession('42')).toBeNull();
    },
  );

  it('never restores an unrelated cached conversation for an uncached deep link', async () => {
    const { cache } = await seed();
    history.replaceState(null, '', '/ui/chat/other');
    const store = createStore(cache);
    const ids: string[] = [];
    const stop = effect(() => {
      ids.push(store.activeSessionId.value);
    });
    const pending = pendingNetwork(store);
    const bootstrap = store.bootstrap();
    await store.runtime.restoreCachedDiscovery();
    expect(store.workspaceShown.value).toBe(false);
    expect(ids).not.toContain('s1');
    pending.capabilities.resolve(capabilities);
    pending.providers.resolve(providers);
    pending.sidebar.resolve({ sessions: [row()] });
    pending.state.resolve({});
    pending.selected.resolve(selected('other', 'routed conversation'));
    pending.models.resolve(models);
    pending.skills.resolve({ skills: [] });
    pending.tree.resolve({});
    await bootstrap;
    expect(store.startupDone.value).toBe(true);
    expect(store.activeSessionId.value).toBe('other');
    expect(ids).not.toContain('s1');
    stop();
  });

  it.each(['new', 'draft'] as const)(
    'restores only the sidebar for %s, never the cached conversation',
    async (mode) => {
      const { cache, first } = await seed();
      history.replaceState(null, '', mode === 'new' ? '/ui?new=1' : '/ui');
      if (mode === 'draft') {
        localStorage.setItem(first.keys.draftSessionActive, 'draft:pending');
        saveDraft(localStorage, first.keys.draftMessages, {
          sessionId: 'draft:pending',
          content: 'saved unsent draft',
          updated: 1,
        });
      }
      const store = createStore(cache);
      const ids: string[] = [];
      const stop = effect(() => {
        ids.push(store.activeSessionId.value);
      });
      const pending = pendingNetwork(store);
      const bootstrap = store.bootstrap();
      await vi.waitFor(() => expect(store.workspaceShown.value).toBe(true));
      expect(store.sidebarSessions.value).toHaveLength(2);
      expect(store.draftActive.value).toBe(true);
      expect(store.activeSessionId.value).toBe('');
      if (mode === 'draft') expect(store.prompt.value).toBe('saved unsent draft');
      expect(store.endpoints.selectedSession).not.toHaveBeenCalled();
      resolveNetwork(pending);
      await bootstrap;
      expect(ids).not.toContain('s1');
      stop();
    },
  );

  it('honors a user sidebar selection while startup hydration is pending', async () => {
    const { cache } = await seed();
    const store = createStore(cache);
    const pending = pendingNetwork(store);
    const state2 = deferred<Payload>();
    const selected2 = deferred<Payload>();
    store.endpoints.sessionState = vi.fn((id) =>
      id === 's2' ? state2.promise : pending.state.promise,
    );
    store.endpoints.selectedSession = vi.fn((id) =>
      id === 's2' ? selected2.promise : pending.selected.promise,
    );
    const select = vi.spyOn(store.selectionStore, 'selectSession');
    const bootstrap = store.bootstrap();
    await vi.waitFor(() => expect(store.workspaceShown.value).toBe(true));
    const navigation = store.selectSession(
      store.sessions.value.find((entry) => entry.id === 's2')!,
    );
    expect(store.activeSessionId.value).toBe('s2');
    state2.resolve({});
    selected2.resolve(selected('s2', 'user choice'));
    await navigation;
    resolveNetwork(pending);
    await bootstrap;
    expect(store.activeSessionId.value).toBe('s2');
    expect(store.activeSession.value?.messages.at(-1)?.content).toBe('user choice');
    expect(select.mock.calls.map(([session]) => session.id)).toEqual(['s1', 's2']);
  });

  it('preserves text typed over cached content through hydration and reauthentication without reselecting', async () => {
    const { cache, backend } = await seed();
    const store = createStore(cache);
    const pending = pendingNetwork(store);
    const select = vi.spyOn(store.selectionStore, 'selectSession');
    const bootstrap = store.bootstrap();
    await vi.waitFor(() => expect(store.workspaceShown.value).toBe(true));
    store.prompt.value = 'typed while reconnecting';
    pending.state.resolve({});
    pending.selected.resolve(selected('s1', 'fresh transcript', 8));
    await vi.waitFor(() => expect(store.selectionStore.unverifiedSessionId.value).toBe(''));
    expect(store.prompt.value).toBe('typed while reconnecting');
    pending.capabilities.resolve(capabilities);
    pending.providers.resolve(providers);
    pending.sidebar.reject(new APIError('Token expired', 401));
    await bootstrap;
    expect(store.authRequired.value).toBe(true);
    await vi.waitFor(() => expect(backend.rows.size).toBe(0));
    expect(store.prompt.value).toBe('typed while reconnecting');
    store.endpoints.capabilities = vi.fn(async () => capabilities);
    store.endpoints.providers = vi.fn(async () => providers);
    store.endpoints.sessions = vi.fn(async () => ({ sessions: [row('s1', 8)] }));
    pending.models.resolve(models);
    pending.skills.resolve({ skills: [] });
    pending.tree.resolve({});
    await store.connect('replacement-token');
    expect(store.startupDone.value).toBe(true);
    expect(store.authRequired.value).toBe(false);
    expect(store.prompt.value).toBe('typed while reconnecting');
    expect(select).toHaveBeenCalledOnce();
  });

  it('does not resume from cached live flags; authoritative state starts exactly one response stream', async () => {
    const { cache, first, backend } = await seed();
    // Save a previously running hydrated row; persistence must strip its live ownership.
    first.services.workspaceCache.saveSession({
      ...first.sessions.value[0],
      activeRun: true,
      activeResponseId: 'r1',
    });
    first.services.workspaceCache.saveSidebar({
      projectsEnabled: false,
      worktreesEnabled: false,
      showArchived: false,
      payload: { sessions: [{ ...row(), active_run: true, active_response_id: 'r1' }] },
    });
    first.services.workspaceCache.flush();
    await vi.waitFor(() =>
      expect(
        JSON.parse(
          [...backend.rows.values()].find((entry) => entry.key.endsWith('\u0000sidebar'))!.json,
        ).payload.sessions,
      ).toHaveLength(1),
    );
    const store = createStore(cache);
    const pending = pendingNetwork(store);
    const snapshot = deferred<Payload>();
    store.endpoints.response = vi.fn(() => snapshot.promise);
    const stream = deferred<Response>();
    store.endpoints.responseEvents = vi.fn(() => stream.promise);
    const resume = vi.spyOn(store.runEngine, 'resumeResponse');
    const bootstrap = store.bootstrap();
    await vi.waitFor(() => expect(store.workspaceShown.value).toBe(true));
    expect(resume).not.toHaveBeenCalled();
    expect(store.endpoints.response).not.toHaveBeenCalled();
    pending.state.resolve({ active_run: true, active_response_id: 'r1' });
    resolveNetwork(pending);
    await bootstrap;
    expect(resume).toHaveBeenCalledWith('s1', 'r1');
    expect(store.endpoints.response).toHaveBeenCalledOnce();
    snapshot.resolve({
      id: 'r1',
      session_id: 's1',
      status: 'in_progress',
      run_epoch: 1,
      last_sequence_number: 4,
      recovery: { sequence_number: 4, events: [] },
    });
    await vi.waitFor(() => expect(store.endpoints.responseEvents).toHaveBeenCalledOnce());
    expect(store.endpoints.responseEvents).toHaveBeenCalledWith('r1', 4, expect.any(AbortSignal));
  });

  it('starts normally with corrupt cache JSON', async () => {
    const { cache, backend } = await seed();
    for (const record of backend.rows.values()) record.json = '{broken';
    const store = createStore(cache);
    await store.bootstrap();
    expect(store.startupDone.value).toBe(true);
    expect(store.startupMetrics.value.restoredFromCache).toBe(false);
    expect(store.activeSession.value?.messages.at(-1)?.content).toBe('cached transcript');
  });

  it('starts normally when every persistent operation throws, recording storage diagnostics', async () => {
    const backend = new MemoryBackend();
    for (const method of [
      'get',
      'put',
      'touch',
      'delete',
      'list',
      'deleteNamespacePrefix',
      'clear',
    ] as const)
      vi.spyOn(backend, method).mockRejectedValue(new Error('Storage disabled'));
    const cache = new PersistentCache(backend, () =>
      store.services.bumpDiagnostic('storageFailures'),
    );
    const store = createStore(cache);
    await store.bootstrap();
    expect(store.startupDone.value).toBe(true);
    expect(store.diagnostics.value.storageFailures).toBeGreaterThan(0);
    expect(store.activeSession.value?.messages.at(-1)?.content).toBe('cached transcript');
    store.services.workspaceCache.flush();
    await vi.waitFor(() => expect(backend.list).toHaveBeenCalled());
  });
});

describe('discovery cache reconciliation', () => {
  it('shows cached providers and the selected model catalog before discovery responds, avoiding a fresh catalog refetch', async () => {
    const { cache } = await seed();
    const store = createStore(cache);
    const pending = pendingNetwork(store);
    const bootstrap = store.bootstrap();
    await vi.waitFor(() => expect(store.models.value[0]?.id).toBe('m'));
    expect(store.providers.value[0]).toMatchObject({ id: 'p', name: 'Provider' });
    expect(store.startupMetrics.value.discoveryCacheHits).toBe(2);
    expect(store.endpoints.providers).not.toHaveBeenCalled();
    resolveNetwork(pending);
    await bootstrap;
    expect(store.endpoints.models).not.toHaveBeenCalled();
  });

  it('does not prune a saved provider on a cached list, but does on the authoritative list', async () => {
    const { cache, first } = await seed();
    localStorage.setItem(first.keys.selectedProvider, 'missing-from-cache');
    const store = createStore(cache);
    const pending = pendingNetwork(store);
    const bootstrap = store.bootstrap();
    await vi.waitFor(() => expect(store.workspaceShown.value).toBe(true));
    expect(store.selectedProvider.value).toBe('missing-from-cache');
    expect(localStorage.getItem(store.keys.selectedProvider)).toBe('missing-from-cache');
    resolveNetwork(pending);
    await bootstrap;
    expect(store.selectedProvider.value).toBe('');
    expect(localStorage.getItem(store.keys.selectedProvider)).toBeNull();
  });

  it('invalidates a fresh cached model catalog when the authoritative provider fingerprint changes', async () => {
    const { cache } = await seed();
    const store = createStore(cache);
    const pending = pendingNetwork(store);
    const bootstrap = store.bootstrap();
    await vi.waitFor(() => expect(store.models.value[0]?.id).toBe('m'));
    pending.capabilities.resolve(capabilities);
    pending.providers.resolve({ data: [{ ...providers.data[0], default_model: 'new-model' }] });
    pending.sidebar.resolve({ sessions: [row()] });
    pending.state.resolve({});
    pending.selected.resolve(selected());
    await vi.waitFor(() => expect(store.endpoints.models).toHaveBeenCalledOnce());
    expect(store.models.value.some((entry) => entry.id === 'm')).toBe(false);
    pending.models.resolve({ data: [{ id: 'new-model', provider: 'p' }] });
    await bootstrap;
    await vi.waitFor(() => expect(store.models.value[0]?.id).toBe('new-model'));
  });

  it('deduplicates concurrent model loads for the same provider', async () => {
    const store = createStore(new PersistentCache(new MemoryBackend()));
    const pending = deferred<Payload>();
    store.endpoints.models = vi.fn(() => pending.promise);
    const first = store.loadModels('p');
    const second = store.loadModels('p');
    expect(store.endpoints.models).toHaveBeenCalledOnce();
    pending.resolve(models);
    await Promise.all([first, second]);
    expect(store.models.value[0]?.id).toBe('m');
  });
});

describe('late event-feed preparation', () => {
  it('does not block startup on the head start and catches up the sidebar when the cursor arrives', async () => {
    vi.useFakeTimers();
    const store = createStore(new PersistentCache(new MemoryBackend()), { cacheScope: '' });
    store.endpoints.capabilities = vi.fn(async () => ({
      ...capabilities,
      event_feed: { version: 1, sse: true, long_poll: true },
    }));
    const ready = deferred<void>();
    vi.spyOn(store.serverEventCoordinator, 'prepare').mockImplementation(() => ready.promise);
    vi.spyOn(store.serverEventCoordinator, 'whenPrepared').mockImplementation(() => ready.promise);
    vi.spyOn(store.serverEventCoordinator, 'preparedSettled', 'get').mockReturnValue(false);
    vi.spyOn(store.serverEventCoordinator, 'isHealthy').mockReturnValue(true);
    const reconcile = vi
      .spyOn(
        store as unknown as {
          reconcile(reason: string, options: { authoritative: boolean }): Promise<void>;
        },
        'reconcile',
      )
      .mockResolvedValue(undefined);
    const bootstrap = store.bootstrap();
    await vi.advanceTimersByTimeAsync(149);
    expect(store.endpoints.sessions).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    await bootstrap;
    expect(store.startupDone.value).toBe(true);
    expect(store.endpoints.sessions).toHaveBeenCalledOnce();
    ready.resolve();
    await vi.advanceTimersByTimeAsync(0);
    expect(store.endpoints.sessions).toHaveBeenCalledTimes(2);
    expect(reconcile).toHaveBeenCalledWith('event-feed-ready', { authoritative: true });
  });
});

describe('review regressions', () => {
  it('keeps restored bodies unverified across navigation until a load succeeds', async () => {
    const { cache } = await seed();
    const store = createStore(cache);
    store.endpoints.selectedSession = vi.fn(async (id: string) => {
      if (id === 's1') throw new APIError('Temporarily unavailable', 500);
      return selected(id, 'other transcript', 1);
    });
    await store.bootstrap();
    expect(store.startupDone.value).toBe(true);
    expect(store.workspaceNotice.value).toBe('unverified');
    await store.selectSession(store.sessions.value.find((entry) => entry.id === 's2')!);
    expect(store.workspaceNotice.value).toBe('');
    await store.selectSession(store.sessions.value.find((entry) => entry.id === 's1')!);
    // Failed again: the cached bodies on screen are still last-known.
    expect(store.selectionStore.unverifiedSessionId.value).toBe('s1');
    expect(store.workspaceNotice.value).toBe('unverified');
    store.endpoints.selectedSession = vi.fn(async (id: string) => selected(id, 'fresh', 9));
    await store.loadSession('s1');
    expect(store.workspaceNotice.value).toBe('');
    expect(store.activeSession.value?.messages.at(-1)?.content).toBe('fresh');
  });

  it('drops a restored conversation that stops resolving when retried after startup', async () => {
    const { cache, backend } = await seed();
    const store = createStore(cache);
    store.endpoints.selectedSession = vi.fn(async () => {
      throw new APIError('Temporarily unavailable', 500);
    });
    await store.bootstrap();
    expect(store.workspaceNotice.value).toBe('unverified');
    store.endpoints.selectedSession = vi.fn(async () => ({ selected_session: null }));
    await store.loadSession('s1');
    expect(store.draftActive.value).toBe(true);
    expect(store.sessions.value.some((entry) => entry.id === 's1')).toBe(false);
    await vi.waitFor(() =>
      expect([...backend.rows.keys()].some((key) => key.endsWith('\u0000s1'))).toBe(false),
    );
  });

  it('never reinstalls a cached catalog after providers change during the cache read', async () => {
    const { cache, backend } = await seed();
    const store = createStore(cache);
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const get = backend.get.bind(backend);
    vi.spyOn(backend, 'get').mockImplementation(async (key: string) => {
      if (key.endsWith('\u0000models:p')) await gate;
      return get(key);
    });
    const restoring = store.runtime.restoreCachedDiscovery();
    await vi.waitFor(() => expect(store.providers.value.map((entry) => entry.id)).toEqual(['p']));
    store.runtime.applyProviders({ data: [{ ...providers.data[0], default_model: 'changed' }] });
    release();
    await restoring;
    expect(store.runtime.modelCatalogs.value.p).toBeUndefined();
  });

  it('purges the private cache when the stored credential is replaced or removed', async () => {
    const { cache, backend } = await seed();
    const store = createStore(cache);
    store.services.setToken('first');
    expect(backend.rows.size).toBe(5);
    store.services.setToken('');
    await vi.waitFor(() => expect(backend.rows.size).toBe(0));
  });
});
