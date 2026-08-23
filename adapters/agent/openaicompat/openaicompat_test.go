package openaicompat_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/knograph/kno/adapters/agent/agentref"
	"github.com/knograph/kno/adapters/agent/internal/transport"
	"github.com/knograph/kno/adapters/agent/openaicompat"
	"github.com/knograph/kno/adapters/agent/pricing"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// pricedModel has a row in the table, so cost is arithmetic a test can predict
// rather than a number the code hands back to itself.
const pricedModel = "gpt-5.6-luna"

// Rates for pricedModel, restated from the published table so a silent table
// edit shows up here as a failing assertion rather than as a quietly changed
// invoice. Micro-USD per million tokens.
const (
	lunaInputPerMTok  = 200_000
	lunaCachedPerMTok = 20_000
	lunaOutputPerMTok = 1_200_000
)

// serve starts a test server that runs h, closed on cleanup.
func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// newAgent builds an Agent aimed at srv.
//
// srv.Client() supplies the transport, which is what keeps the connection pool
// owned by the test server: a pool this test built would outlive srv.Close and
// the package's leak check would report it.
func newAgent(t *testing.T, srv *httptest.Server, tweak ...func(*openaicompat.Options)) *openaicompat.Agent {
	t.Helper()

	a, err := tryAgent(t, srv, tweak...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// tryAgent is newAgent for the tests where the REFUSAL is the behaviour under
// test, so New's error is returned rather than fatal.
func tryAgent(t *testing.T, srv *httptest.Server, tweak ...func(*openaicompat.Options)) (*openaicompat.Agent, error) {
	t.Helper()
	return openaicompat.New(baseOptions(t, srv, tweak...))
}

// baseOptions are the Options every test in this package starts from.
func baseOptions(t *testing.T, srv *httptest.Server, tweak ...func(*openaicompat.Options)) openaicompat.Options {
	t.Helper()

	ref, err := agentref.Parse("openai:" + pricedModel + "@" + srv.URL)
	if err != nil {
		t.Fatalf("parsing the agent ref: %v", err)
	}
	opts := openaicompat.Options{
		Ref: ref,
		// srv.Client() supplies the transport, which is what keeps the
		// connection pool owned by the test server: a pool this test built
		// would outlive srv.Close and the package's leak check would report it.
		HTTPClient: srv.Client(),
		// A test server is loopback over plain HTTP, which is exactly the local
		// vLLM/Ollama case the policy exists to make opt-in.
		Policy: transport.Policy{AllowInsecureHTTP: true, AllowPrivateAddress: true},
	}
	for _, f := range tweak {
		if f != nil {
			f(&opts)
		}
	}
	return opts
}

func newCase(id, input string) *core.Case {
	return &core.Case{Id: id, Input: input, Split: knov1.Split_SPLIT_DEV}
}

// jsonReply writes a body as a chat completion reply.
func jsonReply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

const answeredBody = `{
  "id": "chatcmpl-fixture",
  "object": "chat.completion",
  "model": "gpt-5.6-luna-2026-07-01",
  "system_fingerprint": "fp_abc123",
  "choices": [{
    "index": 0,
    "message": {"role": "assistant", "content": "Paris"},
    "finish_reason": "stop"
  }],
  "usage": {
    "prompt_tokens": 1000,
    "completion_tokens": 500,
    "total_tokens": 1500,
    "prompt_tokens_details": {"cached_tokens": 200}
  }
}`

// TestAnAnsweredCaseCarriesEverythingTheEngineRecords.
//
// Not a "does it work" test. Every field asserted here is load-bearing
// somewhere else: the token counts and the cost feed the budget guard and the
// store, refused and stop_reason decide whether the score means anything, and
// resolved_model is what a resume compares so a re-pointed alias cannot blend
// two models into one aggregate. A field left unset is a silent zero
// downstream, which is why absence is asserted as hard as presence.
func TestAnAnsweredCaseCarriesEverythingTheEngineRecords(t *testing.T) {
	t.Parallel()

	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		jsonReply(w, http.StatusOK, answeredBody)
	})
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), newCase("c1", "What is the capital of France?"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if got := resp.GetCaseId(); got != "c1" {
		t.Errorf("case_id = %q, want c1", got)
	}
	if got := resp.GetOutput(); got != "Paris" {
		t.Errorf("output = %q, want Paris", got)
	}
	if got, want := resp.GetPromptTokens(), int64(1000); got != want {
		t.Errorf("prompt_tokens = %d, want %d", got, want)
	}
	if got, want := resp.GetCompletionTokens(), int64(500); got != want {
		t.Errorf("completion_tokens = %d, want %d", got, want)
	}
	if got, want := resp.GetCachedTokens(), int64(200); got != want {
		t.Errorf("cached_tokens = %d, want %d", got, want)
	}

	// 800 fresh input + 200 cached + 500 output, at the published rates.
	// Stated as arithmetic rather than as a literal so a rate change reads as a
	// changed rate rather than as a mysteriously different number.
	want := int64(800*lunaInputPerMTok/1_000_000) +
		int64(200*lunaCachedPerMTok/1_000_000) +
		int64(500*lunaOutputPerMTok/1_000_000)
	if got := resp.GetCostUsdMicros(); got != want {
		t.Errorf("cost_usd_micros = %d, want %d; the settled cost is what the store "+
			"persists and what Guard.Restore reads on resume", got, want)
	}
	if resp.GetUsageEstimated() {
		t.Error("usage_estimated is set on a reply that reported usage; a consumer " +
			"cannot then tell a measured cost from an inferred one")
	}
	if resp.GetRefused() {
		t.Error("refused is set on an ordinary answer")
	}
	if got, want := resp.GetStopReason(), knov1.StopReason_STOP_REASON_STOP; got != want {
		t.Errorf("stop_reason = %v, want %v", got, want)
	}
	if got, want := resp.GetResolvedModel(), "gpt-5.6-luna-2026-07-01"; got != want {
		t.Errorf("resolved_model = %q, want %q; the ref names an alias and a resume "+
			"compares what actually answered", got, want)
	}
	if got, want := resp.GetProviderBuildId(), "fp_abc123"; got != want {
		t.Errorf("provider_build_id = %q, want %q", got, want)
	}
	if resp.GetError() != "" {
		t.Errorf("error = %q on a successful call", resp.GetError())
	}
	if resp.GetLatencyMs() < 0 {
		t.Errorf("latency_ms = %d, which is not a duration", resp.GetLatencyMs())
	}
}

