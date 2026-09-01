# Redundancy detection for behavior Assets: measure it, don't read it

`docs/plans/2026-08-29-select-export.md` accepted a named limitation *(A12)*:

> **Within-kind redundancy only.** Behavior-asset redundancy is a v0.2 question.

This is that question. `DESIGN.md:398` lists "redundancy detection" in the v0.2 milestone.

**What ships today, verified line by line against `core/select.go`:**

- `defaultShingleOverlap = 0.6` (`core/select.go:24`) — the Jaccard threshold on lowercase word
  3-grams.
- `redundantWith` (`:637`) compares a candidate's `Asset.content` against
  `selectedKnowledge` — a `[]decidedKnowledge{assetID, content}` (`:252`) appended at `:336`
  **only** when `kindOf(v) == KIND_KNOWLEDGE`.
- The gate in `rejectReason` (`:441`) is `asset != nil && kindOf(v) == Kind_KIND_KNOWLEDGE`. A
  behavior Asset can never reach the REDUNDANT branch, by construction.
- The rejection detail is the literal string `"shingle overlap above the redundancy
  threshold"` — **no number, no threshold, no overlap size.** A user who disagrees has nothing
  to check.
- Without `--pool`, `SelectOptions.Pool` is nil, `REDUNDANT` and `WRONG_MECHANISM` are appended
  to `DegradedRules` (`:279`) and neither runs.

So: the tuning-set class — the one where duplication costs the most, because a fine-tuning set
of near-identical demonstrations both wastes the example cap and over-weights one pattern — has
no redundancy detection at all, and the detection that does exist cannot be audited.

**Phase 0. Not implemented. No code written.**

**Phase-1 re-reviewed 2026-08-31 — verdict: amend; amendments applied.** Four things changed.
The reviewer verified that the reconstruction path this plan rests on is real, and it is folded
as settled fact rather than hedge: `store.Measurements` returns rows per
`(case_id, arm, trial)` (`store/measurement.go:409`), `store.CaseScores` returns the baseline
side keyed by Case (`:333`), and `core/value.Plan.Routed` (`core/value/route.go:301`) carries
`CaseIDs` per Asset (`:277`). `SelectOptions` (`core/select.go:31`) has no Agent and no guard
field, so "pure function of the store" holds. The shipped rule matches this plan verbatim — the
kind gate at `core/select.go:441`, `rankLess` at `:625`, `defaultShingleOverlap = 0.6` at `:24`
— as do `tapes/quickstart.tape:98`, `value.DefaultMinSample = 5` (`core/value/route.go:238`) and
`core.MinClusterCases = 5` (`core/gaps.go:14`). Two line numbers the first draft got wrong
(`:408` for the kind gate, `gaps.go:15`) are corrected in place.

The must-fix: **Condition 2's Jaccard threshold was never named** — not in the design, not in the
acceptance criteria, not as a flag. That is the same invented-threshold trap the δ derivation
exists to avoid, committed for the other required condition. It is now derived, symmetrically to
δ, as chance-corrected co-improvement with a user floor defaulting to zero *(F1)*. The cost
tie-break no longer inherits the `context_tokens` bias `docs/debt.md#68` already documents into
an *elimination* decision *(F2)*. The claim that within-destination comparison "subsumes" v0.1
was **false** against the code — it is a narrowing — so the content path keeps its shipped
destination-blind scope and the compatibility fixture must exercise a cross-destination knowledge
pair *(F3)*. And the feature's expected firing frequency under default routing — low, for a
correct reason — is stated in Accepted risks instead of left for a user to discover *(F4)*.
Confirmed sound and not reworked: the TOST margin derivation, the UNKNOWN-selects posture, the
uncertainty reporting, and the vhs scope.

## Problem

Four things, each verified:

1. **Shingles cannot see behavior redundancy.** Two demonstrations of the same tool-use
   pattern — "look up the order, then check the refund window, then answer" — over different
   orders, different SKUs, different phrasing, share almost no word 3-grams. `shingleOverlap`
   (`core/select.go:667`) returns near zero on exactly the pair a human would call duplicate.
2. **And it over-fires on the other side.** Two behavior demonstrations with near-identical
   boilerplate and *different* tool sequences share most of their shingles. Content similarity
   is not the question being asked.
3. **The claim is unverifiable.** The rejection log names which Assets a rejected one duplicated
   (`Rejection.redundant_with_asset_ids`, `portfolio.proto:52` — the select/export plan
   correctly insisted on that field), but not on what evidence or by how much.
4. **The redundancy verdict has no uncertainty attached, in a tool whose prime directive 5 is
   that no reported delta ships without its CI.** "These two Assets are duplicates" is a claim
   about measurements and it is currently reported as a fact.

## Design

### The core decision: redundancy is measured, not read

**Two Assets are redundant when their measured effects are equivalent *and* co-located on the
same Cases.** Not when their text is similar.

The argument for measurement over content, before the mechanics:

- **The data already exists and costs nothing.** `store.Measurements(ctx, runID, assetID)`
  (`store/store.go`) returns every recorded `(case_id, arm, trial)` with its score for one
  Asset; `store.CaseScores(ctx, baselineRunID)` returns the recorded-baseline control side;
  `Valuation.case_ids` (`valuation.proto` field 8) names the routed slice; and
  `Run.value_plan` (`run.proto` field 29) holds the gob-encoded `core/value.Plan` with
  `Routed[].CaseIDs`. Per-Case delta vectors are reconstructable for every measured Asset with
  **no new dependency, no provider call, and no new spend path** — which matters more than it
  sounds, because `SelectOptions` (`core/select.go:31`) has a `Store`, a `Pool` and a `Budget`
  and no `Agent`, no `Goal` and no guard, and its own godoc states the property that buys:
  *"every decision below is a pure function of what the store holds."*
- **It is the right question.** Content similarity asks "do these look alike". Redundancy asks
  "does the second one buy anything the first did not". The second question is answered by what
  the Assets *did*, and Value already recorded it.
- **It is kind-agnostic.** A delta vector is a delta vector. The behavior/knowledge split
  disappears — which is what makes this a fix for A12 rather than a second special case beside
  it.

**The honest limit, stated first.** Nothing in the pipeline ever measures A **and** B together.
The Value stage measures each Asset against the baseline independently. So this detector does
not *measure* marginal contribution given the other Asset — it infers substitutability from two
independent measurements. Two Assets that each fix the same Cases alone might, together, fix no
more than either alone (redundant — the inference is right) or might interact (they are not).
**The tool must say "their measured effects are equivalent and co-located", not "adding the
second buys nothing".** That distinction goes in `docs/what-the-numbers-mean.md` and in the
rejection detail, in those words. Measuring the pair jointly is a Value-stage change with a
combinatorial cost and belongs to a different plan.

### The statistic: two conditions, both required

For a candidate `B` against an already-selected `A`, over the shared routed slice
`C = routed(A) ∩ routed(B)`:

