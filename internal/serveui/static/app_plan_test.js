#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const source = fs.readFileSync(path.join(__dirname, 'app-plan.js'), 'utf8');
let failures = 0;

function fail(name, message) {
  console.error('FAIL:', name, '-', message);
  failures += 1;
}

function pass(name) {
  console.log('PASS:', name);
}

function makeClassList() {
  const values = new Set();
  return {
    add(...names) { names.forEach((name) => values.add(name)); },
    remove(...names) { names.forEach((name) => values.delete(name)); },
    toggle(name, force) {
      const enabled = force === undefined ? !values.has(name) : Boolean(force);
      if (enabled) values.add(name); else values.delete(name);
      return enabled;
    },
    contains(name) { return values.has(name); },
  };
}

function createHarness() {
  let mobile = false;
  const mediaListeners = [];
  const documentListeners = {};
  const viewportListeners = {};
  const document = { activeElement: null };

  function makeNode(tag = 'div') {
    const attrs = new Map();
    const listeners = {};
    const node = {
      tagName: String(tag).toUpperCase(),
      children: [],
      parentNode: null,
      classList: makeClassList(),
      style: {},
      dataset: {},
      hidden: false,
      textContent: '',
      scrollTop: 0,
      listeners,
      appendChild(child) {
        if (!child) return child;
        if (child.tagName === 'FRAGMENT') {
          child.children.slice().forEach((entry) => node.appendChild(entry));
          child.children.length = 0;
          return child;
        }
        if (child.parentNode && child.parentNode !== node) {
          child.parentNode.children = child.parentNode.children.filter((entry) => entry !== child);
        }
        child.parentNode = node;
        node.children.push(child);
        return child;
      },
      replaceChildren(...children) {
        node.children.forEach((child) => { child.parentNode = null; });
        node.children = [];
        children.forEach((child) => node.appendChild(child));
      },
      setAttribute(name, value) { attrs.set(name, String(value)); if (name === 'hidden') node.hidden = true; },
      removeAttribute(name) { attrs.delete(name); if (name === 'hidden') node.hidden = false; },
      getAttribute(name) { return attrs.has(name) ? attrs.get(name) : null; },
      addEventListener(type, listener) { (listeners[type] = listeners[type] || []).push(listener); },
      focus() { document.activeElement = node; node.focusCount = (node.focusCount || 0) + 1; },
      blur() { if (document.activeElement === node) document.activeElement = null; node.blurCount = (node.blurCount || 0) + 1; },
      closest(selector) { return selector === '.composer' && node.inComposer ? node : null; },
      querySelectorAll() { return []; },
      contains(target) {
        if (target === node) return true;
        return node.children.some((child) => child.contains?.(target));
      },
    };
    Object.defineProperty(node, 'className', {
      get() { return node._className || ''; },
      set(value) {
        node._className = String(value || '');
        node.classList = makeClassList();
        node._className.split(/\s+/).filter(Boolean).forEach((name) => node.classList.add(name));
      },
    });
    return node;
  }

  document.createElement = makeNode;
  document.createDocumentFragment = () => makeNode('fragment');
  document.addEventListener = (type, listener) => { (documentListeners[type] = documentListeners[type] || []).push(listener); };

  const elements = {
    appShell: makeNode(),
    mainHeader: makeNode('header'),
    promptInput: makeNode('textarea'),
    planToggleBtn: makeNode('button'),
    planToggleWord: makeNode('span'),
    planToggleProgress: makeNode('span'),
    planUnseenDot: makeNode('span'),
    planPanel: makeNode('aside'),
    planPanelProgress: makeNode('span'),
    planPanelCloseBtn: makeNode('button'),
    planPanelBody: makeNode(),
    planPanelExplanation: makeNode('p'),
    planPanelChecklist: makeNode(),
    planSheet: makeNode('section'),
    planSheetBackdrop: makeNode(),
    planSheetProgress: makeNode('span'),
    planSheetCloseBtn: makeNode('button'),
    planSheetBody: makeNode(),
    planSheetExplanation: makeNode('p'),
    planSheetChecklist: makeNode(),
    planAnnouncement: makeNode(),
  };
  elements.planPanel.appendChild(elements.planPanelCloseBtn);
  elements.planPanel.appendChild(elements.planPanelBody);
  elements.planPanelBody.appendChild(elements.planPanelChecklist);
  elements.planSheet.appendChild(elements.planSheetCloseBtn);
  elements.planSheet.appendChild(elements.planSheetBody);
  elements.planSheetBody.appendChild(elements.planSheetChecklist);
  elements.planSheet.querySelectorAll = () => [elements.planSheetCloseBtn];
  elements.promptInput.inComposer = true;

  const state = {
    activeSessionId: 'session-a',
    draftSessionActive: false,
    currentPlan: null,
    currentPlanSessionId: 'session-a',
    currentPlanInitialized: false,
    currentPlanUnseen: false,
    currentPlanOpen: false,
  };
  let closeDiffCalls = 0;
  const media = {
    get matches() { return mobile; },
    addEventListener(type, listener) { if (type === 'change') mediaListeners.push(listener); },
    addListener(listener) { mediaListeners.push(listener); },
  };
  const visualViewport = {
    height: 600,
    offsetTop: 0,
    addEventListener(type, listener) { (viewportListeners[type] = viewportListeners[type] || []).push(listener); },
  };
  const windowObj = {
    TermLLMApp: {
      state,
      elements,
      closeDiffSidebar() { closeDiffCalls += 1; },
      syncViewportShell() {},
    },
    innerHeight: 600,
    matchMedia: () => media,
    visualViewport,
    requestAnimationFrame(callback) { callback(); return 1; },
    addEventListener() {},
  };
  const context = { window: windowObj, document, console, Set, Array, Number, String, Boolean, Math, Object, JSON };
  context.globalThis = context;
  vm.runInNewContext(source, context, { filename: 'app-plan.js' });

  return {
    app: windowObj.TermLLMApp,
    state,
    elements,
    document,
    documentListeners,
    get closeDiffCalls() { return closeDiffCalls; },
    setMobile(value) {
      mobile = Boolean(value);
      mediaListeners.forEach((listener) => listener({ matches: mobile }));
    },
    setViewportHeight(value) {
      visualViewport.height = Number(value);
      (viewportListeners.resize || []).forEach((listener) => listener());
    },
    click(node) { (node.listeners.click || []).forEach((listener) => listener({ target: node })); },
    keydown(event) { (documentListeners.keydown || []).forEach((listener) => listener(event)); },
  };
}

