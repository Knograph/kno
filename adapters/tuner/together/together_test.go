package together_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/knograph/kno/adapters/tuner/together"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
)

// bindKey returns the --key-env-shaped binding a caller needs when pointing
// --base-url at anything other than Together's own default host — the same
// requirement anthropic's tests bind for a test server's host, and for the
// same reason: DefaultKeyEnv applies ONLY to DefaultBaseURL's host, so a
// local fixture server gets no credential without an explicit binding.
func bindKey(t *testing.T, rawURL string) map[string]string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	return map[string]string{u.Host: "TOGETHER_API_KEY"}
}

// TestNewRefusesWithNoCredential pins that a missing TOGETHER_API_KEY is
// refused at construction — before planning, before any job — naming the
// variable, per the plan's Step 5 posture.
func TestNewRefusesWithNoCredential(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "")

	_, err := together.New(together.Options{})
	if err == nil {
		t.Fatal("want a refusal with no credential set")
	}
	if !errors.Is(err, together.ErrAuthentication) {
		t.Errorf("err = %v, want together.ErrAuthentication", err)
	}
}

// TestNewRefusesPlainHTTP pins the plain-HTTP refusal, matching the same
// posture anthropic and every other Agent adapter enforce.
func TestNewRefusesPlainHTTP(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "sk-test")

	_, err := together.New(together.Options{BaseURL: "http://api.together.xyz"})
	if err == nil {
		t.Fatal("want a refusal for a plain-HTTP base URL")
	}
}

// TestNewRefusesPrivateAddressByDefault pins that a base URL resolving to a
// loopback or private address is refused unless AllowPrivateAddress is set.
func TestNewRefusesPrivateAddressByDefault(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "sk-test")

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	_, err := together.New(together.Options{BaseURL: srv.URL, AllowInsecureBaseURL: true})
	if err == nil {
		t.Fatal("want a refusal: httptest.Server binds to 127.0.0.1, a private address")
	}

	// The opt-out makes it constructible.
	if _, err := together.New(together.Options{BaseURL: srv.URL, AllowPrivateAddress: true, AllowInsecureBaseURL: true, KeyEnv: bindKey(t, srv.URL)}); err != nil {
		t.Errorf("New with AllowPrivateAddress: %v", err)
	}
}

// TestSubmitAndStatusRoundTrip drives Submit and Status against a fixture
// HTTP server built from this adapter's own hand-authored (verify) request
// and response shapes — this is a self-consistency test of the adapter's
// own parsing, not a confirmation of Together's real wire format. See the
// package doc's PROVENANCE WARNING.
func TestSubmitAndStatusRoundTrip(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "sk-test")

	var sawAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/fine-tunes", func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "ft-abc123", "status": "pending", "created_at": "2026-08-31T00:00:00Z",
		})
	})
	mux.HandleFunc("/v1/fine-tunes/ft-abc123", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "ft-abc123", "status": "running", "progress": 0.5,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tuner, err := together.New(together.Options{BaseURL: srv.URL, AllowPrivateAddress: true, AllowInsecureBaseURL: true, KeyEnv: bindKey(t, srv.URL)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	job := &core.TuningJob{
		BaseModel: &core.AgentRef{Ref: "together:meta-llama/Llama-3-8b", Target: "meta-llama/Llama-3-8b"},
		Suffix:    "kno-run-1-all-in",
	}
	ref, err := tuner.Submit(context.Background(), job)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if ref.GetId() != "ft-abc123" {
		t.Errorf("job id = %q, want ft-abc123", ref.GetId())
	}
	if sawAuth != "Bearer sk-test" {
		t.Errorf("Authorization header = %q, want Bearer sk-test", sawAuth)
	}

	state, err := tuner.Status(context.Background(), ref)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if state.GetProgress() != 0.5 {
		t.Errorf("progress = %v, want 0.5", state.GetProgress())
	}
}

// TestFromStatusClassifiesUnauthorizedAsAuthFailure pins that a 401 maps
// onto ErrAuthentication so the run stops with a message naming the fix,
// rather than reading as a generic provider error.
func TestFromStatusClassifiesUnauthorizedAsAuthFailure(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "sk-test")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"message": "invalid api key", "type": "authentication_error"},
		})
	}))
	defer srv.Close()

	tuner, err := together.New(together.Options{BaseURL: srv.URL, AllowPrivateAddress: true, AllowInsecureBaseURL: true, KeyEnv: bindKey(t, srv.URL)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = tuner.Submit(context.Background(), &core.TuningJob{BaseModel: &core.AgentRef{Target: "m"}})
	if !errors.Is(err, together.ErrAuthentication) {
		t.Errorf("err = %v, want together.ErrAuthentication", err)
	}
}

// TestFromStatusClassifiesRateLimitAsRetryable pins a 429's mapping onto
// errs.ErrRateLimited.
func TestFromStatusClassifiesRateLimitAsRetryable(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "sk-test")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	tuner, err := together.New(together.Options{BaseURL: srv.URL, AllowPrivateAddress: true, AllowInsecureBaseURL: true, KeyEnv: bindKey(t, srv.URL)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = tuner.Submit(context.Background(), &core.TuningJob{BaseModel: &core.AgentRef{Target: "m"}})
	if !errors.Is(err, errs.ErrRateLimited) {
		t.Errorf("err = %v, want errs.ErrRateLimited", err)
	}
}

// TestNoHeadersInAnyError guards the fixture discipline: no response body
// or error text this adapter produces contains the Authorization header's
// value, which would leak the credential into a log line or a captured
// error.
func TestNoHeadersInAnyError(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "sk-super-secret-value")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"message": "internal error", "type": "server_error"},
		})
	}))
	defer srv.Close()

	tuner, err := together.New(together.Options{BaseURL: srv.URL, AllowPrivateAddress: true, AllowInsecureBaseURL: true, KeyEnv: bindKey(t, srv.URL)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = tuner.Submit(context.Background(), &core.TuningJob{BaseModel: &core.AgentRef{Target: "m"}})
	if err == nil {
		t.Fatal("want an error from a 500")
	}
	if got := err.Error(); containsSecret(got) {
		t.Errorf("error text leaked the credential: %q", got)
	}
}

func containsSecret(s string) bool {
	const secret = "sk-super-secret-value"
	for i := 0; i+len(secret) <= len(s); i++ {
		if s[i:i+len(secret)] == secret {
			return true
		}
	}
	return false
}
