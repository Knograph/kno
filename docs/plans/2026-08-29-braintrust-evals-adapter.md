# Braintrust Evals adapter

The fourth `core.Evals` implementation (jsonl, langsmith, langfuse, this). The two SaaS-Evals
precedents settled the shared contract: same iterator shape, same endpoint-security posture,
shared split package. This plan inherits all of it and changes only what Braintrust's API
actually is.

**Phase-1 re-reviewed 2026-08-29 — verdict: amend; amendments applied.** The review verified
every API claim against Braintrust's published reference and corrected three P0s: the rows
endpoint is `fetch` with an opaque `cursor` (not `rows` + `ending_before`); cross-page
duplicates are VENDOR-EXPECTED version history, so the duplicate rule is dedupe-by-id
keep-newest-`_xact_id`, not fatal (and the torn-page detector becomes the merge rule instead);
the dataset object carries no `updatedAt`/`revision`, so the fingerprint is the first event's
`_xact_id` — the real monotonic edit counter. Findings P0-1..P0-3 and P1s folded and tagged.

## Problem

Teams running evals on Braintrust keep their cases as **datasets** — rows of `input` /
`expected` / `metadata`, frequently produced by Braintrust's own experiment machinery from
traces. Today Kno measures them only via a manual export. Same question as its two siblings:
map a Braintrust dataset onto `core.Evals` — iterator contract, Provenance, dev/holdout split,
resume fingerprint — importing nothing above `adapters/`.

## Design

`adapters/evals/braintrust` — the langfuse shape, corrected where the vendor differs:

- **Transport, corrected** *(P0-1, P1-5)*:
  - Name resolution: `GET /v1/dataset?dataset_name=<name>` — a FILTER endpoint; a miss
    returns an empty list, so the refusal checks the result and names the host (dataset
    names are unique per project — the refusal also names `BRAINTRUST_ORG_NAME` as the
    disambiguator when one is set).
  - Rows: `GET/POST /v1/dataset/{dataset_id}/fetch` — path param is the **uuid** from
    resolution; response `{events: [...], cursor}`; pagination passes the opaque `cursor`
    back from the body, never a client-computed parameter. `limit` counts TRACES, not rows —
    the response can exceed it, so the per-page cap is a byte bound on the events array,
    not `limit × row-cap` *(P1-6)*.
  - Stdlib `net/http`, Bearer `BRAINTRUST_API_KEY`, environment-only. `BRAINTRUST_ORG_NAME`
    is an OPTIONAL query param (it matters only for keys spanning orgs), not a required
    header *(P1-4)*.
- **Endpoint security, ported verbatim**: scheme/userinfo/private/link-local refusal (no
  opt-out for link-local), dial-time recheck, redirect refusal, `redact()` everywhere; the
  two CLI opt-outs wire through `cli/evals.go` exactly as langfuse wired them.
- **Duplicates, the vendor's rule, not langfuse's** *(P0-2)*: Braintrust pagination walks the
  full version history — later pages "may return rows which showed up in earlier pages,
  except with an earlier `_xact_id`", and the vendor directs clients to exclude duplicate,
  outdated rows **by id**. So: dedupe by id, keep the newest `_xact_id` — cross-page
  duplicates are expected, not a source defect, and a fatal-on-duplicate would make every
  edited multi-page dataset unrunnable. `coretest.EvalsDuplicateIDs` still holds because the
  dedup happens at the adapter before any id reaches core. The torn-page detector BECOMES
  this merge rule: an edit mid-pagination surfaces as exactly these duplicates, and merging
  newest-wins is the correct response — the fixture pins it.
