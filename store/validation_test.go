package store_test

import (
	"context"
	"errors"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/store"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/google/go-cmp/cmp"
)

// validateRun records a Validate run so the two new tables have something to
// reference.
func validateRun(t *testing.T, s *store.SQLite, runID string) {
	t.Helper()
	require.NoError(t, s.CreateRun(context.Background(), &knov1.Run{
		Id: runID, Stage: knov1.Stage_STAGE_VALIDATE,
		Status: knov1.RunStatus_RUN_STATUS_COMPLETED, GoalName: "g",
	}))
}

// sampleValidation is a finished holdout measurement.
func sampleValidation(runID string) *knov1.Validation {
	gain := 0.085
	control, treatment := 0.60, 0.685
	return &knov1.Validation{
		RunId:                runID,
		SelectRunId:          "sel-1",
		ValueRunId:           "val-1",
		BaselineRunId:        "base-1",
		HoldoutCaseCount:     34,
		MeasuredCaseCount:    33,
		NDropped:             1,
		HoldoutUnderpowered:  false,
		ControlScore:         &control,
		TreatmentScore:       &treatment,
		HoldoutGain:          &gain,
		HoldoutInterval:      &knov1.Interval{Low: 0.021, High: 0.149, Level: 0.95},
		DevEstimatedGain:     0.142,
		DevEstimatedInterval: &knov1.Interval{Low: 0.091, High: 0.193, Level: 0.95},
		Verdict:              knov1.ValidationVerdict_VALIDATION_VERDICT_CONFIRMED,
		Trials:               1,
		HoldoutUseIndex:      1,
	}
}

// TestValidationRoundTrip: the record a consumer loads is the one the stage
// decided, including the presence of the optional gain.
func TestValidationRoundTrip(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	validateRun(t, s, "vd-1")
	want := sampleValidation("vd-1")
	require.NoError(t, s.WriteValidation(context.Background(), "vd-1", want))

	got, err := s.Validation(context.Background(), "vd-1")
	require.NoError(t, err)
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("the Validation did not survive the round trip (-want +got):\n%s", diff)
	}
	// Presence, not value: a Validation with no interval must read back with
	// NO gain rather than a gain of zero, which is the whole reason the field
	// is optional.
	if got.HoldoutGain == nil {
		t.Error("the gain lost its presence across the round trip")
	}
}

// TestValidationAbsenceIsAnAnswer.
//
// ErrValidationNotFound is what makes `kno report` keep the not-yet-validated
// caveat for a run that stopped early: "Validate never finished on this run"
// is not "Validate ran and could not form an interval", and the page renders
// them differently.
func TestValidationAbsenceIsAnAnswer(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	_, err := s.Validation(context.Background(), "no-such-run")
	if !errors.Is(err, store.ErrValidationNotFound) {
		t.Errorf("Validation(missing) = %v, want ErrValidationNotFound", err)
	}
}

// TestValidationRewriteReplaces: a resumed run that reaches the end recomputes
// over BOTH processes' measurements, so the row must match the current numbers
// rather than pinning the first, partial answer.
func TestValidationRewriteReplaces(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	validateRun(t, s, "vd-1")
	first := sampleValidation("vd-1")
	require.NoError(t, s.WriteValidation(context.Background(), "vd-1", first))

	second := sampleValidation("vd-1")
	second.MeasuredCaseCount = 34
	second.NDropped = 0
	require.NoError(t, s.WriteValidation(context.Background(), "vd-1", second))

	got, err := s.Validation(context.Background(), "vd-1")
	require.NoError(t, err)
	if got.GetMeasuredCaseCount() != 34 {
		t.Errorf("measured_case_count = %d, want the rewritten 34", got.GetMeasuredCaseCount())
	}
}

// TestValidationRefusesARecordWithNoRunID: the run ID is the key, and a
// Validation without one is a record nothing can find.
func TestValidationRefusesARecordWithNoRunID(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	validateRun(t, s, "vd-1")
	if err := s.WriteValidation(context.Background(), "vd-1", &knov1.Validation{}); err == nil {
		t.Error("a Validation with no run ID was accepted")
	}
}

