package bedrock

// This file is the docs/debt.md#41(d) repayment on the TEST side: the
// regional +10% multiplier is pinned in all three places the guard touches —
// the per-Case reservation (Estimate), the consent quote (WorstCase), and the
// settlement (Settle, before Guard.Settle). The numbers are pinned exactly,
// not recomputed through the same code path they test, so a drift in the
// multiplier is a failing constant rather than a changing expectation.
//
// All figures use the table's sonnet-4-5 row, in the table's own slot order:
// input $3/MTok, output $15/MTok, and the two cache slots as the table
// carries them. The prompt terms are sized so every product divides evenly,
// which keeps the pins readable.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/agent/pricing"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// runFatal reads the escalation the way core does — a structural assertion,
// so nothing crosses the package boundary in either direction.
func runFatal(err error) bool {
	var rf interface{ RunFatal() bool }
	return errors.As(err, &rf) && rf.RunFatal()
}

// TestEstimateCarriesTheRegionalMultiplier pins Estimate's exact figure:
//
//	"hi" is 2 bytes → 1 token → 2 tokens with the 1.5 margin
//	input:  perMTok(3_000_000, 2) = 6
//	output: perMTok(15_000_000, 1000) = 15_000
//	base:   15_006, regional 110%: +ceil(1500.6) = 15_006 + 1_501 = 16_507
func TestEstimateCarriesTheRegionalMultiplier(t *testing.T) {
	t.Parallel()

	a := &Agent{
		opts: Options{Model: testModel, MaxOutputTokens: 1000},
	}
	est, err := a.Estimate(t.Context(), aCase("c1", "hi"))
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.Calls != 1 {
		t.Errorf("Calls = %d, want 1", est.Calls)
	}
	if est.CostUSDMicros != 16_507 {
		t.Errorf("CostUSDMicros = %d, want 16_507 (the 10%% multiplier on 15_006)", est.CostUSDMicros)
	}
	if est.Tokens != 1_002 {
		t.Errorf("Tokens = %d, want 1_002", est.Tokens)
	}
}

// TestWorstCaseCarriesTheRegionalMultiplier pins the consent quote:
//
//	system "s" + 400_000 bytes of input = 400_001 bytes
//	→ 200_001 tokens → 300_002 with the 1.5 margin
//	input:  perMTok(3_000_000, 300_002) = 900_006
//	output: perMTok(15_000_000, 1000) = 15_000
//	base:   915_006, regional 110%: +ceil(91_500.6) = 915_006 + 91_501 = 1_006_507
func TestWorstCaseCarriesTheRegionalMultiplier(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1000, System: "s"},
		func(http.ResponseWriter, *http.Request) {})
	worst := a.WorstCase()
	if worst.CostUSDMicros != 1_006_507 {
		t.Errorf("WorstCase.CostUSDMicros = %d, want 1_006_507 (the 10%% multiplier on 915_006)", worst.CostUSDMicros)
	}
	if worst.Calls != 1 {
		t.Errorf("WorstCase.Calls = %d, want 1", worst.Calls)
	}
}

