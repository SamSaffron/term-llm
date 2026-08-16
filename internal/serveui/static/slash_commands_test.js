#!/usr/bin/env node
'use strict';

const { Element, assert, loadSource, vm } = require('../testdata/completions_harness.js');
const source = loadSource('slash-commands.js');

const promptInput = new Element({ selection: false });
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


promptInput.value = '/';
promptInput.dispatch('input');
assert(!slashCommandMenu.hidden, 'typing / did not show slash commands');
assert(slashCommandMenu.children.length === 11, 'expected all matching slash commands');
const commandNames = slashCommandMenu.children.map((option) => option.children[0].textContent);
assert(JSON.stringify(commandNames) === JSON.stringify(['/compact', '/fork', '/goal', '/mcp', '/model', '/new', '/redo', '/side', '/thread', '/tree', '/undo']), `commands were not alphabetized: ${JSON.stringify(commandNames)}`);
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
assert(slashCommandMenu.hidden, 'removed /compress alias was still discoverable');

for (const command of commandNames) {
  promptInput.value = command;
  promptInput.dispatch('input');
  const exactEntered = promptInput.dispatch('keydown', { key: 'Enter' });
  assert(!exactEntered.defaultPrevented, `exact ${command} did not propagate Enter for immediate execution`);
  assert(promptInput.value === command, `exact ${command} completion inserted a trailing space`);
  assert(slashCommandMenu.hidden, `exact ${command} left autocomplete open`);
}

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
promptInput.value = '/explain';
promptInput.dispatch('input');
const exactSkillEntered = promptInput.dispatch('keydown', { key: 'Enter' });
assert(!exactSkillEntered.defaultPrevented && promptInput.value === '/explain', 'exact skill command did not execute on the first Enter');

app.state.streaming = true;
promptInput.value = '/';
promptInput.dispatch('input');
const streamingNames = slashCommandMenu.children.map((option) => option.children[0].textContent);
assert(streamingNames.includes('/review [scope]'), `isolated skill missing while streaming: ${JSON.stringify(streamingNames)}`);
assert(streamingNames.includes('/side'), `streaming-safe /side missing: ${JSON.stringify(streamingNames)}`);
assert(streamingNames.includes('/tree'), `streaming-safe /tree missing: ${JSON.stringify(streamingNames)}`);
assert(streamingNames.includes('/thread') && streamingNames.includes('/fork'), `streaming-safe branch shortcuts missing: ${JSON.stringify(streamingNames)}`);
assert(!streamingNames.includes('/explain') && !streamingNames.includes('/compact') && !streamingNames.includes('/undo'), `unsafe entries shown while streaming: ${JSON.stringify(streamingNames)}`);

console.log('PASS: slash command discovery and keyboard completion');
