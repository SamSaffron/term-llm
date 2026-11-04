import { signal } from '@preact/signals';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { Session } from '../domain/types';
import { AppStoreServices } from './app-store-services';
import { ReviewStore, type ReviewStoreHost } from './review-store';
import { testConfig, testSession } from './store-test-fixtures';

const chunk = vi.hoisted(() => ({ fail: true }));

vi.mock('./review-operations', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./review-operations')>();
  return {
    ReviewOperations: class extends actual.ReviewOperations {
      constructor(context: ConstructorParameters<typeof actual.ReviewOperations>[0]) {
        if (chunk.fail) throw new Error('review operations chunk unavailable');
        super(context);
      }
    },
  };
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
  return host;
};

const makeReview = () => {
  const services = new AppStoreServices(testConfig, localStorage);
  const store = new ReviewStore(services, makeHost());
  return { services, store };
};

beforeEach(() => {
  chunk.fail = true;
  localStorage.clear();
});

describe('ReviewStore operation chunk failures', () => {
  it('reports a failed operation load, then retries instead of staying broken', async () => {
    const { services, store } = makeReview();
    try {
      services.endpoints.fileChanges = vi.fn(async () => ({ file_changes: [], git: true }));
      await expect(store.loadDiff()).resolves.toBeUndefined();
      expect(store.diff.value.loading).toBe(false);
      expect(store.diff.value.error).toBe('review operations chunk unavailable');

      chunk.fail = false;
      await store.loadDiff();
      expect(store.diff.value.error).toBe('');
      expect(store.diff.value.loading).toBe(false);
    } finally {
      services.dispose();
    }
  });

  it('reports a failed operation load for a worktree diff without fetching', async () => {
    const { services, store } = makeReview();
    try {
      services.endpoints.worktreeDiff = vi.fn(async () => ({ diff: '' }));
      await expect(
        store.openWorktreeDiff('/worktrees/feature', 'feature'),
      ).resolves.toBeUndefined();
      expect(services.endpoints.worktreeDiff).not.toHaveBeenCalled();
      expect(store.diff.value.loading).toBe(false);
      expect(store.diff.value.error).toBe('review operations chunk unavailable');
    } finally {
      services.dispose();
    }
  });

  it('keeps the synchronous comment queue available when operations fail', async () => {
    const { services, store } = makeReview();
    try {
      store.queueDiffComment({
        path: 'main.go',
        side: 'new',
        line: 12,
        body: 'Keep this guard.',
        scope: 'uncommitted',
        context: 'if ready {',
      });
      expect(store.diff.value.comments).toHaveLength(1);
      expect(services.diagnostics.peek().queueValidationFailures).toBe(0);
    } finally {
      services.dispose();
    }
  });
});
