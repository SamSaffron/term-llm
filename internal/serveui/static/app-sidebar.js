(() => {
'use strict';

const app = window.TermLLMApp;
const createEl = app.createEl;
const { UI_PREFIX, state, elements } = app;

const SEARCH_DEBOUNCE_MS = 180;
const NO_PROJECT_SELECTION = '__no_project__';
let searchTimer = null;
let searchAbort = null;
let searchSeq = 0;

const widgetTitle = (widget) => String(widget?.title || widget?.mount || widget?.id || 'Widget');
const widgetMount = (widget) => String(widget?.mount || widget?.id || '').replace(/^\/+|\/+$/g, '');

const buildWidgetLink = (widget) => {
  const mount = widgetMount(widget);
  const link = createEl('a', 'widget-link');
  link.href = `${UI_PREFIX}/widgets/${encodeURIComponent(mount)}/`;
  link.title = widget.description || widgetTitle(widget);

  const titleRow = createEl('div', 'widget-title-row');

  const title = createEl('span', 'widget-title', widgetTitle(widget));

  const normalizedStatus = String(widget.state || 'stopped').toLowerCase();
  const statusClass = normalizedStatus.replace(/[^a-z0-9_-]/g, '');
  const showRunningIndicator = statusClass === 'running' || statusClass === 'starting' || statusClass === 'started';
  const showTextBadge = statusClass && statusClass !== 'stopped' && !showRunningIndicator;

  titleRow.appendChild(title);
  if (showRunningIndicator) {
    const stateBadge = createEl('span', `widget-state ${statusClass}`);
    stateBadge.title = 'Running';
    stateBadge.setAttribute('aria-label', 'Running');
    titleRow.appendChild(stateBadge);
  } else if (showTextBadge) {
    const stateBadge = createEl('span', `widget-state ${statusClass}`, normalizedStatus);
    titleRow.appendChild(stateBadge);
  }
  link.appendChild(titleRow);
  const meta = createEl('div', 'widget-meta', widget.description || mount);
  link.appendChild(meta);

  return link;
};

const renderWidgetSidebar = () => {
  const widgets = Array.isArray(state.widgets) ? state.widgets.filter((widget) => widgetMount(widget)) : [];
  const shouldShow = state.showWidgetsSidebar !== false && state.widgetsLoaded && widgets.length > 0;

  elements.widgetsOpenBtn?.classList.toggle('hidden', !shouldShow);

  if (!shouldShow) {
    elements.widgetsModalList?.replaceChildren();
    elements.widgetsModal?.classList.add('hidden');
    return;
  }

  const rows = widgets.map(buildWidgetLink);
  elements.widgetsModalList?.replaceChildren(...rows);
};

const searchResultToSession = (result) => {
  const id = String(result.id || result.session_id || '');
  if (!id) return null;
  const created = Number(result.created_at || 0) || Date.now();
  const lastMessageAt = Number(result.last_message_at || 0) || created;
  return {
    id,
    number: Number(result.number || result.session_number || 0) || 0,
    name: String(result.name || ''),
    title: String(result.short_title || result.session_name || result.summary || 'New chat'),
    longTitle: String(result.long_title || result.short_title || result.session_name || ''),
    mode: String(result.mode || 'chat'),
    origin: String(result.origin || 'tui'),
    archived: Boolean(result.archived),
    pinned: Boolean(result.pinned),
    created,
    lastMessageAt,
    messageCount: Number(result.message_count || 0) || 0,
    lastResponseId: null,
    activeResponseId: null,
    projectId: String(result.project_id || ''),
    projectName: String(result.project_name || ''),
    _serverOnly: true,
    searchSnippet: String(result.snippet || result.summary || '')
  };
};

const runSidebarSearch = async (query, seq) => {
  if (searchAbort) searchAbort.abort();
  searchAbort = new AbortController();

  const params = new URLSearchParams();
  params.set('q', query);
  params.set('limit', '30');
  const categories = state.sidebarSessionCategories;
  if (Array.isArray(categories) && categories.length > 0 && !categories.includes('all')) {
    params.set('categories', categories.join(','));
  }
  if (state.showHiddenSessions) params.set('include_archived', '1');

  try {
    const headers = app.requestHeaders ? app.requestHeaders('') : {};
    const resp = await app.apiFetch(`${UI_PREFIX}/v1/sessions/search?${params.toString()}`, {
      headers,
      signal: searchAbort.signal
    });
    if (!resp.ok) throw new Error(`search failed (${resp.status})`);
    const data = await resp.json();
    if (seq !== searchSeq) return;
    const results = Array.isArray(data.sessions)
      ? data.sessions.map(searchResultToSession).filter(Boolean)
      : [];
    const byID = new Map((state.sessions || []).map((session) => [session.id, session]));
    results.forEach((incoming) => {
      const existing = byID.get(incoming.id);
      if (existing) Object.assign(existing, incoming);
      else { state.sessions.push(incoming); byID.set(incoming.id, incoming); }
    });
    state.sidebarSearchResults = results;
    state.sidebarSearchLoading = false;
    state.sidebarSearchError = '';
    app.renderSidebar?.();
  } catch (err) {
    if (err?.name === 'AbortError' || seq !== searchSeq) return;
    state.sidebarSearchResults = null;
    state.sidebarSearchLoading = false;
    state.sidebarSearchError = 'Could not search conversations';
    app.renderSidebar?.();
  }
};

const scheduleSidebarSearch = () => {
  const query = String(elements.sidebarSearchInput?.value || '').trim();
  state.sidebarSearchQuery = query;
  state.sidebarSearchError = '';
  searchSeq += 1;
  const seq = searchSeq;
  if (searchTimer !== null) clearTimeout(searchTimer);
  if (searchAbort) searchAbort.abort();

  if (!query) {
    state.sidebarSearchResults = null;
    state.sidebarSearchLoading = false;
    app.renderSidebar?.();
    return;
  }

  state.sidebarSearchLoading = true;
  state.sidebarSearchResults = [];
  app.renderSidebar?.();
  searchTimer = setTimeout(() => {
    searchTimer = null;
    void runSidebarSearch(query, seq);
  }, SEARCH_DEBOUNCE_MS);
};

const openWidgetsModal = () => {
  renderWidgetSidebar();
  elements.widgetsModal?.classList.remove('hidden');
  elements.widgetsModalCloseBtn?.focus?.();
};

const closeWidgetsModal = () => {
  elements.widgetsModal?.classList.add('hidden');
};

const projectSessionFromSummary = (item, project = null) => {
  const created = Date.parse(item.created_at || '') || Number(item.created_at || 0) || Date.now();
  const last = Date.parse(item.last_message_at || '') || Number(item.last_message_at || 0) || created;
  return {
    id: String(item.id || ''), number: Number(item.number || 0), name: String(item.name || ''),
    title: String(item.generated_short_title || item.name || item.summary || 'New chat'),
    longTitle: String(item.generated_long_title || item.generated_short_title || item.summary || ''),
    mode: String(item.mode || 'chat'), origin: String(item.origin || 'web'),
    archived: Boolean(item.archived), pinned: Boolean(item.pinned), created, lastMessageAt: last,
    status: String(item.status || 'active'), transcriptRev: Number(item.transcript_rev || 0),
    messageCount: Number(item.message_count || 0), provider: String(item.provider_key || item.provider || ''),
    worktreeDir: String(item.worktree_dir || ''), projectId: String(item.project_id || project?.id || ''),
    projectName: String(item.project_name || project?.name || ''),
    projectUnavailable: Boolean(project && !project.available),
    projectUnavailableReason: String(project?.unavailable_reason || ''),
    _serverOnly: true,
  };
};

const activateProjectDialog = (backdrop, dialog, returnFocus) => {
  const shell = elements.appShell;
  if (shell) { shell.inert = true; shell.setAttribute?.('inert', ''); }
  const close = () => {
    dialog.removeEventListener?.('keydown', onKeyDown);
    backdrop.remove();
    if (shell) { shell.inert = false; shell.removeAttribute?.('inert'); }
    returnFocus?.focus?.();
  };
  const onKeyDown = (event) => {
    if (event.key === 'Escape') { event.preventDefault?.(); close(); return; }
    if (event.key !== 'Tab') return;
    const focusable = Array.from(dialog.querySelectorAll?.('button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])') || []);
    if (!focusable.length) { event.preventDefault?.(); dialog.focus?.(); return; }
    const first = focusable[0]; const last = focusable[focusable.length - 1];
    if (event.shiftKey && (document.activeElement === first || !dialog.contains?.(document.activeElement))) { event.preventDefault?.(); last.focus?.(); }
    else if (!event.shiftKey && (document.activeElement === last || !dialog.contains?.(document.activeElement))) { event.preventDefault?.(); first.focus?.(); }
  };
  dialog.addEventListener('keydown', onKeyDown);
  backdrop.addEventListener('click', (event) => { if (event.target === backdrop) close(); });
  return close;
};

const clearProjectDraftStorage = () => {
  try {
    const records = JSON.parse(localStorage.getItem(app.STORAGE_KEYS.draftMessages) || '[]');
    if (Array.isArray(records)) {
      localStorage.setItem(app.STORAGE_KEYS.draftMessages, JSON.stringify(records.filter((record) => !String(record?.sessionId || '').startsWith('draft:'))));
    }
  } catch (_) {}
  state.activeProjectId = '';
  state.lastProjectId = '';
  state.projectDrafts = {};
  state.projectAttachments = {};
  state.selectedWorktreeDir = '';
  state.selectedWorktreeName = '';
  state.worktrees = [];
  localStorage.removeItem(app.STORAGE_KEYS.lastProject);
  if (state.draftSessionActive) {
    state.draftSessionActive = false;
    localStorage.removeItem(app.STORAGE_KEYS.draftSessionActive);
    elements.promptInput.value = '';
    app.autoGrowPrompt?.();
  }
};

const loadCapabilities = async () => {
  state.capabilitiesRequired = true;
  state.projectsError = '';
  app.renderSidebar?.();
  app.renderWorktreeChip?.();
  app.updateSendButtonState?.();
  try {
    const response = await app.apiFetch(`${UI_PREFIX}/v1/capabilities`, { headers: app.requestHeaders('') });
    if (!response.ok) throw new Error('Could not load server capabilities');
    const data = await response.json();
    const enabled = Boolean(data?.projects?.enabled);
    const worktreesEnabled = Boolean(data?.worktrees?.enabled);
    const hadProjectState = state.projectsEnabled || Boolean(state.lastProjectId) ||
      Object.keys(state.projectDrafts || {}).length > 0;
    if (!enabled && hadProjectState) {
      clearProjectDraftStorage();
    }
    state.capabilitiesLoaded = true;
    state.projectsEnabled = enabled;
    state.worktreesEnabled = worktreesEnabled;
    state.projectsError = '';
    app.updateSendButtonState?.();
    app.renderSidebar?.();
    app.renderWorktreeChip?.();
    return enabled;
  } catch (err) {
    state.capabilitiesLoaded = false;
    state.worktreesEnabled = false;
    state.projectsError = 'Could not load projects and conversations';
    app.updateSendButtonState?.();
    app.renderSidebar?.();
    app.renderWorktreeChip?.();
    return false;
  }
};

const loadProjectSidebar = async (options = {}) => {
  if (!state.projectsEnabled) return [];
  state.projectsError = '';
  try {
    const refreshStatus = options?.refreshStatus ? '&refresh_status=1' : '';
    const response = await app.apiFetch(`${UI_PREFIX}/v1/sidebar?per_project=12&include_archived_projects=1&include_archived_sessions=${state.showHiddenSessions ? '1' : '0'}${refreshStatus}`, { headers: app.requestHeaders('') });
    if (response.status === 404) {
      const payload = await response.json().catch(() => ({}));
      if (payload?.error?.code === 'projects_disabled') {
        state.projectsEnabled = false;
        clearProjectDraftStorage();
        app.renderSidebar?.();
        app.renderWorktreeChip?.();
        return [];
      }
    }
    if (!response.ok) throw new Error('Could not load projects and conversations');
    const payload = await response.json();
    state.sidebarGroups = Array.isArray(payload.groups) ? payload.groups : [];
    state.projects = state.sidebarGroups.map((group) => group.project).filter(Boolean);
    if (state.lastProjectId && state.lastProjectId !== NO_PROJECT_SELECTION) {
      const remembered = state.projects.find((project) => project.id === state.lastProjectId);
      if (!remembered || remembered.archived_at || !remembered.available) {
        delete state.projectDrafts[state.lastProjectId];
        state.lastProjectId = '';
        localStorage.removeItem(app.STORAGE_KEYS.lastProject);
      }
    }
    const groupedSessions = [];
    state.sidebarGroups.forEach((group) => {
      (Array.isArray(group.sessions) ? group.sessions : []).forEach((item) => groupedSessions.push(projectSessionFromSummary(item, group.project)));
    });
    const byId = new Map(state.sessions.map((session) => [session.id, session]));
    groupedSessions.forEach((incoming) => {
      const existing = byId.get(incoming.id);
      if (existing) Object.assign(existing, incoming);
      else { state.sessions.push(incoming); byId.set(incoming.id, incoming); }
    });
    app.renderSidebar?.();
    app.renderWorktreeChip?.();
    app.updateSendButtonState?.();
    return state.sidebarGroups;
  } catch (err) {
    state.projectsError = 'Could not load projects and conversations';
    app.renderSidebar?.();
    return [];
  }
};

const projectExpanded = (id) => state.projectExpansion[id] !== false;
const projectExpansionAnimations = new Set();

const persistProjectExpansion = () => {
  localStorage.setItem(app.STORAGE_KEYS.projectExpansion, JSON.stringify(state.projectExpansion));
};

const openAssignProjectModal = (conversation) => app.openAssignProjectDialog(conversation, {
  activateDialog: activateProjectDialog,
  onAssigned: loadProjectSidebar,
});

const renderProjectSessionRow = (session) => {
  const row = app.sidebarSessionRow?.(session) || createEl('div', 'session-row');
  row.classList.toggle('project-active-row', session.id === state.activeSessionId);
  row.classList.toggle('project-unavailable-row', Boolean(session.projectUnavailable));
  const existingUnavailable = row.querySelector?.('.project-session-unavailable');
  if (session.projectUnavailable && !existingUnavailable) {
    const unavailable = createEl('span', 'project-session-unavailable', 'Project unavailable');
    unavailable.setAttribute('role', 'status');
    unavailable.title = session.projectUnavailableReason || 'Project unavailable';
    row.appendChild(unavailable);
  } else if (!session.projectUnavailable) {
    existingUnavailable?.remove();
  }
  return row;
};

let projectPaginationObserver = null;

const disconnectProjectPagination = () => {
  projectPaginationObserver?.disconnect?.();
  projectPaginationObserver = null;
};

const observeProjectPaginationSentinel = (sentinel) => {
  if (!sentinel) return;
  if (typeof IntersectionObserver !== 'function') {
    setTimeout(() => sentinel._loadMore?.(), 0);
    return;
  }
  if (!projectPaginationObserver) {
    projectPaginationObserver = new IntersectionObserver((entries) => {
      entries.forEach((entry) => {
        if (!entry.isIntersecting) return;
        projectPaginationObserver?.unobserve?.(entry.target);
        entry.target?._loadMore?.();
      });
    }, { root: elements.sidebarContent || null, rootMargin: '160px 0px' });
  }
  projectPaginationObserver.observe(sentinel);
};

const loadMoreProjectSessions = async (group, list, sentinel) => {
  if (!group?.next_cursor || sentinel?._loading) return;
  sentinel._loading = true;
  sentinel.classList.add('loading');
  sentinel.setAttribute('aria-label', 'Loading older conversations');
  try {
    const projectParam = group.project?.id ? `project_id=${encodeURIComponent(group.project.id)}&` : '';
    const response = await app.apiFetch(`${UI_PREFIX}/v1/sessions?${projectParam}cursor=${encodeURIComponent(group.next_cursor)}&limit=12&include_archived=${state.showHiddenSessions ? '1' : '0'}`, { headers: app.requestHeaders('') });
    if (!response.ok) throw new Error('Could not load more conversations');
    const payload = await response.json();
    sentinel.remove();
    (payload.sessions || []).forEach((item) => {
      const session = projectSessionFromSummary(item, group.project);
      if (!state.sessions.some((existing) => existing.id === session.id)) state.sessions.push(session);
      group.sessions.push(item);
      const row = renderProjectSessionRow(session); row.setAttribute('role', 'listitem'); list.appendChild(row);
    });
    group.next_cursor = String(payload.next_cursor || '');
    const listIsMounted = !('isConnected' in list) || list.isConnected;
    if (group.next_cursor && listIsMounted) appendProjectPaginationSentinel(group, list);
  } catch (_err) {
    sentinel._loading = false;
    sentinel.classList.remove('loading');
    sentinel.classList.add('error');
    sentinel.textContent = 'Couldn’t load older conversations';
    sentinel.setAttribute('aria-label', sentinel.textContent);
    setTimeout(() => {
      const mounted = !('isConnected' in sentinel) || sentinel.isConnected;
      if (!mounted || !group.next_cursor) return;
      sentinel.classList.remove('error');
      sentinel.textContent = '';
      observeProjectPaginationSentinel(sentinel);
    }, 5000);
  }
};

const projectPaginationSentinel = (group, list) => {
  const sentinel = createEl('div', 'project-pagination-sentinel');
  sentinel.setAttribute('role', 'status');
  sentinel.setAttribute('aria-label', 'More conversations load automatically');
  sentinel._loadMore = () => loadMoreProjectSessions(group, list, sentinel);
  return sentinel;
};

const appendProjectPaginationSentinel = (group, list) => {
  const sentinel = projectPaginationSentinel(group, list);
  list.appendChild(sentinel);
  observeProjectPaginationSentinel(sentinel);
  return sentinel;
};

const openProjectManageModal = (project, initialAction = 'rename', returnFocusOverride = null) => {
  document.getElementById('projectManageModal')?.remove();
  const backdrop = createEl('div', 'project-modal-backdrop'); backdrop.id = 'projectManageModal';
  const dialog = createEl('div', 'project-modal project-manage-modal'); dialog.setAttribute('role', 'dialog'); dialog.setAttribute('aria-modal', 'true'); dialog.setAttribute('aria-labelledby', 'projectManageTitle');
  dialog.appendChild(createEl('div', 'project-modal-handle'));

  const header = createEl('div', 'project-modal-header');
  const heading = createEl('div', 'project-modal-heading');
  const title = createEl('h2', '', 'Manage project'); title.id = 'projectManageTitle';
  const explanation = createEl('p', '', project.archived_at ? 'This project is archived. Restore it to start new chats here.' : 'Rename this project, or archive it to hide it from new chats.');
  const closeButton = createEl('button', 'project-modal-close', '×'); closeButton.type = 'button'; closeButton.setAttribute('aria-label', 'Close manage project');
  heading.append(title, explanation); header.append(heading, closeButton);

  const fields = createEl('div', 'project-modal-fields');
  const identity = createEl('section', 'project-manage-identity');
  const identityTop = createEl('div', 'project-field-label-row');
  identityTop.appendChild(createEl('span', 'project-field-label', 'Folder'));
  const stateLabel = project.archived_at ? 'Archived' : (project.available ? 'Available' : 'Unavailable');
  const stateClass = project.archived_at ? 'is-archived' : (project.available ? 'is-available' : 'is-unavailable');
  identityTop.appendChild(createEl('span', `project-manage-state ${stateClass}`, stateLabel));
  const projectPath = createEl('code', 'project-manage-path', project.canonical_dir || ''); projectPath.title = project.canonical_dir || '';
  const conversations = Number(project.conversation_count || 0);
  const metadata = createEl('div', 'project-field-hint', `${project.git ? 'Git repository' : 'Directory'} · ${conversations} conversation${conversations === 1 ? '' : 's'}`);
  identity.append(identityTop, projectPath, metadata);

  const nameField = createEl('div', 'project-field');
  const nameLabel = createEl('label', 'project-field-label', 'Display name'); nameLabel.setAttribute('for', 'projectManageNameInput');
  const name = createEl('input', 'project-input'); name.id = 'projectManageNameInput'; name.value = project.name || ''; name.setAttribute('aria-label', 'Project display name'); name.setAttribute('aria-describedby', 'projectManageNameHint');
  const nameHint = createEl('div', 'project-field-hint', 'Shown in the sidebar; the folder on disk is unchanged.'); nameHint.id = 'projectManageNameHint';
  nameField.append(nameLabel, name, nameHint);

  const danger = createEl('section', `project-manage-danger${project.archived_at ? ' is-restore' : ''}`);
  const dangerCopy = createEl('div', 'project-manage-danger-copy');
  dangerCopy.append(createEl('span', 'project-field-label', project.archived_at ? 'Restore project' : 'Archive project'));
  const archiveHint = createEl('p', 'project-field-hint', project.archived_at ? 'Restoring makes this project available for new chats.' : 'Archiving hides this project from new chats; existing conversations keep working.'); archiveHint.id = 'projectManageArchiveHint';
  dangerCopy.appendChild(archiveHint);
  const archive = createEl('button', project.archived_at ? 'btn project-manage-archive' : 'btn danger-quiet project-manage-archive', project.archived_at ? 'Restore project' : 'Archive project'); archive.type = 'button'; archive.setAttribute('aria-describedby', archiveHint.id);
  const warning = createEl('p', 'project-manage-warning', 'Archive this project? You can restore it later.'); warning.hidden = true; warning.setAttribute('role', 'alert');
  danger.append(dangerCopy, archive, warning);
  fields.append(identity, nameField, danger);

  const status = createEl('div', 'project-modal-status'); status.setAttribute('aria-live', 'polite');
  const actions = createEl('div', 'project-modal-actions');
  const cancel = createEl('button', 'btn', 'Cancel'); cancel.type = 'button';
  const save = createEl('button', 'btn primary', 'Save name'); save.type = 'button';
  const footer = createEl('div', 'project-modal-footer'); actions.append(cancel, save); footer.append(status, actions);
  dialog.append(header, fields, footer); backdrop.appendChild(dialog); document.body.appendChild(backdrop);

  const setStatus = (message = '', error = false) => {
    status.textContent = message; status.classList.toggle('is-error', error);
    if (error) status.setAttribute('role', 'alert'); else status.removeAttribute('role');
  };
  const returnFocus = returnFocusOverride || document.activeElement;
  const close = activateProjectDialog(backdrop, dialog, returnFocus);
  closeButton.addEventListener('click', close); cancel.addEventListener('click', close);
  const saveName = async () => {
    save.disabled = true; save.classList.add('is-loading'); save.textContent = 'Saving…'; setStatus();
    try {
      const response = await app.apiFetch(`${UI_PREFIX}/v1/projects/${encodeURIComponent(project.id)}`, { method: 'PATCH', headers: app.requestHeaders(''), body: JSON.stringify({ name: name.value }) });
      if (!response.ok) { const payload = await response.json().catch(() => ({})); throw new Error(payload?.error?.message || 'Could not rename project'); }
      close(); await loadProjectSidebar();
    } catch (err) { setStatus(err?.message || 'Could not rename project. Retry.', true); }
    finally { save.disabled = false; save.classList.remove('is-loading'); save.textContent = 'Save name'; }
  };
  save.addEventListener('click', saveName);
  name.addEventListener('keydown', async (event) => { if (event.key === 'Enter') { event.preventDefault?.(); await saveName(); } });

  let archiveArmed = Boolean(project.archived_at);
  archive.addEventListener('click', async () => {
    if (!project.archived_at && !archiveArmed) {
      archiveArmed = true; archive.textContent = 'Confirm archive'; archive.classList.remove('danger-quiet'); archive.classList.add('danger'); warning.hidden = false; archive.setAttribute('aria-describedby', `${archiveHint.id} projectManageArchiveWarning`); warning.id = 'projectManageArchiveWarning';
      return;
    }
    archive.disabled = true; archive.classList.add('is-loading'); setStatus();
    try {
      const response = await app.apiFetch(`${UI_PREFIX}/v1/projects/${encodeURIComponent(project.id)}`, { method: 'PATCH', headers: app.requestHeaders(''), body: JSON.stringify({ archived: !Boolean(project.archived_at) }) });
      if (!response.ok) { const payload = await response.json().catch(() => ({})); throw new Error(payload?.error?.message || 'Could not update project'); }
      close(); await loadProjectSidebar();
    } catch (err) { setStatus(err?.message || 'Could not update project. Retry.', true); }
    finally { archive.disabled = false; archive.classList.remove('is-loading'); }
  });
  if (initialAction === 'archive') archive.focus(); else { name.focus(); name.select(); }
};

let activeProjectContextMenu = null;
let activeProjectContextAnchor = null;

const closeProjectMenu = (restoreFocus = false) => {
  if (!activeProjectContextMenu) return;
  activeProjectContextMenu.remove();
  activeProjectContextMenu = null;
  activeProjectContextAnchor?.setAttribute?.('aria-expanded', 'false');
  if (restoreFocus) activeProjectContextAnchor?.focus?.();
  activeProjectContextAnchor = null;
};

const openProjectMenu = (project, anchor) => {
  if (activeProjectContextMenu && activeProjectContextAnchor === anchor) {
    closeProjectMenu(true);
    return;
  }
  closeProjectMenu(false);
  const menu = createEl('div', 'project-context-menu');
  menu.id = 'projectContextMenu';
  menu.setAttribute('role', 'menu');
  menu.setAttribute('aria-label', `Manage ${project.name}`);
  activeProjectContextMenu = menu;
  activeProjectContextAnchor = anchor;
  anchor.setAttribute('aria-expanded', 'true');
  const details = createEl('div', 'project-menu-details', project.canonical_dir || '');
  details.title = project.canonical_dir || '';
  details.setAttribute('role', 'presentation');
  menu.appendChild(details);
  const archived = Boolean(project.archived_at);
  if (project.available && !archived) {
    const newChat = createEl('button', '', 'New chat'); newChat.type = 'button'; newChat.setAttribute('role', 'menuitem'); newChat.addEventListener('click', () => { closeProjectMenu(false); app.createAndSwitchToFreshSession?.(project.id); }); menu.appendChild(newChat);
  }
  const rename = createEl('button', '', 'Rename'); rename.type = 'button'; rename.setAttribute('role', 'menuitem');
  rename.addEventListener('click', () => { closeProjectMenu(false); openProjectManageModal(project, 'rename', anchor); });
  const archive = createEl('button', '', archived ? 'Restore' : 'Archive'); archive.type = 'button'; archive.setAttribute('role', 'menuitem');
  archive.addEventListener('click', () => { closeProjectMenu(false); openProjectManageModal(project, 'archive', anchor); });
  menu.append(rename, archive);
  if (project.git && project.available) {
    const worktrees = createEl('button', '', 'Worktrees'); worktrees.type = 'button'; worktrees.setAttribute('role', 'menuitem');
    worktrees.addEventListener('click', async () => { closeProjectMenu(false); await app.openWorktreeMenuForProject?.(project.id, anchor); });
    menu.appendChild(worktrees);
  }
  menu.addEventListener('keydown', (event) => {
    const items = Array.from(menu.querySelectorAll?.('[role="menuitem"]') || []);
    const current = items.indexOf(document.activeElement);
    if (event.key === 'Escape') { event.preventDefault?.(); closeProjectMenu(true); return; }
    if (event.key === 'Tab') { closeProjectMenu(false); return; }
    let next = -1;
    if (event.key === 'ArrowDown') next = current < 0 ? 0 : (current + 1) % items.length;
    else if (event.key === 'ArrowUp') next = current < 0 ? items.length - 1 : (current - 1 + items.length) % items.length;
    else if (event.key === 'Home') next = 0;
    else if (event.key === 'End') next = items.length - 1;
    if (next >= 0) { event.preventDefault?.(); items[next]?.focus?.(); }
  });
  (anchor.parentElement || anchor.parentNode)?.appendChild(menu);
  (menu.querySelector?.('button') || rename).focus();
};

document.addEventListener('click', (event) => {
  if (!activeProjectContextMenu) return;
  if (activeProjectContextMenu.contains?.(event.target) || activeProjectContextAnchor?.contains?.(event.target)) return;
  closeProjectMenu(false);
});
document.addEventListener('keydown', (event) => {
  if (event.key !== 'Escape' || !activeProjectContextMenu) return;
  event.preventDefault?.();
  closeProjectMenu(true);
});

const projectHeadingLabel = (project) => {
  if (!project) return 'Chat';
  const duplicate = (state.projects || []).filter((candidate) => String(candidate.name || '').localeCompare(String(project.name || ''), undefined, { sensitivity: 'accent' }) === 0).length > 1;
  if (!duplicate) return project.name || 'Project';
  const parts = String(project.canonical_dir || '').split(/[\\/]+/).filter(Boolean); const suffix = parts.slice(-2).join('/');
  return suffix ? `${project.name} — ${suffix}` : project.name;
};
const projectGroupSessions = (group) => {
  const project = group.project || null;
  const canonicalByID = new Map((state.sessions || []).map((session) => [session.id, session]));
  let sessions = (group.sessions || []).map((item) => {
    const projected = projectSessionFromSummary(item, project);
    const canonical = canonicalByID.get(projected.id);
    return canonical ? { ...projected, ...canonical, projectUnavailable: Boolean(project && !project.available), projectUnavailableReason: String(project?.unavailable_reason || '') } : projected;
  });
  if (!state.sidebarSearchQuery) {
    const known = new Set(sessions.map((session) => session.id)); (state.sessions || []).forEach((session) => { if (String(session.projectId || '') === String(project?.id || '') && !known.has(session.id)) sessions.push(session); });
  }
  if (state.sidebarSearchQuery && Array.isArray(state.sidebarSearchResults) && !state.sidebarSearchError) sessions = state.sidebarSearchResults.filter((item) => String(item.projectId || '') === String(project?.id || ''));
  return sessions.sort((a, b) => (b.lastMessageAt || b.created) - (a.lastMessageAt || a.created));
};
const renderPinnedSessions = (groups) => {
  const seen = new Set(); const sessions = groups.flatMap(projectGroupSessions).filter((session) => { if (!session.pinned || seen.has(session.id)) return false; seen.add(session.id); return true; });
  if (!sessions.length) return null;
  const section = createEl('section', 'session-group pinned-sessions'); section.appendChild(createEl('h3', '', 'Pinned'));
  sessions.sort((a, b) => (b.lastMessageAt || b.created) - (a.lastMessageAt || a.created)).forEach((session) => section.appendChild(renderProjectSessionRow(session)));
  return section;
};
const renderProjectGroup = (group) => {
  const project = group.project || null;
  const id = project?.id || '__no_project__';
  const activeProjectID = state.sessions.find((session) => session.id === state.activeSessionId)?.projectId;
  const active = state.draftSessionActive
    ? Boolean(project && String(state.activeProjectId || '') === String(project.id || ''))
    : String(activeProjectID || '') === String(project?.id || '');
  const expanded = projectExpanded(id);
  const section = createEl('section', `project-group${active ? ' active' : ''}${project?.archived_at ? ' archived' : ''}${project && !project.available ? ' unavailable' : ''}`); section.dataset.projectId = String(project?.id || '');
  const headingId = `project-heading-${String(id).replace(/[^a-zA-Z0-9_-]/g, '-')}`; const header = createEl('div', 'project-group-header'); const headingLabel = projectHeadingLabel(project);
  const toggle = createEl('button', 'project-group-toggle'); toggle.type = 'button'; toggle.id = headingId;
  toggle.setAttribute('aria-expanded', String(expanded));
  toggle.setAttribute('aria-label', `${expanded ? 'Collapse' : 'Expand'} ${headingLabel}${project?.canonical_dir ? ` — ${project.canonical_dir}` : ''}`);
  toggle.title = project?.canonical_dir || `${expanded ? 'Collapse' : 'Expand'} ${headingLabel}`;
  const label = createEl('span', 'project-group-label', headingLabel);
  const chevron = createEl('span', 'project-group-chevron');
  chevron.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m9 18 6-6-6-6"/></svg>';
  toggle.append(label, chevron);
  toggle.addEventListener('click', (event) => {
    event.preventDefault?.();
    const nextExpanded = !projectExpanded(id);
    state.projectExpansion[id] = nextExpanded;
    if (nextExpanded) projectExpansionAnimations.add(id);
    else projectExpansionAnimations.delete(id);
    persistProjectExpansion();
    app.renderSidebar?.();
  });
  header.appendChild(toggle);
  if (project) {
    if (!project.available) {
      const retry = createEl('button', 'project-group-action', 'Retry'); retry.type = 'button'; retry.setAttribute('aria-label', `Retry ${project.name} status`);
      retry.addEventListener('click', () => loadProjectSidebar({ refreshStatus: true })); header.appendChild(retry);
    } else if (project.archived_at) {
      const restore = createEl('button', 'project-group-action', 'Restore'); restore.type = 'button'; restore.setAttribute('aria-label', `Restore ${project.name}`);
      restore.addEventListener('click', async () => {
        restore.disabled = true;
        try {
          const response = await app.apiFetch(`${UI_PREFIX}/v1/projects/${encodeURIComponent(project.id)}`, { method: 'PATCH', headers: app.requestHeaders(''), body: JSON.stringify({ archived: false }) });
          if (!response.ok) throw new Error('Could not restore project. Retry.');
          await loadProjectSidebar();
        } catch (err) {
          state.projectsError = err?.message || 'Could not restore project. Retry.';
          app.renderSidebar?.();
        } finally { restore.disabled = false; }
      });
      header.appendChild(restore);
    }
    const menu = createEl('button', 'project-group-action', '⋯'); menu.type = 'button'; menu.setAttribute('aria-label', `Manage ${project.name}`);
    menu.setAttribute('aria-haspopup', 'menu'); menu.setAttribute('aria-expanded', 'false'); menu.setAttribute('aria-controls', 'projectContextMenu');
    menu.addEventListener('click', () => openProjectMenu(project, menu)); header.appendChild(menu);
  }
  section.appendChild(header);
  if (project && !project.available) {
    const unavailable = createEl('div', 'project-status-detail', `${project.unavailable_reason || 'Project unavailable'} — ${project.canonical_dir || ''}`);
    unavailable.setAttribute('role', 'status');
    section.appendChild(unavailable);
  }
  if (expanded) {
    const opening = projectExpansionAnimations.delete(id);
    const list = createEl('div', `project-session-list${opening ? ' is-opening' : ''}`); list.setAttribute('role', 'list'); list.setAttribute('aria-labelledby', headingId);
    const sessions = projectGroupSessions(group).filter((session) => !session.pinned);
    sessions.forEach((session) => { const row = renderProjectSessionRow(session); row.setAttribute('role', 'listitem'); list.appendChild(row); });
    if (group.next_cursor && !state.sidebarSearchQuery) appendProjectPaginationSentinel(group, list);
    section.appendChild(list);
  }
  return section;
};

const projectInlineError = (message, retryAction) => {
  const error = createEl('div', 'project-inline-error'); error.setAttribute('role', 'alert');
  error.appendChild(createEl('span', '', message));
  const retry = createEl('button', '', 'Retry'); retry.type = 'button'; retry.addEventListener('click', retryAction); error.appendChild(retry);
  return error;
};

const renderProjectSidebar = () => {
  const container = elements.sessionGroups;
  disconnectProjectPagination();
  if (state.capabilitiesRequired && !state.capabilitiesLoaded) {
    if (!container) return true;
    if (state.projectsError) {
      container.replaceChildren(projectInlineError(state.projectsError, async () => { if (await loadCapabilities()) await loadProjectSidebar(); }));
    } else {
      container.replaceChildren(createEl('div', 'project-skeleton', 'Loading projects and conversations…'));
    }
    return true;
  }
  if (!state.projectsEnabled) return false;
  if (!container) return true;
  let groups = Array.isArray(state.sidebarGroups) ? state.sidebarGroups.slice() : [];
  if (state.sidebarSearchQuery && Array.isArray(state.sidebarSearchResults) && !state.sidebarSearchError) {
    const ids = new Set(state.sidebarSearchResults.map((session) => String(session.projectId || '')));
    groups = groups.filter((group) => ids.has(String(group.project?.id || '')));
  }
  if (state.sidebarSearchLoading) { container.replaceChildren(createEl('div', 'project-skeleton', 'Searching conversations…')); return true; }
  if (!groups.length && state.sidebarSearchQuery && !state.sidebarSearchError) { container.replaceChildren(createEl('div', 'sidebar-empty', 'No matching conversations')); return true; }
  const active = groups.filter((group) => group.project && !group.project.archived_at);
  const effectiveGroupActivity = (group) => {
    const projectId = String(group.project?.id || '');
    const persistedAt = Date.parse(group.last_activity_at || '') || 0;
    const localAt = (state.sessions || []).reduce((latest, session) => (
      String(session.projectId || '') === projectId
        ? Math.max(latest, Number(session.lastMessageAt || session.created || 0))
        : latest
    ), 0);
    return Math.max(persistedAt, localAt);
  };
  active.sort((a, b) => {
    const aAt = effectiveGroupActivity(a);
    const bAt = effectiveGroupActivity(b);
    return bAt - aAt || String(a.project?.name || '').localeCompare(String(b.project?.name || '')) || String(a.project?.id || '').localeCompare(String(b.project?.id || ''));
  });
  const noProject = groups.filter((group) => group.no_project);
  const archived = groups.filter((group) => group.project?.archived_at);
  const pinned = renderPinnedSessions(groups);
  const nodes = [...(pinned ? [pinned] : []), ...active.map(renderProjectGroup), ...noProject.map(renderProjectGroup)];
  if (state.projectsError) nodes.unshift(projectInlineError(state.projectsError, loadProjectSidebar));
  if (state.sidebarSearchError) {
    nodes.unshift(projectInlineError(state.sidebarSearchError, () => {
      const query = String(elements.sidebarSearchInput?.value || state.sidebarSearchQuery || '').trim();
      state.sidebarSearchLoading = true; state.sidebarSearchError = ''; searchSeq += 1;
      app.renderSidebar?.(); void runSidebarSearch(query, searchSeq);
    }));
  }
  if (archived.length) {
    const details = createEl('details', 'archived-projects');
    const summary = createEl('summary', '', 'Archived projects'); details.appendChild(summary);
    archived.forEach((group) => {
      const rendered = renderProjectGroup(group);
      if (rendered.classList.contains('active')) details.open = true;
      details.appendChild(rendered);
    });
    nodes.push(details);
  }
  container.replaceChildren(...nodes);
  return true;
};

const focusProjectGroupThenStartDraft = async (projectId) => {
  const id = String(projectId || '');
  const heading = elements.sessionGroups?.querySelector?.(`.project-group[data-project-id="${id}"] .project-group-toggle`);
  heading?.focus?.({ preventScroll: true });
  if (heading) await new Promise((resolve) => (window.requestAnimationFrame ? window.requestAnimationFrame(resolve) : setTimeout(resolve, 0)));
  await app.createAndSwitchToFreshSession?.(id);
};

const openProjectModal = (options = {}) => app.openProjectPicker({
  ...options,
  activateDialog: activateProjectDialog,
  onExistingProject: async (projectID) => { await loadProjectSidebar(); await focusProjectGroupThenStartDraft(projectID); },
  onProjectCreated: async (project) => { await loadProjectSidebar(); if (project?.id) await focusProjectGroupThenStartDraft(project.id); },
});

const restoreRememberedProjectDraft = async () => {
  if (!state.projectsEnabled || !state.draftSessionActive || state.activeSessionId) return false;
  const noProjectRemembered = state.lastProjectId === '__no_project__';
  const available = (projectID) => (state.projects || []).some((project) => project.id === projectID && project.available && !project.archived_at);
  let projectID = available(state.lastProjectId) ? state.lastProjectId : '';
  if (!projectID && !noProjectRemembered) {
    const drafts = Object.entries(state.projectDrafts || {})
      .filter(([id]) => available(id))
      .sort((a, b) => Number(b[1]?.created || 0) - Number(a[1]?.created || 0));
    projectID = drafts[0]?.[0] || '';
  }
  if ((!projectID && !noProjectRemembered) || typeof app.switchToDraftSession !== 'function') return false;
  await app.switchToDraftSession({ projectId: noProjectRemembered ? '' : projectID, clearComposer: false, focusPrompt: false, closeSidebar: false });
  return true;
};

const initializeProjectMode = async () => {
  const enabled = await loadCapabilities();
  if (enabled) {
    await loadProjectSidebar();
    await restoreRememberedProjectDraft();
  }
  return enabled;
};

// When this serve was opened through a term-llm Hub (the hub proxy injects
// window.TERM_LLM_HUB, or the serve was started with --hub-url), reveal the
// "Back to Hub" link below the Widgets entry so the hub stays one click away.
const applyBackToHubLink = () => {
  const link = elements.backToHubLink;
  if (!link) return;
  const hub = window.TERM_LLM_HUB;
  const url = hub && typeof hub.url === 'string' ? hub.url : '';
  if (!url) {
    link.classList.add('hidden');
    return;
  }
  link.href = url;
  link.title = hub.nodeName ? `Back to Hub (this node: ${hub.nodeName})` : 'Back to Hub';
  link.classList.remove('hidden');
};

const HUB_AGENT_LINKS_REFRESH_MS = 60000;
const HUB_AGENT_LINKS_FETCH_TIMEOUT_MS = 10000;
let hubAgentLinksLastFetchAt = null;
let hubAgentLinksFetchPromise = null;
let hubAgentLinksRefreshTimer = null;
let hubAgentLinksHasValidRender = false;
const hubAgentAttention = new Map();

const compareCodeUnits = (left, right) => {
  if (left < right) return -1;
  if (left > right) return 1;
  return 0;
};

const safeHubPath = (value) => (
  typeof value === 'string' && /^\/(?![\\/])/.test(value) ? value : ''
);

const appendHubQuery = (path, query) => {
  const hashIndex = path.indexOf('#');
  const base = hashIndex === -1 ? path : path.slice(0, hashIndex);
  const hash = hashIndex === -1 ? '' : path.slice(hashIndex);
  const separator = base.includes('?')
    ? (base.endsWith('?') || base.endsWith('&') ? '' : '&')
    : '?';
  return `${base}${separator}${query}${hash}`;
};

const hubAgentTarget = (node) => {
  const sessions = node?.sessions;
  const resumePath = safeHubPath(sessions?.resume_path);
  if (resumePath) return resumePath;

  const activePath = safeHubPath(Array.isArray(sessions?.active) ? sessions.active[0]?.resume_path : '');
  if (activePath) return activePath;

  const recentPath = safeHubPath(Array.isArray(sessions?.recent) ? sessions.recent[0]?.resume_path : '');
  if (recentPath) return recentPath;

  const newSessionPath = safeHubPath(node?.new_session_path);
  if (newSessionPath) return newSessionPath;

  const proxyPath = safeHubPath(node?.proxy_path);
  return proxyPath ? appendHubQuery(proxyPath, 'new=1') : '';
};

const hubAgentLinksContext = () => {
  if (!elements.hubAgentLinks) return null;
  const hub = window.TERM_LLM_HUB;
  const rawURL = hub && typeof hub.url === 'string' ? hub.url : '';
  const origin = typeof window.location?.origin === 'string' ? window.location.origin : '';
  if (!rawURL || !origin) return null;

  try {
    const parsed = new URL(rawURL, `${origin}/`);
    if (parsed.origin !== origin) return null;
    const hubPath = parsed.pathname.replace(/\/+$/, '');
    return {
      apiURL: `${hubPath}/api/nodes`,
      nodeId: String(hub.nodeId || ''),
    };
  } catch (_) {
    return null;
  }
};

const clearHubAgentLinks = () => {
  elements.hubAgentLinks?.replaceChildren();
  elements.hubAgentLinks?.classList.add('hidden'); hubAgentAttention.clear();
};

const renderHubAgentLinks = (nodes, context) => {
  const candidates = nodes
    .filter((node) => node?.status?.reachable === true)
    .map((node) => {
      const id = String(node.id || '');
      const rawName = String(node.name || '');
      return {
        node,
        id,
        rawName,
        displayName: rawName || id,
        target: hubAgentTarget(node),
      };
    })
    .filter((entry) => entry.displayName && entry.target)
    .sort((left, right) => (
      compareCodeUnits(left.displayName.toLowerCase(), right.displayName.toLowerCase())
      || compareCodeUnits(left.rawName, right.rawName)
      || compareCodeUnits(left.id, right.id)
    ));

  const rows = candidates.map(({ node, id, displayName, target }) => {
    const link = createEl('a', 'hub-agent-link');
    link.href = target;
    if (id && id === context.nodeId) link.setAttribute('aria-current', 'true');
    const icon = createEl('span', 'hub-agent-icon');
    icon.setAttribute('aria-hidden', 'true'); link.appendChild(icon); link.appendChild(createEl('span', 'hub-agent-name', displayName));
    const active = Number(node.sessions?.active_count) > 0
      || (Array.isArray(node.sessions?.active) && node.sessions.active.length > 0);
    const previous = hubAgentAttention.get(id); const attention = id !== context.nodeId
      && Boolean(previous?.attention || (previous?.active && !active));
    hubAgentAttention.set(id, { active, attention });
    if (attention) {
      const dot = createEl('span', 'hub-agent-attention');
      dot.title = 'Needs attention'; dot.setAttribute('aria-hidden', 'true'); link.appendChild(dot); link.appendChild(createEl('span', 'visually-hidden', 'Needs attention'));
    }
    ['click', 'auxclick'].forEach((type) => link.addEventListener(type, () => { hubAgentAttention.set(id, { active, attention: false }); link.querySelector('.hub-agent-attention')?.remove(); link.querySelector('.visually-hidden')?.remove(); })); return link;
  });

  elements.hubAgentLinks.replaceChildren(...rows);
  elements.hubAgentLinks.classList.toggle('hidden', rows.length === 0);
};

const fetchHubAgentLinks = () => {
  const context = hubAgentLinksContext();
  if (!context || document.visibilityState === 'hidden') {
    if (!context) clearHubAgentLinks();
    return Promise.resolve(false);
  }
  if (hubAgentLinksFetchPromise) return hubAgentLinksFetchPromise;

  hubAgentLinksLastFetchAt = Date.now();
  const request = (async () => {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), HUB_AGENT_LINKS_FETCH_TIMEOUT_MS);
    try {
      // Hub auth is a same-origin cookie. Do not use node API auth helpers here.
      const response = await fetch(context.apiURL, { signal: controller.signal });
      if (response.status >= 400 && response.status < 500) {
        clearHubAgentLinks();
        hubAgentLinksHasValidRender = false;
        return false;
      }
      if (!response.ok) throw new Error(`Hub nodes request failed (${response.status})`);
      const data = await response.json();
      if (!Array.isArray(data?.nodes)) throw new Error('Hub nodes response is invalid');
      renderHubAgentLinks(data.nodes, context);
      hubAgentLinksHasValidRender = true;
      return true;
    } catch (_) {
      if (!hubAgentLinksHasValidRender) clearHubAgentLinks();
      return false;
    } finally {
      clearTimeout(timeout);
    }
  })();

  const trackedRequest = request.finally(() => {
    if (hubAgentLinksFetchPromise === trackedRequest) hubAgentLinksFetchPromise = null;
  });
  hubAgentLinksFetchPromise = trackedRequest;
  return trackedRequest;
};

