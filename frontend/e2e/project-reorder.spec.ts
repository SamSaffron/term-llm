import { expect, test, type Locator, type Page, type Route } from '@playwright/test';

const now = 1_800_000_000;

interface Conversation {
  id: string;
  title: string;
  /** Minutes since its last message. */
  minutesAgo: number;
}

interface ProjectFixture {
  id: string;
  name: string;
  rank: number;
  archived?: boolean;
  conversations: Conversation[];
}

const conversations = (prefix: string, title: string, count: number, minutesAgo: number) =>
  Array.from({ length: count }, (_, index) => ({
    id: `${prefix}-${index + 1}`,
    title: `${title} ${index + 1}`,
    minutesAgo: minutesAgo + index,
  }));

const byActivity = (left: Conversation, right: Conversation) => left.minutesAgo - right.minutesAgo;

/**
 * A fake server that persists project ranks the way the real one does: the
 * sidebar lists active projects before archived ones, each in rank order, and
 * a reorder moves the listed projects into the ranks they already hold, so the
 * archived project keeps its rank between them. Ranks deliberately disagree
 * with activity: Alpha, the first project, has the oldest conversations and is
 * much taller than the others once expanded.
 */
async function mockProjectAPI(page: Page) {
  const projects: ProjectFixture[] = [
    {
      id: 'prj-alpha',
      name: 'Alpha project',
      rank: 1,
      conversations: conversations('alpha', 'Alpha task', 5, 300),
    },
    { id: 'prj-old', name: 'Old project', rank: 2, archived: true, conversations: [] },
    {
      id: 'prj-beta',
      name: 'Beta project',
      rank: 3,
      conversations: conversations('beta', 'Beta task', 2, 120),
    },
    {
      id: 'prj-gamma',
      name: 'Gamma project',
      rank: 4,
      conversations: conversations('gamma', 'Gamma task', 1, 5),
    },
  ];
  const loose = conversations('loose', 'Loose chat', 1, 60);
  const numbers = new Map(
    [...projects.flatMap((project) => project.conversations), ...loose].map((entry, index) => [
      entry.id,
      index + 1,
    ]),
  );
  const saved: string[][] = [];
  const summary = (conversation: Conversation, project?: ProjectFixture) => ({
    id: conversation.id,
    number: numbers.get(conversation.id),
    short_title: conversation.title,
    name: conversation.title,
    mode: 'chat',
    origin: 'web',
    created_at: now - conversation.minutesAgo * 60,
    last_message_at: now - conversation.minutesAgo * 60,
    message_count: 1,
    pinned: false,
    archived: false,
    ...(project ? { project_id: project.id, project_name: project.name } : {}),
    file_change_summary: { file_count: 0, adds: 0, dels: 0, git: false },
  });
  const group = (project: ProjectFixture) => ({
    project: {
      id: project.id,
      name: project.name,
      canonical_dir: `/work/${project.id}`,
      sort_order: project.rank,
      ...(project.archived ? { archived_at: '2026-01-01T00:00:00Z' } : {}),
      conversation_count: project.conversations.length,
      available: true,
      git: true,
    },
    session_count: project.conversations.length,
    sessions: [...project.conversations].sort(byActivity).map((entry) => summary(entry, project)),
  });

  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    const json = (value: unknown) =>
      route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(value) });

    if (path.endsWith('/v1/projects/order') && request.method() === 'PATCH') {
      const ids = (request.postDataJSON() as { project_ids: string[] }).project_ids;
      saved.push(ids);
      const listed = ids.map((id) => projects.find((project) => project.id === id)!);
      const slots = listed.map((project) => project.rank).sort((left, right) => left - right);
      listed.forEach((project, index) => {
        project.rank = slots[index];
      });
      return json({
        projects: [...projects]
          .sort((left, right) => left.rank - right.rank)
          .map((project) => ({ id: project.id, sort_order: project.rank })),
      });
    }
    if (path.endsWith('/v1/sidebar')) {
      const ordered = [...projects].sort(
        (left, right) =>
          Number(Boolean(left.archived)) - Number(Boolean(right.archived)) ||
          left.rank - right.rank,
      );
      const recent = [
        ...projects.flatMap((project) =>
          project.conversations.map((entry) => ({ entry, project })),
        ),
        ...loose.map((entry) => ({ entry, project: undefined })),
      ].sort((left, right) => byActivity(left.entry, right.entry));
      return json({
        groups: [
          ...ordered.map(group),
          { no_project: true, session_count: loose.length, sessions: loose.map((e) => summary(e)) },
        ],
        recent_sessions: recent.map(({ entry, project }) => summary(entry, project)),
      });
    }
    if (path.endsWith('/v1/capabilities')) return json({ projects: { enabled: true } });
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
    if (/\/v1\/sessions\/[^/]+\/state$/.test(path)) return json({});
    if (/\/v1\/sessions\/[^/]+\/transcript$/.test(path)) return json({ messages: [] });
    return json({});
  });

  return {
    saved,
    /** Records a new message in a conversation, as another browser or the terminal would. */
    touch: (id: string) => {
      for (const entry of [...projects.flatMap((project) => project.conversations), ...loose])
        if (entry.id === id) entry.minutesAgo = 0;
    },
  };
}

