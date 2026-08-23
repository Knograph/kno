package anthropic_test

import (
	"math"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/knograph/kno/adapters/agent/anthropic"
)

// TestImplausibleReportedUsageIsRefusedRatherThanPriced.
//
// The usage block is provider input, and a broken or hostile intermediary at
// --base-url is a SUPPORTED configuration. Trusting absurd counts saturates the
// cost terms, and the saturated value goes into Guard.Settle, which adds
// unchecked: two such Cases against a $1.00 cap leave spent negative, Remaining
// reporting more than the cap, and the guard authorizing again. Guard.Spent()
// — the number a report shows and the number Guard.Restore re-reads on resume
// — goes negative with it.
//
// The fix is to refuse the block, not to survive it. Asserting only "the cost
// is not negative" would pass against a saturating sum that hands the guard
// MaxInt64, which is the failure this exists to prevent.
func TestImplausibleReportedUsageIsRefusedRatherThanPriced(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"every field at MaxInt64": `"usage":{"input_tokens":9223372036854775807,` +
			`"output_tokens":9223372036854775807,` +
			`"cache_creation_input_tokens":9223372036854775807,` +
			`"cache_read_input_tokens":9223372036854775807}`,

		// Three fields summing past MaxInt64 while each is individually
		// positive: billedInput would wrap to 2^63-3, which is POSITIVE and
		// passes every non-negativity check downstream.
		"three fields that wrap on sum": `"usage":{"input_tokens":9223372036854775807,` +
			`"output_tokens":1,` +
			`"cache_creation_input_tokens":9223372036854775807,` +
			`"cache_read_input_tokens":9223372036854775807}`,

		"just past the plausible bound": `"usage":{"input_tokens":10000001,"output_tokens":5}`,
	}

	for name, usage := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				answer(w, `{"id":"m","model":"claude-sonnet-4-6","stop_reason":"end_turn",`+
					`"content":[{"type":"text","text":"ok"}],`+usage+`}`)
			})
			a := newAgent(t, srv)

			c := aCase("q")
			est, err := a.Estimate(t.Context(), c)
			if err != nil {
				t.Fatalf("Estimate: %v", err)
			}
			resp, err := a.Invoke(t.Context(), c)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}

			if !resp.GetUsageEstimated() {
				t.Error("a usage block beyond any real context window was trusted")
			}
			if got := resp.GetCostUsdMicros(); got != est.CostUSDMicros {
				t.Errorf("cost = %d, want the reservation %d; a refused usage block "+
					"must settle at the estimate rather than at whatever the "+
					"arithmetic produced", got, est.CostUSDMicros)
			}
			if resp.GetCostUsdMicros() < 0 {
				t.Error("a negative settlement raises the guard's remaining headroom, " +
					"so the worse the provider's numbers are the more the run may spend")
			}
		})
	}
}

// TestAPlausibleUsageBlockAtTheBoundStillPrices, so the bound refuses the
// absurd without refusing the merely large.
func TestAPlausibleUsageBlockAtTheBoundStillPrices(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, `{"id":"m","model":"claude-sonnet-4-6","stop_reason":"end_turn",
		            "content":[{"type":"text","text":"ok"}],
		            "usage":{"input_tokens":10000000,"output_tokens":5}}`)
	})
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), aCase("q"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.GetUsageEstimated() {
		t.Errorf("a block exactly at MaxPlausibleTokens (%d) was refused; the bound "+
			"must exclude the absurd, not the merely large", anthropic.MaxPlausibleTokens)
	}
	if resp.GetPromptTokens() != 10_000_000 {
		t.Errorf("prompt_tokens = %d", resp.GetPromptTokens())
	}
}

// TestAddIsTotal.
//
// The precondition "terms are non-negative" was undocumented and untested, and
// the guard written for it — `sum > math.MaxInt64-t` — itself overflows for a
// negative t and returns MaxInt64 for a total of 98. A guard that misfires into
// the exact value it exists to avoid is worse than no guard, because it makes
// an overflow test pass for the wrong reason.
func TestAddIsTotal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		terms []int64
		want  int64
	}{
		{"an ordinary sum", []int64{3000, 750, 1200, 7500}, 12450},
		{"empty", nil, 0},
		{"one saturated term", []int64{math.MaxInt64, 1}, math.MaxInt64},
		{"two saturated terms", []int64{math.MaxInt64, math.MaxInt64}, math.MaxInt64},
		{"exactly MaxInt64", []int64{math.MaxInt64}, math.MaxInt64},

		// The case the old guard got wrong: it computed MaxInt64-(-2), which
		// wraps, so `100 > wrapped` was true and it returned MaxInt64.
		{"a negative term", []int64{100, -2}, 98},
		{"negatives that saturate downward", []int64{math.MinInt64, -1}, math.MinInt64},
		{"a negative cancelling a positive", []int64{math.MaxInt64, math.MinInt64}, -1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := anthropic.Add(tc.terms...); got != tc.want {
				t.Errorf("Add(%v) = %d, want %d", tc.terms, got, tc.want)
			}
		})
	}
}

// TestMicrosRoundsUpAndSaturates.
//
// Rounding up rather than truncating, because shaving a fraction off every
// settlement is a bound that is systematically a little low, and a bound that
// is systematically low eventually is not one.
func TestMicrosRoundsUpAndSaturates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		rate, tokens, want int64
	}{
		{3_000_000, 16, 48},
		{3_000_000, 1, 3},
		{300_000, 1, 1},    // rounds up from 0.3
		{1, 1, 1},          // rounds up from 0.000001
		{0, 1_000, 0},      // an absent rate is not a free one; the caller checks presence
		{3_000_000, 0, 0},  // no tokens, no charge
		{3_000_000, -5, 0}, // refused upstream; must not go negative here
		{math.MaxInt64, 2, math.MaxInt64},
	}

	for _, tc := range tests {
		if got := anthropic.Micros(tc.rate, tc.tokens); got != tc.want {
			t.Errorf("Micros(%d, %d) = %d, want %d", tc.rate, tc.tokens, got, tc.want)
		}
	}
}

// TestSanitizeAlwaysProducesValidUTF8WithinTheBound.
//
// Response.error is a proto3 string, so invalid UTF-8 here is a marshal failure
// downstream. The multi-byte rune is swept across the truncation boundary one
// byte at a time, because a byte-offset cut splits it only at a particular
// alignment and a single fixture length misses it.
func TestSanitizeAlwaysProducesValidUTF8WithinTheBound(t *testing.T) {
	t.Parallel()

	for pad := anthropic.MaxProviderMessage - 8; pad <= anthropic.MaxProviderMessage+8; pad++ {
		if pad < 0 {
			continue
		}
		in := strings.Repeat("a", pad) + "é世\U0001f600 trailing text"
		got := anthropic.Sanitize(in)
		if !utf8.ValidString(got) {
			t.Fatalf("pad=%d produced invalid UTF-8: %q", pad, got)
		}
	}
}

// TestSanitizeStripsControlCharactersAndInvalidBytes.
func TestSanitizeStripsControlCharactersAndInvalidBytes(t *testing.T) {
	t.Parallel()

	got := anthropic.Sanitize("bad\nlevel=fatal\r\ttab\x00nul" + string([]byte{0x80}))
	if strings.ContainsAny(got, "\n\r\t\x00") {
		t.Errorf("a control character survived: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("invalid UTF-8 survived: %q", got)
	}
	if !strings.Contains(got, "level=fatal") {
		t.Errorf("the message text was lost: %q", got)
	}
}
