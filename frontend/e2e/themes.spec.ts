import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { expect, test } from '@playwright/test';

test.use({ video: 'off' });

function stylesheet(path: string): string {
  return readFileSync(path, 'utf8').replace(
    /@import\s+['"]([^'"]+)['"];/g,
    (_, specifier: string) => stylesheet(resolve(dirname(path), specifier)),
  );
}

const css =
  stylesheet(resolve(import.meta.dirname, '../src/styles/app.css')) +
  stylesheet(resolve(import.meta.dirname, '../src/theme/themes.css'));
if (/@import\b/.test(css)) throw new Error('Unsupported CSS import in browser style fixture');

test('selected themes keep text and controls readable in either system color scheme', async ({
  page,
}) => {
  const surfaces = ['--bg', '--surface', '--surface-2', '--surface-3'];
  await page.setContent(
    surfaces
      .flatMap((background) =>
        ['--text', '--text-muted'].map(
          (color) =>
            `<p data-contrast style="background:var(${background});color:var(${color})">${color} on ${background}</p>`,
        ),
      )
      .join('') +
      `<button data-contrast class="send-btn">Send</button>
      <button data-contrast class="diff-queue-send">Send comments</button>
      <div class="worktree-modal"><button data-contrast class="btn danger">Remove worktree</button></div>
      <div class="composer-box"><button data-contrast class="stop-btn visible">Stop</button></div>`,
  );
  // Measure settled colors rather than intermediate theme-transition frames.
  await page.addStyleTag({ content: `${css}\n[data-contrast] { transition: none; }` });
  for (const colorScheme of ['light', 'dark'] as const) {
    await page.emulateMedia({ colorScheme });
    for (const value of [
      'sage-light',
      'sage-dark',
      'catppuccin',
      'dracula',
      'nord',
      'tokyo-night',
    ]) {
      await page.evaluate((theme) => {
        document.documentElement.dataset.chatTheme = theme;
      }, value);
      await page.getByRole('button', { name: 'Stop', exact: true }).hover();
      const contrasts = await page.locator('[data-contrast]').evaluateAll((elements) => {
        const canvas = document.createElement('canvas').getContext('2d')!;
        const luminance = (color: string, background = '#fff') => {
          canvas.clearRect(0, 0, 1, 1);
          canvas.fillStyle = background;
          canvas.fillRect(0, 0, 1, 1);
          canvas.fillStyle = color;
          canvas.fillRect(0, 0, 1, 1);
          const [r, g, b] = [...canvas.getImageData(0, 0, 1, 1).data].slice(0, 3).map((channel) => {
            const value = channel / 255;
            return value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4;
          });
          return r * 0.2126 + g * 0.7152 + b * 0.0722;
        };
        return elements.map((element) => {
          const style = getComputedStyle(element);
          const foreground = luminance(style.color);
          const background = luminance(
            style.backgroundColor,
            getComputedStyle(element.parentElement!).backgroundColor,
          );
          return {
            label: element.textContent,
            ratio:
              (Math.max(foreground, background) + 0.05) / (Math.min(foreground, background) + 0.05),
          };
        });
      });
      for (const { label, ratio } of contrasts) {
        expect(ratio, `${value}, ${colorScheme}, ${label}`).toBeGreaterThanOrEqual(4.5);
      }
    }
  }
});

test('later extension styles can override a built-in theme', async ({ page }) => {
  await page.setContent('<p>Theme override</p>');
  await page.addStyleTag({ content: css });
  await page.evaluate(() => {
    document.documentElement.dataset.chatTheme = 'catppuccin';
  });
  await page.addStyleTag({ content: ':root { --bg: #123456; }' });
  await expect(page.locator('body')).toHaveCSS('background-color', 'rgb(18, 52, 86)');
});
