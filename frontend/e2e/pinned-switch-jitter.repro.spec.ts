import { expect, test, type Page, type Route } from '@playwright/test';

const now = 1_800_000_000;

function session(id: string, title: string, number: number, pinned: boolean) {
  return {
    id,
    number,
    short_title: title,
    name: title,
    mode: 'chat',
    origin: 'web',
    created_at: now - number * 60,
    last_message_at: now - number * 60,
    message_count: number,
    pinned,
    archived: false,
    file_change_summary: { file_count: 0, adds: 0, dels: 0, git: false },
  };
}

const sessions = [
  session('pinned-a', 'Pinned alpha', 1, true),
  session('pinned-b', 'Pinned beta', 2, true),
  session('regular-c', 'Regular gamma', 3, false),
];

async function mockCleanroomAPI(page: Page, projects: boolean) {
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    const json = (value: unknown) =>
      route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(value) });

    if (path.endsWith('/v1/capabilities')) return json({ projects: { enabled: projects } });
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
      return json({
        object: 'list',
        data: [{ id: 'fixture-model', owned_by: 'cleanroom' }],
      });
    }
    if (path.endsWith('/v1/sessions/status')) return json({ sessions: [] });
    if (path.endsWith('/v1/sessions') && url.searchParams.get('selected_only') === '1') {
      const id = url.searchParams.get('selected_session') || 'pinned-a';
      const selected = sessions.find((entry) => entry.id === id) || sessions[0];
      return json({
        selected_session: selected,
        selected_transcript: {
          bodies: {
            rev: 1,
            messages: [
              {
                id: `${id}-message`,
                sequence: 0,
                role: 'user',
                parts: [{ type: 'text', text: `Transcript for ${id}` }],
              },
            ],
          },
        },
      });
    }
    if (path.endsWith('/v1/sidebar')) {
      return json({ sessions, recent_sessions: sessions, groups: [] });
    }
    if (path.endsWith('/v1/sessions')) return json({ sessions });
    if (/\/v1\/sessions\/[^/]+\/state$/.test(path)) return json({});
    if (/\/v1\/sessions\/[^/]+\/transcript$/.test(path)) return json({ messages: [] });
    return json({});
  });
}

type MutationSample = {
  type: string;
  session: string;
  attribute: string | null;
  oldValue: string | null;
  value: string | null;
};

async function beginSidebarMutationTrace(page: Page) {
  await page.evaluate(() => {
    type TraceWindow = Window & {
      __pinnedMutationTrace?: MutationSample[];
      __pinnedMutationObserver?: MutationObserver;
    };
    const target = document.querySelector('#sessionGroups');
    if (!target) throw new Error('session groups missing');
    const traceWindow = window as TraceWindow;
    traceWindow.__pinnedMutationTrace = [];
    const textNodeSessions = new WeakMap<Node, string>();
    for (const row of target.querySelectorAll('.session-row')) {
      const session = row.querySelector('.session-title')?.textContent || '';
      const walker = document.createTreeWalker(row, NodeFilter.SHOW_ALL);
      for (let node = walker.nextNode(); node; node = walker.nextNode()) {
        textNodeSessions.set(node, session);
      }
    }
    traceWindow.__pinnedMutationObserver = new MutationObserver((records) => {
      for (const record of records) {
        const node = record.target;
        const element = node instanceof Element ? node : node.parentElement;
        const row = element?.closest('.session-row');
        traceWindow.__pinnedMutationTrace!.push({
          type: record.type,
          session:
            textNodeSessions.get(node) ||
            row?.querySelector('.session-title')?.textContent ||
            (element?.id === 'sessionGroups' ? '<session-groups-root>' : ''),
          oldValue: record.oldValue,
          attribute: record.attributeName,
          value: node.textContent,
        });
      }
    });
    traceWindow.__pinnedMutationObserver.observe(target, {
      subtree: true,
      childList: true,
      attributes: true,
      characterData: true,
      characterDataOldValue: true,
    });
  });
}

async function finishSidebarMutationTrace(page: Page): Promise<MutationSample[]> {
  return page.evaluate(() => {
    type TraceWindow = Window & {
      __pinnedMutationTrace?: MutationSample[];
      __pinnedMutationObserver?: MutationObserver;
    };
    const traceWindow = window as TraceWindow;
    traceWindow.__pinnedMutationObserver?.disconnect();
    return traceWindow.__pinnedMutationTrace || [];
  });
}

for (const projects of [false, true]) {
  test(`switching pinned conversations keeps the ${projects ? 'project' : 'flat'} sidebar stable`, async ({
    page,
  }, testInfo) => {
    test.skip(
      testInfo.project.name === 'mobile',
      'desktop sidebar remains visible while switching',
    );
    await mockCleanroomAPI(page, projects);
    await page.goto('./');
    await expect(page.locator('#startupSplash')).toBeHidden({ timeout: 10_000 });
    const rows = page.locator('.sidebar-pinned-group .session-row');
    await expect(rows).toHaveCount(2);

    await page.getByRole('button', { name: 'Pinned alpha', exact: true }).click();
    await expect(page.getByText('Transcript for pinned-a')).toBeVisible();
    await page.evaluate(
      () =>
        new Promise((resolve) =>
          requestAnimationFrame(() => requestAnimationFrame(() => resolve(null))),
        ),
    );
    await page.locator('#sessionGroups').evaluate((element) => {
      element.setAttribute('data-cleanroom-identity', 'session-groups');
      element.querySelectorAll('.session-row').forEach((row, index) => {
        row.setAttribute('data-cleanroom-identity', `row-${index}`);
      });
    });

    await beginSidebarMutationTrace(page);
    await page.getByRole('button', { name: 'Pinned beta', exact: true }).click();
    await expect(page.getByText('Transcript for pinned-b')).toBeVisible();
    await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => resolve(null))));
    const mutations = await finishSidebarMutationTrace(page);

    await testInfo.attach(`pinned-switch-${projects ? 'projects' : 'flat'}-mutations.json`, {
      body: JSON.stringify(mutations, null, 2),
      contentType: 'application/json',
    });

    await expect(
      page.locator('#sessionGroups[data-cleanroom-identity="session-groups"]'),
    ).toHaveCount(1);
    await expect(page.locator('.session-row[data-cleanroom-identity]')).toHaveCount(3);
    expect(mutations.filter((entry) => entry.type === 'childList')).toHaveLength(0);
    expect(
      mutations
        .filter((entry) => entry.type === 'attributes')
        .every((entry) => entry.attribute === 'class' || entry.attribute === 'aria-current'),
    ).toBe(true);

    const redundantRootWrites = mutations.filter(
      (entry) =>
        entry.type === 'characterData' &&
        entry.session === '<session-groups-root>' &&
        entry.oldValue === entry.value,
    );
    expect(
      redundantRootWrites.length,
      'a conversation switch must not trigger a redundant second sidebar render',
    ).toBeLessThanOrEqual(1);
  });
}
