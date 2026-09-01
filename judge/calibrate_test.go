package judge_test

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/goal/exactmatch"
	"github.com/knograph/kno/judge"
	"github.com/knograph/kno/stats/budget"
)

func starterSet(t *testing.T) *judge.Set {
	t.Helper()
	set, err := judge.Builtin("starter")
	if err != nil {
		t.Fatalf("loading the built-in calibration set: %v", err)
	}
	return set
}

func calibrate(t *testing.T, opts judge.Options) *judge.Result {
	t.Helper()
	res, err := judge.Calibrate(t.Context(), opts)
	if err != nil {
		t.Fatalf("calibrating: %v", err)
	}
	return res
}

// TestConstantJudgeScoresZeroKappaDespiteHighRawAgreement is the single test
// that carries this feature's statistical claim.
//
// A judge that answers "pass" unconditionally is worthless, and raw agreement
// says it is right most of the time. Kappa says zero, exactly. If this test
// ever passes against a raw-agreement implementation, the gate is measuring
// the wrong thing.
func TestConstantJudgeScoresZeroKappaDespiteHighRawAgreement(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	res := calibrate(t, judge.Options{
		Goal: constantGoal{pass: true}, GoalName: "always-pass", Set: set,
	})

	if math.Abs(res.Agreement.Kappa) > 1e-9 {
		t.Errorf("kappa = %.12f, want 0 exactly for a constant judge", res.Agreement.Kappa)
	}
	if res.Agreement.Raw <= 0.5 {
		t.Errorf("raw agreement = %.4f; the set must be prevalent enough that the "+
			"constant judge LOOKS right, or this test proves nothing", res.Agreement.Raw)
	}
	if res.Verdict != judge.VerdictFail {
		t.Errorf("verdict = %s, want FAIL", res.Verdict)
	}
	if !res.Failed() {
		t.Error("a constant judge did not fail the gate")
	}
}

// TestKappaIsTheAttenuationFactor pins the identity the floor rests on.
//
// kappa = 1 - 2*epsilon on a balanced set with symmetric error, which is what
// makes kappa denominated in the units of the product: it IS the factor by
// which every measured delta is attenuated. A change to the statistic that
// breaks this interpretation fails here rather than silently changing what the
// threshold means.
func TestKappaIsTheAttenuationFactor(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	truth := truthOf(set)

	tests := []struct {
		name   string
		stride int
		eps    float64
	}{
		{"20% error", 5, 0.20},
		{"10% error", 10, 0.10},
		{"5% error", 20, 0.05},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			res := calibrate(t, judge.Options{
				Goal:     truthGoal{truth: truth, flip: everyNth(set, tc.stride)},
				GoalName: "noisy", Set: set,
			})
			want := 1 - 2*tc.eps
			if math.Abs(res.Agreement.Kappa-want) > 0.05 {
				t.Errorf("kappa = %.4f at epsilon = %.2f; the 1 - 2*epsilon identity says "+
					"%.4f (+/- 0.05). The floor's derivation no longer holds.",
					res.Agreement.Kappa, tc.eps, want)
			}
		})
	}
}

// TestAsymmetricJudgeFailsEvenAboveTheFloor covers the assumption the
// derivation cannot enforce.
//
// Under asymmetric error kappa stops being the attenuation factor, so a judge
// comfortably above 0.60 that is biased in one direction moves every delta
// that way while the headline number looks fine. The symmetry gate is what
// catches it, and it must fire ABOVE the floor or it catches nothing.
func TestAsymmetricJudgeFailsEvenAboveTheFloor(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	truth := truthOf(set)

	// Passes every record the humans passed, and 7 of the 28 they failed:
	// sensitivity 1.00 against specificity 0.75.
	flip := map[string]bool{}
	failed := 0
	for _, r := range set.Records {
		if !r.Adjudicated.Passed && failed < 7 {
			flip[r.ID] = true
			failed++
		}
	}

	res := calibrate(t, judge.Options{
		Goal: truthGoal{truth: truth, flip: flip}, GoalName: "lenient", Set: set,
	})

	if res.Agreement.Kappa <= judge.DefaultMinKappa {
		t.Fatalf("kappa = %.3f is not above the floor; this test only means something "+
			"for a judge the kappa gate would pass", res.Agreement.Kappa)
	}
	if res.Verdict != judge.VerdictFail {
		t.Errorf("verdict = %s, want FAIL for a judge with a %.2f symmetry gap",
			res.Verdict, res.Agreement.SymmetryGap())
	}
	if !strings.Contains(res.Cause, "asymmetric error") {
		t.Errorf("the cause does not name asymmetric error:\n%s", res.Cause)
	}
}

