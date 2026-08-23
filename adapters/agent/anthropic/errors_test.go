package anthropic_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/knograph/kno/adapters/agent/anthropic"
	"github.com/knograph/kno/core/errs"
)

// retryAfterOf is the shape core.retryAfterOf looks for.
//
// Copied rather than imported because core's is unexported, and the point of
// this test is that the CONTRACT holds — an anonymous interface with one method
// — not that two packages share a type.
func retryAfterOf(err error) (time.Duration, bool) {
	var ra interface{ RetryAfter() time.Duration }
	if errors.As(err, &ra) {
		d := ra.RetryAfter()
		return d, d > 0
	}
	return 0, false
}

// TestRateLimitCarriesTheProvidersRetryAfterInBothForms.
//
// RFC 9110 permits delta-seconds and an HTTP-date. Parsing only the first is
// the common shortcut, and it reads a dated header as no delay at all — turning
// a polite backoff into a hot loop against a provider that just asked us to
// stop.
func TestRateLimitCarriesTheProvidersRetryAfterInBothForms(t *testing.T) {
	t.Parallel()

	forms := map[string]func() string{
		"delta-seconds": func() string { return "7" },
		"HTTP-date":     func() string { return time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat) },
	}

	for name, header := range forms {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", header())
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
			})
			a := newAgent(t, srv)

			_, err := a.Invoke(t.Context(), aCase("q"))
			if !errors.Is(err, errs.ErrRateLimited) {
				t.Fatalf("err = %v, want ErrRateLimited", err)
			}
			d, ok := retryAfterOf(err)
			if !ok {
				t.Fatal("the error carries no RetryAfter, so core would back off on its " +
					"own schedule and ignore the one the provider asked for")
			}
			if d <= 0 || d > time.Minute {
				t.Errorf("RetryAfter = %v; it must be positive and clamped so a hostile "+
					"header cannot hang a run", d)
			}
		})
	}
}

// TestASpendCapRateLimitIsTerminalRatherThanRetried.
//
// Anthropic returns HTTP 429 with type rate_limit_error both when an
// organization is throttled and when it has crossed its monthly spend cap. The
// second carries no Retry-After and keeps failing until the next calendar
// month. Retried, it burns each Case's whole retry budget and settles one call
// per attempt against --max-calls, for every Case in the run.
func TestASpendCapRateLimitIsTerminalRatherThanRetried(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error",
		  "message":"your organization has crossed its monthly API usage threshold",
		  "details":{"error_code":"enforced_spend_limit_reached"}}}`))
	})
	a := newAgent(t, srv)

	_, err := a.Invoke(t.Context(), aCase("q"))
	if errors.Is(err, errs.ErrRateLimited) {
		t.Error("a spend-cap 429 was classified as retryable; it never clears within the run")
	}
	if !errors.Is(err, anthropic.ErrProvider) {
		t.Errorf("err = %v, want ErrProvider", err)
	}
}

// TestServerSideFailuresAreTransientRatherThanAgentErrors.
//
// 500, 504, and 529 are the provider failing to answer a request it accepted.
// core retries them under its own reservation-per-attempt discipline; calling
// them agent errors trips the 5% error-rate threshold and marks a healthy
// baseline unusable.
func TestServerSideFailuresAreTransientRatherThanAgentErrors(t *testing.T) {
	t.Parallel()

	for _, status := range []int{500, 504, 529} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()

			srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`))
			})
			a := newAgent(t, srv)

			_, err := a.Invoke(t.Context(), aCase("q"))
			if !errors.Is(err, errs.ErrTransportTransient) {
				t.Errorf("HTTP %d gave err = %v, want ErrTransportTransient", status, err)
			}
		})
	}
}

// TestAConnectionClosedAfterTheHeadersIsTransient.
//
// A body that ends early is the provider never having finished answering. At
// concurrency, any pause in a long run produces a handful of these.
func TestAConnectionClosedAfterTheHeadersIsTransient(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		conn, bw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		// Headers promising a body, then nothing.
		_, _ = bw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n" +
			"Content-Length: 200\r\n\r\n{\"id\":\"msg\"")
		_ = bw.Flush()
		_ = conn.Close()
	})
	a := newAgent(t, srv)

	_, err := a.Invoke(t.Context(), aCase("q"))
	if !errors.Is(err, errs.ErrTransportTransient) {
		t.Errorf("err = %v, want ErrTransportTransient", err)
	}
}

