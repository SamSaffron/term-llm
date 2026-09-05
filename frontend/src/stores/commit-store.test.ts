import { signal } from '@preact/signals';
import { describe, expect, it, vi } from 'vitest';
import type { Session } from '../domain/types';
import type { AppStoreServices } from './app-store-services';
import type { Modal } from './store-types';
import { APIError } from '../api/client';
import { CommitStore } from './commit-store';
import { fileChangeStats } from '../components/CommitModal';

const fingerprint = {
  checkout_id: 'checkout',
  head_state: 'born',
  head_oid: 'head',
  index_tree: 'tree',
  operation: { kind: 'none', head_oids: [], digest: 'none' },
};
const status = (staged = false) => ({
  branch: 'main',
  staged: staged
    ? [{ path: 'a.ts', kind: 'modified', staged: true, additions: 3, deletions: 1 }]
    : [],
  unstaged: staged
    ? []
    : [{ path: 'a.ts', kind: 'modified', unstaged: true, additions: 3, deletions: 1 }],
  untracked: [],
  total_staged: staged ? 1 : 0,
  total_unstaged: staged ? 0 : 1,
  total_untracked: 0,
  fingerprint,
  status_token: 'token',
  selection_available: true,
  summary: { files: 1, additions: 1, deletions: 0 },
});

function fixture(initial = status(false), draft = false) {
  const endpoints = {
    commitPublishPlan: vi.fn(
      async (_id: string, kind: 'push' | 'pr'): Promise<Record<string, unknown>> => ({
        checkout_id: 'checkout',
        head_oid: 'committed',
        branch: 'main',
        remote: 'origin',
        url: 'git@github.com:test/repo.git',
        target: 'main',
        ...(kind === 'pr'
          ? { repository: 'https://github.com/test/repo', default_branch: 'main' }
          : {}),
      }),
    ),
    commitStatus: vi.fn(async () => initial),
    commitStage: vi.fn(async () => status(true)),
    createCommitRun: vi.fn(async (_id: string, body: Record<string, unknown>) => ({
      run_id: body.kind === 'scope' ? 'scope-1' : 'message-1',
    })),
    commitRun: vi.fn(async (_id: string, runId: string) =>
      runId === 'scope-1'
        ? {
            status: 'complete',
            proposal: { mode: 'selected', include_paths: ['a.ts'], summary: 'Only A' },
          }
        : { status: 'complete', message: 'Change A' },
    ),
    cancelCommitRun: vi.fn(async () => ({})),
    createCommitOperation: vi.fn(async (_id: string, _body: unknown, _key: string) => ({
      operation_id: 'op-1',
    })),
    commitOperation: vi.fn(async (): Promise<Record<string, unknown>> => ({
      status: 'succeeded',
      result: {
        head_oid: 'committed',
        short_oid: 'abc123',
        subject: 'Change A',
        message: 'Change A\n\nDetails',
      },
    })),
  };
  const toast = vi.fn();
  const services = { endpoints, toast, storage: localStorage } as unknown as AppStoreServices;
  const activeSession = signal({ id: 's1' } as Session);
  const modal = signal<Modal>('');
  const changed = vi.fn();
  const store = new CommitStore(services, {
    activeSession,
    draftActive: signal(draft),
    modal,
    busy: () => false,
    changed,
  });
  return { store, endpoints, modal, changed, toast, activeSession };
}

