# Closing the bridge loop: an exported measurement seam

**Status:** Phase 0 — plan. Not implemented.
**Depends on:** #184 (`feat/tuner-bridge`), which ships everything except the measurement.

## Problem

`kno bridge` submits fine-tuning jobs, polls them to terminal, deploys the
resulting model, tears the endpoint down, and refuses to do any of it. `bridge.Run`
requires an `EvalRunner` — the seam that invokes a deployed model over dev and
control Cases and scores it — and no production implementation exists, so the CLI
plans, prices, confirms, and then stops with an actionable error rather than
deploying a paid endpoint with nothing to measure.

That refusal is correct and it is also the whole feature. Two things block it:

1. **The invoke path is unexported.** Measuring means calling a provider under the
   budget guard with retries, panic recovery and settlement. `core.invoker`
   (`core/invoke.go`) does exactly that and is unexported. Its own doc lists **six
   separately-discovered money and correctness defects** held in place by their
   fixes: the retry budget measured against a real clock, billing accumulated
   across attempts, settled calls counted apart from attempts, a recovered panic
   carrying its spend out, saturating arithmetic matching `Guard.Settle`, and an
   overshoot recorded rather than returned. Re-implementing that in `bridge/` is
   how a third stage comes to disagree with the other two about money.
2. **`kno bridge` has no `--evals`.** The Case IDs in a `value.Plan` are
   identifiers; measuring needs the Case content behind them, and there is no flag
   that resolves one to the other.

This is also the last step to something the project has never done: run
Baseline → Value → Select → Validate → Export → **train** end to end. The
untrainable-tuning-set defect fixed in #183 — every tuning set the tool had ever
written lacked an assistant turn, so no provider could train on it — survived
every gate for months because **the format had no reader**. Closing this loop
creates the reader.

## Proposed design

### 1. `core.ScorePass` — the narrow seam

A new exported function in `core`, and the only new surface:

```go
// ScorePass invokes agent once per Case and scores each answer against goal,
// under the budget guard, with the same retry and settlement behaviour every
// other stage gets.
func ScorePass(ctx context.Context, p ScoreParams) (*ScoreResult, error)

type ScoreParams struct {
    Agent    Agent
    AgentRef *AgentRef
    Goal     Goal
    Cases    iter.Seq2[*Case, error]
    Guard    *budget.Guard
    Estimator Estimator          // optional; nil means the guard's fallback
    Concurrency int
    MaxAttempts int
    RetryBudget, RetryBackoff time.Duration
    OnOvershoot func(...)        // same signature invoker already takes
    OnRetry     func(...)
}

type ScoreResult struct {
    Scores map[string]float64   // Case ID -> score, direction-normalised
    Errors map[string]string    // Case ID -> error code, for Cases that failed
    Spent  budget.Spend         // all three dimensions
}
```

**Amended after Phase 1 review (finding R4).** The first draft returned a bulk
map and explicitly did not checkpoint. That is incompatible with how all three
existing `invoker` callers work: `core/baseline_invoke.go`, `core/value_loop.go`
and `core/validate_loop.go` each drive it through `executor.Run` with a `Skip`
predicate and a per-Case sink recording durably **as each Case completes** —
because the Case is the unit of spend. A bulk return cannot join that discipline,
and it is what makes §6's resume hole unfixable.

So `ScoreParams` gains the two fields that matter:

```go
    Skip     func(caseID string) bool
    OnScored func(ctx context.Context, caseID string,
                  score float64, spend budget.Spend) error
```

`ScorePass` still creates no Run — that belongs to the stage — but it no longer
treats checkpointing as someone else's problem. It calls `OnScored` before moving
on, and an error from it stops the pass.

### 2. Scoring the all-in model — the piece this plan originally got wrong

**Amended after Phase 1 review (finding R1, a blocker).** The first draft said
each group's deltas are paired against "the all-in baseline model's score for the
same Case, which `bridge.Run` already holds." That is false, and checking it
against `feat/tuner-bridge` is what caught it:

- `deployMeasureTeardown` returns **early** for `q.Group == AllIn`
  (`bridge/run.go:355-364`) without ever calling `p.Eval.Measure`. The all-in
  model is deployed and torn down, never scored.
- `baselineModel` (`bridge/run.go:180`) holds a `*knov1.AgentRef` — a model
  pointer, never a score.
- `--bridge-max-live-endpoints` defaults to 1 and deploy/teardown is serialised,
  so by the time any leave-one-out group runs the all-in endpoint is **gone**.
