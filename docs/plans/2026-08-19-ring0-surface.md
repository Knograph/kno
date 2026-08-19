# Plan: Ring-0 Go Surface (Milestone 0c)

- **Date:** 2026-08-19
- **Branch:** `feat/ring0-surface`
- **Status:** Phase 1 complete — amended after two adversarial passes, both BLOCK
- **Author:** DeVaris Brown
- **Depends on:** M0a (#1, merged), M0b (#2, merged), shell-safety fix (#7)

## Problem statement

`main` now has the gate machinery and the `kno.v1` schema. It has no Go API.

M0c lands the five Ring-0 contracts every adapter implements, the error grammar the CLI and API
share, the budget guard, and the two internal tools (`covercheck`, `godoccheck`) whose absence
currently makes two gates report `PEND`.

This is the last milestone before real work — M1 (Baseline) needs all of it. It is also the point
where three things that have been *asserted* become *enforced*:

1. `core` imports nothing above it (an import-boundary test).
2. Coverage floors (85% on `core`, `stats`, `bridge`, `plugin`; 70% repo-wide) and the no-decrease
   ratchet — enforcing means M0c's own new code must meet the floor.
3. Every exported symbol is documented (`godoccheck`).

### Non-goals

- Any pipeline stage. No Baseline, Value, Select, Validate, Export.
- Any adapter implementation. M0c defines the interfaces; `adapters/` stays `doc.go` only.
- `store/`, `executor/`, `tui/`, `api/`, `judge/` — untouched.
- Checkpoint persistence in `stats/budget` (needs `store/`). Deferred to M1 with the
  double-spend-on-resume test.
- `event.proto` and the `Run` resource — still deferred to M1, per Phase-1 finding A5.

## Milestone structure — four PRs, not one

Both reviewers found that `covercheck`/`godoccheck` import none of `core`, `core/errs`, or
`stats/budget` — they are standalone tools bundled into a branch whose hard design questions all
live in the budget guard. And Alternative C's rejection ("floors must bind on the first PR with
code worth measuring") is satisfied by the tools merging *before* that code, not necessarily
*with* it.

| PR | Branch | Contents | Why separate |
|---|---|---|---|
| **M0c-1** | `feat/goal-direction` | `Direction` enum + `Report.goal_direction` | Proto-first: Go work below depends on it |
| **M0c-2** | `feat/coverage-tools` | `covercheck`, `godoccheck`, `.coverage-baseline` | Retires two `PEND`s independently of the guard's design |
| **M0c-3** | `feat/ring0-contracts` | `core`, `core/errs`, `coretest` | The frozen contracts |
| **M0c-4** | `feat/budget-guard` | `stats/budget` | Highest-stakes code; deserves its own review |

## Proposed design

### 1. `core` — the five Ring-0 interfaces

`core` is the public Go API and imports nothing above it. Interfaces exactly as `DESIGN.md` shapes
them, with the two deviations the user already confirmed in the M0 plan (`iter.Seq2`, pointer
element types):

```go
type Agent interface {
    Invoke(ctx context.Context, c *Case) (*Response, error)
}

type Capable interface {
    Capabilities() *Capabilities
}

type ContextInjector interface {
    WithContext(a *Asset) (Agent, error)
}

type KnowledgeInjector interface {
    WithKnowledge(ctx context.Context, a *Asset) (Agent, func() error, error)
}

type Evals interface {
    Cases(ctx context.Context) (iter.Seq2[*Case, error], error)
}

type Pool interface {
    Assets(ctx context.Context) (iter.Seq2[*Asset, error], error)
}

type Goal interface {
    Score(ctx context.Context, c *Case, r *Response) (*Score, error)
}

type Tuner interface {
    Submit(ctx context.Context, job *TuningJob) (*JobRef, error)
    Status(ctx context.Context, ref *JobRef) (*JobState, error)
    Model(ctx context.Context, ref *JobRef) (*AgentRef, error)
}
```

**Type aliases** per ADR-0001 — generated messages *are* the domain types:

```go
type (
    Case = knov1.Case
    Response = knov1.Response
    Score = knov1.Score
    Asset = knov1.Asset
    Valuation = knov1.Valuation
    Portfolio = knov1.Portfolio
    Report = knov1.Report
    Capabilities = knov1.Capabilities
    AgentRef = knov1.AgentRef
    TuningJob = knov1.TuningJob
    JobRef = knov1.JobRef
    JobState = knov1.JobState
)
```

### 1a. Goal direction — a proto gap this milestone must close first

Phase-1 finding: `DESIGN.md`'s vocabulary defines **Goal** as "the outcome metric, **with
direction**", and two merged proto comments lean on that — `Score.value` says "Direction is defined
by the Goal, not assumed here", and `Valuation.delta_goal` says its sign is "relative to the Goal's
own direction". **Direction is defined nowhere**, in Go or on the wire.

That is not only a Go gap. `Report` carries `goal_name` (a bare string), so an SDK consumer holding
a `Report` cannot tell whether `holdout_gain = -0.03` is an improvement or a regression. The
headline number's sign is uninterpretable.

M0c-1 lands, additively (no field renumbered, nothing removed):

```protobuf
// Direction is which way is better for a Goal.
enum Direction {
  DIRECTION_UNSPECIFIED = 0;
  DIRECTION_MAXIMIZE = 1;  // higher Score.value is better (accuracy, resolution rate)
  DIRECTION_MINIMIZE = 2;  // lower is better (latency, cost, escalation rate)
}
```

referenced by `Report.goal_direction`, and mirrored by a Go method on the Ring-0 interface:

```go
type Goal interface {
    Score(ctx context.Context, c *Case, r *Response) (*Score, error)
    Direction() Direction
}
```

Adding a method to `Goal` after Ring-1 adapters exist would break every implementation, so it lands
now. `goal_name` is kept rather than replaced by a richer message — replacing it would be a
breaking change for no gain this milestone.

### 1b. Why `Evals.Cases` still returns one undifferentiated stream

Phase-1 finding: `Cases` hands out dev and holdout `Case`s through a single iterator, with nothing
at the interface level stopping a caller from scoring holdout early. The only planned protection is
a runtime canary test deferred to M1. Since this milestone is billed as the last cheap moment to
reshape Ring 0, the alternative deserves an explicit answer rather than silent inheritance from
`DESIGN.md`'s sketch.

**Considered:** splitting the interface so holdout access requires a distinct, harder-to-reach
accessor — e.g. `Cases()` yielding dev only, plus a `HoldoutCases()` that a Validate-stage token
unlocks.

**Rejected, for now.** Adapters are the wrong layer to enforce a *pipeline-stage* invariant. An
`Evals` adapter reads a JSONL file; it has no idea which stage is running, so any seal it enforced
would be advisory anyway, and the engine could bypass it by calling the other method. Worse, it
would push a statistical guarantee into the ring that external contributors implement — the layer
with the least context and the most implementations.

The seal belongs where the stage is known: the engine wraps `Evals` and refuses to surface
`SPLIT_HOLDOUT` cases before Validate. That keeps one interface for adapter authors and one
enforcement point for the invariant. **`Split` is already on the wire**, so the information needed
is present regardless.

Recorded as an accepted risk with an M1 trigger: the wrapping seal and its canary test must land
*with* the Baseline stage, not after it. If M1 finds the wrapper insufficient, changing `Evals` is
still pre-1.0 and permitted with a CHANGELOG notice — but it will cost more than it would today,
and that is the risk being accepted.

### 2. The iterator contract, written and enforced

The godoc on `Cases`/`Assets` states the contract the M0 plan settled (Phase-1 finding G5):

- **Any yielded error is FATAL.** The consumer must stop ranging. Adapters that tolerate malformed
  records handle them internally and report counts via `Provenance` — never by yielding a skippable
  error. One rule, so the denominator behind every confidence interval cannot vary by adapter.
- **Producers defer cleanup inside the iterator closure.** Early `break` is legal.
- **Producers check `ctx.Err()` before each yield.**
- **Output is borrowed for one iteration.** Clone before retaining or mutating (debt 8).

`coretest.ConformIterator` is a shared harness every adapter must pass:

| Assertion | Catches |
|---|---|
| break-after-N closes the underlying resource | leaked fds/connections on early exit |
| context cancel halts within one yield | unresponsive producers |
| a fatal error stops iteration | skip-vs-halt drift |
| no goroutine leak after the loop ends | background producers outliving the range |

Goroutine leaks are checked with `go.uber.org/goleak`. **New dependency, justified in the PR:**
hand-rolled goroutine counting is racy and every attempt to do it well reimplements goleak.

### 3. `core/errs` — the error grammar

`Actionable{Code, Message, Fix, DocsURL, ExitCode}` implementing `error`, converting to and from
`knov1.Actionable`. Sentinels: `ErrBudgetExceeded`, `ErrHoldoutSealed`, `ErrCapabilityUnsupported`,
`ErrCheckpointStale`, `ErrRateLimited`.

Exit codes live here as typed constants so CLI and API cannot disagree:
`ExitOK 0` · `ExitError 1` · `ExitBudgetStopped 2` · `ExitValidationFailed 3`.

**`Error()` renders the grammar in order — what failed → why → the fix.** A golden test pins the
rendering, because this string is the product surface for every failure.

**Identity must survive the wire (Phase-1 finding, blocking).** `errors.Is` compares by pointer
equality or a custom `Is` method. An `Actionable` rebuilt from proto bytes is a different Go value
than the sentinel, so `errors.Is(reconstructed, errs.ErrBudgetExceeded)` returns **false** by
default — and every place that crosses a boundary (plugin subprocess, persisted checkpoint, API
round-trip) and then branches on the error would silently misclassify it. Budget and holdout errors
are exactly the ones whose misclassification matters most.

Therefore `Actionable` implements:

```go
func (a *Actionable) Is(target error) bool  // compares Code, not identity
```

and the test plan gains the case that catches it: construct an `Actionable`, marshal to proto,
unmarshal, and assert `errors.Is(rebuilt, ErrBudgetExceeded)` is still true.

**Two distinct notions of cause, deliberately separated.** `knov1.Actionable.cause` is a *string*
— the upstream error verbatim, for the wire. But `Unwrap() error` must return an `error`, and
naively returning `errors.New(a.cause)` would mean an `Actionable` with no upstream cause unwraps
to a non-nil error, breaking every `errors.Unwrap(err) == nil` check. So:

- an unexported `wrapped error` field carries the real chain in-process, and `Unwrap` returns it
  (nil when there is none);
- `cause string` is populated *from* `wrapped` when converting to proto, and on the way back it
  populates `Message`/`cause` only — a rebuilt error has no Go chain, which is honest, because the
  chain genuinely did not survive serialization.

### 3a. Retries and rate limiting

`CLAUDE.md`'s Performance section requires "per-provider rate limiters honoring `Retry-After`", and
nothing in Ring 0 expresses transience. `ErrRateLimited` is added to the sentinel set now, since
adding a sentinel later is additive but *classifying* a provider 429 correctly from day one is not
something M1 should have to retrofit. Where the retry policy itself lives (adapter, executor, or a
shared middleware) is an M1 decision — flagged as an accepted risk rather than pre-solved here,
because designing it without a real adapter is exactly the speculative work finding 5 warns about.

### 4. `stats/budget` — the spend guard

Prime directive 4 makes an unguarded spend path a P0 bug. Both reviewers attacked this hardest, and
the original sketch had a defect that made its own stated invariant unimplementable.

**The signature must correlate a reservation to its settlement.** The original
`Authorize(ctx, Estimate) error` / `Record(Spend)` pair carried no handle, so with N concurrent
workers the guard could not know which outstanding reservation a `Record` was settling. Since
actual spend almost never equals the estimate (LLM output length is unpredictable), a global
`reserved -= actual` is arithmetically wrong the moment settlement order differs from authorization
order — which it always will under a worker pool. Corrected:

```go
type Guard struct{ /* ... */ }

func New(limits Limits, confirm ConfirmFunc) *Guard

// Authorize reserves headroom. The returned Reservation MUST be settled or
// released; `defer res.Release()` immediately after is the sanctioned idiom.
func (g *Guard) Authorize(ctx context.Context, est Estimate) (*Reservation, error)

// Settle converts a reservation into recorded spend.
func (r *Reservation) Settle(actual Spend)

// Release returns unspent headroom. Idempotent, and a no-op after Settle.
func (r *Reservation) Release()

func (g *Guard) Remaining() Remaining
```

**Every abandonment path must release (Phase-1 finding, blocking).** The original plan released
only on deny and on `Record` — both success paths. A reservation leaks forever when the operation
errors after authorization, when `ctx` is cancelled between the two (Ctrl-C, which `DESIGN.md`
requires to be boring), or when a worker panics and a supervisor recovers. Each leak permanently
shrinks `Remaining()` and eventually produces **false** `ErrBudgetExceeded` denials. `Release` is
idempotent so `defer res.Release()` is always correct, and a test covers exactly the
authorize-then-abandon case the original test plan omitted.

**`ConfirmFunc` never runs under the lock, and is coalesced.** Unspecified before; both options
were bad. Holding the mutex across a human answering a `huh` prompt serializes every worker behind
one keypress. Releasing it reopens the TOCTOU the single lock exists to close. Resolution:

1. Detect the threshold crossing under the lock and mark a pending confirmation.
2. Release the lock; invoke `ConfirmFunc` exactly once via `singleflight`, so 128 goroutines
   crossing the threshold together produce **one** prompt rather than 128 racing for one terminal.
3. Re-acquire and **re-validate the estimate against current headroom** before authorizing —
   budget consumed during the confirmation window must not be double-spent.

**`Limits` is NOT a mirror of `knov1.Budget` (Phase-1 finding, blocking).** The original plan said
it mirrored `max_cost_usd_micros`, `max_llm_calls`, `max_tokens`. **`Budget` has no `max_tokens`
field** — verified against the merged schema. Its three token-shaped fields (`max_context_tokens`,
`max_training_examples`, `max_knowledge_base_bytes`) are Select-stage *portfolio* ceilings, not
cumulative run spend.

Resolution: the guard caps **dollars and calls only** — `MaxCostUSDMicros`, `MaxLLMCalls` — which
is exactly what `DESIGN.md`'s budget config names (`max_llm_calls`, `max_cost_usd`, `sample_rate`,
`trials`). Tokens are *accumulated and reported*, not capped. **No proto change is needed**, and
the plan no longer claims a mirror that does not exist.

