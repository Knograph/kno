package core

import (
	"context"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/stretchr/testify/require"
)

// TestExplainPrintsThePerCaseTableForARedundantAsset is acceptance criterion
// 15's core: `--explain` on an Asset rejected REDUNDANT returns the per-Case
// table for every Asset named in its evidence, over the shared slice, and
// makes no store call beyond what Select itself would make.
func TestExplainPrintsThePerCaseTableForARedundantAsset(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	pass := intRange(16)
	seedRedundancyFixture(
		t, st, "val", knov1.Direction_DIRECTION_MAXIMIZE,
		measuredAsset{id: "a", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 100},
		measuredAsset{id: "b", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 100},
	)

	opts := selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 100000}, nil)
	opts.RedundancyMaxMargin = 0.4

	cmps, err := opts.Explain(context.Background(), "b", 0)
	require.NoError(t, err)
	require.Len(t, cmps, 1)
	require.Equal(t, "a", cmps[0].WithAssetID)
	require.NotNil(t, cmps[0].Evidence)
	require.Len(t, cmps[0].Rows, 20, "every shared Case appears in the table")
	for _, row := range cmps[0].Rows {
		require.Equal(t, row.ThisDelta, row.WithDelta, "identical fixtures: every row's two deltas agree")
	}
}

// TestExplainReturnsNothingForANonRedundantAsset: an Asset that was selected,
// or rejected for a different reason, has nothing to explain — nil, not an
// error.
func TestExplainReturnsNothingForANonRedundantAsset(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	seedValueRun(t, st, "val", testValuation("a", 0.5, 0.2))

	opts := selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 1000}, nil)
	cmps, err := opts.Explain(context.Background(), "a", 0)
	require.NoError(t, err)
	require.Nil(t, cmps)

	cmps, err = opts.Explain(context.Background(), "does-not-exist", 0)
	require.NoError(t, err)
	require.Nil(t, cmps)
}
