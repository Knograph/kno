package agentref_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/agent/agentref"
)

// TestGrammarGoldenTable.
//
// A table, not only a fuzz target. Fuzzing finds panics; the hazards in this
// grammar are misparses that SUCCEED — a model name silently truncated, a base
// URL that swallows half the model — and a parser that never panics can get
// every one of them wrong.
func TestGrammarGoldenTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		scheme  string
		target  string
		baseURL string
	}{
		{
			name:   "the ordinary case",
			in:     "openai:gpt-4.1",
			scheme: "openai", target: "gpt-4.1",
		},
		{
			name:   "anthropic",
			in:     "anthropic:claude-sonnet-4-6",
			scheme: "anthropic", target: "claude-sonnet-4-6",
		},
		{
			name:   "the fake agent",
			in:     "fake:",
			scheme: "fake", target: "",
		},
		{
			name:    "a base URL points one adapter at another provider",
			in:      "openai:llama-3.3-70b@https://api.groq.com/openai/v1",
			scheme:  "openai",
			target:  "llama-3.3-70b",
			baseURL: "https://api.groq.com/openai/v1",
		},
		{
			// Ollama spells models this way. Splitting the scheme anywhere but
			// the FIRST colon reassigns "8b" or "llama3" to the wrong part.
			name:   "a model name containing a colon",
			in:     "openai:llama3:8b",
			scheme: "openai", target: "llama3:8b",
		},
		{
			name:    "a colon-bearing model AND a base URL",
			in:      "openai:llama3:8b@http://localhost:11434/v1",
			scheme:  "openai",
			target:  "llama3:8b",
			baseURL: "http://localhost:11434/v1",
		},
		{
			// OpenRouter.
			name:   "a model name containing a slash and a colon",
			in:     "openai:meta-llama/llama-3.1-8b-instruct:free",
			scheme: "openai", target: "meta-llama/llama-3.1-8b-instruct:free",
		},
		{
			// An `@` with no URL after it is part of the model, not a split.
			name:   "an at-sign inside a model name",
			in:     "openai:my-model@v2",
			scheme: "openai", target: "my-model@v2",
		},
		{
			name:    "a base URL with a port",
			in:      "openai:local-model@http://localhost:8000/v1",
			scheme:  "openai",
			target:  "local-model",
			baseURL: "http://localhost:8000/v1",
		},
		{
			name:   "a scheme in mixed case is normalized",
			in:     "OpenAI:gpt-4.1",
			scheme: "openai", target: "gpt-4.1",
		},
		{
			name:   "surrounding whitespace is trimmed",
			in:     "  openai:gpt-4.1  ",
			scheme: "openai", target: "gpt-4.1",
		},
		{
			name:   "exec names a command",
			in:     "exec:kno-agent-mybot",
			scheme: "exec", target: "kno-agent-mybot",
		},
		{
			name:   "tuned names a job",
			in:     "tuned:job-abc123",
			scheme: "tuned", target: "job-abc123",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := agentref.Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.in, err)
			}
			if got.GetScheme() != tc.scheme {
				t.Errorf("scheme = %q, want %q", got.GetScheme(), tc.scheme)
			}
			if got.GetTarget() != tc.target {
				t.Errorf("target = %q, want %q", got.GetTarget(), tc.target)
			}
			if got.GetBaseUrl() != tc.baseURL {
				t.Errorf("base URL = %q, want %q", got.GetBaseUrl(), tc.baseURL)
			}
		})
	}
}

// TestRefusals: each of these is a mistake a user actually makes, and each
// refusal has to name what to write instead.
func TestRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		frag string
	}{
		{"empty", "", "empty"},
		{"only whitespace", "   ", "empty"},
		{"no scheme", "gpt-4.1", "no scheme"},
		{"an unknown scheme", "openai-compat:model", "unknown scheme"},
		{"a typo in the scheme", "opeani:gpt-4.1", "unknown scheme"},
		{"a scheme with no model", "openai:", "no model"},
		{"a scheme with only whitespace for a model", "openai:   ", "no model"},
		{"a bare URL, which names no model", "https://api.openai.com/v1", "bare URL"},
		{"a base URL with no host", "openai:m@https://", "no host"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := agentref.Parse(tc.in)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded", tc.in)
			}
			if !errors.Is(err, agentref.ErrMalformed) {
				t.Errorf("err = %v, want ErrMalformed", err)
			}
			if !strings.Contains(err.Error(), tc.frag) {
				t.Errorf("the refusal does not say why (%q):\n%s", tc.frag, err)
			}
		})
	}
}

