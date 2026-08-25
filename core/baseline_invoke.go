package core

import (
	"context"
	"errors"
	"time"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/executor"
	knov1 "github.com/knograph/kno/gen/kno/v1"
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
	return func(ctx context.Context, c *Case) (*caseOutcome, error) {
		// One provider call, estimated before it is made. A zero estimate makes
		// the dollar cap unenforceable, so callers that care about it must
		// supply EstCostPerCallUSDMicros.
		est, err := o.estimate(ctx, c)
		if err != nil {
			return nil, err
		}

		resp, attempts, billed, invokeErr := o.invokeWithRetry(ctx, c, agg, est)
		if invokeErr != nil {
			return &caseOutcome{
				Response: nil, Err: invokeErr, Attempts: attempts, BilledUSDMicros: billed,
			}, invokeErr
		}

		score, scoreErr := o.Goal.Score(ctx, c, resp)
		if scoreErr != nil {
			return &caseOutcome{
				Response: resp, Err: scoreErr, Attempts: attempts, BilledUSDMicros: billed,
			}, scoreErr
		}

		// NOT counted here. An earlier version incremented the scored count on
		// the worker goroutine the moment scoring succeeded — before the sink
		// had persisted anything. A store failure mid-run then left the Run
		// claiming more scored Cases than the outcomes table held, and that
		// inflated count was the last thing durably recorded. Counting happens
		// after persistence, in emit, where errored Cases are already counted.
		return &caseOutcome{
			Response: resp, Score: score, Attempts: attempts, BilledUSDMicros: billed,
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
) (resp *Response, attempts int, billedUSDMicros int64, err error) {
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

	for attempt := 1; attempt <= o.maxAttempts(); attempt++ {
		attempts++
		resp, settled, overshoot, invokeErr := o.invokeOnce(ctx, c, est)
		billed += settled.CostUSDMicros
		if overshoot > 0 {
			// Emitted at settle time and gated on the DELTA, so the count is
			// bounded by concurrency rather than by Case count: once the cap
			// binds, only reservations already in flight can overshoot.
			if emitErr := o.emitSettlementOvershoot(
				ctx, agg, c.GetId(), est.CostUSDMicros, settled.CostUSDMicros,
			); emitErr != nil {
				return nil, attempts, billed, emitErr
			}
		}
		if invokeErr == nil {
			return resp, attempts, billed, nil
		}
		lastErr = invokeErr

		if !retryable(invokeErr) {
			return nil, attempts, billed, invokeErr
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
		if emitErr := o.emitRetryAttempted(ctx, agg, c.GetId(), attempt,
			retryReasonOf(invokeErr), wait, time.Until(deadline)); emitErr != nil {
			return nil, attempts, billed, emitErr
		}

		// A ctx-aware wait. Sleeping through a cancellation would keep a
		// stopped run alive for the length of the backoff.
		select {
		case <-time.After(wait):
			backoff *= 2
		case <-ctx.Done():
			return nil, attempts, billed, ctx.Err()
		}
	}
	return nil, attempts, billed, lastErr
}

// retryReasonOf classifies why a Case is being retried.
//
// UNSPECIFIED for anything the enum does not name, which is honest: a reason
// nobody enumerated is better reported as unknown than mapped to whichever
// neighbouring value looks closest.
func retryReasonOf(err error) knov1.RetryReason {
	switch {
	case errors.Is(err, errs.ErrRateLimited):
		return knov1.RetryReason_RETRY_REASON_RATE_LIMITED
	case errors.Is(err, errs.ErrTransportTransient):
		return knov1.RetryReason_RETRY_REASON_TRANSPORT_TRANSIENT
	default:
		return knov1.RetryReason_RETRY_REASON_UNSPECIFIED
	}
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
) (resp *Response, settled budget.Spend, overshoot int64, err error) {
	res, err := o.Guard.Authorize(ctx, est)
	if err != nil {
		return nil, budget.Spend{}, 0, err
	}
	// Reached on every path: the agent erroring, the context cancelling
	// mid-call, a panic recovered by the executor. Without it each of those
	// leaks headroom and the guard eventually refuses work it should allow.
	defer res.Release()

	resp, invokeErr := o.Agent.Invoke(ctx, c)
	if invokeErr != nil {
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