/** The active projects' names, in sidebar order. */
const projectNames = (page: Page) =>
  page
    .locator('.sidebar-project-groups .project-order-list')
    .first()
    .locator('.project-group-label');
const archivedNames = (page: Page) =>
  page
    .locator('.sidebar-project-groups .project-order-list')
    .nth(1)
    .locator('.project-group-label');
const projectGroup = (page: Page, id: string) =>
  page.locator(`.project-group[data-project-id="${id}"]`);
/** A project's header toggle, which is also its drag surface. */
const projectToggle = (page: Page, name: string) => page.getByRole('button', { name, exact: true });
const conversationTitles = (page: Page, projectID: string) =>
  projectGroup(page, projectID).locator('.session-title');

async function box(
  locator: Locator,
): Promise<{ x: number; y: number; width: number; height: number }> {
  const rect = await locator.boundingBox();
  if (!rect) throw new Error('element has no layout box');
  return rect;
}

async function center(locator: Locator): Promise<{ x: number; y: number }> {
  const rect = await box(locator);
  return { x: rect.x + rect.width / 2, y: rect.y + rect.height / 2 };
}

const expectNear = (actual: number, expected: number, message: string) =>
  expect(Math.abs(actual - expected), `${message}: ${actual} vs ${expected}`).toBeLessThan(1);

async function openApp(page: Page) {
  const api = await mockProjectAPI(page);
  await page.addInitScript(() => localStorage.setItem('term_llm_sidebar_view', 'projects'));
  await page.goto('./');
  await expect(page.locator('#startupSplash')).toBeHidden({ timeout: 10_000 });
  return api;
}

async function reload(page: Page): Promise<void> {
  await page.reload();
  await expect(page.locator('#startupSplash')).toBeHidden({ timeout: 10_000 });
}

