import type { HubConfig } from '../config';
import type { HubStore } from '../stores/hub-store';

export function HubHeader({ config, store }: { config: HubConfig; store: HubStore }) {
  const nodes = store.nodes.value.length;
  const active = store.activeSessionCount.value;
  const summary = nodes
    ? `${store.reachableCount.value}/${nodes} nodes reachable${active ? ` · ${active} active ${active === 1 ? 'session' : 'sessions'}` : ''}`
    : '';
  return (
    <header class="hub-header">
      <div class="hub-brand">
        <svg
          class="hub-logo"
          width="28"
          height="28"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
          stroke-linecap="round"
          stroke-linejoin="round"
          aria-hidden="true"
        >
          <circle cx="12" cy="12" r="3" />
          <circle cx="4.5" cy="5" r="2" />
          <circle cx="19.5" cy="5" r="2" />
          <circle cx="4.5" cy="19" r="2" />
          <circle cx="19.5" cy="19" r="2" />
          <line x1="6.2" y1="6.4" x2="9.8" y2="10" />
          <line x1="17.8" y1="6.4" x2="14.2" y2="10" />
          <line x1="6.2" y1="17.6" x2="9.8" y2="14" />
          <line x1="17.8" y1="17.6" x2="14.2" y2="14" />
        </svg>
        <h1>
          term-llm <span class="hub-brand-accent">Hub</span>
        </h1>
      </div>
      <div class="hub-header-actions">
        <span class="hub-summary" aria-live="polite">
          {summary}
        </span>
        {config.passkeyAuth && (
          <button
            class="hub-btn ghost"
            type="button"
            aria-expanded={store.securityOpen.value}
            aria-controls="hub-security-panel"
            onClick={() => void store.toggleSecurity()}
          >
            Security
          </button>
        )}
        <button
          class="hub-btn ghost"
          type="button"
          title="Refresh nodes"
          disabled={store.refreshing.value}
          onClick={() => void store.refresh('manual')}
        >
          {store.refreshing.value ? 'Refreshing…' : 'Refresh'}
        </button>
        {config.canAddNodes && (
          <button class="hub-btn primary" type="button" onClick={() => store.openAddDialog()}>
            Add node
          </button>
        )}
      </div>
    </header>
  );
}
