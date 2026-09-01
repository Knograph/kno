package bridge

import (
	"context"
	"fmt"
	"sort"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/interval"
	"github.com/knograph/kno/stats/portfolio"
	"github.com/knograph/kno/store"
)

// This file is the pairing and persistence half of the eval seam: which
// Case IDs a group needs, how its per-Case scores are read back and written
// durably, and how a group's Δ_group / Δ_control verdict is computed from
// two score maps. bridge/run.go is the orchestration that calls these at
// the right moments — deploy, measure, teardown, resume.

// unionCaseIDs is every Case ID the all-in group's union pass must score:
// every leave-one-out group's dev Cases, plus the reserved control
// partition — the bridge eval-seam plan's §2: "the union pass scores the
// union of every group's dev Cases plus the control Cases, while its
// endpoint is live. That is the only moment it exists".
func unionCaseIDs(p RunParams) []string {
	set := make(map[string]struct{})
	for _, ids := range p.DevCaseIDs {
		for _, id := range ids {
			set[id] = struct{}{}
		}
	}
	for _, id := range p.ControlCaseIDs {
		set[id] = struct{}{}
	}
	return sortedKeys(set)
}

// groupCaseIDs is the Case IDs one leave-one-out group's own deployed model
// must be invoked over: its cluster's dev Cases (for Δ_group) and the
// reserved control partition (for Δ_control) — the union of the two, since
// EvalRunner.Measure takes one flat ID list rather than a dev/control
// split (see EvalRunner's doc, decided in the plan's §"Both blockers
// resolved", B1).
func groupCaseIDs(p RunParams, group string) []string {
	set := make(map[string]struct{}, len(p.DevCaseIDs[group])+len(p.ControlCaseIDs))
	for _, id := range p.DevCaseIDs[group] {
		set[id] = struct{}{}
	}
	for _, id := range p.ControlCaseIDs {
		set[id] = struct{}{}
	}
	return sortedKeys(set)
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// missingIDs returns the ids in want that have does not hold a score for —
// the Cases a group still has to be measured on before it is fully scored.
// An empty result means fully scored: measureGroup treats len(missing)==0
// as its own "nothing left to do" signal rather than a separate predicate.
// Order follows want, which is already sorted by the callers above.
func missingIDs(want []string, have map[string]float64) []string {
	var out []string
	for _, id := range want {
		if _, ok := have[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

// loadGroupScores reads back one ablation group's durably-recorded per-Case
// scores from the measurements table.
//
// STORE CONVENTION: per-Case bridge scores reuse the measurements table
// with the ablation group in MeasurementKey.AssetID — the bridge
// eval-seam plan's §6, and the same repurposing core/validate_loop.go
// already makes for the Select run ID (measureHoldoutArm's own comment:
// "The measurement key's AssetID is the SELECT RUN ID... it makes the
// measurements table's (run_id, asset_id, case_id, arm, trial) primary key
// work unchanged"). AssetID is vocabulary (CLAUDE.md prime directive 2);
// this reuse is deliberate and documented here, at the call site, exactly
// as that one is.
//
// A Case that errored (or whose score is otherwise unrecoverable) is
// absent from the returned map, not present with a zero — the same
// three-state discipline store.CaseScore documents, and what makes
// missingIDs correct: an errored Case is retried on resume rather than
// silently treated as a zero-valued pass.
func loadGroupScores(ctx context.Context, st store.Store, runID, group string) (map[string]float64, error) {
	recs, err := st.Measurements(ctx, runID, group)
	if err != nil {
		return nil, fmt.Errorf("reading recorded scores for group %s: %w", group, err)
	}
	out := make(map[string]float64, len(recs))
	for _, r := range recs {
		if r.Err != "" || r.Unrecoverable {
			continue
		}
		out[r.Key.CaseID] = r.Score
	}
	return out, nil
}

// armFor reports which Arm one Case's row is recorded under within a
// leave-one-out group's asset_id: ArmTreatment for the group's own dev
// Cases (the transfer question, Δ_group), ArmControl for the reserved
// control partition (the interference question, Δ_control) — mirroring
// store.Arm's Value-stage meaning ("measured with"/"without" the thing
// under test) applied to "was this Case measuring the group's own
// transfer, or measuring interference on Cases it was never trained to
// affect."
//
// The all-in group's union pass does not call this: every one of its rows
// is ArmTreatment uniformly, because the all-in scores are the baseline
// itself rather than either side of a paired comparison — see
// recordAllInScores.
func armFor(id string, devIDs map[string]struct{}) store.Arm {
	if _, ok := devIDs[id]; ok {
		return store.ArmTreatment
	}
	return store.ArmControl
}

// recordScores durably persists group's freshly-measured scores, one
// measurement row per Case, Trial 1 always — EvalRunner's own doc: "bridge
// measures each group's model exactly once per Case, so there is no trial
// dimension to average within a Case."
//
// Idempotent via RecordMeasurement's INSERT OR IGNORE: calling this twice
// for the same (group, Case) — once from inside a production EvalRunner's
// own per-Case durability hook, once here as bridge.Run's own safety net —
// costs nothing and loses nothing.
func recordScores(ctx context.Context, st store.Store, runID, group string, scores map[string]float64, arm func(id string) store.Arm) error {
	for id, score := range scores {
		m := &store.Measurement{
			Key:   store.MeasurementKey{AssetID: group, CaseID: id, Arm: arm(id), Trial: 1},
			Score: &knov1.Score{Value: score, Passed: score > 0},
		}
		if err := st.RecordMeasurement(ctx, runID, m); err != nil {
			return fmt.Errorf("recording the %s group's score for case %s: %w", group, id, err)
		}
	}
	return nil
}

// pairDeltas computes Δ = allIn[id] - group[id] for every id in ids present
// on BOTH sides. A Case the all-in model scored but the ablation model
// errored on (or vice versa) is DROPPED, never defaulted — edge case 5:
// "The all-in baseline model scores a Case the ablation model errors on.
// That Case drops from the pair set; the pair count travels with the
// interval, so the report says how many Cases the claim rests on".
func pairDeltas(allIn, group map[string]float64, ids []string) []float64 {
	var out []float64
	for _, id := range ids {
		a, ok1 := allIn[id]
		b, ok2 := group[id]
		if !ok1 || !ok2 {
			continue
		}
		out = append(out, a-b)
	}
	return out
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// computeVerdict pairs one leave-one-out group's scores against the all-in
// baseline's scores and decides its verdict — the pairing and multiplicity
// discipline the bridge eval-seam plan's §2/§7/§8 describe, factored out so
// both a fresh measurement and a resume's recompute-from-store path (bridge
// eval-seam plan §6) produce it identically.
//
// Bonferroni is GOAL-ONLY (§8): nGroups is the PLANNED leave-one-out group
// count, fixed at quote time (RunParams.NGroups), never a count of groups
// that happened to reach a verdict — a dynamic N would make one group's
// correction depend on how many other groups' jobs had failed by the time
// it was measured. portfolio.Correct requires two-sided input and
// interval.HarmBound always returns one-sided, so the correction
// structurally cannot reach the control interval; Δ_control is reported at
// the RAW level, exactly as core/select.go's REGRESSION gate reads its own
// control arm raw.
func computeVerdict(
	group string, domain knov1.ScoreDomain, level float64, nGroups int,
	allIn, groupScores map[string]float64, devIDs, controlIDs []string,
) *knov1.BridgeGroupMeasured {
	ev := &knov1.BridgeGroupMeasured{AblationGroup: group}

	goalDeltas := pairDeltas(allIn, groupScores, devIDs)
	goalIv := interval.Paired(goalDeltas, domain, 1, level)
	if goalIv == nil {
		ev.NotMeasured = knov1.RejectionReason_REJECTION_REASON_UNDERPOWERED
		ev.Verdict = knov1.BridgeGroupVerdict_BRIDGE_GROUP_VERDICT_UNSPECIFIED
		return ev
	}

	corrected := goalIv
	if nGroups >= 2 {
		c := portfolio.Correct(goalIv, nGroups)
		if c == nil {
			// A method Correct cannot rescale (see its own doc), or a
			// corrected level that would not itself be a valid level.
			// Falling back to the UNCORRECTED interval here would silently
			// under-count the multiplicity — a narrower interval than the
			// family-wise claim can support, which is exactly the
			// "plausible interval of the wrong width" prime directive 5
			// exists to catch. Refusing is the honest answer.
			ev.NotMeasured = knov1.RejectionReason_REJECTION_REASON_UNDERPOWERED
			ev.Verdict = knov1.BridgeGroupVerdict_BRIDGE_GROUP_VERDICT_UNSPECIFIED
			return ev
		}
		corrected = c
	}

	ev.DeltaGroup = mean(goalDeltas)
	ev.DeltaGroupInterval = corrected

	controlDeltas := pairDeltas(allIn, groupScores, controlIDs)
	var controlIv *knov1.Interval
	var controlMean float64
	controlUnderpowered := true
	if len(controlDeltas) > 0 {
		controlMean = mean(controlDeltas)
		controlIv = interval.HarmBound(controlDeltas, domain, 1, level)
		controlUnderpowered = controlIv == nil
	}
	if controlIv != nil {
		ev.DeltaControl = controlMean
		ev.DeltaControlInterval = controlIv
	}
	ev.ControlUnderpowered = controlUnderpowered

	// shared is ALWAYS true for bridge — the plan's §4 amendment, finding
	// R8: both Δ_group and Δ_control pair against the SAME all-in scores,
	// which is structurally the shared-draw case NetEffect's conservative
	// bound exists for. Passing false would narrow the interval and
	// manufacture a false-confident interference claim.
	if !controlUnderpowered {
		if net := interval.NetEffect(corrected, controlMean, controlIv,
			len(goalDeltas), len(controlDeltas), true, level); net != nil && net.GetHigh() <= 0 {
			ev.Verdict = knov1.BridgeGroupVerdict_BRIDGE_GROUP_VERDICT_INTERFERENCE
			ev.NotMeasured = knov1.RejectionReason_REJECTION_REASON_UNSPECIFIED
			return ev
		}
	}

	if corrected.GetLow() > 0 {
		ev.Verdict = knov1.BridgeGroupVerdict_BRIDGE_GROUP_VERDICT_CONFIRMED
	} else {
		ev.Verdict = knov1.BridgeGroupVerdict_BRIDGE_GROUP_VERDICT_UNCONFIRMED
		ev.NotMeasured = knov1.RejectionReason_REJECTION_REASON_BRIDGE_UNCONFIRMED
	}
	return ev
}
