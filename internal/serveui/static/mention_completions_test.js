#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const source = fs.readFileSync(path.join(__dirname, 'slash-commands.js'), 'utf8');

class ClassList {
  constructor(element) { this.element = element; }
  values() { return new Set(String(this.element.className || '').split(/\s+/).filter(Boolean)); }
  toggle(token, force) {
    const values = this.values();
    if (force) values.add(token); else values.delete(token);
    this.element.className = [...values].join(' ');
  }
}

class Element {
  constructor() {
    this.children = [];
    this.listeners = {};
    this.attributes = {};
    this.className = '';
    this.classList = new ClassList(this);
    this.hidden = true;
    this.value = '';
    this.selectionStart = 0;
    this.selectionEnd = 0;
    this.textContent = '';
  }
  addEventListener(type, listener) { (this.listeners[type] ||= []).push(listener); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.children = [...children]; }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  focus() { this.focused = true; }
  dispatch(type, init = {}) {
    const event = {
      type,
      key: '',
      isComposing: false,
      defaultPrevented: false,
      immediatePropagationStopped: false,
      preventDefault() { this.defaultPrevented = true; },
      stopImmediatePropagation() { this.immediatePropagationStopped = true; },
      ...init,
    };
    for (const listener of this.listeners[type] || []) {
      listener(event);
      if (event.immediatePropagationStopped) break;
    }
    return event;
  }
}

const wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const assert = (condition, message) => { if (!condition) throw new Error(message); };
const response = (payload) => ({ ok: true, status: 200, async json() { return payload; } });

