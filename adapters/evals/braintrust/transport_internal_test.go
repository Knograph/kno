package braintrust

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDefaultClientHonorsSystemTrust(t *testing.T) {
	t.Parallel()

	// A nil Transport becomes a CLONE of DefaultTransport with the dial-time
	// address recheck installed: the system proxy and trust store survive
	// the clone, and the process-global transport is not mutated (a dial
	// hook on http.DefaultTransport would leak this adapter's policy into
	// every other consumer in the process).
	c := newHTTPClient(http.DefaultClient, false)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want a *http.Transport carrying the dial recheck", c.Transport)
	}
	if tr.Proxy == nil {
		t.Error("the clone must keep the system proxy")
	}
	// System trust is preserved, not weakened: the clone carries the same
	// default verification the process uses. (DefaultTransport's
	// TLSClientConfig is populated on this Go — NextProtos for h2 — so the
	// meaningful assertion is that nothing replaced verification.)
	if tc := tr.TLSClientConfig; tc != nil {
		if tc.InsecureSkipVerify {
			t.Error("the clone must not disable TLS verification")
		}
		if tc.RootCAs != nil {
			t.Error("the clone must keep the system roots, not a custom pool")
		}
	}

	// A caller-supplied transport is cloned, not used in place: the dial
	// recheck needs its own dialer, and mutating the caller's transport
	// would install this adapter's policy on every other client sharing it.
	rt := &http.Transport{}
	c = newHTTPClient(&http.Client{Transport: rt}, false)
	if c.Transport == rt {
		t.Error("the caller's transport must be cloned, not mutated in place")
	}
	tr, ok = c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want a *http.Transport", c.Transport)
	}
	if tc := tr.TLSClientConfig; tc != nil {
		if tc.InsecureSkipVerify {
			t.Error("the clone must not disable TLS verification")
		}
		if tc.RootCAs != nil {
			t.Error("the clone must keep the system roots, not a custom pool")
		}
	}
}

func TestClientCopiesAndOverrides(t *testing.T) {
	t.Parallel()

	// The caller's client must not be mutated: the adapter owns its
	// redirect policy and timeout.
	original := &http.Client{
		Timeout: time.Hour,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	c := newHTTPClient(original, false)

	if original.Timeout != time.Hour {
		t.Error("the caller's client was mutated")
	}
	if c.Timeout != requestTimeout {
		t.Errorf("Timeout = %v, want %v", c.Timeout, requestTimeout)
	}
	if c.CheckRedirect == nil {
		t.Fatal("CheckRedirect must be set")
	}
	req := &http.Request{URL: &url.URL{Scheme: "https", Host: "elsewhere.example.com"}}
	if err := c.CheckRedirect(req, nil); err == nil ||
		!strings.Contains(err.Error(), "refusing a redirect") {
		t.Errorf("CheckRedirect = %v, want the refusing policy", err)
	}
}

func TestBearerAuthValue(t *testing.T) {
	t.Parallel()
	// The documented Braintrust scheme: the API key as a Bearer token. The
	// token is the credential — the endpoint checks are what keep it from
	// riding a plain-http connection.
	if got := bearerValue("bt-key"); got != "Bearer bt-key" {
		t.Errorf("bearerValue = %q, want %q", got, "Bearer bt-key")
	}
}

func TestRetryAfterParsing(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		v    string
		want time.Duration
	}{
		{name: "delta seconds", v: "30", want: 30 * time.Second},
		{name: "delta seconds zero", v: "0", want: 0},
		{name: "delta seconds clamps", v: "86400", want: maxRetryAfter},
		{name: "http date", v: "Fri, 28 Aug 2026 12:00:30 GMT", want: 30 * time.Second},
		{name: "http date in the past clamps to zero", v: "Fri, 28 Aug 2020 12:00:00 GMT", want: 0},
		{name: "garbage ignored", v: "soon", want: 0},
		{name: "empty ignored", v: "", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := retryAfter(tt.v, now); got != tt.want {
				t.Errorf("retryAfter(%q) = %v, want %v", tt.v, got, tt.want)
			}
		})
	}
}

