package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Defaults for a Client. Every one is overridable; none is zero, because a zero
// timeout in net/http means "wait forever" and a run that hangs on one Case is
// worse than one that errors it.
const (
	// DefaultTimeout bounds a single request end to end.
	DefaultTimeout = 120 * time.Second

	// DefaultMaxResponseBytes bounds what a provider can make us hold in
	// memory. A response is unmarshaled into a Case outcome and persisted, so
	// an endpoint returning gigabytes is a memory exhaustion vector as well as
	// a storage one.
	DefaultMaxResponseBytes = 32 << 20 // 32 MiB
)

// Options configure a Client.
type Options struct {
	// Dest is where requests go and which credential travels there. Required.
	Dest *Destination

	// Limiter paces requests per host. Nil creates one.
	Limiter *Limiter

	// Timeout bounds a single request. Zero uses DefaultTimeout.
	Timeout time.Duration

	// MaxResponseBytes bounds a response body. Zero uses the default.
	MaxResponseBytes int64

	// UserAgent identifies Kno to the provider.
	UserAgent string

	// Headers are sent on every request. An adapter uses this for anything
	// constant across calls — an API version, an Idempotency-Key.
	//
	// A credential does NOT go here. Destination owns that, because it is the
	// only thing that knows which host the key is bound to.
	Headers http.Header

	// HTTPClient supplies the underlying client's TRANSPORT and TLS settings.
	//
	// Its redirect policy and timeout are overwritten, always. A caller
	// legitimately needs to control how connections are made — a custom TLS
	// config, a test server's client — and never needs to control where a
	// credential may travel. An earlier version honored the caller's
	// CheckRedirect, which meant supplying a client for any reason silently
	// disabled cross-host redirect refusal; a demonstration delivered a bound
	// x-api-key to an attacker host through this field.
	HTTPClient *http.Client
}

// Client is the shared HTTP layer.
//
// It does NOT retry. That is not an omission: core owns retry, because each
// attempt takes its own budget reservation and settles its own call, and a
// transport retrying underneath would make N provider calls inside one
// reservation and settle them as one — turning --max-calls 1000 into up to 3000
// real calls.
//
// "Does not retry" is enforced rather than asserted. net/http replays a request
// on a reused connection when nothing was written, provided Request.GetBody is
// set — which http.NewRequest fills in automatically for a bytes.Reader body.
// So GetBody is cleared explicitly, and RoundTrips counts what actually went
// out so a test can assert one per call at the server.
type Client struct {
	opts Options
	http *http.Client

	// roundTrips counts requests that left this client.
	roundTrips atomic.Int64
}

// New builds a Client.
func New(opts Options) (*Client, error) {
	if opts.Dest == nil {
		return nil, errors.New("transport: a destination is required")
	}
	if opts.Limiter == nil {
		opts.Limiter = NewLimiter()
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = DefaultMaxResponseBytes
	}

	// Reject a credential smuggled in through Headers rather than through the
	// Destination that knows which host it is bound to.
	for name := range opts.Headers {
		if RedactHeader(name, "x") == redacted {
			return nil, fmt.Errorf("%w: %s belongs on the Destination, which knows "+
				"which host it may travel to; a header set here would go wherever "+
				"the request goes", ErrKeyBinding, name)
		}
	}

	c := &Client{opts: opts}

	// The caller's client supplies transport and TLS. Policy is ours, always:
	// a shallow copy so their value is not mutated, then the two fields that
	// decide where a credential may go are overwritten unconditionally.
	base := http.Client{}
	if opts.HTTPClient != nil {
		base = *opts.HTTPClient
	}
	if base.Transport == nil {
		base.Transport = defaultTransport()
	}
	base.Transport = withResolvedAddressCheck(base.Transport, opts.Dest)
	base.Transport = &guardedTransport{next: base.Transport, dest: opts.Dest, count: &c.roundTrips}
	base.Timeout = opts.Timeout
	base.CheckRedirect = opts.Dest.CheckRedirect

	c.http = &base
	return c, nil
}

