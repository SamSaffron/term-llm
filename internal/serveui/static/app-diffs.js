(() => {
'use strict';

// Live diff sidebar: shows the active session's cumulative file changes while
// the agent works. Fed by metadata-only `response.file_change` stream events;
// diff content is fetched on demand from the session file-changes endpoints.
// Files render as an accordion: each one expands inline where it sits in the
// list and can be collapsed individually.
const app = window.TermLLMApp || (window.TermLLMApp = {});
const { state, elements, STORAGE_KEYS, UI_PREFIX } = app;

const DIFF_REFRESH_DEBOUNCE_MS = 350;
const DIFF_RENDER_DEBOUNCE_MS = 80;
const DIFF_MIN_WIDTH = 280;
// Per-line tokenizing is cheap but not free; skip highlighting huge diffs.
const DIFF_HIGHLIGHT_MAX_ROWS = 1500;
// Cap initially rendered rows per file so a huge retained diff cannot flood
// the DOM; a "show more" control reveals the rest on demand.
const DIFF_RENDER_MAX_ROWS = 400;

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
      fullDiffPaths: new Set(),  // paths the user asked to render beyond the row cap
      diffCache: new Map(),      // path -> { seq, data }
      dirtyPaths: new Set(),     // cached diff is stale (newer change seen)
      inflight: new Map(),       // path -> Promise (request dedup)
      refreshTimer: null,
      renderTimer: null,
      lastActivityAt: 0,
      pendingScrollPath: '',
      listLoaded: false,
      hidden: false              // user dismissed the sidebar for THIS session
    };
    diffStateBySession.set(sessionId, ds);
  }
  return ds;
};

const authHeaders = () => (state.token ? { Authorization: `Bearer ${state.token}` } : {});

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

  // Live follow: open the file being edited unless the user collapsed it.
  const newlyExpanded = !ds.expanded.has(path) && !ds.userCollapsed.has(path);
  if (!ds.userCollapsed.has(path)) ds.expanded.add(path);

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

