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
  const windowListeners = {};
  const session = options.session || { id: 'session-root', worktreeDir: '' };
  const state = {
    activeSessionId: session.id,
    draftSessionActive: Boolean(options.draftSessionActive),
    selectedWorktreeDir: options.selectedWorktreeDir || '',
    selectedWorktreeName: options.selectedWorktreeName || '',
    sessions: [session],
    worktrees: [],
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
  const fetch = async () => {
    worktreeRequests += 1;
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
  vm.runInNewContext(source, context, { filename: 'app-worktrees.js' });
  return {
    app: windowObj.TermLLMApp,
    backdrop,
    body,
    documentListeners,
    elements,
    label,
    positionCalls,
    trigger,
    windowListeners,
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

(async () => {
  await testGitCapabilityRendersAndLoadsLazily();
  await testNonGitCapabilityNeverRendersOrRequests();
  await testSelectedSessionWorktreeLabelSurvivesLazyBootstrap();
  if (failures > 0) process.exit(1);
  console.log('\nAll tests passed');
})().catch((error) => {
  console.error(error);
  process.exit(1);
});
