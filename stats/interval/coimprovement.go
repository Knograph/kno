package interval

import "math"

// JChance is the co-improvement Jaccard two UNRELATED Assets with the given
// observed improvement counts would produce by coincidence, over a shared
// slice of n Cases.
//
// For a = |I_A|, b = |I_B| over n = |C|, independence gives
// E|I_A n I_B| = ab/n, hence
//
//	J_chance = (ab/n) / (a + b - ab/n)
//
// the redundancy-detection plan's Condition 2 floor: the co-improvement
// Jaccard must clear this before co-location counts as evidence rather than
// shared prevalence. This is the same correction judge-calibrate's kappa
// makes for the same reason — a raw agreement number is uninterpretable until
// chance agreement is subtracted.
//
// Two degenerate cases are exactly the two the plan names, and both fall out
// of the formula rather than needing a special case:
//
//   - a = b = 0 (I_A u I_B empty, neither Asset improved anything on C): the
//     denominator a + b - ab/n is 0, so this returns NaN. A caller must read
//     NaN as "undefined", never as zero — the plan's verdict here is UNKNOWN,
//     not "beyond chance".
//   - a = b = n (both Assets improve every Case in C): ab/n = n, so
//     J_chance = n / (n + n - n) = 1. No interval can exceed 1, so the
//     verdict is UNKNOWN — co-location carries no information when
//     everything is co-located.
//
// Returns NaN for an invalid input (n <= 0, a or b negative, a or b > n)
// rather than a number that would silently compare as a real floor.
func JChance(a, b, n int) float64 {
	if n <= 0 || a < 0 || b < 0 || a > n || b > n {
		return math.NaN()
	}
	fa, fb, fn := float64(a), float64(b), float64(n)
	expectedInter := fa * fb / fn
	denom := fa + fb - expectedInter
	if denom <= 0 {
		return math.NaN()
	}
	return expectedInter / denom
}
