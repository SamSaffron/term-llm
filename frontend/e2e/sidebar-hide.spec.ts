import { test, expect, type Page, type Route, type TestInfo } from '@playwright/test';

interface LayoutSample {
  phase: string;
  time: number;
  scrollTop: number;
  scrollHeight: number;
  clientHeight: number;
  anchorTop: number | null;
  targetHeight: number | null;
  targetPresent: boolean;
  rowCount: number;
}

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

const session = (number: number) => ({
  id: `sidebar-${number}`,
  number,
  title: `No project conversation #${number}`,
  name: `No project conversation #${number}`,
  mode: 'chat',
  origin: 'web',
  created_at: 1_700_000_000 - number,
  last_message_at: 1_700_000_000 - number,
  pinned: false,
  archived: false,
  message_count: number % 5,
});

async function mockSidebarAPI(page: Page) {
  const allSessions = Array.from({ length: 48 }, (_, index) => session(index + 1));
  const targetID = 'sidebar-40';
  const patchStarted = deferred();
  const releasePatch = deferred();
  let archived = false;
  let listCalls = 0;

  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    const json = (value: unknown, status = 200) =>
      route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(value) });

    if (path.endsWith(`/v1/sessions/${targetID}`) && request.method() === 'PATCH') {
      patchStarted.resolve();
      await releasePatch.promise;
      archived = true;
      return json({ ok: true });
    }
    if (path.endsWith('/v1/capabilities')) return json({ projects: { enabled: true } });
    if (path.endsWith('/v1/providers'))
      return json({
        object: 'list',
        data: [
          {
            name: 'openai',
            configured: true,
            is_default: true,
            default_model: 'gpt-test',
            models: ['gpt-test'],
          },
        ],
      });
    if (path.endsWith('/v1/models'))
      return json({ object: 'list', data: [{ id: 'gpt-test', owned_by: 'openai' }] });
    if (path.endsWith('/v1/sessions') && url.searchParams.get('selected_only') === '1') {
      const selectedID = url.searchParams.get('selected_session');
      const selected = allSessions.find((entry) => entry.id === selectedID) || allSessions.at(-1)!;
      return json({
        selected_session: selected,
        selected_transcript: { bodies: { messages: [] } },
      });
    }
    if (path.endsWith('/v1/sidebar') && request.method() === 'GET') {
      listCalls += 1;
      const visible = archived ? allSessions.filter((entry) => entry.id !== targetID) : allSessions;
      const sessions = visible.slice(0, 12);
      return json({
        groups: [
          {
            no_project: true,
            sessions,
            session_count: visible.length,
            next_cursor: visible.length > sessions.length ? '12' : '',
          },
        ],
      });
    }
    if (
      path.endsWith('/v1/sessions') &&
      request.method() === 'GET' &&
      url.searchParams.get('no_project') === '1'
    ) {
      const offset = Number(url.searchParams.get('cursor')) || 0;
      const visible = archived ? allSessions.filter((entry) => entry.id !== targetID) : allSessions;
      const sessions = visible.slice(offset, offset + 12);
      const next = offset + sessions.length;
      return json({ sessions, next_cursor: next < visible.length ? String(next) : '' });
    }
    if (path.endsWith('/v1/sessions') && request.method() === 'GET') {
      return json({ sessions: allSessions });
    }
    if (/\/v1\/sessions\/[^/]+\/state$/.test(path)) return json({});
    if (path.endsWith('/v1/sessions/status')) return json({ sessions: [] });
    return json({});
  });

  return { targetID, patchStarted, releasePatch, sidebarCalls: () => listCalls };
}

async function nextFrames(page: Page, count = 2) {
  await page.evaluate(
    (frames) =>
      new Promise<void>((resolve) => {
        const step = (remaining: number) =>
          requestAnimationFrame(() => (remaining <= 1 ? resolve() : step(remaining - 1)));
        step(frames);
      }),
    count,
  );
}

async function setPhase(page: Page, phase: string) {
  await page.evaluate((value) => {
    document.documentElement.dataset.sidebarHidePhase = value;
  }, phase);
}

async function measure(
  page: Page,
  targetTitle: string,
  anchorTitle: string,
): Promise<LayoutSample> {
  return page.evaluate(
    ({ target, anchor }) => {
      const scroller = document.querySelector<HTMLElement>('.sidebar-content')!;
      const rowFor = (title: string) =>
        document
          .querySelector<HTMLButtonElement>(`.session-btn[aria-label="${title}"]`)
          ?.closest<HTMLElement>('.session-row') || null;
      const targetRow = rowFor(target);
      const anchorRow = rowFor(anchor);
      return {
        phase: document.documentElement.dataset.sidebarHidePhase || '',
        time: performance.now(),
        scrollTop: scroller.scrollTop,
        scrollHeight: scroller.scrollHeight,
        clientHeight: scroller.clientHeight,
        anchorTop: anchorRow?.getBoundingClientRect().top ?? null,
        targetHeight: targetRow?.getBoundingClientRect().height ?? null,
        targetPresent: Boolean(targetRow),
        rowCount: document.querySelectorAll('.session-row').length,
      };
    },
    { target: targetTitle, anchor: anchorTitle },
  );
}

