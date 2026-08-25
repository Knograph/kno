package anthropic_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/agent/anthropic"
	"github.com/knograph/kno/adapters/agent/pricing"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// estimator builds an Agent with no server, for the arithmetic-only tests.
func estimator(t *testing.T, mutate ...func(*anthropic.Options)) *anthropic.Agent {
	t.Helper()
	opts := anthropic.Options{
		Model:           testModel,
		MaxOutputTokens: 1024,
		BaseURL:         "https://example.invalid",
	}
	for _, f := range mutate {
		f(&opts)
	}
	a, err := anthropic.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// TestEstimateCountsEveryByteTheProviderWillBill.
//
// Counting only Case.input is the path of least resistance, and it under-
// reserves by the whole conversation and the whole system prompt —
// systematically, in the direction that walks a run past its cap.
func TestEstimateCountsEveryByteTheProviderWillBill(t *testing.T) {
	t.Parallel()

	bare := estimator(t)
	withSystem := estimator(t, func(o *anthropic.Options) {
		o.System = strings.Repeat("a long standing instruction. ", 40)
	})

	input := aCase("the question")
	withHistory := &knov1.Case{Id: "c", Input: "the question", History: []*knov1.Turn{
		{Role: knov1.Role_ROLE_USER, Content: strings.Repeat("earlier turn. ", 40)},
		{Role: knov1.Role_ROLE_ASSISTANT, Content: strings.Repeat("earlier answer. ", 40)},
	}}
	withSystemTurn := &knov1.Case{Id: "c", Input: "the question", History: []*knov1.Turn{
		{Role: knov1.Role_ROLE_SYSTEM, Content: strings.Repeat("system turn. ", 40)},
	}}

	base := estimate(t, bare, input)

	for name, got := range map[string]int64{
		"the run's system prompt": estimate(t, withSystem, input),
		"prior turns":             estimate(t, bare, withHistory),
		"a system history turn":   estimate(t, bare, withSystemTurn),
	} {
		if got <= base {
			t.Errorf("%s did not raise the estimate (%d vs %d); a term the provider "+
				"bills is missing from the reservation", name, got, base)
		}
	}
}

// TestEstimateBoundsTheRecordedExchanges.
//
// Honest about what it is: seven recorded exchanges, replayed. It is a
// regression check against the specific usage blocks in testdata, NOT evidence
// about usage blocks in general — that is what the property test below is for.
// The previous comment claimed it drew from "usage blocks a provider really
// returned", which overstated a hand-authored table of five numbers.
func TestEstimateBoundsTheRecordedExchanges(t *testing.T) {
	t.Parallel()

	a := estimator(t)
	for _, f := range loadFixtures(t) {
		if f.status != 200 {
			continue
		}
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()

			c := f.evalCase()
			est := estimate(t, a, c)
			resp := replay(t, f)
			if resp.GetUsageEstimated() {
				// Settled AT the reservation by construction; nothing to compare.
				return
			}
			if resp.GetCostUsdMicros() > est {
				t.Errorf("settled %d micro-USD against a reservation of %d; a bound "+
					"that can be too low is not a bound",
					resp.GetCostUsdMicros(), est)
			}
		})
	}
}

// TestEstimateBoundsAnyUsageTheProviderCouldReport.
//
// The property the plan's §7 asked for, over the whole space a real response
// can occupy rather than over numbers someone chose.
//
// The generator is bounded by what the request itself permits, which is what
// makes the property meaningful rather than tautological:
//
//   - output_tokens cannot exceed max_tokens; the provider stops there. That
//     is the ceiling the estimate charges in full.
//   - input tokens cannot exceed what the prompt contains. The densest text
//     measured against the real tokenizer is base64 at 1.47 bytes/token, so
//     bytes/1.47 is the most a prompt of that size can cost — an EXTERNAL
//     bound, not the estimator's own 2-bytes-per-token model, so the property
//     cannot be satisfied by the estimator agreeing with itself.
//   - the split across fresh / cache-write / cache-read is swept, because the
//     three are priced differently and the estimate charges the fresh rate for
//     all of it.
//
// Deterministic: a fixed seed, printed on failure, because a property test that
// cannot be replayed is a flake report.
func TestEstimateBoundsAnyUsageTheProviderCouldReport(t *testing.T) {
	t.Parallel()

	const maxOut = 4096
	const densestBytesPerToken = 1.47

	a := estimator(t, func(o *anthropic.Options) { o.MaxOutputTokens = maxOut })

	const seed = 0x5eed5eed
	rng := rand.New(rand.NewPCG(seed, 0x9e3779b9)) //nolint:gosec // property inputs, not cryptography

	for i := range 300 {
		size := 1 + rng.IntN(20000)
		input := strings.Repeat("x", size)
		c := aCase(input)

		// The most tokens a prompt this size could possibly cost.
		maxIn := int64(float64(size)/densestBytesPerToken) + 1

		// Split that budget across the three input dimensions, then take an
		// output count the request permits.
		fresh := rng.Int64N(maxIn + 1)
		write := rng.Int64N(maxIn - fresh + 1)
		read := maxIn - fresh - write
		out := rng.Int64N(maxOut + 1)

		est := estimate(t, a, c)
		got := settledCostFor(t, a, c, usageJSON(fresh, out, write, read))

		if got > est {
			t.Fatalf("seed=%#x i=%d size=%d fresh=%d write=%d read=%d out=%d: "+
				"settled %d micro-USD against a reservation of %d; a bound that can "+
				"be too low is not a bound",
				seed, i, size, fresh, write, read, out, got, est)
		}
	}
}

