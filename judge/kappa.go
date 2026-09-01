package judge

import (
	"math"
	"sort"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// Every statistic this package REPORTS is rounded to four decimal places at
// the point it is computed, so the human line, the --json document and the
// goldens all carry one number.
//
// This is correctness, not presentation, and it is the same treatment
// cli/evalinspect.go applies to separable_effect for the same reason. Two
// distinct forces put the tail digits of these numbers outside our control:
//
//   - Go permits fusing a multiply-add into a single FMA instruction. Chance
//     agreement is `jp*hp + (1-jp)*(1-hp)` — textbook FMA shape — and arm64
//     fuses it where amd64 does not, so kappa's last bits genuinely differ by
//     architecture. It happened to agree on the committed set; "it agreed on
//     this input" is not "it is stable", and the resample sweeps this package
//     runs will find a less lucky value.
//   - The bootstrap's bounds come from linear interpolation between order
//     statistics, and the degenerate path goes through math.Log, which is
//     architecture-specific outright. That is the one that actually broke:
//     a golden holding 0.929508759876331 on arm64 read 0.9295087598763309 on
//     amd64, one ULP apart, and no re-recording can satisfy both.
//
// The second half of evalinspect's argument applies here with MORE force, not
// less. A percentile bootstrap over thirty records does not carry seventeen
// significant digits. Emitting them is a false precision claim, in a document
// whose entire job is honest reporting.

// round4 rounds a POINT ESTIMATE to four decimal places.
func round4(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return f
	}
	return math.Round(f*1e4) / 1e4
}

// roundInterval rounds an interval's bounds to four decimal places.
//
// TO NEAREST, the same rule cli/evalinspect.go applies to separable_effect,
// which is also a bound. Rounding a bound OUTWARD is the tempting choice —
// it can only ever widen, and this repository does not err toward claiming
// confidence — and it was rejected, for two reasons that point the same way.
//
// It is less stable at exactly the values that occur. Floor and Ceil break at
// grid points; round-to-nearest breaks at half-grid points. A kappa that is a
// small rational — 0.35, 0.5, the order statistics a bootstrap quantile lands
// on when it does not interpolate — sits ON the 1e-4 grid and nowhere near a
// half-grid point, so a 1-ULP difference between architectures flips Floor and
// leaves Round alone. Choosing the outward form would trade a cosmetic
// property for a reappearance of the exact bug this rounding exists to kill.
//
// And the property being bought is not worth anything. Rounding to nearest can
// narrow an interval by at most 1e-4, against typical widths near 0.5 — while
// the percentile bootstrap's own measured coverage error on the decision grid
// is about 0.03 (TestBootstrapCoverageOnTheDecisionGrid). Insisting on the
// conservative direction at 1e-4 optimizes a rounding artifact three orders of
// magnitude below the method's real uncertainty.
//
// It returns a new message rather than mutating in place: the caller's
// interval may already be recorded elsewhere, and a rounding pass is not a
// reason for a value to change under someone.
func roundInterval(iv *knov1.Interval) *knov1.Interval {
	if iv == nil {
		return nil
	}
	out := &knov1.Interval{
		Level:     iv.GetLevel(),
		Method:    iv.GetMethod(),
		Sidedness: iv.GetSidedness(),
		Low:       round4(iv.GetLow()),
		High:      round4(iv.GetHigh()),
	}
	if n := iv.GetNPairs(); n != 0 {
		out.NPairs = &n
	}
	// build() writes zero rather than an infinity for the unbounded side of a
	// one-sided bound, and round4(0) is 0, so nothing to restore.
	//
	// A two-sided interval cannot collapse here: build() and buildBounds()
	// both refuse a zero-width interval, and the narrowest either can produce
	// is the degenerate bootstrap's rule-of-three width, 3/n, which stays
	// above 1e-4 for any record count a calibration set can reach.
	return out
}

