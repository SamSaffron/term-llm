(() => {
'use strict';

const app = window.TermLLMApp;
const createEl = app.createEl;
const { UI_PREFIX, state, elements } = app;

const SEARCH_DEBOUNCE_MS = 180;
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
  scheduleSidebarSearch
});
})();
