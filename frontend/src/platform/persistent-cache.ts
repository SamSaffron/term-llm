/**
 * Bounded, namespaced IndexedDB record cache for last-known UI state.
 *
 * Every operation degrades to a miss or a no-op: private browsing, disabled
 * storage, quota exhaustion, blocked upgrades, corrupt rows, and a stalled
 * IndexedDB must never prevent the authoritative server path from running.
 * Values are stored as JSON text so size limits are measurable and a row that
 * no longer parses is simply discarded.
 */

const DATABASE = 'term_llm_ui_cache';
const STORE = 'records';
const VERSION = 1;
/** A stalled IndexedDB (seen in some WebKit builds) must not hold startup hostage. */
const OPEN_TIMEOUT_MS = 1_500;

export interface CacheLimits {
  /** Maximum entries kept in the namespace; least recently used rows go first. */
  maxEntries: number;
  /** Maximum total UTF-16 code units of JSON text kept in the namespace. */
  maxBytes: number;
  /** Maximum size of one record; larger values are rejected rather than truncated. */
  maxRecordBytes: number;
}

interface StoredRecord {
  key: string;
  namespace: string;
  json: string;
  bytes: number;
  updated: number;
  accessed: number;
}

export interface CacheRecord<T> {
  value: T;
  updated: number;
}

export interface CacheBackend {
  get(key: string): Promise<StoredRecord | undefined>;
  put(record: StoredRecord): Promise<void>;
  touch(key: string, accessed: number): Promise<void>;
  delete(keys: string[]): Promise<void>;
  list(namespace: string): Promise<Omit<StoredRecord, 'json'>[]>;
  deleteNamespacePrefix(prefix: string): Promise<void>;
  clear(): Promise<void>;
}

const requestResult = <T>(request: IDBRequest<T>): Promise<T> =>
  new Promise((resolve, reject) => {
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error || new Error('IndexedDB request failed'));
  });

const transactionDone = (transaction: IDBTransaction): Promise<void> =>
  new Promise((resolve, reject) => {
    transaction.oncomplete = () => resolve();
    transaction.onerror = () => reject(transaction.error || new Error('IndexedDB write failed'));
    transaction.onabort = () => reject(transaction.error || new Error('IndexedDB write aborted'));
  });

export class IndexedDBBackend implements CacheBackend {
  private database: Promise<IDBDatabase> | null = null;

