import {
  expect,
  request as playwrightRequest,
  test,
  type APIRequestContext,
  type Locator,
  type Page,
} from '@playwright/test';

// Runs against the production-shaped Hub that scripts/hub_browser_lifecycle_smoke.sh
// starts: the real "production-node" plus two config nodes the Hub cannot reach,
// each with its own serve token.
const hubRoot = process.env.TERM_LLM_HUB_SMOKE_ROOT || '';
const hubToken = process.env.TERM_LLM_HUB_SMOKE_TOKEN || '';
const nodeToken = process.env.TERM_LLM_HUB_NODE_TOKEN || '';
const secrets = [hubToken, nodeToken, 'order-alpha-token', 'order-beta-token'];

const production = 'production-node';
const alpha = 'order-alpha';
const beta = 'order-beta';
const known = [production, alpha, beta];
const names: Record<string, string> = {
  [production]: 'Production Node',
  [alpha]: 'Order Alpha',
  [beta]: 'Order Beta',
};

/** The known node IDs, in the order a list shows them; other nodes are ignored. */
const knownOrder = (ids: readonly string[]) => ids.filter((id) => known.includes(id));

async function api(headers: Record<string, string>): Promise<APIRequestContext> {
  return playwrightRequest.newContext({ extraHTTPHeaders: headers });
}

/** An operator API client authenticated with the Hub bearer token. */
const operatorAPI = () => api({ Authorization: `Bearer ${hubToken}` });

async function saveOrder(client: APIRequestContext, ids: string[], headers = {}) {
  return client.patch(`${hubRoot}api/nodes/order`, {
    headers: { 'Content-Type': 'application/json', ...headers },
    data: { node_ids: ids },
  });
}

async function expectNoSecrets(body: string) {
  for (const secret of secrets) expect(body).not.toContain(secret);
}

/** Starts every test from the same order, whatever earlier projects saved. */
async function resetOrder() {
  const operator = await operatorAPI();
  const response = await saveOrder(operator, known);
  expect(response.status()).toBe(200);
  const body = (await response.json()) as { node_ids: string[] };
  expect(knownOrder(body.node_ids)).toEqual(known);
  await operator.dispose();
}

async function openDashboard(page: Page) {
  await page.goto(`${hubRoot}?token=${encodeURIComponent(hubToken)}`);
  await expect(page).toHaveURL(new RegExp(`${hubRoot.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`));
  await expect(page.locator('.node-grid > .node-card')).not.toHaveCount(0);
}

/** The known node cards' IDs, in dashboard order. */
async function shownOrder(page: Page): Promise<string[]> {
  return knownOrder(
    await page
      .locator('.node-grid > .node-card')
      .evaluateAll((cards) => cards.map((card) => (card as HTMLElement).dataset.reorderId || '')),
  );
}

const expectShown = (page: Page, ids: string[]) =>
  expect.poll(() => shownOrder(page), { timeout: 10_000 }).toEqual(ids);

const nodeCard = (page: Page, id: string) => page.locator(`.node-card[data-reorder-id="${id}"]`);
const nodeHeader = (page: Page, id: string) => nodeCard(page, id).locator('.node-card-head');

async function box(locator: Locator) {
  const rect = await locator.boundingBox();
  if (!rect) throw new Error('element has no layout box');
  return rect;
}

async function center(locator: Locator) {
  const rect = await box(locator);
  return { x: rect.x + rect.width / 2, y: rect.y + rect.height / 2 };
}

/** Waits for the dashboard's next saved order and returns what it sent and got back. */
async function nextSave(page: Page, move: () => Promise<void>) {
  const [response] = await Promise.all([
    page.waitForResponse(
      (candidate) =>
        new URL(candidate.url()).pathname.endsWith('/api/nodes/order') &&
        candidate.request().method() === 'PATCH',
    ),
    move(),
  ]);
  expect(response.status()).toBe(200);
  const sent = response.request().postDataJSON() as { node_ids: string[] };
  const body = await response.text();
  await expectNoSecrets(body);
  return { sent: knownOrder(sent.node_ids), committed: knownOrder(JSON.parse(body).node_ids) };
}

const expectNear = (actual: number, expected: number, message: string) =>
  expect(Math.abs(actual - expected), `${message}: ${actual} vs ${expected}`).toBeLessThan(1.5);

