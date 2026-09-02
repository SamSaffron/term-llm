import { afterEach, describe, expect, it, vi } from 'vitest';
import type { HubClient } from '../../api/hub-client';
import type { AttentionResponse, NodesResponse } from '../domain/types';
import type { PasskeyPlatform } from '../platform/passkeys';
import { HubStore } from './hub-store';

const nodes = (id = 'alpha'): NodesResponse => ({
  nodes: [
    {
      id,
      name: id,
      source: 'config',
      connection: 'direct',
      url: 'http://node.test',
      base_path: '',
      proxy_path: `/node/${id}/`,
      new_session_path: `/node/${id}/?new=1`,
      has_token: false,
      status: { reachable: true, state: 'ok', latency_ms: 1 },
      sessions: { count_label: '1 session', active_count: 1 },
    },
  ],
});
const attention = (title = 'Ready'): AttentionResponse => ({
  total_running: 0,
  total_input_required: 0,
  total_unseen: 1,
  nodes: [],
  input_required: [],
  inbox: [
    {
      node_id: 'alpha',
      node_name: 'Alpha',
      session_id: 's1',
      title,
      outcome: 'succeeded',
      attention_seq: 1,
      resume_path: '/node/alpha/chat/1',
    },
  ],
  has_more: false,
});

function fakeClient(overrides: Record<string, unknown> = {}) {
  return {
    listNodes: vi.fn(async () => nodes()),
    listAttention: vi.fn(async () => attention()),
    listDelegations: vi.fn(async () => ({ delegations: [] })),
    ...overrides,
  } as unknown as HubClient;
}

