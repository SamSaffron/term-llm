import { expect, test } from '@playwright/test';

const relayURL = process.env.TERM_LLM_WEBRTC_RELAY_URL || '';

test('falls back through a signaling timeout, then establishes a real browser-to-Go channel', async ({
  page,
}) => {
  test.skip(!relayURL, 'isolated WebRTC relay is not configured');
  test.setTimeout(45_000);
  const initialStats = (await (await page.request.get(`${relayURL}/stats`)).json()) as {
    offers: number;
    answers: number;
  };
  await page.request.post(`${relayURL}/control`, { data: { hang_sessions: 1 } });
  await page.goto('./?new=1');

  // The first signaling session hangs until the browser's owned deadline
  // aborts it. HTTPS must remain usable while renegotiation backs off.
  const composer = page.getByRole('textbox', { name: 'Message' });
  await composer.fill('HTTPS continuity before WebRTC recovery');
  await page.getByRole('button', { name: 'Send message' }).click();
  await expect(page.getByRole('heading', { name: 'Debug Provider Output' }).last()).toBeVisible({
    timeout: 15_000,
  });

  // A later owned generation succeeds through the real relay and Go peer.
  await expect
    .poll(
      async () => {
        const stats = (await (await page.request.get(`${relayURL}/stats`)).json()) as {
          offers: number;
          answers: number;
        };
        return Math.min(stats.offers - initialStats.offers, stats.answers - initialStats.answers);
      },
      { timeout: 30_000 },
    )
    .toBeGreaterThan(0);

  await expect
    .poll(
      async () => {
        const diagnostics = await page.evaluate(async () => {
          const response = await fetch('v1/capabilities');
          const body = (await response.json()) as {
            webrtc_diagnostics?: { active?: number; reserved?: number };
          };
          return body.webrtc_diagnostics || {};
        });
        return Number(diagnostics.active || 0);
      },
      { timeout: 20_000 },
    )
    .toBeGreaterThan(0);
});

test('clears an offer timeout and admission rejection before later recovery', async ({
  page,
}, testInfo) => {
  test.skip(!relayURL, 'isolated WebRTC relay is not configured');
  test.skip(testInfo.project.name === 'mobile', 'signaling fault sequence is covered once');
  test.setTimeout(60_000);
  const initialStats = (await (await page.request.get(`${relayURL}/stats`)).json()) as {
    offers: number;
    answers: number;
  };
  await page.request.post(`${relayURL}/control`, { data: { hang_offers: 1 } });
  await page.goto('./?new=1');

  const composer = page.getByRole('textbox', { name: 'Message' });
  await composer.fill('HTTPS remains available during an offer timeout');
  await page.getByRole('button', { name: 'Send message' }).click();
  await expect(page.getByRole('heading', { name: 'Debug Provider Output' }).last()).toBeVisible({
    timeout: 15_000,
  });

  await expect
    .poll(async () => {
      const stats = (await (await page.request.get(`${relayURL}/stats`)).json()) as {
        offers: number;
      };
      return stats.offers - initialStats.offers;
    })
    .toBeGreaterThanOrEqual(1);
  // The currently hung request has consumed hang_offers. Reject the next owned
  // generation explicitly, then permit the following generation to reach Go.
  await page.request.post(`${relayURL}/control`, { data: { reject_offers: 1 } });

  await expect
    .poll(
      async () => {
        const stats = (await (await page.request.get(`${relayURL}/stats`)).json()) as {
          offers: number;
          answers: number;
        };
        return stats.offers - initialStats.offers >= 3 && stats.answers - initialStats.answers >= 2;
      },
      { timeout: 45_000 },
    )
    .toBe(true);

  await expect
    .poll(
      async () =>
        page.evaluate(async () => {
          const body = (await (await fetch('v1/capabilities')).json()) as {
            webrtc_diagnostics?: { active?: number };
          };
          return Number(body.webrtc_diagnostics?.active || 0);
        }),
      { timeout: 20_000 },
    )
    .toBeGreaterThan(0);
});
