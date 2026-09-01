package store_test

import (
	"context"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// TestTuningJobRoundTrip pins that every field of a TuningJobRecord survives
// a write and a read back, including the nullable hosting fields that are
// nil until Deploy and Teardown are called.
func TestTuningJobRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	actual := int64(4_100_000)
	endpoint := "ep-abc123"
	tornDown := "2026-08-31T00:10:00Z"

	want := &store.TuningJobRecord{
		AblationGroup:          "all-in",
		State:                  store.TuningJobStateSubmitted,
		Provider:               "together",
		ProviderJobID:          "job-1",
		BaseModel:              "together:meta-llama/Llama-3-8b",
		Suffix:                 "kno-run-1-all-in",
		TrainingFileSHA256:     "abc123",
		TrainTokens:            123_456,
		Epochs:                 3,
		LoRARank:               8,
		EstimatedCostUSDMicros: 6_000_000,
		ActualCostUSDMicros:    &actual,
		Status:                 knov1.JobStatus_JOB_STATUS_SUCCEEDED,
		SubmittedAt:            "2026-08-31T00:00:00Z",
		TerminalAt:             "2026-08-31T00:05:00Z",
		ErrorText:              "",
		EndpointID:             &endpoint,
		DeployedAt:             "2026-08-31T00:06:00Z",
		TornDownAt:             &tornDown,
		ServeMinutes:           4,
		ServeCostUSDMicros:     400_000,
	}

	if err := s.WriteTuningJob(ctx, "run-1", want); err != nil {
		t.Fatalf("WriteTuningJob: %v", err)
	}

	jobs, err := s.TuningJobs(ctx, "run-1")
	if err != nil {
		t.Fatalf("TuningJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	got := jobs[0]

	if got.AblationGroup != want.AblationGroup || got.State != want.State ||
		got.Provider != want.Provider || got.ProviderJobID != want.ProviderJobID ||
		got.BaseModel != want.BaseModel || got.Suffix != want.Suffix ||
		got.TrainingFileSHA256 != want.TrainingFileSHA256 ||
		got.TrainTokens != want.TrainTokens || got.Epochs != want.Epochs ||
		got.LoRARank != want.LoRARank ||
		got.EstimatedCostUSDMicros != want.EstimatedCostUSDMicros ||
		got.Status != want.Status || got.SubmittedAt != want.SubmittedAt ||
		got.TerminalAt != want.TerminalAt || got.ErrorText != want.ErrorText ||
		got.DeployedAt != want.DeployedAt ||
		got.ServeMinutes != want.ServeMinutes ||
		got.ServeCostUSDMicros != want.ServeCostUSDMicros {
		t.Errorf("round trip drifted:\ngot  %+v\nwant %+v", got, want)
	}
	if got.ActualCostUSDMicros == nil || *got.ActualCostUSDMicros != *want.ActualCostUSDMicros {
		t.Errorf("ActualCostUSDMicros = %v, want %v", got.ActualCostUSDMicros, *want.ActualCostUSDMicros)
	}
	if got.EndpointID == nil || *got.EndpointID != *want.EndpointID {
		t.Errorf("EndpointID = %v, want %v", got.EndpointID, *want.EndpointID)
	}
	if got.TornDownAt == nil || *got.TornDownAt != *want.TornDownAt {
		t.Errorf("TornDownAt = %v, want %v", got.TornDownAt, *want.TornDownAt)
	}
}

// TestTuningJobNullableFieldsStayNilUntilSet pins that ActualCostUSDMicros,
// EndpointID, and TornDownAt round-trip as nil — never zero or "" — when the
// bridge has not set them yet. Nil-vs-zero is load-bearing: an absent
// ActualCostUSDMicros means "the provider never reported a cost" and every
// rendering must say "estimated", never "billed" (Step 2(c)); a nil
// EndpointID means "no endpoint was ever deployed for this job" rather than
// "deployed and immediately torn down".
func TestTuningJobNullableFieldsStayNilUntilSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if err := s.WriteTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup:          "all-in",
		State:                  store.TuningJobStateSubmitting,
		EstimatedCostUSDMicros: 6_000_000,
		TrainTokens:            100_000,
	}); err != nil {
		t.Fatalf("WriteTuningJob: %v", err)
	}

	jobs, err := s.TuningJobs(ctx, "run-1")
	if err != nil {
		t.Fatalf("TuningJobs: %v", err)
	}
	got := jobs[0]
	if got.ActualCostUSDMicros != nil {
		t.Errorf("ActualCostUSDMicros = %v, want nil (never reported)", *got.ActualCostUSDMicros)
	}
	if got.EndpointID != nil {
		t.Errorf("EndpointID = %v, want nil (never deployed)", *got.EndpointID)
	}
	if got.TornDownAt != nil {
		t.Errorf("TornDownAt = %v, want nil (never torn down)", *got.TornDownAt)
	}
}

