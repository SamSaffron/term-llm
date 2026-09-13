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

async function mockAPI(
  page: Page,
  options: {
    holdStream?: boolean;
    media?: boolean;
    model?: string;
    attention?: boolean;
    shell?: boolean;
  } = {},
) {
  const requests: Array<{ method: string; url: string }> = [];
  const model = options.model || 'gpt-test';
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
        shell: { enabled: options.shell === true, version: 1, transport: 'http_sse' },
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
            default_model: model,
            models: [model],
          },
        ],
      });
    if (path.endsWith('/v1/models'))
      return json({
        object: 'list',
        data: [{ id: model, owned_by: 'openai', reasoning_efforts: ['low', 'medium', 'high'] }],
      });
    if (path.endsWith('/v1/sidebar')) {
      const sessions = [session('s1', 'First chat', 1), session('s2', 'Second chat', 2)];
      if (options.attention)
        Object.assign(sessions[0], {
          attention_store_instance_id: 'store-a',
          attention_seq: 42,
          attention_response_id: 'response-a',
          attention_final_rev: 7,
          seen_through_seq: 0,
          attention_unseen: true,
          attention_outcome: 'completed',
        });
      return json({ sessions, recent_sessions: sessions });
    }
    if (path.endsWith('/v1/sessions/status')) return json({ sessions: [] });
    if (path.endsWith('/v1/sessions') && url.searchParams.get('selected_only') === '1') {
      const id = url.searchParams.get('selected_session') || 's1';
      const selected = session(id, id === 's2' ? 'Second chat' : 'First chat', id === 's2' ? 2 : 1);
      if (options.attention && id === 's1')
        Object.assign(selected, {
          attention_store_instance_id: 'store-a',
          attention_seq: 42,
          attention_response_id: 'response-a',
          attention_final_rev: 7,
          seen_through_seq: 0,
          attention_unseen: true,
          attention_outcome: 'completed',
        });
      return json({
        selected_session: selected,
        selected_transcript: {
          bodies: {
            ...(options.attention && id === 's1' ? { rev: 7 } : {}),
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
    if (options.shell && /\/v1\/sessions\/[^/]+\/shell$/.test(path) && request.method() === 'POST')
      return json(
        { shell_id: 'sh_browser', cwd: '/workspace/project', created: true, state: 'running' },
        201,
      );
    if (options.shell && /\/v1\/sessions\/[^/]+\/shell\/stream$/.test(path))
      return route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        body: [
          'event: ready\ndata: {"shell_id":"sh_browser","offset":0,"base_offset":0,"next_offset":0}\n\n',
          'event: output\ndata: {"offset":0,"next_offset":5,"data":"aGVsbG8="}\n\n',
          'event: exit\ndata: {"offset":5,"exit_code":0}\n\n',
        ].join(''),
      });
    if (options.shell && /\/v1\/sessions\/[^/]+\/shell\/(?:input|resize)$/.test(path))
      return request.method() === 'POST' ? json({ accepted: 1 }) : json({});
    if (
      options.shell &&
      /\/v1\/sessions\/[^/]+\/shell$/.test(path) &&
      request.method() === 'DELETE'
    )
      return route.fulfill({ status: 204, body: '' });
    if (/\/v1\/sessions\/s1\/attention\/seen$/.test(path) && request.method() === 'POST')
      return json({
        store_instance_id: 'store-a',
        latest_attention_seq: 42,
        seen_through_seq: 42,
        attention_unseen: false,
      });
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
        headers: {
          'x-response-id': 'r1',
          'x-session-id': 's1',
          'x-term-llm-response-status': 'in_progress',
        },
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
  options: {
    holdStream?: boolean;
    media?: boolean;
    model?: string;
    attention?: boolean;
    shell?: boolean;
  } = {},
) {
  const requests = await mockAPI(page, options);
  await page.goto(`./${suffix}`);
  await expect(page.locator('#startupSplash')).toBeHidden({ timeout: 10_000 });
  return requests;
}

test('lazy-loads the capability-gated interactive shell overlay', async ({ page }) => {
  const requests = await open(page, '', { shell: true });
  const composer = page.getByRole('textbox', { name: 'Message' });
  await composer.fill('/shell');
  await composer.press('Enter');

  await expect(page.getByRole('region', { name: 'Interactive shell' })).toBeVisible();
  await expect(page.getByText('/workspace/project')).toBeVisible();
  await expect(page.getByText('Exited (0)')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Restart' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Back to chat' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'End shell' })).toBeVisible();

  await page.evaluate(() => {
    history.pushState({}, '', new URL('/ui/chat/1', location.origin));
    dispatchEvent(new PopStateEvent('popstate'));
  });
  await expect(page.getByRole('region', { name: 'Interactive shell' })).toBeHidden();
  await expect(page.getByText('First question')).toBeVisible();

  await composer.fill('/shell');
  await composer.press('Enter');
  await expect(page.getByRole('region', { name: 'Interactive shell' })).toBeVisible();
  await page.getByRole('button', { name: 'Back to chat' }).click();
  await expect(page.getByRole('region', { name: 'Interactive shell' })).toBeHidden();

  const returnToShell = page.getByRole('button', { name: 'Return to shell' });
  await expect(returnToShell).toHaveText('Shell');
  await returnToShell.click();
  await expect(page.getByRole('region', { name: 'Interactive shell' })).toBeVisible();
  await page.evaluate(() => {
    history.pushState({}, '', new URL('/ui/', location.origin));
    dispatchEvent(new PopStateEvent('popstate'));
  });
  await expect(page.getByRole('region', { name: 'Interactive shell' })).toBeHidden();
  expect(requests.some((entry) => /\/v1\/sessions\/[^/]+\/shell$/.test(entry.url))).toBe(true);
  expect(requests.some((entry) => /\/v1\/sessions\/[^/]+\/shell\/stream$/.test(entry.url))).toBe(
    true,
  );
});

