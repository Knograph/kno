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
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// Version identifies the table, and is recorded on every Run.
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
func Lookup(scheme, model string) (*knov1.Price, bool) {
	byModel, ok := table[scheme]
	if !ok {
		return nil, false
	}
	p, ok := byModel[model]
	return p, ok
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
	return out
}
