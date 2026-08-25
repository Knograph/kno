package core

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/executor"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
	"google.golang.org/protobuf/proto"
)

// Recording what one Case did: persisting its outcome, classifying its error,
// counting it, and emitting its event.
//
// The sink and the emitter live together because they share an invariant that
// nothing else enforces. sinkFunc skips persisting a Case on three predicates
// and emit skips on one, and they agree only because sinkFunc is emit's sole
// caller and returns first in the other two. Adding a fourth skip to one and
// not the other makes the outcomes table and the reported counts disagree —
// the failure workFunc's own comment records as having cost real money to
// find. Separated across files, that pair is invisible.

// aggregator accumulates the run's score over SCORED Cases only.
//
// Errored Cases are excluded rather than counted as zero. An agent that
// returned a 500 did not answer badly, it did not answer — scoring
// infrastructure failure as task failure biases the baseline downward and makes
// every later Asset look better than it is. The counts are reported separately
// so the exclusion is visible rather than implied.
type aggregator struct {
	mu      sync.Mutex
	sum     float64
	scored  int
	errored int
	seq     int64

	// Counts carried over from an interrupted run, so the totals describe the
	// whole run rather than only the resumed portion.
	priorScored  int
	priorErrored int

	// priorSum is the score total from Cases earlier processes recorded.
	//
	// Seeded alongside the counts. Without it the counts spanned the whole run
	// while the mean spanned only the tail, so a run interrupted after 24 Cases
	// and resumed for 36 reported "60 scored" beside a mean of the last 36 —
	// a denominator and a numerator describing different populations.
	priorSum float64

	// priorCounted is how many prior Cases actually contributed to priorSum.
	//
	// NOT priorScored. The two come from different queries over different
	// predicates: priorScored counts rows marked scored, priorCounted counts
	// rows holding a number. They agree only while every scored row has a
	// score, which nothing in the schema enforces. Dividing priorSum by
	// priorScored would put a numerator and a denominator from two separate
	// reads into one number — the defect this whole change repays, one level
	// down. The store returns both from a single query for that reason.
	priorCounted int

	// emitMu serializes allocate-then-write so the sequence order and the
	// INSERTION order are the same.
	//
	// next() alone is not enough: two goroutines can take 5 and 6 and commit
	// them in the other order. A consumer reading by sequence is fine, but the
	// API streams in insertion order, and it would see 6 then 5 and report a
	// gap — the false positive Event.sequence exists to prevent.
	//
	// Unnecessary today, because every emitter is serialized: the opening
	// events run before the executor, the sink is documented as one goroutine,
	// and closeRun runs after it drains. M2-10c adds a ticker, which is the
	// first emitter that is not.
	emitMu sync.Mutex

	// emitFailure holds the first hot-path event-write failure.
	//
	// Out of band, because an observability failure must never change an
	// attempt's RESULT. Returning it from the worker discarded a paid,
	// scoreable answer and recorded the Case as an agent error — and since the
	// outcome row was still written, a resume skipped it. We would have paid
	// for an answer, thrown it away, and blamed the agent.
	//
	// docs/debt.md#32 already rejected this mechanism for Settle: "making
	// Settle fail would turn a successful, paid, scored call into an errored
	// Case and lose paid work." The emitter is the same hazard by another
	// route. Surfaced at close instead, where it ends the run without
	// destroying what the run bought.
	emitFailure atomic.Pointer[error]

	// closed is set when RunFinished is written.
	//
	// The payload promises it is "always the last event" and nothing enforced
	// it. A ticker that has not been stopped can otherwise append after close,
	// which is a contract break a consumer cannot detect.
	closed bool

	// unrecoverable counts Cases that scored but whose number is gone —
	// purged before the score lived in its own column, or holding a Score blob
	// that could not be read back.
	//
	// They cannot be averaged in as zero: that drags the mean toward zero and
	// presents the result as the run's actual aggregate, which is worse than
	// reporting nothing. The mean refuses instead.
	unrecoverable int
}

