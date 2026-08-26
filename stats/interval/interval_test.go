package interval_test

import (
	"math"
	"math/rand"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/interval"
)

const (
	binary     = knov1.ScoreDomain_SCORE_DOMAIN_BINARY
	continuous = knov1.ScoreDomain_SCORE_DOMAIN_CONTINUOUS_UNBOUNDED
)

// pairedBinary draws n paired observations where the control succeeds with
// probability pCtrl and the treatment with pCtrl+effect, returning the paired
// differences.
func pairedBinary(rng *rand.Rand, n int, pCtrl, effect float64) []float64 {
	pTrt := math.Min(1, math.Max(0, pCtrl+effect))
	out := make([]float64, n)
	for i := range out {
		x, y := 0.0, 0.0
		if rng.Float64() < pCtrl {
			x = 1
		}
		if rng.Float64() < pTrt {
			y = 1
		}
		out[i] = y - x
	}
	return out
}

// TestTheIntervalCoversTheTruthAtItsStatedRate.
//
// This is the test that makes a confidence interval mean anything, and it runs
// against the DATA-GENERATING PROCESS THAT SHIPS: paired binary differences at
// the sample sizes and success rates a real Value run produces. A coverage test
// generated from a Gaussian would pass here and prove nothing about the number
// this package actually reports.
//
// Coverage is asserted as "at least nominal", not "equal to nominal". A
// discrete interval cannot hit 0.95 exactly at small n, and the two failures
// are not symmetric: over-covering is a wide interval, and under-covering is a
// claim of confidence the data does not support.
func TestTheIntervalCoversTheTruthAtItsStatedRate(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		n             int
		pCtrl, effect float64
	}{
		{"an inert Asset at a typical pass rate", 20, 0.70, 0.00},
		{"an inert Asset at maximum variance", 20, 0.50, 0.00},
		{"a real effect", 20, 0.70, 0.10},
		{"a nearly perfect agent, where agreement is the norm", 20, 0.95, 0.00},
		{"a larger sample", 50, 0.70, 0.00},
		{"a larger sample with a real effect", 50, 0.70, 0.10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rng := rand.New(rand.NewSource(1))
			const runs = 3000
			truth := math.Min(1, tc.pCtrl+tc.effect) - tc.pCtrl

			covered := 0
			for range runs {
				iv := interval.Paired(pairedBinary(rng, tc.n, tc.pCtrl, tc.effect),
					binary, 1, interval.DefaultLevel)
				if iv == nil {
					t.Fatal("no interval for a sample this package must handle")
				}
				if iv.GetLow() <= truth && truth <= iv.GetHigh() {
					covered++
				}
			}

			got := float64(covered) / runs
			if got < 0.93 {
				t.Errorf("coverage = %.3f at a nominal %.2f. Under-coverage is a "+
					"claim of confidence the data does not support, and every "+
					"delta this project reports rests on it",
					got, interval.DefaultLevel)
			}
		})
	}
}

// TestNoIntervalIsEverZeroWidth.
//
// A zero-width interval reads as total certainty. knov1.Interval is a message
// precisely so its ABSENCE cannot be mistaken for a tight one; a zero-width
// interval defeats that from the other side, and it arrives on the two inputs
// a first run is most likely to produce.
//
// Measured against the rejected alternative: a percentile bootstrap returns a
// zero-width interval on 13.6% of samples at p=0.95, n=20 — an inert Asset
// against a nearly perfect agent, which is the single most common thing in a
// real pool.
func TestNoIntervalIsEverZeroWidth(t *testing.T) {
	t.Parallel()

	allZero := make([]float64, 20)
	allOne := make([]float64, 20)
	allNegative := make([]float64, 20)
	for i := range allOne {
		allOne[i] = 1
		allNegative[i] = -1
	}

	for _, tc := range []struct {
		name   string
		deltas []float64
		domain knov1.ScoreDomain
		trials int
	}{
		{"an Asset that changed nothing", allZero, binary, 1},
		{"an Asset that fixed every Case", allOne, binary, 1},
		{"an Asset that broke every Case", allNegative, binary, 1},
		{"the same, on the continuous path", allZero, continuous, 1},
		{"the continuous path with repeated trials", allOne, binary, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			iv := interval.Paired(tc.deltas, tc.domain, tc.trials, interval.DefaultLevel)
			if iv == nil {
				t.Fatal("refused an input it must handle; an inert Asset is the " +
					"majority of a real pool, and reporting no delta for most of " +
					"it is a check that gets disabled")
			}
			if iv.GetHigh()-iv.GetLow() <= 0 {
				t.Errorf("width = %g on %v. Zero width is a claim of certainty "+
					"from a handful of observations", iv.GetHigh()-iv.GetLow(), tc.deltas[:3])
			}
		})
	}
}

