// Package interval computes confidence intervals on paired differences.
//
// Prime directive 5: no reported delta without its interval. This package is
// where that promise is kept, so its failure modes matter more than its
// precision. Two of them are worse than a wide interval:
//
//   - A ZERO-WIDTH interval reads as certainty. knov1.Interval exists as a
//     message so that its ABSENCE cannot be mistaken for a tight one, and a
//     zero-width interval defeats that from the other side. Nothing here may
//     return one.
//   - A NaN bound renders as blank. An interval that renders as blank is worse
//     than one that says "we could not measure this", because a reader fills
//     the blank in themselves.
//
// The method is selected from a DECLARED property of the Goal, never from the
// data observed. Choosing an estimator by looking at the sample makes the
// confidence level hold only conditional on a branch that is itself a function
// of the data — and across many measurements some would land in each branch by
// luck, after which a consumer compares intervals with different coverage as
// though they were the same claim.
package interval

import (
	"math"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// Method names, recorded on every Interval because the method is part of the
// claim: a delta whose method changed between two runs did not become more
// precise, it was measured differently.
const (
	// MethodAdjustedWald is the score-based interval for paired binary data:
	// the Agresti-Min form with a unit adjustment.
	MethodAdjustedWald = "adjusted-wald"

	// MethodStudentT is the paired t-interval for continuous data.
	MethodStudentT = "t"

	// MethodSign is the distribution-free fallback when a sample has no
	// variance for a parametric method to work with.
	MethodSign = "sign"
)

// DefaultLevel is the confidence level when a caller does not choose one.
const DefaultLevel = 0.95

// Paired returns a two-sided interval on the mean of deltas.
//
// deltas is ONE VALUE PER CASE, already sign-corrected for the Goal's direction
// by the caller — this package has no opinion about which way is better, only
// about how wide the uncertainty is.
//
// With repeated trials, each Case's value is the MEAN of its trials, not one
// entry per trial. Handing over k x n values instead returns an interval about
// sqrt(k) too narrow with n_pairs k times too large, and nothing errors: the
// count of pairs is not recoverable from the slice. PairedTrials does the
// averaging so a caller cannot get this wrong.
//
// trials < 1 is refused rather than treated as 1, because Valuation.trials is
// a proto int32 whose unset value is zero — and a caller reading it off a
// not-yet-populated message would otherwise select the paired-binary method
// and apply a discordance-count interval to means of k draws.
//
// Returns nil when no interval can be computed, which is a real answer and the
// reason knov1.Interval is a message. A nil interval means delta must not be
// reported; it never means the delta is zero.
func Paired(deltas []float64, domain knov1.ScoreDomain, trials int, level float64) *knov1.Interval {
	if trials < 1 {
		return nil
	}
	return compute(deltas, domain, trials, level, knov1.Sidedness_SIDEDNESS_TWO_SIDED)
}

// PairedTrials is Paired for per-Case trial vectors, averaging each Case's
// trials into its single paired difference.
//
// The shape that makes the mistake Paired's godoc warns about unrepresentable:
// the caller hands over what it has — measurements grouped by Case — and the
// count of pairs is len(perCase) by construction rather than by convention.
func PairedTrials(perCase [][]float64, domain knov1.ScoreDomain, level float64) *knov1.Interval {
	deltas, trials, ok := meanPerCase(perCase)
	if !ok {
		return nil
	}
	return Paired(deltas, domain, trials, level)
}

// meanPerCase collapses trial vectors to one value per Case.
//
// Refuses a ragged input rather than averaging over different denominators: a
// Case measured twice and one measured five times contribute differently to
// the mean, and silently weighting them equally is a bias nothing downstream
// could see.
func meanPerCase(perCase [][]float64) (deltas []float64, trials int, ok bool) {
	if len(perCase) == 0 {
		return nil, 0, false
	}
	trials = len(perCase[0])
	if trials == 0 {
		return nil, 0, false
	}
	deltas = make([]float64, len(perCase))
	for i, tr := range perCase {
		if len(tr) != trials {
			return nil, 0, false
		}
		var sum float64
		for _, v := range tr {
			sum += v
		}
		deltas[i] = sum / float64(trials)
	}
	return deltas, trials, true
}

// HarmBound returns a one-sided LOWER bound on the mean of deltas.
//
// The question a control arm asks is "did this Asset break something it should
// not have touched", and that is one-sided. A two-sided interval answers a
// different question — "is the effect distinguishable from zero" — and at a
// small control sample it spans zero for a real regression, which renders
// under the report's coloring rule as "no regression". An underpowered harm
// test that looks identical to a passed one is worse than no test.
//
// LOWER, not upper, and the direction is the whole point. Paired's deltas are
// already sign-corrected so that positive is better, which makes "the true
// effect is at MOST x" a bound on how much the Asset HELPS — the opposite of
// the question. Harm is delta >= -epsilon, so the bound that answers it is the
// one below.
//
// The first version of this function returned an upper bound, and the
// consequence was measurable: the only rule a consumer could write against it
// was "high < 0 means harm", which at a control arm of ten pairs fired on a
// true -0.30 regression just 43% of the time while firing on the null 6.4% of
// the time. That is verbatim the failure this function exists to prevent — an
// underpowered harm test that looks identical to a passed one.
//
// Only `low` is meaningful on the result, and `high` is written as zero rather
// than an infinity: protojson serializes infinities as the strings "Infinity"
// and "-Infinity", which the generated OpenAPI declares as a number.
func HarmBound(deltas []float64, domain knov1.ScoreDomain, trials int, level float64) *knov1.Interval {
	if trials < 1 {
		return nil
	}
	return compute(deltas, domain, trials, level, knov1.Sidedness_SIDEDNESS_LOWER)
}

// compute dispatches on the DECLARED domain and never on the observed data.
func compute(
	deltas []float64,
	domain knov1.ScoreDomain,
	trials int,
	level float64,
	side knov1.Sidedness,
) *knov1.Interval {
	n := len(deltas)
	if n < 2 || !validLevel(level) {
		// Fewer than two pairs supports no interval at any level. Refusing is
		// the honest answer and the schema can express it.
		return nil
	}
	for _, d := range deltas {
		if math.IsNaN(d) || math.IsInf(d, 0) {
			// A caller handing us a non-finite difference has a bug upstream.
			// Computing over it would launder that bug into an interval.
			return nil
		}
	}

	// Binary data with one trial per Case is the McNemar setting: the
	// information is in the DISCORDANT pairs, and a method that ignores that
	// structure is both wider than it needs to be and — for a bootstrap —
	// degenerate when every pair agrees. With repeated trials the difference
	// takes trials+1 values and is no longer a discordance count at all, so it
	// takes the continuous path.
	if domain == knov1.ScoreDomain_SCORE_DOMAIN_BINARY && trials <= 1 {
		return adjustedWald(deltas, level, side)
	}
	return paired(deltas, level, side)
}

// adjustedWald is the score-based interval on a paired difference of
// proportions: the Agresti-Min form, with the adjustment chosen by simulation
// rather than inherited.
//
// The family was chosen over the alternatives at the sample sizes this stage
// runs at. MOVER-Wilson under-covered in every cell measured (0.907 to 0.932
// against a nominal 0.95), and a percentile bootstrap covered on average while
// returning a ZERO-WIDTH interval in 13.6% of runs at p=0.95, n=20 — the two
// failures this package exists to avoid.
//
// The ADJUSTMENT is 1.0 per discordant count against a denominator of n+2, not
// the published 0.5. Measured across the grid, the 0.5 form under-covers where
// the variance is highest — 0.938 at n=20, p=0.50 — and this package's own
// comments and CHANGELOG had claimed it never fell below nominal. A unit
// adjustment is conservative everywhere measured (minimum 0.955), which is the
// right side to err on: over-covering is a wide interval, under-covering is a
// claim of confidence the data does not support.
//
// The adjustment is also what keeps this non-degenerate. With b = c = 0 —
// every pair agreeing, which is exactly what an inert Asset looks like — the
// unadjusted variance is zero and the interval collapses to a point.
func adjustedWald(deltas []float64, level float64, side knov1.Sidedness) *knov1.Interval {
	var b, c float64 // pairs that improved, pairs that regressed
	for _, d := range deltas {
		switch {
		case d > 0:
			b++
		case d < 0:
			c++
		}
	}
	n := float64(len(deltas))

	const adj = 1.0
	bb, cc, nn := b+adj, c+adj, n+2*adj
	center := (bb - cc) / nn
	variance := (bb + cc - (bb-cc)*(bb-cc)/nn) / (nn * nn)
	half := zFor(level, side) * math.Sqrt(math.Max(variance, 0))

	return build(center, half, level, side, MethodAdjustedWald, len(deltas))
}

// paired is the t-interval on the mean difference, with a distribution-free
// fallback when the sample has no variance.
//
// The fallback is the whole point. A t-interval with s = 0 returns [x̄, x̄], and
// zero variance is not exotic here — it is what an Asset that changed nothing
// looks like, which is the majority of a real pool. Refusing instead would mean
// reporting no delta for most of the pool, which is a check that gets disabled.
func paired(deltas []float64, level float64, side knov1.Sidedness) *knov1.Interval {
	n := float64(len(deltas))

	var sum float64
	for _, d := range deltas {
		sum += d
	}
	mean := sum / n

	var ss float64
	for _, d := range deltas {
		ss += (d - mean) * (d - mean)
	}
	variance := ss / (n - 1)

	if variance <= 0 {
		// Every pair identical. The sign test bounds how confident that can
		// make us: with n agreeing observations and no disagreement, the
		// interval is the one a distribution-free rule allows, which is never
		// a point.
		return signBound(deltas, mean, level, side)
	}

	q := studentTQuantile(quantileFor(level, side), n-1)
	half := q * math.Sqrt(variance/n)
	return build(mean, half, level, side, MethodStudentT, len(deltas))
}

// signBound handles a sample with no observed variance.
//
// The width comes from the sample SIZE rather than from its spread: seeing the
// same difference n times in a row is evidence, and how much is a function of
// n. Using the rule-of-three bound (3/n at 95%) means twenty identical
// observations produce an interval of half-width 0.15 rather than zero — wide,
// honest, and impossible to mistake for certainty.
func signBound(deltas []float64, mean, level float64, side knov1.Sidedness) *knov1.Interval {
	n := float64(len(deltas))
	half := -math.Log(1-level) / n
	if half <= 0 || math.IsInf(half, 0) {
		return nil
	}
	// Scaled by the observed magnitude so a run of identical +1s and a run of
	// identical +0.01s do not get the same absolute uncertainty.
	scale := math.Abs(mean)
	if scale == 0 {
		scale = 1
	}
	return build(mean, half*scale, level, side, MethodSign, len(deltas))
}

// build assembles the message, and is the single place a bound is written.
//
// Every return path goes through here so the two invariants — never
// zero-width, never non-finite — are enforced once rather than at each caller.
func build(
	center, half, level float64,
	side knov1.Sidedness,
	method string,
	n int,
) *knov1.Interval {
	if math.IsNaN(center) || math.IsNaN(half) || math.IsInf(center, 0) || math.IsInf(half, 0) {
		return nil
	}
	if half <= 0 {
		// Reached only by a method that believes it has zero uncertainty.
		// Nothing in this package should, and if something does the honest
		// answer is no interval rather than a claim of certainty.
		return nil
	}

	nn := int32(n) //nolint:gosec // bounded by the eval set
	iv := &knov1.Interval{
		Level:     level,
		Method:    method,
		Sidedness: side,
		NPairs:    &nn,
	}
	switch side {
	case knov1.Sidedness_SIDEDNESS_UPPER:
		// low stays 0: unbounded below, and an infinity would not survive JSON.
		iv.High = center + half
	case knov1.Sidedness_SIDEDNESS_LOWER:
		iv.Low = center - half
	default:
		iv.Low = center - half
		iv.High = center + half
	}
	return iv
}

// quantileFor returns the cumulative probability a bound needs.
//
// A one-sided bound at level L spends its whole error budget on one tail, so it
// uses the probability a two-sided interval would use at 2L-1 — which is why a
// one-sided 95% bound is tighter than either end of a two-sided 95% interval,
// and why Interval.level means different things for different sidedness.
func quantileFor(level float64, side knov1.Sidedness) float64 {
	tail := (1 - level) / 2
	if side != knov1.Sidedness_SIDEDNESS_TWO_SIDED {
		tail = 1 - level
	}
	return 1 - tail
}

// Quantile returns the quantile a bound at the given level and sidedness
// would use: the student-t quantile with df degrees of freedom when df >= 1,
// else the normal quantile.
//
// Exported for the multiplicity correction in stats/portfolio. Correcting an
// interval for multiple comparisons scales its recorded half-width by the
// ratio of the corrected quantile to the recorded one, and that ratio must
// come from the same distribution family the interval was built with — the
// caller cannot derive it from the recorded numbers alone.
func Quantile(level float64, side knov1.Sidedness, df int) float64 {
	p := quantileFor(level, side)
	if df >= 1 {
		return studentTQuantile(p, float64(df))
	}
	return normalQuantile(p)
}

// zFor returns the normal quantile for the level and sidedness.
//
// A one-sided bound at level L uses the same quantile a two-sided interval
// would use at 2L-1 — which is why a one-sided 95% bound is tighter than the
// upper end of a two-sided 95% interval, and why Interval.level means
// different things for different sidedness.
func zFor(level float64, side knov1.Sidedness) float64 {
	return normalQuantile(quantileFor(level, side))
}

func validLevel(level float64) bool {
	return level > 0.5 && level < 1 && !math.IsNaN(level)
}

// normalQuantile is the inverse standard normal CDF.
//
// Acklam's rational approximation, accurate to about 1.15e-9 across the range —
// far tighter than the width of any interval this package reports, and it
// avoids a dependency for one function. The tail refinement steps are omitted
// deliberately: they buy precision below the level anything here can use.
func normalQuantile(p float64) float64 {
	if p <= 0 || p >= 1 {
		return math.NaN()
	}

	a := []float64{
		-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02,
		1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00,
	}
	b := []float64{
		-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02,
		6.680131188771972e+01, -1.328068155288572e+01,
	}
	c := []float64{
		-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00,
		-2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00,
	}
	d := []float64{
		7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00,
		3.754408661907416e+00,
	}

	const plow, phigh = 0.02425, 1 - 0.02425
	switch {
	case p < plow:
		q := math.Sqrt(-2 * math.Log(p))
		return (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	case p > phigh:
		q := math.Sqrt(-2 * math.Log(1-p))
		return -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	default:
		q := p - 0.5
		r := q * q
		return (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q /
			(((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
	}
}

// MinDetectableEffect is the smallest effect m paired observations can
// separate from zero, at the given level and sidedness.
//
// The half-width of a bound over m paired observations, computed from the
// sample size alone: q(level, side, m-1) * sqrt(0.5) / sqrt(m). It is a BOUND
// rather than an estimate from data, which is what makes it printable before
// any measurement exists — `kno eval inspect` quotes it per behavior over an
// eval set nothing has been run against yet.
//
// sqrt(0.5) is the worst-case paired-binary standard deviation: differences
// live in {-1, 0, +1}, and the variance 2p(1-p) is maximised at 0.5 when the
// discordant pairs split evenly. Quoting the observed variance instead would
// shrink the number exactly on the runs where it mattered most. For a
// continuous Goal with lower variance the true detectable effect is smaller,
// so this over-warns — conservative in the recoverable direction.
//
// The quantile is Student-t at m-1 degrees of freedom, never z: t exceeds z at
// every finite df, so a z approximation returns a bound SMALLER than the truth
// — an optimistic figure, and the one thing a bound may not be.
//
// Sidedness is the caller's question, and the two answers differ. SIDEDNESS_UPPER
// answers a directional one ("did this get worse"), which is what core/value's
// harm bound asks. SIDEDNESS_TWO_SIDED answers a symmetric one ("is this
// distinguishable from noise"), which is what inspect asks, and it is the
// larger number at the same level. Reusing the one-sided figure for the
// symmetric question reports more power than exists.
//
// Returns 0 for m < 1: there is no sample, so nothing is detectable at all.
// The caller must not read that zero as a tight bound.
func MinDetectableEffect(m int, side knov1.Sidedness, level float64) float64 {
	if m < 1 {
		return 0
	}
	// sdMax is the STANDARD DEVIATION bound, sqrt(0.5) ~ 0.707, not the
	// variance bound 0.5.
	const sdMax = 0.7071067811865476 // math.Sqrt(0.5)
	return Quantile(level, side, m-1) * sdMax / math.Sqrt(float64(m))
}
