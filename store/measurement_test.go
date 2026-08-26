package store_test

import (
	"context"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
	"google.golang.org/protobuf/proto"
)

// scoredMeasurement builds a measurement that scored, at a stated cost.
func scoredMeasurement(assetID, caseID string, arm store.Arm, value float64, costMicros int64) *store.Measurement {
	return &store.Measurement{
		Key:      store.MeasurementKey{AssetID: assetID, CaseID: caseID, Arm: arm, Trial: 1},
		Response: &knov1.Response{CaseId: caseID, Output: "answer", CostUsdMicros: costMicros},
		Score:    &knov1.Score{CaseId: caseID, Value: value, Passed: value > 0},
		Spend:    budget.Spend{Calls: 1, CostUSDMicros: costMicros, Tokens: 42},
	}
}

// mustExec writes a row directly, for rows this build's writer cannot produce.
func mustExec(t *testing.T, s *store.SQLite, stmt string) {
	t.Helper()

	if err := s.ExecForTest(context.Background(), stmt); err != nil {
		t.Fatalf("executing %s: %v", stmt, err)
	}
}

// seedRun creates a run to hang measurements off.
func seedRun(t *testing.T, s *store.SQLite, runID string) {
	t.Helper()

	if err := s.CreateRun(context.Background(), newRun(runID)); err != nil {
		t.Fatalf("creating run %s: %v", runID, err)
	}
}

// TestOneCaseIsMeasuredOncePerAssetArmAndTrial.
//
// The defect the measurements table exists to fix, asserted directly. The
// outcomes table is PRIMARY KEY (run_id, case_id) and RecordOutcome is INSERT
// OR IGNORE, so a Value run writing there would keep the first measurement of
// each Case and silently discard every later one — 200 Assets over 50 Cases
// keeping 50 rows and dropping 9,950, each of them already paid for, while the
// resume reader reported the whole run finished.
//
// Every field of the key is exercised because dropping ANY of them reproduces
// the same bug one level down: two Assets measured on one Case, two arms of one
// pair, two trials bought to reduce variance.
func TestOneCaseIsMeasuredOncePerAssetArmAndTrial(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s := newStore(t)
	seedRun(t, s, "run-1")

	want := []store.MeasurementKey{
		{AssetID: "asset-a", CaseID: "case-1", Arm: store.ArmTreatment, Trial: 1},
		{AssetID: "asset-b", CaseID: "case-1", Arm: store.ArmTreatment, Trial: 1},
		{AssetID: "asset-a", CaseID: "case-1", Arm: store.ArmControl, Trial: 1},
		{AssetID: "asset-a", CaseID: "case-1", Arm: store.ArmTreatment, Trial: 2},
	}
	for _, k := range want {
		m := scoredMeasurement(k.AssetID, k.CaseID, k.Arm, 1, 100)
		m.Key = k
		if err := s.RecordMeasurement(ctx, "run-1", m); err != nil {
			t.Fatalf("recording %+v: %v", k, err)
		}
	}

	done, err := s.CompletedMeasurements(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedMeasurements: %v", err)
	}
	if len(done) != len(want) {
		t.Fatalf("%d measurements recorded, want %d — a key that collapses two "+
			"distinct measurements discards one that was already paid for, and "+
			"reports the run more finished than it is", len(done), len(want))
	}
	for _, k := range want {
		if _, ok := done[k]; !ok {
			t.Errorf("%+v is absent; resume would pay for it a second time", k)
		}
	}
}

// TestRecordMeasurementKeepsTheFirstResult.
//
// INSERT OR IGNORE, matching RecordOutcome and for the same reason: the money
// for a recorded measurement is already spent and already counted, so a resumed
// run that re-attempts one which in fact completed must not overwrite it.
func TestRecordMeasurementKeepsTheFirstResult(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s := newStore(t)
	seedRun(t, s, "run-1")

	first := scoredMeasurement("asset-a", "case-1", store.ArmTreatment, 1, 100)
	if err := s.RecordMeasurement(ctx, "run-1", first); err != nil {
		t.Fatalf("first: %v", err)
	}
	second := scoredMeasurement("asset-a", "case-1", store.ArmTreatment, 0, 900)
	if err := s.RecordMeasurement(ctx, "run-1", second); err != nil {
		t.Fatalf("second: %v", err)
	}

	spend, err := s.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if spend.CostUSDMicros != 100 || spend.Calls != 1 {
		t.Errorf("settled %d micros over %d calls, want 100 over 1; a second "+
			"insert either replaced the first result or double-counted its spend",
			spend.CostUSDMicros, spend.Calls)
	}
}

