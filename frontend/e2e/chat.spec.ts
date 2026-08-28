import { test, expect, type Page, type Route } from '@playwright/test';

const session = (id: string, title: string, number: number) => ({
  id,
  number,
  title,
  name: title,
  mode: 'chat',
  origin: 'web',
  created_at: 1_700_000_000,
  last_message_at: 1_700_000_001,
  pinned: false,
  archived: false,
  file_change_summary: { file_count: 1, adds: 2, dels: 1, git: true },
});

async function mockAPI(page: Page, options: { holdStream?: boolean; media?: boolean } = {}) {
  const requests: Array<{ method: string; url: string }> = [];
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    requests.push({ method: request.method(), url: path });
    const json = (value: unknown, status = 200) =>
      route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(value) });
    if (path.endsWith('/v1/capabilities'))
      return json({
        projects: { enabled: true },
        widgets: [{ id: 'status', name: 'Status', url: '../widgets/status/' }],
      });
    if (path.endsWith('/v1/providers'))
      return json({
        object: 'list',
        data: [
          {
            name: 'openai',
            configured: true,
            is_default: true,
            default_model: 'gpt-test',
            models: ['gpt-test'],
          },
        ],
      });
    if (path.endsWith('/v1/models'))
      return json({
        object: 'list',
        data: [
          { id: 'gpt-test', owned_by: 'openai', reasoning_efforts: ['low', 'medium', 'high'] },
        ],
      });
    if (path.endsWith('/v1/sidebar'))
      return json({ sessions: [session('s1', 'First chat', 1), session('s2', 'Second chat', 2)] });
    if (path.endsWith('/v1/sessions/status')) return json({ sessions: [] });
    if (path.endsWith('/v1/sessions') && url.searchParams.get('selected_only') === '1') {
      const id = url.searchParams.get('selected_session') || 's1';
      return json({
        selected_session: session(
          id,
          id === 's2' ? 'Second chat' : 'First chat',
          id === 's2' ? 2 : 1,
        ),
        selected_transcript: {
          bodies: {
            messages: [
              {
                id: 1,
                sequence: 0,
                role: 'user',
                parts: [
                  { type: 'text', text: id === 's2' ? 'Second question' : 'First question' },
                  ...(options.media
                    ? [
                        {
                          type: 'image',
                          filename: 'preview.png',
                          mime_type: 'image/png',
                          image_url:
                            'data:image/svg+xml,%3Csvg xmlns="http://www.w3.org/2000/svg" width="20" height="20"%3E%3Crect width="20" height="20" fill="blue"/%3E%3C/svg%3E',
                        },
                      ]
                    : []),
                ],
              },
            ],
          },
        },
      });
    }
    if (/\/v1\/sessions\/s[12]\/state$/.test(path))
      return json({
        session: session(
          path.includes('/s2/') ? 's2' : 's1',
          path.includes('/s2/') ? 'Second chat' : 'First chat',
          path.includes('/s2/') ? 2 : 1,
        ),
      });
    if (/\/v1\/sessions\/s[12]\/transcript$/.test(path))
      return json({
        messages: [
          {
            id: 1,
            sequence: 0,
            role: 'user',
            parts: [
              { type: 'text', text: path.includes('/s2/') ? 'Second question' : 'First question' },
            ],
          },
        ],
      });
    if (path.endsWith('/v1/responses') && request.method() === 'POST') {
      const body = options.holdStream
        ? 'event: response.created\ndata: {"response_id":"r1","run_epoch":1,"sequence_number":1}\n\n'
        : [
            'event: response.created\ndata: {"response_id":"r1","run_epoch":1,"sequence_number":1}\n\n',
            'event: response.output_text.delta\ndata: {"response_id":"r1","run_epoch":1,"sequence_number":2,"delta":"Streamed answer"}\n\n',
            'event: response.completed\ndata: {"response_id":"r1","run_epoch":1,"sequence_number":3,"usage":{"total_tokens":10}}\n\n',
          ].join('');
      return route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        headers: { 'x-response-id': 'r1', 'x-session-id': 's1' },
        body,
      });
    }
    if (path.endsWith('/v1/responses/r1') && request.method() === 'GET')
      return json({
        id: 'r1',
        session_id: 's1',
        run_epoch: 1,
        status: options.holdStream ? 'streaming' : 'completed',
        last_sequence_number: 1,
      });
    if (path.endsWith('/v1/responses/r1/events')) {
      if (options.holdStream) await new Promise((resolve) => setTimeout(resolve, 5_000));
      return route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        body: [
          'event: response.created\ndata: {"response_id":"r1","run_epoch":1,"sequence_number":1}\n\n',
          'event: response.output_text.delta\ndata: {"response_id":"r1","run_epoch":1,"sequence_number":2,"delta":"Streamed answer"}\n\n',
          'event: response.completed\ndata: {"response_id":"r1","run_epoch":1,"sequence_number":3,"usage":{"total_tokens":10}}\n\n',
        ].join(''),
      });
    }
    if (path.endsWith('/v1/responses/r1/cancel')) return json({ ok: true });
    if (path.includes('/file-changes/diff'))
      return json({ diff: '@@ -1,1 +1,2 @@\n-old\n+new\n+line' });
    if (path.includes('/file-changes'))
      return json({
        files: [{ path: 'frontend/src/main.tsx', additions: 2, deletions: 1, status: 'modified' }],
      });
    return json({});
  });
  return requests;
}

