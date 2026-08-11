'use strict';

(function initAppMCP() {

const app = window.TermLLMApp;
const createEl = app.createEl;
const {
  UI_PREFIX, state, elements, saveSessions, getActiveSession, createSession, ensureActiveSession, sessionSlug,
  updateURL, persistAndRefreshShell, updateMCPStatusDisplay, setElementHidden, handleAuthFailure, renderMessages,
  requestHeaders, normalizeError, sessionHasInProgressState
} = app;

const normalizeMCPServerView = (server) => {
  if (!server || typeof server !== 'object') return null;
  const name = String(server.name || '').trim();
  if (!name) return null;
  return {
    name,
    configured: server.configured !== false,
    enabled: Boolean(server.enabled),
    status: String(server.status || (server.enabled ? 'ready' : 'stopped')).trim() || 'stopped',
    error: String(server.error || '').trim(),
    tools: Number.isFinite(Number(server.tools)) ? Number(server.tools) : 0,
  };
};

const applyGoalStateToSession = (session, data) => {
  if (!session || !data || typeof data !== 'object' || !Object.prototype.hasOwnProperty.call(data, 'goal')) return false;
  const nextGoal = data.goal && typeof data.goal === 'object' ? { ...data.goal } : null;
  const before = JSON.stringify(session.goal || null);
  const after = JSON.stringify(nextGoal || null);
  if (before === after) return false;
  session.goal = nextGoal;
  return true;
};

const formatGoalChipText = (goal) => {
  if (!goal || typeof goal !== 'object') return '';
  const status = String(goal.status || 'active').trim() || 'active';
  let text = `🎯 ${status}`;
  const used = Number(goal.tokens_used || 0);
  const budget = Number(goal.token_budget || 0);
  if (budget > 0) text += ` · ${Math.max(0, used)}/${budget} tok`;
  const objective = String(goal.objective || '').trim();
  if (objective) text += ` · ${objective}`;
  return text;
};

const updateGoalChip = (session = ensureActiveSession?.()) => {
  const chip = elements.goalChip;
  if (!chip) return;
  const goal = session?.goal || null;
  const text = formatGoalChipText(goal);
  if (!text) {
    chip.className = 'goal-chip hidden';
    chip.textContent = '';
    return;
  }
  const status = String(goal.status || 'active').trim() || 'active';
  chip.className = `goal-chip goal-${status}`;
  chip.textContent = text;
  chip.title = text;
};

const applyMCPStateToSession = (session, data) => {
  if (!session || !data || typeof data !== 'object') return false;
  const hasServerField = Array.isArray(data.servers) || Array.isArray(data.mcp_servers);
  const hasEnabledField = Array.isArray(data.enabled) || Array.isArray(data.mcp_enabled);
  if (!hasServerField && !hasEnabledField) return false;
  const servers = hasServerField
    ? (Array.isArray(data.servers)
      ? data.servers.map(normalizeMCPServerView).filter(Boolean)
      : data.mcp_servers.map(normalizeMCPServerView).filter(Boolean))
    : (Array.isArray(session.mcpServers) ? session.mcpServers.slice() : []);
  const enabledSource = Array.isArray(data.enabled)
    ? data.enabled
    : (Array.isArray(data.mcp_enabled) ? data.mcp_enabled : servers.filter((server) => server.enabled).map((server) => server.name));
  const enabled = [];
  const seen = new Set();
  enabledSource.forEach((raw) => {
    const name = String(raw || '').trim();
    if (!name || seen.has(name)) return;
    seen.add(name);
    enabled.push(name);
  });
  const serverJSON = JSON.stringify(servers);
  const enabledJSON = JSON.stringify(enabled);
  const changed = JSON.stringify(session.mcpServers || []) !== serverJSON
    || JSON.stringify(session.mcpEnabled || []) !== enabledJSON;
  session.mcpServers = servers;
  session.mcpEnabled = enabled;
  updateMCPStatusDisplay(session);
  return changed;
};

// ===== Composer add menu / MCP controls =====
let mcpModalSessionId = '';
let mcpModalPending = false;
let mcpModalPendingEnabled = null;
let mcpModalErrorMessage = '';

const normalizeMCPEnabledNames = (names) => {
  const enabled = [];
  const seen = new Set();
  (Array.isArray(names) ? names : []).forEach((raw) => {
    const name = String(raw || '').trim();
    if (!name || seen.has(name)) return;
    seen.add(name);
    enabled.push(name);
  });
  return enabled;
};

const setAddMenuOpen = (open) => {
  if (!elements.addMenu || !elements.attachBtn) return;
  setElementHidden(elements.addMenu, !open);
  elements.attachBtn.setAttribute('aria-expanded', open ? 'true' : 'false');
};

const toggleAddMenu = () => {
  const open = elements.addMenu ? elements.addMenu.hidden : true;
  setAddMenuOpen(open);
};

const closeAddMenu = () => setAddMenuOpen(false);

const sessionIsBusyForMCP = (session) => Boolean(
  session && (
    sessionHasInProgressState(session)
    || session.activeResponseId
    || (state.streaming && (!state.currentStreamSessionId || state.currentStreamSessionId === session.id))
  )
);

const ensureActiveSessionForMCP = () => {
  let session = getActiveSession();
  if (session && !state.draftSessionActive) {
    return session;
  }
  session = createSession();
  state.sessions.unshift(session);
  state.activeSessionId = session.id;
  state.draftSessionActive = false;
  updateURL(sessionSlug(session));
  persistAndRefreshShell();
  renderMessages(true);
  app.activateDiffSidebar?.(session.id);
  return session;
};

const mcpHeaders = () => requestHeaders ? requestHeaders('') : { 'Content-Type': 'application/json' };

const fetchSessionMCP = async (sessionId) => {
  const resp = await app.apiFetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(sessionId)}/mcp`, {
    headers: mcpHeaders(),
  });
  if (!resp.ok) throw await normalizeError(resp);
  const data = await resp.json().catch(() => ({ servers: [], enabled: [] }));
  const session = state.sessions.find((item) => item.id === sessionId) || null;
  if (session) {
    applyMCPStateToSession(session, data);
    saveSessions();
    app.updateHeader?.();
  }
  return data;
};

const closeMCPModal = () => {
  if (!elements.mcpModal) return;
  elements.mcpModal.classList.add('hidden');
  mcpModalSessionId = '';
  mcpModalPending = false;
  mcpModalPendingEnabled = null;
  mcpModalErrorMessage = '';
  if (elements.mcpError) {
    elements.mcpError.textContent = '';
    elements.mcpError.classList.remove('is-muted');
  }
};

const renderMCPModal = (session, { loading = false } = {}) => {
  if (!elements.mcpModalBody) return;
  const busy = sessionIsBusyForMCP(session);
  const disabled = loading || mcpModalPending || busy;
  elements.mcpModalBody.innerHTML = '';

  if (loading) {
    const row = createEl('div', 'mcp-server-loading', 'Loading configured MCP servers…');
    elements.mcpModalBody.appendChild(row);
  } else if (!Array.isArray(session?.mcpServers) || session.mcpServers.length === 0) {
    const row = createEl('div', 'mcp-server-empty', 'No MCP servers are configured yet. Add servers to ~/.config/term-llm/mcp.json, then reopen this panel.');
    elements.mcpModalBody.appendChild(row);
  } else {
    const enabledSet = new Set(
      mcpModalPending && Array.isArray(mcpModalPendingEnabled)
        ? mcpModalPendingEnabled
        : normalizeMCPEnabledNames(session.mcpEnabled || [])
    );
    session.mcpServers.forEach((server) => {
      const row = createEl('label', 'mcp-server-row');

      const icon = createEl('span', 'mcp-server-icon');
      icon.setAttribute('aria-hidden', 'true');
      icon.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="5" y="5" width="5" height="5" rx="1.2"/><rect x="14" y="14" width="5" height="5" rx="1.2"/><path d="M10 7.5h2.5a4 4 0 0 1 4 4V14"/><path d="M14 16.5h-2.5a4 4 0 0 1-4-4V10"/></svg>';

      const copy = createEl('span', 'mcp-server-copy');
      const titleRow = createEl('span', 'mcp-server-title-row');
      const name = createEl('span', 'mcp-server-name', server.name);
      const status = createEl('span', `mcp-server-status ${String(server.status || '').toLowerCase()}`, server.status || 'stopped');
      titleRow.appendChild(name);
      titleRow.appendChild(status);

      const subtitle = createEl('span', 'mcp-server-subtitle');
      const toolCount = Number(server.tools || 0);
      if (String(server.status || '').toLowerCase() === 'ready') {
        subtitle.textContent = `${toolCount} tool${toolCount === 1 ? '' : 's'} available`;
      } else if (String(server.status || '').toLowerCase() === 'starting') {
        subtitle.textContent = 'Starting server…';
      } else if (server.error) {
        subtitle.textContent = 'Failed to start';
      } else {
        subtitle.textContent = 'Tools load when enabled';
      }

      copy.appendChild(titleRow);
      copy.appendChild(subtitle);
      if (server.error) {
        const error = createEl('span', 'mcp-server-error', server.error);
        copy.appendChild(error);
      }

      const switchWrap = createEl('span', 'mcp-switch');
      const input = createEl('input', 'mcp-switch-input');
      input.type = 'checkbox';
      input.value = server.name;
      input.checked = enabledSet.has(server.name) || Boolean(server.enabled);
      input.disabled = disabled;
      input.dataset.mcpServer = server.name;
      const track = createEl('span', 'mcp-switch-track');
      const thumb = createEl('span', 'mcp-switch-thumb');
      track.appendChild(thumb);
      switchWrap.appendChild(input);
      switchWrap.appendChild(track);

      row.appendChild(icon);
      row.appendChild(copy);
      row.appendChild(switchWrap);
      elements.mcpModalBody.appendChild(row);
    });
  }

  if (elements.mcpError) {
    const statusMessage = busy && !mcpModalErrorMessage
      ? 'Cannot change MCPs while a response is running.'
      : (mcpModalPending ? 'Saving changes…' : mcpModalErrorMessage);
    elements.mcpError.textContent = statusMessage;
    elements.mcpError.classList.toggle('is-muted', Boolean(mcpModalPending && !mcpModalErrorMessage));
  }
};

const openSessionMCPModal = async () => {
  const session = ensureActiveSessionForMCP();
  if (!session) return null;
  mcpModalSessionId = session.id;
  mcpModalPendingEnabled = null;
  mcpModalErrorMessage = '';
  if (elements.mcpModal) elements.mcpModal.classList.remove('hidden');
  renderMCPModal(session, { loading: true });
  try {
    await fetchSessionMCP(session.id);
    const refreshed = state.sessions.find((item) => item.id === session.id) || session;
    renderMCPModal(refreshed);
    return refreshed;
  } catch (err) {
    if (err?.status === 401) {
      handleAuthFailure();
      return null;
    }
    mcpModalErrorMessage = err?.message || 'Failed to load MCP servers.';
    renderMCPModal(session);
    return null;
  }
};

const selectedMCPNamesFromModal = () => {
  if (!elements.mcpModalBody || typeof elements.mcpModalBody.querySelectorAll !== 'function') return [];
  return Array.from(elements.mcpModalBody.querySelectorAll('input[data-mcp-server]'))
    .filter((input) => input.checked)
    .map((input) => String(input.value || '').trim())
    .filter(Boolean);
};

const applySessionMCP = async (sessionId, enabledNames) => {
  const session = state.sessions.find((item) => item.id === sessionId) || null;
  if (!session || sessionIsBusyForMCP(session)) {
    mcpModalErrorMessage = 'Cannot change MCPs while a response is running.';
    renderMCPModal(session);
    return null;
  }
  const requestedEnabled = normalizeMCPEnabledNames(enabledNames);
  mcpModalPending = true;
  mcpModalPendingEnabled = requestedEnabled;
  mcpModalErrorMessage = '';
  renderMCPModal(session);
  try {
    const resp = await app.apiFetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(sessionId)}/mcp`, {
      method: 'PATCH',
      headers: mcpHeaders(),
      body: JSON.stringify({ enabled: requestedEnabled }),
    });
    if (!resp.ok) throw await normalizeError(resp);
    const data = await resp.json().catch(() => ({ servers: [], enabled: [] }));
    applyMCPStateToSession(session, data);
    saveSessions();
    app.updateHeader?.();
    mcpModalPending = false;
    mcpModalPendingEnabled = null;
    renderMCPModal(session);
    return data;
  } catch (err) {
    mcpModalPending = false;
    mcpModalPendingEnabled = null;
    if (err?.status === 401) {
      handleAuthFailure();
      return null;
    }
    mcpModalErrorMessage = err?.status === 409
      ? 'Cannot change MCPs while a response is running.'
      : (err?.message || 'Failed to update MCP servers.');
    renderMCPModal(session);
    return null;
  }
};

