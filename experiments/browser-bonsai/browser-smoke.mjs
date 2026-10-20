// Routes ONLY static POC files into a fresh context of the authenticated shared
// browser. Inference/model downloads run in that browser, not in Node.
// source browser.env, export its vars, then node browser-smoke.mjs [static-dir] [--model]
import { readFile, writeFile, mkdir } from 'node:fs/promises';
import { resolve, extname, sep } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
const here = fileURLToPath(new URL('.', import.meta.url));
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE ?? pathToFileURL(resolve(here, '../../frontend/node_modules/playwright-core/index.mjs')).href);
const root = resolve(process.argv[2] ?? here + 'dist');
const withModel = process.argv.includes('--model');
const base = (process.env.JARVIS_BROWSER_BASE_URL ?? 'http://host.docker.internal:8787').replace(/\/$/, '');
const token = process.env.JARVIS_BROWSER_TOKEN;
if (!token) throw new Error('JARVIS_BROWSER_TOKEN must be loaded from browser.env');
const prefix = 'https://term-llm.com/__browser-bonsai-poc__/';
const evidence = resolve(here, 'evidence');
await mkdir(evidence, { recursive: true });
const result = { withModel, root, started: new Date().toISOString(), console: [], errors: [], remoteRequests: [], phases: [] };
let browser, context;
try {
  const response = await fetch(base + '/jarvis-browser/json/version', { headers: { Authorization: `Bearer ${token}` } });
  if (!response.ok) throw new Error(`Browser proxy HTTP ${response.status}`);
  const version = await response.json();
  const ws = new URL(version.webSocketDebuggerUrl);
  const proxy = new URL(base);
  proxy.protocol = proxy.protocol === 'https:' ? 'wss:' : 'ws:';
  proxy.pathname = ws.pathname;
  proxy.searchParams.set('token', token);
  browser = await chromium.connectOverCDP(proxy.href);
  context = await browser.newContext({ serviceWorkers: 'block' });
  await context.route(prefix + '**', async route => {
    try {
      const path = decodeURIComponent(new URL(route.request().url()).pathname.slice(new URL(prefix).pathname.length)) || 'index.html';
      const filename = resolve(root, path);
      if (!filename.startsWith(root + sep)) throw new Error('Invalid static path');
      const types = { '.html': 'text/html', '.js': 'application/javascript', '.mjs': 'application/javascript', '.css': 'text/css', '.json': 'application/json', '.wasm': 'application/wasm', '.png': 'image/png', '.woff2': 'font/woff2' };
      await route.fulfill({ status: 200, body: await readFile(filename), contentType: types[extname(filename)] ?? 'application/octet-stream' });
    } catch (error) { result.errors.push(`Static route: ${error.message}`); await route.fulfill({ status: 404, body: error.message }); }
  });
  // A real network request here would violate the static-only architecture.
  await context.route('https://term-llm.com/demo-api/**', async route => {
    result.errors.push(`LEAKED API REQUEST ${route.request().url()}`);
    await route.abort();
  });
  const page = await context.newPage();
  page.on('console', message => { if (result.console.length < 150) result.console.push({ type: message.type(), text: message.text() }); });
  page.on('pageerror', error => result.errors.push(error.message));
  page.on('request', request => {
    if (!request.url().startsWith(prefix)) result.remoteRequests.push({ method: request.method(), url: request.url() });
  });
  await page.goto(prefix, { waitUntil: 'domcontentloaded' });
  result.capabilities = await page.evaluate(async () => {
    const adapter = await navigator.gpu?.requestAdapter();
    return { gpu: !!adapter, features: adapter ? [...adapter.features] : [] };
  });
  console.log('capabilities', JSON.stringify(result.capabilities));
  await page.getByRole('button', { name: withModel ? 'Load Bonsai & start demo' : 'Inspect workspace only', exact: true }).click();
  result.phases.push('clicked-start');
  await page.waitForFunction(() => window.__termLLMDemo || window.__termLLMDemoError, null, { timeout: withModel ? 1800000 : 120000 });
  const gate = await page.locator('#demo-progress').textContent().catch(() => null);
  if (gate) throw new Error(`Startup did not reach frontend: ${gate}`);
  await page.getByRole('textbox', { name: 'Message', exact: true }).waitFor({ timeout: 60000 });
  result.phases.push('real-frontend-ready');
  result.initial = await page.evaluate(async () => {
    const sessions = await fetch('/demo-api/v1/sessions').then(r => r.json());
    const providers = await fetch('/demo-api/v1/providers').then(r => r.json());
    return { sessions, providers, localStorage: Object.keys(localStorage), inferenceLoaded: window.__termLLMDemo.inferenceLoaded };
  });
  await page.getByRole('textbox', { name: 'Message', exact: true }).fill('This draft must disappear on reset.');
  if (withModel) {
    await page.getByRole('textbox', { name: 'Message', exact: true }).fill('Read README.md, then create proof.txt containing exactly BROWSER_GPU_OK. Use the file tools.');
    await page.getByRole('button', { name: 'Send message', exact: true }).click();
    result.phases.push('sent-real-inference-task');
    await page.waitForFunction(() => {
      const assistant = document.querySelector('.message.assistant');
      const text = document.body.innerText;
      if (text.includes('"error":{"message"')) throw new Error(text);
      return assistant && text.includes('BROWSER_GPU_OK') && !document.querySelector('.stop-btn.visible');
    }, null, { timeout: 900000 });
    result.transcript = await page.locator('body').innerText();
    result.workspace = await page.evaluate(() => new Promise((resolve, reject) => {
      const worker = window.__termLLMDemo.worker;
      const timer = setTimeout(() => { worker.removeEventListener('message', receive); reject(new Error('Workspace introspection timed out')); }, 10000);
      function receive({ data }) {
        if (data.type !== 'workspace') return;
        clearTimeout(timer);
        worker.removeEventListener('message', receive);
        resolve(data.files);
      }
      worker.addEventListener('message', receive);
      worker.postMessage({ type: 'workspace' });
    }));
    if (!result.workspace.some(file => file.path === 'proof.txt' && file.size >= 14)) throw new Error('Model answer did not produce proof.txt in the actual WASM workspace');
    result.phases.push('real-model-file-write-verified');
  }
  await page.screenshot({ path: resolve(evidence, withModel ? 'model.png' : 'inspect.png'), fullPage: true });
  // Reload recreates Go+workspace and ephemeral Web Storage; cached model assets
  // are not cleared. No script injects model output or tool calls.
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.getByRole('button', { name: 'Inspect workspace only', exact: true }).click();
  await page.getByRole('textbox', { name: 'Message', exact: true }).waitFor({ timeout: 120000 });
  result.afterReset = await page.evaluate(async () => ({ sessions: await fetch('/demo-api/v1/sessions').then(r => r.json()), draft: document.querySelector('textarea')?.value }));
  if (result.afterReset.draft) throw new Error('Draft survived reset');
  result.phases.push('reset-verified');
  result.ok = result.errors.length === 0;
} catch (error) {
  result.ok = false;
  result.failure = String(error.stack ?? error).replaceAll(token, '[REDACTED]');
  process.exitCode = 1;
} finally {
  result.finished = new Date().toISOString();
  await writeFile(resolve(evidence, withModel ? 'model.json' : 'inspect.json'), JSON.stringify(result, null, 2));
  console.log(JSON.stringify({ ok: result.ok, phases: result.phases, failure: result.failure, errors: result.errors }));
  await context?.close();
  await browser?.close();
}
