package cli

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/evals/split"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
)

// validationWith builds a Validation carrying one scripted interval, so the
// exit-code table can be driven without a store or an agent.
func validationWith(low, high float64, present bool) *core.Validation {
	v := &knov1.Validation{
		RunId:                "validate-1",
		SelectRunId:          "select-1",
		HoldoutCaseCount:     20,
		MeasuredCaseCount:    20,
		Trials:               1,
		HoldoutUseIndex:      1,
		DevEstimatedGain:     0.20,
		DevEstimatedInterval: &knov1.Interval{Low: 0.10, High: 0.30, Level: 0.95},
	}
	if !present {
		v.NotMeasured = knov1.RejectionReason_REJECTION_REASON_UNDERPOWERED
		v.Verdict = knov1.ValidationVerdict_VALIDATION_VERDICT_UNMEASURED
		return v
	}
	gain := (low + high) / 2
	v.HoldoutGain = &gain
	v.HoldoutInterval = &knov1.Interval{Low: low, High: high, Level: 0.95}
	switch {
	case low > 0:
		v.Verdict = knov1.ValidationVerdict_VALIDATION_VERDICT_CONFIRMED
	case high <= 0:
		v.Verdict = knov1.ValidationVerdict_VALIDATION_VERDICT_NOT_CONFIRMED
	default:
		v.Verdict = knov1.ValidationVerdict_VALIDATION_VERDICT_INCONCLUSIVE
	}
	return v
}

// TestValidateExitCodes pins §9's table exactly.
//
// The load-bearing row is the inconclusive one at exit 0. An interval crossing
// zero means "not enough evidence at this sample size", not "it failed", so
// blocking on it by default would make a 20-Case holdout block every deploy
// forever — and the thing people do when a gate blocks forever is pass
// --force, at which point the gate has stopped meaning anything. A gate that
// wants proof of gain asks for it.
func TestValidateExitCodes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		validation  *core.Validation
		status      knov1.RunStatus
		requireGain bool
		want        int
	}{
		{
			"confirmed", validationWith(0.01, 0.20, true),
			knov1.RunStatus_RUN_STATUS_COMPLETED, false, errs.ExitOK,
		},
		{
			"confirmed with --require-gain", validationWith(0.01, 0.20, true),
			knov1.RunStatus_RUN_STATUS_COMPLETED, true, errs.ExitOK,
		},
		{
			"inconclusive is not a failure", validationWith(-0.05, 0.20, true),
			knov1.RunStatus_RUN_STATUS_COMPLETED, false, errs.ExitOK,
		},
		{
			"inconclusive with --require-gain", validationWith(-0.05, 0.20, true),
			knov1.RunStatus_RUN_STATUS_COMPLETED, true, errs.ExitValidationFailed,
		},
		{
			"not confirmed blocks unconditionally", validationWith(-0.20, -0.01, true),
			knov1.RunStatus_RUN_STATUS_COMPLETED, false, errs.ExitValidationFailed,
		},
		{
			"a high bound at exactly zero is a demonstrated non-improvement",
			validationWith(-0.20, 0, true),
			knov1.RunStatus_RUN_STATUS_COMPLETED, false, errs.ExitValidationFailed,
		},
		{
			"unmeasured", validationWith(0, 0, false),
			knov1.RunStatus_RUN_STATUS_COMPLETED, false, errs.ExitOK,
		},
		{
			"unmeasured with --require-gain", validationWith(0, 0, false),
			knov1.RunStatus_RUN_STATUS_COMPLETED, true, errs.ExitValidationFailed,
		},
		{
			"a budget stop is resumable, not a validation failure", nil,
			knov1.RunStatus_RUN_STATUS_BUDGET_STOPPED, false, errs.ExitBudgetStopped,
		},
		{
			"an interruption is resumable, not a validation failure", nil,
			knov1.RunStatus_RUN_STATUS_INTERRUPTED, false, errs.ExitInterrupted,
		},
		{
			"nothing to validate", nil,
			knov1.RunStatus_RUN_STATUS_COMPLETED, false, errs.ExitOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateExit(&core.ValidateResult{
				RunID:      "validate-1",
				Status:     tc.status,
				Validation: tc.validation,
			}, tc.requireGain)
			got := errs.ExitOK
			if err != nil {
				var act *errs.Actionable
				if !errors.As(err, &act) {
					t.Fatalf("the refusal is not Actionable: %v", err)
				}
				got = act.ExitCode
			}
			if got != tc.want {
				t.Errorf("exit = %d, want %d (err: %v)", got, tc.want, err)
			}
		})
	}
}

