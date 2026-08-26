# Value: measuring what each Asset is worth

**Status:** Phase 0, draft 3 — two Phase 1 passes, both **BLOCK** (5 P0s, then 4). Every finding verified against the tree; all upheld. Pass 2 changed the measurement design itself, not a detail of it.
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

**Comparability is a gate**, and `resolved_models` alone is not enough: `Run.generation` records temperature, seed, top_p, and `max_output_tokens`, and `input_fingerprint` explicitly does not cover them (`common.proto:278-283`). A baseline at `max_output_tokens=512` paired against a Value run at 2048 yields a δ that is truncation plus Asset. The gate compares both, and refuses a baseline marked `error_rate_exceeded` — `what-the-numbers-mean.md` already commits later stages to that refusal, and Value is the first later stage.

**A model gate runs on the Value run itself.** `modelGate` exists to catch a model changing *mid-run*, and a Value run is ~100× the wall-clock of the Baseline that produced it, so a provider rollover is likelier — and a blend splits δ across two models *inside one Asset's sample*. #55 records that when the gate fires the tripping Case is still recorded and a second resume completes a blended run with nothing on the record; §7 Q8 asks whether `modelGate` is exported from `core` or reimplemented.

### 2.2 The interval method dispatches on the data, not on an assumption about the Goal

`goal/exactmatch` is the only Goal in the tree and returns 0.0 or 1.0, so today `δ_i ∈ {−1, 0, +1}` — paired binary, the McNemar setting, where the information lives in the discordant counts.

**A percentile bootstrap is wrong for that, and it fails in the direction that reads as certainty.** An inert Asset over 20 samples with every `δ_i = 0` gives **[0.000, 0.000]** — total confidence in an exact null from 20 binary observations. An Asset routed to 5 Cases that flips all five gives **Δ = +1.000, CI [1.000, 1.000]**, top of the greedy ranking from five coin flips. `Interval` exists as a message so its *absence* cannot be mistaken for a tight one; a zero-width interval defeats that.

**But "the data is binary" is an assumption with a short half-life, and draft 2 hard-coded it.** Two things falsify it without a new package landing:

- **`trials` is in this very plan and in shipped proto** (`Valuation.trials`). With *k* trials `δ_i` takes *k+1* values, not three — and McNemar, Wilson-on-discordant-pairs, and exact-conditional intervals are all defined on 2×2 discordant **counts** and are undefined on fractional discordance. Draft 2 named the flag and chose an incompatible method in the same section.
- **`Score.value` is a `double`** and judged Goals are on the v0.2 path. The moment one lands, either the paired-binary estimator is called on continuous δ (invalid, silently) or there is no interval — and prime directive 5 then means **`delta_goal` is not reported at all**.
- A `DIRECTION_MINIMIZE` Goal (latency, cost) is not on [0,1] in the first place.

**So `stats/interval` dispatches on the observed support of δ:**

| Observed δ | Method |
|---|---|
| ⊆ {−1, 0, +1} and `trials == 1` | Score-based paired-binary interval (§7 Q1) |
| anything else | A paired continuous interval — studentised or BCa bootstrap, or a t-interval on the differences |
| neither applies (n < 2, degenerate) | **Refuse**: return no `Interval`, which the schema is built to represent |

A refusal means `delta_goal` is not reported, and `kno value` says which Assets were unmeasurable and why rather than printing a bare number. A ledger entry is triggered on **the first non-binary Goal**, so the judge PR cannot land without touching this.

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

So controls are measured: `--control-sample-rate`, drawn from Cases the Asset was **not** routed to, and **in the consent quote**.

**The quote is a ceiling and it carries every multiplier:**

```
assets × (routed_sample × arms + control_sample) × trials
```

where `arms` is 2 when routing conditioned on the baseline outcome (§2.1) and 1 otherwise. Draft 2 fixed the missing control arm and then **omitted `trials` — the same defect, in the same sentence, one multiplier over.** DESIGN.md's worked example uses 3 trials, so the quote would have under-stated by 3×.

Stated as a ceiling, because routing may send an Asset to fewer Cases than the sample allows and no pre-run number can know. §6 asserts `quoted ceiling ≥ actual settled calls` for a run with all three flags non-default. Because that selection is not conditioned on the baseline outcome, the recorded baseline is a valid control there (§2.1) — the control arm costs one measurement per Case, not two.

**Draft 2 then argued a smaller control sample was fine because "a coarse bound answers it". That is wrong, and in the dangerous direction.** A two-sided interval does not answer "did this break something"; it answers "is the control effect distinguishable from zero". At M=10 paired binary observations the interval is roughly ±0.3, so a true −0.10 regression returns an interval spanning zero — and `what-the-numbers-mean.md`'s shipped rule colors deltas by whether the interval crosses zero, so it renders as **not a regression**. An underpowered harm test that looks identical to a passed one is worse than no test.

So:

