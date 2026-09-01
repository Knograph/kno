package bridge_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/knograph/kno/bridge"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// fakeTuner drives SubmitGroup's tests. Submit optionally queries the store
// mid-call — the shape acceptance criterion 6 needs to assert the
// write-ahead row is durable BEFORE Submit is entered.
type fakeTuner struct {
	submitCalls int
	submitErr   error
	ref         *core.JobRef

	// onSubmit runs INSIDE Submit, before it returns — the hook
	// TestSubmitGroupWriteAheadBeforeSubmit uses to query the store from the
	// same vantage point a real adapter's Submit implementation would have.
	onSubmit func()
}

func (f *fakeTuner) Submit(_ context.Context, _ *core.TuningJob) (*core.JobRef, error) {
	f.submitCalls++
	if f.onSubmit != nil {
		f.onSubmit()
	}
	if f.submitErr != nil {
		return nil, f.submitErr
	}
	return f.ref, nil
}

func (f *fakeTuner) Status(context.Context, *core.JobRef) (*core.JobState, error) {
	return nil, errors.New("fakeTuner.Status not implemented")
}

func (f *fakeTuner) Model(context.Context, *core.JobRef) (*core.AgentRef, error) {
	return nil, errors.New("fakeTuner.Model not implemented")
}

func (f *fakeTuner) Deploy(context.Context, *core.JobRef) (*core.Endpoint, error) {
	return nil, errors.New("fakeTuner.Deploy not implemented")
}

func (f *fakeTuner) Teardown(context.Context, *core.Endpoint) error {
	return errors.New("fakeTuner.Teardown not implemented")
}

var _ core.Tuner = (*fakeTuner)(nil)

func newBridgeTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.NewSQLite(context.Background(), filepath.Join(t.TempDir(), "kno.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// testJob builds a Submit-ready TuningJob. tokens is accepted for callers
// that want the estimate and the token count to read consistently in a
// test's arrange section, but core.TuningJob carries no tokens field on the
// wire — see SubmitGroupParams.TrainTokens for where that dimension travels.
func testJob(group string, estimate, _ int64) *core.TuningJob {
	return &core.TuningJob{
		BaseModel:              &knov1.AgentRef{Ref: "together:meta-llama/Llama-3-8b", Scheme: "together", Target: "meta-llama/Llama-3-8b"},
		AssetIds:               []string{"a1", "a2"},
		TrainingData:           []byte(`{"messages":[{"role":"assistant","content":"x"}]}` + "\n"),
		Suffix:                 "kno-run-1-" + group,
		AblationGroup:          group,
		EstimatedCostUsdMicros: estimate,
	}
}

// params builds SubmitGroupParams against a freshly created "run-1". Callers
// that need a second SubmitGroup call against the same run (a resume, or a
// second group) construct bridge.SubmitGroupParams directly instead of
// calling this helper twice — CreateRun is not idempotent.
func params(t *testing.T, st store.Store, guard *budget.Guard, tuner core.Tuner, group string, estimate, tokens int64) bridge.SubmitGroupParams {
	t.Helper()
	if err := st.CreateRun(context.Background(), &knov1.Run{
		Id:     "run-1",
		Stage:  knov1.Stage_STAGE_BRIDGE,
		Status: knov1.RunStatus_RUN_STATUS_RUNNING,
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return bridge.SubmitGroupParams{
		RunID:              "run-1",
		AblationGroup:      group,
		Store:              st,
		Guard:              guard,
		Tuner:              tuner,
		Job:                testJob(group, estimate, tokens),
		TrainTokens:        tokens,
		TrainingFileSHA256: "deadbeef",
		Provider:           "together",
	}
}

// TestSubmitGroupHappyPathSettlesAllThreeDimensions covers acceptance
// criteria 5, 6, and 7: Submit is only reached with an open reservation, the
// write-ahead row exists before Submit is entered, and the estimate settles
// on Calls/CostUSDMicros/Tokens — all three, matching the #170/#172 fix.
func TestSubmitGroupHappyPathSettlesAllThreeDimensions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	guard := budget.New(budget.Limits{}, nil, 0)
	tuner := &fakeTuner{ref: &core.JobRef{Id: "job-1", Provider: "together", SubmittedAt: "2026-08-31T00:00:00Z"}}

	res, err := bridge.SubmitGroup(ctx, params(t, st, guard, tuner, "all-in", 6_000_000, 500_000))
	if err != nil {
		t.Fatalf("SubmitGroup: %v", err)
	}
	if res.Outcome != bridge.SubmitOutcomeSubmitted {
		t.Errorf("Outcome = %v, want SubmitOutcomeSubmitted", res.Outcome)
	}
	if tuner.submitCalls != 1 {
		t.Errorf("Submit called %d times, want 1", tuner.submitCalls)
	}

	spent := guard.Spent()
	want := budget.Spend{Calls: 1, CostUSDMicros: 6_000_000, Tokens: 500_000}
	if spent != want {
		t.Errorf("Guard.Spent() = %+v, want %+v (all three dimensions)", spent, want)
	}

	settled, err := st.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if settled != want {
		t.Errorf("store.SettledSpend() = %+v, want %+v — a fresh process must reseed identically", settled, want)
	}

	jobs, err := st.TuningJobs(ctx, "run-1")
	if err != nil {
		t.Fatalf("TuningJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].State != store.TuningJobStateSubmitted || jobs[0].ProviderJobID != "job-1" {
		t.Errorf("got %+v, want one submitted row naming job-1", jobs)
	}
}

// TestSubmitGroupWriteAheadBeforeSubmit is acceptance criterion 6 driven
// exactly as the plan specifies: a fake Tuner queries the store from INSIDE
// Submit and asserts the row is present with the estimate in its cost
// columns.
func TestSubmitGroupWriteAheadBeforeSubmit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	guard := budget.New(budget.Limits{}, nil, 0)

	var sawDuringSubmit *store.TuningJobRecord
	tuner := &fakeTuner{ref: &core.JobRef{Id: "job-1", Provider: "together"}}
	tuner.onSubmit = func() {
		jobs, err := st.TuningJobs(ctx, "run-1")
		if err != nil {
			t.Fatalf("TuningJobs from inside Submit: %v", err)
		}
		for _, j := range jobs {
			if j.AblationGroup == "all-in" {
				sawDuringSubmit = j
			}
		}
	}

	if _, err := bridge.SubmitGroup(ctx, params(t, st, guard, tuner, "all-in", 6_000_000, 500_000)); err != nil {
		t.Fatalf("SubmitGroup: %v", err)
	}

	if sawDuringSubmit == nil {
		t.Fatal("no write-ahead row was visible from inside Submit")
	}
	if sawDuringSubmit.State != store.TuningJobStateSubmitting {
		t.Errorf("row state during Submit = %q, want %q", sawDuringSubmit.State, store.TuningJobStateSubmitting)
	}
	if sawDuringSubmit.EstimatedCostUSDMicros != 6_000_000 {
		t.Errorf("row estimate during Submit = %d, want 6000000", sawDuringSubmit.EstimatedCostUSDMicros)
	}
	if sawDuringSubmit.TrainTokens != 500_000 {
		t.Errorf("row train tokens during Submit = %d, want 500000", sawDuringSubmit.TrainTokens)
	}
}

// TestSubmitGroupNeverCalledWithoutAnOpenReservation is the other half of
// acceptance criterion 5: a cap set one micro-dollar below the estimate
// authorizes nothing, and Submit is called zero times.
func TestSubmitGroupNeverCalledWithoutAnOpenReservation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	guard := budget.New(budget.Limits{MaxCostUSDMicros: 5_999_999}, nil, 0)
	tuner := &fakeTuner{ref: &core.JobRef{Id: "job-1", Provider: "together"}}

	_, err := bridge.SubmitGroup(ctx, params(t, st, guard, tuner, "all-in", 6_000_000, 500_000))
	if err == nil {
		t.Fatal("want a budget refusal")
	}
	if tuner.submitCalls != 0 {
		t.Errorf("Submit called %d times, want 0", tuner.submitCalls)
	}
	jobs, err := st.TuningJobs(ctx, "run-1")
	if err != nil {
		t.Fatalf("TuningJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("got %d rows, want 0 — a refused authorization must write nothing", len(jobs))
	}
}

// TestSubmitGroupNeverResubmitsAnAlreadySubmittedGroup is acceptance
// criterion 8's core: a group whose row is already TuningJobStateSubmitted
// must never see a second Submit call, and the durable estimate is reported
// exactly once.
func TestSubmitGroupNeverResubmitsAnAlreadySubmittedGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	guard := budget.New(budget.Limits{}, nil, 0)
	tuner := &fakeTuner{ref: &core.JobRef{Id: "job-1", Provider: "together"}}

	p := params(t, st, guard, tuner, "all-in", 6_000_000, 500_000)
	if _, err := bridge.SubmitGroup(ctx, p); err != nil {
		t.Fatalf("first SubmitGroup: %v", err)
	}
	if tuner.submitCalls != 1 {
		t.Fatalf("Submit called %d times after first call, want 1", tuner.submitCalls)
	}

	// Simulate a resumed run: fresh Guard (a new process would reseed from
	// SettledSpend), same store.
	settled, err := st.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	resumedGuard := budget.New(budget.Limits{}, nil, 0)
	resumedGuard.Restore(settled)

	res, err := bridge.SubmitGroup(ctx, bridge.SubmitGroupParams{
		RunID: "run-1", AblationGroup: "all-in",
		Store: st, Guard: resumedGuard, Tuner: tuner,
		Job: testJob("all-in", 6_000_000, 500_000), TrainTokens: 500_000,
		TrainingFileSHA256: "deadbeef", Provider: "together",
	})
	if err != nil {
		t.Fatalf("resumed SubmitGroup: %v", err)
	}
	if res.Outcome != bridge.SubmitOutcomeAlreadySubmitted {
		t.Errorf("Outcome = %v, want SubmitOutcomeAlreadySubmitted", res.Outcome)
	}
	if tuner.submitCalls != 1 {
		t.Errorf("Submit called %d times total, want exactly 1 (zero additional calls on resume)", tuner.submitCalls)
	}

	finalSpend, err := st.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	want := budget.Spend{Calls: 1, CostUSDMicros: 6_000_000, Tokens: 500_000}
	if finalSpend != want {
		t.Errorf("SettledSpend after resume = %+v, want %+v (the estimate exactly once)", finalSpend, want)
	}
}

// TestSubmitGroupAbandonsACrashedWriteAheadRow is acceptance criterion 9's
// safe subset: a row left "submitting" by a crash inside the request window
// is closed abandoned, Submit is never called, and the estimate stays
// settled. See the package doc for why this build does not attempt
// adopt-by-suffix first.
func TestSubmitGroupAbandonsACrashedWriteAheadRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	guard := budget.New(budget.Limits{}, nil, 0)

	if err := st.CreateRun(ctx, &knov1.Run{Id: "run-1", Stage: knov1.Stage_STAGE_BRIDGE, Status: knov1.RunStatus_RUN_STATUS_RUNNING}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// Simulate a crash: the write-ahead row exists, Submit's outcome is
	// unknown to this process.
	if err := st.WriteTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup:          "all-in",
		State:                  store.TuningJobStateSubmitting,
		EstimatedCostUSDMicros: 6_000_000,
		TrainTokens:            500_000,
	}); err != nil {
		t.Fatalf("WriteTuningJob: %v", err)
	}
	guard.Restore(budget.Spend{}) // a fresh process would reseed from SettledSpend; nothing settled yet

	tuner := &fakeTuner{ref: &core.JobRef{Id: "job-1", Provider: "together"}}
	res, err := bridge.SubmitGroup(ctx, bridge.SubmitGroupParams{
		RunID: "run-1", AblationGroup: "all-in",
		Store: st, Guard: guard, Tuner: tuner,
		Job: testJob("all-in", 6_000_000, 500_000), TrainTokens: 500_000,
		TrainingFileSHA256: "deadbeef", Provider: "together",
	})
	if err != nil {
		t.Fatalf("SubmitGroup: %v", err)
	}
	if res.Outcome != bridge.SubmitOutcomeAbandoned {
		t.Errorf("Outcome = %v, want SubmitOutcomeAbandoned", res.Outcome)
	}
	if tuner.submitCalls != 0 {
		t.Errorf("Submit called %d times, want 0 — a crashed write-ahead row must never be re-submitted blind", tuner.submitCalls)
	}

	jobs, err := st.TuningJobs(ctx, "run-1")
	if err != nil {
		t.Fatalf("TuningJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].State != store.TuningJobStateAbandoned {
		t.Fatalf("got %+v, want one abandoned row", jobs)
	}

	settled, err := st.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	want := budget.Spend{Calls: 1, CostUSDMicros: 6_000_000, Tokens: 500_000}
	if settled != want {
		t.Errorf("SettledSpend for an abandoned job = %+v, want %+v (estimate stays settled)", settled, want)
	}
}

// TestSubmitGroupRefusesToRetryAnAbandonedGroup guards against silently
// re-submitting a group this run has already given up on — a caller bug,
// not a normal resume path.
func TestSubmitGroupRefusesToRetryAnAbandonedGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	guard := budget.New(budget.Limits{}, nil, 0)
	if err := st.CreateRun(ctx, &knov1.Run{Id: "run-1", Stage: knov1.Stage_STAGE_BRIDGE, Status: knov1.RunStatus_RUN_STATUS_RUNNING}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.WriteTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup: "all-in", State: store.TuningJobStateAbandoned,
		EstimatedCostUSDMicros: 6_000_000, TrainTokens: 500_000,
	}); err != nil {
		t.Fatalf("WriteTuningJob: %v", err)
	}

	tuner := &fakeTuner{ref: &core.JobRef{Id: "job-1"}}
	_, err := bridge.SubmitGroup(ctx, bridge.SubmitGroupParams{
		RunID: "run-1", AblationGroup: "all-in",
		Store: st, Guard: guard, Tuner: tuner,
		Job: testJob("all-in", 6_000_000, 500_000), TrainTokens: 500_000,
		TrainingFileSHA256: "deadbeef", Provider: "together",
	})
	if !errors.Is(err, bridge.ErrAlreadyAbandoned) {
		t.Errorf("err = %v, want ErrAlreadyAbandoned", err)
	}
	if tuner.submitCalls != 0 {
		t.Errorf("Submit called %d times, want 0", tuner.submitCalls)
	}
}

