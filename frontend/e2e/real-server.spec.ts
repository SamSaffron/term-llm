import { expect, test, type Page } from '@playwright/test';

async function waitForSessionIdle(page: Page, sessionID: string): Promise<void> {
  await expect
    .poll(
      () =>
        page.evaluate(async (id) => {
          const state = (await (
            await fetch(`v1/sessions/${encodeURIComponent(id)}/state`)
          ).json()) as { active_run?: boolean; active_response_id?: string };
          return !state.active_run && !state.active_response_id;
        }, sessionID),
      { timeout: 15_000 },
    )
    .toBe(true);
}

// These checks deliberately do not install page.route handlers. They exercise
// the embedded production bundle and Go HTTP protocol in the isolated server
// started by scripts/browser_lifecycle_smoke.sh.
test('crosses the real browser-to-Go capability and validation boundary', async ({ page }) => {
  await page.goto('./');

  const result = await page.evaluate(async () => {
    const capabilities = await fetch('v1/capabilities');
    const capabilityBody = await capabilities.json();
    const invalid = await fetch('v1/responses', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        model: 'browser-boundary-fixture',
        input: [
          {
            type: 'message',
            role: 'user',
            content: [
              {
                type: 'input_image',
                filename: 'unsafe.svg',
                image_url: 'data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=',
              },
            ],
          },
        ],
      }),
    });
    return {
      capabilityStatus: capabilities.status,
      capabilityBody,
      invalidStatus: invalid.status,
      invalidBody: await invalid.json(),
    };
  });

  expect(result.capabilityStatus).toBe(200);
  expect(result.capabilityBody.attachments.max_count).toBe(10);
  expect(result.capabilityBody.attachments.max_bytes).toBe(20 * 1024 * 1024);
  expect(result.capabilityBody.event_feed).toMatchObject({
    version: 1,
    sse: true,
    long_poll: true,
  });
  expect(result.invalidStatus).toBe(400);
  expect(JSON.stringify(result.invalidBody)).toContain('unsupported attachment type');
});