async function open(
  page: Page,
  suffix = '',
  options: { holdStream?: boolean; media?: boolean } = {},
) {
  const requests = await mockAPI(page, options);
  await page.goto(`./${suffix}`);
  await expect(page.locator('#startupSplash')).toBeHidden({ timeout: 10_000 });
  return requests;
}

test('loads, navigates sessions, opens settings and preserves normal namespace hygiene', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name === 'mobile', 'desktop session navigation is covered separately');
  await open(page);
  await expect(page.getByRole('heading', { name: 'Second chat' })).toBeVisible();
  await page.getByRole('button', { name: 'First chat', exact: true }).click();
  await expect(page.getByText('First question')).toBeVisible();
  await expect(page).toHaveURL(/\/chat\/1$/);
  await page.locator('#settingsBtn').click();
  const settings = page.getByRole('dialog', { name: 'Settings' });
  await expect(settings).toBeVisible();
  await settings.getByLabel('Provider').selectOption('openai');
  expect(
    await page.evaluate(() => ({
      legacy: 'TermLLMApp' in window,
      test: '__TERM_LLM_TEST__' in window,
    })),
  ).toEqual({ legacy: false, test: false });
});

test('desktop shell uses authored controls, message hierarchy and diff rows', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name === 'mobile', 'desktop visual structure');
  await open(page);
  const sessionButton = page.getByRole('button', { name: 'First chat', exact: true });
  await expect(sessionButton).toHaveCSS('display', 'flex');
  await expect(sessionButton).toHaveCSS('border-radius', '8px');
  await expect(page.locator('#newChatBtn svg')).toBeVisible();
  await expect(page.locator('#attachBtn svg')).toBeVisible();
  await expect(page.locator('.message-body')).toHaveCSS('border-radius', '12px');
  expect(
    await page
      .locator('.message-meta')
      .evaluate((element) => parseFloat(getComputedStyle(element).columnGap) > 0),
  ).toBe(true);

  await page.getByRole('button', { name: 'Toggle file changes' }).click();
  await expect(page.locator('#diffSidebar')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Change scope' })).toBeVisible();
  const diffRow = page.locator('.diff-file-row[data-path="frontend/src/main.tsx"]');
  await expect(diffRow).toHaveCSS('display', 'flex');
  await expect(diffRow).toHaveCSS('border-top-width', '0px');
  await diffRow.hover();
  await expect(
    page.getByRole('button', { name: 'Copy path frontend/src/main.tsx', exact: true }),
  ).toBeVisible();
});