// Agreement is how two binary verdict vectors agree, reported six ways.
//
// Kappa alone cannot say which way a judge is wrong, and the direction of the
// error is the thing that matters most: a judge that never says "fail"
// silently attenuates every delta toward the set's prevalence, and that is
// invisible in a single scalar.
type Agreement struct {
	// N is the number of records both vectors covered.
	N int

	// TP, FP, TN, FN are the confusion counts, with the JUDGE as the
	// predictor and the human reference as the truth.
	TP, FP, TN, FN int

	// Raw is the fraction of records the two agreed on.
	//
	// Reported and NEVER gated on. A judge that answers "good" unconditionally
	// scores the set's prevalence here, which is the exact failure kappa
	// exists to catch — and printing the two side by side is itself the lesson.
	Raw float64

	// Kappa is Cohen's kappa. NaN when it is undefined (every label in one
	// class, so chance agreement is 1 and the denominator is 0).
	Kappa float64

	// Sensitivity is the fraction of human-pass records the judge passed.
	// Specificity is the fraction of human-fail records the judge failed.
	Sensitivity, Specificity float64

	// JudgePositiveRate and HumanPositiveRate are the two marginals. A reader
	// with both can see whether a low kappa is a bad judge or a lopsided set.
	JudgePositiveRate, HumanPositiveRate float64
}

// SymmetryGap is |sensitivity − specificity|.
//
// The floor's derivation assumes NON-DIFFERENTIAL error: that the judge
// misclassifies at the same rate in both directions. Under asymmetric error
// kappa is no longer the attenuation factor and can mask a direction-biased
// judge, so the assumption is measured rather than assumed, and this is the
// measurement.
func (a Agreement) SymmetryGap() float64 {
	// Rounded, because subtracting two four-place floats does not produce a
	// four-place float: 1.0 - 0.875 is exact, but 0.8751 - 0.7502 is not, and
	// the residue would reach --json as symmetry_gap's tail digits.
	return round4(math.Abs(a.Sensitivity - a.Specificity))
}

// Agree computes the agreement of a judge's verdicts against a human
// reference.
//
// The two slices are index-aligned over the same records. A length mismatch
// returns a zero Agreement with a NaN kappa rather than truncating: comparing
// vectors of different lengths is a caller bug, and silently comparing a
// prefix would produce a number.
func Agree(judge, human []bool) Agreement {
	if len(judge) != len(human) || len(judge) == 0 {
		return Agreement{Kappa: math.NaN()}
	}

	var a Agreement
	a.N = len(judge)
	for i := range judge {
		switch {
		case judge[i] && human[i]:
			a.TP++
		case judge[i] && !human[i]:
			a.FP++
		case !judge[i] && human[i]:
			a.FN++
		default:
			a.TN++
		}
	}

	n := float64(a.N)
	raw := float64(a.TP+a.TN) / n
	judgePositive := float64(a.TP+a.FP) / n
	humanPositive := float64(a.TP+a.FN) / n

	// Kappa is computed from the UNROUNDED marginals and then rounded once.
	// Rounding the inputs first would propagate a 1e-4 quantum through a
	// division by (1 - p_e), which is small near the kappa paradox and turns
	// a display convention into an arithmetic error.
	a.Kappa = round4(kappaFrom(raw, judgePositive, humanPositive))
	a.Raw = round4(raw)
	a.JudgePositiveRate = round4(judgePositive)
	a.HumanPositiveRate = round4(humanPositive)
	a.Sensitivity = round4(ratio(a.TP, a.TP+a.FN))
	a.Specificity = round4(ratio(a.TN, a.TN+a.FP))
	return a
}

// kappaFrom is Cohen's kappa from observed agreement and the two marginals.
//
//	kappa = (p_o − p_e) / (1 − p_e)
//
// The constant judge is the property being bought: on a set that is 85%
// "good", answering "good" unconditionally gives p_o = p_e = 0.85 and kappa =
// 0 exactly, while raw agreement reads 0.85.
//
// NaN, not zero, when p_e is 1. Every label in one class means chance
// agreement is certain, and a kappa of 0 there would read as "no better than
// chance" when the honest answer is "undefined". The balance invariant refuses
// such a set before it can arise; this is the belt-and-braces guard.
func kappaFrom(observed, judgePositive, humanPositive float64) float64 {
	expected := judgePositive*humanPositive + (1-judgePositive)*(1-humanPositive)
	if 1-expected <= 0 {
		return math.NaN()
	}
	return (observed - expected) / (1 - expected)
}

// ratio guards the empty class: a set with no human-fail records has no
// specificity to report, and 0/0 rendered as 0 would read as a judge that
// never gets a negative right.
func ratio(num, den int) float64 {
	if den == 0 {
		return math.NaN()
	}
	return float64(num) / float64(den)
}

