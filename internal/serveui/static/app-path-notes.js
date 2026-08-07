(() => {
'use strict';

const app = window.TermLLMApp;
if (!app) return;
const { state } = app;
state.pendingBranchDraft ??= null;

const elements = app.elements || {};
Object.assign(elements, {
  branchContextModal: document.getElementById('branchContextModal'),
  branchContextCloseBtn: document.getElementById('branchContextCloseBtn'),
  branchContextFocusForm: document.getElementById('branchContextFocusForm'),
  branchContextFocusInput: document.getElementById('branchContextFocusInput'),
  branchContextFocusBackBtn: document.getElementById('branchContextFocusBackBtn'),
});

const createPathNoteNode = (message) => {
  const article = document.createElement('article');
  article.className = 'message path-note';
  article.dataset.messageId = message.id;
  const details = document.createElement('details');
  const summary = document.createElement('summary');
  const provenance = message.provenance || {};
  const fileCount = (Array.isArray(provenance.read_files) ? provenance.read_files.length : 0)
    + (Array.isArray(provenance.modified_files) ? provenance.modified_files.length : 0);
  summary.textContent = `Notes from an earlier path · not authoritative${fileCount ? ` · ${fileCount} file${fileCount === 1 ? '' : 's'}` : ''}`;
  const body = document.createElement('div');
  body.className = 'message-body markdown-body path-note-body';
  app.renderAssistantMarkdown?.(body, message.content || '');
  details.append(summary, body);
  article.appendChild(details);
  return article;
};

const closeBranchContextModal = () => {
  state.pendingBranchDraft = null;
  if (elements.branchContextModal) elements.branchContextModal.hidden = true;
  if (elements.branchContextFocusForm) elements.branchContextFocusForm.hidden = true;
  elements.branchContextModal?.querySelector?.('.branch-context-choices')?.removeAttribute?.('hidden');
};

const openBranchContextChooser = (draft) => {
  state.pendingBranchDraft = draft;
  if (draft?.hasLaterConversation === false) {
    commitPendingBranchContext('clean');
    return;
  }
  if (elements.branchContextFocusForm) elements.branchContextFocusForm.hidden = true;
  elements.branchContextModal?.querySelector?.('.branch-context-choices')?.removeAttribute?.('hidden');
  if (elements.branchContextFocusInput) elements.branchContextFocusInput.value = '';
  if (elements.branchContextModal) elements.branchContextModal.hidden = false;
  elements.branchContextModal?.querySelector?.('[data-branch-context="clean"]')?.focus?.();
};

const commitPendingBranchContext = (mode, focus = '') => {
  const draft = state.pendingBranchDraft;
  const session = app.getActiveSession?.();
  if (!draft || !session || draft.sourceSessionId !== session.id) {
    closeBranchContextModal();
    return false;
  }
  const normalizedMode = mode === 'focused' ? 'focused' : (mode === 'notes' ? 'notes' : 'clean');
  const normalizedFocus = normalizedMode === 'focused' ? String(focus || '').trim() : '';
  state.pendingBranchDraft = null;
  if (elements.branchContextModal) elements.branchContextModal.hidden = true;
  void app.createConversationBranch?.(draft, normalizedMode, normalizedFocus);
  return true;
};

if (elements.branchContextCloseBtn) elements.branchContextCloseBtn.addEventListener('click', closeBranchContextModal);
elements.branchContextModal?.querySelectorAll?.('[data-branch-context]')?.forEach((button) => {
  button.addEventListener('click', () => {
    const mode = button.dataset.branchContext;
    if (mode === 'focused') {
      elements.branchContextModal?.querySelector?.('.branch-context-choices')?.setAttribute?.('hidden', '');
      if (elements.branchContextFocusForm) elements.branchContextFocusForm.hidden = false;
      elements.branchContextFocusInput?.focus?.();
      return;
    }
    commitPendingBranchContext(mode);
  });
});
if (elements.branchContextFocusBackBtn) elements.branchContextFocusBackBtn.addEventListener('click', () => {
  if (elements.branchContextFocusForm) elements.branchContextFocusForm.hidden = true;
  elements.branchContextModal?.querySelector?.('.branch-context-choices')?.removeAttribute?.('hidden');
  elements.branchContextModal?.querySelector?.('[data-branch-context="focused"]')?.focus?.();
});
if (elements.branchContextFocusForm) elements.branchContextFocusForm.addEventListener('submit', (event) => {
  event.preventDefault();
  const focus = String(elements.branchContextFocusInput?.value || '').trim();
  if (!focus) {
    elements.branchContextFocusInput?.focus?.();
    return;
  }
  commitPendingBranchContext('focused', focus);
});
if (elements.branchContextModal) elements.branchContextModal.addEventListener('click', (event) => {
  if (event.target === elements.branchContextModal) closeBranchContextModal();
});
document.addEventListener('keydown', (event) => {
  if (event.key === 'Escape' && elements.branchContextModal && !elements.branchContextModal.hidden) closeBranchContextModal();
});

Object.assign(app, { createPathNoteNode, openBranchContextChooser, commitPendingBranchContext, closeBranchContextModal });
})();