**`Estimate` and `Operation` are deferred.** Both reviewers noted no adapter can produce a cost
estimate yet — there is no pricing table and no token-cost model before Ring 1 — and that the
original test plan tested `Authorize`/`Record` against hand-supplied values while never testing
`Estimate()` itself, which is the tell of speculative design. M0c-4 ships `Estimate` as a plain
value type (`Calls`, `CostUSDMicros`, `Tokens`) that callers construct; the *production* of
estimates from a pricing model lands with the first adapter, designed against a real provider.

**Money is `int64` micro-USD throughout.** No float arithmetic anywhere in the guard.

### 5. `internal/cmd/covercheck` and `internal/cmd/godoccheck`

`covercheck`: parses `coverage.out`, enforces per-package floors and the no-decrease ratchet against
`.coverage-baseline`.

Exemption rule per Phase-1 finding G6: a package is exempt **only if it contains no non-test `.go`
file with a function body**. Keying off "absent from the profile" would exempt the dangerous case —
real code, zero tests — permanently and silently. Exempt packages are listed in output.

`godoccheck`: every exported symbol in the checked packages has a doc comment.

### 6. Retiring three `PEND`s

M0c makes `coverage ratchet` and `godoc coverage` real. `OpenAPI generation` stays `PEND` — it
needs a proto *service*, which no milestone before the API defines.

