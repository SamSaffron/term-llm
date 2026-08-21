#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const source = fs.readFileSync(path.join(__dirname, 'app-worktrees.js'), 'utf8');
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
    add(value) { values.add(value); },
    remove(value) { values.delete(value); },
    toggle(value, force) {
      const enabled = force === undefined ? !values.has(value) : Boolean(force);
      if (enabled) values.add(value); else values.delete(value);
      return enabled;
    },
    contains(value) { return values.has(value); },
  };
}

function makeNode(tagName = 'div') {
  const listeners = {};
  const attributes = {};
  const node = {
    tagName: tagName.toUpperCase(),
    listeners,
    attributes,
    children: [],
    dataset: {},
    parentNode: null,
    classList: makeClassList(),
    style: {},
    hidden: false,
    textContent: '',
    appendChild(child) {
      child.parentNode = node;
      node.children.push(child);
      return child;
    },
    append(...children) { children.forEach((child) => node.appendChild(child)); },
    querySelector(selector) {
      const matches = (candidate) => selector === 'button' || selector === 'button:not([disabled])'
        ? candidate.tagName === 'BUTTON' && (selector === 'button' || !candidate.disabled)
        : false;
      const queue = [...node.children];
      while (queue.length) {
        const candidate = queue.shift();
        if (matches(candidate)) return candidate;
        queue.push(...candidate.children);
      }
      return null;
    },
    addEventListener(type, listener) {
      (listeners[type] = listeners[type] || []).push(listener);
    },
    setAttribute(name, value) { attributes[name] = String(value); },
    getAttribute(name) { return attributes[name] || null; },
    contains(target) {
      let current = target;
      while (current) {
        if (current === node) return true;
        current = current.parentNode;
      }
      return false;
    },
    remove() {
      if (!node.parentNode) return;
      node.parentNode.children = node.parentNode.children.filter((child) => child !== node);
      node.parentNode = null;
    },
    focus() {},
  };
  Object.defineProperty(node, 'className', {
    get() { return node._className || ''; },
    set(value) {
      node._className = String(value || '');
      for (const name of node._className.split(/\s+/)) if (name) node.classList.add(name);
    },
  });
  return node;
}

function flushAsync() {
  return new Promise((resolve) => setImmediate(resolve));
}

