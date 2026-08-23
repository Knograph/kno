package core

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/executor"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// Calling the agent for one Case, and deciding what a failure means: whether
// to retry it, how long to wait, and what is settled when it is not retried.

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

// retryBudget is the configured wall-clock bound, or the default.
func (o BaselineOptions) retryBudget() time.Duration {
	if o.RetryBudget > 0 {
		return o.RetryBudget
	}
	return DefaultRetryBudget
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

// sinkFunc persists each outcome and emits its event.
// sinkFunc persists each Case's outcome.
//
// runCtx is the RUN's context, distinct from the ctx the sink is called with:
// the sink deliberately runs on a context that outlives cancellation so it can
// still write during shutdown, which means it cannot ask its own ctx whether
// the run is ending. draining covers the other way a run stops — a fatal error
// such as a budget stop, which never touches the caller's context.
func (o BaselineOptions) sinkFunc(runCtx context.Context, draining *atomic.Bool, agg *aggregator) executor.SinkFunc[*Case, caseOutcome] {
	return func(ctx context.Context, r executor.Result[*Case, caseOutcome]) error {
		// A Case refused by the budget guard was never attempted: no provider
		// call was made and nothing was spent. Recording it as a terminal
		// outcome would mark it complete, so a resumed run would SKIP it — the
		// Case would vanish from the run permanently, and the denominator
		// would shrink with nothing showing why.
		//
		// It is left unrecorded so the resume picks it up, which is the whole
		// point of stopping resumably rather than failing.
		//
		// A Case that could not be PRICED is the same shape: estimate() refuses
		// before Authorize is ever called, so no provider call was made and
		// nothing was spent. Recording it would charge a resumed run for a call
		// that never happened AND mark the Case done, so fixing the pricing
		// table and re-running with --resume would never re-attempt it.
		if errors.Is(r.Err, errs.ErrBudgetExceeded) || errors.Is(r.Err, errUnpriceable) {
			return nil
		}

		// A Case the shutdown cancelled before it produced anything is the same
		// shape again: the run is stopping resumably, and this Case has no
		// result to record.
		//
		// Recording it would mark it complete, so a resume would SKIP it — and
		// the run would report a smaller denominator than it measured, with
		// nothing saying why. Measured: a budget stop at concurrency 8 with a
		// 50ms agent recorded 2 errored Cases every single time, and the
		// resumed run scored 51 of 52 rather than 52. CI caught it as a flaky
		// test; it is not flaky, it is timing-dependent.
		//
		// The trade is deliberate. Not recording means the resumed run does not
		// restore whatever that attempt may have cost, so it gets slightly more
		// headroom than it should — bounded by concurrency, and already the
		// documented dark-spend window (docs/debt.md#20). Losing a Case from
		// the run permanently, silently, is the worse failure: prime directive
		// 5 is what makes the denominator behind every later delta mean
		// something.
		//
		// Two conditions, and both matter.
		//
		// The RUN's context must be done. A per-Case deadline against a healthy
		// run is a provider timeout: that Case genuinely failed, it is recorded,
		// and a run where enough of them time out is marked unusable. Skipping
		// those would hide a broken provider behind a shrinking denominator —
		// the opposite failure, and an existing test caught me making it.
		//
		// And there must be no Response. A Case that failed AFTER a paid call
		// produced one is a real terminal outcome, recorded below with the
		// spend that call incurred.
		shuttingDown := runCtx.Err() != nil || draining.Load()
		cancelled := errors.Is(r.Err, context.Canceled) || errors.Is(r.Err, context.DeadlineExceeded)
		noResult := r.Value == nil || r.Value.Response == nil

		if shuttingDown && cancelled && noResult {
			return nil
		}

		out := &store.Outcome{CaseID: r.Item.GetId()}

		switch {
		case r.Done():
			out.Response = r.Value.Response
			out.Score = r.Value.Score
			out.Spend = spendOfN(r.Value.Response, int64(r.Value.Attempts))
		default:
			out.Err = codeOf(r.Err)
			// A Case can fail AFTER a paid call — a Goal erroring on malformed
			// output, for instance. A flat one-call spend there understates
			// what was actually spent, and SettledSpend is what Guard.Restore
			// reads on resume: the resumed process would believe less was
			// spent than really was, reopening the amnesia M1-0 closed.
			if r.Value != nil && r.Value.Response != nil {
				out.Response = r.Value.Response
				out.Spend = spendOfN(r.Value.Response, int64(r.Value.Attempts))
			} else {
				// No Response, which is the retry-EXHAUSTED path — every
				// attempt failed. Hardcoding one call here is what made the
				// headline fix miss the branch it was written for: measured 5
				// persisted against 15 settled with MaxAttempts 3.
				out.Spend = budget.Spend{Calls: attemptsOf(r.Value)}
			}
		}

		if err := o.Store.RecordOutcome(ctx, o.RunID, out); err != nil {
			return fmt.Errorf("recording %s: %w", r.Item.GetId(), err)
		}
		return o.emit(ctx, r, agg)
	}
}

// caseOutcome is one Case's result inside the executor.
type caseOutcome struct {
	// Attempts is how many provider calls this Case took. Persisted so the
	// store's spend matches what the guard actually settled.
	Attempts int

	Response *Response
	Score    *Score
	Err      error
}
