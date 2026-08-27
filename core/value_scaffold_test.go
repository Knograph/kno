package core

import (
	"context"
	"errors"
	"iter"
	"math"
	"strings"
	"testing"

	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/stats/interval"
	"github.com/knograph/kno/store"
)

// fixedDirectionGoal is a Goal that only supplies what the scaffolding reads:
// a declared direction and domain. Scoring never happens in these tests.
type fixedDirectionGoal struct {
	dir    knov1.Direction
	domain knov1.ScoreDomain
}

func (fixedDirectionGoal) Score(context.Context, *Case, *Response) (*Score, error) {
	return &knov1.Score{}, nil
}

func (g fixedDirectionGoal) Direction() Direction { return g.dir }

func (g fixedDirectionGoal) Domain() ScoreDomain { return g.domain }

// TestMinimizeGoalsProduceSignedDeltas pins the P0-1 fix: direction is applied
// exactly once, in pairs, before any aggregation. Every earlier test in the
// suite used a MAXIMIZE-shaped fixture, which is why an inverted MINIMIZE
// delta could sit in the scaffolding unnoticed.
func TestMinimizeGoalsProduceSignedDeltas(t *testing.T) {
	t.Parallel()

	treatment := map[string]map[int32]float64{"c1": {1: 5.0}}
	baseline := map[string]store.CaseScore{"c1": {Value: 2.0}}

	maxDeltas, _ := pairs([]string{"c1"}, treatment, nil, baseline, false, knov1.Direction_DIRECTION_MAXIMIZE)
	if got := maxDeltas[0][0]; got != 3.0 {
		t.Errorf("MAXIMIZE delta = %v, want +3", got)
	}
	minDeltas, _ := pairs([]string{"c1"}, treatment, nil, baseline, false, knov1.Direction_DIRECTION_MINIMIZE)
	if got := minDeltas[0][0]; got != -3.0 {
		t.Errorf("MINIMIZE delta = %v, want -3: the goal's direction is not "+
			"decoration, and a latency goal that got slower must report a NEGATIVE delta", got)
	}
}

// TestPairingJoinsOnTrialNumber pins the P2-14 fix. After a trial is lost on
// one side only, positional pairing would align treatment-trial-3 with
// control-trial-2 — a pair that never happened — and would hide the drop.
func TestPairingJoinsOnTrialNumber(t *testing.T) {
	t.Parallel()

	// Trial 2 was lost on the treatment side only.
	treatment := map[string]map[int32]float64{"c1": {1: 10.0, 3: 30.0}}
	control := map[string]map[int32]float64{"c1": {1: 5.0, 2: 7.0, 3: 15.0}}

	deltas, dropped := pairs([]string{"c1"}, treatment, control, nil, true, knov1.Direction_DIRECTION_MAXIMIZE)
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1 (the unpaired trial 2)", dropped)
	}
	if len(deltas) != 1 || len(deltas[0]) != 2 {
		t.Fatalf("pairs = %v, want one Case vector of two deltas", deltas)
	}
	// trial 1: 10-5 = 5; trial 3: 30-15 = 15. Positional pairing would have
	// produced 10-5 and 30-7, and the second number is a draw that never
	// happened.
	if deltas[0][0] != 5.0 || deltas[0][1] != 15.0 {
		t.Errorf("deltas = %v, want [5 15]", deltas[0])
	}
}

// TestValuationOmitsDeltaWithoutInterval pins the P0-2 fix: a one-Case routed
// slice cannot form an interval, so the Valuation reports UNDERERPOWERED and
// omits the delta rather than shipping a bare number — the shape prime
// directive 5 exists to ban.
func TestValuationOmitsDeltaWithoutInterval(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	opts := ValueOptions{
		RunID: "run-1",
		Store: st,
		Goal:  fixedDirectionGoal{dir: knov1.Direction_DIRECTION_MAXIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY},
	}
	writeMeasurement(t, st, "run-1", "a", "c1", store.ArmTreatment, 1, 0.9)

	v, err := opts.valuationFor(context.Background(),
		&Asset{Id: "a"},
		value.AssetRouting{AssetID: "a", CaseIDs: []string{"c1"}},
		&value.Plan{Trials: 1, EligibleCases: 1},
		map[string]store.CaseScore{"c1": {Value: 0.5, Passed: true}},
	)
	if err != nil {
		t.Fatalf("valuationFor: %v", err)
	}
	if v.NotMeasured != knov1.RejectionReason_REJECTION_REASON_UNDERPOWERED {
		t.Errorf("NotMeasured = %v, want UNDERPOWERED: one pair cannot form an "+
			"interval, and a delta without its interval must not be reported", v.NotMeasured)
	}
	if v.DeltaInterval != nil || v.DeltaGoal != 0 {
		t.Errorf("DeltaGoal = %v with interval %v, want both absent", v.DeltaGoal, v.DeltaInterval)
	}
	if v.NPairs == nil || v.NDropped == nil {
		t.Fatalf("n_pairs/n_dropped = %v/%v, want the attrition statement to "+
			"travel with the underpowered marker", v.NPairs, v.NDropped)
	}
}

// TestRaggedAttritionReportsUnderpoweredNotAShrunkenDelta pins the other half
// of P0-2: ragged per-Case vectors (one Case measured twice, another once) are
// refused by the interval package, and the fallback is the named reason — not
// a delta computed over whatever survived.
func TestRaggedAttritionReportsUnderpoweredNotAShrunkenDelta(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	opts := ValueOptions{
		RunID: "run-1",
		Store: st,
		Goal:  fixedDirectionGoal{dir: knov1.Direction_DIRECTION_MAXIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY},
	}
	writeMeasurement(t, st, "run-1", "a", "c1", store.ArmTreatment, 1, 0.9)
	writeMeasurement(t, st, "run-1", "a", "c2", store.ArmTreatment, 1, 0.8)
	writeMeasurement(t, st, "run-1", "a", "c2", store.ArmTreatment, 2, 0.8)

	v, err := opts.valuationFor(context.Background(),
		&Asset{Id: "a"},
		value.AssetRouting{AssetID: "a", CaseIDs: []string{"c1", "c2"}},
		&value.Plan{Trials: 2, EligibleCases: 2},
		map[string]store.CaseScore{"c1": {Value: 0.5}, "c2": {Value: 0.5}},
	)
	if err != nil {
		t.Fatalf("valuationFor: %v", err)
	}
	if v.NotMeasured != knov1.RejectionReason_REJECTION_REASON_UNDERPOWERED {
		t.Errorf("NotMeasured = %v, want UNDERPOWERED for a ragged pair set", v.NotMeasured)
	}
	if v.DeltaGoal != 0 || v.DeltaInterval != nil {
		t.Errorf("DeltaGoal = %v, want the ragged delta suppressed", v.DeltaGoal)
	}
}

