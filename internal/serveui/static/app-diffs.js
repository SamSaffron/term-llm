(() => {
'use strict';

// Live diff sidebar: shows the active session's cumulative file changes while
// the agent works. Fed by metadata-only `response.file_change` stream events;
// diff content is fetched on demand from the session file-changes endpoints.
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

const sessionDiffState = (sessionId) => {
  let ds = diffStateBySession.get(sessionId);
  if (!ds) {
    ds = {
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

const normalizeSessionDiffSummary = (value) => {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null;
  const fileCount = Number(value.file_count);
  const adds = Number(value.adds);
  const dels = Number(value.dels);
  if (!Number.isSafeInteger(fileCount) || fileCount < 0
      || !Number.isSafeInteger(adds) || adds < 0
      || !Number.isSafeInteger(dels) || dels < 0) return null;
  return { fileCount, adds, dels };
};

const applySessionDiffSummary = (sessionId, value) => {
  const owner = String(sessionId || '').trim();
  const summary = normalizeSessionDiffSummary(value);
  if (!owner || !summary) return false;
  const ds = sessionDiffState(owner);
  ds.summaryKnown = true;
  ds.summary = summary;
  if (summary.fileCount === 0) {
    ds.files.clear();
    ds.listLoaded = true;
    reconcileDiffPathState(ds);
  } else if (ds.files.size === 0) {
    ds.listLoaded = false;
  }
  if (owner === state.activeSessionId) renderDiffSidebar(owner);
  return true;
};

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
  const path = String(payload.path);
  const seq = Number(payload.seq) || 0;

  const existing = ds.files.get(path);
  // Replayed events (stream reconnect) arrive with stale sequence numbers.
  if (existing && seq && existing.lastSeq && seq <= existing.lastSeq) return;

  ds.files.set(path, {
    path,
    kind: String(payload.kind || 'modify'),
    adds: Number(payload.adds) || 0,
    dels: Number(payload.dels) || 0,
    truncated: Boolean(payload.truncated),
    lastSeq: seq
  });
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
const reconcileDiffPathState = (ds) => {
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
};

const fetchSessionFileChanges = async (sessionId) => {
  if (!isResolvedSessionIdentity(sessionId)) return false;
  const ds = sessionDiffState(sessionId);
  // Snapshot per-path seqs so live rows whose change events land while this
  // fetch is in flight survive the authoritative replace below.
  const seqAtStart = new Map();
  ds.files.forEach((entry, path) => seqAtStart.set(path, entry.lastSeq || 0));
  try {
    const resp = await fetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(sessionId)}/file-changes`, {
      headers: authHeaders()
    });
    if (!resp.ok) return; // 404 = tracking disabled; treat as no changes
    const body = await resp.json();
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
        kind: String(entry.kind || 'modify'),
        adds: Number(entry.adds) || 0,
        dels: Number(entry.dels) || 0,
        truncated: Boolean(entry.truncated),
        lastSeq: Number(entry.seq) || prev?.lastSeq || 0
      });
    });
    ds.files.forEach((entry, path) => {
      if (next.has(path)) return;
      if ((entry.lastSeq || 0) > (seqAtStart.get(path) || 0)) next.set(path, entry);
    });
    ds.files = next;
    ds.listLoaded = true;
    ds.summaryKnown = true;
    ds.summary = { fileCount: next.size, adds: 0, dels: 0 };
    next.forEach((entry) => {
      ds.summary.adds += entry.adds;
      ds.summary.dels += entry.dels;
    });
    reconcileDiffPathState(ds);
    // Cached diffs predating the authoritative seq are stale even though no
    // live event bumped them (the tab may have missed events while detached).
    ds.files.forEach((entry, path) => {
      const cached = ds.diffCache.get(path);
      if (cached && (entry.lastSeq || 0) > (cached.seq || 0)) ds.dirtyPaths.add(path);
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

  const seqAtRequest = ds.files.get(path)?.lastSeq || 0;
  const request = (async () => {
    try {
      const url = `${UI_PREFIX}/v1/sessions/${encodeURIComponent(sessionId)}/file-changes/diff?path=${encodeURIComponent(path)}`;
      const resp = await fetch(url, { headers: authHeaders() });
      if (!resp.ok) {
        markDiffFetchError(sessionId, path);
        return null;
      }
      const data = await resp.json();
      // rev, not seq, keys body rebuilds: a refetch can return newer server
      // content under an unchanged local seq (events missed while detached).
      ds.cacheRev += 1;
      ds.diffCache.set(path, { seq: seqAtRequest, rev: ds.cacheRev, data });
      ds.fetchErrors.delete(path);

      // A newer change may have landed mid-fetch; leave it dirty and schedule
      // another refresh (the debounce timer that requested this fetch already
      // fired, so without rescheduling the stale diff would stick around).
      const latestSeq = ds.files.get(path)?.lastSeq || 0;
      if (latestSeq <= seqAtRequest) ds.dirtyPaths.delete(path);
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
      markDiffFetchError(sessionId, path);
      return null;
    } finally {
      ds.inflight.delete(path);
    }
  })();
  ds.inflight.set(path, request);
  return request;
};

// ===== Rendering =====

const createEl = (tag, className, text) => {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
};

const fileBaseName = (path) => {
  const idx = path.lastIndexOf('/');
  return idx >= 0 ? path.slice(idx + 1) : path;
};

const fileDirName = (path) => {
  const idx = path.lastIndexOf('/');
  return idx > 0 ? path.slice(0, idx) : '';
};

const kindBadgeLabel = { create: 'A', modify: 'M', delete: 'D' };

const diffTotals = (ds) => {
  if (!ds) return { fileCount: 0, adds: 0, dels: 0 };
  if (!ds.listLoaded && ds.summaryKnown) return ds.summary;
  const totals = { fileCount: ds.files.size, adds: 0, dels: 0 };
  ds.files.forEach((entry) => {
    totals.adds += entry.adds;
    totals.dels += entry.dels;
  });
  return totals;
};

const applyDiffSidebarVisibility = (ds) => {
  const hasChanges = diffTotals(ds).fileCount > 0;
  const drawer = isDiffDrawerViewport();
  const visible = hasChanges && !ds.hidden && !drawer;
  const drawerOpen = Boolean(hasChanges && drawer && elements.diffSidebar?.classList.contains('open'));

  if (elements.diffSidebar) {
    if (!hasChanges) {
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
    elements.diffToggleBtn.hidden = !hasChanges;
    if (hasChanges) elements.diffToggleBtn.removeAttribute?.('hidden');
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
  if (fileCount === 0) {
    if (elements.diffSidebarTotals) elements.diffSidebarTotals.textContent = '';
    if (elements.diffToggleBadge) {
      elements.diffToggleBadge.replaceChildren();
    }
    if (elements.diffToggleBtn) {
      elements.diffToggleBtn.title = '';
      elements.diffToggleBtn.setAttribute?.('aria-label', 'Toggle file changes');
    }
    return;
  }

  const summary = [];
  if (adds > 0) summary.push(`+${adds}`);
  if (dels > 0) summary.push(`−${dels}`);
  const summaryText = summary.join(' ');
  if (elements.diffSidebarTotals) elements.diffSidebarTotals.textContent = summaryText;
  if (elements.diffToggleBadge) {
    const badge = elements.diffToggleBadge;
    const parts = [];
    const fileCountEl = createEl('span', 'diff-toggle-file-count');
    fileCountEl.innerHTML = DIFF_FILE_ICON_SVG;
    fileCountEl.dataset.fileCount = String(fileCount);
    parts.push(fileCountEl);
    if (adds > 0) parts.push(createEl('span', 'diff-toggle-stat-add', `+${adds}`));
    if (dels > 0) parts.push(createEl('span', 'diff-toggle-stat-del', `−${dels}`));
    badge.replaceChildren(...parts);
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

const imageDiffContentURL = (sessionId, path, side) => `${UI_PREFIX}/v1/sessions/${encodeURIComponent(sessionId)}/file-changes/content?path=${encodeURIComponent(path)}&side=${side}`;

const renderImageDiff = (sessionId, path, data) => {
  const comparison = createEl('div', `diff-image-comparison diff-image-${data.kind || 'modify'}`);
  const sides = data.kind === 'create' ? ['after'] : data.kind === 'delete' ? ['before'] : ['before', 'after'];
  sides.forEach((side) => {
    const panel = createEl('div', 'diff-image-side');
    panel.appendChild(createEl('div', 'diff-image-label', side === 'before' ? 'Before' : 'After'));
    const src = imageDiffContentURL(sessionId, path, side);
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

const renderDiffFileBody = (sessionId, ds, path) => {
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
    body.appendChild(renderImageDiff(sessionId, path, cached.data));
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

  const table = createEl('div', 'diff-rows');
  visibleRows.forEach((row) => {
    const rowEl = createEl('div', `diff-row ${row.type}`);
    if (row.type === 'hunk') {
      rowEl.appendChild(createEl('span', 'diff-hunk-sep', '⋯'));
    } else {
      rowEl.appendChild(createEl('span', 'diff-ln old', row.oldNo ? String(row.oldNo) : ''));
      rowEl.appendChild(createEl('span', 'diff-ln new', row.newNo ? String(row.newNo) : ''));
      rowEl.appendChild(renderDiffCode(row.type, row.text, lang, row.emph));
    }
    table.appendChild(rowEl);
  });
  body.appendChild(table);

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
    typeof window.hljs !== 'undefined' ? 1 : 0
  ].join('|');
  if (!block.body || block.bodyKey !== bodyKey) {
    block.body = renderDiffFileBody(sessionId, ds, path);
    block.bodyKey = bodyKey;
    block.el.replaceChildren(block.header, block.body);
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
  const desired = paths.map((path) => syncDiffFileBlock(sessionId, ds, path).el);

  if (desired.length === 0 && ds.filter) {
    list.replaceChildren(createEl('div', 'diff-note', 'No files match the filter.'));
  } else {
    // Only touch the list when membership or order actually changed; count
    // updates and body swaps patch reused nodes above without any list-level
    // DOM churn (preserving scroll position, text selection, and focus).
    const current = list.children || [];
    const unchanged = current.length === desired.length && desired.every((el, i) => current[i] === el);
    if (!unchanged) list.replaceChildren(...desired);
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
  const actionElement = button.querySelector?.('.diff-bulk-toggle-action');
  if (actionElement) actionElement.textContent = collapse ? 'Collapse' : 'Expand';
};

const renderDiffSidebarContent = (sessionId, ds) => {
  renderDiffTotals(ds);
  updateDiffBulkToggle(ds);
  renderDiffAccordion(sessionId, ds);
  if (ds.pendingScrollPath) {
    scrollFileIntoView(ds.pendingScrollPath);
    ds.pendingScrollPath = '';
  }
};

const renderDiffSidebar = (sessionId) => {
  if (sessionId !== state.activeSessionId) return;
  const ds = sessionDiffState(sessionId);
  applyDiffSidebarVisibility(ds);
  updateDiffBulkToggle(ds);
  renderDiffTotals(ds);
  if (ds.files.size === 0) return;
  // Skip the accordion (and its lazy diff fetches) while hidden; it renders
  // on reveal.
  if (elements.diffSidebar?.hidden) return;
  renderDiffAccordion(sessionId, ds);
  if (ds.pendingScrollPath) {
    scrollFileIntoView(ds.pendingScrollPath);
    ds.pendingScrollPath = '';
  }
};

// ===== Interactions =====

const toggleDiffFile = (sessionId, path) => {
  const ds = sessionDiffState(sessionId);
  if (ds.expanded.has(path)) {
    ds.expanded.delete(path);
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
  if (!ds.summaryKnown) {
    const session = state.sessions?.find?.((item) => String(item?.id || '').trim() === String(sessionId));
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
    event.preventDefault?.();
    handle.setPointerCapture?.(event.pointerId);
    elements.appShell?.classList.add('diff-resizing');
  });

  handle.addEventListener('pointermove', (event) => {
    if (!elements.appShell?.classList.contains('diff-resizing')) return;
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
    // First escape clears the filter, keeping the panel open.
    const ds = currentDiffState();
    if (ds?.filter) {
      input.value = '';
      setDiffFilter('');
    }
    input.blur?.();
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

elements.diffFileList?.addEventListener?.('keydown', handleDiffListKeydown);
elements.diffToggleBtn?.addEventListener?.('click', toggleDiffSidebar);
elements.diffSidebarCloseBtn?.addEventListener?.('click', () => {
  if (isDiffDrawerViewport()) closeDiffDrawer();
  else setDiffSidebarHidden(true);
});
elements.diffBulkToggleBtn?.addEventListener?.('click', toggleAllDiffFiles);
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
  toggleDiffFile,
  fetchSessionFileChanges,
  renderDiffSidebar
});
})();