// snapshot reads every reported number under one lock.
//
// Taken together rather than through separate accessors: the counts, the mean
// and the refusal are rendered side by side, and reading them at three
// instants lets a live consumer print a mean over a denominator it never had.
// Today closeRun runs after the executor has drained, so nothing tears — this
// keeps that true when a progress view reads the aggregator mid-run.
func (a *aggregator) snapshot() (scored, errored int, mean *float64, unavailable bool, lost int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.scored + a.priorScored,
		a.errored + a.priorErrored,
		a.meanLocked(),
		a.unrecoverable > 0,
		a.unrecoverable
}

func (a *aggregator) add(value float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sum += value
	a.scored++
}

func (a *aggregator) addError() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.errored++
}

// sessionCounts reports what THIS process did, excluding a resumed run's
// prior work.
//
// Distinct from counts, which spans the whole run. A throughput figure needs
// this one: a resume carrying 900 completed Cases into a process that has run
// for one second is not doing 900 Cases a second.
func (a *aggregator) sessionCounts() (scored, errored int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.scored, a.errored
}

func (a *aggregator) counts() (scored, errored int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.scored + a.priorScored, a.errored + a.priorErrored
}

// mean reports the aggregate over every scored Case in the run, or nil.
//
// Nil for three different reasons, and conflating them would be dishonest in
// different directions. Nothing scored: there is no mean. Something scored but
// its number is unrecoverable: there is a mean and we cannot compute it, so
// reporting the partial one would present a number nobody can reproduce. A
// score arrived as NaN: every later arithmetic result is NaN too, and a
// resumed run would carry it forward through the stored sum forever.
//
// Callers that also report the counts should use snapshot instead, so the two
// come from one read.
func (a *aggregator) mean() *float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.meanLocked()
}

func (a *aggregator) meanLocked() *float64 {
	if a.unrecoverable > 0 {
		return nil
	}
	total := a.scored + a.priorCounted
	if total == 0 {
		return nil
	}
	m := (a.sum + a.priorSum) / float64(total)
	if math.IsNaN(m) || math.IsInf(m, 0) {
		return nil
	}
	return &m
}

// recordEmitFailure keeps the first hot-path event-write failure.
func (a *aggregator) recordEmitFailure(err error) {
	if err != nil {
		a.emitFailure.CompareAndSwap(nil, &err)
	}
}

// emitFailed reports the first hot-path event-write failure, if any.
func (a *aggregator) emitFailed() error {
	if p := a.emitFailure.Load(); p != nil {
		return *p
	}
	return nil
}

// isClosed reports whether RunFinished has been written.
func (a *aggregator) isClosed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closed
}

// markClosed records that RunFinished was written, so any later append fails
// rather than silently violating the schema's "always the last event".
func (a *aggregator) markClosed() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
}

// next returns the next event sequence number.
func (a *aggregator) next() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	return a.seq
}

// seedCounts carries an interrupted run's totals into this process.
//
// sum and unrecoverable come from the store's score column, which is why
// docs/debt.md#25 requires a purge to null the trace blobs and never the row:
// the number survives a purge precisely so this can read it.
func (a *aggregator) seedCounts(scored, errored int, sum float64, counted, unrecoverable int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.priorScored = scored
	a.priorErrored = errored
	a.priorSum = sum
	a.priorCounted = counted
	a.unrecoverable = unrecoverable
}

// seedSequence continues numbering after a resume rather than restarting at 1,
// which would collide with events from before the interruption.
func (a *aggregator) seedSequence(from int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq = from
}

