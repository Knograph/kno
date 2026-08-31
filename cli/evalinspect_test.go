package cli_test

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/evals/jsonl"
	"github.com/knograph/kno/adapters/evals/split"
	"github.com/knograph/kno/cli"
	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/store"
)

// The end-to-end tests for `kno eval inspect`: the real command, the real
// jsonl adapter, the real store, and the two honesty canaries.
//
// The goldens live in cli/testdata/. `make update-golden` regenerates them;
// review the diff like code.

// inspectCasesFile writes a JSONL eval set and returns its path.
func inspectCasesFile(t *testing.T, name string, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

// inspectCase writes one Case line with tags.
func inspectCase(id string, tags ...string) string {
	quoted := make([]string, 0, len(tags))
	for _, tg := range tags {
		quoted = append(quoted, fmt.Sprintf("%q", tg))
	}
	return fmt.Sprintf(`{"id":%q,"input":"q-%s","expected":"a-%s","tags":[%s]}`,
		id, id, id, strings.Join(quoted, ","))
}

// TestEvalInspectExitsZeroWhateverItFinds. `inspect` is a diagnostic, not a
// gate: making findings non-zero would collide with ExitError's meaning and
// would make the command unrunnable in the pre-commit position where it is
// most useful.
func TestEvalInspectExitsZeroWhateverItFinds(t *testing.T) {
	t.Parallel()

	t.Run("nothing to flag", func(t *testing.T) {
		t.Parallel()
		var lines []string
		for i := range 200 {
			// Four behaviors, none dominant, all well above the power floor,
			// and a holdout comfortably past split.MinHoldout.
			lines = append(lines, inspectCase(fmt.Sprintf("case-%03d", i),
				[]string{"billing", "refunds", "shipping", "account"}[i%4]))
		}
		path := inspectCasesFile(t, "healthy.jsonl", lines...)

		stdout, _, code := run(t, "eval", "inspect", "--evals", path)
		if code != errs.ExitOK {
			t.Fatalf("exit %d, want %d:\n%s", code, errs.ExitOK, stdout)
		}
		if !strings.Contains(stdout, "0 of 5 checks flagged.") {
			t.Errorf("headline is not the clean count:\n%s", stdout)
		}
	})

	t.Run("everything flaggable flagged", func(t *testing.T) {
		t.Parallel()
		// One dominant tag on every Case, one behavior below the power floor,
		// a tiny holdout, and no run: the worst honest set.
		lines := []string{}
		for i := range 12 {
			lines = append(lines, inspectCase(fmt.Sprintf("case-%03d", i), "overall_quality"))
		}
		lines = append(lines, inspectCase("case-900", "overall_quality", "tool_use"))
		path := inspectCasesFile(t, "flagged.jsonl", lines...)

		stdout, _, code := run(t, "eval", "inspect", "--evals", path)
		if code != errs.ExitOK {
			t.Fatalf("exit %d with findings, want %d — inspect is a diagnostic, not a gate:\n%s",
				code, errs.ExitOK, stdout)
		}
		if !strings.Contains(stdout, " of 5 checks flagged.") {
			t.Errorf("headline does not count out of five:\n%s", stdout)
		}
	})
}

// TestEvalInspectNeverPrintsCaseContent is the honesty sentinel. Every Case's
// input, expected and rubric is a unique marker, and none of them may reach
// stdout, stderr or --json. `inspect` prints tags, counts and IDs only.
//
// Turn content is covered by the internal sentinel test instead: the jsonl
// file format has no history field (adapters/evals/jsonl/format.go), so a
// Case with turns cannot be written here.
func TestEvalInspectNeverPrintsCaseContent(t *testing.T) {
	t.Parallel()

	const sentinel = "SENTINEL-e6f1a2-DO-NOT-PRINT"
	var lines []string
	for i := range 40 {
		lines = append(lines, fmt.Sprintf(
			`{"id":"case-%03d","input":"%s-input-%d","expected":"%s-expected-%d",`+
				`"rubric":"%s-rubric-%d","tags":["billing"]}`,
			i, sentinel, i, sentinel, i, sentinel, i,
		))
	}
	path := inspectCasesFile(t, "sentinel.jsonl", lines...)

	for _, args := range [][]string{
		{"eval", "inspect", "--evals", path},
		{"eval", "inspect", "--evals", path, "--json"},
	} {
		stdout, stderr, code := run(t, args...)
		if code != errs.ExitOK {
			t.Fatalf("%v: exit %d\n%s\n%s", args, code, stdout, stderr)
		}
		if strings.Contains(stdout, sentinel) {
			t.Errorf("%v: Case content reached stdout", args)
		}
		if strings.Contains(stderr, sentinel) {
			t.Errorf("%v: Case content reached stderr", args)
		}
	}
}

// TestEvalInspectNeverReadsTheHoldout is the seal canary. A holdout Case
// carries a tag that appears nowhere else; that tag must not appear in the
// behavior list, the findings, or --json.
//
// The Case IDs are chosen so the tagged Case lands in the holdout at the
// default fraction — asserted, not assumed, because a fixture that silently
// stopped putting the canary in the holdout would prove nothing.
func TestEvalInspectNeverReadsTheHoldout(t *testing.T) {
	t.Parallel()

	const canary = "canary-holdout-only-tag"

	// Find an ID the default split assigns to the holdout. Done here rather
	// than hard-coded so the fixture survives a change to the split's inputs
	// — and if none is found the test fails loudly instead of passing
	// vacuously.
	lines := []string{}
	for i := range 200 {
		lines = append(lines, inspectCase(fmt.Sprintf("case-%03d", i), "billing"))
	}
	canaryID := ""
	for i := range 500 {
		id := fmt.Sprintf("canary-%03d", i)
		if splitOf(id) == knov1.Split_SPLIT_HOLDOUT {
			canaryID = id
			break
		}
	}
	if canaryID == "" {
		t.Fatal("no fixture ID lands in the holdout; the canary cannot be planted")
	}
	lines = append(lines, inspectCase(canaryID, canary))
	path := inspectCasesFile(t, "canary.jsonl", lines...)

	stdout, stderr, code := run(t, "eval", "inspect", "--evals", path)
	if code != errs.ExitOK {
		t.Fatalf("exit %d:\n%s\n%s", code, stdout, stderr)
	}
	if strings.Contains(stdout, canary) {
		t.Errorf("a holdout Case's tag reached the behavior list:\n%s", stdout)
	}
	if strings.Contains(stdout, canaryID) {
		t.Errorf("a holdout Case ID reached the output:\n%s", stdout)
	}

	jsonOut, _, code := run(t, "eval", "inspect", "--evals", path, "--json")
	if code != errs.ExitOK {
		t.Fatalf("--json exit %d:\n%s", code, jsonOut)
	}
	if strings.Contains(jsonOut, canary) || strings.Contains(jsonOut, canaryID) {
		t.Errorf("a holdout Case reached --json:\n%s", jsonOut)
	}
	// The holdout is still COUNTED — counting is not reading, which is the
	// distinction CountSplits already relies on.
	doc, err := cli.DecodeInspectJSON([]byte(jsonOut))
	if err != nil {
		t.Fatalf("--json is not one document: %v", err)
	}
	if doc.Cases.Holdout == 0 {
		t.Error("the holdout is not counted at all; totals come from CountSplits")
	}
}

// splitOf mirrors the default split for the canary fixture. It calls the
// same function every adapter calls, so a change to the split's inputs moves
// the fixture rather than silently un-planting the canary.
func splitOf(id string) knov1.Split {
	return split.AssignSplit(id, "", split.DefaultHoldoutFrac)
}

// contentHashOf is the eval source's fingerprint, computed through the same
// adapter method the Value stage recorded it with.
func contentHashOf(t *testing.T, path string) string {
	t.Helper()
	src, err := jsonl.New(jsonl.Options{Path: path})
	if err != nil {
		t.Fatalf("jsonl.New: %v", err)
	}
	hash, err := src.ContentHash(context.Background())
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	return hash
}

// TestEvalInspectRefusesBadInputWithAFixLine covers the exit-1 paths: an
// empty eval set, a malformed line, an unknown run, an unreadable database,
// and a run of the wrong stage.
func TestEvalInspectRefusesBadInputWithAFixLine(t *testing.T) {
	t.Parallel()

	t.Run("empty eval set", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "empty.jsonl")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("writing: %v", err)
		}
		stdout, stderr, code := run(t, "eval", "inspect", "--evals", path)
		if code != errs.ExitError {
			t.Fatalf("exit %d, want %d", code, errs.ExitError)
		}
		if !strings.Contains(stderr, "--evals") {
			t.Errorf("the refusal does not name --evals:\n%s", stderr)
		}
		if strings.Contains(stdout, "checks flagged") {
			t.Errorf("a partial analysis was printed:\n%s", stdout)
		}
	})

	t.Run("malformed line", func(t *testing.T) {
		t.Parallel()
		path := inspectCasesFile(t, "bad.jsonl",
			inspectCase("case-001", "billing"), "{not json", inspectCase("case-002", "billing"))
		stdout, stderr, code := run(t, "eval", "inspect", "--evals", path)
		if code != errs.ExitError {
			t.Fatalf("exit %d, want %d", code, errs.ExitError)
		}
		if !strings.Contains(stderr, "line 2") {
			t.Errorf("the adapter's line context is missing:\n%s", stderr)
		}
		if strings.Contains(stdout, "checks flagged") {
			t.Errorf("a partial analysis was printed before the fatal error:\n%s", stdout)
		}
	})

	t.Run("unknown run", func(t *testing.T) {
		t.Parallel()
		path := inspectCasesFile(t, "cases.jsonl", inspectCase("case-001", "billing"))
		db := filepath.Join(t.TempDir(), "kno.db")
		_, stderr, code := run(t, "eval", "inspect", "--evals", path,
			"--db", db, "--value-run-id", "no-such-run")
		if code != errs.ExitError {
			t.Fatalf("exit %d, want %d", code, errs.ExitError)
		}
		if !strings.Contains(stderr, "`kno value`") {
			t.Errorf("the fix does not say where run IDs come from:\n%s", stderr)
		}
	})

	t.Run("wrong stage", func(t *testing.T) {
		t.Parallel()
		path := inspectCasesFile(t, "cases.jsonl", inspectCase("case-001", "billing"))
		db := filepath.Join(t.TempDir(), "kno.db")
		writeRun(t, db, &knov1.Run{
			Id:    "b1",
			Stage: knov1.Stage_STAGE_BASELINE,
		})
		_, stderr, code := run(t, "eval", "inspect", "--evals", path,
			"--db", db, "--value-run-id", "b1")
		if code != errs.ExitError {
			t.Fatalf("exit %d, want %d", code, errs.ExitError)
		}
		if !strings.Contains(stderr, "not a value run") {
			t.Errorf("the refusal does not name the run's actual stage:\n%s", stderr)
		}
	})
}