  private open(): Promise<IDBDatabase> {
    if (this.database) return this.database;
    const opening = new Promise<IDBDatabase>((resolve, reject) => {
      if (!globalThis.indexedDB) {
        reject(new Error('IndexedDB is unavailable'));
        return;
      }
      let settled = false;
      const fail = (error: unknown) => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        reject(error instanceof Error ? error : new Error('IndexedDB open failed'));
      };
      const timer = setTimeout(() => fail(new Error('IndexedDB open timed out')), OPEN_TIMEOUT_MS);
      let request: IDBOpenDBRequest;
      try {
        request = indexedDB.open(DATABASE, VERSION);
      } catch (error) {
        fail(error);
        return;
      }
      request.onupgradeneeded = () => {
        const db = request.result;
        // Version 1 is the first schema. Future versions may drop and recreate
        // the store: every record is a disposable cache of server state.
        if (db.objectStoreNames.contains(STORE)) db.deleteObjectStore(STORE);
        const store = db.createObjectStore(STORE, { keyPath: 'key' });
        store.createIndex('namespace', 'namespace', { unique: false });
      };
      request.onblocked = () => fail(new Error('IndexedDB upgrade blocked'));
      request.onsuccess = () => {
        const db = request.result;
        // A connection arriving after a timeout/blocked failure is never used;
        // close it rather than leak one per retry.
        if (settled) {
          db.close();
          return;
        }
        settled = true;
        clearTimeout(timer);
        const release = () => {
          db.close();
          openConnections.delete(db);
          this.database = null;
        };
        openConnections.set(db, release);
        // Let another tab upgrade or delete the database instead of blocking it.
        db.onversionchange = release;
        resolve(db);
      };
      request.onerror = () => fail(request.error);
    });
    // A failed open is retried on the next operation rather than cached forever.
    this.database = opening.catch((error: unknown) => {
      this.database = null;
      throw error;
    });
    return this.database;
  }

  async get(key: string): Promise<StoredRecord | undefined> {
    const db = await this.open();
    return (await requestResult(db.transaction(STORE).objectStore(STORE).get(key))) as
      StoredRecord | undefined;
  }

  async put(record: StoredRecord): Promise<void> {
    const db = await this.open();
    const transaction = db.transaction(STORE, 'readwrite');
    transaction.objectStore(STORE).put(record);
    await transactionDone(transaction);
  }

  async touch(key: string, accessed: number): Promise<void> {
    const db = await this.open();
    const transaction = db.transaction(STORE, 'readwrite');
    const store = transaction.objectStore(STORE);
    const current = (await requestResult(store.get(key))) as StoredRecord | undefined;
    if (current) store.put({ ...current, accessed });
    await transactionDone(transaction);
  }

  async delete(keys: string[]): Promise<void> {
    if (!keys.length) return;
    const db = await this.open();
    const transaction = db.transaction(STORE, 'readwrite');
    const store = transaction.objectStore(STORE);
    keys.forEach((key) => store.delete(key));
    await transactionDone(transaction);
  }

  async list(namespace: string): Promise<Omit<StoredRecord, 'json'>[]> {
    const db = await this.open();
    const rows = (await requestResult(
      db.transaction(STORE).objectStore(STORE).index('namespace').getAll(namespace),
    )) as StoredRecord[];
    return rows.map(({ json: _json, ...meta }) => meta);
  }

  async deleteNamespacePrefix(prefix: string): Promise<void> {
    const db = await this.open();
    const transaction = db.transaction(STORE, 'readwrite');
    const store = transaction.objectStore(STORE);
    const keys = (await requestResult(store.getAllKeys())) as string[];
    keys.filter((key) => key.startsWith(prefix)).forEach((key) => store.delete(key));
    await transactionDone(transaction);
  }

  async clear(): Promise<void> {
    const db = await this.open();
    const transaction = db.transaction(STORE, 'readwrite');
    transaction.objectStore(STORE).clear();
    await transactionDone(transaction);
  }
}

/** This page's live connections and how to release them before a delete. */
const openConnections = new Map<IDBDatabase, () => void>();

const recordKey = (namespace: string, id: string) => `${namespace}\u0000${id}`;

export class PersistentCache {
  constructor(
    private readonly backend: CacheBackend = new IndexedDBBackend(),
    private readonly onFailure: (error: unknown) => void = () => undefined,
    private readonly now: () => number = () => Date.now(),
  ) {}

  async get<T>(namespace: string, id: string): Promise<CacheRecord<T> | null> {
    const key = recordKey(namespace, id);
    try {
      const row = await this.backend.get(key);
      if (!row || row.namespace !== namespace) return null;
      let value: T;
      try {
        value = JSON.parse(row.json) as T;
      } catch {
        await this.backend.delete([key]).catch(() => undefined);
        return null;
      }
      void this.backend.touch(key, this.now()).catch(() => undefined);
      return { value, updated: row.updated };
    } catch (error) {
      this.onFailure(error);
      return null;
    }
  }

