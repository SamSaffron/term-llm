import { Blob as StructuredCloneBlob } from 'node:buffer';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { UploadControls } from '../api/client';
import { DEFAULT_ATTACHMENT_POLICY } from '../domain/attachments';
import * as blobs from '../platform/draft-blobs';
import { readDrafts, saveDraft } from '../platform/storage';
import { AppStore } from './app-store';
import { testConfig, testSession } from './store-test-fixtures';

const uploadID = (index: number) => `upl_${String(index).padStart(43, 'a')}`;
const result = (index: number) => ({
  upload_id: uploadID(index),
  filename: 'image.png',
  media_type: 'image/png',
  size_bytes: 4,
});
const storeWithRefs = () => {
  const store = new AppStore(testConfig);
  store.sessions.value = [testSession()];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  store.composer.applyAttachmentPolicy({ ...DEFAULT_ATTACHMENT_POLICY, uploadRefs: true });
  return store;
};
const image = (index: number) => new File(['data'], `photo ${index}.png`, { type: 'image/png' });

beforeEach(() => {
  localStorage.clear();
  vi.spyOn(blobs, 'blobChecksum').mockResolvedValue('checksum');
  vi.stubGlobal(
    'Image',
    class {
      naturalWidth = 1400;
      naturalHeight = 1400;
      onload: (() => void) | null = () => {};
      set src(_value: string) {
        queueMicrotask(() => this.onload?.());
      }
    },
  );
  URL.createObjectURL = vi.fn(() => 'blob:preview');
  URL.revokeObjectURL = vi.fn();
});
afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('upload reference attachments', () => {
  it('uploads six originals with only two in flight, progress until finalization, and sends ordered refs without base64', async () => {
    const store = storeWithRefs();
    const base64 = vi.spyOn(blobs, 'blobToDataURL');
    const pending: Array<{
      filename: string;
      controls: UploadControls;
      resolve: (value: ReturnType<typeof result>) => void;
    }> = [];
    let active = 0;
    let maximum = 0;
    store.endpoints.uploadAttachment = vi.fn(
      (_blob, filename, controls = {}) =>
        new Promise<ReturnType<typeof result>>((resolve) => {
          active += 1;
          maximum = Math.max(maximum, active);
          pending.push({
            filename,
            controls,
            resolve: (value) => {
              active -= 1;
              resolve(value);
            },
          });
        }),
    );
    try {
      store.addAttachments(Array.from({ length: 6 }, (_, index) => image(index)));
      await vi.waitFor(() => expect(pending).toHaveLength(2));
      pending[0].controls.onProgress?.(4, 4);
      expect(
        store.attachments.value.find((entry) => entry.name === pending[0].filename),
      ).toMatchObject({ status: 'preparing', progress: 0.99 });
      for (let index = 0; index < 6; index += 1) {
        await vi.waitFor(() => expect(pending.length).toBeGreaterThan(index));
        pending[index].resolve(result(Number(pending[index].filename.match(/\d+/)![0])));
      }
      await vi.waitFor(() =>
        expect(store.attachments.value.every((entry) => entry.status === 'ready')).toBe(true),
      );
      expect(maximum).toBe(2);
      expect(base64).not.toHaveBeenCalled();
      expect(store.attachments.value.every((entry) => !entry.dataURL)).toBe(true);
      const saved = readDrafts(localStorage, store.keys.draftMessages)[0].attachments!;
      expect(saved.map((entry) => entry.uploadId)).toEqual(
        Array.from({ length: 6 }, (_, index) => uploadID(index)),
      );
      expect(saved.every((entry) => entry.blobRef && !entry.file && !entry.dataURL)).toBe(true);

      store.prompt.value = 'Six originals';
      store.endpoints.createResponse = vi.fn(async () => new Response('rejected', { status: 400 }));
      await store.send();
      const body = vi.mocked(store.endpoints.createResponse).mock.calls[0][0];
      expect(JSON.stringify(body).length).toBeLessThan(2000);
      expect((body as any).input[0].content.slice(0, 6)).toEqual(
        Array.from({ length: 6 }, (_, index) => ({
          type: 'input_upload',
          upload_id: uploadID(index),
        })),
      );
      expect(JSON.stringify(body)).not.toMatch(/base64|image_url|file_data/);
      expect(store.attachments.value.map((entry) => entry.uploadId)).toEqual(
        saved.map((entry) => entry.uploadId),
      );
    } finally {
      store.dispose();
    }
  });

  it('aborts removal and never starts a removed queued upload', async () => {
    const store = storeWithRefs();
    const started: Array<{ name: string; signal: AbortSignal }> = [];
    store.endpoints.uploadAttachment = vi.fn(
      (_blob, name, controls = {}) =>
        new Promise<ReturnType<typeof result>>((_resolve, reject) => {
          started.push({ name, signal: controls.signal! });
          controls.signal?.addEventListener('abort', () =>
            reject(new DOMException('Cancelled', 'AbortError')),
          );
        }),
    );
    try {
      store.addAttachments([image(0), image(1), image(2), image(3)]);
      await vi.waitFor(() => expect(started).toHaveLength(2));
      store.removeAttachment(store.attachments.value[3].id);
      const removed = store.attachments.value.find((entry) => entry.name === started[0].name)!;
      store.removeAttachment(removed.id);
      expect(started[0].signal.aborted).toBe(true);
      await vi.waitFor(() => expect(started).toHaveLength(3));
      expect(started.some((entry) => entry.name === 'photo 3.png')).toBe(false);
      expect(store.attachments.value).toHaveLength(2);
    } finally {
      store.dispose();
    }
  });

  it('keeps successes and IDs across failure/reload, retries from saved Blob, and never sweeps other drafts or pending blobs', async () => {
    const sweep = vi.spyOn(blobs.DraftBlobStore.prototype, 'deleteOrphans');
    const removeBlob = vi.spyOn(blobs.DraftBlobStore.prototype, 'delete');
    // fake-indexeddb uses Node's structuredClone, which cannot clone jsdom's
    // Blob internals. Store native Blob bytes to model the browser IDB boundary.
    const put = blobs.DraftBlobStore.prototype.put;
    vi.spyOn(blobs.DraftBlobStore.prototype, 'put').mockImplementation(function (
      this: blobs.DraftBlobStore,
      record,
    ) {
      return put.call(this, {
        ...record,
        blob: new StructuredCloneBlob(['data'], { type: record.mime }) as unknown as Blob,
      });
    });
    const first = storeWithRefs();
    first.endpoints.uploadAttachment = vi.fn(async (_blob, filename) => {
      if (filename.includes('1')) throw new Error('upload failed');
      return result(0);
    });
    first.addAttachments([image(0), image(1)]);
    await vi.waitFor(() =>
      expect(first.attachments.value.map((entry) => entry.status)).toEqual(['ready', 'error']),
    );
    const ids = first.attachments.value.map((entry) => entry.id);
    first.dispose();
    const restored = storeWithRefs();
    restored.endpoints.uploadAttachment = vi.fn(async () => result(1));
    try {
      restored.composer.restore('s1', 'session');
      await vi.waitFor(() =>
        expect(restored.attachments.value.map((entry) => entry.status)).toEqual(['ready', 'error']),
      );
      expect(restored.attachments.value[0].uploadId).toBe(uploadID(0));
      expect(restored.endpoints.uploadAttachment).not.toHaveBeenCalled();
      expect(restored.attachments.value[1].file).toBeUndefined();
      restored.retryAttachment(ids[1]);
      await vi.waitFor(() =>
        expect(restored.attachments.value[1]).toMatchObject({ status: 'ready', error: '' }),
      );
      expect(restored.attachments.value.map((entry) => entry.id)).toEqual(ids);
      expect(restored.attachments.value.map((entry) => entry.uploadId)).toEqual([
        uploadID(0),
        uploadID(1),
      ]);
      expect(restored.endpoints.uploadAttachment).toHaveBeenCalledOnce();
      expect(vi.mocked(restored.endpoints.uploadAttachment).mock.calls[0][0]).toMatchObject({
        size: 4,
        type: 'image/png',
      });
      restored.removeAttachment(ids[1]);
      restored.composer.releaseResources(restored.attachments.value, true);
      expect(sweep).not.toHaveBeenCalled();
      expect(removeBlob).not.toHaveBeenCalled();
    } finally {
      restored.dispose();
    }
  });

  it('keeps a missing local ref attachment visibly blocked instead of silently omitting it', async () => {
    const store = storeWithRefs();
    try {
      saveDraft(localStorage, store.keys.draftMessages, {
        sessionId: 's1',
        content: 'Do not drop this attachment',
        updated: Date.now(),
        attachments: [
          {
            id: 'missing-original',
            name: 'important.png',
            type: 'image/png',
            size: 4,
            blobRef: 'missing-blob',
            status: 'preparing',
            uploadScope: `${location.origin}|/ui|`,
          },
        ],
      });
      store.composer.restore('s1', 'session');
      await vi.waitFor(() =>
        expect(store.attachments.value[0]).toMatchObject({
          id: 'missing-original',
          status: 'error',
          error: expect.stringContaining('reselect'),
        }),
      );
      expect(store.attachments.value).toHaveLength(1);
      store.endpoints.createResponse = vi.fn();
      await store.send();
      expect(store.endpoints.createResponse).not.toHaveBeenCalled();
      expect(readDrafts(localStorage, store.keys.draftMessages)[0].attachments).toHaveLength(1);
    } finally {
      store.dispose();
    }
  });

  it('aborts active and queued owners on draft switch and releases every local preview URL', async () => {
    const store = storeWithRefs();
    const started: AbortSignal[] = [];
    store.endpoints.uploadAttachment = vi.fn(
      (_blob, _name, controls = {}) =>
        new Promise<ReturnType<typeof result>>((_resolve, reject) => {
          started.push(controls.signal!);
          controls.signal?.addEventListener('abort', () =>
            reject(new DOMException('Cancelled', 'AbortError')),
          );
        }),
    );
    try {
      store.addAttachments([image(0), image(1), image(2), image(3)]);
      await vi.waitFor(() => expect(started).toHaveLength(2));
      await vi.waitFor(() =>
        expect(store.attachments.value.every((entry) => entry.blobRef)).toBe(true),
      );
      store.composer.persist();
      store.activeSessionId.value = 's2';
      store.composer.restore('s2', 'session');
      await Promise.resolve();
      await Promise.resolve();
      expect(started.every((signal) => signal.aborted)).toBe(true);
      expect(store.endpoints.uploadAttachment).toHaveBeenCalledTimes(2);
      expect(URL.revokeObjectURL).toHaveBeenCalledTimes(4);
      expect(store.attachments.value).toEqual([]);
    } finally {
      store.dispose();
    }
  });

  it('rejects a ref from another node namespace instead of silently sending it', async () => {
    const store = storeWithRefs();
    try {
      await expect(
        store.attachmentInput({
          name: 'other.png',
          type: 'image/png',
          status: 'ready',
          uploadId: uploadID(0),
          uploadScope: 'other-node',
        }),
      ).rejects.toThrow('this node');
    } finally {
      store.dispose();
    }
  });

  it.each([undefined, 2])(
    'falls back to inline data for upload_refs version %s',
    async (version) => {
      const store = storeWithRefs();
      try {
        (store as any).applyCapabilities({ attachments: { upload_refs: { version } } });
        expect(
          await store.attachmentInput({
            name: 'image.png',
            type: 'image/png',
            dataURL: 'data:image/png;base64,AA==',
            status: 'ready',
          }),
        ).toMatchObject({ type: 'input_image', image_url: 'data:image/png;base64,AA==' });
      } finally {
        store.dispose();
      }
    },
  );
});
