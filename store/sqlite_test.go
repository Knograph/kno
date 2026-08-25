package store_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"google.golang.org/protobuf/testing/protocmp"

	"github.com/google/go-cmp/cmp"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

func newStore(t *testing.T) *store.SQLite {
	t.Helper()

	s, err := store.NewSQLite(context.Background(), filepath.Join(t.TempDir(), "kno.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("closing store: %v", err)
		}
	})
	return s
}

func newRun(id string) *knov1.Run {
	return &knov1.Run{
		Id:               id,
		Stage:            knov1.Stage_STAGE_BASELINE,
		Status:           knov1.RunStatus_RUN_STATUS_RUNNING,
		CreatedAt:        "2026-08-20T00:00:00Z",
		GoalName:         "exact-match",
		GoalDirection:    knov1.Direction_DIRECTION_MAXIMIZE,
		InputFingerprint: "fp-1",
	}
}

func scoredOutcome(caseID string, value float64, costMicros int64) *store.Outcome {
	return &store.Outcome{
		CaseID:   caseID,
		Response: &knov1.Response{CaseId: caseID, Output: "answer", CostUsdMicros: costMicros},
		Score:    &knov1.Score{CaseId: caseID, Value: value, Passed: value > 0},
		Spend:    budget.Spend{Calls: 1, CostUSDMicros: costMicros, Tokens: 42},
	}
}

// TestRunRoundTrip covers the basic lifecycle and that a run survives
// marshaling with every field intact.
func TestRunRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)

	want := newRun("run-1")
	if err := s.CreateRun(ctx, want); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	got, err := s.GetRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("run changed across storage (-want +got):\n%s", diff)
	}
}

// TestCreateRunRejectsDuplicates keeps two runs from sharing an ID, which
// would silently merge their outcomes and their spend.
func TestCreateRunRejectsDuplicates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)

	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("first CreateRun: %v", err)
	}
	err := s.CreateRun(ctx, newRun("run-1"))
	if !errors.Is(err, store.ErrRunExists) {
		t.Errorf("second CreateRun returned %v, want ErrRunExists", err)
	}
}

// TestMissingRunIsNotFound checks the sentinel, since the CLI branches on it
// to tell "no such run" from "the database is broken".
func TestMissingRunIsNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.GetRun(ctx, "nope"); !errors.Is(err, store.ErrRunNotFound) {
		t.Errorf("GetRun returned %v, want ErrRunNotFound", err)
	}
	if err := s.FinishRun(ctx, newRun("nope")); !errors.Is(err, store.ErrRunNotFound) {
		t.Errorf("FinishRun returned %v, want ErrRunNotFound", err)
	}
}

// TestRecordOutcomeIsIdempotent is the test that protects the denominator.
//
// Resume re-executes whatever was in flight when the process died. If a second
// recording produced a second row, the scored-case count would grow without
// any new work being done, and every delta measured against this run would be
// computed over a population that never existed.
func TestRecordOutcomeIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)

	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	first := scoredOutcome("case-1", 1, 5_000)
	if err := s.RecordOutcome(ctx, "run-1", first); err != nil {
		t.Fatalf("first RecordOutcome: %v", err)
	}

	// The same Case again, as a resumed run would, with a different result
	// and a different cost.
	second := scoredOutcome("case-1", 0, 9_999)
	if err := s.RecordOutcome(ctx, "run-1", second); err != nil {
		t.Fatalf("second RecordOutcome: %v", err)
	}

	done, err := s.CompletedCases(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}
	if len(done) != 1 {
		t.Errorf("completed cases = %d, want 1: a duplicate recording created a second row", len(done))
	}

	// The FIRST result stands. The money for it is already spent and counted;
	// letting a retry overwrite it would silently replace a real measurement.
	spend, err := s.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if spend.CostUSDMicros != 5_000 {
		t.Errorf("settled spend = %d, want 5000 (the first recording, not the second)",
			spend.CostUSDMicros)
	}
	if spend.Calls != 1 {
		t.Errorf("settled calls = %d, want 1", spend.Calls)
	}
}

