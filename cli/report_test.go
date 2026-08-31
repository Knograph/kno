package cli_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/cli"
	"github.com/knograph/kno/core/errs"
)

// runIDFrom reads the generated run ID off a stage's report line, the way
// a scripted user would ("Baseline <id> (completed)").
func runIDFrom(t *testing.T, out, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if after, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(strings.Split(after, " (")[0])
		}
	}
	t.Fatalf("could not read a run ID with prefix %q from:\n%s", prefix, out)
	return ""
}

// TestReportHelpIsSnapshotted keeps the new command's front door under the
// same review discipline as the other stages': a change to what `kno report`
// promises is a reviewed diff.
func TestReportHelpIsSnapshotted(t *testing.T) {
	t.Parallel()

	stdout, _, code := run(t, "report", "--help")
	if code != errs.ExitOK {
		t.Fatalf("help exit = %d", code)
	}
	for _, want := range []string{
		"--value-run-id",
		"--select-run-id",
		"--export-run-id",
		"--watch",
		"--json",
		// The Long text wraps at 80 columns; assert on phrases that survive
		// the wrap.
		"not yet validated on",
		"No LLM calls",
		"trace content",
		"exits 0",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help missing %q:\n%s", want, stdout)
		}
	}
}

// TestReportRefusals pins the hard refusals: an unknown run, a run of the
// wrong stage, and the --watch/--json combination all fail fast with the
// grammar's fix line — the page never half-renders.
func TestReportRefusals(t *testing.T) {
	t.Parallel()

	casesPath := writeCases(t, 10)
	db := filepath.Join(t.TempDir(), "kno.db")
	stdout, _, code := run(t, "baseline", "--evals", casesPath, "--agent", "fake:", "--db", db, "--yes")
	if code != errs.ExitOK {
		t.Fatalf("baseline exit = %d\nstdout: %s", code, stdout)
	}
	baseRunID := runIDFrom(t, stdout, "Baseline ")
	poolPath := writePool(t, 1)
	valueOut, _, valueCode := run(t, "value", "--evals", casesPath, "--pool", poolPath,
		"--baseline-run-id", baseRunID, "--agent", "fake:", "--db", db, "--yes")
	if valueCode != errs.ExitOK {
		t.Fatalf("value exit = %d", valueCode)
	}
	valueRunID := runIDFrom(t, valueOut, "Value run ")

	for _, tc := range []struct {
		name    string
		args    []string
		wantFix string
	}{
		{
			name: "unknown value run",
			args: []string{"report", "--value-run-id", "no-such-run", "--db", db},
			wantFix: "run `kno value` first — every run ID the page shows comes from " +
				"the report lines of `kno baseline`, `kno value`, `kno select`, and `kno export`",
		},
		{
			name:    "wrong stage for value",
			args:    []string{"report", "--value-run-id", baseRunID, "--db", db},
			wantFix: "pass the run ID of a `kno value` run",
		},
		{
			name:    "unknown select run",
			args:    []string{"report", "--value-run-id", valueRunID, "--select-run-id", "no-such", "--db", db},
			wantFix: "run `kno select` first — run IDs come from the Select report line",
		},
		{
			name:    "unknown export run",
			args:    []string{"report", "--value-run-id", valueRunID, "--export-run-id", "no-such", "--db", db},
			wantFix: "run `kno export` first — run IDs come from the Export report line",
		},
		{
			name:    "watch and json refuse to combine",
			args:    []string{"report", "--value-run-id", "no-such", "--watch", "--json", "--db", db},
			wantFix: "drop one of --watch or --json",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := run(t, tc.args...)
			if code != errs.ExitError {
				t.Errorf("exit = %d, want 1\nstderr: %s", code, stderr)
			}
			if !strings.Contains(stderr, tc.wantFix) {
				t.Errorf("stderr missing fix %q:\n%s", tc.wantFix, stderr)
			}
		})
	}
}