// TestSettleCarriesTheRegionalMultiplier pins the settlement:
//
//	usage: 1000 fresh + 100 cache writes + 500 cache reads + 200 output
//	fresh:   micros(3_000_000, 1000) = 3_000
//	cacheW:  micros(3_750_000, 100) = 375
//	cacheR:  micros(300_000, 500) = 150
//	output:  micros(15_000_000, 200) = 3_000
//	base:    6_525, regional 110%: +ceil(652.5) = 6_525 + 653 = 7_178
//
// The cache slots are pinned exactly as the table carries them — the rate
// whose slot is "cached read" and the rate whose slot is "cache write" are
// what the settlement charges for reads and writes, whatever the vendor's
// published sheet says. A drift in either slot is a failing constant here.
func TestSettleCarriesTheRegionalMultiplier(t *testing.T) {
	t.Parallel()

	a := &Agent{
		opts: Options{Model: testModel, MaxOutputTokens: 1000},
	}
	in, cw, cr, out := int64(1000), int64(100), int64(500), int64(200)
	u := &usage{
		InputTokens:              &in,
		CacheCreationInputTokens: &cw,
		CacheReadInputTokens:     &cr,
		OutputTokens:             &out,
	}

	resp := &core.Response{}
	a.settle(resp, aCase("c1", "hi"), &converseResponse{Usage: u}, "hello")
	if resp.CostUsdMicros != 7_178 {
		t.Errorf("settled cost = %d, want 7_178 (the 10%% multiplier on 6_525)", resp.CostUsdMicros)
	}
	if resp.PromptTokens != 1_600 {
		t.Errorf("PromptTokens = %d, want 1_600", resp.PromptTokens)
	}
	if resp.CompletionTokens != 200 {
		t.Errorf("CompletionTokens = %d, want 200", resp.CompletionTokens)
	}
	if resp.CachedTokens != 500 {
		t.Errorf("CachedTokens = %d, want 500", resp.CachedTokens)
	}
	if resp.UsageEstimated {
		t.Error("a measured usage block must not be flagged as an estimate")
	}
}

// TestEstimateRefusesAProfileIDAndNamesTheRegionClass pins the refusal for a
// cross-region inference profile id: no multiplier, no row, and a fix that
// names the class instead of chasing a typo.
func TestEstimateRefusesAProfileIDAndNamesTheRegionClass(t *testing.T) {
	t.Parallel()

	a := &Agent{
		opts: Options{Model: "us.anthropic.claude-3-5-sonnet-20241022-v2:0", MaxOutputTokens: 1000},
	}
	_, err := a.Estimate(t.Context(), aCase("c1", "hi"))
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	}
	if !runFatal(err) {
		t.Errorf("an unpriced model is run-fatal: it is a property of the model, and the model does not change mid-run")
	}
	msg := err.Error()
	for _, want := range []string{"us.-prefixed", "destination region", "refused until one exists"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not name %q", msg, want)
		}
	}
}

// TestAPriceOverrideIsMultipliedToo pins that a caller's own price is no more
// exempt from the add-on than the table's: the endpoint is regional either
// way.
//
//	price: input $1/MTok, output $2/MTok; prompt "hi", ceiling 100
//	input:  perMTok(1_000_000, 2) = 2
//	output: perMTok(2_000_000, 100) = 200
//	base:   202, regional 110%: +ceil(20.2) = 202 + 21 = 223
func TestAPriceOverrideIsMultipliedToo(t *testing.T) {
	t.Parallel()

	in, out := int64(1_000_000), int64(2_000_000)
	a, _ := newAgent(t, Options{
		Model:           "anthropic.claude-not-in-the-table-9",
		MaxOutputTokens: 100,
		Price:           &knov1.Price{InputPerMtokUsdMicros: &in, OutputPerMtokUsdMicros: &out},
	}, func(http.ResponseWriter, *http.Request) {})

	est, err := a.Estimate(t.Context(), aCase("c1", "hi"))
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.CostUSDMicros != 223 {
		t.Errorf("CostUSDMicros = %d, want 223 (the 10%% multiplier on the override-priced 202)", est.CostUSDMicros)
	}

	worst := a.WorstCase()
	if worst.CostUSDMicros == 0 {
		t.Error("WorstCase must carry the override too — the consent quote is one of the three guard touch-points")
	}
}

// TestEstimateIsLocal pins the contract the guard rests on: Estimate runs
// BEFORE the budget guard authorizes anything, so it must make no network
// call of any kind. An agent whose server fails the request still estimates
// cleanly, and no request reaches the wire.
func TestEstimateIsLocal(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{
		Model:           testModel,
		MaxOutputTokens: 1000,
	}, func(w http.ResponseWriter, r *http.Request) {
		t.Error("Estimate made a network call")
	})

	est, err := a.Estimate(t.Context(), aCase("c1", "hi"))
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.CostUSDMicros <= 0 {
		t.Errorf("CostUSDMicros = %d, want positive", est.CostUSDMicros)
	}
	if a.RoundTrips() != 0 {
		t.Errorf("RoundTrips = %d, want 0", a.RoundTrips())
	}
}

