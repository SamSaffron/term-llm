const DATABASE = 'term_llm_draft_blobs';
const STORE = 'attachments';
const VERSION = 1;

export interface DraftBlobRecord {
  id: string;
  draftId: string;
  blob: Blob;
  mime: string;
  size: number;
  checksum: string;
  updated: number;
}

const requestResult = <T>(request: IDBRequest<T>): Promise<T> =>
  new Promise((resolve, reject) => {
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error || new Error('IndexedDB request failed'));
  });

export class DraftBlobStore {
  private database: Promise<IDBDatabase> | null = null;

  private open(): Promise<IDBDatabase> {
    if (this.database) return this.database;
    this.database = new Promise((resolve, reject) => {
      if (!globalThis.indexedDB) {
        reject(new Error('Attachment draft storage is unavailable in this browser.'));
        return;
      }
      const request = indexedDB.open(DATABASE, VERSION);
      request.onupgradeneeded = () => {
        const db = request.result;
        if (!db.objectStoreNames.contains(STORE)) {
          const store = db.createObjectStore(STORE, { keyPath: 'id' });
          store.createIndex('draftId', 'draftId', { unique: false });
          store.createIndex('updated', 'updated', { unique: false });
        }
      };
      request.onsuccess = () => resolve(request.result);
      request.onerror = () =>
        reject(request.error || new Error('Could not open attachment drafts.'));
    });
    return this.database;
  }

  async put(record: DraftBlobRecord): Promise<void> {
    const db = await this.open();
    const transaction = db.transaction(STORE, 'readwrite');
    transaction.objectStore(STORE).put(record);
    await new Promise<void>((resolve, reject) => {
      transaction.oncomplete = () => resolve();
      transaction.onerror = () =>
        reject(transaction.error || new Error('Could not save attachment.'));
      transaction.onabort = () =>
        reject(transaction.error || new Error('Attachment save aborted.'));
    });
  }

  async get(id: string): Promise<DraftBlobRecord | null> {
    const db = await this.open();
    return (await requestResult(db.transaction(STORE).objectStore(STORE).get(id))) || null;
  }

  async delete(id: string): Promise<void> {
    const db = await this.open();
    const transaction = db.transaction(STORE, 'readwrite');
    transaction.objectStore(STORE).delete(id);
    await new Promise<void>((resolve, reject) => {
      transaction.oncomplete = () => resolve();
      transaction.onerror = () =>
        reject(transaction.error || new Error('Could not delete attachment.'));
    });
  }

  async deleteDraft(draftId: string): Promise<void> {
    const db = await this.open();
    const transaction = db.transaction(STORE, 'readwrite');
    const index = transaction.objectStore(STORE).index('draftId');
    const keys = await requestResult(index.getAllKeys(draftId));
    keys.forEach((key) => transaction.objectStore(STORE).delete(key));
    await new Promise<void>((resolve, reject) => {
      transaction.oncomplete = () => resolve();
      transaction.onerror = () =>
        reject(transaction.error || new Error('Could not clear attachments.'));
    });
  }

  async deleteOrphans(referencedIds: ReadonlySet<string>): Promise<number> {
    const db = await this.open();
    const transaction = db.transaction(STORE, 'readwrite');
    const store = transaction.objectStore(STORE);
    const records = await requestResult(store.getAll());
    let deleted = 0;
    for (const record of records) {
      if (!referencedIds.has(record.id)) {
        store.delete(record.id);
        deleted += 1;
      }
    }
    await new Promise<void>((resolve, reject) => {
      transaction.oncomplete = () => resolve();
      transaction.onerror = () =>
        reject(transaction.error || new Error('Could not reconcile attachment drafts.'));
      transaction.onabort = () =>
        reject(transaction.error || new Error('Attachment reconciliation aborted.'));
    });
    return deleted;
  }

  close(): void {
    void this.database?.then((database) => database.close()).catch(() => undefined);
    this.database = null;
  }
}

export async function blobChecksum(blob: Blob): Promise<string> {
  const bytes = await blob.arrayBuffer();
  const digest = await crypto.subtle.digest('SHA-256', bytes);
  return [...new Uint8Array(digest)].map((value) => value.toString(16).padStart(2, '0')).join('');
}

export const blobToDataURL = (blob: Blob): Promise<string> =>
  new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result || ''));
    reader.onerror = () => reject(reader.error || new Error('Could not read attachment.'));
    reader.onabort = () => reject(new DOMException('Attachment read aborted', 'AbortError'));
    reader.readAsDataURL(blob);
  });
