package interval

import (
	"math"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// Method strings recorded on the intervals NetLoss produces. Moved here from
// stats/portfolio (see NetLoss's own doc for why) rather than duplicated:
// stats/portfolio already imports stats/interval for Quantile, so the reverse
// import would cycle, and this package is where NetEffect — the thing that
// actually needs these — has to live.
const (
	// MethodNetLossShared is a net-loss interval combined under the
	// assumption that the two deltas are perfectly correlated — the shape a
	// shared recorded baseline draw produces.
	MethodNetLossShared = "net-loss-shared"

	// MethodNetLossIndep is a net-loss interval combined under the
	// assumption that the two deltas are independent — the shape a fresh
	// control arm produces, or the best case a recorded baseline allows.
	MethodNetLossIndep = "net-loss-indep"
)

// NetDelta is one arm of the net-loss judgement: a point estimate, its
// interval half-width, and the population the mean is over. See NetLoss.
type NetDelta struct {
	// Mean is the point estimate.
	Mean float64

	// Half is the half-width of a TWO-SIDED interval at the level NetLoss is
	// called with. A one-sided bound must be widened by the caller first —
	// its center is unknown, so this package cannot reconstruct the far
	// side, and silently reading a one-sided bound as symmetric would
	// understate the interval in exactly the direction a REGRESSION verdict
	// cares about. NetEffect does this widening for the caller.
	Half float64

	// N is the population the mean was measured over.
	N int
}

// NetLoss combines a treatment delta and a control delta into one net
// judgement, weighted by their populations, with an interval that accounts
// for the shared recorded-baseline draw conservatively.
//
// The point estimate is the population-weighted mean:
//
//	net = (nT*meanT + nC*meanC) / (nT + nC)
//
// The interval is where the covariance shows up. The two deltas both pair
// against the recorded baseline draw (docs/debt.md#66 names the
// correlation), which makes their errors positively correlated — the
// variance of the net is LARGER than the independent combination, by up to
// the full product term. The exact covariance is not recoverable from the
// recorded aggregates, so:
//
//   - sharedDraw=true (the control arm read the recorded baseline) takes the
//     perfectly-correlated bound, the widest the unknown covariance allows:
//     half = (nT*halfT + nC*halfC) / (nT + nC).
//   - sharedDraw=false (a fresh control arm) takes the independent bound:
//     half = sqrt((nT*halfT)^2 + (nC*halfC)^2) / (nT + nC).
//
// The shared bound is always at least as wide as the independent one, and
// the caller who cannot say which scheme a run used must pass
// sharedDraw=true — the conservative direction, and the one this package
// documents.
//
// Returns nil when any input is unusable (non-positive population,
// non-finite or non-positive half-width, non-finite mean, invalid level)
// rather than laundering a bad input into an interval.
//
// Moved here from stats/portfolio by the bridge eval-seam plan (§4): bridge
// needs this same combination for its interference read, and
// stats/portfolio already imports stats/interval for Quantile, so a
// stats/interval -> stats/portfolio import would cycle. stats/portfolio.NetLoss
// and stats/portfolio.NetDelta remain as aliases to these — see that
// package's doc comment.
func NetLoss(treatment, control NetDelta, sharedDraw bool, level float64) *knov1.Interval {
	if treatment.N <= 0 || control.N <= 0 {
		return nil
	}
	if treatment.Half <= 0 || control.Half <= 0 {
		// A zero-width arm would drag the combined interval toward
		// certainty regardless of the other arm's width — the exact
		// failure this package refuses to manufacture.
		return nil
	}
	if !validNetEffect(level, treatment.Mean, treatment.Half, control.Mean, control.Half) {
		return nil
	}
	nT, nC := float64(treatment.N), float64(control.N)
	total := nT + nC
	mean := (nT*treatment.Mean + nC*control.Mean) / total

	var half float64
	method := MethodNetLossIndep
	if sharedDraw {
		// Perfectly-correlated bound: the covariance term is maximal, so
		// the half-widths combine linearly.
		half = (nT*treatment.Half + nC*control.Half) / total
		method = MethodNetLossShared
	} else {
		// Independent bound: variances add.
		half = math.Sqrt(nT*nT*treatment.Half*treatment.Half+nC*nC*control.Half*control.Half) / total
	}
	if math.IsInf(half, 0) || half <= 0 {
		return nil
	}

	nn := int32(total)
	return &knov1.Interval{
		Low:       mean - half,
		High:      mean + half,
		Level:     level,
		Method:    method,
		Sidedness: knov1.Sidedness_SIDEDNESS_TWO_SIDED,
		NPairs:    &nn,
	}
}

// NetEffect combines a two-sided goal-delta interval and a one-sided
// control-delta bound into one net judgement — bridge's interference read,
// and (via core/select.go's netInterval, which now delegates here) Select's
// REGRESSION gate.
//
// Extracted from core/select.go's unexported netInterval by the bridge
// eval-seam plan (§4/§9). The plan's own text records why this is an
// extraction at the SECOND occurrence, which CLAUDE.md normally reserves for
// the third: a subtly wrong copy here does not produce a bug that looks like
// a bug, it produces a confident false claim that an Asset (or an ablation
// group) is harmful — prime directive 5's failure mode. See
// docs/plans/2026-09-01-bridge-eval-seam.md §4.
//
// goal is the GOAL-delta interval this net judgement is combined with —
// already Bonferroni-corrected where a caller corrects (Select corrects
// before calling; bridge's Bonferroni is likewise goal-only, see the plan's
// §8). level is the RAW, uncorrected confidence level: it is used only to
// widen control's one-sided bound to a two-sided one at the SAME level the
// (uncorrected) net judgement is reported at, matching what the
// pre-extraction netInterval did — it never received the corrected level.
//
// control is a ONE-SIDED LOWER bound (interval.HarmBound's shape): only its
// Low field is meaningful, so its center — the raw point-estimate mean the
// bound was built around — cannot be recovered from the Interval alone and
// must be passed as controlMean. goal, by contrast, is two-sided and DOES
// carry its own center ((Low+High)/2), which is why NetEffect needs no
// separate goalMean parameter.
//
// nT and nC are the populations the two means were measured over. shared
// reports whether the two deltas pair against the same underlying draw —
// ALWAYS true for bridge, because both Δ_group and Δ_control pair against
// the same all-in scores (see the plan's §4 amendment, finding R8): passing
// false here would narrow the interval and manufacture a false-confident
// harm claim.
//
// Returns nil when goal or control is missing or of the wrong sidedness,
// when either population is non-positive, when control carries fewer than
// two pairs, or when the widened control half-width is non-finite or
// non-positive — matching netInterval's existing guards exactly.
func NetEffect(
	goal *knov1.Interval,
	controlMean float64,
	control *knov1.Interval,
	nT, nC int,
	shared bool,
	level float64,
) *knov1.Interval {
	if goal == nil || control == nil {
		return nil
	}
	if goal.GetSidedness() != knov1.Sidedness_SIDEDNESS_TWO_SIDED {
		return nil
	}
	if control.GetSidedness() != knov1.Sidedness_SIDEDNESS_LOWER {
		return nil
	}
	if nT <= 0 || nC <= 0 || control.GetNPairs() < 2 {
		return nil
	}

	df := int(control.GetNPairs()) - 1
	// The harm bound's one-sided half at its OWN level, widened to the
	// two-sided half at the judgement's level by the quantile ratio.
	halfC := (controlMean - control.GetLow()) *
		Quantile(level, knov1.Sidedness_SIDEDNESS_TWO_SIDED, df) /
		Quantile(control.GetLevel(), knov1.Sidedness_SIDEDNESS_LOWER, df)
	if math.IsNaN(halfC) || math.IsInf(halfC, 0) || halfC <= 0 {
		return nil
	}

	goalMean := (goal.GetLow() + goal.GetHigh()) / 2
	goalHalf := (goal.GetHigh() - goal.GetLow()) / 2

	return NetLoss(
		NetDelta{Mean: goalMean, Half: goalHalf, N: nT},
		NetDelta{Mean: controlMean, Half: halfC, N: nC},
		shared, level,
	)
}

// validNetEffect reports whether a level and a set of numbers are usable
// NetLoss inputs. Named distinctly from validLevel (interval.go) because it
// checks a whole tuple of numbers, not one level.
func validNetEffect(level float64, numbers ...float64) bool {
	if math.IsNaN(level) || level <= 0.5 || level >= 1 {
		return false
	}
	for _, x := range numbers {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return false
		}
	}
	return true
}
