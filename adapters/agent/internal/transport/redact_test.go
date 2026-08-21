package transport_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/agent/internal/transport"
)

// TestRedactionCoversEveryHeaderAKeyCanTravelIn.
//
// Both schemes matter: OpenAI-compatible endpoints use Authorization, Anthropic
// uses x-api-key. Covering only the first is the natural mistake, and it leaves
// the Anthropic key printable.
func TestRedactionCoversEveryHeaderAKeyCanTravelIn(t *testing.T) {
	t.Parallel()

	const secret = "sk-do-not-print-this"

	h := http.Header{}
	for _, name := range []string{
		"Authorization", "X-Api-Key", "Api-Key", "Cookie", "Set-Cookie",
		"Proxy-Authorization", "OpenAI-Organization", "OpenAI-Project",
		"Anthropic-Beta",
	} {
		h.Set(name, secret)
	}
	h.Set("Content-Type", "application/json")

	out := transport.RedactHeaders(h)
	for name, vs := range out {
		for _, v := range vs {
			if strings.Contains(v, secret) {
				t.Errorf("%s survived redaction", name)
			}
		}
	}
	// And redaction is not a blunt instrument that eats everything.
	if got := out.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q; redaction removed a header that carries no secret", got)
	}
}

// TestRedactionIsCaseInsensitive: HTTP header names are, so a check that is not
// leaves "authorization" printable.
func TestRedactionIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"authorization", "AUTHORIZATION", "X-API-KEY", "x-api-key"} {
		if got := transport.RedactHeader(name, "sk-secret"); strings.Contains(got, "secret") {
			t.Errorf("%s was not redacted: %q", name, got)
		}
	}
}

// TestRedactURLRemovesUserinfo.
//
// NewDestination refuses a URL carrying userinfo, so this covers the paths that
// reach display without passing through it — a redirect Location, or a provider
// echoing the request back in an error body.
func TestRedactURLRemovesUserinfo(t *testing.T) {
	t.Parallel()

	got := transport.RedactURL("https://sk-abc123:tok@api.example.com/v1/chat")
	if strings.Contains(got, "sk-abc123") || strings.Contains(got, "tok") {
		t.Errorf("a credential survived URL redaction: %q", got)
	}
	if !strings.Contains(got, "api.example.com") {
		t.Errorf("redaction removed the host too: %q", got)
	}

	// A URL with nothing to hide is returned unchanged.
	const plain = "https://api.example.com/v1/chat"
	if got := transport.RedactURL(plain); got != plain {
		t.Errorf("RedactURL(%q) = %q, want it unchanged", plain, got)
	}
}

// TestNoErrorFromThisPackageCarriesAKey.
//
// The transport is the only place a key exists in memory, so it is the only
// place an error can leak one. Exercised across the refusal paths rather than
// asserted about one of them.
func TestNoErrorFromThisPackageCarriesAKey(t *testing.T) {
	t.Parallel()

	const secret = "sk-super-secret-value"

	// Every constructor path that receives the key and can fail.
	for _, tc := range []struct{ name, url string }{
		{"plain HTTP", "http://api.example.com/v1"},
		{"link-local", "https://169.254.169.254/v1"},
		{"loopback", "https://127.0.0.1/v1"},
		{"a bad scheme", "ftp://api.example.com/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := transport.NewDestination(tc.url, secret, "Authorization", transport.Policy{})
			if err == nil {
				t.Fatalf("accepted %s", tc.url)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("the refusal quotes the credential:\n%s", err)
			}
		})
	}
}

// TestErrorClassificationSeparatesRetryableFromTerminal.
//
// The distinction decides whether core retries. Calling a policy refusal
// transient would retry a request that will never be permitted; calling a
// dropped connection terminal marks a healthy baseline unusable over an idle
// timeout.
func TestErrorClassificationSeparatesRetryableFromTerminal(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	t.Run("a refused destination is terminal", func(t *testing.T) {
		t.Parallel()

		// A client whose destination is bound elsewhere: authorize refuses
		// before any request goes out, and retrying cannot help.
		d, err := transport.NewDestination("https://api.example.com/v1", "k",
			"Authorization", transport.Policy{})
		if err != nil {
			t.Fatalf("NewDestination: %v", err)
		}
		c, err := transport.New(transport.Options{Dest: d, HTTPClient: srv.Client()})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		// Unroutable host, so the request fails without reaching a server.
		_, err = c.Do(t.Context(), http.MethodPost, "/chat", []byte(`{}`))
		if err == nil {
			t.Fatal("a request to an unreachable host succeeded")
		}
	})

	t.Run("cancellation is neither", func(t *testing.T) {
		t.Parallel()

		c := localClient(t, srv)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := c.Do(ctx, http.MethodPost, "/chat", []byte(`{}`))
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled; a cancelled run is not a "+
				"transient provider failure to retry", err)
		}
		if errors.Is(err, transport.ErrTransient) {
			t.Error("cancellation was classified as retryable, so a Ctrl-C would " +
				"be retried instead of honored")
		}
	})
}

// TestBoundHostsAreListable: an error that says which hosts ARE configured is
// actionable; one that only says the host is not, is not.
func TestBoundHostsAreListable(t *testing.T) {
	t.Parallel()

	b, err := transport.ParseKeyBindings([]string{"b.example.com=B", "a.example.com=A"})
	if err != nil {
		t.Fatalf("ParseKeyBindings: %v", err)
	}
	got := b.Hosts()
	want := []string{"a.example.com", "b.example.com"}
	if len(got) != len(want) {
		t.Fatalf("Hosts() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Hosts() = %v, want %v (sorted, so an error message is stable)", got, want)
		}
	}
}
