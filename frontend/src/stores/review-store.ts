import { signal } from '@preact/signals';
import { normalizeDiffScope } from '../domain/diff-scope';
import type { DiffComment, DiffFile } from '../domain/types';
import { errorMessage } from '../domain/text';
import {
  reviewAnchorFingerprint,
  validateReviewBatch,
  validateReviewComment,
} from '../domain/review-policy';
import {
  clearSessionDiffComments,
  persistDiffComment,
  readDiffCommentQueue,
  removeDiffComment,
} from '../platform/storage';
import type { AppStoreServices } from './app-store-services';
import type { DiffState } from './store-types';
import { listFrom, recordValue, uuid } from './store-utils';
import type {
  ReviewOperationContext,
  ReviewOperationState,
  ReviewOperationsApi,
  ReviewStoreHost,
} from './review-types';

export type { ReviewStoreHost } from './review-types';

/**
 * Owns the eager diff signal and durable review comment queue. Diff fetching,
 * unified-diff parsing, expansion, revalidation, and send payload preparation
 * live behind the dynamic review-operations import so ordinary session startup
 * does not load them. Queueing, editing, and re-anchoring stay synchronous.
 */
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

  private readonly operationState: ReviewOperationState = {
    loadEpoch: 0,
    expandEpoch: new Map(),
  };
  private operationsPromise: Promise<ReviewOperationsApi> | null = null;

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
      if (this.services.isDisposed || this.diff.peek().sessionId !== sessionId) return;
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

  private markdownContext() {
    return {
      services: this.services,
      host: this.host,
      diff: this.diff,
    };
  }

  private operationContext(): ReviewOperationContext {
    return {
      services: this.services,
      host: this.host,
      diff: this.diff,
      state: this.operationState,
      refreshDiffComments: (sessionId) => this.refreshDiffComments(sessionId),
      loadMarkdownPreview: (file, force) => this.loadMarkdownPreview(file, force),
    };
  }

  /**
   * Loads the deferred implementation once per store. Concurrent callers share
   * the in-flight import; a rejected chunk fetch clears the cache so a later
   * attempt can retry instead of poisoning the session.
   */
  private operations(): Promise<ReviewOperationsApi> {
    if (!this.operationsPromise) {
      const promise = import('./review-operations').then(
        ({ ReviewOperations }) => new ReviewOperations(this.operationContext()),
      );
      this.operationsPromise = promise;
      void promise.catch(() => {
        if (this.operationsPromise === promise) this.operationsPromise = null;
      });
    }
    return this.operationsPromise;
  }

  async toggleDiff(): Promise<void> {
    const session = this.host.activeSession.value;
    if (!session) return;
    this.host.closePlan();
    this.diff.value = { ...this.diff.value, sessionId: session.id, open: !this.diff.value.open };
    this.host.restartStatusPoll();
    if (this.diff.value.open) await this.loadDiff(true);
  }

  async openWorktreeDiff(dir: string, title: string): Promise<void> {
    const owner = this.host.activeSessionId.peek();
    if (!owner || !dir) return;
    this.host.closePlan();
    const epoch = ++this.operationState.loadEpoch;
    this.diff.value = {
      ...this.diff.peek(),
      open: true,
      maximized: false,
      sessionId: owner,
      git: true,
      loading: true,
      files: [],
      error: '',
      selectedPath: '',
      followCurrentFile: false,
      worktreeDir: dir,
      worktreeTitle: title,
      readOnly: true,
    };
    try {
      const operations = await this.operations();
      if (!this.isOwnedWorktree(owner, dir, epoch)) return;
      await operations.openWorktreeDiff(owner, dir, title, epoch);
    } catch (error) {
      if (!this.isOwnedWorktree(owner, dir, epoch)) return;
      this.diff.value = { ...this.diff.peek(), loading: false, error: errorMessage(error) };
    }
  }

  async loadDiff(replaceWorktree = false): Promise<void> {
    if (this.diff.peek().worktreeDir && !replaceWorktree) return;
    const session = this.host.activeSession.value;
    if (!session) return;
    const owner = session.id;
    const scope = normalizeDiffScope(this.diff.value.scope);
    const epoch = ++this.operationState.loadEpoch;
    this.diff.value = {
      ...this.diff.value,
      sessionId: owner,
      scope,
      loading: true,
      error: '',
      worktreeDir: undefined,
      worktreeTitle: undefined,
      readOnly: false,
    };
    void this.refreshDiffComments(owner);
    try {
      const operations = await this.operations();
      if (!this.isOwnedLoad(owner, scope, epoch)) return;
      await operations.loadDiff(owner, scope, epoch);
    } catch (error) {
      if (!this.isOwnedLoad(owner, scope, epoch)) return;
      this.diff.value = { ...this.diff.value, loading: false, error: errorMessage(error) };
    }
  }

  async expandDiff(file: DiffFile, context = 0, refresh = false): Promise<void> {
    const session = this.host.activeSession.value;
    if (!session) return;
    const owner = session.id;
    const scope = normalizeDiffScope(this.diff.value.scope);
    const isRequestedVersion = (entry: DiffFile): boolean =>
      entry.path === file.path &&
      (entry.sequence || 0) === (file.sequence || 0) &&
      (entry.snapshotSeq || 0) === (file.snapshotSeq || 0);
    if (file.lines && !context && !refresh) {
      this.diff.value = {
        ...this.diff.value,
        files: this.diff.value.files.map((entry) =>
          isRequestedVersion(entry) ? { ...entry, expanded: !entry.expanded } : entry,
        ),
      };
      return;
    }
    const worktreeDir = this.diff.peek().worktreeDir;
    if (worktreeDir) {
      if (!file.lines)
        await this.openWorktreeDiff(worktreeDir, this.diff.peek().worktreeTitle || 'Worktree');
      return;
    }
    this.diff.value = {
      ...this.diff.value,
      files: this.diff.value.files.map((entry) =>
        isRequestedVersion(entry) ? { ...entry, expanded: true, loading: true, error: '' } : entry,
      ),
    };
    try {
      const operations = await this.operations();
      if (!this.isExpansionOwnerCurrent(owner, scope)) return;
      await operations.expandDiff(file, owner, scope, context, refresh);
    } catch (error) {
      if (!this.isExpansionOwnerCurrent(owner, scope)) return;
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
    }
  }

  async setMarkdownView(file: DiffFile, view: 'diff' | 'rendered'): Promise<void> {
    const { setMarkdownView } = await import('./markdown-preview-store');
    await setMarkdownView(this.markdownContext(), file, view);
  }

  async loadMarkdownPreview(file: DiffFile, force = false): Promise<void> {
    const { loadMarkdownPreview } = await import('./markdown-preview-store');
    await loadMarkdownPreview(this.markdownContext(), file, force);
  }

  async revealDiffLine(file: DiffFile, side: 'old' | 'new', line: number): Promise<void> {
    await this.setMarkdownView(file, 'diff');
    await this.expandDiff(file, 100_000);
    requestAnimationFrame(() => {
      const fileRoot = document.querySelector<HTMLElement>(
        `[data-diff-file-path="${CSS.escape(file.path)}"]`,
      );
      const target = fileRoot?.querySelector<HTMLElement>(`[data-diff-anchor="${side}:${line}"]`);
      target?.scrollIntoView({ block: 'center' });
      target?.focus({ preventScroll: true });
    });
  }

  async sendDiffComment(comment: DiffComment): Promise<void> {
    const owner = this.host.activeSession.value?.id || '';
    if (!owner || this.services.isDisposed) return;
    try {
      const operations = await this.operations();
      if (!this.isOperationOwnerCurrent(owner)) return;
      await operations.sendDiffComment(owner, comment);
    } catch (error) {
      if (this.isOperationOwnerCurrent(owner)) this.services.toast(error, 'error');
    }
  }

  async sendDiffComments(): Promise<void> {
    const owner = this.host.activeSession.value?.id || '';
    if (!owner || this.services.isDisposed) return;
    try {
      const operations = await this.operations();
      if (!this.isOperationOwnerCurrent(owner)) return;
      await operations.sendDiffComments(owner);
    } catch (error) {
      if (this.isOperationOwnerCurrent(owner)) this.services.toast(error, 'error');
    }
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

  resizeDiff(width: number): void {
    this.diff.value = { ...this.diff.value, width };
    this.services.storage.setItem(this.services.keys.diffSidebarWidth, String(Math.round(width)));
  }

  private isOwnedLoad(owner: string, scope: string, epoch: number): boolean {
    return (
      !this.services.isDisposed &&
      this.operationState.loadEpoch === epoch &&
      this.host.activeSessionId.peek() === owner &&
      this.diff.peek().sessionId === owner &&
      normalizeDiffScope(this.diff.peek().scope) === scope
    );
  }

  private isOwnedWorktree(owner: string, dir: string, epoch: number): boolean {
    return (
      !this.services.isDisposed &&
      this.operationState.loadEpoch === epoch &&
      this.host.activeSessionId.peek() === owner &&
      this.diff.peek().worktreeDir === dir
    );
  }

  private isExpansionOwnerCurrent(owner: string, scope: string): boolean {
    return (
      !this.services.isDisposed &&
      this.host.activeSessionId.peek() === owner &&
      this.host.activeSession.value?.id === owner &&
      this.diff.peek().sessionId === owner &&
      normalizeDiffScope(this.diff.peek().scope) === scope
    );
  }

  private isOperationOwnerCurrent(owner: string): boolean {
    return (
      !this.services.isDisposed &&
      this.host.activeSessionId.peek() === owner &&
      this.host.activeSession.value?.id === owner
    );
  }
}