// TestOutcomeMustBeScoredOrErrored enforces the split that keeps a Case off
// both sides of the denominator.
func TestOutcomeMustBeScoredOrErrored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)

	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	tests := []struct {
		name string
		out  *store.Outcome
	}{
		{
			name: "both scored and errored",
			out: &store.Outcome{
				CaseID: "c", Score: &knov1.Score{CaseId: "c"}, Err: "AGENT_UNREACHABLE",
			},
		},
		{
			name: "neither scored nor errored",
			out:  &store.Outcome{CaseID: "c"},
		},
		{
			name: "no case ID",
			out:  &store.Outcome{Score: &knov1.Score{}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := s.RecordOutcome(ctx, "run-1", tc.out); err == nil {
				t.Error("want an error; an ambiguous outcome corrupts the counts")
			}
		})
	}
}

// TestSettledSpendIsTheDurableRecord covers what resume reads to reseed the
// budget guard.
//
// The guard is in-memory, so this sum is the only thing that survives a crash.
// If it were wrong, a resumed run would authorize against a cap it had already
// consumed.
func TestSettledSpendIsTheDurableRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)

	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// A fresh run has spent nothing. SUM over zero rows is NULL in SQL, and
	// that must read as zero rather than failing the scan.
	spend, err := s.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend on an empty run: %v", err)
	}
	if spend != (budget.Spend{}) {
		t.Errorf("empty run spend = %+v, want zero", spend)
	}

	const n = 100
	var wantCost int64
	for i := range n {
		cost := int64(1_234 + i) // deliberately not round
		wantCost += cost
		if err := s.RecordOutcome(ctx, "run-1", scoredOutcome(fmt.Sprintf("case-%d", i), 1, cost)); err != nil {
			t.Fatalf("RecordOutcome %d: %v", i, err)
		}
	}

	spend, err = s.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if spend.CostUSDMicros != wantCost {
		t.Errorf("summed spend = %d, want exactly %d (drift of %d)",
			spend.CostUSDMicros, wantCost, spend.CostUSDMicros-wantCost)
	}
	if spend.Calls != n {
		t.Errorf("summed calls = %d, want %d", spend.Calls, n)
	}
}

// TestSpendIsScopedToItsRun guards against a resumed run inheriting another
// run's spend, which would refuse work that is actually affordable.
func TestSpendIsScopedToItsRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)

	for _, id := range []string{"run-1", "run-2"} {
		if err := s.CreateRun(ctx, newRun(id)); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
	}
	if err := s.RecordOutcome(ctx, "run-1", scoredOutcome("case-1", 1, 7_000)); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}

	spend, err := s.SettledSpend(ctx, "run-2")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if spend.CostUSDMicros != 0 {
		t.Errorf("run-2 spend = %d, want 0: spend leaked across runs", spend.CostUSDMicros)
	}

	done, err := s.CompletedCases(ctx, "run-2")
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}
	if len(done) != 0 {
		t.Errorf("run-2 has %d completed cases, want 0: outcomes leaked across runs", len(done))
	}
}

// TestErroredOutcomeIsNotScored keeps the two counts distinct in storage, the
// same way the event stream keeps them distinct on the wire.
func TestErroredOutcomeIsNotScored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)

	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	errored := &store.Outcome{
		CaseID:   "case-1",
		Response: &knov1.Response{CaseId: "case-1", Error: "dial tcp: connection refused"},
		Err:      "AGENT_UNREACHABLE",
		Spend:    budget.Spend{Calls: 1, CostUSDMicros: 0},
	}
	if errored.Scored() {
		t.Fatal("an outcome with Err set reports itself as scored")
	}
	if err := s.RecordOutcome(ctx, "run-1", errored); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}

	// An errored Case is still COMPLETED — resume must not retry it forever.
	done, err := s.CompletedCases(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}
	if _, ok := done["case-1"]; !ok {
		t.Error("a terminally errored Case is not marked complete; resume would re-run it")
	}
}