const fetchSessionFileChanges = async (sessionId) => {
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
    if (sessionId === state.activeSessionId) renderDiffSidebar(sessionId);
  } catch {
    // Network failures leave existing state untouched.
  }
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
      if (!resp.ok) return null;
      const data = await resp.json();
      ds.diffCache.set(path, { seq: seqAtRequest, data });

      // A newer change may have landed mid-fetch; leave it dirty and schedule
      // another refresh (the debounce timer that requested this fetch already
      // fired, so without rescheduling the stale diff would stick around).
      const latestSeq = ds.files.get(path)?.lastSeq || 0;
      if (latestSeq <= seqAtRequest) ds.dirtyPaths.delete(path);
      else scheduleDiffRefresh(sessionId);

      // True-up the list entry with cumulative counts from the actual diff.
      const entry = ds.files.get(path);
      if (entry && !data.truncated) {
        const { adds, dels } = countRowChanges(buildDiffRowModel(data.hunks));
        entry.adds = adds;
        entry.dels = dels;
        entry.truncated = Boolean(data.truncated);
      }

      // Coalesced: many fetches resolving in a burst share renders.
      if (sessionId === state.activeSessionId) scheduleDiffRender(sessionId);
      return data;
    } catch {
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

const applyDiffSidebarVisibility = (ds) => {
  const hasChanges = ds && ds.files.size > 0;
  const drawer = isDiffDrawerViewport();
  const visible = hasChanges && !ds.hidden && !drawer;

  if (elements.diffSidebar) {
    if (visible || (drawer && elements.diffSidebar.classList.contains('open') && hasChanges)) {
      elements.diffSidebar.removeAttribute?.('hidden');
      elements.diffSidebar.hidden = false;
    } else {
      elements.diffSidebar.setAttribute?.('hidden', '');
      elements.diffSidebar.hidden = true;
    }
    if (!drawer || !hasChanges) elements.diffSidebar.classList.remove('open');
  }
  elements.appShell?.classList.toggle('diff-open', visible);

  if (elements.diffToggleBtn) {
    elements.diffToggleBtn.hidden = !hasChanges;
    if (hasChanges) elements.diffToggleBtn.removeAttribute?.('hidden');
    else elements.diffToggleBtn.setAttribute?.('hidden', '');
    elements.diffToggleBtn.classList.toggle('active', visible || (drawer && elements.diffSidebar?.classList.contains('open')));
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

// renderDiffCode renders one code cell, syntax-highlighted per line when hljs
// and the language are available. Per-line tokenizing is stateless, so
// multi-line constructs (block comments, template literals) may mis-color
// continuation lines — an accepted trade-off for live re-rendering.
const renderDiffCode = (type, text, lang) => {
  const el = createEl('span', 'diff-code');
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

const renderDiffTotals = (ds) => {
  let adds = 0;
  let dels = 0;
  ds.files.forEach((entry) => {
    adds += entry.adds;
    dels += entry.dels;
  });
  const summary = [];
  if (adds > 0) summary.push(`+${adds}`);
  if (dels > 0) summary.push(`−${dels}`);
  if (elements.diffSidebarTotals) elements.diffSidebarTotals.textContent = summary.join(' ');
  if (elements.diffToggleBadge) {
    const parts = [];
    const fileCount = ds.files.size || 0;
    const fileCountEl = createEl('span', 'diff-toggle-file-count');
    fileCountEl.innerHTML = '<svg class="diff-toggle-file-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4.5 1.75h4.25L12.5 5.5v8.75h-8z"/><path d="M8.75 1.75V5.5h3.75"/><path d="M6.25 8.5h4"/><path d="M6.25 11h3"/></svg>';
    fileCountEl.dataset.fileCount = String(fileCount);
    parts.push(fileCountEl);
    if (adds > 0) parts.push(createEl('span', 'diff-toggle-stat-add', `+${adds}`));
    if (dels > 0) parts.push(createEl('span', 'diff-toggle-stat-del', `−${dels}`));
    if (parts.length === 1 && fileCount === 0) parts.push(createEl('span', 'diff-toggle-stat-neutral', '0'));
    elements.diffToggleBadge.replaceChildren(...parts);
  }
  if (elements.diffToggleBtn) {
    const fileCount = ds.files.size || 0;
    const fileLabel = `${fileCount} changed ${fileCount === 1 ? 'file' : 'files'}`;
    elements.diffToggleBtn.title = summary.length > 0 ? `${fileLabel} (${summary.join(' ')})` : fileLabel;
    elements.diffToggleBtn.setAttribute?.('aria-label', `Toggle file changes: ${elements.diffToggleBtn.title}`);
  }
};

const renderDiffFileBody = (sessionId, ds, path) => {
  const body = createEl('div', 'diff-file-body');
  const cached = ds.diffCache.get(path);
  if (!cached) {
    body.appendChild(createEl('div', 'diff-note', 'Loading diff…'));
    void fetchFileDiff(sessionId, path);
    return body;
  }
  if (cached.data.truncated) {
    body.appendChild(createEl('div', 'diff-note', 'Diff content was not retained for this file (too large, binary, or unrecoverable).'));
    return body;
  }

  const rows = buildDiffRowModel(cached.data.hunks);
  const lang = rows.length <= DIFF_HIGHLIGHT_MAX_ROWS ? (cached.data.lang || '') : '';
  if (lang) requestDiffHighlight();

  let visibleRows = rows;
  let hiddenCount = 0;
  if (rows.length > DIFF_RENDER_MAX_ROWS && !ds.fullDiffPaths.has(path)) {
    visibleRows = rows.slice(0, DIFF_RENDER_MAX_ROWS);
    hiddenCount = rows.length - visibleRows.length;
  }

  const table = createEl('div', 'diff-rows');
  visibleRows.forEach((row) => {
    const rowEl = createEl('div', `diff-row ${row.type}`);
    if (row.type === 'hunk') {
      rowEl.appendChild(createEl('span', 'diff-hunk-sep', '⋯'));
    } else {
      rowEl.appendChild(createEl('span', 'diff-ln old', row.oldNo ? String(row.oldNo) : ''));
      rowEl.appendChild(createEl('span', 'diff-ln new', row.newNo ? String(row.newNo) : ''));
      rowEl.appendChild(renderDiffCode(row.type, row.text, lang));
    }
    table.appendChild(rowEl);
  });
  body.appendChild(table);

  if (hiddenCount > 0) {
    const more = createEl('button', 'diff-show-more', `Show ${hiddenCount} more lines`);
    more.setAttribute('type', 'button');
    more.addEventListener('click', () => {
      ds.fullDiffPaths.add(path);
      renderDiffSidebar(sessionId);
    });
    body.appendChild(more);
  }
  return body;
};

const renderDiffAccordion = (sessionId, ds) => {
  const list = elements.diffFileList;
  if (!list) return;
  const keepScroll = list.scrollTop;

  const blocks = [];
  const paths = Array.from(ds.files.keys()).sort();
  paths.forEach((path) => {
    const entry = ds.files.get(path);
    const expanded = ds.expanded.has(path);
    const block = createEl('div', 'diff-file');

    const header = createEl('button', `diff-file-row${expanded ? ' expanded' : ''}`);
    header.setAttribute('type', 'button');
    header.setAttribute('aria-expanded', expanded ? 'true' : 'false');
    header.title = path;
    if (header.dataset) header.dataset.path = path;
    header.appendChild(createEl('span', 'diff-chevron', '▸'));
    header.appendChild(createEl('span', `diff-kind-badge diff-kind-${entry.kind}`, kindBadgeLabel[entry.kind] || 'M'));
    const nameWrap = createEl('span', 'diff-file-name');
    nameWrap.appendChild(createEl('span', 'diff-file-base', fileBaseName(path)));
    const dir = fileDirName(path);
    if (dir) nameWrap.appendChild(createEl('span', 'diff-file-dir', dir));
    header.appendChild(nameWrap);
    const counts = createEl('span', 'diff-file-counts');
    if (entry.truncated) {
      counts.appendChild(createEl('span', 'diff-count-muted', '–'));
    } else {
      if (entry.adds > 0) counts.appendChild(createEl('span', 'diff-count-add', `+${entry.adds}`));
      if (entry.dels > 0) counts.appendChild(createEl('span', 'diff-count-del', `−${entry.dels}`));
    }
    header.appendChild(counts);
    header.addEventListener('click', () => toggleDiffFile(sessionId, path));
    block.appendChild(header);

    if (expanded) block.appendChild(renderDiffFileBody(sessionId, ds, path));
    blocks.push(block);
  });

  list.replaceChildren(...blocks);
  list.scrollTop = keepScroll;
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

const renderDiffSidebar = (sessionId) => {
  if (sessionId !== state.activeSessionId) return;
  const ds = sessionDiffState(sessionId);
  applyDiffSidebarVisibility(ds);
  if (ds.files.size === 0) return;
  renderDiffTotals(ds);
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
  } else {
    ds.expanded.add(path);
    ds.userCollapsed.delete(path);
    if (ds.dirtyPaths.has(path) || !ds.diffCache.has(path)) {
      void fetchFileDiff(sessionId, path);
    }
  }
  renderDiffSidebar(sessionId);
};

// setDiffSidebarHidden dismisses or reveals the sidebar for the ACTIVE
// session only — each session keeps its own dismissal state.
const setDiffSidebarHidden = (hidden) => {
  const sessionId = state.activeSessionId;
  if (!sessionId) return;
  const ds = sessionDiffState(sessionId);
  ds.hidden = Boolean(hidden);
  renderDiffSidebar(sessionId);
  if (!ds.hidden) scheduleDiffRefresh(sessionId);
};

const toggleDiffSidebar = () => {
  if (isDiffDrawerViewport()) {
    const open = !elements.diffSidebar?.classList.contains('open');
    if (open) {
      elements.diffSidebar?.removeAttribute?.('hidden');
      if (elements.diffSidebar) elements.diffSidebar.hidden = false;
      elements.diffSidebar?.classList.add('open');
      // The closed drawer skips panel rendering; populate it now.
      renderDiffSidebar(state.activeSessionId);
      scheduleDiffRefresh(state.activeSessionId);
    } else {
      closeDiffDrawer();
    }
    return;
  }
  const ds = state.activeSessionId ? sessionDiffState(state.activeSessionId) : null;
  setDiffSidebarHidden(!ds?.hidden);
};

const closeDiffDrawer = () => {
  elements.diffSidebar?.classList.remove('open');
  const ds = diffStateBySession.get(state.activeSessionId);
  if (ds) applyDiffSidebarVisibility(ds);
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
  renderDiffSidebar(sessionId);
  applyDiffSidebarVisibility(ds);
  if (!ds.listLoaded) void fetchSessionFileChanges(sessionId);
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
  let startX = 0;
  let startY = 0;
  let tracking = false;

  panel.addEventListener('pointerdown', (event) => {
    if (!isDiffDrawerViewport()) return;
    if (!panel.classList.contains('open')) return;
    if (isInsideDiffResizeHandle(event.target)) return;
    startX = Number(event.clientX) || 0;
    startY = Number(event.clientY) || 0;
    tracking = true;
  });

  panel.addEventListener('pointerup', (event) => {
    if (!tracking) return;
    tracking = false;
    const dx = (Number(event.clientX) || 0) - startX;
    const dy = Math.abs((Number(event.clientY) || 0) - startY);
    // Right-side drawer: a decisive rightward swipe closes it. Keep the
    // threshold high enough that ordinary vertical diff scrolling is ignored.
    if (dx > 70 && dx > dy * 1.4) closeDiffDrawer();
  });

  panel.addEventListener('pointercancel', () => {
    tracking = false;
  });
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

elements.diffToggleBtn?.addEventListener?.('click', toggleDiffSidebar);
elements.diffSidebarCloseBtn?.addEventListener?.('click', () => {
  if (isDiffDrawerViewport()) closeDiffDrawer();
  else setDiffSidebarHidden(true);
});

Object.assign(app, {
  buildDiffRowModel,
  clampDiffWidth,
  handleFileChangeEvent,
  activateDiffSidebar,
  refreshFileChangesAfterRun,
  setDiffSidebarHidden,
  toggleDiffSidebar,
  toggleDiffFile,
  fetchSessionFileChanges,
  renderDiffSidebar
});
})();
