import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { compactModelLabel, defaultModel, defaultProvider, splitModelEffort, supportedEfforts } from '../domain/runtime';
import { Icon } from './Icon';

function EffortMeter() {
  return <svg class="effort-meter" viewBox="0 0 8 12" aria-hidden="true" focusable="false">
    <rect class="effort-meter-bar effort-meter-bar-1" x="0" y="7" width="1.4" height="5" rx="0.7" />
    <rect class="effort-meter-bar effort-meter-bar-2" x="2.2" y="5" width="1.4" height="7" rx="0.7" />
    <rect class="effort-meter-bar effort-meter-bar-3" x="4.4" y="3" width="1.4" height="9" rx="0.7" />
    <rect class="effort-meter-bar effort-meter-bar-4" x="6.6" y="1" width="1.4" height="11" rx="0.7" />
  </svg>;
}

function RuntimePicker() {
  const store = useStore(); const [open, setOpen] = useState(false); const trigger = useRef<HTMLButtonElement>(null); const popover = useRef<HTMLDivElement>(null);
  const locked = store.streaming.value; const selectedProvider = locked ? (store.activeSession.value?.activeProvider || store.selectedProvider.value) : store.selectedProvider.value;
  const provider = store.providers.value.find((entry) => entry.id === selectedProvider) || defaultProvider(store.providers.value);
  const selectedModel = locked ? (store.activeSession.value?.activeModel || store.selectedModel.value) : store.selectedModel.value;
  const fallbackModel = defaultModel(provider); const split = splitModelEffort(selectedModel || fallbackModel, store.selectedEffort.value || store.activeSession.value?.activeEffort || '');
  const model = store.models.value.find((entry) => entry.id === split.model); const efforts = supportedEfforts(model);
  const effort = split.effort && efforts.includes(split.effort) ? split.effort : '';
  useEffect(() => {
    if (!open) return;
    const close = (event: KeyboardEvent) => { if (event.key === 'Escape') { event.preventDefault(); setOpen(false); trigger.current?.focus(); } };
    addEventListener('keydown', close); return () => removeEventListener('keydown', close);
  }, [open]);
  useLayoutEffect(() => {
    if (!open || !trigger.current || !popover.current) return;
    const rect = trigger.current.getBoundingClientRect(); const panel = popover.current; const margin = 6;
    if (innerWidth <= 540) { panel.style.left = 'calc(0.5rem + var(--safe-left))'; panel.style.top = 'auto'; panel.style.bottom = 'calc(0.5rem + var(--safe-bottom))'; return; }
    const panelRect = panel.getBoundingClientRect(); panel.style.left = `${Math.max(margin, Math.min(rect.left, innerWidth - panelRect.width - margin))}px`;
    const below = rect.bottom + 4; panel.style.top = `${below + panelRect.height <= innerHeight - margin ? below : Math.max(margin, rect.top - panelRect.height - 4)}px`; panel.style.bottom = 'auto';
  }, [open]);
  return <div class={`model-picker ${locked ? 'locked' : ''}`} id="modelPicker" onKeyDown={(event) => { if (open && event.key === 'Escape') { event.preventDefault(); setOpen(false); trigger.current?.focus(); } }}>
    <div class="model-chip model-chip-primary" data-chip="model" id="chipModel">
      <button ref={trigger} type="button" class="chip-trigger narrow-header-action header-action" id="chipModelTrigger" aria-haspopup="dialog" aria-expanded={open} aria-label="Runtime settings" data-effort-level={effort || 'auto'} title={locked ? 'View runtime and queue reasoning effort for the next model turn' : 'Choose provider, model, and reasoning effort'} onClick={() => setOpen((value) => !value)}>
        <span class={`chip-label ${!selectedModel && fallbackModel ? 'stats-muted' : ''}`} id="chipModelLabel">{compactModelLabel(split.model) || 'Auto'}</span><EffortMeter />
      </button>
    </div>
    {open && <><button type="button" class="chip-popover-backdrop" aria-label="Close runtime settings" onClick={() => setOpen(false)} /><div ref={popover} class="chip-popover chip-popover-runtime" role="dialog" aria-label="Runtime settings">
      <div class="runtime-popover-header"><div class="runtime-popover-title">Runtime</div><div class="runtime-popover-hint">Provider, model, and effort for the next reply</div></div>
      <div class="runtime-popover-fields">
        <label class="runtime-popover-field"><span class="runtime-popover-label">Provider</span><select class="runtime-popover-select" aria-label="Provider" disabled={locked} value={store.selectedProvider.value} onChange={(event) => store.setPreference('provider', event.currentTarget.value)}><option value="">Auto (server default)</option>{store.providers.value.map((entry) => <option key={entry.id} value={entry.id}>{entry.name}{entry.is_default ? ' (default)' : ''}</option>)}</select></label>
        <label class="runtime-popover-field"><span class="runtime-popover-label">Model</span><select class="runtime-popover-select" aria-label="Runtime model" disabled={locked} value={store.selectedModel.value} onChange={(event) => store.setPreference('model', event.currentTarget.value)}><option value="">Auto (server default)</option>{store.models.value.map((entry) => <option key={entry.id} value={entry.id}>{compactModelLabel(entry.name)}</option>)}</select></label>
        <label class="runtime-popover-field"><span class="runtime-popover-label">Effort</span><select class="runtime-popover-select" aria-label="Reasoning effort" value={store.selectedEffort.value} onChange={(event) => store.setPreference('effort', event.currentTarget.value)}>{efforts.map((entry) => <option key={entry || 'auto'} value={entry}>{entry || 'Auto (server default)'}</option>)}</select></label>
      </div>
    </div></>}
  </div>;
}

