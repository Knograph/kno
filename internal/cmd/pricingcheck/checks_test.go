package main

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"
)

func rat(micros int64) *big.Rat { return big.NewRat(micros, 1) }

// mkRow builds a row in micro-USD per MTok. nil rate means the source does
// not publish that dimension.
func mkRow(source, scheme, model string, input, cachedRead, w5m, w1h, output *big.Rat) row {
	return row{
		source: source, scheme: scheme, model: model, canonical: canonicalModel(model),
		input: input, cachedRead: cachedRead, cacheWrite5m: w5m, cacheWrite1h: w1h, output: output,
	}
}

func mkInput(sources map[string]sourceData) checkInput {
	return checkInput{sources: sources, tableVersion: "2026-08-21", maxAgeDays: 90}
}

func sourceOf(rows []row) sourceData { return sourceData{name: "x", rows: rows} }

func findingsOf(r checkResult) []string {
	var out []string
	for _, f := range r.Findings {
		out = append(out, fmt.Sprintf("%s|%s", f.verdict, f.Detail))
	}
	return out
}

func wantFinding(t *testing.T, r checkResult, v verdict, contains string) {
	t.Helper()
	for _, f := range r.Findings {
		if f.verdict == v && strings.Contains(f.Detail, contains) {
			return
		}
	}
	t.Errorf("check %d: no %s finding containing %q; findings: %v", r.Number, v, contains, findingsOf(r))
}

func wantNoFinding(t *testing.T, r checkResult, v verdict) {
	t.Helper()
	for _, f := range r.Findings {
		if f.verdict == v {
			t.Errorf("check %d: unexpected %s finding: %s", r.Number, v, f.Detail)
		}
	}
}

// --- check 0 ---

func TestCheckTableVersionBoundary(t *testing.T) {
	t.Parallel()
	// The version parses as UTC midnight, so dates are constructed from UTC:
	// local-time AddDate is DST-flaky and would shift the boundary by an hour.
	// Expiry is calendar-day arithmetic: the version fails once
	// version+maxAgeDays is in the past, so a table dated 89 days ago is still
	// within the 90-day window at any run time and 90 days ago is expired.
	nowUTC := time.Now().UTC()
	day89 := nowUTC.AddDate(0, 0, -89).Format("2006-01-02")
	day90 := nowUTC.AddDate(0, 0, -90).Format("2006-01-02")
	over91 := nowUTC.AddDate(0, 0, -91).Format("2006-01-02")
	future := nowUTC.AddDate(0, 0, 7).Format("2006-01-02")

	if r := checkTableVersion(checkInput{tableVersion: day89, maxAgeDays: 90}); r.Verdict != verdictPass {
		t.Errorf("89 days must pass, got %s", r.Verdict)
	}
	if r := checkTableVersion(checkInput{tableVersion: day90, maxAgeDays: 90}); r.Verdict != verdictFail {
		t.Errorf("90 days must fail, got %s", r.Verdict)
	}
	if r := checkTableVersion(checkInput{tableVersion: over91, maxAgeDays: 90}); r.Verdict != verdictFail {
		t.Errorf("91 days must fail, got %s", r.Verdict)
	}
	if r := checkTableVersion(checkInput{tableVersion: future, maxAgeDays: 90}); r.Verdict != verdictFail {
		t.Errorf("future version must fail, got %s", r.Verdict)
	}
	if r := checkTableVersion(checkInput{tableVersion: "not-a-date", maxAgeDays: 90}); r.Verdict != verdictFail {
		t.Errorf("unparseable version must fail, got %s", r.Verdict)
	}
}

func TestCheckTableVersionFailDetail(t *testing.T) {
	t.Parallel()
	old := time.Now().UTC().AddDate(0, 0, -200).Format("2006-01-02")
	r := checkTableVersion(checkInput{tableVersion: old, maxAgeDays: 90})
	wantFinding(t, r, verdictFail, "older than the 90-day max-age (docs/debt.md#40)")
}

func TestCheckTableVersionFutureDetail(t *testing.T) {
	t.Parallel()
	future := time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")
	r := checkTableVersion(checkInput{tableVersion: future, maxAgeDays: 90})
	wantFinding(t, r, verdictFail, "is in the future; a date typo must not read as fresh")
}

// --- check 1 ---

var opusPage = mkRow("anthropic", "anthropic", "claude-opus-5", rat(5_000_000), rat(500_000), rat(6_250_000), rat(10_000_000), rat(25_000_000))

