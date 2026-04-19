#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const { TextEncoder, TextDecoder } = require('util');
const { webcrypto } = require('crypto');

const dir = __dirname;
const coreSource = fs.readFileSync(path.join(dir, 'app-core.js'), 'utf8');
const streamSource = fs.readFileSync(path.join(dir, 'app-stream.js'), 'utf8');

let failures = 0;
function fail(name, message, details) {
  console.error('FAIL:', name, '-', message);
  if (details !== undefined) console.error('      ', details);
  failures += 1;
}
function pass(name) { console.log('PASS:', name); }

function makeClassList() {
  const cls = new Set();
  return {
    add(c) { cls.add(c); },
    remove(c) { cls.delete(c); },
    toggle(c, on) {
      if (on === undefined) on = !cls.has(c);
      if (on) cls.add(c); else cls.delete(c);
      return on;
    },
    contains(c) { return cls.has(c); },
  };
}

function makeNode(extra = {}) {
  const attrs = {};
  const listeners = {};
  const self = {
    attrs,
    listeners,
    setAttribute(k, v) { attrs[k] = String(v); },
    removeAttribute(k) { delete attrs[k]; },
    getAttribute(k) { return attrs[k] || null; },
    hasAttribute(k) { return Object.prototype.hasOwnProperty.call(attrs, k); },
    addEventListener(type, fn) { (listeners[type] = listeners[type] || []).push(fn); },
    removeEventListener() {},
    dispatchEvent(ev) {
      const arr = listeners[ev.type] || [];
      arr.forEach((fn) => fn(ev));
      return true;
    },
    appendChild(child) {
      self.children.push(child);
      child.parentNode = self;
      return child;
    },
    contains(other) {
      if (!other) return false;
      let cur = other;
      while (cur) { if (cur === self) return true; cur = cur.parentNode; }
      return false;
    },
    querySelector(sel) {
      // Very limited: only look up by class for popover items.
      if (sel === '.chip-popover-item.focused') {
        return self.children.find((c) => c.classList?.contains('chip-popover-item') && c.classList?.contains('focused')) || null;
      }
      if (sel === '.chip-popover-item[aria-selected="true"]') {
        return self.children.find((c) => c.classList?.contains('chip-popover-item') && c.attrs?.['aria-selected'] === 'true') || null;
      }
      if (sel === '.chip-popover-item') {
        return self.children.find((c) => c.classList?.contains('chip-popover-item')) || null;
      }
      return null;
    },
    querySelectorAll(sel) {
      if (sel === '.chip-popover-item') {
        return self.children.filter((c) => c.classList?.contains('chip-popover-item'));
      }
      if (sel === '.chip-popover-item.focused') {
        return self.children.filter((c) => c.classList?.contains('chip-popover-item') && c.classList?.contains('focused'));
      }
      return [];
    },
    closest() { return null; },
    focus() {},
    classList: makeClassList(),
    children: [],
    style: {},
    dataset: {},
    options: [],
    value: '',
    textContent: '',
    innerHTML: '',
    hidden: false,
    disabled: false,
    tabIndex: 0,
    parentNode: null,
    getBoundingClientRect() { return { left: 0, top: 0, right: 0, bottom: 0, width: 100, height: 24 }; },
  };
  // Make innerHTML setter clear children
  Object.defineProperty(self, 'innerHTML', {
    get() { return self._inner || ''; },
    set(v) { self._inner = v; if (v === '') self.children = []; },
  });
  // className is a property in real DOM but it has to sync to classList so
  // selectors like '.chip-popover-item' work in tests.
  Object.defineProperty(self, 'className', {
    get() { return self._className || ''; },
    set(v) {
      if (self._className) {
        for (const c of self._className.split(/\s+/)) if (c) self.classList.remove(c);
      }
      self._className = String(v || '');
      for (const c of self._className.split(/\s+/)) if (c) self.classList.add(c);
    },
  });
  Object.assign(self, extra);
  return self;
}

function makeOption(value, text) {
  return { value, textContent: text || value };
}