**Condition 1 — equivalent magnitude, tested as equivalence.** The paired per-Case differences
`d_A(c) − d_B(c)` for `c ∈ C` have a two-sided interval (built by `stats/interval` under the
Goal's declared `ScoreDomain`, exactly as every other interval in this codebase) that lies
**entirely inside `±δ`**.

This is a **TOST / equivalence test, not a difference test**, and the distinction is the
statistical crux. "Their deltas are indistinguishable" is *accepting* a null. An interval
containing zero does not establish equivalence — it establishes that the sample could not
detect a difference, which is exactly what a tiny overlap produces for every pair. Gating
redundancy on "the CI contains zero" would make redundancy *more* likely the less data you
have. That is backwards, and it is the failure mode this condition exists to prevent.

**Condition 2 — co-located incidence.** Let `I_X = { c ∈ C : d_X(c) > 0 }` (for
`SCORE_DOMAIN_BINARY` this is exactly the set of Cases the Asset flipped fail→pass; for
continuous domains, positive beyond a per-Case noise floor). The **co-improvement Jaccard** is
`J = |I_A ∩ I_B| / |I_A ∪ I_B|`, with its own bootstrap interval over `C`.

**The threshold is derived, not chosen** *(F1)*. The first draft required J "at or above a
threshold" and then never named, derived, or flagged that number anywhere — which is precisely
the invented-threshold trap the δ derivation below exists to avoid, committed for the second of
the two conditions the claim requires. The fix is symmetric to δ: **the data supplies the floor
and the user may only raise it.**

    J_min = max( --redundancy-min-coimprovement , J_chance(|I_A|, |I_B|, |C|) )

`J_chance` is the Jaccard two *unrelated* Assets with the same observed improvement rates would
produce by coincidence: for `a = |I_A|`, `b = |I_B|` over `n = |C|`, independence gives
`E|I_A ∩ I_B| = ab/n`, hence `J_chance = (ab/n) / (a + b − ab/n)`. Condition 2 holds when the
**lower bound of the corrected bootstrap CI on J exceeds `J_chance`** — co-location beyond
coincidence, and nothing weaker.

This is the same correction the sibling `judge calibrate` plan's κ makes, for the same reason: a
raw agreement number is uninterpretable until chance agreement is subtracted. Two Assets that
each improve 80% of a slice overlap on ~64% of it while sharing nothing but prevalence, and a
fixed constant like "J ≥ 0.5" would call that pair co-located. It also fails in the right
direction — a small `C` widens the CI on J, the lower bound drops below `J_chance`, and the
verdict is not REDUNDANT — which is what keeps acceptance criterion 4's monotonicity property
true of Condition 2 as well as Condition 1.

`--redundancy-min-coimprovement` **defaults to 0**, exactly as `--redundancy-margin` does, so no
invented number ships in the default path; a user who wants a stricter claim than "beyond chance"
sets it, and the evidence records which term produced `J_min`. The two knobs deliberately run in
opposite directions — δ's floor stops a user claiming equivalence finer than the data resolves,
J's floor stops one claiming co-location the data does not show — and in both cases the `max()`
picks the more conservative of the user's number and the data's.

Two degenerate cases fall out, handled rather than papered over. When `I_A ∪ I_B` is empty
(neither Asset improved anything on `C`) J is undefined and the verdict is `UNKNOWN`. When both
Assets improve nearly every Case in `C`, `J_chance → 1` and no interval can clear it — correctly,
because co-location carries no information when everything is co-located — so the verdict is
`UNKNOWN` and both Assets are selected.

**`stats/` has no bootstrap today.** Verified: `grep -rn -i bootstrap stats/ --include='*.go'` outside tests returns two comments (`stats/interval/interval.go:173`, `:189`) and no implementation, even though `Interval.method`'s godoc lists `"bootstrap"` as a recognized method name — a fourth schema promise the code does not keep. `docs/plans/2026-08-31-judge-calibrate.md` lands a percentile bootstrap in `stats/interval` for the same reason; whichever of the two merges first owns it and the second consumes it. If both are dropped, this plan lands it.

Condition 2 is load-bearing and Condition 1 alone is actively dangerous without it. Two Assets
with identical mean deltas that fix **disjoint** Cases are perfect complements: dropping either
loses half the gain. Under Condition 1 alone they are "equivalent" and one is thrown away. The
most expensive error this feature can make is exactly that one, so it takes two conditions to
make the claim and one to refuse it.

### The margin δ, argued rather than invented

`δ` is the effect size below which two Assets are treated as the same. It must not be a
constant chosen because it looked reasonable — the `kno eval inspect` plan had an invented 25%
threshold deleted on review, and this plan is not repeating it.

    δ = max( --redundancy-margin , MinDetectableEffect(|C|, domain, level) )

The second term is the argument: **you may not claim two things are equivalent to a resolution
finer than your sample can see.** `core/value.Plan.MinDetectableHarm`
(`core/value/route.go`) already carries exactly this shape for the harm test — the field is
verified, its computation site is ***(verify)*** — and the
`kno eval inspect` plan proposes moving the general form into `stats/interval` as
`MinDetectableEffect`. This plan **consumes** that function; if that plan has not landed, this
one lands it (one function, characterization-tested) — flagged so the dependency is a fact and
not a hope. The user-facing term defaults to **0**, which means "the sample's own resolution
decides", so the default behavior carries no invented number at all.

When `δ` would have to exceed a stated ceiling (`--redundancy-max-margin`, default 0.10 —
argued as "an effect a tenth of the Goal's range apart is not the same effect for any Goal this
tool has shipped") the verdict is `UNKNOWN` rather than a redundancy claim resting on a margin
so wide it would swallow the entire effect.

### Three verdicts, and which way the tool errs

Same discipline as `core/gaps.go`'s IMPROVED / GAP / UNKNOWN, and for the same stated reason —
"we did not really look" must not read like "we looked and found nothing":

| Evidence | Verdict | Action |
|---|---|---|
| `|C| ≥ MinOverlapCases`, Condition 1 and Condition 2 both hold | **REDUNDANT** | reject, with evidence |
| `|C| ≥ MinOverlapCases`, either condition fails | **DISTINCT** | select |
| `|C| < MinOverlapCases`, or `δ` would exceed the ceiling, or a delta vector is unrecoverable | **UNKNOWN** | **select**, and say so in the log |

**UNKNOWN selects.** The costs are asymmetric and not close. A missed redundancy costs budget —
a recoverable, visible, capped resource, and the user can re-run with a tighter cap. A false
redundancy silently deletes a real gain from the portfolio and the user has no signal that it
happened. The detector's posture is therefore *select unless proven redundant*, and every
UNKNOWN appears in the report's degraded-evidence line rather than vanishing into a selection.

`MinOverlapCases` reuses `core.MinClusterCases = 5` (`core/gaps.go:14`) — the repo already has
an exported, defended line between "we looked" and "we did not". Introducing a second, different
number for the same idea is the vocabulary drift `CLAUDE.md` forbids. If 5 is wrong it is wrong
in both places and moves in both.

### Multiplicity: the pairwise tests are not covered by today's correction

`portfolio.Correct(iv, nScreened)` (`stats/portfolio.go:146`) Bonferroni-corrects the keep/reject
intervals for the number of Assets screened. It does **not** cover the redundancy tests, which
are pairwise: greedy performs up to one test per (candidate, already-selected) pair.

The redundancy intervals are corrected for the **number of tests actually performed**, counted
during the run and recorded on the Portfolio (`n_redundancy_tests`, new additive field). The
worst-case bound `k(k−1)/2` was rejected: for a 500-Asset pool it corrects every interval to a
level so extreme that no pair is ever equivalent and the feature is decorative.

The honest caveat, stated rather than elided: the count of tests performed is itself a function
of the selection path, which is a function of the data — so the correction is data-dependent and
its guarantee is slightly weaker than a pre-registered one. Named in
`docs/what-the-numbers-mean.md`, ledgered, and dwarfed by the fact that a *wrong* correction
here removes real gains rather than admitting fake ones.

### Content similarity: demoted, not deleted

The shingle rule survives with a narrower job:

- **When measurement evidence exists** (`|C| ≥ MinOverlapCases` and both delta vectors are
  recoverable), measurement **decides**. Shingle overlap is computed and recorded as
  corroborating evidence, never as the verdict.
- **When measurement evidence does not exist** and both Assets are `KIND_KNOWLEDGE`, shingle
  overlap decides at the existing `0.6` threshold — **today's behavior, byte-for-byte
  unchanged**, so a `--pool`-driven Select over a store with sparse overlap produces the same
  Portfolio it does now.
- **It never decides for behavior Assets.** That is the A12 gap, and covering it with an
  instrument that cannot see the thing would be worse than leaving it open.
- `--pool` remains optional. Without it, content evidence is unavailable and `DegradedRules`
  still names `REDUNDANT` — but measurement evidence needs only the store, so a poolless Select
  now detects redundancy where today it detects none. This is a behavior change for existing
  users and belongs in the CHANGELOG as one.

### Cross-kind: replaced by within-**destination**

v0.1 refuses to compare across kinds. **v0.2 compares within destination.** The first draft
claimed this "subsumes" the kind rule; **that is false against the code** *(F3)*.
`selectedKnowledge` is appended (`core/select.go:335`) under a gate on `kindOf(v) ==
KIND_KNOWLEDGE` and **nothing about destination**, so the shipped rule already compares two
knowledge Assets pinned to *different* destinations — one to `CONTEXT`, one to
`KNOWLEDGE_BASE` — and a within-destination rule would **refuse** that comparison.
Within-destination is a widening on the behavior pair and a **narrowing** on that knowledge
pair. It is not a superset, and calling it one would have quietly made acceptance criterion 7's
byte-compatibility golden false.

The argument for it is mechanism, not taxonomy. Two Assets are substitutes when they compete for
the same vehicle and the same cap. `destinationFor` (`core/select.go:571`) already computes each
Asset's destination — the Asset's own when the pool supplied one, else the kind's home (behavior
→ tuning set, knowledge → context). Because the kind's home destination is the default, the new
rule reproduces the old one for every Asset the user has *not* overridden, and additionally
handles the case the kind rule gets wrong: a behavior Asset the user pinned to `CONTEXT`
(`Asset.destination`, `asset.proto` field 4; `user_overridden`, field 9) competes for
`--max-context-tokens` against knowledge Assets and is a genuine substitute for one.

**The two evidence paths therefore get different scopes, and the reason for the split is
compatibility rather than principle** *(F3)*:

- **Measurement evidence compares within destination.** The mechanism argument above is the
  argument for it; it is new behavior and has no compatibility claim to keep.
- **Content evidence keeps its shipped scope** — `kindOf(v) == KIND_KNOWLEDGE`,
  destination-blind, at `0.6`, exactly as `main` does it. That is what makes criterion 7's
  golden mean something. It is retained for the same reason accepted risk 5 retains the `0.6`
  itself, and not because a destination-blind shingle rule is right: it can still drop a
  knowledge Asset destined for a knowledge base because a context document duplicated its text,
  which is a mechanism claim the shingle rule has no evidence for. It does that on `main` today;
  narrowing it is a deliberate behavior change belonging to whichever plan re-derives the `0.6`.

Cross-**destination** comparison stays refused, on the argument `DESIGN.md` and
`common.proto`'s `Kind` godoc already make: ICL and fine-tuning are not interchangeable
mechanisms, so a document in context and a demonstration in the tuning set are not substitutes
even when they help the same Cases. Dropping a document because a training example covered the
same failures would be a mechanism claim the tool has no evidence for — and
`REJECTION_REASON_WRONG_MECHANISM` exists precisely because this project already decided that
question in the other direction.

### The greedy, the tie-break, and the winner's curse

Today: first-seen-wins, where "first" is `rankLess` (`core/select.go:625`) — `delta_per_cost`
descending, then `delta_goal` descending, then Asset ID. The kept duplicate is the one whose
**noisy** delta ranked higher. The select/export plan states this inherits the winner's curse
and it does.

Two changes, one substantive:

1. **Tie-break on cost, not on delta, once equivalence is established.** When Condition 1 has
   passed, the measurement *by construction* cannot separate the two Assets — so breaking the
   tie on `delta_goal` is breaking it on noise, and the noise is precisely the winner's-curse
   term. The equivalent pair is decided by **carrying cost ascending** (the cheaper Asset;
   `CostVector.context_tokens` for context, content bytes for a knowledge base, one example for
   the tuning set), then Asset ID. Deterministic, not a function of the noise the equivalence
   test just declared uninformative, and strictly better under every budget cap. The criterion
   that decided is recorded in the evidence so the choice is auditable.

   **But `context_tokens` is a biased ruler, and this is an elimination decision** *(F2)*.
   `docs/debt.md#68` states plainly that `CostVector.context_tokens` is the byte-based estimate
   and that "two Assets of equal real token cost can differ ~2.4x by content type alone" — which
   is why this plan's own `charge()` comment already excludes `ft_tokens` from a comparison.
   Sorting two *measurement-equivalent* Assets on that field does not break the tie randomly, it
   breaks it **systematically toward short and terse content**. The winner's-curse tie-break it
   replaces was wrong-because-noisy; an unguarded cost tie-break would be wrong-because-biased,
   and bias does not average out across runs. Using a field the codebase has already flagged to
   decide which of two equivalent Assets is deleted is arguably the worse of the two.

   **So cost decides only outside the bias band.** `decided_by = COST` requires the two carrying
   costs to differ by more than the factor `#68` itself names (2.4×) — a number imported from the
   ledger entry, not invented here. Inside that band the estimate cannot distinguish them and the
   tie falls through to `decided_by = ID`, which is deterministic and unbiased. A 180-vs-640-token
   pair (3.6×) is still decided by cost, which is the case the tie-break exists for; a
   180-vs-300-token pair is not, because the estimate cannot support the claim. When `#68` is
   repaid with real token counting the band collapses toward 1 and the guard becomes a no-op —
   which is this amendment's repayment trigger, recorded in the ledger rather than left to be
   noticed.
2. **The reported delta of the survivor is unchanged and still inflated.** Redundancy resolution
   does not launder the winner's curse: the kept Asset's delta was still selected on the dev
   slice, `portfolio.Correct` still applies, and `dev_estimated_gain` still carries the
   inflation `docs/what-the-numbers-mean.md` describes. The docs say resolving a duplicate pair
   by cost removes *one* noise-driven choice, not the selection effect.

**Non-transitivity, stated.** Redundancy is tested only against already-*selected* Assets, so it
is not an equivalence relation over the pool: A ~ B and B ~ C with A ≁ C produces an outcome
that depends on arrival order. v0.2 keeps first-seen-wins because it is deterministic and
reproducible (two runs over the same store still produce byte-identical Portfolios, which
`core/select.go`'s `decide` godoc promises), documents the non-transitivity, and reports the
full pairwise evidence in the log so a user can see the cluster the greedy walked through.
Clustering the pool into equivalence classes before selecting is a real alternative, considered
and rejected below.

### The rejection log, and how a user checks a claim they disagree with

`Rejection.detail` (`portfolio.proto:56`) is prose and stays prose, but the numbers move into a
typed field — the repo's standing preference, and prose parsed by a report renderer is a schema
by accident. New additive message, one entry per named Asset:

```
RedundancyEvidence {
  with_asset_id       string
  kind                MEASUREMENT | CONTENT_SHINGLE
  n_overlap           int32           // |C|
  paired_difference   double          // mean of d_A - d_B over C
  difference_interval Interval        // corrected; the TOST instrument
  margin              double          // delta, and which term produced it
  margin_source       SAMPLE_RESOLUTION | USER
  co_improvement      double          // J
  co_improvement_interval Interval
  co_improvement_floor double         // J_min, and which term produced it       (F1)
  co_improvement_floor_source CHANCE | USER                                   // (F1)
  shingle_overlap     double          // recorded whenever a pool was supplied
  cost_ratio          double          // the survivor's cost against the rejected's (F2)
  decided_by          COST | ID | CONTENT
}
```

`co_improvement_floor` carries `J_chance` on the default path, so a reader can see how much of
the observed co-location was coincidence before deciding whether they believe the claim *(F1)*.
`cost_ratio` makes the `#68` bias-band guard auditable: a `decided_by = ID` on an equivalent pair
with different costs is explained by the ratio sitting inside the band, not by a bug *(F2)*.

The prose detail becomes, for example: *"equivalent on 34 shared Cases (paired difference
+0.008, CI [−0.021, +0.037] inside ±0.05); improved the same Cases (J = 0.88, CI [0.71, 0.96], against 0.31 expected by
chance); kept the cheaper Asset (180 vs 640 context tokens, 3.6x apart — beyond the estimate's
known ~2.4x content-type bias)."* Every number a reader needs to disagree.

**And a way to actually check it.** `kno select --explain <asset-id>` — free, read-only, no
provider call, the `kno doctor` / `kno eval inspect` posture — prints the per-Case table over
the shared slice: Case ID, the baseline score, each Asset's delta, and whether each counted as
an improvement. A user who believes two Assets are complements can see the disjoint columns.
Without this, "these are duplicates" is an assertion, and this project does not ship
assertions.

## Acceptance criteria

Numbered, testable, each naming an observable.

1. Two behavior Assets with disjoint 3-gram shingle sets whose recorded per-Case deltas are
   equal and co-located over 20 shared Cases produce a `REDUNDANT` rejection with
   `redundant_with_asset_ids` naming the survivor. This fails on `main` today by construction
   (`kindOf(v) == KIND_KNOWLEDGE` gate, `core/select.go:441`) and is the plan's headline test.
2. Two behavior Assets with **identical mean deltas** over 20 shared Cases that improve
   **disjoint** Case sets are both **selected**, with `co_improvement` recorded at 0.0. Test:
   `TestComplementsWithEqualMeansAreNotRedundant`. This is the test that must fail if Condition
   2 is ever dropped.
3. Two Assets with 4 shared Cases (below `MinOverlapCases`) are both selected and the report's
   degraded-evidence line names the pair as `UNKNOWN`; no `REDUNDANT` rejection is emitted.
4. Shrinking the shared slice **increases** the number of `UNKNOWN` verdicts and never increases
   the number of `REDUNDANT` verdicts. Property test over seeded synthetic runs — this is the
   property a difference test (rather than an equivalence test) would violate, and it is the
   executable form of the plan's central statistical claim.
5. With `--redundancy-margin 0`, `δ` equals `MinDetectableEffect(|C|, domain, level)` exactly;
   the emitted `margin_source` is `SAMPLE_RESOLUTION`. With a user margin below that value, `δ`
   is still the sample resolution and `margin_source` is still `SAMPLE_RESOLUTION` — the user
   cannot buy a finer claim than the data supports.
6. A pair whose required `δ` exceeds `--redundancy-max-margin` yields `UNKNOWN`, both Assets
   selected, cause named.
7. Two knowledge Assets with 0.9 shingle overlap and **no** measurement overlap are decided by
   content at the existing 0.6 threshold, producing a Portfolio byte-identical to the one
   `main` produces for the same store and pool. Golden-file test — the compatibility guarantee.
   **The fixture must contain a knowledge pair pinned to two different destinations** (one
   `CONTEXT`, one `KNOWLEDGE_BASE`, `user_overridden = true`) *(F3)*: that pair is exactly what a
   within-destination content rule would silently stop comparing, so a fixture without it makes
   this criterion vacuous. Whether the existing golden fixture carries such a pair is unverified
   as of this plan; if it does not, extending it is part of this PR and is its first commit.
8. A Select run with `--pool` omitted now emits `REDUNDANT` rejections carried by measurement
   evidence, while `DegradedRules` still names `REDUNDANT` for the content half. Both facts
   asserted in one test.
9. A behavior Asset and a knowledge Asset that improve identical Cases with identical deltas are
   **both selected**; no cross-destination redundancy is claimed.
10. A behavior Asset with `destination = CONTEXT` and `user_overridden = true` **is** compared
    against knowledge Assets destined for context, and can be rejected `REDUNDANT` against one.
11. Two equivalent Assets whose carrying costs differ by more than the `#68` bias band (2.4×):
    the cheaper survives regardless of which had the higher `delta_goal`, and `decided_by` is
    `COST`. Test asserts the *more expensive, higher-delta* Asset is the one rejected — the
    winner's-curse reversal.
12. Two equivalent Assets with identical costs are decided by Asset ID and two runs over the
    same store produce byte-identical Portfolios (`decided_by = ID`).
13. Every `REDUNDANT` rejection carries at least one `RedundancyEvidence` with a non-nil
    `difference_interval`; a rejection without one fails the test that enumerates them — the
    `docs/debt.md#1` discipline ("no delta without its interval") extended to this claim.
14. `n_redundancy_tests` on the Portfolio equals the number of pairwise tests performed, and
    every `difference_interval.level` equals `1 − (1 − level)/n_redundancy_tests`.
15. `kno select --explain <asset-id>` prints the per-Case table for every Asset named in that
    Asset's redundancy evidence, exits 0, and makes zero provider calls (asserted by a
    transport that fails on any request).
