package pricing_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/agent/pricing"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// TestTableMatchesThePublishedPrices.
//
// Pinned against the provider pages as published on the table's own date. A
// pricing table nobody checks is a table that silently rots, and every number
// here is one the budget guard reserves against.
func TestTableMatchesThePublishedPrices(t *testing.T) {
	t.Parallel()

	// dollars per million tokens, as published.
	tests := []struct {
		scheme, model             string
		input, cachedRead, output float64
		cacheWrite                float64
		hasCacheWrite             bool
	}{
		{scheme: "anthropic", model: "claude-opus-5", input: 5, cachedRead: 0.50, cacheWrite: 6.25, hasCacheWrite: true, output: 25},
		{scheme: "anthropic", model: "claude-sonnet-5", input: 2, cachedRead: 0.20, cacheWrite: 2.50, hasCacheWrite: true, output: 10},
		{scheme: "anthropic", model: "claude-sonnet-4-5", input: 3, cachedRead: 0.30, cacheWrite: 3.75, hasCacheWrite: true, output: 15},
		{scheme: "anthropic", model: "claude-haiku-4-5", input: 1, cachedRead: 0.10, cacheWrite: 1.25, hasCacheWrite: true, output: 5},
		{scheme: "anthropic", model: "claude-fable-5", input: 10, cachedRead: 1, cacheWrite: 12.50, hasCacheWrite: true, output: 50},
		// OpenAI publishes no separate cache-WRITE rate. Unset, not zero.
		{scheme: "openai", model: "gpt-5.6-sol", input: 4, cachedRead: 0.40, output: 20},
		{scheme: "openai", model: "gpt-5.6-terra", input: 2, cachedRead: 0.20, output: 12},
		{scheme: "openai", model: "gpt-5.6-luna", input: 0.20, cachedRead: 0.02, output: 1.20},
	}

	micros := func(usd float64) int64 { return int64(usd*1_000_000 + 0.5) }

	for _, tc := range tests {
		t.Run(tc.scheme+":"+tc.model, func(t *testing.T) {
			t.Parallel()

			p, ok := pricing.Lookup(tc.scheme, tc.model)
			if !ok {
				t.Fatalf("%s:%s is not priced", tc.scheme, tc.model)
			}
			if got, want := p.GetInputPerMtokUsdMicros(), micros(tc.input); got != want {
				t.Errorf("input = %d, want %d ($%.2f/MTok)", got, want, tc.input)
			}
			if got, want := p.GetCachedInputPerMtokUsdMicros(), micros(tc.cachedRead); got != want {
				t.Errorf("cached input = %d, want %d ($%.2f/MTok)", got, want, tc.cachedRead)
			}
			if got, want := p.GetOutputPerMtokUsdMicros(), micros(tc.output); got != want {
				t.Errorf("output = %d, want %d ($%.2f/MTok)", got, want, tc.output)
			}

			// Presence, not value. "This provider does not bill a cache write
			// separately" and "a cache write is free" are different claims, and
			// the settlement path would charge nothing for the second.
			if got := p.CacheWritePerMtokUsdMicros != nil; got != tc.hasCacheWrite {
				t.Errorf("cache-write present = %v, want %v", got, tc.hasCacheWrite)
			}
			if tc.hasCacheWrite {
				if got, want := p.GetCacheWritePerMtokUsdMicros(), micros(tc.cacheWrite); got != want {
					t.Errorf("cache write = %d, want %d ($%.2f/MTok)", got, want, tc.cacheWrite)
				}
			}
		})
	}
}

// TestAnUnknownModelIsUnpricedNotFree.
//
// A zero estimate makes a dollar cap unenforceable — the failure that overshot
// a $0.10 cap in M1. Every path that cannot produce a real number must say so.
func TestAnUnknownModelIsUnpricedNotFree(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ scheme, model string }{
		{"openai", "gpt-9-imaginary"},
		{"anthropic", "claude-nonexistent"},
		{"openrouter", "meta-llama/llama-3.1-8b"}, // a whole scheme with no table
		{"", ""},
	} {
		if _, ok := pricing.Lookup(tc.scheme, tc.model); ok {
			t.Errorf("%s:%s reported a price", tc.scheme, tc.model)
		}
		_, err := pricing.Estimate(tc.scheme, tc.model, "hello", 1000)
		if !errors.Is(err, pricing.ErrUnpriced) {
			t.Errorf("Estimate(%s:%s) = %v, want ErrUnpriced", tc.scheme, tc.model, err)
		}
	}
}