test('keeps projects in their saved order through activity and reloads; moves them by header, keyboard, and menu', async ({
  page,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop',
    'mouse and keyboard reordering in the desktop sidebar',
  );
  await page.setViewportSize({ width: 1280, height: 1000 });
  const api = await openApp(page);
  await expect(projectNames(page)).toHaveText(['Alpha project', 'Beta project', 'Gamma project']);
  await expect(archivedNames(page)).toHaveText(['Old project']);

  // New activity reorders conversations, never projects.
  api.touch('beta-2');
  await expect(async () => {
    await page.evaluate(() => window.dispatchEvent(new Event('focus')));
    await expect(conversationTitles(page, 'prj-beta')).toHaveText(['Beta task 2', 'Beta task 1'], {
      timeout: 1_000,
    });
  }).toPass();
  await expect(projectNames(page)).toHaveText(['Alpha project', 'Beta project', 'Gamma project']);
  await page.getByRole('tab', { name: 'Recent' }).click();
  await expect(page.locator('.sidebar-recent-groups .session-title').first()).toHaveText(
    'Beta task 2',
  );
  await page.getByRole('tab', { name: 'Projects' }).click();
  await reload(page);
  await expect(projectNames(page)).toHaveText(['Alpha project', 'Beta project', 'Gamma project']);
  expect(api.saved).toEqual([]);

  // The header is the whole drag surface; there is no separate grip.
  const header = projectGroup(page, 'prj-gamma').locator('.project-group-header');
  await expect(header.getByRole('button')).toHaveCount(2);
  const start = await center(projectToggle(page, 'Gamma project'));
  const target = await box(projectToggle(page, 'Alpha project'));
  await page.mouse.move(start.x, start.y);
  await page.mouse.down();
  await page.mouse.move(start.x, target.y + 2, { steps: 12 });
  await expect(projectGroup(page, 'prj-gamma')).toHaveClass(/is-dragging/);
  // Projects slide aside to preview the drop; nothing else joins the list.
  expect(
    await page
      .locator('.sidebar-project-groups .project-order-list')
      .first()
      .evaluate((list) => list.children.length),
  ).toBe(3);
  await page.mouse.up();

  await expect(projectNames(page)).toHaveText(['Gamma project', 'Alpha project', 'Beta project']);
  // Only active projects are sent; the archived one keeps its rank.
  await expect.poll(() => api.saved).toEqual([['prj-gamma', 'prj-alpha', 'prj-beta']]);
  // Releasing the dragged header does not also collapse the project.
  await expect(projectToggle(page, 'Gamma project')).toHaveAttribute('aria-expanded', 'true');

  await reload(page);
  await expect(projectNames(page)).toHaveText(['Gamma project', 'Alpha project', 'Beta project']);
  await expect(archivedNames(page)).toHaveText(['Old project']);

  await projectToggle(page, 'Alpha project').focus();
  await page.keyboard.press('Alt+ArrowUp');
  await expect(projectNames(page)).toHaveText(['Alpha project', 'Gamma project', 'Beta project']);
  await expect(projectToggle(page, 'Alpha project')).toBeFocused();
  await expect.poll(() => api.saved.at(-1)).toEqual(['prj-alpha', 'prj-gamma', 'prj-beta']);

  // The first project cannot move up, and the actions menu moves the others.
  await page.getByRole('button', { name: 'Actions for project Alpha project' }).click();
  await expect(page.getByRole('menuitem', { name: 'Move down' })).toBeVisible();
  await expect(page.getByRole('menuitem', { name: 'Move up' })).toHaveCount(0);
  await page.keyboard.press('Escape');
  await page.getByRole('button', { name: 'Actions for project Gamma project' }).click();
  await page.getByRole('menuitem', { name: 'Move down' }).click();
  await expect(projectNames(page)).toHaveText(['Alpha project', 'Beta project', 'Gamma project']);
  await expect.poll(() => api.saved.at(-1)).toEqual(['prj-alpha', 'prj-beta', 'prj-gamma']);
  await expect(
    page.getByRole('button', { name: 'Actions for project Gamma project' }),
  ).toBeFocused();

  // A plain click on a header still collapses and expands the project.
  await projectToggle(page, 'Beta project').click();
  await expect(projectToggle(page, 'Beta project')).toHaveAttribute('aria-expanded', 'false');
  await projectToggle(page, 'Beta project').click();
  await expect(projectToggle(page, 'Beta project')).toHaveAttribute('aria-expanded', 'true');
  expect(api.saved).toHaveLength(3);
});

