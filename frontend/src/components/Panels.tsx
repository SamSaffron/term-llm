import { useEffect, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import type { DiffComment, DiffFile, DiffLine } from '../domain/types';
import {
  clampDiffWidth,
  fileKind,
  inlineEmphasis,
  linesFromHunks,
  unifiedPatchForFile,
} from '../domain/diff';
import { copyText } from '../platform/browser';
import { rebaseHubAssetURL } from '../app/config';
import { planSummary } from '../domain/plan';
import { Icon } from './Icon';
import { ChipPicker } from './ChipPicker';

function commentTimestamp(
  createdAt: number | undefined,
  now: number,
): { dateTime: string; label: string; title: string } | null {
  if (!createdAt || !Number.isFinite(createdAt)) return null;
  const timestamp = createdAt < 10_000_000_000 ? createdAt * 1000 : createdAt;
  const date = new Date(timestamp);
  if (Number.isNaN(date.getTime())) return null;
  const elapsed = Math.max(0, now - timestamp);
  let label: string;
  if (elapsed < 60_000) label = 'just now';
  else {
    const minutes = Math.floor(elapsed / 60_000);
    if (minutes < 60) label = `${minutes}m ago`;
    else {
      const hours = Math.floor(minutes / 60);
      if (hours < 24) label = `${hours}h ago`;
      else {
        const days = Math.floor(hours / 24);
        label = days < 7 ? `${days}d ago` : date.toLocaleDateString();
      }
    }
  }
  return { dateTime: date.toISOString(), label, title: date.toLocaleString() };
}

function resizeCommentEditor(editor: HTMLTextAreaElement): void {
  editor.style.height = 'auto';
  const computedMaxHeight = getComputedStyle(editor).maxHeight;
  const parsedMaxHeight = Number.parseFloat(computedMaxHeight);
  const maxHeight = computedMaxHeight.endsWith('rem')
    ? parsedMaxHeight * Number.parseFloat(getComputedStyle(document.documentElement).fontSize)
    : parsedMaxHeight;
  editor.style.height = `${Math.min(editor.scrollHeight, Number.isFinite(maxHeight) ? maxHeight : 160)}px`;
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
}: {
  line: DiffLine;
  emphasis?: [number, number];
  lang: string;
  commentKey: string;
  commenting: boolean;
  comments: Array<DiffComment & { queued?: boolean }>;
  body: string;
  onComment: (key: string) => void;
  onBody: (value: string) => void;
  onCancel: () => void;
  onSubmit: (mode: 'send' | 'queue') => void;
}) {
  const [sendMenuOpen, setSendMenuOpen] = useState(false);
  const [clock, setClock] = useState(() => Date.now());
  const affordance = useRef<HTMLButtonElement>(null);
  const panel = useRef<HTMLDivElement>(null);
  const editor = useRef<HTMLTextAreaElement>(null);
  const number = line.kind === 'delete' ? line.oldLine : line.newLine;
  const kind =
    line.kind === 'add'
      ? 'add'
      : line.kind === 'delete'
        ? 'del'
        : line.kind === 'hunk'
          ? 'hunk'
          : 'ctx';
  const canSubmit = Boolean(body.trim());
  const cancel = () => {
    setSendMenuOpen(false);
    onCancel();
    requestAnimationFrame(() => affordance.current?.focus({ preventScroll: true }));
  };
  const submit = (mode: 'send' | 'queue') => {
    if (!canSubmit) return;
    setSendMenuOpen(false);
    onSubmit(mode);
  };
  useEffect(() => {
    if (!commenting) return;
    setClock(Date.now());
    const timer = window.setInterval(() => setClock(Date.now()), 60_000);
    return () => clearInterval(timer);
  }, [commenting]);
  useEffect(() => {
    if (!commenting || !canSubmit) setSendMenuOpen(false);
  }, [canSubmit, commenting]);
  useEffect(() => {
    if (!commenting) return;
    const frame = requestAnimationFrame(() => {
      const reduceMotion = globalThis.matchMedia?.('(prefers-reduced-motion: reduce)').matches;
      panel.current?.scrollIntoView?.({
        block: 'nearest',
        behavior: reduceMotion ? 'auto' : 'smooth',
      });
    });
    return () => cancelAnimationFrame(frame);
  }, [commenting]);
  useEffect(() => {
    if (!commenting || !editor.current) return;
    resizeCommentEditor(editor.current);
  }, [body, commenting]);
  useEffect(() => {
    if (!commenting || !editor.current || typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver(() => {
      if (editor.current) resizeCommentEditor(editor.current);
    });
    observer.observe(editor.current);
    return () => observer.disconnect();
  }, [commenting]);
  return (
    <div
      class={`diff-row ${kind}${commenting ? ' commenting' : ''}`}
      data-commentable={Boolean(number && line.kind !== 'hunk')}
    >
      <span class="diff-ln">{line.oldLine || ''}</span>
      <span class="diff-ln">{line.newLine || ''}</span>
      <DiffCode line={line} emphasis={emphasis} lang={lang} />
      {number && line.kind !== 'hunk' && (
        <button
          ref={affordance}
          class={`diff-comment-affordance${comments.length ? ' has-comments' : ''}${comments.some((comment) => comment.queued) ? ' queued' : ''}`}
          type="button"
          aria-label={
            comments.length
              ? `Show ${comments.length} inline comment${comments.length === 1 ? '' : 's'} for line ${number}`
              : `Comment on line ${number}`
          }
          aria-expanded={commenting}
          onMouseDown={(event) => {
            event.preventDefault();
            event.stopPropagation();
            onComment(commentKey);
          }}
          onKeyDown={(event) => {
            if (event.key === 'Enter' || event.key === ' ') {
              event.preventDefault();
              event.stopPropagation();
              onComment(commentKey);
            }
          }}
        >
          {!comments.length && <Icon name="add" />}
        </button>
      )}
      {commenting && (
        <div
          ref={panel}
          class="diff-comment-panel"
          role="region"
          aria-label={`Inline comments for line ${number}`}
          onKeyDown={(event) => {
            if (event.key !== 'Escape') return;
            event.preventDefault();
            event.stopPropagation();
            if (sendMenuOpen) {
              setSendMenuOpen(false);
              editor.current?.focus();
            } else if (!body.trim()) cancel();
          }}
        >
          <div class="diff-comment-heading">
            <span class="diff-comment-line-chip">Line {number}</span>
            <span>{line.kind === 'delete' ? 'Original' : 'Current'} version</span>
          </div>
          {comments.map((comment) => {
            const status = comment.queued ? 'queued' : comment.optimistic ? 'sending' : 'sent';
            const timestamp = commentTimestamp(comment.createdAt, clock);
            return (
              <div class={`diff-comment-history-item ${status}`} key={comment.id}>
                <div class="diff-comment-history-text">{comment.body}</div>
                <div class="diff-comment-history-meta">
                  <span class={`diff-comment-status ${status}`}>
                    <span class="diff-comment-status-icon" aria-hidden="true">
                      {status === 'sent' && <Icon name="check" />}
                    </span>
                    {status === 'queued' ? 'Queued' : status === 'sending' ? 'Sending' : 'Sent'}
                  </span>
                  {timestamp && (
                    <time dateTime={timestamp.dateTime} title={timestamp.title}>
                      {timestamp.label}
                    </time>
                  )}
                </div>
              </div>
            );
          })}
          <form
            class="diff-comment-editor"
            onSubmit={(event) => {
              event.preventDefault();
              submit('send');
            }}
          >
            <textarea
              ref={editor}
              autoFocus
              aria-label="Inline comment"
              placeholder={comments.length ? 'Add a follow-up…' : 'Add a comment…'}
              value={body}
              onInput={(event) => onBody(event.currentTarget.value)}
              onKeyDown={(event) => {
                if (event.key === 'Enter' && (event.metaKey || event.ctrlKey)) {
                  event.preventDefault();
                  event.stopPropagation();
                  submit('send');
                }
              }}
            />
            <div class="diff-comment-editor-actions">
              <span class="diff-comment-shortcut">⌘/Ctrl + Enter to send</span>
              <button class="diff-comment-cancel" type="button" onClick={cancel}>
                Cancel
              </button>
              <div class="diff-comment-send-split">
                <button class="diff-comment-send" type="submit" disabled={!canSubmit}>
                  Send now
                </button>
                <button
                  class="diff-comment-send-more"
                  type="button"
                  aria-label="More send options"
                  aria-expanded={sendMenuOpen}
                  disabled={!canSubmit}
                  onClick={() => setSendMenuOpen(!sendMenuOpen)}
                >
                  ▾
                </button>
                {sendMenuOpen && (
                  <div class="diff-comment-send-menu">
                    <button
                      class="diff-comment-send-option"
                      type="button"
                      disabled={!canSubmit}
                      onClick={() => submit('send')}
                    >
                      Send now
                    </button>
                    <button
                      class="diff-comment-send-option"
                      type="button"
                      disabled={!canSubmit}
                      onClick={() => submit('queue')}
                    >
                      Queue comment
                      <small>Deliver later as one batch</small>
                    </button>
                  </div>
                )}
              </div>
            </div>
          </form>
        </div>
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
  glyph: string;
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

function splitDiffPath(path: string): { base: string; dir: string } {
  const display = String(path || '').replace(/^\/+/, '');
  const index = display.lastIndexOf('/');
  return index < 0
    ? { base: display, dir: '' }
    : { base: display.slice(index + 1), dir: display.slice(0, index) };
}

function File({ file }: { file: DiffFile }) {
  const store = useStore();
  const [limit, setLimit] = useState(500);
  const [commenting, setCommenting] = useState('');
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  const lines = file.lines || [];
  const kind = fileKind(file);
  const legacyKind = kind === 'add' ? 'create' : kind === 'delete' ? 'delete' : 'modify';
  const name = splitDiffPath(file.path);
  const canExpandContext =
    !file.truncated &&
    lines.length > 0 &&
    (file.context || 3) < Math.max(file.oldLineCount || 0, file.newLineCount || 0);
  const expandContext = () =>
    void store.expandDiff(file, Math.min(100_000, Math.max(12, (file.context || 3) * 4)));
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
  };
  const submitComment = (mode: 'send' | 'queue', line: DiffLine, key: string) => {
    const number = line.kind === 'delete' ? line.oldLine : line.newLine;
    const body = drafts[key] || '';
    if (!body.trim() || !number) return;
    const comment = {
      path: file.path,
      side: line.kind === 'delete' ? ('old' as const) : ('new' as const),
      line: number,
      body: body.trim(),
      scope: store.diff.value.scope,
      context: line.content,
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
  const toggle = () => void store.expandDiff(file);
  return (
    <section class={`diff-file diff-file-${legacyKind}`}>
      <div
        class={`diff-file-row ${file.expanded ? 'expanded' : ''}`}
        role="button"
        tabIndex={0}
        title={file.path}
        data-path={file.path}
        aria-expanded={file.expanded}
        onClick={toggle}
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
          {file.provenance && (
            <span
              class="diff-count-muted"
              title={
                file.provenance === 'direct'
                  ? 'Witnessed by a direct mutation tool'
                  : 'Declared before shell execution and verified by bounded detection'
              }
            >
              {file.provenance === 'direct' ? ' direct tool' : ' declared and verified'}
            </span>
          )}
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
        <span class="diff-file-actions">
          <DiffAction label={`Copy path ${file.path}`} glyph="⧉" value={() => file.path} />
          {!file.image && (
            <DiffAction label={`Copy diff for ${file.path}`} glyph="±" value={patch} />
          )}
        </span>
      </div>
      {file.expanded && (
        <div class="diff-file-body">
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
          {file.baselineState === 'preexisting_dirty' && (
            <div class="diff-error">
              The attributed transition started from content that was already dirty.
            </div>
          )}
          {file.truncated && (
            <div class="diff-error">
              Diff content unavailable ({file.contentStatus || 'retention unavailable'}); line
              counts may be partial.
            </div>
          )}
          {file.image ? (
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
              {canExpandContext && (
                <button
                  class="diff-hunk-expand diff-hunk-expand-above"
                  type="button"
                  aria-label="Show more context above"
                  disabled={file.loading}
                  onClick={expandContext}
                >
                  <Icon name="chevron-up" />
                  <span>Show more above</span>
                </button>
              )}
              <div class={`diff-rows diff-rows-kind-${legacyKind}`}>
                {lines.slice(0, limit).map((line, index) => {
                  const key = `${line.kind}-${line.oldLine || 0}-${line.newLine || 0}-${index}`;
                  const number = line.kind === 'delete' ? line.oldLine : line.newLine;
                  const side = line.kind === 'delete' ? 'old' : 'new';
                  const matchesAnchor = (comment: DiffComment) =>
                    (!comment.sessionId || comment.sessionId === store.diff.value.sessionId) &&
                    comment.path === file.path &&
                    comment.side === side &&
                    comment.line === number &&
                    (!comment.scope || comment.scope === store.diff.value.scope);
                  const comments: Array<DiffComment & { queued?: boolean }> = [
                    ...store.diff.value.historyComments.filter(matchesAnchor),
                    ...store.diff.value.comments.filter(matchesAnchor).map((comment) => ({
                      ...comment,
                      queued: true,
                    })),
                  ].sort((left, right) => (left.createdAt || 0) - (right.createdAt || 0));
                  return (
                    <Line
                      key={key}
                      line={line}
                      emphasis={emphasis.get(index)}
                      lang={lines.length <= 1500 ? file.lang || '' : ''}
                      commentKey={key}
                      commenting={commenting === key}
                      comments={comments}
                      body={drafts[key] || ''}
                      onComment={(next) => {
                        setCommenting((current) => (current === next ? '' : next));
                      }}
                      onBody={(value) => {
                        setDrafts((current) => ({ ...current, [key]: value }));
                      }}
                      onCancel={() => {
                        clearDraft(key);
                        setCommenting('');
                      }}
                      onSubmit={(mode) => submitComment(mode, line, key)}
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
              {canExpandContext && (
                <button
                  class="diff-hunk-expand diff-hunk-expand-below"
                  type="button"
                  aria-label="Show more context below"
                  disabled={file.loading}
                  onClick={expandContext}
                >
                  <span>Show more below</span>
                  <Icon name="chevron-down" />
                </button>
              )}
            </>
          )}
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
  const options = (state.git ? DIFF_SCOPE_OPTIONS : DIFF_SCOPE_OPTIONS.slice(0, 2)).map(
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
  useEffect(() => {
    if (!state.open) return;
    const escape = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return;
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
  }, [state.open, state.maximized, store]);
  if (!state.open) return null;
  const files = state.files.filter((file) =>
    file.path.toLowerCase().includes(state.filter.toLowerCase()),
  );
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
  return (
    <aside
      ref={aside}
      class={`diff-sidebar open ${state.maximized ? 'maximized' : ''}`}
      id="diffSidebar"
      aria-label="Session file changes"
    >
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
        <DiffScopePicker />
        <span class="diff-sidebar-totals">
          {adds > 0 && <span class="diff-sidebar-totals-add">+{adds}</span>}
          {dels > 0 && <span class="diff-sidebar-totals-del">−{dels}</span>}
          {state.unavailableLineCountFiles > 0 && (
            <span class="diff-count-muted" title="Attributed files with unavailable line counts">
              {' '}
              partial ({state.unavailableLineCountFiles})
            </span>
          )}
        </span>
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
          class="icon-btn diff-sidebar-close"
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
            <button class="diff-retry" onClick={() => void store.loadDiff()}>
              Retry
            </button>
          </div>
        )}
        {files.map((file) => (
          <File key={file.path} file={file} />
        ))}
        {!state.loading && !state.error && !files.length && (
          <div class="diff-empty">No file changes in this scope.</div>
        )}
      </div>
      {state.materializations.length > 0 && (
        <details class="diff-observations">
          <summary>Materialized outputs ({state.materializations.length})</summary>
          {state.materializations.map((item) => (
            <div key={item.id}>
              {item.root || item.sampledPaths[0] || 'Output'} — {item.createdCount} created,{' '}
              {item.modifiedCount} modified, {item.deletedCount} deleted (no authored line totals)
            </div>
          ))}
        </details>
      )}
      {state.observations.length > 0 && (
        <details class="diff-observations">
          <summary>Observed side effects ({state.observations.length})</summary>
          <div>Observed during commands; not attributed as agent task output.</div>
          {state.observations.map((item) => (
            <div key={item.id}>
              {item.root || item.sampledPaths[0] || item.classification} — {item.createdCount}{' '}
              created, {item.modifiedCount} modified, {item.deletedCount} deleted; coverage{' '}
              {item.coverageStatus}
            </div>
          ))}
        </details>
      )}
      {state.claimDiagnostics.length > 0 && (
        <details class="diff-observations" open>
          <summary>Output claim diagnostics ({state.claimDiagnostics.length})</summary>
          {state.claimDiagnostics.map((item, index) => (
            <div key={`${item.reason}-${index}`}>
              {item.reason}: {item.normalizedPattern || item.message || 'tracking diagnostic'}
            </div>
          ))}
        </details>
      )}
      {comments.length > 0 && (
        <div class="diff-queue-bar">
          <span class="diff-queue-count">{comments.length} queued</span>
          <button
            class="diff-queue-discard"
            type="button"
            onClick={() => {
              const remaining = state.comments.filter(
                (comment) => comment.sessionId && comment.sessionId !== state.sessionId,
              );
              store.diff.value = { ...state, comments: remaining };
            }}
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
    </aside>
  );
}

function useMediaQuery(query: string): boolean {
  const [matches, setMatches] = useState(() => matchMedia(query).matches);
  useEffect(() => {
    const media = matchMedia(query);
    const update = () => setMatches(media.matches);
    update();
    media.addEventListener('change', update);
    return () => media.removeEventListener('change', update);
  }, [query]);
  return matches;
}

export function PlanSurface() {
  const store = useStore();
  const plan = store.currentPlan.value;
  const open = store.planVisible.value;
  const mobile = useMediaQuery('(max-width: 767px)');
  const surface = useRef<HTMLElement>(null);
  const summary = planSummary(plan);

  useEffect(() => {
    if (open) store.planSeen.value = summary.signature;
  }, [open, store, summary.signature]);

  useEffect(() => {
    if (!open) return;
    const returnFocus =
      document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const blocked = mobile
      ? ['sidebar', 'appMain']
          .map((id) => document.getElementById(id))
          .filter((element): element is HTMLElement => Boolean(element))
          .map((element) => ({ element, inert: element.inert }))
      : [];
    blocked.forEach(({ element }) => {
      element.inert = true;
    });
    if (mobile) {
      const active = document.activeElement;
      if (active instanceof HTMLElement && active.closest('.composer')) active.blur();
      requestAnimationFrame(() => surface.current?.focus({ preventScroll: true }));
    }
    const escape = (event: KeyboardEvent) => {
      if (
        event.key !== 'Escape' ||
        store.modal.peek() ||
        store.askUser.peek() ||
        store.approval.peek() ||
        store.modal.peek() === 'side'
      )
        return;
      event.preventDefault();
      store.closePlan();
    };
    addEventListener('keydown', escape);
    return () => {
      removeEventListener('keydown', escape);
      blocked.forEach(({ element, inert }) => {
        element.inert = inert;
      });
      if (document.contains(returnFocus)) returnFocus?.focus({ preventScroll: true });
    };
  }, [mobile, open, store]);

  if (!plan) return null;

  const trapSheetFocus = (event: KeyboardEvent) => {
    if (!mobile || event.key !== 'Tab') return;
    const focusable = [
      ...(surface.current?.querySelectorAll<HTMLElement>(
        'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
      ) || []),
    ];
    if (!focusable.length) {
      event.preventDefault();
      surface.current?.focus();
      return;
    }
    const first = focusable[0];
    const last = focusable.at(-1)!;
    if (
      document.activeElement === surface.current ||
      !surface.current?.contains(document.activeElement)
    ) {
      event.preventDefault();
      (event.shiftKey ? last : first).focus();
    } else if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  };

  const progress = summary.completed / Math.max(1, summary.total);
  const announcement = summary.complete
    ? `Plan complete. All ${summary.total} steps finished.`
    : summary.activeStep
      ? `Step ${summary.position} of ${summary.total}: ${summary.activeStep}`
      : `Plan has ${summary.total} steps. ${summary.completed} complete.`;

  return (
    <>
      {mobile && open && (
        <div
          class="plan-sheet-backdrop open"
          aria-hidden="true"
          onClick={() => store.closePlan()}
        />
      )}
      <aside
        ref={surface}
        class={`plan-surface ${mobile ? 'plan-sheet' : 'plan-panel'} ${open ? 'open' : ''}`}
        id="planSurface"
        role={mobile ? 'dialog' : 'complementary'}
        aria-modal={mobile ? true : undefined}
        aria-hidden={!open}
        aria-labelledby="planSurfaceTitle"
        tabIndex={-1}
        onKeyDown={trapSheetFocus}
      >
        {mobile && <div class="plan-sheet-handle" aria-hidden="true" />}
        <div class="plan-surface-header">
          <h2 id="planSurfaceTitle">Current plan</h2>
          <span class={`plan-surface-progress ${summary.complete ? 'complete' : ''}`}>
            {summary.complete ? 'Complete' : `Step ${summary.position} of ${summary.total}`}
          </span>
          <button
            class="icon-btn"
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
      </aside>
    </>
  );
}
