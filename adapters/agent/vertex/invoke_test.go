package vertex

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// TestInvokeSuccess asserts the happy path end to end: the exact wire body,
// the bearer header, the %40-free path, and the settlement arithmetic with
// the regional multiplier — 3525 input + 3000 output = 6525, +10% = 7178.
func TestInvokeSuccess(t *testing.T) {
	t.Parallel()

	a, rec := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/kno-test-proj/locations/us-central1/publishers/anthropic/models/claude-sonnet-4-5:rawPredict" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer ya29.test-token" {
			t.Errorf("Authorization = %q", auth)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		io.WriteString(w, rawPredictOK)
	})

	out, err := a.Invoke(context.Background(), aCase("c1", "hello"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out.Output != "hello from vertex" {
		t.Errorf("Output = %q", out.Output)
	}
	if out.StopReason != knov1.StopReason_STOP_REASON_STOP {
		t.Errorf("StopReason = %v", out.StopReason)
	}
	if out.Refused {
		t.Error("a normal answer is refused")
	}
	if out.ProviderBuildId != "" {
		t.Errorf("ProviderBuildId = %q, want empty", out.ProviderBuildId)
	}
	if out.PromptTokens != 1600 {
		t.Errorf("PromptTokens = %d, want 1600", out.PromptTokens)
	}
	if out.CompletionTokens != 200 {
		t.Errorf("CompletionTokens = %d, want 200", out.CompletionTokens)
	}
	if out.CachedTokens != 500 {
		t.Errorf("CachedTokens = %d, want 500", out.CachedTokens)
	}
	if out.CostUsdMicros != 7_178 {
		t.Errorf("CostUsdMicros = %d, want 7178 (6525 + 10%% regional)", out.CostUsdMicros)
	}
	if out.UsageEstimated {
		t.Error("a measured usage block is marked estimated")
	}
	if rec.calls() != 1 {
		t.Errorf("calls = %d, want 1", rec.calls())
	}
	if a.RoundTrips() != 1 {
		t.Errorf("RoundTrips = %d, want 1", a.RoundTrips())
	}

	body := rec.body(t, 0)
	want := `{"anthropic_version":"vertex-2023-10-16","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`
	if string(body) != want {
		t.Errorf("body:\n got %s\nwant %s", body, want)
	}
}

// TestInvokeARNModelPath asserts the @ pin of a dated model id reaches the
// router percent-encoded, asserted on RawPath because the Go server decodes
// Path before the handler sees it.
func TestInvokeDatedModelPath(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{Model: "claude-3-5-sonnet@20240620"}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawPath != "/v1/projects/kno-test-proj/locations/us-central1/publishers/anthropic/models/claude-3-5-sonnet%4020240620:rawPredict" {
			t.Errorf("RawPath = %q", r.URL.RawPath)
		}
		io.WriteString(w, rawPredictOK)
	})

	if _, err := a.Invoke(context.Background(), aCase("c1", "hello")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
}

