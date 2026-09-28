import { expect, test, type Locator, type Page, type Route } from '@playwright/test';

const now = 1_800_000_000;

function session(id: string, title: string, minutesAgo: number, pinOrder?: number) {
  return {
    id,
    number: minutesAgo,
    short_title: title,
    name: title,
    mode: 'chat',
    origin: 'web',
    created_at: now - minutesAgo * 60,
    last_message_at: now - minutesAgo * 60,
    message_count: 1,
    pinned: pinOrder !== undefined,
    ...(pinOrder !== undefined ? { pin_order: pinOrder } : {}),
    archived: false,
    file_change_summary: { file_count: 0, adds: 0, dels: 0, git: false },
  };
}

/**
 * A fake server that persists pinned ranks the way the real one does: a
 * reorder moves the listed pins into the ranks they already hold. Saved ranks
 * deliberately disagree with activity (Gamma is the most recent pin).
 */
async function mockPinnedAPI(page: Page, extraPins = false): Promise<string[][]> {
  const ranks = new Map([
    ['pin-a', 1],
    ['pin-b', 2],
    ['pin-c', 3],
    ...(extraPins
      ? ([
          ['pin-d', 4],
          ['pin-e', 5],
        ] as const)
      : []),
  ]);
  const saved: string[][] = [];
  const sessions = () => [
    session('pin-a', 'Alpha pin', 30, ranks.get('pin-a')),
    session('pin-b', 'Beta pin', 20, ranks.get('pin-b')),
    session('pin-c', 'Gamma pin', 10, ranks.get('pin-c')),
    ...(extraPins
      ? [
          session('pin-d', 'Delta pin', 8, ranks.get('pin-d')),
          session('pin-e', 'Echo pin', 6, ranks.get('pin-e')),
        ]
      : []),
    session('regular', 'Regular chat', 1),
  ];
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    const json = (value: unknown) =>
      route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(value) });

    if (path.endsWith('/v1/sessions/pinned-order') && request.method() === 'PATCH') {
      const ids = (request.postDataJSON() as { session_ids: string[] }).session_ids;
      saved.push(ids);
      const slots = ids.map((id) => ranks.get(id) || 0).sort((left, right) => left - right);
      ids.forEach((id, index) => ranks.set(id, slots[index]));
      return json({
        pinned: [...ranks]
          .sort(([, left], [, right]) => left - right)
          .map(([id, pin_order]) => ({ id, pin_order })),
      });
    }
    if (path.endsWith('/v1/capabilities')) return json({ projects: { enabled: false } });
    if (path.endsWith('/v1/providers')) {
      return json({
        object: 'list',
        data: [
          {
            name: 'cleanroom',
            configured: true,
            is_default: true,
            default_model: 'fixture-model',
            models: ['fixture-model'],
          },
        ],
      });
    }
    if (path.endsWith('/v1/models')) {
      return json({ object: 'list', data: [{ id: 'fixture-model', owned_by: 'cleanroom' }] });
    }
    if (path.endsWith('/v1/sessions/status')) return json({ sessions: [] });
    if (path.endsWith('/v1/sessions') && url.searchParams.get('selected_only') === '1') {
      // Opening a conversation looks it up; an empty answer would drop it.
      const id = url.searchParams.get('selected_session');
      return json({
        selected_session: sessions().find((entry) => entry.id === id) || null,
        selected_transcript: { bodies: { rev: 1, messages: [] } },
      });
    }
    if (path.endsWith('/v1/sessions')) return json({ sessions: sessions() });
    if (/\/v1\/sessions\/[^/]+\/state$/.test(path)) return json({});
    if (/\/v1\/sessions\/[^/]+\/transcript$/.test(path)) return json({ messages: [] });
    return json({});
  });
  return saved;
}

const pinnedTitles = (page: Page) => page.locator('.sidebar-pinned-group .session-title');
const pinnedRow = (page: Page, title: string) =>
  page.locator('.sidebar-pinned-group .session-row', { hasText: title });

async function center(locator: Locator): Promise<{ x: number; y: number }> {
  const box = await locator.boundingBox();
  if (!box) throw new Error('element has no layout box');
  return { x: box.x + box.width / 2, y: box.y + box.height / 2 };
}

async function openApp(page: Page, extraPins = false): Promise<string[][]> {
  const saved = await mockPinnedAPI(page, extraPins);
  await page.goto('./');
  await expect(page.locator('#startupSplash')).toBeHidden({ timeout: 10_000 });
  return saved;
}

const conversation = (page: Page, title: string) =>
  page.getByRole('button', { name: title, exact: true });

