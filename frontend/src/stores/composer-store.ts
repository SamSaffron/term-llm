import { batch, computed, signal, type ReadonlySignal } from '@preact/signals';
import type { Session, Attachment } from '../domain/types';
import {
  DEFAULT_ATTACHMENT_POLICY,
  attachmentAccept,
  validateAttachmentFile,
  isNativeImageType,
  type AttachmentPolicy,
} from '../domain/attachments';
import { blobChecksum, blobToDataURL, DraftBlobStore } from '../platform/draft-blobs';
import { readDrafts, saveDraft } from '../platform/storage';
import { errorMessage } from '../domain/text';
import type { AppStoreServices } from './app-store-services';
import { uuid } from './store-utils';

type ComposerOwner = 'draft' | 'session';

export interface ComposerStoreHost {
  activeSessionId: ReadonlySignal<string>;
  activeProjectId: ReadonlySignal<string>;
  draftActive: ReadonlySignal<boolean>;
  selectedProvider: ReadonlySignal<string>;
  selectedModel: ReadonlySignal<string>;
  selectedEffort: ReadonlySignal<string>;
  selectedReasoningMode: ReadonlySignal<string>;
  selectedAgent: ReadonlySignal<string>;
  setPreference: (
    name: 'provider' | 'model' | 'effort' | 'reasoning' | 'agent',
    value: string,
    commit?: boolean,
  ) => void;
  publishDraftChange: (sessionId: string, revision: number, operationId: string) => void;
}

/** Owns composer text, draft persistence, and attachment materialization. */
export class ComposerStore {
  readonly prompt = signal('');
  readonly attachments = signal<Attachment[]>([]);
  readonly sendPending = signal(false);
  readonly attachmentPolicy = signal<AttachmentPolicy>(DEFAULT_ATTACHMENT_POLICY);
  readonly attachmentAccept = computed(() => attachmentAccept(this.attachmentPolicy.value));
  readonly selectedDraftWorktree = signal('');

  private readonly draftBlobs = new DraftBlobStore();
  private readonly attachmentGenerations = new Map<string, number>();
  private readonly uploadAborts = new Map<string, AbortController>();
  private readonly uploadQueue: Array<() => void> = [];
  private activeUploads = 0;
  private currentDraftRev = 0;
  private newDraftID: string;
  private runtimeDraftID = `draft_${uuid()}`;

  constructor(
    private readonly services: AppStoreServices,
    private readonly host: ComposerStoreHost,
  ) {
    const storedDraftID = services.storage.getItem(services.keys.draftSessionActive) || '';
    this.newDraftID = storedDraftID.startsWith('draft:') ? storedDraftID : `draft:${uuid()}`;
    // The blob DB is shared by all node namespaces and unresolved intents.
    // A draft-only mark/sweep cannot prove ownership; retain blobs conservatively.
  }

  ownerKey(): string {
    return this.host.draftActive.peek() ? this.storageId() : this.host.activeSessionId.peek();
  }

  storageId(): string {
    return this.host.activeSessionId.peek() || this.newDraftID;
  }

  runtimeDraftId(): string {
    return this.runtimeDraftID;
  }

  beginNewDraft(fromSession: boolean, selectedProject: string): void {
    if (fromSession) {
      this.newDraftID = `draft:${uuid()}`;
      this.runtimeDraftID = `draft_${uuid()}`;
      this.currentDraftRev = 0;
    }
    if (!this.services.storage.getItem(this.services.keys.draftSessionActive)) {
      const legacyID = `draft:${selectedProject || 'none'}`;
      if (
        readDrafts(this.services.storage, this.services.keys.draftMessages).some(
          (entry) => entry.sessionId === legacyID,
        )
      )
        this.newDraftID = legacyID;
    }
    this.services.storage.setItem(this.services.keys.draftSessionActive, this.newDraftID);
  }

