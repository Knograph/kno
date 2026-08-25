package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"

	_ "modernc.org/sqlite" // pure-Go driver; see the note on NewSQLite
)

// schemaVersion is the migration level this build expects, recorded in
// SQLite's own `PRAGMA user_version`.
//
// Version 0 is the M1 schema. Every later version is a numbered step in
// migrations below.
const schemaVersion = 2

// schema is the version-0 base, applied on open. It is idempotent.
//
// New columns do NOT go here. They go in a migration step, and a freshly
// created database runs the same steps an existing one does — so the migration
// path is exercised on every open rather than only by databases old enough to
// need it. A migration only old files take is a migration nobody tests.
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
//
// secure_delete makes SQLite ZERO freed content instead of merely unlinking the
// page. Without it, `UPDATE ... SET response_proto = NULL` returns success and
// leaves the bytes in the file's freelist, where `strings kno.db` reads them
// back verbatim. Measured on this store before the pragma was added: 14 of 16
// occurrences of a Case's output survived a purge that reported "Purged 20
// outcome(s)". A deletion feature whose deletion is recoverable is worse than
// no feature, because the user stops worrying.
//
// It costs write throughput on every delete, which is the right trade here:
// this store's writes are dwarfed by the agent calls that produce them.
// busy_timeout is FIRST on purpose. Pragmas are applied in DSN order, and
// journal_mode(WAL) needs an exclusive lock to convert a brand-new database.
// With the timeout set afterwards, two processes opening the same fresh file
// race on that conversion and the loser fails outright with SQLITE_BUSY instead
// of waiting — observed as 2 in 6 concurrent opens failing at connection
// acquisition. Setting the timeout first makes the loser wait for the winner.
const pragmas = "_pragma=busy_timeout(5000)" +
	"&_pragma=journal_mode(WAL)" +
	"&_pragma=foreign_keys(1)" +
	"&_pragma=synchronous(FULL)" +
	"&_pragma=secure_delete(1)"

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

	if err := retryOnBusy(ctx, func() error { return applySchema(ctx, db) }); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrating %s: %w", path, err)
	}
	return &SQLite{db: db}, nil
}

// openBusyRetries bounds how many times an open waits out contention.
//
// With a 5ms base and doubling, the last attempt lands around 160ms — long
// enough for another process to finish creating a database, short enough that a
// genuinely locked file fails while a user is still watching.
const openBusyRetries = 6

// retryOnBusy runs fn, retrying while SQLite reports the database is locked.
//
// busy_timeout does not cover every case. Creating a brand-new database
// converts its journal to WAL, which needs an exclusive lock, and a process
// that loses that race is told the database is locked rather than being made to
// wait — observed as 1-2 of 6 concurrent first-opens failing outright. SQLite's
// own guidance is that SQLITE_BUSY is a condition to handle, not an error to
// surface, and the two `kno` commands a user runs in two terminals should not
// depend on which one reached the file first.
func retryOnBusy(ctx context.Context, fn func() error) error {
	delay := 5 * time.Millisecond
	var err error
	for range openBusyRetries {
		if err = fn(); !isBusy(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
	return err
}

// isBusy reports whether err is SQLite's "database is locked".
func isBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked")
}

// applySchema creates the version-0 tables under the write lock.
//
// The DDL is idempotent, but running it concurrently is not: two processes
// opening the same fresh database race on the schema cookie and one gets
// SQLITE_BUSY, which busy_timeout does not absorb because the loser is not
// waiting on a lock it can acquire — it is being told the schema changed under
// it. Observed as 1 in 6 concurrent opens failing with
// "applying schema: database is locked (5)".
//
// BEGIN IMMEDIATE serializes it the same way migrations are serialized, so the
// second process waits, then finds every CREATE TABLE IF NOT EXISTS satisfied.
func applySchema(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring a connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("taking the write lock: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		}
	}()

	if _, err := conn.ExecContext(ctx, schema); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("committing: %w", err)
	}
	committed = true
	return nil
}

