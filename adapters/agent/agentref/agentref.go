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
	"net"
	"net/url"
	"regexp"
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

// whatSchemeNames says what a scheme's target IS, so a refusal uses the user's
// word rather than the parser's. "exec: names a scheme with no model" is wrong:
// exec names a command.
func whatSchemeNames(scheme string) string {
	switch scheme {
	case SchemeExec:
		return "command"
	case SchemeTuned:
		return "job"
	default:
		return "model"
	}
}

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
			"scheme:model@base-url, for example "+
			"openai:llama-3.3-70b@https://api.groq.com/openai/v1",
			ErrMalformed, redactRef(s))
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
	target = strings.TrimSpace(target)
	if target == "" && schemeNeedsTarget(scheme) {
		return nil, fmt.Errorf("%w: %q names a scheme with no %s",
			ErrMalformed, redactRef(s), whatSchemeNames(scheme))
	}

	// A model slot never legitimately contains a URL scheme. This does not
	// depend on splitBaseURL having got the split right, which is the point:
	// when the split MISSED — a scheme spelled HTTPS://, a fullwidth ＠, a typo
	// like https:/host — the whole URL landed in the target and was
	// reconstructed verbatim into Ref, which is persisted on the Run, emitted
	// on RunStarted, and rendered in --json. A credential there reached all
	// three and `kno purge` did not remove it, because purge clears outcome
	// traces and not the Run's own agent field. Verified: four copies in the
	// database, still there after a purge.
	if strings.Contains(target, ":/") {
		return nil, fmt.Errorf("%w: %q puts a URL where the model belongs. A base "+
			"URL is introduced by @ and must begin http:// or https://",
			ErrMalformed, redactRef(s))
	}
	if strings.Count(target, "@") > 1 {
		// One @ can be part of a model name ("my-model@v2"). Two means a split
		// that did not happen, and the second is almost certainly the userinfo
		// separator of a URL this parser failed to recognize.
		return nil, fmt.Errorf("%w: %q contains more than one @, so the base URL "+
			"could not be identified", ErrMalformed, redactRef(s))
	}

	if baseURL != "" {
		if !schemeUsesBaseURL(scheme) {
			return nil, fmt.Errorf("%w: the %s scheme reaches no endpoint, so a "+
				"base URL would be stored and never read", ErrMalformed, scheme)
		}
		canonical, err := checkBaseURL(baseURL)
		if err != nil {
			return nil, err
		}
		baseURL = canonical
	}

	if err := checkTarget(target); err != nil {
		return nil, err
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
		candidate := strings.TrimSpace(rest[i+1:])
		lower := strings.ToLower(candidate)
		if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
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
func checkBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		// Neither the URL nor the parse error is echoed: a malformed URL can
		// still carry a credential, and url.Parse quotes its input.
		return "", fmt.Errorf("%w: the base URL could not be parsed", ErrMalformed)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%w: the base URL has no host", ErrMalformed)
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
		return "", fmt.Errorf("%w: the base URL carries a %s. It names an endpoint "+
			"root, and anything after it would be appended to every request",
			ErrMalformed, map[bool]string{true: "fragment", false: "query string"}[u.Fragment != ""])
	}
	if u.User != nil {
		// Refused, not stripped. The credential is already in the user's shell
		// history; quietly rewriting it would hide that from them.
		return "", fmt.Errorf("%w: the base URL carries a credential in its userinfo. "+
			"Remove it and bind the key to the host through the environment",
			ErrMalformed)
	}

	// Canonicalized, because Ref is what checkResumable compares across a
	// resume. Stored byte-for-byte, "https://Host.Example.com/v1" and
	// "https://host.example.com/v1" name one endpoint and compare as two — so
	// a resume is refused, and the message tells the user the AGENT changed
	// when it did not, pointing them at a setting they never altered.
	u.Scheme = strings.ToLower(u.Scheme)

	// Rebuilt from Hostname and Port rather than trimmed off the raw host
	// string. Trimming ":443" off "http://:A:443" produced "http://:a", which
	// no longer parses — found by the fuzzer, and it broke the idempotency the
	// resume comparison depends on.
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("%w: the base URL has no host", ErrMalformed)
	}
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	switch {
	case port != "":
		u.Host = net.JoinHostPort(host, port) // brackets IPv6 for us
	case strings.Contains(host, ":"):
		// A bracketless IPv6 host is not a URL. Hostname() strips the brackets,
		// so writing it back unwrapped turned "http://[::]" into "http://::",
		// which re-parses as something else — breaking the idempotency the
		// resume comparison depends on. Found by the fuzzer.
		u.Host = "[" + host + "]"
	default:
		u.Host = host
	}
	// TrimRight, not TrimSuffix: removing ONE trailing slash turns "//" into
	// "/" and then "" on the next pass, so the canonical form of a canonical
	// form differed from itself. Found by the fuzzer.
	u.Path = strings.TrimRight(u.Path, "/")

	// Canonicalizing must not produce something that no longer parses. Checked
	// rather than assumed: this function rewrites a URL, and a rewrite that
	// invalidates it would make Ref unparseable — so a resume would compare a
	// reference against one that cannot be reconstructed.
	canonical := u.String()
	if _, err := url.Parse(canonical); err != nil {
		return "", fmt.Errorf("%w: the base URL could not be parsed", ErrMalformed)
	}
	return canonical, nil
}

