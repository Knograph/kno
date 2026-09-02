# An OpenAI Tuner — and the pricing assumption it breaks

**Status:** Phase 0 — plan, **rewritten after Phase 1 review**. Not implemented.

## Problem

`kno bridge` shipped in v0.2.0 with exactly one Tuner. `newBridgeTuner`
(`cli/bridge_measure.go`) refuses every other scheme:

```go
if scheme != "together" {
    return nil, errs.ErrCapabilityUnsupported.
        WithFix("pass --tuner together:<model>; no other Tuner ships in this build")
}
```

A shipped v0.2 stage is unusable without a Together account, and `core.Tuner`
has never met a second implementation — the only way to learn whether it is an
interface or a description of Together.

## The finding this plan exists for

**An OpenAI Tuner fires `docs/debt.md#159`**, whose trigger reads *"when a second
Tuner adapter lands, or the first time a provider's dedicated pricing carries a
per-token line — whichever is first."* Both legs fire at once.

Bridge asserts its scoring calls are free — `bridge/score.go`'s hardcoded
`AcceptFreeCalls: true`. The two providers are mirror images:

| | Together | OpenAI |
|---|---|---|
| finished job | needs explicit deploy | **auto-served** |
| serving cost | per minute per replica, idle included | **none** |
| inference | zero-marginal — capacity already paid for | **per token** |
| teardown | required, or it bills forever | nothing to tear down |

The seam plan concluded there is no third quote dimension because inference on
reserved capacity is free, and that the assertion is *"true rather than
convenient, since the hosting ticker meters the same dollars."* Every clause is
Together-specific. Under OpenAI there is no ticker, no hosting line, and the eval
pass is the **only** inference cost — so asserting it is free spends money with
no estimate, no consent line and no cap. Prime directive 4.

## What the first draft got wrong

Phase 1 review rejected it. Recorded here because the correction is the design.

The draft added `ServedPrice(ctx, ep) (pricing.Price, bool, error)` to
`core.Tuner`. That was wrong three ways:

- **It named a type that does not exist.** `adapters/agent/pricing` exports
  `knov1.Price`, `TrainPrice` and `ServePrice`; there is no `pricing.Price`. It
  would not have compiled.
- **It reinvented working machinery.** `openaicompat.Agent` **already implements
  `core.Estimator`** when built with a non-nil `Price *knov1.Price`
  (`adapters/agent/openaicompat/estimate.go`). `core.ScorePass`'s `estimate()`
  already does per-Case pessimistic pricing, guard authorization, and the
  zero-versus-unknown distinction — the same path Baseline, Value and Validate
  use.
- **It broke a Ring-0 interface for nothing**, one release after 0.2.0 already
  did, when the existing non-breaking path was sitting there.

## Proposed design

### 1. Resolve a real price at the CLI layer

`cli/bridge_measure.go` currently builds the eval agent with `Price` nil, and
says why: *"Price left nil deliberately: ScorePass's AcceptFreeCalls asserts
these calls are already paid for by the hosting ticker."* That comment is a
Together truth stated as a universal one.

Instead, resolve a `*knov1.Price` from the **base model** — known and static at
quote time — exactly as `resolveTrainPrice` already resolves the training rate
from `(scheme, model)`. Using the base model rather than the deployed name
sidesteps the problem the Together adapter already names: a per-run-generated
endpoint id can never appear in a static table.

**No change to `core.Tuner`.** No new method, nothing breaking, and the pricing
decision lands beside the two that already live at that layer.

### 2. `AcceptFreeCalls` becomes a field, not a constant

`bridge/score.go`'s `AcceptFreeCalls: true` becomes a value the CLI sets per
scheme: true where serving is reserved capacity already metered by the ticker,
false where inference bills per token. **This repays #159**, and the entry is
marked repaid in the same PR rather than left for a later audit.

### 3. `Deploy`/`Teardown` as the no-op the interface already sanctions

`core.Tuner.Deploy`'s doc:

> A Tuner whose provider auto-serves a finished job (no separate deploy step)
> implements this as a no-op returning a **zero-rate Endpoint**, which is how the
> interface stays honest for a provider that is not Together.

Written for this case, never exercised. A place where the no-op does not fit is a
finding about the interface, not something to route around.

One hazard already retired: `--bridge-max-serve-minutes` did not bound a live run
at all, and a zero serve rate removed the cost-based backstop entirely. Fixed in
#206 before this plan proceeds, which is why the no-op is now safe to build on.

### 4. The eval-pass quote, without breaking the un-armed guarantee

`kno bridge` without `--bridge` advertises *"all locally, with zero network calls
and zero dollars spent"*, and never resolves `--evals`. Pricing the eval pass
from real token counts would need Case **content**, which for a remote
`--evals` source costs a network call — breaking that promise for every existing
Together user.