// TestCachedInputIsBilledBelowFreshInput.
//
// The price is a vector, not a pair, and this is why. Settling every reported
// prompt token at the fresh rate overstates spend systematically — the
// divergence a user notices against their invoice.
func TestCachedInputIsBilledBelowFreshInput(t *testing.T) {
	t.Parallel()

	body := func(cached int) string {
		return `{"model":"m","choices":[{"index":0,"message":{"role":"assistant",` +
			`"content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000000,` +
			`"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":` +
			itoa(cached) + `}}}`
	}

	cost := func(cached int) int64 {
		srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			jsonReply(w, http.StatusOK, body(cached))
		})
		a := newAgent(t, srv)
		resp, err := a.Invoke(t.Context(), newCase("c", "hi"))
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		return resp.GetCostUsdMicros()
	}

	fresh, allCached := cost(0), cost(1_000_000)
	if allCached >= fresh {
		t.Errorf("a fully cached prompt cost %d and a fresh one %d; a cache read is "+
			"billed far below fresh input and settling both at the same rate "+
			"overstates every run's spend", allCached, fresh)
	}
	if want := int64(lunaCachedPerMTok); allCached != want {
		t.Errorf("a million cached tokens cost %d, want %d", allCached, want)
	}
}

// TestAContentFilterRefusalIsAScoredCaseNotAnError.
//
// Getting this backwards is the failure the whole refusal field exists for. An
// account whose safety settings refuse every Case produces 100% scored Cases,
// an aggregate of 0.000, and a CLEAN error rate — a confident-looking reference
// number from a run in which the agent was never measured. The refusal must be
// scored AND flagged, never one or the other.
func TestAContentFilterRefusalIsAScoredCaseNotAnError(t *testing.T) {
	t.Parallel()

	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		jsonReply(w, http.StatusOK, `{"model":"m","choices":[{"index":0,`+
			`"message":{"role":"assistant","content":"I cannot help with that."},`+
			`"finish_reason":"content_filter"}],"usage":{"prompt_tokens":10,`+
			`"completion_tokens":5}}`)
	})
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), newCase("c", "hi"))
	if err != nil {
		t.Fatalf("a refusal was returned as an error: %v. The provider ANSWERED; "+
			"erroring it hides the refusal from the score and inflates the error rate", err)
	}
	if !resp.GetRefused() {
		t.Error("refused is not set. It is authoritative for the run-level refusal " +
			"count, and without it a run of refusals reads as a usable baseline")
	}
	if got, want := resp.GetStopReason(), knov1.StopReason_STOP_REASON_CONTENT_FILTER; got != want {
		t.Errorf("stop_reason = %v, want %v", got, want)
	}
	if resp.GetOutput() == "" {
		t.Error("the refusal text was dropped, so the Goal scores a missing answer " +
			"rather than a declined one")
	}
}