test('drags pinned rows with the mouse, keeps clicks and menus working, and keeps the order', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'mouse reordering in the desktop sidebar');
  const saved = await openApp(page);
  await expect(pinnedTitles(page)).toHaveText(['Alpha pin', 'Beta pin', 'Gamma pin']);
  await expect(conversation(page, 'Gamma pin')).not.toHaveAttribute('aria-current', 'page');

  // The whole row is the drag surface; there is no separate grip.
  const start = await center(pinnedRow(page, 'Gamma pin'));
  const target = await pinnedRow(page, 'Alpha pin').boundingBox();
  await page.mouse.move(start.x, start.y);
  await page.mouse.down();
  await page.mouse.move(start.x, target!.y + 2, { steps: 12 });
  await expect(pinnedRow(page, 'Gamma pin')).toHaveClass(/is-dragging/);
  await page.mouse.up();

  await expect(pinnedTitles(page)).toHaveText(['Gamma pin', 'Alpha pin', 'Beta pin']);
  await expect.poll(() => saved).toEqual([['pin-c', 'pin-a', 'pin-b']]);
  // Releasing the dragged row does not also open the conversation.
  await expect(conversation(page, 'Gamma pin')).not.toHaveAttribute('aria-current', 'page');

  await page.reload();
  await expect(page.locator('#startupSplash')).toBeHidden({ timeout: 10_000 });
  await expect(pinnedTitles(page)).toHaveText(['Gamma pin', 'Alpha pin', 'Beta pin']);

  await conversation(page, 'Alpha pin').focus();
  await page.keyboard.press('Alt+ArrowUp');
  await expect(pinnedTitles(page)).toHaveText(['Alpha pin', 'Gamma pin', 'Beta pin']);
  await expect(conversation(page, 'Alpha pin')).toBeFocused();
  await expect.poll(() => saved.at(-1)).toEqual(['pin-a', 'pin-c', 'pin-b']);

  // The row's actions menu still opens on click and moves the pin.
  await page.getByRole('button', { name: 'Actions for Gamma pin' }).click();
  await page.getByRole('menuitem', { name: 'Move down' }).click();
  await expect(pinnedTitles(page)).toHaveText(['Alpha pin', 'Beta pin', 'Gamma pin']);
  await expect.poll(() => saved.at(-1)).toEqual(['pin-a', 'pin-b', 'pin-c']);

  // A plain click on a pinned row opens it.
  await expect(conversation(page, 'Beta pin')).not.toHaveAttribute('aria-current', 'page');
  await conversation(page, 'Beta pin').click();
  await expect(conversation(page, 'Beta pin')).toHaveAttribute('aria-current', 'page');
  expect(saved).toHaveLength(3);
});

test('commits a fast first-to-fifth drop without waiting for the row animation', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'mouse reordering in the desktop sidebar');
  const saved = await openApp(page, true);
  await expect(pinnedTitles(page)).toHaveText([
    'Alpha pin',
    'Beta pin',
    'Gamma pin',
    'Delta pin',
    'Echo pin',
  ]);
  const start = await center(pinnedRow(page, 'Alpha pin'));
  const end = await center(pinnedRow(page, 'Echo pin'));
  await page.mouse.move(start.x, start.y);
  await page.mouse.down();
  // No step-wise movement, assertion, or dwell between moving and dropping.
  await page.mouse.move(start.x, end.y);
  await page.mouse.up();
  await expect(pinnedTitles(page)).toHaveText([
    'Beta pin',
    'Gamma pin',
    'Delta pin',
    'Echo pin',
    'Alpha pin',
  ]);
  await expect.poll(() => saved).toEqual([['pin-b', 'pin-c', 'pin-d', 'pin-e', 'pin-a']]);
  await page.reload();
  await expect(page.locator('#startupSplash')).toBeHidden({ timeout: 10_000 });
  await expect(pinnedTitles(page)).toHaveText([
    'Beta pin',
    'Gamma pin',
    'Delta pin',
    'Echo pin',
    'Alpha pin',
  ]);
});

test('commits a fast drop even if the browser releases pointer capture mid-drag', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'mouse reordering in the desktop sidebar');
  const saved = await openApp(page, true);
  await expect(pinnedTitles(page)).toHaveText([
    'Alpha pin',
    'Beta pin',
    'Gamma pin',
    'Delta pin',
    'Echo pin',
  ]);
  const start = await center(pinnedRow(page, 'Echo pin'));
  const middle = await center(pinnedRow(page, 'Gamma pin'));
  const end = await center(pinnedRow(page, 'Alpha pin'));
  await page.mouse.move(start.x, start.y);
  await page.mouse.down();
  await page.mouse.move(start.x, middle.y);
  await expect(pinnedRow(page, 'Echo pin')).toHaveClass(/is-dragging/);
  // Reproduce the user's trace: capture disappears before mouseup, but the
  // pointer is still pressed and window still receives its release.
  await pinnedRow(page, 'Echo pin').evaluate((row) => {
    if (!row.hasPointerCapture(1)) throw new Error('drag did not capture pointer 1');
    row.releasePointerCapture(1);
  });
  await page.mouse.move(start.x, end.y);
  await page.mouse.up();
  await expect(pinnedTitles(page)).toHaveText([
    'Echo pin',
    'Alpha pin',
    'Beta pin',
    'Gamma pin',
    'Delta pin',
  ]);
  await expect.poll(() => saved).toEqual([['pin-e', 'pin-a', 'pin-b', 'pin-c', 'pin-d']]);
});

