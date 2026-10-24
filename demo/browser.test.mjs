// Optional real-browser acceptance after make frontend && node demo/build.mjs.
// Uses the existing frontend Playwright dependency and a system Chromium.
import test from 'node:test';
import assert from 'node:assert/strict';
import { chromium, expect } from '../frontend/node_modules/@playwright/test/index.mjs';
import { startStaticServer } from './static-server.mjs';

const send = async (page, text) => {
  await page.getByRole('textbox', { name: 'Message', exact: true }).fill(text);
  await page.getByRole('button', { name: 'Send message', exact: true }).click();
};

test('root and nested demo chat work on static nginx; reload resets the matching conversation', async () => {
  const server = await startStaticServer();
  let browser;
  try {
    browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium', args: ['--no-sandbox'] });
    const page = await browser.newPage();
    const errors = [], failed = [], network = [];
    page.on('pageerror', error => errors.push(error.message));
    page.on('requestfailed', request => failed.push(request.url()));
    page.on('request', request => network.push(request.url()));
    await page.goto(server.origin + '/');
    await expect(page.getByText('Welcome to the', { exact: false }).last()).toContainText('term-llm UI demo');
    await send(page, 'help');
    await expect(page.getByText('Scripted demo, real native actions', { exact: false }).last()).toBeVisible();
    await expect(page.getByRole('button', { name: 'Send message', exact: true })).toBeVisible();
    for (const path of ['/node/demo/', '/node/demo/chat/1', '/node/demo/chat/demo-1']) {
      await page.goto(server.origin + path);
      await expect(page.getByText('Welcome to the', { exact: false }).last()).toContainText('term-llm UI demo');
    }
    await page.goto(server.origin + '/node/demo/?new=1');
    await expect(page.getByRole('textbox', { name: 'Message', exact: true })).toBeVisible();
    await expect(page.locator('article')).toHaveCount(0);
    await send(page, 'fresh conversation');
    await expect(page.getByText('fresh conversation', { exact: true }).last()).toBeVisible();
    for (const path of ['/node/demo/chat/2', '/node/demo/chat/demo-2']) {
      await page.goto(server.origin + path);
      await expect(page.getByText('This is the static Hub demo conversation.', { exact: false })).toBeVisible();
      await send(page, 'unique local message');
      await expect(page.getByRole('button', { name: 'Send message', exact: true })).toBeVisible();
      assert.equal(new URL(page.url()).pathname, path);
      await expect(page.getByText('unique local message', { exact: true })).toBeVisible();
      await page.reload();
      await expect(page.getByText('This is the static Hub demo conversation.', { exact: false })).toBeVisible();
      await expect(page.getByText('unique local message', { exact: true })).toHaveCount(0);
    }
    assert.deepEqual(errors, []);
    assert.deepEqual(failed, []);
    assert(network.every(url => new URL(url).origin === server.origin), 'all HTTP traffic stays at demo origin');
    assert(network.every(url => !new URL(url).pathname.startsWith('/demo-api')), 'chat mock never reaches nginx');
  } finally {
    await browser?.close();
    await server.close();
  }
});

test('existing device-tool walkthrough still uses the real frontend WebMCP switch and inline result protocol', async () => {
  const server = await startStaticServer();
  let browser;
  try {
    browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium', args: ['--no-sandbox'] });
    const page = await browser.newPage();
    // Public WebMCP test double, not the private native bridge or real iOS proof.
    await page.addInitScript(() => {
      window.demoToolCalls = [];
      const context = Object.assign(new EventTarget(), {
        async getTools() {
          return [
            { name: 'ping', description: 'Device ping', inputSchema: { type: 'object', properties: { message: { type: 'string' } } } },
            { name: 'playlist_create_demo', description: 'Create sample playlist', inputSchema: { type: 'object', properties: {} } },
          ];
        },
        async executeTool(tool, input) {
          window.demoToolCalls.push({ tool: tool.name, input });
          if (tool.name === 'playlist_create_demo') return JSON.stringify({
            id: 'browser-test-playlist', name: 'Browser Test Sample Playlist', songs: 5,
            entries: Array.from({ length: 5 }, (_, index) => ({ title: `Sample ${index + 1}` })),
            sample_notice: 'Sample tracks are fictional.',
          });
          return 'browser-test-device-pong';
        },
      });
      Object.defineProperty(document, 'modelContext', { value: context });
      Object.defineProperty(window, '__termLLMDeviceTools', { value: { device: 'iPhone' } });
    });
    await page.goto(server.origin + '/');
    await expect(page.getByRole('textbox', { name: 'Message', exact: true })).toBeVisible();
    assert.deepEqual(await page.evaluate(() => window.demoToolCalls), []);
    await page.getByRole('button', { name: 'Add', exact: true }).click();
    await page.getByRole('menuitem', { name: 'MCP servers', exact: true }).click();
    const dialog = page.getByRole('dialog', { name: 'MCP servers', exact: true });
    const toggle = dialog.getByRole('checkbox', { name: 'Enable iPhone', exact: true });
    await expect(toggle).not.toBeChecked();
    await toggle.check();
    await page.keyboard.press('Escape');
    await send(page, 'ping');
    await expect(page.getByText('browser-test-device-pong', { exact: false }).last()).toBeVisible();
    await expect(page.getByRole('button', { name: 'Send message', exact: true })).toBeVisible();
    await send(page, 'create playlist');
    await expect(page.getByText('Browser Test Sample Playlist', { exact: false }).last()).toBeVisible();
    await expect(page.getByText('Sample tracks are fictional.', { exact: false }).last()).toBeVisible();
    assert.deepEqual(await page.evaluate(() => window.demoToolCalls), [
      { tool: 'ping', input: { message: 'Hello from the demo' } },
      { tool: 'playlist_create_demo', input: {} },
    ]);
  } finally {
    await browser?.close();
    await server.close();
  }
});


test('bootstrap preserves the legacy stock-shell query/config contract', async () => {
  const server = await startStaticServer();
  let browser;
  try {
    browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium', args: ['--no-sandbox'] });
    const page = await browser.newPage();
    // Stand in for an older stock entry that consumes the original query hint.
    // Production assets/routes are otherwise served by the same static nginx.
    await page.route('**/assets/app-*.js', route => route.fulfill({
      contentType: 'application/javascript',
      body: `window.legacyBoot = { session: new URL(location.href).searchParams.get('session'),
        prefix: window.TERM_LLM_UI_PREFIX, authorized: window.TERM_LLM_SHELL_AUTHORIZED }`,
    }));
    for (const [path, session] of [['/', 'demo-1'], ['/node/demo/chat/2', 'demo-2']]) {
      await page.goto(server.origin + path);
      await page.waitForFunction(() => window.legacyBoot);
      assert.deepEqual(await page.evaluate(() => window.legacyBoot), { session, prefix: '/demo-api', authorized: true });
    }
  } finally {
    await browser?.close();
    await server.close();
  }
});
