/**
 * Generates public/images/og.png — the default OpenGraph image.
 * Black ground, white Kno symbol, display-type tagline. Run:
 *   pnpm og:generate
 * The PNG is committed; regenerate only when brand assets change.
 */

import sharp from 'sharp';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.join(path.dirname(path.dirname(fileURLToPath(import.meta.url))));

const symbol = await readFile(
  path.join(root, 'public', 'images', 'brand', 'kno_symbol_white.svg'),
  'utf8',
);

const svg = `
<svg xmlns="http://www.w3.org/2000/svg" width="1200" height="630" viewBox="0 0 1200 630">
  <rect width="1200" height="630" fill="#0a0a0a"/>
  <g transform="translate(96,96) scale(0.62)">
    ${symbol.replace(/^<svg[^>]*>/, '').replace('</svg>', '')}
  </g>
  <line x1="96" y1="500" x2="1104" y2="500" stroke="#2b2b2b" stroke-width="2"/>
  <text x="96" y="552" fill="#f4f3ef" font-family="Arial, Helvetica, sans-serif" font-size="56" font-weight="700" letter-spacing="-1">Know which data actually makes your AI better.</text>
  <text x="96" y="596" fill="#9c9c94" font-family="monospace" font-size="22" letter-spacing="3">KNO / OPEN SOURCE · SINGLE GO BINARY · APACHE-2.0</text>
</svg>`;

await sharp(Buffer.from(svg))
  .png()
  .toFile(path.join(root, 'public', 'images', 'og.png'));
console.log('[og] wrote public/images/og.png');