// TestInvokeToolUse asserts a tool_use block is recorded and the stop reason
// maps to TOOL_CALL.
func TestInvokeToolUse(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{
			"content": [{"type":"tool_use","name":"lookup","input":{"q":"x"}},{"type":"text","text":"done"}],
			"stop_reason": "tool_use",
			"usage": {"input_tokens": 10, "output_tokens": 20}
		}`)
	})

	out, err := a.Invoke(context.Background(), aCase("c1", "look it up"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(out.ToolCalls) != 1 || out.ToolCalls[0].Name != "lookup" || out.ToolCalls[0].Arguments != `{"q":"x"}` {
		t.Errorf("ToolCalls = %+v", out.ToolCalls)
	}
	if out.StopReason != knov1.StopReason_STOP_REASON_TOOL_CALL {
		t.Errorf("StopReason = %v", out.StopReason)
	}
}

// TestInvokeRefusal asserts a refusal stop reason is a SCORED Case: flagged,
// mapped to CONTENT_FILTER, and not an error.
func TestInvokeRefusal(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{
			"content": [{"type":"text","text":"I cannot answer that"}],
			"stop_reason": "refusal",
			"usage": {"input_tokens": 42, "output_tokens": 0}
		}`)
	})

	out, err := a.Invoke(context.Background(), aCase("c1", "jailbreak me"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !out.Refused {
		t.Error("a refusal is not flagged")
	}
	if out.StopReason != knov1.StopReason_STOP_REASON_CONTENT_FILTER {
		t.Errorf("StopReason = %v", out.StopReason)
	}
	if out.CostUsdMicros <= 0 {
		t.Error("a refusal settles at zero cost")
	}
}

// TestInvokeUsageAbsent asserts the estimate settlement: no usage block means
// usage_estimated and the regional reservation charged, never zero.
func TestInvokeUsageAbsent(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`)
	})

	out, err := a.Invoke(context.Background(), aCase("c1", "hello"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !out.UsageEstimated {
		t.Error("an estimated settlement is not marked estimated")
	}
	if out.CostUsdMicros <= 0 {
		t.Errorf("CostUsdMicros = %d, want > 0", out.CostUsdMicros)
	}
	if out.PromptTokens != 0 {
		t.Errorf("PromptTokens = %d, want 0", out.PromptTokens)
	}
}

// TestInvokeUsageUnusable asserts a block that disagrees with the body
// settles as absent — an estimate, flagged.
func TestInvokeUsageUnusable(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{
			"content": [{"type":"text","text":"full answer"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 1000, "output_tokens": 0}
		}`)
	})

	out, err := a.Invoke(context.Background(), aCase("c1", "hello"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !out.UsageEstimated {
		t.Error("an unusable block is not settled as estimated")
	}
	if out.CostUsdMicros <= 0 {
		t.Errorf("CostUsdMicros = %d, want > 0", out.CostUsdMicros)
	}
}

// TestInvokeUnpricedNoCap asserts the settlement leaves zero when the model
// has no row AND no cost cap forces the refusal earlier — the one sanctioned
// zero.
func TestInvokeUnpricedNoCap(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{Model: "claude-nowhere-1"}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, rawPredictOK)
	})

	out, err := a.Invoke(context.Background(), aCase("c1", "hello"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out.UsageEstimated {
		t.Error("the tokens are measured, so this must not be marked estimated")
	}
	if out.PromptTokens != 1600 {
		t.Errorf("PromptTokens = %d, want 1600", out.PromptTokens)
	}
	if out.CostUsdMicros != 0 {
		t.Errorf("CostUsdMicros = %d, want 0 — measured tokens with no rate", out.CostUsdMicros)
	}
}

// TestInvokeRateLimited asserts a 429 carries the provider's wait for core's
// backoff and classifies as ErrRateLimited.
func TestInvokeRateLimited(t *testing.T) {
	t.Parallel()

	a, rec := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		http.Error(w, `{"error":{"code":429,"message":"rate limit","status":"RESOURCE_EXHAUSTED"}}`, http.StatusTooManyRequests)
	})

	out, err := a.Invoke(context.Background(), aCase("c1", "hello"))
	if err == nil {
		t.Fatal("Invoke succeeded on a 429")
	}
	if !errors.Is(err, errs.ErrRateLimited) {
		t.Errorf("err = %v, want ErrRateLimited", err)
	}
	after, ok := retryAfterOf(err)
	if !ok || after != 42*time.Second {
		t.Errorf("RetryAfter = %v, %v", after, ok)
	}
	if out == nil || out.Error == "" {
		t.Error("the failure Response carries no Error text")
	}
	if rec.calls() != 1 {
		t.Errorf("calls = %d, want 1", rec.calls())
	}
}

// TestInvokeUnauthorized asserts a 401 is run-fatal authentication.
func TestInvokeUnauthorized(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":401,"message":"Request had invalid authentication credentials","status":"UNAUTHENTICATED"}}`, http.StatusUnauthorized)
	})

	out, err := a.Invoke(context.Background(), aCase("c1", "hello"))
	if !errors.Is(err, ErrAuthentication) {
		t.Fatalf("err = %v, want ErrAuthentication", err)
	}
	if !runFatal(err) {
		t.Errorf("err is not run-fatal: %v", err)
	}
	if out == nil {
		t.Fatal("failure Response is nil")
	}
	if !strings.Contains(out.Error, "UNAUTHENTICATED") {
		t.Errorf("Error = %q", out.Error)
	}
}

// TestInvokeForbidden asserts a 403 is BOTH causes: rejected token or denied
// model, and the fix says so.
func TestInvokeForbidden(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":403,"message":"Permission denied","status":"PERMISSION_DENIED"}}`, http.StatusForbidden)
	})

	_, err := a.Invoke(context.Background(), aCase("c1", "hello"))
	if !errors.Is(err, ErrAuthentication) {
		t.Fatalf("err = %v, want ErrAuthentication", err)
	}
	var ae *errs.Actionable
	if !errors.As(err, &ae) {
		t.Fatalf("err is not Actionable: %v", err)
	}
	for _, want := range []string{"access token was rejected", "Vertex AI User"} {
		if !strings.Contains(ae.Fix, want) {
			t.Errorf("fix = %q, want it to contain %q", ae.Fix, want)
		}
	}
}