test('prepares a selected file in the browser and sends its typed part to Go', async ({ page }) => {
  await page.goto('./');
  const newChat = page.locator('#newChatBtn');
  if (!(await newChat.isVisible()))
    await page.getByRole('button', { name: 'Open sidebar' }).click();
  await newChat.click();
  await page.locator('#fileInput').setInputFiles({
    name: 'browser-fixture.txt',
    mimeType: 'text/plain',
    buffer: Buffer.from('attachment boundary fixture'),
  });
  await expect(page.getByText('browser-fixture.txt', { exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Remove browser-fixture.txt' })).toBeVisible();
  await page.getByRole('textbox', { name: 'Message' }).fill('Inspect the prepared attachment');
  await expect(page.getByRole('button', { name: 'Send message' })).toBeEnabled({ timeout: 15_000 });
  await page.getByRole('button', { name: 'Send message' }).click();
  await expect(page.getByRole('heading', { name: 'Debug Provider Output' }).last()).toBeVisible({
    timeout: 15_000,
  });

  const durable = await page.evaluate(async () => {
    const selected = decodeURIComponent(location.pathname.match(/\/chat\/([^/]+)/)?.[1] || '');
    if (!selected) return '';
    const query = new URLSearchParams({
      selected_only: '1',
      include_transcript: '1',
      selected_session: selected,
    });
    return await (await fetch(`v1/sessions?${query}`)).text();
  });
  expect(durable).toContain('browser-fixture.txt');
  expect(durable).toContain('Inspect the prepared attachment');
});

test('sends a real uncommitted diff comment through the browser and Go protocol', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name === 'mobile', 'real diff protocol is covered once');
  await page.goto('./');
  await page.locator('#newChatBtn').click();
  await page.getByRole('textbox', { name: 'Message' }).fill('Create review fixture runtime');
  await page.getByRole('button', { name: 'Send message' }).click();
  await expect(page.getByRole('heading', { name: 'Debug Provider Output' }).last()).toBeVisible({
    timeout: 15_000,
  });
  const sessionID = await page.evaluate(async () => {
    const selected = decodeURIComponent(location.pathname.match(/\/chat\/([^/]+)/)?.[1] || '');
    const query = new URLSearchParams({ selected_only: '1', selected_session: selected });
    const body = (await (await fetch(`v1/sessions?${query}`)).json()) as {
      selected_session?: { id?: string };
    };
    return body.selected_session?.id || '';
  });
  const fixtureStatus = await page.evaluate(async (id) => {
    const response = await fetch(`${window.TERM_LLM_UI_PREFIX}/__browser_fixture/file-change`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ session_id: id }),
    });
    return response.status;
  }, sessionID);
  expect(fixtureStatus).toBe(201);
  const uncommitted = await page.evaluate(async (id) => {
    const response = await fetch(
      `v1/sessions/${encodeURIComponent(id)}/file-changes?scope=uncommitted`,
    );
    return { status: response.status, body: await response.text() };
  }, sessionID);
  expect(uncommitted.status, uncommitted.body).toBe(200);
  expect(uncommitted.body).toContain('review.txt');

  await page.getByRole('button', { name: 'Toggle file changes' }).click();
  await page.getByRole('button', { name: 'Change scope' }).click();
  await page.getByRole('option', { name: 'Uncommitted' }).click();
  const file = page.locator('.diff-file').filter({ hasText: 'review.txt' });
  await expect(file).toBeVisible();
  await file.locator('.diff-file-row').click();
  await file.getByRole('button', { name: 'Comment on line 2' }).click();
  await file.getByRole('textbox', { name: 'Inline comment' }).fill('Keep this fixture change.');
  await file.getByRole('button', { name: 'Send now' }).click();
  await expect(page.getByRole('heading', { name: 'Debug Provider Output' }).last()).toBeVisible({
    timeout: 15_000,
  });

  const durable = await page.evaluate(async () => {
    const selected = decodeURIComponent(location.pathname.match(/\/chat\/([^/]+)/)?.[1] || '');
    const query = new URLSearchParams({
      selected_only: '1',
      include_transcript: '1',
      selected_session: selected,
    });
    return await (await fetch(`v1/sessions?${query}`)).text();
  });
  expect(durable).toContain('Keep this fixture change.');
  expect(durable).toContain('diff_comment');
});

test('streams a response across the real browser-Go-provider boundary', async ({ page }) => {
  await page.goto('./?new=1');
  const composer = page.getByRole('textbox', { name: 'Message' });
  await expect(composer).toBeVisible();
  await composer.fill('Real boundary response');
  await page.getByRole('button', { name: 'Send message' }).click();
  await expect(page.getByText('Debug Provider Output')).toBeVisible({ timeout: 15_000 });
  await expect(page.getByRole('button', { name: 'Send message' })).toBeVisible({ timeout: 15_000 });
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Debug Provider Output' }).last()).toBeVisible({
    timeout: 15_000,
  });
});

test('separate browser contexts replay one idempotent response mutation', async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === 'mobile', 'separate-context idempotency is covered once');
  const firstContext = await browser.newContext();
  const secondContext = await browser.newContext();
  const first = await firstContext.newPage();
  const second = await secondContext.newPage();
  await Promise.all([first.goto('./?new=1'), second.goto('./?new=1')]);
  const operation = `browser-idempotency-${Date.now()}`;
  const sessionID = `sess_browser_idempotency_${Date.now()}`;
  const submit = (page: typeof first) =>
    page.evaluate(
      async ({ key, session }) => {
        const response = await fetch('v1/responses', {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
            'Idempotency-Key': key,
            session_id: session,
          },
          body: JSON.stringify({
            model: 'fast',
            input: 'one logical mutation',
            stream: true,
            client_message_id: key,
          }),
        });
        return {
          status: response.status,
          responseID: response.headers.get('x-response-id') || '',
          body: await response.text(),
        };
      },
      { key: operation, session: sessionID },
    );
  const [left, right] = await Promise.all([submit(first), submit(second)]);
  expect([left.status, right.status].every((status) => status === 200)).toBe(true);
  expect(left.responseID).not.toBe('');
  expect(right.responseID).toBe(left.responseID);
  await Promise.all([firstContext.close(), secondContext.close()]);
});

