package bedrock

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// testModel is priced in the table and accepts sampling parameters, so a test
// that is not about pricing or capabilities does not have to think about
// either.
const testModel = "anthropic.claude-sonnet-4-5"

// testCreds is the environment resolveCreds accepts.
var testCreds = map[string]string{
	"AWS_ACCESS_KEY_ID":     "AKIAI44QH8DHBEXAMPLE",
	"AWS_SECRET_ACCESS_KEY": "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	"AWS_REGION":            "us-east-1",
}

func testGetenv(key string) string { return testCreds[key] }

// capture records what the misbehaving server actually received.
//
// Assertions are made against THIS rather than against the adapter's own
// accounting, for the same reason the transport counts round trips at the
// RoundTripper: a client-side record of what it believes it sent cannot detect
// the case where something else sent something else.
type capture struct {
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
}

func (c *capture) record(r *http.Request) {
	b, _ := io.ReadAll(r.Body)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, b)
	c.headers = append(c.headers, r.Header.Clone())
}

// calls reports how many requests reached the handler.
func (c *capture) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

// body returns the nth request body.
func (c *capture) body(t *testing.T, n int) []byte {
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

// serve starts a fake Converse API and returns it with its record.
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

// rewrite dials the test server while keeping the request's own URL and Host.
// The Agent's endpoint stays the real-looking regional host — which is what
// the SigV4 signature is computed over, exactly as it would be in production —
// and only the DIAL address is redirected to the fake. The server therefore
// sees the same request line, Host header, and signature AWS would see, and
// the harness tests the real signing path instead of a hand-waved one.
type rewrite struct {
	next http.RoundTripper
	dst  *url.URL
}

func (r *rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	u := *req.URL
	u.Scheme = r.dst.Scheme
	u.Host = r.dst.Host
	req.URL = &u
	return r.next.RoundTrip(req)
}

// newAgent builds an Agent against a fake server.
//
// Defaults fill what a test is not about: the priced test model, the
// credential environment, the regional endpoint, and a fixed clock — a
// deterministic x-amz-date is what lets the harness tests assert on the
// signature's headers without a live clock in the picture.
func newAgent(t *testing.T, opts Options, h http.HandlerFunc) (*Agent, *capture) {
	t.Helper()
	srv, rec := serve(t, h)
	if opts.getenv == nil {
		opts.getenv = testGetenv
	}
	if opts.endpointURL == "" {
		opts.endpointURL = "https://bedrock-runtime.us-east-1.amazonaws.com"
	}
	if opts.Model == "" {
		opts.Model = testModel
	}
	if opts.MaxOutputTokens == 0 {
		opts.MaxOutputTokens = 1024
	}
	if opts.now == nil {
		opts.now = func() time.Time {
			return time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
		}
	}
	if opts.HTTPClient == nil {
		u, err := url.Parse(srv.URL)
		if err != nil {
			t.Fatalf("parsing %s: %v", srv.URL, err)
		}
		opts.HTTPClient = &http.Client{
			Transport: &rewrite{next: http.DefaultTransport, dst: u},
		}
	}
	a, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a, rec
}

// aCase builds a Case with an id and an input.
func aCase(id, input string) *core.Case {
	return &core.Case{Id: id, Input: input}
}

// aHistory builds a Case carrying history.
func aHistory(history []*knov1.Turn, input string) *core.Case {
	return &core.Case{
		Id:      "case-history",
		History: history,
		Input:   input,
	}
}

// turn is a one-line history builder.
func turn(role knov1.Role, content string) *knov1.Turn {
	return &knov1.Turn{Role: role, Content: content}
}

// converseOK is a minimal, valid Converse 200 body. Usage figures are chosen
// so the settlement arithmetic is exact: 1000 input + 100 cache writes + 500
// cache reads = 1600 billed input tokens, 200 output.
const converseOK = `{
	"output": {"message": {"role": "assistant", "content": [{"type": "text", "text": "hello from bedrock"}]}},
	"stopReason": "end_turn",
	"usage": {
		"inputTokens": 1000,
		"cacheCreationInputTokens": 100,
		"cacheReadInputTokens": 500,
		"outputTokens": 200,
		"totalTokens": 1800
	}
}`

// newTestServer starts a bare fake API with no recording, for tests that only
// need a reachable URL (a redirect's target, say).
func newTestServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// retryAfterOf reads the provider's wait the way core's backoff does — a
// structural read, so the adapter can carry the wait without core importing it.
func retryAfterOf(err error) (time.Duration, bool) {
	var ra interface{ RetryAfter() time.Duration }
	if errors.As(err, &ra) {
		return ra.RetryAfter(), true
	}
	return 0, false
}
