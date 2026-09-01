package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/executor"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// ScorePass invokes agent once per Case and scores each answer against goal,
// under the budget guard, with the same retry and settlement behaviour every
// other stage gets — core.invoker (core/invoke.go), the six-separately-
// discovered-defects retry core Baseline, Value and Validate already share.
// This is the narrow seam the tuner-bridge plan's eval-seam amendment calls
// for: bridge needs to invoke a deployed model over Cases and score it, and
// the alternative (re-implementing authorize/call/settle/retry a fourth
// time) is how a fourth caller comes to disagree with the other three about
// money.
//
// ScorePass creates no Run and writes no measurement rows. Persistence is
// the CALLER's job, through OnScored — see ScoreParams.Skip and OnScored's
// doc for why: the Case is the unit of spend, and a bulk in-memory return
// with no per-Case checkpoint cannot express "resume must never re-pay for
// a Case already scored."
//
// The doc this seam originally shipped with said bridge would pair every
// group's scores "against the all-in baseline model's score for the same
// Case, which bridge.Run already holds" — that was false (see
// docs/plans/2026-09-01-bridge-eval-seam.md's Phase 1 review, finding R1),
// and ScoreResult.Scores is deliberately RAW scores keyed by Case ID, never
// deltas: pairing belongs to bridge.Run, the component that legitimately
// holds both a group's scores and the all-in baseline's.
func ScorePass(ctx context.Context, p ScoreParams) (*ScoreResult, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}

	cases, err := p.Cases.Cases(ctx)
	if err != nil {
		return nil, fmt.Errorf("core: score pass: reading cases: %w", err)
	}

	result := &ScoreResult{Scores: map[string]float64{}, Errors: map[string]string{}}
	var mu sync.Mutex

	dir := 1.0
	if p.Goal.Direction() == knov1.Direction_DIRECTION_MINIMIZE {
		dir = -1.0
	}

	iv := p.invoker()

	work := func(ctx context.Context, c *Case) (out *scoreOutcome, err error) {
		var billed, settledCalls int64
		var attempts int
		defer func() {
			if r := recover(); r != nil {
				out = &scoreOutcome{
					Attempts: attempts, BilledUSDMicros: billed, SettledCalls: settledCalls,
					Err: fmt.Errorf("panic scoring case %s: %T", c.GetId(), r),
				}
				err = out.Err
			}
		}()

		est, eerr := p.estimate(ctx, c)
		if eerr != nil {
			return nil, eerr
		}

		var resp *Response
		var ierr error
		resp, attempts, billed, settledCalls, ierr = iv.withRetry(ctx, c, est)
		if ierr != nil {
			return &scoreOutcome{
				Err: ierr, Attempts: attempts, BilledUSDMicros: billed, SettledCalls: settledCalls,
			}, ierr
		}

		score, serr := p.Goal.Score(ctx, c, resp)
		if serr != nil {
			return &scoreOutcome{
				Response: resp, Err: serr, Attempts: attempts,
				BilledUSDMicros: billed, SettledCalls: settledCalls,
			}, serr
		}
		return &scoreOutcome{
			Response: resp, Score: score, Attempts: attempts,
			BilledUSDMicros: billed, SettledCalls: settledCalls,
		}, nil
	}

	sink := func(ctx context.Context, r executor.Result[*Case, scoreOutcome]) error {
		id := r.Item.GetId()
		out := r.Value
		if out == nil {
			out = &scoreOutcome{Err: r.Err}
		}
		spend := budget.Spend{
			Calls:         out.SettledCalls,
			CostUSDMicros: out.BilledUSDMicros,
			Tokens:        out.Response.GetPromptTokens() + out.Response.GetCompletionTokens(),
		}

		mu.Lock()
		result.Spent = addSpend(result.Spent, spend)
		var normalized float64
		scored := out.Err == nil && out.Score != nil
		if scored {
			normalized = dir * out.Score.GetValue()
			result.Scores[id] = normalized
		} else {
			result.Errors[id] = codeOf(out.Err)
		}
		mu.Unlock()

		if scored && p.OnScored != nil {
			if err := p.OnScored(ctx, id, normalized, spend); err != nil {
				return fmt.Errorf("recording the score for case %s: %w", id, err)
			}
		}
		return nil
	}

	_, runErr := executor.Run(ctx, cases, work, sink, executor.Options{
		Concurrency: p.concurrency(),
		ID:          func(item any) string { c, _ := item.(*Case); return c.GetId() },
		Skip:        p.Skip,
		IsFatal: func(err error) bool {
			if errors.Is(err, errs.ErrBudgetExceeded) {
				return true
			}
			return runFatalOf(err)
		},
	})
	if runErr != nil {
		return result, runErr
	}
	return result, nil
}