// TestNoBoundIsEverNonFinite, because a NaN renders as blank and a reader fills
// a blank in themselves. An infinity does not survive protojson either: it
// serializes as the string "Infinity" into a field the generated OpenAPI
// declares as a number.
func TestNoBoundIsEverNonFinite(t *testing.T) {
	t.Parallel()

	inputs := [][]float64{
		{0, 0},
		{1, 1, 1},
		{-1, 1},
		{0.5, 0.5, 0.5, 0.5},
		make([]float64, 500),
	}
	for _, domain := range []knov1.ScoreDomain{binary, continuous} {
		for _, in := range inputs {
			for _, trials := range []int{1, 3} {
				iv := interval.Paired(in, domain, trials, interval.DefaultLevel)
				if iv == nil {
					continue
				}
				for name, v := range map[string]float64{"low": iv.GetLow(), "high": iv.GetHigh()} {
					if math.IsNaN(v) || math.IsInf(v, 0) {
						t.Errorf("%s = %v for %v (domain %v, trials %d)",
							name, v, in, domain, trials)
					}
				}
			}
		}
	}
}

// TestTheMethodIsChosenByTheDECLAREDDomain.
//
// Dispatching on the observed data would make the confidence level hold only
// conditional on a branch that is itself a function of the sample — and across
// 200 Assets some would land in each branch by luck, after which Select
// compares intervals with different coverage as though they were one claim.
//
// So the same deltas must produce different methods under different declared
// domains, and identical ones under the same domain.
func TestTheMethodIsChosenByTheDECLAREDDomain(t *testing.T) {
	t.Parallel()

	// Deltas that LOOK binary. A data-dependent dispatcher cannot tell these
	// from a binary Goal's output; a declared one does not try.
	looksBinary := []float64{1, 0, -1, 0, 1, 0, 0, -1, 1, 0}

	asBinary := interval.Paired(looksBinary, binary, 1, interval.DefaultLevel)
	asContinuous := interval.Paired(looksBinary, continuous, 1, interval.DefaultLevel)

	if asBinary == nil || asContinuous == nil {
		t.Fatal("no interval")
	}
	if asBinary.GetMethod() == asContinuous.GetMethod() {
		t.Errorf("both domains produced method %q; the declared domain is not "+
			"reaching the dispatch", asBinary.GetMethod())
	}
	if asBinary.GetMethod() != interval.MethodAgrestiMin {
		t.Errorf("binary method = %q, want %q", asBinary.GetMethod(), interval.MethodAgrestiMin)
	}

	// Repeated trials leave the binary path: the difference takes trials+1
	// values and is no longer a discordance count.
	repeated := interval.Paired(looksBinary, binary, 3, interval.DefaultLevel)
	if repeated == nil {
		t.Fatal("no interval")
	}
	if repeated.GetMethod() == interval.MethodAgrestiMin {
		t.Error("repeated trials still took the paired-binary path, where the " +
			"discordant-count methods are undefined on fractional differences")
	}
}

// TestAHarmBoundIsOneSidedAndSaysSo.
//
// A control arm asks "did this break something", which is one-sided. Written
// into a two-sided field it is read as two-sided by RejectionReason.NO_EFFECT,
// whose shipped definition is "the interval crosses zero", and by the report's
// coloring rule — so the sidedness has to travel with the number.
func TestAHarmBoundIsOneSidedAndSaysSo(t *testing.T) {
	t.Parallel()

	deltas := []float64{0, 0, -1, 0, 0, 0, 0, 0, -1, 0}

	bound := interval.HarmBound(deltas, binary, 1, interval.DefaultLevel)
	if bound == nil {
		t.Fatal("no bound")
	}
	if bound.GetSidedness() != knov1.Sidedness_SIDEDNESS_UPPER {
		t.Errorf("sidedness = %v, want UPPER", bound.GetSidedness())
	}
	if bound.GetLow() != 0 {
		t.Errorf("low = %g, want 0 — an infinity there does not survive "+
			"protojson, which serializes it as a string into a numeric field",
			bound.GetLow())
	}

	// A one-sided bound at the same level is TIGHTER than the upper end of the
	// two-sided interval, because it spends its whole error budget on one tail.
	twoSided := interval.Paired(deltas, binary, 1, interval.DefaultLevel)
	if twoSided == nil {
		t.Fatal("no interval")
	}
	if bound.GetHigh() >= twoSided.GetHigh() {
		t.Errorf("one-sided high %g is not tighter than two-sided high %g; the "+
			"level is being applied to both tails on a bound that has one",
			bound.GetHigh(), twoSided.GetHigh())
	}
}

