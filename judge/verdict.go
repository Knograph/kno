package judge

import (
	"fmt"
	"math"
	"sort"

	"github.com/knograph/kno/core"
)

// decide assigns the verdict, and the ORDER of the checks is the design.
//
// Each check answers a different question, and a run that fails an earlier one
// cannot have the later one meaningfully answered:
//
//  1. Did enough of the run work? A judge that errored on a tenth of the set
//     has not been measured.
//  2. Do the labels agree with each other? A judge cannot be held to an
//     agreement its own labelers do not reach, and reporting a low kappa as
//     the judge's fault when the ceiling is the problem sends a contributor
//     to edit the wrong file.
//  3. Is the error symmetric? If not, kappa is not the attenuation factor and
//     the floor means nothing — so this fails even at a kappa ABOVE the floor,
//     because a direction-biased judge above 0.60 is worse than a symmetric
//     one below it: it moves every measured delta in one direction.
//  4. Where does the interval sit relative to the floor?
func decide(res *Result) {
	if res.NRecords > 0 {
		if rate := float64(res.NErrored) / float64(res.NRecords); rate > MaxErrorRate {
			res.Verdict = VerdictIndeterminate
			res.Cause = fmt.Sprintf(
				"this is not a usable calibration: the judge errored on %d of %d records (%.1f%%)",
				res.NErrored, res.NRecords, rate*100)
			res.Fix = fmt.Sprintf("fix the judge's failures before reading its agreement; "+
				"above %.0f%% the surviving records are a subset chosen by which calls "+
				"happened to work", MaxErrorRate*100)
			return
		}
	}

	if math.IsNaN(res.InterHuman.Kappa) {
		res.Verdict = VerdictIndeterminate
		res.Cause = "the inter-human ceiling is undefined: no two labelers share enough records"
		res.Fix = "have a second labeler label the records a first one did"
		return
	}
	if res.InterHuman.Kappa < res.MinKappa {
		res.Verdict = VerdictIndeterminate
		res.Cause = fmt.Sprintf(
			"the labels do not agree with each other: inter-human kappa is %.3f, below the %.2f floor",
			res.InterHuman.Kappa, res.MinKappa)
		res.Fix = "this is a statement about the SET, not about the judge. Adjudicate the " +
			"records the labelers split on, or sharpen the rubric they are labeling against"
		return
	}

	if math.IsNaN(res.Agreement.Kappa) {
		res.Verdict = VerdictIndeterminate
		res.Cause = "kappa is undefined on this set: every verdict fell in one class"
		res.Fix = "add records of the other class"
		return
	}

	if gap := res.Agreement.SymmetryGap(); gap > MaxSymmetryGap {
		res.Verdict = VerdictFail
		res.Cause = fmt.Sprintf(
			"asymmetric error: sensitivity %.3f against specificity %.3f is a gap of %.3f, "+
				"above the %.2f limit",
			res.Agreement.Sensitivity, res.Agreement.Specificity, gap, MaxSymmetryGap)
		res.Fix = "the floor rests on kappa being the attenuation factor, which holds only " +
			"under symmetric error. A judge biased in one direction moves every measured " +
			"delta that way, and kappa hides it. Fix the direction before reading the number"
		return
	}

	if res.KappaInterval == nil {
		res.Verdict = VerdictIndeterminate
		res.Cause = "no interval could be computed on kappa"
		res.Fix = "add records: a statistic without an interval is not reportable"
		return
	}

	low, high := res.KappaInterval.GetLow(), res.KappaInterval.GetHigh()
	switch {
	case low >= res.MinKappa:
		res.Verdict = VerdictPass
		res.Cause = fmt.Sprintf("kappa %.3f, 95%% CI [%.3f, %.3f], entirely at or above the %.2f floor",
			res.Agreement.Kappa, low, high, res.MinKappa)
	case high < res.MinKappa:
		res.Verdict = VerdictFail
		res.Cause = fmt.Sprintf("kappa %.3f, 95%% CI [%.3f, %.3f], entirely below the %.2f floor",
			res.Agreement.Kappa, low, high, res.MinKappa)
		res.Fix = fmt.Sprintf("this judge attenuates every delta measured through it by "+
			"about %.0f%%. Improve the prompt, or lower --min-kappa deliberately and say "+
			"in the PR what you traded", (1-math.Max(res.Agreement.Kappa, 0))*100)
	default:
		res.Verdict = VerdictIndeterminate
		res.Cause = fmt.Sprintf("kappa %.3f, 95%% CI [%.3f, %.3f], straddles the %.2f floor",
			res.Agreement.Kappa, low, high, res.MinKappa)
		res.Fix = "the set is too small to decide. Add records. \"We cannot tell\" is not " +
			"\"it is fine\", which is why this fails rather than passing"
	}
}

