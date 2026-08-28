import { signal, type ReadonlySignal } from '@preact/signals';
import type { Session, DiffComment, DiffFile, FilesystemObservation } from '../domain/types';
import { errorMessage } from '../domain/text';
import {
  reviewAnchorFingerprint,
  reviewCommentPayload,
  validateReviewBatch,
  validateReviewComment,
} from '../domain/review-policy';
import {
  linesFromHunks,
  normalizeDiffScope,
  parseUnifiedPatch,
  sortDiffFiles,
} from '../domain/diff';
import {
  clearSessionDiffComments,
  persistDiffComment,
  readDiffCommentQueue,
  removeDiffComment,
} from '../platform/storage';
import type { AppStoreServices } from './app-store-services';
import type { DiffState, SendOptions } from './store-types';
import { listFrom, recordValue, uuid } from './store-utils';

export interface ReviewStoreHost {
  activeSession: ReadonlySignal<Session | null>;
  activeSessionId: ReadonlySignal<string>;
  streaming: ReadonlySignal<boolean>;
  closePlan: () => void;
  restartStatusPoll: () => void;
  send: (options?: SendOptions) => Promise<void>;
  interject: (content: string, options?: SendOptions) => Promise<void>;
  publishSessionChange: (type: 'review-comment-changed', sessionId: string) => void;
}

/** Owns file-change loading, expansion, and the durable review comment queue. */
export class ReviewStore {
  readonly diff = signal<DiffState>({
    open: false,
    sessionId: '',
    scope: 'last_turn',
    git: false,
    loading: false,
    files: [],
    materializations: [],
    observations: [],
    claimDiagnostics: [],
    unavailableLineCountFiles: 0,
    filter: '',
    comments: [],
    historyComments: [],
    error: '',
    maximized: false,
    width: 420,
    selectedPath: '',
    followCurrentFile: true,
  });

  private loadEpoch = 0;
  private readonly expandEpoch = new Map<string, number>();

  constructor(
    private readonly services: AppStoreServices,
    private readonly host: ReviewStoreHost,
  ) {
    this.diff.value = {
      ...this.diff.value,
      width: Math.max(320, Number(services.storage.getItem(services.keys.diffSidebarWidth)) || 420),
      comments: readDiffCommentQueue(services.storage, services.keys.diffCommentQueue),
    };
  }

  reloadCommentQueue(): void {
    this.diff.value = {
      ...this.diff.peek(),
      comments: readDiffCommentQueue(this.services.storage, this.services.keys.diffCommentQueue),
    };
  }

  async refreshDiffComments(sessionId = this.host.activeSessionId.peek()): Promise<void> {
    if (!sessionId) return;
    try {
      const data = await this.services.endpoints.diffComments(sessionId);
      if (this.diff.peek().sessionId !== sessionId) return;
      const incoming = listFrom(data, 'comments', 'items')
        .map((entry): DiffComment | null => {
          const raw = recordValue(entry.diff_comment) || entry;
          const side = String(raw.side || '').toLowerCase();
          const path = String(raw.path || '');
          const body = String(raw.instruction || raw.body || '').trim();
          const line = Number(raw.line) || 0;
          if (!path || !body || !line || (side !== 'old' && side !== 'new')) return null;
          return {
            id: String(raw.id || '') || undefined,
            parentId: String(raw.parent_id || '') || undefined,
            path,
            side,
            line,
            body,
            sessionId,
            scope: String(raw.scope || '') || undefined,
            context: String(raw.line_text ?? raw.context ?? ''),
            fileChangeSeq: Number(raw.file_change_seq ?? raw.fileChangeSeq) || 0,
            clientMessageId:
              String(entry.client_message_id || raw.client_message_id || '') || undefined,
            createdAt: Number(entry.created_at || raw.created_at) || Date.now(),
          };
        })
        .filter((comment): comment is DiffComment => comment !== null);
      const ids = new Set(incoming.map((comment) => comment.id).filter(Boolean));
      const pending = this.diff
        .peek()
        .historyComments.filter(
          (comment) =>
            comment.sessionId === sessionId && comment.optimistic && !ids.has(comment.id),
        );
      this.diff.value = { ...this.diff.peek(), historyComments: [...incoming, ...pending] };
    } catch {
      /* Keep the optimistic trail visible; the status loop will reconcile it. */
    }
  }

