// Generate responsive WebP tour assets.
//
// By default, optimize the browser scenes captured by capture-tour.mjs. Pass
// --terminal-source PATH to publish an approved standalone terminal PNG instead.
import { mkdir } from 'node:fs/promises';
import path from 'node:path';
import { parseArgs } from 'node:util';
import sharp from 'sharp';

const SOURCE_WIDTH = 2560;
const SOURCE_HEIGHT = 1360;
const PREVIEW_WIDTH = 900;
const PREVIEW_HEIGHT = 478;

const { values } = parseArgs({ options: { 'terminal-source': { type: 'string' } } });

const site = new URL('..', import.meta.url).pathname;
const source = path.join(site, 'test-results', 'tour-source');
const target = path.join(site, 'static', 'images', 'tour');
await mkdir(target, { recursive: true });

const images = values['terminal-source']
  ? [['terminal-dark', path.resolve(values['terminal-source'])]]
  : ['review', 'hub', 'worktrees', 'agents', 'shell'].flatMap(scene =>
      ['light', 'dark'].map(theme => [`${scene}-${theme}`, path.join(source, `${scene}-${theme}.png`)]));

for (const [name, file] of images) {
  const image = sharp(file);
  const { width, height } = await image.metadata();
  if (width !== SOURCE_WIDTH || height !== SOURCE_HEIGHT) {
    throw new Error(`${file}: expected ${SOURCE_WIDTH}×${SOURCE_HEIGHT}, got ${width}×${height}`);
  }
  // Full-size text remains pixel-identical; compact previews use high-quality WebP.
  await image.clone().webp({ lossless: true, effort: 6 }).toFile(path.join(target, `${name}.webp`));
  await image
    .clone()
    .resize(PREVIEW_WIDTH, PREVIEW_HEIGHT, { kernel: 'lanczos3' })
    .webp({ quality: 90, effort: 6 })
    .toFile(path.join(target, `${name}-900.webp`));
  console.log(name, 'optimized');
}
