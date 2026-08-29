# Select and Export: portfolio construction and destinations

The two stages that turn per-Asset valuations into decisions. Planned together because Select's
output IS Export's input.

Fires [`docs/debt.md#65`](../debt.md#65) (heterogeneous estimands), [`#66`](../debt.md#66)
(multiplicity), [`#67`](../debt.md#67) (net-loss judgment), and [`#78`](../debt.md#78) (A7
sample-splitting) — each entry names Select/Export as its trigger.

**Phase-1 re-reviewed 2026-08-29 — verdict: do not implement as written.** The review's
dominant finding: the first draft described the schema as if it did not exist. The amendments
below are written against the actual `proto/` and `store/` — the rejection-reason enum already
exists in full, `delta_per_cost` is a field nobody populates, the Portfolio needs persistence
that does not exist, and the #78 disposition the first draft proposed re-dated itself into
self-satisfaction. Every finding (A1–A16) is folded in and tagged.

## Problem

Value reports per-Asset deltas with intervals. Nothing yet: (a) picks a COMBINATION under the
budgets that actually bind (context tokens, training examples, money), (b) explains each
rejection, (c) writes the selected assets to their Destinations (`context | knowledge_base |
tuning_set` — the enum exists in `common.proto`), or (d) disposition debt 78's trial-splitting
question. "Include nothing new" must be a legal, first-class outcome.

## Design

### Step 0 — what already exists (the review's lesson, stated up front)

`valuation.proto` already defines `RejectionReason` with every value this plan needs
(`NO_EFFECT`, `REGRESSION`, `REDUNDANT`, `COST_DOMINATED`, `WRONG_MECHANISM`, `IRRELEVANT`,
`BUDGET_EXHAUSTED`, `MEASUREMENT_FAILED`, `UNDERPOWERED`), and `REDUNDANT` already carries
`repeated redundant_with_asset_ids` — a rejection log that cannot say WHICH Asset something
duplicated is not actionable. `portfolio.proto` defines `Portfolio`, `PortfolioEntry`,
`Rejection`, `Budget`, `CostVector`. `run.proto` defines `STAGE_SELECT` and `STAGE_EXPORT`.
What does NOT exist: a populated `delta_per_cost` anywhere in `core/`, any Portfolio
persistence in `store.Store`, `n_control`/`fresh_control_arm` on the durable Valuation, and
the `PortfolioSelected`/`ExportWritten` event members. The plan is written against that. *(A1)*

### Step 1 — make the ranking denominator real (Value-stage work, first PR)

`Valuation.delta_per_cost` (field 11) is never populated (`core/value_measure.go:169` is a
comment). This plan's first PR populates it in the Value stage: denominator =
**context_tokens** — the bias debt #68 names ("it is the ranking denominator") is
acknowledged in the field's godoc and in `what-the-numbers-mean.md` in the same PR, not
argued away. *(A2, A5)*

### Step 2 — Select (`core` stage, `kno select`)

```
Inputs:  the Value run's recorded Valuations (--value-run-id), budgets:
         --max-context-tokens, --max-training-examples, --max-cost-usd
Output:  Portfolio + rejection log, persisted (see Step 3), rendered + --json
```

- **Greedy on delta_per_cost, honestly labeled** *(A6)*: greedy on one ratio with per-item
  feasibility checks never exceeds a budget, but a three-constraint knapsack has no
  greedy approximation guarantee. The plan claims what greedy actually guarantees —
  feasible, deterministic, reproducible — and states "no approximation guarantee" in the
  docs and the rejection-log header. `--max-cost-usd` bounds the CARRYING cost of selected
  assets; the Value run's measurement cost is reported alongside, not counted against it.
- **Multiplicity correction (#66)**: Bonferroni on the number of Assets screened, applied
  to the intervals used for keep/reject decisions. An Asset whose corrected interval
  crosses zero is rejected `NO_EFFECT` — decisively, not advisory. The winner's-curse
  test is TWO tests, because the first draft's single assertion could not fail
  *(A8)*: (a) thousands of simulated screens over synthetic ground truth (Select is a
  pure function — cheap) asserting the false-discovery rate at the corrected level, and
  (b) the `stats/interval` characterization the entry's trigger demands: ranking
  inflation as a function of `n_screened`, pinned as a bound.
- **Net-loss judgment (#67), computed with what exists once the plan adds it** *(A4)*:
  the durable Valuation gains `n_control` and `fresh_control_arm` (additive) — without
  `n_control` the weighted combination is uncomputable from the named input, and without
  `fresh_control_arm` the covariance is unknown: a recorded-baseline control arm pairs
  every delta against the same draw (debt 66 names the correlation). The net combines
  treatment and control deltas weighted by their populations, WITH an interval that
  accounts for the shared draw conservatively. The regression verdict is gated:
  `control_underpowered` ⇒ never accuse (`REGRESSION` requires the whole net interval
  ≤ 0 AND a powered control — an underpowered harm test that looks like a passed one is
  worse than an absent one, and the enum's strongest reason must not fire on noise).
- **Heterogeneous estimands (#65), the model stated** *(A5)*: scaling tagged deltas by
  `n_routed/n_dev` is sound under ONE model — effect uniform on the routed slice, zero
  elsewhere — and wrong under a fixed-effect model. The plan states the model, writes the
  factor into `what-the-numbers-mean.md` in this PR (a "docs diff pairs with behavior"
  gate), scales under it, records the scaling per entry, and defines the absent-`n_routed`
  case (unscaled, flagged — never a silent division).
- **Redundancy, within-kind only** *(A12)*: shingle overlap on content is meaningful for
  knowledge assets and nearly meaningless for behavior assets (two demonstrations of the
  same tool-use pattern share no shingles), and behavior is exactly the tuning-set class
  where duplication matters most. v0.1 detects redundancy within knowledge-kind only; the
  behavior gap is an accepted limitation named in the docs, not silently covered. The
  greedy's order-dependence (whichever duplicate's noisy delta was higher is kept)
  inherits the winner's curse — stated, and the tie-break is delta-then-id, deterministic.
- **Rejection-rule precedence** *(A14)*: one Asset can trip several rules; the log carries
  one reason. Order, strongest claim first: `REGRESSION` (harm, interval-gated) →
  `NO_EFFECT` (corrected interval crosses zero) → `REDUNDANT` → `COST_DOMINATED` →
  `WRONG_MECHANISM`. Pinned by a table-driven test.
- **Source-run status** *(A16)*: a Select over a `BUDGET_STOPPED` Value run silently builds
  a portfolio over a partial measurement set. Select REFUSES a truncated source unless
  `--allow-partial`, and the Portfolio carries the source run's status and
  `incomplete_reason` either way.
- **The portfolio-level claim** *(A7)*: `Portfolio.dev_estimated_gain` /
  `dev_estimated_interval` are required by the existing proto. They are NOT the sum of
  per-asset deltas — routed slices overlap and every asset pairs against the shared
  baseline draw. Computed with the same shared-draw covariance discipline; the
  portfolio-level interval is a single corrected claim, and the winner's-curse inflation
  travels with it in the godoc and the report.
- **Trials** *(A13)*: the Valuation is trial-averaged (`PairedTrials` collapses trial
  vectors before the interval) — there are no per-trial deltas for Select to consume, and
  the first draft's row claiming otherwise is deleted. There is no budget stop to resume:
  Select makes no LLM calls; the "resumable select" sentence is removed as category
  confusion. *(#75 is unaffected by this plan — one sentence so the record shows it was
  considered.)* *(A15)*

### Step 3 — Portfolio persistence (the interface change, accepted)

`store.Store` has no Portfolio methods. `WritePortfolio`/`Portfolio` join the interface —
a pre-1.0-permitted break with a CHANGELOG migration note, all fakes updated, and a
`portfolios` table holding the Portfolio proto. The Select and Export stages write Run
records with `STAGE_SELECT` / `STAGE_EXPORT` (the enum values already exist), so the
event stream and resume machinery treat them like every other stage. *(A9)*

### Step 4 — Export (`kno export`)

- **Destination grammar**: `--destination context|knowledge_base|tuning_set` — all three
  the design ships, the third named in the CLI, not dropped *(A10)*. `context` = a
  context-pack manifest + rendered pack; `knowledge_base` = a manifest + human-readable
  instruction list (writable-KB adapters are v0.2; the manifest says so); `tuning_set` =
  **OpenAI chat format** JSONL (DESIGN's pinned shape, named here — this is the file the
  Tuner adapters will parse) plus the dataset manifest. Overwrite policy: refuse an
  existing target unless `--force`; writes are temp-then-rename.
- **#78, dispositioned honestly** *(A3)*: the first draft re-dated #78 to "when a third
  Destination lands" — a trigger THIS PR satisfies, i.e. a carryover wearing a
  disposition, the exact failure the ledger rules forbid. The amended disposition pays
  the cheaper half now and re-dates the rest to a trigger that cannot self-satisfy: the
  **pairing-scheme recording** — "which trial routed which Case" — lands in THIS plan as
  an additive proto field on the durable Valuation (the schema half of A7), and the
  measurement-design half (splitting baseline trials across the routed sample) re-dates to
  **"when the first writable Destination adapter lands (v0.2 knowledge injection)"** —
  which nothing in this plan touches. The ledger entry is rewritten to record both halves.
- **Idempotence**: re-export of the same Portfolio produces byte-identical files (golden
  files pin it); export never mutates a destination.
- **The report and gaps artifacts** (DESIGN's "Export & Gaps") are out of scope with a
  named home: they belong to the `report` milestone, which this plan references rather
  than drops silently. *(A10)*

### Events

`PortfolioSelected` and `ExportWritten` are new oneof members on `event.proto` — counted
here as the schema addition they are *(A11)* — additive under `buf breaking`, emitted
from the stages, never side channels.

## Alternatives considered

**Ranking without multiplicity correction.** Rejected: debt 66's correlation makes the
uncorrected ranking systematically wrong exactly where it is loudest.

**Embedding-based redundancy.** Rejected for v0.1; shingle overlap is deterministic and
testable, and the within-kind scope above names the gap.

**Export before Select.** Rejected: the Bridge's premise is that Select's shortlist is the
input.

**Paying all of #78 in this PR.** Rejected: the measurement-design half is a Value-stage
change; paying the recording half and re-dating the rest to a non-self-satisfied trigger is
the disposition the ledger rules actually permit.

## Affected packages

`core/` (select stage, export writer, delta_per_cost population in Value), `stats/`
(net-loss with shared-draw covariance, multiplicity correction, winner's-curse
characterization), `proto` (Valuation additions: `n_control`, `fresh_control_arm`, A7's
pairing recording; `PortfolioEntry` scaling fields; `event.proto` oneof members; Run
already has the stage values), `store/` (portfolios table + interface methods), `cli/`
(`kno select`, `kno export`), `docs/` (mental model, what-the-numbers-mean — the #65
factor and the #68 denominator bias land here in the same PR), `docs/debt.md` (#65/#66/#67
repaid, #78 split-and-re-dated).

## Proto / schema impact

Additive only, verified against the ACTUAL schema: the enum and portfolio messages exist;
the additions are the fields and event members listed above. `buf breaking` passes — no
cardinality changes to existing fields.

## Edge cases

| Case | Behavior |
|---|---|
| Zero Assets pass Value | Portfolio "include nothing new" — legal, reported, exit 0 |
| Budgets smaller than the smallest Asset | `COST_DOMINATED` per Asset; "nothing fits" is explicit |
| Near-duplicates tie | Deterministic tie-break (delta, then id) |
| Source Value run was budget-stopped | Refused unless `--allow-partial`; status travels |
| Underpowered control arm with negative point estimate | NOT rejected as regression — the gate; reported as underpowered |
| `n_routed` absent on a tagged valuation | Unscaled, flagged — never a silent division |
| Export path exists | Refused unless `--force`; temp-then-rename; nothing partial |
| Holdout | Select reads dev-side valuations only; the seal applies |

## Test plan

- The two-part winner's-curse evidence: simulated-screen FDR at the corrected level +
  `stats/interval` inflation-vs-`n_screened` bound.
- Net-loss: weighted-population arithmetic, shared-draw covariance, the
  `control_underpowered` gate (verified failing when the gate is removed).
- Rejection precedence table; redundancy within-kind; determinism golden files.
- Portfolio persistence: store round-trip, fakes updated, Select/Export Run records.
- Export idempotence goldens; overwrite-refuse; tuning_set shape pinned to OpenAI chat
  format.
- The holdout canary (Select never opens the holdout).

## Rollback

Deleting the stages restores today EXCEPT the store interface addition, which is reverted
in the same deletion. All proto additions are additive and unreferenced by pre-existing
code.

## Docs impact

Mental model (Select/Export future tense → present), What the numbers mean (the #65
scaling model, the #68 denominator bias, the corrected-interval discipline), cookbook
("Choose a portfolio under budget", "Export a tuning set"), CLI help snapshots, CHANGELOG,
debt ledger rows (three repaid, one split).

## Accepted risks

- **Bonferroni is conservative.** Correctness-first; Benjamini-Hochberg is the named
  upgrade path.
- **Greedy has no approximation guarantee** on the three-constraint knapsack. Feasible,
  deterministic, reproducible — stated in the report, not hidden.
- **Within-kind redundancy only.** Behavior-asset redundancy is a v0.2 question.
- **The #65 scaling model is an assumption.** Stated, written into the docs, and recorded
  per entry so a reader can undo it.