test.describe('Hub node order', () => {
  test.skip(
    !hubRoot || !hubToken || !nodeToken,
    'production-shaped Hub node order smoke is not configured',
  );

  test('drags cards by their header across grid rows and moves them by keyboard and menu, across reloads', async ({
    page,
  }, testInfo) => {
    test.skip(testInfo.project.name !== 'desktop', 'mouse and keyboard reordering on desktop');
    test.setTimeout(60_000);
    await resetOrder();
    // Two columns: Production and Alpha share the first row; Beta starts the second.
    await page.setViewportSize({ width: 900, height: 1000 });
    // Cards slide without animating, so the preview can be measured at once.
    await page.emulateMedia({ reducedMotion: 'reduce' });
    await openDashboard(page);
    await expectShown(page, [production, alpha, beta]);
    const before = {
      production: await box(nodeCard(page, production)),
      alpha: await box(nodeCard(page, alpha)),
      beta: await box(nodeCard(page, beta)),
    };
    expectNear(before.alpha.y, before.production.y, 'two cards share the first row');
    expect(before.beta.y).toBeGreaterThan(before.production.y + before.production.height);

    // Drag Beta by its header up into Production's cell.
    const start = await center(nodeHeader(page, beta));
    const target = await center(nodeHeader(page, production));
    await page.mouse.move(start.x, start.y);
    await page.mouse.down();
    await page.mouse.move(target.x, target.y, { steps: 12 });
    await expect(nodeCard(page, beta)).toHaveClass(/is-dragging/);
    // The cards it passes slide one cell along, across the row end, to open
    // the cell it will land in. They are the whole preview: nothing else is
    // added to the grid and no card draws an insertion line.
    await expect
      .poll(async () => (await box(nodeCard(page, production))).x)
      .toBeCloseTo(before.alpha.x, 0);
    expectNear((await box(nodeCard(page, alpha))).x, before.beta.x, 'Alpha preview x');
    expectNear((await box(nodeCard(page, alpha))).y, before.beta.y, 'Alpha preview y');
    const lifted = await box(nodeCard(page, beta));
    expectNear(lifted.x, before.production.x, 'lifted card x');
    expect(
      await page.locator('.node-grid').evaluate((grid) => ({
        children: grid.children.length,
        pseudo: [grid, ...grid.children].flatMap((element) =>
          ['::before', '::after']
            .map((pseudo) => getComputedStyle(element, pseudo).content)
            .filter((content) => content && content !== 'none' && content !== 'normal'),
        ),
      })),
    ).toEqual({ children: 3, pseudo: [] });
    const dropped = await nextSave(page, () => page.mouse.up());
    expect(dropped.sent).toEqual([beta, production, alpha]);
    expect(dropped.committed).toEqual([beta, production, alpha]);
    await expectShown(page, [beta, production, alpha]);
    await expect(nodeCard(page, beta)).not.toHaveClass(/is-dragging/);
    await expect(page.getByRole('status').filter({ hasText: 'Moved Order Beta' })).toHaveText(
      'Moved Order Beta to position 1 of 3.',
    );

    await page.reload();
    await expectShown(page, [beta, production, alpha]);

    // Alt+ArrowUp moves a card from any of its controls, which keeps focus.
    const alphaMenu = page.getByRole('button', { name: `More actions for ${names[alpha]}` });
    await alphaMenu.focus();
    const raised = await nextSave(page, () => page.keyboard.press('Alt+ArrowUp'));
    expect(raised.sent).toEqual([alpha, production]);
    expect(raised.committed).toEqual([beta, alpha, production]);
    await expectShown(page, [beta, alpha, production]);
    await expect(alphaMenu).toBeFocused();

    // The card menu moves it too.
    await page.getByRole('button', { name: `More actions for ${names[beta]}` }).click();
    await expect(page.getByRole('menuitem', { name: 'Move earlier' })).toHaveCount(0);
    const lowered = await nextSave(page, () =>
      page.getByRole('menuitem', { name: 'Move later' }).click(),
    );
    expect(lowered.sent).toEqual([alpha, beta]);
    expect(lowered.committed).toEqual([alpha, beta, production]);
    await expectShown(page, [alpha, beta, production]);

    await page.reload();
    await expectShown(page, [alpha, beta, production]);
    const listing = await page.request.get(`${hubRoot}api/nodes`);
    const listed = (await listing.json()) as { nodes: Array<{ id: string }> };
    expect(knownOrder(listed.nodes.map((node) => node.id))).toEqual([alpha, beta, production]);
    await expectNoSecrets(JSON.stringify(listed));
  });

  test('synchronizes the node chat sidebar with Hub order and saves moves from its links', async ({
    page,
  }, testInfo) => {
    test.skip(testInfo.project.name !== 'desktop', 'sidebar pointer and keyboard on desktop');
    test.setTimeout(60_000);
    await resetOrder();
    await openDashboard(page);
    // The fixture's two extra nodes have no live backend. Mark them reachable
    // in this browser's GET response so the chat sidebar can show all three,
    // while PATCH still goes to the real Hub and persists its actual order.
    await page.route('**/hub/api/nodes', async (route) => {
      const response = await route.fetch();
      const data = (await response.json()) as {
        nodes: Array<{ id: string; status: { reachable: boolean } }>;
      };
      await route.fulfill({
        response,
        json: {
          ...data,
          nodes: data.nodes.map((node) =>
            known.includes(node.id)
              ? { ...node, status: { ...node.status, reachable: true } }
              : node,
          ),
        },
      });
    });
    await page.goto(`${hubRoot}node/${production}/?new=1`);
    await expect(page.getByRole('textbox', { name: 'Message' })).toBeVisible();
    const agentRows = () => page.locator('.hub-agent-links > .hub-agent-row');
    const agentLink = (id: string) => page.locator(`.hub-agent-row[data-reorder-id="${id}"] a`);
    const shown = () =>
      agentRows().evaluateAll((rows) =>
        rows.map((row) => (row as HTMLElement).dataset.reorderId || ''),
      );
    await expect.poll(shown).toEqual(known);

    // Keyboard movement from the link preserves focus and changes Hub order.
    const betaLink = agentLink(beta);
    await betaLink.focus();
    const raised = await nextSave(page, () => page.keyboard.press('Alt+ArrowUp'));
    expect(raised.sent).toEqual([beta, alpha]);
    expect(raised.committed).toEqual([production, beta, alpha]);
    await expect.poll(shown).toEqual([production, beta, alpha]);
    await expect(betaLink).toBeFocused();

    // The link itself is the drag surface; a drop saves without navigating.
    const start = await center(betaLink);
    const end = await center(agentLink(production));
    await page.mouse.move(start.x, start.y);
    await page.mouse.down();
    await page.mouse.move(end.x, end.y, { steps: 10 });
    await expect(page.locator(`.hub-agent-row[data-reorder-id="${beta}"]`)).toHaveClass(
      /is-dragging/,
    );
    const dragged = await nextSave(page, () => page.mouse.up());
    expect(dragged.sent).toEqual([beta, production]);
    expect(dragged.committed).toEqual([beta, production, alpha]);
    await expect.poll(shown).toEqual([beta, production, alpha]);
    await expect(page).toHaveURL(new RegExp(`/node/${production}/`));

    await page.getByRole('button', { name: `More actions for ${names[alpha]}` }).click();
    const earlier = await nextSave(page, () =>
      page.getByRole('menuitem', { name: 'Move earlier' }).click(),
    );
    expect(earlier.committed).toEqual([beta, alpha, production]);

    // A link away from this node must wait for an in-flight reorder; otherwise
    // page teardown could cancel the PATCH before it reaches the Hub.
    let releaseSave!: () => void;
    const heldSave = new Promise<void>((resolve) => (releaseSave = resolve));
    await page.route('**/hub/api/nodes/order', async (route) => {
      await heldSave;
      await route.continue();
    });
    await agentLink(alpha).focus();
    const pendingResponse = page.waitForResponse(
      (response) =>
        new URL(response.url()).pathname.endsWith('/api/nodes/order') &&
        response.request().method() === 'PATCH',
    );
    await page.keyboard.press('Alt+ArrowDown');
    await page.getByRole('link', { name: 'Back to Hub' }).click();
    await expect(page).toHaveURL(new RegExp(`/node/${production}/`));
    releaseSave();
    const pending = await pendingResponse;
    expect(pending.status()).toBe(200);
    await expectShown(page, [beta, production, alpha]);

    await page.goto(`${hubRoot}node/${production}/?new=1`);
    await expect.poll(shown).toEqual([beta, production, alpha]);
    await page.reload();
    await expect.poll(shown).toEqual([beta, production, alpha]);
    await page.getByRole('link', { name: 'Back to Hub' }).click();
    await expectShown(page, [beta, production, alpha]);
  });

  test('shows an order a node saved with its own token on the next poll; node tokens reach nothing else', async ({
    page,
  }, testInfo) => {
    test.skip(testInfo.project.name !== 'desktop', 'the protocol is covered once');
    test.setTimeout(60_000);
    await resetOrder();
    // The page clock decides when the dashboard polls.
    await page.clock.install();
    await openDashboard(page);
    await expectShown(page, [production, alpha, beta]);

    const node = await api({
      'X-Term-LLM-Node-ID': production,
      Authorization: `Bearer ${nodeToken}`,
    });
    // A node lists the IDs it moves; they swap into the slots they occupy.
    const saved = await saveOrder(node, [beta, production]);
    expect(saved.status()).toBe(200);
    const savedBody = await saved.text();
    await expectNoSecrets(savedBody);
    expect(knownOrder(JSON.parse(savedBody).node_ids)).toEqual([beta, alpha, production]);
    // Idempotent: repeating the request commits the same order.
    const repeated = await saveOrder(node, [beta, production]);
    expect(knownOrder(((await repeated.json()) as { node_ids: string[] }).node_ids)).toEqual([
      beta,
      alpha,
      production,
    ]);

    await expectShown(page, [production, alpha, beta]);
    await page.clock.runFor(15_000);
    await expectShown(page, [beta, alpha, production]);

    // The node's token authorizes only its own order changes.
    const rejected = [
      await saveOrder(node, [alpha, beta], { 'X-Term-LLM-Node-ID': alpha }),
      await saveOrder(node, [alpha, beta], { Authorization: 'Bearer wrong-token' }),
      await saveOrder(node, [alpha, beta], { 'X-Term-LLM-Node-ID': 'no-such-node' }),
      await node.get(`${hubRoot}api/nodes`),
      await node.get(`${hubRoot}api/registration-info`),
      await node.post(`${hubRoot}api/nodes/test`, {
        headers: { 'Content-Type': 'application/json' },
        data: { url: 'http://127.0.0.1:1/chat' },
      }),
      await node.delete(`${hubRoot}api/nodes/${alpha}`),
      await node.get(`${hubRoot}node/${production}/`),
    ];
    const bearerOnly = await api({ Authorization: `Bearer ${nodeToken}` });
    const anonymous = await api({});
    rejected.push(
      await saveOrder(bearerOnly, [alpha, beta]),
      await saveOrder(anonymous, [alpha, beta]),
    );
    for (const response of rejected) {
      expect(response.status(), response.url()).toBe(401);
      await expectNoSecrets(await response.text());
    }
    const crossSite = await saveOrder(node, [alpha, beta], {
      Origin: 'https://evil.example',
      'Sec-Fetch-Site': 'cross-site',
    });
    expect(crossSite.status()).toBe(403);

    await page.reload();
    await expectShown(page, [beta, alpha, production]);
    await Promise.all([node.dispose(), bearerOnly.dispose(), anonymous.dispose()]);
  });

  test('lifts a card by a long press on its header on touch; quick swipes still scroll', async ({
    page,
  }, testInfo) => {
    test.skip(testInfo.project.name === 'desktop', 'touch reordering on phones');
    test.setTimeout(60_000);
    await resetOrder();
    let saves = 0;
    page.on('request', (sent) => {
      if (sent.method() === 'PATCH' && new URL(sent.url()).pathname.endsWith('/api/nodes/order'))
        saves += 1;
    });
    await openDashboard(page);
    await expectShown(page, [production, alpha, beta]);
    // Phones show one column. Bring Production's header just below the sticky
    // Hub header so Alpha's header is on screen too.
    const headerHeight = (await box(page.locator('.hub-header'))).height;
    await nodeCard(page, production).evaluate((card, offset) => {
      window.scrollBy(0, card.getBoundingClientRect().top - offset - 8);
    }, headerHeight);
    await expect(nodeHeader(page, production)).toBeInViewport();
    await expect(nodeHeader(page, alpha)).toBeInViewport();
    // The page clock decides when a resting touch has become a long press.
    await page.clock.install();

    // Real touch input through the browser's gesture handling.
    const client = await page.context().newCDPSession(page);
    const touch = (type: 'touchStart' | 'touchMove' | 'touchEnd', x: number, y: number) =>
      client.send('Input.dispatchTouchEvent', {
        type,
        touchPoints: type === 'touchEnd' ? [] : [{ x, y }],
      });
    const slide = async (from: { x: number; y: number }, to: { x: number; y: number }) => {
      for (let step = 1; step <= 10; step += 1)
        await touch(
          'touchMove',
          from.x + ((to.x - from.x) * step) / 10,
          from.y + ((to.y - from.y) * step) / 10,
        );
      await touch('touchEnd', to.x, to.y);
    };

    const start = await center(nodeHeader(page, alpha));
    const end = { x: start.x, y: (await box(nodeHeader(page, production))).y + 2 };
    await touch('touchStart', start.x, start.y);
    await page.clock.runFor(500);
    await expect(nodeCard(page, alpha)).toHaveClass(/is-dragging/);
    const dropped = await nextSave(page, () => slide(start, end));
    expect(dropped.committed).toEqual([alpha, production, beta]);
    await expectShown(page, [alpha, production, beta]);

    expect(saves).toBe(1);

    // Without the rest, sliding a header is an ordinary scroll: it never
    // lifts or moves the card.
    const swipe = await center(nodeHeader(page, production));
    await touch('touchStart', swipe.x, swipe.y);
    await slide(swipe, { x: swipe.x, y: (await box(nodeHeader(page, alpha))).y + 2 });
    await page.clock.runFor(500);
    await expect(nodeCard(page, production)).not.toHaveClass(/is-dragging/);
    await expectShown(page, [alpha, production, beta]);
    expect(saves).toBe(1);

    await page.reload();
    await expectShown(page, [alpha, production, beta]);
  });
});