// TestInvokeModelGarden asserts a 404 with NOT_FOUND status names the Model
// Garden terms opt-in — the endpoint's way of saying the terms are not
// accepted — rather than a phantom endpoint.
func TestInvokeModelGarden(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":404,"message":"models/claude-sonnet-4-5 not found","status":"NOT_FOUND"}}`, http.StatusNotFound)
	})

	_, err := a.Invoke(context.Background(), aCase("c1", "hello"))
	if !errors.Is(err, ErrProvider) {
		t.Fatalf("err = %v, want ErrProvider", err)
	}
	if !runFatal(err) {
		t.Errorf("err is not run-fatal: %v", err)
	}
	var ae *errs.Actionable
	if !errors.As(err, &ae) {
		t.Fatalf("err is not Actionable: %v", err)
	}
	for _, want := range []string{"Model Garden terms", "publishers/anthropic/models/*"} {
		if !strings.Contains(ae.Fix, want) {
			t.Errorf("fix = %q, want it to contain %q", ae.Fix, want)
		}
	}
}

// TestInvokeNotFound asserts a 404 without NOT_FOUND names the catalog.
func TestInvokeNotFound(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":404,"message":"no such model in this location","status":""}}`, http.StatusNotFound)
	})

	_, err := a.Invoke(context.Background(), aCase("c1", "hello"))
	if !errors.Is(err, ErrProvider) {
		t.Fatalf("err = %v, want ErrProvider", err)
	}
	var ae *errs.Actionable
	if !errors.As(err, &ae) {
		t.Fatalf("err is not Actionable: %v", err)
	}
	for _, want := range []string{"model id", "Model Garden catalog", "region"} {
		if !strings.Contains(ae.Fix, want) {
			t.Errorf("fix = %q, want it to contain %q", ae.Fix, want)
		}
	}
}

// TestInvokePromptTooLong asserts a 413 is a provider error with a fixing
// line, not a retry.
func TestInvokePromptTooLong(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":413,"message":"request too large","status":"INVALID_ARGUMENT"}}`, http.StatusRequestEntityTooLarge)
	})

	_, err := a.Invoke(context.Background(), aCase("c1", "hello"))
	if !errors.Is(err, ErrProvider) {
		t.Fatalf("err = %v, want ErrProvider", err)
	}
	var ae *errs.Actionable
	if !errors.As(err, &ae) {
		t.Fatalf("err is not Actionable: %v", err)
	}
	if !strings.Contains(ae.Fix, "context window") {
		t.Errorf("fix = %q, want the context-window fix", ae.Fix)
	}
}

// TestInvokeServerError asserts a 5xx is transient.
func TestInvokeServerError(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":500,"message":"boom","status":"INTERNAL"}}`, http.StatusInternalServerError)
	})

	_, err := a.Invoke(context.Background(), aCase("c1", "hello"))
	if !errors.Is(err, errs.ErrTransportTransient) {
		t.Errorf("err = %v, want ErrTransportTransient", err)
	}
}