test('shell terminal fills the viewport and follows height changes', async ({ page }) => {
  await open(page, '', { shell: true });
  const composer = page.getByRole('textbox', { name: 'Message' });
  await composer.fill('/shell');
  await composer.press('Enter');

  const overlay = page.getByRole('region', { name: 'Interactive shell' });
  const terminalHost = overlay.locator('.shell-terminal');
  const terminalScreen = terminalHost.locator('.xterm-screen');
  await expect(terminalScreen).toBeVisible();

  const viewport = page.viewportSize();
  expect(viewport).not.toBeNull();
  const initialHostHeight = await terminalHost.evaluate(
    (element) => element.getBoundingClientRect().height,
  );
  expect(initialHostHeight).toBeGreaterThan(viewport!.height * 0.7);
  const initialScreenHeight = await terminalScreen.evaluate(
    (element) => element.getBoundingClientRect().height,
  );

  await page.setViewportSize({ width: viewport!.width, height: viewport!.height - 200 });
  await expect
    .poll(() => terminalScreen.evaluate((element) => element.getBoundingClientRect().height))
    .toBeLessThan(initialScreenHeight - 100);
  const shrunkenScreenHeight = await terminalScreen.evaluate(
    (element) => element.getBoundingClientRect().height,
  );

  await page.setViewportSize(viewport!);
  await expect
    .poll(() => terminalScreen.evaluate((element) => element.getBoundingClientRect().height))
    .toBeGreaterThan(shrunkenScreenHeight + 100);
  const bounds = await terminalHost.evaluate((host) => {
    const hostRect = host.getBoundingClientRect();
    const screenRect = host.querySelector('.xterm-screen')!.getBoundingClientRect();
    return { hostBottom: hostRect.bottom, screenBottom: screenRect.bottom };
  });
  expect(bounds.screenBottom).toBeLessThanOrEqual(bounds.hostBottom);
});

