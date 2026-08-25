package core

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/knograph/kno/core/errs"
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

// TestSaturatingAddMatchesTheGuardsClamp.
//
// The accumulated billed figure feeds the STORE, and Guard.Settle's clamp
// feeds the guard. If the two disagree on the same input, the store and the
// guard disagree about money — and the store is the one that outlives the
// process, because Guard.Restore reads it on resume.
func TestSaturatingAddMatchesTheGuardsClamp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		total, add int64
		want       int64
	}{
		{"an ordinary charge", 40_000, 10_000, 50_000},
		{"zero adds nothing", 40_000, 0, 40_000},
		{"a negative charge is refused, not subtracted", 40_000, -10_000, 40_000},
		{"saturates rather than wrapping", math.MaxInt64 - 5, 100, math.MaxInt64},
		{"the exact boundary is not saturated", math.MaxInt64 - 100, 100, math.MaxInt64},
		{"from zero", 0, 25, 25},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := saturatingAdd(tt.total, tt.add)
			if got != tt.want {
				t.Errorf("saturatingAdd(%d, %d) = %d, want %d", tt.total, tt.add, got, tt.want)
			}
			if got < 0 {
				t.Errorf("saturatingAdd(%d, %d) went negative; a negative total "+
					"clears every cap comparison at once", tt.total, tt.add)
			}
		})
	}
}

// TestBilledCostOfRefusesWhatItCannotTrust.
//
// An adapter reports what it was charged through an anonymous interface, so
// core never imports it. A negative would CREDIT the run's cap, and "the
// provider said nothing" is not "the provider said zero" — the absence of a
// charge is not a charge.
func TestBilledCostOfRefusesWhatItCannotTrust(t *testing.T) {
	t.Parallel()

	plain := errors.New("no charge reported")

	tests := []struct {
		name string
		err  error
		want int64
	}{
		{"an error reporting nothing", plain, 0},
		{"nil", nil, 0},
		{"a real charge", billedErr{plain, 40_000}, 40_000},
		{"a negative charge is refused", billedErr{plain, -40_000}, 0},
		{"a zero charge is not a charge", billedErr{plain, 0}, 0},
		{"wrapped, so errors.As still finds it", fmt.Errorf("outer: %w", billedErr{plain, 7}), 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := billedCostOf(tt.err); got != tt.want {
				t.Errorf("billedCostOf = %d, want %d", got, tt.want)
			}
		})
	}
}

type billedErr struct {
	error
	micros int64
}

func (b billedErr) BilledCostUSDMicros() int64 { return b.micros }
func (b billedErr) Unwrap() error              { return b.error }

// TestSettledSpendReportsWhatTheGuardSettled.
//
// One function, used by every sink branch. Three branches each re-deriving a
// Case's cost is what let the success-after-retry path lose an earlier
// attempt's charge — $0.25 settled against $0.05 persisted.
func TestSettledSpendReportsWhatTheGuardSettled(t *testing.T) {
	t.Parallel()

	resp := &knov1.Response{PromptTokens: 100, CompletionTokens: 40, CostUsdMicros: 999}

	tests := []struct {
		name string
		in   *caseOutcome
		want budget.Spend
	}{
		{
			// Nothing reached a provider: the executor recovered a panic and
			// the reservation was RELEASED rather than settled. Reporting one
			// call would over-record against the call cap for work the guard
			// gave back — and this value now decides whether an orphan-spend
			// row is written at all, so a phantom call would write one.
			name: "a nil outcome settled nothing",
			in:   nil,
			want: budget.Spend{},
		},
		{
			name: "cost comes from what was settled, NOT from the Response",
			in: &caseOutcome{
				Attempts: 2, SettledCalls: 2, BilledUSDMicros: 50_000, Response: resp,
			},
			want: budget.Spend{Calls: 2, CostUSDMicros: 50_000, Tokens: 140},
		},
		{
			// The retry-exhausted path: every attempt failed, so there is no
			// Response to take tokens from, but the charges were real.
			name: "no Response still carries the charge",
			in:   &caseOutcome{Attempts: 3, SettledCalls: 3, BilledUSDMicros: 120_000},
			want: budget.Spend{Calls: 3, CostUSDMicros: 120_000},
		},
		{
			// The distinction this field exists for: two attempts, one of
			// which the guard refused before settling anything. Persisting the
			// attempt count would over-report against the call cap.
			name: "a refused attempt is not a settled call",
			in:   &caseOutcome{Attempts: 2, SettledCalls: 1, BilledUSDMicros: 40_000},
			want: budget.Spend{Calls: 1, CostUSDMicros: 40_000},
		},
		{
			// Refused before the first call: nothing settled, nothing to
			// persist. sinkFunc's write predicate skips this outcome entirely.
			name: "nothing settled reports nothing",
			in:   &caseOutcome{Attempts: 1, SettledCalls: 0},
			want: budget.Spend{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := settledSpend(tt.in); got != tt.want {
				t.Errorf("settledSpend = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestRetryReasonOfNamesWhatTheProtoDefines.
//
// The reason reaches the TUI and the API. Mapping an unclassifiable error onto
// whichever neighbouring value looks closest would put a confident wrong label
// on the stream, and the proto says an emitter that cannot classify must not
// retry at all.
func TestRetryReasonOfNamesWhatTheProtoDefines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want knov1.RetryReason
	}{
		{
			"a rate limit",
			errs.ErrRateLimited.Wrap(errors.New("slow down")),
			knov1.RetryReason_RETRY_REASON_RATE_LIMITED,
		},
		{
			"a transient transport failure",
			errs.ErrTransportTransient.Wrap(errors.New("connection reset")),
			knov1.RetryReason_RETRY_REASON_TRANSPORT_TRANSIENT,
		},
		{
			// Unreachable through invokeWithRetry, which only emits after
			// retryable() has admitted one of the two above. Pinned so the arm
			// stays honest if a third retryable sentinel is added and nobody
			// grows the enum.
			"anything retryable() would not admit",
			errors.New("a plain failure"),
			knov1.RetryReason_RETRY_REASON_UNSPECIFIED,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := retryReasonOf(tt.err); got != tt.want {
				t.Errorf("retryReasonOf = %v, want %v", got, tt.want)
			}
			// Whatever it returns, retryable() and the reason must agree about
			// whether this is a retry at all.
			if retryable(tt.err) && retryReasonOf(tt.err) == knov1.RetryReason_RETRY_REASON_UNSPECIFIED {
				t.Error("retryable() admits this error but no reason names it, so a " +
					"RetryAttempted would carry UNSPECIFIED — which the proto says " +
					"means the emitter must not retry")
			}
		})
	}
}