export function Header() {
  const store = useStore(); const session = store.activeSession.value;
  const tokenCount = Number(session?.usage?.total_tokens || 0);
  const project = store.projects.value.find((entry) => entry.id === (session?.projectId || store.activeProjectId.value));
  const showWorktree = store.worktreesEnabled.value && (!store.projectsEnabled.value || Boolean(project?.git && project.available !== false));
  return <header class="main-header" tabIndex={-1}>
    <div class="header-title-row">
      <div class="header-left">
        <button class="icon-btn mobile-menu" id="mobileMenuBtn" aria-label="Open sidebar" onClick={() => { store.sidebarOpen.value = true; }}><Icon name="menu" /></button>
        <div class="header-title-context"><h1 class="header-title" id="activeSessionTitle">{session?.title || 'Chat'}</h1>{(session?.projectName || project?.name) && <span class="header-project-subtitle" id="activeProjectSubtitle">{session?.projectName || project?.name}</span>}</div>
        {store.networkState.value !== 'online' && <span class={`connection-state ${store.networkState.value === 'offline' ? 'bad' : ''}`} id="connectionState" aria-live="polite">{store.networkState.value === 'retrying' ? 'Reconnecting…' : store.networkState.value}</span>}
      </div>
      <div class="header-controls-row">
        <div class="header-stats" id="headerStats"><RuntimePicker />
        {(session?.mcpEnabled?.length || 0) > 0 && <button type="button" class="mcp-status header-action" id="mcpStatus" aria-label="Manage MCP servers" onClick={() => { store.modal.value = 'mcp'; void store.loadMCP(); }}>MCP {session?.mcpEnabled?.length}</button>}
        {tokenCount > 0 && <><span class="chip-sep header-tokens-sep" id="headerTokensSep">·</span><span class="header-tokens" id="headerTokens">{tokenCount.toLocaleString()} tokens</span></>}
        </div>
        <div class="header-context-actions">
          {showWorktree && <button type="button" class={`chip-trigger worktree-trigger ${session && !store.draftActive.value ? 'locked' : ''}`} id="chipWorktreeTrigger" aria-label="Worktree" onClick={() => { store.modal.value = 'worktrees'; void store.loadWorktrees(); }}><Icon name="branch" /><span class="chip-label">{(session?.worktreeDir || store.selectedDraftWorktree.value).split('/').pop() || 'root'}</span></button>}
          {session && store.branchPathCount.value > 1 && <button class="header-action branch-tree-trigger" id="branchTreeBtn" type="button" aria-haspopup="dialog" onClick={() => void store.loadBranchTree()}>{store.branchPathCount.value} paths</button>}
          {store.currentPlan.value && <button class="header-action plan-toggle" id="planToggleBtn" type="button" aria-expanded={store.planOpen.value} onClick={() => { store.diff.value = { ...store.diff.value, open: false }; store.planOpen.value = !store.planOpen.value; }}><span>Plan</span><span class="plan-toggle-progress">{store.currentPlan.value.plan.filter((step) => step.status === 'completed').length}/{store.currentPlan.value.plan.length}</span></button>}
        </div>
      </div>
      {session && <button class="icon-btn diff-toggle header-action" id="diffToggleBtn" type="button" aria-label="Toggle file changes" onClick={() => void store.toggleDiff()}><Icon name="diff" /><span class="diff-toggle-badge">{store.diff.value.files.reduce((sum, file) => sum + (file.additions || 0) + (file.deletions || 0), 0) || ''}</span></button>}
    </div>
  </header>;
}