16. `--json` carries the evidence structurally, not as parsed prose; the rendered `detail`
    string contains the same numbers.
17. Select still makes no LLM call, constructs no `Agent`, and creates no budget guard: asserted
    by the same transport guard, so the "pure function of the store" property is a test, not a
    comment.
18. **Select reads `Measurements` only for the gated Value run and `CaseScores` only for that
    run's recorded `baseline_run_id`** *(F5)*. The canary fails on any other run ID, including a
    Validate run's, and `--explain` reads the same two runs.

    This criterion was written as "the holdout canary still passes", and the implementation
    workstream stopped rather than satisfy it, correctly, because it was false in both
    directions.

    *Downward:* the canary forbade `CaseScores` **by name**, and this plan needs `CaseScores` to
    reconstruct per-Case control deltas under `PAIRING_SCHEME_RECORDED_BASELINE` — the default
    here, since `AssetRouting.FreshControlArm` is false under `ModeAllDev`. Satisfying the
    criterion as written would have meant editing a holdout-isolation test until something
    passed. That is the one edit this repository must never make casually.

    *Upward, and worse:* `Measurements` was **not** forbidden, and `kno validate` writes holdout
    results there (`core/validate_loop.go`'s `RecordMeasurement`, read back by
    `core/validate_measure.go`). So Select gaining a `Measurements` reader — which this plan
    also requires — would have acquired a holdout-capable reader **while the canary went
    green**. The criterion's guarantee was vacuous exactly where it had just started to matter.

    The fix is not in this plan's scope and landed separately (#171): the canary is scoped to a
    run ID rather than a method name, which is strictly stronger than what it replaced, since
    `Measurements` was previously unguarded. Implementation of this plan **depends on #171** and
    must seed a `baseline_run_id` on the canary's Value run rather than relax the guard.

    The reason a run-scoped guard is sufficient — and it belonged in this plan from the start,
    since nothing enforced it and it was carried only in reviewers' heads: Baseline takes a
    `*SealedEvals` (`core/baseline.go`), and Select gates its source run to `STAGE_VALUE`. The
    seal is what keeps holdout Cases out of the Value run's measurement rows in the first
    place; the canary's job is to catch a reader that reaches **past** that run, which is a
    foreign run ID and nothing else.
