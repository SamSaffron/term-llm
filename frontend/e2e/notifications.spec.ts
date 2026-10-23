import { expect, test, type Page } from '@playwright/test';

async function bind(page: Page, windowId: string, documentId: string) {
  return page.evaluate(
    async ({ windowId, documentId }) => {
      const registration = await navigator.serviceWorker.ready;
      const active = registration.active;
      if (!active) throw new Error('No active worker');
      return new Promise<{ status: string; previous_document_id?: string }>((resolve, reject) => {
        const channel = new MessageChannel();
        const timer = setTimeout(() => reject(new Error('No binding reply')), 5_000);
        channel.port1.onmessage = (event) => {
          clearTimeout(timer);
          channel.port1.close();
          resolve(event.data);
        };
        active.postMessage(
          {
            type: 'notification-bind',
            version: 2,
            window_id: windowId,
            document_id: documentId,
            // A page cannot supply its own WindowClient identity.
            client_id: 'forged-client-id',
          },
          [channel.port2],
        );
      });
    },
    { windowId, documentId },
  );
}

for (const route of ['', 'chat/9']) {
  test(`worker binds real windows, rejects cloned ownership and preserves reload at ${route || 'root'}`, async ({
    context,
    page,
  }, testInfo) => {
    test.skip(
      testInfo.project.name === 'iphone',
      'WKWebView uses optional native ownership instead',
    );
    await page.goto(`./${route}`);
    expect(await bind(page, 'owner-window', 'owner-document')).toEqual({ status: 'bound' });
    const sibling = await context.newPage();
    await sibling.goto('./');
    expect(await bind(sibling, 'owner-window', 'cloned-document')).toEqual({ status: 'collision' });
    expect(await bind(sibling, 'sibling-window', 'sibling-document')).toEqual({ status: 'bound' });
    await page.reload();
    expect(await bind(page, 'owner-window', 'reloaded-document')).toEqual({
      status: 'bound',
      previous_document_id: 'owner-document',
    });
    await page.close();
    // Gone ownership can be rebound, but a live unrelated sibling never owns it.
    expect(await bind(sibling, 'owner-window', 'new-document')).toEqual({
      status: 'bound',
      previous_document_id: 'reloaded-document',
    });
  });
}

test('retired protocol-terminal page messages cannot create notifications', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name === 'iphone', 'WKWebView has no browser worker');
  await page.goto('./');
  await page.evaluate(async () => {
    const registration = await navigator.serviceWorker.ready;
    registration.active?.postMessage({
      type: 'completion-notification',
      payload: {
        version: 1,
        event_id: 'completion:obsolete:sub',
        title: 'No local authority',
        body: 'Ignored',
      },
    });
  });
  // A durable binding ACK is a worker-side barrier, not an arbitrary sleep.
  expect(await bind(page, 'owner-window', 'document')).toEqual({ status: 'bound' });
  expect(
    await page.evaluate(
      async () =>
        (
          await (
            await navigator.serviceWorker.ready
          ).getNotifications({ tag: 'term-llm-completion:completion:obsolete:sub' })
        ).length,
    ),
  ).toBe(0);
});

/** Invoke the installed worker listener with a synthetic click, not a push/OS alert.
 * CDP is confined to Playwright's disposable browser and never exists in the app.
 */
