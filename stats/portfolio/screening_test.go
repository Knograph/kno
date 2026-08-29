package portfolio

import (
	"math"
	"math/rand"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/interval"
	"github.com/stretchr/testify/require"
)

// TestScreeningFDRAtCorrectedLevel is the first half of the winner's-curse
// evidence the plan demands (docs/debt.md#66 names the full pair): thousands
// of simulated screens over synthetic ground truth, asserting the
// false-discovery rate at the corrected level.
//
// Every Asset in a screen is a null — true effect zero — with a noisy
// measured delta, which is exactly the regime the winner's curse lives in:
// the Assets that look best are the ones whose noise lied most. Select keeps
// the top-ranked subset; a rejection is a kept Asset whose Bonferroni-
// corrected interval excludes zero. Under the correction, the family-wise
// error rate over the whole screen is bounded by alpha, so the false-
// discovery rate — which is no more than the family-wise rate — must be too.
//
// Two assertions keep the test from being vacuous: the corrected screens stay
// at or under alpha, and the uncorrected screens break it by an order of
// magnitude, so the correction is what the margin is buying.
func TestScreeningFDRAtCorrectedLevel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		screened       int // Assets in each simulated screen
		kept           int // the top-ranked subset Select would consider
		pairs          int // pairs behind each asset's delta
		sigma          float64
		screens        int
		wantFWER       float64
		wantUncorrFWER float64
	}{
		{
			// A five-best-of-two-hundred screen: the 5th-largest null delta
			// sits around 2.4 sigma — beyond the naive 95% t-threshold
			// (2.26) so the naive interval fabricates claims, and below the
			// corrected threshold (t at 0.99975 ~ 4.4) so the correction
			// holds the line.
			name:     "two hundred assets, five kept",
			screened: 200, kept: 5, pairs: 10, sigma: 1.0,
			screens:        2000,
			wantFWER:       0.055, // nominal 0.05, a little room for the t-ratio arithmetic
			wantUncorrFWER: 0.60,  // the naive interval is wrong by an order of magnitude
		},
		{
			name:     "thousand assets, ten kept",
			screened: 1000, kept: 10, pairs: 20, sigma: 2.0,
			screens:        1000,
			wantFWER:       0.055,
			wantUncorrFWER: 0.75,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewSource(1))
			// sd of the measured delta: sigma / sqrt(pairs) — the noise a
			// mean over `pairs` observations carries.
			se := math.Sqrt(float64(tc.pairs))

			// The interval a real measurement would have recorded, before
			// any correction: center the noisy delta, width the t-quantile
			// at the recorded level times the pair-error.
			recordedHalf := interval.Quantile(
				interval.DefaultLevel,
				knov1.Sidedness_SIDEDNESS_TWO_SIDED,
				tc.pairs-1,
			) * tc.sigma / se

			var correctedRejections, keptCount, uncorrectedRejections int
			var correctedScreens, uncorrectedScreens int
			for s := 0; s < tc.screens; s++ {
				// One screen: screened null Assets, each with a noisy delta.
				// Rank: deltas descend; keep the top `kept`.
				deltas := make([]float64, tc.screened)
				for i := range deltas {
					deltas[i] = rng.NormFloat64() * tc.sigma / se
				}
				var screenHits, screenUncorr int
				for _, d := range topK(deltas, tc.kept) {
					keptCount++
					// A rejection is a kept Asset whose interval excludes
					// zero. The corrected interval comes from Correct with
					// the whole screen as the multiplicity — the exact
					// mechanism Select runs.
					corrected := Correct(twoSided(d, recordedHalf, interval.DefaultLevel, interval.MethodStudentT, tc.pairs), tc.screened)
					if corrected.GetLow() > 0 || corrected.GetHigh() < 0 {
						correctedRejections++
						screenHits++
					}
					if math.Abs(d) > recordedHalf {
						uncorrectedRejections++
						screenUncorr++
					}
				}
				if screenHits > 0 {
					correctedScreens++
				}
				if screenUncorr > 0 {
					uncorrectedScreens++
				}
			}

			fwer := float64(correctedScreens) / float64(tc.screens)
			fdr := float64(correctedRejections) / float64(keptCount)
			require.LessOrEqual(t, fwer, tc.wantFWER,
				"family-wise error at the corrected level must stay at or under alpha")
			require.LessOrEqual(t, fdr, tc.wantFWER,
				"false-discovery rate cannot exceed the family-wise error rate it is bounded by")

			uncorrFWER := float64(uncorrectedScreens) / float64(tc.screens)
			require.Greater(t, uncorrFWER, tc.wantUncorrFWER,
				"the naive interval is not the false-discovery engine the correction exists for")
		})
	}
}

// topK returns the K largest values of deltas, ordered descending.
func topK(deltas []float64, k int) []float64 {
	best := make([]float64, 0, k)
	for _, d := range deltas {
		if len(best) < k {
			best = append(best, d)
			continue
		}
		// Insert in rank order; drop the smallest once full.
		for i, b := range best {
			if d > b {
				best = append(best, 0)
				copy(best[i+1:], best[i:])
				best[i] = d
				best = best[:k]
				break
			}
		}
	}
	return best
}
