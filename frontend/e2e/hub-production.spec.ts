import { expect, test } from '@playwright/test';

const hubRoot = process.env.TERM_LLM_HUB_SMOKE_ROOT || '';
const hubToken = process.env.TERM_LLM_HUB_SMOKE_TOKEN || '';
const nodeRoot = process.env.TERM_LLM_HUB_NODE_ROOT || '';
const registrationToken = process.env.TERM_LLM_HUB_REGISTRATION_TOKEN || '';

async function authenticateDashboard(page: import('@playwright/test').Page) {
  await page.goto(`${hubRoot}?token=${encodeURIComponent(hubToken)}`);
  await expect(page).toHaveURL(new RegExp(`${hubRoot.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`));
  expect(page.url()).not.toContain('token=');
}

test('Hub Preact dashboard supports mounted refresh, registration, and node mutation', async ({
  page,
}, testInfo) => {
  test.skip(
    !hubRoot || !hubToken || !nodeRoot || !registrationToken,
    'production-shaped Hub dashboard smoke is not configured',
  );
  test.setTimeout(60_000);
  let delegationRequests = 0;
  let failNodes = false;
  let emptyNodes = false;
  let delegations: Record<string, unknown>[] = [];
  const nodeMutationResponses: Promise<string>[] = [];
  page.on('response', (response) => {
    const request = response.request();
    const pathname = new URL(response.url()).pathname;
    if (
      request.method() === 'POST' &&
      (pathname === '/hub/api/nodes' || pathname === '/hub/api/nodes/test')
    ) {
      nodeMutationResponses.push(response.text());
    }
  });
  await page.route('**/hub/api/nodes', async (route) => {
    if (route.request().method() !== 'GET') {
      await route.continue();
      return;
    }
    if (failNodes) {
      await route.fulfill({ status: 500, contentType: 'text/plain', body: 'fixture node failure' });
      return;
    }
    const response = await route.fetch();
    const body = (await response.json()) as { nodes?: Array<Record<string, unknown>> };
    if (emptyNodes) {
      body.nodes = [];
    } else {
      const production = body.nodes?.find((node) => node.id === 'production-node');
      if (production) {
        production.sessions = {
          count_label: '2 sessions',
          active_count: 1,
          active: [
            {
              id: 'fixture-active',
              short_title: 'Fixture active session',
              active_run: true,
              last_message_at: Date.now(),
              message_count: 3,
              resume_path: '/hub/node/production-node/chat/fixture-active',
            },
          ],
          recent: [
            {
              id: 'fixture-recent',
              short_title: 'Fixture recent session',
              last_message_at: Date.now() - 60_000,
              message_count: 2,
              resume_path: '/hub/node/production-node/chat/fixture-recent',
            },
          ],
        };
      }
    }
    await route.fulfill({ response, json: body });
  });
  await page.route('**/hub/api/attention', async (route) => {
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({
        total_running: 0,
        total_input_required: 1,
        total_unseen: 1,
        nodes: [],
        has_more: false,
        input_required: [
          {
            node_id: 'production-node',
            node_name: 'Production Node',
            session_id: 'fixture-input',
            title: 'Fixture question',
            pending_interaction_count: 1,
            pending_interaction_kinds: ['ask_user'],
            required_since: new Date().toISOString(),
            resume_path: '/hub/node/production-node/chat/fixture-input',
          },
        ],
        inbox: [
          {
            node_id: 'production-node',
            node_name: 'Production Node',
            session_id: 'fixture-ready',
            title: 'Fixture review',
            outcome: 'succeeded',
            terminal_at: new Date().toISOString(),
            attention_seq: 1,
            resume_path: '/hub/node/production-node/chat/fixture-ready',
          },
        ],
      }),
    });
  });
  await page.route('**/hub/api/delegations', async (route) => {
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({ delegations }),
    });
  });
  page.on('request', (request) => {
    if (new URL(request.url()).pathname === '/hub/api/delegations') delegationRequests++;
  });

  await authenticateDashboard(page);
  await expect(page.getByRole('heading', { name: 'term-llm Hub' })).toBeVisible();
  const production = page.locator('.node-card').filter({ hasText: 'Production Node' });
  await expect(production).toBeVisible();
  await expect(page.getByRole('region', { name: 'Needs your input' })).toContainText(
    'Fixture question',
  );
  await expect(page.getByRole('region', { name: 'Ready to review' })).toContainText(
    'Fixture review',
  );
  await expect(page.getByText('No delegated work yet.')).toBeVisible();
  await expect(production.getByText('Fixture active session')).toBeVisible();
  await expect(production.getByText('Fixture recent session')).toBeVisible();
  await expect(production.getByText(/active now.*3 messages/)).toBeVisible();
  await expect(production.getByRole('link', { name: 'Resume' })).toHaveAttribute(
    'href',
    '/hub/node/production-node/chat/fixture-active',
  );
  await expect(production.getByRole('link', { name: 'New' })).toHaveAttribute(
    'href',
    /\/hub\/node\/production-node\//,
  );
  expect(await page.locator('body').textContent()).not.toContain(registrationToken);

  await expect.poll(() => delegationRequests).toBeGreaterThanOrEqual(1);
  delegations = [
    {
      id: 'fixture-delegation',
      origin_node: 'production-node',
      target_node: 'production-node',
      agent_name: 'reviewer',
      prompt: 'Fixture delegated prompt',
      response: '[Fixture artifact](https://example.invalid/artifact)',
      status: 'running',
      depth: 1,
      created_at: new Date().toISOString(),
      updated_at: new Date().toISOString(),
    },
  ];
  const beforeRefresh = delegationRequests;
  await page.getByRole('button', { name: 'Refresh' }).click();
  await expect.poll(() => delegationRequests).toBe(beforeRefresh + 1);
  await expect(page.getByText('Fixture delegated prompt')).toBeVisible();
  await expect(page.getByRole('link', { name: 'Fixture artifact' })).toHaveAttribute(
    'rel',
    'noopener noreferrer',
  );

  await page.getByRole('button', { name: 'Add node' }).click();
  const registrationDialog = page.getByRole('dialog');
  await expect(page.getByLabel('Name')).toBeFocused();
  for (let index = 0; index < 10; index++) {
    await page.keyboard.press('Tab');
    expect(
      await registrationDialog.evaluate((dialog) => dialog.contains(document.activeElement)),
    ).toBe(true);
  }
  await page.getByRole('button', { name: 'Private node' }).click();
  await expect(
    page.locator('.help-label').getByText('Registration token', { exact: true }),
  ).toBeVisible();
  expect(await page.getByRole('dialog').textContent()).not.toContain(registrationToken);
  await page.getByRole('button', { name: 'Reveal' }).click();
  await expect(page.getByText(registrationToken)).toBeVisible();
  await page.getByRole('button', { name: 'Copy token' }).click();
  await expect(page.getByText('Copied registration token.')).toBeVisible();
  await page.getByRole('button', { name: 'Close' }).click();
  await expect(page.getByRole('dialog')).toHaveCount(0);
  expect(await page.locator('body').textContent()).not.toContain(registrationToken);

  const throwaway = `Throwaway ${testInfo.project.name}`;
  await page.getByRole('button', { name: 'Add node' }).click();
  await page.getByLabel('Name').fill(throwaway);
  await page.getByLabel('URL').fill(nodeRoot);
  await page.getByLabel(/^Bearer token/).fill('production-node-bearer');
  await page.getByRole('button', { name: 'Test connection' }).click();
  await expect(page.getByRole('status').filter({ hasText: 'Reachable' })).toBeVisible();
  await page.getByRole('button', { name: 'Add node', exact: true }).last().click();
  await expect(page.getByRole('dialog')).toHaveCount(0);
  const card = page.locator('.node-card').filter({ hasText: throwaway });
  await expect(card).toBeVisible();
  expect(await page.locator('body').textContent()).not.toContain('production-node-bearer');
  expect(
    await page
      .locator('input')
      .evaluateAll(
        (inputs, secret) => inputs.some((input) => (input as HTMLInputElement).value === secret),
        'production-node-bearer',
      ),
  ).toBe(false);
  const nodesResponse = await page.request.get(`${hubRoot}api/nodes`);
  expect(await nodesResponse.text()).not.toContain('production-node-bearer');
  await expect.poll(() => nodeMutationResponses.length).toBe(2);
  for (const body of await Promise.all(nodeMutationResponses)) {
    expect(body).not.toContain('production-node-bearer');
  }

  await card.getByRole('button', { name: `More actions for ${throwaway}` }).click();
  page.once('dialog', (dialog) => void dialog.accept());
  await page.getByRole('menuitem', { name: 'Remove node' }).click();
  await expect(card).toHaveCount(0);

  emptyNodes = true;
  await page.getByRole('button', { name: 'Refresh' }).click();
  await expect(page.getByText('No nodes yet.')).toBeVisible();
  emptyNodes = false;
  await page.getByRole('button', { name: 'Refresh' }).click();
  await expect(production).toBeVisible();

  failNodes = true;
  await page.getByRole('button', { name: 'Refresh' }).click();
  await expect(page.getByRole('alert')).toContainText('fixture node failure');
  await expect(production).toBeVisible();
  failNodes = false;
  await page.getByRole('button', { name: 'Refresh' }).click();
  await expect(page.getByRole('alert')).toHaveCount(0);

  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  expect(overflow).toBeLessThanOrEqual(1);
});

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