// TestHarmBoundConsumesPerCaseMeans pins the P0-3 fix: the control interval is
// computed over one value per Case, never over the flattened per-trial deltas.
// Flattening inflates n by Trials, which narrows the harm bound by about
// sqrt(Trials) in the direction that clears harmful assets.
func TestHarmBoundConsumesPerCaseMeans(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	opts := ValueOptions{
		RunID: "run-1",
		Store: st,
		Goal:  fixedDirectionGoal{dir: knov1.Direction_DIRECTION_MAXIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY},
	}
	// Two control Cases, three trials each, baseline recorded at 0.5.
	for _, c := range []string{"c1", "c2"} {
		for trial := int32(1); trial <= 3; trial++ {
			writeMeasurement(t, st, "run-1", "a", c, store.ArmTreatment, trial, 0.9)
		}
	}

	v, err := opts.valuationFor(context.Background(),
		&Asset{Id: "a"},
		value.AssetRouting{AssetID: "a", CaseIDs: []string{"c1", "c2"}},
		&value.Plan{Trials: 3, EligibleCases: 2, ControlCaseIDs: []string{"c1", "c2"}},
		map[string]store.CaseScore{"c1": {Value: 0.5}, "c2": {Value: 0.5}},
	)
	if err != nil {
		t.Fatalf("valuationFor: %v", err)
	}
	// The honest computation: per-Case means [0.4, 0.4], one value per Case.
	honest := interval.HarmBound([]float64{0.4, 0.4}, knov1.ScoreDomain_SCORE_DOMAIN_BINARY, 3, defaultLevel)
	if honest == nil {
		t.Fatal("the honest two-Case bound is nil; the fixture is too small")
	}
	if v.ControlInterval == nil {
		t.Fatal("ControlInterval is nil; the per-Case-mean bound exists")
	}
	if math.Abs(v.ControlInterval.GetLow()-honest.GetLow()) > 1e-12 {
		t.Errorf("ControlInterval low = %v, want %v (per-Case means, n = Case count). "+
			"The flattened shape would compute over n = %d and narrow the bound",
			v.ControlInterval.GetLow(), honest.GetLow(), 2*3)
	}
	if got := v.DeltaControl; math.Abs(got-0.4) > 1e-9 {
		t.Errorf("DeltaControl = %v, want 0.4", got)
	}
}

// TestMeasurementsForSkipsZeroRoutedAssets pins the P1-4 fix: an Asset routed
// to nothing costs no measurements, harm test included. The opposite of this
// test — charging it the control partition — was the 125x over-quote the
// quote-formula fix removed.
func TestMeasurementsForSkipsZeroRoutedAssets(t *testing.T) {
	t.Parallel()

	routing := value.AssetRouting{
		AssetID:           "a",
		CaseIDs:           nil,
		NotMeasuredReason: knov1.RejectionReason_REJECTION_REASON_IRRELEVANT,
	}
	plan := &value.Plan{Trials: 1, ControlCaseIDs: []string{"c1", "c2", "c3"}}
	if got := measurementsFor(routing, plan, "a"); len(got) != 0 {
		t.Errorf("measurementsFor scheduled %d measurements for a zero-routed Asset; "+
			"Plan.Measurements skips it and the schedule must mirror the quote", len(got))
	}
}

// TestNDevIsTheEligiblePool pins the P1-7 fix: n_dev names the dev-split
// population the Asset was routed from, not the control partition — writing
// the partition there understates the population by the reserve fraction and
// any consumer scaling by n_dev gets it wrong by that factor.
func TestNDevIsTheEligiblePool(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	opts := ValueOptions{
		RunID: "run-1",
		Store: st,
		Goal:  fixedDirectionGoal{dir: knov1.Direction_DIRECTION_MAXIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY},
	}
	writeMeasurement(t, st, "run-1", "a", "c1", store.ArmTreatment, 1, 0.9)

	v, err := opts.valuationFor(context.Background(),
		&Asset{Id: "a"},
		value.AssetRouting{AssetID: "a", CaseIDs: []string{"c1"}},
		&value.Plan{Trials: 1, EligibleCases: 50, ControlCaseIDs: make([]string, 15)},
		map[string]store.CaseScore{"c1": {Value: 0.5}},
	)
	if err != nil {
		t.Fatalf("valuationFor: %v", err)
	}
	if v.NDev == nil || *v.NDev != 50 {
		t.Errorf("NDev = %v, want 50 (the eligible pool); the control partition "+
			"holds 15 and is not the population n_dev names", v.NDev)
	}
}

// TestBlendedBaselineIsRefusedUnlessOptedIn pins the P1-5 fix: a baseline that
// resolved more than one model is a mix of estimators, not one estimator, and
// pairing against it would claim a single reference that never existed. This
// is debt #55's marker being read by the first stage that consumes a Baseline.
func TestBlendedBaselineIsRefusedUnlessOptedIn(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	createBaselineRun(t, st, "base-1", []string{"model-a", "model-b"})
	writeBaselineOutcome(t, st, "base-1", "c1", 0.8)

	opts := ValueOptions{
		RunID:         "run-1",
		BaselineRunID: "base-1",
		Store:         st,
	}
	if _, err := opts.baselineCases(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "blended") {
		t.Errorf("baselineCases err = %v, want a blended-model refusal", err)
	}
	opts.UnsafeBaseline = true
	scores, err := opts.baselineCases(context.Background())
	if err != nil {
		t.Fatalf("baselineCases with the opt-in: %v", err)
	}
	if len(scores) != 1 {
		t.Errorf("got %d scores, want 1", len(scores))
	}
}

// TestSingleModelBaselinePassesTheGate is the counter-case: the refusal fires
// on the blend, not on the presence of a baseline.
func TestSingleModelBaselinePassesTheGate(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	createBaselineRun(t, st, "base-1", []string{"model-a"})
	writeBaselineOutcome(t, st, "base-1", "c1", 0.8)

	opts := ValueOptions{
		RunID:         "run-1",
		BaselineRunID: "base-1",
		Store:         st,
	}
	if _, err := opts.baselineCases(context.Background()); err != nil {
		t.Fatalf("baselineCases over a single-model baseline: %v", err)
	}
}

// openTestStore builds an in-memory-backed store in a temp directory.
func openTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.NewSQLite(context.Background(), t.TempDir()+"/kno.db")
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// writeMeasurement records one scored measurement for an (Asset, Case, arm,
// trial) key, creating the Value run row first — the measurements table
// references runs(id), and the FK is part of the store's spend integrity.
func writeMeasurement(t *testing.T, st store.Store, runID, assetID, caseID string, arm store.Arm, trial int32, score float64) {
	t.Helper()
	ensureValueRun(t, st, runID)
	if err := st.RecordMeasurement(context.Background(), runID, &store.Measurement{
		Key: store.MeasurementKey{AssetID: assetID, CaseID: caseID, Arm: arm, Trial: trial},
		Score: &knov1.Score{
			CaseId: caseID,
			Value:  score,
		},
	}); err != nil {
		t.Fatalf("recording measurement: %v", err)
	}
}

// ensureValueRun creates the Value run row a measurements test runs under,
// tolerating a second call for the same run ID.
func ensureValueRun(t *testing.T, st store.Store, runID string) {
	t.Helper()
	run := &knov1.Run{
		Id:            runID,
		Stage:         knov1.Stage_STAGE_VALUE,
		GoalName:      "accuracy",
		GoalDirection: knov1.Direction_DIRECTION_MAXIMIZE,
	}
	if err := st.CreateRun(context.Background(), run); err != nil && !errors.Is(err, store.ErrRunExists) {
		t.Fatalf("creating value run: %v", err)
	}
}

// createBaselineRun writes a finished Baseline run with the given resolved
// models, empty incomplete_reason, and no CaseExecution counts.
func createBaselineRun(t *testing.T, st store.Store, runID string, models []string) {
	t.Helper()
	run := &knov1.Run{
		Id:            runID,
		Stage:         knov1.Stage_STAGE_BASELINE,
		GoalName:      "accuracy",
		GoalDirection: knov1.Direction_DIRECTION_MAXIMIZE,
		CaseExecution: &knov1.CaseExecution{
			ResolvedModels: models,
		},
	}
	if err := st.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("creating baseline run: %v", err)
	}
}

// writeBaselineOutcome records one scored outcome on a baseline run, the shape
// CaseScores reads.
func writeBaselineOutcome(t *testing.T, st store.Store, runID, caseID string, score float64) {
	t.Helper()
	if err := st.RecordOutcome(context.Background(), runID, &store.Outcome{
		CaseID: caseID,
		Score:  &knov1.Score{CaseId: caseID, Value: score, Passed: true},
	}); err != nil {
		t.Fatalf("recording baseline outcome: %v", err)
	}
}

