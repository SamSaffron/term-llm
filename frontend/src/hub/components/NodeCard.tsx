import { memo } from '../../components/memo';
import { useRef, useState } from 'preact/hooks';
import { Menu } from '../../components/Menu';
import { nodeResumePath } from '../domain/formatting';
import type { HubNode } from '../domain/types';
import type { HubStore } from '../stores/hub-store';
import { NodeSessions } from './NodeSessions';

function RemoveNodeAction({ store, remove }: { store: HubStore; remove: () => Promise<void> }) {
  return (
    <button
      class="node-menu-item danger"
      type="button"
      role="menuitem"
      disabled={store.nodeOperation.value !== 'idle'}
      onClick={() => void remove()}
    >
      Remove node
    </button>
  );
}

export const NodeCard = memo(function NodeCard({
  node,
  store,
}: {
  node: HubNode;
  store: HubStore;
}) {
  const [menuOpen, setMenuOpen] = useState(false);
  const menuTrigger = useRef<HTMLButtonElement>(null);
  const status = node.status ?? { reachable: false, state: 'unknown', latency_ms: 0 };
  const summary = [
    status.agent || node.id,
    status.version,
    status.state && !status.reachable ? status.state : '',
  ].filter(Boolean);
  const sessions = node.sessions;
  const resumePath = nodeResumePath(node);
  const attention =
    sessions &&
    (Number(sessions.active_count) > 0 ||
      Number(sessions.input_required_count) > 0 ||
      Number(sessions.unseen_count) > 0);

  const remove = async () => {
    setMenuOpen(false);
    if (!window.confirm(`Remove node "${node.name}"?`)) return;
    await store.removeNode(node.id);
  };

  return (
    <article class="node-card">
      <div class="node-card-head">
        <span
          class={`status-dot ${status.reachable ? 'ok' : 'down'}`}
          title={status.reachable ? 'Reachable' : status.error || 'Unreachable'}
        />
        <h2 class="node-name">{node.name}</h2>
        {attention && (
          <span
            class="attention-dot"
            title={`${Number(sessions.input_required_count) || 0} waiting · ${Number(sessions.active_count) || 0} running · ${Number(sessions.unseen_count) || 0} ready to review`}
          />
        )}
        {sessions?.count_label && <span class="session-count-badge">{sessions.count_label}</span>}
      </div>
      {summary.length > 0 && <div class="node-summary-line">{summary.join(' · ')}</div>}
      {(status.capabilities?.length ?? 0) > 0 && (
        <div class="node-caps">
          {status.capabilities?.map((capability) => (
            <span class="cap-chip" key={capability}>
              {capability}
            </span>
          ))}
        </div>
      )}
      <NodeSessions node={node} />
      {!status.reachable && status.error && <div class="node-error">{status.error}</div>}
      {(node.diagnostics?.length ?? 0) > 0 && (
        <div class="node-diagnostics">
          {node.diagnostics?.map((diagnostic) => (
            <div
              key={`${diagnostic.code}:${diagnostic.message}`}
              class={`node-diagnostic diagnostic-${diagnostic.severity || 'warning'}`}
            >
              <span class="diagnostic-label">
                {(diagnostic.severity || 'warning').toUpperCase()}
              </span>
              <span class="diagnostic-message">
                {diagnostic.message || diagnostic.code || 'Node diagnostic'}
              </span>
            </div>
          ))}
        </div>
      )}
      <div class="node-actions">
        {resumePath ? (
          <a class="hub-btn primary" href={resumePath}>
            Resume
          </a>
        ) : (
          <span class="hub-btn primary disabled" aria-disabled="true">
            Resume
          </span>
        )}
        {node.new_session_path || node.proxy_path ? (
          <a class="hub-btn ghost" href={node.new_session_path || `${node.proxy_path}?new=1`}>
            New
          </a>
        ) : (
          <span class="hub-btn ghost disabled" aria-disabled="true">
            New
          </span>
        )}
        {node.source === 'local' && (
          <div class="node-menu">
            <button
              ref={menuTrigger}
              class="node-menu-toggle"
              type="button"
              aria-label={`More actions for ${node.name}`}
              aria-haspopup="menu"
              aria-expanded={menuOpen}
              onClick={() => setMenuOpen((open) => !open)}
            >
              ⋯
            </button>
            <Menu
              open={menuOpen}
              label={`Actions for ${node.name}`}
              onClose={() => setMenuOpen(false)}
              triggerRef={menuTrigger}
              className="node-menu-list"
            >
              <RemoveNodeAction store={store} remove={remove} />
            </Menu>
          </div>
        )}
      </div>
    </article>
  );
});

export function NodeGrid({ store }: { store: HubStore }) {
  return (
    <section class="node-grid" aria-label="Nodes" aria-busy={store.initialLoading.value}>
      {store.nodes.value.map((node) => (
        <NodeCard key={node.id} node={node} store={store} />
      ))}
    </section>
  );
}