const clearHubAgentLinksRefreshTimer = () => {
  if (hubAgentLinksRefreshTimer === null) return;
  clearTimeout(hubAgentLinksRefreshTimer);
  hubAgentLinksRefreshTimer = null;
};

const refreshHubAgentLinks = () => {
  const context = hubAgentLinksContext();
  if (!context) {
    clearHubAgentLinksRefreshTimer();
    clearHubAgentLinks();
    return Promise.resolve(false);
  }
  if (document.visibilityState === 'hidden') return Promise.resolve(false);
  if (hubAgentLinksFetchPromise) return hubAgentLinksFetchPromise;

  const elapsed = hubAgentLinksLastFetchAt === null
    ? HUB_AGENT_LINKS_REFRESH_MS
    : Date.now() - hubAgentLinksLastFetchAt;
  if (elapsed >= HUB_AGENT_LINKS_REFRESH_MS) {
    clearHubAgentLinksRefreshTimer();
    return fetchHubAgentLinks();
  }

  if (hubAgentLinksRefreshTimer === null) {
    hubAgentLinksRefreshTimer = setTimeout(() => {
      hubAgentLinksRefreshTimer = null;
      if (document.visibilityState !== 'hidden') void fetchHubAgentLinks();
    }, HUB_AGENT_LINKS_REFRESH_MS - Math.max(0, elapsed));
  }
  return Promise.resolve(false);
};

