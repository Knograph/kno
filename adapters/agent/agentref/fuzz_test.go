package agentref_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/agent/agentref"
)

// FuzzParse is the first fuzz target in this repository, and repays part of
// docs/debt.md#4.
//
// It asserts INVARIANTS rather than absence of panics, because a parser that
// never panics can still get every interesting case wrong — which is what the
// golden table is for. What fuzzing adds is the inputs nobody thinks to write:
// embedded NULs, lone surrogates, a thousand colons, an `@` at the end.
//
// The invariants are the ones the rest of the system depends on:
//
//  1. A parsed reference never carries a credential. AgentRef.Ref is persisted
//     on the Run, emitted on RunStarted, and rendered in --json and logs.
//  2. The parts reconstruct the Ref, so what is stored is what was parsed and
//     a resume comparing Refs compares the same thing that ran.
//  3. Parsing is idempotent — Parse(Ref) yields the same Ref. A canonical form
//     that is not stable would refuse a resume against a run it produced.
func FuzzParse(f *testing.F) {
	seeds := []string{
		"openai:gpt-4.1",
		"fake:",
		"openai:llama3:8b@http://localhost:11434/v1",
		"openai:meta-llama/llama-3.1-8b-instruct:free",
		"openai:m@https://user:pass@host/v1",
		"anthropic:claude-sonnet-4-6",
		"https://api.openai.com/v1",
		"openai:my-model@v2",
		"::::",
		"@@@@",
		"openai:@",
		"OpenAI:GPT-4.1@HTTPS://HOST/V1",
		"openai:model@http://[::1]:8000/v1",
		"\x00openai:m",
		"openai:m@HTTPS://user:pw@host/v1",
		"openai:m@ https://user:pw@host/v1",
		"openai:m@https:/user:pw@host/v1",
		"openai:m@https://Host.Example.COM:443/v1/",
		"fake:x@https://host/v1",
		strings.Repeat("openai:", 50),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, in string) {
		got, err := agentref.Parse(in)
		if err != nil {
			return // a refusal is always an acceptable answer
		}

		// 1. No credential survives into anything persisted.
		//
		// Asserted by parsing, not by scanning for "@". An `@` is legitimate in
		// a model name, in a URL path ("http://host/@"), and in a fragment —
		// the fuzzer produced both of the latter within seconds, and a string
		// scan called them credentials. Userinfo is the actual property, and
		// url.Parse is what knows the difference.
		if b := got.GetBaseUrl(); b != "" {
			u, err := url.Parse(b)
			if err != nil {
				t.Errorf("Parse(%q) produced an unparseable base URL %q", in, b)
			} else if u.User != nil {
				t.Errorf("Parse(%q) produced a base URL carrying userinfo: %q", in, b)
			}
		}

		// 2. The parts reconstruct the reference.
		want := got.GetScheme() + ":" + got.GetTarget()
		if b := got.GetBaseUrl(); b != "" {
			want += "@" + b
		}
		if got.GetRef() != want {
			t.Errorf("Parse(%q): Ref = %q but its parts reconstruct to %q",
				in, got.GetRef(), want)
		}

		// 3. A URL never sits in the model slot.
		//
		// This is the invariant whose ABSENCE let the author's own seed —
		// "OpenAI:GPT-4.1@HTTPS://HOST/V1", seed 11 below — pass every other
		// check while smuggling an absolute URL into Target and from there into
		// Ref, which is persisted and emitted. The other invariants could not
		// see it: BaseUrl was empty so 1 was skipped, and 2 and 4 were
		// satisfied by construction.
		if target := got.GetTarget(); strings.Contains(target, ":/") {
			t.Errorf("Parse(%q) put a URL in Target=%q; Ref=%q is persisted on the "+
				"Run, emitted on RunStarted, and rendered in --json",
				in, target, got.GetRef())
		}

		// There is deliberately no fourth invariant about "@" in Ref.
		//
		// Two formulations were tried and both were resemblances rather than
		// properties. A userinfo-shaped regexp flagged "openai:/@", which
		// carries no URL. Counting "@" flagged "openai:0@http://0/@", where the
		// second one is inside the URL PATH and entirely legitimate — the
		// fuzzer's own earlier regression seed.
		//
		// Invariants 1 and 3 together already state it exactly: a credential
		// needs a URL, a URL in the target is caught by 3, and a credential in
		// the base URL is caught by 1. Adding a third check that approximates
		// the same thing only adds false positives.

		// 5. Canonical form is stable.
		again, err := agentref.Parse(got.GetRef())
		if err != nil {
			t.Fatalf("Parse(%q) produced Ref %q, which does not parse: %v",
				in, got.GetRef(), err)
		}
		if again.GetRef() != got.GetRef() {
			t.Errorf("Parse is not idempotent: %q -> %q -> %q",
				in, got.GetRef(), again.GetRef())
		}
	})
}
