// Integration: build first, then test real HTTP served by static nginx.
import test from 'node:test';
import assert from 'node:assert/strict';
import { hubAssets, hubPageConfig, NODE_PATH } from './hub.mjs';
import { startStaticServer } from './static-server.mjs';

test('static nginx serves the published native Hub contract and nested shell assets', async () => {
  const server = await startStaticServer();
  try {
    const get = path => fetch(server.origin + path, { redirect: 'manual' });
    for (const [path, expected] of Object.entries(hubAssets)) {
      const response = await get(path + '?limit=30');
      assert.equal(response.status, 200, path);
      assert.match(response.headers.get('content-type'), /^application\/json/);
      assert.deepEqual(await response.json(), expected);
      assert.equal((await fetch(server.origin + path, { method: 'HEAD' })).status, 200);
    }
    const html = await (await get('/')).text();
    const config = html.match(/data-hub-config="([^"]+)"/)[1].replaceAll('&#34;', '"');
    assert.deepEqual(JSON.parse(config), hubPageConfig);
    const paths = [NODE_PATH, NODE_PATH + '?new=1', ...hubAssets['/api/nodes'].nodes[0].sessions.recent.flatMap(s =>
      [s.resume_path, NODE_PATH + 'chat/' + s.id])];
    for (const path of paths) {
      const response = await get(path);
      assert.equal(response.status, 200, path);
      assert.match(response.headers.get('content-type'), /^text\/html/);
      assert.equal(await response.text(), html);
      for (const [, asset] of html.matchAll(/(?:href|src)="(\/[^" ]+)"/g)) {
        const assetResponse = await get(asset);
        assert.equal(assetResponse.status, 200, asset);
        assert.notEqual(assetResponse.headers.get('content-type'), 'text/html');
      }
    }
    for (const path of ['/api/missing', '/api/auth/session', '/demo-api/v1/sessions',
      '/node/other/', '/node/demo/chat/3', '/node/demo/chat/1/extra', '/assets/missing.js']) {
      assert.equal((await get(path)).status, 404, path);
    }
    for (const method of ['POST', 'PATCH', 'DELETE', 'PUT', 'OPTIONS']) {
      for (const path of ['/', '/api/nodes', '/api/nodes/order', '/api/attention/clear', NODE_PATH]) {
        assert.equal((await fetch(server.origin + path, { method })).status, 405, method + ' ' + path);
      }
    }
  } finally { await server.close(); }
});
