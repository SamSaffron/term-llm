const SHELL_CACHE = 'term-llm-shell-v7';
const SHELL_ASSETS = [
  './manifest.webmanifest',
  './icon-512.png',
];

// Replaced by Go with all chat build files. Hub assets never join this cache.
const HASHED_ASSETS = [];
const ASSET_CACHE = `term-llm-assets-${new URL(self.registration.scope).pathname}`;
const ASSET_METADATA = new URL('./__asset-cache-deployments__', self.registration.scope).href;
const currentAssets = HASHED_ASSETS.map((asset) => new URL(asset, self.registration.scope).href);
let retainedAssets = new Set(currentAssets);
let assetListsLoaded = false;

const previousAssetsFor = (stored) => {
  const previous = stored?.version === SHELL_CACHE ? stored.previous : stored?.current;
  return Array.isArray(previous) ? previous.filter((url) => typeof url === 'string') : [];
};

const pruneAssets = async () => {
  const cache = await caches.open(ASSET_CACHE);
  let stored;
  try {
    stored = await (await cache.match(ASSET_METADATA))?.json();
  } catch {
    // Corrupt/missing metadata starts a fresh retention window.
  }
  const previousAssets = previousAssetsFor(stored);
  retainedAssets = new Set([...currentAssets, ...previousAssets]);
  assetListsLoaded = true;
  for (const request of await cache.keys()) {
    if (request.url !== ASSET_METADATA && !retainedAssets.has(request.url)) await cache.delete(request);
  }
  await cache.put(ASSET_METADATA, new Response(JSON.stringify({
    version: SHELL_CACHE, current: currentAssets, previous: previousAssets,
  }), { headers: { 'Content-Type': 'application/json' } }));
};

const putIfCacheable = async (cache, request, response) => {
  if (!response || !response.ok || response.redirected || response.type === 'opaqueredirect') return response;
  try {
    await cache.put(request, response.clone());
  } catch {
    // Ignore cache write failures. Storage pressure happens.
  }
  return response;
};

self.addEventListener('install', (event) => {
  // Installation must succeed even after an external-auth session expires.
  // Assets populate opportunistically at fetch time; an atomic addAll() here
  // would leave an obsolete worker in control if protected asset fetches redirect.
  event.waitUntil(self.skipWaiting());
});

self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    const keys = await caches.keys();
    await Promise.all(keys.filter((key) => key.startsWith('term-llm-shell-') && key !== SHELL_CACHE).map((key) => caches.delete(key)));
    try { await pruneAssets(); } catch { /* Storage is optional, never block activation. */ }
    await self.clients.claim();
  })());
});

