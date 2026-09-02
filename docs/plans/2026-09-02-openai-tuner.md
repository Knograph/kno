# An OpenAI Tuner — and the pricing assumption it breaks

**Status:** Phase 0 — plan. Not implemented.

## Problem

`kno bridge` shipped in v0.2.0 with exactly one Tuner. `newBridgeTuner`
(`cli/bridge_measure.go:249`) refuses every other scheme:

```go
if scheme != "together" {
    return nil, errs.ErrCapabilityUnsupported.
        WithFix("pass --tuner together:<model>; no other Tuner ships in this build")
}
```

So a shipped v0.2 stage is unusable without a Together account. `adapters/tuner/`
contains one directory. That is a single-vendor dependency at a Ring-0 seam, and
the `Tuner` interface has never been tested against a second implementation —
which is the only way to learn whether it is an interface or a description of
Together.

## The finding this plan exists for

**An OpenAI Tuner fires `docs/debt.md#159`, and repaying it is most of the work.**

That entry records that bridge asserts its scoring calls are free
(`core.ScoreParams.AcceptFreeCalls`, wired by `bridge.ScoreEvalRunner`), with the
trigger *"when a second Tuner adapter lands, or the first time a provider's
dedicated pricing carries a per-token line — whichever is first."* Both legs fire
here at once.

The assertion is true for Together and false for OpenAI, and the reason is
structural rather than a pricing detail:

| | Together | OpenAI |
|---|---|---|
| finished job | needs an explicit deploy | **auto-served**, callable immediately |
| serving cost | per minute per replica, idle included | **none** |
| inference cost | zero-marginal — capacity already paid for | **billed per token** |
| teardown | required, or it bills forever | nothing to tear down |

The bridge's cost model is the mirror image for each. The measurement seam plan
(`2026-09-01-bridge-eval-seam.md`, decision 4) concluded there is **no third
quote dimension** because inference on reserved capacity is free, and that the
free-call assertion is *"true rather than convenient, since the hosting ticker
meters the same dollars and charging per call would double-count."*

Every clause of that is Together-specific. Under OpenAI there is no ticker, no
hosting line, and the eval pass is the **only** inference cost in the run — so
asserting the calls are free would spend the user's money with no estimate, no
consent line, and no cap. Prime directive 4.

## Proposed design

### 1. `Deploy`/`Teardown` as the no-op the interface already sanctions

`core.Tuner.Deploy`'s doc already provides for this:

> A Tuner whose provider auto-serves a finished job (no separate deploy step)
> implements this as a no-op returning a **zero-rate Endpoint**, which is how the
> interface stays honest for a provider that is not Together.

So `Deploy` returns an `Endpoint` naming the fine-tuned model with a zero serve
rate, and `Teardown` is a no-op that cannot fail. The hosting ticker settles zero
per tick, which is correct: nothing is being billed by the minute.

**This is the interface's first real test.** It was written with this case in
mind but never exercised; the plan should treat a place where the no-op does not
fit as a finding about the interface, not a problem to route around.

### 2. Per-call cost becomes a Tuner-declared property

The seam plan hardcoded a Together truth. Replace it with something each adapter
answers:

```go
// ServedPrice reports what invoking the tuned model costs, so the engine can
// quote the eval pass and cap it. A provider that serves from reserved capacity
// already paid for by the hour returns a zero rate AND true for free, which is
// a claim; one that bills per token returns its rate.
ServedPrice(ctx context.Context, ep *Endpoint) (pricing.Price, bool, error)
```

An additive method on `core.Tuner` — breaking for out-of-tree implementers, which
0.2.0's migration notes establish as a pre-1.0 minor's prerogative. `bridge`
stops asserting `AcceptFreeCalls` unconditionally and asks.

### 3. The eval pass reaches the quote when it costs something

