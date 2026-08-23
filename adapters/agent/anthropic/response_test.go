package anthropic_test

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// TestTruncationIsRecordedAsLengthRatherThanScoredAsAWrongAnswer.
//
// stop_reason "max_tokens" is a well-formed 200 with valid JSON and a truncated
// answer. An adapter that drops it lets Kno's own --max-output-tokens depress
// the baseline invisibly: the answer scores as wrong and nothing in the record
// says it was cut off.
func TestTruncationIsRecordedAsLengthRatherThanScoredAsAWrongAnswer(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, `{"id":"m","model":"claude-sonnet-4-6","content":[{"type":"text","text":"the answer is fo"}],
		            "stop_reason":"max_tokens","usage":{"input_tokens":10,"output_tokens":1024}}`)
	})
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), aCase("q"))
	if err != nil {
		t.Fatalf("a truncated answer became an error; it is a well-formed 200: %v", err)
	}
	if resp.GetStopReason() != knov1.StopReason_STOP_REASON_LENGTH {
		t.Errorf("stop_reason = %v, want LENGTH", resp.GetStopReason())
	}
	if resp.GetOutput() != "the answer is fo" {
		t.Errorf("output = %q; the partial answer is the measurement and must survive", resp.GetOutput())
	}
	if resp.GetRefused() {
		t.Error("a truncation was flagged as a refusal")
	}
}

// TestRefusalIsScoredAndFlaggedRatherThanErrored.
//
// Taken as an error, a refusing account produces a run of errored Cases. Taken
// as a plain score, it produces 100% scored Cases, an aggregate of 0.000, and a
// clean error rate — a confident reference number for a run in which the agent
// was never measured. It is both: scored AND flagged.
func TestRefusalIsScoredAndFlaggedRatherThanErrored(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, `{"id":"m","model":"claude-sonnet-4-6","content":[{"type":"text","text":"I can't help with that."}],
		            "stop_reason":"refusal","usage":{"input_tokens":80,"output_tokens":9}}`)
	})
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), aCase("q"))
	if err != nil {
		t.Fatalf("a refusal became an error: %v", err)
	}
	if !resp.GetRefused() {
		t.Error("refused is false; it is the AUTHORITATIVE run-level refusal count, " +
			"and without it a refusing account reports a clean baseline of 0.000")
	}
	if resp.GetStopReason() != knov1.StopReason_STOP_REASON_CONTENT_FILTER {
		t.Errorf("stop_reason = %v, want CONTENT_FILTER", resp.GetStopReason())
	}
	// A mid-output refusal bills what it already produced.
	if resp.GetCostUsdMicros() == 0 {
		t.Error("a refusal that produced output settled at zero; the output was billed")
	}
	if resp.GetUsageEstimated() {
		t.Error("a refusal carrying a real usage block was marked as estimated")
	}
}

// TestARefusalBeforeAnyOutputSettlesAtAMeasuredZero.
//
// Anthropic documents the pre-output refusal as not billed at all — no input
// tokens, no output tokens, no rate-limit consumption. Settling it at the
// pessimistic estimate instead burns a run's entire cost cap on an account that
// declines every Case, having spent nothing.
func TestARefusalBeforeAnyOutputSettlesAtAMeasuredZero(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, `{"id":"m","model":"claude-sonnet-4-6","content":[],
		            "stop_reason":"refusal","usage":{"input_tokens":0,"output_tokens":0}}`)
	})
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), aCase("q"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !resp.GetRefused() {
		t.Error("refused is false on a pre-output refusal")
	}
	if resp.GetCostUsdMicros() != 0 {
		t.Errorf("cost = %d; a pre-output refusal is documented as unbilled",
			resp.GetCostUsdMicros())
	}
	if resp.GetUsageEstimated() {
		t.Error("usage_estimated is true for a zero the provider actually reported; " +
			"it means the number is Kno's prediction, and this one is a measurement")
	}
}

// TestEveryTextBlockReachesTheOutput.
//
// A response legitimately splits into several text blocks. Taking content[0]
// silently truncates the answer, which then scores as a wrong one.
func TestEveryTextBlockReachesTheOutput(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, `{"id":"m","model":"claude-sonnet-4-6","stop_reason":"end_turn",
		            "content":[{"type":"text","text":"one "},
		                       {"type":"thinking","thinking":"ignored"},
		                       {"type":"text","text":"two"}],
		            "usage":{"input_tokens":5,"output_tokens":3}}`)
	})
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), aCase("q"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.GetOutput() != "one two" {
		t.Errorf("output = %q, want %q", resp.GetOutput(), "one two")
	}
}

