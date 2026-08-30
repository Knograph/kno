package pricing

import (
	"math"
	"strings"
)

// This file is the docs/debt.md#41(d) repayment: what a model costs on a
// partner cloud is not the vendor's own price. Bedrock and Vertex reach the
// same Claude models as the `anthropic` scheme, but their regional endpoints
// add 10% to every category, and the estimate must say so in all three places
// the guard touches — the per-Case reservation, the consent quote, and the
// settlement. A 10% shortfall in the reservation alone lets a capped run
// overshoot by a tenth of its spend, which is exactly the bleed the ledger
// entry exists to prevent.

// Bedrock and Vertex are the two partner-cloud schemes the regional rules
// apply to. They are spelled as the agent-ref grammar spells them.
const (
	SchemeBedrock = "bedrock"
	SchemeVertex  = "vertex"
)

// RegionalMultiplierPct reports the regional multiplier for a scheme and
// model, as a percentage: 110 means "charge 1.10x".
//
// The multiplier keys off the MODEL ID, never the caller's environment.
// Bedrock cross-region inference profiles — `us.`- and `eu.`-prefixed ids —
// bill at the DESTINATION region's price, which is not 1.10x of the base
// row, so a profile id gets no multiplier AND no row: Lookup refuses it and
// the adapter's refusal names the region class. The day a row exists for a
// profile class, that row carries the destination-region price and the
// multiplier returns 100 for it.
//
// 100 for anything that is not a partner-cloud scheme: no other multiplier
// exists in the engine, and inventing one here would double-count the first
// scheme that legitimately added one.
func RegionalMultiplierPct(scheme, model string) int64 {
	if scheme != SchemeBedrock && scheme != SchemeVertex {
		return 100
	}
	if RegionClass(model) != "" {
		return 100
	}
	return 110
}

// RegionClass reports the cross-region inference profile class a model id
// names, or "" when it names none.
//
// Both providers spell profiles as a region-class prefix on the model id:
// Bedrock uses `us.anthropic.claude-3-5-sonnet-20241022-v2:0`, Vertex uses
// `us.claude-3-5-sonnet@20240620`. The class is what the pricing drift
// detector checks the 10% constant against, and what an unpriced profile's
// refusal names.
func RegionClass(model string) string {
	switch {
	case strings.HasPrefix(model, "us."):
		return "us"
	case strings.HasPrefix(model, "eu."):
		return "eu"
	}
	return ""
}

// Regional applies the multiplier to a cost, rounding UP and saturating.
//
// Rounding direction matches perMTok: a bound that is systematically a little
// low is a bound that eventually is not one. Saturation matches the settle
// paths: a wrapped product landing small and positive reads as a cheap call
// rather than as an error.
//
// pct is a percentage — the value RegionalMultiplierPct returns. 100 is the
// identity; a pct below 100 is treated as the identity, because the
// multiplier exists to make estimates MORE pessimistic and a caller passing
// one has made a mistake that must not under-reserve silently.
func Regional(cost, pct int64) int64 {
	if pct <= 100 {
		return cost
	}
	if cost <= 0 {
		return 0
	}
	// ceil(cost*pct/100) as cost + ceil(cost*(pct-100)/100), so the extra
	// term is the only one that can overflow. Saturate rather than wrap: a
	// wrapped product lands small and positive, which reads as a cheap call.
	extra := (pct - 100) * cost
	if extra < 0 || extra > math.MaxInt64-cost {
		return math.MaxInt64
	}
	return cost + (extra+99)/100
}
