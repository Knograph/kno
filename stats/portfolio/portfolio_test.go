package portfolio

import (
	"math"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/interval"
	"github.com/stretchr/testify/require"
)

func TestNetLossWeightedPopulationArithmetic(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		treatment NetDelta
		control   NetDelta
		shared    bool
		wantMean  float64
	}{
		{
			name:      "equal populations average",
			treatment: NetDelta{Mean: 0.10, Half: 0.02, N: 10},
			control:   NetDelta{Mean: 0.00, Half: 0.02, N: 10},
			shared:    true,
			wantMean:  0.05,
		},
		{
			name:      "unequal populations weight toward the larger",
			treatment: NetDelta{Mean: 0.10, Half: 0.02, N: 30},
			control:   NetDelta{Mean: 0.00, Half: 0.02, N: 10},
			shared:    false,
			wantMean:  0.075,
		},
		{
			name:      "single-case control arm",
			treatment: NetDelta{Mean: 0.20, Half: 0.04, N: 3},
			control:   NetDelta{Mean: -0.04, Half: 0.01, N: 1},
			shared:    true,
			wantMean:  0.14,
		},
		{
			name:      "harmful control drags the net down",
			treatment: NetDelta{Mean: 0.05, Half: 0.01, N: 20},
			control:   NetDelta{Mean: -0.05, Half: 0.01, N: 20},
			shared:    false,
			wantMean:  0.0,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			iv := NetLoss(tc.treatment, tc.control, tc.shared, interval.DefaultLevel)
			require.NotNil(t, iv)
			require.InDelta(t, tc.wantMean, (iv.GetLow()+iv.GetHigh())/2, 1e-12)
			require.Equal(t, float64(tc.treatment.N+tc.control.N), float64(iv.GetNPairs()))
			require.Equal(t, knov1.Sidedness_SIDEDNESS_TWO_SIDED, iv.GetSidedness())
		})
	}
}

func TestNetLossSharedDrawIsConservative(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		treatment NetDelta
		control   NetDelta
	}{
		{
			name:      "equal halves",
			treatment: NetDelta{Mean: 0.05, Half: 0.01, N: 10},
			control:   NetDelta{Mean: -0.01, Half: 0.03, N: 10},
		},
		{
			name:      "lopsided populations",
			treatment: NetDelta{Mean: 0.05, Half: 0.02, N: 100},
			control:   NetDelta{Mean: -0.01, Half: 0.05, N: 4},
		},
		{
			name:      "identical widths",
			treatment: NetDelta{Mean: 0.1, Half: 0.02, N: 8},
			control:   NetDelta{Mean: 0.0, Half: 0.02, N: 8},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			shared := NetLoss(tc.treatment, tc.control, true, interval.DefaultLevel)
			indep := NetLoss(tc.treatment, tc.control, false, interval.DefaultLevel)
			require.NotNil(t, shared)
			require.NotNil(t, indep)
			sharedHalf := (shared.GetHigh() - shared.GetLow()) / 2
			indepHalf := (indep.GetHigh() - indep.GetLow()) / 2
			require.GreaterOrEqual(t, sharedHalf, indepHalf,
				"the shared recorded baseline can only ADD covariance; its bound must never be narrower")
			require.Equal(t, MethodNetLossShared, shared.GetMethod())
			require.Equal(t, MethodNetLossIndep, indep.GetMethod())
		})
	}
}

func TestNetLossIndependentBoundExact(t *testing.T) {
	t.Parallel()
	// sqrt((nT*hT)^2 + (nC*hC)^2) / (nT+nC), verified against hand arithmetic.
	treatment := NetDelta{Mean: 0.1, Half: 0.02, N: 3}
	control := NetDelta{Mean: -0.1, Half: 0.01, N: 4}
	iv := NetLoss(treatment, control, false, interval.DefaultLevel)
	require.NotNil(t, iv)
	wantHalf := math.Sqrt(9*0.02*0.02+16*0.01*0.01) / 7
	require.InDelta(t, wantHalf, (iv.GetHigh()-iv.GetLow())/2, 1e-12)
}