  persist(): void {
    const id = this.storageId();
    try {
      const drafts = saveDraft(this.services.storage, this.services.keys.draftMessages, {
        sessionId: id,
        content: this.prompt.peek(),
        projectId: this.host.activeProjectId.peek(),
        updated: Date.now(),
        rev: this.currentDraftRev,
        provider: this.host.selectedProvider.peek(),
        model: this.host.selectedModel.peek(),
        effort: this.host.selectedEffort.peek(),
        reasoningMode: this.host.selectedReasoningMode.peek(),
        agent: this.host.selectedAgent.peek(),
        ...(this.host.draftActive.peek() ? { worktreeDir: this.selectedDraftWorktree.peek() } : {}),
        attachments: this.attachments
          .peek()
          .map(
            ({ file: _file, dataURL: _dataURL, previewURL: _previewURL, ...attachment }) =>
              attachment,
          ),
      });
      this.currentDraftRev = drafts.find((draft) => draft.sessionId === id)?.rev || 0;
      if (!this.services.isDisposed)
        this.host.publishDraftChange(
          id,
          this.currentDraftRev,
          `draft:${id}:${this.currentDraftRev}`,
        );
    } catch (error) {
      if (!this.services.isDisposed) this.services.toast(error, 'error');
    }
  }

  reconcileStorage(id: string): void {
    const incoming = readDrafts(this.services.storage, this.services.keys.draftMessages).find(
      (entry) => entry.sessionId === id,
    );
    if (
      incoming &&
      (incoming.rev || 0) > this.currentDraftRev &&
      Boolean(this.prompt.peek().trim()) &&
      incoming.content !== this.prompt.peek()
    ) {
      this.services.toast(
        'This draft changed in another tab. Reload the conversation to choose that version.',
        'error',
      );
      return;
    }
    this.restore(id, this.host.draftActive.peek() ? 'draft' : 'session');
  }

  restore(id: string, owner: ComposerOwner): void {
    this.releaseAttachmentResources(this.attachments.peek(), false);
    const draft = readDrafts(this.services.storage, this.services.keys.draftMessages).find(
      (entry) => entry.sessionId === id,
    );
    this.currentDraftRev = draft?.rev || 0;
    batch(() => {
      this.prompt.value = draft?.content || '';
      this.attachments.value = (draft?.attachments || []).map((attachment, index) => {
        const validation = validateAttachmentFile(
          { name: attachment.name, type: attachment.type, size: Number(attachment.size) || 0 },
          index,
          this.attachmentPolicy.peek(),
        );
        return {
          ...attachment,
          status: validation
            ? ('error' as const)
            : attachment.blobRef
              ? ('preparing' as const)
              : attachment.status || ('error' as const),
          error: validation?.message || attachment.error,
        };
      });
      // This signal belongs only to a new-conversation draft. Existing
      // conversations get their checkout exclusively from Session.worktreeDir.
      this.selectedDraftWorktree.value = owner === 'draft' ? draft?.worktreeDir || '' : '';
    });
    for (const attachment of this.attachments.peek())
      if (attachment.id && attachment.blobRef && attachment.status !== 'error')
        void this.restoreAttachmentBlob(id, attachment.id, attachment.blobRef);
    if (!draft) return;
    if (draft.provider) this.host.setPreference('provider', draft.provider, false);
    if (draft.model) this.host.setPreference('model', draft.model, false);
    if (draft.effort) this.host.setPreference('effort', draft.effort, false);
    if (draft.reasoningMode) this.host.setPreference('reasoning', draft.reasoningMode, false);
    if (draft.agent) this.host.setPreference('agent', draft.agent, false);
  }

  syncRuntimeFromSession(session: Session): void {
    if (session.activeProvider) this.host.setPreference('provider', session.activeProvider, false);
    if (session.activeModel) this.host.setPreference('model', session.activeModel, false);
    // A confirmed effort applies at once, including the server default, so a
    // revisited conversation never shows the previous one's effort.
    const effort = session.confirmedEffort ?? (session.activeEffort || undefined);
    if (effort !== undefined && !this.draftPinsEffort(session.id))
      this.host.setPreference('effort', effort, false);
    if (session.activeReasoningMode)
      this.host.setPreference('reasoning', session.activeReasoningMode, false);
  }

