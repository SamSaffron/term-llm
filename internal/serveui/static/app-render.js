(() => {
'use strict';

const app = window.TermLLMApp;
const {
  STORAGE_KEYS, state, elements, INTERRUPT_BADGE_META, sanitizeInterruptState, relativeTime, fullDate, sessionBucket, toolIcon, formatUsage,
  saveSessions, findMessageElement, scrollToBottom, refreshRelativeTimes, ensureActiveSession, updateDocumentTitle,
  updateSessionUsageDisplay, renderMath, visibleSessions, sessionHasInProgressState, setSessionServerActiveRun
} = app;

const isMobileViewport = () => window.matchMedia('(max-width: 767px)').matches;

const directionForText = (value) => {
  const text = String(value || '');
  const m = text.match(/[A-Za-z\u00C0-\u02AF\u0370-\u03FF\u0400-\u052F\u0590-\u08FF]/);
  if (!m) return 'auto';
  return /[\u0590-\u08FF]/.test(m[0]) ? 'rtl' : 'ltr';
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

  const sorted = [...visibleSessions()].sort((a, b) => {
    const aAt = a.lastMessageAt || a.created;
    const bAt = b.lastMessageAt || b.created;
    return bAt - aAt;
  });
  sorted.forEach((session) => {
    if (session.pinned) {
      grouped.Pinned.push(session);
    } else {
      grouped[sessionBucket(session.lastMessageAt || session.created)].push(session);
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
      row.classList.toggle('is-active', sessionHasInProgressState(session));

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
      const msgCount = session.messageCount || session.messages.filter(m => m.role !== 'tool-group').length || 0;
      const metaParts = [`${msgCount} message${msgCount === 1 ? '' : 's'}`];
      if (session.archived) {
        metaParts.push('hidden');
      }
      const activityAt = session.lastMessageAt || session.created;
      metaParts.push(relativeTime(activityAt));
      meta.textContent = metaParts.join(' · ');
      meta.title = fullDate(activityAt);

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

const OPTIONAL_MARKDOWN_ASSETS = {
  katexCSS: 'vendor/katex/katex.min.css?v=0.16.38',
  katexJS: 'vendor/katex/katex.min.js?v=0.16.38',
  katexAutoRenderJS: 'vendor/katex/auto-render.min.js?v=0.16.38',
  hljsDarkCSS: 'vendor/hljs/github-dark.min.css?v=11.11.1',
  hljsLightCSS: 'vendor/hljs/github.min.css?v=11.11.1',
  hljsJS: 'vendor/hljs/highlight.min.js?v=11.11.1'
};

const optionalAssetLoads = new Map();

const optionalAssetParent = () => document.head || document.documentElement || document.body;

const ensureStylesheetLoaded = (href, options = {}) => {
  if (!href) return Promise.resolve(false);
  const key = `style:${href}:${options.media || ''}`;
  if (optionalAssetLoads.has(key)) return optionalAssetLoads.get(key);

  const promise = new Promise((resolve) => {
    const link = document.createElement('link');
    link.rel = 'stylesheet';
    link.href = href;
    if (options.media) link.media = options.media;
    link.dataset.termLlmOptionalAsset = 'true';
    link.onload = () => resolve(true);
    link.onerror = () => resolve(false);
    optionalAssetParent().appendChild(link);
  });
  optionalAssetLoads.set(key, promise);
  return promise;
};

const ensureScriptLoaded = (src, isReady = null) => {
  if (!src) return Promise.resolve(false);
  if (typeof isReady === 'function' && isReady()) return Promise.resolve(true);
  const key = `script:${src}`;
  if (optionalAssetLoads.has(key)) return optionalAssetLoads.get(key);

  const promise = new Promise((resolve) => {
    const script = document.createElement('script');
    script.src = src;
    script.async = true;
    script.dataset.termLlmOptionalAsset = 'true';
    script.onload = () => resolve(true);
    script.onerror = () => resolve(false);
    optionalAssetParent().appendChild(script);
  });
  optionalAssetLoads.set(key, promise);
  return promise;
};

const ensureKatexLoaded = () => {
  ensureStylesheetLoaded(OPTIONAL_MARKDOWN_ASSETS.katexCSS);
  return ensureScriptLoaded(OPTIONAL_MARKDOWN_ASSETS.katexJS, () => Boolean(window.katex))
    .then((loaded) => (loaded ? ensureScriptLoaded(
      OPTIONAL_MARKDOWN_ASSETS.katexAutoRenderJS,
      () => typeof window.renderMathInElement === 'function'
    ) : false))
    .then(() => typeof window.renderMathInElement === 'function');
};

const ensureHighlightLoaded = () => {
  ensureStylesheetLoaded(OPTIONAL_MARKDOWN_ASSETS.hljsDarkCSS);
  ensureStylesheetLoaded(OPTIONAL_MARKDOWN_ASSETS.hljsLightCSS, { media: '(prefers-color-scheme: light)' });
  return ensureScriptLoaded(OPTIONAL_MARKDOWN_ASSETS.hljsJS, () => Boolean(window.hljs))
    .then(() => Boolean(window.hljs));
};

const sourceContainsMathDelimiters = (content) => {
  const text = String(content || '');
  return text.includes('\\(') || text.includes('\\[') || text.includes('$$');
};

const isAttachedToDocument = (target) => {
  if (!target || !document.body || typeof document.body.contains !== 'function') return true;
  return document.body.contains(target);
};

const highlightCodeBlocks = (target) => {
  const highlighter = window.hljs;
  if (!target || !highlighter || typeof highlighter.highlightElement !== 'function') return;
  target.querySelectorAll('pre code').forEach((code) => {
    if (code.dataset.termLlmHighlighted === 'true' || code.dataset.highlighted === 'yes') return;
    if (/\blanguage-\w+/.test(code.className)) {
      highlighter.highlightElement(code);
      code.dataset.termLlmHighlighted = 'true';
    }
  });
};

const enhanceMathAsync = (target) => {
  ensureKatexLoaded().then((loaded) => {
    if (!loaded || !isAttachedToDocument(target)) return;
    renderMath(target);
  }).catch(() => {});
};

const enhanceHighlightAsync = (target) => {
  ensureHighlightLoaded().then((loaded) => {
    if (!loaded || !isAttachedToDocument(target)) return;
    highlightCodeBlocks(target);
  }).catch(() => {});
};

const decorateAssistantFragment = (target, options = {}) => {
  if (!target) return;
  const streaming = Boolean(options.streaming);
  deferEmbeddedVideos(target);
  target.querySelectorAll('a').forEach((a) => {
    a.target = '_blank';
    a.rel = 'noopener noreferrer';
  });
  window.TermLLMDecoration.decorateLightbox(target, options, (...args) => app.openLightbox(...args));
  if (!streaming) {
    if (sourceContainsMathDelimiters(options.source || target.textContent || '')) {
      enhanceMathAsync(target);
    }
    if (target.querySelectorAll('pre code').length > 0) {
      enhanceHighlightAsync(target);
    }
  }
  target.querySelectorAll('pre').forEach((pre) => {
    if (streaming) {
      const existingBtn = pre.querySelector('.code-copy-btn');
      if (existingBtn) existingBtn.remove();
      return;
    }
    if (pre.querySelector('.code-copy-btn')) return;
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'code-copy-btn';
    btn.title = 'Copy';
    btn.setAttribute('aria-label', 'Copy code');
    btn.innerHTML = '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>';
    btn.addEventListener('click', () => {
      const code = pre.querySelector('code');
      const text = code ? code.textContent : pre.textContent;
      navigator.clipboard.writeText(text).then(() => {
        btn.classList.add('copied');
        btn.innerHTML = '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg>';
        setTimeout(() => {
          btn.classList.remove('copied');
          btn.innerHTML = '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>';
        }, 1500);
      });
    });
    pre.style.position = 'relative';
    pre.appendChild(btn);
  });
};

const renderAssistantMarkdown = (target, content, options = {}) => {
  if (!target) return;
  applyTextDirection(target, content || '');
  const html = marked.parse(content || '');
  const clean = DOMPurify.sanitize(html, {
    ADD_TAGS: ['video', 'source'],
    ADD_ATTR: ['controls', 'playsinline', 'muted', 'loop', 'autoplay', 'poster', 'preload']
  });
  target.innerHTML = clean;
  decorateAssistantFragment(target, { ...options, source: content || '' });
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

const STREAM_STABLE_MIN_TAIL_LENGTH = 256;

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
      stableSource: '',
      stableLength: 0,
      latestContent: '',
      lastTailContent: '',
      lastTailSource: '',
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
  streamState._canPlainCached = null;
  streamState._canPlainCachedAt = 0;
  streamState._stableCheckedAt = 0;
  assistantStreamStates.set(message.id, streamState);
  return streamState;
};

const scheduleAssistantStreamRender = (streamState) => {
  if (!streamState || streamState.rendering || streamState.rafId || streamState.timerId) return;
  const renderDelay = app.markdownStreaming && typeof app.markdownStreaming.nextStreamingRenderDelay === 'function'
    ? app.markdownStreaming.nextStreamingRenderDelay(streamState.latestContent.length)
    : 33;
  const elapsed = Date.now() - streamState.lastRenderAt;
  const enqueueFrame = () => {
    streamState.timerId = 0;
    if (streamState.rafId) return;
    streamState.rafId = window.requestAnimationFrame(() => performAssistantStreamRender(streamState));
  };
  if (elapsed >= renderDelay) {
    enqueueFrame();
    return;
  }
  streamState.timerId = window.setTimeout(enqueueFrame, renderDelay - elapsed);
};

const clearAssistantTailRender = (streamState) => {
  if (!streamState?.tailContainer) return;
  streamState.tailContainer.classList.remove('streaming-plain-text');
  streamState.tailContainer.innerHTML = '';
  streamState.tailTextNode = null;
  streamState.lastTailSource = '';
};

const resetAssistantStableRender = (streamState) => {
  if (!streamState) return;
  if (streamState.stableContainer) {
    streamState.stableContainer.innerHTML = '';
  }
  streamState.stableSource = '';
  streamState.stableLength = 0;
  streamState.lastTailContent = '';
  streamState.lastTailSource = '';
  streamState.tailTextNode = null;
  streamState._canPlainCached = null;
  streamState._canPlainCachedAt = 0;
  streamState._stableCheckedAt = 0;
};

const appendAssistantStableMarkdown = (streamState, source) => {
  if (!streamState?.stableContainer || !source) return;
  const piece = document.createElement('div');
  piece.className = 'markdown-stream-piece';
  renderAssistantMarkdown(piece, source, { streaming: true });
  streamState.stableContainer.appendChild(piece);
  streamState.stableSource = `${streamState.stableSource || ''}${source}`;
  streamState.stableLength = streamState.stableSource.length;
};

const promoteAssistantStableMarkdown = (streamState, content) => {
  if (!streamState?.stableContainer || !app.markdownStreaming || typeof app.markdownStreaming.findStableMarkdownBoundary !== 'function') {
    return false;
  }

  const stableSource = streamState.stableSource || '';
  if (stableSource && !content.startsWith(stableSource)) {
    resetAssistantStableRender(streamState);
    clearAssistantTailRender(streamState);
  }

  const start = Math.max(0, Number(streamState.stableLength) || 0);
  if (start > content.length) {
    resetAssistantStableRender(streamState);
    clearAssistantTailRender(streamState);
  }

  const uncommitted = content.slice(streamState.stableLength || 0);
  const boundary = app.markdownStreaming.findStableMarkdownBoundary(uncommitted, STREAM_STABLE_MIN_TAIL_LENGTH);
  if (!boundary || boundary <= 0) return false;

  appendAssistantStableMarkdown(streamState, uncommitted.slice(0, boundary));
  return true;
};

const renderAssistantTailPlainText = (streamState, tail) => {
  const container = streamState?.tailContainer;
  if (!container) return;
  container.classList.add('streaming-plain-text');

  let textNode = streamState.tailTextNode;
  if (!textNode || textNode.parentNode !== container) {
    container.innerHTML = '';
    textNode = document.createTextNode('');
    container.appendChild(textNode);
    streamState.tailTextNode = textNode;
    streamState.lastTailSource = '';
  }

  const prevLen = (streamState.lastTailSource || '').length;
  if (prevLen > 0 && tail.length > prevLen) {
    textNode.textContent += tail.slice(prevLen);
  } else {
    textNode.textContent = tail;
  }

  streamState.lastTailSource = tail;
};

const renderAssistantTailMarkdown = (streamState, tail) => {
  const container = streamState?.tailContainer;
  if (!container) return;
  container.classList.remove('streaming-plain-text');
  streamState.tailTextNode = null;
  renderAssistantMarkdown(container, tail, { streaming: true });
  streamState.lastTailSource = tail;
};

// Returns true when content qualifies for the fast plain-text tail path,
// using a two-level cache to avoid O(n) re-scans on every render frame:
//   false result: permanent — structural markdown (block syntax, inline
//     markers, math delimiters) can never be removed by appending, so false
//     stays false for the lifetime of the message.
//   true result: reused when the new delta contains no markdown-triggering
//     characters, skipping the full six-pass scan entirely.
const hasPotentialMarkdownChars = (text) => /[`*_~[\]!<>\\$|#\n]/.test(text);

const cachedCanStreamPlainText = (streamState, content, ms) => {
  const prev = streamState._canPlainCached;
  const prevLen = streamState._canPlainCachedAt || 0;

  if (prev !== null && content.length === prevLen) return prev;

  if (prev === false && content.length > prevLen) {
    streamState._canPlainCachedAt = content.length;
    return false;
  }

  if (prev === true && content.length > prevLen) {
    const delta = content.slice(prevLen);
    if (!hasPotentialMarkdownChars(delta)) {
      streamState._canPlainCachedAt = content.length;
      return true;
    }
  }

  const result = ms.canStreamPlainTextTail(content);
  streamState._canPlainCached = result;
  streamState._canPlainCachedAt = content.length;
  return result;
};

const maybePromoteAssistantStableMarkdown = (streamState, content) => {
  const prevAt = streamState._stableCheckedAt || 0;
  if (prevAt > 0 && content.length > prevAt && !content.slice(prevAt).includes('\n')) {
    streamState._stableCheckedAt = content.length;
    return false;
  }
  const result = promoteAssistantStableMarkdown(streamState, content);
  streamState._stableCheckedAt = content.length;
  return result;
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
    // Skip the O(n) direction scan once the body direction is locked in.
    // Direction is determined by the first strong bidi character and never
    // changes as more text is appended, so one scan per element is enough.
    const bodyDir = streamState.body.getAttribute('dir');
    if (bodyDir !== 'ltr' && bodyDir !== 'rtl') {
      applyTextDirection(streamState.body, content);
    }

    if (content) {
      // When stable markdown has already been promoted (stableLength > 0) the
      // plain-text path is unreachable — skip the O(n) eligibility scan.
      // Otherwise use the incremental cache to avoid re-scanning unchanged prefixes.
      const ms = app.markdownStreaming;
      const renderPlainTail = !(streamState.stableLength > 0) && Boolean(
        ms && typeof ms.canStreamPlainTextTail === 'function'
        && cachedCanStreamPlainText(streamState, content, ms)
      );

      if (renderPlainTail) {
        if (content !== streamState.lastTailContent) {
          renderAssistantTailPlainText(streamState, content);
          streamState.lastTailContent = content;
        }
      } else {
        const promoted = maybePromoteAssistantStableMarkdown(streamState, content);
        const tail = content.slice(streamState.stableLength || 0);
        if (promoted || tail !== streamState.lastTailContent) {
          if (tail) {
            renderAssistantTailMarkdown(streamState, tail);
          } else {
            clearAssistantTailRender(streamState);
          }
          streamState.lastTailContent = tail;
        }
      }
    } else {
      resetAssistantStableRender(streamState);
      clearAssistantTailRender(streamState);
      streamState.lastTailContent = '';
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

  // Fast path: state exists — skip all DOM queries on every delta after the first.
  const existingState = assistantStreamStates.get(message.id);
  if (existingState) {
    existingState.latestContent = String(message.content || '');
    existingState.dirty = true;
    scheduleAssistantStreamRender(existingState);
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
  syncTurnActionPanelForAssistant(message.id);
  scheduleAssistantStreamRender(streamState);
};

const finalizeAssistantStreamRender = (message) => {
  let node = findMessageElement(message.id);
  if (!node) {
    node = createMessageNode(message);
    elements.messages.appendChild(node);
    syncTurnActionPanelForAssistant(message.id);
    return;
  }
  renderAssistantNodeFully(node, message);
  syncTurnActionPanelForAssistant(message.id);
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

const createModelSwapNode = (message) => {
  const article = document.createElement('article');
  article.className = 'message model-swap';
  article.dataset.messageId = message.id;

  const body = document.createElement('div');
  body.className = 'message-body model-swap-body';
  body.textContent = message.content || '↔ Model switch';
  article.appendChild(body);
  article.appendChild(createMetaNode(message.created, message));
  return article;
};

const updateModelSwapNode = (message) => {
  let node = findMessageElement(message.id);
  if (!node) {
    node = createModelSwapNode(message);
    elements.messages.appendChild(node);
    return;
  }
  const body = node.querySelector('.message-body');
  if (body) body.textContent = message.content || '↔ Model switch';
};

const createMessageNode = (message) => {
  if (message.role === 'tool') return createToolCard(message);
  if (message.role === 'tool-group') return createToolGroupNode(message);
  if (message.role === 'model-swap') return createModelSwapNode(message);

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
        const previewURL = att.previewURL || att.dataURL || '';
        if (att.type && att.type.startsWith('image/') && previewURL) {
          const img = document.createElement('img');
          img.src = previewURL;
          img.alt = att.name || 'Attached image';
          img.style.cursor = 'pointer';
          img.addEventListener('click', () => app.openLightbox(previewURL));
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
    syncTurnActionPanelForAssistant(message.id);
    return;
  }
  renderAssistantNodeFully(node, message);
  syncTurnActionPanelForAssistant(message.id);
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

const isBlankToolArgValue = (value) => {
  if (value == null) return true;
  if (typeof value === 'string') return value.trim() === '';
  if (Array.isArray(value)) return value.length === 0;
  if (typeof value === 'object') return Object.keys(value).length === 0;
  return false;
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

  // image_generate may carry internal upload paths before the model's prompt
  // arrives; hide those incidental args until the prompt is available.
  if (name === 'image_generate') {
    if (isBlankToolArgValue(args.prompt)) return [];
    const entries = [['prompt', args.prompt]];
    ['aspect_ratio', 'size'].forEach((key) => {
      if (Object.prototype.hasOwnProperty.call(args, key) && !isBlankToolArgValue(args[key])) {
        entries.push([key, args[key]]);
      }
    });
    const inputCount = (Array.isArray(args.input_images) ? args.input_images.length : 0)
      + (isBlankToolArgValue(args.input_image) ? 0 : 1);
    if (inputCount > 0) {
      entries.push(['input', `${inputCount} attached image${inputCount === 1 ? '' : 's'}`]);
    }
    return entries.slice(0, 4);
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

  const allEntries = Object.entries(args).filter(([, value]) => !isBlankToolArgValue(value));
  const pick = priorityKeys[name];
  let entries;
  if (pick) {
    entries = pick
      .filter(k => Object.prototype.hasOwnProperty.call(args, k) && !isBlankToolArgValue(args[k]))
      .map(k => [k, args[k]]);
    // If no priority keys matched, fall back to all non-blank keys
    if (entries.length === 0) entries = allEntries;
  } else {
    entries = allEntries;
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

const TURN_COPY_ICON = '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>';
const TURN_COPIED_ICON = '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="20 6 9 17 4 12"/></svg>';
const TURN_COPY_RESET_MS = 1500;

const isNormalUserBoundary = (message) => (
  message?.role === 'user' && !message.askUser
);

const getAssistantTurns = (session) => {
  const messages = Array.isArray(session?.messages) ? session.messages : [];
  const turns = [];
  let items = [];

  const flush = () => {
    if (items.length === 0) return;
    const assistantsWithContent = items.filter((message) => (
      message?.role === 'assistant' && String(message.content || '').trim()
    ));
    if (assistantsWithContent.length > 0) {
      const lastAssistant = assistantsWithContent[assistantsWithContent.length - 1];
      if (lastAssistant?.id) {
        turns.push({
          items: [...items],
          messages: [...items],
          lastAssistantId: lastAssistant.id,
          assistantMessageIds: items
            .filter((message) => message?.role === 'assistant' && message.id)
            .map((message) => message.id)
        });
      }
    }
    items = [];
  };

  messages.forEach((message) => {
    if (isNormalUserBoundary(message)) {
      flush();
      return;
    }
    if (message?.role === 'assistant' || message?.role === 'tool-group' || message?.role === 'tool') {
      items.push(message);
    }
  });
  flush();

  return turns;
};

const normalizeClipboardValue = (value) => {
  if (value == null) return '';
  const text = typeof value === 'string' ? value : JSON.stringify(value);
  return String(text || '').replace(/\s+/g, ' ').trim();
};

const truncateClipboardLine = (value, max = 220) => {
  const text = String(value || '');
  if (text.length <= max) return text;
  return text.slice(0, Math.max(0, max - 1)).trimEnd() + '…';
};

const formatToolClipboardLines = (tool) => {
  const name = String(tool?.name || 'tool').trim() || 'tool';
  const status = String(tool?.status || 'pending').trim() || 'pending';
  const lines = [`- ${name} [${status}]`];
  let entries = formatToolArgs(tool || {});

  if (entries == null && tool?.arguments) {
    entries = [['arguments', tool.arguments]];
  }

  if (entries && entries.length > 0) {
    const details = entries.slice(0, 2)
      .map(([key, value]) => {
        const normalized = normalizeClipboardValue(value);
        return normalized ? `${key}: ${normalized}` : `${key}:`;
      })
      .filter(Boolean)
      .join(' · ');
    if (details) {
      lines.push(`  ${truncateClipboardLine(details)}`);
    }
  }

  return lines.slice(0, 2);
};

const appendClipboardBlock = (parts, text) => {
  const value = String(text || '').trim();
  if (!value) return;
  if (parts.length > 0 && parts[parts.length - 1] !== '') {
    parts.push('');
  }
  parts.push(value);
};

const buildTurnClipboardText = (turn) => {
  const items = Array.isArray(turn?.items) ? turn.items : Array.isArray(turn?.messages) ? turn.messages : [];
  const parts = [];
  let inToolsSection = false;

  items.forEach((message) => {
    if (message?.role === 'assistant') {
      appendClipboardBlock(parts, message.content || '');
      inToolsSection = false;
      return;
    }

    if (message?.role === 'tool-group') {
      const tools = Array.isArray(message.tools) ? message.tools : [];
      if (tools.length === 0) return;
      if (!inToolsSection) {
        if (parts.length > 0 && parts[parts.length - 1] !== '') parts.push('');
        parts.push('Tools:');
        inToolsSection = true;
      }
      tools.forEach((tool) => {
        formatToolClipboardLines(tool).forEach((line) => parts.push(line));
      });
      return;
    }

    if (message?.role === 'tool') {
      if (!inToolsSection) {
        if (parts.length > 0 && parts[parts.length - 1] !== '') parts.push('');
        parts.push('Tools:');
        inToolsSection = true;
      }
      formatToolClipboardLines(message).forEach((line) => parts.push(line));
    }
  });

  return parts.join('\n').replace(/\n{3,}/g, '\n\n').trim();
};

const getClipboardWriter = () => {
  const clipboard = typeof navigator === 'undefined' ? null : navigator.clipboard;
  return clipboard && typeof clipboard.writeText === 'function' ? clipboard : null;
};

const createTurnActionPanel = (turn) => {
  const panel = document.createElement('div');
  panel.className = 'turn-action-panel';
  panel.dataset.turnAssistantId = turn.lastAssistantId || '';

  const button = document.createElement('button');
  button.type = 'button';
  button.className = 'turn-action-btn turn-copy-btn';
  button.title = 'Copy turn';
  button.setAttribute('aria-label', 'Copy turn');
  button.dataset.turnAssistantId = turn.lastAssistantId || '';
  button.innerHTML = TURN_COPY_ICON;

  if (!getClipboardWriter()) {
    button.disabled = true;
    button.title = 'Clipboard unavailable';
  }

  button.addEventListener('click', async (event) => {
    event.preventDefault();
    const clipboard = getClipboardWriter();
    if (!clipboard) return;

    const assistantId = button.dataset.turnAssistantId || '';
    const currentTurn = getAssistantTurns(ensureActiveSession())
      .find((candidate) => candidate.lastAssistantId === assistantId);
    const text = buildTurnClipboardText(currentTurn);
    if (!text) return;

    button.disabled = true;
    try {
      await clipboard.writeText(text);
      window.clearTimeout(button._turnCopyResetTimer);
      button.classList.add('copied');
      button.innerHTML = TURN_COPIED_ICON;
      button.title = 'Copied';
      button.setAttribute('aria-label', 'Copied');
      button._turnCopyResetTimer = window.setTimeout(() => {
        button.classList.remove('copied');
        button.innerHTML = TURN_COPY_ICON;
        button.title = 'Copy turn';
        button.setAttribute('aria-label', 'Copy turn');
        button.disabled = !getClipboardWriter();
      }, TURN_COPY_RESET_MS);
    } catch (_err) {
      button.title = 'Copy failed';
      window.setTimeout(() => {
        button.title = 'Copy turn';
      }, TURN_COPY_RESET_MS);
    } finally {
      if (!button.classList.contains('copied')) {
        button.disabled = !getClipboardWriter();
      } else {
        button.disabled = false;
      }
    }
  });

  panel.appendChild(button);
  return panel;
};

const removeTurnActionPanel = (node) => {
  node?.querySelector('.turn-action-panel')?.remove();
};

const ensureTurnActionPanel = (node, turn) => {
  if (!node || !turn?.lastAssistantId) return;

  let panel = node.querySelector('.turn-action-panel');
  if (!panel) {
    panel = createTurnActionPanel(turn);
    const meta = node.querySelector('.message-meta');
    if (meta) {
      node.insertBefore(panel, meta);
    } else {
      node.appendChild(panel);
    }
    return;
  }

  panel.dataset.turnAssistantId = turn.lastAssistantId;
  const button = panel.querySelector('.turn-copy-btn');
  if (button) {
    button.dataset.turnAssistantId = turn.lastAssistantId;
  }
};

const syncTurnActionPanelForAssistant = (assistantId) => {
  const root = elements.messages;
  if (!root || !assistantId) return;

  const session = ensureActiveSession();
  const messages = Array.isArray(session?.messages) ? session.messages : [];
  let assistantIndex = -1;

  for (let i = messages.length - 1; i >= 0; i -= 1) {
    if (messages[i]?.id === assistantId) {
      assistantIndex = i;
      break;
    }
  }

  if (assistantIndex === -1) return;

  let start = assistantIndex;
  while (start > 0 && !isNormalUserBoundary(messages[start - 1])) {
    start -= 1;
  }

  let end = assistantIndex;
  while (end + 1 < messages.length && !isNormalUserBoundary(messages[end + 1])) {
    end += 1;
  }

  const assistantMessageIds = [];
  let lastAssistantWithContent = null;

  for (let i = start; i <= end; i += 1) {
    const message = messages[i];
    if (message?.role !== 'assistant' || !message.id) continue;
    assistantMessageIds.push(message.id);
    if (String(message.content || '').trim()) {
      lastAssistantWithContent = message;
    }
  }

  assistantMessageIds.forEach((id) => {
    const node = findMessageElement(id);
    if (!node || !node.classList?.contains('assistant')) return;
    if (!lastAssistantWithContent || id !== lastAssistantWithContent.id) {
      removeTurnActionPanel(node);
      return;
    }
    ensureTurnActionPanel(node, { lastAssistantId: lastAssistantWithContent.id });
  });
};

const syncTurnActionPanels = () => {
  const root = elements.messages;
  if (!root) return;

  root.querySelectorAll('.turn-action-panel').forEach((panel) => panel.remove());

  getAssistantTurns(ensureActiveSession()).forEach((turn) => {
    if (!turn.lastAssistantId) return;
    const node = findMessageElement(turn.lastAssistantId);
    if (!node || !node.classList?.contains('assistant')) return;
    ensureTurnActionPanel(node, turn);
  });
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

  syncTurnActionPanels();
  refreshRelativeTimes();
  scrollToBottom(forceScroll);
  updateHeader();
};

const updateSidebarStatus = (statusSessions) => {
  if (!Array.isArray(statusSessions)) return false;
  let changed = false;
  let orderChanged = false;
  for (const entry of statusSessions) {
    const local = state.sessions.find((session) => session.id === entry.id) || null;
    const busyTarget = local || entry.id;
    const wasActive = sessionHasInProgressState(busyTarget);
    setSessionServerActiveRun(busyTarget, Boolean(entry.active_run));
    const nextActive = sessionHasInProgressState(busyTarget);

    if (local) {
      const nextLastMessageAt = Number(entry.last_message_at);
      if (Number.isFinite(nextLastMessageAt) && nextLastMessageAt > 0) {
        const prev = Number(local.lastMessageAt) || 0;
        if (nextLastMessageAt > prev) {
          local.lastMessageAt = nextLastMessageAt;
          orderChanged = true;
        }
      }
    }

    const row = elements.sessionGroups.querySelector(`.session-row[data-session-id="${CSS.escape(entry.id)}"]`);
    if (row) {
      row.classList.toggle('is-active', nextActive);
    }
    if (wasActive !== nextActive) changed = true;

    if (!row) continue;

    const titleEl = row.querySelector('.session-title');
    if (titleEl && entry.short_title && titleEl.textContent !== entry.short_title) {
      titleEl.textContent = entry.short_title;
      if (entry.long_title) titleEl.title = entry.long_title;
      changed = true;
    }

    const metaEl = row.querySelector('.session-meta');
    if (metaEl && entry.message_count != null) {
      const count = entry.message_count;
      if (local) {
        if (entry.short_title) local.title = entry.short_title;
        if (entry.long_title) local.longTitle = entry.long_title;
        local.messageCount = count;
      }
      const parts = [`${count} message${count === 1 ? '' : 's'}`];
      if (local?.archived) parts.push('hidden');
      const activityAt = local?.lastMessageAt || local?.created || Date.now();
      parts.push(relativeTime(activityAt));
      metaEl.textContent = parts.join(' · ');
    }
  }
  if (orderChanged) {
    renderSidebar();
    return true;
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
  ensureScriptLoaded,
  ensureStylesheetLoaded,
  ensureKatexLoaded,
  ensureHighlightLoaded,
  renderAssistantMarkdown,
  enqueueAssistantStreamUpdate,
  finalizeAssistantStreamRender,
  createToolCard,
  createModelSwapNode,
  updateModelSwapNode,
  createMessageNode,
  updateAssistantNode,
  updateUserNode,
  updateToolNode,
  toolGroupSummaryText,
  createToolGroupNode,
  formatToolArgs,
  getAssistantTurns,
  formatToolClipboardLines,
  buildTurnClipboardText,
  createTurnActionPanel,
  syncTurnActionPanels,
  buildArgsNode,
  createToolEntryNode,
  updateToolGroupNode,
  resetAssistantStreamRenders,
  renderMessages
});
})();