// stubAgent implements Agent plus ContextInjector.
type stubAgent struct{}

func (stubAgent) Invoke(context.Context, *Case) (*Response, error) { return &Response{}, nil }

func (stubAgent) WithContext(*Asset) (Agent, error) { return stubAgent{}, nil }

// uncapableAgent is an Agent that cannot carry an Asset. It deliberately does
// NOT embed stubAgent: embedding would inherit WithContext and silently turn
// the fixture into a capable one.
type uncapableAgent struct{}

func (uncapableAgent) Invoke(context.Context, *Case) (*Response, error) { return &Response{}, nil }

// lyingAgent implements ContextInjector but declares context_inject false —
// the adapter that answers anyway, which validate refuses with the sharper
// message.
type lyingAgent struct{ stubAgent }

func (lyingAgent) Capabilities() *knov1.Capabilities { return &knov1.Capabilities{} }

// emptyEvals yields no Cases — enough for validate, which only needs the
// seal to exist.
type emptyEvals struct{}

func (emptyEvals) Cases(context.Context) (iter.Seq2[*Case, error], error) {
	return func(func(*Case, error) bool) {}, nil
}

// stubPool supplies a fixed list of Assets.
type stubPool struct{ assets []*Asset }

func (p stubPool) Assets(_ context.Context) (iter.Seq2[*Asset, error], error) {
	return func(yield func(*Asset, error) bool) {
		for _, a := range p.assets {
			if !yield(a, nil) {
				return
			}
		}
	}, nil
}

// TestValueValidatesEverythingRefusableBeforeSpend drives every refusal
// branch: each one is free, and the alternative to each is a full-price run
// whose output reads as a result.
func TestValueValidatesEverythingRefusableBeforeSpend(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	full := ValueOptions{
		RunID:         "run-1",
		BaselineRunID: "base-1",
		Agent:         stubAgent{},
		Goal:          fixedDirectionGoal{dir: knov1.Direction_DIRECTION_MAXIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY},
		Guard:         &budget.Guard{},
		Store:         st,
		Evals:         Seal(&emptyEvals{}),
	}
	pool := stubPool{assets: []*Asset{{Id: "a"}}}

	cases := []struct {
		name string
		opts ValueOptions
		want string
	}{
		{"no run ID", func() ValueOptions { o := full; o.RunID = ""; return o }(), "run ID"},
		{"no baseline", func() ValueOptions { o := full; o.BaselineRunID = ""; return o }(), "baseline run"},
		{"no agent", func() ValueOptions { o := full; o.Agent = nil; return o }(), "agent"},
		{"no goal", func() ValueOptions { o := full; o.Goal = nil; return o }(), "goal"},
		{"no store", func() ValueOptions { o := full; o.Store = nil; return o }(), "store"},
		{"no guard", func() ValueOptions { o := full; o.Guard = nil; return o }(), "budget guard"},
		{"no pool", full, "pool"},
		{"agent cannot inject", func() ValueOptions { o := full; o.Agent = uncapableAgent{}; return o }(), "cannot carry an Asset"},
		{"agent lies about injecting", func() ValueOptions { o := full; o.Agent = lyingAgent{}; return o }(), "context_inject false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.opts.validate(pool)
			if tc.name == "no pool" {
				err = tc.opts.validate(nil)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("validate err = %v, want mention of %q", err, tc.want)
			}
		})
	}
	if err := full.validate(pool); err != nil {
		t.Errorf("a complete option set refused: %v", err)
	}
}

// TestBaselineCasesRefusesEverythingThatIsNotABaseline drives the stage,
// incomplete, and empty-scores refusals.
func TestBaselineCasesRefusesEverythingThatIsNotABaseline(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	opts := ValueOptions{BaselineRunID: "base-1", Store: st}

	wrong, err := st.GetRun(context.Background(), "base-1")
	_ = wrong
	if err == nil {
		t.Fatal("expected GetRun on a missing run to error")
	}

	// Wrong stage.
	ensureValueRun(t, st, "base-1")
	if _, err := opts.baselineCases(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "not a baseline") {
		t.Errorf("err = %v, want the wrong-stage refusal", err)
	}

	// Incomplete baseline.
	st2 := openTestStore(t)
	inc := &knov1.Run{
		Id: "base-2", Stage: knov1.Stage_STAGE_BASELINE, GoalName: "accuracy",
		GoalDirection:    knov1.Direction_DIRECTION_MAXIMIZE,
		IncompleteReason: "error_rate_exceeded",
	}
	if err := st2.CreateRun(context.Background(), inc); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	opts2 := ValueOptions{BaselineRunID: "base-2", Store: st2}
	if _, err := opts2.baselineCases(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "incomplete") {
		t.Errorf("err = %v, want the incomplete-baseline refusal", err)
	}

	// Completed baseline with no scores.
	st3 := openTestStore(t)
	createBaselineRun(t, st3, "base-3", []string{"model-a"})
	opts3 := ValueOptions{BaselineRunID: "base-3", Store: st3}
	if _, err := opts3.baselineCases(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "no scores") {
		t.Errorf("err = %v, want the no-scores refusal", err)
	}
}

// TestCaseRefsKeepsTheScoreOutOfTheRouter drives the unpairable counting and
// the error path — the ONLY place a baseline score becomes a routing input,
// and it becomes a bool.
func TestCaseRefsKeepsTheScoreOutOfTheRouter(t *testing.T) {
	t.Parallel()

	cases := []*Case{
		{Id: "c1"},
		{Id: "c2"},
		{Id: "c3"},
	}
	seq := func(yield func(*Case, error) bool) {
		for _, c := range cases {
			if !yield(c, nil) {
				return
			}
		}
	}
	scores := map[string]store.CaseScore{
		"c1": {Value: 0.9, Passed: true},
		"c2": {Unrecoverable: true},
		// c3 was never scored in the baseline.
	}
	refs, unpairable, err := caseRefs(seq, scores)
	if err != nil {
		t.Fatalf("caseRefs: %v", err)
	}
	if len(refs) != 1 || refs[0].ID != "c1" || refs[0].Failed {
		t.Errorf("refs = %+v, want just c1, passed", refs)
	}
	if unpairable != 2 {
		t.Errorf("unpairable = %d, want 2 (unrecoverable + never scored)", unpairable)
	}

	// A source error is fatal, not a partial list.
	broken := func(yield func(*Case, error) bool) {
		yield(nil, errors.New("source is gone"))
	}
	if _, _, err := caseRefs(broken, scores); err == nil {
		t.Error("caseRefs over an erroring source returned no error")
	}
}

// TestMeasurementsForMirrorsTheQuote covers the routed paths: both arms when
// the selection conditioned on the baseline, one arm when it did not, trials
// expansion, and the control partition.
func TestMeasurementsForMirrorsTheQuote(t *testing.T) {
	t.Parallel()

	routing := value.AssetRouting{
		AssetID:         "a",
		CaseIDs:         []string{"c1", "c2"},
		FreshControlArm: true,
	}
	plan := &value.Plan{Trials: 2, ControlCaseIDs: []string{"c3"}}
	got := measurementsFor(routing, plan, "a")
	// 2 routed Cases x 2 arms x 2 trials + 1 control Case x 1 arm x 2 trials.
	if len(got) != 10 {
		t.Fatalf("scheduled %d measurements, want 10", len(got))
	}

	routing.FreshControlArm = false
	got = measurementsFor(routing, plan, "a")
	// 2 routed x 1 arm x 2 trials + 1 control x 1 arm x 2 trials.
	if len(got) != 6 {
		t.Fatalf("scheduled %d measurements without a fresh control arm, want 6", len(got))
	}
}