// TestMalformedJSONOnA200IsTerminalRatherThanRetried.
//
// The opposite trade from a dropped connection, and the difference is money.
// The provider answered, so it billed; retrying pays a second time for one
// answer.
func TestMalformedJSONOnA200IsTerminalRatherThanRetried(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, `{"id":"m","content":[{"type":"text"`)
	})
	a := newAgent(t, srv)

	_, err := a.Invoke(t.Context(), aCase("q"))
	if !errors.Is(err, anthropic.ErrMalformedResponse) {
		t.Fatalf("err = %v, want ErrMalformedResponse", err)
	}
	if errors.Is(err, errs.ErrTransportTransient) || errors.Is(err, errs.ErrRateLimited) {
		t.Error("a 200 the provider billed for was classified as retryable, so the " +
			"run would pay twice for one answer")
	}
}

// TestAuthenticationFailuresNameTheEnvironmentVariable.
//
// Without a specific sentinel this errors every Case and reports "too many
// cases errored for this to be a usable baseline" — a message naming nothing
// about the cause.
func TestAuthenticationFailuresNameTheEnvironmentVariable(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()

			srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
			})
			a := newAgent(t, srv)

			_, err := a.Invoke(t.Context(), aCase("q"))
			if !errors.Is(err, anthropic.ErrAuthentication) {
				t.Fatalf("err = %v, want ErrAuthentication", err)
			}
			if errors.Is(err, errs.ErrTransportTransient) || errors.Is(err, errs.ErrRateLimited) {
				t.Error("a rejected credential was classified as retryable; it will be " +
					"rejected identically on every attempt")
			}
			if !strings.Contains(err.Error(), anthropic.DefaultKeyEnv) {
				t.Errorf("the error does not name %s, so the fix is not actionable: %v",
					anthropic.DefaultKeyEnv, err)
			}
		})
	}
}

// TestAContextWindowRefusalNamesTheLimitAndIsTerminal.
func TestAContextWindowRefusalNamesTheLimitAndIsTerminal(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error",
		  "message":"prompt is too long: 250000 tokens > 200000 maximum"}}`))
	})
	a := newAgent(t, srv)

	_, err := a.Invoke(t.Context(), aCase("q"))
	if !errors.Is(err, anthropic.ErrProvider) {
		t.Fatalf("err = %v, want ErrProvider", err)
	}
	if errors.Is(err, errs.ErrTransportTransient) || errors.Is(err, errs.ErrRateLimited) {
		t.Error("a prompt that does not fit was classified as retryable; it will not " +
			"fit on the next attempt either")
	}
	if !strings.Contains(err.Error(), "200000") {
		t.Errorf("the error does not carry the limit the provider named: %v", err)
	}
	if !strings.Contains(err.Error(), "context window") {
		t.Errorf("the fix does not say what to do: %v", err)
	}
}

// TestACrossHostRedirectDoesNotCarryTheCredential.
//
// Go's net/http strips Authorization, WWW-Authenticate, Cookie, and Cookie2 on
// a cross-domain redirect and does NOT strip x-api-key, which is how this API
// authenticates. A base URL returning a 302 elsewhere would forward the key
// verbatim, with no plain-HTTP hop to refuse and nothing the user did wrong.
//
// A CREDENTIAL IS BOUND, because the threat is about a credential. The previous
// version built an Agent with no key at all, so "the attacker host received no
// key" was true no matter what the redirect policy did. Not parallel: it reads
// process environment.
func TestACrossHostRedirectDoesNotCarryTheCredential(t *testing.T) {
	const envVar = "KNO_TEST_REDIRECT_CREDENTIAL"
	const value = "test-credential-value"
	t.Setenv(envVar, value)

	// The attacker. Records anything that reaches it.
	target, targetRec := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, okBody)
	})

	origin, originRec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/messages", http.StatusFound)
	})
	host := strings.TrimPrefix(origin.URL, "http://")

	a := newAgent(t, origin, func(o *anthropic.Options) {
		o.KeyEnv = map[string]string{host: envVar}
	})

	_, err := a.Invoke(t.Context(), aCase("q"))
	if err == nil {
		t.Fatal("a cross-host redirect was followed")
	}
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Errorf("err = %v, want ErrInvalidInput", err)
	}
	if errors.Is(err, errs.ErrTransportTransient) || errors.Is(err, errs.ErrRateLimited) {
		t.Error("a refused redirect was classified as retryable, so every attempt " +
			"would re-offer the key to the redirect")
	}

	// The credential really was in play on the first hop. Without this the
	// assertion below is vacuous, which is exactly what it used to be.
	if got := originRec.header(t, 0).Get(anthropic.KeyHeader); got != value {
		t.Fatalf("the bound host did not receive the credential (%q), so this test "+
			"is not exercising the threat it describes", got)
	}
	if targetRec.calls() != 0 {
		t.Fatalf("the redirect target received %d requests; the credential header it "+
			"saw was %q", targetRec.calls(), targetRec.header(t, 0).Get(anthropic.KeyHeader))
	}
	// And the refusal must not quote the key back into a log line.
	if strings.Contains(err.Error(), value) {
		t.Errorf("the credential was rendered into the error: %v", err)
	}
}

// TestAnErrorBodyThatIsNotTheProvidersJSONStillClassifies.
//
// A gateway between us and the API substitutes an HTML page for the error body.
// The status is then the whole story, and it must still be classified — a 503
// from a proxy is as transient as a 503 from Anthropic.
func TestAnErrorBodyThatIsNotTheProvidersJSONStillClassifies(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("<html><body>503 Service Unavailable</body></html>"))
	})
	a := newAgent(t, srv)

	_, err := a.Invoke(t.Context(), aCase("q"))
	if !errors.Is(err, errs.ErrTransportTransient) {
		t.Errorf("err = %v, want ErrTransportTransient", err)
	}
}

// TestAProviderMessageCannotInjectIntoALogLine.
//
// A 400 quotes parts of the request back, so this string carries customer data
// into an error that is logged, persisted on the Outcome, and rendered in
// --json. A newline in it is a log-injection primitive.
func TestAProviderMessageCannotInjectIntoALogLine(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\"," +
			"\"message\":\"bad\\nlevel=fatal msg=\\\"everything is fine\\\"\"}}"))
	})
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), aCase("q"))
	if err == nil {
		t.Fatal("a 400 produced no error")
	}
	// The Actionable's own rendering has newlines for the fix and docs lines;
	// what must not survive is a newline from the PROVIDER's message.
	if strings.Contains(err.Error(), "bad\nlevel=fatal") {
		t.Errorf("a provider newline reached the error string: %q", err.Error())
	}
	if resp != nil && strings.ContainsAny(resp.GetError(), "\n\r") {
		t.Errorf("a provider newline reached Response.error: %q", resp.GetError())
	}
}

// TestAFailedCallStillDescribesItselfOnTheResponse.
//
// core discards this Response today, settling a flat one call at zero cost —
// that is docs/debt.md#43. The adapter's half of the fix is to carry the fact;
// carrying it costs nothing and makes the entry repayable without a second
// adapter change.
func TestAFailedCallStillDescribesItselfOnTheResponse(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"nope"}}`))
	})
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), aCase("q"))
	if err == nil {
		t.Fatal("a 400 produced no error")
	}
	if resp == nil {
		t.Fatal("no Response for a call the provider actually answered")
	}
	if resp.GetCaseId() != "case-1" {
		t.Errorf("case_id = %q", resp.GetCaseId())
	}
	if !strings.Contains(resp.GetError(), "400") {
		t.Errorf("Response.error = %q, want the status in it", resp.GetError())
	}
	if resp.GetCostUsdMicros() != 0 {
		t.Errorf("cost = %d; an error body carries no usage block, so there is no "+
			"reported cost to settle", resp.GetCostUsdMicros())
	}
}

