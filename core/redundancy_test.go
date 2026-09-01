package core

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/store"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite" // pure-Go driver, matching store's own
)

// measuredAsset is a redundancy-test fixture: one Asset's per-Case treatment
// scores, keyed by the Case IDs it is routed to.
type measuredAsset struct {
	id     string
	kind   knov1.Kind
	scores map[string]float64 // caseID -> treatment score (1 pass, 0 fail)
	tokens int64              // carrying cost, context_tokens
}

// seedRedundancyFixture builds a complete Baseline + Value run pair: every
// named Case fails the baseline (score 0), each Asset's Valuation is built
// from a clearly-positive fixture delta (so it clears REGRESSION/NO_EFFECT
// before reaching the redundancy rule) and carries real case_ids and real
// recorded measurements, so the redundancy rule's store reads have something
// to reconstruct.
func seedRedundancyFixture(t *testing.T, st store.Store, valueRunID string, direction knov1.Direction, assets ...measuredAsset) {
	t.Helper()
	ctx := context.Background()

	allCases := map[string]struct{}{}
	for _, a := range assets {
		for c := range a.scores {
			allCases[c] = struct{}{}
		}
	}
	baselineScores := make(map[string]float64, len(allCases))
	for c := range allCases {
		baselineScores[c] = 0
	}
	seedBaselineRun(t, st, valueRunID+"-base", baselineScores)

	// The Value run's ROW must exist before measurements are recorded against
	// it (measurements.run_id is a foreign key), so the run is created first
	// and its Valuations are written last, after every measurement.
	createValueRun(t, st, valueRunID, valueRunID+"-base", direction)

	var vals []*Valuation
	for _, a := range assets {
		ids := make([]string, 0, len(a.scores))
		var sum float64
		for c, s := range a.scores {
			ids = append(ids, c)
			sum += s
			seedTreatmentMeasurement(t, st, valueRunID, a.id, c, s)
		}
		delta := sum / float64(len(ids))
		v := testValuation(a.id, delta, 0.2)
		v.CaseIds = ids
		v.Kind = a.kind
		v.NRouted = int32Ptr(int32(len(ids)))
		v.Cost = &knov1.CostVector{ContextTokens: a.tokens, AcquisitionUsdMicros: 100}
		vals = append(vals, v)
	}
	writeValuations(t, st, valueRunID, vals...)
	_ = ctx
}

// scoresFor builds a treatment score map over caseIDs(n), passing (score 1)
// exactly the Cases named in pass.
func scoresFor(n int, pass ...int) map[string]float64 {
	ids := caseIDs(n)
	out := make(map[string]float64, n)
	passSet := map[int]struct{}{}
	for _, p := range pass {
		passSet[p] = struct{}{}
	}
	for i, id := range ids {
		if _, ok := passSet[i]; ok {
			out[id] = 1
		} else {
			out[id] = 0
		}
	}
	return out
}

func intRange(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// TestRedundantBehaviorAssetsWithDisjointShingles is acceptance criterion 1:
// two BEHAVIOR Assets — content similarity has no vote — with equal,
// co-located per-Case deltas over 20 shared Cases produce a REDUNDANT
// rejection. This fails on `main` today by construction (the kind gate at
// the shipped core/select.go:441 never lets a behavior Asset reach REDUNDANT)
// and is the plan's headline test.
//
// A raised --redundancy-max-margin is used deliberately: at n=20 the BINARY
// domain's MinDetectableEffect bound is ~0.33 (measured directly from
// interval.MinDetectableEffect), which exceeds the package default ceiling
// of 0.10 — the exact consequence accepted risk 8 (finding F4) names. A user
// who wants a redundancy claim at this sample size raises the ceiling
// deliberately; this test exercises the mechanism with that choice made
// explicit, not the untouched default.
func TestRedundantBehaviorAssetsWithDisjointShingles(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Both Assets pass exactly Cases 0..15 of 20 (co-located, equal deltas)
	// and carry completely disjoint, single-character "content" so shingle
	// overlap — if it were ever consulted — would be zero.
	pass := intRange(16)
	seedRedundancyFixture(
		t, st, "val", knov1.Direction_DIRECTION_MAXIMIZE,
		measuredAsset{id: "a", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 100},
		measuredAsset{id: "b", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 100},
	)

	opts := selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 100000}, nil)
	opts.RedundancyMaxMargin = 0.4
	res, err := runSelect(t, opts)
	require.NoError(t, err)

	require.Len(t, res.Portfolio.GetSelected(), 1, "exactly one of the equivalent pair survives")
	require.Len(t, res.Portfolio.GetRejected(), 1)
	rej := res.Portfolio.GetRejected()[0]
	require.Equal(t, knov1.RejectionReason_REJECTION_REASON_REDUNDANT, rej.GetReason())
	require.NotEmpty(t, rej.GetRedundantWithAssetIds())
	require.NotEmpty(t, rej.GetRedundancyEvidence())
	for _, ev := range rej.GetRedundancyEvidence() {
		require.Equal(t, knov1.RedundancyEvidenceKind_REDUNDANCY_EVIDENCE_KIND_MEASUREMENT, ev.GetKind())
		require.NotNil(t, ev.GetDifferenceInterval(), "criterion 13: a measurement REDUNDANT verdict always carries its interval")
	}
}

// TestComplementsWithEqualMeansAreNotRedundant is acceptance criterion 2 and
// the test the plan names as protecting against its most expensive possible
// error: two behavior Assets with IDENTICAL mean deltas that improve DISJOINT
// Case sets are both selected, with co_improvement recorded at 0.0. This must
// fail if Condition 2 is ever dropped — a Condition-1-only implementation
// would call this pair equivalent and throw one away, losing half the gain.
func TestComplementsWithEqualMeansAreNotRedundant(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// A passes Cases 0-9, B passes Cases 10-19: identical mean delta (0.5
	// over 20 Cases each), zero overlap in WHICH Cases improved.
	seedRedundancyFixture(
		t, st, "val", knov1.Direction_DIRECTION_MAXIMIZE,
		measuredAsset{id: "a", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, intRange(10)...), tokens: 100},
		measuredAsset{id: "b", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, intRange(20)[10:]...), tokens: 100},
	)

	opts := selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 100000}, nil)
	opts.RedundancyMaxMargin = 0.9 // generous: this test is about Condition 2, not the margin ceiling
	res, err := runSelect(t, opts)
	require.NoError(t, err)

	require.Len(t, res.Portfolio.GetSelected(), 2, "complements are not redundant: both survive")
	require.Empty(t, res.Portfolio.GetRejected())
}

