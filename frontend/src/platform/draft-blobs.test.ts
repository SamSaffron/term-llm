import { describe, expect, it } from 'vitest';
import { blobChecksum, DraftBlobStore } from './draft-blobs';

describe('DraftBlobStore', () => {
  it('persists, restores, and deletes prepared attachment blobs', async () => {
    const store = new DraftBlobStore();
    const blob = new Blob(['hello'], { type: 'text/plain' });
    const checksum = await blobChecksum(blob);
    await store.put({
      id: 'a1',
      draftId: 'd1',
      blob,
      mime: blob.type,
      size: blob.size,
      checksum,
      updated: 1,
    });

    const restored = await store.get('a1');
    expect(restored).toMatchObject({ id: 'a1', draftId: 'd1', size: 5, checksum });

    await store.put({
      id: 'orphan',
      draftId: 'd2',
      blob,
      mime: blob.type,
      size: blob.size,
      checksum,
      updated: 2,
    });
    expect(await store.deleteOrphans(new Set(['a1']))).toBe(1);
    expect(await store.get('a1')).not.toBeNull();
    expect(await store.get('orphan')).toBeNull();

    await store.deleteDraft('d1');
    expect(await store.get('a1')).toBeNull();
    store.close();
  });
});