const handleHubAgentLinksVisibility = () => {
  if (document.visibilityState === 'hidden') {
    clearHubAgentLinksRefreshTimer();
    return;
  }
  void refreshHubAgentLinks();
};

const handleHubAgentLinksFocus = () => {
  if (document.visibilityState !== 'hidden') void refreshHubAgentLinks();
};

if (elements.widgetsOpenBtn) elements.widgetsOpenBtn.addEventListener('click', openWidgetsModal);
if (elements.widgetsModalCloseBtn) elements.widgetsModalCloseBtn.addEventListener('click', closeWidgetsModal);
if (elements.widgetsModal) {
  elements.widgetsModal.addEventListener('click', (event) => {
    if (event.target === elements.widgetsModal) closeWidgetsModal();
  });
  elements.widgetsModal.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && !event.defaultPrevented) {
      event.preventDefault();
      closeWidgetsModal();
    }
  });
}

const isMac = /Mac|iPhone|iPad|iPod/.test(navigator.platform);
document.addEventListener('keydown', (event) => {
  if (event.key !== 'k' && event.key !== 'K') return;
  const primary = isMac ? event.metaKey && !event.ctrlKey : event.ctrlKey && !event.metaKey;
  if (!primary) return;
  if (event.altKey || event.shiftKey) return;
  if (elements.widgetsOpenBtn?.classList.contains('hidden')) return;
  event.preventDefault();
  if (elements.widgetsModal?.classList.contains('hidden')) {
    openWidgetsModal();
  } else {
    closeWidgetsModal();
  }
});
if (elements.sidebarSearchInput) elements.sidebarSearchInput.addEventListener('input', scheduleSidebarSearch);
document.addEventListener('visibilitychange', handleHubAgentLinksVisibility);
window.addEventListener('focus', handleHubAgentLinksFocus);

