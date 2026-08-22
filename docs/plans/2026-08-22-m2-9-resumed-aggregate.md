# M2-9 — A resumed run's aggregate spans the whole run

Repays [`docs/debt.md#27`](../debt.md#27). Written after the fact: the milestone plan
([2026-08-21-m2-first-provider-adapter.md](2026-08-21-m2-first-provider-adapter.md) §M2-9)
covers this line item, but prescribes summing `score_proto` — an approach M2-1 superseded
when the score moved to its own column. Phase 3 flagged the gap. Recording the real design
here rather than leaving the milestone plan describing something nobody built.

## Problem

`Run`'s case counts span the whole run — fixed in M1-5. `BaselineResult.AggregateScore`
did not: it was the mean over the Cases *the resuming process* scored. Counts and mean
described different populations.

Measured on a 40-Case run resumed at the halfway mark: reported `0.48`, true mean `0.5`.

## Design

`Store.ScoreSum` already existed (M2-1). It returns three values from one query:

```sql
SELECT COALESCE(SUM(score_value), 0),
       COALESCE(SUM(score_value IS NOT NULL), 0),
       COALESCE(SUM(scored = 1 AND score_value IS NULL), 0)
FROM outcomes WHERE run_id = ?
```

— the sum, how many rows contributed to it, and how many scored rows can no longer
contribute. `aggregator` seeds all three alongside the counts it already seeded from
`OutcomeCounts`.

**The denominator comes from `ScoreSum`, not `OutcomeCounts`.** `priorCounted` counts rows
holding a number; `priorScored` counts rows marked scored. They agree only while every
scored row has a score, which nothing in the schema enforces — there is no `CHECK`
constraint, only a runtime guard in `RecordOutcome`. Dividing a sum from one query by a
count from another reproduces this very defect one level down, so `mean()` uses
`scored + priorCounted` and `counts()` keeps `priorScored`.

### When the numbers are gone

`unrecoverable > 0` means a mean exists and cannot be computed. Two producers:

1. A run purged by a build older than `score_value`, when the score lived inside the blob a
   purge nulls.
2. A `score_proto` blob that failed to unmarshal during `backfillScoreValues`, which
   `continue`s past it — a corrupt row, or one from a build this one does not understand.

The run reports **no aggregate**, with an `IncompleteReason` naming both causes and the
affected count. Averaging the survivors would be a real number over a population nobody
chose, printed beside a count spanning the whole run — the same defect through a different
door.

## Alternatives considered

**Average the survivors and warn.** Rejected: the warning is a sentence the user can miss;
the number is the thing they act on. `CLAUDE.md` prime directive 5 makes statistical
honesty a feature, not a caveat.

**Re-read and unmarshal every prior `Score` proto.** The original M1-5 plan. Rejected:
M2-1's `score_value` column makes this a single aggregate query instead of streaming and
unmarshaling a million blobs, and the column survives a purge by design
([`docs/debt.md#25`](../debt.md#25)).

**Recompute by re-running the missing Cases.** Rejected: silently spending the user's money
to fill in a report is prime directive 4 backwards.

**Store a running aggregate on the `Run` row.** Rejected: two sources of truth for one
number, and the recovery story after a crash mid-update is worse than the query it saves.

## Interface impact

- `aggregator.seedCounts` gains `sum`, `counted`, `unrecoverable` — internal to `core`.
- `BaselineResult` gains `AggregateUnavailable bool` — **public Go API, additive.** Needed
  because `AggregateScore == nil` now has two meanings and a caller must be able to say
  which; without it `cli` reports "no cases scored" on a run that scored every Case.
- `--json` gains `"score_unavailable"` — additive, `omitempty`.
- No proto change. No migration.

## Edge cases

| Case | Behavior |
|---|---|
| Resumed twice or more | Each process seeds from the store, so the mean always spans everything recorded |
| Resuming process scores nothing | Mean is the prior mean; counts unchanged |
| `ScoreSum` fails | Run stops. Continuing would report a tail-only aggregate — the defect itself, via an error path |
| Score is NaN or ±Inf | `mean()` returns nil. NaN propagates through SQL `SUM`, so one bad Goal would otherwise poison every future resume of that run |
| Also over the error-rate threshold | Both reasons reported, joined. Overwriting lost the aggregate reason, which has no other signal |
| Was over threshold, resume recovers | Both `ErrorRateExceeded` and `IncompleteReason` are cleared before recomputing over the whole run |

## Test plan

Unit, all verified failing against the un-fixed code:

- `TestResumedRunReportsTheWholeRunsMean` — 0.48 vs 0.5 over 40 Cases
- `TestAPurgedRunReportsNoAggregateRatherThanAWrongOne`
- `TestMeanRefusesAValueNothingCanUse` — NaN, ±Inf, and `priorCounted` vs `priorScored`
- `TestBothIncompleteReasonsSurvive`
- `TestAResumedRunDoesNotInheritAStaleVerdict`
- `TestWarningsDistinguishTheTwoEmptyScores` (`cli`)

The purged fixture writes `score_value = NULL` through `database/sql` directly. No
production path produces that state — purging today preserves the number — which is why it
cannot be reached any other way.

## Rollback

Revert. No schema or proto change; `ScoreSum` predates this and stays.

## Docs impact

- `docs/what-the-numbers-mean.md` — new section, and the Baseline score row
- `docs/cookbook/retention.md` — its "one thing that will look wrong" section documented
  the old bug as expected behavior, and its console output is now the signature of the
  unrecoverable path
- godoc on `BaselineResult.AggregateScore` and `AggregateUnavailable`
- CHANGELOG

## Accepted risks

- **`unrecoverable` cannot distinguish a purge from a row written by an older binary.**
  [`docs/debt.md#31`](../debt.md#31), unchanged by this work. The reason string names both
  producers rather than asserting one.
- **`agg.add` fires even when `RecordOutcome`'s `INSERT OR IGNORE` dropped a duplicate
  row.** Now mixed with store-derived counts, so an in-memory overcount lands in the same
  number as a store-derived one. Ledgered — see `docs/debt.md#45`.
