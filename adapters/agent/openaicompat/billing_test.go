package openaicompat_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/knograph/kno/adapters/agent/openaicompat"
)

// TestAFailedCallTheProviderBilledCarriesItsCost.
//
// A failure is not always free. A gateway that answers
// `200 {"error":{…},"usage":{…}}` generated tokens, billed them, and only then
// said the call went wrong — and the usage block is in the payload the adapter
// already parsed. Dropping it settles a paid call at zero, and under
// --max-cost-usd that is spend the cap cannot see: real spend exceeds recorded
// spend, and a resume restores the understated figure and spends the difference
// again.
//
// core.invokeOnce settles every Invoke error as Spend{Calls: 1} with no dollars,
// so it cannot act on this yet. Carrying the figure on the error is what makes
// that a two-line change in core rather than a rediscovery. See
// docs/debt.md#43.
func TestAFailedCallTheProviderBilledCarriesItsCost(t *testing.T) {
	t.Parallel()

	const usage = `"usage":{"prompt_tokens":50000,"completion_tokens":100,` +
		`"prompt_tokens_details":{"cached_tokens":10000}}`

	// 40000 fresh + 10000 cached + 100 output at the published rates.
	want := int64(40000*lunaInputPerMTok/1_000_000) +
		int64(10000*lunaCachedPerMTok/1_000_000) +
		int64(100*lunaOutputPerMTok/1_000_000)

	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{
			// The shape the adapter's own comment says several compatible
			// gateways produce.
			"an error object inside a 200",
			http.StatusOK,
			`{"model":"m","error":{"message":"upstream refused","code":"upstream"},` + usage + `}`,
		},
		{
			// A 200 that generated and billed, then returned nothing usable.
			"a 200 with no choices",
			http.StatusOK,
			`{"model":"m","choices":[],` + usage + `}`,
		},
		{
			// A gateway reporting a charge alongside a server error. OpenAI does
			// not do this; a proxy in front of it can.
			"a 5xx carrying a usage block",
			http.StatusInternalServerError,
			`{"error":{"message":"boom"},` + usage + `}`,
		},
		{
			"a 4xx carrying a usage block",
			http.StatusBadRequest,
			`{"error":{"message":"nope"},` + usage + `}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				jsonReply(w, tc.status, tc.body)
			})
			a := newAgent(t, srv)

			resp, err := a.Invoke(t.Context(), newCase("c", "hi"))
			if err == nil {
				t.Fatalf("a failed call was accepted as an answer: %q", resp.GetOutput())
			}

			got, ok := openaicompat.BilledCostOf(err)
			if !ok {
				t.Fatalf("the provider reported a usage block and the error carries "+
					"no charge, so core settles this paid call at $0: %v", err)
			}
			if got != want {
				t.Errorf("billed cost = %d, want %d; a failed call and a successful "+
					"one with identical usage must settle at the identical figure",
					got, want)
			}
		})
	}
}

// TestAFailureWithNoReportedUsageCarriesNoCharge.
//
// The absence has to stay distinguishable from a reported zero. "The provider
// said it charged nothing" is evidence; "the provider said nothing" is not, and
// a caller that settled the second as zero would be asserting something no
// provider told it. Every recorded OpenAI error fixture is in this group.
func TestAFailureWithNoReportedUsageCarriesNoCharge(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"a 401 with no usage", http.StatusUnauthorized, `{"error":{"message":"no key"}}`},
		{"a 500 with no usage", http.StatusInternalServerError, `{"error":{"message":"boom"}}`},
		{
			// Zeros are a claim the call was free, and usableUsage refuses to
			// believe them for the same reason the success path does.
			"a usage block claiming the call was free",
			http.StatusOK,
			`{"model":"m","error":{"message":"x"},"usage":{"prompt_tokens":0,"completion_tokens":0}}`,
		},
		{
			// A negative count is nonsense, and nonsense must not be priced. This
			// is the case that separates believing the block from merely checking
			// the resulting figure: costOf clamps a negative prompt term to zero
			// and then charges the OUTPUT term in full, so a garbage block would
			// otherwise settle a fabricated charge against the guard. An all-zero
			// block prices to zero either way and cannot tell the two apart.
			"a usage block with a negative count",
			http.StatusOK,
			`{"model":"m","error":{"message":"x"},` +
				`"usage":{"prompt_tokens":-5,"completion_tokens":100000}}`,
		},
		{
			"a 429, which is refused before anything is generated",
			http.StatusTooManyRequests,
			`{"error":{"message":"slow down"}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				jsonReply(w, tc.status, tc.body)
			})
			a := newAgent(t, srv)

			_, err := a.Invoke(t.Context(), newCase("c", "hi"))
			if err == nil {
				t.Fatal("the call produced no error")
			}
			if micros, ok := openaicompat.BilledCostOf(err); ok {
				t.Errorf("a charge of %d was reported for a failure the provider said "+
					"nothing about; inventing one is as wrong as dropping a real one",
					micros)
			}
		})
	}
}