// Graded is what a UNIT_INTERVAL Goal gets instead of a gate.
//
// Reported and NOT gated in v0.2. Kappa is undefined on continuous scores, and
// gating a graded judge needs an anchored scale the calibration format does not
// yet carry — inventing one here would be exactly the invented threshold this
// command exists to avoid.
type Graded struct {
	// WeightedKappa is quadratic-weighted kappa over the labels' own anchor
	// bins.
	WeightedKappa float64

	// Spearman is the rank correlation between the judge's values and the
	// humans'.
	Spearman float64

	// MAE is the mean absolute error on the Goal's own scale.
	MAE float64

	// NBins is how many distinct human anchors the set uses.
	NBins int
}

// gradeAll computes the graded report.
func gradeAll(set *Set, scores []*core.Score, errored []bool) *Graded {
	var judged, human []float64
	for i, rec := range set.Records {
		if errored[i] || scores[i] == nil {
			continue
		}
		judged = append(judged, scores[i].GetValue())
		human = append(human, rec.Adjudicated.Value)
	}
	if len(judged) < MinRecords {
		return &Graded{WeightedKappa: math.NaN(), Spearman: math.NaN(), MAE: math.NaN()}
	}

	var sum float64
	for i := range judged {
		sum += math.Abs(judged[i] - human[i])
	}
	bins := anchorBins(human)
	return &Graded{
		WeightedKappa: quadraticWeightedKappa(judged, human, bins),
		Spearman:      spearman(judged, human),
		MAE:           sum / float64(len(judged)),
		NBins:         len(bins),
	}
}

// anchorBins are the distinct values the humans actually used.
//
// The bins come from the LABELS rather than from an even split of [0, 1]: an
// even split invents an anchored scale, which is the thing this whole branch
// declines to do.
func anchorBins(human []float64) []float64 {
	seen := map[float64]struct{}{}
	for _, v := range human {
		seen[v] = struct{}{}
	}
	out := make([]float64, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Float64s(out)
	return out
}

// binOf snaps a value to the nearest human anchor.
func binOf(v float64, bins []float64) int {
	best, bestDist := 0, math.Inf(1)
	for i, b := range bins {
		if d := math.Abs(v - b); d < bestDist {
			best, bestDist = i, d
		}
	}
	return best
}

// quadraticWeightedKappa is Cohen's kappa with quadratic disagreement weights.
func quadraticWeightedKappa(judge, human []float64, bins []float64) float64 {
	k := len(bins)
	if k < 2 {
		return math.NaN()
	}
	observed := make([][]float64, k)
	for i := range observed {
		observed[i] = make([]float64, k)
	}
	judgeMarginal := make([]float64, k)
	humanMarginal := make([]float64, k)
	for i := range judge {
		a, b := binOf(judge[i], bins), binOf(human[i], bins)
		observed[a][b]++
		judgeMarginal[a]++
		humanMarginal[b]++
	}

	n := float64(len(judge))
	denom := float64((k - 1) * (k - 1))
	var num, den float64
	for i := range k {
		for j := range k {
			w := float64((i-j)*(i-j)) / denom
			num += w * observed[i][j] / n
			den += w * (judgeMarginal[i] / n) * (humanMarginal[j] / n)
		}
	}
	if den == 0 {
		return math.NaN()
	}
	return 1 - num/den
}

// spearman is the rank correlation, with ties averaged.
func spearman(a, b []float64) float64 {
	ra, rb := ranks(a), ranks(b)
	n := float64(len(a))
	var ma, mb float64
	for i := range ra {
		ma += ra[i]
		mb += rb[i]
	}
	ma, mb = ma/n, mb/n

	var cov, va, vb float64
	for i := range ra {
		da, db := ra[i]-ma, rb[i]-mb
		cov += da * db
		va += da * da
		vb += db * db
	}
	if va == 0 || vb == 0 {
		return math.NaN()
	}
	return cov / math.Sqrt(va*vb)
}

// ranks returns average ranks, so tied values do not get an arbitrary order.
func ranks(v []float64) []float64 {
	idx := make([]int, len(v))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(i, j int) bool { return v[idx[i]] < v[idx[j]] })

	out := make([]float64, len(v))
	for i := 0; i < len(idx); {
		j := i
		for j+1 < len(idx) && v[idx[j+1]] == v[idx[i]] {
			j++
		}
		avg := float64(i+j)/2 + 1
		for k := i; k <= j; k++ {
			out[idx[k]] = avg
		}
		i = j + 1
	}
	return out
}
