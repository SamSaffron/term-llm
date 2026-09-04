import { describe, expect, it, vi } from 'vitest';
import type { APIClient } from './client';
import { endpoints } from './endpoints';

describe('shell endpoints', () => {
  it('uses authenticated HTTP/SSE session routes with encoded generation state', async () => {
    const json = vi.fn(async () => ({ shell_id: 'sh/one' }));
    const request = vi.fn(async () => new Response());
    const routes = endpoints({ json, request } as unknown as APIClient);
    const controller = new AbortController();

    await routes.shellCreate('session/one', 100, 30);
    expect(json).toHaveBeenLastCalledWith(
      '/v1/sessions/session%2Fone/shell',
      {
        method: 'POST',
        headers: { 'X-Term-LLM-Session-ID': 'session/one' },
        body: JSON.stringify({ cols: 100, rows: 30 }),
      },
      { policy: 'mutation', auth: 'session' },
    );

    await routes.shellCollaboration('session/one', 'sh/one', true);
    expect(json).toHaveBeenLastCalledWith(
      '/v1/sessions/session%2Fone/shell/collaboration',
      {
        method: 'POST',
        headers: { 'X-Term-LLM-Session-ID': 'session/one' },
        body: JSON.stringify({ shell_id: 'sh/one', enabled: true }),
      },
      { policy: 'mutation', auth: 'session', timeoutMs: 3000 },
    );

    await routes.shellInterrupt('session/one', 'sh/one', 'cmd/one');
    expect(json).toHaveBeenLastCalledWith(
      '/v1/sessions/session%2Fone/shell/interrupt',
      {
        method: 'POST',
        headers: { 'X-Term-LLM-Session-ID': 'session/one' },
        body: JSON.stringify({ shell_id: 'sh/one', command_id: 'cmd/one' }),
      },
      { policy: 'mutation', auth: 'session', timeoutMs: 5000 },
    );

    await routes.shellStream('session/one', 'sh/one', 42, controller.signal);
    expect(request).toHaveBeenLastCalledWith(
      '/v1/sessions/session%2Fone/shell/stream?shell_id=sh%2Fone&offset=42',
      {
        signal: controller.signal,
        headers: {
          'X-Term-LLM-Session-ID': 'session/one',
          Accept: 'text/event-stream',
        },
      },
      { policy: 'stream', retries: 0, timeoutMs: 0, auth: 'session' },
    );
  });
});

describe('file change endpoints', () => {
  it('fetches encoded raw text with version pinning and cancellation', async () => {
    const response = new Response('# Plan\n', {
      status: 200,
      headers: { 'Content-Type': 'text/plain; charset=utf-8' },
    });
    const request = vi.fn(async () => response);
    const routes = endpoints({ request } as unknown as APIClient);
    const controller = new AbortController();

    await expect(
      routes.fileText(
        'session/one',
        '/work/Plan File.md',
        'last_3_turns',
        'after',
        42,
        controller.signal,
      ),
    ).resolves.toBe('# Plan\n');
    expect(request).toHaveBeenCalledWith(
      '/v1/sessions/session%2Fone/file-changes/content?path=%2Fwork%2FPlan%20File.md&scope=last_3_turns&side=after&snapshot_seq=42',
      { signal: controller.signal, headers: { Accept: 'text/plain' } },
      { policy: 'safe-read', auth: 'session', versionCheck: false },
    );
  });

  it('does not treat session review responses as shell asset updates', async () => {
    const get = vi.fn(async () => ({}));
    const routes = endpoints({ get } as unknown as APIClient);

    await routes.fileChanges('session/one', 'uncommitted');
    await routes.fileDiff('session/one', '/work/Plan File.md', 'uncommitted', 12, 42);
    await routes.diffComments('session/one');

    const controls = { auth: 'session', versionCheck: false };
    expect(get).toHaveBeenNthCalledWith(
      1,
      '/v1/sessions/session%2Fone/file-changes?scope=uncommitted',
      undefined,
      controls,
    );
    expect(get).toHaveBeenNthCalledWith(
      2,
      '/v1/sessions/session%2Fone/file-changes/diff?path=%2Fwork%2FPlan%20File.md&scope=uncommitted&context=12&snapshot_seq=42',
      undefined,
      controls,
    );
    expect(get).toHaveBeenNthCalledWith(
      3,
      '/v1/sessions/session%2Fone/diff-comments',
      undefined,
      controls,
    );
  });
});

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
