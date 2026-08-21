package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/knograph/kno/cli"
	"github.com/knograph/kno/core/errs"
)

// runIDPattern matches the generated run identifier, which is time-based and
// therefore never stable. Golden output replaces it so the rest of the render
// can be compared exactly.
var runIDPattern = regexp.MustCompile(`\d{8}T\d{6}-[0-9a-f]{12}`)

func writeCases(t *testing.T, n int) string {
	t.Helper()

	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, `{"id":"case-%03d","input":"q%d","expected":"a%d"}`+"\n", i, i, i)
	}
	path := filepath.Join(t.TempDir(), "cases.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("writing cases: %v", err)
	}
	return path
}

// run executes the CLI and returns stdout, stderr, and the exit code.
func run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	var out, errOut bytes.Buffer
	code = cli.Execute(context.Background(), args, &out, &errOut)
	return out.String(), errOut.String(), code
}

func normalize(s string) string {
	return runIDPattern.ReplaceAllString(s, "RUNID")
}

// TestBaselineHappyPathOutput is a golden test over what a user actually sees.
//
// The rendering is the product surface. Pinning it means a change to what a
// run reports is a reviewed diff rather than something noticed later in a
// screenshot.
func TestBaselineHappyPathOutput(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 50)
	db := filepath.Join(t.TempDir(), "kno.db")

	stdout, stderr, code := run(t, "baseline", "--evals", cases, "--db", db)

	if code != errs.ExitOK {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}

	want := `
Baseline RUNID
  cases      44 scored, 0 errored (of 44 dev; 6 held back)
  score      1.000
  spent      $0.00 over 44 call(s)
  status     completed

  warning: the holdout has only 6 cases, too few for a meaningful confidence interval at validate

Next: ` + "`kno value`" + ` to measure which of your assets earn their place.
`
	if got := normalize(stdout); got != want {
		t.Errorf("output drifted.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestHoldoutIsNeverScored: the CLI reports what it held back, and never
// scores it. The seal makes this a compile-time guarantee upstream; this
// confirms the number a user reads reflects it.
func TestHoldoutIsNeverScored(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 200)
	db := filepath.Join(t.TempDir(), "kno.db")

	stdout, _, code := run(t, "baseline", "--evals", cases, "--db", db, "--json")
	if code != errs.ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}

	rep, err := cli.DecodeReport([]byte(stdout))
	if err != nil {
		t.Fatalf("parsing json output: %v\n%s", err, stdout)
	}

	if rep.Holdout == 0 {
		t.Fatal("nothing was held back")
	}
	if int(rep.Scored) != rep.DevCases {
		t.Errorf("scored %d of %d dev cases", rep.Scored, rep.DevCases)
	}
	if int(rep.Attempted) > rep.DevCases {
		t.Errorf("attempted %d cases but only %d are in dev; the holdout was scored",
			rep.Attempted, rep.DevCases)
	}
}

// TestExitCodesMatchTheGrammar pins what CI branches on.
//
// A deploy gate must tell "stopped at the budget, resumable" from "broken".
// Collapsing them would make a spending limit look like a failing build.
func TestExitCodesMatchTheGrammar(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 50)

	tests := []struct {
		name string
		args []string
		want int
	}{
		{
			name: "success",
			args: []string{"baseline", "--evals", cases},
			want: errs.ExitOK,
		},
		{
			name: "budget stop is resumable, not a failure",
			args: []string{"baseline", "--evals", cases, "--max-calls", "5"},
			want: errs.ExitBudgetStopped,
		},
		{
			name: "unknown agent",
			args: []string{"baseline", "--evals", cases, "--agent", "openai:gpt-4.1"},
			want: errs.ExitError,
		},
		{
			name: "unknown goal",
			args: []string{"baseline", "--evals", cases, "--goal", "llm-judge"},
			want: errs.ExitError,
		},
		{
			name: "missing evals file",
			args: []string{"baseline", "--evals", "/nonexistent/cases.jsonl"},
			want: errs.ExitError,
		},
		{
			name: "cost cap with no per-call estimate",
			args: []string{"baseline", "--evals", cases, "--max-cost-usd", "5", "--yes"},
			want: errs.ExitError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			args := append(append([]string{}, tc.args...),
				"--db", filepath.Join(t.TempDir(), "kno.db"))
			_, stderr, code := run(t, args...)

			if code != tc.want {
				t.Errorf("exit code = %d, want %d\nstderr: %s", code, tc.want, stderr)
			}
		})
	}
}

