package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/executor"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
	"google.golang.org/protobuf/proto"
)

// DefaultMaxErrorRate is the share of errored Cases above which a Run is no
// longer a usable reference.
//
// A baseline is what every later delta is measured against. One computed over
// a run where a third of the Cases never got an answer is not a baseline, it is
// a partial sample dressed as one — so the Run is marked rather than silently
// treated as clean by the stages that read it.
const DefaultMaxErrorRate = 0.05

// DefaultMaxAttempts is how many times one Case is tried when the provider
// rate limits it.
//
// A 429 is the provider asking us to slow down, not the Case failing.
// Recording it as terminal throws away capacity already paid for, and ordinary
// throttling would push a run past max_error_rate — marking a perfectly good
// baseline unusable for no statistical reason.
const DefaultMaxAttempts = 3

// DefaultRetryBackoff is the delay before the first retry. It doubles after
// each attempt.
const DefaultRetryBackoff = 500 * time.Millisecond

// estimateTimeout bounds an Estimator call.
//
// Estimating is arithmetic over a local pricing table — the Estimator godoc
// says so — and this exists for the adapter that does not honor that. Generous
// enough that no honest implementation notices, short enough that a hung one
// costs a single Case rather than the run.
const estimateTimeout = 5 * time.Second

// BaselineOptions configures a Baseline run.
type BaselineOptions struct {
	// RunID identifies this run. Required.
	RunID string

	// Agent is what gets measured.
	Agent Agent

	// AgentRef is how the agent was named, for the record.
	AgentRef *AgentRef

	// Goal scores each Response.
	Goal Goal

	// GoalName identifies the Goal on the Run.
	GoalName string

	// Guard authorizes spend. Required: there is no path in this stage that
	// calls an Agent without passing through it.
	Guard *budget.Guard

	// Store persists outcomes and events.
	Store store.Store

	// Concurrency bounds in-flight work.
	Concurrency int

	// Resume continues an existing run rather than starting one.
	Resume bool

	// InputFingerprint identifies the inputs. A resume whose fingerprint
	// differs from the recorded one is refused.
	InputFingerprint string

	// EvalContentHash is the eval source's own hash, so a refusal can name
	// which input changed rather than only that something did.
	EvalContentHash string

	// SplitSeed and HoldoutFrac are recorded for reproducibility.
	SplitSeed   string
	HoldoutFrac float64

	// DevCases and HoldoutCases are the counts from ingestion.
	//
	// DEBT(docs/debt.md#28): the CALLER owns their accuracy. The stage checks
	// only that neither is zero — a run with nothing in dev measures nothing,
	// and a run with no holdout can never be validated. It does not verify the
	// numbers against what the sealed source actually yields, because counting
	// would mean consuming the iterator a second time. A caller whose split
	// computation is wrong produces a Run and an event stream that misreport
	// their own denominator, and nothing here will notice.
	DevCases     int
	HoldoutCases int

	// HoldoutUnderpowered marks a holdout too small for a meaningful interval.
	HoldoutUnderpowered bool

	// MaxErrorRate overrides DefaultMaxErrorRate. Zero uses the default.
	MaxErrorRate float64

	// MaxAttempts bounds how many times one Case is tried when the provider
	// rate limits. Zero means DefaultMaxAttempts; 1 disables retry.
	MaxAttempts int

	// RetryBackoff is the delay before the first retry, doubling thereafter.
	// Zero means DefaultRetryBackoff.
	RetryBackoff time.Duration

	// EstCostPerCallUSDMicros is what one Agent call is expected to cost.
	//
	// The guard cannot refuse what it was not told about: authorizing with a
	// zero cost estimate means the dollar cap is only ever discovered at
	// settlement, after the money is spent, and a run can overshoot it by up
	// to concurrency calls. An earlier version did exactly that and exceeded a
	// $0.10 cap by $0.01.
	//
	// Deliberately crude — a real pricing model arrives with a real adapter,
	// per the M1 plan — but a wrong estimate that is authorized is still
	// safer than no estimate at all, because the difference is reconciled at
	// settlement.
	EstCostPerCallUSDMicros int64

	// Now returns the current time. Nil uses time.Now. Injected so golden
	// tests over a Run are stable.
	Now func() time.Time
}

func (o BaselineOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now().UTC()
}