// TestAssertQuoteBoundsRefusesAnOverScheduledRun covers the consent check the
// loop start wires: a schedule above the quoted number means the consent
// prompt under-stated the run.
func TestAssertQuoteBoundsRefusesAnOverScheduledRun(t *testing.T) {
	t.Parallel()

	plan := &value.Plan{
		Trials:         1,
		Routed:         []value.AssetRouting{{AssetID: "a", CaseIDs: []string{"c1", "c2"}, FreshControlArm: true}},
		ControlCaseIDs: []string{"c3"},
	}
	// 2x2 + 1 = 5 quoted.
	if err := assertQuoteBounds(5, plan); err != nil {
		t.Errorf("a schedule matching the quote refused: %v", err)
	}
	if err := assertQuoteBounds(6, plan); err == nil {
		t.Error("a schedule above the quote passed; the user consented to a " +
			"smaller run than the one that would execute")
	}
}

// TestValuationForZeroRoutedAssetsCarriesTheReason pins the passthrough:
// an Asset measured against zero Cases is a result, and not_measured carries
// why.
func TestValuationForZeroRoutedAssetsCarriesTheReason(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ensureValueRun(t, st, "run-1")
	opts := ValueOptions{
		RunID: "run-1",
		Store: st,
		Goal:  fixedDirectionGoal{dir: knov1.Direction_DIRECTION_MAXIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY},
	}
	v, err := opts.valuationFor(context.Background(),
		&Asset{Id: "a"},
		value.AssetRouting{AssetID: "a", NotMeasuredReason: knov1.RejectionReason_REJECTION_REASON_IRRELEVANT},
		&value.Plan{Trials: 1, EligibleCases: 42},
		nil,
	)
	if err != nil {
		t.Fatalf("valuationFor: %v", err)
	}
	if v.NotMeasured != knov1.RejectionReason_REJECTION_REASON_IRRELEVANT {
		t.Errorf("NotMeasured = %v, want IRRELEVANT carried through", v.NotMeasured)
	}
	if v.NDev == nil || *v.NDev != 42 {
		t.Errorf("NDev = %v, want 42 even for a zero-routed Asset", v.NDev)
	}
}

// TestValueWiresEveryInvokerHook is debt #77's trigger discharging: Value is
// the second invoker caller, and a stage that forgets a hook is silent about
// it. Both hooks must be non-nil or the money events the quote denominates go
// unreported.
func TestValueWiresEveryInvokerHook(t *testing.T) {
	t.Parallel()

	iv := ValueOptions{}.invoker(store.MeasurementKey{AssetID: "a", Trial: 1}, store.ArmTreatment, stubAgent{}, &valueEmitter{})
	if iv.OnOvershoot == nil {
		t.Error("Value wires no OnOvershoot hook, so a settlement overshoot — " +
			"money spent past its reservation — would go unreported and look " +
			"identical to a run that never overshot")
	}
	if iv.OnRetry == nil {
		t.Error("Value wires no OnRetry hook, so a run obeying a provider's " +
			"backoff would be indistinguishable from a hung one")
	}
}

// pricingAgent is an Agent plus Estimator whose estimate is settable, so the
// unpriceable refusals can be driven through the real path.
type pricingAgent struct {
	stubAgent
	estimate budget.Estimate
	estErr   error
}

func (p pricingAgent) Estimate(context.Context, *Case) (budget.Estimate, error) {
	return p.estimate, p.estErr
}

func (pricingAgent) WorstCase() budget.Estimate { return budget.Estimate{Calls: 1} }

// WithContext keeps the Estimator on the treatment arm: the embedded
// stubAgent's own WithContext would strip the pricing behavior, and the
// unpriceable path must be exercised through the wrapper that actually runs.
func (p pricingAgent) WithContext(*Asset) (Agent, error) {
	return pricingAgent{estimate: p.estimate, estErr: p.estErr}, nil
}

var _ Estimator = pricingAgent{}

