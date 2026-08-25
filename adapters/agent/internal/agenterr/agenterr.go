// Package agenterr carries adapter-side facts that core reads without
// importing an adapter.
//
// core declares anonymous interfaces — `interface{ RunFatal() bool }`,
// `interface{ RetryAfter() time.Duration }` — and an adapter satisfies them
// structurally. Nothing imports anything across the boundary, which is what
// prime directive 3 requires.
//
// The wrappers live here rather than in each adapter because getting one
// subtly wrong is invisible and expensive. A wrapper that embeds an
// *errs.Actionable inherits its Unwrap, which returns the Actionable's own
// cause and jumps straight PAST the Actionable in the chain: errors.Is still
// answers through the promoted Is, but errors.As(err, **errs.Actionable)
// answers false, and core.codeOf then records a generic code in the persisted
// Outcome, on the event stream, and in --json. That is docs/debt.md#39,
// measured rather than theorized. Three hand-written wrappers already exist in
// the two adapters; these are the ones core's newest assertions read, and they
// are written once.
package agenterr

import knov1 "github.com/knograph/kno/gen/kno/v1"

// runFatal marks an error whose condition cannot change within the run.
type runFatal struct{ err error }

// Error renders the wrapped grammar unchanged. The marker adds a fact for
// core, never a sentence for the user.
func (e *runFatal) Error() string { return e.err.Error() }

// Unwrap exposes the cause, so errors.Is and errors.As both reach through to
// the Actionable and its Code survives into the record.
func (e *runFatal) Unwrap() error { return e.err }

// RunFatal reports that this error ends the run.
func (e *runFatal) RunFatal() bool { return true }

// AsRunFatal marks err as ending the whole run rather than one Case.
//
// Applied PER INSTANCE at the point of classification, never to a sentinel.
// errs.Actionable.Is compares by Code, and the provider errors that qualify
// share ErrProvider's code with a dozen that do not — marking the sentinel
// would escalate every one of them and turn a recoverable run into a dead one.
//
// Only for conditions that genuinely cannot change within a run: a rejected
// credential, the provider's own spend cap, an unpaid account, a model that
// does not exist, a refused destination. A plain 429 or a 5xx is the opposite
// case and must stay per-Case.
func AsRunFatal(err error) error {
	if err == nil {
		return nil
	}
	return &runFatal{err: err}
}

// retryReason carries the classification core cannot derive from a sentinel.
type retryReason struct {
	err    error
	reason knov1.RetryReason
}

// Error renders the wrapped grammar unchanged.
func (e *retryReason) Error() string { return e.err.Error() }

// Unwrap exposes the cause, for the same reason runFatal does.
func (e *retryReason) Unwrap() error { return e.err }

// RetryReason reports why this call is being retried.
func (e *retryReason) RetryReason() knov1.RetryReason { return e.reason }

// WithRetryReason attaches the reason a retry is happening.
//
// core classifies from sentinels where it can, and a sentinel cannot separate
// a timeout from a 5xx: an adapter wraps both as ErrTransportTransient, so a
// timed-out request was reported as PROVIDER_UNAVAILABLE — whose schema
// definition is "the provider returned a 5xx". The old label was vague; the
// replacement was specific and sometimes wrong, which is worse. See
// docs/debt.md#53.
//
// UNSPECIFIED is not attached: an emitter that cannot classify should say
// nothing and let core fall back to what the sentinels support.
func WithRetryReason(err error, reason knov1.RetryReason) error {
	if err == nil || reason == knov1.RetryReason_RETRY_REASON_UNSPECIFIED {
		return err
	}
	return &retryReason{err: err, reason: reason}
}