// TestAModelSideRefusalIsFlaggedEvenWhenTheFinishReasonIsOrdinary.
//
// OpenAI expresses the two declines differently: a filtered generation carries
// finish_reason "content_filter", and a model-side decline carries a non-empty
// message.refusal with finish_reason "stop". Deriving the refusal count from
// stop_reason alone would make it depend on which of the two happened.
func TestAModelSideRefusalIsFlaggedEvenWhenTheFinishReasonIsOrdinary(t *testing.T) {
	t.Parallel()

	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		jsonReply(w, http.StatusOK, `{"model":"m","choices":[{"index":0,`+
			`"message":{"role":"assistant","content":"","refusal":"I won't do that."},`+
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	})
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), newCase("c", "hi"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !resp.GetRefused() {
		t.Error("a non-empty message.refusal did not set refused")
	}
	if got := resp.GetOutput(); got != "I won't do that." {
		t.Errorf("output = %q; the decline IS the answer for scoring purposes, and "+
			"an empty output would score as a missing answer instead", got)
	}
}

// TestATruncatedAnswerIsRecordedNotErrored.
//
// finish_reason "length" is a well-formed 200 with valid JSON and an incomplete
// answer. Scored as a wrong answer with nothing recorded, it means Kno's own
// max_output_tokens silently depresses the baseline.
func TestATruncatedAnswerIsRecordedNotErrored(t *testing.T) {
	t.Parallel()

	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		jsonReply(w, http.StatusOK, `{"model":"m","choices":[{"index":0,`+
			`"message":{"role":"assistant","content":"The capital of Fra"},`+
			`"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":8}}`)
	})
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), newCase("c", "hi"))
	if err != nil {
		t.Fatalf("a truncated answer was returned as an error: %v. It is a 200 with "+
			"a real, partial answer; erroring it hides that OUR ceiling cut it off", err)
	}
	if got, want := resp.GetStopReason(), knov1.StopReason_STOP_REASON_LENGTH; got != want {
		t.Errorf("stop_reason = %v, want %v; without it a low score is "+
			"indistinguishable from a wrong answer", got, want)
	}
	if resp.GetRefused() {
		t.Error("a truncation is not a refusal")
	}
}

// TestAReplyWithNoUsageSettlesAtTheReservationNotZero.
//
// A zero settlement is what made a dollar cap unenforceable in M1: the guard
// authorizes against an estimate, and settling zero hands the whole reservation
// back as headroom. Under pessimistic estimation the reservation is a true
// ceiling, so settling there over-charges the budget — the safe direction.
//
// The equality with Estimate is the real assertion. core holds the reservation
// but the STORE persists spendOf(Response), so a settlement that disagreed with
// the reservation would make Guard.Restore under-restore on resume and let a
// resumed run spend that much of its cap twice.
func TestAReplyWithNoUsageSettlesAtTheReservationNotZero(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		usage string
	}{
		{"absent", ""},
		// A block of zeros is a claim that the call was free. Believed, it is
		// the M1 failure reached through a different door.
		{"claims the call was free", `,"usage":{"prompt_tokens":0,"completion_tokens":0}`},
		{"negative counts", `,"usage":{"prompt_tokens":-5,"completion_tokens":-1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				jsonReply(w, http.StatusOK, `{"model":"m","choices":[{"index":0,`+
					`"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]`+
					tc.usage+`}`)
			})
			a := newAgent(t, srv)
			c := newCase("c", strings.Repeat("question ", 40))

			est, err := a.Estimate(t.Context(), c)
			if err != nil {
				t.Fatalf("Estimate: %v", err)
			}
			resp, err := a.Invoke(t.Context(), c)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}

			if !resp.GetUsageEstimated() {
				t.Error("usage_estimated is not set, so a report cannot tell how much " +
					"of its total was inferred rather than measured")
			}
			if resp.GetCostUsdMicros() <= 0 {
				t.Fatalf("cost_usd_micros = %d; a zero settlement hands the whole "+
					"reservation back and makes a dollar cap unenforceable",
					resp.GetCostUsdMicros())
			}
			if got, want := resp.GetCostUsdMicros(), est.CostUSDMicros; got != want {
				t.Errorf("settled %d against a reservation of %d; the guard and the "+
					"store must agree or a resume restores the wrong number", got, want)
			}
		})
	}
}

