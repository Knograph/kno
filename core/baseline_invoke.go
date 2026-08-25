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

// invokeWithRetry returns the response, how many provider calls it took, and
// the error.
//
// The attempt count is returned because store.Outcome.Spend is documented as
// "including any failed attempts preceding a successful retry" and did not
// include them: the guard settled each attempt while the store persisted one,
// so Guard.Restore under-restored the call cap by (attempts-1) for every
// retried Case. Harmless while only a fake agent ran; a real 429 makes it
// routine.
func (o BaselineOptions) invokeWithRetry(
	ctx context.Context,
	c *Case,
	agg *aggregator,
	est budget.Estimate,
) (resp *Response, attempts int, billedUSDMicros, settledCallCount int64, err error) {
	backoff := o.retryBackoff()
	// time.Now, not o.now. The budget bounds real elapsed time, and o.now is
	// injectable — a frozen clock (which BaselineOptions.Now's own godoc
	// invites for stable golden tests) made "now + wait > now + budget"
	// collapse to "wait > budget", turning a cumulative bound into a per-sleep
	// one. Measured under a frozen clock: 40 provider calls for one Case
	// against a 50ms budget.
	deadline := time.Now().Add(o.retryBudget())
	var lastErr error

	// Accumulated across attempts, because each one settles into the guard
	// separately and only the LAST error survives the loop. With MaxAttempts 3
	// the guard can settle three billed charges that the sink could recover at
	// most one of — and SettledSpend is the only durable record of money
	// spent, so the difference is headroom a resumed run spends twice.
	var billed int64

	// Calls the guard actually SETTLED, which is not the attempt count.
	// attempts++ runs at the top of the loop, before invokeOnce, and a refused
	// Authorize returns before settling anything — so a Case that made one
	// real call and was refused on attempt 2 would otherwise persist Calls: 2
	// against a guard that settled 1.
	var settledCalls int64

	// A panic must not take the money with it. The executor recovers, but it
	// discards the value — deliberately — so an outcome built after the
	// unwind would carry no spend, and sinkFunc would persist zero for a Case
	// the guard had already settled. Measured before this: guard 3 calls,
	// store 0.
	//
	// %T rather than %v, matching the executor: a panic value is arbitrary and
	// may embed a prompt or a response, and this string is persisted.
	defer func() {
		if p := recover(); p != nil {
			resp = nil
			billedUSDMicros = billed
			settledCallCount = settledCalls
			err = fmt.Errorf("panic invoking case %s: %T", c.GetId(), p)
		}
	}()

	for attempt := 1; attempt <= o.maxAttempts(); attempt++ {
		attempts++
		resp, settled, overshoot, invokeErr := o.invokeOnce(ctx, c, est, attempt)
		// Saturating, matching Guard.Settle. A plain add wraps where the guard
		// pins, so the store and the guard would disagree on exactly the
		// inputs the clamp exists for — and the store is the one that outlives
		// the process.
		billed = saturatingAdd(billed, settled.CostUSDMicros)
		settledCalls = saturatingAdd(settledCalls, settled.Calls)
		if overshoot > 0 {
			// Emitted at settle time and gated on the DELTA, so the count is
			// bounded by concurrency rather than by Case count: once the cap
			// binds, only reservations already in flight can overshoot.
			//
			// The failure is recorded, NOT returned. Returning it discarded a
			// paid, scoreable answer — the overshoot check runs before the
			// success check — and recorded the Case as an agent error while
			// still writing its outcome row, so a resume skipped it forever.
			//
			// WithoutCancel with its own grace, because a budget stop cancels
			// the worker context and a budget stop is exactly when an
			// overshoot happens.
			emitCtx, cancel := context.WithTimeout(
				context.WithoutCancel(ctx), progressWriteGrace,
			)
			agg.recordEmitFailure(o.emitSettlementOvershoot(
				emitCtx, agg, c.GetId(), est.CostUSDMicros, settled.CostUSDMicros, overshoot,
			))
			cancel()
		}
		if invokeErr == nil {
			return resp, attempts, billed, settledCalls, nil
		}
		lastErr = invokeErr

		if !retryable(invokeErr) {
			return nil, attempts, billed, settledCalls, invokeErr
		}
		if attempt == o.maxAttempts() {
			break
		}

		wait := backoff
		// A provider that says how long to wait is the authority on its own
		// limits; our doubling is only a guess for when it does not say.
		if after, ok := retryAfterOf(invokeErr); ok {
			wait = after
		}

		// Both bounds, whichever binds first. Stopping here rather than after
		// the wait means the budget is a bound on time SPENT, not on time
		// spent plus one more sleep.
		if time.Now().Add(wait).After(deadline) {
			break
		}

		// BEFORE the wait, not after. The whole value of the signal is
		// telling a watcher the run is obeying a provider's backoff rather
		// than hung; emitted after the sleep it announces, it reports
		// idleness only once idleness has ended.
		// Recorded rather than returned, for the same reason: an event-store
		// hiccup must not convert a retryable Case into a terminal error and
		// inflate the run's error rate past ErrorRateExceeded.
		retryCtx, cancelRetry := context.WithTimeout(
			context.WithoutCancel(ctx), progressWriteGrace,
		)
		agg.recordEmitFailure(o.emitRetryAttempted(retryCtx, agg, c.GetId(), attempt,
			retryReasonOf(invokeErr), wait, time.Until(deadline)))
		cancelRetry()

		// A ctx-aware wait. Sleeping through a cancellation would keep a
		// stopped run alive for the length of the backoff.
		select {
		case <-time.After(wait):
			backoff *= 2
		case <-ctx.Done():
			return nil, attempts, billed, settledCalls, ctx.Err()
		}
	}
	return nil, attempts, billed, settledCalls, lastErr
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

// invokeOnce is a single authorized attempt.
func (o BaselineOptions) invokeOnce(
	ctx context.Context,
	c *Case,
	est budget.Estimate,
	attempt int,
) (resp *Response, settled budget.Spend, overshoot int64, err error) {
	// SpanKindClient, because this is the call OUT of the process — which is
	// what makes a provider show up as a dependency edge in a trace rather
	// than as internal work, and what makes provider latency separable from
	// ours. Opened before Authorize so a budget refusal is visible as a call
	// that never happened rather than as a gap.
	ctx, span := observe.StartAgentCall(ctx, o.AgentRef.GetScheme(), attempt)
	defer span.End()

	res, err := o.Guard.Authorize(ctx, est)
	if err != nil {
		observe.Fail(span, codeOf(err))
		return nil, budget.Spend{}, 0, err
	}
	// Reached on every path: the agent erroring, the context cancelling
	// mid-call, a panic recovered by the executor. Without it each of those
	// leaks headroom and the guard eventually refuses work it should allow.
	defer res.Release()

	resp, invokeErr := o.Agent.Invoke(ctx, c)
	if invokeErr != nil {
		observe.Fail(span, codeOf(invokeErr))
		// The attempt consumed a call, and the provider may have CHARGED for
		// it: a 200 carrying both an error object and a usage block is a shape
		// several OpenAI-compatible gateways produce. M2-7 made that
		// observable on the error; settling zero here is what made it free.
		spend := budget.Spend{Calls: 1, CostUSDMicros: billedCostOf(invokeErr)}
		return nil, spend, res.Settle(spend), invokeErr
	}

	// The same computation the sink persists. Two independent derivations of
	// one Case's cost could drift, and the persisted one is what
	// Guard.Restore reads on resume.
	spend := spendOf(resp)
	return resp, spend, res.Settle(spend), nil
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
