#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const source = fs.readFileSync(path.join(__dirname, 'app-render.js'), 'utf8');
const markdownStreaming = require(path.join(__dirname, 'markdown-streaming.js'));
const { reconcileToolCallProjection } = require('./transcript-store.js');
let failures = 0;

function fail(name, message, details) {
  console.error('FAIL:', name, '-', message);
  if (details) console.error('      ', details);
  failures += 1;
}

function pass(name) {
  console.log('PASS:', name);
}

function assert(condition, message) {
  if (!condition) throw new Error(message);
}

function assertEqual(actual, expected, message) {
  if (actual !== expected) {
    throw new Error(`${message}: expected ${JSON.stringify(expected)}, got ${JSON.stringify(actual)}`);
  }
}

function parseAttributes(rawAttrs) {
  const attrs = [];
  String(rawAttrs || '').replace(/([A-Za-z_:][-A-Za-z0-9_:.]*)\s*=\s*"([^"]*)"/g, (_match, name, value) => {
    attrs.push([name, value]);
    return '';
  });
  return attrs;
}

function applyParsedAttributes(node, rawAttrs) {
  parseAttributes(rawAttrs).forEach(([name, value]) => node.setAttribute(name, value));
}

class ClassList {
  constructor(element) {
    this.element = element;
  }

  _set(values) {
    this.element.className = Array.from(values).join(' ');
  }

  _values() {
    return new Set(String(this.element.className || '').split(/\s+/).filter(Boolean));
  }

  add(...tokens) {
    const values = this._values();
    tokens.forEach((token) => values.add(token));
    this._set(values);
  }

  remove(...tokens) {
    const values = this._values();
    tokens.forEach((token) => values.delete(token));
    this._set(values);
  }

  contains(token) {
    return this._values().has(token);
  }

  toggle(token, force) {
    const values = this._values();
    const shouldAdd = force === undefined ? !values.has(token) : Boolean(force);
    if (shouldAdd) values.add(token);
    else values.delete(token);
    this._set(values);
    return shouldAdd;
  }
}

class Element {
  constructor(tagName) {
    this.tagName = String(tagName || '').toUpperCase();
    this.children = [];
    this.parentNode = null;
    this.dataset = {};
    this.attributes = new Map();
    this.className = '';
    this.classList = new ClassList(this);
    this.style = {};
    this.listeners = new Map();
    this.textContent = '';
    this._innerHTML = '';
    this.disabled = false;
    this.title = '';
    this.type = '';
  }

  get innerHTML() {
    return this._innerHTML || '';
  }

  set innerHTML(value) {
    this._innerHTML = String(value || '');
    this.children.forEach((child) => { child.parentNode = null; });
    this.children = [];

    const html = this._innerHTML;
    const codeMatch = html.match(/<pre><code(?: class="([^"]*)")?>([\s\S]*?)<\/code><\/pre>/i);
    if (codeMatch) {
      const pre = new Element('pre');
      const code = new Element('code');
      if (codeMatch[1]) code.className = codeMatch[1];
      code.textContent = codeMatch[2];
      pre.textContent = codeMatch[2];
      pre.appendChild(code);
      this.appendChild(pre);
      return;
    }

    const videoMatch = html.match(/<video\b([^>]*)>([\s\S]*?)<\/video>/i);
    if (videoMatch) {
      const video = new Element('video');
      applyParsedAttributes(video, videoMatch[1]);
      const sourceRe = /<source\b([^>]*)>/gi;
      let sourceMatch;
      while ((sourceMatch = sourceRe.exec(videoMatch[2])) !== null) {
        const source = new Element('source');
        applyParsedAttributes(source, sourceMatch[1]);
        video.appendChild(source);
      }
      this.appendChild(video);
    }

    const imgRe = /<img\b([^>]*)>/gi;
    let imgMatch;
    while ((imgMatch = imgRe.exec(html)) !== null) {
      const img = new Element('img');
      applyParsedAttributes(img, imgMatch[1]);
      this.appendChild(img);
    }

    const anchorRe = /<a\b([^>]*)>([\s\S]*?)<\/a>/gi;
    let anchorMatch;
    while ((anchorMatch = anchorRe.exec(html)) !== null) {
      const anchor = new Element('a');
      applyParsedAttributes(anchor, anchorMatch[1]);
      anchor.textContent = anchorMatch[2];
      this.appendChild(anchor);
    }
  }

  get firstElementChild() {
    return this.children[0] || null;
  }

  appendChild(child) {
    if (child.parentNode) {
      const idx = child.parentNode.children.indexOf(child);
      if (idx !== -1) child.parentNode.children.splice(idx, 1);
    }
    child.parentNode = this;
    this.children.push(child);
    return child;
  }

  replaceChildren(...nodes) {
    this.children.forEach((child) => { child.parentNode = null; });
    this.children = [];
    nodes.forEach((node) => { if (node != null) this.appendChild(node); });
  }

    insertBefore(child, reference) {
    if (child.parentNode) {
      const oldSiblings = child.parentNode.children;
      const oldIndex = oldSiblings.indexOf(child);
      if (oldIndex !== -1) oldSiblings.splice(oldIndex, 1);
    }
    child.parentNode = this;
    const index = this.children.indexOf(reference);
    if (index === -1) {
      this.children.push(child);
    } else {
      this.children.splice(index, 0, child);
    }
    return child;
  }

  replaceWith(replacement) {
    if (!this.parentNode) return;
    const siblings = this.parentNode.children;
    const index = siblings.indexOf(this);
    if (index !== -1) {
      replacement.parentNode = this.parentNode;
      siblings[index] = replacement;
      this.parentNode = null;
    }
  }

  remove() {
    if (!this.parentNode) return;
    const siblings = this.parentNode.children;
    const index = siblings.indexOf(this);
    if (index !== -1) siblings.splice(index, 1);
    this.parentNode = null;
  }

  setAttribute(name, value) {
    const key = String(name);
    const strValue = String(value);
    this.attributes.set(key, strValue);
    if (key === 'class') this.className = strValue;
    if (['alt', 'href', 'poster', 'preload', 'src', 'type'].includes(key)) this[key] = strValue;
  }

  getAttribute(name) {
    return this.attributes.get(name) || null;
  }

  hasAttribute(name) {
    return this.attributes.has(name);
  }

  removeAttribute(name) {
    this.attributes.delete(name);
  }

  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) || [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }

  async dispatchEvent(event) {
    const evt = event || { type: '' };
    const listeners = this.listeners.get(evt.type) || [];
    for (const listener of listeners) {
      await listener(evt);
    }
  }

  matches(selector) {
    if (!selector) return false;
    if (selector.startsWith('.')) return this.classList.contains(selector.slice(1));
    if (selector.startsWith('[data-message-id="') && selector.endsWith('"]')) {
      return this.dataset.messageId === selector.slice(18, -2);
    }
    if (selector.startsWith('[data-tool-id="') && selector.endsWith('"]')) {
      return this.dataset.toolId === selector.slice(15, -2);
    }
    return this.tagName.toLowerCase() === selector.toLowerCase();
  }

  querySelectorAll(selector) {
    const selectorText = String(selector || '').trim();
    if (selectorText.includes(' ')) {
      const parts = selectorText.split(/\s+/);
      const leaf = parts[parts.length - 1];
      return this.querySelectorAll(leaf).filter((node) => {
        let ancestor = node.parentNode;
        for (let i = parts.length - 2; i >= 0; i -= 1) {
          while (ancestor && !ancestor.matches(parts[i])) {
            ancestor = ancestor.parentNode;
          }
          if (!ancestor) return false;
          ancestor = ancestor.parentNode;
        }
        return true;
      });
    }

    const results = [];
    const walk = (node) => {
      node.children.forEach((child) => {
        if (child.matches(selectorText)) results.push(child);
        walk(child);
      });
    };
    walk(this);
    return results;
  }

  querySelector(selector) {
    return this.querySelectorAll(selector)[0] || null;
  }
}

function createDocument() {
  const document = {
    head: new Element('head'),
    body: new Element('body'),
    createElement(tagName) { return new Element(tagName); },
    createTextNode(text) {
      const node = new Element('#text');
      node.textContent = String(text || '');
      node.appendData = function appendData(value) {
        this.textContent += String(value || '');
      };
      Object.defineProperty(node, 'data', {
        get() { return this.textContent; },
        set(value) { this.textContent = String(value || ''); },
      });
      return node;
    },
    addEventListener() {},
    querySelector() { return null; },
    querySelectorAll() { return []; },
  };
  document.body.contains = (target) => {
    let node = target;
    while (node) {
      if (node === document.body) return true;
      node = node.parentNode;
    }
    return false;
  };
  return document;
}

function findByMessageId(root, id) {
  if (!root || !id) return null;
  if (root.dataset?.messageId === id) return root;
  for (const child of root.children || []) {
    const found = findByMessageId(child, id);
    if (found) return found;
  }
  return null;
}

function createHarness(appOverrides = {}) {
  const document = createDocument();
  const messages = new Element('div');
  document.body.appendChild(messages);
  const sidebarContent = new Element('div');
  sidebarContent.scrollTop = 0;
  const sessionGroups = new Element('div');
  sidebarContent.appendChild(sessionGroups);
  const session = { id: 's1', title: 'Chat', created: Date.now(), messages: [] };
  const state = { activeSessionId: 's1', sessions: [session], sidebarCollapsed: false };
  const timers = [];
  let timerId = 0;
  const copied = [];
  const parseCalls = [];

  const app = {
    STORAGE_KEYS: { sidebarCollapsed: 'sidebar' },
    UI_PREFIX: '/chat',
    state,
    elements: {
      messages,
      sidebar: new Element('div'),
      sidebarBackdrop: new Element('div'),
      sidebarToggleBtn: new Element('button'),
      sidebarPanelToggleBtn: new Element('button'),
      appShell: new Element('div'),
      activeSessionTitle: new Element('div'),
      widgetsOpenBtn: new Element('button'),
      widgetsModal: new Element('div'),
      widgetsModalList: new Element('div'),
      widgetsModalCloseBtn: new Element('button'),
      sidebarSearchInput: new Element('input'),
      sidebarContent,
      sessionGroups,
    },
    INTERRUPT_BADGE_META: {},
    sanitizeInterruptState(value) { return value || ''; },
    relativeTime() { return 'now'; },
    fullDate() { return 'today'; },
    sessionBucket() { return 'Today'; },
    toolIcon() { return '·'; },
    formatUsage() { return ''; },
    saveSessions() {},
    findMessageElement(id) { return findByMessageId(messages, id); },
    scrollToBottom() {},
    refreshRelativeTimes() {},
    ensureActiveSession() { return session; },
    updateDocumentTitle() {},
    updateSessionUsageDisplay() {},
    renderMath() {},
    markdownStreaming,
    visibleSessions() { return []; },
    sessionHasInProgressState() { return false; },
    setSessionServerActiveRun() {},
    openLightbox() {},
    ...appOverrides,
  };

  const windowObj = {
    TermLLMApp: app,
    TermLLMDecoration: { decorateLightbox() {} },
    matchMedia() { return { matches: false }; },
    setTimeout(callback, ms) {
      const id = ++timerId;
      timers.push({ id, callback, ms, cleared: false });
      return id;
    },
    clearTimeout(id) {
      const timer = timers.find((item) => item.id === id);
      if (timer) timer.cleared = true;
    },
    requestAnimationFrame(callback) { return this.setTimeout(callback, 0); },
    cancelAnimationFrame(id) { this.clearTimeout(id); },
    addEventListener() {},
  };

  const context = {
    window: windowObj,
    document,
    console,
    localStorage: { getItem() { return null; }, setItem() {} },
    navigator: { clipboard: { async writeText(text) { copied.push(text); } } },
    marked: { parse(text) {
      const value = String(text || '');
      parseCalls.push(value);
      const code = value.match(/^```([A-Za-z0-9_-]+)?\n([\s\S]*?)\n```\s*$/);
      if (code) {
        const lang = code[1] ? ` class="language-${code[1]}"` : '';
        return `<pre><code${lang}>${code[2]}</code></pre>`;
      }
      return value;
    } },
    DOMPurify: { sanitize(html) { return String(html || ''); } },
    CSS: { escape(value) { return String(value); } },
    setTimeout: windowObj.setTimeout.bind(windowObj),
    clearTimeout: windowObj.clearTimeout.bind(windowObj),
    ...(appOverrides.IntersectionObserver ? { IntersectionObserver: appOverrides.IntersectionObserver } : {}),
  };
  context.globalThis = context;
  windowObj.document = document;
  windowObj.navigator = context.navigator;
  windowObj.localStorage = context.localStorage;

  vm.runInNewContext(source, context, { filename: 'app-render.js' });
  return { app, session, messages, document, windowObj, timers, copied, parseCalls };
}

