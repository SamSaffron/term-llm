(() => {
  'use strict';

  const helpers = {
    relativeTime(value, now = Date.now()) {
      const time = new Date(value).getTime();
      if (!Number.isFinite(time)) return 'unknown';
      const delta = now - time;
      const seconds = Math.floor(Math.abs(delta) / 1000);
      if (delta < 0) {
        if (seconds < 60) return 'in under a minute';
        if (seconds < 3600) return `in ${Math.ceil(seconds / 60)}m`;
        if (seconds < 86400) return `in ${Math.ceil(seconds / 3600)}h`;
        if (seconds < 604800) return `in ${Math.ceil(seconds / 86400)}d`;
        return new Date(time).toLocaleDateString();
      }
      if (seconds < 60) return 'just now';
      if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
      if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
      if (seconds < 604800) return `${Math.floor(seconds / 86400)}d ago`;
      return new Date(time).toLocaleDateString();
    },
    ttlSeconds(value, customHours) {
      if (value === 'custom') return Math.max(1, Number(customHours) || 1) * 3600;
      return Math.max(0, Number(value) || 0);
    },
    clearSecret(node) {
      if (node) node.textContent = '';
    },
    errorMessage(payload, fallback) {
      return payload && payload.error && payload.error.message ? payload.error.message : fallback;
    }
  };

  if (typeof module !== 'undefined' && module.exports) module.exports = helpers;
  if (typeof document === 'undefined') return;

  const $ = (id) => document.getElementById(id);
  // Resolve API calls relative to the mounted UI so --base-path works without
  // baking a deployment-specific prefix into the embedded assets.
  const apiBase = new URL('.', window.location.href);
  const apiURL = (path) => new URL(String(path).replace(/^\//, ''), apiBase).toString();
  const state = {
    adminToken: '', clients: [], models: [], requests: [], pendingRequests: [], audit: [], grants: new Map(),
    page: 'overview', requestStatus: 'pending', detailClientID: '', decisionRequestID: '', oneTimeToken: ''
  };
  const el = (tag, className, text) => {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  };
  const clientName = (id) => (state.clients.find((c) => c.id === id) || {}).name || id || 'Unknown client';
  const button = (text, className, action, id) => {
    const b = el('button', className, text); b.type = 'button'; b.dataset.action = action;
    if (id) b.dataset.id = id;
    return b;
  };
  const empty = (text) => el('div', 'empty', text);
  const badge = (text, className) => el('span', `badge ${className || text}`, text);

  function setConnection(mode, label) {
    const node = $('connectionState'); node.className = `connection ${mode}`; node.querySelector('span').textContent = label;
  }
  let flashTimer;
  function flash(message, error = false) {
    const node = $('flash'); node.textContent = message; node.className = `flash${error ? ' error' : ''}`; node.hidden = false;
    clearTimeout(flashTimer); flashTimer = setTimeout(() => { node.hidden = true; node.textContent = ''; }, 4200);
  }
  function showModal(id) { $(id).hidden = false; const focus = $(id).querySelector('input,select,button'); if (focus) setTimeout(() => focus.focus(), 0); }
  function hideModal(node) { node.hidden = true; node.querySelectorAll('input[type="password"]').forEach((input) => { input.value = ''; }); }

  async function api(path, options = {}, allowUnauthorized = false) {
    const headers = new Headers(options.headers || {});
    if (state.adminToken) headers.set('Authorization', `Bearer ${state.adminToken}`);
    if (options.body) headers.set('Content-Type', 'application/json');
    let response;
    try { response = await fetch(apiURL(path), { ...options, headers }); }
    catch (error) { setConnection('offline', 'Offline'); throw new Error(`Proxy is unreachable: ${error.message}`); }
    if (response.status === 401 && !allowUnauthorized) { lock(); throw new Error('Admin session locked'); }
    let payload = null;
    if (response.status !== 204) { try { payload = await response.json(); } catch (_) {} }
    if (!response.ok) throw new Error(helpers.errorMessage(payload, `${response.status} ${response.statusText}`));
    setConnection('online', 'Connected');
    return payload;
  }

  function lock() {
    state.adminToken = ''; state.oneTimeToken = ''; helpers.clearSecret($('secretValue'));
    $('secretModal').hidden = true; $('adminTokenInput').value = ''; $('authError').textContent = '';
    setConnection('', 'Locked'); showModal('authModal');
  }

  async function loadCore(allowUnauthorized = false) {
    const [clients, requests, audit, models] = await Promise.all([
      api('/admin/proxy/clients', {}, allowUnauthorized),
      api('/admin/proxy/requests?status=pending', {}, allowUnauthorized),
      api('/admin/proxy/audit?limit=200', {}, allowUnauthorized),
      api('/admin/proxy/models', {}, allowUnauthorized)
    ]);
    state.clients = clients.clients || []; state.requests = requests.access_requests || [];
    state.pendingRequests = state.requests.filter((request) => request.status === 'pending');
    state.audit = audit.audit || []; state.models = models.models || [];
    const grantResults = await Promise.all(state.clients.map(async (client) => {
      const data = await api(`/admin/proxy/clients/${encodeURIComponent(client.id)}/grants`, {}, allowUnauthorized);
      return [client.id, data.grants || []];
    }));
    state.grants = new Map(grantResults);
    renderAll();
  }

  function navigate(page) {
    state.page = page;
    document.querySelectorAll('.page').forEach((node) => node.classList.toggle('active', node.id === `page-${page}`));
    document.querySelectorAll('[data-page]').forEach((node) => node.classList.toggle('active', node.closest('.nav') && node.dataset.page === page));
    if (page === 'overview' && state.adminToken) loadCore().catch((error) => flash(error.message, true));
    if (page === 'models') loadGrants();
    if (page === 'requests') loadRequests();
    if (page === 'activity') loadAudit();
    $('main').focus({ preventScroll: true }); window.scrollTo({ top: 0, behavior: 'smooth' });
  }

  function renderAll() {
    renderClientOptions(); renderOverview(); renderClients(); renderRequests(); renderAudit(); renderModels();
    const pending = state.pendingRequests.length;
    $('requestBadge').hidden = !pending; $('requestBadge').textContent = String(pending);
  }

  function renderClientOptions() {
    const currentGrant = $('grantClientSelect').value;
    $('grantClientSelect').replaceChildren();
    state.clients.forEach((client) => { const option = el('option', '', client.name); option.value = client.id; $('grantClientSelect').appendChild(option); });
    if (state.clients.some((c) => c.id === currentGrant)) $('grantClientSelect').value = currentGrant;
    $('manualGrantForm').hidden = !state.clients.length;
    const currentAudit = $('auditClientSelect').value;
    $('auditClientSelect').replaceChildren(); const all = el('option', '', 'All clients'); all.value = ''; $('auditClientSelect').appendChild(all);
    state.clients.forEach((client) => { const option = el('option', '', client.name); option.value = client.id; $('auditClientSelect').appendChild(option); });
    if (state.clients.some((c) => c.id === currentAudit)) $('auditClientSelect').value = currentAudit;
  }

  function renderOverview() {
    const pending = state.pendingRequests;
    const activeClients = state.clients.filter((c) => !c.disabled).length;
    const grants = [...state.grants.values()].reduce((sum, list) => sum + list.length, 0);
    $('stats').replaceChildren(...[
      ['Clients', state.clients.length], ['Active', activeClients], ['Pending', pending.length], ['Model grants', grants]
    ].map(([label, value]) => { const node = el('div', 'stat'); node.append(el('strong', '', String(value)), el('span', '', label)); return node; }));
    const requestBox = $('overviewRequests'); requestBox.replaceChildren();
    pending.slice(0, 5).forEach((request) => { const row = el('div', 'list-row'); const main = el('div', 'list-main'); main.append(el('strong', '', `${request.provider || 'Unknown'} / ${request.model || 'unresolved'}`), el('span', '', `${clientName(request.client_id)} · ${helpers.relativeTime(request.updated_at)}`)); row.append(main, badge('Pending', 'pending')); requestBox.appendChild(row); });
    if (!pending.length) requestBox.appendChild(empty('No requests need review.'));
    const activityBox = $('overviewActivity'); activityBox.replaceChildren();
    state.audit.slice(0, 5).forEach((entry) => { const row = el('div', 'list-row'); const main = el('div', 'list-main'); main.append(el('strong', '', entry.action || 'activity'), el('span', '', `${clientName(entry.client_id)} · ${helpers.relativeTime(entry.created_at)}`)); row.append(main, badge(entry.decision || 'event', entry.decision || '')); activityBox.appendChild(row); });
    if (!state.audit.length) activityBox.appendChild(empty('No activity has been recorded.'));
  }

  function renderClients() {
    const box = $('clientsList'); box.replaceChildren();
    state.clients.forEach((client) => {
      const card = el('article', 'card'); const head = el('div', 'card-head'); const title = el('div');
      title.append(el('h2', '', client.name), el('div', 'meta', '',));
      title.lastChild.textContent = `Created ${helpers.relativeTime(client.created_at)}`;
      head.append(title, badge(client.disabled ? 'Disabled' : 'Active', client.disabled ? 'disabled' : 'active'));
      card.append(head, el('p', '', client.description || 'No description provided.'));
      const actions = el('div', 'card-actions'); actions.append(button('Manage', 'button secondary', 'manage-client', client.id), button('Models & grants', 'button secondary', 'client-grants', client.id)); card.appendChild(actions); box.appendChild(card);
    });
    if (!state.clients.length) box.appendChild(empty('No clients yet. Create one to begin issuing credentials.'));
  }

  async function openClient(id) {
    state.detailClientID = id; const client = state.clients.find((c) => c.id === id); if (!client) return;
    $('detailTitle').textContent = client.name; $('detailDescription').textContent = client.description || 'No description provided.';
    $('toggleClientButton').textContent = client.disabled ? 'Enable client' : 'Disable client'; $('toggleClientButton').dataset.disabled = String(!client.disabled);
    $('issueTokenButton').disabled = client.disabled; showModal('clientDetailModal');
    $('tokenList').replaceChildren(empty('Loading credentials…'));
    try { const data = await api(`/admin/proxy/clients/${encodeURIComponent(id)}/tokens`); renderTokens(data.tokens || []); }
    catch (error) { $('tokenList').replaceChildren(empty(error.message)); }
  }

  function renderTokens(tokens) {
    const box = $('tokenList'); box.replaceChildren();
    tokens.slice().reverse().forEach((token) => {
      const expired = token.expires_at && new Date(token.expires_at).getTime() <= Date.now();
      const row = el('div', 'list-row'); const main = el('div', 'list-main');
      main.append(el('strong', '', `${token.prefix}… ${token.note || ''}`.trim()), el('span', '', token.expires_at ? `Expires ${helpers.relativeTime(token.expires_at)}` : 'Never expires'));
      const status = token.revoked ? badge('Revoked', 'revoked') : expired ? badge('Expired', 'disabled') : button('Revoke', 'button danger', 'revoke-token', token.id);
      row.append(main, status); box.appendChild(row);
    });
    if (!tokens.length) box.appendChild(empty('No credentials issued.'));
  }

  async function loadGrants() {
    const id = $('grantClientSelect').value;
    if (!id) { renderModels(); return; }
    try { const data = await api(`/admin/proxy/clients/${encodeURIComponent(id)}/grants`); state.grants.set(id, data.grants || []); renderModels(); renderOverview(); }
    catch (error) { flash(error.message, true); }
  }

  function renderModels() {
    const box = $('modelsList'); box.replaceChildren(); const clientID = $('grantClientSelect').value;
    if (!state.clients.length) { box.appendChild(empty('Create a client before granting model access.')); return; }
    const grants = state.grants.get(clientID) || []; const query = $('modelSearch').value.trim().toLowerCase();
    state.models.filter((model) => !query || `${model.provider} ${model.model} ${model.alias} ${model.display || ''}`.toLowerCase().includes(query)).forEach((model) => {
      const grant = grants.find((g) => g.provider.toLowerCase() === model.provider.toLowerCase() && g.model.toLowerCase() === model.model.toLowerCase());
      const row = el('div', 'model-row'); const main = el('div'); main.append(el('strong', '', model.alias), el('small', '', model.display || (model.model === '*' ? 'All models from this provider' : model.provider)));
      row.append(main, grant ? button('Revoke', 'button danger', 'revoke-grant', grant.id) : button('Grant', 'button secondary', 'grant-model', `${model.provider}\n${model.model}`)); box.appendChild(row);
    });
    if (!box.childNodes.length) box.appendChild(empty('No models match this search.'));
  }

  async function loadRequests() {
    try { const suffix = state.requestStatus ? `?status=${encodeURIComponent(state.requestStatus)}` : ''; const data = await api(`/admin/proxy/requests${suffix}`); state.requests = data.access_requests || []; renderRequests(); const pendingData = state.requestStatus === 'pending' ? data : await api('/admin/proxy/requests?status=pending'); state.pendingRequests = pendingData.access_requests || []; const count = state.pendingRequests.length; $('requestBadge').hidden = !count; $('requestBadge').textContent = String(count); renderOverview(); }
    catch (error) { flash(error.message, true); }
  }

  function renderRequests() {
    const box = $('requestsList'); box.replaceChildren();
    state.requests.forEach((request) => {
      const card = el('article', 'card'); const head = el('div', 'card-head'); const title = el('div');
      title.append(el('h3', '', `${request.provider || 'Unknown provider'} / ${request.model || 'unresolved model'}`), el('div', 'meta', `${clientName(request.client_id)} · ${helpers.relativeTime(request.updated_at)}`)); head.append(title, badge(request.status, request.status)); card.appendChild(head);
      if (request.reason) card.appendChild(el('p', '', `“${request.reason}”`));
      if (request.note) card.appendChild(el('p', '', `Decision: ${request.note}`));
      if (request.count > 1) card.appendChild(el('div', 'meta', `Requested ${request.count} times`));
      if (request.status === 'pending') { const actions = el('div', 'card-actions'); actions.append(button('Review', 'button primary', 'review-request', request.id)); card.appendChild(actions); }
      box.appendChild(card);
    });
    if (!state.requests.length) box.appendChild(empty(`No ${state.requestStatus || ''} access requests.`.trim()));
  }

  async function loadAudit() {
    const params = new URLSearchParams({ limit: $('auditLimit').value }); if ($('auditClientSelect').value) params.set('client_id', $('auditClientSelect').value);
    try { const data = await api(`/admin/proxy/audit?${params}`); state.audit = data.audit || []; renderAudit(); }
    catch (error) { flash(error.message, true); }
  }

  function renderAudit() {
    const box = $('auditList'); box.replaceChildren();
    state.audit.forEach((entry) => { const row = el('div', 'audit-row'); row.append(el('i', `audit-dot ${entry.decision || entry.action || ''}`)); const body = el('div'); const route = [entry.provider, entry.model].filter(Boolean).join('/'); body.append(el('strong', '', `${entry.action || 'event'}${entry.decision ? ` · ${entry.decision}` : ''}`), el('p', '', [clientName(entry.client_id), route, entry.detail, helpers.relativeTime(entry.created_at)].filter(Boolean).join(' · '))); row.appendChild(body); box.appendChild(row); });
    if (!state.audit.length) box.appendChild(empty('No audit entries match this filter.'));
  }

  async function grant(provider, model, note = '') {
    const clientID = $('grantClientSelect').value; if (!clientID) return;
    await api(`/admin/proxy/clients/${encodeURIComponent(clientID)}/grants`, { method: 'POST', body: JSON.stringify({ provider, model, note }) });
    flash(`Granted ${provider}/${model}`); await loadGrants();
  }

  async function decide(approve) {
    const id = state.decisionRequestID; if (!id) return;
    await api(`/admin/proxy/requests/${encodeURIComponent(id)}/${approve ? 'approve' : 'deny'}`, { method: 'POST', body: JSON.stringify({ note: $('decisionNote').value.trim() }) });
    hideModal($('decisionModal')); $('decisionForm').reset(); flash(approve ? 'Request approved and grant created' : 'Request denied'); await loadRequests();
  }

  document.addEventListener('click', async (event) => {
    const pageButton = event.target.closest('[data-page]'); if (pageButton) { navigate(pageButton.dataset.page); return; }
    const target = event.target.closest('[data-action]'); if (!target) return;
    try {
      const { action, id } = target.dataset;
      if (action === 'manage-client') await openClient(id);
      if (action === 'client-grants') { $('grantClientSelect').value = id; navigate('models'); }
      if (action === 'revoke-token' && confirm('Revoke this credential? This cannot be undone.')) { await api(`/admin/proxy/tokens/${encodeURIComponent(id)}`, { method: 'DELETE' }); flash('Credential revoked'); await openClient(state.detailClientID); }
      if (action === 'grant-model') { const [provider, model] = id.split('\n'); await grant(provider, model); }
      if (action === 'revoke-grant' && confirm('Revoke this model grant?')) { await api(`/admin/proxy/grants/${encodeURIComponent(id)}`, { method: 'DELETE' }); flash('Grant revoked'); await loadGrants(); }
      if (action === 'review-request') { state.decisionRequestID = id; const request = state.requests.find((r) => r.id === id); $('decisionSummary').textContent = `${clientName(request.client_id)} requests ${request.provider || 'unknown'}/${request.model || 'unresolved'}.`; $('decisionNote').value = ''; showModal('decisionModal'); }
    } catch (error) { flash(error.message, true); }
  });

  document.querySelectorAll('.close-modal').forEach((node) => node.addEventListener('click', () => hideModal(node.closest('.modal-backdrop'))));
  $('authForm').addEventListener('submit', async (event) => { event.preventDefault(); const candidate = $('adminTokenInput').value.trim(); state.adminToken = candidate; $('authError').textContent = ''; try { await loadCore(true); $('adminTokenInput').value = ''; hideModal($('authModal')); setConnection('online', 'Connected'); } catch (error) { state.adminToken = ''; $('adminTokenInput').value = ''; $('authError').textContent = error.message === 'invalid admin token' ? error.message : `Could not connect: ${error.message}`; setConnection('offline', 'Locked'); } });
  $('lockButton').addEventListener('click', lock);
  $('refreshButton').addEventListener('click', async () => { try { await loadCore(); if (state.page === 'models') await loadGrants(); flash('Data refreshed'); } catch (error) { flash(error.message, true); } });
  $('createClientButton').addEventListener('click', () => { $('clientForm').reset(); showModal('clientModal'); });
  $('clientForm').addEventListener('submit', async (event) => { event.preventDefault(); try { const data = await api('/admin/proxy/clients', { method: 'POST', body: JSON.stringify({ name: $('clientName').value.trim(), description: $('clientDescription').value.trim() }) }); hideModal($('clientModal')); await loadCore(); flash(`Created ${data.client.name}`); await openClient(data.client.id); } catch (error) { flash(error.message, true); } });
  $('toggleClientButton').addEventListener('click', async () => { try { await api(`/admin/proxy/clients/${encodeURIComponent(state.detailClientID)}`, { method: 'PATCH', body: JSON.stringify({ disabled: $('toggleClientButton').dataset.disabled === 'true' }) }); await loadCore(); await openClient(state.detailClientID); flash('Client updated'); } catch (error) { flash(error.message, true); } });
  $('issueTokenButton').addEventListener('click', () => { $('tokenForm').reset(); $('customTTLLabel').hidden = true; hideModal($('clientDetailModal')); showModal('tokenFormModal'); });
  $('tokenTTL').addEventListener('change', () => { $('customTTLLabel').hidden = $('tokenTTL').value !== 'custom'; });
  $('tokenForm').addEventListener('submit', async (event) => { event.preventDefault(); try { const ttl = helpers.ttlSeconds($('tokenTTL').value, $('customTTL').value); const data = await api(`/admin/proxy/clients/${encodeURIComponent(state.detailClientID)}/tokens`, { method: 'POST', body: JSON.stringify({ ttl_seconds: ttl, note: $('tokenNote').value.trim() }) }); hideModal($('tokenFormModal')); $('tokenForm').reset(); state.oneTimeToken = data.token; $('secretValue').textContent = state.oneTimeToken; showModal('secretModal'); } catch (error) { flash(error.message, true); } });
  $('copySecretButton').addEventListener('click', async () => { if (!state.oneTimeToken) return; try { await navigator.clipboard.writeText(state.oneTimeToken); $('copySecretButton').textContent = 'Copied'; } catch (_) { flash('Copy failed; select the token manually', true); } });
  $('dismissSecretButton').addEventListener('click', async () => { state.oneTimeToken = ''; helpers.clearSecret($('secretValue')); $('copySecretButton').textContent = 'Copy token'; hideModal($('secretModal')); await openClient(state.detailClientID); });
  $('grantClientSelect').addEventListener('change', loadGrants); $('modelSearch').addEventListener('input', renderModels);
  $('manualGrantForm').addEventListener('submit', async (event) => { event.preventDefault(); try { await grant($('grantProvider').value.trim(), $('grantModel').value.trim(), $('grantNote').value.trim()); event.target.reset(); } catch (error) { flash(error.message, true); } });
  $('requestFilters').addEventListener('click', (event) => { const target = event.target.closest('[data-status]'); if (!target) return; state.requestStatus = target.dataset.status; $('requestFilters').querySelectorAll('button').forEach((b) => b.classList.toggle('active', b === target)); loadRequests(); });
  $('decisionForm').addEventListener('submit', async (event) => { event.preventDefault(); try { await decide(true); } catch (error) { flash(error.message, true); } });
  $('denyRequestButton').addEventListener('click', async () => { try { await decide(false); } catch (error) { flash(error.message, true); } });
  $('auditClientSelect').addEventListener('change', loadAudit); $('auditLimit').addEventListener('change', loadAudit);
  window.addEventListener('pagehide', () => { state.adminToken = ''; state.oneTimeToken = ''; helpers.clearSecret($('secretValue')); });
  document.addEventListener('keydown', (event) => { if (event.key !== 'Escape') return; const open = [...document.querySelectorAll('.modal-backdrop:not([hidden])')].pop(); if (open && open.id !== 'authModal' && open.id !== 'secretModal') hideModal(open); });

  lock();
})();
