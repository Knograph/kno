package vertex

import (
	"context"
	"encoding/json"
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
const testModel = "claude-sonnet-4-5"

// testSAKey is a PKCS#8 RSA private key, generated once for the test package.
// The JWT signer is exercised against the REAL exchange code, and the
// signature is pinned by the RFC 7515 A.2 known-answer separately — see
// jwt_test.go.
const testSAKey = `-----BEGIN PRIVATE KEY-----
MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQCvJk2jiJTIab/z
XW4iwsoNXJSgi5/nrRlsgUx9O+vzPL8rQnaZ6T6cPZHmsLwuGdOrNnfv1Ohk5KJl
0sOhFZHf8rCBHoXhvi7P3AW0pwZHYeLxPZuMOIYB2hkNTkduXd3IHGAYCgiiVbKc
5S6zOtjLX37ZTRh+DpR1AMmUkLubVGVdcUMgFfhkuF/FwUG+tfZtph9bSy7Iaxsj
Hwf9dSzBuVvsCP8wSiO4U/wdPTa06dDsyX5nhUQfbmb8z6xJKF7lJNg17rYgea0b
yH+N+xb8Soa2JOLvU6bqlEPLvf3IeGwHotCeWw1/ERqUVd7cPmiMGFeG8z/4IiHH
UL54+yCxAgMBAAECggEAOloef58fQm3I34F/EeGngzAW7C3YRk0rLTUekJKIF29j
mTv5W2mTzGXO1/aFmy5LkL0C1EowanyphhbjyiTvhpbKKxpKLF06J1H8LKWAuANq
okiOK/cg3jkVI5OyxJhNLUAW26tsGPlIGnFYT6oJVCgqkKbYxBaDaz+p6O8XMbYr
xajzgUDecVo32ap87OHIgxpAbWqLh0tjAYiW5SDCg0OzLdpeuuMajyZTWlJXBZic
THPBVANfFwDtGLPXv5jvpK+m5beokHEP7U0m8hJnWiIN1w7aMOUhXC3rP6DIxXb0
SSctaidh86pDomX5FeSjjbyTd8F7/fZyrF/VuBSaiQKBgQD0qCqKz5MScckv+Fek
5t72FHLwq0sONZYFRdzOgPTGRUSaYoD5+pwA+AHvtYR6g/k38m8lsrFDZR1KW82H
WLQ0MlkM8/GWOofdcBtQPNJC6SY1/vyUDkNn3UaIG4R90MZ/Uhi9YtD+blZf9c7k
d9VaDz9wLfXvXlWq0aEDsUCqMwKBgQC3RSeosdKzdZCNoDiYjyk5FhP58/49fSd1
50DEAb8h5UYKAH8sSRk2ojNk04X5a0NyTAcITNgX9kcG/T+TvtmGcB0NTKbfmRq5
/7UboN9eC9shE35+lCgRc1Fx5ciDw3PPNUynXzhn52KXA8+bljh/ssGspjK2cvv9
XRHviv1tiwKBgQCqOO8QkYgEh0Kxq5pfU3rBwEyQgr2/7yyoEomk7DhiUwN+XxbZ
1rIAQo4mWCcKjxQxBu6qTf/jolCU0fbYOrF2t6kZyAjIu4SYX03Br++jOlCptPXL
lXj0pRJT1MGEQGQ7ZcVsz3oV7HMQZRhEAdRhysYaqP+6Qepc5WmgBg213QKBgAux
2AQFxOI6wEypSrNBf2nrJL8weKrHz7rQVOutCNtK3BtLSNI0n+1CkHEApm3yEE28
2D4JWUi+KG4jvujYptzTTqdImuVtyazQymfG7jn8G7GSouHE5oGmkC3qcc8mq78v
MYMEqn7G3x2v2pGdFmHfsEgqGtZVpArY44obnmxdAoGAUqR+edeBtWZhDd/AKMC3
mzrTISj+PL9KY8HUbY9Xxxm1bT3IYQEjNM82Gx6SC+mQg2wOHQ28O3ZU3qKFeuxs
xhwrpPvjOnUPGgLQGjsW+fd7Lh3jU2r5fwHplwsy3Z8Nm2x162vdT41KZGfCv27r
lN0Otgs6J7FRr/T/a41vk2w=
-----END PRIVATE KEY-----`

// testSAJSON is the service-account key file the credential chain reads.
//
// Built with json.Marshal rather than by hand: the private key is PEM text
// full of newlines, and only the marshaler knows how to quote them.
var testSAJSON = func() string {
	sa := map[string]string{
		"type":         "service_account",
		"project_id":   "kno-test-proj",
		"private_key":  testSAKey,
		"client_email": "kno-test@kno-test-proj.iam.gserviceaccount.com",
	}
	b, err := json.Marshal(sa)
	if err != nil {
		panic(err)
	}
	return string(b)
}()

// testCreds is the environment resolveCreds accepts.
var testCreds = map[string]string{
	"GOOGLE_APPLICATION_CREDENTIALS": "/secrets/kno-test-sa.json",
	"GOOGLE_CLOUD_REGION":            "us-central1",
}

func testGetenv(key string) string { return testCreds[key] }

// testReadFile serves the service-account key file.
func testReadFile(path string) ([]byte, error) {
	if path != testCreds["GOOGLE_APPLICATION_CREDENTIALS"] {
		return nil, errors.New("no such file")
	}
	return []byte(testSAJSON), nil
}

// testNow is a fixed clock: the JWT's iat is deterministic, and the token
// cache's margin logic is testable.
func testNow() time.Time {
	return time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
}

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

// serve starts a fake :rawPredict API and returns it with its record.
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
// the request is SIGNED against, exactly as it would be in production — and
// only the DIAL address is redirected to the fake. The server therefore sees
// the same request line and Host header Google would see, and the harness
// tests the real bearer path instead of a hand-waved one.
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
// credential environment, the regional endpoint, a fixed clock, and a token
// exchange that never reaches the network. The exchange is pinned the way the
// sibling adapters pin their signer: the tests that are ABOUT the exchange
// use the real one.
func newAgent(t *testing.T, opts Options, h http.HandlerFunc) (*Agent, *capture) {
	t.Helper()
	srv, rec := serve(t, h)
	if opts.getenv == nil {
		opts.getenv = testGetenv
	}
	if opts.readFile == nil {
		opts.readFile = testReadFile
	}
	if opts.endpointURL == "" {
		opts.endpointURL = "https://us-central1-aiplatform.googleapis.com"
	}
	if opts.Model == "" {
		opts.Model = testModel
	}
	if opts.MaxOutputTokens == 0 {
		opts.MaxOutputTokens = 1024
	}
	if opts.now == nil {
		opts.now = testNow
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
	// Pinned exchange: a fixed token that never expires against testNow.
	a.tokens.exchange = func(context.Context) (string, time.Time, error) {
		return "ya29.test-token", testNow().Add(time.Hour), nil
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

// rawPredictOK is a minimal, valid :rawPredict 200 body, Messages shape.
// Usage figures are chosen so the settlement arithmetic is exact: 1000 input
// + 100 cache writes + 500 cache reads = 1600 billed input tokens, 200 output.
const rawPredictOK = `{
	"id": "msg_vrtx_01",
	"type": "message",
	"role": "assistant",
	"model": "claude-sonnet-4-5",
	"content": [{"type": "text", "text": "hello from vertex"}],
	"stop_reason": "end_turn",
	"usage": {
		"input_tokens": 1000,
		"cache_creation_input_tokens": 100,
		"cache_read_input_tokens": 500,
		"output_tokens": 200
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
