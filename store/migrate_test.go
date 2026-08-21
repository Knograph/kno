package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"google.golang.org/protobuf/proto"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// m1Schema is the version-0 schema, verbatim, as it shipped in M1.
//
// Pinned here rather than referenced from the package, because the point is to
// build a database as an M1 binary left it. If this were derived from the
// current schema constant the test would migrate a database that already had
// the new columns and prove nothing — which is exactly how a migration ships
// broken.
const m1Schema = `
CREATE TABLE IF NOT EXISTS runs (
    id                 TEXT PRIMARY KEY,
    proto              BLOB NOT NULL,
    stage              INTEGER NOT NULL,
    status             INTEGER NOT NULL,
    input_fingerprint  TEXT NOT NULL,
    created_at         TEXT NOT NULL
);
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

// writeM1Database creates a database exactly as an M1 binary would have left
// it: version-0 schema, one run, and n scored outcomes whose only record of a
// Score is the protobuf blob.
func writeM1Database(t *testing.T, n int, purgeFirst bool) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "kno.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("opening raw database: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	if _, err := db.ExecContext(ctx, m1Schema); err != nil {
		t.Fatalf("applying the M1 schema: %v", err)
	}

	runBlob, err := proto.Marshal(newRun("run-1"))
	if err != nil {
		t.Fatalf("marshaling run: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO runs (id, proto, stage, status, input_fingerprint, created_at)
		 VALUES ('run-1', ?, 1, 1, 'fp-1', '2026-08-20T00:00:00Z')`, runBlob); err != nil {
		t.Fatalf("inserting run: %v", err)
	}

	for i := range n {
		caseID := caseName(i)
		scoreBlob, err := proto.Marshal(&knov1.Score{
			CaseId: caseID, Value: float64(i), Passed: true,
		})
		if err != nil {
			t.Fatalf("marshaling score: %v", err)
		}
		// A purged-before-upgrade row: the blob is already gone, so its number
		// is unrecoverable and must not be silently counted as zero.
		if purgeFirst && i == 0 {
			scoreBlob = nil
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO outcomes
			   (run_id, case_id, scored, err_code, response_proto, score_proto,
			    calls, cost_usd_micros, tokens)
			 VALUES ('run-1', ?, 1, '', ?, ?, 1, 1000, 42)`,
			caseID, []byte("response"), scoreBlob); err != nil {
			t.Fatalf("inserting outcome: %v", err)
		}
	}
	return path
}

func caseName(i int) string { return string(rune('a'+i)) + "-case" }

// TestOpeningAnM1DatabaseMigratesIt.
//
// Without this, the first paid run against any existing kno.db would spend
// real money on its first Case and THEN fail on a missing column, because the
// schema is CREATE TABLE IF NOT EXISTS and an existing table is skipped
// entirely. Money gone, no rows recorded, no resumability, and every retry
// repeating it. Phase-1 pass three caught the plan shipping without this.
func TestOpeningAnM1DatabaseMigratesIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	path := writeM1Database(t, 3, false)

	s, err := store.NewSQLite(ctx, path)
	if err != nil {
		t.Fatalf("opening an M1-era database: %v", err)
	}
	defer func() { _ = s.Close() }()

	// The thing that would have failed: a write naming the new columns.
	if err := s.RecordOutcome(ctx, "run-1", scoredOutcome("new-case", 1, 500)); err != nil {
		t.Fatalf("recording an outcome after migration: %v", err)
	}

	// And the prior work is still there, unmodified.
	done, err := s.CompletedCases(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}
	if len(done) != 4 {
		t.Errorf("got %d completed Cases, want 4 (3 migrated + 1 new)", len(done))
	}
}

// TestMigrationBackfillsScoresFromBlobs: the numbers an M1 database recorded
// must survive into the column, or M2-9's aggregate silently covers only Cases
// scored after the upgrade.
func TestMigrationBackfillsScoresFromBlobs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	path := writeM1Database(t, 4, false) // values 0, 1, 2, 3
	s, err := store.NewSQLite(ctx, path)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	defer func() { _ = s.Close() }()

	sum, counted, unrecoverable, err := s.ScoreSum(ctx, "run-1")
	if err != nil {
		t.Fatalf("ScoreSum: %v", err)
	}
	if want := 0.0 + 1 + 2 + 3; sum != want {
		t.Errorf("sum = %v, want %v; the pre-upgrade scores were not backfilled", sum, want)
	}
	if counted != 4 {
		t.Errorf("counted = %d, want 4", counted)
	}
	if unrecoverable != 0 {
		t.Errorf("unrecoverable = %d, want 0", unrecoverable)
	}
}

// TestScorePurgedBeforeUpgradeIsUnrecoverableNotZero.
//
// A run purged under M1 has no blob to backfill from. Counting that Case's
// score as zero would drag the mean toward zero and report it as the run's
// actual aggregate — worse than reporting nothing, because it looks like a
// measurement.
func TestScorePurgedBeforeUpgradeIsUnrecoverableNotZero(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	path := writeM1Database(t, 4, true) // case 0's blob is already gone
	s, err := store.NewSQLite(ctx, path)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	defer func() { _ = s.Close() }()

	sum, counted, unrecoverable, err := s.ScoreSum(ctx, "run-1")
	if err != nil {
		t.Fatalf("ScoreSum: %v", err)
	}
	if unrecoverable != 1 {
		t.Fatalf("unrecoverable = %d, want 1; a Case whose score cannot be "+
			"recovered must be reported, not averaged in as zero", unrecoverable)
	}
	if counted != 3 {
		t.Errorf("counted = %d, want 3", counted)
	}
	if want := 1.0 + 2 + 3; sum != want {
		t.Errorf("sum = %v, want %v", sum, want)
	}
}

// TestMigrationIsIdempotent: reopening does not re-run a completed step.
func TestMigrationIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	path := writeM1Database(t, 2, false)
	for i := range 3 {
		s, err := store.NewSQLite(ctx, path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}

	s, err := store.NewSQLite(ctx, path)
	if err != nil {
		t.Fatalf("final open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, counted, _, err := s.ScoreSum(ctx, "run-1"); err != nil || counted != 2 {
		t.Errorf("counted = %d (err %v), want 2; repeated opens changed the data", counted, err)
	}
}

// TestFutureSchemaVersionIsRefused: an older binary must not read a database a
// newer one wrote. It would misread columns it does not know about and write
// rows the newer code cannot interpret.
func TestFutureSchemaVersionIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "kno.db")
	s, err := store.NewSQLite(ctx, path)
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("reopening raw: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA user_version = 9999`); err != nil {
		t.Fatalf("bumping version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing raw: %v", err)
	}

	if _, err := store.NewSQLite(ctx, path); err == nil {
		t.Fatal("a database from a newer build was opened without complaint")
	}
}