self.addEventListener('fetch', (event) => {
  const { request } = event;
  if (request.method !== 'GET') return;

  const url = new URL(request.url);
  if (url.origin !== self.location.origin) return;

  const scopePath = new URL(self.registration.scope).pathname;
  const isAppRequest = url.pathname.startsWith(scopePath);
  if (!isAppRequest) return;
  // Trusted extensions are mutable external assets, never offline shell assets.
  const relativePath = url.pathname.slice(scopePath.length);
  if (relativePath.startsWith('extensions/') || relativePath.startsWith('api/') || relativePath.startsWith('v1/')) return;

  if (/^dist\/hub\.(js|css)$/.test(url.pathname.slice(scopePath.length))) return;
  // Navigations are authoritative. A cached application shell can mask login,
  // logout, deployment, and reverse-proxy redirects, so only cache versioned
  // static assets; the application is not useful offline without its API.
  if (request.mode === 'navigate') return;

  const assetURL = new URL(url.href);
  assetURL.search = '';
  // Match old hashes too, but only retain current/previous manifest members.
  // Worker processes can restart without activation; reload persisted lists on
  // their first asset fetch so an open old client still gets its cached chunks.
  if (/^dist\/.+-[A-Za-z0-9_-]{8,}\.[a-zA-Z0-9]+$/.test(relativePath)) {
    event.respondWith((async () => {
      let cache;
      try {
        cache = await caches.open(ASSET_CACHE);
        if (!assetListsLoaded) {
          let stored;
          try { stored = await (await cache.match(ASSET_METADATA))?.json(); } catch { /* Ignore corrupt metadata. */ }
          retainedAssets = new Set([...currentAssets, ...previousAssetsFor(stored)]);
          assetListsLoaded = true;
        }
        if (retainedAssets.has(assetURL.href)) {
          const cached = await cache.match(assetURL.href);
          if (cached) return cached;
        }
      } catch {
        // CacheStorage can be unavailable in privacy modes. HTTP caching works.
      }
      const response = await fetch(request);
      // Retired/unknown hashes must not enter either the asset or shell cache.
      return cache && retainedAssets.has(assetURL.href) ? putIfCacheable(cache, assetURL.href, response) : response;
    })());
    return;
  }

  const isShellAsset = SHELL_ASSETS.some((asset) => url.href === new URL(asset, self.registration.scope).href);
  if (!isShellAsset && request.destination !== 'script' && request.destination !== 'style' && request.destination !== 'image' && request.destination !== 'font') {
    return;
  }

  event.respondWith((async () => {
    const cache = await caches.open(SHELL_CACHE);
    const cached = await cache.match(request, { ignoreSearch: false });
    const networkFetch = fetch(request)
      .then((response) => putIfCacheable(cache, request, response))
      .catch(() => null);
    // Versioned shell assets are stale-while-revalidate. Mutable scripts and
    // styles remain network-first, outside the content-addressed asset cache.
    if (cached && isShellAsset) {
      event.waitUntil(networkFetch);
      return cached;
    }
    const response = await networkFetch;
    return response || cached || Response.error();
  })());
});

const scopedNotificationURL = (value) => {
  try {
    const scope = new URL(self.registration.scope);
    const target = new URL(String(value || scope.href), scope);
    if (target.origin !== scope.origin || !target.pathname.startsWith(scope.pathname)) return scope.href;
    return target.href;
  } catch {
    return self.registration.scope;
  }
};

// Routing/read/replay state is scoped independently of deploy caches. Operations are
// serialized so concurrent pushes/messages cannot overwrite a replay or binding.
const NOTIFICATION_CACHE = `term-llm-notifications-${new URL(self.registration.scope).pathname}`;
const NOTIFICATION_STATE = new URL('./__notification-state-v2__', self.registration.scope).href;
const REPLAY_MS = 24 * 60 * 60 * 1000;
const NOTIFICATION_LIMIT = 512;
let notificationWork = Promise.resolve();
const notificationState = (operation) => {
  const work = notificationWork.catch(() => {}).then(async () => {
    const cache = await caches.open(NOTIFICATION_CACHE);
    const stored = await cache.match(NOTIFICATION_STATE);
    const state = stored ? await stored.json() : { bindings: [], events: [], reads: [], enabled: true };
    const now = Date.now();
    for (const field of ['bindings', 'events', 'reads']) {
      state[field] = (Array.isArray(state[field]) ? state[field] : [])
        .filter((entry) => entry.at > now - REPLAY_MS);
    }
    const result = await operation(state, now);
    await cache.put(NOTIFICATION_STATE, new Response(JSON.stringify(state), {
      headers: { 'Content-Type': 'application/json' },
    }));
    return result;
  });
  notificationWork = work;
  return work;
};

const opaqueID = (value) => typeof value === 'string' && value.length > 0 && value.length <= 256;
const inScope = (value) => {
  try {
    const url = new URL(value);
    const scope = new URL(self.registration.scope);
    return url.origin === scope.origin && url.pathname.startsWith(scope.pathname);
  } catch { return false; }
};
const isRead = (state, payload) => state.reads.some((read) =>
  read.store_instance_id === payload.store_instance_id && read.source === payload.source &&
  read.session_id === payload.session_id && read.attention_seq >= payload.attention_seq);

