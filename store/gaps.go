package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"google.golang.org/protobuf/proto"
)

// WriteGaps records the gaps verdicts one Export run computed, keyed by the
// EXPORT run that produced them — the same key the report reader will ask
// by. The source Value run that generated the cluster data is reachable
// through the Export run's portfolio; see Gaps.
//
// INSERT OR REPLACE, like WritePortfolio: gaps are derived from the source
// run's plan and valuations, so a re-export that recomputes them must
// produce the row that matches the current computation rather than pinning
// the first one.
func (s *SQLite) WriteGaps(ctx context.Context, runID string, g *knov1.Gaps) error {
	db, err := s.conn()
	if err != nil {
		return err
	}
	if g.GetRunId() == "" {
		return errors.New("store: gaps need a run ID")
	}
	blob, err := proto.Marshal(g)
	if err != nil {
		return fmt.Errorf("marshaling gaps for %s: %w", runID, err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO gaps (run_id, proto) VALUES (?, ?)`,
		runID, blob); err != nil {
		return fmt.Errorf("recording gaps for %s: %w", runID, err)
	}
	return nil
}

// Gaps loads the gaps record one Export run computed.
//
// Returns ErrGapsNotFound when the run recorded none. The ABSENCE is a
// first-class answer, not an error: a run whose source Value run predates
// the Clusters field has no cluster data, and the report must say exactly
// that — "no cluster data for this run" — rather than guess.
func (s *SQLite) Gaps(ctx context.Context, runID string) (*knov1.Gaps, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	var blob []byte
	err = db.QueryRowContext(ctx,
		`SELECT proto FROM gaps WHERE run_id = ?`, runID).Scan(&blob)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrGapsNotFound, runID)
		}
		return nil, fmt.Errorf("reading gaps for %s: %w", runID, err)
	}
	g := &knov1.Gaps{}
	if err := proto.Unmarshal(blob, g); err != nil {
		return nil, fmt.Errorf("unmarshaling gaps for %s: %w", runID, err)
	}
	return g, nil
}
