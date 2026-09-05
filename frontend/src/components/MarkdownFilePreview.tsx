import { memo } from './memo';
import { useEventCallback } from './useEventCallback';
import { useMemo } from 'preact/hooks';
import { useStore } from '../app/context';
import type { DiffComment, DiffFile } from '../domain/types';
import {
  applyDocumentURLPolicy,
  markdownDocumentBlocks,
  type RenderedMarkdownSourceBlock,
} from '../domain/markdown-document';
import { Markdown } from './Markdown';
import { ReviewComment, type ReviewCommentEntry } from './ReviewComment';
import '../styles/features/markdown-preview.css';

export interface MarkdownFilePreviewProps {
  file: DiffFile;
  commenting: string;
  setCommenting: (value: string | ((current: string) => string)) => void;
  drafts: Record<string, string>;
  setDrafts: (
    value: Record<string, string> | ((current: Record<string, string>) => Record<string, string>),
  ) => void;
  clearDraft: (key: string) => void;
  draftTargets: Record<string, string>;
  setDraftTargets: (
    value: Record<string, string> | ((current: Record<string, string>) => Record<string, string>),
  ) => void;
}

function blockLabel(type: string): string {
  const labels: Record<string, string> = {
    heading: 'Heading',
    paragraph: 'Paragraph',
    list: 'List',
    blockquote: 'Block quote',
    table: 'Table',
    code: 'Code block',
    hr: 'Thematic break',
    html: 'HTML block',
  };
  return labels[type] || 'Block';
}

const EMPTY_COMMENTS: ReviewCommentEntry[] = [];

interface PreviewBlockProps extends Pick<
  MarkdownFilePreviewProps,
  'file' | 'setCommenting' | 'setDrafts' | 'clearDraft' | 'setDraftTargets'