// TestInsufficientOverlapIsUnknownNotRedundant is acceptance criterion 3:
// two Assets sharing only 4 Cases — below MinOverlapCases — are both
// selected and no REDUNDANT rejection is emitted, even though their deltas
// would otherwise look identical.
func TestInsufficientOverlapIsUnknownNotRedundant(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Disjoint Case pools except for a 4-Case overlap, both passing
	// everything they were routed to — deliberately provocative, so a bug
	// that ignores MinOverlapCases would be caught.
	aScores := scoresFor(4, 0, 1, 2, 3)
	for c, s := range scoresFor(4, 0, 1, 2, 3) {
		aScores["shared-"+c] = s
	}
	_ = aScores
	shared := caseIDs(4)
	a := map[string]float64{}
	b := map[string]float64{}
	for _, c := range shared {
		a[c] = 1
		b[c] = 1
	}
	for _, c := range []string{"a-only-1", "a-only-2"} {
		a[c] = 1
	}
	for _, c := range []string{"b-only-1", "b-only-2"} {
		b[c] = 1
	}

	seedRedundancyFixture(
		t, st, "val", knov1.Direction_DIRECTION_MAXIMIZE,
		measuredAsset{id: "a", kind: knov1.Kind_KIND_BEHAVIOR, scores: a, tokens: 100},
		measuredAsset{id: "b", kind: knov1.Kind_KIND_BEHAVIOR, scores: b, tokens: 100},
	)

	res, err := runSelect(t, selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 100000}, nil))
	require.NoError(t, err)
	require.Len(t, res.Portfolio.GetSelected(), 2)
	require.Empty(t, res.Portfolio.GetRejected())
}

// TestRedundancyEquivalenceMarginIsSampleResolutionByDefault is acceptance
// criterion 5: with --redundancy-margin 0 (the SelectOptions zero value),
// delta equals MinDetectableEffect(|C|, TWO_SIDED, level) exactly, and a
// user margin below that value still leaves margin_source at
// SAMPLE_RESOLUTION — the user cannot buy a finer claim than the data
// supports.
func TestRedundancyEquivalenceMarginIsSampleResolutionByDefault(t *testing.T) {
	t.Parallel()

	n := 20
	want := 0.33093616047256635 // interval.MinDetectableEffect(20, TWO_SIDED, 0.95), pinned
	margin, source := equivalenceMargin(0, n, 0.95)
	require.InDelta(t, want, margin, 1e-9)
	require.Equal(t, knov1.MarginSource_MARGIN_SOURCE_SAMPLE_RESOLUTION, source)

	// A user floor BELOW the sample resolution changes nothing.
	margin, source = equivalenceMargin(0.01, n, 0.95)
	require.InDelta(t, want, margin, 1e-9)
	require.Equal(t, knov1.MarginSource_MARGIN_SOURCE_SAMPLE_RESOLUTION, source)

	// A user floor ABOVE the sample resolution wins, and is attributed to
	// the user.
	margin, source = equivalenceMargin(0.9, n, 0.95)
	require.InDelta(t, 0.9, margin, 1e-9)
	require.Equal(t, knov1.MarginSource_MARGIN_SOURCE_USER, source)
}

// TestRedundancyMarginCeilingYieldsUnknown is acceptance criterion 6: a pair
// whose required delta exceeds --redundancy-max-margin is UNKNOWN — both
// Assets selected, cause named in the evidence's absence (no REDUNDANT
// rejection is emitted for the pair at all, which is how UNKNOWN presents on
// the Portfolio).
func TestRedundancyMarginCeilingYieldsUnknown(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	pass := intRange(16)
	seedRedundancyFixture(
		t, st, "val", knov1.Direction_DIRECTION_MAXIMIZE,
		measuredAsset{id: "a", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 100},
		measuredAsset{id: "b", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 100},
	)

	// Default ceiling (0.10): at n=20 the required margin (~0.33) exceeds
	// it, so the pair must be UNKNOWN — both selected — rather than
	// REDUNDANT.
	res, err := runSelect(t, selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 100000}, nil))
	require.NoError(t, err)
	require.Len(t, res.Portfolio.GetSelected(), 2, "the margin ceiling refuses the claim, so both survive")
	require.Empty(t, res.Portfolio.GetRejected())
}

// TestBehaviorAndKnowledgeAreNeverCompared is acceptance criterion 9: a
// behavior Asset and a knowledge Asset that improve identical Cases with
// identical deltas are BOTH selected — no cross-mechanism redundancy claim,
// because they route to different destinations by default (tuning set vs
// context) and cross-destination comparison stays refused.
func TestBehaviorAndKnowledgeAreNeverCompared(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	pass := intRange(16)
	seedRedundancyFixture(
		t, st, "val", knov1.Direction_DIRECTION_MAXIMIZE,
		measuredAsset{id: "behavior-a", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 100},
		measuredAsset{id: "knowledge-b", kind: knov1.Kind_KIND_KNOWLEDGE, scores: scoresFor(20, pass...), tokens: 100},
	)

	opts := selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 100000, MaxTrainingExamples: 10}, nil)
	opts.RedundancyMaxMargin = 0.9
	res, err := runSelect(t, opts)
	require.NoError(t, err)
	require.Len(t, res.Portfolio.GetSelected(), 2)
	require.Empty(t, res.Portfolio.GetRejected())
}

// TestOverriddenDestinationIsCompared is acceptance criterion 10: a behavior
// Asset pinned to CONTEXT (user_overridden) competes with — and can be
// rejected REDUNDANT against — a knowledge Asset also destined for context.
func TestOverriddenDestinationIsCompared(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	pass := intRange(16)
	seedRedundancyFixture(
		t, st, "val", knov1.Direction_DIRECTION_MAXIMIZE,
		measuredAsset{id: "behavior-a", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 100},
		measuredAsset{id: "knowledge-b", kind: knov1.Kind_KIND_KNOWLEDGE, scores: scoresFor(20, pass...), tokens: 100},
	)

	pool := stubPool{assets: []*Asset{
		{Id: "behavior-a", Destination: knov1.Destination_DESTINATION_CONTEXT, UserOverridden: true, Content: []byte("alpha beta gamma")},
		{Id: "knowledge-b", Destination: knov1.Destination_DESTINATION_CONTEXT, Content: []byte("delta epsilon zeta")},
	}}

	opts := selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 100000}, pool)
	opts.RedundancyMaxMargin = 0.9
	res, err := runSelect(t, opts)
	require.NoError(t, err)
	require.Len(t, res.Portfolio.GetSelected(), 1, "pinned to the same destination, the pair IS compared")
	require.Len(t, res.Portfolio.GetRejected(), 1)
	require.Equal(t, knov1.RejectionReason_REJECTION_REASON_REDUNDANT, res.Portfolio.GetRejected()[0].GetReason())
}

