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

// measureCall records one Eval.Measure invocation: which group, and
// exactly which Case IDs it was asked to score.
type measureCall struct {
	group   string
	caseIDs []string
}

// fakeEvalRunner drives run_test.go against the NEW Case-ID-keyed
// EvalRunner contract: Measure(ctx, group, model, caseIDs) -> scores.
//
// scores is group -> caseID -> score. A group with no entry falls back to
// the "*" wildcard group, which lets most tests supply one score table
// that both the all-in union pass and a leave-one-out pass can read from
// (a Case's all-in score and its leave-one-out score are different
// numbers in reality, so tests that care about the DIFFERENCE set scores
// per group explicitly instead).
type fakeEvalRunner struct {
	mu     sync.Mutex
	calls  []measureCall
	scores map[string]map[string]float64
	err    error
	// errGroup limits err to one group's calls; empty means every call
	// errors.
	errGroup string
}

func (f *fakeEvalRunner) Measure(_ context.Context, group string, _ *knov1.AgentRef, caseIDs []string) (map[string]float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, measureCall{group: group, caseIDs: append([]string(nil), caseIDs...)})
	if f.err != nil && (f.errGroup == "" || f.errGroup == group) {
		return nil, f.err
	}
	table := f.scores[group]
	if table == nil {
		table = f.scores["*"]
	}
	out := map[string]float64{}
	for _, id := range caseIDs {
		if s, ok := table[id]; ok {
			out[id] = s
		}
	}
	return out, nil
}

func (f *fakeEvalRunner) groupsCalled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.group
	}
	return out
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

// devCaseIDs and controlCaseIDs are the fixture every measurement test
// below shares: three dev Cases for cluster-x, three reserved control
// Cases — enough pairs for interval.Paired and interval.HarmBound to both
// produce a non-nil interval (n >= 2).
func devCaseIDs() map[string][]string {
	return map[string][]string{"cluster-x": {"d1", "d2", "d3"}}
}

func controlCaseIDs() []string { return []string{"ctl1", "ctl2", "ctl3"} }

// helpfulGroupScores is a score table where leaving cluster-x's Assets out
// measurably HURTS the dev Cases they were meant to help (a lower
// leave-one-out score than all-in) and changes nothing on the control
// partition — the CONFIRMED shape, no interference.
func helpfulGroupScores() map[string]map[string]float64 {
	return map[string]map[string]float64{
		bridge.AllIn: {
			"d1": 0.9, "d2": 0.8, "d3": 0.7,
			"ctl1": 0.6, "ctl2": 0.65, "ctl3": 0.55,
		},
		"cluster-x": {
			"d1": 0.5, "d2": 0.4, "d3": 0.3,
			"ctl1": 0.6, "ctl2": 0.65, "ctl3": 0.55,
		},
	}
}

func baseRunParams(st store.Store, guard *budget.Guard, tuner core.Tuner, em *bridge.Emitter, eval bridge.EvalRunner) bridge.RunParams {
	return bridge.RunParams{
		RunID: "run-1", Store: st, Guard: guard, Tuner: tuner, Emitter: em, Provider: "together",
		Quotes:       testQuotes(),
		BaseModel:    &knov1.AgentRef{Ref: "together:meta-llama/Llama-3-8b", Scheme: "together", Target: "meta-llama/Llama-3-8b"},
		Epochs:       3,
		GoalDomain:   knov1.ScoreDomain_SCORE_DOMAIN_UNIT_INTERVAL,
		PollInterval: time.Millisecond, TickInterval: time.Hour,
		Eval: eval, MaxLiveEndpoints: 1, MaxServeMinutes: 30,
		DevCaseIDs: devCaseIDs(), ControlCaseIDs: controlCaseIDs(),
		NGroups: 1,
	}
}