The consent quote is training + hosting today. Under a per-token Tuner it needs
the inference dimension the seam plan withdrew — for this adapter only, driven by
(2). The union pass over every group's Cases is the largest single scoring pass
in a run, so this is not a rounding error.

### 4. The adapter

`adapters/tuner/openai`, implementing all eight methods against OpenAI's
fine-tuning API: file upload, job submission, status polling to a terminal state,
`Model` from the finished job, the `Deploy`/`Teardown` no-ops, and
`ListJobs`/`ListEndpoints` for adopt-by-suffix. `ListEndpoints` has no natural
meaning where nothing is deployed — returning empty is honest and should be
documented as such, not silently.

Reuses `adapters/agent/openaicompat`'s transport where the package boundary
allows. **It may not:** `adapters/agent/internal/transport` is Go-`internal` to
`adapters/agent/`, which is exactly the wall the Together adapter hit and worked
around with a local security layer. Resolve at implementation time, and if a
second adapter needs the same workaround, that is evidence the package boundary
is wrong rather than that both adapters are unlucky.

## Alternatives considered

**A. Leave bridge single-vendor and close #161 as won't-fix.** Rejected: a shipped
stage that requires one vendor's account is a product problem, and the interface
learns nothing.

**B. Write a local/Ollama Tuner instead.** Cheaper to test — no spend at all — and
rejected as the *first* second adapter: it shares OpenAI's auto-serve shape but
not its per-token billing, so it would exercise the `Deploy` no-op without
exercising the pricing assumption. It confirms the easy half and leaves #159
open. Worth doing after.

**C. Add the scheme without the pricing work.** Rejected outright: it ships a path
that spends per token while asserting the calls are free.

## Edge cases

1. **A job that succeeds with a model that will not serve.** OpenAI can finish a
   job whose model is not yet invocable; `Deploy` must not report ready before it
   is, or the eval pass fails against a model that exists but does not answer.
2. **Adopt-by-suffix has no suffix.** OpenAI's job `suffix` is capped and shapes
   the model name. If it cannot carry what `ListJobs` matches on, the crash-
   recovery path needs another key — and if there is none, `ListJobs` must say so
   rather than silently matching nothing.
3. **Zero-rate endpoints and the hosting cap.** `--bridge-max-serve-minutes` tears
   an endpoint down after N minutes. Against a zero-rate no-op that timer protects
   nothing and could end a run early for no reason.
4. **`EstimateServeCap` with a zero rate** must produce a `$0.00` hosting line
   that reads as *"this provider does not bill for serving"*, not as an unpriced
   run — the distinction `core/ring0.go` already draws between zero and unknown.

## Test plan

- `coretest`'s conformance suite against the new adapter, which is what a second
  implementation is for.
- Recorded fixtures for the full job lifecycle including a failure, per
  `CLAUDE.md`'s determinism rule; live tests opt-in and nightly.
- A test asserting the quote **includes** the eval pass under a per-token Tuner
  and **omits** it under a zero-marginal one, since that is the behaviour #159
  exists about.
- The `Deploy` no-op returns a zero-rate Endpoint and `Teardown` cannot fail.

## Rollback

Additive: a new adapter plus one interface method. Reverting leaves Together
working, though the `core.Tuner` change touches out-of-tree implementers and so
belongs in a minor.

## Docs impact

`DESIGN.md` (the roadmap lists Tuner adapters at v0.4 — this pulls one forward),
`docs/what-the-numbers-mean.md` on what a bridge quote covers under each billing
model, `cli/bridge`'s help, `docs/status.json`, `CHANGELOG.md` with migration
notes for the interface change.

## Accepted risks

*To be filled by Phase 1 review, and mirrored to `docs/debt.md` with triggers.*

One to weigh there: this plan repays #159 by making the free-call assertion a
per-adapter answer, which is more surface than a constant. A reviewer should ask
whether a third adapter would need a third shape, and whether `ServedPrice`
returning both a rate and a boolean is one method doing two jobs.