func TestCheckAgreementReport(t *testing.T) {
	t.Parallel()
	or := mkRow("openrouter", "anthropic", "claude-opus-5", rat(5_100_000), nil, nil, nil, rat(25_500_000))
	r := checkAgreement(mkInput(map[string]sourceData{
		"anthropic":  sourceOf([]row{opusPage}),
		"openai":     {},
		"openrouter": sourceOf([]row{or}),
	}))
	wantFinding(t, r, verdictReport, "input: page $5.00 vs openrouter $5.10")
	wantFinding(t, r, verdictReport, "output: page $25.00 vs openrouter $25.50")
	wantFinding(t, r, verdictReport, "platform.claude.com")
	wantFinding(t, r, verdictReport, "openrouter.ai")
	wantNoFinding(t, r, verdictFail)
}

func TestCheckAgreementAgrees(t *testing.T) {
	t.Parallel()
	or := mkRow("openrouter", "anthropic", "claude-opus-5", rat(5_000_000), nil, nil, nil, rat(25_000_000))
	r := checkAgreement(mkInput(map[string]sourceData{
		"anthropic":  sourceOf([]row{opusPage}),
		"openai":     {},
		"openrouter": sourceOf([]row{or}),
	}))
	if len(r.Findings) != 0 {
		t.Errorf("agreeing rows must have no findings: %v", findingsOf(r))
	}
}

// TestCheckAgreementSuppressionLive uses the committed suppression: the
// openrouter price for gpt-5.6-sol is half the page's, and the suppressed
// divergence must stay silent.
func TestCheckAgreementSuppressionLive(t *testing.T) {
	t.Parallel()
	solPage := mkRow("openai", "openai", "gpt-5.6-sol", rat(4_000_000), rat(400_000), nil, nil, rat(20_000_000))
	solOR := mkRow("openrouter", "openai", "gpt-5.6-sol", rat(2_000_000), nil, nil, nil, rat(10_000_000))
	r := checkAgreement(mkInput(map[string]sourceData{
		"anthropic":  {},
		"openai":     sourceOf([]row{solPage}),
		"openrouter": sourceOf([]row{solOR}),
	}))
	if len(r.Findings) != 0 {
		t.Errorf("suppressed divergence must be silent: %v", findingsOf(r))
	}
}

func TestCheckAgreementDeadSuppression(t *testing.T) {
	t.Parallel()
	solPage := mkRow("openai", "openai", "gpt-5.6-sol", rat(4_000_000), rat(400_000), nil, nil, rat(20_000_000))
	solOR := mkRow("openrouter", "openai", "gpt-5.6-sol", rat(4_000_000), nil, nil, nil, rat(20_000_000))
	r := checkAgreement(mkInput(map[string]sourceData{
		"anthropic":  {},
		"openai":     sourceOf([]row{solPage}),
		"openrouter": sourceOf([]row{solOR}),
	}))
	wantFinding(t, r, verdictFail, "suppression for openai/gpt-5-6-sol is dead")
}

func TestCheckAgreementNoOpenRouter(t *testing.T) {
	t.Parallel()
	// An unusable openrouter source — in production a fetch failure, in the
	// fixture path a wrong JSON envelope — is "no data", reported not gated.
	// A healthy-but-empty openrouter is unreachable: the parser rejects an
	// empty envelope as errShape.
	r := checkAgreement(mkInput(map[string]sourceData{
		"anthropic":  sourceOf([]row{opusPage}),
		"openai":     {},
		"openrouter": {name: "openrouter", err: fmt.Errorf("%w: fetch failed", errShape)},
	}))
	wantFinding(t, r, verdictReport, "openrouter has no data")
	wantNoFinding(t, r, verdictFail)
}

// --- check 2 ---

func TestCheckSelectionFailAmbiguous(t *testing.T) {
	t.Parallel()
	r := checkSelection(mkInput(map[string]sourceData{
		"anthropic": {name: "anthropic", err: fmt.Errorf("%w: 2 tables match the committed header literal", errSelect)},
	}))
	wantFinding(t, r, verdictFail, "2 tables match the committed header literal")
}

func TestCheckSelectionFailNone(t *testing.T) {
	t.Parallel()
	r := checkSelection(mkInput(map[string]sourceData{
		"anthropic": {name: "anthropic", err: fmt.Errorf("%w: no table has the committed header", errSelect)},
	}))
	wantFinding(t, r, verdictFail, "no table has the committed header")
}

func TestCheckSelectionRowErrorDoesNotFailSelection(t *testing.T) {
	t.Parallel()
	r := checkSelection(mkInput(map[string]sourceData{
		"anthropic": {name: "anthropic", err: fmt.Errorf("%w: row 2 has 5 cells", errRow)},
	}))
	if r.Verdict != verdictPass {
		t.Errorf("a row error is check 3's, not check 2's: %s", r.Verdict)
	}
}

