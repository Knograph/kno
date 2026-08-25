# Attributing an orphaned charge, and naming a billed retry

Written after the fact, because Phase 3 was right that a schema change needs one. Repays
[`docs/debt.md#51`](../debt.md#51) and [`#52`](../debt.md#52); splits out [`#53`](../debt.md#53).

## Problem

Three defects in the same schema area, found by the Phase-3 reviews of M2-10d and the
orphan-spend PR.

**`SettlementOvershoot` cannot report its own contribution.** `Reservation.Settle` computes the
delta and M2-10d used it only to gate the emit. A consumer deriving it by subtracting `reserved`
from `settled` over-counts by whatever headroom was still under the cap — 450k where the
contribution is 300k — so summing across events inflates the total.

**A billed retry is reported as the reason meaning nothing happened.**
`RETRY_REASON_TRANSPORT_TRANSIENT` is defined as "no evidence the provider processed the
request", and an adapter wraps both a reset connection and a billed 5xx as
`ErrTransportTransient`.

**An orphaned charge has no attribution anywhere.** Money spent on a Case that produced no
outcome is recorded against the `Run`, and — despite four code comments, a plan, the CHANGELOG
and a user-facing page saying otherwise — no event named the Case. A column no event describes
is the side channel `CLAUDE.md`'s Observability section forbids.

## Design

All three additive, landed together rather than as three schema PRs, because they touch one area
and `buf breaking` runs on each PR regardless.

- `SettlementOvershoot.delta_usd_micros = 5`, carrying the figure `Settle` already returns.
- `retryReasonOf` uses the **charge** as a discriminator. No new enum value:
  `RETRY_REASON_PROVIDER_UNAVAILABLE` already means "the provider returned a 5xx" and was simply
  never emitted.
- `OrphanSpend` payload (field 21) + `OrphanReason`, emitted from `sinkFunc`'s two skip paths.

### Two ordering decisions

**Durable write, then emit.** The reverse would put a charge on the wire a resume cannot see.

**The emit is recorded, not returned.** The sink's return value latches the executor's
`sinkBroken`, after which every queued result is dropped — no outcome row, absent from
`CompletedCases`, re-paid on resume. A budget stop delivers in-flight Cases in a burst, so a
failed append on the first takes the rest. It can also be silent: the worker sends its result
*before* calling `fail(ErrBudgetExceeded)`, so the sink's error can lose that race and leave an
ordinary `BUDGET_STOPPED`. Every other hot-path emitter already records out of band; this one
was the exception until Phase 3.

### The reason is not always CANCELLED

A budget stop cancels the executor's context, so every in-flight worker's backoff returns
`ctx.Err()` and lands on the shutdown predicate. At the default concurrency that is seven Cases
labelled "a human interrupted this" for a run that ran out of money — the exact misreport
`OrphanReason` exists to prevent. `draining` is the discriminator and is already in the
expression.

## Alternatives considered

**Three separate PRs.** Rejected: one area, and #52's own entry says it should land with #51.

**A new enum value for a billed 5xx.** Rejected — `PROVIDER_UNAVAILABLE` already says it. This
is what the first draft of #51 got wrong.

**Emit `CaseErrored` for an orphaned Case instead of a new payload.** Rejected: `emit`'s own
comment explains that a budget refusal is not an outcome, and counting one would make the three
counts describe work that did not happen.

**Carry the attribution on `SpendRecorded`.** Rejected: it is cumulative and rides the progress
ticker, which is off by default — so on a default run the attribution would not exist at all.

## Edge cases

| Case | Behaviour |
|---|---|
| Budget stop, one Case refused | `BUDGET_EXCEEDED` |
| Budget stop, others in flight | `BUDGET_EXCEEDED` — `draining` is set |
| Ctrl-C during backoff | `CANCELLED` |
| Refused before the first call | Nothing settled, no record, no event |
| Unpriceable Case | Same — `estimate` refuses before `Authorize` |
| Orphan emit fails | Recorded out of band; the run ends reporting it, after the results are safe |
| Purged run | Events survive; `Purge` touches `outcomes` only |

## Test plan

- The delta is not the derivable figure, verified against reporting `settled - reserved`.
- A billed transient reports `PROVIDER_UNAVAILABLE`, an unbilled one `TRANSPORT_TRANSIENT`.
- Orphan events reconcile with the store's recorded total and count.
- A budget stop at the **default concurrency** reports no `CANCELLED`.
- A failed orphan emit leaves the guard and the store consistent.

## Accepted risks

- **The sink-break protection is structural, not demonstrated.** Reverting it leaves the test
  green, because orphan emits happen during the drain when little is queued behind them, and
  forcing results behind one means controlling the executor's delivery order. The test says so
  rather than implying coverage it lacks.
- **No `kno` command reads a past run's events.** The attribution is recorded and cannot yet be
  surfaced. `docs/cookbook/retention.md` says exactly that, after two earlier versions that
  overstated and understated it.
- **A billed 408 reports `PROVIDER_UNAVAILABLE`.** [`#53`](../debt.md#53) — `core` cannot
  separate a timeout from a server error, because the adapter hands it one sentinel for both.
