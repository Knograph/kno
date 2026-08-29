# Bedrock and Vertex agent adapters

**Phase-1 re-reviewed 2026-08-29 — verdict: amend; amendments applied.** The review corrected
two load-bearing transport facts (Vertex serves the Anthropic format at `:rawPredict`, not
`/v1/messages`; Converse has no `seed`) and one pricing hole (the #41(d) multiplier was
missing from per-Case `Estimate` — a 10% under-reservation on every call is the exact bleed
the entry exists to prevent). SigV4/JWT footguns are now enumerated with their published
test-vector and known-answer sources. Findings P0-1..P0-3 and P1s folded and tagged.

The first partner-cloud Ring-1 agent adapters: AWS Bedrock and GCP Vertex. They share a class
(auth-heavy, region-priced, managed endpoints) but not a wire format, so they are two packages
with one plan — the shared half is the AUTH posture and the #41(d) pricing obligation, the
split half is the two transports.

Fires [`docs/debt.md#41(d)`](../debt.md#41(d)) on purpose: the entry's trigger is "when an
adapter first uses any of them" — these adapters reach partner clouds, and the PR that enables
one **prices it** (regional +10% on the models the table carries). All API facts flagged
***(verify)*** are the reviewer's to confirm before implementation.

## Problem

Teams on AWS/GCP run models through Bedrock (Claude via Converse, OpenAI-compatible inference
profiles) and Vertex (Claude via AnthropicMessages, OpenAI-compatible unified endpoints). The
`openaicompat` adapter exists but cannot reach either: both require cloud-native auth
(SigV4 / OAuth token from service-account credentials), not an API key. Today there is no
path to measure an agent behind those auth walls.

## Design

Two packages: `adapters/agent/bedrock`, `adapters/agent/vertex`. Both implement the Ring-0
`core.Agent` surface exactly as the existing adapters do (Invoke, Settle, WorstCase/Estimator
where the table prices them).

### Shared posture (per package, deliberately unshared — the repo's rule)

- **No cloud SDKs.** SigV4 and the JWT→access-token exchange are implemented with stdlib:
  SigV4 is an HMAC chain over canonicalized headers (~100 lines, unit-tested against AWS's
  published test-vector suite — docs.aws.amazon.com/general/latest/gr/signature-v4-test-suite.html,
  get-vanilla/double-url-encode/post-sts — plus Converse-specific goldens the vectors cannot
  cover *(P1)*: ARN model ids contain `:` and need `%3A` in the canonical URI; the payload
  hash is over the exact body bytes (re-marshaling breaks the signature); `x-amz-security-token`
  is a SIGNED header when session tokens are present; `x-amz-date` skew >15min is a 403, not a
  credential error — retry once with a fresh stamp, never a storm). The GCP
  exchange is a signed JWT (RS256 with the service account's private key) posted to
  `https://oauth2.googleapis.com/token` with `grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer`
  and scope `cloud-platform` *(P1, verified)*. JWT correctness is pinned by RFC 7515 A.2 (the
  published RS256 known-answer) plus claim assertions — a self round-trip catches nothing.
  Access tokens are cached until expiry; a mid-run expiry is a resumable stop (the STS rule);
  clock-skew on `iat`/`exp` surfaces as `invalid_grant` and the refusal says so. Both are
  audit-sized, deterministic, and zero-dependency — the SDK alternative pulls a dependency
  tree larger than the engine.
- **Credentials environment-only**: Bedrock reads `AWS_ACCESS_KEY_ID`/
  `AWS_SECRET_ACCESS_KEY`/`AWS_SESSION_TOKEN` (session-token presence = the chain's only STS
  path), region from `AWS_REGION` — and the refusal text says plainly what is NOT read: no
  `~/.aws/credentials`, no profiles, no SSO, no IMDS/metadata server *(P1: no AWS credential
  code exists in the repo today; "the same way the CLI does elsewhere" was false and is
  withdrawn)*. Vertex reads
  `GOOGLE_APPLICATION_CREDENTIALS` (path to the service-account JSON) or the
  `GOOGLE_CLOUD_PROJECT` + region env; the JSON is parsed for `private_key`/`client_email`
  only, never logged, `redact()`ed at every error-construction point. No ambient credential
  guessing beyond env: a keyring/metadata-server chain is the named v0.2 upgrade, and the
  refusal says so.
- **Endpoint security**: fixed regional endpoints, but the ported checks still apply where
  they apply — redirect refusal and `redact()` unconditionally; the private-address and
  plain-http refusals stay (an env-misdirected endpoint should fail loudly, not silently
  exfiltrate prompts to a loopback service).

### Bedrock package

- **Transport (verify)**: Bedrock's **Converse** API (`bedrock-runtime`, `POST
  /model/{modelId}/converse`, SigV4-signed) for Claude models — the reviewer must confirm the
  request/response envelope field names (`messages`, `system`, `inferenceConfig.maxTokens`,
  `output.message.content[].text`, `usage`) against the published Converse spec. The
  OpenAI-compatible inference-profile endpoint is the named fallback if Converse coverage
  proves too narrow for the pricing table's models — the plan commits to Converse first.
- **Model ref grammar**: `bedrock:<model-id>` (the AWS model id verbatim — a model id is a
  full resource path on Bedrock, not a vendor name).
- **Pricing, the #41(d) repayment, corrected** *(P0-2)*: the regional **+10% multiplier**
  applies in **all three** places the guard touches: per-Case `Estimate` (the reservation —
  a 10% shortfall here under-reserves every call and lets a capped run overshoot),
  `WorstCase` (the consent quote), and `Settle` (before `Guard.Settle`, so Spends records
  true spend). No other multiplier exists in the engine — no double-count — but the
  region-class lookup must key off the **model id**, not `AWS_REGION`: `us.`-prefixed
  cross-region inference profiles bill at destination-region price, so profiles without a
  priced row are **refused until a row exists**, exactly like unpriced regions. The us/eu
  row claims are (verify)-flagged like the transports — asserted, not yet sourced. Unpriced
  models take the `--price-input-per-mtok` path like every adapter.
- **Capabilities**: `Capabilities()` reports what Converse supports for the model family
  — `temperature`, `system`, `maxTokens`, `topP`, `stopSequences`. **No `seed`**: it does not
  exist in `inferenceConfig` *(P0-3)*; a false Capability would make the engine send a
  parameter the endpoint rejects per Case. If a family gains seed via
  `additionalModelRequestFields`, it returns then, verified per model — never assumed.

### Vertex package

- **Transport, corrected** *(P0-1)*: Claude on Vertex is
  `POST https://{region}-aiplatform.googleapis.com/v1/projects/{project}/locations/{region}/publishers/anthropic/models/{model}:rawPredict`
  (`:streamRawPredict` for streaming), OAuth bearer — **not** `/v1/messages`. Two body
  divergences from the anthropic adapter's mapping, named not absorbed: `anthropic_version`
  travels in the **body**, not the `anthropic-version` header, and the model id is in the URL
  (both `claude-3-5-sonnet@20240620` and plain forms exist — the grammar accepts both, the
  URL-encoding golden pins the escaping). Model Garden opt-in matters: unaccepted terms
  return NOT_FOUND on `publishers/anthropic/models/*` — the refusal text must say so, or users
  chase a phantom endpoint bug. The OpenAI-compatible unified endpoint is the fallback, same
  rule as Bedrock.
- **Model ref grammar**: `vertex:<model-id>`.
- **Pricing**: same #41(d) regional multiplier discipline as Bedrock; settlement identical.

### CLI

Agent-ref parsing extended for both grammars; `--key-env` is NOT used (credentials are the
cloud chains, not a single key). Help snapshots updated. Both adapters refuse
`--allow-private-address`-style bypass of their endpoint checks — the opt-out flags do not
apply to fixed cloud endpoints.

## Alternatives considered

**AWS SDK v2 + GCP client libraries.** Rejected: a dependency tree larger than the engine for
two calls (token exchange) and one signature. The SigV4/JWT code is small, pinned by published
test vectors, and the security surface is *smaller* when it is readable.

**Routing both through the OpenAI-compatible endpoints only.** Rejected as the primary path:
Converse and AnthropicMessages carry the provider-native fields the pricing table models
(cache reads/writes, usage structure) that the compat layer flattens — settlement accuracy is
prime directive 4 territory.

**One shared `partner-cloud` transport.** Rejected: SigV4 and OAuth share nothing but the word
"auth"; a shared abstraction would be the wrong abstraction (the repo's rule).

## Affected packages

`adapters/agent/bedrock`, `adapters/agent/vertex` (new), `adapters/agent/pricing` (#41(d)
regional constants), `cli/` (agent-ref grammar), `docs/` (cookbook entries, matrix, pricing
page), CHANGELOG. `docs/debt.md#41(d)` disposition (repaid or partially with the remainder
re-dated — the reviewer decides from the verified pricing facts).

## Proto / schema impact

None.

## Edge cases

| Case | Behavior |
|---|---|
| Missing/invalid cloud credentials | Actionable refusal naming the exact env var; no partial auth |
| Region without a pricing row | Refusal naming the region, until a row exists |
| Session-token present (STS) | Used; expiry honored; a mid-run expiry is a resumable stop, never a retry storm |
| Token-endpoint refusal (Vertex) | Actionable refusal; the JWT is never retried against a different host |
| Converse/AnthropicMessages feature mismatch | Capabilities() reports the narrow set; the engine refuses unsupported params — never silently drops |
| Model Garden terms not accepted (Vertex) | NOT_FOUND on publishers/anthropic/models/* — the refusal names the opt-in, not the endpoint |
| Redirects from the fixed endpoints | Refused (ported check) |
| Unpriced model | The scalar pricing flags path, `--accept-unknown-cost` required |
| The 10% regional multiplier at settlement | Applied and reported per call — never rounded into invisibility |

## Test plan

Recorded fixtures for both transports (hand-authored per the verified specs + note.txt; live
re-record via `make record-fixtures` with `KNO_LIVE_TESTS=1` and a capped budget — the first
live-credential path in the repo, so the record target must refuse to run without the guard).
SigV4 test vectors (canonical requests from AWS's published examples); JWT sign/verify
round-trip against a pinned RSA key; credential-chain refusals per case above; pricing
multiplier unit tests + settlement golden; Capabilities matrices; httptest transport
behavior; the secrets scan (no key material in fixtures); CLI grammar; `goleak.VerifyTestMain`.
A deliberate NO: no test calls a real cloud endpoint in CI.

## Rollback

Delete both packages and the grammar entries. Pricing constants are additive.

## Docs impact

Cookbook entries ("Measure a Bedrock agent", "Measure a Vertex agent"), adapters matrix,
pricing page (the regional multiplier, plainly), CHANGELOG under Unreleased.

## Accepted risks

- **No SDK**: the team owns SigV4/JWT maintenance; both are pinned by published vectors and
  small enough to audit in one sitting.
- **Converse-first for Bedrock**: if a table model is unreachable via Converse, the compat
  fallback is the documented next step, not a silent downgrade.
- **The 10% multiplier is a documented constant per region class**, not a live price lookup —
  per the review's pick, the pricing drift detector's scope IS extended to the regional
  constants, and the ledger records that extension as part of the #41(d) repayment.