describe('CommitStore', () => {
  async function committedFixture() {
    const f = fixture(status(true));
    await f.store.open();
    await vi.waitFor(() => expect(f.store.state.value.phase).toBe('editing'));
    await f.store.commit();
    f.endpoints.createCommitOperation.mockClear();
    return f;
  }

  it('previews publishing, then submits the reviewed destination without another commit', async () => {
    const { store, endpoints } = await committedFixture();
    await store.preparePublish('push');
    expect(store.state.value.publishForm?.branch).toBe('main');
    expect(endpoints.createCommitOperation).not.toHaveBeenCalled();
    endpoints.commitOperation.mockResolvedValue({
      status: 'succeeded',
      publish_result: { pushed: true, branch: 'main' },
    });
    await store.publish();
    expect(endpoints.createCommitOperation).toHaveBeenCalledWith(
      's1',
      expect.objectContaining({
        kind: 'push',
        publish: expect.objectContaining({ branch: 'main' }),
      }),
      expect.stringMatching(/^publish_/),
    );
    expect(store.state.value.phase).toBe('success');
    expect(store.state.value.result?.head_oid).toBe('committed');
    expect(store.state.value.info).toContain('origin/main');
    expect(localStorage.getItem('term-llm.commit-publish.s1')).toBeNull();
  });

  it('suggests a new PR branch on main and submits edited PR metadata', async () => {
    const { store, endpoints } = await committedFixture();
    await store.preparePublish('pr');
    expect(store.state.value.publishForm).toMatchObject({
      branch: 'pr/abc123',
      base: 'main',
      title: 'Change A',
      body: 'Details',
    });
    store.editPublish({ title: 'Reviewed title', body: 'Reviewed body', draft: true });
    endpoints.commitOperation.mockResolvedValue({
      status: 'succeeded',
      publish_result: {
        pushed: true,
        branch: 'pr/abc123',
        pr_url: 'https://github.com/test/repo/pull/1',
        existing: true,
      },
    });
    await store.publish();
    expect(endpoints.createCommitOperation).toHaveBeenCalledWith(
      's1',
      expect.objectContaining({
        kind: 'pr',
        publish: expect.objectContaining({
          title: 'Reviewed title',
          body: 'Reviewed body',
          draft: true,
        }),
      }),
      expect.any(String),
    );
    expect(store.state.value.info).toContain('existing');
    expect(store.state.value.publishResult?.pr_url).toContain('/pull/1');
  });

  it('rejects publishing if HEAD changed after committing', async () => {
    const { store, endpoints } = await committedFixture();
    endpoints.commitPublishPlan.mockResolvedValue({ head_oid: 'newer' });
    await store.preparePublish('push');
    expect(store.state.value.publishForm).toBeNull();
    expect(store.state.value.error).toContain('HEAD changed');
    expect(store.state.value.phase).toBe('success');
  });

  it('keeps the commit and partial push result if PR creation fails', async () => {
    const { store, endpoints } = await committedFixture();
    await store.preparePublish('pr');
    endpoints.commitOperation.mockResolvedValue({
      status: 'uncertain',
      error: 'Check GitHub before retrying',
      publish_result: { pushed: true, branch: 'pr/abc123' },
    });
    await store.publish();
    expect(store.state.value.result?.head_oid).toBe('committed');
    expect(store.state.value.publishResult?.pushed).toBe(true);
    expect(store.state.value.info).toContain('was pushed');
    expect(store.state.value.error).toContain('Check GitHub');
  });

  it('recovers a disconnected publish with the same idempotency key', async () => {
    const { store, endpoints } = await committedFixture();
    await store.preparePublish('push');
    endpoints.createCommitOperation.mockRejectedValueOnce(new Error('offline'));
    await store.publish();
    const firstKey = endpoints.createCommitOperation.mock.calls[0][2];
    expect(store.state.value.publishPending).toBe(true);
    await store.preparePublish('pr');
    expect(store.state.value.publishForm).toBeNull();
    store.reset();
    endpoints.commitOperation.mockResolvedValue({
      status: 'succeeded',
      publish_result: { pushed: true, branch: 'main' },
    });
    await store.open();
    expect(endpoints.createCommitOperation.mock.calls[1][2]).toBe(firstKey);
    expect(store.state.value.publishPending).toBe(false);
    expect(store.state.value.result?.head_oid).toBe('committed');
  });

  it('allows editing again after rejected publish admission', async () => {
    const { store, endpoints } = await committedFixture();
    await store.preparePublish('push');
    endpoints.createCommitOperation.mockRejectedValueOnce(new APIError('checkout busy', 409));
    await store.publish();
    expect(store.state.value.publishPending).toBe(false);
    expect(store.state.value.publishForm?.kind).toBe('push');
    expect(localStorage.getItem('term-llm.commit-publish.s1')).toBeNull();
  });

  it('blocks duplicate submission and closing while publishing, but ignores late results after switching session', async () => {
    const { store, endpoints, activeSession } = await committedFixture();
    await store.preparePublish('push');
    let finish!: (value: Record<string, unknown>) => void;
    endpoints.commitOperation.mockImplementation(
      () =>
        new Promise((resolve) => {
          finish = resolve;
        }),
    );
    const pending = store.publish();
    await vi.waitFor(() => expect(finish).toBeDefined());
    await store.publish();
    store.close();
    expect(store.state.value.publishBusy).toBe(true);
    expect(endpoints.createCommitOperation).toHaveBeenCalledTimes(1);
    activeSession.value = { id: 's2' } as Session;
    store.reset();
    finish({ status: 'succeeded', publish_result: { pushed: true } });
    await pending;
    expect(store.state.value.phase).toBe('closed');
    expect(localStorage.getItem('term-llm.commit-publish.s1')).not.toBeNull();
  });
  it('formats per-file line counts and binary changes for review', () => {
    expect(
      fileChangeStats([
        { path: 'a.ts', kind: 'modified', additions: 3, deletions: 1 },
        { path: 'a.ts', kind: 'modified', additions: 2, deletions: 0 },
      ]),
    ).toBe('+5 −1');
    expect(fileChangeStats([{ path: 'image.png', kind: 'modified', binary: true }])).toBe('binary');
  });

  it('asks all versus staged only when index and working-tree changes coexist', async () => {
    const mixed = {
      ...status(true),
      unstaged: [{ path: 'b.ts', kind: 'modified', unstaged: true, additions: 2, deletions: 0 }],
      total_unstaged: 1,
    };
    const { store, endpoints, modal } = fixture(mixed);
    await store.open('mention issue');
    expect(modal.value).toBe('commit');
    expect(store.state.value.phase).toBe('choosing_scope');
    expect(endpoints.createCommitRun).not.toHaveBeenCalled();
  });

  it('drafts a message immediately when every change is already staged', async () => {
    const { store, endpoints } = fixture(status(true));

    await store.open('mention issue');
    await vi.waitFor(() => expect(store.state.value.phase).toBe('editing'));

    expect(endpoints.commitStage).not.toHaveBeenCalled();
    expect(endpoints.createCommitRun).toHaveBeenCalledWith(
      's1',
      expect.objectContaining({ kind: 'message', intent: 'mention issue' }),
    );
  });

  it('automatically stages all for plain /commit with an empty index', async () => {
    const { store, endpoints } = fixture();
    await store.open('');
    await vi.waitFor(() => expect(store.state.value.phase).toBe('editing'));
    expect(endpoints.commitStage).toHaveBeenCalledWith(
      's1',
      expect.objectContaining({ mode: 'all' }),
    );
    expect(store.state.value.phase).toBe('editing');
    expect(store.state.value.message).toBe('Change A');
  });

  it('normalizes null change lists returned for empty Git status categories', async () => {
    const { store, endpoints } = fixture({
      ...status(false),
      staged: null,
      untracked: null,
    } as never);
    endpoints.commitStage.mockResolvedValue({
      ...status(true),
      unstaged: null,
      untracked: null,
    } as never);

    await store.open('');
    await vi.waitFor(() => expect(store.state.value.phase).toBe('editing'));

    expect(store.state.value.status).toMatchObject({
      staged: [expect.objectContaining({ path: 'a.ts' })],
      unstaged: [],
      untracked: [],
    });
  });

  it('reviews a natural-language subset before staging it', async () => {
    const { store, endpoints } = fixture();
    await store.open('only A');
    await vi.waitFor(() => expect(store.state.value.phase).toBe('reviewing_scope'));
    expect(store.state.value.phase).toBe('reviewing_scope');
    expect(store.state.value.selected).toEqual(['a.ts']);
    await store.backToMessage();
    expect(endpoints.commitStage).toHaveBeenCalledWith(
      's1',
      expect.objectContaining({ mode: 'exact_selection', paths: ['a.ts'] }),
    );
  });

  it('returns from optional file review to the preserved message without creating a safety gate', async () => {
    const { store } = fixture(status(true));
    await store.open('');
    await vi.waitFor(() => expect(store.state.value.phase).toBe('editing'));
    store.setMessage('Preserve this draft');

    await store.reviewFiles();
    expect(store.state.value).toMatchObject({
      phase: 'reviewing_scope',
      reviewingFromEditor: true,
      reviewRequired: false,
    });

    await store.backToMessage();
    expect(store.state.value).toMatchObject({
      phase: 'editing',
      message: 'Preserve this draft',
      reviewingFromEditor: false,
      reviewRequired: false,
    });
  });

  it('applies changed file choices while returning and preserves the existing message', async () => {
    const mixed = {
      ...status(true),
      unstaged: [{ path: 'b.ts', kind: 'modified', unstaged: true, additions: 2, deletions: 0 }],
      total_unstaged: 1,
    };
    const { store, endpoints } = fixture(mixed);
    await store.open('');
    await store.chooseStaged();
    await vi.waitFor(() => expect(store.state.value.phase).toBe('editing'));
    store.setMessage('Preserve this edited message');

    await store.reviewFiles();
    store.setSelected('b.ts', true);
    expect(store.state.value.selectionNeedsApply).toBe(true);
    await store.backToMessage();

    expect(endpoints.commitStage).toHaveBeenCalledWith(
      's1',
      expect.objectContaining({ mode: 'exact_selection', paths: ['a.ts', 'b.ts'] }),
    );
    expect(store.state.value).toMatchObject({
      phase: 'editing',
      message: 'Preserve this edited message',
      reviewRequired: false,
      selectionNeedsApply: false,
    });
    expect(endpoints.createCommitRun).toHaveBeenCalledTimes(1);
  });

  it('clears a pre-existing safety gate only after Back reapplies the reviewed selection', async () => {
    const { store, endpoints } = fixture(status(true));
    await store.open('');
    await vi.waitFor(() => expect(store.state.value.phase).toBe('editing'));
    store.state.value = { ...store.state.value, reviewRequired: true };

    await store.reviewFiles();
    expect(endpoints.commitStatus).toHaveBeenCalled();
    expect(store.state.value.reviewRequired).toBe(true);
    await store.backToMessage();
    expect(endpoints.commitStage).toHaveBeenCalledWith(
      's1',
      expect.objectContaining({ mode: 'exact_selection', paths: ['a.ts'] }),
    );
    expect(store.state.value).toMatchObject({ phase: 'editing', reviewRequired: false });
  });

  it('submits one idempotent operation and reports verified success', async () => {
    const { store, endpoints, changed } = fixture();
    await store.open('');
    await vi.waitFor(() => expect(store.state.value.phase).toBe('editing'));
    await store.commit();
    expect(endpoints.createCommitOperation).toHaveBeenCalledTimes(1);
    expect(String(endpoints.createCommitOperation.mock.calls[0][2])).toMatch(/^commit_/);
    expect(store.state.value.phase).toBe('success');
    expect(store.state.value.result?.short_oid).toBe('abc123');
    expect(changed).toHaveBeenCalled();
  });

  it('does not re-arm Commit with a refreshed fingerprint after failure', async () => {
    const { store, endpoints } = fixture();
    endpoints.commitOperation.mockResolvedValue({ status: 'failed', error: 'stale review' });
    await store.open('');
    await vi.waitFor(() => expect(store.state.value.phase).toBe('editing'));
    await store.commit();
    expect(store.state.value.reviewRequired).toBe(true);
    expect(endpoints.createCommitOperation).toHaveBeenCalledTimes(1);
    await store.commit();
    expect(endpoints.createCommitOperation).toHaveBeenCalledTimes(1);
    expect(store.state.value.error).toContain('review');
  });

  it('preserves and re-arms the same message after a recoverable unchanged failure', async () => {
    const { store, endpoints } = fixture();
    endpoints.commitOperation.mockResolvedValue({ status: 'failed', error: 'hook rejected' });
    await store.open('');
    await vi.waitFor(() => expect(store.state.value.phase).toBe('editing'));
    endpoints.commitStatus.mockResolvedValue(status(true));
    store.setMessage('Keep this exact message');
    await store.commit();
    expect(store.state.value.phase).toBe('editing');
    expect(store.state.value.reviewRequired).toBe(false);
    expect(store.state.value.message).toBe('Keep this exact message');
    expect(store.state.value.error).toContain('preserved for retry');
  });

  it('reconnects a persisted final operation after a browser refresh', async () => {
    const { store, endpoints } = fixture(status(true));
    localStorage.setItem(
      'term-llm.commit-operation.s1',
      JSON.stringify({ operationId: 'op-recover', message: 'Preserved' }),
    );
    await store.open('');
    expect(endpoints.commitOperation).toHaveBeenCalledWith('s1', 'op-recover');
    expect(store.state.value.phase).toBe('success');
    expect(localStorage.getItem('term-llm.commit-operation.s1')).toBeNull();
  });

  it('keeps /commit unavailable for an unpersisted draft', async () => {
    const { store, modal, toast } = fixture(status(false), true);
    await expect(store.open('')).resolves.toBe(false);
    expect(modal.value).toBe('');
    expect(toast).toHaveBeenCalled();
  });
});
