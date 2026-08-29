package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/cli"
	"github.com/knograph/kno/core/errs"
)

// TestSelectHelpIsSnapshotted keeps the new command's front door under the
// same review discipline as baseline's and value's.
func TestSelectHelpIsSnapshotted(t *testing.T) {
	t.Parallel()

	stdout, _, code := run(t, "select", "--help")
	if code != errs.ExitOK {
		t.Fatalf("--help exit = %d", code)
	}
	for _, want := range []string{
		"Rank the recorded Valuations of a Value run on delta per dollar",
		"--value-run-id",
		"--max-context-tokens",
		"--max-training-examples",
		"--max-cost-usd",
		"--allow-partial",
		"--pool",
		"Bonferroni-corrected",
		"never touches the holdout",
		// The greedy construction is labeled, not hidden: the plan's A6
		// requires the honesty to be in the surface a user reads.
		"no approximation guarantee",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help text no longer mentions %q:\n%s", want, stdout)
		}
	}
}

// TestSelectRefusesMissingRequirements: the required input is refused rather
// than defaulted, and a budget with no cap at all is not a budget.
func TestSelectRefusesMissingRequirements(t *testing.T) {
	t.Parallel()

	_, _, code := run(t, "select")
	if code == errs.ExitOK {
		t.Error("select ran without --value-run-id")
	}

	// A fake run ID reaches the budget check before the store does, so the
	// no-cap refusal is observable without a real Value run.
	_, stderr, code := run(t, "select", "--value-run-id", "nope")
	if code == errs.ExitOK {
		t.Error("select ran with no budget cap")
	}
	if !strings.Contains(stderr, "pass at least one budget cap") {
		t.Errorf("no-cap refusal missing its fix:\n%s", stderr)
	}
}

// TestSelectAndExportEndToEnd drives the whole loop on a fake agent and
// asserts the two new commands' reports, the export's overwrite refusal, and
// the byte-identical re-export that the plan pins as a golden.
func TestSelectAndExportEndToEnd(t *testing.T) {
	t.Parallel()

	casesPath := writeCases(t, 30)
	poolPath := writePool(t, 2)
	dbPath := filepath.Join(t.TempDir(), "kno.db")
	outPath := filepath.Join(t.TempDir(), "pack.md")

	baseOut, baseErr, baseCode := run(
		t, "baseline", "--evals", casesPath, "--agent", "fake:", "--db", dbPath, "--yes",
	)
	if baseCode != errs.ExitOK {
		t.Fatalf("baseline exit = %d\nstderr: %s", baseCode, baseErr)
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

	valueOut, valueErr, valueCode := run(
		t, "value", "--evals", casesPath, "--pool", poolPath,
		"--baseline-run-id", baseRunID, "--agent", "fake:", "--db", dbPath, "--yes",
	)
	if valueCode != errs.ExitOK {
		t.Fatalf("value exit = %d\nstdout: %s\nstderr: %s", valueCode, valueOut, valueErr)
	}
	valueRunID := ""
	for _, line := range strings.Split(valueOut, "\n") {
		if after, ok := strings.CutPrefix(line, "Value run "); ok {
			valueRunID = strings.TrimSpace(strings.Split(after, " (")[0])
			break
		}
	}
	if valueRunID == "" {
		t.Fatalf("could not read the value run ID from:\n%s", valueOut)
	}

	selectOut, selectErr, selectCode := run(
		t, "select", "--value-run-id", valueRunID, "--pool", poolPath,
		"--max-context-tokens", "10000", "--db", dbPath,
	)
	if selectCode != errs.ExitOK {
		t.Fatalf("select exit = %d\nstdout: %s\nstderr: %s", selectCode, selectOut, selectErr)
	}
	for _, want := range []string{
		"Select run ",
		"source    " + valueRunID,
		"budget    context ≤ 10000 tokens",
		"Selected ",
		"greedy on delta-per-cost, no approximation guarantee",
		"Portfolio recorded",
	} {
		if !strings.Contains(selectOut, want) {
			t.Errorf("select report missing %q:\n%s", want, selectOut)
		}
	}
	var selectRunID string
	for _, line := range strings.Split(selectOut, "\n") {
		if after, ok := strings.CutPrefix(line, "Select run "); ok {
			selectRunID = strings.TrimSpace(strings.Split(after, " (")[0])
			break
		}
	}
	if selectRunID == "" {
		t.Fatalf("could not read the select run ID from:\n%s", selectOut)
	}

	exportOut, exportErr, exportCode := run(
		t, "export", "--select-run-id", selectRunID, "--pool", poolPath,
		"--destination", "context", "--out", outPath, "--db", dbPath,
	)
	if exportCode != errs.ExitOK {
		t.Fatalf("export exit = %d\nstdout: %s\nstderr: %s", exportCode, exportOut, exportErr)
	}
	for _, want := range []string{
		"Export run ",
		"destination  context",
		"wrote        " + outPath,
		"never mutates a destination",
	} {
		if !strings.Contains(exportOut, want) {
			t.Errorf("export report missing %q:\n%s", want, exportOut)
		}
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("artifact not written: %v", err)
	}
	if _, err := os.Stat(outPath + ".manifest.md"); err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
	first, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading artifact: %v", err)
	}

	// Refusal: the target exists and --force was not passed.
	_, _, code := run(
		t, "export", "--select-run-id", selectRunID, "--pool", poolPath,
		"--destination", "context", "--out", outPath, "--db", dbPath,
	)
	if code == errs.ExitOK {
		t.Error("export overwrote an existing target without --force")
	}

	// Force: replaces, and the re-export is byte-identical (the plan's
	// golden: idempotence is pinned, not promised).
	_, _, code = run(
		t, "export", "--select-run-id", selectRunID, "--pool", poolPath,
		"--destination", "context", "--out", outPath, "--db", dbPath, "--force",
	)
	if code != errs.ExitOK {
		t.Fatalf("forced export exit = %d", code)
	}
	second, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading re-exported artifact: %v", err)
	}
	if string(first) != string(second) {
		t.Error("re-export is not byte-identical; the golden contract is broken")
	}
}

