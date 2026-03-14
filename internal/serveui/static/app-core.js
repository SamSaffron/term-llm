(() => {
'use strict';

const app = window.TermLLMApp || (window.TermLLMApp = {});

// ===== Constants & state =====
// UI_PREFIX is the base path for all routes (UI + API). Injected by the server
// into index.html as window.TERM_LLM_UI_PREFIX, defaults to '/ui'.
const UI_PREFIX = (window.TERM_LLM_UI_PREFIX || '/ui');

const STORAGE_KEYS = {
  sessions: 'term_llm_sessions',
  token: 'term_llm_token',
  activeSession: 'term_llm_active_session',
  selectedModel: 'term_llm_selected_model',
  notificationsEnabled: 'term_llm_notifications_enabled',
  lastNotifiedResponseId: 'term_llm_last_notified_response_id'
};

const state = {
  token: localStorage.getItem(STORAGE_KEYS.token) || '',
  sessions: [],
  activeSessionId: localStorage.getItem(STORAGE_KEYS.activeSession) || '',
  models: [],
  selectedModel: localStorage.getItem(STORAGE_KEYS.selectedModel) || '',
  notificationsEnabled: localStorage.getItem(STORAGE_KEYS.notificationsEnabled) === '1',
  lastNotifiedResponseId: localStorage.getItem(STORAGE_KEYS.lastNotifiedResponseId) || '',
  streaming: false,
  currentStreamResponseId: '',
  currentStreamSessionId: '',
  queuedInterrupts: [],
  pendingInterruptCommits: [],
  expectCanceledRun: false,
  abortController: null,
  autoScroll: true,
  authRequired: false,
  connected: false,
  attachments: [],
  askUser: null,
  approval: null,
  serviceWorkerRegistration: null,
  voice: {
    supported: typeof window !== 'undefined' && typeof navigator !== 'undefined' && !!(navigator.mediaDevices && navigator.mediaDevices.getUserMedia) && typeof window.MediaRecorder !== 'undefined',
    recording: false,
    transcribing: false,
    recorder: null,
    stream: null,
    chunks: [],
    timerId: null,
    startedAt: 0,
    cancelOnStop: false,
    mimeType: '',
    status: ''
  },
  restorePromptFocus: false
};
// Ensure cookie is set on load so <img> requests to basePath/images/ can authenticate
if (state.token) {
  document.cookie = `term_llm_token=${encodeURIComponent(state.token)}; path=${UI_PREFIX}/images; SameSite=Strict; max-age=31536000`;
}

const elements = {
  sidebar: document.getElementById('sidebar'),
  sidebarBackdrop: document.getElementById('sidebarBackdrop'),
  sidebarCloseBtn: document.getElementById('sidebarCloseBtn'),
  mobileMenuBtn: document.getElementById('mobileMenuBtn'),
  settingsBtn: document.getElementById('settingsBtn'),
  newChatBtn: document.getElementById('newChatBtn'),
  sessionGroups: document.getElementById('sessionGroups'),
  activeSessionTitle: document.getElementById('activeSessionTitle'),
  connectionState: document.getElementById('connectionState'),
  chatScroll: document.getElementById('chatScroll'),
  messages: document.getElementById('messages'),
  promptInput: document.getElementById('promptInput'),
  sendBtn: document.getElementById('sendBtn'),
  stopBtn: document.getElementById('stopBtn'),
  authModal: document.getElementById('authModal'),
  authTokenInput: document.getElementById('authTokenInput'),
  authError: document.getElementById('authError'),
  authConnectBtn: document.getElementById('authConnectBtn'),
  authCancelBtn: document.getElementById('authCancelBtn'),
  notificationStatus: document.getElementById('notificationStatus'),
  notificationBtn: document.getElementById('notificationBtn'),
  installHint: document.getElementById('installHint'),
  askUserModal: document.getElementById('askUserModal'),
  askUserModalTitle: document.getElementById('askUserModalTitle'),
  askUserModalSubtitle: document.getElementById('askUserModalSubtitle'),
  askUserModalBody: document.getElementById('askUserModalBody'),
  askUserError: document.getElementById('askUserError'),
  askUserCancelBtn: document.getElementById('askUserCancelBtn'),
  askUserSubmitBtn: document.getElementById('askUserSubmitBtn'),
  attachBtn: document.getElementById('attachBtn'),
  fileInput: document.getElementById('fileInput'),
  attachmentsStrip: document.getElementById('attachmentsStrip'),
  voiceStatus: document.getElementById('voiceStatus'),
  voiceBtn: document.getElementById('voiceBtn'),
  dropOverlay: document.getElementById('dropOverlay'),
  headerStats: document.getElementById('headerStats'),
  modelSelect: document.getElementById('modelSelect'),
  approvalModal: document.getElementById('approvalModal'),
  approvalTitle: document.getElementById('approvalTitle'),
  approvalPath: document.getElementById('approvalPath'),
  approvalBody: document.getElementById('approvalBody'),
  approvalError: document.getElementById('approvalError'),
  approvalDenyBtn: document.getElementById('approvalDenyBtn'),
  approvalApproveBtn: document.getElementById('approvalApproveBtn'),
  lightbox: document.getElementById('lightbox'),
  lightboxImg: document.getElementById('lightboxImg'),
  startupSplash: document.getElementById('startupSplash'),
  startupStatus: document.getElementById('startupStatus')
};

// ===== Markdown setup =====
marked.use({
  breaks: true,
  gfm: true
});

const renderMath = (target) => {
  if (!target || typeof renderMathInElement !== 'function') return;
  renderMathInElement(target, {
    delimiters: [
      { left: '$$', right: '$$', display: true },
      { left: '\\[', right: '\\]', display: true },
      { left: '$', right: '$', display: false },
      { left: '\\(', right: '\\)', display: false }
    ],
    ignoredTags: ['script', 'noscript', 'style', 'textarea', 'pre', 'code', 'option'],
    throwOnError: false
  });
};

// ===== Helpers =====
// crypto.randomUUID() requires a secure context (HTTPS); use getRandomValues fallback for HTTP
const generateUUID = () => {
  if (typeof crypto !== 'undefined' && crypto.randomUUID) {
    return crypto.randomUUID();
  }
  // Works on HTTP — getRandomValues is not restricted to secure contexts
  if (typeof crypto !== 'undefined' && crypto.getRandomValues) {
    return ([1e7]+-1e3+-4e3+-8e3+-1e11).replace(/[018]/g, c =>
      (c ^ crypto.getRandomValues(new Uint8Array(1))[0] & 15 >> c / 4).toString(16)
    );
  }
  // Last resort
  return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, c => {
    const r = Math.random() * 16 | 0;
    return (c === 'x' ? r : (r & 0x3 | 0x8)).toString(16);
  });
};
const generateId = (prefix) => `${prefix}_${generateUUID()}`;

const INTERRUPT_BADGE_META = {
  evaluating: { className: 'pending', label: 'evaluating…' },
  interject: { className: 'interject', icon: '✓', label: 'injected' },
  cancel: { className: 'cancel', icon: '⏹', label: 'cancelled + queued' },
  queue: { className: 'queue', icon: '⏳', label: 'queued' },
  error: { className: 'error', icon: '⚠', label: 'failed' }
};

const sanitizeInterruptState = (value) => {
  const v = String(value || '').toLowerCase();
  return Object.prototype.hasOwnProperty.call(INTERRUPT_BADGE_META, v) ? v : '';
};

const syncTokenCookie = (token) => {
  if (token) {
    document.cookie = `term_llm_token=${encodeURIComponent(token)}; path=${UI_PREFIX}/images; SameSite=Strict; max-age=31536000`;
  } else {
    document.cookie = `term_llm_token=; path=${UI_PREFIX}/images; SameSite=Strict; max-age=0`;
  }
};

const truncate = (text, max = 60) => {
  const value = (text || '').trim().replace(/\s+/g, ' ');
  if (!value) return 'New chat';
  return value.length > max ? `${value.slice(0, max - 1)}…` : value;
};

const asTimestamp = (value) => {
  const n = Number(value);
  return Number.isFinite(n) && n > 0 ? n : Date.now();
};

const fullDate = (ms) => new Date(ms).toLocaleString();

const relativeTime = (ms) => {
  const diff = Date.now() - ms;
  if (diff < 45_000) return 'just now';
  if (diff < 3_600_000) return `${Math.max(1, Math.floor(diff / 60_000))}m ago`;
  if (diff < 86_400_000) return `${Math.max(1, Math.floor(diff / 3_600_000))}h ago`;
  if (diff < 604_800_000) return `${Math.max(1, Math.floor(diff / 86_400_000))}d ago`;
  return new Date(ms).toLocaleDateString();
};

const sessionBucket = (ms) => {
  const now = new Date();
  const startToday = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  const startYesterday = startToday - 86_400_000;
  const startWeek = startToday - (6 * 86_400_000);

  if (ms >= startToday) return 'Today';
  if (ms >= startYesterday) return 'Yesterday';
  if (ms >= startWeek) return 'This week';
  return 'Older';
};

const toolIcon = (name) => {
  const n = String(name || '').toLowerCase();
  if (n === 'shell' || n === 'bash') return '💻';
  if (n === 'read_file') return '📄';
  if (n === 'write_file' || n === 'edit_file') return '✏️';
  if (n === 'web_search') return '🔍';
  if (n === 'read_url') return '🌐';
  if (n === 'image_generate') return '🎨';
  if (n === 'spawn_agent') return '🤖';
  return '🔧';
};

const formatUsage = (usage) => {
  const inTokens = Number(usage?.input_tokens || 0);
  const outTokens = Number(usage?.output_tokens || 0);
  const cached = Number(usage?.input_tokens_details?.cached_tokens || 0);
  return `↙ ${inTokens.toLocaleString()} in · ${outTokens.toLocaleString()} out · ${cached.toLocaleString()} cached`;
};

const fmtTokens = (n) => {
  if (n < 1000) return String(n);
  if (n < 1000000) {
    const k = n / 1000;
    return k < 10 ? `${k.toFixed(1)}k` : `${Math.round(k)}k`;
  }
  const m = n / 1000000;
  return m < 10 ? `${m.toFixed(1)}M` : `${Math.round(m)}M`;
};

const escapeHTML = (str) => {
  const div = document.createElement('div');
  div.textContent = str;
  return div.innerHTML;
};

const updateSessionUsageDisplay = (session) => {
  const el = elements?.headerStats;
  if (!el) return;
  const model = session?.activeModel || '';
  const lu = session?.lastUsage;

  if (!lu && !model) {
    el.innerHTML = '';
    return;
  }

  const parts = [];
  if (model) {
    parts.push(`<span class="stats-model">${escapeHTML(model)}</span>`);
  }

  if (lu) {
    const inTok = Number(lu.input_tokens || 0);
    const outTok = Number(lu.output_tokens || 0);
    const cached = Number(lu.input_tokens_details?.cached_tokens || 0);
    const context = inTok + outTok;
    let s = `${fmtTokens(inTok)} in`;
    if (cached > 0) s += ` <span class="stats-cached">(${fmtTokens(cached)} cached)</span>`;
    s += ` → ${fmtTokens(outTok)} out`;
    parts.push(`<span class="stats-tokens">${s}</span>`);
    parts.push(`<span class="stats-context">context ${fmtTokens(context)}</span>`);
  }

  el.innerHTML = parts.join('<span class="stats-sep">·</span>');
};

const isNearBottom = () => {
  const el = elements.chatScroll;
  return (el.scrollHeight - (el.scrollTop + el.clientHeight)) < 96;
};

const scrollToBottom = (force = false) => {
  if (force || state.autoScroll) {
    elements.chatScroll.scrollTop = elements.chatScroll.scrollHeight;
  }
};

const setConnectionState = (text, mode = '') => {
  elements.connectionState.textContent = text;
  elements.connectionState.classList.remove('ok', 'bad');
  if (mode) {
    elements.connectionState.classList.add(mode);
  }
};

const setStartupStatus = (text) => {
  if (!elements.startupStatus || !text) return;
  elements.startupStatus.textContent = text;
};

const hideStartupSplash = () => {
  if (!elements.startupSplash || elements.startupSplash.classList.contains('hidden')) return;
  document.body.classList.remove('app-loading');
  elements.startupSplash.classList.add('hidden');
  window.setTimeout(() => {
    if (elements.startupSplash) {
      elements.startupSplash.setAttribute('hidden', 'hidden');
    }
  }, 220);
};

const updateDocumentTitle = () => {
  const session = getActiveSession();
  if (session && session.title && session.title !== 'New chat') {
    document.title = `Chat · ${session.title}`;
  } else {
    document.title = 'Chat';
  }
};

const isStandalone = () => window.matchMedia('(display-mode: standalone)').matches || window.navigator.standalone === true;

// Mobile browsers treat programmatic focus as an instruction to pop the keyboard.
const shouldSuppressPromptAutoFocus = () => window.matchMedia('(hover: none) and (pointer: coarse)').matches;

const syncNotificationPermissionState = () => {
  if (typeof Notification === 'undefined') return;
  if (Notification.permission === 'granted') {
    state.notificationsEnabled = true;
    localStorage.setItem(STORAGE_KEYS.notificationsEnabled, '1');
  } else if (Notification.permission === 'denied') {
    state.notificationsEnabled = false;
    localStorage.setItem(STORAGE_KEYS.notificationsEnabled, '0');
  }
};

const shouldAutoSubscribeToPush = () => (
  typeof Notification !== 'undefined' &&
  Notification.permission === 'granted' &&
  !!state.token
);

const refreshNotificationUI = () => {
  if (!elements.notificationStatus || !elements.notificationBtn) return;
  const supported = typeof Notification !== 'undefined';
  syncNotificationPermissionState();
  if (!supported) {
    elements.notificationStatus.textContent = 'This browser does not support notifications.';
    elements.notificationBtn.disabled = true;
    elements.notificationBtn.textContent = 'Unavailable';
  } else if (!state.notificationsEnabled || Notification.permission === 'default') {
    elements.notificationStatus.textContent = isStandalone()
      ? 'Off. Enable to get a ping when a reply finishes while you are away.'
      : 'Off. Install to Home Screen first on iPhone, then enable notifications.';
    elements.notificationBtn.disabled = false;
    elements.notificationBtn.textContent = 'Enable';
  } else if (Notification.permission === 'granted') {
    elements.notificationStatus.textContent = 'On. Replies can notify you when the app is in the background.';
    elements.notificationBtn.disabled = false;
    elements.notificationBtn.textContent = 'Enabled';
  } else {
    elements.notificationStatus.textContent = 'Blocked in browser settings.';
    elements.notificationBtn.disabled = true;
    elements.notificationBtn.textContent = 'Blocked';
  }

  if (elements.installHint) {
    elements.installHint.hidden = isStandalone();
  }
};

const registerServiceWorker = async () => {
  if (!('serviceWorker' in navigator)) return null;
  try {
    const registration = await navigator.serviceWorker.register(`${UI_PREFIX}/sw.js`, { scope: `${UI_PREFIX}/` });
    state.serviceWorkerRegistration = registration;
    return registration;
  } catch {
    return null;
  }
};

const subscribeToPush = async () => {
  const vapidKey = window.TERM_LLM_VAPID_PUBLIC_KEY;
  if (!vapidKey) return;

  // Wait for an active service worker — on first install there may not be one yet.
  if (!('serviceWorker' in navigator)) return;
  const registration = await navigator.serviceWorker.ready;
  if (!registration || !registration.pushManager) return;
  state.serviceWorkerRegistration = registration;

  try {
    // Check for existing subscription first
    let subscription = await registration.pushManager.getSubscription();
    if (!subscription) {
      // Convert base64 VAPID key to Uint8Array
      const padding = '='.repeat((4 - vapidKey.length % 4) % 4);
      const base64 = (vapidKey + padding).replace(/-/g, '+').replace(/_/g, '/');
      const rawData = atob(base64);
      const applicationServerKey = new Uint8Array(rawData.length);
      for (let i = 0; i < rawData.length; i++) {
        applicationServerKey[i] = rawData.charCodeAt(i);
      }

      subscription = await registration.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey
      });
    }

    // Send subscription to server using toJSON() which provides keys in
    // base64url format — the encoding webpush-go expects.
    const subJSON = subscription.toJSON();
    const body = {
      endpoint: subJSON.endpoint,
      keys: subJSON.keys
    };

    const resp = await fetch(`${UI_PREFIX}/v1/push/subscribe`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'Authorization': state.token ? `Bearer ${state.token}` : ''
      },
      body: JSON.stringify(body)
    });
    if (!resp.ok) {
      console.warn('Push subscribe failed:', resp.status, await resp.text().catch(() => ''));
    }
  } catch {
    // Push subscription failed — in-app notifications still work
  }
};

