import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import type { Project, Session } from '../domain/types';
import { displayName } from '../app/config';
import { readJSON, writeJSON } from '../platform/storage';
import { overlayManager } from '../platform/overlay-manager';
import { Icon } from './Icon';
import { trapOverlayFocus } from './Overlay';
import { useMenuKeyboard } from './Menu';

function sessionMessageCount(session: Session): number {
  if (Number.isFinite(session.messageCount)) return Math.max(0, session.messageCount || 0);
  return session.messages.filter(
    (message) => message.role === 'user' || message.role === 'assistant',
  ).length;
}

function sessionRelativeTime(value: number): string {
  const difference = Math.max(0, Date.now() - value);
  if (difference < 45_000) return 'just now';
  if (difference < 3_600_000) return `${Math.max(1, Math.floor(difference / 60_000))}m ago`;
  if (difference < 86_400_000) return `${Math.max(1, Math.floor(difference / 3_600_000))}h ago`;
  if (difference < 604_800_000) return `${Math.max(1, Math.floor(difference / 86_400_000))}d ago`;
  const date = new Date(value);
  const month = date.toLocaleString(undefined, { month: 'short' });
  return date.getFullYear() === new Date().getFullYear()
    ? `${date.getDate()} ${month}`
    : `${month} ${date.getFullYear()}`;
}

function sessionBucket(value: number): 'Today' | 'Yesterday' | 'This week' | 'Older' {
  const now = new Date();
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  if (value >= today) return 'Today';
  if (value >= today - 86_400_000) return 'Yesterday';
  if (value >= today - 6 * 86_400_000) return 'This week';
  return 'Older';
}

/** Flip a dropdown menu above its trigger when it would overflow the viewport. */
function useMenuFlip(open: boolean) {
  const menu = useRef<HTMLDivElement>(null);
  const [up, setUp] = useState(false);
  useLayoutEffect(() => {
    if (!open) {
      setUp(false);
      return;
    }
    const rect = menu.current?.getBoundingClientRect();
    if (rect && rect.bottom > window.innerHeight - 8 && rect.top - rect.height > 8) setUp(true);
  }, [open]);
  return { menu, up };
}

/** Auto-loads older conversations when scrolled into view, like the old sidebar. */
function PaginationSentinel({ load }: { load: () => Promise<void> }) {
  const node = useRef<HTMLDivElement>(null);
  const [state, setState] = useState<'idle' | 'loading' | 'error'>('idle');
  const loader = useRef(load);
  loader.current = load;
  const fallbackFired = useRef(false);
  useEffect(() => {
    const target = node.current;
    if (!target || state !== 'idle') return;
    const trigger = async () => {
      setState('loading');
      try {
        await loader.current();
        setState('idle');
      } catch {
        setState('error');
        setTimeout(() => setState('idle'), 5000);
      }
    };
    if (typeof IntersectionObserver !== 'function') {
      if (fallbackFired.current) return;
      fallbackFired.current = true;
      const timer = setTimeout(() => void trigger(), 0);
      return () => clearTimeout(timer);
    }
    const observer = new IntersectionObserver(
      (entries) => {
        if (!entries.some((entry) => entry.isIntersecting)) return;
        observer.disconnect();
        void trigger();
      },
      // Prefetch well before the sentinel is visible so scrolling feels
      // continuous instead of pausing at the bottom of the list.
      { rootMargin: '480px 0px' },
    );
    observer.observe(target);
    return () => observer.disconnect();
  }, [state]);
  return (
    <div
      ref={node}
      class={`project-pagination-sentinel ${state === 'idle' ? '' : state}`}
      role="status"
      aria-label={
        state === 'loading'
          ? 'Loading older conversations'
          : 'More conversations load automatically'
      }
    >
      {state === 'error' ? 'Couldn’t load older conversations' : ''}
    </div>
  );
}

