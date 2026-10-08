import { memo } from '../../components/memo';
import { useMemo, useRef, useState } from 'preact/hooks';
import { Menu } from '../../components/Menu';
import { reorderKeyOffset, useReorderableList } from '../../components/useReorderableList';
import { nodeResumePath } from '../domain/formatting';
import { mergeShownOrder } from '../domain/node-filter';
import type { HubNode } from '../domain/types';
import type { HubStore } from '../stores/hub-store';
import { NodeSessions } from './NodeSessions';
import { HubRenderBoundary } from './HubRenderBoundary';

/** Reordering callbacks shared, unchanged, by every card of the grid. */
interface NodeReorder {
  /** Tracks a press on a card's header, which drags the card once it moves (mouse) or rests (touch). */
  press: (event: PointerEvent, id: string) => void;
  /** Moves a card `offset` places; `focus` names the control that keeps focus. */
  move: (id: string, offset: number, focus: 'row' | 'menu') => void;
}

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
  position = 0,
  count = 1,
  dragging = false,
  reorder,
}: {
  node: HubNode;
  store: HubStore;
  /** Zero-based place in the grid of `count` cards. */
  position?: number;
  count?: number;
  /** Whether the card is lifted and following the pointer. */
  dragging?: boolean;
  reorder?: NodeReorder;
}) {
  const [menuOpen, setMenuOpen] = useState(false);
  const menuTrigger = useRef<HTMLButtonElement>(null);
  const reorderable = Boolean(reorder && count > 1);
  const stale = !store.nodesVerified.value;
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
  const keyShortcuts = reorderable ? 'Alt+ArrowUp Alt+ArrowDown' : undefined;

  const remove = async () => {
    setMenuOpen(false);
    if (!window.confirm(`Remove node "${node.name}"?`)) return;
    await store.removeNode(node.id);
  };
  const move = (offset: -1 | 1) => {
    setMenuOpen(false);
    reorder!.move(node.id, offset, 'menu');
  };

  return (
    <article
      class={`node-card ${reorderable ? 'is-reorderable' : ''} ${dragging ? 'is-dragging' : ''}`}
      data-reorder-id={reorder ? node.id : undefined}
      // Alt+ArrowUp/Down moves the card from any of its controls; focus stays
      // on that control. The open menu keeps its own arrow keys.
      onKeyDown={(event) => {
        const offset = reorderable ? reorderKeyOffset(event) : 0;
        if (!offset || (event.target as Element).closest('[role="menu"]')) return;
        event.preventDefault();
        reorder!.move(node.id, offset, 'row');
      }}
    >
      <div
        class="node-card-head"
        // The whole header drags the card: a mouse press once it travels, a
        // touch once it rests. The card's links, sessions, and menu below stay
        // ordinary controls.
        onPointerDown={reorderable ? (event) => reorder!.press(event, node.id) : undefined}
      >
        <span
          class={`status-dot ${stale ? 'last-known' : status.reachable ? 'ok' : 'down'}`}
          title={
            stale
              ? 'Last seen · updating'
              : status.reachable
                ? 'Reachable'
                : status.error || 'Unreachable'
          }
        />
        <h2 class="node-name">{node.name}</h2>
        {!stale && attention && (
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
      {stale && <div class="node-summary-line hub-cache-marker">Last seen · updating</div>}
      <NodeSessions node={node} verified={!stale} />
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
          <a class="hub-btn primary" href={resumePath} aria-keyshortcuts={keyShortcuts}>
            Resume
          </a>
        ) : (
          <span class="hub-btn primary disabled" aria-disabled="true">
            Resume
          </span>
        )}
        {node.new_session_path || node.proxy_path ? (
          <a
            class="hub-btn ghost"
            href={node.new_session_path || `${node.proxy_path}?new=1`}
            aria-keyshortcuts={keyShortcuts}
          >
            New
          </a>
        ) : (
          <span class="hub-btn ghost disabled" aria-disabled="true">
            New
          </span>
        )}
        {(node.source === 'local' || reorderable) && (
          <div class="node-menu">
            <button
              ref={menuTrigger}
              class="node-menu-toggle"
              type="button"
              aria-label={`More actions for ${node.name}`}
              aria-haspopup="menu"
              aria-expanded={menuOpen}
              aria-keyshortcuts={keyShortcuts}
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
              {reorderable && position > 0 && (
                <button
                  class="node-menu-item"
                  type="button"
                  role="menuitem"
                  onClick={() => move(-1)}
                >
                  Move earlier
                </button>
              )}
              {reorderable && position < count - 1 && (
                <button
                  class="node-menu-item"
                  type="button"
                  role="menuitem"
                  onClick={() => move(1)}
                >
                  Move later
                </button>
              )}
              {node.source === 'local' && <RemoveNodeAction store={store} remove={remove} />}
            </Menu>
          </div>
        )}
      </div>
    </article>
  );
});

/**
 * The node cards, in the Hub's saved order, which activity, names, and health
 * never change. Users drag a card by its header (pointer, or touch-and-hold),
 * or use Alt+ArrowUp/Down or the card menu, and the order is saved for every
 * browser and node. Cards slide aside to preview where a dragged card lands.
 */
export function NodeGrid({ store }: { store: HubStore }) {
  // Only the nodes passing the filter show. Moving one among them keeps every
  // hidden node in its place in the saved order.
  const nodes = store.filteredNodes.value;
  const { list, reordering, draggingId, announcement, press, move } =
    useReorderableList<HTMLElement>(
      nodes.map((node) => node.id),
      {
        label: (id) => nodes.find((node) => node.id === id)?.name || id,
        save: (orderedIds) =>
          store.reorderNodes(
            mergeShownOrder(
              store.nodes.peek().map((node) => node.id),
              orderedIds,
            ),
          ),
        report: (error) => store.reportNodeOrderError(error),
        focusTargets: { row: '.node-menu-toggle', menu: '.node-menu-toggle' },
      },
    );
  const reorder = useMemo<NodeReorder>(() => ({ press, move }), [press, move]);
  return (
    <>
      <section
        ref={list}
        class={`node-grid ${reordering ? 'is-reordering' : ''}`}
        aria-label="Nodes"
        aria-busy={store.initialLoading.value}
      >
        {nodes.map((node, position) => (
          <HubRenderBoundary
            key={node.id}
            resetKey={node}
            label={typeof node.name === 'string' && node.name ? `Node ${node.name}` : 'Node'}
          >
            <NodeCard
              node={node}
              store={store}
              position={position}
              count={nodes.length}
              dragging={draggingId === node.id}
              reorder={reorder}
            />
          </HubRenderBoundary>
        ))}
      </section>
      <div class="visually-hidden" role="status" aria-live="polite">
        {announcement}
      </div>
      <NodeFilterEmpty store={store} />
    </>
  );
}

/** Explains an empty grid when nodes exist but none pass the filter. */
function NodeFilterEmpty({ store }: { store: HubStore }) {
  const filter = store.nodeFilter.value;
  if (filter === 'all' || store.filteredNodes.value.length || !store.nodes.value.length) {
    return null;
  }
  return (
    <div class="hub-empty node-filter-empty">
      <p>{filter === 'online' ? 'No nodes are online.' : 'Every node is online.'}</p>
      <button class="hub-btn ghost" type="button" onClick={() => (store.nodeFilter.value = 'all')}>
        Show all nodes
      </button>
    </div>
  );
}
