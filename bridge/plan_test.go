package bridge_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/knograph/kno/bridge"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

func portfolioEntry(id string, dest knov1.Destination, kind knov1.Kind) *knov1.PortfolioEntry {
	return &knov1.PortfolioEntry{
		AssetId:     id,
		Destination: dest,
		Valuation:   &knov1.Valuation{AssetId: id, Kind: kind},
	}
}

// TestPopulationSelectsOnlyTuningSetEntries pins that Population reads
// exactly the DESTINATION_TUNING_SET slice of a Portfolio, ignoring
// everything else.
func TestPopulationSelectsOnlyTuningSetEntries(t *testing.T) {
	t.Parallel()

	p := &knov1.Portfolio{Selected: []*knov1.PortfolioEntry{
		portfolioEntry("ctx-a", knov1.Destination_DESTINATION_CONTEXT, knov1.Kind_KIND_KNOWLEDGE),
		portfolioEntry("tune-a", knov1.Destination_DESTINATION_TUNING_SET, knov1.Kind_KIND_BEHAVIOR),
		portfolioEntry("tune-b", knov1.Destination_DESTINATION_TUNING_SET, knov1.Kind_KIND_BEHAVIOR),
	}}

	got, err := bridge.Population(p)
	if err != nil {
		t.Fatalf("Population: %v", err)
	}
	want := []string{"tune-a", "tune-b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Population = %v, want %v", got, want)
	}
}

// TestPopulationRefusesKnowledgeAssetsInTheTuningSet is acceptance
// criterion 15: a KIND_KNOWLEDGE Asset routed to the tuning set is refused
// before any job is submitted, naming the Asset.
func TestPopulationRefusesKnowledgeAssetsInTheTuningSet(t *testing.T) {
	t.Parallel()

	p := &knov1.Portfolio{Selected: []*knov1.PortfolioEntry{
		portfolioEntry("tune-a", knov1.Destination_DESTINATION_TUNING_SET, knov1.Kind_KIND_BEHAVIOR),
		portfolioEntry("leaked-knowledge", knov1.Destination_DESTINATION_TUNING_SET, knov1.Kind_KIND_KNOWLEDGE),
	}}

	_, err := bridge.Population(p)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Errorf("err = %v, want errs.ErrInvalidInput", err)
	}
	if got := err.Error(); !strings.Contains(got, "leaked-knowledge") {
		t.Errorf("error does not name the Asset: %q", got)
	}
}

// TestBuildGroupsOneAllInPlusOneLeaveOneOutPerQualifyingCluster covers the
// core shape: N clusters with enough Cases and enough routed population
// Assets produce N+1 jobs, and a leave-one-out group's training set is
// all-in minus exactly that cluster's members.
func TestBuildGroupsOneAllInPlusOneLeaveOneOutPerQualifyingCluster(t *testing.T) {
	t.Parallel()

	plan := &value.Plan{
		Routed: []value.AssetRouting{
			{AssetID: "a", CaseIDs: refundsCases},
			{AssetID: "b", CaseIDs: billingCases},
		},
		Clusters: []value.ClusterSnapshot{
			{Tag: "refunds", CaseIDs: refundsCases},
			{Tag: "billing", CaseIDs: billingCases},
		},
	}

	got, err := bridge.BuildGroups(plan, []string{"a", "b"}, 6)
	if err != nil {
		t.Fatalf("BuildGroups: %v", err)
	}
	if len(got.AllIn) != 2 {
		t.Errorf("AllIn = %v, want both assets", got.AllIn)
	}
	if len(got.LeaveOneOut) != 2 {
		t.Fatalf("LeaveOneOut has %d groups, want 2", len(got.LeaveOneOut))
	}
	if refunds := got.LeaveOneOut["refunds"]; len(refunds) != 1 || refunds[0] != "b" {
		t.Errorf("leave-refunds-out = %v, want [b] (all-in minus a, whose primary group is refunds)", refunds)
	}
	if billing := got.LeaveOneOut["billing"]; len(billing) != 1 || billing[0] != "a" {
		t.Errorf("leave-billing-out = %v, want [a]", billing)
	}
	if len(got.Skipped) != 0 || len(got.Unknown) != 0 {
		t.Errorf("Skipped=%v Unknown=%v, want both empty", got.Skipped, got.Unknown)
	}

	groups := got.Groups()
	want := []string{bridge.AllIn, "billing", "refunds"}
	if len(groups) != len(want) {
		t.Fatalf("Groups() = %v, want %v", groups, want)
	}
	for i := range want {
		if groups[i] != want[i] {
			t.Errorf("Groups()[%d] = %q, want %q", i, groups[i], want[i])
		}
	}
}

// refundsCases/billingCases are core.MinClusterCases (5) each, so both
// clusters qualify by default.
var (
	refundsCases = []string{"r1", "r2", "r3", "r4", "r5"}
	billingCases = []string{"b1", "b2", "b3", "b4", "b5"}
)