// TestTheEstimateIsPessimisticOnEveryTerm.
//
// It bounds a reservation, and a bound that can be too low is not a bound.
func TestTheEstimateIsPessimisticOnEveryTerm(t *testing.T) {
	t.Parallel()

	const model = "gpt-5.6-terra" // $2/MTok in, $12/MTok out
	input := strings.Repeat("a", 3_000)

	est, err := pricing.Estimate("openai", model, input, 1_000)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.Calls != 1 {
		t.Errorf("calls = %d, want exactly 1", est.Calls)
	}

	// Input is charged at the FRESH rate. Assuming a cache hit would
	// under-reserve exactly when a run repeats similar prompts, which is most
	// of the time.
	cheaper, err := pricing.Estimate("openai", model, input, 1_000)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if cheaper.CostUSDMicros != est.CostUSDMicros {
		t.Errorf("the estimate is not deterministic: %d then %d",
			est.CostUSDMicros, cheaper.CostUSDMicros)
	}

	// A bigger output ceiling costs more, because the ceiling is what the
	// request permits rather than what a typical answer uses.
	bigger, err := pricing.Estimate("openai", model, input, 10_000)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if bigger.CostUSDMicros <= est.CostUSDMicros {
		t.Errorf("raising the output ceiling did not raise the estimate: %d vs %d",
			bigger.CostUSDMicros, est.CostUSDMicros)
	}

	// And longer input costs more.
	longer, err := pricing.Estimate("openai", model, strings.Repeat("a", 30_000), 1_000)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if longer.CostUSDMicros <= est.CostUSDMicros {
		t.Errorf("a 10x longer prompt did not raise the estimate: %d vs %d",
			longer.CostUSDMicros, est.CostUSDMicros)
	}
}

// TestTheDenserTokenizerIsPricedAsDenser.
//
// Anthropic publishes that Claude 4.7 and later produce roughly 30% more tokens
// for the same text. Applying the old ratio under-counts every input by about a
// quarter — and an under-count is the direction that breaks a cap.
func TestTheDenserTokenizerIsPricedAsDenser(t *testing.T) {
	t.Parallel()

	input := strings.Repeat("the quick brown fox ", 200)

	// Same price per token ($3 vs $2 input differ, so compare TOKENS).
	newer, err := pricing.Estimate("anthropic", "claude-sonnet-5", input, 1_000)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	older, err := pricing.Estimate("anthropic", "claude-sonnet-4-5", input, 1_000)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if newer.Tokens <= older.Tokens {
		t.Errorf("the denser tokenizer estimated %d tokens against %d for the "+
			"older one; the same text costs more on 4.7 and later",
			newer.Tokens, older.Tokens)
	}
}

// TestAPinnedModelVersionKeepsItsTokenizer.
//
// Providers append dated suffixes. An exact match would silently fall back to
// the old, cheaper ratio for every pinned version — which is the shape a user
// running in production is most likely to write.
func TestAPinnedModelVersionKeepsItsTokenizer(t *testing.T) {
	t.Parallel()

	p, ok := pricing.Lookup("anthropic", "claude-sonnet-5")
	if !ok {
		t.Fatal("claude-sonnet-5 is not priced")
	}
	input := strings.Repeat("the quick brown fox ", 200)

	base, err := pricing.EstimateWithPrice(p, "claude-sonnet-5", input, 1_000)
	if err != nil {
		t.Fatalf("EstimateWithPrice: %v", err)
	}
	pinned, err := pricing.EstimateWithPrice(p, "claude-sonnet-5-20260514", input, 1_000)
	if err != nil {
		t.Fatalf("EstimateWithPrice: %v", err)
	}
	if pinned.Tokens != base.Tokens {
		t.Errorf("a pinned version estimated %d tokens against %d for the base "+
			"name; the tokenizer did not follow the model", pinned.Tokens, base.Tokens)
	}
}

// TestAnOutputCeilingIsRequired: without one the output term is unbounded, and
// an unbounded term cannot bound anything.
func TestAnOutputCeilingIsRequired(t *testing.T) {
	t.Parallel()

	for _, ceiling := range []int64{0, -1} {
		if _, err := pricing.Estimate("openai", "gpt-5.6-luna", "hi", ceiling); err == nil {
			t.Errorf("an output ceiling of %d was accepted", ceiling)
		}
	}
}

// TestModelsListsWhatIsKnown, so a refusal can name the alternatives rather
// than only reporting that the model is not one of them.
func TestModelsListsWhatIsKnown(t *testing.T) {
	t.Parallel()

	if got := pricing.Models("anthropic"); len(got) == 0 {
		t.Error("no anthropic models are priced")
	}
	if got := pricing.Models("nonexistent"); got != nil {
		t.Errorf("Models(nonexistent) = %v, want nil", got)
	}
}

