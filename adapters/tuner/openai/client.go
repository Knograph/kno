package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/knograph/kno/adapters/internal/endpointsec"
	"github.com/knograph/kno/core/errs"
)

// This file is this package's HTTP transport, built directly on
// adapters/internal/endpointsec — the shared, already-lifted answer to the
// exact problem adapters/tuner/together/security.go's own doc names: a
// Tuner adapter cannot import adapters/agent/internal/transport (Go
// `internal` to adapters/agent/), and reimplementing its policy locally is
// the second copy of it in this repository.
//
// endpointsec is the THIRD copy made explicit — its own package doc names
// the agent transport, LangSmith and Langfuse as the first three, and says
// plainly: "New adapters must use this package instead of copying a
// fourth time." adapters/tuner/together/security.go was written without
// that package in view (its own doc calls the agent transport's
// unreachability "a genuine plan-vs-code contradiction" and proposes
// exactly the shared package endpointsec already is, one directory over)
// and so carries a fourth copy today. This package does not add a fifth:
// it imports endpointsec directly, which is the reason a NEW local
// security.go is not written here at all. See this PR's report and
// docs/debt.md for the entry tracking together's un-migrated copy.
const (
	defaultTimeout   = 120 * time.Second
	maxResponseBytes = 8 << 20 // job/status/model bodies are small JSON documents, not conversation content
)

// errRedirectRefused means the server tried to redirect this client
// somewhere else. endpointsec.RefuseRedirect is the same policy; this
// package installs its own copy of the SENTINEL (not the policy) so
// errors.go's fromTransport can classify it without endpointsec exporting
// one, matching endpointsec.RefuseRedirect's own error text.
var errRedirectRefused = errors.New("openai: refusing to follow a redirect off the bound host")

// errTransientTransport marks a failure that happened before any provider
// status code was seen — a dial failure, a reset connection, a body that
// ended early. Wrapped by fromTransport onto errs.ErrTransportTransient.
var errTransientTransport = errors.New("openai: transient transport failure")

// httpResponse is one reply, already read and bounded.
type httpResponse struct {
	StatusCode int
	Body       []byte
}

// secureClient is a minimal HTTP client enforcing endpointsec's policy
// plus this package's own bounded-response-size and redirect-refusal
// wiring. Safe for concurrent use — http.Client is.
type secureClient struct {
	http *http.Client
	base *url.URL
}

func newSecureClient(baseURL string, allowInsecureHTTP, allowPrivate bool, timeout time.Duration, httpClient *http.Client) (*secureClient, error) {
	if err := endpointsec.Check(baseURL, allowInsecureHTTP, allowPrivate); err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("openai: %q is not a valid base URL", baseURL)
	}

	out := http.Client{}
	if httpClient != nil {
		out = *httpClient // copy: never mutate a client the caller still holds
	}
	if out.Transport == nil {
		// Clone the process default rather than use it: endpointsec's wrap
		// mutates the transport, and mutating http.DefaultTransport would
		// install this policy on every other consumer in the process.
		out.Transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	// Config-time check is endpointsec.Check above; this is the dial-time
	// recheck against the RESOLVED address, which a hostname's config-time
	// check cannot see — see endpointsec's own doc.
	out.Transport = endpointsec.WithResolvedAddressCheck(out.Transport, allowPrivate)
	out.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errRedirectRefused
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	out.Timeout = timeout

	return &secureClient{http: &out, base: u}, nil
}

func (c *secureClient) do(ctx context.Context, method, path string, body []byte, headers http.Header) (*httpResponse, error) {
	u := *c.base
	// path may carry its own "?query" (ListJobs's metadata filter) — split
	// it off before joining onto base's Path, or the query string ends up
	// URL-escaped INTO the path itself and every route 404s.
	route, rawQuery, hasQuery := strings.Cut(path, "?")
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + strings.TrimPrefix(route, "/")
	if hasQuery {
		u.RawQuery = rawQuery
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, fmt.Errorf("openai: building the request: %w", err)
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, errRedirectRefused) {
			return nil, errRedirectRefused
		}
		return nil, fmt.Errorf("%w: %w", errTransientTransport, err)
	}
	defer func() { _ = resp.Body.Close() }()

	read, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading the response body: %w", errTransientTransport, err)
	}
	if int64(len(read)) > maxResponseBytes {
		return nil, fmt.Errorf("openai: the response exceeded %d bytes", maxResponseBytes)
	}
	return &httpResponse{StatusCode: resp.StatusCode, Body: read}, nil
}

// hostOf extracts the host from a base URL, refusing one with none.
func hostOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", errs.ErrInvalidInput.
			WithFix("write --base-url as a full URL, for example https://api.openai.com").
			Wrap(fmt.Errorf("openai: the base URL has no host"))
	}
	return u.Host, nil
}

// resolveKey finds the credential bound to host, applying defaultEnvVar
// ONLY when host is exactly defaultHost — the same rule
// transport.KeyBindings.Resolve enforces (adapters/agent/internal/transport),
// reimplemented locally because that package is unreachable from here, per
// this file's doc. keyEnv is a direct host->env-var map (Options.KeyEnv);
// unlike transport.ParseKeyBindings this does not validate that a bound
// name "looks like" an environment variable rather than a raw key — the
// same reduced-safety-net tradeoff together/security.go's resolveKey
// documents, carried forward here since endpointsec does not cover
// credential BINDING (only the endpoint-safety policy).
func resolveKey(host, defaultHost, defaultEnvVar string, keyEnv map[string]string) (key, envVar string) {
	h := normalizeHost(host)
	for k, v := range keyEnv {
		if normalizeHost(k) == h {
			return os.Getenv(v), v
		}
	}
	if defaultEnvVar != "" && h == normalizeHost(defaultHost) {
		return os.Getenv(defaultEnvVar), defaultEnvVar
	}
	return "", ""
}

func normalizeHost(h string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
}
