import { useId, useRef, useState } from 'preact/hooks';
import { Menu } from '../../components/Menu';
import type { HubConfig } from '../config';
import { NODE_FILTERS, type NodeFilter } from '../domain/node-filter';
import type { HubStore } from '../stores/hub-store';

const FILTER_LABELS: Record<NodeFilter, string> = {
  online: 'Online',
  offline: 'Offline',
  all: 'All nodes',
};
const FILTER_DOTS: Record<NodeFilter, string> = { online: 'ok', offline: 'down', all: '' };

const plural = (count: number, word: string) => `${count} ${word}${count === 1 ? '' : 's'}`;

/**
 * The node count, which opens a menu choosing which nodes the grid shows:
 * online (the default), offline, or all.
 */
function NodeFilterMenu({ store }: { store: HubStore }) {
  const [open, setOpen] = useState(false);
  const trigger = useRef<HTMLButtonElement>(null);
  const menuID = useId();
  const total = store.nodes.value.length;
  if (!total) return null;
  const verified = store.nodesVerified.value;
  const online = store.onlineNodeCount.value;
  const counts: Record<NodeFilter, number> = { online, offline: total - online, all: total };
  const filter = store.nodeFilter.value;
  const [count, context] =
    filter === 'all'
      ? [plural(total, 'node'), `${online} online`]
      : [`${counts[filter]} ${filter}`, `of ${total}`];
  const active = store.activeSessionCount.value;
  const choose = (next: NodeFilter) => {
    store.nodeFilter.value = next;
    setOpen(false);
  };
  return (
    <div class="hub-filter">
      <button
        ref={trigger}
        class="hub-btn ghost hub-filter-toggle"
        type="button"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? menuID : undefined}
        aria-label={`Show nodes: ${count} ${context}`}
        title="Choose which nodes to show"
        onClick={() => setOpen((value) => !value)}
      >
        <span class={`status-dot ${verified ? FILTER_DOTS[filter] : 'last-known'}`} />
        <span>{count}</span>
        <span class="hub-filter-context">{context}</span>
        <svg
          class="hub-filter-chevron"
          width="12"
          height="12"
          viewBox="0 0 12 12"
          aria-hidden="true"
        >
          <path d="M3 4.5 6 7.5 9 4.5" fill="none" stroke="currentColor" stroke-width="1.6" />
        </svg>
      </button>
      <Menu
        id={menuID}
        open={open}
        label="Show nodes"
        onClose={() => setOpen(false)}
        triggerRef={trigger}
        className="hub-filter-menu"
      >
        {NODE_FILTERS.map((option) => (
          <button
            key={option}
            class="hub-filter-item"
            type="button"
            role="menuitemradio"
            aria-checked={option === filter}
            onClick={() => choose(option)}
          >
            <span class="hub-filter-check" aria-hidden="true">
              {option === filter ? '✓' : ''}
            </span>
            <span class={`status-dot ${FILTER_DOTS[option]}`} />
            <span class="hub-filter-label">{FILTER_LABELS[option]}</span>
            <span class="hub-filter-count">{counts[option]}</span>
          </button>
        ))}
        <div class="hub-filter-hint" role="none">
          {!verified
            ? 'Last known · updating'
            : active
              ? plural(active, 'active session')
              : 'No active sessions'}
        </div>
      </Menu>
    </div>
  );
}

export function HubHeader({ config, store }: { config: HubConfig; store: HubStore }) {
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
          {store.lastKnown.value && <span class="hub-cache-marker">last known · updating</span>}
        </span>
        <NodeFilterMenu store={store} />
        {config.passkeyAuth && (
          <button
            class="hub-btn ghost"
            type="button"
            aria-haspopup="dialog"
            aria-expanded={store.securityOpen.value}
            aria-controls="hub-security-dialog"
            onClick={() => void store.openSecurity()}
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
