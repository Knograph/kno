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

**V-4c's loop shape, decided at implementation and recorded here rather than in a second plan.** `executor.Run` is generic over `I proto.Message`, and a measurement — (Asset, Case, arm, trial) — is not a proto message. Three ways out:

- **Add a proto work-item message.** Rejected: `kno.v1` is the wire contract and a scheduling type is not a domain type. ADR-0001 makes proto messages the domain types precisely so the wire and the model cannot drift; putting an engine-internal item there inverts it.
- **Generalize the executor** to `I any` plus a `Clone` option. Rejected for this PR: it changes a shipped package that every stage depends on, and the `proto.Message` bound exists to make the borrow rule (debt #8) unforgeable at the call site. Handing that guarantee to a caller-supplied closure is a real loss for a problem the third option solves for free.
- **Iterate Asset → arm → trial, running the executor over that arm's Case list with `I = *Case`.** Taken. No proto change, no executor change, and it puts the concurrency boundary exactly where the durability boundary already is: a `Valuation` is written when one Asset's measurements are all in, so an Asset is the natural unit.

**Accepted cost:** concurrency is bounded *within* an arm, so an Asset routed to five Cases under-uses an eight-worker pool. Real, and cheaper than either alternative at this stage. Ledger entry with a trigger, not a silent carryover.

The `Valuation` is computed from `Store.Measurements` — what is DURABLY recorded — rather than from in-memory results, which is the pattern `6bb14a8` established for `CaseExecution` and what makes a resumed Asset's recomputation span both processes.
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

**Q2. RESOLVED in V-4b** (amended after Phase 3, which found the first resolution had chosen the convenience number and written its justification backwards — §2.4 required deriving the control default from a stated harm margin ε, and the shipped derivation named no ε at all). **ε = `HarmMargin` = 0.10**, and the honest consequence is stated rather than hidden: detecting it reliably needs a control sample larger than most dev splits hold, so the stage reports `Plan.MinDetectableHarm` — the smallest regression the drawn sample could separate from zero — instead of summarising power into a bool. `MinControlSample` = 20 survives as a floor with its weakness written into its godoc (0.18 detectable, ~18% power against ε), because a threshold set where the instrument actually works would mark nearly every real run underpowered and stop carrying information., with the derivation in each constant's godoc rather than a table here. `--trials` defaults to 1: repeats buy variance reduction and DESIGN.md's 3 is a worked example, not a floor, so the default does not silently triple every run's cost. `--sample-rate` is 0.8 — routing has already cut the candidate set to an Asset's matching failures, so it is bounding a set small by construction and cutting hard again costs interval width, which is the thing the stage reports. `--control-sample-rate` is **1.0, higher than the treatment rate**, which inverts the intuition and is the point: the control question is a one-sided harm bound, and §2.4's arithmetic says an underpowered one is indistinguishable from a passed one. The floor is `MinSample` = 5 and the underpowered marker fires below `MinControlSample` = 20, so a small eval set is reported as untested rather than quietly cleared. *(Original: Defaults for `--sample-rate`, `--control-sample-rate`, and `--trials`. Q1 comes first — the treatment default follows from the method. The control default follows from the harm size worth catching (§2.4), not from the treatment default.)*

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

## 12. Amendment A — Phase 3 review of the V-4c scaffolding (2026-08-27)

**Why this section exists.** The V-4c wip (valuation computation, pairing, schedule construction) was written against this plan but never reviewed. A full adversarial pass over the branch (`feat/value-loop2` vs `main`) found 14 findings — 3 P0, 4 P1, 4 P2, plus dispositions for the open questions V-4c owns. This amendment records the resolutions, the ledger entries, and the schema touches. It amends the plan in place because §11's review-record table is the decision record, and these decisions are V-4c's.

**Scope rule.** Every fix below lands in the V-4c PR (with V-5) — the scaffolding is dead code today, so fixing it and shipping it are the same PR. Proto-first: one lead commit carries every schema touch, per the proto-first coordination rule. The complete proto/schema list is: (1) `RejectionReason` gains `NOT_MEASURED_UNDERPOWERED` — the enum behind `Valuation.not_measured` (field 15, typed `RejectionReason`); the `not_measured` godoc's "only three values are legal" clause and `AssetValued.delta_goal`/`delta_interval` godocs ("never omitted") are amended in the same diff (P0-2); (2) `Run` gains `trials` (new field, P1-5) and `value_plan` (serialized `value.Plan` blob, P1-10); (3) `RetryAttempted`, `SettlementOvershoot`, `OrphanSpend` gain optional `asset_id`, `arm`, `trial` (P1-8); (4) `Valuation`/`AssetValued` gain `n_pairs` and `n_dropped` (P1-9). All additive — `buf breaking` passes. Store migration: runs table gains the two columns, schemaVersion 4, forward-only per §8's rule. (§4/§8's schemaVersion 3 references remain accurate as history; the 3→4 bump is recorded here.) Code ripple from the hook-signature change: `core/baseline_invoke.go` wiring closures and `TestBaselineWiresEveryInvokerHook` in `core/record_internal_test.go` update in the same commit.

### 12.1 P0 resolutions

**P0-1 — Goal direction is applied in `pairs`, not in the interval package.** `stats/interval`'s contract is "deltas arrive sign-corrected" — the caller scales. `pairs` computes `dir := +1` for DIRECTION_MAXIMIZE, `-1` for DIRECTION_MINIMIZE, and scales every per-Case delta before aggregation. The negation happens exactly once, in `pairs`; the report/CLI layer un-negates MINIMIZE deltas for display only, and nothing downstream re-applies direction. A MINIMIZE-goal test (latency) joins the suite; every existing test is MAXIMIZE and would not have caught the inversion.

**P0-2 — A delta ships only beside its interval, for both numbers.** `valuationFor` sets `DeltaGoal` only when `DeltaInterval` is non-nil, and `DeltaControl` only when `ControlInterval` is non-nil — the wip sets `DeltaControl` unconditionally whenever one control Case exists, which is exactly the unguarded-pair shape debt #1 bans. When either interval is nil, `not_measured = NOT_MEASURED_UNDERPOWERED` — a new value on the existing `RejectionReason` enum (the type behind `Valuation.not_measured`), whose godoc states it covers both causes (sample too small to form an interval: fewer than 2 pairs, a 1-Case routed or control slice; and ragged per-Case vectors at Trials>1 after attrition) rather than a small-n-only name. The `AssetValued.delta_goal`/`delta_interval` godocs change from "never omitted" to "omitted exactly when `not_measured` is set". This is the named non-degenerate fallback §2.2 demanded ("a named non-degenerate fallback, never a refusal") — the wip's implicit fallback was emitting the delta anyway. This discharges debt #1's trigger ("when the Value stage lands") — the gate exists where the stage lands, for `delta_control` as well as `delta_goal`.

**P0-3 — The harm bound consumes per-Case means, and flattened per-trial deltas are banned at the call site.** Flattened per-trial deltas share one recorded baseline per Case — correlated within Case — and `HarmBound` treats every value as independent (`compute` uses `n = len(deltas)` for every method), so the bound comes out ~√Trials too narrow in the direction that clears harmful assets. Resolution, stated to exclude the wip's current shape: the control path builds per-Case means first (average each Case's deltas over its completed trials, mirroring `PairedTrials`'s `meanPerCase` shape) and calls `HarmBound` with those — n = Case count, never k×Trials. `plan.Trials` is passed for method dispatch only and is never legal alongside flattened per-trial deltas; the code comment at the call site says so. A unit test at Trials=3 asserts the bound equals the per-Case-mean computation. `DeltaControl`'s point estimate is already `meanOfMeans` — the interval now describes the same quantity as the point. Consequence, stated: a Case that kept exactly one of its trials after attrition dispatches to the continuous method, valid but silently chosen by data loss; acceptable because P1-9 counts the loss. The paired-package's own warning ("handing over k×n values returns an interval about √k too narrow") is the provenance; the fix is the package's intended call shape, not a workaround.

### 12.2 P1 resolutions

**P1-4 — `measurementsFor` mirrors `Plan.Measurements()` exactly, and `assertQuoteBounds` becomes the consent's enforcement.** The wip scheduled control-partition measurements for zero-routed assets that `Plan.Measurements()` deliberately skips ("there is nothing to test for harm — the Asset is never put in front of the agent"). Both sides of that contradiction were money bugs: wire the schedule, zero-routed assets pay unquoted calls; wire the check, the run refuses on its own consent. Resolution: `measurementsFor` skips zero-routed assets byte-for-byte like `Plan.Measurements()`, and `assertQuoteBounds` is wired into the loop start so scheduled-vs-quoted is asserted on every run, not dead code.

**P1-5 — Blended-model baselines are refused; trials are recorded, not gated.** A trials-equality gate is wrong: the recorded-baseline arm pairs against the baseline's aggregate score (`b.Value`), which is a mean over however many trials the baseline ran, so a `Trials=3` baseline against a `Trials=1` Value run is a valid pair — a noisier-control-vs-lower-denominator mismatch never enters the pairing, and gating on equality would refuse the default configuration (DESIGN.md's worked example uses Trials=3, §7 Q2 defaults Value to 1). What is refused: a baseline whose `CaseExecution.resolved_models` set holds more than one model — pairing into a blended control is not an estimator of anything. The refusal reads the baseline run's recorded `resolved_models` (already on `CaseExecution`, no new field) at stage entry, before any spend; opt-out is one flag (`--unsafe-baseline`, CLI naming in V-5). Schema touch: `trials` is recorded on the Run at close (new proto field on `run.proto`, which has none today) — it is not used to gate, it is recorded so resume fingerprints (P1-10) and the report can state the configuration honestly. Legacy policy: baselines recorded before schemaVersion 4 have `trials` unset; unset is accepted and reported as "trials unknown", never refused. Migration: additive column on the runs table, schemaVersion 4, forward-only per §8's rule. This discharges debt #55's trigger — "the first stage that consumes a Baseline as a reference (Valuation)" — because the gate on blended models is the marker, and the discharge takes effect with this PR, not with the plan's prose.

**P1-6 — `minDetectableHarm` uses the honest sd bound and the underpowered marker follows from it.** The wip used 0.5 as the sd; for a paired Bernoulli difference Var = 2p(1−p) ≤ 0.5, so the sd bound is √0.5 ≈ 0.707. The reported bound was ~√2 too small, and `TestTheReportedBoundIsTheOneTheRunCanActuallySee` asserted the wrong arithmetic — the test's name was the claim that failed. Fix: `sdMax = math.Sqrt(0.5)`, the t-quantile for small m, and the test simulates the Bernoulli process and asserts coverage instead of asserting the formula. Consequence, stated honestly: at MinControlSample=20 the one-sided half-width is ≈0.26, larger than HarmMargin ε=0.10, so a run at the floor flags harmless assets at rates above design intent. The underpowered marker therefore fires on the honest bound itself — `MinDetectableHarm > ε` (≈135 Cases against ε=0.10) — `MinSample`=5 remains the hard floor, and `MinControlSample`=20 is KEPT as a flag floor, not removed — deleting it strands the shipped `control_underpowered` proto field (18), `route.go`'s marker, its tests, and the documented promise (`mental-model.md` "Below 20 Cases Kno also marks the run underpowered", `what-the-numbers-mean.md`). The underpowered flag therefore fires when n < 20 OR `MinDetectableHarm > ε`; the bound is always reported as a number regardless of the flag, so the flag is information, not an alarm. Consequence, stated honestly: against ε=0.10 the honest threshold sits near m≈135, so the flag fires on nearly every real run — the exact "flags everything, becomes noise" outcome Q2's review rejected for a bool that REPLACED the number; keeping the number first and the flag second is what makes the same frequency honest reporting instead of noise. §7's Q2 resolution text is superseded on the marker clause, and §2.4's arithmetic supersession extends to its own example: "at M=10 the interval is roughly ±0.3" used the same variance/sd slip (honest: ±0.44). Docs diff: `mental-model.md`, `what-the-numbers-mean.md` update in this PR, plus the wrong-sd godocs in `core/value/route.go` itself (`DefaultControlSampleRate`'s "roughly +/-0.3" and `MinControlSample`'s "0.18 detectable, ~18% power") — the same slip lives in the file the fix edits, and `make docs` gates godoc accuracy. This is a report-accuracy fix, not a redesign: §2.4's arithmetic was already in the plan; the wip implemented it with a variance/sd slip.

**P1-7 — `NDev` is the eligible pool, not the control partition.** Proto godoc says "Cases in the dev split this Asset was routed from". The wip wrote `len(ControlCaseIDs)` — the control partition is ~30% of dev, so consumers scaling Δ×n_dev understate ~3×. `NDev` is set from the routing's eligible pool. No schema change; the field was populated with the wrong source.

**P1-8 — Proto-first: the three money events gain (asset_id, arm, trial), and the invoker hooks carry the measurement key.** `RetryAttempted`, `SettlementOvershoot`, `OrphanSpend` carry `CaseId` only; a Value run's spend is keyed (run, asset, case, arm, trial), so the API cannot attribute retries or overshoot to an Asset. Additive fields — `buf breaking` passes; per §4 the proto diff is a commit of its own at the head of the V-4c PR. `invoker`'s hook signatures (`OnOvershoot`, `OnRetry` in `core/invoke.go`) gain the measurement key; Baseline passes its Case key through unchanged (the fields are additive and Baseline populates asset/arm/trial as empty). Ripple targets, all in the proto-first commit: the hook closures in `core/baseline_invoke.go`, and `TestBaselineWiresEveryInvokerHook` in `core/record_internal_test.go` — the #77 discharge's wiring test updates alongside, since it pins the signatures. Debt #52's lesson — "events must say which Case" — applies one level up.

**P1-9 — Attrition is counted and reported, and Q10 resolves to the shrunken set.** Dropping a pair because the treatment arm errored removes exactly the Cases where the Asset was most harmful — the Q10 bias — and the harm test self-censors the same way (reserved Cases measured under the Asset's injection). Resolution: keep the shrunken set (the only honest option at record time), but every drop is counted per Asset and surfaced: `n_pairs` and `n_dropped` become fields on `Valuation` and on the `AssetValued` event (additive, in the proto-first commit — the wip's `pairs` comment already promises "the count of drops travels with the number" while the function returns only the number vectors, so the contract exists in prose and becomes real), `n_routed` already exists, and the bias direction is stated on the epistemics page. No imputation, no flattening to repair raggedness (that is P0-3 in another costume).

**P1-10 — Resume consumes the recorded Plan; budget-refused measurements persist as done-markers that replace orphan-spend writes.** Two holes: (a) `--resume --trials 3` after a `Trials=1` run blends two configurations into one delta and spends beyond the original consent — `InputFingerprint` pins agent inputs, not the schedule, and hashing the schedule is fragile because `Route()` depends on more than seed/trials/rate (ControlReserve, FreshControlArm, the asset set, the baseline's scores all shape the partition). Fix: the run row stores `value_plan` — the serialized `value.Plan` — at close (proto-first commit), and resume **consumes the recorded Plan instead of re-running Route**; the fingerprint check then compares the requested configuration against the recorded one and refuses on mismatch — scoped to what the recorded Plan can express: seed, trials, sample rates, ControlReserve, routing mode, asset set, baseline run ID. Provider rate limits are deliberately out of scope (a resume differing only in rates fingerprints as identical). (b) A measurement whose attempts settled but whose row is never written (the baseline budget-sink pattern) makes resume re-attempt and re-pay — the plan's own "cap mid-Asset resume re-pays nothing, asserted against SettledSpend" test fails on exactly that path. Resolution: the row IS written as a done-marker with `err_code=BUDGET_EXCEEDED`, counted in attrition, and **the marker replaces the `RecordOrphanSpend` write for measurement-level refusals** — `SettledSpend` sums outcomes + measurements + orphan columns, so writing both would count the same refusal twice. Counting rule: done-marker rows count in the run's `attempted`, never in `scored`. Alternatives rejected: skip-persist (resume re-pays — a spend bug), third option in-memory only (resume can't see it — same bug, different timing).

### 12.3 P2 resolutions

**P2-11 — Capability check runs on the wrapper, not the receiver.** The V-3 contract is `WithContext(asset)` returning the agent that actually runs; the wip type-asserted the receiver. The wrapper is built once per Asset at measurement start and its `Capabilities().ContextInject` is asserted there. This also discharges the "`Capabilities()` is never called" observation — it becomes load-bearing for spend.

**P2-12 — Two invokers per Asset, both hooks wired, wiring test — debt #77's trigger discharges in this PR.** Treatment invoker: receiver Agent + `WithContext(asset)`; control invoker: receiver Agent alone; one shared guard. `invoke.go`'s doc comment already says Value builds one invoker per arm. Both hooks emit events carrying the measurement key (P1-8), and a `TestValueWiresEveryInvokerHook` mirrors `TestBaselineWiresEveryInvokerHook`. The rebase onto main (with the extraction) landed first, so V-4c consumes the same code Baseline does.

**P2-13 — The holdout canary becomes falsifiable, via SealedEvals.** The wip never opens case data at all, so "no holdout Case ID in any measurement row" passed vacuously. The split does not live on the Pool (`Pool = Assets()` only) — it lives in `core/seal.go`'s `SealedEvals`, whose whole point is that holdout Cases are not reachable through it. `ValueOptions` gains an Evals field (interface impact on §4/§6, enumerated here: one new parameter, consumed only for the refusal); the stage asserts at entry that no requested Case ID lies in the holdout, and the routing/reserve paths never consult holdout data. The canary test drives a real run with a holdout Case planted in the input set and asserts it is refused (engine level) and absent from every row (store level). This discharges debt #21 — its trigger "when the second stage lands" fires now, Value IS the second stage.

**P2-14 — Pairing joins on trial index.** Positional pairing (`tr[i]` vs `ct[i]`) misaligns after asymmetric trial gaps: treatment-trial-3 pairs with control-trial-2. Unbiased only if trial effects are absent — which is exactly what Trials>1 exists to measure. `pairs` joins on trial number, drops unpaired trials per Case, and counts them (P1-9's counter).

### 12.4 Open questions — dispositions recorded

| Q | Disposition | Rationale |
|---|---|---|
| Q1 (interval method) | **Accept shipped Agresti–Min** | The decision was made in `stats/interval` with the coverage comparison already documented. V-4c's job is calling it correctly (P0-1..3), not re-selecting the method. |
| Q4 (system block vs message body) | **Message body** | V-3's `WithContext` contract decision, but it sets V-4c's cost semantics: message placement makes the shared Asset prefix prompt-cacheable, matching how real users configure agents and §4's caching assumptions. Recorded consequence: per-Case treatment costs are order-dependent (first Case pays cache-write); cost is not modeled as iid. |
| Q10 (attrition) | **Shrunken set, counted** | See P1-9. Direction of bias stated on the epistemics page. |
| Q11 (A7 sample-splitting) | **Deferred to its own plan** | A7 changes the pairing scheme, the consent quote, and the fresh-control-arm pairing (control becomes trial-conditional). That is a measurement-design change needing its own Phase 0/1, not a V-4c footnote. Ledger entry with trigger: "when a second Destination lands, or a power argument for Trials>1 appears." |
| Q12 (ordering) | **Deterministic order + order-invariance test (uncapped) + truncation marker** | Δ is structurally order-invariant (`valuationFor` reads only (run, asset) measurements); the risk is cost and completion. Deterministic asset order (sorted by ID); the order-invariance test runs UNCAPPED — under a binding cap, which Assets complete depends on execution order by construction, so the assertion is on the intersection of Assets completed in both orders, or simply on uncapped runs; cap-truncation behavior gets its own test. Under a cap, uncompleted assets land `not_measured = REJECTION_REASON_BUDGET_EXHAUSTED` — the value already exists in the proto, no new enum — and the report names the truncated portfolio as truncated. |

### 12.5 Ledger entries created by this amendment

| Entry | What | Trigger |
|---|---|---|
| New | A7 sample-splitting deferred (Q11) | "When a second Destination lands, or a power argument for Trials>1 appears" |
| New | Asset-order dependence of per-Case cost under prompt caching (Q4/Q12); Δ order-invariance is tested, cost is not | "When `costOf` prices cache writes" — the born-lapsed candidate ("cache-read pricing is added") already holds: `openaicompat` prices cache reads and `anthropic` prices four cache tiers, so that trigger is a disposition, not a trigger. Cache WRITES (the first Case pays them) are unpriced today. |
| #68 (amended) | `context_tokens` tokenizer bias (Q9) — folded into the existing #68 entry, not a sibling | #68's trigger stands unchanged |
| #55 | Discharged by P1-5 (blended-model gate) — the discharge takes effect with the PR, conditional on the gate existing in it | — |
| #77 | Discharged by P2-12 (hook wiring + test in V-4c, including the signature-ripple update to `TestBaselineWiresEveryInvokerHook`) | — |
| #1 | Discharged by P0-2 as amended (delta_goal AND delta_control gated on their intervals) | — |
| #21 | Discharged by P2-13 (holdout canary via SealedEvals) | — |

Note on enforcement: `scripts/ledger-check.py` only fails triggers naming the release version; it deliberately never evaluates conditions or dates. The condition-form triggers above are enforced by the minor-release manual review CLAUDE.md requires — which is why each is a checkable condition, not prose.

### 12.6 Review record — pass 4

| Pass | Verdict | Outcome |
|---|---|---|
| 4 | **3 P0s + 4 P1s + 4 P2s**, all verified | The scaffolding was attacked as Phase-2 output without a Phase-3 pass. Three P0s are in the estimator's own arithmetic (direction, CI gate, harm-bound correlation); the P1s are the two money-consent holes the plan's own test bullets would have caught only after being written (quote check dead, resume re-pay) plus two schema gaps (baseline marker, event attribution). Amendment A. |
| 6 | **BLOCK, 1 blocker + 4 holes on the amended text** | All ten pass-5 findings verified resolved against the tree; the amendment introduced one new implementability blocker (P0-2 named a nonexistent `NotMeasuredReason` enum — `not_measured` is typed `RejectionReason` — and its "only three values" godoc amendment was missing from the complete list) plus a duplicated ledger row and a P1-6 self-contradiction. Fixed: enum named correctly, godoc amendment in scope item (1), duplicate removed, "removed vs KEPT" contradiction deleted in favor of the operative n<20 OR bound>ε condition, route.go's wrong-sd godocs added to the docs diff, fingerprint comparison scoped to what the recorded Plan expresses. |
| 5 | **BLOCK, 10 findings on Amendment A** | Five blockers: P0-2 unimplementable as written (enum absent, godocs unamended, DeltaControl ungated); P0-3's wording still permitted the flatten+trials shape it banned; P1-5's trials-equality gate refused the default configuration; P1-10's fingerprint missed Route inputs and the done-marker double-counted SettledSpend; one ledger trigger born lapsed and one entry duplicated #68. All amended: scope rule now enumerates the complete proto list, the trials gate is dropped in favor of the blended-model gate, resume consumes the recorded Plan, the done-marker replaces the orphan-spend write, and §2.4's own ±0.3 example is superseded alongside Q2's marker clause. Remaining objections accepted as stated consequences: the underpowered flag fires on nearly every real run against ε=0.10 (number-first reporting makes it information, not noise), and the order-invariance test is uncapped-only. |

