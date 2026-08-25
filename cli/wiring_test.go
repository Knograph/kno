package cli_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/cli"
	"github.com/knograph/kno/core/errs"
)

// TestABaseURLIsValidatedByOneParser.
//
// agentref.Parse holds the credential-in-a-URL, control-character, and scheme
// refusals, so --base-url composes into the ref rather than being passed to the
// adapter beside it: a second entry point that skipped them would be a second
// place for a key to reach the Run record, which
// openaicompat.Options.Ref's godoc says in as many words.
//
// Two composition holes are covered here because both fail SILENTLY at the
// wrong endpoint rather than loudly at the flag.
func TestABaseURLIsValidatedByOneParser(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 30)

	tests := []struct {
		name string
		args []string
		frag string
	}{
		{
			// splitBaseURL takes the first `@` whose remainder is absolute, so
			// naive concatenation yields "https://a/v1@https://b/v1" — host a,
			// path "/v1@https:/b/v1", no error. The run goes to a with a
			// garbage path and --base-url is silently ignored.
			name: "an endpoint named twice",
			args: []string{
				"--agent", "openai:m@https://a.example.com/v1",
				"--base-url", "https://b.example.com/v1",
			},
			frag: "already names a base URL",
		},
		{
			// "h/v1" composes to "openai:m@h/v1"; splitBaseURL finds no
			// absolute URL, so the flag is absorbed into the MODEL NAME and
			// the provider answers 404 "check the model name".
			name: "a base URL with no scheme",
			args: []string{"--agent", "openai:m", "--base-url", "h/v1"},
			frag: "--base-url has no http or https scheme",
		},
		{
			// A one-slash typo used to be refused with "puts a URL where the
			// model belongs. A base URL is introduced by `@`" — a message
			// about `@` for a user who never typed one.
			name: "a one-slash typo",
			args: []string{"--agent", "openai:m", "--base-url", "https:/h.example.com"},
			frag: "--base-url",
		},
		{
			name: "a base URL with no host",
			args: []string{"--agent", "openai:m", "--base-url", "https://"},
			frag: "--base-url names no host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			args := append([]string{
				"baseline", "--evals", cases,
				"--db", filepath.Join(t.TempDir(), "kno.db"),
			}, tt.args...)
			_, stderr, code := run(t, args...)

			if code == errs.ExitOK {
				t.Fatal("the misconfiguration was accepted")
			}
			if !strings.Contains(stderr, tt.frag) {
				t.Errorf("the message does not say %q:\n%s", tt.frag, stderr)
			}
			if !strings.Contains(stderr, "fix:") {
				t.Errorf("no fix line:\n%s", stderr)
			}
		})
	}
}

// TestACredentialIsNeverAFlagValue.
//
// --key-env names an environment VARIABLE. A key on a command line lands in
// shell history, in ps output, and in CI logs, and nothing downstream can take
// it back — so the obvious mistake is caught at the flag, and the refusal does
// not echo the value it is refusing.
func TestACredentialIsNeverAFlagValue(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 30)
	const secret = "sk-live-DEADBEEF-not-a-variable-name"

	_, stderr, code := run(t, "baseline", "--evals", cases,
		"--agent", "openai:gpt-4.1",
		"--key-env", "api.openai.com="+secret,
		"--db", filepath.Join(t.TempDir(), "kno.db"))

	if code == errs.ExitOK {
		t.Fatal("a key passed where a variable name belongs was accepted")
	}
	if strings.Contains(stderr, secret) {
		t.Errorf("the refusal ECHOED the credential, which is how a near-miss "+
			"becomes the leak the flag exists to prevent:\n%s", stderr)
	}
	if !strings.Contains(stderr, "environment") {
		t.Errorf("the message does not explain what the value should be:\n%s", stderr)
	}
}

