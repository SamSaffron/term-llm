import type { Signal } from '@preact/signals';
import { isMarkdownPath, normalizeDiffScope } from '../domain/diff';
import { reviewAnchorFingerprint } from '../domain/review-policy';
import { errorMessage } from '../domain/text';
import type { DiffComment, DiffFile } from '../domain/types';
import { persistDiffComment } from '../platform/storage';
import type { AppStoreServices } from './app-store-services';
import type { ReviewStoreHost } from './review-store';
import type { DiffState } from './store-types';

export interface MarkdownPreviewStoreContext {
  services: AppStoreServices;
  host: ReviewStoreHost;
  diff: Signal<DiffState>;
}

const requests = new Map<string, { epoch: number; controller: AbortController }>();

export async function setMarkdownView(
  context: MarkdownPreviewStoreContext,
  file: DiffFile,
  view: 'diff' | 'rendered',
): Promise<void> {
  const scope = normalizeDiffScope(context.diff.peek().scope);
  const side = ['delete', 'deleted', 'remove', 'removed'].includes(
    String(file.status || '').toLowerCase(),
  )
    ? 'before'
    : 'after';
  context.diff.value = {
    ...context.diff.peek(),
    files: context.diff.peek().files.map((entry) =>
      entry.path === file.path &&
      (entry.sequence || 0) === (file.sequence || 0) &&
      (entry.snapshotSeq || 0) === (file.snapshotSeq || 0)
        ? {
            ...entry,
            expanded:
              view === 'rendered' ? true : entry.lines === undefined ? false : entry.expanded,
            markdownPreview: {
              ...entry.markdownPreview,
              view,
              side,
              sequence: entry.sequence || 0,
              snapshotSeq: entry.snapshotSeq || 0,
              scope,
            },
          }
        : entry,
    ),
  };
  if (view === 'rendered') await loadMarkdownPreview(context, file);
}

export async function loadMarkdownPreview(
  context: MarkdownPreviewStoreContext,
  file: DiffFile,
  force = false,
): Promise<void> {
  const session = context.host.activeSession.value;
  if (!session || context.diff.peek().worktreeDir) return;
  const owner = session.id;
  const scope = normalizeDiffScope(context.diff.peek().scope);
  const side: 'before' | 'after' = ['delete', 'deleted', 'remove', 'removed'].includes(
    String(file.status || '').toLowerCase(),
  )
    ? 'before'
    : 'after';
  const sequence = file.sequence || 0;
  const snapshotSeq = file.snapshotSeq || 0;
  const current = context.diff.peek().files.find((entry) => entry.path === file.path);
  if (!force && current?.markdownPreview?.source !== undefined) return;

  const requestKey = `${owner}\u0000${scope}\u0000${file.path}`;
  const previous = requests.get(requestKey);
  previous?.controller.abort();
  const controller = new AbortController();
  const requestEpoch = (previous?.epoch || 0) + 1;
  requests.set(requestKey, { epoch: requestEpoch, controller });
  const matches = (entry: DiffFile): boolean =>
    entry.path === file.path &&
    (entry.sequence || 0) === sequence &&
    (entry.snapshotSeq || 0) === snapshotSeq;
  context.diff.value = {
    ...context.diff.peek(),
    files: context.diff.peek().files.map((entry) =>
      matches(entry)
        ? {
            ...entry,
            markdownPreview: {
              view: entry.markdownPreview?.view || 'diff',
              side,
              source: entry.markdownPreview?.source,
              blocks: entry.markdownPreview?.blocks,
              loading: true,
              error: '',
              sequence,
              snapshotSeq,
              scope,
            },
          }
        : entry,
    ),
  };
  try {
    const source = await context.services.endpoints.fileText(
      owner,
      file.path,
      scope,
      side,
      snapshotSeq,
      controller.signal,
    );
    const request = requests.get(requestKey);
    if (
      request?.epoch !== requestEpoch ||
      context.host.activeSessionId.peek() !== owner ||
      context.diff.peek().sessionId !== owner ||
      normalizeDiffScope(context.diff.peek().scope) !== scope
    )
      return;
    if (!context.diff.peek().files.some(matches)) return;
    const { markdownDocumentBlocks } = await import('../domain/markdown-document');
    const parsedBlocks = markdownDocumentBlocks(source);
    if (!context.diff.peek().files.some(matches)) return;
    const blocks = parsedBlocks.map(
      ({ id, type, startLine, endLine, anchorLine, commentable }) => ({
        id,
        type,
        startLine,
        endLine,
        anchorLine,
        commentable,
      }),
    );
    context.diff.value = {
      ...context.diff.peek(),
      files: context.diff.peek().files.map((entry) =>
        matches(entry)
          ? {
              ...entry,
              markdownPreview: {
                view: entry.markdownPreview?.view || 'diff',
                side,
                source,
                blocks,
                loading: false,
                error: '',
                sequence,
                snapshotSeq,
                scope,
              },
            }
          : entry,
      ),
    };
    revalidateMarkdownComments(context, file.path, side, source);
  } catch (error) {
    if (controller.signal.aborted || requests.get(requestKey)?.epoch !== requestEpoch) return;
    context.diff.value = {
      ...context.diff.peek(),
      files: context.diff.peek().files.map((entry) =>
        matches(entry)
          ? {
              ...entry,
              markdownPreview: {
                view: entry.markdownPreview?.view || 'diff',
                side,
                source: entry.markdownPreview?.source,
                blocks: entry.markdownPreview?.blocks,
                loading: false,
                error: errorMessage(error),
                sequence,
                snapshotSeq,
                scope,
              },
            }
          : entry,
      ),
    };
  } finally {
    if (requests.get(requestKey)?.epoch === requestEpoch) requests.delete(requestKey);
  }
}

