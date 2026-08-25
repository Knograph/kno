package cli_test

import (
	"fmt"
	"os"
	"testing"

	"go.uber.org/goleak"
)

// providerKeyVars are every environment variable a provider adapter will read
// for a credential.
//
// The list is the union of what the adapters resolve by default plus the
// common compatible-gateway names, because the failure this guards against is
// a test reaching a REAL endpoint, and any one of these is enough for that.
var providerKeyVars = []string{
	"OPENAI_API_KEY",
	"ANTHROPIC_API_KEY",
	"AZURE_OPENAI_API_KEY",
	"GROQ_API_KEY",
	"TOGETHER_API_KEY",
	"FIREWORKS_API_KEY",
	"OPENROUTER_API_KEY",
	"DEEPSEEK_API_KEY",
	"MISTRAL_API_KEY",
	"PERPLEXITY_API_KEY",
	"XAI_API_KEY",
}

// TestMain makes this package's tests hermetic by unsetting every provider
// credential before any of them runs.
//
// This is not hygiene, it is the same rule as prime directive 4. `kno baseline`
// became able to reach a real provider in this milestone, and the CLI tests
// drive the real command with real flags. On any machine that exports
// OPENAI_API_KEY — a developer laptop, a CI runner with secrets attached — a
// test passing `--agent openai:gpt-4.1` resolves that key and bills the user
// for a suite run. Measured before this existed: one subtest took 8.7 seconds
// and made live calls, on a case the author believed was refused before any
// network access.
//
// CLAUDE.md's rule is that live-API tests are opt-in via KNO_LIVE_TESTS=1 and
// never run in PR CI. That rule protected the adapter packages, which gate
// their live tests explicitly; it did not protect this one, which never had a
// live test and acquired the ability to make one by accident.
//
// KNO_LIVE_TESTS=1 leaves the environment alone, so a deliberate live run
// still works.
func TestMain(m *testing.M) {
	// goleak here too, now that this package owns a background goroutine: the
	// span batcher. Nothing else in cli spawns one, and a batcher that
	// outlives its run would keep exporting into a writer that is gone.
	//
	// VerifyTestMain, not a per-test helper — goleak takes a process-global
	// census, so a parallel sibling's goroutines are indistinguishable from a
	// leak. See CONTRIBUTING.md and docs/debt.md#18.
	//
	// It runs m and exits, so the env scrubbing has to happen BEFORE it and it
	// has to be the last statement. A deferred VerifyTestMain would never run:
	// os.Exit does not unwind.
	if os.Getenv("KNO_LIVE_TESTS") != "1" {
		for _, v := range providerKeyVars {
			if err := os.Unsetenv(v); err != nil {
				fmt.Fprintf(os.Stderr, "cli: unsetting %s: %v\n", v, err)
				os.Exit(1)
			}
		}
	}
	goleak.VerifyTestMain(m)
}
