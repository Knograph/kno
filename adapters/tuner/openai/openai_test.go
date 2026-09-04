package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/knograph/kno/adapters/tuner/openai"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
)

// bindKey returns the --key-env-shaped binding a caller needs when pointing
// --base-url at anything other than OpenAI's own default host — the same
// requirement together's tests bind for a test server's host, and for the
// same reason: DefaultKeyEnv applies ONLY to DefaultBaseURL's host.
func bindKey(t *testing.T, rawURL string) map[string]string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	return map[string]string{u.Host: "OPENAI_API_KEY"}
}

// TestNewRefusesWithNoCredential pins that a missing OPENAI_API_KEY is
// refused at construction — before planning, before any job.
func TestNewRefusesWithNoCredential(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")

	_, err := openai.New(openai.Options{})
	if err == nil {
		t.Fatal("want a refusal with no credential set")
	}
	if !errors.Is(err, openai.ErrAuthentication) {
		t.Errorf("err = %v, want openai.ErrAuthentication", err)
	}
}

// TestNewRefusesPlainHTTP pins the plain-HTTP refusal, matching every other
// adapter's posture — enforced here through adapters/internal/endpointsec.
func TestNewRefusesPlainHTTP(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")

	_, err := openai.New(openai.Options{BaseURL: "http://api.openai.com"})
	if err == nil {
		t.Fatal("want a refusal for a plain-HTTP base URL")
	}
}

// TestNewRefusesPrivateAddressByDefault pins that a base URL resolving to a
// loopback or private address is refused unless AllowPrivateAddress is set.
func TestNewRefusesPrivateAddressByDefault(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	_, err := openai.New(openai.Options{BaseURL: srv.URL, AllowInsecureBaseURL: true})
	if err == nil {
		t.Fatal("want a refusal: httptest.Server binds to 127.0.0.1, a private address")
	}

	// The opt-out makes it constructible.
	if _, err := openai.New(openai.Options{
		BaseURL: srv.URL, AllowPrivateAddress: true, AllowInsecureBaseURL: true, KeyEnv: bindKey(t, srv.URL),
	}); err != nil {
		t.Errorf("New with AllowPrivateAddress: %v", err)
	}
}

