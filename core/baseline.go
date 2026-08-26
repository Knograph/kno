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
	"github.com/knograph/kno/observe"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
	"go.opentelemetry.io/otel/attribute"
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

// DefaultProgressInterval is how often to emit StageProgress when progress
// reporting is on.
//
// One second, against two stated bounds rather than picked: a live view is
// useful at about 1Hz, and the heartbeat must not dominate durable writes.
// Every event is one fsync under synchronous=FULL on the same serialized
// writer as the outcome row that prevents double-spend.
//
// The overhead is 1 / (throughput x writes-per-Case), NOT a function of run
// length. At two durable writes per Case, staying under ~10% needs about five
// Cases a second — which concurrency 8 against one-second calls clears and
// concurrency 1 against a reasoning model does not. Default-off is what
// actually bounds this; M2-11 owns arguing the rate for a run that turns it on.
const DefaultProgressInterval = time.Second

// minProgressInterval is the floor on ProgressInterval.
//
// Every heartbeat is one fsync on the same single writer lane RecordOutcome
// needs, so a fast enough ticker starves the write whose loss costs money. Ten
// milliseconds is two orders of magnitude below the default and still leaves
// the store's own writes the overwhelming majority for any run whose Cases
// take longer than that — which is every run against a real provider.
const minProgressInterval = 10 * time.Millisecond

// progressWriteGrace bounds one heartbeat's write.
//
// Matched to the store's own busy_timeout: a contended SQLite write waits up
// to that long and then succeeds, so a shorter bound turns a slow write into a
// run-ending error of our own making. Measured on a CI runner at a 20ms bound.
//
// It exists at all so a hung write cannot make shutdown unbounded — stop()
// joins the goroutine, and without a deadline a wedged tick would block it
// forever.
const progressWriteGrace = 5 * time.Second

// maxConcurrency bounds Concurrency.
//
// Recorded as an int32 on the wire, and one goroutine per unit. Without it
// --concurrency 3000000000 records a width of -1294967296.
const maxConcurrency = 1024

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

	// ProgressInterval is how often a StageProgress heartbeat is emitted.
	//
	// Zero disables it, which is the default: every event is one fsync under
	// synchronous=FULL on the same serialized writer as the outcome row that
	// prevents double-spend, so a heartbeat nobody watches is pure write
	// contention. The CLI turns it on when there is something rendering.
	//
	// DefaultProgressInterval is the figure to pass.
	ProgressInterval time.Duration

	// concurrency records what checkFeasible decided, for the event stream and
	// the Run record.
	//
	// Unexported: it is an OUTPUT of the feasibility check, not an input a
	// caller supplies. A caller setting it would be describing a decision that
	// had not been made yet.
	concurrency *knov1.ConcurrencyDecision

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

	// NOT SET BY ANYTHING. No caller populates this, so checkResumable's model
	// comparison is inert — see docs/debt.md#42. Filling it needs the check
	// moved to first-response time, because a resolved model is a property of
	// a response and the check runs before any call is made.
	//
	// (BaselineOptions.ResolvedModel was here. It was caller-supplied and read
	// at openRun, BEFORE any request, so the only value it could ever hold was
	// one a previous run had recorded — checkResumable compared it to itself
	// and never fired. The gate moved to first-response time; see modelGate in
	// baseline_gate.go and docs/debt.md#42. Removing an exported field is a
	// public-Go-API break, permitted pre-1.0 with a CHANGELOG migration note.)

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

	// AcceptUnknownCost lets a run proceed when no per-Case cost can be
	// computed.
	//
	// Explicit rather than a prompt. A confirmation that cannot state a dollar
	// figure — "10,000 Cases, per-Case cost unknown" — gives a human no basis
	// to decide and is a dialog people click through; a flag someone had to
	// type is consent, and it is greppable in a CI config, in shell history,
	// and in a code review.
	AcceptUnknownCost bool

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

