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
	defer second.Close() //nolint:errcheck // failures surface through the assertions below

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
