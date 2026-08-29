import { defineMiddleware } from 'astro:middleware';

/**
 * Dev-only convenience: Astro's dev server does not resolve directory
 * indexes under public/, so /admin/ 404s in `pnpm dev`. Redirect to the
 * literal file. Production is unaffected: static output has no server
 * runtime, and Cloudflare Pages resolves /admin/ → index.html natively.
 */
export const onRequest = defineMiddleware((ctx, next) => {
  const { pathname } = ctx.url;
  if (pathname === '/admin' || pathname === '/admin/') {
    return ctx.redirect('/admin/index.html', 302);
  }
  return next();
});
