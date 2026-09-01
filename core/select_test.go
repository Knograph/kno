package core

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"math/rand"
	"sync"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// testValuation is a measured Asset fixture: a two-sided t interval over ten
// pairs, one that portfolio.Correct can always rescale.
func testValuation(id string, delta, half float64) *Valuation {
	return &Valuation{
		AssetId:   id,
		DeltaGoal: delta,
		DeltaInterval: &Interval{
			Low:       delta - half,
			High:      delta + half,
			Level:     0.95,
			Method:    "t",
			Sidedness: knov1.Sidedness_SIDEDNESS_TWO_SIDED,
			NPairs:    int32Ptr(10),
		},
		Kind:    knov1.Kind_KIND_KNOWLEDGE,
		NRouted: int32Ptr(20),
		NDev:    int32Ptr(100),
		Cost:    &knov1.CostVector{ContextTokens: 10, AcquisitionUsdMicros: 100},
	}
}

// testControlValuation is a REGRESSION-shaped fixture: the routed delta is
// clearly positive, the control delta is deeply negative, and the harm bound
// is a LOWER interval tight enough that the net interval sits at or below
// zero.
func testControlValuation(id string) *Valuation {
	v := testValuation(id, 0.3, 0.2)
	v.DeltaControl = -1.5
	v.ControlInterval = &Interval{
		Low:       -1.7,
		Level:     0.95,
		Method:    "sign",
		Sidedness: knov1.Sidedness_SIDEDNESS_LOWER,
		NPairs:    int32Ptr(10),
	}
	v.NControl = int32Ptr(10)
	v.FreshControlArm = boolPtr(true)
	v.PairingScheme = knov1.PairingScheme_PAIRING_SCHEME_FRESH_PER_TRIAL
	return v
}

func boolPtr(b bool) *bool { return &b }

// seedValueRun records a completed Value run carrying the given Valuations.
func seedValueRun(t *testing.T, st store.Store, runID string, vals ...*Valuation) {
	t.Helper()
	run := &knov1.Run{
		Id:              runID,
		Stage:           knov1.Stage_STAGE_VALUE,
		Status:          knov1.RunStatus_RUN_STATUS_COMPLETED,
		GoalName:        "test-goal",
		GoalDirection:   knov1.Direction_DIRECTION_MAXIMIZE,
		GoalScoreDomain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY,
		DevCaseCount:    100,
	}
	if err := st.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("seeding run: %v", err)
	}
	for _, v := range vals {
		if err := st.WriteValuation(context.Background(), runID, v); err != nil {
			t.Fatalf("seeding valuation %s: %v", v.GetAssetId(), err)
		}
	}
}

func selectOpts(st store.Store, valueRun string, budget *knov1.Budget, pool Pool) SelectOptions {
	return SelectOptions{
		RunID:      "sel-1",
		ValueRunID: valueRun,
		Store:      st,
		Pool:       pool,
		Budget:     budget,
	}
}

func runSelect(t *testing.T, o SelectOptions) (*SelectResult, error) {
	t.Helper()
	res, err := o.Select(context.Background())
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = o.Store.Close() })
	return res, nil
}

func reasonOf(p *knov1.Portfolio, id string) knov1.RejectionReason {
	for _, r := range p.GetRejected() {
		if r.GetAssetId() == id {
			return r.GetReason()
		}
	}
	return knov1.RejectionReason_REJECTION_REASON_UNSPECIFIED
}

