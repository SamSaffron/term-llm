import { test, expect, type Page, type Route } from '@playwright/test';

const session = (id: string, title: string, number: number) => ({ id, number, title, name: title, mode: 'chat', origin: 'web', created_at: 1_700_000_000, last_message_at: 1_700_000_001, pinned: false, archived: false });

async function mockAPI(page: Page, options: { holdStream?: boolean } = {}) {
  const requests: Array<{ method: string; url: string }> = [];
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request(); const url = new URL(request.url()); const path = url.pathname; requests.push({ method: request.method(), url: path });
    const json = (value: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(value) });
    if (path.endsWith('/v1/capabilities')) return json({ widgets: [{ id: 'status', name: 'Status', url: '../widgets/status/' }] });
    if (path.endsWith('/v1/providers')) return json({ object: 'list', data: [{ name: 'openai', configured: true, is_default: true, default_model: 'gpt-test', models: ['gpt-test'] }] });
    if (path.endsWith('/v1/models')) return json({ object: 'list', data: [{ id: 'gpt-test', owned_by: 'openai', reasoning_efforts: ['low', 'medium', 'high'] }] });
    if (path.endsWith('/v1/sidebar')) return json({ sessions: [session('s1', 'First chat', 1), session('s2', 'Second chat', 2)] });
    if (path.endsWith('/v1/sessions/status')) return json({ sessions: [] });
    if (path.endsWith('/v1/sessions') && url.searchParams.get('selected_only') === '1') {
      const id = url.searchParams.get('selected_session') || 's1';
      return json({ selected_session: session(id, id === 's2' ? 'Second chat' : 'First chat', id === 's2' ? 2 : 1), selected_transcript: { bodies: { messages: [{ id: 1, sequence: 0, role: 'user', parts: [{ type: 'text', text: id === 's2' ? 'Second question' : 'First question' }] }] } } });
    }
    if (/\/v1\/sessions\/s[12]\/state$/.test(path)) return json({ session: session(path.includes('/s2/') ? 's2' : 's1', path.includes('/s2/') ? 'Second chat' : 'First chat', path.includes('/s2/') ? 2 : 1) });
    if (/\/v1\/sessions\/s[12]\/transcript$/.test(path)) return json({ messages: [{ id: 1, sequence: 0, role: 'user', parts: [{ type: 'text', text: path.includes('/s2/') ? 'Second question' : 'First question' }] }] });
    if (path.endsWith('/v1/responses') && request.method() === 'POST') {
      const body = options.holdStream
        ? 'event: response.created\ndata: {"response_id":"r1","run_epoch":1,"sequence_number":1}\n\n'
        : [
          'event: response.created\ndata: {"response_id":"r1","run_epoch":1,"sequence_number":1}\n\n',
          'event: response.output_text.delta\ndata: {"response_id":"r1","run_epoch":1,"sequence_number":2,"delta":"Streamed answer"}\n\n',
          'event: response.completed\ndata: {"response_id":"r1","run_epoch":1,"sequence_number":3,"usage":{"total_tokens":10}}\n\n',
        ].join('');
      return route.fulfill({ status: 200, contentType: 'text/event-stream', headers: { 'x-response-id': 'r1', 'x-session-id': 's1' }, body });
    }
    if (path.endsWith('/v1/responses/r1') && request.method() === 'GET') return json({ id: 'r1', session_id: 's1', run_epoch: 1, status: options.holdStream ? 'streaming' : 'completed', last_sequence_number: 1 });
    if (path.endsWith('/v1/responses/r1/events')) {
      if (options.holdStream) await new Promise((resolve) => setTimeout(resolve, 5_000));
      return route.fulfill({ status: 200, contentType: 'text/event-stream', body: [
      'event: response.created\ndata: {"response_id":"r1","run_epoch":1,"sequence_number":1}\n\n',
      'event: response.output_text.delta\ndata: {"response_id":"r1","run_epoch":1,"sequence_number":2,"delta":"Streamed answer"}\n\n',
      'event: response.completed\ndata: {"response_id":"r1","run_epoch":1,"sequence_number":3,"usage":{"total_tokens":10}}\n\n',
      ].join('') });
    }
    if (path.endsWith('/v1/responses/r1/cancel')) return json({ ok: true });
    if (path.includes('/file-changes/diff')) return json({ diff: '@@ -1,1 +1,2 @@\n-old\n+new\n+line' });
    if (path.includes('/file-changes')) return json({ files: [{ path: 'frontend/src/main.tsx', additions: 2, deletions: 1, status: 'modified' }] });
    return json({});
  });
  return requests;
}

async function open(page: Page, suffix = '', options: { holdStream?: boolean } = {}) {
  const requests = await mockAPI(page, options);
  await page.goto(`./${suffix}`);
  await expect(page.locator('#startupSplash')).toBeHidden({ timeout: 10_000 });
  return requests;
}