// defaultTransport is a private connection pool.
//
// Not http.DefaultTransport: that is process-global, so CloseIdleConnections
// here would empty every other consumer's pool and per-host idle limits would
// be shared with code that knows nothing about this one.
//
// HTTPS_PROXY from the environment is honored deliberately. A corporate proxy
// is a legitimate deployment, and refusing it would push users toward disabling
// TLS verification, which is worse than a proxy they configured on purpose.
// Note this reasoning holds for HTTPS: the proxy sees a CONNECT and TLS stays
// end to end. For plain HTTP a proxy sees the credential, which is one more
// reason AllowInsecureHTTP is opt-in.
func defaultTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ForceAttemptHTTP2 = true
	return t
}

// withResolvedAddressCheck wraps a transport's dialer so the address policy is
// applied to what the resolver produced.
//
// This is the enforcement point for the private-address rules. Checking the URL
// alone is bypassable by spelling — net.ParseIP rejects 127.1, 2130706433, and
// 0x7f.1 while the resolver accepts all three as 127.0.0.1 — and by any
// hostname that happens to resolve somewhere private.
func withResolvedAddressCheck(t http.RoundTripper, d *Destination) http.RoundTripper {
	tr, ok := t.(*http.Transport)
	if !ok {
		// A caller supplied something that is not an *http.Transport (a test
		// stub, a recording round-tripper). There is no dialer to reach; the
		// configuration-time check is what applies.
		return t
	}
	// Clone before touching it. Mutating the caller's transport in place would
	// install THIS destination's policy on every other client sharing it —
	// caught immediately when two clients in one test, with deliberately
	// different policies, started refusing each other's destinations.
	tr = tr.Clone()

	// Control, not DialContext. DialContext receives the address as WRITTEN —
	// "localhost:8000" — because Go resolves inside the dialer, so a check
	// there re-reads the hostname it was already given and proves nothing.
	// Control runs once per resolved address, immediately before connect, with
	// the literal IP. That is the only point where "where is this actually
	// going" has an answer.
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			return d.checkResolved(address)
		},
	}
	tr.DialContext = dialer.DialContext
	return tr
}

// guardedTransport applies the address policy to the address a connection is
// actually being made to, and counts what actually goes out.
//
// Both belong here rather than in Do. The address check is only meaningful
// against a resolved address — a hostname can be spelled to look ordinary and
// resolve to link-local — and the round-trip count is only truthful if it
// counts round trips rather than calls, since a followed same-host redirect is
// two of the former and one of the latter.
type guardedTransport struct {
	next  http.RoundTripper
	dest  *Destination
	count *atomic.Int64
}

func (g *guardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	g.count.Add(1)
	return g.next.RoundTrip(req)
}

// RoundTrips reports how many requests actually went out.
//
// Counted in the RoundTripper, not in Do: a followed same-host redirect is two
// round trips and one call, and a counter that could not tell them apart would
// report the number it was asked to rather than the number that happened.
func (c *Client) RoundTrips() int64 { return c.roundTrips.Load() }

