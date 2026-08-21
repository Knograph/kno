package core

import (
	"context"
	"fmt"
	"iter"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// SealedEvals yields only dev Cases. Holdout Cases are not reachable through
// it at all.
//
// This is a distinct TYPE rather than a wrapper returning core.Evals, and that
// is the entire point. A function that requires a *SealedEvals cannot be handed
// a raw adapter: forgetting to seal is a compile error, not a discipline lapse
// discovered by review three months from now.
//
// The earlier design returned core.Evals from Seal, which meant any code
// holding the raw adapter compiled cleanly and read the holdout. A canary test
// would then have proven only that one call site behaved on the day it was
// written.
//
// Why the seal lives here and not in the adapter: an Evals adapter reads a
// file. It has no idea which stage is running, so any restriction it enforced
// would be advisory, in the ring with the least context and the most
// implementations. The engine knows the stage; the engine holds the seal.
type SealedEvals struct {
	inner Evals
}

// Seal restricts an Evals source to its dev split.
//
// Every stage before Validate takes the result. Validate reads the holdout
// through a separate path that does not exist yet — deliberately. Designing
// how a holdout is legitimately opened, and who may do it, belongs with the
// stage that needs it, not three milestones early against a token nobody can
// meaningfully gate.
func Seal(e Evals) *SealedEvals {
	return &SealedEvals{inner: e}
}

// Cases yields the dev Cases.
//
// It preserves the Evals.Cases contract and adds one rule: a Case whose Split
// is anything other than SPLIT_DEV is not yielded.
//
// The seal filters; it does not independently enforce the rest of the
// contract. Cancellation checks, cleanup-inside-the-closure, and the borrow
// rule remain the inner producer's obligations, and a non-conformant producer
// stays non-conformant through the seal. coretest.ConformIterator is what
// holds adapters to those; this type holds them to the split.
//
// An unassigned Split (SPLIT_UNSPECIFIED) is filtered out too, and that is not
// an accident. A Case with no split has not been through ingestion, and
// treating "unknown" as "dev" is exactly how a holdout leaks: silently, one
// Case at a time, in the direction that inflates the reported gain.
func (s *SealedEvals) Cases(ctx context.Context) (iter.Seq2[*Case, error], error) {
	if s == nil || s.inner == nil {
		return nil, fmt.Errorf("core: sealed evals has no source")
	}

	inner, err := s.inner.Cases(ctx)
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
			if c.GetSplit() != knov1.Split_SPLIT_DEV {
				continue
			}
			if !yield(c, nil) {
				return
			}
		}
	}, nil
}

// Compile-time proof that a sealed source still satisfies Evals, so it composes
// with anything reading Cases — while the reverse, handing a raw Evals to
// something that requires a seal, remains impossible.
var _ Evals = (*SealedEvals)(nil)
