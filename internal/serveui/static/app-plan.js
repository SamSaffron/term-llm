(() => {
'use strict';

const app = window.TermLLMApp || (window.TermLLMApp = {});
const { state, elements } = app;
if (!state || !elements) return;

const PLAN_MOBILE_QUERY = '(max-width: 767px)';
const PLAN_STATUSES = new Set(['pending', 'in_progress', 'completed']);
let planReturnFocus = null;
let lastMobileMode = false;

const isPlanMobileViewport = () => {
  try {
    return typeof window.matchMedia === 'function' && window.matchMedia(PLAN_MOBILE_QUERY).matches;
  } catch {
    return false;
  }
};
lastMobileMode = isPlanMobileViewport();

const setHidden = (element, hidden) => {
  if (!element) return;
  element.hidden = Boolean(hidden);
  if (hidden) element.setAttribute?.('hidden', '');
  else element.removeAttribute?.('hidden');
};

const setPlanBackgroundInert = (inert) => {
  if (!elements.appShell) return;
  if (inert) elements.appShell.setAttribute?.('inert', '');
  else elements.appShell.removeAttribute?.('inert');
};

const planSummary = (plan) => {
  const steps = Array.isArray(plan?.steps) ? plan.steps : [];
  const completed = steps.filter((step) => step.status === 'completed').length;
  const complete = steps.length > 0 && completed === steps.length;
  const activeIndex = steps.findIndex((step) => step.status === 'in_progress');
  const nextIndex = steps.findIndex((step) => step.status !== 'completed');
  let position = 0;
  if (steps.length > 0) {
    if (complete) position = steps.length;
    else position = (activeIndex >= 0 ? activeIndex : Math.max(0, nextIndex)) + 1;
  }
  return {
    completed,
    total: steps.length,
    complete,
    position,
    activeStep: activeIndex >= 0 ? steps[activeIndex].step : ''
  };
};

const normalizeCurrentPlan = (value) => {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null;
  const version = Number(value.version);
  if (!Number.isSafeInteger(version) || version <= 0 || !Array.isArray(value.steps) || value.steps.length === 0 || value.steps.length > 20) {
    return null;
  }
  if (Object.prototype.hasOwnProperty.call(value, 'explanation') && typeof value.explanation !== 'string') return null;
  const explanation = String(value.explanation || '').trim();
  if ([...explanation].length > 500) return null;

  const seen = new Set();
  let activeCount = 0;
  const steps = [];
  for (const candidate of value.steps) {
    if (!candidate || typeof candidate !== 'object' || Array.isArray(candidate)) return null;
    const step = typeof candidate.step === 'string' ? candidate.step.trim() : '';
    const status = typeof candidate.status === 'string' ? candidate.status : '';
    if (!step || [...step].length > 240 || !PLAN_STATUSES.has(status)) return null;
    if (status === 'in_progress') activeCount += 1;
    const normalizedText = step.toLowerCase().replace(/\s+/g, ' ');
    if (seen.has(normalizedText)) return null;
    seen.add(normalizedText);
    steps.push({ step, status });
  }
  if (activeCount > 1) return null;
  return { version, steps, ...(explanation ? { explanation } : {}) };
};

const normalizeCurrentPlanSummary = (value) => {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null;
  const version = Number(value.version);
  const total = Number(value.step_count);
  const completed = Number(value.completed_steps);
  const position = Number(value.position);
  const status = String(value.state || '');
  if (!Number.isSafeInteger(version) || version <= 0
      || !Number.isSafeInteger(total) || total <= 0
      || !Number.isSafeInteger(completed) || completed < 0 || completed > total
      || !Number.isSafeInteger(position) || position <= 0 || position > total
      || !PLAN_STATUSES.has(status)) return null;
  const complete = status === 'completed';
  if (complete && (completed !== total || position !== total)) return null;
  return { version, total, completed, position, complete, state: status };
};

const planSummaryFromPlan = (plan) => {
  const summary = planSummary(plan);
  return {
    version: Number(plan?.version) || 0,
    total: summary.total,
    completed: summary.completed,
    position: summary.position,
    complete: summary.complete,
    state: summary.complete ? 'completed' : ((plan?.steps || []).some((step) => step.status === 'in_progress') ? 'in_progress' : 'pending'),
  };
};

const markerForStatus = (status) => {
  if (status === 'completed') return '✓';
  if (status === 'in_progress') return '●';
  return '○';
};

const labelForStatus = (status) => {
  if (status === 'completed') return 'Completed';
  if (status === 'in_progress') return 'In progress';
  return 'Pending';
};

const renderPlanChecklist = (container, plan) => {
  if (!container) return;
  const scrollContainer = container.parentNode;
  const previousScrollTop = Number(scrollContainer?.scrollTop || 0);
  const fragment = document.createDocumentFragment?.() || document.createElement('div');
  (plan?.steps || []).forEach((step) => {
    const row = document.createElement('div');
    row.className = `current-plan-step current-plan-step-${step.status}`;

    const marker = document.createElement('span');
    marker.className = 'current-plan-step-marker';
    marker.textContent = markerForStatus(step.status);
    marker.setAttribute('aria-hidden', 'true');

    const content = document.createElement('div');
    content.className = 'current-plan-step-content';
    const text = document.createElement('div');
    text.className = 'current-plan-step-text';
    text.textContent = step.step;
    const status = document.createElement('div');
    status.className = 'current-plan-step-state';
    status.textContent = labelForStatus(step.status);

    content.appendChild(text);
    content.appendChild(status);
    row.appendChild(marker);
    row.appendChild(content);
    fragment.appendChild(row);
  });
  container.replaceChildren(fragment);
  if (scrollContainer) scrollContainer.scrollTop = previousScrollTop;
};

const renderPlanSurface = (progressElement, explanationElement, checklistElement, plan) => {
  const summary = planSummary(plan);
  if (progressElement) progressElement.textContent = `${summary.position}/${summary.total}`;
  if (explanationElement) {
    const explanation = String(plan?.explanation || '').trim();
    explanationElement.textContent = explanation;
    setHidden(explanationElement, !explanation);
  }
  renderPlanChecklist(checklistElement, plan);
};

const currentPlanIsVisible = () => Boolean(
  (state.currentPlan || state.currentPlanSummary)
  && state.currentPlanSessionId
  && state.currentPlanSessionId === state.activeSessionId
  && !state.draftSessionActive
);

const sizePlanSheet = () => {
  if (!state.currentPlanOpen || !isPlanMobileViewport() || !elements.planSheet) return;
  app.syncViewportShell?.();
  const viewport = window.visualViewport;
  const viewportHeight = Number(viewport?.height || window.innerHeight || 0);
  if (viewportHeight <= 0) return;
  const offsetTop = Number(viewport?.offsetTop || 0);
  const layoutHeight = Number(window.innerHeight || viewportHeight);
  const bottomInset = Math.max(0, layoutHeight - (offsetTop + viewportHeight));
  const sheetHeight = Math.max(280, Math.round(viewportHeight * 0.66));
  const availableHeight = Math.max(160, viewportHeight - 16);
  elements.planSheet.style.height = `${Math.min(sheetHeight, availableHeight)}px`;
  elements.planSheet.style.bottom = `${bottomInset}px`;
};

const updateApplicableSurfaceRelationship = () => {
  if (!elements.planToggleBtn) return;
  elements.planToggleBtn.setAttribute('aria-controls', isPlanMobileViewport() ? 'planSheet' : 'planPanel');
};

const showPlanSurfaceForViewport = ({ transferFocus = false } = {}) => {
  const open = Boolean(state.currentPlanOpen && currentPlanIsVisible());
  const mobile = isPlanMobileViewport();
  updateApplicableSurfaceRelationship();
  setPlanBackgroundInert(open && mobile);

  if (mobile) {
    elements.appShell?.classList.remove('plan-open');
    setHidden(elements.planPanel, true);
    elements.planPanel?.classList.remove('open');
    setHidden(elements.planSheetBackdrop, !open);
    setHidden(elements.planSheet, !open);
    elements.planSheet?.classList.toggle('open', open);
    if (open) {
      sizePlanSheet();
      if (transferFocus) elements.planSheetCloseBtn?.focus?.();
    }
  } else {
    setHidden(elements.planSheetBackdrop, true);
    setHidden(elements.planSheet, true);
    elements.planSheet?.classList.remove('open');
    setHidden(elements.planPanel, !open);
    elements.planPanel?.classList.toggle('open', open);
    elements.appShell?.classList.toggle('plan-open', open);
    if (open && transferFocus) elements.planPanelCloseBtn?.focus?.();
  }
  if (!open) elements.appShell?.classList.remove('plan-open');
};

const renderCurrentPlan = () => {
  const visible = currentPlanIsVisible();
  const plan = visible ? state.currentPlan : null;
  const summary = visible
    ? (plan ? planSummaryFromPlan(plan) : state.currentPlanSummary)
    : null;
  if (elements.planToggleBtn) {
    setHidden(elements.planToggleBtn, !summary);
    elements.planToggleBtn.setAttribute('aria-expanded', state.currentPlanOpen && Boolean(plan) && visible ? 'true' : 'false');
    if (summary) {
      if (elements.planToggleWord) {
        elements.planToggleWord.textContent = summary.complete ? 'Done' : 'Plan';
      }
      if (elements.planToggleProgress) {
        elements.planToggleProgress.textContent = `${summary.position}/${summary.total}`;
      }
      const action = state.currentPlanOpen ? 'Close current plan' : 'Open current plan';
      const status = summary.complete
        ? `All ${summary.total} steps complete`
        : `Step ${summary.position} of ${summary.total}, ${summary.completed} of ${summary.total} steps complete`;
      elements.planToggleBtn.setAttribute('aria-label', `${action}. ${status}`);
      elements.planToggleBtn.title = status;
    }
  }
  if (elements.planUnseenDot) setHidden(elements.planUnseenDot, !summary || !state.currentPlanUnseen);

  renderPlanSurface(elements.planPanelProgress, elements.planPanelExplanation, elements.planPanelChecklist, plan);
  renderPlanSurface(elements.planSheetProgress, elements.planSheetExplanation, elements.planSheetChecklist, plan);

  if (!plan && state.currentPlanOpen) state.currentPlanOpen = false;
  showPlanSurfaceForViewport();
};

const announcePlanChange = (text) => {
  if (!elements.planAnnouncement || !text) return;
  elements.planAnnouncement.textContent = '';
  const publish = () => { elements.planAnnouncement.textContent = text; };
  if (typeof window.requestAnimationFrame === 'function') window.requestAnimationFrame(publish);
  else publish();
};

const planSurfaceOwnsFocus = () => {
  const active = document.activeElement;
  if (!active) return false;
  return active === elements.planToggleBtn
    || Boolean(elements.planPanel?.contains?.(active) || elements.planSheet?.contains?.(active));
};

const restoreFocusAfterForcedPlanClose = ({ preferReturnTarget = false } = {}) => {
  const ownsFocus = planSurfaceOwnsFocus();
  setPlanBackgroundInert(false);
  if (!ownsFocus) return;
  const returnTarget = preferReturnTarget && planReturnFocus !== elements.planToggleBtn ? planReturnFocus : null;
  const target = returnTarget?.focus ? returnTarget : elements.mainHeader;
  target?.focus?.();
};

const resetCurrentPlanForSession = (sessionId = '') => {
  restoreFocusAfterForcedPlanClose();
  state.currentPlan = null;
  state.currentPlanSummary = null;
  state.currentPlanSessionId = String(sessionId || '').trim();
  state.currentPlanInitialized = false;
  state.currentPlanUnseen = false;
  state.currentPlanOpen = false;
  planReturnFocus = null;
  renderCurrentPlan();
};

const applyCurrentPlanSummary = (sessionId, value) => {
  const owner = String(sessionId || '').trim();
  if (!owner || owner !== String(state.activeSessionId || '').trim() || state.draftSessionActive) return false;
  if (value === null) {
    restoreFocusAfterForcedPlanClose({ preferReturnTarget: true });
    state.currentPlan = null;
    state.currentPlanSummary = null;
    state.currentPlanSessionId = owner;
    state.currentPlanInitialized = true;
    state.currentPlanUnseen = false;
    state.currentPlanOpen = false;
    planReturnFocus = null;
    renderCurrentPlan();
    return true;
  }
  const incoming = normalizeCurrentPlanSummary(value);
  if (!incoming) return false;
  const currentVersion = Math.max(
    Number(state.currentPlan?.version || 0),
    Number(state.currentPlanSummary?.version || 0),
  );
  if (state.currentPlanSessionId === owner && currentVersion > incoming.version) return false;
  state.currentPlanSummary = incoming;
  state.currentPlanSessionId = owner;
  state.currentPlanInitialized = true;
  state.currentPlanUnseen = false;
  renderCurrentPlan();
  return true;
};

const applyCurrentPlanState = (sessionId, response) => {
  const owner = String(sessionId || '').trim();
  if (!owner || owner !== String(state.activeSessionId || '').trim() || state.draftSessionActive) return false;
  if (!response || typeof response !== 'object' || !Object.prototype.hasOwnProperty.call(response, 'current_plan')) return false;

  const wasInitialized = state.currentPlanInitialized && state.currentPlanSessionId === owner;
  const previousPlan = state.currentPlanSessionId === owner ? state.currentPlan : null;
  const previousVersion = state.currentPlanSessionId === owner
    ? Math.max(Number(previousPlan?.version || 0), Number(state.currentPlanSummary?.version || 0))
    : 0;
  if (response.current_plan === null) {
    restoreFocusAfterForcedPlanClose({ preferReturnTarget: true });
    state.currentPlan = null;
    state.currentPlanSummary = null;
    state.currentPlanSessionId = owner;
    state.currentPlanInitialized = true;
    state.currentPlanUnseen = false;
    state.currentPlanOpen = false;
    planReturnFocus = null;
    renderCurrentPlan();
    if (wasInitialized && previousPlan) announcePlanChange('Plan cleared');
    return true;
  }

  const incoming = normalizeCurrentPlan(response.current_plan);
  if (!incoming) return false;
  if (previousPlan && incoming.version <= Number(previousPlan.version || 0)) return false;

  state.currentPlan = incoming;
  state.currentPlanSummary = planSummaryFromPlan(incoming);
  state.currentPlanSessionId = owner;
  state.currentPlanInitialized = true;
  state.currentPlanUnseen = Boolean(wasInitialized && incoming.version > previousVersion && !state.currentPlanOpen);
  renderCurrentPlan();
  if (wasInitialized && incoming.version > previousVersion) {
    const summary = planSummary(incoming);
    announcePlanChange(`Plan updated, ${summary.completed} of ${summary.total} steps complete`);
  }
  return true;
};

const focusPlanSheetAfterComposerBlur = () => {
  const focusSheet = () => {
    sizePlanSheet();
    elements.planSheetCloseBtn?.focus?.();
  };
  if (typeof window.requestAnimationFrame === 'function') {
    window.requestAnimationFrame(() => window.requestAnimationFrame(focusSheet));
  } else {
    focusSheet();
  }
};

const openCurrentPlanSurface = (source = null) => {
  if (!currentPlanIsVisible()) return false;
  if (!state.currentPlan) {
    const owner = String(state.activeSessionId || '').trim();
    Promise.resolve(app.refreshCurrentPlanFromServer?.()).then(() => {
      if (owner === String(state.activeSessionId || '').trim() && state.currentPlan) {
        openCurrentPlanSurface(source);
      }
    }).catch(() => {});
    return true;
  }
  app.closeDiffSidebar?.();
  planReturnFocus = source?.focus ? source : elements.planToggleBtn;
  state.currentPlanOpen = true;
  state.currentPlanUnseen = false;

  if (isPlanMobileViewport()) {
    const active = document.activeElement;
    const composerFocused = active === elements.promptInput || Boolean(active?.closest?.('.composer'));
    if (composerFocused) active.blur?.();
  }

  renderCurrentPlan();
  if (isPlanMobileViewport()) focusPlanSheetAfterComposerBlur();
  return true;
};

const closeCurrentPlanSurface = ({ restoreFocus = true } = {}) => {
  const wasOpen = Boolean(state.currentPlanOpen);
  state.currentPlanOpen = false;
  renderCurrentPlan();
  if (wasOpen && restoreFocus) {
    const target = planReturnFocus?.focus ? planReturnFocus : elements.planToggleBtn;
    target?.focus?.();
  }
  planReturnFocus = null;
  return wasOpen;
};

const focusablePlanSheetElements = () => {
  if (!elements.planSheet?.querySelectorAll) return [];
  return Array.from(elements.planSheet.querySelectorAll('button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'))
    .filter((node) => !node.hidden && node.getAttribute?.('aria-hidden') !== 'true');
};

const handlePlanKeydown = (event) => {
  if (!state.currentPlanOpen) return;
  if (event.key === 'Escape') {
    event.preventDefault?.();
    event.stopImmediatePropagation?.();
    closeCurrentPlanSurface();
    return;
  }
  if (event.key !== 'Tab' || !isPlanMobileViewport()) return;
  const focusable = focusablePlanSheetElements();
  if (focusable.length === 0) {
    event.preventDefault?.();
    elements.planSheet?.focus?.();
    return;
  }
  const first = focusable[0];
  const last = focusable[focusable.length - 1];
  if (!elements.planSheet?.contains?.(document.activeElement)) {
    event.preventDefault?.();
    (event.shiftKey ? last : first).focus?.();
  } else if (event.shiftKey && document.activeElement === first) {
    event.preventDefault?.();
    last.focus?.();
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault?.();
    first.focus?.();
  }
};

const handlePlanViewportChange = () => {
  const mobile = isPlanMobileViewport();
  const changed = mobile !== lastMobileMode;
  lastMobileMode = mobile;
  if (!state.currentPlanOpen) {
    renderCurrentPlan();
    return;
  }
  showPlanSurfaceForViewport({ transferFocus: changed });
};

const planMobileMedia = (() => {
  try {
    return typeof window.matchMedia === 'function' ? window.matchMedia(PLAN_MOBILE_QUERY) : null;
  } catch {
    return null;
  }
})();
if (planMobileMedia) {
  if (typeof planMobileMedia.addEventListener === 'function') planMobileMedia.addEventListener('change', handlePlanViewportChange);
  else if (typeof planMobileMedia.addListener === 'function') planMobileMedia.addListener(handlePlanViewportChange);
}
window.addEventListener?.('resize', handlePlanViewportChange);
window.visualViewport?.addEventListener?.('resize', sizePlanSheet);
window.visualViewport?.addEventListener?.('scroll', sizePlanSheet);
document.addEventListener?.('keydown', handlePlanKeydown);

elements.planToggleBtn?.addEventListener?.('click', () => {
  if (state.currentPlanOpen) closeCurrentPlanSurface();
  else openCurrentPlanSurface(elements.planToggleBtn);
});
elements.planPanelCloseBtn?.addEventListener?.('click', () => closeCurrentPlanSurface());
elements.planSheetCloseBtn?.addEventListener?.('click', () => closeCurrentPlanSurface());
elements.planSheetBackdrop?.addEventListener?.('click', (event) => {
  if (event.target === elements.planSheetBackdrop) closeCurrentPlanSurface();
});

Object.assign(app, {
  PLAN_MOBILE_QUERY,
  planSummary,
  normalizeCurrentPlan,
  normalizeCurrentPlanSummary,
  renderPlanChecklist,
  renderCurrentPlan,
  resetCurrentPlanForSession,
  applyCurrentPlanSummary,
  applyCurrentPlanState,
  openCurrentPlanSurface,
  closeCurrentPlanSurface,
  isPlanMobileViewport,
  handlePlanViewportChange
});

renderCurrentPlan();
})();