// ===== Sidebar status polling =====
const SIDEBAR_POLL_ACTIVE = 2000;
const SIDEBAR_POLL_VISIBLE_ACTIVE = 5000;
const SIDEBAR_POLL_IDLE = 30000;
// Retry selected-session state after transient upstream/proxy failures so a
// single hub/reverse blip does not permanently stop active-session updates.
const SESSION_STATE_POLL_RETRY = 5000;
let sidebarStatusTimer = null;
let sidebarStatusEtag = null;
let sidebarHasActive = false;
let sidebarStatusPollEnabled = false;
let sidebarStatusPollPromise = null;
let sidebarStatusPollController = null;
let sidebarStatusPollGeneration = 0;
let sidebarStatusPollInFlightGeneration = -1;
let sidebarStatusPollIsRecovery = false;
let sidebarStatusImmediatePending = false;

const applyWidgetStatus = (data) => {
  state.widgets = Array.isArray(data?.widgets) ? data.widgets : [];
  state.widgetsLoaded = true;
  app.renderWidgetSidebar?.();
};

const refreshWidgetsSidebar = async () => {
  if (!app.renderWidgetSidebar) return;
  try {
    const resp = await app.apiFetch(`${UI_PREFIX}/admin/widgets/status`, {
      headers: app.requestHeaders('')
    }, { auth: app.API_FETCH_AUTH?.ignore || 'ignore' });
    if (resp.status === 404) {
      state.widgets = [];
      state.widgetsLoaded = false;
      app.renderWidgetSidebar?.();
      return;
    }
    if (!resp.ok) return;
    applyWidgetStatus(await resp.json());
  } catch (_) {
    // Widgets are optional; leave the section hidden if the admin route is unavailable.
  }
};