// TestEventSequenceSurvivesResume covers the property Event.sequence exists
// for.
//
// A resumed run continues from max+1. Restarting at 1 would collide with
// events from before the interruption, and a consumer watching for gaps would
// see none — silently losing the detection in exactly the case that needs it.
func TestEventSequenceSurvivesResume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)

	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if got, err := s.MaxEventSequence(ctx, "run-1"); err != nil || got != 0 {
		t.Fatalf("MaxEventSequence on a fresh run = %d, %v; want 0, nil", got, err)
	}

	for seq := int64(1); seq <= 5; seq++ {
		ev := &knov1.Event{RunId: "run-1", Sequence: seq, EmittedAt: "2026-08-20T00:00:00Z"}
		if err := s.AppendEvent(ctx, ev); err != nil {
			t.Fatalf("AppendEvent %d: %v", seq, err)
		}
	}

	got, err := s.MaxEventSequence(ctx, "run-1")
	if err != nil {
		t.Fatalf("MaxEventSequence: %v", err)
	}
	if got != 5 {
		t.Errorf("MaxEventSequence = %d, want 5; a resumed run would restart numbering and collide", got)
	}
}

// TestAppendEventRequiresSequence refuses an unnumbered event rather than
// storing one that breaks gap detection for every consumer of that run.
func TestAppendEventRequiresSequence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)

	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	err := s.AppendEvent(ctx, &knov1.Event{RunId: "run-1"})
	if err == nil {
		t.Error("an event with no sequence was accepted")
	}
}

// TestConcurrentWritesDoNotLoseOutcomes is the durability property the
// executor depends on: many workers finishing at once, every result recorded
// exactly once.
//
// SQLite has a single writer, so this is also where SQLITE_BUSY would surface
// if the connection pool were misconfigured.
func TestConcurrentWritesDoNotLoseOutcomes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)

	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	const workers = 64
	var wg sync.WaitGroup
	errs := make(chan error, workers)

	for i := range workers {
		wg.Go(func() {
			out := scoredOutcome(fmt.Sprintf("case-%d", i), 1, 1_000)
			if err := s.RecordOutcome(ctx, "run-1", out); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent RecordOutcome failed: %v", err)
	}

	done, err := s.CompletedCases(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}
	if len(done) != workers {
		t.Errorf("recorded %d outcomes, want %d: concurrent writes lost results", len(done), workers)
	}

	spend, err := s.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if want := int64(workers) * 1_000; spend.CostUSDMicros != want {
		t.Errorf("summed spend = %d, want %d", spend.CostUSDMicros, want)
	}
}

// TestReopenSeesPriorState is the crash-and-resume path end to end: everything
// a resumed process needs must be readable from a freshly opened database.
func TestReopenSeesPriorState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kno.db")

	first, err := store.NewSQLite(ctx, path)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if err := first.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := first.RecordOutcome(ctx, "run-1", scoredOutcome("case-1", 1, 12_345)); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	if err := first.AppendEvent(ctx, &knov1.Event{RunId: "run-1", Sequence: 7}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A new process opens the same database.
	second, err := store.NewSQLite(ctx, path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer func() { _ = second.Close() }()

	if _, err := second.GetRun(ctx, "run-1"); err != nil {
		t.Errorf("run did not survive reopen: %v", err)
	}
	done, err := second.CompletedCases(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}
	if _, ok := done["case-1"]; !ok {
		t.Error("completed case did not survive reopen; resume would pay for it twice")
	}
	spend, err := second.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if spend.CostUSDMicros != 12_345 {
		t.Errorf("spend after reopen = %d, want 12345; the guard would reseed wrong", spend.CostUSDMicros)
	}
	if seq, err := second.MaxEventSequence(ctx, "run-1"); err != nil || seq != 7 {
		t.Errorf("max sequence after reopen = %d, %v; want 7, nil", seq, err)
	}
}

// TestCloseIsIdempotent makes `defer s.Close()` safe alongside an explicit
// close on the success path.
func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	s, err := store.NewSQLite(context.Background(), filepath.Join(t.TempDir(), "kno.db"))
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestPragmasApplyToEveryConnection is the test the original suite could not
// have caught this with.
//
// foreign_keys and busy_timeout are per-CONNECTION settings. Applying them
// with a single Exec after open configures ONLY the connection that served it.
// database/sql silently replaces connections whose last statement was
// interrupted — which is what an ordinary context cancellation does, and what
// the Ctrl-C drain path does by design — and the replacement had
// foreign_keys=0 and busy_timeout=0.
//
// Verified against this driver before the fix: after cancelling a long query,
// an outcome for a nonexistent run inserted silently instead of being rejected
// by the foreign key, and write contention became an immediate SQLITE_BUSY
// rather than a retry — dropping an outcome whose money was already spent.
//
// The property is tested directly rather than by trying to force an
// interruption: an attempt to do that passed against the broken code, because
// a query over a small table finishes before any cancellation lands.
func TestPragmasApplyToEveryConnection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)

	const conns = 4
	got, err := s.PragmaOnEveryConn(ctx, conns)
	if err != nil {
		t.Fatalf("probing pragmas: %v", err)
	}
	if len(got) != conns {
		t.Fatalf("probed %d connections, want %d", len(got), conns)
	}

	for i, p := range got {
		fk, bt := p[0], p[1]
		if fk != 1 {
			t.Errorf("connection %d has foreign_keys=%d, want 1.\n"+
				"An outcome for a nonexistent run would insert silently instead of "+
				"being rejected.", i, fk)
		}
		if bt != 5000 {
			t.Errorf("connection %d has busy_timeout=%d, want 5000.\n"+
				"Write contention would return SQLITE_BUSY immediately instead of "+
				"retrying, dropping an outcome whose money is already spent.", i, bt)
		}
	}
}

