package transport_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
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

// TestEveryRedirectIsRefusedIncludingSameHost.
//
// Cross-host is the credential case. Same-host is refused for a different
// reason: GetBody is cleared so net/http cannot silently replay a request, and
// without it the body cannot survive a redirect. A 307/308 comes back as the
// answer — an empty body with no error, which an adapter reads as a broken
// provider — and a 302 is followed as a bodyless GET, delivering a request that
// is not the one the Case describes.
func TestEveryRedirectIsRefusedIncludingSameHost(t *testing.T) {
	t.Parallel()

	d, err := transport.NewDestination("https://api.example.com/v1", "k", "Authorization",
		transport.Policy{})
	if err != nil {
		t.Fatalf("NewDestination: %v", err)
	}

	for _, target := range []string{
		"https://API.example.com./v2/chat", // same host, different spelling
		"https://api.example.com/v2/chat",  // same host
		"https://evil.example.net/v1/chat", // elsewhere
	} {
		u, err := url.Parse(target)
		if err != nil {
			t.Fatalf("parsing %s: %v", target, err)
		}
		err = d.CheckRedirect(&http.Request{URL: u}, []*http.Request{{}})
		if err == nil {
			t.Errorf("a redirect to %s was followed", target)
		}
		if !errors.Is(err, transport.ErrRefusedDestination) {
			t.Errorf("err = %v, want ErrRefusedDestination", err)
		}
	}
}

// TestKeyDoesNotFollowACrossHostRedirect, through a real client.
//
// The previous version of this test built two Destinations and used the one
// with NO key, so authorize returned before the host check ever ran. It passed
// with the entire binding check deleted from the source — a mutation test
// proved it. It was named for the invariant the package exists for and asserted
// nothing about it.
//
// This drives the keyed client at a provider that redirects elsewhere, and
// checks what the second host received. The redirect policy must come from
// New, not from a hand-built client, because that is the path production takes.
func TestKeyDoesNotFollowACrossHostRedirect(t *testing.T) {
	t.Parallel()

	var elsewhereSaw atomic.Value
	elsewhereSaw.Store("")
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereSaw.Store(r.Header.Get("x-api-key"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(elsewhere.Close)

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/v1/messages", http.StatusFound)
	}))
	t.Cleanup(provider.Close)

	d, err := transport.NewDestination(provider.URL, "sk-BOUND-TO-PROVIDER", "x-api-key",
		transport.Policy{AllowInsecureHTTP: true, AllowPrivateAddress: true})
	if err != nil {
		t.Fatalf("NewDestination: %v", err)
	}

	// No HTTPClient: the production path, whose policy this must prove.
	c, err := transport.New(transport.Options{Dest: d})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := c.Do(t.Context(), http.MethodPost, "/v1/messages", []byte(`{}`)); err == nil {
		t.Error("the cross-host redirect was followed")
	}
	if got := elsewhereSaw.Load().(string); got != "" {
		t.Errorf("a credential bound to one host reached another: %q\n"+
			"net/http strips Authorization and Cookie across a redirect, and NOT "+
			"x-api-key, which is how Anthropic authenticates", got)
	}
}

// TestASuppliedClientCannotDisableThePolicy.
//
// Options.HTTPClient exists so a caller can control TLS and transport. It used
// to be handed back verbatim, so supplying one for any reason silently dropped
// CheckRedirect — and a demonstration delivered a bound x-api-key to an
// attacker host through a supported, documented option. Every test in this
// package supplied a client, so the production default was never exercised.
func TestASuppliedClientCannotDisableThePolicy(t *testing.T) {
	t.Parallel()

	var elsewhereSaw atomic.Value
	elsewhereSaw.Store("")
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereSaw.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(elsewhere.Close)

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/chat", http.StatusFound)
	}))
	t.Cleanup(provider.Close)

	d, err := transport.NewDestination(provider.URL, "sk-BOUND", "Authorization",
		transport.Policy{AllowInsecureHTTP: true, AllowPrivateAddress: true})
	if err != nil {
		t.Fatalf("NewDestination: %v", err)
	}

	// A client with a permissive redirect policy of its own. It must not win.
	permissive := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return nil },
	}
	c, err := transport.New(transport.Options{Dest: d, HTTPClient: permissive})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := c.Do(t.Context(), http.MethodPost, "/chat", []byte(`{}`)); err == nil {
		t.Error("a caller-supplied redirect policy overrode the transport's")
	}
	if got := elsewhereSaw.Load().(string); got != "" {
		t.Errorf("a supplied HTTPClient disabled the credential boundary: %q", got)
	}
}

