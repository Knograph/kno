package main

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/knograph/kno/adapters/agent/pricing"
)

// verdict is a check's outcome. The order is deliberately fail > report >
// pass: any fail anywhere gates the run, and any report anywhere is the
// report-only surface the workflow consumes.
type verdict string

const (
	verdictPass   verdict = "PASS"
	verdictReport verdict = "REPORT"
	verdictFail   verdict = "FAIL"
)

// worse returns the stricter of two verdicts.
func worse(a, b verdict) verdict {
	if a == verdictFail || b == verdictFail {
		return verdictFail
	}
	if a == verdictReport || b == verdictReport {
		return verdictReport
	}
	return verdictPass
}

// finding is one line of evidence under a check. The Detail text is the
// report line exactly: prefixed with FAIL for gated failures and REPORT for
// report-only findings, never for anything else.
type finding struct {
	Check   int    `json:"check"`
	Name    string `json:"name"`
	Detail  string `json:"detail"`
	verdict verdict
}

// checkResult is one check's verdict plus the findings that produced it.
type checkResult struct {
	Number   int       `json:"number"`
	Name     string    `json:"name"`
	Verdict  verdict   `json:"verdict"`
	Summary  string    `json:"summary"`
	Findings []finding `json:"findings,omitempty"`
}

func newResult(n int, name string) *checkResult {
	return &checkResult{Number: n, Name: name, Verdict: verdictPass}
}

func (r *checkResult) add(v verdict, detail string) {
	r.Findings = append(r.Findings, finding{Check: r.Number, Name: r.Name, Detail: detail, verdict: v})
	r.Verdict = worse(r.Verdict, v)
}

func (r *checkResult) fail(detail string)   { r.add(verdictFail, detail) }
func (r *checkResult) report(detail string) { r.add(verdictReport, detail) }

// checkInput is everything the six checks read.
type checkInput struct {
	sources      map[string]sourceData
	tableVersion string
	maxAgeDays   int
}

// checkTableVersion is check 0 (gate): the table version must parse as
// YYYY-MM-DD and must not be older than the max-age. The version is the
// committed table's own date (pricing.Version) unless overridden. The age is
// calendar-day arithmetic: a version parsed as UTC midnight is expired the
// instant maxAgeDays full days have elapsed, so the boundary does not move
// with the run's time of day or a DST shift. A date in the future is a typo
// and fails — it must not read as fresh. A stale table is the one failure
// mode the detector must never sleep through, so this gate has no
// report-only path.
func checkTableVersion(in checkInput) checkResult {
	r := newResult(0, "table version")
	t, err := time.Parse("2006-01-02", in.tableVersion)
	if err != nil {
		// Unreachable: the config is validated before checks run. The guard
		// stays because a future caller must not hand checks a bad version.
		r.fail(fmt.Sprintf("table version %q is not a YYYY-MM-DD date", in.tableVersion))
		return *r
	}
	now := time.Now()
	if now.Before(t) {
		r.fail(fmt.Sprintf("table version %s is in the future; a date typo must not read as fresh", in.tableVersion))
		return *r
	}
	days := int(now.Sub(t).Hours() / 24)
	if t.AddDate(0, 0, in.maxAgeDays).Before(now) {
		r.fail(fmt.Sprintf("table dated %s is %d days old, older than the %d-day max-age (docs/debt.md#40); refresh the table",
			in.tableVersion, days, in.maxAgeDays))
	} else {
		r.Summary = fmt.Sprintf("table dated %s, %d days old, within the %d-day max-age",
			in.tableVersion, days, in.maxAgeDays)
	}
	return *r
}

// rowsOf returns a source's parsed rows and whether they exist. A source
// with any error — including the selection and row errors the anthropic
// checks judge — has no rows.
func rowsOf(sources map[string]sourceData, name string) ([]row, bool) {
	sd, ok := sources[name]
	if !ok || sd.err != nil {
		return nil, false
	}
	return sd.rows, true
}

// checkAgreement is check 1 (report): OpenRouter's prices against the
// price-of-record pages, per model. A disagreement is REPORTED with both
// values and both URLs and never gates. Suppressed models are skipped — but
// a suppression whose sources have converged is dead and FAILS, because a
// stale suppression is how a divergence hides.
func checkAgreement(in checkInput) checkResult {
	r := newResult(1, "source agreement")
	orRows, orOK := rowsOf(in.sources, "openrouter")
	if !orOK {
		r.report("openrouter has no data; agreement not checked")
		return *r
	}
	checked := 0
	for _, scheme := range []string{"anthropic", "openai"} {
		pageRows, ok := rowsOf(in.sources, scheme)
		if !ok {
			r.report(fmt.Sprintf("%s has no data; %s models not checked against openrouter", scheme, scheme))
			continue
		}
		for _, pr := range pageRows {
			or := findMatching(orRows, scheme, pr.canonical)
			if or == nil {
				continue // model not on OpenRouter; nothing to agree with
			}
			checked++
			if sup := matchSuppression(suppressions, scheme, pr.model, pr.canonical); sup != nil {
				if agree(pr, *or) {
					r.fail(fmt.Sprintf("suppression for %s/%s is dead: the sources converged at %s; remove it from the suppression list",
						scheme, pr.canonical, formatUSDPerMTok(pr.input)))
				}
				continue
			}
			for _, d := range compareWithOpenRouter(scheme, pr, *or) {
				r.report(d)
			}
		}
	}
	if checked == 0 {
		r.report("no page model matched an openrouter model; agreement not checked")
	}
	return *r
}

