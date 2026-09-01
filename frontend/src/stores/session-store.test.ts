import { beforeEach, describe, expect, it, vi } from 'vitest';
import { initialProjection } from '../domain/response';
import { AppStore } from './app-store';
import { testConfig, testSession } from './store-test-fixtures';

beforeEach(() => localStorage.clear());

describe('SessionStore', () => {
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
