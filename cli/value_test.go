package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/core/errs"
)

// writePool writes a small asset pool the value command can measure.
func writePool(t *testing.T, n int) string {
	t.Helper()
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, `{"id":"asset-%03d","content":"knowledge %d"}`+"\n", i, i)
	}
	path := filepath.Join(t.TempDir(), "assets.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("writing pool: %v", err)
	}
	return path
}

// TestValueHelpIsSnapshotted keeps the new command's front door under the
// same review discipline as baseline's.
func TestValueHelpIsSnapshotted(t *testing.T) {
	t.Parallel()

	stdout, _, code := run(t, "value", "--help")
	if code != errs.ExitOK {
		t.Fatalf("--help exit = %d", code)
	}
	for _, want := range []string{
		"Measure every asset in the pool against the recorded baseline",
		"--evals",
		"--pool",
		"--baseline-run-id",
		"--resume",
		"without paying for anything twice",
		"The control arm never carries the asset",
		// The pool grammars, pinned here: a bare path is JSONL, the prefixes
		// are the other adapters, and --split-sections is the markdown knob.
		"csv:<file>",
		"md:<file-or-dir>",
		"--split-sections",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help text no longer mentions %q:\n%s", want, stdout)
		}
	}
}

// TestValueRefusesMissingRequirements: all three required inputs are refused
// rather than defaulted.
func TestValueRefusesMissingRequirements(t *testing.T) {
	t.Parallel()

	_, _, code := run(t, "value")
	if code == errs.ExitOK {
		t.Error("value ran without --evals, --pool, or --baseline-run-id")
	}
}

// writeCSVPool writes an asset pool the value command can measure, in the
// csv: grammar.
func writeCSVPool(t *testing.T, n int) string {
	t.Helper()

	var b strings.Builder
	b.WriteString("id,content\n")
	for i := range n {
		fmt.Fprintf(&b, "asset-%03d,knowledge %d\n", i, i)
	}
	path := filepath.Join(t.TempDir(), "assets.csv")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("writing pool: %v", err)
	}
	return path
}

// writeMDPool writes an asset pool the value command can measure, in the
// md: grammar: one .md file per asset in a directory.
func writeMDPool(t *testing.T, n int) (dir string, ids []string) {
	t.Helper()

	dir = t.TempDir()
	for i := range n {
		name := fmt.Sprintf("asset-%03d.md", i)
		body := fmt.Sprintf("# Asset %d\n\nknowledge %d\n", i, i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("writing pool: %v", err)
		}
		ids = append(ids, filepath.Join(dir, name))
	}
	return dir, ids
}

// TestValueCSVAndMarkdownPoolsEndToEnd: the csv: and md: grammars carry a
// whole value run the same way a bare JSONL path does — baseline, then
// value, and the report shows one delta row per asset, keyed by the id each
// adapter produced.
func TestValueCSVAndMarkdownPoolsEndToEnd(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		pool   func(t *testing.T, n int) (string, []string)
		prefix string
	}{
		{"csv", func(t *testing.T, n int) (string, []string) {
			path := writeCSVPool(t, n)
			ids := make([]string, n)
			for i := range n {
				ids[i] = fmt.Sprintf("asset-%03d", i)
			}
			return "csv:" + path, ids
		}, "csv:"},
		{"markdown", func(t *testing.T, n int) (string, []string) {
			dir, ids := writeMDPool(t, n)
			return "md:" + dir, ids
		}, "md:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			casesPath := writeCases(t, 30)
			poolRef, ids := tc.pool(t, 2)
			dbPath := filepath.Join(t.TempDir(), "kno.db")

			baseOut, baseErr, baseCode := run(
				t, "baseline", "--evals", casesPath, "--agent", "fake:", "--db", dbPath, "--yes",
			)
			if baseCode != errs.ExitOK {
				t.Fatalf("baseline exit = %d\nstdout: %s\nstderr: %s", baseCode, baseOut, baseErr)
			}
			var baseRunID string
			for _, line := range strings.Split(baseOut, "\n") {
				if after, ok := strings.CutPrefix(line, "Baseline "); ok {
					baseRunID = strings.TrimSpace(after)
				}
			}
			if baseRunID == "" {
				t.Fatalf("could not read the baseline run ID from:\n%s", baseOut)
			}

			stdout, stderr, code := run(
				t, "value",
				"--evals", casesPath,
				"--pool", poolRef,
				"--baseline-run-id", baseRunID,
				"--agent", "fake:",
				"--db", dbPath,
				"--yes",
			)
			if code != errs.ExitOK {
				t.Fatalf("value exit = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
			}
			for _, id := range ids {
				if !strings.Contains(stdout, id) {
					t.Errorf("report missing the %s pool's asset row %q:\n%s", tc.prefix, id, stdout)
				}
			}
			if !strings.Contains(stdout, "DELTA") {
				t.Errorf("report missing the delta column:\n%s", stdout)
			}
		})
	}
}

// TestValueRefusesSplitSectionsOnNonMarkdownPool: --split-sections with a
// pool it cannot affect is refused at wiring, before anything is read.
func TestValueRefusesSplitSectionsOnNonMarkdownPool(t *testing.T) {
	t.Parallel()

	casesPath := writeCases(t, 20)
	poolPath := writeCSVPool(t, 1)

	_, stderr, code := run(
		t, "value",
		"--evals", casesPath,
		"--pool", "csv:"+poolPath,
		"--baseline-run-id", "any",
		"--agent", "fake:",
		"--split-sections",
	)
	if code == errs.ExitOK {
		t.Fatal("--split-sections with a csv: pool was accepted")
	}
	if !strings.Contains(stderr, "md:") {
		t.Errorf("stderr does not name the only grammar --split-sections applies to:\n%s", stderr)
	}
}