// TestStraddlingIntervalIsIndeterminate: "we cannot tell" is not "it is fine".
func TestStraddlingIntervalIsIndeterminate(t *testing.T) {
	t.Parallel()

	set, err := judge.Builtin("straddle")
	if err != nil {
		t.Fatal(err)
	}
	res := calibrate(t, judge.Options{
		Goal: &exactmatch.Goal{}, GoalName: "exact-match", Set: set,
	})

	if res.Agreement.Kappa <= judge.DefaultMinKappa {
		t.Fatalf("the straddle set's point estimate (%.3f) is below the floor; the "+
			"scenario this set exists for is a POINT above the floor with an interval "+
			"that spans it", res.Agreement.Kappa)
	}
	if res.Verdict != judge.VerdictIndeterminate {
		t.Errorf("verdict = %s, want INDETERMINATE for CI [%.3f, %.3f] against a %.2f floor",
			res.Verdict, res.KappaInterval.GetLow(), res.KappaInterval.GetHigh(), res.MinKappa)
	}
	if !res.Failed() {
		t.Error("INDETERMINATE did not fail the gate")
	}
	if !strings.Contains(res.Fix, "Add records") {
		t.Errorf("the fix does not say to add records:\n%s", res.Fix)
	}
}

// TestLabelsThatDoNotAgreeBlameTheLabels, not the judge. A contributor sent to
// edit a prompt because the labelers disagreed is a contributor sent to the
// wrong file.
func TestLabelsThatDoNotAgreeBlameTheLabels(t *testing.T) {
	t.Parallel()

	set, err := judge.Load(filepath.Join("testdata", "bad", "low-inter-human"))
	if err != nil {
		t.Fatalf("this set must LOAD: the failure is about the labels, not the file: %v", err)
	}
	res := calibrate(t, judge.Options{
		Goal: truthGoal{truth: truthOf(set)}, GoalName: "perfect", Set: set,
	})

	if res.Verdict != judge.VerdictIndeterminate {
		t.Errorf("verdict = %s, want INDETERMINATE", res.Verdict)
	}
	if !strings.Contains(res.Cause, "labels do not agree") {
		t.Errorf("the cause blames something other than the labels:\n%s", res.Cause)
	}
	if !strings.Contains(res.Fix, "SET") {
		t.Errorf("the fix does not send the reader to the set:\n%s", res.Fix)
	}
}

// TestTooManyJudgeErrorsIsNotAUsableCalibration reuses the baseline gate's
// threshold and its words.
func TestTooManyJudgeErrorsIsNotAUsableCalibration(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	res := calibrate(t, judge.Options{
		Goal:     erroringGoal{truth: truthOf(set), fail: everyNth(set, 5)},
		GoalName: "flaky", Set: set,
	})

	if res.NErrored == 0 {
		t.Fatal("no records errored; the scenario did not happen")
	}
	if !strings.Contains(res.Cause, "not a usable calibration") {
		t.Errorf("the cause does not use the baseline gate's words:\n%s", res.Cause)
	}
	if res.Verdict != judge.VerdictIndeterminate {
		t.Errorf("verdict = %s, want INDETERMINATE", res.Verdict)
	}
}

// TestErroredRecordsAreExcludedNotCountedWrong: a handful of judge errors is
// data, not a broken run — the records are dropped from kappa and counted.
func TestErroredRecordsAreExcludedNotCountedWrong(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	fail := map[string]bool{set.Records[0].ID: true}
	res := calibrate(t, judge.Options{
		Goal: erroringGoal{truth: truthOf(set), fail: fail}, GoalName: "flaky", Set: set,
	})

	if res.NErrored != 1 || res.NScored != len(set.Records)-1 {
		t.Errorf("scored %d, errored %d over %d records", res.NScored, res.NErrored, res.NRecords)
	}
	if res.Agreement.N != len(set.Records)-1 {
		t.Errorf("kappa was computed over %d records, want %d — an errored record must not "+
			"count as a disagreement", res.Agreement.N, len(set.Records)-1)
	}
	if res.Verdict != judge.VerdictPass {
		t.Errorf("verdict = %s; one error in sixty is under the threshold", res.Verdict)
	}
}

