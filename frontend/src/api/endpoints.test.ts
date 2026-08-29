import { describe, expect, it, vi } from 'vitest';
import type { APIClient } from './client';
import { endpoints } from './endpoints';

describe('worktree endpoints', () => {
  it('identifies the active session for cleanup-aware mutations', async () => {
    const post = vi.fn(async () => ({}));
    const routes = endpoints({ post } as unknown as APIClient);

    await routes.switchWorktree('project/one', '/tmp/tree', 'session/one');
    expect(post).toHaveBeenLastCalledWith(
      '/v1/projects/project%2Fone/worktrees/switch',
      { dir: '/tmp/tree' },
      'mutation',
      { 'X-Term-LLM-Session-ID': 'session/one' },
    );

    await routes.mergeWorktree('project/one', '/tmp/tree', 'session/one');
    expect(post).toHaveBeenLastCalledWith(
      '/v1/projects/project%2Fone/worktrees/merge',
      { dir: '/tmp/tree' },
      'mutation',
      { 'X-Term-LLM-Session-ID': 'session/one' },
    );

    await routes.mergeWorktree('project/one', '/tmp/tree', 'session/one', true);
    expect(post).toHaveBeenLastCalledWith(
      '/v1/projects/project%2Fone/worktrees/merge',
      { dir: '/tmp/tree', force: true },
      'mutation',
      { 'X-Term-LLM-Session-ID': 'session/one' },
    );

    await routes.assistedMergeWorktree('project/one', '/tmp/tree', 'session/one');
    expect(post).toHaveBeenLastCalledWith(
      '/v1/projects/project%2Fone/worktrees/assisted-merge',
      { dir: '/tmp/tree' },
      'mutation',
      { 'X-Term-LLM-Session-ID': 'session/one' },
    );

    await routes.promoteWorktree('project/one', '/tmp/tree', 'feature/tree', 'session/one');
    expect(post).toHaveBeenLastCalledWith(
      '/v1/projects/project%2Fone/worktrees/promote',
      { dir: '/tmp/tree', branch: 'feature/tree' },
      'mutation',
      { 'X-Term-LLM-Session-ID': 'session/one' },
    );
  });
});

describe('branch tree endpoint', () => {
  it('requests branch points only for the interactive tree browser', async () => {
    const get = vi.fn(async () => ({}));
    const routes = endpoints({ get } as unknown as APIClient);

    await routes.tree('session/one');
    expect(get).toHaveBeenLastCalledWith('/v1/sessions/session%2Fone/tree', undefined);

    await routes.tree('session/one', undefined, true);
    expect(get).toHaveBeenLastCalledWith(
      '/v1/sessions/session%2Fone/tree?include_branch_points=1',
      undefined,
    );
  });

  it('allows path-note requests to outlive the server detached-work budget', async () => {
    const json = vi.fn(async () => ({}));
    const routes = endpoints({ json } as unknown as APIClient);

    await routes.pathNotes('session/one', { mode: 'notes' });

    expect(json).toHaveBeenCalledWith(
      '/v1/sessions/session%2Fone/path-notes',
      { method: 'POST', body: JSON.stringify({ mode: 'notes' }) },
      { policy: 'mutation', timeoutMs: 150_000 },
    );
  });
});
