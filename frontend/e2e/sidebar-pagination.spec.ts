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
