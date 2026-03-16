(() => {
'use strict';

const app = window.TermLLMApp;
const {
  state, elements, INTERRUPT_BADGE_META, sanitizeInterruptState, relativeTime, fullDate, sessionBucket, toolIcon, formatUsage,
  saveSessions, findMessageElement, scrollToBottom, refreshRelativeTimes, ensureActiveSession, updateDocumentTitle,
  updateSessionUsageDisplay, updateURL, renderMath
} = app;

const directionForText = (value) => {
  const text = String(value || '');
  const strongChars = text.match(/[A-Za-z\u00C0-\u02AF\u0370-\u03FF\u0400-\u052F\u0590-\u08FF]/g);
  if (!strongChars || strongChars.length === 0) return 'auto';
  for (const ch of strongChars) {
    if (/[\u0590-\u08FF]/.test(ch)) return 'rtl';
    if (/[A-Za-z\u00C0-\u02AF\u0370-\u03FF\u0400-\u052F]/.test(ch)) return 'ltr';
  }
  return 'auto';
};

const applyTextDirection = (element, value) => {
  if (!element) return;
  const dir = directionForText(value);
  if (dir === 'auto') {
    element.setAttribute('dir', 'auto');
    element.classList.remove('rtl');
    return;
  }
  element.setAttribute('dir', dir);
  element.classList.toggle('rtl', dir === 'rtl');
};

const openSidebar = () => {
  elements.sidebar.classList.add('open');
  elements.sidebarBackdrop.classList.add('open');
};

const closeSidebar = () => {
  elements.sidebar.classList.remove('open');
  elements.sidebarBackdrop.classList.remove('open');
};

const closeSidebarIfMobile = () => {
  if (window.matchMedia('(max-width: 767px)').matches) {
    closeSidebar();
  }
};

const updateHeader = () => {
  const session = ensureActiveSession();
  elements.activeSessionTitle.textContent = session.title || 'Chat';
  updateDocumentTitle();
  updateSessionUsageDisplay(session);
};

const renderSidebar = () => {
  const grouped = {
    Today: [],
    Yesterday: [],
    'This week': [],
    Older: []
  };

  const sorted = [...state.sessions].sort((a, b) => b.created - a.created);
  sorted.forEach((session) => {
    grouped[sessionBucket(session.created)].push(session);
  });

  elements.sessionGroups.innerHTML = '';

  Object.entries(grouped).forEach(([label, sessions]) => {
    if (!sessions.length) return;

    const groupEl = document.createElement('section');
    groupEl.className = 'session-group';

    const heading = document.createElement('h3');
    heading.textContent = label;
    groupEl.appendChild(heading);

    sessions.forEach((session) => {
      const btn = document.createElement('button');
      btn.className = 'session-btn';
      if (session.id === state.activeSessionId) {
        btn.classList.add('active');
      }

      const title = document.createElement('div');
      title.className = 'session-title';
      title.textContent = session.title || 'New chat';
      if (session.longTitle) {
        title.title = session.longTitle;
      }

      const meta = document.createElement('div');
      meta.className = 'session-meta';
      const msgCount = session.messages.length || session.messageCount || 0;
      meta.textContent = `${msgCount} message${msgCount === 1 ? '' : 's'} · ${relativeTime(session.created)}`;
      meta.title = fullDate(session.created);

      btn.appendChild(title);
      btn.appendChild(meta);
      btn.addEventListener('click', async () => {
        await app.switchToSession(session.id);
      });

      groupEl.appendChild(btn);
    });

    elements.sessionGroups.appendChild(groupEl);
  });
};

// ===== Message rendering =====
const createInterruptBadgeNode = (interruptState) => {
  const stateName = sanitizeInterruptState(interruptState);
  if (!stateName) return null;

  const config = INTERRUPT_BADGE_META[stateName];
  if (!config) return null;

  const badge = document.createElement('span');
  badge.className = `interrupt-badge ${config.className}`;

  if (stateName === 'evaluating') {
    const spinner = document.createElement('span');
    spinner.className = 'interrupt-spinner';
    spinner.setAttribute('aria-hidden', 'true');
    badge.appendChild(spinner);

    const text = document.createElement('span');
    text.textContent = config.label;
    badge.appendChild(text);
  } else {
    badge.textContent = `${config.icon} ${config.label}`;
  }

  return badge;
};

const createMetaNode = (created, message = null) => {
  const meta = document.createElement('div');
  meta.className = 'message-meta';

  if (message?.role === 'user') {
    const badge = createInterruptBadgeNode(message.interruptState);
    if (badge) {
      meta.appendChild(badge);
    }
  }

  const time = document.createElement('span');
  time.setAttribute('data-created', String(created));
  time.textContent = relativeTime(created);
  time.title = fullDate(created);

  meta.appendChild(time);
  return meta;
};

const buildDeferredVideoNode = (video) => {
  const wrapper = document.createElement('div');
  wrapper.className = 'deferred-video';

  const button = document.createElement('button');
  button.type = 'button';
  button.className = 'deferred-video-btn';
  button.textContent = 'Load video';

  const src = video.getAttribute('src') || '';
  const poster = video.getAttribute('poster') || '';
  const preload = video.getAttribute('preload') || '';
  if (src) button.dataset.videoSrc = src;
  if (poster) button.dataset.videoPoster = poster;
  if (preload) button.dataset.videoPreload = preload;

  const sources = Array.from(video.querySelectorAll('source'))
    .map((source) => ({
      src: source.getAttribute('src') || '',
      type: source.getAttribute('type') || ''
    }))
    .filter((source) => source.src);
  if (sources.length > 0) {
    button.dataset.videoSources = JSON.stringify(sources);
  }

  button.addEventListener('click', () => {
    const replacement = document.createElement('video');
    ['controls', 'playsinline', 'muted', 'loop'].forEach((attr) => {
      if (video.hasAttribute(attr)) replacement.setAttribute(attr, '');
    });
    if (poster) replacement.setAttribute('poster', poster);
    replacement.setAttribute('preload', 'metadata');

    if (src) {
      replacement.src = src;
    } else {
      sources.forEach((source) => {
        const sourceNode = document.createElement('source');
        sourceNode.src = source.src;
        if (source.type) sourceNode.type = source.type;
        replacement.appendChild(sourceNode);
      });
    }

    wrapper.replaceWith(replacement);
  });

  if (poster) {
    const preview = document.createElement('img');
    preview.src = poster;
    preview.alt = 'Video preview';
    preview.className = 'deferred-video-poster';
    wrapper.appendChild(preview);
  }

  wrapper.appendChild(button);
  return wrapper;
};

const deferEmbeddedVideos = (target) => {
  target.querySelectorAll('video').forEach((video) => {
    video.removeAttribute('autoplay');
    video.setAttribute('preload', 'none');
    video.replaceWith(buildDeferredVideoNode(video));
  });
};

const renderAssistantMarkdown = (target, content) => {
  applyTextDirection(target, content || '');
  const html = marked.parse(content || '');
  const clean = DOMPurify.sanitize(html, {
    ADD_TAGS: ['video', 'source'],
    ADD_ATTR: ['controls', 'playsinline', 'muted', 'loop', 'autoplay', 'poster', 'preload']
  });
  target.innerHTML = clean;
  deferEmbeddedVideos(target);

  renderMath(target);

  target.querySelectorAll('a').forEach((a) => {
    a.target = '_blank';
    a.rel = 'noopener noreferrer';
  });

  target.querySelectorAll('pre code').forEach((code) => {
    hljs.highlightElement(code);
  });
};

const createToolCard = (message) => {
  const wrapper = document.createElement('article');
  wrapper.className = 'message tool';
  wrapper.dataset.messageId = message.id;

  const card = document.createElement('div');
  card.className = 'tool-card';

  const toggle = document.createElement('button');
  toggle.className = 'tool-toggle';
  toggle.type = 'button';
  toggle.setAttribute('aria-expanded', message.expanded ? 'true' : 'false');

  const arrow = document.createElement('span');
  arrow.className = 'tool-arrow';
  arrow.textContent = '▶';

  const name = document.createElement('span');
  name.className = 'tool-name';
  name.textContent = `${toolIcon(message.name)} ${message.name || 'tool'}`;

  const status = document.createElement('span');
  status.className = `tool-status${message.status === 'done' ? ' done' : ''}`;
  status.textContent = message.status === 'done' ? '[done]' : '[running…]';

  const details = document.createElement('div');
  details.className = `tool-details${message.expanded ? ' open' : ''}`;

  const label = document.createElement('div');
  label.className = 'tool-details-label';
  label.textContent = 'Arguments:';

  const args = document.createElement('pre');
  args.textContent = message.arguments || '(waiting for arguments…)';

  details.appendChild(label);
  details.appendChild(args);

  toggle.appendChild(arrow);
  toggle.appendChild(name);
  toggle.appendChild(status);

  toggle.addEventListener('click', () => {
    message.expanded = !message.expanded;
    toggle.setAttribute('aria-expanded', message.expanded ? 'true' : 'false');
    details.classList.toggle('open', message.expanded);
    saveSessions();
  });

  card.appendChild(toggle);
  card.appendChild(details);

  wrapper.appendChild(card);
  wrapper.appendChild(createMetaNode(message.created));
  return wrapper;
};

const createMessageNode = (message) => {
  if (message.role === 'tool') return createToolCard(message);
  if (message.role === 'tool-group') return createToolGroupNode(message);

  const article = document.createElement('article');
  article.className = `message ${message.role}`;
  article.dataset.messageId = message.id;

  const body = document.createElement('div');
  body.className = 'message-body';
  applyTextDirection(body, message.content || '');

  if (message.role === 'assistant') {
    body.classList.add('markdown-body');
    renderAssistantMarkdown(body, message.content || '');
  } else if (message.role === 'error') {
    body.textContent = `Error: ${message.content || 'Unknown error.'}`;
  } else {
    // User message: show attachments if present
    if (message.attachments && message.attachments.length > 0) {
      const attDiv = document.createElement('div');
      attDiv.className = 'message-attachments';
      message.attachments.forEach(att => {
        if (att.type && att.type.startsWith('image/') && att.dataURL) {
          const img = document.createElement('img');
          img.src = att.dataURL;
          img.alt = att.name || 'Attached image';
          img.style.cursor = 'pointer';
          img.addEventListener('click', () => app.openLightbox(att.dataURL));
          attDiv.appendChild(img);
        } else {
          const badge = document.createElement('span');
          badge.className = 'att-file';
          badge.textContent = att.name || 'file';
          attDiv.appendChild(badge);
        }
      });
      body.appendChild(attDiv);
    }
    if (message.content) {
      const textNode = document.createTextNode(message.content);
      body.appendChild(textNode);
    }
  }

  article.appendChild(body);

  if (message.role === 'assistant' && message.usage) {
    const usage = document.createElement('div');
    usage.className = 'usage-line';
    usage.textContent = formatUsage(message.usage);
    article.appendChild(usage);
  }

  article.appendChild(createMetaNode(message.created, message));
  return article;
};

const updateAssistantNode = (message) => {
  let node = findMessageElement(message.id);
  if (!node) {
    node = createMessageNode(message);
    elements.messages.appendChild(node);
  }

  const body = node.querySelector('.message-body');
  if (!body) return;
  applyTextDirection(body, message.content || '');
  renderAssistantMarkdown(body, message.content || '');

  let usageNode = node.querySelector('.usage-line');
  if (message.usage) {
    if (!usageNode) {
      usageNode = document.createElement('div');
      usageNode.className = 'usage-line';
      node.insertBefore(usageNode, node.querySelector('.message-meta'));
    }
    usageNode.textContent = formatUsage(message.usage);
  } else if (usageNode) {
    usageNode.remove();
  }
};

const updateUserNode = (message) => {
  const replacement = createMessageNode(message);
  const existing = findMessageElement(message.id);
  if (existing) {
    existing.replaceWith(replacement);
  } else {
    elements.messages.appendChild(replacement);
  }
};

const updateToolNode = (message) => {
  let node = findMessageElement(message.id);
  if (!node) {
    node = createToolCard(message);
    elements.messages.appendChild(node);
    return;
  }

  const toggle = node.querySelector('.tool-toggle');
  const status = node.querySelector('.tool-status');
  const details = node.querySelector('.tool-details');
  const args = node.querySelector('.tool-details pre');
  const name = node.querySelector('.tool-name');

  if (name) {
    name.textContent = `${toolIcon(message.name)} ${message.name || 'tool'}`;
  }
  if (status) {
    status.className = `tool-status${message.status === 'done' ? ' done' : ''}`;
    status.textContent = message.status === 'done' ? '[done]' : '[running…]';
  }
  if (toggle) {
    toggle.setAttribute('aria-expanded', message.expanded ? 'true' : 'false');
  }
  if (details) {
    details.classList.toggle('open', Boolean(message.expanded));
  }
  if (args) {
    args.textContent = message.arguments || '(waiting for arguments…)';
  }
};

const toolGroupSummaryText = (message) => {
  const total = message.tools.length;
  const done = message.tools.filter(t => t.status === 'done').length;
  if (message.status === 'done' || done === total) {
    return `${total} tool call${total === 1 ? '' : 's'} completed`;
  }
  return `Running ${total} tool${total === 1 ? '' : 's'}… (${done}/${total} done)`;
};

const createToolGroupNode = (message) => {
  const wrapper = document.createElement('article');
  wrapper.className = 'message tool-group';
  wrapper.dataset.messageId = message.id;

  const card = document.createElement('div');
  card.className = 'tool-group-card';

  const toggle = document.createElement('button');
  toggle.className = 'tool-group-toggle';
  toggle.type = 'button';
  toggle.setAttribute('aria-expanded', message.expanded ? 'true' : 'false');

  const arrow = document.createElement('span');
  arrow.className = 'tool-arrow';
  arrow.textContent = '▶';

  const summary = document.createElement('span');
  summary.className = 'tool-group-summary';
  summary.textContent = toolGroupSummaryText(message);

  const statusBadge = document.createElement('span');
  statusBadge.className = 'tool-status';
  if (message.status === 'done') {
    statusBadge.style.display = 'none';
    statusBadge.textContent = '';
  } else {
    statusBadge.textContent = 'running\u2026';
  }

  toggle.appendChild(arrow);
  toggle.appendChild(summary);
  toggle.appendChild(statusBadge);

  const details = document.createElement('div');
  details.className = `tool-group-details${message.expanded ? ' open' : ''}`;

  message.tools.forEach(tool => {
    details.appendChild(createToolEntryNode(tool));
  });

  toggle.addEventListener('click', () => {
    message.expanded = !message.expanded;
    toggle.setAttribute('aria-expanded', message.expanded ? 'true' : 'false');
    details.classList.toggle('open', message.expanded);
    saveSessions();
  });

  card.appendChild(toggle);
  card.appendChild(details);
  wrapper.appendChild(card);
  wrapper.appendChild(createMetaNode(message.created));
  return wrapper;
};

const formatToolArgs = (tool) => {
  if (!tool.arguments) return null;
  let args;
  try {
    args = typeof tool.arguments === 'string' ? JSON.parse(tool.arguments) : tool.arguments;
  } catch { return null; }
  if (!args || typeof args !== 'object') return null;

  const name = (tool.name || '').toLowerCase();

  // spawn_agent: show "@agent_name: truncated prompt"
  if (name === 'spawn_agent') {
    const agentName = args.agent_name || 'agent';
    let prompt = args.prompt || '';
    if (prompt.length > 120) prompt = prompt.slice(0, 117) + '…';
    return [['task', '@' + agentName + ': ' + prompt]];
  }

  if (name === 'ask_user') {
    const questions = Array.isArray(args.questions) ? args.questions : [];
    return questions.slice(0, 4).map((question, index) => [
      question.header || `question_${index + 1}`,
      question.question || ''
    ]);
  }

  // Pick the most relevant key(s) per tool type
  const priorityKeys = {
    'shell': ['command'],
    'bash': ['command'],
    'read_file': ['path', 'file_path'],
    'write_file': ['path', 'file_path'],
    'edit_file': ['path', 'file_path'],
    'web_search': ['query'],
    'search': ['query'],
    'grep': ['pattern', 'path'],
    'glob': ['pattern', 'path'],
  };

  const pick = priorityKeys[name];
  let entries;
  if (pick) {
    entries = pick.filter(k => args[k] != null).map(k => [k, args[k]]);
    // If no priority keys matched, fall back to all keys
    if (entries.length === 0) entries = Object.entries(args);
  } else {
    entries = Object.entries(args);
  }

  return entries.slice(0, 4); // Cap at 4 args to keep it compact
};

const buildArgsNode = (tool) => {
  const entries = formatToolArgs(tool);
  if (!entries || entries.length === 0) return null;

  const argsDiv = document.createElement('div');
  argsDiv.className = 'tool-entry-args';

  entries.forEach(([key, value]) => {
    const line = document.createElement('div');
    line.className = 'arg-line';

    const label = document.createElement('span');
    label.className = 'arg-label';
    label.textContent = key + ':';

    const val = document.createElement('span');
    val.className = 'arg-value';
    val.textContent = typeof value === 'string' ? value : JSON.stringify(value);

    line.appendChild(label);
    line.appendChild(val);
    argsDiv.appendChild(line);
  });

  return argsDiv;
};

const createToolEntryNode = (tool) => {
  const wrapper = document.createElement('div');
  wrapper.dataset.toolId = tool.id;

  const entry = document.createElement('div');
  entry.className = 'tool-group-entry';

  const icon = document.createElement('span');
  icon.textContent = toolIcon(tool.name);

  const name = document.createElement('span');
  name.className = 'tool-entry-name';
  name.textContent = tool.name || 'tool';

  const status = document.createElement('span');
  status.className = `tool-entry-status${tool.status === 'done' ? ' done' : ''}`;
  status.textContent = tool.status === 'done' ? '✓' : '…';

  entry.appendChild(icon);
  entry.appendChild(name);
  entry.appendChild(status);
  wrapper.appendChild(entry);

  const argsNode = buildArgsNode(tool);
  if (argsNode) wrapper.appendChild(argsNode);

  return wrapper;
};

const updateToolGroupNode = (message) => {
  let node = findMessageElement(message.id);
  if (!node) {
    node = createToolGroupNode(message);
    elements.messages.appendChild(node);
    return;
  }

  const summary = node.querySelector('.tool-group-summary');
  if (summary) summary.textContent = toolGroupSummaryText(message);

  const statusBadge = node.querySelector('.tool-status');
  if (statusBadge) {
    if (message.status === 'done') {
      statusBadge.style.display = 'none';
      statusBadge.textContent = '';
    } else {
      statusBadge.style.display = '';
      statusBadge.textContent = 'running\u2026';
    }
  }

  const details = node.querySelector('.tool-group-details');
  if (details) {
    // Update existing entries or add new ones
    message.tools.forEach(tool => {
      let entry = details.querySelector(`[data-tool-id="${CSS.escape(tool.id)}"]`);
      if (!entry) {
        entry = createToolEntryNode(tool);
        details.appendChild(entry);
      } else {
        const status = entry.querySelector('.tool-entry-status');
        if (status) {
          status.className = `tool-entry-status${tool.status === 'done' ? ' done' : ''}`;
          status.textContent = tool.status === 'done' ? '✓' : '…';
        }
        // Update or add arguments display
        const existingArgs = entry.querySelector('.tool-entry-args');
        const newArgs = buildArgsNode(tool);
        if (existingArgs && newArgs) {
          existingArgs.replaceWith(newArgs);
        } else if (!existingArgs && newArgs) {
          entry.appendChild(newArgs);
        }
      }
    });
  }
};

const renderMessages = (forceScroll = false) => {
  const session = ensureActiveSession();
  elements.messages.innerHTML = '';

  if (!session.messages.length) {
    const empty = document.createElement('div');
    empty.className = 'empty-state';
    empty.textContent = 'How can I help you today?';
    elements.messages.appendChild(empty);
  } else {
    session.messages.forEach((message) => {
      elements.messages.appendChild(createMessageNode(message));
    });
  }

  refreshRelativeTimes();
  scrollToBottom(forceScroll);
  updateHeader();
};

Object.assign(app, {
  openSidebar,
  closeSidebar,
  closeSidebarIfMobile,
  updateHeader,
  renderSidebar,
  directionForText,
  applyTextDirection,
  createInterruptBadgeNode,
  createMetaNode,
  renderAssistantMarkdown,
  createToolCard,
  createMessageNode,
  updateAssistantNode,
  updateUserNode,
  updateToolNode,
  toolGroupSummaryText,
  createToolGroupNode,
  formatToolArgs,
  buildArgsNode,
  createToolEntryNode,
  updateToolGroupNode,
  renderMessages
});
})();
