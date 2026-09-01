package interval

import (
	"math"
	"math/rand"
	"sort"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// MethodBootstrap names a percentile-bootstrap interval, recorded on
// Interval.method for the same reason every other method name is: the
// instrument is part of the claim.
const MethodBootstrap = "bootstrap"

// BootstrapPercentile returns a two-sided percentile-bootstrap interval for a
// statistic fn computes over n paired observations, resampling observation
// INDICES with replacement — the caller supplies fn rather than a slice of
// values because the statistic this package's first consumer needs (the
// redundancy-detection plan's co-improvement Jaccard) is not a mean of
// per-observation values; it is a set statistic over the resampled indices.
//
// A statistic package deliberately does not have one before this: the two
// interval families above (adjustedWald, paired/signBound) were chosen
// BECAUSE a bootstrap under-covers or degenerates at the sample sizes this
// package runs at (see adjustedWald's godoc). This is not a replacement for
// either — it exists for statistics neither family can express, and callers
// that can use Paired or HarmBound must keep using them.
//
// fn must be deterministic given idx and must return NaN or +/-Inf for a
// resample it cannot form a statistic from (e.g. an empty union) rather than
// panicking; such resamples are dropped rather than treated as zero, which
// would manufacture a value nothing measured.
//
// Returns nil — never a zero-width or non-finite interval — when: n < 2
// (nothing to resample), iterations < 1, the level is invalid, or more than
// half the resamples produced a non-finite statistic (the degenerate case is
// common enough here to be a real answer, not a bug: two Assets that never
// co-improve produce an empty union on most resamples). A caller with a
// degenerate statistic must treat "no interval" as the honest answer, exactly
// as every other constructor in this package requires.
func BootstrapPercentile(
	n, iterations int,
	level float64,
	rng *rand.Rand,
	fn func(idx []int) float64,
) *knov1.Interval {
	if n < 2 || iterations < 1 || !validLevel(level) || rng == nil || fn == nil {
		return nil
	}
	stats := make([]float64, 0, iterations)
	idx := make([]int, n)
	for i := 0; i < iterations; i++ {
		for j := range idx {
			idx[j] = rng.Intn(n)
		}
		s := fn(idx)
		if math.IsNaN(s) || math.IsInf(s, 0) {
			continue
		}
		stats = append(stats, s)
	}
	if len(stats) < iterations/2 {
		return nil
	}
	sort.Float64s(stats)

	tail := (1 - level) / 2
	lo := percentile(stats, tail)
	hi := percentile(stats, 1-tail)
	if math.IsNaN(lo) || math.IsNaN(hi) || math.IsInf(lo, 0) || math.IsInf(hi, 0) {
		return nil
	}
	if hi <= lo {
		// Every surviving resample landed on the same statistic — real at a
		// small n or a bounded statistic near its ceiling. build()'s rule
		// elsewhere in this package applies here too: nothing may return a
		// zero-width interval, so this widens by the smallest step that keeps
		// it representable rather than claim certainty.
		hi = math.Nextafter(lo, math.Inf(1))
		if hi <= lo {
			return nil
		}
	}

	nn := int32(n) //nolint:gosec // bounded by the eval set
	return &knov1.Interval{
		Low:       lo,
		High:      hi,
		Level:     level,
		Method:    MethodBootstrap,
		Sidedness: knov1.Sidedness_SIDEDNESS_TWO_SIDED,
		NPairs:    &nn,
	}
}

// percentile is the linear-interpolation percentile of an already-sorted
// slice, the same convention numpy's default uses — chosen because it is the
// one most readers checking a bound by hand will reach for.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	pos := p * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}
