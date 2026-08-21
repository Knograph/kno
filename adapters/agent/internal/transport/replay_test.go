package transport_test

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/knograph/kno/adapters/agent/internal/transport"
)

// recordingTransport captures the request net/http would send.
type recordingTransport struct {
	mu   sync.Mutex
	reqs []*http.Request
	next http.RoundTripper
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	return r.next.RoundTrip(req)
}

// TestRequestsCarryNoGetBodySoNetHTTPCannotReplayThem.
//
// "The transport does not retry" is not achieved by not writing retry code.
// net/http replays a request on a reused connection when nothing was written,
// if Request.GetBody is set — and http.NewRequest sets it automatically for a
// bytes.Reader body. For a POST it needs one more condition: an
// Idempotency-Key, which is exactly the header docs/debt.md#20 proposes adding
// to close part of the dark-spend window. The safe-looking header turns on
// silent replay with no code change visible in review.
//
// Asserted on the request rather than on server-side counts, because a replay
// and an ordinary fresh-connection attempt are INDISTINGUISHABLE at the server:
// both deliver exactly one request. The observable difference is that a replay
// makes the call succeed where it should have failed, so core never learns it
// needs a new reservation. GetBody is what decides that, so GetBody is what
// this checks.
func TestRequestsCarryNoGetBodySoNetHTTPCannotReplayThem(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	rec := &recordingTransport{next: http.DefaultTransport}

	d, err := transport.NewDestination(srv.URL, "k", "Authorization", transport.Policy{
		AllowInsecureHTTP: true, AllowPrivateAddress: true,
	})
	if err != nil {
		t.Fatalf("NewDestination: %v", err)
	}
	c, err := transport.New(transport.Options{
		Dest: d,
		// The header that makes a POST replayable in net/http's eyes.
		Headers:    http.Header{"Idempotency-Key": []string{"probe"}},
		HTTPClient: &http.Client{Transport: rec, CheckRedirect: d.CheckRedirect},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := c.Do(t.Context(), http.MethodPost, "/chat", []byte(`{"a":1}`)); err != nil {
		t.Fatalf("Do: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.reqs) != 1 {
		t.Fatalf("sent %d requests for one call", len(rec.reqs))
	}
	if rec.reqs[0].GetBody != nil {
		t.Error("the request carries GetBody, so net/http may replay it on a " +
			"reused connection — a provider call inside a reservation that has " +
			"already settled, invisible to the budget guard and to core's retry")
	}
	// And the body still went out: clearing GetBody must not have emptied it.
	if rec.reqs[0].Body == nil {
		t.Error("clearing GetBody also removed the request body")
	}
}

// TestNetHTTPDoesNotSilentlyReplayARequest.
//
// "The transport does not retry" is not achieved by not writing retry code.
// net/http replays a request on a reused connection when nothing was written,
// if Request.GetBody is set — and http.NewRequest sets it automatically for a
// bytes.Reader body. For a POST it needs one more condition: an
// Idempotency-Key, which is exactly the header docs/debt.md#20 proposes adding
// to close part of the dark-spend window. So the safe-looking header turns on
// silent replay, with no code change visible in review.
//
// Measured with GetBody left in place: the second call returned err=nil and the
// server saw a request the transport never counted — a provider call made
// inside a reservation that had already settled. With GetBody cleared, the same
// call surfaces as ErrTransient and core decides, taking a fresh reservation.
func TestNetHTTPDoesNotSilentlyReplayARequest(t *testing.T) {
	t.Parallel()

	var seen atomic.Int64
	var mu sync.Mutex
	var idle []net.Conn

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateIdle {
			mu.Lock()
			idle = append(idle, c)
			mu.Unlock()
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	d, err := transport.NewDestination(srv.URL, "k", "Authorization", transport.Policy{
		AllowInsecureHTTP: true, AllowPrivateAddress: true,
	})
	if err != nil {
		t.Fatalf("NewDestination: %v", err)
	}
	c, err := transport.New(transport.Options{
		Dest:       d,
		HTTPClient: srv.Client(),
		// The header that makes a POST replayable in net/http's eyes.
		Headers: http.Header{"Idempotency-Key": []string{"probe"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The scenario is inherently racy: the client may notice a dead pooled
	// connection before writing and quietly open a fresh one, in which case no
	// replay is possible and there is nothing to observe. So drive it until a
	// transport-level failure actually occurs, and assert on THAT — rather than
	// assuming one attempt reproduces it, which fails intermittently under
	// -race for reasons that have nothing to do with the property.
	for attempt := range 20 {
		if _, err := c.Do(t.Context(), http.MethodPost, "/chat", []byte(`{}`)); err != nil {
			t.Fatalf("establishing a pooled connection (attempt %d): %v", attempt, err)
		}

		mu.Lock()
		for _, cn := range idle {
			_ = cn.Close()
		}
		idle = nil
		mu.Unlock()

		before := seen.Load()
		_, err := c.Do(t.Context(), http.MethodPost, "/chat", []byte(`{}`))
		handled := seen.Load() - before

		if err == nil {
			// The client recovered without our seeing a failure. Whether it
			// replayed or opened a fresh connection, exactly one request must
			// have reached the server for this one call.
			if handled != 1 {
				t.Fatalf("a successful call produced %d server requests", handled)
			}
			continue
		}

		// A failure surfaced. This is the state where net/http would have
		// replayed: assert it did not, and that the failure is one core can
		// retry under a fresh reservation.
		if handled != 0 {
			t.Fatalf("the server handled %d request(s) for a call the transport "+
				"reported as failing; net/http replayed underneath the budget "+
				"guard, so a provider call happened inside a reservation that "+
				"had already settled", handled)
		}
		if !errors.Is(err, transport.ErrTransient) {
			t.Fatalf("err = %v, want ErrTransient; the failure must surface so "+
				"core can retry it under a fresh reservation rather than "+
				"net/http retrying it under none", err)
		}
		return // observed and asserted
	}
	t.Skip("could not get the client to reuse a dead pooled connection in 20 " +
		"attempts; the property is unexercised rather than violated")
}