// TestAMalformedKeyBindingDoesNotEchoItself, for the same reason: a user who
// typed the key in the wrong shape still typed the key.
func TestAMalformedKeyBindingDoesNotEchoItself(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 30)
	const secret = "AKIAIOSFODNN7EXAMPLE"

	_, stderr, _ := run(t, "baseline", "--evals", cases,
		"--agent", "openai:gpt-4.1",
		"--key-env", secret,
		"--db", filepath.Join(t.TempDir(), "kno.db"))

	if strings.Contains(stderr, secret) {
		t.Errorf("the refusal echoed the binding:\n%s", stderr)
	}
	if !strings.Contains(stderr, "host=VAR") {
		t.Errorf("the message does not say the expected shape:\n%s", stderr)
	}
}

// TestAProviderRunWithNoCredentialIsRefusedBeforeAnyRequest.
//
// It used to proceed: connect simply omitted the Authorization header, so every
// Case made a real request and collected a 401. That is now run-fatal and
// therefore bounded, but it is still one paid round trip and a message about a
// provider rejecting a credential that was never sent. Closes docs/debt.md#57.
func TestAProviderRunWithNoCredentialIsRefusedBeforeAnyRequest(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 30)
	_, stderr, code := run(t, "baseline", "--evals", cases,
		"--agent", "openai:gpt-4.1", "--max-output-tokens", "64",
		"--db", filepath.Join(t.TempDir(), "kno.db"))

	if code == errs.ExitOK {
		t.Fatal("a provider run with no credential started")
	}
	if !strings.Contains(stderr, "no credential") {
		t.Errorf("the message does not say what is missing:\n%s", stderr)
	}
	if !strings.Contains(stderr, "OPENAI_API_KEY") {
		t.Errorf("the fix does not name the variable to export:\n%s", stderr)
	}
}

// TestPriceOverrideFlagsAreAPair.
//
// EstimateWithPrice refuses unless both terms are set, because half a price
// produces an estimate that is wrong in the direction that UNDER-reserves —
// which is a cap that does not bind.
func TestPriceOverrideFlagsAreAPair(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 30)
	for _, args := range [][]string{
		{"--price-input-per-mtok", "3.00"},
		{"--price-output-per-mtok", "15.00"},
	} {
		t.Run(args[0], func(t *testing.T) {
			t.Parallel()

			full := append([]string{
				"baseline", "--evals", cases,
				"--agent", "fake:", "--db", filepath.Join(t.TempDir(), "kno.db"),
			}, args...)
			_, stderr, code := run(t, full...)

			if code == errs.ExitOK {
				t.Fatal("half a price was accepted")
			}
			if !strings.Contains(stderr, "--price-input-per-mtok") ||
				!strings.Contains(stderr, "--price-output-per-mtok") {
				t.Errorf("the fix does not name both flags:\n%s", stderr)
			}
		})
	}
}

