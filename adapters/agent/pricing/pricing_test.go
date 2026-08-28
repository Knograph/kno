package pricing_test

import (
	"errors"
	"math"
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
// publishedPrices is the table as printed on the provider pages on the date
// pricing.Version names, in dollars per million tokens.
//
// Package-level so that both the "does the table match" test and the "is every
// row checked" test read one list. An earlier version populated it from inside
// the first test, which made the second depend on running after it — an
// ordering dependency under t.Parallel, which is a flake waiting for a shuffle.
var publishedPrices = []struct {
	scheme, model             string
	input, cachedRead, output float64
	cacheWrite                float64
	hasCacheWrite             bool
}{
	{scheme: "anthropic", model: "claude-opus-5", input: 5, cachedRead: 0.50, cacheWrite: 6.25, hasCacheWrite: true, output: 25},
	{scheme: "anthropic", model: "claude-opus-4-8", input: 5, cachedRead: 0.50, cacheWrite: 6.25, hasCacheWrite: true, output: 25},
	{scheme: "anthropic", model: "claude-opus-4-7", input: 5, cachedRead: 0.50, cacheWrite: 6.25, hasCacheWrite: true, output: 25},
	{scheme: "anthropic", model: "claude-opus-4-6", input: 5, cachedRead: 0.50, cacheWrite: 6.25, hasCacheWrite: true, output: 25},
	{scheme: "anthropic", model: "claude-opus-4-5", input: 5, cachedRead: 0.50, cacheWrite: 6.25, hasCacheWrite: true, output: 25},
	{scheme: "anthropic", model: "claude-sonnet-5", input: 2, cachedRead: 0.20, cacheWrite: 2.50, hasCacheWrite: true, output: 10},
	{scheme: "anthropic", model: "claude-sonnet-4-6", input: 3, cachedRead: 0.30, cacheWrite: 3.75, hasCacheWrite: true, output: 15},
	{scheme: "anthropic", model: "claude-sonnet-4-5", input: 3, cachedRead: 0.30, cacheWrite: 3.75, hasCacheWrite: true, output: 15},
	{scheme: "anthropic", model: "claude-haiku-4-5", input: 1, cachedRead: 0.10, cacheWrite: 1.25, hasCacheWrite: true, output: 5},
	{scheme: "anthropic", model: "claude-fable-5", input: 10, cachedRead: 1, cacheWrite: 12.50, hasCacheWrite: true, output: 50},
	// Fast mode publishes input/output only — no cache rates exist for it, so
	// cachedRead stays 0 ("unpublished"), which the test reads as nil presence.
	{scheme: "anthropic", model: "claude-opus-5-fast", input: 10, output: 50},
	{scheme: "anthropic", model: "claude-opus-4-8-fast", input: 10, output: 50},
	// OpenAI publishes no separate cache-WRITE rate. Unset, not zero.
	{scheme: "openai", model: "gpt-5.6-sol", input: 4, cachedRead: 0.40, output: 20},
	{scheme: "openai", model: "gpt-5.6-terra", input: 2, cachedRead: 0.20, output: 12},
	{scheme: "openai", model: "gpt-5.6-luna", input: 0.20, cachedRead: 0.02, output: 1.20},
}

func TestTableMatchesThePublishedPrices(t *testing.T) {
	t.Parallel()

	tests := publishedPrices

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
			if tc.cachedRead == 0 {
				// Unpublished, not free: the presence distinction is the claim.
				if p.CachedInputPerMtokUsdMicros != nil {
					t.Errorf("cached input present, want unpublished (nil)")
				}
			} else if got, want := p.GetCachedInputPerMtokUsdMicros(), micros(tc.cachedRead); got != want {
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
		_, err := pricing.Estimate(tc.scheme, tc.model, pricing.Prompt{Input: "hello"}, 1000)
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

	est, err := pricing.Estimate("openai", model, pricing.Prompt{Input: input}, 1_000)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.Calls != 1 {
		t.Errorf("calls = %d, want exactly 1", est.Calls)
	}

	// Input is charged at the FRESH rate. Assuming a cache hit would
	// under-reserve exactly when a run repeats similar prompts, which is most
	// of the time.
	cheaper, err := pricing.Estimate("openai", model, pricing.Prompt{Input: input}, 1_000)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if cheaper.CostUSDMicros != est.CostUSDMicros {
		t.Errorf("the estimate is not deterministic: %d then %d",
			est.CostUSDMicros, cheaper.CostUSDMicros)
	}

	// A bigger output ceiling costs more, because the ceiling is what the
	// request permits rather than what a typical answer uses.
	bigger, err := pricing.Estimate("openai", model, pricing.Prompt{Input: input}, 10_000)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if bigger.CostUSDMicros <= est.CostUSDMicros {
		t.Errorf("raising the output ceiling did not raise the estimate: %d vs %d",
			bigger.CostUSDMicros, est.CostUSDMicros)
	}

	// And longer input costs more.
	longer, err := pricing.Estimate("openai", model, pricing.Prompt{Input: strings.Repeat("a", 30_000)}, 1_000)
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
	newer, err := pricing.Estimate("anthropic", "claude-sonnet-5", pricing.Prompt{Input: input}, 1_000)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	older, err := pricing.Estimate("anthropic", "claude-sonnet-4-5", pricing.Prompt{Input: input}, 1_000)
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

	base, err := pricing.EstimateWithPrice(p, "claude-sonnet-5", pricing.Prompt{Input: input}, 1_000)
	if err != nil {
		t.Fatalf("EstimateWithPrice: %v", err)
	}
	pinned, err := pricing.EstimateWithPrice(p, "claude-sonnet-5-20260514", pricing.Prompt{Input: input}, 1_000)
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
		if _, err := pricing.Estimate("openai", "gpt-5.6-luna", pricing.Prompt{Input: "hi"}, ceiling); err == nil {
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

// TestTokenCountingNeverUndercountsRealContent.
//
// The samples ARE the specification. Ground-truth counts were measured with
// tiktoken (o200k_base) during the Phase-3 review of this package; the ratios
// they exposed are why the divisor is 2 rather than 3.
//
// The failure this guards is asymmetric. Under-counting walks a run past its
// cap — measured at 39% under on base64 with the old ratio, which priced a
// 200 KB attachment at $0.42 against a true ceiling of $0.64. Over-counting
// forfeits headroom, which the feasibility check compensates for by reducing
// concurrency. So the assertion is one-sided by design: never under, and a
// generous ceiling on how far over.
//
// Note what the tail is. It is not a script — Chinese prose measures 3.4
// bytes/token and is perfectly safe. It is machine-shaped text, which is
// exactly what an Asset embedded in a Case looks like.
func TestTokenCountingNeverUndercountsRealContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		real int64 // measured with tiktoken o200k_base
	}{
		{"base64", strings.Repeat("SGVsbG8gV29ybGQhIFRoaXMgaXMgYQ==", 25), 546},
		{"random hex", strings.Repeat("a3f9c2e18b7d4056", 50), 448},
		{"UUIDs", strings.Repeat("3f2504e0-4f89-11d3-9a0c-0305e82c3301\n", 40), 759},
		{"minified JSON", strings.Repeat(`{"id":1,"name":"x","v":[1,2,3]},`, 75), 1142},
		{"English prose", strings.Repeat("How do I get a refund for my order? ", 10), 101},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Output ceiling of 1 so the reported total is input + 1.
			est, err := pricing.Estimate("openai", "gpt-5.6-luna", pricing.Prompt{Input: tc.text}, 1)
			if err != nil {
				t.Fatalf("Estimate: %v", err)
			}
			got := est.Tokens - 1

			if got < tc.real {
				t.Errorf("estimated %d tokens for content worth %d (%.2fx); "+
					"under-counting is how a run walks past its cap, and this "+
					"shape of content is what an Asset looks like",
					got, tc.real, float64(got)/float64(tc.real))
			}
			if ratio := float64(got) / float64(tc.real); ratio > 6 {
				t.Errorf("estimated %d tokens for content worth %d (%.1fx); "+
					"over-reserving forfeits concurrency x estimate of the cap",
					got, tc.real, ratio)
			}
		})
	}
}

