'use strict';

(function initAppRuntime() {

const app = window.TermLLMApp;
const createEl = app.createEl;
const {
  UI_PREFIX, STORAGE_KEYS, state, elements, getActiveSession, updateSessionUsageDisplay, splitHeaderModelEffort,
  compactHeaderModelLabel, getDefaultProviderName, getDefaultModelForProvider
} = app;
const { requestHeaders, normalizeError, effectiveEffortForCompare, sessionHasQueueableActiveRun, setSessionPendingEffort, clearSessionPendingEffort, markRuntimeSelectionIntent, addErrorMessage } = app;

const fetchProviders = async (tokenOverride = '') => {
  const headers = {};
  const token = tokenOverride || state.token;
  if (token) headers.Authorization = `Bearer ${token}`;

  const response = await app.apiFetch(`${UI_PREFIX}/v1/providers`, { headers });
  if (!response.ok) {
    throw await normalizeError(response);
  }

  const data = await response.json().catch(() => ({ data: [] }));
  return Array.isArray(data.data) ? data.data : [];
};

const normalizeModelMetadata = (items) => {
  const ids = [];
  const byID = {};
  (Array.isArray(items) ? items : []).forEach((m) => {
    const id = String(m?.id || '').trim();
    if (!id) return;
    ids.push(id);
    const efforts = Array.isArray(m?.reasoning_efforts)
      ? m.reasoning_efforts.map((v) => String(v || '').trim()).filter(Boolean)
      : [];
    const modes = Array.isArray(m?.reasoning_modes)
      ? m.reasoning_modes.map((v) => String(v || '').trim()).filter(Boolean)
      : [];
    const defaultEffort = String(m?.default_reasoning_effort || '').trim();
    byID[id] = {
      id,
      reasoning_efforts: efforts,
      reasoning_modes: modes,
      ...(defaultEffort ? { default_reasoning_effort: defaultEffort } : {})
    };
  });
  return { ids, byID };
};

const fetchModels = async (tokenOverride = '', provider = '') => {
  const headers = {};
  const token = tokenOverride || state.token;
  if (token) headers.Authorization = `Bearer ${token}`;

  let url = `${UI_PREFIX}/v1/models`;
  if (provider) url += `?provider=${encodeURIComponent(provider)}`;

  const response = await app.apiFetch(url, { headers });
  if (!response.ok) {
    throw await normalizeError(response);
  }

  const data = await response.json().catch(() => ({ data: [] }));
  const { ids, byID } = normalizeModelMetadata(data.data);
  const requestedProvider = String(provider || state.selectedProvider || '').trim();
  if (!requestedProvider || requestedProvider === String(state.selectedProvider || '').trim()) {
    state.modelInfoByID = byID;
  }
  return ids;
};

// ===== Provider picker =====

// Clear stale selectedProvider if it no longer exists in the fetched provider list.
const normalizeSelectedProvider = () => {
  if (!state.selectedProvider) return;
  const exists = state.providers.some((p) => p.name === state.selectedProvider);
  if (!exists) {
    state.selectedProvider = '';
    localStorage.removeItem(STORAGE_KEYS.selectedProvider);
  }
};

const populateProviderSelectOptions = (sel, providers, previous) => {
  if (!sel) return;
  sel.innerHTML = '';

  const autoOption = document.createElement('option');
  autoOption.value = '';
  autoOption.textContent = 'Auto (server default)';
  sel.appendChild(autoOption);

  providers.filter((p) => p.configured || p.is_default).forEach((p) => {
    const option = document.createElement('option');
    option.value = p.name;
    option.textContent = p.name + (p.is_default ? ' (default)' : '');
    sel.appendChild(option);
  });

  sel.value = previous;
};

const renderProviderOptions = () => {
  const previous = state.selectedProvider;
  populateProviderSelectOptions(elements.providerSelect, state.providers, previous);
  populateProviderSelectOptions(elements.chipProviderSelect, state.providers, previous);
};

let providerChangeSequence = 0;

const applyProviderChange = async (provider) => {
  const changeSequence = ++providerChangeSequence;
  markRuntimeSelectionIntent();
  state.selectedProvider = provider;
  if (provider) {
    localStorage.setItem(STORAGE_KEYS.selectedProvider, provider);
  } else {
    localStorage.removeItem(STORAGE_KEYS.selectedProvider);
  }
  state.selectedModel = '';
  localStorage.removeItem(STORAGE_KEYS.selectedModel);

  const providerInfo = state.providers.find((p) => p.name === provider);
  state.models = providerInfo?.models?.length ? providerInfo.models : [];
  state.modelInfoByID = {};
  renderModelOptions();

  // Reflect the clicked provider immediately. Fetching the model list can be
  // slow, and the header chip should not keep showing the previous provider
  // while that async refresh is in flight. Rendering the provider's configured
  // model fallback (or an empty list) also avoids briefly exposing stale models
  // from the previously selected provider.
  syncSettingsSelectValues();
  app.persistActiveProjectDraft?.();
  app.updateHeader();

  let models;
  try {
    models = await fetchModels('', provider);
  } catch {
    models = providerInfo?.models?.length ? providerInfo.models : [];
  }
  if (changeSequence !== providerChangeSequence || state.selectedProvider !== provider) {
    return;
  }
  state.models = models;
  renderModelOptions();
  syncSettingsSelectValues();
  app.updateHeader();
};

const resolveEffectiveModelForEffort = (model, effort) => {
  const split = splitHeaderModelEffort(model || '', effort || '', state.models);
  if (split.model) return split.model;
  const provider = state.selectedProvider || getDefaultProviderName?.() || '';
  return getDefaultModelForProvider?.(provider) || '';
};

const effectiveModelForEffort = () => resolveEffectiveModelForEffort(state.selectedModel, state.selectedEffort);

const modelMetadataFor = (model) => {
  const id = String(model || '').trim();
  if (!id || !state.modelInfoByID) return null;
  return Object.prototype.hasOwnProperty.call(state.modelInfoByID, id) ? state.modelInfoByID[id] : null;
};

const reasoningEffortsForModel = (model) => {
  const info = modelMetadataFor(model);
  return Array.isArray(info?.reasoning_efforts)
    ? info.reasoning_efforts.map((v) => String(v || '').trim()).filter(Boolean)
    : [];
};

const LEGACY_REASONING_EFFORTS = ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'];

const allowedReasoningEffortsForSelection = () => {
  const model = effectiveModelForEffort();
  const info = modelMetadataFor(model);
  const efforts = reasoningEffortsForModel(model);
  if (!info || efforts.length === 0) return LEGACY_REASONING_EFFORTS;
  return efforts;
};

const populateEffortSelectOptions = (sel, efforts, previous) => {
  if (!sel) return;
  sel.innerHTML = '';

  const autoOption = document.createElement('option');
  autoOption.value = '';
  autoOption.textContent = 'Auto (server default)';
  sel.appendChild(autoOption);

  efforts.forEach((effort) => {
    const option = document.createElement('option');
    option.value = effort;
    option.textContent = effort;
    sel.appendChild(option);
  });

  sel.disabled = efforts.length === 0;
  sel.value = efforts.includes(previous) ? previous : '';
};

const renderEffortOptions = () => {
  const efforts = allowedReasoningEffortsForSelection();
  const previous = state.selectedEffort || '';
  populateEffortSelectOptions(elements.effortSelect, efforts, previous);
  populateEffortSelectOptions(elements.chipEffortSelect, efforts, previous);
};

const persistRuntimeSelection = () => {
  const persist = (key, value) => {
    if (value) {
      localStorage.setItem(key, value);
    } else {
      localStorage.removeItem(key);
    }
  };
  persist(STORAGE_KEYS.selectedProvider, state.selectedProvider || '');
  persist(STORAGE_KEYS.selectedModel, state.selectedModel || '');
  persist(STORAGE_KEYS.selectedEffort, state.selectedEffort || '');
  app.persistActiveProjectDraft?.();
};

const canonicalizeSelectedModelEffort = () => {
  const split = splitHeaderModelEffort(state.selectedModel, state.selectedEffort, state.models);
  let nextModel = split.model;
  let nextEffort = split.effort;
  const effectiveModel = resolveEffectiveModelForEffort(nextModel, nextEffort);
  const info = modelMetadataFor(effectiveModel);
  const allowed = info ? reasoningEffortsForModel(effectiveModel) : [];
  if (nextEffort && info && allowed.length > 0 && !allowed.includes(nextEffort)) {
    nextEffort = '';
  }
  if (nextModel === (state.selectedModel || '') && nextEffort === (state.selectedEffort || '')) {
    return false;
  }
  state.selectedModel = nextModel;
  state.selectedEffort = nextEffort;
  persistRuntimeSelection();
  return true;
};

const applyModelChange = (model) => {
  markRuntimeSelectionIntent();
  state.selectedModel = model;
  canonicalizeSelectedModelEffort();
  renderEffortOptions();
  persistRuntimeSelection();
  syncSettingsSelectValues();
  app.updateHeader();
};

const queueActiveRunEffortChange = async (session, effort) => {
  const targetEffort = String(effort || '').trim();
  const model = String(session?.activeModel || state.selectedModel || '').trim();
  if (!session || !session.id || !model) return false;

  const response = await app.apiFetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(session.id)}/runtime/effort`, {
    method: 'POST',
    headers: requestHeaders(session.id),
    body: JSON.stringify({
      model,
      reasoning_effort: targetEffort,
    }),
  });
  if (!response.ok) {
    throw await normalizeError(response);
  }
  const payload = await response.json().catch(() => ({}));
  const queuedEffort = Object.prototype.hasOwnProperty.call(payload || {}, 'reasoning_effort')
    ? String(payload.reasoning_effort || '').trim()
    : targetEffort;

  state.selectedEffort = queuedEffort;
  canonicalizeSelectedModelEffort();
  persistRuntimeSelection();
  syncSettingsSelectValues();

  if (effectiveEffortForCompare(model, queuedEffort) === effectiveEffortForCompare(model, session.activeEffort || '')) {
    clearSessionPendingEffort(session);
  } else {
    setSessionPendingEffort(session, queuedEffort);
  }
  updateSessionUsageDisplay(session);
  app.updateHeader();
  return true;
};

const applyEffortChange = async (effort) => {
  const session = getActiveSession();
  const model = String(session?.activeModel || state.selectedModel || '').trim();
  if (sessionHasQueueableActiveRun(session)
      && effectiveEffortForCompare(model, effort) === effectiveEffortForCompare(model, session.activeEffort || '')) {
    state.selectedEffort = effort;
    clearSessionPendingEffort(session);
    canonicalizeSelectedModelEffort();
    persistRuntimeSelection();
    syncSettingsSelectValues();
    updateSessionUsageDisplay(session);
    app.updateHeader();
    return;
  }
  if (sessionHasQueueableActiveRun(session)) {
    try {
      const queued = await queueActiveRunEffortChange(session, effort);
      if (queued) return;
    } catch (err) {
      const message = err?.message || 'Failed to queue reasoning effort.';
      if (session) addErrorMessage(message, session);
      syncSettingsSelectValues();
      app.updateHeader();
      return;
    }
  }
  markRuntimeSelectionIntent(session);
  state.selectedEffort = effort;
  canonicalizeSelectedModelEffort();
  persistRuntimeSelection();
  syncSettingsSelectValues();
  app.updateHeader();
};

// Keep modal selects mirroring the live state so opening the settings cog never
// shows a stale value vs. what the header chips committed.
const syncSettingsSelectValues = () => {
  if (elements.providerSelect) elements.providerSelect.value = state.selectedProvider || '';
  if (elements.modelSelect) elements.modelSelect.value = state.selectedModel || '';
  if (elements.effortSelect) elements.effortSelect.value = state.selectedEffort || '';
  if (elements.chipProviderSelect) elements.chipProviderSelect.value = state.selectedProvider || '';
  if (elements.chipModelSelect) elements.chipModelSelect.value = state.selectedModel || '';
  if (elements.chipEffortSelect) elements.chipEffortSelect.value = state.selectedEffort || '';
};

elements.providerSelect.addEventListener('change', () => {
  void applyProviderChange(elements.providerSelect.value);
});

elements.modelSelect?.addEventListener('change', () => {
  applyModelChange(elements.modelSelect.value);
});

// Modal effort does not commit live: Cancel must discard the pending value.
// Its change listener records explicit user intent; settings Save commits it.

// The header chip below commits live, matching provider/model behavior.

elements.chipProviderSelect?.addEventListener('change', () => {
  void applyProviderChange(elements.chipProviderSelect.value);
});

elements.chipModelSelect?.addEventListener('change', () => {
  applyModelChange(elements.chipModelSelect.value);
});

elements.chipEffortSelect?.addEventListener('change', () => {
  void applyEffortChange(elements.chipEffortSelect.value);
});

// ===== Custom chip popover =====
// Replaces the native <select> dropdown UI: native pickers are inconsistent
// across OSes, ugly, and can render off-screen. The underlying <select> is kept
// for state/sync — popover items dispatch a 'change' event on it on selection.
const chipPopoverState = { selectEl: null, triggerEl: null, filterInput: null, mode: '' };

const buildChipOptionLabel = (opt) => {
  const text = opt.textContent || opt.value || '';
  const value = opt.value || '';
  if (!value) {
    return { primary: text, meta: '' };
  }
  const defaultMatch = text.match(/^(.*?)\s*\((.+)\)\s*$/);
  if (defaultMatch) {
    return { primary: defaultMatch[1], meta: defaultMatch[2] };
  }
  return { primary: text, meta: '' };
};

const positionChipPopover = (triggerEl, pop = elements.chipPopover, options = {}) => {
  if (!pop || !triggerEl?.getBoundingClientRect) return;
  pop.hidden = false;

  const vv = window.visualViewport;
  const viewportWidth = vv ? Math.round(vv.width) : window.innerWidth;
  const viewportHeight = vv ? Math.round(vv.height) : window.innerHeight;
  const viewportOffsetLeft = vv ? Math.max(0, Math.round(vv.offsetLeft)) : 0;
  const viewportOffsetTop = vv ? Math.max(0, Math.round(vv.offsetTop)) : 0;

  if (viewportWidth <= 540 && options.mobileSheet) {
    const viewportBottomInset = Math.max(0, window.innerHeight - viewportOffsetTop - viewportHeight);
    const sheetMaxHeight = Math.round(viewportHeight * 0.6);
    pop.style.left = `calc(${viewportOffsetLeft}px + 0.5rem + var(--safe-left))`;
    pop.style.top = 'auto';
    pop.style.right = 'auto';
    pop.style.bottom = `calc(${viewportBottomInset}px + 0.5rem + var(--safe-bottom))`;
    pop.style.width = `calc(${viewportWidth}px - 1rem - var(--safe-left) - var(--safe-right))`;
    pop.style.minWidth = '';
    pop.style.maxWidth = 'none';
    pop.style.maxHeight = `calc(${sheetMaxHeight}px - 1rem - var(--safe-top) - var(--safe-bottom))`;
    return;
  }

  if (viewportWidth <= 540) {
    // On iPhone Safari the on-screen keyboard shrinks the visual viewport, but
    // CSS vh units and fixed bottom sheets can still end up underneath it. Pin
    // the picker to the visible viewport instead of the layout viewport so the
    // whole sheet stays inside the safe area while typing in the filter box.
    pop.style.left = `calc(${viewportOffsetLeft}px + 0.5rem + var(--safe-left))`;
    pop.style.top = `calc(${viewportOffsetTop}px + 0.5rem + var(--safe-top))`;
    pop.style.right = 'auto';
    pop.style.bottom = 'auto';
    pop.style.width = `calc(${viewportWidth}px - 1rem - var(--safe-left) - var(--safe-right))`;
    pop.style.minWidth = '';
    pop.style.maxWidth = 'none';
    pop.style.maxHeight = `calc(${viewportHeight}px - 1rem - var(--safe-top) - var(--safe-bottom))`;
    return;
  }

  pop.style.width = '';
  const rect = triggerEl.getBoundingClientRect();
  const margin = 6;
  pop.style.minWidth = `${Math.max(180, rect.width)}px`;
  pop.style.maxWidth = '';
  pop.style.right = 'auto';
  pop.style.bottom = 'auto';
  const popRect = pop.getBoundingClientRect();
  let left = rect.left;
  if (left + popRect.width > window.innerWidth - margin) {
    left = Math.max(margin, window.innerWidth - margin - popRect.width);
  }
  let top = rect.bottom + 4;
  if (top + popRect.height > window.innerHeight - margin) {
    const above = rect.top - 4 - popRect.height;
    top = above >= margin ? above : Math.max(margin, window.innerHeight - margin - popRect.height);
  }
  pop.style.left = `${Math.max(margin, left)}px`;
  pop.style.top = `${Math.max(margin, top)}px`;
  pop.style.maxHeight = '';
};

const closeChipPopover = () => {
  const pop = elements.chipPopover;
  if (!pop || pop.hidden) return;
  pop.hidden = true;
  pop.innerHTML = '';
  pop.classList?.remove('chip-popover-runtime');
  if (elements.chipPopoverBackdrop) elements.chipPopoverBackdrop.hidden = true;
  if (chipPopoverState.triggerEl) {
    chipPopoverState.triggerEl.setAttribute('aria-expanded', 'false');
  }
  chipPopoverState.selectEl = null;
  chipPopoverState.triggerEl = null;
  chipPopoverState.filterInput = null;
  chipPopoverState.mode = '';
};

const focusChipPopoverItem = (item) => {
  if (!item) return;
  const pop = elements.chipPopover;
  pop?.querySelectorAll?.('.chip-popover-item.focused').forEach((el) => {
    el.classList.remove('focused');
  });
  item.classList.add('focused');
  item.focus?.({ preventScroll: false });
};

// Items matching the active filter (or all items when no filter is shown).
// Keyboard navigation skips items hidden by the filter.
const visibleChipPopoverItems = () => {
  const pop = elements.chipPopover;
  const items = pop?.querySelectorAll?.('.chip-popover-item');
  if (!items) return [];
  return Array.from(items).filter((el) => !el.hidden);
};

const moveChipPopoverFocus = (direction) => {
  const pop = elements.chipPopover;
  if (!pop) return;
  const items = visibleChipPopoverItems();
  if (items.length === 0) return;
  const current = pop.querySelector('.chip-popover-item.focused')
    || pop.querySelector('.chip-popover-item[aria-selected="true"]');
  let idx = current ? items.indexOf(current) : -1;
  idx = idx + direction;
  if (idx < 0) idx = items.length - 1;
  if (idx >= items.length) idx = 0;
  focusChipPopoverItem(items[idx]);
};

// Show this many items before adding a filter input. Below this threshold the
// filter just adds noise to small pickers (effort, provider list).
const CHIP_POPOVER_FILTER_THRESHOLD = 10;

const applyChipPopoverFilter = (query) => {
  const pop = elements.chipPopover;
  if (!pop) return;
  const q = (query || '').trim().toLowerCase();
  const items = pop.querySelectorAll?.('.chip-popover-item') || [];
  let firstVisible = null;
  items.forEach((el) => {
    const haystack = el.dataset?.search || '';
    const match = el.dataset?.filterPersistent === 'true' || !q || haystack.includes(q);
    el.hidden = !match;
    if (match && !firstVisible) firstVisible = el;
  });
  // Re-focus the first visible item so Enter/ArrowDown work intuitively after
  // typing — without this, focus could be on a now-hidden item.
  pop.querySelectorAll('.chip-popover-item.focused').forEach((el) => {
    if (el.hidden) el.classList.remove('focused');
  });
  if (firstVisible && !pop.querySelector('.chip-popover-item.focused')) {
    firstVisible.classList.add('focused');
  }
};

const commitChipPopoverItem = (item) => {
  const selectEl = chipPopoverState.selectEl;
  if (!item || !selectEl) return;
  if (typeof item._chipPopoverAction === 'function') {
    const action = item._chipPopoverAction;
    closeChipPopover();
    action();
    return;
  }
  const value = item.dataset.value || '';
  if (selectEl.value !== value) {
    selectEl.value = value;
    selectEl.dispatchEvent(new Event('change', { bubbles: true }));
  }
  closeChipPopover();
};

const openChipPopover = (selectEl, triggerEl, config = {}) => {
  const pop = elements.chipPopover;
  if (!pop || !selectEl) return;
  if (chipPopoverState.triggerEl === triggerEl) {
    closeChipPopover();
    return;
  }
  closeChipPopover();
  pop.setAttribute('role', 'listbox');
  pop.removeAttribute('aria-label');
  pop.classList?.remove('chip-popover-runtime');
  chipPopoverState.mode = 'select';
  chipPopoverState.selectEl = selectEl;
  chipPopoverState.triggerEl = triggerEl;
  pop.innerHTML = '';

  const options = Array.from(selectEl.options).filter((option) => !option.hidden);
  let filterInput = null;
  if (options.length > CHIP_POPOVER_FILTER_THRESHOLD) {
    filterInput = document.createElement('input');
    filterInput.type = 'text';
    filterInput.className = 'chip-popover-filter';
    filterInput.placeholder = 'Filter…';
    filterInput.setAttribute('aria-label', 'Filter options');
    filterInput.setAttribute('autocomplete', 'off');
    filterInput.setAttribute('spellcheck', 'false');
    filterInput.addEventListener('input', () => applyChipPopoverFilter(filterInput.value));
    filterInput.addEventListener('keydown', (e) => {
      switch (e.key) {
        case 'ArrowDown':
          e.preventDefault();
          moveChipPopoverFocus(1);
          return;
        case 'ArrowUp':
          e.preventDefault();
          moveChipPopoverFocus(-1);
          return;
        case 'Enter': {
          e.preventDefault();
          const focused = pop.querySelector('.chip-popover-item.focused');
          if (focused && !focused.hidden) commitChipPopoverItem(focused);
          return;
        }
        case 'Escape': {
          e.preventDefault();
          const trigger = chipPopoverState.triggerEl;
          closeChipPopover();
          trigger?.focus?.();
          return;
        }
      }
    });
    chipPopoverState.filterInput = filterInput;
    pop.appendChild(filterInput);
  } else {
    chipPopoverState.filterInput = null;
  }

  const currentValue = selectEl.value;
  options.forEach((opt) => {
    const item = createEl('div', 'chip-popover-item');
    item.setAttribute('role', 'option');
    item.tabIndex = -1;
    item.dataset.value = opt.value;
    const { primary, meta } = buildChipOptionLabel(opt);
    item.dataset.search = `${primary} ${meta} ${opt.value}`.toLowerCase();
    if (opt.value === currentValue) item.setAttribute('aria-selected', 'true');
    const label = createEl('span', 'chip-popover-item-label', primary);
    item.appendChild(label);
    if (meta) {
      const metaEl = createEl('span', 'chip-popover-item-meta', meta);
      item.appendChild(metaEl);
    }
    item.addEventListener('click', () => commitChipPopoverItem(item));
    item.addEventListener('mouseenter', () => focusChipPopoverItem(item));
    pop.appendChild(item);
  });
  if (config.action && typeof config.action.onSelect === 'function') {
    const action = createEl('div', 'chip-popover-item chip-popover-item-action');
    action.setAttribute('role', 'option');
    action.setAttribute('aria-selected', 'false');
    action.tabIndex = -1;
    action.dataset.search = String(config.action.label || '').toLowerCase();
    action.dataset.filterPersistent = 'true';
    action._chipPopoverAction = config.action.onSelect;
    action.appendChild(createEl('span', 'chip-popover-item-action-icon', '＋'));
    action.appendChild(createEl('span', 'chip-popover-item-label', config.action.label || 'Add'));
    action.addEventListener('click', () => commitChipPopoverItem(action));
    action.addEventListener('mouseenter', () => focusChipPopoverItem(action));
    pop.appendChild(action);
  }
  triggerEl.setAttribute('aria-expanded', 'true');
  if (elements.chipPopoverBackdrop) elements.chipPopoverBackdrop.hidden = false;
  positionChipPopover(triggerEl);
  const initial = pop.querySelector('.chip-popover-item[aria-selected="true"]')
    || pop.querySelector('.chip-popover-item');
  focusChipPopoverItem(initial);
  // Focus the filter input last so the user can type immediately. The selected
  // item is still highlighted (visually focused) without stealing input focus.
  if (filterInput) filterInput.focus?.();
};

const copySelectOptions = (from, to, formatOption = null) => {
  to.innerHTML = '';
  Array.from(from?.options || []).forEach((opt) => {
    const clone = document.createElement('option');
    clone.value = opt.value;
    clone.textContent = formatOption ? formatOption(opt) : opt.textContent;
    clone.disabled = opt.disabled;
    to.appendChild(clone);
  });
};

const runtimeTriggerLocked = (trigger) => Boolean(trigger?.disabled || trigger?.hasAttribute?.('disabled'));

const runtimeField = ({ label, value, sourceSelect, onChange, formatOption = null, disabled = false }) => {
  const field = createEl('label', 'runtime-popover-field');

  const labelEl = createEl('span', 'runtime-popover-label', label);
  field.appendChild(labelEl);

  const select = createEl('select', 'runtime-popover-select');
  copySelectOptions(sourceSelect, select, formatOption);
  select.value = value || '';
  select.disabled = disabled;
  select.addEventListener('change', async () => {
    select.disabled = true;
    try {
      await onChange(select.value);
    } finally {
      if (chipPopoverState.mode === 'runtime') renderRuntimePopoverContent();
    }
  });
  field.appendChild(select);
  return field;
};

const renderRuntimePopoverContent = () => {
  const pop = elements.chipPopover;
  if (!pop) return;
  pop.innerHTML = '';

  const header = createEl('div', 'runtime-popover-header');
  const title = createEl('div', 'runtime-popover-title', 'Runtime');
  const hint = createEl('div', 'runtime-popover-hint', 'Provider, model, and effort for the next reply');
  header.appendChild(title);
  header.appendChild(hint);
  pop.appendChild(header);

  const fields = createEl('div', 'runtime-popover-fields');
  fields.appendChild(runtimeField({
    label: 'Provider',
    value: state.selectedProvider || '',
    sourceSelect: elements.chipProviderSelect,
    disabled: runtimeTriggerLocked(elements.chipProviderTrigger),
    onChange: (value) => applyProviderChange(value),
  }));
  fields.appendChild(runtimeField({
    label: 'Model',
    value: state.selectedModel || '',
    sourceSelect: elements.chipModelSelect,
    disabled: runtimeTriggerLocked(elements.chipProviderTrigger),
    onChange: (value) => applyModelChange(value),
    formatOption: (opt) => opt.value ? compactHeaderModelLabel(opt.value) : opt.textContent,
  }));
  fields.appendChild(runtimeField({
    label: 'Effort',
    value: state.selectedEffort || '',
    sourceSelect: elements.chipEffortSelect,
    onChange: (value) => applyEffortChange(value),
  }));
  pop.appendChild(fields);
};

const openRuntimePopover = (triggerEl) => {
  const pop = elements.chipPopover;
  if (!pop || !triggerEl) return;
  if (chipPopoverState.mode === 'runtime' && chipPopoverState.triggerEl === triggerEl) {
    closeChipPopover();
    return;
  }
  closeChipPopover();
  chipPopoverState.mode = 'runtime';
  chipPopoverState.selectEl = null;
  chipPopoverState.triggerEl = triggerEl;
  chipPopoverState.filterInput = null;
  pop.setAttribute('role', 'dialog');
  pop.setAttribute('aria-label', 'Runtime settings');
  pop.classList?.add('chip-popover-runtime');
  renderRuntimePopoverContent();
  triggerEl.setAttribute('aria-expanded', 'true');
  if (elements.chipPopoverBackdrop) elements.chipPopoverBackdrop.hidden = false;
  pop.hidden = false;
  positionChipPopover(triggerEl);
  // Leave focus on the trigger. Focusing a native <select> here can open the OS
  // picker immediately, making the runtime panel feel like dueling modals.
};

elements.chipPopoverBackdrop?.addEventListener('click', () => {
  closeChipPopover();
});

const wireChipTrigger = (triggerEl, selectEl) => {
  if (!triggerEl || !selectEl) return;
  triggerEl.addEventListener('click', (e) => {
    e.stopPropagation();
    if (triggerEl === elements.chipModelTrigger) {
      openRuntimePopover(triggerEl);
      return;
    }
    openChipPopover(selectEl, triggerEl);
  });
};

wireChipTrigger(elements.chipProviderTrigger, elements.chipProviderSelect);
wireChipTrigger(elements.chipModelTrigger, elements.chipModelSelect);
wireChipTrigger(elements.chipEffortTrigger, elements.chipEffortSelect);

document.addEventListener('click', (e) => {
  const pop = elements.chipPopover;
  if (!pop || pop.hidden) return;
  if (pop.contains?.(e.target)) return;
  if (chipPopoverState.triggerEl?.contains?.(e.target)) return;
  closeChipPopover();
});

document.addEventListener('keydown', (e) => {
  const pop = elements.chipPopover;
  if (!pop || pop.hidden) return;
  if (chipPopoverState.mode === 'runtime') {
    if (e.key === 'Escape') {
      e.preventDefault();
      const trigger = chipPopoverState.triggerEl;
      closeChipPopover();
      trigger?.focus?.();
    }
    return;
  }
  // The filter input owns its own keydown handler for navigation/commit. Don't
  // run the document-level handler when it's focused — otherwise Space would be
  // preventDefault'd and the user couldn't type spaces.
  if (e.target === chipPopoverState.filterInput) return;
  switch (e.key) {
    case 'Escape': {
      e.preventDefault();
      const trigger = chipPopoverState.triggerEl;
      closeChipPopover();
      trigger?.focus?.();
      return;
    }
    case 'ArrowDown':
      e.preventDefault();
      moveChipPopoverFocus(1);
      return;
    case 'ArrowUp':
      e.preventDefault();
      moveChipPopoverFocus(-1);
      return;
    case 'Home': {
      e.preventDefault();
      const items = visibleChipPopoverItems();
      focusChipPopoverItem(items[0]);
      return;
    }
    case 'End': {
      e.preventDefault();
      const items = visibleChipPopoverItems();
      focusChipPopoverItem(items[items.length - 1]);
      return;
    }
    case 'Enter':
    case ' ': {
      e.preventDefault();
      const focused = pop.querySelector('.chip-popover-item.focused');
      if (focused && !focused.hidden) commitChipPopoverItem(focused);
      return;
    }
    case 'Tab':
      closeChipPopover();
      return;
  }
});

const repositionChipPopover = () => {
  if (chipPopoverState.triggerEl) positionChipPopover(chipPopoverState.triggerEl);
};

window.addEventListener('resize', repositionChipPopover);
window.addEventListener('orientationchange', repositionChipPopover);
if (window.visualViewport) {
  window.visualViewport.addEventListener('resize', repositionChipPopover);
  window.visualViewport.addEventListener('scroll', repositionChipPopover);
}

// ===== Model picker =====
const populateModelSelectOptions = (sel, models, previous) => {
  if (!sel) return;
  sel.innerHTML = '';

  const autoOption = document.createElement('option');
  autoOption.value = '';
  autoOption.textContent = 'Auto (server default)';
  sel.appendChild(autoOption);

  models.forEach((id) => {
    const option = document.createElement('option');
    option.value = id;
    option.textContent = id;
    sel.appendChild(option);
  });

  if (previous && !models.includes(previous)) {
    const custom = document.createElement('option');
    custom.value = previous;
    custom.textContent = `${previous} (custom)`;
    sel.appendChild(custom);
  }

  sel.value = previous;
};

const renderModelOptions = () => {
  canonicalizeSelectedModelEffort();
  const previous = state.selectedModel;
  populateModelSelectOptions(elements.modelSelect, state.models, previous);
  populateModelSelectOptions(elements.chipModelSelect, state.models, previous);
  renderEffortOptions();
};


Object.assign(app, {
  fetchProviders,
  fetchModels,
  normalizeSelectedProvider,
  populateProviderSelectOptions,
  renderProviderOptions,
  providerChangeSequence,
  applyProviderChange,
  resolveEffectiveModelForEffort,
  effectiveModelForEffort,
  modelMetadataFor,
  reasoningEffortsForModel,
  LEGACY_REASONING_EFFORTS,
  allowedReasoningEffortsForSelection,
  populateEffortSelectOptions,
  renderEffortOptions,
  persistRuntimeSelection,
  canonicalizeSelectedModelEffort,
  applyModelChange,
  queueActiveRunEffortChange,
  applyEffortChange,
  syncSettingsSelectValues,
  chipPopoverState,
  buildChipOptionLabel,
  positionChipPopover,
  closeChipPopover,
  focusChipPopoverItem,
  visibleChipPopoverItems,
  moveChipPopoverFocus,
  CHIP_POPOVER_FILTER_THRESHOLD,
  applyChipPopoverFilter,
  commitChipPopoverItem,
  openChipPopover,
  copySelectOptions,
  runtimeField,
  renderRuntimePopoverContent,
  openRuntimePopover,
  wireChipTrigger,
  repositionChipPopover,
  populateModelSelectOptions,
  renderModelOptions
});
})();
