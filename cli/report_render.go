package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/charmbracelet/glamour"

	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// reportWidth is the fixed wrap width the page renders at. Fixed, so the
// page is deterministic wherever it renders: a pty that reports 200 columns
// and one that reports 80 would otherwise show different documents for the
// same run.
const reportWidth = 100

// renderReport dispatches to the two renderers that share one composed
// snapshot. The shared input is what keeps the human page and the --json
// contract from drifting apart: both are golden-pinned to the same
// reportData, and a change to the reading code changes both or the
// equivalence test fails.
func renderReport(out io.Writer, jsonOut bool, data *reportData) error {
	if jsonOut {
		return writeReportJSON(out, data)
	}
	return renderReportHuman(out, data)
}

// renderReportHuman renders the page through glamour — the markdown engine
// glow itself renders through (glow v1.5 exports no library API; its
// non-interactive path is this renderer). Non-interactive: no raw mode, no
// event loop; the watch redraw is a plain ticker around this function.
func renderReportHuman(out io.Writer, data *reportData) error {
	tr, err := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(reportWidth),
	)
	if err != nil {
		return fmt.Errorf("starting the markdown renderer: %w", err)
	}
	styled, err := tr.Render(buildReportMarkdown(data))
	if err != nil {
		return fmt.Errorf("rendering the report: %w", err)
	}
	_, err = io.WriteString(out, styled)
	return err
}

