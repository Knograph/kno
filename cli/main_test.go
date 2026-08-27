package cli_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// envAllowlist is the COMPLETE environment this package's tests run with, name
// to the reason the name is on it. Everything else is cleared before the first
// test runs.
//
// An allowlist, not a denylist. The previous form named eleven provider
// credential variables and unset them, which protected the suite against
// exactly the providers somebody had thought of: the twelfth adapter, or a
// gateway with its own variable, or a developer who exports a key under a house
// name, all walked straight through. The failure mode of a denylist is silence
// — it keeps passing while it stops protecting — and that is the same shape as
// the bug it was guarding against. See docs/debt.md#63.
//
// The list is measured, not guessed: the suite passes with an EMPTY
// environment, so nothing here is load-bearing today. The three are kept
// because they are process infrastructure rather than configuration — a test
// that later writes a temp file or execs a binary should fail for its own
// reasons and not because the scrub took the machine apart underneath it — and
// because no provider credential can hide in any of them.
//
// Adding an entry is a deliberate act with a test attached:
// TestTheAllowlistCannotCarryACredential refuses a name that could hold one,
// and every entry needs a written reason.
var envAllowlist = map[string]string{
	"PATH":   "finding a binary, for any test that execs one",
	"HOME":   "os.UserHomeDir; see below before config files land",
	"TMPDIR": "t.TempDir and the SQLite store's scratch files",
}

// DEBT(docs/debt.md#62): HOME is passed through from the machine, which is
// right while the CLI reads no config file and wrong the moment it does — a
// developer's real ~/.config/kno/kno.yaml reaching a test is the same class of
// leak as their real API key. The PR that adds config-file loading must point
// HOME at an empty directory instead of dropping it, so os.UserHomeDir keeps
// working and resolves somewhere hermetic.

// The canary is planted by TestMain and must not survive the scrub. It makes
// the guard self-verifying: delete the scrub, or narrow it back into a
// denylist that does not name this variable, and two tests fail rather than
// none. Its value is deliberately not key-shaped so the repo's secret scan has
// nothing to find.
const (
	envCanary      = "KNO_TEST_CANARY_API_KEY"
	envCanaryValue = "planted-by-TestMain-and-must-not-survive"
)

// TestMain makes this package's tests hermetic by replacing the process
// environment with envAllowlist before any of them runs.
//
// This is not hygiene, it is prime directive 4. `kno baseline` became able to
// reach a real provider in M2, and the CLI tests drive the real command with
// real flags. On any machine that exports OPENAI_API_KEY — a developer laptop,
// a CI runner with secrets attached — a test passing `--agent openai:gpt-4.1`
// resolves that key and bills the user for a suite run. Measured before any
// guard existed: one subtest took 8.7 seconds and made live calls, on a case
// the author believed was refused before any network access.
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
		if err := os.Setenv(envCanary, envCanaryValue); err != nil {
			fmt.Fprintf(os.Stderr, "cli: planting the canary: %v\n", err)
			os.Exit(1)
		}
		scrubEnvironment()
	}
	goleak.VerifyTestMain(m)
}

// scrubEnvironment keeps the allowlisted variables and clears everything else.
//
// Read-then-clear-then-restore rather than unsetting the complement: the
// complement is computed from os.Environ(), and iterating a list while deleting
// from the thing it describes is how you leave one behind.
func scrubEnvironment() {
	keep := make(map[string]string, len(envAllowlist))
	for name := range envAllowlist {
		if v, ok := os.LookupEnv(name); ok {
			keep[name] = v
		}
	}
	os.Clearenv()
	for name, v := range keep {
		if err := os.Setenv(name, v); err != nil {
			fmt.Fprintf(os.Stderr, "cli: restoring %s: %v\n", name, err)
			os.Exit(1)
		}
	}
}

// TestOnlyAllowlistedVariablesReachTheTests is the guard.
//
// It asserts set membership, which is stronger than asserting that no
// credential-SHAPED name survived. A shape test has to describe what a
// credential variable looks like, and the whole lesson of docs/debt.md#63 is
// that the description is always one provider short. It is also unable to
// express this list: `--key-env host=VAR` accepts any variable name, so PATH is
// as bindable as OPENAI_API_KEY, and a shape test would either fail on PATH or
// carry an exemption list — a denylist again, wearing a different hat.
//
// Membership needs no description of the enemy. A variable that reaches an
// adapter is a variable on this list, and the list is three names with reasons.
func TestOnlyAllowlistedVariablesReachTheTests(t *testing.T) {
	t.Parallel()
	requireScrubbed(t)

	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if _, ok := envAllowlist[name]; !ok {
			// The NAME is safe to print; the value is why we are here.
			t.Errorf("%s survived into the test environment and is not on the "+
				"allowlist. Anything a test can read, `--key-env host=%s` can "+
				"send to a provider. Add it to envAllowlist with a reason, or "+
				"find out who is exporting it", name, name)
		}
		if value == envCanaryValue {
			t.Errorf("%s carries the canary value: the scrub ran and something "+
				"put it back", name)
		}
	}
	if got := os.Getenv(envCanary); got != "" {
		t.Errorf("%s survived the scrub, so the scrub is not scrubbing", envCanary)
	}
}

// TestTheAllowlistCannotCarryACredential is what makes the list hard to widen.
//
// The membership test above passes the moment a variable is added to
// envAllowlist, which would make "the test failed, so I allowlisted it" a
// two-line fix for the exact exposure this exists to prevent. So the list
// itself is checked: a name that could plausibly hold a secret is refused, and
// every entry must say why it is there.
func TestTheAllowlistCannotCarryACredential(t *testing.T) {
	t.Parallel()

	// Substrings, matched case-insensitively, that make a variable a plausible
	// home for a credential. Over-broad on purpose: a false positive costs an
	// argument in review, and a false negative costs the user money.
	secretish := []string{
		"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "CRED",
		"AUTH", "BEARER", "SESSION", "COOKIE", "PRIVATE", "SIGNATURE", "APIKEY",
	}

	for name, reason := range envAllowlist {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s is allowlisted with no reason; an entry nobody had to "+
				"justify is how the list grows back into an environment", name)
		}
		upper := strings.ToUpper(name)
		for _, s := range secretish {
			if strings.Contains(upper, s) {
				t.Errorf("%s is on the allowlist and its name contains %q. A "+
					"variable that could hold a credential does not go on the "+
					"list; if this one genuinely cannot, it needs a different "+
					"name or this test needs a reviewed exception", name, s)
			}
		}
	}
}

// requireScrubbed skips a guard when the scrub was deliberately not run.
//
// KNO_LIVE_TESTS=1 is the documented opt-in for a live run, and under it the
// environment is meant to be intact — so asserting it was emptied would fail
// the one configuration in which the full environment is correct.
func requireScrubbed(t *testing.T) {
	t.Helper()
	if os.Getenv("KNO_LIVE_TESTS") == "1" {
		t.Skip("KNO_LIVE_TESTS=1: the environment is deliberately left alone")
	}
}