function makeContext() {
  const elementMap = {};
  const document = {
    activeElement: null,
    body: makeNode(),
    documentElement: makeNode(),
    createElement(tag) {
      const node = makeNode();
      node.tagName = tag.toUpperCase();
      return node;
    },
    getElementById(id) {
      if (!elementMap[id]) {
        elementMap[id] = makeNode();
        elementMap[id].id = id;
      }
      return elementMap[id];
    },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    addEventListener() {},
    removeEventListener() {},
  };

  const localStorageStore = {};
  const localStorage = {
    getItem(k) { return Object.prototype.hasOwnProperty.call(localStorageStore, k) ? localStorageStore[k] : null; },
    setItem(k, v) { localStorageStore[k] = String(v); },
    removeItem(k) { delete localStorageStore[k]; },
    clear() { for (const k of Object.keys(localStorageStore)) delete localStorageStore[k]; },
    key(i) { return Object.keys(localStorageStore)[i] || null; },
    get length() { return Object.keys(localStorageStore).length; },
  };

  const windowObj = {
    setTimeout, clearTimeout, setInterval, clearInterval,
    requestAnimationFrame(cb) { return setTimeout(cb, 0); },
    cancelAnimationFrame(h) { clearTimeout(h); },
    addEventListener() {}, removeEventListener() {},
    location: { search: '', origin: 'https://example.test', href: 'https://example.test/' },
    innerWidth: 1280, innerHeight: 800,
    matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {} }),
    Notification: undefined,
    PushManager: undefined,
    crypto: webcrypto,
    history: { replaceState() {}, pushState() {} },
    fetch: async () => ({ ok: true, status: 200, headers: { get: () => null }, json: async () => ({}), text: async () => '' }),
  };
  windowObj.document = document;
  windowObj.localStorage = localStorage;

  const navigator = { userAgent: 'node-test', serviceWorker: undefined, clipboard: undefined };

  const ctx = {
    window: windowObj,
    document,
    localStorage,
    navigator,
    location: windowObj.location,
    history: windowObj.history,
    console,
    setTimeout, clearTimeout, setInterval, clearInterval,
    requestAnimationFrame: windowObj.requestAnimationFrame,
    cancelAnimationFrame: windowObj.cancelAnimationFrame,
    fetch: windowObj.fetch,
    crypto: webcrypto,
    URL,
    URLSearchParams,
    TextEncoder,
    TextDecoder,
    Event: class Event {
      constructor(type, init = {}) { this.type = type; this.bubbles = !!init.bubbles; }
    },
    CustomEvent: class CustomEvent {
      constructor(type, init = {}) { this.type = type; this.detail = init.detail; }
    },
  };
  ctx.globalThis = ctx;
  return { ctx, document, localStorage, windowObj, elementMap };
}

function loadCore() {
  const { ctx, elementMap, windowObj } = makeContext();
  vm.runInNewContext(coreSource, ctx, { filename: 'app-core.js' });
  const app = ctx.window.TermLLMApp;
  return { ctx, app, elementMap, windowObj };
}

function loadCoreAndStream() {
  const { ctx, elementMap, windowObj } = makeContext();
  vm.runInNewContext(coreSource, ctx, { filename: 'app-core.js' });
  vm.runInNewContext(streamSource, ctx, { filename: 'app-stream.js' });
  const app = ctx.window.TermLLMApp;
  return { ctx, app, elementMap, windowObj };
}

function testSplitHeaderModelEffortDetectsKnownEffortSuffix() {
  const name = 'splitHeaderModelEffort detects known effort suffixes when effort is unset';
  const { app } = loadCore();
  const cases = [
    { in: ['opus-max', ''], out: { model: 'opus', effort: 'max' } },
    { in: ['gpt-5-medium', ''], out: { model: 'gpt-5', effort: 'medium' } },
    { in: ['claude-high', ''], out: { model: 'claude', effort: 'high' } },
    { in: ['model-xhigh', ''], out: { model: 'model', effort: 'xhigh' } },
    { in: ['plain-name', ''], out: { model: 'plain-name', effort: '' } },
    { in: ['gpt-5', 'low'], out: { model: 'gpt-5', effort: 'low' } },
    { in: ['gpt-5-low', 'low'], out: { model: 'gpt-5', effort: 'low' } },
  ];
  for (const c of cases) {
    const got = app.splitHeaderModelEffort(c.in[0], c.in[1]);
    if (got.model !== c.out.model || got.effort !== c.out.effort) {
      fail(name, `for input ${JSON.stringify(c.in)} expected ${JSON.stringify(c.out)} got ${JSON.stringify(got)}`);
      return;
    }
  }
  pass(name);
}

function testUpdateSessionUsageDisplayUsesProviderDefaultModel() {
  const name = 'updateSessionUsageDisplay shows provider default_model muted when no model selected';
  const { app, elementMap } = loadCore();
  app.state.providers = [
    { name: 'openai', is_default: true, default_model: 'gpt-5', models: ['gpt-5', 'gpt-4'] },
    { name: 'venice', is_default: false, default_model: '', models: ['llama'] },
  ];
  app.state.selectedProvider = '';
  app.state.selectedModel = '';
  app.state.selectedEffort = '';

  app.updateSessionUsageDisplay(null);

  const providerLabel = elementMap.chipProviderLabel;
  const modelLabel = elementMap.chipModelLabel;
  if (providerLabel.textContent !== 'openai') {
    fail(name, `expected provider label "openai" got "${providerLabel.textContent}"`);
    return;
  }
  if (modelLabel.textContent !== 'gpt-5') {
    fail(name, `expected model label "gpt-5" (default_model fallback) got "${modelLabel.textContent}"`);
    return;
  }
  if (!providerLabel.classList.contains('stats-muted')) {
    fail(name, 'provider label should be muted when showing default');
    return;
  }
  if (!modelLabel.classList.contains('stats-muted')) {
    fail(name, 'model label should be muted when showing default');
    return;
  }
  pass(name);
}