// TestBuildGroupsSkipsClustersBelowMinClusterCases is acceptance criterion
// 16: a cluster with fewer than core.MinClusterCases dev Cases is skipped,
// reported, and costs zero jobs.
func TestBuildGroupsSkipsClustersBelowMinClusterCases(t *testing.T) {
	t.Parallel()

	tooSmall := refundsCases[:core.MinClusterCases-1] // exported constant, per acceptance criterion 16
	plan := &value.Plan{
		Routed: []value.AssetRouting{
			{AssetID: "a", CaseIDs: tooSmall},
		},
		Clusters: []value.ClusterSnapshot{
			{Tag: "refunds", CaseIDs: tooSmall},
		},
	}

	got, err := bridge.BuildGroups(plan, []string{"a"}, 6)
	if err != nil {
		t.Fatalf("BuildGroups: %v", err)
	}
	if len(got.LeaveOneOut) != 0 {
		t.Errorf("LeaveOneOut = %v, want empty (below MinClusterCases)", got.LeaveOneOut)
	}
	if len(got.Skipped) != 1 || got.Skipped[0] != "refunds" {
		t.Errorf("Skipped = %v, want [refunds]", got.Skipped)
	}
}

// TestBuildGroupsRefusesAboveTheCap is acceptance criterion 17: group count
// above --bridge-max-groups is refused with a fix naming the flag and the
// price per group, and no job is submitted (BuildGroups itself submits
// nothing; this asserts the refusal and that it carries the right shape).
func TestBuildGroupsRefusesAboveTheCap(t *testing.T) {
	t.Parallel()

	plan := &value.Plan{
		Routed: []value.AssetRouting{
			{AssetID: "a", CaseIDs: refundsCases},
			{AssetID: "b", CaseIDs: billingCases},
		},
		Clusters: []value.ClusterSnapshot{
			{Tag: "refunds", CaseIDs: refundsCases},
			{Tag: "billing", CaseIDs: billingCases},
		},
	}

	_, err := bridge.BuildGroups(plan, []string{"a", "b"}, 1)
	if err == nil {
		t.Fatal("want a refusal: 2 qualifying clusters exceed a cap of 1")
	}
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Errorf("err = %v, want errs.ErrInvalidInput", err)
	}
	if !errors.Is(err, bridge.ErrTooManyGroups) {
		t.Errorf("err = %v, want bridge.ErrTooManyGroups", err)
	}
	if got := err.Error(); !strings.Contains(got, "--bridge-max-groups") {
		t.Errorf("error does not name the flag: %q", got)
	}
}

// TestBuildGroupsUnknownAssetsAreExcludedFromAllIn is acceptance criterion
// 14 at the plan level: an Asset with no primary group never appears in the
// all-in training set and never receives a leave-one-out group of its own.
func TestBuildGroupsUnknownAssetsAreExcludedFromAllIn(t *testing.T) {
	t.Parallel()

	plan := &value.Plan{
		Routed: []value.AssetRouting{
			{AssetID: "a", CaseIDs: refundsCases},
			{AssetID: "never-routed", CaseIDs: []string{"z1"}},
		},
		Clusters: []value.ClusterSnapshot{
			{Tag: "refunds", CaseIDs: refundsCases},
		},
	}

	got, err := bridge.BuildGroups(plan, []string{"a", "never-routed"}, 6)
	if err != nil {
		t.Fatalf("BuildGroups: %v", err)
	}
	for _, id := range got.AllIn {
		if id == "never-routed" {
			t.Error("an Unknown Asset appeared in AllIn")
		}
	}
	if len(got.Unknown) != 1 || got.Unknown[0] != "never-routed" {
		t.Errorf("Unknown = %v, want [never-routed]", got.Unknown)
	}
}

// TestDevCaseIDsForGroupsMapsEachLeaveOneOutGroupToItsClusterCases pins
// RunParams.DevCaseIDs's contract: each qualifying leave-one-out group's
// name maps to its cluster's dev Case IDs, read back from the SAME
// persisted value.Plan BuildGroups used, and a skipped or all-in-only
// plan produces no entries.
func TestDevCaseIDsForGroupsMapsEachLeaveOneOutGroupToItsClusterCases(t *testing.T) {
	t.Parallel()

	plan := &value.Plan{
		Routed: []value.AssetRouting{
			{AssetID: "a", CaseIDs: refundsCases},
			{AssetID: "b", CaseIDs: billingCases},
		},
		Clusters: []value.ClusterSnapshot{
			{Tag: "refunds", CaseIDs: refundsCases},
			{Tag: "billing", CaseIDs: billingCases},
		},
	}
	groups, err := bridge.BuildGroups(plan, []string{"a", "b"}, 6)
	if err != nil {
		t.Fatalf("BuildGroups: %v", err)
	}

	got := bridge.DevCaseIDsForGroups(plan, groups)
	if len(got) != 2 {
		t.Fatalf("got %d groups, want 2 (refunds, billing)", len(got))
	}
	if diff := !slicesEqualUnordered(got["refunds"], refundsCases); diff {
		t.Errorf("DevCaseIDs[refunds] = %v, want %v", got["refunds"], refundsCases)
	}
	if diff := !slicesEqualUnordered(got["billing"], billingCases); diff {
		t.Errorf("DevCaseIDs[billing] = %v, want %v", got["billing"], billingCases)
	}
	if _, ok := got[bridge.AllIn]; ok {
		t.Error("DevCaseIDsForGroups must carry no entry for the all-in group")
	}
}

func slicesEqualUnordered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]int, len(a))
	for _, x := range a {
		set[x]++
	}
	for _, x := range b {
		set[x]--
	}
	for _, n := range set {
		if n != 0 {
			return false
		}
	}
	return true
}
