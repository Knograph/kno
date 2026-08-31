package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/cli"
	"github.com/knograph/kno/core/errs"
)

// spendKeys are the keys a stage that ran a budget guard always emits.
var spendKeys = []string{"guarded", "spent_usd", "spent_usd_micros", "llm_calls"}

// TestValueReportsWhatItSpent is the motivating bug, in both renderings.
//
// Before this, kno value reported nothing about money — cli/value_render.go
// contained no occurrence of "spent", "cost" or "usd" — while being the stage
// DESIGN.md sizes at $15–40 against a baseline's fraction of a dollar. Both
// halves of this test fail on the tree as it stood.
func TestValueReportsWhatItSpent(t *testing.T) {
	docs := stageDocuments(t)

	raw, err := cli.DecodeRaw(docs["value"])
	if err != nil {
		t.Fatalf("the value document is not valid json: %v", err)
	}
	for _, key := range spendKeys {
		if _, ok := raw[key]; !ok {
			t.Errorf("kno value --json is missing %q", key)
		}
	}

	// AC16: metered and free is not the same as unmetered, and the document
	// says so three ways that agree — guarded, a zero figure, and a NON-ZERO
	// call count. The call count is the new assertion: cli/demo_test.go has
	// pinned the $0.00 alone since v0.1, because llm_calls did not exist.
	if raw["guarded"] != true {
		t.Errorf("kno value --json reports guarded = %v; value always runs a guard, "+
			"including on fake:", raw["guarded"])
	}
	if raw["spent_usd"] != "$0.00" {
		t.Errorf("the fake: value run reports %v, want $0.00", raw["spent_usd"])
	}
	if micros, _ := raw["spent_usd_micros"].(float64); micros != 0 {
		t.Errorf("spent_usd_micros = %v, want 0", micros)
	}
	calls, _ := raw["llm_calls"].(float64)
	if calls <= 0 {
		t.Errorf("llm_calls = %v on a run that measured every asset. A zero cost beside "+
			"a zero call count cannot be told apart from no meter at all", raw["llm_calls"])
	}
}

// TestSpendBlockIsIdenticalAcrossStages is the plan's AC2 and AC5 in one: the
// two stages that spend report the same keys with the same names, and the
// human line agrees with the document.
//
// A second private spend formatter is the divergence spend.go exists to
// prevent; TestSpendFieldsAreReadInOneFile catches it structurally and this
// catches it behaviorally.
func TestSpendBlockIsIdenticalAcrossStages(t *testing.T) {
	docs := stageDocuments(t)

	baseline, err := cli.DecodeRaw(docs["baseline"])
	if err != nil {
		t.Fatalf("baseline document: %v", err)
	}
	value, err := cli.DecodeRaw(docs["value"])
	if err != nil {
		t.Fatalf("value document: %v", err)
	}
	for _, key := range spendKeys {
		b, okB := baseline[key]
		v, okV := value[key]
		if !okB || !okV {
			t.Errorf("%q present on baseline=%v value=%v; the block is shared or it is "+
				"not a block", key, okB, okV)
			continue
		}
		if key == "guarded" && (b != true || v != true) {
			t.Errorf("guarded is %v / %v; both stages run a guard", b, v)
		}
	}
}

