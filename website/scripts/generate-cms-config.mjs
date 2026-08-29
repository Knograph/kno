/**
 * Generates public/admin/config.yml from scripts/cms-config.template.yml.
 *
 * The CMS OAuth URL differs across environments (local token use vs. the
 * deployed Sveltia CMS Authenticator Worker), so the config is materialized
 * at build time:
 *
 *   CMS_AUTH_BASE_URL   Worker URL, e.g. https://sveltia-cms-auth.kno.workers.dev
 *                       (unset → https://api.github.com, the token method)
 *   PUBLIC_SITE_URL     Canonical origin, e.g. https://kno.example.com
 *                       (unset → http://localhost:4321)
 *
 * The output is public by design. Secrets (GITHUB_CLIENT_SECRET etc.) never
 * appear here — they live only in the Worker's environment.
 */

import { readFile, writeFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)));

const template = await readFile(
  path.join(root, 'scripts', 'cms-config.template.yml'),
  'utf8',
);

const cmsAuthBaseUrl = process.env.CMS_AUTH_BASE_URL || 'https://api.github.com';
const siteUrl = process.env.PUBLIC_SITE_URL || 'http://localhost:4321';

const generated = template
  .replaceAll('{{CMS_AUTH_BASE_URL}}', cmsAuthBaseUrl)
  .replaceAll('{{PUBLIC_SITE_URL}}', siteUrl);

await writeFile(path.join(root, 'public', 'admin', 'config.yml'), generated);
console.log(
  `[cms-config] wrote public/admin/config.yml (auth base: ${cmsAuthBaseUrl}, site: ${siteUrl})`,
);
