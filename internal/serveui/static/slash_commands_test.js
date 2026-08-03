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
    this.hidden = false;
    this.value = '';
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

const promptInput = new Element();
const slashCommandMenu = new Element();
const document = {
  createElement() { return new Element(); },
};
const app = {
  elements: { promptInput, slashCommandMenu },
  state: { streaming: false },
  autoGrowPrompt() { app.growCalls = (app.growCalls || 0) + 1; },
};
const window = { TermLLMApp: app };
vm.runInNewContext(source, { window, document, console }, { filename: 'slash-commands.js' });

const assert = (condition, message) => { if (!condition) throw new Error(message); };

promptInput.value = '/';
promptInput.dispatch('input');
assert(!slashCommandMenu.hidden, 'typing / did not show slash commands');
assert(slashCommandMenu.children.length === 9, 'expected all matching slash commands');
const commandNames = slashCommandMenu.children.map((option) => option.children[0].textContent);
assert(JSON.stringify(commandNames) === JSON.stringify(['/compact', '/compress', '/goal', '/mcp', '/model', '/new', '/redo', '/side', '/undo']), `commands were not alphabetized: ${JSON.stringify(commandNames)}`);
assert(slashCommandMenu.children[7].children[1].textContent.includes('without interrupting'), '/side description was not useful');
assert(promptInput.attributes['aria-expanded'] === 'true', 'composer did not expose expanded autocomplete state');

promptInput.value = '/si';
promptInput.dispatch('input');
const accepted = promptInput.dispatch('keydown', { key: 'Tab' });
assert(accepted.defaultPrevented && accepted.immediatePropagationStopped, 'accepting autocomplete did not consume the key');
assert(promptInput.value === '/side ', 'Tab did not complete /side with a trailing space');
assert(slashCommandMenu.hidden, 'autocomplete remained open after acceptance');
assert(promptInput.focused, 'autocomplete did not return focus to composer');
assert(app.growCalls === 1, 'autocomplete did not resize the composer');

promptInput.value = '/si';
promptInput.dispatch('input');
const entered = promptInput.dispatch('keydown', { key: 'Enter' });
assert(entered.defaultPrevented && promptInput.value === '/side ', 'Enter did not accept /side without sending');

promptInput.value = '/';
promptInput.dispatch('input');
const escaped = promptInput.dispatch('keydown', { key: 'Escape' });
assert(escaped.defaultPrevented && slashCommandMenu.hidden, 'Escape did not dismiss autocomplete');
assert(promptInput.value === '/', 'Escape changed composer text');

promptInput.value = '/compa';
promptInput.dispatch('input');
assert(slashCommandMenu.children.length === 1, '/compact filter did not produce one command');
assert(slashCommandMenu.children[0].children[0].textContent === '/compact', '/compact command was not discoverable');

promptInput.value = '/compr';
promptInput.dispatch('input');
assert(slashCommandMenu.children.length === 1, '/compress filter did not produce one command');
assert(slashCommandMenu.children[0].children[0].textContent === '/compress', '/compress command was not discoverable');
promptInput.dispatch('keydown', { key: 'Enter' });
assert(promptInput.value === '/compress ', 'Enter did not complete /compress');

promptInput.value = '/unknown';
promptInput.dispatch('input');
assert(slashCommandMenu.hidden, 'autocomplete displayed with no matching commands');

app.setSkillCommands({ skills: [
  { name: 'review', description: 'Review changes', argument_hint: '[scope]', execution: 'isolated', source: 'local' },
  { name: 'explain', description: 'Explain code', argument_hint: '', execution: 'main', source: 'user' },
  { name: 'compact', description: 'Must not shadow built-in', execution: 'main', source: 'local', collides_with_builtin: true },
  { name: 'h', description: 'Must not shadow built-in alias', execution: 'main', source: 'local', collides_with_builtin: true },
] });
promptInput.value = '/';
promptInput.dispatch('input');
const dynamicNames = slashCommandMenu.children.map((option) => option.children[0].textContent);
assert(dynamicNames.includes('/review [scope]'), `dynamic skill hint missing: ${JSON.stringify(dynamicNames)}`);
assert(dynamicNames.includes('/explain'), `dynamic main skill missing: ${JSON.stringify(dynamicNames)}`);
assert(dynamicNames.filter((name) => name.startsWith('/compact')).length === 1, `built-in collision was duplicated: ${JSON.stringify(dynamicNames)}`);
assert(!dynamicNames.includes('/h'), `built-in alias collision was shown: ${JSON.stringify(dynamicNames)}`);
const reviewOption = slashCommandMenu.children.find((option) => option.children[0].textContent.startsWith('/review'));
assert(reviewOption.children[1].textContent.includes('skill:local') && reviewOption.children[1].textContent.includes('isolated'), 'skill source/execution markers missing');

const invocation = app.matchSkillInvocation('/review "internal config" lifecycle');
assert(invocation && invocation.name === 'review' && invocation.arguments === '"internal config" lifecycle', `exact skill arguments were not preserved: ${JSON.stringify(invocation)}`);
assert(app.matchSkillInvocation('/rev scope') === null, 'skill prefixes should not dispatch');
assert(app.matchSkillInvocation('/tmp/file') === null, 'absolute paths should remain ordinary prompt text');

app.state.streaming = true;
promptInput.value = '/';
promptInput.dispatch('input');
const streamingNames = slashCommandMenu.children.map((option) => option.children[0].textContent);
assert(streamingNames.includes('/review [scope]'), `isolated skill missing while streaming: ${JSON.stringify(streamingNames)}`);
assert(streamingNames.includes('/side'), `streaming-safe built-in missing: ${JSON.stringify(streamingNames)}`);
assert(!streamingNames.includes('/explain') && !streamingNames.includes('/compact'), `unsafe entries shown while streaming: ${JSON.stringify(streamingNames)}`);

console.log('PASS: slash command discovery and keyboard completion');