// TestEvalInspectObservedSectionJoinsARecordedRun drives the --value-run-id
// path against a real store: a plan with clusters, a baseline with recorded
// per-Case scores, and the ComputeGaps verdicts.
func TestEvalInspectObservedSectionJoinsARecordedRun(t *testing.T) {
	t.Parallel()

	var lines []string
	for i := range 30 {
		lines = append(lines, inspectCase(fmt.Sprintf("case-%03d", i), "refunds"))
	}
	path := inspectCasesFile(t, "cases.jsonl", lines...)
	hash := contentHashOf(t, path)

	plan := value.Plan{
		Mode:                value.ModeTagOverlap,
		EligibleCases:       24,
		ControlCaseIDs:      []string{"case-001", "case-002", "case-003"},
		ControlUnderpowered: true,
		MinDetectableHarm:   0.6741491466,
		Clusters: []value.ClusterSnapshot{{
			Tag:     "refunds",
			CaseIDs: []string{"case-001", "case-002", "case-003", "case-004", "case-005"},
		}},
	}
	db := filepath.Join(t.TempDir(), "kno.db")
	writeRun(t, db, &knov1.Run{
		Id:               "v1",
		Stage:            knov1.Stage_STAGE_VALUE,
		Status:           knov1.RunStatus_RUN_STATUS_COMPLETED,
		BaselineRunId:    "b1",
		InputFingerprint: hash,
		ValuePlan:        encodePlan(t, plan),
	})

	stdout, stderr, code := run(t, "eval", "inspect", "--evals", path,
		"--db", db, "--value-run-id", "v1")
	if code != errs.ExitOK {
		t.Fatalf("exit %d:\n%s\n%s", code, stdout, stderr)
	}
	for _, want := range []string{
		"Observed  value run v1",
		"routing mode tag-overlap",
		"one-sided 95%",
		"refunds",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the observed section is missing %q:\n%s", want, stdout)
		}
	}

	jsonOut, _, code := run(t, "eval", "inspect", "--evals", path,
		"--db", db, "--value-run-id", "v1", "--json")
	if code != errs.ExitOK {
		t.Fatalf("--json exit %d:\n%s", code, jsonOut)
	}
	doc, err := cli.DecodeInspectJSON([]byte(jsonOut))
	if err != nil {
		t.Fatalf("--json: %v\n%s", err, jsonOut)
	}
	if doc.ChecksTotal != 5 {
		t.Errorf("checks_total = %d with a run, want 5", doc.ChecksTotal)
	}
	if doc.Observed == nil {
		t.Fatal("--json carries no observed object")
	}
	if !doc.Observed.EvalSourceMatchesRun {
		t.Error("eval_source_matches_run is false on a matching source")
	}
	if doc.Observed.MinDetectableHarmSidedness != "one-sided" {
		t.Errorf("min_detectable_harm_sidedness = %q, want one-sided",
			doc.Observed.MinDetectableHarmSidedness)
	}
	if len(doc.Observed.Behaviors) != 1 || doc.Observed.Behaviors[0].Tag != "refunds" {
		t.Fatalf("observed behaviors = %+v, want one refunds cluster", doc.Observed.Behaviors)
	}
	// No Valuation was recorded, so nothing covered the cluster: UNKNOWN, and
	// never GAP. "We did not look" is not "we looked and found nothing".
	if got := doc.Observed.Behaviors[0].GapStatus; got != "GAP_STATUS_UNKNOWN" {
		t.Errorf("gap_status = %q with no valuations, want GAP_STATUS_UNKNOWN", got)
	}
	for _, c := range doc.Checks {
		if c.Name == "attribution_observed" && c.Status != "ok" {
			t.Errorf("attribution_observed = %q with a joined run, want ok", c.Status)
		}
	}
}

