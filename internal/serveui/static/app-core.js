(() => {
'use strict';

const app = window.TermLLMApp || (window.TermLLMApp = {});
app.markdownStreaming = window.TermLLMMarkdownStreaming || null;

// ===== Constants & state =====
// UI_PREFIX is the base path for all routes (UI + API). Injected by the server
// into index.html as window.TERM_LLM_UI_PREFIX, defaults to '/ui'.
const UI_PREFIX = (window.TERM_LLM_UI_PREFIX || '/ui');
const LEGACY_DRAFT_SESSION_ID = '__draft__';

const parseSidebarSessionCategories = (raw) => {
  const input = Array.isArray(raw)
    ? raw
    : String(raw || 'all').split(',');
  const seen = new Set();
  const categories = [];
  input.forEach((item) => {
    const value = String(item || '').trim().toLowerCase();
    if (!value || seen.has(value)) return;
    seen.add(value);
    categories.push(value);
  });
  return categories.includes('all') || categories.length === 0 ? ['all'] : categories;
};

const STORAGE_KEYS = {
  token: 'term_llm_token',
  activeSession: 'term_llm_active_session',
  draftSessionActive: 'term_llm_draft_session_active',
  selectedModel: 'term_llm_selected_model',
  selectedProvider: 'term_llm_selected_provider',
  selectedEffort: 'term_llm_selected_effort',
  sidebarCollapsed: 'term_llm_sidebar_collapsed',
  showHiddenSessions: 'term_llm_show_hidden_sessions',
  notificationsEnabled: 'term_llm_notifications_enabled',
  lastNotifiedResponseId: 'term_llm_last_notified_response_id'
};

const initialStoredActiveSessionId = localStorage.getItem(STORAGE_KEYS.activeSession) || '';
const initialDraftSessionActive = initialStoredActiveSessionId === LEGACY_DRAFT_SESSION_ID
  || localStorage.getItem(STORAGE_KEYS.draftSessionActive) === '1';

const state = {
  token: localStorage.getItem(STORAGE_KEYS.token) || '',
  sessions: [],
  sessionProgressById: {},
  activeSessionId: initialStoredActiveSessionId === LEGACY_DRAFT_SESSION_ID ? '' : initialStoredActiveSessionId,
  draftSessionActive: initialDraftSessionActive,
  providers: [],
  selectedProvider: localStorage.getItem(STORAGE_KEYS.selectedProvider) || '',
  models: [],
  selectedModel: localStorage.getItem(STORAGE_KEYS.selectedModel) || '',
  selectedEffort: localStorage.getItem(STORAGE_KEYS.selectedEffort) || '',
  sidebarCollapsed: localStorage.getItem(STORAGE_KEYS.sidebarCollapsed) === '1',
  sidebarSessionCategories: parseSidebarSessionCategories(window.TERM_LLM_SIDEBAR_SESSIONS),
  showHiddenSessions: localStorage.getItem(STORAGE_KEYS.showHiddenSessions) === '1',
  notificationsEnabled: localStorage.getItem(STORAGE_KEYS.notificationsEnabled) === '1',
  lastNotifiedResponseId: localStorage.getItem(STORAGE_KEYS.lastNotifiedResponseId) || '',
  streaming: false,
  streamGeneration: 0,
  currentStreamResponseId: '',
  currentStreamSessionId: '',
  renameSessionId: '',
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
  restorePromptFocus: false,
  lastEventTime: 0
};
// Ensure cookie is set on load so <img> requests to basePath/images/ can authenticate
if (state.token) {
  document.cookie = `term_llm_token=${encodeURIComponent(state.token)}; path=${UI_PREFIX}/images; SameSite=Strict; max-age=31536000`;
}

const elements = {
  appShell: document.getElementById('appShell'),
  sidebar: document.getElementById('sidebar'),
  sidebarBackdrop: document.getElementById('sidebarBackdrop'),
  sidebarToggleBtn: document.getElementById('sidebarToggleBtn'),
  sidebarPanelToggleBtn: document.getElementById('sidebarPanelToggleBtn'),
  sidebarCloseBtn: document.getElementById('sidebarCloseBtn'),
  sidebarRailNewChatBtn: document.getElementById('sidebarRailNewChatBtn'),
  sidebarRailSettingsBtn: document.getElementById('sidebarRailSettingsBtn'),
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
  renameSessionModal: document.getElementById('renameSessionModal'),
  renameSessionInput: document.getElementById('renameSessionInput'),
  renameSessionError: document.getElementById('renameSessionError'),
  renameSessionCancelBtn: document.getElementById('renameSessionCancelBtn'),
  renameSessionSaveBtn: document.getElementById('renameSessionSaveBtn'),
  notificationStatus: document.getElementById('notificationStatus'),
  notificationBtn: document.getElementById('notificationBtn'),
  showHiddenSessionsInput: document.getElementById('showHiddenSessionsInput'),
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
  providerSelect: document.getElementById('providerSelect'),
  modelSelect: document.getElementById('modelSelect'),
  effortSelect: document.getElementById('effortSelect'),
  approvalModal: document.getElementById('approvalModal'),
  approvalTitle: document.getElementById('approvalTitle'),
  approvalPath: document.getElementById('approvalPath'),
  approvalBody: document.getElementById('approvalBody'),
  approvalError: document.getElementById('approvalError'),
  approvalDenyBtn: document.getElementById('approvalDenyBtn'),
  approvalApproveBtn: document.getElementById('approvalApproveBtn'),
  lightbox: document.getElementById('lightbox'),
  lightboxImg: document.getElementById('lightboxImg'),
  lightboxVideo: document.getElementById('lightboxVideo'),
  lightboxBackdrop: document.getElementById('lightboxBackdrop'),
  lightboxCopy: document.getElementById('lightboxCopy'),
  lightboxExpand: document.getElementById('lightboxExpand'),
  lightboxClose: document.getElementById('lightboxClose'),
  startupSplash: document.getElementById('startupSplash'),
  startupStatus: document.getElementById('startupStatus')
};

// marked is configured via markdown-setup.js (loaded before this file).

// Be strict about inline math delimiters. Single-dollar math collides with
// ordinary currency amounts in LLM output, so require \(...\) for inline math.
const MATH_DELIMITERS = [
  { left: '$$', right: '$$', display: true },
  { left: '\\[', right: '\\]', display: true },
  { left: '\\(', right: '\\)', display: false }
];

const renderMath = (target) => {
  if (!target || typeof renderMathInElement !== 'function') return;
  renderMathInElement(target, {
    delimiters: MATH_DELIMITERS,
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

const sessionRefId = (sessionOrId) => {
  if (typeof sessionOrId === 'string') return sessionOrId.trim();
  if (sessionOrId && typeof sessionOrId === 'object') {
    return String(sessionOrId.id || '').trim();
  }
  return '';
};

const sessionProgressEntry = (sessionOrId) => {
  const id = sessionRefId(sessionOrId);
  if (!id) return null;
  return state.sessionProgressById[id] || null;
};

const ensureSessionProgressEntry = (sessionOrId) => {
  const id = sessionRefId(sessionOrId);
  if (!id) return null;
  if (!state.sessionProgressById[id]) {
    state.sessionProgressById[id] = {
      optimisticBusy: false,
      serverActiveRun: false
    };
  }
  return state.sessionProgressById[id];
};

const pruneSessionProgressEntry = (sessionOrId) => {
  const id = sessionRefId(sessionOrId);
  if (!id) return;
  const entry = state.sessionProgressById[id];
  if (!entry) return;
  if (!entry.optimisticBusy && !entry.serverActiveRun) {
    delete state.sessionProgressById[id];
  }
};

const setSessionOptimisticBusy = (sessionOrId, busy) => {
  const id = sessionRefId(sessionOrId);
  if (!id) return false;
  const entry = ensureSessionProgressEntry(id);
  entry.optimisticBusy = Boolean(busy);
  pruneSessionProgressEntry(id);
  return sessionHasInProgressState(sessionOrId);
};

const setSessionServerActiveRun = (sessionOrId, activeRun) => {
  const id = sessionRefId(sessionOrId);
  if (!id) return false;
  const entry = ensureSessionProgressEntry(id);
  entry.serverActiveRun = Boolean(activeRun);
  pruneSessionProgressEntry(id);
  return sessionHasInProgressState(sessionOrId);
};

const moveSessionProgressState = (previousSessionOrId, nextSessionOrId) => {
  const previousId = sessionRefId(previousSessionOrId);
  const nextId = sessionRefId(nextSessionOrId);
  if (!previousId || !nextId || previousId === nextId) return;

  const previous = state.sessionProgressById[previousId];
  if (!previous) return;

  const next = state.sessionProgressById[nextId];
  if (next) {
    next.optimisticBusy = Boolean(next.optimisticBusy || previous.optimisticBusy);
    next.serverActiveRun = Boolean(next.serverActiveRun || previous.serverActiveRun);
  } else {
    state.sessionProgressById[nextId] = { ...previous };
  }
  delete state.sessionProgressById[previousId];
  pruneSessionProgressEntry(nextId);
};

const sessionHasInProgressState = (sessionOrId) => {
  const id = sessionRefId(sessionOrId);
  if (!id) return false;

  const session = (sessionOrId && typeof sessionOrId === 'object')
    ? sessionOrId
    : state.sessions.find((item) => item.id === id) || null;
  const entry = sessionProgressEntry(id);
  const hasActiveResponse = Boolean(String(session?.activeResponseId || '').trim());
  const ownsLiveStream = state.currentStreamSessionId === id
    && Boolean(state.abortController || state.currentStreamResponseId || state.streaming);
  const isVisibleStreamingSession = state.activeSessionId === id && state.streaming;

  return Boolean(
    entry?.optimisticBusy
    || entry?.serverActiveRun
    || hasActiveResponse
    || ownsLiveStream
    || isVisibleStreamingSession
  );
};

const hasAnySessionInProgressState = () => {
  if (state.sessions.some((session) => sessionHasInProgressState(session))) {
    return true;
  }
  return Object.keys(state.sessionProgressById).some((id) => sessionHasInProgressState(id));
};

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

const splitHeaderModelEffort = (model, effort) => {
  const rawModel = String(model || '').trim();
  const rawEffort = String(effort || '').trim();
  if (!rawModel || !rawEffort) {
    return { model: rawModel, effort: rawEffort };
  }

  const normalizedEffort = rawEffort.toLowerCase();
  const suffix = new RegExp(`[-_ ]${normalizedEffort}$`, 'i');
  if (!suffix.test(rawModel)) {
    return { model: rawModel, effort: rawEffort };
  }

  return {
    model: rawModel.replace(suffix, ''),
    effort: rawEffort
  };
};

const updateSessionUsageDisplay = (session) => {
  const el = elements?.headerStats;
  if (!el) return;
  const lu = session?.lastUsage;
  const model = session?.activeModel || state.selectedModel || '';
  const provider = session?.provider || state.selectedProvider || '';
  const effort = state.selectedEffort || '';
  const headerModelEffort = splitHeaderModelEffort(model, effort);

  const parts = [];
  if (provider) {
    parts.push(`<span class="stats-provider">${escapeHTML(provider)}</span>`);
  }
  if (headerModelEffort.model) {
    parts.push(`<span class="stats-model">${escapeHTML(headerModelEffort.model)}</span>`);
  } else if (!provider) {
    parts.push(`<span class="stats-model stats-muted">Auto</span>`);
  }
  if (headerModelEffort.effort) {
    parts.push(`<span class="stats-effort">${escapeHTML(headerModelEffort.effort)}</span>`);
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

// Keep the shell pinned to the visual viewport so the composer stays above
// the on-screen keyboard even after iOS/WebKit viewport scrolling quirks.
const syncViewportShell = (() => {
  let rafId = 0;

  const apply = () => {
    rafId = 0;
    const vv = window.visualViewport;
    const height = vv ? Math.round(vv.height) : window.innerHeight;
    const offsetTop = vv ? Math.max(0, Math.round(vv.offsetTop)) : 0;

    document.documentElement.style.setProperty('--app-height', `${height}px`);
    document.documentElement.style.setProperty('--app-offset-top', `${offsetTop}px`);
  };

  return () => {
    if (rafId) return;
    rafId = window.requestAnimationFrame(apply);
  };
})();

syncViewportShell();
if (window.visualViewport) {
  window.visualViewport.addEventListener('resize', syncViewportShell);
  window.visualViewport.addEventListener('scroll', syncViewportShell);
}
window.addEventListener('resize', syncViewportShell);
window.addEventListener('orientationchange', syncViewportShell);
window.addEventListener('pageshow', syncViewportShell);
document.addEventListener('focusin', syncViewportShell);
document.addEventListener('focusout', () => {
  window.setTimeout(syncViewportShell, 50);
});

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
  const normalized = String(sessionId || '').trim();
  const target = normalized ? (UI_PREFIX + '/' + encodeURIComponent(normalized)) : (UI_PREFIX + '/');
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
    name: typeof session.name === 'string' ? session.name : '',
    title: typeof session.title === 'string' && session.title.trim() ? session.title.trim() : 'New chat',
    longTitle: typeof session.longTitle === 'string' ? session.longTitle : '',
    mode: typeof session.mode === 'string' && session.mode.trim() ? session.mode.trim() : 'chat',
    origin: typeof session.origin === 'string' && session.origin.trim() ? session.origin.trim() : 'tui',
    archived: Boolean(session.archived),
    pinned: Boolean(session.pinned),
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
  if (typeof session.number === 'number' && session.number > 0) result.number = session.number;
  if (typeof session.messageCount === 'number') result.messageCount = session.messageCount;
  return result;
};

const isEphemeralEmptySession = (session) => {
  if (!session || session._serverOnly) return false;
  const msgCount = Number(session.messageCount || 0);
  return session.messages.length === 0
    && msgCount === 0
    && !session.lastResponseId
    && !session.activeResponseId;
};

const loadSessions = () => {
  // Sessions are no longer persisted to localStorage — always start empty
  // and let mergeServerSessions() populate from the server.
  // Clean up legacy key from older versions.
  try { localStorage.removeItem('term_llm_sessions'); } catch { /* ignore */ }
  return [];
};

// Evict messages from in-memory sessions beyond the 10 most-recently-created
// to cap RAM usage.  Evicted sessions become lazy-load stubs.
const MAX_CACHED_MESSAGE_SESSIONS = 10;

const evictStaleSessionMessages = () => {
  const sorted = [...state.sessions]
    .filter(s => s.messages && s.messages.length > 0 && !s._serverOnly)
    .sort((a, b) => b.created - a.created);
  const keep = new Set(sorted.slice(0, MAX_CACHED_MESSAGE_SESSIONS).map(s => s.id));

  for (const s of state.sessions) {
    if (!keep.has(s.id) && s.messages && s.messages.length > 0) {
      s.messages = [];
      s._serverOnly = true;
    }
  }
};

const saveSessions = () => {
  if (state.sessions.length > 100) {
    state.sessions.sort((a, b) => {
      if (Boolean(a.pinned) !== Boolean(b.pinned)) {
        return Number(Boolean(a.pinned)) - Number(Boolean(b.pinned));
      }
      return a.created - b.created;
    });
    state.sessions = state.sessions.slice(-100);
    if (!state.draftSessionActive && !state.sessions.find((s) => s.id === state.activeSessionId)) {
      state.activeSessionId = '';
    }
  }
  evictStaleSessionMessages();
  try {
    localStorage.setItem(STORAGE_KEYS.activeSession, state.activeSessionId || '');
    localStorage.setItem(STORAGE_KEYS.draftSessionActive, state.draftSessionActive ? '1' : '0');
  } catch {
    // storage failure — continue without persistence
  }
};

const getActiveSession = () => state.sessions.find((s) => s.id === state.activeSessionId) || null;

const sessionMatchesSidebarFilters = (session) => {
  if (!session) return false;
  if (session.archived && !state.showHiddenSessions) return false;
  const categories = state.sidebarSessionCategories;
  if (!Array.isArray(categories) || categories.length === 0 || categories.includes('all')) return true;

  const mode = String(session.mode || 'chat').trim().toLowerCase();
  const origin = String(session.origin || 'tui').trim().toLowerCase() || 'tui';
  return categories.some((category) => {
    switch (category) {
      case 'chat':
        return mode === 'chat' && origin === 'tui';
      case 'web':
        return origin === 'web';
      case 'ask':
      case 'plan':
      case 'exec':
        return mode === category;
      default:
        return false;
    }
  });
};

const visibleSessions = () => state.sessions.filter(sessionMatchesSidebarFilters);

const createSession = () => ({
  id: `sess_${generateUUID()}`,
  number: 0,
  name: '',
  title: 'New chat',
  longTitle: '',
  mode: 'chat',
  origin: 'web',
  archived: false,
  pinned: false,
  created: Date.now(),
  messages: [],
  lastResponseId: null,
  activeResponseId: null,
  lastSequenceNumber: 0,
  sessionUsage: null,
  lastUsage: null,
  activeModel: '',
  provider: ''
});

const sessionSlug = (session) => {
  if (session && session.number > 0) return String(session.number);
  return session ? session.id : '';
};

const findSessionBySlug = (slug) => {
  if (!slug) return null;
  const num = /^\d+$/.test(slug) ? Number(slug) : 0;
  if (num > 0) {
    return state.sessions.find(s => s.number === num) || null;
  }
  return state.sessions.find(s => s.id === slug) || null;
};

const ensureActiveSession = () => {
  if (state.draftSessionActive) {
    return null;
  }
  let active = getActiveSession();
  if (active) {
    state.draftSessionActive = false;
    updateURL(sessionSlug(active));
    return active;
  }

  const sorted = [...visibleSessions()].sort((a, b) => {
    if (Boolean(a.pinned) !== Boolean(b.pinned)) {
      return Number(Boolean(b.pinned)) - Number(Boolean(a.pinned));
    }
    return b.created - a.created;
  });
  if (sorted.length === 0) {
    state.activeSessionId = '';
    state.draftSessionActive = true;
    updateURL('');
    saveSessions();
    return null;
  }

  active = sorted[0];
  state.activeSessionId = active.id;
  state.draftSessionActive = false;
  updateURL(sessionSlug(active));
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

let lightboxSrc = '';
let lightboxPrevFocus = null;

const EXPAND_ICON = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="15 3 21 3 21 9"/><polyline points="9 21 3 21 3 15"/><line x1="21" y1="3" x2="14" y2="10"/><line x1="3" y1="21" x2="10" y2="14"/></svg>';
const COLLAPSE_ICON = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="4 14 10 14 10 20"/><polyline points="20 10 14 10 14 4"/><line x1="14" y1="10" x2="21" y2="3"/><line x1="3" y1="21" x2="10" y2="14"/></svg>';
const COPY_ICON = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>';
const CHECK_ICON = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg>';

const resetCopyButton = () => {
  elements.lightboxCopy.classList.remove('copied');
  elements.lightboxCopy.innerHTML = COPY_ICON;
};

const openLightbox = (src, type = 'image') => {
  lightboxPrevFocus = document.activeElement;
  lightboxSrc = src;
  if (type === 'video') {
    elements.lightboxImg.style.display = 'none';
    elements.lightboxVideo.style.display = '';
    elements.lightboxVideo.src = src;
    elements.lightboxVideo.play().catch(() => {});
  } else {
    elements.lightboxVideo.style.display = 'none';
    elements.lightboxImg.style.display = '';
    elements.lightboxImg.src = src;
  }
  elements.lightbox.classList.remove('lightbox-maximized');
  elements.lightboxExpand.innerHTML = EXPAND_ICON;
  elements.lightboxExpand.title = 'Expand';
  elements.lightboxExpand.setAttribute('aria-label', 'Expand');
  resetCopyButton();
  elements.lightboxCopy.style.display = src.startsWith('data:') ? 'none' : '';
  elements.lightbox.classList.add('active');
  document.body.style.overflow = 'hidden';
  elements.lightboxClose.focus();
};

const closeLightbox = () => {
  elements.lightbox.classList.remove('active', 'lightbox-maximized');
  elements.lightboxVideo.pause();
  document.body.style.overflow = '';
  resetCopyButton();
  lightboxSrc = '';
  if (lightboxPrevFocus && typeof lightboxPrevFocus.focus === 'function') {
    lightboxPrevFocus.focus();
    lightboxPrevFocus = null;
  }
  // Defer source clearing until the CSS fade-out transition finishes
  setTimeout(() => {
    if (!elements.lightbox.classList.contains('active')) {
      elements.lightboxImg.src = '';
      elements.lightboxImg.style.display = '';
      elements.lightboxVideo.src = '';
      elements.lightboxVideo.style.display = 'none';
    }
  }, 300);
};

elements.lightboxBackdrop.addEventListener('click', closeLightbox);
elements.lightboxClose.addEventListener('click', closeLightbox);

elements.lightboxCopy.addEventListener('click', () => {
  if (!navigator.clipboard || !navigator.clipboard.writeText) return;
  navigator.clipboard.writeText(lightboxSrc).then(() => {
    elements.lightboxCopy.classList.add('copied');
    elements.lightboxCopy.innerHTML = CHECK_ICON;
    setTimeout(resetCopyButton, 1500);
  }).catch(() => {});
});

elements.lightboxExpand.addEventListener('click', () => {
  const maximized = elements.lightbox.classList.toggle('lightbox-maximized');
  elements.lightboxExpand.innerHTML = maximized ? COLLAPSE_ICON : EXPAND_ICON;
  elements.lightboxExpand.title = maximized ? 'Collapse' : 'Expand';
  elements.lightboxExpand.setAttribute('aria-label', maximized ? 'Collapse' : 'Expand');
});

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && elements.lightbox.classList.contains('active')) {
    closeLightbox();
  }
});

Object.assign(app, {
  STORAGE_KEYS,
  state,
  elements,
  markdownStreaming: app.markdownStreaming,
  generateUUID,
  generateId,
  INTERRUPT_BADGE_META,
  sanitizeInterruptState,
  syncTokenCookie,
  truncate,
  asTimestamp,
  fullDate,
  sessionRefId,
  sessionProgressEntry,
  setSessionOptimisticBusy,
  setSessionServerActiveRun,
  moveSessionProgressState,
  sessionHasInProgressState,
  hasAnySessionInProgressState,
  relativeTime,
  sessionBucket,
  toolIcon,
  formatUsage,
  renderMath,
  splitHeaderModelEffort,
  updateSessionUsageDisplay,
  isNearBottom,
  scrollToBottom,
  setConnectionState,
  setStartupStatus,
  hideStartupSplash,
  updateDocumentTitle,
  syncViewportShell,
  UI_PREFIX,
  parseSidebarSessionCategories,
  isStandalone,
  shouldSuppressPromptAutoFocus,
  refreshNotificationUI,
  registerServiceWorker,
  subscribeToPush,
  shouldAutoSubscribeToPush,
  requestNotificationPermission,
  maybeNotifyResponseComplete,
  sessionIdFromURL,
  sessionSlug,
  findSessionBySlug,
  updateURL,
  sanitizeMessage,
  sanitizeSession,
  isEphemeralEmptySession,
  loadSessions,
  evictStaleSessionMessages,
  saveSessions,
  getActiveSession,
  sessionMatchesSidebarFilters,
  visibleSessions,
  createSession,
  ensureActiveSession,
  findMessageElement,
  refreshRelativeTimes,
  persistAndRefreshShell,
  openLightbox,
  closeLightbox
});
})();