test('collapses file-change counts to an icon at narrow widths', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name === 'mobile', 'narrow desktop breakpoint');
  await page.setViewportSize({ width: 850, height: 800 });
  await open(page);

  const toggle = page.getByRole('button', { name: 'Toggle file changes' });
  await expect(toggle).toHaveCSS('width', '34px');
  await expect(toggle.locator('.diff-toggle-file-icon')).toBeVisible();
  await expect(toggle.locator('.diff-toggle-stat-add')).toBeHidden();
  await expect(toggle.locator('.diff-toggle-stat-del')).toBeHidden();
  await expect(toggle.locator('.diff-toggle-file-count')).not.toHaveAttribute('data-file-count');
});

test('sends through public composer UI and reduces a streamed response', async ({ page }) => {
  const requests = await open(page);
  await page.getByRole('textbox', { name: 'Message' }).fill('Hello');
  await page.getByRole('button', { name: 'Send message' }).click();
  await expect(page.getByText('Streamed answer')).toBeVisible();
  expect(
    requests.some((entry) => entry.method === 'POST' && entry.url.endsWith('/v1/responses')),
  ).toBe(true);
});

test('cancels an active response from the public stop control', async ({ page }) => {
  const requests = await open(page, '', { holdStream: true });
  await page.getByRole('textbox', { name: 'Message' }).fill('Long request');
  await page.getByRole('button', { name: 'Send message' }).click();
  await page.locator('#stopBtn').click();
  await expect
    .poll(() =>
      requests.some(
        (entry) => entry.method === 'POST' && entry.url.endsWith('/v1/responses/r1/cancel'),
      ),
    )
    .toBe(true);
});

test('opens diff UI, expands a file and queues an inline comment', async ({ page }) => {
  await open(page);
  await page.getByRole('button', { name: 'Toggle file changes' }).click();
  expect(
    await page
      .locator('#diffSidebar')
      .evaluate((element) => getComputedStyle(element, '::before').content),
  ).toBe('none');
  const fileToggle = page.locator('.diff-file-row[data-path="frontend/src/main.tsx"]');
  await expect(fileToggle).toBeVisible();
  await fileToggle.click();
  await expect(page.locator('.diff-row.add .diff-code').first()).toHaveText('new');
  await page
    .getByRole('button', { name: /Comment on line 1/ })
    .first()
    .click();
  await page.getByRole('textbox', { name: 'Inline comment' }).fill('Please explain this');
  await page.getByRole('button', { name: 'More send options' }).click();
  await expect(page.getByRole('menuitem', { name: 'Send now' })).toBeFocused();
  await page.getByRole('menu', { name: 'Comment delivery' }).press('End');
  await expect(page.getByRole('menuitem', { name: /Queue comment/ })).toBeFocused();
  await page.getByRole('menuitem', { name: /Queue comment/ }).press('Enter');
  await expect(page.locator('.diff-queue-bar')).toContainText('1 queued');
  await expect(page.getByRole('button', { name: 'Send comments' })).toBeVisible();
  await expect(page.getByText('1 inline comment queued.')).toHaveCount(0);
});

test('mobile viewport opens a styled sidebar and returns to a usable composer', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'mobile', 'mobile-only interaction');
  await open(page);
  const diffToggle = page.getByRole('button', { name: 'Toggle file changes' });
  await expect(diffToggle.locator('.diff-toggle-file-icon')).toBeVisible();
  await expect(diffToggle.locator('.diff-toggle-badge')).toHaveCSS('position', 'static');
  await page.getByRole('button', { name: 'Open sidebar' }).click();
  const sidebar = page.locator('#sidebar');
  await expect(sidebar).toHaveClass(/open/);
  await expect(sidebar).toHaveCSS('visibility', 'visible');
  await expect(page.locator('#newChatBtn')).toHaveCSS('display', 'flex');
  await expect(page.locator('#newChatBtn svg')).toBeVisible();
  expect(await page.locator('#appMain').evaluate((element) => (element as HTMLElement).inert)).toBe(
    true,
  );
  await expect(sidebar).toBeFocused();
  await expect(sidebar).toHaveCSS('outline-style', 'none');
  await page.mouse.click(400, 100);
  await expect(sidebar).not.toHaveClass(/open/);
  expect(await page.locator('#appMain').evaluate((element) => (element as HTMLElement).inert)).toBe(
    false,
  );
  await page.getByRole('button', { name: 'Open sidebar' }).click();
  await expect(sidebar).toBeFocused();
  await page.locator('#newChatBtn').click();
  await expect(sidebar).not.toHaveClass(/open/);
  expect(await page.locator('#appMain').evaluate((element) => (element as HTMLElement).inert)).toBe(
    false,
  );
  await expect(page.getByRole('textbox', { name: 'Message' })).toBeVisible();
  await expect(page.locator('.composer-box')).toHaveCSS('border-radius', '22px');
  await expect(page.locator('#sendBtn svg')).toBeVisible();
});

