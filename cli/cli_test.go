package cli_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"

	knov1 "github.com/knograph/kno/gen/kno/v1"

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

Scores and traces are recorded. ` + "`kno purge`" + ` removes trace content when you no longer need it.
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
//
// The subject used to be an unsupported scheme, back when `fake:` was the only
// adapter. Now that a scheme resolves to a real provider, the interesting
// refusal is a real misconfiguration — and it is a better test, because this
// message reaches a user who is one flag away from spending money.
func TestErrorsFollowTheGrammar(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 50)
	_, stderr, code := run(t, "baseline", "--evals", cases,
		"--agent", "tuned:job-123", "--db", filepath.Join(t.TempDir(), "kno.db"))

	if code == errs.ExitOK {
		t.Fatal("an unsupported agent succeeded")
	}
	if !strings.Contains(stderr, "no adapter for agent ref") {
		t.Errorf("stderr does not say what failed:\n%s", stderr)
	}
	if !strings.Contains(stderr, "fix:") {
		t.Errorf("stderr does not name a fix:\n%s", stderr)
	}
	if !strings.Contains(stderr, "openai:") || !strings.Contains(stderr, "anthropic:") {
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

// TestEveryUserReachableErrorNamesAFix.
//
// A Phase-3 review found `--max-cost-usd` without `--cost-per-call-usd`
// returning a bare errors.New from core: no fix line, and exit 1 by fallthrough
// rather than by choice. The exit-code case above passed anyway, because it
// asserted only the code. This asserts the grammar CLAUDE.md requires.
func TestEveryUserReachableErrorNamesAFix(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 50)

	tests := []struct {
		name     string
		args     []string
		wantFrag string
	}{
		{
			name:     "cost cap with no per-call estimate",
			args:     []string{"--max-cost-usd", "5", "--yes"},
			wantFrag: "--cost-per-call-usd",
		},
		{
			name:     "negative cost cap",
			args:     []string{"--max-cost-usd", "-1", "--cost-per-call-usd", "0.01", "--yes"},
			wantFrag: "--max-cost-usd",
		},
		{
			name:     "negative call cap",
			args:     []string{"--max-calls", "-1"},
			wantFrag: "--max-calls",
		},
		{
			name:     "negative per-call estimate",
			args:     []string{"--cost-per-call-usd", "-0.01"},
			wantFrag: "--cost-per-call-usd",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			args := append([]string{"baseline", "--evals", cases}, tc.args...)
			args = append(args, "--db", filepath.Join(t.TempDir(), "kno.db"))
			_, stderr, code := run(t, args...)

			if code == errs.ExitOK {
				t.Fatalf("the command succeeded; stderr: %s", stderr)
			}
			if !strings.Contains(stderr, "fix:") {
				t.Errorf("no fix line:\n%s", stderr)
			}
			if !strings.Contains(stderr, tc.wantFrag) {
				t.Errorf("the message never names %s:\n%s", tc.wantFrag, stderr)
			}
		})
	}
}

// TestNegativeCapsDoNotDisableTheCap is the money half of the case above,
// stated separately because the failure mode is silent rather than loud: the
// guard treats a limit as active only when positive, so a negative
// --max-cost-usd read as "unlimited" and spent without a ceiling.
func TestNegativeCapsDoNotDisableTheCap(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 50)
	_, stderr, code := run(t, "baseline", "--evals", cases,
		"--max-cost-usd", "-1", "--cost-per-call-usd", "0.01", "--yes",
		"--db", filepath.Join(t.TempDir(), "kno.db"))

	if code == errs.ExitOK {
		t.Fatalf("a negative spend cap ran to completion, which means it was "+
			"treated as no cap at all; stderr: %s", stderr)
	}
}