func TestNetLossRefusesBadInputs(t *testing.T) {
	t.Parallel()
	ok := NetDelta{Mean: 0.05, Half: 0.01, N: 10}
	cases := []struct {
		name      string
		treatment NetDelta
		control   NetDelta
		level     float64
	}{
		{"zero treatment population", NetDelta{Mean: 0.05, Half: 0.01, N: 0}, ok, interval.DefaultLevel},
		{"zero control population", ok, NetDelta{Mean: 0.05, Half: 0.01, N: 0}, interval.DefaultLevel},
		{"NaN mean", NetDelta{Mean: math.NaN(), Half: 0.01, N: 10}, ok, interval.DefaultLevel},
		{"infinite mean", NetDelta{Mean: math.Inf(1), Half: 0.01, N: 10}, ok, interval.DefaultLevel},
		{"zero half-width", NetDelta{Mean: 0.05, Half: 0, N: 10}, ok, interval.DefaultLevel},
		{"NaN half-width", NetDelta{Mean: 0.05, Half: math.NaN(), N: 10}, ok, interval.DefaultLevel},
		{"level at the boundary", ok, ok, 0.5},
		{"level above one", ok, ok, 1.0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Nil(t, NetLoss(tc.treatment, tc.control, true, tc.level))
			require.Nil(t, NetLoss(tc.treatment, tc.control, false, tc.level))
		})
	}
}

// twoSided returns a two-sided interval with the given center, half-width,
// level, method and pair count — the shape Correct accepts.
func twoSided(center, half, level float64, method string, n int) *knov1.Interval {
	n32 := int32(n)
	return &knov1.Interval{
		Low:       center - half,
		High:      center + half,
		Level:     level,
		Method:    method,
		Sidedness: knov1.Sidedness_SIDEDNESS_TWO_SIDED,
		NPairs:    &n32,
	}
}

func TestCorrectBonferroniLevel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		level     float64
		nScreened int
		wantLevel float64
	}{
		{"two comparisons at 95%", 0.95, 2, 0.975},
		{"ten comparisons at 95%", 0.95, 10, 0.995},
		{"hundred comparisons at 95%", 0.95, 100, 0.9995},
		{"two comparisons at 90%", 0.90, 2, 0.95},
		{"one-error-in-fifty across a screen", 0.98, 50, 0.9996},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			iv := Correct(twoSided(0.1, 0.02, tc.level, interval.MethodStudentT, 10), tc.nScreened)
			require.NotNil(t, iv)
			require.InDelta(t, tc.wantLevel, iv.GetLevel(), 1e-12)
		})
	}
}

func TestCorrectWidensWithMoreComparisons(t *testing.T) {
	t.Parallel()
	base := twoSided(0.1, 0.02, interval.DefaultLevel, interval.MethodStudentT, 10)
	forTwo := Correct(base, 2)
	forTen := Correct(base, 10)
	forHundred := Correct(base, 100)
	require.NotNil(t, forTwo)
	require.NotNil(t, forTen)
	require.NotNil(t, forHundred)
	half := func(iv *knov1.Interval) float64 { return (iv.GetHigh() - iv.GetLow()) / 2 }
	require.Greater(t, half(forTen), half(forTwo))
	require.Greater(t, half(forHundred), half(forTen))
	// The claim is rescaled, not relocated or re-sampled.
	require.InDelta(t, 0.1, (forHundred.GetLow()+forHundred.GetHigh())/2, 1e-12)
	require.Equal(t, base.GetMethod(), forHundred.GetMethod())
	require.Equal(t, base.GetNPairs(), forHundred.GetNPairs())
}

func TestCorrectStudentTScalingIsTheQuantileRatio(t *testing.T) {
	t.Parallel()
	// A t-interval's half-width is q * sqrt(variance/n), so the corrected
	// width is the recorded width scaled by the t-quantile ratio at the
	// corrected level with the recorded degrees of freedom.
	df := 9
	iv := Correct(twoSided(0.1, 0.02, interval.DefaultLevel, interval.MethodStudentT, df+1), 4)
	require.NotNil(t, iv)
	wantRatio := interval.Quantile(iv.GetLevel(), knov1.Sidedness_SIDEDNESS_TWO_SIDED, df) /
		interval.Quantile(interval.DefaultLevel, knov1.Sidedness_SIDEDNESS_TWO_SIDED, df)
	require.InDelta(t, 0.02*wantRatio, (iv.GetHigh()-iv.GetLow())/2, 1e-12)
}

