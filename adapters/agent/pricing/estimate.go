package pricing

import (
	"fmt"
	"strings"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
)

// ErrUnpriced means a model has no row in the table.
//
// Deliberately an error rather than a zero price. A zero estimate makes a
// dollar cap unenforceable, and the engine treats an unpriced Case under a cost
// cap exactly as it treats one it could not price at all: refused.
var ErrUnpriced = fmt.Errorf("pricing: no price for this model")

// safetyMargin multiplies the token estimate.
//
// The estimate bounds a reservation, and a bound that can be too low is not a
// bound. Token counting here is an approximation over bytes rather than the
// provider's own tokenizer — see countTokens — so the margin covers the error
// that approximation can have in the ONE direction that matters. Reserving too
// much only makes the guard refuse early, which is recoverable; reserving too
// little is how a run walks past its cap.
const safetyMargin = 1.25

// newTokenizerModels use a tokenizer that produces roughly 30% more tokens for
// the same text than the previous one.
//
// Anthropic publishes this for Claude 4.7 and later. It is not a rounding
// detail: applying the old ratio to a new model under-counts every input by
// about a quarter, and an under-count is the direction that breaks a cap.
var newTokenizerModels = []string{
	"claude-opus-5",
	"claude-opus-4-8",
	"claude-sonnet-5",
	"claude-fable-5",
}

// newTokenizerInflation is the multiplier for those models.
const newTokenizerInflation = 1.3

// Estimate reports the most one call could cost, in the shape the guard
// authorizes against.
//
// Pessimistic by construction, on every term:
//
//   - Input is charged at the FRESH rate, never the cached one. Whether a
//     prompt hits the provider's cache is not knowable before the call, and
//     assuming a hit would under-reserve exactly when a run is repeating
//     similar prompts — which is most of the time.
//   - Output is charged at the full ceiling, not at what a typical answer
//     costs, because the ceiling is what the request permits.
//   - Cache writes are not charged at all: M2's adapters never set
//     cache_control, so none is billed.
func Estimate(scheme, model, input string, maxOutputTokens int64) (budget.Estimate, error) {
	p, ok := Lookup(scheme, model)
	if !ok {
		return budget.Estimate{}, fmt.Errorf("%w: %s:%s", ErrUnpriced, scheme, model)
	}
	return EstimateWithPrice(p, model, input, maxOutputTokens)
}

// EstimateWithPrice is Estimate against a caller-supplied price, which is what
// the --price override flags produce.
func EstimateWithPrice(p *knov1.Price, model, input string, maxOutputTokens int64) (budget.Estimate, error) {
	if p == nil {
		return budget.Estimate{}, ErrUnpriced
	}
	// A model priced with no input or output rate cannot bound anything. Absent
	// is not zero, and a caller must not be handed a cheap-looking number built
	// from missing data.
	if p.InputPerMtokUsdMicros == nil || p.OutputPerMtokUsdMicros == nil {
		return budget.Estimate{}, fmt.Errorf(
			"%w: the row for %s has no input or output rate", ErrUnpriced, model)
	}
	if maxOutputTokens <= 0 {
		return budget.Estimate{}, fmt.Errorf(
			"pricing: %s has no output ceiling, so the output term is unbounded", model)
	}

	inTokens := countTokens(input, model)

	cost := perMTok(p.GetInputPerMtokUsdMicros(), inTokens) +
		perMTok(p.GetOutputPerMtokUsdMicros(), maxOutputTokens)

	return budget.Estimate{Calls: 1, CostUSDMicros: cost, Tokens: inTokens + maxOutputTokens}, nil
}

// perMTok converts a per-million-token rate and a token count to micro-USD,
// rounding UP.
//
// Integer arithmetic throughout, and the rounding direction is deliberate:
// truncating would shave a fraction off every single reservation, and a bound
// that is systematically a little low is a bound that eventually is not one.
func perMTok(ratePerMTok, tokens int64) int64 {
	if tokens <= 0 || ratePerMTok <= 0 {
		return 0
	}
	const mtok = 1_000_000
	return (ratePerMTok*tokens + mtok - 1) / mtok
}

// countTokens approximates how many tokens a string costs.
//
// An approximation, and the godoc says so because a reader deciding whether to
// trust a reservation deserves to know. Shipping a real BPE tokenizer means a
// large dependency plus a per-model vocabulary that goes stale silently, and it
// would buy precision the estimate does not need: this bounds a reservation,
// and settlement reconciles against the provider's own reported usage.
//
// Three BYTES per token, not the usual four characters. Four is the English
// average, and an average is the wrong statistic for a bound. Counting bytes
// rather than characters is what makes one ratio work across scripts: ASCII
// prose comes out about a third over its real count, which is the safe
// direction, while CJK sits at three UTF-8 bytes per character and roughly one
// token per character, so bytes/3 lands about right.
//
// An earlier version also floored this at the RUNE count, reasoning that no
// tokenizer emits fewer than one token per character. True for CJK and badly
// wrong for the common case: for ASCII the floor dominates and estimates one
// token per character, about four times reality. Sanity-checking a realistic
// prompt caught it — every reservation would have been 4x too large, and
// forfeited headroom is not free.
func countTokens(s string, model string) int64 {
	if s == "" {
		return 0
	}
	n := (int64(len(s)) + 2) / 3

	if usesNewTokenizer(model) {
		n = int64(float64(n)*newTokenizerInflation + 0.5)
	}
	return int64(float64(n)*safetyMargin + 0.5)
}

// usesNewTokenizer reports whether a model uses the denser tokenizer.
//
// Prefix-matched, because a provider appends dated suffixes to a model name —
// "claude-opus-5-20260514" is the same tokenizer as "claude-opus-5", and an
// exact match would silently fall back to the old ratio for every pinned
// version.
func usesNewTokenizer(model string) bool {
	for _, m := range newTokenizerModels {
		if strings.HasPrefix(model, m) {
			return true
		}
	}
	return false
}