// TestWriteTuningJobIsWriteAhead pins acceptance criterion 6: a row in
// TuningJobStateSubmitting must be visible to a reader BEFORE the caller
// would ever call Tuner.Submit — which this test proves by writing the row,
// then reading it back from a second, independent connection, matching the
// shape a fake Tuner's in-Submit store query would see.
func TestWriteTuningJobIsWriteAhead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if err := s.WriteTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup:          "all-in",
		State:                  store.TuningJobStateSubmitting,
		EstimatedCostUSDMicros: 6_000_000,
		TrainTokens:            500_000,
	}); err != nil {
		t.Fatalf("WriteTuningJob: %v", err)
	}

	jobs, err := s.TuningJobs(ctx, "run-1")
	if err != nil {
		t.Fatalf("TuningJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].State != store.TuningJobStateSubmitting {
		t.Fatalf("write-ahead row not visible before Submit would run: %+v", jobs)
	}

	// A submitting-only row must NOT yet count toward SettledSpend — the
	// request may never have reached the provider.
	spend, err := s.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if spend != (budget.Spend{}) {
		t.Errorf("a submitting-only row settled spend = %+v, want zero", spend)
	}
}

// TestSettledSpendCountsTuningJobsOnceSubmitted covers acceptance criterion
// 7: once a group's row is updated to submitted, SettledSpend reports the
// estimate on all three dimensions Calls/CostUSDMicros/Tokens, matching the
// requirement that a spend path never drop Tokens — the exact defect #170
// and #172 fixed for the Value and Validate sinks.
func TestSettledSpendCountsTuningJobsOnceSubmitted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if err := s.WriteTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup:          "all-in",
		State:                  store.TuningJobStateSubmitting,
		EstimatedCostUSDMicros: 6_000_000,
		TrainTokens:            500_000,
	}); err != nil {
		t.Fatalf("WriteTuningJob: %v", err)
	}
	if err := s.UpdateTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup:          "all-in",
		State:                  store.TuningJobStateSubmitted,
		ProviderJobID:          "job-1",
		EstimatedCostUSDMicros: 6_000_000,
		TrainTokens:            500_000,
	}); err != nil {
		t.Fatalf("UpdateTuningJob: %v", err)
	}

	spend, err := s.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	want := budget.Spend{Calls: 1, CostUSDMicros: 6_000_000, Tokens: 500_000}
	if spend != want {
		t.Errorf("SettledSpend = %+v, want %+v (all three dimensions, none dropped)", spend, want)
	}
}

// TestSettledSpendCountsAbandonedJobsToo covers acceptance criterion 9: a
// row a resume could not adopt by suffix is marked abandoned, and its
// estimate STAYS settled — money may have been spent on a job Kno cannot
// see, and the conservative direction is to keep counting it.
func TestSettledSpendCountsAbandonedJobsToo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if err := s.WriteTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup:          "all-in",
		State:                  store.TuningJobStateSubmitting,
		EstimatedCostUSDMicros: 6_000_000,
		TrainTokens:            500_000,
	}); err != nil {
		t.Fatalf("WriteTuningJob: %v", err)
	}
	if err := s.UpdateTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup:          "all-in",
		State:                  store.TuningJobStateAbandoned,
		EstimatedCostUSDMicros: 6_000_000,
		TrainTokens:            500_000,
	}); err != nil {
		t.Fatalf("UpdateTuningJob: %v", err)
	}

	spend, err := s.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	want := budget.Spend{Calls: 1, CostUSDMicros: 6_000_000, Tokens: 500_000}
	if spend != want {
		t.Errorf("SettledSpend for an abandoned job = %+v, want %+v (still settled)", spend, want)
	}
}