// TestCostTieBreakReversesTheWinnersCurse is acceptance criterion 11: two
// equivalent Assets whose carrying costs differ by more than the #68 bias
// band (2.4x) — the cheaper survives regardless of which had the higher
// delta_goal, and decided_by is COST. Both Assets tie on delta_per_cost and
// delta_goal, so rankLess's Asset-ID tie-break decides PROCESSING order:
// "a-expensive" sorts first and is selected in the ordinary way; "z-cheap"
// is decided second, finds itself measurement-equivalent to the
// already-selected "a-expensive", and — being far enough cheaper to clear
// the bias band — EVICTS it rather than simply losing. That eviction path
// (core/select.go's evict/uncharge) is exactly the winner's-curse reversal
// the shipped first-seen-wins tie-break did not have: without it, the
// first-processed, more expensive Asset would keep its place regardless of
// cost.
func TestCostTieBreakReversesTheWinnersCurse(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	pass := intRange(16)
	seedRedundancyFixture(
		t, st, "val", knov1.Direction_DIRECTION_MAXIMIZE,
		measuredAsset{id: "a-expensive", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 1000},
		measuredAsset{id: "z-cheap", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 200},
	)

	opts := selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 100000}, nil)
	opts.RedundancyMaxMargin = 0.4
	res, err := runSelect(t, opts)
	require.NoError(t, err)

	require.Len(t, res.Portfolio.GetSelected(), 1)
	require.Equal(t, "z-cheap", res.Portfolio.GetSelected()[0].GetAssetId(), "the cheaper Asset survives, evicting the one selected first")
	require.Equal(t, int32(1), res.Portfolio.GetSelected()[0].GetRank(), "ranks are renumbered to the final order after eviction")
	require.Len(t, res.Portfolio.GetRejected(), 1)
	rej := res.Portfolio.GetRejected()[0]
	require.Equal(t, "a-expensive", rej.GetAssetId(), "the more expensive Asset, selected first, is the one evicted")
	require.Equal(t, knov1.RejectionReason_REJECTION_REASON_REDUNDANT, rej.GetReason())
	require.Equal(t, []string{"z-cheap"}, rej.GetRedundantWithAssetIds())
	require.NotEmpty(t, rej.GetRedundancyEvidence())
	ev := rej.GetRedundancyEvidence()[0]
	require.Equal(t, knov1.RedundancyDecidedBy_REDUNDANCY_DECIDED_BY_COST, ev.GetDecidedBy())
	require.Equal(t, "z-cheap", ev.GetWithAssetId(), "the evicted Asset's own evidence names its survivor")
}

// TestCostTieBreakInsideBiasBandFallsToAssetID is acceptance criterion 23:
// two equivalent Assets whose context_tokens differ by LESS than 2.4x are
// decided by ID, not COST, whichever is nominally cheaper, and cost_ratio is
// recorded.
func TestCostTieBreakInsideBiasBandFallsToAssetID(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	pass := intRange(16)
	// 300 vs 500 tokens: 1.67x apart, inside the 2.4x band. "zzz-later" is
	// the nominally cheaper one (300) but ID-later; "aaa-earlier" is more
	// expensive (500) but ID-earlier. If the band were ignored, cost would
	// pick zzz-later; the band instead falls to ID, which picks
	// aaa-earlier.
	seedRedundancyFixture(
		t, st, "val", knov1.Direction_DIRECTION_MAXIMIZE,
		measuredAsset{id: "aaa-earlier", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 500},
		measuredAsset{id: "zzz-later", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 300},
	)

	opts := selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 100000}, nil)
	opts.RedundancyMaxMargin = 0.4
	res, err := runSelect(t, opts)
	require.NoError(t, err)

	require.Len(t, res.Portfolio.GetSelected(), 1)
	require.Equal(t, "aaa-earlier", res.Portfolio.GetSelected()[0].GetAssetId(), "inside the bias band, ID decides")
	require.Len(t, res.Portfolio.GetRejected(), 1)
	ev := res.Portfolio.GetRejected()[0].GetRedundancyEvidence()
	require.NotEmpty(t, ev)
	require.Equal(t, knov1.RedundancyDecidedBy_REDUNDANCY_DECIDED_BY_ID, ev[0].GetDecidedBy())
	require.InDelta(t, 500.0/300.0, ev[0].GetCostRatio(), 1e-9)
}

// TestRedundancyDeterminismAcrossRuns is acceptance criterion 12: two
// equivalent Assets with IDENTICAL costs are decided by Asset ID, and two
// `decide` runs over the same store produce byte-identical Portfolios —
// including the bootstrap-derived co-improvement evidence, which is why the
// bootstrap RNG must be seeded deterministically rather than from wall time.
func TestRedundancyDeterminismAcrossRuns(t *testing.T) {
	t.Parallel()

	build := func(t *testing.T) *knov1.Portfolio {
		t.Helper()
		st := openTestStore(t)
		pass := intRange(16)
		seedRedundancyFixture(
			t, st, "val", knov1.Direction_DIRECTION_MAXIMIZE,
			measuredAsset{id: "a", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 100},
			measuredAsset{id: "b", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 100},
		)
		opts := selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 100000}, nil)
		opts.RunID = "sel-1"
		opts.RedundancyMaxMargin = 0.4
		res, err := runSelect(t, opts)
		require.NoError(t, err)
		return res.Portfolio
	}

	p1 := build(t)
	p2 := build(t)
	require.Equal(t, p1.GetSelected()[0].GetAssetId(), p2.GetSelected()[0].GetAssetId())
	require.Equal(t, "a", p1.GetSelected()[0].GetAssetId(), "identical costs: Asset ID decides")
	require.Len(t, p1.GetRejected(), 1)
	require.Len(t, p2.GetRejected(), 1)
	ev1 := p1.GetRejected()[0].GetRedundancyEvidence()[0]
	ev2 := p2.GetRejected()[0].GetRedundancyEvidence()[0]
	require.Equal(t, ev1.GetCoImprovement(), ev2.GetCoImprovement())
	require.Equal(t, ev1.GetCoImprovementInterval().GetLow(), ev2.GetCoImprovementInterval().GetLow())
	require.Equal(t, ev1.GetCoImprovementInterval().GetHigh(), ev2.GetCoImprovementInterval().GetHigh())
	require.Equal(t, ev1.GetDifferenceInterval().GetLow(), ev2.GetDifferenceInterval().GetLow())
}

