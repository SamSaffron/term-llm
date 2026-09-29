import { beforeEach, describe, expect, it, vi } from 'vitest';
import { APIError } from '../api/client';
import { initialProjection } from '../domain/response';
import { AppStore } from './app-store';
import { testConfig, testSession } from './store-test-fixtures';
import { compareSessionsByActivity } from './store-utils';

beforeEach(() => localStorage.clear());

describe('SessionStore', () => {
  it('routes legacy session requests by number while preserving their saved identity', async () => {
    const fetcher = vi.fn(async () => new Response('{}', { status: 200 }));
    vi.stubGlobal('fetch', fetcher);
    const store = new AppStore({ ...testConfig, prefix: '/node/jarvis' });
    try {
      const id = 'chat/session';
      const legacy = store.sessionStore.sessionFrom({ id, number: 16 });
      expect(legacy.id).toBe(id);
      await store.endpoints.sessionState(id);
      await store.endpoints.compact(id);
      await store.endpoints.sideQuestionState(id);
      await store.endpoints.createSessionShare(id, { scope: 'conversation' });
      expect(fetcher.mock.calls.map((call) => (call as unknown as [string])[0])).toEqual([
        '/node/jarvis/v1/sessions/16/state',
        '/node/jarvis/v1/sessions/16/runtime/compact',
        '/node/jarvis/api/sessions/16/side-question',
        '/node/jarvis/v1/sessions/16/shares',
      ]);
      const [, shareInit] = fetcher.mock.calls[3] as unknown as [string, RequestInit];
      expect((shareInit.headers as Headers).get('X-Term-LLM-Session-ID')).toBe(id);
      expect(store.api.url('/node/jarvis/v1/sessions/chat%2Fsession/skills?path=a%2Fb')).toBe(
        '/node/jarvis/v1/sessions/16/skills?path=a%2Fb',
      );
      expect(store.api.url('https://other.test/v1/sessions/chat%2Fsession/state')).toBe(
        'https://other.test/v1/sessions/chat%2Fsession/state',
      );
      expect(store.api.url('/v1/projects/chat%2Fsession/worktrees')).toBe(
        '/node/jarvis/v1/projects/chat%2Fsession/worktrees',
      );
    } finally {
      store.dispose();
    }
  });

  it('merges attention watermarks monotonically without clearing a newer marker', () => {
    const store = new AppStore(testConfig);
    try {
      const current = testSession({
        attentionStoreInstanceId: 'store-a',
        attentionSeq: 20,
        attentionResponseId: 'resp-new',
        attentionFinalRev: 8,
        seenThroughSeq: 10,
        attentionUnseen: true,
        attentionOutcome: 'failed',
      });
      const delayed = testSession({
        attentionStoreInstanceId: 'store-a',
        attentionSeq: 15,
        attentionResponseId: 'resp-old',
        attentionFinalRev: 5,
        seenThroughSeq: 15,
        attentionUnseen: false,
      });
      const merged = store.sessionStore.mergeSession(current, delayed);
      expect(merged).toMatchObject({
        attentionSeq: 20,
        attentionResponseId: 'resp-new',
        attentionFinalRev: 8,
        seenThroughSeq: 15,
        attentionUnseen: true,
        attentionOutcome: 'failed',
      });
    } finally {
      store.dispose();
    }
  });

  it('preserves marker metadata when an equal-sequence projection omits optional fields', () => {
    const store = new AppStore(testConfig);
    try {
      const current = testSession({
        attentionStoreInstanceId: 'store-a',
        attentionSeq: 20,
        attentionResponseId: 'resp-current',
        attentionFinalRev: 8,
        attentionOutcome: 'failed',
        attentionTerminalAt: 1234,
        attentionUnseen: true,
      });
      const sparse = testSession({
        attentionStoreInstanceId: 'store-a',
        attentionSeq: 20,
        attentionUnseen: true,
      });
      expect(store.sessionStore.mergeSession(current, sparse)).toMatchObject({
        attentionResponseId: 'resp-current',
        attentionFinalRev: 8,
        attentionOutcome: 'failed',
        attentionTerminalAt: 1234,
      });
    } finally {
      store.dispose();
    }
  });

  it('preserves omitted context usage and clears an authoritative null snapshot', () => {
    const store = new AppStore(testConfig);
    try {
      const usage = {
        usedTokens: 100,
        inputLimit: 1000,
        cachedInputTokens: 20,
        estimated: true,
      };
      const current = testSession({ contextUsage: usage });
      expect(store.sessionStore.mergeSession(current, testSession()).contextUsage).toEqual(usage);
      expect(
        store.sessionStore.mergeSession(current, testSession({ contextUsage: null })).contextUsage,
      ).toBeNull();
    } finally {
      store.dispose();
    }
  });

  it('resets attention watermarks when the node store identity changes', () => {
    const store = new AppStore(testConfig);
    try {
      const current = testSession({
        attentionStoreInstanceId: 'store-a',
        attentionSeq: 20,
        seenThroughSeq: 10,
        attentionUnseen: true,
      });
      const replacement = testSession({ attentionStoreInstanceId: 'store-b' });

      expect(store.sessionStore.mergeSession(current, replacement)).toMatchObject({
        attentionStoreInstanceId: 'store-b',
        attentionSeq: 0,
        seenThroughSeq: 0,
        attentionUnseen: false,
      });
    } finally {
      store.dispose();
    }
  });

  it('keeps running Hub agents distinct from agents with unseen terminal attention', async () => {
    const store = new AppStore({
      ...testConfig,
      hub: { url: '/hub/', nodeId: 'current', nodeBasePath: '/ui' },
    });
    try {
      store.endpoints.hubNodes = vi.fn(async () => ({
        nodes: [
          {
            id: 'active-only',
            name: 'Active only',
            status: { reachable: true },
            sessions: { active_count: 1, unseen_count: 0 },
            new_session_path: '/hub/node/active-only/?new=1',
          },
          {
            id: 'unseen-only',
            name: 'Unseen only',
            status: { reachable: true },
            sessions: { active_count: 0, unseen_count: 1 },
            new_session_path: '/hub/node/unseen-only/?new=1',
          },
          {
            id: 'active-and-unseen',
            name: 'Active and unseen',
            status: { reachable: true },
            sessions: { active_count: 1, unseen_count: 1 },
            new_session_path: '/hub/node/active-and-unseen/?new=1',
          },
          {
            id: 'idle',
            name: 'Idle',
            status: { reachable: true },
            sessions: { active_count: 0, unseen_count: 0 },
            new_session_path: '/hub/node/idle/?new=1',
          },
          {
            id: 'current',
            name: 'Current',
            status: { reachable: true },
            sessions: { active_count: 0, unseen_count: 1 },
            new_session_path: '/hub/node/current/?new=1',
          },
        ],
      }));

      await store.refreshHubAgents(true);

      expect(
        Object.fromEntries(store.hubAgents.value.map((agent) => [agent.id, agent])),
      ).toMatchObject({
        'active-only': { active: true, attention: false },
        'unseen-only': { active: false, attention: true },
        'active-and-unseen': { active: true, attention: true },
        idle: { active: false, attention: false },
        current: { active: false, attention: false },
      });
    } finally {
      store.dispose();
    }
  });

  it('is the command owner for catalog and selection state', () => {
    const store = new AppStore(testConfig);
    try {
      const first = testSession();
      store.sessionStore.prepend(first);
      store.sessionStore.patch(first.id, { title: 'Patched' });
      store.sessionStore.activate(store.sessionStore.find(first.id)!);

      expect(store.sessions.value).toEqual([
        expect.objectContaining({ id: 's1', title: 'Patched' }),
      ]);
      expect(store.activeSessionId.value).toBe('s1');
      expect(store.draftActive.value).toBe(false);

      store.sessionStore.activateDraft('project-1');
      expect(store.activeSessionId.value).toBe('');
      expect(store.activeProjectId.value).toBe('project-1');
      expect(store.draftActive.value).toBe(true);
    } finally {
      store.dispose();
    }
  });

  it('keeps transcript hydration out of the sidebar projection', () => {
    const store = new AppStore(testConfig);
    try {
      const session = testSession({ messageCount: 3 });
      store.sessions.value = [session];
      const sidebarSession = store.sidebarSessions.value[0];
      const updates: unknown[] = [];
      const unsubscribe = store.sidebarSessions.subscribe((value) => updates.push(value));

      store.sessionStore.update(session.id, (current) => ({
        ...current,
        messages: [{ id: 'm1', role: 'user', content: 'hydrated', created: 1 }],
        transcriptRev: 2,
        contextUsage: {
          usedTokens: 100,
          inputLimit: 1000,
          cachedInputTokens: 20,
          estimated: true,
        },
      }));

      expect(store.sidebarSessions.value[0]).toBe(sidebarSession);
      expect(updates).toHaveLength(1);

      store.sessionStore.patch(session.id, { title: 'Visible title change' });
      expect(store.sidebarSessions.value[0]).not.toBe(sidebarSession);
      expect(store.sidebarSessions.value[0].title).toBe('Visible title change');
      expect(updates).toHaveLength(2);
      unsubscribe();
    } finally {
      store.dispose();
    }
  });

  it('normalizes and merges sidebar payloads without replacing live session state', () => {
    const store = new AppStore(testConfig);
    try {
      store.sessionStore.prepend(
        testSession({
          activeRun: true,
          activeResponseId: 'r1',
          messages: [{ id: 'm1', role: 'user', content: 'live', created: 1 }],
        }),
      );
      store.runs.value = {
        s1: initialProjection({
          responseId: 'r1',
          sessionId: 's1',
          epoch: 1,
          status: 'streaming',
          lastSequence: 0,
          startedRev: 0,
          startedAt: 1,
          reconnects: 0,
        }),
      };

      store.sessionStore.applySidebar({
        sessions: [{ id: 's1', title: 'From server', messages: [] }],
      });

      expect(store.sessions.value[0]).toMatchObject({
        id: 's1',
        title: 'From server',
        activeRun: true,
        activeResponseId: 'r1',
      });
      expect(store.sessions.value[0].messages).toHaveLength(1);
    } finally {
      store.dispose();
    }
  });

  it('preserves sidebar signal identities when a refresh is semantically unchanged', () => {
    const store = new AppStore(testConfig);
    try {
      const payload = {
        groups: [
          {
            project: {
              id: 'project-1',
              name: 'Alpha',
              canonical_dir: '/tmp/alpha',
              available: true,
              git: true,
            },
            sessions: [
              {
                id: 's1',
                short_title: 'Stable conversation',
                origin: 'web',
                created_at: 1,
                last_message_at: 2,
                message_count: 3,
              },
            ],
            session_count: 1,
          },
        ],
      };
      store.sessionStore.applySidebar(payload);
      const sessions = store.sessions.value;
      const projects = store.projects.value;
      const session = sessions[0];
      const project = projects[0];

      store.sessionStore.applySidebar(payload);

      expect(store.sessions.value).toBe(sessions);
      expect(store.projects.value).toBe(projects);
      expect(store.sessions.value[0]).toBe(session);
      expect(store.projects.value[0]).toBe(project);
    } finally {
      store.dispose();
    }
  });

  it('retains the active flat-list session when a sidebar page omits it', () => {
    const store = new AppStore(testConfig);
    try {
      const active = testSession({
        id: 's_old',
        title: 'Old convo',
        messages: [{ id: 'm_old', role: 'user', content: 'Still here', created: 1 }],
      });
      store.sessionStore.prepend(active);
      store.sessionStore.activate(active);

      store.sessionStore.applySidebar({
        sessions: [{ id: 's_recent', title: 'Recent convo' }],
      });

      expect(store.activeSession.value?.id).toBe('s_old');
      expect(store.activeSession.value?.messages).toEqual([
        expect.objectContaining({ id: 'm_old', content: 'Still here' }),
      ]);
    } finally {
      store.dispose();
    }
  });

  it('retains the active project session when grouped sidebar data omits it', () => {
    const store = new AppStore(testConfig);
    try {
      store.projectsEnabled.value = true;
      const active = testSession({
        id: 's_old',
        title: 'Old project convo',
        projectId: 'project-old',
        messages: [{ id: 'm_old', role: 'user', content: 'Project transcript', created: 1 }],
      });
      store.sessionStore.prepend(active);
      store.sessionStore.activate(active);

      store.sessionStore.applySidebar({
        groups: [
          {
            project: { id: 'project-recent', name: 'Recent project' },
            sessions: [{ id: 's_recent', title: 'Recent convo' }],
          },
        ],
      });

      expect(store.activeSession.value?.id).toBe('s_old');
      expect(store.activeSession.value?.messages).toEqual([
        expect.objectContaining({ id: 'm_old', content: 'Project transcript' }),
      ]);
    } finally {
      store.dispose();
    }
  });

  it('drops an inactive local session when a complete sidebar snapshot omits it', () => {
    const store = new AppStore(testConfig);
    try {
      const old = testSession({ id: 's_old', title: 'Old convo' });
      const recent = testSession({ id: 's_recent', title: 'Recent convo', lastMessageAt: 2 });
      store.sessionStore.replace([recent, old]);
      store.sessionStore.activate(recent);

      store.sessionStore.applySidebar({
        sessions: [{ id: 's_recent', title: 'Recent convo' }],
      });

      expect(store.sessions.value.map((session) => session.id)).not.toContain('s_old');
    } finally {
      store.dispose();
    }
  });

  it('preserves the loaded tail when a partial first page omits older sessions', () => {
    const store = new AppStore(testConfig);
    try {
      const loaded = Array.from({ length: 21 }, (_, index) =>
        testSession({
          id: `s${index + 1}`,
          title: `Session ${index + 1}`,
          created: 21 - index,
          lastMessageAt: 21 - index,
        }),
      );
      store.sessionStore.replace(loaded);

      store.sessionStore.applySidebar({
        sessions: loaded.slice(0, 7).map((session) => ({
          id: session.id,
          short_title: session.title,
          created_at: session.created,
          last_message_at: session.lastMessageAt,
        })),
        next_cursor: 'older-page',
      });

      expect(store.sessions.value.map((session) => session.id)).toEqual(
        loaded.map((session) => session.id),
      );
    } finally {
      store.dispose();
    }
  });

  it('preserves loaded project tails only while the project page is incomplete', () => {
    const store = new AppStore(testConfig);
    try {
      store.projectsEnabled.value = true;
      const project = { id: 'p1', name: 'Alpha' };
      const sessions = ['s1', 's2', 's3'].map((id, index) => ({
        id,
        short_title: id,
        created_at: 3 - index,
        last_message_at: 3 - index,
      }));
      store.sessionStore.applySidebar({
        groups: [{ project, sessions, session_count: 3 }],
      });

      store.sessionStore.applySidebar({
        groups: [
          {
            project,
            sessions: sessions.slice(0, 1),
            session_count: 3,
            next_cursor: 'older-project-page',
          },
        ],
      });
      expect(store.sessions.value.map((session) => session.id)).toEqual(['s1', 's2', 's3']);
      expect(store.projects.value[0].sessions?.map((session) => session.id)).toEqual([
        's1',
        's2',
        's3',
      ]);

      store.sessionStore.applySidebar({
        groups: [{ project, sessions: sessions.slice(0, 1), session_count: 1 }],
      });
      expect(store.sessions.value.map((session) => session.id)).toEqual(['s1']);
    } finally {
      store.dispose();
    }
  });

  it('preserves catalog identity when an authoritative sidebar is unchanged', () => {
    const store = new AppStore(testConfig);
    try {
      const payload = {
        sessions: [
          {
            id: 's1',
            short_title: 'Stable',
            created_at: 1,
            last_message_at: 2,
            message_count: 3,
          },
        ],
      };
      store.sessionStore.applySidebar(payload);
      const sessions = store.sessions.value;
      const session = sessions[0];

      store.sessionStore.applySidebar(payload);

      expect(store.sessions.value).toBe(sessions);
      expect(store.sessions.value[0]).toBe(session);
    } finally {
      store.dispose();
    }
  });

  it('clears selection on archive and leaves it cleared through the next sidebar refresh', async () => {
    const store = new AppStore(testConfig);
    try {
      const active = testSession({ id: 's_old', title: 'Old convo' });
      store.sessionStore.prepend(active);
      store.sessionStore.activate(active);
      store.endpoints.patchSession = vi.fn(async () => ({}));

      await store.archiveSession(active);
      store.sessionStore.applySidebar({
        sessions: [{ id: 's_recent', title: 'Recent convo' }],
      });

      expect(store.activeSessionId.value).toBe('');
      expect(store.sessions.value.map((session) => session.id)).not.toContain('s_old');
    } finally {
      store.dispose();
    }
  });

  it('restores an active archived session outside the sidebar and refreshes its listing', async () => {
    const store = new AppStore(testConfig);
    try {
      const archived = testSession({ id: 's_archived', title: 'Archived convo', archived: true });
      store.sessionStore.activate(archived);
      expect(store.sessions.value).toEqual([]);
      store.endpoints.patchSession = vi.fn(async () => ({}));
      store.refreshSidebar = vi.fn(async () => undefined);

      await store.archiveSession(archived);

      expect(store.endpoints.patchSession).toHaveBeenCalledWith(archived.id, { archived: false });
      expect(store.activeSession.value?.archived).toBe(false);
      expect(store.refreshSidebar).toHaveBeenCalledOnce();
      await store.archiveSession(store.activeSession.value!);
      expect(store.endpoints.patchSession).toHaveBeenLastCalledWith(archived.id, {
        archived: true,
      });
    } finally {
      store.dispose();
    }
  });

  it('restores a listed archived session without resetting its paginated sidebar', async () => {
    const store = new AppStore(testConfig);
    try {
      const archived = testSession({ id: 's_archived', archived: true });
      store.showArchived.value = true;
      store.sessions.value = [archived];
      store.endpoints.patchSession = vi.fn(async () => ({}));
      store.refreshSidebar = vi.fn(async () => undefined);

      await store.archiveSession(archived);

      expect(store.sessions.value[0].archived).toBe(false);
      expect(store.refreshSidebar).not.toHaveBeenCalled();
    } finally {
      store.dispose();
    }
  });

  it('does not report a saved restore as failed when its sidebar refresh fails', async () => {
    const store = new AppStore(testConfig);
    try {
      const archived = testSession({ id: 's_archived', archived: true });
      store.sessionStore.activate(archived);
      store.endpoints.patchSession = vi.fn(async () => ({}));
      store.refreshSidebar = vi.fn(async () => {
        throw new Error('Sidebar unavailable');
      });

      await expect(store.archiveSession(archived)).resolves.toBeUndefined();
      expect(store.activeSession.value?.archived).toBe(false);
      expect(store.refreshSidebar).toHaveBeenCalledOnce();
    } finally {
      store.dispose();
    }
  });

  it('updates a transient session when pinning, even if the catalog refresh fails', async () => {
    const store = new AppStore(testConfig);
    try {
      const transient = testSession({ id: 's_transient', pinned: false });
      store.sessionStore.activate(transient);
      store.recentSessions.value = [transient];
      store.sessionStore.searchResults.value = [transient];
      store.endpoints.patchSession = vi.fn(async () => ({}));
      store.refreshSidebar = vi.fn(async () => {
        throw new Error('Sidebar unavailable');
      });

      await store.pinSession(store.activeSession.value!);
      expect(store.activeSession.value?.pinned).toBe(true);
      expect(store.recentSessions.value[0].pinned).toBe(true);
      expect(store.sessionStore.searchResults.value?.[0].pinned).toBe(true);
      await store.pinSession(store.activeSession.value!);
      expect(store.endpoints.patchSession).toHaveBeenNthCalledWith(2, transient.id, {
        pinned: false,
      });
    } finally {
      store.dispose();
    }
  });

  it('updates the active transient title after renaming', async () => {
    const store = new AppStore(testConfig);
    try {
      const transient = testSession({ id: 's_transient', title: 'Old title' });
      store.sessionStore.activate(transient);
      store.recentSessions.value = [transient];
      store.sessionStore.openRename(transient);
      store.endpoints.patchSession = vi.fn(async () => ({}));
      store.refreshSidebar = vi.fn(async () => {
        throw new Error('Sidebar unavailable');
      });

      await store.renameSession({ name: 'New title' });
      expect(store.activeSession.value).toMatchObject({ name: 'New title', title: 'New title' });
      expect(store.recentSessions.value[0].title).toBe('New title');
      expect(store.renameTarget.value).toBeNull();
      store.sessionStore.openRename(store.activeSession.value!);
      await store.renameSession({ name: '' });
      expect(store.activeSession.value).toMatchObject({ name: '', title: 'New chat' });
    } finally {
      store.dispose();
    }
  });

  it('removes the old active draft row when rekeying a session', () => {
    const store = new AppStore(testConfig);
    try {
      store.projectsEnabled.value = true;
      const draft = testSession({ id: 'draft_x', title: 'Draft convo' });
      store.sessionStore.prepend(draft);
      store.sessionStore.activate(draft);

      store.runEngine.rekeySession('draft_x', 's9', { id: 's9', title: 'Durable convo' });

      expect(store.sessions.value.map((session) => session.id)).not.toContain('draft_x');
      expect(store.recentSessions.value.map((session) => session.id)).toEqual(['s9']);
      expect(store.recentSessions.value[0].title).toBe('Durable convo');
      expect(store.activeSessionId.value).toBe('s9');
    } finally {
      store.dispose();
    }
  });
});

