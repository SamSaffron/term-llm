import type { ComponentChildren, ComponentType } from 'preact';
import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import type { DiffComment, DiffFile, DiffLine } from '../domain/types';
import {
  clampDiffWidth,
  fileKind,
  inlineEmphasis,
  isMarkdownPath,
  linesFromHunks,
  unifiedPatchForFile,
} from '../domain/diff';
import { copyText } from '../platform/browser';
import { rebaseHubAssetURL } from '../app/config';
import { planSummary } from '../domain/plan';
import { Icon } from './Icon';
import { ChipPicker } from './ChipPicker';
import { Drawer } from './Drawer';
import type { MarkdownFilePreviewProps } from './MarkdownFilePreview';
import { ReviewComment, type ReviewCommentEntry } from './ReviewComment';
import { useMediaQuery } from './useMediaQuery';

let loadedMarkdownPreview: ComponentType<MarkdownFilePreviewProps> | null = null;
let markdownPreviewImport: Promise<ComponentType<MarkdownFilePreviewProps>> | null = null;

function LazyMarkdownFilePreview(props: MarkdownFilePreviewProps) {
  const [Preview, setPreview] = useState<ComponentType<MarkdownFilePreviewProps> | null>(
    () => loadedMarkdownPreview,
  );
  useEffect(() => {
    if (Preview) return;
    markdownPreviewImport ||= import('./MarkdownFilePreview').then(
      ({ MarkdownFilePreview }) => MarkdownFilePreview,
    );
    let live = true;
    void markdownPreviewImport.then((component) => {
      loadedMarkdownPreview = component;
      if (live) setPreview(() => component);
    });
    return () => {
      live = false;
    };
  }, [Preview]);
  return Preview ? <Preview {...props} /> : <div class="diff-loading">Loading preview…</div>;
}

function DiffCode({
  line,
  emphasis,
  lang,
}: {
  line: DiffLine;
  emphasis?: [number, number];
  lang: string;
}) {
  const [html, setHTML] = useState('');
  useEffect(() => {
    let live = true;
    setHTML('');
    if (!lang || !line.content || emphasis)
      return () => {
        live = false;
      };
    void import('../domain/rich-highlight').then(({ highlightDiffLine }) => {
      if (live) setHTML(highlightDiffLine(line.content, lang));
    });
    return () => {
      live = false;
    };
  }, [line.content, lang, emphasis]);
  if (emphasis && emphasis[1] > emphasis[0])
    return (
      <span class="diff-code">
        {line.content.slice(0, emphasis[0])}
        <mark class="diff-word">{line.content.slice(emphasis[0], emphasis[1])}</mark>
        {line.content.slice(emphasis[1])}
      </span>
    );
  return html ? (
    <span class="diff-code" dangerouslySetInnerHTML={{ __html: html }} />
  ) : (
    <span class="diff-code">{line.content}</span>
  );
}

function Line({
  line,
  emphasis,
  lang,
  commentKey,
  commenting,
  comments,
  body,
  onComment,
  onBody,
  onCancel,
  onSubmit,
  onEdit,
  onReanchor,
  onRemove,
  commentable = true,
}: {
  line: DiffLine;
  emphasis?: [number, number];
  lang: string;
  commentKey: string;
  commenting: boolean;
  comments: ReviewCommentEntry[];
  body: string;
  onComment: (key: string) => void;
  onBody: (value: string) => void;
  onCancel: () => void;
  onSubmit: (mode: 'send' | 'queue') => void;
  onEdit: (comment: DiffComment) => void;
  onReanchor: (comment: DiffComment) => void;
  onRemove: (comment: DiffComment) => void;
  commentable?: boolean;
}) {
  const number = line.kind === 'delete' ? line.oldLine : line.newLine;
  const side = line.kind === 'delete' ? 'old' : 'new';
  const kind =
    line.kind === 'add'
      ? 'add'
      : line.kind === 'delete'
        ? 'del'
        : line.kind === 'hunk'
          ? 'hunk'
          : 'ctx';
  const enabled = Boolean(commentable && number && line.kind !== 'hunk');
  return (
    <div
      class={`diff-row ${kind}${commenting ? ' commenting' : ''}`}
      data-commentable={enabled}
      data-diff-anchor={number ? `${side}:${number}` : undefined}
      tabIndex={number ? -1 : undefined}
    >
      <span class="diff-ln">{line.oldLine || ''}</span>
      <span class="diff-ln">{line.newLine || ''}</span>
      <DiffCode line={line} emphasis={emphasis} lang={lang} />
      {enabled && number && (
        <ReviewComment
          controlId={`diff-comment-${commentKey.replace(/[^a-z0-9_-]/gi, '-')}`}
          commenting={commenting}
          comments={comments}
          body={body}
          affordanceLabel={
            comments.length
              ? `Show ${comments.length} inline comment${comments.length === 1 ? '' : 's'} for line ${number}`
              : `Comment on line ${number}`
          }
          regionLabel={`Inline comments for line ${number}`}
          heading={
            <>
              <span class="diff-comment-line-chip">Line {number}</span>
              <span>{line.kind === 'delete' ? 'Original' : 'Current'} version</span>
            </>
          }
          onToggle={() => onComment(commentKey)}
          onBody={onBody}
          onCancel={onCancel}
          onSubmit={onSubmit}
          onEdit={onEdit}
          onReanchor={onReanchor}
          onRemove={onRemove}
        />
      )}
    </div>
  );
}

