import '../styles/features/context-card.css';
import type { RefObject } from 'preact';
import { useLayoutEffect, useRef } from 'preact/hooks';
import { useStore } from '../app/context';
import { formatPercent, formatTokens, type ContextReading } from '../domain/context-meter';
import { observePopoverPosition, positionPopover } from '../platform/browser';
import { containPopoverScroll } from '../platform/popover-scroll';
import { ContextRing } from './ContextRing';
import { Icon } from './Icon';

export interface ContextCardProps {
  id: string;
  reading: ContextReading;
  hint: string;
  trigger: RefObject<HTMLButtonElement | null>;
  onClose: () => void;
}

const share = (tokens: number, limit: number) => `${Math.min(100, (tokens / limit) * 100)}%`;

/** The whole window as a bar, with term-llm's auto-compaction point marked when it has one. */
function ContextBar({ reading, limit }: { reading: ContextReading; limit: number }) {
  const compactAt = reading.autoCompactAt;
  const style: Record<string, string> = { '--context-fill': share(reading.used, limit) };
  if (compactAt !== undefined) style['--context-compact-at'] = share(compactAt, limit);
  return (
    <div class="context-bar-wrap" style={style} aria-hidden="true">
      <div class="context-bar">
        {compactAt !== undefined && <span class="context-bar-zone" />}
        <span class="context-bar-fill" />
        {compactAt !== undefined && <span class="context-bar-marker" />}
      </div>
      <div class="context-bar-scale">
        <span class="context-bar-scale-start">0</span>
        {compactAt !== undefined && (
          <span class={`context-bar-scale-mark ${compactAt < limit / 2 ? 'after' : ''}`}>
            auto-compact {formatTokens(compactAt)}
          </span>
        )}
        <span class="context-bar-scale-end">{formatTokens(limit)}</span>
      </div>
    </div>
  );
}

function contextNote(reading: ContextReading): string {
  if (reading.limit === undefined) return 'The window size for this model is not known.';
  if (reading.untilAutoCompact === undefined) return '';
  return reading.untilAutoCompact > 0
    ? `${formatTokens(reading.untilAutoCompact)} left before term-llm compacts automatically.`
    : 'Past the auto-compaction point: term-llm compacts at its next checkpoint.';
}

/**
 * The context meter's details: occupancy against the window, where term-llm
 * compacts on its own, and manual compaction. Loaded on first open.
 */
export function ContextCard({ id, reading, hint, trigger, onClose }: ContextCardProps) {
  const store = useStore();
  const popover = useRef<HTMLDialogElement>(null);
  const initialFocus = useRef<HTMLButtonElement>(null);
  useLayoutEffect(() => {
    if (!trigger.current || !popover.current) return;
    positionPopover(trigger.current, popover.current, true);
    initialFocus.current?.focus({ preventScroll: true });
    const stopPositioning = observePopoverPosition(trigger.current, popover.current, true);
    const stopScrollContainment = containPopoverScroll(popover.current);
    return () => {
      stopPositioning();
      stopScrollContainment();
    };
  }, [trigger]);
  const close = () => {
    const panel = popover.current;
    if (panel && typeof panel.close === 'function') panel.close();
    onClose();
  };
  const compacting = store.runEngine.compacting.value;
  const running = store.runActive.value;
  const limit = reading.limit;
  const note = contextNote(reading);
  return (
    <dialog
      ref={popover}
      id={id}
      class="chip-popover context-popover"
      data-tone={reading.tone}
      aria-label="Context window"
      onCancel={(event) => {
        event.preventDefault();
        close();
      }}
      onClick={(event) => {
        if (event.target === event.currentTarget) close();
      }}
    >
      <div class="runtime-popover-header">
        <div class="runtime-popover-heading">
          <div class="runtime-popover-title">Context window</div>
          {hint && <div class="runtime-popover-hint">{hint}</div>}
        </div>
        <button
          ref={initialFocus}
          type="button"
          class="runtime-popover-close close-button"
          aria-label="Close context window"
          onClick={close}
        >
          <Icon name="close" />
        </button>
      </div>
      <div class="context-popover-summary">
        {limit !== undefined && (
          <ContextRing reading={reading} size={76} stroke={7} label={formatPercent(reading)} />
        )}
        <dl class="context-popover-figures">
          <dt>In context</dt>
          <dd title={reading.used.toLocaleString('en-US')}>{formatTokens(reading.used)}</dd>
          {reading.free !== undefined && (
            <>
              <dt>Free</dt>
              <dd>{formatTokens(reading.free)}</dd>
            </>
          )}
          <dt>Window</dt>
          <dd>{limit === undefined ? 'Unknown' : formatTokens(limit)}</dd>
        </dl>
      </div>
      {limit !== undefined && <ContextBar reading={reading} limit={limit} />}
      {note && <p class="context-popover-note">{note}</p>}
      <div class="context-popover-actions">
        <button
          type="button"
          class="btn"
          disabled={compacting || running}
          title={running && !compacting ? 'Available when the current reply finishes' : undefined}
          onClick={() => {
            close();
            void store.selectionStore.compactSession();
          }}
        >
          <Icon name="compact" />
          {compacting ? 'Compacting…' : 'Compact now'}
        </button>
        <button
          type="button"
          class="btn"
          onClick={() => {
            close();
            store.modal.value = 'stats';
          }}
        >
          Session stats
        </button>
      </div>
    </dialog>
  );
}
