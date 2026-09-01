package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/cli"
	"github.com/knograph/kno/core/errs"
)

// TestJudgeCalibrateReportsEveryNumberKappaHides is acceptance criterion 1.
//
// A single scalar cannot say which way a judge is wrong, and which way is
// exactly what a prompt edit needs to know. Every one of these is on the same
// screen as kappa, and the --json document carries the same keys.
func TestJudgeCalibrateReportsEveryNumberKappaHides(t *testing.T) {
	t.Parallel()

	out, stderr, code := run(t, "judge", "calibrate")
	if code != errs.ExitOK {
		t.Fatalf("exit %d:\n%s\n%s", code, out, stderr)
	}
	for _, want := range []string{
		"kappa", "95% CI", "raw agreement", "sensitivity", "specificity",
		"marginals", "inter-human kappa", "PASS", "60 scored",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report omits %q:\n%s", want, out)
		}
	}
}

// TestJudgeCalibrateJSONCarriesTheSameData is ADR-0006's rule: the document is
// the same data, not a second rendering. A caveat that survives in one
// renderer and not the other is what this catches.
func TestJudgeCalibrateJSONCarriesTheSameData(t *testing.T) {
	t.Parallel()

	out, _, code := run(t, "judge", "calibrate", "--json")
	if code != errs.ExitOK {
		t.Fatalf("exit %d:\n%s", code, out)
	}

	doc, err := cli.DecodeJudgeCalibrateJSON([]byte(out))
	if err != nil {
		t.Fatalf("the document is not valid JSON: %v\n%s", err, out)
	}
	if doc.Verdict != "PASS" || doc.Failed != 0 {
		t.Errorf("summary says %s with %d failed", doc.Verdict, doc.Failed)
	}
	if len(doc.Calibrations) != 1 {
		t.Fatalf("got %d calibrations", len(doc.Calibrations))
	}

	// The key set is asserted against the raw document, not against the
	// decoded struct: a key deleted from the shape would still decode, and the
	// jq pipelines this is a contract for read keys.
	raw, err := cli.DecodeRawDocument([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	list, ok := raw["calibrations"].([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("no calibrations array in %s", out)
	}
	c, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("the first calibration is not an object")
	}
	for _, key := range []string{
		"kappa", "kappa_interval", "raw_agreement", "constant_judge_raw_agreement",
		"sensitivity", "specificity", "symmetry_gap", "judge_positive_rate",
		"human_positive_rate", "inter_human_kappa", "n_records", "min_kappa",
		"verdict", "prompt_sha", "source", "set_content_sha256",
	} {
		if _, ok := c[key]; !ok {
			t.Errorf("--json omits %q", key)
		}
	}
	if c["source"] != "local" {
		t.Errorf("source = %v; exact-match calls no model", c["source"])
	}
	if _, ok := c["spend"]; ok {
		t.Error("a replay emitted a spend block; there was no meter to read")
	}
}

// TestAStraddlingIntervalExitsOne. "We cannot tell" is not "it is fine", and a
// CI gate must see that as a failure.
func TestAStraddlingIntervalExitsOne(t *testing.T) {
	t.Parallel()

	out, stderr, code := run(t, "judge", "calibrate", "--set-name", "straddle")
	if code != errs.ExitError {
		t.Fatalf("exit %d, want %d:\n%s", code, errs.ExitError, out)
	}
	if !strings.Contains(out, "INDETERMINATE") {
		t.Errorf("the page does not say INDETERMINATE:\n%s", out)
	}
	if !strings.Contains(stderr, "Add records") {
		t.Errorf("the error does not name the fix:\n%s", stderr)
	}
}

// TestShowDisagreementsPrintsTheTable, which is the artifact that makes a
// prompt edit a directed act instead of a guess.
func TestShowDisagreementsPrintsTheTable(t *testing.T) {
	t.Parallel()

	out, _, code := run(t, "judge", "calibrate", "--show-disagreements")
	if code != errs.ExitOK {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "Disagreements") ||
		!strings.Contains(out, "RECORD") || !strings.Contains(out, "RATIONALE") {
		t.Errorf("no disagreement table:\n%s", out)
	}
}

// TestUnknownGoalNamesTheRegistrysKeys. The hardcoded `if` this replaced could
// only ever name exact-match, however many Goals existed.
func TestUnknownGoalNamesTheRegistrysKeys(t *testing.T) {
	t.Parallel()

	_, stderr, code := run(t, "judge", "calibrate", "--goal", "rubric-judge")
	if code != errs.ExitError {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr, "available goals: exact-match") {
		t.Errorf("the error does not list the registry's keys:\n%s", stderr)
	}
}

// TestReplayRefusesASpendCap. A cap on a path that cannot spend suggests there
// is spend to cap.
func TestReplayRefusesASpendCap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"a cost cap on a replay", []string{"--max-cost-usd", "1"}, "--live only"},
		{"a call cap on a replay", []string{"--max-calls", "10"}, "--live only"},
		{"both modes at once", []string{"--live", "--replay"}, "mutually exclusive"},
		{"a floor outside (0, 1)", []string{"--min-kappa", "1.5"}, "min-kappa"},
		{"--all with no baseline", []string{"--all"}, "--all needs --baseline"},
		{"--write-baseline with no baseline", []string{"--write-baseline"}, "needs --baseline"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, stderr, code := run(t, append([]string{"judge", "calibrate"}, tc.args...)...)
			if code == errs.ExitOK {
				t.Fatal("the flag combination was accepted")
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("the refusal does not name %q:\n%s", tc.want, stderr)
			}
		})
	}
}

