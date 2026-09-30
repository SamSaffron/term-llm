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

const completionNotification = (raw) => {
  const valid = raw && raw.version === 1 && typeof raw.event_id === 'string' && raw.event_id;
  const eventID = valid ? raw.event_id : 'malformed-push';
  return {
    title: valid ? String(raw.title || 'term-llm') : 'term-llm notification',
    options: {
      body: valid ? String(raw.body || '') : 'Open term-llm to view this update.',
      icon: new URL('./icon-512.png', self.registration.scope).href,
      badge: new URL('./icon-512.png', self.registration.scope).href,
      tag: `term-llm-completion:${eventID}`,
      renotify: false,
      data: {
        event_id: eventID,
        response_id: valid ? String(raw.response_id || '') : '',
        url: scopedNotificationURL(valid ? raw.url : self.registration.scope),
      },
    },
  };
};

const parsePushPayload = (event) => {
  if (!event.data) return null;
  try {
    return event.data.json();
  } catch {
    try {
      return JSON.parse(event.data.text());
    } catch {
      return null;
    }
  }
};

const notifyVisibleClients = async (tag) => {
  const windows = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
  const scopePath = new URL(self.registration.scope).pathname;
  for (const client of windows) {
    if (new URL(client.url).pathname.startsWith(scopePath) && client.visibilityState === 'visible') {
      client.postMessage({ type: 'completion-push-shown', tag });
    }
  }
};

self.addEventListener('push', (event) => {
  event.waitUntil((async () => {
    const notification = completionNotification(parsePushPayload(event));
    // userVisibleOnly requires every actual push delivery to show (or replace)
    // a visible notification. Foreground pages close the exact tag afterward.
    await self.registration.showNotification(notification.title, notification.options);
    await notifyVisibleClients(notification.options.tag);
  })());
});

self.addEventListener('message', (event) => {
  const message = event.data || {};
  if (message.type !== 'completion-notification') return;
  event.waitUntil((async () => {
    const notification = completionNotification(message.payload);
    const existing = await self.registration.getNotifications({ tag: notification.options.tag });
    if (!existing.length) {
      await self.registration.showNotification(notification.title, notification.options);
    }
    event.ports?.[0]?.postMessage({
      type: 'completion-notification-handled',
      tag: notification.options.tag,
    });
  })());
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const targetURL = scopedNotificationURL(event.notification?.data?.url);

  event.waitUntil((async () => {
    const windows = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    const scopePath = new URL(self.registration.scope).pathname;
    for (const client of windows) {
      if (!new URL(client.url).pathname.startsWith(scopePath)) continue;
      await client.focus();
      if (typeof client.navigate === 'function') {
        try {
          await client.navigate(targetURL);
          return;
        } catch {
          // Safari may expose navigate without implementing it for this client.
        }
      }
      client.postMessage({ type: 'notification-route', url: targetURL });
      return;
    }
    await self.clients.openWindow(targetURL);
  })());
});