// TestTheWidthLineAppearsOnlyWhenTheEngineNarrowedIt.
//
// checkFeasible narrows concurrency when the cost cap cannot admit the width
// asked for, and it did so with no event, no log line, and no field on the Run
// — a 6x slowdown the user did not ask for and could not see (docs/debt.md#44).
//
// The gate is the REASON, not requested != effective. `requested` is
// presence-carrying and absent means "no particular width", so a default run
// reports requested=0 against an effective 8 — the default being applied, not a
// reduction. The first version of this line printed
// "width 8 (asked for 0; unspecified)" on every ordinary run.
func TestTheWidthLineAppearsOnlyWhenTheEngineNarrowedIt(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 60)

	t.Run("a run that got the width it asked for says nothing", func(t *testing.T) {
		t.Parallel()
		stdout, _, code := run(t, "baseline", "--evals", cases, "--agent", "fake:",
			"--concurrency", "4", "--db", filepath.Join(t.TempDir(), "kno.db"))
		if code != errs.ExitOK {
			t.Fatalf("exit = %d", code)
		}
		if strings.Contains(stdout, "width") {
			t.Errorf("a run that was not narrowed reported a width:\n%s", stdout)
		}
	})

	t.Run("a default run says nothing either", func(t *testing.T) {
		t.Parallel()
		stdout, _, code := run(t, "baseline", "--evals", cases, "--agent", "fake:",
			"--db", filepath.Join(t.TempDir(), "kno.db"))
		if code != errs.ExitOK {
			t.Fatalf("exit = %d", code)
		}
		if strings.Contains(stdout, "width") {
			t.Errorf("a default run reported being narrowed from the width it "+
				"never asked for:\n%s", stdout)
		}
	})

	t.Run("a narrowed DEFAULT run does not claim a request", func(t *testing.T) {
		t.Parallel()
		// The case the reason-gate did not cover, and the one that actually
		// shipped the bad line: narrowed, with no --concurrency. Requested is
		// presence-carrying and absent, so printing it said "asked for 0" to
		// someone who asked for nothing. core deliberately does not record the
		// defaulted width as a request — there is a test pinning that — so the
		// fix belongs in the renderer.
		stdout, _, _ := run(t, "baseline", "--evals", cases, "--agent", "fake:",
			"--max-cost-usd", "0.05", "--cost-per-call-usd", "0.01", "--yes",
			"--db", filepath.Join(t.TempDir(), "kno.db"))

		if !strings.Contains(stdout, "width") {
			t.Fatalf("the run was not narrowed, so this proves nothing:\n%s", stdout)
		}
		if strings.Contains(stdout, "asked for 0") {
			t.Errorf("the report told a user who requested nothing that they "+
				"requested zero:\n%s", stdout)
		}
		if !strings.Contains(stdout, "narrowed from the default") {
			t.Errorf("the report does not say the default was narrowed:\n%s", stdout)
		}
	})

	t.Run("a narrowed run says so, and --json carries it", func(t *testing.T) {
		t.Parallel()
		// A cost cap far too small to admit 32 in flight forces the narrowing.
		args := []string{
			"baseline", "--evals", cases, "--agent", "fake:",
			"--concurrency", "32", "--max-cost-usd", "0.05",
			"--cost-per-call-usd", "0.01", "--yes",
			"--db", filepath.Join(t.TempDir(), "kno.db"),
		}

		stdout, _, _ := run(t, args...)
		if !strings.Contains(stdout, "width") {
			t.Errorf("the engine narrowed the run and the report does not say so:\n%s", stdout)
		}
		if !strings.Contains(stdout, "cost-cap") {
			t.Errorf("the report does not say WHY it was narrowed:\n%s", stdout)
		}

		jsonOut, _, _ := run(t, append(args, "--json")...)
		rep, err := cli.DecodeRaw([]byte(jsonOut))
		if err != nil {
			t.Fatalf("decoding --json: %v\n%s", err, jsonOut)
		}
		for _, k := range []string{
			"concurrency", "concurrency_requested",
			"concurrency_reduced_reason",
		} {
			if _, ok := rep[k]; !ok {
				t.Errorf("--json is missing %q; a machine consumer cannot tell "+
					"a narrowed run from a slow one", k)
			}
		}
		// Named, not numeric: a pipeline branching on 1 breaks the day an enum
		// value is inserted.
		if got := rep["concurrency_reduced_reason"]; got != "cost-cap" {
			t.Errorf("reason = %v, want the NAME cost-cap", got)
		}
	})
}

// TestDoctorPrintsTheMatrixWithoutSpendingOrReadingACredential.
//
// errs.ErrCapabilityUnsupported's fix line has always said "run `kno doctor`",
// and no such command existed — so every capability refusal pointed the user at
// nothing. It must also be free and credential-free: somebody diagnosing a
// misconfiguration should not have to risk a bill to ask what is supported.
func TestDoctorPrintsTheMatrixWithoutSpendingOrReadingACredential(t *testing.T) {
	t.Parallel()

	stdout, _, code := run(t, "doctor")
	if code != errs.ExitOK {
		t.Fatalf("exit = %d:\n%s", code, stdout)
	}

	for _, want := range []string{"openai:", "anthropic:", "fake:", "exec:", "tuned:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the matrix omits %q:\n%s", want, stdout)
		}
	}
	// Unavailable schemes are LISTED rather than omitted: "kno does not know
	// that word" and "kno knows it and this build cannot serve it" are
	// different problems with different fixes.
	if !strings.Contains(stdout, "not in this build") {
		t.Errorf("the matrix hides the schemes this build cannot serve:\n%s", stdout)
	}
	if !strings.Contains(stdout, "free") || !strings.Contains(stdout, "spends") {
		t.Errorf("the matrix does not say which agents cost money:\n%s", stdout)
	}

	jsonOut, _, code := run(t, "doctor", "--json")
	if code != errs.ExitOK {
		t.Fatalf("--json exit = %d", code)
	}
	rep, err := cli.DecodeRaw([]byte(jsonOut))
	if err != nil {
		t.Fatalf("decoding: %v\n%s", err, jsonOut)
	}
	for _, k := range []string{"adapters", "goals", "price_table", "priced_models"} {
		if _, ok := rep[k]; !ok {
			t.Errorf("--json is missing %q", k)
		}
	}
}