function messageNode(id, role) {
  const node = new Element('article');
  node.className = `message ${role}`;
  node.dataset.messageId = id;
  const body = new Element('div');
  body.className = 'message-body';
  const meta = new Element('div');
  meta.className = 'message-meta';
  node.appendChild(body);
  node.appendChild(meta);
  return node;
}

function headAssets(document, tagName) {
  return document.head.children.filter((child) => child.tagName === tagName.toUpperCase());
}

function runNextTimer(timers) {
  const timer = timers.find((item) => !item.cleared);
  assert(timer, 'expected a pending timer');
  timer.cleared = true;
  timer.callback();
  return timer;
}

function runAllPendingTimers(timers, limit = 10) {
  let count = 0;
  while (timers.some((item) => !item.cleared)) {
    assert(count < limit, 'too many pending timers');
    runNextTimer(timers);
    count += 1;
  }
}

async function flushMicrotasks() {
  await Promise.resolve();
  await Promise.resolve();
}

async function run(name, fn) {
  try {
    await fn();
    pass(name);
  } catch (err) {
    fail(name, err.message, err.stack);
  }
}

(async () => {
  await run('sidebar registers shared swipe-to-close gesture', () => {
    const swipeRegistrations = [];
    const { app } = createHarness({
      initPanelSwipeToClose(options) { swipeRegistrations.push(options); },
      setAnimatedPanelOpen({ open, classTargets }) {
        classTargets.forEach((target) => target.element.classList.toggle(target.className, Boolean(open)));
      },
    });

    assertEqual(swipeRegistrations.length, 1, 'one sidebar swipe registration');
    const registration = swipeRegistrations[0];
    assertEqual(registration.panel, app.elements.sidebar, 'gesture is registered on the sidebar');
    assertEqual(registration.side, 'left', 'sidebar closes toward the left edge');
    app.elements.sidebar.classList.add('open');
    app.elements.sidebarBackdrop.classList.add('open');
    registration.onClose();
    assert(!app.elements.sidebar.classList.contains('open'), 'gesture close calls sidebar close helper');
    assert(!app.elements.sidebarBackdrop.classList.contains('open'), 'gesture close also clears backdrop');
  });

  await run('discovers every assistant turn and skips empty assistant segments', () => {
    const { app, session } = createHarness();
    session.messages = [
      { id: 'a0', role: 'assistant', content: 'initial' },
      { id: 'u1', role: 'user', content: 'one' },
      { id: 'a1', role: 'assistant', content: 'first' },
      { id: 'u2', role: 'user', content: 'two' },
      { id: 'a-empty', role: 'assistant', content: '   ' },
      { id: 'u3', role: 'user', content: 'three' },
      { id: 'a2', role: 'assistant', content: 'second' },
    ];
    const turns = app.getAssistantTurns(session);
    assertEqual(turns.length, 3, 'turn count');
    assertEqual(turns[0].lastAssistantId, 'a0', 'initial turn target');
    assertEqual(turns[1].lastAssistantId, 'a1', 'first user-bounded turn target');
    assertEqual(turns[2].lastAssistantId, 'a2', 'second user-bounded turn target');
  });

  await run('renders isolated skill run with cancel and child link', async () => {
    let cancelled = null;
    const { app } = createHarness({
      cancelSkillRun(sessionId, runId) { cancelled = { sessionId, runId }; },
    });
    const node = app.createMessageNode({
      id: 'skill-run-1', role: 'skill-run', runId: 'skill-1', sessionId: 'parent-1', skill: 'review', agent: 'reviewer',
      status: 'running', progress: 'checking diff', output: 'partial review', childSessionId: 'child-1', created: Date.now(),
    });
    assert(node.classList.contains('skill-run'), 'expected distinct skill-run node');
    const button = node.querySelector('button');
    const link = node.querySelector('a');
    const output = node.querySelector('pre');
    assert(button && link && output, 'expected cancel, child link, and output');
    assertEqual(link.href, '/chat/child-1', 'child transcript link');
    assertEqual(output.textContent, 'partial review', 'partial output');
    await button.dispatchEvent({ type: 'click' });
    assertEqual(JSON.stringify(cancelled), JSON.stringify({ sessionId: 'parent-1', runId: 'skill-1' }), 'cancel target');
  });

  await run('rebases assistant markdown image and file links before decoration opens them', () => {
    const { app } = createHarness({
      rebaseHubAssetURL(url) {
        return String(url || '')
          .replace('/ui/images/', '/hub/node/alpha/images/')
          .replace('/ui/files/', '/hub/node/alpha/files/');
      }
    });
    const node = app.createMessageNode({
      id: 'a_img',
      role: 'assistant',
      content: '<img src="/ui/images/from-md.png"><a href="/ui/files/report.pdf">report</a>',
      created: Date.now()
    });
    const img = node.querySelector('img');
    const link = node.querySelector('a');
    assert(img, 'expected markdown img');
    assert(link, 'expected markdown link');
    assertEqual(img.src, '/hub/node/alpha/images/from-md.png', 'markdown image src');
    assertEqual(link.href, '/hub/node/alpha/files/report.pdf', 'markdown file link href');
  });

  await run('rebases deferred assistant video artifact URLs', () => {
    const { app } = createHarness({
      rebaseHubAssetURL(url) {
        return String(url || '')
          .replace('/ui/images/', '/hub/node/alpha/images/')
          .replace('/ui/files/', '/hub/node/alpha/files/');
      }
    });
    const node = app.createMessageNode({
      id: 'a_video',
      role: 'assistant',
      content: '<video src="/ui/files/movie.mp4" poster="/ui/images/poster.png"><source src="/ui/files/movie.webm" type="video/webm"></video>',
      created: Date.now()
    });
    const button = node.querySelector('button');
    const poster = node.querySelector('img');
    assert(button, 'expected deferred video button');
    assert(poster, 'expected deferred video poster');
    assertEqual(button.dataset.videoSrc, '/hub/node/alpha/files/movie.mp4', 'video src');
    assertEqual(button.dataset.videoPoster, '/hub/node/alpha/images/poster.png', 'video poster dataset');
    assertEqual(poster.src, '/hub/node/alpha/images/poster.png', 'video poster img');
    const sources = JSON.parse(button.dataset.videoSources || '[]');
    assertEqual(sources[0]?.src, '/hub/node/alpha/files/movie.webm', 'video source src');
  });

  await run('rebases user attachment image URLs before rendering', () => {
    const { app } = createHarness({
      rebaseHubAssetURL(url) { return String(url || '').replace('/ui/images/', '/hub/node/alpha/images/'); }
    });
    const node = app.createMessageNode({
      id: 'u_img',
      role: 'user',
      content: 'see image',
      created: Date.now(),
      attachments: [{ name: 'image', type: 'image/png', dataURL: '/ui/images/upload.png' }]
    });
    const img = node.querySelector('img');
    assert(img, 'expected attachment img');
    assertEqual(img.src, '/hub/node/alpha/images/upload.png', 'attachment image src');
  });

  await run('rebases tool artifact image URLs before rendering', () => {
    const { app } = createHarness({
      rebaseHubAssetURL(url) { return String(url || '').replace('/ui/images/', '/hub/node/alpha/images/'); }
    });
    const node = app.createToolArtifactsNode({
      role: 'tool-group',
      tools: [{ name: 'image_generate', images: ['/ui/images/generated.png'] }]
    });
    const img = node.querySelector('img');
    assert(img, 'expected artifact img');
    assertEqual(img.src, '/hub/node/alpha/images/generated.png', 'tool artifact image src');
  });

  await run('copies a whole user-bounded turn in message order', () => {
    const { app, session } = createHarness();
    session.messages = [
      { id: 'u1', role: 'user', content: 'question' },
      { id: 'a1', role: 'assistant', content: 'Before tools' },
      { id: 'tg1', role: 'tool-group', tools: [
        { id: 't1', name: 'read_file', status: 'done', arguments: '{"path":"internal/serveui/static/app-render.js","start_line":1}' },
      ] },
      { id: 'a2', role: 'assistant', content: 'After tools' },
      { id: 'u2', role: 'user', content: 'next' },
      { id: 'a3', role: 'assistant', content: 'Next turn' },
    ];
    const turn = app.getAssistantTurns(session).find((item) => item.lastAssistantId === 'a2');
    const text = app.buildTurnClipboardText(turn);
    assert(text.indexOf('Before tools') < text.indexOf('Tools:'), 'assistant text should precede tools');
    assert(text.indexOf('Tools:') < text.indexOf('After tools'), 'tools should precede later assistant text');
    assert(text.includes('- read_file [done]\n  path: internal/serveui/static/app-render.js'), 'tool summary should include prioritized path');
    assert(!text.includes('Next turn'), 'copy should stop at next user boundary');
  });

  await run('tool clipboard summaries are capped at two lines with prioritized keys', () => {
    const { app } = createHarness();
    const lines = app.formatToolClipboardLines({
      name: 'shell',
      status: 'done',
      arguments: JSON.stringify({ command: 'printf "hello\\nworld"', path: '/tmp', extra: 'ignored' }),
    });
    assertEqual(lines.length, 2, 'line cap');
    assertEqual(lines[0], '- shell [done]', 'summary line');
    assert(lines[1].includes('command: printf "hello\\nworld"'), 'detail should include shell command');
    assert(!lines[1].includes('extra:'), 'detail should not include excess keys');
  });

  await run('tool summaries are capped at ten visible arguments', () => {
    const { app } = createHarness();
    const entries = app.formatToolArgs({
      name: 'misc_tool',
      status: 'done',
      arguments: JSON.stringify({
        a1: 'v1', a2: 'v2', a3: 'v3', a4: 'v4', a5: 'v5',
        a6: 'v6', a7: 'v7', a8: 'v8', a9: 'v9', a10: 'v10', a11: 'v11',
      }),
    });

    assertEqual(entries.length, 10, 'tool summaries should show up to ten args');
    assertEqual(entries[9][0], 'a10', 'tenth arg should still be visible');
    assert(!entries.some(([key]) => key === 'a11'), 'eleventh arg should be capped');
  });

  await run('spawn agent tool summaries include model and timeout parameters', () => {
    const { app } = createHarness();
    const entries = app.formatToolArgs({
      name: 'spawn_agent',
      status: 'running',
      arguments: JSON.stringify({
        agent_name: 'codebase',
        prompt: 'Inspect the current uncommitted changes and report risks.',
        model: 'claude-bin:opus-max',
        timeout: 900,
      }),
    });

    assert(entries.some(([key, value]) => key === 'agent' && value === 'codebase'), 'agent should be shown separately');
    assert(entries.some(([key, value]) => key === 'model' && value === 'claude-bin:opus-max'), 'model override should be visible');
    assert(entries.some(([key, value]) => key === 'timeout' && value === '900s'), 'timeout should be visible');
    assert(entries.some(([key, value]) => key === 'task' && String(value).includes('Inspect the current')), 'task should remain visible');
  });

  await run('spawn agent tool summaries hide blank optional parameters', () => {
    const { app } = createHarness();
    const entries = app.formatToolArgs({
      name: 'spawn_agent',
      status: 'running',
      arguments: JSON.stringify({
        agent_name: 'codebase',
        prompt: 'Inspect the code.',
        model: '',
        timeout: 0,
      }),
    });

    assert(entries.some(([key, value]) => key === 'agent' && value === 'codebase'), 'agent should remain visible');
    assert(entries.some(([key, value]) => key === 'task' && value === 'Inspect the code.'), 'task should remain visible');
    assert(!entries.some(([key]) => key === 'model'), 'blank model should be hidden');
    assert(!entries.some(([key]) => key === 'timeout'), 'non-positive timeout should be hidden');
  });

  await run('image generation tool summaries prioritize prompt and hide blanks', () => {
    const { app } = createHarness();
    const entries = app.formatToolArgs({
      name: 'image_generate',
      status: 'done',
      arguments: JSON.stringify({
        aspect_ratio: '1:1',
        input_image: '',
        input_images: [],
        output_path: '',
        prompt: 'a luminous fox under the moon',
      }),
    });

    assertEqual(entries[0][0], 'prompt', 'prompt should be first even when JSON order puts it later');
    assertEqual(entries[0][1], 'a luminous fox under the moon', 'prompt value');
    assert(entries.some(([key, value]) => key === 'aspect_ratio' && value === '1:1'), 'non-blank aspect ratio should remain');
    assert(!entries.some(([key]) => key === 'input_image'), 'blank input_image should be hidden');
    assert(!entries.some(([key]) => key === 'input_images'), 'empty input_images should be hidden');
    assert(!entries.some(([key]) => key === 'output_path'), 'blank output_path should be hidden');
  });

  await run('image generation tool summaries wait for prompt before showing incidental args', () => {
    const { app } = createHarness();
    const entries = app.formatToolArgs({
      name: 'image_generate',
      status: 'running',
      arguments: JSON.stringify({
        aspect_ratio: '4:3',
        input_image: '/root/.local/share/term-llm/uploads/image.png',
        input_images: [],
        output_path: '',
      }),
    });

    assertEqual(entries.length, 0, 'image args without prompt should be hidden');
  });

  await run('image generation clipboard summaries do not fall back to hidden raw args', () => {
    const { app } = createHarness();
    const lines = app.formatToolClipboardLines({
      name: 'image_generate',
      status: 'running',
      arguments: JSON.stringify({
        aspect_ratio: '4:3',
        input_image: '/root/.local/share/term-llm/uploads/image.png',
        input_images: [],
        output_path: '',
      }),
    });

    assertEqual(lines.length, 1, 'clipboard should include only summary line when image prompt is unavailable');
    assert(!lines.join('\n').includes('/root/.local/share'), 'clipboard should not include hidden internal upload path');
  });

  await run('image generation tool summaries describe attachments without internal upload paths', () => {
    const { app } = createHarness();
    const entries = app.formatToolArgs({
      name: 'image_generate',
      status: 'running',
      arguments: JSON.stringify({
        prompt: 'turn this sketch into watercolor',
        aspect_ratio: '4:3',
        input_image: '/root/.local/share/term-llm/uploads/image.png',
      }),
    });

    assert(entries.some(([key, value]) => key === 'input' && value === '1 attached image'), 'attached image should be summarized');
    assert(!entries.some(([, value]) => String(value).includes('/root/.local/share')), 'internal upload path should not be shown');
  });

  await run('tool image artifacts render on tool group without assistant markdown', () => {
    const { app } = createHarness();
    const group = {
      id: 'group_img',
      role: 'tool-group',
      status: 'done',
      tools: [{
        id: 'call_img',
        name: 'image_generate',
        status: 'done',
        arguments: '{"prompt":"paint a cat"}',
        images: ['/ui/images/generated.png'],
      }],
    };

    const node = app.createToolGroupNode(group);
    const artifacts = node.querySelector('.tool-artifacts');
    assert(artifacts, 'artifact strip should render');
    const img = artifacts.querySelector('img');
    assert(img, 'artifact image should render');
    assertEqual(img.src, '/ui/images/generated.png', 'artifact image src');
    assertEqual(node.querySelectorAll('.markdown-body').length, 0, 'tool artifact should not create assistant markdown');
  });

  await run('askUser messages do not split assistant turns', () => {
    const { app, session } = createHarness();
    session.messages = [
      { id: 'u1', role: 'user', content: 'start' },
      { id: 'a1', role: 'assistant', content: 'Part one' },
      { id: 'ask', role: 'user', askUser: true, content: 'Need input' },
      { id: 'tg1', role: 'tool-group', tools: [
        { id: 't1', name: 'web_search', status: 'done', arguments: '{"query":"term llm"}' },
      ] },
      { id: 'a2', role: 'assistant', content: 'Part two' },
    ];
    const turns = app.getAssistantTurns(session);
    assertEqual(turns.length, 1, 'askUser should not create boundary');
    assertEqual(turns[0].lastAssistantId, 'a2', 'last assistant after askUser');
    const text = app.buildTurnClipboardText(turns[0]);
    assert(text.includes('Part one'), 'first assistant text included');
    assert(text.includes('- web_search [done]\n  query: term llm'), 'tool after askUser included');
    assert(text.includes('Part two'), 'later assistant text included');
  });

  await run('syncs one copy panel per assistant turn and click copies target turn', async () => {
    const { app, session, messages, timers, copied } = createHarness();
    session.messages = [
      { id: 'u1', role: 'user', content: 'question' },
      { id: 'a1', role: 'assistant', content: 'Earlier assistant' },
      { id: 'tg1', role: 'tool-group', tools: [
        { id: 't1', name: 'grep', status: 'done', arguments: '{"pattern":"needle","path":"src"}' },
      ] },
      { id: 'a2', role: 'assistant', content: 'Final assistant in first turn' },
      { id: 'u2', role: 'user', content: 'second' },
      { id: 'a3', role: 'assistant', content: 'Second turn answer' },
    ];
    ['a1', 'a2', 'a3'].forEach((id) => messages.appendChild(messageNode(id, 'assistant')));

    app.syncTurnActionPanels();
    app.syncTurnActionPanels();

    assertEqual(messages.querySelectorAll('.turn-action-panel').length, 2, 'panel count after resync');
    assertEqual(messages.children[0].querySelectorAll('.turn-action-panel').length, 0, 'earlier assistant in same turn has no panel');
    assertEqual(messages.children[1].querySelectorAll('.turn-action-panel').length, 1, 'first turn panel on last assistant');
    assertEqual(messages.children[2].querySelectorAll('.turn-action-panel').length, 1, 'second turn panel');

    const button = messages.children[1].querySelector('.turn-copy-btn');
    await button.dispatchEvent({ type: 'click', preventDefault() {} });
    assertEqual(copied.length, 1, 'clipboard writes');
    assert(copied[0].includes('Earlier assistant'), 'copied earlier assistant text');
    assert(copied[0].includes('Final assistant in first turn'), 'copied final assistant text');
    assert(copied[0].includes('- grep [done]\n  pattern: needle · path: src'), 'copied tool summary');
    assert(!copied[0].includes('Second turn answer'), 'did not copy later turn');
    assert(button.classList.contains('copied'), 'button gets copied class');

    const reset = timers.find((timer) => timer.ms === 1500 && !timer.cleared);
    assert(reset, 'reset timer scheduled');
    reset.callback();
    assert(!button.classList.contains('copied'), 'copied class resets');
  });

  await run('turn copy uses app clipboard fallback writer when navigator clipboard is unavailable', async () => {
    const fallbackWrites = [];
    const { app, session, messages } = createHarness({
      getClipboardWriter() {
        return { async writeText(text) { fallbackWrites.push(text); } };
      },
    });
    session.messages = [
      { id: 'u1', role: 'user', content: 'question' },
      { id: 'a1', role: 'assistant', content: 'Fallback copy answer' },
    ];
    messages.appendChild(messageNode('a1', 'assistant'));

    app.syncTurnActionPanels();
    const button = messages.children[0].querySelector('.turn-copy-btn');
    assert(button && !button.disabled, 'fallback writer keeps copy button enabled');
    await button.dispatchEvent({ type: 'click', preventDefault() {} });

    assertEqual(fallbackWrites.length, 1, 'fallback writer copies turn');
    assertEqual(fallbackWrites[0], 'Fallback copy answer', 'fallback writer receives turn text');
  });

  await run('turn copy button disables when no clipboard writer exists', () => {
    const { app, session, messages } = createHarness({ getClipboardWriter: () => null });
    session.messages = [
      { id: 'u1', role: 'user', content: 'question' },
      { id: 'a1', role: 'assistant', content: 'No writer answer' },
    ];
    messages.appendChild(messageNode('a1', 'assistant'));

    app.syncTurnActionPanels();
    const button = messages.children[0].querySelector('.turn-copy-btn');
    assert(button && button.disabled, 'copy button disables without clipboard writer');
    assertEqual(button.title, 'Clipboard unavailable', 'disabled button explains unavailable clipboard');
  });

  await run('code copy clears stale reset timers on rapid clicks', async () => {
    const { app, document, copied, timers } = createHarness();
    const target = new Element('div');
    document.body.appendChild(target);

    app.renderAssistantMarkdown(target, '```js\nconsole.log(1);\n```');
    const button = target.querySelector('.code-copy-btn');
    assert(button && !button.disabled, 'code copy button is enabled');

    await button.dispatchEvent({ type: 'click' });
    const firstReset = timers.find((timer) => timer.ms === 1500 && !timer.cleared);
    assert(firstReset, 'first reset timer scheduled');
    await button.dispatchEvent({ type: 'click' });

    assertEqual(copied.length, 2, 'both rapid clicks copy');
    assert(firstReset.cleared, 'first reset timer cleared by second click');
    const activeResets = timers.filter((timer) => timer.ms === 1500 && !timer.cleared);
    assertEqual(activeResets.length, 1, 'only latest reset timer remains active');
  });

  await run('math copy controls copy rendered TeX source as text', async () => {
    const { app, document, copied, timers } = createHarness();
    const root = document.createElement('div');
    const display = document.createElement('span');
    display.className = 'katex-display';
    const katex = document.createElement('span');
    katex.className = 'katex';
    const annotation = document.createElement('annotation');
    annotation.textContent = '\\ell_P = \\sqrt{\\hbar G / c^3}';
    katex.appendChild(annotation);
    display.appendChild(katex);
    root.appendChild(display);

    app.decorateMathCopyControls(root);
    app.decorateMathCopyControls(root);

    const buttons = display.querySelectorAll('.math-copy-btn');
    assertEqual(buttons.length, 1, 'decorator is idempotent');
    await buttons[0].dispatchEvent({ type: 'click', preventDefault() {}, stopPropagation() {} });

    assertEqual(copied.length, 1, 'clipboard writes');
    assertEqual(copied[0], '\\ell_P = \\sqrt{\\hbar G / c^3}', 'copies TeX source');
    assert(buttons[0].classList.contains('copied'), 'button gets copied state');
    const reset = timers.find((timer) => timer.ms === 1500 && !timer.cleared);
    assert(reset, 'reset timer scheduled');
  });

  await run('stream updates only resync the affected turn action panel', () => {
    const { app, session, messages } = createHarness();
    const streamingMessage = {
      id: 'a3',
      role: 'assistant',
      content: '',
      created: Date.now(),
    };
    session.messages = [
      { id: 'u1', role: 'user', content: 'first' },
      { id: 'a1', role: 'assistant', content: 'First turn answer' },
      { id: 'u2', role: 'user', content: 'second' },
      { id: 'a2', role: 'assistant', content: 'Earlier assistant in active turn' },
      { id: 'tg1', role: 'tool-group', tools: [
        { id: 't1', name: 'grep', status: 'done', arguments: '{"pattern":"needle"}' },
      ] },
      streamingMessage,
    ];
    ['a1', 'a2', 'a3'].forEach((id) => messages.appendChild(messageNode(id, 'assistant')));

    app.syncTurnActionPanels();

    const firstTurnPanel = messages.children[0].querySelector('.turn-action-panel');
    const activeTurnPanel = messages.children[1].querySelector('.turn-action-panel');
    assert(firstTurnPanel, 'first turn panel rendered');
    assert(activeTurnPanel, 'active turn panel starts on earlier assistant while stream is empty');
    assertEqual(messages.children[2].querySelectorAll('.turn-action-panel').length, 0, 'empty streaming assistant has no panel yet');

    streamingMessage.content = 'Streaming reply';
    app.enqueueAssistantStreamUpdate(streamingMessage);

    const streamedPanel = messages.children[2].querySelector('.turn-action-panel');
    assertEqual(messages.children[0].querySelector('.turn-action-panel'), firstTurnPanel, 'unrelated earlier turn panel preserved');
    assertEqual(messages.children[1].querySelectorAll('.turn-action-panel').length, 0, 'panel removed from earlier assistant in same turn');
    assert(streamedPanel, 'panel moved onto streaming assistant');

    streamingMessage.content += ' with more text';
    app.enqueueAssistantStreamUpdate(streamingMessage);

    assertEqual(messages.children[0].querySelector('.turn-action-panel'), firstTurnPanel, 'earlier turn panel still preserved on later stream ticks');
    assertEqual(messages.children[2].querySelector('.turn-action-panel'), streamedPanel, 'streaming assistant panel is reused across ticks');
  });

  await run('plain streaming tail appends text node data instead of replacing full text', () => {
    const { app, document, timers } = createHarness();
    const appended = [];
    document.createTextNode = (text) => {
      const node = new Element('#text');
      node.textContent = String(text || '');
      node.appendData = (value) => {
        appended.push(String(value || ''));
        node.textContent += String(value || '');
      };
      Object.defineProperty(node, 'data', {
        get() { return node.textContent; },
        set(value) { node.textContent = String(value || ''); },
      });
      return node;
    };

    const message = { id: 'stream-plain', role: 'assistant', content: 'Hello', created: Date.now() };
    app.enqueueAssistantStreamUpdate(message);
    runAllPendingTimers(timers);

    message.content = 'Hello world';
    app.enqueueAssistantStreamUpdate(message);
    runAllPendingTimers(timers);

    assertEqual(appended.length, 1, 'one incremental text append');
    assertEqual(appended[0], ' world', 'only new delta is appended');
  });

  await run('stream render cadence uses mutable tail length after stable markdown promotion', () => {
    const observedLengths = [];
    const streamingHelpers = {
      ...markdownStreaming,
      nextStreamingRenderDelay(length) {
        observedLengths.push(length);
        return markdownStreaming.nextStreamingRenderDelay(length);
      },
    };
    const { app, timers } = createHarness({ markdownStreaming: streamingHelpers });
    const paragraph = 'Stable paragraph with enough prose to be markdown-rendered later.\n\n';
    const stablePrefix = paragraph.repeat(180);
    const message = {
      id: 'stream-stable-tail',
      role: 'assistant',
      content: `${stablePrefix}Live markdown tail with **formatting** that keeps streaming.`,
      created: Date.now(),
    };

    app.enqueueAssistantStreamUpdate(message);
    runAllPendingTimers(timers);

    message.content += ' more plain tail';
    app.enqueueAssistantStreamUpdate(message);

    const lastObservedLength = observedLengths[observedLengths.length - 1];
    assert(lastObservedLength > 0, 'scheduler should inspect a non-empty mutable tail');
    assert(
      lastObservedLength < 1000,
      `expected cadence to use mutable tail length, got ${lastObservedLength}`
    );
  });

  await run('successful update_plan calls stay out of the transcript while running and failed calls remain visible', () => {
    const { app, messages } = createHarness();
    const tool = {
      id: 'plan_1',
      name: 'update_plan',
      status: 'done',
      resultStatus: 'success',
      arguments: JSON.stringify({
        explanation: 'historical detail should not be rendered',
        plan: [
          { step: 'Inspect patterns', status: 'completed' },
          { step: 'Add tests', status: 'completed' },
          { step: 'Run suite', status: 'pending' },
        ],
      }),
      argumentsFinalized: true,
    };
    const group = { id: 'plan_group', role: 'tool-group', status: 'done', tools: [tool], expanded: false };
    const node = app.createToolGroupNode(group);
    messages.appendChild(node);

    assert(node.hidden, 'successful plan-only group should be hidden');
    assertEqual(messages.querySelectorAll('.plan-update-event').length, 0, 'no plan event row');
    assertEqual(messages.querySelectorAll('.tool-group-entry').length, 0, 'no generic successful plan row');
    assertEqual(app.formatToolClipboardLines(tool).length, 0, 'successful plan update is omitted from clipboard history');
    const copiedTurn = app.buildTurnClipboardText({
      items: [
        { role: 'user', content: 'Plan this' },
        group,
        { role: 'assistant', content: 'Done' },
      ],
    });
    assert(!copiedTurn.includes('Tools:'), 'plan-only turns should not leave an empty clipboard tools section');

    const standalone = {
      ...tool,
      id: 'plan_standalone',
      role: 'tool',
    };
    const standaloneNode = app.createToolCard(standalone);
    messages.appendChild(standaloneNode);
    assert(standaloneNode.hidden, 'legacy standalone successful plan row should be hidden');
    standalone.status = 'running';
    delete standalone.resultStatus;
    app.updateToolNode(standalone);
    assert(!standaloneNode.hidden, 'legacy standalone running plan row should remain visible');
    standalone.status = 'error';
    standalone.resultStatus = 'error';
    app.updateToolNode(standalone);
    assert(!standaloneNode.hidden, 'legacy standalone failed plan row should remain visible');

    tool.arguments = JSON.stringify({ plan: [] });
    app.updateToolGroupNode(group);
    assert(node.hidden, 'successful clear should also stay out of transcript');

    tool.status = 'running';
    delete tool.resultStatus;
    tool.arguments = JSON.stringify({ plan: [{ step: 'Speculative', status: 'in_progress' }] });
    app.updateToolGroupNode(group);
    assert(!node.hidden, 'running plan update should retain ordinary tool feedback');
    assertEqual(messages.querySelectorAll('.tool-group-entry').length, 1, 'running call stays generic');

    tool.status = 'done';
    tool.resultStatus = 'success';
    app.updateToolGroupNode(group);
    assert(node.hidden, 'confirmed success removes the running row');
    assertEqual(messages.querySelectorAll('.tool-group-entry').length, 0, 'success transition leaves no transcript row');

    tool.status = 'error';
    tool.resultStatus = 'error';
    app.updateToolGroupNode(group);
    assert(!node.hidden, 'failed update remains visible');
    assertEqual(messages.querySelectorAll('.tool-group-entry').length, 1, 'failed call uses generic tool row');
    assertEqual(messages.querySelector('.tool-entry-status').textContent, '×', 'failed row keeps failure marker');
  });

  await run('mixed tool groups omit successful plan calls from counts and order', () => {
    const { app, messages } = createHarness();
    const group = {
      id: 'mixed_plan_group',
      role: 'tool-group',
      status: 'done',
      expanded: true,
      tools: [
        { id: 'before', name: 'grep', status: 'done', resultStatus: 'success', arguments: '{"pattern":"x"}' },
        { id: 'plan', name: 'update_plan', status: 'done', resultStatus: 'success', arguments: '{"plan":[{"step":"Work","status":"completed"}]}' },
        { id: 'after', name: 'shell', status: 'done', resultStatus: 'success', arguments: '{"command":"go test ./..."}' },
      ],
    };
    const node = app.createToolGroupNode(group);
    messages.appendChild(node);
    const details = messages.querySelector('.tool-group-details');
    assert(!node.hidden, 'mixed group remains visible for other tools');
    assertEqual(details.children.map((entry) => entry.dataset.toolId).join(','), 'before,after', 'plan call is omitted without reordering siblings');
    assertEqual(messages.querySelector('.tool-group-summary').textContent, '2 tool calls completed', 'summary excludes plan update');
    app.updateToolGroupNode(group);
    assertEqual(details.children.map((entry) => entry.dataset.toolId).join(','), 'before,after', 'reconciliation keeps one copy of visible siblings');
    assertEqual(messages.querySelectorAll('.plan-update-event').length, 0, 'reconciliation never adds a plan event');
  });

  await run('done finalized tool args are not rebuilt on later group updates', () => {
    const { app, messages } = createHarness();
    const tool = {
      id: 'tool_done',
      name: 'grep',
      status: 'done',
      arguments: '{"pattern":"needle"}',
      argumentsFinalized: true,
    };
    const group = {
      id: 'group_done',
      role: 'tool-group',
      status: 'done',
      tools: [tool],
    };
    messages.appendChild(app.createToolGroupNode(group));

    const entry = messages.querySelector('[data-tool-id="tool_done"]');
    const argsBefore = entry && entry.querySelector('.tool-entry-args');
    assert(argsBefore, 'initial args node rendered');

    app.updateToolGroupNode(group);

    const argsAfter = entry.querySelector('.tool-entry-args');
    assertEqual(argsAfter, argsBefore, 'finalized done args node is reused');

    tool.arguments = '{"pattern":"changed"}';
    tool.argumentsFinalized = false;
    app.updateToolGroupNode(group);

    const argsRebuilt = entry.querySelector('.tool-entry-args');
    assert(argsRebuilt, 'args node still present after rebuild');
    assert(argsRebuilt !== argsBefore, 'non-finalized done args can still be rebuilt');
  });

  await run('renders markdown without eager optional libraries for plain markdown', () => {
    const { app, document } = createHarness();
    const target = new Element('div');
    document.body.appendChild(target);

    app.renderAssistantMarkdown(target, 'Plain **markdown** without math or code.');

    assert(target.innerHTML.includes('Plain **markdown**'), 'plain markdown rendered');
    assertEqual(headAssets(document, 'script').length, 0, 'no optional scripts loaded');
    assertEqual(headAssets(document, 'link').length, 0, 'no optional styles loaded');
  });

  await run('math markdown triggers lazy KaTeX loader', async () => {
    const { app, document } = createHarness();
    const target = new Element('div');
    document.body.appendChild(target);

    app.renderAssistantMarkdown(target, 'Value: \\(x + y\\)');

    const initialScripts = headAssets(document, 'script').map((node) => node.src);
    const initialStyles = headAssets(document, 'link').map((node) => node.href);
    assert(initialScripts.includes('vendor/katex/katex.min.js?v=0.16.38'), 'KaTeX script requested');
    assert(initialStyles.includes('vendor/katex/katex.min.css?v=0.16.38'), 'KaTeX stylesheet requested');
    assert(!initialScripts.includes('vendor/hljs/highlight.min.js?v=11.11.1'), 'highlight.js not requested for math');

    const katexScript = headAssets(document, 'script').find((node) => node.src === 'vendor/katex/katex.min.js?v=0.16.38');
    katexScript.onload();
    await flushMicrotasks();
    const scriptsAfterKatex = headAssets(document, 'script').map((node) => node.src);
    assert(scriptsAfterKatex.includes('vendor/katex/auto-render.min.js?v=0.16.38'), 'KaTeX auto-render script requested after core load');
  });

  await run('code blocks trigger lazy highlight.js loader', () => {
    const { app, document } = createHarness();
    const target = new Element('div');
    document.body.appendChild(target);

    app.renderAssistantMarkdown(target, '```js\nconsole.log(1);\n```');

    const scripts = headAssets(document, 'script').map((node) => node.src);
    const styles = headAssets(document, 'link').map((node) => `${node.href}|${node.media || ''}`);
    assert(scripts.includes('vendor/hljs/highlight.min.js?v=11.11.1'), 'highlight.js script requested');
    assert(styles.includes('vendor/hljs/github-dark.min.css?v=11.11.1|'), 'dark highlight stylesheet requested');
    assert(styles.includes('vendor/hljs/github.min.css?v=11.11.1|(prefers-color-scheme: light)'), 'light highlight stylesheet requested');
  });

  await run('streaming markdown preserves stable container across tail updates', () => {
    const { app, session, messages, timers } = createHarness();
    const message = {
      id: 'stream1',
      role: 'assistant',
      content: `First paragraph with **bold**.\n\n${'tail '.repeat(80)}`,
      created: Date.now(),
    };
    session.messages = [message];

    app.enqueueAssistantStreamUpdate(message);
    runAllPendingTimers(timers);

    const node = messages.children[0];
    const body = node.querySelector('.message-body');
    const stable = body.querySelector('.markdown-stream-stable');
    const tail = body.querySelector('.markdown-stream-tail');
    assert(stable, 'stable container created');
    assert(tail, 'tail container created');
    assertEqual(stable.children.length, 1, 'one stable piece promoted');
    const stablePiece = stable.children[0];

    message.content += 'more tail content with **markdown**';
    app.enqueueAssistantStreamUpdate(message);
    runAllPendingTimers(timers);

    assertEqual(stable.children[0], stablePiece, 'stable DOM piece should be preserved');
    assert(tail.innerHTML.includes('more tail content'), 'tail rerendered with appended content');
  });

  await run('stable-prefix hash detects middle replacement and rebuilds promoted markdown', () => {
    const { app, session, messages, timers } = createHarness();
    const stablePrefix = Array.from(
      { length: 20 },
      (_, index) => `Stable paragraph ${index} with enough prose for promotion.\n\n`
    ).join('');
    const message = {
      id: 'stream-stable-middle-replacement',
      role: 'assistant',
      content: `${stablePrefix}${'mutable **tail** '.repeat(30)}`,
      created: Date.now(),
    };
    session.messages = [message];

    app.enqueueAssistantStreamUpdate(message);
    runAllPendingTimers(timers);

    const body = messages.children[0].querySelector('.message-body');
    const stable = body.querySelector('.markdown-stream-stable');
    assertEqual(stable.children.length, 1, 'stable prefix promoted before replacement');
    const originalPiece = stable.children[0];
    const divergence = Math.floor(stablePrefix.length / 2);
    assert(divergence > 64 && divergence < stablePrefix.length - 64, 'divergence is outside the old edge guards');

    const replacementPrefix = `${stablePrefix.slice(0, divergence)}X${stablePrefix.slice(divergence + 1)}`;
    message.content = `${replacementPrefix}${'mutable **tail** '.repeat(30)}`;
    app.enqueueAssistantStreamUpdate(message);
    runAllPendingTimers(timers);

    assertEqual(stable.children.length, 1, 'corrected stable prefix is promoted again');
    assert(stable.children[0] !== originalPiece, 'middle divergence invalidates the old stable DOM piece');
    assert(stable.children[0].innerHTML.includes(replacementPrefix.slice(divergence - 12, divergence + 12)), 'rebuilt stable DOM contains replacement content');
  });

  await run('plain fallback resets stale stable DOM before slicing a divergent snapshot', () => {
    const { app, session, messages, timers, parseCalls } = createHarness();
    const stablePrefix = Array.from(
      { length: 20 },
      (_, index) => `Promoted paragraph ${index} with enough prose for recovery.\n\n`
    ).join('');
    const message = {
      id: 'stream-fallback-stable-recovery',
      role: 'assistant',
      content: `${stablePrefix}${'mutable **tail** '.repeat(30)}`,
      created: Date.now(),
    };
    session.messages = [message];

    app.enqueueAssistantStreamUpdate(message);
    runAllPendingTimers(timers);

    const body = messages.children[0].querySelector('.message-body');
    const stable = body.querySelector('.markdown-stream-stable');
    assertEqual(stable.children.length, 1, 'stable prefix promoted before fallback');

    const oversizedTail = '```txt\n' + 'unfinished **markdown** inside fence\n'.repeat(2500);
    assert(oversizedTail.length > markdownStreaming.MAX_MUTABLE_MARKDOWN_CHARS, 'fallback fixture exceeds mutable limit');
    const divergence = Math.floor(stablePrefix.length / 2);
    const replacementPrefix = `${stablePrefix.slice(0, divergence)}X${stablePrefix.slice(divergence + 1)}`;
    const parsesBeforeRecovery = parseCalls.length;
    message.content = `${replacementPrefix}${oversizedTail}`;
    app.enqueueAssistantStreamUpdate(message);
    runAllPendingTimers(timers);

    const recoveredTail = body.querySelector('.markdown-stream-tail');
    assertEqual(stable.children.length, 0, 'stale stable DOM is cleared on fallback prefix mismatch');
    assertEqual(recoveredTail.children[0].textContent, message.content, 'fallback restarts from the full corrected snapshot');
    assertEqual(parseCalls.length, parsesBeforeRecovery, 'fallback recovery does not parse the oversized snapshot as Markdown');
  });

  await run('finalizing streaming markdown replaces streaming containers with full render', () => {
    const { app, session, messages, timers } = createHarness();
    const message = {
      id: 'stream-final',
      role: 'assistant',
      content: `First paragraph with **bold**.\n\n${'tail '.repeat(80)}`,
      created: Date.now(),
    };
    session.messages = [message];

    app.enqueueAssistantStreamUpdate(message);
    runAllPendingTimers(timers);
    let body = messages.children[0].querySelector('.message-body');
    assert(body.querySelector('.markdown-stream-tail'), 'tail exists before final render');

    app.finalizeAssistantStreamRender(message);
    body = messages.children[0].querySelector('.message-body');
    assert(!body.querySelector('.markdown-stream-tail'), 'tail removed after final render');
    assert(!body.querySelector('.markdown-stream-stable'), 'stable container removed after final render');
    assert(body.innerHTML.includes('First paragraph'), 'full markdown render remains');
  });

  await run('oversized incomplete markdown tail uses bounded incremental fallback and finalizes correctly', () => {
    const observedScheduleLengths = [];
    const streamingHelpers = {
      ...markdownStreaming,
      nextStreamingRenderDelay(length) {
        observedScheduleLengths.push(length);
        return markdownStreaming.nextStreamingRenderDelay(length);
      },
    };
    const { app, session, messages, timers, parseCalls, windowObj } = createHarness({ markdownStreaming: streamingHelpers });
    windowObj.__TERM_LLM_DIAGNOSTICS__ = true;
    const oversizedFence = '```txt\n' + 'unfinished **markdown** inside fence\n'.repeat(2500);
    assert(oversizedFence.length > markdownStreaming.MAX_MUTABLE_MARKDOWN_CHARS, 'fixture must exceed mutable markdown limit');
    const message = {
      id: 'stream-oversized-fence',
      role: 'assistant',
      content: oversizedFence,
      created: Date.now(),
    };
    session.messages = [message];

    app.enqueueAssistantStreamUpdate(message);
    runAllPendingTimers(timers);

    let body = messages.children[0].querySelector('.message-body');
    const tail = body.querySelector('.markdown-stream-tail');
    assert(tail.className.includes('streaming-plain-text'), 'oversized incomplete tail falls back to plain streaming');
    assertEqual(tail.dataset.streamRenderMode, 'plain-fallback', 'fallback mode is exposed for diagnostics');
    assertEqual(Number(tail.dataset.mutableMarkdownTailSize), 0, 'no oversized tail remains mutable markdown work');
    const parsesBeforeAppend = parseCalls.length;
    const textNode = tail.children[0];
    observedScheduleLengths.length = 0;

    message.content += 'one more streamed delta';
    app.enqueueAssistantStreamUpdate(message);
    const fallbackScheduleLength = observedScheduleLengths[observedScheduleLengths.length - 1];
    assert(fallbackScheduleLength > markdownStreaming.MAX_MUTABLE_MARKDOWN_CHARS, 'fallback scheduler receives the actual huge mutable tail length');
    assert(markdownStreaming.nextStreamingRenderDelay(fallbackScheduleLength) > 33, 'huge fallback tail retains slow render cadence');
    runAllPendingTimers(timers);

    assertEqual(parseCalls.length, parsesBeforeAppend, 'fallback append does not reparse the oversized markdown source');
    assert(textNode.textContent.endsWith('one more streamed delta'), 'fallback appends only the streamed delta');

    message.content += '\n```';
    app.finalizeAssistantStreamRender(message);
    body = messages.children[0].querySelector('.message-body');
    assert(!body.querySelector('.markdown-stream-tail'), 'streaming fallback removed after final render');
    assertEqual(parseCalls[parseCalls.length - 1], message.content, 'final render reparses the complete source for correctness');
  });

  await run('plain text streaming renders tail as text node and stays in that mode across extends', () => {
    const { app, session, messages, timers } = createHarness();
    const message = {
      id: 'plain-cache',
      role: 'assistant',
      content: 'Hello world, this is plain text.',
      created: Date.now(),
    };
    session.messages = [message];

    app.enqueueAssistantStreamUpdate(message);
    runAllPendingTimers(timers);

    const node = messages.children[0];
    const body = node.querySelector('.message-body');
    const tail = body.querySelector('.markdown-stream-tail');
    assert(tail, 'tail container exists');
    assert(tail.className.includes('streaming-plain-text'), 'tail uses plain-text mode for plain text');
    assertEqual(tail.dataset.streamRenderMode, undefined, 'stream diagnostics stay unset without opt-in');

    // Extend with more plain text (no markdown chars) — cache fast path
    message.content += ' More words with no special characters at all.';
    app.enqueueAssistantStreamUpdate(message);
    runAllPendingTimers(timers);

    assert(tail.className.includes('streaming-plain-text'), 'tail stays in plain-text mode after plain extend');
    // The plain-text path writes into a child text node, not the container's own textContent
    const textNode = tail.children[0];
    assert(textNode && textNode.textContent.includes('More words'), 'extended content is rendered in text node');
  });

  await run('plain text streaming switches to markdown tail when markdown chars arrive', () => {
    const { app, session, messages, timers } = createHarness();
    const message = {
      id: 'plain-to-md',
      role: 'assistant',
      content: 'Plain text so far.',
      created: Date.now(),
    };
    session.messages = [message];

    app.enqueueAssistantStreamUpdate(message);
    runAllPendingTimers(timers);

    const body = messages.children[0].querySelector('.message-body');
    const tail = body.querySelector('.markdown-stream-tail');
    assert(tail.className.includes('streaming-plain-text'), 'plain-text mode initially');

    // Append markdown — cache must invalidate and run full check
    message.content += ' Now **bold** text appears.';
    app.enqueueAssistantStreamUpdate(message);
    runAllPendingTimers(timers);

    assert(!tail.className.includes('streaming-plain-text'), 'tail leaves plain-text mode once markdown arrives');
  });

  await run('directionForText returns ltr for latin text', () => {
    const { app } = createHarness();
    assertEqual(app.directionForText('Hello world'), 'ltr', 'latin text');
    assertEqual(app.directionForText('Café'), 'ltr', 'accented latin');
  });

  await run('directionForText returns rtl for RTL-first text', () => {
    const { app } = createHarness();
    // Hebrew character א (alef)
    assertEqual(app.directionForText('אבג'), 'rtl', 'Hebrew text');
    // Arabic character ا (alef)
    assertEqual(app.directionForText('ابت'), 'rtl', 'Arabic text');
  });

  await run('directionForText returns auto when no strong bidi chars present', () => {
    const { app } = createHarness();
    assertEqual(app.directionForText(''), 'auto', 'empty string');
    assertEqual(app.directionForText('123 !@# ...'), 'auto', 'digits and punctuation only');
  });

  await run('directionForText first strong char determines direction', () => {
    const { app } = createHarness();
    // LTR char appears before RTL
    assertEqual(app.directionForText('Aא'), 'ltr', 'ltr wins when first');
    // RTL char appears before LTR
    assertEqual(app.directionForText('אA'), 'rtl', 'rtl wins when first');
  });

  await run('directionForText covers supported Unicode direction ranges', () => {
    const { app } = createHarness();
    const cases = [
      ['À', 'ltr', 'Latin Extended'],
      ['Ω', 'ltr', 'Greek'],
      ['Ж', 'ltr', 'Cyrillic'],
      ['א', 'rtl', 'Hebrew'],
      ['ا', 'rtl', 'Arabic'],
      ['   ... Ωא', 'ltr', 'first strong char after neutrals is Greek'],
      ['   ... אΩ', 'rtl', 'first strong char after neutrals is Hebrew'],
    ];
    for (const [text, expected, label] of cases) {
      assertEqual(app.directionForText(text), expected, label);
    }
  });

  await run('renderSidebar creates group sections and session rows', () => {
    const sessions = [
      { id: 'a', title: 'Alpha', created: 2000, messages: [], pinned: false, archived: false, messageCount: 3, lastMessageAt: 2000 },
      { id: 'b', title: 'Beta',  created: 1000, messages: [], pinned: false, archived: false, messageCount: 1, lastMessageAt: 1000 },
    ];
    const { app } = createHarness({ visibleSessions: () => sessions });

    app.renderSidebar();

    const groups = app.elements.sessionGroups.children;
    assertEqual(groups.length, 1, 'one group section');
    const rows = groups[0].querySelectorAll('.session-row');
    assertEqual(rows.length, 2, 'two session rows');
    assertEqual(rows[0].querySelector('.session-title').textContent, 'Alpha', 'first row title');
    assertEqual(rows[1].querySelector('.session-title').textContent, 'Beta', 'second row title');
  });

  await run('renderSidebar trusts server conversation message count over loaded client rows', () => {
    const sessions = [
      { id: 'a', title: 'Alpha', created: 2000, pinned: false, archived: false, lastMessageAt: 2000, messageCount: 2, messages: [
        { id: 'u1', role: 'user', content: 'run tools' },
        { id: 't1', role: 'tool', name: 'grep' },
        { id: 't2', role: 'tool', name: 'read_file' },
        { id: 't3', role: 'tool', name: 'shell' },
        { id: 't4', role: 'tool', name: 'edit_file' },
        { id: 'a1', role: 'assistant', content: 'done' },
      ] },
    ];
    const { app } = createHarness({ visibleSessions: () => sessions });

    app.renderSidebar();

    const metaEl = app.elements.sessionGroups.querySelector('.session-meta');
    assert(metaEl.textContent.startsWith('2 messages'), 'meta uses server conversation count');
  });

  await run('renderSidebar counts only user and assistant messages for unsynced local sessions', () => {
    const sessions = [
      { id: 'a', title: 'Alpha', created: 2000, pinned: false, archived: false, lastMessageAt: 2000, messages: [
        { id: 'u1', role: 'user', content: 'run tools' },
        { id: 't1', role: 'tool', name: 'grep' },
        { id: 'tg1', role: 'tool-group', tools: [{ id: 't2' }, { id: 't3' }, { id: 't4' }] },
        { id: 'a1', role: 'assistant', content: 'done' },
      ] },
    ];
    const { app } = createHarness({ visibleSessions: () => sessions });

    app.renderSidebar();

    const metaEl = app.elements.sessionGroups.querySelector('.session-meta');
    assert(metaEl.textContent.startsWith('2 messages'), 'meta counts user/assistant only');
    assert(!metaEl.textContent.includes('/'), 'meta does not show raw/tool count');
  });

  await run('renderSidebar trusts explicit zero server conversation message count', () => {
    const sessions = [
      { id: 'a', title: 'Alpha', created: 2000, pinned: false, archived: false, lastMessageAt: 2000, messageCount: 0, messages: [
        { id: 'u1', role: 'user', content: 'locally loaded but server says zero' },
        { id: 'a1', role: 'assistant', content: 'locally loaded but server says zero' },
      ] },
    ];
    const { app } = createHarness({ visibleSessions: () => sessions });

    app.renderSidebar();

    const metaEl = app.elements.sessionGroups.querySelector('.session-meta');
    assert(metaEl.textContent.startsWith('0 messages'), 'explicit server zero is authoritative');
  });

  await run('renderSidebar falls back to local count when server count is absent', () => {
    const sessions = [
      { id: 'a', title: 'Alpha', created: 2000, pinned: false, archived: false, lastMessageAt: 2000, messageCount: null, messages: [
        { id: 'u1', role: 'user', content: 'run tools' },
        { id: 'tg1', role: 'tool-group', tools: [{ id: 't1' }, { id: 't2' }, { id: 't3' }, { id: 't4' }] },
        { id: 'a1', role: 'assistant', content: 'done' },
      ] },
    ];
    const { app } = createHarness({ visibleSessions: () => sessions });

    app.renderSidebar();

    const metaEl = app.elements.sessionGroups.querySelector('.session-meta');
    assert(metaEl.textContent.startsWith('2 messages'), 'null server count falls back to local conversation count');
  });

  await run('renderSidebar skips re-render when nothing changed', () => {
    const session = { id: 'a', title: 'Alpha', created: 1000, messages: [], pinned: false, archived: false, messageCount: 2, lastMessageAt: 1000 };
    const { app } = createHarness({ visibleSessions: () => [session] });

    app.renderSidebar();
    const rowBefore = app.elements.sessionGroups.children[0].querySelectorAll('.session-row')[0];

    app.renderSidebar();
    const rowAfter = app.elements.sessionGroups.children[0].querySelectorAll('.session-row')[0];

    assert(rowBefore === rowAfter, 'identical fingerprint: no DOM change, same row node');
  });

  await run('renderSidebar exposes strategic full titles as row tooltips', () => {
    const sessions = [
      { id: 'a', title: 'Short custom name', longTitle: 'Generated long title with the useful strategic detail', created: 1000, messages: [], pinned: false, archived: false, messageCount: 0, lastMessageAt: 1000 },
    ];
    const { app } = createHarness({ visibleSessions: () => sessions });

    app.renderSidebar();

    const row = app.elements.sessionGroups.children[0].querySelectorAll('.session-row')[0];
    const btn = row.querySelector('.session-btn');
    const titleEl = row.querySelector('.session-title');
    const expected = 'Short custom name\nGenerated long title with the useful strategic detail';
    assertEqual(titleEl.title, expected, 'title element tooltip combines visible and long titles');
    assertEqual(btn.title, expected, 'whole row button exposes the same tooltip');
    assertEqual(btn.getAttribute('aria-label'), 'Open session: Short custom name', 'button has a descriptive label');
  });

  await run('renderSidebar updates title in-place, reusing the row DOM node', () => {
    const session = { id: 'a', title: 'Before', created: 1000, messages: [], pinned: false, archived: false, messageCount: 0, lastMessageAt: 1000 };
    const { app } = createHarness({ visibleSessions: () => [session] });

    app.renderSidebar();
    const rowBefore = app.elements.sessionGroups.children[0].querySelectorAll('.session-row')[0];

    session.title = 'After';
    app.renderSidebar();
    const rowAfter = app.elements.sessionGroups.children[0].querySelectorAll('.session-row')[0];

    assert(rowBefore === rowAfter, 'same row DOM node reused');
    assertEqual(rowAfter.querySelector('.session-title').textContent, 'After', 'title updated in-place');
  });

  await run('renderSidebar updates active button class on session switch', () => {
    const sessions = [
      { id: 'a', title: 'A', created: 2000, messages: [], pinned: false, archived: false, messageCount: 0, lastMessageAt: 2000 },
      { id: 'b', title: 'B', created: 1000, messages: [], pinned: false, archived: false, messageCount: 0, lastMessageAt: 1000 },
    ];
    const { app } = createHarness({ visibleSessions: () => sessions });
    app.state.activeSessionId = 'a';

    app.renderSidebar();

    const rows = app.elements.sessionGroups.querySelectorAll('.session-row');
    assert(rows[0].querySelector('.session-btn').classList.contains('active'), 'session A btn is active initially');
    assert(!rows[1].querySelector('.session-btn').classList.contains('active'), 'session B btn is not active');

    app.state.activeSessionId = 'b';
    app.renderSidebar();

    assert(!rows[0].querySelector('.session-btn').classList.contains('active'), 'session A btn no longer active');
    assert(rows[1].querySelector('.session-btn').classList.contains('active'), 'session B btn now active');
  });

  await run('renderSidebar preserves sidebar scroll through rerenders', () => {
    const sessions = [
      { id: 'a', title: 'A', created: 2000, messages: [], pinned: false, archived: false, messageCount: 0, lastMessageAt: 2000 },
      { id: 'b', title: 'B', created: 1000, messages: [], pinned: false, archived: false, messageCount: 0, lastMessageAt: 1000 },
    ];
    const { app, timers } = createHarness({ visibleSessions: () => sessions });
    app.elements.sidebarContent.scrollTop = 420;

    app.renderSidebar();
    sessions[1].title = 'B changed';
    app.renderSidebar();

    assertEqual(app.elements.sidebarContent.scrollTop, 420, 'scrollTop restored immediately');
    app.elements.sidebarContent.scrollTop = 99;
    runAllPendingTimers(timers);
    assertEqual(app.elements.sidebarContent.scrollTop, 420, 'scrollTop restored again on animation frame');
  });

  await run('session row mouse activation does not focus-scroll the sidebar button', async () => {
    const session = { id: 'a', title: 'A', created: 1000, messages: [], pinned: false, archived: false, messageCount: 0, lastMessageAt: 1000 };
    let switchedTo = '';
    const { app } = createHarness({
      visibleSessions: () => [session],
      async switchToSession(id) { switchedTo = id; },
    });

    app.renderSidebar();
    const btn = app.elements.sessionGroups.querySelector('.session-btn');
    let prevented = false;
    await btn.dispatchEvent({ type: 'mousedown', button: 0, preventDefault() { prevented = true; } });
    await btn.dispatchEvent({ type: 'click' });

    assert(prevented, 'primary mouse down prevents browser focus scrolling');
    assertEqual(switchedTo, 'a', 'click still switches sessions');
  });

  await run('renderSidebar marks in-progress session row with is-active', () => {
    const sessions = [
      { id: 'a', title: 'A', created: 2000, messages: [], pinned: false, archived: false, messageCount: 0, lastMessageAt: 2000 },
      { id: 'b', title: 'B', created: 1000, messages: [], pinned: false, archived: false, messageCount: 0, lastMessageAt: 1000 },
    ];
    const { app } = createHarness({
      visibleSessions: () => sessions,
      sessionHasInProgressState: (s) => s.id === 'a',
    });

    app.renderSidebar();
    const rows = app.elements.sessionGroups.querySelectorAll('.session-row');
    assert(rows[0].classList.contains('is-active'), 'in-progress row has is-active');
    assert(!rows[1].classList.contains('is-active'), 'idle row does not have is-active');
  });

  await run('renderSidebar separates pinned sessions into their own group', () => {
    const sessions = [
      { id: 'p', title: 'Pinned', created: 1000, messages: [], pinned: true,  archived: false, messageCount: 0, lastMessageAt: 1000 },
      { id: 'n', title: 'Normal', created: 900,  messages: [], pinned: false, archived: false, messageCount: 0, lastMessageAt: 900 },
    ];
    const { app } = createHarness({ visibleSessions: () => sessions });

    app.renderSidebar();

    const groups = app.elements.sessionGroups.children;
    assertEqual(groups.length, 2, 'two groups: Pinned and Today');
    assertEqual(groups[0].querySelector('h3').textContent, 'Pinned', 'first group is Pinned');
    assertEqual(groups[1].querySelector('h3').textContent, 'Today', 'second group is Today');
    assertEqual(groups[0].querySelectorAll('.session-row').length, 1, 'one pinned row');
    assertEqual(groups[1].querySelectorAll('.session-row').length, 1, 'one today row');
  });

  await run('cached sidebar menu actions resolve latest session object by id', async () => {
    const original = { id: 'stale', title: 'Old', created: 1000, messages: [], pinned: false, archived: false, messageCount: 0, lastMessageAt: 1000 };
    const replacement = { id: 'stale', title: 'New', created: 1000, messages: [], pinned: true, archived: true, messageCount: 0, lastMessageAt: 2000 };
    const calls = [];
    const { app } = createHarness({
      visibleSessions: () => app.state.sessions,
      async promptRenameSession(session) { calls.push(['rename', session]); },
      async setSessionPinned(session, pinned) { calls.push(['pin', session, pinned]); },
      async setSessionArchived(session, archived) { calls.push(['archive', session, archived]); },
    });
    app.state.sessions = [original];
    app.renderSidebar();

    // Simulate a sync path replacing the session object while reusing the cached row.
    app.state.sessions = [replacement];
    app.renderSidebar();
    const buttons = app.elements.sessionGroups.querySelectorAll('button');
    const event = () => ({ type: 'click', preventDefault() {}, stopPropagation() {} });

    await buttons[2].dispatchEvent(event());
    await buttons[3].dispatchEvent(event());
    await buttons[4].dispatchEvent(event());

    assert(calls[0][0] === 'rename' && calls[0][1] === replacement, 'rename uses latest session object');
    assert(calls[1][0] === 'pin' && calls[1][1] === replacement && calls[1][2] === false, 'pin toggle uses latest pinned state');
    assert(calls[2][0] === 'archive' && calls[2][1] === replacement && calls[2][2] === false, 'archive toggle uses latest archived state');
  });

  await run('updateSidebarStatus updates title and meta via cache', () => {
    const session = { id: 'x', title: 'Old', created: 1000, messages: [], pinned: false, archived: false, messageCount: 1, lastMessageAt: 1000 };
    const { app } = createHarness({ visibleSessions: () => [session] });
    app.state.sessions = [session];

    app.renderSidebar();

    const result = app.updateSidebarStatus([{
      id: 'x',
      short_title: 'New Title',
      long_title: 'Full new title',
      message_count: 7,
      active_run: false,
    }]);

    assert(result === true || result === false, 'updateSidebarStatus returns a boolean');
    const rows = app.elements.sessionGroups.querySelectorAll('.session-row');
    assertEqual(rows.length, 1, 'one row rendered');
    const titleEl = rows[0].querySelector('.session-title');
    assertEqual(titleEl.textContent, 'New Title', 'title updated from status');
    assertEqual(titleEl.title, 'Full new title', 'long title set on title element');
    const metaEl = rows[0].querySelector('.session-meta');
    assert(metaEl.textContent.startsWith('7 messages'), 'meta shows updated message count');
  });

  await run('updateSidebarStatus toggles is-active class on cached row', () => {
    const session = { id: 'y', title: 'Busy', created: 2000, messages: [], pinned: false, archived: false, messageCount: 0, lastMessageAt: 2000 };
    let activeRun = false;
    const { app } = createHarness({
      visibleSessions: () => [session],
      sessionHasInProgressState: () => activeRun,
      setSessionServerActiveRun: (_target, val) => { activeRun = val; },
    });
    app.state.sessions = [session];

    app.renderSidebar();
    const row = app.elements.sessionGroups.querySelector('.session-row');
    assert(!row.classList.contains('is-active'), 'not active before status update');

    activeRun = false;
    app.updateSidebarStatus([{ id: 'y', active_run: true }]);
    assert(row.classList.contains('is-active'), 'row gains is-active when active_run is set');

    app.updateSidebarStatus([{ id: 'y', active_run: false }]);
    assert(!row.classList.contains('is-active'), 'row loses is-active when active_run cleared');
  });

  await run('updateSidebarStatus ignores sessions not in cache', () => {
    const session = { id: 'z', title: 'Z', created: 3000, messages: [], pinned: false, archived: false, messageCount: 0, lastMessageAt: 3000 };
    const { app } = createHarness({ visibleSessions: () => [session] });
    app.state.sessions = [session];

    app.renderSidebar();

    let threw = false;
    try {
      app.updateSidebarStatus([{ id: 'unknown-id', short_title: 'Ghost', message_count: 99 }]);
    } catch (_) {
      threw = true;
    }
    assert(!threw, 'updateSidebarStatus must not throw for unknown session id');
    const row = app.elements.sessionGroups.querySelector('.session-row');
    assertEqual(row.querySelector('.session-title').textContent, 'Z', 'known session row unchanged');
  });

  await run('renderMessages: compaction summaries are collapsed and expandable', async () => {
    const { app, session, messages } = createHarness();
    const compaction = {
      id: 'c1',
      role: 'compaction',
      content: 'Context compacted',
      rawContent: '[Context Compaction]\nsecret transcript',
      lineCount: 2,
      activeBoundary: true,
      compactionCount: 1,
      created: Date.now(),
    };
    session.messages = [compaction];

    app.renderMessages(true);

    const node = findByMessageId(messages, 'c1');
    assert(node && node.classList.contains('compaction'), 'expected compaction message node');
    assert(!node.querySelector('.compaction-raw'), 'raw compaction body should be hidden by default');
    assertEqual(node.querySelector('.compaction-label')?.textContent, '↳ Context compacted', 'compaction label');
    assert(node.querySelector('.compaction-body')?.classList.contains('active-boundary'), 'active boundary class should be present');

    const toggle = node.querySelector('.compaction-toggle');
    assert(toggle, 'expected details toggle');
    await toggle.dispatchEvent({ type: 'click' });

    const raw = node.querySelector('.compaction-raw');
    assert(raw, 'raw compaction body should appear after toggle');
    assert(raw.textContent.includes('secret transcript'), 'raw compaction text should be preserved for details');
    assert(compaction.expanded, 'toggle should persist expanded state on message');

    app.renderMessages(false);
    const rerendered = findByMessageId(messages, 'c1');
    assert(rerendered?.querySelector('.compaction-raw'), 'expanded compaction details should survive a full render pass');
  });

  await run('renderMessages: leaves server-only sessions blank while messages load', () => {
    const { app, session, messages } = createHarness();
    session.messages = [];
    session._serverOnly = true;

    app.renderMessages();

    assertEqual(messages.children.length, 0, 'server-only loading session should not show the new-chat empty state');
    assertEqual(messages.textContent, '', 'server-only loading session should be visually blank');
  });

  await run('renderMessages: empty local session shows new-chat prompt', () => {
    const { app, session, messages } = createHarness();
    session.messages = [];

    app.renderMessages();

    assertEqual(messages.children.length, 1, 'empty local session should show one empty-state node');
    assertEqual(messages.children[0].className, 'empty-state', 'empty local session uses empty-state class');
    assertEqual(messages.children[0].textContent, 'How can I help you today?', 'empty local session keeps the new-chat prompt');
  });

  await run('renderMessages: incremental append reuses existing nodes', () => {
    const { app, session, messages } = createHarness();
    session.messages = [
      { id: 'm1', role: 'user', content: 'hello', created: Date.now() },
      { id: 'm2', role: 'assistant', content: 'hi', created: Date.now() },
    ];
    app.renderMessages();
    assertEqual(messages.children.length, 2, 'two nodes after first render');
    const firstNode = messages.children[0];

    // Append a new message and re-render
    session.messages.push({ id: 'm3', role: 'user', content: 'again', created: Date.now() });
    app.renderMessages();
    assertEqual(messages.children.length, 3, 'three nodes after incremental render');
    assert(messages.children[0] === firstNode, 'first node is the same object (not recreated)');
    assertEqual(messages.children[2].dataset.messageId, 'm3', 'new node has correct id');
  });

  await run('mounted reconnect reconciliation restores an earlier user before later tool rows', () => {
    const { app, session, messages } = createHarness();
    const user = {
      id: 'msg_keep_going',
      role: 'user',
      content: 'keep going',
      interruptState: 'interject',
      created: 1000,
    };
    const queueAgent = {
      id: 'tools_queue_agent',
      role: 'tool-group',
      status: 'done',
      tools: [{ id: 'call_queue_agent', name: 'queue_agent', status: 'done', arguments: '{}' }],
      created: 2000,
    };
    const waitForJobs = {
      id: 'tools_wait_for_jobs',
      role: 'tool-group',
      status: 'running',
      tools: [{ id: 'call_wait_for_jobs', name: 'wait_for_jobs', status: 'running', arguments: '{}' }],
      created: 3000,
    };
    session.messages = [user, queueAgent, waitForJobs];

    // Reproduce the live reconnect window: recovery has mounted later stream
    // cards, while the earlier optimistic/interjected user row is temporarily
    // absent. Replaying response.interjection updates that existing message.
    messages.appendChild(app.createToolGroupNode(queueAgent));
    messages.appendChild(app.createToolGroupNode(waitForJobs));
    app.updateMountedUserNode(session, user);

    assertEqual(
      messages.children.map((node) => node.dataset.messageId).join(','),
      'msg_keep_going,tools_queue_agent,tools_wait_for_jobs',
      'live DOM follows session message order without requiring a reload'
    );
  });

  await run('transcript reconciliation removes duplicate optimistic tool DOM without reload', () => {
    const { app, session, messages } = createHarness();
    session.transcript = {};
    const durable = {
      id: 'srv_seq_387_tools_1',
      role: 'tool-group',
      durable: true,
      transcriptSegmentIndex: 0,
      status: 'done',
      tools: [
        { id: 'call_shell_exact_a', name: 'shell', status: 'done', arguments: '{}' },
        { id: 'call_shell_exact_b', name: 'shell', status: 'done', arguments: '{}' }
      ],
      created: 1000
    };
    const exactLive = {
      id: 'msg_ba0-live-tools',
      role: 'tool-group',
      status: 'done',
      tools: [
        { id: 'call_shell_exact_a', name: 'shell', status: 'done', arguments: '{}' },
        { id: 'call_shell_exact_b', name: 'shell', status: 'done', arguments: '{}' }
      ],
      created: 1100
    };
    const partialLive = {
      id: 'msg_652-live-tools',
      role: 'tool-group',
      status: 'running',
      tools: [
        { id: 'call_shell_exact_b', name: 'shell', status: 'done', arguments: '{}' },
        { id: 'call_shell_newer', name: 'shell', status: 'running', arguments: '{}' }
      ],
      created: 1200
    };
    session.messages = [durable, exactLive, partialLive];
    app.renderMessages();
    assertEqual(messages.querySelectorAll('.tool-group').length, 3, 'reproduction mounts durable and duplicate optimistic cards');

    session.messages = reconcileToolCallProjection(session.messages);
    app.renderMessages();

    assertEqual(messages.querySelectorAll('.tool-group').length, 2, 'covered optimistic group is removed in the mounted reconciliation');
    assertEqual(messages.querySelectorAll('[data-tool-id="call_shell_exact_a"]').length, 1, 'first durable call ID appears once in DOM');
    assertEqual(messages.querySelectorAll('[data-tool-id="call_shell_exact_b"]').length, 1, 'second durable call ID appears once in DOM');
    assertEqual(messages.querySelectorAll('[data-tool-id="call_shell_newer"]').length, 1, 'newer optimistic-only call remains once in DOM');
    assertEqual(
      session.messages.map((message) => message.id).join(','),
      'srv_seq_387_tools_1,msg_652-live-tools',
      'session projection preserves durable then newer live ordering'
    );
  });

  await run('forced transcript render starts bottom anchoring after DOM commit', () => {
    const scrollCalls = [];
    let harness = null;
    harness = createHarness({
      scrollToBottom(force) {
        scrollCalls.push({ force, renderedCount: harness.messages.children.length });
      },
    });
    harness.session.messages = [{ id: 'startup-user', role: 'user', content: 'hello', created: 1000 }];

    harness.app.renderMessages(true);

    assertEqual(scrollCalls.length, 1, 'render issues one bottom-anchor request');
    assertEqual(scrollCalls[0].force, true, 'startup render requests forced anchoring');
    assertEqual(scrollCalls[0].renderedCount, 1, 'forced anchoring starts after transcript DOM commit');
  });

  await run('renderMessages exposes gated mounted-count and duration diagnostics', () => {
    const { app, session, messages, windowObj } = createHarness();
    windowObj.__TERM_LLM_DIAGNOSTICS__ = true;
    session.messages = [
      { id: 'diag-u1', role: 'user', content: 'hello', created: Date.now() },
      { id: 'diag-a1', role: 'assistant', content: 'hi', created: Date.now() },
    ];

    app.renderMessages();

    assertEqual(messages.dataset.mountedMessageCount, '2', 'mounted message count');
    assert(Number(messages.dataset.mountedElementCount) >= 2, 'mounted element count should include message DOM');
    assert(Number(messages.dataset.renderDurationMs) >= 0, 'render duration should be numeric');
    assertEqual(messages.dataset.renderMode, 'rebuild', 'render mode diagnostic');
  });

  await run('renderMessages: full rebuild on session switch', () => {
    const { app, session, messages } = createHarness();
    session.messages = [
      { id: 'a1', role: 'user', content: 'first', created: Date.now() },
    ];
    app.renderMessages();
    assertEqual(messages.children.length, 1, 'one node for session s1');
    const originalNode = messages.children[0];

    // Simulate switching sessions by mutating the session object the harness returns
    session.id = 's2';
    session.messages = [
      { id: 'b1', role: 'user', content: 'other', created: Date.now() },
    ];
    app.renderMessages();
    assertEqual(messages.children.length, 1, 'one node after session switch');
    assert(messages.children[0] !== originalNode, 'node was recreated after session switch');
    assertEqual(messages.children[0].dataset.messageId, 'b1', 'new session node has correct id');
  });

  await run('renderMessages: non-append updates reuse unchanged assistant nodes', () => {
    const { app, session, messages, parseCalls } = createHarness();
    session.messages = [
      { id: 'u1', role: 'user', content: 'a', created: Date.now() },
      { id: 'a1', role: 'assistant', content: 'b', created: Date.now() },
    ];
    app.renderMessages();
    assertEqual(messages.children.length, 2, 'two nodes initially');
    const originalUserNode = messages.children[0];
    const originalAssistantNode = messages.children[1];
    assertEqual(parseCalls.length, 1, 'assistant markdown parsed once on initial render');

    session.messages = [
      { id: 'u0', role: 'user', content: 'history', created: Date.now() - 1000 },
      session.messages[0],
      session.messages[1],
    ];
    app.renderMessages();

    assertEqual(messages.children.length, 3, 'history insert keeps all messages rendered');
    assertEqual(messages.children[0].dataset.messageId, 'u0', 'new history node inserted at the front');
    assert(messages.children[1] === originalUserNode, 'existing user node reused after front insertion');
    assert(messages.children[2] === originalAssistantNode, 'existing assistant node reused after front insertion');
    assertEqual(parseCalls.length, 1, 'unchanged assistant markdown was not reparsed');
  });

  await run('renderMessages: same-length updates still refresh changed assistant content', () => {
    const { app, session, messages, parseCalls } = createHarness();
    session.messages = [
      { id: 'u1', role: 'user', content: 'prompt', created: Date.now() },
      { id: 'a1', role: 'assistant', content: 'old', created: Date.now() },
    ];
    app.renderMessages();
    assertEqual(parseCalls.length, 1, 'assistant parsed on initial render');

    session.messages = [
      session.messages[0],
      { ...session.messages[1], content: 'new' },
    ];
    app.renderMessages();

    assertEqual(parseCalls.length, 2, 'assistant reparsed when content changes without any append');
    assertEqual(messages.children[1].querySelector('.message-body').innerHTML, 'new', 'assistant content updated');
  });

  await run('updateSidebarStatus finds local session by id using Map lookup', () => {
    const s1 = { id: 'aaa', title: 'A', created: 1000, messages: [], pinned: false, archived: false, messageCount: 0, lastMessageAt: 1000 };
    const s2 = { id: 'bbb', title: 'B', created: 2000, messages: [], pinned: false, archived: false, messageCount: 0, lastMessageAt: 2000 };
    const s3 = { id: 'ccc', title: 'C', created: 3000, messages: [], pinned: false, archived: false, messageCount: 0, lastMessageAt: 3000 };
    const { app } = createHarness({ visibleSessions: () => [s1, s2, s3] });
    app.state.sessions = [s1, s2, s3];

    const result = app.updateSidebarStatus([{ id: 'bbb', last_message_at: 9999, active_run: false }]);

    assert(result === true, 'returns true when order changed');
    assertEqual(s2.lastMessageAt, 9999, 'lastMessageAt updated on the matched session');
    assertEqual(s1.lastMessageAt, 1000, 'first session unchanged');
    assertEqual(s3.lastMessageAt, 3000, 'third session unchanged');
  });

  await run('enqueueAssistantStreamUpdate reuses cached node on subsequent calls', () => {
    const { app, session, messages } = createHarness();
    const message = { id: 'cached-node', role: 'assistant', content: 'hello', created: Date.now() };
    session.messages = [message];

    app.enqueueAssistantStreamUpdate(message);
    assertEqual(messages.children.length, 1, 'node created on first call');

    // Remove node from DOM: old code would re-query (find null) and create a new node;
    // new code uses existingState.node directly and adds nothing.
    messages.children[0].remove();
    assertEqual(messages.children.length, 0, 'node removed from messages');

    message.content = 'hello world';
    app.enqueueAssistantStreamUpdate(message);
    assertEqual(messages.children.length, 0, 'fast path does not re-query or create a new node');
  });

  await run('renderMessages reuses unchanged transcript nodes during incremental streaming updates', () => {
    const { app, session, messages } = createHarness();
    session.transcript = {};
    session.messages = [
      { id: 'tu1', role: 'user', content: 'one', durable: true, durableRowId: 1, transcriptSegmentIndex: 0, created: Date.now() },
      { id: 'ta1', role: 'assistant', content: 'stable', durable: true, durableRowId: 2, transcriptSegmentIndex: 0, created: Date.now() },
    ];
    app.renderMessages();
    const turn = messages.children[0];
    const userNode = turn.children[0];
    const assistantNode = turn.children[1];

    session.messages.push({ id: 'stream-tail', role: 'assistant', content: 'growing', optimistic: true, created: Date.now() });
    app.renderMessages();

    assert(messages.children[0] === turn, 'turn container should survive the stream append');
    assert(turn.children[0] === userNode, 'unchanged durable user node should survive the stream append');
    assert(turn.children[1] === assistantNode, 'unchanged durable assistant node should survive the stream append');
    assertEqual(messages.children[1].dataset.messageId, 'stream-tail', 'stream tail should append after materialized turn');
  });

  await run('transcript gap observer uses current geometry after initial auto-scroll', async () => {
    const loads = [];
    let observerCallback = null;
    class FakeIntersectionObserver {
      constructor(callback) { observerCallback = callback; }
      observe() {}
      unobserve() {}
    }
    const { app, session, messages, timers } = createHarness({
      IntersectionObserver: FakeIntersectionObserver,
      materializeTranscriptSegments(targetSession, range) { loads.push({ targetSession, range }); }
    });
    app.elements.chatScroll = { getBoundingClientRect: () => ({ top: 0, bottom: 600 }) };
    session.transcript = {};
    session.messages = [
      { id: 'gap', role: 'transcript-gap', transcriptGap: true, startOrdinal: 0, endOrdinal: 1000, estimatedHeight: 5000, startSegmentIndex: 0, endSegmentIndex: 1000 },
      { id: 'u2', role: 'user', content: 'latest', durable: true, durableRowId: 1002, transcriptSegmentIndex: 1001, created: Date.now() },
    ];
    app.renderMessages(true);
    const gap = messages.children[0];
    // The observer captured this gap before renderMessages' forced scroll to the
    // latest turn. By callback time the gap is above the viewport.
    gap.getBoundingClientRect = () => ({ top: -5000, bottom: -100 });
    app.state.autoScroll = true;
    observerCallback([{
      isIntersecting: true,
      target: gap,
      boundingClientRect: { top: 0, bottom: 5000 }
    }]);
    assertEqual(loads.length, 0, 'initial bottom-pinned render does not materialize an earlier gap');
    assertEqual(timers.filter((timer) => !timer.cleared).length, 0, 'bottom-pinned gap does not queue deferred work');

    app.state.autoScroll = false;
    observerCallback([{
      isIntersecting: true,
      target: gap,
      boundingClientRect: { top: 0, bottom: 5000 }
    }]);
    assertEqual(loads.length, 0, 'user-initiated intersection waits for post-scroll layout before materializing');
    runNextTimer(timers);
    await flushMicrotasks();

    assertEqual(loads.length, 1, 'intersection requests one bounded materialization');
    assertEqual(loads[0].targetSession, session, 'intersection targets the active session');
    assertEqual(loads[0].range.targetOrdinal, 1000, 'callback remeasures the gap after forced scrolling');
    assertEqual(loads[0].range.direction, 'backward', 'gap above the current viewport loads from its trailing edge');
  });

  await run('renderMessages bounds transcript DOM with compact clickable gap ranges', async () => {
    const loads = [];
    const { app, session, messages } = createHarness({
      materializeTranscriptSegments(targetSession, range) { loads.push({ targetSession, range }); }
    });
    session.transcript = {};
    session.messages = [
      { id: 'u1', role: 'user', content: 'one', durable: true, durableRowId: 1, transcriptSegmentIndex: 0, created: Date.now() },
      { id: 'a1', role: 'assistant', content: 'answer', durable: true, durableRowId: 2, transcriptSegmentIndex: 0, created: Date.now() },
      { id: 'gap', role: 'transcript-gap', transcriptGap: true, startOrdinal: 2, endOrdinal: 1000, estimatedHeight: 5000, startSegmentIndex: 1, endSegmentIndex: 999 },
      { id: 'u2', role: 'user', content: 'later', durable: true, durableRowId: 1002, transcriptSegmentIndex: 1000, created: Date.now() },
    ];
    app.renderMessages();
    assertEqual(messages.children.length, 3, 'two materialized turns plus one contiguous gap are the only top-level DOM nodes');
    assert(messages.children[0].classList.contains('transcript-turn'), 'first durable turn is grouped');
    assertEqual(messages.children[0].children.length, 2, 'rows in one turn share a container');
    const gap = messages.children[1];
    assert(gap.classList.contains('transcript-gap'), 'unloaded ordinals render as an explicit gap');
    assertEqual(gap.dataset.startSegmentIndex, '1', 'gap serializes only its first segment bound');
    assertEqual(gap.dataset.endSegmentIndex, '999', 'gap serializes only its last segment bound');
    assertEqual(gap.dataset.segmentIndexes, undefined, 'gap must not serialize an unbounded segment CSV');
    app.elements.chatScroll = { getBoundingClientRect: () => ({ top: 0, bottom: 600 }) };
    gap.getBoundingClientRect = () => ({ top: 100, bottom: 1100 });
    await gap.dispatchEvent({ type: 'click', clientY: 350 });
    assertEqual(loads.length, 1, 'clicking a transcript gap requests one bounded materialization');
    assertEqual(loads[0].targetSession, session, 'gap click targets the active session');
    assertEqual(loads[0].range.startSegmentIndex, 1, 'gap click forwards the compact first segment bound');
    assertEqual(loads[0].range.endSegmentIndex, 999, 'gap click forwards the compact last segment bound');
    assertEqual(loads[0].range.targetOrdinal, 251, 'gap click targets the pointer position within the estimated gap');
    assertEqual(loads[0].range.direction, 'center', 'gap click centered inside the viewport loads nearby turns');
    assert(!Array.isArray(loads[0].range), 'gap click must not expand bounds into a segment list');
  });

  if (failures > 0) {
    process.exit(1);
  }
})();