function SessionMenu({ session, onHide }: { session: Session; onHide: () => void }) {
  const store = useStore();
  const [open, setOpen] = useState(false);
  const { menu, up } = useMenuFlip(open);
  const trigger = useRef<HTMLButtonElement>(null);
  const keyboardMenu = useMenuKeyboard(open, () => setOpen(false), trigger);
  return (
    <div class={`session-row-menu ${open ? 'open' : ''}`}>
      <button
        ref={trigger}
        class="session-menu-trigger"
        type="button"
        aria-label={`Actions for ${session.title}`}
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={(event) => {
          event.stopPropagation();
          setOpen(!open);
        }}
      >
        ⋯
      </button>
      {open && (
        <div
          ref={(element) => {
            menu.current = element;
            keyboardMenu.current = element;
          }}
          class={`session-menu ${up ? 'open-up' : ''}`}
          role="menu"
          onClick={(event) => event.stopPropagation()}
        >
          <button
            type="button"
            role="menuitem"
            onClick={() => {
              store.openRename(session);
              setOpen(false);
            }}
          >
            Rename
          </button>
          {store.projectsEnabled.value && !session.projectId && (
            <button
              type="button"
              role="menuitem"
              onClick={() => {
                store.openProjectPicker(session);
                setOpen(false);
              }}
            >
              Assign project…
            </button>
          )}
          <button
            type="button"
            role="menuitem"
            onClick={() => {
              void store.pinSession(session);
              setOpen(false);
            }}
          >
            {session.pinned ? 'Unpin' : 'Pin'}
          </button>
          <button
            type="button"
            role="menuitem"
            onClick={() => {
              if (session.archived) void store.archiveSession(session);
              else onHide();
              setOpen(false);
            }}
          >
            {session.archived ? 'Unhide' : 'Hide'}
          </button>
        </div>
      )}
    </div>
  );
}

function SessionRow({ session }: { session: Session }) {
  const store = useStore();
  const row = useRef<HTMLDivElement>(null);
  const [hiding, setHiding] = useState(false);
  const active = store.activeSessionId.value === session.id;
  const projection = store.runs.value[session.id];
  const running =
    Boolean(session.activeRun) ||
    Boolean(
      projection &&
      ['connecting', 'checking', 'streaming', 'cancelling'].includes(projection.run.status),
    );
  const messageCount = sessionMessageCount(session);
  const activityAt = session.lastMessageAt || session.created;
  const hide = () => {
    setHiding(true);
    const node = row.current;
    let done = false;
    const finish = () => {
      if (done) return;
      done = true;
      void store.archiveSession(session).catch((error) => {
        setHiding(false);
        store.toast(error, 'error');
      });
    };
    const onTransitionEnd = (event: TransitionEvent) => {
      if (event.propertyName === 'max-height') finish();
    };
    node?.addEventListener('transitionend', onTransitionEnd);
    window.setTimeout(finish, 500);
  };
  return (
    <div
      ref={row}
      class={`session-row ${session.archived ? 'archived' : ''} ${running ? 'is-active' : ''} ${hiding ? 'is-hiding' : ''}`}
    >
      <button
        class={`session-btn ${active ? 'active' : ''}`}
        type="button"
        aria-label={session.title || session.name || 'New chat'}
        aria-current={active ? 'page' : undefined}
        title={session.longTitle || session.title}
        onMouseDown={(event) => event.preventDefault()}
        onClick={() => void store.selectSession(session)}
      >
        <span class="session-title">{session.title || session.name || 'New chat'}</span>
        <span class="session-meta" title={new Date(activityAt).toLocaleString()}>
          {messageCount} {messageCount === 1 ? 'message' : 'messages'} ·{' '}
          {sessionRelativeTime(activityAt)}
        </span>
      </button>
      <SessionMenu session={session} onHide={hide} />
    </div>
  );
}

function SessionDateGroups({
  sessions,
  nested = false,
}: {
  sessions: Session[];
  nested?: boolean;
}) {
  const labels = ['Today', 'Yesterday', 'This week', 'Older'] as const;
  return (
    <>
      {labels.map((label) => {
        const entries = sessions.filter(
          (session) => sessionBucket(session.lastMessageAt || session.created) === label,
        );
        if (!entries.length) return null;
        return nested ? (
          <section class="session-date-group" key={label}>
            <h4>{label}</h4>
            {entries.map((session) => (
              <SessionRow key={session.id} session={session} />
            ))}
          </section>
        ) : (
          <section class="session-group" key={label}>
            <h3>{label}</h3>
            {entries.map((session) => (
              <SessionRow key={session.id} session={session} />
            ))}
          </section>
        );
      })}
    </>
  );
}

