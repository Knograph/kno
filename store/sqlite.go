package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

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

// pragmas are applied through the DSN, NOT by executing them once after open.
//
// foreign_keys and busy_timeout are per-CONNECTION settings. Setting them with
// a single Exec configures only whichever connection happened to serve it, and
// database/sql silently replaces connections: modernc returns driver.ErrBadConn
// for any connection whose last statement was interrupted, which is what an
// ordinary context cancellation does. The replacement connection then has
// foreign_keys=0 and busy_timeout=0.
//
// That is not hypothetical. Reproduced against this driver by cancelling a
// long-running query and reusing the pool:
//
//	[before cancel]  foreign_keys=1 busy_timeout=5000
//	[after cancel]   foreign_keys=0 busy_timeout=0
//	orphan outcome insert -> err=<nil> rowsAffected=1
//
// An outcome for a nonexistent run — which the foreign key exists to reject —
// silently succeeded. And busy_timeout=0 turns write contention into an
// immediate SQLITE_BUSY instead of a retry, dropping an outcome whose money is
// already spent, which is exactly the double-spend this store prevents.
//
// In the DSN they are reapplied on every connection the pool opens.
// synchronous=FULL is pinned explicitly rather than inherited, so a durability
// guarantee this store depends on cannot change under it.
const pragmas = "_pragma=journal_mode(WAL)" +
	"&_pragma=foreign_keys(1)" +
	"&_pragma=busy_timeout(5000)" +
	"&_pragma=synchronous(FULL)"

// maxConns bounds the pool. Reads scale under WAL; writes serialize inside
// SQLite regardless, so a larger pool buys nothing but memory.
const maxConns = 8

// SQLite is a Store backed by a local SQLite database.
//
// Safe for concurrent use by multiple goroutines, except that Close must not
// race with an in-flight call.
//
// Concurrency is SQLite's own: WAL allows one writer alongside many readers,
// and busy_timeout absorbs writer contention. There is deliberately no writer
// goroutine — an earlier draft of the plan described one, but funnelling reads
// through the same serialization point is what makes a long CompletedCases
// scan block every worker's RecordOutcome. Under WAL, readers never block the
// writer to begin with.
//
// None of this appears in the Store interface, so another backend is free to
// do something else.
type SQLite struct {
	mu sync.RWMutex
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
	dsn := "file:" + path + "?" + pragmas
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	// Multiple connections are correct under WAL: many readers proceed
	// alongside one writer, and busy_timeout absorbs write contention. Capping
	// at one would serialize a long read ahead of every pending write.
	db.SetMaxOpenConns(maxConns)

	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}
	return &SQLite{db: db}, nil
}

// CreateRun records a new run.
func (s *SQLite) CreateRun(ctx context.Context, run *knov1.Run) error {
	db, err := s.conn()
	if err != nil {
		return err
	}

	blob, err := proto.Marshal(run)
	if err != nil {
		return fmt.Errorf("marshaling run: %w", err)
	}

	_, err = db.ExecContext(ctx,
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
	db, err := s.conn()
	if err != nil {
		return nil, err
	}

	var blob []byte
	err = db.QueryRowContext(ctx, `SELECT proto FROM runs WHERE id = ?`, runID).Scan(&blob)
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
	db, err := s.conn()
	if err != nil {
		return err
	}

	blob, err := proto.Marshal(run)
	if err != nil {
		return fmt.Errorf("marshaling run: %w", err)
	}

	res, err := db.ExecContext(ctx,
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
	db, err := s.conn()
	if err != nil {
		return err
	}
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
	_, err = db.ExecContext(ctx,
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
	db, err := s.conn()
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx, `SELECT case_id FROM outcomes WHERE run_id = ?`, runID)
	if err != nil {
		return nil, fmt.Errorf("listing completed cases for %s: %w", runID, err)
	}
	// DEBT(docs/debt.md#24): rows.Close on a read-only cursor returns only
	// errors already surfaced by rows.Err below, which is checked.
	defer func() { _ = rows.Close() }()

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

// OutcomeCounts reports how many Cases a run scored and how many errored.
func (s *SQLite) OutcomeCounts(ctx context.Context, runID string) (scored, errored int, err error) {
	db, err := s.conn()
	if err != nil {
		return 0, 0, err
	}

	// COALESCE for the same reason SettledSpend needs it: a run with no
	// outcomes yet must read as zero rather than failing the scan.
	err = db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(scored), 0), COALESCE(SUM(1 - scored), 0)
		 FROM outcomes WHERE run_id = ?`, runID).Scan(&scored, &errored)
	if err != nil {
		return 0, 0, fmt.Errorf("counting outcomes for %s: %w", runID, err)
	}
	return scored, errored, nil
}

// SettledSpend sums what a run actually spent.
func (s *SQLite) SettledSpend(ctx context.Context, runID string) (budget.Spend, error) {
	db, err := s.conn()
	if err != nil {
		return budget.Spend{}, err
	}

	var spend budget.Spend
	// COALESCE because SUM over zero rows is NULL, and a fresh run legitimately
	// has none — that must read as zero spent, not as a scan error.
	err = db.QueryRowContext(ctx,
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
	db, err := s.conn()
	if err != nil {
		return err
	}
	if ev.GetSequence() <= 0 {
		return fmt.Errorf("store: event for run %s has no sequence; the caller owns ordering", ev.GetRunId())
	}
	blob, err := proto.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshaling event: %w", err)
	}

	_, err = db.ExecContext(ctx,
		`INSERT OR IGNORE INTO events (run_id, sequence, proto) VALUES (?, ?, ?)`,
		ev.GetRunId(), ev.GetSequence(), blob)
	if err != nil {
		return fmt.Errorf("appending event %d for run %s: %w", ev.GetSequence(), ev.GetRunId(), err)
	}
	return nil
}

// MaxEventSequence returns the highest sequence recorded for a run.
func (s *SQLite) MaxEventSequence(ctx context.Context, runID string) (int64, error) {
	db, err := s.conn()
	if err != nil {
		return 0, err
	}

	var maxSeq int64
	err = db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(sequence), 0) FROM events WHERE run_id = ?`, runID).Scan(&maxSeq)
	if err != nil {
		return 0, fmt.Errorf("reading max sequence for %s: %w", runID, err)
	}
	return maxSeq, nil
}

// Close releases the database handle. Safe to call more than once.
//
// The write lock is what makes this safe against a concurrent caller: an
// earlier version read and nil'd s.db without synchronization, which the race
// detector flags the moment a query overlaps a shutdown — precisely the
// drain-then-close sequence the executor performs.
func (s *SQLite) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

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

// conn returns the handle under a read lock, so callers cannot observe a
// half-closed store.
func (s *SQLite) conn() (*sql.DB, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.db == nil {
		return nil, errors.New("store: closed")
	}
	return s.db, nil
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