function makeHarness(options = {}) {
  const documentListeners = {};
  const body = makeNode('body');
  const trigger = makeNode('button');
  const label = makeNode('span');
  trigger.appendChild(label);
  const backdrop = makeNode('div');
  backdrop.hidden = true;
  const positionCalls = [];
  const recoveryCalls = [];
  const windowListeners = {};
  const session = options.session || { id: 'session-root', worktreeDir: '' };
  const state = {
    activeSessionId: session.id,
    draftSessionActive: Boolean(options.draftSessionActive),
    selectedWorktreeDir: options.selectedWorktreeDir || '',
    selectedWorktreeName: options.selectedWorktreeName || '',
    sessions: [session],
    worktrees: [],
    capabilitiesRequired: options.capabilitiesRequired === true || Boolean(options.projectsEnabled),
    capabilitiesLoaded: options.capabilitiesLoaded !== false,
    worktreesEnabled: options.worktreesEnabled !== false,
    projectsEnabled: Boolean(options.projectsEnabled),
    activeProjectId: options.activeProjectId || session.projectId || '',
    projects: options.projects || [],
    projectDrafts: {},
  };
  const elements = {
    chipWorktree: makeNode(),
    chipWorktreeTrigger: trigger,
    chipWorktreeLabel: label,
    chipSepEffortWorktree: makeNode(),
    chipPopoverBackdrop: backdrop,
  };
  const document = {
    body,
    createElement: (tag) => makeNode(tag),
    getElementById(id) {
      const queue = [body];
      while (queue.length) {
        const candidate = queue.shift();
        if (candidate.id === id) return candidate;
        queue.push(...candidate.children);
      }
      return null;
    },
    addEventListener(type, listener) {
      (documentListeners[type] = documentListeners[type] || []).push(listener);
    },
  };
  const windowObj = {
    TERM_LLM_WORKTREES_ENABLED: options.enabled === true,
    TermLLMApp: {
      UI_PREFIX: '/chat',
      state,
      elements,
      getActiveSession: () => state.sessions[0],
      requestHeaders: () => ({}),
      async normalizeError(response) {
        const payload = await response.json().catch(() => ({}));
        return { status: response.status, code: String(payload?.error?.code || ''), message: payload?.error?.message || `Request failed (${response.status})` };
      },
      loadCapabilities() { recoveryCalls.push({ type: 'capabilities' }); },
      loadProjectSidebar(options) { recoveryCalls.push({ type: 'sidebar', options }); },
      positionChipPopover(...args) { positionCalls.push(args); },
    },
    addEventListener(type, listener) {
      (windowListeners[type] = windowListeners[type] || []).push(listener);
    },
    visualViewport: null,
    prompt: options.prompt || (() => null),
    alert() {},
    confirm: () => false,
  };
  let worktreeRequests = 0;
  const requestURLs = [];
  const fetch = async (url, requestOptions = {}) => {
    worktreeRequests += 1;
    requestURLs.push(String(url));
    if (options.apiFetch) return options.apiFetch(url, requestOptions);
    return ({
      ok: true,
      status: 200,
      json: async () => ({
        worktrees: [
          { root: true, name: 'root', dir: '/repo' },
          { name: 'feature', dir: '/repo-worktrees/feature', branch: 'feature', dirty_files: 2 },
        ],
      }),
      text: async () => '',
    });
  };
  const context = {
    window: windowObj,
    document,
    fetch,
    setInterval() { return 1; },
    clearInterval() {},
    console,
  };
  context.globalThis = context;
  windowObj.TermLLMApp.apiFetch = (...args) => fetch(...args);
  vm.runInNewContext(source, context, { filename: 'app-worktrees.js' });
  return {
    app: windowObj.TermLLMApp,
    backdrop,
    body,
    document,
    documentListeners,
    elements,
    label,
    positionCalls,
    recoveryCalls,
    trigger,
    windowListeners,
    requestURLs,
    state,
    get worktreeRequests() { return worktreeRequests; },
  };
}

async function testGitCapabilityRendersAndLoadsLazily() {
  const name = 'git bootstrap renders immediately and keeps worktree list lazy';
  const harness = makeHarness({ enabled: true });
  await flushAsync();

  if (harness.worktreeRequests !== 0) {
    fail(name, `startup issued ${harness.worktreeRequests} unconditional worktree request(s)`);
    return;
  }
  if (harness.elements.chipWorktree.hidden || harness.elements.chipSepEffortWorktree.hidden) {
    fail(name, 'explicit git capability did not render the worktree control immediately');
    return;
  }
  if (harness.label.textContent !== 'root' || harness.trigger.title !== 'Manage worktrees') {
    fail(name, `root chip rendered label/title ${JSON.stringify(harness.label.textContent)}/${JSON.stringify(harness.trigger.title)}`);
    return;
  }

  const clickListener = harness.trigger.listeners.click && harness.trigger.listeners.click[0];
  if (!clickListener) {
    fail(name, 'worktree trigger has no click listener');
    return;
  }
  clickListener({ target: harness.label, preventDefault() {} });
  await flushAsync();
  await flushAsync();
  if (harness.worktreeRequests !== 1) {
    fail(name, `first interaction issued ${harness.worktreeRequests} worktree requests instead of one lazy list load`);
    return;
  }

  const menu = harness.body.children.find((child) => child.classList.contains('worktree-popover'));
  if (!menu) {
    fail(name, 'clicking root did not render a menu');
    return;
  }
  if (menu.getAttribute('role') !== 'menu' || menu.children.some((child) => child.getAttribute('role') !== 'menuitem')) {
    fail(name, 'active-session worktree actions do not use menu/menuitem semantics');
    return;
  }
  if (!menu.children[0].disabled) {
    fail(name, 'the current root checkout is exposed as an actionable menu item');
    return;
  }
  const lastPositionCall = harness.positionCalls[harness.positionCalls.length - 1];
  if (!lastPositionCall || lastPositionCall[2]?.mobileSheet !== true) {
    fail(name, 'worktree menu did not request mobile bottom-sheet positioning');
    return;
  }
  if (harness.backdrop.hidden) {
    fail(name, 'worktree menu backdrop stayed hidden');
    return;
  }

  for (const listener of harness.documentListeners.click || []) {
    listener({ target: harness.label });
  }
  if (!harness.body.children.includes(menu)) {
    fail(name, 'the opening click bubbling from the trigger label immediately closed the menu');
    return;
  }

  for (const listener of harness.windowListeners.resize || []) {
    listener();
  }
  const resizePositionCall = harness.positionCalls[harness.positionCalls.length - 1];
  if (!resizePositionCall || resizePositionCall[2]?.mobileSheet !== true) {
    fail(name, 'worktree menu lost bottom-sheet positioning after a viewport resize');
    return;
  }

  for (const listener of harness.backdrop.listeners.click || []) {
    listener({ target: harness.backdrop });
  }
  if (harness.body.children.includes(menu) || !harness.backdrop.hidden) {
    fail(name, 'backdrop click did not close the worktree menu');
    return;
  }
  pass(name);
}