// findMatching locates the OpenRouter row for a page model.
func findMatching(orRows []row, scheme, canonical string) *row {
	for i := range orRows {
		if orRows[i].scheme == scheme && orRows[i].canonical == canonical {
			return &orRows[i]
		}
	}
	return nil
}

// agree reports whether two rows agree on the dimensions both publish:
// input and output. The suppressed model's cached-read and write rates are
// not part of the suppression's contract.
func agree(a, b row) bool {
	if a.input == nil || b.input == nil || a.output == nil || b.output == nil {
		return false
	}
	return a.input.Cmp(b.input) == 0 && a.output.Cmp(b.output) == 0
}

// compareWithOpenRouter lists the disagreements between a page row and its
// OpenRouter counterpart, one line per dimension, each carrying both values
// and both URLs.
func compareWithOpenRouter(scheme string, pr, or row) []string {
	var out []string
	if pr.input != nil && or.input != nil && pr.input.Cmp(or.input) != 0 {
		out = append(out, fmt.Sprintf("model %s input: page %s vs openrouter %s (page %s, openrouter %s)",
			pr.canonical, formatUSDPerMTok(pr.input), formatUSDPerMTok(or.input),
			sourceURL(scheme), sourceURL("openrouter")))
	}
	if pr.output != nil && or.output != nil && pr.output.Cmp(or.output) != 0 {
		out = append(out, fmt.Sprintf("model %s output: page %s vs openrouter %s (page %s, openrouter %s)",
			pr.canonical, formatUSDPerMTok(pr.output), formatUSDPerMTok(or.output),
			sourceURL(scheme), sourceURL("openrouter")))
	}
	return out
}

// checkSelection is check 2 (gate): the anthropic price table must be
// findable by the committed header literal, and by exactly one table. Zero
// or two matches mean the page was restructured and every price on it is
// suspect: that is a FAIL, not a report.
func checkSelection(in checkInput) checkResult {
	r := newResult(2, "table selection")
	sd, ok := in.sources["anthropic"]
	if !ok || sd.shapeBroken() {
		r.report("anthropic has no data; table selection not checked")
		return *r
	}
	if errors.Is(sd.err, errSelect) {
		r.fail(sd.err.Error())
	}
	return *r
}

// checkRatios is check 3 (gate): the pages' own published rates must stand
// in the vendors' fixed ratios — cache read at 0.10x input for both
// providers, and on the anthropic table the 5-minute cache write at 1.25x
// input and the 1-hour write at 2.00x. The ratios are exact rational
// comparisons; a page that breaks one is a changed layout or a fat-fingered
// price and fails regardless of magnitude. OpenAI publishes no cache-write
// rate, so its rows are only checked for the read ratio.
func checkRatios(in checkInput) checkResult {
	r := newResult(3, "cache ratios")
	if sd, ok := in.sources["anthropic"]; ok && !sd.shapeBroken() && errors.Is(sd.err, errRow) {
		r.fail(fmt.Sprintf("price row malformed: %s", sd.err.Error()))
	}
	for _, scheme := range []string{"anthropic", "openai"} {
		rows, ok := rowsOf(in.sources, scheme)
		if !ok {
			continue
		}
		for i := range rows {
			checkRowRatios(r, &rows[i], scheme)
		}
	}
	return *r
}

// checkRowRatios applies the ratio obligations to one row.
func checkRowRatios(r *checkResult, rw *row, scheme string) {
	want := func(dim string, got, base, ratio *big.Rat) {
		if got == nil || base == nil {
			return
		}
		if got.Cmp(ratMul(base, ratio)) != 0 {
			r.fail(fmt.Sprintf("model %s: %s %s is not %s of input %s",
				rw.canonical, dim, formatUSDPerMTok(got), ratio.FloatString(2)+"x", formatUSDPerMTok(base)))
		}
	}
	want("cache read", rw.cachedRead, rw.input, big.NewRat(10, 100))
	if scheme == "anthropic" {
		want("5m cache write", rw.cacheWrite5m, rw.input, big.NewRat(125, 100))
		want("1h cache write", rw.cacheWrite1h, rw.input, big.NewRat(200, 100))
	}
}

