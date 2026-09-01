package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The `kno judge calibrate` goldens.
//
// One file per scenario, carrying BOTH renderings separated by a marker — the
// same shape the eval inspect goldens use, and for the same reason: the
// equivalence rule is only reviewable if a reader can see both in one diff.
//
// Regenerate with `make update-golden` and review the diff like code. A change
// to any number here is a change to what the gate decides.

var judgeGoldenScenarios = []struct {
	name string
	args []string
	why  string
}{
	{
		name: "pass",
		args: []string{"judge", "calibrate"},
		why: "exact-match on the committed set: kappa 0.867 with an interval entirely " +
			"above the floor, four near-miss records it gets wrong, and the constant " +
			"judge's raw agreement printed beside the real one",
	},
	{
		name: "indeterminate",
		args: []string{"judge", "calibrate", "--set-name", "straddle"},
		why: "a point estimate above the floor whose interval spans it: the verdict that " +
			"exists so \"we cannot tell\" does not read as \"it is fine\"",
	},
	{
		name: "disagreements",
		args: []string{"judge", "calibrate", "--show-disagreements"},
		why:  "the table a contributor edits a prompt against",
	},
}

func TestJudgeCalibrateGolden(t *testing.T) {
	t.Parallel()

	for _, sc := range judgeGoldenScenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()

			human, _, _ := run(t, sc.args...)
			jsonOut, _, _ := run(t, append(append([]string{}, sc.args...), "--json")...)
			assertJudgeGolden(t, sc.name, sc.why, human, jsonOut)
		})
	}
}

const judgeGoldenSeparator = "\n===== --json =====\n"

func assertJudgeGolden(t *testing.T, name, why, human, jsonOut string) {
	t.Helper()

	path := filepath.Join("testdata", "judge", name+".golden")
	got := "# " + why + "\n\n" + human + judgeGoldenSeparator + jsonOut

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v — re-run with -update to create it", path, err)
	}
	if got != string(want) {
		t.Errorf("%s drifted. Re-run with -update and review the diff.\n\n"+
			"--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}

	// The equivalence rule, asserted rather than eyeballed: the verdict and
	// the raw-agreement caveat are in BOTH halves of the file.
	head, tail, _ := strings.Cut(got, judgeGoldenSeparator)
	if !strings.Contains(head, "a constant judge scores") {
		t.Error("the human rendering lost the constant-judge caveat")
	}
	if !strings.Contains(tail, "constant_judge_raw_agreement") {
		t.Error("--json lost the constant-judge caveat")
	}
}
