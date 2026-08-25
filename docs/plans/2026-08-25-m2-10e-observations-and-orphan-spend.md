> **SUPERSEDED on 2026-08-25** by [2026-08-25-orphan-spend.md](2026-08-25-orphan-spend.md) and
> [2026-08-25-case-observations.md](2026-08-25-case-observations.md).
>
> Kept as a decision record. Phase 1 blocked it, and its recommendation was wrong in a way that
> would have made the leak it was fixing worse.
>
> It recommended a `terminal` column on `outcomes` and deferred the primary-key collision as "the
> sharpest edge". `RecordOutcome` uses `INSERT OR IGNORE`, so an orphan row would **permanently
> block** the real outcome for that Case: every resume re-attempts it, pays again, and has the
> result discarded — silently, because the sink checks `err` and never `RowsAffected`.
>
> Four more, all verified against the code:
>
> - `OutcomeCounts` is `SUM(1 - scored)` with **no predicate**, so every orphan row counts as an
>   errored Case — seeding `priorErrored` on resume, double-counting the Case when it is
>   re-attempted, and able to falsely stamp a healthy run `ErrorRateExceeded`. One query was
>   audited; five read the table.
> - The rollback claim was false. `migrate` **refuses to open** a database from a newer build, so
>   downgrade is a hard open failure.
> - `CaseExecution` presence was made a query result. It is a **stage** property, and derived
>   from a query it reports "this stage executes no Cases" for a run where every Case was refused
>   after being billed — the inverted ambiguity the proto forbids.
> - `attempts++` runs before `invokeOnce`, so a Case refused on attempt 2 would persist
>   `Calls: 2` against a guard that settled 1. The plan's own headline test would have failed.
>
> The rejection of Option C rested on a false statement — "cannot survive the run being resumed
> by a process that reads only outcomes"; no such process exists. C is now the design.

---

# M2-10e — CaseExecution, the `--json` contract, and spend with no Case to attribute it to

Last of the five M2-10 PRs, and the largest. It carries a money fix that M2-10d created and
could not close in scope.

## Problem

Three things, and the third is why this plan needs a review rather than a sentence.