  async toggleDiff(): Promise<void> {
    const session = this.host.activeSession.value;
    if (!session) return;
    this.host.closePlan();
    this.diff.value = { ...this.diff.value, sessionId: session.id, open: !this.diff.value.open };
    this.host.restartStatusPoll();
    if (this.diff.value.open) await this.loadDiff();
  }
  async loadDiff(): Promise<void> {
    const session = this.host.activeSession.value;
    if (!session) return;
    const owner = session.id;
    const scope = normalizeDiffScope(this.diff.value.scope);
    const epoch = ++this.loadEpoch;
    this.diff.value = { ...this.diff.value, sessionId: owner, scope, loading: true, error: '' };
    try {
      void this.refreshDiffComments(owner);
      const data = await this.services.endpoints.fileChanges(owner, scope);
      const state = this.diff.peek();
      if (
        epoch !== this.loadEpoch ||
        this.host.activeSessionId.peek() !== owner ||
        state.sessionId !== owner ||
        normalizeDiffScope(state.scope) !== scope
      )
        return;
      const existing = new Map(state.files.map((file) => [file.path, file]));
      const refreshContexts = new Map<string, number>();
      const files = sortDiffFiles(
        listFrom(data, 'file_changes', 'files', 'changes')
          .map((entry): DiffFile => {
            const path = String(entry.path || '');
            const previous = existing.get(path);
            const sequence = Number(entry.seq ?? entry.sequence) || 0;
            const snapshotSeq = Number(entry.snapshot_seq) || 0;
            const sameContent = Boolean(
              previous &&
              (previous.sequence || 0) === sequence &&
              (previous.snapshotSeq || 0) === snapshotSeq,
            );
            if (previous?.expanded && !sameContent)
              refreshContexts.set(path, previous.context || 0);
            return {
              path,
              old_path: String(entry.old_path || ''),
              status: String(entry.status || entry.kind || ''),
              additions: Number(entry.adds ?? entry.additions) || 0,
              deletions: Number(entry.dels ?? entry.deletions) || 0,
              binary: Boolean(entry.binary || entry.is_binary),
              image: Boolean(entry.image || entry.is_image),
              beforeURL: String(
                entry.before_url || entry.old_url || (sameContent ? previous?.beforeURL : '') || '',
              ),
              afterURL: String(
                entry.after_url || entry.new_url || (sameContent ? previous?.afterURL : '') || '',
              ),
              lastChangedAt: Number(entry.last_changed_at || entry.updated_at) || 0,
              sequence,
              snapshotSeq,
              truncated: Boolean(entry.truncated),
              provenance: String(entry.provenance || '') as DiffFile['provenance'],
              provenances: Array.isArray(entry.provenances) ? entry.provenances.map(String) : [],
              baselineState: String(entry.baseline_state || '') as DiffFile['baselineState'],
              contentStatus: String(entry.content_status || ''),
              contentAvailable: Boolean(entry.content_available),
              claimCoverage: String(entry.claim_coverage || '') as DiffFile['claimCoverage'],
              expanded: previous?.expanded,
              loading: sameContent ? previous?.loading : Boolean(previous?.expanded),
              error: sameContent ? previous?.error : '',
              lines: sameContent ? previous?.lines : undefined,
              patch: sameContent ? previous?.patch : undefined,
              context: sameContent ? previous?.context : undefined,
              oldLineCount: sameContent ? previous?.oldLineCount : undefined,
              newLineCount: sameContent ? previous?.newLineCount : undefined,
              lang: sameContent ? previous?.lang : undefined,
            };
          })
          .filter((entry) => entry.path),
      );
      const parseObservation = (entry: Record<string, unknown>): FilesystemObservation => ({
        id: Number(entry.id) || 0,
        classification: String(entry.classification || ''),
        root: String(entry.root || ''),
        createdCount: Number(entry.created_count) || 0,
        modifiedCount: Number(entry.modified_count) || 0,
        deletedCount: Number(entry.deleted_count) || 0,
        sampledPaths: Array.isArray(entry.sampled_paths) ? entry.sampled_paths.map(String) : [],
        samplesTruncated: Boolean(entry.samples_truncated),
        coverageStatus: String(entry.coverage_status || 'complete'),
        eventSeq: Number(entry.event_seq) || 0,
      });
      const materializations = listFrom(data, 'materializations').map(parseObservation);
      const observationContainer = (data.observations || {}) as Record<string, unknown>;
      const observations = listFrom(observationContainer, 'batches').map(parseObservation);
      const claimDiagnostics = listFrom(data, 'claim_diagnostics').map((entry) => ({
        normalizedPattern: String(entry.normalized_pattern || ''),
        claimKind: String(entry.claim_kind || ''),
        reason: String(entry.reason || ''),
        coverageStatus: String(entry.coverage_status || 'complete'),
        matchingPathCount: Number(entry.matching_path_count) || 0,
        message: String(entry.message || ''),
      }));
      const summary = (data.file_change_summary || {}) as Record<string, unknown>;
      let newlyStale = 0;
      const comments = state.comments.map((comment) => {
        if (comment.sessionId !== owner || comment.state === 'stale') return comment;
        const turnScope = ['last_turn', 'last_3_turns'].includes(
          normalizeDiffScope(comment.scope || scope),
        );
        const file = files.find((entry) => entry.path === comment.path);
        let anchorChanged = !file;
        if (file?.lines && comment.anchorFingerprint) {
          const index = file.lines.findIndex((line) =>
            comment.side === 'old' ? line.oldLine === comment.line : line.newLine === comment.line,
          );
          if (index < 0) anchorChanged = true;
          else {
            const beforeCount = comment.contextBefore?.length || 0;
            const afterCount = comment.contextAfter?.length || 0;
            const currentAnchor: DiffComment = {
              ...comment,
              context: file.lines[index].content,
              contextBefore: file.lines
                .slice(Math.max(0, index - beforeCount), index)
                .map((line) => line.content),
              contextAfter: file.lines
                .slice(index + 1, index + 1 + afterCount)
                .map((line) => line.content),
            };
            anchorChanged = reviewAnchorFingerprint(currentAnchor) !== comment.anchorFingerprint;
          }
        }
        const stale =
          anchorChanged ||
          (turnScope &&
            Boolean(comment.fileChangeSeq && (file?.sequence || 0) > comment.fileChangeSeq));
        if (!stale) return comment;
        newlyStale += 1;
        const updated: DiffComment = { ...comment, state: 'stale', updatedAt: Date.now() };
        try {
          persistDiffComment(this.services.storage, this.services.keys.diffCommentQueue, updated);
        } catch (error) {
          this.services.toast(error, 'error');
        }
        return updated;
      });
      this.diff.value = {
        ...state,
        files,
        comments,
        materializations,
        observations,
        claimDiagnostics,
        unavailableLineCountFiles: Number(summary.line_counts_unavailable_files) || 0,
        git: Boolean(data.git),
        loading: false,
      };
      if (newlyStale)
        this.services.toast(
          `${newlyStale} queued comment${newlyStale === 1 ? '' : 's'} became stale after the source changed.`,
          'info',
        );
      await Promise.all(
        files
          .filter((file) => refreshContexts.has(file.path))
          .map((file) => this.expandDiff(file, refreshContexts.get(file.path) || 0)),
      );
    } catch (error) {
      if (
        epoch === this.loadEpoch &&
        this.host.activeSessionId.peek() === owner &&
        this.diff.peek().sessionId === owner &&
        normalizeDiffScope(this.diff.peek().scope) === scope
      )
        this.diff.value = {
          ...this.diff.value,
          loading: false,
          error: errorMessage(error),
        };
    }
  }
  async expandDiff(file: DiffFile, context = 0): Promise<void> {
    const session = this.host.activeSession.value;
    if (!session) return;
    const owner = session.id;
    const scope = normalizeDiffScope(this.diff.value.scope);
    const isRequestedVersion = (entry: DiffFile): boolean =>
      entry.path === file.path &&
      (entry.sequence || 0) === (file.sequence || 0) &&
      (entry.snapshotSeq || 0) === (file.snapshotSeq || 0);
    if (file.lines && !context) {
      this.diff.value = {
        ...this.diff.value,
        files: this.diff.value.files.map((entry) =>
          isRequestedVersion(entry) ? { ...entry, expanded: !entry.expanded } : entry,
        ),
      };
      return;
    }
    const requestKey = `${owner}\u0000${scope}\u0000${file.path}`;
    const requestEpoch = (this.expandEpoch.get(requestKey) || 0) + 1;
    this.expandEpoch.set(requestKey, requestEpoch);
    this.diff.value = {
      ...this.diff.value,
      files: this.diff.value.files.map((entry) =>
        isRequestedVersion(entry) ? { ...entry, loading: true, error: '' } : entry,
      ),
    };
    try {
      const data = await this.services.endpoints.fileDiff(
        owner,
        file.path,
        scope,
        context,
        file.snapshotSeq || 0,
      );
      if (
        this.expandEpoch.get(requestKey) !== requestEpoch ||
        this.host.activeSessionId.peek() !== owner ||
        this.diff.peek().sessionId !== owner ||
        normalizeDiffScope(this.diff.peek().scope) !== scope
      )
        return;
      const lines = Array.isArray(data.hunks)
        ? linesFromHunks(data.hunks, {
            old: Number(data.old_line_count) || 0,
            new: Number(data.new_line_count) || 0,
          })
        : Array.isArray(data.lines)
          ? (data.lines as DiffFile['lines'])
          : parseUnifiedPatch(String(data.diff || data.patch || ''));
      const image = Boolean(data.image || file.image);
      const status = String(data.kind || file.status || '').toLowerCase();
      const beforeURL =
        image && !['add', 'added', 'create', 'created'].includes(status)
          ? this.services.endpoints.fileContentURL(
              owner,
              file.path,
              scope,
              'before',
              file.snapshotSeq || 0,
            )
          : String(data.before_url || file.beforeURL || '');
      const afterURL =
        image && !['delete', 'deleted', 'remove', 'removed'].includes(status)
          ? this.services.endpoints.fileContentURL(
              owner,
              file.path,
              scope,
              'after',
              file.snapshotSeq || 0,
            )
          : String(data.after_url || file.afterURL || '');
      this.diff.value = {
        ...this.diff.value,
        files: this.diff.value.files.map((entry) =>
          isRequestedVersion(entry)
            ? {
                ...entry,
                status: String(data.kind || entry.status || ''),
                expanded: true,
                loading: false,
                lines,
                image,
                truncated: Boolean(data.truncated),
                contentStatus: String(data.content_status || entry.contentStatus || ''),
                contentAvailable: Boolean(data.content_available),
                provenance: String(
                  data.provenance || entry.provenance || '',
                ) as DiffFile['provenance'],
                baselineState: String(
                  data.baseline_state || entry.baselineState || '',
                ) as DiffFile['baselineState'],
                claimCoverage: String(
                  data.claim_coverage || entry.claimCoverage || '',
                ) as DiffFile['claimCoverage'],
                context: Number(data.context) || context || 3,
                lang: String(data.lang || ''),
                oldLineCount: Number(data.old_line_count) || 0,
                newLineCount: Number(data.new_line_count) || 0,
                patch: String(data.diff || data.patch || entry.patch || ''),
                beforeURL,
                afterURL,
              }
            : entry,
        ),
      };
    } catch (error) {
      if (
        this.expandEpoch.get(requestKey) !== requestEpoch ||
        this.host.activeSessionId.peek() !== owner ||
        this.diff.peek().sessionId !== owner ||
        normalizeDiffScope(this.diff.peek().scope) !== scope
      )
        return;
      this.diff.value = {
        ...this.diff.value,
        files: this.diff.value.files.map((entry) =>
          isRequestedVersion(entry)
            ? {
                ...entry,
                loading: false,
                error: errorMessage(error),
              }
            : entry,
        ),
      };
    } finally {
      if (this.expandEpoch.get(requestKey) === requestEpoch) this.expandEpoch.delete(requestKey);
    }
  }
  private prepareDiffComments(comments: DiffComment[]): {
    payloads: Array<Record<string, unknown>>;
    inputText: string;
  } | null {
    const validation = validateReviewBatch(comments);
    if (validation) {
      this.services.bumpDiagnostic('queueValidationFailures');
      this.services.toast(validation.message, 'error');
      return null;
    }
    const stale = comments.find((comment) => comment.state === 'stale');
    if (stale) {
      this.services.toast(
        `${stale.path}:${stale.line} is stale. Re-anchor it or explicitly send it individually.`,
        'error',
      );
      return null;
    }
    const payloads = comments.map((comment) => reviewCommentPayload(comment));
    if (
      payloads.some(
        (comment) =>
          ['last_turn', 'last_3_turns'].includes(String(comment.scope)) &&
          Number(comment.file_change_seq) <= 0,
      )
    ) {
      this.services.toast(
        'Refresh the diff before sending these comments so their file snapshot can be anchored.',
        'error',
      );
      return null;
    }
    return {
      payloads,
      inputText:
        payloads.length === 1
          ? `[Inline diff instruction]\n${payloads[0].path}:${payloads[0].line} — ${payloads[0].instruction}`
          : `[Inline diff instructions] (${payloads.length} anchored comments)\n\n${payloads.map((comment) => `${comment.path}:${comment.line} — ${comment.instruction}`).join('\n')}`,
    };
  }
  async sendDiffComment(comment: DiffComment): Promise<void> {
    const session = this.host.activeSession.value;
    if (!session || (comment.sessionId && comment.sessionId !== session.id)) return;
    const value: DiffComment = {
      ...comment,
      id: comment.id || uuid(),
      sessionId: session.id,
      createdAt: comment.createdAt || Date.now(),
      optimistic: true,
    };
    const prepared = this.prepareDiffComments([value]);
    if (!prepared) return;
    this.diff.value = {
      ...this.diff.peek(),
      historyComments: [
        ...this.diff.peek().historyComments.filter((entry) => entry.id !== value.id),
        value,
      ],
    };
    const options: SendOptions = {
      contentParts: prepared.payloads.map((diff_comment) => ({
        type: 'diff_comment',
        diff_comment,
      })),
      inputText: prepared.inputText,
      displayContent: value.body,
      preserveComposer: true,
      diffComments: [value],
      onTransportStarted: () => {
        void this.refreshDiffComments(session.id);
      },
      onTransportFailed: (error) => {
        this.diff.value = {
          ...this.diff.peek(),
          historyComments: this.diff
            .peek()
            .historyComments.filter((entry) => entry.id !== value.id || !entry.optimistic),
        };
        this.services.toast(error, 'error');
      },
    };
    if (this.host.streaming.value) await this.host.interject(prepared.inputText, options);
    else await this.host.send(options);
  }
  queueDiffComment(comment: DiffComment): void {
    const sessionId = this.host.activeSessionId.peek();
    const replaced = this.diff.value.comments.find(
      (entry) =>
        entry.sessionId === sessionId &&
        entry.path === comment.path &&
        entry.side === comment.side &&
        entry.line === comment.line,
    );
    const value: DiffComment = {
      ...comment,
      id: comment.id || replaced?.id || uuid(),
      sessionId,
      scope: normalizeDiffScope(comment.scope || this.diff.peek().scope),
      createdAt: comment.createdAt || replaced?.createdAt || Date.now(),
      updatedAt: Date.now(),
      state: 'fresh',
      anchorFingerprint: comment.anchorFingerprint || reviewAnchorFingerprint(comment),
    };
    const validation =
      validateReviewComment(value) ||
      validateReviewBatch([
        ...this.diff.value.comments.filter(
          (entry) => entry.sessionId === sessionId && entry.id !== value.id && entry !== replaced,
        ),
        value,
      ]);
    if (validation) {
      this.services.bumpDiagnostic('queueValidationFailures');
      this.services.toast(validation.message, 'error');
      return;
    }
    try {
      persistDiffComment(this.services.storage, this.services.keys.diffCommentQueue, value);
      if (replaced?.id && replaced.id !== value.id)
        removeDiffComment(
          this.services.storage,
          this.services.keys.diffCommentQueue,
          sessionId,
          replaced.id,
        );
    } catch (error) {
      this.services.toast(error, 'error');
      return;
    }
    const comments = [
      ...this.diff.value.comments.filter((entry) => entry.id !== value.id && entry !== replaced),
      value,
    ];
    this.diff.value = { ...this.diff.value, comments };
    this.host.publishSessionChange('review-comment-changed', sessionId);
  }

