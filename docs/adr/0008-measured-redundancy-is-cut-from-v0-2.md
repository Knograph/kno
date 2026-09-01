# ADR-0008: measured redundancy is cut from v0.2, because TOST over a paired difference is the wrong instrument

- **Status:** accepted
- **Date:** 2026-09-01
- **Context:** [The redundancy-detection plan](../plans/2026-08-31-redundancy-detection.md) §"The statistic" and §"The margin δ, argued rather than invented", its implementation-phase addendum, [PR #178](https://github.com/uknoAI/kno/pull/178), and [debt.md#165](../debt.md#165).

## The problem

`main` rejects an Asset as `REDUNDANT` on one instrument only: 3-gram shingle overlap on content, gated to `KIND_KNOWLEDGE`. Two behavior Assets that say the same thing in different words are both selected, and the tuning-set duplication that costs is invisible. The plan's headline was to make redundancy **measured rather than read** — two Assets whose recorded per-Case deltas are equivalent and co-located are duplicates however differently they are written — and its first condition is where the design failed.

**Condition 1** asks whether the paired per-Case differences `d_A(c) − d_B(c)` over the shared routed slice `C` have a two-sided interval lying entirely inside `±δ`, with

    δ = max( --redundancy-margin , MinDetectableEffect(|C|, TWO_SIDED, level) )

The plan chose TOST deliberately, and said exactly why:

> Gating redundancy on "the CI contains zero" would make redundancy *more* likely the less data you have. That is backwards, and it is the failure mode this condition exists to prevent.

That reasoning is right. **The implementation reproduces the failure it names, by a different route**, and the route is the margin's own derivation rather than the test's shape. This record exists because that is not a bug to patch — the two halves of Condition 1 are each individually correct, and the defect is in composing them into a decision rule.

## What the evidence shows

All four findings below were reproduced on 2026-09-01 against `feat/redundancy-detection` at `1180dd4`, not taken from the branch's own reports.

**1. The margin is derived from the sample's own resolution, so it widens as the sample shrinks.** `MinDetectableEffect` is a floor on what `|C|` observations can separate from zero, and "you may not claim equivalence finer than your sample can see" is the correct argument for it. But Condition 1 uses it as the width of the region the interval must fit *inside*. The interval also widens as `|C|` shrinks — just more slowly. Less data therefore makes "these two Assets are interchangeable" **easier** to claim, which is the plan's own stated failure mode arriving through the margin instead of through the null.

**2. The divergence is unbounded, not a threshold that needs tightening.** `MinDetectableEffect` is `q(level, df=n−1)·√0.5/√n`, decaying as `~1/√n`. The interval Condition 1 actually receives on a zero-spread sample is `signBound`'s, whose half-width is `−log(1−level)/n` scaled by `|mean|` (1 when the mean is 0, which is what equivalence looks like) — decaying as `~1/n` exactly. Their ratio grows without bound. Measured against the real functions, at level 0.95, by calling `interval.Paired` on an `n`-length zero-spread sample and `interval.MinDetectableEffect(n, TWO_SIDED, 0.95)`:

| \|C\| | `signBound` half-width | margin δ | δ ÷ half-width | method |
|---|---|---|---|---|
| 4 | 0.7489 | 1.1252 | 1.50× | sign |
| 6 | 0.4993 | 0.7421 | 1.49× | sign |
| 10 | 0.2996 | 0.5058 | 1.69× | sign |
| 20 | 0.1498 | 0.3309 | 2.21× | sign |
| 40 | 0.0749 | 0.2261 | 3.02× | sign |
| 100 | 0.0300 | 0.1403 | 4.68× | sign |
| 640 | 0.0047 | 0.0549 | 11.73× | sign |
| 1600 | 0.0019 | 0.0347 | 18.52× | sign |

Every row clears the margin, and clears it by more as `n` grows. **A larger shared slice does not make the claim safer; it makes an unfounded one more confident.** No ceiling on δ closes this: `--redundancy-max-margin` removes the sample sizes whose margin exceeds it, and inside whatever sizes survive the comparison is unchanged.

**3. The monotonicity property the plan made an acceptance criterion does not hold — and the test written to check it could not see that.** Acceptance criterion 4 requires that shrinking the shared slice never increases the number of `REDUNDANT` verdicts. `TestRedundancyMonotonicityUnderShrinkingOverlap` sweeps `|C| ∈ {40, 30, 20, 15, 10, 6, 5, 4}` over seeded synthetic pairs at a 0.90 ceiling (`|C| = 4` is below `MinOverlapCases = 5` and is never attempted). At the shipped 200 trials it counts `REDUNDANT` as `[0 0 1 1 1 3 4 0]` — rising as overlap shrinks, so the criterion is already false.

The second half is the one worth recording. With the degenerate path refused — the second alternative below — the same sweep at 200 trials reads `[0 0 1 1 0 0 0 0]` and looks nearly clean. At **4000** trials it reads:

```
|C|:         40  30  20  15  10   6   5   4
REDUNDANT:    4  13  16  20  27   0   0   0
```

`REDUNDANT` climbs steadily, 4 → 27, as the window shrinks from 40 to 10. **The acceptance test was underpowered to measure the property it existed to measure**, and at 200 trials it would have certified a fix that did not exist. An acceptance criterion stated over a random process is a hypothesis test; it needs a power argument of its own, or it reports the absence of evidence as evidence of absence.

**4. The feature's own tests exercise the unsound path almost exclusively.** Every fixture in the suite that produces a measurement-decided `REDUNDANT` verdict passes the **same delta vector on both sides** — the headline criterion-1 test (`TestRedundantBehaviorAssetsWithDisjointShingles`, both Assets `scoresFor(20, pass...)`), the cost tie-break, the destination-override case, the three-way case, the MINIMIZE case ("both Assets improved the SAME 16 Cases identically"), `core/explain_test.go`'s fixture, and `cli/redundancy_test.go`'s `seedRedundantPair` ("16 of 20 pass — identical for both Assets"). At the unit seam it is literal:

```go
evaluateMeasurement("with", scoresFor(20, intRange(16)...), scoresFor(20, intRange(16)...), ...)
```

A difference of identical vectors is all zeros, which has no spread, which takes `signBound`. So Condition 1's positive path is exercised almost entirely through **the one input where a paired-difference interval carries no information about magnitude**. The suite being green was evidence about the degenerate path and nothing else.

## The analysis

The feature's *intuition* is sound: two Assets with identical measured behaviour are redundant. The *machinery* is not. Expressing that intuition as a TOST equivalence test over a paired-difference interval fails exactly where the intuition is strongest, because **TOST has nothing to say about a sample with no variance** — and, per finding 2, the failure gets more confident with more data rather than less.

## The decision

**Redundancy detection is cut from v0.2. PR #178 does not merge, and stays open as the record.** Not merged with a tightened default, not merged behind a flag, not merged with the criterion-4 test deleted. Condition 1 needs a redesign, and a redesign is a Phase-0 plan, not an amendment inside the PR that found the hole.

`main` keeps the rule it already ships — `REDUNDANT` via content shingles, knowledge-kind only. Nothing rolls back, because nothing landed.

## Alternatives considered

**Tighten the margin ceiling (`--redundancy-max-margin`).** Measured, not assumed: a sweep at ceilings 0.10 / 0.15 / 0.20 / 0.25 / 0.30 / 0.35 / 0.50 / 0.90, 200 trials each, over all eight window sizes. Criterion 4 holds only at a ceiling of roughly 0.25–0.29, and only because that admits solely the largest window swept (`|C| = 40`) while refusing everything smaller as `UNKNOWN`; one step to 0.30 and the first violation reappears. That is a razor-thin empirical window, not a property — and finding 2 says why it can never be one: the margin/width ratio diverges, so a ceiling buys a *range of sample sizes* rather than a correct rule. **Rejected.**

**Refuse the degenerate path** — return `UNKNOWN` whenever the difference interval comes back with method `sign` at all, rather than accepting it whenever it happens to clear the derived margin. Implemented and measured. It removes the small-`n` failures at the trial count the acceptance test uses: `[0 0 1 1 1 3 4 0]` becomes `[0 0 1 1 0 0 0 0]` at 200 trials. But a third mechanism survives it, and 4000 trials show it plainly (`[4 13 16 20 27 0 0 0]`): at small `n` the **observed** spread has higher variance, so a sample more often looks tighter than it is. Refusing zero spread does not refuse near-zero spread that arrived by chance. It also breaks the feature's own tests, which is finding 4 restated as a build failure. **Rejected as insufficient** — though some form of it is likely to be part of whatever replaces Condition 1.

**Ship it as-is and document the limitation.** `kno select` drops Assets from a Portfolio; a false `REDUNDANT` is not a misreported number a user can discount, it is data removed from the thing they will fine-tune on. The plan itself names this as the most expensive error the feature can make — two Assets with equal means that fix **disjoint** Cases are perfect complements, and Condition 1 alone throws one away. And the exposure is worst where the feature would actually run: under default routing the pairs that get tested are exactly the small-overlap ones, because `MinOverlapCases` = `core.MinClusterCases` = 5 and `value.DefaultMinSample` = 5 put a typical routed-slice intersection at or barely above the floor (the plan's own finding F4, accepted risk 8). The plan expected that to yield `UNKNOWN`. The measurement says it yields `REDUNDANT` *more* often than a large overlap does. **Rejected.**

## The direction, which is not yet a decision

Recorded so the redesign starts from the diagnosis rather than from the symptom, and explicitly **not designed yet** — there is no plan, no derivation, no false-positive characterization behind any of it:

Treat **behavioural identity as its own evidence kind**, alongside `CONTENT_SHINGLE`, justified on its own terms rather than laundered through an interval that claims a confidence it does not have. An exact or near-exact agreement rate over a shared slice is a real observation; the mistake was dressing it as a confidence statement produced by a test that cannot make one. Keep **TOST for the genuinely noisy case**, where it is the right tool, with a **power requirement** attached — a pair whose sample cannot resolve the margin it is being tested against reports `UNKNOWN`, rather than passing *because* it cannot resolve anything.

Whoever picks this up owns a Phase-0 plan, and criterion 4 stays in it, at a trial count with a stated power argument.

## What is salvaged, and should not be rebuilt

None of this is on `main`. It lives on `feat/redundancy-detection`, which is why PR #178 stays open rather than being closed:

- **The `kno.v1` proto additions** (`RedundancyEvidence`, `RedundancyEvidenceKind`, `RedundancyDecidedBy`, `MarginSource`, `CoImprovementFloorSource`). The evidence message names its *instrument*, which is precisely what makes "add a third evidence kind" additive rather than breaking — the shape the direction above needs.
- **`stats/interval.JChance`** (`stats/interval/coimprovement.go`) — the Jaccard two unrelated Assets with the same observed improvement rates would produce by coincidence. Independent of Condition 1, and Condition 2 is not what failed.
- **The eviction logic in `core/select.go`** and the per-Case delta reader behind it: reading a Value run's Measurements back, sign-correcting per direction, and evicting a candidate against an already-selected Asset in precedence order.
- **`--explain`** (`core/explain.go`, `cli/select.go`) — the per-Case table behind a `REDUNDANT` claim, which is useful whatever instrument decides it.
- **The run-scoped holdout canary** (`core/select_test.go`, the plan's finding F5): the canary now permits reads scoped to the Value run's ID and that run's recorded `baseline_run_id`, and fails on any other run ID, replacing a method-name-scoped guard that Select's new reads had outgrown. Strictly better, and independent of the statistics.

## Consequences

- **`main` is unchanged.** Select's redundancy behaviour, its rejection log, and `--json` are exactly what 0.1.6 ships. There is no migration and nothing to announce.
- **[`docs/debt.md#165`](../debt.md#165) stays open**, pointing here, with its trigger intact. It is the ledger entry that made this decision reachable, and retiring it because the feature was cut would be the silent carryover the ledger exists to prevent.
- **`DESIGN.md`'s v0.2 line still names "redundancy detection", and this record does not edit it.** Flagged rather than silently corrected: the roadmap line and this ADR disagree until whoever owns the release scope decides where the redesigned feature lands. `DESIGN.md` is architecture truth and this record is not the place to re-cut a milestone.
- **The underpowered-acceptance-test lesson generalizes.** Every property test in `stats/` asserts something about a random process at a chosen trial count, and none of them today carries an argument that the count is enough to see the property fail. This is the first case where that gap produced a nearly-clean result for a broken design.
- If someone revives Condition 1 unchanged, or proposes a tighter ceiling as the fix, this record is what a reviewer cites.

## What this record does not claim

**It does not claim `MinDetectableEffect` or `signBound` is wrong.** Each is correct for the question it was built to answer — the first, what effect a sample of this size could separate from zero; the second, how much confidence repeated agreement licenses under a distribution-free rule. Neither function is touched by this decision. The defect is entirely in composing one as the margin and the other as the interval in a single accept-the-null decision, where their different rates of decay in `n` become a divergence.

**It does not claim Condition 2 failed.** The co-improvement Jaccard against its own chance floor is not what let these verdicts through. It did not stop them either, and the reason is recorded in the test's own comment rather than treated as a second defect: when a tied window happens to contain a shared firing, the observed Jaccard and its `J_chance` floor are both small together, for the same combinatorial reason that produced the tie. Condition 2 remains the guard against the feature's most expensive error, and the redesign should keep it.
