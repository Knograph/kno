package pricing_test

import (
	"math"
	"testing"

	"github.com/knograph/kno/adapters/agent/pricing"
)

// TestUnknownBaseModelIsUnpricedNotFree pins that the shipped training and
// serve tables are EMPTY: no vendor number in this package has been
// confirmed well enough to authorize a $3-8, irreversible-at-submission
// spend against it, so every model — not just an unknown one — must refuse
// rather than silently price at zero.
func TestUnknownBaseModelIsUnpricedNotFree(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ scheme, model string }{
		{"together", "meta-llama/Llama-3-8b"},
		{"together", "anything"},
		{"", ""},
	} {
		if _, ok := pricing.LookupTrainPrice(tc.scheme, tc.model); ok {
			t.Errorf("LookupTrainPrice(%s:%s) reported a price; the shipped table must be empty", tc.scheme, tc.model)
		}
		if _, ok := pricing.LookupServePrice(tc.scheme, tc.model); ok {
			t.Errorf("LookupServePrice(%s:%s) reported a price; the shipped table must be empty", tc.scheme, tc.model)
		}
	}
}

// TestEstimateTrainAppliesHeadroomAfterEpochsBeforeFloor pins the arithmetic
// order Step 2(a) requires: rate * tokens * epochs, THEN the headroom
// multiplier, THEN the floor — flooring before headroom would let the
// multiplier erase a floor meant to be a minimum.
func TestEstimateTrainAppliesHeadroomAfterEpochsBeforeFloor(t *testing.T) {
	t.Parallel()

	t.Run("no floor: rate times tokens times epochs times headroom", func(t *testing.T) {
		t.Parallel()
		price := pricing.TrainPrice{PerMTokUSDMicros: 1_000_000} // $1/MTok
		got := pricing.EstimateTrain(price, 2_000_000, 3)        // 2M tokens, 3 epochs
		// base = 1 * 2 * 3 = $6.00; headroom 120% = $7.20.
		want := int64(7_200_000)
		if got != want {
			t.Errorf("EstimateTrain = %d, want %d", got, want)
		}
	})

	t.Run("floor binds after headroom, not before", func(t *testing.T) {
		t.Parallel()
		price := pricing.TrainPrice{PerMTokUSDMicros: 1_000, FloorUSDMicros: 5_000_000} // tiny rate, $5 floor
		got := pricing.EstimateTrain(price, 1_000, 1)
		if got != 5_000_000 {
			t.Errorf("EstimateTrain = %d, want the floor 5000000", got)
		}
	})

	t.Run("zero or negative epochs default to one epoch", func(t *testing.T) {
		t.Parallel()
		price := pricing.TrainPrice{PerMTokUSDMicros: 1_000_000}
		zero := pricing.EstimateTrain(price, 1_000_000, 0)
		one := pricing.EstimateTrain(price, 1_000_000, 1)
		if zero != one {
			t.Errorf("epochs=0 gave %d, epochs=1 gave %d; want equal", zero, one)
		}
	})
}

// TestEstimateServeCapMultipliesAllThreeTerms pins that the cap-bounded
// hosting quote is minutes * replicas * rate, and that a zero cap or zero
// replica count reports zero rather than an unbounded or negative figure.
func TestEstimateServeCapMultipliesAllThreeTerms(t *testing.T) {
	t.Parallel()

	price := pricing.ServePrice{PerMinuteUSDMicros: 100_000} // $0.10/replica/minute
	got := pricing.EstimateServeCap(price, 30, 1)            // 30 minutes, 1 replica
	want := int64(3_000_000)                                 // $3.00
	if got != want {
		t.Errorf("EstimateServeCap = %d, want %d", got, want)
	}

	if got := pricing.EstimateServeCap(price, 0, 1); got != 0 {
		t.Errorf("zero minutes must report zero, got %d", got)
	}
	if got := pricing.EstimateServeCap(price, 30, 0); got != 0 {
		t.Errorf("zero replicas must report zero, got %d", got)
	}
}

// TestPriceArithmeticSaturatesRatherThanWraps guards the overflow direction
// every other money helper in this package guards: a wrapped product can
// land small and positive, which authorizes spend a human never quoted.
func TestPriceArithmeticSaturatesRatherThanWraps(t *testing.T) {
	t.Parallel()

	huge := pricing.TrainPrice{PerMTokUSDMicros: math.MaxInt64}
	got := pricing.EstimateTrain(huge, math.MaxInt64, math.MaxInt32)
	if got <= 0 {
		t.Errorf("EstimateTrain overflowed to a non-positive number: %d", got)
	}
	if got != math.MaxInt64 {
		t.Errorf("EstimateTrain = %d, want saturation at MaxInt64", got)
	}

	hugeServe := pricing.ServePrice{PerMinuteUSDMicros: math.MaxInt64}
	gotServe := pricing.EstimateServeCap(hugeServe, math.MaxInt32, math.MaxInt32)
	if gotServe != math.MaxInt64 {
		t.Errorf("EstimateServeCap = %d, want saturation at MaxInt64", gotServe)
	}
}
