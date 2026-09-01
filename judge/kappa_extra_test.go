package judge_test

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/goal/exactmatch"
	"github.com/knograph/kno/judge"
	"github.com/knograph/kno/stats/interval"
)

// TestKappaIsUndefinedRatherThanZeroWhenChanceIsCertain.
//
// Every label in one class means chance agreement is 1, and a kappa of 0 there
// would read as "no better than chance" when the honest answer is "there is no
// answer". The balance invariant refuses such a set; this is the guard behind
// it.
func TestKappaIsUndefinedRatherThanZeroWhenChanceIsCertain(t *testing.T) {
	t.Parallel()

	all := []bool{true, true, true, true}
	if got := judge.Agree(all, all).Kappa; !math.IsNaN(got) {
		t.Errorf("kappa = %v over a single-class set, want undefined", got)
	}
	if got := judge.KappaOver(all, all, []int{0, 1, 2}); !math.IsNaN(got) {
		t.Errorf("KappaOver = %v over a single-class resample, want undefined", got)
	}
}

// TestAgreeRefusesMismatchedVectors: comparing a prefix would produce a number.
func TestAgreeRefusesMismatchedVectors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		judge, human []bool
	}{
		{"different lengths", []bool{true, false, true}, []bool{true, false}},
		{"empty", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := judge.Agree(tc.judge, tc.human).Kappa; !math.IsNaN(got) {
				t.Errorf("kappa = %v, want undefined", got)
			}
		})
	}
	if got := judge.KappaOver([]bool{true}, []bool{true}, []int{5}); !math.IsNaN(got) {
		t.Errorf("an out-of-range index produced %v", got)
	}
	if got := judge.KappaOver([]bool{true}, []bool{true}, nil); !math.IsNaN(got) {
		t.Errorf("an empty resample produced %v", got)
	}
}

// TestSpecificityIsUndefinedWithNoNegatives: 0/0 rendered as 0 would read as a
// judge that never gets a negative right.
func TestSpecificityIsUndefinedWithNoNegatives(t *testing.T) {
	t.Parallel()

	a := judge.Agree([]bool{true, true}, []bool{true, true})
	if !math.IsNaN(a.Specificity) {
		t.Errorf("specificity = %v with no human-fail records, want undefined", a.Specificity)
	}
	if !math.IsNaN(a.SymmetryGap()) {
		t.Errorf("symmetry gap = %v when one recall is undefined", a.SymmetryGap())
	}
}

// TestInterHumanNeedsTwoLabelers.
func TestInterHumanNeedsTwoLabelers(t *testing.T) {
	t.Parallel()

	one := []judge.Record{{
		ID:     "r1",
		Labels: []judge.HumanLabel{{LabelerID: "labeler-a", Passed: true}},
	}}
	if got := judge.InterHuman(one).Kappa; !math.IsNaN(got) {
		t.Errorf("inter-human kappa = %v with one labeler, want undefined", got)
	}
	if got := judge.InterHuman(nil).Kappa; !math.IsNaN(got) {
		t.Errorf("inter-human kappa = %v with no records, want undefined", got)
	}
}

// TestBaselineRoundTrips: what is written is what is read, so the diff a
// reviewer reads is the data the gate uses.
func TestBaselineRoundTrips(t *testing.T) {
	t.Parallel()

	want := []judge.BaselineEntry{{
		SetName:       "starter",
		SetVersion:    3,
		ContentSHA256: strings.Repeat("a", 64),
		GoalName:      "exact-match",
		PromptSHA:     judge.NoPromptSHA,
		JudgeModel:    "test-judge-1",
		Kappa:         0.8125,
		NRecords:      4,
		Verdicts:      "10-1",
	}}
	b, err := judge.EncodeBaseline(want)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "Review the diff like code") {
		t.Error("the encoded baseline does not carry its own review instruction")
	}

	path := filepath.Join(t.TempDir(), "calibration.baseline.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := judge.LoadBaseline(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 1 || got.Entries[0] != want[0] {
		t.Errorf("round trip drifted:\ngot  %+v\nwant %+v", got.Entries, want)
	}
	if _, ok := got.Find("starter", "exact-match"); !ok {
		t.Error("Find did not locate the entry it just read")
	}
	if _, ok := got.Find("starter", "rubric-judge"); ok {
		t.Error("Find located an entry that is not there")
	}
}