// TestHumanAndJSONSpendAgree pins the two renderings of one stage to the same
// numbers — ADR-0006 rule 6. A CI log shows the human rendering and a CI gate
// reads the document; the two disagreeing about what a run cost is worse than
// either being absent.
func TestHumanAndJSONSpendAgree(t *testing.T) {
	t.Parallel()

	cases := writeCases(t, 12)
	db := t.TempDir() + "/kno.db"

	human, stderr, code := run(t, "baseline", "--evals", cases, "--db", db, "--yes")
	if code != errs.ExitOK {
		t.Fatalf("human baseline exit = %d\nstderr: %s", code, stderr)
	}
	jsonOut, stderr, code := run(t, "baseline", "--evals", cases,
		"--db", t.TempDir()+"/kno.db", "--json")
	if code != errs.ExitOK {
		t.Fatalf("json baseline exit = %d\nstderr: %s", code, stderr)
	}
	raw, err := cli.DecodeRaw([]byte(jsonOut))
	if err != nil {
		t.Fatalf("the baseline document is not valid json: %v", err)
	}

	dollars, _ := raw["spent_usd"].(string)
	calls, _ := raw["llm_calls"].(float64)
	want := "  spent      " + dollars + " over "
	if !strings.Contains(human, want) {
		t.Errorf("the human output has no spend line matching the document's %q:\n%s",
			dollars, human)
	}
	if calls <= 0 {
		t.Errorf("llm_calls = %v on a 12-case baseline", raw["llm_calls"])
	}
}

// TestSelectExportReportEmitNoSpendBlock is the plan's AC8: absence asserted
// rather than incidental, and the positive signal asserted beside it.
//
// The first half fails if a future PR adds a zero "for consistency", which is
// how this bug survived v0.1 on kno value. The second half fails if the
// absence is ever left to be inferred from a missing key — which jq cannot
// tell from an explicit null, and which a consumer repairs with
// `.spent_usd // 0`, reinstating the very ambiguity the absence encodes.
func TestSelectExportReportEmitNoSpendBlock(t *testing.T) {
	docs := stageDocuments(t)

	for _, stage := range []string{"select", "export", "report"} {
		raw, err := cli.DecodeRaw(docs[stage])
		if err != nil {
			t.Fatalf("the %s document is not valid json: %v", stage, err)
		}
		for _, key := range []string{"spent_usd", "spent_usd_micros", "llm_calls"} {
			if _, ok := raw[key]; ok {
				t.Errorf("kno %s --json emits %q. This stage runs no budget guard; a "+
					"plausible zero here is indistinguishable from a missing meter, "+
					"which is the bug this contract exists to remove", stage, key)
			}
		}
		guarded, ok := raw["guarded"]
		if !ok {
			t.Errorf("kno %s --json has no guarded key, so its absent spend block is "+
				"left to be inferred", stage)
			continue
		}
		if guarded != false {
			t.Errorf("kno %s --json reports guarded = %v, want false", stage, guarded)
		}
	}
}

// TestGuardedMatchesTheStage walks every stage document and asserts the
// boolean tracks whether the STAGE constructs a budget guard — not whether
// the run happened to move money. It is constant per command by design, which
// is what makes it cheap to document and impossible to get wrong at runtime.
func TestGuardedMatchesTheStage(t *testing.T) {
	docs := stageDocuments(t)

	want := map[string]bool{
		"baseline": true, "value": true,
		"select": false, "export": false, "report": false,
	}
	for stage, guarded := range want {
		raw, err := cli.DecodeRaw(docs[stage])
		if err != nil {
			t.Fatalf("the %s document is not valid json: %v", stage, err)
		}
		if raw["guarded"] != guarded {
			t.Errorf("kno %s --json reports guarded = %v, want %v", stage, raw["guarded"], guarded)
		}
	}
}