// coveredByTable reports whether the table prices the model, using Lookup as
// the ONLY predicate — the exact-key rule lives in pricing, not here. The
// pages spell anthropic names in prose where the table keys canonically, so
// both spellings are tried; which spelling happens to hit is Lookup's
// business.
func coveredByTable(scheme string, spellings ...string) bool {
	for _, s := range spellings {
		if _, ok := pricing.Lookup(scheme, s); ok {
			return true
		}
	}
	return false
}

// prefixKey finds the longest table key that prefixes the id. This is check
// 5's collision predicate — "the id lives under a priced namespace" — which
// is deliberately NOT Lookup's covered-predicate: a collider is exactly a
// model Lookup refuses. pricing does not export its prefix search, so this
// small loop is implemented here.
func prefixKey(scheme, id string) (string, bool) {
	best := ""
	for _, key := range pricing.Models(scheme) {
		if strings.HasPrefix(id, key) && len(key) > len(best) {
			best = key
		}
	}
	return best, best != ""
}

// checkDiscovery is check 4 (report): models on the price-of-record pages
// the table does not price. Deliberate exclusions are silent; everything
// else is REPORTED — a new model on the page is how the table grows, and the
// report is the natural input for that review. An exclusion whose model the
// table now prices is dead and FAILS.
func checkDiscovery(in checkInput) checkResult {
	r := newResult(4, "discovery")
	seen := false
	for _, scheme := range []string{"anthropic", "openai"} {
		rows, ok := rowsOf(in.sources, scheme)
		if !ok {
			r.report(fmt.Sprintf("%s has no data; discovery not checked for it", scheme))
			continue
		}
		for i := range rows {
			rw := &rows[i]
			seen = true
			if matchExclusion(deliberateExclusions, rw.scheme, rw.model, rw.canonical) != nil {
				continue
			}
			if coveredByTable(rw.scheme, rw.model, rw.canonical) {
				continue
			}
			if matchExclusion(pendingExclusions, rw.scheme, rw.model, rw.canonical) != nil {
				continue // pending exclusions are check 5's to report
			}
			r.report(fmt.Sprintf("model %s (%s) is on the price-of-record page but the table does not price it; add a row or an exclusion",
				rw.canonical, scheme))
		}
	}
	if !seen {
		r.report("no price-of-record data; discovery not checked")
	}
	// A deliberate exclusion whose model the table now prices is dead,
	// whether or not the page still lists the model.
	for i := range deliberateExclusions {
		e := &deliberateExclusions[i]
		if coveredByTable(e.scheme, e.model) {
			r.fail(fmt.Sprintf("deliberate exclusion for %s/%s is dead: the table now prices it; remove the exclusion",
				e.scheme, e.model))
		}
	}
	return *r
}

// checkPrefixCollisions is check 5 (gate with a pending exception): a model
// whose id lives under a priced table key but that Lookup refuses — a
// variant with its own price — is a defect: the engine refuses it under a
// cost cap while the table looks covered. The committed pending exclusions
// are the exception, and they are REPORTED every run, never gated, until
// their rows land. Lifecycle: an exclusion (pending or deliberate) whose
// model the table now prices is dead and FAILS.
func checkPrefixCollisions(in checkInput) checkResult {
	r := newResult(5, "prefix collisions")
	for _, srcName := range []string{"openrouter", "anthropic", "openai"} {
		rows, ok := rowsOf(in.sources, srcName)
		if !ok {
			continue
		}
		for i := range rows {
			rw := &rows[i]
			if rw.scheme == "" {
				continue
			}
			if coveredByTable(rw.scheme, rw.model, rw.canonical) {
				continue
			}
			base, ok := prefixKey(rw.scheme, rw.model)
			if !ok {
				base, ok = prefixKey(rw.scheme, rw.canonical)
				if !ok {
					continue
				}
			}
			if matchExclusion(deliberateExclusions, rw.scheme, rw.model, rw.canonical) != nil {
				continue
			}
			if matchExclusion(pendingExclusions, rw.scheme, rw.model, rw.canonical) != nil {
				continue
			}
			r.report(fmt.Sprintf("model %s/%s prefix-matches priced %s but Lookup refuses it; add a row or an exclusion",
				rw.scheme, rw.canonical, base))
		}
	}
	for i := range pendingExclusions {
		e := &pendingExclusions[i]
		if coveredByTable(e.scheme, e.model) {
			r.fail(fmt.Sprintf("pending exclusion %s/%s is now priced: the docs/debt.md#46 row exists; remove the exclusion",
				e.scheme, e.model))
			continue
		}
		r.report(fmt.Sprintf("model %s/%s carries its own price; a row is owed before 0.1.0 (docs/debt.md#46)",
			e.scheme, e.model))
	}
	return *r
}
