import { batch, computed, signal, type ReadonlySignal } from '@preact/signals';
import type { Session, Attachment } from '../domain/types';
import {
  DEFAULT_ATTACHMENT_POLICY,
  attachmentAccept,
  validateAttachmentFile,
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
  private currentDraftRev = 0;
  private newDraftID: string;
  private runtimeDraftID = `draft_${uuid()}`;

  constructor(
    private readonly services: AppStoreServices,
    private readonly host: ComposerStoreHost,
  ) {
    const storedDraftID = services.storage.getItem(services.keys.draftSessionActive) || '';
    this.newDraftID = storedDraftID.startsWith('draft:') ? storedDraftID : `draft:${uuid()}`;
    const referencedBlobIDs = new Set(
      readDrafts(services.storage, services.keys.draftMessages).flatMap((draft) =>
        (draft.attachments || []).flatMap((attachment) =>
          attachment.blobRef ? [attachment.blobRef] : [],
        ),
      ),
    );
    void this.draftBlobs.deleteOrphans(referencedBlobIDs).catch(() => {
      if (!services.isDisposed) services.bumpDiagnostic('storageFailures');
    });
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
    if (session.activeEffort) this.host.setPreference('effort', session.activeEffort, false);
    if (session.activeReasoningMode)
      this.host.setPreference('reasoning', session.activeReasoningMode, false);
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
  }

  async attachmentInput(attachment: Attachment): Promise<Record<string, unknown>> {
    if (attachment.status && attachment.status !== 'ready')
      throw new Error(attachment.error || `${attachment.name} is not ready to send.`);
    const data = attachment.dataURL || attachment.url || '';
    if (!data) throw new Error(`Could not materialize ${attachment.name}`);
    if (attachment.type.startsWith('image/'))
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
      };
      this.attachments.value = [...this.attachments.peek(), attachment];
      void this.prepareAttachment(attachment);
    }
  }

  private async prepareAttachment(attachment: Attachment): Promise<void> {
    if (!attachment.id || !attachment.file) return;
    const attachmentId = attachment.id;
    const draftId = attachment.draftId || this.storageId();
    const generation = (this.attachmentGenerations.get(attachmentId) || 0) + 1;
    this.attachmentGenerations.set(attachmentId, generation);
    const owns = (): boolean =>
      !this.services.isDisposed &&
      this.storageId() === draftId &&
      this.attachmentGenerations.get(attachmentId) === generation &&
      this.attachments.peek().some((entry) => entry.id === attachmentId);
    const source = attachment.file;
    let previewURL = '';
    try {
      const dataURL = await blobToDataURL(source);
      if (!owns()) return;
      this.updateAttachment(attachmentId, { progress: 0.5 });
      let width: number | undefined;
      let height: number | undefined;
      if (source.type.startsWith('image/')) {
        previewURL = URL.createObjectURL(source);
        const dimensions = await new Promise<{ width: number; height: number }>(
          (resolve, reject) => {
            const image = new Image();
            image.onload = () =>
              resolve({ width: image.naturalWidth, height: image.naturalHeight });
            image.onerror = () => reject(new Error(`Could not decode ${source.name} as an image.`));
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
        await this.draftBlobs.delete(attachmentId);
        if (previewURL) URL.revokeObjectURL(previewURL);
        return;
      }
      this.updateAttachment(attachmentId, {
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
        // The draft metadata can outlive IndexedDB data after browser storage
        // eviction or a partial clear. Drop that stale attachment instead of
        // leaving an unusable error chip in the composer on every reload.
        this.attachmentGenerations.set(id, generation + 1);
        this.attachments.value = this.attachments.peek().filter((entry) => entry.id !== id);
        this.persist();
        return;
      }
      const dataURL = await blobToDataURL(record.blob);
      const previewURL = record.mime.startsWith('image/')
        ? URL.createObjectURL(record.blob)
        : undefined;
      if (!owns()) {
        if (previewURL) URL.revokeObjectURL(previewURL);
        return;
      }
      this.updateAttachment(id, { dataURL, previewURL, status: 'ready', progress: 1, error: '' });
    } catch (error) {
      if (!owns()) return;
      this.updateAttachment(id, { status: 'error', error: errorMessage(error), progress: 0 });
      this.services.toast(error, 'error');
    }
  }

  retryAttachment(id: string | undefined): void {
    const attachment = this.attachments.peek().find((entry) => entry.id === id);
    if (!attachment?.file || !attachment.id) return;
    const validation = validateAttachmentFile(
      attachment.file,
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
    void this.prepareAttachment(attachment);
  }

  private releaseAttachmentResources(attachments: Attachment[], deleteBlobs: boolean): void {
    for (const attachment of attachments) {
      if (attachment.id)
        this.attachmentGenerations.set(
          attachment.id,
          (this.attachmentGenerations.get(attachment.id) || 0) + 1,
        );
      if (attachment.previewURL?.startsWith('blob:')) URL.revokeObjectURL(attachment.previewURL);
      if (deleteBlobs && attachment.blobRef)
        void this.draftBlobs
          .delete(attachment.blobRef)
          .catch((error) => this.services.toast(error, 'error'));
    }
  }

  removeAttachment(id: string | undefined): void {
    const attachment = this.attachments.value.find((entry) => entry.id === id);
    if (id) this.attachmentGenerations.set(id, (this.attachmentGenerations.get(id) || 0) + 1);
    if (attachment?.previewURL?.startsWith('blob:')) URL.revokeObjectURL(attachment.previewURL);
    this.attachments.value = this.attachments.value.filter((entry) => entry.id !== id);
    if (attachment?.blobRef)
      void this.draftBlobs
        .delete(attachment.blobRef)
        .catch((error) => this.services.toast(error, 'error'));
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
