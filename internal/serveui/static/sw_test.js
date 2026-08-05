'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const source = fs.readFileSync(path.join(__dirname, 'sw.js'), 'utf8');
const listeners = new Map();
const entries = new Map();
let addAllCalls = 0;
const cache = {
  async addAll() { addAllCalls += 1; },
  async match(key) {
    const response = entries.get(String(key));
    return response ? response.clone() : undefined;
  },
  async put(key, response) { entries.set(String(key), response.clone()); },
};
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
  async fetch(request) {
    fetchCalls += 1;
    return new Response(`network:${request.url}`, { status: 200 });
  },
  URL,
  Response,
  Promise,
  console,
};
vm.runInNewContext(source, context, { filename: 'sw.js' });

function fetchEvent({ url, mode = 'cors', destination = 'script' }) {
  let responsePromise;
  let intercepted = false;
  return {
    request: { method: 'GET', url, mode, destination },
    respondWith(value) { intercepted = true; responsePromise = Promise.resolve(value); },
    waitUntil() {},
    response: () => responsePromise,
    intercepted: () => intercepted,
  };
}

(async () => {
  await new Promise((resolve, reject) => {
    listeners.get('install')({ waitUntil(value) { Promise.resolve(value).then(resolve, reject); } });
  });
  if (addAllCalls !== 0) {
    throw new Error('install pre-cached protected assets and can fail after auth expiry');
  }

  for (const url of [
    'https://example.test/chat/',
    'https://example.test/chat/index.html',
    'https://example.test/chat/session-123',
    'https://example.test/chat/widgets/clock/',
  ]) {
    const event = fetchEvent({ url, mode: 'navigate', destination: 'document' });
    listeners.get('fetch')(event);
    if (event.intercepted()) throw new Error(`service worker intercepted navigation ${url}`);
  }
  if (fetchCalls !== 0) throw new Error('service worker fetched an authoritative navigation');

  const assetURL = 'https://example.test/chat/app-core.js?v=v3';
  const asset = fetchEvent({ url: assetURL });
  listeners.get('fetch')(asset);
  if (!asset.intercepted()) throw new Error('service worker stopped caching versioned static assets');
  const response = await asset.response();
  if ((await response.text()) !== `network:${assetURL}` || fetchCalls !== 1) {
    throw new Error('versioned static asset did not use the cache/network path');
  }

  console.log('PASS: service worker leaves navigation authoritative and caches only static assets');
})().catch((error) => {
  console.error(error && error.stack ? error.stack : error);
  process.exitCode = 1;
});
