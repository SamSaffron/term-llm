import test from 'node:test';
import assert from 'node:assert/strict';
import { hubAssets, hubPageConfig, scriptedSessions, sessionForRoute, NODE_PATH } from './hub.mjs';
import { createDemoAPI } from './mock-api.mjs';

const request = path => new Request('https://demo.test/demo-api' + path);
test('fixed core Hub inventory and browser-local session seeds agree', async () => {
  assert.equal(hubAssets['/healthz'].role, 'hub');
  assert.equal(hubAssets['/api/nodes'].nodes.length, 1);
  const node = hubAssets['/api/nodes'].nodes[0];
  assert.equal(node.source, 'config'); // existing fixed-config resolver, not a UI-added node
  assert.match(node.name, /DEMO/);
  assert.equal(node.proxy_path, NODE_PATH);
  assert.equal(node.new_session_path, NODE_PATH + '?new=1');
  assert.equal(hubPageConfig.canAddNodes, false);
  assert.equal(hubPageConfig.authMode, 'none');
  assert.equal(hubAssets['/api/registration-info'].can_persist_nodes, false);
  assert.deepEqual(hubAssets['/api/delegations'].delegations, []);
  assert.deepEqual(hubAssets['/api/attention'].inbox, []);
  const api = createDemoAPI();
  for (const entry of node.sessions.recent) {
    // Existing Hub route grammar: no query/fragment for a session selector.
    assert.match(entry.resume_path, /^\/node\/demo\/chat\/[a-zA-Z0-9_-]+$/);
    assert.equal(sessionForRoute(entry.resume_path), entry.id);
    assert.equal(sessionForRoute(NODE_PATH + 'chat/' + entry.id), entry.id);
    const data = await (await api.fetch(request(`/v1/sessions?selected_only=1&selected_session=${entry.id}`))).json();
    assert.equal(data.selected_session.id, entry.id);
    assert.equal(data.selected_session.number, entry.number);
    assert.equal(data.selected_session.short_title, entry.short_title);
    assert.equal(data.selected_session.message_count, entry.message_count);
    assert.equal(data.selected_transcript.bodies.messages.length, 1);
  }
  assert.equal(scriptedSessions.length, node.sessions.recent.length);
});

test('route selection is explicit and cannot select an arbitrary backend/session', () => {
  assert.equal(sessionForRoute(NODE_PATH), 'demo-1');
  for (const path of ['/', '/node/other/chat/1', '/node/demo/chat/3', '/node/demo/chat/../1',
    '/api/nodes', '/node/demo/chat/1/extra', '/node/demo/chat/%31']) {
    assert.equal(sessionForRoute(path), undefined);
  }
});