function snapshot(version, statuses = ['completed', 'in_progress', 'pending']) {
  return {
    version,
    explanation: 'Authoritative explanation',
    steps: statuses.map((status, index) => ({ step: `Step ${index + 1}`, status })),
  };
}

function testPlanSummaryRendersInitialAffordance() {
  const name = 'selected session plan summary renders the final affordance before plan body state';
  const h = createHarness();

  if (!h.app.applyCurrentPlanSummary('session-a', {
    version: 7,
    step_count: 4,
    completed_steps: 2,
    position: 3,
    state: 'in_progress',
  })) return fail(name, 'valid plan summary was not accepted');
  if (h.elements.planToggleBtn.hidden
    || h.elements.planToggleWord.textContent !== 'Plan'
    || h.elements.planToggleProgress.textContent !== '3/4'
    || !h.elements.planToggleBtn.getAttribute('aria-label').includes('Step 3 of 4')) {
    return fail(name, 'initial plan affordance was not complete and accurate');
  }
  if (h.elements.planPanelChecklist.children.length !== 0) return fail(name, 'summary metadata invented full plan steps');

  h.app.applyCurrentPlanSummary('session-a', null);
  if (!h.elements.planToggleBtn.hidden) return fail(name, 'authoritative no-plan summary left the affordance visible');
  pass(name);
}

