import type { APIRoute } from 'astro';

export const GET: APIRoute = ({ site }) => {
  const sitemap = new URL('sitemap-index.xml', site ?? 'http://localhost:4321');
  return new Response(`User-agent: *\nAllow: /\n\nSitemap: ${sitemap.toString()}\n`, {
    headers: { 'Content-Type': 'text/plain; charset=utf-8' },
  });
};
