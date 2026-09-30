import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/preact';
import * as persistence from '../../platform/persistent-cache';
import { HubAPIError, HubClient } from '../../api/hub-client';
import { HubApp } from '../components/HubApp';
import type { HubConfig } from '../config';
import type { AttentionResponse, DelegationsResponse, NodesResponse } from '../domain/types';
import { HubStore } from './hub-store';
import {
  HubCache,
  cacheNodes,
  cacheAttention,
  cacheDelegations,
  HUB_CACHE_LIMITS,
  HUB_CACHE_MAX_AGE,
} from './hub-cache';

const config: HubConfig = {
  page: 'dashboard',
  authMode: 'bearer',
  basePath: '/hub',
  cacheScope: 'a'.repeat(24),
  cacheDisplayAllowed: true,
  canAddNodes: false,
  passkeyAuth: false,
  invalidToken: false,
  formAction: '/hub/',
};
const nodes = (id = 'alpha'): NodesResponse => ({
  nodes: [
    {
      id,
      name: id,
      source: 'config',
      connection: 'direct',
      url: 'https://node.test/?token=secret&X-Amz-Signature=signed',
      base_path: '',
      proxy_path: `/hub/node/${id}/`,
      new_session_path: `/hub/node/${id}/?new=1&signature=secret`,
      has_token: true,
      status: { reachable: true, state: 'ok', latency_ms: 1, details: { token: 'secret' } },
      sessions: {
        count_label: '1 session',
        active_count: 1,
        resume_path: '/hub/node/alpha/chat/1?token=secret',
      },
    },
  ],
});
const attention = (title = 'Cached review'): AttentionResponse => ({
  total_running: 0,
  total_input_required: 1,
  total_unseen: 1,
  nodes: [],
  has_more: false,
  input_required: [
    {
      node_id: 'alpha',
      node_name: 'Alpha',
      session_id: 'waiting',
      title: 'Cached question',
      pending_interaction_count: 1,
      resume_path: '/hub/node/alpha/chat/2?token=secret',
    },
  ],
  inbox: [
    {
      node_id: 'alpha',
      node_name: 'Alpha',
      session_id: 's1',
      title,
      outcome: 'succeeded',
      attention_seq: 1,
      resume_path: '/hub/node/alpha/chat/1?sig=secret',
    },
  ],
});
const delegations = (): DelegationsResponse => ({
  delegations: [
    {
      id: 'd1',
      origin_node: 'alpha',
      target_node: 'alpha',
      status: 'running',
      depth: 1,
      created_at: '2026-01-01',
      updated_at: '2026-01-01',
      prompt: 'secret prompt',
      response: 'https://artifact.test/?token=secret',
      error: 'secret diagnostic',
    },
  ],
});
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((yes, no) => {
    resolve = yes;
    reject = no;
  });
  return { promise, resolve, reject };
}
function pendingClient() {
  const nodeRead = deferred<NodesResponse>();
  const attentionRead = deferred<AttentionResponse>();
  const delegationRead = deferred<DelegationsResponse>();
  const client = {
    config,
    listNodes: vi.fn(() => nodeRead.promise),
    listAttention: vi.fn(() => attentionRead.promise),
    listDelegations: vi.fn(() => delegationRead.promise),
  } as unknown as HubClient;
  return { client, nodeRead, attentionRead, delegationRead };
}
function cacheSetup(scope = config.cacheScope) {
  const backend = new persistence.MemoryBackend();
  const persistent = new persistence.PersistentCache(backend);
  const cache = new HubCache({ ...config, cacheScope: scope }, persistent);
  return { backend, persistent, cache };
}
async function seed(cache: HubCache, timestamp = Date.now()) {
  await cache.write('nodes', cacheNodes(nodes().nodes), timestamp);
  const data = attention();
  await cache.write(
    'attention',
    cacheAttention({
      inputRequired: data.input_required,
      inbox: data.inbox,
      totalInputRequired: 1,
      totalUnseen: 1,
      hasMore: false,
    }),
    timestamp,
  );
  await cache.write('delegations', cacheDelegations(delegations().delegations), timestamp);
}
const stores: HubStore[] = [];
function createStore(client: HubClient, cache: HubCache) {
  const store = new HubStore(client, undefined, { cache });
  stores.push(store);
  return store;
}
const flush = () => vi.advanceTimersByTimeAsync(0);