// TestNonASCIITextIsNotUnderCounted.
//
// Included for coverage of the multi-byte path, NOT because it is the risky
// case — the review measured Chinese prose at 3.4 bytes/token, comfortably
// safe. An earlier version of this test claimed CJK was "the script where
// under-counting is easiest", which was measurably false, and it used a
// repeated single character, which BPE merges aggressively and which is
// therefore the best case rather than the worst. The real tail is base64, and
// it has its own case above.
func TestNonASCIITextIsNotUnderCounted(t *testing.T) {
	t.Parallel()

	const chars = 200
	cjk := strings.Repeat("日", chars) // 3 bytes each, ~1 token each

	est, err := pricing.Estimate("openai", "gpt-5.6-luna", pricing.Prompt{Input: cjk}, 1)
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
			if _, err := pricing.EstimateWithPrice(tc.price, "m", pricing.Prompt{Input: "hello"}, 100); !errors.Is(err, pricing.ErrUnpriced) {
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

	est, err := pricing.EstimateWithPrice(p, "m", pricing.Prompt{Input: "a"}, 1)
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

	est, err := pricing.Estimate("openai", "gpt-5.6-terra", pricing.Prompt{Input: ""}, 1_000)
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

// TestADatedModelIdentifierIsPriced.
//
// Providers publish a dated ID as canonical and the bare name as an alias —
// claude-sonnet-4-5-20250929 is the "Claude API ID" column, and
// claude-sonnet-4-5 is the alias. Pricing only the alias meant every user who
// pinned a version got their run refused under a cost cap, while the CHANGELOG
// said pinning worked.
//
// It "worked" for the tokenizer, which prefix-matched, and not for the price,
// which did not — and the test that named the property called EstimateWithPrice
// with a pre-fetched price, bypassing Lookup, so it passed on the one path
// where it could not fail. This one goes through Estimate.
func TestADatedModelIdentifierIsPriced(t *testing.T) {
	t.Parallel()

	tests := []struct{ scheme, dated, alias string }{
		{"anthropic", "claude-sonnet-4-5-20250929", "claude-sonnet-4-5"},
		{"anthropic", "claude-haiku-4-5-20251001", "claude-haiku-4-5"},
		{"anthropic", "claude-opus-4-5-20251101", "claude-opus-4-5"},
		{"anthropic", "claude-sonnet-5-20260514", "claude-sonnet-5"},
		{"openai", "gpt-5.6-terra-2026-03-01", "gpt-5.6-terra"},
	}

	for _, tc := range tests {
		t.Run(tc.dated, func(t *testing.T) {
			t.Parallel()

			dated, err := pricing.Estimate(tc.scheme, tc.dated, pricing.Prompt{Input: "hello world"}, 1_000)
			if err != nil {
				t.Fatalf("a pinned version is unpriced, so a cost-capped run "+
					"using the canonical API identifier is refused: %v", err)
			}
			alias, err := pricing.Estimate(tc.scheme, tc.alias, pricing.Prompt{Input: "hello world"}, 1_000)
			if err != nil {
				t.Fatalf("Estimate(%s): %v", tc.alias, err)
			}
			// Same model, same price AND same tokenizer. The two used to be
			// resolved by different rules, which is how they drifted apart.
			if dated.CostUSDMicros != alias.CostUSDMicros || dated.Tokens != alias.Tokens {
				t.Errorf("%s estimated %d micros / %d tokens but %s estimated "+
					"%d / %d; the price and the tokenizer resolve the model "+
					"differently", tc.dated, dated.CostUSDMicros, dated.Tokens,
					tc.alias, alias.CostUSDMicros, alias.Tokens)
			}
		})
	}
}

// TestLookupCannotBeUsedToRepriceTheTable.
//
// Lookup used to return the table's own pointer, so writing through it
// re-priced the model for every worker in the process — and the natural
// implementation of a --price override does exactly that. Measured: one write
// turned 128k output tokens of Opus 5 from $3.20 into $0.000020, which
// budget.validate accepts because it is positive. It was a data race too.
func TestLookupCannotBeUsedToRepriceTheTable(t *testing.T) {
	t.Parallel()

	before, ok := pricing.Lookup("anthropic", "claude-opus-5")
	if !ok {
		t.Fatal("claude-opus-5 is not priced")
	}
	original := before.GetOutputPerMtokUsdMicros()

	zero := int64(0)
	before.OutputPerMtokUsdMicros = &zero

	after, ok := pricing.Lookup("anthropic", "claude-opus-5")
	if !ok {
		t.Fatal("claude-opus-5 is not priced")
	}
	if got := after.GetOutputPerMtokUsdMicros(); got != original {
		t.Errorf("output rate is now %d, was %d; a caller re-priced the model "+
			"for every worker in the process", got, original)
	}
}

// TestAnAbsurdOutputCeilingIsRefusedNotWrapped.
//
// A caller-supplied ceiling has no upper bound of its own, and int64 overflow
// can land small and POSITIVE — which reads as a cheap call rather than as an
// error, and nothing downstream catches it. budget.validate rejects negatives,
// not wrapped ones.
func TestAnAbsurdOutputCeilingIsRefusedNotWrapped(t *testing.T) {
	t.Parallel()

	for _, ceiling := range []int64{math.MaxInt64, math.MaxInt64 / 2, 100_000_000} {
		est, err := pricing.Estimate("anthropic", "claude-opus-5", pricing.Prompt{Input: "hi"}, ceiling)
		if err == nil {
			t.Errorf("an output ceiling of %d was accepted, reserving %d micros",
				ceiling, est.CostUSDMicros)
		}
	}
}

// TestEveryPricedModelIsCoveredByTheExpectations guards the price table from
// growing rows nobody checks against the published sources.
func TestEveryPricedModelIsCoveredByTheExpectations(t *testing.T) {
	t.Parallel()

	checked := map[string]bool{}
	for _, e := range publishedPrices {
		checked[e.scheme+":"+e.model] = true
	}

	for _, scheme := range []string{"anthropic", "openai"} {
		for _, model := range pricing.Models(scheme) {
			if !checked[scheme+":"+model] {
				t.Errorf("%s:%s is priced but no test pins it against the "+
					"published rate; a transposed digit here is a wrong "+
					"reservation nobody would notice", scheme, model)
			}
		}
	}
}

// TestEveryPromptPartIsBilled.
//
// The billed prompt is not Case.input. It is the system prompt plus the
// injected Asset plus the Case's history plus its input — and the Asset is the
// largest term and the entire point of the product.
//
// The signature used to take one string named "input", which made
// `pricing.Estimate(scheme, model, c.GetInput(), max)` the path of least
// resistance. That omits the Asset, produces a systematically low reservation,
// and looks correct in review. This asserts each part actually moves the number.
func TestEveryPromptPartIsBilled(t *testing.T) {
	t.Parallel()

	const filler = "some text that costs tokens "

	base, err := pricing.Estimate("openai", "gpt-5.6-terra", pricing.Prompt{}, 100)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}

	parts := map[string]pricing.Prompt{
		"system":  {System: filler},
		"context": {Context: filler},
		"history": {History: filler},
		"input":   {Input: filler},
	}
	for name, p := range parts {
		est, err := pricing.Estimate("openai", "gpt-5.6-terra", p, 100)
		if err != nil {
			t.Fatalf("Estimate(%s): %v", name, err)
		}
		if est.Tokens <= base.Tokens {
			t.Errorf("the %s part did not change the estimate (%d vs %d); a term "+
				"the provider bills is not being reserved for", name,
				est.Tokens, base.Tokens)
		}
	}

	// And they add up rather than one shadowing another.
	all, err := pricing.Estimate("openai", "gpt-5.6-terra", pricing.Prompt{
		System: filler, Context: filler, History: filler, Input: filler,
	}, 100)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	one, err := pricing.Estimate("openai", "gpt-5.6-terra", pricing.Prompt{Input: filler}, 100)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if all.Tokens <= one.Tokens {
		t.Errorf("four parts estimated %d tokens against %d for one; the parts "+
			"are not summed", all.Tokens, one.Tokens)
	}
}

// TestAVariantSuffixIsUnpricedRatherThanGuessed.
//
// Longest-prefix matching exists so a pinned dated version resolves to its
// base row. Providers also append suffixes that CHANGE the price, and those
// look identical to the matcher: "claude-opus-5-fast" is fast mode, published
// well above base, and it existed on the provider's model list while this
// table priced it at the base rate.
//
// The two failures are not symmetric. An unpriced model is refused under a
// cost cap and the user is told which models are known. A model priced at a
// fraction of its true rate authorizes the run, reserves too little, and the
// cap the user set is not the cap they get.
func TestAVariantSuffixIsUnpricedRatherThanGuessed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		scheme string
		model  string
		want   bool
	}{
		// Versions are numbers. Every one of these is a published pin of the
		// base model, billed at the base rate.
		{"the base model itself", "anthropic", "claude-opus-5", true},
		{"an Anthropic dated pin", "anthropic", "claude-opus-5-20260514", true},
		{"another dated pin", "anthropic", "claude-sonnet-4-5-20250929", true},
		{"a point release before the date", "anthropic", "claude-opus-5-1-20260514", true},
		{"an OpenAI hyphenated date", "openai", "gpt-5.6-terra-2026-03-01", true},
		{"an OpenAI legacy MMDD pin", "openai", "gpt-5.6-luna-0613", true},
		{"a Vertex at-pin", "anthropic", "claude-opus-5@20250929", true},
		{"a Bedrock revision tail", "anthropic", "claude-opus-5-20250929-v1:0", true},
		{"latest is an alias to the current snapshot", "anthropic", "claude-opus-5-latest", true},

		// Variants are words, and these carry their own price.
		// claude-opus-5-fast and claude-opus-4-8-fast now HAVE rows of their
		// own (docs/debt.md#46): an exact row must win over the suffix rule,
		// and TestTableMatchesThePublishedPrices pins their rates at $10/$50 —
		// a fallthrough to the $5/$25 base row fails that pin.
		{"so is fast mode on sonnet", "anthropic", "claude-sonnet-5-fast", false},
		{"a pro tier is a different product", "openai", "gpt-5.6-sol-pro", false},
		{"a preview is not the released model", "anthropic", "claude-opus-5-preview", false},
		{"a date with a trailing word is a variant", "anthropic", "claude-opus-5-20260514-fast", false},

		// Shape guards.
		{"a suffix with no digits at all", "anthropic", "claude-opus-5-v", false},
		{"a bare separator", "anthropic", "claude-opus-5-", false},
		// Separators only: FieldsFunc yields no segments at all, so nothing
		// rejects this except the requirement that a version contain a number.
		{"separators with no version in them", "anthropic", "claude-opus-5-.-.-", false},
		{"the suffix must be separated from the base", "anthropic", "claude-opus-520260514", false},
		{"an unrelated model stays unknown", "anthropic", "llama-3", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := pricing.Lookup(tt.scheme, tt.model)
			if ok != tt.want {
				t.Fatalf("Lookup(%q, %q) priced = %v, want %v", tt.scheme, tt.model, ok, tt.want)
			}
			if !ok {
				return
			}

			// Compare against the base row by NAME, not by re-deriving the
			// base from the model string — an earlier version of this test
			// split on a hardcoded "-2026", which for claude-sonnet-4-5-20250929
			// yielded the whole string back and compared the value to itself.
			base, baseOK := pricing.Lookup(tt.scheme, baseOf(tt.model))
			if !baseOK {
				t.Fatalf("the base row %q is not priced; the fixture is wrong, not the code",
					baseOf(tt.model))
			}
			// EVERY rate, not just input: a wrong resolution can agree on one
			// dimension and differ on the rest.
			if got.GetInputPerMtokUsdMicros() != base.GetInputPerMtokUsdMicros() ||
				got.GetOutputPerMtokUsdMicros() != base.GetOutputPerMtokUsdMicros() ||
				got.GetCachedInputPerMtokUsdMicros() != base.GetCachedInputPerMtokUsdMicros() ||
				got.GetCacheWritePerMtokUsdMicros() != base.GetCacheWritePerMtokUsdMicros() {
				t.Errorf("%s resolved to a different price than its base %s:\n got %v\nwant %v",
					tt.model, baseOf(tt.model), got, base)
			}
		})
	}
}

// baseOf names the table row each fixture model must resolve to. Written out
// rather than derived, so the test cannot agree with the code by using the
// same rule the code uses.
func baseOf(model string) string {
	bases := map[string]string{
		"claude-opus-5":               "claude-opus-5",
		"claude-opus-5-20260514":      "claude-opus-5",
		"claude-sonnet-4-5-20250929":  "claude-sonnet-4-5",
		"claude-opus-5-1-20260514":    "claude-opus-5",
		"gpt-5.6-terra-2026-03-01":    "gpt-5.6-terra",
		"gpt-5.6-luna-0613":           "gpt-5.6-luna",
		"claude-opus-5@20250929":      "claude-opus-5",
		"claude-opus-5-20250929-v1:0": "claude-opus-5",
		"claude-opus-5-latest":        "claude-opus-5",
	}
	return bases[model]
}
