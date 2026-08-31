package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// The `kno eval inspect` goldens.
//
// One file per scenario, carrying BOTH renderings separated by a marker. The
// two renderings are pinned in one file on purpose: the equivalence rule —
// human findings and the --json checks array say the same thing — is only
// reviewable if a reader can see both in one diff. A caveat that survived in
// one renderer and not the other is the failure this catches.
//
// Regenerate with `make update-golden` and review the diff like code.

// inspectGoldenScenarios are the eval sets pinned, and why each one is here.
var inspectGoldenScenarios = []struct {
	name string
	// fixture is a file in testdata/evalinspect/. Passed as a RELATIVE path
	// so the source line in the output is stable across machines.
	fixture string
	// why says what this scenario is the golden for.
	why string
}{
	{
		name: "healthy", fixture: "healthy.jsonl",
		why: "four behaviors, none dominant, all above the power floor, holdout past MinHoldout: the zero-flag page",
	},
	{
		name: "untagged", fixture: "untagged.jsonl",
		why: "no dev Case carries a tag, so routing degrades to all-failed and nothing is attributed per behavior",
	},
	{
		name: "multitagged", fixture: "multitagged.jsonl",
		why: "every dev Case carries two behaviors: the multi-behavior share at 100%, reported and never flagged",
	},
	{
		name: "dominant", fixture: "dominant.jsonl",
		why: "one catch-all tag over most of the set: the concentration flag and its 'carried by' wording",
	},
	{
		name: "spellings", fixture: "spellings.jsonl",
		why: "four spellings of one tag, and a holdout below MinHoldout",
	},
}

// TestEvalInspectGolden pins both renderings of every scenario.
func TestEvalInspectGolden(t *testing.T) {
	t.Parallel()

	for _, sc := range inspectGoldenScenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join("testdata", "evalinspect", sc.fixture)
			human, stderr, code := run(t, "eval", "inspect", "--evals", path)
			if code != errs.ExitOK {
				t.Fatalf("exit %d:\n%s\n%s", code, human, stderr)
			}
			jsonOut, _, code := run(t, "eval", "inspect", "--evals", path, "--json")
			if code != errs.ExitOK {
				t.Fatalf("--json exit %d:\n%s", code, jsonOut)
			}
			assertInspectGolden(t, sc.name, sc.why, human, jsonOut)
		})
	}
}

// TestEvalInspectGoldenWithAValueRun pins the observed section in both
// renderings.
//
// The Run is written directly rather than driven through `kno baseline` +
// `kno value`: the free adapters cannot produce a plan with clusters — see
// writeRun's comment — and a golden of an empty cluster list would pin the
// adapters' limits rather than this command's output.
func TestEvalInspectGoldenWithAValueRun(t *testing.T) {
	t.Parallel()

	path := filepath.Join("testdata", "evalinspect", "observed.jsonl")
	hash := contentHashOf(t, path)

	plan := value.Plan{
		Mode:                value.ModeTagOverlap,
		EligibleCases:       34,
		ControlCaseIDs:      controlIDs(20),
		ControlUnderpowered: true,
		// The one-sided figure at 20 control Cases. Written as a literal so
		// the golden pins the number a reader can check by hand against
		// docs/what-the-numbers-mean.md.
		MinDetectableHarm: 0.27339990310659096,
		Clusters: []value.ClusterSnapshot{{
			Tag:     "refunds",
			CaseIDs: controlIDs(9),
		}},
	}
	db := filepath.Join(t.TempDir(), "kno.db")
	writeRun(t, db, &knov1.Run{
		Id:               "v-golden",
		Stage:            knov1.Stage_STAGE_VALUE,
		Status:           knov1.RunStatus_RUN_STATUS_COMPLETED,
		BaselineRunId:    "b-golden",
		InputFingerprint: hash,
		ValuePlan:        encodePlan(t, plan),
	})

	args := []string{"eval", "inspect", "--evals", path, "--db", db, "--value-run-id", "v-golden"}
	human, stderr, code := run(t, args...)
	if code != errs.ExitOK {
		t.Fatalf("exit %d:\n%s\n%s", code, human, stderr)
	}
	jsonOut, _, code := run(t, append(args, "--json")...)
	if code != errs.ExitOK {
		t.Fatalf("--json exit %d:\n%s", code, jsonOut)
	}
	assertInspectGolden(t, "observed",
		"a recorded Value run joined to the source: the routing mode, the cluster verdict, "+
			"and the ONE-sided minimum detectable harm beside the TWO-sided separable effects",
		human, jsonOut)
}

// controlIDs returns the first n Case IDs of the observed fixture.
func controlIDs(n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, fmt.Sprintf("case-%03d", i))
	}
	return out
}

// inspectGoldenSeparator divides the two renderings in one golden file.
const inspectGoldenSeparator = "\n===== --json =====\n"

// assertInspectGolden compares both renderings against the pinned file.
func assertInspectGolden(t *testing.T, name, why, human, jsonOut string) {
	t.Helper()

	path := filepath.Join("testdata", "evalinspect", name+".golden")
	got := "# " + why + "\n\n" + human + inspectGoldenSeparator + jsonOut

	if *updateGolden {
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
	// The equivalence rule, asserted rather than only eyeballed: the standing
	// conditional is in BOTH halves of the file.
	const conditional = "kno cannot distinguish a behavior tag"
	head, tail, _ := strings.Cut(got, inspectGoldenSeparator)
	if !strings.Contains(collapseSpaces(head), "Kno cannot tell the difference") {
		t.Error("the human rendering lost the standing conditional")
	}
	if !strings.Contains(tail, conditional) {
		t.Error("--json lost the standing conditional")
	}
}
