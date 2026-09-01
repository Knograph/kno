package judge_test

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/knograph/kno/judge"
	"github.com/knograph/kno/stats/interval"
)

// The gate rests on a bootstrap this repository had never shipped, and a
// percentile interval is known to under-cover for statistics whose
// small-sample distribution is skewed. Kappa at n = 50..200 with a true value
// near 0.60 — a bounded statistic at a hard decision boundary — is exactly
// that regime.
//
// Under-coverage there does not produce a visibly wrong number. It silently
// flips PASS to INDETERMINATE, or INDETERMINATE to PASS, which is the entire
// output of the command. So coverage is measured on the grid inside which the
// decision is actually made, not "in general".
//
// The grid is n in {50, 100, 200} x true kappa in {0.55, 0.575, 0.60, 0.625,
// 0.65}, at a nominal 95% level. `make judge-calibrate-check` does not enter
// `make check` until this passes.

// coverageFloor is how far below nominal a cell may read before this fails.
//
// 0.90 against a nominal 0.95. Monte-Carlo noise at the trial count below is
// about +/- 0.015 at one standard error, so a floor at 0.94 would fail on
// noise; a floor at 0.85 would accept a genuinely broken method. The gap
// between the floor and nominal is the amount of under-coverage that would
// still leave a verdict decided by the data rather than by the method.
const coverageFloor = 0.90

// TestBootstrapCoverageOnTheDecisionGrid is acceptance criterion 19.
func TestBootstrapCoverageOnTheDecisionGrid(t *testing.T) {
	t.Parallel()

	// Trials per cell. Higher is better and this is what a -short run can
	// afford; the nightly is free to raise it.
	trials := 300
	if testing.Short() {
		trials = 80
	}

	for _, n := range []int{50, 100, 200} {
		for _, trueKappa := range []float64{0.55, 0.575, 0.60, 0.625, 0.65} {
			t.Run(cellName(n, trueKappa), func(t *testing.T) {
				t.Parallel()

				covered, valid := 0, 0
				rng := rand.New(rand.NewPCG(uint64(n), uint64(trueKappa*1000)))
				for trial := range trials {
					judged, human := simulate(rng, n, trueKappa)
					iv := interval.Percentile(n, func(idx []int) float64 {
						return judge.KappaOver(judged, human, idx)
					}, interval.Bootstrap{
						Resamples: 500,
						Seed:      uint64(trial) + 1,
						Support:   &interval.Support{Low: -1, High: 1},
					})
					if iv == nil {
						continue
					}
					valid++
					if iv.GetLow() <= trueKappa && trueKappa <= iv.GetHigh() {
						covered++
					}
				}
				if valid == 0 {
					t.Fatal("no interval was computable in this cell")
				}
				got := float64(covered) / float64(valid)
				t.Logf("coverage %.3f over %d trials (n=%d, kappa=%.3f)", got, valid, n, trueKappa)
				if got < coverageFloor {
					t.Errorf("coverage %.3f at a nominal 0.95 for n=%d, true kappa=%.3f.\n"+
						"Under-coverage here does not look like a wrong number: it flips "+
						"PASS to INDETERMINATE and back. BCa stops being the named upgrade "+
						"and becomes the ship.", got, n, trueKappa)
				}
			})
		}
	}
}

// simulate draws a balanced set of human labels and a judge with symmetric
// error epsilon, chosen so the population kappa is exactly the target.
//
// On a balanced set with symmetric error, kappa = 1 - 2*epsilon — the identity
// the floor rests on — so the generator inverts it: epsilon = (1 - kappa) / 2.
// Using the identity to build the ground truth is what makes "the true kappa"
// a known quantity rather than an estimate from the same sample.
func simulate(rng *rand.Rand, n int, trueKappa float64) (judged, human []bool) {
	eps := (1 - trueKappa) / 2
	judged = make([]bool, n)
	human = make([]bool, n)
	for i := range n {
		h := rng.Float64() < 0.5
		human[i] = h
		j := h
		if rng.Float64() < eps {
			j = !h
		}
		judged[i] = j
	}
	return judged, human
}

func cellName(n int, k float64) string {
	return "n=" + itoa(n) + "/kappa=" + ftoa(k)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func ftoa(f float64) string {
	milli := int(math.Round(f * 1000))
	return itoa(milli/1000) + "." + itoa(milli%1000)
}
