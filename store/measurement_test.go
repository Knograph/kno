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

// mustExecArgs writes a row directly with bound arguments.
func mustExecArgs(t *testing.T, s *store.SQLite, stmt string, args ...any) {
	t.Helper()

	if err := s.ExecForTest(context.Background(), stmt, args...); err != nil {
		t.Fatalf("executing %s: %v", stmt, err)
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
	// Tokens too. Not a cap, but one of the three columns this test's name
	// claims to cover, and dropping its COALESCE survived the first version.
	if want := int64(3*42 + 42); spend.Tokens != want {
		t.Errorf("settled %d tokens, want %d", spend.Tokens, want)
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

	// asset-b FIRST, so insertion order contradicts sorted order. Written the
	// other way the rowid order already matched and the ORDER BY could be
	// deleted with every assertion still passing.
	if err := s.WriteValuation(ctx, "run-1",
		&knov1.Valuation{AssetId: "asset-b", DeltaGoal: 0.1, NRouted: proto.Int32(50)}); err != nil {
		t.Fatalf("asset-b: %v", err)
	}

	partial := &knov1.Valuation{AssetId: "asset-a", DeltaGoal: 0.9, NRouted: proto.Int32(10)}
	if err := s.WriteValuation(ctx, "run-1", partial); err != nil {
		t.Fatalf("first write: %v", err)
	}
	whole := &knov1.Valuation{AssetId: "asset-a", DeltaGoal: 0.2, NRouted: proto.Int32(50)}
	if err := s.WriteValuation(ctx, "run-1", whole); err != nil {
		t.Fatalf("second write: %v", err)
	}
	got, err := s.Valuations(ctx, "run-1")
	if err != nil {
		t.Fatalf("Valuations: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("%d valuations, want 2", len(got))
	}
	// This assertion pins intent and CANNOT currently fail: the primary key
	// (run_id, asset_id) gives SQLite an index in exactly this order, so
	// deleting the ORDER BY changes nothing observable — verified by doing it.
	// Said here rather than left implied, because a test that reads like a
	// guarantee and is really a coincidence is the shape docs/debt.md#70
	// records. The ORDER BY stays: SQLite promises no order without one, and
	// the day a query planner or a schema change stops supplying it, the golden
	// report tests would go flaky rather than red.
	if got[0].GetAssetId() != "asset-a" || got[1].GetAssetId() != "asset-b" {
		t.Errorf("order is %s, %s; want asset-a, asset-b",
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
	m.Response.ProviderBuildId = "build-77"
	// Refused, truncated, and usage-estimated all set on one measurement, so a
	// writer that dropped any of them is caught. Observations documents Refused
	// as counted separately "so a run that was refused rather than measured
	// cannot pass for a clean baseline" — a guarantee nothing asserted for
	// measurements until now.
	m.Response.Refused = true
	m.Response.StopReason = knov1.StopReason_STOP_REASON_LENGTH
	m.Response.UsageEstimated = true
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
	if obs.Refused != 1 || obs.Truncated != 1 || obs.UsageEstimated != 1 {
		t.Errorf("refused/truncated/usage-estimated = %d/%d/%d, want 1/1/1; a run "+
			"the provider refused rather than measured would otherwise pass for a "+
			"clean one", obs.Refused, obs.Truncated, obs.UsageEstimated)
	}
	if len(obs.ResolvedModels) != 1 || obs.ResolvedModels[0] != "gpt-4.1-2026-01-01" {
		t.Errorf("resolved models = %v, want one entry; an empty set makes the "+
			"mid-run model gate report success against nothing", obs.ResolvedModels)
	}
	if len(obs.ProviderBuilds) != 1 || obs.ProviderBuilds[0] != "build-77" {
		t.Errorf("provider builds = %v, want one entry", obs.ProviderBuilds)
	}

	// OutcomeCounts spans both tables too. core/baseline seeds a resumed run's
	// aggregate from this AND from ScoreSum on the same path, so a blind
	// OutcomeCounts puts the numerator over the whole run and the denominator
	// over the tail.
	scored, errored, err := s.OutcomeCounts(ctx, "run-1")
	if err != nil {
		t.Fatalf("OutcomeCounts: %v", err)
	}
	if scored != 1 || errored != 1 {
		t.Errorf("OutcomeCounts = %d scored / %d errored, want 1/1; a reader blind to "+
			"measurements reseeds a resumed aggregate with a denominator covering "+
			"only the tail", scored, errored)
	}
}

// TestScoreSumSeparatesAPurgeFromAnOlderBinary.
//
// docs/debt.md#31. A scored row with no readable number has two causes, and
// reporting both as a purge sends a user looking for a deletion nobody ran.
//
// Built from the REAL states, which is the whole point and which an earlier
// version of this test got wrong: it fabricated a row carrying a writer-version
// marker that no binary can produce, and passed against a predicate under which
// the genuine purge landed in the other bucket and the `Purged` count was
// provably always zero. The discriminator is the BLOB.
func TestScoreSumSeparatesAPurgeFromAnOlderBinary(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	// An M1 database whose first Case was purged before score_value existed —
	// the repository's own fixture for exactly this state. The migration's
	// backfill lifts the number out of every surviving blob and cannot reach
	// this one.
	s, err := store.NewSQLite(ctx, writeM1Database(t, 4, true))
	if err != nil {
		t.Fatalf("opening the migrated M1 database: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	got, err := s.ScoreSum(ctx, "run-1")
	if err != nil {
		t.Fatalf("ScoreSum: %v", err)
	}
	if got.Purged != 1 {
		t.Errorf("a genuine pre-column purge reports Purged = %d, want 1 (summary %+v). "+
			"This is the state the field is named for; if it lands anywhere else the "+
			"user is told their data went missing for a reason that did not happen",
			got.Purged, got)
	}
	if got.UnknownProvenance != 0 {
		t.Errorf("a genuine purge was reported as unknown provenance (%+v)", got)
	}

	// A row an older binary left behind: the Score blob is present and the
	// number was never lifted out of it, because nothing re-runs the backfill.
	blob, err := proto.Marshal(&knov1.Score{CaseId: "case-old", Value: 0.5, Passed: true})
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}
	mustExecArgs(t, s, `INSERT INTO outcomes
	    (run_id, case_id, scored, err_code, calls, cost_usd_micros, tokens, score_proto)
	  VALUES ('run-1', 'case-old', 1, '', 1, 5, 10, ?)`, blob)

	got, err = s.ScoreSum(ctx, "run-1")
	if err != nil {
		t.Fatalf("ScoreSum: %v", err)
	}
	if got.UnknownProvenance != 1 {
		t.Errorf("a row whose Score blob survives but whose number was never lifted "+
			"out reports UnknownProvenance = %d, want 1 (summary %+v)",
			got.UnknownProvenance, got)
	}
	if got.Purged != 1 {
		t.Errorf("the purged row stopped being counted as purged (%+v)", got)
	}
	if got.Unrecoverable() != 2 {
		t.Errorf("unrecoverable = %d, want 2", got.Unrecoverable())
	}
}

// TestATrialNumberIsRefusedRatherThanNormalized.
//
// The key a caller writes must be the key it can look up again. Normalizing a
// zero to 1 on write means a resume that follows the documented contract cannot
// find its own measurement in CompletedMeasurements: it pays the provider a
// second time, and INSERT OR IGNORE then drops that second row AND its spend —
// so the run pays twice and SettledSpend, the only durable record, never sees
// the second payment. A third process then reseeds the guard below what was
// actually spent, and it compounds per resume.
func TestATrialNumberIsRefusedRatherThanNormalized(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s := newStore(t)
	seedRun(t, s, "run-1")

	m := scoredMeasurement("asset-a", "case-1", store.ArmTreatment, 1, 100)
	m.Key.Trial = 0
	if err := s.RecordMeasurement(ctx, "run-1", m); err == nil {
		t.Fatal("trial 0 was accepted; a key normalized on write is a key the writer " +
			"cannot look up on resume, and the resume pays again")
	}

	// And the contract holds for a key that IS supplied: what goes in comes back.
	m.Key.Trial = 2
	if err := s.RecordMeasurement(ctx, "run-1", m); err != nil {
		t.Fatalf("RecordMeasurement: %v", err)
	}
	done, err := s.CompletedMeasurements(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedMeasurements: %v", err)
	}
	if _, ok := done[m.Key]; !ok {
		t.Errorf("the key written (%+v) is not the key returned (%v); a resume "+
			"consulting this set would re-pay for work it already did", m.Key, done)
	}
}

// TestANegativeChargeCannotBuyHeadroom.
//
// SettledSpend reseeds the budget guard on resume, so a negative charge is not
// a reporting error — it subtracts inside SUM() before anything in Go can
// refuse it, and hands the guard money that was never returned. Clamped at the
// choke point, which is the conclusion docs/debt.md#48 reached for
// Reservation.Settle and RecordOrphanSpend already applies.
func TestANegativeChargeCannotBuyHeadroom(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s := newStore(t)
	seedRun(t, s, "run-1")

	charged := scoredMeasurement("asset-a", "case-1", store.ArmTreatment, 1, 5000)
	if err := s.RecordMeasurement(ctx, "run-1", charged); err != nil {
		t.Fatalf("RecordMeasurement: %v", err)
	}
	credit := scoredMeasurement("asset-a", "case-2", store.ArmTreatment, 1, 0)
	credit.Spend = budget.Spend{Calls: -1, CostUSDMicros: -4000, Tokens: -100}
	if err := s.RecordMeasurement(ctx, "run-1", credit); err != nil {
		t.Fatalf("recording the negative charge: %v", err)
	}
	// The same door on the outcomes side.
	refund := scoredOutcome("case-9", 1, 0)
	refund.Spend = budget.Spend{Calls: -5, CostUSDMicros: -9000, Tokens: -1}
	if err := s.RecordOutcome(ctx, "run-1", refund); err != nil {
		t.Fatalf("recording the negative outcome charge: %v", err)
	}

	spend, err := s.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if spend.CostUSDMicros != 5000 {
		t.Errorf("settled %d micros against real spend of 5000; a negative charge "+
			"subtracts inside SUM() and the difference is free headroom the resumed "+
			"guard will authorize", spend.CostUSDMicros)
	}
	if spend.Calls != 1 {
		t.Errorf("settled %d calls, want 1", spend.Calls)
	}
}

// TestMeasurementsReadBackWhatTheyScored.
//
// What makes a resumed Valuation computable. A run stopped mid-Asset must
// recompute over BOTH processes' measurements; without this reader it could
// only recompute over its own half — a delta over half a sample, which is what
// leaving the Valuation unwritten exists to prevent — or re-pay to recover the
// numbers, which is what the table exists to prevent.
func TestMeasurementsReadBackWhatTheyScored(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s := newStore(t)
	seedRun(t, s, "run-1")

	for _, m := range []*store.Measurement{
		scoredMeasurement("asset-a", "case-2", store.ArmTreatment, 0.25, 10),
		scoredMeasurement("asset-a", "case-1", store.ArmControl, 0.75, 10),
		scoredMeasurement("asset-b", "case-1", store.ArmTreatment, 1, 10),
	} {
		if err := s.RecordMeasurement(ctx, "run-1", m); err != nil {
			t.Fatalf("recording %+v: %v", m.Key, err)
		}
	}
	if err := s.RecordMeasurement(ctx, "run-1", &store.Measurement{
		Key:   store.MeasurementKey{AssetID: "asset-a", CaseID: "case-3", Arm: store.ArmTreatment, Trial: 1},
		Err:   "timeout",
		Spend: budget.Spend{Calls: 1},
	}); err != nil {
		t.Fatalf("errored measurement: %v", err)
	}

	got, err := s.Measurements(ctx, "run-1", "asset-a")
	if err != nil {
		t.Fatalf("Measurements: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("%d measurements for asset-a, want 3 (asset-b must not appear)", len(got))
	}
	// Ordered by the full key, so a recomputation is reproducible.
	if got[0].Key.CaseID != "case-1" || got[1].Key.CaseID != "case-2" || got[2].Key.CaseID != "case-3" {
		t.Errorf("order is %s, %s, %s; want case-1, case-2, case-3",
			got[0].Key.CaseID, got[1].Key.CaseID, got[2].Key.CaseID)
	}
	if got[0].Key.Arm != store.ArmControl || got[0].Score != 0.75 {
		t.Errorf("case-1 = %+v, want the control arm at 0.75", got[0])
	}
	if got[2].Err != "timeout" || got[2].Unrecoverable {
		t.Errorf("the errored measurement = %+v, want Err set and Unrecoverable clear — "+
			"an error is not a lost number", got[2])
	}

	// A purge takes the blobs and keeps the numbers, so a purged run stays
	// recomputable. That is the trade retention.md promises.
	if _, err := s.Purge(ctx, "run-1"); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	after, err := s.Measurements(ctx, "run-1", "asset-a")
	if err != nil {
		t.Fatalf("Measurements after purge: %v", err)
	}
	if after[0].Score != 0.75 || after[0].Unrecoverable {
		t.Errorf("a purge cost the score: %+v", after[0])
	}
}

// TestAFailedPurgeReportsWhatItDestroyed.
//
// Two independent statements let the first succeed and the second fail,
// returning an error with no count — telling a user nothing was removed after
// content was irreversibly removed. One transaction means either both or
// neither, on the one command whose whole job is saying what it removed.
func TestAFailedPurgeReportsWhatItDestroyed(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s := newStore(t)
	seedRun(t, s, "run-1")

	if err := s.RecordOutcome(ctx, "run-1", scoredOutcome("case-1", 1, 10)); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	if err := s.RecordMeasurement(ctx, "run-1",
		scoredMeasurement("asset-a", "case-1", store.ArmTreatment, 1, 10)); err != nil {
		t.Fatalf("RecordMeasurement: %v", err)
	}
	// Break the second statement's table out from under the purge.
	mustExec(t, s, `DROP TABLE measurements`)

	n, err := s.Purge(ctx, "run-1")
	if err == nil {
		t.Fatal("a purge over a missing table reported success")
	}
	if n != 0 {
		t.Errorf("a failed purge reported %d rows", n)
	}
	resp, score, err := s.RawBlobs(ctx, "run-1", "case-1")
	if err != nil {
		t.Fatalf("RawBlobs: %v", err)
	}
	if resp == nil && score == nil {
		t.Error("the outcome's content was destroyed by a purge that reported failure " +
			"and no count; the two statements must be one transaction, so a caller " +
			"told nothing was removed can believe it")
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
