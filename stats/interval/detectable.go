package interval

import (
	"math"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// sdMaxPairedBinary is the largest standard deviation a paired binary
// difference can have.
//
// Differences live in {-1, 0, +1} and their variance 2p(1-p) is maximised at
// p = 0.5, so sqrt(0.5) bounds it. This is the STANDARD DEVIATION bound, not
// the variance bound 0.5 — confusing the two understates every minimum
// detectable effect by a factor of sqrt(2), which is a bug this package has
// already shipped once.
const sdMaxPairedBinary = 0.7071067811865476 // math.Sqrt(0.5)

// MinDetectableEffect returns the smallest effect n paired observations can
// separate from zero, at the given level and sidedness.
//
// It is a BOUND, not an estimate: the standard deviation used is the paired
// binary worst case, so the answer depends on n alone and can be reported
// before any measurement exists. That is what makes it printable by
// `kno eval inspect` ahead of a run, and it is also why it over-warns on a
// continuous Goal whose true variance is lower — conservative in the
// recoverable direction, never claiming an eval set is more powerful than it
// is.
//
// SIDEDNESS MATTERS AND IS NOT A DETAIL. A one-sided bound spends its whole
// error budget on one tail, so at the same level it is TIGHTER than the
// two-sided bound. Directional questions ("did this Asset make things worse")
// take SIDEDNESS_UPPER; symmetric ones ("is this behavior distinguishable
// from noise") take SIDEDNESS_TWO_SIDED. Reusing the one-sided figure for a
// symmetric question reports more power than the data has.
//
// The quantile comes from Quantile with df = n - 1, which is the Student-t
// quantile for n >= 2 and the normal quantile at n = 1 — the same computation
// every interval in this package is built from, so a reported bound and the
// interval it anticipates cannot come from two different distributions.
//
// n < 1 returns 0, which means NOTHING IS DETECTABLE rather than "a very
// small effect is". A caller rendering this must not print 0 as a tight
// bound; core/value.Plan.MinDetectableHarm documents the same reading for the
// empty control sample.
func MinDetectableEffect(n int, side knov1.Sidedness, level float64) float64 {
	if n < 1 || !validLevel(level) {
		return 0
	}
	q := Quantile(level, side, n-1)
	if math.IsNaN(q) || math.IsInf(q, 0) {
		return 0
	}
	return q * sdMaxPairedBinary / math.Sqrt(float64(n))
}
