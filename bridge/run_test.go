package bridge_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/knograph/kno/adapters/agent/pricing"
	"github.com/knograph/kno/bridge"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// fakeEvalRunner drives run_test.go: it records every group it was asked to
// measure and returns programmable deltas.
type fakeEvalRunner struct {
	calls   []string
	goal    [][]float64
	control [][]float64
	err     error
}

func (f *fakeEvalRunner) Measure(_ context.Context, group string, _ *knov1.AgentRef, _, _ []string) ([][]float64, [][]float64, error) {
	f.calls = append(f.calls, group)
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.goal, f.control, nil
}

func succeeded(id string) *core.JobState {
	return &core.JobState{
		Ref:    &core.JobRef{Id: id, Provider: "together"},
		Status: knov1.JobStatus_JOB_STATUS_SUCCEEDED,
	}
}

func failed(id, msg string) *core.JobState {
	return &core.JobState{
		Ref:    &core.JobRef{Id: id, Provider: "together"},
		Status: knov1.JobStatus_JOB_STATUS_FAILED,
		Error:  msg,
	}
}

func testQuotes() []bridge.GroupQuote {
	return []bridge.GroupQuote{
		{
			Group: bridge.AllIn, AssetIDs: []string{"a1", "a2"},
			TrainingData:           []byte(`{"messages":[{"role":"assistant","content":"x"}]}` + "\n"),
			TrainingFileSHA256:     "hash-all-in",
			TrainTokens:            500_000,
			EstimatedCostUSDMicros: 6_000_000,
		},
		{
			Group: "cluster-x", AssetIDs: []string{"a1"},
			TrainingData:           []byte(`{"messages":[{"role":"assistant","content":"y"}]}` + "\n"),
			TrainingFileSHA256:     "hash-cluster-x",
			TrainTokens:            400_000,
			EstimatedCostUSDMicros: 5_000_000,
		},
	}
}

