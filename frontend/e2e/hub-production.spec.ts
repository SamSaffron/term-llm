import { expect, test } from '@playwright/test';

const hubRoot = process.env.TERM_LLM_HUB_SMOKE_ROOT || '';
const hubToken = process.env.TERM_LLM_HUB_SMOKE_TOKEN || '';

test('Hub bearer mount preserves a proxied send across reload', async ({ page }, testInfo) => {
  test.skip(!hubRoot || !hubToken, 'production-shaped Hub smoke is not configured');
  test.skip(testInfo.project.name === 'mobile', 'the exact production path is covered once');
  test.setTimeout(60_000);

  const unauthenticated = await page.request.get(`${hubRoot}api/nodes`);
  expect(unauthenticated.status()).toBe(401);

  // Query-token bootstrap must become an HTTP-only cookie and remove the token
  // from the address bar before any node authority is exercised.
  await page.goto(`${hubRoot}?token=${encodeURIComponent(hubToken)}`);
  await expect(page).toHaveURL(new RegExp(`${hubRoot.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`));
  expect(page.url()).not.toContain('token=');
  await expect.poll(async () => (await page.request.get(`${hubRoot}api/nodes`)).status()).toBe(200);

  // The node has WebRTC enabled, but Hub mounts must keep browser traffic on
  // the authenticated same-origin proxy.
  const nodeRoot = `${hubRoot}node/production-node/`;
  await page.goto(`${nodeRoot}?new=1`);
  await expect(page.getByRole('textbox', { name: 'Message' })).toBeVisible();
  const mount = await page.evaluate(() => ({
    prefix: window.TERM_LLM_UI_PREFIX,
    nodeBasePath: window.TERM_LLM_HUB?.nodeBasePath,
    webrtc: window.__WEBRTC_ENABLED__,
  }));
  expect(mount).toEqual({
    prefix: '/hub/node/production-node',
    nodeBasePath: '/chat',
    webrtc: false,
  });

  await page.getByRole('textbox', { name: 'Message' }).fill('Production shaped Hub resume');
  await page.getByRole('button', { name: 'Send message' }).click();
  await expect(page).toHaveURL(/\/hub\/node\/production-node\/chat\//);

  // Reload immediately: this disconnects the original response body while the
  // Go response run continues. The mounted client must resume through the Hub
  // with its cookie and the Hub-injected direct-node bearer credential.
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Debug Provider Output' }).last()).toBeVisible({
    timeout: 20_000,
  });
  await expect(page.locator('#messages').getByText('Production shaped Hub resume')).toBeVisible();
});
