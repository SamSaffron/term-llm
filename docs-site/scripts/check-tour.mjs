import assert from 'node:assert/strict';
import path from 'node:path';
import AxeBuilder from '@axe-core/playwright';

export async function checkTour(browser, origin, results) {
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 }, colorScheme: 'light' });
  const page = await context.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  try {
    await page.clock.install();
    await page.goto(origin);
    const tour = page.locator('[data-product-tour]');
    const selected = () => tour.locator('[role="tab"][aria-selected="true"]').textContent();
    const moveOutside = async () => {
      await page.mouse.move(0, 0);
      await page.locator('#main-content').focus();
    };
    const showTour = async () => {
      await tour.scrollIntoViewIfNeeded();
      await page.waitForTimeout(150); // Allow the native IntersectionObserver to report visibility.
    };
    assert.equal(await tour.getByRole('tab').count(), 5);
    assert.equal(await tour.getByRole('tabpanel').count(), 1);
    await page.clock.runFor(8100);
    assert.equal((await selected()).trim(), 'Review', 'Offscreen tour must not rotate');
    await showTour();
    await moveOutside();
    await page.clock.runFor(8100);
    assert.equal((await selected()).trim(), 'Hub', 'Visible tour should advance after eight seconds');
    await tour.hover();
    await page.clock.runFor(16000);
    assert.equal((await selected()).trim(), 'Hub', 'Hover pauses rotation');
    await tour.getByRole('tab', { name: 'Worktrees', exact: true }).click();
    await moveOutside();
    await page.clock.runFor(16000);
    assert.equal((await selected()).trim(), 'Worktrees', 'Manual selection stops automatic rotation');
    await tour.getByRole('button', { name: 'Start automatic slideshow' }).click();
    await moveOutside();
    await page.clock.runFor(8100);
    assert.equal((await selected()).trim(), 'Agents', 'Play restarts rotation');
    await tour.getByRole('button', { name: 'Pause automatic slideshow' }).click();
    await moveOutside();
    await page.clock.runFor(16000);
    assert.equal((await selected()).trim(), 'Agents');
    await tour.getByRole('tab', { name: 'Agents', exact: true }).focus();
    await page.keyboard.press('ArrowRight');
    assert.equal((await selected()).trim(), 'Shell');
    await page.keyboard.press('ArrowRight');
    assert.equal((await selected()).trim(), 'Review');
    await page.keyboard.press('End');
    assert.equal((await selected()).trim(), 'Shell');
    await page.keyboard.press('Home');
    assert.equal((await selected()).trim(), 'Review');

    // Every scene must load both theme assets, with a matching original-image link.
    for (const theme of ['light', 'dark']) {
      await page.getByLabel('Color theme').selectOption(theme);
      for (const id of ['review', 'hub', 'worktrees', 'agents', 'shell']) {
        await page.locator(`#tour-tab-${id}`).click();
        await page.clock.runFor(300);
        const panel = page.locator(`#tour-panel-${id}`);
        const image = panel.locator('img');
        await image.evaluate(img => img.decode());
        assert.ok((await image.evaluate(img => img.currentSrc)).endsWith(`/tour/${id}-${theme}.png`));
        assert.ok((await panel.locator('[data-tour-image]').getAttribute('href')).endsWith(`/tour/${id}-${theme}.png`));
        const scan = await new AxeBuilder({ page }).include('[data-product-tour]').withTags(['wcag2a', 'wcag2aa', 'wcag21aa']).analyze();
        assert.deepEqual(scan.violations, [], `Tour accessibility: ${id}, ${theme}`);
      }
    }
    const imageLink = page.locator('#tour-panel-shell [data-tour-image]');
    await imageLink.click();
    const dialog = page.getByRole('dialog', { name: 'Shell — full-size screenshot' });
    assert.ok(await dialog.isVisible());
    await dialog.locator('img').evaluate(img => img.decode());
    assert.ok((await dialog.locator('img').evaluate(img => img.currentSrc)).endsWith('/shell-dark.png'));
    assert.ok((await dialog.getByRole('link', { name: 'Open original' }).getAttribute('href')).endsWith('/shell-dark.png'));
    const scan = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21aa']).analyze();
    assert.deepEqual(scan.violations, [], 'Screenshot dialog accessibility');
    await page.keyboard.press('Escape');
    assert.ok(await imageLink.evaluate(el => el === document.activeElement));
    assert.equal(await dialog.count(), 0);

    await page.emulateMedia({ reducedMotion: 'reduce' });
    // emulateMedia returns before Chromium necessarily dispatches the matchMedia
    // change event. Wait for the handler's observable result, not a fixed delay.
    await tour.locator('[data-tour-rotation]:disabled').waitFor({ state: 'attached' });
    assert.ok(await tour.getByRole('button', { name: 'Start automatic slideshow' }).isDisabled());
    await moveOutside();
    await page.clock.runFor(16000);
    assert.equal((await selected()).trim(), 'Shell', 'Reduced motion disables automatic rotation');
    for (const [width, height] of [[1440, 900], [1280, 800], [390, 844], [320, 740]]) {
      await page.setViewportSize({ width, height });
      await page.getByLabel('Color theme').selectOption('light');
      await page.locator('#tour-tab-hub').click();
      await showTour();
      await page.clock.runFor(300);
      const bounds = await tour.boundingBox();
      assert.ok(bounds.width <= Math.min(900, width), 'Tour stays compact');
      assert.ok(bounds.height < height - 100, 'Whole tour fits below the sticky header');
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
      await page.screenshot({ path: path.join(results, `tour-${width}.png`) });
    }
    assert.deepEqual(errors, []);
  } finally {
    await context.close();
  }
  const fallback = await browser.newContext({ javaScriptEnabled: false, colorScheme: 'light' });
  try {
    const page = await fallback.newPage();
    await page.goto(origin);
    assert.ok(await page.locator('#tour-panel-review').isVisible());
    assert.equal(await page.locator('.tour-controls:visible').count(), 0);
    assert.equal(await page.locator('.tour-fallback a').count(), 5);
    for (const link of await page.locator('.tour-fallback a').all()) {
      const response = await page.request.get(new URL(await link.getAttribute('href'), origin).href);
      assert.equal(response.status(), 200);
    }
  } finally {
    await fallback.close();
  }
  console.log('✓ Five-scene tour: rotation, pause, keyboard, themes, enlargement, reduced motion, compact viewports and no-JS fallback');
}