// TestForeignKeyRejectsOrphanOutcome is the consequence of the pragma holding:
// an outcome can never be recorded against a run that does not exist, which
// would otherwise be a Case counted as complete that nothing can attribute.
func TestForeignKeyRejectsOrphanOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)

	err := s.RecordOutcome(ctx, "no-such-run", scoredOutcome("case-1", 1, 100))
	if err == nil {
		t.Error("an outcome for a nonexistent run was accepted; foreign keys are not enforced")
	}
}

// TestCancelledContextDoesNotCorruptState checks that an interrupted call
// leaves nothing half-written.
func TestCancelledContextDoesNotCorruptState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)

	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	// Either it refuses or it succeeds; what it must not do is record a
	// partial outcome that later reads as complete.
	_ = s.RecordOutcome(cancelled, "run-1", scoredOutcome("case-1", 1, 500))

	spend, err := s.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	done, err := s.CompletedCases(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}

	// Whatever happened, the two must agree: a Case counted as done must have
	// contributed its spend, and vice versa.
	if len(done) == 1 && spend.CostUSDMicros != 500 {
		t.Errorf("case recorded as complete but spend = %d, want 500", spend.CostUSDMicros)
	}
	if len(done) == 0 && spend.CostUSDMicros != 0 {
		t.Errorf("no case recorded but spend = %d, want 0", spend.CostUSDMicros)
	}
}