> {
  block: RenderedMarkdownSourceBlock;
  comments: ReviewCommentEntry[];
  body: string;
  draftTarget: string;
  commenting: boolean;
  sourceLines: string[];
  readOnly: boolean;
  editComment: (comment: DiffComment) => void;
  removeComment: (comment: DiffComment) => void;
  submit: (
    mode: 'send' | 'queue',
    line: number,
    key: string,
    before: string[],
    after: string[],
  ) => void;
}
const PreviewBlock = memo(function PreviewBlock({
  file,
  block,
  comments,
  body,
  draftTarget,
  commenting,
  sourceLines,
  readOnly,
  setCommenting,
  setDrafts,
  clearDraft,
  setDraftTargets,
  editComment,
  removeComment,
  submit,
}: PreviewBlockProps) {
  const store = useStore();
  const previewStale = Boolean(file.markdownPreview?.loading || file.markdownPreview?.error);
  const renderedSide: DiffComment['side'] = file.markdownPreview?.side === 'before' ? 'old' : 'new';
  const allowNew = block.commentable && !readOnly;
  const anchorKey = `${renderedSide}:${block.anchorLine}`;
  const blockFingerprint = `${block.type}:${block.startLine}:${block.endLine}:${block.source}`;
  const draftStale = Boolean(body && draftTarget && draftTarget !== blockFingerprint);
  const label = blockLabel(block.type);
  const range =
    block.startLine === block.endLine
      ? `line ${block.startLine}`
      : `lines ${block.startLine}–${block.endLine}`;
  const anchorIndex = block.anchorLine - 1;
  return (
    <div
      key={block.id}
      class={`diff-markdown-block${commenting ? ' commenting' : ''}`}
      data-commentable={allowNew}
      data-source-start={block.startLine || undefined}
      data-source-end={block.endLine || undefined}
    >
      <Markdown
        value={block.source}
        renderedHTML={block.html}
        documentPolicy={applyDocumentURLPolicy}
        variant="document"
        className="markdown-body diff-markdown-block-content"
      />
      {(allowNew || comments.length > 0) && (
        <ReviewComment
          controlId={`rendered-comment-${file.sequence || 0}-${block.id}`}
          commenting={commenting}
          comments={comments}
          body={body}
          affordanceLabel={
            comments.length
              ? `Show ${comments.length} comment${comments.length === 1 ? '' : 's'} for ${label}, ${range}`
              : `Comment on ${label}, ${range}`
          }
          regionLabel={`Comments for ${label}, ${range}`}
          heading={
            <>
              <span class="diff-comment-line-chip">
                {label} · {range}
              </span>
              <span>{renderedSide === 'old' ? 'Original' : 'Current'} version</span>
              {draftStale && (
                <button
                  class="diff-comment-reanchor-draft"
                  type="button"
                  onClick={() =>
                    setDraftTargets((current) => ({
                      ...current,
                      [anchorKey]: blockFingerprint,
                    }))
                  }
                >
                  Re-anchor draft here
                </button>
              )}
            </>
          }
          showCount
          showAnchorLine
          allowNew={allowNew}
          submitDisabled={draftStale || previewStale}
          reanchorDisabled={previewStale}
          onToggle={() => setCommenting((current) => (current === anchorKey ? '' : anchorKey))}
          onBody={(value) => {
            setDrafts((current) => ({ ...current, [anchorKey]: value }));
            if (!draftTarget)
              setDraftTargets((current) => ({
                ...current,
                [anchorKey]: blockFingerprint,
              }));
          }}
          onCancel={() => {
            clearDraft(anchorKey);
            setCommenting('');
          }}
          onSubmit={(mode) =>
            submit(
              mode,
              block.anchorLine,
              anchorKey,
              sourceLines.slice(Math.max(0, anchorIndex - 4), anchorIndex),
              sourceLines.slice(anchorIndex + 1, anchorIndex + 5),
            )
          }
          onEdit={editComment}
          onReanchor={(comment) => {
            if (!comment.id || previewStale) return;
            store.reanchorDiffComment(comment.id, {
              path: file.path,
              side: renderedSide,
              line: block.anchorLine,
              context: sourceLines[anchorIndex] || '',
              fileChangeSeq: file.snapshotSeq || file.sequence || 0,
              scope: store.diff.peek().scope,
            });
          }}
          onRemove={removeComment}
          onReveal={(comment) => void store.revealDiffLine(file, comment.side, comment.line)}
        />
      )}
    </div>
  );
});

