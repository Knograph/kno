package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/knograph/kno/adapters/evals/split"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
)

// renderValidate writes the Validate report, human or machine-readable.
//
// Rendered BEFORE the run error is returned, for the same reason as baseline
// and value: a budget stop or an interruption still shows what it
// accomplished, and what it cost.
func renderValidate(
	out io.Writer,
	f validateFlags,
	res *core.ValidateResult,
	quote core.ValidateQuote,
	counts split.Counts,
	restored budget.Spend,
) error {
	// The stored gain is sign-corrected — positive is better, whatever the
	// Goal's direction — and the display un-negates MINIMIZE once, here, so
	// the report reads in the Goal's own units.
	dir := 1.0
	if res.GoalDirection == knov1.Direction_DIRECTION_MINIMIZE {
		dir = -1.0
	}
	if f.jsonOut {
		return renderValidateJSON(out, res, quote, counts, dir, f)
	}
	return renderValidateHuman(out, res, quote, counts, dir, f, restored)
}

// renderValidateHuman prints the holdout number, what it claims, and what it
// does not.
func renderValidateHuman(
	out io.Writer,
	res *core.ValidateResult,
	quote core.ValidateQuote,
	counts split.Counts,
	dir float64,
	f validateFlags,
	_ budget.Spend,
) error {
	var b strings.Builder

	if res.NothingToValidate {
		fmt.Fprintf(&b, "Validate run %s\n\n", res.RunID)
		b.WriteString("  The Portfolio selected nothing this stage can measure, so there is " +
			"nothing to validate.\n")
		b.WriteString("  No agent call was made and the holdout was not opened — it stays " +
			"untouched for the Portfolio that earns it.\n")
		_, err := io.WriteString(out, b.String())
		return err
	}

	fmt.Fprintf(&b, "Validate run %s (%s)\n", res.RunID, statusName(res.Status))
	fmt.Fprintf(&b, "  portfolio  %s — %d asset(s) injected as a set, in rank order\n",
		f.selectRunID, res.AssetCount)
	fmt.Fprintf(&b, "  holdout    %d Case(s), %s\n", res.HoldoutCases, quote.Derivation())
	if len(quote.ExcludedAssetIDs) > 0 {
		fmt.Fprintf(&b, "  SUBSET     --context-only: %d entr(ies) excluded (%s). This number "+
			"covers a SUBSET of the Portfolio.\n",
			len(quote.ExcludedAssetIDs), strings.Join(quote.ExcludedAssetIDs, ", "))
	}

	v := res.Validation
	if v == nil {
		b.WriteString("\n  No holdout number: the run did not complete, so nothing was written.\n")
		b.WriteString("  A partial peek is not a validation. The holdout is recorded as used " +
			"for this Portfolio, because a run that stopped part-way HAS already looked.\n")
		fmt.Fprintf(&b, "  Continue it with `kno validate --resume --run-id %s`.\n", res.RunID)
		b.WriteString("\n")
		if err := writeStringAndSpend(out, b.String(), res.Spent, f.resume); err != nil {
			return err
		}
		return nil
	}

	b.WriteString("\n")
	if iv := v.GetHoldoutInterval(); iv != nil {
		fmt.Fprintf(&b, "  holdout gain  %+.4f  [%+.4f, %+.4f]   verdict %s\n",
			dir*v.GetHoldoutGain(), dir*iv.GetLow(), dir*iv.GetHigh(),
			verdictName(v.GetVerdict()))
		fmt.Fprintf(&b, "  measured on   %d of %d holdout Case(s) in both arms, %d dropped, %d trial(s)\n",
			v.GetMeasuredCaseCount(), v.GetHoldoutCaseCount(), v.GetNDropped(), v.GetTrials())
		if div := v.GetDevEstimatedInterval(); div != nil {
			fmt.Fprintf(&b, "  dev estimate  %+.4f  [%+.4f, %+.4f]   (selection-time; winner's-curse inflated)\n",
				dir*v.GetDevEstimatedGain(), dir*div.GetLow(), dir*div.GetHigh())
			fmt.Fprintf(&b, "  shrinkage     %+.4f dev -> holdout. EXPECTED, and not evidence of "+
				"interaction: the dev figure was chosen on the dev slice, and the two "+
				"figures measure different populations.\n",
				dir*(v.GetDevEstimatedGain()-v.GetHoldoutGain()))
		}
	} else {
		b.WriteString("  holdout gain  none — the sample could not support an interval " +
			"(fewer than two usable pairs, or ragged attrition across the arms)\n")
		fmt.Fprintf(&b, "  measured on   %d of %d holdout Case(s) in both arms, %d dropped\n",
			v.GetMeasuredCaseCount(), v.GetHoldoutCaseCount(), v.GetNDropped())
		b.WriteString("  A gain is never reported without its interval, so no number is " +
			"reported here rather than a number you could not size.\n")
	}

	if v.GetHoldoutUnderpowered() {
		fmt.Fprintf(&b, "\n  UNDERPOWERED: %d holdout Case(s) is below %d, so the interval "+
			"above is wide by construction. The run still executed; the caveat travels "+
			"with the number.\n", v.GetHoldoutCaseCount(), split.MinHoldout)
	}
	if n := v.GetHoldoutUseIndex(); n > 1 {
		fmt.Fprintf(&b, "\n  this holdout has measured %d portfolios; the interval above is "+
			"NOT corrected for that\n", n)
	} else {
		b.WriteString("\n  this holdout has measured 1 portfolio\n")
	}
	if reason := v.GetIncompleteReason(); reason != "" {
		fmt.Fprintf(&b, "  source Value run was incomplete: %s\n", reason)
	}

	b.WriteString("\n  What this claims: with this Portfolio in context, the agent scored the " +
		"figure above\n  better on Cases it had never been measured on, under CONTEXT " +
		"INJECTION — an upper\n  bound on what retrieval would deliver. It is unbiased for the " +
		"effect of THIS Portfolio\n  on the holdout population. It is not a corrected version " +
		"of the dev estimate, and it\n  is not the effect of the best achievable portfolio.\n")
	b.WriteString("  Pairwise interaction detection is not in this release, so no suspect " +
		"pairs are\n  reported — see docs/what-the-numbers-mean.md.\n")

	if counts.Total() > 0 {
		fmt.Fprintf(&b, "\n  split      %d dev / %d holdout\n", counts.Dev, counts.Holdout)
	}
	b.WriteString("\n")
	return writeStringAndSpend(out, b.String(), res.Spent, f.resume)
}