// TestEqualPlansNamesItsTerms drives every field the resume consent compares.
func TestEqualPlansNamesItsTerms(t *testing.T) {
	t.Parallel()

	base := func() *value.Plan {
		return &value.Plan{
			Mode:           value.ModeAllDev,
			Trials:         1,
			Seed:           7,
			EligibleCases:  30,
			ControlCaseIDs: []string{"c1", "c2"},
			Routed: []value.AssetRouting{{
				AssetID: "a1", CaseIDs: []string{"c3"}, FreshControlArm: true,
			}},
		}
	}
	if !equalPlans(base(), base()) {
		t.Fatal("a plan must equal itself")
	}
	cases := []struct {
		name   string
		mutate func(*value.Plan)
	}{
		{"mode", func(p *value.Plan) { p.Mode = value.ModeTagOverlap }},
		{"trials", func(p *value.Plan) { p.Trials = 3 }},
		{"seed", func(p *value.Plan) { p.Seed = 99 }},
		{"eligible", func(p *value.Plan) { p.EligibleCases = 40 }},
		{"control cases", func(p *value.Plan) { p.ControlCaseIDs = []string{"c9"} }},
		{"routed count", func(p *value.Plan) { p.Routed = append(p.Routed, value.AssetRouting{AssetID: "a2"}) }},
		{"routed asset", func(p *value.Plan) { p.Routed[0].AssetID = "a9" }},
		{"routed fresh", func(p *value.Plan) { p.Routed[0].FreshControlArm = false }},
		{"routed reason", func(p *value.Plan) {
			p.Routed[0].NotMeasuredReason = knov1.RejectionReason_REJECTION_REASON_IRRELEVANT
		}},
		{"routed cases", func(p *value.Plan) { p.Routed[0].CaseIDs = []string{"c8"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := base()
			tc.mutate(b)
			if equalPlans(base(), b) {
				t.Errorf("equalPlans passed despite a change to %s; the resume "+
					"consent check would continue under a drifted plan", tc.name)
			}
		})
	}
}

// TestEstimateRefusesWhatACapCannotEnforce: a dollar cap plus an agent that
// cannot price the call is refused before any spend, the same rule Baseline
// enforces.
func TestEstimateRefusesWhatACapCannotEnforce(t *testing.T) {
	t.Parallel()

	guard := budget.New(budget.Limits{MaxCostUSDMicros: 1000}, nil, 0)
	opts := ValueOptions{
		Guard: guard,
		RunID: "run-1",
		Evals: Seal(&emptyEvals{}),
		Store: openTestStore(t),
	}
	c := &Case{Id: "c1"}

	// An estimate error under a cap is fatal.
	bad := pricingAgent{estErr: errors.New("no price row")}
	if _, err := opts.estimate(context.Background(), bad, c); err == nil {
		t.Error("an unpriceable Case under a dollar cap was accepted")
	}

	// A zero estimate under a cap is fatal.
	zero := pricingAgent{estimate: budget.Estimate{Calls: 1}}
	if _, err := opts.estimate(context.Background(), zero, c); err == nil {
		t.Error("a zero estimate under a dollar cap was accepted; the cap " +
			"would be discovered at settlement instead of before spend")
	}

	// Without a cap, an unpriceable agent falls back to the flat estimate.
	if est, err := (&ValueOptions{
		Guard:                   budget.New(budget.Limits{}, nil, 0),
		EstCostPerCallUSDMicros: 42,
	}).estimate(context.Background(), bad, c); err != nil || est.CostUSDMicros != 42 {
		t.Errorf("uncapped fallback = %v, %v; want 42, nil", est, err)
	}
}

// failingValueStore fails one chosen store operation, so the loop's error
// paths are exercised rather than assumed.
type failingValueStore struct {
	store.Store
	failAppend    bool
	failCreate    bool
	failFinish    bool
	failGetRun    bool
	failMeasure   bool
	failRecord    bool
	failCompleted bool
	failValuation bool
}

var errValueStore = errors.New("value store is unavailable")

func (f *failingValueStore) AppendEvent(ctx context.Context, ev *knov1.Event) error {
	if f.failAppend {
		return errValueStore
	}
	return f.Store.AppendEvent(ctx, ev)
}

func (f *failingValueStore) CreateRun(ctx context.Context, r *knov1.Run) error {
	if f.failCreate {
		return errValueStore
	}
	return f.Store.CreateRun(ctx, r)
}

func (f *failingValueStore) FinishRun(ctx context.Context, r *knov1.Run) error {
	if f.failFinish {
		return errValueStore
	}
	return f.Store.FinishRun(ctx, r)
}

func (f *failingValueStore) GetRun(ctx context.Context, id string) (*knov1.Run, error) {
	if f.failGetRun {
		return nil, errValueStore
	}
	return f.Store.GetRun(ctx, id)
}

func (f *failingValueStore) Measurements(ctx context.Context, runID, assetID string) ([]store.RecordedMeasurement, error) {
	if f.failMeasure {
		return nil, errValueStore
	}
	return f.Store.Measurements(ctx, runID, assetID)
}

func (f *failingValueStore) RecordMeasurement(ctx context.Context, runID string, m *store.Measurement) error {
	if f.failRecord {
		return errValueStore
	}
	return f.Store.RecordMeasurement(ctx, runID, m)
}

func (f *failingValueStore) CompletedMeasurements(ctx context.Context, runID string) (map[store.MeasurementKey]struct{}, error) {
	if f.failCompleted {
		return nil, errValueStore
	}
	return f.Store.CompletedMeasurements(ctx, runID)
}

func (f *failingValueStore) WriteValuation(ctx context.Context, runID string, v *knov1.Valuation) error {
	if f.failValuation {
		return errValueStore
	}
	return f.Store.WriteValuation(ctx, runID, v)
}

// TestValueSurfacesStoreFailures: a failing store ends the run with the
// failure, never with a half-written result presented as complete.
func TestValueSurfacesStoreFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		apply func(*failingValueStore)
	}{
		{"create run", func(f *failingValueStore) { f.failCreate = true }},
		{"append event", func(f *failingValueStore) { f.failAppend = true }},
		{"finish run", func(f *failingValueStore) { f.failFinish = true }},
		{"write valuation", func(f *failingValueStore) { f.failValuation = true }},
		{"measurements", func(f *failingValueStore) { f.failMeasure = true }},
		{"record measurement", func(f *failingValueStore) { f.failRecord = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner := openTestStore(t)
			failing := &failingValueStore{Store: inner}
			tc.apply(failing)

			// A baseline with one score, one Case measured: the failure fires
			// on the chosen operation.
			run := &knov1.Run{
				Id: "base-1", Stage: knov1.Stage_STAGE_BASELINE, GoalName: "g",
				GoalDirection: knov1.Direction_DIRECTION_MAXIMIZE,
				CaseExecution: &knov1.CaseExecution{ResolvedModels: []string{"m"}},
			}
			if err := inner.CreateRun(context.Background(), run); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			if err := inner.RecordOutcome(context.Background(), "base-1", &store.Outcome{
				CaseID: "c1", Score: &knov1.Score{CaseId: "c1", Value: 0.9, Passed: true},
			}); err != nil {
				t.Fatalf("RecordOutcome: %v", err)
			}
			cases := &caseSource{list: []*Case{
				{Id: "c1", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV},
				{Id: "c2", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV},
			}}
			opts := ValueOptions{
				RunID: "run-1", BaselineRunID: "base-1",
				Agent: stubAgent{}, AgentRef: &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
				Goal:        fixedDirectionGoal{dir: knov1.Direction_DIRECTION_MAXIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY},
				GoalName:    "g",
				Guard:       budget.New(budget.Limits{}, nil, 0),
				Store:       failing,
				Evals:       Seal(cases),
				Concurrency: 2,
				Routing:     value.Options{Seed: 1},
			}
			pool := stubPool{assets: []*Asset{{Id: "a1"}}}
			if _, err := opts.Value(context.Background(), pool); err == nil {
				t.Errorf("Value over a store failing at %s returned no error", tc.name)
			}
		})
	}
}

// caseSource is an Evals over a fixed list, for the failure tests.
type caseSource struct{ list []*Case }

func (c *caseSource) Cases(context.Context) (iter.Seq2[*Case, error], error) {
	return func(yield func(*Case, error) bool) {
		for _, cs := range c.list {
			if !yield(cs, nil) {
				return
			}
		}
	}, nil
}

// TestLoopHelpersCoverTheCorners drives the small branches the end-to-end
// runs never reach: the retry-bound resolution, the key projection helpers,
// and the emit-failure recorder.
func TestLoopHelpersCoverTheCorners(t *testing.T) {
	t.Parallel()

	opts := ValueOptions{MaxAttempts: 7, RetryBudget: 123, RetryBackoff: 456}
	if opts.maxAttempts() != 7 || opts.retryBudget() != 123 ||
		opts.retryBackoff() != 456 {
		t.Error("explicit retry bounds did not win over the defaults")
	}
	opts = ValueOptions{}
	if opts.maxAttempts() != DefaultMaxAttempts ||
		opts.retryBudget() != DefaultRetryBudget ||
		opts.retryBackoff() != DefaultRetryBackoff {
		t.Error("the defaults did not resolve")
	}

	if valueStringPtr("") != nil || *valueStringPtr("x") != "x" {
		t.Error("valueStringPtr does not distinguish empty from set")
	}
	if valueArm(store.ArmUnspecified) != nil || *valueArm(store.ArmTreatment) != knov1.Arm_ARM_TREATMENT {
		t.Error("valueArm does not project the measurement arm")
	}
	if valueTrialPtr(0) != nil || *valueTrialPtr(3) != 3 {
		t.Error("valueTrialPtr does not distinguish unset from set")
	}

	em := &valueEmitter{}
	em.recordEmitFailure(nil)
	if em.emitFailure.Load() != nil {
		t.Error("a nil failure was recorded")
	}
	err := errors.New("first failure")
	em.recordEmitFailure(err)
	if got := em.emitFailure.Load(); got == nil || *got != err {
		t.Error("the first failure was not the one kept")
	}
	em.recordEmitFailure(errors.New("second failure"))
	if got := em.emitFailure.Load(); got == nil || *got != err {
		t.Error("a second failure displaced the first")
	}

	// perCaseMeans refuses the ragged shape it exists to refuse.
	if _, _, ok := perCaseMeans([][]float64{{0.5}, {0.6, 0.7}}); ok {
		t.Error("perCaseMeans accepted ragged vectors")
	}
	if means, trials, ok := perCaseMeans(nil); ok || means != nil || trials != 0 {
		t.Errorf("perCaseMeans(nil) = %v, %d, %v; want a refusal", means, trials, ok)
	}
}

// erroringPool is a Pool whose source is broken, for the read-error path.
type erroringPool struct{}

func (erroringPool) Assets(context.Context) (iter.Seq2[*Asset, error], error) {
	return func(yield func(*Asset, error) bool) { yield(nil, errors.New("pool source is gone")) }, nil
}

// erroringInjector builds a treatment arm that cannot be built.
type erroringInjector struct{ stubAgent }

func (erroringInjector) WithContext(*Asset) (Agent, error) {
	return nil, errors.New("injection failed")
}