async function startFrameRecorder(page: Page, targetTitle: string, anchorTitle: string) {
  await page.evaluate(
    ({ target, anchor }) => {
      type RecorderWindow = Window & {
        __sidebarHideFrames?: LayoutSample[];
        __stopSidebarHideFrames?: boolean;
      };
      const recorder = window as RecorderWindow;
      recorder.__sidebarHideFrames = [];
      recorder.__stopSidebarHideFrames = false;
      const rowFor = (title: string) =>
        document
          .querySelector<HTMLButtonElement>(`.session-btn[aria-label="${title}"]`)
          ?.closest<HTMLElement>('.session-row') || null;
      const sample = () => {
        if (recorder.__stopSidebarHideFrames) return;
        const scroller = document.querySelector<HTMLElement>('.sidebar-content');
        if (scroller) {
          const targetRow = rowFor(target);
          const anchorRow = rowFor(anchor);
          recorder.__sidebarHideFrames!.push({
            phase: document.documentElement.dataset.sidebarHidePhase || '',
            time: performance.now(),
            scrollTop: scroller.scrollTop,
            scrollHeight: scroller.scrollHeight,
            clientHeight: scroller.clientHeight,
            anchorTop: anchorRow?.getBoundingClientRect().top ?? null,
            targetHeight: targetRow?.getBoundingClientRect().height ?? null,
            targetPresent: Boolean(targetRow),
            rowCount: document.querySelectorAll('.session-row').length,
          });
        }
        requestAnimationFrame(sample);
      };
      requestAnimationFrame(sample);
    },
    { target: targetTitle, anchor: anchorTitle },
  );
}

async function attachFrameRecorder(page: Page, testInfo: TestInfo) {
  const frames = await page.evaluate(() => {
    type RecorderWindow = Window & {
      __sidebarHideFrames?: LayoutSample[];
      __stopSidebarHideFrames?: boolean;
    };
    const recorder = window as RecorderWindow;
    recorder.__stopSidebarHideFrames = true;
    return recorder.__sidebarHideFrames || [];
  });
  await testInfo.attach('sidebar-hide-layout-frames.json', {
    body: JSON.stringify(frames, null, 2),
    contentType: 'application/json',
  });
  return frames;
}

function expectStableAfterCollapse(baseline: LayoutSample, sample: LayoutSample) {
  expect(sample.scrollTop, `${sample.phase} changed sidebar scrollTop`).toBeCloseTo(
    baseline.scrollTop,
    0,
  );
  expect(
    sample.anchorTop,
    `${sample.phase} moved the stable row above the hidden item`,
  ).toBeCloseTo(baseline.anchorTop!, 0);
}

test('keeps paginated no-project scroll stable when hiding conversation #40', async ({
  page,
}, testInfo) => {
  test.skip(
    testInfo.project.name === 'mobile',
    'one narrow Chromium viewport is the deterministic baseline',
  );
  await page.setViewportSize({ width: 480, height: 600 });
  const controls = await mockSidebarAPI(page);
  await page.goto('./');
  await expect(page.locator('#startupSplash')).toBeHidden({ timeout: 10_000 });
  await page.getByRole('button', { name: 'Open sidebar' }).click();
  await expect(page.locator('#sidebar')).toHaveClass(/open/);
  const targetTitle = 'No project conversation #40';
  const anchorTitle = 'No project conversation #39';
  const targetButton = page.getByRole('button', { name: targetTitle, exact: true });
  for (let pageNumber = 0; pageNumber < 4 && (await targetButton.count()) === 0; pageNumber += 1) {
    const previousRows = await page.locator('.session-row').count();
    await page.locator('.project-pagination-sentinel').last().scrollIntoViewIfNeeded();
    await expect.poll(() => page.locator('.session-row').count()).toBeGreaterThan(previousRows);
  }
  await expect(targetButton).toBeAttached();
  await targetButton.scrollIntoViewIfNeeded();
  await targetButton.click();
  await expect(page).toHaveURL(/\/chat\/40$/);
  await page.getByRole('button', { name: 'Open sidebar' }).click();
  await expect(page.locator('#sidebar')).toHaveClass(/open/);
  await targetButton.scrollIntoViewIfNeeded();
  await page.locator('.sidebar-content').evaluate((element) => {
    const target = document.querySelector<HTMLButtonElement>(
      '.session-btn[aria-label="No project conversation #40"]',
    );
    if (target) element.scrollTop += target.getBoundingClientRect().top - innerHeight / 2;
  });
  await nextFrames(page);

  await setPhase(page, 'transition');
  await startFrameRecorder(page, targetTitle, anchorTitle);
  await page.getByRole('button', { name: `Actions for ${targetTitle}` }).click();
  await page.getByRole('menuitem', { name: 'Hide' }).click();

  await controls.patchStarted.promise;
  await setPhase(page, 'patch-held');
  await nextFrames(page);
  const collapsed = await measure(page, targetTitle, anchorTitle);

  controls.releasePatch.resolve();
  await expect(targetButton).toHaveCount(0);
  await setPhase(page, 'settled');
  await nextFrames(page);
  const settled = await measure(page, targetTitle, anchorTitle);
  const frames = await attachFrameRecorder(page, testInfo);

  expect(collapsed.targetPresent).toBe(true);
  expect(collapsed.targetHeight).toBeLessThanOrEqual(0.5);
  expectStableAfterCollapse(collapsed, settled);
  expect(settled.rowCount).toBe(47);
  expect(controls.sidebarCalls()).toBe(1);
  expect(frames.some((frame) => frame.phase === 'patch-held')).toBe(true);
  expect(frames.some((frame) => frame.phase === 'settled')).toBe(true);
});
