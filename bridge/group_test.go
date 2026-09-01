package bridge_test

import (
	"strconv"
	"testing"

	"github.com/knograph/kno/bridge"
	"github.com/knograph/kno/core/value"
)

// caseIDs is a small helper for building a run of Case IDs like "c1".."cN".
func caseIDs(prefix string, n int) []string {
	out := make([]string, n)
	for i := range n {
		out[i] = prefix + strconv.Itoa(i)
	}
	return out
}

// TestAssignGroupsExclusiveMembership is acceptance criterion 13: an Asset
// routed to two clusters appears in exactly one group, and a fixture with
// deliberate overlap asserts each Asset ID occurs in exactly one group's
// member list.
func TestAssignGroupsExclusiveMembership(t *testing.T) {
	t.Parallel()

	plan := &value.Plan{
		Routed: []value.AssetRouting{
			// Asset "a" overlaps both clusters: 3 of cluster A's 5 Cases and
			// 2 of cluster B's 5 Cases.
			{AssetID: "a", CaseIDs: []string{"a1", "a2", "a3", "b1", "b2"}},
		},
		Clusters: []value.ClusterSnapshot{
			{Tag: "refunds", CaseIDs: []string{"a1", "a2", "a3", "a4", "a5"}},
			{Tag: "billing", CaseIDs: []string{"b1", "b2", "b3", "b4", "b5"}},
		},
	}

	got := bridge.AssignGroups(plan, []string{"a"})
	if len(got) != 1 {
		t.Fatalf("got %d assignments, want 1", len(got))
	}
	if got[0].Unknown {
		t.Fatalf("asset a reported Unknown, want assigned to refunds (3 > 2)")
	}
	if got[0].Cluster != "refunds" {
		t.Errorf("cluster = %q, want %q (the larger intersection)", got[0].Cluster, "refunds")
	}

	// Removing exclusivity (i.e. reporting membership in every cluster with
	// n>0) would put "a" in both refunds and billing; this asserts it is in
	// exactly one.
	seen := 0
	for _, g := range got {
		if g.Cluster == "refunds" || g.Cluster == "billing" {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("asset a appeared in %d groups, want exactly 1", seen)
	}
}

// TestAssignGroupsZeroRoutedIsUnknown is acceptance criterion 14: an Asset
// routed to zero clusters gets no primary group and must be reported
// Unknown rather than assigned to anything.
func TestAssignGroupsZeroRoutedIsUnknown(t *testing.T) {
	t.Parallel()

	plan := &value.Plan{
		Routed: []value.AssetRouting{
			{AssetID: "a", CaseIDs: []string{"z1", "z2"}}, // shares nothing with any cluster
		},
		Clusters: []value.ClusterSnapshot{
			{Tag: "refunds", CaseIDs: []string{"a1", "a2"}},
		},
	}

	got := bridge.AssignGroups(plan, []string{"a", "never-routed"})
	if len(got) != 2 {
		t.Fatalf("got %d assignments, want 2", len(got))
	}
	for _, g := range got {
		if !g.Unknown {
			t.Errorf("asset %s: Unknown = false, want true (zero routed intersection)", g.AssetID)
		}
		if g.Cluster != "" {
			t.Errorf("asset %s: Cluster = %q, want empty for an Unknown assignment", g.AssetID, g.Cluster)
		}
	}
}

// TestAssignGroupsIntersectionNotClusterSize is acceptance criterion 28's
// pinned fixture, verbatim from the plan: Asset "a" is routed to 3 Cases of
// a 40-Case cluster A and 5 Cases of a 6-Case cluster B. The primary group
// must be B — the larger INTERSECTION — even though A is the larger
// cluster. Counting len(routed.CaseIDs) against cluster size instead would
// get this backwards.
func TestAssignGroupsIntersectionNotClusterSize(t *testing.T) {
	t.Parallel()

	// Cluster A: 40 Cases, "a" routed to exactly 3 of them (a1..a3), plus 37
	// more Cases "a" was never routed to.
	clusterA := append([]string{}, caseIDs("a", 40)...)
	// Cluster B: 6 Cases, "a" routed to 5 of them.
	clusterB := caseIDs("b", 6)

	plan := &value.Plan{
		Routed: []value.AssetRouting{
			{AssetID: "a", CaseIDs: append(
				caseIDs("a", 3),    // 3 of cluster A's 40
				caseIDs("b", 5)..., // 5 of cluster B's 6
			)},
		},
		Clusters: []value.ClusterSnapshot{
			{Tag: "cluster-a", CaseIDs: clusterA},
			{Tag: "cluster-b", CaseIDs: clusterB},
		},
	}

	got := bridge.AssignGroups(plan, []string{"a"})
	if len(got) != 1 || got[0].Unknown {
		t.Fatalf("got %+v, want a single non-Unknown assignment", got)
	}
	if got[0].Cluster != "cluster-b" {
		t.Errorf("primary group = %q, want %q (5 routed of 6, the larger INTERSECTION, "+
			"even though cluster-a is the larger CLUSTER)", got[0].Cluster, "cluster-b")
	}
}

// TestAssignGroupsTieBreaksByLowerTag pins the deterministic tie-break: two
// clusters with EQUAL intersection counts resolve to the lexicographically
// lower tag.
func TestAssignGroupsTieBreaksByLowerTag(t *testing.T) {
	t.Parallel()

	plan := &value.Plan{
		Routed: []value.AssetRouting{
			{AssetID: "a", CaseIDs: []string{"x1", "x2", "y1", "y2"}},
		},
		Clusters: []value.ClusterSnapshot{
			// Both clusters intersect "a"'s routed set at exactly 2.
			{Tag: "zzz-cluster", CaseIDs: []string{"x1", "x2"}},
			{Tag: "aaa-cluster", CaseIDs: []string{"y1", "y2"}},
		},
	}

	got := bridge.AssignGroups(plan, []string{"a"})
	if len(got) != 1 || got[0].Cluster != "aaa-cluster" {
		t.Errorf("got %+v, want the lexicographically lower tag aaa-cluster on a tie", got)
	}
}

// TestAssignGroupsNDroppedNeverAffectsAssignment is acceptance criterion 28's
// third pin: ClusterSnapshot.NDropped never changes a primary-group
// assignment — it is a diagnostic count of duplicates already excluded from
// CaseIDs, not an addition to it.
func TestAssignGroupsNDroppedNeverAffectsAssignment(t *testing.T) {
	t.Parallel()

	base := func(nDroppedA, nDroppedB int) *value.Plan {
		return &value.Plan{
			Routed: []value.AssetRouting{
				{AssetID: "a", CaseIDs: []string{"a1", "a2", "b1"}},
			},
			Clusters: []value.ClusterSnapshot{
				{Tag: "cluster-a", CaseIDs: []string{"a1", "a2"}, NDropped: nDroppedA},
				{Tag: "cluster-b", CaseIDs: []string{"b1"}, NDropped: nDroppedB},
			},
		}
	}

	low := bridge.AssignGroups(base(0, 0), []string{"a"})
	high := bridge.AssignGroups(base(1000, 1000), []string{"a"})
	if low[0].Cluster != high[0].Cluster {
		t.Errorf("NDropped changed the assignment: %q (NDropped=0) vs %q (NDropped=1000)",
			low[0].Cluster, high[0].Cluster)
	}
	if low[0].Cluster != "cluster-a" {
		t.Errorf("assignment = %q, want cluster-a (2 routed > 1 routed), independent of NDropped", low[0].Cluster)
	}
}

// TestAssignGroupsIsOrderedByAssetID pins that the returned slice does not
// depend on the order assetIDs were supplied in, or on plan.Routed's order.
func TestAssignGroupsIsOrderedByAssetID(t *testing.T) {
	t.Parallel()

	plan := &value.Plan{
		Routed: []value.AssetRouting{
			{AssetID: "zeta", CaseIDs: []string{"c1"}},
			{AssetID: "alpha", CaseIDs: []string{"c1"}},
		},
		Clusters: []value.ClusterSnapshot{
			{Tag: "t", CaseIDs: []string{"c1"}},
		},
	}
	got := bridge.AssignGroups(plan, []string{"zeta", "alpha"})
	if len(got) != 2 || got[0].AssetID != "alpha" || got[1].AssetID != "zeta" {
		t.Errorf("got %+v, want sorted by Asset ID regardless of input order", got)
	}
}
