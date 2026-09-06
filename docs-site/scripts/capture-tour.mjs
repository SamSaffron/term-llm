// Homepage carousel scenes: real UI, illustrative fixtures, no model calls.
// Use isolated loopback web/Hub servers as described in docs-site/README.md.
// Captures both themes at 2x resolution into the production tour image directory.
import { chromium } from 'playwright';
import { mkdir } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { createServer } from 'node:http';

const base = process.env.DOCS_PRODUCT_URL || 'http://127.0.0.1:18765/ui/';
const hub = process.env.DOCS_HUB_URL || 'http://127.0.0.1:18766/';
for (const value of [base, hub]) {
  if (!['localhost', '127.0.0.1', '[::1]'].includes(new URL(value).hostname)) throw new Error('Use isolated loopback servers only');
}
const output = fileURLToPath(new URL('../static/images/tour/', import.meta.url));
await mkdir(output, { recursive: true });
const now = Math.floor(Date.now() / 1000);
const worktreeDir = '/workspace/worktrees/fix-retry-backoff';
const text = (role, value, id) => ({ id, sequence: id - 1, role, parts: [{ type: 'text', text: value }] });
const review = [
  text('user', 'Review the retry handler. Focus on cancellation and the staged changes.', 1),
  text('assistant', '## One issue worth fixing\n\nThe retry loop **waits even after cancellation**. Replace the unconditional sleep with a cancellable wait:\n\n```go\nselect {\ncase <-ctx.Done():\n    return ctx.Err()\ncase <-time.After(backoff):\n    // Try again.\n}\n```\n\n**Test both cases:** cancellation before a request and during backoff.\n\nNo files were modified by this review.', 2),
];
const agents = [
  text('user', 'Fix the retry cancellation bug. Ask specialists to check the implementation and test coverage.', 1),
  text('assistant', 'I split the investigation into two focused tasks, then combined the findings.', 2),
  { id: 3, sequence: 2, role: 'assistant', parts: [
    ...[['codebase', 'Trace cancellation through the retry loop.', 'The backoff sleep ignores ctx.Done(). The request itself already respects cancellation.'], ['reviewer', 'Check cancellation edge cases and test coverage.', 'Add coverage for cancellation before the first request and during backoff. Keep the final upstream error.']].flatMap(([name, prompt, result], index) => [
      { type: 'tool_call', tool_call_id: `agent-${index}`, tool_name: 'spawn_agent', tool_arguments: JSON.stringify({ agent_name: name, prompt }), duration_ms: 12000 + index * 3000 },
      { type: 'tool_result', tool_call_id: `agent-${index}`, tool_name: 'spawn_agent', output: result, spawn_agent: { agent_name: name, output: result, duration_ms: 12000 + index * 3000 } },
    ]),
  ] },
  text('assistant', '## A focused fix, backed by tests\n\n- Replace the sleep with a context-aware timer.\n- Cover both cancellation paths.\n- Preserve retry limits and the final error.', 4),
];
const shellMessages = [text('user', 'Implement the cancellable retry and add regression tests.', 1), text('assistant', '## Ready to verify\n\nThe retry now respects cancellation during backoff. The new tests cover cancellation before the first request, during the wait, and retry exhaustion.\n\nOpen **/shell** to run the tests in this worktree.', 2)];
const terminal = '\x1b[1;36m~/worktrees/fix-retry-backoff\x1b[0m  \x1b[32mfix-retry-backoff\x1b[0m\r\n$ go test ./internal/retry -v\r\n\r\n=== RUN   TestCancelBeforeRequest\r\n\x1b[32m--- PASS: TestCancelBeforeRequest (0.00s)\x1b[0m\r\n=== RUN   TestCancelDuringBackoff\r\n\x1b[32m--- PASS: TestCancelDuringBackoff (0.01s)\x1b[0m\r\n=== RUN   TestRetryExhaustion\r\n\x1b[32m--- PASS: TestRetryExhaustion (0.02s)\x1b[0m\r\n\x1b[1;32mPASS\x1b[0m\r\nok   example.com/app/internal/retry   0.031s\r\n\r\n$ ';
const streamServer = createServer((req, res) => {
  res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Access-Control-Allow-Origin': '*', 'Cache-Control': 'no-cache' });
  res.write(`event: ready\ndata: ${JSON.stringify({ shell_id: 'preview-shell' })}\n\n`);
  res.write(`event: output\ndata: ${JSON.stringify({ shell_id: 'preview-shell', offset: 0, next_offset: Buffer.byteLength(terminal), data: Buffer.from(terminal).toString('base64') })}\n\n`);
  const timer = setInterval(() => res.write(': heartbeat\n\n'), 1000);
  res.on('close', () => clearInterval(timer));
});
await new Promise(resolve => streamServer.listen(0, '127.0.0.1', resolve));
const browser = await chromium.launch();
try {
  for (const theme of ['light', 'dark']) {
  for (const scene of ['review', 'worktrees', 'agents', 'shell', 'hub']) {
    const context = await browser.newContext({ viewport: { width: 1280, height: 680 }, colorScheme: theme, deviceScaleFactor: 2, serviceWorkers: 'block' });
    const page = await context.newPage();
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    const messages = scene === 'agents' ? agents : scene === 'shell' ? shellMessages : review;
    const titles = ['Review the retry handler', 'Understand authentication', 'Add cache regression tests', 'Research deployment options'];
    const sessions = titles.map((title, index) => ({ id: index ? `session-${index}` : 'review', title: index === 0 && scene === 'agents' ? 'Fix cancellation with specialist agents' : title, name: index === 0 && scene === 'agents' ? 'Fix cancellation with specialist agents' : title, number: index + 1, mode: 'chat', origin: 'web', agent: scene === 'agents' ? 'developer' : 'reviewer', provider: 'chatgpt', model: 'gpt-6-astra-fast', created_at: now - 300, last_message_at: now - 60, pinned: index === 0, archived: false, cwd: '/workspace/term-llm', worktree_dir: index === 0 ? worktreeDir : '', file_change_summary: { file_count: 1, adds: 5, dels: 1, git: true } }));
    await page.route('**/v1/**', route => {
      const path = new URL(route.request().url()).pathname;
      const json = value => route.fulfill({ contentType: 'application/json', body: JSON.stringify(value) });
      if (path.endsWith('/shell/stream')) return route.continue({ url: `http://127.0.0.1:${streamServer.address().port}/` });
      if (path.endsWith('/shell')) return json({ shell_id: 'preview-shell', cwd: worktreeDir, created: true, state: 'running' });
      if (path.endsWith('/capabilities')) return json({ projects: { enabled: false }, worktrees: { enabled: true }, shell: { enabled: true, version: 1, transport: 'http_sse' } });
      if (path.endsWith('/providers')) return json({ object: 'list', data: [{ name: 'chatgpt', configured: true, is_default: true, default_model: 'gpt-6-astra-fast', models: ['gpt-6-astra-fast'] }] });
      if (path.endsWith('/models')) return json({ object: 'list', data: [{ id: 'gpt-6-astra-fast', owned_by: 'chatgpt' }] });
      if (path.endsWith('/sidebar')) return json({ sessions, recent_sessions: sessions });
      if (path.endsWith('/sessions/status')) return json({ sessions: [] });
      if (path.endsWith('/sessions')) return json({ sessions, selected_session: sessions[0], selected_transcript: { bodies: { messages } } });
      if (path.endsWith('/state')) return json({ session: sessions[0] });
      if (path.endsWith('/transcript')) return json({ messages });
      if (path.endsWith('/worktrees')) return json({ worktrees: [
        { name: 'main', branch: 'main', dir: '/workspace/term-llm', root: true, dirty_files: 0 },
        { name: 'fix-retry-backoff', branch: 'fix-retry-backoff', dir: worktreeDir, dirty_files: 2, additions: 24, deletions: 6 },
        { name: 'cache-regression-tests', branch: 'cache-regression-tests', dir: '/workspace/worktrees/cache-regression-tests', dirty_files: 1, additions: 38, deletions: 0 },
        { name: 'docs-refresh', branch: 'docs-refresh', dir: '/workspace/worktrees/docs-refresh', dirty_files: 0 },
      ] });
      if (path.includes('/file-changes/diff')) return json({ diff: '@@ -18,5 +18,9 @@\n     if err == nil {\n         return nil\n     }\n-    time.Sleep(backoff)\n+    select {\n+    case <-ctx.Done():\n+        return ctx.Err()\n+    case <-time.After(backoff):\n+    }\n }' });
      if (path.includes('/file-changes')) return json({ files: [{ path: 'retry.go', additions: 5, deletions: 1, status: 'modified' }] });
      return json({});
    });
    if (scene === 'hub') {
      const nodes = [['macbook', 'MacBook Pro', 'developer', ['Review retries', 'Plan API migration']], ['build', 'Build server', 'developer', ['Run tests', 'Investigate flaky tests']], ['research', 'Research box', 'web-researcher', ['Research hosting', 'Summarize release notes']]].map(([id, name, agent, titles], index) => ({
        id, name, source: 'local', connection: index ? 'reverse' : 'direct', url: `http://${id}.example.test:8080`, base_path: '/ui', proxy_path: `/node/${id}/ui/`, new_session_path: `/node/${id}/ui/?new=1`, has_token: true,
        status: { reachable: true, state: 'ready', latency_ms: [4, 18, 32][index], agent, capabilities: ['web', 'jobs', 'shell'] },
        sessions: { count_label: `${[8, 12, 6][index]} sessions`, active_count: 1, unseen_count: 0, input_required_count: 0, attention_capability: 'supported', attention_last_success_at: Date.now(), recent: titles.map((title, j) => ({ id: `${id}-${j}`, number: j + 1, short_title: title, active_run: j === 0, message_count: 8 + j * 4, last_message_at: (now - 60) * 1000, resume_path: `/node/${id}/ui/chat/${j + 1}` })) },
      }));
      await page.route('**/api/**', route => {
        const path = new URL(route.request().url()).pathname;
        const data = path.endsWith('/nodes') ? { nodes } : path.endsWith('/attention') ? { total_running: 3, total_input_required: 0, total_unseen: 0, nodes: [], input_required: [], inbox: [], has_more: false } : path.endsWith('/delegations') ? { delegations: [
          { id: 'build-check', origin_node: 'macbook', target_node: 'build', agent_name: 'developer', prompt: 'Run the integration suite against the retry fix and report any regressions.', status: 'running', depth: 1, created_at: new Date().toISOString(), updated_at: new Date().toISOString() },
        ] } : {};
        return route.fulfill({ contentType: 'application/json', body: JSON.stringify(data) });
      });
      await page.goto(hub);
      await page.getByRole('heading', { name: 'MacBook Pro', exact: true }).waitFor();
    } else {
      await page.goto(new URL('chat/1', base).href);
      await page.getByText('gpt-6-astra-fast', { exact: true }).waitFor();
      if (scene === 'review') {
        await page.getByRole('button', { name: /Toggle file changes/ }).click();
        await page.locator('.diff-file-row[data-path="retry.go"]').click();
        await page.locator('.diff-row.add').first().waitFor();
      }
      if (scene === 'worktrees') {
        await page.getByRole('button', { name: 'Worktree', exact: true }).click();
        await page.getByRole('button', { name: 'All worktrees', exact: true }).click();
        await page.getByText('cache-regression-tests', { exact: true }).first().waitFor();
      }
      if (scene === 'agents') {
        const toggle = page.locator('.tool-group-toggle').first();
        await toggle.waitFor();
        if (await toggle.getAttribute('aria-expanded') !== 'true') await toggle.click();
        await page.locator('#messages').evaluate(el => { el.scrollTop = 0; });
      }
      if (scene === 'shell') {
        await page.getByRole('button', { name: 'Collapse sidebar', exact: true }).click();
        const composer = page.locator('textarea').first();
        await composer.fill('/shell');
        await composer.press('Enter');
        await page.getByRole('button', { name: 'Back to chat', exact: true }).waitFor();
        await page.getByRole('button', { name: 'Dock right', exact: true }).click();
        await page.waitForTimeout(1000); // xterm paints streamed bytes on its own render frame.
      }
    }
    await page.evaluate(() => document.fonts.ready);
    if (scene === 'review' || scene === 'agents') {
      await page.locator('#messages').evaluate(el => { el.parentElement.scrollTop = 0; });
      await page.waitForTimeout(150);
    }
    if (errors.length) throw new Error(`${scene}: ${errors.join('; ')}`);
    await page.screenshot({ path: `${output}/${scene}-${theme}.png` });
    console.log(`Captured ${scene} (${theme})`);
    await context.close();
  }
  }
} finally {
  await browser.close();
  streamServer.closeAllConnections();
  await new Promise(resolve => streamServer.close(resolve));
}