function DiffAction({
  label,
  glyph,
  value,
}: {
  label: string;
  glyph: ComponentChildren;
  value: () => string | Promise<string>;
}) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<number | undefined>(undefined);
  useEffect(
    () => () => {
      if (timer.current !== undefined) clearTimeout(timer.current);
    },
    [],
  );
  const copy = async (event: MouseEvent) => {
    event.preventDefault();
    event.stopPropagation();
    try {
      const text = await value();
      if (!text) return;
      await copyText(text);
      setCopied(true);
      if (timer.current !== undefined) clearTimeout(timer.current);
      timer.current = window.setTimeout(() => {
        setCopied(false);
        timer.current = undefined;
      }, 700);
    } catch {
      /* Copy actions fail silently like the legacy control. */
    }
  };
  return (
    <button
      class={`diff-action-btn ${copied ? 'copied' : ''}`}
      type="button"
      title={label}
      aria-label={label}
      onClick={(event) => void copy(event)}
    >
      {glyph}
    </button>
  );
}

function splitDiffPath(path: string, workingDir = ''): { base: string; dir: string } {
  const normalizedPath = String(path || '').replaceAll('\\', '/');
  const normalizedWorkingDir = String(workingDir || '')
    .replaceAll('\\', '/')
    .replace(/\/+$/, '');
  const rootName = normalizedWorkingDir.split('/').filter(Boolean).at(-1) || '';
  const display = (
    rootName && normalizedPath.startsWith(`${normalizedWorkingDir}/`)
      ? `${rootName}/${normalizedPath.slice(normalizedWorkingDir.length + 1)}`
      : normalizedPath
  ).replace(/^\/+/, '');
  const index = display.lastIndexOf('/');
  return index < 0
    ? { base: display, dir: '' }
    : { base: display.slice(index + 1), dir: display.slice(0, index) };
}

