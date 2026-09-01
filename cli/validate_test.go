package cli_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/store"
)

// TestValidateHelpIsSnapshotted keeps the stage's front door under the same
// review discipline as every other command's.
//
// Two of these assertions are load-bearing rather than decorative. DESIGN.md
// requires the help to say this is holdout confirmation and NOT schema
// validation — the word "validate" means something else in every other CLI a
// user has run. And prime directive 4 requires the doubling to be visible
// before the run, not discovered on the invoice.
func TestValidateHelpIsSnapshotted(t *testing.T) {
	t.Parallel()

	stdout, _, code := run(t, "validate", "--help")
	if code != errs.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{
		"HOLDOUT CONFIRMATION, not schema validation",
		"--select-run-id",
		"--allow-repeat-holdout",
		"--require-gain",
		"--context-only",
		"Two arms",
		"CONSUMED ONCE PER PORTFOLIO",
		"upper bound on what retrieval would deliver",
		"--resume",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help no longer mentions %q:\n%s", want, stdout)
		}
	}
}

// TestValidateRefusesMissingRequirements: required inputs are refused rather
// than defaulted. Validate measures a Portfolio against a holdout; a run
// missing either has nothing to do and must not invent one.
func TestValidateRefusesMissingRequirements(t *testing.T) {
	t.Parallel()

	if _, _, code := run(t, "validate"); code == errs.ExitOK {
		t.Error("validate ran with no flags at all")
	}
	if _, _, code := run(t, "validate", "--evals", writeCases(t, 5)); code == errs.ExitOK {
		t.Error("validate ran without --pool or --select-run-id")
	}
}

// validatePipeline drives baseline -> value -> select on the fake agent and
// returns the paths and the Select run ID validate needs.
func validatePipeline(t *testing.T) (casesPath, poolPath, db, valueRunID, selectRunID string) {
	t.Helper()

	casesPath = writeCases(t, 60)
	poolPath = writePool(t, 2)
	db = filepath.Join(t.TempDir(), "kno.db")

	baseOut, baseErr, code := run(t, "baseline", "--evals", casesPath,
		"--agent", "fake:", "--db", db, "--yes")
	if code != errs.ExitOK {
		t.Fatalf("baseline exit = %d\n%s", code, baseErr)
	}
	baseRunID := runIDFrom(t, baseOut, "Baseline ")

	valueOut, valueErr, code := run(t, "value", "--evals", casesPath, "--pool", poolPath,
		"--baseline-run-id", baseRunID, "--agent", "fake:", "--db", db, "--yes")
	if code != errs.ExitOK {
		t.Fatalf("value exit = %d\n%s", code, valueErr)
	}
	valueRunID = runIDFrom(t, valueOut, "Value run ")

	selectOut, selectErr, code := run(t, "select", "--value-run-id", valueRunID,
		"--pool", poolPath, "--max-context-tokens", "1000000", "--db", db)
	if code != errs.ExitOK {
		t.Fatalf("select exit = %d\n%s", code, selectErr)
	}
	return casesPath, poolPath, db, valueRunID, runIDFrom(t, selectOut, "Select run ")
}