// TestEvalInspectWithholdsTheObservedSectionWhenTheSourceMoved: a stale plan
// joined to a current tag structure would be a page composed of two eval sets.
func TestEvalInspectWithholdsTheObservedSectionWhenTheSourceMoved(t *testing.T) {
	t.Parallel()

	path := inspectCasesFile(t, "cases.jsonl",
		inspectCase("case-001", "billing"), inspectCase("case-002", "billing"))

	db := filepath.Join(t.TempDir(), "kno.db")
	writeRun(t, db, &knov1.Run{
		Id:               "v1",
		Stage:            knov1.Stage_STAGE_VALUE,
		Status:           knov1.RunStatus_RUN_STATUS_COMPLETED,
		InputFingerprint: "a-hash-from-a-different-file",
		ValuePlan:        encodePlan(t, value.Plan{Mode: value.ModeTagOverlap}),
	})

	stdout, _, code := run(t, "eval", "inspect", "--evals", path,
		"--db", db, "--value-run-id", "v1")
	if code != errs.ExitOK {
		t.Fatalf("exit %d, want 0:\n%s", code, stdout)
	}
	if strings.Contains(stdout, "Observed  value run") {
		t.Errorf("the observed section was rendered against a changed source:\n%s", stdout)
	}
	if !strings.Contains(collapseSpaces(stdout), "the eval source has changed since this run") {
		t.Errorf("the withholding is not explained:\n%s", stdout)
	}
}

