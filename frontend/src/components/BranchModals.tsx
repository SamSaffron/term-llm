import { useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { SearchField } from './FormFields';
import { Overlay } from './Overlay';

export function BranchContext() {
  const store = useStore();
  const [focus, setFocus] = useState('');
  const [mode, setMode] = useState<'choices' | 'focused'>('choices');
  const anchor = store.branchTarget.value;
  const prefill = store.branchPrefill.value;
  const busy = store.branchBusy.value;
  const error = store.branchError.value;
  const choose = (context: 'clean' | 'notes' | 'focused') => {
    if (busy) return;
    if (context === 'focused' && mode !== 'focused') {
      setMode('focused');
      return;
    }
    if (prefill) void store.branchFrom(anchor, context, focus.trim(), '', prefill);
    else void store.branchFrom(anchor, context, focus.trim());
  };
  return (
    <Overlay
      title="Start a conversation path"
      dismissDisabled={busy}
      onEscape={() => {
        if (!busy)
          if (mode === 'focused') setMode('choices');
          else store.modal.value = '';
      }}
    >
      <p>Choose how much context to carry after this turn.</p>
      <div aria-busy={busy ? 'true' : undefined}>
        {mode === 'choices' ? (
          <div class="branch-context-choices">
            <button type="button" disabled={busy} onClick={() => choose('clean')}>
              <strong>Clean branch</strong>
              <small>Continue only with context up to this turn.</small>
            </button>
            <button type="button" disabled={busy} onClick={() => choose('notes')}>
              <strong>Bring concise notes</strong>
              <small>Prepare a short summary of useful later discoveries.</small>
            </button>
            <button type="button" disabled={busy} onClick={() => choose('focused')}>
              <strong>Focused context</strong>
              <small>Tell the agent which later information matters.</small>
            </button>
          </div>
        ) : (
          <div class="branch-context-focus">
            <label for="branchContextFocus">What should this path carry forward?</label>
            <textarea
              id="branchContextFocus"
              autoFocus
              rows={5}
              value={focus}
              placeholder="For example: preserve the database findings, but not the abandoned UI approach."
              disabled={busy}
              onInput={(event) => setFocus(event.currentTarget.value)}
            />
            <div class="modal-actions">
              <button class="btn" disabled={busy} onClick={() => setMode('choices')}>
                Back
              </button>
              <button
                class="btn primary"
                disabled={busy || !focus.trim()}
                onClick={() => choose('focused')}
              >
                Create path
              </button>
            </div>
          </div>
        )}
      </div>
      {busy && (
        <div role="status" aria-live="polite">
          Creating path…
        </div>
      )}
      {error && (
        <div class="modal-error" role="alert">
          {error}
        </div>
      )}
      <p class="branch-tree-note">Filesystem and tool side effects are not undone.</p>
    </Overlay>
  );
}

const BRANCH_POINT_BATCH_SIZE = 50;

export function BranchTree() {
  const store = useStore();
  const [query, setQuery] = useState('');
  const [pointLimit, setPointLimit] = useState(BRANCH_POINT_BATCH_SIZE);
  const tree = store.branchTree.value;
  const nodes =
    tree && Array.isArray(tree.nodes) ? (tree.nodes as Array<Record<string, unknown>>) : [];
  const points =
    tree && Array.isArray(tree.branch_points)
      ? (tree.branch_points as Array<Record<string, unknown>>).filter(
          (point) => String(point.role || '') === 'user',
        )
      : [];
  const active = String(tree?.active_session_id || store.activeSessionId.value);
  const root = String(tree?.root_session_id || '');
  const normalizedQuery = query.trim().toLocaleLowerCase();
  const matchesQuery = (...values: unknown[]) =>
    !normalizedQuery ||
    values.some((value) =>
      String(value || '')
        .toLocaleLowerCase()
        .includes(normalizedQuery),
    );
  const nodeEntries = nodes.map((node, index) => {
    const id = String(node.session_id || '');
    const session = store.sessions.value.find(
      (entry) =>
        entry.id === id || (node.session_number && entry.number === Number(node.session_number)),
    );
    const title = String(node.title || session?.title || `Path ${index + 1}`);
    return { node, index, id, session, title, current: id === active };
  });
  const visibleNodes = nodeEntries.filter(({ node, title, current, id }) =>
    matchesQuery(
      title,
      node.anchor_preview,
      node.session_number,
      current && 'current',
      id === root && 'origin',
    ),
  );
  const pointEntries = points
    .map((point, index) => ({
      point,
      index,
      sequence: Math.max(1, Number(point.sequence) + 1 || 1),
      later: Math.max(0, Number(point.later_message_count) || 0),
      preview: String(point.preview || '(attachment content)'),
    }))
    .sort((left, right) => right.sequence - left.sequence);
  const visiblePoints = pointEntries.filter(({ point, preview, sequence }) =>
    matchesQuery(preview, point.prefill, sequence, `message ${sequence}`),
  );
  const shownPoints = normalizedQuery ? visiblePoints : visiblePoints.slice(0, pointLimit);
  const hiddenPointCount = visiblePoints.length - shownPoints.length;
  const showSearch = nodes.length + points.length > 8;
  const countLabel = (visible: number, total: number) =>
    normalizedQuery ? `${visible} of ${total}` : String(total);

  return (
    <Overlay title="Conversation paths" className="branch-tree-modal">
      <p class="branch-tree-intro">
        Open an existing path, or start a new one from an earlier message.
      </p>
      {showSearch && (
        <SearchField
          className="branch-tree-search"
          aria-label="Filter conversation paths and messages"
          value={query}
          placeholder="Find a path or message…"
          autoFocus
          onInput={(event) => setQuery(event.currentTarget.value)}
          onKeyDown={(event) => {
            if (event.key === 'Escape' && query) {
              event.preventDefault();
              event.stopPropagation();
              setQuery('');
            }
          }}
        />
      )}
      <div class="branch-tree-list">
        {visibleNodes.length > 0 && (
          <div class="branch-tree-section-title">
            <span>Existing paths</span>
            <span class="branch-tree-section-count">
              {countLabel(visibleNodes.length, nodeEntries.length)}
            </span>
          </div>
        )}
        {visibleNodes.map(({ node, index, id, session, title, current }) => {
          const content = (
            <div class="branch-tree-item-content">
              <div class="branch-tree-item-title">
                <strong>{title}</strong>
                {id === root && <span class="project-browser-badge">Origin</span>}
                {current && <span class="project-browser-badge is-added">Current</span>}
              </div>
              {node.anchor_preview && (
                <div class="branch-origin-preview">After “{String(node.anchor_preview)}”</div>
              )}
            </div>
          );
          if (current)
            return (
              <section
                class="branch-tree-item active"
                aria-current="true"
                key={id || String(index)}
              >
                {content}
              </section>
            );
          return (
            <button
              class="branch-tree-item branch-tree-path"
              type="button"
              key={id || String(index)}
              disabled={!id}
              title="Open this path"
              onClick={() => {
                store.modal.value = '';
                if (session) void store.selectSession(session);
                else void store.resolveAndSelectSession(id);
              }}
            >
              {content}
            </button>
          );
        })}
        {visiblePoints.length > 0 && (
          <div class="branch-tree-section-title">
            <span>Branch from a message</span>
            <span class="branch-tree-section-count">
              {countLabel(visiblePoints.length, pointEntries.length)} · newest first
            </span>
          </div>
        )}
        {shownPoints.map(({ point, index, sequence, later, preview }) => (
          <button
            class="branch-tree-item branch-tree-point"
            type="button"
            key={String(point.message_id || index)}
            aria-label={`Branch from message ${sequence}: ${preview}`}
            title={`Start a path from message ${sequence}`}
            onClick={() =>
              store.openBranchContext(
                String(Math.max(0, Number(point.anchor_message_id) || 0)),
                String(point.prefill || ''),
              )
            }
          >
            <div class="branch-tree-item-content">
              <div class="branch-tree-item-title">
                <strong>{preview}</strong>
              </div>
              <small>
                Message {sequence}
                {later > 0 ? ` · ${later} later message${later === 1 ? '' : 's'}` : ''}
              </small>
            </div>
          </button>
        ))}
        {hiddenPointCount > 0 && (
          <button
            class="branch-tree-more"
            type="button"
            onClick={() => setPointLimit((limit) => limit + BRANCH_POINT_BATCH_SIZE)}
          >
            Show {Math.min(BRANCH_POINT_BATCH_SIZE, hiddenPointCount)} older message
            {Math.min(BRANCH_POINT_BATCH_SIZE, hiddenPointCount) === 1 ? '' : 's'}
          </button>
        )}
        {normalizedQuery && visibleNodes.length === 0 && visiblePoints.length === 0 && (
          <div class="branch-tree-empty" role="status">
            <strong>No matching paths or messages</strong>
            <span>Try a different phrase or clear the filter.</span>
          </div>
        )}
      </div>
    </Overlay>
  );
}
