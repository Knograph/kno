# Hugging Face adapters — Evals and Pool

One vendor, two Ring-1 adapters: **Evals** (HF datasets as eval cases) and **Pool** (HF
datasets as the asset pool). Planned together because they share the transport, the
revision-pinning question, and the CLI prefix surface.

**Phase-1 re-reviewed 2026-08-29 — verdict: amend; amendments applied.** The review verified
every API fact against the live datasets-server and corrected the load-bearing ones: the
`revision` query param is accepted and IGNORED (the pin is fake — the fingerprint comes from
the `x-revision` response header instead); the error taxonomy is 401/404/500-shaped, not the
404 story the first draft assumed (name resolution goes through `/splits`); skip-and-count
violated the Ring-0 contract (a dataset without an input-ish column is refused at open, not
skipped per row); and the plan undercounted its own ledger obligation (this IS the firing PR
for re-dated #68, and disposes the row). Findings 1–10 folded and tagged.

## Problem

HF hosts the world's largest corpus of structured datasets, versioned by git revision. Teams
that fine-tune or RAG on HF data today export CSV snapshots by hand. Kno should measure an HF
dataset as Evals and rank HF datasets as a Pool directly — with the revision the fingerprint
carries, so a re-measure after the dataset moves is a loud stale-refusal, not a silent drift.

## Design

`adapters/evals/hf` and `adapters/pool/hf`. The endpoint-security checks are extracted to a
shared internal package in THIS PR — the repo's own rule says extract on the third occurrence,
and this is the third *(F10)*: `internal/hfsecure` (or an adapters-internal home the
implementer names) holds the ported checks; langsmith/langfuse keep their copies (deliberately
unshared history stands) and the shared package is new code, tested, godoc'd.

- **Transport, corrected against the live API** *(F1, F2)*: datasets-server,
  `GET https://datasets-server.huggingface.co/splits?dataset=<org>/<name>` (name resolution:
  401 = dataset missing OR gated — the API conflates them, so the refusal presents BOTH
  remedies, the typo and `HF_TOKEN`; 404 names the config/split pair against the returned
  list) and `GET .../rows?dataset=&config=&split=&offset=N&length=100` (envelope
  `{rows: [{row, row_idx}], num_rows_total, partial}`). Stdlib `net/http`; gated datasets use
  `HF_TOKEN` (Bearer), environment-only.
- **Fingerprint, the real one** *(F1)*: `ContentHash = dataset + config + split +
  x-revision` — the `x-revision` RESPONSE HEADER on `/rows` (and `/splits`) carries the
  current git commit of the dataset; the `revision` QUERY PARAM is accepted and silently
  ignored by the server, so a pin built on it would be a promise that does nothing. The
  header is undocumented — the godoc says so and a fixture pins it; a MISSING header is
  fatal (langfuse-updatedAt precedent: a fingerprint is not optional). Per-page: each page's
  `x-revision` must match the first's — a mid-stream change is fatal, not a torn-page hope
  *(F8)*. Pinning to a past revision needs the Hub API / parquet refs — deferred with #83,
  and the plan no longer promises a flag that cannot work.
- **Evals adapter** — `Evals{ dataset, config, split, holdoutFrac, splitSeed }`:
  - Row mapping, Ring-0-shaped *(F3, F5)*: a dataset whose rows carry NO input-ish column
    (`input`, `prompt`, `question` — first present wins) is **refused at open**, naming the
    columns the dataset actually has — features are dataset-uniform, so a dataset-level
    mismatch is a dataset-level refusal, not a per-row skip. A null value in the chosen
    input-ish column is **fatal, naming the row_idx** (the langsmith null-input rule).
    `core.Case.Expected` is ONE string: the expected-ish column search order is `expected`,
    `completion`, `answer` — first present wins, others are DROPPED, and the golden pins
    that exactly one column feeds Expected (no joins, no lists; a multi-column dataset's
    loser columns are documented as out of scope) *(F5)*. Canonical JSON for structured
    values, key-sorted.
  - id = `row_idx` (stable within one revision); duplicate id across pages is fatal naming
    the row_idx.
  - `partial: true` in the envelope means the server returned a SUBSAMPLE — refused at open
    *(F7)*: a measurement over a subsample is a measurement over an unstated population.
  - Provenance: HF has no per-row trace-source signal — nothing is marked derived, and the
    godoc says the platform's signal is absent, not ignored (a dataset harvested from traces
    keeps its origin invisible to Kno; the honest number is the one without a guess).
  - Split: shared `adapters/evals/split`; id-keyed, edits don't re-split, the fingerprint
    refuses a stale resume.