// TestAnArmlessMeasurementIsRefused.
//
// The arm is part of the primary key, so a Measurement whose Arm was never set
// would file the control arm on top of the treatment arm — losing the half of
// the pair that arrived second, and computing a delta against whichever
// survived. Refused at the door rather than defaulted, because there is no
// default that is right.
func TestAnArmlessMeasurementIsRefused(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s := newStore(t)
	seedRun(t, s, "run-1")

	m := scoredMeasurement("asset-a", "case-1", store.ArmUnspecified, 1, 100)
	if err := s.RecordMeasurement(ctx, "run-1", m); err == nil {
		t.Fatal("an unspecified arm was accepted; it is part of the key, so the " +
			"two arms of a pair would collide on one row")
	}
}

// TestMeasurementSpendIsInTheDurableRecord.
//
// SettledSpend is the ONLY durable record of money spent — the budget guard is
// in-memory and is reseeded from it on resume. A Value run's spend lives in
// measurements and nowhere else, so a SettledSpend that read only outcomes
// would return zero for a run that spent real money: kill it after $8 of a $10
// cap, resume, and the guard authorizes another $10.
//
// The assertion is a comparison against a NON-ZERO expected total, deliberately.
// The plan's original test for this — "resume re-pays nothing, asserted against
// SettledSpend" — would have passed against a method that structurally could
// not see the spend it asserted on, because both sides were zero.
func TestMeasurementSpendIsInTheDurableRecord(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s := newStore(t)
	seedRun(t, s, "run-1")

	for i, k := range []store.MeasurementKey{
		{AssetID: "asset-a", CaseID: "case-1", Arm: store.ArmTreatment, Trial: 1},
		{AssetID: "asset-a", CaseID: "case-1", Arm: store.ArmControl, Trial: 1},
		{AssetID: "asset-b", CaseID: "case-1", Arm: store.ArmTreatment, Trial: 1},
	} {
		m := scoredMeasurement(k.AssetID, k.CaseID, k.Arm, 1, int64(100*(i+1)))
		m.Key = k
		if err := s.RecordMeasurement(ctx, "run-1", m); err != nil {
			t.Fatalf("recording %+v: %v", k, err)
		}
	}
	// One outcome and one orphan charge as well: all three sources must add.
	if err := s.RecordOutcome(ctx, "run-1", scoredOutcome("case-9", 1, 7)); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	if err := s.RecordOrphanSpend(ctx, "run-1", budget.Spend{Calls: 1, CostUSDMicros: 3}); err != nil {
		t.Fatalf("RecordOrphanSpend: %v", err)
	}

	spend, err := s.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	// 100 + 200 + 300 measurements, 7 outcome, 3 orphan.
	if want := int64(610); spend.CostUSDMicros != want {
		t.Errorf("settled %d micros, want %d; money this method cannot see is "+
			"money a resumed run's guard hands back as headroom and spends again",
			spend.CostUSDMicros, want)
	}
	if want := int64(5); spend.Calls != want {
		t.Errorf("settled %d calls, want %d", spend.Calls, want)
	}
}

// TestPurgeCoversMeasurementContent.
//
// A measurement's response holds exactly the same end-user conversation content
// an outcome's does. A purge that cleared only outcomes would report success
// over content still on disk — worse than failing, because the user acts on the
// report.
func TestPurgeCoversMeasurementContent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s := newStore(t)
	seedRun(t, s, "run-1")

	if err := s.RecordMeasurement(ctx, "run-1",
		scoredMeasurement("asset-a", "case-1", store.ArmTreatment, 1, 100)); err != nil {
		t.Fatalf("RecordMeasurement: %v", err)
	}
	if err := s.RecordOutcome(ctx, "run-1", scoredOutcome("case-9", 1, 7)); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}

	before, err := s.PurgeableCount(ctx, "run-1")
	if err != nil {
		t.Fatalf("PurgeableCount: %v", err)
	}
	if before != 2 {
		t.Fatalf("PurgeableCount = %d, want 2; the confirmation prompt would "+
			"understate what the user is agreeing to remove", before)
	}

	n, err := s.Purge(ctx, "run-1")
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if n != 2 {
		t.Errorf("Purge reported %d rows, want 2", n)
	}
	after, err := s.PurgeableCount(ctx, "run-1")
	if err != nil {
		t.Fatalf("PurgeableCount after: %v", err)
	}
	if after != 0 {
		t.Errorf("%d rows still hold content after a purge that reported success", after)
	}
}

