package cli

import (
	"fmt"
	"io"
	"math"
	"strings"
	"text/tabwriter"

	"github.com/knograph/kno/judge"
	"github.com/knograph/kno/stats/interval"
)

// defaultBootstrap is the resampling configuration every calibration uses.
//
// Named in one place so the human rendering, the --json document and the
// ratchet cannot disagree about how the interval was built — the method is
// part of the claim, and a delta whose method changed between two runs did not
// become more precise, it was measured differently.
func defaultBootstrap() interval.Bootstrap {
	return interval.Bootstrap{Support: &interval.Support{Low: -1, High: 1}}
}

// row writes one aligned cell line.
//
// The error is discarded in ONE place rather than at a dozen call sites: the
// destination is a tabwriter over a strings.Builder, and neither can fail. A
// //nolint on every Fprintf would be a dozen unexamined suppressions where one
// examined one will do.
func row(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// renderCalibrations writes the human page for every result.
func renderCalibrations(out io.Writer, results []*judge.Result, showDisagreements bool) error {
	var b strings.Builder
	for i, res := range results {
		if i > 0 {
			b.WriteString("\n")
		}
		renderCalibration(&b, res, showDisagreements)
	}
	if _, err := io.WriteString(out, b.String()); err != nil {
		return fmt.Errorf("writing the calibration report: %w", err)
	}
	return nil
}

// renderCalibration writes one result.
//
// Every number kappa hides is on the same screen as kappa: the raw agreement a
// constant judge would score, the two per-class recalls, both marginals, and
// the ceiling the labelers themselves reach. A single scalar cannot say which
// way a judge is wrong, and which way is exactly what a prompt edit needs to
// know.
func renderCalibration(b *strings.Builder, res *judge.Result, showDisagreements bool) {
	fmt.Fprintf(b, "\nCalibration: %s against %s v%d\n", res.GoalName, res.SetName, res.SetVersion)
	fmt.Fprintf(b, "  source     %s\n", mode(res))
	fmt.Fprintf(b, "  records    %d scored, %d errored\n", res.NScored, res.NErrored)

	if res.BudgetStopped {
		b.WriteString("\n  BUDGET_STOPPED\n")
		fmt.Fprintf(b, "  %s\n", res.Cause)
		b.WriteString("  No agreement statistic is reported: one computed over the records\n" +
			"  that fit under a cap describes a population nobody chose.\n")
		return
	}

	if res.Graded != nil {
		renderGraded(b, res)
		return
	}

	w := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	row(w, "\n  kappa\t%s\t%s\n", num(res.Agreement.Kappa), ciText(res))
	row(w, "  raw agreement\t%s\ta constant judge scores %s on this set\n",
		num(res.Agreement.Raw), num(prevalence(res)))
	row(w, "  sensitivity\t%s\tof the records humans passed\n", num(res.Agreement.Sensitivity))
	row(w, "  specificity\t%s\tof the records humans failed\n", num(res.Agreement.Specificity))
	row(w, "  marginals\tjudge %s\thumans %s\n",
		num(res.Agreement.JudgePositiveRate), num(res.Agreement.HumanPositiveRate))
	row(w, "  inter-human kappa\t%s\tthe ceiling: a judge cannot beat its own labelers\n",
		num(res.InterHuman.Kappa))
	_ = w.Flush()

	if res.Ratchet != nil {
		renderRatchet(b, res.Ratchet)
	}

	fmt.Fprintf(b, "\n  %s\n", res.Verdict)
	if res.Cause != "" {
		fmt.Fprintf(b, "  %s\n", res.Cause)
	}
	if res.Fix != "" {
		fmt.Fprintf(b, "  fix: %s\n", wrapFix(res.Fix))
	}

	if showDisagreements {
		renderDisagreements(b, res)
	} else if n := len(res.Disagreements); n > 0 {
		fmt.Fprintf(b, "\n  %d record(s) disagree. --show-disagreements prints them.\n", n)
	}
}

// renderGraded is the report for a UNIT_INTERVAL Goal: reported, never gated.
func renderGraded(b *strings.Builder, res *judge.Result) {
	w := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	row(w, "\n  weighted kappa\t%s\tquadratic weights over %d human anchors\n",
		num(res.Graded.WeightedKappa), res.Graded.NBins)
	row(w, "  spearman rho\t%s\trank agreement\n", num(res.Graded.Spearman))
	row(w, "  mean abs error\t%s\ton the goal's own scale\n", num(res.Graded.MAE))
	row(w, "  inter-human kappa\t%s\tthe ceiling\n", num(res.InterHuman.Kappa))
	_ = w.Flush()
	b.WriteString("\n  GATE: not applicable (graded domain)\n")
	b.WriteString("  Kappa is undefined on continuous scores, and gating a graded judge needs\n" +
		"  an anchored scale the calibration format does not yet carry. Inventing one\n" +
		"  here would be the invented threshold this command exists to avoid.\n")
}

// renderRatchet writes the comparison against the recorded calibration.
func renderRatchet(b *strings.Builder, r *judge.Ratchet) {
	b.WriteString("\n  against the recorded baseline\n")
	if !r.Comparable {
		fmt.Fprintf(b, "    not comparable: %s\n", r.NotComparable)
		return
	}
	fmt.Fprintf(b, "    kappa %s -> %s", num(r.BaselineKappa), num(r.Kappa))
	if r.Diff != nil {
		fmt.Fprintf(b, ", paired 95%% CI on the difference [%s, %s]",
			num(r.Diff.GetLow()), num(r.Diff.GetHigh()))
	}
	b.WriteString("\n")
	if r.ModelChanged {
		b.WriteString("    the judge MODEL changed: a difference here is not evidence the " +
			"prompt regressed\n")
	}
}

// renderDisagreements is the artifact that makes a prompt edit directed.
func renderDisagreements(b *strings.Builder, res *judge.Result) {
	if len(res.Disagreements) == 0 {
		b.WriteString("\n  No disagreements.\n")
		return
	}
	b.WriteString("\n  Disagreements\n")
	w := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	row(w, "  RECORD\tHUMAN\tJUDGE\tRATIONALE\n")
	for _, d := range res.Disagreements {
		row(w, "  %s\t%s\t%s\t%s\n",
			d.RecordID, verdictWord(d.Human), verdictWord(d.Judge), truncate(d.Rationale, 56))
	}
	_ = w.Flush()
}

// mode names where the judgements came from, because "this cost nothing" and
// "this called a model" are not the same measurement.
func mode(res *judge.Result) string {
	if res.Replay {
		if res.PromptSHA == judge.NoPromptSHA {
			return "computed locally (this goal calls no model)"
		}
		return "replayed from recorded judge responses, prompt " + res.PromptSHA[:12]
	}
	return "live judge calls"
}

// prevalence is what a constant judge would score in raw agreement: the
// majority class share. Printed beside raw agreement so the number a reader
// trusts at a glance arrives with the reason not to.
func prevalence(res *judge.Result) float64 {
	if res.NScored == 0 {
		return math.NaN()
	}
	// roundTo4 for the same reason judge rounds at the source: 1 - 0.4667 is
	// not 0.5333 in binary, and this value is emitted as
	// constant_judge_raw_agreement.
	p := res.Agreement.HumanPositiveRate
	return roundTo4(math.Max(p, 1-p))
}

func ciText(res *judge.Result) string {
	if res.KappaInterval == nil {
		return "no interval"
	}
	return fmt.Sprintf("95%% CI [%s, %s] (%s)",
		num(res.KappaInterval.GetLow()), num(res.KappaInterval.GetHigh()),
		res.KappaInterval.GetMethod())
}

func num(v float64) string {
	if math.IsNaN(v) {
		return "undefined"
	}
	return fmt.Sprintf("%.3f", v)
}

func verdictWord(passed bool) string {
	if passed {
		return "pass"
	}
	return "fail"
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// wrapFix keeps a long fix line readable without a terminal-width dependency.
func wrapFix(s string) string {
	const width = 74
	var b strings.Builder
	col := 0
	for i, word := range strings.Fields(s) {
		if i > 0 {
			if col+1+len(word) > width {
				b.WriteString("\n       ")
				col = 0
			} else {
				b.WriteString(" ")
				col++
			}
		}
		b.WriteString(word)
		col += len(word)
	}
	return b.String()
}