test('lifts a pinned row by long press in the mobile drawer; quick swipes and taps still work', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name === 'desktop', 'touch reordering in the mobile drawer');
  const saved = await openApp(page);
  await page.getByRole('button', { name: 'Open sidebar' }).click();
  const drawer = page.getByRole('dialog', { name: 'Sessions' });
  await expect(drawer).toBeVisible();
  await expect(pinnedTitles(page)).toHaveText(['Alpha pin', 'Beta pin', 'Gamma pin']);
  // Touch points are measured once the drawer has finished sliding in.
  await expect(page.locator('#sidebar')).toHaveCSS('transform', 'matrix(1, 0, 0, 1, 0, 0)');
  // The page clock decides when a resting touch has become a long press.
  await page.clock.install();

  // Real touch input through the browser's gesture handling: nothing but the
  // lifted row may stop the list from scrolling or the drawer from swiping.
  const client = await page.context().newCDPSession(page);
  const touch = (type: 'touchStart' | 'touchMove' | 'touchEnd', x: number, y: number) =>
    client.send('Input.dispatchTouchEvent', {
      type,
      touchPoints: type === 'touchEnd' ? [] : [{ x, y }],
    });
  type Point = { x: number; y: number };
  const slide = async (...path: Point[]) => {
    for (let leg = 1; leg < path.length; leg += 1) {
      const [from, to] = [path[leg - 1], path[leg]];
      for (let step = 1; step <= 8; step += 1) {
        await touch(
          'touchMove',
          from.x + ((to.x - from.x) * step) / 8,
          from.y + ((to.y - from.y) * step) / 8,
        );
      }
    }
    await touch('touchEnd', path.at(-1)!.x, path.at(-1)!.y);
  };
  // Resting first lifts the row, which then follows the finger, even after a
  // leftward slide that would otherwise swipe the drawer closed.
  const dragStart = await center(pinnedRow(page, 'Gamma pin'));
  const dragEndY = (await pinnedRow(page, 'Alpha pin').boundingBox())!.y + 2;
  await touch('touchStart', dragStart.x, dragStart.y);
  await page.clock.runFor(500);
  await expect(pinnedRow(page, 'Gamma pin')).toHaveClass(/is-dragging/);
  await slide(
    dragStart,
    { x: dragStart.x - 80, y: dragStart.y },
    { x: dragStart.x - 80, y: dragEndY },
  );
  await expect(pinnedTitles(page)).toHaveText(['Gamma pin', 'Alpha pin', 'Beta pin']);
  await expect.poll(() => saved).toEqual([['pin-c', 'pin-a', 'pin-b']]);
  await expect(drawer).toBeVisible();
  await expect(page.locator('#sidebar')).toHaveClass(/\bopen\b/);
  await expect(conversation(page, 'Gamma pin')).not.toHaveAttribute('aria-current', 'page');

  // A tap still opens a pinned conversation.
  await expect(conversation(page, 'Beta pin')).not.toHaveAttribute('aria-current', 'page');
  await conversation(page, 'Beta pin').tap();
  await expect(conversation(page, 'Beta pin')).toHaveAttribute('aria-current', 'page');
  expect(saved).toHaveLength(1);

  // Test the quick swipe after the drag: starting a second touch while native
  // momentum scrolling from an earlier swipe is still in flight is ambiguous.
  // Without the long press, sliding a pinned row must not reorder it.
  const swipeStart = await center(pinnedRow(page, 'Gamma pin'));
  const swipeEndY = (await pinnedRow(page, 'Alpha pin').boundingBox())!.y + 2;
  await touch('touchStart', swipeStart.x, swipeStart.y);
  await slide(swipeStart, { x: swipeStart.x, y: swipeEndY });
  await expect(pinnedRow(page, 'Gamma pin')).not.toHaveClass(/is-dragging/);
  await expect(pinnedTitles(page)).toHaveText(['Gamma pin', 'Alpha pin', 'Beta pin']);
  expect(saved).toHaveLength(1);
});
