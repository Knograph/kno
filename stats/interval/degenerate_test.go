package interval_test

import (
	"math"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/interval"
)

// TestIdenticalSmallDeltasDoNotCollapseTheInterval is the regression test for
// a confidence interval that had collapsed to a point.
//
// `variance <= 0` was the guard for "every pair identical", and summing
// (d-mean)^2 over identical SMALL deltas does not give zero — the mean of
// fifty copies of 0.001 is not 0.001 in binary floating point, so the variance
// is a positive number made of rounding noise. That took the Student-t path
// and produced a half-width around 1e-19, reported as method "t": a claim of
// perfect certainty from a sample with no spread at all.
//
// Every caller was exposed — Value's delta, Validate's holdout gain, Select's
// screening interval all compute through here.
func TestIdenticalSmallDeltasDoNotCollapseTheInterval(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		delta float64
		n     int
	}{
		{"a magnitude whose mean is exactly representable", 0.5, 5},
		{"a magnitude whose mean is not", 0.001, 50},
		{"smaller still", 0.0001, 8},
		{"large", 1234.5678, 12},
		{"negative", -0.002, 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			deltas := make([]float64, tc.n)
			for i := range deltas {
				deltas[i] = tc.delta
			}
			ci := interval.Paired(
				deltas,
				knov1.ScoreDomain_SCORE_DOMAIN_CONTINUOUS_UNBOUNDED,
				1,
				interval.DefaultLevel,
			)
			if ci == nil {
				t.Fatal("no interval for a sample of identical deltas; the sign " +
					"bound exists precisely so this case has an honest answer")
			}

			width := ci.GetHigh() - ci.GetLow()
			// The floor is relative: what must never happen is an interval
			// indistinguishable from a point at the scale of its own centre.
			floor := math.Abs(tc.delta) * 1e-6
			if width <= floor {
				t.Errorf("interval collapsed: [%g, %g] width %g via method %q. "+
					"Identical observations are evidence, but never certainty — "+
					"a width at the noise floor of its own centre reads as a "+
					"measurement rather than as the refusal it should be",
					ci.GetLow(), ci.GetHigh(), width, ci.GetMethod())
			}
			if ci.GetMethod() != "sign" {
				t.Errorf("method %q for a sample with no spread; the sign bound "+
					"is the one that does not assume a spread it cannot see",
					ci.GetMethod())
			}
		})
	}
}

// TestRealSpreadStillTakesTheStudentTPath: the guard must not swallow samples
// that genuinely vary. A guard that always fires is not a guard.
func TestRealSpreadStillTakesTheStudentTPath(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		deltas []float64
	}{
		{"ordinary spread", []float64{0.1, 0.2, 0.15, 0.3, 0.05}},
		{"one dissenter among identical values", []float64{0.001, 0.001, 0.001, 0.001, 0.002}},
		{"tiny but real spread", []float64{1e-6, 2e-6, 1.5e-6, 3e-6, 2.5e-6}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ci := interval.Paired(
				tc.deltas,
				knov1.ScoreDomain_SCORE_DOMAIN_CONTINUOUS_UNBOUNDED,
				1,
				interval.DefaultLevel,
			)
			if ci == nil {
				t.Fatal("no interval for a sample that genuinely varies")
			}
			if ci.GetMethod() != "t" {
				t.Errorf("method %q for a sample with real spread; the degenerate "+
					"guard has widened until it catches data it should not",
					ci.GetMethod())
			}
		})
	}
}