// TestInvokeMalformed200 asserts a 200 that is not a Messages response is
// TERMINAL: the provider answered, so it billed, and a retry pays twice.
func TestInvokeMalformed200(t *testing.T) {
	t.Parallel()

	a, rec := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<html>gateway error</html>`)
	})

	_, err := a.Invoke(context.Background(), aCase("c1", "hello"))
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("err = %v, want ErrMalformedResponse", err)
	}
	if rec.calls() != 1 {
		t.Errorf("calls = %d, want 1 (no retry under the adapter)", rec.calls())
	}
}

// TestInvokeRedirectRefused asserts a redirect off the fixed endpoint is
// refused, never followed.
func TestInvokeRedirectRefused(t *testing.T) {
	t.Parallel()

	other := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a redirected request reached another host")
	})
	a, _ := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	})

	_, err := a.Invoke(context.Background(), aCase("c1", "hello"))
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
	if !runFatal(err) {
		t.Errorf("err is not run-fatal: %v", err)
	}
}

// TestInvokeMaxTokens asserts max_tokens stop maps to LENGTH.
func TestInvokeMaxTokens(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{
			"content": [{"type":"text","text":"truncated"}],
			"stop_reason": "max_tokens",
			"usage": {"input_tokens": 10, "output_tokens": 1024}
		}`)
	})

	out, err := a.Invoke(context.Background(), aCase("c1", "hello"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out.StopReason != knov1.StopReason_STOP_REASON_LENGTH {
		t.Errorf("StopReason = %v", out.StopReason)
	}
}

// TestCompose asserts the turn normalization: system turns join the system
// string, tool results ride in user turns, repeated roles merge, and the
// first turn must be a user.
func TestCompose(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{System: "sys", MaxOutputTokens: 10}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, rawPredictOK)
	})

	system, msgs, err := a.compose(aHistory([]*knov1.Turn{
		{Role: knov1.Role_ROLE_SYSTEM, Content: "more sys"},
		{Role: knov1.Role_ROLE_USER, Content: "u1"},
		{Role: knov1.Role_ROLE_TOOL, Content: "tool result"},
		{Role: knov1.Role_ROLE_ASSISTANT, Content: "a1"},
	}, "final input"))
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if system != "sys\n\nmore sys" {
		t.Errorf("system = %q", system)
	}
	want := []rawMessage{
		{Role: roleUser, Content: "u1\n\ntool result"},
		{Role: roleAssistant, Content: "a1"},
		{Role: roleUser, Content: "final input"},
	}
	if len(msgs) != len(want) {
		t.Fatalf("messages = %+v, want %+v", msgs, want)
	}
	for i := range want {
		if msgs[i] != want[i] {
			t.Errorf("message %d = %+v, want %+v", i, msgs[i], want[i])
		}
	}
}

// TestComposeRefusals asserts the Cases this adapter refuses before any
// request goes out.
func TestComposeRefusals(t *testing.T) {
	t.Parallel()

	a, rec := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a refused Case reached the network")
	})

	tests := []struct {
		name string
		c    *core.Case
		want string
	}{
		{"nil case", nil, "vertex: nil case"},
		{"empty input", aCase("c1", ""), "no input"},
		{"assistant first", aHistory([]*knov1.Turn{
			{Role: knov1.Role_ROLE_ASSISTANT, Content: "a1"},
		}, "in"), "begins with an assistant turn"},
		{"roleless turn", aHistory([]*knov1.Turn{
			{Role: knov1.Role_ROLE_UNSPECIFIED, Content: "?"},
		}, "in"), "has no role"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			_, err := a.Invoke(context.Background(), tt.c)
			if err == nil {
				t.Fatalf("Invoke succeeded, want %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %q, want it to contain %q", err, tt.want)
			}
			if tt.c != nil && !errors.Is(err, errs.ErrInvalidInput) {
				t.Errorf("err = %v, want ErrInvalidInput", err)
			}
		})
	}
	if rec.calls() != 0 {
		t.Errorf("calls = %d, want 0", rec.calls())
	}
}

// TestCapabilities asserts the static surface, with no seed.
func TestCapabilities(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, rawPredictOK)
	})
	caps := a.Capabilities()
	if !caps.ContextInject || !caps.TokenCounts || !caps.GenerationParams {
		t.Errorf("caps = %+v", caps)
	}
	if caps.Stream {
		t.Error("rawPredict is not streamed")
	}
}

// TestNewRefusals asserts construction-time refusals happen before anything
// can be sent.
func TestNewRefusals(t *testing.T) {
	t.Parallel()

	if _, err := New(Options{}); err == nil {
		t.Error("New with no model succeeded")
	}
	if _, err := New(Options{Model: testModel}); err == nil {
		t.Error("New with no output ceiling succeeded")
	}
	t0 := 0.5
	if _, err := New(Options{Model: "claude-fable-5", MaxOutputTokens: 100, Temperature: &t0}); err == nil {
		t.Error("New with temperature on a sampling-removed model succeeded")
	}
	neg := -0.1
	if _, err := New(Options{Model: testModel, MaxOutputTokens: 100, Temperature: &neg}); err == nil {
		t.Error("New with a negative temperature succeeded")
	}
}

// TestRegionField asserts Region reports what the endpoint was built from.
func TestRegionField(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, rawPredictOK)
	})
	if a.Region() != "us-central1" {
		t.Errorf("Region = %q", a.Region())
	}
	if a.Model() != testModel {
		t.Errorf("Model = %q", a.Model())
	}
	if !strings.Contains(a.BaseURL(), "us-central1-aiplatform.googleapis.com") {
		t.Errorf("BaseURL = %q", a.BaseURL())
	}
}

func runFatal(err error) bool {
	var rf interface{ RunFatal() bool }
	return errors.As(err, &rf) && rf.RunFatal()
}
