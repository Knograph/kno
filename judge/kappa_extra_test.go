package judge_test

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/judge"
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