// TestCloseDoesNotRaceWithInFlightCalls covers the executor's actual shutdown
// shape: drain in-flight work, then close.
//
// An earlier version read and nil'd s.db without synchronization, which the
// race detector flags the moment a query overlaps a close.
func TestCloseDoesNotRaceWithInFlightCalls(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s, err := store.NewSQLite(ctx, filepath.Join(t.TempDir(), "kno.db"))
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			// Errors are expected once the store closes; the point is that
			// nothing races or panics.
			_ = s.RecordOutcome(ctx, "run-1", scoredOutcome(fmt.Sprintf("case-%d", i), 1, 10))
			_, _ = s.CompletedCases(ctx, "run-1")
		})
	}
	wg.Go(func() {
		_ = s.Close()
	})
	wg.Wait()

	// A closed store reports it rather than panicking on a nil handle.
	if _, err := s.GetRun(ctx, "run-1"); err == nil {
		t.Log("note: the store was still open when this ran; no race either way")
	}
}

// TestOutcomeCountsSpanTheWholeRun covers what a resumed run reads to report
// counts for the whole run rather than only the portion it executed.
func TestOutcomeCountsSpanTheWholeRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)

	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// A fresh run has counted nothing, and that must read as zero rather than
	// failing the scan.
	scored, errored, err := s.OutcomeCounts(ctx, "run-1")
	if err != nil {
		t.Fatalf("OutcomeCounts on an empty run: %v", err)
	}
	if scored != 0 || errored != 0 {
		t.Errorf("empty run = %d scored, %d errored; want 0, 0", scored, errored)
	}

	for i := range 7 {
		if err := s.RecordOutcome(ctx, "run-1", scoredOutcome(fmt.Sprintf("ok-%d", i), 1, 100)); err != nil {
			t.Fatalf("RecordOutcome: %v", err)
		}
	}
	for i := range 3 {
		out := &store.Outcome{
			CaseID: fmt.Sprintf("bad-%d", i),
			Err:    "AGENT_ERROR",
			Spend:  budget.Spend{Calls: 1},
		}
		if err := s.RecordOutcome(ctx, "run-1", out); err != nil {
			t.Fatalf("RecordOutcome: %v", err)
		}
	}

	scored, errored, err = s.OutcomeCounts(ctx, "run-1")
	if err != nil {
		t.Fatalf("OutcomeCounts: %v", err)
	}
	if scored != 7 || errored != 3 {
		t.Errorf("counts = %d scored, %d errored; want 7 and 3", scored, errored)
	}
}

// TestOutcomeCountsAreScopedToTheirRun: a resumed run must not inherit another
// run's counts, which would inflate its reported denominator.
func TestOutcomeCountsAreScopedToTheirRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)

	for _, id := range []string{"run-1", "run-2"} {
		if err := s.CreateRun(ctx, newRun(id)); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
	}
	if err := s.RecordOutcome(ctx, "run-1", scoredOutcome("c1", 1, 10)); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}

	scored, errored, err := s.OutcomeCounts(ctx, "run-2")
	if err != nil {
		t.Fatalf("OutcomeCounts: %v", err)
	}
	if scored != 0 || errored != 0 {
		t.Errorf("run-2 = %d scored, %d errored; counts leaked across runs", scored, errored)
	}
}

// TestClosedStoreRefusesEveryOperation.
//
// A closed store must report that plainly rather than panicking on a nil
// handle, since the executor's shutdown is drain-then-close and a late call is
// expected rather than exceptional.
func TestClosedStoreRefusesEveryOperation(t *testing.T) {
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
		t.Fatalf("Close: %v", err)
	}

	if _, _, err := s.OutcomeCounts(ctx, "run-1"); err == nil {
		t.Error("OutcomeCounts on a closed store returned no error")
	}
	if _, err := s.SettledSpend(ctx, "run-1"); err == nil {
		t.Error("SettledSpend on a closed store returned no error")
	}
	if _, err := s.CompletedCases(ctx, "run-1"); err == nil {
		t.Error("CompletedCases on a closed store returned no error")
	}
	if _, err := s.GetRun(ctx, "run-1"); err == nil {
		t.Error("GetRun on a closed store returned no error")
	}
	if err := s.RecordOutcome(ctx, "run-1", scoredOutcome("c", 1, 1)); err == nil {
		t.Error("RecordOutcome on a closed store returned no error")
	}
	if err := s.AppendEvent(ctx, &knov1.Event{RunId: "run-1", Sequence: 1}); err == nil {
		t.Error("AppendEvent on a closed store returned no error")
	}
	if _, err := s.MaxEventSequence(ctx, "run-1"); err == nil {
		t.Error("MaxEventSequence on a closed store returned no error")
	}
	if err := s.FinishRun(ctx, newRun("run-1")); err == nil {
		t.Error("FinishRun on a closed store returned no error")
	}
}

