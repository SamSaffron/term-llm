import assert from 'node:assert/strict';
import path from 'node:path';
import { stat } from 'node:fs/promises';
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
    await page.evaluate(() => window.scrollTo({ top: document.body.scrollHeight, behavior: 'instant' }));
    await page.waitForTimeout(150);
    await page.clock.runFor(8100);
    assert.equal((await selected()).trim(), 'Review', 'Offscreen tour must not rotate');
    await showTour();
    await moveOutside();
    await page.clock.runFor(5100);
    assert.equal((await selected()).trim(), 'Hub', 'Visible tour advances after five seconds');
    await tour.hover();
    await page.clock.runFor(5100);
    assert.equal((await selected()).trim(), 'Worktrees', 'A parked pointer must not stall autoplay');
    await tour.getByRole('button', { name: 'Show Agents screenshot' }).click();
    assert.equal((await selected()).trim(), 'Agents', 'Dots select the matching slide');
    await moveOutside();
    await page.clock.runFor(5100);
    assert.equal((await selected()).trim(), 'Shell', 'Pointer selection restarts the interval');
    await tour.getByRole('button', { name: 'Pause automatic slideshow' }).click();
    await moveOutside();
    await page.clock.runFor(16000);
    assert.equal((await selected()).trim(), 'Shell', 'Explicit pause stops rotation');
    await tour.getByRole('button', { name: 'Start automatic slideshow' }).click();
    await moveOutside();
    await page.clock.runFor(5100);
    assert.equal((await selected()).trim(), 'Review', 'Autoplay wraps from last to first');
    await page.waitForTimeout(750);
    assert.equal(await tour.locator('.tour-panels').evaluate(el => Math.round(new DOMMatrix(getComputedStyle(el).transform).m41)), -900, 'Wrap snaps to the real first slide after the edge copy');
    await tour.getByRole('button', { name: 'Pause automatic slideshow' }).click();
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
        assert.ok((await image.evaluate(img => img.currentSrc)).endsWith(`/tour/${id}-${theme}-900.webp`));
        assert.ok((await panel.locator('[data-tour-image]').getAttribute('href')).endsWith(`/tour/${id}-${theme}.webp`));
        const scan = await new AxeBuilder({ page }).include('[data-product-tour]').withTags(['wcag2a', 'wcag2aa', 'wcag21aa']).analyze();
        assert.deepEqual(scan.violations, [], `Tour accessibility: ${id}, ${theme}`);
      }
    }
    const imageLink = page.locator('#tour-panel-shell [data-tour-image]');
    await imageLink.click();
    const dialog = page.getByRole('dialog', { name: 'Shell — full-size screenshot' });
    assert.ok(await dialog.isVisible());
    await dialog.locator('img').evaluate(img => img.decode());
    assert.ok((await dialog.locator('img').evaluate(img => img.currentSrc)).endsWith('/shell-dark.webp'));
    assert.ok((await dialog.getByRole('link', { name: 'Open original' }).getAttribute('href')).endsWith('/shell-dark.webp'));
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
  // Drive real Chromium touch input, not synthetic pointer handlers. Horizontal
  // swipes change slides; vertical gestures must still scroll the page.
  const mobile = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, reducedMotion: 'reduce' });
  try {
    const page = await mobile.newPage();
    await page.goto(origin);
    const tour = page.locator('[data-product-tour]');
    await tour.scrollIntoViewIfNeeded();
    const cdp = await mobile.newCDPSession(page);
    const swipe = async (dx, dy = 0) => {
      const box = await page.locator('.tour-window').boundingBox();
      const x = box.x + box.width / 2;
      const y = box.y + box.height / 3;
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y }] });
      for (let step = 1; step <= 8; step++) {
        await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: x + dx * step / 8, y: y + dy * step / 8 }] });
      }
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
    };
    const selected = id => page.locator(`#tour-tab-${id}[aria-selected="true"]`).waitFor();
    await swipe(-120);
    await selected('hub');
    await swipe(120);
    await selected('review');
    await swipe(120);
    await selected('shell');
    assert.ok(Math.abs((await page.locator('#tour-panel-shell').boundingBox()).x - (await page.locator('.tour-window').boundingBox()).x) < 1, 'Reduced-motion wrap shows the real interactive slide, not an inert copy');
    assert.equal(await page.locator('[data-tour-lightbox]').evaluate(el => el.open), false, 'A swipe must not open the image');
    const before = await page.evaluate(() => scrollY);
    await swipe(0, -100);
    await page.waitForFunction(y => scrollY > y + 20, before);
    await selected('shell');
    await tour.scrollIntoViewIfNeeded();
    await page.screenshot({ path: path.join(results, 'tour-swipe-mobile.png') });
  } finally {
    await mobile.close();
  }
  // Prevent accidental reintroduction of multi-hundred-KB PNG previews.
  for (const id of ['review', 'hub', 'worktrees', 'agents', 'shell']) {
    for (const theme of ['light', 'dark']) {
      const prefix = new URL(`../static/images/tour/${id}-${theme}`, import.meta.url).href;
      assert.ok((await stat(new URL(`${prefix}-900.webp`))).size < 45000, 'Preview image budget');
      assert.ok((await stat(new URL(`${prefix}.webp`))).size < 120000, 'Lossless full-size image budget');
    }
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
  console.log('✓ Five-scene tour: sliding, pointer autoplay, dots, pause, keyboard, themes, enlargement, reduced motion, compact viewports, native touch swipes, image budgets and no-JS fallback');
}