  editDiffComment(commentId: string, body: string): void {
    const comment = this.diff.peek().comments.find((entry) => entry.id === commentId);
    if (!comment?.id || !comment.sessionId || !body.trim()) return;
    const updated = { ...comment, body: body.trim(), updatedAt: Date.now() };
    const validation =
      validateReviewComment(updated) ||
      validateReviewBatch(
        this.diff
          .peek()
          .comments.map((entry) => (entry.id === commentId ? updated : entry))
          .filter((entry) => entry.sessionId === comment.sessionId),
      );
    if (validation) {
      this.services.bumpDiagnostic('queueValidationFailures');
      this.services.toast(validation.message, 'error');
      return;
    }
    try {
      persistDiffComment(this.services.storage, this.services.keys.diffCommentQueue, updated);
    } catch (error) {
      this.services.toast(error, 'error');
      return;
    }
    this.diff.value = {
      ...this.diff.peek(),
      comments: this.diff
        .peek()
        .comments.map((entry) => (entry.id === commentId ? updated : entry)),
    };
    this.host.publishSessionChange('review-comment-changed', comment.sessionId);
  }

  reanchorDiffComment(
    commentId: string,
    anchor: Pick<DiffComment, 'path' | 'side' | 'line' | 'context' | 'fileChangeSeq' | 'scope'>,
  ): void {
    const comment = this.diff.peek().comments.find((entry) => entry.id === commentId);
    if (!comment) return;
    this.queueDiffComment({
      ...comment,
      ...anchor,
      state: 'fresh',
      anchorFingerprint: '',
      updatedAt: Date.now(),
    });
    const updated = this.diff.peek().comments.find((entry) => entry.id === commentId);
    if (updated?.state === 'fresh')
      this.services.toast(`Re-anchored comment to ${anchor.path}:${anchor.line}.`, 'success');
  }