// TestTheEstimateReservesExactlyOneCall.
//
// One Invoke settles as exactly one call. An Estimate reserving more would
// reserve N and settle 1, and the call cap would drift by (N-1) for every Case.
func TestTheEstimateReservesExactlyOneCall(t *testing.T) {
	t.Parallel()

	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {})
	a := newAgent(t, srv)

	est, err := a.Estimate(t.Context(), newCase("c", "hi"))
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.Calls != 1 {
		t.Errorf("Calls = %d, want 1", est.Calls)
	}
	if est.CostUSDMicros <= 0 {
		t.Errorf("CostUSDMicros = %d; a zero estimate is what core treats as an "+
			"absent answer, not as a cheap Case", est.CostUSDMicros)
	}
}

// TestWorstCaseBoundsEveryEstimateTheAdapterWillProduce.
//
// WorstCase is what core plans concurrency and the consent prompt against. A
// bound that any real Case can exceed is not a bound, and the run would deny
// its way to a halt with money unspent — the exact failure the feasibility
// check exists to prevent.
//
// The property holds because the adapter ENFORCES the prompt ceiling rather
// than assuming it: a Case past MaxPromptBytes is refused by Estimate, so there
// is no Case whose accepted estimate exceeds this number.
func TestWorstCaseBoundsEveryEstimateTheAdapterWillProduce(t *testing.T) {
	t.Parallel()

	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {})
	a := newAgent(t, srv, func(o *openaicompat.Options) {
		o.MaxPromptBytes = 4096
		o.System = "you are a helpful assistant"
	})

	worst := a.WorstCase()
	if worst.CostUSDMicros <= 0 {
		t.Fatalf("WorstCase = %d for a priced model; core would fall back to its "+
			"own scalar and plan against a number this adapter will not reserve",
			worst.CostUSDMicros)
	}

	// Sizes bracketing the ceiling, including the exact boundary and past it.
	for _, size := range []int{0, 1, 1000, 4000, 4068, 4069, 8192, 100000} {
		c := newCase("c", strings.Repeat("z", size))
		c.History = []*knov1.Turn{{Role: knov1.Role_ROLE_USER, Content: "prior"}}

		est, err := a.Estimate(t.Context(), c)
		if err != nil {
			// A refusal is the other half of the bound: what is refused is
			// never sent, so it cannot exceed anything.
			continue
		}
		if est.CostUSDMicros > worst.CostUSDMicros {
			t.Errorf("a %d-byte Case estimated %d against a WorstCase of %d",
				size, est.CostUSDMicros, worst.CostUSDMicros)
		}
	}
}