describe('pinned conversation order', () => {
  // Pins arrive in rank order, deliberately unlike their activity order.
  const pinnedEntries = () => [
    { id: 'a', short_title: 'A', pinned: true, pin_order: 1, created_at: 1, last_message_at: 10 },
    { id: 'b', short_title: 'B', pinned: true, pin_order: 2, created_at: 1, last_message_at: 50 },
    { id: 'c', short_title: 'C', pinned: true, pin_order: 3, created_at: 1, last_message_at: 90 },
    { id: 'u', short_title: 'U', created_at: 1, last_message_at: 100 },
  ];
  const recentIDs = (store: AppStore) => store.recentSessions.value.map((session) => session.id);
  const pinnedRanks = (store: AppStore) =>
    store.sessions.value
      .filter((session) => session.pinned)
      .map((session) => [session.id, session.pinOrder]);
  const pinnedStore = () => {
    const store = new AppStore(testConfig);
    store.sessionStore.applySidebar({ recent_sessions: pinnedEntries() });
    store.refreshSidebar = vi.fn(async () => undefined);
    return store;
  };

  it('orders pins by their saved rank while activity only reorders unpinned chats', () => {
    const store = pinnedStore();
    try {
      expect(recentIDs(store)).toEqual(['a', 'b', 'c', 'u']);
      expect(pinnedRanks(store)).toEqual([
        ['a', 1],
        ['b', 2],
        ['c', 3],
      ]);

      store.sessionStore.applySidebar({
        recent_sessions: [
          ...pinnedEntries().map((entry) =>
            entry.id === 'c' ? { ...entry, last_message_at: 500 } : entry,
          ),
          { id: 'v', short_title: 'V', created_at: 1, last_message_at: 400 },
        ],
      });

      expect(recentIDs(store)).toEqual(['a', 'b', 'c', 'v', 'u']);
      expect(store.sessions.value.map((session) => session.id)).toEqual(['a', 'b', 'c', 'v', 'u']);
    } finally {
      store.dispose();
    }
  });

  it('places pins without a saved rank after ranked pins and ignores ranks on unpinned chats', () => {
    const store = new AppStore(testConfig);
    try {
      expect(
        store.sessionStore.sessionFrom({ id: 'x', pinned: false, pin_order: 3 }),
      ).toHaveProperty('pinOrder', undefined);
      expect(
        store.sessionStore.sessionFrom({ id: 'y', pinned: true, pin_order: 'x' }).pinOrder,
      ).toBe(undefined);
      const sessions = [
        testSession({ id: 'recent-unranked', pinned: true, lastMessageAt: 90 }),
        testSession({ id: 'regular', lastMessageAt: 100 }),
        testSession({ id: 'second', pinned: true, pinOrder: 2, lastMessageAt: 10 }),
        testSession({ id: 'first', pinned: true, pinOrder: 1, lastMessageAt: 5 }),
        testSession({ id: 'old-unranked', pinned: true, lastMessageAt: 20 }),
      ];
      expect(sessions.sort(compareSessionsByActivity).map((session) => session.id)).toEqual([
        'first',
        'second',
        'recent-unranked',
        'old-unranked',
        'regular',
      ]);
    } finally {
      store.dispose();
    }
  });

  it('appends a new pin with the server rank and appends it again after unpinning', async () => {
    const store = pinnedStore();
    const sorted = () =>
      [...store.recentSessions.value].sort(compareSessionsByActivity).map((session) => session.id);
    try {
      store.endpoints.patchSession = vi.fn(async () => ({ id: 'u', pinned: true, pin_order: 4 }));
      await store.pinSession(store.sessions.value.find((session) => session.id === 'u')!);
      expect(store.endpoints.patchSession).toHaveBeenLastCalledWith('u', { pinned: true });
      expect(sorted()).toEqual(['a', 'b', 'c', 'u']);

      store.endpoints.patchSession = vi.fn(async () => ({ id: 'a', pinned: false }));
      await store.pinSession(store.sessions.value.find((session) => session.id === 'a')!);
      expect(store.sessions.value.find((session) => session.id === 'a')).toMatchObject({
        pinned: false,
        pinOrder: undefined,
      });

      store.endpoints.patchSession = vi.fn(async () => ({ id: 'a', pinned: true, pin_order: 5 }));
      await store.pinSession(store.sessions.value.find((session) => session.id === 'a')!);
      expect(sorted()).toEqual(['b', 'c', 'u', 'a']);
    } finally {
      store.dispose();
    }
  });

  it('shows a reorder at once, keeps it through stale snapshots, then applies saved ranks', async () => {
    const store = pinnedStore();
    try {
      let commit!: (value: Record<string, unknown>) => void;
      store.endpoints.reorderPinnedSessions = vi.fn(
        () => new Promise<Record<string, unknown>>((resolve) => (commit = resolve)),
      );

      const saving = store.reorderPinnedSessions(['c', 'a', 'b']);
      expect(recentIDs(store)).toEqual(['c', 'a', 'b', 'u']);
      await vi.waitFor(() =>
        expect(store.endpoints.reorderPinnedSessions).toHaveBeenCalledWith(['c', 'a', 'b']),
      );
      // A refresh that read the catalog before the save committed cannot undo it.
      store.sessionStore.applySidebar({ recent_sessions: pinnedEntries() });
      expect(recentIDs(store)).toEqual(['c', 'a', 'b', 'u']);

      commit({
        pinned: [
          { id: 'c', pin_order: 1 },
          { id: 'hidden', pin_order: 2 },
          { id: 'a', pin_order: 3 },
          { id: 'b', pin_order: 4 },
        ],
      });
      await saving;
      expect(pinnedRanks(store)).toEqual([
        ['c', 1],
        ['a', 3],
        ['b', 4],
      ]);
      expect(store.refreshSidebar).toHaveBeenCalledOnce();

      // Once saved, server snapshots are authoritative again.
      store.sessionStore.applySidebar({ recent_sessions: pinnedEntries() });
      expect(recentIDs(store)).toEqual(['a', 'b', 'c', 'u']);
    } finally {
      store.dispose();
    }
  });

  it('moves only the listed pins, leaving other pins in their positions', async () => {
    const store = new AppStore(testConfig);
    try {
      store.sessionStore.applySidebar({
        recent_sessions: [
          { id: 'a', pinned: true, pin_order: 1, created_at: 1, last_message_at: 1 },
          { id: 'x', pinned: true, pin_order: 2, created_at: 1, last_message_at: 1 },
          { id: 'b', pinned: true, pin_order: 3, created_at: 1, last_message_at: 1 },
        ],
      });
      store.refreshSidebar = vi.fn(async () => undefined);
      store.endpoints.reorderPinnedSessions = vi.fn(async () => ({}));

      await store.reorderPinnedSessions(['b', 'a']);

      expect(store.endpoints.reorderPinnedSessions).toHaveBeenCalledWith(['b', 'a']);
      expect(recentIDs(store)).toEqual(['b', 'x', 'a']);
      // Reordering into the current order saves nothing.
      await store.reorderPinnedSessions(['b', 'x', 'a']);
      expect(store.endpoints.reorderPinnedSessions).toHaveBeenCalledOnce();
    } finally {
      store.dispose();
    }
  });

  it('restores the previous order, refreshes, and explains a rejected save', async () => {
    const store = pinnedStore();
    try {
      store.endpoints.reorderPinnedSessions = vi.fn(async () => {
        throw new APIError('a listed session is no longer pinned', 409);
      });
      await expect(store.reorderPinnedSessions(['b', 'a', 'c'])).rejects.toThrow(
        'Pinned conversations changed elsewhere; showing the latest order.',
      );
      expect(recentIDs(store)).toEqual(['a', 'b', 'c', 'u']);
      expect(pinnedRanks(store)).toEqual([
        ['a', 1],
        ['b', 2],
        ['c', 3],
      ]);
      expect(store.refreshSidebar).toHaveBeenCalledOnce();

      store.endpoints.reorderPinnedSessions = vi.fn(async () => {
        throw new APIError('failed to save the pinned order', 500);
      });
      await expect(store.reorderPinnedSessions(['c', 'a', 'b'])).rejects.toThrow(
        'Couldn’t save the pinned order: failed to save the pinned order',
      );
      expect(recentIDs(store)).toEqual(['a', 'b', 'c', 'u']);
    } finally {
      store.dispose();
    }
  });

  it('saves only the newest of several quick reorders', async () => {
    const store = pinnedStore();
    try {
      const commits: Array<(value: Record<string, unknown>) => void> = [];
      store.endpoints.reorderPinnedSessions = vi.fn(
        () => new Promise<Record<string, unknown>>((resolve) => commits.push(resolve)),
      );
      const ranks = (ids: string[]) => ({
        pinned: ids.map((id, index) => ({ id, pin_order: index + 1 })),
      });

      const first = store.reorderPinnedSessions(['b', 'a', 'c']);
      await vi.waitFor(() => expect(commits).toHaveLength(1));
      const second = store.reorderPinnedSessions(['b', 'c', 'a']);
      const third = store.reorderPinnedSessions(['c', 'b', 'a']);
      expect(recentIDs(store)).toEqual(['c', 'b', 'a', 'u']);

      commits[0](ranks(['b', 'a', 'c']));
      await first;
      // The earlier save's ranks must not undo the newer, unsaved order.
      expect(recentIDs(store)).toEqual(['c', 'b', 'a', 'u']);
      await vi.waitFor(() => expect(commits).toHaveLength(2));
      expect(store.endpoints.reorderPinnedSessions).toHaveBeenLastCalledWith(['c', 'b', 'a']);

      commits[1](ranks(['c', 'b', 'a']));
      await Promise.all([second, third]);
      expect(recentIDs(store)).toEqual(['c', 'b', 'a', 'u']);
      expect(store.endpoints.reorderPinnedSessions).toHaveBeenCalledTimes(2);
      expect(store.refreshSidebar).toHaveBeenCalledOnce();
    } finally {
      store.dispose();
    }
  });
});