// TestRunHappyPathSubmitsPollsDeploysMeasuresAndTearsDown covers acceptance
// criteria 18-23's spine, updated for the Case-ID-keyed seam: every group's
// job is submitted, polled to terminal, reconciled, deployed, measured
// (including the all-in group's own union pass — the piece the pre-seam
// build never did), and torn down, in order, with the all-in group's
// per-Case scores durably recorded as every leave-one-out group's
// baseline.
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
	eval := &fakeEvalRunner{scores: helpfulGroupScores()}

	result, err := bridge.Run(ctx, baseRunParams(st, guard, tuner, em, eval))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tuner.submitCalls != 2 {
		t.Errorf("Submit called %d times, want 2 (one per group)", tuner.submitCalls)
	}
	if tuner.deployCalls != 2 {
		t.Errorf("Deploy called %d times, want 2 — the all-in group must be deployed and scored too", tuner.deployCalls)
	}
	if tuner.teardownCalls != 2 {
		t.Errorf("Teardown called %d times, want 2 — every deployed endpoint must be torn down", tuner.teardownCalls)
	}
	gotGroups := eval.groupsCalled()
	if len(gotGroups) != 2 || gotGroups[0] != bridge.AllIn || gotGroups[1] != "cluster-x" {
		t.Errorf("Eval.Measure calls = %v, want [all-in cluster-x] — the all-in union pass must run", gotGroups)
	}

	// The all-in group has no verdict of its own: only cluster-x appears.
	if len(result.Measured) != 1 {
		t.Fatalf("got %d measured groups, want 1 (cluster-x only; all-in has no verdict)", len(result.Measured))
	}
	clusterEv := result.Measured[0]
	if clusterEv.GetAblationGroup() != "cluster-x" {
		t.Fatalf("measured group = %s, want cluster-x", clusterEv.GetAblationGroup())
	}
	if clusterEv.GetDeltaGroupInterval() == nil {
		t.Error("DeltaGroupInterval is nil — prime directive 5: no delta without its interval")
	}
	if clusterEv.GetVerdict() != knov1.BridgeGroupVerdict_BRIDGE_GROUP_VERDICT_CONFIRMED {
		t.Errorf("Verdict = %v, want CONFIRMED (leaving the group out measurably hurt its own dev Cases)", clusterEv.GetVerdict())
	}
	if clusterEv.GetDeltaGroup() <= 0 {
		t.Errorf("DeltaGroup = %v, want positive (all-in scored higher than leave-one-out on its own Cases)", clusterEv.GetDeltaGroup())
	}

	// Every submitted job settled its estimate: 6M + 5M training.
	settled, err := st.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if settled.CostUSDMicros < 11_000_000 {
		t.Errorf("SettledSpend.CostUSDMicros = %d, want at least 11000000 (both training estimates)", settled.CostUSDMicros)
	}

	// The all-in group's per-Case scores are durably recorded — never only
	// in memory (the eval-seam plan's §3: "in-memory is not sufficient").
	recs, err := st.Measurements(ctx, "run-1", bridge.AllIn)
	if err != nil {
		t.Fatalf("Measurements(all-in): %v", err)
	}
	if len(recs) != 6 {
		t.Errorf("all-in has %d recorded measurements, want 6 (3 dev + 3 control)", len(recs))
	}

	// The verdict was marked emitted, so a second call to Run for the same
	// run ID would not re-measure or re-emit it (see the resume tests
	// below).
	jobs, err := st.TuningJobs(ctx, "run-1")
	if err != nil {
		t.Fatalf("TuningJobs: %v", err)
	}
	for _, j := range jobs {
		if j.AblationGroup == "cluster-x" && j.VerdictEmittedAt == "" {
			t.Error("cluster-x's VerdictEmittedAt was never set")
		}
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

	p := baseRunParams(st, guard, tuner, em, eval)
	p.Quotes = testQuotes()[:1] // all-in only
	result, err := bridge.Run(ctx, p)
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
	if len(eval.calls) != 0 {
		t.Errorf("Eval.Measure called %d times, want 0", len(eval.calls))
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

	p := baseRunParams(st, guard, tuner, em, &fakeEvalRunner{})
	p.Quotes = testQuotes()[:1]
	p.JobTimeout = 5 * time.Millisecond
	_, err = bridge.Run(ctx, p)
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

// TestRunResumesAnAlreadyMeasuredAllInGroupWithoutRemeasuring covers the
// all-in group's own resume idempotency: once its union pass has fully
// scored every needed Case, a resumed Run must not redeploy or re-invoke
// Eval.Measure for it.
func TestRunResumesAnAlreadyMeasuredAllInGroupWithoutRemeasuring(t *testing.T) {
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
	// Pre-populate every Case the all-in union pass needs, as if a prior
	// process measured them all before crashing on the way to cluster-x.
	scores := helpfulGroupScores()
	for id, score := range scores[bridge.AllIn] {
		if err := st.RecordMeasurement(ctx, "run-1", &store.Measurement{
			Key:   store.MeasurementKey{AssetID: bridge.AllIn, CaseID: id, Arm: store.ArmTreatment, Trial: 1},
			Score: &knov1.Score{Value: score, Passed: score > 0},
		}); err != nil {
			t.Fatalf("seeding all-in measurement %s: %v", id, err)
		}
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
	eval := &fakeEvalRunner{scores: scores}

	p := baseRunParams(st, guard, tuner, em, eval)
	p.Quotes = testQuotes()[:1] // all-in only
	result, err := bridge.Run(ctx, p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tuner.submitCalls != 0 {
		t.Errorf("Submit called %d times, want 0 — an already-submitted row must never be re-submitted", tuner.submitCalls)
	}
	if tuner.deployCalls != 0 {
		t.Errorf("Deploy called %d times, want 0 — a fully-scored group must not be redeployed", tuner.deployCalls)
	}
	if len(eval.calls) != 0 {
		t.Errorf("Eval.Measure called %d times, want 0 — every Case is already durably scored", len(eval.calls))
	}
	if len(result.Measured) != 0 {
		t.Errorf("Measured = %v, want none — the all-in group has no verdict of its own", result.Measured)
	}
}

// TestRunRecomputesAndEmitsALostVerdictWithoutRemeasuring is the plan's
// §6 blocker fix in its sharpest form: a leave-one-out group whose Cases
// are ALL durably scored, but whose BridgeGroupMeasured event was never
// recorded (a crash between finishing the measurement and appending the
// event), must have its verdict RECOMPUTED FROM STORED SCORES and
// emitted on resume — never re-measured (no re-pay), and never silently
// dropped (no lost verdict).
func TestRunRecomputesAndEmitsALostVerdictWithoutRemeasuring(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	if err := st.CreateRun(ctx, &knov1.Run{Id: "run-1", Stage: knov1.Stage_STAGE_BRIDGE, Status: knov1.RunStatus_RUN_STATUS_RUNNING}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	scores := helpfulGroupScores()
	seed := func(group string, table map[string]float64) {
		for id, score := range table {
			if err := st.RecordMeasurement(ctx, "run-1", &store.Measurement{
				Key:   store.MeasurementKey{AssetID: group, CaseID: id, Arm: store.ArmTreatment, Trial: 1},
				Score: &knov1.Score{Value: score, Passed: score > 0},
			}); err != nil {
				t.Fatalf("seeding %s measurement %s: %v", group, id, err)
			}
		}
	}
	seed(bridge.AllIn, scores[bridge.AllIn])
	seed("cluster-x", scores["cluster-x"])

	if err := st.WriteTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup: bridge.AllIn, State: store.TuningJobStateSubmitted,
		Provider: "together", ProviderJobID: "job-all-in", Suffix: "kno-run-1-all-in",
		EstimatedCostUSDMicros: 6_000_000, TrainTokens: 500_000,
	}); err != nil {
		t.Fatalf("WriteTuningJob(all-in): %v", err)
	}
	// cluster-x's row is fully scored (seeded above) but VerdictEmittedAt
	// is empty — exactly the crash window §6 describes.
	if err := st.WriteTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup: "cluster-x", State: store.TuningJobStateSubmitted,
		Provider: "together", ProviderJobID: "job-cluster-x", Suffix: "kno-run-1-cluster-x",
		EstimatedCostUSDMicros: 5_000_000, TrainTokens: 400_000,
	}); err != nil {
		t.Fatalf("WriteTuningJob(cluster-x): %v", err)
	}

	guard := budget.New(budget.Limits{}, nil, 0)
	guard.Restore(budget.Spend{CostUSDMicros: 11_000_000, Tokens: 900_000, Calls: 2})
	tuner := &fakeTuner{
		statusSequence: []*core.JobState{succeeded("job-all-in"), succeeded("job-cluster-x")},
		deployResult:   &core.Endpoint{ID: "ep-1", Provider: "together", Served: "m-ft", Ready: true, ReadyAt: time.Now()},
	}
	em, err := bridge.NewEmitter(ctx, st, "run-1")
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	eval := &fakeEvalRunner{scores: scores}

	result, err := bridge.Run(ctx, baseRunParams(st, guard, tuner, em, eval))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tuner.deployCalls != 0 {
		t.Errorf("Deploy called %d times, want 0 — every Case was already durably scored", tuner.deployCalls)
	}
	if len(eval.calls) != 0 {
		t.Errorf("Eval.Measure called %d times, want 0 — nothing needed re-measuring", len(eval.calls))
	}
	if len(result.Measured) != 1 {
		t.Fatalf("got %d measured groups, want 1 (the recomputed cluster-x verdict)", len(result.Measured))
	}
	if result.Measured[0].GetVerdict() != knov1.BridgeGroupVerdict_BRIDGE_GROUP_VERDICT_CONFIRMED {
		t.Errorf("Verdict = %v, want CONFIRMED", result.Measured[0].GetVerdict())
	}

	jobs, err := st.TuningJobs(ctx, "run-1")
	if err != nil {
		t.Fatalf("TuningJobs: %v", err)
	}
	for _, j := range jobs {
		if j.AblationGroup == "cluster-x" && j.VerdictEmittedAt == "" {
			t.Error("cluster-x's VerdictEmittedAt should be set after the recomputed verdict was emitted")
		}
	}
}

// TestRunNeverReEmitsAnAlreadyReportedVerdict is the other half of §6: a
// group whose verdict was ALREADY emitted (VerdictEmittedAt set) must
// never be measured or emitted again on a subsequent Run call for the
// same run ID — two independently-sampled verdicts for one group is
// exactly what the resume marker exists to prevent.
func TestRunNeverReEmitsAnAlreadyReportedVerdict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	if err := st.CreateRun(ctx, &knov1.Run{Id: "run-1", Stage: knov1.Stage_STAGE_BRIDGE, Status: knov1.RunStatus_RUN_STATUS_RUNNING}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	scores := helpfulGroupScores()
	seed := func(group string, table map[string]float64) {
		for id, score := range table {
			if err := st.RecordMeasurement(ctx, "run-1", &store.Measurement{
				Key:   store.MeasurementKey{AssetID: group, CaseID: id, Arm: store.ArmTreatment, Trial: 1},
				Score: &knov1.Score{Value: score, Passed: score > 0},
			}); err != nil {
				t.Fatalf("seeding %s measurement %s: %v", group, id, err)
			}
		}
	}
	seed(bridge.AllIn, scores[bridge.AllIn])
	seed("cluster-x", scores["cluster-x"])

	if err := st.WriteTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup: bridge.AllIn, State: store.TuningJobStateSubmitted,
		Provider: "together", ProviderJobID: "job-all-in", Suffix: "kno-run-1-all-in",
	}); err != nil {
		t.Fatalf("WriteTuningJob(all-in): %v", err)
	}
	if err := st.WriteTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup: "cluster-x", State: store.TuningJobStateSubmitted,
		Provider: "together", ProviderJobID: "job-cluster-x", Suffix: "kno-run-1-cluster-x",
		VerdictEmittedAt: "2026-09-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("WriteTuningJob(cluster-x): %v", err)
	}

	guard := budget.New(budget.Limits{}, nil, 0)
	tuner := &fakeTuner{
		statusSequence: []*core.JobState{succeeded("job-all-in"), succeeded("job-cluster-x")},
	}
	em, err := bridge.NewEmitter(ctx, st, "run-1")
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	eval := &fakeEvalRunner{scores: scores}

	result, err := bridge.Run(ctx, baseRunParams(st, guard, tuner, em, eval))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tuner.deployCalls != 0 {
		t.Errorf("Deploy called %d times, want 0", tuner.deployCalls)
	}
	if len(eval.calls) != 0 {
		t.Errorf("Eval.Measure called %d times, want 0", len(eval.calls))
	}
	// The already-reported group is still surfaced in THIS process's
	// result (recomputed from store) — asserted above via Measured's
	// length and via zero Deploy/Eval.Measure calls. That the event was
	// not RE-EMITTED is structural, not incidental: measureGroup's
	// already-reported branch calls recomputeVerdict directly and never
	// reaches emitVerdict (the only path that calls
	// Emitter.GroupMeasured) — see bridge/run.go.
	if len(result.Measured) != 1 {
		t.Fatalf("got %d measured groups, want 1 (recomputed for this call's result, not re-emitted)", len(result.Measured))
	}
}