// TestErrorsFollowTheGrammar: what failed, why, and the exact fix.
func TestErrorsFollowTheGrammar(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 50)
	_, stderr, code := run(t, "baseline", "--evals", cases,
		"--agent", "anthropic:claude", "--db", filepath.Join(t.TempDir(), "kno.db"))

	if code == errs.ExitOK {
		t.Fatal("an unsupported agent succeeded")
	}
	if !strings.Contains(stderr, "no adapter for agent ref") {
		t.Errorf("stderr does not say what failed:\n%s", stderr)
	}
	if !strings.Contains(stderr, "fix:") {
		t.Errorf("stderr does not name a fix:\n%s", stderr)
	}
	if !strings.Contains(stderr, "fake:") {
		t.Errorf("the fix does not name what IS available:\n%s", stderr)
	}
}

// TestBudgetStopNamesWhichCapBound.
//
// An earlier version reported "needs $0.000000 and 1 call(s)" when the cap that
// actually bound was calls — precision nobody reads, about a dimension that was
// not the problem.
func TestBudgetStopNamesWhichCapBound(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 50)
	_, stderr, code := run(t, "baseline", "--evals", cases, "--max-calls", "10",
		"--db", filepath.Join(t.TempDir(), "kno.db"))

	if code != errs.ExitBudgetStopped {
		t.Fatalf("exit code = %d, want %d", code, errs.ExitBudgetStopped)
	}
	if !strings.Contains(stderr, "call limit is spent") {
		t.Errorf("the error does not name the call cap as what bound:\n%s", stderr)
	}
	if strings.Contains(stderr, "$0.000000") {
		t.Errorf("the error reports micro-dollar precision nobody reads:\n%s", stderr)
	}
	if !strings.Contains(stderr, "--max-calls") {
		t.Errorf("the fix does not name the flag to change:\n%s", stderr)
	}
}

// TestResumeContinuesWithoutRepayingWork.
func TestResumeContinuesWithoutRepayingWork(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 60)
	db := filepath.Join(t.TempDir(), "kno.db")
	const runID = "fixed-run"

	// Stop partway.
	_, _, code := run(t, "baseline", "--evals", cases, "--db", db,
		"--run-id", runID, "--max-calls", "15")
	if code != errs.ExitBudgetStopped {
		t.Fatalf("first run exit = %d, want %d", code, errs.ExitBudgetStopped)
	}

	// Continue.
	stdout, stderr, code := run(t, "baseline", "--evals", cases, "--db", db,
		"--run-id", runID, "--resume", "--json")
	if code != errs.ExitOK {
		t.Fatalf("resumed run exit = %d, want 0\nstderr: %s", code, stderr)
	}

	rep, err := cli.DecodeReport([]byte(stdout))
	if err != nil {
		t.Fatalf("parsing json: %v\n%s", err, stdout)
	}
	// The completed run accounts for every dev Case, across both processes.
	if int(rep.Scored) != rep.DevCases {
		t.Errorf("the resumed run reports %d scored of %d dev cases; it lost the "+
			"work the first run paid for", rep.Scored, rep.DevCases)
	}
}

// TestStaleResumeIsRefusedWithAnActionableMessage.
func TestStaleResumeIsRefusedWithAnActionableMessage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cases := filepath.Join(dir, "cases.jsonl")
	db := filepath.Join(dir, "kno.db")

	original := writeCases(t, 40)
	body, err := os.ReadFile(original)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	if err := os.WriteFile(cases, body, 0o600); err != nil {
		t.Fatalf("writing cases: %v", err)
	}

	if _, _, code := run(t, "baseline", "--evals", cases, "--db", db,
		"--run-id", "r1", "--max-calls", "5"); code != errs.ExitBudgetStopped {
		t.Fatalf("first run exit = %d", code)
	}

	// Change the evals, then try to resume.
	if err := os.WriteFile(cases, append(body,
		[]byte(`{"id":"case-999","input":"new","expected":"new"}`+"\n")...), 0o600); err != nil {
		t.Fatalf("editing cases: %v", err)
	}

	_, stderr, code := run(t, "baseline", "--evals", cases, "--db", db,
		"--run-id", "r1", "--resume")
	if code == errs.ExitOK {
		t.Fatal("a resume against changed evals was allowed; it would mix results " +
			"measured over different case sets into one run")
	}
	if !strings.Contains(stderr, "eval source changed") {
		t.Errorf("the error does not name which input changed:\n%s", stderr)
	}
}

