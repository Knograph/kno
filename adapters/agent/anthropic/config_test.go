package anthropic_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/agent/anthropic"
	"github.com/knograph/kno/core/errs"
)

// TestNewRefusesTemperatureOnAModelThatRejectsIt.
//
// Several current models return a 400 for any sampling parameter. Sent blindly,
// that is not one error — it is EVERY Case erroring, tripping the error-rate
// threshold, and the user being told "too many cases errored for this to be a
// usable baseline", which names nothing about the cause.
//
// Refused rather than silently dropped: omitting it would run the measurement
// at the provider's default sampling while the Run records a temperature, so
// the report would claim a determinism the run did not have.
func TestNewRefusesTemperatureOnAModelThatRejectsIt(t *testing.T) {
	t.Parallel()

	zero := 0.0
	_, err := anthropic.New(anthropic.Options{
		Model:           "claude-opus-5-20260514",
		MaxOutputTokens: 1024,
		BaseURL:         "https://example.invalid",
		Temperature:     &zero,
	})
	if err == nil {
		t.Fatal("temperature was accepted for a model that rejects it with a 400")
	}
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Errorf("err = %v, want ErrInvalidInput", err)
	}
	if !strings.Contains(err.Error(), "temperature") {
		t.Errorf("the error does not name the setting to remove: %v", err)
	}
}

