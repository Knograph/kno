package cli

import (
	"fmt"
	"io"

	"github.com/knograph/kno/adapters/evals/jsonl"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// renderValue writes the Value report, human or machine-readable.
//
// Rendered BEFORE the run error is returned, for the same reason as baseline:
// a budget stop or an interruption still shows what it accomplished.
func renderValue(
	out io.Writer,
	f valueFlags,
	_ core.ValueOptions,
	res *core.ValueResult,
	_ jsonl.SplitCounts,
	runID string,
) error {
	if f.jsonOut {
		return renderValueJSON(out, res, runID)
	}
	return renderValueHuman(out, res, runID)
}

// renderValueHuman prints one line per Asset: the delta with its interval,
// the harm bound, and the reason when there is no number.
func renderValueHuman(out io.Writer, res *core.ValueResult, runID string) error {
	if _, err := fmt.Fprintf(out, "Value run %s (%s)\n\n", runID, res.Status); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "%-12s  %-28s  %-18s  %s\n",
		"ASSET", "DELTA (95% CI)", "CONTROL", "NOTE"); err != nil {
		return err
	}
	for _, v := range res.Valuations {
		delta := "—"
		control := "—"
		note := ""
		switch v.GetNotMeasured() {
		case knov1.RejectionReason_REJECTION_REASON_IRRELEVANT:
			note = "routed to nothing"
		case knov1.RejectionReason_REJECTION_REASON_BUDGET_EXHAUSTED:
			note = "budget exhausted mid-measurement; --resume continues"
		case knov1.RejectionReason_REJECTION_REASON_UNDERPOWERED:
			note = "sample too small or ragged to form an interval"
		default:
			if iv := v.GetDeltaInterval(); iv != nil {
				delta = fmt.Sprintf("%+.4f  [%+.4f, %+.4f]", v.GetDeltaGoal(), iv.GetLow(), iv.GetHigh())
			}
			if iv := v.GetControlInterval(); iv != nil {
				control = fmt.Sprintf("low %+.4f", iv.GetLow())
				if v.GetControlUnderpowered() {
					control += " (underpowered)"
				}
			}
			if v.GetNDropped() > 0 {
				note = fmt.Sprintf("%d of %d pairs dropped", v.GetNDropped(), v.GetNPairs()+v.GetNDropped())
			}
		}
		if _, err := fmt.Fprintf(out, "%-12s  %-28s  %-18s  %s\n",
			v.GetAssetId(), delta, control, note); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(out, "\nScores and traces are recorded. `kno purge` removes trace content when you no longer need it.")
	return err
}
