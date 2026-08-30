package bedrock

// This file exercises the adapter's behavior at the wire: what goes out, what
// comes back, and — for the three behaviors that carry money — which of them
// happens exactly once. The skew retry is the exception that proves the
// budget rule: it is bounded to a single extra request for the whole Agent,
// and the tests below pin both the bound and the counting.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// TestInvokeSuccess pins the happy path end to end: what the server saw, and
// what the caller got.
func TestInvokeSuccess(t *testing.T) {
	t.Parallel()

	a, rec := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(converseOK))
		})

	resp, err := a.Invoke(t.Context(), aCase("c1", "hello"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	// What came back.
	if resp.Output != "hello from bedrock" {
		t.Errorf("Output = %q", resp.Output)
	}
	if resp.StopReason != knov1.StopReason_STOP_REASON_STOP {
		t.Errorf("StopReason = %v, want STOP", resp.StopReason)
	}
	if resp.Refused {
		t.Error("an end_turn answer is not a refusal")
	}
	if resp.CostUsdMicros != 7_178 {
		t.Errorf("CostUsdMicros = %d, want 7_178", resp.CostUsdMicros)
	}
	if resp.PromptTokens != 1_600 {
		t.Errorf("PromptTokens = %d, want 1_600", resp.PromptTokens)
	}
	if resp.CompletionTokens != 200 {
		t.Errorf("CompletionTokens = %d, want 200", resp.CompletionTokens)
	}
	if resp.UsageEstimated {
		t.Error("a measured usage block must not be flagged as an estimate")
	}
	if resp.ProviderBuildId != "" {
		t.Errorf("ProviderBuildId = %q, want empty — Converse reports no build id", resp.ProviderBuildId)
	}

	// What went out.
	if got := rec.calls(); got != 1 {
		t.Fatalf("the server saw %d requests, want exactly 1", got)
	}
	body := string(rec.body(t, 0))
	for _, want := range []string{
		`"messages":[{"role":"user","content":[{"text":"hello"}]}]`,
		`"inferenceConfig":{"maxTokens":1024}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("request body does not contain %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, `"seed"`) {
		t.Errorf("the request carries a seed parameter Converse does not support (plan P0-3):\n%s", body)
	}
	if strings.Contains(body, `"system"`) {
		t.Errorf("a Case with no system content must not send a system array:\n%s", body)
	}

	// The signature's artifacts, on the wire.
	hdr := rec.header(t, 0)
	if hdr.Get("Authorization") == "" {
		t.Error("no Authorization header — the request went out unsigned")
	}
	if hdr.Get("X-Amz-Date") == "" {
		t.Error("no x-amz-date stamp")
	}
	if hdr.Get("X-Amz-Security-Token") != "" {
		t.Error("a non-STS credential must not send a session token")
	}
	if hdr.Get("X-Amz-Content-Sha256") == "" {
		t.Error("no payload hash")
	}
}

// TestInvokeSendsSystemArrayAndHistory pins the request shape for the parts
// of a Case that are not the bare input: the system array (configured plus
// history roles), and role merging.
func TestInvokeSendsSystemArrayAndHistory(t *testing.T) {
	t.Parallel()

	a, rec := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024, System: "S"},
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(converseOK))
		})

	c := aHistory([]*knov1.Turn{
		turn(knov1.Role_ROLE_SYSTEM, "sys"),
		turn(knov1.Role_ROLE_USER, "a"),
		turn(knov1.Role_ROLE_ASSISTANT, "b"),
		turn(knov1.Role_ROLE_USER, "c"),
		turn(knov1.Role_ROLE_TOOL, "tool result"),
	}, "in")

	if _, err := a.Invoke(t.Context(), c); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	body := string(rec.body(t, 0))
	for _, want := range []string{
		`"system":[{"text":"S"},{"text":"sys"}]`,
		`{"role":"user","content":[{"text":"a"}]}`,
		`{"role":"assistant","content":[{"text":"b"}]}`,
		// The repeated user turn, the tool result, and the input merge into
		// one user message, each joined with a blank line — Converse forbids
		// two user turns back to back, and the anthropic adapter merges the
		// same way.
		`{"role":"user","content":[{"text":"c\n\ntool result\n\nin"}]}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("request body does not contain %s:\n%s", want, body)
		}
	}
}