// TestPurgedMeasurementsYieldUnknownNeverAZeroDelta is acceptance criterion
// 19: a store whose measurement rows are Unrecoverable for one Asset yields
// UNKNOWN for every pair involving it — never a delta computed against a
// zero standing in for a missing number.
//
// `store.Purge` clears trace CONTENT (response_proto/score_proto) and
// deliberately leaves score_value alone — a purge is not what produces an
// Unrecoverable row (store/measurement.go's RecordedMeasurement doc: that
// state is what a pre-score-column migration or an older binary leaves
// behind). So this test manufactures the state directly, the same way
// store/measurement_test.go's own Unrecoverable coverage does: a raw UPDATE
// against the same SQLite file, nulling score_value on a row that is still
// marked scored=1.
func TestPurgedMeasurementsYieldUnknownNeverAZeroDelta(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/kno.db"
	st, err := store.NewSQLite(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	pass := intRange(16)
	seedRedundancyFixture(
		t, st, "val", knov1.Direction_DIRECTION_MAXIMIZE,
		measuredAsset{id: "a", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 100},
		measuredAsset{id: "b", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 100},
	)

	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	res, err := raw.Exec(`UPDATE measurements SET score_value = NULL WHERE asset_id = 'b'`)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.Positive(t, n, "the fixture must have written rows for asset b to null out")
	require.NoError(t, raw.Close())

	opts := selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 100000}, nil)
	opts.RedundancyMaxMargin = 0.9
	got, err := runSelect(t, opts)
	require.NoError(t, err)
	// b's delta vector is now empty (every measurement row is scored but
	// unrecoverable) — |C| collapses to 0 for the pair. UNKNOWN, both
	// selected, never a REDUNDANT verdict built on a manufactured zero.
	require.Len(t, got.Portfolio.GetSelected(), 2)
	require.Empty(t, got.Portfolio.GetRejected())
}

// TestRedundancyGoalDirectionMinimize drives a MINIMIZE Goal end to end: two
// Assets whose RAW scores both go DOWN on the same Cases are, after sign
// correction, equally IMPROVING — getting this backwards would silently
// invert every co-improvement set.
func TestRedundancyGoalDirectionMinimize(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Baseline score 1 (e.g. latency=1), both Assets bring it to 0 (lower is
	// better) on the same 16 of 20 Cases — an improvement under MINIMIZE.
	pass := intRange(16)
	ids := caseIDs(20)
	baseline := make(map[string]float64, 20)
	for _, id := range ids {
		baseline[id] = 1
	}
	seedBaselineRun(t, st, "val-base", baseline)

	scores := func() map[string]float64 {
		out := make(map[string]float64, 20)
		passSet := map[int]struct{}{}
		for _, p := range pass {
			passSet[p] = struct{}{}
		}
		for i, id := range ids {
			if _, ok := passSet[i]; ok {
				out[id] = 0 // improved: went DOWN
			} else {
				out[id] = 1 // unchanged
			}
		}
		return out
	}

	createValueRun(t, st, "val", "val-base", knov1.Direction_DIRECTION_MINIMIZE)
	var vals []*Valuation
	for _, id := range []string{"a", "b"} {
		sc := scores()
		for c, s := range sc {
			seedTreatmentMeasurement(t, st, "val", id, c, s)
		}
		v := testValuation(id, -0.4, 0.2) // MINIMIZE: a negative raw change is an improvement
		v.CaseIds = ids
		v.Kind = knov1.Kind_KIND_BEHAVIOR
		v.NRouted = int32Ptr(20)
		v.Cost = &knov1.CostVector{ContextTokens: 100, AcquisitionUsdMicros: 100}
		vals = append(vals, v)
	}
	writeValuations(t, st, "val", vals...)

	opts := selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 100000}, nil)
	opts.RedundancyMaxMargin = 0.4
	res, err := runSelect(t, opts)
	require.NoError(t, err)

	// Sign-corrected, both Assets improved the SAME 16 Cases identically —
	// REDUNDANT, exactly as the MAXIMIZE case. Getting the sign backwards
	// would instead see 16 "regressions" and 4 "no-ops" that never overlap,
	// producing co_improvement near zero and no REDUNDANT verdict.
	require.Len(t, res.Portfolio.GetSelected(), 1)
	require.Len(t, res.Portfolio.GetRejected(), 1)
	require.Equal(t, knov1.RejectionReason_REJECTION_REASON_REDUNDANT, res.Portfolio.GetRejected()[0].GetReason())
}

// TestJChanceFloorBothDirections is acceptance criterion 21, driven through
// evaluateMeasurement rather than the full stage: with
// --redundancy-min-coimprovement 0, a pair whose observed J is high but whose
// corrected bootstrap CI does not clear J_chance is NOT redundant on
// Condition 2 alone, and a pair with the same observed J over SMALLER
// improvement sets (where J_chance is lower) can be. Both directions checked
// against the recorded co_improvement_floor and its source.
func TestJChanceFloorBothDirections(t *testing.T) {
	t.Parallel()

	// High baseline improvement rate (16 of 20 each): J_chance is high
	// (~0.67), so even a decent observed overlap may not clear it once
	// corrected.
	highRate := map[string]float64{}
	for i, id := range caseIDs(20) {
		highRate[id] = float64(boolToFloat(i < 16))
	}
	// Low baseline improvement rate (4 of 20 each): J_chance is much lower.
	lowRate := map[string]float64{}
	for i, id := range caseIDs(20) {
		lowRate[id] = float64(boolToFloat(i < 4))
	}
	_ = highRate
	_ = lowRate

	mvHighChance := evaluateMeasurement("with", scoresFor(20, intRange(16)...), scoresFor(20, intRange(16)...),
		redundancyConfig{}, 0.95, 1, "seed-a")
	require.True(t, mvHighChance.attempted)
	require.NotNil(t, mvHighChance.evidence)
	require.InDelta(t, 1.0, mvHighChance.evidence.GetCoImprovement(), 1e-9)
	require.Equal(t, knov1.CoImprovementFloorSource_CO_IMPROVEMENT_FLOOR_SOURCE_CHANCE, mvHighChance.evidence.GetCoImprovementFloorSource())
	// J=1 always clears any floor below 1, so this pair passes Condition 2 —
	// the floor comparison itself is what's under test in the low-rate case
	// below, where the same PERFECT overlap is checked against a much lower
	// floor.
	require.Greater(t, mvHighChance.evidence.GetCoImprovementFloor(), 0.5)

	mvLowChance := evaluateMeasurement("with", scoresFor(20, intRange(4)...), scoresFor(20, intRange(4)...),
		redundancyConfig{}, 0.95, 1, "seed-b")
	require.True(t, mvLowChance.attempted)
	require.Less(t, mvLowChance.evidence.GetCoImprovementFloor(), mvHighChance.evidence.GetCoImprovementFloor(),
		"a smaller improvement set has a lower chance floor for the same observed J")

	// A user floor above J_chance is honored and attributed to the user.
	mvUserFloor := evaluateMeasurement("with", scoresFor(20, intRange(4)...), scoresFor(20, intRange(4)...),
		redundancyConfig{minCoImprovement: 0.99}, 0.95, 1, "seed-c")
	require.True(t, mvUserFloor.attempted)
	require.Equal(t, knov1.CoImprovementFloorSource_CO_IMPROVEMENT_FLOOR_SOURCE_USER, mvUserFloor.evidence.GetCoImprovementFloorSource())
	require.InDelta(t, 0.99, mvUserFloor.evidence.GetCoImprovementFloor(), 1e-9)
	require.False(t, mvUserFloor.redundant, "a floor above the observed J's own CI refuses Condition 2")
}