// emit records one event for a Case's outcome.
//
// Events carry identifiers and metrics, never content. The Case's input and the
// agent's output stay in the store, which is the only package that handles
// trace content.
// appendEvent writes one event, allocating its sequence number LAST.
//
// The order is the whole point. Event.sequence exists so a consumer that sees
// a gap knows it lost events rather than silently under-reporting, and a
// number allocated before a path that returns without writing burns it.
//
// How bad a burn is depends on whether anything else has taken a number since.
// Today nothing can — every emitter is serialized, so a burned number is
// always the current maximum, MaxEventSequence returns the highest WRITTEN
// sequence, and a resume reissues it. The gap heals. Once a concurrent emitter
// exists (M2-10c's ticker) that stops being true, and the gap is then
// permanent: MaxEventSequence cannot heal a hole below its own maximum.
//
// emit had exactly this shape — a sequence at construction, then an early
// return on a budget refusal — and was safe only because sinkFunc returns
// first on the same predicate, which is a coincidence of having one caller.
// Every emitter goes through here so the rule holds when that stops.
// The caller supplies an Event carrying only its Payload; the run ID, the
// timestamp, and the sequence are filled here.
//
// A payload-carrying Event rather than the oneof interface because protoc
// generates that interface unexported, so core cannot name it.
func (o BaselineOptions) appendEvent(
	ctx context.Context,
	agg *aggregator,
	ev *knov1.Event,
	what string,
) error {
	return o.appendEventFunc(ctx, agg, func() *knov1.Event { return ev }, what)
}

// appendEventFunc is appendEvent for a payload whose CONTENTS must be read
// under the same lock that orders the write.
//
// The progress heartbeat needs it. Reading the counts before taking emitMu
// lets the sink write several CaseScored events at lower sequences while the
// heartbeat waits, so a consumer replaying in order sees ten Cases scored and
// then a heartbeat claiming five — progress going backwards against events
// already delivered.
func (o BaselineOptions) appendEventFunc(
	ctx context.Context,
	agg *aggregator,
	build func() *knov1.Event,
	what string,
) error {
	// Held across ALL of it, so a concurrent emitter cannot interleave a lower
	// sequence behind a higher one, and cannot change the counts between the
	// read and the write.
	agg.emitMu.Lock()
	defer agg.emitMu.Unlock()

	ev := build()
	ev.RunId = o.RunID
	ev.EmittedAt = o.now().Format(time.RFC3339)

	if agg.isClosed() {
		return fmt.Errorf("appending %s event: the run already emitted RunFinished, "+
			"which the schema promises is the last event", what)
	}
	// Last, immediately before the write.
	ev.Sequence = agg.next()
	if err := o.Store.AppendEvent(ctx, ev); err != nil {
		return fmt.Errorf("appending %s event: %w", what, err)
	}
	if _, done := ev.GetPayload().(*knov1.Event_RunFinished); done {
		agg.markClosed()
	}
	return nil
}

func (o BaselineOptions) emit(ctx context.Context, r executor.Result[*Case, caseOutcome], agg *aggregator) error {
	// Decided before any sequence number is taken. A budget refusal is not an
	// outcome for this Case — it was never attempted. Emitting one would put it
	// in the errored count and make the three counts describe work that did not
	// happen.
	if errors.Is(r.Err, errs.ErrBudgetExceeded) {
		return nil
	}

	ev := &knov1.Event{}
	if r.Done() {
		// Counted here, after the outcome is persisted, so the Run's counts can
		// never outrun the outcomes table.
		agg.add(r.Value.Score.GetValue())
		ev.Payload = &knov1.Event_CaseScored{CaseScored: &knov1.CaseScored{
			CaseId:        r.Item.GetId(),
			Score:         r.Value.Score.GetValue(),
			Passed:        r.Value.Score.GetPassed(),
			CostUsdMicros: r.Value.Response.GetCostUsdMicros(),
			LatencyMs:     r.Value.Response.GetLatencyMs(),
		}}
	} else {
		agg.addError()
		ev.Payload = &knov1.Event_CaseErrored{CaseErrored: &knov1.CaseErrored{
			CaseId: r.Item.GetId(),
			Error: &knov1.EventError{
				Code:    codeOf(r.Err),
				Message: "the agent did not return an answer",
			},
			// Retries are exhausted before an outcome reaches here —
			// invokeWithRetry returns only a terminal result — so every
			// errored Case counts once.
			WillRetry: false,
		}}
	}

	return o.appendEvent(ctx, agg, ev, "case "+r.Item.GetId())
}