- The control quantity is a **one-sided upper confidence bound on harm**, not a two-sided interval.
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

**A6 — Leave `delta_control` absent and skip the control arm.** Considered seriously — it halves the stage's cost and `Interval`'s absence is representable by design. Rejected because the regression signal is the thing that catches an Asset that helps its slice and breaks another, and DESIGN.md promises it. Recorded as the lever to pull if §7 Q2's cost proves unacceptable.

## 4. The PR decomposition

Draft 1's V-4 was the entire stage in one branch. Split, with proto first per CLAUDE.md's coordination rule:

| PR | Scope | Depends on | Spends? |
|---|---|---|---|
| **V-0** | **Proto:** rejection reason on `Valuation`, Asset-scoped events, sampling seed, `n_routed`/`n_dev`; resolve §2.8 | — | No |
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
| Asset exceeds `--max-prompt-bytes` | Refused per Asset, recorded, run continues. **Not** the model's context window — the fix line must name the knob that actually bound |
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

**Q2.** Defaults for `--sample-rate`, `--control-sample-rate`, and `--trials`. Q1 comes first — the treatment default follows from the method. The control default follows from the harm size worth catching (§2.4), not from the treatment default.

**Q3.** What does a Value Run write into `CaseExecution` **and into `Run`'s own `attempted`/`scored`/`errored` counts**? Those are separate fields the API serializes for every stage, and for a Value run whose rows live in `measurements` they close at zero — a Run record asserting the stage did nothing. And what does `error_rate_exceeded` mean when 0 of 0 Cases scored? Resolving §2.8.

**Q4.** System block or message body for the injected Asset? Decides whether provider prompt caching hits across an Asset's whole sample, which is priced separately.

**Q5.** `CaseScores`'s signature. `ScoreSum` returns `(sum, counted, unrecoverable)` because "scored but the number is gone" is a real state; a `map[string]float64` collapses it into "absent". Does Value need Baseline's refusal, or does it drop and report?

**Q6.** `delta_per_cost` for a zero-cost Asset — all three `CostVector` terms zero. Undefined is honest; a separate ranking tier is probably the handling.

**Q7.** `--route none` leaves no un-routed set, so the control arm silently disappears. Refuse the combination, or draw controls from the complement within the full dev split?

**Q8.** Is `modelGate` exported from `core` for V-4c's use, or reimplemented in `core/value`?

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
| R3 | **Kno cannot detect user-side conditioning on the baseline** (§2.3) — tags assigned after reading a failure report, a pool assembled from failures, a second `kno value` run informed by the first. Stated on the epistemics page rather than guarded | None: this is a permanent limit, and the entry records it as "won't fix" with the rationale in the ADR |
| R4 | **No net-loss judgement.** `valuation.proto:96` promises one; combining treatment and control across two differently-sized populations is not computed | The Select stage, which is where a net judgement is acted on |
| R5 | **`context_tokens` carries the tokenizer's bias** (§2.7, Q9) unless Q9 resolves to a real tokenizer. The ranking denominator is content-type-dependent | Q9's resolution, or the first report of a mis-ranked pool |
| R6 | **A Goal that errors mid-valuation is a third attrition channel** alongside the two in §5, correlated with the treatment for the same reason (bigger Asset → more timeouts) | V-4c must decide `REJECTION_REASON_MEASUREMENT_FAILED` vs a `Valuation` over a shrunken pair set |

## 11. Review record

| Pass | Verdict | Outcome |
|---|---|---|
| 1 | **BLOCK**, 5 P0s | All verified and upheld. Three invalidated the decomposition: the proto is not done, the store cannot hold the run, and the control arm was unbudgeted. Two decided the interval method. Draft 2. |
| 2 | **BLOCK**, 4 P0s | All verified and upheld, and one changed the measurement design. Draft 3. |

**What pass 2 changed, and it is the important one.** Draft 2 concluded that routing may never condition on the baseline outcome, and rejected DESIGN.md's failure-clustered routing to enforce it. The bias is real, but the cause is **reusing the selecting draw as the control**, not the conditioning — so a fresh control arm on the routed slice removes it entirely. Draft 2's over-correction had silently overturned DESIGN.md's headline cost model (8,250 calls, $15–40) without the stop-and-flag CLAUDE.md requires. Draft 3 restores failure routing and keeps the arithmetic.

**The failure pattern, now twice.** Both passes found the readings of the tree accurate and the *inferences* one step short: the store diagnosis stopped at the write path and missed four readers, the control-arm fix stopped at the quote and missed `trials`, the routing finding stopped at the code path and missed the user. Reading correctly is necessary and has not been sufficient.

**And the un-failable test appeared in a plan this time.** Draft 2's resume test asserted `SettledSpend` equality on a method that structurally cannot see a Value run's spend — `0 == 0`. That is the same class Phase 3 caught four times during M2-11, arriving one stage earlier.