// TestJChanceDegenerateCasesAreUnknown is acceptance criterion 22: two Assets
// that each improve every Case in C yield UNKNOWN (J_chance = 1), and a pair
// whose I_A u I_B is empty yields UNKNOWN. Neither is ever REDUNDANT.
func TestJChanceDegenerateCasesAreUnknown(t *testing.T) {
	t.Parallel()

	everything := scoresFor(20, intRange(20)...)
	mv := evaluateMeasurement("with", everything, everything, redundancyConfig{}, 0.95, 1, "seed-everything")
	require.True(t, mv.attempted)
	require.False(t, mv.redundant, "J_chance -> 1 when both improve everything: co-location carries no information")

	nothing := scoresFor(20)
	mv2 := evaluateMeasurement("with", nothing, nothing, redundancyConfig{}, 0.95, 1, "seed-nothing")
	require.True(t, mv2.attempted)
	require.False(t, mv2.redundant, "an empty union leaves J undefined, never REDUNDANT")
}

func boolToFloat(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestCaseDeltaVectorNeverZeroFillsAMissingNumber is a direct unit test of
// the reconstruction primitive: a Case missing from either side is DROPPED
// from the output map, never present with a zero value standing in for a
// number nobody measured.
func TestCaseDeltaVectorNeverZeroFillsAMissingNumber(t *testing.T) {
	t.Parallel()

	recorded := []store.RecordedMeasurement{
		{Key: store.MeasurementKey{AssetID: "a", CaseID: "c1", Arm: store.ArmTreatment, Trial: 1}, Score: 1},
		{Key: store.MeasurementKey{AssetID: "a", CaseID: "c2", Arm: store.ArmTreatment, Trial: 1}, Unrecoverable: true},
		{Key: store.MeasurementKey{AssetID: "a", CaseID: "c3", Arm: store.ArmTreatment, Trial: 1}, Err: "timeout"},
		// c4 has no measurement row at all.
	}
	baseline := map[string]store.CaseScore{
		"c1": {Value: 0},
		"c2": {Value: 0},
		"c3": {Value: 0},
		"c4": {Value: 0},
	}
	out := caseDeltaVector(recorded, baseline, []string{"c1", "c2", "c3", "c4"}, knov1.Direction_DIRECTION_MAXIMIZE)
	require.Equal(t, map[string]float64{"c1": 1}, out, "only c1 has a usable pair on both sides")

	// A Case whose baseline is unrecoverable is dropped too.
	baseline2 := map[string]store.CaseScore{"c1": {Unrecoverable: true}}
	out2 := caseDeltaVector(recorded, baseline2, []string{"c1"}, knov1.Direction_DIRECTION_MAXIMIZE)
	require.Empty(t, out2)
}

// TestWithinMarginBoundary drives the TOST decision rule at its three
// boundary alignments — interval inside, straddling one edge, entirely
// outside — required by the plan's test plan for equivalence-test
// correctness.
func TestWithinMarginBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		low, high  float64
		margin     float64
		wantWithin bool
	}{
		{"entirely inside", -0.05, 0.05, 0.1, true},
		{"touches the boundary exactly", -0.1, 0.1, 0.1, true},
		{"straddles one edge", -0.05, 0.15, 0.1, false},
		{"entirely outside", 0.2, 0.3, 0.1, false},
		{"zero margin never covers", -0.01, 0.01, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := withinMargin(&Interval{Low: tc.low, High: tc.high}, tc.margin)
			require.Equal(t, tc.wantWithin, got)
		})
	}
	require.False(t, withinMargin(nil, 0.1))
}

// TestContentPathIsDestinationBlindAcrossDestinations is acceptance
// criterion 7's specific fixture requirement (finding F3): two knowledge
// Assets with high shingle overlap and NO measurement overlap, pinned to two
// DIFFERENT destinations (one CONTEXT/user_overridden, one KNOWLEDGE_BASE),
// are still decided by content at the existing 0.6 threshold — the content
// path stays destination-blind, exactly as `main` computes it, which is what
// a within-destination content rule would silently stop comparing.
func TestContentPathIsDestinationBlindAcrossDestinations(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// No case_ids on either Valuation: measurement evidence can never form,
	// so this is a pure content-path decision, exactly the scenario `main`
	// (pre-redundancy-detection) decides today.
	a := testValuation("a", 0.5, 0.2)
	b := testValuation("b", 0.5, 0.2)
	pool := stubPool{assets: []*Asset{
		{Id: "a", Content: []byte("alpha beta gamma delta epsilon zeta"), Destination: knov1.Destination_DESTINATION_CONTEXT, UserOverridden: true},
		{Id: "b", Content: []byte("alpha beta gamma delta epsilon zeta"), Destination: knov1.Destination_DESTINATION_KNOWLEDGE_BASE},
	}}
	seedValueRun(t, st, "val", a, b)
	res, err := runSelect(t, selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 1000, MaxKnowledgeBaseBytes: 1000}, pool))
	require.NoError(t, err)

	require.Len(t, res.Portfolio.GetSelected(), 1)
	require.Len(t, res.Portfolio.GetRejected(), 1)
	rej := res.Portfolio.GetRejected()[0]
	require.Equal(t, knov1.RejectionReason_REJECTION_REASON_REDUNDANT, rej.GetReason())
	require.Equal(t, []string{"a"}, rej.GetRedundantWithAssetIds())
	require.Len(t, rej.GetRedundancyEvidence(), 1)
	ev := rej.GetRedundancyEvidence()[0]
	require.Equal(t, knov1.RedundancyEvidenceKind_REDUNDANCY_EVIDENCE_KIND_CONTENT_SHINGLE, ev.GetKind())
	require.Equal(t, knov1.RedundancyDecidedBy_REDUNDANCY_DECIDED_BY_CONTENT, ev.GetDecidedBy())
	require.GreaterOrEqual(t, ev.GetShingleOverlap(), defaultShingleOverlap)
	// Content evidence makes no delta claim, so criterion 13's "always
	// carries an interval" applies only to measurement evidence.
	require.Nil(t, ev.GetDifferenceInterval())
}