test('loads, navigates sessions, opens settings and preserves normal namespace hygiene', async ({
  page,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop',
    'desktop session navigation is covered separately',
  );
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

test('turns a durable completion indicator off only after a visible revision-gated visit', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop sidebar attention flow');
  const requests = await open(page, '', { attention: true });
  const ready = page.getByRole('button', {
    name: 'First chat — Completed, not yet visited',
  });
  await expect(ready).toBeVisible();

  await ready.click();

  await expect
    .poll(() =>
      requests.some(
        (request) =>
          request.method === 'POST' && request.url.endsWith('/v1/sessions/s1/attention/seen'),
      ),
    )
    .toBe(true);
  await expect(page.getByRole('button', { name: 'First chat', exact: true })).toBeVisible();
});

test('desktop shell uses authored controls, message hierarchy and diff rows', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop visual structure');
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

  const diffToggle = page.getByRole('button', { name: 'Toggle file changes' });
  await diffToggle.click();
  await expect(diffToggle).toHaveCSS('width', '34px');
  await expect(diffToggle.locator('.diff-toggle-file-icon')).toBeVisible();
  await expect(diffToggle.locator('.diff-toggle-stat-add')).toBeHidden();
  await expect(diffToggle.locator('.diff-toggle-stat-del')).toBeHidden();
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
  test.skip(testInfo.project.name !== 'desktop', 'narrow desktop breakpoint');
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

test('keeps running controls while a response transport reconnects', async ({ page }) => {
  const requests = await open(page, '', { holdStream: true });
  await page.getByRole('textbox', { name: 'Message' }).fill('Long request');
  await page.getByRole('button', { name: 'Send message' }).click();
  await expect(page.locator('#stopBtn')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Response is running' })).toBeEnabled();
  await expect(page.getByPlaceholder('Steer conversation…')).toBeVisible();
  await expect(page.getByRole('status', { name: 'Response status is unknown' })).toBeHidden();
  expect(
    requests.some(
      (entry) => entry.method === 'POST' && entry.url.endsWith('/v1/responses/r1/cancel'),
    ),
  ).toBe(false);
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

test('mobile header keeps the active runtime legible without overflowing when controls compete', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'mobile', 'mobile-only layout');
  await open(page, '', { model: 'gpt-5.6-sol-high' });

  const runtime = page.getByRole('button', { name: /Runtime settings: gpt-5\.6-sol, high effort/ });
  const label = runtime.locator('.chip-label');
  await expect(label).toHaveText('gpt-5.6-sol');
  expect(await label.evaluate((element) => element.scrollWidth <= element.clientWidth + 1)).toBe(
    true,
  );
  expect(
    await page
      .locator('.main-header')
      .evaluate((element) => element.scrollWidth <= element.clientWidth + 1),
  ).toBe(true);

  await runtime.click();
  await expect(page.getByRole('dialog', { name: 'Runtime settings' })).toBeVisible();
});

test('mobile viewport opens a styled sidebar and returns to a usable composer', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'mobile', 'mobile-only interaction');
  await open(page, '', { model: 'gpt-5.6-sol-high' });
  await page.evaluate(() => document.documentElement.style.setProperty('--safe-top', '59px'));
  const titleContext = page.locator('.header-title-context');
  expect((await titleContext.boundingBox())?.width || 0).toBeGreaterThan(140);
  const diffToggle = page.getByRole('button', { name: 'Toggle file changes' });
  const mobileMenu = page.getByRole('button', { name: 'Open sidebar' });
  await expect(diffToggle.locator('.diff-toggle-file-icon')).toBeVisible();
  await expect(diffToggle.locator('.diff-toggle-badge')).toHaveCSS('position', 'static');
  await mobileMenu.click();
  const sidebar = page.locator('#sidebar');
  await expect(sidebar).toHaveClass(/open/);
  await expect(sidebar).toHaveCSS('visibility', 'visible');
  await expect(sidebar).toHaveCSS('padding-top', '0px');
  const [sidebarBox, sidebarHeaderBox] = await Promise.all([
    sidebar.boundingBox(),
    sidebar.locator('.sidebar-header').boundingBox(),
  ]);
  expect(sidebarHeaderBox?.y).toBe(sidebarBox?.y);
  await expect(sidebar.locator('.sidebar-header')).toHaveCSS('padding-top', '72.6px');
  await expect(page.locator('#newChatBtn')).toHaveCSS('display', 'flex');
  await expect(page.locator('#newChatBtn svg')).toBeVisible();
  expect(await page.locator('#appMain').evaluate((element) => (element as HTMLElement).inert)).toBe(
    true,
  );
  await expect(sidebar).toBeFocused();
  await expect(sidebar).toHaveCSS('outline-style', 'none');
  await page.mouse.click(400, 100);
  await expect(sidebar).not.toHaveClass(/open/);
  await expect(mobileMenu).not.toBeFocused();
  expect(await page.locator('#appMain').evaluate((element) => (element as HTMLElement).inert)).toBe(
    false,
  );
  await mobileMenu.click();
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
  const runtimeClose = page.getByRole('button', { name: 'Close runtime settings' });
  await runtimeTrigger.click();
  const runtime = page.getByRole('dialog', { name: 'Runtime settings' });
  await expect(runtime).toBeVisible();
  await expect(runtimeClose).toBeFocused();
  await expect(page.getByRole('combobox', { name: 'Provider' })).not.toBeFocused();
  await expect(runtime).toHaveCSS('border-radius', '18px');
  await expect(page.getByRole('combobox', { name: 'Runtime model' })).toHaveCSS(
    'min-height',
    '48px',
  );
  await page.mouse.click(4, 180);
  await expect(runtime).toBeHidden();
  await expect(runtimeTrigger).not.toBeFocused();
  await expect(runtimeTrigger).toHaveCSS('box-shadow', 'none');

  await runtimeTrigger.click();
  await expect(runtimeClose).toBeFocused();
  await runtimeClose.click();
  await expect(runtime).toBeHidden();
  await expect(runtimeTrigger).not.toBeFocused();
  await expect(runtimeTrigger).toHaveCSS('box-shadow', 'none');

  // Keyboard dismissal still restores focus, using a rounded ring without WebKit artifacts.
  await runtimeTrigger.press('Enter');
  await expect(runtimeClose).toBeFocused();
  await runtimeClose.press('Enter');
  await expect(runtime).toBeHidden();
  await expect(runtimeTrigger).toBeFocused();
  await expect(runtimeTrigger).toHaveCSS('outline-style', 'none');
  await expect(runtimeTrigger).not.toHaveCSS('box-shadow', 'none');

  await page.getByRole('button', { name: 'Toggle file changes' }).click();
  const changes = page.getByRole('dialog', { name: 'Session file changes' });
  await expect(changes).toBeVisible();
  expect(await page.locator('#appMain').evaluate((element) => (element as HTMLElement).inert)).toBe(
    true,
  );
  await page.mouse.click(4, 180);
  await expect(changes).toBeHidden();
  await expect(page.getByRole('button', { name: 'Toggle file changes' })).not.toBeFocused();
  expect(await page.locator('#appMain').evaluate((element) => (element as HTMLElement).inert)).toBe(
    false,
  );
});

test('same-context tabs retain independent session drafts', async ({ context, page }, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop',
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

test('lightbox mouse wheel zooms at the cursor and displays the current percentage', async ({
  page,
  isMobile,
}) => {
  test.skip(isMobile, 'Desktop wheel deltas; mobile uses the multi-touch test below');
  await open(page, '', { media: true });
  await page.getByRole('button', { name: 'preview.png' }).click();
  const image = page.getByRole('dialog', { name: 'Media preview' }).getByRole('img');
  const readout = page.getByRole('button', { name: 'Reset zoom' });
  await expect(readout).toHaveText('100%');
  const initial = (await image.boundingBox())!;
  const cursor = { x: initial.x + initial.width / 4, y: initial.y + initial.height / 4 };
  await page.mouse.move(cursor.x, cursor.y);
  await page.mouse.wheel(0, -300);
  await expect(readout).toHaveText('200%');
  const box = (await image.boundingBox())!;
  expect(box.width / initial.width).toBeCloseTo(2);
  expect(Math.abs(box.x + box.width / 4 - cursor.x)).toBeLessThan(1);
  expect(Math.abs(box.y + box.height / 4 - cursor.y)).toBeLessThan(1);
  await page.mouse.wheel(0, 300);
  await expect(readout).toHaveText('100%');
  await page.mouse.wheel(0, -10000);
  await expect(readout).toHaveText('3200%');
  expect(await page.evaluate(() => window.visualViewport!.scale)).toBe(1);
  await readout.click();
  await expect(readout).toHaveText('100%');
});

test('mobile lightbox pinches to 32× without focal drift or browser zoom', async ({
  page,
  isMobile,
  browserName,
}) => {
  test.skip(!isMobile || browserName !== 'chromium', 'Exercises Chromium CDP multi-touch input');
  await open(page, '', { media: true });
  await page.getByRole('button', { name: 'preview.png' }).click();
  const image = page.getByRole('dialog', { name: 'Media preview' }).getByRole('img');
  await image.evaluate(async (node: HTMLImageElement) => {
    node.src = `data:image/svg+xml,${encodeURIComponent('<svg xmlns="http://www.w3.org/2000/svg" width="320" height="240"><rect width="320" height="240" fill="blue"/></svg>')}`;
    await node.decode();
  });
  const initial = (await image.boundingBox())!;
  const midpoint = {
    x: initial.x + initial.width / 2 - 20,
    y: initial.y + initial.height / 2 - 10,
  };
  const focal = {
    x: (midpoint.x - initial.x) / initial.width,
    y: (midpoint.y - initial.y) / initial.height,
  };
  const cdp = await page.context().newCDPSession(page);
  for (let step = 1; step <= 5; step++) {
    await cdp.send('Input.dispatchTouchEvent', {
      type: 'touchStart',
      touchPoints: [
        { id: 1, x: midpoint.x - 20, y: midpoint.y },
        { id: 2, x: midpoint.x + 20, y: midpoint.y },
      ],
    });
    await cdp.send('Input.dispatchTouchEvent', {
      type: 'touchMove',
      touchPoints: [
        { id: 1, x: midpoint.x - 40, y: midpoint.y },
        { id: 2, x: midpoint.x + 40, y: midpoint.y },
      ],
    });
    await expect
      .poll(async () => (await image.boundingBox())!.width / initial.width)
      .toBeCloseTo(2 ** step, 2);
    await expect(page.getByRole('button', { name: 'Reset zoom' })).toHaveText(
      `${2 ** step * 100}%`,
    );
    const box = (await image.boundingBox())!;
    expect(Math.abs(box.x + box.width * focal.x - midpoint.x)).toBeLessThan(1);
    expect(Math.abs(box.y + box.height * focal.y - midpoint.y)).toBeLessThan(1);
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  }
  await expect(page.getByRole('button', { name: 'Zoom in', exact: true })).toBeDisabled();
  expect(await image.evaluate((node) => getComputedStyle(node).transitionDuration)).toBe('0s');
  expect(await image.evaluate((node) => getComputedStyle(node.parentElement!).touchAction)).toBe(
    'none',
  );
  expect(await page.evaluate(() => window.visualViewport!.scale)).toBe(1);
  await page.getByRole('button', { name: 'Reset zoom' }).click();
  await expect.poll(async () => (await image.boundingBox())!.width).toBeCloseTo(initial.width, 2);
  await cdp.detach();
});

test('lightbox loads its module and stylesheet only when opened', async ({ page }) => {
  const assets: string[] = [];
  page.on('request', (request) => {
    if (/\/(?:chunks|assets)\/Lightbox\.(?:js|css)/.test(request.url())) assets.push(request.url());
  });
  await open(page, '', { media: true });
  expect(assets).toEqual([]);
  await page.getByRole('button', { name: 'preview.png' }).click();
  await expect(page.getByRole('dialog', { name: 'Media preview' }).getByRole('img')).toBeVisible();
  expect(assets.some((url) => url.includes('/chunks/Lightbox.js'))).toBe(true);
  expect(assets.some((url) => url.includes('/assets/Lightbox.css'))).toBe(true);
});

test.describe('lightbox chunk failures and handoff', () => {
  // Fault injection must control module fetches rather than service-worker subrequests.
  test.use({ serviceWorkers: 'block' });

  test('lightbox keeps focus through a delayed module handoff and restores the trigger', async ({
    page,
  }) => {
    let release!: () => void;
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    await page.route('**/chunks/Lightbox.js*', async (route) => {
      await held;
      await route.continue();
    });
    await open(page, '', { media: true });
    const trigger = page.getByRole('button', { name: 'preview.png' });
    await trigger.click();
    try {
      const placeholder = page.getByRole('dialog', { name: 'Media preview' });
      await expect(placeholder.getByRole('button', { name: 'Close Media preview' })).toBeFocused();
    } finally {
      release();
    }
    const viewer = page.getByRole('dialog', { name: 'Media preview' });
    await expect(viewer.getByRole('img')).toBeVisible();
    await expect(viewer).toBeFocused();
    await viewer.press('Escape');
    await expect(trigger).toBeFocused();
  });

  test('lightbox offers a reload and original media when its module cannot load', async ({
    page,
  }) => {
    await page.route('**/chunks/Lightbox.js*', (route) => route.abort('failed'));
    await open(page, '', { media: true });
    await page.getByRole('button', { name: 'preview.png' }).click();
    const dialog = page.getByRole('dialog', { name: 'Media preview' });
    await expect(dialog.getByRole('alert')).toContainText('Could not load the image viewer');
    await expect(dialog.getByRole('button', { name: 'Reload page' })).toBeVisible();
    await expect(dialog.getByRole('link', { name: 'Open original' })).toHaveAttribute(
      'href',
      /^data:image/,
    );
    await dialog.press('Escape');
    await expect(page.getByRole('button', { name: 'preview.png' })).toBeFocused();
  });

  test('lightbox can be dismissed while its module is loading', async ({ page }) => {
    let release!: () => void;
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    await page.route('**/chunks/Lightbox.js*', async (route) => {
      await held;
      await route.continue();
    });
    await open(page, '', { media: true });
    const trigger = page.getByRole('button', { name: 'preview.png' });
    try {
      await trigger.click();
      await expect(page.getByRole('status')).toHaveText('Loading image viewer…');
      await page.getByRole('dialog', { name: 'Media preview' }).press('Escape');
      await expect(trigger).toBeFocused();
    } finally {
      release();
    }
    await expect(page.getByRole('dialog', { name: 'Media preview' })).toHaveCount(0);
  });
});

test('lightbox iPhone controls fit the screen and copying shows visible feedback', async ({
  page,
  isMobile,
}, testInfo) => {
  test.skip(!isMobile, 'Mobile layout and safe-area coverage');
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText: async () => undefined },
    });
  });
  await open(page, '', { media: true });
  await page.getByRole('button', { name: 'preview.png' }).click();
  const dialog = page.getByRole('dialog', { name: 'Media preview' });
  const image = dialog.getByRole('img');
  await expect(image).toBeVisible();
  if (process.env.LIGHTBOX_PREVIEW_IMAGE) {
    await page.route('**/lightbox-preview.png', (route) =>
      route.fulfill({ path: process.env.LIGHTBOX_PREVIEW_IMAGE!, contentType: 'image/png' }),
    );
    await image.evaluate(async (node: HTMLImageElement) => {
      node.src = '/lightbox-preview.png';
      await node.decode();
    });
  }
  // Model the notch/home-indicator insets even in a headless browser without device chrome.
  await dialog.evaluate((node) => {
    node.style.setProperty('--safe-top', '47px');
    node.style.setProperty('--safe-bottom', '34px');
  });
  await expect(dialog.getByRole('button', { name: 'Previous media' })).toHaveCount(0);
  await expect(dialog.getByRole('button', { name: 'Next media' })).toHaveCount(0);
  const assertFits = async () => {
    const width = page.viewportSize()!.width;
    const buttons = dialog.locator('.lightbox-toolbar button, .lightbox-toolbar a');
    for (const button of await buttons.all()) {
      const box = (await button.boundingBox())!;
      expect(box.x).toBeGreaterThanOrEqual(12);
      expect(box.x + box.width).toBeLessThanOrEqual(width - 12);
      expect(box.width).toBeGreaterThanOrEqual(44);
      expect(box.height).toBeGreaterThanOrEqual(44);
    }
  };
  await assertFits();
  for (let i = 0; i < 2; i++)
    await dialog.getByRole('button', { name: 'Zoom in', exact: true }).click();
  await expect(dialog.getByRole('button', { name: 'Reset zoom' })).toHaveText('400%');
  const screenshot = (name: string) =>
    page.screenshot({
      path: process.env.LIGHTBOX_SCREENSHOT_DIR
        ? `${process.env.LIGHTBOX_SCREENSHOT_DIR}/${testInfo.project.name}-${name}.png`
        : testInfo.outputPath(`${name}.png`),
    });
  await screenshot('zoomed');
  await dialog.getByRole('button', { name: 'Copy URL' }).click();
  await expect(dialog.getByRole('status')).toHaveText('Copied');
  await screenshot('copied');
  await expect(dialog.getByRole('button', { name: 'Copy URL' })).toBeVisible();
  await page.setViewportSize({ width: 320, height: 568 });
  await assertFits();
});