function NoProjectGroup({ sessions }: { sessions: Session[] }) {
  const store = useStore();
  const expansionKey = '__no_project__';
  const [open, setOpen] = useState(
    () =>
      readJSON<Record<string, boolean>>(store.storage, store.keys.projectExpansion, {})[
        expansionKey
      ] !== false,
  );
  const toggle = () => {
    const value = !open;
    setOpen(value);
    const expansion = readJSON<Record<string, boolean>>(
      store.storage,
      store.keys.projectExpansion,
      {},
    );
    writeJSON(store.storage, store.keys.projectExpansion, {
      ...expansion,
      [expansionKey]: value,
    });
  };
  const activeSession = sessions.find((session) => session.id === store.activeSessionId.value);
  return (
    <section class="session-group session-ungrouped">
      <h3 class="collapsible-session-group-heading">
        <button
          class="session-group-toggle"
          type="button"
          aria-expanded={open}
          title={`${open ? 'Collapse' : 'Expand'} No project`}
          onClick={toggle}
        >
          <span>No project</span>
          <span class="session-group-chevron" aria-hidden="true">
            <Icon name="chevron-right" />
          </span>
        </button>
      </h3>
      {open ? (
        <div class="session-group-list is-opening">
          <SessionDateGroups sessions={sessions} nested />
          {store.noProjectCursor.value && (
            <PaginationSentinel load={() => store.loadMoreNoProject()} />
          )}
        </div>
      ) : (
        activeSession && (
          <div class="session-group-collapsed-active">
            <SessionRow
              session={
                store.sessions.value.find((session) => session.id === activeSession.id) ||
                activeSession
              }
            />
          </div>
        )
      )}
    </section>
  );
}

function ProjectGroup({ project }: { project: Project }) {
  const store = useStore();
  const expansion = readJSON<Record<string, boolean>>(
    store.storage,
    store.keys.projectExpansion,
    {},
  );
  const [open, setOpen] = useState(expansion[project.id] !== false);
  const [menu, setMenu] = useState(false);
  const { menu: menuRef, up } = useMenuFlip(menu);
  const menuTrigger = useRef<HTMLButtonElement>(null);
  const keyboardMenu = useMenuKeyboard(menu, () => setMenu(false), menuTrigger);
  const toggle = () => {
    const value = !open;
    setOpen(value);
    writeJSON(store.storage, store.keys.projectExpansion, { ...expansion, [project.id]: value });
  };
  const listedSessionIDs = new Set((project.sessions || []).map((session) => session.id));
  const sessions = [
    ...(project.sessions || []).map(
      (session) => store.sessions.value.find((entry) => entry.id === session.id) || session,
    ),
    ...store.sessions.value.filter(
      (session) => session.projectId === project.id && !listedSessionIDs.has(session.id),
    ),
  ].sort(
    (left, right) =>
      Number(right.pinned) - Number(left.pinned) ||
      (right.lastMessageAt || right.created) - (left.lastMessageAt || left.created),
  );
  const regular = sessions.filter((session) => !session.pinned);
  const activeSession =
    store.activeSession.value?.projectId === project.id && !store.activeSession.value.pinned
      ? store.activeSession.value
      : regular.find((session) => session.id === store.activeSessionId.value);
  return (
    <section
      class={`project-group ${project.available === false ? 'unavailable' : ''}`}
      data-project-id={project.id}
    >
      <div class="project-group-header">
        <button
          class="project-group-toggle"
          type="button"
          aria-expanded={open}
          title={project.path || `${open ? 'Collapse' : 'Expand'} ${project.name}`}
          onClick={toggle}
        >
          <span class="project-group-label">{project.name}</span>
          {project.available === false && (
            <span class="project-unavailable-badge">Unavailable</span>
          )}
          <span class="project-group-chevron">
            <Icon name="chevron-right" />
          </span>
        </button>
        <button
          ref={menuTrigger}
          class="project-group-action"
          type="button"
          aria-label={`Actions for project ${project.name}`}
          aria-haspopup="menu"
          aria-expanded={menu}
          onClick={(event) => {
            event.stopPropagation();
            setMenu(!menu);
          }}
        >
          ⋯
        </button>
        {menu && (
          <div
            ref={(element) => {
              menuRef.current = element;
              keyboardMenu.current = element;
            }}
            class={`session-menu project-menu open ${up ? 'open-up' : ''}`}
            role="menu"
          >
            <button
              type="button"
              role="menuitem"
              onClick={() => {
                void store.startProjectChat(project.id);
                setMenu(false);
              }}
            >
              New chat
            </button>
            <button
              type="button"
              role="menuitem"
              onClick={() => {
                const name = prompt('Project name', project.name);
                if (name?.trim()) void store.mutateProject(project, { name: name.trim() });
                setMenu(false);
              }}
            >
              Rename
            </button>
            <button
              type="button"
              role="menuitem"
              onClick={() => {
                if (
                  project.archived ||
                  confirm(
                    'Archive this project? Conversations remain available when hidden sessions are shown.',
                  )
                )
                  void store.mutateProject(project, { archived: !project.archived });
                setMenu(false);
              }}
            >
              {project.archived ? 'Restore' : 'Archive'}
            </button>
          </div>
        )}
      </div>
      {open ? (
        <div class="project-session-list is-opening">
          <SessionDateGroups sessions={regular} nested />
          {project.has_more && (
            <PaginationSentinel load={() => store.loadMoreProject(project.id)} />
          )}
        </div>
      ) : (
        activeSession && (
          <div class="project-session-list">
            <SessionRow session={activeSession} />
          </div>
        )
      )}
    </section>
  );
}

