package interval

import (
	"math"
	"math/rand/v2"
	"sort"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// Bootstrap method names. Both are recognized values of Interval.method, which
// has listed "bootstrap" in its godoc since the schema landed while this
// package shipped no bootstrap at all — the third promise in the schema the
// code did not keep, and the one this file closes.
const (
	// MethodBootstrapPercentile is the percentile bootstrap: resample the
	// UNITS, recompute the statistic, and read the interval off the empirical
	// quantiles of the resample distribution.
	MethodBootstrapPercentile = "bootstrap-percentile"

	// MethodBootstrapDegenerate is a percentile bootstrap whose resample
	// distribution had no spread — every resample returned the same value.
	//
	// It is a DIFFERENT method name rather than the same one with a narrow
	// interval, because the two answers are not the same claim. Perfect
	// agreement resampled a thousand times is still perfect agreement: the
	// quantiles collapse to a point, and a point is the one thing this package
	// may never report (see the package comment). The width comes from the
	// sample SIZE instead, on the rule-of-three argument signBound already
	// uses, and the method name is what tells a reader the width was not
	// measured from spread.
	MethodBootstrapDegenerate = "bootstrap-percentile-degenerate"
)

// DefaultResamples is how many bootstrap resamples a caller gets by default.
//
// Two thousand rather than the folklore thousand: the quantities read off the
// distribution are its 2.5th and 97.5th percentiles, and the Monte-Carlo noise
// on a tail quantile falls as 1/sqrt(B). At B = 1000 that noise is large
// enough to move a bound across a decision boundary between two runs of the
// same data, which for a gate is indistinguishable from the data having
// changed. Determinism (Bootstrap.Seed) removes run-to-run flapping; B is what
// removes the noise itself.
const DefaultResamples = 2000

// DefaultSeed is the resampling seed when a caller does not choose one.
//
// A bootstrap that reseeded from the clock would return a different interval
// for the same input on every run, which for a CI gate means a verdict that
// flips without a diff. Determinism is not a testing convenience here; it is
// what makes the number reviewable.
const DefaultSeed uint64 = 0x6b6e6f6a75646765 // "knojudge"

// Support bounds a statistic. A percentile interval is a range of RESAMPLED
// VALUES, so it cannot itself leave the statistic's range — but the degenerate
// fallback's width is arithmetic rather than a resampled value, and without a
// stated support it can report a bound the statistic cannot take (kappa above
// 1). Callers that know the range say so; the zero value means unbounded.
type Support struct{ Low, High float64 }

// Bootstrap configures a percentile bootstrap.
type Bootstrap struct {
	// Resamples is how many times the units are resampled. Zero means
	// DefaultResamples.
	Resamples int

	// Level is the confidence level. Zero means DefaultLevel.
	Level float64

	// Seed makes the resampling reproducible. Zero means DefaultSeed.
	Seed uint64

	// Sidedness selects a two-sided interval or a one-sided bound. The zero
	// value is SIDEDNESS_UNSPECIFIED, which is treated as two-sided so a
	// caller that does not care gets the interval it expects.
	Sidedness knov1.Sidedness

	// Support bounds the statistic, when it has known bounds.
	Support *Support
}

// Percentile returns a percentile-bootstrap interval on a statistic of n
// units.
//
// The RESAMPLE UNIT is what makes this valid, and it is the caller's to
// choose: stat receives n indices drawn with replacement from [0, n) and
// returns the statistic recomputed over exactly those units. For a judge's
// agreement with human labels the unit is the calibration RECORD, so a record
// that is drawn twice contributes twice to both the judge's marginal and the
// human's — which is the dependence between them that an interval computed
// from the confusion counts alone would throw away.
//
// PAIRING is expressed the same way. A paired difference between two runs over
// the same units is a statistic that computes BOTH runs on ONE index draw and
// subtracts, so the two runs move together across resamples. Two independent
// bootstraps differenced afterwards discard exactly that co-movement and
// produce an interval too wide to detect the regression it exists to detect.
// This package offers no separate paired entry point because there is nothing
// separate to offer: the pairing lives in the closure.
//
// A resample whose statistic is undefined (NaN, as Cohen's kappa is when every
// label falls in one class) is DISCARDED rather than treated as zero, and if
// more than half the resamples are undefined the answer is no interval. Both
// of those are refusals, not repairs: a statistic that is usually undefined has
// no distribution to read quantiles off.
//
// Returns nil when no interval can be computed. A nil interval means the
// statistic must not be reported with one; it never means the width is zero.
func Percentile(n int, stat func(idx []int) float64, opts Bootstrap) *knov1.Interval {
	if n < 2 || stat == nil {
		return nil
	}
	level, resamples, seed := opts.Level, opts.Resamples, opts.Seed
	if level == 0 {
		level = DefaultLevel
	}
	if !validLevel(level) {
		return nil
	}
	if resamples == 0 {
		resamples = DefaultResamples
	}
	if resamples < 100 {
		// Fewer than a hundred resamples cannot resolve a 2.5% tail at all:
		// the bound would be the minimum of the draws, which is a statement
		// about the random number generator rather than about the data.
		return nil
	}
	if seed == 0 {
		seed = DefaultSeed
	}
	side := opts.Sidedness
	if side == knov1.Sidedness_SIDEDNESS_UNSPECIFIED {
		side = knov1.Sidedness_SIDEDNESS_TWO_SIDED
	}

	identity := make([]int, n)
	for i := range identity {
		identity[i] = i
	}
	point := stat(identity)
	if math.IsNaN(point) || math.IsInf(point, 0) {
		return nil
	}

	// The second PCG stream word is derived rather than taken from the caller
	// so that a caller supplying only one number still gets a well-separated
	// pair. The constant is the golden-ratio mix used for exactly this.
	//nolint:gosec // G404: a bootstrap needs a REPRODUCIBLE stream, not an
	// unpredictable one. A cryptographic source here would return a different
	// interval for the same input on every run, which for a CI gate is a
	// verdict that flips without a diff. Nothing here is a secret.
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))

	draws := make([]float64, 0, resamples)
	idx := make([]int, n)
	for range resamples {
		for i := range idx {
			idx[i] = rng.IntN(n)
		}
		v := stat(idx)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		draws = append(draws, v)
	}
	if len(draws)*2 < resamples {
		return nil
	}
	sort.Float64s(draws)

	low, high := percentileBounds(draws, level, side)
	if high-low > 0 {
		return buildBounds(low, high, level, side, MethodBootstrapPercentile, n, opts.Support)
	}

	// Every resample agreed. The rule-of-three width signBound uses: seeing the
	// same answer n times is evidence, and how much is a function of n.
	half := -math.Log(1-level) / float64(n)
	if half <= 0 || math.IsInf(half, 0) {
		return nil
	}
	return buildBounds(point-half, point+half, level, side, MethodBootstrapDegenerate, n, opts.Support)
}

