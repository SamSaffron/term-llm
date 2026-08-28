const SHELL_CACHE = 'term-llm-shell-v6';
const SHELL_ASSETS = [
  './manifest.webmanifest',
  './icon-512.png',
  './dist/app.css',
  './dist/app.js',
];

const putIfCacheable = async (cache, request, response) => {
  if (!response || !response.ok) return response;
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

  // Navigations are authoritative. A cached application shell can mask login,
  // logout, deployment, and reverse-proxy redirects, so only cache versioned
  // static assets; the application is not useful offline without its API.
  if (request.mode === 'navigate') return;

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
    // The rendered HTML requests these versioned shell URLs directly, so they
    // are safe for stale-while-revalidate. Vite imports every stable-named
    // chunk without an AssetVersion query; all chunks therefore stay
    // network-first so a deployment cannot execute an old dependency graph.
    if (cached && isShellAsset) {
      void networkFetch;
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
