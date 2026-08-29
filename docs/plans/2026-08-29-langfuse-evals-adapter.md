# Langfuse Evals adapter

The third `core.Evals` implementation (jsonl, langsmith, this). Written against the LangSmith
plan's precedent deliberately: the two SaaS adapters share a transport shape, a duplicate
story, and an endpoint-security posture — divergence there would be debt, not independence.

**Phase-1 re-reviewed 2026-08-29 — verdict: amend; amendments applied.** The review's three
P0s are folded below: (1) the API facts the first draft asserted were contradicted by
Langfuse's own generated OpenAPI (wrong endpoint, nonexistent `status` filter, wrong envelope);
(2) the endpoint-security posture was absent where the precedent carries six mechanisms — a
credential-leak and SSRF exposure, not a style gap; (3) the headline weak-label claim depended
on a consumer (`weak_label_case_count`) that does not exist on main — the plan now
hard-sequences on PR #103 instead of asserting a mechanism that is not there. Every finding
(F1–F10) is folded and tagged.

## Problem

Teams running Langfuse keep eval sets as **datasets** — rows of `input` / `expectedOutput` /
`metadata`, very often harvested from production traces. Today Kno measures them only via a
manual CSV/JSONL export, which (a) loses the item id, metadata, and trace lineage a dataset
carries, (b) rots the moment the dataset changes, and (c) silently upgrades weak labels into
authored ones — the exact dishonesty `mine` exists to prevent. Same narrow question as
LangSmith: map a Langfuse dataset onto `core.Evals` — iterator contract, Provenance, dev/holdout
split, resume fingerprint — importing nothing above `adapters/`.

## Design

`adapters/evals/langfuse` — same shape as `adapters/evals/langsmith`:

```
Evals{ dataset, publicKey, secretKey, host, holdoutFrac, splitSeed }
Cases(ctx) -> iter.Seq2[*core.Case, error]
CountSplits / ContentHash        (the jsonl/langsmith surface — langsmith has no Close, and
                                  neither does this plan)                        (F7)
```

- **Transport, corrected against the vendor's OpenAPI** *(F1)*:
  `GET /api/public/dataset-items?datasetName=<name>&page=N&limit=100` — **not** the v2
  `datasets/{name}/items` route the first draft invented (v2 paths are only
  `GET/POST /api/public/v2/datasets` and `GET /api/public/v2/datasets/{name}`). Response
  envelope `{data: [...], meta: {page, limit, totalItems, totalPages}}` — page-numbered, not
  langsmith's cursor shape. The `datasetName` filter is optional in the API; the client always
  sends it, pinned by a fixture test (a dropped filter silently reads the entire project's
  items) *(F8)*. `limit` is set at the documented maximum (100; verified at fixture-record
  time, not assumed) *(F10)*. Stdlib `net/http`, basic auth (public key as user, secret key as
  password). Host from `LANGFUSE_HOST` (default `https://cloud.langfuse.com`), keys from
  `LANGFUSE_PUBLIC_KEY` / `LANGFUSE_SECRET_KEY` — environment only, never in kno.yaml, never
  in fixtures, never in logs.
- **Endpoint security, ported verbatim** *(F3)*: the langsmith package's endpoint checks are
  deliberately unshared and documented as such — copied here with the same note: scheme
  refusal (plain `http://` would ride the key in cleartext — basic auth is base64, not
  encryption), userinfo-in-URL refusal, private/loopback/link-local address refusal with no
  opt-out for link-local (169.254.169.254 — the cloud metadata endpoint), a dial-time recheck
  of the *resolved* address (the config-time check is TOCTOU), refusal of every redirect, and
  `redact()` applied at every error-construction point. The two existing opt-out flags
  (`--allow-insecure-base-url`, `--allow-private-address`) are wired through `cli/evals.go`
  exactly as langsmith wires them — self-hosted Langfuse is a real use. The langsmith plan's
  three security tests (http refusal, private-address refusal, header redaction) are carried
  over.
- **Name resolution before streaming** *(F6)*: the items endpoint does **not** 404 for an
  unknown dataset — `datasetName` is a filter, and a miss returns `200 {data: []}`. So the
  adapter first calls `GET /api/public/v2/datasets/{datasetName}` (which does 404), mirroring
  langsmith's `lookupDataset`: a typo'd name is refused loudly with an Actionable fix naming
  the dataset and `LANGFUSE_HOST`, before any page is fetched. The empty-dataset edge case
  stays legal — "the dataset exists and is empty" is a different claim from "no such dataset",
  and only the resolution pass can tell them apart.
