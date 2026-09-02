package pricing

import (
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// seedTrainTable temporarily seeds trainTable/serveTable with one row each,
// restoring the shipped (empty) tables on cleanup. The shipped tables ship
// empty deliberately — see training.go's doc — so the found-a-row branches
// of LookupTrainPrice/LookupServePrice are otherwise unreachable from any
// external test.
func seedTrainTable(t *testing.T, scheme, model string, tp TrainPrice, sp ServePrice) {
	t.Helper()
	prevTrain, hadTrain := trainTable[scheme]
	prevServe, hadServe := serveTable[scheme]
	t.Cleanup(func() {
		if hadTrain {
			trainTable[scheme] = prevTrain
		} else {
			delete(trainTable, scheme)
		}
		if hadServe {
			serveTable[scheme] = prevServe
		} else {
			delete(serveTable, scheme)
		}
	})
	trainTable[scheme] = map[string]TrainPrice{model: tp}
	serveTable[scheme] = map[string]ServePrice{model: sp}
}

// TestLookupTrainPriceExactAndPrefixMatch exercises the two branches
// LookupTrainPrice's doc claims: an exact row, and a version-suffixed
// variant inheriting the base row via the same longestPrefix/isVersionSuffix
// machinery Lookup (table.go) uses for the inference table.
func TestLookupTrainPriceExactAndPrefixMatch(t *testing.T) {
	seedTrainTable(t, "together", "meta-llama/Llama-3-8b",
		TrainPrice{PerMTokUSDMicros: 1_500_000, FloorUSDMicros: 500_000},
		ServePrice{PerMinuteUSDMicros: 100_000})

	got, ok := LookupTrainPrice("together", "meta-llama/Llama-3-8b")
	if !ok || got.PerMTokUSDMicros != 1_500_000 || got.FloorUSDMicros != 500_000 {
		t.Errorf("exact match: got %+v, ok=%v", got, ok)
	}

	// A version suffix inherits the base row, matching Lookup's own rule.
	got, ok = LookupTrainPrice("together", "meta-llama/Llama-3-8b-20260101")
	if !ok || got.PerMTokUSDMicros != 1_500_000 {
		t.Errorf("version-suffixed match: got %+v, ok=%v", got, ok)
	}

	// A variant word (not a version) does not inherit.
	if _, ok := LookupTrainPrice("together", "meta-llama/Llama-3-8b-fast"); ok {
		t.Error("a variant suffix must not inherit the base row")
	}

	if _, ok := LookupTrainPrice("together", "unknown-model"); ok {
		t.Error("an unrelated model must not match")
	}
}

// TestLookupServePriceExactAndPrefixMatch is LookupTrainPrice's test,
// mirrored for the hosting dimension.
func TestLookupServePriceExactAndPrefixMatch(t *testing.T) {
	seedTrainTable(t, "together", "meta-llama/Llama-3-8b",
		TrainPrice{PerMTokUSDMicros: 1_500_000},
		ServePrice{PerMinuteUSDMicros: 100_000})

	got, ok := LookupServePrice("together", "meta-llama/Llama-3-8b")
	if !ok || got.PerMinuteUSDMicros != 100_000 {
		t.Errorf("exact match: got %+v, ok=%v", got, ok)
	}

	got, ok = LookupServePrice("together", "meta-llama/Llama-3-8b@20260101")
	if !ok || got.PerMinuteUSDMicros != 100_000 {
		t.Errorf("version-suffixed match: got %+v, ok=%v", got, ok)
	}

	if _, ok := LookupServePrice("openai", "meta-llama/Llama-3-8b"); ok {
		t.Error("a different scheme with no table must not match")
	}
}

// seedFineTunedTable temporarily seeds fineTunedTable with one row,
// restoring the shipped (empty) table on cleanup — the fineTunedTable
// analogue of seedTrainTable above.
func seedFineTunedTable(t *testing.T, scheme, model string, p *knov1.Price) {
	t.Helper()
	prev, had := fineTunedTable[scheme]
	t.Cleanup(func() {
		if had {
			fineTunedTable[scheme] = prev
		} else {
			delete(fineTunedTable, scheme)
		}
	})
	fineTunedTable[scheme] = map[string]*knov1.Price{model: p}
}

// TestLookupFineTunedPriceExactAndPrefixMatch mirrors
// TestLookupTrainPriceExactAndPrefixMatch for the third bridge pricing
// dimension: an exact row, a version-suffixed variant inheriting it, a
// variant word that does NOT inherit, and an absent row reporting false
// rather than a fabricated price.
func TestLookupFineTunedPriceExactAndPrefixMatch(t *testing.T) {
	price := &knov1.Price{
		InputPerMtokUsdMicros:  int64ptr(4_000_000),
		OutputPerMtokUsdMicros: int64ptr(20_000_000),
	}
	seedFineTunedTable(t, "openai", "gpt-5.6-sol", price)

	got, ok := LookupFineTunedPrice("openai", "gpt-5.6-sol")
	if !ok || got.GetInputPerMtokUsdMicros() != 4_000_000 || got.GetOutputPerMtokUsdMicros() != 20_000_000 {
		t.Errorf("exact match: got %+v, ok=%v", got, ok)
	}

	// A version suffix inherits the base row.
	got, ok = LookupFineTunedPrice("openai", "gpt-5.6-sol-20260101")
	if !ok || got.GetInputPerMtokUsdMicros() != 4_000_000 {
		t.Errorf("version-suffixed match: got %+v, ok=%v", got, ok)
	}

	// A variant word (not a version) does not inherit.
	if _, ok := LookupFineTunedPrice("openai", "gpt-5.6-sol-fast"); ok {
		t.Error("a variant suffix must not inherit the base row")
	}

	if _, ok := LookupFineTunedPrice("openai", "unknown-model"); ok {
		t.Error("an unrelated model must not match")
	}
	if _, ok := LookupFineTunedPrice("together", "gpt-5.6-sol"); ok {
		t.Error("a different scheme with no table must not match")
	}
}

// TestLookupFineTunedPriceReturnsACopy pins the same data-race-prevention
// contract Lookup (table.go) documents for the base inference table: a
// caller mutating the returned Price must not corrupt the table for every
// other caller.
func TestLookupFineTunedPriceReturnsACopy(t *testing.T) {
	seedFineTunedTable(t, "openai", "gpt-5.6-sol", &knov1.Price{
		InputPerMtokUsdMicros: int64ptr(4_000_000),
	})

	got, ok := LookupFineTunedPrice("openai", "gpt-5.6-sol")
	if !ok {
		t.Fatal("want a row")
	}
	*got.InputPerMtokUsdMicros = 1 // mutate the caller's copy

	again, _ := LookupFineTunedPrice("openai", "gpt-5.6-sol")
	if again.GetInputPerMtokUsdMicros() != 4_000_000 {
		t.Errorf("table was mutated through a returned pointer: got %d, want 4000000",
			again.GetInputPerMtokUsdMicros())
	}
}

func int64ptr(v int64) *int64 { return &v }

// TestSaturatingMulNonPositiveOperands covers the early-return branch
// EstimateTrain/EstimateServeCap's own zero-guards otherwise keep this
// helper from ever seeing.
func TestSaturatingMulNonPositiveOperands(t *testing.T) {
	tests := []struct{ a, b int64 }{
		{0, 5}, {5, 0}, {-1, 5}, {5, -1}, {0, 0},
	}
	for _, tc := range tests {
		if got := saturatingMul(tc.a, tc.b); got != 0 {
			t.Errorf("saturatingMul(%d, %d) = %d, want 0", tc.a, tc.b, got)
		}
	}
}