// TestValidateEndToEndAgainstFake drives the real command through the real
// pipeline.
//
// IT EXERCISES THE EMPTY-PORTFOLIO PATH, and that is a fact about `fake:`
// rather than a gap in the test: the fake answers every Case with what the
// Case expects, so injecting an Asset cannot change a deterministic answer,
// every corrected interval crosses zero, and Select correctly includes
// nothing. `kno demo` prints exactly this explanation. The MEASURED path is
// driven end to end in core (TestValidateMeasuresBothArmsOverTheHoldout,
// which scripts an agent whose arms differ) and its two renderings are pinned
// in cli/validate_internal_test.go.
//
// What this asserts is the part only the command can be wrong about: an empty
// Portfolio exits 0, makes no agent call, and — the load-bearing half — does
// NOT consume the holdout. A stage that opened the holdout to discover there
// was nothing to measure would have spent the one thing it cannot get back.
func TestValidateEndToEndAgainstFake(t *testing.T) {
	t.Parallel()

	casesPath, poolPath, db, _, selectRunID := validatePipeline(t)

	stdout, stderr, code := run(t, "validate", "--evals", casesPath, "--pool", poolPath,
		"--select-run-id", selectRunID, "--agent", "fake:", "--db", db, "--yes")
	if code != errs.ExitOK {
		t.Fatalf("validate exit = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, want := range []string{
		"nothing to validate",
		"the holdout was not opened",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the validate page does not mention %q:\n%s", want, stdout)
		}
	}

	// Run it again: with no holdout consumed, a second run is not a repeat.
	// The one-shot refusal must key on a RECORDED peek, not on the command
	// having been typed before.
	_, stderr2, code2 := run(t, "validate", "--evals", casesPath, "--pool", poolPath,
		"--select-run-id", selectRunID, "--agent", "fake:", "--db", db, "--yes")
	if code2 != errs.ExitOK {
		t.Errorf("a second validate of an empty Portfolio exited %d (%s); nothing was "+
			"consumed the first time, so there is nothing to refuse", code2, stderr2)
	}
}

// TestValidateRefusesBeforeSpend drives the free refusals through the command,
// where the user meets them.
func TestValidateRefusesBeforeSpend(t *testing.T) {
	t.Parallel()

	casesPath, poolPath, db, valueRunID, selectRunID := validatePipeline(t)

	for _, tc := range []struct {
		name    string
		args    []string
		wantFix string
	}{
		{
			name: "an unknown select run",
			args: []string{
				"validate", "--evals", casesPath, "--pool", poolPath,
				"--select-run-id", "no-such-run", "--db", db, "--yes",
			},
			wantFix: "pass the run ID of a `kno select` run",
		},
		{
			name: "a run of the wrong stage",
			args: []string{
				"validate", "--evals", casesPath, "--pool", poolPath,
				"--select-run-id", valueRunID, "--db", db, "--yes",
			},
			wantFix: "pass the run ID of a `kno select` run",
		},
		{
			name: "an agent that cannot carry a whole Portfolio",
			args: []string{
				"validate", "--evals", casesPath, "--pool", poolPath,
				"--select-run-id", selectRunID, "--agent", "nonsense:", "--db", db, "--yes",
			},
			wantFix: "write the reference as scheme:target",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := run(t, tc.args...)
			if code == errs.ExitOK {
				t.Fatalf("the command succeeded; this refusal is supposed to be free")
			}
			if !strings.Contains(stderr, tc.wantFix) {
				t.Errorf("stderr missing fix %q:\n%s", tc.wantFix, stderr)
			}
		})
	}
}

// TestValidateJSONDocument pins the machine surface end to end.
func TestValidateJSONDocument(t *testing.T) {
	t.Parallel()

	casesPath, poolPath, db, _, selectRunID := validatePipeline(t)

	stdout, stderr, code := run(t, "validate", "--evals", casesPath, "--pool", poolPath,
		"--select-run-id", selectRunID, "--agent", "fake:", "--db", db, "--json", "--yes")
	if code != errs.ExitOK {
		t.Fatalf("validate --json exit = %d\nstderr: %s", code, stderr)
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Fatalf("--json emitted prose on stdout:\n%s", stdout)
	}
	for _, want := range []string{
		`"guarded": true`,
		`"spent_usd_micros"`,
		`"verdict"`,
		`"arms": 2`,
		`"interaction_penalty_detected": false`,
		`"min_holdout": 20`,
		`"nothing_to_validate": true`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the --json document does not carry %s:\n%s", want, stdout)
		}
	}
}