// Do sends one request and returns its body.
//
// Exactly one round trip, no retry, no redirect off the bound host. Rate-limit
// waiting happens before the request; classification happens after.
func (c *Client) Do(ctx context.Context, method, path string, body []byte) (*Response, error) {
	waited, err := c.opts.Limiter.Wait(ctx, c.opts.Dest.Host())
	if err != nil {
		return nil, err
	}

	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, classifyTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Rate-limit handling runs on the STATUS, before any verdict about the
	// body. A throttling gateway that answers 429 with a verbose HTML error
	// page would otherwise have its signal discarded by the size refusal, and
	// the limiter would never close — defeating the mechanism at exactly the
	// moment it is needed.
	var retryAfter time.Duration
	if resp.StatusCode == http.StatusTooManyRequests {
		d, ok := RetryAfter(resp.Header, time.Now())
		if !ok {
			// A bare 429 is common. Treating "no Retry-After" as "no wait"
			// leaves the gate open and hammers a provider that just refused.
			d = defaultRateLimitHold
		}
		c.opts.Limiter.Close(c.opts.Dest.Host(), d)
		retryAfter = d
	}

	read, err := io.ReadAll(io.LimitReader(resp.Body, c.opts.MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading the response body: %w", ErrTransient, err)
	}
	if int64(len(read)) > c.opts.MaxResponseBytes {
		return nil, fmt.Errorf("%w: the response exceeded %d bytes",
			ErrResponseTooLarge, c.opts.MaxResponseBytes)
	}

	out := &Response{
		StatusCode: resp.StatusCode,
		Body:       read,
		Header:     resp.Header,
		WaitedFor:  waited,
		RetryAfter: retryAfter,
	}

	return out, nil
}

// Response is one provider reply, already read and bounded.
type Response struct {
	StatusCode int
	Body       []byte
	Header     http.Header

	// WaitedFor is how long the limiter held this request back, so a caller can
	// report a run that is deliberately idle rather than hung.
	WaitedFor time.Duration

	// RetryAfter is how long the host was closed for on a 429 — the provider's
	// own Retry-After where it gave one, clamped, and a default otherwise.
	RetryAfter time.Duration
}

func (c *Client) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	u := *c.opts.Dest.base
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + strings.TrimPrefix(path, "/")

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return nil, fmt.Errorf("transport: building the request: %w", err)
	}

	// The line that makes "does not retry" true. http.NewRequest sets GetBody
	// for a bytes.Reader, and net/http uses it to replay a request on a reused
	// connection. Benign for money today — the provider never saw the first
	// attempt — but it becomes a real double-charge the moment an adapter sets
	// an Idempotency-Key, because isReplayable then permits replay on
	// transportReadFromServerError, where bytes WERE written.
	req.GetBody = nil

	for k, vs := range c.opts.Headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	if c.opts.UserAgent != "" {
		req.Header.Set("User-Agent", c.opts.UserAgent)
	}
	if err := c.opts.Dest.authorize(req); err != nil {
		return nil, err
	}
	return req, nil
}

// ErrResponseTooLarge means a provider returned more than the configured
// ceiling. Terminal: a body that big will be that big again.
var ErrResponseTooLarge = errors.New("transport: response too large")

// defaultRateLimitHold is how long a bare 429 closes a host for.
//
// Short enough that a spurious 429 does not stall a run, long enough that the
// next attempt is not immediate. A provider that knows better says so with
// Retry-After and that value is used instead.
const defaultRateLimitHold = 5 * time.Second

// ErrTransient means the request failed in a way that may succeed if retried,
// with no evidence the provider processed it.
//
// core decides whether to retry; the transport only classifies. The distinction
// matters because a stale pooled connection is not an agent error: at
// concurrency, any pause in a long run produces a handful of them, and treating
// them as terminal marks a healthy baseline unusable over an idle timeout.
var ErrTransient = errors.New("transport: transient failure")

// classifyTransportError separates "the provider never saw this" from "the
// provider answered badly".
func classifyTransportError(err error) error {
	if err == nil {
		return nil
	}
	// Refusals from our own policy are terminal: a redirect off the bound host
	// or a credential mismatch will not resolve by trying again.
	if errors.Is(err, ErrRefusedDestination) || errors.Is(err, ErrKeyBinding) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("%w: %w", ErrTransient, err)
	}
	// A connection reset or an idle connection closed underneath us. The
	// request did not reach a handler, so retrying it charges nothing twice.
	if isConnectionError(err) {
		return fmt.Errorf("%w: %w", ErrTransient, err)
	}
	return err
}

func isConnectionError(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := err.Error()
	for _, s := range []string{
		"connection reset by peer",
		"connection refused",
		"server closed idle connection",
		"unexpected EOF",
		"broken pipe",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