// usageJSON renders a usage block with every field present.
func usageJSON(input, output, cacheWrite, cacheRead int64) string {
	return fmt.Sprintf(
		`"usage":{"input_tokens":%d,"output_tokens":%d,`+
			`"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d}`,
		input, output, cacheWrite, cacheRead,
	)
}

// settledCostFor replays one synthetic response through a real Invoke and
// returns what it settled at.
//
// Through Invoke rather than against the pricing arithmetic directly, so the
// property covers the path a run actually takes — including the usable() checks
// that decide whether the block is trusted at all.
func settledCostFor(t *testing.T, a *anthropic.Agent, c *knov1.Case, usage string) int64 {
	t.Helper()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, `{"id":"m","model":"claude-sonnet-4-6","stop_reason":"end_turn",`+
			`"content":[{"type":"text","text":"an answer"}],`+usage+`}`)
	})
	replayer := newAgent(t, srv, func(o *anthropic.Options) {
		o.Model = a.Model()
		o.MaxOutputTokens = 4096
	})

	resp, err := replayer.Invoke(t.Context(), c)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	return resp.GetCostUsdMicros()
}

// TestEstimateChargesOneCall.
//
// One Invoke settles as one provider call. Reserving more would reserve N and
// settle 1, and --max-calls would drift by (N-1) for every Case.
func TestEstimateChargesOneCall(t *testing.T) {
	t.Parallel()

	est, err := estimator(t).Estimate(t.Context(), aCase("q"))
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.Calls != 1 {
		t.Errorf("Calls = %d, want 1", est.Calls)
	}
}

// TestEstimateRefusesAnUnpricedModelRatherThanGuessing.
//
// A zero estimate makes a dollar cap unenforceable, which is the failure that
// already overshot a cap once. An error is not the same as zero, and core
// treats a cost it cannot know as a refusal when a cap is set.
func TestEstimateRefusesAnUnpricedModelRatherThanGuessing(t *testing.T) {
	t.Parallel()

	a := estimator(t, func(o *anthropic.Options) { o.Model = "claude-not-in-the-table-9" })

	if _, err := a.Estimate(t.Context(), aCase("q")); err == nil {
		t.Fatal("an unpriced model produced an estimate")
	}
	if got := a.WorstCase().CostUSDMicros; got != 0 {
		t.Errorf("WorstCase = %d for an unpriced model; core falls back to the "+
			"run-scoped scalar on zero, which is the sanctioned degradation", got)
	}

	// And it is RUN-FATAL, because the model does not change mid-run. Under a
	// dollar cap core refuses every Case it cannot price, so without the
	// escalation a run made one refusal per Case and ended as "too many cases
	// errored" — a verdict naming nothing about pricing, after taking the
	// user's consent for a figure that was never going to apply. Marked here
	// rather than in core, which cannot tell this apart from an Estimator that
	// refuses one Case and prices the rest. See docs/debt.md#46.
	_, err := a.Estimate(t.Context(), aCase("q"))
	var rf interface{ RunFatal() bool }
	if !errors.As(err, &rf) || !rf.RunFatal() {
		t.Error("an unpriced model is not run-fatal, so a capped run refuses " +
			"every Case one at a time and reports an error rate rather than a " +
			"pricing problem")
	}
	if !errors.Is(err, pricing.ErrUnpriced) {
		t.Errorf("the escalation destroyed the classification it wraps: %v", err)
	}
}

// TestEstimateHonorsACancelledContext, so a stopping run does not do arithmetic
// for Cases it will never send.
func TestEstimateHonorsACancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := estimator(t).Estimate(ctx, aCase("q")); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// TestEstimateRefusesANilCase.
func TestEstimateRefusesANilCase(t *testing.T) {
	t.Parallel()

	if _, err := estimator(t).Estimate(t.Context(), nil); err == nil {
		t.Fatal("a nil Case produced an estimate")
	}
}

