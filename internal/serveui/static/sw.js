const SHELL_CACHE = 'term-llm-shell-v2';
const SHELL_ASSETS = [
  './',
  './index.html',
  './manifest.webmanifest',
  './icon-512.png',
  './app.css',
  './app-core.js',
  './app-render.js',
  './app-stream.js',
  './app-sessions.js',
  './vendor/katex/katex.min.css?v=0.16.38',
  './vendor/katex/katex.min.js?v=0.16.38',
  './vendor/katex/auto-render.min.js?v=0.16.38',
  './vendor/marked/marked.umd.min.js?v=16.3.0',
  './vendor/hljs/github-dark.min.css?v=11.11.1',
  './vendor/hljs/github.min.css?v=11.11.1',
  './vendor/hljs/highlight.min.js?v=11.11.1',
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
  event.waitUntil((async () => {
    const cache = await caches.open(SHELL_CACHE);
    await cache.addAll(SHELL_ASSETS);
    await self.skipWaiting();
  })());
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

  if (request.mode === 'navigate') {
    event.respondWith((async () => {
      const cache = await caches.open(SHELL_CACHE);
      try {
        const response = await fetch(request);
        await putIfCacheable(cache, './index.html', response.clone());
        return response;
      } catch {
        return (await cache.match('./index.html')) || (await cache.match('./'));
      }
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
