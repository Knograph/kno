# Value: measuring what each Asset is worth

**Status:** Phase 0, draft 2 — Phase 1 returned **BLOCK** with 5 P0s, all verified against the tree and all upheld. Three of them invalidated the decomposition itself.
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

**(b) The proto is not done, so "proto first is satisfied" was false** and the parallel decomposition rested on it. Three gaps: `Valuation` has no field for the `RejectionReason` §5 promises to record (carrying it in Select's `Rejection` would hand Select's rejection log to the wrong stage); there are no Asset-scoped events, so Value's user-visible state has nowhere to go and CLAUDE.md's parallelization table gates cli+tui on "event schema fixed"; and the sampling seed has nowhere to live, so a seeded sample is unauditable and unreproducible across a resume.

**(c) Draft 1's resume story was a money bug.** It said a cost cap mid-Asset writes no `Valuation` and the resume re-measures that Asset from scratch — so with `--cases-per-asset 50` and a cap binding at Case 40, the resume pays for 40 Cases again, and if the cap still binds it never completes one. With durable per-measurement rows, "no partial Valuation" is achieved by not writing the *Valuation* while the paid measurements survive, and resume is free. That is what "the recorded outcome IS the marker" exists for.

### 2.1 Paired differences against the recorded baseline

```
δ_i = score_with_asset(case_i) − score_baseline(case_i)
Δ   = mean(δ_i)
```

Pairing is right, and draft 1 overclaimed what it does. **It removes between-Case difficulty variance and leaves within-Case sampling noise** — it does not "remove between-Case variance entirely". For the only Goal in the tree that distinction is the whole story (§2.2).

It also removes the need to re-measure the control arm for the *routed* Cases, since Baseline already recorded them. It does **not** halve the run: draft 1 claimed a 2× saving against a naive figure that had no control arm in it, two paragraphs after quoting that figure.

**Comparability is a gate, and `resolved_models` is not enough.** `Run.generation` records temperature, seed, top_p, and `max_output_tokens`, and `input_fingerprint` explicitly does not cover them (`common.proto:278-283`). A baseline at `max_output_tokens=512` paired against a Value run at 2048 yields a δ that is truncation plus Asset. **The gate compares `Run.generation` and `resolved_models`**, and refuses on either.

**It also refuses a baseline marked unusable.** `what-the-numbers-mean.md` already commits to it: a run whose error rate exceeded its threshold is "not a usable baseline, so later stages refuse to treat it as a clean reference." Value is the first later stage. (Draft 1 asked this as an open question; a shipped doc had already answered it.)

**And #55 lands here.** A baseline whose model changed mid-run records *both* models in `resolved_models`, gets no `incomplete_reason`, and would pass a membership check trivially — Value would pair against a blended reference with nothing on the record. This is the PR #55's trigger names, so it either reads a marker or states in writing why prose suffices.

### 2.2 The interval method is decided by the data, and the data is binary

`goal/exactmatch` is the only Goal in the tree and returns 0.0 or 1.0. So `δ_i ∈ {−1, 0, +1}` — paired binary, the McNemar setting, where the information lives in the discordant counts (0→1 and 1→0).

**A percentile bootstrap is the wrong method here, and it fails in the direction that reads as certainty.** Two cases, both reachable on a first run:

- An inert Asset, `--cases-per-asset 20`, every `δ_i = 0`. Every resample mean is 0. The interval is **[0.000, 0.000]** — total confidence in an exact null from 20 binary observations. It does not cross zero, so the coloring rule in `what-the-numbers-mean.md` needs a special case it does not have.
- An Asset routed to 5 Cases, baseline 0 on all five, injected 1 on all five → **Δ = +1.000, CI [1.000, 1.000]**, and `delta_per_cost` puts it at the top of the greedy ranking from five coin flips.

`Interval` exists as a message specifically so its absence cannot be mistaken for a tight one — a zero-width interval defeats that.

**So V-1 implements an interval for paired binary proportions that cannot return zero width.** `Interval.method` already enumerates `"wilson"`; the schema anticipated this and draft 1 did not. The exact method (Wilson on discordant pairs, Agresti–Min, or an exact conditional interval) is §7 Q1 — it is a real choice with tradeoffs, not a detail.

**Neither adapter sends a temperature by default** (`Options.Temperature` is a pointer, "nil sends none"), and both *refuse* temperature on reasoning models — so temperature-0 is not merely un-default, it is unavailable for some models. Repeated trials are therefore the only route to a within-Case variance estimate, and they are a flag, not the default.

### 2.3 Sampling is the bound; routing narrows what is sampled

`--cases-per-asset N` draws a random sample of the routed Cases and is a hard bound on spend. Routing decides which Cases are candidates. Both feed the same interval, and the interval tells the truth about what the bound bought.

**Routing may not read per-Case baseline scores. This is an invariant with a test.**

DESIGN.md's Value bullet says routing "clusters **baseline failures**", and the obvious cheap implementation — route to the Cases the baseline failed — is a P0 statistical bug. Selecting Cases where the baseline scored 0 selects on the baseline's own random draw, so for a null Asset with per-Case success probability *p*, `E[δ | baseline=0] = p`. **At p = 0.7 a completely inert Asset measures Δ = +0.70 with a tight interval.** Regression to the mean, and pairing against a single recorded draw maximizes it.

Tag-overlap routing dodges this because tags are independent of the baseline draw. Draft 1 was accidentally safe and did not know it, which means the guard was one "obvious improvement" away from removal — with DESIGN.md on record recommending that improvement. So: the invariant is stated, tested, and `Store.CaseScores` is scoped so routing code cannot reach it.

Routing v1 is tag intersection, with an escape: an Asset with **no** tags routes to a sample of all dev Cases, because unlabelled is not irrelevant. `--route none` measures everything.

