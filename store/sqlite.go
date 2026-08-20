package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"

	_ "modernc.org/sqlite" // pure-Go driver; see the note on NewSQLite
)

// schema is applied on open. It is idempotent, so opening an existing database
// is a no-op rather than a migration.
//
// Money is INTEGER micro-USD throughout, matching the proto. SQLite has no
// decimal type, and REAL here would reintroduce exactly the float drift the
// int64 discipline exists to avoid.
const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS runs (
    id                 TEXT PRIMARY KEY,
    proto              BLOB NOT NULL,
    stage              INTEGER NOT NULL,
    status             INTEGER NOT NULL,
    input_fingerprint  TEXT NOT NULL,
    created_at         TEXT NOT NULL
);

-- One row per Case that reached a terminal outcome. This table IS the
-- done-marker: there is no separate checkpoint row that could disagree with it
-- after a crash.
CREATE TABLE IF NOT EXISTS outcomes (
    run_id           TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    case_id          TEXT NOT NULL,
    scored           INTEGER NOT NULL,
    err_code         TEXT NOT NULL DEFAULT '',
    response_proto   BLOB,
    score_proto      BLOB,
    calls            INTEGER NOT NULL DEFAULT 0,
    cost_usd_micros  INTEGER NOT NULL DEFAULT 0,
    tokens           INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (run_id, case_id)
);

CREATE TABLE IF NOT EXISTS events (
    run_id    TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    sequence  INTEGER NOT NULL,
    proto     BLOB NOT NULL,
    PRIMARY KEY (run_id, sequence)
);
`

// SQLite is a Store backed by a local SQLite database.
//
// Concurrency is handled by SQLite itself: WAL mode plus a busy timeout, with
// every write in its own transaction. Callers may use it from multiple
// goroutines. Nothing about that arrangement appears in the Store interface,
// so a different backend is free to do something else entirely.
type SQLite struct {
	db *sql.DB
}

// NewSQLite opens or creates a database at path.
//
// The driver is modernc.org/sqlite, which is pure Go. The faster cgo driver
// would break CGO_ENABLED=0 and the cross-compiled release matrix DESIGN.md
// commits to as a single static binary, and that promise outweighs write
// throughput here: a commit costs single-digit milliseconds against agent calls
// that take on the order of a second.
func NewSQLite(ctx context.Context, path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	// One connection. SQLite permits a single writer, and letting the pool
	// open several would produce SQLITE_BUSY under exactly the concurrent
	// writes this store exists to absorb.
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}
	return &SQLite{db: db}, nil
}

// CreateRun records a new run.
func (s *SQLite) CreateRun(ctx context.Context, run *knov1.Run) error {
	blob, err := proto.Marshal(run)
	if err != nil {
		return fmt.Errorf("marshaling run: %w", err)
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO runs (id, proto, stage, status, input_fingerprint, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		run.GetId(), blob, int32(run.GetStage()), int32(run.GetStatus()),
		run.GetInputFingerprint(), run.GetCreatedAt())
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("creating run %s: %w", run.GetId(), ErrRunExists)
		}
		return fmt.Errorf("creating run %s: %w", run.GetId(), err)
	}
	return nil
}

// GetRun loads a run by ID.
func (s *SQLite) GetRun(ctx context.Context, runID string) (*knov1.Run, error) {
	var blob []byte
	err := s.db.QueryRowContext(ctx, `SELECT proto FROM runs WHERE id = ?`, runID).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("loading run %s: %w", runID, ErrRunNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("loading run %s: %w", runID, err)
	}

	var run knov1.Run
	if err := proto.Unmarshal(blob, &run); err != nil {
		return nil, fmt.Errorf("unmarshaling run %s: %w", runID, err)
	}
	return &run, nil
}

// FinishRun records how a run ended.
func (s *SQLite) FinishRun(ctx context.Context, run *knov1.Run) error {
	blob, err := proto.Marshal(run)
	if err != nil {
		return fmt.Errorf("marshaling run: %w", err)
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET proto = ?, status = ? WHERE id = ?`,
		blob, int32(run.GetStatus()), run.GetId())
	if err != nil {
		return fmt.Errorf("finishing run %s: %w", run.GetId(), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("finishing run %s: %w", run.GetId(), err)
	}
	if n == 0 {
		return fmt.Errorf("finishing run %s: %w", run.GetId(), ErrRunNotFound)
	}
	return nil
}