func (o BaselineOptions) maxAttempts() int {
	if o.MaxAttempts > 0 {
		return o.MaxAttempts
	}
	return DefaultMaxAttempts
}

func (o BaselineOptions) retryBackoff() time.Duration {
	if o.RetryBackoff > 0 {
		return o.RetryBackoff
	}
	return DefaultRetryBackoff
}

func (o BaselineOptions) maxErrorRate() float64 {
	if o.MaxErrorRate > 0 {
		return o.MaxErrorRate
	}
	return DefaultMaxErrorRate
}

// BaselineResult reports what a Baseline run produced.
type BaselineResult struct {
	// Run is the persisted record.
	Run *knov1.Run

	// AggregateScore is the mean over SCORED Cases only. Absent when nothing
	// scored: a run that scored nothing has no mean, and zero would be
	// indistinguishable from a real mean of zero.
	AggregateScore *float64

	// Stats is what the executor did.
	Stats executor.Stats

	// Spent is what the run actually cost, settled.
	//
	// Carried here rather than on the Run because it is the guard's number,
	// not the schema's — and a caller reporting spend should read what the
	// guard settled rather than re-deriving it from stored outcomes.
	Spent budget.Spend
}

// Baseline runs the agent over the dev Cases and scores each Response.
//
// It takes a *SealedEvals, not an Evals. That is a compile-time guarantee that
// this stage cannot read the holdout: every statistical claim the tool makes
// downstream depends on the holdout being untouched until Validate, and a
// requirement enforced by the type cannot be forgotten in a later refactor.
//
// Every Agent call passes through the budget guard. There is no path in this
// function that spends without an authorized reservation, and every
// reservation is released or settled on every path.
func Baseline(
	ctx context.Context,
	evals *SealedEvals,
	opts BaselineOptions,
) (*BaselineResult, error) {
	if err := opts.validate(evals); err != nil {
		return nil, err
	}

	run, err := opts.openRun(ctx)
	if err != nil {
		return nil, err
	}

	// Resume needs two things from the store, and both must happen before any
	// work is dispatched: what is already done, and what was already spent.
	done := map[string]struct{}{}
	if opts.Resume {
		if done, err = opts.Store.CompletedCases(ctx, opts.RunID); err != nil {
			return nil, fmt.Errorf("loading completed cases: %w", err)
		}
		spent, err := opts.Store.SettledSpend(ctx, opts.RunID)
		if err != nil {
			return nil, fmt.Errorf("loading prior spend: %w", err)
		}
		// The guard is in-memory. Without this a resumed run believes it has
		// spent nothing and can consume its cap a second time.
		opts.Guard.Restore(spent)
	}

	cases, err := evals.Cases(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading evals: %w", err)
	}

	agg := &aggregator{}
	if opts.Resume {
		// Continue numbering rather than restarting at 1, which would collide
		// with events from before the interruption and silently defeat the
		// gap detection Event.sequence exists for.
		maxSeq, err := opts.Store.MaxEventSequence(ctx, opts.RunID)
		if err != nil {
			return nil, fmt.Errorf("reading event sequence: %w", err)
		}
		agg.seedSequence(maxSeq)

		// Seed the counts too. Without this the aggregate covers only the work
		// THIS process did, so a run interrupted after 24 Cases and resumed for
		// 36 more would report 36 — losing the Cases the first run paid for and
		// understating the denominator behind every later delta.
		//
		// DEBT(docs/debt.md#27): only the COUNTS are seeded. The mean is not,
		// so a resumed run's AggregateScore is the mean over the Cases this
		// process scored, not over the whole run.
		priorScored, priorErrored, err := opts.Store.OutcomeCounts(ctx, opts.RunID)
		if err != nil {
			return nil, fmt.Errorf("loading prior outcome counts: %w", err)
		}
		agg.seedCounts(priorScored, priorErrored)
	}
	if err := opts.emitRunStarted(ctx, agg, opts.DevCases); err != nil {
		return nil, err
	}
	stats, runErr := executor.Run(ctx, cases,
		opts.workFunc(), opts.sinkFunc(agg),
		executor.Options{
			Concurrency: opts.Concurrency,
			ID:          func(item any) string { c, _ := item.(*Case); return c.GetId() },
			Skip:        func(id string) bool { _, ok := done[id]; return ok },
			// Budget exhaustion ends the run rather than failing every
			// remaining Case one at a time.
			IsFatal: func(err error) bool { return errors.Is(err, errs.ErrBudgetExceeded) },
		})

	return opts.closeRun(ctx, run, agg, stats, runErr)
}