- `Measure`'s signature (`bridge/run.go:45`) takes no baseline scores.

There is no channel through which an implementation could obtain the number the
contract requires. Writing `evalRunner` to the original spec would leave an
implementer two options, both wrong: re-invoke a torn-down endpoint, or quietly
pair against nothing and emit a delta that means nothing.

**Amended design.** The all-in group is scored like every other group, and its
per-Case scores are the baseline the rest pair against:

1. `deployMeasureTeardown`'s `AllIn` branch calls `Eval.Measure` over the
   **union** of every group's dev Cases plus `ControlCaseIDs`, while the all-in
   endpoint is live. That is the only moment it exists.
2. Those scores are persisted — see §6; in-memory is not sufficient — and
   `RunParams` carries them forward for subsequent groups.
3. `EvalRunner.Measure` gains no baseline parameter. Pairing moves into
   `bridge.Run`, the component that legitimately holds both sides.

The cost consequence is explicit and must reach the quote (§7): the all-in pass
scores the union of every group's Cases, making it the **largest** single scoring
pass in the run, not a free extra.

### 3. `--evals` on `kno bridge`

The same flag grammar `kno baseline` and `kno validate` already accept, resolved
by the same `adapters/evals` registry. Bridge filters to the Case IDs the
`value.Plan` names; a Case ID in the plan with no Case behind it is a **refusal
before any spend**, not a silently smaller sample.

### 4. `BRIDGE_GROUP_VERDICT_INTERFERENCE`

#184 deliberately never emits this. Reading `interval.HarmBound` alone for a
"confirmed harmful" claim is unsafe: it is one-sided, and the honest question is
whether the *net* effect — goal delta combined with control delta — excludes
zero. `core/select.go`'s unexported `netInterval` already does that combination,
variance-weighted, with the covariance correction a recorded-baseline control arm
needs (`shared := !v.GetFreshControlArm()`).

Proposal: extract the combination into `stats/interval` as an exported
`NetEffect(goal, control *Interval, nT, nC int, shared bool, level float64) *Interval`,
and have both `core/select.go`'s `netInterval` and bridge call it.

**Amended after Phase 1 review (findings R5, R8, R9).** Three corrections:

- **`shared` is not an open parameter for bridge; it is `true`, always.** In
  Select it comes from `Valuation.FreshControlArm`. Bridge has no fresh/recorded
  distinction: under §2's amended design both Δ_group and Δ_control pair against
  the *same* all-in scores, which is structurally the shared case. An implementer
  reasoning "dev and control Cases are disjoint, therefore independent" would pass
  `false`, **narrowing** the interval and manufacturing precisely the
  false-confident-harm claim this extraction exists to prevent. Pinned here rather
  than discovered in Phase 3.
- **`NetEffect` performs the one-sided-to-two-sided widening internally** — the
  `halfC` quantile-ratio computation inline at `core/select.go:504-511`.
  `portfolio.NetDelta.Half`'s doc warns that silently reading a one-sided bound as
  symmetric understates the interval; the signature must not leave which side of
  that line it sits on to the caller.
- **The counter-precedent is acknowledged.** `bridge/run.go` already duplicates
  `core/value_measure.go`'s `perCaseMeans` deliberately, reasoning a divergence
  "would show up immediately as a wrong-shaped interval, not as a silent drift."
  That is sound *there* and does not transfer: a wrong `perCaseMeans` produces a
  visibly wrong shape, whereas a wrong net-effect combination produces a plausible
  interval of the wrong width. The distinction is observability of the error, not
  the presence of interval math.

This is an extraction at the **second** occurrence, which `CLAUDE.md` normally
forbids ("extract on the third occurrence, not the second"). The exception is
argued rather than assumed: the rule exists because a premature abstraction is
costlier than duplication, and that trade flips when the duplicate is a
statistical-validity claim. A subtly wrong copy here does not produce a bug that
looks like a bug — it produces a **confident false claim that an Asset is
harmful**, which is prime directive 5's failure mode. Duplication is cheaper than
the wrong abstraction; it is not cheaper than the wrong statistic.

### 5. Criterion 23 — recorded fixture poll sequences

#184 tested `ListJobs`/`ListEndpoints`/`Status` with inline `httptest` table
tests. The plan's Step 6 specifies on-disk `testdata/fixtures/poll-NN.json`
sequences. Convert them, recorded via `make record-fixtures` with secrets
scrubbed at record time, so the adapter's polling is pinned against real provider
payloads rather than against our idea of them.

### 6. Per-group measurement idempotency — resume must not re-pay

