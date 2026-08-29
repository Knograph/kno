package jsonl

import "github.com/knograph/kno/adapters/evals/split"

// SplitCounts is the shared dev/holdout division, aliased from
// adapters/evals/split so the denominator math cannot vary by source and
// jsonl's public surface stays unchanged for existing callers.
type SplitCounts = split.Counts

// DefaultHoldoutFrac is the holdout share jsonl applies when Options leaves
// it at zero, and MinHoldout is the smallest holdout that supports a
// meaningful interval.
const (
	DefaultHoldoutFrac = split.DefaultHoldoutFrac
	MinHoldout         = split.MinHoldout
)
