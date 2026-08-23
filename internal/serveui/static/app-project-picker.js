(() => {
'use strict';

const app = window.TermLLMApp;
const createEl = app.createEl;
const { UI_PREFIX, elements, state } = app;

const openProjectPicker = (options = {}) => {
  document.getElementById('projectModal')?.remove();
  const backdrop = createEl('div', 'project-modal-backdrop'); backdrop.id = 'projectModal';
  const dialog = createEl('div', 'project-modal'); dialog.setAttribute('role', 'dialog'); dialog.setAttribute('aria-modal', 'true'); dialog.setAttribute('aria-labelledby', 'projectModalTitle');
  dialog.appendChild(createEl('div', 'project-modal-handle'));

  const header = createEl('div', 'project-modal-header');
  const heading = createEl('div', 'project-modal-heading');
  const title = createEl('h2', '', 'Add project'); title.id = 'projectModalTitle';
  const explanation = createEl('p', '', 'Choose a folder on this server, or paste its absolute path.');
  heading.append(title, explanation);
  const closeButton = createEl('button', 'project-modal-close', '×'); closeButton.type = 'button'; closeButton.setAttribute('aria-label', 'Close add project');
  header.append(heading, closeButton);

  const fields = createEl('div', 'project-modal-fields');
  const pathField = createEl('div', 'project-field');
  const pathLabel = createEl('label', 'project-field-label', 'Folder on server'); pathLabel.setAttribute('for', 'projectPathInput');
  const pathControl = createEl('div', 'project-path-control');
  const path = createEl('input', 'project-input project-path-input'); path.id = 'projectPathInput'; path.placeholder = '/absolute/server/path'; path.setAttribute('aria-label', 'Absolute server path'); path.setAttribute('autocorrect', 'off'); path.autocapitalize = 'none'; path.autocomplete = 'off'; path.spellcheck = false;
  const browse = createEl('button', 'project-browse-button', 'Browse'); browse.type = 'button'; browse.setAttribute('aria-expanded', 'false'); browse.setAttribute('aria-controls', 'projectDirectoryBrowser');
  pathControl.append(path, browse);

  const browser = createEl('section', 'project-directory-browser'); browser.id = 'projectDirectoryBrowser'; browser.hidden = true; browser.setAttribute('aria-label', 'Server folders');
  const browserToolbar = createEl('div', 'project-browser-toolbar');
  const upButton = createEl('button', 'project-browser-icon-button', '↑'); upButton.type = 'button'; upButton.title = 'Parent folder'; upButton.setAttribute('aria-label', 'Go to parent folder');
  const homeButton = createEl('button', 'project-browser-icon-button', '⌂'); homeButton.type = 'button'; homeButton.title = 'Home folder'; homeButton.setAttribute('aria-label', 'Go to home folder');
  const breadcrumbs = createEl('nav', 'project-browser-breadcrumbs'); breadcrumbs.setAttribute('aria-label', 'Folder path');
  const hiddenLabel = createEl('label', 'project-browser-hidden');
  const showHidden = createEl('input'); showHidden.type = 'checkbox';
  hiddenLabel.append(showHidden, createEl('span', '', 'Hidden'));
  browserToolbar.append(upButton, homeButton, breadcrumbs, hiddenLabel);
  const browserList = createEl('div', 'project-browser-list'); browserList.setAttribute('role', 'listbox'); browserList.setAttribute('aria-label', 'Folders');
  const browserFooter = createEl('div', 'project-browser-footer');
  const browserStatus = createEl('div', 'project-browser-status'); browserStatus.setAttribute('aria-live', 'polite');
  const useFolder = createEl('button', 'btn project-use-folder', 'Select folder'); useFolder.type = 'button'; useFolder.disabled = true;
  browserFooter.append(browserStatus, useFolder);
  browser.append(browserToolbar, browserList, browserFooter);
  pathField.append(pathLabel, pathControl, browser);

  const nameField = createEl('div', 'project-field');
  const nameLabelRow = createEl('div', 'project-field-label-row');
  const nameLabel = createEl('label', 'project-field-label', 'Display name'); nameLabel.setAttribute('for', 'projectNameInput');
  nameLabelRow.append(nameLabel, createEl('span', 'project-field-optional', 'Optional'));
  const name = createEl('input', 'project-input'); name.id = 'projectNameInput'; name.placeholder = 'Defaults to the folder name'; name.setAttribute('aria-label', 'Project display name');
  const nameHint = createEl('div', 'project-field-hint', 'Use a short name that is easy to spot in the sidebar.');
  nameField.append(nameLabelRow, name, nameHint);
  fields.append(pathField, nameField);

  const summary = createEl('div', 'project-resolution-summary'); summary.hidden = true; summary.setAttribute('aria-live', 'polite');
  const status = createEl('div', 'project-modal-status'); status.setAttribute('aria-live', 'polite');
  const footer = createEl('div', 'project-modal-footer');
  const actions = createEl('div', 'project-modal-actions');
  const cancel = createEl('button', 'btn', 'Cancel'); cancel.type = 'button';
  const submit = createEl('button', 'btn primary', 'Preview'); submit.type = 'button';
  actions.append(cancel, submit); footer.append(status, actions);
  dialog.append(header, fields, summary, footer); backdrop.appendChild(dialog); document.body.appendChild(backdrop);

  let preview = null;
  let currentDirectory = '';
  let currentParent = '';
  let currentHome = '';
  let directorySeq = 0;

  const setStatus = (message = '', kind = '') => {
    status.textContent = message;
    status.classList.toggle('is-error', kind === 'error');
    status.classList.toggle('is-muted', kind === 'muted');
    if (kind === 'error') status.setAttribute('role', 'alert');
    else status.removeAttribute('role');
  };
  const invalidatePreview = () => {
    if (!preview) return;
    preview = null;
    submit.textContent = 'Preview';
    summary.hidden = true;
    summary.replaceChildren();
    setStatus('Details changed — preview the folder again.', 'muted');
  };
  const setPath = (value) => {
    if (path.value === value) return;
    path.value = value;
    invalidatePreview();
  };
  name.addEventListener('input', invalidatePreview);
  path.addEventListener('input', invalidatePreview);

  const returnFocus = options.returnFocus || document.activeElement;
  const close = options.activateDialog(backdrop, dialog, returnFocus);
  const closeBrowser = () => {
    browser.hidden = true;
    browse.textContent = 'Browse';
    browse.setAttribute('aria-expanded', 'false');
  };
  closeButton.addEventListener('click', close);
  cancel.addEventListener('click', close);

  const renderBrowserLoading = () => {
    browserList.setAttribute('aria-busy', 'true');
    browserList.replaceChildren(...Array.from({ length: 5 }, () => {
      const row = createEl('div', 'project-browser-skeleton');
      row.append(createEl('span'), createEl('span'));
      return row;
    }));
    browserStatus.textContent = 'Loading folders…';
    useFolder.disabled = true;
  };

  const focusBrowserRow = (rows, index) => {
    if (!rows.length) return;
    const next = Math.max(0, Math.min(rows.length - 1, index));
    rows.forEach((row, rowIndex) => { row.tabIndex = rowIndex === next ? 0 : -1; });
    rows[next].focus?.();
  };

  const renderBreadcrumbs = (items, loadDirectory) => {
    const crumbs = (Array.isArray(items) ? items : []).map((item, index, all) => {
      const button = createEl('button', '', String(item?.label || item?.path || ''));
      button.type = 'button'; button.title = String(item?.path || '');
      if (index === all.length - 1) button.setAttribute('aria-current', 'page');
      button.addEventListener('click', () => { void loadDirectory(String(item?.path || '')); });
      return button;
    });
    breadcrumbs.replaceChildren(...crumbs);
    breadcrumbs.scrollLeft = breadcrumbs.scrollWidth;
  };

  const loadDirectory = async (target = '') => {
    const seq = ++directorySeq;
    browser.hidden = false; browse.textContent = 'Hide browser'; browse.setAttribute('aria-expanded', 'true');
    renderBrowserLoading();
    const params = new URLSearchParams();
    if (target) params.set('path', target);
    if (showHidden.checked) params.set('show_hidden', '1');
    const query = params.toString();
    try {
      const response = await app.apiFetch(`${UI_PREFIX}/v1/project-directories${query ? `?${query}` : ''}`, { headers: app.requestHeaders('') });
      const payload = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(payload?.error?.message || 'Could not load this folder');
      if (seq !== directorySeq || !document.getElementById('projectModal')) return;
      currentDirectory = String(payload.path || '');
      currentParent = String(payload.parent || '');
      currentHome = String(payload.home || currentHome || '');
      setPath(currentDirectory);
      upButton.disabled = !currentParent;
      homeButton.disabled = Boolean(currentHome && currentDirectory === currentHome);
      useFolder.disabled = !currentDirectory;
      renderBreadcrumbs(payload.breadcrumbs, loadDirectory);
      browserList.removeAttribute('aria-busy');
      const entries = Array.isArray(payload.entries) ? payload.entries : [];
      if (!entries.length) {
        const empty = createEl('div', 'project-browser-empty');
        empty.append(createEl('span', 'project-browser-empty-icon', '◇'), createEl('strong', '', 'No subfolders here'), createEl('span', '', 'You can still select this folder.'));
        browserList.replaceChildren(empty);
      } else {
        const rows = entries.map((entry, index) => {
          const row = createEl('button', 'project-browser-row'); row.type = 'button'; row.setAttribute('role', 'option'); row.setAttribute('aria-selected', 'false'); row.tabIndex = index === 0 ? 0 : -1;
          const folderIcon = createEl('span', 'project-browser-folder-icon'); folderIcon.setAttribute('aria-hidden', 'true'); folderIcon.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><path d="M3.5 7.5h6l2-2h3.5c1.1 0 2 .9 2 2v.5h3.5v9.5c0 1.1-.9 2-2 2h-15v-12Z"/><path d="M3.5 8h16.8"/></svg>';
          const rowName = createEl('span', 'project-browser-row-name', String(entry?.name || ''));
          const meta = createEl('span', 'project-browser-row-meta');
          if (entry?.git) meta.appendChild(createEl('span', 'project-browser-badge', 'Git'));
          if (entry?.existing_project_id) meta.appendChild(createEl('span', 'project-browser-badge is-added', 'Added'));
          meta.appendChild(createEl('span', 'project-browser-row-arrow', '›'));
          row.append(folderIcon, rowName, meta);
          row.addEventListener('click', () => { void loadDirectory(String(entry?.path || '')); });
          row.addEventListener('keydown', (event) => {
            const rows = Array.from(browserList.querySelectorAll?.('.project-browser-row') || []);
            const rowIndex = rows.indexOf(row);
            if (event.key === 'ArrowDown') { event.preventDefault?.(); focusBrowserRow(rows, rowIndex + 1); }
            else if (event.key === 'ArrowUp') { event.preventDefault?.(); focusBrowserRow(rows, rowIndex - 1); }
            else if (event.key === 'Home') { event.preventDefault?.(); focusBrowserRow(rows, 0); }
            else if (event.key === 'End') { event.preventDefault?.(); focusBrowserRow(rows, rows.length - 1); }
            else if (event.key === 'ArrowLeft' && currentParent) { event.preventDefault?.(); void loadDirectory(currentParent); }
          });
          return row;
        });
        browserList.replaceChildren(...rows);
      }
      browserStatus.textContent = payload.truncated ? `${entries.length} folders shown · refine by navigating` : `${entries.length} folder${entries.length === 1 ? '' : 's'}`;
    } catch (err) {
      if (seq !== directorySeq) return;
      browserList.removeAttribute('aria-busy');
      const error = createEl('div', 'project-browser-error'); error.setAttribute('role', 'alert');
      error.append(createEl('strong', '', 'Folder unavailable'), createEl('span', '', err.message || 'Could not load this folder'));
      const retry = createEl('button', 'btn', 'Retry'); retry.type = 'button'; retry.addEventListener('click', () => { void loadDirectory(target); }); error.appendChild(retry);
      browserList.replaceChildren(error);
      browserStatus.textContent = '';
      useFolder.disabled = true;
    }
  };

  browse.addEventListener('click', () => {
    if (!browser.hidden) { closeBrowser(); path.focus?.(); return; }
    void loadDirectory(String(path.value || '').trim());
  });
  upButton.addEventListener('click', () => { if (currentParent) void loadDirectory(currentParent); });
  homeButton.addEventListener('click', () => { void loadDirectory(currentHome); });
  showHidden.addEventListener('change', () => { void loadDirectory(currentDirectory); });
  useFolder.addEventListener('click', () => { setPath(currentDirectory); closeBrowser(); path.focus?.(); });
  path.addEventListener('keydown', (event) => { if (event.key === 'Enter') { event.preventDefault?.(); submit.click?.(); } });

  const renderResolution = (payload) => {
    const top = createEl('div', 'project-resolution-top');
    top.append(createEl('strong', '', payload.git ? 'Git repository ready' : 'Folder ready'));
    if (payload.git) top.appendChild(createEl('span', 'project-browser-badge', 'Git root'));
    const canonical = createEl('code', '', String(payload.canonical_dir || ''));
    const note = createEl('span', 'project-resolution-note', payload.git ? 'Conversations will use the repository root.' : 'Conversations will use this folder as their working directory.');
    if (payload.duplicate && payload.project?.archived_at) note.textContent = 'This archived project will be restored.';
    summary.replaceChildren(top, canonical, note); summary.hidden = false;
  };

  submit.addEventListener('click', async () => {
    setStatus(); submit.disabled = true; submit.classList.add('is-loading');
    const previousLabel = submit.textContent;
    if (!preview) submit.textContent = 'Checking…';
    try {
      const body = JSON.stringify({ name: name.value, path: path.value });
      const suffix = preview ? '' : '?dry_run=1';
      const response = await app.apiFetch(`${UI_PREFIX}/v1/projects${suffix}`, { method: 'POST', headers: app.requestHeaders(''), body });
      const payload = await response.json().catch(() => ({}));
      if (!response.ok) {
        if (response.status === 409 && payload.existing_project_id) {
          close(); await options.onExistingProject?.(payload.existing_project_id); return;
        }
        throw new Error(payload?.error?.message || 'Could not add project');
      }
      if (!preview) {
        if (payload.duplicate && payload.existing_project_id && !payload.project?.archived_at) {
          close(); await options.onExistingProject?.(payload.existing_project_id); return;
        }
        preview = payload;
        const restoresArchived = Boolean(payload.duplicate && payload.project?.archived_at);
        renderResolution(payload);
        submit.textContent = restoresArchived ? 'Restore project' : 'Add project';
      } else {
        const project = payload.project;
        close(); await options.onProjectCreated?.(project);
      }
    } catch (err) {
      submit.textContent = previousLabel === 'Checking…' ? 'Preview' : previousLabel;
      setStatus(err.message || 'Could not add project', 'error');
    } finally { submit.disabled = false; submit.classList.remove('is-loading'); }
  });
  path.focus();
};

const openAssignProjectDialog = (conversation, options = {}) => {
  document.getElementById('assignProjectModal')?.remove();
  const backdrop = createEl('div', 'project-modal-backdrop'); backdrop.id = 'assignProjectModal';
  const dialog = createEl('div', 'project-modal project-assign-modal'); dialog.setAttribute('role', 'dialog'); dialog.setAttribute('aria-modal', 'true'); dialog.setAttribute('aria-labelledby', 'assignProjectTitle'); dialog.setAttribute('aria-describedby', 'assignProjectNote');
  dialog.appendChild(createEl('div', 'project-modal-handle'));
  const header = createEl('div', 'project-modal-header');
  const heading = createEl('div', 'project-modal-heading');
  const title = createEl('h2', '', 'Assign project'); title.id = 'assignProjectTitle';
  const subtitle = createEl('p', '', 'Choose where this conversation should appear in the sidebar.');
  const closeButton = createEl('button', 'project-modal-close', '×'); closeButton.type = 'button'; closeButton.setAttribute('aria-label', 'Close assign project');
  heading.append(title, subtitle); header.append(heading, closeButton);
  const fields = createEl('div', 'project-modal-fields');
  const notice = createEl('section', 'project-assign-notice'); notice.id = 'assignProjectNote';
  const noticeIcon = createEl('span', 'project-assign-notice-icon', 'i'); noticeIcon.setAttribute('aria-hidden', 'true');
  const noticeCopy = createEl('div'); noticeCopy.append(createEl('strong', '', 'Grouping only'), createEl('span', '', 'The working directory and worktree stay exactly as they are.'));
  notice.append(noticeIcon, noticeCopy);
  const upgrade = createEl('section', 'project-assign-upgrade'); upgrade.hidden = true;
  const choiceField = createEl('div', 'project-field');
  const choiceLabelRow = createEl('div', 'project-field-label-row'); choiceLabelRow.append(createEl('span', 'project-field-label', 'Choose a project'));
  const choices = createEl('div', 'project-choice-list'); choices.setAttribute('role', 'radiogroup'); choices.setAttribute('aria-label', 'Available projects');
  const eligible = (state.projects || []).filter((project) => project.available && !project.archived_at).sort((a, b) => String(a.name || '').localeCompare(String(b.name || '')));
  const rows = []; let selectedProject = null;
  const selectProject = (project, row) => {
    selectedProject = project; rows.forEach((item) => { const active = item === row; item.classList.toggle('is-selected', active); item.setAttribute('aria-checked', active ? 'true' : 'false'); item.tabIndex = active ? 0 : -1; });
    assign.disabled = false; status.textContent = ''; row.focus?.();
  };
  eligible.forEach((project, index) => {
    const row = createEl('button', 'project-choice'); row.type = 'button'; row._projectID = String(project.id || ''); row.setAttribute('role', 'radio'); row.setAttribute('aria-checked', 'false'); row.tabIndex = index === 0 ? 0 : -1;
    const check = createEl('span', 'project-choice-check'); check.setAttribute('aria-hidden', 'true');
    const content = createEl('span', 'project-choice-content');
    const nameRow = createEl('span', 'project-choice-name-row'); nameRow.append(createEl('strong', 'project-choice-name', project.name || 'Untitled project'));
    if (project.git) nameRow.appendChild(createEl('span', 'project-browser-badge', 'Git'));
    content.appendChild(nameRow);
    if (project.canonical_dir) { const path = createEl('code', 'project-choice-path', project.canonical_dir); path.title = project.canonical_dir; content.appendChild(path); }
    const count = Number(project.conversation_count || 0); content.appendChild(createEl('span', 'project-choice-meta', `${count} conversation${count === 1 ? '' : 's'}`));
    row.append(check, content); row.addEventListener('click', () => selectProject(project, row));
    row.addEventListener('keydown', (event) => { const current = rows.indexOf(row); let next = -1; if (event.key === 'ArrowDown') next = Math.min(rows.length - 1, current + 1); else if (event.key === 'ArrowUp') next = Math.max(0, current - 1); else if (event.key === 'Home') next = 0; else if (event.key === 'End') next = rows.length - 1; if (next >= 0) { event.preventDefault?.(); selectProject(eligible[next], rows[next]); } });
    rows.push(row); choices.appendChild(row);
  });
  if (!rows.length) { const empty = createEl('div', 'project-choice-empty'); empty.append(createEl('strong', '', 'No available projects'), createEl('span', '', 'Add or restore a project before assigning this conversation.')); choices.appendChild(empty); }
  choiceField.append(choiceLabelRow, choices); fields.append(notice, upgrade, choiceField);
  const status = createEl('div', 'project-modal-status'); status.setAttribute('aria-live', 'polite');
  const actions = createEl('div', 'project-modal-actions');
  const cancel = createEl('button', 'btn', 'Cancel'); cancel.type = 'button';
  const assign = createEl('button', 'btn primary', 'Assign project'); assign.type = 'button'; assign.disabled = true;
  const footer = createEl('div', 'project-modal-footer'); actions.append(cancel, assign); footer.append(status, actions); dialog.append(header, fields, footer); backdrop.appendChild(dialog); document.body.appendChild(backdrop);
  const returnFocus = document.activeElement; const close = options.activateDialog(backdrop, dialog, returnFocus);
  const setStatus = (message = '', error = false) => { status.textContent = message; status.classList.toggle('is-error', error); if (error) status.setAttribute('role', 'alert'); else status.removeAttribute('role'); };
  const setRowsDisabled = (disabled) => { rows.forEach((row) => { row.disabled = disabled; }); };
  const submitAssignment = async (body, fallbackProject, trigger, busyLabel) => {
    const originalLabel = trigger.textContent; trigger.disabled = true; trigger.classList.add('is-loading'); trigger.textContent = busyLabel; setRowsDisabled(true); assign.disabled = true; setStatus();
    try {
      const response = await app.apiFetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(conversation.id)}/project`, { method: 'POST', headers: app.requestHeaders(conversation.id), body: JSON.stringify(body) }).catch(() => { throw new Error('Could not assign this conversation. Retry.'); });
      const payload = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(payload?.error?.message || 'This conversation does not match that project.');
      conversation.projectId = String(payload.project_id || fallbackProject?.id || ''); conversation.projectName = String(payload.project_name || fallbackProject?.name || ''); close(); await options.onAssigned?.();
    } catch (err) { setStatus(err?.message || 'Could not assign this conversation. Retry.', true); trigger.disabled = false; trigger.textContent = originalLabel; setRowsDisabled(false); assign.disabled = !selectedProject; }
    finally { trigger.classList.remove('is-loading'); }
  };
  closeButton.addEventListener('click', close); cancel.addEventListener('click', close);
  assign.addEventListener('click', async () => { if (selectedProject) await submitAssignment({ project_id: selectedProject.id }, selectedProject, assign, 'Assigning…'); });
  const loadCandidate = async () => {
    try {
      const response = await app.apiFetch(`${UI_PREFIX}/v1/sessions/${encodeURIComponent(conversation.id)}/project`, { headers: app.requestHeaders(conversation.id) });
      const payload = await response.json().catch(() => ({})); const candidate = payload?.candidate;
      if (!response.ok || !candidate || !document.getElementById('assignProjectModal')) return;
      const matchingRow = rows.find((row) => row._projectID === String(candidate.existing_project_id || ''));
      if (matchingRow && !candidate.existing_archived) {
        matchingRow.classList.add('is-recommended'); matchingRow.querySelector('.project-choice-name-row')?.appendChild(createEl('span', 'project-choice-recommended', 'Current folder'));
        const matchingProject = eligible.find((project) => String(project.id) === matchingRow._projectID); if (matchingProject) selectProject(matchingProject, matchingRow);
        return;
      }
      upgrade.hidden = false; choiceLabelRow.children[0].textContent = 'Or choose an existing project';
      const top = createEl('div', 'project-assign-upgrade-top'); top.append(createEl('strong', '', candidate.existing_archived ? 'Restore project from current folder' : 'Create project from current folder'), createEl('span', 'project-choice-recommended', 'Recommended'));
      const candidatePath = createEl('code', 'project-manage-path', candidate.canonical_dir || '');
      const upgradeHint = createEl('span', 'project-field-hint', candidate.existing_archived ? 'Restore this archived project and assign the conversation.' : 'Turn the conversation’s working folder into a project and assign it in one step.');
      const controls = createEl('div', 'project-assign-upgrade-controls');
      const candidateName = createEl('input', 'project-input'); candidateName.value = candidate.existing_name || candidate.default_name || ''; candidateName.setAttribute('aria-label', 'New project display name');
      const createButton = createEl('button', 'btn primary', candidate.existing_archived ? 'Restore & assign' : 'Create & assign'); createButton.type = 'button';
      createButton.addEventListener('click', async () => { await submitAssignment({ create_from_workspace: true, name: candidateName.value }, { name: candidateName.value }, createButton, candidate.existing_archived ? 'Restoring…' : 'Creating…'); });
      controls.append(candidateName, createButton); upgrade.replaceChildren(top, candidatePath, upgradeHint, controls);
    } catch (_) {}
  };
  void loadCandidate(); rows[0]?.focus?.();
};

app.openAssignProjectDialog = openAssignProjectDialog;
app.openProjectPicker = openProjectPicker;
})();