- **Pool adapter, within the real interface** *(F6)*: `core.Pool` is `Assets()` — nothing
  else. `Pool{ dataset, config, split, kind }`: rows' text-bearing columns (string values)
  become Asset content; `kind` declared at the CLI (`hf:...:knowledge`) — undeclared kind is
  a refusal, not a guess (stricter than csv's omit→UNSPECIFIED, deliberately: an HF dataset
  is a named corpus, and a Pool of unknown kind is a Pool that cannot be ranked). Asset id =
  `<dataset>/<config>/<split>@<row_idx>`. The pool-side stale-refusal claim is WITHDRAWN:
  `Value.InputFingerprint` folds only the evals' ContentHash today — pool-side
  fingerprinting is new plumbing (a Pool interface addition), out of scope here and named
  as such, not smuggled.
- **CLI**: `--evals hf:<org>/<name>/<config>/<split>` and
  `--pool hf:<org>/<name>/<config>/<split>:<kind>` — the four-segment grammar pins slashes
  in config names unambiguously *(F9)*; help snapshots updated.
- **The #68 obligation, disposed** *(F4)*: this is the firing PR for re-dated #68's second
  leg. `docs/debt.md#68` gets its disposition IN THIS PR (the re-dated trigger's text
  requires the firing PR to record the acknowledged bias — the plan says so in the ledger
  row, and what-the-numbers-mean gains the line: the context_tokens denominator is estimated
  from bytes for HF content exactly as for md/csv, and the ~3x reservation bias applies).

## Alternatives considered

**The `huggingface_hub` Go SDK.** Rejected: heavy dependency for rows + splits; datasets-server
is the stable HTTP surface and stdlib is the in-repo pattern.

**Using the datasets library's streaming export (parquet).** Rejected twice: parquet is
explicitly deferred (docs/debt.md#83) and the rows endpoint returns plain JSON rows.

**Pin-by-query-param revision.** Rejected in review *(F1)*: the server ignores it; a pin that
silently does nothing is the exact failure this plan's fingerprint discipline exists to
prevent. The x-revision header is the real mechanism.

## Affected packages

`adapters/evals/hf`, `adapters/pool/hf`, the shared endpoint-security extraction (new),
`cli/` (two prefixes, help snapshots), `docs/` (cookbook entry covering both directions,
matrix rows, what-the-numbers-mean #68 line), `docs/debt.md` (#68 disposition), CHANGELOG.

## Proto / schema impact

None.

## Edge cases

| Case | Behavior |
|---|---|
| Dataset missing OR gated (both 401) | Refusal presenting BOTH remedies — the typo and `HF_TOKEN` |
| Config/split missing | `/splits` resolution names the real list; rows-404 names the pair |
| Rows-500 for a split `/splits` validated | Genuinely transient — the one retried 5xx, bounded |
| Dataset with no input-ish column | Refused at open, naming the columns it has |
| Null value in the input-ish column | Fatal, naming the row_idx |
| `partial: true` | Refused — a subsample is an unstated population |
| `x-revision` missing or changing mid-stream | Fatal — the fingerprint is not optional |
| Pool with undeclared kind | Refusal naming the flag |
| Empty split | Zero Cases/Assets, legal |
| Holdout | Shared split; the seal applies |

## Test plan

Hand-authored fixtures (datasets-server response shapes + note.txt; re-record via
`make record-fixtures` when HF_TOKEN exists): `/splits` resolution (401/404 taxonomy), two-page
rows listing with `x-revision` headers, gated 401, `partial: true` refusal, no-input-column
refusal, null-input fatal, multi-expected-column single-winner golden. Streaming +
torn-page tests; cross-adapter split identity (five Evals sources); canonical JSON goldens;
the security trio; Asset id stability; the holdout canary; `goleak.VerifyTestMain`; CLI
grammar for both prefixes.

## Rollback

Delete both packages, the shared extraction, and the prefix parses.

## Docs impact

Cookbook entry ("Measure and rank Hugging Face datasets"), matrix rows, #68 acknowledgment
line + ledger disposition, CHANGELOG under Unreleased.

## Accepted risks

- **`x-revision` is undocumented.** A fixture pins it; if datasets-server drops the header,
  the adapter fails loudly (fatal), which is the honest behavior for a fingerprint.
- **Expected is one column.** The search order is pinned; a dataset whose signal lives in a
  loser column is documented as out of scope, not silently averaged.
- **No derived marking on HF**: absent per-row signal; documented as absence, not denial.
- **Pool-side fingerprinting deferred** — the pool adapter trusts the id stability it
  constructs and the evals-side fingerprint; the named upgrade is a Pool interface addition.