// TestReportE2EGoldens drives the whole loop on the fake agent and asserts
// the composed page at each stage combination: the minimal page, the page
// with a Portfolio, and the full story whose gaps section must say the
// honest thing about a run with no cluster data.
func TestReportE2EGoldens(t *testing.T) {
	t.Parallel()

	casesPath := writeCases(t, 30)
	poolPath := writePool(t, 2)
	db := filepath.Join(t.TempDir(), "kno.db")

	baseOut, _, baseCode := run(t, "baseline", "--evals", casesPath, "--agent", "fake:", "--db", db, "--yes")
	if baseCode != errs.ExitOK {
		t.Fatalf("baseline exit = %d", baseCode)
	}
	baseRunID := runIDFrom(t, baseOut, "Baseline ")

	valueOut, _, valueCode := run(t, "value", "--evals", casesPath, "--pool", poolPath,
		"--baseline-run-id", baseRunID, "--agent", "fake:", "--db", db, "--yes")
	if valueCode != errs.ExitOK {
		t.Fatalf("value exit = %d", valueCode)
	}
	valueRunID := runIDFrom(t, valueOut, "Value run ")

	selectOut, _, selectCode := run(t, "select", "--value-run-id", valueRunID, "--pool", poolPath,
		"--max-context-tokens", "10000", "--db", db)
	if selectCode != errs.ExitOK {
		t.Fatalf("select exit = %d", selectCode)
	}
	selectRunID := runIDFrom(t, selectOut, "Select run ")

	outPath := filepath.Join(t.TempDir(), "pack.md")
	exportOut, _, exportCode := run(t, "export", "--select-run-id", selectRunID, "--pool", poolPath,
		"--destination", "context", "--out", outPath, "--db", db)
	if exportCode != errs.ExitOK {
		t.Fatalf("export exit = %d", exportCode)
	}
	exportRunID := runIDFrom(t, exportOut, "Export run ")

	t.Run("baseline and value only", func(t *testing.T) {
		stdout, _, code := run(t, "report", "--value-run-id", valueRunID, "--db", db)
		if code != errs.ExitOK {
			t.Fatalf("report exit = %d", code)
		}
		for _, want := range []string{
			"# Kno report",
			"Value run " + valueRunID,
			"Baseline " + baseRunID,
			"score **1.000**",
			"Asset verdicts",
			"No Portfolio recorded",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("report missing %q:\n%s", want, stdout)
			}
		}
		for _, not := range []string{"Gaps", "not yet recorded"} {
			if strings.Contains(stdout, not) {
				t.Errorf("report shows a stage that was not named (%q):\n%s", not, stdout)
			}
		}
	})

	t.Run("with select", func(t *testing.T) {
		stdout, _, code := run(t, "report", "--value-run-id", valueRunID,
			"--select-run-id", selectRunID, "--db", db)
		if code != errs.ExitOK {
			t.Fatalf("report exit = %d", code)
		}
		for _, want := range []string{
			"Select run " + selectRunID,
			"Rejected, by reason",
			"no-effect",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("report missing %q:\n%s", want, stdout)
			}
		}
	})

	t.Run("full story with export", func(t *testing.T) {
		stdout, _, code := run(t, "report", "--value-run-id", valueRunID,
			"--select-run-id", selectRunID, "--export-run-id", exportRunID, "--db", db)
		if code != errs.ExitOK {
			t.Fatalf("report exit = %d", code)
		}
		for _, want := range []string{
			"Export run " + exportRunID,
			// The fake agent fails nothing, so no failure clusters exist and
			// the honest answer is the absent-answer, never a guess.
			"no cluster data for this run",
			"Recorded aggregates only",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("report missing %q:\n%s", want, stdout)
			}
		}
	})
}

// TestReportJSONE2E pins the machine shape on a real run: the contract keys
// exist, the run IDs line up, and the status words match the human page's.
func TestReportJSONE2E(t *testing.T) {
	t.Parallel()

	casesPath := writeCases(t, 20)
	db := filepath.Join(t.TempDir(), "kno.db")
	baseOut, _, baseCode := run(t, "baseline", "--evals", casesPath, "--agent", "fake:", "--db", db, "--yes")
	if baseCode != errs.ExitOK {
		t.Fatalf("baseline exit = %d", baseCode)
	}
	baseRunID := runIDFrom(t, baseOut, "Baseline ")
	valueOut, _, valueCode := run(t, "value", "--evals", casesPath, "--pool", writePool(t, 1),
		"--baseline-run-id", baseRunID, "--agent", "fake:", "--db", db, "--yes")
	if valueCode != errs.ExitOK {
		t.Fatalf("value exit = %d", valueCode)
	}
	valueRunID := runIDFrom(t, valueOut, "Value run ")

	stdout, _, code := run(t, "report", "--value-run-id", valueRunID, "--json", "--db", db)
	if code != errs.ExitOK {
		t.Fatalf("report exit = %d", code)
	}
	rep, err := cli.DecodeReportJSON([]byte(stdout))
	if err != nil {
		t.Fatalf("decoding report json: %v", err)
	}
	if rep.ValueRunID != valueRunID || rep.Baseline.RunID != baseRunID {
		t.Errorf("run ids wrong: value %q/%q baseline %q/%q",
			rep.ValueRunID, valueRunID, rep.Baseline.RunID, baseRunID)
	}
	if rep.Baseline.Score == nil {
		t.Error("baseline score missing")
	}
	if rep.Portfolio != nil || rep.Gaps != nil {
		t.Errorf("unnamed stages present: %+v", rep)
	}
}