// TestUserinfoIsRefusedAndNeverEchoed.
//
// Two properties, because getting one without the other is worse than useless:
// the reference is refused, AND the credential does not appear in the refusal.
// AgentRef.Ref is persisted on the Run, emitted on RunStarted, and rendered in
// --json and logs — so a parser that stored it would put a key in all four, and
// one that quoted it back would put a key in the error.
//
// The `@` inside the URL is also the case that decides the split rule: on the
// LAST `@` this parses as target "m@https://user:pass" and base URL
// "host/v1" — wrong, and wrong in a way that hides the credential from the
// check meant to catch it.
func TestUserinfoIsRefusedAndNeverEchoed(t *testing.T) {
	t.Parallel()

	const secret = "EXAMPLE-CREDENTIAL-VALUE"

	for _, in := range []string{
		"openai:m@https://user:" + secret + "@api.example.com/v1",
		"openai:m@https://" + secret + "@api.example.com/v1",
		"anthropic:claude@https://user:" + secret + "@host/v1",
	} {
		got, err := agentref.Parse(in)
		if err == nil {
			t.Errorf("Parse(%q) accepted a reference carrying a credential; "+
				"Ref would be persisted as %q", in, got.GetRef())
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the refusal quotes the credential:\n%s", err)
		}
	}
}

// TestRefIsReconstructedFromTheParts.
//
// Ref is what error messages quote back and what checkResumable compares across
// a resume, so it has to be canonical rather than verbatim — otherwise
// "OpenAI:gpt-4.1" and "openai:gpt-4.1" name the same agent and compare as
// different, and a resume is refused for a difference in capitalization.
func TestRefIsReconstructedFromTheParts(t *testing.T) {
	t.Parallel()

	a, err := agentref.Parse("  OpenAI:gpt-4.1  ")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, err := agentref.Parse("openai:gpt-4.1")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.GetRef() != b.GetRef() {
		t.Errorf("two spellings of one agent produced %q and %q; a resume would "+
			"be refused over capitalization", a.GetRef(), b.GetRef())
	}
}

// TestOnlyTheFakeSchemeMayOmitItsTarget.
//
// `fake:` is the CLI default and the first reference anyone reading the README
// types — it names the local deterministic agent, which has no model because it
// calls nothing. Every other scheme names something specific, and a scheme with
// no target is a half-typed reference rather than a valid one.
//
// The golden table caught this: a blanket "every scheme needs a target" rule
// refused the one spelling the docs use everywhere.
func TestOnlyTheFakeSchemeMayOmitItsTarget(t *testing.T) {
	t.Parallel()

	if _, err := agentref.Parse("fake:"); err != nil {
		t.Errorf("`fake:` was refused: %v", err)
	}
	for _, scheme := range []string{"openai", "anthropic", "exec", "tuned"} {
		if _, err := agentref.Parse(scheme + ":"); err == nil {
			t.Errorf("%s: was accepted with no target", scheme)
		}
	}
}

// TestAURLNeverLandsInTheTargetHoweverItIsSpelled.
//
// splitBaseURL matched "http://" case-sensitively, so HTTPS:// — or a leading
// space, or a fullwidth ＠ — failed to split and the whole URL, credential and
// all, landed in Target and was reconstructed into Ref. Ref is persisted on the
// Run, emitted on RunStarted, and rendered in --json. Verified end to end
// before the fix: four copies of the credential in kno.db, and `kno purge` left
// every one of them, because purge clears outcome traces and not the Run's own
// agent field.
//
// The post-condition this pins does not depend on the split being right. That
// is deliberate: the split rule can be wrong again, and this catches it.
func TestAURLNeverLandsInTheTargetHoweverItIsSpelled(t *testing.T) {
	t.Parallel()

	const secret = "EXAMPLE-CREDENTIAL"
	for _, in := range []string{
		"openai:m@HTTPS://user:" + secret + "@api.evil.com/v1",
		"openai:m@Https://user:" + secret + "@api.evil.com/v1",
		"openai:m@ https://user:" + secret + "@api.evil.com/v1",
		"openai:m@\thttps://user:" + secret + "@api.evil.com/v1",
		"openai:m＠https://user:" + secret + "@api.evil.com/v1", // fullwidth ＠
		"openai:m@https:/user:" + secret + "@api.evil.com/v1",  // one slash
		"fake:m@HTTPS://user:" + secret + "@api.evil.com/v1",
	} {
		got, err := agentref.Parse(in)
		if err == nil {
			t.Errorf("Parse(%q) succeeded with Target=%q Ref=%q; a credential in "+
				"Ref reaches the Run record, the event stream, and --json, and "+
				"kno purge does not remove it",
				in, got.GetTarget(), got.GetRef())
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the refusal quotes the credential:\n%s", err)
		}
	}
}