const requestNotificationPermission = async () => {
  if (typeof Notification === 'undefined') {
    refreshNotificationUI();
    return 'unsupported';
  }
  if (Notification.permission === 'granted') {
    syncNotificationPermissionState();
    subscribeToPush();
    refreshNotificationUI();
    return 'granted';
  }
  const permission = await Notification.requestPermission();
  syncNotificationPermissionState();
  if (permission === 'granted') {
    subscribeToPush();
  }
  refreshNotificationUI();
  return permission;
};

const maybeNotifyResponseComplete = async (session, assistantMessage, responseId) => {
  const normalizedResponseId = String(responseId || '').trim();
  if (!state.notificationsEnabled || !normalizedResponseId) return;
  if (state.lastNotifiedResponseId === normalizedResponseId) return;
  if (typeof Notification === 'undefined' || Notification.permission !== 'granted') return;
  if (document.visibilityState === 'visible' && document.hasFocus()) return;

  const body = String(assistantMessage?.content || 'Reply finished.')
    .replace(/\s+/g, ' ')
    .trim()
    .slice(0, 180) || 'Reply finished.';
  const title = session?.title && session.title !== 'New chat'
    ? `Reply finished · ${session.title}`
    : 'Reply finished';
  const targetURL = `${UI_PREFIX}/${encodeURIComponent(session?.id || '')}`;
  const options = {
    body,
    tag: `response-${normalizedResponseId}`,
    renotify: false,
    icon: `${UI_PREFIX}/icon-512.png`,
    badge: `${UI_PREFIX}/icon-512.png`,
    data: { url: targetURL }
  };

  try {
    const registration = state.serviceWorkerRegistration || await registerServiceWorker();
    if (registration && typeof registration.showNotification === 'function') {
      await registration.showNotification(title, options);
    } else {
      const notification = new Notification(title, options);
      notification.onclick = () => {
        window.focus();
        window.location.href = targetURL;
        notification.close();
      };
    }
    state.lastNotifiedResponseId = normalizedResponseId;
    localStorage.setItem(STORAGE_KEYS.lastNotifiedResponseId, normalizedResponseId);
  } catch {
    // Ignore notification failures — chat completion still succeeded.
  }
};

