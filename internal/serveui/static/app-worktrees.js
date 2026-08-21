'use strict';

const worktreeApp = window.TermLLMApp || (window.TermLLMApp = {});

(() => {
  const { UI_PREFIX, state, elements } = worktreeApp;
  if (!UI_PREFIX || !state || !elements) return;

  const legacyWorktreesEnabled = window.TERM_LLM_WORKTREES_ENABLED === true;
  let contextProjectID = '';
  let contextReturnFocus = null;
  const activeProject = () => state.projects?.find((project) => project.id === (contextProjectID || activeSession()?.projectId || state.activeProjectId)) || null;
  const worktreeCapabilityEnabled = () => state.capabilitiesRequired
    ? Boolean(state.capabilitiesLoaded && state.worktreesEnabled)
    : legacyWorktreesEnabled;
  const worktreesEnabled = () => worktreeCapabilityEnabled() && (
    state.projectsEnabled ? Boolean(activeProject()?.git && activeProject()?.available) : true
  );
  const worktreeBaseURL = () => state.projectsEnabled && activeProject()?.id
    ? `${UI_PREFIX}/v1/projects/${encodeURIComponent(activeProject().id)}/worktrees`
    : `${UI_PREFIX}/v1/worktrees`;
  let loading = false;
  let menu = null;

  const worktreeErrorMessage = (error, fallback = 'Worktree action failed') => {
    switch (String(error?.code || '')) {
      case 'project_required': return 'Choose a project before selecting a worktree.';
      case 'worktrees_unavailable': return 'Worktrees are unavailable for this project.';
      case 'project_not_found': return 'This project no longer exists.';
      case 'project_archived': return 'Restore this project before creating a worktree.';
      case 'project_unavailable': return error?.message || 'This project is unavailable.';
      case 'projects_disabled': return 'Project mode was disabled; reload the available workspace.';
      default: {
        const message = String(error?.message || '');
        return message && !/^Request failed \(\d+\)\.?$/.test(message) ? message : fallback;
      }
    }
  };

  const recoverWorktreeError = (error) => {
    const code = String(error?.code || '');
    if (code === 'projects_disabled') void worktreeApp.loadCapabilities?.();
    else if (['project_required', 'worktrees_unavailable', 'project_not_found', 'project_archived', 'project_unavailable'].includes(code)) {
      void worktreeApp.loadProjectSidebar?.({ refreshStatus: code === 'project_unavailable' });
    }
  };

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
    if (!worktreesEnabled()) {
      const project = activeProject();
      const projectUnavailable = Boolean(state.projectsEnabled && project && !project.available);
      const explain = state.projectsEnabled && project
        ? (project.available ? 'Worktrees unavailable — not a Git repository' : (project.unavailable_reason || 'Project unavailable'))
        : '';
      setChipVisible(Boolean(explain));
      if (elements.chipWorktreeLabel && explain) elements.chipWorktreeLabel.textContent = projectUnavailable ? 'Project unavailable · Retry' : 'Worktrees unavailable';
      if (elements.chipWorktreeTrigger) {
        elements.chipWorktreeTrigger.disabled = !projectUnavailable;
        elements.chipWorktreeTrigger.dataset.worktreeRetry = projectUnavailable ? '1' : '';
        elements.chipWorktreeTrigger.title = projectUnavailable ? `${explain} — Retry project status` : explain;
        elements.chipWorktreeTrigger.setAttribute('aria-label', projectUnavailable ? `${explain}. Retry project status` : (explain || 'Worktrees unavailable'));
      }
      return;
    }
    if (elements.chipWorktreeTrigger) {
      elements.chipWorktreeTrigger.disabled = false;
      elements.chipWorktreeTrigger.dataset.worktreeRetry = '';
    }
    setChipVisible(true);
    const session = activeSession();
    const lockedDir = !state.draftSessionActive && session ? (session.worktreeDir || '') : '';
    const draftState = state.projectDrafts?.[state.activeProjectId] || {};
    const draftDir = state.draftSessionActive ? (draftState.worktreeDir || state.selectedWorktreeDir || '') : '';
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
    if (!worktreesEnabled()) return [];
    try {
      const res = await worktreeApp.apiFetch(worktreeBaseURL(), { headers: authHeaders() });
      if (!res.ok) throw await (worktreeApp.normalizeError ? worktreeApp.normalizeError(res) : res.text());
      const data = await res.json();
      state.worktrees = Array.isArray(data.worktrees) ? data.worktrees : [];
      state.worktreesError = '';
      renderWorktreeChip();
      return state.worktrees;
    } catch (err) {
      state.worktrees = [];
      state.worktreesError = worktreeErrorMessage(err, 'Could not load worktrees. Retry.');
      recoverWorktreeError(err);
      renderWorktreeChip();
      return [];
    }
  };

  const closeMenu = (preserveProject = false) => {
    const anchor = contextReturnFocus || elements.chipWorktreeTrigger;
    if (!menu) {
      if (!preserveProject) { contextProjectID = ''; contextReturnFocus = null; }
      return;
    }
    menu.remove();
    menu = null;
    if (!preserveProject) { contextProjectID = ''; contextReturnFocus = null; }
    if (elements.chipPopoverBackdrop) elements.chipPopoverBackdrop.hidden = true;
    anchor?.setAttribute?.('aria-expanded', 'false');
  };

  const chooseWorktree = (row) => {
    state.selectedWorktreeDir = row && !row.root ? row.dir : '';
    state.selectedWorktreeName = row && !row.root ? row.name : '';
    if (state.projectsEnabled && state.activeProjectId) {
      state.projectDrafts[state.activeProjectId] = {
        ...(state.projectDrafts[state.activeProjectId] || {}),
        worktreeDir: state.selectedWorktreeDir,
        worktreeName: state.selectedWorktreeName,
      };
      worktreeApp.persistActiveProjectDraft?.();
    }
    worktreeApp.invalidateMentionCompletions?.();
    renderWorktreeChip();
    closeMenu();
  };

  const openWorktreeSheet = (titleText) => {
    const returnFocus = contextReturnFocus || elements.chipWorktreeTrigger;
    closeMenu(true);
    document.getElementById('worktreeActionSheet')?.remove();
    const backdrop = document.createElement('div');
    backdrop.className = 'project-modal-backdrop';
    backdrop.id = 'worktreeActionSheet';
    const dialog = document.createElement('div');
    dialog.className = 'project-modal worktree-action-sheet';
    dialog.setAttribute('role', 'dialog');
    dialog.setAttribute('aria-modal', 'true');
    dialog.setAttribute('aria-labelledby', 'worktreeSheetTitle');
    const title = document.createElement('h2');
    title.id = 'worktreeSheetTitle';
    title.textContent = titleText;
    const content = document.createElement('div');
    content.className = 'worktree-sheet-content';
    const status = document.createElement('pre');
    status.className = 'worktree-sheet-status';
    status.setAttribute('aria-live', 'polite');
    const close = document.createElement('button');
    close.type = 'button'; close.textContent = 'Close';
    const dismissFallback = () => { backdrop.remove(); contextProjectID = ''; contextReturnFocus = null; returnFocus?.focus?.(); };
    const activatedDismiss = worktreeApp.activateProjectDialog
      ? worktreeApp.activateProjectDialog(backdrop, dialog, returnFocus)
      : dismissFallback;
    const dismiss = () => { contextProjectID = ''; contextReturnFocus = null; activatedDismiss(); };
    close.addEventListener('click', dismiss);
    dialog.append(title, content, status, close);
    backdrop.appendChild(dialog);
    document.body.appendChild(backdrop);
    return { content, status, close: dismiss, dialog };
  };

  const createWorktree = async () => {
    const sheet = openWorktreeSheet('Create worktree');
    const input = document.createElement('input');
    input.className = 'project-input';
    input.placeholder = 'Name (optional)';
    input.setAttribute('aria-label', 'New worktree name');
    const create = document.createElement('button');
    create.type = 'button'; create.textContent = 'Create worktree';
    sheet.content.append(input, create);
    create.addEventListener('click', async () => {
      loading = true; create.disabled = true; sheet.status.textContent = 'Creating worktree…'; renderWorktreeChip();
      try {
        const res = await worktreeApp.apiFetch(worktreeBaseURL(), {
          method: 'POST', headers: authHeaders(), body: JSON.stringify({ name: input.value || '' })
        });
        if (!res.ok) throw await (worktreeApp.normalizeError ? worktreeApp.normalizeError(res) : res.text());
        const data = await res.json();
        await loadWorktrees();
        if (data.worktree) chooseWorktree(data.worktree);
        sheet.status.textContent = `Created ${data.worktree?.name || 'worktree'}`;
        create.remove();
      } catch (err) {
        recoverWorktreeError(err);
        sheet.status.textContent = worktreeErrorMessage(err, 'Failed to create worktree');
      } finally { loading = false; create.disabled = false; renderWorktreeChip(); }
    });
    input.focus();
  };

  const openDiffActions = async (dir) => {
    if (!dir) return;
    const sheet = openWorktreeSheet('Worktree actions');
    const branch = document.createElement('input');
    branch.className = 'project-input'; branch.placeholder = 'Branch name for promote'; branch.setAttribute('aria-label', 'Branch name for promote');
    const actions = document.createElement('div'); actions.className = 'project-modal-actions';
    const actionButton = (label, fn) => {
      const button = document.createElement('button'); button.type = 'button'; button.textContent = label;
      button.addEventListener('click', async () => {
        button.disabled = true; sheet.status.textContent = `${label}…`;
        try { await fn(button); } catch (err) { recoverWorktreeError(err); sheet.status.textContent = worktreeErrorMessage(err); }
        finally { button.disabled = false; }
      });
      actions.appendChild(button); return button;
    };
    actionButton('Show diff', async () => {
      const res = await worktreeApp.apiFetch(`${worktreeBaseURL()}/diff?dir=${encodeURIComponent(dir)}`, { headers: authHeaders() });
      if (!res.ok) throw await (worktreeApp.normalizeError ? worktreeApp.normalizeError(res) : res.text());
      const data = await res.json();
      sheet.status.textContent = (data.diff || 'Worktree is clean.') + (data.truncated ? `\n\n[Diff truncated: ${(data.truncation_reasons || []).join(', ') || 'limit reached'}]` : '');
    });
    actionButton('Merge', async () => {
      const res = await worktreeApp.apiFetch(`${worktreeBaseURL()}/merge`, { method: 'POST', headers: authHeaders(), body: JSON.stringify({ dir }) });
      const data = await res.json().catch(() => ({}));
      if (!res.ok && res.status !== 409) throw await (worktreeApp.normalizeError ? worktreeApp.normalizeError(res) : JSON.stringify(data));
      const result = data.result || {};
      if (res.status === 409) sheet.status.textContent = data.message || (data.error === 'root_dirty' ? 'Merge not attempted: root checkout is dirty.' : 'Merge conflicts; root was reset cleanly.');
      else sheet.status.textContent = `Merge complete: ${result.committed ? 'committed' : (result.applied ? 'staged, uncommitted' : 'no changes to apply')}`;
      await loadWorktrees();
    });
    actionButton('Promote', async () => {
      const branchName = String(branch.value || '').trim();
      if (!branchName) { branch.focus(); throw new Error('Enter a branch name before promoting.'); }
      const res = await worktreeApp.apiFetch(`${worktreeBaseURL()}/promote`, { method: 'POST', headers: authHeaders(), body: JSON.stringify({ dir, branch: branchName }) });
      const data = await res.json().catch(() => ({}));
      if (!res.ok && res.status !== 409) throw await (worktreeApp.normalizeError ? worktreeApp.normalizeError(res) : JSON.stringify(data));
      sheet.status.textContent = res.status === 409 ? (data.message || 'Promote not attempted: root checkout is dirty.') : `Promoted to branch ${data.result?.branch || branchName}.`;
    });
    let removeArmed = false;
    let forceRemove = false;
    actionButton('Remove', async (button) => {
      if (!removeArmed) {
        removeArmed = true;
        button.textContent = 'Confirm remove';
        sheet.status.textContent = 'Remove this managed worktree? This cannot be undone.';
        return;
      }
      const forceParam = forceRemove ? '&force=1' : '';
      const res = await worktreeApp.apiFetch(`${worktreeBaseURL()}?dir=${encodeURIComponent(dir)}${forceParam}`, { method: 'DELETE', headers: authHeaders() });
      if (!res.ok) {
        const payload = await res.json().catch(() => ({}));
        if (res.status === 409 && Array.isArray(payload.in_use) && payload.in_use.length) {
          forceRemove = true;
          button.textContent = 'Force remove';
          const sessions = payload.in_use.map((item) => item.name || (item.number ? `#${item.number}` : item.id)).filter(Boolean);
          sheet.status.textContent = `Worktree is in use by ${sessions.join(', ')}. Force removal leaves those conversations bound to a missing checkout; they will safely fall back to the project root.`;
          return;
        }
        throw {
          status: res.status,
          code: String(payload?.error?.code || ''),
          message: payload?.error?.message || payload?.error || 'Could not remove worktree',
        };
      }
      const session = activeSession();
      if (session && session.worktreeDir === dir) session.worktreeRemoved = true;
      if (state.selectedWorktreeDir === dir) {
        state.selectedWorktreeDir = '';
        state.selectedWorktreeName = '';
        const draftProjectID = String(state.activeProjectId || '');
        if (draftProjectID && state.projectDrafts?.[draftProjectID]) {
          state.projectDrafts[draftProjectID].worktreeDir = '';
          state.projectDrafts[draftProjectID].worktreeName = '';
          worktreeApp.persistActiveProjectDraft?.();
        }
      }
      sheet.status.textContent = forceRemove ? 'Worktree force removed.' : 'Worktree removed.';
      button.remove();
      await loadWorktrees();
    });
    sheet.content.append(branch, actions);
    actions.querySelector('button')?.focus?.();
  };

  const openWorktreeLoadError = () => {
    const projectID = contextProjectID;
    const returnFocus = contextReturnFocus;
    const sheet = openWorktreeSheet('Worktrees unavailable');
    sheet.status.textContent = state.worktreesError || 'Could not load worktrees. Retry.';
    const retry = document.createElement('button');
    retry.type = 'button'; retry.textContent = 'Retry';
    retry.addEventListener('click', async () => {
      retry.disabled = true; sheet.status.textContent = 'Loading worktrees…';
      await loadWorktrees();
      if (state.worktreesError) {
        sheet.status.textContent = state.worktreesError;
        retry.disabled = false;
        return;
      }
      sheet.close();
      if (projectID) await openWorktreeMenuForProject(projectID, returnFocus);
      else await openMenu();
    });
    sheet.content.appendChild(retry);
    retry.focus();
  };

  const openMenu = async () => {
    if (!worktreesEnabled()) return;
    const session = activeSession();
    const projectManagement = Boolean(contextProjectID);
    const isDraft = !projectManagement && state.draftSessionActive;
    const activeDir = !isDraft && !projectManagement ? (session?.worktreeDir || '') : '';
    if (!isDraft && activeDir) {
      await openDiffActions(activeDir);
      return;
    }
    const rows = await loadWorktrees();
    if (state.worktreesError) {
      openWorktreeLoadError();
      return;
    }
    closeMenu(true);
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
    const selectedDir = isDraft
      ? (state.projectDrafts?.[state.activeProjectId]?.worktreeDir || state.selectedWorktreeDir || '')
      : activeDir;
    addRow('root checkout', isDraft ? '' : 'current session', () => {
      if (isDraft) chooseWorktree(null); else closeMenu();
    }, !selectedDir, !isDraft);
    rows.filter((r) => !r.root).forEach((row) => {
      const ref = row.branch || (row.head_sha ? `detached@${row.head_sha.slice(0, 8)}` : 'detached');
      addRow(row.name, `±${row.dirty_files || 0} · ${ref}`, () => {
        if (isDraft) {
          chooseWorktree(row);
        } else {
          closeMenu(true);
          void openDiffActions(row.dir);
        }
      }, row.dir === selectedDir);
    });
    if (isDraft || projectManagement) {
      addRow(loading ? 'creating…' : '+ new worktree…', '', () => { void createWorktree(); });
    }
    document.body.appendChild(menu);
    if (elements.chipPopoverBackdrop) elements.chipPopoverBackdrop.hidden = false;
    const anchor = contextReturnFocus || elements.chipWorktreeTrigger;
    if (typeof worktreeApp.positionChipPopover === 'function') {
      worktreeApp.positionChipPopover(anchor, menu, { mobileSheet: true });
    }
    anchor?.setAttribute?.('aria-expanded', 'true');
    menu.querySelector?.('button:not([disabled])')?.focus?.();
  };

  const openWorktreeMenuForProject = async (projectID, returnFocus = null) => {
    contextProjectID = String(projectID || '');
    contextReturnFocus = returnFocus || null;
    if (!contextProjectID) return;
    await openMenu();
  };

  if (elements.chipWorktreeTrigger) {
    elements.chipWorktreeTrigger.addEventListener('click', (event) => {
      event.preventDefault();
      if (elements.chipWorktreeTrigger.dataset.worktreeRetry === '1') {
        void worktreeApp.loadProjectSidebar?.({ refreshStatus: true });
        return;
      }
      if (menu) closeMenu(); else void openMenu();
    });
  }
  document.addEventListener('click', (event) => {
    if (!menu) return;
    if (elements.chipWorktreeTrigger?.contains?.(event.target) || contextReturnFocus?.contains?.(event.target) || menu.contains(event.target)) return;
    closeMenu();
  });
  elements.chipPopoverBackdrop?.addEventListener('click', closeMenu);
  document.addEventListener('keydown', (event) => {
    if (event.key !== 'Escape' || !menu) return;
    const returnFocus = contextReturnFocus || elements.chipWorktreeTrigger;
    closeMenu();
    returnFocus?.focus?.();
  });

  const repositionMenu = () => {
    if (!menu || typeof worktreeApp.positionChipPopover !== 'function') return;
    worktreeApp.positionChipPopover(contextReturnFocus || elements.chipWorktreeTrigger, menu, { mobileSheet: true });
  };
  window.addEventListener('resize', repositionMenu);
  window.addEventListener('orientationchange', repositionMenu);
  if (window.visualViewport) {
    window.visualViewport.addEventListener('resize', repositionMenu);
    window.visualViewport.addEventListener('scroll', repositionMenu);
  }

  Object.assign(worktreeApp, {
    loadWorktrees,
    renderWorktreeChip,
    openWorktreeMenu: openMenu,
    openWorktreeMenuForProject
  });

  renderWorktreeChip();
  if (legacyWorktreesEnabled || state.projectsEnabled) setInterval(renderWorktreeChip, 1000);
})();
