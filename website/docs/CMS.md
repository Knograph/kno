# CMS — Sveltia CMS operation

The website content is Git-backed and edited through
[Sveltia CMS](https://sveltiacms.app/) at `/admin`. Git is the source of
truth; the CMS is only a UI. Every change is a normal Git commit or PR,
history stays visible in GitHub, and the website renders without any CMS
API — content is read at build time by Astro.

## What is vendored

- `public/admin/index.html` — CMS shell.
- `public/admin/sveltia-cms.js` — pinned `@sveltia/cms@0.201.2` bundle,
  self-hosted. Upgrade by installing the new package version and copying
  `dist/sveltia-cms.js` over the vendored file.
- `public/admin/config.yml` — **generated** at build/dev time by
  `scripts/generate-cms-config.mjs` from
  `scripts/cms-config.template.yml`. Do not hand-edit the generated file.
  `public/admin/config.yml.example` is a committed reference.
- `scripts/cms-config.template.yml` — the collection/field definitions
  (mirrors `src/content.config.ts`).

## Configuration that varies by environment

`backend.base_url` differs: local token use points at the GitHub API, the
deployed site points at the Sveltia CMS Authenticator Worker.

```yaml
backend:
  name: github
  repo: knograph/kno
  branch: main
  base_url: https://api.github.com # ← replaced at build time
  auth_scope: public_repo
  squash_merges: true
  open_authoring: true
publish_mode: editorial_workflow
```

The generator substitutes:

| Placeholder             | Environment variable | Fallback                 |
| ----------------------- | -------------------- | ------------------------ |
| `{{CMS_AUTH_BASE_URL}}` | `CMS_AUTH_BASE_URL`  | `https://api.github.com` |
| `{{PUBLIC_SITE_URL}}`   | `PUBLIC_SITE_URL`    | `http://localhost:4321`  |

The generated file is public by design — no secrets ever enter it.

## Local usage

```bash
cd website
pnpm install
pnpm dev            # regenerates config.yml, serves http://localhost:4321
```

Open `http://localhost:4321/admin/`. Two paths:

1. **Work with Local Repository** — point Sveltia at the repository root
   when prompted; edits are written directly to your working tree.
   No auth, no network.
2. **Sign In Using Access Token** — a GitHub personal access token with
   `public_repo` scope; Sveltia talks to `api.github.com` and commits on
   your behalf. Use this to exercise the real backend (including the
   editorial workflow) locally.

Neither path requires Cloudflare credentials or the OAuth Worker.

## Remote usage (production /admin)

The deployed `/admin` uses GitHub OAuth via the Sveltia CMS Authenticator
(see [DEPLOYMENT.md](DEPLOYMENT.md)). Editorial workflow + open authoring
are active:

- **Editorial workflow** — draft changes are committed to a working
  branch and opened as a PR; publishing merges the PR. Content review
  happens in GitHub like code review.
- **Open authoring** — outside contributors who lack write access can
  still propose changes: Sveltia forks the repository, commits to the
  fork, and opens a PR against `main`. The authenticator performs the
  fork + PR using the user's GitHub identity, so contributors need no
  repository permissions.

Merge result: PR → `main` → Cloudflare Pages build → deploy. The CMS
never publishes directly; every change passes through Git.

## Content model

Collections defined in `scripts/cms-config.template.yml` (CMS side) and
typed in `src/content.config.ts` (build side). Keep them in sync — the
CMS fields are the editable surface; the Astro schema is the contract.

| Collection    | Kind           | Files                                |
| ------------- | -------------- | ------------------------------------ |
| Site settings | file singleton | `website/src/content/site/site.yaml` |
| Homepage      | file singleton | `website/src/content/home/home.yaml` |
| Blog          | folder         | `website/src/content/blog/*.md`      |
| Use cases     | folder         | `website/src/content/use-cases/*.md` |
| Pages         | folder         | `website/src/content/pages/*.md`     |

Media uploads go to `website/public/images/uploads/` and are served from
`/images/uploads/`. Committed like any other asset.

## Adding or changing a field

1. Update `src/content.config.ts` (schema).
2. Update `scripts/cms-config.template.yml` (editor field).
3. If the new field needs a default in the homepage/settings singleton,
   add it to the content YAML.
4. `pnpm check && pnpm build` — the build fails loudly on schema
   mismatches, which is the point.

## Troubleshooting

- **"Failed to load entry" / config errors in the CMS** — the generated
  `config.yml` is out of sync with the template. Run `pnpm cms:generate`.
- **Sign in with GitHub missing in production** — `CMS_AUTH_BASE_URL` not
  set at build time. See [DEPLOYMENT.md](DEPLOYMENT.md).
- **Fork PRs fail** — the authenticator requires the user's GitHub OAuth
  grant (`public_repo` scope is requested by the Worker) and
  `ALLOWED_DOMAINS` must include the site hostname.
- **Editing the homepage shows old values** — content is static: merge the
  PR and wait for the Pages build; nothing is read at request time.