// buildReportMarkdown composes the recorded stages into the one-page
// document the one-shot render and every --watch snapshot share.
//
// Everything rendered is an aggregate or a header: response blobs are trace
// content and never reach the page, in either renderer. The page is the
// pinned content the equivalence test holds the --json contract to.
func buildReportMarkdown(d *reportData) string {
	var b strings.Builder

	b.WriteString("# Kno report\n\n")
	fmt.Fprintf(&b, "- Value run `%s` (%s)\n", d.ValueRun.GetId(), statusName(d.ValueRun.GetStatus()))
	if reason := d.ValueRun.GetIncompleteReason(); reason != "" {
		fmt.Fprintf(&b, "- value run incomplete: %s\n", reason)
	}
	fmt.Fprintf(&b, "- Baseline `%s` (%s)\n\n", d.Baseline.GetId(), statusName(d.Baseline.GetStatus()))

	b.WriteString("## Baseline\n\n")
	if d.BaselineScore != nil {
		fmt.Fprintf(&b, "score **%.3f** — %d scored", *d.BaselineScore, d.BaselineScored)
		if d.BaselineErrored > 0 {
			fmt.Fprintf(&b, ", %d errored", d.BaselineErrored)
		}
		b.WriteString("\n")
	} else {
		b.WriteString("no score recorded\n")
	}

	b.WriteString("\n## Asset verdicts\n\n")
	dir := reportDir(d.ValueRun)
	b.WriteString("_Deltas are in the Goal's own units; positive is toward the Goal._\n\n")
	b.WriteString("| Asset | Delta (95% CI) | Corrected |\n|---|---|---|\n")
	for _, v := range d.Valuations {
		delta := "—"
		if note := notMeasuredNote(v.GetNotMeasured()); note != "" {
			delta = "— (" + note + ")"
		} else if iv := v.GetDeltaInterval(); iv != nil {
			delta = fmt.Sprintf("%+.4f [%+.4f, %+.4f]",
				dir*v.GetDeltaGoal(), dir*iv.GetLow(), dir*iv.GetHigh())
		}
		corrected := "—"
		if scale, ok := portfolioScale(d.Portfolio, v.GetAssetId()); ok {
			corrected = fmt.Sprintf("×%.4f", scale)
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", v.GetAssetId(), delta, corrected)
	}
	if d.Portfolio == nil {
		b.WriteString("\n_No Portfolio recorded: intervals are raw, uncorrected for screening._\n")
	}
	if hasScale(d.Portfolio) {
		b.WriteString("\n_×scale rows carry the Select run's routing-fraction correction to the tagged delta._\n")
	}

	if d.SelectRun != nil {
		b.WriteString("\n## Portfolio\n\n")
		fmt.Fprintf(&b, "Select run `%s` (%s)\n\n", d.SelectRun.GetId(), statusName(d.SelectRun.GetStatus()))
		if d.Portfolio == nil {
			b.WriteString("portfolio not yet recorded\n")
		} else {
			p := d.Portfolio
			if iv := p.GetDevEstimatedInterval(); iv != nil {
				fmt.Fprintf(&b, "- dev-estimated gain **%+.4f** [%+.4f, %+.4f]  (selection-time; "+
					"winner's-curse inflated)\n",
					p.GetDevEstimatedGain(), iv.GetLow(), iv.GetHigh())
			}
			// The holdout block prints beside a dev estimate, and also
			// whenever a Validate run was named — but NOT for a Portfolio that
			// selected nothing and was never validated. v0.1 printed no caveat
			// there and it was right to: "not yet validated on holdout" over an
			// empty selection is a caveat about a number that does not exist.
			if p.GetDevEstimatedInterval() != nil || d.ValidateRun != nil {
				writeHoldoutBlock(&b, d)
			}
			if reason := p.GetSourceIncompleteReason(); reason != "" {
				fmt.Fprintf(&b, "- source Value run %s (%s): %s\n",
					p.GetSourceRunId(), statusName(p.GetSourceStatus()), reason)
			}
			if len(p.GetRejected()) > 0 {
				b.WriteString("\n### Rejected, by reason\n\n")
				b.WriteString("| Reason | Count | Assets |\n|---|---|---|\n")
				for _, g := range rejectionsByReason(p.GetRejected()) {
					fmt.Fprintf(&b, "| %s | %d | %s |\n",
						g.reason, g.count, strings.Join(g.assets, ", "))
				}
			}
		}
	}

	if d.ExportRun != nil {
		b.WriteString("\n## Gaps\n\n")
		fmt.Fprintf(&b, "Export run `%s` (%s)\n\n", d.ExportRun.GetId(), statusName(d.ExportRun.GetStatus()))
		if d.Gaps == nil {
			b.WriteString("no cluster data for this run\n")
		} else {
			b.WriteString("| Cluster | Verdict | Coverage | Best asset | Best delta (95% CI) |\n|---|---|---|---|---|\n")
			for _, c := range d.Gaps.GetClusters() {
				best, delta := "—", "—"
				if c.GetBestAssetId() != "" {
					best = c.GetBestAssetId()
					if iv := c.GetBestInterval(); iv != nil {
						delta = fmt.Sprintf("%+.4f [%+.4f, %+.4f]",
							dir*c.GetBestDelta(), dir*iv.GetLow(), dir*iv.GetHigh())
					}
				}
				fmt.Fprintf(&b, "| `%s` | %s | %d of %d | %s | %s |\n",
					c.GetTag(), gapVerdictWord(c),
					c.GetCoveredCount(), c.GetCaseCount(), best, delta)
			}
			if d.Gaps.GetMultipleTesting() {
				fmt.Fprintf(&b, "\n_This list is a discovery aid, not a test — with %d clusters, "+
					"as many as %d of these verdicts can be noise under screening._\n",
					len(d.Gaps.GetClusters()), len(d.Gaps.GetClusters()))
			}
		}
	}

	// The answer to "what did knowing this cost me?" — the ROI frame the
	// design's success criterion is built on, and a question the tool could
	// not answer before, because the number is a sum across runs and no
	// single stage holds them all.
	spendCostSection(&b, d.Spend)

	b.WriteString("\n_Recorded aggregates only: no LLM calls, no evals re-read, no trace content — " +
		"the money above was spent by the runs named, not by this page._\n")
	return b.String()
}

// reportDir un-negates MINIMIZE once, for display, the way value's own
// report does: stored deltas are sign-corrected (positive is better), and
// the page reads in the Goal's own units.
func reportDir(run *knov1.Run) float64 {
	if run.GetGoalDirection() == knov1.Direction_DIRECTION_MINIMIZE {
		return -1
	}
	return 1
}

// notMeasuredNote says why one Asset carries no delta, in the value
// report's own words.
func notMeasuredNote(r knov1.RejectionReason) string {
	switch r {
	case knov1.RejectionReason_REJECTION_REASON_IRRELEVANT:
		return "routed to nothing"
	case knov1.RejectionReason_REJECTION_REASON_BUDGET_EXHAUSTED:
		return "budget exhausted mid-measurement"
	case knov1.RejectionReason_REJECTION_REASON_UNDERPOWERED:
		return "sample too small or ragged to form an interval"
	default:
		return ""
	}
}

// reasonGroup is one rejection reason folded across the log.
type reasonGroup struct {
	reason string
	count  int
	assets []string
}

// rejectionsByReason folds the rejection log into counts per reason,
// ordered deterministically: count descending, then reason. The asset list
// within a group stays in the Portfolio's own order.
func rejectionsByReason(rejected []*knov1.Rejection) []reasonGroup {
	groups := map[string]*reasonGroup{}
	for _, r := range rejected {
		reason := rejectReasonName(r.GetReason())
		g, ok := groups[reason]
		if !ok {
			g = &reasonGroup{reason: reason}
			groups[reason] = g
		}
		g.count++
		g.assets = append(g.assets, r.GetAssetId())
	}
	out := make([]reasonGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].reason < out[j].reason
	})
	return out
}