// TestTheRatchetGateRunsAgainstTheCommittedBaseline is what `make
// judge-calibrate-check` runs.
func TestTheRatchetGateRunsAgainstTheCommittedBaseline(t *testing.T) {
	t.Parallel()

	out, stderr, code := run(t, "judge", "calibrate", "--all",
		"--baseline", filepath.Join("..", "judge", "calibration.baseline.json"))
	if code != errs.ExitOK {
		t.Fatalf("the committed baseline does not reproduce: exit %d\n%s\n%s", code, out, stderr)
	}
	if !strings.Contains(out, "against the recorded baseline") {
		t.Errorf("the ratchet did not run:\n%s", out)
	}
}

// TestARecordedRegressionFailsTheGate drives the ratchet end to end through
// the command: a baseline claiming a much better judge must fail this run.
func TestARecordedRegressionFailsTheGate(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile(filepath.Join("..", "judge", "calibration.baseline.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Flip the recorded verdicts on the four records exact-match gets wrong,
	// so the baseline describes a perfect judge and this run is a real drop.
	doctored := strings.Replace(string(src),
		`"verdicts": "011111101111111101111111011111110000000000000000000000000000"`,
		`"verdicts": "111111111111111111111111111111110000000000000000000000000000"`, 1)
	if doctored == string(src) {
		t.Fatal("the verdict vector this test doctors has moved; update it")
	}
	doctored = strings.Replace(doctored, `"kappa": 0.8672566371681416`, `"kappa": 1.0`, 1)

	path := filepath.Join(t.TempDir(), "calibration.baseline.json")
	if err := os.WriteFile(path, []byte(doctored), 0o600); err != nil {
		t.Fatal(err)
	}

	out, stderr, code := run(t, "judge", "calibrate", "--all", "--baseline", path)
	if code != errs.ExitError {
		t.Fatalf("a recorded regression passed: exit %d\n%s", code, out)
	}
	if !strings.Contains(stderr, "regressed") && !strings.Contains(out, "regressed") {
		t.Errorf("the failure does not name the regression:\n%s\n%s", out, stderr)
	}
}

// TestDoctorReportsTheRegistrysGoals: one list, two readers.
func TestDoctorReportsTheRegistrysGoals(t *testing.T) {
	t.Parallel()

	out, _, code := run(t, "doctor", "--json")
	if code != errs.ExitOK {
		t.Fatalf("exit %d", code)
	}
	raw, err := cli.DecodeRawDocument([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	goals, ok := raw["goals"].([]any)
	if !ok || len(goals) != 1 || goals[0] != "exact-match" {
		t.Errorf("doctor reports goals %v", raw["goals"])
	}
}
