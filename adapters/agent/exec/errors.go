package exec

import (
	"github.com/knograph/kno/core/errs"
)

// The exec adapter's error classification.
//
// Each is errs.Actionable, so the CLI renders it in the grammar (what failed →
// why → fix) and the store records the Code. Per-Case failures never end the
// run — they are recorded as errored Cases, which is the provider-failure
// classification the plan pins. None of these wraps errs.ErrRateLimited or
// errs.ErrTransportTransient, so none is retryable, on purpose: a local
// script that hung once will hang again, and retrying multiplies wall-clock
// by MaxAttempts for nothing.

// ErrFailed is a command that started and ended with an error.
//
// Nonzero exit, a failed exec, or a start failure. The error context carries
// the capped stderr, when there is any.
var ErrFailed = &errs.Actionable{
	Code:     "EXEC_FAILED",
	Message:  "the exec agent's command failed",
	Fix:      "run the command by hand to see what it says",
	ExitCode: errs.ExitError,
}

// ErrTimedOut is a command that outlived the per-call deadline.
//
// The process group was TERM'd and, after the grace, KILL'd; the Case is
// recorded as errored, never as run-fatal.
var ErrTimedOut = &errs.Actionable{
	Code:     "EXEC_TIMED_OUT",
	Message:  "the exec agent's command did not finish in time",
	Fix:      "raise --timeout, or make the command finish faster",
	ExitCode: errs.ExitError,
}

// ErrOutputTooLarge is a command that produced more stdout than the cap.
//
// The output is truncated and counted; the Case is errored with the cap in
// the fix line.
var ErrOutputTooLarge = &errs.Actionable{
	Code:     "EXEC_OUTPUT_TOO_LARGE",
	Message:  "the exec agent's command produced too much output",
	Fix:      "make the command emit less stdout per Case",
	ExitCode: errs.ExitError,
}
