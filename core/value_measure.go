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
//
// Deltas ship only beside their intervals, for both numbers. A nil interval is
// a real answer ("this sample cannot support one") and the Valuation says so
// with not_measured=UNDERPOWERED rather than emitting a bare delta — the shape
// prime directive 5 exists to ban, and the schema godocs now enforce it.
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
		//nolint:gosec // bounded by the eval set: a dev split cannot hold 2^31 Cases
		NRouted: int32Ptr(int32(len(routing.CaseIDs))),
		//nolint:gosec // bounded by the eval set, same argument as n_routed
		NDev: int32Ptr(int32(plan.EligibleCases)),
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

	// Grouped by Case and trial, then averaged per Case, rather than flattened
	// into one list of differences. stats/interval refuses the flattened shape
	// on purpose: n trials of one Case are not n independent observations, and
	// counting them as such narrows every interval by about sqrt(trials) while
	// reporting the same confidence level.
	treatment := byCase(recorded, store.ArmTreatment)
	control := byCase(recorded, store.ArmControl)

	// Direction is applied exactly here, once, to every delta. stats/interval's
	// contract is "deltas arrive sign-corrected for the Goal's direction by the
	// caller" — positive is better inside the interval package, and the report
	// layer un-negates MINIMIZE deltas for display only.
	goalDeltas, goalDropped := pairs(routing.CaseIDs, treatment, control, baseline, routing.FreshControlArm, o.Goal.Direction())
	controlDeltas, controlDropped := pairs(plan.ControlCaseIDs, treatment, nil, baseline, false, o.Goal.Direction())

	//nolint:gosec // bounded by the eval set: pair and drop counts cannot exceed the Case count
	v.NPairs = int32Ptr(int32(len(goalDeltas)))
	//nolint:gosec // bounded by the eval set, same argument as n_pairs
	v.NDropped = int32Ptr(int32(goalDropped + controlDropped))

	var goalIv, controlIv *knov1.Interval
	if len(goalDeltas) > 0 {
		goalIv = interval.PairedTrials(goalDeltas, o.Goal.Domain(), defaultLevel)
	}
	// The control arm answers a different question from the routed one — "did
	// this break something else" — and it is one-sided. A two-sided interval
	// there answers "is the control effect distinguishable from zero", and at
	// small M that spans zero for a real regression, which the shipped
	// colouring rule renders as no regression.
	//
	// The bound consumes per-Case means, never the flattened per-trial values:
	// flattened deltas share one recorded baseline per Case — correlated within
	// Case — and HarmBound treats every value as independent, so flat plus
	// plan.Trials would come out about sqrt(Trials) too narrow in the direction
	// that clears harmful assets. Trials is passed through for method dispatch
	// only, over a slice of length Case-count.
	if len(controlDeltas) > 0 {
		if means, trials, ok := perCaseMeans(controlDeltas); ok {
			controlIv = interval.HarmBound(means, o.Goal.Domain(), trials, defaultLevel)
		}
	}

	if goalIv != nil {
		v.DeltaGoal = meanOfMeans(goalDeltas)
		v.DeltaInterval = goalIv
	}
	if controlIv != nil {
		v.DeltaControl = meanOfMeans(controlDeltas)
		v.ControlInterval = controlIv
	}
	// The underpowered gate: measurements existed, but no interval could be
	// formed around at least one of them — fewer than two pairs, or ragged
	// per-Case attrition. Reporting the surviving number alone would present
	// "whatever survived" as the measured answer; the schema's fallback is a
	// named reason instead of a refusal (plan §2.2), and the attrition
	// statement n_pairs/n_dropped travels with it.
	if (len(goalDeltas) > 0 && goalIv == nil) || (len(controlDeltas) > 0 && controlIv == nil) {
		v.NotMeasured = knov1.RejectionReason_REJECTION_REASON_UNDERPOWERED
		v.DeltaGoal, v.DeltaInterval, v.DeltaControl, v.ControlInterval = 0, nil, 0, nil
	}
	if plan.ControlUnderpowered {
		under := true
		v.ControlUnderpowered = &under
	}
	return v, nil
}

// byCase groups one arm's recorded scores by Case ID and trial number.
//
// Trial-keyed, not positional, because pairing joins on the trial NUMBER:
// after a trial is lost on one side only, positional pairing would align
// treatment-trial-3 with control-trial-2 and the pair would claim to compare
// draws that never happened together.
//
// Errored measurements are excluded rather than scored as zero. An error is not
// a score — the same rule the Baseline stage's counts follow — and a zero here
// would be a measurement claiming the Asset made the agent fail, when what
// happened is that the call did not complete.
func byCase(recorded []store.RecordedMeasurement, arm store.Arm) map[string]map[int32]float64 {
	out := make(map[string]map[int32]float64)
	for _, m := range recorded {
		if m.Key.Arm != arm || m.Err != "" || m.Unrecoverable {
			continue
		}
		scores, ok := out[m.Key.CaseID]
		if !ok {
			scores = make(map[int32]float64)
			out[m.Key.CaseID] = scores
		}
		scores[m.Key.Trial] = m.Score
	}
	return out
}

