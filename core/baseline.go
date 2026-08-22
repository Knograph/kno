package core

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
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

// DefaultRetryBudget bounds retry by TIME as well as by attempts.
//
// Attempts alone was the wrong bound. Three attempts at 500ms doubling gives a
// total window of 1.5 seconds, and a real provider's sustained 429 window is
// minutes — so a rate-limited account marked a perfectly good baseline
// ErrorRateExceeded and told the user "too many cases errored for this to be a
// usable baseline", naming nothing about the cause.
//
// Time alone is also wrong, and for the opposite reason: each attempt takes its
// own reservation and settles its own call, so a long window lets one Case
// consume dozens of calls against --max-calls. Both bounds apply, whichever
// binds first.
const DefaultRetryBudget = 90 * time.Second

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

	// ResolvedModel is what the provider reported actually answering, once one
	// has. Empty until the adapter supplies it.
	//
	// Compared on resume: a ref like openai:gpt-4.1 is a moving pointer, and a
	// run resumed after the alias re-points would otherwise blend two models
	// into one AggregateScore.
	ResolvedModel string

	// HoldoutUnderpowered marks a holdout too small for a meaningful interval.
	HoldoutUnderpowered bool

	// MaxErrorRate overrides DefaultMaxErrorRate. Zero uses the default.
	MaxErrorRate float64

	// MaxAttempts bounds how many times one Case is tried. Zero means
	// DefaultMaxAttempts; 1 disables retry.
	MaxAttempts int

	// RetryBudget bounds the total WALL-CLOCK time spent retrying one Case.
	// Zero means DefaultRetryBudget.
	//
	// Applies alongside MaxAttempts rather than replacing it. A provider's
	// Retry-After can ask for a minute; three attempts of that is three
	// minutes on one Case, and a run of a thousand Cases cannot afford it.
	RetryBudget time.Duration

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

	// Resume state first: both guards below need it. checkFeasible must see the
	// headroom a resume actually has, and confirmRun must quote only the Cases
	// that are left.
	done := map[string]struct{}{}
	if opts.Resume {
		probe, err := opts.Store.GetRun(ctx, opts.RunID)
		if err != nil {
			return nil, fmt.Errorf("loading run %s: %w", opts.RunID, err)
		}
		if err := opts.checkResumable(probe); err != nil {
			return nil, err
		}
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

	// Both refusals happen BEFORE the Run record exists. Refusing after
	// openRun left a row permanently in RUNNING with no outcomes and no
	// events — and since the interactive path declines by default, every
	// above-threshold `kno baseline` minted a fresh orphan. A CI gate reading
	// exit 2 as "not a failure" then reported green for a run that never
	// started.
	if err := (&opts).checkFeasible(len(done)); err != nil {
		return nil, err
	}
	if err := opts.confirmRun(ctx, len(done)); err != nil {
		return nil, err
	}

	run, err := opts.openRun(ctx)
	if err != nil {
		return nil, err
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
		// Both the counts and the score sum, so the aggregate spans the whole
		// run rather than the tail of it.
		priorScored, priorErrored, err := opts.Store.OutcomeCounts(ctx, opts.RunID)
		if err != nil {
			return nil, fmt.Errorf("loading prior outcome counts: %w", err)
		}
		// The score SUM too, not only the counts. Seeding one without the other
		// left the denominator spanning the whole run while the numerator
		// spanned the tail.
		priorSum, _, unrecoverable, err := opts.Store.ScoreSum(ctx, opts.RunID)
		if err != nil {
			return nil, fmt.Errorf("loading prior scores: %w", err)
		}
		agg.seedCounts(priorScored, priorErrored, priorSum, unrecoverable)
	}
	if err := opts.emitRunStarted(ctx, agg, opts.DevCases); err != nil {
		return nil, err
	}
	// draining is set the moment a fatal error starts the shutdown, and read by
	// the sink to tell "this Case was cancelled BY the stop" from "this Case
	// timed out on its own".
	//
	// The run's own context is not that signal: a budget stop cancels the
	// executor's internal context, never the caller's, so runCtx.Err() stays
	// nil throughout — which is exactly the wrong answer for the case this
	// exists to catch.
	var draining atomic.Bool

	stats, runErr := executor.Run(ctx, cases,
		opts.workFunc(), opts.sinkFunc(ctx, &draining, agg),
		executor.Options{
			Concurrency: opts.Concurrency,
			ID:          func(item any) string { c, _ := item.(*Case); return c.GetId() },
			Skip:        func(id string) bool { _, ok := done[id]; return ok },
			// Budget exhaustion ends the run rather than failing every
			// remaining Case one at a time.
			IsFatal: func(err error) bool {
				if errors.Is(err, errs.ErrBudgetExceeded) {
					draining.Store(true)
					return true
				}
				return false
			},
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
	case resolvedModelChanged(run, o.ResolvedModel):
		// A ref like openai:gpt-4.1 is a moving pointer. A run interrupted on
		// Monday and resumed on Friday after the alias re-points passes every
		// check above and blends two models into one AggregateScore.
		//
		// Only the RESOLVED model. The provider's build identifier is recorded
		// and reported but never refused on: it changes whenever the backend
		// config changes, routinely and with no model change, and refusing on
		// it would cost the user a full re-run for a false positive — worse
		// than the blending it prevents.
		changed = fmt.Sprintf("a different resolved model (recorded %q, now %q)",
			firstResolvedModel(run), o.ResolvedModel)
	default:
		return nil
	}
	return errs.ErrCheckpointStale.WithFix(o.staleFix(run)).
		Wrap(fmt.Errorf("run %s was recorded against %s", o.RunID, changed))
}

// resolvedModelChanged reports whether the provider is now answering with a
// different model than the one this run recorded.
//
// Empty on either side means unknown — the first process may not have reached a
// response, or this one has not yet. Unknown is not a mismatch: refusing on
// absence would make every run that stopped before its first answer
// unresumable.
func resolvedModelChanged(run *knov1.Run, now string) bool {
	recorded := firstResolvedModel(run)
	return recorded != "" && now != "" && recorded != now
}

func firstResolvedModel(run *knov1.Run) string {
	models := run.GetCaseExecution().GetResolvedModels()
	if len(models) == 0 {
		return ""
	}
	return models[0]
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

// confirmRun asks the human about the whole run before any of it is authorized.
//
// Computed HERE rather than in the CLI, and after CompletedCases, because this
// is the only place that knows how many Cases are actually left. The CLI has
// DevCases but not the completed set, so a run killed at 9,988 of 10,000 would
// have prompted for the full amount to finish twelve Cases.
//
// Bounded by the cap. With --max-cost-usd 5.00 the honest maximum exposure is
// $5.00, not DevCases x per-Case: showing the larger number when the guard will
// stop at the smaller one is false in the direction that teaches people to
// dismiss the prompt.
func (o BaselineOptions) confirmRun(ctx context.Context, alreadyDone int) error {
	remaining := int64(o.DevCases - alreadyDone)
	perCall := o.planningCostPerCall()
	if remaining <= 0 || perCall <= 0 {
		return nil
	}

	total := budget.Estimate{
		Calls:         remaining,
		CostUSDMicros: saturatingMul(remaining, perCall),
	}
	// Never quote more than the cap can permit.
	if ceiling := o.Guard.Limits().MaxCostUSDMicros; ceiling > 0 && total.CostUSDMicros > ceiling {
		total.CostUSDMicros = ceiling
	}

	ok, err := o.Guard.PreConfirm(ctx, total)
	if err != nil {
		return err
	}
	if !ok {
		return errs.ErrBudgetExceeded.WithFix(
			"re-run with --yes to proceed, or lower --max-cost-usd").
			Wrap(fmt.Errorf("the run was not confirmed; nothing was spent"))
	}
	return nil
}

// planningCostPerCall is what one Case may cost, for planning rather than for
// authorizing.
//
// An Estimator prices each Case from the Case, so a scalar supplied by the
// caller is not what the guard will reserve. Both the feasibility check and the
// consent prompt have to plan against the number that will actually be
// reserved, or they answer a different question than the one being asked — and
// the consent prompt in particular then shows a human a figure that is wrong by
// whatever the two differ by.
func (o BaselineOptions) planningCostPerCall() int64 {
	if e, ok := o.Agent.(Estimator); ok {
		if w := e.WorstCase(); w.CostUSDMicros > 0 {
			return w.CostUSDMicros
		}
	}
	return o.EstCostPerCallUSDMicros
}

// saturatingMul multiplies without wrapping.
//
// An overflowed product goes negative, sails past the cap clamp and under the
// confirmation threshold, so PreConfirm silently returns true and the run falls
// back to the per-call prompt — a consent path skipped by arithmetic.
func saturatingMul(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	return a * b
}

// feasibleHeadroomFraction bounds how much of a cost cap may be held as
// un-spendable reservation at once.
//
// A pessimistic reservation holds concurrency x estimate of the cap in flight.
// If that exceeds the cap, the guard denies before anything settles and the run
// stops having done almost nothing: measured at a 32k output ceiling against
// --max-cost-usd 1.00 at concurrency 8, the FOURTH Case was denied with $0.00
// spent.
//
// A quarter, so a run never holds more than that back. Chosen against the
// measured forfeiture — 4.7% of a $5 cap at concurrency 8, 19.5% at 32 — and
// written down as a decision rather than left as a constant to reverse-engineer.
const feasibleHeadroomFraction = 0.25

// checkFeasible reduces concurrency, or refuses, when the caps cannot admit the
// run as configured.
//
// It runs AFTER Guard.Restore, against the headroom a resumed run actually has
// rather than against the cap it was originally given. Placing it before would
// be the same defect this file already fixed for the confirmation prompt:
// computing against a number the run is not operating under.
//
// A refusal exits 2, not 1. An exhausted cap is a resumable stop with nothing
// wrong with the data, and reporting it as a broken build is what trains people
// to ignore exit 1.
func (o *BaselineOptions) checkFeasible(alreadyDone int) error {
	// A resume of a finished run has nothing left to authorize, so an exhausted
	// cap is not a problem to report — it is the expected state. Refusing here
	// turned an idempotent `--resume` in CI from a clean exit 0 into exit 2,
	// with a fix line telling the user to re-run and pay for all of it again.
	if o.DevCases > 0 && o.DevCases-alreadyDone <= 0 {
		return nil
	}

	limits := o.Guard.Limits()
	perCall := o.planningCostPerCall()
	if limits.MaxCostUSDMicros <= 0 || perCall <= 0 {
		return nil // no dollar cap, or nothing to compute against
	}

	remaining := o.Guard.Remaining().CostUSDMicros
	if remaining <= 0 {
		return errs.ErrBudgetExceeded.WithFix(
			"raise --max-cost-usd, or start a fresh run without --resume").
			Wrap(fmt.Errorf("the cost cap is already spent, so no Case can be authorized"))
	}

	// Refusing outright only makes sense when every Case costs the same, which
	// is true of the scalar and false of an Estimator: it prices each Case from
	// the Case, so the worst one exceeding the cap says nothing about whether
	// the cheap ones fit. Refusing there would reject a run where most Cases
	// are affordable, and the per-Case guard already stops the dear ones.
	if _, perCase := o.Agent.(Estimator); !perCase && perCall > remaining {
		return errs.ErrBudgetExceeded.WithFix(fmt.Sprintf(
			"raise --max-cost-usd above %s", formatUSDMicros(perCall))).
			Wrap(fmt.Errorf("one Case is estimated at %s and only %s remains, so "+
				"not a single Case can be authorized",
				formatUSDMicros(perCall), formatUSDMicros(remaining)))
	}

	// How many in-flight reservations the headroom admits.
	affordable := int(float64(remaining) * feasibleHeadroomFraction / float64(perCall))
	if affordable < 1 {
		affordable = 1
	}
	// Zero is not "no concurrency", it is the CLI's default and the executor
	// turns it into min(NumCPU, 8) — the exact figure in the measurement this
	// check exists for. Treating it as a bypass meant the guard still denied
	// its way to a halt on the path almost every user takes: measured 0 of 60
	// Cases scored, $0.00 spent, on a $1.00 cap.
	requested := o.Concurrency
	if requested <= 0 {
		requested = executor.DefaultConcurrency()
	}
	if requested <= affordable {
		return nil
	}

	o.Concurrency = affordable
	return nil
}

// formatUSDMicros renders micro-USD for a human.
func formatUSDMicros(micros int64) string {
	return fmt.Sprintf("$%.4f", float64(micros)/1_000_000)
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

// invokeWithRetry calls the Agent, backing off when the provider rate limits.
//
// Each attempt takes its OWN reservation and settles it. A retry is a new
// provider call: it consumes call budget whether or not it produces an answer,
// and pretending otherwise would let a throttled run spend past its cap
// invisibly.
//
// Which errors are retried is decided by retryable, below. A 429 is the
// provider asking us to slow down, not the Case failing — recording it as
// terminal throws away capacity already paid for, and ordinary throttling would
// push a run past max_error_rate and mark a perfectly good baseline unusable
// for no statistical reason. A dropped connection is the same shape: the
// request never reached a handler.
//
// An agent that answered badly — malformed output a Goal cannot score — did
// fail, and retrying it burns budget on a Case that will fail again.

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

	// A purged run cannot produce an aggregate, and saying so is the point.
	// Reporting the partial mean would present a number nobody can reproduce
	// beside counts that describe a larger population.
	if agg.aggregateUnavailable() {
		run.IncompleteReason = "some Cases were purged before their score was " +
			"stored separately, so this run has no reportable aggregate; the " +
			"Cases themselves are intact and resume normally"
	}

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

func spendOf(r *knov1.Response) budget.Spend { return spendOfN(r, 1) }

// attemptsOf reports how many provider calls an outcome took, floored at one.
//
// A nil outcome means the work never produced one, which is still one call's
// worth of exposure at most.
func attemptsOf(o *caseOutcome) int64 {
	if o == nil || o.Attempts < 1 {
		return 1
	}
	return int64(o.Attempts)
}

// spendOfN is spendOf for a Case that took n provider calls.
func spendOfN(r *knov1.Response, n int64) budget.Spend {
	if n < 1 {
		n = 1
	}
	return budget.Spend{
		Calls:         n,
		CostUSDMicros: r.GetCostUsdMicros(),
		Tokens:        r.GetPromptTokens() + r.GetCompletionTokens(),
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