// TestAPromptPastTheCeilingIsRefusedBeforeAnythingIsSent.
//
// Counted at the server, because the point is that no money moved. The
// provider would reject an oversized prompt with a context-length 400 anyway,
// and docs/debt.md#43 records that whether a provider bills a request it then
// rejects is not something this adapter can observe — so the free refusal is
// strictly better than the paid one.
func TestAPromptPastTheCeilingIsRefusedBeforeAnythingIsSent(t *testing.T) {
	t.Parallel()

	var seen int
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		seen++
		jsonReply(w, http.StatusOK, answeredBody)
	})
	a := newAgent(t, srv, func(o *openaicompat.Options) { o.MaxPromptBytes = 64 })

	c := newCase("c", strings.Repeat("x", 4096))
	if _, err := a.Estimate(t.Context(), c); err == nil {
		t.Error("Estimate accepted a Case past the prompt ceiling, so WorstCase " +
			"would no longer bound it")
	}
	if _, err := a.Invoke(t.Context(), c); err == nil {
		t.Error("Invoke sent a Case past the prompt ceiling")
	}
	if seen != 0 {
		t.Errorf("the server saw %d requests for a Case that should never have been "+
			"sent", seen)
	}
}

// TestAnUnpricedModelIsAnAbsenceNotAZero.
//
// A zero estimate makes a dollar cap unenforceable, so an unpriced model must
// report the absence and let core decide: refuse under a cap, run with a
// warning without one. Inventing a cheap number here would make the cap look
// enforceable when it is not.
func TestAnUnpricedModelIsAnAbsenceNotAZero(t *testing.T) {
	t.Parallel()

	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		jsonReply(w, http.StatusOK, answeredBody)
	})
	ref, err := agentref.Parse("openai:some-self-hosted-model@" + srv.URL)
	if err != nil {
		t.Fatalf("parsing the agent ref: %v", err)
	}
	a, err := openaicompat.New(openaicompat.Options{
		Ref:        ref,
		HTTPClient: srv.Client(),
		Policy:     transport.Policy{AllowInsecureHTTP: true, AllowPrivateAddress: true},
	})
	if err != nil {
		t.Fatalf("New refused an unpriced model. With no cost cap that is a "+
			"legitimate run, so the refusal belongs at Estimate where core knows "+
			"whether a cap is set: %v", err)
	}

	if _, err := a.Estimate(t.Context(), newCase("c", "hi")); !errors.Is(err, pricing.ErrUnpriced) {
		t.Errorf("Estimate error = %v, want ErrUnpriced", err)
	}
	if got := a.WorstCase().CostUSDMicros; got != 0 {
		t.Errorf("WorstCase = %d for an unpriced model; core reads zero as "+
			"'cannot plan' and falls back to its scalar, and any other number "+
			"would be invented", got)
	}

	// It still RUNS: the tokens are measured even though the price is not.
	resp, err := a.Invoke(t.Context(), newCase("c", "hi"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.GetPromptTokens() == 0 {
		t.Error("the provider's token counts were dropped along with the price")
	}
	if !resp.GetUsageEstimated() {
		t.Error("cost_usd_micros is zero because the model is unpriced, and nothing " +
			"says so; a report would add that zero into a total as if it were free")
	}
}

// TestSamplingParametersAreRefusedForAModelThatRejectsThem.
//
// One readable refusal at construction beats N identical 400s the user paid
// for. OpenAI's reasoning models answer any non-default temperature with a 400:
// every Case errors, the error-rate threshold fires, and the user is told "too
// many cases errored for this to be a usable baseline" — naming nothing about
// the cause.
func TestSamplingParametersAreRefusedForAModelThatRejectsThem(t *testing.T) {
	t.Parallel()

	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {})
	ref, err := agentref.Parse("openai:gpt-5.6-sol@" + srv.URL)
	if err != nil {
		t.Fatalf("parsing the agent ref: %v", err)
	}
	zero := 0.0
	_, err = openaicompat.New(openaicompat.Options{
		Ref:         ref,
		HTTPClient:  srv.Client(),
		Policy:      transport.Policy{AllowInsecureHTTP: true, AllowPrivateAddress: true},
		Temperature: &zero,
	})
	if !errors.Is(err, errs.ErrCapabilityUnsupported) {
		t.Fatalf("New error = %v, want ErrCapabilityUnsupported", err)
	}
	if !strings.Contains(err.Error(), "gpt-5.6-sol") {
		t.Errorf("the refusal does not name the model: %v", err)
	}

	// And the override exists, because the matrix is static and a compatible
	// endpoint may legitimately disagree with it.
	yes := true
	if _, err := openaicompat.New(openaicompat.Options{
		Ref:              ref,
		HTTPClient:       srv.Client(),
		Policy:           transport.Policy{AllowInsecureHTTP: true, AllowPrivateAddress: true},
		Temperature:      &zero,
		GenerationParams: &yes,
	}); err != nil {
		t.Errorf("the override was refused: %v", err)
	}
}

