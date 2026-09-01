package judge_test

import (
	"strings"
	"testing"

	"github.com/knograph/kno/judge"
	"github.com/knograph/kno/stats/interval"
)

func bootstrapOpts() interval.Bootstrap {
	return interval.Bootstrap{Support: &interval.Support{Low: -1, High: 1}}
}

// entryFor records a run as if `make record-calibration` had.
func entryFor(set *judge.Set, judged []bool) judge.BaselineEntry {
	errored := make([]bool, len(judged))
	return judge.BaselineEntry{
		SetName:       set.Name,
		SetVersion:    set.Version,
		ContentSHA256: set.ContentSHA256,
		GoalName:      "prompted",
		PromptSHA:     "sha-a",
		Kappa:         judge.Agree(judged, set.Reference()).Kappa,
		NRecords:      len(set.Records),
		Verdicts:      judge.VerdictVector(judged, errored),
	}
}

// verdictsWithErrors flips the truth for the first n records.
func verdictsWithErrors(set *judge.Set, wrong int) []bool {
	out := set.Reference()
	for i := range out {
		if i < wrong {
			out[i] = !out[i]
		}
	}
	return out
}

// TestARealRegressionFailsTheRatchet: 0.80 to 0.62 with a paired difference
// interval excluding zero is a drop the run's own noise does not explain.
func TestARealRegressionFailsTheRatchet(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	before := verdictsWithErrors(set, 3)
	after := verdictsWithErrors(set, 11)

	prev := entryFor(set, before)
	r := judge.CompareToBaseline(prev, set, after, make([]bool, len(set.Records)), bootstrapOpts())

	if !r.Comparable {
		t.Fatalf("not comparable: %s", r.NotComparable)
	}
	if r.Kappa >= r.BaselineKappa {
		t.Fatalf("the scenario did not regress: %.3f -> %.3f", r.BaselineKappa, r.Kappa)
	}
	if !r.Regressed {
		t.Errorf("a drop from %.3f to %.3f with paired CI [%.3f, %.3f] did not fail the ratchet",
			r.BaselineKappa, r.Kappa, r.Diff.GetLow(), r.Diff.GetHigh())
	}
}

// TestADropInsideTheNoiseDoesNotFireTheRatchet is the direction that matters
// more.
//
// A gate that fires on noise gets disabled, and a disabled gate protects
// nothing. Gating on the point estimate alone would let a two-record flip in a
// sixty-record set fail a build.
func TestADropInsideTheNoiseDoesNotFireTheRatchet(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	before := verdictsWithErrors(set, 3)
	after := verdictsWithErrors(set, 4)

	prev := entryFor(set, before)
	r := judge.CompareToBaseline(prev, set, after, make([]bool, len(set.Records)), bootstrapOpts())

	if !r.Comparable {
		t.Fatalf("not comparable: %s", r.NotComparable)
	}
	if r.Kappa >= r.BaselineKappa {
		t.Fatalf("the scenario did not drop at all: %.3f -> %.3f", r.BaselineKappa, r.Kappa)
	}
	if r.Regressed {
		t.Errorf("a one-record drop (%.3f -> %.3f, paired CI [%.3f, %.3f]) fired the ratchet",
			r.BaselineKappa, r.Kappa, r.Diff.GetLow(), r.Diff.GetHigh())
	}
}

// TestAnImprovementPasses: the ratchet is one-sided. It exists to stop a
// silent drop, not to freeze a number.
func TestAnImprovementPasses(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	prev := entryFor(set, verdictsWithErrors(set, 11))
	r := judge.CompareToBaseline(prev, set, verdictsWithErrors(set, 2),
		make([]bool, len(set.Records)), bootstrapOpts())

	if r.Regressed {
		t.Errorf("an improvement from %.3f to %.3f failed the ratchet", r.BaselineKappa, r.Kappa)
	}
}

// TestBothOperandsMovingIsNotComparable. A set edit and a prompt edit in one
// PR leave nothing to compare, and reporting a difference anyway would name
// the wrong cause.
func TestBothOperandsMovingIsNotComparable(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	judged := set.Reference()

	tests := []struct {
		name string
		fix  func(e *judge.BaselineEntry)
		want string
	}{
		{"the set version moved", func(e *judge.BaselineEntry) { e.SetVersion = 99 }, "the set moved"},
		{"the set contents moved", func(e *judge.BaselineEntry) { e.ContentSHA256 = "deadbeef" }, "contents moved"},
		{"the record count moved", func(e *judge.BaselineEntry) { e.Verdicts = "101" }, "verdict vector"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			prev := entryFor(set, judged)
			tc.fix(&prev)
			r := judge.CompareToBaseline(prev, set, judged,
				make([]bool, len(set.Records)), bootstrapOpts())
			if r.Comparable {
				t.Fatal("two different measurements compared as one")
			}
			if !strings.Contains(r.NotComparable, tc.want) {
				t.Errorf("the reason does not name %q: %s", tc.want, r.NotComparable)
			}
		})
	}
}

// TestRecordsOnlyOneRunJudgedAreNotPaired. Imputing a verdict for a record one
// run errored on would invent agreement.
func TestRecordsOnlyOneRunJudgedAreNotPaired(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	judged := set.Reference()
	prev := entryFor(set, judged)
	prev.Verdicts = "-" + prev.Verdicts[1:]

	errored := make([]bool, len(set.Records))
	errored[1] = true

	r := judge.CompareToBaseline(prev, set, judged, errored, bootstrapOpts())
	if !r.Comparable {
		t.Fatalf("not comparable: %s", r.NotComparable)
	}
	if got := r.Diff.GetNPairs(); got != int32(len(set.Records)-2) {
		t.Errorf("paired over %d records, want %d", got, len(set.Records)-2)
	}
}
