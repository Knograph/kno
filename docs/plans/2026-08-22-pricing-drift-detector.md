# Pricing drift detector

Repays [`docs/debt.md#40`](../debt.md#40). Supersedes the first draft of this plan, which
proposed a code-generating worker that opened pull requests. Phase 1 blocked it; this is the
rewritten scope. The generator is deferred, not abandoned — see "What is deferred".

**Re-reviewed 2026-08-28, after M2-11 landed and 0.0.1–0.0.4 shipped** — the condition the
deferral named ("after M2-11") is met, and a Phase-1 re-review against the current codebase
returned verdict **implement with amendments**. The amendments are folded into the sections
below, each tagged with its review finding; nothing is deferred a second time. The deferral
disposition: M2-11's adapters showed that identifiers reach `Lookup` exactly as the original
review predicted — aliases, dated pins, and provider variants — so the detector's discovery
rules are now writable against real observed shapes rather than guesses.

## Problem

`adapters/agent/pricing.Version` is `"2026-08-21"` and the table is hand-entered. Providers
reprice and ship models continuously. Two failure modes, not symmetric:

- **A stale price** produces a wrong estimate. Bounded — settlement reconciles against the
  provider's reported usage, so the spend is right even when the estimate was not.
- **A missing or wrongly-matched model** produces a wrong *ceiling*. `docs/debt.md#46` is
  exactly this, found by fetching the provider's live model list during review: nothing in
  the repo was watching, and a human reading a page found it in minutes.

Nothing today tells us either has happened. A third failure mode is time itself: the table is
hand-entered, and debt 40's trigger is "before 0.1.0, **or when the committed table is 90 days
old**". A detector whose sources all hold still would be green forever while the age trigger
lapses with no disposition — so the age check is the detector's check 0. *(R1)*

## What this builds

A **detector**. It fetches, compares against the committed table, and reports. It generates
no code, opens no pull request, and holds no write token. It *does* open issues — one per
signature, deduplicated, closed with a verification comment when a later run is green.

```
make pricing-check     (also a scheduled CI job, weekly)
  ├── check 0: table age — the trigger's 90-day leg, no network needed
  ├── fetch     three sources, read-only
  ├── compare   committed table vs each source
  └── report    gated checks fail the job; reported checks land in the issue body
```

The output is a failing job and an issue. A human then edits `table.go` by hand — a
thirteen-row file — and that edit arrives as an ordinary reviewed diff with every rationale
comment intact. That is what debt 40's trigger asks for: *"a price change is reviewed like
code."* A generator is one way to get there; it is not the only way, and it is by far the
more dangerous one.

## Why not the generator (the superseded draft)

Phase 1 established four things I had wrong:

1. **My gate threshold was set at the amplitude of the most likely bug.** I proposed failing
   on price moves >50%. Anthropic's batch-pricing table is *exactly* −50% on every row, and
   the modal scraper error is picking the wrong table for the right model name. The gate
   passes the error it exists to catch.
2. **`table.go` is not a data file.** It carries the only in-code pointer to
   [`docs/debt.md#41`](../debt.md#41), the `nil`-versus-zero rationale that
   `EstimateWithPrice` depends on, and `Lookup`'s measured-incident comment. A generator has
   no source for any of it. It also has no memory of deliberate omissions — `claude-mythos-5`
   is absent on purpose and would be re-proposed every week forever.
3. **My round-trip byte-identity test does not prove what I claimed.** It constrains the
   renderer only until the first legitimate formatting change, which regenerates the golden
   and reformats every line *in the same diff* — precisely where one changed digit hides.
4. **My OpenRouter margin claim was unmeasured and wrong.** `anthropic/claude-opus-5`
   reproduces Anthropic's published rates to the cent across all five dimensions. The
   conclusion (not a price of record) survives for a different and better reason: it *can*
   disagree, and does — `openai/gpt-5.6-sol` is reported at half what OpenAI's own page says.

That last one is the whole design. **Disagreement between two independent sources is a
signal with no free parameters,** where "moved more than 50%" is a number I made up.

## Design

### Sources

| Source | Mechanism | Role |
|---|---|---|
| OpenRouter | `GET /api/v1/models`, JSON | Cross-check + model discovery |
| Anthropic | HTML, real `<table>` markup | Price of record for `anthropic` |
| OpenAI | HTML, **zero `<table>` elements** — nested divs, hashed CSS class names | Price of record for `openai` |

The two HTML pages are different problems, not one. Anthropic's is a table-selection problem
(13 tables, "Claude Opus 5" appears in four). OpenAI's has no tables at all, and the only
stable anchor is label text (`Input`, `Cached Input`, `Output`) with a sibling lookup —
any selector keyed on the build-hashed class names breaks on an unrelated CSS build.

### The checks, in order of how much they are trusted

**Check 0 — table age (gate).** Parse `pricing.Version` (`table.go:33`, format `YYYY-MM-DD`)
as a date; fail when the table is older than 90 days, naming the version and its age in the
report. This is the only check that needs no network and it is the one the ledger trigger
names — debt 40's 90-day leg is otherwise enforced by nothing, and `scripts/ledger-check.py`
deliberately does not parse prose dates. *(R1)*

**Check 1 — cross-source agreement (report).** Where two sources price the same model, they
must agree. No threshold, no tuning. Verified by hand today: it reproduces Anthropic exactly
and flags the live `gpt-5.6-sol` disagreement. **A known disagreement is suppressed by
committed entry, not by the job failing forever:** `gpt-5.6-sol` (OpenRouter reports half of
OpenAI's own page) is seeded as a suppressed disagreement with its reason, and any NEW
disagreement is reported in the issue body. Disagreement alone never fails the job — it is a
signal for a human, not proof of a defect, and a job that is red from birth gets muted, which
is the failure debt #36 and #70 describe. *(R2a)*

**Check 2 — header-literal selection (gate).** The Anthropic table is *selected* by its
header row: find the table whose header equals a committed literal; zero matches or more than
one is an error. This makes the header literal the selector rather than a post-hoc check —
a layout change that moves or inserts a table errors deterministically, where a model-count
heuristic cannot (the standard and batch tables have identical row counts). The committed
literal spans the full column set, including both cache-write columns, because the 1.25×/2.00×
invariants are checkable only against this page — OpenRouter exposes only prompt/completion,
so the cache columns have exactly one source. *(R7)*

**Check 3 — ratio invariants (gate).** Per provider, held on 100% of live rows at review
time: `cache read = 0.10 × input` for both providers; for Anthropic additionally
`5m cache write = 1.25 × input` and `1h cache write = 2.00 × input`. A row violating its
provider's invariants is a parse error regardless of magnitude. Paired with (2), never
used alone — a batch row violates none of them.

**Check 4 — discovery (report).** A model present at a source and absent from the table
as judged by **`Lookup` itself**. The plan's first draft said "never `Lookup`": when it was
written, `Lookup`'s longest-prefix fallback resolved `claude-opus-5-fast` to base and would
have hidden the variant. That fallback now accepts only a **version** suffix, so `Lookup`
is exactly the right covered-predicate — `claude-sonnet-4-5-20250929` resolves (covered),
`claude-opus-5-fast` does not (candidate). Reimplementing an exact-key rule beside the real
one is a second copy of the rule that can drift from `table.go`; calling the real one cannot.
*(R3)*

**Check 5 — prefix-collision hard stop (gate).** A *discovered* identifier that
prefix-resolves to an existing row — i.e. `Lookup` says not-found but `longestPrefix` finds a
base — is a defect, not a note: it is debt 46's failure mode, watched for automatically now
that we know it happens. Gated **subject to the pending exclusion below**, because debt 46's
variants are *known* to exist and their rows are owed before 0.1.0; the detector is their
promised input, so they are reported every run under a "pending, linked to debt 46"
disposition rather than failing the job every week until the rows land. *(R2b, R9)*

A magnitude comparison is still *reported*. It is not a gate.

### Report-vs-gate classification *(R2a)*

| Check | On failure | On success |
|---|---|---|
| 0 table age | **gate** — job exits non-zero, issue filed | silence |
| 1 cross-source disagreement | report — issue body only | nothing (and closes open issues, see lifecycle) |
| 2 header selection | **gate** | silence |
| 3 ratio invariants | **gate** | silence |
| 4 discovery | report | silence |
| 5 prefix collision | **gate** except pending-debt-46 entries (report) | silence |

### Suppressions and the exclusion list *(R2a, R8, R9)*

Two committed lists, both structs carrying a reason per entry:

- **Suppressed disagreements**: `model, sources, reason`. Seeds: `gpt-5.6-sol`
  (OpenRouter vs OpenAI, OpenRouter reports half). A suppressed entry whose sources have
  since converged is dead and **fails the check** — the same lifecycle as exclusions below.
- **Exclusions**: `model, reason, ledger link, disposition`. Dispositions:
  - `deliberate` — `claude-mythos-5` (invitation-only) and `claude-mythos-preview` (named in
    `estimate.go`'s `newTokenizerModels` and equally absent). Seeds both, or the first run
    flags the second. *(R8)*
  - `pending` — the debt-46 variants, linked to `docs/debt.md#46`. Reported in the issue
    body every run so the "natural input" survives, but not gated.
  - An exclusion whose model has since gained a table row is dead and **fails the check** —
    that is the mechanism that forces cleanup the day #46's rows land.

### Issue lifecycle *(R2c)*

One open issue per signature, never a re-open storm:

- The run renders a report; a `gh issue create` step (repo token, `issues: write`) files it
  under the `pricing-drift` label. The step receives only the rendered text.
- **Dedupe, set-based** *(Phase-3 refinement, 2026-08-28)*: the current run's signature SET
  is every `FAIL` or `REPORT` line with digit runs collapsed, deduplicated. An open issue's
  signature is the same normalization over the `FAIL`/`REPORT` lines in its body. Membership
  rules: **intersect → update** (the issue gets the fresh report body); **disjoint →
  close** (with a "finding absent from the <date> run" comment); **create** — at most ONE
  new issue per run, for the first report-order signature no open issue carries. The
  at-most-one rule is what makes "never a re-open storm" true: a per-signature create would
  file duplicates every run, because every created issue's body is the full report and its
  signature set intersects every other's. A first-line-only signature was rejected for the
  symmetric reason: when a transient finding resolved, the signature flipped to the next
  line, the old issue was orphaned forever and a new one filed. A gated failure prints its
  detail as a line beginning `FAIL`; a report-only finding begins `REPORT` — this is the
  output contract the workflow enforces, and nothing else may begin with either word.
- **Close-when-absent**: the disjoint rule above. A run with an empty set is disjoint from
  everything, so it closes all open issues — the old "close-on-green" is the empty-set case.
  Job color is keyed on the detector's exit code alone: exit 0 with a non-empty set files
  and stays green. The evergreen pending-46 issue (the 16 owed rows) is expected to stay
  open and refreshed weekly until the rows land, at which point the detector's own
  dead-exclusion check fails the run and a human removes the exclusions. Silence is not the
  same as closure — and neither is a report that nobody files.

### Containment

The detector reads third-party bytes and writes none of them to disk as code. Still:

- Fetch timeouts, response-size caps, and a host allowlist — `CLAUDE.md` mandates these at
  the plugin boundary, and the open internet deserves no less scrutiny.
- A stable identifying `User-Agent`.
- The workflow: **its own file** — `.github/workflows/pricing-check.yml` — weekly cron
  (the 90-day trigger wants ~12 checks/year; daily HTML fetches burn the sources' goodwill),
  plus `workflow_dispatch` and `push` to main (drift caught right after a merge, safe once
  dedupe exists). A workflow has exactly one schedule, and nightly.yml's is taken. *(R6)*
- Permissions: `contents: read`, job-level `issues: write`, no secrets. The issue step is
  `gh` on the runner — the precedent changelog.yml set for API work, zero actions to pin.
  Any action used (checkout, setup-go) is pinned by SHA per [`docs/debt.md#14`](../debt.md#14).
  *(R6)*
- Bot protection: the HTML endpoints are reachable from GitHub runner IPs today; a
  Cloudflare-class block flips the job to fail-soft "unreachable" rather than hard-failing.
  Named so that a week where *all three* sources are unreachable is read as "no data", not
  "everything is fine". *(R8)*

## Makefile placement *(R10)*

`make pricing-check` is **not** part of `make check`: a network-dependent gate in PR CI is
flaky by construction — same posture as `test-live`. The parser/check tests ride `go test
./...` against fixtures; live fetches are behind `-tags=integration` and
`KNO_LIVE_TESTS=1`, per adapter convention, and `make record-fixtures` reuses the
`KNO_RECORD_FIXTURES` path. The scheduled workflow owns the live run.

## Alternatives considered

**Fetch prices at runtime.** Rejected, standing decision (M2 plan, Alternative D). An
endpoint that is down forces a choice between refusing to run and running with no ceiling;
one that is wrong is a spend path with no ceiling at all.

**The code-generating worker.** Rejected for now — see above. Its cost is a supply-chain
path from third-party bytes into compiled Go, and its benefit is saving a hand-edit of
thirteen rows.

**Vendor a third-party pricing library.** Rejected: a dependency deciding how much of the
user's money we may authorize is the last place to take on someone else's maintenance risk.

**Do nothing until the first tagged release.** Genuinely tenable — that is the entry's
trigger, and [`docs/debt.md#13`](../debt.md#13) says the release pipeline does not exist yet.
Rejected because debt 46 proved detection has value *now*: a live mispricing sat in `main`
and only a manual fetch found it.

## Affected packages

`internal/cmd/pricingcheck` (new, `package main`), `adapters/agent/pricing` (test-only —
the detector reads the exported `Lookup`/`Models`/`Version` surface), `.github/workflows/`
(new `pricing-check.yml`), `Makefile`, `CODEOWNERS` (none — the existing
`.github/workflows/` rule covers the new file), `docs/debt.md`, `CONTRIBUTING.md`,
`docs/what-the-numbers-mean.md`.

**New dependency: `golang.org/x/net/html`** — the stdlib has no HTML parser, and hand-rolling
one creates a *new parser*, which [`docs/debt.md#4`](../debt.md#4) requires to ship with a
fuzz target. x/net/html is the spec-compliant HTML5 parser (quasi-stdlib, Google-maintained,
BSD-3-Clause); the extraction logic built on it is fixture-defended rather than fuzzed, which
is the division debt 4 draws: the *tokenizer* is off the shelf, the *semantics* are tested.
*(R5)*

## Proto / schema impact

**None.** The detector writes nothing and changes no wire type.

Phase 1 raised a related gap that is **not** this plan's to fix and is ledgered separately:
nothing compares `pricing_table_version` on resume, so a run interrupted under one table and
resumed under another is capped against a blend of two price regimes. That needs the resolved
`Price` recorded on the `Run` and a `checkResumable` check — additive proto, and it belongs
with M2-10/M2-11 which already write `CaseExecution`.

## Edge cases

| Case | Behavior |
|---|---|
| A source is unreachable | Reported as unreachable; other sources still check. One provider being down must not blind the others |
| A source returns HTML where JSON was expected | Treated as unreachable, **not** as an empty model list — empty would read as "every model was removed" |
| OpenRouter returns valid JSON with a moved envelope (no `.data`, or entries without `id`/`pricing`) | Treated as unreachable, not as empty — assert `.data` is an array of objects carrying `id` and `pricing.prompt`/`pricing.completion` strings before trusting it *(R8)* |
| Two sources disagree | Report both values and both URLs. Do not pick a winner. Gated only if NOT suppressed (see the lists above) |
| A legitimate >50% repricing | Reported, does not fail the job by magnitude alone. Generational cuts of this size are ordinary — Opus 4.1 at $15 to Opus 4.5 at $5 is −67% |
| A model is retired at the source | Reported. Never acted on — a removed row turns priced Cases into refused ones |
| A deliberate omission (`claude-mythos-5`) | Suppressed via the exclusion list, reason per entry — including `claude-mythos-preview`, which the first draft of the list missed *(R8)* |
| Discovery that prefix-collides | Gated as a defect — except the debt-46 variants under their `pending` disposition, which are reported every run until the rows land |
| Unit normalization | OpenRouter is USD/token as a decimal *string*; pages are USD/MTok; the schema is micro-USD/MTok. Parsed with `math/big.Rat`, not float — a named test asserts all three representations produce identical micro-USD |
| A model priced by exactly one source (not on OpenRouter) | No cross-check exists for it; coverage is invariants + age only, and the report says so rather than leaving the single-sourced row's status invisible *(R8)* |

## Test plan

- Fetchers and parsers against **recorded fixtures** in `internal/cmd/pricingcheck/testdata/`
  (not a top-level path — Go tooling expects it under the package). Live fetches are
  `KNO_LIVE_TESTS=1` only, never in PR CI.
- **Fixtures are real captures**, trimmed only of `<script>`/`<style>` and whitespace, with
  structure intact — a hand-built fragment cannot regression-test *table selection*, the
  fragile step. Each fixture dir carries a `note.txt` with provenance: source URL, capture
  date, and sha256 of the full untrimmed capture — the convention the adapters'
  `refusal-preoutput/note.txt` set. *(R7)*
- Every check gets a fixture that trips it, each verified to fail before the check is written.
- A deliberately-broken HTML fixture (columns shifted, layout changed) asserts the parser
  **errors** rather than returning a plausible wrong number. This is the case that matters
  most and the one the superseded design got wrong.
- A fixture reproducing the batch table asserts it is refused by the header check — the
  specific bug that defeated the old gate.
- A fixture where the target table was moved or duplicated asserts the header-literal
  *selector* refuses: zero or two matches are both errors. *(R7)*
- Check 0 needs no fixture: it is unit-tested against `pricing.Version` values injected as
  strings, including the 90-day boundary itself.
- The anthropic fixture must carry both cache-write columns so the 1.25×/2.00× invariants are
  actually checkable. *(R7)*
- Exclusion lifecycle tests: a dead suppression, a dead exclusion, and a `pending` entry whose
  row has since landed all fail the check — the mechanism that forces cleanup when #46 closes.
- Coverage: the package is new and lands under the 70% repo-wide floor with the no-decrease
  ratchet; `make check` must be green before merge, and a floor entry is added if the package
  proves to warrant one.
- Fixture size: payloads are ~1.9 MB per full capture. Only the parsed-relevant subset is
  committed, not whole pages, and there is no per-run accumulation because nothing is
  captured on a schedule into the repo.

## Rollback

Delete the workflow and the Makefile target. The detector writes nothing, so there is no
state to unwind — which is most of why this scope was chosen.

## Docs impact

- `CONTRIBUTING.md` — how to respond to a drift issue, and that a price edit is a spend-path
  change reviewed accordingly
- `docs/what-the-numbers-mean.md` — cost figures mean "reported usage at these rates", and
  the rates carry a date

## What is deferred, and on what trigger

The generator, the renderer, the policy file, and the PR-opening workflow. **Disposition,
2026-08-28** *(R4)*: the deferral's condition ("after M2-11") has fired — M2-11 shipped and
0.0.1–0.0.4 released, and the adapters showed real identifiers reaching `Lookup` (aliases,
dated pins, variants). The generator is re-justified on the original argument, which holds:
the detector's issue output is the review trail a generator would need to earn write
authority, and nothing about post-M2-11 traffic weakens it. Revisit **when a price edit
caused by a drift issue has been reviewed twice**, the point at which the human step has
demonstrated its own cost.

## Accepted risks

- **Scrapers break.** Mitigated by failing loudly, and bounded because a broken scraper leaves
  the table exactly where it is today. The header-literal check turns "broke silently" into
  "broke visibly."
- **A compromised source publishes a plausible wrong price.** Cross-source agreement catches
  it unless both are compromised. The detector proposes nothing, so the blast radius is a
  false issue rather than a merged diff.
- ~~**A `--price` override still does not exist.**~~ **CLOSED by M2-11b** *(R4)*: the
  `--price-input-per-mtok` / `--price-output-per-mtok` flags landed at `cli/baseline.go:156`
  and flow through `EstimateWithPrice`. A user hitting a wrong or missing price now has a
  local remedy without waiting for a release.
- **The known `gpt-5.6-sol` disagreement is suppressed, and suppression is an act of trust.**
  Seeded with the one measured disagreement and a written reason; the stale-suppression check
  fails the run when the sources converge, so a suppression cannot outlive its cause.
  *(R2a, R9)*
- **Single-sourced rows have no cross-check.** The report names them; coverage is invariants
  and age only. Accepted because the alternative — dropping the row — converts an estimate
  into a refusal, and the row exists because a human already reviewed it once. *(R8)*
- **OpenAI page restructures fail soft where Anthropic's gate red** *(Phase-3 finding,
  2026-08-28)*: Anthropic's table selection is pinned by a header literal and gates; OpenAI's
  page has no table structure to pin, so a restructure degrades to "no data" REPORT lines
  and exit 0. Accepted: there is nothing deterministic to pin, and inventing an anchor that
  happens to hold today is a gate that fails open. A transient fetch failure is also
  indistinguishable from a restructure in the report; the close-when-absent rule means a
  blip files an issue that the next healthy run closes, so the churn is one issue per blip.
- **Report values are rounded to cents for display.** `formatUSDPerMTok` prints to cents,
  so a human must not copy a report value into `table.go`; the report header says so and
  CONTRIBUTING's response flow starts from the linked page, never from the report.
