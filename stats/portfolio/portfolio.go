// Package portfolio holds the statistics behind the Select stage: the
// net-loss judgement with shared-draw covariance, and the multiplicity
// correction that every keep/reject decision is made under.
//
// Prime directive 5 applies here as everywhere: every number this package
// combines carries its interval, and every combination widens rather than
// narrows wherever a covariance is unknown. Where this package cannot derive
// the corrected number, it returns nil — the refusal is the honest answer,
// and the schema can express it.
package portfolio

import (
	"math"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/interval"
)

// Method strings recorded on the intervals this package produces. They join
// the list on Interval.method (see proto/kno/v1/valuation.proto), and a
// reader comparing methods across runs sees the combination for what it is.
const (
	// MethodNetLossShared is a net-loss interval combined under the
	// assumption that the two deltas are perfectly correlated — the shape a
	// shared recorded baseline draw produces.
	MethodNetLossShared = "net-loss-shared"

	// MethodNetLossIndep is a net-loss interval combined under the
	// assumption that the two deltas are independent — the shape a fresh
	// control arm produces, or the best case a recorded baseline allows.
	MethodNetLossIndep = "net-loss-indep"
)

// NetDelta is one arm of the net-loss judgement: a point estimate, its
// interval half-width, and the population the mean is over.
//
// A NetDelta is deliberately the few numbers a recorded Valuation can supply
// (delta_goal or delta_control, their interval width, and n_routed or
// n_control) rather than the raw pairs, because the combination must be
// computable for a run that is already in the store.
type NetDelta struct {
	// Mean is the point estimate.
	Mean float64

	// Half is the half-width of a TWO-SIDED interval at the level NetLoss is
	// called with. A one-sided bound must be widened by the caller first —
	// its center is unknown, so this package cannot reconstruct the far side,
	// and silently reading a one-sided bound as symmetric would understate
	// the interval in exactly the direction a REGRESSION verdict cares about.
	Half float64

	// N is the population the mean was measured over.
	N int
}

// NetLoss combines a treatment delta and a control delta into one net
// judgement, weighted by their populations, with an interval that accounts
// for the shared recorded-baseline draw conservatively.
//
// The point estimate is the population-weighted mean:
//
//	net = (nT*meanT + nC*meanC) / (nT + nC)
//
// The interval is where the covariance shows up. The two deltas both pair
// against the recorded baseline draw (docs/debt.md#66 names the correlation),
// which makes their errors positively correlated — the variance of the net is
// LARGER than the independent combination, by up to the full product term.
// The exact covariance is not recoverable from the recorded aggregates, so:
//
//   - sharedDraw=true (the control arm read the recorded baseline) takes the
//     perfectly-correlated bound, the widest the unknown covariance allows:
//     half = (nT*halfT + nC*halfC) / (nT + nC).
//   - sharedDraw=false (a fresh control arm) takes the independent bound:
//     half = sqrt((nT*halfT)^2 + (nC*halfC)^2) / (nT + nC).
//
// The shared bound is always at least as wide as the independent one, and the
// caller who cannot say which scheme a run used must pass sharedDraw=true —
// the conservative direction, and the one this package documents.
//
// Returns nil when any input is unusable (non-positive population, non-finite
// or non-positive half-width, non-finite mean, invalid level) rather than
// laundering a bad input into an interval.
func NetLoss(treatment, control NetDelta, sharedDraw bool, level float64) *knov1.Interval {
	if treatment.N <= 0 || control.N <= 0 {
		return nil
	}
	if treatment.Half <= 0 || control.Half <= 0 {
		// A zero-width arm would drag the combined interval toward certainty
		// regardless of the other arm's width — the exact failure this
		// package refuses to manufacture.
		return nil
	}
	if !valid(level, treatment.Mean, treatment.Half, control.Mean, control.Half) {
		return nil
	}
	nT, nC := float64(treatment.N), float64(control.N)
	total := nT + nC
	mean := (nT*treatment.Mean + nC*control.Mean) / total

	var half float64
	method := MethodNetLossIndep
	if sharedDraw {
		// Perfectly-correlated bound: the covariance term is maximal, so the
		// half-widths combine linearly.
		half = (nT*treatment.Half + nC*control.Half) / total
		method = MethodNetLossShared
	} else {
		// Independent bound: variances add.
		half = math.Sqrt(nT*nT*treatment.Half*treatment.Half+nC*nC*control.Half*control.Half) / total
	}
	if math.IsInf(half, 0) || half <= 0 {
		return nil
	}

	nn := int32(total)
	return &knov1.Interval{
		Low:       mean - half,
		High:      mean + half,
		Level:     level,
		Method:    method,
		Sidedness: knov1.Sidedness_SIDEDNESS_TWO_SIDED,
		NPairs:    &nn,
	}
}

