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
		if strings.Count(host, ":") > 1 && !strings.Contains(host, "]") {
			return nil, fmt.Errorf("%w: %q looks like a bare IPv6 address; write it "+
				"in brackets ([::1] or [::1]:8000) so the port is unambiguous",
				ErrKeyBinding, host)
		}
		if looksLikeASecret(envVar) {
			return nil, fmt.Errorf("%w: %s=… looks like a key rather than the NAME "+
				"of an environment variable holding one. Pass the variable's name; "+
				"the key itself must not appear in a flag", ErrKeyBinding, host)
		}
		// Normalized exactly as Resolve will normalize its lookup. They used to
		// differ by hostOnly: a binding written with a port was stored with it
		// and looked up without, so it silently never resolved — and the
		// "bound twice" refusal, whose whole point is that argv order must not
		// decide which key goes where, was bypassed by adding ":443" to one of
		// them.
		k := bindingKey(host)
		if prev, dup := out[k]; dup {
			return nil, fmt.Errorf("%w: %s is bound twice, to %s and %s; which one "+
				"applies would depend on argument order",
				ErrKeyBinding, host, prev, envVar)
		}
		out[k] = envVar
	}
	return out, nil
}

// bindingKey is the single normalization both sides use.
func bindingKey(h string) string { return normalizeHost(hostOnly(h)) }

// normalizeHost canonicalizes for comparison only — never for constructing a
// variable name, which is the mistake that made the derived scheme unsafe.
func normalizeHost(h string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
}

// looksLikeASecret reports whether s resembles a credential rather than a
// variable name.
//
// Stated as "this is not a variable name" rather than "this looks like a key".
// The length-based version tested the wrong thing in both directions: its
// 40-character threshold sat just above the most common real key length, so
// Groq's gsk_… (39), Gemini's AIza… (39), HuggingFace's hf_… (39), a 32-char
// Azure key and a 20-char AWS key all passed as "names" — while a legitimate
// KNO_OPENAI_COMPATIBLE_PRODUCTION_API_KEY_US (43) was rejected.
//
// An environment variable name is [A-Za-z_][A-Za-z0-9_]* by convention and, in
// practice, uppercase. Anything else is not one, at any length.
func looksLikeASecret(s string) bool {
	if s == "" {
		return true
	}
	if c := s[0]; !(c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')) {
		return true // a name does not start with a digit or punctuation
	}
	for _, r := range s {
		isNameChar := r == '_' ||
			(r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !isNameChar {
			return true // hyphens, slashes, spaces, '+', '=' are key alphabet
		}
		// Real keys are lowercase-heavy (sk-, gsk_, hf_, r8_); env var names
		// are conventionally not. This is the test that catches a key whose
		// alphabet is otherwise name-shaped.
		if r >= 'a' && r <= 'z' {
			return true
		}
	}
	return false
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
	h := bindingKey(host)

	if v, ok := b[h]; ok {
		return os.Getenv(v), v
	}
	if defaultEnvVar != "" && h == bindingKey(defaultHost) {
		return os.Getenv(defaultEnvVar), defaultEnvVar
	}
	return "", ""
}

// hostOnly strips a port, so a binding for "localhost" covers "localhost:8000".
// Ports do not change who is on the other end.
//
// A bracketed IPv6 host keeps its brackets ("[::1]:8000" -> "[::1]"). A BARE
// IPv6 address is not handled and cannot be: "::1" is indistinguishable from a
// host with a port, and guessing would mangle it into ":". ParseKeyBindings
// refuses that spelling and says to bracket it.
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