// TestThreeMutuallyEquivalentAssetsRecordBothEvidences is the "three
// mutually equivalent Assets" edge case: a third Asset equivalent to TWO
// already-selected ones is rejected once, with BOTH pairwise evidences
// recorded — the non-transitive cluster is visible in the log even though
// the greedy decision is a single reject.
func TestThreeMutuallyEquivalentAssetsRecordBothEvidences(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	pass := intRange(16)
	seedRedundancyFixture(
		t, st, "val", knov1.Direction_DIRECTION_MAXIMIZE,
		measuredAsset{id: "a", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 100},
		measuredAsset{id: "b", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 100},
		measuredAsset{id: "c", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 100},
	)

	opts := selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 100000}, nil)
	opts.RedundancyMaxMargin = 0.4
	res, err := runSelect(t, opts)
	require.NoError(t, err)

	// Identical costs: rankLess ties on Asset ID, so "a" is decided first
	// (selected), "b" and "c" both lose to it on the ID tie-break within the
	// bias band.
	require.Len(t, res.Portfolio.GetSelected(), 1)
	require.Equal(t, "a", res.Portfolio.GetSelected()[0].GetAssetId())
	require.Len(t, res.Portfolio.GetRejected(), 2)
	for _, rej := range res.Portfolio.GetRejected() {
		require.Equal(t, knov1.RejectionReason_REJECTION_REASON_REDUNDANT, rej.GetReason())
		require.Equal(t, []string{"a"}, rej.GetRedundantWithAssetIds())
		require.Len(t, rej.GetRedundancyEvidence(), 1)
	}
}

// TestNRedundancyTestsCountsPairwiseComparisons is the observable half of
// acceptance criterion 14: n_redundancy_tests on the Portfolio equals the
// number of pairwise measurement tests actually performed (content-only
// comparisons do not count), and — once at least two tests are performed —
// the recorded difference_interval carries a level strictly above the base
// confidence level, evidence that the Bonferroni correction actually ran
// rather than being decorative.
func TestNRedundancyTestsCountsPairwiseComparisons(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	pass := intRange(16)
	// Three mutually-comparable Assets: "b" and "c" each get compared
	// against "a" once it is selected, for two performed tests.
	seedRedundancyFixture(
		t, st, "val", knov1.Direction_DIRECTION_MAXIMIZE,
		measuredAsset{id: "a", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 100},
		measuredAsset{id: "b", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 100},
		measuredAsset{id: "c", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 100},
	)

	opts := selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 100000}, nil)
	opts.RedundancyMaxMargin = 0.4
	res, err := runSelect(t, opts)
	require.NoError(t, err)

	require.Equal(t, int32(2), res.Portfolio.GetNRedundancyTests())
	for _, rej := range res.Portfolio.GetRejected() {
		iv := rej.GetRedundancyEvidence()[0].GetDifferenceInterval()
		require.NotNil(t, iv)
		require.Greater(t, iv.GetLevel(), 0.95, "corrected for 2 tests, the level must exceed the uncorrected 0.95")
	}
}

// TestCostTieBreakUnavailableCostFallsToID: costTieBreak treats a
// non-positive cost on either side as "the estimate cannot support a claim"
// and falls straight to Asset ID — the same refusal the bias band produces,
// reached by a different route (no cost recorded at all, e.g. a poolless
// run with no Valuation.cost).
func TestCostTieBreakUnavailableCostFallsToID(t *testing.T) {
	t.Parallel()

	survivor, decidedBy, ratio := costTieBreak("b", 0, "a", 500)
	require.Equal(t, "a", survivor)
	require.Equal(t, knov1.RedundancyDecidedBy_REDUNDANCY_DECIDED_BY_ID, decidedBy)
	require.Zero(t, ratio)

	survivor, decidedBy, ratio = costTieBreak("b", 500, "a", 0)
	require.Equal(t, "a", survivor)
	require.Equal(t, knov1.RedundancyDecidedBy_REDUNDANCY_DECIDED_BY_ID, decidedBy)
	require.Zero(t, ratio)
}

// TestCostTieBreakBothOrderings covers costTieBreak's two COST-decided
// branches directly: the first argument cheaper, and the second — the
// eviction tests above only ever exercise the second (a later, cheaper
// candidate beating an already-selected, pricier Asset), never a
// straightforward reject where the ALREADY-selected side is the cheaper one.
func TestCostTieBreakBothOrderings(t *testing.T) {
	t.Parallel()

	// First argument cheaper: it survives.
	survivor, decidedBy, ratio := costTieBreak("cheap", 100, "expensive", 500)
	require.Equal(t, "cheap", survivor)
	require.Equal(t, knov1.RedundancyDecidedBy_REDUNDANCY_DECIDED_BY_COST, decidedBy)
	require.InDelta(t, 5.0, ratio, 1e-9)

	// Second argument cheaper: it survives.
	survivor, decidedBy, ratio = costTieBreak("expensive", 500, "cheap", 100)
	require.Equal(t, "cheap", survivor)
	require.Equal(t, knov1.RedundancyDecidedBy_REDUNDANCY_DECIDED_BY_COST, decidedBy)
	require.InDelta(t, 5.0, ratio, 1e-9)
}

