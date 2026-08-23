// Package pricing holds what each model costs, and turns that into the
// pessimistic estimate the budget guard reserves against.
//
// The table is STATIC and DATED. It is never fetched at runtime: a pricing
// endpoint that is down leaves the engine choosing between refusing to run and
// running with no ceiling, and one that is wrong is a spend path with no
// ceiling at all. Prices change, so the table carries the date it was taken and
// a refresh is a reviewed diff — see docs/debt.md#40.
//
// An unknown model is NOT priced at zero. A zero estimate makes a dollar cap
// unenforceable, which is the failure that overshot a cap in M1. Lookup reports
// absence; the caller decides, and under a cost cap the answer is to refuse.
package pricing

import (
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// Version identifies the table.
//
// Run.pricing_table_version exists to carry it, and NOTHING writes that field
// yet — the wiring lands with the adapter. Said plainly because the previous
// wording claimed it was already recorded on every Run.
//
// A date, because that is the only honest thing to call it: these are the
// prices as published on this day, and a run's cost figures mean "reported
// usage at these rates" rather than "your invoice".
const Version = "2026-08-21"

// usd converts dollars-per-million-tokens to the micro-USD the schema uses.
//
// Written as a function over a float literal rather than as a pre-multiplied
// integer so the table reads like the published price list and a reviewer can
// check a row against the provider's page without arithmetic.
func usd(perMTok float64) *int64 {
	v := int64(perMTok*1_000_000 + 0.5)
	return &v
}

// price builds a Price from the four rates. A nil rate means the provider does
// not bill that dimension separately — which is NOT the same as free, and is
// why every field on the wire has presence.
func price(input, cachedRead, cacheWrite, output *int64) *knov1.Price {
	return &knov1.Price{
		InputPerMtokUsdMicros:       input,
		CachedInputPerMtokUsdMicros: cachedRead,
		CacheWritePerMtokUsdMicros:  cacheWrite,
		OutputPerMtokUsdMicros:      output,
	}
}

// table is keyed by scheme and then by model.
//
// Prices as published on 2026-08-21:
//   - https://platform.claude.com/docs/en/about-claude/pricing
//   - https://developers.openai.com/api/docs/models/compare
//
// Only the two schemes M2 ships adapters for. A model reached through a base
// URL — OpenRouter, a self-hosted server, another provider's compatible
// endpoint — is deliberately absent: its prices are not these, and inventing a
// row would be worse than reporting the model as unpriced.
var table = map[string]map[string]*knov1.Price{
	"anthropic": {
		// Anthropic publishes TWO cache-write rates: 5-minute at 1.25x base
		// input and 1-hour at 2x. The schema carries one. The 5-minute rate is
		// recorded because it is the default, and M2's adapter never sets
		// cache_control, so no cache write is billed at all — see
		// docs/debt.md#41 for the trigger to revisit.
		"claude-opus-5":     price(usd(5), usd(0.50), usd(6.25), usd(25)),
		"claude-opus-4-8":   price(usd(5), usd(0.50), usd(6.25), usd(25)),
		"claude-opus-4-7":   price(usd(5), usd(0.50), usd(6.25), usd(25)),
		"claude-opus-4-6":   price(usd(5), usd(0.50), usd(6.25), usd(25)),
		"claude-opus-4-5":   price(usd(5), usd(0.50), usd(6.25), usd(25)),
		"claude-sonnet-5":   price(usd(2), usd(0.20), usd(2.50), usd(10)),
		"claude-sonnet-4-6": price(usd(3), usd(0.30), usd(3.75), usd(15)),
		"claude-sonnet-4-5": price(usd(3), usd(0.30), usd(3.75), usd(15)),
		"claude-haiku-4-5":  price(usd(1), usd(0.10), usd(1.25), usd(5)),
		"claude-fable-5":    price(usd(10), usd(1), usd(12.50), usd(50)),
	},
	"openai": {
		// No separate cache-WRITE rate is published: OpenAI discounts cached
		// input on read and charges nothing to populate the cache. Left unset
		// rather than zero, which is exactly the distinction presence exists
		// for — "not billed separately" against "billed at nothing".
		"gpt-5.6-sol":   price(usd(4), usd(0.40), nil, usd(20)),
		"gpt-5.6-terra": price(usd(2), usd(0.20), nil, usd(12)),
		"gpt-5.6-luna":  price(usd(0.20), usd(0.02), nil, usd(1.20)),
	},
}

// Lookup returns the price for a model.
//
// The second return is false for an unknown model, and the caller must not
// substitute a zero: with a cost cap set, an unpriceable Case is refused rather
// than authorized against a guess.
//
// A COPY, always. Returning the table's own pointer let a caller re-price the
// model for every worker in the process by writing through it — and the
// obvious implementation of a --price override does exactly that. Measured:
// one write turned 128k output tokens of Opus 5 from $3.20 into $0.000020,
// which budget.validate accepts because it is positive. It was also a genuine
// data race, since workers read the table concurrently.
//
// An exact match is tried first, then the longest matching prefix. Providers
// publish dated identifiers — claude-sonnet-4-5-20250929 is the canonical API
// ID and claude-sonnet-4-5 is the alias — and pricing only the alias means a
// user who pins a version gets their run refused under a cost cap.
func Lookup(scheme, model string) (*knov1.Price, bool) {
	byModel, ok := table[scheme]
	if !ok {
		return nil, false
	}
	if p, ok := byModel[model]; ok {
		return proto.CloneOf(p), true
	}
	// Only a VERSION suffix inherits the base row. A provider also appends
	// suffixes that change the price — "claude-opus-5-fast" is fast mode, and
	// Anthropic prices it well above base — and inheriting there authorizes a
	// run at a fraction of its true rate with no signal at all. An unpriced
	// model refuses under a cost cap, which the user sees; a confidently wrong
	// ceiling is the failure prime directive 4 exists to prevent.
	if base, ok := longestPrefix(model, byModel); ok && isVersionSuffix(model[len(base):]) {
		return proto.CloneOf(byModel[base]), true
	}
	return nil, false
}

// isVersionSuffix reports whether suffix names a VERSION of the base model
// rather than a different product.
//
// The distinction providers actually draw is orthographic: **versions are
// numbers, variants are words.** Every published pin is digits and separators
// — "-20250805", "-2026-03-01", "-0613", "-1-20250805" for a point release,
// "@20250929" on Vertex, "-20250929-v1:0" on Bedrock — and every variant that
// carries its own price is a word: "-fast", "-pro", "-mini", "-preview". The
// one word that is a version is "latest", which providers document as an alias
// to the current snapshot at the same rate.
//
// Deliberately NOT a date parser. An earlier rule required exactly eight
// digits, which refused "claude-opus-4-1-20250805" — a shipped identifier
// whose point-release segment makes nine — while accepting "-2026-13-45",
// which is not a date. Counting digits described neither versions nor dates.
// Validating a date would still refuse the point-release form, and the table
// already carries claude-opus-4-5 and claude-opus-4-8, so point releases are
// not hypothetical here.
//
// A suffix with no digit at all is refused, so a bare "-" or "-v" cannot pass.
func isVersionSuffix(suffix string) bool {
	if len(suffix) < 2 || (suffix[0] != '-' && suffix[0] != '@') {
		return false
	}
	body := suffix[1:]
	if body == "latest" {
		return true
	}

	digits := false
	for _, seg := range strings.FieldsFunc(body, isVersionSeparator) {
		switch {
		case isAllDigits(seg):
			digits = true
		// A platform revision tag: Bedrock's "-v1:0" trails the date.
		case len(seg) > 1 && seg[0] == 'v' && isAllDigits(seg[1:]):
			digits = true
		default:
			return false
		}
	}
	return digits
}

// isVersionSeparator reports whether r divides one version segment from the
// next. Providers use all four across their pinned identifiers.
func isVersionSeparator(r rune) bool {
	return r == '-' || r == '.' || r == ':' || r == '@'
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// longestPrefix finds the most specific table key that model starts with.
//
// Longest rather than first: if both "claude-opus-4" and "claude-opus-4-5"
// were priced, "claude-opus-4-5-20251101" must resolve to the second.
//
// Finding a base is not the same as accepting it: Lookup additionally requires
// the remainder to be a version suffix. This function only answers "which row
// is the longest prefix".
func longestPrefix[V any](model string, keys map[string]V) (string, bool) {
	best := ""
	for k := range keys {
		if strings.HasPrefix(model, k) && len(k) > len(best) {
			best = k
		}
	}
	return best, best != ""
}

// Models lists what is priced for a scheme, so a refusal can say what IS known
// rather than only that the model is not.
func Models(scheme string) []string {
	byModel, ok := table[scheme]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(byModel))
	for m := range byModel {
		out = append(out, m)
	}
	// Sorted: this goes into a refusal message, and CLI output is snapshot
	// tested. Map order would flake the snapshot and read differently to a
	// user on every run.
	sort.Strings(out)
	return out
}