// TestACredentialInAMalformedURLIsNotEchoed.
//
// The parse-failure path ran before the userinfo refusal and quoted the raw URL
// — twice, once from %q and once from url.Parse's own message. It is the one
// error path in this package that can carry a secret, and it was the one the
// redaction test did not cover.
func TestACredentialInAMalformedURLIsNotEchoed(t *testing.T) {
	t.Parallel()

	const secret = "sk-EXAMPLE-MUST-NOT-APPEAR"
	for _, raw := range []string{
		"https://user:" + secret + "@api.example.com:notaport/v1",
		"https://user:" + secret + "@api.example.com/v1\x7f",
		"ht tp://user:" + secret + "@api.example.com/v1",
	} {
		_, err := transport.NewDestination(raw, "k", "Authorization", transport.Policy{})
		if err == nil {
			t.Errorf("accepted %q", raw)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the refusal quotes the credential from a malformed URL:\n%s", err)
		}
	}
}

// TestAddressPolicyCannotBeSpelledAround.
//
// net.ParseIP rejects 127.1, 2130706433 and 0x7f.1; the resolver accepts all
// three as 127.0.0.1. Checking only the typed URL let them through, and a
// demonstration reached loopback with the credential under a zero Policy. A
// hostname resolving to link-local did the same against a rule documented as
// having no override.
func TestAddressPolicyCannotBeSpelledAround(t *testing.T) {
	t.Parallel()

	for _, h := range []string{"127.1", "2130706433", "0x7f.1", "0177.0.0.1"} {
		t.Run(h, func(t *testing.T) {
			t.Parallel()
			_, err := transport.NewDestination("https://"+h+"/v1", "k", "Authorization",
				transport.Policy{})
			if err == nil {
				t.Errorf("%s was accepted; it resolves to loopback", h)
			}
		})
	}

	// And an ordinary hostname is still fine — the check must not reject the
	// normal case in its enthusiasm.
	for _, h := range []string{"api.openai.com", "internal.corp", "xn--80ak6aa92e.com", "host-1.example.co.uk"} {
		if _, err := transport.NewDestination("https://"+h+"/v1", "k", "Authorization",
			transport.Policy{}); err != nil {
			t.Errorf("refused the ordinary hostname %s: %v", h, err)
		}
	}
}

// TestAResolvedPrivateAddressIsRefusedAtDial: the enforcement point. A name
// that looks ordinary and resolves somewhere private is caught where the
// connection is made, which is the only place that cannot be spelled around.
func TestAResolvedPrivateAddressIsRefusedAtDial(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// "localhost" is a plausible DNS name and passes the configuration check;
	// it resolves to loopback, which the dial check refuses.
	host := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	d, err := transport.NewDestination(host, "k", "Authorization",
		transport.Policy{AllowInsecureHTTP: true})
	if err != nil {
		t.Fatalf("NewDestination refused at config time; the dial check is what "+
			"should catch this: %v", err)
	}
	c, err := transport.New(transport.Options{Dest: d})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Do(t.Context(), http.MethodPost, "/chat", []byte(`{}`)); err == nil {
		t.Error("a hostname resolving to loopback was dialed without AllowPrivateAddress")
	}
}

// TestACredentialCannotBeSmuggledThroughHeaders: Headers goes wherever the
// request goes, so a key there escapes the host binding entirely.
func TestACredentialCannotBeSmuggledThroughHeaders(t *testing.T) {
	t.Parallel()

	d, err := transport.NewDestination("https://api.example.com/v1", "", "", transport.Policy{})
	if err != nil {
		t.Fatalf("NewDestination: %v", err)
	}
	_, err = transport.New(transport.Options{
		Dest:    d,
		Headers: http.Header{"Authorization": []string{"Bearer sk-EXAMPLE"}},
	})
	if err == nil {
		t.Fatal("a credential header was accepted through Options.Headers")
	}
	if !errors.Is(err, transport.ErrKeyBinding) {
		t.Errorf("err = %v, want ErrKeyBinding", err)
	}
}