func TestCorrectAdjustedWaldScalingIsTheNormalQuantileRatio(t *testing.T) {
	t.Parallel()
	iv := Correct(twoSided(0.1, 0.02, interval.DefaultLevel, interval.MethodAdjustedWald, 20), 8)
	require.NotNil(t, iv)
	wantRatio := interval.Quantile(iv.GetLevel(), knov1.Sidedness_SIDEDNESS_TWO_SIDED, 0) /
		interval.Quantile(interval.DefaultLevel, knov1.Sidedness_SIDEDNESS_TWO_SIDED, 0)
	require.InDelta(t, 0.02*wantRatio, (iv.GetHigh()-iv.GetLow())/2, 1e-12)
}

func TestCorrectSignScalingIsExact(t *testing.T) {
	t.Parallel()
	// For method "sign", half = -ln(1-level)/n * scale, so the corrected
	// half-width is the recorded one rescaled by the log-ratio alone.
	base := twoSided(0.05, 0.15, interval.DefaultLevel, interval.MethodSign, 20)
	iv := Correct(base, 5)
	require.NotNil(t, iv)
	wantRatio := -math.Log(1-iv.GetLevel()) / -math.Log(1-base.GetLevel())
	require.InDelta(t, 0.15*wantRatio, (iv.GetHigh()-iv.GetLow())/2, 1e-12)
}

func TestCorrectRefusesWhatItCannotRescale(t *testing.T) {
	t.Parallel()
	n32 := int32(10)
	oneSided := &knov1.Interval{
		Low:       0.08,
		Level:     interval.DefaultLevel,
		Method:    interval.MethodStudentT,
		Sidedness: knov1.Sidedness_SIDEDNESS_LOWER,
		NPairs:    &n32,
	}
	unknown := twoSided(0.1, 0.02, interval.DefaultLevel, "mystery-method", 10)
	noPairs := twoSided(0.1, 0.02, interval.DefaultLevel, interval.MethodStudentT, 10)
	noPairs.NPairs = nil
	onePair := twoSided(0.1, 0.02, interval.DefaultLevel, interval.MethodStudentT, 1)

	cases := []struct {
		name      string
		iv        *knov1.Interval
		nScreened int
	}{
		{"nil interval", nil, 10},
		{"one-sided bound", oneSided, 10},
		{"single comparison", twoSided(0.1, 0.02, interval.DefaultLevel, interval.MethodStudentT, 10), 1},
		{"unknown method", unknown, 10},
		{"student-t without pair count", noPairs, 10},
		{"student-t with one pair", onePair, 10},
		{"zero-width interval", twoSided(0.1, 0, interval.DefaultLevel, interval.MethodSign, 10), 10},
		{"NaN level", twoSided(0.1, 0.02, math.NaN(), interval.MethodStudentT, 10), 10},
		{"level at zero", twoSided(0.1, 0.02, 0, interval.MethodStudentT, 10), 10},
		{"infinite bound", twoSided(math.Inf(1), 0.02, interval.DefaultLevel, interval.MethodStudentT, 10), 10},
		{"NaN bound", twoSided(math.NaN(), 0.02, interval.DefaultLevel, interval.MethodStudentT, 10), 10},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Nil(t, Correct(tc.iv, tc.nScreened))
		})
	}
}

// TestValidRefusesNaNAndInfiniteInputsDirectly covers valid's own branches
// beyond what Correct's callers happen to exercise — this package's helper,
// not exported, but load-bearing for every refusal Correct makes.
func TestValidRefusesNaNAndInfiniteInputsDirectly(t *testing.T) {
	t.Parallel()
	require.False(t, valid(math.NaN(), 0.1))
	require.False(t, valid(0.3, 0.1)) // <= 0.5
	require.False(t, valid(1.0, 0.1)) // >= 1
	require.False(t, valid(0.95, math.NaN()))
	require.False(t, valid(0.95, math.Inf(1)))
	require.False(t, valid(0.95, math.Inf(-1)))
	require.True(t, valid(0.95, 0.1, -0.2, 3.4))
}