// pairs builds one delta vector per Case, trial joined on trial number.
//
// control is the fresh control arm when there is one; otherwise the recorded
// baseline is the control, which is valid exactly where the selection did not
// condition on the baseline outcome — the reserved partition, and any run where
// routing made no outcome-based selection at all.
//
// A Case missing from either side is DROPPED, not defaulted, and so is a trial
// present on one side only. Both directions matter and neither is symmetric:
// dropping a Case whose TREATMENT arm errored removes exactly the Cases where
// the Asset was most harmful — a long injected context that timed out — so the
// delta is biased upward, and the bias grows with Asset size, which is
// delta_per_cost's numerator against its own denominator. The returned drop
// count travels with the number rather than being absorbed into it.
func pairs(
	caseIDs []string,
	treatment, control map[string]map[int32]float64,
	baseline map[string]store.CaseScore,
	fresh bool,
	direction knov1.Direction,
) ([][]float64, int) {
	dir := 1.0
	if direction == knov1.Direction_DIRECTION_MINIMIZE {
		dir = -1.0
	}
	var out [][]float64
	dropped := 0
	// Sorted, so a recomputation after a resume produces the identical vector
	// and therefore the identical interval. Map iteration order would make a
	// resumed Valuation differ from the one the first process would have
	// written, in the last decimal place, for no reason a reader could find.
	ids := append([]string(nil), caseIDs...)
	sort.Strings(ids)

	for _, id := range ids {
		tr := treatment[id]
		if len(tr) == 0 {
			dropped++
			continue
		}
		var deltas []float64
		if fresh {
			ct := control[id]
			if len(ct) == 0 {
				dropped++
				continue
			}
			// Iterated over the UNION of both sides' trial numbers, so a trial
			// lost on EITHER side is counted once. A control measurement with
			// no treatment partner is paid-for noise the delta cannot use, and
			// hiding it would under-report the attrition the bias lives in.
			for _, trial := range sortedTrials(unionKeys(tr, ct)) {
				a, okA := tr[trial]
				b, okB := ct[trial]
				if !okA || !okB {
					// One side lost this trial: no pair exists for it, and
					// pairing it positionally against the next one would
					// manufacture a draw that never happened.
					dropped++
					continue
				}
				deltas = append(deltas, dir*(a-b))
			}
		} else {
			b, ok := baseline[id]
			if !ok || b.Unrecoverable {
				dropped++
				continue
			}
			for _, trial := range sortedTrials(tr) {
				deltas = append(deltas, dir*(tr[trial]-b.Value))
			}
		}
		if len(deltas) > 0 {
			out = append(out, deltas)
		}
	}
	return out, dropped
}

// sortedTrials returns one arm's trial numbers in ascending order, so a
// recomputation after a resume builds the identical vector.
func sortedTrials(scores map[int32]float64) []int32 {
	trials := make([]int32, 0, len(scores))
	for t := range scores {
		trials = append(trials, t)
	}
	sort.Slice(trials, func(i, j int) bool { return trials[i] < trials[j] })
	return trials
}

// unionKeys merges both arms' trial numbers so the pairing loop can count a
// trial lost on either side exactly once.
func unionKeys(a, b map[int32]float64) map[int32]float64 {
	out := make(map[int32]float64, len(a)+len(b))
	for t, v := range a {
		out[t] = v
	}
	for t, v := range b {
		out[t] = v
	}
	return out
}

// meanOfMeans averages each Case's trials, then averages the Cases.
//
// Not the mean of every measurement: that weights a Case whose trials all
// completed above one that lost a trial to a timeout, so the reported delta
// would move with the transport rather than with the Asset.
func meanOfMeans(perCase [][]float64) float64 {
	means, _, ok := perCaseMeans(perCase)
	if !ok {
		return 0
	}
	var total float64
	for _, m := range means {
		total += m
	}
	return total / float64(len(means))
}

// perCaseMeans collapses per-Case trial vectors to one value per Case.
//
// Mirrors stats/interval's own shape rule: it refuses ragged input rather than
// averaging over different denominators, because a Case measured twice and one
// measured five times contribute differently to the mean. Trials is returned
// for the interval methods that dispatch on it — over a slice of length
// Case-count, which is the only legal use; flattened per-trial deltas plus a
// trials count is the shape that inflates n.
func perCaseMeans(perCase [][]float64) (means []float64, trials int, ok bool) {
	if len(perCase) == 0 {
		return nil, 0, false
	}
	trials = len(perCase[0])
	if trials == 0 {
		return nil, 0, false
	}
	means = make([]float64, len(perCase))
	for i, tr := range perCase {
		if len(tr) != trials {
			return nil, 0, false
		}
		var sum float64
		for _, v := range tr {
			sum += v
		}
		means[i] = sum / float64(trials)
	}
	return means, trials, true
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
//
// Mirrors Plan.Measurements exactly: an Asset routed to nothing costs no
// measurements here, control partition included — there is nothing to test for
// harm, because the Asset is never put in front of the agent at all.
func measurementsFor(routing value.AssetRouting, plan *value.Plan, assetID string) []store.MeasurementKey {
	if len(routing.CaseIDs) == 0 {
		return nil
	}
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
