package interval

import (
	"math"
	"testing"
)

// TestJChance pins the closed-form reference over a table of (a, b, n), and
// is what acceptance criterion 21 (redundancy-detection plan) asks for: a
// grep-checkable, table-driven proof the formula matches its derivation.
func TestJChance(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		a, b, n int
		want    float64
	}{
		{"symmetric halves", 10, 10, 20, 5.0 / 15.0},
		// The plan's own worked example: two Assets each improving 80% of a
		// 20-Case slice have an expected INTERSECTION of 0.8*0.8*20=12.8
		// Cases ("overlap on ~64% of it") purely from shared prevalence; the
		// Jaccard chance level divides that by the expected UNION, not by n.
		{"80pct each over n=20 (the plan's ~64%-overlap example)", 16, 16, 20, 12.8 / (16 + 16 - 12.8)},
		{"asymmetric counts", 4, 12, 20, 2.4 / (4 + 12 - 2.4)},
		{"n=5 floor case", 3, 3, 5, 1.8 / (3 + 3 - 1.8)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := JChance(tc.a, tc.b, tc.n)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("JChance(%d, %d, %d) = %v, want %v", tc.a, tc.b, tc.n, got, tc.want)
			}
		})
	}
}

// TestJChanceEmptyUnionIsUndefined: neither Asset improved anything on the
// shared slice — a = b = 0 — so J itself is undefined, and the chance floor
// must read as undefined too (NaN), never as zero. A caller comparing a real
// J against a zero floor would treat "nothing happened" as "conclusively
// co-located", which is the opposite of what an empty union means.
func TestJChanceEmptyUnionIsUndefined(t *testing.T) {
	t.Parallel()
	got := JChance(0, 0, 20)
	if !math.IsNaN(got) {
		t.Fatalf("JChance(0, 0, 20) = %v, want NaN", got)
	}
}

// TestJChanceEverythingCoImprovedApproachesOne: both Assets improve every
// Case in C, so J_chance -> 1 and no interval can clear it — co-location
// carries no information when everything is co-located, which is exactly
// what makes the redundancy verdict UNKNOWN rather than REDUNDANT in that
// case (the caller's job; this function's job is to make the floor say so).
func TestJChanceEverythingCoImprovedApproachesOne(t *testing.T) {
	t.Parallel()
	for _, n := range []int{5, 20, 100} {
		got := JChance(n, n, n)
		if math.Abs(got-1) > 1e-9 {
			t.Fatalf("JChance(%d, %d, %d) = %v, want 1", n, n, n, got)
		}
	}
}

// TestJChanceInvalidInputsAreNaN: a caller handing over a nonsensical count
// (negative, or larger than the population it is drawn from) gets NaN rather
// than a number that would silently compare as a real floor.
func TestJChanceInvalidInputsAreNaN(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		a, b, n int
	}{
		{"n is zero", 1, 1, 0},
		{"n is negative", 1, 1, -1},
		{"a negative", -1, 1, 5},
		{"b negative", 1, -1, 5},
		{"a exceeds n", 6, 1, 5},
		{"b exceeds n", 1, 6, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := JChance(tc.a, tc.b, tc.n); !math.IsNaN(got) {
				t.Fatalf("JChance(%d, %d, %d) = %v, want NaN", tc.a, tc.b, tc.n, got)
			}
		})
	}
}

// TestJChanceMonotoneInOverlap: holding a and b fixed as a FRACTION of n, a
// smaller shared slice yields a shrinking absolute count but the same
// fraction produces the same chance level — J_chance depends on the observed
// RATES, not the raw counts, which is what makes it comparable across pairs
// measured over different-sized slices.
func TestJChanceMonotoneInOverlap(t *testing.T) {
	t.Parallel()
	// 40% improvement rate for both Assets, at two different slice sizes.
	small := JChance(2, 2, 5)
	large := JChance(20, 20, 50)
	if math.Abs(small-large) > 1e-9 {
		t.Fatalf("JChance(2,2,5)=%v and JChance(20,20,50)=%v should agree (same rate)", small, large)
	}
}
