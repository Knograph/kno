# CMS OAuth Worker

The only Worker in the Kno website architecture: GitHub OAuth for
`/admin`, running the maintained
[Sveltia CMS Authenticator](https://github.com/sveltia/sveltia-cms-auth).

Do not implement an ad hoc OAuth protocol here — deploy the maintained
project. Full step-by-step (GitHub OAuth app, callback URL, variables,
secrets, verification) lives in
[../docs/DEPLOYMENT.md](../docs/DEPLOYMENT.md).

Secrets (`GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET`) are configured with
`wrangler secret put` and never committed. `wrangler.jsonc` in this
directory is a reference configuration only.
