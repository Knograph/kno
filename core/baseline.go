package core

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/executor"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// Package-level entry point for the Baseline stage: its options, its result,
// and the run loop that ties the other files together.

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

	// AggregateScore is the mean over SCORED Cases across the whole run,
	// including any part completed by an earlier process before a resume.
	//
	// Absent for two distinct reasons, and a caller that renders the absence
	// must say which — see AggregateUnavailable. Either nothing scored, in
	// which case there is no mean and zero would be indistinguishable from a
	// real mean of zero; or Cases scored but their numbers cannot be read
	// back, in which case a mean exists and we cannot compute it.
	AggregateScore *float64

	// AggregateUnavailable distinguishes "there is a mean and we cannot
	// compute it" from "nothing scored", when AggregateScore is nil.
	//
	// True when scored Cases can no longer contribute a number: purged before
	// scores were stored separately, or holding a Score that failed to
	// unmarshal. The counts remain accurate — those Cases really did score —
	// so a caller that reports "no cases scored" on a nil aggregate would
	// contradict the count it prints beside it.
	AggregateUnavailable bool

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
		priorScored, priorErrored, err := opts.Store.OutcomeCounts(ctx, opts.RunID)
		if err != nil {
			return nil, fmt.Errorf("loading prior outcome counts: %w", err)
		}
		// The score SUM too, not only the counts. Seeding one without the other
		// leaves the denominator spanning the whole run while the numerator
		// spans the tail — the defect this repays.
		// priorCounted, not priorScored, is priorSum's denominator: it comes
		// from the same query over the same predicate. Dividing by the count
		// from the OTHER query would reintroduce the defect one level down.
		priorSum, priorCounted, unrecoverable, err := opts.Store.ScoreSum(ctx, opts.RunID)
		if err != nil {
			return nil, fmt.Errorf("loading prior scores: %w", err)
		}
		agg.seedCounts(priorScored, priorErrored, priorSum, priorCounted, unrecoverable)
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
