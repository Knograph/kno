package cli

import (
	"testing"

	"github.com/knograph/kno/bridge"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// evalCeilingGroups builds a two-group GroupsPlan (a qualifying
// leave-one-out group "refunds" plus the implicit all-in group) — the
// shape evalPassCeiling and evalGroupCaseIDs are written against.
func evalCeilingGroups() *bridge.GroupsPlan {
	return &bridge.GroupsPlan{
		AllIn:       []string{"tune-a", "tune-b"},
		LeaveOneOut: map[string][]string{"refunds": {"tune-b"}},
	}
}

// TestEvalPassCeilingNilPriceIsZero pins §4's Together case: no per-token
// rate resolved means no eval-pass addend at all, not a zero-priced one.
func TestEvalPassCeilingNilPriceIsZero(t *testing.T) {
	devCaseIDs := map[string][]string{"refunds": {"c1", "c2"}}
	capUSD, calls, err := evalPassCeiling(nil, "meta-llama/Llama-3-8b", evalCeilingGroups(), devCaseIDs, []string{"ctl1"})
	if err != nil {
		t.Fatalf("evalPassCeiling: %v", err)
	}
	if capUSD != 0 || calls != 0 {
		t.Errorf("evalPassCeiling(nil price) = (%d, %d), want (0, 0)", capUSD, calls)
	}
}

// TestEvalPassCeilingPricedIsPositiveAndCountsEveryGroupsPass pins the
// OTHER half: a resolved price produces a positive ceiling, and the call
// count is the SUM over every group's own pass (the all-in union pass plus
// each leave-one-out group's dev-plus-control pass) — NOT the deduplicated
// Case-ID union across the whole run, which would under-count real billed
// calls whenever more than one group shares a Case.
func TestEvalPassCeilingPricedIsPositiveAndCountsEveryGroupsPass(t *testing.T) {
	price := &knov1.Price{
		InputPerMtokUsdMicros:  proto64(1_000_000),
		OutputPerMtokUsdMicros: proto64(1_000_000),
	}
	devCaseIDs := map[string][]string{"refunds": {"c1", "c2"}}
	controlCaseIDs := []string{"ctl1"}

	capUSD, calls, err := evalPassCeiling(price, "ft:gpt-5.6-terra", evalCeilingGroups(), devCaseIDs, controlCaseIDs)
	if err != nil {
		t.Fatalf("evalPassCeiling: %v", err)
	}
	if capUSD <= 0 {
		t.Errorf("evalPassCeiling with a resolved price = %d, want > 0", capUSD)
	}
	// all-in's union pass: {c1, c2, ctl1} = 3. refunds' own pass: {c1, c2,
	// ctl1} = 3 (its dev Cases plus control; refunds has no OTHER group's
	// dev Cases to add since it is the only leave-one-out group here).
	// 3 + 3 = 6, not the 3-Case deduplicated union a naive implementation
	// would report.
	if calls != 6 {
		t.Errorf("calls = %d, want 6 (3 per group x 2 groups, not deduplicated across groups)", calls)
	}
}

// TestEvalGroupCaseIDsAllInIsTheUnionOfEveryGroup pins evalGroupCaseIDs'
// all-in branch: every OTHER group's dev Cases, unioned with control —
// bridge/measure.go's unionCaseIDs, mirrored.
func TestEvalGroupCaseIDsAllInIsTheUnionOfEveryGroup(t *testing.T) {
	devCaseIDs := map[string][]string{
		"refunds": {"c1", "c2"},
		"billing": {"c3"},
	}
	got := evalGroupCaseIDs(bridge.AllIn, devCaseIDs, []string{"ctl1"})
	want := map[string]struct{}{"c1": {}, "c2": {}, "c3": {}, "ctl1": {}}
	if len(got) != len(want) {
		t.Fatalf("evalGroupCaseIDs(all-in) = %v, want %v", got, want)
	}
	for id := range want {
		if _, ok := got[id]; !ok {
			t.Errorf("evalGroupCaseIDs(all-in) is missing %s", id)
		}
	}
}

// TestEvalGroupCaseIDsLeaveOneOutIsItsOwnClusterOnly pins the OTHER branch:
// a leave-one-out group's set is its OWN dev Cases plus control, not every
// group's — bridge/measure.go's groupCaseIDs, mirrored.
func TestEvalGroupCaseIDsLeaveOneOutIsItsOwnClusterOnly(t *testing.T) {
	devCaseIDs := map[string][]string{
		"refunds": {"c1", "c2"},
		"billing": {"c3"},
	}
	got := evalGroupCaseIDs("refunds", devCaseIDs, []string{"ctl1"})
	want := map[string]struct{}{"c1": {}, "c2": {}, "ctl1": {}}
	if len(got) != len(want) {
		t.Fatalf("evalGroupCaseIDs(refunds) = %v, want %v", got, want)
	}
	for id := range want {
		if _, ok := got[id]; !ok {
			t.Errorf("evalGroupCaseIDs(refunds) is missing %s", id)
		}
	}
	if _, ok := got["c3"]; ok {
		t.Error("evalGroupCaseIDs(refunds) leaked billing's c3")
	}
}

// proto64 is a tiny int64-pointer helper, matching pricing/table.go's own
// usd() pattern for building a *knov1.Price inline.
func proto64(v int64) *int64 { return &v }