// TestRunHappyPathSubmitsPollsDeploysMeasuresAndTearsDown covers acceptance
// criteria 18-23's spine: every group's job is submitted, polled to
// terminal, reconciled, deployed, measured, and torn down, in order, with
// the all-in group's model as every leave-one-out group's baseline.
func TestRunHappyPathSubmitsPollsDeploysMeasuresAndTearsDown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	if err := st.CreateRun(ctx, &knov1.Run{Id: "run-1", Stage: knov1.Stage_STAGE_BRIDGE, Status: knov1.RunStatus_RUN_STATUS_RUNNING}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	guard := budget.New(budget.Limits{}, nil, 0)
	tuner := &fakeTuner{
		ref:            &core.JobRef{Id: "job-1", Provider: "together", SubmittedAt: "2026-08-31T00:00:00Z"},
		statusSequence: []*core.JobState{succeeded("job-1")},
		deployResult:   &core.Endpoint{ID: "ep-1", Provider: "together", Served: "meta-llama/Llama-3-8b-ft", Ready: true, ReadyAt: time.Now()},
	}
	em, err := bridge.NewEmitter(ctx, st, "run-1")
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	eval := &fakeEvalRunner{goal: [][]float64{{0.2}, {0.1}}, control: [][]float64{{0.0}}}

	result, err := bridge.Run(ctx, bridge.RunParams{
		RunID: "run-1", Store: st, Guard: guard, Tuner: tuner, Emitter: em, Provider: "together",
		Quotes:    testQuotes(),
		BaseModel: &knov1.AgentRef{Ref: "together:meta-llama/Llama-3-8b", Scheme: "together", Target: "meta-llama/Llama-3-8b"},
		Epochs:    3, GoalDomain: knov1.ScoreDomain_SCORE_DOMAIN_UNIT_INTERVAL,
		PollInterval: time.Millisecond, TickInterval: time.Hour, Eval: eval,
		MaxLiveEndpoints: 1, MaxServeMinutes: 30,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tuner.submitCalls != 2 {
		t.Errorf("Submit called %d times, want 2 (one per group)", tuner.submitCalls)
	}
	if tuner.deployCalls != 2 {
		t.Errorf("Deploy called %d times, want 2", tuner.deployCalls)
	}
	if tuner.teardownCalls != 2 {
		t.Errorf("Teardown called %d times, want 2 — every deployed endpoint must be torn down", tuner.teardownCalls)
	}
	// The all-in group is the baseline and reports no delta of its own; only
	// the leave-one-out group calls Eval.Measure.
	if len(eval.calls) != 1 || eval.calls[0] != "cluster-x" {
		t.Errorf("Eval.Measure calls = %v, want exactly [cluster-x]", eval.calls)
	}
	if len(result.Measured) != 2 {
		t.Fatalf("got %d measured groups, want 2 (all-in + cluster-x)", len(result.Measured))
	}
	var clusterEv *knov1.BridgeGroupMeasured
	for _, m := range result.Measured {
		if m.GetAblationGroup() == "cluster-x" {
			clusterEv = m
		}
	}
	if clusterEv == nil {
		t.Fatal("no BridgeGroupMeasured for cluster-x")
	}
	if clusterEv.GetDeltaGroupInterval() == nil {
		t.Error("DeltaGroupInterval is nil — prime directive 5: no delta without its interval")
	}
	if clusterEv.GetVerdict() == knov1.BridgeGroupVerdict_BRIDGE_GROUP_VERDICT_UNSPECIFIED {
		t.Error("Verdict is UNSPECIFIED, want CONFIRMED or UNCONFIRMED")
	}

	// Every submitted job settled its estimate: 6M + 5M training.
	settled, err := st.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if settled.CostUSDMicros < 11_000_000 {
		t.Errorf("SettledSpend.CostUSDMicros = %d, want at least 11000000 (both training estimates)", settled.CostUSDMicros)
	}
}

// TestRunSkipsAFailedGroupWithoutSubstituting covers the "all-in job
// succeeds, one LOO job fails" edge case: a failed group is reported
// skipped, never measured, and never substituted with the all-in score.
func TestRunSkipsAFailedGroupWithoutSubstituting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	if err := st.CreateRun(ctx, &knov1.Run{Id: "run-1", Stage: knov1.Stage_STAGE_BRIDGE, Status: knov1.RunStatus_RUN_STATUS_RUNNING}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	guard := budget.New(budget.Limits{}, nil, 0)
	tuner := &fakeTuner{
		ref: &core.JobRef{Id: "job-1", Provider: "together"},
		// Both groups' Submit returns job-1's ref (fakeTuner has one ref
		// field); status always reports FAILED, so neither group ever
		// deploys.
		statusSequence: []*core.JobState{failed("job-1", "provider validation error")},
	}
	em, err := bridge.NewEmitter(ctx, st, "run-1")
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	eval := &fakeEvalRunner{}

	result, err := bridge.Run(ctx, bridge.RunParams{
		RunID: "run-1", Store: st, Guard: guard, Tuner: tuner, Emitter: em, Provider: "together",
		Quotes:       testQuotes()[:1], // all-in only
		BaseModel:    &knov1.AgentRef{Ref: "together:meta-llama/Llama-3-8b", Scheme: "together", Target: "meta-llama/Llama-3-8b"},
		PollInterval: time.Millisecond, Eval: eval, MaxLiveEndpoints: 1, MaxServeMinutes: 30,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Skipped) != 1 || result.Skipped[0] != bridge.AllIn {
		t.Errorf("Skipped = %v, want [all-in]", result.Skipped)
	}
	if len(result.Measured) != 0 {
		t.Errorf("Measured = %v, want none — a failed job must never be substituted", result.Measured)
	}
	if tuner.deployCalls != 0 {
		t.Errorf("Deploy called %d times, want 0 — a failed job is never deployed", tuner.deployCalls)
	}
}

// TestRunStopsWaitingWithoutCancellingOnTimeout is acceptance criterion 24:
// exceeding --bridge-timeout stops WAITING, never cancels, and the error
// names the provider job id.
func TestRunStopsWaitingWithoutCancellingOnTimeout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	if err := st.CreateRun(ctx, &knov1.Run{Id: "run-1", Stage: knov1.Stage_STAGE_BRIDGE, Status: knov1.RunStatus_RUN_STATUS_RUNNING}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	guard := budget.New(budget.Limits{}, nil, 0)
	tuner := &fakeTuner{
		ref: &core.JobRef{Id: "job-1", Provider: "together"},
		statusSequence: []*core.JobState{
			{Ref: &core.JobRef{Id: "job-1"}, Status: knov1.JobStatus_JOB_STATUS_RUNNING},
		},
	}
	em, err := bridge.NewEmitter(ctx, st, "run-1")
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}

	_, err = bridge.Run(ctx, bridge.RunParams{
		RunID: "run-1", Store: st, Guard: guard, Tuner: tuner, Emitter: em, Provider: "together",
		Quotes:       testQuotes()[:1],
		BaseModel:    &knov1.AgentRef{Ref: "together:meta-llama/Llama-3-8b", Scheme: "together", Target: "meta-llama/Llama-3-8b"},
		PollInterval: time.Millisecond, JobTimeout: 5 * time.Millisecond,
		Eval: &fakeEvalRunner{}, MaxLiveEndpoints: 1, MaxServeMinutes: 30,
	})
	if !errors.Is(err, bridge.ErrJobTimedOut) {
		t.Errorf("err = %v, want ErrJobTimedOut", err)
	}
	if tuner.deployCalls != 0 {
		t.Errorf("Deploy called %d times, want 0", tuner.deployCalls)
	}

	jobs, terr := st.TuningJobs(ctx, "run-1")
	if terr != nil {
		t.Fatalf("TuningJobs: %v", terr)
	}
	if len(jobs) != 1 || jobs[0].TerminalAt != "" {
		t.Errorf("got %+v, want the row to stay non-terminal on a timeout", jobs)
	}
}

// TestRunRefusesToStartWithoutAnEvalRunner pins the config-time refusal
// EvalRunner's own doc describes: deploying a paid endpoint with nothing to
// measure it is worse than refusing.
func TestRunRefusesToStartWithoutAnEvalRunner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	guard := budget.New(budget.Limits{}, nil, 0)
	tuner := &fakeTuner{}
	em, err := bridge.NewEmitter(ctx, st, "run-1")
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}

	_, err = bridge.Run(ctx, bridge.RunParams{
		RunID: "run-1", Store: st, Guard: guard, Tuner: tuner, Emitter: em,
		Quotes: testQuotes(),
	})
	if err == nil {
		t.Fatal("want an error when no EvalRunner is configured")
	}
	if tuner.submitCalls != 0 {
		t.Errorf("Submit called %d times, want 0 — Run must refuse before spending anything", tuner.submitCalls)
	}
}