// TestAMissingBaselineIsRefused: a gate whose reference is absent has nothing
// to gate against, and passing quietly is how a ratchet stops ratcheting.
func TestAMissingBaselineIsRefused(t *testing.T) {
	t.Parallel()

	if _, err := judge.LoadBaseline(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("a missing baseline loaded")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := judge.LoadBaseline(bad); err == nil {
		t.Fatal("a malformed baseline loaded")
	}
}

// TestTheCommittedBaselineMatchesTheCommittedSet is the gate `make
// judge-calibrate-check` runs, asserted here too so a `go test ./...` catches
// it without the Makefile.
func TestTheCommittedBaselineMatchesTheCommittedSet(t *testing.T) {
	t.Parallel()

	b, err := judge.LoadBaseline(filepath.Join("..", "judge", "calibration.baseline.json"))
	if err != nil {
		t.Fatalf("the committed baseline does not load: %v", err)
	}
	if len(b.Entries) == 0 {
		t.Fatal("the committed baseline lists no entries")
	}
	for _, e := range b.Entries {
		set, err := judge.Builtin(e.SetName)
		if err != nil {
			t.Fatalf("baseline names set %q, which does not exist: %v", e.SetName, err)
		}
		if e.ContentSHA256 != set.ContentSHA256 {
			t.Errorf("the baseline for %s/%s attests to %s; the set hashes to %s. "+
				"Re-record it in the commit that edited the set.",
				e.SetName, e.GoalName, e.ContentSHA256[:12], set.ContentSHA256[:12])
		}
		if len(e.Verdicts) != len(set.Records) {
			t.Errorf("the baseline's verdict vector covers %d records; %s holds %d",
				len(e.Verdicts), e.SetName, len(set.Records))
		}
	}
}

// TestEveryReportedStatisticIsRoundedAtTheSource is the regression test for a
// bug the goldens caught on linux and darwin could not.
//
// `kappa_interval.high` was recorded as 0.929508759876331 on arm64 and read
// 0.9295087598763309 on amd64 — one ULP apart, and no re-recording satisfies
// both. Go may fuse a multiply-add into an FMA, which arm64 has and amd64 does
// not, and the bootstrap's bounds additionally pass through interpolation and
// math.Log; the tail digits of these numbers are simply not ours.
//
// Rounding at the source fixes it, and this test is what keeps it fixed: it
// asserts the PROPERTY (every reported float is its own four-place rounding)
// rather than a set of literals, so a statistic added later without the
// treatment fails here rather than on whichever architecture CI happens to
// run.
func TestEveryReportedStatisticIsRoundedAtTheSource(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	res := calibrate(t, judge.Options{
		Goal: &exactmatch.Goal{}, GoalName: "exact-match", Set: set,
	})

	reported := map[string]float64{
		"kappa":               res.Agreement.Kappa,
		"raw":                 res.Agreement.Raw,
		"sensitivity":         res.Agreement.Sensitivity,
		"specificity":         res.Agreement.Specificity,
		"judge_positive_rate": res.Agreement.JudgePositiveRate,
		"human_positive_rate": res.Agreement.HumanPositiveRate,
		"symmetry_gap":        res.Agreement.SymmetryGap(),
		"inter_human_kappa":   res.InterHuman.Kappa,
		"kappa_interval.low":  res.KappaInterval.GetLow(),
		"kappa_interval.high": res.KappaInterval.GetHigh(),
	}
	for name, got := range reported {
		if math.IsNaN(got) {
			continue
		}
		if want := math.Round(got*1e4) / 1e4; got != want {
			t.Errorf("%s = %v carries more than four decimal places (%v). "+
				"An unrounded statistic here differs by architecture and cannot "+
				"be pinned by a golden.", name, got, want)
		}
	}
}

// TestIntervalBoundsAreRoundedWithoutCollapsing.
//
// Rounding a bound is only safe while it cannot turn an interval into a point:
// a zero-width interval reads as certainty, which stats/interval's package
// comment names as one of the two failures worse than a wide interval.
func TestIntervalBoundsAreRoundedWithoutCollapsing(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	res := calibrate(t, judge.Options{
		Goal: &exactmatch.Goal{}, GoalName: "exact-match", Set: set,
	})
	raw := interval.Percentile(res.Agreement.N, func(idx []int) float64 {
		return judge.KappaOver(res.Judge, set.Reference(), idx)
	}, interval.Bootstrap{Support: &interval.Support{Low: -1, High: 1}})
	if raw == nil {
		t.Fatal("no unrounded interval to compare against")
	}

	if res.KappaInterval.GetHigh() <= res.KappaInterval.GetLow() {
		t.Fatal("rounding collapsed the interval; a zero-width interval reads as certainty")
	}
	for _, tc := range []struct {
		name     string
		got, was float64
	}{
		{"low", res.KappaInterval.GetLow(), raw.GetLow()},
		{"high", res.KappaInterval.GetHigh(), raw.GetHigh()},
	} {
		if math.Abs(tc.got-tc.was) > 5e-5 {
			t.Errorf("%s moved from %.9f to %.9f, further than rounding to four "+
				"places can move it", tc.name, tc.was, tc.got)
		}
	}
}

// TestTheVerdictIsDecidedOnTheNumbersItPrints.
//
// The verdict compares the interval against the floor. If it compared the
// UNROUNDED bound while the report printed the rounded one, a run could print
// a bound at the floor and call itself INDETERMINATE, and nobody could
// reproduce the decision from the output.
func TestTheVerdictIsDecidedOnTheNumbersItPrints(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	res := calibrate(t, judge.Options{
		Goal: &exactmatch.Goal{}, GoalName: "exact-match", Set: set,
	})

	low, high := res.KappaInterval.GetLow(), res.KappaInterval.GetHigh()
	var want string
	switch {
	case low >= res.MinKappa:
		want = judge.VerdictPass
	case high < res.MinKappa:
		want = judge.VerdictFail
	default:
		want = judge.VerdictIndeterminate
	}
	if res.Verdict != want {
		t.Errorf("verdict %s, but the printed interval [%.4f, %.4f] against a %.2f "+
			"floor says %s", res.Verdict, low, high, res.MinKappa, want)
	}
}