// TestTheAnthropicSchemeIsReachableAndSaysWhatItNeeds.
//
// The Messages API requires max_tokens, so the adapter refuses without a
// ceiling rather than 400ing every Case — and a cost cap cannot bound an output
// term that has no ceiling either.
func TestTheAnthropicSchemeIsReachableAndSaysWhatItNeeds(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 30)
	db := filepath.Join(t.TempDir(), "kno.db")

	t.Run("no output ceiling", func(t *testing.T) {
		t.Parallel()
		_, stderr, code := run(t, "baseline", "--evals", cases,
			"--agent", "anthropic:claude-opus-5", "--db", db)
		if code == errs.ExitOK {
			t.Fatal("a run with no output ceiling started")
		}
		if !strings.Contains(stderr, "--max-output-tokens") {
			t.Errorf("the fix does not name the flag:\n%s", stderr)
		}
	})

	t.Run("no credential", func(t *testing.T) {
		t.Parallel()
		_, stderr, code := run(t, "baseline", "--evals", cases,
			"--agent", "anthropic:claude-opus-5", "--max-output-tokens", "256",
			"--db", filepath.Join(t.TempDir(), "kno.db"))
		if code == errs.ExitOK {
			t.Fatal("a provider run with no credential started")
		}
		if !strings.Contains(stderr, "ANTHROPIC_API_KEY") {
			t.Errorf("the fix does not name the variable to export:\n%s", stderr)
		}
	})
}

// TestGenerationParamsIsATriState.
//
// Nothing in the tree calls Capabilities(), so whether a model accepts
// generation parameters is answered at adapter construction — which makes this
// override load-bearing rather than a convenience. See docs/debt.md#58.
func TestGenerationParamsIsATriState(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 30)

	for _, v := range []string{"auto", "on", "off"} {
		t.Run(v, func(t *testing.T) {
			t.Parallel()
			_, _, code := run(t, "baseline", "--evals", cases, "--agent", "fake:",
				"--generation-params", v, "--db", filepath.Join(t.TempDir(), "kno.db"))
			if code != errs.ExitOK {
				t.Errorf("--generation-params %s was refused", v)
			}
		})
	}

	t.Run("anything else", func(t *testing.T) {
		t.Parallel()
		_, stderr, code := run(t, "baseline", "--evals", cases,
			"--agent", "openai:gpt-4.1", "--generation-params", "maybe",
			"--db", filepath.Join(t.TempDir(), "kno.db"))
		if code == errs.ExitOK {
			t.Fatal("an unknown value was accepted")
		}
		if !strings.Contains(stderr, "auto, on, or off") {
			t.Errorf("the fix does not name the valid values:\n%s", stderr)
		}
	})
}