// migration is one numbered schema step.
type migration struct {
	// to is the user_version this step produces.
	to int

	// stmts are applied in order, in one transaction with the version bump.
	stmts []string

	// backfill populates the new columns from data the previous schema
	// already held. Runs inside the same transaction as stmts.
	backfill func(context.Context, *sql.Conn) error
}

// migrations is the ordered list of schema steps.
//
// Adding a step means appending one, never editing an existing one: a step
// that has run somewhere is history, and changing it produces two databases
// claiming the same version with different shapes.
var migrations = []migration{{
	to: 1,
	// M2-1. Every fact a later stage needs about an outcome gets its own
	// column, because the alternative is reading it back out of a protobuf
	// blob — and `kno purge` nulls those blobs. A number that lives only
	// inside purged trace content is a number a privacy-conscious user
	// silently loses.
	//
	// score_value is REAL, unlike every money column in this file. A Score is
	// not money: it is a double in the schema, bounded and never accumulated
	// across thousands of operations, so the int64 discipline that exists to
	// stop cost drift does not apply and would misrepresent the value.
	stmts: []string{
		`ALTER TABLE outcomes ADD COLUMN score_value REAL`,
		`ALTER TABLE outcomes ADD COLUMN score_passed INTEGER`,
		`ALTER TABLE outcomes ADD COLUMN refused INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE outcomes ADD COLUMN truncated INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE outcomes ADD COLUMN usage_estimated INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE outcomes ADD COLUMN provider_build_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE outcomes ADD COLUMN resolved_model TEXT NOT NULL DEFAULT ''`,
	},
	backfill: backfillScoreValues,
}, {
	to: 2,
	// M2: spend the guard settled for a Case that never produced an outcome.
	//
	// On runs rather than as a row in outcomes. RecordOutcome is INSERT OR
	// IGNORE — deliberately, so a resumed run's second attempt cannot silently
	// replace the first result — so a spend-only row in that table would
	// permanently BLOCK the real outcome for its Case: the insert is ignored,
	// every resume re-attempts the Case, pays again, and discards the answer.
	// A second row type would also need a predicate on every query that reads
	// outcomes, and OutcomeCounts is SUM(1 - scored) with none, so an orphan
	// row would count as an errored Case and could stamp a healthy run
	// ErrorRateExceeded.
	//
	// The cost is per-Case attribution, which the event stream already carries
	// — SettlementOvershoot and CaseErrored both name their Case.
	stmts: []string{
		`ALTER TABLE runs ADD COLUMN orphan_cost_usd_micros INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE runs ADD COLUMN orphan_calls INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE runs ADD COLUMN orphan_tokens INTEGER NOT NULL DEFAULT 0`,
	},
}}

// migrate brings the database up to schemaVersion.
//
// Each step runs in its own transaction with its own version bump, so an
// interrupted upgrade leaves the database at the last version that completed
// rather than half-way through one.
func migrate(ctx context.Context, db *sql.DB) error {
	current, err := userVersion(ctx, db)
	if err != nil {
		return err
	}
	if current > schemaVersion {
		// Refuse rather than guess. A newer binary wrote this file; running
		// older code against it would read columns it does not know about and
		// write rows the newer code cannot interpret.
		return fmt.Errorf(
			"database is at schema version %d but this build understands %d; "+
				"upgrade kno, or point --db at a different file",
			current, schemaVersion)
	}

	for _, m := range migrations {
		if m.to <= current {
			continue
		}
		if err := retryOnBusy(ctx, func() error { return applyMigration(ctx, db, m) }); err != nil {
			return fmt.Errorf("migrating to version %d: %w", m.to, err)
		}
	}
	return nil
}

// userVersion reads the schema version.
func userVersion(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
},
) (int, error) {
	var v int
	if err := q.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("reading user_version: %w", err)
	}
	return v, nil
}

