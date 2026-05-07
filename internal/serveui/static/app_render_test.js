#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const source = fs.readFileSync(path.join(__dirname, 'app-render.js'), 'utf8');
const markdownStreaming = require(path.join(__dirname, 'markdown-streaming.js'));
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
    this.attributes.set(name, String(value));
    if (name === 'class') this.className = String(value);
  }

  getAttribute(name) {
    return this.attributes.get(name) || null;
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
  const session = { id: 's1', title: 'Chat', created: Date.now(), messages: [] };
  const state = { activeSessionId: 's1', sessions: [session], sidebarCollapsed: false };
  const timers = [];
  let timerId = 0;
  const copied = [];

  const app = {
    STORAGE_KEYS: { sidebarCollapsed: 'sidebar' },
    state,
    elements: {
      messages,
      sidebar: new Element('div'),
      sidebarBackdrop: new Element('div'),
      sidebarToggleBtn: new Element('button'),
      sidebarPanelToggleBtn: new Element('button'),
      appShell: new Element('div'),
      activeSessionTitle: new Element('div'),
      sessionGroups: new Element('div'),
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
  };
  context.globalThis = context;
  windowObj.document = document;
  windowObj.navigator = context.navigator;
  windowObj.localStorage = context.localStorage;

  vm.runInNewContext(source, context, { filename: 'app-render.js' });
  return { app, session, messages, document, timers, copied };
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

  await run('renderSidebar skips re-render when nothing changed', () => {
    const session = { id: 'a', title: 'Alpha', created: 1000, messages: [], pinned: false, archived: false, messageCount: 2, lastMessageAt: 1000 };
    const { app } = createHarness({ visibleSessions: () => [session] });

    app.renderSidebar();
    const rowBefore = app.elements.sessionGroups.children[0].querySelectorAll('.session-row')[0];

    app.renderSidebar();
    const rowAfter = app.elements.sessionGroups.children[0].querySelectorAll('.session-row')[0];

    assert(rowBefore === rowAfter, 'identical fingerprint: no DOM change, same row node');
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

  if (failures > 0) {
    process.exit(1);
  }
})();