// percentileBounds reads the interval off the sorted resample distribution.
func percentileBounds(sorted []float64, level float64, side knov1.Sidedness) (low, high float64) {
	tail := (1 - level) / 2
	if side != knov1.Sidedness_SIDEDNESS_TWO_SIDED {
		tail = 1 - level
	}
	return quantileOf(sorted, tail), quantileOf(sorted, 1-tail)
}

// quantileOf is the (B+1)p order statistic with linear interpolation.
//
// The (B+1) convention rather than Bp: with B resamples the empirical
// distribution has B+1 gaps, and using Bp systematically pulls both bounds
// inward, which is under-coverage bought for nothing.
func quantileOf(sorted []float64, p float64) float64 {
	b := len(sorted)
	if b == 0 {
		return math.NaN()
	}
	pos := p * float64(b+1)
	switch {
	case pos <= 1:
		return sorted[0]
	case pos >= float64(b):
		return sorted[b-1]
	}
	lo := int(math.Floor(pos))
	frac := pos - float64(lo)
	// pos is in (1, b), so lo is in [1, b-1] and both indices are in range.
	return sorted[lo-1] + frac*(sorted[lo]-sorted[lo-1])
}

// buildBounds assembles a bootstrap Interval from explicit bounds.
//
// build() takes a center and a half-width because every other method in this
// package is symmetric about a point estimate. A percentile interval is not:
// its asymmetry about the point is the whole reason to use it on a bounded,
// skewed statistic. Forcing it through build() would symmetrize it and throw
// away exactly what it was chosen for — so this is a second assembler, holding
// the same two invariants (never non-finite, never zero-width) in the same one
// place for its own return paths.
func buildBounds(
	low, high, level float64,
	side knov1.Sidedness,
	method string,
	n int,
	support *Support,
) *knov1.Interval {
	if support != nil {
		low = math.Max(low, support.Low)
		high = math.Min(high, support.High)
	}
	if math.IsNaN(low) || math.IsNaN(high) || math.IsInf(low, 0) || math.IsInf(high, 0) {
		return nil
	}
	if high <= low {
		// Reachable only when the support is degenerate. Nothing here may
		// report a point as if it were an interval.
		return nil
	}

	nn := int32(n) //nolint:gosec // bounded by the record count
	iv := &knov1.Interval{
		Level:     level,
		Method:    method,
		Sidedness: side,
		NPairs:    &nn,
	}
	switch side {
	case knov1.Sidedness_SIDEDNESS_UPPER:
		iv.High = high
	case knov1.Sidedness_SIDEDNESS_LOWER:
		iv.Low = low
	default:
		iv.Low, iv.High = low, high
	}
	return iv
}
