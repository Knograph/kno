package bridge_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/knograph/kno/adapters/agent/pricing"
	"github.com/knograph/kno/bridge"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// deployedRun sets up a store with one submitted tuning job row for "run-1",
// group "all-in" — the state DeployGroup expects to find.
func deployedRun(t *testing.T, st store.Store) {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateRun(ctx, &knov1.Run{Id: "run-1", Stage: knov1.Stage_STAGE_BRIDGE, Status: knov1.RunStatus_RUN_STATUS_RUNNING}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.WriteTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup:          "all-in",
		State:                  store.TuningJobStateSubmitted,
		Provider:               "together",
		ProviderJobID:          "job-1",
		Suffix:                 "kno-run-1-all-in",
		EstimatedCostUSDMicros: 6_000_000,
	}); err != nil {
		t.Fatalf("WriteTuningJob: %v", err)
	}
}

// TestDeployGroupRecordsWriteAheadThenEndpointID covers the write-ahead
// discipline DeployGroup's doc describes: DeployedAt is recorded before
// Deploy is called, EndpointID only after it succeeds.
func TestDeployGroupRecordsWriteAheadThenEndpointID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	deployedRun(t, st)

	// ReadyAt is set because a ready Endpoint without one has no serve-minutes
	// deadline and no hosting ticker — DeployGroup refuses it.
	tuner := &fakeTuner{deployResult: &core.Endpoint{
		ID: "ep-1", Provider: "together", Ready: true, ReadyAt: time.Now(),
	}}
	ep, err := bridge.DeployGroup(ctx, bridge.DeployParams{
		RunID: "run-1", AblationGroup: "all-in",
		Store: st, Tuner: tuner, Ref: &core.JobRef{Id: "job-1"},
	})
	if err != nil {
		t.Fatalf("DeployGroup: %v", err)
	}
	if ep.ID != "ep-1" {
		t.Errorf("Endpoint.ID = %q, want ep-1", ep.ID)
	}

	jobs, err := st.TuningJobs(ctx, "run-1")
	if err != nil {
		t.Fatalf("TuningJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d rows, want 1", len(jobs))
	}
	if jobs[0].DeployedAt == "" {
		t.Error("DeployedAt is empty, want a timestamp recorded before Deploy was called")
	}
	if jobs[0].EndpointID == nil || *jobs[0].EndpointID != "ep-1" {
		t.Errorf("EndpointID = %v, want ep-1", jobs[0].EndpointID)
	}
}

// TestDeployGroupLeavesEndpointIDUnsetOnFailure pins the half of the
// write-ahead contract a Deploy failure exercises: DeployedAt stays, but no
// EndpointID is recorded — the exact shape SweepEndpoints's ListEndpoints
// fallback exists to disambiguate.
func TestDeployGroupLeavesEndpointIDUnsetOnFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	deployedRun(t, st)

	tuner := &fakeTuner{deployErr: errors.New("provider unavailable")}
	if _, err := bridge.DeployGroup(ctx, bridge.DeployParams{
		RunID: "run-1", AblationGroup: "all-in",
		Store: st, Tuner: tuner, Ref: &core.JobRef{Id: "job-1"},
	}); err == nil {
		t.Fatal("want an error from a failing Deploy")
	}

	jobs, err := st.TuningJobs(ctx, "run-1")
	if err != nil {
		t.Fatalf("TuningJobs: %v", err)
	}
	if jobs[0].DeployedAt == "" {
		t.Error("DeployedAt is empty, want the write-ahead timestamp to survive a Deploy failure")
	}
	if jobs[0].EndpointID != nil {
		t.Errorf("EndpointID = %v, want nil — Deploy never confirmed success", jobs[0].EndpointID)
	}
}