// TestTheRequestSaysExactlyWhatWasConfigured.
//
// The output ceiling in particular: sending both spellings makes the request's
// meaning depend on which one the server reads last, and a ceiling the server
// silently drops is not cosmetic — it is the output term of every reservation,
// so the estimate would bound something the request no longer bounds.
func TestTheRequestSaysExactlyWhatWasConfigured(t *testing.T) {
	t.Parallel()

	// io.ReadAll, not one Read into a fixed buffer. A short read is legal at any
	// size — net/http may hand over a chunk boundary — so the buffered version
	// silently truncated the very body it was asserting about, and a test that
	// looks for `"stream":false` near the end would have passed or failed on
	// framing rather than on behaviour.
	//
	// The mutex is not decoration either. The handler runs on the server's
	// goroutine and the assertion reads on the test's; -race flags the
	// unsynchronized version as a data race, and go test -shuffle makes it show
	// up on some runs and not others.
	capture := func(t *testing.T, tweak func(*openaicompat.Options)) string {
		t.Helper()
		var (
			mu   sync.Mutex
			body string
		)
		srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("reading the request body: %v", err)
				return
			}
			mu.Lock()
			body = string(b)
			mu.Unlock()
			jsonReply(w, http.StatusOK, answeredBody)
		})
		a := newAgent(t, srv, tweak)
		c := newCase("c", "hello")
		c.History = []*knov1.Turn{{Role: knov1.Role_ROLE_ASSISTANT, Content: "earlier"}}
		if _, err := a.Invoke(t.Context(), c); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		return body
	}

	t.Run("the modern output ceiling, alone", func(t *testing.T) {
		t.Parallel()
		got := capture(t, func(o *openaicompat.Options) { o.MaxOutputTokens = 77 })
		if !strings.Contains(got, `"max_completion_tokens":77`) {
			t.Errorf("max_completion_tokens is missing from %s", got)
		}
		if strings.Contains(got, `"max_tokens"`) {
			t.Errorf("both spellings were sent: %s", got)
		}
	})

	t.Run("the legacy output ceiling, alone", func(t *testing.T) {
		t.Parallel()
		got := capture(t, func(o *openaicompat.Options) {
			o.MaxOutputTokens = 77
			o.UseLegacyMaxTokens = true
		})
		if !strings.Contains(got, `"max_tokens":77`) {
			t.Errorf("max_tokens is missing from %s", got)
		}
		if strings.Contains(got, `"max_completion_tokens"`) {
			t.Errorf("both spellings were sent: %s", got)
		}
	})

	t.Run("no sampling parameters unless asked", func(t *testing.T) {
		t.Parallel()
		got := capture(t, nil)
		if strings.Contains(got, "temperature") || strings.Contains(got, "seed") {
			t.Errorf("a parameter was sent that nobody configured: %s", got)
		}
	})

	t.Run("temperature zero is sent, not treated as unset", func(t *testing.T) {
		t.Parallel()
		zero, seed, yes := 0.0, int64(7), true
		// The override, because every priced OpenAI row is a gpt-5 model and
		// the static matrix correctly says those reject sampling parameters.
		got := capture(t, func(o *openaicompat.Options) {
			o.Temperature, o.Seed, o.GenerationParams = &zero, &seed, &yes
		})
		if !strings.Contains(got, `"temperature":0`) {
			t.Errorf("temperature 0 was dropped, so nothing pins the sampler: %s", got)
		}
		if !strings.Contains(got, `"seed":7`) {
			t.Errorf("seed is missing from %s", got)
		}
	})

	t.Run("history and the system prompt reach the wire in order", func(t *testing.T) {
		t.Parallel()
		got := capture(t, func(o *openaicompat.Options) { o.System = "be terse" })
		sys := strings.Index(got, "be terse")
		hist := strings.Index(got, "earlier")
		input := strings.Index(got, "hello")
		if sys < 0 || hist < 0 || input < 0 {
			t.Fatalf("a message is missing from %s", got)
		}
		if !(sys < hist && hist < input) {
			t.Errorf("messages are out of order in %s; a reordered conversation is a "+
				"different Case than the one being scored", got)
		}
	})

	t.Run("streaming is stated rather than left to the server", func(t *testing.T) {
		t.Parallel()
		if got := capture(t, nil); !strings.Contains(got, `"stream":false`) {
			t.Errorf("stream is not stated; a server defaulting to SSE would hand "+
				"this adapter a body it reports as malformed: %s", got)
		}
	})
}