- **Fingerprint, one request** *(F5)*: the first draft hashed the per-item
  `(id, updatedAt, status)` sequence — a full pass (20k requests at limit=100 for a 1M-item
  dataset), with server-return order (not a documented contract) and torn-pass semantics that
  no duplicate check could see. The review's cheaper alternative wins: the dataset object from
  the resolution pass carries `updatedAt`, and any item edit bumps it — so
  `ContentHash = host + dataset name + dataset updatedAt (+ version when pinned)` has the same
  edit sensitivity at one request, no ordering hole, no torn pass. The langsmith plan's
  pass-budget pricing discipline is inherited by not inheriting the pass.
- **Streaming**: pages fetched lazily inside the iterator (`iter.Seq2` end-to-end; a 1M-item
  dataset must not load into RAM). Each page: ctx check, decode, yield, fetch next. Fatal
  errors surface through the iterator's error return per the iterator contract. A torn page
  (dataset edited mid-pagination) is detected as a duplicate item id and made **fatal, naming
  the item id** — jsonl-identical semantics, langsmith-identical rationale: the same item seen
  twice across a pagination seam is a source defect to fix, not a count to paper over.
- **Case mapping**: id = item id (the row's primary key — the #45 premise holds here too);
  prompt = canonical JSON of `input`, key-sorted and golden-pinned — **null input is fatal**
  (langsmith parity: "a Case is a prompt and an expectation, not a corpus") *(F7)*; expected =
  canonical JSON of `expectedOutput`, empty when null (a judge Goal scores without it, and
  absence must not become a silent skip). Message-array inputs map to raw canonical JSON text
  as the prompt — the divergence from langsmith's turn-mapping is accepted in writing, not
  absorbed *(F7)*. No tags: langsmith sets none, and the mine plan's review rejected
  tags-as-markers because tags feed failure-cluster routing — a constant dataset tag pollutes
  that space *(F7)*.
- **Provenance, reconciled** *(F4)*: langsmith marks **every** Case `Derived: true` —
  hand-authored examples included — because a LangSmith example is always platform-managed
  state, never a kno-authored row. Langfuse has a per-item signal langsmith lacks:
  `sourceObservationId`/`sourceTraceId` non-empty means the expectation was harvested from a
  trace. This plan marks **derived exactly then**, with the item id in the provenance record.
  Consequence, stated rather than hidden: a weak-label count keyed on `Provenance.Derived`
  (the mechanism #103 delivers) will count trace-harvested Langfuse items as weak and
  hand-authored Langfuse items as authored, while LangSmith counts uniformly derived — the
  semantic difference is recorded in *What the numbers mean* in this PR. The two rules are one
  rule: "derived = the expectation did not originate as a kno-authored eval row"; the signal
  granularity differs by platform because the platforms differ.
- **The weak-label count, hard-sequenced** *(F2)*: `weak_label_case_count` does not exist on
  main; PR #103 (mine) delivers the consumer. This plan **blocks on #103 merged** and states
  the dependency: the derived-marking above lands here, the count lands there, and the
  end-to-end test ("a harvested item is counted as a weak label through a real baseline over
  the fixture") exists in this PR's test plan but only runs against the merged mechanism. If
  #103 does not land, this plan drops the marking-and-count claim in its entirety — marking a
  field with no consumer is the dead-field trap debt #42 records.
- **Split**: the shared `adapters/evals/split` package, exactly as langsmith: holdout fraction
  + `FingerprintSplit` on the Case id. The split is **id-keyed**: an item edit does NOT
  re-split (ids are stable under Langfuse upsert); it changes the fingerprint, which is what
  refuses a stale `--resume` — the two mechanisms are stated separately so nobody implements
  them as one *(F10)*.
- **Retries, precedent-shaped** *(F7)*: 429 only, honoring `Retry-After` with a bounded cap;
  no other status is retried (langsmith's rule, stated not absorbed). Every retry sleeps
  through a `ctx.Done()` check.
- **Errors, precedent-shaped** *(F7)*: the adapter returns plain wrapped errors; the CLI maps
  them to `errs.Actionable` in `cli/evals.go` (`langfuseNewFix`, the `langsmithNewFix`
  pattern). Mid-iteration errors surface through the iterator, where the CLI's fix-line
  wrapping differs from the open path — stated in godoc, as the langsmith plan required.
- **No spend**: read-only; no budget guard path, same as langsmith.
- **CLI**: `--evals langfuse:<dataset-name>`, parsed where the `langsmith:` prefix is parsed;
  help text and snapshot tests updated in the same PR.

## Alternatives considered

**The official Langfuse Go SDK.** Rejected: dependency weight and version churn for two
read-only endpoints; the stdlib pattern is already proven in-repo by langsmith, and the SDK's
surface (ingestion, span processing) is write-side machinery Kno never uses.

**CSV export from the Langfuse UI.** Rejected: manual, rots, and drops the trace lineage that
the provenance half of this plan exists to record.

**Reading items through the OTel ingestion API.** Rejected: wrong direction entirely — that
endpoint writes traces; datasets are served by the public API.

**A generic `http-json` Evals adapter parameterized over both SaaS vendors.** Rejected:
Langfuse and LangSmith auth, pagination, and row shapes differ enough that the shared
abstraction would be the wrong abstraction (the CLAUDE.md DRY rule: extract on the third
occurrence; Braintrust will be the test of that).

**Per-item content hashing for the fingerprint.** Rejected in review *(F5)*: full-pass cost,
unspecified server ordering, torn-pass semantics with no duplicate check. Dataset-level
`updatedAt` gives the same edit sensitivity at one request.

## Affected packages

`adapters/evals/langfuse` (new), `cli/` (prefix parse, `langfuseNewFix`, the two opt-out
flags, help snapshots), `docs/` (cookbook entry, adapters matrix, *What the numbers mean*
weak-label note), CHANGELOG. Nothing else.

## Proto / schema impact

None. `Provenance.Derived` already exists and langsmith already sets it; this adapter marks it
with finer granularity. No wire change.

## Edge cases

| Case | Behavior |
|---|---|
| Dataset missing | The v2 resolution pass 404s — Actionable refusal naming the dataset and `LANGFUSE_HOST` *(F6)* |
| Empty dataset that exists | Zero Cases, legal (baseline over zero cases refuses on its own terms) |
| Bad credentials (401/403) | Actionable refusal naming the two key env vars |
| 429 storms | Retry-After honored, bounded, ctx-aware; refusal after budget; no other status retried |
| Dataset edited mid-pagination (torn page) | Duplicate id is fatal, naming the id — source defect, not a count to paper over |
| `input` null | Fatal (langsmith parity) — a Case is a prompt and an expectation |
| `expectedOutput` null | Empty expected, provenance note — never a silent skip |
| Trace-harvested item (`sourceObservationId` set) | Provenance marked derived; counted weak once #103 lands |
| Archived items | `status == ARCHIVED` filtered client-side (the API has no status param) *(F1)* |
| Plain-http / private / userinfo / redirecting host | Refused by the ported endpoint checks, with the two CLI opt-outs |
| Holdout | Split by the shared package; the seal applies as everywhere |

## Test plan

- **Fixtures** recorded via `make record-fixtures` (`KNO_LIVE_TESTS` gated, secrets scrubbed at
  record time): item listing (2 pages), trace-harvested items, authored items, null fields,
  archived rows, resolution pass.
- Pagination: lazy fetch (a 2-page fixture proves the iterator streams, not slurps);
  torn-page duplicate fatal test; archive client-side exclusion; `datasetName` filter always
  sent *(F8)*.
- Endpoint security: http refusal, private-address refusal, header redaction — the langsmith
  plan's three, carried over *(F3)*.
- `coretest.CheckEvalsDuplicateIDs` passes; split determinism and edit-sensitivity (one item
  edit → dataset `updatedAt` moves → new `ContentHash`); the cross-adapter split-identity
  test (jsonl, langsmith, langfuse assign identical ids to identical splits) *(F8)*.
- Canonical JSON goldens (key order, unicode).
- Derived-provenance: the marking round-trips to the proto field; the
  counts-in-`weak_label_case_count` test runs against #103's merged mechanism *(F2)*.
- Auth/retry/rate-limit behavior against a local httptest server, including Retry-After.
- CLI grammar: `langfuse:` prefix accepted, opt-out flags wired, help snapshots, unknown host
  refusal.
- `goleak.VerifyTestMain`, the adapter-package convention *(F8)*.

## Rollback

Delete the package and the prefix parse. Nothing persists, nothing else references it.

## Docs impact

Cookbook entry ("Measure a Langfuse dataset"), adapters matrix row, *What the numbers mean*
weak-label note (including the LangSmith-uniform / Langfuse-per-item semantic difference),
CHANGELOG under Unreleased.

## Accepted risks

- **Current-state reads, not pinned dataset versions — an informed deferral now** *(F9)*: the
  items endpoint already supports a `version` query parameter (ISO 8601, returns items as of a
  point in time). Pinning needs a version timestamp obtainable from dataset runs; that is the
  v0.2 path, and the deferral records the capability exists rather than inventing an absence.
  Until then the `updatedAt` fingerprint makes staleness loud instead of silent.
- **Canonical JSON may differ from Langfuse's own rendering.** The golden pins Kno's encoding;
  provenance records the item id, so the source row stays findable.
- **Weak-label provenance trusts `sourceObservationId`.** Langfuse's own marker is the best
  available signal; a mis-marked item degrades to today's behavior, not worse.
- **Message-array inputs are raw canonical JSON as the prompt**, not langsmith's turn mapping.
  Accepted in writing; a turn-mapping rule is the named upgrade.
- **One fingerprint request per dataset `updatedAt`** is coarser than per-item: a rename
  changes the name half, any edit changes the timestamp half — the failure mode is a
  spurious-refused resume, never a silently-stale measurement.