// writeStringAndSpend writes the page and then the shared spend block.
//
// The spend block goes through cli/spend.go's one renderer, never a private
// copy: the human line and the --json block disagreeing about what a stage
// cost is worse than either one being absent.
func writeStringAndSpend(out io.Writer, page string, spent budget.Spend, resumed bool) error {
	if _, err := io.WriteString(out, page); err != nil {
		return fmt.Errorf("writing the validate report: %w", err)
	}
	return spendLines(out, spent, 0, resumed)
}

// verdictName renders a verdict in the vocabulary's words.
//
// Named rather than numeric for statusName's reason: a jq pipeline branching
// on 1 breaks the day an enum value is inserted ahead of it, and a reader
// should not need the proto to interpret the document (docs/debt.md#44).
func verdictName(v knov1.ValidationVerdict) string {
	switch v {
	case knov1.ValidationVerdict_VALIDATION_VERDICT_CONFIRMED:
		return "confirmed"
	case knov1.ValidationVerdict_VALIDATION_VERDICT_INCONCLUSIVE:
		return "inconclusive"
	case knov1.ValidationVerdict_VALIDATION_VERDICT_NOT_CONFIRMED:
		return "not_confirmed"
	case knov1.ValidationVerdict_VALIDATION_VERDICT_UNMEASURED:
		return "unmeasured"
	default:
		return "unspecified"
	}
}

// validateExit maps the run to its exit code.
//
// Keyed on the INTERVAL and never on the sign of the point estimate:
//
//	low > 0            confirmed      0
//	low <= 0 <= high   inconclusive   0, or 3 with --require-gain
//	high <= 0          not_confirmed  3
//	no interval        unmeasured     0, or 3 with --require-gain
//
// An interval crossing zero means "not enough evidence at this sample size",
// not "it failed", so it must not block a deploy by default — a 20-Case
// holdout would then block every deploy forever and train people to pass
// --force, at which point the gate has stopped meaning anything. A gate that
// wants proof of gain asks for it. high <= 0 is a demonstrated
// non-improvement and blocks unconditionally.
func validateExit(res *core.ValidateResult, requireGain bool) error {
	switch res.Status {
	case knov1.RunStatus_RUN_STATUS_BUDGET_STOPPED:
		return errs.ErrBudgetExceeded.Wrap(fmt.Errorf(
			"the budget stopped validate run %s before every holdout Case was measured "+
				"in both arms; no holdout number was written, because a number over "+
				"whatever got measured first is not a validation", res.RunID))
	case knov1.RunStatus_RUN_STATUS_INTERRUPTED:
		return errs.ErrInterrupted.Wrap(fmt.Errorf(
			"validate run %s was interrupted before it finished", res.RunID))
	case knov1.RunStatus_RUN_STATUS_COMPLETED, knov1.RunStatus_RUN_STATUS_FAILED,
		knov1.RunStatus_RUN_STATUS_RUNNING, knov1.RunStatus_RUN_STATUS_UNSPECIFIED:
	}
	if res.Validation == nil {
		return nil
	}
	switch res.Validation.GetVerdict() {
	case knov1.ValidationVerdict_VALIDATION_VERDICT_NOT_CONFIRMED:
		return errValidationFailed(res.RunID,
			"the holdout interval sits at or below zero: this Portfolio demonstrably did "+
				"not improve the agent on Cases it had never been measured on")
	case knov1.ValidationVerdict_VALIDATION_VERDICT_INCONCLUSIVE:
		if requireGain {
			return errValidationFailed(res.RunID,
				"the holdout interval straddles zero, and --require-gain asks for a "+
					"demonstrated gain. This is not a measured failure — it is not enough "+
					"evidence at this holdout size")
		}
	case knov1.ValidationVerdict_VALIDATION_VERDICT_UNMEASURED:
		if requireGain {
			return errValidationFailed(res.RunID,
				"no holdout interval could be formed, and --require-gain asks for a "+
					"demonstrated gain")
		}
	case knov1.ValidationVerdict_VALIDATION_VERDICT_CONFIRMED,
		knov1.ValidationVerdict_VALIDATION_VERDICT_UNSPECIFIED:
	}
	return nil
}

// errValidationFailed is the exit-3 refusal a deploy gate blocks on.
//
// errs.ExitValidationFailed has existed since v0.1 with the godoc "the
// portfolio did not hold up against the holdout. This is the code a deploy
// gate should block on." Nothing had ever returned it; this is its first
// caller.
func errValidationFailed(runID, why string) error {
	return (&errs.Actionable{
		Code:     "VALIDATION_FAILED",
		Message:  "the portfolio did not hold up against the holdout",
		Fix:      fmt.Sprintf("read the full page with `kno report --validate-run-id %s`", runID),
		ExitCode: errs.ExitValidationFailed,
	}).Wrap(fmt.Errorf("%s", why))
}
