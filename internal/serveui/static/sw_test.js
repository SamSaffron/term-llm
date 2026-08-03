'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const source = fs.readFileSync(path.join(__dirname, 'sw.js'), 'utf8');
const listeners = new Map();
const entries = new Map();
const cache = {
  async addAll() {},
  async match(key) {
    const response = entries.get(String(key));
    return response ? response.clone() : undefined;
  },
  async put(key, response) { entries.set(String(key), response.clone()); },
};
const pendingFetches = [];
let fetchCalls = 0;
const context = {
  self: {
    location: { origin: 'https://example.test' },
    registration: { scope: 'https://example.test/chat/' },
    clients: { claim: async () => {} },
    skipWaiting: async () => {},
    addEventListener(type, handler) { listeners.set(type, handler); },
  },
  caches: {
    async open() { return cache; },
    async keys() { return []; },
    async delete() { return true; },
  },
  fetch() {
    fetchCalls += 1;
    return new Promise((resolve) => { pendingFetches.push(resolve); });
  },
  URL,
  Response,
  Promise,
  setTimeout,
  console,
};
vm.runInNewContext(source, context, { filename: 'sw.js' });

function navigationEvent(url = 'https://example.test/chat/') {
  let responsePromise;
  let lifetimePromise;
  let intercepted = false;
  return {
    request: { method: 'GET', url, mode: 'navigate', destination: 'document' },
    respondWith(value) { intercepted = true; responsePromise = Promise.resolve(value); },
    waitUntil(value) { lifetimePromise = Promise.resolve(value); },
    response: () => responsePromise,
    lifetime: () => lifetimePromise,
    intercepted: () => intercepted,
  };
}

(async () => {
  entries.set('./index.html', new Response('cached shell', { status: 200 }));
  const event = navigationEvent();
  listeners.get('fetch')(event);

  const response = await event.response();
  if (await response.text() !== 'cached shell') throw new Error('slow navigation did not fall back to the cached shell');
  if (fetchCalls !== 1 || pendingFetches.length !== 1) throw new Error('navigation did not start one network refresh');

  const immediateReload = navigationEvent();
  listeners.get('fetch')(immediateReload);
  let immediateReloadSettled = false;
  immediateReload.response().then(() => { immediateReloadSettled = true; });
  await new Promise((resolve) => setTimeout(resolve, 120));
  if (immediateReloadSettled) throw new Error('immediate follow-up reload reused the stale shell');
  pendingFetches[1](new Response('<script>window.TERM_LLM_UI_VERSION="v3";</script>follow-up shell', { status: 200 }));
  if (!(await (await immediateReload.response()).text()).endsWith('follow-up shell')) {
    throw new Error('immediate follow-up reload did not wait for the authoritative network shell');
  }
  await immediateReload.lifetime();

  pendingFetches[0](new Response('<script>window.TERM_LLM_UI_VERSION="v3";</script>fresh shell', { status: 200 }));
  await event.lifetime();
  const refreshed = await cache.match('./index.html');
  if (!(await refreshed.text()).endsWith('fresh shell')) throw new Error('compatible background refresh did not update cached shell');

  const compatibleShell = await (await cache.match('./index.html')).text();
  const upgradeEvent = navigationEvent();
  listeners.get('fetch')(upgradeEvent);
  await upgradeEvent.response();
  pendingFetches[2](new Response('<script>window.TERM_LLM_UI_VERSION="v4";</script>next release', { status: 200 }));
  await upgradeEvent.lifetime();
  if (await (await cache.match('./index.html')).text() !== compatibleShell) {
    throw new Error('old service worker mixed a new HTML release into its shell cache');
  }

  const widgetEvent = navigationEvent('https://example.test/chat/widgets/clock/');
  listeners.get('fetch')(widgetEvent);
  if (widgetEvent.intercepted()) throw new Error('service worker intercepted a non-shell widget navigation');
  if ((await (await cache.match('./index.html')).text()).endsWith('WIDGET PAGE')) throw new Error('widget navigation poisoned the shell cache');

  console.log('PASS: shell navigation uses bounded cache fallback without intercepting widget documents');
})().catch((error) => {
  console.error(error && error.stack ? error.stack : error);
  process.exitCode = 1;
});
