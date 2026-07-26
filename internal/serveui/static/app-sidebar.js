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
    lastResponseId: null,
    activeResponseId: null,
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
    const resp = await fetch(`${UI_PREFIX}/admin/widgets/status`, {
      headers: app.requestHeaders('')
    });
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

      const resp = await fetch(`${UI_PREFIX}/v1/sessions/status${query ? `?${query}` : ''}`, {
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

Object.assign(app, {
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
  scheduleSidebarSearch
});
})();