function File({
  file,
  fullscreen,
  onFullscreenToggle,
}: {
  file: DiffFile;
  fullscreen: boolean;
  onFullscreenToggle: () => void;
}) {
  const store = useStore();
  const [limit, setLimit] = useState(500);
  const [commenting, setCommenting] = useState('');
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  const [draftTargets, setDraftTargets] = useState<Record<string, string>>({});
  const lines = file.lines || [];
  const kind = fileKind(file);
  const legacyKind = kind === 'add' ? 'create' : kind === 'delete' ? 'delete' : 'modify';
  const markdownPath = isMarkdownPath(file.path);
  const markdownUnavailableReason = store.diff.value.worktreeDir
    ? 'Rendered preview is unavailable for worktree patches'
    : file.binary || file.image
      ? 'Rendered preview is unavailable for binary content'
      : file.truncated
        ? 'Rendered preview is unavailable for truncated content'
        : file.contentAvailable === false
          ? 'Rendered Markdown source is unavailable'
          : '';
  const markdownAvailable = markdownPath && !markdownUnavailableReason;
  const session = store.activeSession.value;
  const project = store.projects.value.find(
    (entry) => entry.id === (session?.projectId || store.activeProjectId.value),
  );
  const name = splitDiffPath(
    file.path,
    session?.worktreeDir || session?.workingDir || project?.path || '',
  );
  const selectedView =
    markdownAvailable && file.markdownPreview?.view === 'rendered' ? 'rendered' : 'diff';
  const viewIdentity = `${file.sequence || 0}-${Math.abs(
    [...file.path].reduce((hash, character) => (hash * 31 + character.codePointAt(0)!) | 0, 0),
  ).toString(36)}`;
  const previewPanelID = `diff-panel-${viewIdentity}`;
  const hasGapRows = lines.some((line) => line.kind === 'gap');
  const canExpandFallback =
    !hasGapRows &&
    !file.truncated &&
    lines.length > 0 &&
    (file.context || 3) < Math.max(file.oldLineCount || 0, file.newLineCount || 0);
  const expandFallback = () =>
    void store.expandDiff(file, Math.min(100_000, Math.max(12, (file.context || 3) * 4)));
  const expandGap = async (key: string, target: HTMLElement) => {
    const top = target.getBoundingClientRect().top;
    await store.expandDiff(file, Math.min(100_000, Math.max(12, (file.context || 3) * 4)));
    requestAnimationFrame(() => {
      const replacement = document.querySelector<HTMLElement>(
        `[data-diff-gap="${CSS.escape(key)}"]`,
      );
      if (replacement) window.scrollBy(0, replacement.getBoundingClientRect().top - top);
    });
  };
  const emphasis = new Map<number, [number, number]>();
  for (let index = 0; index + 1 < lines.length; index += 1)
    if (lines[index].kind === 'delete' && lines[index + 1].kind === 'add') {
      const ranges = inlineEmphasis(lines[index].content, lines[index + 1].content);
      emphasis.set(index, ranges.old);
      emphasis.set(index + 1, ranges.new);
    }
  const clearDraft = (key: string) => {
    setDrafts((current) => {
      if (!(key in current)) return current;
      const next = { ...current };
      delete next[key];
      return next;
    });
    setDraftTargets((current) => {
      if (!(key in current)) return current;
      const next = { ...current };
      delete next[key];
      return next;
    });
  };
  const matchingComments = (
    side: DiffComment['side'],
    startLine: number,
    endLine = startLine,
  ): Array<DiffComment & { queued?: boolean }> => {
    const matches = (comment: DiffComment) =>
      (!comment.sessionId || comment.sessionId === store.diff.value.sessionId) &&
      comment.path === file.path &&
      comment.side === side &&
      comment.line >= startLine &&
      comment.line <= endLine &&
      (!comment.scope || comment.scope === store.diff.value.scope);
    return [
      ...store.diff.value.historyComments.filter(matches),
      ...store.diff.value.comments.filter(matches).map((comment) => ({
        ...comment,
        queued: true,
      })),
    ].sort((left, right) => (left.createdAt || 0) - (right.createdAt || 0));
  };
  const submitAnchor = (
    mode: 'send' | 'queue',
    side: DiffComment['side'],
    number: number,
    context: string,
    key: string,
    contextBefore?: string[],
    contextAfter?: string[],
  ) => {
    const body = drafts[key] || '';
    if (!body.trim() || !number) return;
    const comment: DiffComment = {
      path: file.path,
      side,
      line: number,
      body: body.trim(),
      scope: store.diff.value.scope,
      context,
      contextBefore,
      contextAfter,
      fileChangeSeq: file.snapshotSeq || file.sequence || 0,
    };
    if (mode === 'queue') store.queueDiffComment(comment);
    else void store.sendDiffComment(comment);
    clearDraft(key);
  };
  const patch = async () => {
    if (file.lines || file.patch) return unifiedPatchForFile(file);
    const data = await store.endpoints.fileDiff(
      store.activeSessionId.peek(),
      file.path,
      store.diff.peek().scope,
      0,
      file.snapshotSeq || 0,
    );
    if (data.image || data.truncated) return '';
    return unifiedPatchForFile({
      ...file,
      lines: linesFromHunks(data.hunks),
      patch: String(data.diff || data.patch || ''),
    });
  };
  const toggle = () => {
    store.diff.value = {
      ...store.diff.peek(),
      selectedPath: file.path,
      followCurrentFile: false,
    };
    void store.expandDiff(file);
  };
  const editComment = (comment: DiffComment) => {
    const body = window.prompt('Edit queued comment', comment.body);
    if (body !== null && comment.id) store.editDiffComment(comment.id, body);
  };
  const removeComment = (comment: DiffComment) => {
    if (comment.id) store.removeDiffComment(comment.id);
  };
  const diffContent = (
    <>
      {file.loading && <div class="diff-loading">Loading…</div>}
      {file.error && (
        <div class="diff-error">
          {file.error}
          <button
            class="diff-retry"
            onClick={() => void store.expandDiff({ ...file, lines: undefined })}
          >
            Retry
          </button>
        </div>
      )}
      {file.truncated && <div class="diff-error">Diff unavailable.</div>}
      {file.binary ? (
        <div class="diff-loading">Binary file changed.</div>
      ) : file.image ? (
        <div class={`diff-image-comparison diff-image-${legacyKind}`}>
          {kind !== 'add' && file.beforeURL && (
            <figure class="diff-image-side">
              <figcaption class="diff-image-label">Before</figcaption>
              <img
                class="diff-image-preview"
                src={rebaseHubAssetURL(store.config, file.beforeURL)}
                alt={`Before ${file.path}`}
              />
            </figure>
          )}
          {kind !== 'delete' && file.afterURL && (
            <figure class="diff-image-side">
              <figcaption class="diff-image-label">After</figcaption>
              <img
                class="diff-image-preview"
                src={rebaseHubAssetURL(store.config, file.afterURL)}
                alt={`After ${file.path}`}
              />
            </figure>
          )}
        </div>
      ) : (
        <>
          {canExpandFallback && (
            <button
              class="diff-hunk-expand diff-hunk-expand-above"
              type="button"
              aria-label="Show more context above"
              disabled={file.loading}
              onClick={expandFallback}
            >
              <Icon name="chevron-up" />
              <span>Show more above</span>
            </button>
          )}
          <div class={`diff-rows diff-rows-kind-${legacyKind}`}>
            {lines.slice(0, limit).map((line, index) => {
              const rowKey = `${line.kind}-${line.oldLine || 0}-${line.newLine || 0}-${index}`;
              if (line.kind === 'gap') {
                const hidden = Math.max(line.hiddenOld || 0, line.hiddenNew || 0);
                const direction =
                  line.gapDirection === 'above'
                    ? 'above'
                    : line.gapDirection === 'below'
                      ? 'below'
                      : 'between hunks';
                return (
                  <button
                    key={rowKey}
                    data-diff-gap={rowKey}
                    class="diff-hunk-expand"
                    type="button"
                    disabled={file.loading}
                    onClick={(event) => void expandGap(rowKey, event.currentTarget)}
                  >
                    Show {hidden} hidden {hidden === 1 ? 'line' : 'lines'} {direction}
                  </button>
                );
              }
              const number = line.kind === 'delete' ? line.oldLine : line.newLine;
              const side: DiffComment['side'] = line.kind === 'delete' ? 'old' : 'new';
              const anchorKey = number ? `${side}:${number}` : rowKey;
              const comments = number ? matchingComments(side, number) : [];
              return (
                <Line
                  key={rowKey}
                  line={line}
                  emphasis={emphasis.get(index)}
                  lang={lines.length <= 1500 ? file.lang || '' : ''}
                  commentKey={anchorKey}
                  commenting={commenting === anchorKey}
                  comments={comments}
                  body={drafts[anchorKey] || ''}
                  commentable={!store.diff.value.readOnly}
                  onComment={(next) => setCommenting((current) => (current === next ? '' : next))}
                  onBody={(value) => setDrafts((current) => ({ ...current, [anchorKey]: value }))}
                  onCancel={() => {
                    clearDraft(anchorKey);
                    setCommenting('');
                  }}
                  onSubmit={(mode) =>
                    number && submitAnchor(mode, side, number, line.content, anchorKey)
                  }
                  onEdit={editComment}
                  onReanchor={(comment) => {
                    if (!comment.id || !number) return;
                    store.reanchorDiffComment(comment.id, {
                      path: file.path,
                      side,
                      line: number,
                      context: line.content,
                      fileChangeSeq: file.snapshotSeq || file.sequence || 0,
                      scope: store.diff.value.scope,
                    });
                  }}
                  onRemove={removeComment}
                />
              );
            })}
            {lines.length > limit && (
              <button
                class="diff-show-more"
                onClick={() => setLimit((value) => Math.min(lines.length, value + 500))}
              >
                Show {Math.min(500, lines.length - limit)} more lines
              </button>
            )}
          </div>
          {canExpandFallback && (
            <button
              class="diff-hunk-expand diff-hunk-expand-below"
              type="button"
              aria-label="Show more context below"
              disabled={file.loading}
              onClick={expandFallback}
            >
              <span>Show more below</span>
              <Icon name="chevron-down" />
            </button>
          )}
        </>
      )}
    </>
  );
  return (
    <section
      class={`diff-file diff-file-${legacyKind}${fullscreen ? ' diff-file-fullscreen' : ''}`}
      data-diff-file-path={file.path}
    >
      <div
        class={`diff-file-row ${file.expanded ? 'expanded' : ''} ${store.diff.value.selectedPath === file.path ? 'selected' : ''}`}
        role="button"
        tabIndex={0}
        title={file.path}
        data-path={file.path}
        aria-expanded={file.expanded}
        onClick={(event) => {
          if ((event.target as Element).closest('.diff-file-actions')) return;
          toggle();
        }}
        onKeyDown={(event) => {
          if (
            event.target === event.currentTarget &&
            (event.key === 'Enter' || event.key === ' ')
          ) {
            event.preventDefault();
            toggle();
          }
        }}
      >
        <span class="diff-chevron">▸</span>
        <span class={`diff-kind-badge diff-kind-${legacyKind}`}>
          {legacyKind === 'create' ? 'A' : legacyKind === 'delete' ? 'D' : 'M'}
        </span>
        <span class="diff-file-name">
          <span class="diff-file-base">{name.base}</span>
          {name.dir && <span class="diff-file-dir">{name.dir}</span>}
        </span>
        <span class="diff-file-counts">
          {file.truncated ? (
            <span class="diff-count-muted">–</span>
          ) : (
            <>
              {Boolean(file.additions) && <span class="diff-count-add">+{file.additions}</span>}
              {Boolean(file.deletions) && <span class="diff-count-del">−{file.deletions}</span>}
            </>
          )}
        </span>
        <span class="diff-file-actions" onClick={(event) => event.stopPropagation()}>
          {markdownPath && (
            <button
              class={`diff-action-btn diff-markdown-toggle${selectedView === 'rendered' ? ' active' : ''}`}
              type="button"
              title={markdownUnavailableReason || 'Toggle rendered Markdown'}
              aria-label={
                selectedView === 'rendered' ? 'Show Markdown diff' : 'Show rendered Markdown'
              }
              aria-pressed={selectedView === 'rendered'}
              aria-controls={previewPanelID}
              disabled={!markdownAvailable}
              onMouseDown={(event) => {
                event.preventDefault();
                event.stopPropagation();
              }}
              onClick={(event) => {
                event.preventDefault();
                event.stopPropagation();
                const nextView = selectedView === 'rendered' ? 'diff' : 'rendered';
                void store.setMarkdownView(file, nextView);
              }}
            >
              <Icon name="markdown" />
            </button>
          )}
          <button
            class={`diff-action-btn diff-fullscreen-toggle${fullscreen ? ' active' : ''}`}
            type="button"
            title={fullscreen ? 'Exit fullscreen' : 'View file fullscreen'}
            aria-label={`${fullscreen ? 'Exit fullscreen for' : 'View fullscreen'} ${file.path}`}
            aria-pressed={fullscreen}
            onMouseDown={(event) => {
              event.preventDefault();
              event.stopPropagation();
            }}
            onClick={(event) => {
              event.preventDefault();
              event.stopPropagation();
              onFullscreenToggle();
            }}
          >
            <Icon name={fullscreen ? 'restore' : 'expand'} />
          </button>
          <DiffAction
            label={`Copy path ${file.path}`}
            glyph={<Icon name="copy" />}
            value={() => file.path}
          />
          {!file.image && (
            <DiffAction
              label={`Copy diff for ${file.path}`}
              glyph={<Icon name="diff" />}
              value={patch}
            />
          )}
        </span>
      </div>
      {file.expanded && (
        <div class="diff-file-body">
          <div id={previewPanelID}>
            {selectedView === 'rendered' ? (
              <LazyMarkdownFilePreview
                file={file}
                commenting={commenting}
                setCommenting={setCommenting}
                drafts={drafts}
                setDrafts={setDrafts}
                clearDraft={clearDraft}
                draftTargets={draftTargets}
                setDraftTargets={setDraftTargets}
              />
            ) : (
              diffContent
            )}
          </div>
        </div>
      )}
    </section>
  );
}