describe('HubStore', () => {
  afterEach(() => vi.useRealTimers());

  it('starts one refresh and polls each endpoint exactly once per interval', async () => {
    vi.useFakeTimers();
    const client = fakeClient();
    const store = new HubStore(client);
    store.start();
    await vi.runAllTicks();
    await vi.advanceTimersByTimeAsync(0);
    expect(client.listNodes).toHaveBeenCalledTimes(1);
    expect(client.listAttention).toHaveBeenCalledTimes(1);
    expect(client.listDelegations).toHaveBeenCalledTimes(1);
    expect(store.reachableCount.value).toBe(1);
    expect(store.activeSessionCount.value).toBe(1);
    await vi.advanceTimersByTimeAsync(15_000);
    expect(client.listNodes).toHaveBeenCalledTimes(2);
    expect(client.listAttention).toHaveBeenCalledTimes(2);
    expect(client.listDelegations).toHaveBeenCalledTimes(2);
    store.dispose();
    await vi.advanceTimersByTimeAsync(30_000);
    expect(client.listNodes).toHaveBeenCalledTimes(2);
  });

  it('coalesces a poll behind a pending refresh and aborts the read on disposal', async () => {
    vi.useFakeTimers();
    let readSignal: AbortSignal | undefined;
    const listNodes = vi.fn(
      (signal?: AbortSignal) =>
        new Promise<NodesResponse>((_resolve, reject) => {
          readSignal = signal;
          signal?.addEventListener('abort', () =>
            reject(new DOMException('aborted', 'AbortError')),
          );
        }),
    );
    const client = fakeClient({ listNodes });
    const store = new HubStore(client);
    store.start();
    await vi.runAllTicks();
    await vi.advanceTimersByTimeAsync(15_000);
    expect(client.listNodes).toHaveBeenCalledOnce();
    expect(client.listAttention).toHaveBeenCalledOnce();
    expect(client.listDelegations).toHaveBeenCalledOnce();
    store.dispose();
    expect(readSignal?.aborted).toBe(true);
    await vi.runAllTicks();
  });

  it('settles endpoints independently and retains durable attention on failure', async () => {
    const client = fakeClient();
    const store = new HubStore(client);
    await store.refresh('initial');
    expect(store.inbox.value[0].title).toBe('Ready');
    vi.mocked(client.listNodes).mockResolvedValueOnce(nodes('beta'));
    vi.mocked(client.listAttention).mockRejectedValueOnce(new Error('attention unavailable'));
    vi.mocked(client.listDelegations).mockRejectedValueOnce(new Error('delegations unavailable'));
    await store.refresh('manual');
    expect(store.nodes.value[0].id).toBe('beta');
    expect(store.inbox.value[0].title).toBe('Ready');
    expect(store.attentionError.value).toBe('attention unavailable');
    expect(store.delegationError.value).toBe('delegations unavailable');
  });

  it('aborts an older read cycle so stale results cannot overwrite a manual refresh', async () => {
    let resolveOld!: (value: NodesResponse) => void;
    const oldNodes = new Promise<NodesResponse>((resolve) => (resolveOld = resolve));
    const client = fakeClient({
      listNodes: vi.fn().mockReturnValueOnce(oldNodes).mockResolvedValueOnce(nodes('new')),
    });
    const store = new HubStore(client);
    const first = store.refresh('initial');
    const second = store.refresh('manual');
    await second;
    resolveOld(nodes('old'));
    await first;
    expect(store.nodes.value[0].id).toBe('new');
  });

  it('rejects duplicate node operations and preserves warning forms', async () => {
    let resolveAdd!: (value: { id: string; warning?: string }) => void;
    const add = new Promise<{ id: string; warning?: string }>((resolve) => (resolveAdd = resolve));
    const client = fakeClient({
      addNode: vi.fn().mockReturnValue(add),
    });
    const store = new HubStore(client);
    const value = { name: 'Alpha', url: 'http://node.test', token: 'secret' };
    const first = store.addNode(value);
    await store.addNode(value);
    expect(client.addNode).toHaveBeenCalledOnce();
    resolveAdd({ id: 'alpha', warning: 'shadowed' });
    expect(await first).toEqual({ clean: false });
    expect(store.nodeOperationResult.value).toContain('Added with warning: shadowed');
  });

  it('reports node test, add, and remove failures without retrying operations', async () => {
    const testNode = vi
      .fn()
      .mockResolvedValueOnce({ status: { reachable: true, state: 'ok', latency_ms: 7 } })
      .mockRejectedValueOnce(new Error('probe failed'));
    const addNode = vi.fn(async () => {
      throw new Error('add failed');
    });
    const removeNode = vi.fn(async () => {
      throw new Error('remove failed');
    });
    const store = new HubStore(fakeClient({ testNode, addNode, removeNode }));
    const value = { name: 'Alpha', url: 'http://node.test', token: '' };

    await store.testNode(value);
    expect(store.nodeOperationResult.value).toContain('Reachable in 7 ms');
    await store.testNode(value);
    expect(store.nodeOperationResult.value).toBe('✗ probe failed');
    await expect(store.addNode(value)).resolves.toEqual({ clean: false });
    expect(store.nodeOperationResult.value).toBe('✗ add failed');
    await store.removeNode('alpha');
    expect(store.nodeError.value).toBe('Could not remove node: remove failed');
    expect(testNode).toHaveBeenCalledTimes(2);
    expect(addNode).toHaveBeenCalledOnce();
    expect(removeNode).toHaveBeenCalledOnce();
  });

  it('loads registration lazily and clears the token whenever help or the dialog closes', async () => {
    const client = fakeClient({
      registrationInfo: vi.fn(async () => ({
        enabled: true,
        registration_token: 'registration-secret',
      })),
    });
    const store = new HubStore(client);
    expect(client.registrationInfo).not.toHaveBeenCalled();
    await store.openRegistrationHelp();
    expect(store.registrationInfo.value?.registration_token).toBe('registration-secret');
    store.registrationRevealed.value = true;
    store.closeRegistrationHelp();
    expect(store.registrationInfo.value).toBeNull();
    expect(store.registrationRevealed.value).toBe(false);
    await store.openRegistrationHelp();
    expect(client.registrationInfo).toHaveBeenCalledTimes(2);
    store.closeAddDialog();
    expect(store.registrationInfo.value).toBeNull();
  });

  it('keeps a mutation follow-up read newer than an older poll response', async () => {
    let resolveOld!: (value: NodesResponse) => void;
    const oldNodes = new Promise<NodesResponse>((resolve) => (resolveOld = resolve));
    const client = fakeClient({
      listNodes: vi.fn().mockReturnValueOnce(oldNodes).mockResolvedValueOnce(nodes('new')),
      addNode: vi.fn(async () => ({ id: 'new' })),
    });
    const store = new HubStore(client);
    const poll = store.refresh('initial');
    await expect(
      store.addNode({ name: 'New', url: 'http://new.test', token: '' }),
    ).resolves.toEqual({
      clean: true,
    });
    resolveOld(nodes('old'));
    await poll;
    expect(store.nodes.value[0].id).toBe('new');
  });

  it('does not report a successful add as failed when only its follow-up read fails', async () => {
    const client = fakeClient({
      addNode: vi.fn(async () => ({ id: 'new' })),
      listNodes: vi.fn(async () => {
        throw new Error('refresh unavailable');
      }),
    });
    const store = new HubStore(client);
    const result = await store.addNode({ name: 'New', url: 'http://new.test', token: '' });
    expect(result).toEqual({ clean: true });
    expect(store.nodeOperationResult.value).toBe('');
    expect(store.nodeError.value).toContain('Node was added');
  });

  it('reauthenticates before removing a credential and refreshes security afterward', async () => {
    const calls: string[] = [];
    const client = fakeClient({
      beginReauthentication: vi.fn(async () => {
        calls.push('begin-reauth');
        return { publicKey: {} };
      }),
      finishReauthentication: vi.fn(async () => {
        calls.push('finish-reauth');
        return { ok: true };
      }),
      removeCredential: vi.fn(async () => {
        calls.push('remove');
        return { ok: true };
      }),
      listCredentials: vi.fn(async () => {
        calls.push('list-credentials');
        return { credentials: [] };
      }),
      session: vi.fn(async () => {
        calls.push('session');
        return { active_sessions: 1 };
      }),
    });
    const passkeys = {
      get: vi.fn(async () => {
        calls.push('platform-get');
        return { id: 'credential' };
      }),
    };
    const store = new HubStore(client, passkeys as unknown as PasskeyPlatform);
    await store.removeCredential('record');
    expect(calls.slice(0, 4)).toEqual(['begin-reauth', 'platform-get', 'finish-reauth', 'remove']);
    expect(calls.slice(4).sort()).toEqual(['list-credentials', 'session']);
  });

  it('loads security metadata together and reports a partial read failure', async () => {
    const listCredentials = vi
      .fn()
      .mockResolvedValueOnce({
        credentials: [
          {
            record_id: 'primary',
            display_name: 'Primary',
            created_at: '2026-01-01T00:00:00Z',
            last_used_at: '2026-01-01T00:00:00Z',
          },
        ],
      })
      .mockRejectedValueOnce(new Error('credentials unavailable'));
    const session = vi.fn(async () => ({ active_sessions: 2 }));
    const store = new HubStore(fakeClient({ listCredentials, session }));

    await store.loadSecurity();
    expect(store.credentials.value.map((credential) => credential.record_id)).toEqual(['primary']);
    expect(store.activeSessions.value).toBe(2);
    expect(listCredentials).toHaveBeenCalledOnce();
    expect(session).toHaveBeenCalledOnce();
    await store.loadSecurity();
    expect(store.securityStatus.value).toBe('credentials unavailable');
  });

  it('runs rename, add-passkey, revoke, and logout administration flows', async () => {
    const calls: string[] = [];
    const client = fakeClient({
      renameCredential: vi.fn(async () => calls.push('rename')),
      beginReauthentication: vi.fn(async () => {
        calls.push('begin-reauth');
        return { publicKey: {} };
      }),
      finishReauthentication: vi.fn(async () => calls.push('finish-reauth')),
      beginAdditionalRegistration: vi.fn(async () => {
        calls.push('begin-add');
        return { publicKey: {} };
      }),
      finishAdditionalRegistration: vi.fn(async () => calls.push('finish-add')),
      revokeOtherSessions: vi.fn(async () => {
        calls.push('revoke');
        return { revoked: 2 };
      }),
      logout: vi.fn(async () => {
        calls.push('logout');
        return { redirect: '/hub/auth/login' };
      }),
      listCredentials: vi.fn(async () => ({ credentials: [] })),
      session: vi.fn(async () => ({ active_sessions: 1 })),
    });
    const passkeys = {
      get: vi.fn(async () => {
        calls.push('get');
        return { id: 'reauth' };
      }),
      create: vi.fn(async () => {
        calls.push('create');
        return { id: 'additional' };
      }),
    } as unknown as PasskeyPlatform;
    const store = new HubStore(client, passkeys);

    await store.renameCredential('primary', 'Renamed');
    expect(store.securityStatus.value).toBe('Passkey renamed.');
    await store.addPasskey('Backup');
    expect(calls).toEqual(
      expect.arrayContaining([
        'rename',
        'begin-reauth',
        'get',
        'finish-reauth',
        'begin-add',
        'create',
        'finish-add',
      ]),
    );
    expect(calls.indexOf('finish-reauth')).toBeLessThan(calls.indexOf('begin-add'));
    await store.revokeOtherSessions();
    expect(store.securityStatus.value).toBe('Revoked 2 other sessions.');
    await expect(store.signOut()).resolves.toBe('/hub/auth/login');
    expect(calls.slice(-2)).toEqual(['revoke', 'logout']);
  });

  it('does not restore a registration token after help closes during its request', async () => {
    let resolveInfo!: (value: { enabled: boolean; registration_token: string }) => void;
    const pendingInfo = new Promise<{ enabled: boolean; registration_token: string }>(
      (resolve) => (resolveInfo = resolve),
    );
    const client = fakeClient({ registrationInfo: vi.fn(() => pendingInfo) });
    const store = new HubStore(client);
    const pending = store.openRegistrationHelp();
    store.closeRegistrationHelp();
    resolveInfo({ enabled: true, registration_token: 'late-secret' });
    await pending;
    expect(store.registrationInfo.value).toBeNull();
    expect(store.registrationLoading.value).toBe(false);
  });
});