// applyMigration runs one step under the write lock, re-checking the version
// inside the transaction.
//
// The re-check is not belt-and-braces; without it two `kno` processes opening
// the same database both read version 0, both try to add the same column, and
// all but one fail to start with `duplicate column name`. Reproduced at 2-3
// failures in 4 concurrent opens. BEGIN IMMEDIATE takes the write lock up
// front, so the second process reads the version the first one committed and
// skips the step instead of repeating it.
//
// The deferred BEGIN that database/sql issues by default is not enough: it
// acquires no lock until the first write, which is after the version has
// already been read.
func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	// One connection for the whole transaction. BEGIN IMMEDIATE has to be
	// issued on the same connection the statements run on, which a pooled
	// *sql.DB does not otherwise promise.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring a connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("taking the write lock: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		}
	}()

	// Re-read under the lock. Another process may have applied this step
	// between our check and our lock.
	current, err := userVersion(ctx, conn)
	if err != nil {
		return err
	}
	if m.to <= current {
		// Already applied by whoever held the lock first. Committing an empty
		// transaction is the cheapest way to release it.
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return fmt.Errorf("releasing the write lock: %w", err)
		}
		committed = true
		return nil
	}

	tx := conn
	for _, stmt := range m.stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	if m.backfill != nil {
		if err := m.backfill(ctx, tx); err != nil {
			return fmt.Errorf("backfilling: %w", err)
		}
	}
	// PRAGMA does not accept a bound parameter, and m.to is an int constant
	// from this file rather than anything a caller supplies.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, m.to)); err != nil {
		return fmt.Errorf("setting user_version: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("committing: %w", err)
	}
	committed = true
	return nil
}