// TestGenerationFlagsReachTheAdapter, so a flag that parses but is dropped on
// the floor is caught. A silently ignored --temperature is worse than a
// refused one: the run reports numbers measured under settings nobody chose.
func TestGenerationFlagsReachTheAdapter(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 30)

	// A negative price is refused at the flag rather than inside a constructor,
	// so it is caught whichever scheme it is paired with.
	_, stderr, code := run(t, "baseline", "--evals", cases, "--agent", "fake:",
		"--price-input-per-mtok", "-1", "--price-output-per-mtok", "-2",
		"--db", filepath.Join(t.TempDir(), "kno.db"))
	if code == errs.ExitOK {
		t.Fatal("a negative price was accepted")
	}
	if !strings.Contains(stderr, "credit the budget") {
		t.Errorf("the message does not say why a negative price is wrong:\n%s", stderr)
	}

	// Temperature and seed pass through to a run that costs nothing.
	_, _, code = run(t, "baseline", "--evals", cases, "--agent", "fake:",
		"--temperature", "0", "--seed", "42", "--system", "be terse",
		"--max-output-tokens", "128", "--max-prompt-bytes", "100000",
		"--timeout", "30s", "--use-legacy-max-tokens",
		"--db", filepath.Join(t.TempDir(), "kno.db"))
	if code != errs.ExitOK {
		t.Errorf("the generation flags were refused on a free agent: exit %d", code)
	}
}

// TestARunThatCannotStateItsCostIsRefusedAtTheCLI is the end-to-end half of
// the consent guard: the engine refuses, and the CLI surfaces the flag that
// gets through.
func TestARunThatCannotStateItsCostIsRefusedAtTheCLI(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 30)
	srv := t.TempDir()

	// A self-hosted endpoint needs no credential, prices nothing, and is
	// therefore the reachable case where no per-Case cost can be computed.
	args := []string{
		"baseline", "--evals", cases,
		"--agent", "openai:my-local-model",
		"--base-url", "http://127.0.0.1:1/v1",
		"--allow-insecure-base-url", "--allow-private-address",
		"--db", filepath.Join(srv, "kno.db"),
	}

	_, stderr, code := run(t, args...)
	if code == errs.ExitOK {
		t.Fatal("a run that cannot state its cost started")
	}
	if !strings.Contains(stderr, "--accept-unknown-cost") {
		t.Errorf("the fix does not name the way through:\n%s", stderr)
	}
	if !strings.Contains(stderr, "--cost-per-call-usd") {
		t.Errorf("the fix does not name the other way through:\n%s", stderr)
	}
}

// TestTheCLIAndAgentrefAgreeOnWhichURLIsTheBaseURL.
//
// --base-url is composed into the ref rather than passed alongside so that
// agentref.Parse is the ONLY validator. That guarantee holds only if the CLI's
// "is there already a base URL here" test agrees with agentref's byte for byte.
//
// The first version did not: it matched the scheme case-sensitively and
// without trimming, while agentref lowercases and trims first. So
// `--agent openai:m@HTTPS://evil/v1 --base-url https://good/v1` slipped past
// the double-endpoint refusal, composed into one ref, and agentref then took
// the SECOND URL as the base — the CLI validated good and the adapter dialled
// evil. The address policy still applied to the real host, so the consequence
// was a silently wrong endpoint rather than a bypassed refusal, but the
// guarantee was gone.
func TestTheCLIAndAgentrefAgreeOnWhichURLIsTheBaseURL(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 30)
	for _, ref := range []string{
		"openai:m@https://evil.example.com/v1",
		"openai:m@HTTPS://evil.example.com/v1",
		"openai:m@ https://evil.example.com/v1",
		"openai:m@HtTpS://evil.example.com/v1",
	} {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()
			_, stderr, code := run(t, "baseline", "--evals", cases, "--agent", ref,
				"--base-url", "https://good.example.com/v1",
				"--db", filepath.Join(t.TempDir(), "kno.db"))
			if code == errs.ExitOK {
				t.Fatal("two endpoints were accepted")
			}
			if !strings.Contains(stderr, "already names a base URL") {
				t.Errorf("the double-endpoint refusal did not fire, so the CLI "+
					"validated one URL and the adapter would use another:\n%s", stderr)
			}
		})
	}
}