// TestSelectRejectionPrecedence pins the five-rule order: REGRESSION,
// NO_EFFECT, REDUNDANT, COST_DOMINATED, WRONG_MECHANISM — and the two gates
// that shape it (an underpowered harm test never accuses REGRESSION, and a
// record the multiplicity correction cannot rescale is refused rather than
// decided on). The precedence is the product: a team reading the rejection
// log must be able to trust that the strongest reason won.
func TestSelectRejectionPrecedence(t *testing.T) {
	t.Parallel()

	budget := func() *knov1.Budget { return &knov1.Budget{MaxContextTokens: 1000, MaxCostUsdMicros: 10000} }

	t.Run("regression wins over no-effect", func(t *testing.T) {
		t.Parallel()
		// The routed delta is positive AND the net interval is at or below
		// zero: both rules could fire, REGRESSION must.
		v := testControlValuation("a")
		st := openTestStore(t)
		seedValueRun(t, st, "val", v, testValuation("b", 0.0, 0.2))
		res, err := runSelect(t, selectOpts(st, "val", budget(), nil))
		require.NoError(t, err)
		require.Equal(t, knov1.RejectionReason_REJECTION_REASON_REGRESSION, reasonOf(res.Portfolio, "a"))
	})

	t.Run("underpowered control never accuses regression", func(t *testing.T) {
		t.Parallel()
		// Identical numbers, control_underpowered set: the harm signal is
		// noise the gate refuses to dress as the strongest reason in the
		// enum. The Asset survives to the later rules — here it is selected.
		v := testControlValuation("a")
		v.ControlUnderpowered = boolPtr(true)
		st := openTestStore(t)
		seedValueRun(t, st, "val", v, testValuation("b", 0.0, 0.2))
		res, err := runSelect(t, selectOpts(st, "val", budget(), nil))
		require.NoError(t, err)
		require.Len(t, res.Portfolio.GetSelected(), 1)
		require.Equal(t, "a", res.Portfolio.GetSelected()[0].GetAssetId())
		require.Equal(t, knov1.RejectionReason_REJECTION_REASON_UNSPECIFIED, reasonOf(res.Portfolio, "a"))
	})

	t.Run("no-effect wins over redundant", func(t *testing.T) {
		t.Parallel()
		// Zero-effect Asset whose content duplicates a selected one: the
		// measurement answer is the stronger claim.
		a := testValuation("a", 0.5, 0.2)
		b := testValuation("b", 0.0, 0.2)
		st := openTestStore(t)
		seedValueRun(t, st, "val", a, b)
		pool := stubPool{assets: []*Asset{
			{Id: "a", Content: []byte("alpha beta gamma delta epsilon")},
			{Id: "b", Content: []byte("alpha beta gamma delta epsilon")},
		}}
		res, err := runSelect(t, selectOpts(st, "val", budget(), pool))
		require.NoError(t, err)
		require.Equal(t, knov1.RejectionReason_REJECTION_REASON_NO_EFFECT, reasonOf(res.Portfolio, "b"))
	})

	t.Run("redundant wins over cost-dominated", func(t *testing.T) {
		t.Parallel()
		// A duplicate that ALSO would not fit the budget: the duplicate is
		// the more actionable reason — the user cannot fix a budget, they
		// can fix a pool.
		a := testValuation("a", 0.5, 0.2)
		b := testValuation("b", 0.5, 0.2)
		pool := stubPool{assets: []*Asset{
			{Id: "a", Content: []byte("alpha beta gamma delta epsilon")},
			{Id: "b", Content: []byte("alpha beta gamma delta epsilon")},
		}}
		st := openTestStore(t)
		seedValueRun(t, st, "val", a, b)
		bb := &knov1.Budget{MaxContextTokens: 10} // one of them alone already busts it
		res, err := runSelect(t, selectOpts(st, "val", bb, pool))
		require.NoError(t, err)
		require.Equal(t, knov1.RejectionReason_REJECTION_REASON_REDUNDANT, reasonOf(res.Portfolio, "b"))
	})

	t.Run("redundant names what it duplicates", func(t *testing.T) {
		t.Parallel()
		a := testValuation("a", 0.5, 0.2)
		b := testValuation("b", 0.5, 0.2)
		pool := stubPool{assets: []*Asset{
			{Id: "a", Content: []byte("alpha beta gamma delta epsilon")},
			{Id: "b", Content: []byte("alpha beta gamma delta epsilon")},
		}}
		st := openTestStore(t)
		seedValueRun(t, st, "val", a, b)
		res, err := runSelect(t, selectOpts(st, "val", budget(), pool))
		require.NoError(t, err)
		var rej *Rejection
		for _, r := range res.Portfolio.GetRejected() {
			if r.GetAssetId() == "b" {
				rej = r
			}
		}
		require.NotNil(t, rej)
		require.Equal(t, []string{"a"}, rej.GetRedundantWithAssetIds())
	})

	t.Run("redundancy is within knowledge-kind only", func(t *testing.T) {
		t.Parallel()
		// Identical content, different kinds: shingle overlap is meaningless
		// across kinds, so both earn their place.
		a := testValuation("a", 0.5, 0.2)
		b := testValuation("b", 0.5, 0.2)
		b.Kind = knov1.Kind_KIND_BEHAVIOR
		pool := stubPool{assets: []*Asset{
			{Id: "a", Content: []byte("alpha beta gamma delta epsilon")},
			{Id: "b", Content: []byte("alpha beta gamma delta epsilon")},
		}}
		st := openTestStore(t)
		seedValueRun(t, st, "val", a, b)
		res, err := runSelect(t, selectOpts(st, "val", budget(), pool))
		require.NoError(t, err)
		require.Len(t, res.Portfolio.GetSelected(), 2)
	})

	t.Run("cost-dominated wins over wrong-mechanism", func(t *testing.T) {
		t.Parallel()
		// A knowledge Asset destined for the tuning set whose cost already
		// busts the cap: the budget answer precedes the mechanism one.
		a := testValuation("a", 0.5, 0.2)
		a.Cost = &knov1.CostVector{ContextTokens: 5, AcquisitionUsdMicros: 9000}
		pool := stubPool{assets: []*Asset{
			{Id: "a", Content: []byte("alpha beta gamma delta epsilon"), Destination: knov1.Destination_DESTINATION_TUNING_SET},
		}}
		st := openTestStore(t)
		seedValueRun(t, st, "val", a, testValuation("b", 0.0, 0.2))
		bb := &knov1.Budget{MaxCostUsdMicros: 5000}
		res, err := runSelect(t, selectOpts(st, "val", bb, pool))
		require.NoError(t, err)
		require.Equal(t, knov1.RejectionReason_REJECTION_REASON_COST_DOMINATED, reasonOf(res.Portfolio, "a"))
	})

	t.Run("wrong-mechanism rejects knowledge to tuning", func(t *testing.T) {
		t.Parallel()
		a := testValuation("a", 0.5, 0.2)
		pool := stubPool{assets: []*Asset{
			{Id: "a", Content: []byte("alpha beta gamma delta epsilon"), Destination: knov1.Destination_DESTINATION_TUNING_SET},
		}}
		st := openTestStore(t)
		seedValueRun(t, st, "val", a, testValuation("b", 0.0, 0.2))
		res, err := runSelect(t, selectOpts(st, "val", budget(), pool))
		require.NoError(t, err)
		require.Equal(t, knov1.RejectionReason_REJECTION_REASON_WRONG_MECHANISM, reasonOf(res.Portfolio, "a"))
	})

	t.Run("uncorrectable interval is refused, not decided", func(t *testing.T) {
		t.Parallel()
		// A t interval without its pair count cannot be multiplicity-
		// corrected, and the stage says so rather than deciding on it.
		v := testValuation("a", 0.5, 0.2)
		v.DeltaInterval.NPairs = nil
		st := openTestStore(t)
		seedValueRun(t, st, "val", v, testValuation("b", 0.0, 0.2))
		res, err := runSelect(t, selectOpts(st, "val", budget(), nil))
		require.NoError(t, err)
		require.Equal(t, knov1.RejectionReason_REJECTION_REASON_UNDERPOWERED, reasonOf(res.Portfolio, "a"))
	})
}