function testPlanStateAndRendering() {
  const name = 'authoritative plan versions render shared progress and explicit clears';
  const h = createHarness();
  if (!h.elements.planToggleBtn.hidden) return fail(name, 'trigger is visible without a plan');

  if (!h.app.applyCurrentPlanState('session-a', { current_plan: snapshot(2) })) return fail(name, 'initial plan was not accepted');
  if (h.elements.planToggleBtn.hidden
    || h.elements.planToggleWord.textContent !== 'Plan'
    || h.elements.planToggleProgress.textContent !== '2/3'
    || h.elements.planPanelProgress.textContent !== '2/3'
    || h.elements.planSheetProgress.textContent !== '2/3') {
    return fail(name, 'mixed plan did not show the active step position across plan surfaces');
  }
  if (!h.elements.planToggleBtn.getAttribute('aria-label').includes('Step 2 of 3')) return fail(name, 'accessible active-step label is missing');
  if (h.elements.planPanelChecklist.children.length !== 3 || h.elements.planSheetChecklist.children.length !== 3) return fail(name, 'shared checklist was not rendered into both surfaces');
  const markers = h.elements.planPanelChecklist.children.map((row) => row.children[0].textContent).join('');
  if (markers !== '✓●○') return fail(name, `status markers were ${markers}`);
  if (h.state.currentPlanUnseen) return fail(name, 'initial session load was marked unseen');

  h.app.applyCurrentPlanState('session-a', { other_state: true });
  if (h.state.currentPlan.version !== 2) return fail(name, 'omitted field changed current plan');
  h.app.applyCurrentPlanState('session-a', { current_plan: snapshot(1, ['pending']) });
  h.app.applyCurrentPlanState('session-a', { current_plan: snapshot(2, ['pending']) });
  if (h.state.currentPlan.version !== 2 || h.state.currentPlan.steps.length !== 3) return fail(name, 'equal or older version replaced current plan');

  h.app.applyCurrentPlanState('session-a', { current_plan: snapshot(3, ['pending', 'pending']) });
  if (h.elements.planToggleWord.textContent !== 'Plan'
    || h.elements.planToggleProgress.textContent !== '1/2'
    || h.elements.planPanelProgress.textContent !== '1/2'
    || h.elements.planSheetProgress.textContent !== '1/2'
    || h.elements.planPanelChecklist.children.map((row) => row.children[0].textContent).join('') !== '○○') {
    return fail(name, 'all-pending position or markers are wrong');
  }
  h.app.applyCurrentPlanState('session-a', { current_plan: snapshot(4, ['in_progress']) });
  if (h.elements.planToggleProgress.textContent !== '1/1'
    || h.elements.planPanelChecklist.children[0]?.children[0]?.textContent !== '●') {
    return fail(name, 'in-progress-only position or marker is wrong');
  }
  h.app.applyCurrentPlanState('session-a', { current_plan: snapshot(5, ['completed', 'completed']) });
  if (!h.state.currentPlanUnseen
    || h.elements.planToggleWord.textContent !== 'Done'
    || h.elements.planToggleProgress.textContent !== '2/2'
    || h.elements.planPanelProgress.textContent !== '2/2'
    || h.elements.planSheetProgress.textContent !== '2/2'
    || h.elements.planToggleProgress.textContent.includes('✓')) {
    return fail(name, 'newer completed plan did not show unseen state and explicit Done progress');
  }
  if (!h.elements.planToggleBtn.getAttribute('aria-label').includes('All 2 steps complete')) return fail(name, 'accessible completed label is missing');

  h.app.openCurrentPlanSurface(h.elements.planToggleBtn);
  if (!h.state.currentPlanOpen || h.state.currentPlanUnseen || h.elements.planPanel.hidden || h.closeDiffCalls !== 1) return fail(name, 'desktop open state or Changes mutual exclusion is wrong');
  if (h.elements.appShell.getAttribute('inert') !== null) return fail(name, 'desktop plan panel incorrectly made the app inert');
  h.elements.planPanelBody.scrollTop = 47;
  h.app.applyCurrentPlanState('session-a', { current_plan: snapshot(6, ['completed', 'pending']) });
  if (!h.state.currentPlanOpen || h.elements.planPanelBody.scrollTop !== 47) return fail(name, 'open update closed surface or reset scroll');

  h.app.applyCurrentPlanState('session-a', { current_plan: null });
  if (h.state.currentPlan !== null || h.state.currentPlanOpen || !h.elements.planToggleBtn.hidden || !h.elements.planPanel.hidden) return fail(name, 'authoritative clear did not remove and close plan UI');
  pass(name);
}

function testSessionIsolationAndReset() {
  const name = 'plan application is isolated to the selected session';
  const h = createHarness();
  if (h.app.applyCurrentPlanState('session-b', { current_plan: snapshot(1) })) return fail(name, 'inactive session plan was applied');
  h.app.applyCurrentPlanState('session-a', { current_plan: snapshot(5) });
  h.app.resetCurrentPlanForSession('session-b');
  if (h.state.currentPlan !== null || h.state.currentPlanSessionId !== 'session-b' || !h.elements.planToggleBtn.hidden) return fail(name, 'session reset retained prior plan');
  pass(name);
}

