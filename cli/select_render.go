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
			// Detail carries the full claim for every reason, REDUNDANT
			// included: core/redundancy.go's redundancyDetail names which
			// Asset(s) were duplicated AND the evidence behind the claim
			// (shared Case count, paired difference, co-improvement,
			// which criterion decided a tie). An older build's detail here
			// was a generic string with no numbers, and this rendering used
			// to paper over that by substituting redundant_with_asset_ids
			// for it — which would now DISCARD the richer prose in favor of
			// exactly the bare asset list it replaces.
			detail := r.GetDetail()
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

// redundancyEvidenceKindName renders which instrument decided a redundancy
// comparison.
func redundancyEvidenceKindName(k knov1.RedundancyEvidenceKind) string {
	switch k {
	case knov1.RedundancyEvidenceKind_REDUNDANCY_EVIDENCE_KIND_MEASUREMENT:
		return "measurement"
	case knov1.RedundancyEvidenceKind_REDUNDANCY_EVIDENCE_KIND_CONTENT_SHINGLE:
		return "content_shingle"
	default:
		return "unspecified"
	}
}

// marginSourceName renders which term produced the equivalence margin.
func marginSourceName(s knov1.MarginSource) string {
	if s == knov1.MarginSource_MARGIN_SOURCE_USER {
		return "user"
	}
	return "sample_resolution"
}

// coImprovementFloorSourceName renders which term produced the co-improvement
// floor.
func coImprovementFloorSourceName(s knov1.CoImprovementFloorSource) string {
	if s == knov1.CoImprovementFloorSource_CO_IMPROVEMENT_FLOOR_SOURCE_USER {
		return "user"
	}
	return "chance"
}

// redundancyDecidedByName renders which criterion broke a measurement-
// equivalent pair's tie.
func redundancyDecidedByName(d knov1.RedundancyDecidedBy) string {
	switch d {
	case knov1.RedundancyDecidedBy_REDUNDANCY_DECIDED_BY_COST:
		return "cost"
	case knov1.RedundancyDecidedBy_REDUNDANCY_DECIDED_BY_ID:
		return "id"
	case knov1.RedundancyDecidedBy_REDUNDANCY_DECIDED_BY_CONTENT:
		return "content"
	default:
		return "unspecified"
	}
}

// renderExplain writes `kno select --explain <asset-id>`'s output: the
// per-Case table for every Asset named in the explained Asset's redundancy
// evidence. Free, read-only, no provider call.
func renderExplain(out io.Writer, jsonOut bool, assetID string, cmps []core.ExplainComparison) error {
	if jsonOut {
		return writeJSON(out, explainReport(assetID, cmps))
	}
	if len(cmps) == 0 {
		_, err := fmt.Fprintf(out, "%s was not rejected as redundant by this decision; nothing to explain.\n", assetID)
		return err
	}
	if _, err := fmt.Fprintf(out, "%s — redundancy evidence\n", assetID); err != nil {
		return err
	}
	for _, c := range cmps {
		if _, err := fmt.Fprintf(out, "\nagainst %s (%s)\n", c.WithAssetID, redundancyEvidenceKindName(c.Evidence.GetKind())); err != nil {
			return err
		}
		if len(c.Rows) == 0 {
			if _, err := fmt.Fprintf(out, "  no per-Case table: %s decides with no Case-level claim\n",
				redundancyEvidenceKindName(c.Evidence.GetKind())); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(out, "  %-14s  %-10s  %-14s  %-10s  %-14s\n",
			"CASE", "BASELINE", "THIS DELTA", "IMPROVED", "WITH DELTA"); err != nil {
			return err
		}
		for _, r := range c.Rows {
			thisDelta := fmt.Sprintf("%+.4f", r.ThisDelta)
			withDelta := fmt.Sprintf("%+.4f", r.WithDelta)
			if _, err := fmt.Fprintf(out, "  %-14s  %-10.4f  %-14s  %-10t  %-14s\n",
				r.CaseID, r.Baseline, thisDelta, r.ThisImproved, withDelta); err != nil {
				return err
			}
		}
	}
	return nil
}

// explainReportRow / explainReportComparison / explainReport are --explain's
// JSON shape: structured evidence, per the same rule select's own --json
// follows — a reader who disagrees with a claim needs the numbers, not
// parsed prose.
type explainReportRow struct {
	CaseID       string  `json:"case_id"`
	Baseline     float64 `json:"baseline"`
	ThisDelta    float64 `json:"this_delta"`
	ThisImproved bool    `json:"this_improved"`
	WithDelta    float64 `json:"with_delta"`
	WithImproved bool    `json:"with_improved"`
}

type explainReportComparison struct {
	WithAssetID string                `json:"with_asset_id"`
	Evidence    *selectReportEvidence `json:"evidence,omitempty"`
	Rows        []explainReportRow    `json:"rows,omitempty"`
}

type explainReportDoc struct {
	AssetID     string                    `json:"asset_id"`
	Comparisons []explainReportComparison `json:"comparisons"`
}

func explainReport(assetID string, cmps []core.ExplainComparison) explainReportDoc {
	doc := explainReportDoc{AssetID: assetID}
	for _, c := range cmps {
		rc := explainReportComparison{WithAssetID: c.WithAssetID}
		if c.Evidence != nil {
			ev := redundancyEvidenceReport(c.Evidence)
			rc.Evidence = &ev
		}
		for _, r := range c.Rows {
			rc.Rows = append(rc.Rows, explainReportRow{
				CaseID: r.CaseID, Baseline: r.Baseline,
				ThisDelta: r.ThisDelta, ThisImproved: r.ThisImproved,
				WithDelta: r.WithDelta, WithImproved: r.WithImproved,
			})
		}
		doc.Comparisons = append(doc.Comparisons, rc)
	}
	return doc
}
