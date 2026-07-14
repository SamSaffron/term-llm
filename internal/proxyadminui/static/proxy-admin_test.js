'use strict';

const assert = require('node:assert/strict');
const helpers = require('./proxy-admin.js');

assert.equal(helpers.relativeTime('2026-07-14T10:00:00Z', Date.parse('2026-07-14T10:00:30Z')), 'just now');
assert.equal(helpers.relativeTime('2026-07-14T11:00:00Z', Date.parse('2026-07-14T10:00:00Z')), 'in 1h');
assert.equal(helpers.relativeTime('2026-07-14T09:30:00Z', Date.parse('2026-07-14T10:00:00Z')), '30m ago');
assert.equal(helpers.ttlSeconds('604800', 1), 604800);
assert.equal(helpers.ttlSeconds('custom', 12), 43200);
assert.equal(helpers.errorMessage({ error: { message: 'denied' } }, 'fallback'), 'denied');
assert.equal(helpers.errorMessage(null, 'fallback'), 'fallback');

const secretNode = { textContent: 'tlp_secret_value' };
helpers.clearSecret(secretNode);
assert.equal(secretNode.textContent, '', 'dismissal must remove one-time secrets from the DOM node');

console.log('proxy admin JavaScript tests passed');