// TestSettleServeTickSettlesWholeMinutesOnce is acceptance criterion 32: with
// a fake clock advanced 7 minutes, Guard.Spent() and store.SettledSpend each
// report 7 minutes at the serve rate before Teardown is called, and a
// process killed at minute 4 leaves 4 minutes settled in the durable row.
func TestSettleServeTickSettlesWholeMinutesOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	deployedRun(t, st)
	guard := budget.New(budget.Limits{}, nil, 0)
	price := pricing.ServePrice{PerMinuteUSDMicros: 100_000} // $0.10/replica/minute
	readyAt := time.Now()

	// Kill at minute 4.
	delta, err := bridge.SettleServeTick(ctx, bridge.SettleServeParams{
		RunID: "run-1", AblationGroup: "all-in", Store: st, Guard: guard,
		Price: price, Replicas: 1, ReadyAt: readyAt, Now: readyAt.Add(4 * time.Minute),
	})
	if err != nil {
		t.Fatalf("SettleServeTick: %v", err)
	}
	if delta != 4 {
		t.Errorf("minutesSettled = %d, want 4", delta)
	}
	spent := guard.Spent()
	if spent.CostUSDMicros != 400_000 {
		t.Errorf("Guard.Spent().CostUSDMicros = %d, want 400000 (4 minutes at $0.10)", spent.CostUSDMicros)
	}
	settled, err := st.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	// deployedRun's row also carries a 6,000,000 training estimate, settled
	// and counted regardless of hosting — SettledSpend sums every dimension.
	if settled.CostUSDMicros != 6_400_000 {
		t.Errorf("store.SettledSpend().CostUSDMicros = %d, want 6400000 (6M training + 400k hosting)", settled.CostUSDMicros)
	}

	// Resume/continue: advance to minute 7 and tick again. Only the DELTA
	// (3 more minutes) is settled, never the running total re-priced.
	delta, err = bridge.SettleServeTick(ctx, bridge.SettleServeParams{
		RunID: "run-1", AblationGroup: "all-in", Store: st, Guard: guard,
		Price: price, Replicas: 1, ReadyAt: readyAt, Now: readyAt.Add(7 * time.Minute),
	})
	if err != nil {
		t.Fatalf("second SettleServeTick: %v", err)
	}
	if delta != 3 {
		t.Errorf("minutesSettled = %d, want 3 (the delta, not the running total)", delta)
	}
	spent = guard.Spent()
	if spent.CostUSDMicros != 700_000 {
		t.Errorf("Guard.Spent().CostUSDMicros = %d, want 700000 (7 minutes total)", spent.CostUSDMicros)
	}
}

// TestSettleServeTickIsANoOpWithinTheSameMinute guards against a tight
// polling loop re-authorizing zero-delta ticks.
func TestSettleServeTickIsANoOpWithinTheSameMinute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	deployedRun(t, st)
	guard := budget.New(budget.Limits{}, nil, 0)
	readyAt := time.Now()

	delta, err := bridge.SettleServeTick(ctx, bridge.SettleServeParams{
		RunID: "run-1", AblationGroup: "all-in", Store: st, Guard: guard,
		Price: pricing.ServePrice{PerMinuteUSDMicros: 100_000}, Replicas: 1,
		ReadyAt: readyAt, Now: readyAt.Add(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("SettleServeTick: %v", err)
	}
	if delta != 0 {
		t.Errorf("minutesSettled = %d, want 0 (under one whole minute elapsed)", delta)
	}
	if spent := guard.Spent(); spent != (budget.Spend{}) {
		t.Errorf("Guard.Spent() = %+v, want zero", spent)
	}
}

// TestTeardownGroupRecordsTornDownAt covers the success path.
func TestTeardownGroupRecordsTornDownAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	deployedRun(t, st)
	ep := &core.Endpoint{ID: "ep-1", Provider: "together"}
	id := "ep-1"
	if err := st.UpdateTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup: "all-in", State: store.TuningJobStateSubmitted,
		Provider: "together", ProviderJobID: "job-1", EndpointID: &id,
	}); err != nil {
		t.Fatalf("UpdateTuningJob: %v", err)
	}

	tuner := &fakeTuner{}
	if err := bridge.TeardownGroup(ctx, bridge.TeardownParams{
		RunID: "run-1", AblationGroup: "all-in", Store: st, Tuner: tuner, Endpoint: ep,
	}); err != nil {
		t.Fatalf("TeardownGroup: %v", err)
	}
	if tuner.teardownCalls != 1 {
		t.Errorf("Teardown called %d times, want 1", tuner.teardownCalls)
	}
	jobs, err := st.TuningJobs(ctx, "run-1")
	if err != nil {
		t.Fatalf("TuningJobs: %v", err)
	}
	if jobs[0].TornDownAt == nil {
		t.Error("TornDownAt is nil, want a timestamp")
	}
}

