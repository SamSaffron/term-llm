'use strict';

const worktreeApp = window.TermLLMApp || (window.TermLLMApp = {});

(() => {
  const { UI_PREFIX, state, elements } = worktreeApp;
  if (!UI_PREFIX || !state || !elements) return;

  const worktreesEnabled = window.TERM_LLM_WORKTREES_ENABLED === true;
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
    if (!worktreesEnabled) {
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
        : (dir ? 'Open worktree diff/actions' : 'Manage worktrees');
      elements.chipWorktreeTrigger.classList.toggle('locked', !state.draftSessionActive);
    }
  };

  const loadWorktrees = async () => {
    if (!worktreesEnabled) return [];
    try {
      const res = await fetch(`${UI_PREFIX}/v1/worktrees`, { headers: authHeaders() });
      if (!res.ok) throw await (worktreeApp.normalizeError ? worktreeApp.normalizeError(res) : res.text());
      const data = await res.json();
      state.worktrees = Array.isArray(data.worktrees) ? data.worktrees : [];
      renderWorktreeChip();
      return state.worktrees;
    } catch (_err) {
      return [];
    }
  };

  const closeMenu = () => {
    if (!menu) return;
    menu.remove();
    menu = null;
    if (elements.chipPopoverBackdrop) elements.chipPopoverBackdrop.hidden = true;
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
        const result = data.result || {};
        const mergeOutcome = result.committed ? 'committed' : (result.applied ? 'staged, uncommitted' : 'no changes to apply');
        if (res.status === 409) {
          if (data.error === 'root_dirty') {
            window.alert(`Merge not attempted: root checkout is dirty.\n\nRoot: ${result.root_dir || ''}\nSource: ${result.worktree_name || ''} ${result.worktree_dir || ''}\n\nRoot status:\n${result.root_status || '(dirty)'}\n\nNext: inspect/commit/stash root changes, then retry merge.`);
          } else {
            window.alert(`Merge conflicts; root was reset cleanly.\n\nRoot: ${result.root_dir || ''}\nSource: ${result.worktree_name || ''} ${result.worktree_dir || ''}\n\nConflicts:\n${(result.conflicts || []).join('\n')}\n\nNext: consider promoting the worktree or ask the LLM for a recovery plan.`);
          }
        } else {
          const cleanup = data.cleanup || {};
          if (cleanup.removed) {
            const session = activeSession();
            if (session && session.worktreeDir === dir) session.worktreeRemoved = true;
            window.alert(`Merged back to root and removed the worktree: ${result.root_dir || ''}\nSource: ${result.worktree_name || ''}\nResult: ${mergeOutcome}${result.snapshot_commit ? `\nRecovery snapshot: ${result.snapshot_commit.slice(0, 12)}` : ''}`);
          } else if (Array.isArray(cleanup.in_use) && cleanup.in_use.length > 0) {
            const remove = window.confirm(`Merge succeeded, but the worktree is used by ${cleanup.in_use.length} session(s). Remove it anyway?`);
            if (remove) {
              const deleteRes = await fetch(`${UI_PREFIX}/v1/worktrees?dir=${encodeURIComponent(dir)}&force=1`, { method: 'DELETE', headers: authHeaders() });
              if (!deleteRes.ok) throw await (worktreeApp.normalizeError ? worktreeApp.normalizeError(deleteRes) : deleteRes.text());
              const session = activeSession();
              if (session && session.worktreeDir === dir) session.worktreeRemoved = true;
              window.alert(`Merged back to root and removed the worktree.\nResult: ${mergeOutcome}${result.snapshot_commit ? `\nRecovery snapshot: ${result.snapshot_commit.slice(0, 12)}` : ''}`);
            } else {
              window.alert(`Merged back to root; the worktree was kept.\nResult: ${mergeOutcome}`);
            }
          } else {
            window.alert(`Merged back to root; the worktree was kept.\nResult: ${mergeOutcome}`);
          }
          await loadWorktrees();
          renderWorktreeChip();
        }
      } else if (value === 'promote') {
        const branch = window.prompt('Branch name:', '');
        if (!branch) return;
        const res = await fetch(`${UI_PREFIX}/v1/worktrees/promote`, { method: 'POST', headers: authHeaders(), body: JSON.stringify({ dir, branch }) });
        if (!res.ok && res.status !== 409) throw await (worktreeApp.normalizeError ? worktreeApp.normalizeError(res) : res.text());
        const data = await res.json().catch(() => ({}));
        if (res.status === 409) {
          const result = data.result || {};
          window.alert(`Promote not attempted: root checkout is dirty.\n\nRoot: ${result.root_dir || ''}\nBranch: ${branch}\n\nRoot status:\n${result.root_status || '(dirty)'}\n\nNext: inspect/commit/stash root changes, then retry promote.`);
        } else {
          const result = data.result || {};
          window.alert(`Promoted to branch ${result.branch || branch}.\n\nChecked out in root: ${result.root_dir || ''}\nSource worktree: ${result.worktree_dir || dir}\n${result.applied ? 'Dirty worktree changes are staged/uncommitted in root.' : 'No dirty changes were applied.'}\n\nNext: open a shell, run git status, commit, and push.`);
        }
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
    if (!worktreesEnabled) return;
    const session = activeSession();
    const isDraft = state.draftSessionActive;
    const activeDir = !isDraft ? (session?.worktreeDir || '') : '';
    if (!isDraft && activeDir) {
      await openDiffActions(activeDir);
      return;
    }
    const rows = await loadWorktrees();
    closeMenu();
    menu = document.createElement('div');
    menu.className = 'chip-popover chip-popover-runtime worktree-popover';
    menu.setAttribute('role', isDraft ? 'listbox' : 'menu');
    const addRow = (labelText, metaText, onClick, selected = false, disabled = false) => {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'chip-popover-item worktree-option';
      btn.setAttribute('role', isDraft ? 'option' : 'menuitem');
      if (isDraft && selected) btn.setAttribute('aria-selected', 'true');
      btn.disabled = disabled;
      const label = document.createElement('span');
      label.className = 'chip-popover-item-label';
      label.textContent = labelText;
      btn.appendChild(label);
      if (metaText) {
        const meta = document.createElement('span');
        meta.className = 'chip-popover-item-meta';
        meta.textContent = metaText;
        btn.appendChild(meta);
      }
      btn.addEventListener('click', onClick);
      menu.appendChild(btn);
    };
    const selectedDir = isDraft ? (state.selectedWorktreeDir || '') : activeDir;
    addRow('root checkout', isDraft ? '' : 'current session', () => {
      if (isDraft) chooseWorktree(null); else closeMenu();
    }, !selectedDir, !isDraft);
    rows.filter((r) => !r.root).forEach((row) => {
      const ref = row.branch || (row.head_sha ? `detached@${row.head_sha.slice(0, 8)}` : 'detached');
      addRow(row.name, `±${row.dirty_files || 0} · ${ref}`, () => {
        if (isDraft) {
          chooseWorktree(row);
        } else {
          closeMenu();
          void openDiffActions(row.dir);
        }
      }, row.dir === selectedDir);
    });
    if (isDraft) {
      addRow(loading ? 'creating…' : '+ new worktree…', '', () => { void createWorktree(); });
    }
    document.body.appendChild(menu);
    if (elements.chipPopoverBackdrop) elements.chipPopoverBackdrop.hidden = false;
    if (typeof worktreeApp.positionChipPopover === 'function') {
      worktreeApp.positionChipPopover(elements.chipWorktreeTrigger, menu, { mobileSheet: true });
    }
    elements.chipWorktreeTrigger.setAttribute('aria-expanded', 'true');
  };

  if (worktreesEnabled && elements.chipWorktreeTrigger) {
    elements.chipWorktreeTrigger.addEventListener('click', (event) => {
      event.preventDefault();
      if (menu) closeMenu(); else void openMenu();
    });
  }
  document.addEventListener('click', (event) => {
    if (!menu) return;
    if (elements.chipWorktreeTrigger?.contains?.(event.target) || menu.contains(event.target)) return;
    closeMenu();
  });
  elements.chipPopoverBackdrop?.addEventListener('click', closeMenu);
  document.addEventListener('keydown', (event) => {
    if (event.key !== 'Escape' || !menu) return;
    closeMenu();
    elements.chipWorktreeTrigger?.focus?.();
  });

  const repositionMenu = () => {
    if (!menu || typeof worktreeApp.positionChipPopover !== 'function') return;
    worktreeApp.positionChipPopover(elements.chipWorktreeTrigger, menu, { mobileSheet: true });
  };
  window.addEventListener('resize', repositionMenu);
  window.addEventListener('orientationchange', repositionMenu);
  if (window.visualViewport) {
    window.visualViewport.addEventListener('resize', repositionMenu);
    window.visualViewport.addEventListener('scroll', repositionMenu);
  }

  Object.assign(worktreeApp, {
    loadWorktrees,
    renderWorktreeChip
  });

  renderWorktreeChip();
  if (worktreesEnabled) setInterval(renderWorktreeChip, 1000);
})();
