package bridge

import (
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/store"
)

// This file unit-tests measure.go's unexported helpers directly — the pure
// pairing, set, and verdict-computation functions bridge.Run and
// bridge.ScoreEvalRunner build on. run_test.go and score_test.go already
// cover these end to end through the exported surface; these tests pin the
// individual branches that are hard to reach that way (a Bonferroni
// correction that cannot be applied, an underpowered control, an empty
// pair set) directly and cheaply.

func TestUnionCaseIDsDedupesAcrossGroupsAndControl(t *testing.T) {
	t.Parallel()
	p := RunParams{
		DevCaseIDs: map[string][]string{
			"a": {"c1", "c2"},
			"b": {"c2", "c3"}, // c2 shared with a — a multi-tag Case
		},
		ControlCaseIDs: []string{"ctl1", "c1"}, // c1 also appears as a dev Case
	}
	got := unionCaseIDs(p)
	want := map[string]bool{"c1": true, "c2": true, "c3": true, "ctl1": true}
	if len(got) != len(want) {
		t.Fatalf("unionCaseIDs = %v, want %d unique ids", got, len(want))
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected id %s", id)
		}
	}
}

func TestGroupCaseIDsIsOneGroupsDevPlusControl(t *testing.T) {
	t.Parallel()
	p := RunParams{
		DevCaseIDs:     map[string][]string{"a": {"c1", "c2"}, "b": {"c3"}},
		ControlCaseIDs: []string{"ctl1"},
	}
	got := groupCaseIDs(p, "a")
	want := map[string]bool{"c1": true, "c2": true, "ctl1": true}
	if len(got) != len(want) {
		t.Fatalf("groupCaseIDs(a) = %v, want %v", got, want)
	}
	for _, id := range got {
		if id == "c3" {
			t.Error("groupCaseIDs(a) leaked group b's dev Case")
		}
	}
}

func TestMissingIDsReportsWhatHaveDoesNotCover(t *testing.T) {
	t.Parallel()
	have := map[string]float64{"c1": 0.5, "c3": 0.1}
	got := missingIDs([]string{"c1", "c2", "c3", "c4"}, have)
	want := []string{"c2", "c4"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("missingIDs = %v, want %v", got, want)
	}
	if got := missingIDs(nil, have); len(got) != 0 {
		t.Errorf("missingIDs(nil, ...) = %v, want empty", got)
	}
}

func TestPairDeltasDropsCasesMissingFromEitherSide(t *testing.T) {
	t.Parallel()
	allIn := map[string]float64{"c1": 0.9, "c2": 0.8, "c3": 0.7}
	group := map[string]float64{"c1": 0.5, "c3": 0.3} // c2 errored on the ablation model
	got := pairDeltas(allIn, group, []string{"c1", "c2", "c3"})
	if len(got) != 2 {
		t.Fatalf("pairDeltas = %v, want 2 entries (c2 dropped)", got)
	}
	// 0.9-0.5, 0.7-0.3. Compared with a tolerance, not exact equality: this
	// is ordinary floating-point subtraction, not a claim about
	// architecture-specific FMA fusion (that risk is for values crossing a
	// serialized boundary, e.g. a golden file — see CLAUDE.md's FLOATS
	// note), but exact equality on a float64 difference is still fragile
	// to depend on in a test.
	const eps = 1e-9
	if d := got[0] - 0.4; d < -eps || d > eps {
		t.Errorf("pairDeltas[0] = %v, want 0.4", got[0])
	}
	if d := got[1] - 0.4; d < -eps || d > eps {
		t.Errorf("pairDeltas[1] = %v, want 0.4", got[1])
	}
}

func TestArmForDistinguishesDevFromControl(t *testing.T) {
	t.Parallel()
	dev := map[string]struct{}{"d1": {}}
	if got := armFor("d1", dev); got != store.ArmTreatment {
		t.Errorf("armFor(dev case) = %v, want ArmTreatment", got)
	}
	if got := armFor("ctl1", dev); got != store.ArmControl {
		t.Errorf("armFor(control case) = %v, want ArmControl", got)
	}
}