// TestSelectJSONOutput pins the --json contract's shape without importing
// encoding/json here — the CLI's exemption is scoped to jsonreport.go.
func TestSelectJSONOutput(t *testing.T) {
	t.Parallel()

	casesPath := writeCases(t, 20)
	poolPath := writePool(t, 1)
	dbPath := filepath.Join(t.TempDir(), "kno.db")

	baseOut, _, baseCode := run(
		t, "baseline", "--evals", casesPath, "--agent", "fake:", "--db", dbPath, "--yes",
	)
	if baseCode != errs.ExitOK {
		t.Fatalf("baseline exit = %d", baseCode)
	}
	var baseRunID string
	for _, line := range strings.Split(baseOut, "\n") {
		if after, ok := strings.CutPrefix(line, "Baseline "); ok {
			baseRunID = strings.TrimSpace(after)
		}
	}
	valueOut, _, valueCode := run(
		t, "value", "--evals", casesPath, "--pool", poolPath,
		"--baseline-run-id", baseRunID, "--agent", "fake:", "--db", dbPath, "--yes",
	)
	if valueCode != errs.ExitOK {
		t.Fatalf("value exit = %d", valueCode)
	}
	valueRunID := ""
	for _, line := range strings.Split(valueOut, "\n") {
		if after, ok := strings.CutPrefix(line, "Value run "); ok {
			valueRunID = strings.TrimSpace(strings.Split(after, " (")[0])
			break
		}
	}

	stdout, _, code := run(
		t, "select", "--value-run-id", valueRunID, "--pool", poolPath,
		"--max-cost-usd", "5", "--db", dbPath, "--json",
	)
	if code != errs.ExitOK {
		t.Fatalf("select --json exit = %d\n%s", code, stdout)
	}
	raw, err := cli.DecodeRaw([]byte(stdout))
	if err != nil {
		t.Fatalf("parsing select --json: %v\n%s", err, stdout)
	}
	for _, key := range []string{
		"run_id", "status", "source_run_id", "source_status",
		"budget", "selected", "rejected", "total_cost",
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("select --json missing key %q:\n%s", key, stdout)
		}
	}
	budget, _ := raw["budget"].(map[string]any)
	if budget == nil || budget["max_cost_usd"] != "$5.00" {
		t.Errorf("budget caps not rendered in dollars:\n%s", stdout)
	}
}

// TestExportRefusesBadDestination: the grammar is exactly the three the
// design ships, and anything else is refused with the fix naming them.
func TestExportRefusesBadDestination(t *testing.T) {
	t.Parallel()

	_, stderr, code := run(
		t, "export", "--select-run-id", "nope", "--pool", "nope",
		"--destination", "dataset", "--out", filepath.Join(t.TempDir(), "x.jsonl"),
	)
	if code == errs.ExitOK {
		t.Error("export accepted a destination outside the grammar")
	}
	if !strings.Contains(stderr, "pass one of context, knowledge_base, tuning_set") {
		t.Errorf("bad-destination refusal missing its fix:\n%s", stderr)
	}
}
