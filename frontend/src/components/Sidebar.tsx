import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import type { Project, Session } from '../domain/types';
import { displayName } from '../app/config';
import { readJSON, writeJSON } from '../platform/storage';
import { Icon } from './Icon';

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

function SessionMenu({ session }: { session: Session }) {
  const store = useStore();
  const [open, setOpen] = useState(false);
  const { menu, up } = useMenuFlip(open);
  const trigger = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (event: MouseEvent) => {
      if (!(event.target as HTMLElement).closest('.session-row-menu')) setOpen(false);
    };
    addEventListener('click', close);
    return () => removeEventListener('click', close);
  }, [open]);
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
          ref={menu}
          class={`session-menu ${up ? 'open-up' : ''}`}
          role="menu"
          onClick={(event) => event.stopPropagation()}
          onKeyDown={(event) => {
            if (event.key === 'Escape') {
              setOpen(false);
              trigger.current?.focus();
            }
          }}
        >
          <button
            role="menuitem"
            onClick={() => {
              store.openRename(session);
              setOpen(false);
            }}
          >
            Rename
          </button>
          {store.projectsEnabled.value && (
            <button
              role="menuitem"
              onClick={() => {
                store.openProjectPicker(session);
                setOpen(false);
              }}
            >
              Move to project…
            </button>
          )}
          <button
            role="menuitem"
            onClick={() => {
              void store.pinSession(session);
              setOpen(false);
            }}
          >
            {session.pinned ? 'Unpin' : 'Pin'}
          </button>
          <button
            role="menuitem"
            onClick={() => {
              void store.archiveSession(session);
              setOpen(false);
            }}
          >
            {session.archived ? 'Unhide' : 'Hide'}
          </button>
          <button
            role="menuitem"
            class="danger"
            onClick={() => {
              if (confirm(`Delete “${session.title}”?`)) void store.removeSession(session);
              setOpen(false);
            }}
          >
            Delete
          </button>
        </div>
      )}
    </div>
  );
}

function SessionRow({ session }: { session: Session }) {
  const store = useStore();
  const active = store.activeSessionId.value === session.id;
  const projection = store.runs.value[session.id];
  const running =
    projection && ['connecting', 'streaming', 'cancelling'].includes(projection.run.status);
  return (
    <div class={`session-row ${session.archived ? 'archived' : ''} ${running ? 'is-active' : ''}`}>
      <button
        class={`session-btn ${active ? 'active' : ''}`}
        type="button"
        aria-label={session.title || session.name || 'New chat'}
        aria-current={active ? 'page' : undefined}
        title={session.longTitle || session.title}
        onMouseDown={(event) => event.preventDefault()}
        onClick={() => void store.selectSession(session)}
      >
        <span class="session-title">
          {session.pinned && (
            <span class="session-pin" aria-label="Pinned">
              ◆
            </span>
          )}
          {session.title || session.name || 'New chat'}
        </span>
        <span class="session-meta">
          {session.projectUnavailable
            ? 'Project unavailable'
            : session.longTitle ||
              session.workingDir ||
              (session.messageCount != null
                ? `${session.messageCount} ${session.messageCount === 1 ? 'message' : 'messages'}`
                : '')}
        </span>
        {running && <span class="session-progress" aria-label="Response in progress" />}
      </button>
      <SessionMenu session={session} />
    </div>
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
  useEffect(() => {
    if (!menu) return;
    const close = (event: MouseEvent) => {
      if (!(event.target as HTMLElement).closest('.project-group-header')) setMenu(false);
    };
    addEventListener('click', close);
    return () => removeEventListener('click', close);
  }, [menu]);
  const toggle = () => {
    const value = !open;
    setOpen(value);
    writeJSON(store.storage, store.keys.projectExpansion, { ...expansion, [project.id]: value });
  };
  const sessions = project.sessions || [];
  const pinned = sessions.filter((session) => session.pinned);
  const regular = sessions.filter((session) => !session.pinned);
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
            ref={menuRef}
            class={`session-menu project-menu open ${up ? 'open-up' : ''}`}
            role="menu"
          >
            <button
              role="menuitem"
              onClick={() => {
                void store.startProjectChat(project.id);
                setMenu(false);
              }}
            >
              New chat
            </button>
            <button
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
      {open && (
        <div class="project-session-list is-opening">
          {pinned.map((session) => (
            <SessionRow
              key={session.id}
              session={store.sessions.value.find((entry) => entry.id === session.id) || session}
            />
          ))}
          {regular.map((session) => (
            <SessionRow
              key={session.id}
              session={store.sessions.value.find((entry) => entry.id === session.id) || session}
            />
          ))}
          {project.has_more && (
            <PaginationSentinel load={() => store.loadMoreProject(project.id)} />
          )}
        </div>
      )}
    </section>
  );
}

export function Sidebar() {
  const store = useStore();
  const collapsed = store.sidebarCollapsed.value;
  const standalone = store.sessions.value.filter((session) => !session.projectId);
  const results = store.searchResults.value;
  const brand = store.config.title.trim() || displayName(store.config.agentName);
  const pinned = standalone.filter((session) => session.pinned);
  const regular = standalone.filter((session) => !session.pinned);
  const newChat = () => {
    store.newChat();
    store.sidebarOpen.value = false;
  };
  return (
    <>
      <aside
        class={`sidebar ${collapsed ? 'collapsed' : ''} ${store.sidebarOpen.value ? 'open' : ''}`}
        id="sidebar"
        aria-label="Sessions"
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
              {store.projectsEnabled.value && (
                <button
                  class="new-project-btn"
                  type="button"
                  onClick={() => {
                    store.projectTarget.value = null;
                    store.modal.value = 'project';
                  }}
                >
                  <Icon name="add" /> <span>Project</span>
                </button>
              )}
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
                  {store.projects.value.map((project) => (
                    <ProjectGroup key={project.id} project={project} />
                  ))}
                  {pinned.length > 0 && (
                    <section class="session-group">
                      <h3>Pinned</h3>
                      {pinned.map((session) => (
                        <SessionRow key={session.id} session={session} />
                      ))}
                    </section>
                  )}
                  {regular.length > 0 && (
                    <section class="session-group session-ungrouped">
                      <h3>{store.projectsEnabled.value ? 'No project' : 'Chats'}</h3>
                      {regular.map((session) => (
                        <SessionRow key={session.id} session={session} />
                      ))}
                      {store.noProjectCursor.value && (
                        <PaginationSentinel load={() => store.loadMoreNoProject()} />
                      )}
                    </section>
                  )}
                </>
              )}
            </div>
          </div>
        </div>
      </aside>
      <div
        class={`sidebar-backdrop ${store.sidebarOpen.value ? 'open' : ''}`}
        id="sidebarBackdrop"
        onClick={() => {
          store.sidebarOpen.value = false;
        }}
      />
    </>
  );
}