// TestRecordOrphanSpendIsDurableWithoutMarkingACaseDone.
//
// The whole point of the method: money the guard settled for a Case that
// produced no outcome has to survive into SettledSpend, which Guard.Restore
// reads on resume — while the Case stays absent from CompletedCases so a
// resumed run re-attempts it.
//
// Recording it as an outcome row would do the opposite on both counts.
// RecordOutcome is INSERT OR IGNORE, so a spend-only row would permanently
// block the real outcome for that Case.
func TestRecordOrphanSpendIsDurableWithoutMarkingACaseDone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := newStore(t)
	if err := st.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// One Case completes normally.
	if err := st.RecordOutcome(ctx, "run-1", &store.Outcome{
		CaseID: "done-1",
		Score:  &knov1.Score{Value: 1},
		Spend:  budget.Spend{Calls: 1, CostUSDMicros: 10_000, Tokens: 5},
	}); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}

	// Another was charged and then refused, so it has no outcome.
	if err := st.RecordOrphanSpend(ctx, "run-1",
		budget.Spend{Calls: 2, CostUSDMicros: 80_000, Tokens: 3}); err != nil {
		t.Fatalf("RecordOrphanSpend: %v", err)
	}

	spend, err := st.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	want := budget.Spend{Calls: 3, CostUSDMicros: 90_000, Tokens: 8}
	if spend != want {
		t.Errorf("SettledSpend = %+v, want %+v — a resume restores this figure, so "+
			"anything missing from it is headroom spent twice", spend, want)
	}

	done, err := st.CompletedCases(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}
	if len(done) != 1 {
		t.Errorf("%d Cases marked complete, want 1; orphan spend must not mark a "+
			"Case done that never produced an answer", len(done))
	}
	if _, ok := done["done-1"]; !ok {
		t.Error("the completed Case is missing from CompletedCases")
	}
}

// TestOrphanSpendAccumulates.
//
// Additive, because a run can refuse several Cases after charging for them,
// and each is a separate settlement the guard already made.
func TestOrphanSpendAccumulates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := newStore(t)
	if err := st.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	for range 3 {
		if err := st.RecordOrphanSpend(ctx, "run-1",
			budget.Spend{Calls: 1, CostUSDMicros: 20_000}); err != nil {
			t.Fatalf("RecordOrphanSpend: %v", err)
		}
	}

	spend, err := st.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if spend.CostUSDMicros != 60_000 || spend.Calls != 3 {
		t.Errorf("SettledSpend = %+v, want 3 calls and 60000 micro-USD; the writes "+
			"must add rather than replace", spend)
	}
}

// TestRecordOrphanSpendRefusesANegativeCharge.
//
// This UPDATE is the first subtraction primitive on the money path. A negative
// folds into the sum inside SQLite before Guard.Restore sees it, so addSpend's
// refusal of negatives protects nothing: a run with real spend plus a negative
// orphan write restores less than it spent and gets the difference as free
// headroom.
//
// Clamped in the statement rather than trusted from the caller — the same
// conclusion docs/debt.md#48 reached for Reservation.Settle.
func TestRecordOrphanSpendRefusesANegativeCharge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := newStore(t)
	if err := st.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.RecordOutcome(ctx, "run-1", scoredOutcome("c1", 1, 5_000)); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}

	if err := st.RecordOrphanSpend(ctx, "run-1",
		budget.Spend{Calls: -5, CostUSDMicros: -1_000, Tokens: -3}); err != nil {
		t.Fatalf("RecordOrphanSpend: %v", err)
	}

	spend, err := st.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if spend.CostUSDMicros != 5_000 {
		t.Errorf("settled spend = %d, want 5000 — a negative charge must not "+
			"subtract from what the run has already spent, or a resume restores "+
			"less than it owes and spends the difference again", spend.CostUSDMicros)
	}
	if spend.Calls < 0 || spend.Tokens < 0 {
		t.Errorf("settled spend went negative: %+v", spend)
	}
}