func (o BaselineOptions) validate(evals *SealedEvals) error {
	switch {
	case evals == nil:
		return errors.New("core: baseline needs a sealed evals source")
	case o.RunID == "":
		return errors.New("core: baseline needs a run id")
	case o.Agent == nil:
		return errors.New("core: baseline needs an agent")
	case o.Goal == nil:
		return errors.New("core: baseline needs a goal")
	case o.Guard == nil:
		// Not defaulted to an unlimited guard. A missing guard is a
		// programmer error on a spend path, and quietly substituting one that
		// permits everything is how prime directive 4 gets violated by
		// omission.
		return errors.New("core: baseline needs a budget guard")
	case o.Store == nil:
		return errors.New("core: baseline needs a store")
	case o.Guard.Limits().MaxCostUSDMicros > 0 && o.EstCostPerCallUSDMicros <= 0:
		// The guard cannot refuse what it was not told about. A dollar cap
		// with a zero estimate is only discovered at settlement, after the
		// money is spent — which already caused a real overshoot once.
		// User-reachable, unlike the nil-field cases above, so it carries the
		// grammar: what failed, why, and the flag to pass.
		return errs.ErrInvalidInput.WithFix(
			"pass --cost-per-call-usd alongside --max-cost-usd").
			Wrap(errors.New("a run with a cost cap needs a per-call cost estimate, " +
				"or the cap is only enforced after the money is spent"))
	case o.Goal.Direction() == knov1.Direction_DIRECTION_UNSPECIFIED:
		return errors.New("core: the goal must report a direction, or the sign of every " +
			"number this run produces is uninterpretable")
	case o.DevCases <= 0:
		// A run with nothing in dev measures nothing. Checked here rather than
		// only at the CLI edge: the rule is a property of the stage, not of one
		// front end, and api/tui/plugins call this same function.
		return errs.ErrInvalidInput.WithFix(
			"add Cases, or lower the holdout fraction").
			Wrap(errors.New("no Cases landed in dev, leaving nothing to measure"))
	case o.HoldoutCases <= 0:
		// The refusal DESIGN.md advertises as an engine property. A run that
		// can never produce a holdout number is not a run: every later stage
		// would compute against a reference with no honest confirmation.
		return errs.ErrInvalidInput.WithFix(
			"add Cases, or raise the holdout fraction").
			Wrap(errors.New("no Cases landed in the holdout, so this run can never be validated"))
	}
	return nil
}

// openRun creates or reloads the Run record.
func (o BaselineOptions) openRun(ctx context.Context) (*knov1.Run, error) {
	if o.Resume {
		run, err := o.Store.GetRun(ctx, o.RunID)
		if err != nil {
			return nil, fmt.Errorf("loading run %s: %w", o.RunID, err)
		}
		if err := o.checkResumable(run); err != nil {
			return nil, err
		}
		return run, nil
	}

	run := &knov1.Run{
		Id:                  o.RunID,
		Stage:               knov1.Stage_STAGE_BASELINE,
		CreatedAt:           o.now().Format(time.RFC3339),
		Agent:               o.AgentRef,
		GoalName:            o.GoalName,
		GoalDirection:       o.Goal.Direction(),
		Status:              knov1.RunStatus_RUN_STATUS_RUNNING,
		InputFingerprint:    o.InputFingerprint,
		EvalContentHash:     o.EvalContentHash,
		SplitSeed:           o.SplitSeed,
		HoldoutFrac:         o.HoldoutFrac,
		DevCaseCount:        int32(o.DevCases),     //nolint:gosec // bounded by the eval set
		HoldoutCaseCount:    int32(o.HoldoutCases), //nolint:gosec // bounded by the eval set
		HoldoutUnderpowered: o.HoldoutUnderpowered,
	}
	if err := o.Store.CreateRun(ctx, run); err != nil {
		return nil, fmt.Errorf("creating run %s: %w", o.RunID, err)
	}
	return run, nil
}

