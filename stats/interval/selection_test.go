package interval

import (
	"math"
	"math/rand"
	"testing"
)

// TestScreeningInflationBound pins the winner's-curse characterization the
// debt ledger demands (docs/debt.md#66): how much ranking on a noisy number
// inflates the best-looking Assets, as a function of how many were screened.
//
// The setting is the Select stage's: n_screened Assets, every one with a true
// effect of zero, each measured with noise sigma. The Assets that RANK BEST
// are the ones whose noise lied most, so the best measured delta is not a
// sample of the pool — it is an inflated draw from the right tail. How far up
// the tail selection reaches is a known function of the screen size:
//
//	E[max of n N(0, sigma)] ~= sigma * sqrt(2 * ln(n))
//
// which is an upper bound at every finite n — the extreme-value asymptotics
// approach it from below. The bound, not the asymptotic: the report can say
// "ranking n_screened nulls can fake a delta of sigma*sqrt(2*ln(n_screened))"
// without overstating the tail it claims to bound.
func TestScreeningInflationBound(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		nScreened int
		screens   int
		sigma     float64
	}{
		{"four assets", 4, 20000, 0.01},
		{"sixteen assets", 16, 20000, 0.01},
		{"sixty-four assets", 64, 10000, 0.01},
		{"two hundred fifty-six assets", 256, 5000, 0.01},
	}
	for i, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewSource(int64(i + 1)))
			var sum float64
			for s := 0; s < tc.screens; s++ {
				best := -math.MaxFloat64
				for a := 0; a < tc.nScreened; a++ {
					d := rng.NormFloat64() * tc.sigma
					if d > best {
						best = d
					}
				}
				sum += best
			}
			observed := sum / float64(tc.screens)

			bound := tc.sigma * math.Sqrt(2*math.Log(float64(tc.nScreened)))
			if observed > bound {
				t.Errorf("observed inflation %v exceeds the pinned bound %v at n_screened=%d",
					observed, bound, tc.nScreened)
			}
			if observed > 0.95*bound {
				t.Errorf("observed inflation %v sits at the bound %v instead of under it: "+
					"the characterization has no headroom", observed, bound)
			}
		})
	}
}

// TestScreeningInflationGrowsWithScreenSize is the second half of the
// characterization: the inflation is a function of n_screened, not a single
// number, so a larger screen must inflate more. Serial, because the claim is
// a comparison across screen sizes and the parallel subtests cannot compare.
func TestScreeningInflationGrowsWithScreenSize(t *testing.T) {
	t.Parallel()
	var observed []float64
	for i, n := range []int{4, 16, 64, 256} {
		rng := rand.New(rand.NewSource(int64(i + 1)))
		const screens, sigma = 20000, 0.01
		var sum float64
		for s := 0; s < screens; s++ {
			best := -math.MaxFloat64
			for a := 0; a < n; a++ {
				d := rng.NormFloat64() * sigma
				if d > best {
					best = d
				}
			}
			sum += best
		}
		observed = append(observed, sum/screens)
	}
	for i := 1; i < len(observed); i++ {
		if observed[i] <= observed[i-1] {
			t.Errorf("inflation did not grow with n_screened: %v (n=%d) <= %v (n=%d); "+
				"the characterization is not a function of the screen size",
				observed[i], []int{4, 16, 64, 256}[i], observed[i-1], []int{4, 16, 64, 256}[i-1])
		}
	}
}
