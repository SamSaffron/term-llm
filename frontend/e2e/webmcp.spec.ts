import { expect, test, type Page } from '@playwright/test';

// Page-provided (WebMCP) tools across the real browser, Go server, and debug
// provider started by scripts/browser_lifecycle_smoke.sh. The page tool below
// stands in for what the term-llm iOS app injects.

declare global {
  interface Window {
    __pingCalls: unknown[];
  }
}

async function installPageTool(page: Page): Promise<void> {
  await page.addInitScript(() => {
    window.__pingCalls = [];
    const tools = new Map<string, Record<string, unknown>>();
    const context = Object.assign(new EventTarget(), {
      async getTools() {
        return [...tools.values()];
      },
      async executeTool(tool: { name: string }, input: unknown) {
        window.__pingCalls.push(input);
        return `pong from ${tool.name}`;
      },
    });
    tools.set('ping', {
      name: 'ping',
      description: 'Check that the phone is reachable.',
      inputSchema: { type: 'object', properties: { message: { type: 'string' } } },
      annotations: { readOnlyHint: true },
    });
    Object.defineProperty(document, 'modelContext', { value: context, configurable: true });
    Object.defineProperty(window, '__termLLMDeviceTools', { value: { device: 'iPhone' } });
  });
}

async function send(page: Page, prompt: string): Promise<void> {
  const composer = page.getByRole('textbox', { name: 'Message' });
  await expect(composer).toBeVisible();
  await composer.fill(prompt);
  await page.getByRole('button', { name: 'Send message' }).click();
}

test('runs a page tool the model calls and continues the response', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'the client tool protocol is covered once');
  await installPageTool(page);
  const continuation = page.waitForRequest(
    (request) =>
      request.method() === 'POST' &&
      new URL(request.url()).pathname.endsWith('/v1/responses') &&
      (request.postData() || '').includes('function_call_output'),
  );
  await page.goto('./?new=1');

  await send(page, 'call webmcp__ping message=hi');

  const request = await continuation;
  expect((await request.response())?.status()).toBe(200);
  const body = JSON.parse(request.postData() || '{}') as Record<string, unknown>;
  expect(body.input).toEqual([
    { type: 'function_call_output', call_id: expect.any(String), output: 'pong from ping' },
  ]);
  expect(body.previous_response_id).toEqual(expect.any(String));

  // The debug provider answers a tool result with this text, so seeing it
  // proves the page ran the tool and posted a function_call_output.
  await expect(page.getByText('Debug: Tool execution completed successfully.')).toBeVisible({
    timeout: 15_000,
  });
  expect(await page.evaluate(() => window.__pingCalls)).toEqual([{ message: 'hi' }]);
  await expect(page.getByRole('button', { name: 'Send message' })).toBeVisible();

  await page.reload();
  await expect(page.getByText('Debug: Tool execution completed successfully.').last()).toBeVisible({
    timeout: 15_000,
  });
});

test('lists page tools as an MCP server that can be turned off per conversation', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'the MCP dialog row is covered once');
  await installPageTool(page);
  await page.goto('./?new=1');

  await page.getByRole('button', { name: 'Manage MCP servers' }).click();
  const dialog = page.getByRole('dialog', { name: 'MCP servers' });
  await expect(dialog.getByText('iPhone')).toBeVisible();
  await expect(dialog.getByText('1 tool · WebMCP from this page')).toBeVisible();
  // A real click on the switch (the thumb sits at its centre when on).
  await dialog.getByRole('checkbox', { name: 'Disable iPhone' }).uncheck();
  await expect(dialog.getByRole('checkbox', { name: 'Enable iPhone' })).not.toBeChecked();
  await expect(dialog.getByText('0 of 1 on')).toBeVisible();
  await page.keyboard.press('Escape');

  // Without the tool on offer, the debug provider falls back to plain output.
  await send(page, 'call webmcp__ping message=hi');
  await expect(page.getByText('Debug Provider Output')).toBeVisible({ timeout: 15_000 });
  expect(await page.evaluate(() => window.__pingCalls)).toEqual([]);
});
