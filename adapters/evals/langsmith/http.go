package langsmith

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// maxRetryAfter clamps a server's Retry-After. The pagination is a read of a
// bounded dataset; a server demanding minutes of waiting is broken, and a
// minute is long enough for any honest throttle.
const maxRetryAfter = 60 * time.Second

// newHTTPClient builds the client every request goes through.
//
// The caller's client contributes transport and TLS settings only: the
// redirect policy is always the refusing one, because a redirect would carry
// the x-api-key header to a host the user never named, and the timeout is
// always the page timeout, because a page bigger or slower than the caps is
// broken, not slow. Same contract as adapters/agent/openaicompat's
// Options.HTTPClient.
//
// The private-address policy is installed as a dial-time recheck of the
// RESOLVED address, mirroring the agent transport — the config-time check in
// checkEndpoint cannot see where a hostname resolves. A transport the caller
// supplied that is not an *http.Transport (a test stub) has no dialer to
// reach, so the config-time check is what applies to it.
func newHTTPClient(given *http.Client, allowPrivateAddress bool) *http.Client {
	out := http.Client{}
	if given != nil {
		out = *given // copy: never mutate a client the caller still holds
	}
	if out.Transport == nil {
		// Clone the process default rather than use it: the check below
		// mutates the transport, and mutating http.DefaultTransport would
		// install this adapter's policy on every other consumer in the
		// process. The clone keeps the system proxy, trust store, and idle
		// connection limits.
		out.Transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	out.Transport = withResolvedAddressCheck(out.Transport, allowPrivateAddress)
	out.CheckRedirect = refuseRedirect
	out.Timeout = requestTimeout
	return &out
}

// withResolvedAddressCheck wraps a transport's dialer so the address policy
// is applied to what the resolver produced. Mirror of
// adapters/agent/internal/transport/client.go's withResolvedAddressCheck.
func withResolvedAddressCheck(t http.RoundTripper, allowPrivateAddress bool) http.RoundTripper {
	tr, ok := t.(*http.Transport)
	if !ok {
		// A caller supplied something that is not an *http.Transport (a test
		// stub, a recording round-tripper). There is no dialer to reach; the
		// configuration-time check is what applies.
		return t
	}
	// Clone before touching it. Mutating the caller's transport in place
	// would install THIS adapter's policy on every other client sharing it.
	tr = tr.Clone()

	// Control, not DialContext. DialContext receives the address as WRITTEN —
	// "localhost:8000" — because Go resolves inside the dialer, so a check
	// there re-reads the hostname it was already given and proves nothing.
	// Control runs once per resolved address, immediately before connect,
	// with the literal IP. That is the only point where "where is this
	// actually going" has an answer.
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			return checkResolved(address, allowPrivateAddress)
		},
	}
	tr.DialContext = dialer.DialContext
	return tr
}

// refuseRedirect refuses every redirect.
//
// Go's net/http strips Authorization and Cookie headers on a cross-host
// redirect but NOT x-api-key, so a 302 from the configured endpoint would
// forward the credential verbatim to wherever it points. Refusing all
// redirects — same host or not — is the only safe policy, and the datasets
// and examples endpoints have no legitimate reason to redirect.
func refuseRedirect(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("langsmith: refusing a redirect to %s; the API key must not follow one", req.URL.Host)
}

// do performs one API request, retrying a 429 with the server's Retry-After.
//
// No other status is retried: a 4xx is a permanent error and retrying it
// would only multiply requests against a provider that already refused. The
// caller owns the returned response body.
//
// No error ever contains the API key or a response body.
func (e *Evals) do(ctx context.Context, path string, query url.Values) (*http.Response, error) {
	u := e.endpoint + path
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
			return nil, fmt.Errorf("langsmith: building the request: %s", e.redact(err.Error()))
		}
		req.Header.Set("x-api-key", e.key)
		req.Header.Set("Accept", "application/json")

		resp, err := e.transport.Do(req)
		if err != nil {
			// The transport error quotes the request URL; the key is not in
			// it, but redact defensively so a future change to the URL
			// cannot leak the credential.
			lastErr = fmt.Errorf("langsmith: request failed: %s", e.redact(err.Error()))
			continue
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}

		// 429: the server asked us to slow down. Drain a little so the
		// connection can be reused, then back off with Retry-After.
		wait := retryAfter(resp.Header.Get("Retry-After"), time.Now())
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if attempt == maxAttempts {
			lastErr = fmt.Errorf("langsmith: the dataset API kept answering 429 after %d "+
				"attempts; it is throttling this address", maxAttempts)
			continue
		}
		lastErr = fmt.Errorf("langsmith: the dataset API answered 429; retrying in %s", wait)
		if err := sleepCtx(ctx, wait); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// retryAfter parses a Retry-After header: either RFC 9110 delta-seconds or
// an HTTP-date. A value that parses as neither is 0 — retry immediately.
// Anything longer than maxRetryAfter is clamped to it.
func retryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if sec, err := strconv.Atoi(v); err == nil {
		if sec < 0 {
			return 0
		}
		d := time.Duration(sec) * time.Second
		if d > maxRetryAfter {
			return maxRetryAfter
		}
		return d
	}
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		if d < 0 {
			return 0
		}
		if d > maxRetryAfter {
			return maxRetryAfter
		}
		return d
	}
	return 0
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

// redact removes every occurrence of the API key from a message.
//
// Errors must never carry the credential — it would land in a log line, a
// trace, or a report. Applied at every error-construction point rather than
// at the package boundary, so nothing can slip through.
func (e *Evals) redact(msg string) string {
	return strings.ReplaceAll(msg, e.key, "[redacted]")
}

// pageLimitError reports the pagination backstop once more than maxPages
// pages have been fetched. The seen-cursor check catches a server that
// repeats itself; this catches a server that mints a fresh cursor per page
// and never runs out.
func pageLimitError(fetched int) error {
	if fetched > maxPages {
		return fmt.Errorf("langsmith: pagination exceeded %d pages; the dataset keeps "+
			"issuing fresh cursors, so the fetch stops here rather than spin", maxPages)
	}
	return nil
}