function revalidateMarkdownComments(
  context: MarkdownPreviewStoreContext,
  path: string,
  previewSide: 'before' | 'after',
  source: string,
): void {
  const side = previewSide === 'before' ? 'old' : 'new';
  const lines = source.split(/\r?\n/);
  let changed = false;
  const comments = context.diff.peek().comments.map((comment) => {
    if (
      comment.path !== path ||
      comment.side !== side ||
      comment.state === 'stale' ||
      !comment.anchorFingerprint
    )
      return comment;
    const index = comment.line - 1;
    const beforeCount = comment.contextBefore?.length || 0;
    const afterCount = comment.contextAfter?.length || 0;
    const stale =
      index < 0 ||
      index >= lines.length ||
      reviewAnchorFingerprint({
        ...comment,
        context: lines[index],
        contextBefore: lines.slice(Math.max(0, index - beforeCount), index),
        contextAfter: lines.slice(index + 1, index + 1 + afterCount),
      }) !== comment.anchorFingerprint;
    if (!stale) return comment;
    changed = true;
    const updated: DiffComment = { ...comment, state: 'stale', updatedAt: Date.now() };
    try {
      persistDiffComment(context.services.storage, context.services.keys.diffCommentQueue, updated);
    } catch (error) {
      context.services.toast(error, 'error');
    }
    return updated;
  });
  if (changed) context.diff.value = { ...context.diff.peek(), comments };
}

export async function revalidateGitMarkdownComments(
  context: MarkdownPreviewStoreContext,
  comments: DiffComment[],
): Promise<boolean> {
  const scope = normalizeDiffScope(context.diff.peek().scope);
  if (scope === 'last_turn' || scope === 'last_3_turns') return true;
  const files = context.diff.peek().files.filter((file) => {
    const side = ['delete', 'deleted', 'remove', 'removed'].includes(
      String(file.status || '').toLowerCase(),
    )
      ? 'old'
      : 'new';
    return (
      isMarkdownPath(file.path) &&
      comments.some((comment) => comment.path === file.path && comment.side === side)
    );
  });
  for (const file of files) {
    await loadMarkdownPreview(context, file, true);
    const current = context.diff.peek().files.find((entry) => entry.path === file.path);
    const preview = current?.markdownPreview;
    if (preview?.source === undefined || preview.error) {
      context.services.toast(`Refresh ${file.path} before sending its rendered comments.`, 'error');
      return false;
    }
    const sourceLines = preview.source.split(/\r?\n/);
    const side = preview.side === 'before' ? 'old' : 'new';
    for (const comment of comments.filter(
      (entry) => entry.path === file.path && entry.side === side,
    )) {
      const index = comment.line - 1;
      const beforeCount = comment.contextBefore?.length || 0;
      const afterCount = comment.contextAfter?.length || 0;
      const originalFingerprint = comment.anchorFingerprint || reviewAnchorFingerprint(comment);
      const fresh =
        index >= 0 &&
        index < sourceLines.length &&
        reviewAnchorFingerprint({
          ...comment,
          context: sourceLines[index],
          contextBefore: sourceLines.slice(Math.max(0, index - beforeCount), index),
          contextAfter: sourceLines.slice(index + 1, index + 1 + afterCount),
        }) === originalFingerprint;
      if (!fresh) {
        if (comment.id && context.diff.peek().comments.some((entry) => entry.id === comment.id)) {
          const updated: DiffComment = { ...comment, state: 'stale', updatedAt: Date.now() };
          try {
            persistDiffComment(
              context.services.storage,
              context.services.keys.diffCommentQueue,
              updated,
            );
          } catch (error) {
            context.services.toast(error, 'error');
          }
          context.diff.value = {
            ...context.diff.peek(),
            comments: context.diff
              .peek()
              .comments.map((entry) => (entry.id === comment.id ? updated : entry)),
          };
        }
        context.services.toast(
          `${comment.path}:${comment.line} changed. Re-anchor the comment before sending.`,
          'error',
        );
        return false;
      }
    }
  }
  return true;
}
