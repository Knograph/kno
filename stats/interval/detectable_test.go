package interval_test

import (
	"math"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"

	"github.com/knograph/kno/stats/interval"
)

// preRefactorMinDetectableHarm is core/value.minDetectableHarm exactly as it
// stood before MinDetectableEffect existed, table and z fallback included.
//
// Copied rather than imported: it is deleted code, and a characterization
// test that imported the current implementation would assert the code matches
// itself. See TestMinDetectableEffectCharacterizesThePreRefactorBound for
// what this pins and where the two deliberately diverge.
func preRefactorMinDetectableHarm(m int) float64 {
	// t95OneSided as it was: the one-sided 95% Student-t critical value by
	// degrees of freedom, df 1..31, rounded to the table's own 3-4 digits.
	t95OneSided := [...]float64{
		6.314, 2.920, 2.353, 2.132, 2.015, 1.943, 1.895, 1.860, 1.833, 1.812,
		1.796, 1.782, 1.771, 1.761, 1.753, 1.746, 1.740, 1.734, 1.729, 1.725,
		1.721, 1.717, 1.714, 1.711, 1.708, 1.706, 1.703, 1.701, 1.699, 1.697,
		1.695,
	}
	if m < 1 {
		return 0
	}
	z := 1.645
	if df := m - 1; df >= 1 && df <= len(t95OneSided) {
		z = t95OneSided[df-1]
	}
	const sdMax = 0.7071067811865476
	return z * sdMax / math.Sqrt(float64(m))
}

// TestMinDetectableEffectCharacterizesThePreRefactorBound pins the refactor
// that moved this arithmetic out of core/value.
//
// A refactor that changes a reported statistical bound is a P0, so this is the
// test that makes an accidental change impossible. It cannot be exact
// equality, and the reason is worth stating rather than tolerating: the old
// implementation read a table rounded to 3-4 significant digits, so exact
// agreement with a computed t quantile is arithmetically unavailable. The
// tolerance below is the table's own precision, not a hedge.
//
// It also pins the ONE deliberate divergence. The old table stopped at df=31
// and fell back to z=1.645 beyond it, so at m=40 it reported 0.1839 where the
// true one-sided t bound is 0.1884 — a bound 2.4% NARROWER than the data
// supports, which is the wrong direction for a number a user acts on. The
// refactor computes t at every df and therefore widens it. Asserted, so that
// the correction is a recorded decision rather than a silent drift.
func TestMinDetectableEffectCharacterizesThePreRefactorBound(t *testing.T) {
	t.Parallel()

	// tableRounding is the largest relative disagreement the old table's
	// 3-4-digit values can produce against the exact quantile. Measured, not
	// guessed: the worst case in 1..32 is m=32 at 3.1e-4.
	const tableRounding = 1e-3

	// lastTabulated is the largest m the old table covered (df=31). Beyond it
	// the old code used z and the new code uses t.
	const lastTabulated = 32

	for m := 1; m <= 40; m++ {
		old := preRefactorMinDetectableHarm(m)
		got := interval.MinDetectableEffect(m, knov1.Sidedness_SIDEDNESS_UPPER, interval.DefaultLevel)

		if m <= lastTabulated {
			if rel := math.Abs(got-old) / old; rel > tableRounding {
				t.Errorf("m=%d: one-sided bound %.10f, pre-refactor %.10f (relative %.2e > %.0e): "+
					"the refactor changed a reported bound by more than the old table's rounding",
					m, got, old, rel, tableRounding)
			}
			continue
		}
		// Beyond the table: the correction, and its direction.
		if got <= old {
			t.Errorf("m=%d: one-sided bound %.10f is not wider than the pre-refactor z fallback %.10f; "+
				"the whole point of computing t past df=31 is that z was too narrow there",
				m, got, old)
		}
		if rel := (got - old) / old; rel > 0.05 {
			t.Errorf("m=%d: one-sided bound %.10f is %.1f%% wider than the pre-refactor %.10f; "+
				"the z fallback was off by ~3%%, not by this much — check the quantile",
				m, got, rel*100, old)
		}
	}
}

// TestMinDetectableEffectIsWiderTwoSidedThanOneSided is the property that
// makes reusing Plan.MinDetectableHarm for a symmetric question a power
// overstatement.
//
// A one-sided bound spends its whole error budget on one tail, so it is
// tighter at the same level. `kno eval inspect` asks "is this behavior
// distinguishable from noise", which is symmetric, and reporting the tighter
// figure would tell users their eval sets can separate effects they cannot.
// Asserted rather than assumed, because the two numbers appear in one output.
func TestMinDetectableEffectIsWiderTwoSidedThanOneSided(t *testing.T) {
	t.Parallel()

	for m := 2; m <= 500; m++ {
		one := interval.MinDetectableEffect(m, knov1.Sidedness_SIDEDNESS_UPPER, interval.DefaultLevel)
		two := interval.MinDetectableEffect(m, knov1.Sidedness_SIDEDNESS_TWO_SIDED, interval.DefaultLevel)
		if !(two > one) {
			t.Fatalf("m=%d: two-sided %.10f is not strictly greater than one-sided %.10f", m, two, one)
		}
	}
	// SIDEDNESS_LOWER is the other directional bound and must read as the
	// one-sided figure too: quantileFor treats anything but TWO_SIDED as
	// one-tailed, and a caller passing LOWER must not silently get the wider
	// number.
	for _, m := range []int{2, 5, 20, 100} {
		upper := interval.MinDetectableEffect(m, knov1.Sidedness_SIDEDNESS_UPPER, interval.DefaultLevel)
		lower := interval.MinDetectableEffect(m, knov1.Sidedness_SIDEDNESS_LOWER, interval.DefaultLevel)
		if upper != lower {
			t.Errorf("m=%d: upper %.10f and lower %.10f are both one-sided and must agree", m, upper, lower)
		}
	}
}

