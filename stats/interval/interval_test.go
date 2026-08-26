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

// coverageFloor is the lowest coverage a correct method can plausibly show,
// given that the measurement is itself a sample.
//
// Derived from the run count rather than picked, because a picked floor is
// where a real defect hides: this package shipped a z-interval labelled "t"
// that covered 0.930 at fifteen pairs, and the test that should have caught it
// used a hand-chosen 0.90 floor while its comment claimed "at least nominal".
//
// Three standard errors of a binomial proportion at the nominal rate. A method
// that genuinely covers will land above this essentially always; one that
// systematically under-covers — which is what every bug in this package's
// history looked like — will not.
func coverageFloor(runs int) float64 {
	const level = interval.DefaultLevel
	se := math.Sqrt(level * (1 - level) / float64(runs))
	return level - 3*se
}

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
			const runs = 20000
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
			if got < coverageFloor(runs) {
				t.Errorf("coverage = %.4f, below the %.4f floor at a nominal %.2f. "+
					"Under-coverage is a "+
					"claim of confidence the data does not support, and every "+
					"delta this project reports rests on it",
					got, coverageFloor(runs), interval.DefaultLevel)
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
	if asBinary.GetMethod() != interval.MethodAdjustedWald {
		t.Errorf("binary method = %q, want %q", asBinary.GetMethod(), interval.MethodAdjustedWald)
	}

	// Repeated trials leave the binary path: the difference takes trials+1
	// values and is no longer a discordance count.
	repeated := interval.Paired(looksBinary, binary, 3, interval.DefaultLevel)
	if repeated == nil {
		t.Fatal("no interval")
	}
	if repeated.GetMethod() == interval.MethodAdjustedWald {
		t.Error("repeated trials still took the paired-binary path, where the " +
			"discordant-count methods are undefined on fractional differences")
	}
}

// TestAHarmBoundActuallyDetectsHarm.
//
// The first version of this test asserted the sidedness and the tightness and
// never that the bound ANSWERS ITS QUESTION — so it passed against a bound
// pointing the wrong way, which is what shipped. Paired's deltas are
// sign-corrected so positive is better, which makes an UPPER bound a statement
// about how much the Asset helps. Harm is delta >= -epsilon, so the bound is
// the one below.
//
// Asserted by POWER, not by shape: a harm detector that cannot see a real
// regression is the failure this instrument exists to prevent, and shape
// assertions cannot see that.
func TestAHarmBoundActuallyDetectsHarm(t *testing.T) {
	t.Parallel()

	// A -0.30 regression: three Cases in ten that the Asset broke.
	regressed := []float64{0, -1, 0, 0, -1, 0, 0, -1, 0, 0}
	inert := make([]float64, 10)

	harm := interval.HarmBound(regressed, binary, 1, interval.DefaultLevel)
	if harm == nil {
		t.Fatal("no bound")
	}
	if harm.GetSidedness() != knov1.Sidedness_SIDEDNESS_LOWER {
		t.Errorf("sidedness = %v, want LOWER. An upper bound on a "+
			"positive-is-better delta bounds how much the Asset HELPS, which is "+
			"the opposite of the question", harm.GetSidedness())
	}
	if harm.GetHigh() != 0 {
		t.Errorf("high = %g, want 0 — an infinity does not survive protojson, "+
			"which serializes it as a string into a numeric field", harm.GetHigh())
	}

	// The bound must exclude "no harm" for a real regression: if the true
	// effect could be zero, the bound has told us nothing.
	if harm.GetLow() >= 0 {
		t.Errorf("low = %g on a -0.30 regression; the bound admits zero harm, "+
			"so it cannot distinguish a broken Asset from an inert one",
			harm.GetLow())
	}

	// And it must NOT cry harm on an inert Asset.
	clean := interval.HarmBound(inert, binary, 1, interval.DefaultLevel)
	if clean == nil {
		t.Fatal("no bound for an inert Asset")
	}
	if clean.GetLow() > 0 {
		t.Errorf("low = %g on an Asset that changed nothing", clean.GetLow())
	}

	// A one-sided bound spends its whole error budget on one tail, so it is
	// tighter than the matching end of the two-sided interval.
	twoSided := interval.Paired(regressed, binary, 1, interval.DefaultLevel)
	if twoSided == nil {
		t.Fatal("no interval")
	}
	if harm.GetLow() <= twoSided.GetLow() {
		t.Errorf("one-sided low %g is not tighter than two-sided low %g; the "+
			"level is being spread over two tails on a bound that has one",
			harm.GetLow(), twoSided.GetLow())
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
			const runs = 20000

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
			// At the NOMINAL level, not below it. The first version of this
			// test used a 0.90 floor while its own comment claimed "at least
			// nominal" — which is exactly the slack a z-interval mislabelled
			// as t hid in: 0.930 at fifteen pairs, inside a 0.90 floor.
			if got < coverageFloor(runs) {
				t.Errorf("coverage = %.4f, below the %.4f floor at a nominal %.2f, "+
					"on the CONTINUOUS path. Every judged Goal lands here",
					got, coverageFloor(runs), interval.DefaultLevel)
			}
		})
	}
}

// TestPairedTrialsMakesPseudoReplicationUnrepresentable.
//
// Paired takes one value per Case. A caller holding per-Case-per-trial
// measurements — which is exactly what the measurement loop produces — can
// flatten them into one slice, and nothing errors: the pair count is not
// recoverable from the slice, so the interval comes back about sqrt(k) too
// narrow with n_pairs k times too large. That is pseudo-replication, and it is
// the failure a shape can prevent and a comment cannot.
func TestPairedTrialsMakesPseudoReplicationUnrepresentable(t *testing.T) {
	t.Parallel()

	perCase := [][]float64{
		{1, 0, 1},
		{0, 0, 0},
		{1, 1, 1},
		{0, -1, 0},
		{1, 0, 0},
		{0, 0, 1},
		{1, 1, 0},
		{0, 0, 0},
	}

	grouped := interval.PairedTrials(perCase, binary, interval.DefaultLevel)
	if grouped == nil {
		t.Fatal("no interval")
	}
	if grouped.GetNPairs() != int32(len(perCase)) {
		t.Errorf("n_pairs = %d, want %d — the pair count must be the number of "+
			"CASES, not of measurements", grouped.GetNPairs(), len(perCase))
	}

	// The flattened mistake, for comparison: same data, wrong shape.
	var flat []float64
	for _, tr := range perCase {
		flat = append(flat, tr...)
	}
	flattened := interval.Paired(flat, binary, 1, interval.DefaultLevel)
	if flattened == nil {
		t.Fatal("no interval")
	}
	if flattened.GetHigh()-flattened.GetLow() >= grouped.GetHigh()-grouped.GetLow() {
		t.Error("flattening trials did not narrow the interval, so this test " +
			"is not exercising the hazard it describes")
	}

	// A ragged input is refused rather than averaged over mixed denominators.
	if interval.PairedTrials([][]float64{{1, 0}, {1}}, binary, interval.DefaultLevel) != nil {
		t.Error("a ragged input was accepted; Cases measured a different number " +
			"of times would be weighted equally, which nothing downstream sees")
	}

	// trials < 1 is refused: Valuation.trials is a proto int32 whose unset
	// value is zero, and zero would select the paired-binary method.
	if interval.Paired([]float64{1, 0, 1}, binary, 0, interval.DefaultLevel) != nil {
		t.Error("trials = 0 was accepted, which is what an unpopulated proto field reads as")
	}
}
