package core

import (
	"context"
	"fmt"
	"sort"

	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/interval"
	"github.com/knograph/kno/store"
)

// defaultLevel is the confidence level every interval this stage reports is
// computed at, until a flag exposes it.
const defaultLevel = 0.95

// valuationFor computes one Asset's Valuation from what is DURABLY RECORDED,
// not from in-memory results.
//
// The pattern CaseExecution established, and here it is what makes a resume
// correct rather than merely tidy: a run stopped by its cap part-way through an
// Asset leaves paid measurements and no Valuation, and the process that
// finishes the Asset must compute over BOTH processes' measurements. Reading
// its own results would compute a delta over half a sample and write it as a
// finished number.
func (o ValueOptions) valuationFor(
	ctx context.Context,
	asset *Asset,
	routing value.AssetRouting,
	plan *value.Plan,
	baseline map[string]store.CaseScore,
) (*Valuation, error) {
	recorded, err := o.Store.Measurements(ctx, o.RunID, asset.GetId())
	if err != nil {
		return nil, err
	}

	v := &Valuation{
		AssetId: asset.GetId(),
		Trials:  plan.Trials,
		Mode:    knov1.InjectionMode_INJECTION_MODE_CONTEXT,
		NRouted: int32Ptr(int32(len(routing.CaseIDs))),
		NDev:    int32Ptr(int32(len(plan.ControlCaseIDs))),
	}
	if len(routing.CaseIDs) == 0 {
		// not_measured carries the REASON, and its zero value is UNSPECIFIED —
		// so "this Asset was measured" and "this Asset was not measured, and
		// here is why" are one field rather than a bool plus a reason that can
		// disagree with it.
		v.NotMeasured = routing.NotMeasuredReason
		return v, nil
	}
	v.CaseIds = append([]string(nil), routing.CaseIDs...)

	// Grouped by Case, then averaged, rather than flattened into one list of
	// differences. stats/interval refuses the flattened shape on purpose: n
	// trials of one Case are not n independent observations, and counting them
	// as such narrows every interval by about sqrt(trials) while reporting the
	// same confidence level.
	treatment := byCase(recorded, store.ArmTreatment)
	control := byCase(recorded, store.ArmControl)

	goalDeltas := pairs(routing.CaseIDs, treatment, control, baseline, routing.FreshControlArm)
	if len(goalDeltas) > 0 {
		v.DeltaGoal = meanOfMeans(goalDeltas)
		v.DeltaInterval = interval.PairedTrials(goalDeltas, o.Goal.Domain(), defaultLevel)
	}

	// The control arm answers a different question from the routed one — "did
	// this break something else" — and it is one-sided. A two-sided interval
	// there answers "is the control effect distinguishable from zero", and at
	// small M that spans zero for a real regression, which the shipped
	// colouring rule renders as no regression.
	controlDeltas := pairs(plan.ControlCaseIDs, treatment, nil, baseline, false)
	if len(controlDeltas) > 0 {
		v.DeltaControl = meanOfMeans(controlDeltas)
		flat := flatten(controlDeltas)
		v.ControlInterval = interval.HarmBound(flat, o.Goal.Domain(), int(plan.Trials), defaultLevel)
	}
	if plan.ControlUnderpowered {
		under := true
		v.ControlUnderpowered = &under
	}
	return v, nil
}

// byCase groups one arm's recorded scores by Case ID.
//
// Errored measurements are excluded rather than scored as zero. An error is not
// a score — the same rule the Baseline stage's counts follow — and a zero here
// would be a measurement claiming the Asset made the agent fail, when what
// happened is that the call did not complete.
func byCase(recorded []store.RecordedMeasurement, arm store.Arm) map[string][]float64 {
	out := make(map[string][]float64)
	for _, m := range recorded {
		if m.Key.Arm != arm || m.Err != "" || m.Unrecoverable {
			continue
		}
		out[m.Key.CaseID] = append(out[m.Key.CaseID], m.Score)
	}
	return out
}

