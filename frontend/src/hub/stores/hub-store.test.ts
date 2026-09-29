import { afterEach, describe, expect, it, vi } from 'vitest';
import { HubAPIError, type HubClient } from '../../api/hub-client';
import type { AttentionResponse, NodeOrderResponse, NodesResponse } from '../domain/types';
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
  it('retains unchanged collections across polls and replaces only changed nodes', async () => {
    let payload = { nodes: [...nodes('alpha').nodes, ...nodes('beta').nodes] };
    const client = fakeClient({ listNodes: vi.fn(async () => structuredClone(payload)) });
    const store = new HubStore(client);
    await store.refresh();
    const firstNodes = store.nodes.value;
    const firstInbox = store.inbox.value;
    const firstRequired = store.inputRequired.value;
    const firstDelegations = store.delegations.value;
    await store.refresh('poll');
    expect(store.nodes.value).toBe(firstNodes);
    expect(store.inbox.value).toBe(firstInbox);
    expect(store.inputRequired.value).toBe(firstRequired);
    expect(store.delegations.value).toBe(firstDelegations);
    payload.nodes[1].status.reachable = false;
    await store.refresh('poll');
    expect(store.nodes.value[0]).toBe(firstNodes[0]);
    expect(store.nodes.value[1]).not.toBe(firstNodes[1]);
    expect(store.nodes.value[1].sessions).toBe(firstNodes[1].sessions);
    payload = { nodes: [payload.nodes[1], payload.nodes[0]] };
    const beforeReorder = store.nodes.value;
    await store.refresh('poll');
    expect(store.nodes.value[0]).toBe(beforeReorder[1]);
    expect(store.nodes.value[1]).toBe(beforeReorder[0]);
    store.dispose();
  });

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

