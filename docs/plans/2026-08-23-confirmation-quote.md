# What the confirmation prompt quotes

Written because Phase 3 was right that it was needed. The change looked like one expression, so
it shipped without a plan; the review found it altered **when consent is required**, which is the
contract prime directive 4 protects.

## Problem

`confirmRun` bounded its quoted total against `Guard.Limits().MaxCostUSDMicros` — the static cap.
`Baseline` calls `Guard.Restore` before it, so on a resume that is the cap the run *started* with,
not what it has left.

Measured: a $5.00 cap with $4.90 spent quotes **$5.00** for a run the guard stops at $0.10. The
CLI prints both numbers in one sentence — *"would spend about $5.00 ($0.10 remaining)"* — so the
contradiction reaches the user intact.

A second instance, one field over: `--max-calls` was never applied to the dollar figure at all.
200 Cases against `--max-calls 10` quotes $10.00 for $0.50 of permitted spend. Same class, same
direction, 20x.

## Design

**The bound moves into `Guard.PreConfirm`.** The caller passes its *intent* — every remaining Case
at the planning rate — and the guard bounds it to what it will authorize.

The caller cannot do this correctly. It needs both the static limits and the live headroom, and
reading them itself takes a second snapshot, so the figure quoted and the "remaining" shown beside
it can come from two instants. They appear in one sentence, so they must come from one read.
`PreConfirm` already holds the lock and already computes `rem`.

`permitted(est, limits, rem)` bounds both dimensions:

- **Calls first, then cost.** Bounding the call count reduces the cost; bounding the cost does not
  reduce the call count.
- **Gated on the LIMIT existing, not on the headroom being positive.** `Remaining` reports zero
  both for "no cap" and for "cap exhausted", and those need opposite handling — skipping the bound
  is right for the first and restores the full unbounded figure for the second.
- **`min(headroom, cap)`.** `Restore` is additive and unvalidated, so a negative settled spend read
  back from the store would make `Remaining` exceed the cap, and the quote would sit above a limit
  the guard still enforces.

## The consent change, stated because it is one

The threshold is compared against the **bounded** figure. So a run that can only spend $0.10 no
longer asks about a `--confirm-threshold` of $1.00: it proceeds and spends the $0.10.

Previously it quoted the whole $5.00 cap, crossed the threshold, and — since the current
`confirmFunc` is non-interactive and always declines — refused. That refusal was an accident of a
wrong number, not consent. The threshold means *"ask before spending more than this"*; a run that
cannot spend more than it should not ask, and the cap still binds regardless.

This is the part the first version of this change made silently, which is why the plan exists.

## Alternatives considered

**Bound against `Remaining()` in `confirmRun`.** The first fix. Trades one wrong number for
another: correct on a resume, and on an exhausted cap it removes the bound entirely, because the
`ceiling > 0` test cannot tell "no cap" from "cap spent". Latent only because `checkFeasible`
refuses one line earlier — so the safety of a consent path would depend on the ordering of two
calls in another file, with no test pinning it.

**Bound against `min(Limits(), Remaining())` in `confirmRun`.** Correct, but leaves the two-snapshot
problem and duplicates the guard's own knowledge in a caller. Every future caller — the API's
estimate-only path — would have to repeat it.

**Compare the threshold against the unbounded intent.** Preserves today's prompting behaviour
exactly. Rejected: it means asking about money the run cannot spend, which is the same false
number in a different place, and it would make the prompt fire more often the *more* exhausted a
cap is.

## Affected packages

`stats/budget` (the bound and its gate), `core` (drops its own bound). No proto, no schema. No
change to `ConfirmFunc`'s signature, so no caller breaks.

## Edge cases

| Case | Behaviour |
|---|---|
| No cap at all | Unbounded. `Remaining{Unlimited: true}` carries zeros that mean "meaningless", and the limit gate skips them |
| Cost cap, nothing spent | Bounded to the cap — unchanged from before |
| Cost cap, partly spent | Bounded to the headroom. This is the fix |
| Cost cap exhausted | Bounded to zero, so nothing is quoted and nothing is asked. Unreachable today (`checkFeasible` refuses first) and correct anyway |
| Call cap only | Dollar figure bounded via the implied per-call rate |
| Both caps | Calls first, then cost; the tighter one wins |
| Headroom above the cap | Bounded to the cap. Only reachable through an unvalidated `Restore` |
| Reservations outstanding | `Remaining` subtracts them, so the quote is understated. Unreachable in `core` — `confirmRun` runs before any `Authorize` — but `Guard` is caller-supplied, and a caller sharing one across concurrent runs would see it |

## Test plan

Every case above, as a table on `permitted`, in an internal test — because the case that matters
most is **unreachable from `core`** and testing it through a run would prove nothing.

Through a real run: the resume quote is asserted as an exact `100_000`, not an inequality, since
both sides of an inequality derive from the same `Remaining()` call and would move together. The
fixture's numbers are load-bearing and say so: 98 calls of a 100-call cap leaves exactly $0.10.

The consent change gets its own test at the real $1.00 threshold, asserting the run proceeds
unprompted **and** that the cap still binds.

## Rollback

Revert. No persisted state, no schema, no signature change.

## Docs impact

CHANGELOG, including the consent change in the same entry as the display fix — a user reading only
"the quoted figure was wrong" would not learn that some resumes now skip the prompt.

## Accepted risks

- **A caller sharing one `Guard` across concurrent runs gets an understated quote**, because
  `Remaining` subtracts outstanding reservations. Not reachable from `core` or the CLI today; the
  shape an API server would take. Not ledgered separately — `Guard`'s own contract is per-run, and
  the API stage will need its own budget story regardless.