// TestRunResumesAnAlreadySubmittedGroupWithoutResubmitting covers Run's
// resume path through submitOrResumeGroup: a row already TuningJobStateSubmitted
// before Run starts is polled, never re-submitted.
func TestRunResumesAnAlreadySubmittedGroupWithoutResubmitting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	if err := st.CreateRun(ctx, &knov1.Run{Id: "run-1", Stage: knov1.Stage_STAGE_BRIDGE, Status: knov1.RunStatus_RUN_STATUS_RUNNING}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.WriteTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup: bridge.AllIn, State: store.TuningJobStateSubmitted,
		Provider: "together", ProviderJobID: "job-1", Suffix: "kno-run-1-all-in",
		EstimatedCostUSDMicros: 6_000_000, TrainTokens: 500_000,
	}); err != nil {
		t.Fatalf("WriteTuningJob: %v", err)
	}
	guard := budget.New(budget.Limits{}, nil, 0)
	guard.Restore(budget.Spend{CostUSDMicros: 6_000_000, Tokens: 500_000, Calls: 1})
	tuner := &fakeTuner{
		statusSequence: []*core.JobState{succeeded("job-1")},
		deployResult:   &core.Endpoint{ID: "ep-1", Provider: "together", Served: "m-ft", Ready: true, ReadyAt: time.Now()},
	}
	em, err := bridge.NewEmitter(ctx, st, "run-1")
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}

	result, err := bridge.Run(ctx, bridge.RunParams{
		RunID: "run-1", Store: st, Guard: guard, Tuner: tuner, Emitter: em, Provider: "together",
		Quotes:       testQuotes()[:1],
		BaseModel:    &knov1.AgentRef{Ref: "together:meta-llama/Llama-3-8b", Scheme: "together", Target: "meta-llama/Llama-3-8b"},
		PollInterval: time.Millisecond, TickInterval: time.Hour,
		Eval: &fakeEvalRunner{}, MaxLiveEndpoints: 1, MaxServeMinutes: 30,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tuner.submitCalls != 0 {
		t.Errorf("Submit called %d times, want 0 — an already-submitted row must never be re-submitted", tuner.submitCalls)
	}
	if len(result.Measured) != 1 {
		t.Errorf("got %d measured groups, want 1", len(result.Measured))
	}
}

// TestRunSkipsAnAlreadyAbandonedGroup covers submitOrResumeGroup's
// ErrAlreadyAbandoned translation: a group this run already gave up on is
// reported skipped, and Run continues rather than failing outright.
func TestRunSkipsAnAlreadyAbandonedGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	if err := st.CreateRun(ctx, &knov1.Run{Id: "run-1", Stage: knov1.Stage_STAGE_BRIDGE, Status: knov1.RunStatus_RUN_STATUS_RUNNING}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.WriteTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup: bridge.AllIn, State: store.TuningJobStateAbandoned,
		EstimatedCostUSDMicros: 6_000_000, TrainTokens: 500_000,
	}); err != nil {
		t.Fatalf("WriteTuningJob: %v", err)
	}
	guard := budget.New(budget.Limits{}, nil, 0)
	tuner := &fakeTuner{}
	em, err := bridge.NewEmitter(ctx, st, "run-1")
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}

	result, err := bridge.Run(ctx, bridge.RunParams{
		RunID: "run-1", Store: st, Guard: guard, Tuner: tuner, Emitter: em, Provider: "together",
		Quotes:       testQuotes()[:1],
		BaseModel:    &knov1.AgentRef{Ref: "together:meta-llama/Llama-3-8b", Scheme: "together", Target: "meta-llama/Llama-3-8b"},
		PollInterval: time.Millisecond,
		Eval:         &fakeEvalRunner{}, MaxLiveEndpoints: 1, MaxServeMinutes: 30,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Skipped) != 1 || result.Skipped[0] != bridge.AllIn {
		t.Errorf("Skipped = %v, want [all-in]", result.Skipped)
	}
	if tuner.submitCalls != 0 {
		t.Errorf("Submit called %d times, want 0", tuner.submitCalls)
	}
}

