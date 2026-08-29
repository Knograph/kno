// @ts-check
import { defineConfig } from 'astro/config';
import sitemap from '@astrojs/sitemap';

// `site` must be set for canonical URLs, sitemap, and RSS to work.
// Cloudflare Pages provides PUBLIC_SITE_URL in production. Locally it
// falls back to the dev server origin.
export default defineConfig({
  site: process.env.PUBLIC_SITE_URL ?? 'http://localhost:4321',
  integrations: [sitemap()],
  build: {
    format: 'directory',
  },
});