// TestValueRefusesAnUnpriceableRunBeforeSpend: an estimator that errors under
// a dollar cap fails the measurement before Authorize, and the row records
// the failure rather than the spend.
func TestValueRefusesAnUnpriceableRunBeforeSpend(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	createBaselineRun(t, st, "base-1", []string{"m"})
	if err := st.RecordOutcome(context.Background(), "base-1", &store.Outcome{
		CaseID: "c1", Score: &knov1.Score{CaseId: "c1", Value: 0.9, Passed: true},
	}); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	cases := &caseSource{list: []*Case{
		{Id: "c1", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV},
		{Id: "c2", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV},
	}}
	opts := ValueOptions{
		RunID: "run-1", BaselineRunID: "base-1",
		Agent:    pricingAgent{estErr: errors.New("no price row")},
		AgentRef: &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
		Goal:     fixedDirectionGoal{dir: knov1.Direction_DIRECTION_MAXIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY},
		GoalName: "g",
		Guard:    budget.New(budget.Limits{MaxCostUSDMicros: 1000}, nil, 0),
		Store:    st, Evals: Seal(cases),
		Concurrency: 2,
		Routing:     value.Options{Seed: 1},
	}
	// The refusal is per-measurement and lands BEFORE Authorize: the run
	// completes with every measurement errored, and the provider was never
	// called — the unpriceable Case cost nothing.
	res, err := opts.Value(context.Background(), stubPool{assets: []*Asset{{Id: "a1"}}})
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	spent, err := st.SettledSpend(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if spent.Calls != 0 {
		t.Errorf("an unpriceable run settled %d calls; the refusal must land "+
			"before Authorize", spent.Calls)
	}
	rows, err := st.Measurements(context.Background(), "run-1", "a1")
	if err != nil {
		t.Fatalf("Measurements: %v", err)
	}
	for _, m := range rows {
		if m.Err == "" {
			t.Errorf("measurement %s/%s scored despite the unpriceable estimate", m.Key.AssetID, m.Key.CaseID)
		}
	}
	_ = res
}

// TestValueRefusesBrokenSources: a pool that cannot be read, an evals source
// that cannot be read, and an injector that cannot build the treatment arm
// all refuse with the failure, not a partial run.
func TestValueRefusesBrokenSources(t *testing.T) {
	t.Parallel()

	t.Run("broken pool", func(t *testing.T) {
		t.Parallel()
		st := openTestStore(t)
		createBaselineRun(t, st, "base-1", []string{"m"})
		opts := ValueOptions{
			RunID: "run-1", BaselineRunID: "base-1",
			Agent: stubAgent{}, AgentRef: &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
			Goal:     fixedDirectionGoal{dir: knov1.Direction_DIRECTION_MAXIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY},
			GoalName: "g",
			Guard:    budget.New(budget.Limits{}, nil, 0),
			Store:    st, Evals: Seal(&caseSource{}),
			Routing: value.Options{Seed: 1},
		}
		if _, err := opts.Value(context.Background(), erroringPool{}); err == nil {
			t.Error("a broken pool source produced no error")
		}
	})

	t.Run("broken evals", func(t *testing.T) {
		t.Parallel()
		st := openTestStore(t)
		createBaselineRun(t, st, "base-1", []string{"m"})
		opts := ValueOptions{
			RunID: "run-1", BaselineRunID: "base-1",
			Agent: stubAgent{}, AgentRef: &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
			Goal:     fixedDirectionGoal{dir: knov1.Direction_DIRECTION_MAXIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY},
			GoalName: "g",
			Guard:    budget.New(budget.Limits{}, nil, 0),
			Store:    st,
			Evals:    Seal(&erroringCaseSource{}),
			Routing:  value.Options{Seed: 1},
		}
		if _, err := opts.Value(context.Background(), stubPool{}); err == nil {
			t.Error("a broken evals source produced no error")
		}
	})

	t.Run("broken injector", func(t *testing.T) {
		t.Parallel()
		st := openTestStore(t)
		createBaselineRun(t, st, "base-1", []string{"m"})
		if err := st.RecordOutcome(context.Background(), "base-1", &store.Outcome{
			CaseID: "c1", Score: &knov1.Score{CaseId: "c1", Value: 0.9, Passed: true},
		}); err != nil {
			t.Fatalf("RecordOutcome: %v", err)
		}
		cases := &caseSource{list: []*Case{
			{Id: "c1", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV},
			{Id: "c2", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV},
		}}
		opts := ValueOptions{
			RunID: "run-1", BaselineRunID: "base-1",
			Agent: erroringInjector{}, AgentRef: &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
			Goal:     fixedDirectionGoal{dir: knov1.Direction_DIRECTION_MAXIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY},
			GoalName: "g",
			Guard:    budget.New(budget.Limits{}, nil, 0),
			Store:    st, Evals: Seal(cases),
			Routing: value.Options{Seed: 1},
		}
		if _, err := opts.Value(context.Background(), stubPool{assets: []*Asset{{Id: "a1"}}}); err == nil {
			t.Error("an injector that cannot build the treatment arm produced no error")
		}
	})
}

// erroringCaseSource is an Evals whose iterator fails.
type erroringCaseSource struct{}

func (erroringCaseSource) Cases(context.Context) (iter.Seq2[*Case, error], error) {
	return func(yield func(*Case, error) bool) {
		yield(nil, errors.New("evals source is gone"))
	}, nil
}

// panickyGoal panics on Score, so the workFunc panic guard can be driven
// through a real run rather than asserted against.
type panickyGoal struct{}

func (panickyGoal) Score(context.Context, *Case, *Response) (*Score, error) {
	panic("the goal blew up")
}

func (panickyGoal) Direction() Direction { return knov1.Direction_DIRECTION_MAXIMIZE }

func (panickyGoal) Domain() ScoreDomain { return knov1.ScoreDomain_SCORE_DOMAIN_BINARY }

// TestValueSurvivesAPanickingGoal: a panic in the Ring-1 plug-in point must
// not take the run or the paid call down with it.
func TestValueSurvivesAPanickingGoal(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	createBaselineRun(t, st, "base-1", []string{"m"})
	if err := st.RecordOutcome(context.Background(), "base-1", &store.Outcome{
		CaseID: "c1", Score: &knov1.Score{CaseId: "c1", Value: 0.9, Passed: true},
	}); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	cases := &caseSource{list: []*Case{
		{Id: "c1", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV},
		{Id: "c2", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV},
	}}
	opts := ValueOptions{
		RunID: "run-1", BaselineRunID: "base-1",
		Agent: stubAgent{}, AgentRef: &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
		Goal: panickyGoal{}, GoalName: "panicky",
		Guard: budget.New(budget.Limits{}, nil, 0),
		Store: st, Evals: Seal(cases),
		Concurrency: 2,
		Routing:     value.Options{Seed: 1},
	}
	res, err := opts.Value(context.Background(), stubPool{assets: []*Asset{{Id: "a1"}}})
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if res.Status != knov1.RunStatus_RUN_STATUS_COMPLETED {
		t.Errorf("Status = %v; a panicking goal must not stop the run", res.Status)
	}
	rows, err := st.Measurements(context.Background(), "run-1", "a1")
	if err != nil {
		t.Fatalf("Measurements: %v", err)
	}
	for _, m := range rows {
		if !strings.Contains(m.Err, "panic") {
			t.Errorf("measurement %s/%s err = %q, want the panic recorded",
				m.Key.AssetID, m.Key.CaseID, m.Err)
		}
	}
}

// TestValueRefusesAnEmptyPool: routing has nothing to do, and the refusal is
// free.
func TestValueRefusesAnEmptyPool(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	createBaselineRun(t, st, "base-1", []string{"m"})
	opts := ValueOptions{
		RunID: "run-1", BaselineRunID: "base-1",
		Agent: stubAgent{}, AgentRef: &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
		Goal:     fixedDirectionGoal{dir: knov1.Direction_DIRECTION_MAXIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY},
		GoalName: "g",
		Guard:    budget.New(budget.Limits{}, nil, 0),
		Store:    st, Evals: Seal(&caseSource{}),
		Routing: value.Options{Seed: 1},
	}
	if _, err := opts.Value(context.Background(), stubPool{}); err == nil {
		t.Error("an empty pool produced no refusal")
	}
}

// TestValueResumeSurfacesAStoreFailure: the resume path's GetRun failure is
// returned, not guessed around.
func TestValueResumeSurfacesAStoreFailure(t *testing.T) {
	t.Parallel()

	inner := openTestStore(t)
	createBaselineRun(t, inner, "base-1", []string{"m"})
	failing := &failingValueStore{Store: inner, failGetRun: true}
	opts := ValueOptions{
		RunID: "run-1", BaselineRunID: "base-1", Resume: true,
		Agent: stubAgent{}, AgentRef: &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
		Goal:     fixedDirectionGoal{dir: knov1.Direction_DIRECTION_MAXIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY},
		GoalName: "g",
		Guard:    budget.New(budget.Limits{}, nil, 0),
		Store:    failing, Evals: Seal(&caseSource{}),
		Routing: value.Options{Seed: 1},
	}
	if _, err := opts.Value(context.Background(), stubPool{assets: []*Asset{{Id: "a1"}}}); err == nil {
		t.Error("a resume over a failing store produced no error")
	}
}

// TestCasesSeqHonorsEarlyBreak: the iterator adapter must stop when the
// consumer stops.
func TestCasesSeqHonorsEarlyBreak(t *testing.T) {
	t.Parallel()

	seen := 0
	casesSeq([]*Case{{Id: "c1"}, {Id: "c2"}, {Id: "c3"}})(func(*Case, error) bool {
		seen++
		return seen < 2
	})
	if seen != 2 {
		t.Errorf("seen = %d, want 2: the consumer broke and the iterator kept yielding", seen)
	}
}

// evalsOpenError is an Evals whose OPEN fails, the branch casesByID's
// materialization path cannot produce through the iterator.
type evalsOpenError struct{}

func (evalsOpenError) Cases(context.Context) (iter.Seq2[*Case, error], error) {
	return nil, errors.New("evals source cannot be opened")
}

// TestValueRefusesAtEveryEntryPoint drives the stage's own error returns —
// not the helpers', the stage's: an invalid option set, a baseline that is
// not a baseline, and an evals source that cannot be opened.
func TestValueRefusesAtEveryEntryPoint(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	base := ValueOptions{
		RunID: "run-1", BaselineRunID: "base-1",
		Agent: stubAgent{}, AgentRef: &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
		Goal:     fixedDirectionGoal{dir: knov1.Direction_DIRECTION_MAXIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY},
		GoalName: "g",
		Guard:    budget.New(budget.Limits{}, nil, 0),
		Store:    st, Evals: Seal(&caseSource{}),
		Routing: value.Options{Seed: 1},
	}
	pool := stubPool{assets: []*Asset{{Id: "a1"}}}

	t.Run("invalid options", func(t *testing.T) {
		t.Parallel()
		opts := base
		opts.Store = nil
		if _, err := opts.Value(context.Background(), pool); err == nil {
			t.Error("an invalid option set ran the stage")
		}
	})
	t.Run("wrong-stage baseline", func(t *testing.T) {
		t.Parallel()
		ensureValueRun(t, st, "base-1") // a VALUE run where a baseline is required
		if _, err := base.Value(context.Background(), pool); err == nil {
			t.Error("a non-baseline reference was accepted")
		}
	})
	t.Run("unopenable evals", func(t *testing.T) {
		t.Parallel()
		opts := base
		opts.Evals = Seal(evalsOpenError{})
		if _, err := opts.Value(context.Background(), pool); err == nil {
			t.Error("an evals source that cannot be opened was accepted")
		}
	})
}

// TestAppendRefusesAfterRunFinished: the emitter's closed branch exists
// because the schema promises RunFinished is last.
func TestAppendRefusesAfterRunFinished(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ensureValueRun(t, st, "run-1")
	opts := ValueOptions{RunID: "run-1", Store: st}
	em := &valueEmitter{}
	if err := opts.append(context.Background(), em, func() *knov1.Event {
		return &knov1.Event{Payload: &knov1.Event_RunFinished{RunFinished: &knov1.RunFinished{}}}
	}, "run-finished"); err != nil {
		t.Fatalf("closing append: %v", err)
	}
	if err := opts.append(context.Background(), em, func() *knov1.Event {
		return &knov1.Event{Payload: &knov1.Event_RunStarted{RunStarted: &knov1.RunStarted{}}}
	}, "run-started"); err == nil {
		t.Error("an append after RunFinished was accepted")
	}
}

// erroringGoal fails Score, so the workFunc scoring path is driven through a
// real run.
type erroringGoal struct{}

func (erroringGoal) Score(context.Context, *Case, *Response) (*Score, error) {
	return nil, errors.New("judge unavailable")
}

func (erroringGoal) Direction() Direction { return knov1.Direction_DIRECTION_MAXIMIZE }

func (erroringGoal) Domain() ScoreDomain { return knov1.ScoreDomain_SCORE_DOMAIN_BINARY }

// TestValueRecordsScoreFailures: a failing judge ends the measurement with
// the failure recorded, never with a zero score.
func TestValueRecordsScoreFailures(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	createBaselineRun(t, st, "base-1", []string{"m"})
	if err := st.RecordOutcome(context.Background(), "base-1", &store.Outcome{
		CaseID: "c1", Score: &knov1.Score{CaseId: "c1", Value: 0.9, Passed: true},
	}); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	cases := &caseSource{list: []*Case{
		{Id: "c1", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV},
		{Id: "c2", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV},
	}}
	opts := ValueOptions{
		RunID: "run-1", BaselineRunID: "base-1",
		Agent: stubAgent{}, AgentRef: &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
		Goal: erroringGoal{}, GoalName: "g",
		Guard: budget.New(budget.Limits{}, nil, 0),
		Store: st, Evals: Seal(cases),
		Concurrency: 2,
		Routing:     value.Options{Seed: 1},
	}
	res, err := opts.Value(context.Background(), stubPool{assets: []*Asset{{Id: "a1"}}})
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	rows, err := st.Measurements(context.Background(), "run-1", "a1")
	if err != nil {
		t.Fatalf("Measurements: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the run recorded nothing; a judge failure is a result, not an omission")
	}
	for _, m := range rows {
		if m.Err == "" {
			t.Errorf("measurement %s/%s scored despite the failing judge", m.Key.AssetID, m.Key.CaseID)
		}
	}
	_ = res
}

// cancelingGoal cancels the run's context on its first Score, so the mid-run
// interruption path is driven through a real loop rather than asserted
// against a pre-cancelled context (which refuses at the baseline read).
type cancelingGoal struct{ cancel context.CancelFunc }

func (c cancelingGoal) Score(context.Context, *Case, *Response) (*Score, error) {
	c.cancel()
	return &knov1.Score{Value: 1}, nil
}

func (cancelingGoal) Direction() Direction { return knov1.Direction_DIRECTION_MAXIMIZE }

func (cancelingGoal) Domain() ScoreDomain { return knov1.ScoreDomain_SCORE_DOMAIN_BINARY }

// TestValueInterruptsResumably: a cancelled context stops the loop with
// INTERRUPTED — a resumable stop, never a failure.
func TestValueInterruptsResumably(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	createBaselineRun(t, st, "base-1", []string{"m"})
	var cases []*Case
	for i := range 40 {
		cases = append(cases, &Case{
			Id:    "c" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV,
		})
	}
	for _, c := range cases {
		if err := st.RecordOutcome(context.Background(), "base-1", &store.Outcome{
			CaseID: c.GetId(), Score: &knov1.Score{CaseId: c.GetId(), Value: 0.9, Passed: true},
		}); err != nil {
			t.Fatalf("RecordOutcome: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	opts := ValueOptions{
		RunID: "run-1", BaselineRunID: "base-1",
		Agent: stubAgent{}, AgentRef: &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
		Goal: cancelingGoal{cancel: cancel}, GoalName: "g",
		Guard: budget.New(budget.Limits{}, nil, 0),
		Store: st, Evals: Seal(&caseSource{list: cases}),
		Concurrency: 2,
		Routing:     value.Options{Seed: 1},
	}
	res, err := opts.Value(ctx, stubPool{assets: []*Asset{{Id: "a1"}}})
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if res.Status != knov1.RunStatus_RUN_STATUS_INTERRUPTED {
		t.Errorf("Status = %v, want INTERRUPTED for a cancelled run", res.Status)
	}
}

// failingAfterNStore fails AppendEvent from the Nth call on, so the mid-run
// emit-failure surfacing can be driven: the run finishes, and the first
// hot-path write failure comes back with the result.
type failingAfterNStore struct {
	store.Store
	after int
	seen  int
}

func (f *failingAfterNStore) AppendEvent(ctx context.Context, ev *knov1.Event) error {
	f.seen++
	if f.seen > f.after {
		return errValueStore
	}
	return f.Store.AppendEvent(ctx, ev)
}

// TestValueSurfacesMidRunEmitFailures: a hot-path event write failure is
// remembered, not returned — the run it interrupted stays resumable — and
// surfaces at close.
func TestValueSurfacesMidRunEmitFailures(t *testing.T) {
	t.Parallel()

	inner := openTestStore(t)
	createBaselineRun(t, inner, "base-1", []string{"m"})
	if err := inner.RecordOutcome(context.Background(), "base-1", &store.Outcome{
		CaseID: "c1", Score: &knov1.Score{CaseId: "c1", Value: 0.9, Passed: true},
	}); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	cases := &caseSource{list: []*Case{
		{Id: "c1", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV},
		{Id: "c2", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV},
	}}
	// Two events before the first AssetValued emit: RunStarted + AssetRouted.
	failing := &failingAfterNStore{Store: inner, after: 2}
	opts := ValueOptions{
		RunID: "run-1", BaselineRunID: "base-1",
		Agent: stubAgent{}, AgentRef: &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
		Goal:     fixedDirectionGoal{dir: knov1.Direction_DIRECTION_MAXIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY},
		GoalName: "g",
		Guard:    budget.New(budget.Limits{}, nil, 0),
		Store:    failing, Evals: Seal(cases),
		Concurrency: 2,
		Routing:     value.Options{Seed: 1},
	}
	res, err := opts.Value(context.Background(), stubPool{assets: []*Asset{{Id: "a1"}}})
	if err == nil {
		t.Fatal("a mid-run emit failure did not surface at close")
	}
	if res == nil {
		t.Fatal("the result was discarded with the emit failure; the run it " +
			"describes is resumable and must come back")
	}
}

// TestValueZeroRoutedFailurePaths: the free answer — an Asset routed to
// nothing — still surfaces its own failures: the emit, the passthrough
// Valuation read, and its write.
func TestValueZeroRoutedFailurePaths(t *testing.T) {
	t.Parallel()

	newOpts := func(t *testing.T, failing store.Store) (ValueOptions, *caseSource) {
		t.Helper()
		cases := &caseSource{list: []*Case{
			{Id: "c1", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV, Tags: []string{"billing"}},
			{Id: "c2", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV, Tags: []string{"billing"}},
		}}
		opts := ValueOptions{
			RunID: "run-1", BaselineRunID: "base-1",
			Agent: stubAgent{}, AgentRef: &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
			Goal:     fixedDirectionGoal{dir: knov1.Direction_DIRECTION_MAXIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY},
			GoalName: "g",
			Guard:    budget.New(budget.Limits{}, nil, 0),
			Store:    failing, Evals: Seal(cases),
			Concurrency: 2,
			Routing:     value.Options{Seed: 1},
		}
		return opts, cases
	}
	// Every Case passes the baseline, the Asset is tagged for another domain:
	// it routes to nothing.
	baseline := func(t *testing.T, inner store.Store, cases *caseSource) {
		t.Helper()
		createBaselineRun(t, inner, "base-1", []string{"m"})
		for _, c := range cases.list {
			if err := inner.RecordOutcome(context.Background(), "base-1", &store.Outcome{
				CaseID: c.GetId(),
				Score:  &knov1.Score{CaseId: c.GetId(), Value: 0.9, Passed: true},
			}); err != nil {
				t.Fatalf("RecordOutcome: %v", err)
			}
		}
	}
	pool := stubPool{assets: []*Asset{{Id: "a1", Tags: []string{"astronomy"}}}}

	t.Run("routed emit", func(t *testing.T) {
		t.Parallel()
		inner := openTestStore(t)
		cases := &caseSource{list: []*Case{
			{Id: "c1", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV, Tags: []string{"billing"}},
			{Id: "c2", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV, Tags: []string{"billing"}},
		}}
		baseline(t, inner, cases)
		failing := &failingAfterNStore{Store: inner, after: 1}
		opts, _ := newOpts(t, failing)
		if _, err := opts.Value(context.Background(), pool); err == nil {
			t.Error("the zero-routed emit failure did not surface")
		}
	})

	t.Run("passthrough read", func(t *testing.T) {
		t.Parallel()
		inner := openTestStore(t)
		cases := &caseSource{list: []*Case{
			{Id: "c1", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV, Tags: []string{"billing"}},
			{Id: "c2", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV, Tags: []string{"billing"}},
		}}
		baseline(t, inner, cases)
		failing := &failingValueStore{Store: inner, failMeasure: true}
		opts, _ := newOpts(t, failing)
		if _, err := opts.Value(context.Background(), pool); err == nil {
			t.Error("the zero-routed Valuation read failure did not surface")
		}
	})

	t.Run("passthrough write", func(t *testing.T) {
		t.Parallel()
		inner := openTestStore(t)
		cases := &caseSource{list: []*Case{
			{Id: "c1", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV, Tags: []string{"billing"}},
			{Id: "c2", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV, Tags: []string{"billing"}},
		}}
		baseline(t, inner, cases)
		failing := &failingValueStore{Store: inner, failValuation: true}
		opts, _ := newOpts(t, failing)
		if _, err := opts.Value(context.Background(), pool); err == nil {
			t.Error("the zero-routed Valuation write failure did not surface")
		}
	})
}

// TestValueResumeCompletedReadFailure: the resume path's CompletedMeasurements
// failure is surfaced, not guessed around.
func TestValueResumeCompletedReadFailure(t *testing.T) {
	t.Parallel()

	inner := openTestStore(t)
	createBaselineRun(t, inner, "base-1", []string{"m"})
	failing := &failingValueStore{Store: inner, failCompleted: true}
	opts := ValueOptions{
		RunID: "run-1", BaselineRunID: "base-1", Resume: true,
		Agent: stubAgent{}, AgentRef: &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
		Goal:     fixedDirectionGoal{dir: knov1.Direction_DIRECTION_MAXIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY},
		GoalName: "g",
		Guard:    budget.New(budget.Limits{}, nil, 0),
		Store:    failing, Evals: Seal(&caseSource{}),
		Routing: value.Options{Seed: 1},
	}
	if _, err := opts.Value(context.Background(), stubPool{assets: []*Asset{{Id: "a1"}}}); err == nil {
		t.Error("a resume over a CompletedMeasurements failure produced no error")
	}
}