// TestTokenCountingErrsHighButNotAbsurdly.
//
// Two failure modes, opposite directions, and both matter. Under-counting is
// how a run walks past its cap. Over-counting forfeits headroom: a pessimistic
// reservation holds `concurrency x estimate` of the cap un-spendable, so a
// 4x over-estimate makes a cap four times harder to use.
//
// An earlier version floored the count at the rune count, which for ASCII
// estimated one token per character — about four times reality. It passed every
// test that only checked "bigger input costs more". Sanity-checking a realistic
// prompt is what caught it, so the ratio is pinned here.
func TestTokenCountingErrsHighButNotAbsurdly(t *testing.T) {
	t.Parallel()

	// ~4 characters per token is the published English rule of thumb, so this
	// prompt is roughly 90 tokens.
	const prompt = "How do I get a refund for my order? " // 36 chars
	input := strings.Repeat(prompt, 10)                   // 360 chars, ~90 tokens
	const roughlyReal = 90

	est, err := pricing.Estimate("openai", "gpt-5.6-luna", input, 0+1)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	inputTokens := est.Tokens - 1 // the output ceiling was 1

	if inputTokens < roughlyReal {
		t.Errorf("estimated %d input tokens for text worth roughly %d; "+
			"under-counting is how a run walks past its cap",
			inputTokens, roughlyReal)
	}
	if ratio := float64(inputTokens) / roughlyReal; ratio > 2.5 {
		t.Errorf("estimated %d input tokens for text worth roughly %d (%.1fx); "+
			"a pessimistic reservation holds concurrency x estimate of the cap "+
			"un-spendable, so over-counting makes the cap harder to use",
			inputTokens, roughlyReal, ratio)
	}
}

// TestNonASCIITextIsNotUnderCounted.
//
// CJK runs about one token per character at three UTF-8 bytes each, so a
// byte-based count that works for English has to hold up here too — this is the
// script where under-counting is easiest.
func TestNonASCIITextIsNotUnderCounted(t *testing.T) {
	t.Parallel()

	const chars = 200
	cjk := strings.Repeat("日", chars) // 3 bytes each, ~1 token each

	est, err := pricing.Estimate("openai", "gpt-5.6-luna", cjk, 1)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if inputTokens := est.Tokens - 1; inputTokens < chars {
		t.Errorf("estimated %d tokens for %d CJK characters, which cost roughly "+
			"one token each", inputTokens, chars)
	}
}

// TestARowMissingARateCannotProduceAnEstimate.
//
// Absent is not zero. A row whose author forgot the input or output rate would
// otherwise yield a cheap-looking number built from missing data, and the guard
// would authorize against it.
func TestARowMissingARateCannotProduceAnEstimate(t *testing.T) {
	t.Parallel()

	rate := int64(2_000_000)

	tests := []struct {
		name  string
		price *knov1.Price
	}{
		{"nil price", nil},
		{"no input rate", &knov1.Price{OutputPerMtokUsdMicros: &rate}},
		{"no output rate", &knov1.Price{InputPerMtokUsdMicros: &rate}},
		{"neither", &knov1.Price{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := pricing.EstimateWithPrice(tc.price, "m", "hello", 100); !errors.Is(err, pricing.ErrUnpriced) {
				t.Errorf("err = %v, want ErrUnpriced", err)
			}
		})
	}
}

// TestCostRoundsUpNotDown.
//
// Truncating would shave a fraction off every single reservation, and a bound
// that is systematically a little low is a bound that eventually is not one.
func TestCostRoundsUpNotDown(t *testing.T) {
	t.Parallel()

	// One token at $1/MTok is 1 micro-USD exactly; the interesting case is a
	// rate that does not divide evenly.
	rate := int64(1) // 1 micro-USD per million tokens
	p := &knov1.Price{InputPerMtokUsdMicros: &rate, OutputPerMtokUsdMicros: &rate}

	est, err := pricing.EstimateWithPrice(p, "m", "a", 1)
	if err != nil {
		t.Fatalf("EstimateWithPrice: %v", err)
	}
	// A handful of tokens at 1 micro-USD per MILLION rounds to a fraction of a
	// micro-USD. Rounding down would make it free.
	if est.CostUSDMicros < 1 {
		t.Errorf("cost = %d; a non-zero amount of usage rounded to nothing, so "+
			"a cap would never see it accumulate", est.CostUSDMicros)
	}
}

// TestEmptyInputIsPricedForItsOutputOnly: a Case with no prompt still permits
// an answer, and that answer is what the ceiling bounds.
func TestEmptyInputIsPricedForItsOutputOnly(t *testing.T) {
	t.Parallel()

	est, err := pricing.Estimate("openai", "gpt-5.6-terra", "", 1_000)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.Tokens != 1_000 {
		t.Errorf("tokens = %d, want 1000 (the output ceiling alone)", est.Tokens)
	}
	if est.CostUSDMicros <= 0 {
		t.Error("a Case with no prompt reserved nothing, though it may still answer")
	}
}
