// Package agentref parses the string that names an agent.
//
// One grammar, used identically in flags, kno.yaml, the API, and the SDKs:
//
//	scheme:target[@base-url]
//
// The parser is separate from resolution on purpose. Parsing answers "is this
// well formed, and what are its parts"; resolving answers "is there an adapter
// for it". Conflating them means an unknown scheme and a malformed reference
// produce the same error, and the user cannot tell a typo from an unsupported
// provider.
package agentref

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// Schemes this grammar defines. Whether an adapter exists for one is a
// separate question, answered at resolution.
const (
	// SchemeOpenAI is the OpenAI Chat Completions shape. With a base URL it
	// reaches every OpenAI-compatible provider, which is why there is no
	// second "openai-compat" scheme: two user-visible names for one adapter is
	// vocabulary drift.
	SchemeOpenAI = "openai"

	// SchemeAnthropic is the Messages API.
	SchemeAnthropic = "anthropic"

	// SchemeFake is the local deterministic agent. No network, no cost.
	SchemeFake = "fake"

	// SchemeExec is an agent behind a shell command.
	SchemeExec = "exec"

	// SchemeTuned names a fine-tuning job's output.
	SchemeTuned = "tuned"
)

// knownSchemes is the closed set the grammar accepts.
//
// Closed rather than open: an unrecognized scheme is far more often a typo
// than a provider Kno has never heard of, and "unknown scheme %q; known are
// ..." is a better answer than carrying it forward to fail later as a missing
// adapter.
var knownSchemes = map[string]bool{
	SchemeOpenAI:    true,
	SchemeAnthropic: true,
	SchemeFake:      true,
	SchemeExec:      true,
	SchemeTuned:     true,
}

// schemeNeedsTarget reports whether a scheme is meaningless without one.
//
// Every scheme but fake names something specific — a model, a command, a job.
// `fake:` names the local deterministic agent, which has no model because it
// calls nothing; it is the CLI's default and the spelling used throughout the
// docs and the cookbook. Requiring a target here would refuse the one reference
// every reader of the README types first.
func schemeNeedsTarget(scheme string) bool { return scheme != SchemeFake }

// ErrMalformed means a reference does not fit the grammar.
var ErrMalformed = fmt.Errorf("agentref: malformed")

// Parse splits a reference into its parts.
//
// The returned AgentRef's Ref is the reference as written MINUS any credential:
// it is persisted on the Run, emitted on RunStarted, and rendered in --json and
// logs, so a reference carrying one is refused rather than stored.
func Parse(raw string) (*knov1.AgentRef, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, fmt.Errorf("%w: the agent reference is empty", ErrMalformed)
	}

	// A bare URL is the form the schema reserves but this build does not
	// accept: it names an endpoint with no model, and every adapter needs one.
	// Named explicitly so the error can say what to write instead.
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return nil, fmt.Errorf("%w: %q is a bare URL with no model. Write it as "+
			"scheme:model@base-url, for example openai:llama-3.3-70b@%s",
			ErrMalformed, redactRef(s), redactRef(s))
	}

	// Scheme ends at the FIRST colon. A model name may contain colons of its
	// own — Ollama spells them llama3:8b, OpenRouter spells them
	// vendor/model:free — so splitting anywhere else silently reassigns part
	// of the model to the scheme.
	scheme, rest, ok := strings.Cut(s, ":")
	if !ok {
		return nil, fmt.Errorf("%w: %q has no scheme. Write scheme:model, for "+
			"example openai:gpt-4.1", ErrMalformed, redactRef(s))
	}
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	if !knownSchemes[scheme] {
		return nil, fmt.Errorf("%w: unknown scheme %q. Known schemes are %s",
			ErrMalformed, scheme, strings.Join(knownSchemeNames(), ", "))
	}

	target, baseURL := splitBaseURL(rest)
	if strings.TrimSpace(target) == "" && schemeNeedsTarget(scheme) {
		return nil, fmt.Errorf("%w: %q names a scheme with no model",
			ErrMalformed, redactRef(s))
	}

	if baseURL != "" {
		if err := checkBaseURL(baseURL); err != nil {
			return nil, err
		}
	}

	return &knov1.AgentRef{
		Ref:     scheme + ":" + target + suffix(baseURL),
		Scheme:  scheme,
		Target:  target,
		BaseUrl: baseURL,
	}, nil
}