const clearSidebarStatusTimer = () => {
  if (sidebarStatusTimer !== null) {
    clearTimeout(sidebarStatusTimer);
    sidebarStatusTimer = null;
  }
};

const stopSidebarStatusPoll = () => {
  const pending = sidebarStatusPollPromise;
  sidebarStatusPollEnabled = false;
  sidebarStatusImmediatePending = false;
  sidebarStatusPollGeneration += 1;
  clearSidebarStatusTimer();
  sidebarStatusPollController?.abort();
  return pending || Promise.resolve(false);
};

const scheduleSidebarStatusPoll = (delay) => {
  clearSidebarStatusTimer();
  if (!state.connected || !sidebarStatusPollEnabled || document.visibilityState === 'hidden') return;
  sidebarStatusTimer = setTimeout(() => {
    sidebarStatusTimer = null;
    return pollSidebarStatus(false);
  }, delay);
};

const sidebarStatusPollDelay = () => {
  sidebarHasActive = app.hasAnySessionInProgressState();
  if (sidebarHasActive) return SIDEBAR_POLL_ACTIVE;
  if (document.visibilityState === 'visible' && !state.draftSessionActive && app.getActiveSession()) {
    return SIDEBAR_POLL_VISIBLE_ACTIVE;
  }
  return SIDEBAR_POLL_IDLE;
};