// TestReportCaveatDisappearsOnlyForACompletedValidate is the honesty gate on
// the page most people read.
//
// The Validation is seeded directly into the store rather than produced by a
// `kno validate` run, because `fake:` cannot produce a non-empty Portfolio to
// validate (see TestValidateEndToEndAgainstFake). `kno report` is a read-only
// composer over recorded aggregates, so a seeded record is exactly what it
// would read from a real run — and what is under test here is the report's
// rendering rule, not validate's measurement.
func TestReportCaveatDisappearsOnlyForACompletedValidate(t *testing.T) {
	t.Parallel()

	_, _, db, valueRunID, selectRunID := validatePipeline(t)
	// `fake:` selects nothing, so the Portfolio carries no dev estimate and
	// v0.1 printed no caveat for it. The caveat rule under test is about a
	// Portfolio that HAS a number, so one is recorded here.
	seedDevEstimate(t, db, selectRunID)

	before, _, code := run(t, "report", "--value-run-id", valueRunID,
		"--select-run-id", selectRunID, "--db", db)
	if code != errs.ExitOK {
		t.Fatalf("report exit = %d", code)
	}
	if !strings.Contains(before, "not yet validated on holdout") {
		t.Errorf("the caveat is missing before any validation:\n%s", before)
	}

	seedValidation(t, db, selectRunID, true)

	after, _, code := run(t, "report", "--value-run-id", valueRunID,
		"--select-run-id", selectRunID, "--validate-run-id", seededValidateRunID, "--db", db)
	if code != errs.ExitOK {
		t.Fatalf("report exit = %d", code)
	}
	if strings.Contains(after, "not yet validated on holdout") {
		t.Errorf("the caveat survived a completed validation:\n%s", after)
	}
	for _, want := range []string{"holdout gain", "shrinkage", "this holdout has measured 1 portfolio"} {
		if !strings.Contains(after, want) {
			t.Errorf("the page does not carry %q:\n%s", want, after)
		}
	}

	jsonOut, _, code := run(t, "report", "--value-run-id", valueRunID,
		"--select-run-id", selectRunID, "--validate-run-id", seededValidateRunID,
		"--db", db, "--json")
	if code != errs.ExitOK {
		t.Fatalf("report --json exit = %d", code)
	}
	if !strings.Contains(jsonOut, `"validated_on_holdout": true`) {
		t.Errorf("validated_on_holdout is not computed:\n%s", jsonOut)
	}
	if !strings.Contains(jsonOut, `"validation"`) {
		t.Errorf("the report document carries no validation object:\n%s", jsonOut)
	}
}

// TestAnInterruptedValidateDoesNotRemoveTheCaveat.
//
// A partial peek is not a validation. The page keeps the caveat and ADDS a
// line saying an attempt was made and produced no number — the failure mode
// this guards against is a page that reads "validated" because somebody
// STARTED a validate.
func TestAnInterruptedValidateDoesNotRemoveTheCaveat(t *testing.T) {
	t.Parallel()

	_, _, db, valueRunID, selectRunID := validatePipeline(t)
	seedDevEstimate(t, db, selectRunID)
	seedValidation(t, db, selectRunID, false)

	page, _, code := run(t, "report", "--value-run-id", valueRunID,
		"--select-run-id", selectRunID, "--validate-run-id", seededValidateRunID, "--db", db)
	if code != errs.ExitOK {
		t.Fatalf("report exit = %d", code)
	}
	if !strings.Contains(page, "not yet validated on holdout") {
		t.Errorf("an interrupted validation removed the caveat:\n%s", page)
	}
	if !strings.Contains(page, "a validation was attempted") {
		t.Errorf("the page does not say a validation was attempted:\n%s", page)
	}

	jsonOut, _, code := run(t, "report", "--value-run-id", valueRunID,
		"--select-run-id", selectRunID, "--validate-run-id", seededValidateRunID,
		"--db", db, "--json")
	if code != errs.ExitOK {
		t.Fatalf("report --json exit = %d", code)
	}
	if !strings.Contains(jsonOut, `"validated_on_holdout": false`) {
		t.Errorf("validated_on_holdout is true for an interrupted run:\n%s", jsonOut)
	}
	if !strings.Contains(jsonOut, `"not_recorded": true`) {
		t.Errorf("the validation object does not say the run recorded nothing:\n%s", jsonOut)
	}
}