async function testNonGitCapabilityNeverRendersOrRequests() {
  const name = 'non-git bootstrap never renders or requests worktrees';
  const harness = makeHarness({ enabled: false });
  await flushAsync();

  if (!harness.elements.chipWorktree.hidden || !harness.elements.chipSepEffortWorktree.hidden) {
    fail(name, 'non-git bootstrap left the worktree control or separator visible');
    return;
  }
  const clickListeners = harness.trigger.listeners.click || [];
  for (const listener of clickListeners) {
    listener({ target: harness.label, preventDefault() {} });
  }
  await harness.app.loadWorktrees();
  await flushAsync();
  await flushAsync();
  if (harness.worktreeRequests !== 0) {
    fail(name, `startup/click issued ${harness.worktreeRequests} worktree request(s)`);
    return;
  }
  if (harness.body.children.some((child) => child.classList.contains('worktree-popover'))) {
    fail(name, 'non-git capability opened an unusable worktree menu');
    return;
  }
  pass(name);
}

async function testSelectedSessionWorktreeLabelSurvivesLazyBootstrap() {
  const name = 'git bootstrap preserves selected session worktree labels without eager list';
  const harness = makeHarness({
    enabled: true,
    session: {
      id: 'session-worktree',
      worktreeDir: '/repo-worktrees/feature',
      worktreeName: 'feature',
    },
  });
  await flushAsync();

  if (harness.elements.chipWorktree.hidden) {
    fail(name, 'selected session worktree chip is hidden');
    return;
  }
  if (harness.label.textContent !== '⌥ feature') {
    fail(name, `selected worktree label = ${JSON.stringify(harness.label.textContent)}`);
    return;
  }
  if (harness.trigger.title !== 'Open worktree diff/actions') {
    fail(name, `selected worktree title = ${JSON.stringify(harness.trigger.title)}`);
    return;
  }
  if (harness.worktreeRequests !== 0) {
    fail(name, `selected session label required ${harness.worktreeRequests} eager list request(s)`);
    return;
  }
  pass(name);
}

