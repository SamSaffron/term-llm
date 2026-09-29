import { computed } from '@preact/signals';
import type { ComponentChildren } from 'preact';
import { useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import type { Project, Session } from '../domain/types';
import { displayName } from '../app/config';
import { readJSON, writeJSON } from '../platform/storage';
import { overlayManager } from '../platform/overlay-manager';
import { compareSessionsByActivity } from '../stores/store-utils';
import { Icon } from './Icon';
import { trapOverlayFocus } from './Overlay';
import { useMenuKeyboard } from './Menu';
import { useChatShortcuts } from './useChatShortcuts';
import { useMediaQuery } from './useMediaQuery';
import { reorderKeyOffset, useReorderableList, type ReorderControls } from './useReorderableList';
import { useEdgeSwipeOpen, useSwipeDismiss } from './useSwipeDismiss';

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

/** Keep a dropdown within its nearest scrollport and flip it when needed. */
function useMenuFlip(open: boolean) {
  const menu = useRef<HTMLDivElement>(null);
  const [up, setUp] = useState(false);
  useLayoutEffect(() => {
    if (!open) {
      setUp(false);
      return;
    }
    const panel = menu.current;
    if (!panel) return;
    const scrollport = panel.closest<HTMLElement>('.sidebar-content');
    const update = () => {
      const rect = panel.getBoundingClientRect();
      const menuHeight = Math.max(rect.height, panel.scrollHeight);
      const triggerRect = panel.parentElement?.getBoundingClientRect() || rect;
      const bounds = scrollport?.getBoundingClientRect() || {
        top: 8,
        bottom: window.innerHeight - 8,
      };
      const below = Math.max(0, bounds.bottom - triggerRect.bottom - 8);
      const above = Math.max(0, triggerRect.top - bounds.top - 8);
      const nextUp = menuHeight > below && above > below;
      setUp(nextUp);
      panel.style.maxHeight = `${Math.max(72, nextUp ? above : below)}px`;
    };
    update();
    scrollport?.addEventListener('scroll', update, { passive: true });
    window.addEventListener('resize', update);
    const observer = typeof ResizeObserver === 'function' ? new ResizeObserver(update) : null;
    observer?.observe(panel);
    return () => {
      scrollport?.removeEventListener('scroll', update);
      window.removeEventListener('resize', update);
      observer?.disconnect();
    };
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

function SessionMenu({
  session,
  onArchive,
  open,
  onOpenChange,
  pinned,
}: {
  session: Session;
  onArchive: () => void;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  pinned?: ReorderControls;
}) {
  const store = useStore();
  const menuID = useId();
  const { menu, up } = useMenuFlip(open);
  const trigger = useRef<HTMLButtonElement>(null);
  const keyboardMenu = useMenuKeyboard(open, () => onOpenChange(false), trigger);
  return (
    <div class={`session-row-menu ${open ? 'open' : ''}`}>
      <button
        ref={trigger}
        class="session-menu-trigger"
        type="button"
        aria-label={`Actions for ${session.title}`}
        aria-haspopup="menu"
        aria-controls={menuID}
        aria-expanded={open}
        onClick={(event) => {
          event.stopPropagation();
          onOpenChange(!open);
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
          id={menuID}
          class={`session-menu ${up ? 'open-up' : ''}`}
          role="menu"
          aria-label={`Actions for ${session.title}`}
          onClick={(event) => event.stopPropagation()}
        >
          <button
            type="button"
            role="menuitem"
            onClick={() => {
              store.openRename(session);
              onOpenChange(false);
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
                onOpenChange(false);
              }}
            >
              Assign project…
            </button>
          )}
          <button
            type="button"
            role="menuitem"
            onClick={() => {
              void store.pinSession(session).catch((error) => store.toast(error, 'error'));
              onOpenChange(false);
            }}
          >
            {session.pinned ? 'Unpin' : 'Pin'}
          </button>
          {pinned && pinned.position > 0 && (
            <button
              type="button"
              role="menuitem"
              onClick={() => {
                onOpenChange(false);
                pinned.move(-1, 'menu');
              }}
            >
              Move up
            </button>
          )}
          {pinned && pinned.position < pinned.count - 1 && (
            <button
              type="button"
              role="menuitem"
              onClick={() => {
                onOpenChange(false);
                pinned.move(1, 'menu');
              }}
            >
              Move down
            </button>
          )}
          <button
            type="button"
            role="menuitem"
            onClick={() => {
              if (session.archived) {
                void store.archiveSession(session).catch((error) => store.toast(error, 'error'));
              } else onArchive();
              onOpenChange(false);
            }}
          >
            {session.archived ? 'Restore' : 'Archive'}
          </button>
        </div>
      )}
    </div>
  );
}

function SessionRow({
  session,
  showProject = false,
  shortcutEligible = true,
  pinned,
}: {
  session: Session;
  showProject?: boolean;
  shortcutEligible?: boolean;
  pinned?: ReorderControls;
}) {
  const store = useStore();
  const reorderable = Boolean(pinned && pinned.count > 1);
  const row = useRef<HTMLDivElement>(null);
  const [archiving, setArchiving] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  const active = store.activeSessionId.value === session.id;
  const localPendingInteractionCount = useMemo(
    () =>
      computed(
        () =>
          store.interactionOrder.value
            .map((key) => store.interactions.value[key])
            .filter(
              (entry) =>
                entry?.sessionId === session.id &&
                ['waiting', 'dismissed', 'submitting', 'failed'].includes(entry.state),
            ).length,
      ),
    [store, session.id],
  ).value;
  const needsInput = Boolean(session.interactionRequired || localPendingInteractionCount > 0);
  const running = useMemo(
    () =>
      computed(() => {
        const projection = store.runs.value[session.id];
        return (
          Boolean(
            projection &&
            ['connecting', 'checking', 'streaming', 'cancelling'].includes(projection.run.status) &&
            store.runEngine.hasActiveResponseTransport(session.id, projection.run.responseId),
          ) || Boolean(store.services.eventFeedHealthy.value && session.activeRun)
        );
      }),
    [store, session],
  ).value;
  const unseen = !needsInput && !running && Boolean(session.attentionUnseen);
  const attentionLabel = needsInput
    ? localPendingInteractionCount > 1 || (session.pendingInteractionCount || 0) > 1
      ? `${Math.max(localPendingInteractionCount, session.pendingInteractionCount || 0)} decisions waiting`
      : 'Waiting for your input'
    : running
      ? 'Running'
      : unseen
        ? session.attentionOutcome === 'failed'
          ? 'Failed, not yet visited'
          : session.attentionOutcome === 'orphaned'
            ? 'Recovery required, not yet visited'
            : session.attentionOutcome === 'cancelled'
              ? 'Cancelled with output, not yet visited'
              : 'Completed, not yet visited'
        : '';
  const messageCount = sessionMessageCount(session);
  const activityAt = session.lastMessageAt || session.created;
  const archive = () => {
    if (store.showArchived.peek()) {
      void store.archiveSession(session).catch((error) => store.toast(error, 'error'));
      return;
    }
    setArchiving(true);
    const node = row.current;
    let done = false;
    const finish = () => {
      if (done) return;
      done = true;
      node?.removeEventListener('transitionend', onTransitionEnd);
      window.clearTimeout(timeout);
      void store
        .archiveSession(session)
        .then(() => {
          if (store.showArchived.peek()) setArchiving(false);
        })
        .catch((error) => {
          setArchiving(false);
          store.toast(error, 'error');
        });
    };
    const onTransitionEnd = (event: TransitionEvent) => {
      if (event.propertyName === 'max-height') finish();
    };
    node?.addEventListener('transitionend', onTransitionEnd);
    const timeout = window.setTimeout(finish, 500);
  };
  return (
    <div
      ref={row}
      class={`session-row ${session.archived ? 'archived' : ''} ${needsInput ? 'is-input-required' : running ? 'is-active' : ''} ${unseen ? 'is-unseen' : ''} ${menuOpen ? 'menu-open' : ''} ${archiving ? 'is-archiving' : ''} ${reorderable ? 'is-reorderable' : ''} ${pinned?.dragging ? 'is-dragging' : ''}`}
      data-reorder-id={pinned ? session.id : undefined}
    >
      <button
        class={`session-btn ${active ? 'active' : ''}`}
        data-chat-shortcut-eligible={
          shortcutEligible && !session.archived && !archiving ? '' : undefined
        }
        type="button"
        aria-label={`${session.title || session.name || 'New chat'}${attentionLabel ? ` — ${attentionLabel}` : ''}`}
        aria-current={active ? 'page' : undefined}
        aria-keyshortcuts={reorderable ? 'Alt+ArrowUp Alt+ArrowDown' : undefined}
        title={session.longTitle || session.title}
        onMouseDown={(event) => event.preventDefault()}
        // The row itself is the drag surface (its actions menu is not); keyboard
        // users reorder with Alt+Arrow keys or the menu.
        onPointerDown={reorderable ? (event) => pinned!.press(event) : undefined}
        onClick={() => void store.selectSession(session)}
        onKeyDown={(event) => {
          const offset = reorderable ? reorderKeyOffset(event) : 0;
          if (!offset) return;
          event.preventDefault();
          pinned!.move(offset, 'row');
        }}
      >
        <span class="session-title">{session.title || session.name || 'New chat'}</span>
        <span class="session-meta" title={new Date(activityAt).toLocaleString()}>
          {messageCount} {messageCount === 1 ? 'message' : 'messages'} ·{' '}
          {sessionRelativeTime(activityAt)}
          {showProject && (
            <>
              {' · '}
              <span class="session-project-context">{session.projectName || 'Chat'}</span>
            </>
          )}
        </span>
      </button>
      <SessionMenu
        session={session}
        onArchive={archive}
        open={menuOpen}
        onOpenChange={setMenuOpen}
        pinned={reorderable ? pinned : undefined}
      />
    </div>
  );
}

/**
 * The global pinned section. Its order is the server-persisted pinned rank, so
 * activity never reorders it; users drag rows (pointer or touch) or use
 * Alt+Arrow keys or the row menu to move them, and the order is saved.
 */
function PinnedSessions({ sessions, showProject }: { sessions: Session[]; showProject: boolean }) {
  const store = useStore();
  const { list, reordering, announcement, controls } = useReorderableList(
    sessions.map((session) => session.id),
    {
      label: (id) => sessions.find((session) => session.id === id)?.title || 'Conversation',
      save: (orderedIds) => store.reorderPinnedSessions(orderedIds),
      report: (error) => store.toast(error, 'error'),
      focusTargets: { row: '.session-btn', menu: '.session-menu-trigger' },
      scrollContainer: '.sidebar-content',
    },
  );
  return (
    <section class="session-group sidebar-pinned-group">
      <h3>Pinned</h3>
      <div
        ref={list}
        class={`pinned-session-list reorder-list ${reordering ? 'is-reordering' : ''}`}
      >
        {sessions.map((session, position) => (
          <SessionRow
            key={session.id}
            session={session}
            showProject={showProject}
            pinned={controls(session.id, position)}
          />
        ))}
      </div>
      <div class="visually-hidden" role="status" aria-live="polite">
        {announcement}
      </div>
    </section>
  );
}

function SessionDateGroups({
  sessions,
  nested = false,
  showProject = false,
}: {
  sessions: Session[];
  nested?: boolean;
  showProject?: boolean;
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
              <SessionRow key={session.id} session={session} showProject={showProject} />
            ))}
          </section>
        ) : (
          <section class="session-group" key={label}>
            <h3>{label}</h3>
            {entries.map((session) => (
              <SessionRow key={session.id} session={session} showProject={showProject} />
            ))}
          </section>
        );
      })}
    </>
  );
}