async function main() {
  const promptInput = new Element();
  const slashCommandMenu = new Element();
  const requests = [];
  let fetchImpl = async (url, options) => {
    requests.push({ url, options });
    return response({ active: false, items: [] });
  };
  const session = { id: 'session one', worktreeDir: '/managed/worktree' };
  const app = {
    UI_PREFIX: '/chat',
    elements: { promptInput, slashCommandMenu },
    state: { streaming: false, draftSessionActive: false, selectedWorktreeDir: '' },
    getActiveSession: () => session,
    requestHeaders: (id) => ({ 'Content-Type': 'application/json', session_id: id }),
    apiFetch: (...args) => fetchImpl(...args),
    autoGrowPrompt() { app.growCalls = (app.growCalls || 0) + 1; },
  };
  const document = {
    createElement() { return new Element(); },
    createTextNode(text) { const node = new Element(); node.textContent = String(text); return node; },
  };
  const window = { TermLLMApp: app };
  vm.runInNewContext(source, {
    window, document, console, setTimeout, clearTimeout, AbortController, fetch: () => {},
  }, { filename: 'slash-commands.js' });

  fetchImpl = async (url, options) => {
    requests.push({ url, options });
    return response({
      active: true,
      token: { start_utf16: 7, end_utf16: 11, query: 'typ', quoted: false },
      items: [
        {
          path: 'types.go', kind: 'file', insert_text: '@types.go',
          segments: [{ text: 'typ', matched: true }, { text: 'es.go', matched: false }],
        },
        { path: 'typed', kind: 'directory', insert_text: '@typed/', segments: [] },
      ],
    });
  };
  promptInput.value = 'review @typ now';
  promptInput.selectionStart = 11;
  promptInput.selectionEnd = 11;
  promptInput.dispatch('input');
  await wait(70);
  assert(requests.length === 1, `expected one mention request, got ${requests.length}`);
  assert(requests[0].url === '/chat/v1/mentions/search', `unexpected mention URL: ${requests[0].url}`);
  const body = JSON.parse(requests[0].options.body);
  assert(body.text === 'review @typ now' && body.cursor_utf16 === 11, `unexpected request body: ${requests[0].options.body}`);
  assert(body.limit === 10, `mention result limit was not capped at 10: ${body.limit}`);
  assert(body.worktree_dir === '/managed/worktree', 'active worktree was not sent as draft/session context');
  assert(requests[0].options.headers.session_id === 'session one', 'session headers were not reused');
  assert(!slashCommandMenu.hidden && slashCommandMenu.children.length === 2, 'mention results did not open shared completion menu');
  const optionName = slashCommandMenu.children[0].children[0];
  assert(optionName.children[0].className === 'mention-completion-match', 'server match segments were not highlighted');
  const requestCountBeforeNavigation = requests.length;
  promptInput.focus();
  const up = promptInput.dispatch('keydown', { key: 'ArrowUp' });
  promptInput.dispatch('keyup', { key: 'ArrowUp' });
  assert(up.defaultPrevented && up.immediatePropagationStopped, 'ArrowUp did not stay owned by the completion menu');
  assert(promptInput.focused, 'ArrowUp moved focus away from the composer');
  assert(promptInput.attributes['aria-activedescendant'] === 'composer-completion-1', 'ArrowUp did not wrap to the final result');
  await wait(70);
  assert(requests.length === requestCountBeforeNavigation, 'ArrowUp incorrectly restarted mention search');
  promptInput.dispatch('keydown', { key: 'ArrowDown' });

  const accepted = promptInput.dispatch('keydown', { key: 'Enter' });
  assert(accepted.defaultPrevented && accepted.immediatePropagationStopped, 'mention acceptance did not consume Enter');
  assert(promptInput.value === 'review @types.go now', `mention replacement damaged surrounding text: ${promptInput.value}`);
  assert(promptInput.selectionStart === 16 && promptInput.selectionEnd === 16, 'caret was not restored after mention acceptance');
  assert(slashCommandMenu.hidden && promptInput.focused, 'mention acceptance did not close and refocus the menu');

  requests.length = 0;
  promptInput.value = 'mail@example.com';
  promptInput.selectionStart = promptInput.value.length;
  promptInput.dispatch('input');
  await wait(70);
  assert(requests.length === 0 && slashCommandMenu.hidden, 'email address incorrectly triggered project mentions');

  fetchImpl = async (_url, options) => {
    const request = JSON.parse(options.body);
    return response({
      active: true,
      token: { start_utf16: 0, end_utf16: 1, query: '', quoted: false },
      items: [{ path: 'README.md', kind: 'file', insert_text: '@README.md', segments: [] }],
      request,
    });
  };
  promptInput.value = '@';
  promptInput.selectionStart = 1;
  promptInput.dispatch('input');
  await wait(70);
  const bareEnter = promptInput.dispatch('keydown', { key: 'Enter' });
  assert(!bareEnter.defaultPrevented && promptInput.value === '@', 'Enter on bare @ should submit instead of selecting a file');
  app.invalidateMentionCompletions();

  const pending = [];
  fetchImpl = (url, options) => new Promise((resolve) => pending.push({ url, options, resolve }));
  promptInput.value = '@a';
  promptInput.selectionStart = 2;
  promptInput.dispatch('input');
  await wait(60);
  assert(pending.length === 1, `expected initial controlled search, got ${pending.length}`);
  pending[0].resolve(response({
    active: true,
    token: { start_utf16: 0, end_utf16: 2, query: 'a', quoted: false },
    items: [
      { path: 'alpha.md', kind: 'file', insert_text: '@alpha.md', segments: [] },
      { path: 'ancient.md', kind: 'file', insert_text: '@ancient.md', segments: [] },
    ],
  }));
  await wait(0);
  promptInput.value = '@ab';
  promptInput.selectionStart = 3;
  promptInput.dispatch('input');
  await wait(60);
  assert(pending.length === 2, `expected refresh search, got ${pending.length}`);
  const staleUp = promptInput.dispatch('keydown', { key: 'ArrowUp' });
  const staleTab = promptInput.dispatch('keydown', { key: 'Tab' });
  assert(staleUp.defaultPrevented && staleTab.defaultPrevented, 'stale visible results did not retain arrow/Tab ownership');
  assert(promptInput.value === '@ab' && promptInput.focused, 'stale Tab changed text or moved composer focus');
  pending[1].resolve(response({
    active: true,
    token: { start_utf16: 0, end_utf16: 3, query: 'ab', quoted: false },
    items: [{ path: 'about.md', kind: 'file', insert_text: '@about.md', segments: [] }],
  }));
  await wait(0);

  promptInput.value = '@abc';
  promptInput.selectionStart = 4;
  promptInput.dispatch('input');
  await wait(60);
  promptInput.value = '@abcd';
  promptInput.selectionStart = 5;
  promptInput.dispatch('input');
  await wait(60);
  assert(pending.length === 4, `expected two generation-controlled searches, got ${pending.length}`);
  pending[3].resolve(response({
    active: true,
    token: { start_utf16: 0, end_utf16: 5, query: 'abcd', quoted: false },
    items: [{ path: 'abcd.md', kind: 'file', insert_text: '@abcd.md', segments: [] }],
  }));
  await wait(0);
  pending[2].resolve(response({
    active: true,
    token: { start_utf16: 0, end_utf16: 4, query: 'abc', quoted: false },
    items: [{ path: 'ancient.md', kind: 'file', insert_text: '@ancient.md', segments: [] }],
  }));
  await wait(0);
  assert(slashCommandMenu.children[0].children[0].textContent === 'abcd.md', 'stale mention response replaced newer results');

  app.state.draftSessionActive = true;
  app.state.selectedWorktreeDir = '/managed/draft';
  app.invalidateMentionCompletions();
  assert(slashCommandMenu.hidden, 'context invalidation did not close mention completions');

  console.log('PASS: project mention autocomplete');
}

main().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
