package anthropic_test

import (
	"net/http"
	"testing"
)

// What a call COST, as distinct from what it said.
//
// Split out of response_test.go because these are the assertions that carry
// money: a wrong number here is a wrong invoice, a wrong cap, and a wrong
// figure in the report — and none of it looks like a failure.

// TestCachedInputIsPricedAtItsOwnRate.
//
// Anthropic bills fresh input, cache writes, cache reads, and output at four
// different rates. Settling cache reads at the fresh input rate overstates that
// term by 10x — a systematic divergence from the user's invoice, in the
// direction that looks like Kno working correctly.
func TestCachedInputIsPricedAtItsOwnRate(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, `{"id":"m","model":"claude-sonnet-4-6","content":[{"type":"text","text":"ok"}],
		            "stop_reason":"end_turn",
		            "usage":{"input_tokens":1000,"output_tokens":500,
		                     "cache_creation_input_tokens":200,"cache_read_input_tokens":4000}}`)
	})
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), aCase("q"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	// claude-sonnet-4-6 at $3 / $0.30 / $3.75 / $15 per MTok:
	//   fresh input  1000 * 3.00 / 1e6 = 3000 micro-USD
	//   cache write   200 * 3.75 / 1e6 =  750
	//   cache read   4000 * 0.30 / 1e6 = 1200   (12000 at the fresh rate)
	//   output        500 * 15.0 / 1e6 = 7500
	const want = 3000 + 750 + 1200 + 7500
	if resp.GetCostUsdMicros() != want {
		t.Errorf("cost = %d micro-USD, want %d", resp.GetCostUsdMicros(), want)
	}

	// Billed input is the SUM. Anthropic's input_tokens counts only what falls
	// after the last cache breakpoint, so reading it alone under-reports a
	// cached request by the whole cached prefix.
	if got := resp.GetPromptTokens(); got != 1000+200+4000 {
		t.Errorf("prompt_tokens = %d, want %d — input_tokens alone is not the "+
			"billed input on this API", got, 1000+200+4000)
	}
	if got := resp.GetCachedTokens(); got != 4000 {
		t.Errorf("cached_tokens = %d, want 4000", got)
	}
	if got := resp.GetCompletionTokens(); got != 500 {
		t.Errorf("completion_tokens = %d, want 500", got)
	}
	if resp.GetUsageEstimated() {
		t.Error("a real usage block was reported as estimated")
	}
}

// TestAMissingUsageBlockSettlesAtTheEstimateNeverAtZero.
//
// A zero settlement is what makes a dollar cap unenforceable, and it has
// already caused one real overshoot. The ADAPTER stamps the inferred number,
// because core derives spend from the Response and the store persists that same
// derivation — charging the guard a number the Response does not carry makes a
// resumed run spend its cap twice.
func TestAMissingUsageBlockSettlesAtTheEstimateNeverAtZero(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, `{"id":"m","model":"claude-sonnet-4-6",
		            "content":[{"type":"text","text":"an answer with no usage block"}],
		            "stop_reason":"end_turn"}`)
	})
	a := newAgent(t, srv)

	c := aCase("q")
	est, err := a.Estimate(t.Context(), c)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}

	resp, err := a.Invoke(t.Context(), c)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !resp.GetUsageEstimated() {
		t.Error("usage_estimated is false for a cost nothing reported")
	}
	if resp.GetCostUsdMicros() != est.CostUSDMicros {
		t.Errorf("cost = %d, want the reservation %d", resp.GetCostUsdMicros(), est.CostUSDMicros)
	}
	if resp.GetCostUsdMicros() == 0 {
		t.Error("settled at zero, which is what makes a dollar cap unenforceable")
	}
}