func TestPageLimitError(t *testing.T) {
	t.Parallel()

	if err := pageLimitError(maxPages); err != nil {
		t.Errorf("at maxPages the limit must not have tripped, got %v", err)
	}
	err := pageLimitError(maxPages + 1)
	if err == nil {
		t.Fatal("past maxPages the limit must trip")
	}
	if !strings.Contains(err.Error(), "page") {
		t.Errorf("error %q does not say page", err)
	}
}

func TestRedact(t *testing.T) {
	t.Parallel()
	// The credential is the Bearer token and the key itself. The token is
	// computed, never spelled out: a literal "Bearer <key>" in source trips
	// the secrets scan.
	e := &Evals{bearer: bearerValue("test-secret-key"), key: "test-secret-key"}
	msg := "request failed with test-secret-key and again " + e.bearer
	got := e.redact(msg)
	for _, frag := range []string{"test-secret-key", e.bearer} {
		if strings.Contains(got, frag) {
			t.Errorf("redact left %q in %q", frag, got)
		}
	}
	if !strings.Contains(got, "[redacted]") {
		t.Errorf("redact did not mark the credential in %q", got)
	}
	if e.redact("nothing here") != "nothing here" {
		t.Error("redact altered a message with no key")
	}
}

func TestPageReaderHandlesAnyKeyOrder(t *testing.T) {
	t.Parallel()
	// cursor before events: the envelope walk must not assume order.
	body := `{"cursor":"cur-2","events":[{"id":"it-1","input":{"question":"q"}}]}`
	pr, err := newPageReader(bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("newPageReader: %v", err)
	}
	raw, ok, err := pr.nextRow()
	if err != nil {
		t.Fatalf("nextRow: %v", err)
	}
	if !ok {
		t.Fatal("nextRow found no row")
	}
	if !bytes.Contains(raw, []byte(`"it-1"`)) {
		t.Errorf("row = %s, want the event object", raw)
	}
	next, cont := pr.nextCursor()
	if next != "cur-2" || !cont {
		t.Errorf("nextCursor = %q,%v, want cur-2,true", next, cont)
	}
	if _, ok, err := pr.nextRow(); err != nil || ok {
		t.Errorf("nextRow after the last event = %v,%v, want EOF", ok, err)
	}
}

func TestPageReaderRejectsNonObjectEnvelope(t *testing.T) {
	t.Parallel()
	_, err := newPageReader(bytes.NewReader([]byte(`["not","an","envelope"]`)))
	if err == nil {
		t.Fatal("a non-object envelope was accepted")
	}
}

func TestPageReaderIgnoresUnknownFields(t *testing.T) {
	t.Parallel()
	// A server that grows the envelope must not break the walk.
	body := `{"events":[{"id":"it-1","input":{"question":"q"}}],` +
		`"something_new":{"x":1},"cursor":""}`
	pr, err := newPageReader(bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("newPageReader: %v", err)
	}
	if _, ok, err := pr.nextRow(); err != nil || !ok {
		t.Fatalf("nextRow = %v,%v", ok, err)
	}
}

func TestPageReaderRejectsTwoEventsArrays(t *testing.T) {
	t.Parallel()
	// A second events array after the first is a shape the stream was never
	// designed to carry; the refusal is bounded, not a hang.
	body := `{"events":[{"id":"it-1","input":{"question":"q"}}],"events":[],"cursor":""}`
	pr, err := newPageReader(bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("newPageReader: %v", err)
	}
	if _, ok, err := pr.nextRow(); err != nil || !ok {
		t.Fatalf("nextRow = %v,%v", ok, err)
	}
	_, ok, err := pr.nextRow()
	if err == nil || !strings.Contains(err.Error(), "two events arrays") {
		t.Errorf("after the first array, nextRow = %v,%v, want the two-arrays refusal", ok, err)
	}
}

