import { normalizeDiffScope } from '../domain/diff-scope';
import {
  linesFromHunks,
  parseUnifiedDiffFiles,
  parseUnifiedPatch,
  sortDiffFiles,
} from '../domain/diff';
import {
  reviewAnchorFingerprint,
  reviewCommentPayload,
  validateReviewBatch,
} from '../domain/review-policy';
import { errorMessage } from '../domain/text';
import type { DiffComment, DiffFile, FilesystemObservation } from '../domain/types';
import { persistDiffComment, removeDiffComment } from '../platform/storage';
import type { SendOptions } from './store-types';
import { listFrom, uuid } from './store-utils';
import type { ReviewOperationContext } from './review-types';

/**
 * Deferred review implementation: diff fetching, unified-diff parsing,
 * expansion, comment revalidation, and send payload preparation.
 *
 * This module is loaded only through a dynamic import from review-store.ts.
 * Keep eager imports (and domain/diff.ts) out of review-store.ts so ordinary
 * session startup does not parse, hash, or bundle this work. Review comments
 * themselves stay in the eager queue layer because queueing, editing, and
 * re-anchoring are synchronous, validated actions.
 */
export class ReviewOperations {
  constructor(private readonly context: ReviewOperationContext) {}

  async loadDiff(owner: string, scope: string, epoch: number): Promise<void> {
    const { services, diff } = this.context;
    try {
      if (!this.isCurrentLoad(owner, scope, epoch)) return;
      const data = await services.endpoints.fileChanges(owner, scope);
      const state = diff.peek();
      if (!this.isCurrentLoad(owner, scope, epoch)) return;
      const existing = new Map(state.files.map((file) => [file.path, file]));
      const turnScope = scope === 'last_turn' || scope === 'last_3_turns';
      const refreshContexts = new Map<string, number>();
      const refreshMarkdown = new Set<string>();
      const files = sortDiffFiles(
        listFrom(data, 'file_changes', 'files', 'changes')
          .map((entry): DiffFile => {
            const path = String(entry.path || '');
            const previous = existing.get(path);
            const sequence = Number(entry.seq ?? entry.sequence) || 0;
            const snapshotSeq = Number(entry.snapshot_seq) || 0;
            const sameContent = Boolean(
              turnScope &&
              previous &&
              (previous.sequence || 0) === sequence &&
              (previous.snapshotSeq || 0) === snapshotSeq,
            );
            if (previous?.expanded && !sameContent)
              refreshContexts.set(path, previous.context || 0);
            const status = String(entry.status || entry.kind || '');
            const previewSide = status.toLowerCase() === 'delete' ? 'before' : 'after';
            const preservePreview = Boolean(
              turnScope && sameContent && previous?.markdownPreview?.scope === scope,
            );
            const refreshRenderedPreview = Boolean(
              previous?.markdownPreview?.view === 'rendered' &&
              (!preservePreview ||
                previous.markdownPreview.source === undefined ||
                Boolean(previous.markdownPreview.error)),
            );
            if (refreshRenderedPreview) refreshMarkdown.add(path);
            const markdownPreview =
              preservePreview && !refreshRenderedPreview
                ? previous?.markdownPreview
                : refreshRenderedPreview
                  ? {
                      ...previous?.markdownPreview,
                      view: 'rendered' as const,
                      side: previewSide as 'before' | 'after',
                      sequence,
                      snapshotSeq,
                      scope,
                      loading: true,
                      error: '',
                    }
                  : undefined;
            const preserveExpandedDiff = Boolean(previous?.expanded && previous.lines);
            return {
              path,
              old_path: String(entry.old_path || ''),
              status,
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
              lines: sameContent || preserveExpandedDiff ? previous?.lines : undefined,
              patch: sameContent || preserveExpandedDiff ? previous?.patch : undefined,
              context: sameContent || preserveExpandedDiff ? previous?.context : undefined,
              oldLineCount:
                sameContent || preserveExpandedDiff ? previous?.oldLineCount : undefined,
              newLineCount:
                sameContent || preserveExpandedDiff ? previous?.newLineCount : undefined,
              lang: sameContent || preserveExpandedDiff ? previous?.lang : undefined,
              markdownPreview,
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
        const commentTurnScope = ['last_turn', 'last_3_turns'].includes(
          normalizeDiffScope(comment.scope || scope),
        );
        const file = files.find((entry) => entry.path === comment.path);
        let anchorChanged = !file;
        if (file?.lines && comment.anchorFingerprint) {
          const index = file.lines.findIndex((line) =>
            comment.side === 'old' ? line.oldLine === comment.line : line.newLine === comment.line,
          );
          // A missing line in a partial hunk is unknown, not stale. Complete
          // preview source or a later context expansion can validate it.
          if (index >= 0) {
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
          (commentTurnScope &&
            Boolean(comment.fileChangeSeq && (file?.sequence || 0) > comment.fileChangeSeq));
        if (!stale) return comment;
        newlyStale += 1;
        const updated: DiffComment = { ...comment, state: 'stale', updatedAt: Date.now() };
        try {
          persistDiffComment(services.storage, services.keys.diffCommentQueue, updated);
        } catch (error) {
          services.toast(error, 'error');
        }
        return updated;
      });
      diff.value = {
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
        services.toast(
          `${newlyStale} queued comment${newlyStale === 1 ? '' : 's'} became stale after the source changed.`,
          'info',
        );
      await Promise.all([
        ...files
          .filter((file) => refreshContexts.has(file.path))
          .map((file) =>
            this.expandDiff(file, owner, scope, refreshContexts.get(file.path) || 0, true),
          ),
        ...files
          .filter((file) => refreshMarkdown.has(file.path))
          .map((file) => this.context.loadMarkdownPreview(file, true)),
      ]);
    } catch (error) {
      if (!this.isCurrentLoad(owner, scope, epoch)) return;
      diff.value = {
        ...diff.value,
        loading: false,
        error: errorMessage(error),
      };
    }
  }

  async openWorktreeDiff(owner: string, dir: string, title: string, epoch: number): Promise<void> {
    const { services, host, diff } = this.context;
    try {
      if (!this.isCurrentWorktree(owner, dir, epoch)) return;
      const data = await services.endpoints.worktreeDiff(
        host.activeSession.value?.projectId || '',
        dir,
      );
      if (!this.isCurrentWorktree(owner, dir, epoch)) return;
      const parsed = sortDiffFiles(parseUnifiedDiffFiles(String(data.diff || data.patch || '')));
      const files =
        data.truncated === true
          ? parsed.map((file, index) => ({
              ...file,
              truncated: index === parsed.length - 1 || file.truncated,
            }))
          : parsed;
      diff.value = {
        ...diff.peek(),
        files,
        loading: false,
        error:
          data.truncated === true
            ? 'This worktree diff was truncated; showing the available changes.'
            : '',
      };
    } catch (error) {
      if (!this.isCurrentWorktree(owner, dir, epoch)) return;
      diff.value = { ...diff.peek(), loading: false, error: errorMessage(error) };
    }
  }

  async expandDiff(
    file: DiffFile,
    owner: string,
    scope: string,
    context = 0,
    refresh = false,
  ): Promise<void> {
    const { services, diff, state } = this.context;
    if (!this.isOwnerContext(owner, scope)) return;
    const isRequestedVersion = (entry: DiffFile): boolean =>
      entry.path === file.path &&
      (entry.sequence || 0) === (file.sequence || 0) &&
      (entry.snapshotSeq || 0) === (file.snapshotSeq || 0);
    if (file.lines && !context && !refresh) {
      diff.value = {
        ...diff.value,
        files: diff.value.files.map((entry) =>
          isRequestedVersion(entry) ? { ...entry, expanded: !entry.expanded } : entry,
        ),
      };
      return;
    }
    // A worktree diff owns whole-patch parsing; the eager layer forwards that
    // path before importing this module.
    if (diff.peek().worktreeDir) return;
    const requestKey = `${owner}\u0000${scope}\u0000${file.path}`;
    const requestEpoch = (state.expandEpoch.get(requestKey) || 0) + 1;
    state.expandEpoch.set(requestKey, requestEpoch);
    diff.value = {
      ...diff.value,
      files: diff.value.files.map((entry) =>
        isRequestedVersion(entry) ? { ...entry, expanded: true, loading: true, error: '' } : entry,
      ),
    };
    try {
      const data = await services.endpoints.fileDiff(
        owner,
        file.path,
        scope,
        context,
        file.snapshotSeq || 0,
      );
      if (!this.isCurrentExpansion(requestKey, requestEpoch, owner, scope)) return;
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
          ? services.endpoints.fileContentURL(
              owner,
              file.path,
              scope,
              'before',
              file.snapshotSeq || 0,
            )
          : String(data.before_url || file.beforeURL || '');
      const afterURL =
        image && !['delete', 'deleted', 'remove', 'removed'].includes(status)
          ? services.endpoints.fileContentURL(
              owner,
              file.path,
              scope,
              'after',
              file.snapshotSeq || 0,
            )
          : String(data.after_url || file.afterURL || '');
      diff.value = {
        ...diff.value,
        files: diff.value.files.map((entry) =>
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
      this.revalidateExpandedDiffComments(file.path, lines || []);
    } catch (error) {
      if (!this.isCurrentExpansion(requestKey, requestEpoch, owner, scope)) return;
      diff.value = {
        ...diff.value,
        files: diff.value.files.map((entry) =>
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
      if (state.expandEpoch.get(requestKey) === requestEpoch) state.expandEpoch.delete(requestKey);
    }
  }

  async sendDiffComments(owner: string): Promise<void> {
    const { services, host, diff } = this.context;
    const session = host.activeSession.value;
    if (!session || session.id !== owner || !this.isOwnerCurrent(owner)) return;
    let comments = diff.value.comments.filter(
      (comment) => !comment.sessionId || comment.sessionId === owner,
    );
    if (!comments.length) return;
    if (!(await this.revalidateGitMarkdownComments(comments))) return;
    if (!this.isOwnerCurrent(owner)) return;
    comments = diff.value.comments.filter(
      (comment) => !comment.sessionId || comment.sessionId === owner,
    );
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
    if (!this.isOwnerCurrent(owner)) return;
    const markComments = (state: DiffComment['state'], error = ''): boolean => {
      const updates = new Map(
        comments.map((comment) => [
          comment.id,
          { ...comment, state, error: error || undefined, updatedAt: Date.now() },
        ]),
      );
      try {
        updates.forEach((comment) =>
          persistDiffComment(services.storage, services.keys.diffCommentQueue, comment),
        );
      } catch (persistError) {
        services.toast(persistError, 'error');
        return false;
      }
      diff.value = {
        ...diff.peek(),
        comments: diff.peek().comments.map((comment) => updates.get(comment.id) || comment),
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
          sessionId: owner,
          createdAt: comment.createdAt || Date.now(),
          optimistic: true,
        }));
        const sentIDs = new Set(sent.map((comment) => comment.id));
        const current = diff.peek();
        const remaining = current.comments.filter(
          (comment) => comment.sessionId && comment.sessionId !== owner,
        );
        diff.value = {
          ...current,
          comments: remaining,
          historyComments: [
            ...current.historyComments.filter((comment) => !sentIDs.has(comment.id)),
            ...sent,
          ],
        };
        comments.forEach((comment) => {
          if (comment.id && (comment.sessionId || owner))
            removeDiffComment(
              services.storage,
              services.keys.diffCommentQueue,
              comment.sessionId || owner,
              comment.id,
            );
        });
        host.publishSessionChange('review-comment-changed', owner);
        void this.context.refreshDiffComments(owner);
      },
      onTransportFailed: (error) => {
        markComments('failed', errorMessage(error));
        services.toast(error, 'error');
      },
    };
    if (host.streaming.value) await host.steer(inputText, options);
    else await host.send(options);
  }

  async sendDiffComment(owner: string, comment: DiffComment): Promise<void> {
    const { services, host, diff } = this.context;
    const session = host.activeSession.value;
    if (!session || session.id !== owner || !this.isOwnerCurrent(owner)) return;
    if (comment.sessionId && comment.sessionId !== owner) return;
    if (!(await this.revalidateGitMarkdownComments([comment]))) return;
    if (!this.isOwnerCurrent(owner)) return;
    const value: DiffComment = {
      ...comment,
      id: comment.id || uuid(),
      sessionId: owner,
      createdAt: comment.createdAt || Date.now(),
      optimistic: true,
    };
    const prepared = this.prepareDiffComments([value]);
    if (!prepared) return;
    diff.value = {
      ...diff.peek(),
      historyComments: [
        ...diff.peek().historyComments.filter((entry) => entry.id !== value.id),
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
        void this.context.refreshDiffComments(owner);
      },
      onTransportFailed: (error) => {
        diff.value = {
          ...diff.peek(),
          historyComments: diff
            .peek()
            .historyComments.filter((entry) => entry.id !== value.id || !entry.optimistic),
        };
        services.toast(error, 'error');
      },
    };
    if (host.streaming.value) await host.steer(prepared.inputText, options);
    else await host.send(options);
  }

  private revalidateExpandedDiffComments(
    path: string,
    lines: NonNullable<DiffFile['lines']>,
  ): void {
    const { services, diff } = this.context;
    const state = diff.peek();
    let newlyStale = 0;
    const comments = state.comments.map((comment) => {
      if (
        comment.path !== path ||
        (comment.sessionId && comment.sessionId !== state.sessionId) ||
        comment.state === 'stale' ||
        !comment.anchorFingerprint
      )
        return comment;
      const index = lines.findIndex((line) =>
        comment.side === 'old' ? line.oldLine === comment.line : line.newLine === comment.line,
      );
      // Expanded diffs can still contain partial hunks. A missing line is
      // unknown until complete source or broader context is available.
      if (index < 0) return comment;
      const beforeCount = comment.contextBefore?.length || 0;
      const afterCount = comment.contextAfter?.length || 0;
      const currentAnchor: DiffComment = {
        ...comment,
        context: lines[index].content,
        contextBefore: lines
          .slice(Math.max(0, index - beforeCount), index)
          .map((line) => line.content),
        contextAfter: lines.slice(index + 1, index + 1 + afterCount).map((line) => line.content),
      };
      if (reviewAnchorFingerprint(currentAnchor) === comment.anchorFingerprint) return comment;
      newlyStale += 1;
      const updated: DiffComment = { ...comment, state: 'stale', updatedAt: Date.now() };
      try {
        persistDiffComment(services.storage, services.keys.diffCommentQueue, updated);
      } catch (error) {
        services.toast(error, 'error');
      }
      return updated;
    });
    if (!newlyStale) return;
    diff.value = { ...diff.peek(), comments };
    services.toast(
      `${newlyStale} queued comment${newlyStale === 1 ? '' : 's'} became stale after the source changed.`,
      'info',
    );
  }

  private async revalidateGitMarkdownComments(comments: DiffComment[]): Promise<boolean> {
    const { revalidateGitMarkdownComments } = await import('./markdown-preview-store');
    return revalidateGitMarkdownComments(
      { services: this.context.services, host: this.context.host, diff: this.context.diff },
      comments,
    );
  }

  private prepareDiffComments(comments: DiffComment[]): {
    payloads: Array<Record<string, unknown>>;
    inputText: string;
  } | null {
    const { services } = this.context;
    const validation = validateReviewBatch(comments);
    if (validation) {
      services.bumpDiagnostic('queueValidationFailures');
      services.toast(validation.message, 'error');
      return null;
    }
    const stale = comments.find((comment) => comment.state === 'stale');
    if (stale) {
      services.toast(
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
      services.toast(
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

  private isCurrentLoad(owner: string, scope: string, epoch: number): boolean {
    const { services, host, diff, state } = this.context;
    return (
      !services.isDisposed &&
      state.loadEpoch === epoch &&
      host.activeSessionId.peek() === owner &&
      diff.peek().sessionId === owner &&
      normalizeDiffScope(diff.peek().scope) === scope
    );
  }

  private isCurrentWorktree(owner: string, dir: string, epoch: number): boolean {
    const { services, host, diff, state } = this.context;
    return (
      !services.isDisposed &&
      state.loadEpoch === epoch &&
      host.activeSessionId.peek() === owner &&
      diff.peek().worktreeDir === dir
    );
  }

  private isCurrentExpansion(
    requestKey: string,
    requestEpoch: number,
    owner: string,
    scope: string,
  ): boolean {
    return (
      this.isOwnerContext(owner, scope) &&
      this.context.state.expandEpoch.get(requestKey) === requestEpoch
    );
  }

  private isOwnerContext(owner: string, scope: string): boolean {
    const { diff } = this.context;
    return (
      this.isOwnerCurrent(owner) &&
      diff.peek().sessionId === owner &&
      normalizeDiffScope(diff.peek().scope) === scope
    );
  }

  private isOwnerCurrent(owner: string): boolean {
    const { services, host } = this.context;
    return (
      !services.isDisposed &&
      host.activeSessionId.peek() === owner &&
      host.activeSession.value?.id === owner
    );
  }
}
