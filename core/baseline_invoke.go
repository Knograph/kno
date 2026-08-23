package core

import (
	"context"
	"errors"
	"time"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/executor"
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
func (o BaselineOptions) workFunc() executor.WorkFunc[*Case, caseOutcome] {
	return func(ctx context.Context, c *Case) (*caseOutcome, error) {
		// One provider call, estimated before it is made. A zero estimate makes
		// the dollar cap unenforceable, so callers that care about it must
		// supply EstCostPerCallUSDMicros.
		est, err := o.estimate(ctx, c)
		if err != nil {
			return nil, err
		}

		resp, attempts, invokeErr := o.invokeWithRetry(ctx, c, est)
		if invokeErr != nil {
			return &caseOutcome{Response: nil, Err: invokeErr, Attempts: attempts}, invokeErr
		}

		score, scoreErr := o.Goal.Score(ctx, c, resp)
		if scoreErr != nil {
			return &caseOutcome{Response: resp, Err: scoreErr, Attempts: attempts}, scoreErr
		}

		// NOT counted here. An earlier version incremented the scored count on
		// the worker goroutine the moment scoring succeeded — before the sink
		// had persisted anything. A store failure mid-run then left the Run
		// claiming more scored Cases than the outcomes table held, and that
		// inflated count was the last thing durably recorded. Counting happens
		// after persistence, in emit, where errored Cases are already counted.
		return &caseOutcome{Response: resp, Score: score, Attempts: attempts}, nil
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
	est budget.Estimate,
) (*Response, int, error) {
	backoff := o.retryBackoff()
	// time.Now, not o.now. The budget bounds real elapsed time, and o.now is
	// injectable — a frozen clock (which BaselineOptions.Now's own godoc
	// invites for stable golden tests) made "now + wait > now + budget"
	// collapse to "wait > budget", turning a cumulative bound into a per-sleep
	// one. Measured under a frozen clock: 40 provider calls for one Case
	// against a 50ms budget.
	deadline := time.Now().Add(o.retryBudget())
	var lastErr error

	attempts := 0
	for attempt := 1; attempt <= o.maxAttempts(); attempt++ {
		attempts++
		resp, err := o.invokeOnce(ctx, c, est)
		if err == nil {
			return resp, attempts, nil
		}
		lastErr = err

		if !retryable(err) {
			return nil, attempts, err
		}
		if attempt == o.maxAttempts() {
			break
		}

		wait := backoff
		// A provider that says how long to wait is the authority on its own
		// limits; our doubling is only a guess for when it does not say.
		if after, ok := retryAfterOf(err); ok {
			wait = after
		}

		// Both bounds, whichever binds first. Stopping here rather than after
		// the wait means the budget is a bound on time SPENT, not on time
		// spent plus one more sleep.
		if time.Now().Add(wait).After(deadline) {
			break
		}

		// A ctx-aware wait. Sleeping through a cancellation would keep a
		// stopped run alive for the length of the backoff.
		select {
		case <-time.After(wait):
			backoff *= 2
		case <-ctx.Done():
			return nil, attempts, ctx.Err()
		}
	}
	return nil, attempts, lastErr
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
) (*Response, error) {
	res, err := o.Guard.Authorize(ctx, est)
	if err != nil {
		return nil, err
	}
	// Reached on every path: the agent erroring, the context cancelling
	// mid-call, a panic recovered by the executor. Without it each of those
	// leaks headroom and the guard eventually refuses work it should allow.
	defer res.Release()

	resp, invokeErr := o.Agent.Invoke(ctx, c)
	if invokeErr != nil {
		// The attempt still consumed a call, whether or not it answered.
		res.Settle(budget.Spend{Calls: 1})
		return nil, invokeErr
	}

	// The same computation the sink persists. Two independent derivations of
	// one Case's cost could drift, and the persisted one is what
	// Guard.Restore reads on resume.
	res.Settle(spendOf(resp))
	return resp, nil
}