// TestValidateHumanAndJSONAgree pins the two renderings to the same content.
//
// The convention the report/TUI plan established, and it matters most here:
// the holdout gain is the one number in this product people quote out of
// context, and the human page and the --json document disagreeing about it
// would be worse than either being absent.
func TestValidateHumanAndJSONAgree(t *testing.T) {
	t.Parallel()

	res := &core.ValidateResult{
		RunID:           "validate-1",
		Status:          knov1.RunStatus_RUN_STATUS_COMPLETED,
		GoalDirection:   knov1.Direction_DIRECTION_MAXIMIZE,
		Validation:      validationWith(0.0210, 0.1490, true),
		HoldoutCases:    34,
		HoldoutUseIndex: 1,
		AssetCount:      3,
		Spent:           budget.Spend{Calls: 68, CostUSDMicros: 1_500_000},
	}
	res.Validation.HoldoutCaseCount = 34
	res.Validation.MeasuredCaseCount = 34
	quote := core.ValidateQuote{
		HoldoutCases: 34, Arms: 2, Trials: 1, Calls: 68, AssetCount: 3,
	}
	f := validateFlags{selectRunID: "select-1"}

	var human bytes.Buffer
	if err := renderValidate(&human, f, res, quote, split.Counts{Dev: 136, Holdout: 34}, budget.Spend{}); err != nil {
		t.Fatalf("human render: %v", err)
	}
	f.jsonOut = true
	var machine bytes.Buffer
	if err := renderValidate(&machine, f, res, quote, split.Counts{Dev: 136, Holdout: 34}, budget.Spend{}); err != nil {
		t.Fatalf("json render: %v", err)
	}

	// Exactly one document and no prose on stdout.
	doc, err := decodeExactlyOneDocument(machine.Bytes())
	if err != nil {
		t.Fatalf("the --json output is not one JSON document: %v\n%s", err, machine.String())
	}

	// Enum-valued keys carry NAMES, not numbers (ADR-0006 rule 3).
	if got := doc["verdict"]; got != "confirmed" {
		t.Errorf("verdict = %v, want the name \"confirmed\"", got)
	}
	if got := doc["status"]; got != "completed" {
		t.Errorf("status = %v, want the name \"completed\"", got)
	}
	// The spend block is present because validate runs a guard, and guarded is
	// what distinguishes a metered zero from a missing meter.
	if doc["guarded"] != true {
		t.Error("guarded is not true; validate runs a budget guard on every run")
	}
	if _, ok := doc["spent_usd_micros"]; !ok {
		t.Error("spent_usd_micros is missing; the display string cannot be summed")
	}

	// The same numbers, in both renderings.
	humanText := human.String()
	for _, want := range []string{
		"+0.0850", "+0.0210", "+0.1490", "confirmed",
		"68 calls (34 holdout Cases x 2 arms x 1 trial(s))",
		"this holdout has measured 1 portfolio",
	} {
		if !strings.Contains(humanText, want) {
			t.Errorf("the human page does not contain %q\n%s", want, humanText)
		}
	}
	if gain, _ := doc["holdout_gain"].(float64); gain < 0.0849 || gain > 0.0851 {
		t.Errorf("holdout_gain = %v, want the same +0.0850 the page prints", doc["holdout_gain"])
	}
	if doc["planned_calls"].(float64) != 68 {
		t.Errorf("planned_calls = %v, want 68 — the same figure the page shows", doc["planned_calls"])
	}
}

// TestNoGainWithoutItsIntervalInJSON pins the absence rule on the one document
// whose top-level number is a claim about the world.
//
// holdout_gain, low and high are present together or not at all, and
// not_measured is the positive signal beside their absence — because a
// consumer reading `.holdout_gain // 0` cannot tell a missing key from a
// measured zero, and this is the key where that matters most.
func TestNoGainWithoutItsIntervalInJSON(t *testing.T) {
	t.Parallel()

	res := &core.ValidateResult{
		RunID: "validate-1", Status: knov1.RunStatus_RUN_STATUS_COMPLETED,
		GoalDirection: knov1.Direction_DIRECTION_MAXIMIZE,
		Validation:    validationWith(0, 0, false),
		HoldoutCases:  1,
	}
	var out bytes.Buffer
	f := validateFlags{selectRunID: "select-1"}
	f.jsonOut = true
	if err := renderValidate(&out, f, res, core.ValidateQuote{HoldoutCases: 1, Arms: 2, Trials: 1, Calls: 2},
		split.Counts{}, budget.Spend{}); err != nil {
		t.Fatalf("render: %v", err)
	}
	doc, err := decodeExactlyOneDocument(out.Bytes())
	if err != nil {
		t.Fatalf("not one valid json document: %v", err)
	}
	for _, key := range []string{"holdout_gain", "low", "high"} {
		if _, ok := doc[key]; ok {
			t.Errorf("%s is present with no interval; a gain must never ship without one", key)
		}
	}
	if doc["not_measured"] != "underpowered" {
		t.Errorf("not_measured = %v, want \"underpowered\" beside the absent gain", doc["not_measured"])
	}
	if doc["verdict"] != "unmeasured" {
		t.Errorf("verdict = %v, want \"unmeasured\"", doc["verdict"])
	}
}

// TestValidatePassesTheSplitPackagesMinHoldout is what makes criterion 16's
// "changing the constant changes the verdict" true.
//
// core cannot import adapters/evals/split — prime directive 3 — so the
// threshold travels from the package that owns the split, through
// ValidateOptions.MinHoldout. This asserts the CLI passes the constant itself
// rather than a copy of its current value, which is the difference between a
// threshold with one definition and two numbers that agree today.
func TestValidatePassesTheSplitPackagesMinHoldout(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("validate.go")
	if err != nil {
		t.Fatalf("reading cli/validate.go: %v", err)
	}
	if !strings.Contains(string(src), "MinHoldout:              split.MinHoldout,") {
		t.Error("cli/validate.go does not pass split.MinHoldout into ValidateOptions. " +
			"core cannot import the split package, so a literal here would be a second " +
			"definition of the threshold that agrees with the first only until someone " +
			"changes one of them.")
	}
}