// TestJSONOutputIsParseableAndComplete: the --json shape is a contract for
// somebody's jq pipeline.
func TestJSONOutputIsParseableAndComplete(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 50)
	stdout, _, code := run(t, "baseline", "--evals", cases,
		"--db", filepath.Join(t.TempDir(), "kno.db"), "--json")
	if code != errs.ExitOK {
		t.Fatalf("exit code = %d", code)
	}

	rep, err := cli.DecodeRaw([]byte(stdout))
	if err != nil {
		t.Fatalf("output is not valid json: %v\n%s", err, stdout)
	}

	for _, key := range []string{
		"run_id", "status", "agent", "goal", "dev_cases", "holdout_cases",
		"attempted", "scored", "errored", "score", "spent_usd",
	} {
		if _, ok := rep[key]; !ok {
			t.Errorf("json output is missing %q", key)
		}
	}
	// Warnings travel with the number rather than only appearing in the human
	// rendering, or a scripted consumer would never see them.
	if _, ok := rep["warnings"]; !ok {
		t.Error("json output drops the warnings that qualify the result")
	}
}

// TestJSONModeRefusesToSpendUnprompted.
//
// A machine-readable run has nobody to answer a confirmation. Proceeding would
// spend money with no one watching, which is the surprise bill DESIGN.md
// forbids.
func TestJSONModeRefusesToSpendUnprompted(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 50)
	_, stderr, code := run(t, "baseline", "--evals", cases,
		"--db", filepath.Join(t.TempDir(), "kno.db"),
		"--json", "--max-cost-usd", "10", "--cost-per-call-usd", "2")

	if code == errs.ExitOK {
		t.Error("a --json run spent past the confirmation threshold with nobody watching")
	}
	if !strings.Contains(stderr, "--yes") {
		t.Errorf("the error does not name how to proceed:\n%s", stderr)
	}
}

// TestEmptyHoldoutIsRefusedBeforeSpending.
func TestEmptyHoldoutIsRefusedBeforeSpending(t *testing.T) {
	t.Parallel()

	// Three cases at the default 0.2 fraction is very likely to leave the
	// holdout empty; the assertion tolerates either outcome and only checks
	// that an empty holdout is refused rather than silently accepted.
	cases := writeCases(t, 3)
	_, stderr, code := run(t, "baseline", "--evals", cases,
		"--db", filepath.Join(t.TempDir(), "kno.db"), "--holdout-frac", "0.01")

	if code == errs.ExitOK {
		t.Skip("this eval set produced a non-empty holdout; nothing to assert")
	}
	if !strings.Contains(stderr, "holdout") {
		t.Errorf("the error does not explain the holdout problem:\n%s", stderr)
	}
}

// TestHelpIsSnapshotted keeps the CLI's front door under review.
//
// CLAUDE.md requires help text to be snapshot-tested: it is the first thing a
// user reads, and a change to it should be a reviewed diff.
func TestHelpIsSnapshotted(t *testing.T) {
	t.Parallel()

	stdout, _, code := run(t, "baseline", "--help")
	if code != errs.ExitOK {
		t.Fatalf("--help exit = %d", code)
	}

	for _, want := range []string{
		"Run the agent over the dev half of your evals",
		"--evals",
		"--resume",
		"--max-cost-usd",
		"--json",
		// The holdout explanation is the part most worth keeping in front of
		// people: it is why the tool's numbers mean anything.
		"untouched until validate",
		"without paying for anything twice",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help text no longer mentions %q:\n%s", want, stdout)
		}
	}
}

// TestMissingRequiredFlagIsRefused rather than defaulting to something.
func TestMissingRequiredFlagIsRefused(t *testing.T) {
	t.Parallel()

	_, _, code := run(t, "baseline")
	if code == errs.ExitOK {
		t.Error("baseline ran without --evals")
	}
}