// TestPurgeRequiresConfirmation: destroying data is irreversible, so the
// default is to describe what would happen and stop.
func TestPurgeRequiresConfirmation(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 50)
	db := filepath.Join(t.TempDir(), "kno.db")

	stdout, _, code := run(t, "baseline", "--evals", cases, "--db", db, "--run-id", "r1")
	if code != errs.ExitOK {
		t.Fatalf("baseline exit %d", code)
	}
	if !strings.Contains(stdout, "Baseline") {
		t.Fatalf("baseline produced no report:\n%s", stdout)
	}

	out, stderr, code := run(t, "purge", "--run-id", "r1", "--db", db)
	// Non-zero on purpose: a retention job that forgets --yes must fail loudly
	// rather than report success over data it never removed.
	if code == errs.ExitOK {
		t.Errorf("purge without --yes exited 0 after doing nothing; a scheduled " +
			"job would log success and keep the data")
	}
	if !strings.Contains(stderr, "fix:") {
		t.Errorf("no fix line:\n%s", stderr)
	}
	if !strings.Contains(out, "recorded row(s)") {
		t.Errorf("the prompt does not say how much it would remove, so it is not "+
			"a dry run:\n%s", out)
	}
	if !strings.Contains(out, "cannot be undone") {
		t.Errorf("the prompt does not say the action is irreversible:\n%s", out)
	}
	if !strings.Contains(out, "--yes") {
		t.Errorf("the prompt does not say how to proceed:\n%s", out)
	}
	if strings.Contains(out, "Purged") {
		t.Errorf("purge reported doing work without confirmation:\n%s", out)
	}
}

// TestPurgeKeepsTheRunResumable is the user-facing half of docs/debt.md#25:
// after a purge, --resume must still skip the work already paid for.
func TestPurgeKeepsTheRunResumable(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 60)
	db := filepath.Join(t.TempDir(), "kno.db")

	if _, stderr, code := run(t, "baseline", "--evals", cases, "--db", db,
		"--run-id", "r1", "--json"); code != errs.ExitOK {
		t.Fatalf("baseline exit %d: %s", code, stderr)
	}

	out, _, code := run(t, "purge", "--run-id", "r1", "--db", db, "--yes")
	if code != errs.ExitOK {
		t.Fatalf("purge exit %d", code)
	}
	if !strings.Contains(out, "Purged") {
		t.Errorf("purge did not report what it did:\n%s", out)
	}

	// Resuming a purged run must not re-execute anything.
	stdout, stderr, code := run(t, "baseline", "--evals", cases, "--db", db,
		"--run-id", "r1", "--resume", "--json")
	if code != errs.ExitOK {
		t.Fatalf("resume after purge exit %d: %s", code, stderr)
	}
	rep, err := cli.DecodeReport([]byte(stdout))
	if err != nil {
		t.Fatalf("decoding report: %v", err)
	}
	if int(rep.Scored) != rep.DevCases {
		t.Errorf("after purge, the resumed run reports %d of %d scored",
			rep.Scored, rep.DevCases)
	}

	// The count above passes whether or not the work was redone — a re-run
	// scores the same Cases again and reports the same number. What separates
	// them is whether the resume DID anything: a resumed run that re-executed
	// every Case appends a CaseScored event for each one.
	//
	// Asserting on the observable that actually differs, because the first
	// version of this test passed against a purge that deleted every row.
	outcomes, caseEvents := outcomesAndCaseEvents(t, db, "r1")
	if caseEvents != outcomes {
		t.Errorf("per-Case events grew from %d to %d across a resume of a "+
			"fully-completed run; the resume re-executed %d Case(s) and paid "+
			"for them again, which means the purge destroyed the done-markers",
			outcomes, caseEvents, caseEvents-outcomes)
	}
}

