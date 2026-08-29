package value

import (
	"math/rand/v2"
	"testing"
)

// TestShuffleIsDeterministic pins the one property the Run's recorded seed
// exists for: the same seed produces the same permutation, every time, in
// this process and (per shuffle.go's contract) every future release from
// v0.1.0.
func TestShuffleIsDeterministic(t *testing.T) {
	t.Parallel()
	want := shuffleOf(7, 42)
	for range 3 {
		if got := shuffleOf(7, 42); !equalPerm(got, want) {
			t.Fatalf("seed 42 permuted differently across runs: %v vs %v", got, want)
		}
	}
}

// TestShuffleGoldenPermutation pins the exact stream: PCG + the inlined
// bounded draw. If a future change alters this table, it alters the meaning
// of every seed recorded on a Run — the change is a breaking promise and must
// be deliberate, not an accident of refactoring.
func TestShuffleGoldenPermutation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		n    int
		seed uint64
		want []int
	}{
		{7, 42, []int{4, 1, 5, 0, 3, 2, 6}},
		{7, 1, []int{5, 4, 2, 1, 6, 0, 3}},
		{10, 99, []int{1, 8, 4, 7, 2, 9, 0, 6, 5, 3}},
	}
	for _, tc := range tests {
		got := shuffleOf(tc.n, tc.seed)
		if !equalPerm(got, tc.want) {
			t.Errorf("shuffleOf(%d, %d) = %v, want %v", tc.n, tc.seed, got, tc.want)
		}
	}
}

// TestUniformBelowDistribution is the bias check: a bounded draw whose
// buckets drift is a ranked selection with a silent preference, and a
// preference in the routing draw is a bias in every measurement it feeds.
// The bounds are generous — the check exists to catch the broken-draw class
// of bug, not to re-derive the CLT.
func TestUniformBelowDistribution(t *testing.T) {
	t.Parallel()
	const n, draws = 7, 700000
	rng := rand.New(rand.NewPCG(20260829, 0x9e3779b9))
	counts := make([]int, n)
	for range draws {
		counts[uniformBelow(rng, n)]++
	}
	want := float64(draws) / n
	for i, c := range counts {
		dev := (float64(c) - want) / want
		if dev > 0.03 || dev < -0.03 {
			t.Errorf("bucket %d deviates %.4f%% from uniform (%d draws)", i, dev*100, c)
		}
	}
}

// TestUniformBelowEdges: the degenerate bounds must not panic or loop, and
// must return the only legal value.
func TestUniformBelowEdges(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 1))
	if got := uniformBelow(rng, 0); got != 0 {
		t.Errorf("uniformBelow(n=0) = %d, want 0", got)
	}
	if got := uniformBelow(rng, 1); got != 0 {
		t.Errorf("uniformBelow(n=1) = %d, want 0", got)
	}
}

// TestShuffleEdges: n=0 and n=1 must leave the slice untouched without
// calling swap at all.
func TestShuffleEdges(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 1))
	swaps := 0
	shuffle(rng, 0, func(_, _ int) { swaps++ })
	shuffle(rng, 1, func(_, _ int) { swaps++ })
	if swaps != 0 {
		t.Errorf("shuffle over n<=1 swapped %d times, want 0", swaps)
	}
}

// shuffleOf runs the inlined shuffle over the identity permutation and
// returns the result, so the tests above share one harness.
func shuffleOf(n int, seed uint64) []int {
	perm := make([]int, n)
	for i := range perm {
		perm[i] = i
	}
	rng := rand.New(rand.NewPCG(seed, seed+0x9e3779b9))
	shuffle(rng, n, func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })
	return perm
}

func equalPerm(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