// TestSelectBudgetAccounting charges each destination its own currency: the
// context cap counts tokens, the tuning cap counts examples, the cost cap
// counts dollars — and a later, cheaper Asset can still fit after a better-
// ranked one took the room.
func TestSelectBudgetAccounting(t *testing.T) {
	t.Parallel()

	t.Run("context cap counts tokens", func(t *testing.T) {
		t.Parallel()
		a := testValuation("a", 0.5, 0.2)
		a.Cost = &knov1.CostVector{ContextTokens: 60, AcquisitionUsdMicros: 100}
		b := testValuation("b", 0.4, 0.2)
		b.Cost = &knov1.CostVector{ContextTokens: 60, AcquisitionUsdMicros: 100}
		st := openTestStore(t)
		seedValueRun(t, st, "val", b, a)
		bb := &knov1.Budget{MaxContextTokens: 100}
		res, err := runSelect(t, selectOpts(st, "val", bb, nil))
		require.NoError(t, err)
		require.Len(t, res.Portfolio.GetSelected(), 1)
		require.Equal(t, knov1.RejectionReason_REJECTION_REASON_COST_DOMINATED, reasonOf(res.Portfolio, "b"))
		require.Equal(t, int64(60), res.Portfolio.GetTotalCost().GetContextTokens())
	})

	t.Run("tuning cap counts examples", func(t *testing.T) {
		t.Parallel()
		a := testValuation("a", 0.5, 0.2)
		a.Kind = knov1.Kind_KIND_BEHAVIOR
		b := testValuation("b", 0.4, 0.2)
		b.Kind = knov1.Kind_KIND_BEHAVIOR
		st := openTestStore(t)
		seedValueRun(t, st, "val", b, a)
		bb := &knov1.Budget{MaxTrainingExamples: 1}
		res, err := runSelect(t, selectOpts(st, "val", bb, nil))
		require.NoError(t, err)
		require.Len(t, res.Portfolio.GetSelected(), 1)
		require.Equal(t, knov1.RejectionReason_REJECTION_REASON_COST_DOMINATED, reasonOf(res.Portfolio, "b"))
		require.Equal(t, knov1.Destination_DESTINATION_TUNING_SET, res.Portfolio.GetSelected()[0].GetDestination())
	})

	t.Run("cost cap counts acquisition dollars", func(t *testing.T) {
		t.Parallel()
		a := testValuation("a", 0.5, 0.2)
		a.Cost = &knov1.CostVector{ContextTokens: 10, AcquisitionUsdMicros: 800}
		b := testValuation("b", 0.4, 0.2)
		b.Cost = &knov1.CostVector{ContextTokens: 10, AcquisitionUsdMicros: 800}
		st := openTestStore(t)
		seedValueRun(t, st, "val", b, a)
		bb := &knov1.Budget{MaxCostUsdMicros: 1000}
		res, err := runSelect(t, selectOpts(st, "val", bb, nil))
		require.NoError(t, err)
		require.Len(t, res.Portfolio.GetSelected(), 1)
		require.Equal(t, knov1.RejectionReason_REJECTION_REASON_COST_DOMINATED, reasonOf(res.Portfolio, "b"))
	})
}

// TestSelectIncludeNothingNew: an all-rejected Portfolio is a completed,
// first-class outcome — the rejection log is the deliverable, not an empty
// run to be ashamed of.
func TestSelectIncludeNothingNew(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	seedValueRun(
		t, st, "val",
		testValuation("a", 0.0, 0.2),
		testValuation("b", 0.0, 0.2),
	)
	res, err := runSelect(t, selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 1000}, nil))
	require.NoError(t, err)
	require.Len(t, res.Portfolio.GetSelected(), 0)
	require.Len(t, res.Portfolio.GetRejected(), 2)
	require.Equal(t, knov1.RunStatus_RUN_STATUS_COMPLETED, res.Status)
	require.Equal(t, knov1.RejectionReason_REJECTION_REASON_NO_EFFECT, reasonOf(res.Portfolio, "a"))
}

// TestSelectIsDeterministic: two runs over identically seeded stores produce
// byte-identical Portfolios — every ordering ends in the Asset ID, so the
// Portfolio is a function of the store, not of map iteration or insertion
// order.
func TestSelectIsDeterministic(t *testing.T) {
	t.Parallel()

	vals := []*Valuation{
		testValuation("a", 0.5, 0.2),
		testValuation("b", 0.5, 0.2),
		testValuation("c", 0.3, 0.2),
		testValuation("d", 0.0, 0.2),
	}
	pool := stubPool{assets: []*Asset{
		{Id: "a", Content: []byte("alpha beta gamma delta epsilon")},
		{Id: "b", Content: []byte("zeta eta theta iota kappa")},
		{Id: "c", Content: []byte("lambda mu nu xi omicron")},
		{Id: "d", Content: []byte("pi rho sigma tau upsilon")},
	}}
	bb := &knov1.Budget{MaxContextTokens: 1000}

	var first []byte
	for i := 0; i < 2; i++ {
		st := openTestStore(t)
		seedValueRun(t, st, "val", vals...)
		res, err := runSelect(t, selectOpts(st, "val", bb, pool))
		require.NoError(t, err)
		blob, err := proto.Marshal(res.Portfolio)
		require.NoError(t, err)
		if i == 0 {
			first = blob
		} else {
			require.Equal(t, first, blob, "portfolio drifted between identical runs")
		}
	}
}

// TestSelectTieBreakIsAssetID: two Assets with identical rank keys order by
// Asset ID, so a portfolio cannot flip when the pool loads in a different
// order.
func TestSelectTieBreakIsAssetID(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	seedValueRun(
		t, st, "val",
		testValuation("zz", 0.5, 0.2),
		testValuation("aa", 0.5, 0.2),
	)
	res, err := runSelect(t, selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 1000}, nil))
	require.NoError(t, err)
	require.Equal(t, []string{"aa", "zz"}, []string{
		res.Portfolio.GetSelected()[0].GetAssetId(),
		res.Portfolio.GetSelected()[1].GetAssetId(),
	})
}

// TestSelectRoutedScaleFlag: a tagged delta (n_routed present) carries its
// n_routed / n_dev scale on the entry, and the portfolio gain is the scaled
// sum — an untagged delta carries no scale field at all.
func TestSelectRoutedScaleFlag(t *testing.T) {
	t.Parallel()

	tagged := testValuation("a", 0.5, 0.2)
	tagged.NRouted = int32Ptr(20)
	tagged.NDev = int32Ptr(100)
	st := openTestStore(t)
	// "b" is screened (so the correction has a denominator) but carries no
	// effect — it is rejected NO_EFFECT and contributes nothing to the gain.
	seedValueRun(t, st, "val", tagged, testValuation("b", 0.0, 0.2))
	res, err := runSelect(t, selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 1000}, nil))
	require.NoError(t, err)
	e := res.Portfolio.GetSelected()[0]
	require.Equal(t, "a", e.GetAssetId())
	require.NotNil(t, e.GetNRoutedScale())
	require.InDelta(t, 0.2, e.GetNRoutedScale(), 1e-9)
	// Gain = delta * scale (only selected asset): 0.5 * 0.2 = 0.1.
	require.InDelta(t, 0.1, res.Portfolio.GetDevEstimatedGain(), 1e-9)
	// The interval's level is the corrected one, not the raw 0.95.
	require.InDelta(t, 0.975, res.Portfolio.GetDevEstimatedInterval().GetLevel(), 1e-9)
}