beforeEach(() => vi.useFakeTimers());
afterEach(() => {
  cleanup();
  stores.splice(0).forEach((store) => store.dispose());
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe('Hub startup cache', () => {
  it('applies attention while nodes are pending; delegation failure blocks neither section', async () => {
    const { client, nodeRead, attentionRead, delegationRead } = pendingClient();
    const store = createStore(client, cacheSetup('').cache);
    const refresh = store.refresh('manual');
    attentionRead.resolve(attention('Fresh review'));
    await flush();
    expect(store.inbox.value[0].title).toBe('Fresh review');
    expect(store.attentionVerified.value).toBe(true);
    expect(store.nodesVerified.value).toBe(false);
    expect(store.initialLoading.value).toBe(true);
    expect(store.refreshing.value).toBe(true);
    delegationRead.reject(new Error('delegation failed'));
    await flush();
    expect(store.delegationError.value).toBe('delegation failed');
    nodeRead.resolve(nodes());
    await refresh;
    expect(store.nodesVerified.value).toBe(true);
    expect(store.refreshing.value).toBe(false);
    expect(store.initialLoading.value).toBe(false);
  });

  it('shows cached navigation promptly, with no online/running indicator until each section verifies', async () => {
    const { cache } = cacheSetup();
    await seed(cache);
    const { client, nodeRead, attentionRead, delegationRead } = pendingClient();
    const store = createStore(client, cache);
    const view = render(
      <HubApp config={config} store={store} clipboard={{ writeText: vi.fn() }} />,
    );
    await flush();
    expect(screen.queryByText('Loading Hub…')).not.toBeInTheDocument();
    expect(screen.getByText('Cached review')).toBeVisible();
    expect(screen.getByText('Cached question')).toBeVisible();
    expect(screen.getByRole('link', { name: 'New' })).toHaveAttribute(
      'href',
      '/hub/node/alpha/?new=1',
    );
    expect(screen.getByRole('link', { name: 'Resume' })).toHaveAttribute(
      'href',
      '/hub/node/alpha/chat/1',
    );
    expect(view.container.querySelector('.status-dot.ok')).toBeNull();
    expect(view.container.querySelector('.status-running')).toBeNull();
    expect(store.reachableCount.value).toBe(0);
    expect(store.activeSessionCount.value).toBe(0);
    expect(store.lastKnown.value).toBe(true);
    attentionRead.resolve(attention('Fresh review'));
    await flush();
    expect(screen.queryByText('Cached review')).not.toBeInTheDocument();
    expect(screen.getByText('Fresh review')).toBeVisible();
    expect(store.lastKnown.value).toBe(true);
    nodeRead.resolve(nodes());
    delegationRead.resolve({ delegations: [] });
    await flush();
    expect(view.container.querySelector('.status-dot.ok')).not.toBeNull();
    expect(store.lastKnown.value).toBe(false);
    expect(store.reachableCount.value).toBe(1);
  });

  it('waits for an authenticated success when shell display permission is missing', async () => {
    const { cache } = cacheSetup();
    await seed(cache);
    const { client, attentionRead } = pendingClient();
    Object.assign(client, { config: { ...config, cacheDisplayAllowed: false } });
    const store = createStore(client, cache);
    store.start();
    await flush();
    expect(store.nodes.value).toEqual([]);
    attentionRead.resolve(attention('Verified'));
    await flush();
    expect(store.nodes.value[0].id).toBe('alpha');
    expect(store.nodesVerified.value).toBe(false);
    expect(store.inbox.value[0].title).toBe('Verified');
  });

  it('ignores older generation responses in both signals and persisted sections', async () => {
    const { cache } = cacheSetup();
    const { client, nodeRead, attentionRead, delegationRead } = pendingClient();
    const store = createStore(client, cache);
    const older = store.refresh('initial');
    vi.mocked(client.listNodes).mockResolvedValueOnce(nodes());
    vi.mocked(client.listAttention).mockResolvedValueOnce(attention('Newer'));
    vi.mocked(client.listDelegations).mockResolvedValueOnce({ delegations: [] });
    await store.refresh('manual');
    await vi.advanceTimersByTimeAsync(200);
    nodeRead.resolve(nodes('old'));
    attentionRead.resolve(attention('Older'));
    delegationRead.resolve(delegations());
    await older;
    await vi.advanceTimersByTimeAsync(200);
    expect(store.nodes.value[0].id).toBe('alpha');
    expect(store.inbox.value[0].title).toBe('Newer');
    expect((await cache.read('nodes'))?.data[0].id).toBe('alpha');
    expect((await cache.read('attention'))?.data.inbox[0].title).toBe('Newer');
    expect((await cache.read('delegations'))?.data).toEqual([]);
  });

  it('keeps an ordering edit made during an in-flight node read in both display and cache', async () => {
    const { cache } = cacheSetup();
    const { client, nodeRead, attentionRead, delegationRead } = pendingClient();
    const listing = { nodes: [...nodes('alpha').nodes, ...nodes('beta').nodes] };
    vi.mocked(client.listNodes).mockResolvedValueOnce(listing);
    vi.mocked(client.listAttention).mockResolvedValueOnce(attention());
    vi.mocked(client.listDelegations).mockResolvedValueOnce({ delegations: [] });
    client.reorderNodes = vi.fn(async () => ({ node_ids: ['beta', 'alpha'] }));
    const store = createStore(client, cache);
    await store.refresh('initial');
    const pending = store.refresh('manual');
    await store.reorderNodes(['beta', 'alpha']);
    nodeRead.resolve(listing);
    attentionRead.resolve(attention());
    delegationRead.resolve({ delegations: [] });
    await pending;
    await vi.advanceTimersByTimeAsync(200);
    expect(store.nodes.value.map((node) => node.id)).toEqual(['beta', 'alpha']);
    expect((await cache.read('nodes'))?.data.map((node) => node.id)).toEqual(['beta', 'alpha']);
  });

  it.each([401, 403])(
    'purges and hides its scope on %s, without purging another Hub scope',
    async (status) => {
      const { cache, persistent, backend } = cacheSetup();
      const other = new HubCache({ ...config, cacheScope: 'b'.repeat(24) }, persistent);
      await seed(cache);
      await seed(other);
      const { client, nodeRead, attentionRead } = pendingClient();
      const store = createStore(client, cache);
      store.start();
      await flush();
      // One verified response queues a debounced write before auth is lost.
      attentionRead.resolve(attention('Authenticated'));
      await flush();
      nodeRead.reject(new HubAPIError(status, 'expired'));
      await flush();
      await vi.advanceTimersByTimeAsync(500);
      expect(store.nodes.value).toEqual([]);
      expect(store.inbox.value).toEqual([]);
      expect([...backend.rows.values()].some((row) => row.namespace === cache.namespace)).toBe(
        false,
      );
      expect((await other.read('nodes'))?.data[0].id).toBe('alpha');
    },
  );

  it('purges on mutation authorization failures before passkey navigation', async () => {
    const { cache, backend } = cacheSetup();
    await seed(cache);
    const navigate = vi.fn(() => expect(backend.rows.size).toBe(0));
    const client = new HubClient(
      { ...config, authMode: 'passkey' },
      {
        fetch: vi.fn(
          async () =>
            new Response('{}', { status: 401, headers: { 'Content-Type': 'application/json' } }),
        ),
        navigate,
      },
    );
    createStore(client, cache);
    await expect(client.removeNode('alpha')).rejects.toMatchObject({ status: 401 });
    expect(navigate).toHaveBeenCalledOnce();
  });

  it('clears every persistent UI cache on successful logout, including proxied chats', async () => {
    const clear = vi.spyOn(persistence, 'deletePersistentUICaches').mockResolvedValue();
    const { cache, backend } = cacheSetup();
    await seed(cache);
    const { client } = pendingClient();
    client.logout = vi.fn(async () => ({ redirect: '/hub/auth/login' }));
    const store = createStore(client, cache);
    expect(await store.signOut()).toBe('/hub/auth/login');
    expect(clear).toHaveBeenCalledOnce();
    expect(backend.rows.size).toBe(0);
  });

  it('removes vanished nodes and their cached summaries even when attention/delegations fail', async () => {
    const { cache } = cacheSetup();
    await seed(cache);
    const { client, nodeRead, attentionRead, delegationRead } = pendingClient();
    const store = createStore(client, cache);
    store.start();
    await flush();
    nodeRead.resolve({ nodes: [] });
    attentionRead.reject(new Error('offline'));
    delegationRead.reject(new Error('offline'));
    await vi.advanceTimersByTimeAsync(200);
    expect((await cache.read('nodes'))?.data).toEqual([]);
    expect((await cache.read('attention'))?.data.inbox).toEqual([]);
    expect((await cache.read('attention'))?.data.inputRequired).toEqual([]);
    expect((await cache.read('delegations'))?.data).toEqual([]);
    expect(store.inbox.value).toEqual([]);
  });

  it('isolates origin, base path and cache scope, and disables persistence without a scope', async () => {
    const { cache, persistent, backend } = cacheSetup();
    await seed(cache);
    for (const isolated of [
      new HubCache({ ...config, cacheScope: 'b'.repeat(24) }, persistent),
      new HubCache({ ...config, basePath: '/other' }, persistent),
      new HubCache(config, persistent, 'https://other.test'),
      new HubCache({ basePath: '/hub' }, persistent),
    ]) {
      expect(await isolated.read('nodes')).toBeNull();
    }
    const disabled = new HubCache({ basePath: '/hub' }, persistent);
    await disabled.write('nodes', [], Date.now());
    expect(backend.rows.size).toBe(3);
  });

  it.each(['schema', 'corrupt-json', 'malformed', 'expired', 'throwing'])(
    'falls back to normal startup for %s records/backends',
    async (failure) => {
      const { cache, backend } = cacheSetup();
      await seed(cache, failure === 'expired' ? Date.now() - HUB_CACHE_MAX_AGE - 1 : Date.now());
      for (const row of backend.rows.values()) {
        if (failure === 'schema')
          row.json = JSON.stringify({ schema: 999, timestamp: Date.now(), data: [] });
        if (failure === 'corrupt-json') row.json = '{';
        if (failure === 'malformed')
          row.json = JSON.stringify({
            schema: 1,
            timestamp: Date.now(),
            data: [{ id: 'invalid' }],
          });
      }
      if (failure === 'throwing')
        vi.spyOn(backend, 'get').mockRejectedValue(new Error('storage denied'));
      const { client, nodeRead, attentionRead, delegationRead } = pendingClient();
      const store = createStore(client, cache);
      store.start();
      await flush();
      expect(store.hasDisplayData.value).toBe(false);
      expect(store.initialLoading.value).toBe(true);
      nodeRead.resolve(nodes());
      attentionRead.resolve(attention('Fresh'));
      delegationRead.resolve({ delegations: [] });
      await flush();
      expect(store.nodesVerified.value).toBe(true);
      expect(store.inbox.value[0].title).toBe('Fresh');
      expect(store.initialLoading.value).toBe(false);
    },
  );

  it('bounds summaries and strips credential/signed URL and delegation body fields', async () => {
    const { cache, backend } = cacheSetup();
    await seed(cache);
    const json = [...backend.rows.values()].map((row) => row.json).join('');
    expect(json).not.toContain('secret');
    expect(json).not.toContain('Signature');
    expect(json).not.toContain('signed');
    expect(json).not.toContain('details');
    expect(json).not.toContain('response');
    const raw = attention();
    const summary = cacheAttention({
      inputRequired: Array(80).fill(raw.input_required[0]),
      inbox: Array(80).fill(raw.inbox[0]),
      totalInputRequired: 80,
      totalUnseen: 80,
      hasMore: false,
    });
    expect(summary.inputRequired).toHaveLength(50);
    expect(summary.inbox).toHaveLength(50);
    expect(summary.hasMore).toBe(true);
    expect(cacheNodes(Array(300).fill(nodes().nodes[0]))).toHaveLength(200);
    expect(cacheDelegations(Array(100).fill(delegations().delegations[0]))).toHaveLength(50);
    expect(HUB_CACHE_LIMITS.maxEntries).toBe(3);
    expect(HUB_CACHE_LIMITS.maxBytes).toBe(384 * 1_024);
  });
});