// TestEvalInspectReportsUnknownForAnUndecodablePlan: no guess, no panic.
func TestEvalInspectReportsUnknownForAnUndecodablePlan(t *testing.T) {
	t.Parallel()

	path := inspectCasesFile(t, "cases.jsonl", inspectCase("case-001", "billing"))
	hash := contentHashOf(t, path)

	tests := []struct {
		name string
		plan []byte
		want string
	}{
		{name: "no plan recorded", plan: nil, want: "recorded no routing plan"},
		{
			name: "plan does not decode", plan: []byte("not a gob stream"),
			want: "cannot decode",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := filepath.Join(t.TempDir(), "kno.db")
			writeRun(t, db, &knov1.Run{
				Id:               "v1",
				Stage:            knov1.Stage_STAGE_VALUE,
				Status:           knov1.RunStatus_RUN_STATUS_COMPLETED,
				InputFingerprint: hash,
				ValuePlan:        tt.plan,
			})
			stdout, _, code := run(t, "eval", "inspect", "--evals", path,
				"--db", db, "--value-run-id", "v1")
			if code != errs.ExitOK {
				t.Fatalf("exit %d, want 0:\n%s", code, stdout)
			}
			if !strings.Contains(collapseSpaces(stdout), tt.want) {
				t.Errorf("the unknown answer does not say %q:\n%s", tt.want, stdout)
			}
			if !strings.Contains(collapseSpaces(stdout), "attribution_observed is unknown") {
				t.Errorf("attribution_observed is not unknown:\n%s", stdout)
			}
		})
	}
}