  removeDiffComment(commentId: string): void {
    const comment = this.diff.peek().comments.find((entry) => entry.id === commentId);
    if (!comment?.id || !comment.sessionId) return;
    removeDiffComment(
      this.services.storage,
      this.services.keys.diffCommentQueue,
      comment.sessionId,
      comment.id,
    );
    this.diff.value = {
      ...this.diff.peek(),
      comments: this.diff.peek().comments.filter((entry) => entry.id !== commentId),
    };
    this.host.publishSessionChange('review-comment-changed', comment.sessionId);
  }

  discardDiffComments(sessionId = this.host.activeSessionId.peek()): void {
    if (!sessionId) return;
    clearSessionDiffComments(this.services.storage, this.services.keys.diffCommentQueue, sessionId);
    this.diff.value = {
      ...this.diff.peek(),
      comments: this.diff.peek().comments.filter((entry) => entry.sessionId !== sessionId),
    };
    this.host.publishSessionChange('review-comment-changed', sessionId);
  }
  async sendDiffComments(): Promise<void> {
    const session = this.host.activeSession.value;
    const comments = this.diff.value.comments.filter(
      (comment) => !comment.sessionId || comment.sessionId === session?.id,
    );
    if (!session || !comments.length) return;
    const staleCount = comments.filter((comment) => comment.state === 'stale').length;
    if (
      staleCount &&
      !window.confirm(
        `${staleCount} comment${staleCount === 1 ? '' : 's'} no longer match the current source. Send anyway?`,
      )
    )
      return;
    const prepared = this.prepareDiffComments(
      staleCount ? comments.map((comment) => ({ ...comment, state: 'fresh' as const })) : comments,
    );
    if (!prepared) return;
    const markComments = (state: DiffComment['state'], error = ''): boolean => {
      const updates = new Map(
        comments.map((comment) => [
          comment.id,
          { ...comment, state, error: error || undefined, updatedAt: Date.now() },
        ]),
      );
      try {
        updates.forEach((comment) =>
          persistDiffComment(this.services.storage, this.services.keys.diffCommentQueue, comment),
        );
      } catch (persistError) {
        this.services.toast(persistError, 'error');
        return false;
      }
      this.diff.value = {
        ...this.diff.peek(),
        comments: this.diff.peek().comments.map((comment) => updates.get(comment.id) || comment),
      };
      return true;
    };
    if (!markComments('sending')) return;
    const { payloads, inputText } = prepared;
    const options: SendOptions = {
      contentParts: payloads.map((diff_comment) => ({ type: 'diff_comment', diff_comment })),
      inputText,
      displayContent:
        comments.length === 1 ? comments[0].body : `${comments.length} inline comments`,
      preserveComposer: true,
      diffComments: comments,
      onTransportStarted: () => {
        const sent = comments.map((comment) => ({
          ...comment,
          sessionId: session.id,
          createdAt: comment.createdAt || Date.now(),
          optimistic: true,
        }));
        const sentIDs = new Set(sent.map((comment) => comment.id));
        const current = this.diff.peek();
        const remaining = current.comments.filter(
          (comment) => comment.sessionId && comment.sessionId !== session.id,
        );
        this.diff.value = {
          ...current,
          comments: remaining,
          historyComments: [
            ...current.historyComments.filter((comment) => !sentIDs.has(comment.id)),
            ...sent,
          ],
        };
        comments.forEach((comment) => {
          if (comment.id && (comment.sessionId || session.id))
            removeDiffComment(
              this.services.storage,
              this.services.keys.diffCommentQueue,
              comment.sessionId || session.id,
              comment.id,
            );
        });
        this.host.publishSessionChange('review-comment-changed', session.id);
        void this.refreshDiffComments(session.id);
      },
      onTransportFailed: (error) => {
        markComments('failed', errorMessage(error));
        this.services.toast(error, 'error');
      },
    };
    if (this.host.streaming.value) await this.host.interject(inputText, options);
    else await this.host.send(options);
  }
  resizeDiff(width: number): void {
    this.diff.value = { ...this.diff.value, width };
    this.services.storage.setItem(this.services.keys.diffSidebarWidth, String(Math.round(width)));
  }
}