// RecordOutcome durably records one Case's terminal outcome in one
// transaction.
func (s *SQLite) RecordOutcome(ctx context.Context, runID string, out *Outcome) error {
	if out == nil || out.CaseID == "" {
		return errors.New("store: outcome needs a case ID")
	}
	// Check the two fields directly rather than via Scored(): Scored() already
	// returns false when Err is set, so comparing against it would let an
	// outcome carrying BOTH a Score and an error slip through — which is
	// exactly the ambiguity this guard exists to reject.
	hasScore, hasErr := out.Score != nil, out.Err != ""
	if hasScore == hasErr {
		return fmt.Errorf("store: case %s must be either scored or errored, not both or neither", out.CaseID)
	}

	var responseBlob, scoreBlob []byte
	var err error
	if out.Response != nil {
		if responseBlob, err = proto.Marshal(out.Response); err != nil {
			return fmt.Errorf("marshaling response for %s: %w", out.CaseID, err)
		}
	}
	if out.Score != nil {
		if scoreBlob, err = proto.Marshal(out.Score); err != nil {
			return fmt.Errorf("marshaling score for %s: %w", out.CaseID, err)
		}
	}

	scored := 0
	if out.Scored() {
		scored = 1
	}

	// INSERT OR IGNORE, not INSERT OR REPLACE. A Case that already has an
	// outcome keeps the one it has: the money for it is already spent and
	// already counted, and overwriting would let a resumed run's second
	// attempt silently replace the first result.
	_, err = s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO outcomes
		   (run_id, case_id, scored, err_code, response_proto, score_proto,
		    calls, cost_usd_micros, tokens)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, out.CaseID, scored, out.Err, responseBlob, scoreBlob,
		out.Spend.Calls, out.Spend.CostUSDMicros, out.Spend.Tokens)
	if err != nil {
		return fmt.Errorf("recording outcome for %s: %w", out.CaseID, err)
	}
	return nil
}

// CompletedCases returns the IDs of every Case with a terminal outcome.
func (s *SQLite) CompletedCases(ctx context.Context, runID string) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT case_id FROM outcomes WHERE run_id = ?`, runID)
	if err != nil {
		return nil, fmt.Errorf("listing completed cases for %s: %w", runID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; rows.Err below reports real failures

	out := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning completed case: %w", err)
		}
		out[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing completed cases for %s: %w", runID, err)
	}
	return out, nil
}

// SettledSpend sums what a run actually spent.
func (s *SQLite) SettledSpend(ctx context.Context, runID string) (budget.Spend, error) {
	var spend budget.Spend
	// COALESCE because SUM over zero rows is NULL, and a fresh run legitimately
	// has none — that must read as zero spent, not as a scan error.
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(calls), 0), COALESCE(SUM(cost_usd_micros), 0), COALESCE(SUM(tokens), 0)
		 FROM outcomes WHERE run_id = ?`, runID).
		Scan(&spend.Calls, &spend.CostUSDMicros, &spend.Tokens)
	if err != nil {
		return budget.Spend{}, fmt.Errorf("summing spend for %s: %w", runID, err)
	}
	return spend, nil
}

// AppendEvent records one event.
func (s *SQLite) AppendEvent(ctx context.Context, ev *knov1.Event) error {
	if ev.GetSequence() <= 0 {
		return fmt.Errorf("store: event for run %s has no sequence; the caller owns ordering", ev.GetRunId())
	}
	blob, err := proto.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshaling event: %w", err)
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO events (run_id, sequence, proto) VALUES (?, ?, ?)`,
		ev.GetRunId(), ev.GetSequence(), blob)
	if err != nil {
		return fmt.Errorf("appending event %d for run %s: %w", ev.GetSequence(), ev.GetRunId(), err)
	}
	return nil
}

// MaxEventSequence returns the highest sequence recorded for a run.
func (s *SQLite) MaxEventSequence(ctx context.Context, runID string) (int64, error) {
	var maxSeq int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(sequence), 0) FROM events WHERE run_id = ?`, runID).Scan(&maxSeq)
	if err != nil {
		return 0, fmt.Errorf("reading max sequence for %s: %w", runID, err)
	}
	return maxSeq, nil
}

// Close releases the database handle.
func (s *SQLite) Close() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	if err != nil {
		return fmt.Errorf("closing store: %w", err)
	}
	return nil
}

// isUniqueViolation reports whether err is a primary-key conflict.
//
// Matched on the driver's message rather than a typed code: modernc's error
// type is not part of its stable API, and a string match that degrades to
// "some other error" is safer than a type assertion that panics.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE")
}

// Compile-time proof that SQLite satisfies the interface.
var _ Store = (*SQLite)(nil)
