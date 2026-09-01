# A quickstart that demonstrates the product working

**Status:** Phase 0 — plan. Not implemented.

## Problem

`docs/quickstart.gif` is the first thing a reader sees in the README. It ends:

```
Rejected 3
  brand-guide       no-effect   delta +0.0000, CI [-0.4837, +0.4837] crosses zero
  refund-policy-v3  no-effect   delta +0.0000, CI [-0.4837, +0.4837] crosses zero
  ship-promise      no-effect   delta +0.0000, CI [-0.4837, +0.4837] crosses zero
```

Selected 0. The exported tuning set is empty. The demo of a tool that measures
which data helps concludes that **nothing helps**, every time, on the most-read
surface the project has.

This is long-standing, not a regression. Verified against the binary from before
the treatment-arm fix (`8f09b10~1`) on the same fixtures: byte-identical output.

## Why it cannot be fixed with better fixtures

The obvious reading is that the demo Assets are weak. They are not the cause.

`adapters/agent/fake`'s injected wrapper is:

```go
func (c *contextAgent) Invoke(ctx context.Context, cs *core.Case) (*core.Response, error) {
	n, _ := c.inner.injected.LoadOrStore(c.asset.GetId(), new(atomic.Int64))
	n.(*atomic.Int64).Add(1)
	return c.inner.Invoke(ctx, cs)   // the Asset is discarded
}
```

It records that an injection happened, then answers from the Case alone. The
Asset never reaches the answer. So the treatment arm's response is **identical**
to the control's for every Case, the paired difference is exactly zero by
construction, and no arrangement of fixture content can produce a non-zero
delta. The quickstart is not unlucky; it is closed-form.

Worth stating plainly because it also explains a bug: this is why the
empty-`&Asset{Id:}` defect (#190) survived against `fake:` for so long. An
adapter that ignores the Asset cannot tell a populated one from an empty one.
`fake.WithContextSet` **does** refuse an empty set, with reasoning that names the
exact failure — *"reads in the report as 'measured, and inert', the one
conclusion the stage exists to reach honestly"* — while `WithContext` has no such
guard. The asymmetry is the hole.

## Proposed design

### 1. `fake:` conditions its answer on the injected Asset

The minimum change that makes any demo possible. Injection must be able to change
an answer, or the fake cannot model the thing Kno measures.

Proposed rule, chosen because it is one line to describe and impossible to
mistake for cleverness: **a Case whose `expected` appears in the injected Asset's
content is answered correctly; otherwise the fake answers as it does today.**

That models the real phenomenon honestly — context supplies knowledge the model
lacks — without simulating a model. It keeps determinism, which is what the fake
exists for: the same (Case, Asset) pair always produces the same answer.

Fixtures then split cleanly. Some Cases are answerable from the Case alone (the
fake already gets these right, so a good Asset shows **no** gain on them — which
is itself worth showing). Some are answerable only with the right Asset. And an
Asset that carries nothing relevant measures as no-effect, correctly.

**A demo that shows only wins is a worse demo.** The output should have a
selected Asset *and* a rejected one, because "this one did not help" is the claim
the product exists to make.

### 2. `fake.WithContext` refuses an empty Asset

Mirroring `WithContextSet`, with the same reasoning it already carries. This is
independent of the demo and worth doing regardless: it closes the hole that let
#190 ship.

### 3. Re-cut the quickstart fixtures

Small enough to read in a GIF, with a holdout large enough not to trip the
underpowered warning — the current 12 Cases leave 4, and the run warns about it
on camera. Sizing is set by what `split.MinHoldout` needs, not by taste.

### 4. Re-record, and pin the outcome

`tapes/quickstart.tape` re-recorded. The scenario suite in `uknoAI/kno-examples`
pins the fixtures' expected output, so a future change that silently returns the
demo to all-zeros fails there rather than being noticed by a reader.

## Alternatives considered

**A. Record the GIF against a real provider.** Most honest, and rejected: the
tape must be reproducible by a contributor with no API key, and a GIF whose
numbers cannot be regenerated is a screenshot with extra steps.

**B. Ship recorded fixtures from a real run, replayed by a fixture agent.** The
numbers would be real. Rejected for this plan and worth revisiting: it needs a
recorded-fixture agent that does not exist, and the recording would have to be
re-made whenever the demo changes — a maintenance cost the quickstart does not
earn today.

**C. Leave the demo and explain the zeros in README prose.** Rejected. A reader
who has to be told why the demo shows nothing has already formed the impression.

**D. Hand-write the GIF's output.** Rejected outright: the tape records a real
binary precisely so the README cannot drift from behaviour, and every gate in
this repository exists to stop exactly that.

## Edge cases

1. **Determinism must survive.** Golden tests across the repo depend on the
   fake's answers. The new rule changes an answer only when an Asset is injected
   AND contains the Case's expected text; an un-injected run is byte-identical to
   today. This must be asserted, not assumed.
2. **`WithContextSet` already joins content for Validate.** The same containment
   rule has to work for a set, or Validate's demo diverges from Value's.
3. **A gain that is too clean is its own problem.** If every routed Case flips,
   the interval is degenerate and Select reports a suspiciously perfect delta.
   Fixtures should leave some Cases unhelped, so the CI has width and the demo
   shows a real interval rather than a point.
4. **The underpowered warning** should be absent by design, not by luck.

## Test plan

- `adapters/agent/fake`: an un-injected run is byte-identical to today's; an
  injected Asset containing the expected text changes the answer; one that does
  not, does not; an empty Asset is refused.
- A golden over the quickstart fixtures pinning **one selected and one rejected**
  Asset, so a regression to all-zeros fails a test rather than a reader.
- `uknoAI/kno-examples`: the scenario expectations updated in the companion PR.

## Rollback

The fake's rule is additive and gated on injection; reverting restores today's
behaviour exactly. The fixtures and tape are data.

## Docs impact

README (the GIF and the prose around it), `tapes/quickstart.tape`, the
`kno-examples` scenario expectations, and `docs/what-the-numbers-mean.md` if the
demo's numbers are quoted anywhere.

## Accepted risks

*To be filled by Phase 1 review, and mirrored to `docs/debt.md` with triggers.*

One to weigh there: **a fake that can be improved by injection is a fake that can
flatter the product.** The mitigation is that the rule is mechanical and stated
in the docs — content containment, not a model of helpfulness — and that the
demo deliberately shows a rejection alongside a selection. A reviewer should
push on whether that is enough, or whether any synthetic demo of a measurement
tool is inherently a claim the tool cannot back.