const parsePushPayload = (event) => {
  try { return event.data?.json() || null; } catch { return null; }
};

const decodeNotification = (raw, now) => {
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return null;
  // Only the actual old manual shape is compatible. Never reinterpret a broken
  // completion/unknown version as arbitrary manual text.
  const legacy = Object.keys(raw).every((key) => key === 'title' || key === 'body') &&
    typeof raw.title === 'string' && typeof raw.body === 'string';
  if (!legacy) {
    if (raw.version !== 2 || !['manual', 'completion'].includes(raw.kind) ||
      (raw.kind === 'completion' && raw.source !== new URL(self.registration.scope).pathname.replace(/\/$/, '')) ||
      !opaqueID(raw.event_id) || typeof raw.title !== 'string' || typeof raw.body !== 'string') return null;
    const created = Date.parse(raw.created_at);
    const expires = Date.parse(raw.expires_at);
    if (!Number.isFinite(created) || !Number.isFinite(expires) || created > now + 300000 ||
      expires <= now || expires <= created || expires - created > REPLAY_MS) return null;
    if (raw.kind === 'completion') {
      if (!['turn_id', 'response_id', 'window_id', 'store_instance_id', 'session_id'].every((key) => opaqueID(raw[key])) ||
        !Number.isSafeInteger(raw.attention_seq) || raw.attention_seq <= 0 ||
        !Number.isSafeInteger(raw.run_epoch) || raw.run_epoch <= 0 || typeof raw.source !== 'string' ||
        !['completed', 'failed'].includes(raw.outcome)) return null;
    } else if (['turn_id', 'response_id', 'window_id', 'store_instance_id', 'session_id', 'attention_seq', 'outcome']
      .some((key) => key in raw)) return null;
  }
  const kind = legacy ? 'manual' : raw.kind;
  return {
    title: raw.title,
    options: {
      body: raw.body,
      icon: new URL('./icon-512.png', self.registration.scope).href,
      badge: new URL('./icon-512.png', self.registration.scope).href,
      ...(legacy ? {} : { tag: `term-llm-${kind}:${raw.event_id}` }),
      renotify: false,
      data: { ...raw, kind, url: scopedNotificationURL(raw.url) },
    },
  };
};

self.addEventListener('push', (event) => {
  event.waitUntil(notificationState((state, now) => {
    const notification = decodeNotification(parsePushPayload(event), now);
    if (!notification || state.enabled === false) return null;
    const payload = notification.options.data;
    if (payload.event_id && state.events.some((entry) => entry.id === payload.event_id)) return null;
    if (payload.event_id && state.events.length >= NOTIFICATION_LIMIT) return null;
    if (payload.kind === 'completion' && (isRead(state, payload) || state.read_block_until > now)) return null;
    // Reserve replay identity durably before crossing the OS side-effect boundary.
    // A failed/ambiguous display stays reserved: safety is fail-closed, not exactly-once.
    if (payload.event_id) state.events.push({ id: payload.event_id, at: now });
    return notification;
  }).then(async (notification) => {
    if (!notification) return;
    await self.registration.showNotification(notification.title, notification.options);
    const payload = notification.options.data;
    if (payload.kind !== 'completion') return;
    // A read can persist and snapshot no cards while OS display is still pending.
    // Recheck after display, serialized with read writes, and close only this event.
    // This removes a stale card; it cannot undo a sound the OS has already begun.
    await notificationState(async (state, now) => {
      if (!isRead(state, payload) && !(state.read_block_until > now)) return;
      const displayed = await self.registration.getNotifications({ tag: notification.options.tag });
      for (const card of displayed) {
        if (card.data?.kind === 'completion' && card.data.event_id === payload.event_id) card.close();
      }
    });
  }).catch(() => {
    // Without durable replay/opt-out state automatic delivery fails closed.
  }));
});

