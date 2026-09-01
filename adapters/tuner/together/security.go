package together

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// This file is a REDUCED, LOCAL security layer for this adapter's HTTP
// calls — NOT adapters/agent/internal/transport ported wholesale, which is
// what the tuner-bridge plan's Step 5 calls for ("credentials env-only
// through transport.ParseKeyBindings / KeyBindings.Resolve... endpointsec-
// equivalent redirect refusal, plain-HTTP refusal, private-address
// refusal").
//
// THIS IS A GENUINE PLAN-VS-CODE CONTRADICTION, not a simplification of
// convenience: adapters/agent/internal/transport is a Go `internal`
// package rooted at adapters/agent/, and Go's internal-package visibility
// rule means only code whose import path has adapters/agent as an
// ancestor may import it. adapters/tuner/together does not, and the
// compiler refuses the import outright — confirmed by attempting exactly
// that import while writing this file. The plan's own Step 5 states BOTH
// "internal/transport is internal to adapters/agent" (true, and given AS
// THE REASON Options stays plain Go) and that this package should reuse
// transport's exported API (not reachable from here) in the same sentence.
//
// What ships here covers the properties that matter for THIS package's
// safety — redirect refusal, plain-HTTP refusal, private-address refusal
// with an opt-out, credentials bound to one host by an explicit
// host->env-var map, no header ever logged or included in an error — using
// stdlib only. It does NOT replicate transport's fuller edge-case handling:
// its secret-shaped-env-var-name heuristic, its IPv6 binding-syntax
// checks, its per-host rate limiter honoring Retry-After, or its
// TestFixturesCarryNothingTheyShouldNot-style allowlisted fixture
// discipline. See this PR's report — the real fix is a shared package
// (e.g. adapters/internal/transport, one level up from both adapters/agent
// and adapters/tuner) that both directories can import, which is a
// multi-file relocation across every existing Agent adapter and is out of
// scope for this PR.
const (
	defaultTimeout   = 120 * time.Second
	maxResponseBytes = 8 << 20 // 8 MiB: job/status/endpoint bodies are small JSON documents, not conversation content
)

// errRedirectRefused means the server tried to redirect this client
// somewhere else. Refused rather than followed: a redirect off the bound
// host is exactly how a credential travels to a place nobody consented to
// send it.
var errRedirectRefused = errors.New("together: refusing to follow a redirect off the bound host")

// secureClient is a minimal HTTP client enforcing this package's transport
// policy. Safe for concurrent use — http.Client is.
type secureClient struct {
	http *http.Client
	base *url.URL
}

func newSecureClient(baseURL string, allowInsecureHTTP, allowPrivate bool, timeout time.Duration, httpClient *http.Client) (*secureClient, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("together: %q is not a valid base URL", baseURL)
	}
	if u.User != nil {
		return nil, errors.New("together: the base URL carries a credential in its userinfo; " +
			"remove it and bind the key through the environment instead")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowInsecureHTTP {
			return nil, fmt.Errorf(
				"together: %s is plain HTTP, which sends the request and any credential in the clear",
				u.Host,
			)
		}
	default:
		return nil, fmt.Errorf("together: scheme %q is not http or https", u.Scheme)
	}
	if !allowPrivate {
		if err := refusePrivateAddress(u.Hostname()); err != nil {
			return nil, err
		}
	}

	var transport http.RoundTripper
	if httpClient != nil {
		transport = httpClient.Transport
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &secureClient{
		http: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errRedirectRefused
			},
		},
		base: u,
	}, nil
}

// refusePrivateAddress refuses loopback, RFC1918, and link-local
// addresses. A DNS name is resolved and every returned address is
// checked — refusing only the literal host string would let a name that
// resolves to a private address through untouched.
func refusePrivateAddress(host string) error {
	if ip := net.ParseIP(host); ip != nil {
		return checkIP(ip)
	}
	ips, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
	if err != nil {
		// Unresolvable now; the real request will fail with a clearer error
		// than a resolution hiccup here would produce.
		return nil
	}
	for _, ip := range ips {
		if err := checkIP(ip.IP); err != nil {
			return err
		}
	}
	return nil
}

func checkIP(ip net.IP) error {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf(
			"together: %s resolves to a private or loopback address; "+
				"pass AllowPrivateAddress if this is deliberate (e.g. a local proxy)", ip,
		)
	}
	return nil
}

// httpResponse is one reply, already read and bounded.
type httpResponse struct {
	StatusCode int
	Body       []byte
}

func (c *secureClient) do(ctx context.Context, method, path string, body []byte, headers http.Header) (*httpResponse, error) {
	u := *c.base
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + strings.TrimPrefix(path, "/")

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, fmt.Errorf("together: building the request: %w", err)
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
		return nil, fmt.Errorf("together: the response exceeded %d bytes", maxResponseBytes)
	}
	return &httpResponse{StatusCode: resp.StatusCode, Body: read}, nil
}

// errTransientTransport marks a failure that happened before any provider status
// code was seen — a dial failure, a reset connection, a body that ended
// early. Wrapped by fromTransport onto errs.ErrTransportTransient.
var errTransientTransport = errors.New("together: transient transport failure")

// resolveKey finds the credential bound to host, applying defaultEnvVar
// ONLY when host is exactly defaultHost — the same rule
// transport.KeyBindings.Resolve enforces, reimplemented locally per this
// file's doc. keyEnv is a direct host->env-var map (Options.KeyEnv);
// unlike transport.ParseKeyBindings this does not validate that a bound
// name "looks like" an environment variable rather than a raw key — a
// smaller safety net than the ported package would have had.
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
