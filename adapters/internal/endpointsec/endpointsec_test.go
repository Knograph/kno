package endpointsec

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestCheckRefusesUnsafeInputs pins the config-time taxonomy: every unsafe
// endpoint is refused with a message naming the problem, and the allow flags
// lift exactly the checks they are named for.
func TestCheckRefusesUnsafeInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		endpoint      string
		allowInsecure bool
		allowPrivate  bool
		want          string
	}{
		{name: "unparseable", endpoint: "://host", want: "could not be parsed"},
		{name: "credential in userinfo", endpoint: "https://sk-secret@host/v1", want: "userinfo"},
		{name: "plain http refused", endpoint: "http://datasets.example.com", want: "plain HTTP"},
		{name: "bad scheme", endpoint: "ftp://datasets.example.com", want: "not http or https"},
		{name: "no host", endpoint: "https://", want: "no host"},
		{name: "loopback literal", endpoint: "https://127.0.0.1:8000", want: "private address"},
		{name: "private literal", endpoint: "https://192.168.1.1", want: "private address"},
		{name: "link-local never overridable", endpoint: "https://169.254.169.254", allowPrivate: true, want: "link-local"},
		{name: "non-canonical ip spelling", endpoint: "https://127.1", want: "neither a hostname nor a canonical IP"},
		{name: "numeric tld", endpoint: "https://datasets.example.123", want: "neither a hostname nor a canonical IP"},
		{name: "hex ip spelling", endpoint: "https://0x7f.0x0.0x0.0x1", want: "neither a hostname nor a canonical IP"},

		{name: "plain http allowed by flag", endpoint: "http://datasets.example.com", allowInsecure: true, want: ""},
		{name: "private allowed by flag", endpoint: "http://127.0.0.1:8000", allowInsecure: true, allowPrivate: true, want: ""},
		{name: "plausible hostname passes", endpoint: "https://datasets-server.huggingface.co", want: ""},
		{name: "hostname with subdomain passes", endpoint: "https://api.datasets.example.co.uk", want: ""},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := Check(tt.endpoint, tt.allowInsecure, tt.allowPrivate)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Check(%q) = %v, want nil", tt.endpoint, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Check(%q) = nil, want error containing %q", tt.endpoint, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Check(%q) error %q does not contain %q", tt.endpoint, err, tt.want)
			}
		})
	}
}

// TestCheckDoesNotEchoTheURL pins the refusal to quote the endpoint back:
// a malformed URL can still carry a credential, and the error is what lands
// in logs and traces.
func TestCheckDoesNotEchoTheURL(t *testing.T) {
	t.Parallel()
	err := Check("http://user:sk-secret@127.0.0.1:8000/v1", false, false)
	if err == nil {
		t.Fatal("Check accepted a credential-bearing endpoint")
	}
	if strings.Contains(err.Error(), "sk-secret") || strings.Contains(err.Error(), "127.0.0.1:8000") {
		t.Fatalf("the error echoes the endpoint back: %q", err)
	}
}

// TestCheckResolvedAppliesThePolicyToTheActualAddress proves the dial-time
// half: a hostname that passes the config-time check is refused once the
// resolver reveals where it actually points.
func TestCheckResolvedAppliesThePolicyToTheActualAddress(t *testing.T) {
	t.Parallel()

	if err := CheckResolved("127.0.0.1:8000", false); err == nil {
		t.Fatal("CheckResolved accepted a loopback address")
	}
	if err := CheckResolved("127.0.0.1:8000", true); err != nil {
		t.Fatalf("CheckResolved refused loopback with the flag set: %v", err)
	}
	if err := CheckResolved("169.254.169.254:80", true); err == nil {
		t.Fatal("CheckResolved accepted link-local even with the flag set")
	}
	if err := CheckResolved("93.184.216.34:443", false); err != nil {
		t.Fatalf("CheckResolved refused a public address: %v", err)
	}
}

// TestDialTimeRecheckRefusesLocalhost drives a real client through
// WithResolvedAddressCheck: "localhost" passes the config-time check (it is
// a plausible hostname) and must be refused at dial time by the Control
// recheck, with no override.
func TestDialTimeRecheckRefusesLocalhost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite the host so the dial-time recheck sees loopback.
	loopback := "http://localhost:" + u.Port()

	if err := Check(loopback, true, false); err != nil {
		t.Fatalf("config-time check refused localhost, which must pass so the "+
			"dial-time recheck can catch it: %v", err)
	}

	client := &http.Client{
		Transport: WithResolvedAddressCheck(http.DefaultTransport.(*http.Transport).Clone(), false),
		Timeout:   5 * time.Second,
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, loopback, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(req)
	if err == nil {
		t.Fatal("the request reached localhost; the dial-time recheck did not refuse it")
	}
	if !strings.Contains(err.Error(), "private address") {
		t.Fatalf("the dial-time refusal does not say what was refused: %v", err)
	}
}

// TestRefuseRedirectPinsTheRedirectPolicy: the CheckRedirect refuses every
// redirect, and its error names the host the credentials would follow.
func TestRefuseRedirectPinsTheRedirectPolicy(t *testing.T) {
	t.Parallel()

	err := RefuseRedirect(&http.Request{URL: &url.URL{Host: "elsewhere.example.com"}}, nil)
	if err == nil {
		t.Fatal("RefuseRedirect accepted a redirect")
	}
	if !strings.Contains(err.Error(), "elsewhere.example.com") {
		t.Fatalf("the refusal does not name the redirect target: %v", err)
	}
}