// TestInvokeARNModelPath pins the percent-encoded ARN on the wire. This is
// the pairing the pinned signature test holds: the URL and the canonical URI
// must agree byte for byte, and the server must see the %3A form.
func TestInvokeARNModelPath(t *testing.T) {
	t.Parallel()

	model := "arn:aws:bedrock:us-east-1:123456789012:foundation-model/anthropic.claude-sonnet-4-5-20250929-v1:0"

	// The server decodes Path; RawPath holds the request line's form, which
	// is what the signer hashed.
	var rawPath string
	a, _ := newAgent(t, Options{Model: model, MaxOutputTokens: 1024},
		func(w http.ResponseWriter, r *http.Request) {
			rawPath = r.URL.RawPath
			_, _ = w.Write([]byte(converseOK))
		})

	if _, err := a.Invoke(t.Context(), aCase("c1", "hi")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	want := "/model/arn%3Aaws%3Abedrock%3Aus-east-1%3A123456789012%3Afoundation-model/anthropic.claude-sonnet-4-5-20250929-v1%3A0/converse"
	if rawPath != want {
		t.Errorf("wire path = %q, want the %%-encoded ARN %q", rawPath, want)
	}
}

// TestInvokeRefusedContentFilter pins that a safety refusal is a SCORED Case,
// never an error.
func TestInvokeRefusedContentFilter(t *testing.T) {
	t.Parallel()

	body := `{"output":{"message":{"role":"assistant","content":[{"type":"text","text":""}]}},"stopReason":"content_filtered","usage":{"inputTokens":10,"outputTokens":0,"totalTokens":10}}`
	a, _ := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024},
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		})

	resp, err := a.Invoke(t.Context(), aCase("c1", "hello"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !resp.Refused {
		t.Error("content_filtered must set Refused")
	}
	if resp.StopReason != knov1.StopReason_STOP_REASON_CONTENT_FILTER {
		t.Errorf("StopReason = %v, want CONTENT_FILTER", resp.StopReason)
	}
}

// TestInvokeRefusedGuardrail pins the guardrail_intervened stop reason.
func TestInvokeRefusedGuardrail(t *testing.T) {
	t.Parallel()

	body := `{"output":{"message":{"role":"assistant","content":[{"type":"text","text":""}]}},"stopReason":"guardrail_intervened","usage":{"inputTokens":10,"outputTokens":0,"totalTokens":10}}`
	a, _ := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024},
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		})

	resp, err := a.Invoke(t.Context(), aCase("c1", "hello"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !resp.Refused {
		t.Error("guardrail_intervened must set Refused")
	}
}

// TestInvokeSkewRetriesOnceWithAFreshStamp pins the clock-skew path: a 403
// whose body names the clock gets ONE retry with a fresh x-amz-date, and the
// fresh stamp is what the retry signs with.
func TestInvokeSkewRetriesOnceWithAFreshStamp(t *testing.T) {
	t.Parallel()

	stamp := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	next := 0
	clk := func() time.Time {
		next++
		if next == 1 {
			return stamp
		}
		return stamp.Add(2 * time.Minute)
	}

	seen := 0
	a, rec := newAgent(t, Options{
		Model:           testModel,
		MaxOutputTokens: 1024,
		now:             clk,
	}, func(w http.ResponseWriter, r *http.Request) {
		seen++
		if seen == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Request was rejected because the date was skewed beyond the allowed 15 minutes","type":"ExpiredTokenException"}`))
			return
		}
		_, _ = w.Write([]byte(converseOK))
	})

	resp, err := a.Invoke(t.Context(), aCase("c1", "hello"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Output != "hello from bedrock" {
		t.Errorf("Output = %q", resp.Output)
	}
	if got := rec.calls(); got != 2 {
		t.Errorf("the server saw %d requests, want exactly 2 (one skew, one retry)", got)
	}
	d1 := rec.header(t, 0).Get("X-Amz-Date")
	d2 := rec.header(t, 1).Get("X-Amz-Date")
	if d1 == d2 {
		t.Errorf("the retry re-stamped x-amz-date %q — a replay, not a fresh signature", d1)
	}
	if a.RoundTrips() != 2 {
		t.Errorf("RoundTrips = %d, want 2", a.RoundTrips())
	}
}

// TestInvokeSkewRetryIsOncePerAgent pins the bound: a skewing clock skews
// every call, so the retry budget is per-Agent — one extra request ever, and
// the second Case gets the honest terminal 403.
func TestInvokeSkewRetryIsOncePerAgent(t *testing.T) {
	t.Parallel()

	a, rec := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"clock skew detected","type":"ExpiredTokenException"}`))
		})

	// Case 1: burns the retry, then gets the terminal 403.
	_, err := a.Invoke(t.Context(), aCase("c1", "hello"))
	if !errors.Is(err, ErrAuthentication) {
		t.Fatalf("case 1 error = %v, want ErrAuthentication", err)
	}
	if !runFatal(err) {
		t.Error("a second 403 is run-fatal — the credential is read once and does not rotate mid-run")
	}
	if !strings.Contains(err.Error(), "clock is skewed") {
		t.Errorf("the terminal fix must name the clock, got: %v", err)
	}

	// Case 2: no retry left — one request, same terminal error.
	_, err = a.Invoke(t.Context(), aCase("c2", "hello"))
	if !errors.Is(err, ErrAuthentication) {
		t.Fatalf("case 2 error = %v, want ErrAuthentication", err)
	}
	if got := rec.calls(); got != 3 {
		t.Errorf("the server saw %d requests, want 3 (1 + 1 retry + 1)", got)
	}
}

// TestInvokeForbiddenWithoutSkewDoesNotRetry pins the distinction that keeps
// a rejected credential from burning the retry budget.
func TestInvokeForbiddenWithoutSkewDoesNotRetry(t *testing.T) {
	t.Parallel()

	a, rec := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Access denied","type":"AccessDeniedException"}`))
		})

	_, err := a.Invoke(t.Context(), aCase("c1", "hello"))
	if !errors.Is(err, ErrAuthentication) {
		t.Fatalf("error = %v, want ErrAuthentication", err)
	}
	if got := rec.calls(); got != 1 {
		t.Errorf("the server saw %d requests, want 1 — a non-skew 403 must not retry", got)
	}
}