  /**
   * Adopts the confirmed effort of a freshly hydrated session, so the composer
   * stops showing whichever effort the previously viewed session left in the
   * shared preference. An unsent draft that pinned its own effort, or a
   * selection the user changed after `sampledEffort` was read, takes precedence.
   */
  syncPersistedEffort(session: Session, sampledEffort: string): void {
    if (session.confirmedEffort === undefined) return;
    if (this.host.activeSessionId.peek() !== session.id) return;
    if (this.host.selectedEffort.peek() !== sampledEffort) return;
    if (this.draftPinsEffort(session.id)) return;
    this.host.setPreference('effort', session.confirmedEffort, false);
  }

  // Leaving any project conversation persists a draft stamped with the shared
  // effort, so only a draft holding an unsent message records a deliberate one.
  private draftPinsEffort(sessionId: string): boolean {
    const draft = readDrafts(this.services.storage, this.services.keys.draftMessages).find(
      (entry) => entry.sessionId === sessionId,
    );
    return Boolean(draft?.effort && (draft.content.trim() || draft.attachments?.length));
  }

  clearSubmitted(
    sessionId: string,
    inputText: string,
    attachments: Attachment[],
    draftSessionId = sessionId,
  ): void {
    const value = inputText.trim();
    const submittedIDs = new Set(
      attachments.map((attachment) => attachment.id).filter((id): id is string => Boolean(id)),
    );
    if (this.host.activeSessionId.peek() === sessionId)
      batch(() => {
        if (this.prompt.peek().trim() === value) this.prompt.value = '';
        const submitted = new Set(attachments);
        this.attachments.value = this.attachments
          .peek()
          .filter(
            (attachment) =>
              !submitted.has(attachment) && (!attachment.id || !submittedIDs.has(attachment.id)),
          );
      });
    try {
      const draft = readDrafts(this.services.storage, this.services.keys.draftMessages).find(
        (candidate) => candidate.sessionId === draftSessionId,
      );
      if (draft?.content.trim() === value)
        saveDraft(this.services.storage, this.services.keys.draftMessages, {
          ...draft,
          content: '',
          attachments: (draft.attachments || []).filter(
            (attachment) => !attachment.id || !submittedIDs.has(attachment.id),
          ),
          updated: Date.now(),
        });
    } catch {
      this.services.bumpDiagnostic('storageFailures');
    }
  }

  applyAttachmentPolicy(policy: AttachmentPolicy): void {
    this.attachmentPolicy.value = policy;
    if (policy.uploadRefs)
      this.attachments.value = this.attachments.peek().map((attachment) =>
        attachment.status === 'ready' &&
        (!attachment.uploadId || attachment.uploadScope !== this.uploadScope())
          ? {
              ...attachment,
              status: 'error',
              error: 'Retry to upload this saved attachment to this node.',
            }
          : attachment,
      );
  }

  async attachmentInput(attachment: Attachment): Promise<Record<string, unknown>> {
    if (attachment.status && attachment.status !== 'ready')
      throw new Error(attachment.error || `${attachment.name} is not ready to send.`);
    if (this.attachmentPolicy.peek().uploadRefs) {
      if (!attachment.uploadId || attachment.uploadScope !== this.uploadScope())
        throw new Error(`${attachment.name} must finish uploading to this node before sending.`);
      return { type: 'input_upload', upload_id: attachment.uploadId };
    }
    const blob =
      !attachment.dataURL && attachment.blobRef
        ? await this.draftBlobs.get(attachment.blobRef)
        : null;
    const data =
      attachment.dataURL || (blob ? await blobToDataURL(blob.blob) : '') || attachment.url || '';
    if (!data) throw new Error(`Could not materialize ${attachment.name}`);
    if (isNativeImageType(attachment.type))
      return {
        type: 'input_image',
        image_url: data,
        filename: attachment.name,
        ...(attachment.width && attachment.height
          ? { width: attachment.width, height: attachment.height }
          : {}),
      };
    return { type: 'input_file', file_data: data, filename: attachment.name };
  }

