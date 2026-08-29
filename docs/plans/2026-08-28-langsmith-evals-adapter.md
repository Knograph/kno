# LangSmith Evals adapter

Fires [`docs/debt.md#45`](../debt.md#45) on purpose: its trigger is "when a second Evals adapter
lands", and this is that adapter — so the dedup repayment rides in the same change. Both halves
are scoped here.

**Phase-1 re-reviewed 2026-08-28 — verdict: amend, and one amendment replaced the original
design's centerpiece.** The first draft repaid #45 by changing `Store.RecordOutcome` to report
whether it inserted. The review proved that path has a money hole (a dropped duplicate takes its
settled spend with it, and a resumed run's restored cap is inflated by exactly that amount), the
identical defect exists one table over in the Value stage, and the premise — "two distinct
examples can collide on an id" — cannot occur: a LangSmith example id IS the row's primary key,
so an in-run duplicate is the same example seen twice across a pagination seam. The amended
design deduplicates at the adapter, jsonl-identical, and enforces the invariant in `coretest`
so every future Evals adapter inherits it. Every review finding is folded below, tagged with
its number.

## Problem

Teams that build agents on LangChain/LangSmith already keep their eval sets in LangSmith
**datasets** — versioned, human-curated example rows. Today the only way to measure them with Kno
is a manual export to JSONL, which (a) loses the row identity and version a dataset carries,
(b) rots the moment the dataset changes, and (c) makes the second Evals adapter land eventually
anyway, firing debt 45's trigger on whatever PR happens to add it. Doing it deliberately is
cheaper than inheriting it accidentally.

The design question is narrow: **map a LangSmith dataset onto `core.Evals`** — iterator contract,
Provenance, dev/holdout split, resume fingerprint — without importing anything above `adapters/`.

## Design

`adapters/evals/langsmith` — one package, same shape as `adapters/evals/jsonl`:

```
Evals{ dataset, apiKey, endpoint, holdoutFrac, splitSeed }
Cases(ctx) -> iter.Seq2[*core.Case, error]
```

- **Transport**: LangSmith's REST API, stdlib `net/http`. Endpoint from `LANGSMITH_ENDPOINT`
  (default `https://api.smith.langchain.com`), key from `LANGSMITH_API_KEY` — environment only,
  never in `kno.yaml`, never in logs. No new dependency: the dataset surface is two endpoints
  (list datasets, list examples) — list-examples returns complete example payloads paginated via
  the `{items, next_cursor}` envelope, so the first draft's "fetch one" third endpoint was an
  N+1 anti-pattern and is dropped. *(R8)*
- **Pagination**: cursor-based, page size 100, `Retry-After`-aware backoff for 429s (LangSmith's
  rate limit is per-key, ~2000 req/min). A server that repeats a cursor is a defect: a page-count
  cap bounds the loop and fails loudly instead of spinning. *(R8, R10)*
- **Row mapping, deterministic** *(R6)*: `inputs`/`outputs` decoded as ordered JSON, not as a
  map — named keys first (`question`, `input`, `answer`, `output`), then document order. Go map
  iteration is randomized, and a mapping that varies between passes would silently reclassify
  Cases. Chat-format datasets carry `inputs.messages` — extract the concatenated message
  contents as `Case.Input`, and `Expected` from `outputs.answer` when present, else from the last
  assistant message. Both formats map; the rule is stated once, not claimed.
- **Split**: LangSmith has no dev/holdout concept. A **shared `adapters/evals/split` package**
  holds `assignSplit`, `fingerprintSplit`, `SplitCounts`, `MinHoldout`, and
  `DefaultHoldoutFrac` — moved out of `jsonl`, imported by both adapters, and the CLI keeps
  compiling against the same types. "Byte-for-byte" is then a compile-time fact rather than a
  copied hash that can drift. `HoldoutFrac: 0` maps to `DefaultHoldoutFrac` exactly as jsonl
  does, or Value's dev-set size changes silently. *(R4, R5)*
- **Streaming**: examples are fetched one page at a time inside the iterator closure; `ctx.Err()`
  checked before every yield; cleanup deferred inside the closure. Memory profile identical to
  jsonl. **Latency is not**: `CountSplits` and the consent quote need a full pass before the run
  iterates again — at page 100 and ~2000 req/min, a 1M-example dataset is ~10k requests per pass,
  and a run costs 2–3 passes. Priced here rather than pretended away; the JSONL path has the same
  shape at disk speed. *(R4)*
- **Errors**: failure to open (bad key, unreachable host, dataset not found) = the outer error.
  A malformed ROW mid-stream is fatal, per the `core.Evals` contract — counted and named in the
  fatal error, never yielded as skippable. Mid-pagination 404/transport failure is also fatal
  (yielded), and the CLI's open-vs-yield wrapping difference is stated in the adapter godoc.
  *(R10)*
- **Credentials are the one live seam**: recorded-fixture tests cover parsing and pagination;
  live tests are `KNO_LIVE_TESTS=1` only, never in PR CI.

### The #45 repayment: dedup at the adapter, invariant in coretest

Debt 45's actual invariant — the one its own text names — is "the Evals adapter dedups Case IDs
at load". The amended design keeps that invariant for the second adapter **identically**: a
duplicate example id is **fatal, naming the id**, the same semantics `jsonl.go` already has.
An in-run duplicate from LangSmith is the same example seen twice across a pagination seam of a
concurrently-edited dataset — a source defect the user should fix, not a count to paper over.

The original draft's insert-report path is **rejected and recorded**: the review proved a dropped
duplicate discards its settled spend from the durable record (a resumed run's restored cap is
inflated by exactly that amount — prime directive 4), the identical hole exists in the Value
stage over `RecordMeasurement`, and reporting the count would need a wire field the plan
promised not to add. *(R1, R2, R3)*

To make the invariant a **core** property rather than a per-adapter convention, `coretest`
gains one conformance assertion: `ConformIterator` (or a sibling it already runs) fails an
Evals adapter that yields a duplicate Case ID. That is the "dedup in core" arm of the trigger,
without a `store.Store` signature change, a migration, or ~20 test-fake edits. *(R1, R9)*

### CLI grammar

`--evals` today takes a file path. The adapter gets a scheme:

```
kno baseline --evals langsmith:my-support-dataset
```

`langsmith:` prefix, dataset name after — same orthography as `--agent openai:model`. A bare
path keeps meaning "JSONL file". Self-hosted teams set `LANGSMITH_ENDPOINT`. No proto change.

### Resume fingerprint: ContentHash and CountSplits for a remote source

The CLI needs both. Defined here rather than discovered at implementation *(R4)*:

- **`CountSplits`**: one full streaming pass computing dev/holdout counts by the shared split
  rule. That is a second full download on top of the run's own iteration — the pass budget above.
- **`ContentHash`**: `dataset_id + ":" + modified_at + ":" + example_count` — metadata, not
  content. A content hash is a third full pass for a fingerprint whose job is to catch "you
  resumed against a different dataset"; the metadata form catches versions, renames, and
  re-uploads. An in-place content edit that leaves `modified_at` untouched is undetected — that
  gap is accepted below, and it is narrower than the gap today, where the fingerprint does not
  exist at all. The resume gate's semantics are unchanged: a mismatch refuses the resume with
  the existing fix line.

### Endpoint security: parity with the agent adapters *(R7)*

- `LANGSMITH_ENDPOINT` of `http://` (not `https://`) is **refused** — the key would ride the
  connection in cleartext.
- Private/loopback endpoints are **refused by default**, with the same opt-out grammar the agent
  adapters use (`--allow-private-address`-class flag on the CLI, mirroring `--base-url`
  behavior).
- Error strings redact the `Authorization` header value — stated as a rule, tested like the
  agent adapters' `classify` tests.
- `SSL_CERT_FILE` honored via the default transport and system pool; a test pins that no custom
  TLS config bypasses it.

## Alternatives considered

**A full LangChain/LangSmith Go SDK.** Rejected: a dependency graph for two REST endpoints, on
a seam (`adapters/evals`) that is the designed contributor on-ramp.

**A `kno import` command that snapshots a dataset to JSONL.** Rejected: two-step flows lose row
identity, version, and liveness — the dataset changes, the snapshot doesn't, and nothing tells
the user.

**The insert-report path for #45 (the original draft's centerpiece).** Rejected in Phase-1
review — money hole on the dropped-duplicate spend, the identical Value-stage hole, and a wire
field the plan promised not to add. Recorded here so the next reader sees why the store
interface was NOT changed.

**A generic CSV/JSONL-over-HTTP adapter.** Rejected: LangSmith datasets have a real schema, and
pretending it is generic buys nothing while making the row mapping someone else's problem.

## Affected packages

`adapters/evals/langsmith` (new), `adapters/evals/split` (new shared package; `jsonl` re-wired
to it), `coretest` (the duplicate-ID conformance assertion), `cli` (the `--evals` grammar branch,
endpoint-security flags), `docs/debt.md` (#45 repaid), `docs/cookbook/` (LangSmith recipe).

## Proto / schema impact

**None.** No wire type changes; `buf breaking` untouched. The #45 repayment deliberately makes
no store change.

## Edge cases

| Case | Behavior |
|---|---|
| Dataset deleted between open and iteration | Fatal error naming the dataset — outer on open, yielded mid-pagination; both paths and their CLI wrapping are stated in godoc |
| Empty dataset | Open succeeds; zero Cases; Baseline refuses a zero dev split as it does today |
| Row whose `inputs`/`outputs` hold no string field | Counted and fatal, naming the example id — never skipped |
| `outputs: null` | Legal in LangSmith — an empty `Expected`, named in Provenance rather than silently |
| Chat-format dataset | `inputs.messages` extracted per the mapping rule; `Expected` from `outputs.answer` or the last assistant message |
| Duplicate example id (pagination seam of a concurrent edit) | **Fatal, naming the id** — the #45 invariant, jsonl-identical. `coretest` enforces it for every future Evals adapter |
| 429 / transient HTTP errors mid-pagination | Retryable with `Retry-After`-aware backoff, mirrored from the agent adapters; persistent failure is fatal |
| A server repeating a pagination cursor | Page-count cap; fails loudly, never spins |
| Self-hosted endpoint over plain HTTP or a private address | Refused by default; opt-out mirrors the agent adapters' grammar |
| Dataset edited between baseline and value runs | New ids have no baseline score and are dropped from pairs; deleted ids vanish. jsonl has the same gap today — accepted, named, and the fingerprint catches the coarse case (versions) |
| Response rows exceeding a size cap | Per-row cap like `jsonl`'s `maxLineBytes`, fatal with the example id |

## Test plan

- **Recorded fixtures** in `adapters/evals/langsmith/testdata/`: one LLM-format dataset, one
  chat-format, one with a malformed row, one empty, one with a duplicate example id — captured
  with provenance (`note.txt`: endpoint, capture date, sha256) and secrets scrubbed at record
  time, the adapter convention. `KNO_RECORD_FIXTURES=1` re-records.
- `coretest.ConformIterator` and `coretest.CleanupProbe`, as `jsonl` proves them — plus the new
  duplicate-ID conformance assertion, verified failing against a deliberately-duplicating
  adapter.
- The shared split package: `jsonl`'s existing split tests move with it and stay green; a
  cross-adapter test asserts `jsonl` and `langsmith` assign identical ids to identical splits.
- Deterministic mapping: the same fixture maps identically across repeated parses (a
  randomized-map regression would fail this).
- Endpoint-security refusals: `http://` and private-address endpoints refused; header
  redaction test; `SSL_CERT_FILE` transport test.
- CLI snapshot tests for the `langsmith:` grammar (and for a bare path still meaning JSONL).
- Live tests: `KNO_LIVE_TESTS=1` only, never in PR CI, listed in the adapter capability matrix.

## Rollback

Delete the adapter package, the split package re-import, and the CLI grammar branch. No store
schema change, no migration, nothing to unwind.

## Docs impact

- Godoc on the package (the Ring-1 on-ramp: this is the template for the next Evals adapter).
- CLI help snapshot for the `--evals` grammar and the endpoint-security flags.
- Cookbook entry (LangSmith datasets into a baseline) — lands with or right after the adapter.
- `docs/debt.md#45` repaid; the ledger gains any entry this review accepts.
- CHANGELOG entry under Unreleased.

## Accepted risks

- **The LangSmith REST schema can change under us.** Bounded the same way the pricing detector
  bounds page changes: the fixtures fail loudly, and a live run is a schedule away.
- **The metadata fingerprint misses in-place content edits** that leave `modified_at`
  untouched. Narrower than today's nonexistent fingerprint, and the coarse cases (versions,
  renames, re-uploads) are caught. *(R4)*
- **Two full passes per run at remote-latency.** A 1M-example dataset is ~10k paginated
  requests per pass. Accepted for the remote source; the consent quote is where the cost is
  visible. *(R4)*
- **Baseline→value dataset drift** is not detected beyond the fingerprint. Same gap as jsonl
  today; the fingerprint catches versions, not mid-run edits. *(R10)*
