import { expect, test } from '@playwright/test';

// Icons draw <symbol>s from the hashed sprite with <use>, which jsdom cannot render.
test('icons draw from the preloaded sprite, downloaded once', async ({ page }) => {
  const downloads: Promise<number>[] = [];
  const warnings: string[] = [];
  // The head preloads the sprite as an image; <use> then fetches it again with its own
  // destination, which the HTTP cache answers, so only one response carries a body.
  page.on('requestfinished', (request) => {
    if (/\/dist\/assets\/icons-[\w-]+\.svg$/.test(new URL(request.url()).pathname))
      downloads.push(request.sizes().then((sizes) => sizes.responseBodySize));
  });
  page.on('console', (message) => {
    if (message.type() === 'warning' || message.type() === 'error') warnings.push(message.text());
  });

  await page.goto('./');
  await expect(page.getByRole('textbox', { name: 'Message' })).toBeVisible();

  const send = page.locator('svg[data-icon="send"]').first();
  await expect(send).toBeVisible();
  // The symbol's shapes are in the <use> shadow tree; a resolved reference has a size.
  await expect
    .poll(() =>
      send.evaluate((svg) => {
        const box = (svg.querySelector('use') as SVGGraphicsElement).getBBox();
        return box.width > 0 && box.height > 0;
      }),
    )
    .toBe(true);
  expect(await send.locator('use').getAttribute('href')).toMatch(
    /dist\/assets\/icons-[\w-]+\.svg#send$/,
  );

  const bodies = await Promise.all(downloads);
  expect(bodies.filter((size) => size > 0)).toHaveLength(1);
  expect(warnings.filter((text) => /preload/i.test(text))).toEqual([]);
});
