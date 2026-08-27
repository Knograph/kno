# Value: measuring what each Asset is worth

**Status:** Phase 0, draft 4 — three Phase 1 passes, all **BLOCK** (5 P0s, 4, then 3). Every finding verified; all upheld. Pass 3 worked the estimator algebra directly and **the fresh-control-arm design survives** — what did not survive was the *scope* of its rule.
**Stage:** Value — DESIGN.md §2, the differentiated step
**Repayment triggers coming due:** [#1](../debt.md#1) ("construction-time invariant when the Value stage lands"), [#55](../debt.md#55) ("the first stage that consumes a Baseline as a reference"), [#58](../debt.md#58) (`Capabilities()` gets its first caller)

## 1. Problem

Baseline answers "how good is my agent." Value answers the question the product exists for: **which of my data earns its place, and what does it cost.**

The schema looked further along than the code, and draft 1 took that at face value. It is not: `valuation.proto` defines a `Valuation` that Value cannot fully write, `event.proto` has **zero** Asset-scoped payloads, and the `outcomes` table is keyed in a way that would silently discard 99.5% of a Value run's measurements. Those are §2.0.

### 1.1 The cost crux

Naively, Value costs `assets × dev_cases × trials`. 200 Assets against 1,000 dev Cases at one trial is **200,000 calls** — two orders of magnitude past the run that produced the baseline.

Everything below is shaped by that number. DESIGN.md's answer is routing, and routing is right — but **routing is a heuristic that happens to reduce work, not a bound.** A stage that spends the user's money needs a bound the user sets.

### 1.2 What is actually missing

| Piece | State |
|---|---|
| `Case.tags`, `Asset.tags`, `CostVector`, `Interval`, `RejectionReason` | Exist and mean what this plan needs |
| `core.Pool`, `core.ContextInjector`, `core.KnowledgeInjector` | Interfaces exist; **nothing implements the injectors** |
| `Valuation` carrying a rejection reason | **Missing.** Fields 1–14, none is a `RejectionReason` |
| Asset-scoped events | **Missing entirely.** All 12 payloads are Case- or Run-scoped |
| A durable home for per-`(asset, case)` measurements | **Missing.** `outcomes` is `PRIMARY KEY (run_id, case_id)` |
| A `Valuation` writer on `Store` | **Missing** |
| `stats/interval` | **Missing.** Prime directive 5 forbids a delta without a CI |
| `adapters/pool/*`, `judge/` | **Empty packages** |
| A per-Case score reader | **Missing.** `ScoreSum` aggregates only |

## 2. Design

### 2.0 Three structural blockers, found in review

**(a) The store cannot hold a Value run.** `outcomes` is `PRIMARY KEY (run_id, case_id)` (`store/sqlite.go:60`) and `RecordOutcome` uses `INSERT OR IGNORE` (`:622`), documented as idempotent on that key. Value measures the same Case against many Assets in one Run. Concretely: 200 Assets × 50 Cases → the first Asset writes 50 rows and **each of the remaining 9,950 measurements is silently discarded**, `CompletedCases` reports all 50 Cases done, and a resume skips the entire run.

Value needs a `measurements` table keyed `(run_id, asset_id, case_id)` — `schemaVersion` 3 — and the `Valuation` is **assembled from what is durably recorded**, which is the pattern `6bb14a8` established for `CaseExecution`. That also fixes draft 1's resume bug (below).

**And the new table must be visible to the readers that assume `outcomes` IS the run's record — which draft 2 missed, and which is a double-spend.** `SettledSpend` sums `outcomes` plus the run row's orphan columns and nothing else, and `store.go` calls it *"the only durable record of money spent. Without it a resumed run starts at zero and can spend its cap a second time."* A Value run writing 9,950 paid measurements into `measurements` and zero rows into `outcomes` returns **`SettledSpend = 0`**: kill it after $8 of a $10 cap, resume, and the guard is reseeded at zero and authorizes another $10.

Worse, draft 2's test for exactly this — *"resume re-pays nothing, asserted against `SettledSpend`"* — would have **passed against a method that structurally cannot see the spend it asserts on**. `0 == 0`. That is the same class of un-failable test that Phase 3 caught four times in M2-11, arriving this time in a plan.

**Orphan spend needs the same treatment**, and draft 3 omitted it: `SettledSpend` reads `outcomes` **plus the run row's orphan columns**, which exist precisely because money settled for a Case that never produced an outcome would otherwise be invisible (migration 2). A *measurement* billed but never recorded — process death between settle and write — reappears as the double-spend this whole section exists to prevent. V-4a reads all three sources; V-4c attributes orphan spend the way `core/baseline_record` does.

`CompletedCases`, `OutcomeCounts`, and `ScoreSum` are the same shape: all three return empty or zero for a Value run, so whatever resume consults skips nothing. **V-4a therefore ships the readers, not just the table**: `SettledSpend` gains the measurements source in the same statement (the pattern its own comment already describes for two sources), and a `CompletedMeasurements` reader exists and is what V-4c's resume consults. The resume test becomes **end-to-end** — kill, resume, assert total provider calls across both processes equals the single-process count — because an equality on `SettledSpend` cannot catch this.

[Debt #22](../debt.md#22) is re-dated here: it accepted `CompletedCases` loading the full set into memory at Baseline's scale, and the Value set is `assets × sample` — two orders of magnitude past the bound that entry accepted.

**(b) The proto is not done, so "proto first is satisfied" was false** and the parallel decomposition rested on it. Three gaps: `Valuation` has no field for the `RejectionReason` §5 promises to record (carrying it in Select's `Rejection` would hand Select's rejection log to the wrong stage); there are no Asset-scoped events, so Value's user-visible state has nowhere to go and CLAUDE.md's parallelization table gates cli+tui on "event schema fixed"; and the sampling seed has nowhere to live, so a seeded sample is unauditable and unreproducible across a resume.

**(c) Draft 1's resume story was a money bug.** It said a cost cap mid-Asset writes no `Valuation` and the resume re-measures that Asset from scratch — so with `--cases-per-asset 50` and a cap binding at Case 40, the resume pays for 40 Cases again, and if the cap still binds it never completes one. With durable per-measurement rows, "no partial Valuation" is achieved by not writing the *Valuation* while the paid measurements survive, and resume is free. That is what "the recorded outcome IS the marker" exists for.

### 2.1 Paired differences, and when the control arm must be fresh

```
δ_i = dir × (score_treatment(case_i) − score_control(case_i))
Δ   = mean(δ_i)
```

**`dir` is not decoration.** `valuation.proto:89` says *"Sign is relative to the Goal's own direction"*, and `Goal.Direction`'s own godoc says why: *"a −0.03 is an improvement for a latency Goal and a regression for an accuracy one."* Draft 2's formula omitted it, so every `DIRECTION_MINIMIZE` Goal would have shipped inverted — `delta_per_cost` ranking the most harmful Assets first, `REJECTION_REASON_REGRESSION` assigned to improvements, and the interval needing negate-**and-swap** or `low > high`.

**Where `score_control` comes from is the central decision, and it depends on how the Case was chosen.**

- **If Case selection did NOT condition on the baseline outcome** — tag overlap, a random sample, the un-routed control slice — the recorded baseline score is a valid control. Reuse it; Baseline already paid for it.
- **If selection DID condition on the baseline outcome** — routing to the Cases the baseline failed — the recorded score **must not** be reused, and a fresh control draw is measured on the same Cases.

The reason is regression to the mean. Selecting Cases where the recorded baseline scored 0 selects on that baseline's own random draw, so for a null Asset with per-Case success probability *p*, `E[δ | baseline = 0] = p`. **At p = 0.7 a completely inert Asset measures Δ = +0.70 with a tight interval.** Reusing the selecting draw as the control is what creates the bias; conditioning on the outcome is not itself invalid.

Draft 2 concluded from this that routing may never read baseline scores, and rejected failure-routing outright. That was one step too far, and it cost the design its cost model (§2.3). **Measuring a fresh control arm on the routed slice removes the bias entirely**: `X'` and `Y` are both fresh draws from the same conditionally-selected set, so `E[δ] = 0` under the null. Between-Case difficulty variance is still removed, since both arms run the same Cases. The price is 2× on the routed slice — and the routed slice is the small one, which is exactly the arithmetic DESIGN.md already budgets.

**`δ_i` under `--trials k` is the mean of the k draws in each arm**, so `δ_i` takes *k+1* values and the two arms get *k* draws each. Draft 3 shipped the flag and never said which of three possible estimators it meant — per-trial pairs, mean-of-trials, or a single control against k treatments — and they have different variances (`2E[p(1−p)]/k` versus `2E[p(1−p)]`), different supports, and therefore different dispatch outcomes.

**Comparability is a gate**, and `resolved_models` alone is not enough: `Run.generation` records temperature, seed, top_p, and `max_output_tokens`, and `input_fingerprint` explicitly does not cover them (`common.proto:278-283`). A baseline at `max_output_tokens=512` paired against a Value run at 2048 yields a δ that is truncation plus Asset. The gate compares both **and the baseline's `trials`**: a baseline at `trials = 3` records `X_i ∈ {0, ⅓, ⅔, 1}`, so a 1-trial Value run paired against it produces non-binary δ and silently takes the continuous branch — a run measured weeks earlier deciding this run's estimator. It refuses a baseline marked `error_rate_exceeded` — `what-the-numbers-mean.md` already commits later stages to that refusal, and Value is the first later stage.

**A model gate runs on the Value run itself.** `modelGate` exists to catch a model changing *mid-run*, and a Value run is ~100× the wall-clock of the Baseline that produced it, so a provider rollover is likelier — and a blend splits δ across two models *inside one Asset's sample*. #55 records that when the gate fires the tripping Case is still recorded and a second resume completes a blended run with nothing on the record; §7 Q8 asks whether `modelGate` is exported from `core` or reimplemented.

### 2.2 The interval method dispatches on the data, not on an assumption about the Goal

`goal/exactmatch` is the only Goal in the tree and returns 0.0 or 1.0, so today `δ_i ∈ {−1, 0, +1}` — paired binary, the McNemar setting, where the information lives in the discordant counts.

**A percentile bootstrap is wrong for that, and it fails in the direction that reads as certainty.** An inert Asset over 20 samples with every `δ_i = 0` gives **[0.000, 0.000]** — total confidence in an exact null from 20 binary observations. An Asset routed to 5 Cases that flips all five gives **Δ = +1.000, CI [1.000, 1.000]**, top of the greedy ranking from five coin flips. `Interval` exists as a message so its *absence* cannot be mistaken for a tight one; a zero-width interval defeats that.

**But "the data is binary" is an assumption with a short half-life, and draft 2 hard-coded it.** Two things falsify it without a new package landing:

- **`trials` is in this very plan and in shipped proto** (`Valuation.trials`). With *k* trials `δ_i` takes *k+1* values, not three — and McNemar, Wilson-on-discordant-pairs, and exact-conditional intervals are all defined on 2×2 discordant **counts** and are undefined on fractional discordance. Draft 2 named the flag and chose an incompatible method in the same section.
- **`Score.value` is a `double`** and judged Goals are on the v0.2 path. The moment one lands, either the paired-binary estimator is called on continuous δ (invalid, silently) or there is no interval — and prime directive 5 then means **`delta_goal` is not reported at all**.
- A `DIRECTION_MINIMIZE` Goal (latency, cost) is not on [0,1] in the first place.

**Draft 3 dispatched on the observed support of δ. That is a method selected from the sample**, and it has three costs: the 95% claim becomes conditional on a branch that is a function of the data, so V-1's coverage test proves nothing about the reported number; one extra routed Case can flip an Asset between branches and change its interval discontinuously; and with 200 Assets at different *n*, some land in each branch **by luck**, after which Select applies "the interval crosses zero" across intervals computed by different estimators with different coverage.

**So dispatch on a DECLARED property instead.** V-0 is open anyway: `Goal` declares its score domain — binary, bounded-continuous, unbounded — which is a fact about the Goal rather than about the draw. Undeclared means continuous.

| Declared domain | `trials` | Method |
|---|---|---|
| binary | 1 | Score-based paired-binary interval (§7 Q1) |
| binary | > 1 | Continuous: δ takes *k+1* values, and McNemar-family methods are undefined on fractional discordance |
| bounded / unbounded / undeclared | any | Paired continuous interval |
| **any, where the estimator is degenerate on this sample** | any | **A named non-degenerate fallback, never a refusal** |

That last row is P0-3, and draft 3 got it wrong in the way it spent four paragraphs warning against. Draft 3 scoped the refusal to `n < 2`, so **zero observed variance with n ≥ 2 fell into the continuous branch** — where a t-interval gives `s = 0` → **[0.000, 0.000]**, a percentile bootstrap gives identical resamples → **[0.000, 0.000]**, and BCa's bias correction is `Φ⁻¹(0) = −∞` → **NaN in `Interval.low`**. That is verbatim the failure A5 was rejected for, and the shipping default (`--trials 3` with `exactmatch`, which is DESIGN's own example) takes that branch.

Refusing instead is not an option either: an inert Asset has zero variance, inert Assets are the *majority* of a real pool, and a stage that reports no `delta_goal` for most of the pool is a check that gets disabled. So the degenerate case gets a distribution-free bound or a continuity-corrected interval on the nonzero-δ count — a stated mechanism, not an assertion that NaN will not happen.

Declaring the domain also **removes the ledger trigger draft 3 needed**: a judge PR must declare its Goal's domain to compile, so it cannot land without touching this.

**Q1 narrows to score-based methods.** An exact conditional interval conditions on the discordant total, and at `b + c = 0` — the all-δ-zero case that motivated this whole section — the conditional likelihood is flat and the interval is uninformative rather than merely wide. Tango, Agresti–Min, and Newcombe/MOVER-Wilson all stay non-degenerate there.

**`Interval.method`'s godoc enumerates `"bootstrap", "t", "wilson"`** and Q1's likely answers are none of those. Proto comments generate the published OpenAPI, so amending that comment is in V-0's scope.

### 2.3 Routing, sampling, and DESIGN.md's cost model

**Vocabulary first.** DESIGN.md already names this configuration: *"Budgets are first-class config (`max_llm_calls`, `max_cost_usd`, `sample_rate`, `trials`)"*. Draft 1 invented `--cases-per-asset` without checking, which is prime-directive-2 drift. The flags are **`--sample-rate`** and **`--trials`**, and the bound is expressed as a rate over the routed set with a floor.

**Routing is failure-clustered, as DESIGN.md prescribes** — restored in draft 3, because §2.1's fresh control arm removes the bias that made draft 2 reject it. This matters beyond correctness: DESIGN.md:363's worked example — *"50 × 55 × 3 ≈ 8,250 agent calls… ~$15–40"* — is the product's headline affordability claim, and its `55` is `15 failure cases + 40 controls`. **Draft 2 rejected failure routing and thereby overturned that number without flagging it**, which CLAUDE.md forbids: *"Where they conflict, stop and flag it; do not silently pick one."* Draft 3 does not conflict, so there is nothing to flag — but `DESIGN.md` joins §9 anyway, because the routing *mechanism* changes even where the arithmetic survives.

**Routing v1 is failure clustering by `Case.tags`, with two fallbacks.** Cluster the dev Cases the baseline failed, by tag; route an Asset to the clusters its own tags overlap. Then:

- **An Asset with no tags** routes to a sample of all failed Cases. Unlabelled is not irrelevant.
- **No Case in the dev split carries a tag** — which is the *default state of a real eval file*, since `tags` is `omitempty` and nothing populates it — routing is inapplicable and the stage samples all dev Cases under `--sample-rate`. Decided and stated **before the consent quote**, so the quote reflects the fallback's cost. Draft 2 treated the modal user's configuration as an edge case whose outcome was 200 `IRRELEVANT`s and a stage that appears to do nothing.

**Sampling is the bound routing is not.** Routing decides candidates; `--sample-rate` bounds the draw from them. Both feed the same interval.

**The residual bias Kno cannot detect, stated on the epistemics page.** §2.1's fresh control arm removes the bias from *Kno's* conditioning. It does not remove it from the user's: someone who tags Cases after reading a baseline failure report, assembles a pool by inspecting failures, or re-runs `kno value` after seeing the first run's results has conditioned on the outcome in a way no invariant here can see. That sentence goes in `what-the-numbers-mean.md` beside the existing "Kno cannot detect this" entries — it is the same species.

**Enforcement is structural, not a lint.** Draft 2 said `Store.CaseScores` would be "scoped so routing code cannot reach it", which Go cannot express for a method on an interface the same package already holds. Instead the router's constructor takes a narrow input carrying Case IDs, tags, and a `failed bool` — **no `Store`, no `Score`** — so the delta path and the routing path cannot share a source by construction.

### 2.4 Controls answer "did this break something", which is a one-sided question

Draft 1 said controls come free. **Arithmetically false**: for an un-routed Case the with-Asset score is never measured, so `δ = baseline − baseline = 0` identically, and `delta_control` would be a hard 0.000 with interval [0.000, 0.000] — not a regression signal, a constant that renders as "no regression, measured with perfect precision."

**Draft 3 said controls could reuse the recorded baseline "because that selection is not conditioned on the baseline outcome". That is false, and it is §2.1's own bug mirrored.** Routing selects `X_i = 0`, so the complement is selected on `X_i = 1` — **the complement of an outcome-conditioned selection is outcome-conditioned.** For a null Asset:

```
E[δ_control | X = 1] = E[Z_i] − 1 = p − 1
```

At p = 0.7 that is **−0.30 with a tight interval, for an Asset that does nothing** — and §2.4's instrument is a one-sided *harm* bound, so this would have been a systematic harm signal aimed at inert Assets, read through the detector most sensitive to it. `REJECTION_REASON_REGRESSION`, which `valuation.proto` calls the strongest reason to exclude something, would fire on the null. Simulated over 14,043 Cases: **−0.2947 against −0.30 predicted.**

The magnitude also varies per Asset — in the tag-routed path the complement is `(baseline passed) ∪ (tags do not overlap)`, so the bias depends on each Asset's overlap and is not comparable across the ranking; in the no-tags fallback the complement is exactly the passed set and the bias is maximal.

**The fix is free: partition the dev split at random BEFORE routing runs.** Routing operates only inside the routing-eligible portion; controls are drawn from the reserved portion, which is outcome-independent **by construction**. The recorded baseline is then a valid control there, so the control arm costs one measurement per Case rather than two, and the reservation is auditable from the seed V-0 already records.

So controls are measured: `--control-sample-rate`, drawn from the **reserved partition**, and **in the consent quote**.

**The quote is a ceiling and it carries every multiplier:**

```
assets × (routed_sample × arms + control_sample) × trials
```

where `arms` is 2 when routing conditioned on the baseline outcome (§2.1) and 1 otherwise. Draft 2 fixed the missing control arm and then **omitted `trials` — the same defect, in the same sentence, one multiplier over.** DESIGN.md's worked example uses 3 trials, so the quote would have under-stated by 3×.

**It is a ceiling on MEASUREMENTS, not on calls, and draft 3 asserted the stronger thing.** `invokeWithRetry` bills every attempt, so a single rate-limit retry breaks `quoted ≥ actual settled calls` — and a test asserting it would pass only on a run that happened not to hit a 429. That is the same "one multiplier over" defect this plan's review record already names twice, arriving a third time in the same formula. The quote says what it is: *calls at one attempt per measurement; transient-failure retries and settlement overshoot are additional and bounded by `--max-cost-usd`, not by this number.* §6 asserts the bound at one attempt per measurement.

**And DESIGN.md's arithmetic does not survive, which draft 3 claimed it did.** On DESIGN's own inputs (50 assets, 15 failure + 40 control, 3 trials), its figure is `50 × 55 × 3 ≈ 8,250` **at one arm**. Draft 4 costs `50 × (15×2 + 40) × 3 = 10,500` — **+27%**, and the `$15–40` headline moves with it. §2.3 asserted "there is nothing to flag"; there is. `DESIGN.md`'s cost-model section is in §9 for the number as well as the mechanism.

**Two further gaps between DESIGN's example and what v1 ships**, both inherited without support: DESIGN's `~1.5 clusters` concentration and `~30% of assets route to zero` are properties of *diagnosis-based* routing, and v1's router is free tag overlap; and DESIGN budgets a per-Asset **routing call** that has no term in the formula above. Because that selection is not conditioned on the baseline outcome, the recorded baseline is a valid control there (§2.1) — the control arm costs one measurement per Case, not two.

**Draft 2 then argued a smaller control sample was fine because "a coarse bound answers it". That is wrong, and in the dangerous direction.** A two-sided interval does not answer "did this break something"; it answers "is the control effect distinguishable from zero". At M=10 paired binary observations the interval is roughly ±0.3, so a true −0.10 regression returns an interval spanning zero — and `what-the-numbers-mean.md`'s shipped rule colors deltas by whether the interval crosses zero, so it renders as **not a regression**. An underpowered harm test that looks identical to a passed one is worse than no test.

So:

- The control quantity is a **one-sided upper confidence bound on harm**, not a two-sided interval — and **the shipped schema cannot represent one.** `Interval` is `{low, high, level, method}` with no sidedness field and no *n*. A one-sided bound written into `control_interval` is read as two-sided by everything downstream: `REJECTION_REASON_NO_EFFECT` is defined in shipped proto as "the confidence interval crosses it" and returns garbage; `what-the-numbers-mean.md`'s coloring rule cannot tell it from the two-sided interval §2.4 is arguing against; `level` means two different things in two fields of one message; and `high = +Inf` **is not serializable in JSON**, which the generated OpenAPI serves. So V-0 adds a sidedness discriminator (or a distinct `HarmBound`), the *n* behind each interval, and the underpowered marker — which draft 3 said would "travel with the number" through a field that does not exist.
- **A bound is not a decision rule.** The instrument that answers "did this break something" is a **non-inferiority test against a stated harm margin ε** — and ε is what `--control-sample-rate`'s default derives from, so it is named rather than left implicit.
- `delta_control` is marked **underpowered** below a stated M, and the marker travels with the number — the convention `what-the-numbers-mean.md` already uses for a holdout under 20 Cases.
- `what-the-numbers-mean.md` gains: *an interval crossing zero on controls means untested, not safe.*
- `--control-sample-rate`'s default comes from the harm size worth catching (§7 Q2), not from "smaller than treatment".

**`--route none` silently disables the control arm** by leaving the un-routed set empty. Refused, or the control sample is drawn from the routed set's complement within the full dev split — §7 Q7. Not an edge case: it is the flag a user reaches for when they distrust the tags.

**`valuation.proto:96` promises a net-loss judgement**, which requires combining treatment and control across two different Case populations weighted by their sizes. §2.5 names that problem for `delta_goal`; nothing computes the combination, and this plan does not either. Recorded as an accepted risk with a trigger rather than left implied.

### 2.5 What Δ estimates, said out loud

Under routing, a tagged Asset's `delta_goal` is the mean effect **over the Cases it was routed to**, and nothing else. An untagged Asset's is the mean over all dev Cases. Two Assets with identical content get different estimands purely from user labelling, and Select then ranks them against each other on `delta_per_cost` as if commensurable.

Concretely: Asset A routed to 20 billing Cases, Δ=+0.30; Asset B untagged over 500 Cases, Δ=+0.05. A outranks B while B moves the eval set five times as much (25 Case-points vs 6).

The plan does not fix this — ranking heterogeneous estimands is Select's problem and needs its own design. It **names** it: `Valuation` records `n_routed` and `n_dev` so a reader can scale, and `what-the-numbers-mean.md` says that differently-routed Δs are not comparable while `delta_per_cost` ranks them anyway.

### 2.6 Multiplicity, named rather than ignored

`REJECTION_REASON_NO_EFFECT` is defined as "the confidence interval crosses zero", and `Rejection.detail`'s worked example is a per-Asset significance claim. So Select runs a per-comparison test N times: **with 200 null Assets, ~10 intervals exclude zero by construction**, and they concentrate exactly where `delta_per_cost` ranks highest — small n and small cost.

Holdout Validate catches this at the portfolio level and `what-the-numbers-mean.md` already explains the winner's curse for `dev_estimated_gain`. Neither covers the per-Asset claim.

V-1 states the error rate `delta_interval` controls (per-comparison), and **CLAUDE.md's required winner's-curse property test asserts on the selected set, not on one interval** — N null Assets, ground truth zero, checking what greedy selection surfaces.

**Deferring the fix to Select is not free, because the interval is a decision input on WRITE.** `REJECTION_REASON_NO_EFFECT` is defined in shipped proto as *"the confidence interval crosses it"*, so Select cannot recover the family-wise picture from 200 independently-written `Interval`s unless it knows N — and `Interval` carries `level` but nothing carries N. **V-0 records the comparison count** while the proto is open anyway, and `Interval`'s godoc says `level` is per-comparison.

**A second source of dependence, unnamed in draft 2:** where the recorded baseline is reused as the control, all Assets are paired against the *same* draw, so their errors are positively correlated through the shared `X_i` — a Case where the baseline drew low makes every Asset routed there look good at once. So "200 × 0.05 ≈ 10" is a floor, not the number. **The winner's-curse generator must share a baseline draw across Assets**; one with independent baselines per Asset makes the test pass while proving nothing, which is the same trap §6 already flags for a Gaussian generator.

### 2.7 Cost, and the denominator that is not a reservation bound

`pricing.countTokens` reserves ~3× what English prose uses, deliberately — correct for reserving money, wrong for a ranking denominator. Two Assets with identical real token cost, one prose and one base64, would differ ~3× in `context_tokens` computed that way, so **greedy Select would order them by content type.**

`CostVector.context_tokens` is computed for ranking, not from the reservation path, and §2.5's docs row says so. The reservation still uses the pessimistic bound, because under-reserving is the failure that costs money.

The Asset goes into `pricing.Prompt.Context` — a field that exists and is documented as empty until `ContextInjector` lands — so the cost cap binds on the thing being measured.

**That is not true by construction, and draft 2 asserted it as though it were.** `WithContext` returns `Agent`, the narrowest interface; the budget path needs `Estimator`, which is *optional* and whose absence falls back to a run-scoped scalar containing no Asset. If V-3's wrapper does not forward it, every reservation is made against a per-call constant while the prompt carries the Asset — and `ring0.go` records that exact failure already measured once: *"the consent prompt quoted $0.06 for a run whose real exposure was $12.00."*

**V-3's contract**, with a `coretest` conformance check:

- the Agent returned by `WithContext` MUST forward `Estimator` and `Capable`;
- its `Estimate` MUST include the Asset in `Prompt.Context`;
- **V-4c refuses an Asset before any spend if the adapter does not declare `Capabilities().ContextInject`.** An adapter that ignores `Prompt.Context` otherwise produces a full-price run of pure noise, with intervals, indistinguishable from a real result.

That last line is **[#58](../debt.md#58)'s first caller**, which discharges it rather than mentioning it.

### 2.8 Two documents disagree about whether Value executes Cases

- `run.proto:227` — "Absent for a stage that invokes no agent: **Value works over Assets** and has no concurrency to report."
- `docs/adr/0004:37` — "**Value also executes Cases**, so a stage-named message would be either wrong for Value or duplicated for it."

Proto comments are the single source for the published OpenAPI, so shipping Value without resolving this publishes a reference that is wrong about the stage. Per CLAUDE.md this is a stop-and-flag, not a silent pick. It is also a live design question: what does a Value Run write into `CaseExecution`, whose `scored_case_count` is documented as "the denominator" and is aggregated from rows keyed by `case_id`? For a Value run that is distinct Cases, not measurements — so the Run's counts and its spend would describe different populations. **§7 Q3.**

### 2.9 Purge must cover what Value persists

`Purge` nulls `outcomes.response_proto` and `outcomes.score_proto` only. A new measurements table holding responses would **survive `kno purge`**, which would report "Purged 44 outcome(s)" over content still on disk — the same class as the `secure_delete` bug `retention.md` records having been fixed once. The new table's content columns are in `Purge` and in `PurgeableCount`'s denominator, and `retention.md`'s "What purge does not cover" gets the diff.

## 3. Alternatives considered

**A1 — Two fresh runs per Asset, unpaired.** Rejected: pays twice for a control already recorded, and carries Case-difficulty variance in both terms.

**A2 — Routing as the only bound.** Rejected as the only mechanism, kept as a lever. Routing reduces work by an amount nobody can predict before the run.

**A3 — Judge-based relevance routing in v1.** Rejected: `judge/` is empty and judges need a calibration set with agreement thresholds first.

**A4 — Route to the Cases the baseline failed.** **Rejected as a statistical bug**, and recorded here because DESIGN.md recommends it and it looks like a pure cost win (§2.3).

**A5 — Percentile bootstrap over paired δ.** Rejected: zero-width intervals on binary data, in the direction that reads as certainty (§2.2).

**A7 — Sample-split the baseline's own trials.** DESIGN.md's example runs the baseline at 3 trials. Route on trial 1's outcome and use trials 2–3 as the control: the selecting draw and the control draw are then independent given the Case's latent difficulty, so `E[δ] = 0` by §2.1's argument **at zero incremental spend** — the baseline already paid for all three. DESIGN's 8,250 survives intact. Not universally available (a `trials = 1` baseline has nothing to split, and the gate would need to record which trial routed), so the fresh arm stays as the fallback — but this is strictly better where it applies, and draft 3 did not consider it. §7 Q11 asks whether V-1 ships it.

**A6 — Leave `delta_control` absent and skip the control arm.** Considered seriously — it halves the stage's cost and `Interval`'s absence is representable by design. Rejected because the regression signal is the thing that catches an Asset that helps its slice and breaks another, and DESIGN.md promises it. Recorded as the lever to pull if §7 Q2's cost proves unacceptable.

## 4. The PR decomposition

Draft 1's V-4 was the entire stage in one branch. Split, with proto first per CLAUDE.md's coordination rule:

| PR | Scope | Depends on | Spends? |
|---|---|---|---|
| **V-0** | **Proto:** `Valuation.not_measured`/`n_routed`/`n_dev`/`control_underpowered`/`n_comparisons`; `Interval.sidedness`/`n`; `AssetRouted`/`AssetValued` events; `Run.sampling_seed`; `ScoreDomain`; §2.8 resolved; ADR-0005 | — | No |
| **V-1** | `stats/interval` — support-dispatched interval, winner's-curse property test, **the #1 construction-time invariant** | — (`Interval.method` is a shipped `string`; V-0 amends its godoc but does not block) | No |
| **V-2** | `adapters/pool/jsonl` | — | No |
| **V-3** | `ContextInjector` on both adapters; Asset into `Prompt.Context`; `Capabilities.ContextInject` | V-0 | No (fixtures) |
| **V-4a** | Store: `schemaVersion` 3, `measurements` table, `Valuation` writer, `CaseScores`, **`SettledSpend` over both sources, `CompletedMeasurements`**, purge coverage | V-0 | No |
| **V-4b** | `core/value` failure-cluster routing + sampling, **router constructor takes no `Store`**, no spend | V-0, V-2 | No |
| **V-4c** | The measurement loop: inject, pair, budget, resume, events | all above | **Yes** |
| **V-5** | `kno value`, report, docs | V-4c | Yes |

**V-1 does not start before this review lands**, which draft 1 proposed — its interval method is decided by §2.2, which is a review finding.

**V-3 and V-4c are coupled** on one decision draft 1 hid: whether the Asset lands in the system block or the message body. `anthropic`'s `System` is a top-level API field while `openaicompat`'s is a message, and the choice decides whether provider prompt caching hits across an Asset's whole sample — which `costOf` prices at a separate rate. V-3 does not pick it alone; it is §7 Q4.

## 5. Edge cases

| Case | Mitigation |
|---|---|
| Pool empty | Refused before any spend |
| Asset routes to zero Cases | `REJECTION_REASON_IRRELEVANT`, no measurement cost. The cheap valuable answer |
| Every Asset routes to zero | Completes, reports why the tags did not overlap |
| No baseline score for a routed Case | Dropped from the pair, **counted and reported**. `CaseScores` must distinguish "absent" from "unrecoverable" (§7 Q5) |
| Baseline model or generation config differs | Refused (§2.1) |
| Baseline marked `error_rate_exceeded` | Refused (§2.1) |
| Asset exceeds `--max-prompt-bytes` | Refused per Asset, recorded, run continues. **Not** the model's context window — the fix line must name the knob that actually bound. The flag bounds the Case; the Asset is bounded by it separately and charged **on top**, so no Case is ever accepted by the control arm and refused by the treatment arm (V-3 Phase 3, P1-3) |
| Injected Asset pushes a Case over the limit | Case dropped from that Asset's sample, counted. **This is attrition correlated with the treatment** — a large Asset gets measured only on short Cases. Reported, not hidden |
| Cap stops the run mid-Asset | Measurements are durable; no `Valuation` is written for that Asset; resume continues from the measurement rows and re-pays nothing |
| Duplicate Asset IDs | Refused at ingestion |
| Zero-cost Asset | `delta_per_cost` undefined; ranked in a separate tier rather than by an infinity (§7 Q6) |
| Holdout leakage | Canary: no holdout Case ID in any measurement row or `Valuation.case_ids` |

## 6. Test plan

**V-1**
- **Coverage property against the data-generating process that ships** — paired binary at realistic *p*, not Gaussian. A Gaussian generator makes this test pass while proving nothing.
- **No zero-width interval** for any input, including all-δ-zero and all-δ-one.
- **Winner's-curse property test** (CLAUDE.md requires one): N null Assets, ground truth zero, asserting on the **selected set**.
- Pairing-vs-unpairing compared **at the shipping regime** (binary, p≈0.5, n = `--cases-per-asset`), not at a constructed one.
- Degenerate n=0, n=1; no NaN may reach `Interval.low`.

**V-4b**
- **The routing invariant**: routing code cannot reach per-Case baseline scores. A test that fails if it does.
- A regression-to-the-mean canary: a null Asset routed by tags measures Δ≈0; the same Asset routed by baseline failure measures Δ≈p. Pins §2.3's reason.

**V-4c**
- Holdout canary.
- Zero-routed Asset costs zero calls, asserted against the guard.
- Pairing uses the RECORDED baseline score, proven by a fixture where re-running would differ.
- Cap mid-Asset: measurements durable, no `Valuation`, resume re-pays nothing — asserted against `SettledSpend`.
- Every test mutation-verified, and every mutation verified to have applied.

## 7. Open questions

**Q1.** Which score-based paired-binary interval — Tango, Agresti–Min, or Newcombe/MOVER-Wilson? Exact-conditional is ruled out: it is uninformative at `b + c = 0`, the case that motivated §2.2.

**Q2. RESOLVED in V-4b**, with the derivation in each constant's godoc rather than a table here. `--trials` defaults to 1: repeats buy variance reduction and DESIGN.md's 3 is a worked example, not a floor, so the default does not silently triple every run's cost. `--sample-rate` is 0.8 — routing has already cut the candidate set to an Asset's matching failures, so it is bounding a set small by construction and cutting hard again costs interval width, which is the thing the stage reports. `--control-sample-rate` is **1.0, higher than the treatment rate**, which inverts the intuition and is the point: the control question is a one-sided harm bound, and §2.4's arithmetic says an underpowered one is indistinguishable from a passed one. The floor is `MinSample` = 5 and the underpowered marker fires below `MinControlSample` = 20, so a small eval set is reported as untested rather than quietly cleared. *(Original: Defaults for `--sample-rate`, `--control-sample-rate`, and `--trials`. Q1 comes first — the treatment default follows from the method. The control default follows from the harm size worth catching (§2.4), not from the treatment default.)*

**Q3. RESOLVED in V-0.** Value **does** execute Cases, so it writes `CaseExecution` and records a `ConcurrencyDecision`; `run.proto`'s two claims that it "works over Assets" were wrong and are corrected, ADR-0004 was right. The counts are of **measurements**, not distinct Cases — 200 Assets over 50 Cases attempts 10,000 measurements over 50 Cases, and counting distinct Cases would put a denominator of 50 beside the spend for 10,000 calls. Per-Asset denominators live on `Valuation.n_routed`. `newModelGate` therefore reads a populated `CaseExecution` and the mid-run gate works. *(Original question: what does a Value Run write into `CaseExecution` **and into `Run`'s own `attempted`/`scored`/`errored` counts**? Those are separate fields the API serializes for every stage, and for a Value run whose rows live in `measurements` they close at zero — a Run record asserting the stage did nothing. And what does `error_rate_exceeded` mean when 0 of 0 Cases scored? Resolving §2.8.

**Q4.** System block or message body for the injected Asset? Decides whether provider prompt caching hits across an Asset's whole sample, which is priced separately.

**Q5. RESOLVED in V-4a.** `CaseScores` returns `map[string]CaseScore`, where `CaseScore` carries the value and an `Unrecoverable` flag. Presence means the Case scored; absence means it never did. A `map[string]float64` collapses "scored, number purged" into "absent", and pairing against the resulting zero manufactures a delta — so the state is kept rather than flattened, and the Value stage drops such a Case from the pair and reports the count rather than refusing the run the way Baseline does. Baseline refuses because its aggregate IS the deliverable; Value's deliverable is per-Asset and a handful of unpairable Cases shrinks one denominator rather than invalidating the run, provided the shrinkage is reported — which `Valuation.n_routed` against the pair count already makes visible. *(Original question: `CaseScores`'s signature. `ScoreSum` returns `(sum, counted, unrecoverable)` because "scored but the number is gone" is a real state; a `map[string]float64` collapses it into "absent". Does Value need Baseline's refusal, or does it drop and report?)*

**Q6.** `delta_per_cost` for a zero-cost Asset — all three `CostVector` terms zero. Undefined is honest; a separate ranking tier is probably the handling.

**Q7. RESOLVED in V-4b, by the design rather than by a choice.** The question assumed controls are drawn from the routed set's complement, which §2.4 had already replaced with a partition reserved at random **before** routing runs. Controls therefore do not depend on routing at all, and `--route none` cannot remove them — `Options.DisableRouting` is asserted to leave the reserved partition byte-identical. It also drops the fresh control arm, which is a saving rather than a corner cut: a random sample is not conditioned on the baseline outcome, so the recorded baseline is a valid control there. Neither refusing the combination nor drawing from the complement is needed. *(Original: `--route none` leaves no un-routed set, so the control arm silently disappears. Refuse the combination, or draw controls from the complement within the full dev split?)*

**Q8. RESOLVED with Q3.** Half of it is a mechanical fact rather than a question: `modelGate` is unexported in `package core`, so `core/value` cannot reach it either way. The half that matters is inside Q3 — **`newModelGate` reads `run.GetCaseExecution().GetResolvedModels()`, so if Q3 resolves to "a Value Run writes no `CaseExecution`", the mid-run model gate §2.1 argues is essential becomes a no-op that reports success.**

**Q10.** A Goal or an `Invoke` that errors mid-valuation: `REJECTION_REASON_MEASUREMENT_FAILED`, or a `Valuation` over a shrunken pair set? Note the direction — dropping a pair because the *treatment* arm errored removes exactly the Cases where the Asset was most harmful (long injected context → timeout), so Δ is biased **upward**, and the bias scales with Asset size, which is `delta_per_cost`'s numerator against its own denominator. §5's other two attrition rows are symmetric; this one is not.

**Q11.** Does V-1 ship A7 (sample-splitting the baseline's trials) where the baseline has `trials > 1`? It removes the 2× on the routed slice for free and preserves DESIGN.md's arithmetic.

**Q12.** Asset ordering. With Q4's prompt caching, measuring A before B changes B's cost; under a binding cap, order decides which Assets get a `Valuation` at all. What is the order, and what asserts that valuing A before B does not change B's Δ?

**Q9.** How is `CostVector.context_tokens` computed? §2.7 rules out `countTokens` — it reserves ~3× for prose, so two Assets of equal real cost differ ~2.4× in the ranking denominator — but the only unbiased alternative is a real BPE tokenizer, which that same godoc rejects as "a large dependency plus a per-model vocabulary that goes stale silently". Draft 2 asserted a computation that does not exist. Either accept the bias with its direction stated, or name the dependency.

## 8. Rollback

V-0's proto additions are additive (`buf breaking` clean). V-1 to V-3 add packages with no callers until V-4c. **V-4a is the one-way door**: a `schemaVersion` 3 migration cannot be un-run on a database that has taken it, so a revert requires a forward migration rather than a rollback — the same rule every prior migration has followed.

## 9. Docs

| Doc | PR | Change |
|---|---|---|
| `docs/what-the-numbers-mean.md` | V-1 | What a paired interval claims; **per-comparison error and what that means across N Assets**; that differently-routed Δs are not comparable |
| `docs/mental-model.md` | V-4b | Routing and sampling; why the stage bounds its own cost |
| `docs/cookbook/retention.md` | V-4a | The measurements table under "what purge covers" |
| `docs/cookbook/` | V-5 | Valuing a pool; reading intervals; what "irrelevant" means |
| `README.md` | V-5 | Value moves to Shipped |
| `run.proto` / ADR-0004 | V-0 | Whichever §2.8 resolves to |

## 10. Accepted risks

CLAUDE.md makes Phase 1 exit conditional on remaining objections being explicitly accepted **and mirrored to the ledger**. Draft 2 left this empty, which is not a valid exit. Each row gets a ledger entry with a trigger before V-0 starts.

| # | Accepted | Trigger |
|---|---|---|
| R1 | **The estimand varies per Asset** (§2.5). A tagged Asset's Δ is over its routed slice; an untagged one's is over all dev Cases; Select ranks them together on `delta_per_cost` as if commensurable. Named and recorded (`n_routed`, `n_dev`), not fixed — ranking heterogeneous estimands is Select's design problem | The Select stage |
| R2 | **Multiplicity is per-comparison** (§2.6). V-0 records N so Select *can* correct; this stage does not | The Select stage, with the winner's-curse test as its evidence |
| R3 | **Kno cannot detect user-side conditioning on the baseline** (§2.3) — tags assigned after reading a failure report, a pool assembled from failures, a second `kno value` run informed by the first. Stated on the epistemics page rather than guarded | A permanent limit, so CLAUDE.md requires "won't fix" with the rationale **moved into an ADR** — `docs/adr/0005-value-cannot-see-user-side-conditioning.md`, written in V-0. An entry with no trigger and no ADR is a rejected finding, not accepted debt |
| R4 | **No net-loss judgement.** `valuation.proto:96` promises one; combining treatment and control across two differently-sized populations is not computed | The Select stage, which is where a net judgement is acted on |
| R5 | **`context_tokens` carries the tokenizer's bias** (§2.7, Q9) unless Q9 resolves to a real tokenizer. The ranking denominator is content-type-dependent | Q9's resolution, or the first report of a mis-ranked pool |
| R6 | ~~moved to §7 Q10~~ — a decision V-4c must make is a task inside this plan, not a repayment trigger. CLAUDE.md: "'Someday' is not a trigger" | — |

## 11. Review record

| Pass | Verdict | Outcome |
|---|---|---|
| 1 | **BLOCK**, 5 P0s | All verified and upheld. Three invalidated the decomposition: the proto is not done, the store cannot hold the run, and the control arm was unbudgeted. Two decided the interval method. Draft 2. |
| 2 | **BLOCK**, 4 P0s | All verified and upheld, and one changed the measurement design. Draft 3. |
| 3 | **BLOCK**, 3 P0s + 9 P1s | **The estimator was attacked directly and survives.** Pass 3 worked the algebra: `E[δ] = 0` under the null holds for the routed arm, and pairing removes `2Var(p)` even with two fresh draws, because the shared object is the latent difficulty rather than the realization. Fresh-vs-fresh and fresh-vs-recorded have *identical* variance — the second arm buys **bias removal only**, which is the honest framing. What did not survive was the rule's SCOPE. Draft 4. |

**What pass 2 changed, and it is the important one.** Draft 2 concluded that routing may never condition on the baseline outcome, and rejected DESIGN.md's failure-clustered routing to enforce it. The bias is real, but the cause is **reusing the selecting draw as the control**, not the conditioning — so a fresh control arm on the routed slice removes it entirely. Draft 2's over-correction had silently overturned DESIGN.md's headline cost model (8,250 calls, $15–40) without the stop-and-flag CLAUDE.md requires. Draft 3 restores failure routing and keeps the arithmetic.

**The pattern, now three times, and pass 3 named it inside the draft that diagnosed it.** §2.1 stated a conditional rule and §2.4 violated it in writing — on the complement of the very selection §2.1 was about, in the direction that fires the harm detector. The plan's own self-diagnosis was accurate and recurred one paragraph later.

**The failure pattern, now twice.** Both passes found the readings of the tree accurate and the *inferences* one step short: the store diagnosis stopped at the write path and missed four readers, the control-arm fix stopped at the quote and missed `trials`, the routing finding stopped at the code path and missed the user. Reading correctly is necessary and has not been sufficient.

**And the un-failable test appeared in a plan this time.** Draft 2's resume test asserted `SettledSpend` equality on a method that structurally cannot see a Value run's spend — `0 == 0`. That is the same class Phase 3 caught four times during M2-11, arriving one stage earlier.
