package interval_test

import (
	"math"
	"math/rand/v2"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/interval"
)

// meanOf is the statistic used by most of these tests: the mean over the
// resampled units.
func meanOf(xs []float64) func(idx []int) float64 {
	return func(idx []int) float64 {
		var sum float64
		for _, i := range idx {
			sum += xs[i]
		}
		return sum / float64(len(idx))
	}
}

// TestPercentileCoversTheTruthForAMean is the sanity floor: a method whose
// coverage is not measured is a method nobody may gate on.
func TestPercentileCoversTheTruthForAMean(t *testing.T) {
	t.Parallel()

	const (
		trials = 400
		n      = 60
		truth  = 0.5
	)
	rng := rand.New(rand.NewPCG(1, 2))

	covered := 0
	for trial := range trials {
		xs := make([]float64, n)
		for i := range xs {
			if rng.Float64() < truth {
				xs[i] = 1
			}
		}
		iv := interval.Percentile(n, meanOf(xs), interval.Bootstrap{
			Resamples: 400,
			Seed:      uint64(trial) + 1,
			Support:   &interval.Support{Low: 0, High: 1},
		})
		if iv == nil {
			continue
		}
		if iv.GetLow() <= truth && truth <= iv.GetHigh() {
			covered++
		}
	}
	got := float64(covered) / trials
	if got < 0.90 {
		t.Errorf("coverage %.3f at a nominal 0.95 for a mean at n=%d; "+
			"the percentile bootstrap is under-covering badly enough to flip a gate", got, n)
	}
}

// TestPercentileIsDeterministic pins the property a CI gate rests on: the same
// input returns the same interval, so a verdict cannot flip without a diff.
func TestPercentileIsDeterministic(t *testing.T) {
	t.Parallel()

	xs := []float64{0, 1, 1, 0, 1, 1, 1, 0, 1, 0, 1, 1}
	a := interval.Percentile(len(xs), meanOf(xs), interval.Bootstrap{})
	b := interval.Percentile(len(xs), meanOf(xs), interval.Bootstrap{})
	if a == nil || b == nil {
		t.Fatal("no interval")
	}
	if a.GetLow() != b.GetLow() || a.GetHigh() != b.GetHigh() {
		t.Errorf("two runs disagreed: [%.6f, %.6f] vs [%.6f, %.6f]",
			a.GetLow(), a.GetHigh(), b.GetLow(), b.GetHigh())
	}
	if a.GetMethod() != interval.MethodBootstrapPercentile {
		t.Errorf("method = %q, want %q", a.GetMethod(), interval.MethodBootstrapPercentile)
	}
	if a.GetNPairs() != int32(len(xs)) {
		t.Errorf("n_pairs = %d, want %d", a.GetNPairs(), len(xs))
	}
}

// TestPairedBootstrapIsNarrowerThanTwoIndependentOnes is the property that
// justifies the pairing, asserted rather than asserted-in-prose.
//
// Two runs over the SAME units that agree on most of them differ by very
// little, and a paired resample keeps that agreement inside every draw.
// Bootstrapping the two runs independently and differencing the intervals
// afterwards discards it, and the result is an interval wide enough to contain
// zero for a regression that is real — which is the ratchet failing open.
func TestPairedBootstrapIsNarrowerThanTwoIndependentOnes(t *testing.T) {
	t.Parallel()

	const n = 80
	rng := rand.New(rand.NewPCG(7, 11))
	before := make([]float64, n)
	after := make([]float64, n)
	for i := range before {
		if rng.Float64() < 0.7 {
			before[i] = 1
		}
		after[i] = before[i]
		// A small, real regression on a tenth of the units.
		if before[i] == 1 && rng.Float64() < 0.1 {
			after[i] = 0
		}
	}

	paired := interval.Percentile(n, func(idx []int) float64 {
		var a, b float64
		for _, i := range idx {
			a += after[i]
			b += before[i]
		}
		return (a - b) / float64(len(idx))
	}, interval.Bootstrap{})

	ivA := interval.Percentile(n, meanOf(after), interval.Bootstrap{})
	ivB := interval.Percentile(n, meanOf(before), interval.Bootstrap{Seed: 99})
	if paired == nil || ivA == nil || ivB == nil {
		t.Fatal("no interval")
	}

	pairedWidth := paired.GetHigh() - paired.GetLow()
	independentWidth := (ivA.GetHigh() - ivB.GetLow()) - (ivA.GetLow() - ivB.GetHigh())
	if pairedWidth >= independentWidth {
		t.Errorf("paired width %.4f is not narrower than the independent-difference width %.4f; "+
			"the pairing bought nothing and the ratchet would fail open", pairedWidth, independentWidth)
	}
	if paired.GetHigh() >= 0 {
		t.Errorf("the paired interval [%.4f, %.4f] contains zero for a real regression",
			paired.GetLow(), paired.GetHigh())
	}
}

