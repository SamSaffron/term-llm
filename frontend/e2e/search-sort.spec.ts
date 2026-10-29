import { expect, test, type Page } from '@playwright/test';

const sessions = [
  { id: 'best', short_title: 'Best match', last_message_at: 1_800_000_200_000 },
  { id: 'old', short_title: 'Oldest activity', last_message_at: 1_800_000_100_000 },
  { id: 'new', short_title: 'Newest activity', last_message_at: 1_800_000_300_000 },
].map((entry, index) => ({
  ...entry,
  number: index + 1,
  mode: 'chat',
  origin: 'web',
  created_at: 1_800_000_000_000,
  message_count: 1,
  snippet: 'A matching search phrase',
}));

// Reusable deterministic search fixture for desktop/mobile visual inspection.
async function mockSearchAPI(page: Page) {
  const requests: URLSearchParams[] = [];
  await page.route('**/v1/**', async (route) => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    const json = (value: unknown) =>
      route.fulfill({ contentType: 'application/json', body: JSON.stringify(value) });
    if (path.endsWith('/v1/capabilities')) return json({ projects: { enabled: false } });
    if (path.endsWith('/v1/providers'))
      return json({
        data: [
          {
            name: 'debug',
            configured: true,
            is_default: true,
            default_model: 'fast',
            models: ['fast'],
          },
        ],
      });
    if (path.endsWith('/v1/models')) return json({ data: [{ id: 'fast', owned_by: 'debug' }] });
    if (path.endsWith('/v1/sessions/search')) {
      requests.push(url.searchParams);
      const sort = url.searchParams.get('sort');
      return json({
        sessions:
          sort === 'newest'
            ? [sessions[2], sessions[0], sessions[1]]
            : sort === 'oldest'
              ? [sessions[1], sessions[0], sessions[2]]
              : sessions,
      });
    }
    if (path.endsWith('/v1/sessions/status')) return json({ sessions: [] });
    if (path.endsWith('/v1/sessions')) return json({ sessions });
    return json({});
  });
  return requests;
}

async function openSearch(page: Page, mobile: boolean) {
  const requests = await mockSearchAPI(page);
  await page.goto('./');
  await expect(page.locator('#startupSplash')).toBeHidden();
  if (mobile) await page.getByRole('button', { name: 'Open sidebar', exact: true }).click();
  await page.getByRole('searchbox', { name: 'Search chats' }).fill('matching phrase');
  await expect(page.locator('#sessionGroups .session-title')).toHaveText([
    'Best match',
    'Oldest activity',
    'Newest activity',
  ]);
  return requests;
}

test('search sort menu orders server requests and supports keyboard or mobile touch', async ({
  page,
}, testInfo) => {
  const mobile = testInfo.project.name === 'mobile';
  const requests = await openSearch(page, mobile);
  expect(requests[0].has('sort')).toBe(false);
  const trigger = page.getByRole('button', { name: /^Sort search results:/ });
  const menu = page.getByRole('menu', { name: 'Sort search results', exact: true });
  if (mobile) {
    const box = await trigger.boundingBox();
    expect(box!.width).toBeGreaterThanOrEqual(44);
    expect(box!.height).toBeGreaterThanOrEqual(44);
    await trigger.tap();
  } else {
    await trigger.focus();
    await page.keyboard.press('ArrowDown');
  }
  const relevance = page.getByRole('menuitemradio', { name: 'Relevance', exact: true });
  await expect(relevance).toHaveAttribute('aria-checked', 'true');
  await expect(relevance).toBeFocused();
  if (mobile) {
    const newest = page.getByRole('menuitemradio', { name: 'Newest first' });
    expect((await newest.boundingBox())!.height).toBeGreaterThanOrEqual(44);
    await newest.tap();
  } else {
    await page.keyboard.press('ArrowDown');
    await page.keyboard.press('Enter');
  }
  await expect(menu).toBeHidden();
  await expect(trigger).toBeFocused();
  await expect(page.locator('#sessionGroups .session-title')).toHaveText([
    'Newest activity',
    'Best match',
    'Oldest activity',
  ]);
  expect(requests.at(-1)!.get('sort')).toBe('newest');
  expect(requests.at(-1)!.get('q')).toBe('matching phrase');
  await trigger.click();
  await expect(page.getByRole('menuitemradio', { name: 'Newest first' })).toHaveAttribute(
    'aria-checked',
    'true',
  );
  await page.keyboard.press('Escape');
  await expect(menu).toBeHidden();
  await expect(trigger).toBeFocused();
  if (mobile) await expect(page.locator('#sidebar')).toHaveClass(/open/);
  await trigger.click();
  await page.getByRole('menuitemradio', { name: 'Oldest first' }).click();
  await expect(page.locator('#sessionGroups .session-title')).toHaveText([
    'Oldest activity',
    'Best match',
    'Newest activity',
  ]);
  expect(requests.at(-1)!.get('sort')).toBe('oldest');
  await trigger.click();
  await relevance.click();
  await expect(page.locator('#sessionGroups .session-title').first()).toHaveText('Best match');
  expect(requests.at(-1)!.has('sort')).toBe(false);
  await trigger.click();
  await page.getByRole('searchbox', { name: 'Search chats' }).click();
  await expect(menu).toBeHidden();
});

test('search sort menu stays reachable in a short desktop or narrow mobile sidebar', async ({
  page,
}, testInfo) => {
  const mobile = testInfo.project.name === 'mobile';
  await page.setViewportSize({ width: mobile ? 360 : 1100, height: 340 });
  await openSearch(page, mobile);
  await page.getByRole('button', { name: /^Sort search results:/ }).click();
  const menu = page.getByRole('menu', { name: 'Sort search results', exact: true });
  await expect(menu).toBeVisible();
  const box = (await menu.boundingBox())!;
  const scrollport = (await page.locator('#sidebarContent').boundingBox())!;
  expect(box.x).toBeGreaterThanOrEqual(scrollport.x);
  expect(box.y).toBeGreaterThanOrEqual(scrollport.y);
  expect(box.x + box.width).toBeLessThanOrEqual(scrollport.x + scrollport.width + 1);
  expect(box.y + box.height).toBeLessThanOrEqual(scrollport.y + scrollport.height + 1);
  expect(box.x + box.width).toBeLessThanOrEqual(page.viewportSize()!.width);
  expect(box.y + box.height).toBeLessThanOrEqual(page.viewportSize()!.height);
  const oldest = page.getByRole('menuitemradio', { name: 'Oldest first' });
  await oldest.scrollIntoViewIfNeeded();
  await oldest.click();
  await expect(menu).toBeHidden();
  await expect(page.locator('#sessionGroups .session-title').first()).toHaveText('Oldest activity');
});