// Correct applies the Bonferroni correction to one interval, dividing the
// error budget across nScreened comparisons:
//
//	corrected level = 1 - (1 - level) / nScreened
//
// and rescaling the recorded half-width by the quantile ratio the interval's
// own method would produce at that level — the student-t quantile with the
// interval's recorded pair count for method "t", the normal quantile for
// "adjusted-wald", and an exact recomputation for "sign", whose width is a
// function of the level directly.
//
// Only two-sided intervals are correctable here: a one-sided bound does not
// record its center, so the far-side width is unknown and scaling the bounded
// side alone would silently claim confidence on the side the bound does not
// cover. A method this package cannot rescale, a missing pair count where the
// method needs it, or a corrected level the interval package would refuse all
// return nil — refusal over invention.
//
// The n_pairs recorded on the interval are preserved: correction changes the
// claim, not the sample it was computed from.
func Correct(iv *knov1.Interval, nScreened int) *knov1.Interval {
	if iv == nil || nScreened < 2 {
		return nil
	}
	if iv.GetSidedness() != knov1.Sidedness_SIDEDNESS_TWO_SIDED {
		return nil
	}
	level := iv.GetLevel()
	if !valid(level, iv.GetLow(), iv.GetHigh()) {
		return nil
	}
	center := (iv.GetLow() + iv.GetHigh()) / 2
	half := (iv.GetHigh() - iv.GetLow()) / 2
	if half <= 0 {
		return nil
	}

	correctedLevel := 1 - (1-level)/float64(nScreened)
	if correctedLevel <= 0.5 || correctedLevel >= 1 {
		return nil
	}

	nn := int(iv.GetNPairs())
	var ratio float64
	switch iv.GetMethod() {
	case interval.MethodStudentT:
		// The t-quantile needs the degrees of freedom; without the pair
		// count the ratio is under-determined, and substituting the normal
		// quantile would narrow the correction.
		if nn < 2 {
			return nil
		}
		df := nn - 1
		ratio = interval.Quantile(correctedLevel, knov1.Sidedness_SIDEDNESS_TWO_SIDED, df) /
			interval.Quantile(level, knov1.Sidedness_SIDEDNESS_TWO_SIDED, df)
	case interval.MethodAdjustedWald:
		ratio = interval.Quantile(correctedLevel, knov1.Sidedness_SIDEDNESS_TWO_SIDED, 0) /
			interval.Quantile(level, knov1.Sidedness_SIDEDNESS_TWO_SIDED, 0)
	case interval.MethodSign:
		// half = -ln(1-level)/n * scale; recover the scale and recompute at
		// the corrected level. Exact, not a quantile approximation.
		if nn < 1 {
			return nil
		}
		n := float64(nn)
		scale := half * n / -math.Log(1-level)
		correctedHalf := -math.Log(1-correctedLevel) / n * scale
		if math.IsNaN(correctedHalf) || math.IsInf(correctedHalf, 0) || correctedHalf <= 0 {
			return nil
		}
		n32 := int32(nn) //nolint:gosec // bounded by the eval set
		return &knov1.Interval{
			Low:       center - correctedHalf,
			High:      center + correctedHalf,
			Level:     correctedLevel,
			Method:    iv.GetMethod(),
			Sidedness: knov1.Sidedness_SIDEDNESS_TWO_SIDED,
			NPairs:    &n32,
		}
	default:
		// A method this package does not know how to rescale gets no
		// corrected interval — guessing a ratio would be inventing a method.
		return nil
	}

	if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio <= 0 {
		return nil
	}
	correctedHalf := half * ratio
	n32 := int32(nn) //nolint:gosec // bounded by the eval set
	return &knov1.Interval{
		Low:       center - correctedHalf,
		High:      center + correctedHalf,
		Level:     correctedLevel,
		Method:    iv.GetMethod(),
		Sidedness: knov1.Sidedness_SIDEDNESS_TWO_SIDED,
		NPairs:    &n32,
	}
}

// valid reports whether a level and a set of numbers are usable inputs.
func valid(level float64, numbers ...float64) bool {
	if math.IsNaN(level) || level <= 0.5 || level >= 1 {
		return false
	}
	for _, x := range numbers {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return false
		}
	}
	return true
}
