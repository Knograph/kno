package cli_test

import (
	"bytes"
	"context"
	"encoding/gob"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/evals/jsonl"
	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/store"
)

// observedFixture writes a tagged eval file and a recorded Value run over it,
// and returns the eval path and the database path.
//
// The run is recorded directly rather than driven through `kno baseline` and
// `kno value`: the observed section reads a plan, a fingerprint and a set of
// Valuations, and a fixture that states those exactly is what makes the
// UNKNOWN branches — an absent plan, an undecodable one, a moved fingerprint —
// testable at all.
func observedFixture(t *testing.T, mutate func(run *knov1.Run)) (evalsPath, dbPath string) {
	t.Helper()

	dir := t.TempDir()
	dev, hold := devHoldoutIDs(t, 0.2, 20, 25)
	cases := make([]inspectCase, 0, len(dev))
	for n, id := range dev {
		cases = append(cases, inspectCase{
			ID: id, Tags: []string{[]string{"billing", "refunds"}[n%2]}, Expected: "a",
		})
	}
	evalsPath = writeInspectCases(t, dir, "cases.jsonl", append(cases, tagged(hold, "billing")...))

	src, err := jsonl.New(jsonl.Options{Path: evalsPath})
	if err != nil {
		t.Fatalf("opening the fixture: %v", err)
	}
	ctx := context.Background()
	hash, err := src.ContentHash(ctx)
	if err != nil {
		t.Fatalf("fingerprinting the fixture: %v", err)
	}

	// billing's cluster is well covered by ctx-a, whose interval excludes zero
	// (IMPROVED). refunds' cluster is routed to nothing, so it stays UNKNOWN —
	// which is exactly the state this check exists to surface.
	plan := &value.Plan{
		Mode:                value.ModeTagOverlap,
		EligibleCases:       18,
		ControlCaseIDs:      dev[:2],
		ControlUnderpowered: true,
		MinDetectableHarm:   0.42,
		Clusters: []value.ClusterSnapshot{
			{Tag: "billing", CaseIDs: dev[0:8]},
			{Tag: "refunds", CaseIDs: dev[8:14]},
		},
		Routed: []value.AssetRouting{{AssetID: "ctx-a", CaseIDs: dev[0:8]}},
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(plan); err != nil {
		t.Fatalf("encoding the plan: %v", err)
	}
	run := &knov1.Run{
		Id:              "val-1",
		Stage:           knov1.Stage_STAGE_VALUE,
		Status:          knov1.RunStatus_RUN_STATUS_COMPLETED,
		BaselineRunId:   "base-1",
		EvalContentHash: hash,
		ValuePlan:       buf.Bytes(),
	}
	if mutate != nil {
		mutate(run)
	}

	dbPath = filepath.Join(dir, "kno.db")
	db, err := store.NewSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatalf("recording the run: %v", err)
	}
	npairs := int32(8)
	if err := db.WriteValuation(ctx, "val-1", &knov1.Valuation{
		AssetId:   "ctx-a",
		DeltaGoal: 0.5,
		DeltaInterval: &knov1.Interval{
			Low: 0.2, High: 0.8, Level: 0.95, Method: "t",
			Sidedness: knov1.Sidedness_SIDEDNESS_TWO_SIDED, NPairs: &npairs,
		},
	}); err != nil {
		t.Fatalf("recording the valuation: %v", err)
	}
	return evalsPath, dbPath
}

