package judge

import (
	"math"
	"sort"
)

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
	return math.Abs(a.Sensitivity - a.Specificity)
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
	a.Raw = float64(a.TP+a.TN) / n
	a.JudgePositiveRate = float64(a.TP+a.FP) / n
	a.HumanPositiveRate = float64(a.TP+a.FN) / n
	a.Sensitivity = ratio(a.TP, a.TP+a.FN)
	a.Specificity = ratio(a.TN, a.TN+a.FP)
	a.Kappa = kappaFrom(a.Raw, a.JudgePositiveRate, a.HumanPositiveRate)
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
// This is the resample statistic the percentile bootstrap draws on. It takes
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
	pooled.Kappa = sum / float64(pairs)
	if pooled.N > 0 {
		n := float64(pooled.N)
		pooled.Raw = float64(pooled.TP+pooled.TN) / n
		pooled.JudgePositiveRate = float64(pooled.TP+pooled.FP) / n
		pooled.HumanPositiveRate = float64(pooled.TP+pooled.FN) / n
		pooled.Sensitivity = ratio(pooled.TP, pooled.TP+pooled.FN)
		pooled.Specificity = ratio(pooled.TN, pooled.TN+pooled.FP)
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
