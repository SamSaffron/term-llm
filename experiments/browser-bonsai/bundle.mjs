import { build } from 'esbuild';
import { mkdir, copyFile, cp } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
const root = dirname(fileURLToPath(import.meta.url));
const out = process.argv[2] ?? join(root, 'web');
await mkdir(out, { recursive: true });
// The entry imports the vendored kernels.ts; esbuild erases its types and
// inlines the Talk engine into one self-contained browser module.
await build({ entryPoints: [join(root, 'web/inference.mjs')], outfile: join(out, 'inference.bundle.mjs'), bundle: true, platform: 'browser', format: 'esm', target: ['chrome120', 'safari18'], minify: true, sourcemap: false, conditions: ['browser'] });
for (const name of ['boot.mjs', 'worker.mjs', 'transport.mjs', 'prose-stream.mjs', 'linux-client.mjs', 'linux-worker.mjs', 'linux.mjs', 'linux-assets.mjs', 'python-runtime.mjs']) if (out !== join(root, 'web')) await copyFile(join(root, 'web', name), join(out, name));
// The pinned CPython overlay is a same-origin static asset loaded only when the
// guest first boots. It lives beside the JS modules so import.meta.url resolves.
if (out !== join(root, 'web')) await cp(join(root, 'web/runtime'), join(out, 'runtime'), { recursive: true });
console.log(`Browser inference bundle written to ${out}`);
await mkdir(join(out, 'licenses'), { recursive: true });
for (const [source, name] of [
  ['vendor/LICENSE', 'PrismML-Talk-LICENSE.txt'],
  ['vendor/NOTICE', 'PrismML-Talk-NOTICE.txt'],
  ['vendor/README.md', 'PrismML-Talk-provenance.md'],
  ['vendor/model/LICENSE', 'Bonsai2-LICENSE.txt'],
  ['vendor/model/NOTICE.txt', 'Bonsai2-NOTICE.txt'],
  ['licenses/Python-runtime.md', 'Python-runtime.md'],
  ['licenses/Python-LICENSE.txt', 'Python-LICENSE.txt'],
  ['licenses/musl-COPYRIGHT.txt', 'musl-COPYRIGHT.txt'],
  ['licenses/zlib-LICENSE.txt', 'zlib-LICENSE.txt'],
]) await copyFile(join(root, source), join(out, 'licenses', name));

for (const name of ['libv86.mjs', 'v86.wasm']) await copyFile(join(root, 'node_modules/v86/build', name), join(out, name));
await copyFile(join(root, 'node_modules/v86/LICENSE'), join(out, 'licenses/v86-LICENSE.txt'));
await copyFile(join(root, 'LINUX-SOURCES.md'), join(out, 'licenses/Linux-provenance.md'));
