package value

import "math/rand/v2"

// This file is the repayment of docs/debt.md#75: the routing shuffle and the
// label draw previously called rand.Rand.Shuffle, whose consumption pattern
// and bounded-draw loop carry no compatibility promise across Go releases —
// and the Run's recorded seed exists to be re-derived years later. PCG output
// IS specified; these nine lines are the arithmetic that turns the specified
// stream into a bounded draw and a Fisher-Yates permutation, and by owning
// them the promise stops depending on anything unspecified.
//
// The stream changed once, when this file landed: seeds recorded by earlier
// releases re-derive only under the pre-v0.1.0 binary. From the release that
// ships this file forward, the stream is specified and stable, and Route's
// godoc states the boundary.

// uniformBelow returns a value uniform in [0, n) from the raw PCG stream.
// math/rand/v2's Uint64N is deliberately not used: its rejection loop is
// undocumented implementation, which is the exact dependency #75 removes.
//
// The bound: values below 2^64 mod n are rejected; the accepted range is an
// exact multiple of n, so the modulo maps a uniform stream to a uniform
// bounded draw with no bias.
func uniformBelow(rng *rand.Rand, n uint64) uint64 {
	if n <= 1 {
		return 0
	}
	threshold := -n % n // 2^64 mod n, in uint64 wraparound arithmetic
	for {
		v := rng.Uint64()
		if v >= threshold {
			return v % n
		}
	}
}

// shuffle is Fisher-Yates over the inlined bounded draw. swap receives the
// two indices to exchange; it is a closure so the same loop serves slice
// permutation at both call sites without touching their element types.
func shuffle(rng *rand.Rand, n int, swap func(i, j int)) {
	for i := n - 1; i > 0; i-- {
		//nolint:gosec // G115: the draw is bounded by i+1 ≤ n, and n arrived
		// as an int, so the value cannot overflow int — the conversion is
		// provably safe (issue #110; docs/debt.md#75 records the stream).
		j := int(uniformBelow(rng, uint64(i+1)))
		swap(i, j)
	}
}
