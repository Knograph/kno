package interval_test

import (
	"math"
	"math/rand"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/interval"
	"github.com/stretchr/testify/require"
)

// netIntervalPreExtraction is a FROZEN copy of core/select.go's netInterval
// as it existed before the bridge eval-seam plan
// (docs/plans/2026-09-01-bridge-eval-seam.md §4/§9) extracted its
// combination into interval.NetEffect. It must never be "improved" — its
// only job is to be the pre-extraction behavior this test pins NetEffect
// against, so a change here would defeat the point of a characterization
// test.
//
// goalMean/goalHalf and control/controlMean/nT/nC/shared/level are exactly
// the arguments core/select.go's netInterval passed to
// stats/portfolio.NetLoss before the extraction (see that function's git
// history): goalMean was v.GetDeltaGoal(), goalHalf was
// halfWidth(corrected), and the control widening used ctrl.GetLow(),
// ctrl.GetLevel() and ctrl.GetNPairs().
func netIntervalPreExtraction(
	goalMean, goalHalf float64,
	control *knov1.Interval,
	nT, nC int,
	controlMean float64,
	shared bool,
	level float64,
) *knov1.Interval {
	if control == nil {
		return nil
	}
	if nT <= 0 || nC <= 0 || control.GetNPairs() < 2 {
		return nil
	}
	df := int(control.GetNPairs()) - 1
	halfC := (controlMean - control.GetLow()) *
		interval.Quantile(level, knov1.Sidedness_SIDEDNESS_TWO_SIDED, df) /
		interval.Quantile(control.GetLevel(), knov1.Sidedness_SIDEDNESS_LOWER, df)
	if math.IsNaN(halfC) || math.IsInf(halfC, 0) || halfC <= 0 {
		return nil
	}
	return interval.NetLoss(
		interval.NetDelta{Mean: goalMean, Half: goalHalf, N: nT},
		interval.NetDelta{Mean: controlMean, Half: halfC, N: nC},
		shared, level,
	)
}

// requireIntervalsEqual compares every field a caller can read off an
// Interval, with a float tolerance rather than exact equality — the two
// formulas being compared do the same arithmetic in the same order, so exact
// equality would normally hold, but this test cares whether the two
// SHAPES agree, not whether floating-point rounding is bit-identical (see
// CLAUDE.md's FMA note: fused multiply-add differs by architecture, and this
// package's own comments elsewhere already flag that arm64/amd64 can land a
// bisection one ULP apart).
func requireIntervalsEqual(t *testing.T, want, got *knov1.Interval) {
	t.Helper()
	if want == nil || got == nil {
		require.Equal(t, want == nil, got == nil, "nil-ness disagrees: want %v got %v", want, got)
		return
	}
	const eps = 1e-9
	require.InDelta(t, want.GetLow(), got.GetLow(), eps, "Low")
	require.InDelta(t, want.GetHigh(), got.GetHigh(), eps, "High")
	require.Equal(t, want.GetLevel(), got.GetLevel(), "Level")
	require.Equal(t, want.GetMethod(), got.GetMethod(), "Method")
	require.Equal(t, want.GetSidedness(), got.GetSidedness(), "Sidedness")
	require.Equal(t, want.GetNPairs(), got.GetNPairs(), "NPairs")
}

func twoSided(low, high, level float64, nPairs int32) *knov1.Interval {
	n := nPairs
	return &knov1.Interval{
		Low: low, High: high, Level: level, Method: "t",
		Sidedness: knov1.Sidedness_SIDEDNESS_TWO_SIDED, NPairs: &n,
	}
}

func lowerBound(low, level float64, nPairs int32) *knov1.Interval {
	n := nPairs
	return &knov1.Interval{
		Low: low, Level: level,
		Sidedness: knov1.Sidedness_SIDEDNESS_LOWER, NPairs: &n,
	}
}