// checkResumable refuses a resume whose measurement configuration differs from
// the one recorded.
//
// InputFingerprint is supplied by the caller and covers the caller's inputs;
// core cannot assume it covers the Goal or the Agent, and the one caller that
// exists today does not fold them in. So the recorded fields are compared
// directly rather than by a convention every future caller has to remember.
//
// Without this, `--run-id r --agent A` followed by `--run-id r --resume --agent
// B` silently blends Cases scored under two different agents into one
// AggregateScore and presents it as a single homogeneous number — the
// corrupted-reference failure prime directive 5 exists to prevent.
func (o BaselineOptions) checkResumable(run *knov1.Run) error {
	var changed string
	switch {
	case run.GetInputFingerprint() != o.InputFingerprint:
		changed = "different inputs"
	case run.GetGoalName() != o.GoalName:
		changed = fmt.Sprintf("a different goal (recorded %q, now %q)", run.GetGoalName(), o.GoalName)
	case run.GetGoalDirection() != o.Goal.Direction():
		changed = fmt.Sprintf("a different goal direction (recorded %v, now %v)",
			run.GetGoalDirection(), o.Goal.Direction())
	case run.GetAgent().GetRef() != o.AgentRef.GetRef():
		changed = fmt.Sprintf("a different agent (recorded %q, now %q)",
			run.GetAgent().GetRef(), o.AgentRef.GetRef())
	default:
		return nil
	}
	return errs.ErrCheckpointStale.WithFix(o.staleFix(run)).
		Wrap(fmt.Errorf("run %s was recorded against %s", o.RunID, changed))
}

// staleFix names which input changed, rather than only that something did.
func (o BaselineOptions) staleFix(run *knov1.Run) string {
	if run.GetEvalContentHash() != o.EvalContentHash {
		return "the eval source changed since this run started; re-run without --resume, " +
			"or restore the original file"
	}
	return "the goal, agent, or split configuration changed since this run started; " +
		"re-run without --resume, or restore the setting it was recorded against"
}

// estimate reports what one invocation of c may cost.
//
// An adapter implementing Estimator answers per Case; anything else falls back
// to the run-scoped scalar, which is what the fake and every M1 caller use.
//
// A Estimator that cannot price a Case is a refusal when a dollar cap is set,
// never a zero: the guard cannot refuse what it was not told about, and a zero
// estimate is precisely what made a cap unenforceable in M1.
func (o BaselineOptions) estimate(ctx context.Context, c *Case) (budget.Estimate, error) {
	e, ok := o.Agent.(Estimator)
	if !ok {
		return budget.Estimate{Calls: 1, CostUSDMicros: o.EstCostPerCallUSDMicros}, nil
	}

	// Bounded, because Estimate runs BEFORE Authorize on the worker's own
	// context. An adapter that hangs here would pin a worker for the life of
	// the run, and the godoc's "must be local" is a contract, not a guarantee.
	ctx, cancel := context.WithTimeout(ctx, estimateTimeout)
	defer cancel()

	est, err := e.Estimate(ctx, c)
	capped := o.Guard.Limits().MaxCostUSDMicros > 0

	switch {
	case err != nil && capped:
		return budget.Estimate{}, o.unpriceable(c, err)
	case err != nil:
		// No dollar cap: the call cap still applies, and refusing the run
		// because a price is unknown would be worse than running uncapped when
		// the user asked for uncapped.
		return budget.Estimate{Calls: 1, CostUSDMicros: o.EstCostPerCallUSDMicros}, nil

	// A zero cost is not a cheap Case, it is an absent answer — the natural
	// output of a pricing-table miss coded as a zero row. Accepting it puts the
	// cap back to being discovered at settlement, which is the M1 failure this
	// whole interface exists to close, reached through the interface.
	case est.CostUSDMicros <= 0 && capped:
		return budget.Estimate{}, o.unpriceable(c,
			errors.New("the estimate is zero, which a cost cap cannot be enforced against"))

	// One Invoke settles as exactly one call (see spendOf), so an Estimate
	// reserving more would reserve N and settle 1, and the call cap would drift
	// by (N-1) per Case. Measured at 18 real calls against a cap of 10.
	// Rejected rather than coerced: silently rewriting one out-of-contract
	// field hides the adapter bug a reviewer needs to see.
	case est.Calls != 1:
		return budget.Estimate{}, o.unpriceable(c,
			fmt.Errorf("the estimate reserves %d calls, but one Invoke settles as "+
				"exactly one call", est.Calls))
	}
	return est, nil
}

