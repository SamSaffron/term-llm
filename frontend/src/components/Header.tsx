import { useEffect, useId, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import {
  compactModelLabel,
  defaultModel,
  defaultProvider,
  isFastServiceTier,
  splitModelEffort,
  supportedEfforts,
  supportsFastMode,
} from '../domain/runtime';
import { planSummary } from '../domain/plan';
import { observePopoverPosition, positionPopover } from '../platform/browser';
import { containPopoverScroll } from '../platform/popover-scroll';
import { Icon } from './Icon';
import { ContextMeter } from './ContextMeter';
import { lazyComponent } from './lazyComponent';
import { HeaderBreadcrumb } from './HeaderBreadcrumb';
import { resolveProjectContext } from '../domain/project-context';

// Activity is optional presentation; keep it out of the initial chat bundle.
const ActivityMeter = lazyComponent(() =>
  import('./ActivityMeter').then((module) => module.ActivityMeter).catch(() => () => null),
);

function EffortMeter() {
  return (
    <span class="effort-meter" aria-hidden="true">
      <span class="effort-meter-bar effort-meter-bar-1" />
      <span class="effort-meter-bar effort-meter-bar-2" />
      <span class="effort-meter-bar effort-meter-bar-3" />
      <span class="effort-meter-bar effort-meter-bar-4" />
    </span>
  );
}

function RuntimePicker() {
  const store = useStore();
  const [open, setOpen] = useState(false);
  const popoverID = useId();
  const trigger = useRef<HTMLButtonElement>(null);
  const popover = useRef<HTMLDialogElement>(null);
  const initialFocus = useRef<HTMLButtonElement>(null);
  const locked = store.runActive.value;
  const session = store.activeSession.value;
  const mcpOwnerId = session?.id || store.composer.runtimeDraftId();
  const pageTools = store.webMCP.available.value && store.webMCPEnabled.value ? 1 : 0;
  const mcpCount =
    pageTools +
    (store.mcp.value.ownerId === mcpOwnerId
      ? store.mcp.value.enabled.length
      : session?.mcpEnabled?.length || 0);
  const mcpNames =
    store.mcp.value.ownerId === mcpOwnerId ? store.mcp.value.enabled : session?.mcpEnabled || [];
  const mcpHint = [...mcpNames, ...(pageTools ? ['Page tools'] : [])].join(', ') || 'None enabled';
  // The selected runtime is the target of an in-progress model swap. Keep it
  // visible while the durable session still reports the previous runtime; if
  // cancelled, the selected target remains ready for the next reply.
  const selectedProvider =
    store.selectedProvider.value || (locked ? store.activeSession.value?.activeProvider || '' : '');
  const provider =
    store.providers.value.find((entry) => entry.id === selectedProvider) ||
    defaultProvider(store.providers.value);
  const selectedModel =
    store.selectedModel.value || (locked ? store.activeSession.value?.activeModel || '' : '');
  const fallbackModel = defaultModel(provider);
  const split = splitModelEffort(
    selectedModel || fallbackModel,
    store.selectedEffort.value || store.activeSession.value?.activeEffort || '',
  );
  const model = store.runtime.modelFor(selectedProvider, split.model);
  const runtimePending =
    store.runtime.modelsLoadingProvider.value === selectedProvider &&
    !store.runtime.modelCatalogs.value[selectedProvider];
  const efforts = supportedEfforts(model);
  const effort = split.effort && efforts.includes(split.effort) ? split.effort : '';
  const fastSupported = supportsFastMode(model);
  const providerFast = isFastServiceTier(provider?.service_tier);
  const fast = providerFast || (fastSupported && store.selectedFast.value);
  useLayoutEffect(() => {
    if (!open || !trigger.current || !popover.current) return;
    positionPopover(trigger.current, popover.current);
    initialFocus.current?.focus({ preventScroll: true });
    const stopPositioning = observePopoverPosition(trigger.current, popover.current);
    const stopScrollContainment = containPopoverScroll(popover.current);
    return () => {
      stopPositioning();
      stopScrollContainment();
    };
  }, [open]);
  const close = () => {
    const panel = popover.current;
    if (panel && typeof panel.close === 'function') panel.close();
    setOpen(false);
    trigger.current?.focus({ preventScroll: true });
  };
  const baseDisplayModel = compactModelLabel(model?.name || split.model) || 'Auto';
  const displayModel = runtimePending
    ? 'Loading runtime…'
    : `${baseDisplayModel}${fast && !/-fast$/i.test(baseDisplayModel) ? '-fast' : ''}`;
  const picker = (
    <div class={`model-picker ${locked ? 'locked' : ''}`}>
      <div class="model-chip model-chip-primary" data-chip="model">
        <button
          ref={trigger}
          type="button"
          class="chip-trigger narrow-header-action header-action"
          aria-haspopup="dialog"
          aria-controls={popoverID}
          aria-expanded={open}
          aria-label={`Runtime settings: ${displayModel}${effort ? `, ${effort} effort` : ''}`}
          data-effort-level={runtimePending ? 'auto' : effort || 'auto'}
          aria-busy={runtimePending || undefined}
          title={
            locked
              ? `${displayModel} · view runtime and queue reasoning effort for the next model turn`
              : `${displayModel} · choose provider, model, and reasoning effort`
          }
          onClick={() => (open ? close() : setOpen(true))}
        >
          <span class={`chip-label ${!selectedModel && fallbackModel ? 'stats-muted' : ''}`}>
            {displayModel}
          </span>
          <EffortMeter />
        </button>
      </div>
    </div>
  );
  const overlay = open ? (
    <dialog
      ref={popover}
      id={popoverID}
      class="chip-popover chip-popover-runtime"
      aria-label="Runtime settings"
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
          <div class="runtime-popover-title">Runtime</div>
          <div class="runtime-popover-hint">
            Provider, model, effort, speed, and MCP servers for the next reply
          </div>
        </div>
        <button
          ref={initialFocus}
          type="button"
          class="runtime-popover-close close-button"
          aria-label="Close runtime settings"
          autoFocus
          onClick={close}
        >
          <Icon name="close" />
        </button>
      </div>
      <div class="runtime-popover-fields">
        <label class="runtime-popover-field">
          <span class="runtime-popover-label">Provider</span>
          <select
            class="runtime-popover-select"
            aria-label="Provider"
            disabled={locked}
            value={store.selectedProvider.value}
            onChange={(event) => store.setPreference('provider', event.currentTarget.value)}
          >
            <option value="">Auto (server default)</option>
            {store.providers.value.map((entry) => (
              <option key={entry.id} value={entry.id}>
                {entry.name}
                {entry.is_default ? ' (default)' : ''}
              </option>
            ))}
          </select>
        </label>
        <label class="runtime-popover-field">
          <span class="runtime-popover-label">Model</span>
          <select
            class="runtime-popover-select"
            aria-label="Runtime model"
            disabled={locked}
            value={store.selectedModel.value}
            onChange={(event) => store.setPreference('model', event.currentTarget.value)}
          >
            <option value="">Auto (server default)</option>
            {store.models.value.map((entry) => (
              <option key={entry.id} value={entry.id}>
                {compactModelLabel(entry.name)}
              </option>
            ))}
          </select>
        </label>
        <label class="runtime-popover-field">
          <span class="runtime-popover-label">Effort</span>
          <select
            class="runtime-popover-select"
            aria-label="Reasoning effort"
            value={store.selectedEffort.value}
            onChange={(event) => store.setPreference('effort', event.currentTarget.value)}
          >
            {efforts.map((entry) => (
              <option key={entry || 'auto'} value={entry}>
                {entry || 'Auto (server default)'}
              </option>
            ))}
          </select>
        </label>
        {fastSupported && !providerFast && (
          <div class="runtime-popover-fast-row">
            <div>
              <div class="runtime-popover-label">Fast</div>
              <div id={`${popoverID}-fast-hint`} class="runtime-popover-fast-hint">
                Use priority processing for this model
              </div>
            </div>
            <button
              type="button"
              role="switch"
              class="runtime-popover-fast-toggle"
              aria-label="Fast mode"
              aria-describedby={`${popoverID}-fast-hint`}
              aria-checked={fast}
              onClick={() => store.setFast(!fast)}
            >
              <span aria-hidden="true" />
            </button>
          </div>
        )}
        <div class="runtime-mcp-row runtime-popover-fast-row">
          <div>
            <div class="runtime-popover-label">MCP servers</div>
            <div class="runtime-popover-fast-hint">{mcpHint}</div>
          </div>
          <button
            type="button"
            class="runtime-mcp-manage"
            aria-label="Manage MCP servers"
            onClick={() => {
              close();
              store.modal.value = 'mcp';
              void store.loadMCP();
            }}
          >
            {mcpCount ? `${mcpCount} enabled` : 'Manage'}
            <Icon name="chevron-right" />
          </button>
        </div>
      </div>
    </dialog>
  ) : null;
  return (
    <>
      {picker}
      {overlay}
    </>
  );
}

export function Header() {
  const store = useStore();
  const header = useRef<HTMLElement>(null);
  // Keep focus restoration for keyboard navigation, but do not leave any header action
  // focused after a pointer/touch interaction closes its surface.
  useEffect(() => {
    let blurFrame = 0;
    const blurFocusedAction = () => {
      cancelAnimationFrame(blurFrame);
      blurFrame = requestAnimationFrame(() => {
        const active = document.activeElement;
        if (
          active instanceof HTMLButtonElement &&
          header.current?.contains(active) &&
          active.matches('.header-action, .mobile-menu, .header-crumb, .header-title-badge')
        )
          active.blur();
      });
    };
    const blurAfterPointerClick = (event: MouseEvent) => {
      if (event.detail > 0) blurFocusedAction();
    };
    addEventListener('pointerup', blurFocusedAction, true);
    addEventListener('click', blurAfterPointerClick, true);
    return () => {
      cancelAnimationFrame(blurFrame);
      removeEventListener('pointerup', blurFocusedAction, true);
      removeEventListener('click', blurAfterPointerClick, true);
    };
  }, []);
  const session = store.activeSession.value;
  const resumableShell = Boolean(
    session &&
    store.shellStore.enabled.value &&
    !store.shellStore.visible.value &&
    store.shellStore.shellId.value &&
    store.shellStore.sessionId.value === session.id &&
    store.shellStore.status.value !== 'idle',
  );
  const loadedDiff =
    session && store.diff.value.sessionId === session.id && store.diff.value.files.length
      ? store.diff.value.files
      : null;
  const summary = session?.fileChangeSummary;
  const diffFileCount = loadedDiff?.length ?? summary?.fileCount ?? 0;
  const diffAdds =
    loadedDiff?.reduce((sum, file) => sum + (file.additions || 0), 0) ?? summary?.additions ?? 0;
  const diffDels =
    loadedDiff?.reduce((sum, file) => sum + (file.deletions || 0), 0) ?? summary?.deletions ?? 0;
  const showDiff = Boolean(
    session &&
    (diffFileCount > 0 ||
      summary?.git ||
      store.worktreesAvailable() ||
      store.worktreesEnabled.value),
  );
  const diffTitle = `${diffFileCount} changed ${diffFileCount === 1 ? 'file' : 'files'}${diffAdds || diffDels ? ` (${diffAdds ? `+${diffAdds}` : ''}${diffAdds && diffDels ? ' ' : ''}${diffDels ? `−${diffDels}` : ''})` : ''}`;
  const projectContext = resolveProjectContext(
    store.projectsEnabled.value,
    store.projects.value,
    session,
    store.activeProjectId.value,
  );
  const currentPlan = store.currentPlan.value;
  const currentPlanSummary = planSummary(currentPlan);
  const planStatus = currentPlanSummary.complete
    ? `All ${currentPlanSummary.total} steps complete`
    : `Step ${currentPlanSummary.position} of ${currentPlanSummary.total}, ${currentPlanSummary.completed} of ${currentPlanSummary.total} complete`;
  return (
    <header ref={header} class="main-header" tabIndex={-1}>
      <div class="header-title-row">
        <div class="header-left">
          <button
            class="icon-btn mobile-menu"
            id="mobileMenuBtn"
            aria-label="Open sidebar"
            aria-expanded={store.sidebarOpen.value}
            aria-controls="sidebar"
            onClick={() => {
              store.sidebarOpen.value = true;
            }}
          >
            <Icon name="menu" />
          </button>
          <HeaderBreadcrumb context={projectContext} />
          {store.networkState.value !== 'online' && (
            <span
              class={`connection-state ${store.networkState.value === 'offline' ? 'bad' : ''}`}
              id="connectionState"
              aria-live="polite"
            >
              {store.networkState.value === 'retrying' ? 'Reconnecting…' : store.networkState.value}
            </span>
          )}
        </div>
        {store.selectionStore.headerLoading.value ? (
          <div
            class="header-controls-row"
            role="status"
            aria-label="Loading session controls"
            aria-busy="true"
          />
        ) : (
          <div class="header-controls-row">
            <div class="header-stats" id="headerStats">
              <RuntimePicker />
            </div>
            <ActivityMeter />
            <ContextMeter />
            <div class="header-context-actions">
              {resumableShell && session && (
                <button
                  type="button"
                  class={`header-action shell-return shell-return-${store.shellStore.status.value}`}
                  aria-label="Return to shell"
                  title="Return to shell"
                  onClick={() => store.shellStore.show(session.id)}
                >
                  <span class="shell-return-dot" aria-hidden="true" />
                  <Icon class="shell-return-icon" name="terminal" />
                  <span class="shell-return-label">Shell</span>
                </button>
              )}
              {currentPlan && (
                <button
                  class={`header-action plan-toggle ${currentPlanSummary.complete ? 'complete' : ''}`}
                  id="planToggleBtn"
                  type="button"
                  aria-expanded={store.planVisible.value}
                  aria-controls="planSurface"
                  aria-label={`${store.planVisible.value ? 'Close' : 'Open'} current plan. ${planStatus}`}
                  title={planStatus}
                  onClick={() => {
                    if (store.planVisible.value) store.closePlan();
                    else store.openPlan();
                  }}
                >
                  {currentPlanSummary.complete ? (
                    <Icon class="plan-toggle-check" name="check" />
                  ) : (
                    <>
                      <span class="plan-toggle-word">Plan</span>
                      <span class="plan-toggle-progress">
                        {currentPlanSummary.position}/{currentPlanSummary.total}
                      </span>
                    </>
                  )}
                </button>
              )}
            </div>
            {showDiff && (
              <button
                class={`icon-btn diff-toggle header-action ${store.diff.value.open ? 'active' : ''}`}
                id="diffToggleBtn"
                type="button"
                aria-label={`Toggle file changes: ${diffTitle}`}
                aria-expanded={store.diff.value.open}
                aria-controls="diffSidebar"
                title={diffTitle}
                onClick={() => void store.toggleDiff()}
              >
                <span class={`diff-toggle-badge ${!diffAdds && !diffDels ? 'no-stats' : ''}`}>
                  <span class="diff-toggle-file-count">
                    <span class="diff-toggle-file-icon" aria-hidden="true" />
                  </span>
                  {!store.diff.value.open && (
                    <>
                      {diffAdds > 0 && <span class="diff-toggle-stat-add">+{diffAdds}</span>}
                      {diffDels > 0 && <span class="diff-toggle-stat-del">−{diffDels}</span>}
                    </>
                  )}
                </span>
              </button>
            )}
          </div>
        )}
      </div>
    </header>
  );
}