// TestSelectPortfolioIntervalIsCorrected: with several Assets screened, the
// portfolio claim carries the Bonferroni level and the shared-draw method —
// the two markers a reader needs to know the number is already multiplicity-
// corrected.
func TestSelectPortfolioIntervalIsCorrected(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	seedValueRun(
		t, st, "val",
		testValuation("a", 0.5, 0.2),
		testValuation("b", 0.5, 0.2),
		testValuation("c", 0.5, 0.2),
	)
	res, err := runSelect(t, selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 1000}, nil))
	require.NoError(t, err)
	iv := res.Portfolio.GetDevEstimatedInterval()
	require.NotNil(t, iv)
	// 1 - (1-0.95)/3 = 0.98333...
	require.InDelta(t, 0.9833333, iv.GetLevel(), 1e-6)
	require.Equal(t, "portfolio-greedy-shared", iv.GetMethod())
}

// TestSelectRefusesPartialSource: a Budget-stopped source run cannot be
// ranked as if it were the whole answer — unless the user says so, and then
// the incompleteness travels with the Portfolio.
func TestSelectRefusesPartialSource(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	run := &knov1.Run{
		Id:               "val",
		Stage:            knov1.Stage_STAGE_VALUE,
		Status:           knov1.RunStatus_RUN_STATUS_BUDGET_STOPPED,
		IncompleteReason: "budget exhausted",
		GoalName:         "g",
	}
	require.NoError(t, st.CreateRun(context.Background(), run))
	require.NoError(t, st.WriteValuation(context.Background(), "val", testValuation("a", 0.5, 0.2)))

	_, err := runSelect(t, selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 1000}, nil))
	require.Error(t, err)
	require.Contains(t, err.Error(), "BUDGET_STOPPED")

	o := selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 1000}, nil)
	o.AllowPartial = true
	res, err := runSelect(t, o)
	require.NoError(t, err)
	require.Equal(t, knov1.RunStatus_RUN_STATUS_BUDGET_STOPPED, res.Portfolio.GetSourceStatus())
	require.Equal(t, "budget exhausted", res.Portfolio.GetSourceIncompleteReason())
}

// TestSelectRefusesNonValueSource: Select builds on Value runs, and a wrong
// run ID gets the fix spelled out.
func TestSelectRefusesNonValueSource(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	require.NoError(t, st.CreateRun(context.Background(), &knov1.Run{
		Id: "base", Stage: knov1.Stage_STAGE_BASELINE,
		Status: knov1.RunStatus_RUN_STATUS_COMPLETED, GoalName: "g",
	}))
	_, err := runSelect(t, selectOpts(st, "base", &knov1.Budget{MaxContextTokens: 1000}, nil))
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a Value run")
}

// TestSelectValidatesEverythingRefusable: each refusal is free and readable.
func TestSelectValidatesEverythingRefusable(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	full := selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 1000}, nil)
	cases := []struct {
		name string
		opts SelectOptions
		want string
	}{
		{"no run ID", func() SelectOptions { o := full; o.RunID = ""; return o }(), "run ID"},
		{"no value run", func() SelectOptions { o := full; o.ValueRunID = ""; return o }(), "Value run"},
		{"no store", func() SelectOptions { o := full; o.Store = nil; return o }(), "store"},
		{"no budget", func() SelectOptions { o := full; o.Budget = nil; return o }(), "budget"},
		{"level at 0.5", func() SelectOptions { o := full; o.Level = 0.5; return o }(), "level"},
		{"level at 1.0", func() SelectOptions { o := full; o.Level = 1.0; return o }(), "level"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := tc.opts.Select(context.Background())
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestSelectDegradedRulesReported: without a Pool, the two rules that need
// content and destinations are named as degraded in the result — the report
// can say what was not decided, and nothing is silently skipped.
func TestSelectDegradedRulesReported(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	seedValueRun(
		t, st, "val",
		testValuation("a", 0.5, 0.2),
		testValuation("b", 0.0, 0.2),
	)
	res, err := runSelect(t, selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 1000}, nil))
	require.NoError(t, err)
	require.Equal(t, []string{"REDUNDANT", "WRONG_MECHANISM"}, res.DegradedRules)

	st2 := openTestStore(t)
	seedValueRun(
		t, st2, "val",
		testValuation("a", 0.5, 0.2),
		testValuation("b", 0.0, 0.2),
	)
	res2, err := runSelect(t, selectOpts(st2, "val", &knov1.Budget{MaxContextTokens: 1000}, stubPool{assets: []*Asset{{Id: "a"}}}))
	require.NoError(t, err)
	require.Nil(t, res2.DegradedRules)
}

// TestSelectEventSequence: RunStarted (sequence 1) → PortfolioSelected →
// RunFinished, and the counts on PortfolioSelected match the Portfolio.
func TestSelectEventSequence(t *testing.T) {
	t.Parallel()

	rec := &recordingStore{Store: openTestStore(t)}
	seedValueRun(
		t, rec, "val",
		testValuation("a", 0.5, 0.2),
		testValuation("b", 0.0, 0.2),
	)
	res, err := runSelect(t, selectOpts(rec, "val", &knov1.Budget{MaxContextTokens: 1000}, nil))
	require.NoError(t, err)
	require.Len(t, rec.events, 3)

	started := rec.events[0].GetRunStarted()
	require.NotNil(t, started)
	require.Equal(t, int64(1), rec.events[0].GetSequence())
	sel := rec.events[1].GetPortfolioSelected()
	require.NotNil(t, sel)
	require.Equal(t, int32(1), sel.GetSelected())
	require.Equal(t, int32(1), sel.GetRejected())
	finished := rec.events[2].GetRunFinished()
	require.NotNil(t, finished)
	require.Equal(t, knov1.RunStatus_RUN_STATUS_COMPLETED, finished.GetStatus())
	require.Equal(t, knov1.RunStatus_RUN_STATUS_COMPLETED, res.Status)
}

