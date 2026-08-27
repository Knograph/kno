package core

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/executor"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/observe"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// Calling the agent for one Case, and deciding what a failure means: whether
// to retry it, how long to wait, and what is authorized and settled around it.
//
// This is the spend path. Guard.Authorize, Reservation.Settle and
// Reservation.Release appear here and nowhere else in the stage; the planning
// arithmetic they consume is in baseline_budget.go.
//
// Recording an outcome and emitting its event is deliberately elsewhere, in
// baseline_record.go: what a Case cost and what a Case is worth are different
// questions, and the sink answers the second.

// workFunc invokes the agent under the budget guard and scores the response.
//
// It does not touch the aggregator: counting happens after persistence, in
// emit, so the Run's counts can never outrun the outcomes table.
func (o BaselineOptions) workFunc(agg *aggregator) executor.WorkFunc[*Case, caseOutcome] {
	return func(ctx context.Context, c *Case) (out *caseOutcome, err error) {
		// One span per Case, hanging off the run span. The ID, never the
		// input: a Case ID is a label the user chose, while the input is the
		// conversation content docs/retention.md promises stays local.
		ctx, span := observe.StartCase(ctx, c.GetId())
		defer func() {
			if err != nil {
				observe.Fail(span, codeOf(err))
			}
			span.End()
		}()

		// The same guard one frame up, for a panic AFTER the provider call —
		// Goal.Score is a Ring-1 plug-in point on this goroutine, and a panic
		// there would otherwise discard everything the call was charged for.
		var billed, settledCalls int64
		var attempts int
		defer func() {
			if p := recover(); p != nil {
				out = &caseOutcome{
					Attempts: attempts, BilledUSDMicros: billed, SettledCalls: settledCalls,
					Err: fmt.Errorf("panic scoring case %s: %T", c.GetId(), p),
				}
				err = out.Err
			}
		}()
		// One provider call, estimated before it is made. A zero estimate makes
		// the dollar cap unenforceable, so callers that care about it must
		// supply EstCostPerCallUSDMicros.
		est, err := o.estimate(ctx, c)
		if err != nil {
			return nil, err
		}

		var resp *Response
		var invokeErr error
		resp, attempts, billed, settledCalls, invokeErr = o.invokeWithRetry(ctx, c, agg, est)
		if invokeErr != nil {
			return &caseOutcome{
				Response: nil, Err: invokeErr, Attempts: attempts,
				BilledUSDMicros: billed, SettledCalls: settledCalls,
			}, invokeErr
		}

		score, scoreErr := o.Goal.Score(ctx, c, resp)
		if scoreErr != nil {
			return &caseOutcome{
				Response: resp, Err: scoreErr, Attempts: attempts,
				BilledUSDMicros: billed, SettledCalls: settledCalls,
			}, scoreErr
		}

		// NOT counted here. An earlier version incremented the scored count on
		// the worker goroutine the moment scoring succeeded — before the sink
		// had persisted anything. A store failure mid-run then left the Run
		// claiming more scored Cases than the outcomes table held, and that
		// inflated count was the last thing durably recorded. Counting happens
		// after persistence, in emit, where errored Cases are already counted.
		return &caseOutcome{
			Response: resp, Score: score, Attempts: attempts,
			BilledUSDMicros: billed, SettledCalls: settledCalls,
		}, nil
	}
}

// invokeWithRetry invokes one Case through the shared budget-and-retry core.
//
// A thin wrapper, and thin on purpose: everything it used to contain now lives
// in invoker, so the Value stage calls the same code rather than a second copy.
// Two stages with two implementations of "authorize, call, settle, retry" is
// how they come to disagree about money, and every line moved was already the
// fix for a defect measured once.
//
// What stays here is the only part that is genuinely Baseline's: which events
// to emit, and the aggregator they are recorded against.
func (o BaselineOptions) invokeWithRetry(
	ctx context.Context,
	c *Case,
	agg *aggregator,
	est budget.Estimate,
) (resp *Response, attempts int, billedUSDMicros, settledCallCount int64, err error) {
	return o.invoker(agg).withRetry(ctx, c, est)
}

// invoker builds the shared core with Baseline's event hooks.
//
// The hooks receive a context that is already detached and already carries its
// grace, so neither stage can get that wrong independently — which matters
// because an overshoot is emitted exactly when a budget stop has cancelled the
// worker, and a hook that used the live context would drop the one event
// explaining where the money went.
func (o BaselineOptions) invoker(agg *aggregator) invoker {
	return invoker{
		Agent:        o.Agent,
		AgentRef:     o.AgentRef,
		Guard:        o.Guard,
		MaxAttempts:  o.maxAttempts(),
		RetryBudget:  o.retryBudget(),
		RetryBackoff: o.retryBackoff(),
		OnOvershoot: func(ctx context.Context, key store.MeasurementKey, estimated, settled, overshoot int64) {
			agg.recordEmitFailure(o.emitSettlementOvershoot(
				ctx, agg, key, estimated, settled, overshoot))
		},
		OnRetry: func(ctx context.Context, key store.MeasurementKey, attempt int,
			reason knov1.RetryReason, wait, remaining time.Duration,
		) {
			agg.recordEmitFailure(o.emitRetryAttempted(
				ctx, agg, key, attempt, reason, wait, remaining))
		},
	}
}