self.addEventListener('message', (event) => {
  const message = event.data || {};
  if (!event.source?.id || !inScope(event.source.url)) return;
  if (!['notification-bind', 'notification-read', 'notification-preference'].includes(message.type)) return;
  event.waitUntil(notificationState(async (state, now) => {
    const windows = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    if (!windows.some((client) => client.id === event.source.id && inScope(client.url))) return;
    if (message.type === 'notification-bind') {
      if (message.version !== 2 || !opaqueID(message.window_id) || !opaqueID(message.document_id)) return;
      const previous = state.bindings.find((entry) => entry.window_id === message.window_id);
      if (previous && previous.client_id !== event.source.id &&
        windows.some((client) => client.id === previous.client_id && inScope(client.url))) {
        event.ports?.[0]?.postMessage({ status: 'collision' });
        return;
      }
      state.bindings = state.bindings.filter((entry) =>
        entry.window_id !== message.window_id && entry.client_id !== event.source.id);
      state.bindings.push({ window_id: message.window_id, document_id: message.document_id,
        client_id: event.source.id, at: now });
      state.bindings = state.bindings.slice(-NOTIFICATION_LIMIT);
      // Reply only after durable binding succeeds (below).
      return { status: 'bound',
        ...(previous && previous.document_id !== message.document_id
          ? { previous_document_id: previous.document_id } : {}),
      };
    }
    if (message.type === 'notification-preference') {
      if (typeof message.enabled === 'boolean') state.enabled = message.enabled;
      return;
    }
    if (message.version !== 2 || message.source !== new URL(self.registration.scope).pathname.replace(/\/$/, '') || !opaqueID(message.store_instance_id) ||
      !opaqueID(message.session_id) || typeof message.source !== 'string' ||
      !Number.isSafeInteger(message.attention_seq) || message.attention_seq <= 0) return;
    const read = { store_instance_id: message.store_instance_id, source: message.source,
      session_id: message.session_id, attention_seq: message.attention_seq, at: now };
    const previous = state.reads.find((entry) => entry.store_instance_id === read.store_instance_id &&
      entry.source === read.source && entry.session_id === read.session_id);
    if (previous) { previous.attention_seq = Math.max(previous.attention_seq, read.attention_seq); previous.at = now; }
    else if (state.reads.length < NOTIFICATION_LIMIT) state.reads.push(read);
    else {
      // Never evict live read protection. Without room for an exact watermark,
      // fail closed for automatic alerts through this read's replay horizon.
      state.read_block_until = Math.max(state.read_block_until || 0, now + REPLAY_MS);
    }
    return { read };
  }).then(async (result) => {
    if (result?.read) {
      const notifications = await self.registration.getNotifications();
      for (const notification of notifications) {
        const payload = notification.data;
        if (payload?.kind === 'completion' && isRead({ reads: [result.read] }, payload)) notification.close();
      }
    } else if (result) event.ports?.[0]?.postMessage(result);
  }).catch(() => event.ports?.[0]?.postMessage({ status: 'unavailable' })));
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const payload = event.notification?.data || {};
  const targetURL = scopedNotificationURL(payload.url);
  event.waitUntil((async () => {
    let binding;
    if (payload.kind === 'completion' && opaqueID(payload.window_id)) {
      try { binding = await notificationState((state) => state.bindings.find((entry) => entry.window_id === payload.window_id)); }
      catch { /* No proven owner: open fresh, never a sibling. */ }
    }
    if (binding) {
      const windows = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
      const client = windows.find((entry) => entry.id === binding.client_id && inScope(entry.url));
      if (client) {
        try {
          await client.focus();
          client.postMessage({ type: 'notification-route', url: targetURL });
          return;
        } catch { /* An owner can disappear between lookup and focus. */ }
      }
    }
    await self.clients.openWindow(targetURL);
  })());
});