const sessionIdFromURL = () => {
  const path = window.location.pathname;
  const escaped = UI_PREFIX.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const match = path.match(new RegExp('^' + escaped + '/(.+)$'));
  return match ? decodeURIComponent(match[1]) : '';
};

const updateURL = (sessionId) => {
  if (!sessionId) return;
  const target = UI_PREFIX + '/' + encodeURIComponent(sessionId);
  if (window.location.pathname !== target) {
    history.pushState(null, '', target);
  }
  updateDocumentTitle();
};

const sanitizeMessage = (msg) => {
  if (!msg || typeof msg !== 'object' || typeof msg.role !== 'string') return null;
  const role = msg.role;
  const base = {
    id: typeof msg.id === 'string' ? msg.id : generateId('msg'),
    role,
    created: asTimestamp(msg.created)
  };

  if (role === 'user' || role === 'assistant' || role === 'error') {
    base.content = String(msg.content || '');
    if (role === 'assistant' && msg.usage && typeof msg.usage === 'object') {
      base.usage = msg.usage;
    }
    if (role === 'user') {
      const interruptState = sanitizeInterruptState(msg.interruptState);
      if (interruptState) {
        base.interruptState = interruptState;
      }
      if (msg.askUser) {
        base.askUser = true;
      }
      if (Array.isArray(msg.attachments) && msg.attachments.length > 0) {
        base.attachments = msg.attachments.map(a => ({
          name: String(a.name || 'file'),
          type: String(a.type || '')
        }));
      }
    }
    return base;
  }

  if (role === 'tool') {
    base.name = String(msg.name || 'tool');
    base.arguments = String(msg.arguments || '');
    base.status = msg.status === 'done' ? 'done' : 'running';
    base.expanded = Boolean(msg.expanded);
    return base;
  }

  if (role === 'tool-group') {
    base.tools = Array.isArray(msg.tools) ? msg.tools.map(t => ({
      id: String(t.id || ''),
      name: String(t.name || 'tool'),
      arguments: String(t.arguments || ''),
      status: t.status === 'done' ? 'done' : 'running',
      created: asTimestamp(t.created)
    })) : [];
    base.expanded = Boolean(msg.expanded);
    base.status = msg.status === 'done' ? 'done' : 'running';
    return base;
  }

  return null;
};

