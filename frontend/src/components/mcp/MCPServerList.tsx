import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { useStore } from '../../app/context';
import type { MCPServer } from '../../domain/types';
import type { MCPOAuthUIState } from '../../stores/mcp-store';
import { SearchField } from '../FormFields';
import { Icon } from '../Icon';
import { Menu } from '../Menu';
import { mcpServerMeta, mcpServerTone } from './mcp-format';

const UNDO_TIMEOUT_MS = 8_000;
const MENU_GAP_PX = 6;
const MENU_FLIP_THRESHOLD_PX = 140;

interface MenuPosition {
  top?: number;
  bottom?: number;
  right: number;
}

/**
 * The row menu is fixed to the viewport so the scrolling list never clips it;
 * it opens upward when the trigger sits near the bottom of the screen.
 */
function menuPositionFor(trigger: HTMLElement): MenuPosition {
  const rect = trigger.getBoundingClientRect();
  const right = Math.max(8, window.innerWidth - rect.right);
  if (window.innerHeight - rect.bottom < MENU_FLIP_THRESHOLD_PX)
    return { bottom: window.innerHeight - rect.top + MENU_GAP_PX, right };
  return { top: rect.bottom + MENU_GAP_PX, right };
}

function ServerMenu({ server, disabled }: { server: MCPServer; disabled: boolean }) {
  const store = useStore();
  const trigger = useRef<HTMLButtonElement>(null);
  const [position, setPosition] = useState<MenuPosition | null>(null);
  const open = position !== null;
  const close = () => setPosition(null);

  useLayoutEffect(() => {
    if (!open) return;
    const list = trigger.current?.closest('.mcp-list');
    list?.addEventListener('scroll', close, { passive: true });
    window.addEventListener('resize', close);
    return () => {
      list?.removeEventListener('scroll', close);
      window.removeEventListener('resize', close);
    };
  }, [open]);

  const canSignOut = server.canSignOut && server.authState === 'signed_in';
  return (
    <>
      <button
        ref={trigger}
        class="mcp-row-more"
        type="button"
        aria-label={`More actions for ${server.name}`}
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => (open ? close() : setPosition(menuPositionFor(trigger.current!)))}
      >
        <Icon name="more" />
      </button>
      {position && (
        <div class="mcp-row-menu-layer" style={{ ...position }}>
          <Menu
            open
            label={`${server.name} actions`}
            onClose={close}
            triggerRef={trigger}
            className="mcp-row-menu"
          >
            {canSignOut && (
              <button
                type="button"
                role="menuitem"
                disabled={store.streaming.value}
                onClick={() => {
                  close();
                  void store.logoutMCPOAuth(server.name);
                }}
              >
                <Icon name="logout" />
                Sign out
              </button>
            )}
            <button
              type="button"
              role="menuitem"
              class="danger"
              disabled={disabled}
              onClick={() => {
                close();
                void store.removeMCPServer(server.name);
              }}
            >
              <Icon name="trash" />
              Remove server
            </button>
          </Menu>
        </div>
      )}
    </>
  );
}

function AuthActions({ server }: { server: MCPServer }) {
  const store = useStore();
  const oauth = store.mcp.value.oauth?.[server.name];
  if (oauth?.state === 'starting' || oauth?.state === 'pending')
    return (
      <>
        {oauth.authorizationURL && (
          <button
            class="mcp-inline-action"
            type="button"
            onClick={() => void store.copyMCPOAuthLink(server.name)}
          >
            Copy link
          </button>
        )}
        <button
          class="mcp-inline-action"
          type="button"
          onClick={() => void store.cancelMCPOAuth(server.name)}
        >
          Cancel
        </button>
      </>
    );
  if (oauth?.state === 'failed')
    return (
      <button
        class="mcp-inline-action attention"
        type="button"
        onClick={() => void store.startMCPOAuth(server.name, true)}
      >
        Retry
      </button>
    );
  if (!server.canSignIn || server.authState === 'signed_in') return null;
  const label =
    server.authState === 'needs_sign_in'
      ? 'Sign in again'
      : server.authState === 'retry'
        ? 'Retry'
        : 'Sign in';
  return (
    <button
      class="mcp-inline-action attention"
      type="button"
      disabled={store.streaming.value}
      onClick={() => void store.startMCPOAuth(server.name, server.authState === 'needs_sign_in')}
    >
      {label}
    </button>
  );
}

function rowMeta(server: MCPServer, oauth: MCPOAuthUIState | undefined): string {
  if (oauth?.state === 'starting' || oauth?.state === 'pending')
    return oauth.popupBlocked
      ? 'popup blocked — copy the sign-in link'
      : 'waiting for authorization…';
  if (oauth?.state === 'failed') return oauth.error || 'sign-in failed';
  return mcpServerMeta(server);
}