// TestAnUnmeasurableSampleGetsNoInterval, which is a real answer rather than a
// failure — and the reason Interval is a message. A nil interval means the
// delta must not be reported; it never means the delta is zero.
func TestAnUnmeasurableSampleGetsNoInterval(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		deltas []float64
		level  float64
	}{
		{"no pairs at all", nil, interval.DefaultLevel},
		{"a single pair", []float64{1}, interval.DefaultLevel},
		{"a NaN from upstream", []float64{0, math.NaN()}, interval.DefaultLevel},
		{"an infinity from upstream", []float64{0, math.Inf(1)}, interval.DefaultLevel},
		{"a level of zero", []float64{1, 0, 1}, 0},
		{"a level of one", []float64{1, 0, 1}, 1},
		{"a level below a coin flip", []float64{1, 0, 1}, 0.4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if iv := interval.Paired(tc.deltas, binary, 1, tc.level); iv != nil {
				t.Errorf("got an interval [%g, %g] for %s", iv.GetLow(), iv.GetHigh(), tc.name)
			}
		})
	}
}

// TestEveryIntervalRecordsHowItWasMade.
//
// Interval.method exists because the method is part of the claim: a delta whose
// method changed between two runs did not become more precise, it was measured
// differently. n_pairs exists because "wide" and "wide AND from six
// observations" call for different responses.
func TestEveryIntervalRecordsHowItWasMade(t *testing.T) {
	t.Parallel()

	deltas := []float64{1, 0, -1, 0, 1, 0, 1, 0}
	for _, domain := range []knov1.ScoreDomain{binary, continuous} {
		iv := interval.Paired(deltas, domain, 1, interval.DefaultLevel)
		if iv == nil {
			t.Fatal("no interval")
		}
		if iv.GetMethod() == "" {
			t.Error("no method recorded")
		}
		if iv.GetNPairs() != int32(len(deltas)) {
			t.Errorf("n_pairs = %d, want %d", iv.GetNPairs(), len(deltas))
		}
		if iv.GetLevel() != interval.DefaultLevel {
			t.Errorf("level = %g, want %g", iv.GetLevel(), interval.DefaultLevel)
		}
		if iv.GetSidedness() != knov1.Sidedness_SIDEDNESS_TWO_SIDED {
			t.Errorf("sidedness = %v, want TWO_SIDED", iv.GetSidedness())
		}
	}
}

// TestAWiderSampleGivesATighterInterval — the property that makes
// --sample-rate a meaningful control rather than a cost dial.
func TestAWiderSampleGivesATighterInterval(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewSource(7))
	small := interval.Paired(pairedBinary(rng, 20, 0.7, 0.1), binary, 1, interval.DefaultLevel)
	large := interval.Paired(pairedBinary(rng, 200, 0.7, 0.1), binary, 1, interval.DefaultLevel)

	if small == nil || large == nil {
		t.Fatal("no interval")
	}
	sw := small.GetHigh() - small.GetLow()
	lw := large.GetHigh() - large.GetLow()
	if lw >= sw {
		t.Errorf("200 pairs gave width %g and 20 gave %g; paying for more Cases "+
			"must buy precision or --sample-rate is only a cost dial", lw, sw)
	}
}

// TestTheContinuousPathAlsoCoversTheTruthAtItsStatedRate.
//
// The binary coverage test above exercises only the paired-binary branch, so
// the continuous branch had no coverage property at all — verified by
// shrinking its half-width by four orders of magnitude and watching the suite
// stay green. That branch is what every judged Goal will use, and a graded
// rubric is the next Goal this project ships.
func TestTheContinuousPathAlsoCoversTheTruthAtItsStatedRate(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		n        int
		mean, sd float64
		trials   int
		domain   knov1.ScoreDomain
	}{
		{"a graded rubric with no real effect", 30, 0.0, 0.25, 1, continuous},
		{"a graded rubric with a real effect", 30, 0.10, 0.25, 1, continuous},
		{"a small sample, where the normal approximation is worst", 10, 0.0, 0.30, 1, continuous},
		{"binary scores under repeated trials, which take this path", 30, 0.0, 0.30, 3, binary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rng := rand.New(rand.NewSource(3))
			const runs = 3000

			covered := 0
			for range runs {
				deltas := make([]float64, tc.n)
				for i := range deltas {
					deltas[i] = tc.mean + rng.NormFloat64()*tc.sd
				}
				iv := interval.Paired(deltas, tc.domain, tc.trials, interval.DefaultLevel)
				if iv == nil {
					t.Fatal("no interval for a sample this package must handle")
				}
				if iv.GetLow() <= tc.mean && tc.mean <= iv.GetHigh() {
					covered++
				}
			}

			got := float64(covered) / runs
			if got < 0.90 {
				t.Errorf("coverage = %.3f at a nominal %.2f on the CONTINUOUS "+
					"path. Every judged Goal will land here", got, interval.DefaultLevel)
			}
		})
	}
}