## Alternatives considered

**A. Ship `core` interfaces without the conformance harness; add it with the first adapter.**
Rejected. The harness exists to stop adapters from disagreeing about the iterator contract, and the
first adapter is exactly when that drift starts. Writing it against zero adapters also keeps it
honest: it tests the contract, not one implementation's quirks.

**B. Put the budget guard in `core` rather than `stats/budget`.**
Rejected. `DESIGN.md` places trials, intervals, splits, and redundancy in `stats/`; spend is the
same kind of cross-cutting accounting, and `core` is the pipeline. Also keeps `core`'s public
surface to contracts alone.

**C. Defer `covercheck`/`godoccheck` to a later PR to keep M0c small.**
Rejected. They are the reason two gates print `PEND`, and every PR that lands before them
establishes a coverage baseline nobody checked. The floors must bind on the first PR that has code
worth measuring — which is this one.

**D. Hand-rolled goroutine-leak detection instead of `goleak`.**
Rejected. Comparing `runtime.NumGoroutine()` before and after is racy against runtime-internal
goroutines and produces exactly the flaky test the flaky policy forbids.

## Affected packages

`core/`, `core/errs/`, `coretest/` (new), `stats/budget/`, `internal/cmd/covercheck/`,
`internal/cmd/godoccheck/`, plus `Makefile` (retiring two `PEND`s) and `.coverage-baseline` (new).

