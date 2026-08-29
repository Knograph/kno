package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/core/errs"
)

// withTTYSeam runs fn with the stdin-is-a-terminal seam forced to report
// want, and restores it afterwards — the seam is a package variable so the
// interactive review loop can be driven with a pipe in tests, which is the
// only way its guard is testable at all.
func withTTYSeam(t *testing.T, want bool, fn func()) {
	t.Helper()
	orig := stdinIsTTY
	stdinIsTTY = func() bool { return want }
	defer func() { stdinIsTTY = orig }()
	fn()
}

// mineExec is a miniature of the cli_test run helper for the internal
// package: Execute with an explicit stdin and captured writers.
func mineExec(t *testing.T, in string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Execute(context.Background(), args, strings.NewReader(in), &out, &errOut)
	return out.String(), errOut.String(), code
}

const mineTranscript = "../adapters/evals/mine/testdata/transcript.jsonl"

// TestMineReviewRefusedWithoutATerminal pins the guard: the review loop
// answers prompts, and a prompt answered by a script is not a review. The
// refusal names the way out — drop --review, or run on a terminal.
func TestMineReviewRefusedWithoutATerminal(t *testing.T) {
	// Not parallel: the TTY seam is a package variable, and parallel tests
	// would race each other's override.
	withTTYSeam(t, false, func() {
		_, stderr, code := mineExec(t, "", "mine", "--logs", mineTranscript, "--format", "jsonl-chat", "--review")
		if code == errs.ExitOK {
			t.Fatal("--review without a terminal succeeded")
		}
		if !strings.Contains(stderr, "run on a terminal") {
			t.Errorf("the refusal does not name the fix:\n%s", stderr)
		}
		if !strings.Contains(stderr, "drop --review") {
			t.Errorf("the refusal does not name the override:\n%s", stderr)
		}
	})
}

// TestMineReviewFlowDrivenByAPipe pins the interactive path end to end: keep
// one case, drop the second, and the output holds one case while the
// manifest records both decisions.
func TestMineReviewFlowDrivenByAPipe(t *testing.T) {
	// Not parallel: the TTY seam is a package variable (see the refusal test).
	withTTYSeam(t, true, func() {
		dir := t.TempDir()
		outPath := filepath.Join(dir, "cases.jsonl")
		_, _, code := mineExec(t, "k\nd\n", "mine", "--logs", mineTranscript,
			"--format", "jsonl-chat", "--review", "--out", outPath)
		if code != errs.ExitOK {
			t.Fatalf("mine exit %d", code)
		}
		data, err := os.ReadFile(outPath)
		if err != nil {
			t.Fatalf("reading mined output: %v", err)
		}
		if got := bytes.Count(data, []byte("\n")); got != 1 {
			t.Fatalf("output has %d case lines, want 1 (one kept, one dropped)", got)
		}
		manifest, err := os.ReadFile(outPath + ".review.json")
		if err != nil {
			t.Fatalf("reading review manifest: %v", err)
		}
		for _, want := range []string{`"decision": "keep"`, `"decision": "drop"`} {
			if !strings.Contains(string(manifest), want) {
				t.Errorf("manifest does not record %s:\n%s", want, manifest)
			}
		}
	})
}

// TestMineMinCasesGatesBeforeWriting pins the ordering: a failing yield
// gate must not leave a partial set behind that a downstream pipeline step
// could pick up as a complete eval.
func TestMineMinCasesGatesBeforeWriting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outPath := filepath.Join(dir, "cases.jsonl")
	_, stderr, code := mineExec(t, "", "mine", "--logs", mineTranscript,
		"--format", "jsonl-chat", "--min-cases", "5", "--out", outPath)
	if code == errs.ExitOK {
		t.Fatal("a sub-threshold yield passed the gate")
	}
	if !strings.Contains(stderr, "--min-cases") {
		t.Errorf("the refusal does not name the flag:\n%s", stderr)
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Errorf("the gate wrote output before failing: %v", err)
	}
}

// TestMineMarkdownWithoutAgentNameNamesItsFlag pins the one refusal that
// names a flag in its fix line: markdown transcripts cannot be paired at
// all without knowing which speaker is the agent.
func TestMineMarkdownWithoutAgentNameNamesItsFlag(t *testing.T) {
	t.Parallel()
	_, stderr, code := mineExec(t, "", "mine", "--logs", "../adapters/evals/mine/testdata/transcript.md")
	if code == errs.ExitOK {
		t.Fatal("markdown without --agent-name succeeded")
	}
	if !strings.Contains(stderr, "--agent-name") {
		t.Errorf("the refusal does not name the flag:\n%s", stderr)
	}
}
