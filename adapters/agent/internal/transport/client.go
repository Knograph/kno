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

	// HTTPClient overrides the underlying client. Provided for tests; a caller
	// that supplies one is responsible for its redirect policy.
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

	c := &Client{opts: opts}
	c.http = opts.HTTPClient
	if c.http == nil {
		c.http = &http.Client{
			Timeout:       opts.Timeout,
			CheckRedirect: opts.Dest.CheckRedirect,
			// The default transport honors HTTPS_PROXY from the environment.
			// Kept deliberately: a corporate proxy is a legitimate deployment,
			// and refusing it would push users to disable TLS verification
			// instead — a worse outcome than a proxy they configured on
			// purpose. Stated rather than inherited silently.
			Transport: http.DefaultTransport,
		}
	}
	return c, nil
}

// RoundTrips reports how many requests this client has sent.
//
// Exists so a test can assert exactly one provider call per authorization. A
// count kept on the client side is not proof on its own — the server-side count
// is the real assertion — but a mismatch between the two localizes the bug.
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

	c.roundTrips.Add(1)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, classifyTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	read, err := io.ReadAll(io.LimitReader(resp.Body, c.opts.MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading the response body: %w", ErrTransient, err)
	}
	if int64(len(read)) > c.opts.MaxResponseBytes {
		return nil, fmt.Errorf("transport: the response exceeded %d bytes",
			c.opts.MaxResponseBytes)
	}

	out := &Response{
		StatusCode: resp.StatusCode,
		Body:       read,
		Header:     resp.Header,
		WaitedFor:  waited,
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		if d, ok := RetryAfter(resp.Header, time.Now()); ok {
			c.opts.Limiter.Close(c.opts.Dest.Host(), d)
			out.RetryAfter = d
		}
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

	// RetryAfter is what the provider asked for on a 429, clamped.
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
