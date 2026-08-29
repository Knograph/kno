package hf

import "github.com/knograph/kno/adapters/evals/split"

// SplitCounts is the shared dev/holdout division, aliased from
// adapters/evals/split so the CLI can name one type across every Evals
// adapter. Same pattern as the langfuse adapter.
type SplitCounts = split.Counts
