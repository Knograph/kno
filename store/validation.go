package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"google.golang.org/protobuf/proto"
)

// WriteValidation records the Validation one Validate run produced.
//
// One row per run, INSERT OR REPLACE, modelled on WritePortfolio: the
// Validation is DERIVED from the run's recorded measurements, so a
// recomputation after a resume must produce the row matching the current
// numbers rather than pinning the first, partial answer.
//
// Written atomically at the end of the stage. There is no per-Case progression
// to checkpoint here — the measurements table is the checkpoint — and a
// half-written Validation would read as a real verdict to every consumer that
// loads one.
func (s *SQLite) WriteValidation(ctx context.Context, runID string, v *knov1.Validation) error {
	db, err := s.conn()
	if err != nil {
		return err
	}
	if v.GetRunId() == "" {
		return errors.New("store: validation needs a run ID")
	}
	blob, err := proto.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshaling validation for %s: %w", runID, err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO validations (run_id, proto) VALUES (?, ?)`,
		runID, blob); err != nil {
		return fmt.Errorf("recording validation for %s: %w", runID, err)
	}
	return nil
}

// Validation loads a run's Validation.
//
// Returns ErrValidationNotFound when the run recorded none, which is different
// from a Validation with no gain: "Validate never finished on this run" is not
// "Validate ran and could not form an interval", and the report renders them
// differently — one keeps the not-yet-validated caveat, the other replaces it
// with a stated reason.
func (s *SQLite) Validation(ctx context.Context, runID string) (*knov1.Validation, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	var blob []byte
	err = db.QueryRowContext(ctx,
		`SELECT proto FROM validations WHERE run_id = ?`, runID).Scan(&blob)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrValidationNotFound, runID)
		}
		return nil, fmt.Errorf("reading validation for %s: %w", runID, err)
	}
	v := &knov1.Validation{}
	if err := proto.Unmarshal(blob, v); err != nil {
		return nil, fmt.Errorf("unmarshaling validation for %s: %w", runID, err)
	}
	return v, nil
}