// ScoreParams configures one ScorePass.
type ScoreParams struct {
	// Agent is the deployed model to invoke.
	Agent Agent

	// AgentRef names the scheme on the provider-call span.
	AgentRef *AgentRef

	// Goal scores each Response. Its Direction is applied to every score
	// this pass returns — see ScoreResult.Scores.
	Goal Goal

	// Cases is a *SealedEvals, not a bare iter.Seq2. core/seal.go:19-22
	// records "convention plus a canary" as a design tried and rejected for
	// exactly this reason: a canary test proves only that one call site
	// behaved on the day it was written, while a compile-time requirement
	// cannot be forgotten in a later refactor. core.Baseline already takes
	// the sealed type and calls .Cases(ctx) internally; ScorePass does the
	// same.
	Cases *SealedEvals

	// Guard authorizes and settles every call this pass makes.
	Guard *budget.Guard

	// AcceptFreeCalls asserts that every Invoke this pass makes is already
	// paid for through a channel other than the per-call budget guard.
	//
	// The bridge is the caller this exists for: a Together dedicated
	// endpoint is reserved capacity billed per minute per replica, settled
	// forward by its own hosting ticker (bridge/hosting.go); inference on
	// it is zero-marginal, already covered by that ticker's dollars, and
	// charging a per-call estimate on top would double-count the same
	// money. core/ring0.go's Estimator doc states the engine's default
	// rule: "a zero estimate makes a dollar cap unenforceable, so the
	// engine treats a zero-cost answer under a cost cap exactly as it
	// treats an error" — an UNASSERTED zero must not be trusted. This flag
	// is the assertion, mirroring cli/baseline.go:346's
	// `AcceptUnknownCost: f.acceptUnknownCost || f.costPerCallSet` and its
	// own reasoning: "an explicit zero is a claim that the calls are free;
	// an absent flag is no claim." When true, ScorePass estimates every
	// Case at budget.Estimate{Calls: 1} (zero cost) unconditionally.
	AcceptFreeCalls bool

	// EstCostPerCallUSDMicros is the fallback per-call estimate when Agent
	// does not implement Estimator and AcceptFreeCalls is false. Mirrors
	// BaselineOptions.EstCostPerCallUSDMicros.
	EstCostPerCallUSDMicros int64

	// Concurrency bounds in-flight work. Zero picks a conservative default.
	Concurrency int

	// MaxAttempts, RetryBudget and RetryBackoff bound retries. Zero means
	// the same defaults Baseline and Value use.
	MaxAttempts               int
	RetryBudget, RetryBackoff time.Duration

	// Skip reports whether a Case is already scored and should not be
	// invoked again. Nil means score everything. This is how a resumed
	// bridge group avoids re-paying for Cases a prior process already
	// scored and durably recorded through OnScored.
	Skip func(caseID string) bool

	// OnScored is called once a Case's score is settled — durably, as it
	// happens, not batched at the end. An error stops the pass. This is
	// what makes ScorePass's resume story work at all: the caller (bridge)
	// persists each Case's score the moment it lands, so a process killed
	// mid-pass leaves exactly the Cases it paid for recorded, and none
	// re-paid for on resume.
	OnScored func(ctx context.Context, caseID string, score float64, spend budget.Spend) error

	// OnOvershoot and OnRetry mirror invoker's hooks of the same name. Nil
	// is allowed and means "do not report". The measurement key they
	// receive carries only the Case ID: ScorePass has no Asset or Arm
	// concept of its own, and a caller that wants one (bridge: the
	// ablation group) enriches the key itself before emitting.
	OnOvershoot func(ctx context.Context, key store.MeasurementKey, estimated, settled, overshoot int64)
	OnRetry     func(ctx context.Context, key store.MeasurementKey, attempt int, reason knov1.RetryReason, wait, remaining time.Duration)
}

// ScoreResult is what one ScorePass produced.
type ScoreResult struct {
	// Scores maps Case ID to its score, direction-normalised: multiplied by
	// -1 when Goal.Direction() is DIRECTION_MINIMIZE, so that a HIGHER
	// number always means better regardless of the Goal's natural polarity
	// — the same convention core/value_measure.go's pairs applies to
	// deltas, applied here to a raw score instead. Raw, never a delta:
	// pairing against another pass's scores is the caller's job.
	Scores map[string]float64

	// Errors maps Case ID to its terminal failure code, for Cases that did
	// not score. A Case present in neither Scores nor Errors was never
	// reached (Skip returned true, or the pass stopped early).
	Errors map[string]string

	// Spent is every dimension the guard settled across the whole pass —
	// Calls, CostUSDMicros and Tokens. Dropping Tokens has been the same
	// bug twice (#170, #172); it is carried here explicitly rather than
	// left to a caller that only remembers CostUSDMicros.
	Spent budget.Spend
}

