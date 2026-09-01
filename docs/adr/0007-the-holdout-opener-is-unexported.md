# ADR-0007: the holdout opener is unexported, and that guarantee has an exact size

- **Status:** accepted
- **Date:** 2026-08-31
- **Context:** [The Validate plan](../plans/2026-08-31-validate.md) §2, and the Phase-1 finding (F3) that narrowed the claim.

## The problem

Prime directive 5 says no holdout access before Validate. Until Validate shipped, that rule cost nothing to keep, because there was no way to read a holdout at all: `core.SealedEvals` filters *to* `SPLIT_DEV` and has no inverse, and `errs.ErrHoldoutSealed` was declared and returned by no non-test code. The seal was enforced by the type system's silence.

Validate has to break that silence. Something in this repository must now be able to iterate holdout Cases, and the question this record answers is **who** — not as a convention, but as a property of the code that survives contributors who have not read the convention.

The obvious shapes both put the reader on the public API. `Seal` returns an exported value, so a symmetric `Unseal` returning an exported `core.HoldoutEvals` is the design the existing code suggests. `core/seal.go:34-38` had already flagged the trap in that symmetry, before the stage existed:

> Validate reads the holdout through a separate path that does not exist yet — deliberately. Designing how a holdout is legitimately opened, and who may do it, belongs with the stage that needs it, not three milestones early against a token nobody can meaningfully gate.

The asymmetry that makes this a decision rather than a style question: **the seal and the opener have opposite failure modes.** Forgetting to seal must be a compile error, which is why `SealedEvals` is an exported, distinct type — the caller has to say the word. The opener's requirement is the mirror: nothing outside `core` may ever build one. An exported type discharges a *requirement to wrap*; it cannot discharge a *prohibition on constructing*.

## The decision

**`holdoutEvals` and `openHoldout` are package-private to `core`.** `ValidateOptions.Evals` is a plain `Evals`; `core.Validate` calls `openHoldout` itself. `cli`, `tui`, `api` and every external consumer therefore **cannot construct a holdout reader** — not "must not". Another package cannot name the type, cannot build one reflectively, and cannot produce one with a composite or interface literal. Go leaves no route around that, and it costs an exported symbol nothing, because the option struct takes the raw `Evals` under either design.

**A capability token was rejected.** `Unseal(token ValidateToken)` mirrors `Seal` and buys nothing: a token minted inside `core` is forgeable by anything inside `core`, which is the only place the token would ever be checked. It costs an exported type, an exported constructor, and a public promise the code cannot keep — and it invites `cli` to hold one, at which point the guarantee degrades from *impossible* to *reviewed*.

**An exported `core.HoldoutEvals` + `core.OpenHoldout` was rejected** for the same reason at lower cost. It is usable by `cli`, `api`, and every consumer not yet written, and prime directive 5 should not rest on those callers being careful.

**The guarantee's exact scope, stated because overclaiming it is how it gets weakened.** Unexported means *unconstructible outside `core`*. It does not mean *unholdable*. Interface satisfaction is indifferent to whether the concrete type is exported: an exported `core` function, event field or callback returning an `Evals` **interface** value backed by a `*holdoutEvals` would let an outside caller call its exported `Cases` method and read the holdout without ever naming the type. So the invariant this repository actually commits to is stated positively, not as a consequence of the export rules:

> No exported function, method, struct field, event or callback in `core` ever returns or forwards a holdoutEvals-backed `Evals`.

**And `openHoldout` refuses a `*SealedEvals` with `ErrHoldoutSealed`** rather than iterating one and yielding nothing. A sealed source filters to `SPLIT_DEV`, so opening one as a holdout would produce zero Cases — indistinguishable downstream from "your eval set has no holdout", which is a silent, plausible, wrong answer. This is the sentinel's first non-test caller.

## What this record does not do

**It does not claim the type system enforces the positive invariant.** The compiler enforces the construction half and nothing else. The rest is held by `TestOnlyValidateOpensTheHoldout` and its siblings in `core/holdout_boundary_test.go`, which parse `core`'s own non-test files with `go/ast` and assert four things: exactly one `ast.CallExpr` whose `Fun` is `openHoldout`, in `core/validate.go`, zero everywhere else including the file that declares it; no `*holdoutEvals` in any exported signature within `core`; **no exported outlet in `core` whose type is the `Evals` interface at all** — the check that closes the interface hole above, currently empty, so the PR that opens an outlet fails and its author has to argue the new outlet cannot be handed a holdout; and no reference to either identifier anywhere in the module outside `core`.

**That test is a bespoke AST walk, and it is deliberately not compared to `core/boundary_test.go`.** The import-boundary test shells out to `go list -deps` and asserts over the **import graph**: a coarse, total relation computed by the toolchain, hard to fool and impossible to half-satisfy. This one reasons about occurrences *inside a single package*, where the import graph says nothing, and it is the walk itself — not the toolchain — that a reviewer has to trust. It can in principle be evaded by an alias, a method value, or a wrapper it does not model. Its own correctness is pinned by falsification cases rather than by precedent, and the residual risk is [ledgered](../debt.md) with a trigger rather than argued away.

**It does not weaken the existing canaries.** `Baseline` and `Value` still fail their planted-holdout-Case tests if the seal is removed from either. This record adds a path for one stage; it does not open one for the others.

## Consequences

- The holdout reader has no public API surface, so there is nothing for a future contributor to "improve" by exporting — and if someone tries, this record is what a reviewer cites.
- `errs.ErrHoldoutSealed` is reachable at last, which means the "sealed source passed to validate" case fails loudly instead of reporting an empty holdout.
- Adding any exported `core` symbol that returns an `Evals` is now a gated change: it fails a test, and the fix is an argument, not a code edit.
- The claim in `docs/mental-model.md` — that `cli` cannot construct a holdout reader — is true as written and stops there. Anything stronger about *holding* one rests on a test, and both documents say so.
