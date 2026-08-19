package errs_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// TestSentinelIdentitySurvivesTheWire is the most important test in this
// package.
//
// errors.Is compares by pointer equality unless a type provides Is. An
// Actionable rebuilt from proto bytes — out of a plugin subprocess, a
// checkpoint, or an API response — is a different Go value than the sentinel,
// so without Actionable.Is this returns false and any code branching on it
// silently misclassifies the error.
//
// The two kinds whose misclassification matters most are exactly these:
// mistaking a budget stop for a generic failure loses a resumable run, and
// mistaking a sealed-holdout error for anything else is a statistical-integrity
// failure that would not be visible in the output.
func TestSentinelIdentitySurvivesTheWire(t *testing.T) {
	t.Parallel()

	for _, sentinel := range []*errs.Actionable{
		errs.ErrBudgetExceeded,
		errs.ErrHoldoutSealed,
		errs.ErrCapabilityUnsupported,
		errs.ErrCheckpointStale,
		errs.ErrRateLimited,
	} {
		t.Run(sentinel.Code, func(t *testing.T) {
			t.Parallel()

			rebuilt := errs.FromProto(sentinel.Proto())

			if !errors.Is(rebuilt, sentinel) {
				t.Errorf("errors.Is failed after a proto round trip for %s.\n"+
					"Code branching on this sentinel would misclassify the error.", sentinel.Code)
			}
			if errors.Is(rebuilt, errs.ErrCheckpointStale) && sentinel.Code != "CHECKPOINT_STALE" {
				t.Errorf("%s matched an unrelated sentinel; Is is too permissive", sentinel.Code)
			}
		})
	}
}

// TestIsDoesNotMatchUnrelatedErrors guards the other direction: an Is that
// returns true too readily is worse than none, because it routes real failures
// into a handler written for something else.
func TestIsDoesNotMatchUnrelatedErrors(t *testing.T) {
	t.Parallel()

	// Wrapped so the first argument reads as an error under test rather than a
	// target, which is both clearer and what errors.Is expects.
	budget := fmt.Errorf("value stage: %w", errs.ErrBudgetExceeded)

	if errors.Is(budget, errs.ErrHoldoutSealed) {
		t.Error("two distinct sentinels compared equal")
	}
	if errors.Is(budget, errors.New("budget exceeded")) {
		t.Error("an Actionable matched a plain error with the same text")
	}
}

// TestWrapPreservesSentinelAndChain checks that wrapping a cause keeps both
// properties callers depend on: the sentinel still matches, and the underlying
// error is still reachable.
func TestWrapPreservesSentinelAndChain(t *testing.T) {
	t.Parallel()

	cause := errors.New("connection refused")
	err := errs.ErrBudgetExceeded.Wrap(cause)

	if !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Error("wrapping lost the sentinel")
	}
	if !errors.Is(err, cause) {
		t.Error("wrapping lost the underlying cause")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("rendered error omits the upstream text: %q", err.Error())
	}
}

// TestWrapDoesNotMutateTheSentinel guards a bug that would be invisible until
// it produced a wrong error message far from its cause: if Wrap mutated the
// receiver, every later use of the package-level sentinel would carry one
// call's cause.
func TestWrapDoesNotMutateTheSentinel(t *testing.T) {
	t.Parallel()

	_ = errs.ErrBudgetExceeded.Wrap(errors.New("first call"))

	if got := errs.ErrBudgetExceeded.Error(); strings.Contains(got, "first call") {
		t.Errorf("the package-level sentinel was mutated by Wrap: %q", got)
	}
	if errors.Unwrap(errs.ErrBudgetExceeded) != nil {
		t.Error("the package-level sentinel acquired a wrapped error")
	}
}

// TestUnwrapIsNilWithoutACause pins the behavior that the wire's string cause
// must not fabricate.
//
// Returning errors.New("") for an absent cause would break every
// errors.Unwrap(err) == nil check and mint a fresh non-identity error on each
// call.
func TestUnwrapIsNilWithoutACause(t *testing.T) {
	t.Parallel()

	if got := errors.Unwrap(errs.ErrBudgetExceeded); got != nil {
		t.Errorf("Unwrap = %v, want nil for an error with no cause", got)
	}

	rebuilt := errs.FromProto(&knov1.Actionable{Code: "X", Message: "m"})
	if got := errors.Unwrap(rebuilt); got != nil {
		t.Errorf("Unwrap = %v, want nil after rebuilding an error with an empty cause", got)
	}
}

