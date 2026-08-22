const { test, expect } = require('@playwright/test');
const fs = require('node:fs');
if (process.env.TERM_LLM_HUB_CHROMIUM) test.use({ launchOptions: { executablePath: process.env.TERM_LLM_HUB_CHROMIUM } });
test.use({ ignoreHTTPSErrors: true });

test('adds a host-recovery passkey without deleting credentials', async ({ page, context }) => {
  const cdp = await context.newCDPSession(page);
  await cdp.send('WebAuthn.enable');
  const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', { options: {
    protocol: 'ctap2', transport: 'internal', hasResidentKey: true,
    hasUserVerification: true, isUserVerified: true,
    automaticPresenceSimulation: true,
  }});
  await cdp.send('WebAuthn.addCredential', { authenticatorId, credential: JSON.parse(fs.readFileSync(process.env.TERM_LLM_HUB_CREDENTIAL_FILE, 'utf8')) });
  await page.goto(process.env.TERM_LLM_HUB_SMOKE_URL);
  await expect(page).toHaveURL(/\/hub\/auth\/login/);
  await page.getByRole('button', { name: 'Sign in with a passkey' }).click();
  await expect(page).toHaveURL(/\/hub\/$/);
  await page.getByRole('button', { name: 'Security' }).click();
  await page.getByRole('button', { name: 'Sign out' }).click();
  await expect(page).toHaveURL(/\/hub\/auth\/login$/);
  // The documented unauthenticated-auth limiter is shared by this loopback proxy.
  await page.waitForTimeout(12_000);

  await cdp.send('WebAuthn.addVirtualAuthenticator', { options: {
    protocol: 'ctap2', transport: 'usb', hasResidentKey: true,
    hasUserVerification: true, isUserVerified: true,
    automaticPresenceSimulation: true,
  }});

  await page.goto(new URL('auth/recover', process.env.TERM_LLM_HUB_SMOKE_URL).href);
  const verified = await page.evaluate(async token => {
    const response = await fetch('../api/auth/recovery/verify', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ code: token }) });
    return response.ok;
  }, process.env.TERM_LLM_HUB_SMOKE_TOKEN);
  expect(verified).toBe(true);
  const grantDataStatus = await page.evaluate(async () => (await fetch('../api/nodes')).status);
  expect(grantDataStatus).toBe(401);
  await page.evaluate(() => sessionStorage.setItem('term_llm_hub_grant_verified', '1'));
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Add a recovery passkey' })).toBeVisible();
  await page.getByLabel('Passkey name').fill('Virtual recovery key');
  await page.getByRole('button', { name: 'Verify and add passkey' }).click();
  await expect(page).toHaveURL(/\/hub\/auth\/login$/);
  await page.getByRole('button', { name: 'Sign in with a passkey' }).click();
  await expect(page).toHaveURL(/\/hub\/$/);
  await page.getByRole('button', { name: 'Security' }).click();
  await expect(page.getByText('Virtual platform passkey')).toBeVisible();
  await expect(page.getByText('Virtual recovery key')).toBeVisible();
});