test('recovers and resolves an ask-user request across real same-context tabs', async ({
  context,
  page,
}, testInfo) => {
  test.skip(testInfo.project.name === 'mobile', 'real two-tab control plane is covered once');
  await page.goto('./');
  await page.locator('#newChatBtn').click({ force: true });
  await page.getByRole('textbox', { name: 'Message' }).fill('Create interaction fixture runtime');
  await page.getByRole('button', { name: 'Send message' }).click();
  await expect(page.getByRole('heading', { name: 'Debug Provider Output' }).last()).toBeVisible({
    timeout: 15_000,
  });
  const sessionID = await page.evaluate(async () => {
    const selected = decodeURIComponent(location.pathname.match(/\/chat\/([^/]+)/)?.[1] || '');
    const query = new URLSearchParams({ selected_only: '1', selected_session: selected });
    const body = (await (await fetch(`v1/sessions?${query}`)).json()) as {
      selected_session?: { id?: string };
    };
    return body.selected_session?.id || '';
  });
  expect(sessionID).not.toBe('');
  // These fixtures are standalone control-plane requests. If they are created
  // while the setup response is still active, response completion correctly
  // retires them as interactions owned by that response.
  await waitForSessionIdle(page, sessionID);
  const fixtureStatus = await page.evaluate(async (id) => {
    const response = await fetch(`${window.TERM_LLM_UI_PREFIX}/__browser_fixture/ask-user`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ session_id: id }),
    });
    return response.status;
  }, sessionID);
  expect(fixtureStatus).toBe(201);

  const second = await context.newPage();
  await Promise.all([page.reload(), second.goto(page.url())]);
  const question = 'Which path should the browser fixture take?';
  await expect(page.getByText(question)).toBeVisible();
  await expect(second.getByText(question)).toBeVisible();
  await expect(page.getByRole('dialog', { name: 'Answer question' })).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.getByText('Decision waiting — Open')).toBeVisible();
  await second.getByRole('radio', { name: /Safe/ }).check();
  await second.getByRole('button', { name: 'Continue' }).click();
  await expect(second.getByRole('dialog', { name: 'Answer question' })).toBeHidden();
  await expect
    .poll(
      () =>
        second.evaluate(async (id) => {
          const state = (await (
            await fetch(`v1/sessions/${encodeURIComponent(id)}/state`)
          ).json()) as { pending_ask_user?: unknown; pending_ask_users?: unknown[] };
          return Boolean(state.pending_ask_user || state.pending_ask_users?.length);
        }, sessionID),
      { timeout: 10_000 },
    )
    .toBe(false);

  await page.reload();
  await expect(page.getByText('Decision waiting — Open')).toBeHidden();
  await expect(page.getByText(question)).toBeHidden();
  await second.close();
});