// TestFromStatusClassifiesUnauthorizedAsAuthFailure pins that a 401 maps
// onto ErrAuthentication so the run stops with a message naming the fix.
func TestFromStatusClassifiesUnauthorizedAsAuthFailure(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"message": "invalid api key", "type": "authentication_error"},
		})
	}))
	defer srv.Close()

	tuner, err := openai.New(openai.Options{BaseURL: srv.URL, AllowPrivateAddress: true, AllowInsecureBaseURL: true, KeyEnv: bindKey(t, srv.URL)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = tuner.Submit(context.Background(), &core.TuningJob{BaseModel: &core.AgentRef{Target: "gpt-5.6-terra"}})
	if !errors.Is(err, openai.ErrAuthentication) {
		t.Errorf("err = %v, want openai.ErrAuthentication", err)
	}
}

// TestFromStatusClassifiesRateLimitAsRetryable pins a 429's mapping onto
// errs.ErrRateLimited.
func TestFromStatusClassifiesRateLimitAsRetryable(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	tuner, err := openai.New(openai.Options{BaseURL: srv.URL, AllowPrivateAddress: true, AllowInsecureBaseURL: true, KeyEnv: bindKey(t, srv.URL)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = tuner.Submit(context.Background(), &core.TuningJob{BaseModel: &core.AgentRef{Target: "gpt-5.6-terra"}})
	if !errors.Is(err, errs.ErrRateLimited) {
		t.Errorf("err = %v, want errs.ErrRateLimited", err)
	}
}

// TestNoHeadersInAnyError guards the fixture discipline: no response body or
// error text this adapter produces contains the Authorization header's
// value, which would leak the credential into a log line or a captured
// error.
func TestNoHeadersInAnyError(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-super-secret-value")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"message": "internal error", "type": "server_error"},
		})
	}))
	defer srv.Close()

	tuner, err := openai.New(openai.Options{BaseURL: srv.URL, AllowPrivateAddress: true, AllowInsecureBaseURL: true, KeyEnv: bindKey(t, srv.URL)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = tuner.Submit(context.Background(), &core.TuningJob{BaseModel: &core.AgentRef{Target: "gpt-5.6-terra"}})
	if err == nil {
		t.Fatal("want an error from a 500")
	}
	if got := err.Error(); containsSecret(got) {
		t.Errorf("error text leaked the credential: %q", got)
	}
}

// TestListJobsMatchesOnlyByMetadataSuffix pins the adopt-by-suffix
// mechanism's filtering — via the namespaced metadata key, NOT a "suffix"
// field on the wire (docs/plans/2026-09-02-openai-tuner.md §6): a list
// carrying jobs tagged under several suffix values returns only the one
// matching exactly, most-recently-submitted first when more than one
// matches, and sends the metadata filter as a query parameter.
func TestListJobsMatchesOnlyByMetadataSuffix(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/fine_tuning/jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if got := r.URL.Query().Get("metadata[kno_suffix]"); got != "kno-run-1-all-in" {
			t.Errorf("metadata[kno_suffix] query param = %q, want %q", got, "kno-run-1-all-in")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{
					"id": "ftjob-old", "status": "succeeded", "created_at": 1_800_000_000,
					"metadata": map[string]string{"kno_suffix": "kno-run-1-all-in"},
				},
				{
					"id": "ftjob-new", "status": "running", "created_at": 1_800_000_100,
					"metadata": map[string]string{"kno_suffix": "kno-run-1-all-in"},
				},
				{
					"id": "ftjob-other", "status": "running", "created_at": 1_800_000_050,
					"metadata": map[string]string{"kno_suffix": "kno-run-1-cluster-x"},
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tuner, err := openai.New(openai.Options{BaseURL: srv.URL, AllowPrivateAddress: true, AllowInsecureBaseURL: true, KeyEnv: bindKey(t, srv.URL)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	refs, err := tuner.ListJobs(context.Background(), "kno-run-1-all-in")
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("got %d refs, want 2 (only the matching suffix)", len(refs))
	}
	if refs[0].GetId() != "ftjob-new" {
		t.Errorf("refs[0].Id = %q, want %q (most recent first)", refs[0].GetId(), "ftjob-new")
	}
}

// TestListJobsRequiresASuffix guards against a caller accidentally matching
// every job on the account.
func TestListJobsRequiresASuffix(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")

	tuner, err := openai.New(openai.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := tuner.ListJobs(context.Background(), ""); err == nil {
		t.Fatal("want an error for an empty suffix")
	}
}

// TestListEndpointsAlwaysEmpty pins §3's deliberate contract: OpenAI creates
// no dedicated-endpoint resource, so ListEndpoints returns empty even when
// the fixture server would answer with something if asked — this adapter
// must never call it.
func TestListEndpointsAlwaysEmpty(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")

	called := false
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(http.ResponseWriter, *http.Request) { called = true })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tuner, err := openai.New(openai.Options{BaseURL: srv.URL, AllowPrivateAddress: true, AllowInsecureBaseURL: true, KeyEnv: bindKey(t, srv.URL)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	eps, err := tuner.ListEndpoints(context.Background(), "kno-run-1-all-in")
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if len(eps) != 0 {
		t.Errorf("got %d endpoints, want 0 — OpenAI never has a dedicated endpoint to list", len(eps))
	}
	if called {
		t.Error("ListEndpoints made an HTTP call; it must always return empty without one")
	}
}

// TestListEndpointsRequiresASuffix mirrors TestListJobsRequiresASuffix for
// interface consistency with together.Tuner, even though the data is always
// empty.
func TestListEndpointsRequiresASuffix(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")

	tuner, err := openai.New(openai.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := tuner.ListEndpoints(context.Background(), ""); err == nil {
		t.Fatal("want an error for an empty suffix")
	}
}

// TestTeardownNeverFails pins §3's "cannot fail" contract: Teardown makes no
// HTTP call at all, so even a server that would 500 every request cannot
// make it return an error.
func TestTeardownNeverFails(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tuner, err := openai.New(openai.Options{BaseURL: srv.URL, AllowPrivateAddress: true, AllowInsecureBaseURL: true, KeyEnv: bindKey(t, srv.URL)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := tuner.Teardown(context.Background(), &core.Endpoint{ID: "gpt-5.6-terra:ft-abc"}); err != nil {
		t.Errorf("Teardown returned an error: %v", err)
	}
	// Teardown must also be safe against a nil Endpoint — a defensive
	// no-op, unlike together.Tuner.Teardown which refuses one.
	if err := tuner.Teardown(context.Background(), nil); err != nil {
		t.Errorf("Teardown(nil) returned an error: %v", err)
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