describe('HubStore node order', () => {
  /** The Hub's listing: nodes in the given (saved) order. */
  const listing = (...ids: string[]): NodesResponse => ({
    nodes: ids.flatMap((id) => nodes(id).nodes),
  });
  const shown = (store: HubStore) => store.nodes.value.map((node) => node.id);
  const deferred = <T>() => {
    let resolve!: (value: T) => void;
    let reject!: (error: unknown) => void;
    const promise = new Promise<T>((done, fail) => {
      resolve = done;
      reject = fail;
    });
    return { promise, resolve, reject };
  };
  const committed = (...ids: string[]): NodeOrderResponse => ({ node_ids: ids });

  it('shows a new order at once and keeps it through listings read before it was saved', async () => {
    const listNodes = vi.fn(async () => listing('alpha', 'beta', 'gamma'));
    const save = deferred<NodeOrderResponse>();
    const client = fakeClient({ listNodes, reorderNodes: vi.fn(() => save.promise) });
    const store = new HubStore(client);
    await store.refresh('initial');
    expect(shown(store)).toEqual(['alpha', 'beta', 'gamma']);

    // A poll is in flight when the user moves Gamma first.
    const stale = deferred<NodesResponse>();
    listNodes.mockReturnValueOnce(stale.promise);
    const poll = store.refresh('poll');
    const saving = store.reorderNodes(['gamma', 'alpha', 'beta']);
    expect(shown(store)).toEqual(['gamma', 'alpha', 'beta']);
    await vi.waitFor(() =>
      expect(client.reorderNodes).toHaveBeenCalledWith(['gamma', 'alpha', 'beta']),
    );

    // That poll read the old order, so it keeps the order shown, as does one
    // read while the save is still on its way to the Hub.
    stale.resolve(listing('alpha', 'beta', 'gamma'));
    await poll;
    expect(shown(store)).toEqual(['gamma', 'alpha', 'beta']);
    await store.refresh('poll');
    expect(shown(store)).toEqual(['gamma', 'alpha', 'beta']);

    save.resolve(committed('gamma', 'alpha', 'beta'));
    await saving;
    expect(shown(store)).toEqual(['gamma', 'alpha', 'beta']);

    // A poll can start after the optimistic move but read before the PATCH
    // commits, then arrive after the save resolves. It must not undo the move.
    const secondSave = deferred<NodeOrderResponse>();
    (client.reorderNodes as ReturnType<typeof vi.fn>).mockReturnValueOnce(secondSave.promise);
    const second = store.reorderNodes(['alpha', 'gamma', 'beta']);
    await vi.waitFor(() => expect(client.reorderNodes).toHaveBeenCalledTimes(2));
    const delayed = deferred<NodesResponse>();
    listNodes.mockReturnValueOnce(delayed.promise);
    const racingPoll = store.refresh('poll');
    secondSave.resolve(committed('alpha', 'gamma', 'beta'));
    await second;
    delayed.resolve(listing('gamma', 'alpha', 'beta'));
    await racingPoll;
    expect(shown(store)).toEqual(['alpha', 'gamma', 'beta']);

    // Later listings are the Hub's saved order, which new nodes join at the end.
    listNodes.mockResolvedValue(listing('alpha', 'gamma', 'delta'));
    await store.refresh('poll');
    expect(shown(store)).toEqual(['alpha', 'gamma', 'delta']);
    store.dispose();
  });

  it('keeps a listing read before a save in the shown order, with its new nodes last', async () => {
    const listNodes = vi.fn(async () => listing('alpha', 'beta', 'gamma'));
    const client = fakeClient({
      listNodes,
      reorderNodes: vi.fn(async (ids: string[]) => committed(...ids)),
    });
    const store = new HubStore(client);
    await store.refresh('initial');
    const stale = deferred<NodesResponse>();
    listNodes.mockReturnValueOnce(stale.promise);
    const poll = store.refresh('poll');
    await store.reorderNodes(['beta', 'gamma', 'alpha']);
    // Read before the move, the listing drops Gamma and adds Delta.
    stale.resolve(listing('alpha', 'delta', 'beta'));
    await poll;
    expect(shown(store)).toEqual(['beta', 'alpha', 'delta']);
    store.dispose();
  });

  it('coalesces rapid moves, saving one request at a time and skipping superseded orders', async () => {
    const first = deferred<NodeOrderResponse>();
    const reorderNodes = vi
      .fn()
      .mockReturnValueOnce(first.promise)
      .mockImplementation(async (ids: string[]) => committed(...ids));
    const client = fakeClient({
      listNodes: vi.fn(async () => listing('alpha', 'beta', 'gamma')),
      reorderNodes,
    });
    const store = new HubStore(client);
    await store.refresh('initial');

    const saves = [store.reorderNodes(['beta', 'alpha', 'gamma'])];
    await vi.waitFor(() => expect(reorderNodes).toHaveBeenCalledOnce());
    // Two more moves while the first is on its way: they wait behind it, and
    // the newest replaces the one before it.
    saves.push(
      store.reorderNodes(['beta', 'gamma', 'alpha']),
      store.reorderNodes(['gamma', 'beta', 'alpha']),
    );
    expect(shown(store)).toEqual(['gamma', 'beta', 'alpha']);
    await Promise.resolve();
    expect(reorderNodes).toHaveBeenCalledOnce();
    // The second move acts on Gamma/Alpha, and the third on Gamma/Beta.
    // Neither subset covers the other, so both have to be sent in sequence.
    first.resolve(committed('beta', 'alpha', 'gamma'));
    await Promise.all(saves);
    expect(reorderNodes.mock.calls).toEqual([
      [['beta', 'alpha']],
      [['gamma', 'alpha']],
      [['gamma', 'beta']],
    ]);
    expect(shown(store)).toEqual(['gamma', 'beta', 'alpha']);
    store.dispose();
  });

  it('sends only the moved span so a separate node-auth move is preserved', async () => {
    let server = ['alpha', 'beta', 'gamma', 'delta'];
    const reorderNodes = vi.fn(async (listed: string[]) => {
      // A different agent changed nodes outside the span before our PATCH.
      server = ['alpha', 'beta', 'delta', 'gamma'];
      const slots = listed.map((id) => server.indexOf(id)).sort((a, b) => a - b);
      listed.forEach((id, index) => (server[slots[index]] = id));
      return committed(...server);
    });
    const store = new HubStore(
      fakeClient({ listNodes: vi.fn(async () => listing(...server)), reorderNodes }),
    );
    await store.refresh('initial');
    await store.reorderNodes(['beta', 'alpha', 'gamma', 'delta']);
    expect(reorderNodes).toHaveBeenCalledWith(['beta', 'alpha']);
    expect(shown(store)).toEqual(['beta', 'alpha', 'delta', 'gamma']);
    store.dispose();
  });

  it('clears manual refresh state when a failed save replaces the in-flight refresh', async () => {
    const pending = deferred<NodesResponse>();
    const listNodes = vi.fn(async () => listing('alpha', 'beta'));
    const store = new HubStore(
      fakeClient({
        listNodes,
        reorderNodes: vi.fn(async () => {
          throw new HubAPIError(500, 'not saved');
        }),
      }),
    );
    await store.refresh('initial');
    listNodes.mockReturnValueOnce(pending.promise);
    const manual = store.refresh('manual');
    expect(store.refreshing.value).toBe(true);
    const failure = store.reorderNodes(['beta', 'alpha']);
    await expect(failure).rejects.toThrow('not saved');
    expect(store.refreshing.value).toBe(false);
    pending.resolve(listing('alpha', 'beta'));
    await manual;
    expect(shown(store)).toEqual(['alpha', 'beta']);
    store.dispose();
  });

  it('restores the previous order, shows the Hub order, and explains a failed save', async () => {
    const listNodes = vi.fn(async () => listing('alpha', 'beta', 'gamma'));
    const reorderNodes = vi
      .fn()
      .mockRejectedValueOnce(new HubAPIError(500, 'failed to save the node order'))
      .mockRejectedValueOnce(new HubAPIError(404, 'a listed node was not found', 'node_not_found'));
    const client = fakeClient({ listNodes, reorderNodes });
    const store = new HubStore(client);
    await store.refresh('initial');

    // Another browser saved an order meanwhile: the failure shows it.
    listNodes.mockResolvedValue(listing('beta', 'alpha', 'gamma'));
    const failed = store.reorderNodes(['gamma', 'alpha', 'beta']);
    expect(shown(store)).toEqual(['gamma', 'alpha', 'beta']);
    await expect(failed).rejects.toThrow(
      'Couldn’t save the node order: failed to save the node order',
    );
    expect(shown(store)).toEqual(['beta', 'alpha', 'gamma']);
    expect(listNodes).toHaveBeenCalledTimes(2);

    // A node the Hub no longer lists makes the save fail as a conflict.
    listNodes.mockResolvedValue(listing('beta', 'gamma'));
    const conflict = store.reorderNodes(['gamma', 'alpha', 'beta']);
    await expect(conflict).rejects.toThrow('Nodes changed elsewhere; showing the latest order.');
    expect(shown(store)).toEqual(['beta', 'gamma']);

    await conflict.catch((error) => store.reportNodeOrderError(error));
    expect(store.nodeError.value).toBe('Nodes changed elsewhere; showing the latest order.');
    await store.refresh('poll');
    expect(store.nodeError.value).toBe('');
    store.dispose();
    await store.reorderNodes(['gamma', 'beta']);
    expect(reorderNodes).toHaveBeenCalledTimes(2);
  });

  it('follows an order saved elsewhere on the next poll and keeps unchanged cards', async () => {
    const listNodes = vi.fn(async () => listing('alpha', 'beta', 'gamma'));
    const client = fakeClient({ listNodes });
    const store = new HubStore(client);
    await store.refresh('initial');
    const [alpha, beta, gamma] = store.nodes.value;
    // Another browser, or a node with its own token, moved Gamma first.
    listNodes.mockResolvedValue(listing('gamma', 'alpha', 'beta'));
    await store.refresh('poll');
    expect(store.nodes.value).toEqual([gamma, alpha, beta]);
    expect(store.nodes.value[0]).toBe(gamma);
    expect(store.nodes.value[1]).toBe(alpha);
    expect(store.nodes.value[2]).toBe(beta);
    store.dispose();
  });
});