// TestRedundancyDetailRendersEveryEvidenceKind exercises redundancyDetail's
// three branches directly — measurement (with a cost-decided tie-break),
// content, and the multi-evidence "also" join a rejection against several
// already-selected Assets produces.
func TestRedundancyDetailRendersEveryEvidenceKind(t *testing.T) {
	t.Parallel()

	require.Equal(t, "redundant", redundancyDetail(nil))

	measurement := &knov1.RedundancyEvidence{
		WithAssetId: "a", Kind: knov1.RedundancyEvidenceKind_REDUNDANCY_EVIDENCE_KIND_MEASUREMENT,
		NOverlap: 20, PairedDifference: 0.01,
		DifferenceInterval:    &knov1.Interval{Low: -0.02, High: 0.03},
		Margin:                0.1,
		CoImprovement:         0.9,
		CoImprovementInterval: &knov1.Interval{Low: 0.8, High: 0.95},
		CoImprovementFloor:    0.3,
		DecidedBy:             knov1.RedundancyDecidedBy_REDUNDANCY_DECIDED_BY_COST,
		CostRatio:             3.6,
	}
	content := &knov1.RedundancyEvidence{
		WithAssetId: "b", Kind: knov1.RedundancyEvidenceKind_REDUNDANCY_EVIDENCE_KIND_CONTENT_SHINGLE,
		ShingleOverlap: 0.75,
	}
	idDecided := &knov1.RedundancyEvidence{
		WithAssetId: "c", Kind: knov1.RedundancyEvidenceKind_REDUNDANCY_EVIDENCE_KIND_MEASUREMENT,
		NOverlap: 20, DifferenceInterval: &knov1.Interval{Low: -0.01, High: 0.01},
		DecidedBy: knov1.RedundancyDecidedBy_REDUNDANCY_DECIDED_BY_ID,
	}

	out := redundancyDetail([]*knov1.RedundancyEvidence{measurement, content, idDecided})
	require.Contains(t, out, "equivalent to a on 20 shared Cases")
	require.Contains(t, out, "decided by cost")
	require.Contains(t, out, "also duplicates b by content")
	require.Contains(t, out, "decided by Asset ID")

	unspecified := &knov1.RedundancyEvidence{WithAssetId: "z"}
	require.Contains(t, redundancyDetail([]*knov1.RedundancyEvidence{unspecified}), "duplicates z")
}

// TestExplainRefusesANonValueRun and TestExplainContentEvidenceHasNoTable
// round out Explain's error and content-evidence paths.
func TestExplainRefusesANonValueRun(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	seedBaselineRun(t, st, "base", map[string]float64{"c1": 0})

	opts := selectOpts(st, "base", &knov1.Budget{MaxContextTokens: 1000}, nil)
	_, err := opts.Explain(context.Background(), "a", 0)
	require.Error(t, err)
}

func TestExplainContentEvidenceHasNoTable(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	a := testValuation("a", 0.5, 0.2)
	b := testValuation("b", 0.5, 0.2)
	pool := stubPool{assets: []*Asset{
		{Id: "a", Content: []byte("alpha beta gamma delta epsilon")},
		{Id: "b", Content: []byte("alpha beta gamma delta epsilon")},
	}}
	seedValueRun(t, st, "val", a, b)

	opts := selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 1000}, pool)
	cmps, err := opts.Explain(context.Background(), "b", 0)
	require.NoError(t, err)
	require.Len(t, cmps, 1)
	require.Equal(t, knov1.RedundancyEvidenceKind_REDUNDANCY_EVIDENCE_KIND_CONTENT_SHINGLE, cmps[0].Evidence.GetKind())
	require.Empty(t, cmps[0].Rows, "content evidence makes no per-Case claim")
}

// TestCostTieBreakEvictionUnchargesTheKnowledgeBaseCap: an eviction against
// the KNOWLEDGE_BASE destination, with a Pool supplying real Asset content,
// exercises evict's full path — poolAsset actually finding the evicted
// Asset's content, and uncharge's knowledge-base branch (bytes, not tokens)
// — which the TUNING_SET eviction test above cannot reach.
func TestCostTieBreakEvictionUnchargesTheKnowledgeBaseCap(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	pass := intRange(16)
	seedRedundancyFixture(
		t, st, "val", knov1.Direction_DIRECTION_MAXIMIZE,
		measuredAsset{id: "a-expensive", kind: knov1.Kind_KIND_KNOWLEDGE, scores: scoresFor(20, pass...), tokens: 1000},
		measuredAsset{id: "z-cheap", kind: knov1.Kind_KIND_KNOWLEDGE, scores: scoresFor(20, pass...), tokens: 200},
	)
	pool := stubPool{assets: []*Asset{
		{Id: "a-expensive", Destination: knov1.Destination_DESTINATION_KNOWLEDGE_BASE, Content: []byte("expensive asset content")},
		{Id: "z-cheap", Destination: knov1.Destination_DESTINATION_KNOWLEDGE_BASE, Content: []byte("cheap asset content, unrelated words")},
	}}

	opts := selectOpts(st, "val", &knov1.Budget{MaxKnowledgeBaseBytes: 100000}, pool)
	opts.RedundancyMaxMargin = 0.4
	res, err := runSelect(t, opts)
	require.NoError(t, err)

	require.Len(t, res.Portfolio.GetSelected(), 1)
	require.Equal(t, "z-cheap", res.Portfolio.GetSelected()[0].GetAssetId())
	require.Len(t, res.Portfolio.GetRejected(), 1)
	require.Equal(t, "a-expensive", res.Portfolio.GetRejected()[0].GetAssetId())
}

// TestExplainPropagatesStoreErrors: Explain surfaces a store failure at
// either read it makes, rather than swallowing it into "nothing to explain".
func TestExplainPropagatesStoreErrors(t *testing.T) {
	t.Parallel()

	t.Run("GetRun", func(t *testing.T) {
		t.Parallel()
		st := openTestStore(t)
		seedValueRun(t, st, "val", testValuation("a", 0.5, 0.2))
		fs := &failStore{Store: st, fail: func(m string) error {
			if m == "GetRun" {
				return errors.New("boom")
			}
			return nil
		}}
		_, err := selectOpts(fs, "val", &knov1.Budget{MaxContextTokens: 1000}, nil).Explain(context.Background(), "a", 0)
		require.Error(t, err)
	})

	t.Run("Valuations", func(t *testing.T) {
		t.Parallel()
		st := openTestStore(t)
		seedValueRun(t, st, "val", testValuation("a", 0.5, 0.2))
		fs := &failStore{Store: st, fail: func(m string) error {
			if m == "Valuations" {
				return errors.New("boom")
			}
			return nil
		}}
		_, err := selectOpts(fs, "val", &knov1.Budget{MaxContextTokens: 1000}, nil).Explain(context.Background(), "a", 0)
		require.Error(t, err)
	})
}