// schemeUsesBaseURL reports whether a scheme reaches an HTTP endpoint at all.
//
// fake runs locally, exec runs a command, tuned names a job. Accepting an
// @base-url on those would store a value nothing reads — and it was the `fake:`
// vehicle that made the credential-in-target defect reachable end to end today,
// since fake is the only scheme this build resolves.
func schemeUsesBaseURL(scheme string) bool {
	return scheme == SchemeOpenAI || scheme == SchemeAnthropic
}

// checkTarget rejects characters that have no place in a model name.
//
// The target is reconstructed into Ref, which is written to SQLite, put on the
// event stream, and rendered wherever a run is displayed. A NUL or a newline
// there is a log-injection primitive waiting for the first log line that
// renders an agent ref with %s.
func checkTarget(target string) error {
	for _, r := range target {
		if r == 0 || r == '\n' || r == '\r' || r == '\t' {
			return fmt.Errorf("%w: the model name contains a control character",
				ErrMalformed)
		}
	}
	return nil
}

// userinfoPattern matches a `scheme://userinfo@` prefix.
//
// Applied to the raw string rather than to a parsed URL, because a reference
// reaches an error path precisely when it does not parse — and a malformed one
// may be malformed IN the scheme. `https:/user:pass@host` has a single slash,
// so a pattern anchored on "://" would leave the credential in place, which is
// how the first version of this leaked one.
var userinfoPattern = regexp.MustCompile(`(?i)([a-z][a-z0-9+.\-]*:/+)[^/?#@]*@`)

// redactRef removes a credential from a reference before it is quoted back.
//
// Parse refuses userinfo, but the refusals ABOVE that check quote the input
// too, and a reference can be malformed and credential-bearing at once.
//
// An earlier version keyed on finding a SECOND "@" after the first, so it left
// the common single-@ shape — "https://user:pass@host/v1" — completely
// unredacted, and the bare-URL refusal then interpolated it twice.
func redactRef(s string) string {
	return userinfoPattern.ReplaceAllString(s, "${1}[redacted]@")
}

// Redact removes userinfo from a reference or URL before it is shown.
//
// Exported because agentref is not the only place a user-supplied reference is
// echoed back: the CLI composes --base-url into a ref and can refuse before
// this package ever sees it, and an unredacted refusal reaches stderr and
// therefore CI logs. One redactor, wherever the echo happens.
func Redact(s string) string { return redactRef(s) }

func knownSchemeNames() []string {
	out := make([]string, 0, len(knownSchemes))
	for s := range knownSchemes {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
