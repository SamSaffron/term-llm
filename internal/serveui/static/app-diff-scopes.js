(() => {
'use strict';

const app = window.TermLLMApp || (window.TermLLMApp = {});
const turnScopes = new Set(['last_turn', 'last_3_turns']);
const scopes = new Set([...turnScopes, 'uncommitted', 'unstaged', 'staged']);
const normalizeDiffScope = (value) => {
  const scope = String(value || 'last_turn').trim().toLowerCase();
  return scopes.has(scope) ? scope : '';
};
const isTurnDiffScope = (value) => turnScopes.has(normalizeDiffScope(value));
const normalizeDiffSummary = (value) => {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null;
  const counts = [value.file_count, value.adds, value.dels].map(Number);
  if (!counts.every((number) => Number.isSafeInteger(number) && number >= 0)) return null;
  return { fileCount: counts[0], adds: counts[1], dels: counts[2], git: Boolean(value.git) };
};
const diffTotals = (ds) => {
  if (!ds) return { fileCount: 0, adds: 0, dels: 0 };
  if (!ds.listLoaded && ds.summaryKnown) return ds.summary;
  const totals = { fileCount: ds.files.size, adds: 0, dels: 0 };
  ds.files.forEach((entry) => { totals.adds += entry.adds; totals.dels += entry.dels; });
  return totals;
};
const applyDiffSummary = ({ owner, value, sessionState, activeSessionId, reconcile, render }) => {
  owner = String(owner || '').trim();
  const summary = normalizeDiffSummary(value);
  if (!owner || !summary) return false;
  const ds = sessionState(owner);
  Object.assign(ds, { gitKnown: true, git: summary.git });
  if (ds.scope !== 'last_turn') {
    if (owner === activeSessionId()) render(owner); return true;
  }
  Object.assign(ds, { summaryKnown: true, summary });
  if (summary.fileCount === 0) {
    ds.files.clear();
    ds.listLoaded = true;
    reconcile(owner, ds);
  } else if (ds.files.size === 0) ds.listLoaded = false;
  if (owner === activeSessionId()) render(owner);
  return true;
};
const selectedScopeLabel = (select) => {
  const option = Array.from(select?.options || []).find((entry) => entry.value === select.value);
  return option?.textContent || option?.label || select?.value || '';
};
const renderDiffScope = (select, trigger, label, ds) => {
  if (!select) return;
  const show = Boolean(ds.gitKnown);
  Array.from(select.options || []).forEach((option) => { option.hidden = !ds.git && !isTurnDiffScope(option.value); });
  if (!show && app.chipPopoverState?.triggerEl === trigger) app.closeChipPopover?.();
  select.hidden = !show;
  trigger.hidden = !show;
  if (show) {
    select.removeAttribute?.('hidden');
    trigger.removeAttribute?.('hidden');
  } else {
    select.setAttribute?.('hidden', '');
    trigger.setAttribute?.('hidden', '');
  }
  if (select.value !== ds.scope) select.value = ds.scope;
  if (label) label.textContent = selectedScopeLabel(select);
};

const wireDiffScopePicker = (trigger, select) => {
  trigger?.addEventListener?.('click', (event) => {
    event.stopPropagation?.();
    app.openChipPopover?.(select, trigger);
  });
};

const createDiffScopeSetter = ({ activeSessionId, sessionState, render, fetchList, clearComments }) => (value) => {
  const sessionId = activeSessionId();
  if (!sessionId) return;
  const ds = sessionState(sessionId);
  const scope = normalizeDiffScope(value) || 'last_turn';
  if (scope === ds.scope) return;
  ds.scope = scope;
  [ds.files, ds.expanded, ds.userCollapsed, ds.userExpanded, ds.rowLimits, ds.diffCache,
    ds.dirtyPaths, ds.fetchErrors, ds.inflight, ds.blocks].forEach((collection) => collection.clear());
  Object.assign(ds, { autoExpandedPath: '', pendingScrollPath: '', listLoaded: false, summaryKnown: false,
    summary: { fileCount: 0, adds: 0, dels: 0, git: ds.git } });
  clearComments?.(sessionId);
  render(sessionId);
  void fetchList(sessionId);
};
Object.assign(app, {
  DIFF_SCOPES: scopes,
  normalizeDiffScope, isTurnDiffScope,
  normalizeDiffSummary, diffTotals, applyDiffSummary, renderDiffScope,
  wireDiffScopePicker, createDiffScopeSetter
});
})();