// billedCostOf reports what a provider charged for a call that failed.
//
// An anonymous interface, the same shape retryAfterOf uses, so an adapter can
// report a charge without core importing it — prime directive 3.
//
// Non-positive is discarded rather than settled. An adapter reporting a
// negative would CREDIT the run's cap, and "the provider said nothing" is not
// the same as "the provider said zero": the absence of a charge is not a
// charge. Guard.Settle clamps too, but a caller that hands it nonsense and
// relies on the clamp is describing a bug rather than a charge.
func billedCostOf(err error) int64 {
	var b interface{ BilledCostUSDMicros() int64 }
	if !errors.As(err, &b) {
		return 0
	}
	if v := b.BilledCostUSDMicros(); v > 0 {
		return v
	}
	return 0
}

// saturatingAdd adds without wrapping. Non-positive is discarded: a charge
// that credits the run is not a charge.
func saturatingAdd(total, add int64) int64 {
	if add <= 0 {
		return total
	}
	if total > math.MaxInt64-add {
		return math.MaxInt64
	}
	return total + add
}

// retryReasonOf classifies why a Case is being retried.
//
// Only reached after retryable() has passed, which admits exactly the two
// sentinels below — so the default arm is unreachable, and deliberately so:
// the proto says an emitter that cannot classify MUST NOT retry, which makes
// emitting UNSPECIFIED on a RetryAttempted a contract violation rather than an
// honest shrug. The arm exists because a future retryable sentinel would
// otherwise fail to compile into a reason, and that should be caught by the
// enum growing, not by a panic.
//
// A billed failure reports PROVIDER_UNAVAILABLE rather than
// TRANSPORT_TRANSIENT. See the case below for why the charge is the
// discriminator.
func retryReasonOf(err error) knov1.RetryReason {
	// The adapter's own classification wins where it has one. It is the only
	// layer that saw the status code, and a sentinel cannot recover what a
	// status code knew.
	if r, ok := retryReasonFrom(err); ok {
		return r
	}
	switch {
	case errors.Is(err, errs.ErrRateLimited):
		return knov1.RetryReason_RETRY_REASON_RATE_LIMITED

	// A CHARGE is evidence the provider processed the request, and
	// TRANSPORT_TRANSIENT is defined as having none. An adapter wraps both a
	// reset connection and a billed 5xx as ErrTransportTransient, so the
	// sentinel alone cannot separate them — the charge can. PROVIDER_UNAVAILABLE
	// already means "the provider returned a 5xx", so no new enum value is
	// needed, only an emitter that stops reporting the one retry reason that
	// costs money as the one that means nothing happened.
	case billedCostOf(err) > 0:
		return knov1.RetryReason_RETRY_REASON_PROVIDER_UNAVAILABLE

	case errors.Is(err, errs.ErrTransportTransient):
		return knov1.RetryReason_RETRY_REASON_TRANSPORT_TRANSIENT
	default:
		return knov1.RetryReason_RETRY_REASON_UNSPECIFIED
	}
}

// runFatalOf reports whether an error ends the whole run rather than one Case.
//
// An anonymous interface, the same shape retryAfterOf and billedCostOf use, so
// an adapter can escalate without core importing it — prime directive 3. The
// alternative was a sentinel in core/errs that adapters wrap, and it is broken
// in both possible orientations: Actionable has ONE wrapped slot and Wrap
// replaces it, so wrapping outward makes errors.As return the sentinel's
// Actionable and codeOf persist a generic code (docs/debt.md#39 verbatim),
// while wrapping inward doubles the fix line and leaves nowhere for the
// provider's cause.
//
// This does NOT survive serialization. errs.FromProto rebuilds an Actionable
// with no chain, so a run-fatal error arriving from a Ring-2 plugin or over
// the api reaches here as an ordinary one. Ledgered as docs/debt.md#56 rather
// than fixed with a wire field whose only consumer would be a plugin Agent
// that does not exist.
func runFatalOf(err error) bool {
	var rf interface{ RunFatal() bool }
	return errors.As(err, &rf) && rf.RunFatal()
}

// retryReasonFrom reads a reason an adapter attached, if it did.
//
// core classifies from sentinels where it can. It cannot separate a timeout
// from a 5xx that way — an adapter wraps both as ErrTransportTransient — so a
// timed-out request was reported as PROVIDER_UNAVAILABLE, whose schema
// definition is "the provider returned a 5xx". See docs/debt.md#53.
func retryReasonFrom(err error) (knov1.RetryReason, bool) {
	var rr interface{ RetryReason() knov1.RetryReason }
	if !errors.As(err, &rr) {
		return knov1.RetryReason_RETRY_REASON_UNSPECIFIED, false
	}
	r := rr.RetryReason()
	return r, r != knov1.RetryReason_RETRY_REASON_UNSPECIFIED
}

// retryable reports whether an error may succeed on another attempt.
//
// Two kinds, and both must be here. ErrRateLimited is the provider deliberately
// refusing. ErrTransportTransient is the request never reaching a handler — a
// stale pooled connection, a reset before any bytes were written. Treating the
// second as terminal marks a healthy baseline unusable over an idle timeout,
// because at concurrency any pause in a long run produces a handful of them.
//
// A budget refusal is explicitly not retryable: the cap will not have moved,
// and re-authorizing would spin.
func retryable(err error) bool {
	if errors.Is(err, errs.ErrBudgetExceeded) {
		return false
	}
	return errors.Is(err, errs.ErrRateLimited) || errors.Is(err, errs.ErrTransportTransient)
}

// retryAfterOf extracts a provider-requested wait, if the error carries one.
//
// The transport parses Retry-After in both RFC 9110 forms and clamps it; this
// only reads what it decided. An adapter signals it by wrapping a RetryAfter.
func retryAfterOf(err error) (time.Duration, bool) {
	var ra interface{ RetryAfter() time.Duration }
	if errors.As(err, &ra) {
		d := ra.RetryAfter()
		return d, d > 0
	}
	return 0, false
}