describe('project order', () => {
  // Groups arrive in saved rank order, deliberately unlike their activity:
  // Gamma has the newest conversation and Zulu is archived.
  const group = (id: string, name: string, rank: number, activity: number, archived = false) => ({
    project: {
      id,
      name,
      canonical_dir: `/p/${id}`,
      sort_order: rank,
      ...(archived ? { archived_at: '2026-01-01T00:00:00Z' } : {}),
    },
    session_count: 1,
    sessions: [
      {
        id: `${id}-chat`,
        short_title: `${name} chat`,
        created_at: 1,
        last_message_at: activity,
      },
    ],
  });
  const groups = (activity: Record<string, number> = {}) => [
    group('a', 'Alpha', 1, activity.a ?? 10),
    group('b', 'Beta', 2, activity.b ?? 20),
    group('c', 'Gamma', 3, activity.c ?? 90),
    group('z', 'Zulu', 4, activity.z ?? 5, true),
  ];
  const projectIDs = (store: AppStore) => store.projects.value.map((project) => project.id);
  const projectRanks = (store: AppStore) =>
    store.projects.value.map((project) => [project.id, project.sortOrder]);
  const projectStore = () => {
    const store = new AppStore(testConfig);
    store.sessionStore.applySidebar({ groups: groups(), recent_sessions: [] });
    store.refreshSidebar = vi.fn(async () => undefined);
    return store;
  };
  const ranks = (ids: string[]) => ({
    projects: ids.map((id, index) => ({ id, sort_order: index + 1 })),
  });

  it('keeps projects in their saved order, archived last, whatever their activity', () => {
    const store = projectStore();
    try {
      expect(projectIDs(store)).toEqual(['a', 'b', 'c', 'z']);
      expect(projectRanks(store)).toEqual([
        ['a', 1],
        ['b', 2],
        ['c', 3],
        ['z', 4],
      ]);
      // New replies never move a project, and archived projects list after
      // active ones even when a snapshot interleaves them.
      store.sessionStore.applySidebar({
        groups: [group('z', 'Zulu', 1, 999, true), ...groups({ a: 500, b: 400 }).slice(0, 3)],
        recent_sessions: [],
      });
      expect(projectIDs(store)).toEqual(['a', 'b', 'c', 'z']);
    } finally {
      store.dispose();
    }
  });

  it('shows a reorder at once, keeps it through stale snapshots, then applies saved ranks', async () => {
    const store = projectStore();
    try {
      let commit!: (value: Record<string, unknown>) => void;
      store.endpoints.reorderProjects = vi.fn(
        () => new Promise<Record<string, unknown>>((resolve) => (commit = resolve)),
      );

      const saving = store.reorderProjects(['c', 'a', 'b']);
      expect(projectIDs(store)).toEqual(['c', 'a', 'b', 'z']);
      await vi.waitFor(() =>
        expect(store.endpoints.reorderProjects).toHaveBeenCalledWith(['c', 'a', 'b']),
      );
      // A refresh that read the catalog before the save committed cannot undo it.
      store.sessionStore.applySidebar({ groups: groups({ a: 800 }), recent_sessions: [] });
      expect(projectIDs(store)).toEqual(['c', 'a', 'b', 'z']);

      commit({
        projects: [
          { id: 'c', sort_order: 1 },
          { id: 'a', sort_order: 2 },
          { id: 'z', sort_order: 3 },
          { id: 'b', sort_order: 4 },
        ],
      });
      await saving;
      expect(projectRanks(store)).toEqual([
        ['c', 1],
        ['a', 2],
        ['b', 4],
        ['z', 3],
      ]);
      expect(store.refreshSidebar).toHaveBeenCalledOnce();

      // Once saved, server snapshots are authoritative again.
      store.sessionStore.applySidebar({ groups: groups(), recent_sessions: [] });
      expect(projectIDs(store)).toEqual(['a', 'b', 'c', 'z']);
    } finally {
      store.dispose();
    }
  });

  it('moves only the listed projects and saves nothing for the current order', async () => {
    const store = projectStore();
    try {
      store.endpoints.reorderProjects = vi.fn(async (ids: string[]) => ranks(ids));

      await store.reorderProjects(['c', 'a']);
      expect(store.endpoints.reorderProjects).toHaveBeenCalledWith(['c', 'a']);
      expect(projectIDs(store)).toEqual(['c', 'b', 'a', 'z']);
      await store.reorderProjects(['c', 'b', 'a']);
      expect(store.endpoints.reorderProjects).toHaveBeenCalledOnce();
    } finally {
      store.dispose();
    }
  });

  it('restores the previous order, refreshes, and explains a rejected save', async () => {
    const store = projectStore();
    try {
      store.endpoints.reorderProjects = vi.fn(async () => {
        throw new APIError('a listed project was not found; refresh and try again', 404);
      });
      await expect(store.reorderProjects(['b', 'a', 'c'])).rejects.toThrow(
        'Projects changed elsewhere; showing the latest order.',
      );
      expect(projectIDs(store)).toEqual(['a', 'b', 'c', 'z']);
      expect(projectRanks(store).slice(0, 3)).toEqual([
        ['a', 1],
        ['b', 2],
        ['c', 3],
      ]);
      expect(store.refreshSidebar).toHaveBeenCalledOnce();

      store.endpoints.reorderProjects = vi.fn(async () => {
        throw new APIError('failed to save the project order', 500);
      });
      await expect(store.reorderProjects(['c', 'a', 'b'])).rejects.toThrow(
        'Couldn’t save the project order: failed to save the project order',
      );
      expect(projectIDs(store)).toEqual(['a', 'b', 'c', 'z']);
    } finally {
      store.dispose();
    }
  });

  it('saves queued active and archived project moves even when their ID sets differ', async () => {
    const store = projectStore();
    try {
      store.sessionStore.applySidebar({
        groups: [...groups(), group('y', 'Yankee', 5, 1, true)],
        recent_sessions: [],
      });
      const commits: Array<(value: Record<string, unknown>) => void> = [];
      store.endpoints.reorderProjects = vi.fn(
        () => new Promise<Record<string, unknown>>((resolve) => commits.push(resolve)),
      );

      const first = store.reorderProjects(['b', 'a', 'c']);
      await vi.waitFor(() => expect(commits).toHaveLength(1));
      const active = store.reorderProjects(['b', 'c', 'a']);
      const archived = store.reorderProjects(['y', 'z']);
      expect(projectIDs(store)).toEqual(['b', 'c', 'a', 'y', 'z']);

      commits[0](ranks(['b', 'a', 'c']));
      await first;
      await vi.waitFor(() => expect(commits).toHaveLength(2));
      expect(store.endpoints.reorderProjects).toHaveBeenNthCalledWith(2, ['b', 'c', 'a']);
      commits[1](ranks(['b', 'c', 'a']));
      await active;
      await vi.waitFor(() => expect(commits).toHaveLength(3));
      expect(store.endpoints.reorderProjects).toHaveBeenNthCalledWith(3, ['y', 'z']);
      commits[2]({
        projects: [
          { id: 'y', sort_order: 4 },
          { id: 'z', sort_order: 5 },
        ],
      });
      await archived;
      expect(projectIDs(store)).toEqual(['b', 'c', 'a', 'y', 'z']);
    } finally {
      store.dispose();
    }
  });

  it('reports a failed active move even if an archived move is queued next', async () => {
    const store = projectStore();
    try {
      store.sessionStore.applySidebar({
        groups: [...groups(), group('y', 'Yankee', 5, 1, true)],
        recent_sessions: [],
      });
      let fail!: (error: Error) => void;
      store.endpoints.reorderProjects = vi
        .fn()
        .mockImplementationOnce(() => new Promise((_, reject) => (fail = reject)))
        .mockImplementationOnce(async () => ({
          projects: [
            { id: 'a', sort_order: 1 },
            { id: 'b', sort_order: 2 },
            { id: 'c', sort_order: 3 },
            { id: 'y', sort_order: 4 },
            { id: 'z', sort_order: 5 },
          ],
        }));

      const active = store.reorderProjects(['b', 'a', 'c']);
      await vi.waitFor(() => expect(fail).toBeTypeOf('function'));
      const archived = store.reorderProjects(['y', 'z']);
      fail(new APIError('active move failed', 500));
      await expect(active).rejects.toThrow('active move failed');
      await archived;
      expect(store.endpoints.reorderProjects).toHaveBeenCalledTimes(2);
      expect(projectIDs(store)).toEqual(['a', 'b', 'c', 'y', 'z']);
    } finally {
      store.dispose();
    }
  });

  it('saves only the newest of several quick reorders', async () => {
    const store = projectStore();
    try {
      const commits: Array<(value: Record<string, unknown>) => void> = [];
      store.endpoints.reorderProjects = vi.fn(
        () => new Promise<Record<string, unknown>>((resolve) => commits.push(resolve)),
      );

      const first = store.reorderProjects(['b', 'a', 'c']);
      await vi.waitFor(() => expect(commits).toHaveLength(1));
      const second = store.reorderProjects(['b', 'c', 'a']);
      const third = store.reorderProjects(['c', 'b', 'a']);
      expect(projectIDs(store)).toEqual(['c', 'b', 'a', 'z']);

      commits[0](ranks(['b', 'a', 'c']));
      await first;
      // The earlier save's ranks must not undo the newer, unsaved order.
      expect(projectIDs(store)).toEqual(['c', 'b', 'a', 'z']);
      await vi.waitFor(() => expect(commits).toHaveLength(2));
      expect(store.endpoints.reorderProjects).toHaveBeenLastCalledWith(['c', 'b', 'a']);

      commits[1](ranks(['c', 'b', 'a']));
      await Promise.all([second, third]);
      expect(projectIDs(store)).toEqual(['c', 'b', 'a', 'z']);
      expect(store.endpoints.reorderProjects).toHaveBeenCalledTimes(2);
      expect(store.refreshSidebar).toHaveBeenCalledOnce();
    } finally {
      store.dispose();
    }
  });
});