function testMobileInteractionsAndBreakpointTransfer() {
  const name = 'mobile sheet dismisses, traps focus, and transfers across breakpoints';
  const h = createHarness();
  h.setMobile(true);
  h.app.applyCurrentPlanState('session-a', { current_plan: snapshot(1) });
  h.elements.promptInput.focus();
  h.app.openCurrentPlanSurface(h.elements.planToggleBtn);
  if (!h.elements.promptInput.blurCount || h.document.activeElement !== h.elements.planSheetCloseBtn) return fail(name, 'composer blur or initial sheet focus failed');
  if (h.elements.planSheet.hidden || h.elements.planSheetBackdrop.hidden || !h.elements.planPanel.hidden) return fail(name, 'mobile surface visibility is wrong');
  if (h.elements.appShell.getAttribute('inert') === null) return fail(name, 'mobile sheet did not make the background app inert');
  if (h.elements.planToggleBtn.getAttribute('aria-controls') !== 'planSheet') return fail(name, 'trigger does not control mobile sheet');
  if (h.elements.planSheet.style.height !== '396px') return fail(name, `sheet height was ${h.elements.planSheet.style.height}`);
  h.setViewportHeight(240);
  if (h.elements.planSheet.style.height !== '224px') return fail(name, `short viewport sheet height was ${h.elements.planSheet.style.height}`);
  h.setViewportHeight(600);

  let prevented = false;
  h.keydown({ key: 'Tab', target: h.elements.planSheetCloseBtn, preventDefault() { prevented = true; } });
  if (!prevented) return fail(name, 'focus trap did not cycle from the final control');
  prevented = false;
  h.keydown({ key: 'Tab', shiftKey: true, target: h.elements.planSheetCloseBtn, preventDefault() { prevented = true; } });
  if (!prevented) return fail(name, 'focus trap did not cycle backward from the first control');
  h.elements.mainHeader.focus();
  prevented = false;
  h.keydown({ key: 'Tab', preventDefault() { prevented = true; } });
  if (!prevented || h.document.activeElement !== h.elements.planSheetCloseBtn) return fail(name, 'focus trap did not recover focus from outside the dialog');

  h.setMobile(false);
  if (h.elements.planPanel.hidden || !h.elements.planSheet.hidden || h.document.activeElement !== h.elements.planPanelCloseBtn) return fail(name, 'mobile-to-desktop transfer failed');
  if (h.elements.appShell.getAttribute('inert') !== null) return fail(name, 'desktop transfer left the background app inert');
  if (h.elements.planToggleBtn.getAttribute('aria-controls') !== 'planPanel') return fail(name, 'trigger does not control desktop panel');
  h.setMobile(true);
  if (h.elements.planSheet.hidden || !h.elements.planPanel.hidden) return fail(name, 'desktop-to-mobile transfer left both surfaces visible');
  if (h.elements.appShell.getAttribute('inert') === null) return fail(name, 'mobile transfer did not restore background inertness');

  let escapePrevented = false;
  h.keydown({ key: 'Escape', preventDefault() { escapePrevented = true; }, stopImmediatePropagation() {} });
  if (!escapePrevented || h.state.currentPlanOpen || h.document.activeElement !== h.elements.planToggleBtn) return fail(name, 'Escape did not close and restore trigger focus');
  if (h.elements.appShell.getAttribute('inert') !== null) return fail(name, 'Escape left the background app inert');

  h.app.openCurrentPlanSurface(h.elements.planToggleBtn);
  h.click(h.elements.planSheetBackdrop);
  if (h.state.currentPlanOpen || h.elements.appShell.getAttribute('inert') !== null) return fail(name, 'backdrop did not close sheet and restore background interaction');
  h.app.openCurrentPlanSurface(h.elements.planToggleBtn);
  h.click(h.elements.planSheetCloseBtn);
  if (h.state.currentPlanOpen || h.elements.appShell.getAttribute('inert') !== null) return fail(name, 'close button did not close sheet and restore background interaction');

  h.app.openCurrentPlanSurface(h.elements.planToggleBtn);
  if (h.elements.appShell.getAttribute('inert') === null) return fail(name, 'forced-clear setup did not make the background inert');
  h.app.applyCurrentPlanState('session-a', { current_plan: null });
  if (h.document.activeElement !== h.elements.mainHeader || !h.elements.planToggleBtn.hidden
    || h.elements.appShell.getAttribute('inert') !== null) {
    return fail(name, 'authoritative clear left focus in the hidden sheet or background inert');
  }
  pass(name);
}

function testInvalidSnapshotsAreRejected() {
  const name = 'invalid server snapshots never populate current plan';
  const h = createHarness();
  const invalid = snapshot(1, ['in_progress', 'in_progress']);
  if (h.app.applyCurrentPlanState('session-a', { current_plan: invalid }) || h.state.currentPlan) return fail(name, 'multiple active steps were accepted');
  if (h.app.applyCurrentPlanState('session-a', { current_plan: { version: 1, steps: [] } }) || h.state.currentPlan) return fail(name, 'empty present snapshot was accepted');
  pass(name);
}

[
  testPlanSummaryRendersInitialAffordance,
  testPlanStateAndRendering,
  testSessionIsolationAndReset,
  testMobileInteractionsAndBreakpointTransfer,
  testInvalidSnapshotsAreRejected,
].forEach((test) => test());

if (failures > 0) process.exit(1);
console.log('\nAll tests passed');
