'use strict';

const worktreeApp = window.TermLLMApp || (window.TermLLMApp = {});

(() => {
  const { UI_PREFIX, state, elements } = worktreeApp;
  if (!UI_PREFIX || !state || !elements) return;

  let available = false;
  let loading = false;
  let menu = null;

  const authHeaders = () => (typeof worktreeApp.requestHeaders === 'function'
    ? worktreeApp.requestHeaders(state.activeSessionId || '')
    : { 'Content-Type': 'application/json' });

  const activeSession = () => (typeof worktreeApp.getActiveSession === 'function'
    ? worktreeApp.getActiveSession()
    : (state.sessions || []).find((s) => s.id === state.activeSessionId) || null);

  const labelForDir = (dir) => {
    if (!dir) return 'root';
    const wt = (state.worktrees || []).find((item) => item.dir === dir);
    if (wt) return `⌥ ${wt.name}`;
    const session = activeSession();
    if (session?.worktreeDir === dir && session.worktreeName) return `⌥ ${session.worktreeName}`;
    return '⌥ worktree';
  };

  const setChipVisible = (visible) => {
    if (elements.chipWorktree) elements.chipWorktree.hidden = !visible;
    if (elements.chipSepEffortWorktree) elements.chipSepEffortWorktree.hidden = !visible;
  };

  const renderWorktreeChip = () => {
    if (!available) {
      setChipVisible(false);
      return;
    }
    setChipVisible(true);
    const session = activeSession();
    const lockedDir = !state.draftSessionActive && session ? (session.worktreeDir || '') : '';
    const draftDir = state.draftSessionActive ? (state.selectedWorktreeDir || '') : '';
    const dir = lockedDir || draftDir;
    if (elements.chipWorktreeLabel) elements.chipWorktreeLabel.textContent = labelForDir(dir);
    if (elements.chipWorktreeTrigger) {
      elements.chipWorktreeTrigger.title = state.draftSessionActive
        ? 'Choose worktree for this draft session'
        : (dir ? 'Open worktree diff/actions' : 'Root checkout');
      elements.chipWorktreeTrigger.classList.toggle('locked', !state.draftSessionActive);
    }
  };

  const loadWorktrees = async () => {
    try {
      const res = await fetch(`${UI_PREFIX}/v1/worktrees`, { headers: authHeaders() });
      if (res.status === 409) {
        available = false;
        state.worktrees = [];
        renderWorktreeChip();
        return [];
      }
      if (!res.ok) throw await (worktreeApp.normalizeError ? worktreeApp.normalizeError(res) : res.text());
      const data = await res.json();
      available = true;
      state.worktrees = Array.isArray(data.worktrees) ? data.worktrees : [];
      renderWorktreeChip();
      return state.worktrees;
    } catch (_err) {
      available = false;
      renderWorktreeChip();
      return [];
    }
  };

  const closeMenu = () => {
    if (menu) menu.remove();
    menu = null;
    if (elements.chipWorktreeTrigger) elements.chipWorktreeTrigger.setAttribute('aria-expanded', 'false');
  };

  const chooseWorktree = (row) => {
    state.selectedWorktreeDir = row && !row.root ? row.dir : '';
    state.selectedWorktreeName = row && !row.root ? row.name : '';
    renderWorktreeChip();
    closeMenu();
  };

  const createWorktree = async () => {
    const name = window.prompt('New worktree name (blank for generated):', '') || '';
    loading = true;
    renderWorktreeChip();
    try {
      const res = await fetch(`${UI_PREFIX}/v1/worktrees`, {
        method: 'POST',
        headers: authHeaders(),
        body: JSON.stringify({ name })
      });
      if (!res.ok) throw await (worktreeApp.normalizeError ? worktreeApp.normalizeError(res) : res.text());
      const data = await res.json();
      await loadWorktrees();
      if (data.worktree) chooseWorktree(data.worktree);
    } catch (err) {
      window.alert(err?.message || String(err || 'Failed to create worktree'));
    } finally {
      loading = false;
      renderWorktreeChip();
    }
  };

  const openDiffActions = async (dir) => {
    if (!dir) return;
    const action = window.prompt('Worktree action: diff, merge, promote, remove', 'diff');
    if (!action) return;
    const value = action.trim().toLowerCase();
    try {
      if (value === 'diff') {
        const res = await fetch(`${UI_PREFIX}/v1/worktrees/diff?dir=${encodeURIComponent(dir)}`, { headers: authHeaders() });
        if (!res.ok) throw await (worktreeApp.normalizeError ? worktreeApp.normalizeError(res) : res.text());
        const data = await res.json();
        window.alert(data.diff || 'Worktree is clean.');
      } else if (value === 'merge') {
        const res = await fetch(`${UI_PREFIX}/v1/worktrees/merge`, { method: 'POST', headers: authHeaders(), body: JSON.stringify({ dir }) });
        if (!res.ok && res.status !== 409) throw await (worktreeApp.normalizeError ? worktreeApp.normalizeError(res) : res.text());
        const data = await res.json().catch(() => ({}));
        if (res.status === 409) window.alert(`Conflicts:\n${(data.result?.conflicts || []).join('\n')}`);
        else window.alert('Merged back staged, uncommitted on root.');
      } else if (value === 'promote') {
        const branch = window.prompt('Branch name:', '');
        if (!branch) return;
        const res = await fetch(`${UI_PREFIX}/v1/worktrees/promote`, { method: 'POST', headers: authHeaders(), body: JSON.stringify({ dir, branch }) });
        if (!res.ok) throw await (worktreeApp.normalizeError ? worktreeApp.normalizeError(res) : res.text());
        window.alert('Promoted.');
      } else if (value === 'remove' || value === 'rm') {
        if (!window.confirm('Remove this worktree?')) return;
        const res = await fetch(`${UI_PREFIX}/v1/worktrees?dir=${encodeURIComponent(dir)}`, { method: 'DELETE', headers: authHeaders() });
        if (!res.ok) throw await (worktreeApp.normalizeError ? worktreeApp.normalizeError(res) : res.text());
        const session = activeSession();
        if (session && session.worktreeDir === dir) session.worktreeRemoved = true;
        await loadWorktrees();
        renderWorktreeChip();
      }
    } catch (err) {
      window.alert(err?.message || String(err || 'Worktree action failed'));
    }
  };

  const openMenu = async () => {
    if (!state.draftSessionActive) {
      const session = activeSession();
      await openDiffActions(session?.worktreeDir || '');
      return;
    }
    const rows = await loadWorktrees();
    closeMenu();
    menu = document.createElement('div');
    menu.className = 'chip-popover-runtime worktree-popover';
    menu.setAttribute('role', 'listbox');
    const addRow = (text, onClick) => {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'chip-option';
      btn.textContent = text;
      btn.addEventListener('click', onClick);
      menu.appendChild(btn);
    };
    addRow('root checkout', () => chooseWorktree(null));
    rows.filter((r) => !r.root).forEach((row) => {
      const ref = row.branch || (row.head_sha ? `detached@${row.head_sha.slice(0, 8)}` : 'detached');
      addRow(`${row.name} · ±${row.dirty_files || 0} · ${ref}`, () => chooseWorktree(row));
    });
    addRow(loading ? 'creating…' : '+ new worktree…', () => { void createWorktree(); });
    const rect = elements.chipWorktreeTrigger.getBoundingClientRect();
    menu.style.position = 'fixed';
    menu.style.top = `${Math.round(rect.bottom + 6)}px`;
    menu.style.left = `${Math.round(rect.left)}px`;
    menu.style.zIndex = '1000';
    document.body.appendChild(menu);
    elements.chipWorktreeTrigger.setAttribute('aria-expanded', 'true');
  };

  if (elements.chipWorktreeTrigger) {
    elements.chipWorktreeTrigger.addEventListener('click', (event) => {
      event.preventDefault();
      if (menu) closeMenu(); else void openMenu();
    });
  }
  document.addEventListener('click', (event) => {
    if (!menu) return;
    if (event.target === elements.chipWorktreeTrigger || menu.contains(event.target)) return;
    closeMenu();
  });

  Object.assign(worktreeApp, {
    loadWorktrees,
    renderWorktreeChip
  });

  void loadWorktrees();
  setInterval(renderWorktreeChip, 1000);
})();
