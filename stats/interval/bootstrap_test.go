package interval

import (
	"math"
	"math/rand"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// meanStat is a bootstrap statistic over a fixed value slice — the mean of
// the resampled values — used to check BootstrapPercentile against a value
// whose sampling distribution is well understood.
func meanStat(values []float64) func(idx []int) float64 {
	return func(idx []int) float64 {
		var sum float64
		for _, i := range idx {
			sum += values[i]
		}
		return sum / float64(len(idx))
	}
}

func TestBootstrapPercentileRefusesInvalidInputs(t *testing.T) {
	t.Parallel()

	values := []float64{1, 2, 3, 4, 5}
	rng := rand.New(rand.NewSource(1))
	fn := meanStat(values)

	for _, tc := range []struct {
		name       string
		n          int
		iterations int
		level      float64
		rng        *rand.Rand
		fn         func(idx []int) float64
	}{
		{"n below two", 1, 1000, 0.95, rng, fn},
		{"n zero", 0, 1000, 0.95, rng, fn},
		{"iterations below one", 5, 0, 0.95, rng, fn},
		{"level too low", 5, 1000, 0.5, rng, fn},
		{"level too high", 5, 1000, 1.0, rng, fn},
		{"level NaN", 5, 1000, math.NaN(), rng, fn},
		{"nil rng", 5, 1000, 0.95, nil, fn},
		{"nil fn", 5, 1000, 0.95, rng, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := BootstrapPercentile(tc.n, tc.iterations, tc.level, tc.rng, tc.fn)
			if got != nil {
				t.Fatalf("BootstrapPercentile(%d, %d, %v, ...) = %v, want nil",
					tc.n, tc.iterations, tc.level, got)
			}
		})
	}
}

// TestBootstrapPercentileCoversAKnownMean: over a large, symmetric sample the
// percentile bootstrap on the mean should center near the true mean and
// bracket it — a coarse sanity check that the machinery is not inverted or
// off by a tail.
func TestBootstrapPercentileCoversAKnownMean(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewSource(42))
	values := make([]float64, 200)
	var sum float64
	for i := range values {
		v := rng.NormFloat64()*2 + 10 // mean 10, sd 2
		values[i] = v
		sum += v
	}
	trueMean := sum / float64(len(values))

	iv := BootstrapPercentile(len(values), 4000, 0.95, rand.New(rand.NewSource(7)), meanStat(values))
	if iv == nil {
		t.Fatal("BootstrapPercentile returned nil for a well-formed sample")
	}
	if iv.GetLow() > trueMean || iv.GetHigh() < trueMean {
		t.Fatalf("interval [%v, %v] does not cover the sample mean %v", iv.GetLow(), iv.GetHigh(), trueMean)
	}
	if iv.GetMethod() != MethodBootstrap {
		t.Fatalf("method = %q, want %q", iv.GetMethod(), MethodBootstrap)
	}
	if iv.GetSidedness() != knov1.Sidedness_SIDEDNESS_TWO_SIDED {
		t.Fatalf("sidedness = %v, want TWO_SIDED", iv.GetSidedness())
	}
	if iv.GetHigh() <= iv.GetLow() {
		t.Fatalf("interval must never be zero-width: [%v, %v]", iv.GetLow(), iv.GetHigh())
	}
}

// TestBootstrapPercentileDropsNonFiniteResamples: a statistic that returns
// NaN for half its resamples (an "undefined" degenerate case, e.g. an empty
// union) must not poison the interval with NaN bounds — those resamples are
// dropped, and if too many are dropped the whole interval is refused.
func TestBootstrapPercentileDropsNonFiniteResamples(t *testing.T) {
	t.Parallel()

	values := []float64{1, 1, 1, 1, 1, 0, 0, 0, 0, 0}
	fn := func(idx []int) float64 {
		var sum float64
		for _, i := range idx {
			sum += values[i]
		}
		if sum == 0 {
			return math.NaN() // every resampled index landed on a zero
		}
		return sum / float64(len(idx))
	}
	iv := BootstrapPercentile(len(values), 2000, 0.95, rand.New(rand.NewSource(3)), fn)
	// Some resamples are all-zero (NaN'd out) but most are not, so an
	// interval should still form.
	if iv == nil {
		t.Fatal("expected an interval when most resamples are finite")
	}

	// Now every resample is degenerate.
	allNaN := func([]int) float64 { return math.NaN() }
	if got := BootstrapPercentile(len(values), 2000, 0.95, rand.New(rand.NewSource(3)), allNaN); got != nil {
		t.Fatalf("expected nil when every resample is non-finite, got %v", got)
	}
}

// TestBootstrapPercentileNeverZeroWidth: a constant statistic (every resample
// returns the same value) must still return a non-degenerate interval per
// this package's standing invariant — a zero-width interval reads as
// certainty, and nothing here may produce one.
func TestBootstrapPercentileNeverZeroWidth(t *testing.T) {
	t.Parallel()
	constant := func([]int) float64 { return 0.5 }
	iv := BootstrapPercentile(10, 500, 0.95, rand.New(rand.NewSource(9)), constant)
	if iv == nil {
		t.Fatal("expected an interval for a constant statistic")
	}
	if iv.GetHigh() <= iv.GetLow() {
		t.Fatalf("zero-width interval: [%v, %v]", iv.GetLow(), iv.GetHigh())
	}
}

func TestPercentileInterpolation(t *testing.T) {
	t.Parallel()
	sorted := []float64{1, 2, 3, 4, 5}
	for _, tc := range []struct {
		p    float64
		want float64
	}{
		{0, 1},
		{1, 5},
		{0.5, 3},
		{0.25, 2},
	} {
		if got := percentile(sorted, tc.p); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("percentile(%v, %v) = %v, want %v", sorted, tc.p, got, tc.want)
		}
	}
}