// emitRunStarted opens the event stream.
func (o BaselineOptions) emitRunStarted(ctx context.Context, agg *aggregator, total int) error {
	return o.appendEvent(ctx, agg, &knov1.Event{
		Payload: &knov1.Event_RunStarted{RunStarted: &knov1.RunStarted{
			Stage:         knov1.Stage_STAGE_BASELINE,
			Agent:         o.AgentRef,
			GoalName:      o.GoalName,
			GoalDirection: o.Goal.Direction(),
			TotalCases:    int32(total), //nolint:gosec // bounded by the eval set
		}},
	}, "run-started")
}

// emitRunFinished closes the event stream. Always the last event.
func (o BaselineOptions) emitRunFinished(ctx context.Context, agg *aggregator, run *knov1.Run) error {
	scored, errored := agg.counts()

	finished := &knov1.RunFinished{
		Status:           run.GetStatus(),
		IncompleteReason: run.GetIncompleteReason(),
		Attempted:        int32(scored + errored), //nolint:gosec // bounded by the eval set
		Scored:           int32(scored),           //nolint:gosec // bounded by the eval set
		Errored:          int32(errored),          //nolint:gosec // bounded by the eval set
	}
	if m := agg.mean(); m != nil {
		finished.AggregateScore = proto.Float64(*m)
	}

	return o.appendEvent(ctx, agg,
		&knov1.Event{Payload: &knov1.Event_RunFinished{RunFinished: finished}},
		"run-finished")
}

// emitOpening writes the event that opens this process's contribution.
//
// RunStarted when the stream is empty, RunResumed when it is not — regardless
// of whether --resume was passed. A resumed run whose first process died
// before emitting anything still needs its identity on the wire, and a fresh
// run cannot have a prior sequence to continue from.
func (o BaselineOptions) emitOpening(
	ctx context.Context,
	agg *aggregator,
	priorSeq int64,
	alreadyDone int,
	restored budget.Spend,
) error {
	if priorSeq > 0 {
		return o.emitRunResumed(ctx, agg, alreadyDone, o.DevCases, restored)
	}
	return o.emitRunStarted(ctx, agg, o.DevCases)
}

// emitConcurrencyReduced reports a width the engine chose rather than the user.
//
// Only on an actual reduction: a run that got what it asked for has no news,
// and Run.concurrency records the decision either way. Emitted after the
// opening event, so a consumer has the run's identity before its caveats.
//
// Nothing to report when nothing was decided — a stage that executes no Cases
// has no concurrency.
func (o BaselineOptions) emitConcurrencyReduced(ctx context.Context, agg *aggregator) error {
	d := o.concurrency
	if d == nil || d.GetReason() == knov1.ConcurrencyReason_CONCURRENCY_REASON_UNSPECIFIED {
		return nil
	}
	return o.appendEvent(ctx, agg, &knov1.Event{
		Payload: &knov1.Event_ConcurrencyReduced{
			ConcurrencyReduced: &knov1.ConcurrencyReduced{Decision: d},
		},
	}, "concurrency-reduced")
}