test('production build does not expose the browser-test bridge', async ({ page }) => {
  await open(page, '?test_bridge=1');
  expect(await page.evaluate(() => Boolean(window.__TERM_LLM_TEST__))).toBe(false);
});

test('queues separate steering, removes a selected row, and rushes without losing a new draft', async ({
  page,
}) => {
  await mockAPI(page, { holdStream: true });
  let pending: Array<{ id: string; text: string; status: string }> = [];
  const rushed: Record<string, unknown>[] = [];
  const removed: string[] = [];
  let operation: Record<string, unknown> | null = null;
  let stops = 0;
  await page.route('**/v1/responses/r1/events*', (route) =>
    route.fulfill({
      contentType: 'text/event-stream',
      body: 'event: response.created\ndata: {"response_id":"r1","run_epoch":1,"sequence_number":1}\n\n',
    }),
  );
  await page.route('**/v1/sessions/s1/state', (route) =>
    route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({
        steering: {
          protocol: 1,
          can_steer: !operation,
          can_rush: !operation && pending.length > 0,
          ...(!pending.length ? { unavailable_reason: 'no_user_steering' } : {}),
        },
        pending_steering: pending,
        ...(operation ? { active_rush: operation } : {}),
      }),
    }),
  );
  await page.route('**/v1/sessions/s1/steering**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const json = (body: unknown, status = 200) =>
      route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });
    expect(request.headers()['x-term-llm-steering-protocol']).toBe('1');
    if (url.pathname.endsWith('/rush') && request.method() === 'POST') {
      const body = request.postDataJSON() as Record<string, unknown>;
      rushed.push(body);
      expect(body.expected_response_id).toBe('r1');
      expect(body.expected_run_epoch).toBe(1);
      expect(Object.keys(body).sort()).toEqual([
        'expected_response_id',
        'expected_run_epoch',
        'request_id',
      ]);
      operation = {
        rush_id: body.request_id,
        session_id: 's1',
        source_response_id: 'r1',
        source_run_epoch: 1,
        status: 'interrupting',
        revision: 1,
        steering_ids: pending.map((entry) => entry.id),
      };
      return json(operation, 202);
    }
    if (url.pathname.endsWith('/cancel')) {
      stops++;
      operation = { ...operation, status: 'cancelled', revision: 2 };
      return json(operation);
    }
    if (url.pathname.includes('/rush/')) return json(operation);
    if (request.method() === 'DELETE') {
      const id = url.pathname.split('/').at(-1)!;
      removed.push(id);
      expect(url.searchParams.get('expected_response_id')).toBe('r1');
      expect(url.searchParams.get('expected_run_epoch')).toBe('1');
      pending = pending.filter((entry) => entry.id !== id);
      return json({ cancelled: true });
    }
    const body = request.postDataJSON() as Record<string, unknown>;
    expect(body.delivery).toBe('steer');
    expect(body.expected_response_id).toBe('r1');
    pending.push({
      id: String(body.client_message_id),
      text: String(body.message),
      status: 'queued',
    });
    return json({ action: 'steer', steering_id: body.client_message_id });
  });
  await page.goto('./chat/s1');
  await expect(page.locator('#startupSplash')).toBeHidden({ timeout: 10_000 });
  const input = page.locator('#promptInput');
  await input.fill('Begin');
  await input.press('Enter');
  await expect(page.getByPlaceholder('Steer conversation…')).toBeVisible();
  for (const text of ['first guidance', 'remove this']) {
    await input.fill(text);
    await input.press('Enter');
    await expect(
      page.locator('.pending-steering-text').getByText(text, { exact: true }),
    ).toBeVisible();
    await expect(input).toHaveValue('');
  }
  await input.press('ArrowUp');
  await input.press('Delete');
  await expect(page.locator('.pending-steering-text')).toHaveCount(1);
  expect(removed).toHaveLength(1);
  await input.fill('second guidance');
  await input.press('Enter');
  await expect(page.locator('.pending-steering-text')).toHaveCount(2);
  await expect(
    page
      .locator('.pending-steering-row')
      .filter({ hasText: 'second guidance' })
      .getByRole('button', { name: 'Steer all now', exact: true }),
  ).toBeVisible();
  await expect(page.getByText(/Esc to steer|↑ to select|Delete to remove/)).toHaveCount(0);
  await input.fill('new unsent draft');
  await input.press('Escape');
  await expect(page.getByText('Interrupting…', { exact: true })).toBeVisible();
  await input.press('Escape');
  expect(rushed).toHaveLength(1);
  expect(stops).toBe(0);
  await expect(input).toHaveValue('new unsent draft');
  await page.locator('#stopBtn').click();
  await expect.poll(() => stops).toBe(1);
  await expect(input).toHaveValue('new unsent draft');
  expect(pending.map((entry) => entry.text)).toEqual(['first guidance', 'second guidance']);
});