// TestHoldoutUseRoundTrip: the record that a Portfolio met a holdout survives,
// and the uses come back oldest first so the ordinal a report prints is stable
// across processes.
func TestHoldoutUseRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newStore(t)
	validateRun(t, s, "vd-1")
	validateRun(t, s, "vd-2")

	require.NoError(t, s.RecordHoldoutUse(ctx, &store.HoldoutUse{
		EvalFingerprint: "fp", SelectRunID: "sel-1", ValidateRunID: "vd-1",
		CreatedAt: "2026-08-31T00:00:00Z",
	}))
	require.NoError(t, s.RecordHoldoutUse(ctx, &store.HoldoutUse{
		EvalFingerprint: "fp", SelectRunID: "sel-2", ValidateRunID: "vd-2",
		CreatedAt: "2026-08-31T01:00:00Z",
	}))
	// A different holdout entirely: a re-split IS a different holdout, and it
	// must not collide with the first.
	require.NoError(t, s.RecordHoldoutUse(ctx, &store.HoldoutUse{
		EvalFingerprint: "other-fp", SelectRunID: "sel-1", ValidateRunID: "vd-1",
		CreatedAt: "2026-08-31T02:00:00Z",
	}))

	uses, err := s.HoldoutUses(ctx, "fp")
	require.NoError(t, err)
	if len(uses) != 2 {
		t.Fatalf("recorded %d uses of this holdout, want 2", len(uses))
	}
	if uses[0].SelectRunID != "sel-1" || uses[1].SelectRunID != "sel-2" {
		t.Errorf("uses are not oldest first: %+v", uses)
	}
	other, err := s.HoldoutUses(ctx, "other-fp")
	require.NoError(t, err)
	if len(other) != 1 {
		t.Errorf("a different eval fingerprint returned %d uses, want 1", len(other))
	}
	none, err := s.HoldoutUses(ctx, "never-used")
	require.NoError(t, err)
	if len(none) != 0 {
		t.Errorf("an unused holdout returned %d uses, want 0", len(none))
	}
}

// TestHoldoutUseIsIdempotentPerPortfolio.
//
// The key excludes the validate run deliberately: a RESUME continues one look
// and must not count as a second peek. Re-recording the same pair — with a
// different validate run ID, which is what a careless resume would do — is a
// no-op rather than a second row.
func TestHoldoutUseIsIdempotentPerPortfolio(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newStore(t)
	validateRun(t, s, "vd-1")
	validateRun(t, s, "vd-2")

	for _, runID := range []string{"vd-1", "vd-2", "vd-1"} {
		require.NoError(t, s.RecordHoldoutUse(ctx, &store.HoldoutUse{
			EvalFingerprint: "fp", SelectRunID: "sel-1", ValidateRunID: runID,
			CreatedAt: "2026-08-31T00:00:00Z",
		}))
	}
	uses, err := s.HoldoutUses(ctx, "fp")
	require.NoError(t, err)
	if len(uses) != 1 {
		t.Fatalf("recorded %d uses, want 1 — a Portfolio meets a holdout once", len(uses))
	}
	if uses[0].ValidateRunID != "vd-1" {
		t.Errorf("the FIRST run to look is the one recorded; got %s", uses[0].ValidateRunID)
	}
}

// TestHoldoutUseRefusesAnIncompleteRecord.
//
// Every field is load-bearing: without the fingerprint the holdout has no
// identity, without the select run the ordinal means nothing, and without the
// validate run the refusal cannot name what to resume.
func TestHoldoutUseRefusesAnIncompleteRecord(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newStore(t)
	validateRun(t, s, "vd-1")

	for _, tc := range []struct {
		name string
		use  *store.HoldoutUse
	}{
		{"nothing at all", nil},
		{"no eval fingerprint", &store.HoldoutUse{SelectRunID: "s", ValidateRunID: "vd-1"}},
		{"no select run", &store.HoldoutUse{EvalFingerprint: "fp", ValidateRunID: "vd-1"}},
		{"no validate run", &store.HoldoutUse{EvalFingerprint: "fp", SelectRunID: "s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.RecordHoldoutUse(ctx, tc.use); err == nil {
				t.Error("an incomplete holdout-use record was accepted")
			}
		})
	}
}

// TestHoldoutUseRequiresARealRun: the foreign key is the point. A row naming a
// run that does not exist would claim a holdout was peeked at by nothing.
func TestHoldoutUseRequiresARealRun(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	err := s.RecordHoldoutUse(context.Background(), &store.HoldoutUse{
		EvalFingerprint: "fp", SelectRunID: "sel-1", ValidateRunID: "no-such-run",
		CreatedAt: "2026-08-31T00:00:00Z",
	})
	if err == nil {
		t.Error("a holdout use was recorded against a run that does not exist")
	}
}
