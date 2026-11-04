import { useEffect, useId, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { describeContext, formatPercent, formatTokens, readContext } from '../domain/context-meter';
import { compactModelLabel } from '../domain/runtime';
import type { ContextCardProps } from './ContextCard';
import { ContextRing } from './ContextRing';
import { lazyComponent } from './lazyComponent';

// A failed import closes the card and says so rather than leaving the meter
// expanded over nothing.
function ContextCardLoadError({ onClose }: ContextCardProps) {
  const store = useStore();
  const reported = useRef(false);
  useEffect(() => {
    if (reported.current) return;
    reported.current = true;
    store.toast('Could not load the context details. Reload the page to retry.', 'error');
    onClose();
  }, [store, onClose]);
  return null;
}

const LazyContextCard = lazyComponent<ContextCardProps>(() =>
  import('./ContextCard')
    .then(({ ContextCard }) => ContextCard)
    .catch((error: unknown) => {
      console.error('Failed to load the context details', error);
      return ContextCardLoadError;
    }),
);

/**
 * Header meter for the active session's approximate context occupancy: a ring
 * with "22% · 160k" that narrows to the ring alone on the smallest phones. It
 * opens a details card with compaction controls, loaded on demand.
 */
export function ContextMeter() {
  const store = useStore();
  const [open, setOpen] = useState(false);
  const cardID = useId();
  const trigger = useRef<HTMLButtonElement>(null);
  const session = store.draftActive.value ? null : store.activeSession.value;
  const usage = session?.contextUsage;
  const visible = Boolean(usage && usage.usedTokens > 0);
  // A card left open must not reappear for another session or a later reading.
  useEffect(() => setOpen(false), [session?.id, visible]);
  if (!session || !usage || !visible) return null;

  const reading = readContext(usage);
  const limit = reading.limit;
  const summary = `Context window: ${describeContext(reading)}`;
  const model = compactModelLabel(session.activeModel || '');
  const close = () => {
    setOpen(false);
    trigger.current?.focus({ preventScroll: true });
  };
  return (
    <>
      <button
        ref={trigger}
        type="button"
        class={`header-action context-meter${limit === undefined ? '' : ' has-window'}`}
        data-tone={reading.tone}
        aria-haspopup="dialog"
        aria-controls={cardID}
        aria-expanded={open}
        aria-label={summary}
        title={summary}
        onPointerEnter={() => void LazyContextCard.preload()}
        onFocus={() => void LazyContextCard.preload()}
        onClick={() => (open ? close() : setOpen(true))}
      >
        {limit !== undefined && <ContextRing reading={reading} size={16} stroke={2.25} />}
        <span class="context-meter-text">
          <span class="context-meter-value">
            {limit === undefined ? formatTokens(reading.used) : formatPercent(reading)}
          </span>
          <span class="context-meter-detail">
            {limit === undefined ? ' tokens' : ` · ${formatTokens(limit)}`}
          </span>
        </span>
      </button>
      {open && (
        <LazyContextCard
          id={cardID}
          reading={reading}
          hint={reading.estimated ? (model ? `${model} · estimated` : 'Estimated') : model}
          trigger={trigger}
          onClose={close}
        />
      )}
    </>
  );
}