const DIFF_SCOPE_OPTIONS = [
  ['last_turn', 'Last turn'],
  ['last_3_turns', 'Last 3 turns'],
  ['uncommitted', 'Uncommitted'],
  ['unstaged', 'Unstaged'],
  ['staged', 'Staged'],
] as const;

function DiffScopePicker() {
  const store = useStore();
  const state = store.diff.value;
  const gitScopesAvailable =
    state.git || store.worktreesAvailable() || store.worktreesEnabled.value;
  const options = (gitScopesAvailable ? DIFF_SCOPE_OPTIONS : DIFF_SCOPE_OPTIONS.slice(0, 2)).map(
    ([value, label]) => ({ value, label }),
  );
  return (
    <ChipPicker
      ariaLabel="Change scope"
      value={state.scope}
      options={options}
      triggerClass="chip-trigger diff-scope-trigger"
      popoverClass="diff-scope-popover"
      onChange={(scope) => {
        store.diff.value = { ...store.diff.peek(), scope, files: [], error: '' };
        void store.loadDiff();
      }}
      renderTrigger={(selected) => (
        <>
          <span class="chip-label">{selected.label}</span>
          <svg
            class="diff-scope-chevron"
            viewBox="0 0 12 8"
            fill="none"
            stroke="currentColor"
            stroke-width="1.6"
            stroke-linecap="round"
            stroke-linejoin="round"
            aria-hidden="true"
          >
            <path d="m2 2 4 4 4-4" />
          </svg>
        </>
      )}
    />
  );
}

