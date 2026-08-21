package transport_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/knograph/kno/adapters/agent/internal/transport"
)

// localClient builds a Client aimed at a test server.
func localClient(t *testing.T, srv *httptest.Server, opts ...func(*transport.Options)) *transport.Client {
	t.Helper()

	d, err := transport.NewDestination(srv.URL, "test-key", "Authorization", transport.Policy{
		AllowInsecureHTTP: true, AllowPrivateAddress: true,
	})
	if err != nil {
		t.Fatalf("NewDestination: %v", err)
	}
	o := transport.Options{Dest: d, HTTPClient: srv.Client()}
	for _, f := range opts {
		f(&o)
	}
	c, err := transport.New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestExactlyOneRoundTripPerCall, counted AT THE SERVER.
//
// "The transport does not retry" is not achieved by not writing retry code.
// net/http replays a request on a reused connection when nothing was written,
// provided Request.GetBody is set — and http.NewRequest sets it automatically
// for a bytes.Reader body. Benign for money today, because the provider never
// saw the first attempt; a real double-charge the moment an adapter sets an
// Idempotency-Key, since isReplayable then permits replay on
// transportReadFromServerError, where bytes WERE written.
//
// A client-side counter cannot prove this. The server counts.
func TestExactlyOneRoundTripPerCall(t *testing.T) {
	t.Parallel()

	var seen atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		// Close the connection so the next call reuses nothing, which is the
		// state where net/http would replay.
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	c := localClient(t, srv)
	const calls = 5
	for range calls {
		if _, err := c.Do(t.Context(), http.MethodPost, "/chat", []byte(`{}`)); err != nil {
			t.Fatalf("Do: %v", err)
		}
	}

	if got := seen.Load(); got != calls {
		t.Errorf("the server saw %d requests for %d calls; the transport retried "+
			"underneath the budget guard, which reserves once per call", got, calls)
	}
	if got := c.RoundTrips(); got != calls {
		t.Errorf("RoundTrips = %d, want %d", got, calls)
	}
}

// TestIdleConnectionCloseIsTransientNotAnAgentError.
//
// A stale pooled connection is not the agent failing. At concurrency, any pause
// in a long run produces a handful of these, and treating them as terminal
// marks a healthy baseline unusable over an idle timeout.
func TestIdleConnectionCloseIsTransientNotAnAgentError(t *testing.T) {
	t.Parallel()

	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			// Hijack and drop, so the client sees the connection die with no
			// response — exactly what a server closing an idle connection does.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("Hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := localClient(t, srv)
	_, err := c.Do(t.Context(), http.MethodPost, "/chat", []byte(`{}`))
	if err == nil {
		t.Fatal("a dropped connection produced no error")
	}
	if !errors.Is(err, transport.ErrTransient) {
		t.Errorf("err = %v, want ErrTransient; a connection the provider never "+
			"answered on is retryable, and calling it an agent error trips the "+
			"error-rate threshold on a healthy run", err)
	}
}

// TestResponseBodyIsBounded: a provider returning gigabytes is a memory
// exhaustion vector, and the body is persisted as a trace on top of that.
func TestResponseBodyIsBounded(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for range 100 {
			if _, err := w.Write(make([]byte, 1024)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	c := localClient(t, srv, func(o *transport.Options) { o.MaxResponseBytes = 4096 })
	_, err := c.Do(t.Context(), http.MethodPost, "/chat", []byte(`{}`))
	if err == nil {
		t.Fatal("an oversized response was accepted whole")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("err = %v, want a size refusal", err)
	}
}

// TestRateLimitClosesTheHostForAsLongAsAsked.
func TestRateLimitClosesTheHostForAsLongAsAsked(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := localClient(t, srv)
	resp, err := c.Do(t.Context(), http.MethodPost, "/chat", []byte(`{}`))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if resp.RetryAfter != time.Second {
		t.Errorf("RetryAfter = %v, want 1s", resp.RetryAfter)
	}

	// The next call waits, and reports that it did — so a caller can say
	// "waiting on a rate limit" rather than looking hung.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := c.Do(ctx, http.MethodPost, "/chat", []byte(`{}`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v; the limiter did not hold the host after a 429", err)
	}
}
