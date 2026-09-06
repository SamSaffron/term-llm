import { expect, test } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

// Dedicated clean-room harness: no mocked HTTP responses or model credentials.
test('creates a Dracula workspace, manages extensions, and repairs a broken theme', async ({
  page,
  context,
}, testInfo) => {
  test.skip(!process.env.TERM_LLM_EXTENSION_DIR, 'Run scripts/extensions_smoke.sh');
  const root = process.env.TERM_LLM_EXTENSION_DIR!;
  const configPath = process.env.TERM_LLM_EXTENSION_CONFIG!;
  const mainConfigPath = process.env.TERM_LLM_EXTENSION_MAIN_CONFIG!;
  const mainConfig = await readFile(mainConfigPath, 'utf8');
  const api = async (route: string, body?: unknown) => {
    const response =
      body === undefined
        ? await page.request.get(`admin/extensions/${route}`)
        : await page.request.post(`admin/extensions/${route}`, { data: body });
    expect(response.ok(), await response.text()).toBe(true);
    return response.json();
  };
  const initial = await api('status');
  await api('config', { enabled: [], revision: initial.config_revision });
  await page.goto('./');
  await expect(page.getByRole('textbox', { name: 'Message' })).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath('01-default.png'), fullPage: true });
  if (!(await page.locator('#settingsBtn').isVisible()))
    await page.getByRole('button', { name: 'Open sidebar' }).click();
  await page.locator('#settingsBtn').click();
  const settings = page.getByRole('dialog', { name: 'Settings' });
  await expect(settings.getByRole('tab')).toHaveCount(4);
  if (testInfo.project.name === 'desktop')
    expect((await settings.boundingBox())!.width).toBeGreaterThan(700);
  await page.screenshot({ path: testInfo.outputPath('01-settings-model.png'), fullPage: true });
  await settings.getByRole('tab', { name: 'Extensions', exact: true }).click();
  await expect(settings.getByText('Dracula', { exact: true })).toBeVisible();
  expect(await settings.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(
    true,
  );
  await page.screenshot({
    path: testInfo.outputPath('01-settings-extensions.png'),
    fullPage: true,
  });
  await page.getByRole('button', { name: 'Close Settings', exact: true }).click();

  // Act like the extension builder: files were installed in the isolated extension root,
  // and it edits config directly, then requests a registry reload.
  const before = await readFile(configPath, 'utf8');
  expect(before).toContain('enabled: []');
  await writeFile(configPath, before.replace('enabled: []', 'enabled: [dracula, studio-clock]'));
  const active = await api('activate', {});
  expect(active.enabled).toEqual(['dracula', 'studio-clock']);
  // No manual refresh: the activation request reloads this idle browser.
  await expect
    .poll(() =>
      page.evaluate(() =>
        getComputedStyle(document.documentElement).getPropertyValue('--bg').trim(),
      ),
    )
    .toBe('#282a36');
  await expect(page.locator('.studio-clock')).toHaveCount(1);
  await expect
    .poll(() => page.locator('#root').evaluate((root) => getComputedStyle(root).backgroundColor))
    .toBe('rgb(40, 42, 54)');
  await expect
    .poll(() => page.locator('#root').evaluate((root) => getComputedStyle(root).color))
    .toBe('rgb(248, 248, 242)');
  await page
    .getByRole('textbox', { name: 'Message' })
    .fill('My midnight workspace: calm colors, clear code, and a little room to think.');
  await page.screenshot({ path: testInfo.outputPath('02-dracula.png'), fullPage: true });

  if (!(await page.locator('#settingsBtn').isVisible()))
    await page.getByRole('button', { name: 'Open sidebar' }).click();
  await page.locator('#settingsBtn').click();
  await page.getByRole('tab', { name: 'Extensions', exact: true }).click();
  const section = page.getByRole('region', { name: 'Extensions', exact: true });
  await expect(section.getByText('Dracula', { exact: true })).toBeVisible();
  await section.scrollIntoViewIfNeeded();
  await page.screenshot({ path: testInfo.outputPath('03-interface-settings.png'), fullPage: true });
  await section.getByRole('button', { name: /Make UI Mine/ }).click();
  await expect(page.getByRole('textbox', { name: 'Message' })).toBeVisible();

  await page
    .getByRole('textbox', { name: 'Message' })
    .fill('Help me refine this midnight workspace.');
  const submission = page.waitForRequest(
    (request) => request.method() === 'POST' && request.url().endsWith('/v1/responses'),
  );
  await page.getByRole('button', { name: 'Send message' }).click();
  expect((await submission).postDataJSON().ui_context.loaded).toEqual(['dracula', 'studio-clock']);
  await expect(page.getByRole('heading', { name: 'Debug Provider Output' }).last()).toBeVisible();

  // Active service workers must never retain extension assets.
  await page.evaluate(async () => {
    if ('serviceWorker' in navigator) await navigator.serviceWorker.ready;
  });
  await page.reload();
  await expect(page.locator('.studio-clock')).toHaveCount(1);
  const cached = await page.evaluate(async () => {
    const names = await caches.keys();
    const urls = await Promise.all(
      names.map(async (name) => (await (await caches.open(name)).keys()).map((r) => r.url)),
    );
    return urls.flat().filter((url) => url.includes('/extensions/'));
  });
  expect(cached).toEqual([]);

  // Editing does not mutate a live snapshot; reload creates a new generation.
  const cssPath = path.join(root, 'dracula', 'style.css');
  const original = await readFile(cssPath, 'utf8');
  await writeFile(cssPath, original + '\n#root { display: none !important; }\n');
  const asset = await page.request.get(`extensions/${active.generation}/dracula/style.css`);
  expect(await asset.text()).not.toContain('display: none !important');
  const broken = await api('reload', {});
  expect(broken.generation).not.toBe(active.generation);
  await page.reload();
  await expect(page.locator('#root')).toBeHidden();

  const recovery = await context.newPage();
  const extensionRequests: string[] = [];
  recovery.on('request', (request) => {
    if (/\/extensions\/[a-f0-9]{20}\//.test(request.url())) extensionRequests.push(request.url());
  });
  await recovery.goto('./?safe-mode=1');
  await expect(recovery.getByRole('textbox', { name: 'Message' })).toBeVisible();
  if (!(await recovery.locator('#settingsBtn').isVisible()))
    await recovery.getByRole('button', { name: 'Open sidebar' }).click();
  await recovery.locator('#settingsBtn').click();
  await recovery.getByRole('tab', { name: 'Extensions', exact: true }).click();
  await expect(recovery.getByText('Safe mode is on', { exact: true })).toBeVisible();
  expect(extensionRequests).toEqual([]);
  await recovery.getByRole('button', { name: 'Disable all', exact: true }).click();
  await recovery.getByRole('button', { name: 'Save configuration', exact: true }).click();
  await expect.poll(async () => (await api('status')).enabled).toEqual([]);
  await recovery.getByRole('button', { name: 'Exit safe mode and reload' }).click();
  await expect(recovery.getByRole('textbox', { name: 'Message' })).toBeVisible();
  await expect(recovery.locator('.studio-clock')).toHaveCount(0);
  await writeFile(cssPath, original);
  await api('reload', {});
  await recovery.close();
  expect(await readFile(mainConfigPath, 'utf8')).toBe(mainConfig);
});
