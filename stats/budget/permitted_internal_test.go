package budget

import "testing"

// TestPermittedBoundsAQuoteToWhatTheGuardWillAuthorize.
//
// Unit-tested here rather than through a run, because the case that matters
// most is unreachable from core: an exhausted cost cap is refused by
// checkFeasible one line before the quote is built. That ordering is the only
// thing keeping the bug latent, and it lives in a different package.
func TestPermittedBoundsAQuoteToWhatTheGuardWillAuthorize(t *testing.T) {
	t.Parallel()

	// 200 calls at $0.05 = $10.00 of intent.
	intent := Estimate{Calls: 200, CostUSDMicros: 10_000_000}

	tests := []struct {
		name   string
		limits Limits
		rem    Remaining
		want   Estimate
	}{
		{
			name:   "no cap bounds nothing",
			limits: Limits{},
			rem:    Remaining{Unlimited: true},
			want:   intent,
		},
		{
			name:   "a cost cap bounds the dollars",
			limits: Limits{MaxCostUSDMicros: 5_000_000},
			rem:    Remaining{CostUSDMicros: 5_000_000},
			want:   Estimate{Calls: 200, CostUSDMicros: 5_000_000},
		},
		{
			// The resume case. Headroom, not the cap.
			name:   "a partly spent cost cap bounds to the headroom",
			limits: Limits{MaxCostUSDMicros: 5_000_000},
			rem:    Remaining{CostUSDMicros: 100_000},
			want:   Estimate{Calls: 200, CostUSDMicros: 100_000},
		},
		{
			// Unreachable from core today, and the reason the gate is on the
			// LIMIT rather than on the headroom. Remaining reports zero both
			// for "no cap" and for "cap exhausted"; skipping the bound is right
			// for the first and restores the full $10.00 for the second.
			name:   "an exhausted cost cap bounds to nothing, not to everything",
			limits: Limits{MaxCostUSDMicros: 5_000_000},
			rem:    Remaining{CostUSDMicros: 0},
			want:   Estimate{Calls: 200, CostUSDMicros: 0},
		},
		{
			// A run stopped by its call cap is just as bounded as one stopped
			// by its dollar cap.
			name:   "a call cap bounds the dollars too",
			limits: Limits{MaxLLMCalls: 10},
			rem:    Remaining{LLMCalls: 10},
			want:   Estimate{Calls: 10, CostUSDMicros: 500_000},
		},
		{
			name:   "a call cap wider than the run bounds nothing",
			limits: Limits{MaxLLMCalls: 1_000},
			rem:    Remaining{LLMCalls: 1_000},
			want:   intent,
		},
		{
			// Calls first, then cost: bounding calls reduces the cost, and the
			// cost bound then applies to the already-reduced figure.
			name:   "both caps apply, the tighter one wins",
			limits: Limits{MaxCostUSDMicros: 5_000_000, MaxLLMCalls: 10},
			rem:    Remaining{CostUSDMicros: 5_000_000, LLMCalls: 10},
			want:   Estimate{Calls: 10, CostUSDMicros: 500_000},
		},
		{
			// Restore is additive and unvalidated, so a negative settled spend
			// read back from the store can make Remaining exceed the cap. The
			// quote must never sit above a limit the guard still enforces.
			name:   "headroom above the cap does not lift the quote above it",
			limits: Limits{MaxCostUSDMicros: 5_000_000},
			rem:    Remaining{CostUSDMicros: 9_000_000},
			want:   Estimate{Calls: 200, CostUSDMicros: 5_000_000},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := permitted(intent, tt.limits, tt.rem)
			if got.Calls != tt.want.Calls || got.CostUSDMicros != tt.want.CostUSDMicros {
				t.Errorf("permitted = {Calls: %d, Cost: %d}, want {Calls: %d, Cost: %d}",
					got.Calls, got.CostUSDMicros, tt.want.Calls, tt.want.CostUSDMicros)
			}
		})
	}
}