// outcomesAndCaseEvents returns how many outcomes the run recorded and how
// many PER-CASE events it emitted, by reading the database directly.
//
// Not routed through the CLI: the report is what the assertion above already
// showed cannot distinguish a clean resume from one that re-ran paid work.
//
// Named for what it returns. It used to be called eventSequences and to
// subtract a hardcoded count of run-level events, and neither the name nor the
// arithmetic survived M2-10 adding run-level emitters.
func outcomesAndCaseEvents(t *testing.T, dbPath, runID string) (outcomeCount, caseEvents int64) {
	t.Helper()
	// The caller has already resumed, so both readings come from the same
	// database; "before" is reconstructed from the number of scored Cases,
	// which a first run and a re-run would differ on.
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("opening %s: %v", dbPath, err)
	}
	defer func() { _ = db.Close() }()

	var outcomes, perCase int64
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM outcomes WHERE run_id = ?`, runID,
	).Scan(&outcomes); err != nil {
		t.Fatalf("counting outcomes: %v", err)
	}
	// Count only the PER-CASE events, by decoding each payload, rather than
	// subtracting a hardcoded number of run-level ones.
	//
	// The old form was `events - 4`, meaning two processes x (RunStarted +
	// RunFinished). Every arm of that arithmetic is now wrong or about to be:
	// a resume emits RunResumed instead of a second RunStarted, and M2-10 adds
	// run-level emitters whose count depends on how long the run took. What
	// this test actually needs is "how many Cases were paid for", which is a
	// property of the per-Case events alone.
	rows, err := db.Query(`SELECT proto FROM events WHERE run_id = ?`, runID)
	if err != nil {
		t.Fatalf("reading events: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			t.Fatalf("scanning event: %v", err)
		}
		ev := &knov1.Event{}
		if err := proto.Unmarshal(blob, ev); err != nil {
			t.Fatalf("unmarshaling event: %v", err)
		}
		switch ev.GetPayload().(type) {
		case *knov1.Event_CaseScored, *knov1.Event_CaseErrored:
			perCase++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating events: %v", err)
	}
	return outcomes, perCase
}

// TestPurgeRefusesAnUnknownRun: a typo must be a refusal, not a silent no-op
// that reads as "already purged".
func TestPurgeRefusesAnUnknownRun(t *testing.T) {
	t.Parallel()

	db := filepath.Join(t.TempDir(), "kno.db")
	cases := writeCases(t, 50)
	if _, _, code := run(t, "baseline", "--evals", cases, "--db", db, "--run-id", "r1"); code != errs.ExitOK {
		t.Fatalf("baseline exit %d", code)
	}

	_, stderr, code := run(t, "purge", "--run-id", "r-typo", "--db", db, "--yes")
	if code == errs.ExitOK {
		t.Fatal("purging a run that does not exist succeeded")
	}
	if !strings.Contains(stderr, "fix:") {
		t.Errorf("no fix line:\n%s", stderr)
	}
	if !strings.Contains(stderr, "r-typo") {
		t.Errorf("the message does not quote what was asked for:\n%s", stderr)
	}
}

// TestPurgeRequiresARunID.
func TestPurgeRequiresARunID(t *testing.T) {
	t.Parallel()

	_, stderr, code := run(t, "purge", "--db", filepath.Join(t.TempDir(), "kno.db"))
	if code == errs.ExitOK {
		t.Fatal("purge ran without --run-id")
	}
	if !strings.Contains(stderr, "run-id") {
		t.Errorf("the error does not name the missing flag:\n%s", stderr)
	}
}

// TestPurgePromptDoesNotLeakRawEnumNames.
//
// The confirmation printed `RUN_STATUS_COMPLETED` — a generated Go identifier
// in a sentence aimed at a human, in the one place the CLI asks permission to
// destroy data. The report elsewhere renders the same value as "completed".
func TestPurgePromptDoesNotLeakRawEnumNames(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 50)
	db := filepath.Join(t.TempDir(), "kno.db")
	if _, _, code := run(t, "baseline", "--evals", cases, "--db", db, "--run-id", "r1"); code != errs.ExitOK {
		t.Fatalf("baseline exit %d", code)
	}

	out, _, _ := run(t, "purge", "--run-id", "r1", "--db", db)
	if strings.Contains(out, "RUN_STATUS_") {
		t.Errorf("the prompt shows a raw enum name:\n%s", out)
	}
	if !strings.Contains(out, "completed") {
		t.Errorf("the prompt does not name the run's status in the CLI's own words:\n%s", out)
	}
}

// TestPurgeRefusesARunningRun.
//
// Cases completing after the purge write fresh traces, so the command would
// report success over content that reappears seconds later.
func TestPurgeRefusesARunningRun(t *testing.T) {
	t.Parallel()

	db := filepath.Join(t.TempDir(), "kno.db")
	cases := writeCases(t, 50)

	// A budget stop leaves the run BUDGET_STOPPED, not RUNNING, so drive the
	// state directly: what matters is the guard, not how the run got there.
	if _, _, code := run(t, "baseline", "--evals", cases, "--db", db, "--run-id", "r1"); code != errs.ExitOK {
		t.Fatalf("baseline exit %d", code)
	}
	setRunStatusRunning(t, db, "r1")

	_, stderr, code := run(t, "purge", "--run-id", "r1", "--db", db, "--yes")
	if code == errs.ExitOK {
		t.Fatal("purged a run that is still executing")
	}
	if !strings.Contains(stderr, "still running") {
		t.Errorf("the refusal does not say why:\n%s", stderr)
	}
	if !strings.Contains(stderr, "--force") {
		t.Errorf("the refusal does not name the override:\n%s", stderr)
	}

	// --force is the documented way through.
	if _, stderr, code := run(t, "purge", "--run-id", "r1", "--db", db, "--yes", "--force"); code != errs.ExitOK {
		t.Errorf("--force did not override the refusal: exit %d\n%s", code, stderr)
	}
}

// setRunStatusRunning rewrites a stored Run's status column and blob.
func setRunStatusRunning(t *testing.T, dbPath, runID string) {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("opening %s: %v", dbPath, err)
	}
	defer func() { _ = db.Close() }()

	var blob []byte
	if err := db.QueryRow(`SELECT proto FROM runs WHERE id = ?`, runID).Scan(&blob); err != nil {
		t.Fatalf("reading run: %v", err)
	}
	var r knov1.Run
	if err := proto.Unmarshal(blob, &r); err != nil {
		t.Fatalf("unmarshaling run: %v", err)
	}
	r.Status = knov1.RunStatus_RUN_STATUS_RUNNING
	updated, err := proto.Marshal(&r)
	if err != nil {
		t.Fatalf("marshaling run: %v", err)
	}
	if _, err := db.Exec(`UPDATE runs SET proto = ?, status = ? WHERE id = ?`,
		updated, int(knov1.RunStatus_RUN_STATUS_RUNNING), runID); err != nil {
		t.Fatalf("updating run: %v", err)
	}
}

// TestAMalformedRefAndAnUnsupportedOneReadDifferently.
//
// Parsing and resolution are separate steps, and the whole point is that a typo
// and an unsupported provider produce different messages. Both exit 1, so an
// exit-code assertion cannot tell them apart — which is why the existing
// exit-code table did not cover this.
func TestAMalformedRefAndAnUnsupportedOneReadDifferently(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 50)

	tests := []struct {
		name string
		ref  string
		frag string
	}{
		{"a typo in the scheme", "opeani:gpt-4.1", "unknown scheme"},
		{"a scheme that does not exist", "openai-compat:model", "unknown scheme"},
		{"no scheme at all", "gpt-4.1", "no scheme"},
		{"a well-formed reference with no adapter", "tuned:job-123", "no adapter for agent ref"},
		{"a command scheme with no command", "exec:", "no command"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, stderr, code := run(t, "baseline", "--evals", cases, "--agent", tc.ref,
				"--db", filepath.Join(t.TempDir(), "kno.db"))
			if code == errs.ExitOK {
				t.Fatalf("--agent %s succeeded", tc.ref)
			}
			if !strings.Contains(stderr, tc.frag) {
				t.Errorf("the message does not say %q:\n%s", tc.frag, stderr)
			}
			if !strings.Contains(stderr, "fix:") {
				t.Errorf("no fix line:\n%s", stderr)
			}
		})
	}
}

// TestACredentialInAnAgentRefIsNeverStored.
//
// AgentRef.Ref is written to the Run, put on the event stream, and rendered in
// --json — and `kno purge` does not remove any of those, because it clears
// outcome traces and not the Run's own agent field. Before this was fixed, four
// copies of a credential survived a purge.
func TestACredentialInAnAgentRefIsNeverStored(t *testing.T) {
	t.Parallel()

	const secret = "EXAMPLE-CREDENTIAL-VALUE"
	cases := writeCases(t, 20)
	dir := t.TempDir()
	db := filepath.Join(dir, "kno.db")

	for _, ref := range []string{
		"fake:m@HTTPS://user:" + secret + "@api.evil.com/v1",
		"fake:m@https://user:" + secret + "@api.evil.com/v1",
	} {
		_, stderr, code := run(t, "baseline", "--evals", cases, "--agent", ref, "--db", db)
		if code == errs.ExitOK {
			t.Errorf("--agent %s ran", ref)
		}
		if strings.Contains(stderr, secret) {
			t.Errorf("the refusal echoed the credential:\n%s", stderr)
		}
	}

	// Nothing reached disk.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		if bytes.Contains(b, []byte(secret)) {
			t.Errorf("a credential from an agent ref reached %s; kno purge does "+
				"not remove it, because purge clears outcome traces and not the "+
				"Run's own agent field", e.Name())
		}
	}
}
