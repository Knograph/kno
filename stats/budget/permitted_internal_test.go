package budget

import (
	"context"
	"math"
	"testing"
)

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

// TestDeclinedDistinguishesRefusalFromNotYetAsked.
//
// The confirmed flag alone cannot express it: false means both "nobody has
// been asked" and "somebody said no". A caller that conflated them would
// re-prompt after a refusal and, on a yes, authorize spend the user had just
// declined — which is why PreConfirm records the refusal at all.
func TestDeclinedDistinguishesRefusalFromNotYetAsked(t *testing.T) {
	t.Parallel()

	limits := Limits{MaxCostUSDMicros: 10_000_000}
	over := Estimate{Calls: 1, CostUSDMicros: 5_000_000}

	t.Run("not asked yet", func(t *testing.T) {
		t.Parallel()
		g := New(limits, func(context.Context, Estimate, Remaining) (bool, error) {
			return true, nil
		}, 1_000_000)
		if g.Declined() {
			t.Error("Declined before anyone was asked")
		}
	})

	t.Run("asked and agreed", func(t *testing.T) {
		t.Parallel()
		g := New(limits, func(context.Context, Estimate, Remaining) (bool, error) {
			return true, nil
		}, 1_000_000)
		if ok, err := g.PreConfirm(context.Background(), over); !ok || err != nil {
			t.Fatalf("PreConfirm = %v, %v", ok, err)
		}
		if g.Declined() {
			t.Error("Declined after the user agreed")
		}
	})

	t.Run("asked and refused", func(t *testing.T) {
		t.Parallel()
		g := New(limits, func(context.Context, Estimate, Remaining) (bool, error) {
			return false, nil
		}, 1_000_000)
		if ok, _ := g.PreConfirm(context.Background(), over); ok {
			t.Fatal("PreConfirm returned true on a refusal")
		}
		if !g.Declined() {
			t.Error("a refusal was not recorded, so a later prompt could authorize " +
				"spend the user had just declined")
		}
	})

	t.Run("below the threshold, never asked", func(t *testing.T) {
		t.Parallel()
		g := New(limits, func(context.Context, Estimate, Remaining) (bool, error) {
			t.Error("asked about a total below the threshold")
			return false, nil
		}, 1_000_000)
		if ok, err := g.PreConfirm(context.Background(),
			Estimate{Calls: 1, CostUSDMicros: 1}); !ok || err != nil {
			t.Fatalf("PreConfirm = %v, %v", ok, err)
		}
		if g.Declined() {
			t.Error("Declined for a run nobody was asked about")
		}
	})
}

// TestFormatUSDRendersCentsAndSigns.
//
// It appears in refusal messages, which are the last thing a user reads before
// deciding the tool is wrong about their money. A negative is reachable:
// Overshoot's own godoc describes a resume under a lower cap reading as over
// immediately.
func TestFormatUSDRendersCentsAndSigns(t *testing.T) {
	t.Parallel()

	tests := []struct {
		micros int64
		want   string
	}{
		{0, "$0.00"},
		{1, "$0.00"}, // sub-cent truncates rather than rounding up
		{9_999, "$0.00"},
		{10_000, "$0.01"},
		{1_000_000, "$1.00"},
		{5_432_100, "$5.43"},
		{-2_500_000, "-$2.50"},
		{-1, "-$0.00"},
	}

	for _, tt := range tests {
		if got := formatUSD(tt.micros); got != tt.want {
			t.Errorf("formatUSD(%d) = %q, want %q", tt.micros, got, tt.want)
		}
	}
}

// TestSettleClampsWhatAnAdapterReports.
//
// Every caller is a provider adapter reporting what it was charged, and
// Settle's total is the number a report shows AND the number Restore reads
// back on resume. docs/debt.md#48: two MaxInt64 settlements against a $1.00
// cap left spent at -2, Remaining reporting more than the cap, and the guard
// authorizing again.
//
// Clamped here rather than at each call site because this is the choke point.
// Both M2 adapters bound their own usage blocks; every future one would have
// to remember to.
func TestSettleClampsWhatAnAdapterReports(t *testing.T) {
	t.Parallel()

	const costCap = 1_000_000 // $1.00

	tests := []struct {
		name      string
		settles   []int64
		wantSpent int64
	}{
		{"an ordinary settlement", []int64{250_000}, 250_000},
		{
			// The reproduction from #48.
			name:      "two saturated settlements do not wrap negative",
			settles:   []int64{math.MaxInt64, math.MaxInt64},
			wantSpent: math.MaxInt64,
		},
		{
			// A negative would CREDIT the run's cap — a settlement that gives
			// money back is not a settlement.
			name:      "a negative charge is refused, not subtracted",
			settles:   []int64{500_000, -400_000},
			wantSpent: 500_000,
		},
		{"a zero charge adds nothing", []int64{500_000, 0}, 500_000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := New(Limits{MaxCostUSDMicros: costCap}, nil, 0)
			for _, v := range tt.settles {
				r, err := g.Authorize(context.Background(), Estimate{Calls: 1, CostUSDMicros: 1})
				if err != nil {
					// Once the cap binds, further authorization is refused —
					// which is correct, and means the earlier settles stuck.
					break
				}
				r.Settle(Spend{Calls: 1, CostUSDMicros: v})
			}
			if got := g.Spent().CostUSDMicros; got != tt.wantSpent {
				t.Errorf("spent = %d, want %d", got, tt.wantSpent)
			}
			if g.Spent().CostUSDMicros < 0 {
				t.Error("spent went negative; Remaining would then report more than " +
					"the cap and the guard would authorize again")
			}
		})
	}
}