function ServerRow({ server, disabled }: { server: MCPServer; disabled: boolean }) {
  const store = useStore();
  const checked = store.mcp.value.enabled.includes(server.name);
  const tone = mcpServerTone(server);
  const meta = rowMeta(server, store.mcp.value.oauth?.[server.name]);
  return (
    <div class="mcp-row" data-enabled={checked ? 'true' : 'false'} data-tone={tone}>
      <span class={`mcp-dot ${tone}`} aria-hidden="true" />
      <span class="mcp-row-text">
        <span class="mcp-row-name">{server.name}</span>
        {meta && (
          <span class="mcp-row-meta" title={meta}>
            {meta}
          </span>
        )}
      </span>
      <AuthActions server={server} />
      {server.configured && <ServerMenu server={server} disabled={disabled} />}
      <span class="mcp-switch">
        <input
          class="mcp-switch-input"
          type="checkbox"
          aria-label={`${checked ? 'Disable' : 'Enable'} ${server.name}`}
          checked={checked}
          disabled={disabled}
          onChange={() => void store.toggleMCP(server.name)}
        />
        <span class="mcp-switch-track" aria-hidden="true">
          <span class="mcp-switch-thumb" />
        </span>
      </span>
    </div>
  );
}

function UndoBar() {
  const store = useStore();
  const removed = store.mcpRemoved.value;
  useEffect(() => {
    if (!removed) return;
    const timer = setTimeout(() => store.dismissRemovedMCPServer(), UNDO_TIMEOUT_MS);
    return () => clearTimeout(timer);
  }, [removed, store]);
  if (!removed) return null;
  return (
    <div class="mcp-undo" role="status">
      <span>
        Removed <strong>{removed.name}</strong>
      </span>
      <button type="button" onClick={() => void store.undoRemoveMCPServer()}>
        Undo
      </button>
    </div>
  );
}

function EmptyState({ onAdd }: { onAdd: () => void }) {
  const state = useStore().mcp.value;
  if (state.error)
    return (
      <div class="mcp-empty" role="status">
        <strong>Unable to load MCP servers</strong>
        <span>The configured server list could not be read. See the error below.</span>
      </div>
    );
  return (
    <div class="mcp-empty" role="status">
      <strong>No MCP servers yet</strong>
      <span>Add one from the catalogue, a URL, or a local command.</span>
      <button class="mcp-button" type="button" onClick={onAdd}>
        <Icon name="add" />
        Add server
      </button>
    </div>
  );
}

function ErrorPanel() {
  const store = useStore();
  const state = store.mcp.value;
  const serverErrors = state.servers.filter((server) => server.error);
  if (state.loading || (!state.error && serverErrors.length === 0)) return null;
  return (
    <section class="mcp-error-panel" role="alert" aria-labelledby="mcp-error-title">
      <div class="mcp-error-header">
        <strong id="mcp-error-title">MCP server error</strong>
        <button class="mcp-inline-action" type="button" onClick={() => void store.loadMCP()}>
          Retry
        </button>
      </div>
      <div
        class="mcp-error-details"
        role="region"
        aria-label="MCP server error details"
        tabIndex={0}
      >
        {state.error
          ? state.error
          : serverErrors.map((server) => (
              <div class="mcp-error-entry" key={server.name}>
                <strong>{server.name}: </strong>
                {server.error}
              </div>
            ))}
      </div>
    </section>
  );
}

function Feedback({ notice }: { notice: string }) {
  const store = useStore();
  const state = store.mcp.value;
  let message = notice;
  if (store.streaming.value)
    message = 'Servers can’t be turned on or off while a response is running.';
  else if (state.pending)
    message = `${state.enabled.includes(state.pending) ? 'Starting' : 'Stopping'} ${state.pending}…`;
  if (!message || state.error) return null;
  return (
    <div class="mcp-feedback" aria-live="polite">
      {message}
    </div>
  );
}

/** The flat server list: filter, dot + name + one muted line, and a switch. */
export function MCPServerList({ notice, onAdd }: { notice: string; onAdd: () => void }) {
  const store = useStore();
  const state = store.mcp.value;
  const [query, setQuery] = useState('');
  const normalized = query.trim().toLocaleLowerCase();
  const visible = normalized
    ? state.servers.filter((server) => server.name.toLocaleLowerCase().includes(normalized))
    : state.servers;
  const disabled = state.loading || Boolean(state.pending) || store.streaming.value;
  const ready = !state.loading && state.servers.length > 0;
  return (
    <>
      {ready && (
        <div class="mcp-filter-row">
          <SearchField
            className="mcp-filter"
            aria-label="Filter MCP servers"
            autoFocus
            value={query}
            placeholder="Filter"
            onInput={(event) => setQuery(event.currentTarget.value)}
          />
          <span
            class="mcp-count"
            aria-label={`${state.enabled.length} ${state.enabled.length === 1 ? 'server' : 'servers'} enabled`}
          >
            {state.enabled.length} of {state.servers.length} on
          </span>
        </div>
      )}
      <div class="mcp-list" aria-busy={state.loading ? 'true' : undefined}>
        {state.loading ? (
          <div class="mcp-empty" role="status">
            Loading MCP servers…
          </div>
        ) : state.servers.length === 0 ? (
          <EmptyState onAdd={onAdd} />
        ) : visible.length === 0 ? (
          <div class="mcp-empty" role="status">
            <strong>No matching servers</strong>
            <span>Try a different name or clear the filter.</span>
          </div>
        ) : (
          visible.map((server) => (
            <ServerRow key={server.name} server={server} disabled={disabled} />
          ))
        )}
      </div>
      <ErrorPanel />
      <Feedback notice={notice} />
      <UndoBar />
    </>
  );
}
