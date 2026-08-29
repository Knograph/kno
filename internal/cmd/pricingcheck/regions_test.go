package main

import (
	"errors"
	"math/big"
	"strings"
	"testing"
)

// mkRegionalInput builds a checkInput carrying one bedrock regional price
// list, the state checkRegionalMultiplier reads.
func mkRegionalInput(regions bedrockRegions) checkInput {
	in := mkInput(nil)
	in.regionPrices = map[string]bedrockRegions{"bedrock": regions}
	return in
}

// TestParseBedrockFixture parses the committed fixture: three regions at the
// published AWS Bedrock Claude rates — us-east-1 and us-west-2 at the base
// price, eu-central-1 at the +10% premium.
func TestParseBedrockFixture(t *testing.T) {
	t.Parallel()
	regions, err := parseBedrock(readFixture(t, "testdata/bedrock.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(regions) != 3 {
		t.Fatalf("parsed %d regions, want 3", len(regions))
	}
	usEast := regions["us-east-1"]
	if got := usEast["AnthropicClaude-Sonnet45InputTokens"]; got == nil || got.Cmp(big.NewRat(3, 1000)) != 0 {
		t.Errorf("us-east-1 input = %v, want 0.0030000000", got)
	}
	eu := regions["eu-central-1"]
	if got := eu["AnthropicClaude-Sonnet45OutputTokens"]; got == nil || got.Cmp(big.NewRat(165, 10000)) != 0 {
		t.Errorf("eu-central-1 output = %v, want 0.0165000000", got)
	}
}

// TestParseBedrockBroken drives the parser's error kinds: a wrong envelope is
// errShape (unusable as data), a malformed price is errRow (the checks judge
// it), and an absent regions map is errShape — a fixture with no regions
// would make every regional check vacuous.
func TestParseBedrockBroken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want error
	}{
		{"not json", "not json at all", errShape},
		{"no regions", `{"regions":{}}`, errShape},
		{"bad price", `{"regions":{"us-east-1":{"code":{"price":"nan"}}}}`, errRow},
		{"nonpositive price", `{"regions":{"us-east-1":{"code":{"price":"0"}}}}`, errRow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseBedrock([]byte(tt.body))
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want kind %v", err, tt.want)
			}
		})
	}
}

// TestExtractBedrockRegions reduces a synthetic AWS offers index: edge rows
// and non-Anthropic usage types drop, the surviving rows re-parse into the
// fixture shape, and duplicate SKUs with the same price merge.
func TestExtractBedrockRegions(t *testing.T) {
	t.Parallel()
	raw := `{
		"products": {
			"sku-a": {"productFamily": "AI Service", "attributes": {"region": "us-east-1", "locationType": "AWS Region", "usagetype": "AnthropicClaude-Sonnet45InputTokens"}},
			"sku-a2": {"productFamily": "AI Service", "attributes": {"region": "us-east-1", "locationType": "AWS Region", "usagetype": "AnthropicClaude-Sonnet45InputTokens"}},
			"sku-b": {"productFamily": "AI Service", "attributes": {"region": "eu-central-1", "locationType": "AWS Region", "usagetype": "AnthropicClaude-Sonnet45InputTokens"}},
			"sku-edge": {"productFamily": "AI Service", "attributes": {"region": "us-east-1", "locationType": "AWS Edge", "usagetype": "AnthropicClaude-Sonnet45InputTokens"}},
			"sku-aux": {"productFamily": "AI Service", "attributes": {"region": "us-east-1", "locationType": "AWS Region", "usagetype": "AgentInputTokens"}}
		},
		"terms": {
			"OnDemand": {
				"sku-a": {"code-a": {"priceDimensions": {"code-a": {"unit": "Count", "pricePerUnit": {"USD": "0.0030000000"}}}}},
				"sku-a2": {"code-a2": {"priceDimensions": {"code-a2": {"unit": "Count", "pricePerUnit": {"USD": "0.0030000000"}}}}},
				"sku-b": {"code-b": {"priceDimensions": {"code-b": {"unit": "Count", "pricePerUnit": {"USD": "0.0033000000"}}}}},
				"sku-edge": {"code-edge": {"priceDimensions": {"code-edge": {"pricePerUnit": {"USD": "0.0020000000"}}}}},
				"sku-aux": {"code-aux": {"priceDimensions": {"code-aux": {"pricePerUnit": {"USD": "0.0000500000"}}}}}
			}
		}
	}`
	out, err := extractBedrockRegions([]byte(raw))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	regions, err := parseBedrock(out)
	if err != nil {
		t.Fatalf("extracted fixture must re-parse: %v", err)
	}
	if len(regions) != 2 {
		t.Fatalf("extracted %d regions, want 2 (edge and auxiliary rows dropped)", len(regions))
	}
	if got := regions["us-east-1"]["AnthropicClaude-Sonnet45InputTokens"]; got == nil || got.Cmp(big.NewRat(3, 1000)) != 0 {
		t.Errorf("us-east-1 input = %v, want 0.0030000000 (duplicate SKUs merged)", got)
	}
	if _, ok := regions["us-east-1"]["AgentInputTokens"]; ok {
		t.Error("non-Anthropic usage type survived extraction")
	}
}

