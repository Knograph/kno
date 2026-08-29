package langsmith

import "github.com/knograph/kno/adapters/evals/split"

// SplitCounts is the shared dev/holdout division, aliased from
// adapters/evals/split so langsmith's CountSplits is shaped exactly like
// jsonl's for the renderers, which take the shared type.
type SplitCounts = split.Counts
