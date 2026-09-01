package core

import (
	"context"
	"fmt"
	"iter"

	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// holdoutEvals yields only holdout Cases. It is the mirror image of
// SealedEvals, and it is UNEXPORTED, which is the entire security decision of
// the Validate stage.
//
// SealedEvals is a distinct exported type because forgetting to seal has to be
// a compile error. The holdout reader has the opposite requirement: nothing
// outside core may ever build one. Unexported gives that for free and gives it
// absolutely — another package cannot name the type, cannot construct one
// reflectively, and cannot produce one with a composite or interface literal.
// That is strictly stronger than the seal's guarantee, which is a REQUIREMENT
// to wrap discharged by discipline at the call site.
//
// The guarantee is exact for CONSTRUCTION and narrower than it first reads for
// EXPOSURE. "Cannot construct" is not "cannot hold or use": interface
// satisfaction is indifferent to whether the concrete type is exported, so an
// exported core function returning an Evals INTERFACE value backed by a
// *holdoutEvals would let an outside caller read the holdout without ever
// naming the type. The invariant this package commits to is therefore stated
// positively:
//
//	No exported function, method, struct field, event or callback in core ever
//	returns or forwards a holdoutEvals-backed Evals.
//
// The value is built in openHoldout, consumed inside Validate, and never
// escapes that call. TestOnlyValidateOpensTheHoldout's fourth check is what
// keeps that true over time; the type system alone does not, and pretending
// otherwise would be the kind of overclaim this file exists to avoid. See
// docs/adr/0007-the-holdout-opener-is-unexported.md.
type holdoutEvals struct{ inner Evals }

// openHoldout is the one legitimate path to the holdout, and it is unexported
// for the reason holdoutEvals is.
//
// Handed a *SealedEvals it returns ErrHoldoutSealed rather than iterating one
// and yielding nothing. That distinction is the point: a sealed source filters
// to SPLIT_DEV, so opening one as a holdout would produce ZERO Cases —
// indistinguishable in every downstream surface from "your eval set has no
// holdout", which is a silent, plausible, wrong answer. It is the exact
// failure mode core/seal.go's unassigned-split filter and split.Counts.Validate
// are both built to prevent, and it is the first non-test caller of
// errs.ErrHoldoutSealed.
func openHoldout(e Evals) (*holdoutEvals, error) {
	if e == nil {
		return nil, errs.ErrInvalidInput.
			WithFix("point --evals at the same eval source the pipeline was measured against").
			Wrap(fmt.Errorf("validate: there is no eval source to open a holdout from"))
	}
	if _, sealed := e.(*SealedEvals); sealed {
		return nil, errs.ErrHoldoutSealed.Wrap(fmt.Errorf(
			"validate: the eval source handed to the holdout opener is already sealed to " +
				"its dev split; iterating it would yield no Cases at all, which reads " +
				"downstream as an eval set with no holdout rather than as this mistake"))
	}
	return &holdoutEvals{inner: e}, nil
}

// Cases yields the holdout Cases.
//
// SealedEvals.Cases with one line changed: SPLIT_HOLDOUT passes and everything
// else is skipped. SPLIT_UNSPECIFIED is skipped for the mirror-image reason
// the seal gives — a Case with no split has not been through ingestion, and
// admitting it here inflates the denominator of the only number that belongs
// in a slide.
//
// Like the seal, this filters and does not independently enforce the rest of
// the Evals contract: cancellation checks, cleanup inside the closure, and the
// borrow rule remain the inner producer's obligations.
func (h *holdoutEvals) Cases(ctx context.Context) (iter.Seq2[*Case, error], error) {
	if h == nil || h.inner == nil {
		return nil, fmt.Errorf("core: holdout evals has no source")
	}

	inner, err := h.inner.Cases(ctx)
	if err != nil {
		return nil, err
	}

	return func(yield func(*Case, error) bool) {
		for c, err := range inner {
			if err != nil {
				// Fatal by contract. Pass it through and stop.
				yield(nil, err)
				return
			}
			if c.GetSplit() != knov1.Split_SPLIT_HOLDOUT {
				continue
			}
			if !yield(c, nil) {
				return
			}
		}
	}, nil
}

// Compile-time proof that a holdout source satisfies Evals, so it composes
// with the same iteration helpers the rest of the engine uses — while staying
// unconstructible outside this package.
var _ Evals = (*holdoutEvals)(nil)
