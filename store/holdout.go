package store

import (
	"context"
	"errors"
	"fmt"
)

// HoldoutUse is the durable record that a holdout was opened.
//
// It exists because the holdout is consumable exactly once per Portfolio and
// nothing else in the system knows that. docs/mental-model.md already promises
// the user "the Portfolio meets it exactly once, at Validate"; this table is
// what turns that sentence from prose into a refusal.
//
// Recorded at the moment a Validate run STARTS, before its first agent call —
// never at completion. A validate that crashed half way through has already
// seen part of the holdout, and a record written at completion would let that
// run look like it never looked. See core.Validate.
type HoldoutUse struct {
	// EvalFingerprint identifies the holdout: the eval source's content hash
	// combined with the split configuration that divided it. Two runs over the
	// same fingerprint met the same Cases.
	EvalFingerprint string

	// SelectRunID is the Portfolio that met the holdout. The key is keyed on
	// this and NOT on the agent: a post-tune `validate --agent tuned:<ref>` is
	// deliberately a second, disclosed use of the same holdout rather than a
	// silently-permitted repeat.
	SelectRunID string

	// ValidateRunID is the run that did the looking.
	ValidateRunID string

	// CreatedAt is RFC 3339.
	CreatedAt string
}

// RecordHoldoutUse durably records that a Portfolio has met a holdout.
//
// Idempotent on (eval_fingerprint, select_run_id): re-recording the same pair
// is a no-op rather than a second row, so a resumed Validate does not count as
// a second peek. That is the whole reason the key excludes the validate run —
// a resume continues one look, it does not take another.
func (s *SQLite) RecordHoldoutUse(ctx context.Context, use *HoldoutUse) error {
	db, err := s.conn()
	if err != nil {
		return err
	}
	switch {
	case use == nil:
		return errors.New("store: there is no holdout use to record")
	case use.EvalFingerprint == "":
		return errors.New("store: a holdout use needs an eval fingerprint")
	case use.SelectRunID == "":
		return errors.New("store: a holdout use needs a select run ID")
	case use.ValidateRunID == "":
		return errors.New("store: a holdout use needs a validate run ID")
	}
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO holdout_uses
		 (eval_fingerprint, select_run_id, validate_run_id, created_at)
		 VALUES (?, ?, ?, ?)`,
		use.EvalFingerprint, use.SelectRunID, use.ValidateRunID, use.CreatedAt); err != nil {
		return fmt.Errorf("recording the holdout use for %s: %w", use.ValidateRunID, err)
	}
	return nil
}

// HoldoutUses returns every recorded use of one holdout, oldest first.
//
// Ordered by created_at then by select run ID, so the use index a report
// prints is stable across processes rather than depending on row order.
func (s *SQLite) HoldoutUses(ctx context.Context, evalFingerprint string) ([]HoldoutUse, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT eval_fingerprint, select_run_id, validate_run_id, created_at
		   FROM holdout_uses WHERE eval_fingerprint = ?
		  ORDER BY created_at, select_run_id`, evalFingerprint)
	if err != nil {
		return nil, fmt.Errorf("reading holdout uses: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []HoldoutUse
	for rows.Next() {
		var u HoldoutUse
		if err := rows.Scan(&u.EvalFingerprint, &u.SelectRunID, &u.ValidateRunID, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning a holdout use: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading holdout uses: %w", err)
	}
	return out, nil
}
