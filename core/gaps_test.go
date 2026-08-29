package core

import (
	"bytes"
	"encoding/gob"
	"math/rand"
	"testing"

	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// gapsFixture builds the smallest plan + valuations pair a test needs: one
// cluster of n cluster cases and routed Assets with synthetic measurements.
func gapsFixture() (*value.Plan, map[string]*knov1.Valuation) {
	plan := &value.Plan{
		Clusters: []value.ClusterSnapshot{
			{Tag: "billing", CaseIDs: []string{"c1", "c2", "c3", "c4", "c5", "c6", "c7", "c8"}},
		},
	}
	return plan, make(map[string]*knov1.Valuation)
}

func vals(m map[string]*knov1.Valuation) []*knov1.Valuation {
	out := make([]*knov1.Valuation, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func gapValuation(assetID string, delta float64, low, high float64, pairs int32) *knov1.Valuation {
	return &knov1.Valuation{
		AssetId:   assetID,
		DeltaGoal: delta,
		DeltaInterval: &knov1.Interval{
			Low:       low,
			High:      high,
			Level:     0.95,
			Sidedness: knov1.Sidedness_SIDEDNESS_TWO_SIDED,
			NPairs:    int32Ptr(pairs),
		},
	}
}

func routed(assetID string, caseIDs ...string) value.AssetRouting {
	return value.AssetRouting{AssetID: assetID, CaseIDs: caseIDs}
}

func TestComputeGapsImproved(t *testing.T) {
	t.Parallel()
	plan, m := gapsFixture()
	plan.Routed = []value.AssetRouting{routed("a1", "c1", "c2", "c3", "c4", "c5", "c6", "c7", "c8")}
	m["a1"] = gapValuation("a1", 0.5, 0.2, 0.8, 10)

	got := ComputeGaps(plan, vals(m))
	rec := got.GetClusters()[0]
	if rec.GetStatus() != knov1.GapStatus_GAP_STATUS_IMPROVED {
		t.Fatalf("status = %s, want IMPROVED", rec.GetStatus())
	}
	if rec.GetBestAssetId() != "a1" || rec.GetCoveredCount() != 8 {
		t.Fatalf("best = %s covered %d, want a1/8", rec.GetBestAssetId(), rec.GetCoveredCount())
	}
	if rec.GetCaseCount() != 8 {
		t.Fatalf("case_count = %d, want 8", rec.GetCaseCount())
	}
	if got.GetMultipleTesting() {
		t.Fatal("multiple_testing true for one cluster, want false")
	}
}

func TestComputeGapsGapWhenWellCoveredButNotSignificant(t *testing.T) {
	t.Parallel()
	plan, m := gapsFixture()
	plan.Routed = []value.AssetRouting{routed("a1", "c1", "c2", "c3", "c4", "c5", "c6")}
	// Well covered (6 >= 5), interval crosses zero: a spend on this cluster
	// is not supported by what was measured — the honest GAP, not an unknown.
	m["a1"] = gapValuation("a1", 0.1, -0.1, 0.3, 10)

	rec := ComputeGaps(plan, vals(m)).GetClusters()[0]
	if rec.GetStatus() != knov1.GapStatus_GAP_STATUS_GAP {
		t.Fatalf("status = %s, want GAP", rec.GetStatus())
	}
	if rec.GetBestAssetId() != "a1" || rec.GetCoveredCount() != 6 {
		t.Fatalf("best = %s covered %d, want a1/6", rec.GetBestAssetId(), rec.GetCoveredCount())
	}
}

func TestComputeGapsUnknownBelowMinClusterCases(t *testing.T) {
	t.Parallel()
	plan, m := gapsFixture()
	// Routed to 4 of 8: below MinClusterCases(5), the measurement cannot
	// testify. Non-significance is not absence — the verdict is UNKNOWN, and
	// no Asset is reported.
	plan.Routed = []value.AssetRouting{routed("a1", "c1", "c2", "c3", "c4")}
	m["a1"] = gapValuation("a1", 0.5, 0.2, 0.8, 10)

	rec := ComputeGaps(plan, vals(m)).GetClusters()[0]
	if rec.GetStatus() != knov1.GapStatus_GAP_STATUS_UNKNOWN {
		t.Fatalf("status = %s, want UNKNOWN", rec.GetStatus())
	}
	if rec.GetBestAssetId() != "" || rec.GetCoveredCount() != 0 {
		t.Fatalf("best = %q covered %d, want empty/0", rec.GetBestAssetId(), rec.GetCoveredCount())
	}
}

func TestComputeGapsBoundaryAtMinClusterCases(t *testing.T) {
	t.Parallel()
	plan, m := gapsFixture()
	plan.Routed = []value.AssetRouting{routed("a1", "c1", "c2", "c3", "c4", "c5")}
	m["a1"] = gapValuation("a1", 0.5, 0.2, 0.8, 10)

	if rec := ComputeGaps(plan, vals(m)).GetClusters()[0]; rec.GetStatus() != knov1.GapStatus_GAP_STATUS_IMPROVED {
		t.Fatalf("5 covered = status %s, want IMPROVED (the boundary is inclusive)", rec.GetStatus())
	}
	plan.Routed = []value.AssetRouting{routed("a1", "c1", "c2", "c3", "c4")}
	if rec := ComputeGaps(plan, vals(m)).GetClusters()[0]; rec.GetStatus() != knov1.GapStatus_GAP_STATUS_UNKNOWN {
		t.Fatalf("4 covered = status %s, want UNKNOWN (one below the boundary is not covering)", rec.GetStatus())
	}
}

func TestComputeGapsUnknownUnderpoweredCovering(t *testing.T) {
	t.Parallel()
	plan, m := gapsFixture()
	// Routed to all 8, but the measurement has 3 pairs: the covering
	// measurement is underpowered, so the interval cannot separate zero from
	// a real effect. UNKNOWN, not GAP.
	plan.Routed = []value.AssetRouting{routed("a1", "c1", "c2", "c3", "c4", "c5", "c6", "c7", "c8")}
	m["a1"] = gapValuation("a1", 0.5, 0.2, 0.8, 3)

	rec := ComputeGaps(plan, vals(m)).GetClusters()[0]
	if rec.GetStatus() != knov1.GapStatus_GAP_STATUS_UNKNOWN {
		t.Fatalf("status = %s, want UNKNOWN (underpowered)", rec.GetStatus())
	}
	if rec.GetBestAssetId() != "" {
		t.Fatalf("best = %q, want empty: an underpowered measurement is not reported as the number", rec.GetBestAssetId())
	}
	if rec.GetCoveredCount() != 8 {
		t.Fatalf("covered = %d, want 8: the routing DID reach the cluster — the shape of "+
			"the record must say 'routed but underpowered', not 'not routed'",
			rec.GetCoveredCount())
	}
}

func TestComputeGapsUnknownWithoutMeasurement(t *testing.T) {
	t.Parallel()
	plan, _ := gapsFixture()
	// Routed enough, but no Valuation at all: the run stopped before this
	// Asset was measured, or the source predates valuations. Same UNKNOWN.
	plan.Routed = []value.AssetRouting{routed("a1", "c1", "c2", "c3", "c4", "c5")}

	rec := ComputeGaps(plan, nil).GetClusters()[0]
	if rec.GetStatus() != knov1.GapStatus_GAP_STATUS_UNKNOWN {
		t.Fatalf("status = %s, want UNKNOWN (no measurement)", rec.GetStatus())
	}
}

func TestComputeGapsReportsBestCoveringAsset(t *testing.T) {
	t.Parallel()
	plan, m := gapsFixture()
	// Two covering Assets; a2's delta is stronger. The reported number is
	// a2's CI — one number per cluster, the strongest effect it saw.
	plan.Routed = []value.AssetRouting{
		routed("a1", "c1", "c2", "c3", "c4", "c5"),
		routed("a2", "c1", "c2", "c3", "c4", "c5", "c6"),
	}
	m["a1"] = gapValuation("a1", 0.3, 0.1, 0.5, 10)
	m["a2"] = gapValuation("a2", 0.7, 0.4, 1.0, 10)

	rec := ComputeGaps(plan, vals(m)).GetClusters()[0]
	if rec.GetBestAssetId() != "a2" || rec.GetBestDelta() != 0.7 {
		t.Fatalf("best = %s delta %v, want a2/0.7", rec.GetBestAssetId(), rec.GetBestDelta())
	}
	if rec.GetBestInterval().GetLow() != 0.4 || rec.GetBestInterval().GetHigh() != 1.0 {
		t.Fatalf("best interval = [%v %v], want [0.4 1.0]",
			rec.GetBestInterval().GetLow(), rec.GetBestInterval().GetHigh())
	}
	if rec.GetCoveredCount() != 6 {
		t.Fatalf("covered = %d, want 6 (the best asset's own intersection)", rec.GetCoveredCount())
	}
}

func TestComputeGapsBestTieBreaksByAssetID(t *testing.T) {
	t.Parallel()
	plan, m := gapsFixture()
	plan.Routed = []value.AssetRouting{
		routed("b2", "c1", "c2", "c3", "c4", "c5"),
		routed("b1", "c1", "c2", "c3", "c4", "c5"),
	}
	m["b2"] = gapValuation("b2", 0.4, 0.1, 0.7, 10)
	m["b1"] = gapValuation("b1", 0.4, 0.1, 0.7, 10)

	rec := ComputeGaps(plan, vals(m)).GetClusters()[0]
	if rec.GetBestAssetId() != "b1" {
		t.Fatalf("best = %s, want b1 (ties by Asset ID, not by input order)", rec.GetBestAssetId())
	}
}

func TestComputeGapsNegativeDeltaIsImprovedToo(t *testing.T) {
	t.Parallel()
	plan, m := gapsFixture()
	// A significantly NEGATIVE delta is also an improvement claim the report
	// must surface — a cluster where a spend made things worse is the one
	// worth reading.
	plan.Routed = []value.AssetRouting{routed("a1", "c1", "c2", "c3", "c4", "c5")}
	m["a1"] = gapValuation("a1", -0.4, -0.6, -0.2, 10)

	rec := ComputeGaps(plan, vals(m)).GetClusters()[0]
	if rec.GetStatus() != knov1.GapStatus_GAP_STATUS_IMPROVED {
		t.Fatalf("status = %s, want IMPROVED (significant in either direction)", rec.GetStatus())
	}
}

func TestComputeGapsOneSidedIntervalsReadTheirBound(t *testing.T) {
	t.Parallel()
	// LOWER: only low means anything; high is written 0 and must be ignored.
	plan, m := gapsFixture()
	plan.Routed = []value.AssetRouting{routed("a1", "c1", "c2", "c3", "c4", "c5", "c6")}
	m["a1"] = &knov1.Valuation{
		AssetId:   "a1",
		DeltaGoal: 0.3,
		DeltaInterval: &knov1.Interval{
			Low:       0.1,
			High:      0,
			Level:     0.95,
			Sidedness: knov1.Sidedness_SIDEDNESS_LOWER,
			NPairs:    int32Ptr(10),
		},
	}
	if rec := ComputeGaps(plan, vals(m)).GetClusters()[0]; rec.GetStatus() != knov1.GapStatus_GAP_STATUS_IMPROVED {
		t.Fatalf("LOWER low=0.1: status %s, want IMPROVED", rec.GetStatus())
	}
	// UPPER: the meaningless low bound (written > 0) must not read as
	// significant; only high < 0 is.
	plan.Routed = []value.AssetRouting{routed("a1", "c1", "c2", "c3", "c4", "c5", "c6")}
	m["a1"] = &knov1.Valuation{
		AssetId:   "a1",
		DeltaGoal: -0.1,
		DeltaInterval: &knov1.Interval{
			Low:       0.5,
			High:      0,
			Level:     0.95,
			Sidedness: knov1.Sidedness_SIDEDNESS_UPPER,
			NPairs:    int32Ptr(10),
		},
	}
	if rec := ComputeGaps(plan, vals(m)).GetClusters()[0]; rec.GetStatus() != knov1.GapStatus_GAP_STATUS_GAP {
		t.Fatalf("UPPER low=0.5 high=0: status %s, want GAP (the meaningless bound is not evidence)", rec.GetStatus())
	}
}

func TestComputeGapsClustersInPlanOrder(t *testing.T) {
	t.Parallel()
	plan := &value.Plan{
		Clusters: []value.ClusterSnapshot{
			{Tag: "billing", CaseIDs: []string{"c1", "c2", "c3", "c4", "c5"}},
			{Tag: "refunds", CaseIDs: []string{"c1", "c2", "c3", "c4", "c5"}},
		},
	}
	plan.Routed = []value.AssetRouting{routed("a1", "c1", "c2", "c3", "c4", "c5")}
	m := map[string]*knov1.Valuation{
		"a1": gapValuation("a1", 0.5, 0.2, 0.8, 10),
	}
	got := ComputeGaps(plan, vals(m)).GetClusters()
	if len(got) != 2 || got[0].GetTag() != "billing" || got[1].GetTag() != "refunds" {
		t.Fatalf("cluster order = %q, want the plan's snapshot order", []string{got[0].GetTag(), got[1].GetTag()})
	}
	if !ComputeGaps(plan, vals(m)).GetMultipleTesting() {
		t.Fatal("multiple_testing false for two clusters, want true (labeled, never hidden)")
	}
}

func TestComputeGapsDeterministicUnderShuffledInput(t *testing.T) {
	t.Parallel()
	plan, m := gapsFixture()
	plan.Routed = []value.AssetRouting{
		routed("a1", "c1", "c2", "c3", "c4", "c5"),
		routed("a2", "c1", "c2", "c3", "c4", "c5", "c6", "c7"),
	}
	m["a1"] = gapValuation("a1", 0.3, 0.1, 0.5, 10)
	m["a2"] = gapValuation("a2", 0.2, -0.1, 0.5, 10)

	want := ComputeGaps(plan, vals(m))
	// A shuffled Valuations slice (as the store's read order might deliver)
	// must not move the verdict or the reported Asset.
	vals := vals(m)
	rng := rand.New(rand.NewSource(42))
	rng.Shuffle(len(vals), func(i, j int) { vals[i], vals[j] = vals[j], vals[i] })
	got := ComputeGaps(plan, vals)
	if got.GetClusters()[0].GetBestAssetId() != want.GetClusters()[0].GetBestAssetId() {
		t.Fatalf("best = %s, want %s under shuffled input",
			got.GetClusters()[0].GetBestAssetId(), want.GetClusters()[0].GetBestAssetId())
	}
	if got.GetClusters()[0].GetStatus() != want.GetClusters()[0].GetStatus() {
		t.Fatalf("status = %s, want %s under shuffled input",
			got.GetClusters()[0].GetStatus(), want.GetClusters()[0].GetStatus())
	}
}

// TestGapsComputesNothingFromAnOldPlanBlob pins the gob append-tolerance
// promise: a plan recorded BEFORE the Clusters field decodes with an empty
// Clusters, and ComputeGaps on it yields an empty record — the "no cluster
// data for this run" answer, never a guess.
func TestGapsComputesNothingFromAnOldPlanBlob(t *testing.T) {
	t.Parallel()

	// The pre-Clusters Plan, as gob serialized it before this change.
	old := &preClusterPlan{
		Mode:                value.ModeTagOverlap,
		Routed:              []value.AssetRouting{routed("a1", "c1", "c2", "c3", "c4", "c5")},
		EligibleCases:       8,
		ControlCaseIDs:      []string{"r1", "r2"},
		ControlUnderpowered: true,
		Trials:              3,
		Seed:                7,
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(old); err != nil {
		t.Fatalf("encoding the old plan: %v", err)
	}

	var decoded value.Plan
	if err := gob.NewDecoder(&buf).Decode(&decoded); err != nil {
		t.Fatalf("decoding the old plan: %v", err)
	}
	if decoded.Clusters != nil {
		t.Fatalf("old blob decoded with %d clusters, want none", len(decoded.Clusters))
	}
	if len(decoded.Routed) != 1 || decoded.Seed != 7 {
		t.Fatalf("old blob lost pre-existing fields: Routed=%d Seed=%d", len(decoded.Routed), decoded.Seed)
	}

	// The whole point: the old run computes an empty record, and the store
	// layer reports it as absent rather than fabricating verdicts.
	gaps := ComputeGaps(&decoded, nil)
	if len(gaps.GetClusters()) != 0 || gaps.GetMultipleTesting() {
		t.Fatalf("old plan produced %d clusters (multiple_testing %v), want none",
			len(gaps.GetClusters()), gaps.GetMultipleTesting())
	}
}

// preClusterPlan is the gob wire shape of value.Plan as it existed before
// the Clusters field — frozen here so the append-tolerance claim is tested
// against the actual old shape, not against a copy of today's struct.
type preClusterPlan struct {
	Mode                value.Mode
	Routed              []value.AssetRouting
	EligibleCases       int
	ControlCaseIDs      []string
	ControlUnderpowered bool
	MinDetectableHarm   float64
	Trials              int32
	Seed                int64
}

// TestPlanBlobRoundTripsClusters covers the new direction: a plan WITH
// clusters survives gob, so a Value run's recorded plan is what Export
// decodes and computes from.
func TestPlanBlobRoundTripsClusters(t *testing.T) {
	t.Parallel()
	plan := &value.Plan{
		Clusters: []value.ClusterSnapshot{
			{Tag: "billing", CaseIDs: []string{"c1", "c2"}, NDropped: 1},
		},
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(plan); err != nil {
		t.Fatalf("encoding: %v", err)
	}
	var back value.Plan
	if err := gob.NewDecoder(&buf).Decode(&back); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(back.Clusters) != 1 || back.Clusters[0].Tag != "billing" ||
		back.Clusters[0].NDropped != 1 || len(back.Clusters[0].CaseIDs) != 2 {
		t.Fatalf("round trip changed the snapshot: %+v", back.Clusters)
	}
}
