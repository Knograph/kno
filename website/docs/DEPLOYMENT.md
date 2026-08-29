# Deployment — Cloudflare Pages + CMS OAuth Worker

Two moving parts:

1. **Cloudflare Pages** serves the static site (GitHub-connected builds).
   No Worker serves normal traffic.
2. **One Worker** — the [Sveltia CMS
   Authenticator](https://github.com/sveltia/sveltia-cms-auth) — handles
   GitHub OAuth for `/admin` only.

```text
GitHub (uknoAI/kno, main)
   │  push / PR merge
   ▼
Cloudflare Pages ──► kno website (static, /website root)
   │
   └── /admin ──► Sveltia CMS ──► Authenticator Worker ──► GitHub
                                   (OAuth; fork + PR for contributors)
```

## 1. Cloudflare Pages

Create a Pages project connected to the GitHub repository.

| Setting                | Value                |
| ---------------------- | -------------------- |
| Build command          | `pnpm build`         |
| Build output directory | `website/dist`       |
| Root directory         | `website`            |
| Node version           | 24 (22.12+ required) |

Cloudflare Pages detects pnpm from `pnpm-lock.yaml`. Production branch:
`main`. Pull requests get preview deployments automatically.

Environment variables (build-time, **not** secrets):

| Variable            | Value                                           |
| ------------------- | ----------------------------------------------- |
| `PUBLIC_SITE_URL`   | `https://<your-domain>` (canonical origin)      |
| `CMS_AUTH_BASE_URL` | `https://<worker-name>.<subdomain>.workers.dev` |

Optional analytics:

| Variable                            | Value                                                      |
| ----------------------------------- | ---------------------------------------------------------- |
| `PUBLIC_ANALYTICS_PROVIDER`         | `cloudflare`                                               |
| `PUBLIC_CLOUDFLARE_ANALYTICS_TOKEN` | Web Analytics site token (not used by the abstraction yet) |

Security headers ship in `public/_headers` (CSP, X-Content-Type-Options,
Referrer-Policy, Permissions-Policy, frame-ancestors). They were verified
against the CMS: Sveltia is fully self-hosted and makes no cross-origin
requests, so `connect-src 'self'` does not break it.

Attach a custom domain from the Pages project (DNS is created
automatically on Cloudflare).

## 2. Sveltia CMS Authenticator (the only Worker)

Use the maintained implementation — do not build your own OAuth.

### 2a. Deploy the Worker

Clone [sveltia/sveltia-cms-auth](https://github.com/sveltia/sveltia-cms-auth)
(or click its "Deploy to Cloudflare Workers" button) and deploy with
Wrangler. A reference `wrangler.jsonc` lives in `website/worker/`.

```bash
git clone https://github.com/sveltia/sveltia-cms-auth
cd sveltia-cms-auth
npx wrangler deploy        # note the worker URL, e.g.
                           # https://sveltia-cms-auth.uknoAI.workers.dev
```

### 2b. Register a GitHub OAuth app

GitHub → Settings → Developer settings → OAuth Apps → New OAuth App:

| Field                      | Value                             |
| -------------------------- | --------------------------------- |
| Application name           | `Kno CMS`                         |
| Homepage URL               | `https://github.com/uknoAI/kno` |
| Authorization callback URL | `<WORKER_URL>/callback`           |

Generate a client secret after registering.

### 2c. Configure the Worker secrets/variables

Dashboard → Workers → `sveltia-cms-auth` → Settings → Variables:

| Name                   | Value                                                                                                 | Encrypt |
| ---------------------- | ----------------------------------------------------------------------------------------------------- | ------- |
| `GITHUB_CLIENT_ID`     | Client ID from 2b                                                                                     | no      |
| `GITHUB_CLIENT_SECRET` | Client Secret from 2b                                                                                 | **yes** |
| `ALLOWED_DOMAINS`      | your site hostname, e.g. `kno.example.com` (comma-separated list; wildcard `*.example.com` supported) | no      |

`ALLOWED_DOMAINS` is optional in the authenticator but strongly
recommended: it stops other sites using your Worker (abuse) and from
obtaining tokens through it (security).

Secrets exist only in the Worker. Nothing from this step may enter the
repository, `.env`, or the generated CMS config.

### 2d. Point the CMS at the Worker

Set `CMS_AUTH_BASE_URL` in the Pages environment (step 1). At build time
`scripts/generate-cms-config.mjs` writes it into
`public/admin/config.yml` as `backend.base_url`. Redeploy.

## 3. Verify

1. Open `https://<domain>/admin/` — "Sign In with GitHub" appears.
2. Sign in — you land in the CMS with the Kno collections listed.
3. Edit a use case, save as draft → a PR appears on
   `uknoAI/kno` (editorial workflow).
4. From a second GitHub account without repo access, open `/admin`, sign
   in, propose an edit → a fork-based PR appears (open authoring).
5. Merge → Pages build → content live.

## 4. Local preview of production behavior

```bash
cd website
cp .env.example .env   # set PUBLIC_SITE_URL and CMS_AUTH_BASE_URL
pnpm build
pnpm preview
```

The build reads the same environment variables as Cloudflare Pages, so
`dist/admin/config.yml` matches production exactly.
