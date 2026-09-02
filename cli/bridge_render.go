package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/knograph/kno/bridge"
)

// renderBridgePlan prints the un-armed plan every bridge run computes
// first: one line per job, a separately-labelled hosting line, an
// eval-pass line, the total, and — armed or not — the irreversibility
// sentence, per the tuner-bridge plan's Step 4. hostingCapUSDMicros is
// Step 2(f)'s cap-bounded worst case (N+1 endpoints x
// --bridge-max-serve-minutes x rate). evalCapUSDMicros is the eval pass's
// own worst case (docs/plans/2026-09-02-openai-tuner.md §4) — zero for a
// provider with no per-token rate, such as Together today. Both are stated
// as ceilings rather than predictions.
func renderBridgePlan(out io.Writer, f bridgeFlags, quotes []bridge.GroupQuote, groups *bridge.GroupsPlan, hostingCapUSDMicros, evalCapUSDMicros int64) error {
	if f.jsonOut {
		return renderBridgePlanJSON(out, f, quotes, groups, hostingCapUSDMicros, evalCapUSDMicros)
	}
	return renderBridgePlanHuman(out, f, quotes, groups, hostingCapUSDMicros, evalCapUSDMicros)
}

func renderBridgePlanHuman(out io.Writer, f bridgeFlags, quotes []bridge.GroupQuote, groups *bridge.GroupsPlan, hostingCapUSDMicros, evalCapUSDMicros int64) error {
	var b []byte
	b = append(b, fmt.Sprintf("Bridge plan for %s (%d job%s):\n\n",
		f.tuner, len(quotes), pluralSuffix(len(quotes)))...)
	for _, q := range quotes {
		b = append(b, fmt.Sprintf("  %-24s %4d asset(s)  %10d tokens  %s\n",
			q.Group, len(q.AssetIDs), q.TrainTokens, formatUSD(q.EstimatedCostUSDMicros))...)
	}
	total := bridge.TotalEstimatedCostUSDMicros(quotes) + hostingCapUSDMicros + evalCapUSDMicros
	b = append(b, fmt.Sprintf("\n  Training subtotal:            %s\n", formatUSD(bridge.TotalEstimatedCostUSDMicros(quotes)))...)
	b = append(b, fmt.Sprintf("  Hosting (worst case, capped):  %s  (%d endpoint%s x %d min)\n",
		formatUSD(hostingCapUSDMicros), len(quotes), pluralSuffix(len(quotes)), f.maxServeMinutes)...)
	b = append(b, fmt.Sprintf("  Eval pass (worst case):        %s\n", formatUSD(evalCapUSDMicros))...)
	b = append(b, fmt.Sprintf("  Total (worst case):            %s\n", formatUSD(total))...)

	if len(groups.Skipped) > 0 {
		b = append(b, fmt.Sprintf("\n  Skipped (below core.MinClusterCases): %v\n", groups.Skipped)...)
	}
	if len(groups.Unknown) > 0 {
		b = append(b, fmt.Sprintf("  Unknown (routed to no cluster, no bridge verdict possible): %v\n", groups.Unknown)...)
	}

	b = append(b, "\n  Each job is charged when it is submitted and cannot be un-submitted; "+
		"hosting is charged per minute per endpoint, including while idle. A cap reached mid-bridge "+
		"stops the next job and tears down any live endpoint; it does not refund a job already sent.\n"...)
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
	Tuner               string          `json:"tuner"`
	Jobs                []bridgeJobJSON `json:"jobs"`
	TrainingSubtotalUSD float64         `json:"training_subtotal_usd"`
	HostingCapUSD       float64         `json:"hosting_cap_usd"`
	// EvalPassCapUSD is the eval pass's own worst case
	// (docs/plans/2026-09-02-openai-tuner.md §4) — 0 for a provider with no
	// per-token rate, such as Together today.
	EvalPassCapUSD    float64  `json:"eval_pass_cap_usd"`
	TotalEstimatedUSD float64  `json:"total_estimated_usd"`
	Skipped           []string `json:"skipped_clusters,omitempty"`
	Unknown           []string `json:"unknown_assets,omitempty"`
	Armed             bool     `json:"armed"`
}

type bridgeJobJSON struct {
	Group            string  `json:"group"`
	AssetCount       int     `json:"asset_count"`
	TrainTokens      int64   `json:"train_tokens"`
	EstimatedCostUSD float64 `json:"estimated_cost_usd"`
}

func renderBridgePlanJSON(out io.Writer, f bridgeFlags, quotes []bridge.GroupQuote, groups *bridge.GroupsPlan, hostingCapUSDMicros, evalCapUSDMicros int64) error {
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
	trainingSubtotal := bridge.TotalEstimatedCostUSDMicros(quotes)
	doc.TrainingSubtotalUSD = microsToUSD(trainingSubtotal)
	doc.HostingCapUSD = microsToUSD(hostingCapUSDMicros)
	doc.EvalPassCapUSD = microsToUSD(evalCapUSDMicros)
	doc.TotalEstimatedUSD = microsToUSD(trainingSubtotal + hostingCapUSDMicros + evalCapUSDMicros)

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

// renderBridgeResult prints what an armed bridge run measured: one line
// per group with its verdict and delta, and every skipped group.
func renderBridgeResult(out io.Writer, f bridgeFlags, result *bridge.RunResult) error {
	if f.jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(bridgeResultJSONOf(result))
	}
	var b []byte
	for _, m := range result.Measured {
		b = append(b, fmt.Sprintf("  %-24s verdict=%-32s delta_group=%+.4f\n",
			m.GetAblationGroup(), m.GetVerdict(), m.GetDeltaGroup())...)
	}
	for _, g := range result.Skipped {
		b = append(b, fmt.Sprintf("  %-24s skipped (job did not succeed)\n", g)...)
	}
	_, err := out.Write(b)
	return err
}

// bridgeResultJSON is the --json shape of an armed run's result.
type bridgeResultJSON struct {
	Measured []bridgeGroupJSON `json:"measured"`
	Skipped  []string          `json:"skipped"`
}

// bridgeGroupJSON is one measured group's --json shape.
type bridgeGroupJSON struct {
	Group      string  `json:"group"`
	Verdict    string  `json:"verdict"`
	DeltaGroup float64 `json:"delta_group"`
}

func bridgeResultJSONOf(result *bridge.RunResult) bridgeResultJSON {
	doc := bridgeResultJSON{Skipped: result.Skipped}
	for _, m := range result.Measured {
		doc.Measured = append(doc.Measured, bridgeGroupJSON{
			Group: m.GetAblationGroup(), Verdict: m.GetVerdict().String(),
			// Rounded to four decimal places at the source: Go fuses
			// multiply-add differently on arm64 vs amd64, and this is
			// the same discipline cli/evalinspect.go and judge/kappa.go
			// already apply before anything crosses a --json boundary.
			DeltaGroup: roundTo4(m.GetDeltaGroup()),
		})
	}
	return doc
}