test('infinitely loads older transcript turns and preserves the visible row', async ({ page }) => {
  await mockAPI(page);
  const messages = Array.from({ length: 200 }, (_, index) => ({
    id: index + 1,
    sequence: index,
    role: index % 2 === 0 ? 'user' : 'assistant',
    parts: [{ type: 'text', text: `History message ${index}` }],
  }));
  const pages: number[][] = [];
  await page.route('**/v1/sessions?**', async (route) => {
    const url = new URL(route.request().url());
    if (url.searchParams.get('selected_only') !== '1') return route.fallback();
    await route.fulfill({
      json: {
        selected_session: session('s1', 'Long history', 1),
        selected_transcript: {
          index: { rev: 7, rows: { ids: messages.map((row) => row.id), roles: 'ua'.repeat(100) } },
          bodies: { rev: 7, messages: messages.slice(-18) },
        },
      },
    });
  });
  await page.route('**/v1/sessions/s1/transcript/bodies?**', async (route) => {
    const anchors = new URL(route.request().url()).searchParams.get('ids')!.split(',').map(Number);
    pages.push(anchors);
    await route.fulfill({
      json: {
        rev: 7,
        messages: messages.filter((row) =>
          anchors.includes(row.role === 'user' ? row.id : row.id - 1),
        ),
      },
    });
  });
  await page.goto('./chat/s1');
  await expect(page.getByText('History message 199', { exact: true })).toBeVisible();
  expect(pages).toHaveLength(0);
  const viewport = page.locator('#chatScroll');
  const anchor = page.locator('[data-message-id="srv_seq_182"]');
  const initialTop = await viewport.evaluate((element) => {
    element.scrollTop = 0;
    return element.querySelector('[data-message-id="srv_seq_182"]')!.getBoundingClientRect().top;
  });
  await expect(page.getByText('History message 164', { exact: true })).toBeAttached();
  await expect
    .poll(async () => Math.abs((await anchor.boundingBox())!.y - initialTop))
    .toBeLessThan(3);
  for (let pageNumber = 1; pageNumber < 11; pageNumber++) {
    await viewport.evaluate((element) => {
      element.scrollTop = 0;
    });
    await expect.poll(() => pages.length).toBe(pageNumber + 1);
    await expect(page.getByRole('button', { name: 'Loading earlier messages…' })).toHaveCount(0);
  }
  await viewport.evaluate((element) => {
    element.scrollTop = 0;
  });
  await expect(page.getByText('History message 0', { exact: true })).toBeVisible();
  await expect(
    page.getByRole('button', { name: 'Load earlier messages', exact: true }),
  ).toHaveCount(0);
  expect(pages.flat()).toHaveLength(91);
  expect(new Set(pages.flat()).size).toBe(91);
});