// TestErrorGrammar pins the rendering order: what failed → why → the fix.
// This string is the product surface for every failure.
func TestErrorGrammar(t *testing.T) {
	t.Parallel()

	err := (&errs.Actionable{
		Code:    "AGENT_UNREACHABLE",
		Message: "cannot reach the agent at http://localhost:8000/v1",
		Fix:     "check the endpoint is running: curl http://localhost:8000/v1/models",
		DocsURL: "https://kno.dev/docs/errors#agent-unreachable",
	}).Wrap(errors.New("dial tcp 127.0.0.1:8000: connect: connection refused"))

	got := err.Error()
	want := "cannot reach the agent at http://localhost:8000/v1: " +
		"dial tcp 127.0.0.1:8000: connect: connection refused\n" +
		"  fix: check the endpoint is running: curl http://localhost:8000/v1/models\n" +
		"  docs: https://kno.dev/docs/errors#agent-unreachable"

	if got != want {
		t.Errorf("error rendering drifted.\ngot:\n%s\n\nwant:\n%s", got, want)
	}

	// The order is the contract: a user reads what broke before why, and why
	// before what to do.
	iMsg := strings.Index(got, "cannot reach")
	iCause := strings.Index(got, "connection refused")
	iFix := strings.Index(got, "fix:")
	if iMsg >= iCause || iCause >= iFix {
		t.Error("grammar out of order; must read what failed -> why -> fix")
	}
}

// TestProtoRoundTripPreservesFields covers the field-level fidelity the API
// depends on, including the cause text that replaces the Go chain.
func TestProtoRoundTripPreservesFields(t *testing.T) {
	t.Parallel()

	original := (&errs.Actionable{
		Code:     "BUDGET_EXCEEDED",
		Message:  "the run would exceed its budget",
		Fix:      "raise max_cost_usd",
		DocsURL:  "https://kno.dev/docs/errors#budget",
		ExitCode: errs.ExitBudgetStopped,
	}).Wrap(errors.New("estimated $12.40 against a $10.00 cap"))

	rebuilt := errs.FromProto(original.Proto())

	if rebuilt.Code != original.Code || rebuilt.Message != original.Message ||
		rebuilt.Fix != original.Fix || rebuilt.DocsURL != original.DocsURL ||
		rebuilt.ExitCode != original.ExitCode {
		t.Errorf("field drift across the wire:\ngot  %+v\nwant %+v", rebuilt, original)
	}
	if !strings.Contains(rebuilt.Error(), "estimated $12.40") {
		t.Error("the cause text did not survive the round trip")
	}
}

// TestExitCodeOf pins the mapping CI gates depend on. A deploy gate must be
// able to tell "validation failed" from "it broke".
func TestExitCodeOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int
	}{
		{"no error", nil, errs.ExitOK},
		{"plain error is unclassified", errors.New("boom"), errs.ExitError},
		{"budget stop is resumable, not a failure", errs.ErrBudgetExceeded, errs.ExitBudgetStopped},
		{"holdout seal is an error", errs.ErrHoldoutSealed, errs.ExitError},
		{
			"validation failure is its own code",
			&errs.Actionable{Code: "VALIDATION_FAILED", ExitCode: errs.ExitValidationFailed},
			errs.ExitValidationFailed,
		},
		{
			"code survives being wrapped by fmt.Errorf",
			fmt.Errorf("running value stage: %w", errs.ErrBudgetExceeded),
			errs.ExitBudgetStopped,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := errs.ExitCodeOf(tc.err); got != tc.want {
				t.Errorf("ExitCodeOf = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestExitCodesMatchTheSchema keeps the Go constants and the wire in step. The
// CLI reads one and the API serializes the other; a drift between them would
// make a deploy gate behave differently depending on which surface it used.
func TestExitCodesMatchTheSchema(t *testing.T) {
	t.Parallel()

	a := &errs.Actionable{Code: "X", ExitCode: errs.ExitValidationFailed}
	if got := a.Proto().GetExitCode(); int(got) != errs.ExitValidationFailed {
		t.Errorf("proto exit_code = %d, want %d", got, errs.ExitValidationFailed)
	}
}

// TestFromProtoNil covers the nil case explicitly: a response with no error
// must not produce a non-nil error value.
func TestFromProtoNil(t *testing.T) {
	t.Parallel()

	if got := errs.FromProto(nil); got != nil {
		t.Errorf("FromProto(nil) = %v, want nil", got)
	}
}
