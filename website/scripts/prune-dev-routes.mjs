/**
 * Removes development-only routes (e.g. /design) from the production build.
 *
 * The design gallery uses getStaticPaths, but Astro still emits a page for
 * paramless routes even when the path list is empty, so pruning happens
 * after the build. Also strips the route from the sitemap so search
 * engines never learn the URL.
 */

import { rm } from 'node:fs/promises';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.join(path.dirname(path.dirname(fileURLToPath(import.meta.url))));
const dist = path.join(root, 'dist');

const DEV_ROUTES = ['design'];

for (const route of DEV_ROUTES) {
  await rm(path.join(dist, route), { recursive: true, force: true });
  await rm(path.join(dist, `${route}.html`), { force: true });
  console.log(`[prune] removed /${route}/`);
}

// Clean the sitemap of dev-only URLs.
for (const file of ['sitemap-0.xml', 'sitemap-index.xml']) {
  const p = path.join(dist, file);
  try {
    let xml = await readFile(p, 'utf8');
    const before = xml;
    for (const route of DEV_ROUTES) {
      xml = xml
        .replaceAll(new RegExp(`<url>\\s*<loc>[^<]*/${route}/?</loc>.*?</url>`, 'gs'), '')
        .replaceAll(
          new RegExp(`<sitemap>\\s*<loc>[^<]*/${route}/?</loc>.*?</sitemap>`, 'gs'),
          '',
        );
    }
    if (xml !== before) {
      await writeFile(p, xml);
      console.log(`[prune] stripped dev routes from ${file}`);
    }
  } catch {
    // no sitemap — nothing to strip
  }
}