// splitBaseURL separates target from an @base-url suffix.
//
// The split is the FIRST `@` whose remainder is an absolute http(s) URL, not
// the first `@` and not the last. Both simpler rules misparse a real case:
//
//	openai:model@v2                        — an `@` with no URL after it
//	openai:m@https://user:pass@host/v1     — an `@` INSIDE the URL
//
// Splitting on the first `@` breaks the first; splitting on the last breaks the
// second, yielding a base URL of "host/v1" and a target of
// "m@https://user:pass". The second case is then refused for carrying a
// credential — but only if it was parsed correctly enough to notice.
func splitBaseURL(rest string) (target, baseURL string) {
	for i, r := range rest {
		if r != '@' {
			continue
		}
		candidate := rest[i+1:]
		if strings.HasPrefix(candidate, "http://") || strings.HasPrefix(candidate, "https://") {
			return rest[:i], candidate
		}
	}
	return rest, ""
}

func suffix(baseURL string) string {
	if baseURL == "" {
		return ""
	}
	return "@" + baseURL
}

// checkBaseURL rejects a base URL that cannot be stored safely.
//
// Only what the PARSER can decide: it is a URL, it has a host, and it carries
// no credential. Whether the destination is reachable — plain HTTP, private
// addresses, link-local — belongs to the transport, which applies it against
// the resolved address. Duplicating those rules here would give two places to
// change and one of them would drift.
func checkBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		// Neither the URL nor the parse error is echoed: a malformed URL can
		// still carry a credential, and url.Parse quotes its input.
		return fmt.Errorf("%w: the base URL could not be parsed", ErrMalformed)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: the base URL has no host", ErrMalformed)
	}
	// A base URL is an endpoint root, not a request. A fragment is never sent
	// to a server, and a query would be silently appended to every request the
	// adapter makes — so both are user error, and quietly dropping them would
	// hide it.
	//
	// Found by the fuzz target on its first run: "openai:0@http://0#@" parsed
	// cleanly with a base URL of "http://0#@", which carries an `@` that is not
	// userinfo. Rejecting the fragment is the honest fix; special-casing the
	// invariant to allow it would have been the convenient one.
	if u.Fragment != "" || u.RawQuery != "" {
		return fmt.Errorf("%w: the base URL carries a %s. It names an endpoint "+
			"root, and anything after it would be appended to every request",
			ErrMalformed, map[bool]string{true: "fragment", false: "query string"}[u.Fragment != ""])
	}
	if u.User != nil {
		// Refused, not stripped. The credential is already in the user's shell
		// history; quietly rewriting it would hide that from them.
		return fmt.Errorf("%w: the base URL carries a credential in its userinfo. "+
			"Remove it and bind the key to the host through the environment",
			ErrMalformed)
	}
	return nil
}

// redactRef removes a credential from a reference before it is quoted back.
//
// Parse refuses userinfo, but the refusals ABOVE that check quote the input
// too, and a reference can be malformed and credential-bearing at once.
func redactRef(s string) string {
	at := strings.Index(s, "@")
	if at < 0 {
		return s
	}
	// Any `user:pass@` inside the URL portion is replaced wholesale rather than
	// parsed, because the string may not parse at all — which is how it reached
	// an error path.
	if i := strings.Index(s[at+1:], "@"); i >= 0 {
		return s[:at+1] + "[redacted]@" + s[at+1+i+1:]
	}
	return s
}

func knownSchemeNames() []string {
	out := make([]string, 0, len(knownSchemes))
	for s := range knownSchemes {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