// KappaOver recomputes kappa over a subset of record indices.
//
// UNROUNDED, unlike every statistic Agree reports, and the split is
// deliberate: this is a resample statistic, not a reported one. Quantizing it
// to four places would quantize the bootstrap's own distribution, and the
// quantiles read off it are then a claim about the rounding rather than about
// the data. The bounds are rounded once, at the end, by roundInterval.
//
// This is the percentile bootstrap's draw. It takes
// INDICES rather than a filtered slice because the bootstrap draws with
// replacement: an index appearing twice must contribute twice to both
// marginals, which is the dependence between the judge's rate and the human's
// that a confusion-count interval would throw away.
func KappaOver(judge, human []bool, idx []int) float64 {
	if len(judge) != len(human) || len(idx) == 0 {
		return math.NaN()
	}
	var tp, fp, tn, fn int
	for _, i := range idx {
		if i < 0 || i >= len(judge) {
			return math.NaN()
		}
		switch {
		case judge[i] && human[i]:
			tp++
		case judge[i] && !human[i]:
			fp++
		case !judge[i] && human[i]:
			fn++
		default:
			tn++
		}
	}
	n := float64(len(idx))
	return kappaFrom(
		float64(tp+tn)/n,
		float64(tp+fp)/n,
		float64(tp+fn)/n,
	)
}

// InterHuman is the agreement of the labelers with each other on the same
// records: the ceiling.
//
// A judge cannot be held to an agreement its own labelers do not reach. With
// exactly two labelers this is their pairwise kappa; with more it is the mean
// pairwise kappa over every pair that labeled a shared record, which is the
// honest generalization at this set size (Fleiss' kappa needs every record
// labeled by the same number of raters, which a growing set does not have).
//
// Records a pair did not both label are skipped rather than imputed.
func InterHuman(records []Record) Agreement {
	labelers := map[string]struct{}{}
	for _, r := range records {
		for _, l := range r.Labels {
			labelers[l.LabelerID] = struct{}{}
		}
	}
	names := make([]string, 0, len(labelers))
	for name := range labelers {
		names = append(names, name)
	}
	if len(names) < 2 {
		return Agreement{Kappa: math.NaN()}
	}
	// Sorted so the mean is order-independent and the number is reproducible.
	sort.Strings(names)

	var sum float64
	var pairs int
	var pooled Agreement
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			a, b := labelVectors(records, names[i], names[j])
			if len(a) < 2 {
				continue
			}
			ag := Agree(a, b)
			if math.IsNaN(ag.Kappa) {
				continue
			}
			sum += ag.Kappa
			pairs++
			pooled.TP += ag.TP
			pooled.FP += ag.FP
			pooled.TN += ag.TN
			pooled.FN += ag.FN
			pooled.N += ag.N
		}
	}
	if pairs == 0 {
		return Agreement{Kappa: math.NaN()}
	}
	// The confusion counts are pooled across pairs so a reader can see the
	// volume behind the ceiling; the kappa is the MEAN of the pairwise
	// kappas, because pooling confusion counts across pairs and computing one
	// kappa mixes marginals from different people.
	pooled.Kappa = round4(sum / float64(pairs))
	if pooled.N > 0 {
		n := float64(pooled.N)
		pooled.Raw = round4(float64(pooled.TP+pooled.TN) / n)
		pooled.JudgePositiveRate = round4(float64(pooled.TP+pooled.FP) / n)
		pooled.HumanPositiveRate = round4(float64(pooled.TP+pooled.FN) / n)
		pooled.Sensitivity = round4(ratio(pooled.TP, pooled.TP+pooled.FN))
		pooled.Specificity = round4(ratio(pooled.TN, pooled.TN+pooled.FP))
	}
	return pooled
}

// labelVectors returns the two labelers' verdicts over the records both
// labeled.
func labelVectors(records []Record, a, b string) (av, bv []bool) {
	for _, r := range records {
		var got, want bool
		var haveA, haveB bool
		for _, l := range r.Labels {
			switch l.LabelerID {
			case a:
				got, haveA = l.Passed, true
			case b:
				want, haveB = l.Passed, true
			}
		}
		if haveA && haveB {
			av = append(av, got)
			bv = append(bv, want)
		}
	}
	return av, bv
}
