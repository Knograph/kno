package bridge_test

import (
	"context"
	"testing"

	"github.com/knograph/kno/bridge"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

func submittedRecord(t *testing.T, st store.Store, group string, estimate int64) *store.TuningJobRecord {
	t.Helper()
	rec := &store.TuningJobRecord{
		AblationGroup:          group,
		State:                  store.TuningJobStateSubmitted,
		ProviderJobID:          "job-1",
		EstimatedCostUSDMicros: estimate,
		TrainTokens:            500_000,
	}
	if err := st.WriteTuningJob(context.Background(), "run-1", rec); err != nil {
		t.Fatalf("WriteTuningJob: %v", err)
	}
	return rec
}

// TestReconcileOvershootRecordsTheDeltaAndRestoresTheGuard is acceptance
// criterion 10's overshoot half: actual above the estimate records the
// delta through RecordOrphanSpend, and Guard.Overshoot() reads non-zero by
// exactly that delta.
func TestReconcileOvershootRecordsTheDeltaAndRestoresTheGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	if err := st.CreateRun(ctx, &knov1.Run{Id: "run-1", Stage: knov1.Stage_STAGE_BRIDGE, Status: knov1.RunStatus_RUN_STATUS_RUNNING}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// A cap equal to the estimate: Guard.Overshoot() only reports spend
	// beyond a CAP (it reads zero with no cap set at all), so the cap here
	// is what makes the overshoot assertion below meaningful.
	guard := budget.New(budget.Limits{MaxCostUSDMicros: 6_000_000}, nil, 0)
	guard.Restore(budget.Spend{Calls: 1, CostUSDMicros: 6_000_000, Tokens: 500_000}) // as if just settled at submission

	rec := submittedRecord(t, st, "all-in", 6_000_000)

	delta, err := bridge.ReconcileTerminal(ctx, st, guard, "run-1", rec, &core.JobState{
		Status:              knov1.JobStatus_JOB_STATUS_SUCCEEDED,
		ActualCostUsdMicros: 7_400_000,
	})
	if err != nil {
		t.Fatalf("ReconcileTerminal: %v", err)
	}
	if delta != 1_400_000 {
		t.Errorf("delta = %d, want 1400000 (7.40 - 6.00)", delta)
	}
	if got := guard.Overshoot(); got != 1_400_000 {
		t.Errorf("Guard.Overshoot() = %d, want 1400000", got)
	}

	settled, err := st.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if settled.CostUSDMicros != 7_400_000 {
		t.Errorf("SettledSpend.CostUSDMicros = %d, want 7400000 (estimate + orphan overshoot)", settled.CostUSDMicros)
	}

	jobs, err := st.TuningJobs(ctx, "run-1")
	if err != nil {
		t.Fatalf("TuningJobs: %v", err)
	}
	if jobs[0].ActualCostUSDMicros == nil || *jobs[0].ActualCostUSDMicros != 7_400_000 {
		t.Errorf("ActualCostUSDMicros = %v, want 7400000", jobs[0].ActualCostUSDMicros)
	}
	if jobs[0].Status != knov1.JobStatus_JOB_STATUS_SUCCEEDED {
		t.Errorf("Status = %v, want SUCCEEDED", jobs[0].Status)
	}
}

// TestReconcileUnderrunRecordsNothing is acceptance criterion 10's underrun
// half: actual below the estimate leaves Spent() unchanged and records no
// negative or credited spend.
func TestReconcileUnderrunRecordsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	if err := st.CreateRun(ctx, &knov1.Run{Id: "run-1", Stage: knov1.Stage_STAGE_BRIDGE, Status: knov1.RunStatus_RUN_STATUS_RUNNING}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	guard := budget.New(budget.Limits{}, nil, 0)
	guard.Restore(budget.Spend{Calls: 1, CostUSDMicros: 6_000_000, Tokens: 500_000})
	before := guard.Spent()

	rec := submittedRecord(t, st, "all-in", 6_000_000)

	delta, err := bridge.ReconcileTerminal(ctx, st, guard, "run-1", rec, &core.JobState{
		Status:              knov1.JobStatus_JOB_STATUS_SUCCEEDED,
		ActualCostUsdMicros: 4_100_000, // cheaper than feared
	})
	if err != nil {
		t.Fatalf("ReconcileTerminal: %v", err)
	}
	if delta != 0 {
		t.Errorf("delta = %d, want 0 (no refund)", delta)
	}
	after := guard.Spent()
	if after != before {
		t.Errorf("Guard.Spent() changed on an underrun: before %+v after %+v, want unchanged", before, after)
	}

	jobs, err := st.TuningJobs(ctx, "run-1")
	if err != nil {
		t.Fatalf("TuningJobs: %v", err)
	}
	if jobs[0].ActualCostUSDMicros == nil || *jobs[0].ActualCostUSDMicros != 4_100_000 {
		t.Errorf("ActualCostUSDMicros = %v, want 4100000 (recorded for reporting, even though not settled)", jobs[0].ActualCostUSDMicros)
	}
}

// TestReconcileAbsentActualLeavesTheEstimateStanding covers "actual absent
// or zero": the estimate stands, nothing is recorded as an actual, and no
// rendering can call it "billed".
func TestReconcileAbsentActualLeavesTheEstimateStanding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	if err := st.CreateRun(ctx, &knov1.Run{Id: "run-1", Stage: knov1.Stage_STAGE_BRIDGE, Status: knov1.RunStatus_RUN_STATUS_RUNNING}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	guard := budget.New(budget.Limits{}, nil, 0)
	guard.Restore(budget.Spend{Calls: 1, CostUSDMicros: 6_000_000, Tokens: 500_000})

	rec := submittedRecord(t, st, "all-in", 6_000_000)

	delta, err := bridge.ReconcileTerminal(ctx, st, guard, "run-1", rec, &core.JobState{
		Status: knov1.JobStatus_JOB_STATUS_SUCCEEDED,
		// ActualCostUsdMicros left unset: the provider reported no cost.
	})
	if err != nil {
		t.Fatalf("ReconcileTerminal: %v", err)
	}
	if delta != 0 {
		t.Errorf("delta = %d, want 0", delta)
	}

	settled, err := st.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if settled.CostUSDMicros != 6_000_000 {
		t.Errorf("SettledSpend.CostUSDMicros = %d, want 6000000 (the estimate stands)", settled.CostUSDMicros)
	}

	jobs, err := st.TuningJobs(ctx, "run-1")
	if err != nil {
		t.Fatalf("TuningJobs: %v", err)
	}
	if jobs[0].ActualCostUSDMicros != nil {
		t.Errorf("ActualCostUSDMicros = %v, want nil — never zeroed, never guessed", *jobs[0].ActualCostUSDMicros)
	}
}

// TestReconcileRecordsTheProviderErrorVerbatim pins that a FAILED job's
// error text is carried onto the durable row unmodified.
func TestReconcileRecordsTheProviderErrorVerbatim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newBridgeTestStore(t)
	if err := st.CreateRun(ctx, &knov1.Run{Id: "run-1", Stage: knov1.Stage_STAGE_BRIDGE, Status: knov1.RunStatus_RUN_STATUS_RUNNING}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	guard := budget.New(budget.Limits{}, nil, 0)
	rec := submittedRecord(t, st, "all-in", 6_000_000)

	const wantErr = "training file failed validation: line 4 has no assistant message"
	if _, err := bridge.ReconcileTerminal(ctx, st, guard, "run-1", rec, &core.JobState{
		Status: knov1.JobStatus_JOB_STATUS_FAILED,
		Error:  wantErr,
	}); err != nil {
		t.Fatalf("ReconcileTerminal: %v", err)
	}

	jobs, err := st.TuningJobs(ctx, "run-1")
	if err != nil {
		t.Fatalf("TuningJobs: %v", err)
	}
	if jobs[0].ErrorText != wantErr {
		t.Errorf("ErrorText = %q, want %q verbatim", jobs[0].ErrorText, wantErr)
	}
	if jobs[0].Status != knov1.JobStatus_JOB_STATUS_FAILED {
		t.Errorf("Status = %v, want FAILED", jobs[0].Status)
	}
}
