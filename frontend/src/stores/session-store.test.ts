import { beforeEach, describe, expect, it } from 'vitest';
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
});
