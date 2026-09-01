package core

import (
	"context"
	"sort"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/interval"
	"github.com/knograph/kno/store"
)

// validationFor computes the Validation from what is DURABLY RECORDED, not
// from in-memory results.
//
// The pattern valuationFor established, and here it is what makes a resume
// correct rather than merely tidy: a run stopped by its cap part-way through
// leaves paid measurements and no Validation, and the process that finishes
// must compute over BOTH processes' measurements. Reading its own results
// would compute the headline number over half a sample and write it as
// finished.
//
// The gain ships only beside its interval. A nil interval is a real answer
// ("this sample cannot support one") and the Validation says so with
// not_measured = UNDERPOWERED rather than emitting a bare number — the shape
// prime directive 5 exists to ban.
//
// stats/portfolio.Correct is deliberately NOT applied here, and this comment
// is the reason a future reader should not "fix" that. Correct is Bonferroni
// over n_screened — the number of Assets a SELECTION considered. Validate
// screens nothing; it makes one pre-registered comparison, decided before any
// holdout Case was read. A Bonferroni factor here would widen the one interval
// in the product that has earned its nominal coverage. The multiplicity that
// does apply is repeated holdout use across portfolios, and that is counted in
// holdout_use_index and disclosed, rather than corrected.
func (o ValidateOptions) validationFor(ctx context.Context, plan *validatePlan) (*Validation, error) {
	recorded, err := o.Store.Measurements(ctx, o.RunID, o.SelectRunID)
	if err != nil {
		return nil, err
	}

	v := &Validation{
		RunId:            o.RunID,
		SelectRunId:      o.SelectRunID,
		ValueRunId:       plan.valueRunID,
		BaselineRunId:    plan.baselineRunID,
		Trials:           plan.trials,
		HoldoutUseIndex:  plan.useIndex,
		ContextOnly:      o.ContextOnly,
		IncompleteReason: plan.incompleteReason,
		//nolint:gosec // bounded by the eval set: a holdout cannot hold 2^31 Cases
		HoldoutCaseCount:     int32(len(plan.cases)),
		HoldoutUnderpowered:  plan.underpowered(o.MinHoldout),
		DevEstimatedGain:     plan.portfolio.GetDevEstimatedGain(),
		DevEstimatedInterval: plan.portfolio.GetDevEstimatedInterval(),
	}
	if len(plan.excluded) > 0 {
		v.ExcludedAssetIds = append([]string(nil), plan.excluded...)
	}

	treatment := byCase(recorded, store.ArmTreatment)
	control := byCase(recorded, store.ArmControl)

	ids := make([]string, 0, len(plan.cases))
	for _, c := range plan.cases {
		ids = append(ids, c.GetId())
	}
	// Sorted, so a recomputation after a resume produces the identical vector
	// and therefore the identical interval. Map iteration order would make a
	// resumed Validation differ from the one the first process would have
	// written, in the last decimal place, for no reason a reader could find.
	sort.Strings(ids)

	// Direction is applied exactly here, once. stats/interval's contract is
	// "deltas arrive sign-corrected by the caller" — positive is better inside
	// that package, and the report layer un-negates MINIMIZE deltas for
	// display only.
	dir := 1.0
	if o.Goal.Direction() == knov1.Direction_DIRECTION_MINIMIZE {
		dir = -1.0
	}

	var perCase [][]float64
	var treatmentMeans, controlMeans []float64
	dropped := 0
	for _, id := range ids {
		tr, ct := treatment[id], control[id]
		if len(tr) == 0 || len(ct) == 0 {
			// A Case that errored in EITHER arm contributes to neither. Both
			// directions matter: dropping a Case whose treatment arm errored
			// removes exactly the Cases where a long injected Portfolio timed
			// out, so the gain is biased upward — which is why the count
			// travels with the number rather than being absorbed into it.
			dropped++
			continue
		}
		var deltas, trVals, ctVals []float64
		for _, trial := range sortedTrials(unionKeys(tr, ct)) {
			a, okA := tr[trial]
			b, okB := ct[trial]
			if !okA || !okB {
				// One side lost this trial. Pairing it positionally against
				// the next one would manufacture a draw that never happened.
				dropped++
				continue
			}
			deltas = append(deltas, dir*(a-b))
			trVals = append(trVals, a)
			ctVals = append(ctVals, b)
		}
		if len(deltas) == 0 {
			continue
		}
		perCase = append(perCase, deltas)
		treatmentMeans = append(treatmentMeans, mean(trVals))
		controlMeans = append(controlMeans, mean(ctVals))
	}

	v.MeasuredCaseCount = int32(len(perCase)) //nolint:gosec // bounded by the eval set
	v.NDropped = int32(dropped)

	var iv *knov1.Interval
	if len(perCase) > 0 {
		iv = interval.PairedTrials(perCase, o.Goal.Domain(), interval.DefaultLevel)
	}
	if iv == nil {
		// Measurements may have existed, but no interval could be formed —
		// fewer than two pairs, or ragged per-Case attrition. Reporting the
		// surviving number alone would present "whatever survived" as the
		// measured answer.
		v.NotMeasured = knov1.RejectionReason_REJECTION_REASON_UNDERPOWERED
		v.Verdict = knov1.ValidationVerdict_VALIDATION_VERDICT_UNMEASURED
		return v, nil
	}

	gain := meanOfMeans(perCase)
	v.HoldoutGain = &gain
	v.HoldoutInterval = iv
	tScore := meanOf(treatmentMeans)
	cScore := meanOf(controlMeans)
	v.TreatmentScore = &tScore
	v.ControlScore = &cScore
	v.Verdict = validationVerdictFor(iv)
	return v, nil
}

// validationVerdictFor keys the verdict on the INTERVAL and never on the sign of the
// point estimate — the rule docs/what-the-numbers-mean.md already states for
// every other number this tool reports.
//
// An interval crossing zero means "not enough evidence at this sample size",
// not "it failed". Collapsing that into a failure would make a 20-Case holdout
// block every deploy forever and train people to pass --force, at which point
// the gate has stopped meaning anything. A gate that wants proof of gain asks
// for it with --require-gain, which is a CLI decision about exit codes rather
// than a change to what was measured.
func validationVerdictFor(iv *knov1.Interval) knov1.ValidationVerdict {
	switch {
	case iv == nil:
		return knov1.ValidationVerdict_VALIDATION_VERDICT_UNMEASURED
	case iv.GetLow() > 0:
		return knov1.ValidationVerdict_VALIDATION_VERDICT_CONFIRMED
	case iv.GetHigh() <= 0:
		return knov1.ValidationVerdict_VALIDATION_VERDICT_NOT_CONFIRMED
	default:
		return knov1.ValidationVerdict_VALIDATION_VERDICT_INCONCLUSIVE
	}
}

// mean averages one Case's trials.
func mean(vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	var total float64
	for _, v := range vs {
		total += v
	}
	return total / float64(len(vs))
}

// meanOf averages one value per Case.
//
// Not the mean of every measurement: that weights a Case whose trials all
// completed above one that lost a trial to a timeout, so the reported arm mean
// would move with the transport rather than with the Portfolio.
func meanOf(vs []float64) float64 { return mean(vs) }