// retryBudget is the configured wall-clock bound, or the default.
func (o BaselineOptions) retryBudget() time.Duration {
	if o.RetryBudget > 0 {
		return o.RetryBudget
	}
	return DefaultRetryBudget
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
) (result *BaselineResult, err error) {
	if err := opts.validate(evals); err != nil {
		return nil, err
	}

	// The span every other span in this run hangs from. A no-op unless the
	// caller installed a TracerProvider, so this costs nothing on the default
	// path — which is why it is unconditional rather than behind a flag.
	ctx, runSpan := observe.StartRun(ctx, opts.RunID)
	// Named return plus one deferred close, rather than marking the span at
	// the single place the run finishes normally. There are fourteen early
	// returns between here and there — every store failure, a stale
	// checkpoint, an unreadable eval source, and BOTH budget refusals — and
	// each of them ended the span with status Unset, which a collector renders
	// identically to a clean run. A refused run is the one run-level event a
	// trace has to show.
	defer func() {
		if err != nil {
			observe.Fail(runSpan, codeOf(err))
		}
		runSpan.End()
	}()

	// Resume state first: both guards below need it. checkFeasible must see the
	// headroom a resume actually has, and confirmRun must quote only the Cases
	// that are left.
	done := map[string]struct{}{}
	// Hoisted: emitRunResumed reports it, and it is declared inside the block
	// that loads it.
	var restored budget.Spend
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
		restored, err = opts.Store.SettledSpend(ctx, opts.RunID)
		if err != nil {
			return nil, fmt.Errorf("loading prior spend: %w", err)
		}
		// The guard is in-memory. Without this a resumed run believes it has
		// spent nothing and can consume its cap a second time.
		opts.Guard.Restore(restored)
	}

	// Both refusals happen BEFORE the Run record exists. Refusing after
	// openRun left a row permanently in RUNNING with no outcomes and no
	// events — and since the interactive path declines by default, every
	// above-threshold `kno baseline` minted a fresh orphan. A CI gate reading
	// exit 2 as "not a failure" then reported green for a run that never
	// started.
	// Before checkFeasible and before confirmRun, because it answers a
	// question both of them assume: can this run state what it will cost?
	// confirmRun's arithmetic collapses to zero when it cannot, and a zero
	// quote is indistinguishable from a cheap one — so the run proceeded
	// silently on exactly the configuration we know least about.
	if err := opts.checkCostIsKnowable(); err != nil {
		return nil, err
	}
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
	// Hoisted: the opening event gates on whether the stream already has one,
	// not on whether the user passed --resume.
	var maxSeq int64
	if opts.Resume {
		// Continue numbering rather than restarting at 1, which would collide
		// with events from before the interruption and silently defeat the
		// gap detection Event.sequence exists for.
		maxSeq, err = opts.Store.MaxEventSequence(ctx, opts.RunID)
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
		prior, err := opts.Store.ScoreSum(ctx, opts.RunID)
		if err != nil {
			return nil, fmt.Errorf("loading prior scores: %w", err)
		}
		// Unrecoverable() rather than either count alone: Baseline suppresses
		// the aggregate whenever a score is missing, and it is missing either
		// way. The split exists so the REASON can be reported (docs/debt.md#31)
		// — a purge the user performed reads differently from a row an older
		// binary wrote — and the reporting of it lands with the reader that
		// renders it, not here.
		agg.seedCounts(priorScored, priorErrored, prior.Sum, prior.Counted, prior.Unrecoverable())
	}
	// RunResumed continues a stream; RunStarted opens one. The predicate is
	// therefore the STREAM's state, not the user's flag.
	//
	// Gating on opts.Resume loses the run's identity entirely when the first
	// process died before emitting anything — reading the evals can fail
	// without changing the content hash, so that resume is accepted. The
	// stream would then contain no RunStarted at all, and RunResumed carries
	// no stage, agent, goal, or goal direction: a consumer could never learn
	// which way "better" points. Before this change the resumed process
	// re-emitted RunStarted with a stale total, which was the bug being fixed
	// but did at least supply those fields.
	//
	// The old shape's actual defect — a SECOND RunStarted carrying the
	// ORIGINAL total — is what maxSeq > 0 rules out. docs/debt.md#29.
	if err := opts.emitOpening(ctx, agg, maxSeq, len(done), restored); err != nil {
		return nil, err
	}
	// After the opening event: a consumer should have the run's identity
	// before its caveats.
	if err := opts.emitConcurrencyReduced(ctx, agg); err != nil {
		return nil, err
	}
	// Stopped and JOINED before closeRun, so RunFinished is last by
	// construction rather than by appendEvent refusing a late append. A
	// refusal is an error nobody reads.
	stopProgress := opts.progressTicker(ctx, agg, opts.DevCases, opts.now())
	// A panic guard. There is no early return between here and the explicit
	// stop below, so this catches only a panic in executor.Run; the explicit
	// call is what orders RunFinished last and surfaces a write failure.
	defer func() { _ = stopProgress() }()
	// stopReason is set the moment a fatal error starts the shutdown, and read
	// by the sink to tell "this Case was cancelled BY the stop" from "this Case
	// timed out on its own" — and, now, WHICH stop.
	//
	// The run's own context is not that signal: a budget stop cancels the
	// executor's internal context, never the caller's, so runCtx.Err() stays
	// nil throughout — which is exactly the wrong answer for the case this
	// exists to catch.
	//
	// It carries the reason rather than a bool because there are three stops
	// and they need different words. A run stopped by a human is not a run that
	// ran out of money (docs/debt.md#52), and a run stopped by a rejected
	// credential is neither: telling that user the cost cap could not admit
	// another attempt sends them to raise a cap that was never the problem.
	// UNSPECIFIED means "not draining", which is why the zero value is right.
	var stopReason atomic.Int32

	// Armed from the RESUMED run's record; inert on a fresh run.
	gate := newModelGate(run)

	stats, runErr := executor.Run(ctx, cases,
		opts.workFunc(agg), opts.sinkFunc(ctx, &stopReason, agg),
		executor.Options{
			// The only path from a SUCCESSFUL Case to shutdown. IsFatal is
			// consulted on work errors only, and a re-pointed model alias is
			// visible in an answer we already paid for and want to keep.
			AfterRecord: gate.afterRecord,
			Concurrency: opts.Concurrency,
			ID:          func(item any) string { c, _ := item.(*Case); return c.GetId() },
			Skip:        func(id string) bool { _, ok := done[id]; return ok },
			// Budget exhaustion ends the run rather than failing every
			// remaining Case one at a time.
			//
			// So does a condition that cannot change WITHIN the run: a
			// rejected credential, the provider's own spend cap, an unpaid
			// account, a model that does not exist, a refused destination, a
			// model with no price row under a dollar cap. Each was
			// classified per-Case, so a wrong ANTHROPIC_API_KEY on a
			// 10,000-Case run made 10,000 requests and settled 10,000 calls
			// against --max-calls before telling the user anything — which is
			// precisely what anthropic.ErrAuthentication's own godoc claims it
			// prevents. See docs/debt.md#47.
			//
			// The adapter classifies and core escalates. The adapter cannot do
			// the escalation (it does not own the run) and core cannot do the
			// classification (it never saw the status code), which is why this
			// reads a structural assertion rather than a sentinel.
			//
			// Deliberately NOT keyed on errUnpriceable, which would have been
			// the easy way to repay docs/debt.md#46 and is wrong. That sentinel
			// covers three causes and only some are run-invariant: a model with
			// no price row cannot change mid-run, while an Estimator that
			// refuses one Case and prices the rest is a per-Case problem, and
			// TestEstimatorFailureRefusesWhenACostCapIsSet exists to keep the
			// second from being aborted — "no money can be spent on a Case that
			// was never authorized, and the rest are priced correctly". So the
			// ADAPTER marks its model-level pricing failure and this reads the
			// mark, rather than core guessing from a sentinel that cannot tell
			// the two apart.
			IsFatal: func(err error) bool {
				if errors.Is(err, errs.ErrBudgetExceeded) {
					// CompareAndSwap, not Store: executor.fail keeps the FIRST
					// error, so a reason that overwrote would describe a
					// different stop than the one being reported. A resume near
					// its cap with a bad key hits both.
					stopReason.CompareAndSwap(0,
						int32(knov1.OrphanReason_ORPHAN_REASON_BUDGET_EXCEEDED))
					return true
				}
				if runFatalOf(err) {
					stopReason.CompareAndSwap(0,
						int32(knov1.OrphanReason_ORPHAN_REASON_RUN_FATAL))
					return true
				}
				return false
			},
		})

	// Before closeRun, not deferred after it: a deferred stop runs once
	// closeRun has already returned, leaving the ticker free to take a
	// sequence number during it. RunFinished is documented as the last event.
	// Before closeRun, not deferred after it: a deferred stop runs once
	// closeRun has already returned, leaving the ticker free to take a
	// sequence number during it. RunFinished is documented as the last event.
	//
	// Its error is a run-ending one. A failed heartbeat append burns a
	// sequence number that is never written, and a gap below the maximum
	// survives every resume — so a stream with a silent hole is worse than a
	// run that stops and says why.
	if err := stopProgress(); err != nil && runErr == nil {
		runErr = err
	}
	// A hot-path event-write failure ends the run, but only here — recorded
	// during the run rather than returned, so it could not destroy the paid
	// work it was reporting on.
	if err := agg.emitFailed(); err != nil && runErr == nil {
		runErr = err
	}
	res, closeErr := opts.closeRun(ctx, run, agg, stats, runErr)

	// Recorded on the run span before it ends, so a trace answers "what did
	// this run cost and how much of it worked" without joining to the store.
	// Counts and money only; the numbers, never the answers.
	if res != nil {
		runSpan.SetAttributes(
			observe.CostUSDMicros(res.Spent.CostUSDMicros),
			attribute.Int("kno.cases.scored", int(res.Run.GetScoredCaseCount())),
			attribute.Int("kno.cases.errored", int(res.Run.GetErroredCaseCount())),
		)
	}
	// The deferred close marks the failure; assigning the named return is what
	// tells it to.
	return res, closeErr
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
	case o.Guard.Limits().MaxCostUSDMicros > 0 && o.EstCostPerCallUSDMicros <= 0 &&
		!o.agentCanPriceItself():
		// The guard cannot refuse what it was not told about. A dollar cap
		// with a zero estimate is only discovered at settlement, after the
		// money is spent — which already caused a real overshoot once.
		// User-reachable, unlike the nil-field cases above, so it carries the
		// grammar: what failed, why, and the flag to pass.
		//
		// Not required of an Agent that prices ITSELF. This rule refused
		// `--agent anthropic:claude-opus-5 --max-cost-usd 5` even though the
		// adapter prices every Case exactly — and the scalar the user was
		// forced to supply is then IGNORED, because estimate() consults the
		// Estimator and never falls back to it. So the flag was mandatory,
		// inert, and the only way to run the flagship invocation.
		return errs.ErrInvalidInput.WithFix(
			"pass --cost-per-call-usd alongside --max-cost-usd, or use an agent " +
				"that can price its own calls",
		).
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
			"add Cases, or lower the holdout fraction",
		).
			Wrap(errors.New("no Cases landed in dev, leaving nothing to measure"))
	case o.Concurrency < 0:
		return errs.ErrInvalidInput.WithFix("pass --concurrency 0 for the default, or a positive number").
			Wrap(fmt.Errorf("concurrency %d is negative", o.Concurrency))
	case o.Concurrency > maxConcurrency:
		// Bounded because it is recorded as an int32 and because the executor
		// spawns a goroutine per unit. --concurrency 3000000000 recorded
		// Requested: -1294967296, a negative width on the wire.
		return errs.ErrInvalidInput.WithFix(fmt.Sprintf("pass --concurrency at or below %d", maxConcurrency)).
			Wrap(fmt.Errorf("concurrency %d is beyond what one process can run", o.Concurrency))
	case o.ProgressInterval < 0:
		return errs.ErrInvalidInput.WithFix("pass a positive interval, or zero to disable progress").
			Wrap(fmt.Errorf("progress interval %s is negative", o.ProgressInterval))
	case o.ProgressInterval > 0 && o.ProgressInterval < minProgressInterval:
		// A floor, because every heartbeat is one fsync on the single writer
		// lane RecordOutcome needs. At 1ms a 12-Case run emitted 48
		// heartbeats — four durable writes per Case, which is the ratio the
		// heartbeat is off by default to avoid.
		return errs.ErrInvalidInput.WithFix(fmt.Sprintf("pass a progress interval of at least %s", minProgressInterval)).
			Wrap(fmt.Errorf("progress interval %s would put more durable writes on the store than the run itself", o.ProgressInterval))
	case o.HoldoutCases <= 0:
		// The refusal DESIGN.md advertises as an engine property. A run that
		// can never produce a holdout number is not a run: every later stage
		// would compute against a reference with no honest confirmation.
		return errs.ErrInvalidInput.WithFix(
			"add Cases, or raise the holdout fraction",
		).
			Wrap(errors.New("no Cases landed in the holdout, so this run can never be validated"))
	}
	return nil
}
