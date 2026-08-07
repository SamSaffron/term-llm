const SHELL_CACHE = 'term-llm-shell-v3';
const SHELL_ASSETS = [
  './manifest.webmanifest',
  './icon-512.png',
  './app.css',
  './markdown-setup.js',
  './markdown-streaming.js',
  './decoration.js',
  './transcript-window.js',
  './active-response.js',
  './conversation.js',
  './app-core.js',
  './toast.js',
  './app-network.js',
  './app-plan.js',
  './slash-commands.js',
  './app-render.js',
  './app-attachments.js',
  './app-stream.js',
  './app-response-effects.js',
  './app-send.js',
  './app-runtime.js',
  './app-interject.js',
  './app-modals.js',
  './app-composer.js',
  './app-skills.js',
  './side-question.js',
  './app-sidebar.js',
  './app-sessions.js',
  './app-path-notes.js',
  './app-branching.js',
  './app-session-events.js',
  './app-mcp.js',
  './app-goals-location.js',
  './app-message-convert.js',
  './intent-storage.js',
  './app-session-admin.js',
  './app-diffs.js',
  './app-worktrees.js',
  // term-llm:webrtc-shell-asset
  './vendor/marked/marked.umd.min.js?v=16.3.0',
  './vendor/dompurify/purify.min.js?v=3.2.7'
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
    if (cached) {
      void networkFetch;
      return cached;
    }
    const response = await networkFetch;
    return response || Response.error();
  })());
});

self.addEventListener('push', (event) => {
  const data = event.data?.json() || {};
  event.waitUntil(
    self.registration.showNotification(data.title || 'term-llm', {
      body: data.body || '',
      icon: './icon-512.png',
      badge: './icon-512.png',
      data: { url: data.url || self.registration.scope }
    })
  );
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const targetURL = String(event.notification?.data?.url || self.registration.scope);

  event.waitUntil((async () => {
    const clients = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    for (const client of clients) {
      const url = new URL(client.url);
      if (url.pathname.startsWith(new URL(self.registration.scope).pathname)) {
        await client.focus();
        if ('navigate' in client) {
          await client.navigate(targetURL);
        }
        return;
      }
    }
    await self.clients.openWindow(targetURL);
  })());
});
