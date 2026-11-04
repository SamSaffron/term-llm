import { signal } from '@preact/signals';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { Session } from '../domain/types';
import { AppStoreServices } from './app-store-services';
import { ReviewStore, type ReviewStoreHost } from './review-store';
import { testConfig, testSession } from './store-test-fixtures';

const operations = vi.hoisted(() => ({ loads: 0 }));

vi.mock('./review-operations', async (importOriginal) => {
  operations.loads += 1;
  return importOriginal();
});

const makeHost = (session: Session | null = testSession()) => {
  const activeSession = signal<Session | null>(session);
  const activeSessionId = signal(session?.id || '');
  const host: ReviewStoreHost = {
    activeSession,
    activeSessionId,
    streaming: signal(false),
    closePlan: vi.fn(),
    restartStatusPoll: vi.fn(),
    send: vi.fn(async () => undefined),
    steer: vi.fn(async () => undefined),
    publishSessionChange: vi.fn(),
  };
  return { host, activeSession, activeSessionId };
};

const makeReview = () => {
  const services = new AppStoreServices(testConfig, localStorage);
  const view = makeHost();
  const store = new ReviewStore(services, view.host);
  return { services, host: view.host, view, store };
};

beforeEach(() => localStorage.clear());

describe('ReviewStore deferred operations', () => {
  it('keeps startup, queue, and comment refresh in the eager layer', async () => {
    expect(operations.loads).toBe(0);
    const { services, store } = makeReview();
    try {
      services.endpoints.diffComments = vi.fn(async () => ({ comments: [] }));
      store.queueDiffComment({
        path: 'main.go',
        side: 'new',
        line: 12,
        body: 'Keep this guard.',
        scope: 'uncommitted',
        context: 'if ready {',
      });
      expect(store.diff.value.comments).toHaveLength(1);
      await store.refreshDiffComments('s1');
      store.reloadCommentQueue();
      store.resizeDiff(500);
      expect(operations.loads).toBe(0);

      services.endpoints.fileChanges = vi.fn(async () => ({ file_changes: [], git: true }));
      const loading = store.loadDiff();
      const concurrent = store.loadDiff();
      await Promise.all([loading, concurrent]);

      expect(operations.loads).toBe(1);
      expect(store.diff.value.loading).toBe(false);
      expect(store.diff.value.comments).toHaveLength(1);
    } finally {
      services.dispose();
    }
  });

  it('does not import operations when a closed diff is toggled or a cached line is toggled', async () => {
    const before = operations.loads;
    const { services, store } = makeReview();
    try {
      store.diff.value = {
        ...store.diff.value,
        open: true,
        sessionId: 's1',
        files: [
          {
            path: 'main.go',
            status: 'modify',
            expanded: false,
            lines: [{ kind: 'add', content: 'ready', newLine: 1 }],
          },
        ],
      };
      await store.toggleDiff();
      expect(store.diff.value.open).toBe(false);
      await store.expandDiff(store.diff.value.files[0]);
      expect(store.diff.value.files[0].expanded).toBe(true);
      expect(operations.loads).toBe(before);
    } finally {
      services.dispose();
    }
  });
});
