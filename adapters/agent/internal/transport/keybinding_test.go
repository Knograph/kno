package transport_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/agent/internal/transport"
)

// TestKeyBindingsAreExplicitNotDerived.
//
// An earlier design computed the env var name from the host
// (KNO_API_KEY_<NORMALIZED_HOST>). That is not injective: env var names permit
// only [A-Za-z0-9_], so api.groq.com, api-groq-com and api_groq_com all collapse
// to API_GROQ_COM — and an attacker registering the typosquat api-groq.com would
// receive the key bound to api.groq.com with no flag involved.
//
// This asserts the property that replaced it: hosts that a derived scheme would
// have collided are kept distinct, and each resolves only to its own binding.
func TestKeyBindingsAreExplicitNotDerived(t *testing.T) {
	// Not parallel: t.Setenv mutates process state, and the two are mutually
	// exclusive for that reason.
	t.Setenv("REAL_KEY", "real")
	t.Setenv("TYPO_KEY", "typo")

	b, err := transport.ParseKeyBindings([]string{
		"api.groq.com=REAL_KEY",
		"api-groq.com=TYPO_KEY",
	})
	if err != nil {
		t.Fatalf("ParseKeyBindings: %v", err)
	}

	if got, _ := b.Resolve("api.groq.com", "", ""); got != "real" {
		t.Errorf("api.groq.com resolved to %q, want the key bound to it", got)
	}
	if got, _ := b.Resolve("api-groq.com", "", ""); got != "typo" {
		t.Errorf("api-groq.com resolved to %q; a derived scheme would have "+
			"collapsed these two hosts to one variable", got)
	}
}

func TestKeyBindingsRefuseAmbiguityAndSecrets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		pairs []string
		frag  string
	}{
		{
			name:  "the same host bound twice",
			pairs: []string{"api.example.com=A_KEY", "API.example.com.=B_KEY"},
			// Which one applied would depend on argument order.
			frag: "bound twice",
		},
		{
			name:  "a key passed where a variable name belongs",
			pairs: []string{"api.example.com=sk-EXAMPLE-NOT-A-REAL-KEY"},
			frag:  "looks like a key",
		},
		{
			name:  "a long opaque value",
			pairs: []string{"api.example.com=" + strings.Repeat("x", 60)},
			frag:  "looks like a key",
		},
		{name: "no separator", pairs: []string{"api.example.com"}, frag: "host=ENV_VAR"},
		{name: "an empty variable name", pairs: []string{"api.example.com="}, frag: "host=ENV_VAR"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := transport.ParseKeyBindings(tc.pairs)
			if err == nil {
				t.Fatalf("accepted %v", tc.pairs)
			}
			if !errors.Is(err, transport.ErrKeyBinding) {
				t.Errorf("err = %v, want ErrKeyBinding", err)
			}
			if !strings.Contains(err.Error(), tc.frag) {
				t.Errorf("the refusal does not say why (%q):\n%s", tc.frag, err)
			}
		})
	}
}

// TestASchemeDefaultAppliesOnlyToItsOwnHost.
//
// `openai:model@https://api.groq.com/v1` needs GROQ_API_KEY. Falling back to
// OPENAI_API_KEY would mail the user's OpenAI key to a third party by following
// the documented recipe — threat #1 of the security design, reached through the
// docs.
func TestASchemeDefaultAppliesOnlyToItsOwnHost(t *testing.T) {
	// Not parallel: t.Setenv mutates process state, and the two are mutually
	// exclusive for that reason.
	t.Setenv("OPENAI_API_KEY", "openai-secret")

	b, err := transport.ParseKeyBindings(nil)
	if err != nil {
		t.Fatalf("ParseKeyBindings: %v", err)
	}

	if got, name := b.Resolve("api.openai.com", "api.openai.com", "OPENAI_API_KEY"); got != "openai-secret" || name != "OPENAI_API_KEY" {
		t.Errorf("the default host resolved to (%q, %q), want the scheme default", got, name)
	}
	if got, _ := b.Resolve("api.groq.com", "api.openai.com", "OPENAI_API_KEY"); got != "" {
		t.Errorf("a third-party host resolved to %q; the scheme default must not "+
			"travel off its own host", got)
	}
}

// TestAPortDoesNotChangeWhoIsOnTheOtherEnd: a binding for localhost covers
// localhost:8000, because the port is not the identity.
func TestAPortDoesNotChangeWhoIsOnTheOtherEnd(t *testing.T) {
	// Not parallel: t.Setenv mutates process state, and the two are mutually
	// exclusive for that reason.
	t.Setenv("LOCAL_KEY", "local")
	b, err := transport.ParseKeyBindings([]string{"localhost=LOCAL_KEY"})
	if err != nil {
		t.Fatalf("ParseKeyBindings: %v", err)
	}
	if got, _ := b.Resolve("localhost:8000", "", ""); got != "local" {
		t.Errorf("localhost:8000 resolved to %q, want the localhost binding", got)
	}
}