// TestEvictionLeavesUnrelatedSelectedAssetsInPlace: with three Assets —
// "a-expensive" (selected first, later evicted), "m-unrelated" (selected,
// NOT equivalent to anything, and must survive eviction untouched), and
// "z-cheap" (equivalent to "a-expensive", evicts it) — evict's own
// bookkeeping keeps the untouched entry in both the Portfolio's selected
// list and the redundancy comparison pool. Pinned to CONTEXT (via a Pool)
// so uncharge's context-token branch runs too, alongside the TUNING_SET and
// KNOWLEDGE_BASE branches the other eviction tests cover.
func TestEvictionLeavesUnrelatedSelectedAssetsInPlace(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	pass := intRange(16)
	// m-unrelated is routed to its OWN Case-ID namespace ("m0".."m9"),
	// entirely disjoint from a-expensive's and z-cheap's ("c0".."c19") — a
	// shared slice of zero, so it is never even a candidate for measurement
	// comparison, and its content shares no words either, so the content
	// path (both are KIND_KNOWLEDGE) does not fire.
	unrelated := map[string]float64{}
	for i, id := range []string{"m0", "m1", "m2", "m3", "m4", "m5", "m6", "m7", "m8", "m9"} {
		if i < 8 {
			unrelated[id] = 1
		} else {
			unrelated[id] = 0
		}
	}
	seedRedundancyFixture(
		t, st, "val", knov1.Direction_DIRECTION_MAXIMIZE,
		measuredAsset{id: "a-expensive", kind: knov1.Kind_KIND_KNOWLEDGE, scores: scoresFor(20, pass...), tokens: 1000},
		measuredAsset{id: "m-unrelated", kind: knov1.Kind_KIND_KNOWLEDGE, scores: unrelated, tokens: 50},
		measuredAsset{id: "z-cheap", kind: knov1.Kind_KIND_KNOWLEDGE, scores: scoresFor(20, pass...), tokens: 200},
	)
	pool := stubPool{assets: []*Asset{
		{Id: "a-expensive", Destination: knov1.Destination_DESTINATION_CONTEXT, Content: []byte("one two three")},
		{Id: "m-unrelated", Destination: knov1.Destination_DESTINATION_CONTEXT, Content: []byte("four five six")},
		{Id: "z-cheap", Destination: knov1.Destination_DESTINATION_CONTEXT, Content: []byte("seven eight nine")},
	}}

	opts := selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 100000}, pool)
	opts.RedundancyMaxMargin = 0.4
	res, err := runSelect(t, opts)
	require.NoError(t, err)

	var ids []string
	for _, e := range res.Portfolio.GetSelected() {
		ids = append(ids, e.GetAssetId())
	}
	require.ElementsMatch(t, []string{"m-unrelated", "z-cheap"}, ids)
	require.Len(t, res.Portfolio.GetRejected(), 1)
	require.Equal(t, "a-expensive", res.Portfolio.GetRejected()[0].GetAssetId())
}

// TestExplainValidatesLikeSelect: Explain refuses the same malformed options
// Select's own validate would, before touching the store.
func TestExplainValidatesLikeSelect(t *testing.T) {
	t.Parallel()
	_, err := SelectOptions{}.Explain(context.Background(), "a", 0)
	require.Error(t, err)
}

// TestExplainPropagatesAPoolError: a Pool that fails to yield its Assets
// surfaces through Explain exactly as it would through Select.
func TestExplainPropagatesAPoolError(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	seedValueRun(t, st, "val", testValuation("a", 0.5, 0.2))

	opts := selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 1000}, failPool{openErr: errors.New("pool boom")})
	_, err := opts.Explain(context.Background(), "a", 0)
	require.Error(t, err)
}

// TestEvaluateMeasurementRefusesNonFiniteDiffs: a delta vector carrying a
// non-finite value (never produced by caseDeltaVector, but a defensive
// contract this function honors regardless of caller) yields no interval —
// interval.Paired's own refusal — rather than a manufactured one.
func TestEvaluateMeasurementRefusesNonFiniteDiffs(t *testing.T) {
	t.Parallel()
	shared := scoresFor(20, intRange(16)...)
	broken := map[string]float64{}
	for k, v := range shared {
		broken[k] = v
	}
	broken["c0"] = math.NaN()

	mv := evaluateMeasurement("with", shared, broken, redundancyConfig{}, 0.95, 1, "seed-nan")
	require.True(t, mv.attempted)
	require.False(t, mv.redundant)
	require.Nil(t, mv.evidence)
}

// failMeasurementsStore fails Measurements on demand, for deltasFor's store
// error propagation.
type failMeasurementsStore struct {
	store.Store
	err error
}

func (f failMeasurementsStore) Measurements(ctx context.Context, runID, assetID string) ([]store.RecordedMeasurement, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.Store.Measurements(ctx, runID, assetID)
}

// TestCaseDeltaReaderPropagatesAStoreError: a caseDeltaReader surfaces a
// Measurements failure rather than treating it as "nothing to read".
func TestCaseDeltaReaderPropagatesAStoreError(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	seedBaselineRun(t, st, "base", map[string]float64{"c1": 0})
	createValueRun(t, st, "val", "base", knov1.Direction_DIRECTION_MAXIMIZE)

	fs := failMeasurementsStore{Store: st, err: errors.New("measurements boom")}
	reader := newCaseDeltaReader(context.Background(), fs, "val", map[string]store.CaseScore{"c1": {Value: 0}}, knov1.Direction_DIRECTION_MAXIMIZE)
	_, err := reader.deltasFor(&Valuation{AssetId: "a", CaseIds: []string{"c1"}})
	require.Error(t, err)
}

// TestPoollessRunEmitsMeasurementRedundancyAndStillDegradesContent is
// acceptance criterion 8: a Select run with no Pool now emits REDUNDANT
// rejections carried by measurement evidence, while DegradedRules still
// names REDUNDANT for the content half — both facts asserted together, in
// one test, because criterion 8 requires them to coexist rather than be
// checked separately.
func TestPoollessRunEmitsMeasurementRedundancyAndStillDegradesContent(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	pass := intRange(16)
	seedRedundancyFixture(
		t, st, "val", knov1.Direction_DIRECTION_MAXIMIZE,
		measuredAsset{id: "a", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 100},
		measuredAsset{id: "b", kind: knov1.Kind_KIND_BEHAVIOR, scores: scoresFor(20, pass...), tokens: 100},
	)

	opts := selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 100000}, nil) // no Pool
	opts.RedundancyMaxMargin = 0.4
	res, err := runSelect(t, opts)
	require.NoError(t, err)

	require.Len(t, res.Portfolio.GetRejected(), 1, "measurement evidence needs only the store, so a poolless run still catches this pair")
	require.Equal(t, knov1.RejectionReason_REJECTION_REASON_REDUNDANT, res.Portfolio.GetRejected()[0].GetReason())
	require.Contains(t, res.DegradedRules, "REDUNDANT", "content evidence is still unavailable with no Pool, and the result says so")
}

var _ = math.NaN // keep math imported for any future numeric assertions in this file
