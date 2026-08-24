> **SUPERSEDED on 2026-08-23 by [2026-08-23-m2-10-the-event-spine.md](2026-08-23-m2-10-the-event-spine.md).**
>
> Kept as a decision record. Phase 1 blocked it. The findings that killed it:
>
> 1. **The `#42` repayment does not work.** Populating `resolved_models` fills only one operand.
>    `BaselineOptions.ResolvedModel` — the other — is set nowhere outside tests, and cannot be:
>    `checkResumable` runs before any provider call, and a resolved model is a property of a
>    response. The check stays dead, and the plan would have marked the entry repaid.
> 2. **10d creates a double-spend.** It settles the billed cost into the guard but not into
>    `sinkFunc`'s retry-exhausted branch, which persists `CostUSDMicros: 0`. `SettledSpend` is
>    "the only durable record of money spent" and is what `Guard.Restore` reads on resume — so
>    the resumed run gets headroom the first process already used.
> 3. **The event volume is a producer-side cost, not a consumer-side one.** `AppendEvent` is a
>    bare INSERT under `synchronous=FULL` — one fsync per event, on the same serialized writer
>    as the outcome row that prevents double-spend. Declining to bound the rate because "no
>    consumer reads it yet" answers the wrong question.
> 4. **`SettlementOvershoot` was specified twice, differently** — per-settlement in the test
>    plan, cap-gated in the edge cases.
> 5. **`observed_backends`** is the name ADR-0004 rejects by name, twice. Prime directive 2.
>
> Also: the four-PR split was cut at "emission vs money", but four of the six payloads are
> emitted from the spend path, so 10b was a spend-path PR labelled "no money".

---

# M2-10 — emitting what the engine already knows

Six ledger entries collapse into one theme: **the engine computes something and tells nobody.**
Six of the ten `Event.payload` variants have never been emitted, `Guard.Overshoot()` makes the
cap excess computable but nothing surfaces it, `CaseExecution` exists with no writer, and a
provider-reported charge on a failed call is observed by an adapter and discarded by `core`.

## Problem

