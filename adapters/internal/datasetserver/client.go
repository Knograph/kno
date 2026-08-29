// Package datasetserver is the HTTP client both Hugging Face adapters share:
// the Evals adapter reads dataset splits as Cases, the Pool adapter reads the
// same rows as Assets. The two adapters must talk to the same server with the
// same rules — the same error taxonomy, the same x-revision discipline, the
// same refusal to follow redirects — or one of them drifts into behavior the
// other was reviewed for. This package is the third shared thing in the
// adapters layer, after the agent transport and endpointsec, and it sits on
// the same principle: the policy lives once, and both adapters inherit it.
//
// The datasets-server API (https://datasets-server.huggingface.co, documented
// in the plan at docs/plans/2026-08-29-huggingface-adapters.md) has a small,
// stable surface:
//
//   - GET /splits?dataset=<org>/<name> lists the config/split pairs the
//     dataset actually offers. The server answers 401 for a dataset that does
//     not exist AND for one that is gated or private — one status, two
//     meanings, so the refusal offers both remedies.
//   - GET /rows?dataset=&config=&split=&offset=N&length=100 returns a page of
//     rows with the envelope {rows, num_rows_total, partial}. partial:true
//     means the server served a subsample of the split, which would quietly
//     change the measurement's population, so it is refused.
//
// Every response carries an x-revision header naming the git commit the
// server served from. That header — never the revision query parameter, which
// the server ignores — is the fingerprint that pins a split's identity across
// runs: if the dataset's commit moves between pages, or between this run and
// the next, the fingerprint changes and the split is a different object. A
// response without the header is treated as broken, not as fingerprintless.
//
// Credentials: an optional bearer token (the HF_TOKEN environment value,
// resolved by the adapters, never by this package). The token never appears
// in an error. Redirects are refused outright. A 5xx is the one retried
// status class, bounded; a 429 is not retried — the server is throttling this
// address, and retrying multiplies the load it asked us to reduce.
package datasetserver

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/knograph/kno/adapters/internal/endpointsec"
)

// DefaultHost is the public datasets-server root, used when Options.Host is
// empty. Tests and self-hosted mirrors supply their own.
const DefaultHost = "https://datasets-server.huggingface.co"

// PageSize is the number of rows requested per page. Fixed, matching the
// server's own default; a page is a bounded unit of work and of memory.
const PageSize = 100

// MaxPages is the pagination backstop. A split whose offset never runs out
// of rows is caught here rather than fetched forever. 100,000 pages is ten
// million cases — past the design point of the engine, so the cap is a
// sanity bound, not a plan.
const MaxPages = 100_000

const (
	requestTimeout = 60 * time.Second
	maxAttempts    = 3
	// maxPageBytes caps one page's response body. A page bigger than this is
	// broken, not slow.
	maxPageBytes = 32 << 20
)

// retryPause is the fixed pause between 5xx attempts. A load-balanced server
// mid-failover answers 5xx transiently; a fixed pause is honest about not
// knowing how long the failover takes. It is a var, not a const, so the
// retry tests can shrink it.
var retryPause = 500 * time.Millisecond

// Options configures the client. See each field; the security-relevant ones
// exist because self-hosted mirrors of datasets-server are a real use.
type Options struct {
	// Host is the API root. Empty means DefaultHost.
	Host string

	// Token is the bearer credential, resolved from HF_TOKEN by the adapters.
	// Empty for public datasets; a gated dataset without a token answers 401.
	Token string

	// AllowInsecureBaseURL permits plain HTTP, which sends the request and
	// the token in the clear. Off by default.
	AllowInsecureBaseURL bool

	// AllowPrivateAddress permits loopback and private-address endpoints,
	// for self-hosted mirrors. Off by default; link-local is refused with no
	// override either way.
	AllowPrivateAddress bool

	// HTTPClient contributes transport and TLS settings only: the redirect
	// policy is always the refusing one and the timeout is always ours, same
	// contract as the other adapters' Options.HTTPClient.
	HTTPClient *http.Client
}

// Client is a configured datasets-server client. It is safe for concurrent
// use, but the adapters use one per Evals/Pool, so concurrency never arises
// in practice.
type Client struct {
	endpoint  string
	token     string
	transport *http.Client
}