test('loads, navigates sessions, opens settings and preserves normal namespace hygiene', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name === 'mobile', 'desktop session navigation is covered separately');
  await open(page);
  await expect(page.getByRole('heading', { name: 'First chat' })).toBeVisible();
  await page.getByRole('button', { name: 'Second chat', exact: true }).click();
  await expect(page.getByText('Second question')).toBeVisible();
  await expect(page).toHaveURL(/\/chat\/2$/);
  await page.locator('#settingsBtn').click();
  const settings = page.getByRole('dialog', { name: 'Settings' });
  await expect(settings).toBeVisible();
  await settings.getByLabel('Provider').selectOption('openai');
  expect(await page.evaluate(() => ({ legacy: 'TermLLMApp' in window, test: '__TERM_LLM_TEST__' in window }))).toEqual({ legacy: false, test: false });
});

test('desktop shell uses authored controls, message hierarchy and diff rows', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name === 'mobile', 'desktop visual structure');
  await open(page);
  const sessionButton = page.getByRole('button', { name: 'First chat', exact: true });
  await expect(sessionButton).toHaveCSS('display', 'flex');
  await expect(sessionButton).toHaveCSS('border-radius', '8px');
  await expect(page.locator('#newChatBtn svg')).toBeVisible();
  await expect(page.locator('#attachBtn svg')).toBeVisible();
  await expect(page.locator('.message-body')).toHaveCSS('border-radius', '12px');
  expect(await page.locator('.message-meta').evaluate((element) => parseFloat(getComputedStyle(element).columnGap) > 0)).toBe(true);

  await page.getByRole('button', { name: 'Toggle file changes' }).click();
  await expect(page.locator('#diffSidebar')).toBeVisible();
  await expect(page.locator('.diff-scope-select')).toHaveCSS('display', 'block');
  await expect(page.locator('.diff-file-row')).toHaveCSS('display', 'flex');
  await expect(page.locator('.diff-file-toggle')).toHaveCSS('border-top-width', '0px');
  await page.locator('.diff-file-row').hover();
  await expect(page.locator('.diff-action-btn svg')).toBeVisible();
});

test('sends through public composer UI and reduces a streamed response', async ({ page }) => {
  const requests = await open(page);
  await page.getByRole('textbox', { name: 'Message' }).fill('Hello');
  await page.getByRole('button', { name: 'Send message' }).click();
  await expect(page.getByText('Streamed answer')).toBeVisible();
  expect(requests.some((entry) => entry.method === 'POST' && entry.url.endsWith('/v1/responses'))).toBe(true);
});

test('cancels an active response from the public stop control', async ({ page }) => {
  const requests = await open(page, '', { holdStream: true });
  await page.getByRole('textbox', { name: 'Message' }).fill('Long request');
  await page.getByRole('button', { name: 'Send message' }).click();
  await page.locator('#stopBtn').click();
  await expect.poll(() => requests.some((entry) => entry.method === 'POST' && entry.url.endsWith('/v1/responses/r1/cancel'))).toBe(true);
});

test('opens diff UI, expands a file and queues an inline comment', async ({ page }) => {
  await open(page);
  await page.getByRole('button', { name: 'Toggle file changes' }).click();
  await expect(page.getByText('frontend/src/main.tsx')).toBeVisible();
  await page.getByRole('button', { name: 'Toggle diff for frontend/src/main.tsx' }).click();
  await expect(page.getByText('+new')).toBeVisible();
  await page.getByRole('button', { name: /Comment on line 1/ }).first().click();
  await page.getByRole('textbox', { name: 'Inline comment' }).fill('Please explain this');
  await page.getByRole('button', { name: 'Queue' }).click();
  await expect(page.getByText('1 inline comment queued.')).toBeVisible();
});

test('mobile viewport opens a styled sidebar and returns to a usable composer', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== 'mobile', 'mobile-only interaction');
  await open(page);
  await page.getByRole('button', { name: 'Open sidebar' }).click();
  const sidebar = page.locator('#sidebar');
  await expect(sidebar).toHaveClass(/open/);
  await expect(sidebar).toHaveCSS('visibility', 'visible');
  await expect(page.locator('#newChatBtn')).toHaveCSS('display', 'flex');
  await expect(page.locator('#newChatBtn svg')).toBeVisible();
  await page.locator('#newChatBtn').click();
  await expect(sidebar).not.toHaveClass(/open/);
  await expect(page.getByRole('textbox', { name: 'Message' })).toBeVisible();
  await expect(page.locator('.composer-box')).toHaveCSS('border-radius', '22px');
  await expect(page.locator('#sendBtn svg')).toBeVisible();
});

test('explicit browser-test bridge is gated and excludes credentials', async ({ page }) => {
  await open(page, '?test_bridge=1');
  const bridge = await page.evaluate(() => ({ present: Boolean(window.__TERM_LLM_TEST__), keys: Object.keys(window.__TERM_LLM_TEST__ || {}), serialized: JSON.stringify(window.__TERM_LLM_TEST__) }));
  expect(bridge.present).toBe(true); expect(bridge.keys).toEqual(['store', 'domain']); expect(bridge.serialized).not.toContain('token'); expect(bridge.serialized).not.toContain('Authorization');
});
