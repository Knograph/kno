package core

import (
	"math"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
)

// TestSaturatingMulDoesNotWrap covers the arithmetic behind a consent path.
//
// A wrapped product goes negative. Negative sails past the cap clamp and under
// the confirmation threshold, so PreConfirm returns true without asking and the
// run proceeds on consent nobody gave. That is prime directive 4 defeated by
// two's complement, so the overflow branch is tested rather than assumed.
func TestSaturatingMulDoesNotWrap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b int64
		want int64
	}{
		{"ordinary product", 1_000, 25, 25_000},
		{"zero calls costs nothing", 0, 500, 0},
		{"zero cost over many calls is still zero", 500, 0, 0},
		{"a negative estimate cannot credit the run", -1, 500, 0},
		{"a negative count cannot credit the run", 500, -1, 0},
		{"both negative does not multiply to a positive", -5, -5, 0},
		{"overflow saturates rather than wrapping", math.MaxInt64, 2, math.MaxInt64},
		{"overflow saturates on the other operand too", 2, math.MaxInt64, math.MaxInt64},
		{"the largest non-overflowing product is exact", math.MaxInt64, 1, math.MaxInt64},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := saturatingMul(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("saturatingMul(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
			if got < 0 {
				t.Errorf("saturatingMul(%d, %d) = %d; a negative product clears both the "+
					"cap clamp and the confirmation threshold, spending without consent",
					tt.a, tt.b, got)
			}
		})
	}
}

// TestAttemptsOfFloorsAtOneCall.
//
// The count feeds the settled spend. Reporting zero calls for work that reached
// the provider understates what the run owes, and the budget guard settles
// against that number.
func TestAttemptsOfFloorsAtOneCall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *caseOutcome
		want int64
	}{
		{"no outcome is still one call's exposure", nil, 1},
		{"an unset attempt count floors at one", &caseOutcome{}, 1},
		{"a negative attempt count floors at one", &caseOutcome{Attempts: -3}, 1},
		{"one attempt is one call", &caseOutcome{Attempts: 1}, 1},
		{"retries count", &caseOutcome{Attempts: 4}, 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := attemptsOf(tt.in); got != tt.want {
				t.Errorf("attemptsOf(%+v) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// TestSpendOfNCountsEveryAttempt.
//
// A Case that took four attempts spent four calls against the cap. Settling one
// leaves three unaccounted, and the guard's call budget drifts by the retry
// rate — which is exactly when the run is already in trouble.
func TestSpendOfNCountsEveryAttempt(t *testing.T) {
	t.Parallel()

	resp := &knov1.Response{
		CostUsdMicros:    2_500,
		PromptTokens:     100,
		CompletionTokens: 40,
	}

	tests := []struct {
		name string
		n    int64
		want budget.Spend
	}{
		{"a single call", 1, budget.Spend{Calls: 1, CostUSDMicros: 2_500, Tokens: 140}},
		{"four attempts settle four calls", 4, budget.Spend{Calls: 4, CostUSDMicros: 2_500, Tokens: 140}},
		{"zero floors to one", 0, budget.Spend{Calls: 1, CostUSDMicros: 2_500, Tokens: 140}},
		{"negative floors to one", -2, budget.Spend{Calls: 1, CostUSDMicros: 2_500, Tokens: 140}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := spendOfN(resp, tt.n); got != tt.want {
				t.Errorf("spendOfN(resp, %d) = %+v, want %+v", tt.n, got, tt.want)
			}
		})
	}

	t.Run("a nil response spends the calls but no money", func(t *testing.T) {
		t.Parallel()
		want := budget.Spend{Calls: 2}
		if got := spendOfN(nil, 2); got != want {
			t.Errorf("spendOfN(nil, 2) = %+v, want %+v", got, want)
		}
	})
}

// TestMeanRefusesAValueNothingCanUse.
//
// NaN propagates through SUM in SQLite exactly as it does in Go, so a single
// Goal returning NaN does not spoil one run — it is written to score_value and
// every later resume of that run reads it back and reports NaN forever. It
// renders as "NaN" and serializes to invalid JSON. Refusing costs one branch.
func TestMeanRefusesAValueNothingCanUse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		agg  *aggregator
		want *float64
	}{
		{"nothing scored has no mean", &aggregator{}, nil},
		{
			"an unrecoverable score refuses rather than averaging survivors",
			&aggregator{scored: 2, sum: 2, priorCounted: 2, priorSum: 2, unrecoverable: 3},
			nil,
		},
		{"NaN is not a mean", &aggregator{scored: 2, sum: math.NaN()}, nil},
		{"positive infinity is not a mean", &aggregator{scored: 2, sum: math.Inf(1)}, nil},
		{"negative infinity is not a mean", &aggregator{scored: 2, sum: math.Inf(-1)}, nil},
		{"an ordinary mean survives", &aggregator{scored: 4, sum: 3}, ptr(0.75)},
		{
			"the prior sum divides by the prior COUNT, not the prior scored",
			// 6 scored earlier, but only 4 of them carry a number, and the sum
			// is over those 4. Dividing by 6 would understate the mean.
			&aggregator{scored: 0, sum: 0, priorScored: 6, priorCounted: 4, priorSum: 2},
			ptr(0.5),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.agg.mean()
			switch {
			case tt.want == nil && got != nil:
				t.Errorf("mean() = %v, want nil", *got)
			case tt.want != nil && got == nil:
				t.Errorf("mean() = nil, want %v", *tt.want)
			case tt.want != nil && *got != *tt.want:
				t.Errorf("mean() = %v, want %v", *got, *tt.want)
			}
		})
	}
}

func ptr(f float64) *float64 { return &f }
