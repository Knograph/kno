package pricing_test

import (
	"math"
	"testing"

	"github.com/knograph/kno/adapters/agent/pricing"
)

// TestRegionalMultiplierPctPinsThePartnerCloudPremium (docs/debt.md#41(d)).
// Bedrock and Vertex reach the same Claude models as `anthropic`, but their
// regional endpoints add 10% to every category. The multiplier keys off the
// MODEL ID, never the caller's environment, so a cross-region inference
// profile id — which bills at the destination region's price, not 1.10x of
// the base row — gets no multiplier; it is refused by Lookup and the
// adapter's refusal names the class.
func TestRegionalMultiplierPctPinsThePartnerCloudPremium(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		scheme string
		model  string
		want   int64
	}{
		{"anthropic", "anthropic", "claude-sonnet-4-5", 100},
		{"openai", "openai", "gpt-5.6-sol", 100},
		{"exec has no regional add-on", "exec", "my-agent-command", 100},
		{"bedrock base id", "bedrock", "anthropic.claude-sonnet-4-5-20250929-v1:0", 110},
		{"vertex base id", "vertex", "claude-sonnet-4-5", 110},
		{"bedrock us profile", "bedrock", "us.anthropic.claude-3-5-sonnet-20241022-v2:0", 100},
		{"vertex eu profile", "vertex", "eu.claude-3-5-sonnet@20240620", 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pricing.RegionalMultiplierPct(tt.scheme, tt.model); got != tt.want {
				t.Errorf("RegionalMultiplierPct(%q, %q) = %d, want %d", tt.scheme, tt.model, got, tt.want)
			}
		})
	}
}

// TestRegionClassNamesCrossRegionProfiles pins the prefix rule the pricing
// drift detector's check 6 and the unpriced refusals both rely on.
func TestRegionClassNamesCrossRegionProfiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		model string
		want  string
	}{
		{"us.anthropic.claude-3-5-sonnet-20241022-v2:0", "us"},
		{"eu.claude-3-5-sonnet@20240620", "eu"},
		{"anthropic.claude-sonnet-4-5-20250929-v1:0", ""},
		{"claude-sonnet-4-5", ""},
		{"useless-model", ""}, // a prefix, not a class
		{"", ""},
	}
	for _, tt := range tests {
		if got := pricing.RegionClass(tt.model); got != tt.want {
			t.Errorf("RegionClass(%q) = %q, want %q", tt.model, got, tt.want)
		}
	}
}

// TestRegionalRoundsUpAndSaturates pins the multiplier's arithmetic: rounding
// UP on the extra term (a bound that is systematically a little low is a
// bound that eventually is not one) and saturating on overflow (a wrapped
// product landing small and positive reads as a cheap call).
func TestRegionalRoundsUpAndSaturates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cost int64
		pct  int64
		want int64
	}{
		{"identity at 100", 1234, 100, 1234},
		{"pct below 100 is identity", 1000, 99, 1000}, // a mistake must not under-reserve
		{"zero cost stays zero", 0, 110, 0},
		{"negative cost stays zero", -5, 110, 0},
		{"exact multiple", 100, 110, 110},
		{"rounds up", 101, 110, 112}, // ceil(111.1)
		{"small cost rounds up", 1, 110, 2},
		{"saturates on overflow", math.MaxInt64, 110, math.MaxInt64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pricing.Regional(tt.cost, tt.pct); got != tt.want {
				t.Errorf("Regional(%d, %d) = %d, want %d", tt.cost, tt.pct, got, tt.want)
			}
		})
	}
}