19. A store whose measurement rows are `Unrecoverable` (purged) for one Asset yields `UNKNOWN`
    for every pair involving it — never a delta computed against a zero standing in for a
    missing number (`store.RecordedMeasurement.Unrecoverable`).
20. Benchmark: redundancy testing over a 500-Asset / 400-Case store stays within the
    `make bench-diff` budget, with the per-Asset delta vectors streamed rather than materialized
    for all Assets at once.
21. **Condition 2's floor is `J_chance`, not a constant** *(F1)*. With
    `--redundancy-min-coimprovement 0`, a pair whose observed J is high but whose corrected
    bootstrap CI does not clear `J_chance(|I_A|, |I_B|, |C|)` is **not** `REDUNDANT`, and the
    evidence records `co_improvement_floor_source = CHANCE`. A pair with the same observed J over
    *smaller* improvement sets — where `J_chance` is lower — is. Both directions in one test, and
    a grep over `core/` proves no numeric co-improvement constant exists to be tuned.
22. Two Assets that each improve every Case in `C` yield `UNKNOWN` (`J_chance` = 1), and a pair
    whose `I_A ∪ I_B` is empty yields `UNKNOWN`. Neither is ever `REDUNDANT` *(F1)*. A user floor
    above `J_chance` is honored and recorded as `co_improvement_floor_source = USER`.