// progressTicker emits StageProgress until stop is called.
//
// A ticker rather than a per-Case emission, because AppendEvent is one fsync
// each under synchronous=FULL, on the same serialized writer as the outcome
// row that prevents double-spend. Per-Case would put four to six durable
// writes behind every agent call and queue them in front of the write whose
// loss costs money.
//
// One second, chosen against a stated target rather than picked: a live view
// is useful at about 1Hz, and a 1M-Case run must not add more than ~10% to
// durable writes. At 1Hz a run of any length adds one write per second.
//
// The returned stop function blocks until the goroutine has finished, which is
// what keeps RunFinished last. appendEvent refuses a late append anyway, but a
// refusal is an error nobody reads; joining means there is nothing to refuse.
func (o BaselineOptions) progressTicker(
	ctx context.Context,
	agg *aggregator,
	total int,
	startedAt time.Time,
) (stop func() error) {
	if o.ProgressInterval <= 0 {
		return func() error { return nil }
	}

	done := make(chan struct{})
	finished := make(chan struct{})
	var failure atomic.Pointer[error]

	go func() {
		defer close(finished)
		t := time.NewTicker(o.ProgressInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				// Bounded, but by what a legitimate write can cost rather
				// than by the tick period. Bounding it by the period ended
				// runs on a slow runner: a 20ms interval against a SQLite
				// write that took longer produced "context deadline exceeded"
				// and killed the run, which is a self-inflicted failure
				// dressed as a store failure. The store's own busy_timeout is
				// 5s, so a contended write can legitimately take that long.
				//
				// Uncancellable, like closeRun, because a budget stop and a
				// Ctrl-C are when a watcher most wants the last position — but
				// unlike closeRun this carries a deadline, because losing a
				// heartbeat costs nothing and a hung write must not make
				// shutdown unbounded. The executor's sink takes the same form.
				tickCtx, cancel := context.WithTimeout(
					context.WithoutCancel(ctx), progressWriteGrace)
				err := o.emitStageProgress(tickCtx, agg, total, startedAt)
				if err == nil {
					// On the same tick, so the two heartbeats a watcher reads
					// together describe the same instant.
					err = o.emitSpendRecorded(tickCtx, agg)
				}
				cancel()
				if err != nil {
					// NOT swallowed. appendEvent allocates a sequence number
					// immediately before the write, so a failed append burns
					// one — and with a concurrent emitter running, that hole
					// is below the maximum and MaxEventSequence can never heal
					// it. A consumer reading the stream then correctly
					// concludes it lost events. Measured: 48 permanent holes
					// on a 12-Case run, exit 0.
					failure.CompareAndSwap(nil, &err)
					return
				}
			}
		}
	}()

	// Idempotent: the caller defers it as a panic guard AND calls it
	// explicitly before closeRun. Closing a channel twice panics.
	var once sync.Once
	return func() error {
		once.Do(func() {
			close(done)
			<-finished
		})
		if err := failure.Load(); err != nil {
			return *err
		}
		return nil
	}
}

// emitSettlementOvershoot reports a settlement that pushed spend past the cap.
//
// Gated on the DELTA rather than on Overshoot() being positive, and emitted at
// settle time. Once the cap binds, fitsLocked refuses every further
// authorization, so only reservations already in flight can overshoot — which
// bounds the event count by concurrency rather than by Case count. That is the
// C + N x delta_max bound docs/debt.md#32 already writes down, and this event
// enumerates its terms.
//
// The DELTA that gates this comes back from Settle rather than being read
// afterwards: detecting an overshoot by reading Overshoot() after the fact is
// a race in which two concurrent settlements both see the same positive value
// and both report it. The gate is in invokeWithRetry, where the delta is.
//
// The cumulative figure below IS read after, which is correct for it — it is a
// running total, and "as of now" is what it means.
//
// The per-event contribution is NOT derivable from this payload, and an
// earlier version of this comment said to get it by subtracting reserved from
// settled. That over-counts by the pre-cap headroom: reserved 50k, settled
// 500k, cap 200k gives 450k where the contribution to the overshoot is 300k,
// because the first 200k was still under the cap. Summing that across events
// inflates the total. Carrying the delta Settle returns is the fix and needs a
// proto field — docs/debt.md#50.
func (o BaselineOptions) emitSettlementOvershoot(
	ctx context.Context,
	agg *aggregator,
	caseID string,
	reserved, settled int64,
) error {
	return o.appendEventFunc(ctx, agg, func() *knov1.Event {
		return &knov1.Event{
			Payload: &knov1.Event_SettlementOvershoot{
				SettlementOvershoot: &knov1.SettlementOvershoot{
					CaseId:                       caseID,
					ReservedUsdMicros:            reserved,
					SettledUsdMicros:             settled,
					CumulativeOvershootUsdMicros: o.Guard.Overshoot(),
				},
			},
		}
	}, "settlement-overshoot")
}