async function testProjectScopedRoutesAndAccessibleActions() {
  const name = 'project drafts use project-scoped worktree routes without browser prompts';
  if (/window\.(prompt|alert|confirm)\s*\(/.test(source)) {
    fail(name, 'worktree UI still invokes window.prompt/window.alert/window.confirm');
    return;
  }
  const harness = makeHarness({
    enabled: false,
    projectsEnabled: true,
    activeProjectId: 'prj_alpha',
    projects: [{ id: 'prj_alpha', name: 'Alpha', git: true, available: true }],
    session: { id: 'draft-session', projectId: 'prj_alpha', worktreeDir: '' },
    draftSessionActive: true,
  });
  await harness.app.loadWorktrees();
  if (harness.requestURLs[0] !== '/chat/v1/projects/prj_alpha/worktrees') {
    fail(name, `project worktree URL = ${JSON.stringify(harness.requestURLs[0])}`);
    return;
  }
  if (harness.elements.chipWorktree.hidden || harness.trigger.disabled) {
    fail(name, 'available Git project did not expose its Worktree control');
    return;
  }
  pass(name);
}

async function testProjectContextMenuManagesInactiveProjectWithoutStartingDraft() {
  const name = 'project context worktrees target inactive project without changing draft context';
  const harness = makeHarness({
    enabled: false,
    projectsEnabled: true,
    activeProjectId: 'prj_beta',
    projects: [
      { id: 'prj_alpha', name: 'Alpha', git: true, available: true },
      { id: 'prj_beta', name: 'Beta', git: true, available: true },
    ],
    session: { id: 'session-beta', projectId: 'prj_beta', worktreeDir: '' },
    draftSessionActive: false,
  });
  await harness.app.openWorktreeMenuForProject('prj_alpha');
  if (harness.requestURLs[0] !== '/chat/v1/projects/prj_alpha/worktrees') {
    fail(name, `management URL = ${JSON.stringify(harness.requestURLs[0])}`);
    return;
  }
  if (harness.state.activeProjectId !== 'prj_beta' || harness.state.draftSessionActive) {
    fail(name, 'management action changed the active project/draft');
    return;
  }
  const menu = harness.body.children.find((node) => node.classList.contains('worktree-popover'));
  if (!menu || !menu.children.some((node) => node.children?.[0]?.textContent === '+ new worktree…')) {
    fail(name, 'project management menu did not expose worktree creation');
    return;
  }
  pass(name);
}

async function testAuthenticatedCapabilityOverridesBootstrapHint() {
  const name = 'authenticated capability overrides the unauthenticated bootstrap hint';
  const harness = makeHarness({ enabled: true, capabilitiesRequired: true, capabilitiesLoaded: true, worktreesEnabled: false });
  harness.app.renderWorktreeChip();
  if (!harness.elements.chipWorktree.hidden || !harness.trigger.disabled) {
    fail(name, 'stale bootstrap hint exposed worktrees after authenticated capability disabled them');
    return;
  }
  harness.state.worktreesEnabled = true;
  harness.app.renderWorktreeChip();
  if (harness.elements.chipWorktree.hidden || harness.trigger.disabled) {
    fail(name, 'authenticated enabled capability did not reveal legacy worktrees');
    return;
  }
  pass(name);
}

async function testTypedProjectFailureTriggersRecovery() {
  const name = 'typed project worktree failures trigger project recovery without raw status copy';
  const harness = makeHarness({
    projectsEnabled: true,
    activeProjectId: 'prj_missing',
    projects: [{ id: 'prj_missing', name: 'Missing', git: true, available: true }],
    session: { id: 'draft-missing', projectId: 'prj_missing', worktreeDir: '' },
    draftSessionActive: true,
    apiFetch: async () => new Response(JSON.stringify({ error: { code: 'project_not_found', message: 'project not found' } }), { status: 404, headers: { 'Content-Type': 'application/json' } }),
  });
  const rows = await harness.app.loadWorktrees();
  await flushAsync();
  if (rows.length !== 0 || harness.recoveryCalls.length !== 1 || harness.recoveryCalls[0].type !== 'sidebar') {
    fail(name, `typed failure recovery = ${JSON.stringify(harness.recoveryCalls)}`);
    return;
  }
  pass(name);
}

async function testFailedWorktreeListShowsRetrySheetWithoutRawHTTP() {
  const name = 'failed worktree list opens a visible Retry sheet without raw HTTP copy';
  const harness = makeHarness({
    enabled: true,
    apiFetch: async () => new Response('{}', { status: 500, headers: { 'Content-Type': 'application/json' } }),
  });
  harness.trigger.listeners.click[0]({ target: harness.label, preventDefault() {} });
  await flushAsync();
  await flushAsync();
  const sheet = harness.document.getElementById('worktreeActionSheet');
  const dialog = sheet?.children?.[0];
  const content = dialog?.children?.[1];
  const status = dialog?.children?.[2];
  if (!sheet || content?.children?.[0]?.textContent !== 'Retry') {
    fail(name, 'worktree list failure did not expose a Retry action');
    return;
  }
  if (!status.textContent.includes('Could not load worktrees') || /500|Request failed/.test(status.textContent)) {
    fail(name, `failure exposed raw transport copy: ${status.textContent}`);
    return;
  }
  pass(name);
}

async function testUnavailableProjectChipExposesStatusRetry() {
  const name = 'unavailable project worktree chip exposes visible status Retry';
  const harness = makeHarness({
    projectsEnabled: true,
    activeProjectId: 'prj_unavailable',
    projects: [{ id: 'prj_unavailable', name: 'Unavailable', git: true, available: false, unavailable_reason: 'Directory identity changed' }],
    session: { id: 'session-unavailable', projectId: 'prj_unavailable', worktreeDir: '' },
  });
  harness.app.renderWorktreeChip();
  if (harness.elements.chipWorktree.hidden || harness.trigger.disabled || !harness.label.textContent.includes('Retry')) {
    fail(name, 'unavailable project did not expose an actionable Retry state');
    return;
  }
  harness.trigger.listeners.click[0]({ target: harness.label, preventDefault() {} });
  await flushAsync();
  if (harness.recoveryCalls.length !== 1 || harness.recoveryCalls[0].type !== 'sidebar' || harness.recoveryCalls[0].options?.refreshStatus !== true) {
    fail(name, `Retry did not refresh project status: ${JSON.stringify(harness.recoveryCalls)}`);
    return;
  }
  pass(name);
}

async function testInUseRemovalRequiresExplicitForceRecovery() {
  const name = 'in-use worktree removal lists sessions and requires explicit force recovery';
  let deleteCount = 0;
  const harness = makeHarness({
    enabled: true,
    session: { id: 'session-worktree', worktreeDir: '/repo-worktrees/feature', worktreeName: 'feature' },
    apiFetch: async (url, requestOptions = {}) => {
      if (requestOptions.method === 'DELETE') {
        deleteCount += 1;
        if (deleteCount === 1) {
          return new Response(JSON.stringify({ error: 'worktree in use', in_use: [{ id: 'sess-1', number: 12, name: 'Active fix' }] }), { status: 409, headers: { 'Content-Type': 'application/json' } });
        }
        return new Response(JSON.stringify({ ok: true }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      return new Response(JSON.stringify({ worktrees: [{ root: true, name: 'root', dir: '/repo' }] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
  });
  const click = harness.trigger.listeners.click?.[0];
  click({ target: harness.label, preventDefault() {} });
  await flushAsync();
  const sheet = harness.document.getElementById('worktreeActionSheet');
  const dialog = sheet?.children?.[0];
  const content = dialog?.children?.[1];
  const status = dialog?.children?.[2];
  const actions = content?.children?.[1];
  const remove = actions?.children?.find((button) => button.textContent === 'Remove');
  if (!remove) {
    fail(name, 'worktree action sheet did not expose Remove');
    return;
  }
  await remove.listeners.click[0](removeEvent(remove));
  if (deleteCount !== 0 || remove.textContent !== 'Confirm remove') {
    fail(name, 'first removal click was not a local confirmation');
    return;
  }
  await remove.listeners.click[0](removeEvent(remove));
  if (deleteCount !== 1 || remove.textContent !== 'Force remove' || !status.textContent.includes('Active fix')) {
    fail(name, `in-use recovery missing: deletes=${deleteCount} label=${remove.textContent} status=${status.textContent}`);
    return;
  }
  await remove.listeners.click[0](removeEvent(remove));
  if (deleteCount !== 2 || !harness.requestURLs.some((url) => url.includes('force=1')) || !status.textContent.includes('force removed')) {
    fail(name, `force removal did not complete: ${JSON.stringify(harness.requestURLs)} ${status.textContent}`);
    return;
  }
  pass(name);
}

function removeEvent(target) {
  return { type: 'click', target, preventDefault() {} };
}

(async () => {
  await testGitCapabilityRendersAndLoadsLazily();
  await testNonGitCapabilityNeverRendersOrRequests();
  await testSelectedSessionWorktreeLabelSurvivesLazyBootstrap();
  await testProjectScopedRoutesAndAccessibleActions();
  await testProjectContextMenuManagesInactiveProjectWithoutStartingDraft();
  await testAuthenticatedCapabilityOverridesBootstrapHint();
  await testTypedProjectFailureTriggersRecovery();
  await testFailedWorktreeListShowsRetrySheetWithoutRawHTTP();
  await testUnavailableProjectChipExposesStatusRetry();
  await testInUseRemovalRequiresExplicitForceRecovery();
  if (failures > 0) process.exit(1);
  console.log('\nAll tests passed');
})().catch((error) => {
  console.error(error);
  process.exit(1);
});
