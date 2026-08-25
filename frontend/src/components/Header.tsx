import { useStore } from '../app/context';
import { compactModelLabel, supportedEfforts } from '../domain/runtime';
import { Icon } from './Icon';

export function Header() {
  const store = useStore(); const session = store.activeSession.value; const model = store.models.value.find((entry) => entry.id === store.selectedModel.value);
  const tokenCount = Number(session?.usage?.total_tokens || 0); const runtimeLocked = store.streaming.value; const efforts = supportedEfforts(model);
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
        <div class="header-stats" id="headerStats"><div class="model-picker" id="modelPicker">
          <div class="model-chip" data-chip="provider" id="chipProvider"><select class="chip-trigger" id="chipProviderSelect" aria-label="Provider" disabled={runtimeLocked} value={store.selectedProvider.value} onChange={(event) => store.setPreference('provider', event.currentTarget.value)}><option value="">Auto</option>{store.providers.value.map((provider) => <option key={provider.id} value={provider.id}>{provider.name}</option>)}</select></div>
          <span class="chip-sep" id="chipSepProviderModel">·</span>
          <div class="model-chip model-chip-primary" data-chip="model" id="chipModel"><select class="chip-trigger narrow-header-action header-action" id="chipModelSelect" aria-label="Runtime model" disabled={runtimeLocked} value={store.selectedModel.value} onChange={(event) => store.setPreference('model', event.currentTarget.value)}><option value="">Auto</option>{store.models.value.map((entry) => <option key={entry.id} value={entry.id}>{compactModelLabel(entry.name)}</option>)}</select></div>
          <span class="chip-sep" id="chipSepModelEffort">·</span>
          <div class="model-chip" data-chip="effort" id="chipEffort"><select class="chip-trigger" id="chipEffortSelect" aria-label="Reasoning effort" value={store.selectedEffort.value} onChange={(event) => store.setPreference('effort', event.currentTarget.value)}>{efforts.map((effort) => <option key={effort} value={effort}>{effort || 'auto'}</option>)}</select></div>
        </div>
        {(session?.mcpEnabled?.length || 0) > 0 && <button type="button" class="mcp-status header-action" id="mcpStatus" aria-label="Manage MCP servers" onClick={() => { store.modal.value = 'mcp'; void store.loadMCP(); }}>MCP {session?.mcpEnabled?.length}</button>}
        {tokenCount > 0 && <><span class="chip-sep header-tokens-sep" id="headerTokensSep">·</span><span class="header-tokens" id="headerTokens">{tokenCount.toLocaleString()} tokens</span></>}
        </div>
        <div class="header-context-actions">
          {showWorktree && <button type="button" class={`chip-trigger worktree-trigger ${session && !store.draftActive.value ? 'locked' : ''}`} id="chipWorktreeTrigger" aria-label="Worktree" onClick={() => { store.modal.value = 'worktrees'; void store.loadWorktrees(); }}><Icon name="branch" /><span class="chip-label">{(session?.worktreeDir || store.selectedDraftWorktree.value).split('/').pop() || 'root'}</span></button>}
          {session && <button class="header-action branch-tree-trigger" id="branchTreeBtn" type="button" aria-haspopup="dialog" onClick={() => void store.loadBranchTree()}>Paths</button>}
          {store.currentPlan.value && <button class="header-action plan-toggle" id="planToggleBtn" type="button" aria-expanded={store.planOpen.value} onClick={() => { store.diff.value = { ...store.diff.value, open: false }; store.planOpen.value = !store.planOpen.value; }}><span>Plan</span><span class="plan-toggle-progress">{store.currentPlan.value.plan.filter((step) => step.status === 'completed').length}/{store.currentPlan.value.plan.length}</span></button>}
        </div>
      </div>
      {session && <button class="icon-btn diff-toggle header-action" id="diffToggleBtn" type="button" aria-label="Toggle file changes" onClick={() => void store.toggleDiff()}><Icon name="diff" /><span class="diff-toggle-badge">{store.diff.value.files.reduce((sum, file) => sum + (file.additions || 0) + (file.deletions || 0), 0) || ''}</span></button>}
    </div>
  </header>;
}