// TestInspectReportsWhatARunAttributed is criterion 10, and criterion 24's
// sidedness separation.
func TestInspectReportsWhatARunAttributed(t *testing.T) {
	t.Parallel()

	evalsPath, dbPath := observedFixture(t, nil)
	doc := inspectJSON(t, "--evals", evalsPath, "--db", dbPath, "--value-run-id", "val-1")

	o := doc.Observed
	if o == nil {
		t.Fatalf("the observed section is absent: %v", doc.Checks)
	}
	if o.RoutingMode != "tag-overlap" {
		t.Errorf("routing_mode = %q, want tag-overlap", o.RoutingMode)
	}
	if !o.ControlUnderpowered || o.MinDetectableHarm != 0.42 {
		t.Errorf("the control arm is reported as %+v, not the plan's own record", o)
	}
	if o.MinDetectableHarmSidedness != "one-sided" {
		t.Errorf("min_detectable_harm_sidedness = %q, want one-sided", o.MinDetectableHarmSidedness)
	}
	for _, b := range doc.Behaviors {
		if b.Sidedness != "two-sided" {
			t.Errorf("behavior %q reports sidedness %q, want two-sided", b.Tag, b.Sidedness)
		}
	}
	if len(o.Behaviors) != 2 {
		t.Fatalf("the observed section has %d behaviors, want one per plan cluster", len(o.Behaviors))
	}
	byTag := map[string]string{}
	for _, b := range o.Behaviors {
		byTag[b.Tag] = b.GapStatus
	}
	if byTag["billing"] != "improved" {
		t.Errorf("billing's verdict is %q, want improved", byTag["billing"])
	}
	if byTag["refunds"] != "unknown" {
		t.Errorf("refunds' verdict is %q, want unknown — nothing was routed to it", byTag["refunds"])
	}
	if got := checkStatus(t, doc, "attribution_observed"); got != "flagged" {
		t.Errorf("attribution_observed = %q, want flagged: the control arm is underpowered "+
			"and one behavior got no verdict", got)
	}

	stdout, _, code := run(t, "eval", "inspect", "--evals", evalsPath, "--db", dbPath,
		"--value-run-id", "val-1")
	if code != errs.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"ONE-sided 95%", "routing mode tag-overlap", "Observed  value run val-1"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the observed block does not say %q:\n%s", want, stdout)
		}
	}
}

// TestInspectWithholdsAStalePlan is criterion 11: the eval source moved, so the
// current tag structure must not be joined to a stale plan. Never silently, and
// still exit 0.
func TestInspectWithholdsAStalePlan(t *testing.T) {
	t.Parallel()

	evalsPath, dbPath := observedFixture(t, func(run *knov1.Run) {
		run.EvalContentHash = "sha256:not-this-file"
	})
	doc := inspectJSON(t, "--evals", evalsPath, "--db", dbPath, "--value-run-id", "val-1")
	if doc.Observed != nil {
		t.Errorf("a stale plan was joined to the current tags: %+v", doc.Observed)
	}
	if got := checkStatus(t, doc, "attribution_observed"); got != "unknown" {
		t.Errorf("attribution_observed = %q, want unknown", got)
	}
	stdout, _, code := run(t, "eval", "inspect", "--evals", evalsPath, "--db", dbPath,
		"--value-run-id", "val-1")
	if code != errs.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "the eval source has changed since this run") {
		t.Errorf("the page does not say why the observed section is missing:\n%s", stdout)
	}
}

// TestInspectHandlesAnAbsentOrBrokenPlan is criterion 12: no guess, no panic,
// exit 0, and a detail that names the absence.
func TestInspectHandlesAnAbsentOrBrokenPlan(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*knov1.Run){
		"absent":      func(r *knov1.Run) { r.ValuePlan = nil },
		"undecodable": func(r *knov1.Run) { r.ValuePlan = []byte("not a gob stream at all") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			evalsPath, dbPath := observedFixture(t, mutate)
			doc := inspectJSON(t, "--evals", evalsPath, "--db", dbPath, "--value-run-id", "val-1")
			if doc.Observed != nil {
				t.Errorf("an %s plan produced an observed section: %+v", name, doc.Observed)
			}
			if got := checkStatus(t, doc, "attribution_observed"); got != "unknown" {
				t.Errorf("attribution_observed = %q, want unknown", got)
			}
			var detail string
			for _, c := range doc.Checks {
				if c.Name == "attribution_observed" {
					detail = c.Detail
				}
			}
			if detail == "" {
				t.Errorf("the unknown verdict names no reason")
			}
		})
	}
}