So the un-armed quote uses a **worst-case ceiling from the Case count**, the
shape `EstimateServeCap` already uses for hosting: a pessimistic per-Case token
bound times the number of Cases, needing no content. It over-states, which is the
safe direction for a figure someone consents to.

### 5. `resolveServePrice` must be able to say "confirmed zero"

`cli/bridge.go`'s `if priceUSDPerMinute > 0` means an explicit
`--price-serve-per-minute 0` falls through to the refusal. With `serveTable`
shipping empty, `kno bridge --tuner openai:<model>` would hard-refuse at
plan-print time with no way to unblock it: passing `0` fails the check, and any
positive number is a lie.

Fix, and add the OpenAI `serveTable` rows carrying a genuine zero — the
zero-versus-unknown distinction `core/ring0.go` already draws, applied to serving.

### 6. Adopt-by-suffix uses `metadata`, not `suffix`

Verified against OpenAI's current API: the fine-tuning job object exposes no
field echoing the creation-time `suffix`. It appears only inside
`fine_tuned_model`, which is `null` until the job succeeds — precisely the
non-terminal state `ListJobs` exists to recover. List Jobs filters on `after`,
`limit` and `metadata[k]=v` only.

So the adapter tags each job with a `metadata` key at creation and filters on it.
`core.Tuner.ListJobs`'s parameter stays a `suffix` string; what an adapter *does*
with it is its own business, and its doc should say so rather than implying every
provider has a queryable suffix.

### 7. The adapter

`adapters/tuner/openai` implementing all eight methods. `ListEndpoints` has no
natural meaning where nothing is deployed — returning empty is honest, and its
doc should say that rather than leaving a reader to infer it.

## Alternatives considered

**A. Add `ServedPrice` to `core.Tuner`.** The first draft. Rejected above: it
breaks a Ring-0 interface to duplicate `core.Estimator`.

**B. Optional capability assertion** (`if p, ok := tuner.(ServedPricer); ok`),
the pattern already used for `ContextInjector`. Non-breaking, and still
unnecessary once (1) puts the decision at the CLI layer where the other two
pricing decisions live.

**C. A local/Ollama Tuner first.** Cheaper to test — no spend — and rejected as
the *first* second adapter: it shares OpenAI's auto-serve shape without its
per-token billing, so it exercises the `Deploy` no-op while leaving #159 open. It
confirms the easy half. Worth doing after.

**D. Leave bridge single-vendor.** Rejected: a shipped stage requiring one
vendor's account is a product problem, and the interface learns nothing.

## Edge cases

1. **A job that succeeds with a model that will not yet serve.** `Deploy` must
   not report ready before the model answers, or the eval pass fails against a
   model that exists but is not callable.
2. **Zero-rate endpoints and the hosting cap.** Now bounded by a clock (#206)
   rather than by cost, which is what makes a zero rate safe.
3. **`EstimateServeCap` with a zero rate** must render `$0.00` as *"this provider
   does not bill for serving"*, not as an unpriced run.
4. **A `metadata` key that collides** with a user's own. Namespace it.

## Test plan

- **A `Tuner` conformance suite in `coretest` — new work, not reuse.**
  `coretest` exports only `ConformIterator` today; there is nothing for `Tuner`.
  Building one spanning two structurally different adapters (deploy-required
  versus auto-serve, per-minute versus per-token, suffix versus metadata) is the
  point of having a second implementation, and it is not free.
- Recorded fixtures for the full job lifecycle including a failure; live tests
  opt-in and nightly.
- A test asserting the quote **includes** the eval pass under a per-token Tuner
  and **omits** it under a zero-marginal one — the behaviour #159 is about.
- `resolveServePrice` accepts an explicit zero and still refuses an absent price.

## Rollback

Additive: a new adapter, a CLI-layer price resolution, and one field that was a
constant. No interface change, so nothing out-of-tree breaks.

## Docs impact

`DESIGN.md` (Tuner adapters are listed at v0.4; this pulls one forward),
`docs/what-the-numbers-mean.md` on what a bridge quote covers under each billing
model, `cli/bridge`'s help, `docs/status.json`, `CHANGELOG.md`.

## Accepted risks

*To be filled by the second Phase 1 review, and mirrored to `docs/debt.md` with
triggers.*

Two the review should weigh:

1. **`adapters/agent/internal/transport` is unreachable** from `adapters/tuner/`,
   so the Together adapter reimplemented a local security layer and this one will
   face the same wall. Two instances is not yet the third that `CLAUDE.md`'s DRY
   rule fires on, but the together adapter's own comment already names the fix
   (`adapters/internal/transport`). Decide now or accept with a trigger.
2. **The un-armed quote's worst-case ceiling will over-state**, possibly by a
   lot. Over-stating a figure someone consents to is the safe direction, but a
   ceiling far above the real cost is its own kind of dishonest.