export function Sidebar() {
  const store = useStore();
  const collapsed = store.sidebarCollapsed.value;
  const mobileOpen = store.sidebarOpen.value;
  const sidebar = useRef<HTMLElement>(null);
  const overlayToken = useRef<symbol | null>(null);
  useLayoutEffect(() => {
    if (!mobileOpen || !globalThis.matchMedia?.('(max-width: 760px)').matches) return;
    const trigger =
      document.activeElement instanceof HTMLElement ? document.activeElement : undefined;
    overlayToken.current = overlayManager.acquire(trigger, sidebar.current);
    const frame = requestAnimationFrame(() =>
      sidebar.current?.querySelector<HTMLElement>('#sidebarCloseBtn')?.focus(),
    );
    return () => {
      cancelAnimationFrame(frame);
      if (overlayToken.current) overlayManager.release(overlayToken.current);
      overlayToken.current = null;
    };
  }, [mobileOpen]);
  const standalone = store.sessions.value.filter((session) => !session.projectId);
  const sidebarSessions = [
    ...store.sessions.value,
    ...store.projects.value.flatMap((project) => project.sessions || []),
  ];
  const results = store.searchResults.value;
  const brand = store.config.title.trim() || displayName(store.config.agentName);
  const pinned = sidebarSessions.filter(
    (session, index) =>
      session.pinned &&
      sidebarSessions.findIndex((candidate) => candidate.id === session.id) === index,
  );
  const regular = standalone.filter((session) => !session.pinned);
  const newChat = () => {
    store.newChat();
    store.sidebarOpen.value = false;
  };
  return (
    <>
      <aside
        ref={sidebar}
        class={`sidebar ${collapsed ? 'collapsed' : ''} ${mobileOpen ? 'open' : ''}`}
        id="sidebar"
        aria-label="Sessions"
        role={mobileOpen ? 'dialog' : undefined}
        aria-modal={mobileOpen || undefined}
        onKeyDown={(event) => {
          if (
            event.key === 'Escape' &&
            overlayToken.current &&
            overlayManager.isTop(overlayToken.current)
          ) {
            event.preventDefault();
            event.stopPropagation();
            store.sidebarOpen.value = false;
            return;
          }
          if (overlayToken.current)
            trapOverlayFocus(
              event,
              'button:not([disabled]),input:not([disabled]),a[href],[tabindex]:not([tabindex="-1"])',
            );
        }}
      >
        <div class="sidebar-rail">
          <button
            class="icon-btn sidebar-rail-btn"
            id="sidebarToggleBtn"
            type="button"
            aria-label="Expand sidebar"
            aria-expanded={!collapsed}
            onClick={() => {
              store.sidebarCollapsed.value = false;
              store.storage.removeItem(store.keys.sidebarCollapsed);
            }}
          >
            <Icon name="panel" />
          </button>
          <button
            class="icon-btn sidebar-rail-btn"
            id="sidebarRailNewChatBtn"
            type="button"
            aria-label="New chat"
            onClick={newChat}
          >
            <Icon name="add" />
          </button>
          <button
            class="icon-btn sidebar-rail-btn"
            id="sidebarRailSettingsBtn"
            type="button"
            aria-label="Token settings"
            onClick={() => {
              store.modal.value = 'settings';
            }}
          >
            <Icon name="settings" />
          </button>
        </div>
        <div class="sidebar-panel">
          <div class="sidebar-header">
            <button
              class="icon-btn"
              id="sidebarPanelToggleBtn"
              type="button"
              aria-label="Collapse sidebar"
              aria-expanded={!collapsed}
              onClick={() => {
                store.sidebarCollapsed.value = true;
                store.storage.setItem(store.keys.sidebarCollapsed, '1');
              }}
            >
              <Icon name="panel" />
            </button>
            <div class="brand">
              <span id="sidebarBrandText">{brand}</span>
            </div>
            <div class="sidebar-header-actions">
              <button
                class="icon-btn"
                id="settingsBtn"
                aria-label="Token settings"
                onClick={() => {
                  store.modal.value = 'settings';
                }}
              >
                <Icon name="settings" />
              </button>
              <button
                class="icon-btn sidebar-close"
                id="sidebarCloseBtn"
                aria-label="Close sidebar"
                onClick={() => {
                  store.sidebarOpen.value = false;
                }}
              >
                <Icon name="close" />
              </button>
            </div>
          </div>
          <div class="sidebar-content" id="sidebarContent">
            <div class="sidebar-search-wrap">
              <input
                class="sidebar-search-input"
                id="sidebarSearchInput"
                type="search"
                placeholder="Search chats"
                autoComplete="off"
                aria-label="Search chats"
                value={store.sidebarSearch.value}
                onInput={(event) => void store.search(event.currentTarget.value)}
              />
            </div>
            <div class="sidebar-actions">
              <button class="new-chat-btn" id="newChatBtn" type="button" onClick={newChat}>
                <Icon class="sidebar-action-icon" name="edit" />
                <span>New chat</span>
              </button>
              {store.showWidgets.value && store.widgets.value.length > 0 && (
                <button
                  class="widgets-sidebar-btn"
                  id="widgetsOpenBtn"
                  type="button"
                  onClick={() => {
                    store.modal.value = 'widgets';
                  }}
                >
                  <Icon class="sidebar-action-icon" name="widgets" />
                  <span>Widgets</span>
                </button>
              )}
              {store.config.hub?.url && (
                <a class="back-to-hub-link" id="backToHubLink" href={store.config.hub.url}>
                  <Icon class="sidebar-action-icon" name="arrow-left" />
                  <span>Back to Hub</span>
                </a>
              )}
              {store.hubAgents.value.length > 0 && (
                <nav class="hub-agent-links" aria-label="Hub agents">
                  {store.hubAgents.value.map((agent) => (
                    <a
                      class="hub-agent-link"
                      key={agent.id}
                      href={agent.target}
                      aria-current={agent.id === store.config.hub?.nodeId ? 'true' : undefined}
                      onClick={() => store.clearHubAttention(agent.id)}
                    >
                      <span class="hub-agent-icon" aria-hidden="true" />
                      <span class="hub-agent-name">{agent.name}</span>
                      {agent.attention && (
                        <>
                          <span class="hub-agent-attention" aria-hidden="true" />
                          <span class="visually-hidden">Needs attention</span>
                        </>
                      )}
                    </a>
                  ))}
                </nav>
              )}
            </div>
            <div class="session-groups" id="sessionGroups">
              {store.searchLoading.value && <div class="sidebar-loading">Searching…</div>}
              {store.searchError.value && (
                <div class="sidebar-error">
                  {store.searchError.value}
                  <button onClick={() => void store.search(store.sidebarSearch.value)}>
                    Retry
                  </button>
                </div>
              )}
              {results ? (
                <section class="session-group">
                  <h3>Search results</h3>
                  {results.map((session) => (
                    <SessionRow key={session.id} session={session} />
                  ))}
                  {!results.length && !store.searchLoading.value && (
                    <div class="sidebar-empty">No chats found.</div>
                  )}
                </section>
              ) : (
                <>
                  {pinned.length > 0 && (
                    <section class="session-group sidebar-pinned-group">
                      <h3>Pinned</h3>
                      {pinned.map((session) => (
                        <SessionRow key={session.id} session={session} />
                      ))}
                    </section>
                  )}
                  {store.projects.value.length > 0 && (
                    <section class="session-group sidebar-project-groups">
                      <h3>Projects</h3>
                      {store.projects.value.map((project) => (
                        <ProjectGroup key={project.id} project={project} />
                      ))}
                    </section>
                  )}
                  {regular.length > 0 &&
                    (store.projectsEnabled.value ? (
                      <NoProjectGroup sessions={regular} />
                    ) : (
                      <div class="flat-session-date-groups">
                        <SessionDateGroups sessions={regular} />
                        {store.noProjectCursor.value && (
                          <PaginationSentinel load={() => store.loadMoreNoProject()} />
                        )}
                      </div>
                    ))}
                </>
              )}
            </div>
          </div>
        </div>
      </aside>
      <div
        class={`sidebar-backdrop ${mobileOpen ? 'open' : ''}`}
        id="sidebarBackdrop"
        onClick={() => {
          if (!overlayToken.current || overlayManager.isTop(overlayToken.current))
            store.sidebarOpen.value = false;
        }}
      />
    </>
  );
}