const sanitizeSession = (session) => {
  if (!session || typeof session !== 'object') return null;
  const messages = Array.isArray(session.messages)
    ? session.messages.map(sanitizeMessage).filter(Boolean)
    : [];

  const result = {
    id: typeof session.id === 'string' ? session.id : `sess_${generateUUID()}`,
    title: typeof session.title === 'string' && session.title.trim() ? session.title.trim() : 'New chat',
    created: asTimestamp(session.created),
    messages,
    lastResponseId: typeof session.lastResponseId === 'string' ? session.lastResponseId : null,
    activeResponseId: typeof session.activeResponseId === 'string' ? session.activeResponseId : null,
    lastSequenceNumber: Number.isFinite(Number(session.lastSequenceNumber)) ? Number(session.lastSequenceNumber) : 0,
    sessionUsage: session.sessionUsage && typeof session.sessionUsage === 'object' ? session.sessionUsage : null,
    lastUsage: session.lastUsage && typeof session.lastUsage === 'object' ? session.lastUsage : null,
    activeModel: typeof session.activeModel === 'string' ? session.activeModel : ''
  };
  if (session._serverOnly) result._serverOnly = true;
  if (typeof session.messageCount === 'number') result.messageCount = session.messageCount;
  return result;
};

const loadSessions = () => {
  try {
    const raw = localStorage.getItem(STORAGE_KEYS.sessions);
    if (!raw) return [];
    const parsed = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed.map(sanitizeSession).filter(Boolean);
  } catch {
    return [];
  }
};