// emitRetryAttempted reports that a Case is being retried, BEFORE the wait.
//
// Before, because the whole value of the signal is telling a watcher the run
// is obeying a provider's backoff rather than hung. Emitted after the sleep it
// announces, it says "we were idle" only once idleness ended.
func (o BaselineOptions) emitRetryAttempted(
	ctx context.Context,
	agg *aggregator,
	caseID string,
	ordinal int,
	reason knov1.RetryReason,
	wait, budgetLeft time.Duration,
) error {
	return o.appendEvent(ctx, agg, &knov1.Event{
		Payload: &knov1.Event_RetryAttempted{RetryAttempted: &knov1.RetryAttempted{
			CaseId:                 caseID,
			AttemptOrdinal:         int32(ordinal), //nolint:gosec // bounded by maxAttempts
			Reason:                 reason,
			BackoffMs:              wait.Milliseconds(),
			RetryBudgetRemainingMs: budgetLeft.Milliseconds(),
		}},
	}, "retry-attempted")
}

// emitSpendRecorded reports the run's cumulative spend.
//
// On the progress ticker rather than per settlement. All three of its totals
// are cumulative — the message was shaped for a heartbeat, not for a per-Case
// event — and per-settlement emission would put another fsync behind every
// agent call, on the same serialized writer as the outcome row that prevents
// double-spend.
func (o BaselineOptions) emitSpendRecorded(ctx context.Context, agg *aggregator) error {
	return o.appendEventFunc(ctx, agg, func() *knov1.Event {
		spent := o.Guard.Spent()
		rem := o.Guard.Remaining()
		rec := &knov1.SpendRecorded{
			TotalCostUsdMicros: spent.CostUSDMicros,
			TotalCalls:         spent.Calls,
			TotalTokens:        spent.Tokens,
		}
		// Absent when uncapped: Remaining's fields are meaningless rather than
		// zero without a cap, and its own godoc says so.
		if !rem.Unlimited {
			rec.RemainingCostUsdMicros = proto.Int64(rem.CostUSDMicros)
			rec.RemainingCalls = proto.Int64(rem.LLMCalls)
		}
		return &knov1.Event{Payload: &knov1.Event_SpendRecorded{SpendRecorded: rec}}
	}, "spend-recorded")
}

// emitStageProgress reports where the run has got to.
func (o BaselineOptions) emitStageProgress(
	ctx context.Context,
	agg *aggregator,
	total int,
	startedAt time.Time,
) error {
	scored, errored := agg.counts()
	attempted := scored + errored

	// Averaged over THIS PROCESS's work and THIS PROCESS's clock.
	//
	// Both halves matter. Over the whole run rather than the last tick,
	// because a rate measured across one heartbeat swings wildly when a single
	// Case takes longer than the window — which for an LLM call is most of
	// them. And over the session rather than the run, because attempted spans
	// a resume while startedAt does not: a resume carrying 900 completed Cases
	// into a process one second old would report 900 Cases a second.
	sessionScored, sessionErrored := agg.sessionCounts()
	var rate float64
	if elapsed := o.now().Sub(startedAt).Seconds(); elapsed > 0 {
		rate = float64(sessionScored+sessionErrored) / elapsed
	}

	return o.appendEvent(ctx, agg, &knov1.Event{
		Payload: &knov1.Event_StageProgress{StageProgress: &knov1.StageProgress{
			Stage:          knov1.Stage_STAGE_BASELINE,
			Attempted:      int32(attempted), //nolint:gosec // bounded by the eval set
			Scored:         int32(scored),    //nolint:gosec // bounded by the eval set
			Errored:        int32(errored),   //nolint:gosec // bounded by the eval set
			TotalCases:     int32(total),     //nolint:gosec // bounded by the eval set
			CasesPerSecond: rate,
		}},
	}, "stage-progress")
}

