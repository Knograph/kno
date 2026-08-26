package interval

import "math"

// The Student-t quantile, and the two special functions it needs.
//
// A normal quantile is not a substitute, which is how this package shipped its
// first version: it stamped "t" onto an interval computed with z, and the
// result under-covered exactly where the t distribution differs most from the
// normal — 0.880 against a nominal 0.95 at five pairs, and 0.930 at fifteen,
// which is the sample size DESIGN.md's own worked example produces. A method
// label that names a distribution the code did not use is worse than an
// honest wide interval, because the label is what a reader checks.
//
// Inverted by bisection on the CDF rather than by an approximation formula.
// The CDF is monotone and the bracket is known, so bisection cannot diverge or
// land in a bad region the way a truncated series can at low degrees of
// freedom — which is precisely where this is used.

// studentTQuantile returns t such that P(T <= t) = p for df degrees of freedom.
//
// Returns NaN for a df or p outside the domain, which build() turns into a
// refusal rather than a bound.
func studentTQuantile(p, df float64) float64 {
	if df <= 0 || p <= 0 || p >= 1 || math.IsNaN(p) || math.IsNaN(df) {
		return math.NaN()
	}
	if p == 0.5 {
		return 0
	}

	// The normal quantile is the large-df limit and always inside the true
	// bracket on the correct side, so it is a sound starting width to expand
	// from. Doubling out is bounded: the t quantile is finite for df > 0.
	hi := math.Max(1, math.Abs(normalQuantile(p)))
	for range 60 {
		if studentTCDF(hi, df) >= p {
			break
		}
		hi *= 2
	}
	lo := -hi

	for range 200 {
		mid := (lo + hi) / 2
		if studentTCDF(mid, df) < p {
			lo = mid
		} else {
			hi = mid
		}
		if hi-lo < 1e-12*math.Max(1, math.Abs(hi)) {
			break
		}
	}
	return (lo + hi) / 2
}

// studentTCDF is P(T <= t), via the regularized incomplete beta.
func studentTCDF(t, df float64) float64 {
	x := df / (df + t*t)
	half := regularizedIncompleteBeta(x, df/2, 0.5) / 2
	if t > 0 {
		return 1 - half
	}
	return half
}

// regularizedIncompleteBeta is I_x(a, b).
//
// Lentz's continued fraction, with the standard symmetry swap that keeps the
// fraction in its fast-converging region — without it the recurrence needs
// far more iterations near x = 1 and loses precision where the t tails live.
func regularizedIncompleteBeta(x, a, b float64) float64 {
	switch {
	case x <= 0:
		return 0
	case x >= 1:
		return 1
	}

	lbeta := logGamma(a+b) - logGamma(a) - logGamma(b) +
		a*math.Log(x) + b*math.Log(1-x)
	front := math.Exp(lbeta)

	if x < (a+1)/(a+b+2) {
		return front * betaContinuedFraction(x, a, b) / a
	}
	return 1 - front*betaContinuedFraction(1-x, b, a)/b
}

// betaContinuedFraction evaluates the continued fraction for I_x(a, b).
func betaContinuedFraction(x, a, b float64) float64 {
	const tiny = 1e-30
	qab, qap, qam := a+b, a+1, a-1

	c := 1.0
	d := 1 - qab*x/qap
	if math.Abs(d) < tiny {
		d = tiny
	}
	d = 1 / d
	h := d

	for m := 1; m <= 300; m++ {
		fm := float64(m)
		m2 := 2 * fm

		// Even step.
		num := fm * (b - fm) * x / ((qam + m2) * (a + m2))
		d = 1 + num*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + num/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		h *= d * c

		// Odd step.
		num = -(a + fm) * (qab + fm) * x / ((a + m2) * (qap + m2))
		d = 1 + num*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + num/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		del := d * c
		h *= del

		if math.Abs(del-1) < 1e-14 {
			break
		}
	}
	return h
}

// logGamma is the natural log of the gamma function.
//
// Lanczos approximation, g = 7, which is accurate to about 1e-15 across the
// range this package uses — degrees of freedom from 1 upward.
func logGamma(x float64) float64 {
	g := []float64{
		0.99999999999980993, 676.5203681218851, -1259.1392167224028,
		771.32342877765313, -176.61502916214059, 12.507343278686905,
		-0.13857109526572012, 9.9843695780195716e-6, 1.5056327351493116e-7,
	}
	if x < 0.5 {
		// Reflection, so the series stays in its accurate half-plane.
		return math.Log(math.Pi/math.Sin(math.Pi*x)) - logGamma(1-x)
	}
	x--
	a := g[0]
	t := x + 7.5
	for i := 1; i < len(g); i++ {
		a += g[i] / (x + float64(i))
	}
	return 0.5*math.Log(2*math.Pi) + (x+0.5)*math.Log(t) - t + math.Log(a)
}