// TestInvokeUnauthorizedIsRunFatal pins the 401 path.
func TestInvokeUnauthorizedIsRunFatal(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials","type":"UnrecognizedClientException"}`))
		})

	_, err := a.Invoke(t.Context(), aCase("c1", "hello"))
	if !errors.Is(err, ErrAuthentication) {
		t.Fatalf("error = %v, want ErrAuthentication", err)
	}
	if !runFatal(err) {
		t.Error("a rejected credential is run-fatal")
	}
}

// TestInvokeNotFoundNamesTheModel pins the 404 path.
func TestInvokeNotFoundNamesTheModel(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"The model is not found","type":"ResourceNotFoundException"}`))
		})

	_, err := a.Invoke(t.Context(), aCase("c1", "hello"))
	if !errors.Is(err, ErrProvider) {
		t.Fatalf("error = %v, want ErrProvider", err)
	}
	if !runFatal(err) {
		t.Error("a model id that does not resolve is run-fatal")
	}
	if !strings.Contains(err.Error(), "model id") {
		t.Errorf("the fix must point at the model id, got: %v", err)
	}
}

// TestInvokeRateLimitedPinsRetryAfter pins that a 429 carries the provider's
// wait, in the shape core's backoff reads.
func TestInvokeRateLimitedPinsRetryAfter(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"Throttled","type":"ThrottlingException"}`))
		})

	_, err := a.Invoke(t.Context(), aCase("c1", "hello"))
	if !errors.Is(err, errs.ErrRateLimited) {
		t.Fatalf("error = %v, want ErrRateLimited", err)
	}
	after, ok := retryAfterOf(err)
	if !ok || after < 30*time.Second {
		t.Errorf("RetryAfter = %v, want >= 30s from the provider's header", after)
	}
	if runFatal(err) {
		t.Error("a rate limit is not run-fatal — it clears")
	}
}

