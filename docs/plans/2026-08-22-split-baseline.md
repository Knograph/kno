# Splitting core/baseline.go

Written because Phase 3 was right that it was missing. `CLAUDE.md` requires a plan for anything
over ~50 LOC, and a mechanical move of 1200 lines trips that threshold with no new logic at all.

## Problem

`core/baseline.go` was 1203 lines against a ~400-line soft cap. M2-10 adds seven behavioural
changes to it. Splitting afterwards would mean reviewing behaviour and relocation in one diff,
where the second hides the first.

## Design

Six files, grouped by **what a reviewer must check**, not by call graph:

| File | Holds | The question it answers |
|---|---|---|
| `baseline.go` | options, result, the run loop, `validate` | what is a Baseline run |
| `baseline_run.go` | `openRun`, `checkResumable`, and friends | where does a Run record come from |
| `baseline_budget.go` | estimate, feasibility, consent, spend arithmetic | what will this cost |
| `baseline_invoke.go` | `workFunc`, retry, `invokeOnce` | what happens to one Case, and what is authorized |
| `baseline_record.go` | `sinkFunc`, `codeOf`, the aggregator, `emit` | what is persisted and reported |
| `baseline_close.go` | `closeRun`, `statusFor`, `classifyRunErr` | how does a run end |

Two groupings are judgement calls and are the ones worth arguing with:

**`baseline_record.go` holds the sink and the emitter together** because they share an invariant
nothing else enforces. `sinkFunc` skips persisting on three predicates and `emit` skips on one;
they agree only because `sinkFunc` is `emit`'s sole caller and returns first in the other two.
Adding a fourth skip to one and not the other makes the outcomes table and the reported counts
disagree — the failure `workFunc`'s own comment records as having cost real money to find.
Separated across files, that pair is invisible.

**`baseline_budget.go` does NOT hold the spend paths.** `Guard.Authorize`, `Reservation.Settle`
and `Reservation.Release` are in `invokeOnce`, in `baseline_invoke.go`. The budget file plans
numbers; the invoke file spends them. This distinction is stated in both headers because the
first draft got it wrong in a way that mattered — see Accepted risks.

## Alternatives considered

**Split by call graph** (a file per entry point). Rejected: `estimate` is called by `workFunc`
and `checkFeasible` reads the same arithmetic, so the graph has no clean cut, and the result
would be files named after functions rather than after concerns.

**Move only the largest functions out and leave the rest.** Rejected: it produces a file that is
under the cap and about nothing in particular, which is worse than a long file with a coherent
subject.

**Don't split; do it as part of M2-10.** Rejected — the reason this plan exists.

**One file per ledger concern (a `baseline_money.go` holding everything prime directive 4
touches).** Rejected after Phase 3: the promise cannot be kept. `Authorize` and `Settle` live
inside the retry loop, and pulling them out to satisfy a filename would be a real restructuring
of the spend path to make a comment true.

## Affected packages

`core` only. No proto, no schema, no exported API change.

## Test plan

No new tests, and **no test file touched** — that is the evidence, not an omission. A refactor
that edits its own tests cannot claim the suite that passes is the same suite.

Three machine checks instead:

1. Every top-level declaration extracted with `go/ast`, moved, re-extracted. 42 before, 42 after.
2. Every declaration printed with comments stripped (`go/printer`, parsed without
   `ParseComments`), sorted, diffed against `main`. **901 lines each, identical** — so the code
   is unchanged and only prose and placement moved.
3. `go doc -all ./core` byte-identical; `core` coverage unchanged at 97.0%.

## Rollback

Revert. Nothing depends on the file layout.

## Docs impact

None user-facing. No CHANGELOG entry: nothing changed that a consumer of the binary can observe,
and `refactor:` produces no release-note line. Stated here rather than assumed, which is what
Phase 3 asked for.

## Accepted risks

- **`baseline.go` is 408 lines**, marginally over the soft cap. What remains is
  `BaselineOptions` with heavily documented fields plus the run loop. Moving a declaration purely
  to reach 400 is the gaming the rule warns against, so it is flagged rather than shaved.
- **The one-Case path spans three files** (`baseline_invoke.go` → `baseline_record.go` →
  `baseline_budget.go` for the arithmetic). Before the split it was one 1200-line file, which is
  not obviously better. Every header names where the rest is, which is the mitigation.
- **The first draft's headers were wrong**, and that is worth recording rather than quietly
  fixing. `baseline_spend.go` claimed "every path here can spend the user's money" while the
  only paths that can are in another file — a reviewer trusting it and auditing that file alone
  for prime-directive-4 compliance would never have read `Authorize`/`Settle`. Three of five
  headers were inaccurate. The prose was the only new content in the change, and it was the part
  that was wrong.