const SIDEBAR_EXPANSION_KEYS = {
  noProject: '__no_project__',
  projects: '__projects__',
  hubAgents: '__hub_agents__',
} as const;

function useSidebarExpansion(key: string, animate = true) {
  const store = useStore();
  const [open, setOpen] = useState(
    () =>
      readJSON<Record<string, boolean>>(store.storage, store.keys.projectExpansion, {})[key] !==
      false,
  );
  const [opening, setOpening] = useState(false);
  useEffect(() => {
    if (!opening) return;
    // animationend normally clears this first. The timeout also covers reduced
    // motion and interrupted animations so a later data refresh cannot replay it.
    const timer = window.setTimeout(() => setOpening(false), 250);
    return () => window.clearTimeout(timer);
  }, [opening]);
  const toggle = () => {
    const value = !open;
    setOpen(value);
    setOpening(animate && value);
    const expansion = readJSON<Record<string, boolean>>(
      store.storage,
      store.keys.projectExpansion,
      {},
    );
    writeJSON(store.storage, store.keys.projectExpansion, { ...expansion, [key]: value });
  };
  return {
    open,
    opening,
    toggle,
    finishOpening: () => setOpening(false),
  };
}

function CollapsibleSectionHeading({
  id,
  label,
  open,
  onToggle,
  status,
}: {
  id?: string;
  label: string;
  open: boolean;
  onToggle: () => void;
  status?: ComponentChildren;
}) {
  return (
    <h3 class="collapsible-session-group-heading" id={id}>
      <button
        class="session-group-toggle"
        type="button"
        aria-expanded={open}
        title={`${open ? 'Collapse' : 'Expand'} ${label}`}
        onClick={onToggle}
      >
        <span>{label}</span>
        <span class="session-group-chevron" aria-hidden="true">
          <Icon name="chevron-right" />
        </span>
      </button>
      {status && <span class="collapsible-session-group-status">{status}</span>}
    </h3>
  );
}