  /** Returns false when the value was too large or storage refused it. */
  async put(namespace: string, id: string, value: unknown, limits: CacheLimits): Promise<boolean> {
    let json: string;
    try {
      json = JSON.stringify(value);
    } catch (error) {
      this.onFailure(error);
      return false;
    }
    if (json.length > limits.maxRecordBytes) {
      // An oversized replacement must not leave an older copy looking current.
      await this.delete(namespace, id);
      return false;
    }
    const now = this.now();
    const record: StoredRecord = {
      key: recordKey(namespace, id),
      namespace,
      json,
      bytes: json.length,
      updated: now,
      accessed: now,
    };
    try {
      await this.evict(namespace, limits, record);
      await this.backend.put(record);
      return true;
    } catch (error) {
      // Quota pressure: shed the namespace to half its budget once, then give up.
      try {
        await this.evict(
          namespace,
          { ...limits, maxBytes: Math.floor(limits.maxBytes / 2) },
          record,
        );
        await this.backend.put(record);
        return true;
      } catch (retryError) {
        this.onFailure(retryError || error);
        return false;
      }
    }
  }

  async delete(namespace: string, id: string): Promise<void> {
    try {
      await this.backend.delete([recordKey(namespace, id)]);
    } catch (error) {
      this.onFailure(error);
    }
  }

  async entries(namespace: string): Promise<{ id: string; updated: number; bytes: number }[]> {
    try {
      const prefix = `${namespace}\u0000`;
      return (await this.backend.list(namespace)).map((row) => ({
        id: row.key.slice(prefix.length),
        updated: row.updated,
        bytes: row.bytes,
      }));
    } catch (error) {
      this.onFailure(error);
      return [];
    }
  }

  /** Removes every namespace that starts with prefix (e.g. one account scope). */
  async purge(prefix: string): Promise<void> {
    try {
      await this.backend.deleteNamespacePrefix(prefix);
    } catch (error) {
      this.onFailure(error);
    }
  }

  async clear(): Promise<void> {
    try {
      await this.backend.clear();
    } catch (error) {
      this.onFailure(error);
    }
  }

  private async evict(namespace: string, limits: CacheLimits, incoming: StoredRecord) {
    const rows = (await this.backend.list(namespace)).filter((row) => row.key !== incoming.key);
    rows.sort((left, right) => right.accessed - left.accessed);
    let bytes = incoming.bytes;
    const remove: string[] = [];
    rows.forEach((row, index) => {
      // index + 2 counts this row and the incoming record.
      if (index + 2 > limits.maxEntries || bytes + row.bytes > limits.maxBytes)
        remove.push(row.key);
      else bytes += row.bytes;
    });
    await this.backend.delete(remove);
  }
}

/** In-memory backend for tests and for browsers without IndexedDB. */
export class MemoryBackend implements CacheBackend {
  readonly rows = new Map<string, StoredRecord>();
  async get(key: string) {
    const row = this.rows.get(key);
    return row ? { ...row } : undefined;
  }
  async put(record: StoredRecord) {
    this.rows.set(record.key, { ...record });
  }
  async touch(key: string, accessed: number) {
    const row = this.rows.get(key);
    if (row) row.accessed = accessed;
  }
  async delete(keys: string[]) {
    keys.forEach((key) => this.rows.delete(key));
  }
  async list(namespace: string) {
    return [...this.rows.values()]
      .filter((row) => row.namespace === namespace)
      .map(({ json: _json, ...meta }) => meta);
  }
  async deleteNamespacePrefix(prefix: string) {
    for (const key of [...this.rows.keys()]) if (key.startsWith(prefix)) this.rows.delete(key);
  }
  async clear() {
    this.rows.clear();
  }
}

/** Deletes every persisted UI cache for this origin (Hub logout, "clear local cache"). */
export async function deletePersistentUICaches(): Promise<void> {
  if (!globalThis.indexedDB) return;
  // Release this page's connections first (backends reopen lazily afterwards)
  // so our own tab never blocks the delete.
  [...openConnections.values()].forEach((release) => release());
  await new Promise<void>((resolve) => {
    // Other tabs close on versionchange. One that never answers must not hang
    // logout; the blocked delete still completes once it closes.
    const timer = setTimeout(resolve, 3_000);
    const done = () => {
      clearTimeout(timer);
      resolve();
    };
    try {
      const request = indexedDB.deleteDatabase(DATABASE);
      request.onsuccess = done;
      request.onerror = done;
    } catch {
      done();
    }
  });
}