const pollSidebarStatus = (isRecovery = false) => {
  if (!state.connected || !sidebarStatusPollEnabled || document.visibilityState === 'hidden') return Promise.resolve(false);
  if (sidebarStatusPollPromise) return sidebarStatusPollPromise;

  clearSidebarStatusTimer();
  const generation = sidebarStatusPollGeneration;
  const controller = new AbortController();
  sidebarStatusPollController = controller;
  sidebarStatusPollInFlightGeneration = generation;
  sidebarStatusPollIsRecovery = isRecovery;
  const sampledRunEpochs = new Map(state.sessions.map((session) => [
    String(session.id || ''),
    Math.max(0, Number(session.transcript?.latestRunEpoch) || 0),
  ]));

  const isCurrent = () => (
    state.connected
    && sidebarStatusPollEnabled
    && document.visibilityState !== 'hidden'
    && sidebarStatusPollGeneration === generation
    && sidebarStatusPollController === controller
  );

  const request = (async () => {
    try {
      const params = new URLSearchParams();
      const categories = state.sidebarSessionCategories;
      if (Array.isArray(categories) && categories.length > 0 && !categories.includes('all')) {
        params.set('categories', categories.join(','));
      }
      if (state.showHiddenSessions) params.set('include_archived', '1');
      const query = params.toString();

      const headers = app.requestHeaders('');
      if (sidebarStatusEtag) headers['If-None-Match'] = sidebarStatusEtag;

      const resp = await app.apiFetch(`${UI_PREFIX}/v1/sessions/status${query ? `?${query}` : ''}`, {
        headers,
        signal: controller.signal,
      });
      if (!isCurrent()) return false;

      if (resp.status === 304) return isCurrent();
      if (!resp.ok) return false;

      const data = await resp.json();
      if (!isCurrent()) return false;
      const etag = resp.headers.get('ETag');
      if (etag) sidebarStatusEtag = etag;
      if (Array.isArray(data.sessions)) {
        app.updateSidebarStatus(data.sessions);
        await app.reconcileTranscriptFromStatus(data.sessions, { sampledRunEpochs });
        if (!isCurrent()) return false;
        const active = app.getActiveSession?.();
        const activeStatus = active ? data.sessions.find((entry) => entry?.id === active.id) : null;
        const ownedResponseId = String(active?.activeResponseId || (
          state.currentStreamSessionId === active?.id ? state.currentStreamResponseId : ''
        ) || '').trim();
        if (activeStatus && !activeStatus.active_run && !activeStatus.active_response_id && ownedResponseId) {
          // Status is independent of SSE. Confirm selected-session truth when a
          // server-issued response ID remains locally owned after status says idle.
          try {
            const result = await app.syncActiveSessionFromServer(active, true, { skipMessagesFetch: true });
            if (result?.kind === 'retry') app.scheduleSessionStatePoll?.(active.id, 0);
          } catch (_err) {
            app.scheduleSessionStatePoll?.(active.id, 0);
          }
          if (!isCurrent()) return false;
        }
        // Discover sessions created in other tabs/devices
        const localIds = new Set(state.sessions.map((s) => s.id));
        const hasUnknown = data.sessions.some((entry) => !localIds.has(entry.id));
        if (hasUnknown) app.mergeServerSessions();
      }
      return true;
    } catch (_e) {
      // Network error or an intentional visibility abort — recover below when visible.
      return false;
    }
  })();

  const trackedRequest = request.finally(() => {
    if (sidebarStatusPollPromise !== trackedRequest) return;
    sidebarStatusPollPromise = null;
    sidebarStatusPollIsRecovery = false;
    if (sidebarStatusPollController === controller) sidebarStatusPollController = null;

    if (!sidebarStatusPollEnabled || document.visibilityState === 'hidden') return;
    if (sidebarStatusImmediatePending) {
      sidebarStatusImmediatePending = false;
      sidebarStatusEtag = null;
      void pollSidebarStatus(true);
      return;
    }
    if (sidebarStatusPollGeneration === generation) {
      scheduleSidebarStatusPoll(sidebarStatusPollDelay());
    }
  });
  sidebarStatusPollPromise = trackedRequest;
  return trackedRequest;
};