  addAttachments(files: FileList | File[]): void {
    let count = this.attachments.peek().length;
    for (const file of Array.from(files)) {
      const validation = validateAttachmentFile(file, count, this.attachmentPolicy.peek());
      if (validation) {
        this.services.toast(validation.message, 'error');
        continue;
      }
      count += 1;
      const attachment: Attachment = {
        id: uuid(),
        name: file.name,
        type: file.type || 'application/octet-stream',
        size: file.size,
        file,
        status: 'preparing',
        progress: 0,
        draftId: this.storageId(),
        ...(this.attachmentPolicy.peek().uploadRefs ? { uploadScope: this.uploadScope() } : {}),
      };
      this.attachments.value = [...this.attachments.peek(), attachment];
      void this.prepareAttachment(attachment);
    }
  }

  private uploadScope(): string {
    return `${location.origin}|${this.services.config.prefix}|${this.services.config.hub?.nodeId || ''}`;
  }

  /** Only actual upload requests hold a slot; 100% bytes is not completion until 201. */
  private uploadBlob(
    blob: Blob,
    attachment: Attachment,
    controller: AbortController,
  ): Promise<string> {
    return new Promise((resolve, reject) => {
      const start = () => {
        if (controller.signal.aborted) {
          reject(controller.signal.reason);
          return;
        }
        this.activeUploads += 1;
        void this.services.endpoints
          .uploadAttachment(blob, attachment.name, {
            signal: controller.signal,
            onProgress: (loaded, total) => {
              if (!controller.signal.aborted)
                this.updateAttachment(attachment.id!, {
                  progress: Math.min(0.99, loaded / (total || blob.size || 1)),
                });
            },
          })
          .then((result) => {
            if (!/^upl_[A-Za-z0-9_-]{43}$/.test(result.upload_id))
              throw new Error('Server returned an invalid upload ID.');
            resolve(result.upload_id);
          })
          .catch(reject)
          .finally(() => {
            this.activeUploads -= 1;
            this.drainUploads();
          });
      };
      controller.signal.addEventListener(
        'abort',
        () => {
          const index = this.uploadQueue.indexOf(start);
          if (index >= 0) this.uploadQueue.splice(index, 1);
          reject(controller.signal.reason || new DOMException('Upload cancelled', 'AbortError'));
        },
        { once: true },
      );
      this.uploadQueue.push(start);
      this.drainUploads();
    });
  }

  private drainUploads(): void {
    while (this.activeUploads < 2 && this.uploadQueue.length) this.uploadQueue.shift()!();
  }