// TestRunEmitsInterferenceWhenTheNetEffectExcludesZeroBelow drives a
// fixture where the group's own dev Cases show no measurable help but the
// control partition regresses hard when the group is included — the
// interference read BRIDGE_GROUP_VERDICT_INTERFERENCE exists for, and
// which #184 shipped without ever reaching (see EvalRunner's history in
// this file's package doc).
func TestRunEmitsInterferenceWhenTheNetEffectExcludesZeroBelow(t *testing.T) {
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
		deployResult:   &core.Endpoint{ID: "ep-1", Provider: "together", Served: "m-ft", Ready: true, ReadyAt: time.Now()},
	}
	em, err := bridge.NewEmitter(ctx, st, "run-1")
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	// Dev Cases: all-in and leave-one-out score nearly identically (no
	// measurable transfer either way — small per-Case noise so the
	// interval is a real Student-t interval, not the degenerate
	// distribution-free fallback). Control Cases: leave-one-out scores
	// are markedly and consistently HIGHER than all-in — training on this
	// group regressed the control partition, an interference signal wide
	// enough that its net effect excludes zero.
	scores := map[string]map[string]float64{
		bridge.AllIn: {
			"d1": 0.50, "d2": 0.52, "d3": 0.48,
			"ctl1": 0.10, "ctl2": 0.12, "ctl3": 0.08,
		},
		"cluster-x": {
			"d1": 0.51, "d2": 0.50, "d3": 0.49,
			"ctl1": 0.97, "ctl2": 0.90, "ctl3": 0.99,
		},
	}
	eval := &fakeEvalRunner{scores: scores}

	result, err := bridge.Run(ctx, baseRunParams(st, guard, tuner, em, eval))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Measured) != 1 {
		t.Fatalf("got %d measured groups, want 1", len(result.Measured))
	}
	ev := result.Measured[0]
	if ev.GetVerdict() != knov1.BridgeGroupVerdict_BRIDGE_GROUP_VERDICT_INTERFERENCE {
		t.Errorf("Verdict = %v, want INTERFERENCE (delta_group=%v CI=%v, delta_control=%v CI=%v)",
			ev.GetVerdict(), ev.GetDeltaGroup(), ev.GetDeltaGroupInterval(), ev.GetDeltaControl(), ev.GetDeltaControlInterval())
	}
	if ev.GetDeltaControlInterval() == nil {
		t.Error("DeltaControlInterval is nil — an interference verdict must still carry its control interval")
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

	// Only the leave-one-out group, never all-in: emitVerdict must refuse
	// rather than compute a delta against a nonexistent baseline.
	p := baseRunParams(st, guard, tuner, em, &fakeEvalRunner{scores: helpfulGroupScores()})
	p.Quotes = testQuotes()[1:2]
	_, err = bridge.Run(ctx, p)
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

// blockAfterAllIn answers the all-in group normally and then hangs, so a test
// can reach a leave-one-out measurement with a real baseline behind it.
type blockAfterAllIn struct {
	scores   map[string]float64
	started  chan struct{}
	returned chan struct{}
	once     sync.Once
}

func (b *blockAfterAllIn) Measure(ctx context.Context, group string, _ *knov1.AgentRef, ids []string) (map[string]float64, error) {
	if group == bridge.AllIn {
		out := make(map[string]float64, len(ids))
		for _, id := range ids {
			out[id] = b.scores[id]
		}
		return out, nil
	}
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	close(b.returned)
	return nil, ctx.Err()
}

func (b *blockingEvalRunner) Measure(ctx context.Context, _ string, _ *knov1.AgentRef, _ []string) (map[string]float64, error) {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	close(b.returned)
	return nil, ctx.Err()
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

	// Exactly the all-in group's training cost (6,000,000) and not one
	// micro more, so submission and deploy are authorized and the FIRST
	// hosting tick is what gets refused. A tighter cap would block
	// submission and test the wrong thing — the run would never reach an
	// endpoint at all. Only the all-in group is quoted: since this build
	// measures the all-in group's own union pass too (unlike the pre-seam
	// build, which deployed and immediately tore it down with no
	// measurement), the all-in group alone is enough to reach a live,
	// billing endpoint under a blocking Eval.Measure.
	guard := budget.New(budget.Limits{MaxCostUSDMicros: 6_000_000}, nil, 0)
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

	p := baseRunParams(st, guard, tuner, em, eval)
	p.Quotes = testQuotes()[:1] // all-in only
	// Short enough that a tick lands while the measurement blocks.
	p.TickInterval = time.Millisecond
	p.ServePrice = pricing.ServePrice{PerMinuteUSDMicros: 100_000}
	p.ServeReplicas = 1

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = bridge.Run(ctx, p)
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

// TestServeMinutesCapStopsAHungMeasurement is the flag doing what its help
// says, and it is a spend-safety test rather than a lifecycle one.
//
// `--bridge-max-serve-minutes` is documented as "tear an endpoint down and
// report its group unknown after this many served minutes." It reached only
// SweepEndpoints, which runs on a LATER invocation cleaning up after a crash;
// nothing bounded a live run. A measurement that hung billed the endpoint for
// as long as it hung, with --max-cost-usd defaulting to 0 (unlimited) as the
// only other bound.
//
// A zero-rate provider makes it strictly worse: SettleServeTick settles zero
// every tick, the guard never refuses a zero estimate, and the cost-based
// backstop disappears. A clock is the only bound that does not depend on the
// endpoint costing something.
func TestServeMinutesCapStopsAHungMeasurement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	if err := st.CreateRun(ctx, &knov1.Run{Id: "run-1", Stage: knov1.Stage_STAGE_BRIDGE, Status: knov1.RunStatus_RUN_STATUS_RUNNING}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	em, err := bridge.NewEmitter(ctx, st, "run-1")
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}

	// ReadyAt already past the cap, so the deadline has expired before the
	// measurement starts: no sleeping in a test to prove a timeout fires.
	tuner := &fakeTuner{
		ref:            &core.JobRef{Id: "job-1", Provider: "together", SubmittedAt: "2026-08-31T00:00:00Z"},
		statusSequence: []*core.JobState{succeeded("job-1")},
		deployResult: &core.Endpoint{
			ID: "ep-1", Provider: "together", Served: "meta-llama/Llama-3-8b-ft",
			Ready: true, ReadyAt: time.Now().Add(-90 * time.Minute),
		},
	}
	eval := &blockAfterAllIn{
		scores:   map[string]float64{},
		started:  make(chan struct{}),
		returned: make(chan struct{}),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = bridge.Run(ctx, bridge.RunParams{
			RunID: "run-1", Store: st, Guard: budget.New(budget.Limits{}, nil, 0),
			Tuner: tuner, Emitter: em, Provider: "together",
			Quotes:    testQuotes(),
			BaseModel: &knov1.AgentRef{Ref: "together:meta-llama/Llama-3-8b", Scheme: "together", Target: "meta-llama/Llama-3-8b"},
			Epochs:    3, GoalDomain: knov1.ScoreDomain_SCORE_DOMAIN_UNIT_INTERVAL,
			PollInterval: time.Millisecond, TickInterval: time.Hour,
			// A zero serve price: the guard can never refuse, so the clock is
			// the only thing that can stop this.
			ServePrice: pricing.ServePrice{PerMinuteUSDMicros: 0},
			Eval:       eval,
			// 30 minutes, against an endpoint ready 90 minutes ago.
			MaxLiveEndpoints: 1, MaxServeMinutes: 30,
		})
	}()

	select {
	case <-eval.returned:
	case <-time.After(15 * time.Second):
		t.Fatal("the serve-minutes cap did not stop a hung measurement; the " +
			"endpoint bills for as long as the measurement hangs, and at a zero " +
			"serve rate no cost cap can stop it either")
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run never returned after the cap expired")
	}
	if tuner.teardownCalls == 0 {
		t.Error("the endpoint was never torn down after the serve-minutes cap expired")
	}
}
