package transport_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/agent/internal/transport"
)

func TestDestinationRefusesWhatItMust(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		url    string
		policy transport.Policy
		frag   string
	}{
		{
			name: "a credential in the URL",
			url:  "https://sk-abc123@api.example.com/v1",
			// Refused rather than stripped: the user has already put this in
			// their shell history, and quietly rewriting it hides that.
			frag: "userinfo",
		},
		{
			name: "plain HTTP without opting in",
			url:  "http://api.example.com/v1",
			frag: "plain HTTP",
		},
		{
			name: "instance metadata, with everything opted in",
			url:  "https://169.254.169.254/latest/meta-data/",
			// AllowPrivateAddress deliberately does NOT cover link-local.
			policy: transport.Policy{AllowPrivateAddress: true, AllowInsecureHTTP: true},
			frag:   "link-local",
		},
		{
			name: "loopback without opting in",
			url:  "https://127.0.0.1:8000/v1",
			frag: "private address",
		},
		{
			name:   "an RFC1918 address without opting in",
			url:    "https://10.1.2.3/v1",
			frag:   "private address",
			policy: transport.Policy{},
		},
		{
			name: "a scheme that is not HTTP",
			url:  "file:///etc/passwd",
			frag: "not http or https",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := transport.NewDestination(tc.url, "k", "Authorization", tc.policy)
			if err == nil {
				t.Fatalf("accepted %s", tc.url)
			}
			if !errors.Is(err, transport.ErrRefusedDestination) {
				t.Errorf("err = %v, want ErrRefusedDestination", err)
			}
			if !strings.Contains(err.Error(), tc.frag) {
				t.Errorf("the refusal does not say why (%q):\n%s", tc.frag, err)
			}
		})
	}
}

func TestDestinationAcceptsWhatItShould(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		url    string
		policy transport.Policy
	}{
		{"an ordinary provider", "https://api.openai.com/v1", transport.Policy{}},
		{"a local model server, opted in", "http://localhost:8000/v1",
			transport.Policy{AllowInsecureHTTP: true, AllowPrivateAddress: true}},
		{"a hostname that is not a literal IP", "https://internal.corp/v1", transport.Policy{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := transport.NewDestination(tc.url, "k", "Authorization", tc.policy); err != nil {
				t.Errorf("refused %s: %v", tc.url, err)
			}
		})
	}
}

// TestCrossHostRedirectIsRefused is the x-api-key case.
//
// Go's net/http strips Authorization, WWW-Authenticate, Cookie, and Cookie2 on
// a cross-domain redirect — and NOT x-api-key, which is how Anthropic
// authenticates. So a base URL returning a 302 elsewhere forwards that key
// verbatim, with no plain-HTTP hop to refuse and nothing the user did wrong.
func TestCrossHostRedirectIsRefused(t *testing.T) {
	t.Parallel()

	d, err := transport.NewDestination("https://api.example.com/v1", "k", "x-api-key", transport.Policy{})
	if err != nil {
		t.Fatalf("NewDestination: %v", err)
	}

	elsewhere, _ := url.Parse("https://evil.example.net/v1/messages")
	req := &http.Request{URL: elsewhere}

	err = d.CheckRedirect(req, nil)
	if err == nil {
		t.Fatal("a redirect to another host was followed; an x-api-key would have gone with it")
	}
	if !errors.Is(err, transport.ErrRefusedDestination) {
		t.Errorf("err = %v, want ErrRefusedDestination", err)
	}
	if !strings.Contains(err.Error(), "evil.example.net") {
		t.Errorf("the refusal does not name where it was being sent:\n%s", err)
	}
}

// TestSameHostRedirectIsAllowedButBounded: a version path moving is legitimate;
// a loop is not.
func TestSameHostRedirectIsAllowedButBounded(t *testing.T) {
	t.Parallel()

	d, err := transport.NewDestination("https://api.example.com/v1", "k", "Authorization", transport.Policy{})
	if err != nil {
		t.Fatalf("NewDestination: %v", err)
	}

	same, _ := url.Parse("https://API.example.com./v2/chat")
	req := &http.Request{URL: same}

	// Case and a trailing dot are the same host; refusing them would break a
	// legitimate request.
	if err := d.CheckRedirect(req, nil); err != nil {
		t.Errorf("a same-host redirect was refused: %v", err)
	}

	var via []*http.Request
	for range 5 {
		via = append(via, req)
	}
	if err := d.CheckRedirect(req, via); err == nil {
		t.Error("an unbounded same-host redirect chain was followed")
	}
}

// TestKeyNeverTravelsToAnUnboundHost is the invariant the whole type exists
// for. Whatever got a request pointed elsewhere, the credential does not follow.
func TestKeyNeverTravelsToAnUnboundHost(t *testing.T) {
	t.Parallel()

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// Bind the key to a host the server is NOT on.
	d, err := transport.NewDestination("https://api.example.com/v1", "secret-key",
		"Authorization", transport.Policy{})
	if err != nil {
		t.Fatalf("NewDestination: %v", err)
	}

	c, err := transport.New(transport.Options{
		Dest:       d,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Point the request at the test server by rewriting the destination host —
	// which is exactly what a misconfiguration or a redirect would achieve.
	other, err := transport.NewDestination(srv.URL, "", "", transport.Policy{
		AllowInsecureHTTP: true, AllowPrivateAddress: true,
	})
	if err != nil {
		t.Fatalf("NewDestination for the server: %v", err)
	}
	mismatched, err := transport.New(transport.Options{Dest: other, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := mismatched.Do(t.Context(), http.MethodPost, "/chat", []byte(`{}`)); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("a credential reached a host it was not bound to: %q", gotAuth)
	}
	_ = c
}