// TestReportSpendTotalEqualsItsParts is the plan's AC10, AC11 and AC13.
//
// AC13 is the rewritten one: the original asked for a report referencing zero
// metered runs, which is unreachable — --value-run-id is REQUIRED and
// loadBaseline refuses an empty baseline ID. The reachable state carrying the
// same risk is every referenced run settling zero, which is what a fake:
// pipeline produces on every CI run, and which must not read as "free".
func TestReportSpendTotalEqualsItsParts(t *testing.T) {
	docs := stageDocuments(t)

	raw, err := cli.DecodeRaw(docs["report"])
	if err != nil {
		t.Fatalf("the report document is not valid json: %v", err)
	}
	spend, ok := raw["spend"].(map[string]any)
	if !ok {
		t.Fatalf("kno report --json has no spend object: %v", raw["spend"])
	}
	// Select and export are structurally absent rather than present at zero.
	for _, stage := range []string{"select", "export"} {
		if _, ok := spend[stage]; ok {
			t.Errorf("the spend object has a %s entry; that stage runs no guard", stage)
		}
	}

	var total float64
	var calls float64
	for _, stage := range []string{"baseline", "value"} {
		entry, ok := spend[stage].(map[string]any)
		if !ok {
			t.Fatalf("the spend object has no %s entry; the page always names one", stage)
		}
		if entry["run_id"] == "" || entry["run_id"] == nil {
			t.Errorf("the %s entry names no run", stage)
		}
		micros, _ := entry["spent_usd_micros"].(float64)
		llm, _ := entry["llm_calls"].(float64)
		total += micros
		calls += llm
	}
	if got, _ := spend["total_usd_micros"].(float64); got != total {
		t.Errorf("total_usd_micros = %v, its entries sum to %v", got, total)
	}
	if got, _ := spend["total_llm_calls"].(float64); got != calls {
		t.Errorf("total_llm_calls = %v, its entries sum to %v", got, calls)
	}
	if spend["incomplete"] != false {
		t.Errorf("incomplete = %v on a pipeline where nothing stopped early", spend["incomplete"])
	}
	// AC13's reachable state: metered, and it read zero.
	if total != 0 {
		t.Fatalf("the fake: pipeline settled %v micros; the fixture changed", total)
	}
	if spend["no_metered_spend"] != true {
		t.Errorf("no_metered_spend = %v on a pipeline whose metered runs all settled "+
			"zero. Without it, total_usd_micros: 0 cannot be told from an unmetered "+
			"pipeline", spend["no_metered_spend"])
	}
	if calls <= 0 {
		t.Errorf("the spend object totals %v LLM calls; the meter ran", calls)
	}
}

// TestSpendRenderingCarriesNoCaseContent is the plan's AC19, extended over
// the surfaces this change adds.
//
// Structural rather than incidental: the spend block is built from a
// budget.Spend (three integers), a run ID, and an int32 count, so there is no
// field a Case's input could travel through. This asserts it anyway, because
// "it cannot happen" is the claim every leak was made under — and because the
// cheapest moment to catch a future `spent %s on case %q` is now.
func TestSpendRenderingCarriesNoCaseContent(t *testing.T) {
	t.Parallel()

	const sentinel = "SENTINEL-CASE-CONTENT-MUST-NEVER-BE-PRINTED"
	path := writeSentinelCases(t, sentinel, 8)
	db := t.TempDir() + "/kno.db"

	human, stderr, code := run(t, "baseline", "--evals", path, "--db", db,
		"--run-id", "sentinel-base", "--yes")
	if code != errs.ExitOK {
		t.Fatalf("baseline exit = %d\nstderr: %s", code, stderr)
	}
	jsonOut, stderr, code := run(t, "baseline", "--evals", path,
		"--db", t.TempDir()+"/kno.db", "--json")
	if code != errs.ExitOK {
		t.Fatalf("baseline --json exit = %d\nstderr: %s", code, stderr)
	}
	for name, out := range map[string]string{"human": human, "--json": jsonOut} {
		if strings.Contains(out, sentinel) {
			t.Errorf("the %s baseline rendering carries Case content:\n%s", name, out)
		}
	}
}

// writeSentinelCases writes an eval set whose every text field is the
// sentinel, so any leak into a rendering is unambiguous.
func writeSentinelCases(t *testing.T, sentinel string, n int) string {
	t.Helper()
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, `{"id":"case-%03d","input":%q,"expected":%q}`+"\n",
			i, sentinel+"-input", sentinel+"-expected")
	}
	path := filepath.Join(t.TempDir(), "sentinel.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("writing sentinel cases: %v", err)
	}
	return path
}