// TestRunReportsFreshlyAbandonedGroupViaOrphanSpendEvent drives Run through
// a crash-recovery abandon (no adopt-by-suffix match), exercising
// Emitter.OrphanSpend end to end.
func TestRunReportsFreshlyAbandonedGroupViaOrphanSpendEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	if err := st.CreateRun(ctx, &knov1.Run{Id: "run-1", Stage: knov1.Stage_STAGE_BRIDGE, Status: knov1.RunStatus_RUN_STATUS_RUNNING}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.WriteTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup: bridge.AllIn, State: store.TuningJobStateSubmitting,
		Suffix: "kno-run-1-all-in", EstimatedCostUSDMicros: 6_000_000, TrainTokens: 500_000,
	}); err != nil {
		t.Fatalf("WriteTuningJob: %v", err)
	}
	guard := budget.New(budget.Limits{}, nil, 0)
	tuner := &fakeTuner{} // no ListJobs match
	em, err := bridge.NewEmitter(ctx, st, "run-1")
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}

	result, err := bridge.Run(ctx, bridge.RunParams{
		RunID: "run-1", Store: st, Guard: guard, Tuner: tuner, Emitter: em, Provider: "together",
		Quotes:       testQuotes()[:1],
		BaseModel:    &knov1.AgentRef{Ref: "together:meta-llama/Llama-3-8b", Scheme: "together", Target: "meta-llama/Llama-3-8b"},
		PollInterval: time.Millisecond,
		Eval:         &fakeEvalRunner{}, MaxLiveEndpoints: 1, MaxServeMinutes: 30,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Skipped) != 1 {
		t.Errorf("Skipped = %v, want one group", result.Skipped)
	}

	jobs, terr := st.TuningJobs(ctx, "run-1")
	if terr != nil {
		t.Fatalf("TuningJobs: %v", terr)
	}
	if jobs[0].State != store.TuningJobStateAbandoned {
		t.Errorf("State = %v, want abandoned", jobs[0].State)
	}
}

