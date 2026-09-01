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

// Method strings recorded on the intervals NetLoss produces.
//
// MOVED to stats/interval by the bridge eval-seam plan
// (docs/plans/2026-09-01-bridge-eval-seam.md §4): bridge needs the same
// net-loss combination for its interference read (stats/interval.NetEffect),
// and this package already imports stats/interval for Quantile, so a
// stats/interval -> stats/portfolio import would cycle. Aliased here,
// unchanged, so every existing caller and test in this package compiles
// without modification — see stats/interval/neteffect.go for the real
// definitions and their doc comments.
const (
	MethodNetLossShared = interval.MethodNetLossShared
	MethodNetLossIndep  = interval.MethodNetLossIndep
)

// NetDelta is one arm of the net-loss judgement. See interval.NetDelta.
type NetDelta = interval.NetDelta

// NetLoss combines a treatment delta and a control delta into one net
// judgement. See interval.NetLoss.
var NetLoss = interval.NetLoss

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