| What | Computed where | Who learns |
|---|---|---|
| `StageProgress`, `SpendRecorded`, `RunResumed`, `RetryAttempted`, `RateLimitWaiting`, `SettlementOvershoot` | throughout | nobody — the payloads exist, nothing emits them |
| Resume progress | `openRun` | nobody: a resumed run re-emits `RunStarted` with the ORIGINAL total, so a live view jumps backward ([#29](../debt.md#29)) |
| Case counts with presence | `CaseExecution`, landed M2-0 | nobody: `closeRun` still writes the flat counters, `cli/jsonreport.go` still reads them ([#26](../debt.md#26)) |
| Resolved provider model | the adapters, since M2-7/M2-8 | nobody: `checkResumable` compares `case_execution.resolved_models` and **nothing writes it**, so the check is dead in production ([#42](../debt.md#42)) |
| Cap overshoot | `Guard.Overshoot()`, M2-2 | nobody ([#32](../debt.md#32)) |
| Reduced concurrency | `checkFeasible` | nobody: `--concurrency 32` silently becomes 5 ([#44](../debt.md#44)) |
| A provider's charge on a failed call | `openaicompat`, M2-7 | nobody: `invokeOnce` settles `Spend{Calls: 1}` and drops the cost ([#43](../debt.md#43)) |

`CLAUDE.md` names the event stream "the single spine": the engine emits typed events, the TUI
renders them, the API streams them, logs record them. Today the spine carries four of ten
messages, and two stages' worth of state travels in no channel at all.

## Scope: four PRs, not one

The natural instinct is one PR — it is one theme. Rejected, for the reason the `baseline.go`
split was done first: a money change buried in twenty event emissions does not get reviewed as
a money change. The split is by **what a reviewer must check**, not by what is convenient.

### M2-10a — the proto delta (first, and alone)

`CLAUDE.md`'s coordination rule is proto first: anything touching wire types lands with
`buf breaking` passing before dependent work begins.

Two additions, both additive:

- `ConcurrencyReduced` payload — `requested`, `effective`, `reason`
- `CaseExecution.requested_concurrency` and `.effective_concurrency`

The event answers "why is this slow" while the run is happening; the `Run` fields answer it
afterwards, from a record. [#44](../debt.md#44) asks for "either"; this proposes both, because
the two questions are asked at different times by different people, and the field costs one
line given the message already exists.

**No other proto change.** `CaseExecution` already carries the counts with message presence
(M2-0), so [#26](../debt.md#26) needs a writer and a reader, not a schema.

### M2-10b — emission (observability only, no money)

Emit the six unemitted payloads. Replace the resumed `RunStarted` with `RunResumed` carrying
already-completed, remaining, and total ([#29](../debt.md#29)). Emit `SettlementOvershoot`
([#32](../debt.md#32)) and `ConcurrencyReduced` ([#44](../debt.md#44)).

Reviewable as one question: **does the event stream now describe the run truthfully?**

### M2-10c — `CaseExecution` writer and reader

`closeRun` populates it; `cli/jsonreport.go` and `cli/render.go` read it; the flat counters stay
on the wire but stop being the source of truth ([#26](../debt.md#26)).

Populate `resolved_models` and `observed_backends` from what the adapters now report, which
makes `checkResumable`'s resolved-model check **live** for the first time
([#42](../debt.md#42)). That entry requires an end-to-end test driving the check through a real
run rather than a hand-populated `Run`, and that test is the point of this PR.

### M2-10d — the billed cost on the error path (money, own review)

`core.invokeOnce` settles every Invoke error as `Spend{Calls: 1}` with zero dollars. M2-7's
adapter reports the provider's own charge on the error; nothing reads it. Four lines, on the
same anonymous-interface shape `retryAfterOf` already uses:

```go
spend := budget.Spend{Calls: 1}
var b interface{ BilledCostUSDMicros() int64 }
if errors.As(invokeErr, &b) {
    spend.CostUSDMicros = b.BilledCostUSDMicros()
}
res.Settle(spend)
```

Half of [#43](../debt.md#43). The transport half (`ErrPartialResponse`) is a different package
and a different PR.

## Alternatives considered

**One PR.** Rejected above. The `baseline.go` split exists precisely so this diff reads as
behaviour; undoing that by bundling four concerns wastes it.

**Emit `ConcurrencyReduced` without the proto delta, as a log line.** Rejected: `CLAUDE.md` says
new user-visible state is a new event type, never a side channel, and a 6x slowdown the user did
not ask for is user-visible.

**Fix `checkFeasible` to take a `ctx` and emit inline.** Rejected: it runs *before* the event
stream is open, by design — the refusals it can produce must happen before a `Run` row exists,
or a refused run is left permanently `RUNNING`. Recording the decision on the options and
emitting after the stream opens preserves that ordering.

**Force the flat `Run` counters to `optional`.** Rejected, and this is the trap
[#26](../debt.md#26) records: it is a cardinality change `buf breaking` correctly refuses. The
submessage was the additive answer and it already exists.

**Populate `resolved_models` from the first response.** Rejected: with concurrency there is no
"first response", and during a provider rollout two workers legitimately see different builds.
The field is a set for that reason, and `ADR-0004` records it.

## Affected packages

`proto/` (10a only), `gen/` (regenerated), `core` (10b, 10c, 10d), `cli` (10c), `store` (10c —
reading back what `closeRun` writes), `docs` (all four).

## Edge cases

| Case | Behaviour |
|---|---|
| Resume of a resume | Each `RunResumed` carries the counts as of that process's start. Sequence numbers continue from `MaxEventSequence`, already handled |
| A run that executes no Cases | `CaseExecution` absent, not zeroed. That is the whole point of [#26](../debt.md#26) |
| Overshoot with no cap set | No `SettlementOvershoot`. `Guard.Overshoot()` returns 0 when there is no cap, and an event saying "exceeded nothing" is noise |
| Concurrency not reduced | No `ConcurrencyReduced` event, and the two `CaseExecution` fields are equal rather than absent — "we considered it and did not reduce" is a different fact from "no Cases ran" |
| A failed call with no reported charge | `BilledCostUSDMicros` not implemented, `errors.As` fails, settlement is unchanged. The absence of a charge is not a charge of zero |
| Two workers see different resolved models | Both land in the set. `checkResumable` refuses a resume whose model set changed |
| Events emitted after the store fails | Already handled — `closeRun` uses `context.WithoutCancel` |

## Test plan

- **10a**: `buf lint`, `buf breaking --against main`, `make typecheck-proto`. Generated code compiles.
- **10b**: for each payload, a test asserting it is emitted with the right fields, each verified
  failing against a suppressed emitter. A resumed run asserts `RunResumed` **and** the absence of
  a second `RunStarted`. `SettlementOvershoot` is driven by an agent that settles more than it
  estimated, not by calling `Overshoot()` directly.
- **10c**: `closeRun` writes `CaseExecution`; a stage-shaped run that executes no Cases leaves it
  absent; `cli` renders from it. **The [#42](../debt.md#42) test drives a real run through a fake
  agent reporting a changing `resolved_model` and asserts the resume is refused** — the entry's
  own requirement, and the reason that PR exists.
- **10d**: a fake agent whose error carries `BilledCostUSDMicros`, asserting the guard's `Spent()`
  reflects it; and one whose error does not, asserting settlement is unchanged. Verified against
  the current code, where the first fails.
- Every event test asserts **no Case or Response content** appears in any payload. `CLAUDE.md`
  forbids content in the event stream, and this is the PR that makes the stream carry enough to
  be worth checking.

## Rollback

10a is additive; nothing reads the new fields until 10b. 10b–10d are revertible independently —
that is what the split buys.

## Docs impact

- `docs/mental-model.md` — the event stream section, which currently describes payloads nothing sends
- `docs/what-the-numbers-mean.md` — what an overshoot means, and that a cap is a soft bound ([#32](../debt.md#32))
- CLI help and golden files (10c changes rendered output), vhs tape re-recorded
- CHANGELOG per PR

## Accepted risks

- **`ConcurrencyReduced` reports a decision, not a cause.** The `reason` is an enum, so a novel
  reason arrives as `UNSPECIFIED` until the enum grows. Preferred over free text, which the API
  would have to serialize and the TUI render.
- **Emitting six new payload types raises event volume.** No consumer yet reads the stream
  live, so this is unmeasured until the TUI lands. `StageProgress` is a heartbeat and is the one
  worth a rate bound; the plan does not set one, and that is deliberate — a number invented
  before there is a consumer is a number chosen against nothing.
