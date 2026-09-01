import type { ComponentChildren } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import type { DiffComment } from '../domain/types';
import { Icon } from './Icon';
import { useMenuKeyboard } from './Menu';

export type ReviewCommentEntry = DiffComment & { queued?: boolean };

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

export function ReviewComment({
  controlId,
  commenting,
  comments,
  body,
  affordanceLabel,
  regionLabel,
  heading,
  onToggle,
  onBody,
  onCancel,
  onSubmit,
  onEdit,
  onReanchor,
  onRemove,
  onReveal,
  showCount = false,
  showAnchorLine = false,
  allowNew = true,
  submitDisabled = false,
  reanchorDisabled = false,
}: {
  controlId: string;
  commenting: boolean;
  comments: ReviewCommentEntry[];
  body: string;
  affordanceLabel: string;
  regionLabel: string;
  heading: ComponentChildren;
  onToggle: () => void;
  onBody: (value: string) => void;
  onCancel: () => void;
  onSubmit: (mode: 'send' | 'queue') => void;
  onEdit: (comment: DiffComment) => void;
  onReanchor: (comment: DiffComment) => void;
  onRemove: (comment: DiffComment) => void;
  onReveal?: (comment: DiffComment) => void;
  showCount?: boolean;
  showAnchorLine?: boolean;
  allowNew?: boolean;
  submitDisabled?: boolean;
  reanchorDisabled?: boolean;
}) {
  const [sendMenuOpen, setSendMenuOpen] = useState(false);
  const [clock, setClock] = useState(() => Date.now());
  const affordance = useRef<HTMLButtonElement>(null);
  const panel = useRef<HTMLDivElement>(null);
  const editor = useRef<HTMLTextAreaElement>(null);
  const sendTrigger = useRef<HTMLButtonElement>(null);
  const menuId = `${controlId}-delivery-menu`;
  const sendMenu = useMenuKeyboard(sendMenuOpen, () => setSendMenuOpen(false), sendTrigger);
  const canSubmit = Boolean(body.trim()) && !submitDisabled;
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
    <>
      <button
        ref={affordance}
        class={`diff-comment-affordance${comments.length ? ' has-comments' : ''}${comments.some((comment) => comment.queued) ? ' queued' : ''}`}
        type="button"
        aria-label={affordanceLabel}
        aria-expanded={commenting}
        aria-controls={`${controlId}-panel`}
        onMouseDown={(event) => {
          event.preventDefault();
          event.stopPropagation();
        }}
        onClick={(event) => {
          event.stopPropagation();
          onToggle();
        }}
      >
        {comments.length ? (
          showCount ? (
            <span class="diff-comment-count" aria-hidden="true">
              {comments.length}
            </span>
          ) : null
        ) : (
          <Icon name="add" />
        )}
      </button>
      {commenting && (
        <div
          ref={panel}
          id={`${controlId}-panel`}
          class="diff-comment-panel"
          role="region"
          aria-label={regionLabel}
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
          <div class="diff-comment-heading">{heading}</div>
          {comments.map((comment) => {
            const status = comment.queued ? 'queued' : comment.optimistic ? 'sending' : 'sent';
            const timestamp = commentTimestamp(comment.createdAt, clock);
            return (
              <div class={`diff-comment-history-item ${status}`} key={comment.id}>
                <div class="diff-comment-history-text">
                  {showAnchorLine && (
                    <span class="diff-comment-anchor-line">Line {comment.line}</span>
                  )}
                  {comment.body}
                </div>
                <div class="diff-comment-history-meta">
                  <span class={`diff-comment-status ${status}`}>
                    <span class="diff-comment-status-icon" aria-hidden="true">
                      {status === 'sent' && <Icon name="check" />}
                    </span>
                    {status === 'queued'
                      ? comment.state === 'stale'
                        ? 'Stale'
                        : 'Queued'
                      : status === 'sending'
                        ? 'Sending'
                        : 'Sent'}
                  </span>
                  {comment.queued && comment.id && (
                    <span class="diff-comment-actions">
                      {comment.state === 'stale' && (
                        <button
                          type="button"
                          disabled={reanchorDisabled}
                          onClick={() => onReanchor(comment)}
                        >
                          Re-anchor here
                        </button>
                      )}
                      <button type="button" onClick={() => onEdit(comment)}>
                        Edit
                      </button>
                      <button type="button" onClick={() => onRemove(comment)}>
                        Remove
                      </button>
                    </span>
                  )}
                  {onReveal && (
                    <button type="button" onClick={() => onReveal(comment)}>
                      Reveal in Diff
                    </button>
                  )}
                  {timestamp && (
                    <time dateTime={timestamp.dateTime} title={timestamp.title}>
                      {timestamp.label}
                    </time>
                  )}
                </div>
              </div>
            );
          })}
          {allowNew && (
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
                    ref={sendTrigger}
                    class="diff-comment-send-more"
                    type="button"
                    aria-label="More send options"
                    aria-haspopup="menu"
                    aria-controls={menuId}
                    aria-expanded={sendMenuOpen}
                    disabled={!canSubmit}
                    onClick={() => setSendMenuOpen(!sendMenuOpen)}
                  >
                    ▾
                  </button>
                  {sendMenuOpen && (
                    <div
                      ref={sendMenu}
                      id={menuId}
                      class="diff-comment-send-menu"
                      role="menu"
                      aria-label="Comment delivery"
                    >
                      <button
                        class="diff-comment-send-option"
                        type="button"
                        role="menuitem"
                        disabled={!canSubmit}
                        onClick={() => submit('send')}
                      >
                        Send now
                      </button>
                      <button
                        class="diff-comment-send-option"
                        type="button"
                        role="menuitem"
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
          )}
        </div>
      )}
    </>
  );
}