// TestCaseScoresSeparatesAbsentFromUnrecoverable.
//
// Three states, three different correct handlings. A Case that never scored is
// absent. A Case that scored has a number. A Case that scored and whose number
// is gone is PRESENT with Unrecoverable set — because a pair built against it
// is not a pair with a zero in it, it is a pair that cannot be formed, and a
// map[string]float64 would hand the caller a zero indistinguishable from a real
// score of zero.
func TestCaseScoresSeparatesAbsentFromUnrecoverable(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s := newStore(t)
	seedRun(t, s, "run-1")

	if err := s.RecordOutcome(ctx, "run-1", scoredOutcome("case-scored", 0.75, 10)); err != nil {
		t.Fatalf("scored: %v", err)
	}
	if err := s.RecordOutcome(ctx, "run-1", &store.Outcome{
		CaseID: "case-errored",
		Err:    "timeout",
		Spend:  budget.Spend{Calls: 1},
	}); err != nil {
		t.Fatalf("errored: %v", err)
	}

	// A scored row whose number is gone, as a purge before the score column
	// existed left it.
	mustExec(t, s, `INSERT INTO outcomes
	    (run_id, case_id, scored, err_code, calls, cost_usd_micros, tokens)
	  VALUES ('run-1', 'case-gone', 1, '', 1, 5, 10)`)

	scores, err := s.CaseScores(ctx, "run-1")
	if err != nil {
		t.Fatalf("CaseScores: %v", err)
	}
	if _, ok := scores["case-errored"]; ok {
		t.Error("an errored Case appears in CaseScores; pairing against it would " +
			"manufacture a delta out of a Case that produced no number")
	}
	got, ok := scores["case-scored"]
	if !ok {
		t.Fatal("a scored Case is absent from CaseScores")
	}
	if got.Unrecoverable || got.Value != 0.75 {
		t.Errorf("scored Case = %+v, want {0.75 false}", got)
	}

	gone, ok := scores["case-gone"]
	if !ok {
		t.Fatal("a Case that scored and lost its number is absent from CaseScores; " +
			"absent means NEVER SCORED, and the two license different reports")
	}
	if !gone.Unrecoverable {
		t.Errorf("case-gone = %+v, want Unrecoverable; a zero handed back here is "+
			"indistinguishable from a real score of zero, and pairing against it "+
			"manufactures a delta", gone)
	}
}