export function DiffSidebar() {
  const store = useStore();
  const state = store.diff.value;
  const aside = useRef<HTMLElement>(null);
  const fullscreenFollow = useRef<boolean | null>(null);
  const [fullscreenPath, setFullscreenPath] = useState('');
  const mobile = useMediaQuery('(max-width: 767px)');
  const compact = useMediaQuery('(max-width: 1099px)');
  const files = state.files.filter((file) =>
    file.path.toLowerCase().includes(state.filter.toLowerCase()),
  );
  const displayedFiles = fullscreenPath
    ? state.files.filter((file) => file.path === fullscreenPath)
    : files;
  const exitFullscreen = useCallback(() => {
    setFullscreenPath('');
    if (fullscreenFollow.current === null) return;
    store.diff.value = {
      ...store.diff.peek(),
      followCurrentFile: fullscreenFollow.current,
    };
    fullscreenFollow.current = null;
  }, [store]);
  const selectFile = (delta: number) => {
    if (!files.length) return;
    const current = Math.max(
      0,
      files.findIndex((file) => file.path === (fullscreenPath || state.selectedPath)),
    );
    const next = Math.max(0, Math.min(files.length - 1, current + delta));
    const file = files[next];
    if (fullscreenPath) setFullscreenPath(file.path);
    store.diff.value = {
      ...store.diff.peek(),
      selectedPath: file.path,
      followCurrentFile: false,
    };
    if (!file.expanded) void store.expandDiff(file);
    requestAnimationFrame(() =>
      document
        .querySelector<HTMLElement>(`.diff-file-row[data-path="${CSS.escape(file.path)}"]`)
        ?.scrollIntoView({ block: 'nearest' }),
    );
  };

  useEffect(() => {
    if (!state.open || mobile || !compact) return;
    const outside = (event: PointerEvent) => {
      const target = event.target as Node | null;
      if (
        aside.current?.contains(target) ||
        (target instanceof Element && target.closest('[aria-controls="diffSidebar"]'))
      )
        return;
      store.diff.value = { ...store.diff.peek(), open: false, maximized: false };
    };
    document.addEventListener('pointerdown', outside);
    return () => document.removeEventListener('pointerdown', outside);
  }, [compact, mobile, state.open, store]);
  useEffect(() => {
    if (!state.open) return;
    const escape = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return;
      if (fullscreenPath) {
        event.preventDefault();
        exitFullscreen();
        return;
      }
      const target = event.target as HTMLInputElement;
      if (target?.matches('input[type="search"]') && target.value) {
        store.diff.value = { ...store.diff.peek(), filter: '' };
        target.value = '';
      } else
        store.diff.value = {
          ...store.diff.peek(),
          maximized: false,
          open: state.maximized ? true : false,
        };
    };
    addEventListener('keydown', escape);
    return () => removeEventListener('keydown', escape);
  }, [exitFullscreen, fullscreenPath, state.open, state.maximized, store]);
  useEffect(() => {
    if (fullscreenPath && !state.files.some((file) => file.path === fullscreenPath))
      exitFullscreen();
  }, [exitFullscreen, fullscreenPath, state.files]);
  useEffect(() => {
    if (!state.open) return;
    const navigate = (event: KeyboardEvent) => {
      if (
        event.target instanceof HTMLElement &&
        event.target.matches('input,textarea,select,[contenteditable="true"]')
      )
        return;
      if (event.key !== '[' && event.key !== ']') return;
      event.preventDefault();
      selectFile(event.key === ']' ? 1 : -1);
    };
    addEventListener('keydown', navigate);
    return () => removeEventListener('keydown', navigate);
  });
  useEffect(() => {
    const path = store.currentActivityFile.value;
    if (!state.open || !state.followCurrentFile || !path) return;
    const file = state.files.find((entry) => entry.path === path);
    if (!file) return;
    store.diff.value = { ...store.diff.peek(), selectedPath: path };
    if (!file.expanded) void store.expandDiff(file);
  }, [state.open, state.followCurrentFile, state.files, store, store.currentActivityFile.value]);
  if (!state.open) return null;

  const comments = state.comments.filter(
    (comment) => !comment.sessionId || comment.sessionId === state.sessionId,
  );
  const allExpanded = state.files.length > 0 && state.files.every((file) => file.expanded);
  const adds = state.files.reduce((sum, file) => sum + (file.additions || 0), 0);
  const dels = state.files.reduce((sum, file) => sum + (file.deletions || 0), 0);
  const startResize = (event: PointerEvent) => {
    event.preventDefault();
    const handle = event.currentTarget as HTMLElement;
    const shell = handle.closest('.app');
    const startX = event.clientX;
    const startWidth = state.width;
    const move = (next: PointerEvent) =>
      store.resizeDiff(clampDiffWidth(startWidth + startX - next.clientX, innerWidth));
    const finish = (next: PointerEvent) => {
      removeEventListener('pointermove', move);
      removeEventListener('pointerup', finish);
      removeEventListener('pointercancel', finish);
      shell?.classList.remove('diff-resizing');
      handle.releasePointerCapture?.(next.pointerId);
    };
    handle.setPointerCapture?.(event.pointerId);
    shell?.classList.add('diff-resizing');
    addEventListener('pointermove', move);
    addEventListener('pointerup', finish, { once: true });
    addEventListener('pointercancel', finish, { once: true });
  };
  const content = (
    <>
      <div
        class="diff-resize-handle"
        role="separator"
        aria-label="Resize changes panel"
        aria-orientation="vertical"
        tabIndex={0}
        onPointerDown={startResize}
        onDblClick={() => store.resizeDiff(420)}
      />
      <div class="diff-sidebar-header">
        {state.worktreeDir ? (
          <strong class="diff-source-title">{state.worktreeTitle || 'Worktree'} changes</strong>
        ) : (
          <DiffScopePicker />
        )}
        <span class="diff-sidebar-totals">
          {adds > 0 && <span class="diff-sidebar-totals-add">+{adds}</span>}
          {dels > 0 && <span class="diff-sidebar-totals-del">−{dels}</span>}
        </span>
        {!mobile && (
          <button
            class="icon-btn diff-bulk-toggle"
            type="button"
            aria-label={`${allExpanded ? 'Collapse' : 'Expand'} all files`}
            title={`${allExpanded ? 'Collapse' : 'Expand'} all`}
            data-action={allExpanded ? 'collapse' : 'expand'}
            onClick={() => {
              if (allExpanded)
                store.diff.value = {
                  ...store.diff.peek(),
                  files: store.diff.peek().files.map((file) => ({ ...file, expanded: false })),
                };
              else {
                store.diff.value = {
                  ...store.diff.peek(),
                  files: store.diff
                    .peek()
                    .files.map((file) => (file.lines ? { ...file, expanded: true } : file)),
                };
                state.files
                  .filter((file) => !file.lines)
                  .forEach((file) => void store.expandDiff(file));
              }
            }}
          >
            <svg
              width="18"
              height="18"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="2"
              stroke-linecap="round"
              stroke-linejoin="round"
              aria-hidden="true"
            >
              <g class="diff-bulk-expand-icon">
                <path d="m7 9 5-5 5 5" />
                <path d="m7 15 5 5 5-5" />
              </g>
              <g class="diff-bulk-collapse-icon">
                <path d="m7 4 5 5 5-5" />
                <path d="m7 20 5-5 5 5" />
              </g>
            </svg>
          </button>
        )}
        <button
          class="icon-btn diff-sidebar-maximize"
          type="button"
          aria-label={state.maximized ? 'Restore changes' : 'Maximize changes'}
          data-action={state.maximized ? 'restore' : 'maximize'}
          onClick={() => {
            store.diff.value = { ...state, maximized: !state.maximized };
          }}
        >
          <Icon name={state.maximized ? 'restore' : 'expand'} />
        </button>
        <button
          class="icon-btn close-button diff-sidebar-close"
          type="button"
          aria-label="Hide changes"
          onClick={() => {
            store.diff.value = { ...state, open: false };
          }}
        >
          <Icon name="close" />
        </button>
      </div>
      {(state.files.length > 8 || state.filter) && (
        <div class="diff-filter-row">
          <input
            class="diff-filter-input"
            type="search"
            placeholder="Filter files…"
            aria-label="Filter changed files"
            value={state.filter}
            onInput={(event) => {
              store.diff.value = { ...state, filter: event.currentTarget.value };
            }}
          />
        </div>
      )}
      <div class="diff-file-list" aria-label="Changed files">
        {state.loading && <div class="diff-loading">Loading changes…</div>}
        {state.error && (
          <div class="diff-error">
            {state.error}
            <button
              class="diff-retry"
              onClick={() =>
                void (state.worktreeDir
                  ? store.openWorktreeDiff(state.worktreeDir, state.worktreeTitle || 'Worktree')
                  : store.loadDiff())
              }
            >
              Retry
            </button>
          </div>
        )}
        {displayedFiles.map((file) => (
          <File
            key={file.path}
            file={file}
            fullscreen={fullscreenPath === file.path}
            onFullscreenToggle={() => {
              if (fullscreenPath === file.path) {
                exitFullscreen();
                return;
              }
              fullscreenFollow.current = state.followCurrentFile;
              setFullscreenPath(file.path);
              store.diff.value = {
                ...store.diff.peek(),
                selectedPath: file.path,
                followCurrentFile: false,
              };
              if (!file.expanded) void store.expandDiff(file);
            }}
          />
        ))}
        {!state.loading && !state.error && !displayedFiles.length && (
          <div class="diff-empty">
            {state.worktreeDir ? 'This worktree is clean.' : 'No file changes in this scope.'}
          </div>
        )}
      </div>
      {!state.readOnly && comments.length > 0 && (
        <div class="diff-queue-bar">
          <span class="diff-queue-count" role="status" aria-live="polite">
            {comments.length} queued
            {comments.some((comment) => comment.state === 'stale')
              ? `, ${comments.filter((comment) => comment.state === 'stale').length} stale`
              : ''}
          </span>
          <button
            class="diff-queue-discard"
            type="button"
            onClick={() => store.discardDiffComments(state.sessionId)}
          >
            Discard
          </button>
          <button
            class="diff-queue-send"
            type="button"
            onClick={() => void store.sendDiffComments()}
          >
            Send comments
          </button>
        </div>
      )}
    </>
  );
  if (!mobile)
    return (
      <aside
        ref={aside}
        class={`diff-sidebar open ${state.maximized ? 'maximized' : ''} ${fullscreenPath ? 'file-fullscreen' : ''}`.trim()}
        id="diffSidebar"
        aria-label={
          state.worktreeDir
            ? `${state.worktreeTitle || 'Worktree'} changes`
            : 'Session file changes'
        }
      >
        {content}
      </aside>
    );
  return (
    <Drawer
      open
      id="diffSidebar"
      className={`diff-sidebar open ${state.maximized ? 'maximized' : ''} ${fullscreenPath ? 'file-fullscreen' : ''}`.trim()}
      title={
        state.worktreeDir ? `${state.worktreeTitle || 'Worktree'} changes` : 'Session file changes'
      }
      side="right"
      onClose={() => {
        if (fullscreenPath) {
          exitFullscreen();
          return;
        }
        store.diff.value = { ...store.diff.peek(), open: false, maximized: false };
      }}
    >
      {content}
    </Drawer>
  );
}

