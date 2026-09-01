package judge_test

import (
	"math"
	"testing"

	"github.com/knograph/kno/judge"
)

// The floor rests on kappa = 1 - 2*epsilon, which holds exactly only on a
// PERFECTLY balanced set. The set's balance invariant is 40% minority, not
// 50%, so the identity is approximate — and the size of that approximation is
// derived here rather than asserted as a round number in prose.
//
// docs/what-the-numbers-mean.md publishes this table. This test is what stops
// the doc and the arithmetic drifting apart: each row is recomputed from
// (p, epsilon) through the production kappa, and must match to 1e-3.

// TestPrevalenceSensitivityTable is acceptance criterion 20.
func TestPrevalenceSensitivityTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// p is the minority-class share of the set.
		p float64
		// eps is the judge's symmetric error rate.
		eps float64
		// wantKappa is the published figure.
		wantKappa float64
		// wantShortfall is the published percentage shortfall against the
		// 1 - 2*eps identity.
		wantShortfall float64
	}{
		{"balanced, 20% error", 0.50, 0.20, 0.600, 0.0},
		{"45% minority, 20% error", 0.45, 0.20, 0.598, 0.4},
		{"40% minority — the invariant, 20% error", 0.40, 0.20, 0.590, 1.6},
		{"30% minority, 20% error", 0.30, 0.20, 0.558, 7.1},
		{"20% minority, 20% error", 0.20, 0.20, 0.490, 18.4},

		{"balanced, 10% error", 0.50, 0.10, 0.800, 0.0},
		{"45% minority, 10% error", 0.45, 0.10, 0.798, 0.2},
		{"40% minority — the invariant, 10% error", 0.40, 0.10, 0.793, 0.8},
		{"30% minority, 10% error", 0.30, 0.10, 0.771, 3.7},
		{"20% minority, 10% error", 0.20, 0.10, 0.719, 10.1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			judged, human := exactPopulation(tc.p, tc.eps)
			got := judge.Agree(judged, human).Kappa
			if math.Abs(got-tc.wantKappa) > 1e-3 {
				t.Errorf("kappa = %.4f, published table says %.3f.\n"+
					"The derivation the floor rests on has drifted from the doc.",
					got, tc.wantKappa)
			}

			ideal := 1 - 2*tc.eps
			shortfall := (ideal - got) / ideal * 100
			if math.Abs(shortfall-tc.wantShortfall) > 0.1 {
				t.Errorf("shortfall against 1 - 2*epsilon = %.1f%%, published table says %.1f%%",
					shortfall, tc.wantShortfall)
			}
		})
	}
}

// TestTheWorstCaseShortfallAtTheInvariant pins the number the first draft
// asserted without showing.
//
// The 1.6% figure above is the shortfall NEAR THE FLOOR, which is where the
// decision is made. Across the whole kappa range it widens, and the widest
// point is as kappa approaches zero. That worst case is what "about 4%" was
// gesturing at, and it is bounded here.
func TestTheWorstCaseShortfallAtTheInvariant(t *testing.T) {
	t.Parallel()

	worst := 0.0
	var worstEps float64
	for eps := 0.01; eps < 0.50; eps += 0.01 {
		judged, human := exactPopulation(judge.MinMinorityShare, eps)
		// KappaOver, not Agree: the sweep runs to epsilon = 0.49, where the
		// ideal kappa is 0.02 and the four-decimal quantum Agree rounds its
		// REPORTED statistic to is a 0.5-point relative error on its own. This
		// test pins the derivation the floor rests on — a statement about the
		// quantity, not about how it is printed — so it reads the unrounded
		// statistic. The table above stays on Agree, where the tolerances are
		// three orders of magnitude wider than the quantum.
		got := judge.KappaOver(judged, human, allIndices(len(judged)))
		ideal := 1 - 2*eps
		if shortfall := (ideal - got) / ideal * 100; shortfall > worst {
			worst, worstEps = shortfall, eps
		}
	}
	t.Logf("worst shortfall at the 40%% invariant: %.1f%% at epsilon = %.2f", worst, worstEps)
	if worst > 4.0 {
		t.Errorf("the identity is off by %.1f%% at the balance invariant (epsilon = %.2f); "+
			"the published bound is 4%%", worst, worstEps)
	}
}

// allIndices is the identity resample: every unit, once.
func allIndices(n int) []int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	return idx
}

// exactPopulation builds a confusion matrix with EXACTLY the stated minority
// share and symmetric error rate, at a size large enough that rounding does
// not move the third decimal.
//
// Exact rather than sampled: this pins a derivation, and a sampled version
// would pin a draw.
func exactPopulation(p, eps float64) (judged, human []bool) {
	const n = 100000
	positives := int(math.Round(p * n))
	negatives := n - positives
	flipPos := int(math.Round(eps * float64(positives)))
	flipNeg := int(math.Round(eps * float64(negatives)))

	judged = make([]bool, 0, n)
	human = make([]bool, 0, n)
	for i := range positives {
		human = append(human, true)
		judged = append(judged, i >= flipPos)
	}
	for i := range negatives {
		human = append(human, false)
		judged = append(judged, i < flipNeg)
	}
	return judged, human
}