// errUnpriceable marks a Case the guard was never asked to authorize.
//
// A sentinel rather than an inspection of the message, so the sink can tell
// "refused before any call" from "failed after a paid call" — the two have
// opposite correct handling, and only one of them is resumable work.
var errUnpriceable = errors.New("core: case cannot be priced")

// unpriceable renders a refusal to authorize a Case.
//
// The fix names things that actually work. An explicit --cost-per-call-usd does
// NOT override an Estimator, so advertising it would send the user down a dead
// end — which the first version of this message did.
func (o BaselineOptions) unpriceable(c *Case, cause error) error {
	return errs.ErrInvalidInput.WithFix(
		"drop --max-cost-usd to run without a dollar cap, or use an agent that " +
			"can price this model").
		Wrap(fmt.Errorf("cannot price case %s, and a cost cap cannot be enforced "+
			"against an unknown cost: %w: %w", c.GetId(), errUnpriceable, cause))
}

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

		resp, invokeErr := o.invokeWithRetry(ctx, c, est)
		if invokeErr != nil {
			return &caseOutcome{Response: nil, Err: invokeErr}, invokeErr
		}

		score, scoreErr := o.Goal.Score(ctx, c, resp)
		if scoreErr != nil {
			return &caseOutcome{Response: resp, Err: scoreErr}, scoreErr
		}

		// NOT counted here. An earlier version incremented the scored count on
		// the worker goroutine the moment scoring succeeded — before the sink
		// had persisted anything. A store failure mid-run then left the Run
		// claiming more scored Cases than the outcomes table held, and that
		// inflated count was the last thing durably recorded. Counting happens
		// after persistence, in emit, where errored Cases are already counted.
		return &caseOutcome{Response: resp, Score: score}, nil
	}
}

