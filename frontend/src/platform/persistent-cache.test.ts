import { afterEach, describe, expect, it, vi } from 'vitest';
import { IDBFactory } from 'fake-indexeddb';
import {
  IndexedDBBackend,
  MemoryBackend,
  PersistentCache,
  deletePersistentUICaches,
  type CacheBackend,
  type CacheLimits,
} from './persistent-cache';

const limits: CacheLimits = { maxEntries: 3, maxBytes: 100, maxRecordBytes: 100 };
const key = (namespace: string, id: string) => `${namespace}\u0000${id}`;

afterEach(() => vi.unstubAllGlobals());

describe('PersistentCache', () => {
  it.each(['entries', 'bytes'] as const)(
    'evicts the least recently accessed record by %s',
    async (budget) => {
      const backend = new MemoryBackend();
      let now = 0;
      const cache = new PersistentCache(backend, vi.fn(), () => ++now);
      const bounded = {
        ...limits,
        ...(budget === 'entries' ? { maxEntries: 2 } : { maxBytes: 8 }),
      };
      await cache.put('scope', 'a', 'aa', bounded);
      await cache.put('scope', 'b', 'bb', bounded);
      expect(await cache.get('scope', 'a')).toMatchObject({ value: 'aa', updated: 1 });
      await cache.put('scope', 'c', 'cc', bounded);
      expect((await cache.entries('scope')).map((row) => row.id).sort()).toEqual(['a', 'c']);
      expect(await cache.get('scope', 'b')).toBeNull();
    },
  );

  it('rejects an oversized replacement and removes its previous copy', async () => {
    const backend = new MemoryBackend();
    const cache = new PersistentCache(backend);
    await cache.put('scope', 'a', 'old', limits);
    expect(await cache.put('scope', 'a', 'x'.repeat(100), limits)).toBe(false);
    expect(await cache.get('scope', 'a')).toBeNull();
    expect(backend.rows.size).toBe(0);
  });

  it('discards corrupt JSON instead of throwing', async () => {
    const backend = new MemoryBackend();
    const cache = new PersistentCache(backend);
    await cache.put('scope', 'a', 'old', limits);
    backend.rows.get(key('scope', 'a'))!.json = '{broken';
    expect(await cache.get('scope', 'a')).toBeNull();
    expect(backend.rows.has(key('scope', 'a'))).toBe(false);
  });

  it('degrades backend failures to misses and no-ops with diagnostics', async () => {
    const backend = new MemoryBackend();
    for (const method of [
      'get',
      'put',
      'touch',
      'delete',
      'list',
      'deleteNamespacePrefix',
      'clear',
    ] as const)
      vi.spyOn(backend, method).mockRejectedValue(new Error('Storage unavailable'));
    const failure = vi.fn();
    const cache = new PersistentCache(backend, failure);
    expect(await cache.get('scope', 'a')).toBeNull();
    expect(await cache.put('scope', 'a', {}, limits)).toBe(false);
    expect(await cache.entries('scope')).toEqual([]);
    await cache.delete('scope', 'a');
    await cache.purge('scope');
    await cache.clear();
    expect(failure).toHaveBeenCalledTimes(6);
  });

  it('purges only namespaces under the requested scope prefix', async () => {
    const cache = new PersistentCache(new MemoryBackend());
    for (const namespace of ['account|sessions', 'account|discovery', 'account-other|sessions'])
      await cache.put(namespace, 'a', namespace, limits);
    await cache.purge('account|');
    expect(await cache.entries('account|sessions')).toEqual([]);
    expect(await cache.entries('account|discovery')).toEqual([]);
    expect(await cache.get('account-other|sessions', 'a')).not.toBeNull();
  });

  it('halves the byte budget and retries once after quota failure', async () => {
    const backend = new MemoryBackend();
    const failure = vi.fn();
    const cache = new PersistentCache(
      backend,
      failure,
      (() => {
        let now = 0;
        return () => ++now;
      })(),
    );
    const budget = { ...limits, maxEntries: 10, maxBytes: 24 };
    for (const id of ['a', 'b', 'c', 'd']) await cache.put('scope', id, id.repeat(4), budget);
    const originalPut = backend.put.bind(backend);
    const put = vi
      .spyOn(backend, 'put')
      .mockRejectedValueOnce(new DOMException('full', 'QuotaExceededError'))
      .mockImplementation(originalPut);
    expect(await cache.put('scope', 'e', 'eeee', budget)).toBe(true);
    expect(put).toHaveBeenCalledTimes(2);
    expect((await cache.entries('scope')).map((row) => row.id).sort()).toEqual(['d', 'e']);
    expect(failure).not.toHaveBeenCalled();
  });

  it('rejects unserializable input without contacting the backend', async () => {
    const backend = new MemoryBackend();
    const failure = vi.fn();
    const put = vi.spyOn(backend, 'put');
    const cache = new PersistentCache(backend, failure);
    expect(await cache.put('scope', 'a', 1n, limits)).toBe(false);
    expect(failure).toHaveBeenCalledOnce();
    expect(put).not.toHaveBeenCalled();
  });
});