  private async prepareAttachment(attachment: Attachment, restoredBlob?: Blob): Promise<void> {
    if (!attachment.id || (!attachment.file && !restoredBlob)) return;
    const attachmentId = attachment.id;
    const draftId = attachment.draftId || this.storageId();
    const generation = (this.attachmentGenerations.get(attachmentId) || 0) + 1;
    this.attachmentGenerations.set(attachmentId, generation);
    const owns = (): boolean =>
      !this.services.isDisposed &&
      this.storageId() === draftId &&
      this.attachmentGenerations.get(attachmentId) === generation &&
      this.attachments.peek().some((entry) => entry.id === attachmentId);
    this.uploadAborts.get(attachmentId)?.abort();
    const controller = new AbortController();
    this.uploadAborts.set(attachmentId, controller);
    const source = restoredBlob || attachment.file!;
    if (attachment.previewURL?.startsWith('blob:')) URL.revokeObjectURL(attachment.previewURL);
    this.updateAttachment(attachmentId, { previewURL: undefined });
    let previewURL = '';
    const revokePreview = () => {
      if (previewURL) URL.revokeObjectURL(previewURL);
      previewURL = '';
    };
    controller.signal.addEventListener('abort', revokePreview, { once: true });
    try {
      const refs = this.attachmentPolicy.peek().uploadRefs;
      const dataURL = refs ? undefined : await blobToDataURL(source);
      if (!owns()) return;
      this.updateAttachment(attachmentId, { progress: 0.5 });
      let width: number | undefined;
      let height: number | undefined;
      if (isNativeImageType(source.type)) {
        previewURL = URL.createObjectURL(source);
        const dimensions = await new Promise<{ width: number; height: number }>(
          (resolve, reject) => {
            const image = new Image();
            const cancelled = () => {
              image.onload = null;
              image.onerror = null;
              image.src = '';
              reject(
                controller.signal.reason || new DOMException('Attachment cancelled', 'AbortError'),
              );
            };
            controller.signal.addEventListener('abort', cancelled, { once: true });
            image.onload = () => {
              controller.signal.removeEventListener('abort', cancelled);
              resolve({ width: image.naturalWidth, height: image.naturalHeight });
            };
            image.onerror = () => {
              controller.signal.removeEventListener('abort', cancelled);
              reject(new Error(`Could not decode ${attachment.name} as an image.`));
            };
            image.src = previewURL;
          },
        );
        width = dimensions.width;
        height = dimensions.height;
      }
      const checksum = await blobChecksum(source);
      if (!owns()) {
        if (previewURL) URL.revokeObjectURL(previewURL);
        return;
      }
      await this.draftBlobs.put({
        id: attachmentId,
        draftId,
        blob: source,
        mime: source.type,
        size: source.size,
        checksum,
        updated: Date.now(),
      });
      if (!owns()) {
        if (previewURL) URL.revokeObjectURL(previewURL);
        return;
      }
      // Persist the local original before starting the remote transfer, so a
      // reload or failed upload can retry from IndexedDB without the File object.
      this.updateAttachment(attachmentId, {
        blobRef: attachmentId,
        checksum,
        ...(refs ? { uploadScope: this.uploadScope() } : {}),
      });
      this.persist();
      const uploadId = refs
        ? attachment.uploadId && attachment.uploadScope === this.uploadScope()
          ? attachment.uploadId
          : await this.uploadBlob(source, attachment, controller)
        : undefined;
      if (!owns()) {
        if (previewURL) URL.revokeObjectURL(previewURL);
        return;
      }
      this.updateAttachment(attachmentId, {
        ...(uploadId ? { uploadId, uploadScope: this.uploadScope() } : {}),
        dataURL,
        previewURL: previewURL || undefined,
        width,
        height,
        checksum,
        blobRef: attachment.id,
        status: 'ready',
        progress: 1,
        error: '',
      });
      this.persist();
    } catch (error) {
      if (previewURL) URL.revokeObjectURL(previewURL);
      if (!owns()) return;
      this.updateAttachment(attachmentId, {
        status: 'error',
        error: errorMessage(error),
        progress: 0,
      });
      this.services.toast(error, 'error');
      this.persist();
    } finally {
      controller.signal.removeEventListener('abort', revokePreview);
      if (this.uploadAborts.get(attachmentId) === controller)
        this.uploadAborts.delete(attachmentId);
    }
  }

  private updateAttachment(id: string, patch: Partial<Attachment>): void {
    this.attachments.value = this.attachments
      .peek()
      .map((entry) => (entry.id === id ? { ...entry, ...patch } : entry));
  }

  private async restoreAttachmentBlob(draftId: string, id: string, blobRef: string): Promise<void> {
    const generation = (this.attachmentGenerations.get(id) || 0) + 1;
    this.attachmentGenerations.set(id, generation);
    const owns = (): boolean =>
      !this.services.isDisposed &&
      this.storageId() === draftId &&
      this.attachmentGenerations.get(id) === generation &&
      this.attachments.peek().some((entry) => entry.id === id);
    try {
      const record = await this.draftBlobs.get(blobRef);
      if (!owns()) return;
      if (!record || record.draftId !== draftId) {
        const attachment = this.attachments.peek().find((entry) => entry.id === id);
        if (attachment?.uploadId && attachment.uploadScope === this.uploadScope()) {
          this.updateAttachment(id, { status: 'ready', progress: 1, error: '' });
          return;
        }
        if (this.attachmentPolicy.peek().uploadRefs || attachment?.uploadScope) {
          this.updateAttachment(id, {
            status: 'error',
            progress: 0,
            error: 'Local attachment unavailable; reselect the file before sending.',
          });
          this.persist();
          return;
        }
        // Preserve the legacy behavior for old inline drafts only.
        this.attachmentGenerations.set(id, generation + 1);
        this.attachments.value = this.attachments.peek().filter((entry) => entry.id !== id);
        this.persist();
        return;
      }
      const previewURL = isNativeImageType(record.mime)
        ? URL.createObjectURL(record.blob)
        : undefined;
      if (!owns()) {
        if (previewURL) URL.revokeObjectURL(previewURL);
        return;
      }
      const attachment = this.attachments.peek().find((entry) => entry.id === id)!;
      const needsUpload =
        this.attachmentPolicy.peek().uploadRefs &&
        (!attachment.uploadId || attachment.uploadScope !== this.uploadScope());
      this.updateAttachment(id, {
        previewURL,
        status: needsUpload ? 'error' : 'ready',
        progress: needsUpload ? 0 : 1,
        error: needsUpload ? 'Upload not completed. Retry to upload this saved attachment.' : '',
      });
    } catch (error) {
      if (!owns()) return;
      this.updateAttachment(id, { status: 'error', error: errorMessage(error), progress: 0 });
      this.services.toast(error, 'error');
    }
  }