// TestWorstCaseBoundsEveryPerCaseEstimate.
//
// Planning needs a number and per-Case estimates need a Case. Measured with an
// adapter pricing at $0.20 against a scalar of $0.001, the consent prompt
// quoted $0.06 for a run whose real exposure was $12.00 — so this number must
// be an upper bound, not a typical one.
func TestWorstCaseBoundsEveryPerCaseEstimate(t *testing.T) {
	t.Parallel()

	a := estimator(t, func(o *anthropic.Options) { o.MaxPromptBytes = 4096 })
	worst := a.WorstCase()
	if worst.CostUSDMicros <= 0 {
		t.Fatal("WorstCase is zero for a priced model, so planning would fall back " +
			"to a scalar an Estimator does not use")
	}

	for _, size := range []int{1, 100, 1000, 4096} {
		c := aCase(strings.Repeat("x", size))
		if got := estimate(t, a, c); got > worst.CostUSDMicros {
			t.Errorf("a %d-byte Case estimates %d against a worst case of %d",
				size, got, worst.CostUSDMicros)
		}
	}
}

// TestWorstCaseGrowsWithTheOutputCeiling.
//
// The output term dominates and is known up front, which is what makes the
// question answerable before any Case is seen.
func TestWorstCaseGrowsWithTheOutputCeiling(t *testing.T) {
	t.Parallel()

	small := estimator(t, func(o *anthropic.Options) { o.MaxOutputTokens = 1024 }).WorstCase()
	large := estimator(t, func(o *anthropic.Options) { o.MaxOutputTokens = 64000 }).WorstCase()

	if large.CostUSDMicros <= small.CostUSDMicros {
		t.Errorf("raising --max-output-tokens did not raise the worst case (%d vs %d)",
			large.CostUSDMicros, small.CostUSDMicros)
	}
}

// TestAnAbsurdPromptCeilingIsClampedRatherThanAllocated.
//
// WorstCase has no error return, so a fat-fingered value must not become a
// multi-gigabyte allocation on a planning call.
func TestAnAbsurdPromptCeilingIsClampedRatherThanAllocated(t *testing.T) {
	t.Parallel()

	a := estimator(t, func(o *anthropic.Options) { o.MaxPromptBytes = 1 << 40 })
	if a.WorstCase().CostUSDMicros <= 0 {
		t.Error("the clamped worst case is not a usable number")
	}
}

// estimate is Estimate's cost, or a fatal test failure.
func estimate(t *testing.T, a *anthropic.Agent, c *knov1.Case) int64 {
	t.Helper()
	est, err := a.Estimate(t.Context(), c)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	return est.CostUSDMicros
}

// TestAPriceOverrideReachesTheEstimateAndTheWorstCase.
//
// The CLI accepted --price-input-per-mtok and --price-output-per-mtok, validated
// them as a pair, and then DISCARDED them for this scheme — while the cookbook,
// the CI recipe, and `kno doctor` all named them as the remedy for an unpriced
// model. A silently ignored flag on the money path is worse than a missing one:
// the user believes they supplied a price, and the run is refused for having
// none.
//
// WorstCase matters as much as Estimate: it is the figure checkCostIsKnowable
// reads, so an override that reached only Estimate would still leave the run
// refused for having no computable cost.
func TestAPriceOverrideReachesTheEstimateAndTheWorstCase(t *testing.T) {
	t.Parallel()

	in, out := int64(3_000_000), int64(15_000_000)
	price := &knov1.Price{
		InputPerMtokUsdMicros:  &in,
		OutputPerMtokUsdMicros: &out,
	}

	// A model with no row in the table, which is the whole point of an
	// override.
	a := estimator(t, func(o *anthropic.Options) {
		o.Model = "claude-not-in-the-table-9"
		o.Price = price
	})

	est, err := a.Estimate(t.Context(), aCase("q"))
	if err != nil {
		t.Fatalf("an explicit price did not price an unpriced model: %v", err)
	}
	if est.CostUSDMicros <= 0 {
		t.Errorf("estimate = %d; the override produced no cost", est.CostUSDMicros)
	}

	if got := a.WorstCase().CostUSDMicros; got <= 0 {
		t.Errorf("WorstCase = %d for a model with an explicit price; the run "+
			"would still be refused for having no computable cost", got)
	}

	// Without the override the same model is unpriced and run-fatal, so the
	// test above is not passing because the table quietly covers it.
	bare := estimator(t, func(o *anthropic.Options) {
		o.Model = "claude-not-in-the-table-9"
	})
	if _, err := bare.Estimate(t.Context(), aCase("q")); err == nil {
		t.Fatal("the model is priced by the table, so this test proves nothing")
	}
}