**`CaseExecution` has no writer.** It landed in M2-0 with message presence so a stage that
executes no Cases can be told from one that attempted nothing. `closeRun` still writes the flat
counters and `cli/jsonreport.go` still reads them, so [#26](../debt.md#26) is half repaid.

**The `--json` counts cannot express absence.** `Attempted`/`Scored`/`Errored` are non-pointer
`int32`. Migrating the reader while they stay that way repays nothing: a run with no
`CaseExecution` still renders `"attempted": 0`, which is the hard zero the entry describes.

**A budget refusal drops every charge the earlier attempts incurred.** [#50](../debt.md#50).
`sinkFunc` skips persisting a Case whose error is `ErrBudgetExceeded`, which is right for one
refused before any call and wrong for one whose attempts were made, charged, and settled.
Measured: guard $0.50, store $0.00. M2-10d created that exposure — before it, a failed attempt
settled at zero cost, so the skip lost only a call count.

## The decision this plan exists to make

`CompletedCases` is `SELECT case_id FROM outcomes WHERE run_id = ?` — **every** row. `SettledSpend`
sums the spend columns of the same rows. So the outcomes table means two things at once: *this
Case is done* and *this is what it cost*. Recording orphan spend as an ordinary row would mark a
Case done that never got an answer, and a resume would skip it — trading a money bug for a
statistical one, which is the worse trade under prime directive 5.

### Option A — a `terminal` column on `outcomes` (recommended)

```sql
ALTER TABLE outcomes ADD COLUMN terminal INTEGER NOT NULL DEFAULT 1;
```

`CompletedCases` gains `WHERE terminal = 1`. `SettledSpend` is unchanged and sums everything.

What it buys: one table, one additive migration, and the table's meaning becomes **explicit**.
Today "a row means the Case is done" is an invariant nothing states; after this it is a column.

What it costs: every insert path must set it, and `kno purge` must keep working —
[#25](../debt.md#25) records that purge and the done-marker already share a row, so a second row
type is a second thing purge must not break. The migration must also be visible to a reader of
the schema, which `PRAGMA user_version` already handles.

### Option B — a separate `orphan_spend` table

`SettledSpend` sums both tables. `CompletedCases` is untouched, so resumability cannot regress
by accident.

What it costs: two sources of truth for one number, a second table for `purge` to consider, and
a join or a second query on the hot resume path. The isolation is real, but the number that
matters most in the system now comes from two places.

### Option C — attribute orphan spend to the `Run` rather than to a Case

A column on `runs`. Simplest to write, and wrong: the spend is per-attempt and per-Case, and
losing that attribution means a later report cannot say which Cases cost money without
answering. It also cannot survive the run being resumed by a process that reads only outcomes.

**Recommendation: A.** The invariant it makes explicit is one the codebase already depends on
implicitly, and B's second source of truth for settled spend is the kind of split that produced
[#50](../debt.md#50) in the first place — the guard and the store disagreeing because two things
computed the same number.

## Scope

| Item | Repays |
|---|---|
| `Store.CaseObservations` — one query, one struct | [#26](../debt.md#26) |
| `closeRun` writes `CaseExecution`; `cli` reads it | [#26](../debt.md#26) |
| `--json` counts become `*int32`, rendering `null` | [#26](../debt.md#26) |
| `terminal` column; orphan spend persisted | [#50](../debt.md#50) |
| Kill/resume test asserting no double-spend | CLAUDE.md's resume-from-checkpoint rule |

## The `Store` interface change

```go
// CaseObservations aggregates per-outcome facts for one Run in a single query.
CaseObservations(ctx context.Context, runID string) (Observations, error)
```

One method returning one struct, not five accessors: five reads at five instants in one message
is the defect `aggregator.snapshot` exists to avoid.

**ADR-0004's ingestion rule, restated here because a subagent gets this plan and not the ADR:**
`dev_case_count` and `holdout_case_count` come from `BaselineOptions`, **never** from SQL. They
describe what was loaded, not what executed. An implementer following "aggregate everything from
outcomes" reports a zero holdout count — the number that sets every interval's width.

## The `--json` break

The counts become `*int32` and render `null` when `CaseExecution` is absent. That breaks
somebody's `jq '.attempted > 0'`, so it ships with a CHANGELOG entry under `Unreleased` carrying
migration notes, per CLAUDE.md's pre-1.0 rule, and a test asserting `null` via `decodeRaw` for a
run that executed no Cases.

`dev_cases`/`holdout_cases` keep coming from the loader's `SplitCounts`, not from the `Run` —
the provenance stays what it is today.

## Alternatives considered

**Split 10e in two.** Tempting, and rejected: the orphan-spend fix needs the `terminal` column,
`CaseObservations` needs the same migration slot, and two migrations in two PRs is worse than
one. If review disagrees, the money half goes first — it is the one with a measured exposure.

**Leave `#50` to M2-11.** Rejected. It is a live dollar leak that this milestone introduced, and
M2-11 is the CLI PR — a money fix landing there would be reviewed as a flag change.

**Keep the flat counters as the source of truth and add `CaseExecution` alongside.** Rejected:
two writers for one fact is what [#26](../debt.md#26) is about.

## Edge cases

| Case | Behaviour |
|---|---|
| A run with no Cases at all | `CaseExecution` absent; `--json` counts `null` |
| Orphan spend, then resume | The Case is NOT in `CompletedCases`, so it is re-attempted; its earlier charge is already in `SettledSpend` and is not spent twice |
| Orphan spend, then the same Case completes | Two rows for one Case — the primary key is `(run_id, case_id)`, so this needs a distinct key or an UPDATE. **This is the sharpest edge and the plan's main risk** |
| `kno purge` over a non-terminal row | Must null its blobs like any other and must not delete it |
| A pre-migration database | `terminal` defaults to 1, so every existing row stays a done-marker |
| Concurrent orphan writes | Same single-writer lane as every other outcome write |

The two-rows-for-one-Case problem is the reason Option A needs the review rather than just the
implementation: `(run_id, case_id)` is the primary key today, and an orphan row followed by a
real outcome for the same Case collides. Either the orphan row is UPDATEd into the real one — in
which case its spend must be added, not replaced — or the key grows an attempt ordinal, which
changes what `CompletedCases` returns.

## Test plan

- Every check verified failing first.
- **Kill mid-run with billed failures, resume, assert `Guard.Restore` sees what the first
  process settled.** The test CLAUDE.md's resume-from-checkpoint rule requires, and the one whose
  absence let [#50](../debt.md#50) through — M2-10d compared guard against store inside one
  process, which is exactly the proxy that cannot see a resume defect.
- A budget refusal after a billed attempt: assert the charge is durable and the Case is still
  re-attemptable.
- The orphan-then-complete collision, asserting the spend is summed rather than replaced.
- `--json` renders `null` for a run with no `CaseExecution`, via `decodeRaw`.
- A stage-shaped run that executes no Cases leaves `CaseExecution` absent.
- `kno purge` over a database containing a non-terminal row.

## Rollback

The migration is additive with a default, so an older binary reading a migrated database sees
every row as it did before. The `--json` change is not reversible without a second contract
note, which is why it is last in the PR and called out in the CHANGELOG.

## Docs impact

- `docs/what-the-numbers-mean.md` — what an overshoot means and that a cap is a **soft** bound.
  M2-10d surfaced the event and did not write this; CLAUDE.md says a PR that changes what a
  number means changes that page in the same PR, and "your cap is not a ceiling" qualifies.
- `docs/mental-model.md` — the event-stream section the plan has assigned to M2-10 since 10a and
  which no PR has yet written.
- `docs/cookbook/retention.md` — purge over a non-terminal row.
- CHANGELOG with migration notes for the `--json` break.

## Accepted risks

- **The `terminal` column adds a second row type to a table `kno purge` already treats as one
  thing.** [#25](../debt.md#25) records that purge and the done-marker share a row; this makes
  that sharing more complex, and the purge test must cover it.
- **`CaseObservations` is a new public interface method** obliging every future `Store`. Its
  contract needs stating: what it returns for a run with zero outcomes, whether it excludes
  purged rows, and whether `DISTINCT` results are ordered — the last one because
  `firstResolvedModel` reads `models[0]` and M2-10 left that ordering unspecified.