test('drags a tall expanded project as a whole and lands it where the preview showed', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'mouse reordering in the desktop sidebar');
  await page.setViewportSize({ width: 1280, height: 1000 });
  // Groups slide without animating, so the preview can be measured at once.
  await page.emulateMedia({ reducedMotion: 'reduce' });
  const api = await openApp(page);
  await expect(projectNames(page)).toHaveText(['Alpha project', 'Beta project', 'Gamma project']);
  const alpha = projectGroup(page, 'prj-alpha');
  const beta = projectGroup(page, 'prj-beta');
  const gamma = projectGroup(page, 'prj-gamma');
  const alphaLastRow = alpha.locator('.session-row').last();
  const before = {
    alpha: await box(alpha),
    beta: await box(beta),
    gamma: await box(gamma),
    alphaLastRow: await box(alphaLastRow),
  };
  // Alpha is taller than all the groups it will pass put together.
  expect(before.alpha.height).toBeGreaterThan(before.beta.height + before.gamma.height);

  const start = await center(projectToggle(page, 'Alpha project'));
  const end = await center(projectToggle(page, 'Gamma project'));
  await page.mouse.move(start.x, start.y);
  await page.mouse.down();
  await page.mouse.move(start.x, end.y, { steps: 10 });
  await expect(alpha).toHaveClass(/is-dragging/);

  // The whole group moves with its header, conversations included, and stops
  // with its bottom at the end of the list. The groups it passed rise by its
  // height and spacing to open the slot it will land in.
  const lifted = await box(alpha);
  expectNear(lifted.y + lifted.height, before.gamma.y + before.gamma.height, 'lifted bottom');
  expectNear(
    (await box(alphaLastRow)).y - before.alphaLastRow.y,
    lifted.y - before.alpha.y,
    'conversation offset',
  );
  const pitch = before.beta.y - before.alpha.y;
  expectNear((await box(beta)).y, before.beta.y - pitch, 'Beta preview');
  expectNear((await box(gamma)).y, before.gamma.y - pitch, 'Gamma preview');
  await page.mouse.up();

  await expect(projectNames(page)).toHaveText(['Beta project', 'Gamma project', 'Alpha project']);
  await expect.poll(() => api.saved).toEqual([['prj-beta', 'prj-gamma', 'prj-alpha']]);
  await expect(alpha).not.toHaveClass(/is-dragging/);
  expectNear((await box(alpha)).y, lifted.y, 'landing');
  await expect(projectToggle(page, 'Alpha project')).toHaveAttribute('aria-expanded', 'true');
});

test('commits fast drops of a tall expanded project without waiting for the slide animation', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'mouse reordering in the desktop sidebar');
  await page.setViewportSize({ width: 1280, height: 1000 });
  const api = await openApp(page);
  await expect(projectNames(page)).toHaveText(['Alpha project', 'Beta project', 'Gamma project']);

  // No step-wise movement, assertion, or dwell between pressing, moving, and dropping.
  let start = await center(projectToggle(page, 'Alpha project'));
  let end = await center(projectToggle(page, 'Gamma project'));
  await page.mouse.move(start.x, start.y);
  await page.mouse.down();
  await page.mouse.move(start.x, end.y);
  await page.mouse.up();
  await expect(projectNames(page)).toHaveText(['Beta project', 'Gamma project', 'Alpha project']);
  await expect.poll(() => api.saved).toEqual([['prj-beta', 'prj-gamma', 'prj-alpha']]);

  await reload(page);
  await expect(projectNames(page)).toHaveText(['Beta project', 'Gamma project', 'Alpha project']);
  start = await center(projectToggle(page, 'Alpha project'));
  end = await center(projectToggle(page, 'Beta project'));
  await page.mouse.move(start.x, start.y);
  await page.mouse.down();
  await page.mouse.move(start.x, end.y);
  await page.mouse.up();
  await expect(projectNames(page)).toHaveText(['Alpha project', 'Beta project', 'Gamma project']);
  await expect.poll(() => api.saved.at(-1)).toEqual(['prj-alpha', 'prj-beta', 'prj-gamma']);
  await expect(projectToggle(page, 'Alpha project')).toHaveAttribute('aria-expanded', 'true');

  await reload(page);
  await expect(projectNames(page)).toHaveText(['Alpha project', 'Beta project', 'Gamma project']);
  expect(api.saved).toHaveLength(2);
});

