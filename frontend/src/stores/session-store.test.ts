import { beforeEach, describe, expect, it, vi } from 'vitest';
import { initialProjection } from '../domain/response';
import { AppStore } from './app-store';
import { testConfig, testSession } from './store-test-fixtures';

beforeEach(() => localStorage.clear());

describe('SessionStore', () => {
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

  it('drops an inactive local session when a sidebar page omits it', () => {
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
      const draft = testSession({ id: 'draft_x', title: 'Draft convo' });
      store.sessionStore.prepend(draft);
      store.sessionStore.activate(draft);

      store.runEngine.rekeySession('draft_x', 's9', { id: 's9', title: 'Durable convo' });

      expect(store.sessions.value.map((session) => session.id)).not.toContain('draft_x');
      expect(store.activeSessionId.value).toBe('s9');
    } finally {
      store.dispose();
    }
  });
});
