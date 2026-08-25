package agenterr_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/knograph/kno/adapters/agent/internal/agenterr"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// runFatalOf and reasonOf read the markers exactly as core does — anonymous
// interfaces, asserted structurally. Copied rather than imported on purpose:
// core cannot import this package and must not, so a test that shared a
// definition with it would be proving the opposite of prime directive 3.
func runFatalOf(err error) bool {
	var rf interface{ RunFatal() bool }
	return errors.As(err, &rf) && rf.RunFatal()
}

func reasonOf(err error) knov1.RetryReason {
	var rr interface{ RetryReason() knov1.RetryReason }
	if !errors.As(err, &rr) {
		return knov1.RetryReason_RETRY_REASON_UNSPECIFIED
	}
	return rr.RetryReason()
}

// TestTheMarkersAddAFactWithoutDestroyingTheClassification.
//
// This is the property the package exists for and the one docs/debt.md#39
// records the cost of getting wrong. A wrapper that omits Unwrap — or embeds an
// *errs.Actionable and inherits the promoted one, which returns the
// Actionable's OWN cause — jumps straight past the Actionable in the chain.
// errors.Is still answers through the promoted Is, so a sentinel check looks
// healthy; errors.As(err, **errs.Actionable) answers FALSE, and core.codeOf
// then records a generic code in the persisted Outcome, on the event stream,
// and in --json.
//
// Asserting only that the marker reads back true does not catch it: the marker
// is the outermost type either way.
func TestTheMarkersAddAFactWithoutDestroyingTheClassification(t *testing.T) {
	t.Parallel()

	sentinel := &errs.Actionable{
		Code: "TEST_CODE", Message: "the provider rejected this", Fix: "fix it",
		ExitCode: errs.ExitError,
	}
	cause := errors.New("underlying cause")

	tests := []struct {
		name    string
		wrapped error
	}{
		{"run-fatal", agenterr.AsRunFatal(sentinel.Wrap(cause))},
		{
			"a retry reason",
			agenterr.WithRetryReason(sentinel.Wrap(cause),
				knov1.RetryReason_RETRY_REASON_TIMEOUT),
		},
		{
			"both, composed",
			agenterr.AsRunFatal(agenterr.WithRetryReason(sentinel.Wrap(cause),
				knov1.RetryReason_RETRY_REASON_TIMEOUT)),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var a *errs.Actionable
			if !errors.As(tt.wrapped, &a) {
				t.Fatal("the marker hid the Actionable; every consumer of the " +
					"chain now records a generic code instead of the adapter's")
			}
			if a.Code != "TEST_CODE" {
				t.Errorf("code = %q, want TEST_CODE", a.Code)
			}
			if got := errs.ExitCodeOf(tt.wrapped); got != errs.ExitError {
				t.Errorf("exit code = %d, want %d", got, errs.ExitError)
			}
			if !errors.Is(tt.wrapped, cause) {
				t.Error("the original cause is no longer reachable")
			}
			// The marker carries a fact for core, never a sentence for a human.
			if tt.wrapped.Error() != sentinel.Wrap(cause).Error() {
				t.Errorf("the message changed:\n got %q\nwant %q",
					tt.wrapped.Error(), sentinel.Wrap(cause).Error())
			}
		})
	}
}

// TestTheMarkersReadBack, so a wrapper that preserves the chain but forgets its
// own method is caught too.
func TestTheMarkersReadBack(t *testing.T) {
	t.Parallel()

	base := errors.New("boom")

	if !runFatalOf(agenterr.AsRunFatal(base)) {
		t.Error("AsRunFatal does not read back as run-fatal")
	}
	if runFatalOf(base) {
		t.Error("an unmarked error reads as run-fatal, so everything escalates")
	}
	if got := reasonOf(agenterr.WithRetryReason(base,
		knov1.RetryReason_RETRY_REASON_TIMEOUT)); got != knov1.RetryReason_RETRY_REASON_TIMEOUT {
		t.Errorf("reason = %v, want TIMEOUT", got)
	}

	// Composed in either order, both facts survive: a wrapper that shadowed the
	// other's method would silently drop one.
	both := agenterr.AsRunFatal(agenterr.WithRetryReason(base,
		knov1.RetryReason_RETRY_REASON_TIMEOUT))
	if !runFatalOf(both) || reasonOf(both) != knov1.RetryReason_RETRY_REASON_TIMEOUT {
		t.Error("composing the two markers dropped one of them")
	}
}

// TestNothingIsMarkedForNothing.
//
// A nil error stays nil — a marker on nothing would turn a success into a
// non-nil error and end the run. And UNSPECIFIED is not attached: an emitter
// that cannot classify should say nothing and let core fall back to what its
// sentinels support, rather than asserting "reason: unset" over them.
func TestNothingIsMarkedForNothing(t *testing.T) {
	t.Parallel()

	if err := agenterr.AsRunFatal(nil); err != nil {
		t.Errorf("AsRunFatal(nil) = %v, want nil", err)
	}
	if err := agenterr.WithRetryReason(nil, knov1.RetryReason_RETRY_REASON_TIMEOUT); err != nil {
		t.Errorf("WithRetryReason(nil, ...) = %v, want nil", err)
	}

	base := fmt.Errorf("boom")
	got := agenterr.WithRetryReason(base, knov1.RetryReason_RETRY_REASON_UNSPECIFIED)
	if got != base { //nolint:errorlint // identity is the assertion
		t.Error("an UNSPECIFIED reason was attached, shadowing core's own " +
			"sentinel-derived classification with nothing")
	}
}