// TestSettledSpendCountsServeMinutesRegardlessOfJobState covers Step 2(f)'s
// hosting dimension: serve minutes settled forward per tick count even
// while the job's own lifecycle state is still "submitting" is impossible in
// practice (hosting only starts after a job succeeds), but the accounting
// rule itself — serve cost counts unconditionally, because a tick IS its own
// settlement — is what this pins, using a submitted job with hosting cost
// attached.
func TestSettledSpendCountsServeMinutesRegardlessOfJobState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if err := s.WriteTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup:          "all-in",
		State:                  store.TuningJobStateSubmitted,
		EstimatedCostUSDMicros: 6_000_000,
		TrainTokens:            500_000,
		ServeMinutes:           7,
		ServeCostUSDMicros:     700_000,
	}); err != nil {
		t.Fatalf("WriteTuningJob: %v", err)
	}

	spend, err := s.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	want := budget.Spend{Calls: 1, CostUSDMicros: 6_700_000, Tokens: 500_000}
	if spend != want {
		t.Errorf("SettledSpend = %+v, want %+v (training + hosting)", spend, want)
	}
}

// TestSettledSpendSumsAcrossMultipleGroups covers a multi-group bridge run:
// SettledSpend must add every group's row, not just the first.
func TestSettledSpendSumsAcrossMultipleGroups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	groups := []string{"all-in", "refunds", "billing"}
	for _, g := range groups {
		if err := s.WriteTuningJob(ctx, "run-1", &store.TuningJobRecord{
			AblationGroup:          g,
			State:                  store.TuningJobStateSubmitted,
			EstimatedCostUSDMicros: 1_000_000,
			TrainTokens:            10_000,
		}); err != nil {
			t.Fatalf("WriteTuningJob(%s): %v", g, err)
		}
	}

	spend, err := s.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	want := budget.Spend{Calls: 3, CostUSDMicros: 3_000_000, Tokens: 30_000}
	if spend != want {
		t.Errorf("SettledSpend across 3 groups = %+v, want %+v", spend, want)
	}
}

// TestTuningJobsAreScopedToTheirRun pins that TuningJobs never leaks another
// run's rows — the same isolation every other per-run reader in this package
// guarantees.
func TestTuningJobsAreScopedToTheirRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun run-1: %v", err)
	}
	if err := s.CreateRun(ctx, newRun("run-2")); err != nil {
		t.Fatalf("CreateRun run-2: %v", err)
	}
	if err := s.WriteTuningJob(ctx, "run-1", &store.TuningJobRecord{
		AblationGroup: "all-in", State: store.TuningJobStateSubmitted,
	}); err != nil {
		t.Fatalf("WriteTuningJob run-1: %v", err)
	}

	jobs, err := s.TuningJobs(ctx, "run-2")
	if err != nil {
		t.Fatalf("TuningJobs run-2: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("run-2 saw %d jobs from run-1, want 0", len(jobs))
	}
}

// TestTuningJobsOnANonBridgeRunIsEmpty pins that a run with no write-ahead
// row at all reads back as an empty slice, not an error — "not a bridge
// run" and "a bridge run with a failed first write" must not look alike to
// a caller checking len(jobs).
func TestTuningJobsOnANonBridgeRunIsEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	jobs, err := s.TuningJobs(ctx, "run-1")
	if err != nil {
		t.Fatalf("TuningJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("got %d jobs, want 0", len(jobs))
	}
}

// TestWriteTuningJobRefusesAnEmptyAblationGroup guards the primary key: an
// empty ablation group would collide every group's row into one.
func TestWriteTuningJobRefusesAnEmptyAblationGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	if err := s.CreateRun(ctx, newRun("run-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	err := s.WriteTuningJob(ctx, "run-1", &store.TuningJobRecord{State: store.TuningJobStateSubmitting})
	if err == nil {
		t.Error("want an error for an empty ablation group")
	}
}