### 2.4 Controls must be measured, because un-routed Cases are identically zero

Draft 1 said "control Cases come free — every Case not routed is already a control." **That is arithmetically false.** For an un-routed Case the with-Asset score is never measured, so `δ_i = baseline − baseline = 0`, identically, for every un-routed Case and every Asset. `delta_control` would be a hard 0.000 with `control_interval` [0.000, 0.000] — not a regression signal, a constant that renders as "no regression, measured with perfect precision."

DESIGN.md and `docs/mental-model.md` both say the opposite in the same words: *"inject the asset, **re-run** the mapped dev slices **plus untouched control slices**."*

So controls are measured: `--control-cases-per-asset M`, sampled from Cases the Asset was **not** routed to, and **in the consent quote**. Real exposure is `assets × (cases-per-asset + control-cases-per-asset)`; draft 1's quote under-stated by the entire control arm, which is a prime-directive-4 defect.

Controls can take a smaller sample than the treatment arm: the question is "did this Asset break something it should not touch", and a coarse bound answers it. The default is not the same number (§7 Q2).

### 2.5 What Δ estimates, said out loud

Under routing, a tagged Asset's `delta_goal` is the mean effect **over the Cases it was routed to**, and nothing else. An untagged Asset's is the mean over all dev Cases. Two Assets with identical content get different estimands purely from user labelling, and Select then ranks them against each other on `delta_per_cost` as if commensurable.

Concretely: Asset A routed to 20 billing Cases, Δ=+0.30; Asset B untagged over 500 Cases, Δ=+0.05. A outranks B while B moves the eval set five times as much (25 Case-points vs 6).

The plan does not fix this — ranking heterogeneous estimands is Select's problem and needs its own design. It **names** it: `Valuation` records `n_routed` and `n_dev` so a reader can scale, and `what-the-numbers-mean.md` says that differently-routed Δs are not comparable while `delta_per_cost` ranks them anyway.

### 2.6 Multiplicity, named rather than ignored

`REJECTION_REASON_NO_EFFECT` is defined as "the confidence interval crosses zero", and `Rejection.detail`'s worked example is a per-Asset significance claim. So Select runs a per-comparison test N times: **with 200 null Assets, ~10 intervals exclude zero by construction**, and they concentrate exactly where `delta_per_cost` ranks highest — small n and small cost.

Holdout Validate catches this at the portfolio level and `what-the-numbers-mean.md` already explains the winner's curse for `dev_estimated_gain`. Neither covers the per-Asset claim.

V-1 states the error rate `delta_interval` controls (per-comparison), and **CLAUDE.md's required winner's-curse property test asserts on the selected set, not on one interval** — N null Assets, ground truth zero, checking what greedy selection surfaces.

### 2.7 Cost, and the denominator that is not a reservation bound

`pricing.countTokens` reserves ~3× what English prose uses, deliberately — correct for reserving money, wrong for a ranking denominator. Two Assets with identical real token cost, one prose and one base64, would differ ~3× in `context_tokens` computed that way, so **greedy Select would order them by content type.**

`CostVector.context_tokens` is computed for ranking, not from the reservation path, and §2.5's docs row says so. The reservation still uses the pessimistic bound, because under-reserving is the failure that costs money.

The Asset goes into `pricing.Prompt.Context` — a field that exists and is documented as empty until `ContextInjector` lands — so **the cost cap binds on the thing being measured.**

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
| **V-1** | `stats/interval` — paired-binary interval, winner's-curse property test | V-0 (for `method` vocabulary only) | No |
| **V-2** | `adapters/pool/jsonl` | — | No |
| **V-3** | `ContextInjector` on both adapters; Asset into `Prompt.Context`; `Capabilities.ContextInject` | V-0 | No (fixtures) |
| **V-4a** | Store: migration to `schemaVersion` 3, `measurements` table, `Valuation` writer, `CaseScores`, purge coverage | V-0 | No |
| **V-4b** | `core/value` routing + sampling, no spend, testable against the fake | V-0, V-2 | No |
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

**Q1.** Which paired-binary interval — Wilson on discordant pairs, Agresti–Min, or exact conditional? Decides V-1 and, through it, the default `--cases-per-asset`.

**Q2.** Default `--cases-per-asset` and `--control-cases-per-asset`. These set the stage's default cost and every interval's default width. Q1 comes first: the default follows from the method, not the reverse.

**Q3.** Does a Value Run write `CaseExecution`, and of what — distinct Cases or measurements? Resolving §2.8's contradiction between `run.proto` and ADR-0004.

**Q4.** System block or message body for the injected Asset? Decides prompt-cache behavior across a sample, which is priced separately.

**Q5.** `CaseScores` signature: `ScoreSum` returns `(sum, counted, unrecoverable)` because "scored but the number is gone" is a real state. A `map[string]float64` collapses it into "absent". Does Value need Baseline's refusal here?

**Q6.** `delta_per_cost` for a zero-cost Asset — all three `CostVector` terms zero. Undefined is the honest answer; a separate ranking tier is probably the right handling.

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

*(To be filled after the second Phase 1 review. Empty is not "none found".)*

## 11. Review record

| Pass | Verdict | Outcome |
|---|---|---|
| 1 | **BLOCK**, 5 P0s | All verified and upheld. Three invalidated the decomposition: the proto is not done, the store cannot hold the run, and the control arm was unbudgeted. Two decided V-1's method: the data is paired binary, and a percentile bootstrap returns zero-width intervals on it. Draft 2. |

The review checked eight of draft 1's claims about the tree and found all eight correct — the failures were in what was **inferred** from a correct reading, not in the reading. That is a different failure mode from the previous four plans and worth recording as such.