**Added after Phase 1 review (finding R2, a blocker).** `bridge.Run`'s loop gates
`deployMeasureTeardown` on one thing: the job reaching `JOB_STATUS_SUCCEEDED`
(`bridge/run.go:210-215`). Nothing records that a group has already been
**measured**.

Concrete failure: `cluster-3`'s job succeeds, its endpoint deploys, 250 Cases are
scored against a per-minute-billed endpoint, the verdict is emitted, the endpoint
is torn down. The process is killed while measuring `cluster-4`. On resume,
`cluster-3`'s job row is still terminal-succeeded so submission is correctly
skipped — and `deployMeasureTeardown` runs again unconditionally. A second
endpoint is deployed, all 250 Cases re-scored, and a second independently-sampled
verdict emitted for a group already reported, with nothing reconciling the two.
CLAUDE.md's "resume never re-pays", violated on the one path that bills by the
minute while idle.

Required: per-Case durable scores (§1's `OnScored`) plus a per-group completion
check before deploy, mirroring `CompletedMeasurements`/`Skip` in Value and
Validate. A resumed run re-deploys only if Cases remain unscored, and re-scores
only those.

### 7. The consent quote must include the eval pass

**Added after Phase 1 review (finding R3, a blocker).** `cli/bridge.go:220-229`
computes the quote as training plus a hosting cap. There is no inference line — in
`bridge.GroupQuote`, `TotalEstimatedCostUSDMicros`, or `confirmAndStop` — because
until now `confirmAndStop` always refused before any `EvalRunner` could spend.
**This plan is the first to wire a spending EvalRunner**, so it inherits the
obligation.

A user consenting to "$47.20 training + $12.00 hosting" would have inference calls
authorised and settled — the all-in union pass plus one per group over dev and
control Cases — that they were never shown a number for. Prime directive 4,
independent of whether `--max-cost-usd` eventually caps it.

Required before any scoring pass ships: a third quote dimension, estimated from
the Case count and the served model's price, rendered in the plan and included in
the confirm total.

### 8. Multiplicity across ablation groups

**Added after Phase 1 review (finding R6, a blocker).** The parent tuner-bridge
plan promises Bonferroni over N groups. `portfolio.Correct` appears **zero** times
in `bridge/`: `groupMeasuredEvent` computes `interval.PairedTrials` at the raw
level and decides off `goalIv.GetLow() > 0`. With six leave-one-out groups tested
independently at a raw 95%, family-wise false-positive risk is roughly 26%.

Bridge is in the **screening** regime, like Select (`core/select.go:298-305`
corrects over `nScreened`), not the pre-registered regime Validate is deliberately
in — it tests every group and reports the ones that clear. It takes the same
correction, and it must land with the verdict logic rather than after it.

### 9. Holdout isolation needs a choke point and a canary

**Added after Phase 1 review (finding R7).** `ScoreParams.Cases` is a bare
`iter.Seq2[*Case, error]`, unlike Baseline and Value which take `*SealedEvals` so
that forgetting to seal is a **compile-time** error. `ScorePass` cannot enforce
sealing on an arbitrary iterator.

Required: name the choke point — whatever resolves `--evals` for bridge calls
`core.Seal`, as `cli/baseline.go` and `cli/value.go` do — and add the bridge
equivalent of `TestSelectHoldoutCanary`, which the parent plan's acceptance
criterion 20 already promises and this plan's test list omitted.

## Alternatives considered

**A. Export `core.Invoker` (the struct) with `WithRetry`.** Rejected: it exposes
the hooks, `store.MeasurementKey` and `budget.Estimate` on the public API, making
every future change to the retry machinery a breaking change pre-1.0. The seam
callers need is "score these Cases", not "here is a retry loop".

**B. Reuse `BaselineOptions.Baseline()` per ablation group.** Genuinely
attractive — it is exactly "run an agent over Evals and score" and it already
checkpoints. Rejected because it creates a `Run` row per group with
`STAGE_BASELINE`, which makes the run table lie about what happened, and because
Baseline owns holdout sealing (`*SealedEvals`) that has no meaning here. A stage's
run record is a claim about provenance; borrowing one to get its side effects
corrupts that claim.

**C. Move `invoker` to a module-root `internal/invoke` package.** Rejected: it
relocates the six-defects code and every one of its tests to satisfy a single new
caller, and `internal/` at the module root is importable by everything, which is a
wider blast radius than one exported function.

**D. Leave the loop open; ship bridge as plan-and-price only.** Rejected because
it is the status quo and it means the export format still has no reader — the
condition that let #183's defect survive.

## Affected packages

| Package | Change |
|---|---|
| `core` | **new exported** `ScorePass`, `ScoreParams`, `ScoreResult`. No change to `invoker` itself. |
| `stats/interval` | **new exported** `NetEffect`; `core/select.go`'s `netInterval` becomes a caller. |
| `bridge` | `evalRunner` implementation; `Run` wired to emit INTERFERENCE. |
| `cli` | `--evals` on `kno bridge`; the flag is refused if it names no Evals source. |
| `adapters/tuner/together` | fixture-backed poll tests. |
| `docs` | `what-the-numbers-mean.md` gains what a bridge verdict claims. |

## Proto / schema impact

**None.** `BRIDGE_GROUP_VERDICT_INTERFERENCE` already exists in the enum; this
plan makes it reachable. No new messages, no field-number changes, `buf breaking`
unaffected.

## Edge cases

1. **A Case ID in the plan with no Case in `--evals`.** Refuse before spend,
   naming the Case. A quietly smaller sample changes what every downstream
   interval means.
2. **The deployed endpoint fails mid-pass.** `ScorePass` returns partial `Scores`
   plus `Errors`; bridge reports the group `unknown` rather than computing a
   delta over the Cases that happened to succeed — selection on success is the
   winner's curse with extra steps.
3. **The budget cap is reached during a scoring pass.** The guard refuses,
   `ScorePass` returns what it settled, and bridge's existing mid-hosting
   teardown path (fixed in #184's follow-up) applies unchanged.
4. **Zero overlap between a group's dev Cases and the Evals provided.** Refuse
   the run, not the group: it means the wrong Evals were passed.
5. **The all-in baseline model scores a Case the ablation model errors on.** That
   Case drops from the pair set; the pair count travels with the interval, so the
   report says how many Cases the claim rests on.
6. **`NetEffect` with a control interval of fewer than two pairs.** Returns nil,
   matching `netInterval`'s existing guard; the verdict falls back to
   non-INTERFERENCE rather than being computed from a bound that does not exist.

## Test plan

- `core`: `ScorePass` unit tests — budget refusal mid-pass, a panicking Agent, a
  retryable error, all three spend dimensions recorded, and a test asserting
  `ScorePass` writes no Run and no measurement rows.
- `stats/interval`: `NetEffect` property tests, plus a **characterization test
  asserting `NetEffect` and the pre-extraction `netInterval` agree** on the
  Valuation shapes `core/select.go` feeds it. The extraction is only safe if it
  is provably behaviour-preserving.
- `bridge`: `evalRunner` against a fake Agent; an INTERFERENCE verdict that fires
  and one that correctly does not.
- `cli`: `--evals` resolution, and the refusal when a plan Case has no Case.
- `adapters/tuner/together`: fixture-backed poll sequences (criterion 23).
- **End to end, opt-in:** a `KNO_LIVE_TESTS=1` nightly test that runs the whole
  loop against a real provider with a capped budget. This is the test that would
  have caught #183.

## Rollback

`ScorePass` and `NetEffect` are additive; `netInterval` delegating to `NetEffect`
is the only change to existing behaviour and is covered by the characterization
test. Reverting means bridge returns to refusing to start — the current state,
which is safe.

## Docs impact

- `docs/what-the-numbers-mean.md` — what a bridge group verdict claims, and what
  INTERFERENCE means specifically.
- `docs/mental-model.md` — the Bridge stage.
- Godoc on `ScorePass` and `NetEffect`.
- `CHANGELOG.md` under `Unreleased`, flagged as a behaviour change.
- The recipes live in `uknoAI/kno-examples` — a **companion PR**, not
  `docs/cookbook/*`, which are tombstones enforced by
  `scripts/cookbook-stub-check.sh`.

## Phase 1 review outcome

The first draft was reviewed adversarially and **did not pass**. Six findings were
blockers, and the lead one falsified the plan's central data-flow claim: the
all-in model's per-Case scores, which §2 asserted `bridge.Run` "already holds", do
not exist anywhere — the all-in group returns before `Eval.Measure` is ever
called. Every blocker was verified against `feat/tuner-bridge` before amendment
rather than accepted on the reviewer's word.

Amendments: §2 rewritten around scoring the all-in model, §1's seam given `Skip`
and `OnScored`, and four new sections (§6 resume idempotency, §7 the consent
quote, §8 multiplicity, §9 holdout sealing).

**This plan needs a second adversarial review before Phase 2.** The amendments are
substantial enough that a review of the first draft says little about the second,
and §2's redesign changes `bridge.Run`'s control flow rather than only adding to
it.

## Second Phase 1 review outcome — Phase 2 remains blocked

The amended plan was reviewed again and **also did not pass**. Two of the new
blockers were introduced *by the first round of amendments*, which is the signal
worth recording: each amend cycle has added unreviewed design at the same rate it
retired reviewed design.

**B1 — `EvalRunner.Measure` cannot express §2.** Its signature returns
`(goalDeltas, controlDeltas [][]float64)` (`bridge/run.go:45`) — positional
slices carrying no Case ID. §2 moves pairing into `bridge.Run`, which therefore
needs the all-in pass's per-Case scores *keyed by Case*. The union pass's ID list
has different membership and order from any single group's, and the divergence is
guaranteed rather than hypothetical: `core/value/route.go:638-646` assigns a Case
to one cluster **per tag**, so a Case tagged `["refunds","billing"]` is in both
groups' dev sets. Aligning "slot i of the union pass" with "slot j of group G's
pass" is then wrong, and wrong in the worst way — a plausible interval of the
wrong width, which §4 itself names as the failure mode that justifies extracting
`NetEffect` at all.

**Δ_group is not well-defined as specified.** `Measure` must return Case-ID-keyed
scores (mirroring §1's own `ScoreResult.Scores`) before any Phase 2 workstream
can start, because `core`, `bridge` and the store question below all depend on
that shape. This is the "proto first" rule applied to an interface that is not a
wire type.

**B2 — §7's inference estimate collides with a deliberate engine rule.**
`core/ring0.go:122-124`: *"a zero estimate makes a dollar cap unenforceable, so
the engine treats a zero-cost answer under a cost cap exactly as it treats an
error."* A dedicated endpoint is reserved capacity billed per minute, which is
what `ServePrice`/`EstimateServeCap` already assume. If inference on it carries no
separate per-call charge, the honest per-Case estimate is **zero** — and the
engine will run-fatal-refuse every Case under a cost cap. §7 asked for "a third
quote dimension" without establishing that a third dimension exists.

The question is empirical and must be answered before any CLI or quote work:
**does a Together dedicated endpoint bill per token on top of per minute?**

- If **no**: there is no third quote dimension; the existing training + hosting
  quote is already complete, and the real problem is that the scoring pass must
  not be charged twice — the ticker already meters it. That needs a stated
  mechanism for running `ScorePass` whose spend is accounted elsewhere, without
  tripping the zero-cost refusal.
- If **yes**: new price tables keyed by **base** model are required, since a
  per-run-generated endpoint id can never appear in a static table
  (`adapters/agent/pricing/table.go`), plus a two-rate CLI escape hatch, since
  `openaicompat`'s `applyPrice` requires input *and* output rates rather than the
  single scalar `resolveTrainPrice`/`resolveServePrice` use.

Refusing until priced is right for the second branch and actively wrong for the
first — it would make the feature unshippable against exactly the provider shape
the parent plan targets.

**Also to fix, one level down from where the first review looked:** resume must
recompute and emit the verdict for a group whose Cases are all durably scored but
whose event was never recorded, or a paid-for measurement silently vanishes
(prime directive 5 by omission); the store schema for per-Case bridge scores is
unspecified and the obvious reuse stuffs a group name into `MeasurementKey.AssetID`,
which is vocabulary rather than a free string; §8 must state that the Bonferroni
family is **goal-only** (`portfolio.Correct` refuses one-sided input, so it
structurally cannot touch the control interval) and that N is pinned at quote
time, since a dynamic N makes a group's verdict depend on how many *other* groups
had failed by the time it was measured; and §9's convention-plus-canary is the
design `core/seal.go`'s own history records as tried and rejected, when
`ScoreParams.Cases` could simply take `*SealedEvals` for the price of one internal
call.

## Status

**Phase 2 does not start.** Two questions must be answered first, and neither is a
plan-text edit:

1. `EvalRunner.Measure`'s return shape — Case-ID-keyed, decided and written down.
2. Whether dedicated-endpoint inference is a real cost dimension or zero by
   construction.

Two review rounds have each surfaced blockers of the same class — an unreviewed
data-flow or pricing claim that does not survive contact with the types. A third
amend-and-review cycle is not obviously the cheapest way to converge; reducing
scope is worth considering, starting with whether the union-pass optimisation
earns its complexity against simply scoring the all-in model once per group.

## Accepted risks

*None yet. The plan is not approved, so there is nothing to accept.*