// scoreOutcome is one Case's raw result, before ScorePass's sink turns it
// into ScoreResult entries and an OnScored call.
type scoreOutcome struct {
	Response                      *Response
	Score                         *Score
	Err                           error
	Attempts                      int
	BilledUSDMicros, SettledCalls int64
}

func (p ScoreParams) validate() error {
	switch {
	case p.Agent == nil:
		return errors.New("core: score pass needs an agent")
	case p.AgentRef == nil:
		return errors.New("core: score pass needs an agent ref")
	case p.Goal == nil:
		return errors.New("core: score pass needs a goal")
	case p.Cases == nil:
		return errors.New("core: score pass needs sealed evals")
	case p.Guard == nil:
		return errors.New("core: score pass needs a budget guard")
	case p.Goal.Direction() == knov1.Direction_DIRECTION_UNSPECIFIED:
		return errors.New("core: the goal must report a direction, or the sign of every " +
			"score this pass produces is uninterpretable")
	}
	return nil
}

func (p ScoreParams) maxAttempts() int {
	if p.MaxAttempts > 0 {
		return p.MaxAttempts
	}
	return DefaultMaxAttempts
}

func (p ScoreParams) retryBudget() time.Duration {
	if p.RetryBudget > 0 {
		return p.RetryBudget
	}
	return DefaultRetryBudget
}

func (p ScoreParams) retryBackoff() time.Duration {
	if p.RetryBackoff > 0 {
		return p.RetryBackoff
	}
	return DefaultRetryBackoff
}

func (p ScoreParams) concurrency() int {
	if p.Concurrency > 0 {
		return p.Concurrency
	}
	return executor.DefaultConcurrency()
}

// invoker builds the shared budget-and-retry core with ScorePass's hooks.
func (p ScoreParams) invoker() invoker {
	return invoker{
		Agent: p.Agent, AgentRef: p.AgentRef, Guard: p.Guard,
		MaxAttempts: p.maxAttempts(), RetryBudget: p.retryBudget(), RetryBackoff: p.retryBackoff(),
		OnOvershoot: p.OnOvershoot, OnRetry: p.OnRetry,
	}
}

// estimate reports what one invocation of c may cost. Mirrors
// BaselineOptions.estimate exactly, with one addition: AcceptFreeCalls
// short-circuits to an asserted-free estimate before any of Baseline's
// unpriced-under-a-cap refusals can fire. See AcceptFreeCalls's doc for why
// that is safe here and would not be safe as a default.
func (p ScoreParams) estimate(ctx context.Context, c *Case) (budget.Estimate, error) {
	if p.AcceptFreeCalls {
		return budget.Estimate{Calls: 1}, nil
	}

	e, ok := p.Agent.(Estimator)
	if !ok {
		return budget.Estimate{Calls: 1, CostUSDMicros: p.EstCostPerCallUSDMicros}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, estimateTimeout)
	defer cancel()

	est, err := e.Estimate(ctx, c)
	capped := p.Guard.Limits().MaxCostUSDMicros > 0
	switch {
	case err != nil && capped:
		return budget.Estimate{}, p.unpriceable(c, err)
	case err != nil:
		return budget.Estimate{Calls: 1, CostUSDMicros: p.EstCostPerCallUSDMicros}, nil
	case est.CostUSDMicros <= 0 && capped:
		return budget.Estimate{}, p.unpriceable(c,
			errors.New("the estimate is zero, which a cost cap cannot be enforced against"))
	case est.Calls != 1:
		return budget.Estimate{}, p.unpriceable(c,
			fmt.Errorf("the estimate reserves %d calls, but one Invoke settles as "+
				"exactly one call", est.Calls))
	}
	return est, nil
}

func (p ScoreParams) unpriceable(c *Case, cause error) error {
	return errs.ErrInvalidInput.WithFix(
		"drop the cost cap to run without one, use an agent that can price this " +
			"model, or pass AcceptFreeCalls when these calls are already paid for elsewhere",
	).
		Wrap(fmt.Errorf("cannot price case %s, and a cost cap cannot be enforced "+
			"against an unknown cost: %w: %w", c.GetId(), errUnpriceable, cause))
}

// addSpend sums two budget.Spend values dimension by dimension, saturating
// rather than wrapping — the same discipline saturatingAdd applies to a
// single dimension in baseline_invoke.go, extended to all three so dropping
// Tokens here cannot become the same bug a third time.
func addSpend(total, add budget.Spend) budget.Spend {
	return budget.Spend{
		Calls:         saturatingAdd(total.Calls, add.Calls),
		CostUSDMicros: saturatingAdd(total.CostUSDMicros, add.CostUSDMicros),
		Tokens:        saturatingAdd(total.Tokens, add.Tokens),
	}
}
