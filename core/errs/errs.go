// Package errs defines Kno's error grammar.
package errs

import (
	"errors"
	"strings"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// Exit codes. These are a covenant after 1.0: CI gates branch on the
// difference between "it broke" and "validation failed", so they live here
// rather than in cli/ where the API could not see them.
const (
	// ExitOK means the command succeeded.
	ExitOK = 0

	// ExitError means the command failed for any unclassified reason.
	ExitError = 1

	// ExitBudgetStopped means a budget cap halted the run before completion.
	// The run is resumable; nothing is wrong with the data.
	ExitBudgetStopped = 2

	// ExitValidationFailed means the portfolio did not hold up against the
	// holdout. This is the code a deploy gate should block on.
	ExitValidationFailed = 3
)

// Sentinels. Compare with errors.Is, never with ==: an Actionable rebuilt from
// the wire is a different Go value, and Is matches on Code so that identity
// survives serialization.
var (
	// ErrBudgetExceeded means an operation was refused because it would exceed
	// a budget cap.
	ErrBudgetExceeded = &Actionable{
		Code:     "BUDGET_EXCEEDED",
		Message:  "the run would exceed its budget",
		Fix:      "raise max_cost_usd in kno.yaml, or re-run with --resume to continue where it stopped",
		ExitCode: ExitBudgetStopped,
	}

	// ErrHoldoutSealed means something tried to read the holdout before
	// Validate. This is a statistical-integrity failure, not a permission one.
	ErrHoldoutSealed = &Actionable{
		Code:     "HOLDOUT_SEALED",
		Message:  "the holdout cannot be read before Validate",
		Fix:      "run `kno validate` to open the holdout; selecting against it would inflate the reported gain",
		ExitCode: ExitError,
	}

	// ErrCapabilityUnsupported means an adapter was asked for something it
	// does not declare through Capable.
	ErrCapabilityUnsupported = &Actionable{
		Code:     "CAPABILITY_UNSUPPORTED",
		Message:  "the adapter does not support the requested capability",
		Fix:      "run `kno doctor` to print the capability matrix, then pick a supported injection mode",
		ExitCode: ExitError,
	}

	// ErrCheckpointStale means a checkpoint does not match the current inputs,
	// so resuming from it would mix results from different runs.
	ErrCheckpointStale = &Actionable{
		Code:     "CHECKPOINT_STALE",
		Message:  "the checkpoint does not match the current evals, pool, or goal",
		Fix:      "re-run without --resume to start a fresh run",
		ExitCode: ExitError,
	}

	// ErrRateLimited means a provider asked us to slow down. Transient: the
	// caller should back off and retry rather than fail the run.
	ErrRateLimited = &Actionable{
		Code:     "RATE_LIMITED",
		Message:  "the provider is rate limiting this run",
		Fix:      "lower concurrency in kno.yaml, or wait for the provider's Retry-After window",
		ExitCode: ExitError,
	}
)

// Actionable is a user-facing error that answers three questions in order:
// what failed, why, and the exact thing to do about it.
//
// It is the same struct the CLI renders and the API serializes, so a terminal
// message and an SDK exception cannot drift apart.
type Actionable struct {
	// Code is stable and machine-readable, e.g. "BUDGET_EXCEEDED". Stable
	// across releases so CI can branch on it.
	Code string

	// Message says what failed, in one sentence, in the user's terms.
	Message string

	// Fix is the exact command or config line that resolves it — a thing to
	// run or edit, not advice.
	Fix string

	// DocsURL links to the page explaining this failure.
	DocsURL string

	// ExitCode is the process exit code the CLI will use.
	ExitCode int

	// wrapped is the underlying error, kept in-process only.
	//
	// It is deliberately NOT the same thing as the wire's `cause` string. An
	// error chain cannot survive serialization, so a rebuilt Actionable
	// reports the cause as text and returns nil from Unwrap — which is honest,
	// rather than fabricating a chain that no longer exists.
	wrapped error
}

// Error renders the grammar: what failed → why → the fix.
func (a *Actionable) Error() string {
	var b strings.Builder
	b.WriteString(a.Message)
	if a.wrapped != nil {
		b.WriteString(": ")
		b.WriteString(a.wrapped.Error())
	}
	if a.Fix != "" {
		b.WriteString("\n  fix: ")
		b.WriteString(a.Fix)
	}
	if a.DocsURL != "" {
		b.WriteString("\n  docs: ")
		b.WriteString(a.DocsURL)
	}
	return b.String()
}

// Unwrap returns the underlying error, or nil when there is none.
//
// Returning a non-nil error for an empty cause would break every
// errors.Unwrap(err) == nil check, so an Actionable with nothing wrapped
// unwraps to nil.
func (a *Actionable) Unwrap() error { return a.wrapped }

// Is reports whether target is the same KIND of error, comparing Code rather
// than identity.
//
// This is what makes sentinels survive a round trip. errors.Is compares by
// pointer equality by default, so an Actionable rebuilt from proto bytes — out
// of a plugin subprocess, a checkpoint, or an API response — would not match
// ErrBudgetExceeded, and code branching on that would silently misclassify the
// two error kinds whose misclassification matters most.
func (a *Actionable) Is(target error) bool {
	var t *Actionable
	if !errors.As(target, &t) {
		return false
	}
	return a.Code == t.Code
}

// Wrap returns a copy of a carrying err as its cause.
//
// The receiver is unmodified, so package-level sentinels stay pristine —
// mutating them would make every later comparison see one call's cause.
func (a *Actionable) Wrap(err error) *Actionable {
	out := *a
	out.wrapped = err
	return &out
}

// WithFix returns a copy of a with a more specific fix.
func (a *Actionable) WithFix(fix string) *Actionable {
	out := *a
	out.Fix = fix
	return &out
}

// Proto converts to the wire representation.
//
// The error chain does not survive: `cause` carries the wrapped error's text,
// because a chain of Go values has no wire encoding.
func (a *Actionable) Proto() *knov1.Actionable {
	out := &knov1.Actionable{
		Code:     a.Code,
		Message:  a.Message,
		Fix:      a.Fix,
		DocsUrl:  a.DocsURL,
		ExitCode: int32(a.ExitCode), //nolint:gosec // exit codes are 0-3, never truncating
	}
	if a.wrapped != nil {
		out.Cause = a.wrapped.Error()
	}
	return out
}

// FromProto rebuilds an Actionable from the wire.
//
// The result has no Go error chain — Unwrap returns nil — but errors.Is still
// matches the corresponding sentinel, because Is compares Code.
func FromProto(p *knov1.Actionable) *Actionable {
	if p == nil {
		return nil
	}
	a := &Actionable{
		Code:     p.GetCode(),
		Message:  p.GetMessage(),
		Fix:      p.GetFix(),
		DocsURL:  p.GetDocsUrl(),
		ExitCode: int(p.GetExitCode()),
	}
	if c := p.GetCause(); c != "" {
		a.wrapped = errors.New(c)
	}
	return a
}

// ExitCodeOf reports the process exit code for err.
//
// Anything that is not an Actionable is an unclassified failure and exits 1.
func ExitCodeOf(err error) int {
	if err == nil {
		return ExitOK
	}
	var a *Actionable
	if errors.As(err, &a) {
		return a.ExitCode
	}
	return ExitError
}