// TestAValueOutsideTheDeclaredDomainIsAnError: reading a 0.5 as a pass on a
// binary Goal would launder a broken judge into a number.
func TestAValueOutsideTheDeclaredDomainIsAnError(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	res := calibrate(t, judge.Options{
		Goal: outOfDomainGoal{}, GoalName: "broken", Set: set,
	})
	if res.NErrored != len(set.Records) {
		t.Errorf("errored %d of %d; a value the declared Domain cannot take is not a verdict",
			res.NErrored, len(set.Records))
	}
	if res.Verdict != judge.VerdictIndeterminate {
		t.Errorf("verdict = %s, want INDETERMINATE", res.Verdict)
	}
}

// TestGradedGoalIsReportedAndNotGated: kappa is undefined on continuous
// scores, and inventing an anchored scale here would be exactly the invented
// threshold this command exists to avoid.
func TestGradedGoalIsReportedAndNotGated(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	res := calibrate(t, judge.Options{
		Goal: gradedGoal{truth: truthOf(set)}, GoalName: "graded", Set: set,
	})

	if res.Graded == nil {
		t.Fatal("no graded report for a UNIT_INTERVAL goal")
	}
	if res.Verdict != judge.VerdictNotApplicable {
		t.Errorf("verdict = %s, want NOT_APPLICABLE", res.Verdict)
	}
	if res.Failed() {
		t.Error("a graded goal failed a gate it is not subject to")
	}
	if math.IsNaN(res.Graded.Spearman) || math.IsNaN(res.Graded.WeightedKappa) {
		t.Errorf("graded statistics are undefined: %+v", res.Graded)
	}
}

// TestDisagreementsCarryWhatAPromptEditNeeds. A record id alone sends a
// contributor to grep; the rubric and the judge's own rationale are what make
// the edit directed.
func TestDisagreementsCarryWhatAPromptEditNeeds(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	res := calibrate(t, judge.Options{
		Goal: &exactmatch.Goal{}, GoalName: "exact-match", Set: set,
	})
	if len(res.Disagreements) == 0 {
		t.Fatal("exact-match agrees with every human label; the set has no near-misses")
	}
	d := res.Disagreements[0]
	if d.RecordID == "" || d.Rubric == "" || d.Rationale == "" {
		t.Errorf("a disagreement is missing the context an edit needs: %+v", d)
	}
}

// TestBudgetStoppedReportsTheCountAndNoStatistic.
//
// A kappa over the records that happened to fit under a cap is a kappa over a
// population nobody chose. The count is reported; the number is not.
func TestBudgetStoppedReportsTheCountAndNoStatistic(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	guard := budget.New(
		budget.Limits{MaxCostUSDMicros: 30_000},
		func(context.Context, budget.Estimate, budget.Remaining) (bool, error) { return true, nil },
		1_000_000,
	)
	res := calibrate(t, judge.Options{
		Goal:     &promptedGoal{prompt: "verdict?", truth: truthOf(set), cost: 10_000},
		GoalName: "costly", Set: set, Live: true, Guard: guard,
	})

	if !res.BudgetStopped {
		t.Fatalf("the run did not stop at its cap: scored %d of %d", res.NScored, res.NRecords)
	}
	if res.KappaInterval != nil || res.Agreement.N != 0 {
		t.Error("a partial run reported an agreement statistic")
	}
	if !strings.Contains(res.Cause, "budget") {
		t.Errorf("the cause does not name the cap:\n%s", res.Cause)
	}
	if res.NScored == 0 {
		t.Error("the record count was not reported")
	}
}

// TestLiveRunSettlesEveryJudgeCall pins the other half of prime directive 4:
// the guard must see the spend, not merely authorize it.
func TestLiveRunSettlesEveryJudgeCall(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	guard := budget.New(
		budget.Limits{},
		func(context.Context, budget.Estimate, budget.Remaining) (bool, error) { return true, nil },
		1_000_000,
	)
	res := calibrate(t, judge.Options{
		Goal:     &promptedGoal{prompt: "verdict?", truth: truthOf(set), cost: 1_000},
		GoalName: "costly", Set: set, Live: true, Guard: guard,
	})

	if got := guard.Spent().Calls; got != int64(len(set.Records)) {
		t.Errorf("the guard settled %d calls over %d records", got, len(set.Records))
	}
	if res.Spend.CostUSDMicros != int64(len(set.Records))*1_000 {
		t.Errorf("settled %d micro-USD, want %d",
			res.Spend.CostUSDMicros, len(set.Records)*1_000)
	}
	if !res.Guarded {
		t.Error("a live run did not report itself guarded")
	}
}
