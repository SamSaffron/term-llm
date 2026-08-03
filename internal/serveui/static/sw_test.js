'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const source = fs.readFileSync(path.join(__dirname, 'sw.js'), 'utf8');
const listeners = new Map();
const entries = new Map();
const cache = {
  async addAll() {},
  async match(key) { return entries.get(String(key)) || undefined; },
  async put(key, response) { entries.set(String(key), response); },
};
let resolveFetch;
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
    return new Promise((resolve) => { resolveFetch = resolve; });
  },
  URL,
  Response,
  Promise,
  console,
};
vm.runInNewContext(source, context, { filename: 'sw.js' });

function navigationEvent() {
  let responsePromise;
  let lifetimePromise;
  return {
    request: { method: 'GET', url: 'https://example.test/chat/', mode: 'navigate', destination: 'document' },
    respondWith(value) { responsePromise = Promise.resolve(value); },
    waitUntil(value) { lifetimePromise = Promise.resolve(value); },
    response: () => responsePromise,
    lifetime: () => lifetimePromise,
  };
}

(async () => {
  entries.set('./index.html', new Response('cached shell', { status: 200 }));
  const event = navigationEvent();
  listeners.get('fetch')(event);

  const response = await event.response();
  if (await response.text() !== 'cached shell') throw new Error('navigation did not return cached shell immediately');
  if (fetchCalls !== 1 || typeof resolveFetch !== 'function') throw new Error('navigation did not start one background refresh');

  resolveFetch(new Response('fresh shell', { status: 200 }));
  await event.lifetime();
  const refreshed = await cache.match('./index.html');
  if (await refreshed.text() !== 'fresh shell') throw new Error('background refresh did not update cached shell');

  console.log('PASS: navigation returns cached shell before background network refresh');
})().catch((error) => {
  console.error(error && error.stack ? error.stack : error);
  process.exitCode = 1;
});