// Strip large binary payloads from attachment metadata before serialization.
const sessionsForStorage = () => {
  return state.sessions.map(s => {
    if (!s.messages || !s.messages.some(m => m.attachments)) return s;
    return {
      ...s,
      messages: s.messages.map(m => {
        if (!m.attachments) return m;
        return {
          ...m,
          attachments: m.attachments.map(a => ({ name: a.name, type: a.type }))
        };
      })
    };
  });
};

const saveSessions = () => {
  if (state.sessions.length > 100) {
    state.sessions.sort((a, b) => a.created - b.created);
    state.sessions = state.sessions.slice(-100);
    if (!state.sessions.find((s) => s.id === state.activeSessionId)) {
      state.activeSessionId = state.sessions[state.sessions.length - 1]?.id || '';
    }
  }
  try {
    localStorage.setItem(STORAGE_KEYS.sessions, JSON.stringify(sessionsForStorage()));
    localStorage.setItem(STORAGE_KEYS.activeSession, state.activeSessionId || '');
  } catch {
    // QuotaExceededError or other storage failure — continue without persistence
  }
};

const getActiveSession = () => state.sessions.find((s) => s.id === state.activeSessionId) || null;

const createSession = () => ({
  id: `sess_${generateUUID()}`,
  title: 'New chat',
  created: Date.now(),
  messages: [],
  lastResponseId: null,
  activeResponseId: null,
  lastSequenceNumber: 0,
  sessionUsage: null,
  lastUsage: null,
  activeModel: ''
});