async function clickInWorker(page: Page, payload: Record<string, unknown>) {
  const browser = page.context().browser();
  if (!browser) throw new Error('No owned test browser');
  const cdp = await browser.newBrowserCDPSession();
  const scope = await page.evaluate(async () => (await navigator.serviceWorker.ready).scope);
  const { targetInfos } = await cdp.send('Target.getTargets');
  const target = targetInfos.find(
    (entry) => entry.type === 'service_worker' && entry.url.startsWith(`${scope}sw.js`),
  );
  if (!target) throw new Error('No worker target');
  const { sessionId } = await cdp.send('Target.attachToTarget', {
    targetId: target.targetId,
    flatten: false,
  });
  try {
    return await new Promise<{ focused: string[]; opened: string[] }>((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('Worker click did not settle')), 5_000);
      cdp.on('Target.receivedMessageFromTarget', (event) => {
        if (event.sessionId !== sessionId) return;
        const message = JSON.parse(event.message);
        if (message.id !== 1) return;
        clearTimeout(timer);
        if (message.error || message.result?.exceptionDetails)
          reject(new Error(JSON.stringify(message)));
        else resolve(message.result.result.value);
      });
      const expression = `(async () => {
        const pending = [], focused = [], opened = [];
        const window = (await self.clients.matchAll({type: 'window', includeUncontrolled: true}))[0];
        const windowPrototype = Object.getPrototypeOf(window);
        const clientsPrototype = Object.getPrototypeOf(self.clients);
        const originalFocus = windowPrototype.focus, originalOpen = clientsPrototype.openWindow;
        // Chromium requires a trusted OS click to focus/open. Shim only these
        // activation-gated side effects; matching and postMessage use real clients.
        windowPrototype.focus = function() { focused.push(this.id); return Promise.resolve(this); };
        clientsPrototype.openWindow = async function(url) { opened.push(String(url)); return null; };
        try {
          const event = new Event('notificationclick');
          Object.defineProperties(event, {
            notification: {value: { data: ${JSON.stringify(payload)}, close() {} }},
            waitUntil: {value: (promise) => pending.push(promise)},
          });
          self.dispatchEvent(event);
          await Promise.all(pending);
          return {focused, opened};
        } finally {
          windowPrototype.focus = originalFocus;
          clientsPrototype.openWindow = originalOpen;
        }
      })()`;
      void cdp
        .send('Target.sendMessageToTarget', {
          sessionId,
          message: JSON.stringify({
            id: 1,
            method: 'Runtime.evaluate',
            params: { expression, awaitPromise: true, returnByValue: true, userGesture: true },
          }),
        })
        .catch(reject);
    });
  } finally {
    await cdp.detach();
  }
}

test('notification click routes only the bound owner WindowClient, or opens fresh after it closes', async ({
  context,
  page,
  baseURL,
}, testInfo) => {
  test.skip(testInfo.project.name === 'iphone', 'WKWebView uses native scene routing');
  await context.addInitScript(() => {
    navigator.serviceWorker.addEventListener('message', (event) => {
      if (event.data?.type !== 'notification-route') return;
      sessionStorage.setItem('observed-notification-route', event.data.url);
      // Observe real worker-to-WindowClient routing without navigating the test
      // document out from under its binding. Product navigation has unit coverage.
      event.stopImmediatePropagation();
    });
  });
  const sibling = await context.newPage();
  await sibling.goto('./');
  await page.goto('./');
  await bind(sibling, 'sibling-window', 'sibling-document');
  await bind(page, 'owner-window', 'owner-document');
  const target = new URL('chat/exact-owner', baseURL).href;
  const payload = { kind: 'completion', window_id: 'owner-window', url: target };
  const routed = await clickInWorker(page, payload);
  expect(routed.focused).toHaveLength(1);
  expect(routed.opened).toEqual([]);
  await expect
    .poll(() => page.evaluate(() => sessionStorage.getItem('observed-notification-route')))
    .toBe(target);
  expect(
    await sibling.evaluate(() => sessionStorage.getItem('observed-notification-route')),
  ).toBeNull();
  await page.close();
  const missing = await clickInWorker(sibling, payload);
  expect(missing.focused).toEqual([]);
  expect(missing.opened).toEqual([target]);
  expect(
    await sibling.evaluate(() => sessionStorage.getItem('observed-notification-route')),
  ).toBeNull();
});

