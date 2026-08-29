package store_test

import (
	"context"
	"errors"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/store"
	"github.com/stretchr/testify/require"
)

// portfolioFixture is one Select run with a small Portfolio recorded for it.
func portfolioFixture(t *testing.T, s *store.SQLite, runID string) *knov1.Portfolio {
	t.Helper()
	run := &knov1.Run{
		Id: runID, Stage: knov1.Stage_STAGE_SELECT,
		Status: knov1.RunStatus_RUN_STATUS_COMPLETED, GoalName: "g",
	}
	require.NoError(t, s.CreateRun(context.Background(), run))
	p := &knov1.Portfolio{
		RunId: runID, SourceRunId: "val-1",
		Selected: []*knov1.PortfolioEntry{{
			AssetId: "a", Rank: 1, Destination: knov1.Destination_DESTINATION_CONTEXT,
			Valuation: &knov1.Valuation{AssetId: "a", DeltaGoal: 0.5},
		}},
		TotalCost: &knov1.CostVector{ContextTokens: 10},
	}
	require.NoError(t, s.WritePortfolio(context.Background(), runID, p))
	return p
}

// TestPortfolioRoundTrip: the Portfolio survives a write and read exactly —
// the derived artifact a consumer loads is the one the stage decided.
func TestPortfolioRoundTrip(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	written := portfolioFixture(t, s, "sel-1")
	got, err := s.Portfolio(context.Background(), "sel-1")
	require.NoError(t, err)
	require.Equal(t, written.GetRunId(), got.GetRunId())
	require.Equal(t, written.GetSourceRunId(), got.GetSourceRunId())
	require.Len(t, got.GetSelected(), 1)
	require.Equal(t, "a", got.GetSelected()[0].GetAssetId())
	require.Equal(t, int64(10), got.GetTotalCost().GetContextTokens())
}

// TestPortfolioNotFound: a run that never selected reads as not-found, not
// as an empty Portfolio — "Select never ran" is not "nothing was selected".
func TestPortfolioNotFound(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	_, err := s.Portfolio(context.Background(), "never-selected")
	require.ErrorIs(t, err, store.ErrPortfolioNotFound)
}

// TestPortfolioReplaceOnResume: a resumed Select run that decides again
// replaces the recorded Portfolio — the row matches the current decision,
// never the first write.
func TestPortfolioReplaceOnResume(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	portfolioFixture(t, s, "sel-1")
	p, err := s.Portfolio(context.Background(), "sel-1")
	require.NoError(t, err)
	p.Selected = nil
	require.NoError(t, s.WritePortfolio(context.Background(), "sel-1", p))
	got, err := s.Portfolio(context.Background(), "sel-1")
	require.NoError(t, err)
	require.Empty(t, got.GetSelected())
}

// TestPortfolioValidatesItsRun: the Portfolio names the run it belongs to,
// and the run must exist — a Portfolio for a run the store never saw is
// refused.
func TestPortfolioValidatesItsRun(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	err := s.WritePortfolio(context.Background(), "ghost", &knov1.Portfolio{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "run ID")

	err = s.WritePortfolio(context.Background(), "ghost", &knov1.Portfolio{RunId: "ghost"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "recording portfolio")
}

// TestPortfolioClosedStore: a closed store refuses both sides — reading and
// writing need a live connection.
func TestPortfolioClosedStore(t *testing.T) {
	t.Parallel()

	s, err := store.NewSQLite(context.Background(), t.TempDir()+"/kno.db")
	require.NoError(t, err)
	require.NoError(t, s.Close())
	_, err = s.Portfolio(context.Background(), "x")
	require.Error(t, err)
	err = s.WritePortfolio(context.Background(), "x", &knov1.Portfolio{RunId: "x"})
	require.Error(t, err)
	require.False(t, errors.Is(err, store.ErrPortfolioNotFound))
}