// invokeWithRetry calls the Agent, backing off when the provider rate limits.
//
// Each attempt takes its OWN reservation and settles it. A retry is a new
// provider call: it consumes call budget whether or not it produces an answer,
// and pretending otherwise would let a throttled run spend past its cap
// invisibly.
//
// Only ErrRateLimited is retried. A 429 is the provider asking us to slow
// down, not the Case failing — recording it as terminal throws away capacity
// already paid for, and ordinary throttling would push a run past
// max_error_rate and mark a perfectly good baseline unusable for no
// statistical reason. An agent that returned a 500 or malformed output did
// fail, and retrying it burns budget on a Case that will fail again.
func (o BaselineOptions) invokeWithRetry(
	ctx context.Context,
	c *Case,
	est budget.Estimate,
) (*Response, error) {
	backoff := o.retryBackoff()
	var lastErr error

	for attempt := 1; attempt <= o.maxAttempts(); attempt++ {
		resp, err := o.invokeOnce(ctx, c, est)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		// A budget refusal is not retryable: the cap will not have moved, and
		// re-authorizing would spin.
		if !errors.Is(err, errs.ErrRateLimited) || errors.Is(err, errs.ErrBudgetExceeded) {
			return nil, err
		}
		if attempt == o.maxAttempts() {
			break
		}

		// A ctx-aware wait. Sleeping through a cancellation would keep a
		// stopped run alive for the length of the backoff.
		select {
		case <-time.After(backoff):
			backoff *= 2
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
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
func (o BaselineOptions) sinkFunc(agg *aggregator) executor.SinkFunc[*Case, caseOutcome] {
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

		out := &store.Outcome{CaseID: r.Item.GetId()}

		switch {
		case r.Done():
			out.Response = r.Value.Response
			out.Score = r.Value.Score
			out.Spend = spendOf(r.Value.Response)
		default:
			out.Err = codeOf(r.Err)
			// A Case can fail AFTER a paid call — a Goal erroring on malformed
			// output, for instance. A flat one-call spend there understates
			// what was actually spent, and SettledSpend is what Guard.Restore
			// reads on resume: the resumed process would believe less was
			// spent than really was, reopening the amnesia M1-0 closed.
			if r.Value != nil && r.Value.Response != nil {
				out.Response = r.Value.Response
				out.Spend = spendOf(r.Value.Response)
			} else {
				out.Spend = budget.Spend{Calls: 1}
			}
		}

		if err := o.Store.RecordOutcome(ctx, o.RunID, out); err != nil {
			return fmt.Errorf("recording %s: %w", r.Item.GetId(), err)
		}
		return o.emit(ctx, r, agg)
	}
}

// closeRun records how the run ended and computes its aggregate.
func (o BaselineOptions) closeRun(
	ctx context.Context,
	run *knov1.Run,
	agg *aggregator,
	stats executor.Stats,
	runErr error,
) (*BaselineResult, error) {
	scored, errored := agg.counts()
	attempted := scored + errored

	// Classify before anything reads runErr. A bare context.Canceled leaving
	// this package exits 1, indistinguishable from a broken build — and a
	// scheduled run killed by a pod eviction is not a broken build. Wrapping
	// keeps the chain, so errors.Is(err, context.Canceled) still answers.
	runErr = classifyRunErr(runErr)

	run.Status = statusFor(runErr)
	run.FinishedAt = proto.String(o.now().Format(time.RFC3339))
	// DEBT(docs/debt.md#26): these do not track presence, so a stage that does
	// not execute Cases writes a zero indistinguishable from a real one.
	run.AttemptedCaseCount = int32(attempted) //nolint:gosec // bounded by the eval set
	run.ScoredCaseCount = int32(scored)       //nolint:gosec // bounded by the eval set
	run.ErroredCaseCount = int32(errored)     //nolint:gosec // bounded by the eval set

	// A run whose error rate is too high is completed but not clean. Later
	// stages must refuse to treat it as a reference rather than computing
	// deltas against a partial sample.
	if attempted > 0 {
		rate := float64(errored) / float64(attempted)
		if rate > o.maxErrorRate() {
			run.ErrorRateExceeded = true
			run.IncompleteReason = fmt.Sprintf(
				"%d of %d Cases errored (%.1f%%), above the %.1f%% threshold; "+
					"this run is not a usable baseline",
				errored, attempted, rate*100, o.maxErrorRate()*100)
		}
	}

	result := &BaselineResult{
		Run:            run,
		Stats:          stats,
		AggregateScore: agg.mean(),
		Spent:          o.Guard.Spent(),
	}

	// Recording how the run ended must survive the cancellation that ended it:
	// on Ctrl-C the caller's context is precisely the one that died, and a Run
	// left in RUNNING would look like a crash rather than an interruption.
	closeCtx := context.WithoutCancel(ctx)
	if err := o.Store.FinishRun(closeCtx, run); err != nil {
		return result, fmt.Errorf("finishing run %s: %w", o.RunID, err)
	}
	if err := o.emitRunFinished(closeCtx, agg, run); err != nil {
		return result, err
	}
	return result, runErr
}

// statusFor maps a run error to how the run ended.
//
// The distinction matters to CI: a budget stop means the run did what it was
// told and can continue, while a failure means something is broken.
// classifyRunErr gives a run-ending error the CLI grammar and an exit code
// chosen deliberately, rather than the unclassified default.
func classifyRunErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errs.ErrInterrupted):
		return err
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return errs.ErrInterrupted.Wrap(err)
	default:
		return err
	}
}

func statusFor(err error) knov1.RunStatus {
	switch {
	case err == nil:
		return knov1.RunStatus_RUN_STATUS_COMPLETED
	case errors.Is(err, errs.ErrBudgetExceeded):
		return knov1.RunStatus_RUN_STATUS_BUDGET_STOPPED
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return knov1.RunStatus_RUN_STATUS_INTERRUPTED
	default:
		return knov1.RunStatus_RUN_STATUS_FAILED
	}
}

// codeOf extracts a machine-readable code, never verbatim provider text.
func codeOf(err error) string {
	var a *errs.Actionable
	if errors.As(err, &a) {
		return a.Code
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "INTERRUPTED"
	}
	return "AGENT_ERROR"
}

func spendOf(r *knov1.Response) budget.Spend {
	return budget.Spend{
		Calls:         1,
		CostUSDMicros: r.GetCostUsdMicros(),
		Tokens:        r.GetPromptTokens() + r.GetCompletionTokens(),
	}
}

// caseOutcome is one Case's result inside the executor.
type caseOutcome struct {
	Response *Response
	Score    *Score
	Err      error
}
