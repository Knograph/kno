// This file is the single spend renderer. Every stage that runs a budget
// guard reports what it spent through the two constructors here, and nothing
// outside this file reaches into a budget.Spend.
//
// The rule is mechanical rather than cultural: TestSpendFieldsAreReadInOneFile
// walks the AST of every non-test file in cli/ and fails on any selector that
// reaches through a field named Spent, or one whose name ends in Spend,
// outside this file. Passing a whole budget.Spend to a renderer stays legal
// anywhere; formatting its contents does not. A second private spend
// rendering is the divergence this file exists to prevent — the human line
// and the --json block disagreeing about what a stage cost is worse than
// either one being absent.
//
// The shapes here carry --json struct tags but nothing here encodes: the
// encoding/json exemption stays scoped to jsonreport.go.

package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/knograph/kno/stats/budget"
)

// spendReport is the spend block emitted by every stage that runs a budget
// guard, embedded into that stage's --json document so the keys are identical
// across stages and flatten to the top level.
//
// Seven keys, and each earns its place:
//
//   - guarded says whether a budget guard ran, which is a fact about the
//     STAGE and not about the dollars. It is what makes the absence of this
//     whole block on select/export/report readable rather than inferable: jq
//     returns null for a missing key exactly as it does for an explicit null,
//     so absence alone degrades a pipeline quietly, and the repair a consumer
//     reaches for — `.spent_usd // 0` — reinstates the very ambiguity this
//     block exists to remove, relocated to the consumer where no golden here
//     can see it. The CI idiom is `select(.guarded) | .spent_usd_micros`.
//   - spent_usd is the released v0.1 key and stays a formatted display
//     string. Retyping it would break every jq pipeline written against v0.1.
//   - spent_usd_micros exists because the string cannot be summed. Money in
//     this codebase is integer micro-USD end to end; emitting only the display
//     form pushes CI authors into parsing a currency-formatted float, which
//     is exactly what the engine refuses to do internally.
//   - llm_calls closes the human/--json asymmetry: the human line has printed
//     a call count since v0.1 and the document has not. Paired with a $0.00
//     figure it is what distinguishes "the meter ran and read zero" from
//     "there is no meter".
//   - tokens, usage_estimated_cases and resumed are qualifiers, absent when
//     they have nothing to say.
type spendReport struct {
	Guarded        bool   `json:"guarded"`
	SpentUSD       string `json:"spent_usd"`
	SpentUSDMicros int64  `json:"spent_usd_micros"`
	LLMCalls       int64  `json:"llm_calls"`
	Tokens         int64  `json:"tokens,omitempty"`

	// UsageEstimatedCases is how many Cases were priced from the engine's
	// prediction rather than from reported usage — the qualifier
	// CaseExecution.usage_estimated_case_count has recorded since v0.1 and
	// which nothing rendered. The spend figure is a guess to this extent.
	UsageEstimatedCases int32 `json:"usage_estimated_cases,omitempty"`

	// Resumed marks a run-lifetime figure that covers sessions this process
	// did not run. The run is the unit the user authorized — the consent
	// quote, the cost cap and the resume dialog all bound the run — so the
	// run is what is reported, and the caveat travels with it.
	Resumed bool `json:"resumed,omitempty"`
}

// newSpendReport builds the block for a stage that ran a guard.
//
// Guarded is unconditionally true: this constructor is reachable only from a
// stage that constructed a guard, and a stage that did not never emits the
// block at all.
func newSpendReport(s budget.Spend, usageEstimatedCases int32, resumed bool) spendReport {
	return spendReport{
		Guarded:             true,
		SpentUSD:            formatUSD(s.CostUSDMicros),
		SpentUSDMicros:      s.CostUSDMicros,
		LLMCalls:            s.Calls,
		Tokens:              s.Tokens,
		UsageEstimatedCases: usageEstimatedCases,
		Resumed:             resumed,
	}
}

// spendLines writes the human spend block: one line always, and a note line
// per qualifier that has something to say.
//
// Byte-identical to the line kno baseline has printed since v0.1, deliberately
// — the existing transcript golden is untouched by the extraction, which is
// what makes the extraction reviewable as a refactor rather than as a change.
//
// The resumed note is here and not only in --json because §6's argument for
// reporting run-lifetime spend rests on what a CI log shows, and a CI log
// shows the human rendering. A caveat that appears only in the surface nobody
// reads is not a caveat.
func spendLines(w io.Writer, s budget.Spend, usageEstimatedCases int32, resumed bool) error {
	var b strings.Builder
	fmt.Fprintf(&b, "  spent      %s over %d call(s)\n", formatUSD(s.CostUSDMicros), s.Calls)
	if usageEstimatedCases > 0 {
		fmt.Fprintf(&b, "  note       %d case(s) priced from the engine's estimate "+
			"rather than reported usage\n", usageEstimatedCases)
	}
	if resumed {
		b.WriteString("  note       resumed run — this figure covers earlier sessions " +
			"of the same run, not just this one\n")
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("writing the spend block: %w", err)
	}
	return nil
}

