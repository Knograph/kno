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

**Corrected after the second review (P1): the base rate alone is a systematic
UNDER-estimate, and that is worse than no estimate at all.**

OpenAI bills fine-tuned inference above the base rate. `openaicompat` uses the
same `a.price` for both the per-Case reservation and the settlement of the
invoked model (`adapters/agent/openaicompat/estimate.go`), so reservation and
settlement would agree with each other and both be wrong against the real
invoice. A user setting `--max-cost-usd 5` reaches "cap reached" on arithmetic
that understates what OpenAI actually charged.

That is worse than having no estimate, and the code says so: with no `Estimator`
under a cap, `core.ScorePass.estimate()` returns `unpriceable` and the Case is
refused — an honest stop (`core/score.go`). A zero estimate under a cap is
refused for the same reason. Only a *confident wrong number* gets authorized.

**Corrected again after the third review: a multiplier is the fabricated-rate
failure this repository refuses elsewhere, and the analogy was wrong.**

`TrainingHeadroomPct` and `RegionalMultiplierPct` bound genuinely *unmeasurable*
provider-internal slop — packed sequences, padding, infra markup with no clean
per-model source. A fine-tuned model's inference rate is not unmeasurable:
OpenAI publishes it, per base model, on the same page `table.go` already cites.
Inventing a multiplier where a real number is look-up-able is precisely what
`trainTable` and `serveTable` ship **empty** to prevent, and what `table.go`'s
own doctrine states: *"An unknown model is NOT priced at zero... Lookup reports
absence; the caller decides."*

So: a `fineTunedTable` keyed by `(scheme, base model)`, carrying published
fine-tuned inference rates, **shipping empty** and filled by a reviewed diff
exactly as the other two tables are. A base model with no row is **refused**
under a cap, not multiplied. That is the same honest stop `ScorePass.estimate()`
already makes for an unpriceable Case, and it keeps the property that no number
in the guard's path was invented by us.

**No signature change to `core.Tuner`.** No new method, nothing breaking. (§6
does edit one doc comment on it — non-breaking, but worth saying so rather than
surprising a reviewer diffing `core/`.)

**The price must be threaded, not re-derived (third review).**
`bridgeAgentFactory` hardcodes `Price: nil` today and also hardcodes
`Ref.Scheme: "openai"` for Together's OpenAI-compatible HTTP route. Two plain
strings both called "scheme", and the type system will not catch a mix-up:
re-deriving the price inside the factory off `ref.Scheme` would resolve
OpenAI-table rates for a Together model, charging per call on top of the hosting
ticker — the double-count the seam plan exists to prevent. The eval price is
resolved once in `runBridgeCore`, beside `resolveTrainPrice`/`resolveServePrice`,
and passed down.

### 2. `AcceptFreeCalls` becomes a field, not a constant

`bridge/score.go`'s `AcceptFreeCalls: true` becomes a value the CLI sets.

**Corrected after the second review (P5): keyed on whether a price resolved, not
on the scheme.** `AcceptFreeCalls := evalPrice == nil` — free only when this
model has no per-token rate at all. Keying on scheme would bake in an assumption
that breaks the first time one provider offers both reserved capacity and
per-token serving, and it would mean another hardcoded `switch scheme` beside the
two that already exist. Deriving it from the resolved price is scheme-agnostic,
costs no extra code, and handles a hybrid provider without a new branch.

**This repays #159**, and the entry is marked repaid in the same PR rather than
left for a later audit.

### 3. `Deploy`/`Teardown` as the no-op the interface already sanctions

`core.Tuner.Deploy`'s doc:

> A Tuner whose provider auto-serves a finished job (no separate deploy step)
> implements this as a no-op returning a **zero-rate Endpoint**, which is how the
> interface stays honest for a provider that is not Together.

Written for this case, never exercised. A place where the no-op does not fit is a
finding about the interface, not something to route around.

Two hazards retired before this plan proceeds, both found by reviewing it:

