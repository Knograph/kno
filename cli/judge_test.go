package cli_test

import (
	"fmt"
	"math"
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

// TestNoJudgeJSONFloatCarriesMoreThanFourPlaces is the gate that would have
// caught the golden drift before CI did.
//
// `kappa_interval.high` was recorded as 0.929508759876331 on darwin/arm64 and
// read 0.9295087598763309 on linux/amd64 — one ULP apart, and re-recording the
// golden only moves the failure to the other architecture. The fix is rounding
// at the source (see judge/kappa.go); this asserts the PROPERTY across every
// scenario and every float in the document, so a statistic added later without
// the treatment fails here rather than on whichever runner CI picks.
//
// It walks the raw document rather than the decoded struct, because the
// contract is the emitted JSON.
func TestNoJudgeJSONFloatCarriesMoreThanFourPlaces(t *testing.T) {
	t.Parallel()

	scenarios := [][]string{
		{"judge", "calibrate", "--json"},
		{"judge", "calibrate", "--set-name", "straddle", "--json"},
		{"judge", "calibrate", "--show-disagreements", "--json"},
		{
			"judge", "calibrate", "--all", "--json",
			"--baseline", filepath.Join("..", "judge", "calibration.baseline.json"),
		},
	}
	for _, args := range scenarios {
		t.Run(strings.Join(args[2:], " "), func(t *testing.T) {
			t.Parallel()

			out, _, _ := run(t, args...)
			raw, err := cli.DecodeRawDocument([]byte(out))
			if err != nil {
				t.Fatalf("not a JSON document: %v\n%s", err, out)
			}
			walkFloats(t, "", raw)
		})
	}
}

// walkFloats asserts every float in a decoded document is its own four-place
// rounding.
func walkFloats(t *testing.T, path string, v any) {
	t.Helper()

	switch typed := v.(type) {
	case map[string]any:
		for k, child := range typed {
			walkFloats(t, path+"."+k, child)
		}
	case []any:
		for i, child := range typed {
			walkFloats(t, fmt.Sprintf("%s[%d]", path, i), child)
		}
	case float64:
		if want := math.Round(typed*1e4) / 1e4; typed != want {
			t.Errorf("%s = %v carries more than four decimal places.\n"+
				"An unrounded statistic differs between arm64 and amd64 in its tail "+
				"digits, so no golden can hold it — and a bootstrap over a few dozen "+
				"records does not carry seventeen significant figures anyway. Round it "+
				"at the source, as judge/kappa.go does.", strings.TrimPrefix(path, "."), typed)
		}
	}
}