// recordingStore wraps a Store and captures every event appended, so the
// event grammar can be pinned without a reader on the interface.
type recordingStore struct {
	store.Store
	mu     sync.Mutex
	events []*knov1.Event
}

func (r *recordingStore) AppendEvent(ctx context.Context, ev *knov1.Event) error {
	r.mu.Lock()
	r.events = append(r.events, proto.Clone(ev).(*knov1.Event))
	r.mu.Unlock()
	return r.Store.AppendEvent(ctx, ev)
}

// holdoutCanaryStore passes every read/write through to a real SQLite store
// except the readers that would let Select touch the holdout.
//
// Forbidding by METHOD NAME was sound only while no holdout row existed
// anywhere Select could read. `kno validate` ended that: it writes holdout
// measurements through RecordMeasurement (core/validate_loop.go) and reads
// them back through Measurements (core/validate_measure.go), and Measurements
// is not on the forbid list — so the day Select acquires a Measurements
// reader, this canary goes green while the guarantee it names is gone.
//
// The guard is therefore RUN-SCOPED, not name-scoped. Measurements is allowed
// for the gated Value run and CaseScores for that run's recorded baseline;
// any other run ID is a failure, including a Validate run ID. That is
// strictly stronger than forbidding names: it survives a new reader being
// added, which is exactly how the name-scoped version decayed.
type holdoutCanaryStore struct {
	store.Store
	// canaryT rather than *testing.T so the guard itself can be tested. A
	// guard that has never been watched to fail is a guard nobody knows
	// works (docs/debt.md#16), and this one now guards a reader Select does
	// not yet call — so "it passes" proves nothing on its own.
	t canaryT

	// valueRunID is the Value run Select was pointed at, and baselineRunID is
	// the baseline that run recorded. Empty means "no read of this kind is
	// legitimate", which is the safe default for a canary.
	valueRunID    string
	baselineRunID string
}

// canaryT is the slice of *testing.T the canary uses.
type canaryT interface {
	Helper()
	Fatalf(format string, args ...any)
}

func (h *holdoutCanaryStore) forbid(name string) {
	h.t.Helper()
	h.t.Fatalf("select reached the holdout through %s", name)
}

// allow fails unless runID is the one run this reader is legitimately scoped
// to. Named for what it permits rather than what it blocks, because the
// permitted set is the shorter and more reviewable half.
func (h *holdoutCanaryStore) allow(name, runID, want string) {
	h.t.Helper()
	if want == "" || runID != want {
		h.t.Fatalf("select read %s for run %q; the only run it may read this "+
			"way is %q. A Validate run ID here would be a holdout read: "+
			"validate writes holdout measurements through this table",
			name, runID, want)
	}
}

func (h *holdoutCanaryStore) RecordOutcome(_ context.Context, _ string, _ *store.Outcome) error {
	h.forbid("RecordOutcome")
	return nil
}

func (h *holdoutCanaryStore) CaseScores(ctx context.Context, runID string) (map[string]store.CaseScore, error) {
	// Permitted only for the baseline the gated Value run recorded. Any other
	// run — a Validate run above all — is the leak this canary exists for.
	h.allow("CaseScores", runID, h.baselineRunID)
	return h.Store.CaseScores(ctx, runID)
}

// Measurements is guarded even though Select does not call it today. The
// canary's job is to fail when a future change reaches the holdout, and a
// reader that is unguarded until someone uses it is a guard that arrives
// after the fact.
func (h *holdoutCanaryStore) Measurements(ctx context.Context, runID, assetID string) ([]store.RecordedMeasurement, error) {
	// Run-scoped, not asset-scoped, and that is the load-bearing detail:
	// validate records its holdout rows under its OWN run ID, with the Select
	// run in the asset_id column (core/validate_loop.go RecordMeasurement,
	// read back by core/validate_measure.go). So a holdout read is exactly a
	// read at a run ID that is not the gated Value run, whatever asset it names.
	h.allow("Measurements", runID, h.valueRunID)
	return h.Store.Measurements(ctx, runID, assetID)
}

func (h *holdoutCanaryStore) CompletedCases(_ context.Context, _ string) (map[string]struct{}, error) {
	h.forbid("CompletedCases")
	return nil, nil
}

func (h *holdoutCanaryStore) OutcomeCounts(_ context.Context, _ string) (int, int, error) {
	h.forbid("OutcomeCounts")
	return 0, 0, nil
}

func (h *holdoutCanaryStore) ScoreSum(_ context.Context, _ string) (store.ScoreSummary, error) {
	h.forbid("ScoreSum")
	return store.ScoreSummary{}, nil
}

func (h *holdoutCanaryStore) CaseObservations(_ context.Context, _ string) (store.Observations, error) {
	h.forbid("CaseObservations")
	return store.Observations{}, nil
}

func (h *holdoutCanaryStore) SettledSpend(_ context.Context, _ string) (budget.Spend, error) {
	h.forbid("SettledSpend")
	return budget.Spend{}, nil
}

func (h *holdoutCanaryStore) RecordOrphanSpend(_ context.Context, _ string, _ budget.Spend) error {
	h.forbid("RecordOrphanSpend")
	return nil
}

func (h *holdoutCanaryStore) Purge(_ context.Context, _ string) (int64, error) {
	h.forbid("Purge")
	return 0, nil
}

// TestSelectHoldoutCanary: the holdout-isolation invariant as a test. Select
// takes no Evals, so it cannot measure the holdout itself; what remains is
// whether it can READ a holdout result someone else recorded, and that is
// what the canary store answers.
//
// The scope is a run ID, not a method name. Select may read Measurements only
// for the Value run it was pointed at and CaseScores only for that run's
// recorded baseline; every other run ID fails, including a Validate run's.
// Naming methods instead was sound only until something wrote a holdout row
// where Select could reach it, and `kno validate` did exactly that.
func TestSelectHoldoutCanary(t *testing.T) {
	t.Parallel()

	// baselineRunID is left empty deliberately: seedValueRun records no
	// baseline, so no CaseScores read is legitimate for this run and every one
	// fails — the same strictness the method-name forbid had. A Select that
	// legitimately needs per-Case deltas must seed a baseline here first, which
	// makes the widening visible in the diff rather than silent.
	st := &holdoutCanaryStore{Store: openTestStore(t), t: t, valueRunID: "val"}
	seedValueRun(
		t, st, "val",
		testValuation("a", 0.5, 0.2),
		testValuation("b", 0.0, 0.2),
	)
	res, err := runSelect(t, selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 1000}, nil))
	require.NoError(t, err)
	require.Len(t, res.Portfolio.GetSelected(), 1)
}