func TestCheckSelectionNoData(t *testing.T) {
	t.Parallel()
	r := checkSelection(mkInput(map[string]sourceData{
		"anthropic": {name: "anthropic", err: fmt.Errorf("%w: envelope wrong", errShape)},
	}))
	wantFinding(t, r, verdictReport, "anthropic has no data")
	wantNoFinding(t, r, verdictFail)
}

// --- check 3 ---

var (
	goodAnthropicRow = mkRow("anthropic", "anthropic", "claude-opus-5", rat(5_000_000), rat(500_000), rat(6_250_000), rat(10_000_000), rat(25_000_000))
	goodOpenAIRow    = mkRow("openai", "openai", "gpt-5.6-sol", rat(4_000_000), rat(400_000), nil, nil, rat(20_000_000))
)

func TestCheckRatiosPass(t *testing.T) {
	t.Parallel()
	r := checkRatios(mkInput(map[string]sourceData{
		"anthropic": sourceOf([]row{goodAnthropicRow}),
		"openai":    sourceOf([]row{goodOpenAIRow}),
	}))
	if len(r.Findings) != 0 {
		t.Errorf("correct ratios must pass: %v", findingsOf(r))
	}
}

func TestCheckRatiosFailCachedRead(t *testing.T) {
	t.Parallel()
	bad := goodOpenAIRow
	bad.cachedRead = rat(800_000) // 0.20x, not 0.10x
	r := checkRatios(mkInput(map[string]sourceData{
		"openai": sourceOf([]row{bad}),
	}))
	wantFinding(t, r, verdictFail, "model gpt-5-6-sol: cache read $0.80 is not 0.10x of input $4.00")
}

func TestCheckRatiosFailCacheWrites(t *testing.T) {
	t.Parallel()
	bad := goodAnthropicRow
	bad.cacheWrite5m = rat(10_000_000) // 2.00x, not 1.25x
	bad.cacheWrite1h = rat(7_500_000)  // 1.50x, not 2.00x
	r := checkRatios(mkInput(map[string]sourceData{
		"anthropic": sourceOf([]row{bad}),
	}))
	wantFinding(t, r, verdictFail, "5m cache write")
	wantFinding(t, r, verdictFail, "1h cache write")
}

func TestCheckRatiosFailMalformedRow(t *testing.T) {
	t.Parallel()
	r := checkRatios(mkInput(map[string]sourceData{
		"anthropic": {name: "anthropic", err: fmt.Errorf("%w: row 2 has 5 cells, want 6", errRow)},
	}))
	wantFinding(t, r, verdictFail, "price row malformed")
}

// --- check 4 ---

func TestCheckDiscoveryReportUnpriced(t *testing.T) {
	t.Parallel()
	unpriced := mkRow("anthropic", "anthropic", "claude-mythos-4", rat(5_000_000), rat(500_000), rat(6_250_000), rat(10_000_000), rat(25_000_000))
	r := checkDiscovery(mkInput(map[string]sourceData{
		"anthropic": sourceOf([]row{unpriced}),
		"openai":    {},
	}))
	wantFinding(t, r, verdictReport, "model claude-mythos-4 (anthropic) is on the price-of-record page but the table does not price it")
	wantNoFinding(t, r, verdictFail)
}

func TestCheckDiscoveryDeliberateSilent(t *testing.T) {
	t.Parallel()
	mythos := mkRow("anthropic", "anthropic", "claude-mythos-5", rat(10_000_000), rat(1_000_000), rat(12_500_000), rat(20_000_000), rat(50_000_000))
	priced := goodAnthropicRow
	r := checkDiscovery(mkInput(map[string]sourceData{
		"anthropic": sourceOf([]row{mythos, priced}),
		"openai":    {},
	}))
	if len(r.Findings) != 0 {
		t.Errorf("deliberately excluded and priced models must be silent: %v", findingsOf(r))
	}
}

func TestCheckDiscoveryPendingSilent(t *testing.T) {
	t.Parallel()
	fast := mkRow("anthropic", "anthropic", "claude-opus-5-fast", rat(10_000_000), rat(1_000_000), rat(12_500_000), rat(20_000_000), rat(50_000_000))
	r := checkDiscovery(mkInput(map[string]sourceData{
		"anthropic": sourceOf([]row{fast}),
		"openai":    {},
	}))
	if len(r.Findings) != 0 {
		t.Errorf("pending exclusions are check 5's to report: %v", findingsOf(r))
	}
}