// pairs builds one delta vector per Case.
//
// control is the fresh control arm when there is one; otherwise the recorded
// baseline is the control, which is valid exactly where the selection did not
// condition on the baseline outcome — the reserved partition, and any run where
// routing made no outcome-based selection at all.
//
// A Case missing from either side is DROPPED, not defaulted. Both directions
// matter and neither is symmetric: dropping a Case whose TREATMENT arm errored
// removes exactly the Cases where the Asset was most harmful — a long injected
// context that timed out — so the delta is biased upward, and the bias grows
// with Asset size, which is delta_per_cost's numerator against its own
// denominator. The count of drops travels with the number rather than being
// absorbed into it.
func pairs(
	caseIDs []string,
	treatment, control map[string][]float64,
	baseline map[string]store.CaseScore,
	fresh bool,
) [][]float64 {
	var out [][]float64
	// Sorted, so a recomputation after a resume produces the identical vector
	// and therefore the identical interval. Map iteration order would make a
	// resumed Valuation differ from the one the first process would have
	// written, in the last decimal place, for no reason a reader could find.
	ids := append([]string(nil), caseIDs...)
	sort.Strings(ids)

	for _, id := range ids {
		tr := treatment[id]
		if len(tr) == 0 {
			continue
		}
		var deltas []float64
		if fresh {
			ct := control[id]
			if len(ct) == 0 {
				continue
			}
			// Paired by position within the Case: trial i against trial i.
			n := min(len(tr), len(ct))
			for i := range n {
				deltas = append(deltas, tr[i]-ct[i])
			}
		} else {
			b, ok := baseline[id]
			if !ok || b.Unrecoverable {
				continue
			}
			for _, t := range tr {
				deltas = append(deltas, t-b.Value)
			}
		}
		if len(deltas) > 0 {
			out = append(out, deltas)
		}
	}
	return out
}

// meanOfMeans averages each Case's trials, then averages the Cases.
//
// Not the mean of every measurement: that weights a Case whose trials all
// completed above one that lost a trial to a timeout, so the reported delta
// would move with the transport rather than with the Asset.
func meanOfMeans(perCase [][]float64) float64 {
	if len(perCase) == 0 {
		return 0
	}
	var total float64
	for _, deltas := range perCase {
		var sum float64
		for _, d := range deltas {
			sum += d
		}
		total += sum / float64(len(deltas))
	}
	return total / float64(len(perCase))
}

// flatten collapses per-Case vectors for the interval functions that take a
// trials count alongside a flat slice.
func flatten(perCase [][]float64) []float64 {
	var out []float64
	for _, d := range perCase {
		out = append(out, d...)
	}
	return out
}

// int32Ptr is for the proto's optional int32 fields, whose presence is what
// distinguishes "measured against zero Cases" from "this stage does not report
// that number".
func int32Ptr(v int32) *int32 { return &v }

// measurementsFor lists every measurement one Asset's Plan calls for.
//
// Built once and consulted against what is already recorded, so a resume skips
// what it paid for rather than re-deriving the schedule from scratch and
// hoping the two agree.
func measurementsFor(routing value.AssetRouting, plan *value.Plan, assetID string) []store.MeasurementKey {
	var keys []store.MeasurementKey
	add := func(caseID string, arm store.Arm) {
		for trial := int32(1); trial <= plan.Trials; trial++ {
			keys = append(keys, store.MeasurementKey{
				AssetID: assetID, CaseID: caseID, Arm: arm, Trial: trial,
			})
		}
	}
	for _, id := range routing.CaseIDs {
		add(id, store.ArmTreatment)
		if routing.FreshControlArm {
			add(id, store.ArmControl)
		}
	}
	// The harm test: the treatment arm only. Its control is the recorded
	// baseline, which is valid here because the reservation happened before
	// routing and saw no outcome.
	for _, id := range plan.ControlCaseIDs {
		add(id, store.ArmTreatment)
	}
	return keys
}

// assertQuoteBounds is a construction-time check that the schedule the loop
// will execute is no larger than the number the user consented to.
//
// Cheap, and it exists because the plan's own review record catches a missing
// multiplier in this formula twice. A quote that under-states is a consent
// prompt for a smaller run than the one that happens.
func assertQuoteBounds(scheduled int, plan *value.Plan) error {
	if quoted := plan.Measurements(); int64(scheduled) > quoted {
		return fmt.Errorf("value: the schedule holds %d measurements against a quote "+
			"of %d; the figure the user consented to is not a bound on the run",
			scheduled, quoted)
	}
	return nil
}