test('lifts a project by long press on its header in the mobile drawer; taps and quick swipes still work', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name === 'desktop', 'touch reordering in the mobile drawer');
  const api = await openApp(page);
  await page.getByRole('button', { name: 'Open sidebar' }).click();
  const drawer = page.getByRole('dialog', { name: 'Sessions' });
  await expect(drawer).toBeVisible();
  await expect(projectNames(page)).toHaveText(['Alpha project', 'Beta project', 'Gamma project']);
  // Touch points are measured once the drawer has finished sliding in.
  await expect(page.locator('#sidebar')).toHaveCSS('transform', 'matrix(1, 0, 0, 1, 0, 0)');
  // Beta is dragged above the tall expanded Alpha, so both headers must be on
  // screen: bring Alpha's to the top of the scrolling list.
  await projectToggle(page, 'Alpha project').evaluate((toggle) =>
    toggle.scrollIntoView({ block: 'start' }),
  );
  await expect(projectToggle(page, 'Alpha project')).toBeInViewport();
  await expect(projectToggle(page, 'Beta project')).toBeInViewport();
  // The page clock decides when a resting touch has become a long press.
  await page.clock.install();

  // Real touch input through the browser's gesture handling: nothing but the
  // lifted project may stop the list from scrolling or the drawer from swiping.
  const client = await page.context().newCDPSession(page);
  const touch = (type: 'touchStart' | 'touchMove' | 'touchEnd', x: number, y: number) =>
    client.send('Input.dispatchTouchEvent', {
      type,
      touchPoints: type === 'touchEnd' ? [] : [{ x, y }],
    });
  type Point = { x: number; y: number };
  const moveThrough = async (...path: Point[]) => {
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
  };
  const slide = async (...path: Point[]) => {
    await moveThrough(...path);
    await touch('touchEnd', path.at(-1)!.x, path.at(-1)!.y);
  };
  // Resting first lifts the whole project, which then follows the finger, even
  // after a leftward slide that would otherwise swipe the drawer closed.
  const dragStart = await center(projectToggle(page, 'Beta project'));
  const dragEndY = (await box(projectToggle(page, 'Alpha project'))).y + 2;
  const alphaLastMenu = projectGroup(page, 'prj-alpha').locator('.session-menu-trigger').last();
  await touch('touchStart', dragStart.x, dragStart.y);
  await page.clock.runFor(500);
  await expect(projectGroup(page, 'prj-beta')).toHaveClass(/is-dragging/);
  // Partway up, the lifted project covers Alpha's last conversation, which has
  // not slid aside yet. Its menu button, always shown on touch screens, must
  // stay beneath the lifted project rather than show through it.
  const partway = { x: dragStart.x, y: dragStart.y - 100 };
  await moveThrough(dragStart, partway);
  const covered = await center(alphaLastMenu);
  expect(
    await page.evaluate(
      ({ x, y }) =>
        document.elementFromPoint(x, y)?.closest<HTMLElement>('.project-group')?.dataset.projectId,
      covered,
    ),
  ).toBe('prj-beta');
  await slide(partway, { x: dragStart.x - 80, y: partway.y }, { x: dragStart.x - 80, y: dragEndY });
  await expect(projectNames(page)).toHaveText(['Beta project', 'Alpha project', 'Gamma project']);
  await expect.poll(() => api.saved).toEqual([['prj-beta', 'prj-alpha', 'prj-gamma']]);
  await expect(drawer).toBeVisible();
  await expect(page.locator('#sidebar')).toHaveClass(/\bopen\b/);
  await expect(projectToggle(page, 'Beta project')).toHaveAttribute('aria-expanded', 'true');

  // A tap on a header still collapses the project.
  await projectToggle(page, 'Beta project').tap();
  await expect(projectToggle(page, 'Beta project')).toHaveAttribute('aria-expanded', 'false');
  expect(api.saved).toHaveLength(1);

  // Test the quick swipe after the drag, as pinned-reorder.spec.ts does:
  // without the long press, sliding a project header scrolls instead of
  // reordering.
  await projectToggle(page, 'Alpha project').scrollIntoViewIfNeeded();
  const swipeStart = await center(projectToggle(page, 'Alpha project'));
  await touch('touchStart', swipeStart.x, swipeStart.y);
  await slide(swipeStart, { x: swipeStart.x, y: swipeStart.y + 150 });
  await expect(projectGroup(page, 'prj-alpha')).not.toHaveClass(/is-dragging/);
  await expect(projectNames(page)).toHaveText(['Beta project', 'Alpha project', 'Gamma project']);
  expect(api.saved).toHaveLength(1);
});