export function MarkdownFilePreview({
  file,
  commenting,
  setCommenting,
  drafts,
  setDrafts,
  clearDraft,
  draftTargets,
  setDraftTargets,
}: MarkdownFilePreviewProps) {
  const store = useStore();
  const preview = file.markdownPreview;
  const previewStale = Boolean(preview?.loading || preview?.error);
  const renderedSide: DiffComment['side'] = preview?.side === 'before' ? 'old' : 'new';
  const sourceLines = useMemo(() => preview?.source?.split(/\r?\n/) || [], [preview?.source]);
  const renderedBlocks = useMemo(
    () => (preview?.source === undefined ? [] : markdownDocumentBlocks(preview.source)),
    [preview?.source],
  );
  const state = store.diff.value;
  const matchingComments = useMemo(() => {
    const cache = new Map<string, ReviewCommentEntry[]>();
    return (startLine: number, endLine = startLine): ReviewCommentEntry[] => {
      const key = `${startLine}:${endLine}`;
      const cached = cache.get(key);
      if (cached) return cached;
      const matches = (comment: DiffComment) =>
        (!comment.sessionId || comment.sessionId === state.sessionId) &&
        comment.path === file.path &&
        comment.side === renderedSide &&
        comment.line >= startLine &&
        comment.line <= endLine &&
        (!comment.scope || comment.scope === state.scope);
      const result = [
        ...state.historyComments.filter(matches),
        ...state.comments.filter(matches).map((comment) => ({ ...comment, queued: true })),
      ].sort((a, b) => (a.createdAt || 0) - (b.createdAt || 0));
      cache.set(key, result);
      return result;
    };
  }, [
    state.sessionId,
    state.scope,
    state.historyComments,
    state.comments,
    file.path,
    renderedSide,
  ]);
  const sourceOnlyComments = useMemo(
    () =>
      preview?.blocks
        ? matchingComments(1, Math.max(1, sourceLines.length)).filter(
            (comment) =>
              !preview.blocks?.some(
                (block) =>
                  block.commentable &&
                  comment.line >= block.startLine &&
                  comment.line <= block.endLine,
              ),
          )
        : [],
    [preview?.blocks, matchingComments, sourceLines.length],
  );
  const stableClearDraft = useEventCallback(clearDraft);
  const editComment = useEventCallback((comment: DiffComment) => {
    const body = window.prompt('Edit queued comment', comment.body);
    if (body !== null && comment.id) store.editDiffComment(comment.id, body);
  });
  const removeComment = useEventCallback((comment: DiffComment) => {
    if (comment.id) store.removeDiffComment(comment.id);
  });
  const submit = useEventCallback(
    (
      mode: 'send' | 'queue',
      line: number,
      key: string,
      contextBefore: string[],
      contextAfter: string[],
    ) => {
      const body = drafts[key] || '';
      if (!body.trim() || previewStale) return;
      const comment: DiffComment = {
        path: file.path,
        side: renderedSide,
        line,
        body: body.trim(),
        scope: store.diff.value.scope,
        context: sourceLines[line - 1] || '',
        contextBefore,
        contextAfter,
        fileChangeSeq: file.snapshotSeq || file.sequence || 0,
      };
      if (mode === 'queue') store.queueDiffComment(comment);
      else void store.sendDiffComment(comment);
      clearDraft(key);
    },
  );

  return (
    <div class="diff-markdown-preview" aria-busy={preview?.loading || undefined}>
      {preview?.side === 'before' && (
        <div class="diff-markdown-deleted-banner">Deleted — showing the last retained version</div>
      )}
      {preview?.loading && preview.source === undefined && (
        <div class="diff-loading" aria-live="polite">
          Loading rendered document…
        </div>
      )}
      {preview?.error && (
        <div class="diff-error">
          {preview.error}
          <button
            class="diff-retry"
            type="button"
            onClick={() => void store.loadMarkdownPreview(file, true)}
          >
            Retry
          </button>
        </div>
      )}
      {preview?.source !== undefined && preview.blocks && (
        <div class="diff-markdown-document">
          {renderedBlocks.map((block) => {
            const key = `${renderedSide}:${block.anchorLine}`;
            return (
              <PreviewBlock
                key={block.id}
                block={block}
                file={file}
                comments={
                  block.commentable
                    ? matchingComments(block.startLine, block.endLine)
                    : EMPTY_COMMENTS
                }
                body={drafts[key] || ''}
                draftTarget={draftTargets[key] || ''}
                commenting={commenting === key}
                sourceLines={sourceLines}
                readOnly={Boolean(state.readOnly)}
                setCommenting={setCommenting}
                setDrafts={setDrafts}
                clearDraft={stableClearDraft}
                setDraftTargets={setDraftTargets}
                editComment={editComment}
                removeComment={removeComment}
                submit={submit}
              />
            );
          })}
          {sourceOnlyComments.length > 0 && (
            <section class="diff-source-only-comments" aria-label="Source-only comments">
              <h4>Source-only comments</h4>
              {sourceOnlyComments.map((comment) => (
                <div class="diff-source-only-comment" key={comment.id}>
                  <div>
                    <strong>Line {comment.line}</strong> · {comment.body}
                  </div>
                  <div class="diff-source-only-actions">
                    <button
                      type="button"
                      onClick={() => void store.revealDiffLine(file, comment.side, comment.line)}
                    >
                      Reveal in Diff
                    </button>
                    {comment.queued && (
                      <>
                        <button type="button" onClick={() => editComment(comment)}>
                          Edit
                        </button>
                        <button type="button" onClick={() => removeComment(comment)}>
                          Remove
                        </button>
                      </>
                    )}
                  </div>
                </div>
              ))}
            </section>
          )}
        </div>
      )}
    </div>
  );
}
