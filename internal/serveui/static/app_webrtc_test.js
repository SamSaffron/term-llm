'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const source = fs.readFileSync(path.join(__dirname, 'app-webrtc.js'), 'utf8');
const window = {
  __WEBRTC_ENABLED__: false,
  __TERM_LLM_WEBRTC_TESTING__: true,
};
vm.runInNewContext(source, { window }, { filename: 'app-webrtc.js' });

const hooks = window.__TERM_LLM_WEBRTC_TEST_HOOKS__;
if (!hooks || typeof hooks.responseTimeoutForMethod !== 'function') {
  throw new Error('WebRTC timeout test hook was not installed');
}

const cases = [
  ['GET', 1000],
  ['get', 1000],
  ['HEAD', 1000],
  ['OPTIONS', 1000],
  ['POST', 5000],
  ['PATCH', 5000],
  ['PUT', 5000],
  ['DELETE', 5000],
];

for (const [method, expected] of cases) {
  const actual = hooks.responseTimeoutForMethod(method);
  if (actual !== expected) {
    throw new Error(`${method} timeout = ${actual}, want ${expected}`);
  }
}

console.log('PASS: WebRTC first-frame timeout is 1s for reads and 5s for mutations');