## Proto / schema impact

**None.** M0c writes Go against the merged `kno.v1`. If it turns out a Ring-0 interface needs a type
the schema lacks, that is a finding: it means M0b was incomplete, and the proto change lands first
as its own PR per the proto-first rule.

## Edge cases and mitigations

| # | Edge case | Mitigation |
|---|---|---|
| 1 | Coverage floor "unreachable" for interface-only `core` | The original framing was circular. `core` consists of interface declarations and type aliases — **zero statements**, so it is automatically and permanently exempt under the body-less rule; the scenario the old text worried about cannot occur. The 85% floor genuinely binds on `core/errs` and `stats/budget`, and on nothing else this milestone. Stated plainly so nobody implementing `covercheck` inherits the confusion |
| 2 | `.coverage-baseline` conflicts on concurrent PRs | One sorted line per package; `make update-coverage-baseline` regenerates |
| 3 | A PR that legitimately deletes covered code | Floors compare per-package percentage, not statement counts |
| 4 | `goleak` false positives from test-framework goroutines | `goleak.IgnoreCurrent()` at harness start |
| 5 | Budget guard TOCTOU: two workers both pass `Authorize` | `Authorize` reserves against the cap under the same lock that `Record` settles; a `-race` test with 128 goroutines asserts the cap is never exceeded |
| 6 | `Authorize` deny leaks a reservation | Reservations are released on deny and on `Record`; a property test asserts reserved+spent never exceeds authorized |
| 7 | Import-boundary test blind to build tags or test-only imports | Walks with `go/packages` including tests, all tags (finding G10) |
| 8 | Exit-code constants drifting from `knov1.Actionable.exit_code` | Round-trip test asserts the Go constant and the proto field agree |
| 9 | Iterator harness passing a producer that never yields | Harness requires ≥2 items so break-after-1 is meaningful |
| 10 | `godoccheck` tripping on generated code | Skips `gen/` — the same exclusion `.golangci.yml` uses |