// portfolioScale returns the Select run's correction metadata for an Asset:
// the routing-fraction scale recorded on its Portfolio entry. Presence is
// the flag the page's Corrected column shows — the correction is a
// measurement, not a constant.
func portfolioScale(p *knov1.Portfolio, assetID string) (float64, bool) {
	for _, e := range p.GetSelected() {
		if e.GetAssetId() == assetID && e.NRoutedScale != nil {
			return *e.NRoutedScale, true
		}
	}
	return 0, false
}

// hasScale reports whether any Portfolio entry carries correction metadata,
// which gates the legend line.
func hasScale(p *knov1.Portfolio) bool {
	for _, e := range p.GetSelected() {
		if e.NRoutedScale != nil {
			return true
		}
	}
	return false
}

// gapVerdictWord renders one cluster's verdict, keeping the two UNKNOWN
// flavors apart the way the record does: a cluster with no covered cases
// was never measured, and a cluster with covered cases but no usable
// interval was measured but underpowered.
func gapVerdictWord(c *knov1.GapCluster) string {
	switch c.GetStatus() {
	case knov1.GapStatus_GAP_STATUS_IMPROVED:
		return "**improved**"
	case knov1.GapStatus_GAP_STATUS_GAP:
		return "**gap**"
	case knov1.GapStatus_GAP_STATUS_UNKNOWN:
		if c.GetCoveredCount() == 0 {
			return fmt.Sprintf("unknown — nothing routed to ≥ %d cases", core.MinClusterCases)
		}
		return "unknown — routed but underpowered"
	default:
		return "unknown"
	}
}

// writeHoldoutBlock renders what the holdout says, or says that nothing does.
//
// THE CAVEAT'S ABSENCE HAS TO BE EARNED. The v0.1 string is unchanged and is
// still printed whenever there is no COMPLETED Validate run with a number for
// this Portfolio — including for a validate run that was interrupted, which
// adds a line rather than removing one. A partial peek is not a validation,
// and a page that dropped the caveat because someone STARTED a validate would
// be the exact dishonesty the caveat exists to prevent.
func writeHoldoutBlock(b *strings.Builder, d *reportData) {
	v := d.Validation
	if v == nil || v.GetHoldoutInterval() == nil {
		// Byte-identical to what v0.1 printed. The existing golden pins it,
		// and the sentence about validate not being in this release is now
		// wrong — so it is rewritten to say what is actually true, which is
		// that this PORTFOLIO has no holdout number.
		b.WriteString("- **not yet validated on holdout** — this is a selection-time " +
			"estimate, winner's-curse inflation included. Run `kno validate` to " +
			"measure this portfolio against the untouched holdout.\n")
		if d.ValidateRun != nil {
			fmt.Fprintf(b, "- a validation was attempted (run `%s`, %s) and produced no "+
				"number; a partial peek is not a validation\n",
				d.ValidateRun.GetId(), statusName(d.ValidateRun.GetStatus()))
		}
		return
	}
	iv := v.GetHoldoutInterval()
	fmt.Fprintf(b, "- **holdout gain %+.4f** [%+.4f, %+.4f] — %d holdout Cases, %d trial(s), "+
		"verdict %s\n",
		v.GetHoldoutGain(), iv.GetLow(), iv.GetHigh(),
		v.GetMeasuredCaseCount(), v.GetTrials(), strings.ToUpper(verdictName(v.GetVerdict())))
	fmt.Fprintf(b, "- shrinkage %+.4f dev → holdout. Expected: the dev figure was chosen on "+
		"the dev slice, and the two figures measure different populations. Not evidence "+
		"of interaction.\n", v.GetDevEstimatedGain()-v.GetHoldoutGain())
	if v.GetHoldoutUnderpowered() {
		fmt.Fprintf(b, "- underpowered: %d holdout Cases is below the minimum for a "+
			"full-strength interval\n", v.GetHoldoutCaseCount())
	}
	if v.GetContextOnly() {
		fmt.Fprintf(b, "- **subset**: --context-only excluded %d entr(ies) (%s), so this "+
			"number covers part of the Portfolio\n",
			len(v.GetExcludedAssetIds()), strings.Join(v.GetExcludedAssetIds(), ", "))
	}
	if n := v.GetHoldoutUseIndex(); n > 1 {
		fmt.Fprintf(b, "- this holdout has measured %d portfolios; the interval above is "+
			"NOT corrected for that\n", n)
	} else {
		b.WriteString("- this holdout has measured 1 portfolio\n")
	}
}
