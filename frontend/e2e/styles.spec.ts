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

test('diff layout retains grid rows, line markers and comment placement', async ({ page }) => {
  await page.setContent(`<div class="diff-file-body">
    <div class="diff-row add"><span class="diff-ln">1</span><span class="diff-ln">2</span><span class="diff-code">added</span><button class="diff-comment-affordance">+</button></div>
    <div class="diff-row del"><span class="diff-code">deleted</span></div>
    <div class="diff-row hunk"><span class="diff-ln">1</span><span class="diff-ln">2</span><span class="diff-code">hunk</span></div>
    <div class="diff-comment-panel">Comment</div>
  </div>`);
  await page.addStyleTag({ content: css });
  const styles = await page.evaluate(() => {
    const style = (selector: string, pseudo?: string) =>
      getComputedStyle(document.querySelector(selector)!, pseudo);
    return {
      rowDisplay: style('.diff-row').display,
      rowAlign: style('.diff-row').alignItems,
      rowWhitespace: style('.diff-row').whiteSpace,
      codeOrder: style('.diff-code').order,
      addMarker: style('.add .diff-code', '::before').content,
      deleteMarker: style('.del .diff-code', '::before').content,
      hunkSecondNumber: style('.hunk .diff-ln:nth-child(2)').display,
      commentColumn: style('.diff-comment-panel').gridColumn,
      commentWhitespace: style('.diff-comment-panel').whiteSpace,
    };
  });
  expect(styles).toEqual({
    rowDisplay: 'grid',
    rowAlign: 'stretch',
    rowWhitespace: 'pre-wrap',
    codeOrder: '0',
    addMarker: '"+"',
    deleteMarker: '"−"',
    hunkSecondNumber: 'none',
    commentColumn: '3 / -1',
    commentWhitespace: 'normal',
  });
});

test('MCP, widget and project labels retain ellipsis clipping', async ({ page }) => {
  const classes = [
    'mcp-row-name',
    'mcp-row-meta',
    'widget-card-name',
    'widget-card-meta',
    'widget-card-error',
    'project-choice-name',
    'project-choice-path',
  ];
  await page.setContent(classes.map((name) => `<div class="${name}">Long label</div>`).join(''));
  await page.addStyleTag({ content: css });
  // The MCP modal loads its stylesheet separately from the main app stylesheet.
  await page.addStyleTag({
    content: stylesheet(resolve(import.meta.dirname, '../src/styles/features/mcp.css')),
  });
  for (const name of classes) {
    await expect(page.locator(`.${name}`)).toHaveCSS('overflow', 'hidden');
    await expect(page.locator(`.${name}`)).toHaveCSS('text-overflow', 'ellipsis');
    await expect(page.locator(`.${name}`)).toHaveCSS('white-space', 'nowrap');
  }
});

test('composer controls stay aligned and inside the input at every size', async ({ page }) => {
  await page.setContent(`<meta name="viewport" content="width=device-width, initial-scale=1"><footer class="composer"><div class="composer-inner"><div class="composer-box">
    <button class="composer-icon-btn attach-btn" aria-label="Add">+</button>
    <textarea class="prompt" rows="1" aria-label="Message"></textarea>
    <div class="composer-actions"><button class="stop-btn visible">Stop</button><button class="composer-icon-btn voice-btn" aria-label="Record">●</button><button class="send-btn" aria-label="Send">↑</button></div>
  </div></div></footer>`);
  await page.addStyleTag({ content: css });
  for (const width of [320, 390, 768, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    for (const value of ['Hi', 'A longer\nmessage\nwith lines', 'unbroken'.repeat(80)]) {
      await page.getByRole('textbox', { name: 'Message' }).fill(value);
      const layout = await page.locator('.composer-box').evaluate((element) => {
        const prompt = element.querySelector('textarea')!;
        const input = prompt.getBoundingClientRect();
        const style = getComputedStyle(prompt);
        const lastLineCenter =
          input.bottom - parseFloat(style.paddingBottom) - parseFloat(style.lineHeight) / 2;
        const box = element.getBoundingClientRect();
        const controls = [...element.querySelectorAll('button')].map((button) => {
          const rect = button.getBoundingClientRect();
          return {
            label: button.getAttribute('aria-label') || button.textContent,
            offset: Math.abs(rect.top + rect.height / 2 - lastLineCenter),
            height: rect.height,
            width: rect.width,
            inside: rect.left >= box.left && rect.right <= box.right,
            clearOfInput: rect.right <= input.left || rect.left >= input.right,
          };
        });
        return {
          inputWidth: input.width,
          touch: matchMedia('(hover: none), (pointer: coarse)').matches,
          overflow: document.documentElement.scrollWidth - innerWidth,
          controls,
        };
      });
      const context = `${width}px, ${value.slice(0, 30)}`;
      expect(layout.inputWidth, context).toBeGreaterThan(0);
      expect(layout.overflow, context).toBeLessThanOrEqual(1);
      const size = width <= 700 || layout.touch ? 44 : 36;
      for (const control of layout.controls) {
        const label = `${context}, ${control.label}`;
        expect(control.offset, label).toBeLessThanOrEqual(1);
        expect(control.height, label).toBe(size);
        expect(control.width, label).toBeGreaterThanOrEqual(size);
        expect(control.inside, label).toBe(true);
        expect(control.clearOfInput, label).toBe(true);
      }
    }
  }
});

test('composer attachments and status chips keep clearance above the input', async ({ page }) => {
  await page.setContent(`<meta name="viewport" content="width=device-width, initial-scale=1"><footer class="composer"><div class="composer-inner">
    <button class="interaction-attention-banner">Decision waiting — Open</button>
    <div class="attachments"><div class="attachment-chip"><span class="att-name">notes.txt</span></div></div>
    <button class="approval-mode-chip">Approvals · Prompt for this chat</button>
    <button class="goal-chip">Goal · active · Review the interface</button>
    <div class="composer-box"><textarea class="prompt" rows="1" aria-label="Message"></textarea></div>
  </div></footer>`);
  await page.addStyleTag({ content: css });
  for (const width of [320, 390, 768, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    const clearances = await page.locator('.composer-inner').evaluate((element) => {
      const boxes = [...element.children].map((child) => child.getBoundingClientRect());
      return boxes.slice(1).map((box, index) => box.top - boxes[index].bottom);
    });
    for (const clearance of clearances) expect(clearance, `${width}px`).toBeGreaterThanOrEqual(7);
  }
});

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

test('mobile composer keeps its bottom inset clear of the input', async ({ page }) => {
  await page.setContent(`<meta name="viewport" content="width=device-width, initial-scale=1"><footer class="composer"><div class="composer-inner"><div class="composer-box">
    <textarea class="prompt" rows="1" aria-label="Message"></textarea>
  </div></div></footer>`);
  await page.addStyleTag({ content: css });
  for (const width of [320, 390]) {
    await page.setViewportSize({ width, height: 900 });
    for (const inset of [0, 34]) {
      await page.evaluate((inset) => {
        document.documentElement.style.setProperty('--safe-bottom', `${inset}px`);
      }, inset);
      const clearance = await page.locator('.composer').evaluate((element) => {
        const box = element.querySelector('.composer-box')!.getBoundingClientRect();
        return element.getBoundingClientRect().bottom - box.bottom;
      });
      expect(clearance, `${width}px, ${inset}px safe area`).toBeGreaterThanOrEqual(
        Math.max(16, inset),
      );
    }
  }
});