// TestEvalInspectHumanAndJSONAgree is the equivalence golden's assertion in
// code: the checks array and the human findings carry the same statuses in
// the same order, and the behavior ordering is identical.
func TestEvalInspectHumanAndJSONAgree(t *testing.T) {
	t.Parallel()

	var lines []string
	for i := range 40 {
		lines = append(lines, inspectCase(fmt.Sprintf("case-%03d", i),
			[]string{"overall_quality", "overall_quality", "billing", "tool_use"}[i%4]))
	}
	path := inspectCasesFile(t, "cases.jsonl", lines...)

	human, _, code := run(t, "eval", "inspect", "--evals", path)
	if code != errs.ExitOK {
		t.Fatalf("exit %d:\n%s", code, human)
	}
	jsonOut, _, code := run(t, "eval", "inspect", "--evals", path, "--json")
	if code != errs.ExitOK {
		t.Fatalf("--json exit %d:\n%s", code, jsonOut)
	}

	doc, err := cli.DecodeInspectJSON([]byte(jsonOut))
	if err != nil {
		t.Fatalf("--json is not one document: %v\n%s", err, jsonOut)
	}

	// Exactly one document, no prose.
	if strings.TrimSpace(jsonOut)[0] != '{' {
		t.Error("--json emitted prose before the document")
	}

	// The headline count matches checks_flagged.
	if !strings.Contains(collapseSpaces(human), fmt.Sprintf("%d of %d checks flagged.", doc.ChecksFlagged, doc.ChecksTotal)) {
		t.Errorf("the human headline disagrees with checks_flagged (%d of %d):\n%s",
			doc.ChecksFlagged, doc.ChecksTotal, human)
	}

	// Every check's detail appears in the human findings, in order. Compared
	// on collapsed whitespace: the human rendering wraps, and a wrapped
	// sentence is the same sentence.
	flat := collapseSpaces(human)
	at := 0
	for _, c := range doc.Checks {
		idx := strings.Index(flat[at:], collapseSpaces(c.Detail))
		if idx < 0 {
			t.Errorf("check %q's detail %q is absent from the human rendering", c.Name, c.Detail)
			continue
		}
		at += idx
	}

	// Behavior ordering is identical.
	at = 0
	for _, b := range doc.Behaviors {
		idx := strings.Index(flat[at:], b.Tag)
		if idx < 0 {
			t.Errorf("behavior %q is absent from the human table or out of order", b.Tag)
			continue
		}
		at += idx
	}

	// The standing conditional is notes[0] in --json and appears once, above
	// the table, in the human rendering.
	if len(doc.Notes) == 0 || !strings.Contains(doc.Notes[0], "kno cannot distinguish a behavior tag") {
		t.Errorf("notes[0] is not the standing conditional: %v", doc.Notes)
	}
	// Every suggestion is present in the human rendering too.
	for _, s := range doc.Suggestions {
		if !strings.Contains(collapseSpaces(human), collapseSpaces(s)) {
			t.Errorf("suggestion %q is in --json and not in the human rendering", s)
		}
	}
}

// collapseSpaces normalizes wrapped prose for comparison across renderings.
func collapseSpaces(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestEvalInspectHelpNamesWhatItDoesAndDoesNot: the help is the contract a
// user reads before they trust the command with an eval set.
func TestEvalInspectHelpNamesWhatItDoesAndDoesNot(t *testing.T) {
	t.Parallel()

	parent, _, code := run(t, "eval", "--help")
	if code != errs.ExitOK {
		t.Fatalf("`kno eval --help` exit %d", code)
	}
	if !strings.Contains(parent, "inspect") {
		t.Errorf("`kno eval --help` does not list inspect:\n%s", parent)
	}

	out, _, code := run(t, "eval", "inspect", "--help")
	if code != errs.ExitOK {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{
		"--evals",
		"--value-run-id",
		"makes no LLM call",
		"vendor's API",
		"exit\ncode is 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("`kno eval inspect --help` does not mention %q:\n%s", want, out)
		}
	}
}

// writeRun records a Run row directly.
//
// Direct rather than by driving `kno baseline` + `kno value`: the only free
// adapters are `fake:`, which answers every Case correctly so nothing fails
// and routing degrades to all-dev with no clusters, and `exec:`, which cannot
// carry an Asset in its context and so cannot complete a Value run at all.
// Neither can produce a plan with clusters, which is the shape these tests
// are about.
func writeRun(t *testing.T, dbPath string, run *knov1.Run) {
	t.Helper()

	ctx := context.Background()
	db, err := store.NewSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("opening %s: %v", dbPath, err)
	}
	defer func() { _ = db.Close() }()

	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatalf("recording run %s: %v", run.GetId(), err)
	}
}

// encodePlan gob-encodes a routing plan the way the Value stage persists it.
func encodePlan(t *testing.T, p value.Plan) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(p); err != nil {
		t.Fatalf("encoding the plan: %v", err)
	}
	return buf.Bytes()
}
