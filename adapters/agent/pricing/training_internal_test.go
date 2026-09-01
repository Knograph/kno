package pricing

import "testing"

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
