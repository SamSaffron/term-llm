import { test, expect, type Page } from '@playwright/test';

const yesterday = Date.parse('2026-11-03T12:00:00Z') / 1000;
const session = (id: string, pinned: boolean) => ({
  id,
  name: id,
  mode: 'chat',
  origin: 'web',
  created_at: yesterday,
  last_message_at: yesterday,
  message_count: 2,
  pinned,
  archived: false,
});

async function mockPinnedFirstPage(page: Page, projects: boolean) {
  const pins = Array.from({ length: 30 }, (_, index) => session(`Pinned chat ${index}`, true));
  const older = session('Yesterday chat', false);
  let pageCalls = 0;
  await page.clock.setFixedTime('2026-11-04T12:00:00Z');
  await page.route('**/v1/**', async (route) => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    const json = (value: unknown) => route.fulfill({ json: value });
    if (path.endsWith('/v1/capabilities')) return json({ projects: { enabled: projects } });
    if (path.endsWith('/v1/sidebar'))
      return json({
        groups: [{ no_project: true, sessions: pins, next_cursor: 'older-page' }],
        recent_sessions: pins,
      });
    if (path.endsWith('/v1/sessions') && url.searchParams.get('selected_only') === '1')
      return json({ selected_session: pins[0], selected_transcript: { bodies: { messages: [] } } });
    if (path.endsWith('/v1/sessions')) {
      if (url.searchParams.get('cursor') === 'older-page') {
        pageCalls += 1;
        return json({ sessions: [older], next_cursor: '' });
      }
      return json({ sessions: pins, next_cursor: 'older-page' });
    }
    if (path.endsWith('/v1/sessions/status')) return json({ sessions: [] });
    return json({});
  });
  return () => pageCalls;
}

for (const projects of [false, true]) {
  test(`loads Yesterday with only pins and no Today (projects: ${projects})`, async ({
    page,
  }, testInfo) => {
    const pageCalls = await mockPinnedFirstPage(page, projects);
    await page.goto('./');
    if (testInfo.project.name === 'mobile')
      await page.getByRole('button', { name: 'Open sidebar' }).click();
    if (projects) await page.getByRole('tab', { name: 'Projects', exact: true }).click();
    await expect(page.getByRole('button', { name: 'Pinned chat 29', exact: true })).toBeAttached();
    const sidebar = page.locator('.sidebar-content');
    await sidebar.evaluate((element) => {
      element.scrollTop = element.scrollHeight;
    });
    await expect(page.getByRole('button', { name: 'Yesterday chat', exact: true })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Yesterday', exact: true })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Today', exact: true })).toHaveCount(0);
    expect(pageCalls()).toBe(1);
    await expect(page.locator('.project-pagination-sentinel')).toHaveCount(0);
  });
}

test('periodic sidebar refresh does not rewind loaded no-project pages', async ({
  page,
}, testInfo) => {
  const pins = Array.from({ length: 30 }, (_, index) => session(`Pinned chat ${index}`, true));
  const pages: Record<string, { sessions: unknown[]; next_cursor: string }> = {
    'page-2': { sessions: [session('Page two chat', false)], next_cursor: 'page-3' },
    'page-3': { sessions: [session('Page three chat', false)], next_cursor: '' },
  };
  const cursorCalls: string[] = [];
  let sidebarCalls = 0;
  await page.clock.install({ time: new Date('2026-11-04T12:00:00Z') });
  await page.route('**/v1/**', async (route) => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    const json = (value: unknown) => route.fulfill({ json: value });
    if (path.endsWith('/v1/capabilities')) return json({ projects: { enabled: true } });
    if (path.endsWith('/v1/sidebar')) {
      sidebarCalls += 1;
      // Later snapshots rename one row so the test can tell when one was applied.
      const first = sidebarCalls === 1 ? pins : [session('Refreshed chat', true), ...pins.slice(1)];
      return json({
        groups: [{ no_project: true, sessions: first, next_cursor: 'page-2' }],
        recent_sessions: first,
      });
    }
    if (path.endsWith('/v1/sessions') && url.searchParams.get('selected_only') === '1')
      return json({ selected_session: pins[0], selected_transcript: { bodies: { messages: [] } } });
    if (path.endsWith('/v1/sessions') && url.searchParams.get('no_project') === '1') {
      const cursor = url.searchParams.get('cursor') || '';
      cursorCalls.push(cursor);
      return json(pages[cursor] || { sessions: [], next_cursor: '' });
    }
    if (path.endsWith('/v1/sessions/status')) return json({ sessions: [] });
    return json({});
  });
  await page.goto('./');
  if (testInfo.project.name === 'mobile')
    await page.getByRole('button', { name: 'Open sidebar' }).click();
  await page.getByRole('tab', { name: 'Projects', exact: true }).click();
  const sidebar = page.locator('.sidebar-content');
  const scrollToBottom = () =>
    sidebar.evaluate((element) => {
      element.scrollTop = element.scrollHeight;
    });
  await expect(page.getByRole('button', { name: 'Pinned chat 29', exact: true })).toBeAttached();
  await expect
    .poll(async () => {
      await scrollToBottom();
      return cursorCalls.join(',');
    })
    .toBe('page-2,page-3');
  await expect(page.getByRole('button', { name: 'Page three chat', exact: true })).toBeAttached();
  await expect(page.locator('.project-pagination-sentinel')).toHaveCount(0);

  // Status polling periodically re-fetches the sidebar's first page. That must
  // not rewind the cursor and make the bottom sentinel re-walk loaded pages.
  // The poller reschedules itself only after its awaited fetches settle, so
  // advance fake time in steps until the refreshed snapshot has been applied.
  const refreshed = page.getByRole('button', { name: 'Refreshed chat', exact: true });
  await expect
    .poll(
      async () => {
        await page.clock.runFor(10_000);
        return refreshed.count();
      },
      { timeout: 15_000 },
    )
    .toBe(1);
  // A rewound cursor re-mounts the sentinel and re-requests page 2 within a
  // frame or two; give that ample real time to surface before asserting.
  for (let attempt = 0; attempt < 5; attempt += 1) {
    await scrollToBottom();
    await page.waitForTimeout(200);
  }
  expect(cursorCalls).toEqual(['page-2', 'page-3']);
  await expect(page.locator('.project-pagination-sentinel')).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Page three chat', exact: true })).toBeAttached();
});
