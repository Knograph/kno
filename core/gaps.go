package core

import (
	"sort"

	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// MinClusterCases is the minimum number of a cluster's failed dev Cases an
// Asset must have been routed to before its measurement may testify about
// that cluster. Named, exported, and load-bearing: it is the line between
// "we looked and found nothing" (GAP) and "we did not really look" (UNKNOWN).
const MinClusterCases = 5

// ComputeGaps reduces a source Value run's plan and its recorded Valuations
// into the per-cluster improvement verdicts the report renders.
//
// The statistic, per the report plan: a cluster is IMPROVED when at least one
// Asset routed to >= MinClusterCases of its Cases has a delta whose 95% CI
// excludes zero. It is UNKNOWN when nothing routed reaches the minimum or
// the covering measurement is underpowered — non-significance is not absence,
// and the output is a spend recommendation. It is GAP when the cluster is
// well-covered and no covering measurement is significant.
//
// One reported number per cluster: the BEST covering Asset's delta and
// interval — the strongest measured effect the cluster actually saw — never
// a cluster-level threshold game. Deterministic: best is the covering Asset
// with the largest |delta|, ties broken by Asset ID, clusters in the plan's
// snapshot order.
func ComputeGaps(plan *value.Plan, valuations []*knov1.Valuation) *knov1.Gaps {
	gaps := &knov1.Gaps{
		MultipleTesting: len(plan.Clusters) > 1,
	}
	byAsset := make(map[string]*knov1.Valuation, len(valuations))
	for _, v := range valuations {
		byAsset[v.GetAssetId()] = v
	}
	for _, cluster := range plan.Clusters {
		gaps.Clusters = append(gaps.Clusters, verdictFor(cluster, plan.Routed, byAsset))
	}
	return gaps
}

// verdictFor computes one cluster's record. See ComputeGaps for the rules.
func verdictFor(
	cluster value.ClusterSnapshot,
	routed []value.AssetRouting,
	byAsset map[string]*knov1.Valuation,
) *knov1.GapCluster {
	rec := &knov1.GapCluster{
		Tag:       cluster.Tag,
		CaseCount: int32(len(cluster.CaseIDs)), //nolint:gosec // bounded by the dev split
		Status:    knov1.GapStatus_GAP_STATUS_UNKNOWN,
	}

	// Covering Assets, strongest usable first. "Covering" is about the
	// PLAN's routing, not about what got measured: an Asset routed to the
	// cluster's Cases is in the running even if its measurement is unusable —
	// that is the underpowered branch, and the record must distinguish it
	// from no Asset having been routed at all.
	var covering []assetCover
	for _, r := range sortedRouted(routed) {
		covered := intersectCount(r.CaseIDs, cluster.CaseIDs)
		if covered < MinClusterCases {
			continue
		}
		v := byAsset[r.AssetID]
		covering = append(covering, assetCover{
			assetID: r.AssetID,
			covered: covered,
			v:       v,
		})
	}
	sort.Slice(covering, func(i, j int) bool {
		di := absDelta(covering[i].v)
		dj := absDelta(covering[j].v)
		if di != dj {
			return di > dj
		}
		return covering[i].assetID < covering[j].assetID
	})

	for _, c := range covering {
		// CoveredCount is recorded BEFORE the usability gate: a record that
		// says "routing reached N of the cluster's Cases, verdict UNKNOWN"
		// is an underpowered measurement; one that says "routing reached
		// nothing" is no coverage at all. The two must not read alike.
		rec.CoveredCount = int32(c.covered) //nolint:gosec // bounded by the dev split
		if !usable(c.v) {
			// A covering measurement that is absent or underpowered cannot
			// separate zero from a real effect. Not a GAP: nothing here
			// certified that a spend would find nothing.
			continue
		}
		rec.BestAssetId = c.assetID
		rec.BestDelta = c.v.GetDeltaGoal()
		rec.BestInterval = c.v.GetDeltaInterval()
		if intervalExcludesZero(c.v.GetDeltaInterval()) {
			rec.Status = knov1.GapStatus_GAP_STATUS_IMPROVED
		} else {
			rec.Status = knov1.GapStatus_GAP_STATUS_GAP
		}
		return rec
	}
	// No covering Asset at all, or none with a usable measurement: UNKNOWN.
	// The shape of the record says which: CoveredCount zero is "nothing
	// routed to the minimum"; CoveredCount set with no BestAssetId is
	// "routed but underpowered". Never a guess either way.
	return rec
}

type assetCover struct {
	assetID string
	covered int
	v       *knov1.Valuation
}

// sortedRouted is a copy of the routings in Asset-ID order. Copies: sorting
// in place would reorder plan.Routed, which other consumers read in supply
// order.
func sortedRouted(routed []value.AssetRouting) []value.AssetRouting {
	out := append([]value.AssetRouting(nil), routed...)
	sort.Slice(out, func(i, j int) bool { return out[i].AssetID < out[j].AssetID })
	return out
}

// intersectCount counts routed ∩ cluster, both deduplicated by construction
// (routing dedups within an Asset; the snapshot dedups within a cluster).
func intersectCount(routed, cluster []string) int {
	clusterSet := make(map[string]struct{}, len(cluster))
	for _, id := range cluster {
		clusterSet[id] = struct{}{}
	}
	n := 0
	for _, id := range routed {
		if _, ok := clusterSet[id]; ok {
			n++
		}
	}
	return n
}

// usable reports whether a Valuation may testify about a cluster: it was
// measured (a delta and interval exist) at or above the power floor. A
// measurement below the floor is underpowered — it could not separate zero
// from the effect the cluster is asking about.
func usable(v *knov1.Valuation) bool {
	if v == nil {
		return false
	}
	iv := v.GetDeltaInterval()
	if iv == nil {
		return false
	}
	// NPairs is a pointer even though the getter is not: presence is the
	// power claim, absent means the interval was built without a pair count.
	np := iv.NPairs
	return np != nil && *np >= MinClusterCases
}

// intervalExcludesZero reads the interval with its sidedness: the meaningless
// bound of a one-sided interval is not evidence. Unspecified reads as
// two-sided, which is what Interval's own godoc promises.
func intervalExcludesZero(iv *knov1.Interval) bool {
	switch iv.GetSidedness() {
	case knov1.Sidedness_SIDEDNESS_UPPER:
		return iv.GetHigh() < 0
	case knov1.Sidedness_SIDEDNESS_LOWER:
		return iv.GetLow() > 0
	default:
		return iv.GetLow() > 0 || iv.GetHigh() < 0
	}
}

func absDelta(v *knov1.Valuation) float64 {
	if v == nil {
		return -1 // a nil measurement sorts below any measured one
	}
	d := v.GetDeltaGoal()
	if d < 0 {
		return -d
	}
	return d
}