// TestCapabilitiesFollowTheModelRatherThanTheAdapter.
//
// generation_params is not a property of "the Anthropic adapter". It is a
// property of the model, and answering it per adapter is what would make the
// blanket temperature default look safe.
func TestCapabilitiesFollowTheModelRatherThanTheAdapter(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{
		"claude-sonnet-4-6":      true,
		"claude-sonnet-4-5":      true,
		"claude-haiku-4-5":       true,
		"claude-opus-5":          false,
		"claude-opus-5-20260514": false,
		"claude-opus-4-7":        false,
		"claude-sonnet-5":        false,
		"claude-fable-5":         false,
	}
	for model, want := range tests {
		t.Run(model, func(t *testing.T) {
			t.Parallel()

			a, err := anthropic.New(anthropic.Options{
				Model:           model,
				MaxOutputTokens: 1024,
				BaseURL:         "https://example.invalid",
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if got := a.Capabilities().GetGenerationParams(); got != want {
				t.Errorf("generation_params = %v, want %v", got, want)
			}
		})
	}
}

// TestCapabilitiesDeclareOnlyWhatThisAdapterDoes.
//
// Claiming an injection mode it does not have would let a valuation run report
// a measurement mode it never used.
func TestCapabilitiesDeclareOnlyWhatThisAdapterDoes(t *testing.T) {
	t.Parallel()

	a, err := anthropic.New(anthropic.Options{
		Model:           testModel,
		MaxOutputTokens: 1024,
		BaseURL:         "https://example.invalid",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := a.Capabilities()
	if c.GetContextInject() || c.GetKnowledgeWrite() {
		t.Error("an injection capability is declared that M2 does not implement")
	}
	if c.GetStream() {
		t.Error("streaming is declared and is not implemented")
	}
	if !c.GetTokenCounts() {
		t.Error("token_counts is false; the Messages API reports usage, and a Response " +
			"that did not carries usage_estimated instead")
	}
}

// TestNewRefusesConfigurationItCannotRunWith.
//
// Refused HERE, where the message is a startup error the user can read, rather
// than per Case, where the same mistake becomes a run of failures.
func TestNewRefusesConfigurationItCannotRunWith(t *testing.T) {
	t.Parallel()

	tests := map[string]anthropic.Options{
		"no model": {
			MaxOutputTokens: 1024, BaseURL: "https://example.invalid",
		},
		"no output ceiling": {
			Model: testModel, BaseURL: "https://example.invalid",
		},
		"a base URL with no host": {
			Model: testModel, MaxOutputTokens: 1024, BaseURL: "not-a-url",
		},
		"a base URL carrying a credential": {
			Model: testModel, MaxOutputTokens: 1024,
			BaseURL: "https://user:secret@example.invalid/v1",
		},
		"plain HTTP without opting in": {
			Model: testModel, MaxOutputTokens: 1024, BaseURL: "http://example.invalid",
		},
		"a link-local base URL": {
			Model: testModel, MaxOutputTokens: 1024,
			BaseURL: "https://169.254.169.254", AllowPrivateAddress: true,
		},
		"a private base URL without opting in": {
			Model: testModel, MaxOutputTokens: 1024, BaseURL: "https://10.0.0.1",
		},
		"a key rather than a variable NAME": {
			Model: testModel, MaxOutputTokens: 1024, BaseURL: "https://example.invalid",
			KeyEnv: map[string]string{"example.invalid": "not-a-var-name"},
		},
	}
	for name, opts := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := anthropic.New(opts); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

// TestABaseURLWithUserinfoIsNotEchoedBack.
//
// A malformed or credential-bearing URL is the one error path that can carry a
// secret, and quoting the input back is how it reaches a log line, the Run, and
// --json.
func TestABaseURLWithUserinfoIsNotEchoedBack(t *testing.T) {
	t.Parallel()

	_, err := anthropic.New(anthropic.Options{
		Model:           testModel,
		MaxOutputTokens: 1024,
		BaseURL:         "https://user:hunter2@example.invalid/v1",
	})
	if err == nil {
		t.Fatal("a base URL carrying a credential was accepted")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the credential was quoted back into the error: %v", err)
	}
}

// TestAnthropicsOwnHostWithNoCredentialIsRefusedAtStartup.
//
// Not parallel: it reads process environment. Without this, every Case 401s and
// the run reports "too many cases errored" rather than "export
// ANTHROPIC_API_KEY".
func TestAnthropicsOwnHostWithNoCredentialIsRefusedAtStartup(t *testing.T) {
	t.Setenv(anthropic.DefaultKeyEnv, "")

	_, err := anthropic.New(anthropic.Options{
		Model:           testModel,
		MaxOutputTokens: 1024,
	})
	if !errors.Is(err, anthropic.ErrAuthentication) {
		t.Fatalf("err = %v, want ErrAuthentication", err)
	}
	if !strings.Contains(err.Error(), anthropic.DefaultKeyEnv) {
		t.Errorf("the error does not name %s: %v", anthropic.DefaultKeyEnv, err)
	}
}

// TestTheCredentialTravelsAsXAPIKeyToTheHostItIsBoundTo.
//
// x-api-key rather than Authorization, which is the header Go's net/http would
// have stripped on a cross-domain redirect. Not parallel: it reads process
// environment.
func TestTheCredentialTravelsAsXAPIKeyToTheHostItIsBoundTo(t *testing.T) {
	const envVar = "KNO_TEST_ANTHROPIC_CREDENTIAL"
	const value = "test-credential-value"
	t.Setenv(envVar, value)

	srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) { answer(w, okBody) })
	host := strings.TrimPrefix(srv.URL, "http://")

	a := newAgent(t, srv, func(o *anthropic.Options) {
		o.KeyEnv = map[string]string{host: envVar}
	})
	if _, err := a.Invoke(t.Context(), aCase("q")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	h := rec.header(t, 0)
	if got := h.Get(anthropic.KeyHeader); got != value {
		t.Errorf("%s = %q, want the bound credential", anthropic.KeyHeader, got)
	}
	if h.Get("Authorization") != "" {
		t.Error("an Authorization header was sent; this API authenticates with x-api-key, " +
			"and sending both is itself a documented 401")
	}
}

// TestAKeyBoundToAnotherHostDoesNotTravel.
//
// The whole point of explicit binding: a key bound to host A never reaches host
// B, flag or no flag. Not parallel: it reads process environment.
func TestAKeyBoundToAnotherHostDoesNotTravel(t *testing.T) {
	const envVar = "KNO_TEST_OTHER_HOST_CREDENTIAL"
	t.Setenv(envVar, "must-not-travel")

	srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) { answer(w, okBody) })

	a := newAgent(t, srv, func(o *anthropic.Options) {
		o.KeyEnv = map[string]string{"api.somewhere-else.example": envVar}
	})
	if _, err := a.Invoke(t.Context(), aCase("q")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := rec.header(t, 0).Get(anthropic.KeyHeader); got != "" {
		t.Errorf("%s = %q; a credential bound to another host reached this one",
			anthropic.KeyHeader, got)
	}
}

// TestTheAnthropicKeyDoesNotFollowABaseURLElsewhere.
//
// ANTHROPIC_API_KEY applies to Anthropic's host and to no other. Falling back
// to it for a user-supplied base URL is a documented recipe that mails the
// user's key to a third party. Not parallel: it reads process environment.
func TestTheAnthropicKeyDoesNotFollowABaseURLElsewhere(t *testing.T) {
	t.Setenv(anthropic.DefaultKeyEnv, "anthropics-own-credential")

	srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) { answer(w, okBody) })
	a := newAgent(t, srv)

	if _, err := a.Invoke(t.Context(), aCase("q")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := rec.header(t, 0).Get(anthropic.KeyHeader); got != "" {
		t.Errorf("%s = %q; ANTHROPIC_API_KEY followed --base-url to a host it was "+
			"never bound to", anthropic.KeyHeader, got)
	}
}

// TestModelAndBaseURLAreReportedForTheRecord.
func TestModelAndBaseURLAreReportedForTheRecord(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) { answer(w, okBody) })
	a := newAgent(t, srv)

	if a.Model() != testModel {
		t.Errorf("Model = %q, want %q", a.Model(), testModel)
	}
	if a.BaseURL() != srv.URL {
		t.Errorf("BaseURL = %q, want %q", a.BaseURL(), srv.URL)
	}
}