// TestSelectFDRThroughSelect: 100 null Assets screened together select
// nothing — the Bonferroni correction the stage applies holds the family
// error rate down through the real pipeline, not just in stats/.
func TestSelectFDRThroughSelect(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewSource(7))
	st := openTestStore(t)
	vals := make([]*Valuation, 0, 100)
	for i := 0; i < 100; i++ {
		v := testValuation(string(rune('a'+i%26))+string(rune('0'+i/26)), 0, 0)
		se := 1.0 / 10
		d := rng.NormFloat64() * se
		v.DeltaGoal = d
		iv := v.DeltaInterval
		iv.Low = d - ivHalfWidth
		iv.High = d + ivHalfWidth
		vals = append(vals, v)
	}
	seedValueRun(t, st, "val", vals...)
	res, err := runSelect(t, selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 1000000}, nil))
	require.NoError(t, err)
	require.Empty(t, res.Portfolio.GetSelected())
	require.Len(t, res.Portfolio.GetRejected(), 100)
}

// ivHalfWidth is the interval half-width the FDR smoke uses: the 0.975
// quantile of t with 9 df times one standard error, the correction's
// headline — a naive 0.95 interval would fire ~5% of the time, the corrected
// one essentially never on the null.
const ivHalfWidth = 2.262157162740992

// failStore wraps a real SQLite store and fails one method on demand, so the
// run-shape error paths — every "the run failed" return — are exercised once.
type failStore struct {
	store.Store
	fail func(method string) error
}

func (f *failStore) GetRun(ctx context.Context, id string) (*knov1.Run, error) {
	if f.fail != nil {
		if err := f.fail("GetRun"); err != nil {
			return nil, err
		}
	}
	return f.Store.GetRun(ctx, id)
}

func (f *failStore) Valuations(ctx context.Context, id string) ([]*Valuation, error) {
	if f.fail != nil {
		if err := f.fail("Valuations"); err != nil {
			return nil, err
		}
	}
	return f.Store.Valuations(ctx, id)
}

func (f *failStore) CreateRun(ctx context.Context, r *knov1.Run) error {
	if f.fail != nil {
		if err := f.fail("CreateRun"); err != nil {
			return err
		}
	}
	return f.Store.CreateRun(ctx, r)
}

func (f *failStore) WritePortfolio(ctx context.Context, id string, p *knov1.Portfolio) error {
	if f.fail != nil {
		if err := f.fail("WritePortfolio"); err != nil {
			return err
		}
	}
	return f.Store.WritePortfolio(ctx, id, p)
}

func (f *failStore) AppendEvent(ctx context.Context, e *knov1.Event) error {
	if f.fail != nil {
		if err := f.fail("AppendEvent"); err != nil {
			return err
		}
	}
	return f.Store.AppendEvent(ctx, e)
}

func (f *failStore) FinishRun(ctx context.Context, r *knov1.Run) error {
	if f.fail != nil {
		if err := f.fail("FinishRun"); err != nil {
			return err
		}
	}
	return f.Store.FinishRun(ctx, r)
}

func (f *failStore) Portfolio(ctx context.Context, id string) (*knov1.Portfolio, error) {
	if f.fail != nil {
		if err := f.fail("Portfolio"); err != nil {
			return nil, err
		}
	}
	return f.Store.Portfolio(ctx, id)
}

// TestSelectErrorPaths: every store failure surfaces as an error naming the
// operation — the run shape never swallows a failed read or write.
func TestSelectErrorPaths(t *testing.T) {
	t.Parallel()

	for _, method := range []string{"GetRun", "Valuations", "CreateRun", "AppendEvent", "WritePortfolio", "FinishRun"} {
		method := method
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			st := &failStore{Store: openTestStore(t)}
			seedValueRun(
				t, st, "val",
				testValuation("a", 0.5, 0.2),
				testValuation("b", 0.0, 0.2),
			)
			st.fail = func(m string) error {
				if m == method {
					return errors.New("boom")
				}
				return nil
			}
			_, err := runSelect(t, selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 1000}, nil))
			require.Error(t, err)
			require.Contains(t, err.Error(), "boom")
		})
	}
}

// failPool yields an error instead of Assets, or yields one error mid-iteration.
type failPool struct {
	openErr  error
	yieldErr error
}

func (p failPool) Assets(_ context.Context) (iter.Seq2[*Asset, error], error) {
	if p.openErr != nil {
		return nil, p.openErr
	}
	return func(yield func(*Asset, error) bool) {
		if p.yieldErr != nil && !yield(nil, p.yieldErr) {
			return
		}
	}, nil
}

// TestSelectPoolErrorPaths: a pool that fails to open or fails mid-iteration
// aborts the run with the operation named.
func TestSelectPoolErrorPaths(t *testing.T) {
	t.Parallel()

	for name, pool := range map[string]Pool{
		"open":  failPool{openErr: errors.New("boom")},
		"yield": failPool{yieldErr: errors.New("boom")},
	} {
		name, pool := name, pool
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st := openTestStore(t)
			seedValueRun(
				t, st, "val",
				testValuation("a", 0.5, 0.2),
				testValuation("b", 0.0, 0.2),
			)
			_, err := runSelect(t, selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 1000}, pool))
			require.Error(t, err)
			require.Contains(t, err.Error(), "boom")
		})
	}
}

// TestSelectSkipsPoolNulls: a pool yielding nil Assets is tolerated — the
// measurement is what the Valuation carries, not the pool row.
func TestSelectSkipsPoolNulls(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	seedValueRun(
		t, st, "val",
		testValuation("a", 0.5, 0.2),
		testValuation("b", 0.0, 0.2),
	)
	pool := stubPool{assets: []*Asset{nil, nil}}
	res, err := runSelect(t, selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 1000}, pool))
	require.NoError(t, err)
	require.Len(t, res.Portfolio.GetSelected(), 1)
}

