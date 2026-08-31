# Stage spend reporting: one contract, every stage

The stage that spends the most money says nothing about spending.

**Phase-1 re-reviewed 2026-08-31 — verdict: amend; amendments applied.** Five things changed, and
the audit underneath them is confirmed rather than hedged: `ValueResult` has no `Spent` field;
`BaselineResult.Spent` is set at `core/baseline_close.go:142` from `o.Guard.Spent()`; and `value`
**does** run a guard (`core/value.go:49`, checked at `:139`) whose settled total
`core/value_loop.go:441-446` reads back only to seed `Restore` on resume and never to report — so
the gap is plumbing and rendering exactly as §1's table says, and is **not** a recording gap.
`select.go` and `export.go` contain no budget reference at all. §7's premise holds too: ADR-0001
is exactly *"generated proto messages are the domain types"* with no `--json` stability content,
and `docs/cookbook/ci-gate.md:35` is the only stability prose in the repo, so ADR-0006 fills a
real hole rather than restating one. `docs/debt.md#50` matches the history §2 cites. One overclaim
is corrected: `cli/demo_test.go:245` asserts `SpentUSD == "$0.00"` and **nothing about call
counts** — the metered-zero-with-non-zero-calls pairing is this plan's AC16, not existing behavior
(§4). The substantive change is that bare absence no longer ships unargued: every stage document
gains a self-describing `guarded` boolean beside the absence rule, and the cookbook's `// 0` idiom
is withdrawn *(F1)*. Also amended: the PR's internal review sequencing is stated *(F2)*, `resumed`
reaches the human rendering and not only `--json` *(F3)*, ADR-0006 is described as codification
with the goldens and the AST test doing the actual binding *(F4)*, and a new acceptance criterion
covers a stage that spends and then fails for a **non-budget** reason *(F5)*.

