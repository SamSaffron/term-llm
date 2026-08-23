(() => {
'use strict';

// Live diff sidebar: defaults to changes from the latest agent turn and can
// switch to Git worktree/index scopes when the session is in a repository.
// Files render as an accordion ordered by recency: each one expands inline
// where it sits in the list and can be collapsed individually. Rendering is
// keyed by path — blocks are reused across renders so live updates patch the
// existing DOM instead of rebuilding it (no flicker, selection/focus survive).
const app = window.TermLLMApp || (window.TermLLMApp = {});
const {
  state,
  elements,
  STORAGE_KEYS,
  UI_PREFIX,
  createEl,
  setAnimatedPanelOpen: setPanelOpen,
  setElementHidden: setPanelHidden,
  initPanelSwipeToClose
} = app;

const DIFF_REFRESH_DEBOUNCE_MS = 350;
const DIFF_RENDER_DEBOUNCE_MS = 80;
const DIFF_MIN_WIDTH = 280;
// Per-line tokenizing is cheap but not free; skip highlighting huge diffs.
// The cap applies to the rows actually rendered, so a capped view of a huge
// diff still gets color.
const DIFF_HIGHLIGHT_MAX_ROWS = 1500;
// Cap initially rendered rows per file so a huge retained diff cannot flood
// the DOM; a "show more" control reveals further chunks on demand.
const DIFF_RENDER_MAX_ROWS = 400;
// Show the filter input once the list is long enough for scanning to hurt.
const DIFF_FILTER_MIN_FILES = 8;
// How long transient feedback (update pulse, copied checkmark) stays applied.
const DIFF_FEEDBACK_MS = 700;

// Per-session diff state lives here, NOT on session objects: sessions persist
// to localStorage and this data is server-backed and rebuildable.
const diffStateBySession = new Map();
let diffMaximized = false;
let diffMaximizeReturnFocus = null;

Object.assign(elements, {
  appMain: document.getElementById?.('appMain'),
  diffMaximizeBtn: document.getElementById?.('diffMaximizeBtn'),
  diffQueueBar: document.getElementById?.('diffQueueBar')
});

const sessionDiffState = (sessionId) => {
  let ds = diffStateBySession.get(sessionId);
  if (!ds) {
    ds = {
      scope: 'last_turn',
      gitKnown: false,
      git: false,
      files: new Map(),          // path -> { path, kind, adds, dels, truncated, lastSeq }
      expanded: new Set(),       // paths whose diff body is open
      userCollapsed: new Set(),  // paths the user explicitly collapsed (blocks auto-expand)
      userExpanded: new Set(),   // paths the user explicitly expanded (blocks auto-collapse)
      autoExpandedPath: '',      // the file currently held open by live-follow
      rowLimits: new Map(),      // path -> rendered row cap raised by "show more"
      diffCache: new Map(),      // path -> { seq, rev, data }
      cacheRev: 0,               // bumped on every cache write; keys body rebuilds
      dirtyPaths: new Set(),     // cached diff is stale (newer change seen)
      fetchErrors: new Set(),    // paths whose last diff fetch failed (shows retry)
      inflight: new Map(),       // path -> Promise (request dedup)
      blocks: new Map(),         // path -> reusable DOM block (see syncDiffFileBlock)
      filter: '',                // substring filter over paths (display only)
      refreshTimer: null,
      renderTimer: null,
      lastActivityAt: 0,
      pendingScrollPath: '',
      listLoaded: false,
      summaryKnown: false,
      summary: { fileCount: 0, adds: 0, dels: 0 },
      hidden: true               // panel starts closed; only an explicit user toggle reveals it
    };
    diffStateBySession.set(sessionId, ds);
  }
  return ds;
};

const normalizeSessionDiffSummary = app.normalizeDiffSummary;

const applySessionDiffSummary = (sessionId, value) => app.applyDiffSummary({
  owner: sessionId,
  value,
  sessionState: sessionDiffState,
  activeSessionId: () => state.activeSessionId,
  reconcile: reconcileDiffPathState,
  render: renderDiffSidebar
});

const authHeaders = () => (state.token ? { Authorization: `Bearer ${state.token}` } : {});
const isResolvedSessionIdentity = typeof app.isSessionIdentityResolved === 'function'
  ? app.isSessionIdentityResolved
  : (sessionId) => Boolean(String(sessionId || '').trim()) && !/^\d+$/.test(String(sessionId).trim());

const isDiffDrawerViewport = () => {
  try {
    return typeof window.matchMedia === 'function' && window.matchMedia('(max-width: 1099px)').matches;
  } catch {
    return false;
  }
};

const currentDiffState = () => (state.activeSessionId ? diffStateBySession.get(state.activeSessionId) || null : null);

// Opening or submitting a line comment is an explicit expand. Live-follow may
// keep following newer files, but it must not collapse this anchor's file.
const pinDiffFileExpanded = (sessionId, path) => {
  const ds = sessionDiffState(sessionId);
  if (!ds.files.has(path)) return false;
  ds.expanded.add(path);
  ds.userCollapsed.delete(path);
  ds.userExpanded.add(path);
  if (ds.dirtyPaths.has(path) || !ds.diffCache.has(path)) void fetchFileDiff(sessionId, path);
  return true;
};

const setDiffBackgroundInert = (inert) => {
  for (const element of [elements.sidebar, elements.appMain, elements.planPanel]) {
    if (!element) continue;
    element.inert = Boolean(inert);
    if (inert) element.setAttribute?.('inert', '');
    else element.removeAttribute?.('inert');
  }
};

const captureDiffSpatialAnchor = () => {
  const list = elements.diffFileList;
  const scrollTop = Number(list?.scrollTop) || 0;
  const panel = list?.querySelector?.('.diff-comment-panel');
  if (panel) {
    const path = panel.closest?.('.diff-file')?.querySelector?.('.diff-file-row')?.dataset?.path || '';
    return { element: panel, path, scrollTop };
  }
  const listTop = Number(list?.getBoundingClientRect?.().top) || 0;
  const rows = Array.from(list?.querySelectorAll?.('.diff-file-row') || []);
  const row = rows.find((entry) => Number(entry.getBoundingClientRect?.().bottom) >= listTop) || rows[0];
  return { path: row?.dataset?.path || '', scrollTop };
};

const setDiffMaximized = (on) => {
  const next = Boolean(on);
  if (next === diffMaximized) return false;
  const spatial = captureDiffSpatialAnchor();
  const active = document.activeElement;
  if (next && (elements.sidebar?.contains?.(active) || elements.appMain?.contains?.(active))) {
    diffMaximizeReturnFocus = active;
  }
  diffMaximized = next;
  elements.appShell?.classList.toggle('diff-maximized', next);
  elements.diffSidebar?.classList.toggle('maximized', next);
  setDiffBackgroundInert(next);
  const button = elements.diffMaximizeBtn;
  if (button) {
    button.setAttribute?.('aria-label', next ? 'Restore changes' : 'Maximize changes');
    button.setAttribute?.('title', next ? 'Restore' : 'Maximize');
    button.dataset.action = next ? 'restore' : 'maximize';
  }
  if (elements.diffFileList) elements.diffFileList.scrollTop = spatial.scrollTop;
  if (spatial.element && spatial.element.isConnected !== false) spatial.element.scrollIntoView?.({ block: 'nearest' });
  else if (spatial.path) scrollFileIntoView(spatial.path);
  if (next && diffMaximizeReturnFocus) button?.focus?.();
  if (!next) {
    const ds = currentDiffState();
    if (ds) {
      if (isDiffDrawerViewport() && !ds.hidden) elements.diffSidebar?.classList.add('open');
      applyDiffSidebarVisibility(ds);
    }
    const returnFocus = diffMaximizeReturnFocus;
    diffMaximizeReturnFocus = null;
    if (returnFocus?.isConnected !== false) returnFocus?.focus?.();
  }
  return true;
};

// ===== Pure model building (node-tested) =====

// buildDiffRowModel flattens server hunks into renderable rows with old/new
// line numbers. Hunk separators appear between hunks, never before the first.
const buildDiffRowModel = (hunks) => {
  const rows = [];
  (Array.isArray(hunks) ? hunks : []).forEach((hunk, index) => {
    if (index > 0) rows.push({ type: 'hunk', oldNo: 0, newNo: 0, text: '' });
    let oldNo = Number(hunk.old_start) || 1;
    let newNo = Number(hunk.new_start) || 1;
    (Array.isArray(hunk.lines) ? hunk.lines : []).forEach((line) => {
      const text = String(line.s ?? '');
      if (line.t === 'add') {
        rows.push({ type: 'add', oldNo: 0, newNo, text });
        newNo += 1;
      } else if (line.t === 'del') {
        rows.push({ type: 'del', oldNo, newNo: 0, text });
        oldNo += 1;
      } else {
        rows.push({ type: 'ctx', oldNo, newNo, text });
        oldNo += 1;
        newNo += 1;
      }
    });
  });
  return rows;
};

const countRowChanges = (rows) => {
  let adds = 0;
  let dels = 0;
  rows.forEach((row) => {
    if (row.type === 'add') adds += 1;
    else if (row.type === 'del') dels += 1;
  });
  return { adds, dels };
};

// sortDiffPaths orders file entries most-recently-changed first (server seq
// is monotonic), falling back to path order for ties. A live panel should
// keep the file being edited at the top, not buried alphabetically.
const sortDiffPaths = (entries) => entries
  .slice()
  .sort((a, b) => ((b.lastSeq || 0) - (a.lastSeq || 0)) || (a.path < b.path ? -1 : a.path > b.path ? 1 : 0))
  .map((entry) => entry.path);

// emphasizeRowPair computes the changed span between a paired del/add line
// (common prefix/suffix trimmed) and stores it as row.emph = [start, end).
// Pairs with nothing in common are left alone — whole-line emphasis is noise.
const emphasizeRowPair = (del, add) => {
  const a = del.text;
  const b = add.text;
  if (a === b) return;
  // Compare code points, not UTF-16 units, so the mark never splits a
  // surrogate pair (two different emoji share a high surrogate).
  const aCP = Array.from(a);
  const bCP = Array.from(b);
  const maxP = Math.min(aCP.length, bCP.length);
  let p = 0;
  while (p < maxP && aCP[p] === bCP[p]) p += 1;
  let s = 0;
  while (s < maxP - p && aCP[aCP.length - 1 - s] === bCP[bCP.length - 1 - s]) s += 1;
  if (p + s === 0) return;
  // Skip when the lines barely relate: emphasis should mark a small edit,
  // not repaint an entire replaced line.
  if (p + s < Math.max(aCP.length, bCP.length) * 0.2) return;
  const unitLen = (cps, count) => (count > 0 ? cps.slice(0, count).join('').length : 0);
  const prefixA = unitLen(aCP, p);
  const prefixB = unitLen(bCP, p);
  const suffixA = unitLen(aCP.slice(aCP.length - s), s);
  const suffixB = unitLen(bCP.slice(bCP.length - s), s);
  del.emph = [prefixA, a.length - suffixA];
  add.emph = [prefixB, b.length - suffixB];
};

// computeInlineEmphasis pairs consecutive del/add runs index-wise (GitHub
// style) and marks the changed span within each pair.
const computeInlineEmphasis = (rows) => {
  let i = 0;
  while (i < rows.length) {
    if (rows[i].type !== 'del') {
      i += 1;
      continue;
    }
    const delStart = i;
    while (i < rows.length && rows[i].type === 'del') i += 1;
    const addStart = i;
    while (i < rows.length && rows[i].type === 'add') i += 1;
    const pairs = Math.min(addStart - delStart, i - addStart);
    for (let j = 0; j < pairs; j += 1) {
      emphasizeRowPair(rows[delStart + j], rows[addStart + j]);
    }
  }
  return rows;
};

// buildUnifiedDiff reconstructs a unified diff patch from the cached hunk
// payload, for the per-file "copy diff" action.
const buildUnifiedDiff = (path, data) => {
  const out = [`--- a/${path}`, `+++ b/${path}`];
  (Array.isArray(data?.hunks) ? data.hunks : []).forEach((hunk) => {
    const lines = Array.isArray(hunk.lines) ? hunk.lines : [];
    let oldLen = 0;
    let newLen = 0;
    lines.forEach((line) => {
      if (line.t === 'add') newLen += 1;
      else if (line.t === 'del') oldLen += 1;
      else {
        oldLen += 1;
        newLen += 1;
      }
    });
    out.push(`@@ -${Number(hunk.old_start) || 1},${oldLen} +${Number(hunk.new_start) || 1},${newLen} @@`);
    lines.forEach((line) => {
      const prefix = line.t === 'add' ? '+' : line.t === 'del' ? '-' : ' ';
      out.push(prefix + String(line.s ?? ''));
    });
  });
  return `${out.join('\n')}\n`;
};

// ===== State updates =====

const handleFileChangeEvent = (session, payload) => {
  if (!session?.id || !payload?.path) return;
  const ds = sessionDiffState(session.id);
  if (ds.scope !== 'last_turn') return;
  const path = String(payload.path);
  const seq = Number(payload.seq) || 0;

  const existing = ds.files.get(path);
  // Replayed events (stream reconnect) arrive with stale sequence numbers.
  if (existing && seq && existing.lastSeq && seq <= existing.lastSeq) return;

  const incoming = {
    path,
    kind: normalizeDiffKind(payload.kind),
    adds: Number(payload.adds) || 0,
    dels: Number(payload.dels) || 0,
    truncated: Boolean(payload.truncated),
    lastSeq: seq
  };
  // Tool events describe only the latest before→after operation, while this
  // panel shows the cumulative session-baseline diff. Once canonical metadata
  // exists, retain it until the scheduled server refresh replaces it. Otherwise
  // a second write to a newly-created file briefly relabels the whole file as a
  // modification and flashes every added row green.
  ds.files.set(path, existing ? { ...existing, lastSeq: seq } : incoming);
  ds.dirtyPaths.add(path);

  // Live follow: keep only the file being edited open. The previously
  // followed file collapses again unless the user opened it themselves —
  // a busy run should read as "one file at a time", not an ever-growing
  // wall of expanded diffs.
  const newlyExpanded = !ds.expanded.has(path) && !ds.userCollapsed.has(path);
  if (!ds.userCollapsed.has(path)) {
    const prev = ds.autoExpandedPath;
    if (prev && prev !== path && !ds.userExpanded.has(prev)) ds.expanded.delete(prev);
    ds.expanded.add(path);
    ds.autoExpandedPath = path;
  }

  if (session.id !== state.activeSessionId) return;
  if (newlyExpanded) ds.pendingScrollPath = path;
  scheduleDiffRender(session.id);
  scheduleDiffRefresh(session.id);
};

// scheduleDiffRender coalesces accordion re-renders during bursts of change
// events or diff-fetch completions. An isolated change renders immediately
// (the sidebar should react instantly); rapid-fire activity within the window
// shares one trailing render. A shell call touching hundreds of files would
// otherwise rebuild the DOM per event.
const scheduleDiffRender = (sessionId) => {
  const ds = sessionDiffState(sessionId);
  const now = Date.now();
  const sinceLastActivity = now - ds.lastActivityAt;
  ds.lastActivityAt = now;
  if (ds.renderTimer) return; // trailing render already queued
  if (sinceLastActivity >= DIFF_RENDER_DEBOUNCE_MS) {
    renderDiffSidebar(sessionId);
    return;
  }
  ds.renderTimer = setTimeout(() => {
    ds.renderTimer = null;
    renderDiffSidebar(sessionId);
  }, DIFF_RENDER_DEBOUNCE_MS);
};

const scheduleDiffRefresh = (sessionId) => {
  const ds = sessionDiffState(sessionId);
  if (ds.refreshTimer) clearTimeout(ds.refreshTimer);
  ds.refreshTimer = setTimeout(() => {
    ds.refreshTimer = null;
    if (sessionId !== state.activeSessionId) return;
    if (ds.hidden) return; // fetch lazily on reveal
    ds.expanded.forEach((path) => {
      if (ds.dirtyPaths.has(path)) void fetchFileDiff(sessionId, path);
    });
  }, DIFF_REFRESH_DEBOUNCE_MS);
};

// ===== Fetching =====

// reconcileDiffPathState prunes path-keyed state after the authoritative
// server list replaced ds.files: live rows can carry non-canonical paths
// (e.g. relative) that the replace renames, and without pruning their
// expansion/cache/limit state both leaks and detaches live-follow.
const reconcileDiffPathState = (sessionId, ds) => {
  const prune = (collection) => {
    collection.forEach((_, key) => {
      // Sets iterate as (value, value); Maps as (value, key) — key works for both.
      if (!ds.files.has(key)) collection.delete(key);
    });
  };
  const wasFollowing = Boolean(ds.autoExpandedPath);
  prune(ds.expanded);
  prune(ds.userCollapsed);
  prune(ds.userExpanded);
  prune(ds.rowLimits);
  prune(ds.diffCache);
  prune(ds.dirtyPaths);
  prune(ds.fetchErrors);
  prune(ds.blocks);
  if (ds.pendingScrollPath && !ds.files.has(ds.pendingScrollPath)) ds.pendingScrollPath = '';
  if (ds.autoExpandedPath && !ds.files.has(ds.autoExpandedPath)) {
    // The followed path was canonicalized away; keep following the change
    // stream by moving to the most recent entry the user hasn't collapsed.
    ds.autoExpandedPath = '';
    if (wasFollowing) {
      let candidate = null;
      ds.files.forEach((entry) => {
        if (ds.userCollapsed.has(entry.path)) return;
        if (!candidate || (entry.lastSeq || 0) > (candidate.lastSeq || 0)) candidate = entry;
      });
      if (candidate) {
        ds.autoExpandedPath = candidate.path;
        ds.expanded.add(candidate.path);
      }
    }
  }
  app.reconcileDiffCommentPanel?.(sessionId, new Set(ds.files.keys()));
};

const fetchSessionFileChanges = async (sessionId) => {
  if (!isResolvedSessionIdentity(sessionId)) return false;
  const ds = sessionDiffState(sessionId);
  const requestedScope = ds.scope;
  // Snapshot per-path seqs so live rows whose change events land while this
  // fetch is in flight survive the authoritative replace below.
  const seqAtStart = new Map();
  ds.files.forEach((entry, path) => seqAtStart.set(path, entry.lastSeq || 0));
  try {
    const scopeQuery = requestedScope === 'last_turn' ? '' : `?scope=${encodeURIComponent(requestedScope)}`;
    const resp = await app.apiFetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(sessionId)}/file-changes${scopeQuery}`, {
      headers: authHeaders()
    });
    if (!resp.ok) return; // 404 = tracking disabled; treat as no changes
    const body = await resp.json();
    if (ds.scope !== requestedScope) return false;
    ds.gitKnown = true;
    ds.git = Boolean(body?.git);
    const entries = Array.isArray(body?.file_changes) ? body.file_changes : [];
    // The server list is authoritative: live rows may carry non-canonical
    // paths (e.g. relative) that would otherwise duplicate the canonical
    // entry, and rows the server has since collapsed (net no-ops) drop out.
    // The one exception: rows whose change event arrived AFTER this fetch
    // started — the response predates them, so they must survive.
    const next = new Map();
    entries.forEach((entry) => {
      const path = String(entry.path || '');
      if (!path) return;
      const prev = ds.files.get(path);
      next.set(path, {
        path,
        kind: normalizeDiffKind(entry.kind),
        adds: Number(entry.adds) || 0,
        dels: Number(entry.dels) || 0,
        truncated: Boolean(entry.truncated),
        lastSeq: Number(entry.seq) || prev?.lastSeq || 0,
        snapshotSeq: Number(entry.snapshot_seq) || 0
      });
    });
    ds.files.forEach((entry, path) => {
      if (next.has(path)) return;
      if ((entry.lastSeq || 0) > (seqAtStart.get(path) || 0)) next.set(path, entry);
    });
    ds.files = next;
    ds.listLoaded = true;
    ds.summaryKnown = true;
    ds.summary = { fileCount: next.size, adds: 0, dels: 0, git: ds.git };
    next.forEach((entry) => {
      ds.summary.adds += entry.adds;
      ds.summary.dels += entry.dels;
    });
    reconcileDiffPathState(sessionId, ds);
    // Cached diffs predating the authoritative path or window snapshot are
    // stale even when the tab missed every live event while detached.
    ds.files.forEach((entry, path) => {
      const cached = ds.diffCache.get(path);
      if (cached && ((entry.lastSeq || 0) > (cached.seq || 0) || (entry.snapshotSeq || 0) !== (cached.snapshotSeq || 0))) ds.dirtyPaths.add(path);
    });
    if (sessionId === state.activeSessionId) renderDiffSidebar(sessionId);
    return true;
  } catch {
    // Network failures leave existing state untouched.
    return false;
  }
};

// markDiffFetchError records a failed diff fetch so the body can offer a
// retry instead of sitting on "Loading diff…" forever.
const markDiffFetchError = (sessionId, path) => {
  const ds = sessionDiffState(sessionId);
  ds.fetchErrors.add(path);
  if (sessionId === state.activeSessionId) scheduleDiffRender(sessionId);
};

const fetchFileDiff = (sessionId, path) => {
  const ds = sessionDiffState(sessionId);
  const existingRequest = ds.inflight.get(path);
  if (existingRequest) return existingRequest;

  const requestedScope = ds.scope;
  const { lastSeq: seqAtRequest = 0, snapshotSeq = 0 } = ds.files.get(path) || {};
  const request = (async () => {
    try {
      const diffQuery = `${requestedScope === 'last_turn' ? '' : `&scope=${encodeURIComponent(requestedScope)}`}${Number(snapshotSeq) > 0 ? `&snapshot_seq=${snapshotSeq}` : ''}`;
      const url = `${UI_PREFIX}/v1/sessions/${encodeURIComponent(sessionId)}/file-changes/diff?path=${encodeURIComponent(path)}${diffQuery}`;
      const resp = await app.apiFetch(url, { headers: authHeaders() });
      if (!resp.ok) {
        if (ds.scope === requestedScope) markDiffFetchError(sessionId, path);
        return null;
      }
      const data = await resp.json();
      if (ds.scope !== requestedScope) return null;
      // rev, not seq, keys body rebuilds: a refetch can return newer server
      // content under an unchanged local seq (events missed while detached).
      ds.cacheRev += 1;
      ds.diffCache.set(path, { seq: seqAtRequest, snapshotSeq, rev: ds.cacheRev, data });
      ds.fetchErrors.delete(path);

      // A newer change may have landed mid-fetch; leave it dirty and schedule
      // another refresh (the debounce timer that requested this fetch already
      // fired, so without rescheduling the stale diff would stick around).
      const latest = ds.files.get(path);
      if ((latest?.lastSeq || 0) <= seqAtRequest && (latest?.snapshotSeq || 0) === snapshotSeq) ds.dirtyPaths.delete(path);
      else scheduleDiffRefresh(sessionId);

      // True-up the list entry with cumulative counts from the actual diff.
      const entry = ds.files.get(path);
      if (entry && !data.truncated && !data.image) {
        const { adds, dels } = countRowChanges(buildDiffRowModel(data.hunks));
        entry.adds = adds;
        entry.dels = dels;
        entry.truncated = Boolean(data.truncated);
      }

      // Coalesced: many fetches resolving in a burst share renders.
      if (sessionId === state.activeSessionId) scheduleDiffRender(sessionId);
      return data;
    } catch {
      if (ds.scope === requestedScope) markDiffFetchError(sessionId, path);
      return null;
    } finally {
      if (ds.inflight.get(path) === request) ds.inflight.delete(path);
    }
  })();
  ds.inflight.set(path, request);
  return request;
};

// ===== Rendering =====

const fileBaseName = (path) => {
  const idx = path.lastIndexOf('/');
  return idx >= 0 ? path.slice(idx + 1) : path;
};

const fileDirName = (path) => {
  const idx = path.lastIndexOf('/');
  return idx > 0 ? path.slice(0, idx) : '';
};

const kindBadgeLabel = { create: 'A', modify: 'M', delete: 'D' };
const normalizeDiffKind = (kind) => kindBadgeLabel[kind] ? kind : 'modify';
const diffTotals = app.diffTotals;

const applyDiffSidebarVisibility = (ds) => {
  const hasChanges = diffTotals(ds).fileCount > 0;
  const available = hasChanges || Boolean(ds.gitKnown && ds.git);
  const drawer = isDiffDrawerViewport();
  const visible = available && !ds.hidden && !drawer;
  const drawerOpen = Boolean(available && drawer && elements.diffSidebar?.classList.contains('open'));

  if (elements.diffSidebar) {
    if (!available) {
      elements.diffSidebar.classList.remove('open');
      elements.appShell?.classList.remove('diff-open');
      setPanelHidden(elements.diffSidebar, true);
    } else if (drawer) {
      elements.appShell?.classList.remove('diff-open');
      if (drawerOpen) setPanelHidden(elements.diffSidebar, false);
      else setPanelHidden(elements.diffSidebar, true);
    } else {
      elements.diffSidebar.classList.remove('open');
      setPanelOpen({
        panel: elements.diffSidebar,
        open: visible,
        hiddenWhenClosed: true,
        classTargets: [{ element: elements.appShell, className: 'diff-open' }],
        transitionElement: elements.appShell
      });
    }
  } else {
    elements.appShell?.classList.toggle('diff-open', visible);
  }

  if (elements.diffToggleBtn) {
    elements.diffToggleBtn.hidden = !available;
    if (available) elements.diffToggleBtn.removeAttribute?.('hidden');
    else elements.diffToggleBtn.setAttribute?.('hidden', '');
    elements.diffToggleBtn.classList.toggle('active', visible || drawerOpen);
  }
};

// resolveHljsLanguage maps the server's file-extension lang hint to a loaded
// hljs language name, or '' when highlighting is unavailable.
const HLJS_LANG_ALIASES = {
  js: 'javascript',
  jsx: 'javascript',
  mjs: 'javascript',
  cjs: 'javascript',
  ts: 'typescript',
  tsx: 'typescript',
  py: 'python',
  rb: 'ruby',
  rs: 'rust',
  kt: 'kotlin',
  sh: 'bash',
  zsh: 'bash',
  yml: 'yaml',
  md: 'markdown',
  cc: 'cpp',
  cxx: 'cpp',
  hpp: 'cpp',
  h: 'c',
  cs: 'csharp',
  ex: 'elixir',
  exs: 'elixir'
};

const resolveHljsLanguage = (lang) => {
  const highlighter = window.hljs;
  if (!highlighter || !lang) return '';
  const name = HLJS_LANG_ALIASES[lang] || lang;
  return highlighter.getLanguage?.(name) ? name : '';
};

// renderDiffCode renders one code cell. Rows with a word-level emphasis span
// render as plain text with the changed range marked — the mark carries more
// signal than syntax colors for an edited line, and mixing both cheaply is
// not possible with per-line re-highlighting. Other rows are syntax-
// highlighted per line when hljs and the language are available. Per-line
// tokenizing is stateless, so multi-line constructs (block comments, template
// literals) may mis-color continuation lines — an accepted trade-off for
// live re-rendering.
const renderDiffCode = (type, text, lang, emph) => {
  const el = createEl('span', 'diff-code');
  if (Array.isArray(emph) && emph[1] > emph[0]) {
    if (emph[0] > 0) el.appendChild(createEl('span', '', text.slice(0, emph[0])));
    el.appendChild(createEl('span', 'diff-word', text.slice(emph[0], emph[1])));
    if (emph[1] < text.length) el.appendChild(createEl('span', '', text.slice(emph[1])));
    return el;
  }
  const language = resolveHljsLanguage(lang);
  if (language && text) {
    try {
      el.innerHTML = window.hljs.highlight(text, { language, ignoreIllegals: true }).value;
      return el;
    } catch {
      // Fall through to plain text.
    }
  }
  el.textContent = text;
  return el;
};

// Lazy-load hljs (plus its theme stylesheets) the first time a highlightable
// diff renders, then re-render once it arrives.
let hljsLoadRequested = false;
const requestDiffHighlight = () => {
  if (typeof window.hljs !== 'undefined' || hljsLoadRequested) return;
  hljsLoadRequested = true;
  const loading = app.ensureHighlightLoaded?.();
  loading?.then?.((loaded) => {
    if (loaded && state.activeSessionId) renderDiffSidebar(state.activeSessionId);
  });
};

const DIFF_FILE_ICON_SVG = '<svg class="diff-toggle-file-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4.5 1.75h4.25L12.5 5.5v8.75h-8z"/><path d="M8.75 1.75V5.5h3.75"/><path d="M6.25 8.5h4"/><path d="M6.25 11h3"/></svg>';

const renderDiffTotals = (ds) => {
  const { fileCount, adds, dels } = diffTotals(ds);
  const summary = [];
  const sidebarParts = [];
  if (adds > 0) {
    summary.push(`+${adds}`);
    sidebarParts.push(createEl('span', 'diff-sidebar-totals-add', `+${adds}`));
  }
  if (dels > 0) {
    summary.push(`−${dels}`);
    sidebarParts.push(createEl('span', 'diff-sidebar-totals-del', `−${dels}`));
  }
  elements.diffSidebarTotals?.replaceChildren(...sidebarParts);

  if (elements.diffToggleBadge) {
    const badge = elements.diffToggleBadge;
    if (fileCount > 0 || ds?.git) {
      const fileCountEl = createEl('span', 'diff-toggle-file-count');
      fileCountEl.innerHTML = DIFF_FILE_ICON_SVG;
      fileCountEl.dataset.fileCount = String(fileCount);
      const parts = [fileCountEl];
      if (adds > 0) parts.push(createEl('span', 'diff-toggle-stat-add', `+${adds}`));
      if (dels > 0) parts.push(createEl('span', 'diff-toggle-stat-del', `−${dels}`));
      badge.classList.toggle('no-stats', summary.length === 0);
      badge.replaceChildren(...parts);
    } else {
      badge.classList.remove('no-stats');
      badge.replaceChildren();
    }
  }
  if (elements.diffToggleBtn) {
    const fileLabel = `${fileCount} changed ${fileCount === 1 ? 'file' : 'files'}`;
    elements.diffToggleBtn.title = summary.length > 0 ? `${fileLabel} (${summary.join(' ')})` : fileLabel;
    elements.diffToggleBtn.setAttribute?.('aria-label', `Toggle file changes: ${elements.diffToggleBtn.title}`);
  }
};

// applyTransientClass flashes a feedback class, clearing any pending removal
// first so rapid re-triggers extend the feedback instead of cutting it short.
const applyTransientClass = (el, className) => {
  if (!el?.classList) return;
  const timers = el._diffFeedbackTimers || (el._diffFeedbackTimers = {});
  if (timers[className]) clearTimeout(timers[className]);
  el.classList.add(className);
  timers[className] = setTimeout(() => {
    el.classList.remove(className);
    delete timers[className];
  }, DIFF_FEEDBACK_MS);
};

// copyDiffText writes text to the clipboard and flashes the button as
// feedback. Uses the app-wide writer, which falls back to execCommand in
// contexts without the async clipboard API (e.g. plain http).
const copyDiffText = (button, text) => {
  const writer = app.getClipboardWriter?.();
  if (!writer) return;
  Promise.resolve(writer.writeText(text)).then(() => {
    applyTransientClass(button, 'copied');
  }).catch(() => {});
};

const imageDiffContentURL = (sessionId, path, side, scope = 'last_turn', snapshotSeq = 0) => {
  const scopeQuery = `${scope === 'last_turn' ? '' : `&scope=${encodeURIComponent(scope)}`}${Number(snapshotSeq) > 0 ? `&snapshot_seq=${snapshotSeq}` : ''}`;
  return `${UI_PREFIX}/v1/sessions/${encodeURIComponent(sessionId)}/file-changes/content?path=${encodeURIComponent(path)}&side=${side}${scopeQuery}`;
};

const renderImageDiff = (sessionId, path, data, scope = 'last_turn', snapshotSeq = 0) => {
  const comparison = createEl('div', `diff-image-comparison diff-image-${normalizeDiffKind(data.kind)}`);
  const sides = data.kind === 'create' ? ['after'] : data.kind === 'delete' ? ['before'] : ['before', 'after'];
  sides.forEach((side) => {
    const panel = createEl('div', 'diff-image-side');
    panel.appendChild(createEl('div', 'diff-image-label', side === 'before' ? 'Before' : 'After'));
    const src = imageDiffContentURL(sessionId, path, side, scope, snapshotSeq);
    const image = createEl('img', 'diff-image-preview');
    image.src = src;
    image.alt = `${side === 'before' ? 'Before' : 'After'} ${fileBaseName(path)}`;
    image.loading = 'lazy';
    image.addEventListener('click', () => app.openLightbox?.(src));
    image.addEventListener('error', () => {
      image.hidden = true;
      if (!panel.querySelector?.('.diff-image-error')) {
        panel.appendChild(createEl('div', 'diff-note diff-image-error', 'Preview unavailable'));
      }
    });
    panel.appendChild(image);
    comparison.appendChild(panel);
  });
  return comparison;
};

const captureDiffCommentFocus = (body) => {
  const focused = document.activeElement;
  if (!focused || !body?.contains?.(focused)) return null;
  const panel = focused.closest?.('.diff-comment-panel');
  const key = panel?._diffCommentKey || focused._diffCommentKey;
  if (!key) return null;
  return panel ? { key, kind: 'editor' } : { key, kind: 'marker', target: focused.classList?.contains?.('diff-comment-affordance') ? 'button' : 'row' };
};

const renderDiffFileBody = (sessionId, ds, path, commentFocus = null) => {
  const body = createEl('div', 'diff-file-body');
  const cached = ds.diffCache.get(path);
  if (!cached) {
    if (ds.fetchErrors.has(path)) {
      const note = createEl('div', 'diff-note diff-error');
      note.appendChild(createEl('span', '', 'Couldn’t load this diff.'));
      const retry = createEl('button', 'diff-retry', 'Retry');
      retry.setAttribute('type', 'button');
      retry.addEventListener('click', (event) => {
        event.stopPropagation?.();
        ds.fetchErrors.delete(path);
        void fetchFileDiff(sessionId, path);
        renderDiffSidebar(sessionId);
      });
      note.appendChild(retry);
      body.appendChild(note);
      return body;
    }
    body.appendChild(createEl('div', 'diff-note', 'Loading diff…'));
    void fetchFileDiff(sessionId, path);
    return body;
  }
  if (cached.data.truncated) {
    body.appendChild(createEl('div', 'diff-note', 'Diff content was not retained for this file (too large, unsupported binary, or unrecoverable).'));
    return body;
  }
  if (cached.data.image) {
    if (!app.isTurnDiffScope?.(ds.scope)) { body.appendChild(createEl('div', 'diff-note', 'Image previews are available only for turn-based scopes.')); return body; }
    body.appendChild(renderImageDiff(sessionId, path, cached.data, ds.scope, ds.files.get(path)?.snapshotSeq));
    return body;
  }

  const rows = computeInlineEmphasis(buildDiffRowModel(cached.data.hunks));

  const limit = ds.rowLimits.get(path) || DIFF_RENDER_MAX_ROWS;
  let visibleRows = rows;
  let hiddenCount = 0;
  if (rows.length > limit) {
    visibleRows = rows.slice(0, limit);
    hiddenCount = rows.length - visibleRows.length;
  }

  const lang = visibleRows.length <= DIFF_HIGHLIGHT_MAX_ROWS ? (cached.data.lang || '') : '';
  if (lang) requestDiffHighlight();

  const table = createEl('div', `diff-rows diff-rows-kind-${normalizeDiffKind(ds.files.get(path)?.kind)}`);
  const postAttachCommentFocus = [];
  const firstCommentableRowIndex = visibleRows.findIndex((row) => row.type !== 'hunk');
  visibleRows.forEach((row, rowIndex) => {
    const rowEl = createEl('div', `diff-row ${row.type}`);
    if (row.type === 'hunk') {
      rowEl.appendChild(createEl('span', 'diff-hunk-sep', '⋯'));
    } else {
      rowEl.appendChild(createEl('span', 'diff-ln old', row.oldNo ? String(row.oldNo) : ''));
      rowEl.appendChild(createEl('span', 'diff-ln new', row.newNo ? String(row.newNo) : ''));
      rowEl.appendChild(renderDiffCode(row.type, row.text, lang, row.emph));
      const restoreCommentPanel = app.decorateDiffCommentRow?.({
        sessionId,
        path,
        row,
        rows,
        rowIndex,
        rowElement: rowEl,
        scope: ds.scope,
        fileChangeSeq: Number(ds.files.get(path)?.snapshotSeq) || Number(ds.files.get(path)?.lastSeq) || Number(cached.seq) || 0,
        initialTabStop: rowIndex === firstCommentableRowIndex
      });
      table.appendChild(rowEl);
      const restoreFocus = restoreCommentPanel?.({ deferFocus: true, commentFocus });
      if (restoreFocus) postAttachCommentFocus.push(restoreFocus);
      return;
    }
    table.appendChild(rowEl);
  });
  if (!table.querySelector?.('.diff-comment-panel')) app.clearDiffCommentPanel?.(sessionId, path);
  body.appendChild(table);
  if (postAttachCommentFocus.length > 0) body._restoreDiffCommentFocus = () => postAttachCommentFocus.forEach((restoreFocus) => restoreFocus());

  if (hiddenCount > 0) {
    // Reveal in chunks: rendering thousands of rows in one synchronous pass
    // janks the panel. A second control jumps straight to everything.
    const chunk = Math.min(DIFF_RENDER_MAX_ROWS, hiddenCount);
    const more = createEl('button', 'diff-show-more', `Show ${chunk} more lines`);
    more.setAttribute('type', 'button');
    more.addEventListener('click', () => {
      ds.rowLimits.set(path, limit + DIFF_RENDER_MAX_ROWS);
      renderDiffSidebar(sessionId);
    });
    body.appendChild(more);
    if (hiddenCount > DIFF_RENDER_MAX_ROWS) {
      const all = createEl('button', 'diff-show-more diff-show-all', `Show all ${hiddenCount} hidden lines`);
      all.setAttribute('type', 'button');
      all.addEventListener('click', () => {
        ds.rowLimits.set(path, Infinity);
        renderDiffSidebar(sessionId);
      });
      body.appendChild(all);
    }
  }
  return body;
};

// syncDiffFileBlock creates or patches the reusable DOM block for one file.
// The header is built once and updated in place; the body is rebuilt only
// when the underlying diff data, row limit, error state, or highlighter
// availability changed (tracked via bodyKey).
const syncDiffFileBlock = (sessionId, ds, path) => {
  const entry = ds.files.get(path);
  const expanded = ds.expanded.has(path);
  let block = ds.blocks.get(path);

  if (!block) {
    const el = createEl('div', 'diff-file');
    const header = createEl('div', 'diff-file-row');
    header.setAttribute('role', 'button');
    header.setAttribute('tabindex', '0');
    header.title = path;
    if (header.dataset) header.dataset.path = path;
    header.addEventListener('click', () => toggleDiffFile(sessionId, path));
    header.addEventListener('keydown', (event) => {
      // Keys pressed on the nested action buttons must activate those
      // buttons, not toggle the accordion.
      if (event.target && event.target !== header) return;
      if (event.key === 'Enter' || event.key === ' ') {
        event.preventDefault?.();
        toggleDiffFile(sessionId, path);
      }
    });

    const chevron = createEl('span', 'diff-chevron', '▸');
    const kindBadge = createEl('span', 'diff-kind-badge');
    const nameWrap = createEl('span', 'diff-file-name');
    const base = createEl('span', 'diff-file-base', fileBaseName(path));
    nameWrap.appendChild(base);
    const dirName = fileDirName(path);
    if (dirName) nameWrap.appendChild(createEl('span', 'diff-file-dir', dirName));
    const counts = createEl('span', 'diff-file-counts');

    const actions = createEl('span', 'diff-file-actions');
    const copyPath = createEl('button', 'diff-action-btn', '⧉');
    copyPath.setAttribute('type', 'button');
    copyPath.title = 'Copy path';
    copyPath.setAttribute('aria-label', `Copy path ${path}`);
    copyPath.addEventListener('click', (event) => {
      event.stopPropagation?.();
      copyDiffText(copyPath, path);
    });
    const copyPatch = createEl('button', 'diff-action-btn', '±');
    copyPatch.setAttribute('type', 'button');
    copyPatch.title = 'Copy diff';
    copyPatch.setAttribute('aria-label', `Copy diff for ${path}`);
    copyPatch.addEventListener('click', (event) => {
      event.stopPropagation?.();
      const cached = ds.diffCache.get(path);
      if (cached && !cached.data.truncated && !cached.data.image) {
        copyDiffText(copyPatch, buildUnifiedDiff(path, cached.data));
        return;
      }
      fetchFileDiff(sessionId, path).then((data) => {
        if (data && !data.truncated && !data.image) copyDiffText(copyPatch, buildUnifiedDiff(path, data));
      });
    });
    actions.appendChild(copyPath);
    actions.appendChild(copyPatch);

    header.appendChild(chevron);
    header.appendChild(kindBadge);
    header.appendChild(nameWrap);
    header.appendChild(counts);
    header.appendChild(actions);
    el.appendChild(header);

    block = { el, header, kindBadge, counts, copyPatch, body: null, bodyKey: '', renderedKind: '', renderedAdds: null, renderedDels: null, renderedTruncated: null };
    ds.blocks.set(path, block);
  }

  block.header.className = `diff-file-row${expanded ? ' expanded' : ''}`;
  block.header.setAttribute('aria-expanded', expanded ? 'true' : 'false');

  if (block.renderedKind !== entry.kind) {
    block.kindBadge.className = `diff-kind-badge diff-kind-${entry.kind}`;
    block.kindBadge.textContent = kindBadgeLabel[entry.kind] || 'M';
    block.renderedKind = entry.kind;
  }

  const countsChanged = block.renderedAdds !== entry.adds
    || block.renderedDels !== entry.dels
    || block.renderedTruncated !== entry.truncated;
  if (countsChanged) {
    const parts = [];
    if (entry.truncated) {
      parts.push(createEl('span', 'diff-count-muted', '–'));
    } else {
      if (entry.adds > 0) parts.push(createEl('span', 'diff-count-add', `+${entry.adds}`));
      if (entry.dels > 0) parts.push(createEl('span', 'diff-count-del', `−${entry.dels}`));
    }
    block.counts.replaceChildren(...parts);
    // Pulse rows that changed after their first paint so live updates are
    // visible without re-reading the numbers.
    if (block.renderedAdds !== null) {
      applyTransientClass(block.header, 'updated');
    }
    block.renderedAdds = entry.adds;
    block.renderedDels = entry.dels;
    block.renderedTruncated = entry.truncated;
  }

  const cached = ds.diffCache.get(path);
  block.copyPatch.hidden = Boolean(cached?.data?.image);
  if (block.copyPatch.hidden) block.copyPatch.setAttribute?.('hidden', '');
  else block.copyPatch.removeAttribute?.('hidden');

  if (!expanded) {
    if (block.body) {
      block.body = null;
      block.bodyKey = '';
      block.el.replaceChildren(block.header);
    }
    return block;
  }

  const bodyKey = [
    cached ? cached.rev : 'none',
    ds.rowLimits.get(path) || 0,
    ds.fetchErrors.has(path) ? 1 : 0,
    typeof window.hljs !== 'undefined' ? 1 : 0,
    app.diffCommentRevision?.(sessionId, path) || 0
  ].join('|');
  if (!block.body || block.bodyKey !== bodyKey) {
    const commentFocus = captureDiffCommentFocus(block.body);
    block.body = renderDiffFileBody(sessionId, ds, path, commentFocus);
    block.bodyKey = bodyKey;
    block.el.replaceChildren(block.header, block.body);
    block.body._restoreDiffCommentFocus?.();
  }
  return block;
};

const diffFilterMatches = (ds, path) => {
  const filter = String(ds.filter || '').toLowerCase();
  return !filter || path.toLowerCase().includes(filter);
};

const updateDiffFilterVisibility = (ds) => {
  const row = elements.diffFilterRow;
  if (!row) return;
  const show = ds.files.size >= DIFF_FILTER_MIN_FILES || Boolean(ds.filter);
  row.hidden = !show;
  if (show) row.removeAttribute?.('hidden');
  else row.setAttribute?.('hidden', '');
};

const renderDiffAccordion = (sessionId, ds) => {
  const list = elements.diffFileList;
  if (!list) return;
  const keepScroll = list.scrollTop;

  ds.blocks.forEach((_, path) => {
    if (!ds.files.has(path)) ds.blocks.delete(path);
  });

  const paths = sortDiffPaths(Array.from(ds.files.values())).filter((path) => diffFilterMatches(ds, path));
  app.reconcileDiffCommentPanel?.(sessionId, new Set(paths));
  const desired = paths.map((path) => syncDiffFileBlock(sessionId, ds, path).el);

  const desiredChildren = desired.length === 0
    ? [createEl('div', 'diff-note', ds.filter ? 'No files match the filter.' : 'No changes in this scope.')]
    : desired;
  // Avoid list churn unless membership/order changed, preserving exact surviving focus across necessary replacements.
  const current = list.children || [];
  const unchanged = current.length === desiredChildren.length && desiredChildren.every((el, i) => current[i] === el);
  if (!unchanged) {
    const focused = list.contains?.(document.activeElement) ? document.activeElement : null;
    list.replaceChildren(...desiredChildren);
    if (focused && document.activeElement !== focused && focused.isConnected && list.contains?.(focused)) focused.focus?.();
  }
  list.scrollTop = keepScroll;
  updateDiffFilterVisibility(ds);
};

const scrollFileIntoView = (path) => {
  const list = elements.diffFileList;
  if (!list?.querySelectorAll) return;
  const rows = list.querySelectorAll('.diff-file-row');
  for (const row of rows) {
    if (row.dataset?.path === path) {
      row.scrollIntoView?.({ block: 'nearest' });
      return;
    }
  }
};

const allDiffFilesExpanded = (ds) => {
  if (ds.files.size === 0) return false;
  for (const path of ds.files.keys()) {
    if (!ds.expanded.has(path)) return false;
  }
  return true;
};

const updateDiffBulkToggle = (ds) => {
  const button = elements.diffBulkToggleBtn;
  if (!button) return;
  const collapse = allDiffFilesExpanded(ds);
  const action = collapse ? 'collapse' : 'expand';
  const label = collapse ? 'Collapse all' : 'Expand all';
  button.dataset.action = action;
  button.setAttribute('aria-label', `${label} files`);
  button.setAttribute('title', label);
};

const renderDiffSidebarContent = (sessionId, ds) => {
  app.renderDiffScope?.(elements.diffScopeSelect, elements.diffScopeTrigger, elements.diffScopeLabel, ds);
  renderDiffTotals(ds);
  updateDiffBulkToggle(ds);
  renderDiffAccordion(sessionId, ds);
  if (ds.pendingScrollPath) {
    if (!app.diffCommentPanelOpen?.(sessionId)) scrollFileIntoView(ds.pendingScrollPath);
    ds.pendingScrollPath = '';
  }
  app.renderDiffCommentQueueBar?.(sessionId);
};

const renderDiffSidebar = (sessionId) => {
  if (sessionId !== state.activeSessionId) return;
  const session = state.sessions?.find?.((item) => String(item?.id || '') === String(sessionId));
  void app.hydrateDiffComments?.(sessionId, { revision: Math.max(Number(session?.transcriptRev) || 0, Number(session?.transcript?.rev) || 0) });
  const ds = sessionDiffState(sessionId);
  applyDiffSidebarVisibility(ds);
  updateDiffBulkToggle(ds);
  app.renderDiffScope?.(elements.diffScopeSelect, elements.diffScopeTrigger, elements.diffScopeLabel, ds);
  renderDiffTotals(ds);
  app.renderDiffCommentQueueBar?.(sessionId);
  // Skip the accordion (and its lazy diff fetches) while hidden; it renders
  // on reveal.
  if (elements.diffSidebar?.hidden) return;
  renderDiffAccordion(sessionId, ds);
  if (ds.pendingScrollPath) {
    if (!app.diffCommentPanelOpen?.(sessionId)) scrollFileIntoView(ds.pendingScrollPath);
    ds.pendingScrollPath = '';
  }
};

// ===== Interactions =====

const toggleDiffFile = (sessionId, path) => {
  const ds = sessionDiffState(sessionId);
  if (ds.expanded.has(path)) {
    ds.expanded.delete(path);
    app.clearDiffCommentPanel?.(sessionId, path);
    // Remember the explicit collapse so live changes stop re-opening it.
    ds.userCollapsed.add(path);
    ds.userExpanded.delete(path);
    if (ds.autoExpandedPath === path) ds.autoExpandedPath = '';
  } else {
    ds.expanded.add(path);
    ds.userCollapsed.delete(path);
    // Explicit expands survive live-follow auto-collapse.
    ds.userExpanded.add(path);
    if (ds.dirtyPaths.has(path) || !ds.diffCache.has(path)) {
      void fetchFileDiff(sessionId, path);
    }
  }
  renderDiffSidebar(sessionId);
};

const expandAllDiffFiles = () => {
  const sessionId = state.activeSessionId;
  if (!sessionId) return;
  const ds = sessionDiffState(sessionId);
  ds.userCollapsed.clear();
  ds.files.forEach((_, path) => {
    ds.expanded.add(path);
    ds.userExpanded.add(path);
  });
  renderDiffSidebar(sessionId);
};

const collapseAllDiffFiles = () => {
  const sessionId = state.activeSessionId;
  if (!sessionId) return;
  const ds = sessionDiffState(sessionId);
  ds.expanded.clear();
  app.clearDiffCommentPanel?.(sessionId);
  ds.userExpanded.clear();
  ds.autoExpandedPath = '';
  // Live changes must not immediately re-open what the user just closed.
  ds.files.forEach((_, path) => ds.userCollapsed.add(path));
  renderDiffSidebar(sessionId);
};

const toggleAllDiffFiles = () => {
  const sessionId = state.activeSessionId;
  if (!sessionId) return;
  const ds = sessionDiffState(sessionId);
  if (allDiffFilesExpanded(ds)) collapseAllDiffFiles();
  else expandAllDiffFiles();
};

const setDiffFilter = (value) => {
  const sessionId = state.activeSessionId;
  if (!sessionId) return;
  const ds = sessionDiffState(sessionId);
  ds.filter = String(value || '');
  renderDiffSidebar(sessionId);
};

// setDiffSidebarHidden dismisses or reveals the sidebar for the ACTIVE
// session only — each session keeps its own dismissal state.
const setDiffSidebarHidden = (hidden) => {
  const sessionId = state.activeSessionId;
  if (!sessionId) return;
  if (hidden) setDiffMaximized(false);
  const ds = sessionDiffState(sessionId);
  ds.hidden = Boolean(hidden);
  if (!ds.hidden && !ds.listLoaded && (ds.summaryKnown || ds.files.size === 0)) void fetchSessionFileChanges(sessionId);
  renderDiffSidebar(sessionId);
  if (!ds.hidden) scheduleDiffRefresh(sessionId);
};

const toggleDiffSidebar = () => {
  const sessionId = state.activeSessionId;
  const ds = sessionId ? sessionDiffState(sessionId) : null;
  if (!sessionId || !ds) return;

  if (isDiffDrawerViewport()) {
    const open = !elements.diffSidebar?.classList.contains('open');
    if (open) {
      app.closeCurrentPlanSurface?.({ restoreFocus: false });
      ds.hidden = false;
      if (!ds.listLoaded && (ds.summaryKnown || ds.files.size === 0)) void fetchSessionFileChanges(sessionId);
      setPanelOpen({
        panel: elements.diffSidebar,
        open: true,
        hiddenWhenClosed: true,
        classTargets: [{ element: elements.diffSidebar, className: 'open' }],
        transitionElement: elements.diffSidebar
      });
      elements.appShell?.classList.remove('diff-open');
      elements.diffToggleBtn?.classList.toggle('active', true);
      renderDiffSidebarContent(sessionId, ds);
      scheduleDiffRefresh(sessionId);
    } else {
      closeDiffDrawer();
    }
    return;
  }
  if (ds.hidden) app.closeCurrentPlanSurface?.({ restoreFocus: false });
  setDiffSidebarHidden(!ds.hidden);
};

const closeDiffDrawer = () => {
  setDiffMaximized(false);
  setPanelOpen({
    panel: elements.diffSidebar,
    open: false,
    hiddenWhenClosed: true,
    classTargets: [{ element: elements.diffSidebar, className: 'open' }],
    transitionElement: elements.diffSidebar
  });
  elements.diffToggleBtn?.classList.toggle('active', false);
  elements.appShell?.classList.remove('diff-open');
  const ds = diffStateBySession.get(state.activeSessionId);
  if (ds) ds.hidden = true;
};

const closeDiffSidebar = () => {
  setDiffMaximized(false);
  const ds = currentDiffState();
  if (!ds) return false;
  const wasOpen = elements.diffSidebar?.classList.contains('open') || !ds.hidden;
  if (isDiffDrawerViewport()) closeDiffDrawer();
  else setDiffSidebarHidden(true);
  // Mutual exclusion with another right-edge surface must be immediate: do
  // not leave a fading Changes element participating in the grid while Plan
  // takes over the same edge.
  elements.diffSidebar?.classList.remove('open');
  elements.appShell?.classList.remove('diff-open');
  elements.diffToggleBtn?.classList.toggle('active', false);
  setPanelHidden(elements.diffSidebar, true);
  ds.hidden = true;
  return Boolean(wasOpen);
};

// ===== Session lifecycle =====

const activateDiffSidebar = (sessionId) => {
  setDiffMaximized(false);
  if (!sessionId) {
    elements.appShell?.classList.remove('diff-open');
    if (elements.diffSidebar) {
      elements.diffSidebar.setAttribute?.('hidden', '');
      elements.diffSidebar.hidden = true;
      elements.diffSidebar.classList.remove('open');
    }
    if (elements.diffToggleBtn) {
      elements.diffToggleBtn.hidden = true;
      elements.diffToggleBtn.setAttribute?.('hidden', '');
    }
    return;
  }
  const ds = sessionDiffState(sessionId);
  const session = state.sessions?.find?.((item) => String(item?.id || '').trim() === String(sessionId));
  void app.hydrateDiffComments?.(sessionId, { revision: Math.max(Number(session?.transcriptRev) || 0, Number(session?.transcript?.rev) || 0) });
  if (!ds.summaryKnown) {
    if (session?.fileChangeSummary) applySessionDiffSummary(sessionId, session.fileChangeSummary);
  }
  if (elements.diffFilterInput) elements.diffFilterInput.value = ds.filter || '';
  renderDiffSidebar(sessionId);
  applyDiffSidebarVisibility(ds);
};

// After a run completes/fails, true-up against the server (events may have
// been missed while detached) and refresh the expanded diffs.
const refreshFileChangesAfterRun = (session) => {
  if (!session?.id) return;
  const ds = diffStateBySession.get(session.id);
  if (!ds || ds.files.size === 0) {
    // The run may have produced changes we never saw (detached tab).
    if (session.id === state.activeSessionId) void fetchSessionFileChanges(session.id);
    return;
  }
  void fetchSessionFileChanges(session.id);
  if (session.id === state.activeSessionId && !ds.hidden) {
    ds.expanded.forEach((path) => {
      ds.dirtyPaths.add(path);
      void fetchFileDiff(session.id, path);
    });
  }
};

// ===== Resizing =====

// clampDiffWidth bounds a requested panel width: never narrower than
// DIFF_MIN_WIDTH, never wider than 60% of the viewport (capped at 900px).
const clampDiffWidth = (width, viewportWidth) => {
  const viewport = Number(viewportWidth) > 0 ? Number(viewportWidth) : 1280;
  const max = Math.max(DIFF_MIN_WIDTH, Math.min(900, Math.floor(viewport * 0.6)));
  return Math.max(DIFF_MIN_WIDTH, Math.min(max, Math.round(Number(width) || 0)));
};

const applyDiffSidebarWidth = (width) => {
  if (!width) return;
  elements.appShell?.style?.setProperty?.('--diff-sidebar-user-width', `${width}px`);
};

const restoreDiffSidebarWidth = () => {
  try {
    const stored = parseInt(localStorage.getItem(STORAGE_KEYS?.diffSidebarWidth || '') || '', 10);
    if (Number.isFinite(stored) && stored > 0) {
      applyDiffSidebarWidth(clampDiffWidth(stored, window.innerWidth));
    }
  } catch {
    // Storage unavailable; default width applies.
  }
};

const resetDiffSidebarWidth = () => {
  elements.appShell?.style?.removeProperty?.('--diff-sidebar-user-width');
  try {
    if (STORAGE_KEYS?.diffSidebarWidth) localStorage.removeItem(STORAGE_KEYS.diffSidebarWidth);
  } catch {
    // Storage unavailable; nothing stored to clear.
  }
};

const initDiffResize = () => {
  const handle = elements.diffResizeHandle;
  if (!handle?.addEventListener) return;
  let draggedWidth = 0;

  handle.addEventListener('pointerdown', (event) => {
    if (diffMaximized) return;
    event.preventDefault?.();
    handle.setPointerCapture?.(event.pointerId);
    elements.appShell?.classList.add('diff-resizing');
  });

  handle.addEventListener('pointermove', (event) => {
    if (diffMaximized || !elements.appShell?.classList.contains('diff-resizing')) return;
    // The panel is anchored to the right edge in both grid and drawer modes.
    draggedWidth = clampDiffWidth(window.innerWidth - event.clientX, window.innerWidth);
    applyDiffSidebarWidth(draggedWidth);
  });

  const finishDrag = (event) => {
    if (!elements.appShell?.classList.contains('diff-resizing')) return;
    elements.appShell.classList.remove('diff-resizing');
    handle.releasePointerCapture?.(event.pointerId);
    if (draggedWidth > 0 && STORAGE_KEYS?.diffSidebarWidth) {
      try {
        localStorage.setItem(STORAGE_KEYS.diffSidebarWidth, String(draggedWidth));
      } catch {
        // Width still applies for this page.
      }
    }
  };
  handle.addEventListener('pointerup', finishDrag);
  handle.addEventListener('pointercancel', finishDrag);
  handle.addEventListener('dblclick', resetDiffSidebarWidth);
};

const isInsideDiffResizeHandle = (target) => {
  let node = target;
  while (node) {
    if (node === elements.diffResizeHandle || node.classList?.contains?.('diff-resize-handle')) return true;
    node = node.parentNode;
  }
  return false;
};

const initDiffCloseGesture = () => {
  const panel = elements.diffSidebar;
  if (!panel?.addEventListener) return;
  initPanelSwipeToClose?.({
    panel,
    side: 'right',
    isEnabled: isDiffDrawerViewport,
    isOpen: () => panel.classList.contains('open'),
    shouldIgnoreTarget: isInsideDiffResizeHandle,
    onClose: closeDiffDrawer
  });
};

// ===== Keyboard =====

const isEditableTarget = (target) => {
  const tag = String(target?.tagName || '').toLowerCase();
  return tag === 'input' || tag === 'textarea' || Boolean(target?.isContentEditable);
};

const handleDiffGlobalKeydown = (event) => {
  if (event.key !== 'Escape') return;
  const input = elements.diffFilterInput;
  if (input && event.target === input) {
    // First escape clears the filter, keeping the panel and maximize state.
    const ds = currentDiffState();
    if (ds?.filter) {
      input.value = '';
      setDiffFilter('');
    }
    input.blur?.();
    return;
  }
  if (diffMaximized) {
    event.preventDefault?.();
    setDiffMaximized(false);
    return;
  }
  if (isEditableTarget(event.target)) return;
  if (isDiffDrawerViewport() && elements.diffSidebar?.classList.contains('open')) {
    closeDiffDrawer();
  }
};

// Arrow keys walk the file headers so the accordion is usable without a
// pointer; Enter/Space on a header toggles it (wired per header).
const handleDiffListKeydown = (event) => {
  if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp') return;
  const list = elements.diffFileList;
  if (!list?.querySelectorAll) return;
  const rows = Array.from(list.querySelectorAll('.diff-file-row'));
  if (rows.length === 0) return;
  const active = typeof document !== 'undefined' ? document.activeElement : null;
  const index = rows.indexOf(active);
  const next = event.key === 'ArrowDown'
    ? (index < 0 ? 0 : Math.min(rows.length - 1, index + 1))
    : (index < 0 ? rows.length - 1 : Math.max(0, index - 1));
  rows[next].focus?.();
  event.preventDefault?.();
};

restoreDiffSidebarWidth();
initDiffResize();
initDiffCloseGesture();

// Fresh page load: session switches activate the sidebar, but the boot path
// never goes through switchToSession. This script loads last, after app-core
// restored the active session id, so activate for it directly. (initialize()
// re-activates after server sync in case boot lands on a different session.)
if (state.activeSessionId && !state.draftSessionActive) {
  activateDiffSidebar(state.activeSessionId);
}

// ===== Wiring =====

const handleDiffViewportChange = () => {
  const ds = currentDiffState();
  if (!ds) {
    elements.appShell?.classList.remove('diff-open');
    elements.diffSidebar?.classList.remove('open');
    return;
  }

  if (diffMaximized) {
    renderDiffSidebarContent(state.activeSessionId, ds);
    if (!ds.hidden) scheduleDiffRefresh(state.activeSessionId);
    return;
  }

  // Drawer mode (narrow) and grid-column mode (wide) use different visibility
  // mechanics. Re-apply immediately when crossing the breakpoint so a drawer
  // opened on mobile becomes a real column on desktop, and a desktop column
  // becomes a closed drawer instead of lingering in an in-between state.
  if (!isDiffDrawerViewport()) {
    elements.diffSidebar?.classList.remove('open');
  }
  renderDiffSidebar(state.activeSessionId);
  if (!ds.hidden) scheduleDiffRefresh(state.activeSessionId);
};

const diffViewportMedia = (() => {
  try {
    return typeof window.matchMedia === 'function' ? window.matchMedia('(max-width: 1099px)') : null;
  } catch {
    return null;
  }
})();
if (diffViewportMedia) {
  if (typeof diffViewportMedia.addEventListener === 'function') {
    diffViewportMedia.addEventListener('change', handleDiffViewportChange);
  } else if (typeof diffViewportMedia.addListener === 'function') {
    diffViewportMedia.addListener(handleDiffViewportChange);
  }
}
window.addEventListener?.('resize', handleDiffViewportChange);
window.addEventListener?.('keydown', handleDiffGlobalKeydown);

const setDiffScope = app.createDiffScopeSetter({
  activeSessionId: () => state.activeSessionId,
  sessionState: sessionDiffState,
  render: renderDiffSidebar,
  fetchList: fetchSessionFileChanges,
  clearComments: app.clearDiffCommentPanel
});
app.wireDiffScopePicker?.(elements.diffScopeTrigger, elements.diffScopeSelect);
elements.diffFileList?.addEventListener?.('keydown', handleDiffListKeydown);
elements.diffScopeSelect?.addEventListener?.('change', (event) => setDiffScope(event.target?.value));
elements.diffToggleBtn?.addEventListener?.('click', toggleDiffSidebar);
elements.diffSidebarCloseBtn?.addEventListener?.('click', () => {
  if (isDiffDrawerViewport()) closeDiffDrawer();
  else setDiffSidebarHidden(true);
});
elements.diffBulkToggleBtn?.addEventListener?.('click', toggleAllDiffFiles);
elements.diffMaximizeBtn?.addEventListener?.('click', () => setDiffMaximized(!diffMaximized));
elements.diffFilterInput?.addEventListener?.('input', (event) => {
  setDiffFilter(event.target?.value ?? elements.diffFilterInput.value ?? '');
});

Object.assign(app, {
  buildDiffRowModel,
  clampDiffWidth,
  computeInlineEmphasis,
  sortDiffPaths,
  buildUnifiedDiff,
  normalizeSessionDiffSummary,
  applySessionDiffSummary,
  handleFileChangeEvent,
  activateDiffSidebar,
  refreshFileChangesAfterRun,
  setDiffSidebarHidden,
  closeDiffSidebar,
  toggleDiffSidebar,
  setDiffScope,
  toggleDiffFile,
  pinDiffFileExpanded,
  scrollDiffFileIntoView: scrollFileIntoView,
  setDiffMaximized,
  fetchSessionFileChanges,
  renderDiffSidebar
});
})();