if (elements.mcpStatus) elements.mcpStatus.addEventListener('click', () => void openSessionMCPModal());
if (elements.mcpModalCloseBtn) elements.mcpModalCloseBtn.addEventListener('click', closeMCPModal);
if (elements.mcpModalBody) {
  elements.mcpModalBody.addEventListener('change', async (event) => {
    const input = event.target;
    if (!input?.dataset?.mcpServer || !mcpModalSessionId || mcpModalPending) return;
    await applySessionMCP(mcpModalSessionId, selectedMCPNamesFromModal());
  });
}
if (elements.mcpModal) {
  elements.mcpModal.addEventListener('keydown', (event) => {
    if (event.key !== 'Escape' || event.defaultPrevented) return;
    event.preventDefault();
    closeMCPModal();
  });
}

Object.assign(app, {
  normalizeMCPServerView,
  applyGoalStateToSession,
  formatGoalChipText,
  updateGoalChip,
  applyMCPStateToSession,
  mcpModalSessionId,
  mcpModalPending,
  mcpModalPendingEnabled,
  mcpModalErrorMessage,
  normalizeMCPEnabledNames,
  setAddMenuOpen,
  toggleAddMenu,
  closeAddMenu,
  sessionIsBusyForMCP,
  ensureActiveSessionForMCP,
  mcpHeaders,
  fetchSessionMCP,
  closeMCPModal,
  renderMCPModal,
  openSessionMCPModal,
  selectedMCPNamesFromModal,
  applySessionMCP
});
})();
