(() => {
'use strict';

const app = window.TermLLMApp;
const {
  STORAGE_KEYS, state, elements, INTERRUPT_BADGE_META, sanitizeInterruptState, relativeTime, fullDate, sessionBucket, toolIcon, formatUsage,
  saveSessions, findMessageElement, scrollToBottom, refreshRelativeTimes, ensureActiveSession, updateDocumentTitle,
  updateSessionUsageDisplay, renderMath, visibleSessions
} = app;

const isMobileViewport = () => window.matchMedia('(max-width: 767px)').matches;

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
  if (isMobileViewport()) {
    closeSidebar();
  }
};

const applySidebarToggleButtonState = () => {
  const expanded = !state.sidebarCollapsed;
  // Rail toggle (visible when collapsed)
  if (elements.sidebarToggleBtn) {
    elements.sidebarToggleBtn.title = 'Expand sidebar';
    elements.sidebarToggleBtn.setAttribute('aria-label', 'Expand sidebar');
    elements.sidebarToggleBtn.setAttribute('aria-expanded', 'false');
  }
  // Panel toggle (visible when expanded)
  if (elements.sidebarPanelToggleBtn) {
    elements.sidebarPanelToggleBtn.title = expanded ? 'Collapse sidebar' : 'Expand sidebar';
    elements.sidebarPanelToggleBtn.setAttribute('aria-label', expanded ? 'Collapse sidebar' : 'Expand sidebar');
    elements.sidebarPanelToggleBtn.setAttribute('aria-expanded', expanded ? 'true' : 'false');
  }
};

const applyDesktopSidebarState = () => {
  const collapsed = !isMobileViewport() && state.sidebarCollapsed;
  elements.appShell?.classList.toggle('sidebar-collapsed', collapsed);
  elements.sidebar?.classList.toggle('collapsed', collapsed);
  applySidebarToggleButtonState();
};

const setSidebarCollapsed = (collapsed) => {
  state.sidebarCollapsed = Boolean(collapsed);
  localStorage.setItem(STORAGE_KEYS.sidebarCollapsed, state.sidebarCollapsed ? '1' : '0');
  applyDesktopSidebarState();
};

const toggleSidebarCollapsed = () => {
  if (isMobileViewport()) {
    openSidebar();
    return;
  }
  setSidebarCollapsed(!state.sidebarCollapsed);
};

const updateHeader = () => {
  const session = ensureActiveSession();
  elements.activeSessionTitle.textContent = session?.title || 'Chat';
  updateDocumentTitle();
  updateSessionUsageDisplay(session);
  applyDesktopSidebarState();
};

const closeAllSessionMenus = () => {
  elements.sessionGroups.querySelectorAll('.session-row.menu-open').forEach((row) => {
    row.classList.remove('menu-open');
  });
};

const SESSION_MENU_ICONS = {
  pin: '<svg viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"><path d="M4.146.146A.5.5 0 0 1 4.5 0h7a.5.5 0 0 1 .5.5c0 .68-.342 1.174-.646 1.479-.126.125-.25.224-.354.298v4.431l.078.048c.203.127.476.314.751.555C12.36 7.775 13 8.527 13 9.5a.5.5 0 0 1-.5.5h-4v4.5c0 .276-.224 1.5-.5 1.5s-.5-1.224-.5-1.5V10h-4a.5.5 0 0 1-.5-.5c0-.973.64-1.725 1.17-2.189A6 6 0 0 1 5 6.708V2.277a3 3 0 0 1-.354-.298C4.342 1.674 4 1.179 4 .5a.5.5 0 0 1 .146-.354"/></svg>',
  unpin: '<svg viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"><g transform="rotate(38 8 8)"><path d="M4.146.146A.5.5 0 0 1 4.5 0h7a.5.5 0 0 1 .5.5c0 .68-.342 1.174-.646 1.479-.126.125-.25.224-.354.298v4.431l.078.048c.203.127.476.314.751.555C12.36 7.775 13 8.527 13 9.5a.5.5 0 0 1-.5.5h-4v4.5c0 .276-.224 1.5-.5 1.5s-.5-1.224-.5-1.5V10h-4a.5.5 0 0 1-.5-.5c0-.973.64-1.725 1.17-2.189A6 6 0 0 1 5 6.708V2.277a3 3 0 0 1-.354-.298C4.342 1.674 4 1.179 4 .5a.5.5 0 0 1 .146-.354"/></g></svg>',
  rename: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 20h9"/><path d="M16.5 3.5a2.12 2.12 0 1 1 3 3L7 19l-4 1 1-4 12.5-12.5Z"/></svg>',
  hide: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M17.94 17.94A10.94 10.94 0 0 1 12 20C7 20 2.73 16.89 1 12c.92-2.6 2.63-4.77 4.83-6.2"/><path d="M9.88 9.88a3 3 0 1 0 4.24 4.24"/><path d="M10.73 5.08A11.02 11.02 0 0 1 12 5c5 0 9.27 3.11 11 7a11.05 11.05 0 0 1-2.16 3.19"/><path d="M1 1l22 22"/></svg>',
  unhide: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M1 12s4-7 11-7 11 7 11 7-4 7-11 7S1 12 1 12Z"/><circle cx="12" cy="12" r="3"/></svg>'
};

const createSessionMenuButton = (label, iconName, onClick) => {
  const button = document.createElement('button');
  button.type = 'button';

  const icon = document.createElement('span');
  icon.className = 'session-menu-icon';
  icon.innerHTML = SESSION_MENU_ICONS[iconName] || '';

  const text = document.createElement('span');
  text.className = 'session-menu-label';
  text.textContent = label;

  button.appendChild(icon);
  button.appendChild(text);
  button.addEventListener('click', onClick);
  return button;
};

document.addEventListener('click', (event) => {
  if (!event.target.closest('.session-row-menu')) {
    closeAllSessionMenus();
  }
});

const renderSidebar = () => {
  const grouped = {
    Pinned: [],
    Today: [],
    Yesterday: [],
    'This week': [],
    Older: []
  };

  const sorted = [...visibleSessions()].sort((a, b) => b.created - a.created);
  sorted.forEach((session) => {
    if (session.pinned) {
      grouped.Pinned.push(session);
    } else {
      grouped[sessionBucket(session.created)].push(session);
    }
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
      const row = document.createElement('div');
      row.className = 'session-row';
      row.dataset.sessionId = session.id;

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
      const metaParts = [`${msgCount} message${msgCount === 1 ? '' : 's'}`];
      if (session.archived) {
        metaParts.push('hidden');
      }
      metaParts.push(relativeTime(session.created));
      meta.textContent = metaParts.join(' · ');
      meta.title = fullDate(session.created);

      btn.appendChild(title);
      btn.appendChild(meta);
      btn.addEventListener('click', async () => {
        await app.switchToSession(session.id);
      });

      const menuWrap = document.createElement('div');
      menuWrap.className = 'session-row-menu';

      const actionBtn = document.createElement('button');
      actionBtn.className = 'session-menu-trigger';
      actionBtn.type = 'button';
      actionBtn.textContent = '⋯';
      actionBtn.title = 'Session actions';
      actionBtn.setAttribute('aria-label', 'Session actions');
      actionBtn.addEventListener('click', (event) => {
        event.preventDefault();
        event.stopPropagation();
        const willOpen = !row.classList.contains('menu-open');
        closeAllSessionMenus();
        row.classList.toggle('menu-open', willOpen);
      });

      const menu = document.createElement('div');
      menu.className = 'session-menu';

      const renameBtn = createSessionMenuButton('Rename', 'rename', async (event) => {
        event.preventDefault();
        event.stopPropagation();
        closeAllSessionMenus();
        await app.promptRenameSession(session);
      });

      const pinBtn = createSessionMenuButton(session.pinned ? 'Unpin' : 'Pin', session.pinned ? 'unpin' : 'pin', async (event) => {
        event.preventDefault();
        event.stopPropagation();
        closeAllSessionMenus();
        await app.setSessionPinned(session, !session.pinned);
      });

      const archiveBtn = createSessionMenuButton(session.archived ? 'Unhide' : 'Hide', session.archived ? 'unhide' : 'hide', async (event) => {
        event.preventDefault();
        event.stopPropagation();
        closeAllSessionMenus();
        await app.setSessionArchived(session, !session.archived);
      });

      menu.appendChild(renameBtn);
      menu.appendChild(pinBtn);
      menu.appendChild(archiveBtn);
      menuWrap.appendChild(actionBtn);
      menuWrap.appendChild(menu);

      row.appendChild(btn);
      row.appendChild(menuWrap);
      groupEl.appendChild(row);
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

const assistantStreamStates = new Map();

const decorateAssistantFragment = (target) => {
  if (!target) return;
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

const renderAssistantMarkdown = (target, content) => {
  if (!target) return;
  applyTextDirection(target, content || '');
  const html = marked.parse(content || '');
  const clean = DOMPurify.sanitize(html, {
    ADD_TAGS: ['video', 'source'],
    ADD_ATTR: ['controls', 'playsinline', 'muted', 'loop', 'autoplay', 'poster', 'preload']
  });
  target.innerHTML = clean;
  decorateAssistantFragment(target);
};

const disposeAssistantStreamState = (messageId) => {
  const streamState = assistantStreamStates.get(messageId);
  if (!streamState) return;
  if (streamState.rafId) {
    window.cancelAnimationFrame(streamState.rafId);
  }
  if (streamState.timerId) {
    window.clearTimeout(streamState.timerId);
  }
  assistantStreamStates.delete(messageId);
};

const resetAssistantStreamRenders = () => {
  Array.from(assistantStreamStates.keys()).forEach((messageId) => {
    disposeAssistantStreamState(messageId);
  });
};

const syncAssistantUsageNode = (node, message) => {
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

const createAssistantStreamContainers = (body) => {
  body.innerHTML = '';
  const stableContainer = document.createElement('div');
  stableContainer.className = 'markdown-stream-stable';
  const tailContainer = document.createElement('div');
  tailContainer.className = 'markdown-stream-tail';
  body.appendChild(stableContainer);
  body.appendChild(tailContainer);
  return { stableContainer, tailContainer };
};

const getOrCreateAssistantStreamState = (message, body) => {
  let streamState = assistantStreamStates.get(message.id);
  if (streamState && streamState.body === body) {
    return streamState;
  }
  disposeAssistantStreamState(message.id);
  streamState = app.markdownStreaming && typeof app.markdownStreaming.createStreamingState === 'function'
    ? app.markdownStreaming.createStreamingState()
    : {
      messageId: '',
      body: null,
      stableContainer: null,
      tailContainer: null,
      committedLength: 0,
      latestContent: '',
      lastTailContent: '',
      dirty: false,
      rendering: false,
      rafId: 0,
      timerId: 0,
      lastRenderAt: 0
    };
  const containers = createAssistantStreamContainers(body);
  streamState.messageId = message.id;
  streamState.body = body;
  streamState.stableContainer = containers.stableContainer;
  streamState.tailContainer = containers.tailContainer;
  assistantStreamStates.set(message.id, streamState);
  return streamState;
};

const scheduleAssistantStreamRender = (streamState) => {
  if (!streamState) return;
  const renderDelay = app.markdownStreaming && typeof app.markdownStreaming.nextStreamingRenderDelay === 'function'
    ? app.markdownStreaming.nextStreamingRenderDelay(streamState.latestContent.length)
    : 33;
  const elapsed = Date.now() - streamState.lastRenderAt;
  const enqueueFrame = () => {
    streamState.timerId = 0;
    if (streamState.rafId) return;
    streamState.rafId = window.requestAnimationFrame(() => performAssistantStreamRender(streamState));
  };

  if (streamState.rendering || streamState.rafId || streamState.timerId) return;
  if (elapsed >= renderDelay) {
    enqueueFrame();
    return;
  }
  streamState.timerId = window.setTimeout(enqueueFrame, renderDelay - elapsed);
};

const performAssistantStreamRender = (streamState) => {
  if (!streamState || !streamState.body) return;
  streamState.rafId = 0;

  if (!document.body.contains(streamState.body)) {
    disposeAssistantStreamState(streamState.messageId);
    return;
  }

  if (streamState.rendering) return;

  streamState.rendering = true;
  streamState.dirty = false;
  const content = String(streamState.latestContent || '');

  try {
    applyTextDirection(streamState.body, content);

    const nextBoundary = app.markdownStreaming && typeof app.markdownStreaming.findStreamingBoundary === 'function'
      ? app.markdownStreaming.findStreamingBoundary(content, streamState.committedLength)
      : -1;

    if (Number.isInteger(nextBoundary) && nextBoundary > streamState.committedLength) {
      const committedChunk = content.slice(streamState.committedLength, nextBoundary);
      if (committedChunk) {
        const piece = document.createElement('div');
        piece.className = 'markdown-stream-piece';
        renderAssistantMarkdown(piece, committedChunk);
        streamState.stableContainer.appendChild(piece);
      }
      streamState.committedLength = nextBoundary;
    }

    const tail = content.slice(streamState.committedLength);
    if (tail !== streamState.lastTailContent) {
      if (tail) {
        renderAssistantMarkdown(streamState.tailContainer, tail);
      } else {
        streamState.tailContainer.innerHTML = '';
      }
      streamState.lastTailContent = tail;
    }

    streamState.lastRenderAt = Date.now();
    app.scheduleStreamPersistence?.();
    app.scheduleStreamScroll?.();
  } finally {
    streamState.rendering = false;
    if (streamState.dirty || streamState.latestContent !== content) {
      scheduleAssistantStreamRender(streamState);
    }
  }
};

const renderAssistantNodeFully = (node, message) => {
  const body = node.querySelector('.message-body');
  if (!body) return;
  disposeAssistantStreamState(message.id);
  body.classList.add('markdown-body');
  renderAssistantMarkdown(body, message.content || '');
  syncAssistantUsageNode(node, message);
};

const enqueueAssistantStreamUpdate = (message) => {
  if (!app.markdownStreaming) {
    updateAssistantNode(message);
    if (typeof app.scheduleStreamScroll === 'function') {
      app.scheduleStreamScroll();
    } else {
      scrollToBottom();
    }
    return;
  }

  let node = findMessageElement(message.id);
  if (!node) {
    node = createMessageNode({ ...message, content: '' });
    elements.messages.appendChild(node);
  }

  const body = node.querySelector('.message-body');
  if (!body) return;

  body.classList.add('markdown-body');
  const streamState = getOrCreateAssistantStreamState(message, body);
  streamState.latestContent = String(message.content || '');
  streamState.dirty = true;
  syncAssistantUsageNode(node, message);
  scheduleAssistantStreamRender(streamState);
};

const finalizeAssistantStreamRender = (message) => {
  let node = findMessageElement(message.id);
  if (!node) {
    node = createMessageNode(message);
    elements.messages.appendChild(node);
    return;
  }
  renderAssistantNodeFully(node, message);
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
    return;
  }
  renderAssistantNodeFully(node, message);
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
  resetAssistantStreamRenders();
  elements.messages.innerHTML = '';

  if (!session || !session.messages.length) {
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

const updateSidebarStatus = (statusSessions) => {
  if (!Array.isArray(statusSessions)) return false;
  let changed = false;
  for (const entry of statusSessions) {
    const row = elements.sessionGroups.querySelector(`.session-row[data-session-id="${CSS.escape(entry.id)}"]`);
    if (!row) continue;
    const wasActive = row.classList.contains('is-active');
    row.classList.toggle('is-active', Boolean(entry.active_run));
    if (wasActive !== Boolean(entry.active_run)) changed = true;

    const titleEl = row.querySelector('.session-title');
    if (titleEl && entry.short_title && titleEl.textContent !== entry.short_title) {
      titleEl.textContent = entry.short_title;
      if (entry.long_title) titleEl.title = entry.long_title;
      changed = true;
    }

    const metaEl = row.querySelector('.session-meta');
    if (metaEl && entry.message_count != null) {
      const count = entry.message_count;
      const local = state.sessions.find(s => s.id === entry.id);
      if (local) {
        if (entry.short_title) local.title = entry.short_title;
        if (entry.long_title) local.longTitle = entry.long_title;
        local.messageCount = count;
      }
      const parts = [`${count} message${count === 1 ? '' : 's'}`];
      if (local?.archived) parts.push('hidden');
      parts.push(relativeTime(local?.created || Date.now()));
      metaEl.textContent = parts.join(' · ');
    }
  }
  return changed;
};

Object.assign(app, {
  openSidebar,
  closeSidebar,
  closeSidebarIfMobile,
  applyDesktopSidebarState,
  setSidebarCollapsed,
  toggleSidebarCollapsed,
  updateHeader,
  renderSidebar,
  updateSidebarStatus,
  directionForText,
  applyTextDirection,
  createInterruptBadgeNode,
  createMetaNode,
  renderAssistantMarkdown,
  enqueueAssistantStreamUpdate,
  finalizeAssistantStreamRender,
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
  resetAssistantStreamRenders,
  renderMessages
});
})();
