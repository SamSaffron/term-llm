#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const source = fs.readFileSync(path.join(__dirname, 'side-question.js'), 'utf8');

class ClassList {
  constructor(element) { this.element = element; }
  values() { return new Set(String(this.element.className || '').split(/\s+/).filter(Boolean)); }
  toggle(token, force) { const values = this.values(); if (force) values.add(token); else values.delete(token); this.element.className = [...values].join(' '); }
  contains(token) { return this.values().has(token); }
}

class Element {
  constructor(tag = 'div') {
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.listeners = {};
    this.className = '';
    this.classList = new ClassList(this);
    this.value = '';
    this.textContent = '';
    this.disabled = false;
    this.scrollHeight = 0;
    this.scrollTop = 0;
    this.clientHeight = 100;
  }
  append(...children) { this.children.push(...children); }
  appendChild(child) { this.children.push(child); return child; }
  replaceChildren(...children) { this.children = [...children]; }
  addEventListener(type, listener) { (this.listeners[type] ||= []).push(listener); }
  async dispatch(type, event = {}) { for (const listener of this.listeners[type] || []) await listener({ type, preventDefault() {}, ...event }); }
  focus() { this.focused = true; }
}

const names = [
  'sideQuestionOverlay', 'sideQuestionStatus', 'sideQuestionMainAttention', 'sideQuestionTranscript',
  'sideQuestionError', 'sideQuestionComposer', 'sideQuestionInput', 'sideQuestionSendBtn',
  'sideQuestionCloseBtn', 'sideQuestionCopyBtn', 'sideQuestionClearBtn', 'sideQuestionCancelBtn',
];
const elements = Object.fromEntries(names.map((name) => [name, new Element(name.includes('Input') ? 'input' : 'div')]));
const document = new Element('document');
document.createElement = (tag) => new Element(tag);
let session = null;
let fetches = [];
const historyView = {
  running: false,
  history: [
    { question: 'first', response: '**safe**' },
    { question: 'second', response: '<img src=x onerror=alert(1)>' },
  ],
};
const fetch = async (url, options = {}) => {
  fetches.push({ url, options });
  return new Response(JSON.stringify(historyView), { status: 200, headers: { 'Content-Type': 'application/json' } });
};
const state = {
  sideQuestion: { visible: false, running: false, question: '', response: '', error: '', usage: {}, history: [], generation: 0 },
  streaming: false,
  draftSessionActive: false,
};
const markdownInputs = [];
const app = {
  UI_PREFIX: '/chat', state, elements,
  getActiveSession: () => session,
  requestHeaders: () => ({}),
  getClipboardWriter: () => ({ writeText: async () => {} }),
  renderAssistantMarkdown: (target, markdown) => {
    markdownInputs.push(markdown);
    target.textContent = String(markdown).replace(/<[^>]*>/g, '');
  },
};
const window = {
  TermLLMApp: app,
  alert() {},
  confirm: () => true,
  setTimeout: (fn) => { fn(); return 1; },
};
const context = { window, document, fetch, Response, TextDecoder, console, setInterval: () => 1 };
context.globalThis = context;
vm.runInNewContext(source, context, { filename: 'side-question.js' });

const assert = (condition, message) => { if (!condition) throw new Error(message); };

(async () => {
  session = { id: 'main' };
  await app.openSideQuestion('');
  assert(state.sideQuestion.visible, '/side did not open the overlay');
  assert(elements.sideQuestionInput.focused, '/side did not focus the side composer');
  assert(elements.sideQuestionTranscript.children.length === 2, 'history was not rendered as two chronological exchanges');
  assert(elements.sideQuestionTranscript.children[0].children[1].textContent === 'first', 'first exchange was not rendered first');
  assert(elements.sideQuestionTranscript.children[1].children[1].textContent === 'second', 'second exchange was not rendered second');
  assert(markdownInputs.includes('<img src=x onerror=alert(1)>'), 'side answer did not use shared Markdown renderer');
  assert(!elements.sideQuestionTranscript.children[1].children[3].textContent.includes('onerror'), 'dangerous HTML survived sanitized renderer');

  elements.sideQuestionInput.value = 'draft';
  await document.dispatch('keydown', { key: 'Escape' });
  assert(state.sideQuestion.visible && elements.sideQuestionInput.value === '', 'first idle Escape did not clear draft in place');
  await document.dispatch('keydown', { key: 'Escape' });
  assert(!state.sideQuestion.visible, 'second idle Escape did not close overlay');

  state.sideQuestion.visible = true;
  state.sideQuestion.running = true;
  state.sideQuestion.question = 'running';
  const before = fetches.length;
  await app.openSideQuestion('must not queue');
  assert(fetches.length === before, 'concurrent side send reached the server');
  assert(state.sideQuestion.error.includes('already running'), 'concurrent side send did not report an error');
  await document.dispatch('keydown', { key: 'Escape' });
  assert(state.sideQuestion.visible && !state.sideQuestion.running, 'running Escape did not cancel while keeping overlay open');
  assert(!elements.sideQuestionComposer.classList.contains('hidden'), 'composer was not restored after cancellation');

  console.log('PASS: side overlay transcript, sanitization, composer, Escape, and concurrency');
})().catch((error) => {
  console.error('FAIL:', error.stack || error);
  process.exit(1);
});
