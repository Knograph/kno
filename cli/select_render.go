package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// renderSelect writes the Select report, human or machine-readable.
func renderSelect(out io.Writer, jsonOut bool, res *core.SelectResult) error {
	if jsonOut {
		return renderSelectJSON(out, res)
	}
	return renderSelectHuman(out, res)
}

// renderSelectHuman prints the Portfolio: the source it was built from, the
// budget it was built under, the selected assets in selection order, the
// rejection log, and the portfolio-level claim — with the winner's curse
// stated, not buried.
func renderSelectHuman(out io.Writer, res *core.SelectResult) error {
	p := res.Portfolio

	if _, err := fmt.Fprintf(out, "Select run %s (%s)\n", res.RunID, res.Status); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  source    %s (%s)\n",
		p.GetSourceRunId(), statusName(p.GetSourceStatus())); err != nil {
		return err
	}
	if reason := p.GetSourceIncompleteReason(); reason != "" {
		if _, err := fmt.Fprintf(out, "  source    incomplete: %s\n", reason); err != nil {
			return err
		}
	}
	if caps := budgetCaps(p.GetBudget()); caps != "" {
		if _, err := fmt.Fprintf(out, "  budget    %s\n", caps); err != nil {
			return err
		}
	}

	selected := p.GetSelected()
	if _, err := fmt.Fprintf(out, "\nSelected %d — greedy on delta-per-cost, no approximation "+
		"guarantee; keep/reject decisions used Bonferroni-corrected intervals\n",
		len(selected)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  %-3s  %-12s  %-16s  %s\n",
		"RANK", "ASSET", "DESTINATION", "DELTA (95% CI)"); err != nil {
		return err
	}
	for _, e := range selected {
		delta := "—"
		if v := e.GetValuation(); v.GetDeltaInterval() != nil {
			iv := v.GetDeltaInterval()
			delta = fmt.Sprintf("%+.4f  [%+.4f, %+.4f]",
				v.GetDeltaGoal(), iv.GetLow(), iv.GetHigh())
		}
		if _, err := fmt.Fprintf(out, "  %-3d  %-12s  %-16s  %s\n",
			e.GetRank(), e.GetAssetId(), destinationName(e.GetDestination()), delta); err != nil {
			return err
		}
	}

	if iv := p.GetDevEstimatedInterval(); iv != nil {
		if _, err := fmt.Fprintf(out, "\n  dev-estimated gain %+.4f [%+.4f, %+.4f] "+
			"(single corrected claim, shared-draw)\n", p.GetDevEstimatedGain(), iv.GetLow(), iv.GetHigh()); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(out, "  this is a selection-time estimate, inflated by the "+
			"winner's curse — the honest number is the validate report's holdout gain\n"); err != nil {
			return err
		}
	}

	rejected := p.GetRejected()
	if len(rejected) > 0 {
		if _, err := fmt.Fprintf(out, "\nRejected %d\n", len(rejected)); err != nil {
			return err
		}
		for _, r := range rejected {
			detail := r.GetDetail()
			if len(r.GetRedundantWithAssetIds()) > 0 {
				detail = "duplicates " + strings.Join(r.GetRedundantWithAssetIds(), ", ")
			}
			if detail != "" {
				detail = "  " + detail
			}
			if _, err := fmt.Fprintf(out, "  %-12s  %-16s%s\n",
				r.GetAssetId(), rejectReasonName(r.GetReason()), detail); err != nil {
				return err
			}
		}
	}

	if len(res.DegradedRules) > 0 {
		if _, err := fmt.Fprintf(out, "\n  rejection rules degraded: %s (no --pool)\n",
			strings.Join(res.DegradedRules, ", ")); err != nil {
			return err
		}
	}

	_, err := fmt.Fprintf(out,
		"\nPortfolio recorded. `kno export` writes the selected assets to their destinations.\n")
	return err
}

// budgetCaps renders the constraint set the Portfolio was built under, in
// the flag names that set it.
func budgetCaps(b *knov1.Budget) string {
	var parts []string
	if b.GetMaxContextTokens() > 0 {
		parts = append(parts, fmt.Sprintf("context ≤ %d tokens", b.GetMaxContextTokens()))
	}
	if b.GetMaxTrainingExamples() > 0 {
		parts = append(parts, fmt.Sprintf("tuning ≤ %d examples", b.GetMaxTrainingExamples()))
	}
	if b.GetMaxKnowledgeBaseBytes() > 0 {
		parts = append(parts, fmt.Sprintf("knowledge base ≤ %d bytes", b.GetMaxKnowledgeBaseBytes()))
	}
	if b.GetMaxCostUsdMicros() > 0 {
		parts = append(parts, "cost ≤ "+formatUSD(b.GetMaxCostUsdMicros()))
	}
	return strings.Join(parts, "; ")
}

// rejectReasonName renders a rejection reason in the vocabulary's words.
func rejectReasonName(r knov1.RejectionReason) string {
	switch r {
	case knov1.RejectionReason_REJECTION_REASON_NO_EFFECT:
		return "no-effect"
	case knov1.RejectionReason_REJECTION_REASON_REGRESSION:
		return "regression"
	case knov1.RejectionReason_REJECTION_REASON_REDUNDANT:
		return "redundant"
	case knov1.RejectionReason_REJECTION_REASON_COST_DOMINATED:
		return "cost-dominated"
	case knov1.RejectionReason_REJECTION_REASON_WRONG_MECHANISM:
		return "wrong-mechanism"
	case knov1.RejectionReason_REJECTION_REASON_IRRELEVANT:
		return "irrelevant"
	case knov1.RejectionReason_REJECTION_REASON_BUDGET_EXHAUSTED:
		return "budget-exhausted"
	case knov1.RejectionReason_REJECTION_REASON_MEASUREMENT_FAILED:
		return "measurement-failed"
	case knov1.RejectionReason_REJECTION_REASON_UNDERPOWERED:
		return "underpowered"
	default:
		return "unspecified"
	}
}

// destinationName renders a Destination in the flag grammar.
func destinationName(d knov1.Destination) string {
	switch d {
	case knov1.Destination_DESTINATION_KNOWLEDGE_BASE:
		return "knowledge_base"
	case knov1.Destination_DESTINATION_TUNING_SET:
		return "tuning_set"
	default:
		return "context"
	}
}
