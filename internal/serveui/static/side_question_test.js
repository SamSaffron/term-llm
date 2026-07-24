#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const source = fs.readFileSync(path.join(__dirname, 'side-question.js'), 'utf8');
const indexHTML = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');

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
    this.hidden = false;
    this.attributes = {};
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
  'sideQuestionOverlay', 'sideQuestionMainAttention', 'sideQuestionTranscript',
  'sideQuestionError', 'sideQuestionComposer', 'sideQuestionInput', 'sideQuestionSendBtn',
  'sideQuestionCloseBtn',
];
const elements = Object.fromEntries(names.map((name) => [name, new Element(name.includes('Input') ? 'input' : 'div')]));
const document = new Element('document');
document.createElement = (tag) => new Element(tag);
let session = { id: 'main' };
let fetches = [];
let failNextPost = false;
let deferNextGet = false;
let resolveDeferredGet = null;
const historyView = {
  running: false,
  question: 'second',
  response: '<img src=x onerror=alert(1)>',
  history: [
    { question: 'first', response: '**safe**' },
    { question: 'second', response: '<img src=x onerror=alert(1)>' },
  ],
};
const fetch = async (url, options = {}) => {
  fetches.push({ url, options });
  if (failNextPost && options.method === 'POST') {
    failNextPost = false;
    return new Response(JSON.stringify({ error: { message: 'side failed' } }), { status: 500, headers: { 'Content-Type': 'application/json' } });
  }
  if (deferNextGet && !options.method) {
    deferNextGet = false;
    return new Promise((resolve) => {
      resolveDeferredGet = () => resolve(new Response(JSON.stringify(historyView), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    });
  }
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
  assert(fetches.length === 0, 'unopened side-question issued an eager startup recovery request');
  session = { id: '3347' };
  const unresolvedStart = fetches.length;
  const unresolved = await app.recoverSideQuestion(session.id);
  assert(unresolved === false, 'numeric route stub recovery should report that identity is unresolved');
  assert(fetches.length === unresolvedStart, 'numeric route stub reached the side-question endpoint');
  session = { id: 'main' };

  assert(!indexHTML.includes('sideQuestionCopyBtn'), 'side overlay retained redundant copy control');
  assert(!indexHTML.includes('sideQuestionClearBtn'), 'side overlay retained clear control');
  assert(!indexHTML.includes('sideQuestionCancelBtn'), 'side overlay retained cancel control');
  assert(!indexHTML.includes('side-question-actions'), 'side overlay retained redundant action row');

  session = { id: 'main' };
  await app.openSideQuestion('');
  assert(state.sideQuestion.visible, '/side did not open the overlay');
  assert(elements.sideQuestionInput.focused, '/side did not focus the side composer');
  assert(elements.sideQuestionTranscript.children.length === 2, 'history was not rendered as two chronological exchanges');
  assert(elements.sideQuestionTranscript.children[0].children[0].children[0].textContent === 'first', 'first exchange was not rendered first');
  assert(elements.sideQuestionTranscript.children[1].children[0].children[0].textContent === 'second', 'second exchange was not rendered second');
  assert(elements.sideQuestionTranscript.children[0].children.length === 2, 'exchange rendered redundant role-label nodes');
  assert(elements.sideQuestionTranscript.children[0].children[0].className === 'message user', 'side user did not use conversation styling');
  assert(elements.sideQuestionTranscript.children[0].children[1].className === 'message assistant', 'side assistant did not use conversation styling');
  assert(markdownInputs.includes('<img src=x onerror=alert(1)>'), 'side answer did not use shared Markdown renderer');
  assert(!elements.sideQuestionTranscript.children[1].children[1].children[0].textContent.includes('onerror'), 'dangerous HTML survived sanitized renderer');

  elements.sideQuestionInput.value = 'draft';
  await document.dispatch('keydown', { key: 'Escape' });
  assert(state.sideQuestion.visible && elements.sideQuestionInput.value === '', 'first idle Escape did not clear draft in place');
  await document.dispatch('keydown', { key: 'Escape' });
  assert(!state.sideQuestion.visible, 'second idle Escape did not close overlay');

  deferNextGet = true;
  const pendingOpen = app.openSideQuestion('');
  await Promise.resolve();
  assert(typeof resolveDeferredGet === 'function', 'deferred side recovery did not start');
  await elements.sideQuestionCloseBtn.dispatch('click');
  resolveDeferredGet();
  await pendingOpen;
  assert(!state.sideQuestion.visible, 'one close click during recovery allowed the side overlay to reopen');

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

  state.sideQuestion.visible = true;
  failNextPost = true;
  const failedFetchStart = fetches.length;
  await app.openSideQuestion('network failure');
  assert(!state.sideQuestion.running && !state.sideQuestion.pending, 'failed POST left side state running or pending');
  assert(state.sideQuestion.error.includes('side failed'), 'failed POST did not expose the server error');
  assert(fetches.slice(failedFetchStart).some((entry) => entry.options.method === 'DELETE' && entry.url.endsWith('/active')), 'failed POST did not clean up the server-side request');

  state.sideQuestion.visible = true;
  state.sideQuestion.running = true;
  const closeFetchStart = fetches.length;
  await elements.sideQuestionCloseBtn.dispatch('click');
  assert(!state.sideQuestion.visible && !state.sideQuestion.running, 'close button did not close and cancel an active side request');
  assert(fetches.slice(closeFetchStart).some((entry) => entry.options.method === 'DELETE' && entry.url.endsWith('/active')), 'closing an active side request did not cancel it on the server');

  console.log('PASS: side overlay transcript, sanitization, composer, Escape, errors, and concurrency');
})().catch((error) => {
  console.error('FAIL:', error.stack || error);
  process.exit(1);
});
