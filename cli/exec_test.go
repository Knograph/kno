package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/core/errs"
)

// writeEchoCases writes cases whose input and expected answer are identical,
// so exec:cat scores 1.0 and the run proves the whole pipeline end to end.
func writeEchoCases(t *testing.T, n int) string {
	t.Helper()
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, `{"id":"ecase-%03d","input":"hello %d","expected":"hello %d"}`+"\n",
			i, i, i)
	}
	path := filepath.Join(t.TempDir(), "cases.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("writing cases: %v", err)
	}
	return path
}

// writeCasesWithAnswer writes cases with a fixed expected answer, so a
// fixture whose output depends on its environment can be scored.
func writeCasesWithAnswer(t *testing.T, n int, answer string) string {
	t.Helper()
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, `{"id":"gcase-%03d","input":"q%d","expected":"%s"}`+"\n",
			i, i, answer)
	}
	path := filepath.Join(t.TempDir(), "cases.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("writing cases: %v", err)
	}
	return path
}

// execFixture is the exec adapter's testdata script, relative to the cli
// package directory where the test binary runs.
const execFixture = "../adapters/agent/exec/testdata/"

// TestBaselineRunsExecAgentEndToEnd drives the full pipeline through the
// exec adapter: stdin carries the Case, the answer is judged by the goal,
// and a free command spends nothing.
func TestBaselineRunsExecAgentEndToEnd(t *testing.T) {
	t.Parallel()

	cases := writeEchoCases(t, 20)
	stdout, stderr, code := run(t, "baseline", "--evals", cases,
		"--agent", "exec:cat", "--db", filepath.Join(t.TempDir(), "kno.db"))
	if code != errs.ExitOK {
		t.Fatalf("exit = %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "score      1.000") {
		t.Errorf("the run did not score 1.000 (exec:cat echoes the input):\n%s", stdout)
	}
	if !strings.Contains(stdout, "0 errored") {
		t.Errorf("an exec Case errored:\n%s", stdout)
	}
	if !strings.Contains(stdout, "spent      $0.00") {
		t.Errorf("a free exec run spent money:\n%s", stdout)
	}
}

// TestExecEnvGrantReachesTheChild proves --exec-env plumbing end to end: the
// fixture answers "grant-visible" only when the grant reached its
// environment, so the run's score IS the assertion.
func TestExecEnvGrantReachesTheChild(t *testing.T) {
	t.Parallel()

	cases := writeCasesWithAnswer(t, 10, "grant-visible")
	stdout, stderr, code := run(t, "baseline", "--evals", cases,
		"--agent", "exec:sh "+execFixture+"env-grant-gate.sh",
		"--exec-env", "KNO_CLI_GRANT=visible",
		"--db", filepath.Join(t.TempDir(), "kno.db"))
	if code != errs.ExitOK {
		t.Fatalf("exit = %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "score      1.000") {
		t.Errorf("the grant did not reach the child (score not 1.000):\n%s", stdout)
	}
}

// TestExecCostDeclaredConsentFiresAndRefuses pins the plan's cost contract:
// with --cost-per-call-usd set, Spends() is true, the run is over the $1.00
// confirmation threshold, and the consent path FIRES — declining by default
// until the TUI lands (docs/debt.md#59).
func TestExecCostDeclaredConsentFiresAndRefuses(t *testing.T) {
	t.Parallel()

	cases := writeEchoCases(t, 44)
	stdout, stderr, code := run(t, "baseline", "--evals", cases,
		"--agent", "exec:cat", "--cost-per-call-usd", "0.03",
		"--db", filepath.Join(t.TempDir(), "kno.db"))
	if code == errs.ExitOK {
		t.Fatalf("a costed run proceeded without consent:\n%s", stdout)
	}
	if !strings.Contains(stderr, "nothing was spent") {
		t.Errorf("the consent path did not refuse loudly:\n%s", stderr)
	}
	if !strings.Contains(stderr, "fix:") || !strings.Contains(stderr, "--yes") {
		t.Errorf("the refusal does not point at --yes:\n%s", stderr)
	}
}

// TestExecCostDeclaredRunsWithYesAndEstimates pins the other half: with
// --yes the costed run proceeds, prints the estimate (the non-Estimator
// planning fallback core provides), and settles the declared spend.
func TestExecCostDeclaredRunsWithYesAndEstimates(t *testing.T) {
	t.Parallel()

	cases := writeEchoCases(t, 44)
	stdout, stderr, code := run(t, "baseline", "--evals", cases,
		"--agent", "exec:cat", "--cost-per-call-usd", "0.03", "--yes",
		"--db", filepath.Join(t.TempDir(), "kno.db"))
	if code != errs.ExitOK {
		t.Fatalf("exit = %d\nstderr: %s", code, stderr)
	}
	// 44 cases, ~20% held back, so 34 dev at $0.03 each — the estimate and
	// the settled spend must agree with the declared cost. (The dev count
	// itself is asserted by the "34 call(s)" figure, so a split change is a
	// test change, not a silent drift.)
	if !strings.Contains(stdout, "would spend about $1.02") {
		t.Errorf("no estimate printed for the costed run:\n%s", stdout)
	}
	if !strings.Contains(stdout, "spent      $1.02 over 34 call(s)") {
		t.Errorf("the settled spend does not match the declared cost:\n%s", stdout)
	}
}

// TestExecRefWithBaseURLIsRefusedAtWiring pins the plan's P3-4 refusal:
// exec: has no endpoint, so --base-url with an exec ref must be refused
// naming the flag — before composeRef silently absorbs the URL into the
// command.
func TestExecRefWithBaseURLIsRefusedAtWiring(t *testing.T) {
	t.Parallel()

	cases := writeEchoCases(t, 10)
	_, stderr, code := run(t, "baseline", "--evals", cases,
		"--agent", "exec:cat", "--base-url", "https://x.example/v1",
		"--db", filepath.Join(t.TempDir(), "kno.db"))
	if code == errs.ExitOK {
		t.Fatal("exec with --base-url succeeded; it must be refused")
	}
	if !strings.Contains(stderr, "no endpoint") {
		t.Errorf("the refusal does not explain the reason:\n%s", stderr)
	}
	if !strings.Contains(stderr, "fix:") || !strings.Contains(stderr, "--base-url") {
		t.Errorf("the refusal does not name the flag in its fix line:\n%s", stderr)
	}
}
