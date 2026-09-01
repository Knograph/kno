package bridge

import (
	"sort"

	"github.com/knograph/kno/core/value"
)

// GroupAssignment is one Asset's primary ablation group, computed per the
// tuner-bridge plan's Step 3: exclusive membership, decided by which
// cluster it shares the most ROUTED dev Cases with.
//
// "Most routed Cases" is the sharpest correctness point in the design: an
// Asset routed to several failure clusters is measured against exactly one
// of them, because leave-one-group-out does not decompose under overlapping
// membership — an Asset in both A and B stays present in the "leave out A"
// training set via B, so the A-LOO delta measures B's contribution and
// attributes it to A.
type GroupAssignment struct {
	// AssetID identifies the Asset.
	AssetID string

	// Cluster is the tag of this Asset's primary group — the ablation group
	// it is leave-one-out for. Empty when Unknown.
	Cluster string

	// Unknown reports that this Asset has no primary group: it was routed
	// to zero of the plan's clusters (n(a, c) = 0 for every cluster c),
	// which also covers an Asset the Value plan never routed at all — e.g.
	// a pool-pinned Asset Value did not measure. An Unknown Asset gets no
	// bridge verdict: it is never folded into a group it did not earn, and
	// never receives a BRIDGE_UNCONFIRMED rejection.
	Unknown bool
}

// AssignGroups computes the primary ablation group for every Asset ID in
// assetIDs, per the intersection rule:
//
//	n(a, c) = |routed(a).CaseIDs ∩ c.CaseIDs|
//	primary(a) = argmax_c n(a, c) over clusters with n(a, c) > 0,
//	             ties broken by cluster tag (unique per cluster, so this
//	             alone disambiguates every tie the shape of the data can
//	             produce).
//
// Both operands come from the SAME persisted value.Plan — plan.Routed for
// the numerator, plan.Clusters for the denominator — never derived from
// cluster size or read from any other source. A cluster's NDropped plays no
// part: dropped Cases were never routed, so counting them would let an
// Asset's primary group be decided by Cases it was never measured on.
//
// Deterministic regardless of map iteration order or the order of assetIDs:
// the result depends only on plan and the SET of IDs, and the returned slice
// is sorted by Asset ID.
func AssignGroups(plan *value.Plan, assetIDs []string) []GroupAssignment {
	routedByAsset := make(map[string]value.AssetRouting, len(plan.Routed))
	for _, r := range plan.Routed {
		routedByAsset[r.AssetID] = r
	}

	out := make([]GroupAssignment, 0, len(assetIDs))
	for _, id := range assetIDs {
		out = append(out, assignOne(id, routedByAsset[id], plan.Clusters))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AssetID < out[j].AssetID })
	return out
}

// clusterHit is one cluster's routed-intersection count for one Asset,
// tracked while scanning plan.Clusters in assignOne.
type clusterHit struct {
	tag string
	n   int
}

func assignOne(assetID string, routed value.AssetRouting, clusters []value.ClusterSnapshot) GroupAssignment {
	var best clusterHit
	found := false
	for _, c := range clusters {
		n := intersectCount(routed.CaseIDs, c.CaseIDs)
		if n == 0 {
			continue
		}
		if !found || n > best.n || (n == best.n && c.Tag < best.tag) {
			best, found = clusterHit{c.Tag, n}, true
		}
	}
	if !found {
		return GroupAssignment{AssetID: assetID, Unknown: true}
	}
	return GroupAssignment{AssetID: assetID, Cluster: best.tag}
}

// intersectCount counts routed ∩ cluster, both taken as sets.
//
// Mirrors core.intersectCount (core/gaps.go) rather than importing it: that
// helper is unexported, five lines, and exporting it from core purely for
// this package's benefit would widen core's public surface for a function
// with exactly one caller outside it. Both implementations must agree that
// NDropped plays no part — enforced by TestAssignGroupsMatchesTheGapsIntersectionRule
// in this package's tests, which drives the same fixture through both.
func intersectCount(routed, cluster []string) int {
	set := make(map[string]struct{}, len(cluster))
	for _, id := range cluster {
		set[id] = struct{}{}
	}
	n := 0
	for _, id := range routed {
		if _, ok := set[id]; ok {
			n++
		}
	}
	return n
}