// TestSelectUnmeasuredValuations: Valuations recorded as unmeasured are
// rejected with the reason Value recorded, ordered by Asset ID — never
// ranked, never screened, never corrected.
func TestSelectUnmeasuredValuations(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	seedValueRun(
		t, st, "val",
		testValuation("a", 0.5, 0.2),
		testValuation("c", 0.3, 0.2),
		testValuation("m1", 0.0, 0.2),
		testValuation("m2", 0.0, 0.2),
	)
	vals, err := st.Valuations(context.Background(), "val")
	require.NoError(t, err)
	for _, v := range vals {
		if v.GetAssetId() == "m1" || v.GetAssetId() == "m2" {
			v.NotMeasured = knov1.RejectionReason_REJECTION_REASON_COST_DOMINATED
			require.NoError(t, st.WriteValuation(context.Background(), "val", v))
		}
	}
	res, err := runSelect(t, selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 1000}, nil))
	require.NoError(t, err)
	require.Len(t, res.Portfolio.GetSelected(), 2)
	require.Len(t, res.Portfolio.GetRejected(), 2)
	require.Equal(t, knov1.RejectionReason_REJECTION_REASON_COST_DOMINATED, reasonOf(res.Portfolio, "m1"))
	require.Equal(t, knov1.RejectionReason_REJECTION_REASON_COST_DOMINATED, reasonOf(res.Portfolio, "m2"))
	rejected := res.Portfolio.GetRejected()
	require.Equal(t, "m1", rejected[0].GetAssetId())
	require.Equal(t, "m2", rejected[1].GetAssetId())
}

// TestSelectEmitterRefusesAfterClose: appending to an emitter that already
// emitted RunFinished is refused — the sequence is one run, closed once.
func TestSelectEmitterRefusesAfterClose(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	em := &selectEmitter{closed: true}
	err := SelectOptions{RunID: "x", Store: st}.
		append(context.Background(), em, func() *knov1.Event {
			return &knov1.Event{Payload: &knov1.Event_RunStarted{RunStarted: &knov1.RunStarted{Stage: knov1.Stage_STAGE_SELECT}}}
		}, "run-started")
	require.Error(t, err)
	require.Contains(t, err.Error(), "RunFinished")
}

// TestSelectAppendFails: an AppendEvent failure surfaces from the append,
// named by the event kind.
func TestSelectAppendFails(t *testing.T) {
	t.Parallel()

	st := &failStore{Store: openTestStore(t), fail: func(string) error { return errors.New("boom") }}
	em := &selectEmitter{}
	err := SelectOptions{RunID: "x", Store: st}.
		append(context.Background(), em, func() *knov1.Event {
			return &knov1.Event{Payload: &knov1.Event_RunStarted{RunStarted: &knov1.RunStarted{Stage: knov1.Stage_STAGE_SELECT}}}
		}, "run-started")
	require.Error(t, err)
	require.Contains(t, err.Error(), "boom")
}

// TestSelectDecisionUnits pins the small decision helpers directly — the
// branches a full run cannot cheaply reach, each one a refusal or a default.
func TestSelectDecisionUnits(t *testing.T) {
	t.Parallel()

	// rankLess: delta_per_cost desc, then delta desc, then Asset ID asc.
	a := testValuation("a", 0.5, 0.2)
	a.DeltaPerCost = 9
	b := testValuation("b", 0.4, 0.2)
	b.DeltaPerCost = 5
	require.True(t, rankLess(a, b)) // ratio desc
	b.DeltaPerCost = 9              // same ratio, smaller delta
	require.True(t, rankLess(a, b)) // delta desc
	require.False(t, rankLess(b, a))
	a2 := testValuation("aa", 0.5, 0.2)
	a2.DeltaPerCost = 9
	require.True(t, rankLess(a, a2)) // same ratio, same delta: Asset ID asc

	// redundantWith: a selected entry with no content is skipped, never a
	// duplicate of nothing.
	require.Empty(t, redundantWith([]byte("x"), []decidedKnowledge{{assetID: "a"}}))

	// overBudget: a budget with no cap refuses nothing.
	noCaps := &knov1.Budget{}
	require.Empty(t, overBudget(testValuation("a", 0.5, 0.2), nil, knov1.Destination_DESTINATION_CONTEXT, noCaps, spend{}))
	require.Empty(t, overBudget(testValuation("a", 0.5, 0.2), nil, knov1.Destination_DESTINATION_TUNING_SET, noCaps, spend{}))

	// correctedLevel: below two screened, the level is used uncorrected.
	require.Equal(t, 0.95, correctedLevel(0.95, 1))

	// routedScale: absent n_routed means no scale, flagged.
	v := testValuation("a", 0.5, 0.2)
	v.NRouted = nil
	_, ok := routedScale(v)
	require.False(t, ok)

	// kindOf: the Valuation's kind wins; absent falls back to knowledge.
	behavioral := testValuation("a", 0.5, 0.2)
	behavioral.Kind = knov1.Kind_KIND_BEHAVIOR
	require.Equal(t, knov1.Kind_KIND_BEHAVIOR, kindOf(behavioral))
	require.Equal(t, knov1.Kind_KIND_KNOWLEDGE, kindOf(&Valuation{}))

	// shingleOverlap: either empty set overlaps nothing.
	require.Zero(t, shingleOverlap(map[string]struct{}{}, map[string]struct{}{"x y z": {}}))
	require.Zero(t, shingleOverlap(map[string]struct{}{"x y z": {}}, map[string]struct{}{}))

	// redundantWith: no content duplicates nothing.
	require.Nil(t, redundantWith(nil, []decidedKnowledge{{assetID: "a", content: []byte("x")}}))

	// combineCost: a nil cost vector adds nothing.
	total := &knov1.CostVector{ContextTokens: 5}
	combineCost(total, nil)
	require.Equal(t, int64(5), total.GetContextTokens())
}