// TestMinDetectableEffectShrinksAsTheSampleGrows: a bound that does not
// improve with data is not a bound.
func TestMinDetectableEffectShrinksAsTheSampleGrows(t *testing.T) {
	t.Parallel()

	sides := []knov1.Sidedness{
		knov1.Sidedness_SIDEDNESS_UPPER,
		knov1.Sidedness_SIDEDNESS_TWO_SIDED,
	}
	for _, side := range sides {
		// m=2 is the first df the t quantile is defined at and is the maximum;
		// m=1 uses the normal quantile and is smaller than m=2, which is not a
		// monotonicity break but the df=0 fallback. Start the sweep at 2.
		prev := interval.MinDetectableEffect(2, side, interval.DefaultLevel)
		for m := 3; m <= 1000; m++ {
			got := interval.MinDetectableEffect(m, side, interval.DefaultLevel)
			if got >= prev {
				t.Fatalf("%v: m=%d gives %.10f, not less than m=%d's %.10f", side, m, got, m-1, prev)
			}
			prev = got
		}
	}
}

// TestMinDetectableEffectReportsNothingDetectableRatherThanASmallNumber: the
// no-sample and bad-level answers must not read as tight bounds.
func TestMinDetectableEffectReportsNothingDetectableRatherThanASmallNumber(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		n     int
		side  knov1.Sidedness
		level float64
	}{
		{name: "no observations", n: 0, side: knov1.Sidedness_SIDEDNESS_TWO_SIDED, level: 0.95},
		{name: "negative observations", n: -7, side: knov1.Sidedness_SIDEDNESS_TWO_SIDED, level: 0.95},
		{name: "level at the coin flip", n: 20, side: knov1.Sidedness_SIDEDNESS_TWO_SIDED, level: 0.5},
		{name: "level at certainty", n: 20, side: knov1.Sidedness_SIDEDNESS_TWO_SIDED, level: 1},
		{name: "level above certainty", n: 20, side: knov1.Sidedness_SIDEDNESS_TWO_SIDED, level: 1.5},
		{name: "level below zero", n: 20, side: knov1.Sidedness_SIDEDNESS_TWO_SIDED, level: -0.1},
		{name: "level not a number", n: 20, side: knov1.Sidedness_SIDEDNESS_TWO_SIDED, level: math.NaN()},
		{name: "sidedness unspecified reads one-sided", n: 20, side: knov1.Sidedness_SIDEDNESS_UNSPECIFIED, level: 0.95},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := interval.MinDetectableEffect(tt.n, tt.side, tt.level)
			if tt.name == "sidedness unspecified reads one-sided" {
				// Not a refusal: quantileFor's default branch is one-tailed,
				// and the caller gets the one-sided figure. Pinned so that
				// nobody "fixes" the zero value into the wider number and
				// changes two commands' output at once.
				want := interval.MinDetectableEffect(tt.n, knov1.Sidedness_SIDEDNESS_UPPER, tt.level)
				if got != want {
					t.Fatalf("unspecified sidedness gave %.10f, want the one-sided %.10f", got, want)
				}
				return
			}
			if got != 0 {
				t.Fatalf("MinDetectableEffect(%d, %v, %v) = %.10f, want 0 — "+
					"nothing is detectable, and a small number here reads as a tight bound",
					tt.n, tt.side, tt.level, got)
			}
		})
	}
}

// TestMinDetectableEffectPinsThePublishedTable pins the figures
// docs/evaluation-design.md and docs/what-the-numbers-mean.md print.
//
// Those pages turn "~10+ Cases per behavior" into arithmetic. If this
// function's answer moves, the published table is wrong, and a docs page that
// disagrees with the command is worse than no page.
func TestMinDetectableEffectPinsThePublishedTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		devCases int
		twoSided float64
		oneSided float64
	}{
		{devCases: 3, twoSided: 1.76, oneSided: 1.19},
		{devCases: 5, twoSided: 0.88, oneSided: 0.67},
		{devCases: 10, twoSided: 0.51, oneSided: 0.41},
		{devCases: 20, twoSided: 0.33, oneSided: 0.27},
		{devCases: 44, twoSided: 0.21, oneSided: 0.18},
		{devCases: 135, twoSided: 0.12, oneSided: 0.10},
		{devCases: 195, twoSided: 0.10, oneSided: 0.08},
	}
	for _, tt := range tests {
		got2 := interval.MinDetectableEffect(tt.devCases, knov1.Sidedness_SIDEDNESS_TWO_SIDED, interval.DefaultLevel)
		if math.Abs(got2-tt.twoSided) > 0.005 {
			t.Errorf("%d dev Cases: two-sided %.4f, the published table says %.2f",
				tt.devCases, got2, tt.twoSided)
		}
		got1 := interval.MinDetectableEffect(tt.devCases, knov1.Sidedness_SIDEDNESS_UPPER, interval.DefaultLevel)
		if math.Abs(got1-tt.oneSided) > 0.005 {
			t.Errorf("%d dev Cases: one-sided %.4f, the published table says %.2f",
				tt.devCases, got1, tt.oneSided)
		}
	}
}