23. Two equivalent Assets whose `context_tokens` differ by **less** than 2.4× are decided by
    `ID`, not `COST`, whichever is nominally cheaper, and `cost_ratio` is recorded *(F2)*. The
    test names `docs/debt.md#68` so deleting the guard fails a test that explains itself.
24. **Firing frequency is measured, not assumed** *(F4)*. A characterization test records δ,
    `J_chance` and the verdict for a synthetic store with a known-duplicate pair at
    |C| ∈ {5, 10, 20, 40}, and the PR description carries that table plus the observed
    `REDUNDANT` / `UNKNOWN` split over a quickstart-shaped fixture. No frequency claim ships as a
    gate; the numbers are pinned so that a future routing change which improves them is visible
    as a diff.

## Alternatives considered

**A. Embedding similarity over the pool.** Rejected, on three independent grounds.
*(i) Cost and architecture.* Embedding via a provider makes Select a spend path: it would need a
`budget.Guard`, a consent quote, checkpointing and resume — none of which Select has, because
`SelectOptions` (`core/select.go:31`) holds a Store, a Pool and a Budget and nothing that can
call a provider. Its godoc's "pure function of what the store holds" would become false, and a
Select that can be budget-stopped is a different stage.
*(ii) Dependency.* A local embedding model is a heavyweight new dependency in a binary whose
design constraint is "no torch in the OSS binary" (`DESIGN.md:134`), and would need a
justification under the new-dependency rule that "it makes a similarity number" does not meet.
*(iii) It is off-target, in both directions, on exactly the class it was proposed for.* Two
paraphrases of one demonstration are redundant and embed close (fine). Two demonstrations with
near-identical prose and *different* tool sequences are not redundant and embed close (false
positive). Two demonstrations of one pattern over different domains are redundant and embed
apart (false negative). Semantic similarity is a better content signal than shingles and it is
still a content signal.

**B. Structural comparison of tool-call sequences.** The most attractive rejected option, and
rejected on a fact rather than a preference: **an `Asset` is `bytes content`**
(`asset.proto` field 2) with no structure at all. `ToolCall{name, arguments, result, error}` is
real and structured (`case.proto`), but it lives on `Case.history` and `Response.tool_calls` —
the *evaluation* side — not on Assets. Structural comparison of Assets therefore requires
declaring and parsing a behavior-asset format, which is a pool-format promise this plan is not
scoped to make. It becomes available the day the tuning-set JSONL shape the select/export plan
pinned (OpenAI chat format) is an *input* format as well as an export format; named as the
v0.3 upgrade, with the note that even then it answers "same shape of demonstration", which is
still not "buys nothing extra".