// TestValueRunsEndToEnd over a fake agent: baseline, then value, and the
// report shows one delta row per asset.
func TestValueRunsEndToEnd(t *testing.T) {
	t.Parallel()

	casesPath := writeCases(t, 30)
	poolPath := writePool(t, 2)
	dbPath := filepath.Join(t.TempDir(), "kno.db")

	baseOut, baseErr, baseCode := run(
		t,
		"baseline",
		"--evals", casesPath,
		"--agent", "fake:",
		"--db", dbPath,
		"--yes",
	)
	if baseCode != errs.ExitOK {
		t.Fatalf("baseline exit = %d\nstdout: %s\nstderr: %s", baseCode, baseOut, baseErr)
	}
	// The human report opens with "Baseline <run-id>"; the --yes estimate
	// line comes first, so the ID is found by prefix rather than position.
	var baseRunID string
	for _, line := range strings.Split(baseOut, "\n") {
		if after, ok := strings.CutPrefix(line, "Baseline "); ok {
			baseRunID = strings.TrimSpace(after)
		}
	}
	if baseRunID == "" {
		t.Fatalf("could not read the baseline run ID from:\n%s", baseOut)
	}

	stdout, stderr, code := run(
		t,
		"value",
		"--evals", casesPath,
		"--pool", poolPath,
		"--baseline-run-id", baseRunID,
		"--agent", "fake:",
		"--db", dbPath,
		"--yes",
	)
	if code != errs.ExitOK {
		t.Fatalf("value exit = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "asset-000") || !strings.Contains(stdout, "asset-001") {
		t.Errorf("report missing asset rows:\n%s", stdout)
	}
	if !strings.Contains(stdout, "DELTA") {
		t.Errorf("report missing the delta column:\n%s", stdout)
	}
}

// TestValueRoutingFlagsAreRefused: every knob that would invert the schedule
// is refused with the fix line before anything is read.
func TestValueRoutingFlagsAreRefused(t *testing.T) {
	t.Parallel()

	casesPath := writeCases(t, 20)
	poolPath := writePool(t, 1)

	tests := []struct {
		name string
		args []string
	}{
		{"unknown route", []string{"--route", "everything"}},
		{"negative trials", []string{"--trials", "-1"}},
		{"negative sample rate", []string{"--sample-rate", "-0.5"}},
		{"negative control reserve", []string{"--control-reserve", "-0.1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{
				"value",
				"--evals", casesPath,
				"--pool", poolPath,
				"--baseline-run-id", "any",
				"--agent", "fake:",
			}, tc.args...)
			_, _, code := run(t, args...)
			if code == errs.ExitOK {
				t.Errorf("%s was accepted", tc.name)
			}
		})
	}
}

// TestValueRefusesUnreadableInputs: a missing evals file and a missing pool
// file are refused, not defaulted.
func TestValueRefusesUnreadableInputs(t *testing.T) {
	t.Parallel()

	poolPath := writePool(t, 1)
	_, _, code := run(
		t, "value",
		"--evals", filepath.Join(t.TempDir(), "nope.jsonl"),
		"--pool", poolPath,
		"--baseline-run-id", "any",
		"--agent", "fake:",
	)
	if code == errs.ExitOK {
		t.Error("a missing evals file was accepted")
	}

	casesPath := writeCases(t, 20)
	_, _, code = run(
		t, "value",
		"--evals", casesPath,
		"--pool", filepath.Join(t.TempDir(), "nope.jsonl"),
		"--baseline-run-id", "any",
		"--agent", "fake:",
	)
	if code == errs.ExitOK {
		t.Error("a missing pool file was accepted")
	}
}

// TestValueJSONOutput: --json is a machine contract — one document, no prose
// prefix, valuations keyed with the headline numbers.
func TestValueJSONOutput(t *testing.T) {
	t.Parallel()

	casesPath := writeCases(t, 30)
	poolPath := writePool(t, 2)
	dbPath := filepath.Join(t.TempDir(), "kno.db")

	baseOut, _, baseCode := run(t, "baseline",
		"--evals", casesPath, "--agent", "fake:", "--db", dbPath, "--yes")
	if baseCode != errs.ExitOK {
		t.Fatalf("baseline exit = %d\n%s", baseCode, baseOut)
	}
	var baseRunID string
	for _, line := range strings.Split(baseOut, "\n") {
		if after, ok := strings.CutPrefix(line, "Baseline "); ok {
			baseRunID = strings.TrimSpace(after)
		}
	}

	stdout, stderr, code := run(
		t, "value",
		"--evals", casesPath,
		"--pool", poolPath,
		"--baseline-run-id", baseRunID,
		"--agent", "fake:",
		"--db", dbPath,
		"--yes",
		"--json",
	)
	if code != errs.ExitOK {
		t.Fatalf("value exit = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `"valuations"`) || !strings.Contains(stdout, `"asset_id"`) {
		t.Errorf("the JSON report is missing the valuations shape:\n%s", stdout)
	}
	if strings.HasPrefix(strings.TrimSpace(stdout), "Proceeding") {
		t.Errorf("a prose prefix corrupted the JSON contract:\n%s", stdout)
	}
}
