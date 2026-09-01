package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/knograph/kno/bridge"
)

// renderBridgePlan prints the un-armed plan every bridge run computes
// first: one line per job, the total, and — armed or not — the
// irreversibility sentence, per the tuner-bridge plan's Step 4.
func renderBridgePlan(out io.Writer, f bridgeFlags, quotes []bridge.GroupQuote, groups *bridge.GroupsPlan) error {
	if f.jsonOut {
		return renderBridgePlanJSON(out, f, quotes, groups)
	}
	return renderBridgePlanHuman(out, f, quotes, groups)
}

func renderBridgePlanHuman(out io.Writer, f bridgeFlags, quotes []bridge.GroupQuote, groups *bridge.GroupsPlan) error {
	var b []byte
	b = append(b, fmt.Sprintf("Bridge plan for %s (%d job%s):\n\n",
		f.tuner, len(quotes), pluralSuffix(len(quotes)))...)
	for _, q := range quotes {
		b = append(b, fmt.Sprintf("  %-24s %4d asset(s)  %10d tokens  %s\n",
			q.Group, len(q.AssetIDs), q.TrainTokens, formatUSD(q.EstimatedCostUSDMicros))...)
	}
	b = append(b, fmt.Sprintf("\n  Total (training only): %s\n", formatUSD(bridge.TotalEstimatedCostUSDMicros(quotes)))...)

	if len(groups.Skipped) > 0 {
		b = append(b, fmt.Sprintf("\n  Skipped (below core.MinClusterCases): %v\n", groups.Skipped)...)
	}
	if len(groups.Unknown) > 0 {
		b = append(b, fmt.Sprintf("  Unknown (routed to no cluster, no bridge verdict possible): %v\n", groups.Unknown)...)
	}

	b = append(b, "\n  Each job is charged when it is submitted and cannot be un-submitted.\n"...)
	if !f.bridgeArmed {
		b = append(b, "  Re-run with --bridge to submit; without it this printed the plan and spent nothing.\n"...)
	}

	_, err := out.Write(b)
	return err
}

// pluralSuffix returns "s" unless n is exactly one. Named apart from the
// package's existing plural(n, singular, pluralForm) helper, which returns
// a whole rendered phrase rather than a bare suffix.
func pluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// bridgePlanJSON is the --json shape of the un-armed plan.
type bridgePlanJSON struct {
	Tuner             string          `json:"tuner"`
	Jobs              []bridgeJobJSON `json:"jobs"`
	TotalEstimatedUSD float64         `json:"total_estimated_usd"`
	Skipped           []string        `json:"skipped_clusters,omitempty"`
	Unknown           []string        `json:"unknown_assets,omitempty"`
	Armed             bool            `json:"armed"`
}

type bridgeJobJSON struct {
	Group            string  `json:"group"`
	AssetCount       int     `json:"asset_count"`
	TrainTokens      int64   `json:"train_tokens"`
	EstimatedCostUSD float64 `json:"estimated_cost_usd"`
}

func renderBridgePlanJSON(out io.Writer, f bridgeFlags, quotes []bridge.GroupQuote, groups *bridge.GroupsPlan) error {
	doc := bridgePlanJSON{
		Tuner:   f.tuner,
		Armed:   f.bridgeArmed,
		Skipped: groups.Skipped,
		Unknown: groups.Unknown,
	}
	for _, q := range quotes {
		doc.Jobs = append(doc.Jobs, bridgeJobJSON{
			Group:            q.Group,
			AssetCount:       len(q.AssetIDs),
			TrainTokens:      q.TrainTokens,
			EstimatedCostUSD: microsToUSD(q.EstimatedCostUSDMicros),
		})
	}
	doc.TotalEstimatedUSD = microsToUSD(bridge.TotalEstimatedCostUSDMicros(quotes))

	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// microsToUSD renders micro-USD as a float for --json only. Rounded to the
// cent: a float printed at full micro-USD precision invites a consumer to
// treat trailing digits as meaningful, and every human-facing rendering in
// this package already rounds through formatUSD.
func microsToUSD(micros int64) float64 {
	cents := micros / 10_000
	return float64(cents) / 100
}