// TestRecordedOutcomeCarriesProviderObservations: the facts a later stage needs
// live in columns, so a purge cannot take them.
func TestRecordedOutcomeCarriesProviderObservations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := newStore(t)
	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	out := &store.Outcome{
		CaseID: "c1",
		Response: &knov1.Response{
			CaseId:          "c1",
			Output:          "refused",
			Refused:         true,
			StopReason:      knov1.StopReason_STOP_REASON_LENGTH,
			UsageEstimated:  true,
			ResolvedModel:   "gpt-4.1-2026-01-01",
			ProviderBuildId: "fp_abc123",
		},
		Score: &knov1.Score{CaseId: "c1", Value: 0.5},
		Spend: budget.Spend{Calls: 1, CostUSDMicros: 100},
	}
	if err := s.RecordOutcome(ctx, "run-1", out); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}

	if _, err := s.Purge(ctx, "run-1"); err != nil {
		t.Fatalf("Purge: %v", err)
	}

	// The score survives the purge because it is a column, not a blob field.
	sum, counted, unrecoverable, err := s.ScoreSum(ctx, "run-1")
	if err != nil {
		t.Fatalf("ScoreSum: %v", err)
	}
	if sum != 0.5 || counted != 1 || unrecoverable != 0 {
		t.Errorf("after purge: sum=%v counted=%d unrecoverable=%d; want 0.5/1/0 — "+
			"the score is a column precisely so a purge cannot take it",
			sum, counted, unrecoverable)
	}
}

// TestPurgeLeavesTheRunResumable is the test docs/debt.md#25 requires by name.
//
// The outcomes table IS the done-marker: there is no separate checkpoint row.
// So a purge that DELETED rows would make a purged run pay for every Case a
// second time on resume — reopening the exact double-spend this store was
// built to close, through the privacy feature.
//
// Asserts all three things a resume depends on, not just the one the entry
// names: which Cases completed, what was already spent, and the aggregate.
func TestPurgeLeavesTheRunResumable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := newStore(t)
	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	for i := range 10 {
		if err := s.RecordOutcome(ctx, "run-1", scoredOutcome(caseName(i), float64(i), 1_000)); err != nil {
			t.Fatalf("RecordOutcome: %v", err)
		}
	}

	before := snapshot(ctx, t, s, "run-1")

	purged, err := s.Purge(ctx, "run-1")
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if purged != 10 {
		t.Errorf("purged %d rows, want 10", purged)
	}

	after := snapshot(ctx, t, s, "run-1")

	if after.completed != before.completed {
		t.Errorf("completed Cases went from %d to %d; a purge that loses the "+
			"done-marker makes a resumed run pay for that work twice",
			before.completed, after.completed)
	}
	if after.spentMicros != before.spentMicros {
		t.Errorf("settled spend went from %d to %d; a resumed run would restore "+
			"the wrong prior spend and could consume its cap a second time",
			before.spentMicros, after.spentMicros)
	}
	if after.sum != before.sum || after.counted != before.counted {
		t.Errorf("aggregate went from (%v over %d) to (%v over %d); the score "+
			"lives in a column so that a purge cannot move it",
			before.sum, before.counted, after.sum, after.counted)
	}
	if after.unrecoverable != 0 {
		t.Errorf("%d Cases became unrecoverable; purging today preserves the "+
			"number, only a pre-migration purge cannot", after.unrecoverable)
	}

	// Purging again is a no-op rather than an error.
	if n, err := s.Purge(ctx, "run-1"); err != nil || n != 0 {
		t.Errorf("second purge affected %d rows (err %v), want 0", n, err)
	}
}