// TestTeardownGroupFailureIsNeverSwallowed is acceptance criterion 31's
// failure half: a Teardown that errors fails the call, and the row keeps its
// EndpointID with a nil TornDownAt — exactly what kno doctor and the sweep
// both look for.
func TestTeardownGroupFailureIsNeverSwallowed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	deployedRun(t, st)
	id := "ep-1"
	if err := st.UpdateTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup: "all-in", State: store.TuningJobStateSubmitted,
		Provider: "together", ProviderJobID: "job-1", EndpointID: &id,
	}); err != nil {
		t.Fatalf("UpdateTuningJob: %v", err)
	}

	tuner := &fakeTuner{teardownErr: errors.New("provider unreachable")}
	err := bridge.TeardownGroup(ctx, bridge.TeardownParams{
		RunID: "run-1", AblationGroup: "all-in", Store: st, Tuner: tuner,
		Endpoint: &core.Endpoint{ID: "ep-1", Provider: "together"},
	})
	if err == nil {
		t.Fatal("want an error from a failing Teardown — it must never be swallowed")
	}

	jobs, err := st.TuningJobs(ctx, "run-1")
	if err != nil {
		t.Fatalf("TuningJobs: %v", err)
	}
	if jobs[0].EndpointID == nil || *jobs[0].EndpointID != "ep-1" {
		t.Error("EndpointID lost after a failed Teardown, want it to survive so the row still names the leak")
	}
	if jobs[0].TornDownAt != nil {
		t.Error("TornDownAt set after a failed Teardown, want nil — the endpoint may still be live")
	}
}

// TestLiveEndpointLimiterCapsConcurrency is acceptance criterion 30: at most
// max endpoints are live at any instant — asserted here at the limiter
// level directly, which is what any concurrent caller (present or future)
// composes against.
func TestLiveEndpointLimiterCapsConcurrency(t *testing.T) {
	t.Parallel()
	limiter := bridge.NewLiveEndpointLimiter(1)
	ctx := context.Background()

	if err := limiter.Acquire(ctx); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		_ = limiter.Acquire(ctx)
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("second Acquire returned before the first Release — the cap did not hold")
	case <-time.After(50 * time.Millisecond):
		// Expected: the second Acquire is blocked.
	}

	limiter.Release()
	select {
	case <-acquired:
		// Expected: releasing the first slot frees the second Acquire.
	case <-time.After(time.Second):
		t.Fatal("second Acquire never returned after Release")
	}
	limiter.Release()
}

