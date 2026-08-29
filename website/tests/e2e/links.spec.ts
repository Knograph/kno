import { expect, test } from '@playwright/test';

/**
 * Automated broken-link check: crawl the site's own pages and verify every
 * internal link resolves. External links (GitHub etc.) are not fetched.
 */

const IGNORED = new Set(['#', '/admin/']);

test('no broken internal links', async ({ page, request }) => {
  const visited = new Set<string>();
  const broken: string[] = [];
  const queue = ['/'];

  while (queue.length > 0) {
    const path = queue.shift()!;
    if (visited.has(path)) continue;
    visited.add(path);

    const res = await page.goto(path);
    if (!res || res.status() >= 400) {
      broken.push(`${path} (page status ${res?.status() ?? 'unknown'})`);
      continue;
    }

    const hrefs = await page.$$eval('a[href]', (links) =>
      links.map((a) => a.getAttribute('href') ?? ''),
    );
    for (const href of hrefs) {
      if (href.startsWith('#')) continue; // in-page anchor
      if (href.startsWith('mailto:') || href.startsWith('http')) continue; // external
      const clean = href.split('#')[0];
      if (!clean || IGNORED.has(clean) || visited.has(clean)) continue;
      queue.push(clean);
    }
  }

  // Verify collected pages, including assets linked from the homepage.
  for (const path of visited) {
    const res = await request.get(path);
    if (res.status() >= 400) {
      broken.push(`${path} (status ${res.status()})`);
    }
  }

  expect(broken, `Broken internal links found:\n${broken.join('\n')}`).toEqual([]);
});
