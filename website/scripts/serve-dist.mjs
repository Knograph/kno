/**
 * Tiny foreground static server for dist/ — used by Playwright e2e tests.
 * Astro 7's `astro preview` daemonizes itself and exits, which Playwright's
 * webServer treats as a crash. This server stays in the foreground.
 */

import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.join(
  path.dirname(path.dirname(fileURLToPath(import.meta.url))),
  'dist',
);
const port = Number(process.env.PORT ?? 4321);

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.gif': 'image/gif',
  '.jpg': 'image/jpeg',
  '.ico': 'image/x-icon',
  '.xml': 'application/xml',
  '.txt': 'text/plain; charset=utf-8',
  '.webmanifest': 'application/manifest+json',
  '.json': 'application/json',
};

const server = createServer(async (req, res) => {
  try {
    const url = new URL(req.url ?? '/', `http://localhost:${port}`);
    let file = path.normalize(path.join(root, url.pathname));
    if (!file.startsWith(root)) {
      res.writeHead(403).end();
      return;
    }
    if (file.endsWith('/')) file = path.join(file, 'index.html');
    let body;
    try {
      body = await readFile(file);
    } catch {
      body = await readFile(path.join(root, '404.html'));
      res.writeHead(404, { 'Content-Type': MIME['.html'] });
      res.end(body);
      return;
    }
    const ext = path.extname(file);
    res.writeHead(200, { 'Content-Type': MIME[ext] ?? 'application/octet-stream' });
    res.end(body);
  } catch {
    res.writeHead(500).end();
  }
});

server.listen(port, () => {
  console.log(`[serve-dist] http://localhost:${port} (dist/)`);
});
