# ADR-0005: Kno cannot detect user-side conditioning on a baseline, and says so

- **Status:** accepted (won't fix)
- **Date:** 2026-08-26
- **Context:** [Value plan](../plans/2026-08-26-value-stage.md) §2.3, accepted risk R3, opened by Phase-1 pass two and sharpened by pass three.

## The problem

The Value stage measures an Asset by comparing a treatment arm against a control arm on a set of Cases. Which Cases get measured is a **selection**, and if that selection depends on the baseline's recorded outcome, the recorded outcome cannot also serve as the control — reusing the draw that *selected* a Case as that Case's control manufactures the effect being measured.

The arithmetic: routing to the Cases a baseline failed selects on `X_i = 0`. For an Asset with no real effect and per-Case success probability *p*,

```
E[δ | X = 0] = E[Y] − 0 = p
```

At `p = 0.7`, an Asset that does nothing measures **Δ = +0.70** with a tight interval. Regression to the mean, and pairing against a single recorded draw is the design that maximizes it.

Kno guards its own version of this. Routing may condition on the baseline outcome — DESIGN.md prescribes exactly that, and it is what makes the stage affordable — but when it does, the control arm is measured **fresh** on the routed Cases, and the control *slice* is drawn from a partition reserved at random **before** routing runs. Both are enforced in code and tested.

## What we cannot guard

The same bias arrives through the user, and nothing in the system can see it:

- A user reads the baseline failure report — which DESIGN.md actively teaches them to do — and then tags the Cases that failed. Tag-overlap routing is now conditioned on the outcome, through a channel that looks identical to honest labelling.
- A user assembles a pool by inspecting failures and writing Assets aimed at them.
- A user runs `kno value`, reads the results, adjusts, and runs it again. The second run's Assets are conditioned on the first run's draws.

In each case the Cases Kno measures were chosen using information from the baseline, and Kno sees only a tag string or an Asset file. There is no signal in the inputs that distinguishes "these Cases share a topic" from "these Cases are the ones that failed."

## Decision

**We do not attempt to detect it, and we say so on the epistemics page.**

`docs/what-the-numbers-mean.md` carries the limitation beside the other "Kno cannot detect this" entries, in the form a reader can act on: *a Δ measured over Cases you selected after reading a baseline is biased upward by approximately the baseline's pass rate on that slice.*

## Why not fix it

**Detection is not possible from the inputs.** Kno holds tags and Asset content. Correlating tag assignment with baseline failure would flag every honest user whose eval set is tagged by failure *mode* — which is what good tagging looks like — and would miss anyone who tagged before running. A test with that false-positive profile gets disabled.

**Prevention would break the workflow the product is for.** Forbidding a user to look at their own failures before choosing what data to try is the opposite of the loop DESIGN.md describes. "Read the gaps, go find data that fills them" is the value proposition.

**Statistical correction needs a quantity we do not have.** Correcting for selection requires knowing the selection rule. The user's rule is in their head.

**The honest instrument already exists**: the holdout. Validate measures the selected portfolio against Cases nothing in the loop has touched, and that is the number a user is allowed to act on. A Value-stage Δ inflated by user-side conditioning shows up there as a gain that does not replicate — which is what the holdout is for, and why `validate` is a separate stage rather than a flag.

## Consequences

- `what-the-numbers-mean.md` gains the limitation, and the Value cookbook recipe repeats it where a user is choosing tags.
- The winner's-curse property test in `stats/interval` covers Kno's *own* selection effects only. It cannot cover this one, and its documentation says so rather than implying broader coverage.
- If a user reports a Value gain that Validate does not reproduce, this ADR is the first hypothesis, ahead of a bug.
- Revisiting this requires a new capability — a record of when tags were assigned relative to a run, or a pre-registered selection rule. Neither exists, and neither is planned. If one arrives, this ADR is superseded rather than amended.
