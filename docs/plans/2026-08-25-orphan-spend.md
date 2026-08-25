# Spend with no Case to attribute it to

Repays [`docs/debt.md#50`](../debt.md#50). First of three PRs replacing the blocked
[M2-10e plan](2026-08-25-m2-10e-observations-and-orphan-spend.md); it lands alone and first,
because it is the only one that touches money.

## Problem

`sinkFunc` skips persisting a Case whose error is `ErrBudgetExceeded`, and skips again when a
shutdown cancelled it before it produced a Response. Both are right for a Case refused *before*
any call. Both are wrong for one whose attempts were made, charged, and settled.

Measured: **guard $0.50, store $0.00.**

M2-10d created the exposure. Before it, a failed attempt settled `Spend{Calls: 1}` at zero cost,
so the skip lost only a call count. `SettledSpend` is the only durable record of money spent and
`Guard.Restore` reads it, so the difference is headroom a resumed run spends again.

## Design: a column on `runs`

```sql
ALTER TABLE runs ADD COLUMN orphan_cost_usd_micros INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN orphan_calls           INTEGER NOT NULL DEFAULT 0;
```

`SettledSpend` returns `SUM(outcomes) + runs.orphan_*`. A new `RecordOrphanSpend(ctx, runID,
spend)` adds to the columns. Nothing else changes.

**A column, not a proto field on the `Run` blob.** `FinishRun` re-marshals the whole message, so
a field inside it would race a concurrent sink write.

### Why not a row in `outcomes`

The superseded plan's recommendation, and it is worse than the bug:

`RecordOutcome` is `INSERT OR IGNORE` — deliberately, so *"a resumed run's second attempt cannot
silently replace the first result"*. An orphan row therefore **permanently blocks** the real
outcome for that Case. The insert is ignored, `rowsAffected` is 0, the sink checks only `err`, and
every subsequent resume re-attempts the Case, pays again, and discards the answer. Unbounded
repeat spend, silent, produced by the fix for a leak.

An `ON CONFLICT DO UPDATE ... WHERE terminal = 0` upsert closes that, but the cost is a `terminal`
column plus an audit of every query that reads the table — and one of them is already wrong:
`OutcomeCounts` is `SUM(1 - scored)` with no predicate, so an orphan row counts as an **errored
Case**. That seeds `priorErrored` on resume, double-counts the Case when it is re-attempted, and
can stamp a healthy run `ErrorRateExceeded`. Trading a money bug for a statistical one is the
worse trade under prime directive 5.

Growing the key to `(run_id, case_id, attempt)` is disqualified outright: it double-counts every
retried Case in `OutcomeCounts` and `ScoreSum`, makes a second terminal row per Case
*representable* against `RecordOutcome`'s documented idempotency, changes what `Purge` reports to
the user, and turns `backfillScoreValues`' single-row `UPDATE` into a multi-row one writing one
attempt's score onto all of them.

### What the column costs

Per-Case attribution for orphan spend — and Phase 3 established this is **lost, not relocated**.
This plan asserted the event stream carried it. It does not: `sinkFunc` returns before reaching
`emit` on both skip paths, and `emit` skips a budget refusal in any case, so no event carries the
amount. `RetryAttempted` names the Case and has no cost field; `SettlementOvershoot` fires only
when a settlement crosses the cap.

A column no event describes is a side channel, which `CLAUDE.md`'s Observability section forbids.
Ledgered as [#52](../debt.md#52) rather than fixed here: the honest fix is a payload carrying the
amount, and that is a proto change in the one PR that touches money.

## Scope

- Migration `{to: 2}`, appended never edited; `schemaVersion` becomes 2.
- `Store.RecordOrphanSpend`; `SettledSpend` sums both sources.
- `sinkFunc` records orphan spend on **both** skip paths.
- `caseOutcome` carries settled **calls**, not `Attempts` (see below).
- Kill/resume test.

## Two things the review found in the code this touches

**`sinkFunc` has three skip predicates, not one.** The budget refusal is the obvious one; the
other is `shuttingDown && cancelled && noResult`, and `invokeWithRetry` returns `ctx.Err()` from
its backoff wait *after* `billed` has accumulated. So a Ctrl-C during backoff after a billed 429
drops the charge too — which [#50](../debt.md#50) and [#20](../debt.md#20) both say, and the
superseded plan named only `ErrBudgetExceeded`.

`errUnpriceable` is correctly skipped and must stay skipped: `estimate` refuses before
`Authorize`, so nothing was settled.

**Calls are over-counted by one.** `attempts++` runs at the top of the loop, before `invokeOnce`,
and a refused `Authorize` returns before settling. So a Case that made one real call and was
refused on attempt 2 would persist `Calls: 2` against a guard that settled 1 — and this PR's own
headline test asserts they agree. `caseOutcome` accumulates `settled.Calls` alongside `billed`,
and the write predicate becomes *the guard settled something for this Case*
(`settledCalls > 0 || billed > 0`), which also skips a Case refused before its first call rather
than fabricating one.

## Alternatives considered

**`terminal` column on `outcomes`.** Rejected above.
**Attempt ordinal in the key.** Rejected above.
**Leave it to M2-11.** Rejected: a live dollar leak this milestone introduced, and M2-11 is the
CLI PR, where a money fix would be reviewed as a flag change.

## Edge cases

| Case | Behaviour |
|---|---|
| Refused before any call | Nothing settled, nothing recorded. The predicate is "the guard settled something", not "the Case errored" |
| Refused after billed attempts | Orphan spend recorded; the Case is **not** in `CompletedCases`, so a resume re-attempts it and its earlier charge is already in `SettledSpend` |
| Ctrl-C during backoff, billed | Same |
| The same Case later completes | No collision — the orphan spend is on `runs`, the outcome is a fresh row |
| `kno purge` | Untouched. Orphan spend is columns on `runs`, never trace content, and no row is deleted — [#25](../debt.md#25) holds |
| A pre-migration database | Columns default to 0 |
| Downgrade after migrating | **A hard open failure.** `migrate` refuses a database from a newer build; that is correct and is the rollback story |

## Test plan

- **Kill mid-run with billed failures, resume, assert `Guard.Restore` sees what the first process
  settled.** CLAUDE.md's resume-from-checkpoint rule, and the test whose absence let this through:
  M2-10d compared guard against store *inside one process*, which cannot see a resume defect.
- A budget refusal after a billed attempt: the charge is durable **and** the Case is still
  re-attemptable.
- A Ctrl-C during backoff after a billed attempt, same two assertions.
- A Case refused before its first call records nothing.
- `Calls` agree between guard and store across a refused retry.
- `kno purge` over a run carrying orphan spend.
- Every one verified failing first.

## Rollback

The migration is additive with defaults, but **downgrade is impossible**: `migrate` refuses to
open a database whose `user_version` exceeds what the binary understands, and that refusal is the
user-facing contract. [#31](../debt.md#31)'s trigger — "when a second `kno` version is in
circulation" — gets closer with every migration; re-dated in this PR rather than left implied.

## Docs impact

- `docs/cookbook/retention.md` — what `kno purge` does and does not touch, now that spend can
  live outside `outcomes`.
- CHANGELOG, including that a downgrade past this migration is a refusal.

## Accepted risks

- **Orphan spend has no per-Case attribution in the store.** The event stream carries it. If a
  report ever needs "which Cases cost money without answering", it reads events, not outcomes.
- **`SettledSpend` now sums two sources.** One writer per source and one reader summing them;
  the failure this avoids is two writers for one fact, which is what
  [#26](../debt.md#26) is about.