const ensureSidebarStatusPoll = () => {
  if (document.visibilityState === 'hidden') {
    stopSidebarStatusPoll();
    return Promise.resolve(false);
  }
  if (!state.connected) return Promise.resolve(false);

  sidebarStatusPollEnabled = true;
  clearSidebarStatusTimer();
  if (sidebarStatusPollPromise) {
    if (sidebarStatusPollInFlightGeneration !== sidebarStatusPollGeneration) {
      sidebarStatusImmediatePending = true;
      return sidebarStatusPollPromise.then(() => sidebarStatusPollPromise || false);
    }
    return sidebarStatusPollPromise;
  }

  sidebarStatusImmediatePending = false;
  sidebarStatusEtag = null;
  return pollSidebarStatus(true);
};

const startSidebarStatusPoll = () => ensureSidebarStatusPoll();

const refreshSidebarStatusPoll = (forceNow = false) => {
  if (!state.connected || document.visibilityState === 'hidden') return Promise.resolve(false);
  if (forceNow) return ensureSidebarStatusPoll();

  sidebarStatusPollEnabled = true;
  if (!sidebarStatusPollPromise) scheduleSidebarStatusPoll(sidebarStatusPollDelay());
  return Promise.resolve(false);
};

const handleFetchTransportFallback = () => {
  if (!state.connected) return Promise.resolve(false);
  if (document.visibilityState !== 'hidden' && sidebarStatusPollPromise && !sidebarStatusPollIsRecovery) {
    sidebarStatusPollEnabled = true;
    clearSidebarStatusTimer();
    sidebarStatusImmediatePending = true;
    return sidebarStatusPollPromise.then(() => sidebarStatusPollPromise || false);
  }
  return ensureSidebarStatusPoll();
};