// TestReportRefusesAMismatchedValidateRun: a holdout number measured against a
// different Portfolio than the one the page shows is two stories spliced into
// one page, and this is the one number people quote out of context.
func TestReportRefusesAMismatchedValidateRun(t *testing.T) {
	t.Parallel()

	_, _, db, valueRunID, selectRunID := validatePipeline(t)
	seedValidation(t, db, selectRunID, true)

	_, stderr, code := run(t, "report", "--value-run-id", valueRunID,
		"--validate-run-id", selectRunID, "--db", db)
	if code == errs.ExitOK {
		t.Error("report accepted a Select run as a Validate run")
	}
	if !strings.Contains(stderr, "pass the run ID of a `kno validate` run") {
		t.Errorf("the fix does not name the right run kind:\n%s", stderr)
	}

	_, stderr, code = run(t, "report", "--value-run-id", valueRunID,
		"--validate-run-id", seededValidateRunID, "--db", db)
	if code == errs.ExitOK {
		t.Error("report accepted a validation of a Portfolio it is not showing")
	}
	if !strings.Contains(stderr, "pass --select-run-id of the Portfolio that was validated") {
		t.Errorf("the fix does not name the mismatch:\n%s", stderr)
	}
}

// seedDevEstimate records a dev-slice gain on an existing Portfolio.
//
// `fake:` answers every Case with what the Case expects, so no Asset can
// measure an effect and Select correctly includes nothing — which leaves the
// Portfolio with no dev estimate for the holdout block to sit beside. The
// number here stands in for a Select run against a real agent.
func seedDevEstimate(t *testing.T, dbPath, selectRunID string) {
	t.Helper()

	ctx := context.Background()
	db, err := store.NewSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	defer func() { _ = db.Close() }()

	p, err := db.Portfolio(ctx, selectRunID)
	if err != nil {
		t.Fatalf("loading the portfolio: %v", err)
	}
	p.DevEstimatedGain = 0.1420
	p.DevEstimatedInterval = &knov1.Interval{Low: 0.0910, High: 0.1930, Level: 0.95}
	if err := db.WritePortfolio(ctx, selectRunID, p); err != nil {
		t.Fatalf("writing the portfolio: %v", err)
	}
}

// seededValidateRunID is the run ID every seeded Validation carries.
const seededValidateRunID = "seeded-validate-run"

// seedValidation writes a Validate run into an existing pipeline database, and
// optionally the Validation it produced.
//
// `withNumber` false is the interrupted case: the Run exists and no Validation
// does, which is exactly the state core.Validate leaves behind when it stops
// early — it writes the Validation only on a COMPLETED run.
func seedValidation(t *testing.T, dbPath, selectRunID string, withNumber bool) {
	t.Helper()

	ctx := context.Background()
	db, err := store.NewSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	defer func() { _ = db.Close() }()

	status := knov1.RunStatus_RUN_STATUS_COMPLETED
	if !withNumber {
		status = knov1.RunStatus_RUN_STATUS_INTERRUPTED
	}
	if err := db.CreateRun(ctx, &knov1.Run{
		Id:            seededValidateRunID,
		Stage:         knov1.Stage_STAGE_VALIDATE,
		Status:        status,
		GoalName:      "exact-match",
		GoalDirection: knov1.Direction_DIRECTION_MAXIMIZE,
	}); err != nil {
		t.Fatalf("creating the validate run: %v", err)
	}
	if !withNumber {
		return
	}
	gain := 0.0850
	if err := db.WriteValidation(ctx, seededValidateRunID, &knov1.Validation{
		RunId:                seededValidateRunID,
		SelectRunId:          selectRunID,
		HoldoutCaseCount:     34,
		MeasuredCaseCount:    34,
		Trials:               1,
		HoldoutUseIndex:      1,
		HoldoutGain:          &gain,
		HoldoutInterval:      &knov1.Interval{Low: 0.0210, High: 0.1490, Level: 0.95},
		DevEstimatedGain:     0.1420,
		DevEstimatedInterval: &knov1.Interval{Low: 0.0910, High: 0.1930, Level: 0.95},
		Verdict:              knov1.ValidationVerdict_VALIDATION_VERDICT_CONFIRMED,
	}); err != nil {
		t.Fatalf("writing the validation: %v", err)
	}
}
