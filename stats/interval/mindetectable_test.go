package interval_test

import (
	"math"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/interval"
)

// mdeLevel is the level every assertion here quotes, matching the one
// core/value's harm bound and `kno eval inspect` both use.
const mdeLevel = 0.95

// TestMinDetectableEffectIsTheTQuantileFormula pins the arithmetic against
// published Student-t constants rather than against the function's own output.
//
// A test that recomputed the formula would pass over a z approximation, which
// is the exact regression this bound has already suffered once: t exceeds z at
// every finite df, so a z fallback narrows the reported bound on precisely the
// small samples where the difference decides a safety gate.
func TestMinDetectableEffectIsTheTQuantileFormula(t *testing.T) {
	t.Parallel()

	// Published one-sided 95% t quantiles, three decimals, at df = m-1.
	oneSided := map[int]float64{
		5:  2.132, // df 4
		10: 1.833, // df 9
		20: 1.729, // df 19
	}
	// Published two-sided 95% t quantiles, three decimals, at df = m-1.
	twoSided := map[int]float64{
		5:  2.776, // df 4
		10: 2.262, // df 9
		20: 2.093, // df 19
	}
	const sdMax = 0.7071067811865476

	for m, q := range oneSided {
		want := q * sdMax / math.Sqrt(float64(m))
		got := interval.MinDetectableEffect(m, knov1.Sidedness_SIDEDNESS_UPPER, mdeLevel)
		// The published constants carry three decimals, so the comparison
		// cannot be tighter than they are.
		if math.Abs(got-want) > 5e-4 {
			t.Errorf("one-sided m=%d: got %v, published arithmetic says %v", m, got, want)
		}
	}
	for m, q := range twoSided {
		want := q * sdMax / math.Sqrt(float64(m))
		got := interval.MinDetectableEffect(m, knov1.Sidedness_SIDEDNESS_TWO_SIDED, mdeLevel)
		if math.Abs(got-want) > 5e-4 {
			t.Errorf("two-sided m=%d: got %v, published arithmetic says %v", m, got, want)
		}
	}
}

// TestTwoSidedIsStrictlyWiderThanOneSided is the property that makes reusing
// the one-sided figure for a symmetric question a power OVERSTATEMENT.
//
// `kno eval inspect` asks "is this behavior distinguishable from noise", which
// is symmetric; Plan.MinDetectableHarm asks "did this get worse", which is
// directional. Quoting the directional number against the symmetric question
// would report every eval set as more powerful than it is.
func TestTwoSidedIsStrictlyWiderThanOneSided(t *testing.T) {
	t.Parallel()

	for m := 2; m <= 400; m++ {
		one := interval.MinDetectableEffect(m, knov1.Sidedness_SIDEDNESS_UPPER, mdeLevel)
		two := interval.MinDetectableEffect(m, knov1.Sidedness_SIDEDNESS_TWO_SIDED, mdeLevel)
		if !(two > one) {
			t.Fatalf("m=%d: two-sided %v is not strictly larger than one-sided %v", m, two, one)
		}
	}
}

// TestMinDetectableEffectFallsWithSampleSize: more Cases separate smaller
// effects, in both sidednesses. A bound that did not would make the whole
// "add Cases to this behavior" suggestion incoherent.
func TestMinDetectableEffectFallsWithSampleSize(t *testing.T) {
	t.Parallel()

	for _, side := range []knov1.Sidedness{
		knov1.Sidedness_SIDEDNESS_UPPER,
		knov1.Sidedness_SIDEDNESS_TWO_SIDED,
	} {
		prev := math.Inf(1)
		for m := 2; m <= 400; m++ {
			got := interval.MinDetectableEffect(m, side, mdeLevel)
			if !(got < prev) {
				t.Fatalf("%v m=%d: %v did not fall below %v", side, m, got, prev)
			}
			prev = got
		}
	}
}

// TestMinDetectableEffectHasNoSampleAnswer: below one observation the answer is
// "nothing is detectable", and the caller must not read the returned zero as a
// tight bound. Pinned so a later "return something small" cannot slip in.
func TestMinDetectableEffectHasNoSampleAnswer(t *testing.T) {
	t.Parallel()

	for _, m := range []int{-1, 0} {
		for _, side := range []knov1.Sidedness{
			knov1.Sidedness_SIDEDNESS_UPPER,
			knov1.Sidedness_SIDEDNESS_TWO_SIDED,
		} {
			if got := interval.MinDetectableEffect(m, side, mdeLevel); got != 0 {
				t.Errorf("m=%d %v: got %v, want 0", m, side, got)
			}
		}
	}
}

// TestMinDetectableEffectAtOneObservationIsNotABound: a single pair has zero
// degrees of freedom, so the quantile falls back to the normal and the answer
// is finite but enormous. Asserted rather than assumed, because a number
// smaller than 1 here would read as a detectable effect on a binary Goal.
func TestMinDetectableEffectAtOneObservationIsNotABound(t *testing.T) {
	t.Parallel()

	got := interval.MinDetectableEffect(1, knov1.Sidedness_SIDEDNESS_TWO_SIDED, mdeLevel)
	if got <= 1 {
		t.Errorf("one observation reports a detectable effect of %v; on a binary Goal "+
			"anything at or below 1 reads as measurable", got)
	}
}