function testUpdateSessionUsageDisplayFallsBackToFirstModelWithoutDefault() {
  const name = 'updateSessionUsageDisplay falls back to first model when provider has no default_model';
  const { app, elementMap } = loadCore();
  app.state.providers = [
    { name: 'venice', is_default: true, default_model: '', models: ['llama-1', 'llama-2'] },
  ];
  app.state.selectedProvider = '';
  app.state.selectedModel = '';
  app.state.selectedEffort = '';

  app.updateSessionUsageDisplay(null);

  if (elementMap.chipModelLabel.textContent !== 'llama-1') {
    fail(name, `expected first model "llama-1" got "${elementMap.chipModelLabel.textContent}"`);
    return;
  }
  pass(name);
}

function testChipLockEngagesWithActiveSession() {
  const name = 'updateSessionUsageDisplay locks chip triggers when an active session exists';
  const { app, elementMap } = loadCore();
  app.state.providers = [{ name: 'openai', is_default: true, default_model: 'gpt-5', models: ['gpt-5'] }];

  // No session — chips unlocked.
  app.updateSessionUsageDisplay(null);
  for (const id of ['chipProviderTrigger', 'chipModelTrigger', 'chipEffortTrigger']) {
    if (elementMap[id].hasAttribute('disabled')) {
      fail(name, `${id} should not be disabled when session is null`);
      return;
    }
  }

  // Active session — chips locked.
  app.updateSessionUsageDisplay({ id: 'sess-1', activeModel: 'gpt-5' });
  for (const id of ['chipProviderTrigger', 'chipModelTrigger', 'chipEffortTrigger']) {
    if (!elementMap[id].hasAttribute('disabled')) {
      fail(name, `${id} should be disabled when session is active`);
      return;
    }
    if (elementMap[id].getAttribute('aria-disabled') !== 'true') {
      fail(name, `${id} should have aria-disabled=true when locked`);
      return;
    }
  }

  // Back to draft — chips unlocked again.
  app.updateSessionUsageDisplay(null);
  for (const id of ['chipProviderTrigger', 'chipModelTrigger', 'chipEffortTrigger']) {
    if (elementMap[id].hasAttribute('disabled')) {
      fail(name, `${id} should be re-enabled after returning to draft`);
      return;
    }
  }
  pass(name);
}

function testPopoverItemSelectionDispatchesChangeAndCloses() {
  const name = 'popover item click sets select value, dispatches change, and closes the popover';
  const { app, elementMap } = loadCoreAndStream();

  // Stub header refresh — the change listener calls app.updateHeader() which
  // is wired by app-sessions.js (not loaded here).
  app.updateHeader = () => {};

  // Wire up the underlying select with options the popover will render.
  const sel = elementMap.chipModelSelect;
  sel.value = '';
  sel.options = [makeOption('', 'Auto'), makeOption('gpt-5', 'gpt-5'), makeOption('gpt-4', 'gpt-4')];

  // Capture change events on the underlying select.
  let changeCount = 0;
  let lastChangeValue = null;
  sel.addEventListener('change', () => { changeCount += 1; lastChangeValue = sel.value; });

  // Simulate the user clicking the chip trigger to open the popover.
  const trigger = elementMap.chipModelTrigger;
  const triggerListeners = trigger.listeners?.click || [];
  if (triggerListeners.length === 0) {
    fail(name, 'chipModelTrigger has no click listener wired');
    return;
  }
  triggerListeners[0]({ stopPropagation() {}, preventDefault() {} });

  const popover = elementMap.chipPopover;
  if (popover.hidden) {
    fail(name, 'popover should be visible after trigger click');
    return;
  }
  if (popover.children.length !== 3) {
    fail(name, `expected 3 popover items, got ${popover.children.length}`);
    return;
  }

  // Click the second item ("gpt-5").
  const target = popover.children[1];
  const itemListeners = target.listeners?.click || [];
  if (itemListeners.length === 0) {
    fail(name, 'popover item missing click listener');
    return;
  }
  itemListeners[0]();

  if (changeCount !== 1) {
    fail(name, `expected change event to fire once, fired ${changeCount}x`);
    return;
  }
  if (lastChangeValue !== 'gpt-5') {
    fail(name, `expected change to commit value "gpt-5" got "${lastChangeValue}"`);
    return;
  }
  if (!popover.hidden) {
    fail(name, 'popover should be hidden after item selection');
    return;
  }
  pass(name);
}

