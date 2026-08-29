package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"

	"github.com/knograph/kno/adapters/agent/pricing"
)

// This file is the pricing-drift detector's half of the docs/debt.md#41(d)
// repayment: the +10% regional multiplier the engine charges on Bedrock and
// Vertex must be confirmed by a price-of-record source, or the constant is
// just a comment. AWS publishes a machine-readable Bedrock price list, so
// Bedrock is CHECKED. Vertex publishes no machine-readable price source,
// which is reported on every run — a scheme whose constant nothing checks is
// a scheme whose constant nobody notices drifting, and the report line is the
// standing obligation.

// bedrockRegions carries one region's prices per rate code, as rationals.
//
// The committed fixture (testdata/bedrock.json) is the extracted shape
// recordBedrock writes from AWS's raw offers index; the prices are USD per
// the unit AWS publishes (per 1K tokens), kept as decimal strings on the wire
// and parsed to rationals here.
type bedrockRegions map[string]map[string]*big.Rat

// regionalBandPct is the acceptance band around the committed multiplier,
// relative to the multiplier itself. Prices round to 8 decimals of a
// per-1K-token USD figure, so a 1.10x multiplier can surface as 1.095x or
// 1.105x for a small rate; the band absorbs that rounding while a multiplier
// changed to 1.05 or 1.20 still fails.
const regionalBandPct = 1

// parseBedrock parses the trimmed AWS Bedrock price list fixture.
//
// Shape:
//
//	{"regions":{"us-east-1":{"AnthropicClaudeSonnet45InputTokens":{"price":"0.0030000000"}}}}
//
// A missing envelope is errShape (unusable as data); a malformed price is
// errRow (the checks judge it).
func parseBedrock(body []byte) (bedrockRegions, error) {
	var o struct {
		Regions map[string]map[string]struct {
			Price string `json:"price"`
		} `json:"regions"`
	}
	if err := json.Unmarshal(body, &o); err != nil {
		return nil, fmt.Errorf("%w: bedrock price list does not parse: %v", errShape, err)
	}
	if len(o.Regions) == 0 {
		return nil, fmt.Errorf("%w: bedrock price list carries no regions", errShape)
	}
	out := make(bedrockRegions, len(o.Regions))
	for region, rates := range o.Regions {
		m := make(map[string]*big.Rat, len(rates))
		for code, entry := range rates {
			r, ok := new(big.Rat).SetString(entry.Price)
			if !ok || r.Sign() <= 0 {
				return nil, fmt.Errorf("%w: bedrock rate code %s/%s has price %q", errRow, region, code, entry.Price)
			}
			m[code] = r
		}
		out[region] = m
	}
	return out, nil
}

// checkRegionalMultiplier is check 6 (gate): the +10% regional multiplier
// committed in pricing.RegionalMultiplierPct must be confirmed by AWS's
// machine-readable Bedrock price list. For every rate code the fixture
// carries in more than one region, the spread of regional prices (highest
// over lowest) must be the committed multiplier within the rounding band.
// A fixture with no rate code in two regions FAILS: a multiplier nothing
// checks is a multiplier nobody notices drifting.
//
// Vertex has no machine-readable price source, which is REPORTED every run —
// its multiplier is the same committed constant, and the report line is the
// reminder that nothing live guards it.
func checkRegionalMultiplier(in checkInput) checkResult {
	r := newResult(6, "regional multiplier")

	// The constant is keyed by scheme and model; the probe model is any
	// non-profile Bedrock id, which is the class the multiplier covers.
	pct := pricing.RegionalMultiplierPct(pricing.SchemeBedrock, "anthropic.claude-sonnet-4-5-20250929-v1:0")
	if pct <= 100 {
		r.fail(fmt.Sprintf("RegionalMultiplierPct returns %d, but a multiplier at or below 100%% cannot reserve the regional premium and under-reserves every Case", pct))
	}

	regions, ok := in.regionPrices["bedrock"]
	if !ok || len(regions) == 0 {
		r.report("bedrock has no regional price data; the regional multiplier is not checked against AWS's price list")
	} else {
		checked := 0
		want := big.NewRat(pct, 100)
		lo := new(big.Rat).Mul(want, big.NewRat(100-regionalBandPct, 100))
		hi := new(big.Rat).Mul(want, big.NewRat(100+regionalBandPct, 100))
		for _, code := range rateCodesOf(regions) {
			prices := pricesOf(regions, code)
			if len(prices) < 2 {
				continue
			}
			checked++
			if !spreadWithin(prices, lo, hi) {
				r.fail(fmt.Sprintf("rate code %s: %s is not the committed %s (%d%% +/-%d%%); the constant or the price list drifted",
					code, spreadText(prices), want.FloatString(2)+"x", pct, regionalBandPct))
			}
		}
		if checked == 0 {
			r.fail("no bedrock rate code appears in two regions; the regional multiplier is uncheckable — re-record the fixture")
		} else {
			r.Summary = fmt.Sprintf("%d rate codes across %d regions confirm the %d%% regional multiplier",
				checked, len(regions), pct)
		}
	}
	r.report("vertex has no machine-readable price source; its regional multiplier is the same committed constant, unchecked by any live source (docs/debt.md#41(d))")
	return *r
}

// regionPrice is one region's price for one rate code.
type regionPrice struct {
	region string
	price  *big.Rat
}

// rateCodesOf returns the union of rate codes across regions, sorted for the
// report's determinism.
func rateCodesOf(regions bedrockRegions) []string {
	seen := make(map[string]bool)
	for _, rates := range regions {
		for code := range rates {
			seen[code] = true
		}
	}
	out := make([]string, 0, len(seen))
	for code := range seen {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}

// pricesOf lists every region's price for one rate code.
func pricesOf(regions bedrockRegions, code string) []regionPrice {
	var out []regionPrice
	for region, rates := range regions {
		if p, ok := rates[code]; ok {
			out = append(out, regionPrice{region: region, price: p})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].region < out[j].region })
	return out
}

// spreadWithin reports whether highest-over-lowest across the prices lands in
// [lo, hi].
func spreadWithin(prices []regionPrice, lo, hi *big.Rat) bool {
	hiP, loP := prices[0].price, prices[0].price
	for _, p := range prices[1:] {
		if p.price.Cmp(hiP) > 0 {
			hiP = p.price
		}
		if p.price.Cmp(loP) < 0 {
			loP = p.price
		}
	}
	spread := new(big.Rat).Quo(hiP, loP)
	return spread.Cmp(lo) >= 0 && spread.Cmp(hi) <= 0
}

// spreadText renders the spread with the regions behind it, for the FAIL
// line a reviewer acts on.
func spreadText(prices []regionPrice) string {
	hiP, loP, hiR, loR := prices[0].price, prices[0].price, prices[0].region, prices[0].region
	for _, p := range prices[1:] {
		if p.price.Cmp(hiP) > 0 {
			hiP, hiR = p.price, p.region
		}
		if p.price.Cmp(loP) < 0 {
			loP, loR = p.price, p.region
		}
	}
	spread := new(big.Rat).Quo(hiP, loP)
	return fmt.Sprintf("spread %s (%s %s over %s %s)", spread.FloatString(4)+"x", hiR, hiP.FloatString(10), loR, loP.FloatString(10))
}

// regionPricesOf gathers the parsed regional lists from the sources.
func regionPricesOf(sources map[string]sourceData) map[string]bedrockRegions {
	out := make(map[string]bedrockRegions)
	for name, sd := range sources {
		if len(sd.regions) > 0 {
			out[name] = sd.regions
		}
	}
	return out
}