**C. Difference test instead of equivalence test** ("reject as redundant when the paired
difference CI contains zero"). Rejected: it makes redundancy *more* likely the less evidence
there is, so the least-measured pairs would be the most confidently merged. Acceptance
criterion 4 is the executable form of this rejection.

**D. Cluster the whole pool into equivalence classes before the greedy runs**, then select one
representative per class. Genuinely better on non-transitivity, and rejected for v0.2 on two
grounds: it needs the full pairwise matrix (quadratic in a pool the streaming design is built
to avoid materializing — `iter.Seq` is load-bearing per `CLAUDE.md`), and clustering under a
non-transitive relation requires a linkage choice (single/complete/average) that is a third
invented parameter. First-seen-wins with the class visible in the log gives the user the
information without the parameter. Named as the upgrade path.

**E. Leave A12 open and ship nothing for behavior.** Rejected: the tuning-set cap
(`--max-training-examples`) is the budget most sensitive to duplication, and a fine-tuning set
of near-identical demonstrations does not just waste slots, it over-weights one pattern in the
training distribution — a harm that outlives the run.

**F. Measure A+B jointly to get true marginal contribution.** The correct answer to the question
and out of scope: it is a Value-stage change with combinatorial cost, and it is the reason this
plan's claim is worded as "equivalent and co-located" rather than "buys nothing". Ledgered with
a trigger.

## Affected packages

`core/` (`select.go` — the redundancy rule, the tie-break, evidence construction; a new reader
that reconstructs per-Case delta vectors from `store.Measurements` + `store.CaseScores`),
`stats/interval` (`MinDetectableEffect` if the `kno eval inspect` plan has not landed it — that
plan is unmerged as of this writing; the percentile bootstrap; the
paired-difference and co-improvement intervals), `stats/portfolio` (correction over the pairwise
test count), `store/` (**no interface change** — `Measurements`, `CaseScores`, `Valuations`,
`GetRun` are all existing methods; verified against `store/store.go`), `proto` (the additive
message and fields below), `cli/` (`kno select --explain`, `--redundancy-margin`,
`--redundancy-max-margin`, `--redundancy-min-coimprovement` *(F1)*, the rendered detail,
`--json`), `docs/`
(`what-the-numbers-mean.md`, `select-a-portfolio.md`, `export-a-tuning-set.md`, mental model),
`docs/debt.md` (A12 repaid; three new entries).

## Proto / schema impact

**Additive only.** Verified against `proto/kno/v1/portfolio.proto` and `valuation.proto`:

| Change | Where | Field number |
|---|---|---|
| `message RedundancyEvidence` | `portfolio.proto` (new message) | n/a |
| `enum RedundancyEvidenceKind` (`MEASUREMENT`, `CONTENT_SHINGLE`) | `portfolio.proto` | n/a |
| `enum RedundancyDecidedBy` (`COST`, `ID`, `CONTENT`) | `portfolio.proto` | n/a |
| `enum CoImprovementFloorSource` (`CHANCE`, `USER`) *(F1)* | `portfolio.proto` | n/a |
| `repeated RedundancyEvidence redundancy_evidence` | `Rejection` | **6** (1–5 used) |
| `int32 n_redundancy_tests` | `Portfolio` | **11** (1–10 used) |

`RejectionReason` needs no new value: `REJECTION_REASON_REDUNDANT` (`valuation.proto`) already
exists and already documents `redundant_with_asset_ids`. `Interval` is reused as-is, including
`n_pairs` — which carries `|C|` for the difference interval and makes the overlap size visible
to any consumer without reading the evidence message. No cardinality change to any existing
field, so `buf breaking --against main` (`make typecheck-proto`) passes. No new `Event` member
— redundancy is part of the `PortfolioSelected` decision, not a separate user-visible state, so
adding an event would be the side channel `CLAUDE.md` forbids in the other direction.

## Edge cases

| Case | Behavior |
|---|---|
| `|C| = 0` (disjoint routing) | `UNKNOWN`; both selected; named in the degraded line |
| `|C|` between 1 and 4 | `UNKNOWN`; no interval is computed at all below 2 pairs |
| One Asset untagged (routed to the whole dev split), one tagged | `C` is the tagged Asset's slice; the untagged Asset's deltas are read over exactly that subset. The `n_routed` scaling (`docs/debt.md#65`) is **not** applied inside the comparison — it is a reporting transform, and applying it here would compare a scaled quantity to an unscaled one |
| Both Assets untagged | `C` is the whole dev split; the largest, cleanest comparison the run can make |
| Delta vectors of different trial counts | Trials are averaged per Case before differencing, matching `interval.PairedTrials`; a Case present in one Asset and not the other is outside `C` |
| Purged store (`Unrecoverable` scores) | Those Cases leave `C`; if `|C|` drops below the floor the verdict is `UNKNOWN`. Never a zero standing in for a missing number |
| Asset measured but `not_measured` set (IRRELEVANT / UNDERPOWERED / …) | Never a redundancy operand — an unmeasured Asset has no delta vector. It is already rejected with Value's own reason |
| Three mutually equivalent Assets | Two rejected against the first; both evidences recorded; the log shows the cluster |
| A ~ B, B ~ C, A ≁ C | First-seen-wins; documented non-transitivity; all pairwise evidence in the log |
| Equivalent pair, identical cost and identical delta | `decided_by = ID`; deterministic |
| Equivalent pair whose costs differ by less than the `#68` bias band (2.4×) | `decided_by = ID`; `cost_ratio` recorded. The estimate cannot tell them apart, so it does not get to choose which one is deleted *(F2)* |
| `I_A ∪ I_B` empty — neither Asset improved anything on `C` | J undefined; `UNKNOWN`; both selected *(F1)* |
| Both Assets improve (nearly) every Case in `C` | `J_chance` → 1, no interval can clear it, so `UNKNOWN`. Co-location carries no information when everything is co-located *(F1)* |
| Two knowledge Assets in **different** destinations with high shingle overlap | Compared by content exactly as `main` compares them — the content path stays destination-blind for byte-compatibility; measurement evidence does not cross destinations *(F3)* |
| Equivalent pair across destinations | Not compared; both selected |
| Knowledge pair with measurement overlap **and** high shingle overlap that **disagree** | Measurement decides; the shingle value is recorded, and the disagreement is visible in the evidence. This is a real and interesting state, not an error |
| No `--pool` | Measurement evidence still runs; content evidence unavailable; `DegradedRules` still names `REDUNDANT` |
| `--value-run-id` from a `BUDGET_STOPPED` run under `--allow-partial` | Redundancy runs over whatever was measured; the `UNKNOWN` count will be high and the source status already travels on the Portfolio |
| Goal domain `CONTINUOUS_UNBOUNDED` | `I_X` uses a per-Case noise floor derived from the same sample resolution as `δ`; for an unbounded domain the max-margin ceiling is expressed as a fraction of the observed delta scale, and when that cannot be formed the verdict is `UNKNOWN` |
| Goal direction MINIMIZE | "Improvement" is sign-corrected by `Run.goal_direction` before `I_X` is formed. A test drives a MINIMIZE Goal end to end, because getting this backwards inverts every co-improvement set silently |
| Holdout | Untouched. Select reads dev-side Valuations and Measurements; `--explain` reads the same rows |

## Test plan

What must fail if it regresses:

- **The A12 test** (criterion 1): behavior-asset redundancy detected with zero shingle overlap.
  Verified failing on `main`.
- **The complements test** (criterion 2): equal means, disjoint improvements, both selected.
  Verified failing against a Condition-1-only implementation before Condition 2 is written.
  This is the test that protects against the plan's most expensive possible error.
- **The monotonicity property** (criterion 4): less overlap ⇒ more `UNKNOWN`, never more
  `REDUNDANT`. Property test (rapid/gopter, per `CLAUDE.md`'s `stats/` rule) over seeded
  synthetic stores with known ground truth.
- **Equivalence-test correctness**: the TOST decision matches a reference implementation over a
  table of (difference, half-width, δ) triples, including the three boundary alignments
  (interval inside, straddling one edge, entirely outside).
- **The chance floor** *(F1)*: `J_chance` matches a closed-form reference over a table of
  (a, b, n); a high-J pair that fails to clear its own chance level is not `REDUNDANT`; the two
  degenerate cases (empty union, everything co-improved) are `UNKNOWN`; and a grep asserts no
  co-improvement constant exists in `core/`.
- **The cost bias band** *(F2)*: the 2.4× guard, both sides, with the `#68` reference in the test
  name so the reason survives the next reader.
- **Cross-destination content compatibility** *(F3)*: the criterion-7 fixture carries a knowledge
  pair in two destinations, and a mutation that makes the *content* path destination-aware breaks
  the compatibility golden — which is the test proving the split is load-bearing.
- **Winner's-curse reversal** (criterion 11): the higher-delta, more expensive Asset is the one
  rejected. Verified failing against the shipped `delta`-then-`id` tie-break.
- **Compatibility golden** (criterion 7): a knowledge-only store with no measurement overlap
  produces a Portfolio byte-identical to `main`'s. Regenerated with `make update-golden` only
  when a change is intended.
- **Determinism**: two `decide` runs over the same store produce byte-identical Portfolios,
  including evidence ordering. The existing determinism goldens are extended, not replaced.
- **Multiplicity**: `n_redundancy_tests` matches the count, every evidence interval carries the
  corrected level, and a simulated-screen FDR test over synthetic ground truth shows the
  redundancy false-positive rate at or below the corrected level — the second half of the
  two-part evidence the select/export plan established for the winner's curse.
- **Purity**: no provider call, no guard, no `Agent` in Select or `--explain`.
- **Holdout canary**: the existing seal test extended over the new store readers.
- **CLI**: `--explain` golden output, `--json` shape, help snapshots, exit codes.
- **Bench**: `make bench-diff` over the 500×400 store; the streaming shape is a benchmark, not
  an assertion in prose.
- **`make selftest`**: the redundancy gate fails when Condition 2 or the multiplicity correction
  is removed.

## Rollback

Reverting `core/select.go` to the shipped `redundantWith` restores today's behavior exactly.
The proto additions are additive and unreferenced by anything that predates them: a Portfolio
written by the new build decodes in the old one with the evidence ignored, and a Portfolio
written by the old build decodes in the new one with an empty evidence list — which the
renderer must treat as "no evidence recorded" rather than "no redundancy", and a test pins that
reading. `store` is unchanged, so no migration exists to roll back. `kno select --explain`
deletes cleanly.

## Docs impact

- **`docs/what-the-numbers-mean.md`** — a new section, *What a redundancy claim claims*: that it
  is an inference from two independent measurements and **not** a measurement of marginal
  contribution given the other Asset; the three verdicts; that UNKNOWN selects and why the costs
  are asymmetric; the equivalence margin and why it is bounded below by the sample's own
  resolution; that co-improvement is judged against the co-location two unrelated Assets would
  show by chance rather than against a fixed number *(F1)*; that a duplicate pair is resolved by
  cost only where the cost estimate's known content-type bias cannot flip the ordering *(F2)*;
  that on a default run many pairs will be `UNKNOWN` because routing does not aim for overlap
  *(F4)*; the data-dependent multiplicity caveat; and that resolving a duplicate pair by
  cost removes one noise-driven choice, not the winner's curse.
- **The two cookbook recipes now live in `uknoAI/kno-examples`, not in this repo.** #163 moved
  the cookbook out and left one-line tombstones behind, after this plan was written. Editing
  `docs/cookbook/select-a-portfolio.md` or `docs/cookbook/export-a-tuning-set.md` in place would
  overwrite a tombstone and be caught by `scripts/cookbook-stub-check.sh`, which asserts each is
  one line and one link. The recipe changes are a **companion PR against
  `uknoAI/kno-examples`**, opened from the same branch-point and linked from this PR's body:
  - `recipes/select-a-portfolio.md` — reading a redundancy rejection, and `kno select --explain`.
  - `recipes/export-a-tuning-set.md` — behavior redundancy is now detected; what that does to
    the example cap.

  This is the second workstream to be stopped by a cookbook path that was accurate when its plan
  was written. The general rule, worth stating once: **a plan's docs-impact paths are resolved at
  implementation time, not trusted from the plan.** Any path a plan names may have moved between
  Phase 0 and Phase 2, and in a repo that migrates docs to sibling repositories it will.
- **`docs/mental-model.md`** — the Select stage's rule list.
- **`CHANGELOG.md`** under `Unreleased`, flagged as a behavior change: a poolless Select now
  emits `REDUNDANT` rejections it did not emit before — and saying plainly that under default
  routing many pairs will report `UNKNOWN` rather than a redundancy verdict *(F4)*.
- **`docs/debt.md`** — the select/export plan's A12 accepted risk ("Within-kind redundancy only.
  Behavior-asset redundancy is a v0.2 question") is repaid and struck through in place; three
  new entries added.
- **`tapes/quickstart.tape` must be re-recorded.** The tape runs `kno select --value-run-id
  qs-value --pool pool.jsonl ...` at line 98, so any change to Select's rendered output changes
  the README GIF — the Definition of Done's vhs clause applies to this PR. (An earlier draft of
  this section asserted the opposite; the tape was checked and it does not hold.)

## Accepted risks

Each mirrored to `docs/debt.md` with a trigger.

1. **Substitutability is inferred, not measured.** Nothing measures A and B jointly, so a pair
   that is individually equivalent and co-located might still be complementary in combination.
   Stated in the docs in those words. *Trigger: when the Value stage gains group measurement
   (the proxy-FT bridge's group ablations, `DESIGN.md` Tier 3, already measure sets).*
2. **Redundancy needs measurement overlap, and routing does not aim for it.** Two genuinely
   duplicate Assets routed to disjoint clusters are `UNKNOWN` forever, no matter how much is
   spent. Routing could reserve a shared probe slice; it does not, and this plan does not change
   routing. *Trigger: when routing next changes, or before 1.0.*
3. **The multiplicity correction is data-dependent** (the test count depends on the selection
   path). Weaker than a pre-registered correction; conservative in the direction that admits
   fewer redundancy claims. *Trigger: if a redundancy false-positive is ever reported.*
4. **Non-transitivity is documented, not fixed.** First-seen-wins over a non-transitive relation
   is order-dependent; deterministic, reproducible, and not optimal. *Trigger: when a pool with
   more than ~10 mutually-near-equivalent Assets is observed in practice, or when clustering
   lands.*
5. **The shingle fallback keeps its unargued 0.6.** Retained deliberately for byte-compatibility
   with shipped behavior rather than re-derived here; the measurement path is what this plan
   defends. *Trigger: when content evidence next decides a rejection that a user disputes, or at
   1.0.*
6. **`MinOverlapCases` is borrowed from `core.MinClusterCases = 5`.** Right vocabulary, and
   possibly the wrong number for this use — the two answer the same question ("did we look?")
   about different quantities. *Trigger: with the first characterization of equivalence-test
   power at n = 5.*
7. **The cost tie-break still reads a biased estimate** *(F2)*. `CostVector.context_tokens` is
   byte-based and `docs/debt.md#68` records a ~2.4× content-type spread. The bias band confines
   it — cost decides only where the spread cannot flip the ordering — but inside the band a real
   cost difference goes undetected and the tie falls to Asset ID, which is arbitrary rather than
   cheap. Accepted: arbitrary is unbiased, and biased elimination is the failure this feature
   cannot afford. *Trigger: when `#68` is repaid with real token counting, at which point the
   band collapses and this guard is deleted.*
8. **Under default routing the feature will often report `UNKNOWN`, and the plan should have said
   so** *(F4)*. `MinOverlapCases` = `core.MinClusterCases` = 5 and `value.DefaultMinSample` = 5
   (`core/value/route.go:238`) combine so that a typical routed-slice *intersection* sits at or
   barely above the floor, where an equivalence test has almost no power — which pushes δ up
   toward the `--redundancy-max-margin` ceiling and lands the pair on `UNKNOWN`. That is the
   correct and safe outcome (UNKNOWN selects), but it means the tuning-set duplication cost this
   whole feature is justified on will frequently go undetected on a default run, and the first
   draft justified the feature without ever stating that consequence. Stated here, in the
   CHANGELOG entry, and in `what-the-numbers-mean.md`. No power table is asserted: producing one
   needs the `MinDetectableEffect` implementation this plan consumes, so it is **measured then
   pinned** (acceptance criterion 24) rather than guessed — the same measure-then-pin discipline
   `docs/plans/2026-08-30-kno-demo.md` applies to its wall-clock gate. *Trigger: with the first
   characterization of equivalence-test power at n = 5 (shared with risk 6). If the measured
   `REDUNDANT` rate on a realistic store is near zero, the answer is routing support (risk 2),
   not a wider margin.*

## Implementation-phase addendum (2026-09-01)

Three interpretation calls the implementation had to make that this plan left open, recorded here
per the same discipline as the Phase-1 amendments above — decided in the code, but the argument
belongs where a reviewer reading the plan will look for it, not only in a handoff message.

**Criterion 13 is scoped to `MEASUREMENT` evidence.** "Every REDUNDANT rejection carries at least
one `RedundancyEvidence` with a non-nil `difference_interval`" reads, taken alone, as a requirement
on every REDUNDANT verdict regardless of which evidence path decided it — and criterion 7's
compatibility fixture is a pure content-decided REDUNDANT verdict with no measurement overlap at
all, for which no `difference_interval` can exist (Condition 1's paired-difference machinery needs
a measured delta vector on both sides). The two criteria are reconciled by reading criterion 13 as
the docs/debt.md#1 discipline ("no delta without its interval") applied to what it protects: a
**delta claim**. `RedundancyEvidence.paired_difference` is a delta; `shingle_overlap` is not — it
asserts textual similarity, not a measured effect — so a `CONTENT_SHINGLE` evidence entry makes no
claim that entry #1's discipline has anything to guard. `core/redundancy.go`'s
`evaluateRedundancyForCandidate` enforces this by construction (a `CONTENT_SHINGLE` evidence is
only ever built without a `DifferenceInterval` field set), and
`TestRedundantBehaviorAssetsWithDisjointShingles` / `TestContentPathIsDestinationBlindAcrossDestinations`
pin both halves: `MEASUREMENT` evidence always carries a non-nil interval, `CONTENT_SHINGLE`
evidence never does.

**Condition 1's TOST is computed under `SCORE_DOMAIN_CONTINUOUS_UNBOUNDED`, never the Goal's own
declared domain.** The quantity under test, `d_with(c) − d_this(c)`, is a difference of two
already-sign-corrected per-Case deltas, not a raw paired score — for a `SCORE_DOMAIN_BINARY` Goal,
each delta itself lives in `{-1, 0, +1}` (treatment score minus baseline score), so their
difference lives in `{-2, -1, 0, +1, +2}`, a five-point set a binary-domain interval method has no
warrant to treat as a McNemar-style discordant-pair problem. `stats/interval.compute` dispatches
`adjustedWald` — the paired-binary method — only when `domain == SCORE_DOMAIN_BINARY`, and that
method's variance formula is derived from differences constrained to `{-1, 0, +1}`; handing it a
`{-2,...,+2}`-valued series would still run without error (it only inspects the SIGN of each
difference, discarding magnitude) but would silently be answering a different, narrower question
than "is the mean difference within `±δ`". `SCORE_DOMAIN_CONTINUOUS_UNBOUNDED` routes to
`interval.paired`'s t-interval-with-`signBound`-fallback path unconditionally, which makes no
assumption about the value range and is the correct instrument for a bounded-but-not-binary
difference series regardless of the Goal's own domain. This choice is unconditional — it does not
change per Goal — so a `CONTINUOUS_UNBOUNDED`-domain Goal's redundancy comparisons go through the
identical code path as a `BINARY`-domain Goal's, which is what keeps the redundancy rule
kind-and-domain-agnostic as designed. See the finding this addendum resolved:
`core/redundancy_test.go`'s `TestRedundancyMonotonicityUnderShrinkingOverlap` and
`docs/debt.md#165` show that this same `signBound` fallback has a real, separate small-sample
weakness — orthogonal to the domain question, present regardless of which domain routes into it.

**The cost tie-break's ledger trigger (`docs/debt.md#156`) is real, not "someday".** It is stated
as "when `docs/debt.md#68` is repaid with real token counting" — `#68` is itself an open, tracked,
FIRED entry (fired 2026-08-29 by the Hugging Face adapters PR, per its own row), not a hypothetical
future event; its repayment is a concrete PR that replaces the byte-based `context_tokens` estimate
with a real tokenizer count, and `#156`'s trigger fires automatically the day that PR lands, because
`redundancyCostBiasBand`'s 2.4x band is *defined* as the spread `#68` measures — repaying `#68`
mechanically collapses the band toward 1 and the guard becomes checkable dead code, not a judgment
call someone has to remember to revisit.