// TestARefusalNeverEchoesACredential.
//
// composeRef refuses before agentref ever sees the ref, so agentref's own
// non-echoing userinfo refusal cannot help — and this message reaches stderr
// and therefore the CI log. SECURITY.md asserts the value never appears.
func TestARefusalNeverEchoesACredential(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 30)
	const secret = "sk-SUPERSECRET-should-never-print"

	_, stderr, code := run(t, "baseline", "--evals", cases,
		"--agent", "openai:m@https://user:"+secret+"@a.example.com/v1",
		"--base-url", "https://b.example.com/v1",
		"--db", filepath.Join(t.TempDir(), "kno.db"))

	if code == errs.ExitOK {
		t.Fatal("two endpoints were accepted")
	}
	if strings.Contains(stderr, secret) {
		t.Errorf("the refusal echoed the credential into stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "redacted") {
		t.Errorf("the refusal does not show that something was removed:\n%s", stderr)
	}
}

// TestABindingIsValidatedByTheTransportNotByACopyOfIt.
//
// The CLI built a raw map from --key-env and the adapter cast it, skipping
// transport.ParseKeyBindings entirely. Two things broke:
//
//   - Resolve looks up a NORMALIZED host — lowercased, port stripped — so a
//     binding written with a port was stored with it, silently resolved
//     nothing, and the request went out unauthenticated. That is verbatim the
//     defect keybinding.go records as already fixed.
//   - The CLI's replacement key-shape check was a 7-prefix denylist that
//     missed `gsk_`, which is Groq, which is the worked example in this
//     project's own cookbook.
func TestABindingIsValidatedByTheTransportNotByACopyOfIt(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 30)

	t.Run("a key pasted where a variable name belongs", func(t *testing.T) {
		t.Parallel()
		const secret = "gsk_ABCdefGHIjklMNOpqrSTUvwx"
		_, stderr, code := run(t, "baseline", "--evals", cases,
			"--agent", "openai:m", "--base-url", "https://api.groq.com/openai/v1",
			"--key-env", "api.groq.com="+secret,
			"--db", filepath.Join(t.TempDir(), "kno.db"))

		if code == errs.ExitOK {
			t.Fatal("a key was accepted as a variable name")
		}
		if !strings.Contains(stderr, "looks like a key") {
			t.Errorf("a Groq-shaped key was not recognized:\n%s", stderr)
		}
		if strings.Contains(stderr, secret) {
			t.Errorf("the refusal echoed the key:\n%s", stderr)
		}
	})

	t.Run("a host written with a port still binds", func(t *testing.T) {
		t.Parallel()
		_, stderr, _ := run(t, "baseline", "--evals", cases,
			"--agent", "openai:m", "--base-url", "https://api.groq.com/openai/v1",
			"--key-env", "api.groq.com:443=SOME_KEY_VAR",
			"--db", filepath.Join(t.TempDir(), "kno.db"))

		// The binding resolves, so the run gets past credential resolution and
		// fails later (on the unknown cost) rather than on "no credential".
		if strings.Contains(stderr, "no credential") {
			t.Errorf("a binding written with a port silently resolved nothing, "+
				"so the request would go out unauthenticated:\n%s", stderr)
		}
	})

	t.Run("the same host bound twice", func(t *testing.T) {
		t.Parallel()
		_, stderr, code := run(t, "baseline", "--evals", cases,
			"--agent", "openai:m", "--base-url", "https://gw.example.com/v1",
			"--key-env", "gw.example.com=FIRST_VARIABLE",
			"--key-env", "GW.EXAMPLE.COM:443=SECOND_VARIABLE",
			"--db", filepath.Join(t.TempDir(), "kno.db"))

		if code == errs.ExitOK {
			t.Fatal("one host bound to two variables was accepted; argv order " +
				"would decide which key is sent")
		}
		_ = stderr
	})
}