export function PlanSurface() {
  const store = useStore();
  const plan = store.currentPlan.value;
  const open = store.planVisible.value;
  const mobile = useMediaQuery('(max-width: 767px)');
  const compact = useMediaQuery('(max-width: 1099px)');
  const surface = useRef<HTMLElement>(null);
  const summary = planSummary(plan);

  useEffect(() => {
    if (open) store.planSeen.value = summary.signature;
  }, [open, store, summary.signature]);

  useEffect(() => {
    if (!open || mobile || !compact) return;
    const outside = (event: PointerEvent) => {
      const target = event.target as Node | null;
      if (
        surface.current?.contains(target) ||
        (target instanceof Element && target.closest('[aria-controls="planSurface"]'))
      )
        return;
      store.closePlan();
    };
    document.addEventListener('pointerdown', outside);
    return () => document.removeEventListener('pointerdown', outside);
  }, [compact, mobile, open, store]);

  useEffect(() => {
    if (!open || mobile) return;
    const returnFocus =
      document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const escape = (event: KeyboardEvent) => {
      if (
        event.key !== 'Escape' ||
        store.modal.peek() ||
        store.askUser.peek() ||
        store.approval.peek()
      )
        return;
      event.preventDefault();
      store.closePlan();
    };
    addEventListener('keydown', escape);
    return () => {
      removeEventListener('keydown', escape);
      if (document.contains(returnFocus)) returnFocus?.focus({ preventScroll: true });
    };
  }, [mobile, open, store]);

  if (!plan) return null;

  const progress = summary.completed / Math.max(1, summary.total);
  const announcement = summary.complete
    ? `Plan complete. All ${summary.total} steps finished.`
    : summary.activeStep
      ? `Step ${summary.position} of ${summary.total}: ${summary.activeStep}`
      : `Plan has ${summary.total} steps. ${summary.completed} complete.`;

  const panel = (
    <section
      ref={surface}
      class={`plan-surface ${mobile ? 'plan-sheet-content' : 'plan-panel'} ${open ? 'open' : ''}`}
      id="planSurface"
      role={mobile ? undefined : 'complementary'}
      aria-hidden={!open}
      aria-labelledby="planSurfaceTitle"
      tabIndex={-1}
    >
      {mobile && (
        <div class="plan-sheet-handle" data-drawer-handle aria-hidden="true">
          <span />
        </div>
      )}
      <div class="plan-surface-header">
        <h2 id="planSurfaceTitle">Current plan</h2>
        <span class={`plan-surface-progress ${summary.complete ? 'complete' : ''}`}>
          {summary.complete ? 'Complete' : `Step ${summary.position} of ${summary.total}`}
        </span>
        <button
          class="icon-btn close-button"
          type="button"
          aria-label="Close current plan"
          onClick={() => store.closePlan()}
        >
          <Icon name="close" />
        </button>
      </div>
      <div class="plan-progress-track" aria-hidden="true" style={{ '--plan-progress': progress }}>
        <span />
      </div>
      <div class="plan-surface-body">
        {plan.explanation && <p class="current-plan-explanation">{plan.explanation}</p>}
        <ol class="current-plan-checklist" role="list">
          {plan.plan.map((step, index) => (
            <li
              class={`current-plan-step current-plan-step-${step.status}`}
              key={`${index}-${step.step}`}
              aria-current={step.status === 'in_progress' ? 'step' : undefined}
            >
              <span class="current-plan-step-marker" aria-hidden="true">
                {step.status === 'completed' ? (
                  <Icon name="check" />
                ) : step.status === 'in_progress' ? (
                  <span class="current-plan-step-pulse" />
                ) : (
                  <span class="current-plan-step-ring" />
                )}
              </span>
              <div class="current-plan-step-content">
                <div class="current-plan-step-text">{step.step}</div>
                <div class="current-plan-step-state">
                  {step.status === 'in_progress'
                    ? 'In progress'
                    : step.status === 'completed'
                      ? 'Completed'
                      : 'Pending'}
                </div>
              </div>
            </li>
          ))}
        </ol>
        <div class="visually-hidden" role="status" aria-live="polite" aria-atomic="true">
          {announcement}
        </div>
      </div>
    </section>
  );
  if (!mobile) return panel;
  return (
    <Drawer
      open={open}
      id="planSurfaceDrawer"
      className="plan-sheet open"
      title="Current plan"
      side="bottom"
      onClose={() => store.closePlan()}
    >
      {panel}
    </Drawer>
  );
}