// reportSpendEntry is one referenced run's settled spend, read from the store
// rather than from a guard: `kno report` runs no guard and reports on runs
// whose processes are long gone.
type reportSpendEntry struct {
	RunID          string `json:"run_id"`
	SpentUSD       string `json:"spent_usd"`
	SpentUSDMicros int64  `json:"spent_usd_micros"`
	LLMCalls       int64  `json:"llm_calls"`
}

// reportSpend is `kno report`'s pipeline total: what knowing this cost.
//
// An object rather than a bare top-level spent_usd, which would claim report
// spent it. Every entry is by construction a run that ran a guard — the page
// names exactly one Value run and, through valueRun.GetBaselineRunId(), one
// Baseline — and select and export are structurally absent rather than
// present at zero, for the same reason their own documents emit no spend
// block.
type reportSpend struct {
	Baseline reportSpendEntry `json:"baseline"`
	Value    reportSpendEntry `json:"value"`

	TotalUSD       string `json:"total_usd"`
	TotalUSDMicros int64  `json:"total_usd_micros"`
	TotalLLMCalls  int64  `json:"total_llm_calls"`

	// Incomplete is true when any referenced run stopped early, so a total
	// assembled from a budget-stopped Value run cannot pass for a complete
	// cost. It is a floor, and it says so.
	Incomplete bool `json:"incomplete"`

	// NoMeteredSpend distinguishes the reachable zero from a missing meter.
	// Every fake: pipeline in CI settles zero across metered runs, and a
	// consumer reading total_usd_micros: 0 must be able to tell "these runs
	// were metered and cost nothing" from "nothing here was measured". The
	// entries carry non-zero llm_calls beside it, saying the same thing twice.
	NoMeteredSpend bool `json:"no_metered_spend,omitempty"`
}

// newReportSpend composes the pipeline total from the two metered runs the
// page always names.
func newReportSpend(
	baselineRunID string, baseline budget.Spend,
	valueRunID string, value budget.Spend,
	incomplete bool,
) reportSpend {
	total := baseline.CostUSDMicros + value.CostUSDMicros
	calls := baseline.Calls + value.Calls
	return reportSpend{
		Baseline: reportSpendEntry{
			RunID:          baselineRunID,
			SpentUSD:       formatUSD(baseline.CostUSDMicros),
			SpentUSDMicros: baseline.CostUSDMicros,
			LLMCalls:       baseline.Calls,
		},
		Value: reportSpendEntry{
			RunID:          valueRunID,
			SpentUSD:       formatUSD(value.CostUSDMicros),
			SpentUSDMicros: value.CostUSDMicros,
			LLMCalls:       value.Calls,
		},
		TotalUSD:       formatUSD(total),
		TotalUSDMicros: total,
		TotalLLMCalls:  calls,
		Incomplete:     incomplete,
		NoMeteredSpend: total == 0,
	}
}

// spendCostSection renders the report page's ## Cost table, pinned to the
// --json spend object by sharing this file's one composition.
func spendCostSection(b *strings.Builder, s reportSpend) {
	b.WriteString("\n## Cost\n\n")
	b.WriteString("| Stage | Run | Spent | LLM calls |\n|---|---|---|---|\n")
	fmt.Fprintf(b, "| baseline | `%s` | %s | %d |\n",
		s.Baseline.RunID, s.Baseline.SpentUSD, s.Baseline.LLMCalls)
	fmt.Fprintf(b, "| value | `%s` | %s | %d |\n",
		s.Value.RunID, s.Value.SpentUSD, s.Value.LLMCalls)
	fmt.Fprintf(b, "| **total** | | **%s** | **%d** |\n", s.TotalUSD, s.TotalLLMCalls)
	b.WriteString("\n_Select and export make no LLM calls and are absent from this table " +
		"rather than listed at zero._\n")
	if s.NoMeteredSpend {
		b.WriteString("\n_Both runs were metered and settled nothing — a zero measured, " +
			"not a missing meter. The call counts above are what the meter counted._\n")
	}
	if s.Incomplete {
		b.WriteString("\n_**Incomplete**: a referenced run stopped early, so this total is a " +
			"floor on what the pipeline cost, not the total._\n")
	}
}