test('opens complete scrollable web stats without sending a model request', async ({ page }) => {
  const requests = await open(page, 'chat/s1');
  const metrics = {
    input_tokens: 18420,
    cached_input_tokens: 64200,
    cache_write_tokens: 3200,
    output_tokens: 4850,
    tool_calls: 24,
    llm_turns: 8,
  };
  await page.route('**/v1/sessions/s1/stats', (route) =>
    route.fulfill({
      json: {
        scope: 'runtime_local',
        metrics,
        active_ms: 31400,
        model_ms: 22000,
        tool_ms: 9400,
        cost_usd: 0.0421,
        models: [
          { ...metrics, model: 'shared-model', active_ms: 12000, tool_ms: 4000, cost_usd: 0.0234 },
        ],
        sections: [
          'Current Context / Window Pressure',
          'Tool Discovery',
          'Cumulative Token Usage',
          'Private Side-Question Usage',
          'Guardian Usage',
          'Compaction Usage',
          'Cumulative Session Activity',
          'Compactions',
        ].map((title) => ({
          title,
          rows: Array.from({ length: 5 }, (_, i) => ({
            label: `Detail ${i + 1}`,
            value: `Value ${i + 1}`,
          })),
        })),
      },
    }),
  );
  await page.route('**/v1/sessions/s1/children', (route) =>
    route.fulfill({ json: { children: [] } }),
  );
  await page.locator('#promptInput').fill('/stats');
  await page.locator('#sendBtn').click();
  const dialog = page.getByRole('dialog', { name: 'Chat Stats' });
  await expect(dialog).toBeVisible();
  await expect(page.getByText('31.4s', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Refresh statistics' }).focus();
  await page.keyboard.press('Tab');
  await expect(page.getByRole('region', { name: 'Subagent model usage' })).toBeFocused();
  await page.keyboard.press('Shift+Tab');
  await expect(page.getByRole('button', { name: 'Refresh statistics' })).toBeFocused();
  await expect(page.getByRole('rowheader', { name: 'shared-model' })).toBeVisible();
  expect(await dialog.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(
    true,
  );
  await page.getByRole('heading', { name: 'Compactions', exact: true }).scrollIntoViewIfNeeded();
  await expect(page.getByRole('heading', { name: 'Compactions', exact: true })).toBeVisible();
  expect(await dialog.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
  expect(
    requests.filter((request) => request.method === 'POST' && request.url.endsWith('/responses')),
  ).toHaveLength(0);
  await page.keyboard.press('Escape');
  await expect(dialog).toBeHidden();
});