test('mobile floating surfaces dismiss from their outside area and restore interaction', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'mobile', 'mobile-only interaction');
  await open(page);

  const runtimeTrigger = page.getByRole('button', { name: 'Runtime settings' });
  await runtimeTrigger.click();
  const runtime = page.getByRole('dialog', { name: 'Runtime settings' });
  await expect(runtime).toBeVisible();
  await expect(page.getByRole('button', { name: 'Close runtime settings' })).toBeFocused();
  await expect(page.getByRole('combobox', { name: 'Provider' })).not.toBeFocused();
  await expect(runtime).toHaveCSS('border-radius', '18px');
  await expect(page.getByRole('combobox', { name: 'Runtime model' })).toHaveCSS(
    'min-height',
    '48px',
  );
  await page.mouse.click(4, 180);
  await expect(runtime).toBeHidden();
  await expect(runtimeTrigger).toBeFocused();

  await page.getByRole('button', { name: 'Toggle file changes' }).click();
  const changes = page.getByRole('dialog', { name: 'Session file changes' });
  await expect(changes).toBeVisible();
  expect(await page.locator('#appMain').evaluate((element) => (element as HTMLElement).inert)).toBe(
    true,
  );
  await page.mouse.click(4, 180);
  await expect(changes).toBeHidden();
  expect(await page.locator('#appMain').evaluate((element) => (element as HTMLElement).inert)).toBe(
    false,
  );
});

test('same-context tabs retain independent session drafts', async ({ context, page }, testInfo) => {
  test.skip(
    testInfo.project.name === 'mobile',
    'multi-tab storage is covered once in desktop Chromium',
  );
  const second = await context.newPage();
  await Promise.all([mockAPI(page), mockAPI(second)]);
  await Promise.all([page.goto('./'), second.goto('./')]);
  await Promise.all([
    expect(page.locator('#startupSplash')).toBeHidden({ timeout: 10_000 }),
    expect(second.locator('#startupSplash')).toBeHidden({ timeout: 10_000 }),
  ]);

  await page.getByRole('button', { name: 'First chat', exact: true }).click();
  await page.getByRole('textbox', { name: 'Message' }).fill('draft in first');
  await page.getByRole('button', { name: 'Second chat', exact: true }).click();

  await second.getByRole('textbox', { name: 'Message' }).fill('draft in second');
  await second.getByRole('button', { name: 'First chat', exact: true }).click();

  await page.getByRole('button', { name: 'First chat', exact: true }).click();
  await expect(page.getByRole('textbox', { name: 'Message' })).toHaveValue('draft in first');
  await second.getByRole('button', { name: 'Second chat', exact: true }).click();
  await expect(second.getByRole('textbox', { name: 'Message' })).toHaveValue('draft in second');
  await second.close();
});

test('lightbox Escape restores focus to the media trigger', async ({ page }) => {
  await open(page, '', { media: true });
  const trigger = page.getByRole('button', { name: 'preview.png' });
  await trigger.click();
  const dialog = page.getByRole('dialog', { name: 'Media preview' });
  await expect(dialog).toBeVisible();
  await dialog.press('Escape');
  await expect(dialog).toBeHidden();
  await expect(trigger).toBeFocused();
});

test('production build does not expose the browser-test bridge', async ({ page }) => {
  await open(page, '?test_bridge=1');
  expect(await page.evaluate(() => Boolean(window.__TERM_LLM_TEST__))).toBe(false);
});
