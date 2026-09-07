import { expect, test } from '@playwright/test';
import { readFileSync, writeFileSync } from 'node:fs';

const url = process.env.TERM_LLM_WEB_PASSKEY_URL || '';
const state = process.env.TERM_LLM_WEB_PASSKEY_STATE || '';
const phase = process.env.TERM_LLM_WEB_PASSKEY_PHASE || 'enroll';
const code = process.env.TERM_LLM_WEB_PASSKEY_CODE || '';

test('Web passkeys: enrollment, cookie-only chat, restart, login and recovery', async ({
  page,
  context,
}) => {
  test.skip(!url || !state, 'run scripts/web_passkey_smoke.sh');
  const cdp = await context.newCDPSession(page);
  await cdp.send('WebAuthn.enable');
  let { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', {
    options: {
      protocol: 'ctap2',
      transport: 'internal',
      hasResidentKey: true,
      hasUserVerification: true,
      isUserVerified: true,
      automaticPresenceSimulation: true,
    },
  });
  if (phase !== 'enroll') {
    const saved = JSON.parse(readFileSync(`${state}/browser.json`, 'utf8'));
    await context.addCookies(saved.cookies);
    const credentials = JSON.parse(readFileSync(`${state}/credentials.json`, 'utf8'));
    for (const credential of credentials)
      await cdp.send('WebAuthn.addCredential', { authenticatorId, credential });
  }
  if (phase === 'enroll') {
    await context.addCookies([{ name: 'term_llm_token', value: 'stale-bearer', url }]);
    await page.addInitScript(() => {
      if (!sessionStorage.getItem('seeded')) {
        localStorage.setItem('term_llm_token', 'stale-bearer');
        sessionStorage.setItem('seeded', '1');
      }
    });
  }
  const authorization: string[] = [];
  page.on('request', (request) => {
    if (request.headers().authorization) authorization.push(request.url());
  });
  await page.goto(url);
  if (phase === 'enroll') {
    await expect(page).toHaveURL(/\/ui\/auth\/setup$/);
    await page.getByLabel('One-time setup code').fill(code);
    await page.getByLabel('Passkey name').fill('My browser passkey');
    await page.getByRole('button', { name: 'Verify and create passkey' }).click();
  }
  // On restart the original browser cookie must still work without a ceremony.
  await expect(page.getByRole('textbox', { name: 'Message', exact: true })).toBeVisible();
  expect(
    (await context.cookies()).find((cookie) => cookie.name === 'term_llm_web_session'),
  ).toMatchObject({
    httpOnly: true,
    sameSite: 'Strict',
    path: '/ui/',
    secure: false,
  });
  const probe = await page.evaluate(async () => {
    const capabilities = await fetch('v1/capabilities');
    const events = await fetch('v1/events', { signal: AbortSignal.timeout(3000) });
    const status = events.status;
    await events.body?.cancel();
    return { capabilities: capabilities.status, events: status };
  });
  expect(probe).toEqual({ capabilities: 200, events: 200 });
  if (phase === 'enroll') {
    await page
      .getByRole('textbox', { name: 'Message', exact: true })
      .fill('Hello from a passkey-authenticated browser');
    await page.getByRole('button', { name: 'Send message', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Debug Provider Output' }).last()).toBeVisible({
      timeout: 15000,
    });
  }
  await page.goto(`${url}auth/security`);
  await expect(page.getByText('My browser passkey', { exact: true })).toBeVisible();
  if (phase !== 'enroll') {
    await page.getByRole('button', { name: 'Sign out', exact: true }).click();
    await expect(page).toHaveURL(/\/ui\/auth\/login$/);
    expect(await page.evaluate(async () => (await fetch('../v1/capabilities')).status)).toBe(401);
    await page.getByRole('button', { name: 'Sign in with a passkey', exact: true }).click();
    await expect(page.getByRole('textbox', { name: 'Message', exact: true })).toBeVisible();
    // Respect the shared authentication rate limiter between full ceremonies.
    await page.waitForTimeout(12_000);
    await cdp.send('WebAuthn.removeVirtualAuthenticator', { authenticatorId });
    ({ authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', {
      options: {
        protocol: 'ctap2',
        transport: 'internal',
        hasResidentKey: true,
        hasUserVerification: true,
        isUserVerified: true,
        automaticPresenceSimulation: true,
      },
    }));
    await context.clearCookies();
    await page.goto(`${url}auth/recover`);
    await page.getByLabel('One-time setup code').fill(code);
    await page.getByLabel('Passkey name').fill('Recovery passkey');
    await page.getByRole('button', { name: 'Verify and add passkey' }).click();
    await expect(page).toHaveURL(/\/ui\/auth\/login$/);
    await page.getByRole('button', { name: 'Sign in with a passkey', exact: true }).click();
    await expect(page.getByRole('textbox', { name: 'Message', exact: true })).toBeVisible();
    await page.goto(`${url}auth/security`);
    await expect(page.getByText('Recovery passkey', { exact: true })).toBeVisible();
  }
  expect(authorization).toEqual([]);
  expect(await page.evaluate(() => localStorage.getItem('term_llm_token'))).toBeNull();
  await context.storageState({ path: `${state}/browser.json` });
  const { credentials } = await cdp.send('WebAuthn.getCredentials', { authenticatorId });
  writeFileSync(`${state}/credentials.json`, JSON.stringify(credentials));
});