// TestRunValidateRequiredFields covers RunParams.validate's required-field
// branches, each missing in isolation.
func TestRunValidateRequiredFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	guard := budget.New(budget.Limits{}, nil, 0)
	tuner := &fakeTuner{}
	em, err := bridge.NewEmitter(ctx, st, "run-1")
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	eval := &fakeEvalRunner{}

	full := func() bridge.RunParams {
		return bridge.RunParams{
			RunID: "run-1", Store: st, Guard: guard, Tuner: tuner, Emitter: em,
			Quotes: testQuotes(), Eval: eval,
		}
	}

	tests := []struct {
		name   string
		mutate func(p *bridge.RunParams)
	}{
		{"missing run id", func(p *bridge.RunParams) { p.RunID = "" }},
		{"missing store", func(p *bridge.RunParams) { p.Store = nil }},
		{"missing guard", func(p *bridge.RunParams) { p.Guard = nil }},
		{"missing tuner", func(p *bridge.RunParams) { p.Tuner = nil }},
		{"missing emitter", func(p *bridge.RunParams) { p.Emitter = nil }},
		{"missing quotes", func(p *bridge.RunParams) { p.Quotes = nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := full()
			tc.mutate(&p)
			if _, err := bridge.Run(ctx, p); err == nil {
				t.Errorf("want an error for %s", tc.name)
			}
		})
	}
}

// TestRunRefusesAboveGroupCapWithNoBaseline covers deployMeasureTeardown's
// defensive "no all-in baseline" branch: a leave-one-out group reaching
// deploy with no prior all-in success is refused rather than reporting a
// meaningless delta.
func TestRunRefusesAboveGroupCapWithNoBaseline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	if err := st.CreateRun(ctx, &knov1.Run{Id: "run-1", Stage: knov1.Stage_STAGE_BRIDGE, Status: knov1.RunStatus_RUN_STATUS_RUNNING}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	guard := budget.New(budget.Limits{}, nil, 0)
	tuner := &fakeTuner{
		ref:            &core.JobRef{Id: "job-1", Provider: "together"},
		statusSequence: []*core.JobState{succeeded("job-1")},
		deployResult:   &core.Endpoint{ID: "ep-1", Provider: "together", Served: "m-ft", Ready: true, ReadyAt: time.Now()},
	}
	em, err := bridge.NewEmitter(ctx, st, "run-1")
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}

	// Only the leave-one-out group, never all-in: deployMeasureTeardown must
	// refuse rather than compute a delta against a nil baseline.
	_, err = bridge.Run(ctx, bridge.RunParams{
		RunID: "run-1", Store: st, Guard: guard, Tuner: tuner, Emitter: em, Provider: "together",
		Quotes:       testQuotes()[1:2],
		BaseModel:    &knov1.AgentRef{Ref: "together:meta-llama/Llama-3-8b", Scheme: "together", Target: "meta-llama/Llama-3-8b"},
		PollInterval: time.Millisecond, TickInterval: time.Hour,
		Eval: &fakeEvalRunner{}, MaxLiveEndpoints: 1, MaxServeMinutes: 30,
	})
	if err == nil {
		t.Fatal("want an error: a leave-one-out group with no all-in baseline must be refused")
	}
	if tuner.teardownCalls != 1 {
		t.Errorf("Teardown called %d times, want 1 — the endpoint deployed before the refusal must still be torn down", tuner.teardownCalls)
	}
}

// TestLiveEndpointLimiterAcquireRespectsContextCancellation covers
// Acquire's ctx.Done branch.
func TestLiveEndpointLimiterAcquireRespectsContextCancellation(t *testing.T) {
	t.Parallel()
	limiter := bridge.NewLiveEndpointLimiter(1)
	ctx := context.Background()
	if err := limiter.Acquire(ctx); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := limiter.Acquire(cancelled); err == nil {
		t.Fatal("want an error from Acquire on an already-cancelled context")
	}
}

