import { useRef } from 'preact/hooks';
import { useStore } from '../app/context';
import { Drawer } from './Drawer';
import { Icon } from './Icon';
import { lazyComponent } from './lazyComponent';
import { useMediaQuery } from './useMediaQuery';

function PanelPlaceholder({ failed = false }: { failed?: boolean }) {
  const store = useStore();
  const mobile = useMediaQuery('(max-width: 767px)');
  const diff = store.diff.value.open;
  if (!diff && !store.planVisible.value) return null;
  const title = diff ? 'Session file changes' : 'Current plan';
  const id = diff ? 'diffSidebar' : 'planSurface';
  const className = diff ? 'diff-sidebar open' : 'plan-surface open';
  const close = () => {
    if (diff) store.diff.value = { ...store.diff.peek(), open: false, maximized: false };
    else store.closePlan();
  };
  const content = (
    <>
      <div class={diff ? 'diff-sidebar-header' : 'plan-surface-header'}>
        <strong>{title}</strong>
        <button class="icon-btn close-button" aria-label={`Close ${title}`} onClick={close}>
          <Icon name="close" />
        </button>
      </div>
      <p role={failed ? 'alert' : 'status'}>
        {failed ? 'Could not load this panel. Reload the page to retry.' : 'Loading panel…'}
      </p>
    </>
  );
  if (mobile)
    return (
      <Drawer
        open
        id={id}
        title={title}
        className={diff ? className : 'plan-sheet open'}
        side={diff ? 'right' : 'bottom'}
        onClose={close}
      >
        {content}
      </Drawer>
    );
  return (
    <aside id={id} class={className} aria-label={title} aria-busy={!failed || undefined}>
      {content}
    </aside>
  );
}

const DeferredPanels = lazyComponent(
  () =>
    import('./Panels')
      .then(({ DiffSidebar, PlanSurface }) => () => (
        <>
          <DiffSidebar />
          <PlanSurface />
        </>
      ))
      .catch(() => () => <PanelPlaceholder failed />),
  <PanelPlaceholder />,
);

/** Test/intent preloading shares the same import used by the first open panel. */
export const preloadCompanionPanels = () => DeferredPanels.preload();

/** Keep panel hooks mounted after first use, but download no viewer code for ordinary chat. */
export function CompanionPanels() {
  const store = useStore();
  const requested = useRef(false);
  requested.current ||= store.diff.value.open || store.planVisible.value;
  return requested.current ? <DeferredPanels /> : null;
}