// TestACancelledContextIsNotAnAgentError.
//
// core tells a run that is shutting down from a Case that genuinely failed by
// asking exactly this question. A cancelled Case must stay unrecorded so the
// resume picks it up rather than skipping it — recorded, it is marked complete
// and vanishes from the run permanently, shrinking the denominator behind every
// later delta with nothing saying why.
//
// Both halves are exercised. Cancelling BEFORE the call hits an early guard and
// proves nothing about the transport; cancelling MID-FLIGHT is the path that
// runs through classifyTransportError, where a cancellation is one `errors.Is`
// away from being classified as a transient failure and retried.
func TestACancelledContextIsNotAnAgentError(t *testing.T) {
	t.Parallel()

	t.Run("before the call", func(t *testing.T) {
		t.Parallel()

		srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) { answer(w, okBody) })
		a := newAgent(t, srv)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := a.Invoke(ctx, aCase("q"))
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if rec.calls() != 0 {
			t.Error("a cancelled Case still reached the provider")
		}
	})

	t.Run("mid flight", func(t *testing.T) {
		t.Parallel()

		reached := make(chan struct{})
		srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
			close(reached)
			// Hold the response open until the client gives up, which is what a
			// provider looks like when a run is shutting down underneath it.
			<-r.Context().Done()
		})
		a := newAgent(t, srv)

		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			<-reached
			cancel()
		}()

		_, err := a.Invoke(ctx, aCase("q"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if errors.Is(err, errs.ErrTransportTransient) {
			t.Error("a cancellation was classified as transient, so a stopping run " +
				"would retry every in-flight Case on its way down")
		}
		if errors.Is(err, anthropic.ErrProvider) {
			t.Error("a cancellation was classified as an agent error, so shutdown " +
				"would look like a broken provider")
		}
	})
}