// TestReportHoldoutCanary plants a distinctive Case id in the evals and
// pins that it never reaches the report's page: the report reads recorded
// aggregates, never the source — a holdout Case leaking into the page would
// be the statistical-honesty breach the canary exists to catch.
func TestReportHoldoutCanary(t *testing.T) {
	t.Parallel()

	casesPath := writeCasesWithAnswer(t, 30, "distinctive-holdout-answer")
	db := filepath.Join(t.TempDir(), "kno.db")
	poolPath := writePool(t, 1)

	baseOut, _, baseCode := run(t, "baseline", "--evals", casesPath, "--agent", "fake:", "--db", db, "--yes")
	if baseCode != errs.ExitOK {
		t.Fatalf("baseline exit = %d", baseCode)
	}
	baseRunID := runIDFrom(t, baseOut, "Baseline ")
	valueOut, _, valueCode := run(t, "value", "--evals", casesPath, "--pool", poolPath,
		"--baseline-run-id", baseRunID, "--agent", "fake:", "--db", db, "--yes")
	if valueCode != errs.ExitOK {
		t.Fatalf("value exit = %d", valueCode)
	}
	valueRunID := runIDFrom(t, valueOut, "Value run ")
	selectOut, _, selectCode := run(t, "select", "--value-run-id", valueRunID, "--pool", poolPath,
		"--max-context-tokens", "10000", "--db", db)
	if selectCode != errs.ExitOK {
		t.Fatalf("select exit = %d", selectCode)
	}
	selectRunID := runIDFrom(t, selectOut, "Select run ")

	stdout, _, code := run(t, "report", "--value-run-id", valueRunID,
		"--select-run-id", selectRunID, "--db", db)
	if code != errs.ExitOK {
		t.Fatalf("report exit = %d", code)
	}
	for _, forbidden := range []string{"distinctive-holdout-answer", "case-"} {
		if strings.Contains(stdout, forbidden) {
			t.Errorf("report leaks a Case id (%q) into the page:\n%s", forbidden, stdout)
		}
	}
}

// TestExportJSONNamesItsSelectRun pins the artifact's provenance link.
//
// `kno export --json` declared `select_run_id` in its contract and never
// populated it: the field rendered as "" on every run, including runs that
// were given a --select-run-id. A consumer holding a tuning set could not
// say which measured Portfolio produced it without re-deriving that from the
// manifest — which is exactly what the field exists to save them.
//
// Found by uknoAI/kno-examples, whose scenario asserts on projected --json
// subsets; an empty string where a run ID belongs is the kind of thing a
// human reading their own output stops noticing.
func TestExportJSONNamesItsSelectRun(t *testing.T) {
	t.Parallel()

	db := filepath.Join(t.TempDir(), "kno.db")
	evalsPath := writeCases(t, 10)
	poolPath := writePool(t, 2)

	baseOut, _, code := run(t, "baseline", "--evals", evalsPath, "--agent", "fake:", "--db", db, "--yes")
	if code != errs.ExitOK {
		t.Fatalf("baseline exit = %d", code)
	}
	baseRunID := runIDFrom(t, baseOut, "Baseline ")

	valueOut, _, code := run(t, "value", "--evals", evalsPath, "--pool", poolPath,
		"--baseline-run-id", baseRunID, "--agent", "fake:", "--db", db, "--yes")
	if code != errs.ExitOK {
		t.Fatalf("value exit = %d", code)
	}
	valueRunID := runIDFrom(t, valueOut, "Value run ")

	selectOut, _, code := run(t, "select", "--value-run-id", valueRunID, "--pool", poolPath,
		"--max-context-tokens", "10000", "--db", db)
	if code != errs.ExitOK {
		t.Fatalf("select exit = %d", code)
	}
	selectRunID := runIDFrom(t, selectOut, "Select run ")

	outPath := filepath.Join(t.TempDir(), "pack.md")
	stdout, _, code := run(t, "export", "--select-run-id", selectRunID, "--pool", poolPath,
		"--destination", "context", "--out", outPath, "--db", db, "--json")
	if code != errs.ExitOK {
		t.Fatalf("export exit = %d", code)
	}

	raw, err := cli.DecodeRaw([]byte(stdout))
	if err != nil {
		t.Fatalf("decoding export --json: %v", err)
	}
	got, ok := raw["select_run_id"].(string)
	if !ok {
		t.Fatalf("select_run_id is %T, want a string", raw["select_run_id"])
	}
	if got != selectRunID {
		t.Errorf("select_run_id = %q, want %q — the field names the Portfolio "+
			"this artifact was rendered from", got, selectRunID)
	}
}
