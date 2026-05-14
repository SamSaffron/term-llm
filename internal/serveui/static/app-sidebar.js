(() => {
'use strict';

const app = window.TermLLMApp;
const { UI_PREFIX, state, elements } = app;

const SEARCH_DEBOUNCE_MS = 180;
let searchTimer = null;
let searchAbort = null;
let searchSeq = 0;

const widgetTitle = (widget) => String(widget?.title || widget?.mount || widget?.id || 'Widget');
const widgetMount = (widget) => String(widget?.mount || widget?.id || '').replace(/^\/+|\/+$/g, '');

const buildWidgetLink = (widget) => {
  const mount = widgetMount(widget);
  const link = document.createElement('a');
  link.className = 'widget-link';
  link.href = `${UI_PREFIX}/widgets/${encodeURIComponent(mount)}/`;
  link.title = widget.description || widgetTitle(widget);

  const titleRow = document.createElement('div');
  titleRow.className = 'widget-title-row';

  const title = document.createElement('span');
  title.className = 'widget-title';
  title.textContent = widgetTitle(widget);

  const normalizedStatus = String(widget.state || 'stopped').toLowerCase();
  const statusClass = normalizedStatus.replace(/[^a-z0-9_-]/g, '');
  const showRunningIndicator = statusClass === 'running' || statusClass === 'starting' || statusClass === 'started';
  const showTextBadge = statusClass && statusClass !== 'stopped' && !showRunningIndicator;

  titleRow.appendChild(title);
  if (showRunningIndicator) {
    const stateBadge = document.createElement('span');
    stateBadge.className = `widget-state ${statusClass}`;
    stateBadge.title = 'Running';
    stateBadge.setAttribute('aria-label', 'Running');
    titleRow.appendChild(stateBadge);
  } else if (showTextBadge) {
    const stateBadge = document.createElement('span');
    stateBadge.className = `widget-state ${statusClass}`;
    stateBadge.textContent = normalizedStatus;
    titleRow.appendChild(stateBadge);
  }
  link.appendChild(titleRow);
  const meta = document.createElement('div');
  meta.className = 'widget-meta';
  meta.textContent = widget.description || mount;
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
    messages: [],
    lastResponseId: null,
    activeResponseId: null,
    lastSequenceNumber: 0,
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
    const resp = await fetch(`${UI_PREFIX}/v1/sessions/search?${params.toString()}`, {
      headers,
      signal: searchAbort.signal
    });
    if (!resp.ok) throw new Error(`search failed (${resp.status})`);
    const data = await resp.json();
    if (seq !== searchSeq) return;
    state.sidebarSearchResults = Array.isArray(data.sessions)
      ? data.sessions.map(searchResultToSession).filter(Boolean)
      : [];
    state.sidebarSearchLoading = false;
    app.renderSidebar?.();
  } catch (err) {
    if (err?.name === 'AbortError' || seq !== searchSeq) return;
    state.sidebarSearchResults = [];
    state.sidebarSearchLoading = false;
    app.renderSidebar?.();
  }
};

const scheduleSidebarSearch = () => {
  const query = String(elements.sidebarSearchInput?.value || '').trim();
  state.sidebarSearchQuery = query;
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
if (elements.sidebarSearchInput) elements.sidebarSearchInput.addEventListener('input', scheduleSidebarSearch);

Object.assign(app, {
  renderWidgetSidebar,
  scheduleSidebarSearch
});
})();
