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

// The inline httptest submit/status round trip that used to live here was
// converted to an on-disk fixture SEQUENCE — see poll_fixtures_test.go's
// TestPollSequenceReplaysFromFixturesToSuccess and
// TestPollSequenceFailedFixtureSurfacesProviderErrorVerbatim — per the
// tuner-bridge plan's Step 6(3) and acceptance criterion 23.

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

// TestListJobsMatchesOnlyBySuffix pins the adopt-by-suffix mechanism's
// filtering: a list carrying jobs with several suffixes returns only the
// one matching exactly, most-recently-submitted first when more than one
// matches.
func TestListJobsMatchesOnlyBySuffix(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "sk-test")

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/fine-tunes", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "ft-old", "status": "succeeded", "suffix": "kno-run-1-all-in", "created_at": "2026-08-30T00:00:00Z"},
				{"id": "ft-new", "status": "running", "suffix": "kno-run-1-all-in", "created_at": "2026-08-31T00:00:00Z"},
				{"id": "ft-other", "status": "running", "suffix": "kno-run-1-cluster-x", "created_at": "2026-08-31T01:00:00Z"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tuner, err := together.New(together.Options{BaseURL: srv.URL, AllowPrivateAddress: true, AllowInsecureBaseURL: true, KeyEnv: bindKey(t, srv.URL)})
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
	if refs[0].GetId() != "ft-new" {
		t.Errorf("refs[0].Id = %q, want %q (most recent first)", refs[0].GetId(), "ft-new")
	}
}

// TestListJobsRequiresASuffix guards against a caller accidentally matching
// every job on the account.
func TestListJobsRequiresASuffix(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "sk-test")

	tuner, err := together.New(together.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := tuner.ListJobs(context.Background(), ""); err == nil {
		t.Fatal("want an error for an empty suffix")
	}
}

// TestListEndpointsMatchesOnlyBySuffix pins the resume-time sweep's
// filtering: a list carrying endpoints for several served models returns
// only the one whose model name carries the run's suffix.
func TestListEndpointsMatchesOnlyBySuffix(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "sk-test")

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/endpoints", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "ep-mine", "state": "started", "model": "meta-llama/Llama-3-8b-kno-run-1-all-in"},
				{"id": "ep-other", "state": "started", "model": "meta-llama/Llama-3-8b-kno-run-2-all-in"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tuner, err := together.New(together.Options{BaseURL: srv.URL, AllowPrivateAddress: true, AllowInsecureBaseURL: true, KeyEnv: bindKey(t, srv.URL)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	eps, err := tuner.ListEndpoints(context.Background(), "kno-run-1-all-in")
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if len(eps) != 1 || eps[0].ID != "ep-mine" {
		t.Fatalf("got %+v, want exactly the matching endpoint", eps)
	}
	if !eps[0].Ready {
		t.Errorf("Ready = false for state %q, want true", "started")
	}
}

// TestListEndpointsRequiresASuffix mirrors TestListJobsRequiresASuffix for
// the endpoint sweep's own filter.
func TestListEndpointsRequiresASuffix(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "sk-test")

	tuner, err := together.New(together.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := tuner.ListEndpoints(context.Background(), ""); err == nil {
		t.Fatal("want an error for an empty suffix")
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
