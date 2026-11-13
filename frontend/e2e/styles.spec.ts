import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { expect, test } from '@playwright/test';
import { buildSync } from 'esbuild';

test.use({ video: 'off' });

function stylesheet(path: string): string {
  return readFileSync(path, 'utf8').replace(
    /@import\s+['"]([^'"]+)['"];/g,
    (_, specifier: string) => stylesheet(resolve(dirname(path), specifier)),
  );
}

const css = stylesheet(resolve(import.meta.dirname, '../src/styles/app.css'));
if (/@import\b/.test(css)) throw new Error('Unsupported CSS import in browser style fixture');

// The production Markdown renderer as a page script, exposed as `markdownFixture`.
function markdownRenderer(): string {
  return buildSync({
    entryPoints: [resolve(import.meta.dirname, '../src/domain/markdown.ts')],
    bundle: true,
    write: false,
    format: 'iife',
    globalName: 'markdownFixture',
    external: ['./rich-highlight', './rich-katex'],
  }).outputFiles[0].text;
}

test('diff action glyphs retain shared desktop sizing and their touch override', async ({
  page,
}) => {
  await page.setContent(`<meta name="viewport" content="width=device-width, initial-scale=1">
    <button class="icon-btn"><svg aria-hidden="true"></svg></button>
    <div class="diff-file-row"><div class="diff-file-actions">
      <button class="diff-action-btn"><svg aria-hidden="true"></svg></button>
    </div></div>`);
  await page.addStyleTag({ content: css });
  const touch = await page.evaluate(() => matchMedia('(hover: none), (pointer: coarse)').matches);
  for (const width of [360, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    const sizes = await page.evaluate(() => {
      const size = (selector: string) => {
        const style = getComputedStyle(document.querySelector(selector)!);
        return { width: parseFloat(style.width), height: parseFloat(style.height) };
      };
      return { shared: size('.icon-btn svg'), action: size('.diff-action-btn svg') };
    });
    expect(sizes.shared.width).toBeCloseTo(16.8, 1);
    expect(sizes.shared.height).toBeCloseTo(16.8, 1);
    expect(sizes.action.width).toBeCloseTo(touch ? 17 : sizes.shared.width, 1);
    expect(sizes.action.height).toBeCloseTo(touch ? 17 : sizes.shared.height, 1);
  }
});

test('conversation text uses a readable 16px base on desktop and mobile', async ({ page }) => {
  await page.setContent(`<meta name="viewport" content="width=device-width, initial-scale=1">
    <article class="message user"><div class="message-body">Explain typography.</div></article>
    <article class="message assistant"><div class="message-body"><div class="markdown-body">
      <p>Typography makes written communication clear and readable.</p>
      <ul><li>Use a consistent type size.</li></ul>
      <p>Keep <code>inline code</code> proportional.</p>
      <pre><code>const size = 16;</code></pre>
    </div></div></article>`);
  await page.addStyleTag({ content: css });
  for (const width of [320, 390, 768, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    for (const selector of ['.message-body', '.markdown-body p', '.markdown-body li']) {
      for (const element of await page.locator(selector).all()) {
        await expect(element).toHaveCSS('font-size', '16px');
        await expect(element).toHaveCSS('line-height', '25.6px');
      }
    }
    await expect(page.locator('.markdown-body p code')).toHaveCSS('font-size', '14px');
    await expect(page.locator('.markdown-body pre code')).toHaveCSS('font-size', '14px');
    expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBe(0);
  }
});

test('ordered list markers fit inside assistant messages on desktop and mobile', async ({
  page,
}) => {
  const renderer = markdownRenderer();
  const items = Array.from({ length: 20 }, (_, index) => `<li>Element ${index + 1}</li>`).join('');
  await page.setContent(`<meta name="viewport" content="width=device-width, initial-scale=1">
    <article class="message assistant"><div class="message-body"><div class="markdown-body"></div></div></article>`);
  await page.addScriptTag({ content: renderer });
  await page.evaluate((items) => {
    // The fixture bundle exposes the production renderer without a running server.
    const fixture = (
      window as unknown as { markdownFixture: { renderMarkdown: (source: string) => string } }
    ).markdownFixture;
    document.querySelector('.markdown-body')!.innerHTML = fixture.renderMarkdown(`
<ol><li>Short list</li></ol>
<ol>${items}</ol>
<ol start="98"><li>Nested list<ol>${items}</ol></li><li>Element</li><li>Element</li></ol>
<ol start="198"><li>Element</li><li>Element</li><li>Element</li></ol>
<ol start="998"><li>Element</li><li>Element</li><li>Element</li></ol>
<ol start="999999998"><li>Element</li><li>Element</li><li>Element</li></ol>
<ol reversed start="1000"><li>Element</li><li>Element</li></ol>
<ol><li value="-10000">Element</li><li value="99999">Element</li><li>Element</li></ol>`);
  }, items);
  await page.addStyleTag({ content: css });
  for (const width of [320, 390, 768, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    for (const fontSize of [16, 24]) {
      await page.locator('.markdown-body').evaluate((element, size) => {
        element.style.fontSize = `${size}px`;
      }, fontSize);
      const shortListPadding = await page
        .locator('ol')
        .first()
        .evaluate((list) => parseFloat(getComputedStyle(list).paddingInlineStart));
      expect(
        shortListPadding,
        'single-digit lists should not reserve a large-number gutter',
      ).toBeLessThan(2 * fontSize);
      const clearances = await page.locator('ol').evaluateAll((lists) => {
        const context = document.createElement('canvas').getContext('2d')!;
        return lists.map((list) => {
          const style = getComputedStyle(list);
          context.font = `${style.fontWeight} ${style.fontSize} ${style.fontFamily}`;
          const ordered = list as HTMLOListElement;
          const items = [...list.children].filter((child) => child.tagName === 'LI');
          let number = ordered.hasAttribute('start')
            ? ordered.start
            : ordered.reversed
              ? items.length
              : 1;
          let markerWidth = 0;
          for (const item of items) {
            if (item.hasAttribute('value')) number = (item as HTMLLIElement).value;
            markerWidth = Math.max(markerWidth, context.measureText(`${number}. `).width);
            number += ordered.reversed ? -1 : 1;
          }
          return parseFloat(style.paddingInlineStart) - markerWidth;
        });
      });
      for (const clearance of clearances) {
        expect(clearance, `${width}px viewport, ${fontSize}px text`).toBeGreaterThanOrEqual(0);
      }
      expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBe(0);
    }
  }
});

test('Markdown tables size columns to content, break long tokens and scroll when wide', async ({
  page,
}) => {
  await page.setContent(`<meta name="viewport" content="width=device-width, initial-scale=1">
    <article class="message assistant"><div class="message-body"><div class="markdown-body"></div></div></article>`);
  await page.addScriptTag({ content: markdownRenderer() });
  await page.evaluate(() => {
    const fixture = (
      window as unknown as { markdownFixture: { renderMarkdown: (source: string) => string } }
    ).markdownFixture;
    document.querySelector('.markdown-body')!.innerHTML = fixture.renderMarkdown(`
| ID | Status | Summary |
| --- | --- | --- |
| T-101 | Done | Let the sessions sidebar be resized so long chat titles stay readable on wide screens |

| File | Link |
| --- | --- |
| \`frontend/src/components/conversation/transcript/MarkdownTableScrollContainer.tsx\` | https://example.com/builds/2026-11-11/artifacts/frontend-bundle-report.html |
| Build hash | sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08 |

| Model | Provider | Context window | Input per million | Output per million | Median first token | Tool calls | Vision | Notes |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| alpha-large | Example Cloud | 1,000,000 tokens | $3.00 | $15.00 | 820 ms | Parallel | Yes | Best for long refactors |`);
  });
  await page.addStyleTag({ content: css });
  for (const width of [390, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    const [tickets, files, models] = await page
      .locator('.markdown-table-scroll')
      .evaluateAll((boxes) =>
        boxes.map((box) => {
          const lines = (cell: Element) => {
            const range = document.createRange();
            range.selectNodeContents(cell);
            return new Set([...range.getClientRects()].map((rect) => Math.round(rect.top))).size;
          };
          return {
            overflow: box.scrollWidth - box.clientWidth,
            columns: [...box.querySelectorAll('th')].map((th) => th.getBoundingClientRect().width),
            // A cell holding one word or ID (no spaces, no break points) belongs on one line.
            splitWords: [...box.querySelectorAll('th, td')]
              .filter((cell) => !cell.querySelector('wbr') && !/\s/.test(cell.textContent!.trim()))
              .filter((cell) => lines(cell) > 1)
              .map((cell) => cell.textContent),
          };
        }),
      );
    const context = `${width}px viewport`;
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth - innerWidth),
      context,
    ).toBe(0);
    expect(tickets.overflow, context).toBeLessThanOrEqual(1);
    expect(tickets.columns[0], `${context}: the ID column hugs its content`).toBeLessThan(
      tickets.columns[2] / 3,
    );
    expect(files.overflow, `${context}: paths, links and hashes wrap`).toBeLessThanOrEqual(1);
    expect(models.overflow > 1, `${context}: only a table too wide to fit scrolls`).toBe(
      width < 768,
    );
    for (const table of [tickets, files, models]) expect(table.splitWords, context).toEqual([]);
  }
});

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
      const size = width <= 700 || layout.touch ? 44 : 32;
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

test('linked-device iPhone composer keeps Send right aligned without attachment or voice controls', async ({
  page,
}) => {
  await page.setContent(`<meta name="viewport" content="width=device-width, initial-scale=1"><footer class="composer"><div class="composer-inner"><div class="composer-box">
    <textarea class="prompt" rows="1" aria-label="Message"></textarea>
    <div class="composer-actions"><button class="send-btn" aria-label="Send">↑</button></div>
  </div></div></footer>`);
  await page.addStyleTag({ content: css });
  for (const width of [320, 390, 768, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    for (const value of ['', 'Hi', 'A longer\nmessage\nwith lines', 'unbroken'.repeat(80)]) {
      await page.getByRole('textbox', { name: 'Message' }).fill(value);
      const layout = await page.locator('.composer-box').evaluate((element) => {
        const box = element.getBoundingClientRect();
        const style = getComputedStyle(element);
        const input = element.querySelector('textarea')!.getBoundingClientRect();
        const send = element.querySelector('.send-btn')!.getBoundingClientRect();
        return {
          rightGap: box.right - send.right,
          expectedGap: parseFloat(style.paddingRight) + parseFloat(style.borderRightWidth),
          inputWidth: input.width,
          availableWidth:
            box.width -
            send.width -
            2 * (parseFloat(style.paddingLeft) + parseFloat(style.borderLeftWidth)),
          inputOverlapsSend: input.right > send.left,
          overflow: document.documentElement.scrollWidth - innerWidth,
        };
      });
      const context = `${width}px, ${value.slice(0, 30)}`;
      expect(Math.abs(layout.rightGap - layout.expectedGap), context).toBeLessThanOrEqual(1);
      expect(Math.abs(layout.inputWidth - layout.availableWidth), context).toBeLessThanOrEqual(1);
      expect(layout.inputOverlapsSend, context).toBe(false);
      expect(layout.overflow, context).toBeLessThanOrEqual(1);
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

test('default light and dark controls keep readable text', async ({ page }) => {
  await page.setContent(
    `<button data-contrast class="diff-queue-send">Send comments</button>
    <div class="worktree-modal"><button data-contrast class="btn danger">Remove worktree</button></div>`,
  );
  // Measure settled colors rather than intermediate transition frames.
  await page.addStyleTag({ content: `${css}\n[data-contrast] { transition: none; }` });
  for (const colorScheme of ['light', 'dark'] as const) {
    await page.emulateMedia({ colorScheme });
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
      expect(ratio, `${colorScheme}, ${label}`).toBeGreaterThanOrEqual(4.5);
    }
  }
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

test('tool group icons centre on the tool name in any font', async ({ page }) => {
  const entry = `<div class="tool-group-entry"><span class="tool-entry-icon" aria-hidden="true"><svg class="glyph-icon" data-icon="terminal" viewBox="0 0 24 24"></svg></span><div class="tool-group-entry-body"><article class="message tool tool-done"><div class="tool-card"><button class="tool-toggle" type="button"><span class="tool-name">shell</span><span class="tool-summary">command: ls</span></button></div></article></div></div>`;
  await page.setContent(
    `<main class="chat-scroll"><div class="messages"><div class="tool-group"><article class="tool-group-card"><div class="tool-group-details open">${entry}</div></article></div></div></main>`,
  );
  await page.addStyleTag({ content: css });
  // Fonts differ in ascent, descent and line gap; the icon must follow the text, not them.
  for (const font of ['sans-serif', 'serif', 'monospace']) {
    await page.addStyleTag({ content: `body { font-family: ${font} !important; }` });
    const offset = await page.evaluate(() => {
      const name = document.querySelector('.tool-name')!;
      const probe = document.createElement('span');
      probe.style.cssText = 'display:inline-block;width:0;height:0;vertical-align:baseline';
      name.append(probe);
      const baseline = probe.getBoundingClientRect().bottom;
      probe.remove();
      const style = getComputedStyle(name);
      const context = document.createElement('canvas').getContext('2d')!;
      context.font = `${style.fontWeight} ${style.fontSize} ${style.fontFamily}`;
      const xHeight = context.measureText('x').actualBoundingBoxAscent;
      const icon = document.querySelector('.tool-entry-icon svg')!.getBoundingClientRect();
      return icon.top + icon.height / 2 - (baseline - xHeight / 2);
    });
    expect(Math.abs(offset), font).toBeLessThanOrEqual(1);
  }
});