// TestValuationIsWrittenWholeAndRecomputedOnResume.
//
// A Valuation is DERIVED from measurements rather than being itself a record of
// spend, so it is the one writer here that replaces. A resume that finishes an
// Asset's remaining measurements must produce the row matching what is now
// recorded; keeping a first write made over half a sample would pin the report
// to a delta computed over the Cases that happened to run before the cap bound.
func TestValuationIsWrittenWholeAndRecomputedOnResume(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s := newStore(t)
	seedRun(t, s, "run-1")

	partial := &knov1.Valuation{AssetId: "asset-a", DeltaGoal: 0.9, NRouted: proto.Int32(10)}
	if err := s.WriteValuation(ctx, "run-1", partial); err != nil {
		t.Fatalf("first write: %v", err)
	}
	whole := &knov1.Valuation{AssetId: "asset-a", DeltaGoal: 0.2, NRouted: proto.Int32(50)}
	if err := s.WriteValuation(ctx, "run-1", whole); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if err := s.WriteValuation(ctx, "run-1",
		&knov1.Valuation{AssetId: "asset-b", DeltaGoal: 0.1, NRouted: proto.Int32(50)}); err != nil {
		t.Fatalf("asset-b: %v", err)
	}

	got, err := s.Valuations(ctx, "run-1")
	if err != nil {
		t.Fatalf("Valuations: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("%d valuations, want 2", len(got))
	}
	if got[0].GetAssetId() != "asset-a" || got[1].GetAssetId() != "asset-b" {
		t.Errorf("order is %s, %s; want asset-a, asset-b — an unordered read makes "+
			"the golden report tests flaky rather than wrong",
			got[0].GetAssetId(), got[1].GetAssetId())
	}
	if got[0].GetDeltaGoal() != 0.2 || got[0].GetNRouted() != 50 {
		t.Errorf("asset-a = delta %v over n %d, want 0.2 over 50; the partial "+
			"valuation survived a recompute and pins the report to half a sample",
			got[0].GetDeltaGoal(), got[0].GetNRouted())
	}
}

// TestAValuationNeedsAnAssetID: a Valuation with no Asset ID has nothing to
// identify it, and the key would collapse every such row onto one.
func TestAValuationNeedsAnAssetID(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	seedRun(t, s, "run-1")

	if err := s.WriteValuation(t.Context(), "run-1", &knov1.Valuation{DeltaGoal: 1}); err == nil {
		t.Fatal("a Valuation with no Asset ID was accepted")
	}
}

// TestObservationsAndTheModelGateSeeMeasurements.
//
// CaseExecution's counts are what a Value Run reports about itself, and
// resolved_models is what the mid-run model gate compares a response against. A
// reader that saw only outcomes would hand a Value run zero counts — a Run
// record asserting the stage did nothing while its spend says otherwise — and
// hand the gate an empty set, so it would report success against nothing. That
// is the shape docs/debt.md#42 records the gate having had once already.
func TestObservationsAndTheModelGateSeeMeasurements(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s := newStore(t)
	seedRun(t, s, "run-1")

	m := scoredMeasurement("asset-a", "case-1", store.ArmTreatment, 1, 100)
	m.Response.ResolvedModel = "gpt-4.1-2026-01-01"
	if err := s.RecordMeasurement(ctx, "run-1", m); err != nil {
		t.Fatalf("RecordMeasurement: %v", err)
	}
	if err := s.RecordMeasurement(ctx, "run-1", &store.Measurement{
		Key:   store.MeasurementKey{AssetID: "asset-a", CaseID: "case-2", Arm: store.ArmTreatment, Trial: 1},
		Err:   "timeout",
		Spend: budget.Spend{Calls: 1},
	}); err != nil {
		t.Fatalf("errored measurement: %v", err)
	}

	obs, err := s.CaseObservations(ctx, "run-1")
	if err != nil {
		t.Fatalf("CaseObservations: %v", err)
	}
	if obs.Attempted != 2 || obs.Scored != 1 || obs.Errored != 1 {
		t.Errorf("attempted/scored/errored = %d/%d/%d, want 2/1/1; a Run reporting "+
			"zeros beside real spend asserts the stage did nothing",
			obs.Attempted, obs.Scored, obs.Errored)
	}
	if len(obs.ResolvedModels) != 1 || obs.ResolvedModels[0] != "gpt-4.1-2026-01-01" {
		t.Errorf("resolved models = %v, want one entry; an empty set makes the "+
			"mid-run model gate report success against nothing", obs.ResolvedModels)
	}
}

// TestScoreSumSeparatesAPurgeFromAnOlderBinary.
//
// docs/debt.md#31. A scored row with no readable number has two causes — the
// user purged it before the score lived in a column, or a binary that predated
// the column wrote it — and reporting both as a purge sends the user looking
// for a deletion nobody performed. The writer_schema_version column is what
// distinguishes them, from schema version 3 onward.
func TestScoreSumSeparatesAPurgeFromAnOlderBinary(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s := newStore(t)
	seedRun(t, s, "run-1")

	if err := s.RecordOutcome(ctx, "run-1", scoredOutcome("case-1", 1, 10)); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	// A row as an older binary left it: scored, no number, no writer version.
	mustExec(t, s, `INSERT INTO outcomes
	    (run_id, case_id, scored, err_code, calls, cost_usd_micros, tokens,
	     writer_schema_version)
	  VALUES ('run-1', 'case-old', 1, '', 1, 5, 10, 0)`)
	// A row this build wrote and a purge later emptied.
	mustExec(t, s, `INSERT INTO outcomes
	    (run_id, case_id, scored, err_code, calls, cost_usd_micros, tokens,
	     writer_schema_version)
	  VALUES ('run-1', 'case-purged', 1, '', 1, 5, 10, 3)`)

	got, err := s.ScoreSum(ctx, "run-1")
	if err != nil {
		t.Fatalf("ScoreSum: %v", err)
	}
	if got.Counted != 1 || got.Sum != 1 {
		t.Errorf("counted %d summing to %v, want 1 summing to 1", got.Counted, got.Sum)
	}
	if got.Purged != 1 {
		t.Errorf("purged = %d, want 1", got.Purged)
	}
	if got.UnknownProvenance != 1 {
		t.Errorf("unknown provenance = %d, want 1; a row an older binary wrote "+
			"reported as a purge sends the user looking for a deletion nobody ran",
			got.UnknownProvenance)
	}
	if got.Unrecoverable() != 2 {
		t.Errorf("unrecoverable = %d, want 2", got.Unrecoverable())
	}
}

// TestNewReadersFailClosedAfterClose: every Store method must refuse on a
// closed store rather than panicking on a nil handle.
func TestNewReadersFailClosedAfterClose(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := store.NewSQLite(ctx, t.TempDir()+"/kno.db")
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	if err := s.RecordMeasurement(ctx, "run-1",
		scoredMeasurement("asset-a", "case-1", store.ArmTreatment, 1, 1)); err == nil {
		t.Error("RecordMeasurement succeeded on a closed store")
	}
	if _, err := s.CompletedMeasurements(ctx, "run-1"); err == nil {
		t.Error("CompletedMeasurements succeeded on a closed store")
	}
	if _, err := s.CaseScores(ctx, "run-1"); err == nil {
		t.Error("CaseScores succeeded on a closed store")
	}
	if err := s.WriteValuation(ctx, "run-1", &knov1.Valuation{AssetId: "a"}); err == nil {
		t.Error("WriteValuation succeeded on a closed store")
	}
	if _, err := s.Valuations(ctx, "run-1"); err == nil {
		t.Error("Valuations succeeded on a closed store")
	}
}
