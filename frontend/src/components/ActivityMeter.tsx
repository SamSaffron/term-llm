import { createPortal } from 'preact/compat';
import { useEffect, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { activeExecutionCount, activityUncertain } from '../domain/session-activity-summary';
import { lazyComponent } from './lazyComponent';
import { Overlay } from './Overlay';

const ActivitySheet = lazyComponent<{ onClose: () => void }>(() =>
  import('./ActivitySheet')
    .then((module) => module.ActivitySheet)
    .catch(
      () =>
        function Unavailable({ onClose }) {
          return (
            <Overlay title="Activity" onClose={onClose}>
              <p>Activity could not be loaded. Close and reload to try again.</p>
            </Overlay>
          );
        },
    ),
);

function Robot({ x, y, front = false }: { x: number; y: number; front?: boolean }) {
  return (
    <g
      transform={`translate(${x} ${y}) scale(.58)`}
      fill="var(--bg)"
      stroke="currentColor"
      stroke-width="1.35"
      stroke-linecap="round"
      stroke-linejoin="round"
    >
      <rect x="3" y="5" width="18" height="15" rx="4" vector-effect="non-scaling-stroke" />
      <path d="M3 11H1v4h2m18-4h2v4h-2" fill="none" vector-effect="non-scaling-stroke" />
      {front && (
        <>
          <path d="M12 5V2" vector-effect="non-scaling-stroke" />
          <circle cx="12" cy="2" r="1" fill="currentColor" stroke="none" />
          <circle cx="8.3" cy="11.5" r="1.3" fill="currentColor" stroke="none" />
          <circle cx="15.7" cy="11.5" r="1.3" fill="currentColor" stroke="none" />
          <path d="M9 16h6" vector-effect="non-scaling-stroke" />
        </>
      )}
    </g>
  );
}

export function ActivityGlyph({ count }: { count: number }) {
  const heads = Math.min(3, Math.max(1, count));
  // Centre the painted cascade, not just its 22×16 canvas (approved geometry).
  const [x, y] = heads === 3 ? [1.04, -0.43] : heads === 2 ? [2.54, 0.57] : [4.04, 1.57];
  return (
    <svg
      class="activity-glyph"
      width="22"
      height="16"
      viewBox="0 0 22 16"
      aria-hidden="true"
      focusable="false"
    >
      {heads >= 3 && <Robot x={x + 6} y={y + 4} />}
      {heads >= 2 && <Robot x={x + 3} y={y + 2} />}
      <Robot x={x} y={y} front />
    </svg>
  );
}

export function ActivityMeter() {
  const store = useStore();
  const children = store.childSessionStore;
  const scope = children.activitySessionId.value;
  const hasRows =
    children.activity.value?.agents?.some((child) => child.kind !== 'skill_run') ||
    !!children.activity.value?.jobs?.length;
  const count = activeExecutionCount(children.activity.value);
  const uncertain = activityUncertain(children.activity.value, children.activityStale.value);
  const caveat = uncertain ? ' · Activity may be incomplete or out of date' : '';
  const label = count ? `${count} active` : uncertain ? 'Status unknown' : 'idle';
  const [open, setOpen] = useState(false);
  useEffect(() => setOpen(false), [scope]);
  if (
    !scope ||
    (!children.activity.value && !children.activityStale.value) ||
    (!hasRows && !uncertain)
  )
    return null;
  return (
    <>
      <button
        type="button"
        class="activity-trigger header-action narrow-header-action"
        style={{ paddingInline: '6.4px', color: count ? undefined : '#8a919a' }}
        data-state={count ? 'active' : uncertain ? 'unknown' : 'idle'}
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-label={`Activity: ${label}${caveat}`}
        title={`Activity: ${label}${caveat}`}
        onClick={() => setOpen(true)}
      >
        <span
          class="activity-cluster"
          style={{ display: 'inline-flex', alignItems: 'center', gap: '6.72px' }}
        >
          <ActivityGlyph count={count} />
          {count > 0 && <span class="activity-count">{count}</span>}
        </span>
      </button>
      {open && createPortal(<ActivitySheet onClose={() => setOpen(false)} />, document.body)}
    </>
  );
}