// TestInvokeServerErrorIsTransient pins the 5xx path.
func TestInvokeServerErrorIsTransient(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom","type":"InternalServerException"}`))
		})

	_, err := a.Invoke(t.Context(), aCase("c1", "hello"))
	if !errors.Is(err, errs.ErrTransportTransient) {
		t.Fatalf("error = %v, want ErrTransportTransient", err)
	}
}

// TestInvokeMalformedResponseIsTerminal pins that a 200 that is not Converse
// JSON is TERMINAL, not transient: the provider answered, so it billed, and
// retrying pays twice for one answer.
func TestInvokeMalformedResponseIsTerminal(t *testing.T) {
	t.Parallel()

	a, rec := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024},
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`<html>not json</html>`))
		})

	_, err := a.Invoke(t.Context(), aCase("c1", "hello"))
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("error = %v, want ErrMalformedResponse", err)
	}
	if runFatal(err) {
		t.Error("a malformed 200 is terminal for the Case, not the run")
	}
	if got := rec.calls(); got != 1 {
		t.Errorf("the server saw %d requests, want exactly 1", got)
	}
}

// TestInvokeRefusesRedirectsOffTheEndpoint pins that a cross-host redirect is
// refused, and that the refusal is run-fatal — the policy that refused this
// request refuses every one after it.
func TestInvokeRefusesRedirectsOffTheEndpoint(t *testing.T) {
	t.Parallel()

	// A second server plays the redirect's target.
	other := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a request reached the redirect target")
	})

	a, _ := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024},
		func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+"/model/x/converse", http.StatusFound)
		})

	_, err := a.Invoke(t.Context(), aCase("c1", "hello"))
	if !runFatal(err) {
		t.Fatalf("error = %v, want run-fatal", err)
	}
	if !strings.Contains(err.Error(), "does not follow redirects off it") {
		t.Errorf("the refusal must say the endpoint is fixed, got: %v", err)
	}
}