// New validates the endpoint and builds the transport. The endpoint policy
// is endpointsec's; a host the policy refuses cannot be the host a token
// travels to.
func New(opts Options) (*Client, error) {
	host := opts.Host
	if host == "" {
		host = DefaultHost
	}
	if err := endpointsec.Check(host, opts.AllowInsecureBaseURL, opts.AllowPrivateAddress); err != nil {
		return nil, err
	}
	return &Client{
		endpoint:  strings.TrimSuffix(host, "/"),
		token:     opts.Token,
		transport: newHTTPClient(opts.HTTPClient, opts.AllowPrivateAddress),
	}, nil
}

// newHTTPClient builds the client every request goes through.
//
// The caller's client contributes transport and TLS settings only: the
// redirect policy is always the refusing one, because a redirect would carry
// the Authorization header to a host the user never named, and the timeout
// is always the request timeout, because a page slower than the cap is
// broken, not slow. Same contract as the other adapters' Options.HTTPClient.
//
// The private-address policy is installed as a dial-time recheck of the
// RESOLVED address: the config-time check in New cannot see where a
// hostname resolves. A transport the caller supplied that is not an
// *http.Transport (a test stub) has no dialer to reach, so the config-time
// check is what applies to it.
func newHTTPClient(given *http.Client, allowPrivateAddress bool) *http.Client {
	out := http.Client{}
	if given != nil {
		out = *given // copy: never mutate a client the caller still holds
	}
	if out.Transport == nil {
		// Clone the process default rather than use it: the check below
		// mutates the transport, and mutating http.DefaultTransport would
		// install this policy on every other consumer in the process. The
		// clone keeps the system proxy, trust store, and idle connection
		// limits.
		out.Transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	out.Transport = endpointsec.WithResolvedAddressCheck(out.Transport, allowPrivateAddress)
	out.CheckRedirect = endpointsec.RefuseRedirect
	out.Timeout = requestTimeout
	return &out
}

// do performs one API request.
//
// A 5xx is retried, bounded at maxAttempts with retryPause between attempts;
// every other status is returned to the caller, which owns the body and the
// taxonomy. A 429 is deliberately NOT retried: the server is throttling this
// address, and the measurement has no deadline that justifies multiplying
// the load it asked us to reduce.
//
// The request URL never carries the token — it travels in the Authorization
// header — but transport errors quote the URL, so redact() runs on every
// message defensively. No error from this package contains a token or a
// response body.
func (c *Client) do(ctx context.Context, path string, query url.Values) (*http.Response, error) {
	u := c.endpoint + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, fmt.Errorf("building the request: %s", c.redact(err.Error()))
		}
		req.Header.Set("Accept", "application/json")
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}

		resp, err := c.transport.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("request failed: %s", c.redact(err.Error()))
			continue
		}
		if resp.StatusCode < 500 {
			return resp, nil
		}

		// Drain a little so the connection can be reused, then back off.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if attempt == maxAttempts {
			return nil, fmt.Errorf("the datasets-server API kept answering %s after %d "+
				"attempts; it is not healthy", resp.Status, maxAttempts)
		}
		lastErr = fmt.Errorf("the datasets-server API answered %s; retrying", resp.Status)
		if err := sleepCtx(ctx, retryPause); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// redact removes every occurrence of the token from a message. The token
// would otherwise land in a log line, a trace, or a report. Applied at every
// error-construction point rather than at the package boundary, so nothing
// can slip through.
func (c *Client) redact(msg string) string {
	if c.token != "" {
		msg = strings.ReplaceAll(msg, c.token, "[redacted]")
	}
	return msg
}

// sleepCtx sleeps for d, or until ctx is done, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// readBody reads a response body up to maxPageBytes. A body that exceeds the
// cap is a refusal, not a truncation: silently slicing a page would drop
// cases no one asked to drop.
func readBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxPageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading the response: %w", err)
	}
	if len(body) > maxPageBytes {
		return nil, fmt.Errorf("the response body exceeded %d bytes; a page that big is broken, not slow", maxPageBytes)
	}
	return body, nil
}