test('recovers, neutrally dismisses, and resolves a real approval', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name === 'mobile', 'real approval protocol is covered once');
  await page.goto('./?new=1');
  await page.getByRole('textbox', { name: 'Message' }).fill('Start approval fixture runtime');
  await page.getByRole('button', { name: 'Send message' }).click();
  await expect(page.getByRole('heading', { name: 'Debug Provider Output' }).last()).toBeVisible({
    timeout: 15_000,
  });
  const sessionID = await page.evaluate(async () => {
    const selected = decodeURIComponent(location.pathname.match(/\/chat\/([^/]+)/)?.[1] || '');
    const query = new URLSearchParams({ selected_only: '1', selected_session: selected });
    const body = (await (await fetch(`v1/sessions?${query}`)).json()) as {
      selected_session?: { id?: string };
    };
    return body.selected_session?.id || '';
  });
  expect(sessionID).not.toBe('');
  await waitForSessionIdle(page, sessionID);
  const fixtureStatus = await page.evaluate(async (id) => {
    const response = await fetch(`${window.TERM_LLM_UI_PREFIX}/__browser_fixture/approval`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ session_id: id }),
    });
    return response.status;
  }, sessionID);
  expect(fixtureStatus).toBe(201);
  await page.reload();

  const title = 'Write Access Request';
  const decisionBanner = page.getByRole('button', { name: 'Decision waiting — Open' });
  const approvalDialog = page.getByRole('dialog', { name: title });
  await expect(approvalDialog).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(approvalDialog).toBeHidden();
  await expect(page.locator('.modal-overlay')).toHaveCount(0);
  await expect(decisionBanner).toBeVisible();
  await decisionBanner.click();
  await page.getByRole('button', { name: 'Approve' }).click();
  await expect(decisionBanner).toBeHidden();
  await expect(page.locator('.modal-overlay')).toHaveCount(0);
  await page.reload();
  await expect(approvalDialog).toBeHidden();
});

test('a suspended same-context tab resumes through authoritative reconciliation', async ({
  context,
  page,
}, testInfo) => {
  test.skip(testInfo.project.name === 'mobile', 'real same-context suspension is covered once');
  const second = await context.newPage();
  await Promise.all([page.goto('./?new=1'), second.goto('./')]);
  const before = await second.locator('.session-row').count();
  const cdp = await context.newCDPSession(second);
  await cdp.send('Page.setWebLifecycleState', { state: 'frozen' });

  await page
    .getByRole('textbox', { name: 'Message' })
    .fill('Suspended tab reconciliation boundary');
  await page.getByRole('button', { name: 'Send message' }).click();
  await expect(page.getByRole('heading', { name: 'Debug Provider Output' }).last()).toBeVisible({
    timeout: 15_000,
  });

  await cdp.send('Page.setWebLifecycleState', { state: 'active' });
  await second.evaluate(() =>
    window.dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true })),
  );
  await expect
    .poll(() => second.locator('.session-row').count(), { timeout: 15_000 })
    .toBeGreaterThan(before);
  await second.close();
});

test('a newly sent plain HTTPS response keeps its owned stream', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'plain HTTPS admission race is covered once');
  test.setTimeout(30_000);
  await page.goto('./?new=1&no_webrtc=1');
  await page.getByRole('textbox', { name: 'Message' }).fill('sleep 5 response transport probe');
  await page.getByRole('button', { name: 'Send message' }).click();

  await expect(page.locator('#stopBtn')).toBeVisible({ timeout: 5_000 });
  const unknown = page.getByRole('status', { name: 'Response status is unknown' });
  await expect(unknown).toBeHidden();
  await page.waitForTimeout(2_000);
  await expect(page.locator('#stopBtn')).toBeVisible();
  await expect(unknown).toBeHidden();
  await expect(page.getByRole('heading', { name: 'Debug Provider Output' }).last()).toBeVisible({
    timeout: 15_000,
  });
});

test('reloading a running HTTPS session never presents it as idle', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'running-session reload is covered once');
  test.setTimeout(30_000);
  await page.goto('./?new=1&no_webrtc=1');
  await page.getByRole('textbox', { name: 'Message' }).fill('sleep 8 reload running response');
  await page.getByRole('button', { name: 'Send message' }).click();
  await expect(page.locator('#stopBtn')).toBeVisible({ timeout: 5_000 });

  await page.reload();

  const unknown = page.getByRole('status', { name: 'Response status is unknown' });
  await expect(page.locator('#stopBtn')).toBeVisible({ timeout: 5_000 });
  await expect(page.getByRole('button', { name: 'Response is running' })).toBeEnabled();
  await expect(page.getByPlaceholder('Type to interject…')).toBeVisible();
  await expect(unknown).toBeHidden();
  await page.waitForTimeout(2_000);
  await expect(page.locator('#stopBtn')).toBeVisible();
  await expect(unknown).toBeHidden();
  await expect(page.getByRole('heading', { name: 'Debug Provider Output' }).last()).toBeVisible({
    timeout: 15_000,
  });
});