// TestSweepEndpointsTearsDownAKnownLiveEndpoint is acceptance criterion 33's
// primary case: a row with a recorded EndpointID and no TornDownAt is torn
// down on sweep, and the minutes since the last settled tick are recorded
// as an overshoot.
func TestSweepEndpointsTearsDownAKnownLiveEndpoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	deployedRun(t, st)
	id := "ep-1"
	if err := st.UpdateTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup: "all-in", State: store.TuningJobStateSubmitted,
		Provider: "together", ProviderJobID: "job-1", Suffix: "kno-run-1-all-in",
		EndpointID: &id, DeployedAt: "2026-08-31T00:00:00Z",
		ServeMinutes: 4, ServeCostUSDMicros: 400_000, // 4 minutes settled at $0.10/min
	}); err != nil {
		t.Fatalf("UpdateTuningJob: %v", err)
	}

	guard := budget.New(budget.Limits{}, nil, 0)
	guard.Restore(budget.Spend{CostUSDMicros: 400_000})
	em, err := bridge.NewEmitter(ctx, st, "run-1")
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	tuner := &fakeTuner{}

	results, err := bridge.SweepEndpoints(ctx, st, tuner, guard, em, "run-1", 30)
	if err != nil {
		t.Fatalf("SweepEndpoints: %v", err)
	}
	if tuner.teardownCalls != 1 {
		t.Errorf("Teardown called %d times, want 1", tuner.teardownCalls)
	}
	if len(results) != 1 || !results[0].TornDown {
		t.Fatalf("got %+v, want one torn-down result", results)
	}
	// maxServeMinutes=30, 4 already settled: 26 more minutes swept at the
	// row's own $0.10/min rate (400000/4) = 2,600,000.
	if results[0].OvershootUSDMicros != 2_600_000 {
		t.Errorf("OvershootUSDMicros = %d, want 2600000", results[0].OvershootUSDMicros)
	}

	jobs, err := st.TuningJobs(ctx, "run-1")
	if err != nil {
		t.Fatalf("TuningJobs: %v", err)
	}
	if jobs[0].TornDownAt == nil {
		t.Error("TornDownAt is nil after sweep, want a timestamp")
	}

	settled, err := st.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if settled.CostUSDMicros != 3_000_000 { // 400k already settled + 2.6M orphan
		t.Errorf("SettledSpend.CostUSDMicros = %d, want 3000000", settled.CostUSDMicros)
	}
}

// TestSweepEndpointsResolvesAnUnrecordedEndpointByListing covers the
// crash-during-deploy window: EndpointID is nil but DeployedAt is set, so
// the sweep must ask the provider via ListEndpoints rather than assuming
// nothing was created.
func TestSweepEndpointsResolvesAnUnrecordedEndpointByListing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	deployedRun(t, st)
	if err := st.UpdateTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup: "all-in", State: store.TuningJobStateSubmitted,
		Provider: "together", ProviderJobID: "job-1", Suffix: "kno-run-1-all-in",
		DeployedAt: "2026-08-31T00:00:00Z", // EndpointID nil: the ambiguous window
	}); err != nil {
		t.Fatalf("UpdateTuningJob: %v", err)
	}

	guard := budget.New(budget.Limits{}, nil, 0)
	em, err := bridge.NewEmitter(ctx, st, "run-1")
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	tuner := &fakeTuner{listEndpointsResult: []*core.Endpoint{{ID: "ep-found", Provider: "together"}}}

	results, err := bridge.SweepEndpoints(ctx, st, tuner, guard, em, "run-1", 30)
	if err != nil {
		t.Fatalf("SweepEndpoints: %v", err)
	}
	if tuner.listEndpointsCalls != 1 {
		t.Errorf("ListEndpoints called %d times, want 1", tuner.listEndpointsCalls)
	}
	if tuner.teardownCalls != 1 {
		t.Errorf("Teardown called %d times, want 1", tuner.teardownCalls)
	}
	if len(results) != 1 || !results[0].TornDown {
		t.Fatalf("got %+v, want one torn-down result", results)
	}

	jobs, err := st.TuningJobs(ctx, "run-1")
	if err != nil {
		t.Fatalf("TuningJobs: %v", err)
	}
	if jobs[0].EndpointID == nil || *jobs[0].EndpointID != "ep-found" {
		t.Errorf("EndpointID = %v, want ep-found recorded from the listing", jobs[0].EndpointID)
	}
}

