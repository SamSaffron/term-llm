import { useLayoutEffect, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import {
  compactModelLabel,
  defaultModel,
  defaultProvider,
  splitModelEffort,
  supportedEfforts,
} from '../domain/runtime';
import { planSummary } from '../domain/plan';
import { Icon } from './Icon';

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
  const trigger = useRef<HTMLButtonElement>(null);
  const popover = useRef<HTMLDialogElement>(null);
  const locked = store.streaming.value;
  const selectedProvider = locked
    ? store.activeSession.value?.activeProvider || store.selectedProvider.value
    : store.selectedProvider.value;
  const provider =
    store.providers.value.find((entry) => entry.id === selectedProvider) ||
    defaultProvider(store.providers.value);
  const selectedModel = locked
    ? store.activeSession.value?.activeModel || store.selectedModel.value
    : store.selectedModel.value;
  const fallbackModel = defaultModel(provider);
  const split = splitModelEffort(
    selectedModel || fallbackModel,
    store.selectedEffort.value || store.activeSession.value?.activeEffort || '',
  );
  const model = store.models.value.find((entry) => entry.id === split.model);
  const efforts = supportedEfforts(model);
  const effort = split.effort && efforts.includes(split.effort) ? split.effort : '';
  useLayoutEffect(() => {
    if (!open || !trigger.current || !popover.current) return;
    const rect = trigger.current.getBoundingClientRect();
    const panel = popover.current;
    const margin = 6;
    if (!panel.open) panel.showModal();
    if (innerWidth <= 540) {
      panel.style.left = 'calc(0.5rem + var(--safe-left))';
      panel.style.top = 'auto';
      panel.style.bottom = 'calc(0.5rem + var(--safe-bottom))';
      return;
    }
    const panelRect = panel.getBoundingClientRect();
    panel.style.left = `${Math.max(margin, Math.min(rect.left, innerWidth - panelRect.width - margin))}px`;
    const below = rect.bottom + 4;
    panel.style.top = `${below + panelRect.height <= innerHeight - margin ? below : Math.max(margin, rect.top - panelRect.height - 4)}px`;
    panel.style.bottom = 'auto';
  }, [open]);
  const picker = (
    <div class={`model-picker ${locked ? 'locked' : ''}`}>
      <div class="model-chip model-chip-primary" data-chip="model">
        <button
          ref={trigger}
          type="button"
          class="chip-trigger narrow-header-action header-action"
          aria-haspopup="dialog"
          aria-expanded={open}
          aria-label="Runtime settings"
          data-effort-level={effort || 'auto'}
          title={
            locked
              ? 'View runtime and queue reasoning effort for the next model turn'
              : 'Choose provider, model, and reasoning effort'
          }
          onClick={() => setOpen((value) => !value)}
        >
          <span class={`chip-label ${!selectedModel && fallbackModel ? 'stats-muted' : ''}`}>
            {compactModelLabel(split.model) || 'Auto'}
          </span>
          <EffortMeter />
        </button>
      </div>
    </div>
  );
  const overlay = open ? (
    <dialog
      ref={popover}
      class="chip-popover chip-popover-runtime"
      aria-label="Runtime settings"
      onCancel={(event) => {
        event.preventDefault();
        setOpen(false);
        trigger.current?.focus();
      }}
      onClick={(event) => {
        if (event.target === event.currentTarget) setOpen(false);
      }}
    >
      <div class="runtime-popover-header">
        <div class="runtime-popover-title">Runtime</div>
        <div class="runtime-popover-hint">Provider, model, and effort for the next reply</div>
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
  const session = store.activeSession.value;
  const tokenCount = Number(session?.usage?.total_tokens || 0);
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
  const showDiff = Boolean(session && (diffFileCount > 0 || summary?.git));
  const diffTitle = `${diffFileCount} changed ${diffFileCount === 1 ? 'file' : 'files'}${diffAdds || diffDels ? ` (${diffAdds ? `+${diffAdds}` : ''}${diffAdds && diffDels ? ' ' : ''}${diffDels ? `−${diffDels}` : ''})` : ''}`;
  const project = store.projects.value.find(
    (entry) => entry.id === (session?.projectId || store.activeProjectId.value),
  );
  const showWorktree = store.worktreesAvailable();
  const currentPlan = store.currentPlan.value;
  const currentPlanSummary = planSummary(currentPlan);
  const planUnseen =
    Boolean(currentPlan) &&
    !store.planVisible.value &&
    store.planSeen.value !== null &&
    store.planSeen.value !== currentPlanSummary.signature;
  const planStatus = currentPlanSummary.complete
    ? `All ${currentPlanSummary.total} steps complete`
    : `Step ${currentPlanSummary.position} of ${currentPlanSummary.total}, ${currentPlanSummary.completed} of ${currentPlanSummary.total} complete`;
  return (
    <header class="main-header" tabIndex={-1}>
      <div class="header-title-row">
        <div class="header-left">
          <button
            class="icon-btn mobile-menu"
            id="mobileMenuBtn"
            aria-label="Open sidebar"
            onClick={() => {
              store.sidebarOpen.value = true;
            }}
          >
            <Icon name="menu" />
          </button>
          <div class="header-title-context">
            <h1 class="header-title" id="activeSessionTitle">
              {session?.title || 'Chat'}
            </h1>
            {(session?.projectName || project?.name) && (
              <span class="header-project-subtitle" id="activeProjectSubtitle">
                {session?.projectName || project?.name}
              </span>
            )}
          </div>
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
        <div class="header-controls-row">
          <div class="header-stats" id="headerStats">
            <RuntimePicker />
            {(session?.mcpEnabled?.length || 0) > 0 && (
              <button
                type="button"
                class="mcp-status header-action"
                id="mcpStatus"
                aria-label="Manage MCP servers"
                onClick={() => {
                  store.modal.value = 'mcp';
                  void store.loadMCP();
                }}
              >
                MCP {session?.mcpEnabled?.length}
              </button>
            )}
            {tokenCount > 0 && (
              <>
                <span class="chip-sep header-tokens-sep" id="headerTokensSep">
                  ·
                </span>
                <span class="header-tokens" id="headerTokens">
                  {tokenCount.toLocaleString()} tokens
                </span>
              </>
            )}
          </div>
          <div class="header-context-actions">
            {showWorktree && (
              <button
                type="button"
                class={`chip-trigger worktree-trigger ${session && !store.draftActive.value ? 'locked' : ''}`}
                id="chipWorktreeTrigger"
                aria-label="Worktree"
                onClick={() => {
                  store.modal.value = 'worktrees';
                  void store.loadWorktrees();
                }}
              >
                <Icon name="branch" />
                <span class="chip-label">
                  {(session?.worktreeDir || store.selectedDraftWorktree.value).split('/').pop() ||
                    'root'}
                </span>
              </button>
            )}
            {session && store.branchPathCount.value > 1 && (
              <button
                class="header-action branch-tree-trigger"
                id="branchTreeBtn"
                type="button"
                aria-haspopup="dialog"
                onClick={() => void store.loadBranchTree()}
              >
                {store.branchPathCount.value} paths
              </button>
            )}
            {currentPlan && (
              <button
                class={`header-action plan-toggle ${currentPlanSummary.complete ? 'complete' : ''} ${planUnseen ? 'updated' : ''}`}
                id="planToggleBtn"
                type="button"
                aria-expanded={store.planVisible.value}
                aria-controls="planSurface"
                aria-label={`${store.planVisible.value ? 'Close' : 'Open'} current plan. ${planStatus}${planUnseen ? '. Updated' : ''}`}
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
                {planUnseen && <span class="plan-unseen-dot" aria-hidden="true" />}
              </button>
            )}
          </div>
          {showDiff && (
            <button
              class={`icon-btn diff-toggle header-action ${store.diff.value.open ? 'active' : ''}`}
              id="diffToggleBtn"
              type="button"
              aria-label={`Toggle file changes: ${diffTitle}`}
              title={diffTitle}
              onClick={() => void store.toggleDiff()}
            >
              <span class={`diff-toggle-badge ${!diffAdds && !diffDels ? 'no-stats' : ''}`}>
                <span class="diff-toggle-file-count">
                  <span class="diff-toggle-file-icon" aria-hidden="true" />
                </span>
                {diffAdds > 0 && <span class="diff-toggle-stat-add">+{diffAdds}</span>}
                {diffDels > 0 && <span class="diff-toggle-stat-del">−{diffDels}</span>}
              </span>
            </button>
          )}
        </div>
      </div>
    </header>
  );
}
