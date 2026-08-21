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
