// Build the existing frontend first: make frontend && node demo/build.mjs
import { readFile, writeFile, mkdir, readdir, copyFile, rm } from 'node:fs/promises';
import { gunzipSync } from 'node:zlib';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { hubAssets, hubPageConfig } from './hub.mjs';
const here = dirname(fileURLToPath(import.meta.url));
const source = join(here, '../internal/serveui/static');
const out = join(here, 'dist');
const read = async path => readFile(path).catch(() => readFile(path + '.gz').then(gunzipSync));
const manifest = JSON.parse(await read(join(source, 'dist/asset-manifest.json')));
const entry = Object.values(manifest).find(value => value.isEntry && value.src === 'src/main.tsx');
if (!entry) throw Error('Build the frontend first with make frontend');
await rm(out, { recursive: true, force: true });
await mkdir(join(out, 'assets'), { recursive: true });
async function copyTree(from, to) {
  await mkdir(to, { recursive: true });
  for (const item of await readdir(from, { withFileTypes: true })) {
    const src = join(from, item.name), dst = join(to, item.name);
    if (item.isDirectory()) await copyTree(src, dst);
    else if (item.name.endsWith('.gz')) await writeFile(dst.slice(0, -3), gunzipSync(await readFile(src)));
    else await copyFile(src, dst);
  }
}
await copyTree(join(source, 'dist'), join(out, 'assets'));
let html = await readFile(join(source, 'index.html'), 'utf8');
html = html.replace('<title>Chat</title>', '<title>term-llm · UI demo</title>')
  .replace('<link rel="stylesheet" href="dist/app.css">', entry.css.map(css => `<link rel="stylesheet" href="/assets/${css}">`).join('\n'))
  .replace('<script type="module" src="dist/app.js"></script>', `<script>window.TERM_LLM_DEMO_ENTRY=${JSON.stringify('/assets/' + entry.file)}</script>\n<script type="module" src="/boot.mjs"></script>`);
html = html.replace('href="icon-512.png"', 'href="/icon-512.png"')
  .replace('href="manifest.webmanifest"', 'href="/manifest.webmanifest"')
  .replace('<div id="root">', `<div id="root" data-hub-config="${JSON.stringify(hubPageConfig).replaceAll('"', '&#34;')}">`);
if (html.includes('dist/app.')) throw Error('Stock shell asset placeholders changed');
await writeFile(join(out, 'index.html'), html);
for (const name of ['boot.mjs', 'mock-api.mjs', 'hub.mjs']) await copyFile(join(here, name), join(out, name));
await copyFile(join(source, 'icon-512.png'), join(out, 'icon-512.png'));
const webManifest = JSON.parse(await readFile(join(source, 'manifest.webmanifest'), 'utf8'));
Object.assign(webManifest, { name: 'term-llm UI demo', description: 'A scripted preview of the term-llm chat interface.' });
await writeFile(join(out, 'manifest.webmanifest'), JSON.stringify(webManifest));
for (const [route, body] of Object.entries(hubAssets)) {
  const target = join(out, route.slice(1) + '.json');
  await mkdir(dirname(target), { recursive: true });
  await writeFile(target, JSON.stringify(body, null, 2) + '\n');
}
console.log(`UI demo written to ${out} (stock frontend + scripted API; no model/runtime)`);