// TestSubmitGroupDoesNotSettleOnSubmitFailure covers the "Submit fails"
// half of the no-retry rule: the reservation is released, nothing is
// settled, and the row stays submitting for a LATER resume to close.
func TestSubmitGroupDoesNotSettleOnSubmitFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	guard := budget.New(budget.Limits{MaxCostUSDMicros: 6_000_000}, nil, 0)
	tuner := &fakeTuner{submitErr: errors.New("dial tcp: connection refused")}

	_, err := bridge.SubmitGroup(ctx, params(t, st, guard, tuner, "all-in", 6_000_000, 500_000))
	if err == nil {
		t.Fatal("want an error from a failing Submit")
	}
	if tuner.submitCalls != 1 {
		t.Errorf("Submit called %d times, want exactly 1 (no retry)", tuner.submitCalls)
	}

	spent := guard.Spent()
	if spent != (budget.Spend{}) {
		t.Errorf("Guard.Spent() = %+v, want zero — a failed Submit settles nothing", spent)
	}
	settled, err := st.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if settled != (budget.Spend{}) {
		t.Errorf("store.SettledSpend() = %+v, want zero", settled)
	}

	jobs, err := st.TuningJobs(ctx, "run-1")
	if err != nil {
		t.Fatalf("TuningJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].State != store.TuningJobStateSubmitting {
		t.Fatalf("got %+v, want one row still in state submitting for a resume to find", jobs)
	}

	// The cap is now fully free again for a subsequent call — the guard was
	// not left holding a phantom reservation.
	if rem := guard.Remaining(); rem.CostUSDMicros != 6_000_000 {
		t.Errorf("Remaining().CostUSDMicros = %d, want the full cap restored", rem.CostUSDMicros)
	}
}