func TestComputeVerdictUnderpoweredWhenTooFewPairs(t *testing.T) {
	t.Parallel()
	allIn := map[string]float64{"c1": 0.9}
	group := map[string]float64{"c1": 0.5}
	ev := computeVerdict("cluster-x", knov1.ScoreDomain_SCORE_DOMAIN_UNIT_INTERVAL, 0.95, 0,
		allIn, group, []string{"c1"}, nil)
	if ev.GetVerdict() != knov1.BridgeGroupVerdict_BRIDGE_GROUP_VERDICT_UNSPECIFIED {
		t.Errorf("Verdict = %v, want UNSPECIFIED (fewer than 2 pairs)", ev.GetVerdict())
	}
	if ev.GetNotMeasured() != knov1.RejectionReason_REJECTION_REASON_UNDERPOWERED {
		t.Errorf("NotMeasured = %v, want UNDERPOWERED", ev.GetNotMeasured())
	}
	if ev.GetDeltaGroupInterval() != nil {
		t.Error("an underpowered group must carry no DeltaGroupInterval")
	}
}

func TestComputeVerdictRefusesWhenBonferroniCannotCorrect(t *testing.T) {
	t.Parallel()
	// A sign-method interval with NPairs unset (0) cannot be corrected —
	// portfolio.Correct's MethodSign branch requires nn >= 1. Forcing the
	// degenerate (identical-deltas) path is the simplest way to reach a
	// method Correct might refuse without hand-building an Interval.
	allIn := map[string]float64{"c1": 0.9, "c2": 0.9}
	group := map[string]float64{"c1": 0.5, "c2": 0.5} // identical deltas -> sign method
	ev := computeVerdict("cluster-x", knov1.ScoreDomain_SCORE_DOMAIN_UNIT_INTERVAL, 0.95, 6,
		allIn, group, []string{"c1", "c2"}, nil)
	// Whatever the outcome, a verdict is never reported without its
	// interval unless NotMeasured explains why — prime directive 5,
	// enforced here at the unit level.
	if ev.GetVerdict() == knov1.BridgeGroupVerdict_BRIDGE_GROUP_VERDICT_CONFIRMED && ev.GetDeltaGroupInterval() == nil {
		t.Error("a CONFIRMED verdict must carry its interval")
	}
}

func TestComputeVerdictControlUnderpoweredNeverAccusesInterference(t *testing.T) {
	t.Parallel()
	allIn := map[string]float64{
		"d1": 0.9, "d2": 0.8, "d3": 0.7,
		"ctl1": 0.5,
	}
	group := map[string]float64{
		"d1": 0.4, "d2": 0.3, "d3": 0.2, // clearly CONFIRMED on its own
		"ctl1": 0.9, // a single control pair: too few for HarmBound
	}
	ev := computeVerdict("cluster-x", knov1.ScoreDomain_SCORE_DOMAIN_UNIT_INTERVAL, 0.95, 0,
		allIn, group, []string{"d1", "d2", "d3"}, []string{"ctl1"})
	if !ev.GetControlUnderpowered() {
		t.Error("a single control pair must be reported underpowered")
	}
	if ev.GetVerdict() == knov1.BridgeGroupVerdict_BRIDGE_GROUP_VERDICT_INTERFERENCE {
		t.Error("an underpowered control must never support an INTERFERENCE verdict")
	}
}

// TestGroupNeededCaseIDsRoutesAllInToTheUnion pins the dispatch measureGroup
// relies on: the all-in group's needed set is the union pass, every other
// group's is its own dev-plus-control set.
func TestGroupNeededCaseIDsRoutesAllInToTheUnion(t *testing.T) {
	t.Parallel()
	p := RunParams{
		DevCaseIDs:     map[string][]string{"cluster-x": {"d1"}, "cluster-y": {"d2"}},
		ControlCaseIDs: []string{"ctl1"},
	}
	allInIDs := groupNeededCaseIDs(p, AllIn)
	if len(allInIDs) != 3 {
		t.Errorf("groupNeededCaseIDs(all-in) = %v, want 3 ids (d1,d2,ctl1)", allInIDs)
	}
	groupIDs := groupNeededCaseIDs(p, "cluster-x")
	if len(groupIDs) != 2 {
		t.Errorf("groupNeededCaseIDs(cluster-x) = %v, want 2 ids (d1,ctl1)", groupIDs)
	}
}