- `--bridge-max-serve-minutes` did not bound a live run at all, and a zero serve
  rate removed the cost-based backstop entirely (#206).
- **The plan previously claimed #206 made the no-op safe. It did not.** Both the
  deadline and the hosting ticker are guarded on `Endpoint.ReadyAt`, and a no-op
  written the natural way — `return &core.Endpoint{Ready: true}, nil` — sets no
  `ReadyAt`, disarming both silently. `DeployGroup` now refuses that (#208).

So the adapter's `Deploy` MUST stamp `ReadyAt`. That is now enforced centrally,
and the `coretest` conformance suite (see Test plan) asserts it too — not
because `DeployGroup` might forget, since #208 makes that refusal universal, but
because it fails at the adapter's own unit level rather than requiring a full
`DeployGroup` integration path, and it documents the contract for the next
adapter author before they write `Deploy`.

### 4. The eval-pass quote, without breaking the un-armed guarantee

`kno bridge` without `--bridge` advertises *"all locally, with zero network calls
and zero dollars spent"*, and never resolves `--evals`. Pricing the eval pass
from real token counts would need Case **content**, which for a remote
`--evals` source costs a network call — breaking that promise for every existing
Together user.

So the un-armed quote uses a **worst-case ceiling from the Case count**, the
shape `EstimateServeCap` already uses for hosting.

**Corrected after the third review: it must NOT construct an Agent to get one.**
`Agent.WorstCase()` is content-free, but `openaicompat.New` refuses construction
without a bound credential — so computing the ceiling that way would make
`kno bridge` (un-armed, no `OPENAI_API_KEY`) fail to print a plan at all,
breaking the command's own promise of *"all locally, with zero network calls and
zero dollars spent... read the plan before you decide whether to pay for it."*
Today's un-armed path constructs no Agent, and `EstimateServeCap` — the hosting
analogue — is pure arithmetic over a `ServePrice`.

The ceiling is computed with `pricing.EstimateWithPrice`, already exported and
already what `WorstCase()` calls internally. No Agent, no credential, no network.

**Corrected after the second review (P4): the count is available un-armed, but
the wiring is not, and the plan has to name it.** The Case IDs come from the
persisted `value.Plan`, which `runBridgeCore` already loads locally with no
network and no `--evals`. The computation that derives them currently lives
inside `confirmAndRun`, reached only *after* arming and after the first
`Authorize`. It must move or be duplicated ahead of `renderBridgePlan`, and
**both** the printed plan and `confirmAndRun`'s consent total need a third
addend. A ceiling enforced silently at runtime by `ScorePass`'s guard, without
appearing in the figure the user agreed to, is the same defect in a new place.

### 5. `resolveServePrice` must be able to say "confirmed zero"

`cli/bridge.go`'s `if priceUSDPerMinute > 0` means an explicit
`--price-serve-per-minute 0` falls through to the refusal. With `serveTable`
shipping empty, `kno bridge --tuner openai:<model>` would hard-refuse at
plan-print time with no way to unblock it: passing `0` fails the check, and any
positive number is a lie.

**The wiring, named rather than left to the keyboard (third review).** The
precedent is `cli/baseline.go`, and the mechanism is three parts, not one:
`bridgeFlags` gains a `priceServeUSDSet bool`; `newBridgeCmd`'s `RunE` captures
`cmd.Flags().Changed("price-serve-per-minute")` into it, because `runBridgeCore`
receives a plain struct and never sees `cmd`; and `resolveServePrice` takes that
flag. `cli/bridge_test.go` builds `bridgeFlags{}` literals directly at several
call sites, bypassing cobra entirely — so a unit test can only exercise the
field, and proving the `Changed()` capture works needs a cobra-level test too.

**Corrected after the second review (P3): the mechanism matters, and the naive
one is worse than the bug.** `--price-serve-per-minute` defaults to `0`, so at
the `float64` level an explicit `0` is indistinguishable from an absent flag.
Relaxing `> 0` to `>= 0` would make **every unset flag, on every scheme**,
silently resolve to "confirmed free" — a far larger hole than the one being
fixed. The presence check is `cmd.Flags().Changed("price-serve-per-minute")`,
the same way `--cost-per-call-usd` already distinguishes an explicit zero from
no claim (`cli/baseline.go`).

Add the OpenAI `serveTable` rows carrying a genuine zero as well — the
zero-versus-unknown distinction `core/ring0.go` already draws, applied to
serving. With a table row present, `LookupServePrice` succeeds on map presence
and the flag path is not reached for OpenAI at all; the flag fix is for
everything else.

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

## This is two PRs, not one

Third review, and correct: the plan bundles four separable workstreams. Splitting
them along package boundaries, per `CLAUDE.md`'s own decomposition rule:

**PR 1 — the pricing seam.** §1, §2, §4, §5, plus the `newBridgeTuner` registry.
Touches `cli/` and `bridge/score.go` only. Repays #159 and #161. Testable today
against Together with **no behaviour change** — `evalPrice` stays nil because
`table.go` has no `"together"` key — plus a synthetic priced Tuner fixture for
"the quote includes the eval pass under a per-token Tuner". No adapter needed.

**PR 2 — the adapter.** `adapters/tuner/openai`, the `coretest` Tuner conformance
suite, and the real fine-tuned-inference table rows. The conformance suite needs
two structurally different adapters to mean anything, so it belongs here.

**The sequencing is a safety property, not tidiness.** PR 2 must not be reachable
via `--tuner openai:` before PR 1 merges, or it ships the exact
asserted-free-while-really-billing hole #159 describes.

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

**Debt dispositions this PR owes**, per CLAUDE.md's ledger rules:

- **#159 — repaid.** `AcceptFreeCalls` stops being a constant.
- **#161 — its trigger fires on this PR.** It reads in part *"when a second
  Tuner adapter lands and the `switch scheme` in `newBridgeTuner`/
  `bridgeAgentFactory` needs a real second case."* That is exactly this change.
  **Corrected after the third review: §2 does not repay it.** §2 is about
  `AcceptFreeCalls`; adding an OpenAI branch to `newBridgeTuner` and
  `bridgeAgentFactory` *adds an arm to* the switch rather than removing it. Only
  a scheme-keyed registry (`map[string]TunerFactory`) removes it. PR 1 builds
  that registry and repays #161 properly; if it does not, #161 is re-dated with
  a written reason in the same PR. Naming a fired trigger without dispositioning
  it is the silent carryover the ledger exists to prevent.

Three the review should weigh:

3. **`api` and `tui` cannot reach this pricing decision.** `resolveTrainPrice`
   and `resolveServePrice` are already unexported in `cli/`, and this adds a
   third money decision to that pile. Prime directive 3 says the shells are
   supposed to be thin over identical engine calls; a future `kno serve` bridge
   endpoint would have to re-derive all three, with real risk of the same run
   being priced differently depending on which shell invoked it. Pre-existing,
   worsened here. *Suggested trigger: before `kno serve` exposes a bridge
   endpoint.*

Two more the review should weigh:

1. **`adapters/agent/internal/transport` is unreachable** from `adapters/tuner/`,
   so the Together adapter reimplemented a local security layer and this one will
   face the same wall. Two instances is not yet the third that `CLAUDE.md`'s DRY
   rule fires on, but the together adapter's own comment already names the fix
   (`adapters/internal/transport`). Decide now or accept with a trigger.
2. **The un-armed quote's worst-case ceiling will over-state**, possibly by a
   lot. Over-stating a figure someone consents to is the safe direction, but a
   ceiling far above the real cost is its own kind of dishonest.
3. **No escape-hatch flag exists for the eval-pass rate.** `--price-train-per-mtok`
   and `--price-serve-per-minute` cover their dimensions; there is no equivalent
   for inference. `table.go` carries three OpenAI rows, so any other fine-tunable
   base model has no rate, and every Case then refuses under a cap or falls back
   to `EstCostPerCallUSDMicros` without one — reopening the free-calls hole for
   an untabled model.
