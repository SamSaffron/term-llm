import { expect, test } from '@playwright/test';
import { writeFileSync } from 'node:fs';

const smokeURL = process.env.TERM_LLM_HUB_SMOKE_URL || '';
const smokeToken = process.env.TERM_LLM_HUB_SMOKE_TOKEN || '';
const credentialFile = process.env.TERM_LLM_HUB_CREDENTIAL_FILE || '';

test('enrolls and authenticates with a virtual passkey', async ({ page, context }) => {
  test.skip(!smokeURL || !smokeToken || !credentialFile, 'passkey Hub smoke is not configured');
  const cdp = await context.newCDPSession(page);
  await cdp.send('WebAuthn.enable');
  const { authenticatorId: primaryAuthenticatorId } = await cdp.send(
    'WebAuthn.addVirtualAuthenticator',
    {
      options: {
        protocol: 'ctap2',
        transport: 'internal',
        hasResidentKey: true,
        hasUserVerification: true,
        isUserVerified: true,
        automaticPresenceSimulation: true,
      },
    },
  );

  await page.goto(smokeURL);
  await expect(page).toHaveURL(/\/hub\/auth\/setup$/);
  await page.getByLabel('One-time setup code').fill(smokeToken);
  await page.getByLabel('Passkey name').fill('Virtual platform passkey');
  await page.getByRole('button', { name: 'Verify and create passkey' }).click();
  await expect(page).toHaveURL(/\/hub\/$/);
  const sessionCookie = (await context.cookies()).find(
    (cookie) => cookie.name === 'term_llm_hub_session',
  );
  expect(sessionCookie).toMatchObject({
    httpOnly: true,
    secure: true,
    sameSite: 'Strict',
    path: '/hub/',
  });
  await expect(page.getByRole('button', { name: 'Security' })).toBeVisible();

  await page.getByRole('button', { name: 'Security' }).click();
  await expect(page.getByText('Virtual platform passkey')).toBeVisible();

  await cdp.send('WebAuthn.addVirtualAuthenticator', {
    options: {
      protocol: 'ctap2',
      transport: 'usb',
      hasResidentKey: true,
      hasUserVerification: true,
      isUserVerified: true,
      automaticPresenceSimulation: true,
    },
  });
  page.once('dialog', (dialog) => void dialog.accept('Virtual second key'));
  await page.getByRole('button', { name: 'Add passkey' }).click();
  await expect(page.getByText('Virtual second key')).toBeVisible();
  let second = page.locator('.delegation-card').filter({ hasText: 'Virtual second key' });
  page.once('dialog', (dialog) => void dialog.accept('Renamed virtual key'));
  await second.getByRole('button', { name: 'Rename' }).click();
  await expect(page.getByText('Renamed virtual key')).toBeVisible();
  second = page.locator('.delegation-card').filter({ hasText: 'Renamed virtual key' });
  await second.getByRole('button', { name: 'Remove' }).click();
  await expect(page.getByText('Renamed virtual key')).toHaveCount(0);
  await expect(
    page
      .locator('.delegation-card')
      .filter({ hasText: 'Virtual platform passkey' })
      .getByRole('button', { name: 'Remove' }),
  ).toBeDisabled();
  await page.getByRole('button', { name: 'Revoke other sessions' }).click();
  await expect(page.getByText('Revoked 0 other sessions.')).toBeVisible();

  await page.getByRole('button', { name: 'Security' }).click();
  await context.clearCookies();
  await page.getByRole('button', { name: 'Security' }).click();
  await expect(page).toHaveURL(/\/hub\/auth\/login$/);
  await page.getByRole('button', { name: 'Sign in with a passkey' }).click();
  await expect(page).toHaveURL(/\/hub\/$/);
  await page.getByRole('button', { name: 'Security' }).click();
  await expect(page.getByRole('button', { name: 'Sign out' })).toBeVisible();

  await page.getByRole('button', { name: 'Sign out' }).click();
  await expect(page).toHaveURL(/\/hub\/auth\/login$/);
  const { credentials } = await cdp.send('WebAuthn.getCredentials', {
    authenticatorId: primaryAuthenticatorId,
  });
  expect(credentials).toHaveLength(1);
  writeFileSync(credentialFile, JSON.stringify(credentials[0]));
});