- **Fingerprint, the real monotonic counter** *(P0-3)*: the dataset object has no
  `updatedAt` and no `revision`. `_xact_id` is the version counter — every insert, update,
  and delete bumps it. `ContentHash = host + dataset id + dataset name + first event's
  `_xact_id` via `fetch?limit=1` — one request, same cost model as langfuse's, real edit
  sensitivity. An empty dataset has no `_xact_id` — `split.Validate` refuses empty eval sets
  anyway; the refusal text is the honest path.
- **Streaming**: pages fetched lazily inside the iterator; ctx check per page; byte-capped
  pages per above; the cursor chain is followed until exhausted or capped (a cap constant,
  langsmith-style).
- **Case mapping**: id = event id (primary key — the #45 premise holds, dedupe keeps the
  invariant); prompt = canonical JSON of `input`, key-sorted and golden-pinned; **null input
  fatal** (langsmith parity); expected = canonical JSON of `expected`, empty when null (judge
  goals score without it); no tags.
- **Provenance, pinned to the real field** *(P1-7)*: events carry
  `origin: {object_type, object_id, _xact_id}` — "the event was copied from another object".
  Derived **iff `origin` is present** — the langfuse rule with Braintrust's signal. The
  `object_type` values (experiment/span vs eval-result links) are enumerated at
  fixture-record time; a mis-marked link row changes weak-label counts, so the enumeration is
  recorded in the fixture note, not guessed in code.
- **Split**: shared package; id-keyed; edits don't re-split, the fingerprint refuses a stale
  resume.
- **Retries**: 429 only, `Retry-After` honored, bounded, ctx-aware.
- **Errors**: plain wrapped errors; CLI mapping via `braintrustNewFix` in `cli/evals.go`.
- **No spend**: read-only.
- **CLI**: `--evals braintrust:<dataset-name>`, help snapshots updated.

## Alternatives considered

**Braintrust's Go SDK.** Rejected: dependency weight for two read-only endpoints; the stdlib
pattern is proven twice in-repo, and the SDK's write-side machinery (logging, experiments) is
surface Kno never uses.

**CSV export from the Braintrust UI.** Rejected: manual, rots, drops the origin lineage the
provenance half exists to record.

**A shared three-vendor SaaS-Evals abstraction.** Rejected for the same reason as last time:
auth, pagination, and row shapes differ — Braintrust's cursor+version-history model proves
the point again.

## Affected packages

`adapters/evals/braintrust` (new), `cli/` (prefix, fix mapping, opt-outs, snapshots), `docs/`
(cookbook entry, matrix row, what-the-numbers-mean line), CHANGELOG. Nothing else.

## Proto / schema impact

None.

## Edge cases

| Case | Behavior |
|---|---|
| Dataset name unknown | The list-filter endpoint returns empty — refusal naming the host and org |
| Bad credentials | Actionable refusal naming the env var |
| 429 storms | Retry-After honored, bounded; no other status retried |
| Same id twice across pages (edit mid-pagination) | Dedupe, newest `_xact_id` wins — vendor-directed, fixture-pinned |
| `limit` exceeded by trace-counting | Byte cap on the events array, not a row-count assumption |
| `input` null | Fatal (langsmith parity) |
| `expected` null | Empty expected, provenance note |
| Event with `origin` | Provenance derived; counted weak (mechanism exists) |
| Empty dataset | Refused by split validation, honestly |
| Plain-http / private / userinfo / redirecting host | Refused by the ported checks + opt-outs |
| Holdout | Shared split; the seal applies |

## Test plan

Hand-authored fixtures (note.txt provenance; re-record via `make record-fixtures` when keys
exist): list-filter resolution (hit + miss), fetch 2-page cursor chain, same-id-edited
mid-pagination (dedupe keeps newest), origin-carrying + authored events, null fields.
Fingerprint sensitivity test (one `_xact_id` bump changes the hash); byte-cap test;
`coretest.CheckEvalsDuplicateIDs`; cross-adapter split identity (all four Evals sources
assign identical ids to identical splits); canonical JSON goldens; endpoint-security trio;
derived round-trip + weak-label count through a real baseline over the fixture; httptest
auth/429/Retry-After; CLI grammar; `goleak.VerifyTestMain`.

## Rollback

Delete the package and the prefix parse.

## Docs impact

Cookbook entry ("Measure a Braintrust dataset"), adapters matrix row, what-the-numbers-mean
provenance line, CHANGELOG under Unreleased.

## Accepted risks

- **`origin` presence is the derived signal** — the vendor's own marker; a mis-marked link row
  degrades to today's behavior, and the object_type enumeration lives in the fixture note.
- **Cursor pagination is opaque** — no random access, no snapshot; the `_xact_id` fingerprint
  makes staleness loud.
- **Current-state reads, not pinned dataset versions** — Braintrust versioning exists
  (`version` param filters to a transaction id) and pinning is the named upgrade.