function NoProjectGroup({ sessions }: { sessions: Session[] }) {
  const store = useStore();
  const { open, opening, toggle, finishOpening } = useSidebarExpansion(
    SIDEBAR_EXPANSION_KEYS.noProject,
  );
  const activeSession = sessions.find((session) => session.id === store.activeSessionId.value);
  return (
    <section class="session-group session-ungrouped">
      <CollapsibleSectionHeading label="Chat" open={open} onToggle={toggle} />
      {open ? (
        <div
          class={`session-group-list ${opening ? 'is-opening' : ''}`}
          onAnimationEnd={(event) => {
            if (event.target === event.currentTarget) finishOpening();
          }}
        >
          <SessionDateGroups sessions={sessions} nested />
          {store.noProjectCursor.value && (
            <PaginationSentinel load={() => store.loadMoreNoProject()} />
          )}
        </div>
      ) : (
        activeSession && (
          <div class="session-group-collapsed-active">
            <SessionRow
              shortcutEligible={false}
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

function ProjectGroup({ project, order }: { project: Project; order?: ReorderControls }) {
  const store = useStore();
  const reorderable = Boolean(order && order.count > 1);
  const { open, opening, toggle, finishOpening } = useSidebarExpansion(project.id);
  const [menu, setMenu] = useState(false);
  const menuID = useId();
  const { menu: menuRef, up } = useMenuFlip(menu);
  const menuTrigger = useRef<HTMLButtonElement>(null);
  const keyboardMenu = useMenuKeyboard(menu, () => setMenu(false), menuTrigger);
  const listedSessionIDs = new Set(
    (project.sessions || []).filter(isSidebarSessionVisible).map((session) => session.id),
  );
  const sessions = [
    ...(project.sessions || [])
      .filter(isSidebarSessionVisible)
      .map((session) => store.sessionStore.sidebarSessionById.value.get(session.id) || session),
    ...(store.sessionStore.sidebarSessionsByProject.value.get(project.id) || []).filter(
      (session) =>
        session.projectId === project.id &&
        isSidebarSessionVisible(session) &&
        !listedSessionIDs.has(session.id),
    ),
  ].sort(
    (left, right) =>
      Number(right.pinned) - Number(left.pinned) ||
      (right.lastMessageAt || right.created) - (left.lastMessageAt || left.created),
  );
  const regular = sessions.filter((session) => !session.pinned);
  const selected = store.sessionStore.sidebarSessionById.value.get(store.activeSessionId.value);
  const activeSession =
    selected?.projectId === project.id && !selected.pinned
      ? selected
      : regular.find((session) => session.id === store.activeSessionId.value);
  return (
    <section
      class={`project-group ${project.available === false ? 'unavailable' : ''} ${reorderable ? 'is-reorderable' : ''} ${order?.dragging ? 'is-dragging' : ''}`}
      data-project-id={project.id}
      data-reorder-id={order ? project.id : undefined}
    >
      <div class="project-group-header">
        <button
          class="project-group-toggle"
          type="button"
          aria-expanded={open}
          aria-keyshortcuts={reorderable ? 'Alt+ArrowUp Alt+ArrowDown' : undefined}
          title={project.path || `${open ? 'Collapse' : 'Expand'} ${project.name}`}
          // The header is the drag surface for the whole project; its actions
          // menu and conversations are not. Keyboard users reorder with
          // Alt+Arrow keys or the menu.
          onPointerDown={reorderable ? (event) => order!.press(event) : undefined}
          onClick={toggle}
          onKeyDown={(event) => {
            const offset = reorderable ? reorderKeyOffset(event) : 0;
            if (!offset) return;
            event.preventDefault();
            order!.move(offset, 'row');
          }}
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
          aria-controls={menuID}
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
            id={menuID}
            class={`session-menu project-menu open ${up ? 'open-up' : ''}`}
            role="menu"
            aria-label={`Actions for project ${project.name}`}
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
            {reorderable && order!.position > 0 && (
              <button
                type="button"
                role="menuitem"
                onClick={() => {
                  setMenu(false);
                  order!.move(-1, 'menu');
                }}
              >
                Move up
              </button>
            )}
            {reorderable && order!.position < order!.count - 1 && (
              <button
                type="button"
                role="menuitem"
                onClick={() => {
                  setMenu(false);
                  order!.move(1, 'menu');
                }}
              >
                Move down
              </button>
            )}
            <button
              type="button"
              role="menuitem"
              onClick={() => {
                if (
                  project.archived ||
                  confirm(
                    'Archive this project? Conversations remain available when archived sessions are shown.',
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
        <div
          class={`project-session-list ${opening ? 'is-opening' : ''}`}
          onAnimationEnd={(event) => {
            if (event.target === event.currentTarget) finishOpening();
          }}
        >
          <SessionDateGroups sessions={regular} nested />
          {project.has_more && (
            <PaginationSentinel load={() => store.loadMoreProject(project.id)} />
          )}
        </div>
      ) : (
        activeSession && (
          <div class="project-session-list">
            <SessionRow session={activeSession} shortcutEligible={false} />
          </div>
        )
      )}
    </section>
  );
}

/**
 * Projects in their server-persisted order, which conversation activity never
 * changes. Users drag a project by its header (pointer or touch-and-hold) or
 * use Alt+Arrow keys or its menu to move it, and the order is saved. Active
 * and archived projects are separate lists, so a project never lands on the
 * other side of the boundary between them.
 */
function ProjectOrderList({ projects }: { projects: Project[] }) {
  const store = useStore();
  const { list, reordering, announcement, controls } = useReorderableList(
    projects.map((project) => project.id),
    {
      label: (id) => projects.find((project) => project.id === id)?.name || 'Project',
      save: (orderedIds) => store.reorderProjects(orderedIds),
      report: (error) => store.toast(error, 'error'),
      focusTargets: { row: '.project-group-toggle', menu: '.project-group-action' },
      scrollContainer: '.sidebar-content',
    },
  );
  return (
    <>
      <div
        ref={list}
        class={`project-order-list reorder-list ${reordering ? 'is-reordering' : ''}`}
      >
        {projects.map((project, position) => (
          <ProjectGroup key={project.id} project={project} order={controls(project.id, position)} />
        ))}
      </div>
      <div class="visually-hidden" role="status" aria-live="polite">
        {announcement}
      </div>
    </>
  );
}

function ProjectsGroup({ projects }: { projects: Project[] }) {
  const store = useStore();
  const { open, opening, toggle, finishOpening } = useSidebarExpansion(
    SIDEBAR_EXPANSION_KEYS.projects,
  );
  const activeSessionID = store.activeSessionId.value;
  const listedActiveSession = projects
    .flatMap((project) => project.sessions || [])
    .find((session) => session.id === activeSessionID);
  const activeSession =
    store.sessionStore.sidebarSessionById.value.get(activeSessionID) || listedActiveSession;
  const collapsedActiveSession =
    activeSession &&
    !activeSession.pinned &&
    projects.some((project) => project.id === activeSession.projectId)
      ? activeSession
      : undefined;
  const archived = projects.filter((project) => project.archived);
  return (
    <section class="session-group sidebar-project-groups">
      <CollapsibleSectionHeading label="Projects" open={open} onToggle={toggle} />
      {open ? (
        <div
          class={`session-group-list ${opening ? 'is-opening' : ''}`}
          onAnimationEnd={(event) => {
            if (event.target === event.currentTarget) finishOpening();
          }}
        >
          <ProjectOrderList projects={projects.filter((project) => !project.archived)} />
          {archived.length > 0 && <ProjectOrderList projects={archived} />}
        </div>
      ) : (
        collapsedActiveSession && (
          <div class="session-group-collapsed-active">
            <SessionRow session={collapsedActiveSession} shortcutEligible={false} />
          </div>
        )
      )}
    </section>
  );
}

function HubAgents() {
  const store = useStore();
  const { open, toggle } = useSidebarExpansion(SIDEBAR_EXPANSION_KEYS.hubAgents, false);
  const headingID = useId();
  const needsAttention = store.hubAgents.value.some((agent) => agent.attention);
  return (
    <section class="session-group hub-agent-group">
      <CollapsibleSectionHeading
        id={headingID}
        label="Agents"
        open={open}
        onToggle={toggle}
        status={
          !open &&
          needsAttention && (
            <>
              <span class="hub-agent-attention" aria-hidden="true" />
              <span class="visually-hidden">Agents need attention</span>
            </>
          )
        }
      />
      {open && (
        <>
          <nav class="hub-agent-links" aria-labelledby={headingID}>
            {store.hubAgents.value.map((agent) => (
              <a
                class="hub-agent-link"
                key={agent.id}
                href={agent.target}
                aria-current={agent.id === store.config.hub?.nodeId ? 'true' : undefined}
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
          {store.config.hub?.url && (
            <a class="back-to-hub-link" id="backToHubLink" href={store.config.hub.url}>
              <Icon class="sidebar-action-icon" name="arrow-left" />
              <span>Back to Hub</span>
            </a>
          )}
        </>
      )}
    </section>
  );
}

function SidebarViewSwitch({ disabled = false }: { disabled?: boolean }) {
  const store = useStore();
  const view = store.sidebarView.value;
  const [indicatorCycle, setIndicatorCycle] = useState(false);
  const select = (next: 'recent' | 'projects') => {
    setIndicatorCycle((current) => !current);
    store.setSidebarView(next);
  };
  const onKeyDown = (event: KeyboardEvent) => {
    let next: 'recent' | 'projects' | undefined;
    if (event.key === 'ArrowLeft' || event.key === 'Home') next = 'recent';
    if (event.key === 'ArrowRight' || event.key === 'End') next = 'projects';
    if (!next) return;
    event.preventDefault();
    select(next);
    (event.currentTarget as HTMLDivElement)
      .querySelector<HTMLButtonElement>(`[data-sidebar-view="${next}"]`)
      ?.focus();
  };
  return (
    <div
      class="sidebar-view-switch"
      data-view={view}
      data-indicator-cycle={indicatorCycle ? 'alternate' : 'initial'}
      role="tablist"
      aria-label="Conversation view"
      onKeyDown={onKeyDown}
    >
      {(['recent', 'projects'] as const).map((option) => (
        <button
          key={option}
          id={`sidebarView${option === 'recent' ? 'Recent' : 'Projects'}`}
          class={view === option ? 'active' : ''}
          data-sidebar-view={option}
          type="button"
          role="tab"
          aria-selected={view === option}
          aria-disabled={disabled || undefined}
          aria-controls="sessionGroups"
          disabled={disabled}
          tabIndex={view === option ? 0 : -1}
          onClick={() => select(option)}
        >
          {option === 'recent' ? 'Recent' : 'Projects'}
        </button>
      ))}
    </div>
  );
}

function isSidebarSessionVisible(session: Session): boolean {
  // Parentless one-shot and background terminal runs (ask, jobs, goals) stay
  // hidden so a sidebar shared with the terminal does not become an agent
  // process monitor. Interactive terminal chats are first-class sessions and
  // may be continued from the web whenever their turn is free.
  return session.origin !== 'tui' || !session.mode || session.mode === 'chat';
}

export function Sidebar() {
  const store = useStore();
  const collapsed = store.sidebarCollapsed.value;
  const mobileOpen = store.sidebarOpen.value;
  const mobile = useMediaQuery('(max-width: 767px)');
  const overlayRoot = useRef<HTMLDivElement>(null);
  const sidebar = useRef<HTMLElement>(null);
  useChatShortcuts(sidebar);
  const overlayToken = useRef<symbol | null>(null);
  useSwipeDismiss(sidebar, {
    enabled: mobile && mobileOpen,
    axis: 'x',
    direction: -1,
    property: '--panel-swipe-offset-x',
    onDismiss: () => {
      store.sidebarOpen.value = false;
    },
  });
  useEdgeSwipeOpen(sidebar, {
    enabled: mobile && !mobileOpen,
    edge: 'left',
    property: '--panel-swipe-offset-x',
    onOpen: () => {
      store.sidebarOpen.value = true;
    },
  });
  useLayoutEffect(() => {
    if (!mobileOpen || !mobile) return;
    const trigger =
      document.activeElement instanceof HTMLElement ? document.activeElement : undefined;
    overlayToken.current = overlayManager.acquire(trigger, overlayRoot.current);
    const frame = requestAnimationFrame(() => sidebar.current?.focus({ preventScroll: true }));
    return () => {
      cancelAnimationFrame(frame);
      if (overlayToken.current) overlayManager.release(overlayToken.current);
      overlayToken.current = null;
    };
  }, [mobile, mobileOpen]);
  const catalog = store.sidebarSessions.value;
  const projectsEnabled = store.projectsEnabled.value;
  const standalone = catalog.filter(
    (session) => (!projectsEnabled || !session.projectId) && isSidebarSessionVisible(session),
  );
  const sessionByID = new Map(catalog.map((session) => [session.id, session]));
  const recent = store.recentSessions.value
    .map((summary) => sessionByID.get(summary.id) || summary)
    .filter(isSidebarSessionVisible);
  const activeRecent = projectsEnabled
    ? catalog.find((session) => session.id === store.activeSessionId.peek())
    : undefined;
  if (
    activeRecent &&
    isSidebarSessionVisible(activeRecent) &&
    !recent.some((session) => session.id === activeRecent.id)
  )
    recent.push(activeRecent);
  recent.sort(compareSessionsByActivity);
  const sidebarSessions = [
    ...catalog,
    ...store.projects.value.flatMap((project) => project.sessions || []),
  ].filter(isSidebarSessionVisible);
  const results = store.searchResults.value;
  const brand = store.config.title.trim() || displayName(store.config.agentName);
  const sidebarView = projectsEnabled ? store.sidebarView.value : 'recent';
  const visibleForView = projectsEnabled && sidebarView === 'recent' ? recent : sidebarSessions;
  const pinnedIDs = new Set<string>();
  // Both views list pins in their persisted rank, never by activity.
  const pinned = visibleForView
    .filter((session) => {
      if (!session.pinned || pinnedIDs.has(session.id)) return false;
      pinnedIDs.add(session.id);
      return true;
    })
    .sort(compareSessionsByActivity);
  const regular = standalone.filter((session) => !session.pinned);
  const recentRegular = recent.filter((session) => !session.pinned);
  const newChat = () => {
    store.newChat();
    store.sidebarOpen.value = false;
  };
  return (
    <div ref={overlayRoot} class="sidebar-overlay-root">
      <aside
        ref={sidebar}
        class={`sidebar ${collapsed ? 'collapsed' : ''} ${mobileOpen ? 'open' : ''}`}
        id="sidebar"
        aria-label="Sessions"
        role={mobile && mobileOpen ? 'dialog' : undefined}
        aria-modal={(mobile && mobileOpen) || undefined}
        tabIndex={mobile && mobileOpen ? -1 : undefined}
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
          if (overlayToken.current) trapOverlayFocus(event);
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
                class="icon-btn close-button sidebar-close"
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
              {store.hubAgents.value.length > 0 && <HubAgents />}
            </div>
            <div
              class="session-groups"
              id="sessionGroups"
              role={projectsEnabled && !results ? 'tabpanel' : undefined}
              aria-labelledby={
                projectsEnabled && !results
                  ? sidebarView === 'recent'
                    ? 'sidebarViewRecent'
                    : 'sidebarViewProjects'
                  : undefined
              }
            >
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
                    <PinnedSessions
                      sessions={pinned}
                      showProject={projectsEnabled && sidebarView === 'recent'}
                    />
                  )}
                  {projectsEnabled ? (
                    sidebarView === 'recent' ? (
                      <div class="flat-session-date-groups sidebar-recent-groups">
                        <SessionDateGroups sessions={recentRegular} showProject />
                        {store.recentCursor.value && (
                          <PaginationSentinel load={() => store.loadMoreRecent()} />
                        )}
                      </div>
                    ) : (
                      <>
                        {store.projects.value.length > 0 && (
                          <ProjectsGroup projects={store.projects.value} />
                        )}
                        {regular.length > 0 && <NoProjectGroup sessions={regular} />}
                      </>
                    )
                  ) : (
                    regular.length > 0 && (
                      <div class="flat-session-date-groups">
                        <SessionDateGroups sessions={regular} />
                        {store.noProjectCursor.value && (
                          <PaginationSentinel load={() => store.loadMoreNoProject()} />
                        )}
                      </div>
                    )
                  )}
                </>
              )}
            </div>
          </div>
          {projectsEnabled && (
            <div class="sidebar-view-footer">
              <SidebarViewSwitch disabled={Boolean(results)} />
            </div>
          )}
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
    </div>
  );
}
