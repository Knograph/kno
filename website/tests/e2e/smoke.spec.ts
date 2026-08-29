import { expect, test } from '@playwright/test';

test('homepage loads with headline and primary CTA', async ({ page }) => {
  await page.goto('/');
  await expect(page).toHaveTitle(/Kno/);
  await expect(page.getByRole('heading', { level: 1 })).toContainText(
    'Know which data actually makes your AI better',
  );
  const runKno = page.getByRole('link', { name: 'Run Kno' });
  await expect(runKno).toBeVisible();
  await expect(runKno).toHaveAttribute('href', '#quickstart');
  await expect(
    page.getByRole('link', { name: 'View on GitHub' }).first(),
  ).toHaveAttribute('href', 'https://github.com/knograph/kno');
});

test('navigation works', async ({ page }) => {
  await page.goto('/');
  await page.locator('header').getByRole('link', { name: 'Use Cases' }).click();
  await expect(page).toHaveURL(/\/use-cases\/$/);
  await expect(page.getByRole('heading', { level: 1 })).toContainText(
    'Data questions, answered with measurement',
  );
});

test('hero install command copies', async ({ page, context }) => {
  await context.grantPermissions(['clipboard-read', 'clipboard-write']);
  await page.goto('/');
  const copyButton = page.locator('.hero .copycmd__btn');
  await copyButton.click();
  await expect(copyButton).toHaveAttribute('aria-pressed', 'true');
  const clipboard = await page.evaluate(() => navigator.clipboard.readText());
  expect(clipboard).toContain('curl -sSfL');
  expect(clipboard).toContain('install.sh');
});

test('use-case pages render', async ({ page }) => {
  await page.goto('/use-cases/rag-document-selection/');
  await expect(page.getByRole('heading', { level: 1 })).toContainText(
    'Which documents should go into my RAG system?',
  );
  await expect(page.getByRole('link', { name: /Try it/ })).toBeVisible();
});

test('blog pages render', async ({ page }) => {
  await page.goto('/blog/');
  await expect(page.getByRole('heading', { level: 1 })).toContainText(
    'Notes from the lab',
  );
  await page.goto('/blog/2026-08-29-kno-v01/');
  await expect(page.getByRole('heading', { level: 1 })).toContainText(
    'the measurement loop is complete',
  );
});

test('rss feed works', async ({ request }) => {
  const res = await request.get('/rss.xml');
  expect(res.status()).toBe(200);
  const body = await res.text();
  expect(body).toContain('<rss');
  expect(body).toContain('<item>');
});

test('robots and sitemap exist', async ({ request }) => {
  const robots = await request.get('/robots.txt');
  expect(robots.status()).toBe(200);
  expect(await robots.text()).toContain('Sitemap:');

  const sitemap = await request.get('/sitemap-index.xml');
  expect(sitemap.status()).toBe(200);
});

test('admin loads the CMS', async ({ page }) => {
  const errors: string[] = [];
  page.on('console', (msg) => {
    if (msg.type() === 'error') errors.push(msg.text());
  });
  page.on('pageerror', (err) => errors.push(String(err)));
  await page.goto('/admin/');
  // Sveltia replaces the title and renders its own shell + login options.
  await expect(page).toHaveTitle('Sveltia CMS');
  try {
    await expect(page.locator('body')).toContainText(/Sign In with/, {
      timeout: 15_000,
    });
    await expect(page.locator('body')).toContainText(/Work with Local Repository/);
  } catch (err) {
    const body = await page.locator('body').innerText();
    throw new Error(
      `Admin page failed to render. Console errors: ${errors.join(' | ') || 'none'}\nBody: ${body.slice(0, 400)}`,
    );
  }
});