// TestSweepEndpointsSettlesAtTheCapWhenNothingIsListed covers the "resume
// finds a run's endpoint gone" edge case: no match from ListEndpoints
// settles at maxServeMinutes, the conservative bound, rather than assuming
// zero extra minutes were billed.
func TestSweepEndpointsSettlesAtTheCapWhenNothingIsListed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	deployedRun(t, st)
	if err := st.UpdateTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup: "all-in", State: store.TuningJobStateSubmitted,
		Provider: "together", ProviderJobID: "job-1", Suffix: "kno-run-1-all-in",
		DeployedAt: "2026-08-31T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpdateTuningJob: %v", err)
	}

	guard := budget.New(budget.Limits{}, nil, 0)
	em, err := bridge.NewEmitter(ctx, st, "run-1")
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	tuner := &fakeTuner{} // listEndpointsResult nil: nothing found

	results, err := bridge.SweepEndpoints(ctx, st, tuner, guard, em, "run-1", 30)
	if err != nil {
		t.Fatalf("SweepEndpoints: %v", err)
	}
	if tuner.teardownCalls != 0 {
		t.Errorf("Teardown called %d times, want 0 — nothing to tear down", tuner.teardownCalls)
	}
	if len(results) != 1 || results[0].TornDown {
		t.Fatalf("got %+v, want one NOT-torn-down result", results)
	}

	jobs, err := st.TuningJobs(ctx, "run-1")
	if err != nil {
		t.Fatalf("TuningJobs: %v", err)
	}
	if jobs[0].ServeMinutes != 30 {
		t.Errorf("ServeMinutes = %d, want 30 (settled at the cap)", jobs[0].ServeMinutes)
	}
}

// TestSweepEndpointsSkipsAlreadyTornDownRows guards against re-sweeping a
// row that already confirmed teardown.
func TestSweepEndpointsSkipsAlreadyTornDownRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	deployedRun(t, st)
	id, tornDown := "ep-1", "2026-08-31T01:00:00Z"
	if err := st.UpdateTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup: "all-in", State: store.TuningJobStateSubmitted,
		Provider: "together", ProviderJobID: "job-1",
		EndpointID: &id, TornDownAt: &tornDown,
	}); err != nil {
		t.Fatalf("UpdateTuningJob: %v", err)
	}

	guard := budget.New(budget.Limits{}, nil, 0)
	em, err := bridge.NewEmitter(ctx, st, "run-1")
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	tuner := &fakeTuner{}

	results, err := bridge.SweepEndpoints(ctx, st, tuner, guard, em, "run-1", 30)
	if err != nil {
		t.Fatalf("SweepEndpoints: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("got %d results, want 0 — an already-torn-down row needs no sweep", len(results))
	}
	if tuner.teardownCalls != 0 || tuner.listEndpointsCalls != 0 {
		t.Errorf("tuner called (teardown=%d, listEndpoints=%d), want zero calls", tuner.teardownCalls, tuner.listEndpointsCalls)
	}
}

// TestDeployRefusesAReadyEndpointWithNoReadyAt pins the precondition two
// safety mechanisms are silently guarded on.
//
// The serve-minutes deadline (bridge/run.go) only attaches when ReadyAt is
// non-zero, and SettleServeTick returns (0, nil) when it is zero. Those are
// the ONLY two bounds on a live measurement — one by time, one by cost — so a
// ready Endpoint with a zero ReadyAt runs with neither, and neither path says
// it gave up.
//
// This is the shape core.Tuner.Deploy's own doc invites: it tells a provider
// that auto-serves to implement Deploy as "a no-op returning a zero-rate
// Endpoint", and the natural way to write that is
// `return &core.Endpoint{Ready: true}, nil`, which sets no ReadyAt. The
// interface's escape hatch leads straight to the unbounded case, so the check
// lives where every adapter passes rather than in each adapter's memory.
func TestDeployRefusesAReadyEndpointWithNoReadyAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	deployedRun(t, st)

	tuner := &fakeTuner{deployResult: &core.Endpoint{
		ID: "ep-1", Provider: "together", Ready: true, // no ReadyAt
	}}
	_, err := bridge.DeployGroup(ctx, bridge.DeployParams{
		RunID: "run-1", AblationGroup: "all-in", Store: st, Tuner: tuner,
		Ref: &core.JobRef{Id: "job-1", Provider: "together"},
	})
	if err == nil {
		t.Fatal("DeployGroup accepted a ready Endpoint with no ReadyAt; that " +
			"endpoint runs with no serve-minutes deadline and no hosting ticker, " +
			"so nothing bounds it by time or by cost")
	}
	if !strings.Contains(err.Error(), "ReadyAt") {
		t.Errorf("the refusal does not name ReadyAt: %v", err)
	}
}