// TestYesPrintsTheEstimateEvenBelowTheThreshold.
//
// The first version printed from inside the ConfirmFunc, which PreConfirm
// short-circuits below the $1.00 threshold — so --yes was silent for exactly
// the runs small enough not to prompt, while the flag's help, the cookbook,
// the CI recipe, and the plan all promised a figure unconditionally.
func TestYesPrintsTheEstimateEvenBelowTheThreshold(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 60)

	stdout, _, code := run(t, "baseline", "--evals", cases, "--agent", "fake:",
		"--max-cost-usd", "0.50", "--cost-per-call-usd", "0.001", "--yes",
		"--db", filepath.Join(t.TempDir(), "kno.db"))
	if code != errs.ExitOK {
		t.Fatalf("exit = %d:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "Proceeding with --yes") {
		t.Errorf("--yes printed no figure for a run below the prompt threshold:\n%s", stdout)
	}

	// In --json mode stdout is a machine contract: a prose line ahead of the
	// document makes it unparseable. The figure travels in the report.
	jsonOut, _, _ := run(t, "baseline", "--evals", cases, "--agent", "fake:",
		"--max-cost-usd", "0.50", "--cost-per-call-usd", "0.001", "--yes", "--json",
		"--db", filepath.Join(t.TempDir(), "kno.db"))
	if strings.Contains(jsonOut, "Proceeding with --yes") {
		t.Errorf("the prose line corrupted --json stdout:\n%s", jsonOut)
	}
	rep, err := cli.DecodeRaw([]byte(jsonOut))
	if err != nil {
		t.Fatalf("decoding --json: %v\n%s", err, jsonOut)
	}
	if _, ok := rep["estimated_usd"]; !ok {
		t.Errorf("--json records no estimate, so a run that waived the prompt "+
			"has no record of what it waived:\n%s", jsonOut)
	}
}

// TestTheLocalModelServerRecipeRuns, verbatim from the cookbook.
//
// The documented recipe passed --cost-per-call-usd 0 and was refused with a fix
// line naming the flag it had just passed: 0 is also the default, so the value
// alone could not express "these calls are free". An explicit flag is now the
// claim.
func TestTheLocalModelServerRecipeRuns(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 30)
	_, stderr, _ := run(t, "baseline", "--evals", cases,
		"--agent", "openai:my-local-model",
		"--base-url", "http://127.0.0.1:1/v1",
		"--allow-insecure-base-url", "--allow-private-address",
		"--cost-per-call-usd", "0",
		"--db", filepath.Join(t.TempDir(), "kno.db"))

	// It reaches the provider (and fails to connect, since nothing is
	// listening) rather than being refused up front for an unknown cost.
	if strings.Contains(stderr, "--accept-unknown-cost") {
		t.Errorf("the documented recipe is still refused, with a fix line "+
			"naming a flag the user just passed:\n%s", stderr)
	}
}

// TestTraceSpansGoToStderrNotStdout.
//
// stdout is the report, and --json makes it a machine contract. A span
// document interleaved there makes it unparseable — the same failure a
// one-line consent notice already caused once in this milestone.
func TestTraceSpansGoToStderrNotStdout(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 30)
	stdout, stderr, code := run(t, "baseline", "--evals", cases, "--agent", "fake:",
		"--trace-spans", "--json", "--db", filepath.Join(t.TempDir(), "kno.db"))
	if code != errs.ExitOK {
		t.Fatalf("exit = %d:\n%s", code, stderr)
	}

	if _, err := cli.DecodeRaw([]byte(stdout)); err != nil {
		t.Errorf("--trace-spans corrupted the --json contract: %v\n%s", err, stdout)
	}
	if !strings.Contains(stderr, "kno.baseline") {
		t.Errorf("no run span reached stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "kno.case") {
		t.Errorf("no Case spans reached stderr:\n%s", stderr)
	}
}

// TestTracingIsOffByDefault, so a run that did not ask for spans emits none —
// and so the flag is a real opt-in rather than a filter over output that was
// being produced anyway.
func TestTracingIsOffByDefault(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 30)
	_, stderr, code := run(t, "baseline", "--evals", cases, "--agent", "fake:",
		"--db", filepath.Join(t.TempDir(), "kno.db"))
	if code != errs.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(stderr, "kno.baseline") || strings.Contains(stderr, "kno.case") {
		t.Errorf("spans were exported without --trace-spans:\n%s", stderr)
	}
}