// TestAToolUseBlockIsRecordedRatherThanDropped.
//
// M2 sends no tools, so a tool_use block means the provider did something we did
// not ask for. Dropped, it reads downstream as an empty answer.
func TestAToolUseBlockIsRecordedRatherThanDropped(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, `{"id":"m","model":"claude-sonnet-4-6","stop_reason":"tool_use",
		            "content":[{"type":"tool_use","name":"search","input":{"q":"x"}}],
		            "usage":{"input_tokens":5,"output_tokens":3}}`)
	})
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), aCase("q"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.GetStopReason() != knov1.StopReason_STOP_REASON_TOOL_CALL {
		t.Errorf("stop_reason = %v, want TOOL_CALL", resp.GetStopReason())
	}
	if len(resp.GetToolCalls()) != 1 || resp.GetToolCalls()[0].GetName() != "search" {
		t.Errorf("tool calls = %v, want one named search", resp.GetToolCalls())
	}
}

// TestTheResolvedModelIsRecorded.
//
// An agent ref like anthropic:claude-sonnet-4-6 is a moving pointer. A run
// interrupted on Monday and resumed on Friday after the alias re-points would
// otherwise blend two models into one aggregate and present it as a single
// homogeneous number.
func TestTheResolvedModelIsRecorded(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) { answer(w, okBody) })
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), aCase("q"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.GetResolvedModel() != "claude-sonnet-4-6-20260101" {
		t.Errorf("resolved_model = %q, want the dated ID the provider reported",
			resp.GetResolvedModel())
	}
	// The dated ID must still price, through the table's longest-prefix match.
	if resp.GetCostUsdMicros() == 0 {
		t.Error("a dated model ID priced at zero; the alias is in the table and " +
			"a pinned version must resolve to it")
	}
}

// TestAnUnknownStopReasonIsUnspecifiedRatherThanStop.
//
// Claiming STOP for a value we do not recognize would hide a future truncation
// behind a plausible-looking record.
func TestAnUnknownStopReasonIsUnspecifiedRatherThanStop(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, `{"id":"m","model":"claude-sonnet-4-6","stop_reason":"something_new",
		            "content":[{"type":"text","text":"x"}],
		            "usage":{"input_tokens":5,"output_tokens":1}}`)
	})
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), aCase("q"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.GetStopReason() != knov1.StopReason_STOP_REASON_UNSPECIFIED {
		t.Errorf("stop_reason = %v, want UNSPECIFIED", resp.GetStopReason())
	}
}

// TestNoProviderBuildIDIsInvented.
//
// Anthropic reports no backend build identifier. Filling the field with the
// per-request message id would make the Run's SET of observed builds one entry
// per Case — a field that says nothing, at the size of the run.
func TestNoProviderBuildIDIsInvented(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) { answer(w, okBody) })
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), aCase("q"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.GetProviderBuildId() != "" {
		t.Errorf("provider_build_id = %q; Anthropic reports none, and the message "+
			"id is per request rather than per build", resp.GetProviderBuildId())
	}
}

// TestLatencyExcludesTheRateLimitWait.
//
// A run held back by a Retry-After would otherwise report the backoff as the
// provider's latency, and every latency Goal downstream would be measuring
// Kno's own waiting rather than the agent.
//
// The wait is INDUCED rather than assumed: the first call takes a 429 with a
// one-second Retry-After, which closes the host in the limiter, so the second
// call blocks inside the transport before its request goes out. Asserting
// LatencyMs >= 0 against a value the code clamps to >= 0 tests nothing, which
// is what this used to do.
func TestLatencyExcludesTheRateLimitWait(t *testing.T) {
	t.Parallel()

	var n atomic.Int64
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`))
			return
		}
		answer(w, okBody)
	})
	a := newAgent(t, srv)

	// Closes the host for a second.
	if _, err := a.Invoke(t.Context(), aCase("q")); err == nil {
		t.Fatal("the first call was expected to be rate limited")
	}

	start := time.Now()
	resp, err := a.Invoke(t.Context(), aCase("q"))
	wall := time.Since(start)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if wall < 500*time.Millisecond {
		t.Fatalf("the second call took %v, so no rate-limit wait was induced and "+
			"this test proves nothing", wall)
	}
	if resp.GetLatencyMs() >= 500 {
		t.Errorf("latency_ms = %d after a %v wall-clock call; the limiter's wait was "+
			"reported as the provider's latency", resp.GetLatencyMs(), wall)
	}
}
