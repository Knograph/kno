package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"google.golang.org/protobuf/proto"
)

// WritePortfolio records the Portfolio one Select run produced.
//
// One row per run, INSERT OR REPLACE: the Portfolio is DERIVED from the
// run's recorded Valuations rather than being itself a record of spend, so a
// recomputation after a resume must produce the row that matches the current
// decision — keeping the first write would pin the store to a stale answer.
//
// The Portfolio is written atomically at the end of the stage, not
// incrementally. Select has no per-Asset progression to checkpoint, and a
// half-written Portfolio would read as a real decision by every consumer
// that loads one.
func (s *SQLite) WritePortfolio(ctx context.Context, runID string, p *knov1.Portfolio) error {
	db, err := s.conn()
	if err != nil {
		return err
	}
	if p.GetRunId() == "" {
		return errors.New("store: portfolio needs a run ID")
	}
	blob, err := proto.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshaling portfolio for %s: %w", runID, err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO portfolios (run_id, proto) VALUES (?, ?)`,
		runID, blob); err != nil {
		return fmt.Errorf("recording portfolio for %s: %w", runID, err)
	}
	return nil
}

// Portfolio loads a run's Portfolio.
//
// Returns ErrPortfolioNotFound when the run recorded none, which is
// different from an empty Portfolio: "Select never ran on this run" is not
// "Select ran and included nothing new", and the two must not read alike.
func (s *SQLite) Portfolio(ctx context.Context, runID string) (*knov1.Portfolio, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	var blob []byte
	err = db.QueryRowContext(ctx,
		`SELECT proto FROM portfolios WHERE run_id = ?`, runID).Scan(&blob)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrPortfolioNotFound, runID)
		}
		return nil, fmt.Errorf("reading portfolio for %s: %w", runID, err)
	}
	p := &knov1.Portfolio{}
	if err := proto.Unmarshal(blob, p); err != nil {
		return nil, fmt.Errorf("unmarshaling portfolio for %s: %w", runID, err)
	}
	return p, nil
}