type runSnapshot struct {
	completed     int
	spentMicros   int64
	sum           float64
	counted       int
	unrecoverable int
}

func snapshot(ctx context.Context, t *testing.T, s *store.SQLite, runID string) runSnapshot {
	t.Helper()

	done, err := s.CompletedCases(ctx, runID)
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}
	spend, err := s.SettledSpend(ctx, runID)
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	sum, counted, unrecoverable, err := s.ScoreSum(ctx, runID)
	if err != nil {
		t.Fatalf("ScoreSum: %v", err)
	}
	return runSnapshot{
		completed:     len(done),
		spentMicros:   spend.CostUSDMicros,
		sum:           sum,
		counted:       counted,
		unrecoverable: unrecoverable,
	}
}

// TestPurgeRemovesTraceContent guards the test above from being satisfied by a
// purge that does nothing at all.
func TestPurgeRemovesTraceContent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := newStore(t)
	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	out := scoredOutcome("c1", 1, 1_000)
	out.Response.Output = "the user's private conversation content"
	out.Score.Rationale = "the judge's reasoning about it"
	if err := s.RecordOutcome(ctx, "run-1", out); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}

	if _, err := s.Purge(ctx, "run-1"); err != nil {
		t.Fatalf("Purge: %v", err)
	}

	resp, score, err := s.RawBlobs(ctx, "run-1", "c1")
	if err != nil {
		t.Fatalf("reading blobs: %v", err)
	}
	if resp != nil {
		t.Error("response_proto survived a purge; it can contain end-user conversation content")
	}
	if score != nil {
		t.Error("score_proto survived a purge; the judge rationale is trace content too")
	}
}

// TestPurgeAndScoreSumFailClosedAfterClose: every Store method must refuse on
// a closed store rather than panicking on a nil handle. The two new ones are
// held to the same rule as the rest.
func TestPurgeAndScoreSumFailClosedAfterClose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s, err := store.NewSQLite(ctx, filepath.Join(t.TempDir(), "kno.db"))
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	if _, err := s.Purge(ctx, "run-1"); err == nil {
		t.Error("Purge succeeded on a closed store")
	}
	if _, _, _, err := s.ScoreSum(ctx, "run-1"); err == nil {
		t.Error("ScoreSum succeeded on a closed store")
	}
}

// TestBackfillSkipsUnparseableScores.
//
// A blob this build cannot decode must not block the upgrade: the row keeps a
// NULL score_value and reports as unrecoverable, which is the same honest
// outcome as a purge. Refusing to open the database instead would strand a
// user's entire history behind one corrupt row.
func TestBackfillSkipsUnparseableScores(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	path := writeM1Database(t, 2, false)

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("reopening raw: %v", err)
	}
	// Not a valid Score encoding: field 1 declared as varint, truncated.
	if _, err := db.ExecContext(ctx,
		`UPDATE outcomes SET score_proto = ? WHERE case_id = ?`,
		[]byte{0x08}, caseName(0)); err != nil {
		t.Fatalf("corrupting a score: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing raw: %v", err)
	}

	s, err := store.NewSQLite(ctx, path)
	if err != nil {
		t.Fatalf("a corrupt score blob blocked the upgrade: %v", err)
	}
	defer func() { _ = s.Close() }()

	_, counted, unrecoverable, err := s.ScoreSum(ctx, "run-1")
	if err != nil {
		t.Fatalf("ScoreSum: %v", err)
	}
	if counted != 1 || unrecoverable != 1 {
		t.Errorf("counted=%d unrecoverable=%d, want 1/1; an undecodable score "+
			"must be reported as unrecoverable, not counted as zero",
			counted, unrecoverable)
	}
}

// TestPurgeIsScopedToItsRun: purging one run must not touch another's traces.
func TestPurgeIsScopedToItsRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := newStore(t)
	for _, id := range []string{"run-1", "run-2"} {
		if err := s.CreateRun(ctx, newRun(id)); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
		if err := s.RecordOutcome(ctx, id, scoredOutcome("c1", 1, 1_000)); err != nil {
			t.Fatalf("RecordOutcome %s: %v", id, err)
		}
	}

	if _, err := s.Purge(ctx, "run-1"); err != nil {
		t.Fatalf("Purge: %v", err)
	}

	resp, _, err := s.RawBlobs(ctx, "run-2", "c1")
	if err != nil {
		t.Fatalf("RawBlobs: %v", err)
	}
	if resp == nil {
		t.Error("purging run-1 also cleared run-2's traces")
	}
}