// TestCarryingAChargeDoesNotDisturbClassification.
//
// The charge rides on a wrapper, and a wrapper that hid what it wrapped would
// be worse than no wrapper at all: core decides whether to retry by walking the
// chain, and a 500 that stopped reading as ErrTransportTransient would become a
// terminally errored Case.
func TestCarryingAChargeDoesNotDisturbClassification(t *testing.T) {
	t.Parallel()

	const usage = `"usage":{"prompt_tokens":5000,"completion_tokens":10}`
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		jsonReply(w, http.StatusInternalServerError,
			`{"error":{"message":"boom","code":"server_error"},`+usage+`}`)
	})
	a := newAgent(t, srv)

	_, err := a.Invoke(t.Context(), newCase("c", "hi"))
	if err == nil {
		t.Fatal("a 500 produced no error")
	}
	if _, ok := openaicompat.BilledCostOf(err); !ok {
		t.Fatal("the fixture is not exercising a billed failure")
	}
	if !retryable(err) {
		t.Error("a 500 carrying a charge stopped being retryable; the wrapper is " +
			"hiding the classification underneath it")
	}
	if !strings.Contains(err.Error(), "server_error") {
		t.Errorf("the wrapper changed the rendered message: %v", err)
	}
}

// TestTheReservedPromptCountIsRecordedWhenUsageIsAbsent.
//
// Response.prompt_tokens is not decoration on this path: core.spendOfN persists
// prompt+completion as the run's token total, and the store's figure is what
// Guard.Restore reads on resume. Settling the reservation's DOLLARS while
// recording zero TOKENS makes the two halves of one settlement disagree, and
// the token total silently under-reports every usage-less Case.
func TestTheReservedPromptCountIsRecordedWhenUsageIsAbsent(t *testing.T) {
	t.Parallel()

	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		jsonReply(w, http.StatusOK, `{"model":"m","choices":[{"index":0,`+
			`"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`)
	})
	a := newAgent(t, srv)
	c := newCase("c", strings.Repeat("question ", 64))

	est, err := a.Estimate(t.Context(), c)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	resp, err := a.Invoke(t.Context(), c)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !resp.GetUsageEstimated() {
		t.Fatal("the fixture is not exercising the usage-less path")
	}
	if resp.GetPromptTokens() <= 0 {
		t.Fatalf("prompt_tokens = %d on a settlement that charged %d micro-USD; "+
			"the dollars and the tokens describe the same call and must agree",
			resp.GetPromptTokens(), resp.GetCostUsdMicros())
	}
	// The reservation's own input term: Estimate reports input plus the output
	// ceiling, so removing the ceiling leaves what was reserved for the prompt.
	if want := est.Tokens - a.OutputCeiling(); resp.GetPromptTokens() != want {
		t.Errorf("prompt_tokens = %d, want the reserved input term %d",
			resp.GetPromptTokens(), want)
	}
}