// TestInvokeUsageAbsentSettlesFromEstimate pins the estimated settlement:
// never zero, and never silent about being an estimate.
func TestInvokeUsageAbsentSettlesFromEstimate(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024},
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"output":{"message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}},"stopReason":"end_turn"}`))
		})

	resp, err := a.Invoke(t.Context(), aCase("c1", "hello"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !resp.UsageEstimated {
		t.Error("a settlement without measured usage must say so")
	}
	if resp.CostUsdMicros <= 0 {
		t.Error("the settlement must never be zero")
	}
	est, err := a.Estimate(t.Context(), aCase("c1", "hello"))
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if resp.CostUsdMicros != est.CostUSDMicros {
		t.Errorf("settled %d, want the reservation %d", resp.CostUsdMicros, est.CostUSDMicros)
	}
}

// TestInvokeRecordsToolCalls pins that an unrequested toolUse block is
// recorded rather than silently dropped.
func TestInvokeRecordsToolCalls(t *testing.T) {
	t.Parallel()

	// Converse nests the toolUse fields under a "toolUse" object, unlike the
	// Messages API's flat block.
	body := `{"output":{"message":{"role":"assistant","content":[{"type":"toolUse","toolUse":{"name":"lookup","input":{"q":"x"}}},{"type":"text","text":"checking"}]}},"stopReason":"tool_use","usage":{"inputTokens":10,"outputTokens":5,"totalTokens":15}}`
	a, _ := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024},
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		})

	resp, err := a.Invoke(t.Context(), aCase("c1", "hello"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.StopReason != knov1.StopReason_STOP_REASON_TOOL_CALL {
		t.Errorf("StopReason = %v, want TOOL_CALL", resp.StopReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != "lookup" || resp.ToolCalls[0].Arguments != `{"q":"x"}` {
		t.Errorf("ToolCalls[0] = %v", resp.ToolCalls[0])
	}
	// The text block is still the answer.
	if resp.Output != "checking" {
		t.Errorf("Output = %q", resp.Output)
	}
}

// TestInvokeRefusesMalformedCases pins the refusals that cost nothing because
// they happen before the request goes out.
func TestInvokeRefusesMalformedCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		c    *core.Case
	}{
		{"nil case", nil},
		{"empty input", aCase("c1", "")},
		{"history opens as assistant", aHistory(
			[]*knov1.Turn{turn(knov1.Role_ROLE_ASSISTANT, "b")}, "in",
		)},
		{"history turn without a role", aHistory(
			[]*knov1.Turn{{Content: "x"}}, "in",
		)},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, rec := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024},
				func(w http.ResponseWriter, r *http.Request) {
					t.Error("a malformed Case must not reach the wire")
				})
			_, err := a.Invoke(t.Context(), tc.c)
			if tc.name == "nil case" {
				// Same refusal the anthropic adapter gives: a bare message, no
				// Case id to hang a fix on. The other rows are errs-wrapped.
				if err == nil || !strings.Contains(err.Error(), "nil case") {
					t.Fatalf("error = %v, want a nil-case refusal", err)
				}
			} else if !errors.Is(err, errs.ErrInvalidInput) {
				t.Fatalf("error = %v, want ErrInvalidInput", err)
			}
			if got := rec.calls(); got != 0 {
				t.Errorf("the server saw %d requests, want 0", got)
			}
		})
	}
}

// TestInvokeSessionTokenIsASignedHeader pins the STS path on the wire: with a
// session token in the environment, the token travels as a SIGNED header.
func TestInvokeSessionTokenIsASignedHeader(t *testing.T) {
	t.Parallel()

	withToken := map[string]string{
		"AWS_ACCESS_KEY_ID":     "AKIAI44QH8DHBEXAMPLE",
		"AWS_SECRET_ACCESS_KEY": "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		"AWS_SESSION_TOKEN":     "AQoDYXdzEPT//////////wEXAMPLE",
		"AWS_REGION":            "us-east-1",
	}

	a, rec := newAgent(t, Options{
		Model:           testModel,
		MaxOutputTokens: 1024,
		getenv:          func(key string) string { return withToken[key] },
	}, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(converseOK))
	})

	if _, err := a.Invoke(t.Context(), aCase("c1", "hello")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	hdr := rec.header(t, 0)
	if hdr.Get("X-Amz-Security-Token") != "AQoDYXdzEPT//////////wEXAMPLE" {
		t.Error("the session token did not travel as a signed header")
	}
	auth := hdr.Get("Authorization")
	if !strings.Contains(auth, "x-amz-security-token") {
		t.Errorf("the token must be in SignedHeaders, got: %s", auth)
	}
}

// TestInvokeCanceledContext pins that cancellation is refused before any
// request goes out.
func TestInvokeCanceledContext(t *testing.T) {
	t.Parallel()

	a, rec := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024},
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("a canceled Case must not reach the wire")
		})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := a.Invoke(ctx, aCase("c1", "hello"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if got := rec.calls(); got != 0 {
		t.Errorf("the server saw %d requests, want 0", got)
	}
}