// TestNetIntervalRefusals: the net-loss judgement refuses records that cannot
// support it — no control arm, no control draw size, or a harm bound with no
// width — each returning nil, never a fabricated interval.
func TestNetIntervalRefusals(t *testing.T) {
	t.Parallel()

	good := testValuation("a", 0.5, 0.2)
	corrected := &Interval{Low: -0.4, High: 0.8, Level: 0.975, Method: "t", Sidedness: knov1.Sidedness_SIDEDNESS_TWO_SIDED, NPairs: int32Ptr(10)}

	noControl := proto.Clone(good).(*Valuation)
	require.Nil(t, netInterval(noControl, corrected, 0.95))

	noControlDraw := proto.Clone(good).(*Valuation)
	noControlDraw.DeltaControl = -1.5
	noControlDraw.ControlInterval = &Interval{Low: -1.7, Level: 0.95, Sidedness: knov1.Sidedness_SIDEDNESS_LOWER, NPairs: int32Ptr(10)}
	noControlDraw.NControl = nil
	require.Nil(t, netInterval(noControlDraw, corrected, 0.95))

	zeroWidth := proto.Clone(noControlDraw).(*Valuation)
	zeroWidth.NControl = int32Ptr(10)
	zeroWidth.DeltaControl = -1.7 // equals the Low: the harm bound has no width
	require.Nil(t, netInterval(zeroWidth, corrected, 0.95))
}

// TestSelectBudgetKnowledgeBaseCap: the knowledge-base cap counts content
// bytes, and the destination comes from the pool Asset, not the kind.
func TestSelectBudgetKnowledgeBaseCap(t *testing.T) {
	t.Parallel()

	a := testValuation("a", 0.5, 0.2)
	b := testValuation("b", 0.4, 0.2)
	pool := stubPool{assets: []*Asset{
		{Id: "a", Content: []byte("0123456789"), Destination: knov1.Destination_DESTINATION_KNOWLEDGE_BASE},
		{Id: "b", Content: []byte("0123456789"), Destination: knov1.Destination_DESTINATION_KNOWLEDGE_BASE},
	}}
	st := openTestStore(t)
	seedValueRun(t, st, "val", b, a)
	bb := &knov1.Budget{MaxKnowledgeBaseBytes: 10}
	res, err := runSelect(t, selectOpts(st, "val", bb, pool))
	require.NoError(t, err)
	require.Len(t, res.Portfolio.GetSelected(), 1)
	require.Equal(t, knov1.Destination_DESTINATION_KNOWLEDGE_BASE, res.Portfolio.GetSelected()[0].GetDestination())
	require.Equal(t, knov1.RejectionReason_REJECTION_REASON_COST_DOMINATED, reasonOf(res.Portfolio, "b"))
	require.Contains(t, rejectionDetailOf(res.Portfolio, "b"), "allowed bytes")
}

// TestSelectAppendFailsMidRun: an AppendEvent failure after the run started
// surfaces with the event named — the run cannot finish with a missing
// portfolio-selected or run-finished record.
func TestSelectAppendFailsMidRun(t *testing.T) {
	t.Parallel()

	for _, failOn := range []int{2, 3} {
		failOn := failOn
		t.Run(fmt.Sprintf("event-%d", failOn), func(t *testing.T) {
			t.Parallel()
			n := 0
			st := &failStore{Store: openTestStore(t), fail: func(m string) error {
				if m == "AppendEvent" {
					n++
					if n == failOn {
						return errors.New("boom")
					}
				}
				return nil
			}}
			seedValueRun(
				t, st, "val",
				testValuation("a", 0.5, 0.2),
				testValuation("b", 0.0, 0.2),
			)
			_, err := runSelect(t, selectOpts(st, "val", &knov1.Budget{MaxContextTokens: 1000}, nil))
			require.Error(t, err)
			require.Contains(t, err.Error(), "boom")
		})
	}
}

// rejectionDetailOf returns a Rejection's detail.
func rejectionDetailOf(p *knov1.Portfolio, id string) string {
	for _, r := range p.GetRejected() {
		if r.GetAssetId() == id {
			return r.GetDetail()
		}
	}
	return ""
}

// recordingT captures what a canary reported instead of failing the test, so
// the canary's own failure path can be asserted on.
type recordingT struct {
	fired bool
	msg   string
}

func (r *recordingT) Helper() {}

func (r *recordingT) Fatalf(format string, args ...any) {
	r.fired = true
	r.msg = fmt.Sprintf(format, args...)
}

// TestSelectHoldoutCanaryCatchesAForeignRun: the canary fails when a read
// names a run that is not the gated Value run.
//
// This is the case the method-name version could not express. `kno validate`
// records holdout measurements under its own run ID, so a Select that grew a
// Measurements reader would have read the holdout while a canary that only
// listed forbidden method names stayed green. The guarantee has to be watched
// failing to be worth anything.
func TestSelectHoldoutCanaryCatchesAForeignRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	for _, tc := range []struct {
		name string
		read func(*holdoutCanaryStore) error
		want string
	}{
		{
			name: "a validate run's measurements are the holdout",
			read: func(st *holdoutCanaryStore) error {
				_, err := st.Measurements(ctx, "validate-run", "sel-1")
				return err
			},
			want: "Measurements",
		},
		{
			name: "case scores for an unrecorded baseline",
			read: func(st *holdoutCanaryStore) error {
				_, err := st.CaseScores(ctx, "some-other-run")
				return err
			},
			want: "CaseScores",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := &recordingT{}
			st := &holdoutCanaryStore{Store: openTestStore(t), t: rec, valueRunID: "val"}
			t.Cleanup(func() { _ = st.Store.Close() })

			_ = tc.read(st)

			require.True(t, rec.fired, "the canary permitted a read it exists to stop")
			require.Contains(t, rec.msg, tc.want)
		})
	}

	// And the permitted read does NOT fire, because a canary that is always
	// red is a canary people learn to ignore (docs/debt.md#70).
	t.Run("the gated value run is permitted", func(t *testing.T) {
		t.Parallel()

		rec := &recordingT{}
		st := &holdoutCanaryStore{Store: openTestStore(t), t: rec, valueRunID: "val"}
		t.Cleanup(func() { _ = st.Store.Close() })

		_, _ = st.Measurements(ctx, "val", "asset-a")
		require.False(t, rec.fired, "the canary blocked the one read Select may make: %s", rec.msg)
	})
}