**Phase-2 amendment 2026-08-31 — the docs surface moved.** Everything above was verified against
`4622b90`. [#163](https://github.com/uknoAI/kno/pull/163) then landed — *move the cookbook to
`uknoAI/kno-examples`, leaving tombstones* — and deleted this plan's docs surface out from under
it. `docs/cookbook/ci-gate.md` is now an 80-byte tombstone (`Moved to <…/recipes/ci-gate.md>.`),
and `scripts/cookbook-stub-check.sh`, which runs inside `make docs`, fails any stub that grows a
second line: *"a stub is exactly one line; anything more is a second copy of a recipe waiting to
drift from the real one."* Three items named that page. They are unwritable as stated, and one of
the corrections **strengthens** §7 rather than weakening it.

**The migration deleted the repository's only statement of the `--json` stability promise.**
`grep -rn "stable, hand-written"` now matches this plan file and nothing else. §7 argued ADR-0006
was worth writing because the promise lived in a recipe rather than a decision record — a
codification. That argument is now too weak for what happened: the promise does not live anywhere.
Every `jq` pipeline written against v0.1 is running against a contract this repository no longer
states. ADR-0006 is not a restatement; after this PR it is the **only** place the contract exists,
and the sentence it replaces is one the reader can no longer go and check. §7 is amended to say
that, and the withdrawn sentence is quoted there from the v0.1.3 tag rather than from a live path.

Six further corrections, each folded into the section it belongs to:

- **AC4** cannot be written: there is no fenced sample in a one-line tombstone to parse. Replaced
  with a check against an **in-repo frozen v0.1 shape fixture** this PR creates
  (`cli/testdata/json/v0.1-shape.json`, derived mechanically from the `v0.1.0` tag's struct tags),
  asserted as a subset of current output. That is a stronger regression guard than the original —
  it covers all five stage shapes rather than one page's baseline sample, and it does not depend
  on a second repository staying reachable or unedited.
- **AC17** linked the ADR from `docs/cookbook/ci-gate.md`. Linking a decision record from a
  redirect stub is wrong on its own terms and would now fail `make docs`. The in-repo home is
  **README's Documentation list** — defended below — plus a cross-link from
  `docs/what-the-numbers-mean.md`, the page that says what a cost figure claims. Updating the
  `kno-examples` recipe to cite ADR-0006 is a follow-up in that repository and is explicitly
  **out of scope for this PR**; it is recorded as a Debt Ledger row with a trigger rather than
  left as an intention.
- **AC13's state is unreachable.** `cli/report.go:74` marks `--value-run-id` REQUIRED, and
  `loadBaseline` (`cli/report.go:335`) refuses an empty baseline ID with *"the value run paired
  against no baseline"*. No `kno report` invocation can therefore reference zero metered runs:
  every page names exactly one Value run and one Baseline, and both stages run a guard. Rewritten
  against the reachable state that carries the same risk — every referenced run settled zero,
  which is precisely what a `fake:` pipeline in CI produces.
- **§1 and §5 state `report`'s flags wrongly.** `reportFlags` (`cli/report.go:20-27`) is
  `db`/`value-run-id`/`select-run-id`/`export-run-id`/`watch`/`json`. There is **no**
  `--baseline-run-id`: the Baseline is reached through `valueRun.GetBaselineRunId()`. The design
  is unaffected — `report` still holds every run ID it needs, it just does not hold them all as
  flags — but the sentence claiming a flag that does not exist is corrected in both places.
- **AC6's allowlist was wrong by more than double.** `formatUSD` has **nine** non-test call sites
  in `cli/` across **six** enclosing functions — `printEstimate`, `renderHuman`, `quoteEstimate`
  (twice), `renderJSON` (twice), `renderSelectJSON` (twice), `budgetCaps` — not the three the
  allowlist named. Re-deriving the list would produce a test that fails on the next legitimate
  cap or estimate rendering, which is a test about the wrong thing: `formatUSD` also formats
  estimates, caps and carrying costs, none of which are spend. The mechanism is replaced with an
  invariant that enumerates nothing — see AC6.
- **AC18 is self-referential on this PR.** There are no JSON goldens in the tree, so there is no
  "previous release's shape" for the first ones to be a superset of. Stated as such: on this PR
  AC18 establishes the baseline, and the superset check becomes load-bearing at the next release,
  when the frozen fixture of AC4 is re-cut against v0.2.

Nothing in §2, §4, §6 or §8 changed, and the confirmed audit underneath them is unchanged:
`ValueResult` has no `Spent`; `BaselineResult.Spent` is set at `core/baseline_close.go:141` from
`o.Guard.Spent()`; `value` runs a guard; `select` and `export` reference no `budget.Guard`
(`core/select.go`'s `budget` identifier is a `*knov1.Budget` local, not the guard);
`cli/demo_test.go:246` asserts `$0.00` and nothing about call counts; and `make update-golden`
exists (`Makefile:276-277`).


## Problem

**The bug, verified against the tree at `4622b90`.**

`spent_usd` exists in exactly **one** place in the whole repo: `cli/jsonreport.go:55`, on the
Baseline `--json` document, populated at `cli/jsonreport.go:105` from
`formatUSD(res.Spent.CostUSDMicros)`. The human equivalent is a single line at
`cli/render.go:242-243`:

```go
fmt.Fprintf(&b, "  spent      %s over %d call(s)\n",
    formatUSD(res.Spent.CostUSDMicros), res.Spent.Calls)
```

`kno value` reports **nothing** about spend in either rendering. `cli/value_render.go` contains
no occurrence of `spent`, `cost`, or `usd`; `valueReport` (`cli/jsonreport.go:185-190`) carries
`run_id`, `status`, `goal_direction`, `valuations` and no money field. The `--yes` pre-run line
in `cli/value.go:349-351` prints a *measurement count*, not dollars.

Value is the expensive stage. `DESIGN.md:361-364` sizes it: routed valuation is ~8,250 agent
calls and ~12–15k LLM calls at **$15–40** for a 50-asset, 400-case run, against a baseline of
400 calls. Value's calls scale by `Asset x routed Case x arm x trial`; Baseline's scale by
`Case`. The stage whose spend a user actually needs to see is the one with no meter on the
dashboard.

Downstream, this is not cosmetic. `uknoAI/kno-benchmarks` books a stage's cost from `--json`,
and a stage that emits no field is booked as zero — which for a paid agent is a wrong number
presented as a measurement, not a missing one ***(verify: the benchmarks repo is not in this
tree; this is the stated motivation, restated here for the record and not checked against its
code)***.

Two further holes, found while verifying the first and belonging to the same fix:

- **`CaseExecution.usage_estimated_case_count`** (`run.proto:402-404`) exists with the godoc
  *"Cases whose cost is the engine's prediction rather than reported usage. The spend figure is
  a guess to this extent, and says so."* It is written by `core/baseline_close.go:242` and
  **rendered by nothing** — verified by grep across `cli/`. The qualifier on the one spend number
  we do print is itself unprinted.
- **The human line prints calls; `--json` does not.** `spent_usd` has no `llm_calls` companion,
  so the two surfaces disagree about what a spend report contains.

## Design

### 1 — Per-stage audit: is this rendering, plumbing, or recording?

Answered from the code, per stage. The three layers are: **recorded** (durable, survives the
process — `Store.SettledSpend`), **plumbed** (reaches the CLI in a result struct), **rendered**.

| Stage | Runs a `budget.Guard`? | Recorded | Plumbed | Rendered | Gap |
|---|---|---|---|---|---|
| `baseline` | **Yes** (`core/baseline.go:118`) | `outcomes.cost_usd_micros` + `runs.orphan_*` | `BaselineResult.Spent` (`core/baseline.go:323`, set at `core/baseline_close.go:142`) | human + `--json` | **none** (except `llm_calls` and the estimated-usage qualifier) |
| `value` | **Yes** (`core/value.go:49`) | `measurements.cost_usd_micros` + `runs.orphan_*`; readable via `SettledSpend` | **NO** — `ValueResult` (`core/value.go:94-115`) has no `Spent` field | **NO** | **plumbing + rendering.** Not recording |
| `select` | No — grep finds no `budget.Guard` in `core/select.go` or `cli/select.go` | n/a; `SettledSpend` returns `{}` | n/a | prints *caps* and *carrying cost*, not spend | **naming/ambiguity** |
| `export` | No | n/a | n/a | nothing | **contract** (see §4) |
| `report` | No; `cli/report_render.go:166` prints *"Recorded aggregates only: no LLM calls"* | n/a for itself; it references runs that DO have spend | no | nothing | **rendering** — it is the one surface that should show the pipeline total |
| `mine` | No LLM calls at all | n/a | n/a | **has no `--json` flag** | out of scope |
| `demo` | Runs the loop over `fake:` | via the embedded stage docs | partial | baseline's `spent_usd` only, as embedded raw JSON | follows from the above |

Three things this table settles:

1. **Value is not a recording gap.** The money is fully durable. `store/sqlite.go:816-834`'s
   `SettledSpend` sums `outcomes + measurements + runs.orphan_*` in one statement, and its godoc
   at `store/sqlite.go:806-807` says *"this method is the only durable record of money spent."*
   `core/value_loop.go:442-446` already reads it back — but only to seed a resume's consent
   dialog, never to report.
2. **Select and Export have no spend to report, and their existing money fields are about
   something else.** `selectReportBudget.MaxCostUSD` (`json:"max_cost_usd,omitempty"`,
   `cli/jsonreport.go:267`) is the *cap the Portfolio was built under*, and
   `costReport.AcquisitionUSD` (`json:"acquisition_usd"`, `cli/jsonreport.go:298`) is the
   *carrying cost of the selected Assets*. Neither is run spend. A consumer scanning for dollars
   in `kno select --json` today finds two dollar figures, neither of which answers "what did this
   cost me" — which is worse than finding none.
3. **`Report` is a rendering gap of a different shape.** Its flags are `--value-run-id`
   (REQUIRED, `cli/report.go:74`), `--select-run-id` and `--export-run-id`; the Baseline is not a
   flag but is reached through `valueRun.GetBaselineRunId()` and loaded by `loadBaseline`
   (`:335`), which refuses an empty one. So `report` holds — by flag or by chain — every run ID
   the pipeline produced, and can call `SettledSpend` on each to produce the number nobody
   currently gets: **what the pipeline cost**. The two IDs it always has are exactly the two
   metered stages.

### 2 — Where the truth lives, and what must not become the source

Two readers, and the choice between them matters:

- **`Guard.Spent() Spend`** (`stats/budget/budget.go:712`), whose godoc is unambiguous:
  *"Spent reports what has actually been settled, excluding outstanding reservations. **This is
  the number a report shows.**"* In-process, exact, includes overshoot.
- **`Store.SettledSpend(ctx, runID)`** (`store/store.go:183`), the durable sum. Survives the
  process; the only reader available to a stage that did not run the guard.

**Decision: a stage that ran a guard reports `Guard.Spent()`; a surface reporting on *other*
runs reports `Store.SettledSpend`.** They agree by construction — `Restore` seeds the guard from
`SettledSpend` and every settled charge is persisted before the run ends — and a test pins the
agreement to the micro-dollar.

**Correction, found by writing that test** *(Phase-2 amendment)*. They agree on **calls and
cost**. They do **not** agree on **tokens** for a Value run. `core/value_loop.go`'s `sinkFunc`
records `budget.Spend{Calls, CostUSDMicros}` and drops the token count, while Baseline's
`settledSpend` (`core/baseline_budget.go:452-463`) carries all three from the Response. So
`Store.SettledSpend` returns `Tokens: 0` for a Value run whose guard settled a real token total —
and, since `Restore` seeds from `SettledSpend`, **a resumed Value run restores a token total of
zero**, which under-enforces a token cap across a resume. That is a budget-recording bug in
`core`, one struct field wide, and it is **not fixed in this PR**: changing what a stage persists
about money inside a rendering PR is precisely the drive-by this repository's process exists to
prevent, and it deserves its own plan, its own resume test, and its own review. It is ledgered
with a trigger.

Nothing this plan reports is wrong because of it, which is why it can wait. The in-process spend
block reads `Guard.Spent()`, whose token count is correct; `report`'s entries carry no token field
at all; and `AC1`'s test compares calls and cost exactly while logging — loudly — if the token
divergence ever changes shape.

**What must NOT happen: spend must not move onto `knov1.Run`.** `Run` carries no settled-spend
field today (verified: `run.proto`'s only micro-USD fields are on `ConcurrencyDecision`), and
`BaselineResult.Spent`'s godoc (`core/baseline.go:318-323`) records the reason —
*"Carried here rather than on the Run because it is the guard's number, not the schema's."*
Adding it would create a second source that can disagree with `SettledSpend`, which is the
failure `docs/debt.md#50` already cost this project once.

### 3 — The shape: one renderer, one JSON fragment, one set of keys

**Human.** One shared helper in `cli/render.go`, producing byte-for-byte the line Baseline
prints today so the existing golden is untouched:

```go
func spendLines(w io.Writer, s budget.Spend, usageEstimatedCases int32, resumed bool) error
```

```
  spent      $2.41 over 8,250 call(s)
  note       412 case(s) priced from the engine's estimate rather than reported usage
  note       resumed run — this figure covers earlier sessions of the same run, not just this one
```

The second line appears only when `usage_estimated_case_count > 0`, closing the hole in §Problem:
the qualifier the schema already records finally reaches a human.

**The third line is the review amendment** *(F3)*. §6 argues that a resumed run must report
run-lifetime spend, and its own justification is that *"the fourth [resumed run] is the one a CI
log shows"* — but a CI log shows the **human** rendering, and the first draft put the `resumed`
marker only in `--json`. A caveat that appears in the surface nobody reads and not in the surface
everybody reads is not a caveat. Both renderings carry it — the `note` line here and the
`resumed` key on `spendReport` below, which is a new key rather than an existing one (the repo
has no `resumed` JSON key today; only the resume *path* exists) — and the equivalence golden
(§7 rule 6) pins them together like every other spend content.

**JSON.** One embedded struct, so the keys are identical across stages and Baseline's existing
top-level `spent_usd` key is preserved exactly (Go flattens anonymous embedded structs):

```go
// cli/jsonreport.go
type spendReport struct {
    Guarded        bool   `json:"guarded"`
    SpentUSD       string `json:"spent_usd"`
    SpentUSDMicros int64  `json:"spent_usd_micros"`
    LLMCalls       int64  `json:"llm_calls"`
    Tokens         int64  `json:"tokens,omitempty"`
    UsageEstimatedCases int32 `json:"usage_estimated_cases,omitempty"`
    Resumed        bool   `json:"resumed,omitempty"`
}
```

Four decisions inside that struct, the last added in review:

- **`spent_usd` stays a formatted string.** It is the released key, shipped since v0.1.0 and
  sampled as `"spent_usd": "$0.00"` in the CI-gate recipe — which now lives in `kno-examples` and
  is therefore *outside this repository's ability to update in lockstep*, which makes the key more
  frozen than it was, not less. Changing its type would break every `jq` pipeline written against
  v0.1 for a cosmetic gain, and would break them from a repository the pipeline's author is not
  watching.
- **`spent_usd_micros` is added because the string cannot be summed.** `jq` cannot add `"$2.41"`
  and `"$0.35"`, and the repo's own money discipline is integer micro-USD end to end
  (`stats/budget/budget.go:725-727` warns that `formatUSD` is "for error messages only. Never use
  it for arithmetic"). Emitting only the display form pushes CI authors into exactly the float
  parsing the engine refuses to do. The string is for eyes; the micros are for arithmetic, and
  the docs say which is which.
- **`llm_calls` closes the human/JSON asymmetry.** The human line has printed the call count
  since v0.1; the JSON has not.
- **`guarded` makes §4's absence rule self-describing** *(F1)*. §4 decides that a stage which ran
  no budget guard emits **no** spend keys rather than a zero, and that decision stands — but bare
  absence was shipped unargued, and two things are wrong with it on its own. First, the claimed
  loud failure is quieter than stated: `jq`'s `.spent_usd` on a document without the key returns
  `null`, not an error, indistinguishable from an explicit null, so a pipeline degrades rather
  than breaks. Second, and worse, the natural repair a consumer reaches for is
  `.spent_usd // 0` — which **resurrects the metered-zero-versus-unmetered ambiguity this entire
  plan exists to kill**, relocated from the producer to the consumer, where neither the docs nor
  the goldens can see it. Absence carries the right fact and carries it silently; a reader who has
  not consulted `--help` has nothing in the document to notice. `guarded` is that signal:
  `kno value --json` says `"guarded": true` and carries every spend key, `kno select --json` says
  `"guarded": false` and carries none, and the CI idiom the cookbook teaches becomes
  `select(.guarded) | .spent_usd_micros` — never `// 0`.

  `guarded` is a fact about the **stage**, not about the run: `true` for `baseline`, `value` and
  `validate` on every agent including `fake:`; `false` for `select`, `export`, `report` and (when
  it grows `--json`) `mine`. Because it is constant per command it is cheap to document, safe to
  pin in a golden, and impossible to get wrong at runtime. On spending stages it rides in the
  embedded `spendReport` and flattens to the top level; on non-spending stages it is a plain
  bool field tagged `json:"guarded"` on that stage's own report struct, since there is no spend
  block for it to live in. It does **not** replace the absence rule — it explains it, which
  is the half §4 was missing.

**Existing consumers are unaffected.** Every change is an added key. `cli/cli_test.go:353-365`'s
key-presence assertion still passes, and the added keys join it.

### 4 — Stages that genuinely cannot spend: absent, not `$0.00`

**Decision: the presence of `spent_usd` tracks whether the stage ran a budget guard — not whether
money moved — and a top-level `guarded` boolean states that fact in the document rather than
leaving a consumer to infer it from a missing key** *(F1, §3)*. Concretely:

- `baseline`, `value`, and (per
  [`2026-08-31-validate.md`](2026-08-31-validate.md)) `validate` **always** emit the spend block,
  including under `fake:`, where it legitimately reads `$0.00`.
- `select`, `export`, `mine` **never** emit it.
- `report` emits a differently-shaped, differently-named block (§5).

**Why not `spent_usd: "$0.00"` everywhere.** A uniform zero is a true statement that a machine
consumer will read as *"this stage was metered and cost nothing"* — which is indistinguishable
from *"this stage spent money and the meter is missing"*. That confusion is the entire bug this
plan exists to fix; shipping it as the fix for four more stages would be self-defeating. And the
counterfactual is concrete: had `kno value` emitted `spent_usd: "$0.00"` in v0.1, nobody would
have noticed the hole, because the field would have been present and plausible on every `fake:`
run in CI.

**Why `$0.00` is nonetheless right for `fake:` on a spending stage.** The guard ran, authorized,
and settled — zero is a *measurement*. This is not a new convention: `cli/demo_test.go:245`
already asserts `base.SpentUSD == "$0.00"` for the fake-agent demo, and `docs/status.json`
already declares a per-adapter `spends` boolean (`fake` and `exec` are `false`, the four provider
schemes `true`). **That existing assertion covers the dollars only** — there is no `llm_calls`
assertion in the tree today, because there is no `llm_calls` key; pairing a zero cost with a
non-zero call count is this plan's AC16, proposed behavior rather than precedent, and the earlier
draft of this paragraph implied otherwise. The distinction "the meter ran and read zero" vs
"there is no meter" is one the repo already draws; this plan makes it legible in the output, in
three ways that agree: `guarded: true`, a non-zero `llm_calls`, and a `$0.00` figure.

**Documented, not merely omitted, and now also self-describing** *(F1)*. Absence is only honest
if it is stated, and `--help` states it to humans while `guarded: false` states it to machines.
`kno select --help` and the ADR both say: *select makes no LLM calls and therefore reports no
spend; the measurement it ranks was paid for by the Value run named in `source_run_id`.*

### 5 — `kno report`: the pipeline total

`report` reads recorded aggregates only, and it is the only surface holding every run ID at once
— not all of them as flags (`reportFlags` is value/select/export; the Baseline arrives through
`valueRun.GetBaselineRunId()`), but all of them in `reportData` by the time either renderer runs.
It gains a `spend` object — **never a bare top-level `spent_usd`**, which would claim `report`
spent it:

```json
"spend": {
  "baseline": { "run_id": "…", "spent_usd": "$0.31", "spent_usd_micros": 310000, "llm_calls": 400 },
  "value":    { "run_id": "…", "spent_usd": "$23.80", "spent_usd_micros": 23800000, "llm_calls": 12480 },
  "validate": { "run_id": "…", "spent_usd": "$0.12", "spent_usd_micros": 120000, "llm_calls": 68 },
  "total_usd": "$24.23", "total_usd_micros": 24230000, "total_llm_calls": 12948,
  "incomplete": false
}
```

The `report` document itself carries `"guarded": false` at the top level *(F1)* — `report` ran no
guard and spent nothing; the money in its `spend` object belongs to the runs it names, and every
entry inside that object is by construction a run that did run a guard. Each entry is
`Store.SettledSpend` for that run. `select` and `export` are **absent** from the object rather
than present at zero, for §4's reason. `incomplete` is true when any referenced run
has a non-empty `Run.incomplete_reason`, so a total assembled from a budget-stopped Value run
cannot pass for a complete cost. The human rendering gets the same block as a small table under
a `## Cost` heading, pinned equal to the JSON by the equivalence golden the report already uses.

This is the answer to the question the tool currently cannot answer: **"what did knowing this
cost me?"** — which is, verbatim, the ROI frame `DESIGN.md`'s success criterion 2 is built on.

### 6 — Resume: which number, and is it honest?

**Verified mechanism.** `Guard.Restore` (`stats/budget/budget.go:529-537`) is **additive** across
all three dimensions via `addSpend`, and both spending stages seed it from the durable sum before
authorizing anything — `core/baseline.go:381-387` (inside `if opts.Resume`) and
`core/value_loop.go:442-446` (inside the `if !o.Resume { ... }` early-return's else branch at
`core/value_loop.go:380`, so it is resume-only in both stages; verified, the two paths are
symmetric).

**Therefore `Guard.Spent()` after a resumed run is prior sessions + this session = run-lifetime
spend, and that is the number reported.**

**Is that honest? Yes, and it is the only honest one.** The unit the user authorized is the
*run*: the consent dialog quotes against `Limits` that bound the run, `--max-cost-usd` caps the
run, and `docs/debt.md#32` characterizes the cap as a soft bound on the run. Reporting a session
figure would mean a run killed and resumed four times prints four small numbers that sum to the
real cost and individually understate it — and the fourth is the one a CI log shows. The run is
the accounting unit everywhere else in the system; spend does not get its own.

Two consequences stated in the docs rather than hidden:

- A resumed run's spend line covers work this process did not do. **Both** renderings say so
  *(F3)*: `--json` carries `resumed: true` (from the existing resume path), and the human output
  carries the matching `note` line from §3. Putting the marker only in `--json` would have
  contradicted this section's own argument, which rests on what a CI log shows — and a CI log
  shows the human rendering.
- `SettledSpend` cannot see `docs/debt.md#20`'s dark-spend window, and every figure is
  *"reported usage at rates as published on `<date>`"*, not an invoice
  (`docs/what-the-numbers-mean.md:181-189`). The renderer's docs point there; the renderer does
  not restate it per line.

### 7 — The jq contract, and where the promise actually lives

**The premise needs correcting, and the correction is the deliverable.** ADR-0001 is
*"Generated proto messages are the domain types"* and says **nothing** about `--json` stability.
Its money content is a *serialization-correctness* rule — that `cost_usd_micros` marshals as
`"1500000"` under protojson and `1500000` under `encoding/json`, so `depguard` bans
`encoding/json` outside `api/`. Useful, but not a stability promise. **No ADR covers `--json`
output stability.**

The only stability promise that ever existed was prose in a cookbook page, and **it is gone**
*(Phase-2 amendment)*. Until #163 it read, at `docs/cookbook/ci-gate.md:35`:

> `--json` emits a stable, hand-written shape aimed at `jq` — not the internal schema, so it
> won't shift under you when the proto gains a field

That page is now a one-line tombstone; the sentence survives only at the `v0.1.3` tag and in the
`kno-examples` copy, which this repository cannot edit and CI here does not read.
`grep -rn "stable, hand-written"` over the tree matches this plan file and nothing else.

**This changes the argument for ADR-0006, in the direction of writing it.** The first draft
justified the ADR as codification: the promise existed but lived in the wrong genre of document,
and moving it to a decision record would make it citable. That is no longer the situation. The
promise exists **nowhere in this repository**, while the behavior it described — hand-written
structs, additive keys, a released `spent_usd` — is unchanged and is what every v0.1 `jq` pipeline
is still running against. A contract that a codebase honors and never states is one the next
contributor breaks in good faith, and this PR is about to add seven keys to it. ADR-0006 is
therefore not a tidy-up: **it is the only statement of the `--json` contract this repository will
have**, and the migration is the reason rather than an obstacle. It is also the reason the ADR's
home matters (AC17): a decision record nothing links to is one nobody finds.

**Decision: write `docs/adr/0006-the-json-contract.md`** in this PR, codifying what the code
already does and what this plan extends:

1. `--json` shapes are **hand-written structs** in `cli/jsonreport.go` — the single file holding
   the `encoding/json` exemption, scoped by filename — never `protojson` over a `kno.v1` type.
   The reason is the one that file's header already gives: a contract aimed at a `jq` pipeline
   must not mirror proto field names or shift when the schema gains a field.
2. Keys may be **added**. Pre-1.0, a key may be renamed or removed only with a CHANGELOG
   migration note; post-1.0 it is a covenant requiring a major, alongside exit codes and the
   proto (`CLAUDE.md`, *SemVer with teeth*).
3. Enum-valued keys carry **names, not numbers** (`docs/debt.md#44`).
4. **Money appears twice**: a display string (`*_usd`) and an integer micro-USD field
   (`*_usd_micros`). Arithmetic uses the latter.
5. **Absence is meaningful, documented per key, and paired with a self-describing signal
   wherever a consumer could read it as a zero** *(F1)*. A field is omitted when its absence is a
   fact about the stage (§4), and never merely because a value happened to be zero — the
   `optional`-presence discipline `report.proto:64-88` already applies to the schema, applied to
   the CLI contract. Where the omission is load-bearing, the document also states the fact
   positively: `guarded` is the first such signal, and the rule is that a future omission of the
   same kind ships with one too.
6. Human and `--json` renderings of one stage are **pinned to identical content** by an
   equivalence golden.

**What the ADR does and does not do** *(F4)*. ADR-0006 records a decision; it does not enforce
one. Nothing in `make check` reads a markdown file, and a rule that lives only in `docs/adr/` is
obeyed exactly as long as the next contributor happens to have read it. **The enforcement is three
mechanical artifacts, and they are the deliverable the rules describe**: the per-stage JSON
goldens (rule 2 — a renamed or removed key shows up as a golden diff reviewed like code), the AST
test that confines spend-field reads to one file (rule 4's single formatter — AC6, whose
mechanism the Phase-2 amendment replaced), and the explicit absence assertion
`TestSelectExportReportEmitNoSpentUSD` (rules 4–5 — AC8). The ADR's value is that a reviewer
rejecting a future PR has something to cite; its value is **not** that the PR would have been
caught without those three tests. Any rule added to it later that no test enforces should be
labelled as guidance in the ADR itself rather than left to read as a constraint.

Rule 6 needs backing that does not exist yet: **there are no JSON golden files anywhere in this
repo.** The only two `.golden` files are `cli/testdata/demo_transcript.golden` (a human
transcript) and `adapters/evals/hf/testdata/golden/single-winner.golden`. The `--json` contract
is currently guarded by one key-presence loop at `cli/cli_test.go:353`. This plan adds a JSON
golden per stage under `cli/testdata/json/`, regenerated by the existing
`make update-golden` (`Makefile:276-277`) and reviewed like code — which is what makes rule 2
enforceable rather than aspirational. Two honest consequences, stated rather than glossed
*(Phase-2 amendment)*: this PR **creates** the baseline these goldens compare against, so on this
PR the comparison is against itself and catches nothing (AC18); and the thing that does have
historical force on this PR is the frozen v0.1 shape fixture (AC4), cut from the `v0.1.0` tag's
struct tags rather than from anything this PR wrote.

### 8 — Is this a prerequisite for `--bridge`? Yes, and here is the sequencing argument

v0.2 ships `Tuner` + the proxy-FT bridge behind `--bridge` (`DESIGN.md:398`). Three verified
facts make the ordering non-negotiable:

1. **The bridge is a new spend path with a different unit and a different magnitude.**
   `DESIGN.md:139` sizes it at *~7 LoRA runs x $3–8 ≈ $30–80 per bridge confirmation* —
   *"comparable to the valuation run itself"*. Its money is not per-call: `TuningJob` carries
   `estimated_cost_usd_micros` (`tuner.proto:73`) and `JobState` carries
   `actual_cost_usd_micros` (`tuner.proto:110`), settled minutes-to-hours after submission via
   `Tuner.Submit` / `Tuner.Status` (`core/ring0.go:209-219`).
2. **The schema already carries those numbers and there is no surface to report them into.**
   `tuner.proto:108-110` says `actual_cost_usd_micros` exists to be *"compared against
   `TuningJob.estimated_cost_usd_micros` to keep estimates honest over time"* — a comparison
   nothing can currently render, because the stage the bridge extends (`value`) reports no money
   at all.
3. **Landing `--bridge` first means retrofitting three stages instead of extending one
   contract.** The bridge's spend has to reach the same block, the same keys, the same ADR rule
   4 (string + micros), and the same total in `kno report`. Building that surface once, for the
   stage that already spends the most, and then plugging the bridge into it, is strictly less
   work and strictly less risk than the reverse.

There is also a directive argument, and it is the stronger one. Prime directive 4 — *never spend
the user's money silently* — is not discharged by the consent prompt alone. A user asked to
authorize a $30–80 bridge run has no basis for the decision if the $15–40 valuation run that
preceded it reported nothing. **A confirm-before is only half of consent; a report-after is the
other half.** Shipping a second, larger, slower spend path on top of a stage with no meter is the
worst version of that gap, not a neutral one.

**Review sequencing inside the PR** *(F2)*. Review was right that this is a wide diff for one
PR: every stage, a new ADR, the repo's first JSON goldens, and `report`'s pipeline total. The
narrow alternative — fix `kno value` only — is rejected below, and that rejection stands:
without the shared renderer, `validate` and `--bridge` each invent a private spend rendering inside one
milestone. The scope is accepted, and the mitigation is ordering rather than splitting. The diff
lands as **two internally sequenced halves**, in this order, so a reviewer can finish the first
before the second exists: **(1) the fix and the contract** — `ValueResult.Spent`, `spendLines`,
`spendReport`, `guarded`, and ADR-0006; **(2) the surface and its pins** — `report`'s `spend`
object and the per-stage JSON goldens, which are only meaningful once (1) has settled what a
spend block contains. Half (2) is where a golden churn would otherwise force half (1) to be
re-read. Landing as one squashed PR per CLAUDE.md, reviewed in two passes.

**Sequencing: this plan lands first in v0.2, before both `--bridge` and
[`2026-08-31-validate.md`](2026-08-31-validate.md)'s CLI work.** Validate consumes the shared
renderer rather than adding a fourth private one, and the bridge extends the block rather than
inventing a fifth.

## Acceptance criteria

1. `core.ValueResult` gains `Spent budget.Spend`, populated at close from `o.Guard.Spent()` —
   and on **every** path that returns a non-nil result, error paths included (AC21). A test
   asserts it equals `Store.SettledSpend(ctx, runID)` for the same run **on calls and cost, to
   the micro-dollar**, for a completed run, a run that failed after settling, and a resumed run.

   **Scoped to two of the three dimensions by the Phase-2 amendment**, and the scoping is a
   finding rather than a weakening: the Value sink does not persist token counts (see §2), so a
   three-dimension equality assertion fails on `main`'s recording behavior rather than on
   anything this plan changes. The test compares calls and cost exactly and logs if the token
   divergence changes shape, so the day `core` starts recording tokens the allowance is removed
   deliberately rather than forgotten. The orphan-spend fixture folds into AC15, which is where
   `docs/debt.md#50`'s path already lives.
2. `kno value` human output contains the line `  spent      $X.XX over N call(s)` with the same
   spacing, ordering and pluralization as `kno baseline`'s — pinned by a golden that diffs the
   two stages' spend blocks and fails on any divergence.
3. `kno value --json` contains `guarded: true`, `spent_usd`, `spent_usd_micros` and `llm_calls`.
   A test runs a fake agent priced at a known non-zero rate and asserts
   `spent_usd_micros == calls * rate` exactly, and `spent_usd == formatUSD(spent_usd_micros)`.
4. **Rewritten by the Phase-2 amendment.** `kno baseline --json` gains `spent_usd_micros` and
   `llm_calls` and its `spent_usd` value is **byte-identical** to v0.1 for the same run. The
   original criterion parsed the fenced sample in `docs/cookbook/ci-gate.md`; that page is a
   one-line tombstone and has no fenced sample, so the check is not writable and — worse — would
   have been a check against a file in another repository that this one's CI cannot see change.
   Instead this PR commits **`cli/testdata/json/v0.1-shape.json`**: a frozen, in-repo capture of
   the v0.1 `--json` shape for all five stage documents, one entry per key that v0.1.0 emitted
   unconditionally, recording its JSON type (`string`, `number`, `boolean`, `array`, `object`,
   or `number|null` for `score`). It is derived mechanically from the `v0.1.0` tag's struct tags
   in `cli/jsonreport.go` (`jsonReport`, `valueReport`, `selectReport`, `exportReport`,
   `reportJSON`) and it is **frozen**: a PR that edits it is renaming or removing a released key
   and owes a CHANGELOG migration note per ADR-0006 rule 2. `TestV01ShapeIsStillASubset` renders
   every stage document and asserts each frozen key is still present with the same JSON type.
   This is strictly stronger than the criterion it replaces — five shapes rather than one, no
   cross-repository dependency, and it fails on a *retype* as well as a removal.
5. Human and `--json` spend content are equal for `baseline` and `value`: an equivalence golden
   asserts the dollars and call count rendered by each surface match.
6. **Rewritten by the Phase-2 amendment.** `spendLines` is the **only** place a spend line is
   formatted. The original mechanism — an AST allowlist of `formatUSD`'s callers — was wrong on
   the facts and wrong in kind. On the facts: `formatUSD` has nine non-test call sites in `cli/`
   across six functions (`printEstimate`, `renderHuman`, `quoteEstimate` ×2, `renderJSON` ×2,
   `renderSelectJSON` ×2, `budgetCaps`), not the three named, so the test fails on `main` before
   it tests anything. In kind: `formatUSD` renders estimates, cost caps and carrying costs as
   well as spend, so an allowlist over its callers polices the wrong noun and would fail the next
   legitimate cap rendering — a test that has to be edited to stay green is a test that will be
   edited to stay green.

   The replacement enumerates no callers. The invariant is **spend fields are read in exactly one
   file**: `spendLines` and `newSpendReport` move to a new `cli/spend.go`, and
   `TestSpendFieldsAreReadInOneFile` walks the AST of every non-test file in `cli/` and fails on
   any selector chain that reaches *into* a `budget.Spend` — a `SelectorExpr` whose own `X` is a
   `SelectorExpr` named `Spent` (`res.Spent.CostUSDMicros`, `d.ValueSpend.Calls`) — outside
   `spend.go`. Passing `res.Spent` whole to a renderer is one selector and stays legal anywhere;
   formatting its contents is confined to one file. Purely syntactic, needs no type information,
   names no caller, and fails on exactly the thing the rule is about: a second private spend
   formatter. A companion assertion pins the spend line's format literal to `spendLines`.
7. When `CaseExecution.usage_estimated_case_count > 0`, both renderings carry the qualifier
   (`usage_estimated_cases` in JSON, the `note` line in human output). When it is zero, neither
   does — `omitempty` on the key, no line in human output.
8. `kno select --json`, `kno export --json` and `kno report --json` contain **no** `spent_usd`
   key **and do contain `"guarded": false`** *(F1)*. A test asserts both — the absence, so a
   future refactor that "helpfully" adds a zero fails, and the presence of the negative signal, so
   the absence is never left to be inferred. `baseline`, `value` and `validate` carry
   `"guarded": true` on every agent scheme including `fake:`; a table test walks all commands and
   asserts the boolean matches the stage's guard, not the run's dollars.
9. `kno select --json` retains `max_cost_usd` and `acquisition_usd` with unchanged meanings, and
   `kno select --help` states that select makes no LLM calls and reports no spend, naming
   `source_run_id` as where the measurement's cost lives. Snapshot-tested.
10. `kno report --json` contains a `spend` object with one entry per referenced run that ran a
    guard, `total_usd`, `total_usd_micros`, `total_llm_calls`, and `incomplete`. Entries for
    `select` and `export` are absent.
11. `report`'s `spend.total_usd_micros` equals the sum of its entries' `spent_usd_micros`
    exactly, and each entry equals `Store.SettledSpend` for that run ID.
12. `report`'s `spend.incomplete` is `true` when any referenced run has a non-empty
    `Run.incomplete_reason`, and the human `## Cost` table carries a visible marker in that case.
13. **Rewritten by the Phase-2 amendment: the original state is unreachable.** The criterion
    described `kno report` given only `--select-run-id`. That invocation does not exist:
    `cli/report.go:74` marks `--value-run-id` REQUIRED via `MarkFlagRequired`, and `loadBaseline`
    (`cli/report.go:335`) refuses an empty baseline ID before the page composes. Every `kno
    report` page therefore names exactly one Value run and one Baseline, and both are stages that
    run a guard — there is no path to "zero metered runs referenced", and a test for it would
    have been unwritable or, worse, written against a stubbed `reportData` that no CLI invocation
    produces.

    The reachable state carrying the same risk is **every referenced metered run settled zero**,
    which is exactly what a `fake:` pipeline in CI produces on every run — including `kno demo`.
    A machine consumer reading `total_usd_micros: 0` there must be able to tell "these runs were
    metered and cost nothing" from "no meter", which is §4's distinction one level up. So: a
    `report` over a `fake:` baseline and value run emits the `spend` object with **both entries
    present**, each at zero with a non-zero `llm_calls`, `total_usd_micros: 0`, and
    `no_metered_spend: true`; the human `## Cost` table carries the matching sentence. The block
    is never omitted and the entries are never dropped for being zero — dropping them would
    reproduce, inside `report`, the ambiguity the whole plan exists to remove.
14. **Resume reports run-lifetime spend, and says so in both renderings** *(F3)*. A test kills a
    `value` run mid-flight, resumes it, and asserts the resumed run's reported `spent_usd_micros`
    equals first-process + second-process settled spend, that no Case is paid for twice, that
    `resumed: true` appears in `--json`, **and** that the human output carries the resumed-run
    `note` line from §3. A non-resumed run carries neither — asserted, so the marker cannot
    become unconditional.
15. A `value` run stopped by its cost cap reports the spend it incurred before stopping, exits
    `errs.ExitBudgetStopped` (2), and its reported figure equals `SettledSpend` — asserted with
    orphan spend present, so `docs/debt.md#50`'s path is covered.
16. `kno value --agent fake:...` emits `guarded: true`, `spent_usd: "$0.00"`,
    `spent_usd_micros: 0`, `llm_calls: <the real call count, non-zero>`. The call count being
    non-zero while dollars are zero is the assertion — it is what distinguishes "metered, free"
    from "unmetered", and `guarded` states the same thing without requiring the consumer to reason
    about it. **This pairing is new behavior, not a pin of existing behavior**: today
    `cli/demo_test.go:245` asserts the `$0.00` alone, because `llm_calls` does not yet exist.
17. **Rewritten by the Phase-2 amendment: the link target moved.** `docs/adr/0006-the-json-contract.md`
    exists and states rules 1–6 of §7. It is linked from **README's Documentation list** and
    cross-linked from `docs/what-the-numbers-mean.md`.

    Why not the original targets. `docs/cookbook/ci-gate.md` is an 80-byte tombstone, and
    `scripts/cookbook-stub-check.sh` — inside `make docs` — fails any stub of more than one line;
    linking a decision record from a redirect stub is also wrong on its own terms, since the stub
    exists to send the reader somewhere else. `CONTRIBUTING.md` carries no ADR index at all
    (`grep -in adr CONTRIBUTING.md` returns nothing), so a lone ADR-0006 link there would be a
    link with no list to join, and the four existing ADRs would remain unlinked beside it.

    Why README's Documentation list. It is this repository's only curated documentation index —
    mental model, what-the-numbers-mean, evaluation design, cookbook, DESIGN, CONTRIBUTING, debt
    ledger — and it is where a reader who wants to know what a surface promises already looks.
    The `--json` contract is a **user-facing promise**, not an internal decision: it belongs
    beside "what each number claims" and not in a contributor workflow document. The cross-link
    from `docs/what-the-numbers-mean.md` is the second half, because the spend figure is the
    number this PR adds and that page is where its meaning is stated.

    **Out of scope for this PR:** updating the `kno-examples` CI-gate recipe to cite ADR-0006 and
    to teach `select(.guarded) | .spent_usd_micros` instead of `// 0`. That edit lands in
    `uknoAI/kno-examples`, which is a different repository with its own CI, and a PR here cannot
    make it atomic. It is recorded as a Debt Ledger row with a trigger rather than left as an
    intention — which is the whole point of the ledger.
18. A JSON golden exists under `cli/testdata/json/` for `baseline`, `value`, `select`, `export`
    and `report`, regenerated by `make update-golden` and reviewed like code — the mechanical
    form of ADR-0006 rule 2: a renamed or removed key shows up as a golden diff.

    **Honest scope on this PR** *(Phase-2 amendment)*. The original criterion asked each golden to
    be a key-for-key superset of "the previous release's shape". There is no such artifact: this
    PR creates the repository's first JSON goldens, so the comparison would be against itself and
    would catch nothing. On this PR AC18 **establishes** the baseline and its value is the review
    diff, not an assertion. The historical check that does bite today is AC4's frozen v0.1 shape
    fixture, cut from the `v0.1.0` tag rather than from this PR's output. AC18's superset check
    becomes load-bearing at the next release, when `v0.1-shape.json` gains a `v0.2-shape.json`
    sibling and the goldens have a predecessor to be a superset of.
19. No spend rendering path emits `Case.input`, `Case.expected` or any `Turn.content`, and no
    spend log line above DEBUG carries trace content. Pinned by the existing sentinel test,
    extended to the new surfaces.
20. `make check` is green with no coverage ratchet decrease, and `make bench-diff` shows no
    regression on the scoring loop (the change is render-path only).
21. **A stage that spends and then fails for a non-budget reason still accounts for what it
    spent** *(F5)*. AC15 covers the budget-stopped path and AC13 the never-spent path; neither
    covers the case that loses money most confusingly — real charges settled, then a failure that
    has nothing to do with money. A `value` run whose `Store` returns an error after N
    measurements have settled: the command exits non-zero with the wrapped store error, and
    `Store.SettledSpend` for that run still equals the N settled charges, asserted directly. For
    any *returned* error after the guard exists, `core.Value` returns a non-nil `*ValueResult`
    with `Spent` populated — the same discipline the budget-stop path already uses — and the CLI
    renders the spend block **before** printing the error, so the user sees the cost of a failed
    run in the same output; the test asserts the block, the error text, and that the exit code is
    the error's rather than 0. For a **panic**, recovered at the CLI top level with the
    bug-report template, no block is rendered; the documented recovery is
    `kno report --value-run-id <id>`, whose `spend` object reads the durable record, and a test
    drives a panicking fake agent and asserts the money is recoverable that way. The asymmetry is
    stated in `kno value --help`, not papered over.

## Alternatives considered

**Bare absence with no positive signal** (this plan's own first draft). Rejected in review
*(F1)*: `jq` returns `null` for a missing key exactly as for an explicit null, so absence is not
the loud failure the draft claimed; and the repair a consumer reaches for, `.spent_usd // 0`,
reintroduces the metered-zero ambiguity at the consumer. The alternative of leaving it unargued
was the specific objection. `guarded` costs one boolean per document and makes the fact readable
without `--help`. The absence rule itself is unchanged.

**Emit `spent_usd: "$0.00"` from every stage, uniformly.** The simplest contract: one key,
always present, never a special case, and a `jq` pipeline never has to branch. Rejected: a
present-and-plausible zero is exactly how this bug survived v0.1. A machine consumer cannot
distinguish "metered, cost nothing" from "no meter", and the second is the case that loses money.
§4 draws the line at "did a guard run", which is a fact about the stage that the code can answer
and the docs can state.

**Put settled spend on `knov1.Run` and let every surface read it from the record.** Superficially
attractive — one source, automatically available to `report`, `serve` and the future SDK.
Rejected: `BaselineResult.Spent`'s godoc explicitly declines it (*"the guard's number, not the
schema's"*), and a `Run.spent_usd_micros` written at close would be a **second** source that can
disagree with `SettledSpend` after an orphan-spend write or a crash between the last settle and
`FinishRun`. `docs/debt.md#50` is the record of what a disagreement between the guard and the
store costs. `SettledSpend` is already the single durable source and needs no rival.

**Change `spent_usd` to a number (micro-USD or float dollars).** Cleaner for `jq`. Rejected: it
breaks the one released spend key, sampled in the CI-gate recipe that now lives in a repository
this one cannot update in the same commit *(Phase-2 amendment)* — which makes the break worse,
not more tolerable, because the sample would keep teaching the old type; adding
`spent_usd_micros` gets the arithmetic without the break, and floats-for-money is banned by the
repo's own micro-USD discipline.

**Report per-session rather than run-lifetime spend on a resume.** Rejected in §6: the run is the
authorization unit, and a session figure understates the cost in exactly the log a CI job shows.

**Ship `--bridge` first and retrofit spend reporting afterwards.** Rejected in §8: more work,
more risk, and it puts the product's largest spend path behind the smallest meter.

**Fix only `kno value` and leave the rest.** The minimal patch for the reported bug. Rejected:
without the shared renderer and the ADR, `validate` and `--bridge` each add a fourth and fifth
private spend rendering within one milestone, and the divergence this plan exists to remove
returns immediately.

## Affected packages

| Package | Change |
|---|---|
| `core/` | `ValueResult.Spent` + its population at close (mirroring `core/baseline_close.go:142`). Nothing else — the guard, the store and the invoker are unchanged |
| `stats/budget` | none. `Guard.Spent`, `Restore`, `Spend` are used as-is |
| `store/` | none. `SettledSpend` already sums all three sources |
| `proto/` | **none.** Verified: no schema change is required, and §2 argues one must not be made |
| `cli/` | `render.go` (`spendLines`, now taking `resumed` *(F3)*), `jsonreport.go` (`spendReport` embedded into `jsonReport` and `valueReport`; a plain `guarded: false` field on the select/export/report structs *(F1)*; `spend` object on `reportJSON`), `value_render.go`, `report_render.go`, `select.go` help text, new `cli/testdata/json/` goldens |
| `docs/` | new `adr/0006-the-json-contract.md`; `cookbook/ci-gate.md` (link the ADR, add the value-stage example); `what-the-numbers-mean.md` (one line: reported spend is run-lifetime and is not an invoice); CHANGELOG |

No proto change means no `buf` sequencing constraint and no workstream is blocked on schema.
`core` and `cli` are the only two, and `core`'s change is a struct field.

## Proto / schema impact

**None.** Verified against `proto/`:

- `Run` has no settled-spend field and gains none (§2). Its only micro-USD fields are
  `ConcurrencyDecision.headroom_usd_micros` and `.per_case_estimate_usd_micros`
  (`run.proto:308`, `:318`), both unrelated.
- `CaseExecution.usage_estimated_case_count` (`run.proto:404`) already exists and is already
  written; this plan only renders it.
- `Report.total_cost_usd_micros` (`report.proto:104`) and `.total_llm_calls` (`:107`) already
  exist. `knov1.Report` is constructed nowhere outside `gen/`, so this plan does not populate it
  — the `report` command composes hand-written JSON, which is ADR-0006 rule 1. That `Report` has
  no producer is pre-existing and is ledgered by
  [`2026-08-31-validate.md`](2026-08-31-validate.md), not by this plan.
- `TuningJob.estimated_cost_usd_micros` and `JobState.actual_cost_usd_micros`
  (`tuner.proto:73`, `:110`) already exist and are what `--bridge` will render into this plan's
  block. No addition needed then either.

`buf breaking` has nothing to compare — there is no proto diff. `make typecheck-proto` passes
unchanged. **The store schema is also unchanged**: no migration.

## Edge cases

| Case | Behavior |
|---|---|
| `fake:` agent on `baseline`/`value` | Block present, `spent_usd: "$0.00"`, `llm_calls` non-zero. The meter ran |
| `exec:` agent with no `--cost-per-call-usd` | Same as `fake:` — `docs/status.json` declares `exec` as `spends: false`, and the guard still counts calls |
| `exec:` agent **with** `--cost-per-call-usd` | Real dollars; nothing special |
| Adapter that does not report token usage | `tokens` omitted; `usage_estimated_cases` non-zero; the qualifier line appears |
| `select`, `export`, `mine` | No spend key, `"guarded": false`, no spend line, help text says why |
| Stage settles real spend, then fails for a non-budget reason (store error) | Non-zero exit with the wrapped error; spend block rendered **before** the error; `SettledSpend` intact *(F5)* |
| Stage settles real spend, then panics | Top-level recovery prints the bug-report template; no spend block. Money is recoverable via `kno report --value-run-id`, which the help text names *(F5)* |
| `report` with no metered run referenced | `spend` object present, empty entries, zero total, explicit "no metered run referenced" line. Never omitted |
| `report` over a budget-stopped Value run | Total rendered with `incomplete: true` and a visible marker; the number is a floor, not a total, and says so |
| `report` over a purged run (`kno purge`) | Unaffected: purge NULLs `response_proto`/`score_proto` and never touches `calls`/`cost_usd_micros`, so `SettledSpend` is intact. A test pins this |
| Resumed run | Run-lifetime figure, `resumed: true` in JSON |
| Run killed after a provider charge but before persistence | Under-counts by up to `concurrency` calls' worth — `docs/debt.md#20`, inherited and unfixable here |
| Cost cap exceeded via settlement overshoot | `Guard.Spent()` includes the overshoot (it is settled spend); `Guard.Overshoot()` is **not** rendered by this plan — the reported figure is what was spent, not what was over |
| Saturated / negative charge from a broken adapter | `addSpend` clamps at `Settle` time (`docs/debt.md#48`, repaid); the renderer receives an already-clamped number and adds no second clamp |
| Spend exceeding `$999,999.99` | `formatUSD` is integer arithmetic on `int64` micro-USD and does not overflow below ~$9.2e12; no special case |
| `--json` with a run that errored before any call | Block present with all zeros — the guard existed and settled nothing. Distinguished from an absent block |
| `demo` | Each embedded stage doc carries its own block; the demo's own summary gains the pipeline total, matching `report`'s shape |

## Test plan — what fails if this regresses

- **The motivating bug.** `TestValueReportsSpend` (human) and `TestValueJSONCarriesSpend`
  (`--json`) — *both fail today, and both fail again the moment `ValueResult.Spent` stops being
  populated.* This is the test the fix ships with, per CLAUDE.md's "no test, no fix".
- **Cross-stage identity.** `TestSpendBlockIsIdenticalAcrossStages` — diffs `baseline`'s and
  `value`'s rendered spend blocks. *Fails if anyone writes a second formatter.*
  `TestSpendFieldsAreReadInOneFile` (AST, and enumerating no callers — see AC6).
  *Fails on a private copy.*
- **Truth agreement.** `TestGuardSpentEqualsSettledSpend` over three fixtures: measurements
  only, orphan only, both. *Fails if a settle path stops persisting.*
- **Absence is asserted, not incidental.** `TestSelectExportReportEmitNoSpentUSD` and
  `TestGuardedMatchesTheStage` (table over every command) *(F1)*. *The first fails if a future PR
  adds a zero "for consistency"; the second fails if a stage's `guarded` value drifts from whether
  it actually constructs a guard.*
- **Failure after spend.** `TestValueReportsSpendAfterStoreError` (block rendered, error printed,
  exit non-zero, `SettledSpend` intact) and `TestPanickedRunsSpendIsRecoverableViaReport` *(F5)*.
  *Fails if the result struct stops being returned on non-budget error paths.*
- **Contract stability.** JSON goldens per stage (AC18 — the baseline this PR establishes) and
  `TestV01ShapeIsStillASubset`, which reads the frozen in-repo `cli/testdata/json/v0.1-shape.json`
  and checks every key v0.1.0 emitted unconditionally still exists with the same JSON type across
  all five stage documents *(Phase-2 amendment: the original test parsed a fenced sample in
  `docs/cookbook/ci-gate.md`, which is now a one-line tombstone)*. *Fails if a released key is
  renamed, removed, or retyped.*
- **Resume.** `TestResumedValueReportsLifetimeSpend` (kill, resume, assert the sum and no double
  charge); `TestBudgetStoppedRunStillReportsWhatItSpent`. *Fails if `Restore` is moved after the
  first `Authorize` or dropped.*
- **The qualifier.** `TestUsageEstimatedCasesAppearsInBothRenderings` and its zero-case
  counterpart. *Fails if the estimated-usage note is dropped again.*
- **Report composition.** `TestReportSpendTotalEqualsItsParts`;
  `TestReportSpendMarksIncompleteSourceRun`; `TestReportWithNoMeteredRunSaysSo`.
- **Privacy.** The sentinel-content assertion extended over the new surfaces.

## Rollback

Every change is additive and independently revertible:

- Reverting `ValueResult.Spent` and the `cli/value_render.go` + `cli/jsonreport.go` hunks returns
  `kno value` to silence. Nothing else depends on the field.
- Reverting `spendLines` restores Baseline's inline `Fprintf`; the golden is unchanged either
  way because the helper reproduces the line byte-for-byte.
- The new JSON keys (`spent_usd_micros`, `llm_calls`, `tokens`, `usage_estimated_cases`) can be
  removed individually; `spent_usd` is untouched throughout, so no v0.1 consumer is affected by
  any partial rollback.
- `report`'s `spend` object is one block in one renderer.
- ADR-0006 and the JSON goldens are documentation and test data; deleting them loses the
  enforcement, not the behavior.

There is no schema change and no store migration, so there is nothing irreversible.

## Docs impact

- **`docs/adr/0006-the-json-contract.md`** — new, §7's six rules. This is the deliverable that
  outlives the patch.
- **README** — ADR-0006 joins the Documentation list. This is the ADR's in-repo home
  *(Phase-2 amendment, AC17)*: the repository's only curated doc index, and the `--json` contract
  is a user-facing promise rather than a contributor-workflow rule.
- **~~`docs/cookbook/ci-gate.md`~~ — gone, and out of scope** *(Phase-2 amendment)*. The original
  plan added the ADR link, a `kno value` example, and the cross-stage `jq` snippet
  (`map(select(.guarded) | .spent_usd_micros) | add`, and never `// 0`) to that page. #163 turned
  it into a one-line tombstone that `make docs` forbids growing. The snippet and the `// 0`
  warning are **not lost** — they move into ADR-0006 rule 5, which is now the contract's only
  home and the right place for them. Teaching them in the `kno-examples` recipe is a follow-up in
  that repository, ledgered with a trigger, and explicitly not part of this PR.
- **`docs/what-the-numbers-mean.md`** — extend *"What a cost figure claims"* with two sentences:
  reported spend is the **run's lifetime** spend including resumed sessions, and it is what the
  guard settled, which is neither an invoice nor a bound on the dark-spend window
  (`docs/debt.md#20`).
- **CLI help** — `kno value --help` states that it reports what it spent; `kno select --help` and
  `kno export --help` state that they make no LLM calls and therefore report no spend, and where
  the measurement's cost lives. Snapshot-tested.
- **CHANGELOG** under `Unreleased`, with the added keys listed explicitly since they are a public
  contract change (additive).
- **`docs/debt.md`** — the rows in Accepted risks.
- **vhs** — `quickstart.tape` re-recorded: `kno value`'s output gains a line.

## Accepted risks

- **`spent_usd` remains a display string forever.** Fixing the type would break v0.1 consumers,
  so both forms ship and the docs say which is which. A `jq` author who reaches for the string
  and tries to sum it still gets a bad time. *Trigger: 1.0, when the contract becomes a covenant
  and the string can be deprecated in favor of the micros field with a major-version note.*
- **`spent_usd_micros` doubles every money key.** Two keys for one number is redundancy in a
  contract that should be minimal. Accepted because the alternative is float parsing of a
  currency-formatted string in CI. *Trigger: same as above.*
- **The absent-vs-zero rule is still a rule a consumer must learn** *(F1)*. `guarded` makes it
  self-describing for a consumer who reads it, but nothing in a CLI can police its consumer's
  `jq`: `.spent_usd` on `kno select --json` returns `null` rather than erroring, so the naive
  pipeline degrades quietly, and `.spent_usd // 0` is one character from there back into the
  ambiguity this plan exists to remove. The mitigation is documentation and example — the cookbook
  shows `select(.guarded) | .spent_usd_micros` and **never** shows `// 0`, which the first draft
  of this row wrongly recommended. *Trigger: the first user report of a broken pipeline, or 1.0,
  whichever is first.*
- **Reported spend is settled spend, not billed spend.** Discounts, committed-use pricing and the
  dark-spend window all put daylight between this figure and the invoice
  (`docs/what-the-numbers-mean.md:181`, `docs/debt.md#20`, `#32`, `#36`). Stated, not resolved;
  resolving it requires reading the provider's billing API, which is out of scope for an OSS
  engine. *Trigger: when any provider adapter can read a usage/billing endpoint.*
- **`Guard.Overshoot()` is recorded but still unrendered.** This plan renders what was spent and
  not how far past a cap the settlement went. A cost cap silently overshooting by
  `SettlementOvershoot`'s delta is observable in the event stream and not in the report.
  *Trigger: the first `SettlementOvershoot` observed in a nightly live run, or 1.0.*
- **`kno mine` has no `--json` at all** and is therefore untouched by this plan's contract.
  *Trigger: when `mine` grows a `--json` flag.*
- **The CI-gate recipe still teaches the pre-`guarded` idiom** *(Phase-2 amendment)*. The recipe
  moved to `uknoAI/kno-examples` in #163 and cannot be updated in this PR's commit. Until it is,
  the published example of a CI cost gate does not mention `guarded`, does not sum
  `spent_usd_micros`, and its `--json` stability sentence points at nothing citable. ADR-0006 is
  the in-repo mitigation. *Trigger: within one minor release of this PR shipping, or the first
  time a `kno-examples` CI-gate run reports a spend figure from an unmetered stage — whichever is
  first.*
- **`Store.SettledSpend` loses a Value run's token count** *(Phase-2 amendment, found by AC1's
  test)*. `core/value_loop.go`'s `sinkFunc` writes `budget.Spend{Calls, CostUSDMicros}` and drops
  `Tokens`; Baseline's `settledSpend` writes all three. Consequences: `kno report` cannot ever
  report a Value run's tokens, and — the part that is a real defect rather than a reporting gap —
  `Guard.Restore` seeds a resumed Value run with zero tokens, so a token cap is under-enforced
  across a resume. Not fixed here: it is a change to what a stage persists about money, which
  needs its own plan and its own resume test rather than a hunk in a rendering PR. This plan's
  own surfaces are unaffected (the in-process block reads the guard; `report`'s entries carry no
  token field). *Trigger: before the next minor release, or the first time a token cap is set on
  a Value run that resumes — whichever is first.*
- **`value` records no `usage_estimated_case_count`, so its qualifier line can never fire**
  *(Phase-2 amendment)*. `core/value_loop.go:1015-1020` writes a `CaseExecution` with attempted /
  scored / errored / resolved models and no usage-estimated count; only
  `core/baseline_close.go:242` writes one. AC7's qualifier is therefore exercised on `baseline`
  and is structurally dead on `value`, which means a Value run priced from the engine's estimate
  rather than reported usage says so nowhere. This plan renders the field and does not start
  recording it — that is a `core` measurement-aggregation change with its own store query, and
  bundling it into a rendering PR is how a rendering PR becomes unreviewable. The renderer is
  built to take the number the day it exists. *Trigger: before 1.0, or when the first provider
  adapter that cannot report token usage is measured with `kno value` — whichever is first.*