// backfillScoreValues lifts each existing Score's number out of its protobuf
// blob and into the new columns.
//
// Rows whose score_proto is already gone — purged before the upgrade — keep a
// NULL score_value. That NULL is load-bearing: `scored = 1 AND score_value IS
// NULL` is the only way to tell "this Case scored, and the number is
// unrecoverable" from "this Case did not score". Summing NULL as zero would
// produce an aggregate biased toward zero and report it as though it were the
// run's mean.
func backfillScoreValues(ctx context.Context, tx *sql.Conn) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT run_id, case_id, score_proto FROM outcomes
		 WHERE score_proto IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("reading scores: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type scoreRow struct {
		runID, caseID string
		value         float64
		passed        bool
	}
	var pending []scoreRow

	for rows.Next() {
		var r scoreRow
		var blob []byte
		if err := rows.Scan(&r.runID, &r.caseID, &blob); err != nil {
			return fmt.Errorf("scanning score: %w", err)
		}
		var score knov1.Score
		if err := proto.Unmarshal(blob, &score); err != nil {
			// A blob this build cannot parse is not a reason to refuse the
			// upgrade: the row keeps a NULL score_value and is reported as
			// unrecoverable, which is the same honest outcome as a purge.
			continue
		}
		r.value, r.passed = score.GetValue(), score.GetPassed()
		pending = append(pending, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading scores: %w", err)
	}

	for _, r := range pending {
		if _, err := tx.ExecContext(ctx,
			`UPDATE outcomes SET score_value = ?, score_passed = ?
			 WHERE run_id = ? AND case_id = ?`,
			r.value, boolToInt(r.passed), r.runID, r.caseID); err != nil {
			return fmt.Errorf("updating %s/%s: %w", r.runID, r.caseID, err)
		}
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
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

	// Columns, not blob fields, for everything a later stage reads back. The
	// blobs are trace content and `kno purge` nulls them; a number that lives
	// only inside one is a number a privacy-conscious user silently loses.
	var scoreValue, scorePassed any // NULL when the Case errored
	if out.Score != nil {
		scoreValue, scorePassed = out.Score.GetValue(), boolToInt(out.Score.GetPassed())
	}
	r := out.Response
	truncated := r.GetStopReason() == knov1.StopReason_STOP_REASON_LENGTH

	// INSERT OR IGNORE, not INSERT OR REPLACE. A Case that already has an
	// outcome keeps the one it has: the money for it is already spent and
	// already counted, and overwriting would let a resumed run's second
	// attempt silently replace the first result.
	_, err = db.ExecContext(ctx,
		`INSERT OR IGNORE INTO outcomes
		   (run_id, case_id, scored, err_code, response_proto, score_proto,
		    calls, cost_usd_micros, tokens,
		    score_value, score_passed, refused, truncated, usage_estimated,
		    provider_build_id, resolved_model)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, out.CaseID, scored, out.Err, responseBlob, scoreBlob,
		out.Spend.Calls, out.Spend.CostUSDMicros, out.Spend.Tokens,
		scoreValue, scorePassed,
		boolToInt(r.GetRefused()), boolToInt(truncated), boolToInt(r.GetUsageEstimated()),
		r.GetProviderBuildId(), r.GetResolvedModel())
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
	// Both sources, in one statement. Money the guard settled for a Case that
	// never produced an outcome lives on the run — see migration 2 — and a
	// resume that read only the outcomes would get it back as headroom and
	// spend it again.
	//
	// COALESCE because SUM over zero rows is NULL, and a fresh run legitimately
	// has none — that must read as zero spent, not as a scan error.
	err = db.QueryRowContext(ctx,
		`SELECT COALESCE(o.calls, 0)  + r.orphan_calls,
		        COALESCE(o.cost, 0)   + r.orphan_cost_usd_micros,
		        COALESCE(o.tokens, 0) + r.orphan_tokens
		 FROM runs r
		 LEFT JOIN (
		     SELECT run_id,
		            SUM(calls) AS calls,
		            SUM(cost_usd_micros) AS cost,
		            SUM(tokens) AS tokens
		     FROM outcomes WHERE run_id = ?
		 ) o ON o.run_id = r.id
		 WHERE r.id = ?`, runID, runID).
		Scan(&spend.Calls, &spend.CostUSDMicros, &spend.Tokens)
	if errors.Is(err, sql.ErrNoRows) {
		// No run row yet. A fresh run has spent nothing, which is what the
		// caller needs — Guard.Restore is a no-op against zero.
		return budget.Spend{}, nil
	}
	if err != nil {
		return budget.Spend{}, fmt.Errorf("summing spend for %s: %w", runID, err)
	}
	return spend, nil
}

// RecordOrphanSpend adds spend the guard settled for a Case that produced no
// outcome.
//
// Additive, and separate from RecordOutcome, because the two answer different
// questions: RecordOutcome says a Case is DONE, and this says money was spent
// on one that is not. A Case with orphan spend stays absent from
// CompletedCases and is re-attempted on resume; its earlier charge is already
// in SettledSpend and is not spent twice.
//
// Not a row in outcomes: RecordOutcome is INSERT OR IGNORE, so a spend-only
// row would permanently block the real outcome for that Case.
func (s *SQLite) RecordOrphanSpend(ctx context.Context, runID string, spend budget.Spend) error {
	db, err := s.conn()
	if err != nil {
		return err
	}
	return retryOnBusy(ctx, func() error {
		res, err := db.ExecContext(ctx,
			`UPDATE runs
			 SET orphan_calls           = orphan_calls + ?,
			     orphan_cost_usd_micros = orphan_cost_usd_micros + ?,
			     orphan_tokens          = orphan_tokens + ?
			 WHERE id = ?`,
			spend.Calls, spend.CostUSDMicros, spend.Tokens, runID)
		if err != nil {
			return fmt.Errorf("recording orphan spend for %s: %w", runID, err)
		}
		// A missing run is a caller bug, not a no-op. Silently dropping the
		// spend is the failure this method exists to prevent.
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("recording orphan spend for %s: %w", runID, err)
		}
		if n == 0 {
			return fmt.Errorf("recording orphan spend: run %s does not exist", runID)
		}
		return nil
	})
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

// Purge removes trace content from a run while leaving the run resumable.
//
// It NULLs the protobuf blobs and never deletes a row. That distinction is the
// whole design: the outcomes table IS the done-marker this store uses to skip
// finished work, so a purge that deleted rows would reopen the double-spend it
// exists to prevent — a purged run, resumed, would pay for every Case a second
// time. See docs/debt.md#25.
//
// What survives: which Cases completed, what they cost, whether they scored,
// and the numeric score itself, all of which live in columns. What goes: the
// agent's output and the judge's rationale, which are the parts that can
// contain end-user conversation content.
//
// A purged Case keeps `scored = 1` with a NULL `score_value` only if it was
// purged before the score column existed. Purging today preserves the number.
func (s *SQLite) Purge(ctx context.Context, runID string) (int64, error) {
	db, err := s.conn()
	if err != nil {
		return 0, err
	}
	res, err := db.ExecContext(ctx,
		`UPDATE outcomes SET response_proto = NULL, score_proto = NULL
		 WHERE run_id = ? AND (response_proto IS NOT NULL OR score_proto IS NOT NULL)`,
		runID)
	if err != nil {
		return 0, fmt.Errorf("purging traces for %s: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting purged rows for %s: %w", runID, err)
	}

	// Clearing the column is not deleting the data, and this is the one
	// operation where that distinction is the whole point.
	//
	// The UPDATE lives in the write-ahead log until a checkpoint folds it into
	// the database file, and pages freed before secure_delete was enabled --
	// every row any earlier build wrote -- still hold their original bytes.
	// Both are readable with `strings`. So: checkpoint the WAL, then VACUUM,
	// which rewrites the file from live content only.
	//
	// VACUUM cannot run inside a transaction and rewrites the whole database.
	// That is acceptable precisely here: purge is rare, explicit, and
	// irreversible, and a user who asked for deletion has priced in the wait.
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return n, fmt.Errorf("checkpointing after purge of %s: %w", runID, err)
	}
	if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
		return n, fmt.Errorf("reclaiming purged pages for %s: %w", runID, err)
	}
	return n, nil
}

// ScoreSum reports the sum of recorded scores for a run, and how many Cases
// scored but can no longer contribute a number.
//
// Two counts rather than one because they must not be conflated. A Case whose
// score_value is NULL while scored = 1 was purged before the column existed:
// its number is gone. Summing it as zero would drag the mean toward zero and
// present the result as the run's actual aggregate — which is worse than
// reporting nothing, and is why the caller is handed `unrecoverable` rather
// than a single pre-mixed average.
func (s *SQLite) ScoreSum(ctx context.Context, runID string) (sum float64, counted, unrecoverable int, err error) {
	db, err := s.conn()
	if err != nil {
		return 0, 0, 0, err
	}
	err = db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(score_value), 0),
		        COALESCE(SUM(score_value IS NOT NULL), 0),
		        COALESCE(SUM(scored = 1 AND score_value IS NULL), 0)
		 FROM outcomes WHERE run_id = ?`, runID).Scan(&sum, &counted, &unrecoverable)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("summing scores for %s: %w", runID, err)
	}
	return sum, counted, unrecoverable, nil
}

// PurgeableCount reports how many outcomes still hold trace content.
//
// The confirmation prompt needs it: "this would remove content from 44
// outcomes" is a dry run, while "this would remove content" is an assertion the
// user cannot check before agreeing to something irreversible.
func (s *SQLite) PurgeableCount(ctx context.Context, runID string) (int, error) {
	db, err := s.conn()
	if err != nil {
		return 0, err
	}
	var n int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM outcomes
		 WHERE run_id = ? AND (response_proto IS NOT NULL OR score_proto IS NOT NULL)`,
		runID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting purgeable outcomes for %s: %w", runID, err)
	}
	return n, nil
}