func TestEndpointCheckCopiesTheDestinationPrecedent(t *testing.T) {
	t.Parallel()

	// These cases mirror adapters/evals/langfuse/endpoint.go's behavior,
	// which itself mirrored adapters/evals/langsmith/endpoint.go and
	// adapters/agent/internal/transport/destination.go; the adapter must not
	// drift from the precedent it copied.
	tests := []struct {
		raw      string
		insecure bool
		private  bool
		wantErr  string
	}{
		{raw: "https://api.braintrust.dev", wantErr: ""},
		{raw: "http://api.braintrust.dev", insecure: true, wantErr: ""},
		{raw: "https://10.0.0.5", private: true, wantErr: ""},
		{raw: "https://192.168.1.1", wantErr: "private address"},
		{raw: "http://localhost", wantErr: "plain HTTP"},
		{raw: "https://169.254.169.254", private: true, wantErr: "link-local"},
		{raw: "https://example.com:443", wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			t.Parallel()
			err := checkEndpoint(tt.raw, tt.insecure, tt.private)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("checkEndpoint(%q): %v", tt.raw, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkEndpoint(%q) accepted, want refusal %q", tt.raw, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestCheckResolvedAppliesThePolicyToTheResolvedAddress(t *testing.T) {
	t.Parallel()

	// These mirror the langfuse and langsmith adapters' checkResolved: the
	// address here is what the resolver produced, so "localhost" appears as
	// its literal IP.
	tests := []struct {
		addr    string
		private bool
		wantErr string
	}{
		{addr: "169.254.169.254:80", wantErr: "link-local"},
		{addr: "169.254.169.254", wantErr: "link-local"},
		{addr: "127.0.0.1:8000", wantErr: "private address"},
		{addr: "127.0.0.1:8000", private: true, wantErr: ""},
		{addr: "10.0.0.5:443", wantErr: "private address"},
		{addr: "10.0.0.5:443", private: true, wantErr: ""},
		{addr: "example.com:443", wantErr: ""},
		{addr: "[::1]:8000", wantErr: "private address"},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			t.Parallel()
			err := checkResolved(tt.addr, tt.private)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("checkResolved(%q): %v", tt.addr, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkResolved(%q) accepted, want refusal %q", tt.addr, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

// countingReader counts the bytes a body actually yields, so a test can
// prove the reader stopped far short of the body's end.
type countingReader struct {
	r     io.Reader
	bytes int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.bytes += int64(n)
	return n, err
}

func TestRowCapBindsTheReadNotJustTheRow(t *testing.T) {
	t.Parallel()
	// A row over the cap must fail WITHOUT the whole page being buffered:
	// the memory held at any moment is bounded by maxRowBytes, not by the
	// page. The counting reader proves the underlying stream was not
	// drained — a decoder-based reader would buffer the whole row before
	// its size check, letting a hostile server push a page-sized allocation.
	big := `{"id":"it-big","input":{"question":"` + strings.Repeat("q", (maxRowBytes*3)/2) + `"}}`
	body := `{"events":[` + big + `,{"id":"after"},{"id":"also-after"}],"cursor":""}`
	cr := &countingReader{r: bytes.NewReader([]byte(body))}

	pr, err := newPageReader(cr)
	if err != nil {
		t.Fatalf("newPageReader: %v", err)
	}
	raw, ok, err := pr.nextRow()
	if err == nil {
		t.Fatalf("an oversized row was accepted (ok=%v, raw %d bytes)", ok, len(raw))
	}
	for _, frag := range []string{"it-big", "row cap"} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("error %q does not mention %q", err, frag)
		}
	}
	if cr.bytes >= 2*maxRowBytes {
		t.Errorf("the reader consumed %d bytes, want far less than 2*maxRowBytes; "+
			"the row cap must bound the read, not just the check", cr.bytes)
	}
	if cr.bytes < maxRowBytes/2 {
		t.Errorf("the reader consumed only %d bytes; the oversized row should have been "+
			"read up to the cap before failing", cr.bytes)
	}
}