describe('IndexedDBBackend', () => {
  it('round trips rows, touches, lists, deletes by prefix, and clears', async () => {
    vi.stubGlobal('indexedDB', new IDBFactory());
    const backend = new IndexedDBBackend();
    const row: Parameters<CacheBackend['put']>[0] = {
      key: key('account|sessions', 'a'),
      namespace: 'account|sessions',
      json: '{"text":"saved"}',
      bytes: 16,
      updated: 10,
      accessed: 10,
    };
    await backend.put(row);
    expect(await backend.get(row.key)).toEqual(row);
    await backend.touch(row.key, 20);
    expect(await backend.list(row.namespace)).toEqual([
      {
        key: row.key,
        namespace: row.namespace,
        bytes: row.bytes,
        updated: row.updated,
        accessed: 20,
      },
    ]);
    await backend.put({
      ...row,
      key: key('account|discovery', 'b'),
      namespace: 'account|discovery',
    });
    await backend.put({ ...row, key: key('other|sessions', 'a'), namespace: 'other|sessions' });
    await backend.deleteNamespacePrefix('account|');
    expect(await backend.get(row.key)).toBeUndefined();
    expect(await backend.list('account|discovery')).toEqual([]);
    expect(await backend.list('other|sessions')).toHaveLength(1);
    await backend.delete([key('other|sessions', 'a')]);
    expect(await backend.list('other|sessions')).toEqual([]);
    await backend.put(row);
    await backend.clear();
    expect(await backend.get(row.key)).toBeUndefined();
    await deletePersistentUICaches();
  });

  it('treats unavailable IndexedDB as a miss and retries when it becomes available', async () => {
    vi.stubGlobal('indexedDB', undefined);
    const failure = vi.fn();
    const cache = new PersistentCache(new IndexedDBBackend(), failure);
    expect(await cache.get('scope', 'a')).toBeNull();
    expect(await cache.put('scope', 'a', {}, limits)).toBe(false);
    expect(failure).toHaveBeenCalledTimes(2);
    await deletePersistentUICaches();
    vi.stubGlobal('indexedDB', new IDBFactory());
    expect(await cache.put('scope', 'a', { text: 'available' }, limits)).toBe(true);
    expect(await cache.get('scope', 'a')).toMatchObject({ value: { text: 'available' } });
    await deletePersistentUICaches();
  });
  it('deletes the database while this page holds a connection, then reopens lazily', async () => {
    vi.stubGlobal('indexedDB', new IDBFactory());
    const cache = new PersistentCache(new IndexedDBBackend());
    expect(await cache.put('scope', 'a', { text: 'private' }, limits)).toBe(true);
    // Our own open connection must not block (or delay) the delete.
    await deletePersistentUICaches();
    expect(await cache.get('scope', 'a')).toBeNull();
    expect(await cache.put('scope', 'b', { text: 'after' }, limits)).toBe(true);
    expect(await cache.get('scope', 'b')).toMatchObject({ value: { text: 'after' } });
    await deletePersistentUICaches();
  });
});