// TestASingleAtCredentialIsRedactedInEveryRefusal.
//
// redactRef keyed on finding a SECOND "@" after the first, so the common
// single-@ shape was returned unchanged — and the bare-URL refusal then
// interpolated it twice.
func TestASingleAtCredentialIsRedactedInEveryRefusal(t *testing.T) {
	t.Parallel()

	const secret = "EXAMPLE-TOKEN-VALUE"
	for _, in := range []string{
		"https://" + secret + ":pw@api.evil.com/v1", // bare URL, one @
		"https://" + secret + "@api.evil.com/v1",    // userinfo, no password
		"ftp://" + secret + ":pw@host/v1",           // unknown scheme
		"openai:m@https://" + secret + ":pw@h/v1",   // two @
	} {
		_, err := agentref.Parse(in)
		if err == nil {
			t.Errorf("Parse(%q) succeeded", in)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("Parse(%q) echoed the credential:\n%s", in, err)
		}
	}
}

// TestCanonicalFormSurvivesAResume.
//
// Ref is what core.checkResumable compares. Stored byte-for-byte, two spellings
// of one endpoint compare as two agents, and the user is told the AGENT changed
// — pointed at a setting they never altered.
func TestCanonicalFormSurvivesAResume(t *testing.T) {
	t.Parallel()

	groups := [][]string{
		{"openai:gpt-4.1", "OpenAI:gpt-4.1", "  openai:gpt-4.1  ", "openai: gpt-4.1"},
		{
			"openai:m@https://host.example.com/v1",
			"openai:m@https://Host.Example.com/v1",
			"openai:m@HTTPS://HOST.EXAMPLE.COM/v1",
			"openai:m@https://host.example.com:443/v1",
			"openai:m@https://host.example.com/v1/",
		},
	}

	for _, group := range groups {
		want, err := agentref.Parse(group[0])
		if err != nil {
			t.Fatalf("Parse(%q): %v", group[0], err)
		}
		for _, in := range group[1:] {
			got, err := agentref.Parse(in)
			if err != nil {
				t.Errorf("Parse(%q): %v", in, err)
				continue
			}
			if got.GetRef() != want.GetRef() {
				t.Errorf("%q and %q name one agent but produced %q and %q; a resume "+
					"would be refused over spelling", group[0], in,
					want.GetRef(), got.GetRef())
			}
		}
	}
}

// TestABaseURLIsRefusedForSchemesThatReachNoEndpoint.
//
// fake runs locally, exec runs a command, tuned names a job. Storing a base URL
// for those keeps a value nothing reads — and `fake:` was the vehicle that made
// the credential-in-target defect reachable today, since it is the only scheme
// this build resolves.
func TestABaseURLIsRefusedForSchemesThatReachNoEndpoint(t *testing.T) {
	t.Parallel()

	for _, scheme := range []string{"fake", "exec", "tuned"} {
		if _, err := agentref.Parse(scheme + ":x@https://host/v1"); err == nil {
			t.Errorf("%s: accepted a base URL it will never read", scheme)
		}
	}
	for _, scheme := range []string{"openai", "anthropic"} {
		if _, err := agentref.Parse(scheme + ":m@https://host/v1"); err != nil {
			t.Errorf("%s: refused a base URL it needs: %v", scheme, err)
		}
	}
}

// TestControlCharactersAreRefusedInATarget: Ref reaches SQLite, the event
// stream, and anything that renders a run. A newline there is a log-injection
// primitive waiting for the first log line that formats an agent ref.
func TestControlCharactersAreRefusedInATarget(t *testing.T) {
	t.Parallel()

	for _, in := range []string{
		"openai:m\x00odel",
		"openai:m\nINFO an injected log line",
		"openai:m\rmodel",
	} {
		if _, err := agentref.Parse(in); err == nil {
			t.Errorf("Parse(%q) accepted a control character into Ref", in)
		}
	}
}