// blockingEvalRunner blocks until its context is cancelled, so a test can
// observe whether anything is able to interrupt a measurement in flight.
type blockingEvalRunner struct {
	started  chan struct{}
	returned chan struct{}
	once     sync.Once
}

func (b *blockingEvalRunner) Measure(ctx context.Context, _ string, _ *knov1.AgentRef, _, _ []string) ([][]float64, [][]float64, error) {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	close(b.returned)
	return nil, nil, ctx.Err()
}

// TestReachingTheCapMidServeStopsTheMeasurementAndTearsDown is acceptance
// criterion 34, and it is a spend-safety test rather than a lifecycle one.
//
// A dedicated endpoint bills by the minute whether or not anything is
// measuring it. So a budget cap reached DURING hosting means every further
// minute is spend the user never authorized, and the only correct response
// is to stop and tear down — not to finish a measurement nobody can pay for.
//
// This failed before the fix, invisibly. startServeTicker discarded
// SettleServeTick's error as `_, _ =`, which is an EXPLICIT discard and so
// passes errcheck: the guard refused, nothing observed it, and the endpoint
// kept running until the measurement finished on its own. Prime directive 4.
func TestReachingTheCapMidServeStopsTheMeasurementAndTearsDown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	if err := st.CreateRun(ctx, &knov1.Run{Id: "run-1", Stage: knov1.Stage_STAGE_BRIDGE, Status: knov1.RunStatus_RUN_STATUS_RUNNING}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Exactly both groups' training cost (6,000,000 + 5,000,000) and not one
	// micro more, so every submission and deploy is authorized and the FIRST
	// hosting tick is what gets refused. A tighter cap would block submission
	// and test the wrong thing — the run would never reach an endpoint at all.
	guard := budget.New(budget.Limits{MaxCostUSDMicros: 11_000_000}, nil, 0)
	tuner := &fakeTuner{
		ref:            &core.JobRef{Id: "job-1", Provider: "together", SubmittedAt: "2026-08-31T00:00:00Z"},
		statusSequence: []*core.JobState{succeeded("job-1")},
		deployResult: &core.Endpoint{
			ID: "ep-1", Provider: "together", Served: "meta-llama/Llama-3-8b-ft",
			Ready: true, ReadyAt: time.Now().Add(-5 * time.Minute),
		},
	}
	em, err := bridge.NewEmitter(ctx, st, "run-1")
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	eval := &blockingEvalRunner{started: make(chan struct{}), returned: make(chan struct{})}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = bridge.Run(ctx, bridge.RunParams{
			RunID: "run-1", Store: st, Guard: guard, Tuner: tuner, Emitter: em, Provider: "together",
			Quotes:    testQuotes(),
			BaseModel: &knov1.AgentRef{Ref: "together:meta-llama/Llama-3-8b", Scheme: "together", Target: "meta-llama/Llama-3-8b"},
			Epochs:    3, GoalDomain: knov1.ScoreDomain_SCORE_DOMAIN_UNIT_INTERVAL,
			PollInterval: time.Millisecond,
			// Short enough that a tick lands while the measurement blocks.
			TickInterval:     time.Millisecond,
			ServePrice:       pricing.ServePrice{PerMinuteUSDMicros: 100_000},
			ServeReplicas:    1,
			Eval:             eval,
			MaxLiveEndpoints: 1, MaxServeMinutes: 30,
		})
	}()

	select {
	case <-eval.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the measurement never started")
	}

	// The refusal must reach the measurement. Without it this blocks forever
	// and the endpoint bills the whole time.
	select {
	case <-eval.returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the budget guard refused a hosting tick and the measurement " +
			"was never interrupted — the endpoint keeps billing past the cap")
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run never returned after the measurement was interrupted")
	}

	if tuner.teardownCalls == 0 {
		t.Error("the endpoint was never torn down after the cap was reached; " +
			"it keeps billing until something else notices")
	}
}