function openModelPopover(elementMap, options) {
  const sel = elementMap.chipModelSelect;
  sel.value = '';
  sel.options = options;
  const trigger = elementMap.chipModelTrigger;
  const triggerListeners = trigger.listeners?.click || [];
  if (triggerListeners.length === 0) return null;
  triggerListeners[0]({ stopPropagation() {}, preventDefault() {} });
  return elementMap.chipPopover;
}

function testPopoverHidesFilterInputBelowThreshold() {
  const name = 'popover does not render filter input when option count is at or below threshold';
  const { app, elementMap } = loadCoreAndStream();
  app.updateHeader = () => {};

  // 10 options + auto = 11 — wait, the threshold is "options.length > 10".
  // Use exactly 10 options to confirm the filter is suppressed at the boundary.
  const opts = [];
  for (let i = 0; i < 10; i++) opts.push(makeOption(`m-${i}`, `m-${i}`));
  const popover = openModelPopover(elementMap, opts);
  if (!popover) return fail(name, 'no click listener on chipModelTrigger');

  const filterInputs = popover.children.filter((c) => c.tagName === 'INPUT');
  if (filterInputs.length !== 0) {
    fail(name, `expected no filter input at threshold, got ${filterInputs.length}`);
    return;
  }
  pass(name);
}

function testPopoverShowsFilterInputAboveThreshold() {
  const name = 'popover renders filter input when option count exceeds threshold';
  const { app, elementMap } = loadCoreAndStream();
  app.updateHeader = () => {};

  const opts = [];
  for (let i = 0; i < 15; i++) opts.push(makeOption(`m-${i}`, `m-${i}`));
  const popover = openModelPopover(elementMap, opts);
  if (!popover) return fail(name, 'no click listener on chipModelTrigger');

  const filterInputs = popover.children.filter((c) => c.tagName === 'INPUT');
  if (filterInputs.length !== 1) {
    fail(name, `expected 1 filter input, got ${filterInputs.length}`);
    return;
  }
  const items = popover.children.filter((c) => c.classList?.contains('chip-popover-item'));
  if (items.length !== 15) {
    fail(name, `expected 15 popover items, got ${items.length}`);
    return;
  }
  pass(name);
}

function testFilterInputHidesNonMatchingItems() {
  const name = 'typing into the filter hides items that do not match the query';
  const { app, elementMap } = loadCoreAndStream();
  app.updateHeader = () => {};

  const opts = [];
  for (let i = 0; i < 12; i++) opts.push(makeOption(`gpt-${i}`, `gpt-${i}`));
  opts.push(makeOption('claude-haiku', 'claude-haiku'));
  opts.push(makeOption('claude-sonnet', 'claude-sonnet'));
  const popover = openModelPopover(elementMap, opts);
  if (!popover) return fail(name, 'no click listener on chipModelTrigger');

  const filterInput = popover.children.find((c) => c.tagName === 'INPUT');
  if (!filterInput) return fail(name, 'expected a filter input');

  filterInput.value = 'claude';
  const inputListeners = filterInput.listeners?.input || [];
  if (inputListeners.length === 0) return fail(name, 'filter input has no input listener');
  inputListeners[0]();

  const visibleItems = popover.children.filter(
    (c) => c.classList?.contains('chip-popover-item') && !c.hidden
  );
  if (visibleItems.length !== 2) {
    fail(name, `expected 2 visible items after filtering for "claude", got ${visibleItems.length}`);
    return;
  }
  for (const it of visibleItems) {
    if (!it.dataset?.value?.startsWith('claude')) {
      fail(name, `unexpected visible item ${it.dataset?.value}`);
      return;
    }
  }
  // Clearing the filter restores all items.
  filterInput.value = '';
  inputListeners[0]();
  const restored = popover.children.filter(
    (c) => c.classList?.contains('chip-popover-item') && !c.hidden
  );
  if (restored.length !== opts.length) {
    fail(name, `expected all ${opts.length} items visible after clearing filter, got ${restored.length}`);
    return;
  }
  pass(name);
}

testSplitHeaderModelEffortDetectsKnownEffortSuffix();
testUpdateSessionUsageDisplayUsesProviderDefaultModel();
testUpdateSessionUsageDisplayFallsBackToFirstModelWithoutDefault();
testChipLockEngagesWithActiveSession();
testPopoverItemSelectionDispatchesChangeAndCloses();
testPopoverHidesFilterInputBelowThreshold();
testPopoverShowsFilterInputAboveThreshold();
testFilterInputHidesNonMatchingItems();

if (failures > 0) {
  console.error(`\n${failures} test(s) failed`);
  process.exit(1);
}
console.log('\nAll tests passed');
process.exit(0);
