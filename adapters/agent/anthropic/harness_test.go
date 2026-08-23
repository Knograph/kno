package anthropic_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/knograph/kno/adapters/agent/anthropic"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// testModel is priced in the table and accepts sampling parameters, so a test
// that is not about pricing or capabilities does not have to think about either.
const testModel = "claude-sonnet-4-6"

// capture records what the misbehaving server actually received.
//
// Assertions are made against THIS rather than against the adapter's own
// accounting, for the same reason the transport counts round trips at the
// RoundTripper: a client-side record of what it believes it sent cannot detect
// the case where something else sent something else.
type capture struct {
	mu      sync.Mutex
	bodies  []string
	headers []http.Header
}

func (c *capture) record(r *http.Request) {
	b, _ := io.ReadAll(r.Body)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, string(b))
	c.headers = append(c.headers, r.Header.Clone())
}

// calls reports how many requests reached the handler.
func (c *capture) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

// body returns the nth request body.
func (c *capture) body(t *testing.T, n int) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if n >= len(c.bodies) {
		t.Fatalf("the server saw %d requests, wanted at least %d", len(c.bodies), n+1)
	}
	return c.bodies[n]
}

// header returns the nth request's headers.
func (c *capture) header(t *testing.T, n int) http.Header {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if n >= len(c.headers) {
		t.Fatalf("the server saw %d requests, wanted at least %d", len(c.headers), n+1)
	}
	return c.headers[n]
}

// serve starts a misbehaving Messages API and returns it with its record.
func serve(t *testing.T, h http.HandlerFunc) (*httptest.Server, *capture) {
	t.Helper()
	rec := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// answer replies with a well-formed Messages API response.
func answer(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// newAgent builds an Agent aimed at a test server.
//
// No credential: a test server is not Anthropic's host, and a host with no
// binding legitimately needs none. The one test that is ABOUT credential
// binding sets an environment variable and does not run in parallel.
func newAgent(t *testing.T, srv *httptest.Server, mutate ...func(*anthropic.Options)) *anthropic.Agent {
	t.Helper()
	opts := anthropic.Options{
		Model:                testModel,
		MaxOutputTokens:      1024,
		BaseURL:              srv.URL,
		AllowInsecureBaseURL: true,
		AllowPrivateAddress:  true,
		HTTPClient:           srv.Client(),
	}
	for _, f := range mutate {
		f(&opts)
	}
	a, err := anthropic.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// aCase is a minimal single-turn Case.
func aCase(input string) *core.Case {
	return &knov1.Case{Id: "case-1", Input: input, Expected: "4"}
}