// TestLatencyIsMeasuredRatherThanAssumed.
//
// With the clock private and every test in an external package, nothing could
// reach the seam — so "latency is measured" was an untested claim, and
// replacing the measurement with a constant zero broke nothing. Options.Now
// exists so this assertion can exist.
func TestLatencyIsMeasuredRatherThanAssumed(t *testing.T) {
	t.Parallel()

	// Invoke reads the clock exactly twice: once before the request and once
	// after. A fixed step between reads makes the measured duration a value the
	// test chose rather than one the scheduler happened to produce.
	const step = 250 * time.Millisecond
	base := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	var reads int

	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		jsonReply(w, http.StatusOK, answeredBody)
	})
	a := newAgent(t, srv, func(o *openaicompat.Options) {
		o.Now = func() time.Time {
			reads++
			return base.Add(time.Duration(reads-1) * step)
		}
	})

	resp, err := a.Invoke(t.Context(), newCase("c", "hi"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if reads != 2 {
		t.Errorf("the clock was read %d times, want 2 (before and after the "+
			"request); anything else means latency spans the wrong interval", reads)
	}
	if got, want := resp.GetLatencyMs(), step.Milliseconds(); got != want {
		t.Errorf("latency_ms = %d, want %d", got, want)
	}
}

// TestLatencyExcludesTheRateLimitersOwnHold.
//
// Client.Do waits inside itself when an earlier 429 closed the host, and returns
// WaitedFor precisely so a caller can subtract it. Left in, latency_ms stops
// describing the provider and starts describing our own pacing — measured at
// 1002ms for a call the server answered instantly. That number is compared
// across adapters and across runs, so an adapter that includes the hold and one
// that does not are not reporting the same quantity.
func TestLatencyExcludesTheRateLimitersOwnHold(t *testing.T) {
	t.Parallel()

	var seen int
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		seen++
		if seen == 1 {
			w.Header().Set("Retry-After", "1")
			jsonReply(w, http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`)
			return
		}
		jsonReply(w, http.StatusOK, answeredBody)
	})
	a := newAgent(t, srv)

	// The 429 closes the host for a second. The adapter does not retry — core
	// does — so this stands in for the next attempt.
	if _, err := a.Invoke(t.Context(), newCase("c", "hi")); err == nil {
		t.Fatal("the first call was expected to be rate limited")
	}

	start := time.Now()
	resp, err := a.Invoke(t.Context(), newCase("c", "hi"))
	wall := time.Since(start)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if wall < 500*time.Millisecond {
		t.Fatalf("the second call took %v; the limiter never held it, so this "+
			"fixture is not exercising the subtraction", wall)
	}
	if got := resp.GetLatencyMs(); got > 500 {
		t.Errorf("latency_ms = %d for a call the server answered instantly after a "+
			"%v hold; the limiter's wait is our pacing, not the model's latency",
			got, wall)
	}
}

// TestAnOutOfRangeCeilingIsRefusedAtConstruction.
//
// An output ceiling past the pricing package's overflow bound makes every
// Estimate return an error AND makes WorstCase return zero — and core reads a
// zero WorstCase as "this adapter cannot plan" and falls back to its own
// scalar. That is precisely the failure WorstCase exists to prevent, reached
// through a mistyped flag, and nothing in the run would name the cause: under a
// cost cap every Case is refused as unpriceable and the message blames the
// pricing table.
func TestAnOutOfRangeCeilingIsRefusedAtConstruction(t *testing.T) {
	t.Parallel()

	srv := serve(t, func(_ http.ResponseWriter, _ *http.Request) {})

	t.Run("an absurd output ceiling", func(t *testing.T) {
		t.Parallel()
		if _, err := tryAgent(t, srv, func(o *openaicompat.Options) {
			o.MaxOutputTokens = 10_000_001
		}); err == nil {
			t.Error("New accepted an output ceiling the cost arithmetic cannot bound")
		}
	})

	t.Run("an absurd prompt ceiling", func(t *testing.T) {
		t.Parallel()
		if _, err := tryAgent(t, srv, func(o *openaicompat.Options) {
			o.MaxPromptBytes = (64 << 20) + 1
		}); err == nil {
			t.Error("New accepted a prompt ceiling that bounds nothing")
		}
	})

	t.Run("the largest ceiling that still works", func(t *testing.T) {
		t.Parallel()
		a, err := tryAgent(t, srv, func(o *openaicompat.Options) {
			o.MaxOutputTokens = 10_000_000
		})
		if err != nil {
			t.Fatalf("New refused the boundary value: %v", err)
		}
		// The point of the refusal: at an accepted ceiling, WorstCase is a real
		// number rather than the zero core reads as "cannot plan".
		if a.WorstCase().CostUSDMicros <= 0 {
			t.Error("WorstCase is zero at an accepted ceiling, so the refusal is " +
				"drawn in the wrong place")
		}
	})
}