// TestInspectRefusesABadRunReference is criterion 13: an unknown run, a run of
// the wrong stage, and an unreadable database are refused with a fix line.
func TestInspectRefusesABadRunReference(t *testing.T) {
	t.Parallel()

	evalsPath, dbPath := observedFixture(t, nil)

	t.Run("unknown_run", func(t *testing.T) {
		t.Parallel()
		_, stderr, code := run(t, "eval", "inspect", "--evals", evalsPath, "--db", dbPath,
			"--value-run-id", "no-such-run")
		if code != errs.ExitError {
			t.Fatalf("exit = %d, want %d", code, errs.ExitError)
		}
		if !strings.Contains(stderr, "kno value") {
			t.Errorf("the refusal does not say where run IDs come from: %s", stderr)
		}
	})

	t.Run("wrong_stage", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		db, err := store.NewSQLite(ctx, dbPath)
		if err != nil {
			t.Fatalf("opening the store: %v", err)
		}
		defer func() { _ = db.Close() }()
		if err := db.CreateRun(ctx, &knov1.Run{
			Id: "base-only", Stage: knov1.Stage_STAGE_BASELINE,
			Status: knov1.RunStatus_RUN_STATUS_COMPLETED,
		}); err != nil {
			t.Fatalf("recording the baseline run: %v", err)
		}
		_, stderr, code := run(t, "eval", "inspect", "--evals", evalsPath, "--db", dbPath,
			"--value-run-id", "base-only")
		if code != errs.ExitError {
			t.Fatalf("exit = %d, want %d", code, errs.ExitError)
		}
		if !strings.Contains(stderr, "not a value run") {
			t.Errorf("the refusal does not name the run's actual stage: %s", stderr)
		}
	})

	t.Run("unreadable_db", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		bad := filepath.Join(dir, "not-a-db")
		if err := os.Mkdir(bad, 0o750); err != nil {
			t.Fatalf("creating the directory: %v", err)
		}
		_, stderr, code := run(t, "eval", "inspect", "--evals", evalsPath, "--db", bad,
			"--value-run-id", "val-1")
		if code != errs.ExitError {
			t.Fatalf("exit = %d, want %d", code, errs.ExitError)
		}
		if !strings.Contains(stderr, "--db") {
			t.Errorf("the refusal does not name --db: %s", stderr)
		}
	})
}

// TestInspectObservedCarriesTheHarmSidednessNote is the other half of criterion
// 24: a jq consumer reading both numbers is told, in the document, that they
// answer different questions.
func TestInspectObservedCarriesTheHarmSidednessNote(t *testing.T) {
	t.Parallel()

	evalsPath, dbPath := observedFixture(t, nil)
	doc := inspectJSON(t, "--evals", evalsPath, "--db", dbPath, "--value-run-id", "val-1")
	var found bool
	for _, n := range doc.Notes {
		if strings.Contains(n, "min_detectable_harm") && strings.Contains(n, "ONE-sided") {
			found = true
		}
	}
	if !found {
		t.Errorf("the document does not separate the two bounds: %v", doc.Notes)
	}
}

// TestInspectMakesNoLLMCall is criterion 16. The fake agent counts its calls,
// and inspect never constructs one — so the assertion that matters is the
// negative: no provider credential is needed, and a jsonl source is read with
// no agent flag on the command at all.
func TestInspectMakesNoLLMCall(t *testing.T) {
	t.Parallel()

	dev, hold := devHoldoutIDs(t, 0.2, 10, 25)
	path := writeInspectCases(t, t.TempDir(), "cases.jsonl",
		append(tagged(dev, "billing"), tagged(hold, "billing")...))

	if _, _, code := run(t, "eval", "inspect", "--evals", path); code != errs.ExitOK {
		t.Fatalf("exit = %d with no provider configured, want 0", code)
	}
	// The command must not accept an agent at all: a flag that took one would
	// be a spend path nobody had gated.
	_, _, code := run(t, "eval", "inspect", "--evals", path, "--agent", "fake:")
	if code == errs.ExitOK {
		t.Errorf("`kno eval inspect` accepted --agent; it must construct no Agent")
	}
}