// TestUsageThatDisagreesWithTheBodyIsNotTrusted.
//
// Worse than an absent block: absent is handled, while a block reporting zero
// output tokens for a full answer settles the Case at input-only cost, and a
// dollar cap then under-counts every Case in the run with nothing saying so.
func TestUsageThatDisagreesWithTheBodyIsNotTrusted(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"zero output tokens for a real answer": `"usage":{"input_tokens":50,"output_tokens":0}`,
		"zero billed input":                    `"usage":{"input_tokens":0,"output_tokens":20}`,
		"negative tokens":                      `"usage":{"input_tokens":-5,"output_tokens":20}`,

		// An empty object is NOT a report of zeros. It decodes to the same
		// values a provider reporting genuine zeros would, and inferring
		// "billed nothing" from it settles every Case in the run at whatever
		// the absent fields default to.
		"an empty usage object": `"usage":{}`,

		// The shape a schema move produces. cache_creation already exists as a
		// nested object alongside the scalar cache_creation_input_tokens on
		// this API; if input_tokens ever moves the same way, presence is the
		// only thing that notices.
		"fields moved under a wrapper": `"usage":{"input":{"tokens":50},"output_tokens":20}`,

		// The case where presence is the ONLY thing standing. input_tokens is
		// gone but the cache fields remain, so billed input is positive and
		// every value check passes — and the Case would settle at the cached
		// rate alone, missing the entire fresh-input term, silently, for every
		// Case in the run.
		"input_tokens gone while the cache fields remain": `"usage":{` +
			`"output_tokens":20,"cache_read_input_tokens":5000}`,

		// The same hole on the output side: a body with text and no
		// output_tokens field at all.
		"output_tokens gone": `"usage":{"input_tokens":50,"cache_read_input_tokens":10}`,

		// Beyond any real context window, so it is not describing a served
		// request. Trusted, it saturates the cost arithmetic and hands the
		// budget guard a value that makes its cap looser.
		"implausible token counts": `"usage":{"input_tokens":9223372036854775807,"output_tokens":20}`,
	}
	for name, usage := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				answer(w, `{"id":"m","model":"claude-sonnet-4-6",
				            "content":[{"type":"text","text":"a full and complete answer"}],
				            "stop_reason":"end_turn",`+usage+`}`)
			})
			a := newAgent(t, srv)

			c := aCase("q")
			est, err := a.Estimate(t.Context(), c)
			if err != nil {
				t.Fatalf("Estimate: %v", err)
			}
			resp, err := a.Invoke(t.Context(), c)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if !resp.GetUsageEstimated() {
				t.Error("a usage block that cannot describe this response was trusted")
			}
			if resp.GetCostUsdMicros() != est.CostUSDMicros {
				t.Errorf("cost = %d, want the reservation %d",
					resp.GetCostUsdMicros(), est.CostUSDMicros)
			}
		})
	}
}

// TestCostFollowsTheModelThatActuallyAnswered.
//
// The requested name is an alias; the resolved name is what the invoice bills.
// A run that asks for one model and is served another — an alias re-pointing
// mid-run, a gateway substituting a model — must be costed at what was served,
// or every Case in the run diverges from the user's bill in whichever direction
// the substitution went.
//
// Driven with two models the table prices DIFFERENTLY, which is the only way
// the preference is observable at all: a dated suffix of the requested model
// resolves to the same row and would agree either way.
func TestCostFollowsTheModelThatActuallyAnswered(t *testing.T) {
	t.Parallel()

	// claude-sonnet-4-6 is $3 in / $15 out; claude-opus-5 is $5 / $25.
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, `{"id":"m","model":"claude-opus-5","stop_reason":"end_turn",
		            "content":[{"type":"text","text":"ok"}],
		            "usage":{"input_tokens":1000,"output_tokens":1000}}`)
	})
	a := newAgent(t, srv) // requests claude-sonnet-4-6

	resp, err := a.Invoke(t.Context(), aCase("q"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.GetResolvedModel() != "claude-opus-5" {
		t.Fatalf("resolved_model = %q", resp.GetResolvedModel())
	}

	const wantOpus = 5000 + 25000   // what was served
	const wantSonnet = 3000 + 15000 // what was asked for
	switch resp.GetCostUsdMicros() {
	case wantOpus:
	case wantSonnet:
		t.Errorf("cost = %d, the price of the model REQUESTED; the provider served "+
			"%s and that is what the invoice charges", resp.GetCostUsdMicros(),
			resp.GetResolvedModel())
	default:
		t.Errorf("cost = %d micro-USD, want %d", resp.GetCostUsdMicros(), wantOpus)
	}
}

// TestAnUnpricedResolvedModelFallsBackToTheRequestedOne.
//
// The reverse case. A provider resolving to a name the table has never heard of
// must not un-price a Case whose requested model IS priced — with a cost cap
// set, core already authorized that Case against the requested model's
// estimate, and settling it at zero would break the pairing between what was
// reserved and what was settled.
func TestAnUnpricedResolvedModelFallsBackToTheRequestedOne(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, `{"id":"m","model":"some-internal-build-name","stop_reason":"end_turn",
		            "content":[{"type":"text","text":"ok"}],
		            "usage":{"input_tokens":1000,"output_tokens":1000}}`)
	})
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), aCase("q"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if want := int64(3000 + 15000); resp.GetCostUsdMicros() != want {
		t.Errorf("cost = %d, want %d — the requested model is priced and the Case "+
			"was authorized against it", resp.GetCostUsdMicros(), want)
	}
	if resp.GetUsageEstimated() {
		t.Error("a Case with a real usage block and a priced requested model was " +
			"reported as estimated")
	}
}
