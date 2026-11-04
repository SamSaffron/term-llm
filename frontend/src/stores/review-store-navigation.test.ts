import { signal } from '@preact/signals';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { Session } from '../domain/types';
import { AppStoreServices } from './app-store-services';
import { ReviewStore, type ReviewStoreHost } from './review-store';
import type { ReviewOperationsApi } from './review-types';
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
  return { services, view, store };
};

/** Replace the deferred import resolution with a controllable promise. */
const gateOperations = (store: ReviewStore) => {
  const gate = deferred<ReviewOperationsApi>();
  (store as unknown as { operations: () => Promise<ReviewOperationsApi> }).operations = () =>
    gate.promise;
  return gate;
};

const fakeOperations = (): ReviewOperationsApi => ({
  loadDiff: vi.fn(async () => undefined),
  openWorktreeDiff: vi.fn(async () => undefined),
  expandDiff: vi.fn(async () => undefined),
  sendDiffComments: vi.fn(async () => undefined),
  sendDiffComment: vi.fn(async () => undefined),
});

const navigate = (view: ReturnType<typeof makeHost>, sessionId: string) => {
  view.activeSessionId.value = sessionId;
  view.activeSession.value = testSession({ id: sessionId });
};

beforeEach(() => localStorage.clear());

describe('ReviewStore navigation while operations load', () => {
  it('does not fetch a diff load for a session that changed during import', async () => {
    const { services, view, store } = makeReview();
    try {
      services.endpoints.fileChanges = vi.fn(async () => ({ file_changes: [] }));
      const gate = gateOperations(store);
      const pending = store.loadDiff();
      expect(store.diff.value).toMatchObject({ sessionId: 's1', loading: true });

      navigate(view, 's2');
      const operations = fakeOperations();
      gate.resolve(operations);
      await pending;

      expect(services.endpoints.fileChanges).not.toHaveBeenCalled();
      expect(operations.loadDiff).not.toHaveBeenCalled();
    } finally {
      services.dispose();
    }
  });

  it('does not fetch an expansion for a session that changed during import', async () => {
    const { services, view, store } = makeReview();
    try {
      store.diff.value = {
        ...store.diff.value,
        open: true,
        sessionId: 's1',
        files: [{ path: 'main.go', status: 'modify', context: 3 }],
      };
      services.endpoints.fileDiff = vi.fn(async () => ({ hunks: [] }));
      const gate = gateOperations(store);
      const pending = store.expandDiff(store.diff.value.files[0], 12);
      expect(store.diff.value.files[0]).toMatchObject({ expanded: true, loading: true });

      navigate(view, 's2');
      const operations = fakeOperations();
      gate.resolve(operations);
      await pending;

      expect(services.endpoints.fileDiff).not.toHaveBeenCalled();
      expect(operations.expandDiff).not.toHaveBeenCalled();
    } finally {
      services.dispose();
    }
  });

  it('does not send an immediate comment for a session that changed during import', async () => {
    const { services, view, store } = makeReview();
    try {
      const gate = gateOperations(store);
      const pending = store.sendDiffComment({
        path: 'main.go',
        side: 'new',
        line: 12,
        body: 'Keep this guard.',
        scope: 'uncommitted',
        context: 'if ready {',
      });

      navigate(view, 's2');
      const operations = fakeOperations();
      gate.resolve(operations);
      await pending;

      expect(operations.sendDiffComment).not.toHaveBeenCalled();
      expect(view.host.send).not.toHaveBeenCalled();
    } finally {
      services.dispose();
    }
  });

  it('does not send the queued batch for a session that changed during import', async () => {
    const { services, view, store } = makeReview();
    try {
      store.queueDiffComment({
        path: 'main.go',
        side: 'new',
        line: 12,
        body: 'Keep this guard.',
        scope: 'uncommitted',
        context: 'if ready {',
      });
      const gate = gateOperations(store);
      const pending = store.sendDiffComments();

      navigate(view, 's2');
      const operations = fakeOperations();
      gate.resolve(operations);
      await pending;

      expect(operations.sendDiffComments).not.toHaveBeenCalled();
      expect(view.host.send).not.toHaveBeenCalled();
    } finally {
      services.dispose();
    }
  });

  it('does not invoke operations after disposal during import', async () => {
    const { services, store } = makeReview();
    try {
      const gate = gateOperations(store);
      const pending = store.loadDiff();
      services.dispose();
      const operations = fakeOperations();
      gate.resolve(operations);
      await pending;

      expect(operations.loadDiff).not.toHaveBeenCalled();
    } finally {
      services.dispose();
    }
  });

  it('passes the captured owner, scope, and refresh flag once the import resolves', async () => {
    const { services, store } = makeReview();
    try {
      store.diff.value = {
        ...store.diff.value,
        open: true,
        sessionId: 's1',
        files: [{ path: 'main.go', status: 'modify', context: 3 }],
      };
      const gate = gateOperations(store);
      const file = store.diff.value.files[0];
      const pending = store.expandDiff(file, 48, true);
      const operations = fakeOperations();
      gate.resolve(operations);
      await pending;

      expect(operations.expandDiff).toHaveBeenCalledWith(file, 's1', 'last_turn', 48, true);
    } finally {
      services.dispose();
    }
  });
});