test('real enrollment rotates cloned sessionStorage, preserves reload lineage and routes to the original owner', async ({
  context,
  page,
  baseURL,
}, testInfo) => {
  test.skip(
    testInfo.project.name === 'iphone',
    'Non-standalone iPhone browser push is unsupported',
  );
  await context.grantPermissions(['notifications'], { origin: new URL(baseURL!).origin });
  await context.addInitScript(() => {
    // Enrollment is entirely fake; registration and WindowClients are real.
    // Never create a real PushSubscription or contact a push provider.
    for (const [name, value] of Object.entries({
      TERM_LLM_VAPID_PUBLIC_KEY: 'AQID',
      TERM_LLM_PUSH_SUPPORTED: true,
      TERM_LLM_NOTIFICATION_VERSION: 2,
    }))
      Object.defineProperty(window, name, { configurable: true, get: () => value, set: () => {} });
    Object.defineProperty(PushManager.prototype, 'getSubscription', {
      configurable: true,
      value: async () => ({
        endpoint: 'https://push.invalid/disposable-test',
        options: { applicationServerKey: new Uint8Array([1, 2, 3]).buffer },
        toJSON: () => ({
          endpoint: 'https://push.invalid/disposable-test',
          keys: { p256dh: 'test', auth: 'test' },
        }),
        unsubscribe: async () => true,
      }),
    });
    navigator.serviceWorker.addEventListener('message', (event) => {
      if (event.data?.type !== 'notification-route') return;
      sessionStorage.setItem('observed-notification-route', event.data.url);
      event.stopImmediatePropagation();
    });
  });
  const leases: Array<{ page: Page; body: Record<string, unknown> }> = [];
  await context.route('**/v1/push/**', async (route) => {
    const request = route.request();
    const isPresence = new URL(request.url()).pathname.endsWith('/presence');
    if (isPresence) leases.push({ page: request.frame().page(), body: request.postDataJSON() });
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify(
        isPresence
          ? { ok: true, version: 2 }
          : { id: 'shared-test-subscription', state: 'active', vapid_key_id: 'test' },
      ),
    });
  });
  await page.goto('./');
  await expect.poll(() => leases.find((entry) => entry.page === page)?.body.window_id).toBeTruthy();
  const original = leases.find((entry) => entry.page === page)!.body;
  // Duplicate Tab clones sessionStorage but is a separate top-level client.
  // Reproduce that copy explicitly; window.open's initial about:blank container
  // has different SW lifecycle behavior in Chromium and is not Duplicate Tab.
  const copied = await page.evaluate(() =>
    Object.fromEntries(
      Object.keys(sessionStorage)
        .filter((key) => key.startsWith('term_llm_notification_'))
        .map((key) => [key, sessionStorage.getItem(key)!]),
    ),
  );
  const clone = await context.newPage();
  await clone.addInitScript((entries) => {
    for (const [key, value] of Object.entries(entries)) sessionStorage.setItem(key, value);
  }, copied);
  await clone.goto('./');
  await expect
    .poll(() => leases.find((entry) => entry.page === clone)?.body.window_id)
    .toBeTruthy();
  const cloned = leases.find((entry) => entry.page === clone)!.body;
  expect(cloned.window_id).not.toBe(original.window_id);
  expect(cloned.subscription_id).toBe(original.subscription_id);
  const target = new URL('chat/original-owner', baseURL).href;
  await clickInWorker(page, { kind: 'completion', window_id: original.window_id, url: target });
  await expect
    .poll(() => page.evaluate(() => sessionStorage.getItem('observed-notification-route')))
    .toBe(target);
  expect(
    await clone.evaluate(() => sessionStorage.getItem('observed-notification-route')),
  ).toBeNull();
  await page.reload();
  await expect
    .poll(
      () =>
        leases.find(
          (entry) => entry.page === page && entry.body.document_id !== original.document_id,
        )?.body,
    )
    .toMatchObject({
      window_id: original.window_id,
      previous_document_id: original.document_id,
    });
});