applyBackToHubLink();
void refreshHubAgentLinks();

Object.assign(app, {
  HUB_AGENT_LINKS_REFRESH_MS,
  HUB_AGENT_LINKS_FETCH_TIMEOUT_MS,
  SIDEBAR_POLL_ACTIVE,
  SIDEBAR_POLL_VISIBLE_ACTIVE,
  SIDEBAR_POLL_IDLE,
  SESSION_STATE_POLL_RETRY,
  sidebarStatusTimer,
  sidebarStatusEtag,
  sidebarHasActive,
  sidebarStatusPollEnabled,
  sidebarStatusPollPromise,
  sidebarStatusPollController,
  sidebarStatusPollGeneration,
  sidebarStatusPollInFlightGeneration,
  sidebarStatusPollIsRecovery,
  sidebarStatusImmediatePending,
  applyWidgetStatus,
  refreshWidgetsSidebar,
  clearSidebarStatusTimer,
  stopSidebarStatusPoll,
  scheduleSidebarStatusPoll,
  sidebarStatusPollDelay,
  pollSidebarStatus,
  ensureSidebarStatusPoll,
  startSidebarStatusPoll,
  refreshSidebarStatusPoll,
  handleFetchTransportFallback,
  renderWidgetSidebar,
  applyBackToHubLink,
  renderHubAgentLinks,
  fetchHubAgentLinks,
  refreshHubAgentLinks,
  scheduleSidebarSearch,
  loadCapabilities,
  loadProjectSidebar,
  renderProjectSidebar,
  openAssignProjectModal,
  activateProjectDialog,
  openProjectModal,
  initializeProjectMode
});
})();