  retryAttachment(id: string | undefined): void {
    const attachment = this.attachments.peek().find((entry) => entry.id === id);
    if (!attachment?.id || (!attachment.file && !attachment.blobRef)) return;
    const validation = validateAttachmentFile(
      {
        name: attachment.name,
        type: attachment.type,
        size: attachment.size || attachment.file?.size || 0,
      },
      this.attachments.peek().filter((entry) => entry.id !== attachment.id).length,
      this.attachmentPolicy.peek(),
    );
    if (validation) {
      this.updateAttachment(attachment.id, {
        status: 'error',
        error: validation.message,
        progress: 0,
      });
      this.services.toast(validation.message, 'error');
      return;
    }
    this.updateAttachment(attachment.id, { status: 'preparing', error: '', progress: 0 });
    if (attachment.file) void this.prepareAttachment(attachment);
    else {
      const draftId = this.storageId();
      const generation = this.attachmentGenerations.get(attachment.id);
      void this.draftBlobs
        .get(attachment.blobRef!)
        .then((record) => {
          if (
            this.services.isDisposed ||
            this.storageId() !== draftId ||
            this.attachmentGenerations.get(attachment.id!) !== generation
          )
            return;
          if (!record)
            throw new Error(
              `The saved original for ${attachment.name} is unavailable. Remove and select it again.`,
            );
          if (this.attachments.peek().some((entry) => entry.id === attachment.id))
            return this.prepareAttachment(attachment, record.blob);
        })
        .catch((error) => {
          if (
            this.storageId() === draftId &&
            this.attachmentGenerations.get(attachment.id!) === generation
          )
            this.updateAttachment(attachment.id!, {
              status: 'error',
              error: errorMessage(error),
              progress: 0,
            });
        });
    }
  }

  private releaseAttachmentResources(attachments: Attachment[], _deleteBlobs: boolean): void {
    for (const attachment of attachments) {
      if (attachment.id) this.uploadAborts.get(attachment.id)?.abort();
      if (attachment.id)
        this.attachmentGenerations.set(
          attachment.id,
          (this.attachmentGenerations.get(attachment.id) || 0) + 1,
        );
      if (attachment.previewURL?.startsWith('blob:')) URL.revokeObjectURL(attachment.previewURL);
    }
  }

  removeAttachment(id: string | undefined): void {
    const attachment = this.attachments.value.find((entry) => entry.id === id);
    if (id) this.uploadAborts.get(id)?.abort();
    if (id) this.attachmentGenerations.set(id, (this.attachmentGenerations.get(id) || 0) + 1);
    if (attachment?.previewURL?.startsWith('blob:')) URL.revokeObjectURL(attachment.previewURL);
    this.attachments.value = this.attachments.value.filter((entry) => entry.id !== id);
    this.persist();
  }

  releaseResources(attachments: Attachment[], deleteBlobs: boolean): void {
    this.releaseAttachmentResources(attachments, deleteBlobs);
  }

  dispose(): void {
    this.releaseAttachmentResources(this.attachments.peek(), false);
    this.draftBlobs.close();
  }
}
