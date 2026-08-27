package core

import (
	"context"
	"fmt"
	"time"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/observe"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// invoker is the budget-and-retry core: authorize, call, settle, retry.
//
// Extracted from the Baseline stage rather than written for a second one,
// because a second copy is how two stages come to disagree about money. What
// lives here is not generic plumbing — it is six separately-discovered defects
// held in place by their fixes:
//
//   - the retry budget measured against a real clock, not an injectable one (a
//     frozen clock turned a cumulative bound into a per-sleep one: 40 provider
//     calls for one Case against a 50ms budget);
//   - billing accumulated across attempts, because only the last error survives
//     the loop and the guard settles each attempt separately;
//   - settled calls counted apart from attempts, because a refused Authorize
//     returns before settling anything;
//   - a recovered panic carrying its spend out, so a Case the guard charged
//     cannot be persisted as free;
//   - saturating arithmetic matching Guard.Settle, so the store and the guard
//     cannot disagree on exactly the inputs the clamp exists for;
//   - an overshoot RECORDED rather than returned, because returning it
//     discarded a paid, scoreable answer and then skipped the Case forever on
//     resume.
//
// A stage that reimplemented this would rediscover them, and each one is money.
// The two stage-specific parts — which events to emit — are hooks.
type invoker struct {
	// Agent is what gets called. Value builds one invoker per arm, because the
	// treatment arm's agent carries the Asset and the control arm's does not.
	Agent Agent

	// AgentRef names the scheme on the provider-call span.
	AgentRef *AgentRef

	// Guard authorizes every attempt and settles what it cost.
	Guard *budget.Guard

	// MaxAttempts, RetryBudget and RetryBackoff bound retries. Resolved values,
	// not options: defaulting belongs to the stage that owns the flags.
	MaxAttempts  int
	RetryBudget  time.Duration
	RetryBackoff time.Duration

	// Key is the measurement-key template the hooks receive: withRetry fills
	// CaseID from the Case it is invoking and leaves the rest as set. Baseline
	// leaves it zero; Value sets AssetID, Arm and Trial so the money events
	// are attributable to the measurement that caused them.
	Key store.MeasurementKey

	// OnOvershoot reports that settlement exceeded its reservation. The context
	// handed over is already detached from cancellation and carries its own
	// grace, because an overshoot happens exactly when a budget stop has
	// cancelled the worker. Nil is allowed and means "do not report".
	//
	// key identifies the measurement the overshoot belongs to: the Case alone
	// for Baseline, the full (Asset, Case, arm, trial) for Value — which is
	// what makes a retry or an overshoot attributable to the Asset that caused
	// it rather than to a Case both arms share.
	OnOvershoot func(ctx context.Context, key store.MeasurementKey, estimated, settled, overshoot int64)

	// OnRetry reports an attempt about to wait. Called BEFORE the wait: the
	// whole value of the signal is telling a watcher the run is obeying a
	// provider's backoff rather than hung, and emitted afterwards it announces
	// idleness only once idleness has ended. Same detached context. Nil is
	// allowed.
	OnRetry func(ctx context.Context, key store.MeasurementKey, attempt int, reason knov1.RetryReason, wait, remaining time.Duration)
}

// keyFor fills the Case side of the measurement-key template.
func (iv invoker) keyFor(c *Case) store.MeasurementKey {
	key := iv.Key
	key.CaseID = c.GetId()
	return key
}

// withRetry invokes one Case, retrying what is retryable within both bounds.
//
// Returns the spend the guard SETTLED, not the spend the estimate predicted, so
// a caller persists what was actually charged.
func (iv invoker) withRetry(
	ctx context.Context,
	c *Case,
	est budget.Estimate,
) (resp *Response, attempts int, billedUSDMicros, settledCallCount int64, err error) {
	backoff := iv.RetryBackoff
	// time.Now, not an injectable clock. The budget bounds real elapsed time,
	// and a frozen clock — which a stage's Now field invites for stable golden
	// tests — made "now + wait > now + budget" collapse to "wait > budget",
	// turning a cumulative bound into a per-sleep one. Measured under a frozen
	// clock: 40 provider calls for one Case against a 50ms budget.
	deadline := time.Now().Add(iv.RetryBudget)
	var lastErr error

	// Accumulated across attempts, because each one settles into the guard
	// separately and only the LAST error survives the loop. With MaxAttempts 3
	// the guard can settle three billed charges that the sink could recover at
	// most one of — and settled spend is the only durable record of money
	// spent, so the difference is headroom a resumed run spends twice.
	var billed int64

	// Calls the guard actually SETTLED, which is not the attempt count.
	// attempts++ runs at the top of the loop, before the call, and a refused
	// Authorize returns before settling anything — so a Case that made one real
	// call and was refused on attempt 2 would otherwise persist Calls: 2
	// against a guard that settled 1.
	var settledCalls int64

	// A panic must not take the money with it. The executor recovers, but it
	// discards the value — deliberately — so an outcome built after the unwind
	// would carry no spend, and the sink would persist zero for a Case the
	// guard had already settled. Measured before this: guard 3 calls, store 0.
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

	for attempt := 1; attempt <= iv.MaxAttempts; attempt++ {
		attempts++
		resp, settled, overshoot, invokeErr := iv.once(ctx, c, est, attempt)
		// Saturating, matching Guard.Settle. A plain add wraps where the guard
		// pins, so the store and the guard would disagree on exactly the inputs
		// the clamp exists for — and the store is the one that outlives the
		// process.
		billed = saturatingAdd(billed, settled.CostUSDMicros)
		settledCalls = saturatingAdd(settledCalls, settled.Calls)
		if overshoot > 0 && iv.OnOvershoot != nil {
			// Gated on the DELTA, so the count is bounded by concurrency rather
			// than by Case count: once the cap binds, only reservations already
			// in flight can overshoot.
			//
			// Reported, NOT returned. Returning it discarded a paid, scoreable
			// answer — the overshoot check runs before the success check — and
			// recorded the Case as an agent error while still writing its
			// outcome row, so a resume skipped it forever.
			//
			// WithoutCancel with its own grace, because a budget stop cancels
			// the worker context and a budget stop is exactly when an overshoot
			// happens.
			emitCtx, cancel := context.WithTimeout(
				context.WithoutCancel(ctx), progressWriteGrace,
			)
			iv.OnOvershoot(emitCtx, iv.keyFor(c), est.CostUSDMicros, settled.CostUSDMicros, overshoot)
			cancel()
		}
		if invokeErr == nil {
			return resp, attempts, billed, settledCalls, nil
		}
		lastErr = invokeErr

		if !retryable(invokeErr) {
			return nil, attempts, billed, settledCalls, invokeErr
		}
		if attempt == iv.MaxAttempts {
			break
		}

		wait := backoff
		// A provider that says how long to wait is the authority on its own
		// limits; our doubling is only a guess for when it does not say.
		if after, ok := retryAfterOf(invokeErr); ok {
			wait = after
		}

		// Both bounds, whichever binds first. Stopping here rather than after
		// the wait means the budget is a bound on time SPENT, not on time spent
		// plus one more sleep.
		if time.Now().Add(wait).After(deadline) {
			break
		}

		if iv.OnRetry != nil {
			// Reported rather than returned, for the same reason as the
			// overshoot: an event-store hiccup must not convert a retryable
			// Case into a terminal error and inflate the run's error rate past
			// its threshold.
			retryCtx, cancelRetry := context.WithTimeout(
				context.WithoutCancel(ctx), progressWriteGrace,
			)
			iv.OnRetry(retryCtx, iv.keyFor(c), attempt,
				retryReasonOf(invokeErr), wait, time.Until(deadline))
			cancelRetry()
		}

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

// once authorizes, calls, and settles a single attempt.
func (iv invoker) once(
	ctx context.Context,
	c *Case,
	est budget.Estimate,
	attempt int,
) (resp *Response, settled budget.Spend, overshoot int64, err error) {
	// SpanKindClient, because this is the call OUT of the process — which is
	// what makes a provider show up as a dependency edge in a trace rather than
	// as internal work, and what makes provider latency separable from ours.
	// Opened before Authorize so a budget refusal is visible as a call that
	// never happened rather than as a gap.
	ctx, span := observe.StartAgentCall(ctx, iv.AgentRef.GetScheme(), attempt)
	// Named return, so the panic path marks the span too. A recovered panic
	// unwinds through here with err set, and without this the provider-call
	// span ended Unset while its parent Case span was correctly marked failed —
	// a trace showing a healthy call inside a broken Case.
	defer func() {
		if err != nil {
			observe.Fail(span, codeOf(err))
		}
		span.End()
	}()

	res, err := iv.Guard.Authorize(ctx, est)
	if err != nil {
		return nil, budget.Spend{}, 0, err
	}
	// Reached on every path: the agent erroring, the context cancelling
	// mid-call, a panic recovered by the executor. Without it each of those
	// leaks headroom and the guard eventually refuses work it should allow.
	defer res.Release()

	resp, invokeErr := iv.Agent.Invoke(ctx, c)
	if invokeErr != nil {
		// The attempt consumed a call, and the provider may have CHARGED for
		// it: a 200 carrying both an error object and a usage block is a shape
		// several OpenAI-compatible gateways produce. Settling zero here is
		// what made it free.
		spend := budget.Spend{Calls: 1, CostUSDMicros: billedCostOf(invokeErr)}
		return nil, spend, res.Settle(spend), invokeErr
	}

	// The same computation the sink persists. Two independent derivations of
	// one Case's cost could drift, and the persisted one is what Guard.Restore
	// reads on resume.
	spend := spendOf(resp)

	// What the call actually cost and which model answered, on the span that
	// made it. The RESOLVED model, not the ref: a moving alias tells a reader
	// nothing about what was measured.
	span.SetAttributes(observe.ResolvedModel(resp.GetResolvedModel()))
	span.SetAttributes(observe.Tokens(resp.GetPromptTokens(), resp.GetCompletionTokens())...)
	span.SetAttributes(observe.CostUSDMicros(spend.CostUSDMicros))

	return resp, spend, res.Settle(spend), nil
}
