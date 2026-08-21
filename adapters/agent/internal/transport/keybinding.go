package transport

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// KeyBindings maps a host to the NAME of the environment variable holding its
// credential.
//
// The name of a variable is not a secret, so binding it on the command line
// does not violate the rule that keys never appear in a flag — the key itself
// still only ever comes from the environment.
//
// Explicit, never derived. An earlier design computed the variable name from
// the host (KNO_API_KEY_<NORMALIZED_HOST>), which is not injective: env var
// names permit only [A-Za-z0-9_], so api.groq.com, api-groq-com, and
// api_groq_com all collapse to API_GROQ_COM. An attacker registering the
// typosquat api-groq.com would receive the key bound to api.groq.com with no
// flag involved — a mechanism introduced to stop a key reaching the wrong host,
// delivering exactly that. Ports, case, trailing dots, and punycode were all
// unspecified on top of it, and the user could not guess the name anyway.
type KeyBindings map[string]string

// ParseKeyBindings reads `host=ENV_VAR` pairs.
//
// Two bindings that resolve to the same host are refused rather than silently
// ordered: whichever won would depend on argument order, and "which key went to
// which host" is not a question anyone should answer by reading argv.
func ParseKeyBindings(pairs []string) (KeyBindings, error) {
	out := make(KeyBindings, len(pairs))
	for _, p := range pairs {
		host, envVar, ok := strings.Cut(p, "=")
		if !ok {
			return nil, fmt.Errorf("%w: %q is not host=ENV_VAR", ErrKeyBinding, p)
		}
		host, envVar = strings.TrimSpace(host), strings.TrimSpace(envVar)
		if host == "" || envVar == "" {
			return nil, fmt.Errorf("%w: %q is not host=ENV_VAR", ErrKeyBinding, p)
		}
		// The VALUE is the secret; the NAME is not. Refusing a name that looks
		// like a key catches the likeliest confusion — someone passing the key
		// itself — before it reaches shell history and a process listing.
		if looksLikeASecret(envVar) {
			return nil, fmt.Errorf("%w: %s=… looks like a key rather than the NAME "+
				"of an environment variable holding one. Pass the variable's name; "+
				"the key itself must not appear in a flag", ErrKeyBinding, host)
		}
		k := normalizeHost(host)
		if prev, dup := out[k]; dup {
			return nil, fmt.Errorf("%w: %s is bound twice, to %s and %s; which one "+
				"applies would depend on argument order",
				ErrKeyBinding, host, prev, envVar)
		}
		out[k] = envVar
	}
	return out, nil
}

// normalizeHost canonicalizes for comparison only — never for constructing a
// variable name, which is the mistake that made the derived scheme unsafe.
func normalizeHost(h string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
}

// looksLikeASecret reports whether s resembles a credential rather than a
// variable name. Deliberately loose: a false positive costs one clear error
// message, a false negative puts a key in `ps` output.
func looksLikeASecret(s string) bool {
	for _, prefix := range []string{"sk-", "sk_", "pk-", "xoxb-", "ghp_", "Bearer "} {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	// Env var names are conventionally short and [A-Z0-9_]; a long string with
	// lowercase or punctuation is not one.
	if len(s) > 40 {
		return true
	}
	return strings.ContainsAny(s, " \t/+=")
}

// Resolve returns the credential for host.
//
// A scheme's default variable applies ONLY to that scheme's default host. That
// is the whole point: `openai:model@https://api.groq.com/v1` needs GROQ_API_KEY,
// and falling back to OPENAI_API_KEY would mail the user's OpenAI key to a third
// party by following the documented recipe.
//
// A host with no binding gets no credential — not an error, because a local
// model server legitimately needs none. Whether the absence is fatal is the
// adapter's call, made when the provider rejects the unauthenticated request.
func (b KeyBindings) Resolve(host, defaultHost, defaultEnvVar string) (key string, envVar string) {
	h := normalizeHost(hostOnly(host))

	if v, ok := b[h]; ok {
		return os.Getenv(v), v
	}
	if defaultEnvVar != "" && h == normalizeHost(hostOnly(defaultHost)) {
		return os.Getenv(defaultEnvVar), defaultEnvVar
	}
	return "", ""
}

// hostOnly strips a port, so a binding for "localhost" covers "localhost:8000".
// Ports do not change who is on the other end.
func hostOnly(h string) string {
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[i:], "]") {
		return h[:i]
	}
	return h
}

// Hosts lists the bound hosts, sorted, for error messages that tell a user what
// IS configured rather than only what is not.
func (b KeyBindings) Hosts() []string {
	out := make([]string, 0, len(b))
	for h := range b {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}