test('a disconnected mobile response stream cannot keep claiming work is running', async ({
  context,
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'mobile', 'mobile disconnect recovery is covered once');
  test.setTimeout(45_000);
  await page.addInitScript(() => {
    const NativePeer = window.RTCPeerConnection;
    const peers: RTCPeerConnection[] = [];
    Object.defineProperty(window, '__responseStreamTestPeers', { value: peers });
    window.RTCPeerConnection = class extends NativePeer {
      constructor(configuration?: RTCConfiguration) {
        super(configuration);
        peers.push(this);
      }
    };
  });
  await page.goto('./?new=1');
  await page.getByRole('textbox', { name: 'Message' }).fill('sleep 5 response transport probe');
  await page.getByRole('button', { name: 'Send message' }).click();
  await expect(page.locator('#stopBtn')).toBeVisible();
  // Chat routes prefer a human-facing numeric slug; resolve it to the durable
  // session ID used by status and response APIs.
  const sessionID = await page.evaluate(async () => {
    const selected = decodeURIComponent(location.pathname.match(/\/chat\/([^/]+)/)?.[1] || '');
    const query = new URLSearchParams({ selected_only: '1', selected_session: selected });
    const body = (await (await fetch(`v1/sessions?${query}`)).json()) as {
      selected_session?: { id?: string };
    };
    return body.selected_session?.id || '';
  });
  expect(sessionID).not.toBe('');

  const baseURL = String(testInfo.project.use.baseURL || '');
  const sessionStatus = async () => {
    const query = new URLSearchParams({ selected_session: sessionID });
    const response = await fetch(`${baseURL}v1/sessions/status?${query}`);
    const data = (await response.json()) as { sessions?: Array<Record<string, unknown>> };
    return data.sessions?.find((entry) => String(entry.id || entry.session_id) === sessionID);
  };
  // The stop button is optimistic client state. Wait for authoritative server
  // admission before cutting the browser's transports, or the request itself
  // can be dropped and there is no running response to recover from.
  await expect.poll(async () => Boolean((await sessionStatus())?.active_run)).toBe(true);

  await context.setOffline(true);
  await page.evaluate(() => {
    const peers = (window as unknown as { __responseStreamTestPeers?: RTCPeerConnection[] })
      .__responseStreamTestPeers;
    peers?.forEach((peer) => peer.close());
    window.stop();
  });

  await expect(page.locator('#stopBtn')).toBeHidden({ timeout: 5_000 });
  await expect(page.getByRole('status', { name: 'Response status is unknown' })).toBeHidden();

  await expect
    .poll(
      async () => {
        const status = await sessionStatus();
        return Boolean(status && !status.active_run && !status.active_response_id);
      },
      { timeout: 15_000 },
    )
    .toBe(true);

  await context.setOffline(false);
  await expect(page.getByRole('heading', { name: 'Debug Provider Output' }).last()).toBeVisible({
    timeout: 15_000,
  });
  await expect(page.getByRole('status', { name: 'Response status is unknown' })).toBeHidden();
});

test('real status endpoint honors browser conditional requests', async ({ page }) => {
  await page.goto('./');
  const result = await page.evaluate(async () => {
    for (let attempt = 0; attempt < 5; attempt += 1) {
      const first = await fetch('v1/sessions/status');
      const etag = first.headers.get('etag') || '';
      await first.text();
      const second = await fetch('v1/sessions/status', {
        headers: etag ? { 'If-None-Match': etag } : {},
      });
      if (second.status === 304) return { first: first.status, etag, second: second.status };
      await second.text();
    }
    return { first: 200, etag: '', second: 0 };
  });
  expect(result.first).toBe(200);
  expect(result.etag).not.toBe('');
  expect(result.second).toBe(304);
});
