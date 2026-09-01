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

`ScorePass` is a thin wrapper over the existing `invoker` and the existing
concurrency helper. It does **not** create a Run, write measurements, or
checkpoint — those belong to the stage that owns the run.

### 2. `bridge.evalRunner` — the implementation

Lives in `bridge/`, implements `bridge.EvalRunner`, and is roughly:

- build an `Agent` for the deployed `core.Endpoint` via
  `adapters/agent/openaicompat` (Together's dedicated endpoints speak the
  OpenAI-compatible API; the adapter already exists);
- call `core.ScorePass` twice per group — once over `devCaseIDs`, once over
  `ControlCaseIDs`;
- pair each Case's score against the all-in baseline model's score for the same
  Case, which `bridge.Run` already holds;
- return the `[][]float64` shape `EvalRunner.Measure` specifies.

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

## Accepted risks

*To be filled by Phase 1 review, and mirrored to `docs/debt.md` with triggers.*