// TestNetEffectAgreesWithPreExtractionFormula is the plan's required
// characterization test: NetEffect and netIntervalPreExtraction must agree
// on the shapes core/select.go feeds them, across a battery of randomized
// inputs, or the extraction is not behavior-preserving.
func TestNetEffectAgreesWithPreExtractionFormula(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(20260901))

	for i := 0; i < 2000; i++ {
		goalMean := rng.Float64()*4 - 2
		goalHalf := rng.Float64()*2 + 0.01
		goal := twoSided(goalMean-goalHalf, goalMean+goalHalf, 0.95, int32(rng.Intn(50)+2))

		controlMean := rng.Float64()*4 - 2
		controlLow := controlMean - (rng.Float64()*2 + 0.01)
		controlLevel := 0.90 + rng.Float64()*0.09
		nPairs := int32(rng.Intn(50) + 2)
		control := lowerBound(controlLow, controlLevel, nPairs)

		nT := rng.Intn(200) + 1
		nC := rng.Intn(200) + 1
		shared := rng.Intn(2) == 0
		level := 0.90 + rng.Float64()*0.09

		want := netIntervalPreExtraction(goalMean, goalHalf, control, nT, nC, controlMean, shared, level)
		got := interval.NetEffect(goal, controlMean, control, nT, nC, shared, level)
		requireIntervalsEqual(t, want, got)
	}
}

// TestNetEffectAgreesOnDegenerateShapes covers the refusal boundaries
// core/select.go's TestNetIntervalRefusals already pins at the Valuation
// level: no control, a control with fewer than two pairs, and a
// zero-width harm bound (controlMean equal to control.Low).
func TestNetEffectAgreesOnDegenerateShapes(t *testing.T) {
	t.Parallel()
	goal := twoSided(-0.4, 0.8, 0.975, 10)

	cases := []struct {
		name        string
		control     *knov1.Interval
		nT, nC      int
		controlMean float64
	}{
		{"nil control", nil, 5, 5, -1.5},
		{"control below two pairs", lowerBound(-1.7, 0.95, 1), 5, 5, -1.5},
		{"zero-width harm bound", lowerBound(-1.7, 0.95, 10), 5, 10, -1.7},
		{"zero nT", lowerBound(-1.7, 0.95, 10), 0, 10, -1.5},
		{"zero nC", lowerBound(-1.7, 0.95, 10), 5, 0, -1.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := interval.NetEffect(goal, tc.controlMean, tc.control, tc.nT, tc.nC, true, 0.95)
			require.Nil(t, got, "want a refusal for %s", tc.name)
		})
	}
}

// TestNetEffectRefusesTheWrongSidedness pins that NetEffect checks shape,
// not merely nilness: a control interval that is not one-sided LOWER, or a
// goal interval that is not two-sided, is refused rather than silently
// misinterpreted.
func TestNetEffectRefusesTheWrongSidedness(t *testing.T) {
	t.Parallel()
	control := lowerBound(-1.7, 0.95, 10)
	goal := twoSided(-0.4, 0.8, 0.975, 10)

	require.Nil(t, interval.NetEffect(nil, -1.5, control, 5, 5, true, 0.95))
	require.Nil(t, interval.NetEffect(goal, -1.5, nil, 5, 5, true, 0.95))

	upperGoal := &knov1.Interval{Low: -0.4, High: 0.8, Level: 0.95, Sidedness: knov1.Sidedness_SIDEDNESS_UPPER}
	require.Nil(t, interval.NetEffect(upperGoal, -1.5, control, 5, 5, true, 0.95))

	twoSidedControl := twoSided(-1.7, -1.3, 0.95, 10)
	require.Nil(t, interval.NetEffect(goal, -1.5, twoSidedControl, 5, 5, true, 0.95))
}

// TestNetEffectSharedIsAtLeastAsWideAsIndependent mirrors
// TestNetLossSharedDrawIsConservative one layer up: NetEffect must not
// invert the conservatism NetLoss already guarantees just because the
// control side now comes from a one-sided bound.
func TestNetEffectSharedIsAtLeastAsWideAsIndependent(t *testing.T) {
	t.Parallel()
	goal := twoSided(-0.2, 0.6, 0.95, 20)
	control := lowerBound(-1.0, 0.95, 20)

	shared := interval.NetEffect(goal, -0.8, control, 30, 20, true, 0.95)
	indep := interval.NetEffect(goal, -0.8, control, 30, 20, false, 0.95)
	require.NotNil(t, shared)
	require.NotNil(t, indep)

	sharedHalf := (shared.GetHigh() - shared.GetLow()) / 2
	indepHalf := (indep.GetHigh() - indep.GetLow()) / 2
	require.GreaterOrEqual(t, sharedHalf, indepHalf)
	require.Equal(t, interval.MethodNetLossShared, shared.GetMethod())
	require.Equal(t, interval.MethodNetLossIndep, indep.GetMethod())
}