// TestCapabilitiesClaimNothingTheAdapterDoesNotImplement.
//
// A declared capability the adapter does not have lets a valuation run report a
// measurement mode it never used, which is worse than reporting a gap.
func TestCapabilitiesClaimNothingTheAdapterDoesNotImplement(t *testing.T) {
	t.Parallel()

	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {})
	a := newAgent(t, srv)
	caps := a.Capabilities()

	if caps.GetContextInject() {
		t.Error("context_inject is declared but core.ContextInjector is not implemented")
	}
	if caps.GetKnowledgeWrite() {
		t.Error("knowledge_write is declared but a Chat Completions endpoint has no index")
	}
	if caps.GetStream() {
		t.Error("stream is declared but streaming is unimplemented (docs/debt.md#35)")
	}
	if !caps.GetTokenCounts() {
		t.Error("token_counts is not declared, but the provider reports usage")
	}

	if _, ok := any(a).(core.ContextInjector); ok != caps.GetContextInject() {
		t.Error("the declared context_inject capability and the implemented " +
			"interface disagree")
	}
}

// TestOnlyTheOpenAISchemeIsServedHere.
//
// A wrong scheme reaching this adapter would send Chat Completions bodies to a
// Messages API endpoint and report the resulting 400s as agent failures.
func TestOnlyTheOpenAISchemeIsServedHere(t *testing.T) {
	t.Parallel()

	ref, err := agentref.Parse("anthropic:claude-sonnet-5")
	if err != nil {
		t.Fatalf("parsing the agent ref: %v", err)
	}
	if _, err := openaicompat.New(openaicompat.Options{Ref: ref}); !errors.Is(err, errs.ErrInvalidInput) {
		t.Errorf("New error = %v, want ErrInvalidInput", err)
	}
	if _, err := openaicompat.New(openaicompat.Options{}); !errors.Is(err, errs.ErrInvalidInput) {
		t.Errorf("New with no ref: error = %v, want ErrInvalidInput", err)
	}
}

// TestTheDefaultKeyVariableDoesNotTravelToAnotherHost.
//
// `openai:llama-3.3-70b@https://api.groq.com/openai/v1` needs GROQ_API_KEY.
// Falling back to OPENAI_API_KEY would mail the user's OpenAI key to a third
// party by following the documented recipe — and the recipe is the milestone's
// headline feature.
//
// Not parallel: t.Setenv and t.Parallel are mutually exclusive.
func TestTheDefaultKeyVariableDoesNotTravelToAnotherHost(t *testing.T) {
	t.Setenv(openaicompat.DefaultKeyEnv, "sk-not-a-real-key-for-tests")

	var got string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		jsonReply(w, http.StatusOK, answeredBody)
	})
	a := newAgent(t, srv)
	if _, err := a.Invoke(t.Context(), newCase("c", "hi")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got != "" {
		t.Errorf("an Authorization header reached a host with no binding. The "+
			"default variable applies only to %s", openaicompat.DefaultBaseURL)
	}

	// Bound explicitly, it does travel — and as a Bearer credential, which is
	// what this shape expects.
	t.Setenv("KNO_TEST_LOCAL_KEY", "sk-bound-to-this-host")
	bindings, err := transport.ParseKeyBindings([]string{
		strings.TrimPrefix(srv.URL, "http://") + "=KNO_TEST_LOCAL_KEY",
	})
	if err != nil {
		t.Fatalf("ParseKeyBindings: %v", err)
	}
	b := newAgent(t, srv, func(o *openaicompat.Options) { o.KeyBindings = bindings })
	if _, err := b.Invoke(t.Context(), newCase("c", "hi")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if want := "Bearer sk-bound-to-this-host"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}

// itoa is strconv.Itoa without the import, kept local so the JSON builders
// above read as JSON.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
