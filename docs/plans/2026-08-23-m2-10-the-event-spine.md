# M2-10 — the event spine

Supersedes [2026-08-22-m2-10-event-emission.md](2026-08-22-m2-10-event-emission.md), which
Phase 1 blocked. The theme is unchanged — **the engine computes things and tells nobody** — but
the decomposition, the `#42` repayment, and the money story are all different.

## Problem

Six of ten `Event.payload` variants have never been emitted. `Guard.Overshoot()` makes the cap
excess computable and nothing surfaces it. `CaseExecution` exists with no writer. A provider's
reported charge on a failed call is observed by an adapter and discarded by `core`.

`CLAUDE.md` calls the event stream "the single spine": the engine emits, the TUI renders, the API
streams, logs record. Today the spine carries four messages.

## The two decisions Phase 1 forced

### Where the resolved model comes from — and why `#42` is not repaid by writing a field

`checkResumable` compares `firstResolvedModel(run)` against `BaselineOptions.ResolvedModel`.
Writing `CaseExecution.resolved_models` fills the first operand. **The second is set nowhere
outside tests**, and structurally cannot be filled before the check runs: `checkResumable` is
called before `openRun`, before the executor, before any provider call, and a resolved model is a
property of a *response*.

So `now == ""`, `resolvedModelChanged` short-circuits to false, and the resume is accepted —
exactly what the entry says the check exists to prevent. A test that sets `opts.ResolvedModel` by
hand would recreate the hand-populated shape [#42](../debt.md#42) already calls dishonest.

**Decision: move the check to first-response time.** On a resumed run, compare the first
response's `resolved_model` against the recorded set and make a mismatch **run-fatal**. Cost: one
Case is paid for before the refusal. That is cheap against blending two models into one
`AggregateScore`, which is the harm.

Rejected: **an adapter resolution probe** called before `Baseline`. It is a Ring-0 interface
change plus a network call on every run, and a probe can answer differently from the call that
follows it, so it would be a check against a different fact.

`firstResolvedModel` also reads `models[0]` of a field the proto and ADR-0004 both call a **set**.
With `{A, B}` recorded and `B` now serving, the resume is refused or accepted depending on
`SELECT DISTINCT` ordering. Replaced with ordered aggregation plus set membership.

### The billed cost must reach the store, not just the guard

The superseded plan settled the provider's charge into `Guard` and left `sinkFunc`'s
retry-exhausted branch persisting `budget.Spend{Calls: n}` — cost zero. `SettledSpend`'s own
godoc: *"The guard is in-memory, so this is the only durable record of money spent. Without it a
resumed run starts at zero and can spend its cap a second time."* `Guard.Restore` reads it.

So that version would have made the guard and the store disagree, and the resumed run would spend
the difference again — the amnesia `sinkFunc`'s comment says it closed, reopened by the PR meant
to repay a money debt.

It is also not one line: `invokeWithRetry` settles **per attempt** and only `lastErr` survives, so
with `MaxAttempts: 3` the guard can settle three charges the sink could at best recover one of.

**Decision:** `caseOutcome` carries `BilledCostUSDMicros` accumulated across attempts inside
`invokeWithRetry`; `sinkFunc`'s exhausted branch persists it. The invariant is tested by killing a
run mid-flight with billed failures, resuming, and asserting `Guard.Spent()` after `Restore`
equals what the first process settled.

## Decomposition — five PRs, cut by which invariant a reviewer holds

The superseded plan cut at *emission vs money*. That seam was wrong: four of the six unemitted
payloads (`SpendRecorded`, `RetryAttempted`, `RateLimitWaiting`, `SettlementOvershoot`) are
emitted from `baseline_invoke.go`, whose own header says *"This is the spend path."* The
"observability only, no money" PR was a spend-path PR.

| PR | Contents | The question its reviewer answers |
|---|---|---|
| **10a — spine mechanics** | A single `append(ctx, agg, payload)` helper that allocates the sequence **last**; hoist `spent`; `RunResumed` replacing the resumed `RunStarted`; rewrite the two brittle event-count tests; add the gap-free-sequence test. No new payloads, no proto. | Is the sequence gap-free, and is `RunFinished` still last? |
| **10b — proto delta** | `ConcurrencyReduced`; scheduling fields on **`Run`**, not `CaseExecution`. | Is it additive, and do the names match what ADR-0004 settled? |
| **10c — off-hot-path emission** | `ConcurrencyReduced` (once, at open); `StageProgress` (ticker, stated interval, lifecycle). | Does the ticker stop before close, and is the rate bounded? |
| **10d — the spend path, whole** | `SpendRecorded`, `SettlementOvershoot`, `RetryAttempted`, **and** the billed-cost settlement with its `sinkFunc`/`caseOutcome` half and the clamp. | Do the guard, the store, and the stream agree about every dollar? |
| **10e — `CaseExecution`** | `Store` query method, `closeRun` writer, `cli` reader, `--json` nullable counts. | Can a reader tell "no Cases ran" from "zero scored", on every surface? |

Standalone proto-first was dropped for 10a: `CLAUDE.md`'s rule exists to unblock parallel
workstreams consuming generated types, and `api/` and `tui/` are each a single `doc.go`. There is
nothing to unblock, `buf breaking` runs on every PR anyway, and a reviewer of the emission must
read the proto diff regardless.

**`#42` is in none of them.** It gets its own PR once the first-response check above is designed
in detail. No PR may mark it repaid before then.

## Bounding the event rate — against a stated target, not against nothing

The superseded plan declined to bound `StageProgress`, arguing a number invented before there is a
consumer is chosen against nothing. That answers the wrong question: the cost is **producer-side
and paid today**.

`Store.AppendEvent` is a bare `ExecContext` INSERT — no transaction, no batching — and the DSN
pins `synchronous(FULL)`. One WAL fsync per event. The store's own comment justifying
`secure_delete` says *"this store's writes are dwarfed by the agent calls that produce them"*;
per-Case emission of `CaseScored` + `SpendRecorded` + `RetryAttempted` inverts that premise. Those
writes queue in the same single-writer lane as `RecordOutcome`, whose pragma comment warns that
losing that race drops *"an outcome whose money is already spent, which is exactly the double-spend
this store prevents."*

**Targets, stated so they can be argued with:** a live view is useful at ~1 Hz, and a 1M-Case run
must not add more than ~10% to durable writes.

- `StageProgress` — ticker at 1 s, not per Case.
- `SpendRecorded` — on the same ticker, carrying the cumulative totals the message already holds.
  All three of its fields are cumulative; it was shaped for a heartbeat, not a per-Case event.
- `SettlementOvershoot` — see below; bounded by concurrency, by construction.
- A benchmark on the sink/`AppendEvent` path lands with 10d so `make bench-diff` has a baseline.

## `SettlementOvershoot` — one trigger, stated once

The superseded plan specified it twice, differently. The rule:

**Emit at settle time, only when `Guard.Overshoot()` *increased* as a result of that settlement**,
carrying that Case's reservation, its settled figure, and the new cumulative.

Once the cap binds, `fitsLocked` refuses every further authorization, so only reservations already
in flight can overshoot: the event count is bounded by concurrency, not by Case count. That is the
`C + N × δ_max` bound [#32](../debt.md#32) already writes down, and the event enumerates its terms.

This needs `Reservation.Settle` to **return the overshoot delta** — it currently returns nothing,
so reading `Guard.Overshoot()` afterwards is a race in which two concurrent settles report the
same cumulative and a consumer double-counts. That is a `stats/budget` API change, listed below.

## `RateLimitWaiting` — half of it is deferred

The payload documents both the provider's signal and the **client-side limiter**. The client-side
wait happens inside `transport.Client.Do`, before the request; `Limiter.Wait` returns how long it
waited precisely so a caller can emit this, and no consumer passes it further than a latency
subtraction. `core` has no `Destination` and no host, and cannot learn either without a new
`Response` field — a proto change, and the timing would be wrong regardless: `Wait` blocks and
then returns, so an event emitted afterwards announces idleness only once it has ended.

**Decision:** 10d emits only the `Retry-After` half, from `invokeWithRetry`, **before** the wait.
The client-side half is ledgered with a trigger rather than half-built.

## Affected packages

`core` (10a, 10c, 10d, 10e), `proto`/`gen` (10b), `stats/budget` (10d — `Settle` returns the
overshoot delta), `store` (10e — a new interface method), `cli` (10c, 10e), `docs` (all).

The superseded plan described `store` as *"reading back what `closeRun` writes"*. Backwards:
`closeRun` must **read** the aggregates before it can write them, and ADR-0004 says they come from
SQL. 10e adds:

```go
// CaseObservations aggregates per-outcome facts for one Run in a single query.
CaseObservations(ctx context.Context, runID string) (Observations, error)
```

One method returning one struct, not five accessors — five reads at five instants in one message
is the defect `aggregator.snapshot` exists to avoid.

**ADR-0004's ingestion rule, restated here because a subagent gets this plan and not the ADR:**
`dev_case_count` and `holdout_case_count` come from `BaselineOptions`, **never** from SQL. They
describe what was loaded, not what executed. An implementer following "aggregate everything from
outcomes" reports a zero holdout count — the number that sets every interval's width.

## The `--json` contract

`cli/jsonreport.go` declares itself the stable CLI contract. `Attempted`/`Scored`/`Errored` are
non-pointer `int32`. Migrating the reader to `CaseExecution` while they stay non-pointer repays
nothing: a run with no `CaseExecution` still renders `"attempted": 0`, the hard zero
[#26](../debt.md#26) describes.

**Decision:** they become `*int32` and render `null`. That breaks somebody's `jq '.attempted > 0'`,
so it ships with a CHANGELOG entry under `Unreleased` carrying migration notes, per `CLAUDE.md`'s
pre-1.0 rule, and a test asserting `null` via `decodeRaw` for a run that executed no Cases.

`dev_cases`/`holdout_cases` keep coming from the loader's `SplitCounts`, not from the `Run` — the
provenance stays what it is today.

## Edge cases

| Case | Behaviour |
|---|---|
| Resume of a resume | Each `RunResumed` carries counts as of that process's start; sequence continues from `MaxEventSequence` |
| A run resumed three times | `RunStarted, …, RunResumed ×3, …, RunFinished` on one gap-free sequence. Only the last process emits `RunFinished`; a replaying consumer must already tolerate that |
| Sequence allocated then not written | Forbidden by construction: the `append` helper allocates last. `emit` has this latent today and is fixed in 10a |
| `AppendEvent` fails on a non-outcome event | Returned, not swallowed. A stream with a silent gap is worse than a run that stops, and `Event.sequence`'s whole purpose is that a consumer can tell |
| Emitter context during shutdown | `context.WithoutCancel`, like `closeRun`. A budget stop and Ctrl-C are when the final `SpendRecorded` and `SettlementOvershoot` matter most |
| Ticker vs `RunFinished` | The ticker is stopped and joined before `closeRun`. `RunFinished` is documented "always the last event"; a test asserts it holds `MaxEventSequence` |
| `BilledCostUSDMicros` negative or absurd | Clamped at the call site: `> 0` and within a ceiling. Unclamped it reaches `Settle`, which adds unchecked — [#48](../debt.md#48) |
| Overshoot with no cap | None. `Guard.Overshoot()` returns 0 without a cap, and an event saying "exceeded nothing" is noise |
| Concurrency reduced, then the user declines | The reduction is never reported. `checkFeasible` runs before `confirmRun`, so the decision exists before the stream does |
| First run, empty `resolved_models` | No comparison. `recorded == ""` short-circuits, which is correct — there is nothing to disagree with |
| Purged run closed | `resolved_model` survives a purge (only `response_proto`/`score_proto` are nulled), so aggregation is unaffected |
| Two workers, different resolved models | Both in the set, ordered. The check is set membership, not `models[0]` |

## Test plan

Unchanged in discipline — every test verified failing against the un-emitted or un-fixed code
first — with these additions Phase 1 named:

- Sequence is `1..N` with **no gaps**, across a resume and across a budget stop.
- `RunFinished` holds `MaxEventSequence` with the ticker running.
- **Kill mid-run with billed failures, resume, assert `Guard.Restore` sees what the first process
  settled.** This is `CLAUDE.md`'s resume-from-checkpoint requirement ("assert no double-spend").
- No Case or Response content in any payload, on every payload.
- The two existing tests this breaks are rewritten, not re-numbered: `core/baseline_test.go:443`
  asserts `maxSeq == 17` exactly, which a ticker makes nondeterministic — it becomes a decode-and-
  count-by-payload-type assertion. `cli/cli_test.go:605`'s `events - 4` hardcodes two processes ×
  (`RunStarted` + `RunFinished`); the `RunResumed` swap makes it 5.
- A sink/`AppendEvent` benchmark, so `make bench-diff` gates the write-path regression.

## Rollback

Each PR reverts independently — that is what the split buys. Once 10e changes the `--json` shape,
reverting is itself a contract change and needs its own CHANGELOG note; that asymmetry is the
reason 10e is last.

## Docs impact

- `docs/mental-model.md` — **a new event-stream section.** The superseded plan claimed it would
  amend a stale one; there is no such section, which is a materially different amount of work.
- `docs/mental-model.md:112` — *"Resuming is refused if the evals, the goal, or the agent
  changed."* The `#42` work changes that sentence, and it is the user-facing statement of the
  resume contract.
- Event **retention**: `kno purge` touches `outcomes` only. Events carry IDs and metrics, never
  content, and are retained — stated plainly in the docs, per `SECURITY.md`.
- `docs/what-the-numbers-mean.md` — what an overshoot means, and that a cap is a soft bound.
- CHANGELOG per PR; migration notes on 10e.

## Accepted risks — both ledgered, per `CLAUDE.md`

- **Event retention is unbounded.** Events are never purged and there is no cap on the table.
  Ledger entry with trigger *before the first tagged release*.
- **The 1 Hz ticker is a chosen number, not a measured one.** It is chosen against a stated target
  (~1 Hz useful, ≤10% durable-write overhead at 1M Cases) rather than against nothing, and the
  benchmark lands with it so the next person can argue with data. Ledger entry with trigger *when
  the TUI renders the stream (M2-11)*.
- **`RateLimitWaiting`'s client-side half is not emitted.** Ledger entry, trigger *the next
  `internal/transport` PR*, which is already carrying `ErrPartialResponse` for
  [#43](../debt.md#43).
- **[#48](../debt.md#48) gains a third path into `Settle`** — the error object rather than the
  response. The adapters' 10M-token refusal does not cover it. Disposed of in 10d rather than left
  to its "third adapter" trigger, which a new caller does not fire.
- **[#20](../debt.md#20)'s dark-spend window widens** from one cancelled attempt to every billed
  failure, until 10d's sink half lands. Noted on the entry.
