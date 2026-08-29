# Kno website

Marketing website for [Kno](https://github.com/knograph/kno) — built with
Astro, served from Cloudflare Pages, edited through Sveltia CMS.

**Requires Node 22.12+ and pnpm.**

```bash
cd website
pnpm install
pnpm dev          # http://localhost:4321
```

## Architecture

```text
website/
├── src/
│   ├── components/        Astro components (Nav, Footer, Terminal, …)
│   ├── layouts/           BaseLayout, ArticleLayout
│   ├── pages/             Routes (index, blog, use-cases, pages, rss, …)
│   ├── content/           Content collections (the CMS's source of truth)
│   ├── content.config.ts  Strongly typed schemas — the CMS/renderer contract
│   ├── lib/               Pure helpers (unit-tested)
│   └── styles/            Design tokens + global styles (see docs/BRAND.md)
├── public/
│   ├── admin/             Sveltia CMS (vendored, pinned) + generated config.yml
│   └── images/            Brand assets, quickstart.gif, og.png
├── scripts/
│   ├── generate-cms-config.mjs   Build-time CMS config materialization
│   ├── generate-og.mjs           OpenGraph image generation
│   ├── prune-dev-routes.mjs      Removes dev-only routes from prod builds
│   └── serve-dist.mjs            Foreground static server for e2e tests
├── tests/e2e/             Playwright smoke tests + broken-link crawl
├── worker/                Sveltia CMS Authenticator deployment reference
└── docs/                  BRAND.md, CMS.md, DEPLOYMENT.md, ANALYTICS.md
```

Static-first: every page is generated at build time. The only Worker in
the architecture is the CMS OAuth authenticator; no Worker serves site
traffic. No client framework — progressive enhancement only.

## Content model

Content lives in `src/content/` as Git-tracked YAML/Markdown, typed by
`src/content.config.ts`:

| Collection  | File             | Renders at                             |
| ----------- | ---------------- | -------------------------------------- |
| `site`      | `site/site.yaml` | Every page (nav, footer, SEO defaults) |
| `home`      | `home/home.yaml` | `/`                                    |
| `blog`      | `blog/*.md`      | `/blog/`, `/blog/[slug]/`, `/rss.xml`  |
| `use-cases` | `use-cases/*.md` | `/use-cases/`, `/use-cases/[slug]/`    |
| `pages`     | `pages/*.md`     | `/[slug]/` (about, community, …)       |

Schemas are loose by design — a partially-filled CMS draft never breaks
the build. Editors control content; developers control layout.

## CMS (Sveltia)

Sveltia CMS is served from `/admin` as vendored static assets (pinned
`@sveltia/cms@0.201.2` — no CDN, no mutable "latest"). Its `config.yml`
is generated at build/dev time from
`scripts/cms-config.template.yml` so the OAuth URL can differ per
environment without secrets in the repo.

```bash
pnpm dev              # regenerates config.yml, starts Astro
pnpm cms:generate     # regenerate config.yml alone
```

Two ways to use the CMS locally:

- **Work with Local Repository** — edits your working copy directly; no
  authentication needed.
- **GitHub (access token)** — sign in with a personal access token
  (`public_repo` scope) and edit through the GitHub backend.

In production, `/admin` authenticates through the **Sveltia CMS
Authenticator** Worker (GitHub OAuth). Outside contributors can propose
edits via fork + PR (open authoring); all changes become normal Git
commits/PRs with an editorial workflow. Details: [docs/CMS.md](docs/CMS.md).

## Environment variables

Copy `.env.example` to `.env` as needed. Everything is optional locally.

| Variable                    | Purpose                                                     | Default                                 |
| --------------------------- | ----------------------------------------------------------- | --------------------------------------- |
| `PUBLIC_SITE_URL`           | Canonical origin (canonical URLs, sitemap, RSS, CMS config) | `http://localhost:4321`                 |
| `CMS_AUTH_BASE_URL`         | Sveltia CMS Authenticator Worker URL for `/admin`           | `https://api.github.com` (token method) |
| `PUBLIC_ANALYTICS_PROVIDER` | `cloudflare` to enable custom-event analytics               | unset (no tracking)                     |

Secrets (`GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET`, `ALLOWED_DOMAINS`)
live only in the Worker's environment — never in the repo or `.env`.

## Editing content

- **Homepage**: edit `src/content/home/home.yaml` (by hand or via `/admin`).
- **Adding a use case**: create `src/content/use-cases/my-case.md` with the
  frontmatter fields from the `use-cases` collection schema; it appears on
  `/use-cases/` and `/use-cases/my-case/` automatically.
- **Publishing a blog post**: create `src/content/blog/YYYY-MM-DD-slug.md`
  with `draft: false`; it appears on `/blog/` and in `/rss.xml`.
- **Adding a page**: create `src/content/pages/name.md`; it renders at
  `/name/`.

## Testing

```bash
pnpm check         # astro check (typecheck)
pnpm lint          # eslint
pnpm format        # prettier --write
pnpm format:check  # prettier --check
pnpm test          # vitest unit tests
pnpm build && pnpm test:e2e   # Playwright: smoke + broken-link crawl
```

`test:e2e` serves `dist/` with a foreground static server, so build first.

## Deployment

Cloudflare Pages, GitHub-connected: production = `main`, previews = PRs.
Full steps, including the CMS OAuth Worker and its GitHub OAuth app:
[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md).

## Design system

See [docs/BRAND.md](docs/BRAND.md). A live primitives gallery exists at
`/design/` in dev only (`pnpm dev`), pruned from production builds.