// TestDegenerateResampleReportsWidthFromSizeNotZero covers the failure this
// package's comment names first: a zero-width interval reads as certainty.
func TestDegenerateResampleReportsWidthFromSizeNotZero(t *testing.T) {
	t.Parallel()

	xs := make([]float64, 40)
	for i := range xs {
		xs[i] = 1
	}
	iv := interval.Percentile(len(xs), meanOf(xs), interval.Bootstrap{
		Support: &interval.Support{Low: 0, High: 1},
	})
	if iv == nil {
		t.Fatal("no interval for a perfectly agreeing sample")
	}
	if iv.GetMethod() != interval.MethodBootstrapDegenerate {
		t.Errorf("method = %q, want %q — the reader must be told the width was not "+
			"measured from spread", iv.GetMethod(), interval.MethodBootstrapDegenerate)
	}
	if iv.GetHigh()-iv.GetLow() <= 0 {
		t.Error("zero-width interval: this reads as certainty")
	}
	if iv.GetHigh() > 1 {
		t.Errorf("high = %.4f leaves the declared support", iv.GetHigh())
	}
}

// TestPercentileRefusesWhatItCannotAnswer pins every refusal in one table. Each
// row is a case where returning an interval would be a claim the data does not
// support.
func TestPercentileRefusesWhatItCannotAnswer(t *testing.T) {
	t.Parallel()

	xs := []float64{0, 1, 1, 0, 1}
	tests := []struct {
		name string
		n    int
		stat func(idx []int) float64
		opts interval.Bootstrap
	}{
		{"one unit supports no interval", 1, meanOf(xs), interval.Bootstrap{}},
		{"no statistic", 5, nil, interval.Bootstrap{}},
		{"a level outside (0.5, 1)", 5, meanOf(xs), interval.Bootstrap{Level: 0.4}},
		{"too few resamples to resolve a tail", 5, meanOf(xs), interval.Bootstrap{Resamples: 50}},
		{
			"a statistic that is NaN on the whole sample", 5,
			func([]int) float64 { return math.NaN() },
			interval.Bootstrap{},
		},
		{
			"a statistic undefined on most resamples", 5,
			func(idx []int) float64 {
				if idx[0] == 0 {
					return math.NaN()
				}
				return 1
			},
			interval.Bootstrap{},
		},
		{
			"a support with no width", 5, meanOf(xs),
			interval.Bootstrap{Support: &interval.Support{Low: 1, High: 1}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if iv := interval.Percentile(tc.n, tc.stat, tc.opts); iv != nil {
				t.Errorf("got [%.4f, %.4f]; want no interval", iv.GetLow(), iv.GetHigh())
			}
		})
	}
}

// TestPercentileHonorsSidedness checks the one-sided bound writes only the
// bound it means, for the reason build() writes zero rather than an infinity.
func TestPercentileHonorsSidedness(t *testing.T) {
	t.Parallel()

	// A continuous sample rather than a binary one: the resample distribution
	// of a mean over twelve zeros and ones takes a dozen distinct values, and
	// two nearby quantiles of a coarse lattice land on the same rung for
	// reasons that have nothing to do with sidedness.
	rng := rand.New(rand.NewPCG(3, 5))
	xs := make([]float64, 80)
	for i := range xs {
		xs[i] = rng.NormFloat64() + 1
	}
	lower := interval.Percentile(len(xs), meanOf(xs), interval.Bootstrap{
		Sidedness: knov1.Sidedness_SIDEDNESS_LOWER,
	})
	if lower == nil {
		t.Fatal("no interval")
	}
	if lower.GetHigh() != 0 {
		t.Errorf("a lower bound wrote high = %v", lower.GetHigh())
	}
	if lower.GetLow() <= 0 {
		t.Errorf("a lower bound of %v on a clearly positive mean", lower.GetLow())
	}

	upper := interval.Percentile(len(xs), meanOf(xs), interval.Bootstrap{
		Sidedness: knov1.Sidedness_SIDEDNESS_UPPER,
	})
	if upper == nil {
		t.Fatal("no interval")
	}
	if upper.GetLow() != 0 {
		t.Errorf("an upper bound wrote low = %v", upper.GetLow())
	}

	// The one-sided bound spends its whole error budget on one tail, so it is
	// tighter than the matching end of the two-sided interval.
	two := interval.Percentile(len(xs), meanOf(xs), interval.Bootstrap{})
	if two == nil {
		t.Fatal("no interval")
	}
	if lower.GetLow() <= two.GetLow() {
		t.Errorf("one-sided lower %.4f is not tighter than two-sided %.4f",
			lower.GetLow(), two.GetLow())
	}
}