// TestCheckDiscoveryDeadDeliberate is serial: it temporarily prices a
// deliberately excluded model by pointing the deadness check at one that IS
// in the table.
func TestCheckDiscoveryDeadDeliberate(t *testing.T) {
	orig := deliberateExclusions
	deliberateExclusions = append(append([]exclusion{}, orig...), exclusion{scheme: "anthropic", model: "claude-opus-5", reason: "test"})
	defer func() { deliberateExclusions = orig }()

	prose := mkRow("anthropic", "anthropic", "Claude Opus 5", rat(5_000_000), rat(500_000), rat(6_250_000), rat(10_000_000), rat(25_000_000))
	r := checkDiscovery(mkInput(map[string]sourceData{
		"anthropic": sourceOf([]row{prose}),
		"openai":    {},
	}))
	wantFinding(t, r, verdictFail, "deliberate exclusion for anthropic/claude-opus-5 is dead")
}

func TestCheckDiscoveryNoData(t *testing.T) {
	t.Parallel()
	r := checkDiscovery(mkInput(map[string]sourceData{"openai": {name: "openai", err: errors.New("fetch failed")}}))
	wantFinding(t, r, verdictReport, "no price-of-record data")
}

// --- check 5 ---

func TestCheckPrefixCollisionsNoPendingExclusions(t *testing.T) {
	t.Parallel()
	// docs/debt.md#46 repaid: the pending list is gone (fast variants became
	// rows; batch and no-price-of-record variants became deliberate
	// exclusions), so an empty source set produces no findings and no gate —
	// the old contract reported every pending exclusion on every run, which
	// would now be permanent noise.
	r := checkPrefixCollisions(mkInput(map[string]sourceData{}))
	if r.Verdict != verdictPass {
		t.Errorf("no pending exclusions remain; got %s, want PASS", r.Verdict)
	}
	if len(r.Findings) != 0 {
		t.Errorf("got %d findings, want none", len(r.Findings))
	}
}

// TestCheckPrefixCollisionsNewCollider uses a model on NO committed list —
// gpt-5.6-sol-pro itself is a committed deliberate exclusion, so a fresh
// suffix on it must still be reported as a collider.
func TestCheckPrefixCollisionsNewCollider(t *testing.T) {
	t.Parallel()
	collider := mkRow("openrouter", "openai", "gpt-5.6-sol-pro-max", rat(5_000_000), nil, nil, nil, rat(25_000_000))
	r := checkPrefixCollisions(mkInput(map[string]sourceData{
		"openrouter": sourceOf([]row{collider}),
	}))
	wantFinding(t, r, verdictReport, "model openai/gpt-5-6-sol-pro-max prefix-matches priced gpt-5.6-sol but Lookup refuses it")
	found := 0
	for _, f := range r.Findings {
		if strings.Contains(f.Detail, "gpt-5-6-sol-pro-max") {
			found++
		}
	}
	if found != 1 {
		t.Errorf("collider reported %d times, want exactly 1", found)
	}
}

func TestCheckPrefixCollisionsCoveredSilent(t *testing.T) {
	t.Parallel()
	// A covered model (the table prices gpt-5.6-sol) is not a collider:
	// no collider line and no gate.
	priced := mkRow("openrouter", "openai", "gpt-5.6-sol", rat(2_000_000), nil, nil, nil, rat(10_000_000))
	r := checkPrefixCollisions(mkInput(map[string]sourceData{
		"openrouter": sourceOf([]row{priced}),
	}))
	wantNoCollider(t, r)
	wantNoFinding(t, r, verdictFail)
}

func TestCheckPrefixCollisionsNoCollisionSilent(t *testing.T) {
	t.Parallel()
	// A model under no priced key is not a collider.
	unrelated := mkRow("openrouter", "anthropic", "claude-mythos-4", rat(5_000_000), nil, nil, nil, rat(25_000_000))
	r := checkPrefixCollisions(mkInput(map[string]sourceData{
		"openrouter": sourceOf([]row{unrelated}),
	}))
	wantNoCollider(t, r)
	wantNoFinding(t, r, verdictFail)
}

// wantNoCollider asserts no finding reports a fresh prefix collision.
func wantNoCollider(t *testing.T, r checkResult) {
	t.Helper()
	for _, f := range r.Findings {
		if f.verdict == verdictReport && strings.Contains(f.Detail, "prefix-matches priced") {
			t.Errorf("check %d: unexpected collider finding: %s", r.Number, f.Detail)
		}
	}
}