const ensureActiveSession = () => {
  let active = getActiveSession();
  if (active) {
    updateURL(active.id);
    return active;
  }

  if (state.sessions.length === 0) {
    active = createSession();
    state.sessions.unshift(active);
  } else {
    const sorted = [...state.sessions].sort((a, b) => b.created - a.created);
    active = sorted[0];
  }

  state.activeSessionId = active.id;
  updateURL(active.id);
  saveSessions();
  return active;
};

const findMessageElement = (id) => elements.messages.querySelector(`[data-message-id="${id}"]`);

const refreshRelativeTimes = () => {
  document.querySelectorAll('[data-created]').forEach((node) => {
    const ts = Number(node.getAttribute('data-created'));
    if (!Number.isFinite(ts)) return;
    node.textContent = relativeTime(ts);
    node.title = fullDate(ts);
  });
};

const persistAndRefreshShell = () => {
  saveSessions();
  app.renderSidebar();
  app.updateHeader();
};

const openLightbox = (src) => {
  elements.lightboxImg.src = src;
  elements.lightbox.classList.remove('hidden');
};

const closeLightbox = () => {
  elements.lightbox.classList.add('hidden');
  elements.lightboxImg.src = '';
};

elements.lightbox.addEventListener('click', closeLightbox);
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && !elements.lightbox.classList.contains('hidden')) {
    closeLightbox();
  }
});

Object.assign(app, {
  STORAGE_KEYS,
  state,
  elements,
  generateUUID,
  generateId,
  INTERRUPT_BADGE_META,
  sanitizeInterruptState,
  syncTokenCookie,
  truncate,
  asTimestamp,
  fullDate,
  relativeTime,
  sessionBucket,
  toolIcon,
  formatUsage,
  renderMath,
  updateSessionUsageDisplay,
  isNearBottom,
  scrollToBottom,
  setConnectionState,
  setStartupStatus,
  hideStartupSplash,
  updateDocumentTitle,
  UI_PREFIX,
  isStandalone,
  shouldSuppressPromptAutoFocus,
  refreshNotificationUI,
  registerServiceWorker,
  subscribeToPush,
  shouldAutoSubscribeToPush,
  requestNotificationPermission,
  maybeNotifyResponseComplete,
  sessionIdFromURL,
  updateURL,
  sanitizeMessage,
  sanitizeSession,
  loadSessions,
  sessionsForStorage,
  saveSessions,
  getActiveSession,
  createSession,
  ensureActiveSession,
  findMessageElement,
  refreshRelativeTimes,
  persistAndRefreshShell,
  openLightbox,
  closeLightbox
});
})();