// TestExtractBedrockRegionsConflictingPrices fails an extraction that would
// silently drop a price: two SKUs sharing a region and usage type must not
// hide a discrepancy from the fixture's sha256.
func TestExtractBedrockRegionsConflictingPrices(t *testing.T) {
	t.Parallel()
	raw := `{
		"products": {
			"sku-a": {"attributes": {"region": "us-east-1", "locationType": "AWS Region", "usagetype": "AnthropicClaude-Sonnet45InputTokens"}},
			"sku-b": {"attributes": {"region": "us-east-1", "locationType": "AWS Region", "usagetype": "AnthropicClaude-Sonnet45InputTokens"}}
		},
		"terms": {
			"OnDemand": {
				"sku-a": {"code-a": {"priceDimensions": {"code-a": {"pricePerUnit": {"USD": "0.0030000000"}}}}},
				"sku-b": {"code-b": {"priceDimensions": {"code-b": {"pricePerUnit": {"USD": "0.0033000000"}}}}}
			}
		}
	}`
	if _, err := extractBedrockRegions([]byte(raw)); err == nil {
		t.Fatal("conflicting duplicate prices must fail the extraction")
	}
}

// TestCheckRegionalMultiplierPass runs the gate against the committed
// fixture: the 1.10x spread between us regions and eu-central-1 confirms the
// committed 110% multiplier within the rounding band, and the vertex line is
// reported as the standing obligation.
func TestCheckRegionalMultiplierPass(t *testing.T) {
	t.Parallel()
	regions, err := parseBedrock(readFixture(t, "testdata/bedrock.json"))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	r := checkRegionalMultiplier(mkRegionalInput(regions))
	if r.Verdict != verdictReport {
		t.Fatalf("verdict = %s, want REPORT (only the standing vertex report line)", r.Verdict)
	}
	if !strings.Contains(r.Summary, "2 rate codes") || !strings.Contains(r.Summary, "110%") {
		t.Errorf("summary = %q, want the confirmed 110%% regional multiplier", r.Summary)
	}
	wantNoFinding(t, r, verdictFail)
	wantFinding(t, r, verdictReport, "vertex has no machine-readable price source")
}

// TestCheckRegionalMultiplierFailSpread fails a spread outside the band: a
// 1.20x premium is not the committed 1.10x, and the constant or the price
// list drifted.
func TestCheckRegionalMultiplierFailSpread(t *testing.T) {
	t.Parallel()
	regions := bedrockRegions{
		"us-east-1":    {"AnthropicClaude-Sonnet45InputTokens": big.NewRat(3, 1000)},
		"eu-central-1": {"AnthropicClaude-Sonnet45InputTokens": big.NewRat(36, 10000)},
	}
	r := checkRegionalMultiplier(mkRegionalInput(regions))
	wantFinding(t, r, verdictFail, "is not the committed 1.10x")
}

// TestCheckRegionalMultiplierFailNoSpread fails a fixture with no rate code
// in two regions: a multiplier nothing checks is a multiplier nobody notices
// drifting.
func TestCheckRegionalMultiplierFailNoSpread(t *testing.T) {
	t.Parallel()
	regions := bedrockRegions{
		"us-east-1": {"AnthropicClaude-Sonnet45InputTokens": big.NewRat(3, 1000)},
	}
	r := checkRegionalMultiplier(mkRegionalInput(regions))
	wantFinding(t, r, verdictFail, "no bedrock rate code appears in two regions")
}

// TestCheckRegionalMultiplierNoData reports, never gates, when the bedrock
// source has no regional prices: live failures are fail-soft so a flaky
// fetch cannot page anyone.
func TestCheckRegionalMultiplierNoData(t *testing.T) {
	t.Parallel()
	r := checkRegionalMultiplier(mkInput(nil))
	wantFinding(t, r, verdictReport, "bedrock has no regional price data")
	wantNoFinding(t, r, verdictFail)
}
