import { signal } from '@preact/signals';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { reviewAnchorFingerprint } from '../domain/review-policy';
import type { Session } from '../domain/types';
import { readDiffCommentQueue } from '../platform/storage';
import { AppStoreServices } from './app-store-services';
import { ReviewStore, type ReviewStoreHost } from './review-store';
import { testConfig, testSession } from './store-test-fixtures';

const deferred = <T>() => {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((done, fail) => {
    resolve = done;
    reject = fail;
  });
  return { promise, resolve, reject };
};

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
  services.endpoints.diffComments = vi.fn(async () => ({ comments: [] }));
  return { services, host: view.host, view, store };
};

beforeEach(() => localStorage.clear());

describe('ReviewStore eager comment queue', () => {
  it('queues, edits, re-anchors, and removes comments synchronously', () => {
    const { services, host, store } = makeReview();
    try {
      store.queueDiffComment({
        path: 'main.go',
        side: 'new',
        line: 12,
        body: 'Keep this guard.',
        scope: 'LAST-TURN',
        context: 'if ready {',
        contextBefore: ['prepare()'],
        contextAfter: ['run()'],
      });

      expect(store.diff.value.comments).toHaveLength(1);
      const queued = store.diff.value.comments[0];
      expect(queued).toMatchObject({
        path: 'main.go',
        line: 12,
        sessionId: 's1',
        scope: 'last_turn',
        state: 'fresh',
      });
      expect(queued.anchorFingerprint).toBe(reviewAnchorFingerprint(queued));
      expect(host.publishSessionChange).toHaveBeenCalledWith('review-comment-changed', 's1');

      const queuedId = queued.id || '';
      store.editDiffComment(queuedId, 'Updated instruction.');
      expect(store.diff.value.comments[0].body).toBe('Updated instruction.');
      expect(readDiffCommentQueue(localStorage, services.keys.diffCommentQueue)[0].body).toBe(
        'Updated instruction.',
      );

      store.reanchorDiffComment(queuedId, {
        path: 'other.go',
        side: 'old',
        line: 3,
        context: 'old line',
        fileChangeSeq: 4,
        scope: 'uncommitted',
      });
      expect(store.diff.value.comments[0]).toMatchObject({
        id: queuedId,
        path: 'other.go',
        line: 3,
        side: 'old',
        state: 'fresh',
        scope: 'uncommitted',
      });

      store.removeDiffComment(queuedId);
      expect(store.diff.value.comments).toEqual([]);
      expect(readDiffCommentQueue(localStorage, services.keys.diffCommentQueue)).toEqual([]);
    } finally {
      services.dispose();
    }
  });

  it('rejects an invalid comment immediately and keeps the queue unchanged', () => {
    const { services, store } = makeReview();
    try {
      store.queueDiffComment({
        path: 'main.go',
        side: 'new',
        line: 12,
        body: 'Keep this guard.',
        scope: 'uncommitted',
        context: 'if ready {',
        contextBefore: ['1', '2', '3', '4', '5'],
      });

      expect(store.diff.value.comments).toEqual([]);
      expect(services.diagnostics.peek().queueValidationFailures).toBe(1);
      expect(services.toasts.peek().at(-1)).toMatchObject({ kind: 'error' });
    } finally {
      services.dispose();
    }
  });

  it('restores a persisted queue without loading diff operations', () => {
    const first = makeReview();
    first.store.queueDiffComment({
      path: 'queued.go',
      side: 'new',
      line: 3,
      body: 'Keep this queued.',
      scope: 'staged',
      context: 'queued line',
    });
    const id = first.store.diff.value.comments[0].id;
    first.services.dispose();

    const second = makeReview();
    try {
      expect(second.store.diff.value.comments).toEqual([
        expect.objectContaining({ id, path: 'queued.go', body: 'Keep this queued.' }),
      ]);
      second.store.discardDiffComments('s1');
      expect(second.store.diff.value.comments).toEqual([]);
      expect(readDiffCommentQueue(localStorage, second.services.keys.diffCommentQueue)).toEqual([]);
    } finally {
      second.services.dispose();
    }
  });
});

describe('ReviewStore deferred diff coordination', () => {
  it('keeps the newest of two overlapping loads', async () => {
    const { services, store } = makeReview();
    try {
      const older = deferred<Record<string, unknown>>();
      const newer = deferred<Record<string, unknown>>();
      services.endpoints.fileChanges = vi
        .fn()
        .mockReturnValueOnce(older.promise)
        .mockReturnValueOnce(newer.promise);

      const first = store.loadDiff();
      await vi.waitFor(() => expect(services.endpoints.fileChanges).toHaveBeenCalledTimes(1));
      const second = store.loadDiff();
      await vi.waitFor(() => expect(services.endpoints.fileChanges).toHaveBeenCalledTimes(2));

      newer.resolve({
        git: true,
        file_changes: [{ path: 'new.go', kind: 'modify', adds: 1, dels: 0 }],
      });
      await second;
      older.resolve({
        git: true,
        file_changes: [{ path: 'old.go', kind: 'modify', adds: 1, dels: 0 }],
      });
      await first;

      expect(store.diff.value.files.map((file) => file.path)).toEqual(['new.go']);
      expect(store.diff.value.loading).toBe(false);
    } finally {
      services.dispose();
    }
  });

  it('drops a load result after the active session changes', async () => {
    const { services, view, store } = makeReview();
    try {
      const pending = deferred<Record<string, unknown>>();
      services.endpoints.fileChanges = vi.fn(() => pending.promise);
      const loading = store.loadDiff();
      await vi.waitFor(() => expect(services.endpoints.fileChanges).toHaveBeenCalledTimes(1));

      view.activeSessionId.value = 's2';
      view.activeSession.value = testSession({ id: 's2' });
      pending.resolve({
        git: true,
        file_changes: [{ path: 'private.go', kind: 'modify' }],
      });
      await loading;

      expect(store.diff.value.files).toEqual([]);
    } finally {
      services.dispose();
    }
  });

  it('drops an in-flight load after the store is disposed', async () => {
    const { services, store } = makeReview();
    try {
      const pending = deferred<Record<string, unknown>>();
      services.endpoints.fileChanges = vi.fn(() => pending.promise);
      const loading = store.loadDiff();
      await vi.waitFor(() => expect(services.endpoints.fileChanges).toHaveBeenCalledTimes(1));

      services.dispose();
      pending.resolve({
        git: true,
        file_changes: [{ path: 'late.go', kind: 'modify' }],
      });
      await loading;

      expect(store.diff.value.files).toEqual([]);
    } finally {
      services.dispose();
    }
  });
});