// TestRecordOrphanSpendRefusesAnUnknownRun.
//
// Silently dropping the spend is the failure this method exists to prevent, so
// a caller bug must be loud rather than a no-op that reads as success.
func TestRecordOrphanSpendRefusesAnUnknownRun(t *testing.T) {
	t.Parallel()

	st := newStore(t)
	err := st.RecordOrphanSpend(context.Background(), "no-such-run",
		budget.Spend{Calls: 1, CostUSDMicros: 1_000})
	if err == nil {
		t.Fatal("recording spend against a run that does not exist succeeded; the " +
			"money would vanish with nothing reporting it")
	}
}

// TestSettledSpendOnAFreshRunIsZero.
//
// SettledSpend now joins runs against outcomes, so a run with no outcomes must
// still read as zero rather than as a scan error — Guard.Restore calls this on
// every resume, including the first one after a run was created.
func TestSettledSpendOnAFreshRunIsZero(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := newStore(t)
	if err := st.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	spend, err := st.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend on a run with no outcomes: %v", err)
	}
	if (spend != budget.Spend{}) {
		t.Errorf("SettledSpend = %+v, want zero", spend)
	}

	// And a run that does not exist at all is zero, not an error: the guard
	// restores nothing and the run proceeds.
	if spend, err = st.SettledSpend(ctx, "never-created"); err != nil {
		t.Errorf("SettledSpend on an unknown run: %v", err)
	}
	if (spend != budget.Spend{}) {
		t.Errorf("SettledSpend on an unknown run = %+v, want zero", spend)
	}
}

// TestPurgeLeavesOrphanSpendIntact.
//
// docs/debt.md#25 records that `kno purge` and the resume done-marker share a
// row, and that a purge which DELETED rows would reopen the double-spend hole
// the store exists to close. Orphan spend adds a second place money lives, so
// purge has to be checked against it too.
//
// It survives by design rather than by accident: the spend is columns on
// `runs`, never trace content, and Purge only nulls blob columns on
// `outcomes`. Asserted so a future purge that widened its reach would fail
// here rather than silently erase a run's spend record.
func TestPurgeLeavesOrphanSpendIntact(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := newStore(t)
	if err := st.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.RecordOutcome(ctx, "run-1", scoredOutcome("c1", 1, 10_000)); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	if err := st.RecordOrphanSpend(ctx, "run-1",
		budget.Spend{Calls: 2, CostUSDMicros: 80_000, Tokens: 7}); err != nil {
		t.Fatalf("RecordOrphanSpend: %v", err)
	}

	before, err := st.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}

	if _, err := st.Purge(ctx, "run-1"); err != nil {
		t.Fatalf("Purge: %v", err)
	}

	after, err := st.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend after purge: %v", err)
	}
	if after != before {
		t.Errorf("purge changed the run's settled spend from %+v to %+v. Spend is "+
			"not trace content, and a resumed run restores this figure — losing it "+
			"lets the run spend its cap a second time", before, after)
	}

	// And the completed Case is still marked complete, which is #25's own
	// invariant: a purge that reopened the double-spend hole would be a
	// privacy feature that costs money.
	done, err := st.CompletedCases(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}
	if _, ok := done["c1"]; !ok {
		t.Error("purge removed the done-marker for a completed Case")
	}
}
