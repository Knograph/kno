package store_test

import (
	"context"
	"errors"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/store"
	"github.com/stretchr/testify/require"
)

// gapsFixture is one Export run with a small gaps record written for it.
func gapsFixture(t *testing.T, s *store.SQLite, runID string) *knov1.Gaps {
	t.Helper()
	run := &knov1.Run{
		Id: runID, Stage: knov1.Stage_STAGE_EXPORT,
		Status: knov1.RunStatus_RUN_STATUS_COMPLETED, GoalName: "g",
	}
	require.NoError(t, s.CreateRun(context.Background(), run))
	g := &knov1.Gaps{
		RunId: runID, MultipleTesting: true,
		Clusters: []*knov1.GapCluster{{
			Tag: "billing", CaseCount: 8, CoveredCount: 6,
			Status:      knov1.GapStatus_GAP_STATUS_IMPROVED,
			BestAssetId: "a", BestDelta: 0.5,
			BestInterval: &knov1.Interval{
				Low: 0.2, High: 0.8, Level: 0.95,
				Sidedness: knov1.Sidedness_SIDEDNESS_TWO_SIDED,
			},
		}},
	}
	require.NoError(t, s.WriteGaps(context.Background(), runID, g))
	return g
}

// TestGapsRoundTrip: the gaps record survives a write and read exactly — the
// report reads the verdicts Export computed, not a reshaping of them.
func TestGapsRoundTrip(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	written := gapsFixture(t, s, "exp-1")
	got, err := s.Gaps(context.Background(), "exp-1")
	require.NoError(t, err)
	require.Equal(t, written.GetRunId(), got.GetRunId())
	require.True(t, got.GetMultipleTesting())
	require.Len(t, got.GetClusters(), 1)
	c := got.GetClusters()[0]
	require.Equal(t, "billing", c.GetTag())
	require.Equal(t, knov1.GapStatus_GAP_STATUS_IMPROVED, c.GetStatus())
	require.Equal(t, "a", c.GetBestAssetId())
	require.Equal(t, 0.5, c.GetBestDelta())
	require.Equal(t, 0.8, c.GetBestInterval().GetHigh())
}

// TestGapsNotFound: a run that computed no gaps reads as not-found — the
// report's "no cluster data for this run" answer, never an empty verdict set.
func TestGapsNotFound(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	_, err := s.Gaps(context.Background(), "never-exported")
	require.ErrorIs(t, err, store.ErrGapsNotFound)
}

// TestGapsReplaceOnRewrite: a re-export that recomputes replaces the row —
// the record matches the current computation, never the first write.
func TestGapsReplaceOnRewrite(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	gapsFixture(t, s, "exp-1")
	g, err := s.Gaps(context.Background(), "exp-1")
	require.NoError(t, err)
	g.Clusters = nil
	g.MultipleTesting = false
	require.NoError(t, s.WriteGaps(context.Background(), "exp-1", g))
	got, err := s.Gaps(context.Background(), "exp-1")
	require.NoError(t, err)
	require.Empty(t, got.GetClusters())
	require.False(t, got.GetMultipleTesting())
}

// TestGapsValidatesItsRun: the record names the run it belongs to, and the
// run must exist — a gaps row for a run the store never saw is refused.
func TestGapsValidatesItsRun(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	err := s.WriteGaps(context.Background(), "ghost", &knov1.Gaps{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "run ID")

	err = s.WriteGaps(context.Background(), "ghost", &knov1.Gaps{RunId: "ghost"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "recording gaps")
}

// TestGapsCascadeWithItsRun: deleting the Export run deletes its gaps row —
// the record is derived from the run, not a document of its own.
func TestGapsCascadeWithItsRun(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	gapsFixture(t, s, "exp-1")
	// The cascade fires on a plain DELETE of the run row: the gaps record is
	// derived from the run, not a document of its own.
	require.NoError(t, s.ExecForTest(context.Background(),
		`DELETE FROM runs WHERE id = 'exp-1'`))
	_, err := s.Gaps(context.Background(), "exp-1")
	require.ErrorIs(t, err, store.ErrGapsNotFound)
}

// TestGapsClosedStore: a closed store refuses both sides.
func TestGapsClosedStore(t *testing.T) {
	t.Parallel()

	s, err := store.NewSQLite(context.Background(), t.TempDir()+"/kno.db")
	require.NoError(t, err)
	require.NoError(t, s.Close())
	_, err = s.Gaps(context.Background(), "x")
	require.Error(t, err)
	err = s.WriteGaps(context.Background(), "x", &knov1.Gaps{RunId: "x"})
	require.Error(t, err)
	require.False(t, errors.Is(err, store.ErrGapsNotFound))
}