// emitRunResumed reports that this process picked up an interrupted Run.
//
// A resumed Run used to emit a second RunStarted carrying the ORIGINAL total,
// so a live view that resets progress on RunStarted jumped backward on every
// resume. That is docs/debt.md#29.
//
// The two counts are in different coordinate systems and the payload's own
// comment forbids mixing them: alreadyDone..total is OVERALL progress, and
// 0..remaining is SESSION progress. Both are carried so a consumer can render
// either without inventing the other.
func (o BaselineOptions) emitRunResumed(
	ctx context.Context,
	agg *aggregator,
	alreadyDone, total int,
	restored budget.Spend,
) error {
	// Clamped, because the DIFFERENCE is not bounded by the eval set the way
	// its operands are. checkResumable compares the eval content hash, the
	// goal, and the agent — not the split — so a resume with a larger holdout
	// fraction has fewer dev Cases than the first process already completed.
	// confirmRun and checkFeasible both already guard this same subtraction;
	// this is the only place that publishes it, and remaining is the
	// denominator of SESSION progress, so a renderer divides by it.
	if alreadyDone > total {
		alreadyDone = total
	}
	return o.appendEvent(ctx, agg, &knov1.Event{
		Payload: &knov1.Event_RunResumed{RunResumed: &knov1.RunResumed{
			AlreadyCompleted: int32(alreadyDone),         //nolint:gosec // bounded by the eval set
			Remaining:        int32(total - alreadyDone), //nolint:gosec // clamped non-negative above
			TotalCases:       int32(total),               //nolint:gosec // bounded by the eval set
			// Carried so a consumer can see the resumed run did NOT believe it
			// had spent nothing — the cap-twice failure Guard.Restore closes.
			RestoredCostUsdMicros: restored.CostUSDMicros,
			RestoredCalls:         restored.Calls,
		}},
	}, "run-resumed")
}

// sinkFunc persists each Case's outcome and emits its event.
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
			out.Spend = settledSpend(r.Value)
		default:
			out.Err = codeOf(r.Err)
			// A Case can fail AFTER a paid call — a Goal erroring on malformed
			// output, for instance. A flat one-call spend there understates
			// what was actually spent, and SettledSpend is what Guard.Restore
			// reads on resume: the resumed process would believe less was
			// spent than really was, reopening the amnesia M1-0 closed.
			if r.Value != nil && r.Value.Response != nil {
				out.Response = r.Value.Response
				out.Spend = settledSpend(r.Value)
			} else {
				// No Response, which is the retry-EXHAUSTED path — every
				// attempt failed. Hardcoding one call here is what made the
				// headline fix miss the branch it was written for: measured 5
				// persisted against 15 settled with MaxAttempts 3.
				//
				// The cost is what the provider charged across every attempt,
				// not zero. The guard settles each attempt as it happens, and
				// SettledSpend is the only durable record of money spent —
				// Guard.Restore reads it on resume. Persisting zero here while
				// the guard holds a real figure gives the resumed run that
				// difference as headroom and it spends it again.
				out.Spend = settledSpend(r.Value)
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

	// BilledUSDMicros is what the provider charged across every attempt.
	//
	// Accumulated in invokeWithRetry rather than derived from Response,
	// because a failed attempt can be billed and produces no Response. The
	// guard settles each attempt as it happens; this is what the SINK
	// persists, and SettledSpend is the only durable record of money spent —
	// so a difference between the two is headroom a resumed run spends twice.
	BilledUSDMicros int64

	Response *Response
	Score    *Score
	Err      error
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