## Test plan

**Unit**
- `errs`: grammar rendering (golden), `errors.Is`/`As` through `%w`, proto round-trip, exit-code
  table, exit-code/proto agreement (#8).
- `budget`: per-dimension caps; exact micro-USD accumulation over 10,000 sub-cent charges; deny
  *at* the boundary; `-race` with 128 goroutines (#5); reservation accounting (#6); `ConfirmFunc`
  called once per threshold crossing, and never when auto-approved.
- `covercheck`: table-driven over synthetic profiles — floor pass/fail, ratchet pass/fail, exempt
  vs code-bearing-untested (#1), deletion case (#3).

**Structural**
- `core` import-boundary test (#7).
- `coretest.ConformIterator` self-test against deliberately broken producers: one that leaks on
  break, one that ignores cancellation, one that yields errors and continues. Each must be caught.
  *A harness that has never been seen to fail is not a harness* — this is debt 16's lesson applied
  locally.

## Rollback story

Additive. `git revert` the squash commit. The one lasting effect is `.coverage-baseline`: reverting
resets it, and the next PR re-establishes it. No schema, no persisted data, no published artifact.

## Docs impact

- godoc on every exported symbol (now gated by `godoccheck`).
- `CONTRIBUTING.md`: the iterator contract is a rule adapter authors must follow — it belongs
  alongside the non-negotiables, not only in godoc.
- `CHANGELOG.md` under `Unreleased`.
- `docs/debt.md`: **entry 11 is NOT retired** — see below. Entry 8's M1 trigger reaffirmed. New
  entries for the accepted risks below.

### Debt entry 11 stays open, and its trigger is corrected

The original plan claimed M0c retires it. Both reviewers called that false, and they are right.
Entry 11 is the unenforced `KNO_MAX_COST_USD` cap, currently backstopped by a grep interlock in
`make test-live`. A guard that *reads* the variable would satisfy the grep while the cap remains
unenforced, because **nothing calls `Authorize`** until an adapter routes spend through it — which
is M1.

Retiring it here would land exactly the false "protection exists" claim that Phase-3 finding F3
caught on the nightly workflow, in the ledger built to prevent that. The entry stays open with its
trigger corrected to name the real completion condition: *the CLI parses the cap into `Limits` AND
the first adapter's spend path calls `Authorize`*. The grep interlock stays until then — it is
crude, but it fails closed.

## Open questions

None blocking. The two `DESIGN.md` deviations this milestone realizes (`iter.Seq2`, pointer element
types) were confirmed by the user during M0 Phase 1.

## Phase-1 review record

Two independent adversarial passes, both **BLOCK**. Four findings sat inside the P0 component.

**Fixed in this amendment:**

| Finding | What was wrong | Fix |
|---|---|---|
| Guard correlation | `Authorize(…) error` / `Record(Spend)` carried no handle, so with N workers the guard could not tell which reservation a settlement closed. Since actual spend rarely equals the estimate, global arithmetic is wrong whenever settlement order differs from authorization order — and the plan's own "reserved+spent ≤ authorized" invariant was therefore unimplementable | `Authorize` returns a `*Reservation`; `Settle`/`Release` on it |
| Abandoned reservations | Release happened only on deny and on `Record` — both success paths. Error-after-authorize, ctx cancel, and recovered panics each leaked a reservation permanently, eventually causing **false** budget denials | Idempotent `Release`, `defer` idiom, and the missing authorize-then-abandon test |
| `Limits` mirrors `Budget` | **False.** `Budget` has no `max_tokens`; its token fields are Select-stage portfolio ceilings, not run spend. Verified against the merged schema | Guard caps dollars and calls only; tokens accumulated, not capped. No proto change needed |
| `errors.Is` across the wire | An `Actionable` rebuilt from proto is a different Go value than the sentinel, so `errors.Is(err, ErrBudgetExceeded)` returns false after any serialization boundary — silently misclassifying the two error kinds that matter most | `Actionable.Is` compares `Code`; explicit round-trip-then-`errors.Is` test |
| `ConfirmFunc` contract | Unspecified. Under the lock it serializes every worker behind one keypress; outside it reopens the TOCTOU the lock exists to close | Detect under lock → confirm outside via `singleflight` → re-validate under lock |
| Goal direction | `DESIGN.md` defines Goal as having direction and two proto comments rely on it; it exists nowhere. A `Report` consumer cannot interpret the sign of `holdout_gain` | `Direction` enum + `Report.goal_direction` (M0c-1, proto-first) + `Goal.Direction()` |
| Debt 11 retirement | Claimed falsely; a guard that reads the cap does not enforce it | Entry stays open, trigger corrected |
| `Evals` holdout shape | Inherited from the sketch without re-examination at the one milestone meant for it | Considered and explicitly rejected, with reasons and an M1 trigger |
| Scope | Tools import none of the other packages; bundling them put standalone work behind the guard's design questions | Split into four PRs |
| Edge case 1 | Circular: worried about a scenario the exemption rule makes impossible | Rewritten |

## Accepted risks

Mirrored to `docs/debt.md`, each with a trigger:

| # | Risk | Trigger |
|---|---|---|
| 1 | `covercheck` is build-tag-blind: an AST walk sees `//go:build windows` files that `coverage.out` never instrumented, so a package can fail the floor for code that was never compiled | Before `covercheck` is trusted on a second PR |
| 2 | `covercheck` has no `gen/` carve-out, unlike `godoccheck`. Generated `.pb.go` is full of function bodies and would be judged untested if ever pulled into the profile | Same PR as risk 1 |
| 3 | A package absent from `coverage.out` because it failed to build is indistinguishable from one with no data. It must not sail past enforcement | Same PR as risk 1 |
| 4 | `.coverage-baseline` is written by the same PR that introduces the tool that reads it — self-grading, on its first exercise, exactly debt 16's lesson | The PR must include a deliberately-broken-profile case proving the gate fails |
| 5 | `goleak` is a process-global census; `t.Parallel()` plus `-shuffle=on` means sibling subtests are live at check time, so `IgnoreCurrent` can both mask real leaks and flake | Before the first adapter runs `ConformIterator` |
| 6 | The harness claims to catch fd leaks but only checks goroutines. A producer that forgets `Close()` with no background goroutine passes undetected | Same as risk 5 |
| 7 | No test for a producer that panics in a background prefetch goroutine — which crashes the process rather than propagating to the consumer | Same as risk 5 |
| 8 | Nil-argument preconditions (`Invoke(ctx, nil)`) and the adapter metadata-attachment idiom are undocumented, so parallel adapter workstreams will each invent one | Before two adapters exist |
| 9 | Retry/rate-limit policy placement is undecided; only the `ErrRateLimited` sentinel lands | M1, with the first adapter |
| 10 | The import-boundary test cannot enumerate "all build tags", and the `go/packages` node it asserts against must be chosen carefully or `coretest → core` is misread as `core → coretest` | Before Ring-2 work adds new import surfaces |
