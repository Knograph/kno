package core

import (
	"context"
	"iter"
)

// splitCases yields a fixed list of Cases, for the in-package seal and
// holdout tests.
type splitCases struct{ cases []*Case }

func (s *splitCases) Cases(_ context.Context) (iter.Seq2[*Case, error], error) {
	return func(yield func(*Case, error) bool) {
		for _, c := range s.cases {
			if !yield(c, nil) {
				return
			}
		}
	}, nil
}
