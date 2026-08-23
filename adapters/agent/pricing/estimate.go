package pricing

import (
	"fmt"
	"math"
	"math/bits"
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
const safetyMargin = 1.5

// newTokenizerModels use a tokenizer that produces roughly 30% more tokens for
// the same text than the previous one.
//
// Anthropic publishes this for Claude 4.7 and later AND for Claude Mythos
// Preview. It is not a rounding detail: applying the old ratio to a new model
// under-counts every input by about a quarter, and an under-count is the
// direction that breaks a cap.
//
// Claude Sonnet 4.6 and earlier are deliberately absent — the vendor documents
// them as using the previous tokenizer.
var newTokenizerModels = []string{
	"claude-opus-5",
	"claude-opus-4-8",
	"claude-opus-4-7",
	"claude-sonnet-5",
	"claude-fable-5",
	"claude-mythos-5",
	"claude-mythos-preview",
}

// newTokenizerInflation is the multiplier for those models.
// The high end of the vendor's "approximately 30%". Their own
// characters-per-token figures give 3.4 against 2.5, a ratio of 1.36, and a
// bound takes the high end of a stated approximation rather than the low one.
const newTokenizerInflation = 1.36

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
func Estimate(scheme, model string, prompt Prompt, maxOutputTokens int64) (budget.Estimate, error) {
	p, ok := Lookup(scheme, model)
	if !ok {
		return budget.Estimate{}, fmt.Errorf("%w: %s:%s", ErrUnpriced, scheme, model)
	}
	return EstimateWithPrice(p, model, prompt, maxOutputTokens)
}

// Prompt is everything the provider will bill as input.
//
// A struct rather than a string, and the fields are named after what they are,
// because the billed prompt is not Case.input. It is the system prompt, plus
// the injected Asset, plus the Case's history, plus its input — and the Asset
// is the largest term and the entire point of the product.
//
// The previous signature took one string named "input", which made
// `pricing.Estimate(scheme, model, c.GetInput(), max)` the path of least
// resistance. That omits the Asset, produces a systematically low reservation,
// and looks correct in review. Naming the parts means a forgotten one is
// visible at the call site rather than invisible in the number.
type Prompt struct {
	// System is the system prompt, including any tool definitions.
	System string

	// Context is the injected Asset, for a context-injection measurement.
	Context string

	// History is prior turns the Case carries.
	History string

	// Input is the Case's own input.
	Input string
}

// size reports the total bytes the provider will see.
func (p Prompt) size() int {
	return len(p.System) + len(p.Context) + len(p.History) + len(p.Input)
}

// EstimateWithPrice is Estimate against a caller-supplied price.
//
// It exists for the price-override flags the plan schedules for the CLI wiring
// PR. Those flags do not exist yet; this is the seam they will use.
func EstimateWithPrice(p *knov1.Price, model string, prompt Prompt, maxOutputTokens int64) (budget.Estimate, error) {
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
	// No current model's context window is within three orders of magnitude of
	// this. The bound exists so a fat-fingered ceiling cannot overflow the
	// arithmetic below into a small POSITIVE number, which nothing downstream
	// would catch — budget.validate rejects negatives, not wrapped ones.
	if maxOutputTokens > maxOutputCeiling {
		return budget.Estimate{}, fmt.Errorf(
			"pricing: an output ceiling of %d is beyond any real model's context "+
				"window, and the cost arithmetic cannot bound it", maxOutputTokens)
	}

	inTokens := countTokens(prompt.size(), model)

	cost := perMTok(p.GetInputPerMtokUsdMicros(), inTokens) +
		perMTok(p.GetOutputPerMtokUsdMicros(), maxOutputTokens)

	return budget.Estimate{Calls: 1, CostUSDMicros: cost, Tokens: inTokens + maxOutputTokens}, nil
}

// maxOutputCeiling bounds the output term. Ten million tokens is far above any
// published context window and far below where int64 arithmetic wraps.
const maxOutputCeiling = 10_000_000

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
	// Overflow-checked. A caller-supplied rate is not bounded by anything here,
	// and a wrapped product can land small and positive, which reads as a cheap
	// call rather than as an error.
	hi, lo := bits.Mul64(uint64(ratePerMTok), uint64(tokens))
	if hi != 0 || lo > math.MaxInt64-mtok {
		return math.MaxInt64
	}
	return (int64(lo) + mtok - 1) / mtok
}

// countTokens approximates how many tokens a string costs.
//
// An approximation, and the godoc says so because a reader deciding whether to
// trust a reservation deserves to know. Shipping a real BPE tokenizer means a
// large dependency plus a per-model vocabulary that goes stale silently, and it
// would buy precision the estimate does not need: this bounds a reservation,
// and settlement reconciles against the provider's own reported usage.
//
// TWO bytes per token before the margin, giving about 1.33 effective. Not
// three, and emphatically not the four-characters-per-token rule of thumb.
//
// The rule of thumb is an average over English prose, and a bound needs the
// TAIL. Measured against the real tokenizer, the tail is not a script at all —
// it is machine-shaped text, which is exactly what an Asset embedded in a Case
// looks like:
//
//	base64            1.47 bytes/token
//	random hex        1.79
//	UUID list         1.95
//	newline rows      2.03
//	minified JSON     2.06
//	emoji             2.17
//	Chinese prose     3.4
//	English prose     3.6
//
// An earlier version used bytes/3 with a 1.25 margin — 2.4 bytes/token
// effective — which under-counted base64 by 39%. Priced out, a 200 KB
// attachment on gpt-5.6-sol reserved $0.42 against a true ceiling of $0.64, so
// a $10 cap would settle at $15. That is not the soft bound docs/debt.md#32
// accepts; that entry assumes the per-call error is small, and 34% is not.
//
// An earlier version before THAT floored the count at the rune count, which for
// ASCII estimated one token per character — about four times reality. Both
// mistakes were caught by measuring rather than by reasoning about scripts.
//
// The cost of getting this right is that English prose now reserves about 3x
// what it uses. That is the trade a bound makes: over-reserving forfeits
// headroom, which the feasibility check compensates for by reducing
// concurrency, while under-reserving walks past the cap.
func countTokens(sizeBytes int, model string) int64 {
	if sizeBytes <= 0 {
		return 0
	}
	n := math.Ceil(float64(sizeBytes) / bytesPerToken)

	if usesNewTokenizer(model) {
		n *= newTokenizerInflation
	}
	// Ceil, not round-to-nearest, matching perMTok. Rounding down turned the
	// safety margin into 1.0x for any input of three bytes or fewer.
	return int64(math.Ceil(n * safetyMargin))
}

// bytesPerToken is the divisor before the safety margin. See countTokens.
const bytesPerToken = 2.0

// usesNewTokenizer reports whether a model uses the denser tokenizer.
//
// Prefix-matched, because a provider appends dated suffixes to a model name —
// "claude-opus-5-20260514" is the same tokenizer as "claude-opus-5", and an
// exact match would silently fall back to the old ratio for every pinned
// version.
//
// Unlike Lookup, this accepts ANY suffix, including a variant Lookup refuses.
// The directions differ: the denser tokenizer produces a larger count, so
// inheriting it for an unknown variant over-reserves, and over-reserving is
// recoverable where under-reserving spends past a cap. Lookup refuses instead
// because a wrong RATE has no safe direction.
func usesNewTokenizer(model string) bool {
	for _, m := range newTokenizerModels {
		if strings.HasPrefix(model, m) {
			return true
		}
	}
	return false
}