// TestUnpricedModelRefuses pins the plain unpriced path: no region class, so
// the refusal is the table's own.
func TestUnpricedModelRefuses(t *testing.T) {
	t.Parallel()

	a := &Agent{
		opts: Options{Model: "anthropic.claude-not-in-the-table-9", MaxOutputTokens: 1000},
	}
	_, err := a.Estimate(t.Context(), aCase("c1", "hi"))
	if !errors.Is(err, pricing.ErrUnpriced) {
		t.Fatalf("error = %v, want ErrUnpriced", err)
	}
	if !runFatal(err) {
		t.Errorf("an unpriced model is run-fatal")
	}
}

// TestEstimateNilAndCanceled pins the guardrails around Estimate itself.
func TestEstimateNilAndCanceled(t *testing.T) {
	t.Parallel()

	a := &Agent{opts: Options{Model: testModel, MaxOutputTokens: 1000}}
	if _, err := a.Estimate(t.Context(), nil); err == nil {
		t.Error("nil Case must be refused")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := a.Estimate(ctx, aCase("c1", "hi")); err == nil {
		t.Error("a canceled context must be refused")
	}
}

// TestSettleFromEstimateChargesTheReservation pins that an absent usage block
// settles the reservation, multiplier included — never zero, because a zero
// settlement is what makes a dollar cap unenforceable.
func TestSettleFromEstimateChargesTheReservation(t *testing.T) {
	t.Parallel()

	a := &Agent{opts: Options{Model: testModel, MaxOutputTokens: 1000}}
	resp := &core.Response{}
	a.settle(resp, aCase("c1", "hi"), &converseResponse{}, "")
	if !resp.UsageEstimated {
		t.Error("an estimated settlement must say so")
	}
	est, err := a.Estimate(t.Context(), aCase("c1", "hi"))
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if resp.CostUsdMicros != est.CostUSDMicros {
		t.Errorf("settled %d, want the reservation %d", resp.CostUsdMicros, est.CostUSDMicros)
	}
	if resp.CostUsdMicros <= 0 {
		t.Error("settlement must never be zero")
	}
}

// TestRegionalMultiplierPinsSaturation exercises the multiplier's edges
// through the shared pricing helper, which the adapter calls for all three
// guard touch-points.
func TestRegionalMultiplierPinsSaturation(t *testing.T) {
	t.Parallel()

	if got := pricing.Regional(100, 110); got != 110 {
		t.Errorf("Regional(100, 110) = %d, want 110", got)
	}
	if got := pricing.Regional(0, 110); got != 0 {
		t.Errorf("Regional(0, 110) = %d, want 0", got)
	}
	if got := pricing.Regional(100, 100); got != 100 {
		t.Errorf("Regional(100, 100) = %d, want the identity", got)
	}
	if got := pricing.Regional(100, 99); got != 100 {
		t.Errorf("Regional(100, 99) = %d, want the identity — a sub-100 pct must not under-reserve", got)
	}
	if got := pricing.RegionalMultiplierPct("anthropic", testModel); got != 100 {
		t.Errorf("a non-partner scheme must not carry the multiplier, got %d", got)
	}
	if got := pricing.RegionalMultiplierPct(Scheme, "us."+testModel); got != 100 {
		t.Errorf("a profile id must not carry the multiplier, got %d", got)
	}
	if got := pricing.RegionalMultiplierPct(Scheme, testModel); got != 110 {
		t.Errorf("the partner-cloud multiplier = %d, want 110", got)
	}
}
